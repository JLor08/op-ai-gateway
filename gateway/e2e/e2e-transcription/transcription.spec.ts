// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { expect, request as apiRequest, test, type APIRequestContext, type APIResponse, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

// POST /v1/audio/transcriptions, proven across a real process boundary. The
// gateway under test is the REAL binary (memory mode); the audio.cpp it relays to
// is a small stand-in this spec starts in-process. Everything between them is
// real: the portal's own application form creates the audio_cpp application, the
// gateway's app-health loop reads the stub's /v1/models and writes the
// transcription verdicts itself, the capability gate refuses or admits on those
// verdicts, the multipart upload crosses the relay, and the usage row it leaves is
// read back from the portal's Activity view.
//
// What the in-process Go suites (internal/gateway/transcription_handler_test.go)
// cannot reach, and this suite therefore owns:
//
//   1. THE PORTAL FORM'S audio_cpp DEFAULTS reaching a working route: type
//      audio_cpp alone is enough, the form supplies the openai_audio flavor and
//      the /health path, and the gateway routes a transcription request over them
//      -- the same application type speech uses, with no new type.
//   2. THE PROBE, not a seeded row, deciding capability: a model the stub lists
//      with task "asr" becomes (transcription, yes), one it lists with task "tts"
//      becomes (transcription, no), and one it does not list stays unknown -- and
//      both the "no" and the unknown are refused the same way.
//   3. THE WIRE CONTRACT of the relay: a multipart upload whose file part arrives
//      byte for byte, the gateway model name rewritten to the application's own
//      model id on the way out, and a gateway 400 for response_format "text" with
//      nothing sent upstream.
//   4. THE BILLING UNIT: the row's measure is audio seconds (from the timing
//      block of the stub's JSON BODY -- unlike speech, there is no duration
//      header), not tokens, and the Activity view renders the five token columns
//      of that row as the not-applicable em dash instead of a measured zero.
//   5. STREAMING (issue #262): the probe also reads each model's `mode` from the
//      stub's /v1/models and writes the transcription_streaming verdict from it
//      (streaming -> yes, offline -> no, no mode -> no row), and that verdict -- not
//      the client -- decides what a stream=true request is answered with:
//        - a yes is relayed as the upstream's event stream, UNBUFFERED (the first
//          frame reaches the client while the stub still holds the stream open),
//          and billed from the duration in the uploaded WAV's header, because the
//          streamed done event states no audio_duration_ms (clean completion only);
//        - a yes that has gone stale (the stub lists mode=streaming but answers
//          stream=true with the 500 "requires a model configured with
//          mode=streaming") is retried ONCE buffered: the client gets the one-shot
//          JSON, never the 500;
//        - a no (listed offline) and an absent verdict (no mode listed) are answered
//          buffered up front: the stream part is never sent upstream, so the upstream
//          is asked exactly once and cannot refuse. A streaming model asked WITHOUT
//          stream=true is answered one-shot as well.

const t = messages.de;

const GW = "http://127.0.0.1:8091";
const TRANSCRIPTION_PATH = "/v1/audio/transcriptions";

// Memory-mode dev principal (cmd/gateway memoryDeps): a system_admin with
// totp_mode=off who owns the seeded "Dev Admin Group".
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";

const CSRF = { "X-OP-CSRF": "1" };

const SERVER_NAME = "E2E-Transcription-Server";
const TOKEN_NAME = "e2e-transcription-token";

// The gateway model names a client sends, and the model ids audio.cpp serves
// them under. They differ on purpose: the relay's model rewrite is then
// observable at the stub, which refuses (500, as the real server does for an
// unknown model) any request that names a model it does not serve.
const ASR_MODEL = "e2e-asr-whisper";
const ASR_UPSTREAM = "whisper_x";
const TTS_MODEL = "e2e-tts-voice";
const TTS_UPSTREAM = "pocket_tts_english_bf16";
// A mapping whose model audio.cpp has not loaded: /v1/models lists only LOADED
// models, so the stub does not list it and the probe has nothing to say.
const UNPROBED_MODEL = "e2e-unprobed-model";
const UNPROBED_UPSTREAM = "not_loaded_yet";
// The streaming mappings (issue #262). What the stub's /v1/models LISTS for each (the
// probe's input, hence the verdict) and what it actually DOES with stream=true (the
// truth) are separate on purpose, so the three states the handler tells apart can each
// be reached: a model that is listed streaming and is streaming (verdict yes, relayed);
// one listed offline and offline (verdict no, answered buffered up front); and one listed
// streaming but, behind the verdict's back, no longer loaded with mode=streaming (verdict
// yes gone stale: the upstream refuses the stream and the handler retries buffered).
// The existing ASR_MODEL lists no mode at all, which is the absent-verdict case.
const STREAM_MODEL = "e2e-asr-streaming";
const STREAM_UPSTREAM = "vibevoice_asr_streaming";
const OFFLINE_MODEL = "e2e-asr-offline";
const OFFLINE_UPSTREAM = "whisper_offline";
const STALE_MODEL = "e2e-asr-stale";
const STALE_UPSTREAM = "vibevoice_swapped";

