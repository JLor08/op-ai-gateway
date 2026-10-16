// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { expect, request as apiRequest, test, type APIRequestContext, type APIResponse, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

// POST /v1/audio/speech, proven across a real process boundary. The gateway
// under test is the REAL binary (memory mode); the audio.cpp it relays to is a
// small stand-in this spec starts in-process. Everything between them is real:
// the portal's own application form creates the audio_cpp application, the
// gateway's app-health loop reads the stub's /v1/models and writes the speech
// verdicts itself, the capability gate refuses or admits on those verdicts, the
// WAV crosses the relay, and the usage row it leaves is read back from the
// portal's Activity view.
//
// What the in-process Go suites (internal/gateway/speech_handler_test.go) cannot
// reach, and this suite therefore owns:
//
//   1. THE PORTAL FORM'S audio_cpp DEFAULTS reaching a working route: type
//      audio_cpp alone is enough, the form supplies the openai_audio flavor and
//      the /health path, and the gateway routes a speech request over them.
//   2. THE PROBE, not a seeded row, deciding capability: a model the stub lists
//      with task "tts" becomes (speech, yes), one it lists with task "asr"
//      becomes (speech, no), and one it does not list stays unknown -- and both
//      the "no" and the unknown are refused the same way.
//   3. THE WIRE CONTRACT of the relay: an audio/wav body byte for byte with the
//      upstream's own Content-Length, the gateway model name rewritten to the
//      application's own model id on the way out, and a gateway 400 for
//      response_format "mp3" with nothing sent upstream.
//   4. THE BILLING UNIT: the row's measure is audio seconds (from the stub's
//      X-AudioCPP-Audio-Duration-Ms header), not tokens, and the Activity view
//      renders the five token columns of that row as the not-applicable em dash
//      instead of a measured zero.

const t = messages.de;

const GW = "http://127.0.0.1:8091";
const SPEECH_PATH = "/v1/audio/speech";

// Memory-mode dev principal (cmd/gateway memoryDeps): a system_admin with
// totp_mode=off who owns the seeded "Dev Admin Group".
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";

const CSRF = { "X-OP-CSRF": "1" };

const SERVER_NAME = "E2E-Speech-Server";
const TOKEN_NAME = "e2e-speech-token";

// The gateway model names a client sends, and the model ids audio.cpp serves
// them under. They differ on purpose: the relay's model rewrite is then
// observable at the stub, which refuses (500, as the real server does for an
// unknown model) any request that names a model it does not serve.
const TTS_MODEL = "e2e-tts-voice";
const TTS_UPSTREAM = "pocket_tts_english_bf16";
const ASR_MODEL = "e2e-asr-model";
const ASR_UPSTREAM = "whisper_x";
// A mapping whose model audio.cpp has not loaded: /v1/models lists only LOADED
// models, so the stub does not list it and the probe has nothing to say.
const UNPROBED_MODEL = "e2e-unprobed-voice";
const UNPROBED_UPSTREAM = "not_loaded_yet";

// The metering header audio.cpp reports a clip's length in, in milliseconds as
// a decimal string, and the value the stub sends: two seconds.
const DURATION_HEADER = "X-AudioCPP-Audio-Duration-Ms";
const CLIP_DURATION_MS = "2000.000000";
const CLIP_SECONDS = 2;

/**
 * A small, valid RIFF/WAVE body (mono, 16 kHz, 16-bit) whose sample bytes are
 * neither valid UTF-8 nor valid JSON, so a relay that decoded, re-encoded or
 * text-handled the body in any way would corrupt it and the byte comparison
 * below would catch it.
 */
function wavClip(): Buffer {
  const samples = Buffer.from([0x00, 0xff, 0xfe, 0x80, 0x7f, 0x00, 0x01, 0xc3]);
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
 *   POST /v1/audio/speech   -> the WAV as audio/wav, with a Content-Length and
 *                              the duration metering header; a model it does not
 *                              serve is answered 500, which is what the real
 *                              server does for a client error
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
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
        res.writeHead(200, {
          "Content-Type": "audio/wav",
          "Content-Length": String(WAV.length),
          [DURATION_HEADER]: CLIP_DURATION_MS
        });
        res.end(WAV);
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
// A cookie-less request context: the speech calls must authenticate with the
// bearer token exactly as an OpenAI-compatible client does, never the portal
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

/** The speech verdict the gateway holds for each of the three mappings. */
type SpeechVerdict = { verdict: string; source: string } | null;

async function speechVerdicts(): Promise<Record<string, SpeechVerdict>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string; source: string }[] }[];
  };
  const out: Record<string, SpeechVerdict> = {};
  for (const mapping of body.data) {
    const found = mapping.capabilities.find((c) => c.capability === "speech");
    out[mapping.gateway_model_name] = found ? { verdict: found.verdict, source: found.source } : null;
  }
  return out;
}

