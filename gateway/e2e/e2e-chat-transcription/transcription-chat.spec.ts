// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import * as fs from "node:fs";
import * as http from "node:http";
import type { AddressInfo } from "node:net";
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
// (scenarios 3 and 5, WAV clips this spec makes) and RECORDED in the browser
// (scenario 6). A microphone is the one thing no suite can rely on -- headless
// Chromium has no capture device, and its fake one does not even answer getUserMedia
// on every host -- so the spec replaces only the capture SOURCE (see
// installSyntheticMicrophone): the portal's recorder, the browser's REAL MediaRecorder
// and its real audio/webm (Opus) encoding are all untouched. The recorder's state
// machine and error paths are unit-tested (AudioRecorder.test.tsx); what is
// cross-process -- the clip's bytes on the wire and in the store -- is what the two
// ways are compared on here.
//
// What the in-process suites cannot reach (the Go run-executor tests stub the HTTP
// hop, the Vitest suites stub fetch and the audio element), and this suite owns:
//
//   1. OFFERING: the model the probe marks transcription-capable is in the chat's
//      picker and a model the probe has no verdict for (an unprobed one) is not, and
//      picking the former switches the composer to transcription mode -- an audio
//      input in place of the text prompt, Send closed until a clip is attached, the
//      language / prompt / temperature settings shown, the text-chat controls gone.
//   2. THE WIRE: one send makes exactly one upstream multipart POST to
//      /v1/audio/transcriptions, with the gateway model name rewritten to the
//      application's own model id, the thread's language, prompt and temperature
//      carried as form fields, and the clip arriving as the `file` part BYTE FOR BYTE
//      (named by its media type); the stub never sees a chat-completions call.
//   3. THE ANSWER: a pending state while the upstream is still working, then the
//      transcript as a plain text turn, next to the user's own clip, which plays from
//      the chat blob store (byte for byte the attached WAV, as audio/wav, which the
//      browser really decodes) and downloads as the same bytes; the untitled chat is
//      named after its transcript.
//   4. PERSISTENCE: after a full reload the thread is still a transcription thread,
//      its settings are restored, the clip plays again from the store and the
//      transcript is still there -- and reading it back sent nothing upstream.
//   5. THE FOLLOW-UP: a second clip is a second upstream call with the NEW clip's
//      bytes (an earlier turn's clip is never re-sent), and the advanced JSON
//      passthrough reaches the form next to the thread's own settings, a passthrough
//      key that repeats a set setting not being sent twice.
//   6. THE RECORDING: a clip recorded in the browser is relayed like an attached one --
//      the recorder's audio/webm reaches the upstream as the file part (named
//      audio.webm, byte for byte what the chat blob store holds) and is answered with
//      its own transcript.
//   7. THE BILLING UNIT: each run leaves an audio_second usage row (from the stub's
//      timing.audio_duration_ms, 2000 ms = 2 s), not a token-metered one.
//
// Not covered here, and owned by the unit/integration suites instead: the reserved
// passthrough fields and the invalid-JSON refusal, the run's error codes and their
// labels (including "no transcript" for silence), the format and size refusals of the
// attach, the recorder's permission-denied / unsupported / failed paths, and the
// object-URL revoke on unmount -- none of those is a cross-process property.

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

// What the stub answers the nth transcription with: the transcript, and the timing
// block whose audio_duration_ms (milliseconds) is the quantity the gateway bills. Two
// seconds, and the WAV clips below really are this long, so the number the stub
// reports, the bytes and the duration the browser decodes all agree. Every transcript
// is shorter than the 40 characters the chat title is cut at.
const CLIP_SECONDS = 2;
const CLIP_DURATION_MS = CLIP_SECONDS * 1000;
const TRANSCRIPTS = ["Guten Tag, das ist das Transkript.", "Und hier noch ein zweiter Satz.", "Dies stammt aus einer Aufnahme."];
// How long the stub holds the FIRST answer, so the pending state is on screen for long
// enough to be observed without a race.
const FIRST_ANSWER_DELAY_MS = 1500;