// What the stub answers a transcription with: the transcript, and the timing
// block whose audio_duration_ms (milliseconds) is the quantity the gateway bills.
// Two seconds.
const TRANSCRIPT = "hello from the stub audio.cpp";
const CLIP_DURATION_MS = 2000;
const CLIP_SECONDS = 2;

// What the stub streams for stream=true on a model that really is mode=streaming: the
// increments and the done event that restates the whole text and carries a time to first
// token -- and NO audio_duration_ms, as the real server's done event has none.
const STREAM_DELTAS = ["hello ", "from the ", "streaming stub"];
const STREAM_TEXT = "hello from the streaming stub";
const STREAM_TTFT_MS = 120;
// The WAV the streaming requests upload is three seconds long (the header says so), not
// the stub's CLIP_SECONDS, so the quantity a streamed row bills can only have come from
// the upload's header.
const STREAM_CLIP_SECONDS = 3;
// The sentence audio.cpp's refusal carries when stream=true reaches a model that is not
// loaded with mode=streaming (HTTP 500, as for every error it answers).
const STREAM_REFUSAL = "streaming transcription requires a model configured with mode=streaming";
// A held stream is released by itself after this long, so a failing assertion can never
// leave the stub (and the gateway's relay) waiting for a release that will not come.
const HOLD_SAFETY_MS = 15000;

/**
 * A valid RIFF/WAVE body (mono, 16 kHz, 16-bit) whose sample bytes are neither valid
 * UTF-8 nor valid JSON, so a relay that decoded, re-encoded or text-handled the upload
 * in any way would corrupt it and the byte comparison below would catch it. The default
 * is a handful of bytes (the buffered scenarios never bill from the header); `seconds`
 * makes a clip whose header really does declare that many seconds of audio.
 */
function wavClip(seconds?: number): Buffer {
  const pattern = [0x00, 0xff, 0xfe, 0x80, 0x7f, 0x00, 0x01, 0xc3];
  const samples =
    seconds === undefined
      ? Buffer.from(pattern)
      : Buffer.from(Array.from({ length: seconds * 32000 }, (_, i) => pattern[i % pattern.length]));
  const header = Buffer.alloc(44);
  header.write("RIFF", 0, "ascii");
  header.writeUInt32LE(36 + samples.length, 4);
  header.write("WAVE", 8, "ascii");
  header.write("fmt ", 12, "ascii");
  header.writeUInt32LE(16, 16); // fmt chunk size
  header.writeUInt16LE(1, 20); // PCM
  header.writeUInt16LE(1, 22); // mono
  header.writeUInt32LE(16000, 24); // sample rate
  header.writeUInt32LE(32000, 28); // byte rate
  header.writeUInt16LE(2, 32); // block align
  header.writeUInt16LE(16, 34); // bits per sample
  header.write("data", 36, "ascii");
  header.writeUInt32LE(samples.length, 40);
  return Buffer.concat([header, samples]);
}

const WAV = wavClip();
const STREAM_WAV = wavClip(STREAM_CLIP_SECONDS);

/** One part of a multipart/form-data body. */
type FormPart = { name: string; filename?: string; value: Buffer };

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
    const disposition = /name="([^"]*)"(?:;\s*filename="([^"]*)")?/i.exec(content.slice(0, split));
    if (!disposition) {
      continue;
    }
    parts.push({ name: disposition[1], filename: disposition[2], value: Buffer.from(content.slice(split + 4), "latin1") });
  }
  return parts;
}

