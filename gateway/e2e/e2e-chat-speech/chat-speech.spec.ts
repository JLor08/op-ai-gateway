// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import * as fs from "node:fs";
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

// A text-to-speech turn in the PORTAL CHAT, proven across a real process boundary.
// The gateway under test is the REAL binary (memory mode) and the UI is the REAL
// built portal; the audio.cpp the chat's run executor relays to is a small stand-in
// this spec starts in-process. Everything between them is real: the portal's own
// application form creates the audio_cpp application, the gateway's app-health loop
// reads the stub's /v1/models and writes the speech verdict itself, that verdict is
// what makes the chat offer the model, the chat run executor loopback-POSTs
// /v1/audio/speech, the WAV is moved into the chat blob store, and the turn the
// portal renders is read back out of that store.
//
// What the in-process suites cannot reach (the Go run-executor tests stub the HTTP
// hop, the Vitest suites stub fetch and the audio element), and this suite owns:
//
//   1. OFFERING: the model the probe marks speech-capable is in the chat's picker, a
//      model the probe marks as not speech-capable (an asr one) is not, and picking
//      the former switches the composer to speech mode -- the text input relabelled,
//      the voice and speed settings shown, the text-chat controls gone.
//   2. THE WIRE: one send makes exactly one upstream POST to /v1/audio/speech, with
//      the gateway model name rewritten to the application's own model id and the
//      chosen voice and speed carried; the stub never sees a chat-completions call.
//   3. THE ANSWER: an <audio> player whose bytes came out of /api/portal/chat-blobs/
//      (byte for byte the upstream WAV, as audio/wav) and which the browser really
//      decodes (its duration is the clip's), and a download that saves the same bytes
//      under a .wav name; a pending state while the upstream is still working.
//   4. PERSISTENCE: after a full reload the thread is still a speech thread, its
//      settings are restored and the audio turn plays again from the store.
//   5. THE FOLLOW-UP: the advanced JSON passthrough reaches the upstream body next to
//      the thread's own voice and speed.
//   6. THE BILLING UNIT: each run leaves an audio_second usage row (the stub's
//      X-AudioCPP-Audio-Duration-Ms header), not a token-metered one.
//
// Not covered here, and owned by the unit/integration suites instead: the reserved
// passthrough fields and the invalid-JSON refusal (buildSpeechBody and Chat.test.tsx),
// the run's error codes and their labels, and the object-URL revoke on unmount
// (AudioTurn.test.tsx) -- none of those is a cross-process property.

const t = messages.de;

// Memory-mode dev principal (cmd/gateway memoryDeps): a system_admin with
// totp_mode=off who owns the seeded "Dev Admin Group".
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";

const CSRF = { "X-OP-CSRF": "1" };

const SPEECH_PATH = "/v1/audio/speech";
const SERVER_NAME = "E2E-ChatSpeech-Server";

// The gateway model names the chat offers, and the model ids audio.cpp serves them
// under. They differ on purpose: the relay's model rewrite is then observable at the
// stub, which refuses (500, as the real server does for an unknown model) any
// request that names a model it does not serve.
const TTS_MODEL = "e2e-chat-voice";
const TTS_UPSTREAM = "pocket_tts_english_bf16";
const ASR_MODEL = "e2e-chat-asr";
const ASR_UPSTREAM = "whisper_x";

// The metering header audio.cpp reports a clip's length in, in milliseconds as a
// decimal string. The clip below really is this long, so the header, the bytes and
// the duration the browser decodes all agree.
const DURATION_HEADER = "X-AudioCPP-Audio-Duration-Ms";
const CLIP_SECONDS = 2;
const CLIP_DURATION_MS = `${CLIP_SECONDS * 1000}.000000`;
// How long the stub holds the FIRST speech answer, so the pending state is on screen
// for long enough to be observed without a race.
const FIRST_ANSWER_DELAY_MS = 1500;

const FIRST_TEXT = "Guten Tag, dies ist ein Test der Sprachausgabe.";
const SECOND_TEXT = "Und jetzt noch ein zweiter Satz.";
const VOICE = "alba";
const SPEED = 1.25;
const INSTRUCTIONS = "Speak calmly.";

