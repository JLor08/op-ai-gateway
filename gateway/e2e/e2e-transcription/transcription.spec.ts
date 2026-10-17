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

// What the stub answers a transcription with: the transcript, and the timing
// block whose audio_duration_ms (milliseconds) is the quantity the gateway bills.
// Two seconds.
const TRANSCRIPT = "hello from the stub audio.cpp";
const CLIP_DURATION_MS = 2000;
const CLIP_SECONDS = 2;

/**
 * A small, valid RIFF/WAVE body (mono, 16 kHz, 16-bit) whose sample bytes are
 * neither valid UTF-8 nor valid JSON, so a relay that decoded, re-encoded or
 * text-handled the upload in any way would corrupt it and the byte comparison
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
 *   POST /v1/audio/transcriptions  -> a JSON {"text","timing"} document, the
 *                                     timing block carrying audio_duration_ms; a
 *                                     model it does not serve is answered 500,
 *                                     which is what the real server does for a
 *                                     client error
 */
async function startAudioCppStub(): Promise<Stub> {
  const calls: StubCall[] = [];
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
            data: [
              { id: ASR_UPSTREAM, object: "model", task: "asr" },
              { id: TTS_UPSTREAM, object: "model", task: "tts" }
            ]
          })
        );
        return;
      }
      if (method === "POST" && path === TRANSCRIPTION_PATH) {
        if (fields.model !== ASR_UPSTREAM) {
          res.writeHead(500, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: { message: `model not loaded: ${fields.model ?? ""}` } }));
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

/** The transcription verdict the gateway holds for each of the three mappings. */
type TranscriptionVerdict = { verdict: string; source: string } | null;

async function transcriptionVerdicts(): Promise<Record<string, TranscriptionVerdict>> {
  const resp = await page.request.get(`/api/portal/applications/${applicationId}/mappings`);
  expect(resp.ok(), `listing the mappings: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { gateway_model_name: string; capabilities: { capability: string; verdict: string; source: string }[] }[];
  };
  const out: Record<string, TranscriptionVerdict> = {};
  for (const mapping of body.data) {
    const found = mapping.capabilities.find((c) => c.capability === "transcription");
    out[mapping.gateway_model_name] = found ? { verdict: found.verdict, source: found.source } : null;
  }
  return out;
}

/**
 * One POST /v1/audio/transcriptions with the bearer token: a multipart form of
 * the WAV upload as `file`, the gateway `model`, and any extra text fields. Never
 * throws on a non-2xx: a refusal is an outcome.
 */
function transcribe(model: string, extra: Record<string, string> = {}): Promise<APIResponse> {
  return api.post(`${GW}${TRANSCRIPTION_PATH}`, {
    headers: { Authorization: `Bearer ${secret}` },
    multipart: { file: { name: "clip.wav", mimeType: "audio/wav", buffer: WAV }, model, ...extra },
    failOnStatusCode: false
  });
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

  // Three mappings, one per verdict state the capability gate tells apart.
  for (const [gateway, upstream] of [
    [ASR_MODEL, ASR_UPSTREAM],
    [TTS_MODEL, TTS_UPSTREAM],
    [UNPROBED_MODEL, UNPROBED_UPSTREAM]
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
      [UNPROBED_MODEL]: null
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