type StubCall = {
  method: string;
  path: string;
  contentType: string;
  /** The text form fields of a multipart body, by name. */
  fields: Record<string, string>;
  /** The bytes of the multipart `file` part, when there is one. */
  file?: Buffer;
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

/** What the stub's /v1/models LISTS for a model, and what POST does with stream=true. */
type UpstreamModel = {
  id: string;
  task: "asr" | "tts";
  /** The `mode` the listing states, or none (an audio.cpp or agent that states no mode). */
  listedMode?: "streaming" | "offline";
  /** Whether stream=true is really honoured: false is answered with the 500 refusal. */
  streams: boolean;
};

const UPSTREAM_MODELS: UpstreamModel[] = [
  { id: ASR_UPSTREAM, task: "asr", streams: false },
  { id: TTS_UPSTREAM, task: "tts", streams: false },
  { id: STREAM_UPSTREAM, task: "asr", listedMode: "streaming", streams: true },
  { id: OFFLINE_UPSTREAM, task: "asr", listedMode: "offline", streams: false },
  { id: STALE_UPSTREAM, task: "asr", listedMode: "streaming", streams: false }
];

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
 *                                     signal) and, for the streaming mappings, the
 *                                     `mode` ("streaming" | "offline") the
 *                                     transcription_streaming verdict is read from
 *   POST /v1/audio/transcriptions  -> by default a JSON {"text","timing"} document, the
 *                                     timing block carrying audio_duration_ms; a
 *                                     model it does not serve is answered 500,
 *                                     which is what the real server does for a
 *                                     client error. With stream=true:
 *                                       - a model that really streams answers 200
 *                                         text/event-stream: a transcript.text.delta
 *                                         per increment, transcript.text.done (the
 *                                         whole text and timing.ttft_ms, no
 *                                         audio_duration_ms), then [DONE], each frame
 *                                         written as its own chunk;
 *                                       - any other model answers 500 with the error
 *                                         "... requires a model configured with
 *                                         mode=streaming", as the real server does.
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
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
      calls.push({ method, path, contentType, fields, file: parts.find((p) => p.name === "file")?.value });

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
            data: UPSTREAM_MODELS.map((m) => ({ id: m.id, object: "model", task: m.task, ...(m.listedMode ? { mode: m.listedMode } : {}) }))
          })
        );
        return;
      }
      if (method === "POST" && path === TRANSCRIPTION_PATH) {
        const model = UPSTREAM_MODELS.find((m) => m.id === fields.model && m.task === "asr");
        if (!model) {
          res.writeHead(500, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: { message: `model not loaded: ${fields.model ?? ""}` } }));
          return;
        }
        if (fields.stream === "true") {
          if (!model.streams) {
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ error: { message: STREAM_REFUSAL, type: "server_error" } }));
            return;
          }
          void streamTranscript(res);
          return;
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ text: TRANSCRIPT, timing: { audio_duration_ms: CLIP_DURATION_MS } }));
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
// A cookie-less request context: the transcription calls must authenticate with
// the bearer token exactly as an OpenAI-compatible client does, never the portal
// session.
let api: APIRequestContext;
let stub: Stub;
let serverId = "";
let applicationId = "";
let secret = "";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ browser }) => {
  stub = await startAudioCppStub();
  page = await browser.newPage();
  api = await apiRequest.newContext();
});

test.afterAll(async () => {
  await api?.dispose();
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

/** The verdict of one capability the gateway holds for each mapping (null: no row). */
type TranscriptionVerdict = { verdict: string; source: string } | null;

async function capabilityVerdicts(capability: string): Promise<Record<string, TranscriptionVerdict>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string; source: string }[] }[];
  };
  const out: Record<string, TranscriptionVerdict> = {};
  for (const mapping of body.data) {
    const found = mapping.capabilities.find((c) => c.capability === capability);
    out[mapping.gateway_model_name] = found ? { verdict: found.verdict, source: found.source } : null;
  }
  return out;
}

const transcriptionVerdicts = () => capabilityVerdicts("transcription");
const streamingVerdicts = () => capabilityVerdicts("transcription_streaming");

/**
 * One POST /v1/audio/transcriptions with the bearer token: a multipart form of
 * the WAV upload as `file`, the gateway `model`, and any extra text fields. Never
 * throws on a non-2xx: a refusal is an outcome.
 */
