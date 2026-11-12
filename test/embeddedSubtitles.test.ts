// Run with: node --test test/
// The fixtures are four-second clips made with ffmpeg from a two-cue subtitle.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import {
  cuesToSrt,
  extractSubtitleTracks,
} from "../src/utils/embeddedSubtitles.ts";

const load = async (name: string): Promise<Blob> =>
  new Blob([await readFile(new URL(`./fixtures/${name}`, import.meta.url))]);

const PLAIN_CUES = [
  { start: 0.5, end: 1.5, text: "Hello there" },
  { start: 2, end: 3.25, text: "Zwei Zeilen\ncafé ✓" },
];

test("reads text subtitles from an MP4", async () => {
  const tracks = await extractSubtitleTracks(await load("subs.mp4"));
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].language, "deu");
  assert.deepEqual(tracks[0].cues, PLAIN_CUES);
});

test("reads an MP4 whose index comes before the media", async () => {
  const tracks = await extractSubtitleTracks(await load("subs-faststart.mp4"));
  assert.deepEqual(tracks[0].cues, PLAIN_CUES);
});

test("reads SRT subtitles from a Matroska file", async () => {
  const tracks = await extractSubtitleTracks(await load("subs-srt.mkv"));
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].language, "deu");
  assert.deepEqual(tracks[0].cues, PLAIN_CUES);
});

test("strips styling from ASS subtitles in a Matroska file", async () => {
  const tracks = await extractSubtitleTracks(await load("subs-ass.mkv"));
  assert.deepEqual(tracks[0].cues, [
    { start: 0.5, end: 1.5, text: "Styled, with comma" },
    { start: 2, end: 3.25, text: "Line one\nLine two" },
  ]);
});

test("finds nothing in a file without subtitles", async () => {
  assert.deepEqual(await extractSubtitleTracks(await load("no-subs.mp4")), []);
});

test("finds nothing in a file that is not a video", async () => {
  assert.deepEqual(await extractSubtitleTracks(new Blob(["not a video"])), []);
  assert.deepEqual(await extractSubtitleTracks(new Blob([])), []);
});

test("survives a truncated file", async () => {
  const whole = await load("subs.mp4");
  const mkv = await load("subs-srt.mkv");
  for (const cut of [10, 100, 700, 2000]) {
    await extractSubtitleTracks(whole.slice(0, cut));
    await extractSubtitleTracks(mkv.slice(0, cut));
  }
});

test("writes cues as SRT", () => {
  assert.equal(
    cuesToSrt(PLAIN_CUES),
    "1\n00:00:00,500 --> 00:00:01,500\nHello there\n\n" +
      "2\n00:00:02,000 --> 00:00:03,250\nZwei Zeilen\ncafé ✓\n",
  );
  assert.equal(
    cuesToSrt([{ start: 3661.007, end: 3662, text: "late" }]),
    "1\n01:01:01,007 --> 01:01:02,000\nlate\n",
  );
});