/**
 * A valid RIFF/WAVE body (mono, 16 kHz, 16-bit PCM): CLIP_SECONDS of a 440 Hz tone.
 * The sample bytes are neither valid UTF-8 nor valid JSON, so a hop that decoded,
 * re-encoded or text-handled the body in any way would corrupt it and the byte
 * comparisons below would catch it; and it is long enough for a media element to
 * decode and report a duration for.
 */
function wavClip(): Buffer {
  const sampleRate = 16000;
  const count = sampleRate * CLIP_SECONDS;
  const samples = Buffer.alloc(count * 2);
  for (let i = 0; i < count; i += 1) {
    samples.writeInt16LE(Math.round(Math.sin((2 * Math.PI * 440 * i) / sampleRate) * 12000), i * 2);
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

const WAV = wavClip();

type StubCall = { method: string; path: string; body: string };

type Stub = {
  port: number;
  /** Every request the stub has received, in order. */
  calls: StubCall[];
  /** The POST /v1/audio/speech requests among them. */
  speechCalls(): StubCall[];
  close(): Promise<void>;
};

/**
 * The audio.cpp stand-in, kept to what the gateway actually talks to and honest
 * about the headers and statuses the real server uses:
 *
 *   GET  /health            -> 200 (the application's health path)
 *   GET  /v1/models         -> the OpenAI list of LOADED models, each with the
 *                              `task` audio.cpp states (the capability signal)
 *   POST /v1/audio/speech   -> the WAV as audio/wav, with a Content-Length and the
 *                              duration metering header; the first one is held for
 *                              FIRST_ANSWER_DELAY_MS; a model it does not serve is
 *                              answered 500, which is what the real server does for
 *                              a client error
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
  let speechAnswered = 0;
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const method = req.method ?? "";
      const path = req.url ?? "";
      const body = Buffer.concat(chunks).toString("utf8");
      calls.push({ method, path, body });

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
              { id: TTS_UPSTREAM, object: "model", task: "tts" },
              { id: ASR_UPSTREAM, object: "model", task: "asr" }
            ]
          })
        );
        return;
      }
      if (method === "POST" && path === SPEECH_PATH) {
        let model = "";
        try {
          model = String((JSON.parse(body) as { model?: unknown }).model ?? "");
        } catch {
          // Left empty: an unreadable body names no model the stub serves.
        }
        if (model !== TTS_UPSTREAM) {
          res.writeHead(500, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: { message: `model not loaded: ${model}` } }));
          return;
        }
        const delay = speechAnswered === 0 ? FIRST_ANSWER_DELAY_MS : 0;
        speechAnswered += 1;
        setTimeout(() => {
          res.writeHead(200, {
            "Content-Type": "audio/wav",
            "Content-Length": String(WAV.length),
            [DURATION_HEADER]: CLIP_DURATION_MS
          });
          res.end(WAV);
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
    speechCalls: () => calls.filter((c) => c.method === "POST" && c.path === SPEECH_PATH),
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

test.beforeAll(async ({ browser }) => {
  stub = await startAudioCppStub();
  page = await browser.newPage();
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

// The assistant turn of the CURRENT chat: .last() so an earlier bubble never trips
// strict-mode matching.
const assistantTurns = () => page.locator('[data-role="assistant"]');
const lastAssistant = () => assistantTurns().last();

/** The speech verdict the gateway holds for each mapping. */
async function speechVerdicts(): Promise<Record<string, string | null>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string }[] }[];
  };
  const out: Record<string, string | null> = {};
  for (const mapping of body.data) {
    out[mapping.gateway_model_name] = mapping.capabilities.find((c) => c.capability === "speech")?.verdict ?? null;
  }
  return out;
}

/** The recorded usage rows of the speech endpoint. */
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

async function speechUsageRows(): Promise<UsageRow[]> {
  const resp = await page.request.get(`/api/portal/usage?req_path=${encodeURIComponent(SPEECH_PATH)}&limit=100`);
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
  if (!(await page.locator("#chat-speech-voice").isVisible())) {
    await page.getByRole("button", { name: t.chatSettings, exact: true }).click();
  }
  await expect(page.locator("#chat-speech-voice")).toBeVisible();
}

/** Sends the prompt and returns once the speech request has been accepted. */
async function send(text: string): Promise<void> {
  await page.locator("#chat-message").fill(text);
  await page.getByRole("button", { name: t.send, exact: true }).click();
}

