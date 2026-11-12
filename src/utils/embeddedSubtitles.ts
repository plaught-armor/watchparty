/**
 * Reads text subtitle tracks stored inside MP4 (tx3g) and Matroska (SRT, ASS,
 * WebVTT) files, straight from the file in the browser. Browsers ignore these
 * tracks, so the app extracts them and shows them as ordinary subtitles.
 *
 * Picture-based subtitles (PGS, VobSub) and fragmented MP4 are not supported.
 * This file has no imports so it can also run under plain Node for its tests.
 */

export interface SubtitleCue {
  /** Seconds from the start of the video. */
  start: number;
  end: number;
  text: string;
}

export interface SubtitleTrack {
  /** Three-letter language code as stored in the file, e.g. "eng". */
  language: string;
  cues: SubtitleCue[];
}

const MAX_BOXES = 4096;
const MAX_MOOV_BYTES = 256 * 1024 * 1024;
const MAX_SAMPLES = 50_000;
const MAX_CUE_BYTES = 64 * 1024;
const MAX_HEADER_BYTES = 4 * 1024 * 1024;
// A two-hour Matroska file holds roughly half a million blocks.
const MAX_ELEMENTS = 4_000_000;
const WINDOW_BYTES = 1024 * 1024;
// Used when a Matroska cue does not say how long it stays on screen.
const DEFAULT_CUE_SECONDS = 3;

const utf8 = new TextDecoder();

async function readView(
  file: Blob,
  offset: number,
  length: number,
): Promise<DataView> {
  return new DataView(await file.slice(offset, offset + length).arrayBuffer());
}

/** A part of `view`. Throws RangeError when the part lies outside it. */
function subView(view: DataView, offset: number, length: number): DataView {
  return new DataView(view.buffer, view.byteOffset + offset, length);
}

function fourcc(view: DataView, offset: number): string {
  return String.fromCharCode(
    view.getUint8(offset),
    view.getUint8(offset + 1),
    view.getUint8(offset + 2),
    view.getUint8(offset + 3),
  );
}

function cleanText(text: string): string {
  return text
    .replace(/\r/g, "")
    .replace(/\n{2,}/g, "\n")
    .trim();
}

// ---------------------------------------------------------------- MP4 ----

interface Box {
  type: string;
  body: DataView;
}

function listBoxes(parent: DataView | undefined): Box[] {
  const boxes: Box[] = [];
  if (!parent) {
    return boxes;
  }
  let offset = 0;
  for (let i = 0; i < MAX_BOXES && offset + 8 <= parent.byteLength; i++) {
    let size = parent.getUint32(offset);
    let header = 8;
    if (size === 1) {
      size = Number(parent.getBigUint64(offset + 8));
      header = 16;
    } else if (size === 0) {
      size = parent.byteLength - offset;
    }
    if (size < header || offset + size > parent.byteLength) {
      break;
    }
    boxes.push({
      type: fourcc(parent, offset + 4),
      body: subView(parent, offset + header, size - header),
    });
    offset += size;
  }
  return boxes;
}

function findBox(
  parent: DataView | undefined,
  type: string,
): DataView | undefined {
  return listBoxes(parent).find((box) => box.type === type)?.body;
}

/** Finds the index box ("moov"), which sits before or after the media data. */
async function readMoov(file: Blob): Promise<DataView | undefined> {
  let offset = 0;
  for (let i = 0; i < MAX_BOXES && offset + 8 <= file.size; i++) {
    const head = await readView(file, offset, 16);
    let size = head.getUint32(0);
    let header = 8;
    if (size === 1) {
      size = Number(head.getBigUint64(8));
      header = 16;
    } else if (size === 0) {
      size = file.size - offset;
    }
    if (size < header) {
      return undefined;
    }
    if (fourcc(head, 4) === "moov") {
      if (size > MAX_MOOV_BYTES) {
        return undefined;
      }
      return readView(file, offset + header, size - header);
    }
    offset += size;
  }
  return undefined;
}

interface Mp4SampleTable {
  language: string;
  timescale: number;
  /** One entry per sample, in the track's time units. */
  durations: number[];
  sizes: number[];
  /** Position of each sample in the file. */
  offsets: number[];
}