const LANGUAGE = "en";
const PROMPT = "Names: Alba, Jonas.";
const TEMPERATURE = 0.2;
// The follow-up's advanced passthrough: a native knob and a repeat of the language the
// thread already carries (a first-class setting that is set wins, so it must not be
// sent a second time -- the real endpoint refuses a repeated field).
const PASSTHROUGH = { word_timestamps: true, seed: 7, language: "fr" };

/**
 * A valid RIFF/WAVE body (mono, 16 kHz, 16-bit PCM): CLIP_SECONDS of a tone at `hz`.
 * The sample bytes are neither valid UTF-8 nor valid JSON, so a hop that decoded,
 * re-encoded or text-handled the clip in any way would corrupt it and the byte
 * comparisons below would catch it; it is long enough for a media element to decode
 * and report a duration for; and a different pitch makes a different clip, so which
 * of two clips reached the upstream is observable.
 */
function wavClip(hz: number): Buffer {
  const sampleRate = 16000;
  const count = sampleRate * CLIP_SECONDS;
  const samples = Buffer.alloc(count * 2);
  for (let i = 0; i < count; i += 1) {
    samples.writeInt16LE(Math.round(Math.sin((2 * Math.PI * hz * i) / sampleRate) * 12000), i * 2);
  }
  const header = Buffer.alloc(44);
  header.write("RIFF", 0, "ascii");
  header.writeUInt32LE(36 + samples.length, 4);
  header.write("WAVE", 8, "ascii");
  header.write("fmt ", 12, "ascii");
  header.writeUInt32LE(16, 16); // fmt chunk size
  header.writeUInt16LE(1, 20); // PCM
  header.writeUInt16LE(1, 22); // mono
  header.writeUInt32LE(sampleRate, 24);
  header.writeUInt32LE(sampleRate * 2, 28); // byte rate
  header.writeUInt16LE(2, 32); // block align
  header.writeUInt16LE(16, 34); // bits per sample
  header.write("data", 36, "ascii");
  header.writeUInt32LE(samples.length, 40);
  return Buffer.concat([header, samples]);
}

const FIRST_WAV = wavClip(440);
const SECOND_WAV = wavClip(880);

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
  close(): Promise<void>;
};

/**
 * The audio.cpp stand-in, kept to what the gateway actually talks to and honest
 * about the shapes and statuses the real server uses:
 *
 *   GET  /health                   -> 200 (the application's health path)
 *   GET  /v1/models                -> the OpenAI list of LOADED models, each with
 *                                     the `task` audio.cpp states (the capability
 *                                     signal)
 *   POST /v1/audio/transcriptions  -> a multipart request answered with a JSON
 *                                     {"text","timing"} document, the timing block
 *                                     carrying audio_duration_ms; the nth answer
 *                                     carries the nth transcript and the first is
 *                                     held for FIRST_ANSWER_DELAY_MS; a model it does
 *                                     not serve is answered 500, which is what the
 *                                     real server does for a client error
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
  let answered = 0;
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
        res.end(JSON.stringify({ object: "list", data: [{ id: ASR_UPSTREAM, object: "model", task: "asr" }] }));
        return;
      }
      if (method === "POST" && path === TRANSCRIPTION_PATH) {
        if (fields.model !== ASR_UPSTREAM) {
          res.writeHead(500, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: { message: `model not loaded: ${fields.model ?? ""}` } }));
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

test.beforeAll(async ({ browser }) => {
  stub = await startAudioCppStub();
  page = await browser.newPage();
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

/** The transcription verdict the gateway holds for each mapping. */
async function transcriptionVerdicts(): Promise<Record<string, string | null>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string }[] }[];
  };
  const out: Record<string, string | null> = {};
  for (const mapping of body.data) {
    out[mapping.gateway_model_name] = mapping.capabilities.find((c) => c.capability === "transcription")?.verdict ?? null;
  }
  return out;
}

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
 * wraps), and returns once the composer shows it: a player for the waiting clip.
 */
