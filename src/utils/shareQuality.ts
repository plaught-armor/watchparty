// Browsers cap WebRTC video near 2.5 Mbps by default, which smears 1080p
// film. This is per viewer, so the sharer needs this much upload for each one.
const MAX_VIDEO_BITRATE = 8_000_000;

// H.264 first because most GPUs can encode it, which keeps the sharer's CPU
// free. The connection settles on the first codec the viewer can also decode,
// so unsupported ones are skipped automatically.
const CODEC_ORDER = ["video/H264", "video/VP9"];

function codecRank(codec: { mimeType: string }): number {
  const rank = CODEC_ORDER.findIndex(
    (mime) => mime.toLowerCase() === codec.mimeType.toLowerCase(),
  );
  return rank === -1 ? CODEC_ORDER.length : rank;
}

function preferQualityCodecs(transceiver: RTCRtpTransceiver): void {
  const codecs = RTCRtpSender.getCapabilities?.("video")?.codecs;
  if (!codecs || !transceiver.setCodecPreferences) {
    return;
  }
  // Array.prototype.sort is stable, so helper formats (rtx, red) keep their order.
  const ordered = [...codecs].sort((a, b) => codecRank(a) - codecRank(b));
  try {
    transceiver.setCodecPreferences(ordered);
  } catch (err) {
    // Some browsers only accept their receive list here; defaults still work.
    console.warn("[share] could not set codec preferences", err);
  }
}

async function raiseBitrate(sender: RTCRtpSender): Promise<void> {
  const params = sender.getParameters();
  if (!params.encodings?.length) {
    params.encodings = [{}];
  }
  for (const encoding of params.encodings) {
    encoding.maxBitrate = MAX_VIDEO_BITRATE;
  }
  try {
    await sender.setParameters(params);
  } catch (err) {
    console.warn("[share] could not raise video bitrate", err);
  }
}

/**
 * Tunes the video being shared over `pc` for picture quality. Call after the
 * tracks are added and before the offer is created. `isFile` marks film-like
 * content, where smooth motion matters more than crisp text.
 */
export function tuneShareQuality(pc: RTCPeerConnection, isFile: boolean): void {
  for (const transceiver of pc.getTransceivers()) {
    const track = transceiver.sender.track;
    if (track?.kind !== "video") {
      continue;
    }
    if (isFile) {
      track.contentHint = "motion";
    }
    preferQualityCodecs(transceiver);
    void raiseBitrate(transceiver.sender);
  }
}
