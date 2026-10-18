// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import * as fs from "node:fs";
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath } from "node:url";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

// A speech-to-text turn in the PORTAL CHAT, proven across a real process boundary.
// The gateway under test is the REAL binary (memory mode) and the UI is the REAL
// built portal; the audio.cpp the chat's run executor relays to is a small stand-in
// this spec starts in-process. Everything between them is real: the portal's own
// application form creates the audio_cpp application, the gateway's app-health loop
// reads the stub's /v1/models and writes the transcription verdict itself, that
// verdict is what makes the chat offer the model, the audio clip the browser attaches
// is stored in the chat blob store, the chat run executor loopback-POSTs the clip as a
// multipart upload to /v1/audio/transcriptions, and the transcript turn the portal
// renders is read back out of the chat store.
//
// The clip gets into the composer both ways the portal offers: ATTACHED from a file
// (scenarios 3 and 5, tiny NON-WAV fixtures: a WebM/Opus and an MP3 tone, both stereo
// and not at 16 kHz) and RECORDED in the browser (scenario 6). A microphone is the one
// thing no suite can rely on -- headless Chromium has no capture device, and its fake
// one does not even answer getUserMedia on every host -- so the spec replaces only the
// capture SOURCE (see installSyntheticMicrophone): the portal's recorder, the browser's
// REAL MediaRecorder and its real audio/webm (Opus) encoding are all untouched. The
// recorder's state machine and error paths are unit-tested (AudioRecorder.test.tsx);
// what is cross-process -- the clip's bytes on the wire and in the store -- is what the
// two ways are compared on here.
//
// The audio.cpp server reads WAV ONLY (a live probe: webm, ogg, m4a and mp3 uploads are
// refused with a 400), so the composer converts EVERY clip, attached or recorded, to a
// 16 kHz mono 16-bit PCM WAV in the browser before it enters the turn
// (shared/audioToWav.ts). Chromium has real WebAudio, so that conversion runs for real
// here: the fixtures and the recording are decoded, down-mixed, resampled and
// re-encoded by the portal's own code, and what the suite asserts is what then crosses
// the process boundary -- a WAV, whatever went in. (jsdom has no WebAudio, so the
// Vitest suites drive the encoder through fakes; this suite is where it meets a real
// decoder.) The gateway itself never transcodes: it relays the stored WAV as it is.
//
// The fixtures are 2 s tones, generated once and committed (fixtures/), with
//
//   ffmpeg -f lavfi -i sine=frequency=440:duration=2:sample_rate=48000 -ac 2 \
//     -map_metadata -1 -fflags +bitexact -flags:a +bitexact -c:a libopus -b:a 24k tone-440hz.webm
//   ffmpeg -f lavfi -i sine=frequency=880:duration=2:sample_rate=44100 -ac 2 \
//     -map_metadata -1 -fflags +bitexact -flags:a +bitexact -c:a libmp3lame -b:a 24k \
//     -write_xing 0 -id3v2_version 0 tone-880hz.mp3
//
// What the in-process suites cannot reach (the Go run-executor tests stub the HTTP
// hop, the Vitest suites stub fetch, the audio element and WebAudio), and this suite
// owns:
//
//   1. OFFERING: the model the probe marks transcription-capable is in the chat's
//      picker and a model the probe has no verdict for (an unprobed one) is not, and
//      picking the former switches the composer to transcription mode -- an audio
//      input in place of the text prompt, Send closed until a clip is attached, the
//      language / prompt / temperature settings shown, the text-chat controls gone.
//   2. THE WIRE: one send makes exactly one upstream multipart POST to
//      /v1/audio/transcriptions, with the gateway model name rewritten to the
//      application's own model id, the thread's language, prompt and temperature
//      carried as form fields, and the clip arriving as the `file` part -- a WAV (the
//      attached WebM converted: RIFF/WAVE, PCM, mono, 16 kHz, 16 bit, the tone still
//      at its pitch) named audio.wav, never the WebM that was attached; the stub never
//      sees a chat-completions call.
//   3. THE ANSWER: a pending state while the upstream is still working, then the
//      transcript as a plain text turn, next to the user's own clip, which plays from
//      the chat blob store (byte for byte the WAV the upstream received, as audio/wav,
//      which the browser really decodes) and downloads as the same bytes; the untitled
//      chat is named after its transcript.
//   4. PERSISTENCE: after a full reload the thread is still a transcription thread,
//      its settings are restored, the clip plays again from the store and the
//      transcript is still there -- and reading it back sent nothing upstream.
//   5. THE FOLLOW-UP: a second clip (an MP3, another format and another pitch) is a
//      second upstream call with the NEW clip's WAV (an earlier turn's clip is never
//      re-sent), and the advanced JSON passthrough reaches the form next to the
//      thread's own settings, a passthrough key that repeats a set setting not being
//      sent twice.
//   6. THE RECORDING: a clip recorded in the browser is converted like an attached
//      one -- the recorder's audio/webm never reaches the wire: the upstream gets a WAV
//      (named audio.wav, byte for byte what the chat blob store holds) and answers it
//      with its own transcript.
//   7. THE BILLING UNIT: each run leaves an audio_second usage row (from the stub's
//      timing.audio_duration_ms, 2000 ms = 2 s), not a token-metered one.
//   8. STREAMING (issue #262): the run always asks the gateway for stream=true, and the
//      gateway decides per model. The models above state no `mode` in the stub's
//      /v1/models, so they carry no transcription_streaming verdict and are answered
//      buffered (the scenarios above, whose upstream form never has a stream part, are
//      that path through the portal). A model the stub lists with mode=streaming gets the
//      verdict yes, the run's stream=true reaches it, and the transcript turn FILLS IN as
//      the stub's transcript.text.delta frames arrive: the stub pauses its stream after
//      each of the first two deltas and the spec looks at the turn at every pause, so a
//      transcript that only appeared when the run ended could not pass. The committed
//      transcript is the done event's text (it restates the whole text, with a final
//      period the deltas lack), survives a reload, and the run's usage row is billed from
//      the converted WAV's header -- the streamed done event states no audio_duration_ms.
//
// Not covered here, and owned by the unit/integration suites instead: the reserved
// passthrough fields and the invalid-JSON refusal, the run's error codes and their
// labels (including "no transcript" for silence), the refusals of the attach (not audio,
// undecodable, too large), the recorder's permission-denied / unsupported / failed paths,
// and the object-URL revoke on unmount -- none of those is a cross-process property.

const t = messages.de;

// Memory-mode dev principal (cmd/gateway memoryDeps): a system_admin with
// totp_mode=off who owns the seeded "Dev Admin Group".
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";

const CSRF = { "X-OP-CSRF": "1" };

const TRANSCRIPTION_PATH = "/v1/audio/transcriptions";
const SERVER_NAME = "E2E-ChatTranscription-Server";