function transcribe(model: string, extra: Record<string, string> = {}, wav: Buffer = WAV): Promise<APIResponse> {
  return api.post(`${GW}${TRANSCRIPTION_PATH}`, {
    headers: { Authorization: `Bearer ${secret}` },
    multipart: { file: { name: "clip.wav", mimeType: "audio/wav", buffer: wav }, model, ...extra },
    failOnStatusCode: false
  });
}

/**
 * The frames of a text/event-stream body, one `data:` payload at a time, as they
 * arrive: the generator yields a frame as soon as its blank-line terminator has been
 * read, so a test can look at the first frame while the stream is still open. A
 * client that cannot see a frame before the whole body is in (APIRequestContext
 * buffers it) could not tell a relayed stream from a buffered one.
 */
async function* sseData(body: ReadableStream<Uint8Array>): AsyncGenerator<string> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    pending += decoder.decode(value, { stream: true });
    for (let end = pending.indexOf("\n\n"); end >= 0; end = pending.indexOf("\n\n")) {
      const frame = pending.slice(0, end);
      pending = pending.slice(end + 2);
      for (const line of frame.split("\n")) {
        if (line.startsWith("data:")) {
          yield line.slice("data:".length).trim();
        }
      }
    }
  }
}

/**
 * A stream=true transcription of `wav`, sent with the global fetch rather than
 * APIRequestContext so the response body can be read incrementally. Authenticates with
 * the bearer token like every other call here.
 */
function transcribeStreaming(model: string, wav: Buffer): Promise<Response> {
  const form = new FormData();
  form.append("file", new Blob([new Uint8Array(wav)], { type: "audio/wav" }), "clip.wav");
  form.append("model", model);
  form.append("stream", "true");
  return fetch(`${GW}${TRANSCRIPTION_PATH}`, { method: "POST", headers: { Authorization: `Bearer ${secret}` }, body: form });
}

/** The gateway's error code from an OpenAI-shaped `{"error":{"code":...}}` body. */
async function errorCode(resp: APIResponse): Promise<string | undefined> {
  const body = (await resp.json()) as { error?: { code?: string } };
  return body.error?.code;
}