/** One POST /v1/audio/speech with the bearer token. Never throws on a non-2xx: a refusal is an outcome. */
function speech(body: Record<string, unknown>): Promise<APIResponse> {
  return api.post(`${GW}${SPEECH_PATH}`, {
    headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
    data: body,
    failOnStatusCode: false
  });
}

/** The gateway's error code from an OpenAI-shaped `{"error":{"code":...}}` body. */
async function errorCode(resp: APIResponse): Promise<string | undefined> {
  const body = (await resp.json()) as { error?: { code?: string } };
  return body.error?.code;
}

/** The recorded usage rows of the speech endpoint, newest first. */
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

async function speechUsageRows(): Promise<UsageRow[]> {
  const resp = await page.request.get(`/api/portal/usage?req_path=${encodeURIComponent(SPEECH_PATH)}&limit=100`);
  expect(resp.ok(), `reading the usage rows: ${resp.status()} ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { data: UsageRow[] }).data;
}

// --- Scenario 1 -------------------------------------------------------------

test("1 — an audio_cpp application created through the portal form is probed, and the probe, not a seed, writes the speech verdicts", async () => {
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

  // Three mappings, one per verdict state the capability gate tells apart.
  for (const [gateway, upstream] of [
    [TTS_MODEL, TTS_UPSTREAM],
    [ASR_MODEL, ASR_UPSTREAM],
    [UNPROBED_MODEL, UNPROBED_UPSTREAM]
  ]) {
    const mapped = await page.request.post(`/api/portal/applications/${applicationId}/mappings`, {
      headers: CSRF,
      data: { gateway_model_name: gateway, app_model_name: upstream }
    });
    expect(mapped.ok(), `creating the mapping ${gateway}: ${mapped.status()} ${await mapped.text()}`).toBe(true);
  }

  // The probe writes the verdicts; nothing here seeds them. The loop reads the
  // stub's /v1/models per mapping: a "tts" task is a speech yes, an "asr" task is
  // a speech no, and a model the listing does not name states nothing, so it
  // gets no row at all. The wait covers the loop's own tick (up to the 30s
  // system cadence) before the 5s per-application cadence takes over.
  await expect
    .poll(speechVerdicts, { timeout: 90000, intervals: [1000] })
    .toEqual({
      [TTS_MODEL]: { verdict: "yes", source: "audiocpp_models" },
      [ASR_MODEL]: { verdict: "no", source: "audiocpp_models" },
      [UNPROBED_MODEL]: null
    });

  // The API token the speech calls authenticate with.
  const minted = await page.request.post("/api/portal/tokens", {
    headers: CSRF,
    data: { name: TOKEN_NAME, scopes: ["gateway:use"] }
  });
  expect(minted.ok(), `minting a token: ${minted.status()} ${await minted.text()}`).toBe(true);
  secret = ((await minted.json()) as { secret: string }).secret;
  expect(secret.length).toBeGreaterThan(0);
});

// --- Scenario 2 -------------------------------------------------------------

test("2 — a speech-capable model is relayed as audio/wav with the upstream's Content-Length and a byte-identical body", async () => {
  const resp = await speech({ model: TTS_MODEL, input: "Hello there.", voice: "alba" });
  expect(resp.status(), `speech response: ${(await resp.body()).toString("latin1")}`).toBe(200);
  expect(resp.headers()["content-type"]).toBe("audio/wav");
  // The relay streams the body and states the length the UPSTREAM stated, rather
  // than chunking an unknown one.
  expect(resp.headers()["content-length"]).toBe(String(WAV.length));
  const body = await resp.body();
  expect(Buffer.compare(body, WAV), "the relayed body must equal the upstream WAV byte for byte").toBe(0);

  // Exactly one upstream call, with the gateway model name rewritten to the
  // application's own model id and every other field relayed untouched.
  const calls = stub.speechCalls();
  expect(calls).toHaveLength(1);
  expect(JSON.parse(calls[0].body)).toEqual({ model: TTS_UPSTREAM, input: "Hello there.", voice: "alba" });
});

// --- Scenario 3 -------------------------------------------------------------

test("3 — a model that is not speech-capable is refused 404 routing.model_not_capable, and response_format mp3 is refused before anything is sent upstream", async () => {
  const before = stub.speechCalls().length;

  // The probe's own "no" (the stub lists this model with task asr)...
  const asr = await speech({ model: ASR_MODEL, input: "hi" });
  expect(asr.status()).toBe(404);
  expect(await errorCode(asr)).toBe("routing.model_not_capable");

  // ...and a model never probed: no verdict is not a yes, the gate fails closed.
  const unprobed = await speech({ model: UNPROBED_MODEL, input: "hi" });
  expect(unprobed.status()).toBe(404);
  expect(await errorCode(unprobed)).toBe("routing.model_not_capable");

  // response_format other than wav is a gateway 400: audio.cpp would answer an
  // mp3 request with raw WAV labelled audio/wav, handing the client the wrong
  // format without telling it.
  const mp3 = await speech({ model: TTS_MODEL, input: "hi", response_format: "mp3" });
  expect(mp3.status()).toBe(400);
  expect(await errorCode(mp3)).toBe("speech.response_format_unsupported");

  // None of the three reached the upstream.
  expect(stub.speechCalls()).toHaveLength(before);
});

// --- Scenario 4 -------------------------------------------------------------

test("4 — the usage row is metered in audio seconds, not tokens", async () => {
  // The row is recorded as the response finishes, so poll rather than assume.
  await expect
    .poll(async () => (await speechUsageRows()).filter((r) => r.model === TTS_MODEL).length, { timeout: 30000, intervals: [500] })
    .toBe(1);
  const rows = await speechUsageRows();

  // The relayed request: the unit is the endpoint's identity, the quantity is
  // the upstream's own duration header in seconds, and every token column is
  // zero because tokens do not apply (the portal renders those as an em dash).
  const served = rows.filter((r) => r.model === TTS_MODEL);
  expect(served).toHaveLength(1);
  expect(served[0]).toMatchObject({
    http_status: 200,
    status: "success",
    api_flavor: "openai_audio",
    content_type: "audio/wav",
    provider_model: TTS_UPSTREAM,
    token_name: TOKEN_NAME,
    billing_unit: "audio_second",
    billing_quantity: CLIP_SECONDS,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0
  });

  // The two refusals recorded a row each, carrying the same unit with a zero
  // quantity: a refused request produced no audio, and the unit must not read as
  // a token-metered zero. The mp3 request was refused as malformed before
  // routing, so it recorded nothing.
  for (const model of [ASR_MODEL, UNPROBED_MODEL]) {
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

test("5 — the Activity view renders the speech row's five token columns as the not-applicable em dash and its billing columns as audio seconds", async () => {
  await page.getByRole("link", { name: t.usage }).click();
  // The completed-requests table, not the running-connections panel above it:
  // both carry a column menu and their own rows.
  const completed = page.getByRole("region", { name: t.usageTableTitle });
  const speechRow = completed.getByRole("row").filter({ hasText: TTS_MODEL });
  await expect(speechRow).toHaveCount(1);

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
  const notApplicable = speechRow.getByLabel(t.activityNotTokenMetered);
  await expect(notApplicable).toHaveCount(5);
  await expect(notApplicable).toHaveText(["—", "—", "—", "—", "—"]);

  // The billing columns carry the measure instead: the unit chip shows the raw
  // wire value, and the quantity is the clip's two seconds.
  await expect(speechRow.getByText("audio_second", { exact: true })).toBeVisible();
  const headers = (await speechRow.locator("xpath=ancestor::table[1]").getByRole("columnheader").allTextContents()).map((h) => h.trim());
  const quantityColumn = headers.findIndex((h) => h.startsWith(t.activityColBillingQuantity));
  expect(quantityColumn, `the billing-quantity column in ${JSON.stringify(headers)}`).toBeGreaterThanOrEqual(0);
  await expect(speechRow.getByRole("cell").nth(quantityColumn)).toHaveText(String(CLIP_SECONDS));
});