// The gateway model names the chat can be asked about, and the model ids audio.cpp
// serves them under. They differ on purpose: the relay's model rewrite is then
// observable at the stub, which refuses (500, as the real server does for an unknown
// model) any request that names a model it does not serve. The unprobed model's
// upstream is one the stub does not list (/v1/models names only LOADED models), so
// the probe has nothing to say about it.
const ASR_MODEL = "e2e-chat-transcribe";
const ASR_UPSTREAM = "whisper_x";
const UNPROBED_MODEL = "e2e-chat-unprobed";
const UNPROBED_UPSTREAM = "not_loaded_yet";
// The streaming model (issue #262): the stub lists it with mode=streaming, so the probe
// gives it the transcription_streaming verdict yes and the gateway relays the run's
// stream=true to it.
const STREAM_MODEL = "e2e-chat-streaming";
const STREAM_UPSTREAM = "vibevoice_asr_streaming";

// What the stub answers the nth transcription with: the transcript, and the timing
// block whose audio_duration_ms (milliseconds) is the quantity the gateway bills. Two
// seconds, and the fixture clips below really are this long, so the number the stub
// reports, the converted WAV and the duration the browser decodes all agree. Every
// transcript is shorter than the 40 characters the chat title is cut at.
const CLIP_SECONDS = 2;
const CLIP_DURATION_MS = CLIP_SECONDS * 1000;
const TRANSCRIPTS = ["Guten Tag, das ist das Transkript.", "Und hier noch ein zweiter Satz.", "Dies stammt aus einer Aufnahme."];
// How long the stub holds the FIRST answer, so the pending state is on screen for long
// enough to be observed without a race.
const FIRST_ANSWER_DELAY_MS = 1500;

// What the stub streams for stream=true on the streaming model: the increments, and the
// done event that restates the whole text (with the final period no delta carries, which
// is what shows the committed transcript is the done event's) and a time to first token,
// but no audio_duration_ms.
const STREAM_DELTAS = ["Guten Tag, ", "das ist ein ", "Streaming-Transkript"];
const STREAM_TEXT = "Guten Tag, das ist ein Streaming-Transkript.";
const STREAM_TTFT_MS = 120;
// A held stream is released by itself after this long, so a failing assertion can never
// leave the stub (and the gateway's relay and the run) waiting for a release that will
// not come.
const HOLD_SAFETY_MS = 15000;

const LANGUAGE = "en";
const PROMPT = "Names: Alba, Jonas.";
const TEMPERATURE = 0.2;
// The follow-up's advanced passthrough: a native knob and a repeat of the language the
// thread already carries (a first-class setting that is set wins, so it must not be
// sent a second time -- the real endpoint refuses a repeated field).
const PASSTHROUGH = { word_timestamps: true, seed: 7, language: "fr" };

// The clips the spec attaches: NON-WAV audio, so the composer's conversion is what
// makes the upload a WAV. Each is a CLIP_SECONDS tone, stereo and not at 16 kHz (48 kHz
// Opus, 44.1 kHz MP3), so the down-mix and the resample are exercised too; a different
// pitch and container make a different clip, so which of two reached the upstream is
// observable from the bytes AND from the tone in them.
type Fixture = { name: string; mimeType: string; hz: number; bytes: Buffer };

function fixture(name: string, mimeType: string, hz: number): Fixture {
  return { name, mimeType, hz, bytes: fs.readFileSync(fileURLToPath(new URL(`./fixtures/${name}`, import.meta.url))) };
}

const FIRST_CLIP = fixture("tone-440hz.webm", "audio/webm", 440);
const SECOND_CLIP = fixture("tone-880hz.mp3", "audio/mpeg", 880);
// The pitch of the synthetic microphone (installSyntheticMicrophone).
const MICROPHONE_HZ = 440;

// What the composer converts every clip to (shared/audioToWav.ts) and the audio.cpp
// server reads: a canonical 44-byte RIFF/WAVE header, then 16-bit PCM, mono, 16 kHz.
const WAV_SAMPLE_RATE = 16000;
const WAV_HEADER_BYTES = 44;

type WavFacts = {
  riff: string;
  riffSize: number;
  wave: string;
  fmt: string;
  fmtSize: number;
  formatTag: number;
  channels: number;
  sampleRate: number;
  byteRate: number;
  blockAlign: number;
  bitsPerSample: number;
  data: string;
  dataBytes: number;
  /** The length of the clip, from the samples. */
  seconds: number;
  /** The largest sample, as a fraction of full scale: 0 is silence. */
  peak: number;
  /** The pitch of the tone, from the zero crossings of the audible stretch. */
  hz: number;
};

/**
 * Reads a WAV the way a strict consumer would: the header fields exactly where the
 * canonical 44-byte layout puts them, and, from the 16-bit samples, the length, the
 * loudest sample and the pitch. The pitch is the number of sign changes between the
 * first and the last audible sample (|sample| over a threshold, so a recording's quiet
 * lead-in and tail do not count) over twice their duration: a decode, down-mix and
 * resample that went wrong would change it, which makes it a check that the audio
 * survived the conversion and not just that a header was written.
 */
function readWav(wav: Buffer): WavFacts {
  const samples = Math.max(0, (wav.length - WAV_HEADER_BYTES) >> 1);
  const at = (i: number) => wav.readInt16LE(WAV_HEADER_BYTES + i * 2);
  let peak = 0;
  let first = -1;
  let last = -1;
  const audible = 500;
  for (let i = 0; i < samples; i += 1) {
    const magnitude = Math.abs(at(i));
    peak = Math.max(peak, magnitude);
    if (magnitude > audible) {
      first = first < 0 ? i : first;
      last = i;
    }
  }
  let crossings = 0;
  for (let i = first + 1; first >= 0 && i <= last; i += 1) {
    if ((at(i - 1) < 0) !== (at(i) < 0)) {
      crossings += 1;
    }
  }
  const audibleSeconds = (last - first) / WAV_SAMPLE_RATE;
  return {
    riff: wav.toString("ascii", 0, 4),
    riffSize: wav.readUInt32LE(4),
    wave: wav.toString("ascii", 8, 12),
    fmt: wav.toString("ascii", 12, 16),
    fmtSize: wav.readUInt32LE(16),
    formatTag: wav.readUInt16LE(20),
    channels: wav.readUInt16LE(22),
    sampleRate: wav.readUInt32LE(24),
    byteRate: wav.readUInt32LE(28),
    blockAlign: wav.readUInt16LE(32),
    bitsPerSample: wav.readUInt16LE(34),
    data: wav.toString("ascii", 36, 40),
    dataBytes: wav.readUInt32LE(40),
    seconds: samples / WAV_SAMPLE_RATE,
    peak: peak / 0x8000,
    hz: audibleSeconds > 0 ? crossings / 2 / audibleSeconds : 0
  };
}