/**
 * The audio player of the newest assistant turn, once it has its bytes (an object
 * URL the portal made from the stored reply) and the browser has decoded them: its
 * duration is the clip's.
 */
async function expectPlayableClip(player: Locator): Promise<void> {
  await expect(player).toBeVisible();
  await expect(player).toHaveAttribute("src", /^blob:/);
  await expect
    .poll(
      () =>
        player.evaluate((el) => {
          const audio = el as HTMLAudioElement;
          return { error: audio.error?.code ?? null, ready: audio.readyState >= 1, seconds: audio.duration };
        }),
      { timeout: 15000, intervals: [250] }
    )
    .toMatchObject({ error: null, ready: true, seconds: expect.closeTo(CLIP_SECONDS, 1) });
}

// --- Scenario 1 -------------------------------------------------------------

test("1 — an audio_cpp application created through the portal form is probed, and the chat is offered the speech model that the probe, not a seed, marked", async () => {
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

  // One mapping per side of the offering rule: a speech-capable model and one the
  // probe will mark as NOT speech-capable.
  for (const [gateway, upstream] of [
    [TTS_MODEL, TTS_UPSTREAM],
    [ASR_MODEL, ASR_UPSTREAM]
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
  await expect.poll(speechVerdicts, { timeout: 90000, intervals: [1000] }).toEqual({ [TTS_MODEL]: "yes", [ASR_MODEL]: "no" });

  // The portal's model listing (what the chat's picker is built from) carries the
  // verdict as its `speech` flag.
  await expect
    .poll(
      async () => {
        const resp = await page.request.get("/api/portal/models");
        const data = ((await resp.json()) as { data: { id: string; speech?: boolean }[] }).data;
        return Object.fromEntries(data.filter((m) => [TTS_MODEL, ASR_MODEL].includes(m.id)).map((m) => [m.id, m.speech === true]));
      },
      { timeout: 30000, intervals: [500] }
    )
    .toMatchObject({ [TTS_MODEL]: true });
});

// --- Scenario 2 -------------------------------------------------------------

test("2 — the chat offers the speech model and not the asr one, and picking it switches the composer to speech mode", async () => {
  await openNewChat();

  const picker = page.getByRole("combobox", { name: t.chatModel });
  await picker.click();
  await expect(page.getByRole("option", { name: TTS_MODEL })).toBeVisible();
  // An asr model is an openai_audio model too, but without the speech verdict the
  // chat has nothing it could send it, so it is not offered at all.
  await expect(page.getByRole("option", { name: ASR_MODEL })).toHaveCount(0);
  await page.getByRole("option", { name: TTS_MODEL }).click();

  // The composer: the text input is relabelled for speaking, and the settings are
  // the speech knobs, not the text chat's.
  await expect(page.getByLabel(t.chatSpeechPromptLabel)).toBeVisible();
  await page.getByRole("button", { name: t.chatSettings, exact: true }).click();
  await expect(page.locator("#chat-speech-voice")).toBeVisible();
  await expect(page.locator("#chat-speech-speed")).toBeVisible();
  await expect(page.locator("#chat-system-prompt")).toHaveCount(0);
  await expect(page.locator("#chat-max-tokens")).toHaveCount(0);
  // The output is always WAV: no format control, just the statement of it.
  await expect(page.getByText(t.chatSpeechFormatHint)).toBeVisible();
});

// --- Scenario 3 -------------------------------------------------------------

test("3 — sending text relays one /v1/audio/speech request and renders a playable, downloadable audio turn", async () => {
  await openSettings();
  await page.locator("#chat-speech-voice").fill(VOICE);
  await page.locator("#chat-speech-speed").fill(String(SPEED));

  // The audio turn's bytes come from the chat blob store, fetched same-origin.
  const blobResponse = page.waitForResponse((r) => new URL(r.url()).pathname.startsWith("/api/portal/chat-blobs/"));
  await send(FIRST_TEXT);

  // The upstream is held for a moment (see FIRST_ANSWER_DELAY_MS): the chat is in
  // its non-streaming wait state, not showing a blank or a text bubble.
  await expect(page.getByText(t.chatAudioRunPending).first()).toBeVisible();

  const player = lastAssistant().locator("audio");
  await expectPlayableClip(player);

  // The bytes the player was given are the upstream's WAV, byte for byte, served
  // as audio/wav out of the owner-scoped blob endpoint.
  const blob = await blobResponse;
  expect(blob.status()).toBe(200);
  expect(blob.headers()["content-type"]).toMatch(/^audio\/wav/);
  expect(Buffer.compare(await blob.body(), WAV), "the stored blob must equal the upstream WAV byte for byte").toBe(0);

  // The download saves the same bytes under a .wav name.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    lastAssistant().getByRole("button", { name: t.chatDownloadAudio, exact: true }).click()
  ]);
  expect(download.suggestedFilename()).toMatch(/^generated-audio-[A-Za-z0-9._-]+\.wav$/);
  const savedPath = await download.path();
  expect(Buffer.compare(fs.readFileSync(savedPath), WAV), "the downloaded file must equal the upstream WAV byte for byte").toBe(0);
  await expect(lastAssistant().getByRole("alert")).toHaveCount(0);

  // Exactly one upstream speech call, with the gateway model name rewritten to the
  // application's own model id and the thread's voice and speed (speed as a number)
  // carried; nothing else in the body.
  const calls = stub.speechCalls();
  expect(calls).toHaveLength(1);
  expect(JSON.parse(calls[0].body)).toEqual({ model: TTS_UPSTREAM, input: FIRST_TEXT, voice: VOICE, speed: SPEED });
  // The chat went to the speech endpoint and nowhere else: no chat-completions call,
  // and no POST of any other kind, reached the stub.
  expect(stub.calls.filter((c) => c.path.startsWith("/v1/chat"))).toHaveLength(0);
  expect(stub.calls.filter((c) => c.method === "POST").map((c) => c.path)).toEqual([SPEECH_PATH]);
});