/** The recorded usage rows of the transcription endpoint, newest first. */
type UsageRow = {
  model: string;
  provider_model: string;
  http_status: number;
  status: string;
  error_code?: string;
  content_type: string;
  api_flavor: string;
  token_name: string;
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

// --- Scenario 1 -------------------------------------------------------------

test("1 — an audio_cpp application created through the portal form is probed, and the probe, not a seed, writes the transcription verdicts", async () => {
  await login(page, DEV_EMAIL, DEV_PASSWORD);

  // The AI server: Domain 127.0.0.1, because the gateway reaches the application
  // at server.Domain:app.Port. Created through the API (it is only the
  // precondition here); the application below is what this suite is about, and
  // that goes through the real form.
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

  // The application, through the Servers view's form: ONLY the type, the port
  // and a short probe interval are entered. The flavor (openai_audio), the
  // /health path and the 600s timeout all come from the form's audio_cpp
  // defaults, which is exactly what is under test. The interval is pinned to the
  // 5s minimum so the capability probe repeats quickly once the loop has picked
  // the application up.
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
  const app = (await created.json()) as { id: string; type: string; port: number; api_flavors: string[]; health_check_path: string };
  expect(app).toMatchObject({ type: "audio_cpp", port: stub.port, api_flavors: ["openai_audio"], health_check_path: "/health" });
  applicationId = app.id;

  // The mappings: one per verdict state the capability gate tells apart (asr, tts and
  // never probed), and the three streaming ones (listed streaming, listed offline, and
  // listed streaming but no longer so upstream).
  for (const [gateway, upstream] of [
    [ASR_MODEL, ASR_UPSTREAM],
    [TTS_MODEL, TTS_UPSTREAM],
    [UNPROBED_MODEL, UNPROBED_UPSTREAM],
    [STREAM_MODEL, STREAM_UPSTREAM],
    [OFFLINE_MODEL, OFFLINE_UPSTREAM],
    [STALE_MODEL, STALE_UPSTREAM]
  ]) {
    const mapped = await page.request.post(`/api/portal/applications/${applicationId}/mappings`, {
      headers: CSRF,
      data: { gateway_model_name: gateway, app_model_name: upstream }
    });
    expect(mapped.ok(), `creating the mapping ${gateway}: ${mapped.status()} ${await mapped.text()}`).toBe(true);
  }

  // The probe writes the verdicts; nothing here seeds them. The loop reads the
  // stub's /v1/models per mapping: an "asr" task is a transcription yes, a "tts"
  // task is a transcription no, and a model the listing does not name states
  // nothing, so it gets no row at all. The wait covers the loop's own tick (up to
  // the 30s system cadence) before the 5s per-application cadence takes over.
  await expect
    .poll(transcriptionVerdicts, { timeout: 90000, intervals: [1000] })
    .toEqual({
      [ASR_MODEL]: { verdict: "yes", source: "audiocpp_models" },
      [TTS_MODEL]: { verdict: "no", source: "audiocpp_models" },
      [UNPROBED_MODEL]: null,
      [STREAM_MODEL]: { verdict: "yes", source: "audiocpp_models" },
      [OFFLINE_MODEL]: { verdict: "yes", source: "audiocpp_models" },
      [STALE_MODEL]: { verdict: "yes", source: "audiocpp_models" }
    });

  // The same pass states each asr model's `mode` as the transcription_streaming
  // verdict: streaming is a yes, offline a no, and a model whose listing states no
  // mode (ASR_MODEL), one that is no transcriber (TTS_MODEL) and one never listed
  // (UNPROBED_MODEL) get no row at all -- unknown is not a no, and unknown is answered
  // buffered. The stale model's listing still says streaming, so it is a yes.
  await expect
    .poll(streamingVerdicts, { timeout: 30000, intervals: [1000] })
    .toEqual({
      [ASR_MODEL]: null,
      [TTS_MODEL]: null,
      [UNPROBED_MODEL]: null,
      [STREAM_MODEL]: { verdict: "yes", source: "audiocpp_models" },
      [OFFLINE_MODEL]: { verdict: "no", source: "audiocpp_models" },
      [STALE_MODEL]: { verdict: "yes", source: "audiocpp_models" }
    });

  // The API token the transcription calls authenticate with.
  const minted = await page.request.post("/api/portal/tokens", {
    headers: CSRF,
    data: { name: TOKEN_NAME, scopes: ["gateway:use"] }
  });
  expect(minted.ok(), `minting a token: ${minted.status()} ${await minted.text()}`).toBe(true);
  secret = ((await minted.json()) as { secret: string }).secret;
  expect(secret.length).toBeGreaterThan(0);
});

// --- Scenario 2 -------------------------------------------------------------

test("2 — a transcription-capable model is relayed as one multipart POST with the model rewritten and the upload byte-identical", async () => {
  const resp = await transcribe(ASR_MODEL, { language: "en" });
  expect(resp.status(), `transcription response: ${await resp.text()}`).toBe(200);
  expect(resp.headers()["content-type"]).toContain("application/json");
  // The transcript and its timing block are relayed as the upstream wrote them.
  expect(await resp.json()).toEqual({ text: TRANSCRIPT, timing: { audio_duration_ms: CLIP_DURATION_MS } });

  // Exactly one upstream call, multipart, with the gateway model name rewritten to
  // the application's own model id, every other text field relayed untouched and
  // the audio upload arriving byte for byte.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(1);
  expect(calls[0].contentType).toMatch(/^multipart\/form-data;/);
  expect(calls[0].fields).toEqual({ model: ASR_UPSTREAM, language: "en" });
  expect(calls[0].file, "the upstream received a file part").toBeDefined();
  expect(Buffer.compare(calls[0].file as Buffer, WAV), "the relayed upload must equal the client's WAV byte for byte").toBe(0);
});

// --- Scenario 3 -------------------------------------------------------------

test("3 — a model that is not transcription-capable is refused 404 routing.model_not_capable, and response_format text is refused before anything is sent upstream", async () => {
  const before = stub.transcriptionCalls().length;

  // The probe's own "no" (the stub lists this model with task tts)...
  const tts = await transcribe(TTS_MODEL);
  expect(tts.status()).toBe(404);
  expect(await errorCode(tts)).toBe("routing.model_not_capable");

  // ...and a model never probed: no verdict is not a yes, the gate fails closed.
  const unprobed = await transcribe(UNPROBED_MODEL);
  expect(unprobed.status()).toBe(404);
  expect(await errorCode(unprobed)).toBe("routing.model_not_capable");

  // response_format other than json is a gateway 400: audio.cpp ignores the field
  // and always answers its own JSON document, so a "text" request would be handed
  // a shape it did not ask for without being told.
  const text = await transcribe(ASR_MODEL, { response_format: "text" });
  expect(text.status()).toBe(400);
  expect(await errorCode(text)).toBe("transcription.response_format_unsupported");

  // None of the three reached the upstream.
  expect(stub.transcriptionCalls()).toHaveLength(before);
});

// --- Scenario 4 -------------------------------------------------------------

test("4 — the usage row is metered in audio seconds, not tokens", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === ASR_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const rows = await transcriptionUsageRows();

  // The relayed request: the unit is the endpoint's identity, the quantity is the
  // upstream's own timing.audio_duration_ms in seconds, and every token column is
  // zero because tokens do not apply (the portal renders those as an em dash).
  const served = rows.filter((r) => r.model === ASR_MODEL);
  expect(served).toHaveLength(1);
  expect(served[0]).toMatchObject({
    http_status: 200,
    status: "success",
    api_flavor: "openai_audio",
    content_type: "application/json",
    provider_model: ASR_UPSTREAM,
    token_name: TOKEN_NAME,
    billing_unit: "audio_second",
    billing_quantity: CLIP_SECONDS,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0
  });

  // The two capability refusals recorded a row each, carrying the same unit with a
  // zero quantity: a refused request transcribed nothing, and the unit must not
  // read as a token-metered zero. The response_format request was refused as
  // malformed before routing, so it recorded nothing: three rows in all.
  for (const model of [TTS_MODEL, UNPROBED_MODEL]) {
    const refused = rows.filter((r) => r.model === model);
    expect(refused, `refusal rows for ${model}`).toHaveLength(1);
    expect(refused[0]).toMatchObject({
      http_status: 404,
      status: "error",
      error_code: "routing.model_not_capable",
      billing_unit: "audio_second",
      billing_quantity: 0
    });
  }
  expect(rows).toHaveLength(3);
});