function readDurations(stts: DataView): number[] {
  const durations: number[] = [];
  const runs = stts.getUint32(4);
  for (let i = 0; i < runs && durations.length < MAX_SAMPLES; i++) {
    const count = stts.getUint32(8 + i * 8);
    const delta = stts.getUint32(12 + i * 8);
    for (let j = 0; j < count && durations.length < MAX_SAMPLES; j++) {
      durations.push(delta);
    }
  }
  return durations;
}

function readSizes(stsz: DataView): number[] {
  const fixedSize = stsz.getUint32(4);
  const count = Math.min(stsz.getUint32(8), MAX_SAMPLES);
  const sizes: number[] = [];
  for (let i = 0; i < count; i++) {
    sizes.push(fixedSize || stsz.getUint32(12 + i * 4));
  }
  return sizes;
}

function readChunkOffsets(stbl: DataView): number[] {
  const stco = findBox(stbl, "stco");
  const co64 = findBox(stbl, "co64");
  const table = stco ?? co64;
  if (!table) {
    return [];
  }
  const count = Math.min(table.getUint32(4), MAX_SAMPLES);
  const offsets: number[] = [];
  for (let i = 0; i < count; i++) {
    offsets.push(
      stco ? table.getUint32(8 + i * 4) : Number(table.getBigUint64(8 + i * 8)),
    );
  }
  return offsets;
}

/** Samples lie back to back inside chunks; "stsc" says how many per chunk. */
function readSampleOffsets(stbl: DataView, sizes: number[]): number[] {
  const stsc = findBox(stbl, "stsc");
  const chunks = readChunkOffsets(stbl);
  const offsets: number[] = [];
  if (!stsc) {
    return offsets;
  }
  const runs = stsc.getUint32(4);
  let run = 0;
  for (let chunk = 0; chunk < chunks.length; chunk++) {
    // Runs are keyed by the 1-based number of their first chunk.
    while (run + 1 < runs && stsc.getUint32(8 + (run + 1) * 12) <= chunk + 1) {
      run++;
    }
    const perChunk = runs > 0 ? stsc.getUint32(12 + run * 12) : 0;
    let position = chunks[chunk];
    for (let i = 0; i < perChunk && offsets.length < sizes.length; i++) {
      offsets.push(position);
      position += sizes[offsets.length - 1];
    }
  }
  return offsets;
}

/** Language is three letters packed into 15 bits, each offset from 0x60. */
function mp4Language(packed: number): string {
  return String.fromCharCode(
    ((packed >> 10) & 31) + 0x60,
    ((packed >> 5) & 31) + 0x60,
    (packed & 31) + 0x60,
  );
}

/** The sample table of a text subtitle track, or undefined for other tracks. */
function readSampleTable(trak: DataView): Mp4SampleTable | undefined {
  const mdia = findBox(trak, "mdia");
  const hdlr = findBox(mdia, "hdlr");
  const mdhd = findBox(mdia, "mdhd");
  const stbl = findBox(findBox(mdia, "minf"), "stbl");
  const stsd = findBox(stbl, "stsd");
  const stts = findBox(stbl, "stts");
  const stsz = findBox(stbl, "stsz");
  if (!hdlr || !mdhd || !stbl || !stsd || !stts || !stsz) {
    return undefined;
  }
  const handler = fourcc(hdlr, 8);
  const format = stsd.getUint32(4) > 0 ? fourcc(stsd, 12) : "";
  if ((handler !== "sbtl" && handler !== "text") || format !== "tx3g") {
    return undefined;
  }
  // Version 1 of this box stores its two dates in 8 bytes instead of 4.
  const wide = mdhd.getUint8(0) === 1;
  const sizes = readSizes(stsz);
  return {
    language: mp4Language(mdhd.getUint16(wide ? 32 : 20)),
    timescale: mdhd.getUint32(wide ? 20 : 12),
    durations: readDurations(stts),
    sizes,
    offsets: readSampleOffsets(stbl, sizes),
  };
}