async function attachClip(clip: Buffer, name: string): Promise<void> {
  await composer().getByLabel(t.chatAttachAudio, { exact: true }).setInputFiles({ name, mimeType: "audio/wav", buffer: clip });
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
    // A recording's WebM has no duration header (the browser reports Infinity until the
    // file is played to its end), so its length is not asserted: the clip is decoded
    // when the metadata loaded without a media error.
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
  // (a transcription yes) and one it does not list (no verdict at all).
  for (const [gateway, upstream] of [
    [ASR_MODEL, ASR_UPSTREAM],
    [UNPROBED_MODEL, UNPROBED_UPSTREAM]
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
    .toEqual({ [ASR_MODEL]: "yes", [UNPROBED_MODEL]: null });

  // The portal's model listing (what the chat's picker is built from) carries the
  // verdict as its `transcription` flag -- and an asr model is not a speech one.
  await expect
    .poll(
      async () => {
        const resp = await page.request.get("/api/portal/models");
        const data = ((await resp.json()) as { data: { id: string; transcription?: boolean; speech?: boolean }[] }).data;
        return Object.fromEntries(
          data
            .filter((m) => [ASR_MODEL, UNPROBED_MODEL].includes(m.id))
            .map((m) => [m.id, { transcription: m.transcription === true, speech: m.speech === true }])
        );
      },
      { timeout: 30000, intervals: [500] }
    )
    .toMatchObject({ [ASR_MODEL]: { transcription: true, speech: false } });
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

test("3 — an attached clip is relayed as one multipart /v1/audio/transcriptions request and renders as a transcript turn next to its playable, downloadable clip", async () => {
  await openSettings();
  await page.locator("#chat-transcription-language").fill(LANGUAGE);
  await page.locator("#chat-transcription-prompt").fill(PROMPT);
  await page.locator("#chat-transcription-temperature").fill(String(TEMPERATURE));

  await attachClip(FIRST_WAV, "first-clip.wav");
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

  // The user's turn is the clip: a player that plays the stored bytes -- byte for byte
  // the attached WAV, served as audio/wav out of the owner-scoped blob endpoint.
  await expect(userTurns()).toHaveCount(1);
  await expectStoredClip(lastUser().locator("audio"));
  const blob = await blobResponse;
  expect(blob.status()).toBe(200);
  expect(blob.headers()["content-type"]).toMatch(/^audio\/wav/);
  expect(Buffer.compare(await blob.body(), FIRST_WAV), "the stored blob must equal the attached WAV byte for byte").toBe(0);

  // The download saves the same bytes, named as the user's own input, not as audio the
  // gateway generated.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    lastUser().getByRole("button", { name: t.chatDownloadAudio, exact: true }).click()
  ]);
  expect(download.suggestedFilename()).toMatch(/^input-audio-[A-Za-z0-9._-]+\.wav$/);
  expect(Buffer.compare(fs.readFileSync(await download.path()), FIRST_WAV), "the downloaded file must equal the attached WAV byte for byte").toBe(0);

  // The untitled chat is named after its transcript.
  await expect(page.getByLabel(t.chatSidebarLabel)).toContainText(TRANSCRIPTS[0]);

  // Exactly one upstream transcription call: multipart, the gateway model name
  // rewritten to the application's own model id, the thread's settings as form fields
  // (temperature in its plain decimal form) and nothing else, and the clip arriving as
  // the file part byte for byte under the name of its media type.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(1);
  expect(calls[0].contentType).toMatch(/^multipart\/form-data;/);
  expect(calls[0].fields).toEqual({ model: ASR_UPSTREAM, language: LANGUAGE, prompt: PROMPT, temperature: String(TEMPERATURE) });
  expect(calls[0].filename).toBe("audio.wav");
  expect(calls[0].file, "the upstream received a file part").toBeDefined();
  expect(Buffer.compare(calls[0].file as Buffer, FIRST_WAV), "the relayed upload must equal the attached WAV byte for byte").toBe(0);
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

test("5 — a follow-up clip is a second upstream call with the new clip's bytes, and the advanced passthrough reaches the form next to the thread's own settings", async () => {
  await openSettings();
  await page.getByRole("button", { name: t.chatTranscriptionAdvanced, exact: true }).click();
  await page.locator("#chat-transcription-passthrough").fill(JSON.stringify(PASSTHROUGH));

  // A transcription turn IS its clip: with the first one sent, Send is closed again
  // until another is attached.
  await expect(sendButton()).toBeDisabled();
  await attachClip(SECOND_WAV, "second-clip.wav");
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
  // re-sent), the thread's own settings, and the passthrough fields relayed as form
  // text (each value as its literal) -- but not the passthrough "language", which
  // repeats a set setting: the thread's own value wins and the field appears once.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(2);
  expect(calls[1].filename).toBe("audio.wav");
  expect(Buffer.compare(calls[1].file as Buffer, SECOND_WAV), "the second upload must be the second clip, byte for byte").toBe(0);
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

// --- Scenario 6 -------------------------------------------------------------

test("6 — a clip recorded in the browser is relayed like an attached one: the recorder's WebM reaches the upstream and the chat store byte for byte", async () => {
  // The REAL recorder, on the synthetic microphone: start, let the clock reach one
  // second so the take holds audio, stop, and use the take.
  await composer().getByRole("button", { name: t.chatRecordStart, exact: true }).click();
  await expect(page.getByRole("timer", { name: t.chatRecordElapsed })).toHaveText("0:01", { timeout: 15000 });
  await composer().getByRole("button", { name: t.chatRecordStop, exact: true }).click();
  await expect(composer().getByLabel(t.chatRecordPreview)).toBeVisible();
  await composer().getByRole("button", { name: t.chatRecordUse, exact: true }).click();

  // The take is the composer's clip, shown under the name a recording gets (it has no
  // file name), and Send is open.
  await expect(composer().getByText(t.chatAudioRecordingLabel, { exact: true })).toBeVisible();
  await expect(composer().locator("audio")).toHaveAttribute("src", /^data:audio\/webm;base64,/);
  await expect(sendButton()).toBeEnabled();

  // The stored copy of the recording is the third blob the chat serves, as audio/webm.
  const blobResponse = page.waitForResponse(
    (r) => new URL(r.url()).pathname.startsWith("/api/portal/chat-blobs/") && (r.headers()["content-type"] ?? "").startsWith("audio/webm")
  );
  await sendButton().click();

  await expect(assistantTurns()).toHaveCount(3);
  await expect(lastAssistant()).toContainText(TRANSCRIPTS[2]);
  await expect(userTurns()).toHaveCount(3);
  await expect(lastAssistant().getByRole("alert")).toHaveCount(0);
  await expect(composer().locator("audio")).toHaveCount(0);
  await expectStoredClip(lastUser().locator("audio"), null);

  // The third upstream call: the recorder's own bytes -- an EBML/WebM stream -- as the
  // file part under the name and media type of the canonical audio/webm (the recorder
  // reports audio/webm;codecs=opus; the codec parameter never reaches the wire), and
  // the thread's settings unchanged.
  const calls = stub.transcriptionCalls();
  expect(calls).toHaveLength(3);
  expect(calls[2].filename).toBe("audio.webm");
  expect(calls[2].fileType).toBe("audio/webm");
  const upload = calls[2].file as Buffer;
  expect(upload.length, "the recording holds audio").toBeGreaterThan(1000);
  expect(upload.subarray(0, 4).toString("hex"), "the upload is a WebM (EBML) stream").toBe("1a45dfa3");
  expect(calls[2].fields).toMatchObject({ model: ASR_UPSTREAM, language: LANGUAGE, prompt: PROMPT, temperature: String(TEMPERATURE) });

  // What the chat store holds of the clip is what the upstream received.
  const blob = await blobResponse;
  expect(blob.status()).toBe(200);
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