// --- Scenario 5 -------------------------------------------------------------

test("5 — the Activity view renders the transcription row's five token columns as the not-applicable em dash and its billing columns as audio seconds", async () => {
  await page.getByRole("link", { name: t.usage }).click();
  // The completed-requests table, not the running-connections panel above it:
  // both carry a column menu and their own rows.
  const completed = page.getByRole("region", { name: t.usageTableTitle });
  const transcriptionRow = completed.getByRole("row").filter({ hasText: ASR_MODEL });
  await expect(transcriptionRow).toHaveCount(1);

  // Switch on the other four token columns (the total is visible by default) and
  // both billing columns through the column menu.
  await completed.getByRole("button", { name: t.listColumns }).click();
  for (const label of [
    t.activityColCached,
    t.activityColCacheWrite,
    t.activityColInput,
    t.activityColOutput,
    t.activityColBillingUnit,
    t.activityColBillingQuantity
  ]) {
    await page.getByRole("checkbox", { name: label, exact: true }).check();
  }
  await page.keyboard.press("Escape");

  // Five token cells, five em dashes, each carrying the not-token-metered
  // tooltip -- not five zeros.
  const notApplicable = transcriptionRow.getByLabel(t.activityNotTokenMetered);
  await expect(notApplicable).toHaveCount(5);
  await expect(notApplicable).toHaveText(["—", "—", "—", "—", "—"]);

  // The billing columns carry the measure instead: the unit chip shows the raw
  // wire value, and the quantity is the clip's two seconds.
  await expect(transcriptionRow.getByText("audio_second", { exact: true })).toBeVisible();
  const headers = (await transcriptionRow.locator("xpath=ancestor::table[1]").getByRole("columnheader").allTextContents()).map((h) => h.trim());
  const quantityColumn = headers.findIndex((h) => h.startsWith(t.activityColBillingQuantity));
  expect(quantityColumn, `the billing-quantity column in ${JSON.stringify(headers)}`).toBeGreaterThanOrEqual(0);
  await expect(transcriptionRow.getByRole("cell").nth(quantityColumn)).toHaveText(String(CLIP_SECONDS));
});