/**
 * `upload` is a WAV of the kind the audio.cpp server reads -- RIFF/WAVE, a `fmt `
 * chunk that declares PCM, mono, 16000 Hz, 16 bit, and a data chunk that is exactly
 * the rest of the file -- carrying an audible tone at `hz` that lasts `seconds` (or,
 * for a recording whose length is not fixed, at least `minSeconds`).
 */
function expectWav(upload: Buffer, expected: { hz: number; seconds?: number; minSeconds?: number }): void {
  const wav = readWav(upload);
  expect(wav).toMatchObject({
    riff: "RIFF",
    riffSize: upload.length - 8,
    wave: "WAVE",
    fmt: "fmt ",
    fmtSize: 16,
    formatTag: 1, // PCM
    channels: 1,
    sampleRate: WAV_SAMPLE_RATE,
    byteRate: WAV_SAMPLE_RATE * 2,
    blockAlign: 2,
    bitsPerSample: 16,
    data: "data",
    dataBytes: upload.length - WAV_HEADER_BYTES
  });
  expect(wav.peak, "the WAV is not silence").toBeGreaterThan(0.05);
  expect(Math.abs(wav.hz - expected.hz), `the tone survived the conversion at its pitch (${wav.hz} Hz, not ${expected.hz} Hz)`).toBeLessThan(
    expected.hz * 0.03
  );
  if (expected.seconds !== undefined) {
    // Within a tenth of a second: an MP3's decoder delay and padding (about 40 ms here)
    // are part of what the browser decodes.
    expect(Math.abs(wav.seconds - expected.seconds), `the WAV is as long as the clip (${wav.seconds} s, not ${expected.seconds} s)`).toBeLessThan(0.1);
  }
  if (expected.minSeconds !== undefined) {
    expect(wav.seconds, "the WAV holds the recording").toBeGreaterThanOrEqual(expected.minSeconds);
  }
}

/** One part of a multipart/form-data body. */
type FormPart = { name: string; filename?: string; contentType?: string; value: Buffer };

/**
 * Split a multipart/form-data body into its parts. Done on a latin1 view of the
 * bytes (one character per byte, so the offsets are byte offsets and the binary
 * file part survives the round trip unchanged).
 */
function parseMultipart(contentType: string, body: Buffer): FormPart[] {
  const boundary = /boundary=(?:"([^"]+)"|([^;\s]+))/i.exec(contentType);
  if (!boundary) {
    return [];
  }
  const delimiter = `--${boundary[1] ?? boundary[2]}`;
  const text = body.toString("latin1");
  const parts: FormPart[] = [];
  // The first chunk is the preamble, the last is the closing "--" terminator.
  for (const chunk of text.split(delimiter).slice(1)) {
    if (chunk.startsWith("--")) {
      break;
    }
    const content = chunk.replace(/^\r\n/, "").replace(/\r\n$/, "");
    const split = content.indexOf("\r\n\r\n");
    if (split < 0) {
      continue;
    }
    const head = content.slice(0, split);
    const disposition = /name="([^"]*)"(?:;\s*filename="([^"]*)")?/i.exec(head);
    if (!disposition) {
      continue;
    }
    parts.push({
      name: disposition[1],
      filename: disposition[2],
      contentType: /^content-type:\s*(.+)$/im.exec(head)?.[1].trim(),
      value: Buffer.from(content.slice(split + 4), "latin1")
    });
  }
  return parts;
}

type StubCall = {
  method: string;
  path: string;
  contentType: string;
  /** The name of every multipart part, in order and repeats included. */
  partNames: string[];
  /** The text form fields of a multipart body, by name. */
  fields: Record<string, string>;
  /** The bytes of the multipart `file` part, when there is one. */
  file?: Buffer;
  /** The file name the `file` part was sent under. */
  filename?: string;
  /** The media type the `file` part was sent under. */
  fileType?: string;
};

type Stub = {
  port: number;
  /** Every request the stub has received, in order. */
  calls: StubCall[];
  /** The POST /v1/audio/transcriptions requests among them. */
  transcriptionCalls(): StubCall[];
  /**
   * Makes the next `count` streamed answers pause, in turn, right after one of their
   * delta frames: a hold per pause. `reached` settles once the stub has written that
   * delta and is waiting; `release()` lets the stream go on. A hold nobody releases
   * lets go by itself after HOLD_SAFETY_MS.
   */
  holdStream(count: number): Hold[];
  /** Whether a streamed answer is paused at a hold right now. */
  streamHeld(): boolean;
  close(): Promise<void>;
};

type Hold = { reached: Promise<void>; release: () => void };

/** One SSE `data:` frame. */
function sseFrame(payload: unknown): string {
  return `data: ${typeof payload === "string" ? payload : JSON.stringify(payload)}\n\n`;
}