async function readMp4Cues(
  file: Blob,
  table: Mp4SampleTable,
): Promise<SubtitleCue[]> {
  const cues: SubtitleCue[] = [];
  const count = Math.min(table.offsets.length, table.durations.length);
  let time = 0;
  for (let i = 0; i < count; i++) {
    const start = time / table.timescale;
    time += table.durations[i];
    // A sample is a 2-byte text length, the text, then optional styling.
    // Samples with no text mark the gaps between cues.
    if (table.sizes[i] <= 2 || table.sizes[i] > MAX_CUE_BYTES) {
      continue;
    }
    const sample = await readView(file, table.offsets[i], table.sizes[i]);
    const length = Math.min(sample.getUint16(0), sample.byteLength - 2);
    const text = cleanText(utf8.decode(subView(sample, 2, length)));
    if (text) {
      cues.push({ start, end: time / table.timescale, text });
    }
  }
  return cues;
}

async function readMp4(file: Blob): Promise<SubtitleTrack[]> {
  const moov = await readMoov(file);
  const tracks: SubtitleTrack[] = [];
  for (const box of listBoxes(moov)) {
    const table = box.type === "trak" ? readSampleTable(box.body) : undefined;
    if (!table || table.timescale === 0) {
      continue;
    }
    const cues = await readMp4Cues(file, table);
    if (cues.length > 0) {
      tracks.push({ language: table.language, cues });
    }
  }
  return tracks;
}

// ----------------------------------------------------------- Matroska ----

const EBML_HEADER = 0x1a45dfa3;
const SEGMENT = 0x18538067;
const INFO = 0x1549a966;
const TIMESTAMP_SCALE = 0x2ad7b1;
const TRACKS = 0x1654ae6b;
const TRACK_ENTRY = 0xae;
const TRACK_NUMBER = 0xd7;
const CODEC_ID = 0x86;
const LANGUAGE = 0x22b59c;
const CLUSTER = 0x1f43b675;
const CLUSTER_TIMESTAMP = 0xe7;
const SIMPLE_BLOCK = 0xa3;
const BLOCK_GROUP = 0xa0;
const BLOCK = 0xa1;
const BLOCK_DURATION = 0x9b;

// Element ids are unique across nesting levels, so the file can be walked as
// one flat list as long as these containers are entered instead of skipped.
const CONTAINERS = new Set([SEGMENT, CLUSTER, BLOCK_GROUP]);
const TEXT_CODECS = new Set(["S_TEXT/UTF8", "S_TEXT/WEBVTT"]);
const ASS_CODECS = new Set(["S_TEXT/ASS", "S_TEXT/SSA"]);

interface Element {
  id: number;
  /** Length of the content, or -1 when the file does not say. */
  size: number;
  /** Where the content starts, in the same coordinates as the given offset. */
  dataOffset: number;
}

/** Byte length of a variable-length number, from the zeros leading its first byte. */
function vintLength(firstByte: number): number {
  return firstByte === 0 ? 0 : Math.clz32(firstByte) - 23;
}

function readUint(view: DataView, offset: number, length: number): number {
  let value = 0;
  for (let i = 0; i < length; i++) {
    value = value * 256 + view.getUint8(offset + i);
  }
  return value;
}

/**
 * A variable-length number without its length marker bit. The marker is
 * removed before the bytes are combined, because an 8-byte number with the
 * marker still in place is too large for a JavaScript number to hold exactly.
 */
function readVint(view: DataView, offset: number, length: number): number {
  let value = view.getUint8(offset) & (0xff >> length);
  for (let i = 1; i < length; i++) {
    value = value * 256 + view.getUint8(offset + i);
  }
  return value;
}

/** Parses the element header at `offset` of `view`, or null if it is cut off. */
function parseElement(view: DataView, offset: number): Element | null {
  if (offset >= view.byteLength) {
    return null;
  }
  const idLength = vintLength(view.getUint8(offset));
  if (idLength < 1 || idLength > 4 || offset + idLength >= view.byteLength) {
    return null;
  }
  const sizeAt = offset + idLength;
  const sizeLength = vintLength(view.getUint8(sizeAt));
  if (sizeLength < 1 || sizeAt + sizeLength > view.byteLength) {
    return null;
  }
  const size = readVint(view, sizeAt, sizeLength);
  return {
    id: readUint(view, offset, idLength),
    // All value bits set means "unknown size", used by live recordings.
    size: size === 2 ** (7 * sizeLength) - 1 ? -1 : size,
    dataOffset: sizeAt + sizeLength,
  };
}