// --- Scenario 4 -------------------------------------------------------------

test("4 — the speech thread survives a full reload: still a speech thread, settings restored, the audio turn plays again from the store", async () => {
  await page.reload();
  await page.getByRole("link", { name: t.chat }).click();

  // The thread was pinned to the speech kind by its first send, so its composer is
  // the speech one again without anything being picked.
  await expect(page.getByLabel(t.chatSpeechPromptLabel)).toBeVisible();
  await expectPlayableClip(lastAssistant().locator("audio"));
  await expect(assistantTurns()).toHaveCount(1);

  // The knobs the first run was sent with are the thread's own settings again.
  await openSettings();
  await expect(page.locator("#chat-speech-voice")).toHaveValue(VOICE);
  await expect(page.locator("#chat-speech-speed")).toHaveValue(String(SPEED));
  // Reading the transcript back sent nothing upstream.
  expect(stub.speechCalls()).toHaveLength(1);
});

// --- Scenario 5 -------------------------------------------------------------

test("5 — a follow-up turn carries the advanced passthrough next to the thread's own voice and speed", async () => {
  await openSettings();
  await page.getByRole("button", { name: t.chatSpeechAdvanced, exact: true }).click();
  await page.locator("#chat-speech-passthrough").fill(JSON.stringify({ instructions: INSTRUCTIONS }));
  await send(SECOND_TEXT);

  await expect(assistantTurns()).toHaveCount(2);
  await expectPlayableClip(lastAssistant().locator("audio"));
  await expect(page.locator('[data-role="assistant"] audio')).toHaveCount(2);

  // The second upstream call: the new text only (the run speaks the last user
  // message, not the transcript), the thread's voice and speed, and the passthrough
  // field relayed untouched to audio.cpp.
  const calls = stub.speechCalls();
  expect(calls).toHaveLength(2);
  expect(JSON.parse(calls[1].body)).toEqual({
    model: TTS_UPSTREAM,
    input: SECOND_TEXT,
    voice: VOICE,
    speed: SPEED,
    instructions: INSTRUCTIONS
  });
  expect(stub.calls.filter((c) => c.path.startsWith("/v1/chat"))).toHaveLength(0);
});

// --- Scenario 6 -------------------------------------------------------------

test("6 — each speech run left an audio_second usage row, not a token-metered one", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await speechUsageRows()).filter((r) => r.model === TTS_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(2);
  const served = (await speechUsageRows()).filter((r) => r.model === TTS_MODEL);
  for (const usage of served) {
    expect(usage).toMatchObject({
      http_status: 200,
      status: "success",
      provider_model: TTS_UPSTREAM,
      billing_unit: "audio_second",
      billing_quantity: CLIP_SECONDS,
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0
    });
  }
});