/**
 * The audio.cpp stand-in, kept to what the gateway actually talks to and honest
 * about the shapes and statuses the real server uses:
 *
 *   GET  /health                   -> 200 (the application's health path)
 *   GET  /v1/models                -> the OpenAI list of LOADED models, each with
 *                                     the `task` audio.cpp states (the capability
 *                                     signal) and, for the streaming model, the
 *                                     `mode` ("streaming") the
 *                                     transcription_streaming verdict is read from
 *   POST /v1/audio/transcriptions  -> a multipart request answered with a JSON
 *                                     {"text","timing"} document, the timing block
 *                                     carrying audio_duration_ms; the nth answer
 *                                     carries the nth transcript and the first is
 *                                     held for FIRST_ANSWER_DELAY_MS; a model it does
 *                                     not serve is answered 500, which is what the
 *                                     real server does for a client error. With
 *                                     stream=true the streaming model answers 200
 *                                     text/event-stream: a transcript.text.delta per
 *                                     increment (each its own chunk, paused at the
 *                                     test's holds), transcript.text.done (the whole
 *                                     text and timing.ttft_ms, no audio_duration_ms),
 *                                     then [DONE]; any other model answers 500 "...
 *                                     requires a model configured with
 *                                     mode=streaming", as the real server does.
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
  let answered = 0;
  const pendingHolds: { markReached: () => void; released: Promise<void> }[] = [];
  let held = 0;

  /** Waits at the next pending hold, if one is armed. */
  const pauseAtHold = async (): Promise<void> => {
    const hold = pendingHolds.shift();
    if (!hold) {
      return;
    }
    held += 1;
    hold.markReached();
    let safety: NodeJS.Timeout | undefined;
    await Promise.race([
      hold.released,
      new Promise<void>((resolve) => {
        safety = setTimeout(resolve, HOLD_SAFETY_MS);
      })
    ]);
    clearTimeout(safety);
    held -= 1;
  };

  const streamTranscript = async (res: http.ServerResponse): Promise<void> => {
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
    res.flushHeaders();
    for (const [i, delta] of STREAM_DELTAS.entries()) {
      res.write(sseFrame({ type: "transcript.text.delta", delta }));
      if (i < STREAM_DELTAS.length - 1) {
        await pauseAtHold();
      }
    }
    res.write(sseFrame({ type: "transcript.text.done", text: STREAM_TEXT, timing: { ttft_ms: STREAM_TTFT_MS } }));
    res.write(sseFrame("[DONE]"));
    res.end();
  };

  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const method = req.method ?? "";
      const path = req.url ?? "";
      const contentType = String(req.headers["content-type"] ?? "");
      const parts = parseMultipart(contentType, Buffer.concat(chunks));
      const fields: Record<string, string> = {};
      for (const part of parts) {
        if (part.name !== "file") {
          fields[part.name] = part.value.toString("utf8");
        }
      }
      const filePart = parts.find((p) => p.name === "file");
      calls.push({
        method,
        path,
        contentType,
        partNames: parts.map((p) => p.name),
        fields,
        file: filePart?.value,
        filename: filePart?.filename,
        fileType: filePart?.contentType
      });

      if (method === "GET" && path === "/health") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end('{"status":"ok"}');
        return;
      }
      if (method === "GET" && path === "/v1/models") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(
          JSON.stringify({
            object: "list",
            data: [
              // No mode: this model carries no transcription_streaming verdict.
              { id: ASR_UPSTREAM, object: "model", task: "asr" },
              { id: STREAM_UPSTREAM, object: "model", task: "asr", mode: "streaming" }
            ]
          })
        );
        return;
      }
      if (method === "POST" && path === TRANSCRIPTION_PATH) {
        if (fields.model !== ASR_UPSTREAM && fields.model !== STREAM_UPSTREAM) {
          res.writeHead(500, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: { message: `model not loaded: ${fields.model ?? ""}` } }));
          return;
        }
        if (fields.stream === "true") {
          if (fields.model !== STREAM_UPSTREAM) {
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ error: { message: "streaming transcription requires a model configured with mode=streaming", type: "server_error" } }));
            return;
          }
          void streamTranscript(res);
          return;
        }
        const text = TRANSCRIPTS[Math.min(answered, TRANSCRIPTS.length - 1)];
        const delay = answered === 0 ? FIRST_ANSWER_DELAY_MS : 0;
        answered += 1;
        setTimeout(() => {
          res.writeHead(200, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ text, timing: { audio_duration_ms: CLIP_DURATION_MS } }));
        }, delay);
        return;
      }
      res.writeHead(404);
      res.end();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    port: (server.address() as AddressInfo).port,
    calls,
    transcriptionCalls: () => calls.filter((c) => c.method === "POST" && c.path === TRANSCRIPTION_PATH),
    holdStream: (count) =>
      Array.from({ length: count }, () => {
        let markReached!: () => void;
        let release!: () => void;
        const reached = new Promise<void>((resolve) => {
          markReached = resolve;
        });
        const released = new Promise<void>((resolve) => {
          release = resolve;
        });
        pendingHolds.push({ markReached, released });
        return { reached, release };
      }),
    streamHeld: () => held > 0,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      })
  };
}

let page: Page;
let stub: Stub;
let serverId = "";
let applicationId = "";
// The WAV the first upstream call received: scenario 5 tells the follow-up clip's upload
// apart from it.
let firstUpload: Buffer;
// The WAV the streaming model's upstream call received (scenario 9 reads the duration its
// header declares).
let streamedUpload: Buffer;

test.describe.configure({ mode: "serial" });

/**
 * Replaces navigator.mediaDevices.getUserMedia -- in every document the page loads,
 * reloads included -- with a stream a WebAudio oscillator makes: a one-tone "microphone"
 * that needs no device, no permission prompt and no system audio. Nothing downstream of
 * the capture source is faked: the recorder asks for the stream exactly as it would for a
 * microphone and the MediaRecorder it builds on it really encodes it.
 */
async function installSyntheticMicrophone(target: Page): Promise<void> {
  await target.addInitScript(() => {
    navigator.mediaDevices.getUserMedia = async () => {
      const context = new AudioContext();
      await context.resume();
      const tone = context.createOscillator();
      tone.frequency.value = 440;
      const destination = context.createMediaStreamDestination();
      tone.connect(destination);
      tone.start();
      return destination.stream;
    };
  });
}

// The address of every chat blob the page has fetched so far (scenario 6 finds the one
// it is waiting for by its not being among them).
const fetchedBlobPaths = new Set<string>();

test.beforeAll(async ({ browser }) => {
  stub = await startAudioCppStub();
  page = await browser.newPage();
  page.on("response", (response) => {
    const blobPath = new URL(response.url()).pathname;
    if (blobPath.startsWith("/api/portal/chat-blobs/")) {
      fetchedBlobPaths.add(blobPath);
    }
  });
  await installSyntheticMicrophone(page);
});

test.afterAll(async () => {
  await page?.close();
  await stub?.close();
});

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The row of any portal table whose accessible text contains `name`. */
function row(name: string): Locator {
  return page.getByRole("row", { name: new RegExp(escapeRegExp(name)) });
}

// The turns of the CURRENT chat: .last() so an earlier bubble never trips strict-mode
// matching.
const assistantTurns = () => page.locator('[data-role="assistant"]');
const userTurns = () => page.locator('[data-role="user"]');
const lastAssistant = () => assistantTurns().last();
const lastUser = () => userTurns().last();

/** The transcription composer: the form that holds the audio input. */
const composer = () => page.locator("form").filter({ hasText: t.chatTranscriptionInputLabel });
const sendButton = () => page.getByRole("button", { name: t.send, exact: true });

/** The verdict of one capability the gateway holds for each mapping (null: no row). */
async function capabilityVerdicts(capability: string): Promise<Record<string, string | null>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string }[] }[];
  };
  const out: Record<string, string | null> = {};
  for (const mapping of body.data) {
    out[mapping.gateway_model_name] = mapping.capabilities.find((c) => c.capability === capability)?.verdict ?? null;
  }
  return out;
}

const transcriptionVerdicts = () => capabilityVerdicts("transcription");
const streamingVerdicts = () => capabilityVerdicts("transcription_streaming");

/** The recorded usage rows of the transcription endpoint. */
type UsageRow = {
  model: string;
  provider_model: string;
  http_status: number;
  status: string;
  billing_unit?: string;
  billing_quantity?: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

async function transcriptionUsageRows(): Promise<UsageRow[]> {
  const resp = await page.request.get(`/api/portal/usage?req_path=${encodeURIComponent(TRANSCRIPTION_PATH)}&limit=100`);
  expect(resp.ok(), `reading the usage rows: ${resp.status()} ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { data: UsageRow[] }).data;
}

/**
 * Waits for the stub to reach `hold` (it has written a delta and is paused). A run that
 * never got a stream (the gateway answered buffered, or the request never went out) never
 * reaches it, and the spec must say so instead of idling out the whole test budget.
 */
async function stubReached(hold: Hold, what: string): Promise<void> {
  let timer: NodeJS.Timeout | undefined;
  try {
    await Promise.race([
      hold.reached,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`the stub never streamed ${what}: the run's stream=true did not reach the streaming model`)), 15000);
      })
    ]);
  } finally {
    clearTimeout(timer);
  }
}