// --- Scenario 6 -------------------------------------------------------------

test("6 — a stream=true request to a model whose verdict is streaming yes is relayed as the upstream's event stream, frame by frame, with the stream flag and the upload forwarded", async () => {
  const before = stub.transcriptionCalls().length;
  // The stub pauses its stream after the first delta until this test lets it go.
  const [hold] = stub.holdStream(1);

  const resp = await transcribeStreaming(STREAM_MODEL, STREAM_WAV);
  expect(resp.status).toBe(200);
  expect(resp.headers.get("content-type")).toContain("text/event-stream");

  // The first frame reaches the client WHILE the upstream is still holding the rest
  // back: a relay that buffered the stream (as the one-shot path does, to read its
  // timing block) would deliver nothing before the stub let go, and the stub is still
  // paused only if the frame came through it.
  const frames = sseData(resp.body as ReadableStream<Uint8Array>);
  const first = await frames.next();
  expect(first.done, "the stream ended before its first frame").toBe(false);
  expect(JSON.parse(String(first.value))).toEqual({ type: "transcript.text.delta", delta: STREAM_DELTAS[0] });
  await hold.reached;
  expect(stub.streamHeld(), "the first frame was relayed before the upstream finished its stream").toBe(true);

  hold.release();
  const rest: string[] = [];
  for await (const frame of frames) {
    rest.push(frame);
  }
  // The rest of the stream, in the upstream's own order and framing: the remaining
  // deltas, the done event restating the whole text with its time to first token, then
  // the sentinel. Nothing is added, dropped or translated.
  expect(rest.map((frame) => (frame === "[DONE]" ? frame : (JSON.parse(frame) as { type: string }).type))).toEqual([
    "transcript.text.delta",
    "transcript.text.delta",
    "transcript.text.done",
    "[DONE]"
  ]);
  expect(rest.slice(0, 2).map((frame) => (JSON.parse(frame) as { delta: string }).delta)).toEqual(STREAM_DELTAS.slice(1));
  expect(JSON.parse(rest[2])).toEqual({ type: "transcript.text.done", text: STREAM_TEXT, timing: { ttft_ms: STREAM_TTFT_MS } });

  // One upstream call: the stream flag reached the model (and nothing else was added to
  // the form), the gateway model name was rewritten to the application's own id, and
  // the upload arrived byte for byte.
  const calls = stub.transcriptionCalls().slice(before);
  expect(calls).toHaveLength(1);
  expect(calls[0].fields).toEqual({ model: STREAM_UPSTREAM, stream: "true" });
  expect(Buffer.compare(calls[0].file as Buffer, STREAM_WAV), "the relayed upload must equal the client's WAV byte for byte").toBe(0);
});

// --- Scenario 7 -------------------------------------------------------------

test("7 — the streamed call is billed in audio seconds from the uploaded WAV's header, the streamed done event stating none", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === STREAM_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const streamed = (await transcriptionUsageRows()).filter((r) => r.model === STREAM_MODEL);
  expect(streamed).toHaveLength(1);

  // The stub's done event carries ttft_ms and no audio_duration_ms, and its one-shot
  // answer says CLIP_SECONDS: the quantity here is the three seconds the uploaded
  // header declares (the stream completed: done, then [DONE]), so it can only have
  // been read from the upload. The token columns stay zero (tokens do not apply).
  expect(streamed[0]).toMatchObject({
    http_status: 200,
    status: "success",
    api_flavor: "openai_audio",
    content_type: expect.stringContaining("text/event-stream"),
    provider_model: STREAM_UPSTREAM,
    token_name: TOKEN_NAME,
    billing_unit: "audio_second",
    billing_quantity: STREAM_CLIP_SECONDS,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0
  });
  expect(STREAM_CLIP_SECONDS).not.toBe(CLIP_SECONDS);
});

// --- Scenario 8 -------------------------------------------------------------