/** The direct children of an element that is fully in memory. */
function listElements(view: DataView): { id: number; body: DataView }[] {
  const elements: { id: number; body: DataView }[] = [];
  let offset = 0;
  for (let i = 0; i < MAX_BOXES && offset < view.byteLength; i++) {
    const element = parseElement(view, offset);
    if (!element || element.size < 0) {
      break;
    }
    elements.push({
      id: element.id,
      body: subView(view, element.dataOffset, element.size),
    });
    offset = element.dataOffset + element.size;
  }
  return elements;
}

type Reader = (offset: number, length: number) => Promise<DataView>;

/**
 * Reads the file through a sliding window, so walking hundreds of thousands
 * of small headers costs one disk read per megabyte instead of one each.
 * The result is shorter than asked for at the end of the file.
 */
function windowReader(file: Blob): Reader {
  let start = 0;
  let cached: DataView = new DataView(new ArrayBuffer(0));
  return async (offset, length) => {
    const end = Math.min(offset + length, file.size);
    if (offset < start || end > start + cached.byteLength) {
      start = offset;
      cached = await readView(file, offset, Math.max(length, WINDOW_BYTES));
    }
    return subView(cached, offset - start, Math.max(0, end - offset));
  };
}

interface MatroskaTrack {
  language: string;
  isAss: boolean;
  cues: SubtitleCue[];
}

interface MatroskaState {
  /** Nanoseconds per timestamp unit. */
  scale: number;
  clusterTime: number;
  tracks: Map<number, MatroskaTrack>;
  /** The cue of the block just read, waiting for its duration. */
  pending: SubtitleCue | undefined;
}

function readTracks(body: DataView, tracks: Map<number, MatroskaTrack>): void {
  for (const entry of listElements(body)) {
    if (entry.id !== TRACK_ENTRY) {
      continue;
    }
    let number = 0;
    let codec = "";
    // The format's default when a track does not name its language.
    let language = "eng";
    for (const field of listElements(entry.body)) {
      if (field.id === TRACK_NUMBER) {
        number = readUint(field.body, 0, field.body.byteLength);
      } else if (field.id === CODEC_ID) {
        codec = utf8.decode(field.body);
      } else if (field.id === LANGUAGE) {
        language = utf8.decode(field.body).replace(/\0+$/, "");
      }
    }
    if (TEXT_CODECS.has(codec) || ASS_CODECS.has(codec)) {
      tracks.set(number, { language, isAss: ASS_CODECS.has(codec), cues: [] });
    }
  }
}

/** An ASS cue is eight comma-separated fields followed by the styled text. */
function assText(line: string): string {
  let comma = -1;
  for (let i = 0; i < 8; i++) {
    comma = line.indexOf(",", comma + 1);
    if (comma < 0) {
      return "";
    }
  }
  return line
    .slice(comma + 1)
    .replace(/\{[^}]*\}/g, "")
    .replace(/\\[Nn]/g, "\n")
    .replace(/\\h/g, " ");
}

/** A block is a track number, a time relative to its cluster, flags, then data. */
async function readBlock(
  read: Reader,
  element: Element,
  state: MatroskaState,
): Promise<void> {
  state.pending = undefined;
  const head = await read(element.dataOffset, 12);
  const numberLength = head.byteLength > 0 ? vintLength(head.getUint8(0)) : 0;
  const headerLength = numberLength + 3;
  if (numberLength < 1 || head.byteLength < headerLength) {
    return;
  }
  const number = readVint(head, 0, numberLength);
  const track = state.tracks.get(number);
  const textLength = element.size - headerLength;
  if (!track || textLength <= 0 || textLength > MAX_CUE_BYTES) {
    return;
  }
  const raw = utf8.decode(
    await read(element.dataOffset + headerLength, textLength),
  );
  const text = cleanText(track.isAss ? assText(raw) : raw);
  if (!text || track.cues.length >= MAX_SAMPLES) {
    return;
  }
  const ticks = state.clusterTime + head.getInt16(numberLength);
  const start = (ticks * state.scale) / 1e9;
  state.pending = { start, end: start + DEFAULT_CUE_SECONDS, text };
  track.cues.push(state.pending);
}