/** Opens a fresh chat (no model picked yet) from the chat view. */
async function openNewChat(): Promise<void> {
  await page.getByRole("link", { name: t.chat }).click();
  await page.getByRole("button", { name: t.chatNewChat }).click();
  await expect(assistantTurns()).toHaveCount(0);
}

/** Opens the settings panel if it is not open already. */
async function openSettings(): Promise<void> {
  if (!(await page.locator("#chat-transcription-language").isVisible())) {
    await page.getByRole("button", { name: t.chatSettings, exact: true }).click();
  }
  await expect(page.locator("#chat-transcription-language")).toBeVisible();
}

/**
 * Attaches `clip` through the composer's attach control (the file input the button
 * wraps), and returns once the composer shows it: a player for the waiting clip. What
 * the player holds is the CONVERTED clip -- an inline audio/wav data url, never the
 * WebM or MP3 that was picked -- which is what the user hears before sending and what
 * is then stored and sent.
 */
async function attachClip(clip: Fixture): Promise<void> {
  await composer().getByLabel(t.chatAttachAudio, { exact: true }).setInputFiles({ name: clip.name, mimeType: clip.mimeType, buffer: clip.bytes });
  await expect(composer().locator("audio")).toHaveAttribute("src", /^data:audio\/wav;base64,/);
  await expectPlayableClip(composer().locator("audio"));
}

/**
 * An audio player once it has its bytes and the browser has decoded them: no media
 * error, metadata loaded, and the duration is the clip's.
 */
async function expectDecodedClip(player: Locator, seconds: number | null = CLIP_SECONDS): Promise<void> {
  await expect(player).toBeVisible();
  await expect
    .poll(
      () =>
        player.evaluate((el) => {
          const audio = el as HTMLAudioElement;
          return { error: audio.error?.code ?? null, ready: audio.readyState >= 1, seconds: audio.duration };
        }),
      { timeout: 15000, intervals: [250] }
    )
    // A recording's length is whatever the take lasted, so it is not asserted exactly:
    // the clip is decoded when the metadata loaded without a media error.
    .toMatchObject({ error: null, ready: true, ...(seconds === null ? {} : { seconds: expect.closeTo(seconds, 1) }) });
}

/**
 * The clip's player, once it plays an object URL (the portal made it from the bytes it
 * holds: the attached file, or the stored clip fetched out of the chat blob store) and
 * the browser has decoded it.
 */