test("8 — a streaming verdict gone stale is retried once buffered: the upstream's 500 \"requires a model configured with mode=streaming\" never reaches the client, who gets the one-shot JSON", async () => {
  const before = stub.transcriptionCalls().length;

  // The verdict (yes, from the listing) says the model streams; the stub, which no
  // longer has it loaded with mode=streaming, refuses the stream with the 500 the real
  // server answers.
  const resp = await transcribe(STALE_MODEL, { stream: "true" }, STREAM_WAV);
  expect(resp.status(), `transcription response: ${await resp.text()}`).toBe(200);
  expect(resp.headers()["content-type"]).toContain("application/json");
  expect(await resp.json()).toEqual({ text: TRANSCRIPT, timing: { audio_duration_ms: CLIP_DURATION_MS } });

  // Two upstream calls, in this order: the stream the verdict asked for (refused), then
  // the same upload and model once more WITHOUT the stream flag.
  const calls = stub.transcriptionCalls().slice(before);
  expect(calls).toHaveLength(2);
  expect(calls[0].fields).toEqual({ model: STALE_UPSTREAM, stream: "true" });
  expect(calls[1].fields).toEqual({ model: STALE_UPSTREAM });
  for (const call of calls) {
    expect(Buffer.compare(call.file as Buffer, STREAM_WAV), "both attempts carry the client's WAV byte for byte").toBe(0);
  }

  // One usage row for the one request the client made, the answer the client got: a
  // buffered one, billed like any one-shot transcription from the response's own
  // timing block (CLIP_SECONDS), not from the three-second upload.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === STALE_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const rows = (await transcriptionUsageRows()).filter((r) => r.model === STALE_MODEL);
  expect(rows).toHaveLength(1);
  expect(rows[0]).toMatchObject({
    http_status: 200,
    status: "success",
    content_type: expect.stringContaining("application/json"),
    provider_model: STALE_UPSTREAM,
    billing_unit: "audio_second",
    billing_quantity: CLIP_SECONDS
  });
});

// --- Scenario 9 -------------------------------------------------------------

test("9 — a stream=true request to an offline model (verdict no) or one with no streaming verdict is answered buffered up front: one upstream call, no stream flag sent, never an error", async () => {
  // OFFLINE_MODEL: its listing says mode=offline (a no). ASR_MODEL: its listing states
  // no mode, so there is no verdict at all (unknown), and unknown is not a yes.
  for (const [model, upstream] of [
    [OFFLINE_MODEL, OFFLINE_UPSTREAM],
    [ASR_MODEL, ASR_UPSTREAM]
  ]) {
    const before = stub.transcriptionCalls().length;
    const resp = await transcribe(model, { stream: "true" });
    expect(resp.status(), `${model}: ${await resp.text()}`).toBe(200);
    expect(resp.headers()["content-type"]).toContain("application/json");
    expect(await resp.json()).toEqual({ text: TRANSCRIPT, timing: { audio_duration_ms: CLIP_DURATION_MS } });

    // Asked ONCE, and without the stream flag: the gateway never asked the upstream for
    // what it would refuse, so there was no 500 to retry. (The stub would have answered
    // a stream=true here with that very 500.)
    const calls = stub.transcriptionCalls().slice(before);
    expect(calls, `${model}: upstream calls`).toHaveLength(1);
    expect(calls[0].fields).toEqual({ model: upstream });
  }

  // A model that streams is also a superset of the one-shot: asked without stream=true
  // it is answered one-shot JSON, the stream flag absent upstream.
  const before = stub.transcriptionCalls().length;
  const oneShot = await transcribe(STREAM_MODEL);
  expect(oneShot.status(), `one-shot to the streaming model: ${await oneShot.text()}`).toBe(200);
  expect(oneShot.headers()["content-type"]).toContain("application/json");
  expect(await oneShot.json()).toEqual({ text: TRANSCRIPT, timing: { audio_duration_ms: CLIP_DURATION_MS } });
  const calls = stub.transcriptionCalls().slice(before);
  expect(calls).toHaveLength(1);
  expect(calls[0].fields).toEqual({ model: STREAM_UPSTREAM });

  // The offline model's row is an ordinary buffered one: success, JSON, the response's
  // own timing as the quantity.
  await expect
    .poll(async () => (await transcriptionUsageRows()).filter((r) => r.model === OFFLINE_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const offline = (await transcriptionUsageRows()).filter((r) => r.model === OFFLINE_MODEL);
  expect(offline[0]).toMatchObject({
    http_status: 200,
    status: "success",
    content_type: expect.stringContaining("application/json"),
    provider_model: OFFLINE_UPSTREAM,
    billing_unit: "audio_second",
    billing_quantity: CLIP_SECONDS
  });
});