async function readSmallBody(
  read: Reader,
  element: Element,
): Promise<DataView> {
  return read(element.dataOffset, Math.min(element.size, MAX_HEADER_BYTES));
}

/** Handles one element; returns false when the rest of the file is of no use. */
async function visitElement(
  read: Reader,
  element: Element,
  state: MatroskaState,
): Promise<boolean> {
  if (element.id === CLUSTER) {
    // Track descriptions come before the media, so no text tracks by now
    // means there are none, and the media need not be read at all.
    return state.tracks.size > 0;
  }
  if (element.id === INFO) {
    for (const field of listElements(await readSmallBody(read, element))) {
      if (field.id === TIMESTAMP_SCALE) {
        state.scale = readUint(field.body, 0, field.body.byteLength);
      }
    }
  } else if (element.id === TRACKS) {
    readTracks(await readSmallBody(read, element), state.tracks);
  } else if (element.id === CLUSTER_TIMESTAMP) {
    const body = await read(element.dataOffset, element.size);
    state.clusterTime = readUint(body, 0, body.byteLength);
  } else if (element.id === BLOCK || element.id === SIMPLE_BLOCK) {
    await readBlock(read, element, state);
  } else if (element.id === BLOCK_DURATION && state.pending) {
    const body = await read(element.dataOffset, element.size);
    const ticks = readUint(body, 0, body.byteLength);
    state.pending.end = state.pending.start + (ticks * state.scale) / 1e9;
    state.pending = undefined;
  }
  return true;
}

async function readMatroska(file: Blob): Promise<SubtitleTrack[]> {
  const read = windowReader(file);
  const state: MatroskaState = {
    scale: 1_000_000,
    clusterTime: 0,
    tracks: new Map(),
    pending: undefined,
  };
  let offset = 0;
  for (let i = 0; i < MAX_ELEMENTS && offset < file.size; i++) {
    const head = await read(offset, 12);
    const element = parseElement(head, 0);
    if (!element) {
      break;
    }
    element.dataOffset += offset;
    if (!(await visitElement(read, element, state))) {
      break;
    }
    if (CONTAINERS.has(element.id)) {
      offset = element.dataOffset;
    } else if (element.size < 0) {
      break;
    } else {
      offset = element.dataOffset + element.size;
    }
  }
  return [...state.tracks.values()]
    .filter((track) => track.cues.length > 0)
    .map(({ language, cues }) => ({ language, cues }));
}

// ------------------------------------------------------------- public ----

/**
 * Returns the text subtitle tracks found inside a video file, in file order.
 * Empty when there are none, or when the file is not MP4 or Matroska.
 */
export async function extractSubtitleTracks(
  file: Blob,
): Promise<SubtitleTrack[]> {
  try {
    const head = await readView(file, 0, 8);
    if (head.byteLength < 8) {
      return [];
    }
    if (head.getUint32(0) === EBML_HEADER) {
      return await readMatroska(file);
    }
    return fourcc(head, 4) === "ftyp" ? await readMp4(file) : [];
  } catch (err) {
    if (err instanceof RangeError) {
      // A table points outside the file: it is cut short or damaged.
      console.warn("[subtitles] could not read embedded subtitles", err);
      return [];
    }
    throw err;
  }
}

function srtTime(seconds: number): string {
  const total = Math.round(seconds * 1000);
  const pad = (value: number, width: number) =>
    String(value).padStart(width, "0");
  return (
    `${pad(Math.floor(total / 3_600_000), 2)}:` +
    `${pad(Math.floor(total / 60_000) % 60, 2)}:` +
    `${pad(Math.floor(total / 1000) % 60, 2)},${pad(total % 1000, 3)}`
  );
}

/** Formats cues as an SRT subtitle file. */
export function cuesToSrt(cues: SubtitleCue[]): string {
  return cues
    .map(
      (cue, index) =>
        `${index + 1}\n${srtTime(cue.start)} --> ${srtTime(cue.end)}\n${cue.text}\n`,
    )
    .join("\n");
}