async function expectPlayableClip(player: Locator): Promise<void> {
  await expect(player).toHaveAttribute("src", /^blob:|^data:audio\//);
  await expectDecodedClip(player);
}

/**
 * A turn's player once it plays the STORED clip (an object URL, never an inline data
 * url). `seconds` is the clip's length, or null for one whose length is not asserted.
 */
async function expectStoredClip(player: Locator, seconds: number | null = CLIP_SECONDS): Promise<void> {
  await expect(player).toHaveAttribute("src", /^blob:/, { timeout: 15000 });
  await expectDecodedClip(player, seconds);
}

// --- Scenario 1 -------------------------------------------------------------

test("1 — an audio_cpp application created through the portal form is probed, and the portal lists the asr model as transcription-capable and the unprobed one as not", async () => {
  await login(page, DEV_EMAIL, DEV_PASSWORD);

  // The AI server: Domain 127.0.0.1, because the gateway reaches the application at
  // server.Domain:app.Port. Created through the API (it is only the precondition
  // here); the application below goes through the real form.
  const candidates = await page.request.get("/api/portal/server-admin-group-candidates");
  expect(candidates.ok(), `admin-group candidates: ${candidates.status()} ${await candidates.text()}`).toBe(true);
  const groups = ((await candidates.json()) as { data: { id: string }[] }).data;
  expect(groups.length, "the dev seed must offer at least one admin group for CreateServer").toBeGreaterThan(0);
  const createdServer = await page.request.post("/api/portal/servers", {
    headers: CSRF,
    data: { name: SERVER_NAME, domain: "127.0.0.1", status: "active", admin_group_ids: [groups[0].id], owner_ids: [] }
  });
  expect(createdServer.ok(), `creating the server: ${createdServer.status()} ${await createdServer.text()}`).toBe(true);
  serverId = ((await createdServer.json()) as { id: string }).id;

  // The application, through the Servers view's form: ONLY the type, the port and a
  // short probe interval are entered; the openai_audio flavor and the /health path
  // come from the form's audio_cpp defaults. The interval is pinned to the 5s
  // minimum so the capability probe repeats quickly once the loop has picked the
  // application up.
  await page.getByRole("link", { name: t.servers }).click();
  await expect(row(SERVER_NAME)).toBeVisible();
  await row(SERVER_NAME).getByRole("button", { name: t.applicationManage }).click();
  await page.getByRole("button", { name: t.applicationCreate }).click();
  await page.getByRole("combobox", { name: t.applicationType }).click();
  await page.getByRole("option", { name: "audio_cpp", exact: true }).click();
  await page.locator("#application-port").fill(String(stub.port));
  await page.getByRole("combobox", { name: t.applicationHealthIntervalLabel }).click();
  await page.getByRole("option", { name: t.applicationHealthIntervalCustom, exact: true }).click();
  await page.locator("#application-health-interval-seconds").fill("5");
  const [created] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/portal/servers/${serverId}/applications` && r.request().method() === "POST"
    ),
    page.getByRole("button", { name: t.applicationCreate }).click()
  ]);
  expect(created.ok(), `creating the audio_cpp application: ${created.status()} ${await created.text()}`).toBe(true);
  const app = (await created.json()) as { id: string; type: string; port: number; api_flavors: string[] };
  expect(app).toMatchObject({ type: "audio_cpp", port: stub.port, api_flavors: ["openai_audio"] });
  applicationId = app.id;

  // One mapping per side of the offering rule: a model the stub lists with task asr
  // (a transcription yes) and one it does not list (no verdict at all), and the model the
  // stub lists with task asr and mode streaming (a transcription yes and, on top, a
  // transcription_streaming yes).
  for (const [gateway, upstream] of [
    [ASR_MODEL, ASR_UPSTREAM],
    [UNPROBED_MODEL, UNPROBED_UPSTREAM],
    [STREAM_MODEL, STREAM_UPSTREAM]
  ]) {
    const mapped = await page.request.post(`/api/portal/applications/${applicationId}/mappings`, {
      headers: CSRF,
      data: { gateway_model_name: gateway, app_model_name: upstream }
    });
    expect(mapped.ok(), `creating the mapping ${gateway}: ${mapped.status()} ${await mapped.text()}`).toBe(true);
  }

  // The probe writes the verdicts; nothing here seeds them. The wait covers the
  // loop's own tick (up to the 30s system cadence) before the 5s per-application
  // cadence takes over.
  await expect
    .poll(transcriptionVerdicts, { timeout: 90000, intervals: [1000] })
    .toEqual({ [ASR_MODEL]: "yes", [UNPROBED_MODEL]: null, [STREAM_MODEL]: "yes" });

  // The same pass states the stream model's mode as the transcription_streaming verdict;
  // the model whose listing states no mode gets no row (unknown, answered buffered).
  await expect
    .poll(streamingVerdicts, { timeout: 30000, intervals: [1000] })
    .toEqual({ [ASR_MODEL]: null, [UNPROBED_MODEL]: null, [STREAM_MODEL]: "yes" });

  // The portal's model listing (what the chat's picker is built from) carries the
  // verdict as its `transcription` flag -- and an asr model is not a speech one.
  await expect
    .poll(
      async () => {
        const resp = await page.request.get("/api/portal/models");
        const data = ((await resp.json()) as { data: { id: string; transcription?: boolean; speech?: boolean }[] }).data;
        return Object.fromEntries(
          data
            .filter((m) => [ASR_MODEL, UNPROBED_MODEL, STREAM_MODEL].includes(m.id))
            .map((m) => [m.id, { transcription: m.transcription === true, speech: m.speech === true }])
        );
      },
      { timeout: 30000, intervals: [500] }
    )
    .toMatchObject({ [ASR_MODEL]: { transcription: true, speech: false }, [STREAM_MODEL]: { transcription: true, speech: false } });
});

// --- Scenario 2 -------------------------------------------------------------

test("2 — the chat offers the asr model and not the unprobed one, and picking it switches the composer to transcription mode", async () => {
  await openNewChat();

  const picker = page.getByRole("combobox", { name: t.chatModel });
  await picker.click();
  await expect(page.getByRole("option", { name: ASR_MODEL })).toBeVisible();
  // An unprobed model is an openai_audio model too, but with no verdict the chat has
  // nothing it could send it, so it is not offered at all.
  await expect(page.getByRole("option", { name: UNPROBED_MODEL })).toHaveCount(0);
  await page.getByRole("option", { name: ASR_MODEL }).click();

  // The composer: an audio input in place of the text prompt, and Send closed until a
  // clip is attached.
  await expect(page.getByText(t.chatTranscriptionInputLabel)).toBeVisible();
  await expect(page.getByText(t.chatTranscriptionOnlyHint)).toBeVisible();
  await expect(composer().getByLabel(t.chatAttachAudio, { exact: true })).toBeAttached();
  await expect(page.locator("#chat-message")).toHaveCount(0);
  await expect(sendButton()).toBeDisabled();

  // The settings are the transcription knobs, not the text chat's.
  await page.getByRole("button", { name: t.chatSettings, exact: true }).click();
  await expect(page.locator("#chat-transcription-language")).toBeVisible();
  await expect(page.locator("#chat-transcription-prompt")).toBeVisible();
  await expect(page.locator("#chat-transcription-temperature")).toBeVisible();
  await expect(page.locator("#chat-system-prompt")).toHaveCount(0);
  await expect(page.locator("#chat-max-tokens")).toHaveCount(0);
  await expect(page.locator("#chat-speech-voice")).toHaveCount(0);
  // The reply is always text: no format control, just the statement of it.
  await expect(page.getByText(t.chatTranscriptionFormatHint)).toBeVisible();
});

// --- Scenario 3 -------------------------------------------------------------

test("3 — an attached WebM is converted to a WAV, relayed as one multipart /v1/audio/transcriptions request, and renders as a transcript turn next to its playable, downloadable clip", async () => {
  await openSettings();
  await page.locator("#chat-transcription-language").fill(LANGUAGE);
  await page.locator("#chat-transcription-prompt").fill(PROMPT);
  await page.locator("#chat-transcription-temperature").fill(String(TEMPERATURE));

  await attachClip(FIRST_CLIP);
  await expect(sendButton()).toBeEnabled();

  // The user's clip comes back from the chat blob store once the turn is committed,
  // fetched same-origin.
  const blobResponse = page.waitForResponse((r) => new URL(r.url()).pathname.startsWith("/api/portal/chat-blobs/"));
  await sendButton().click();

  // The upstream is held for a moment (see FIRST_ANSWER_DELAY_MS): the chat is in its
  // non-streaming wait state -- worded for a transcript, not the text chat's reasoning
  // counter ("0 characters") for a run that emits nothing to count.
  await expect(page.getByText(t.chatTranscriptionRunPending).first()).toBeVisible();
  await expect(page.getByText(t.chatTranscriptionNoIntermediateNews).first()).toBeVisible();

  // The answer: the transcript, as a plain text turn.
  await expect(lastAssistant()).toContainText(TRANSCRIPTS[0]);
  await expect(assistantTurns()).toHaveCount(1);
  await expect(lastAssistant().getByRole("alert")).toHaveCount(0);

  // The composer let go of the clip it sent: Send is closed again until the next one.
  await expect(composer().locator("audio")).toHaveCount(0);
  await expect(sendButton()).toBeDisabled();

  // The user's turn is the clip: a player that plays the stored bytes -- the converted
  // WAV, served as audio/wav out of the owner-scoped blob endpoint.
  await expect(userTurns()).toHaveCount(1);
  await expectStoredClip(lastUser().locator("audio"));
  const blob = await blobResponse;
  expect(blob.status()).toBe(200);
  expect(blob.headers()["content-type"]).toMatch(/^audio\/wav/);
  const stored = await blob.body();
  expectWav(stored, { hz: FIRST_CLIP.hz, seconds: CLIP_SECONDS });
  expect(Buffer.compare(stored, FIRST_CLIP.bytes), "what is stored is the converted WAV, not the attached WebM").not.toBe(0);

  // The download saves the same bytes, named as the user's own input, not as audio the
  // gateway generated.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    lastUser().getByRole("button", { name: t.chatDownloadAudio, exact: true }).click()
  ]);
  expect(download.suggestedFilename()).toMatch(/^input-audio-[A-Za-z0-9._-]+\.wav$/);
  expect(Buffer.compare(fs.readFileSync(await download.path()), stored), "the downloaded file must equal the stored WAV byte for byte").toBe(0);

  // The untitled chat is named after its transcript.
  await expect(page.getByLabel(t.chatSidebarLabel)).toContainText(TRANSCRIPTS[0]);

  // Exactly one upstream transcription call: multipart, the gateway model name
  // rewritten to the application's own model id, the thread's settings as form fields
  // (temperature in its plain decimal form) and nothing else, and the clip arriving as
  // the file part: a WAV (never the attached WebM), under the name and media type of a
  // WAV, byte for byte what the chat store holds -- the gateway relays, it never
  // transcodes.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(1);
  expect(calls[0].contentType).toMatch(/^multipart\/form-data;/);
  expect(calls[0].fields).toEqual({ model: ASR_UPSTREAM, language: LANGUAGE, prompt: PROMPT, temperature: String(TEMPERATURE) });
  expect(calls[0].filename).toBe("audio.wav");
  expect(calls[0].fileType).toBe("audio/wav");
  expect(calls[0].file, "the upstream received a file part").toBeDefined();
  const upload = calls[0].file as Buffer;
  expect(upload.subarray(0, 4).toString("hex"), "the attached clip is a WebM (EBML) stream, the upload is not").not.toBe("1a45dfa3");
  expectWav(upload, { hz: FIRST_CLIP.hz, seconds: CLIP_SECONDS });
  expect(Buffer.compare(upload, stored), "the relayed upload must equal the stored WAV byte for byte").toBe(0);
  firstUpload = upload;
  // The chat went to the transcription endpoint and nowhere else: no chat-completions
  // call, and no POST of any other kind, reached the stub.
  expect(stub.calls.filter((c) => c.path.startsWith("/v1/chat"))).toHaveLength(0);
  expect(stub.calls.filter((c) => c.method === "POST").map((c) => c.path)).toEqual([TRANSCRIPTION_PATH]);
});

// --- Scenario 4 -------------------------------------------------------------

test("4 — the transcription thread survives a full reload: still a transcription thread, settings restored, the clip plays again from the store, the transcript is still there", async () => {
  await page.reload();
  await page.getByRole("link", { name: t.chat }).click();

  // The thread was pinned to the transcription kind by its first send, so its
  // composer is the transcription one again without anything being picked.
  await expect(page.getByText(t.chatTranscriptionInputLabel)).toBeVisible();
  await expect(lastAssistant()).toContainText(TRANSCRIPTS[0]);
  await expectStoredClip(lastUser().locator("audio"));
  await expect(assistantTurns()).toHaveCount(1);
  await expect(userTurns()).toHaveCount(1);

  // The knobs the first run was sent with are the thread's own settings again.
  await openSettings();
  await expect(page.locator("#chat-transcription-language")).toHaveValue(LANGUAGE);
  await expect(page.locator("#chat-transcription-prompt")).toHaveValue(PROMPT);
  await expect(page.locator("#chat-transcription-temperature")).toHaveValue(String(TEMPERATURE));
  // Reading the transcript back sent nothing upstream.
  expect(stub.transcriptionCalls()).toHaveLength(1);
});

// --- Scenario 5 -------------------------------------------------------------

test("5 — a follow-up MP3 is a second upstream call with the new clip's WAV, and the advanced passthrough reaches the form next to the thread's own settings", async () => {
  await openSettings();
  await page.getByRole("button", { name: t.chatTranscriptionAdvanced, exact: true }).click();
  await page.locator("#chat-transcription-passthrough").fill(JSON.stringify(PASSTHROUGH));

  // A transcription turn IS its clip: with the first one sent, Send is closed again
  // until another is attached.
  await expect(sendButton()).toBeDisabled();
  await attachClip(SECOND_CLIP);
  await expect(sendButton()).toBeEnabled();
  await sendButton().click();

  await expect(assistantTurns()).toHaveCount(2);
  await expect(lastAssistant()).toContainText(TRANSCRIPTS[1]);
  await expect(userTurns()).toHaveCount(2);
  await expectStoredClip(lastUser().locator("audio"));
  // The first turn is untouched: its transcript and its clip are still there.
  await expect(assistantTurns().first()).toContainText(TRANSCRIPTS[0]);
  await expectStoredClip(userTurns().first().locator("audio"));
  await expect(page.locator('[data-role="assistant"]').getByRole("alert")).toHaveCount(0);

  // The second upstream call: the NEW clip only (an earlier turn's clip is never
  // re-sent) -- the MP3 converted to a WAV like the first clip was, at its own pitch --
  // the thread's own settings, and the passthrough fields relayed as form text (each
  // value as its literal) -- but not the passthrough "language", which repeats a set
  // setting: the thread's own value wins and the field appears once.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(2);
  expect(calls[1].filename).toBe("audio.wav");
  expect(calls[1].fileType).toBe("audio/wav");
  const upload = calls[1].file as Buffer;
  expectWav(upload, { hz: SECOND_CLIP.hz, seconds: CLIP_SECONDS });
  expect(Buffer.compare(upload, firstUpload), "the second upload must be the second clip, not the first again").not.toBe(0);
  expect(calls[1].fields).toEqual({
    model: ASR_UPSTREAM,
    language: LANGUAGE,
    prompt: PROMPT,
    temperature: String(TEMPERATURE),
    word_timestamps: "true",
    seed: "7"
  });
  expect([...calls[1].partNames].sort()).toEqual(["file", "language", "model", "prompt", "seed", "temperature", "word_timestamps"]);
  expect(stub.calls.filter((c) => c.path.startsWith("/v1/chat"))).toHaveLength(0);
});

// --- Scenario 6 ---------------------------------------------------------------

test("6 — a clip recorded in the browser is converted like an attached one: the recorder's WebM never reaches the wire, a WAV reaches the upstream and the chat store", async () => {
  // The REAL recorder, on the synthetic microphone: start, let the clock reach one
  // second so the take holds audio, stop, and use the take.
  await composer().getByRole("button", { name: t.chatRecordStart, exact: true }).click();
  await expect(page.getByRole("timer", { name: t.chatRecordElapsed })).toHaveText("0:01", { timeout: 15000 });
  await composer().getByRole("button", { name: t.chatRecordStop, exact: true }).click();
  await expect(composer().getByLabel(t.chatRecordPreview)).toBeVisible();
  await composer().getByRole("button", { name: t.chatRecordUse, exact: true }).click();

  // The take is the composer's clip, shown under the name a recording gets (it has no
  // file name), and Send is open. What the composer holds of it is the converted WAV:
  // the recorder made an audio/webm (Opus), and that is not what is kept.
  await expect(composer().getByText(t.chatAudioRecordingLabel, { exact: true })).toBeVisible();
  await expect(composer().locator("audio")).toHaveAttribute("src", /^data:audio\/wav;base64,/);
  await expect(sendButton()).toBeEnabled();

  // The stored copy of the recording is the third blob the chat serves. Every clip is a
  // WAV now, so the new blob is told apart from the two already fetched by its address.
  const known = new Set(fetchedBlobPaths);
  const blobResponse = page.waitForResponse((r) => {
    const blobPath = new URL(r.url()).pathname;
    return blobPath.startsWith("/api/portal/chat-blobs/") && !known.has(blobPath);
  });
  await sendButton().click();

  await expect(assistantTurns()).toHaveCount(3);
  await expect(lastAssistant()).toContainText(TRANSCRIPTS[2]);
  await expect(userTurns()).toHaveCount(3);
  await expect(lastAssistant().getByRole("alert")).toHaveCount(0);
  await expect(composer().locator("audio")).toHaveCount(0);
  await expectStoredClip(lastUser().locator("audio"), null);

  // The third upstream call: a WAV made from the recorder's own audio -- the file part
  // under the name and media type of a WAV, not the audio.webm the recorder produced
  // (an EBML/WebM stream starts 1a45dfa3; a WAV starts RIFF) -- carrying the take's
  // tone (at least the second the clock counted), and the thread's settings unchanged.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(3);
  expect(calls[2].filename).toBe("audio.wav");
  expect(calls[2].fileType).toBe("audio/wav");
  const upload = calls[2].file as Buffer;
  expect(upload.subarray(0, 4).toString("ascii"), "the upload is a WAV, not the recorder's WebM").toBe("RIFF");
  expectWav(upload, { hz: MICROPHONE_HZ, minSeconds: 0.75 });
  expect(calls[2].fields).toMatchObject({ model: ASR_UPSTREAM, language: LANGUAGE, prompt: PROMPT, temperature: String(TEMPERATURE) });

  // What the chat store holds of the clip is what the upstream received.
  const blob = await blobResponse;
  expect(blob.status()).toBe(200);
  expect(blob.headers()["content-type"]).toMatch(/^audio\/wav/);
  expect(Buffer.compare(await blob.body(), upload), "the stored recording must equal the relayed upload byte for byte").toBe(0);
});

// --- Scenario 7 -------------------------------------------------------------

test("7 — each transcription run left an audio_second usage row of 2, not a token-metered one", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === ASR_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(3);
  const served = (await transcriptionUsageRows()).filter((r) => r.model === ASR_MODEL);
  for (const usage of served) {
    expect(usage).toMatchObject({
      http_status: 200,
      status: "success",
      provider_model: ASR_UPSTREAM,
      billing_unit: "audio_second",
      billing_quantity: CLIP_SECONDS,
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0
    });
  }
});

// --- Scenario 8 -------------------------------------------------------------

test("8 — a streaming model's transcript fills in as the deltas arrive, and the committed transcript is the done event's text", async () => {
  await openNewChat();
  await page.getByRole("combobox", { name: t.chatModel }).click();
  await page.getByRole("option", { name: STREAM_MODEL }).click();
  await expect(page.getByText(t.chatTranscriptionInputLabel)).toBeVisible();
  await attachClip(FIRST_CLIP);
  await expect(sendButton()).toBeEnabled();

  const before = stub.transcriptionCalls().length;
  // The stub pauses its stream after the first and after the second delta until this
  // test lets it go: at each pause the turn is looked at, with the upstream provably
  // still mid-stream, so what is on screen can only have come from the deltas so far.
  const holds = stub.holdStream(2);
  await sendButton().click();

  // First pause: one delta written. The wait state worded for a transcript is replaced
  // by that delta's text -- not by the whole transcript -- and the run is not over.
  await stubReached(holds[0], "its first delta");
  await expect(lastAssistant()).toContainText(STREAM_DELTAS[0].trim());
  await expect(lastAssistant()).not.toContainText(STREAM_DELTAS[1].trim());
  await expect(page.getByText(t.chatTranscriptionRunPending)).toHaveCount(0);
  expect(stub.streamHeld(), "the first delta is on screen while the upstream is still streaming").toBe(true);

  // Second pause: two deltas written, and the turn has grown to both -- still not the
  // third, and still before the end of the stream.
  holds[0].release();
  await stubReached(holds[1], "its second delta");
  await expect(lastAssistant()).toContainText((STREAM_DELTAS[0] + STREAM_DELTAS[1]).trim());
  await expect(lastAssistant()).not.toContainText(STREAM_DELTAS[2]);
  expect(stub.streamHeld(), "the second delta is on screen while the upstream is still streaming").toBe(true);

  // The stream runs to its end: the committed transcript is the done event's text. Its
  // final period is in no delta, so seeing it is seeing the restated whole text.
  holds[1].release();
  await expect(lastAssistant()).toContainText(STREAM_TEXT);
  await expect(assistantTurns()).toHaveCount(1);
  await expect(lastAssistant().getByRole("alert")).toHaveCount(0);
  await expect(composer().locator("audio")).toHaveCount(0);
  await expect(sendButton()).toBeDisabled();

  // The user's turn is still the clip, played from the chat blob store.
  await expect(userTurns()).toHaveCount(1);
  await expectStoredClip(lastUser().locator("audio"));

  // One upstream call: the run's stream=true reached the model (the gateway relays it on
  // the verdict), the gateway name rewritten to the application's own id, and the clip
  // the converted WAV, as for a one-shot run.
  const calls = stub.transcriptionCalls().slice(before);
  expect(calls).toHaveLength(1);
  expect(calls[0].fields).toEqual({ model: STREAM_UPSTREAM, stream: "true" });
  expect(calls[0].filename).toBe("audio.wav");
  expect(calls[0].fileType).toBe("audio/wav");
  expectWav(calls[0].file as Buffer, { hz: FIRST_CLIP.hz, seconds: CLIP_SECONDS });
  streamedUpload = calls[0].file as Buffer;
  expect(stub.calls.filter((c) => c.path.startsWith("/v1/chat"))).toHaveLength(0);
});

// --- Scenario 9 -------------------------------------------------------------

test("9 — the streamed run is billed in audio seconds from the converted WAV's header, and its transcript survives a reload", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === STREAM_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const [streamed] = (await transcriptionUsageRows()).filter((r) => r.model === STREAM_MODEL);

  // The stub's done event states no audio_duration_ms, so the quantity is what the
  // uploaded WAV's own header declares (its data bytes over its byte rate, to the
  // millisecond, as the gateway reads it) -- the clip is two seconds, so it is also what
  // the one-shot rows above are billed.
  const upload = readWav(streamedUpload);
  const headerSeconds = Math.floor((upload.dataBytes * 1000) / upload.byteRate) / 1000;
  expect(streamed).toMatchObject({
    http_status: 200,
    status: "success",
    content_type: expect.stringContaining("text/event-stream"),
    provider_model: STREAM_UPSTREAM,
    billing_unit: "audio_second",
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0
  });
  expect(streamed.billing_quantity).toBeCloseTo(headerSeconds, 3);
  expect(Math.abs((streamed.billing_quantity ?? 0) - CLIP_SECONDS)).toBeLessThan(0.1);

  // The committed turn is in the chat store, not only on the page that watched it
  // stream: after a full reload the thread shows the done event's transcript, once, and
  // its clip plays again.
  await page.reload();
  await page.getByRole("link", { name: t.chat }).click();
  await expect(lastAssistant()).toContainText(STREAM_TEXT);
  await expect(assistantTurns()).toHaveCount(1);
  await expect(userTurns()).toHaveCount(1);
  await expectStoredClip(lastUser().locator("audio"));
  // Reading it back sent nothing upstream.
  expect(stub.transcriptionCalls()).toHaveLength(4);
});
