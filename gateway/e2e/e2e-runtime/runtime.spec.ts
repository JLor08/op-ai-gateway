// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { expect, request as apiRequest, test, type APIRequestContext, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";
import { RUNTIME_AGENT_BIN, RUNTIME_ROUTER_PORT, RUNTIME_STUB_BIN } from "../playwright.runtime.config";

// The agent-managed model runtime, proven across REAL process boundaries.
// Everything else on this feature is in-process; this suite is the only place
// where the gateway hands a launch spec to a real server-agent binary, that
// agent exec()s a real child process, an inference routes through to it, and
// the resulting state comes back up into the portal.
//
// What no in-process test can reach, and this suite therefore owns:
//
//   1. COLD START ON DEMAND. Nothing is running when the first inference
//      arrives; the request itself is what starts the process, waits for its
//      health endpoint, and gets the answer back. The stub deliberately holds
//      /health at 503 for two seconds (-ready-after), so `starting` is a real,
//      observed state on the portal's SSE stream rather than something raced
//      past.
//   2. THE ADMISSION ARITHMETIC with real processes -- which model may run,
//      and what happens when the per-GPU VRAM budget fits only one of two.
//      Scenario 5 shrinks nothing and changes no matrix: with a 1000 MB budget
//      and two 700 MB specs, an inference for B evicts the IDLE A. Scenario 6
//      then raises the budget to 2000 MB, changing NOTHING else, and the same
//      two specs become co-resident. Neither scenario alone would isolate the
//      arithmetic (see scenario 6's own comment); together they do.
//   3. Force-stop from the portal, and that a further inference starts the
//      model again.
//
// The stub model server is BUILT here and never started here (see
// playwright.runtime.config.ts's own note): starting it is the agent's job.

const t = messages.de;

// Direct gateway origin. Used for the raw SSE read and for the inference
// calls, so neither goes through the vite preview proxy that fronts the portal
// UI on 4173 (an SSE stream in particular has no business being buffered by an
// intermediary that is not part of what this suite tests).
const GW = "http://127.0.0.1:8091";
const ROUTER = `http://127.0.0.1:${RUNTIME_ROUTER_PORT}`;

// Memory-mode dev principals (cmd/gateway memoryDeps + seedDefaultServer).
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";
const DEV_DISPLAY_NAME = "Dev User";
// The dev API token, i.e. the bearer an OpenAI-compatible client would use.
const DEV_BEARER = "dev-secret";

const CSRF = { "X-OP-CSRF": "1" };

// Distinct, non-substring-colliding names. Several helpers below locate a table
// row via getByRole("row", { name: new RegExp(...) }), a SUBSTRING match
// against the row's whole accessible text -- so "e2e-model" and "e2e-model-b"
// would be indistinguishable (the same discipline e2e-servers/servers.spec.ts
// documents for its own group names).
const SERVER_NAME = "E2E-RT-Server";
const SYSTEM_GROUP = "E2E-RT-System";
const ADMIN_GROUP = "E2E-RT-Admin";
const MODEL_A = "e2e-model-alpha";
const MODEL_B = "e2e-model-bravo";
// The upstream (app_model_name) names: what the gateway sends upstream, what
// the agent's router routes on, and what the agent reports as `loaded_models`.
const UPSTREAM_A = "stub-alpha";
const UPSTREAM_B = "stub-bravo";
// The stub's -tag, echoed back inside the completion text. This is what turns
// "some upstream answered" into "THIS child process answered".
const TAG_A = "alpha";
const TAG_B = "bravo";

// Per-GPU VRAM arithmetic (design spec §5, rule 3). Two 700 MB specs against a
// 1000 MB budget: either alone fits, both together do not.
const VRAM_PER_SPEC_MB = 700;
const BUDGET_TIGHT_MB = 1000;
const BUDGET_ROOMY_MB = 2000;

let page: Page;
// A cookie-less request context: the inference calls must authenticate with the
// bearer token exactly as a real OpenAI-compatible client does, and the router
// probes must not carry a portal session at all.
let api: APIRequestContext;
let agent: ChildProcess | undefined;
let workDir = "";
let serverId = "";
let applicationId = "";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ browser }) => {
  // Build the REAL agent from its own module (mirrors e2e-agent/agent.spec.ts).
  execSync(`go build -o ${RUNTIME_AGENT_BIN} .`, {
    cwd: "../../server-agent",
    stdio: "inherit",
    env: { ...process.env, GOCACHE: process.env.GOCACHE ?? "/private/tmp/op-ai-gateway-go-build-cache" }
  });
  // Build the stub model server -- BUILD ONLY. It is never started from here;
  // every process of it that runs during this suite was exec'd by the agent,
  // which is the whole point (a `webServer` entry would defeat the suite while
  // leaving every assertion green).
  execSync(`go build -o ${RUNTIME_STUB_BIN} .`, {
    cwd: "e2e-runtime/fixtures/stubserver",
    stdio: "inherit",
    env: { ...process.env, GOCACHE: process.env.GOCACHE ?? "/private/tmp/op-ai-gateway-go-build-cache" }
  });

  // One real directory that is BOTH the agent's permitted work-directory
  // prefix and the specs' work_dir. LocalPolicy.Permit compares them
  // lexically and refuses a spec with an EMPTY work_dir outright once
  // OP_AGENT_RUNTIME_ALLOWED_DIRS is set, so the two must genuinely agree.
  workDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-e2e-runtime-work-"));

  page = await browser.newPage();
  api = await apiRequest.newContext();
});

test.afterAll(async () => {
  agent?.kill("SIGTERM");
  // Give the agent's own shutdown path (signal.NotifyContext -> deferred
  // mgr.Close) the moment it needs to drain-stop its children, so no stub
  // process outlives the suite.
  if (agent) {
    await new Promise<void>((resolve) => {
      const done = () => resolve();
      agent?.once("exit", done);
      setTimeout(done, 5000);
    });
  }
  await api?.dispose();
  await page?.close();
  if (workDir) fs.rmSync(workDir, { recursive: true, force: true });
});

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The row of any portal table whose accessible text contains `name`. */
function row(name: string): Locator {
  return page.getByRole("row", { name: new RegExp(escapeRegExp(name)) });
}

/**
 * Enters System-Admin mode (step-up): a fresh system_admin session is NOT
 * elevated, and the `system` scope is what lets this suite create a
 * system-tier group and see every admin group as a server candidate. Mirrors
 * e2e-servers/servers.spec.ts's helper of the same name.
 */
async function enterSystemAdminMode(): Promise<void> {
  await page.getByRole("button", { name: DEV_DISPLAY_NAME }).click();
  await page.getByRole("menuitem", { name: t.systemAdminModeEnter }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel(t.systemAdminModePasswordLabel).fill(DEV_PASSWORD);
  await dialog.getByRole("button", { name: t.systemAdminModeEnter }).click();
  await expect(dialog).toHaveCount(0);
}

/**
 * Creates one user group through the portal API. Group management is fully
 * covered by e2e:groups and e2e:servers; here it is pure precondition —
 * CreateServer requires at least one admin group for every caller, and memory
 * mode seeds none (the "Standard" pair is a SQL-migration artifact). Doing it
 * through the raw API keeps this suite's UI steps on the area it actually
 * owns. Uses the elevated session (page.request), the same credential every
 * sibling suite uses for raw portal writes.
 */
async function createGroup(name: string, tier: "system" | "admin", parentGroupId?: string): Promise<string> {
  const resp = await page.request.post("/api/portal/groups", {
    headers: CSRF,
    data: { name, tier, ...(parentGroupId === undefined ? {} : { parent_group_id: parentGroupId }) }
  });
  expect(resp.ok(), `creating the ${tier} group ${name}: ${resp.status()} ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { id: string }).id;
}

/** Reads the id of the AI server this suite created. */
async function resolveServerId(): Promise<string> {
  const resp = await page.request.get("/api/portal/servers");
  expect(resp.ok(), `listing servers: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { data: { id: string; name: string }[] };
  const found = body.data.find((s) => s.name === SERVER_NAME);
  expect(found, `expected ${SERVER_NAME} in ${JSON.stringify(body.data.map((s) => s.name))}`).toBeTruthy();
  return found!.id;
}

/** Mints the server's reporting-agent token, the credential the real agent runs with. */
async function mintAgentToken(): Promise<string> {
  const resp = await page.request.post(`/api/portal/servers/${serverId}/agent-token`, { headers: CSRF });
  expect(resp.ok(), `minting an agent token: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { secret?: string };
  expect(body.secret, "the one-time agent-token secret must come back on creation").toBeTruthy();
  return body.secret!;
}

/** The application row for the `server_agent` app, plus its live reachability. */
async function applicationState(): Promise<{ reachable: boolean; lastCheckedAt: string | null }> {
  const resp = await page.request.get(`/api/portal/servers/${serverId}/applications`);
  expect(resp.ok(), `listing applications: ${resp.status()} ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as {
    data: { id: string; reachable: boolean; last_checked_at: string | null }[];
  };
  const app = body.data.find((a) => a.id === applicationId);
  expect(app, `expected application ${applicationId} in the server's application list`).toBeTruthy();
  return { reachable: app!.reachable, lastCheckedAt: app!.last_checked_at };
}

type Completion = { status: number; content: string };

/**
 * One non-streaming inference through the gateway's OpenAI-compatible
 * endpoint, authenticated with the dev bearer token. Never throws on a non-2xx
 * — the caller asserts on the status, because "this request was refused" is
 * itself an outcome two scenarios below depend on.
 */
async function inference(model: string, prompt: string): Promise<Completion> {
  const resp = await api.post(`${GW}/openai/v1/chat/completions`, {
    headers: { Authorization: `Bearer ${DEV_BEARER}`, "Content-Type": "application/json" },
    data: { model, messages: [{ role: "user", content: prompt }] },
    // Generous: a cold start plus the stub's readiness delay happen INSIDE
    // this one request, which is exactly the property under test.
    timeout: 120000,
    failOnStatusCode: false
  });
  const text = await resp.text();
  if (!resp.ok()) return { status: resp.status(), content: text };
  const body = JSON.parse(text) as { choices?: { message?: { content?: string } }[] };
  return { status: resp.status(), content: body.choices?.[0]?.message?.content ?? "" };
}

/**
 * The router's health status, or 0 when the listener is not bound at all.
 * A refused connection has to be a VALUE rather than a thrown error: this is
 * polled while waiting for the agent to bind, and expect.poll aborts on a
 * throwing callback instead of retrying it.
 */
async function routerHealthStatus(): Promise<number> {
  try {
    return (await api.get(`${ROUTER}/health`, { failOnStatusCode: false })).status();
  } catch {
    return 0;
  }
}

/**
 * The upstream model names the agent's router reports as currently loaded, via
 * its own /running endpoint (llama-swap shape) — a second, independent source
 * of the same truth as the telemetry the portal renders. Sorted, so a
 * comparison never depends on process start order. Transport failures come
 * back as a value for the same reason as routerHealthStatus above.
 */
async function runningUpstreams(): Promise<string[]> {
  try {
    const resp = await api.get(`${ROUTER}/running`, { failOnStatusCode: false });
    if (!resp.ok()) return [`router ${resp.status()}`];
    const body = (await resp.json()) as { running: { model: string }[] };
    return body.running.map((r) => r.model).sort();
  } catch (err) {
    return [`router unreachable: ${String(err)}`];
  }
}

type RuntimeStatus = {
  spec_id: string;
  model: string;
  state: string;
  pid?: number;
  port?: number;
  in_flight: number;
  restarts: number;
};

/**
 * A raw reader of the portal's runtime-status SSE stream
 * (GET /api/portal/servers/{id}/runtime/events), collecting EVERY frame rather
 * than sampling the rendered DOM. That distinction is what makes the
 * `starting` assertion in scenario 3 deterministic: a transient state that
 * lasts ~2s cannot be reliably caught by polling a badge, but it cannot be
 * missed by a stream reader that keeps every frame it was sent.
 */
async function openRuntimeStream(): Promise<{
  statesOf(upstreamModel: string): string[];
  latest(upstreamModel: string): RuntimeStatus | undefined;
  close(): void;
}> {
  const cookies = await page.context().cookies(GW);
  const cookieHeader = cookies.map((c) => `${c.name}=${c.value}`).join("; ");
  const controller = new AbortController();
  const resp = await fetch(`${GW}/api/portal/servers/${serverId}/runtime/events`, {
    headers: { Cookie: cookieHeader, ...CSRF, Accept: "text/event-stream" },
    signal: controller.signal
  });
  expect(resp.ok, `opening the runtime SSE stream: ${resp.status}`).toBe(true);
  expect(resp.body, "the runtime SSE response must have a readable body").toBeTruthy();

  const frames: RuntimeStatus[][] = [];
  const reader = resp.body!.getReader();
  const decoder = new TextDecoder();
  void (async () => {
    let buffered = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        buffered += decoder.decode(value, { stream: true });
        let newline = buffered.indexOf("\n");
        while (newline >= 0) {
          const line = buffered.slice(0, newline).trim();
          buffered = buffered.slice(newline + 1);
          if (line.startsWith("data:")) {
            const payload = JSON.parse(line.slice("data:".length).trim()) as { runtimes: RuntimeStatus[] };
            frames.push(payload.runtimes);
          }
          newline = buffered.indexOf("\n");
        }
      }
    } catch {
      // An aborted read is the normal way this loop ends (close() below).
    }
  })();

  return {
    // Every distinct state this spec was reported in, in order, collapsing
    // consecutive repeats: the stream re-sends the full list on every sample
    // (~2/s), so the raw sequence is mostly repetition.
    statesOf(upstreamModel: string): string[] {
      const seen: string[] = [];
      for (const frame of frames) {
        const entry = frame.find((r) => r.model === upstreamModel);
        if (entry === undefined) continue;
        if (seen[seen.length - 1] !== entry.state) seen.push(entry.state);
      }
      return seen;
    },
    latest(upstreamModel: string): RuntimeStatus | undefined {
      for (let i = frames.length - 1; i >= 0; i -= 1) {
        const entry = frames[i].find((r) => r.model === upstreamModel);
        if (entry !== undefined) return entry;
      }
      return undefined;
    },
    close(): void {
      controller.abort();
    }
  };
}

/** Switches the runtime admin to one of its four tabs. */
async function openRuntimeTab(name: string): Promise<void> {
  await page.getByRole("tab", { name }).click();
}

/** The live-status badge text currently shown for a gateway model. */
function stateBadge(model: string, label: string): Locator {
  return row(model).locator("[data-status]").filter({ hasText: label });
}

/**
 * Fills and submits the runtime-spec create form. "Spezifikation anlegen"
 * creates BOTH the model mapping and its launch spec (submitCreate:
 * createMapping then putRuntimeSpec), so the PUT is what says the whole thing
 * landed.
 */
async function createSpec(opts: {
  model: string;
  upstream: string;
  tag: string;
  readyAfter: string;
  vramMb: number;
}): Promise<void> {
  await openRuntimeTab(t.runtimeSpecs);
  await page.getByRole("button", { name: t.runtimeSpecCreate }).click();

  await page.locator("#runtime-spec-gateway-name").fill(opts.model);
  await page.locator("#runtime-spec-app-name").fill(opts.upstream);
  await page.locator("#runtime-spec-binary").fill(RUNTIME_STUB_BIN);
  // One argument per line. ${PORT} is resolved by the AGENT to the loopback
  // port it picked for this child (runtime.ExpandPlaceholders) -- the gateway
  // never knows it, which is why listen_port stays 0.
  const args = ["-port", "${PORT}", "-tag", opts.tag];
  if (opts.readyAfter !== "") args.push("-ready-after", opts.readyAfter);
  await page.locator("#runtime-spec-args").fill(args.join("\n"));
  await page.locator("#runtime-spec-work-dir").fill(workDir);
  // 0 = never unload on idle. Left explicit: an idle ticker unloading a
  // process mid-suite would race every state assertion below.
  await page.locator("#runtime-spec-idle-timeout").fill("0");
  // Bounded, rather than the form default of 0 ("wait until the client
  // disconnects"): if admission ever blocks unexpectedly, this suite must fail
  // with a refusal, not hang until the application's 600s timeout.
  await page.locator("#runtime-spec-admission-wait-timeout").fill("30");

  await page.getByRole("button", { name: t.runtimeSpecGpuAdd }).click();
  await page.locator("#runtime-spec-gpu-index-0").fill("0");
  await page.locator("#runtime-spec-gpu-vram-0").fill(String(opts.vramMb));

  const [resp] = await Promise.all([
    page.waitForResponse((r) => /\/api\/portal\/mappings\/[^/]+\/runtime-spec$/.test(new URL(r.url()).pathname) && r.request().method() === "PUT"),
    page.getByRole("button", { name: t.runtimeSpecCreate }).click()
  ]);
  expect(resp.ok(), `saving the runtime spec for ${opts.model}: ${resp.status()} ${await resp.text()}`).toBe(true);
  await expect(row(opts.model)).toBeVisible();
}

/** Sets the server's single GPU-0 VRAM budget on the "Runtime-Limits" tab and saves. */
async function setGpuBudget(budgetMb: number): Promise<void> {
  await openRuntimeTab(t.runtimeLimits);
  const indexField = page.locator("#runtime-budget-index-0");
  if ((await indexField.count()) === 0) {
    // First visit: no budget row exists yet.
    await page.getByRole("button", { name: t.runtimeSpecGpuAdd }).click();
  }
  await indexField.fill("0");
  await page.locator("#runtime-budget-mb-0").fill(String(budgetMb));
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/portal/servers/${serverId}/gpu-budgets` && r.request().method() === "PUT"
    ),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok(), `saving the GPU budget: ${resp.status()} ${await resp.text()}`).toBe(true);
  // Read it back rather than asserting on the "Gespeichert." toast: the toasts
  // stack and never disappear, so by the second save a toast assertion matches
  // a stale one and proves nothing. What matters is that the value the
  // admission arithmetic reads is now stored.
  const stored = await page.request.get(`/api/portal/servers/${serverId}/gpu-budgets`);
  expect(stored.ok(), `reading the GPU budget back: ${stored.status()}`).toBe(true);
  const budgets = ((await stored.json()) as { budgets: { index: number; budget_mb: number }[] }).budgets;
  expect(budgets).toHaveLength(1);
  // toMatchObject, not toEqual: a new budget row also snapshots the live
  // GPU name/UUID from telemetry for the portal's drift detector, and on a
  // real machine that is the actual hardware ("Apple M4 Max" here) — not
  // something a test may pin.
  expect(budgets[0]).toMatchObject({ index: 0, budget_mb: budgetMb });
}

// --- Scenario 1 -------------------------------------------------------------

test("1 — an AI server and a server_agent application, created through the portal, with the real agent already running", async () => {
  await login(page, DEV_EMAIL, DEV_PASSWORD);
  await enterSystemAdminMode();

  // Preconditions for CreateServer's mandatory admin-group gate.
  const systemGroupId = await createGroup(SYSTEM_GROUP, "system");
  await createGroup(ADMIN_GROUP, "admin", systemGroupId);

  // The Servers view, waiting for the admin-group candidate fetch so the
  // create form's picker is resolved rather than racing an in-flight request
  // (e2e-servers/servers.spec.ts's gotoServers, same reason).
  const [candidates] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/portal/server-admin-group-candidates" && r.request().method() === "GET"
    ),
    page.getByRole("link", { name: t.servers }).click()
  ]);
  expect(candidates.ok()).toBe(true);

  // Domain 127.0.0.1: the gateway reaches the application at
  // server.Domain:app.Port (routing.ApplicationEndpoint), and app.Port is the
  // port the agent's router binds. With exactly one admin-group candidate the
  // picker auto-selects it and renders no field (AdminGroupPicker).
  await page.getByRole("button", { name: t.serverCreate }).click();
  await page.locator("#server-name").fill(SERVER_NAME);
  await page.locator("#server-domain").fill("127.0.0.1");
  await page.getByRole("button", { name: t.serverCreate }).click();
  await expect(row(SERVER_NAME)).toBeVisible();

  serverId = await resolveServerId();
  const agentToken = await mintAgentToken();

  // Spawn the REAL agent BEFORE the application exists, so it is already
  // polling when router_listen appears and binds within one interval. Its
  // runtime-config source is the gateway; its local policy permits exactly the
  // one stub binary and the one work directory.
  agent = spawn(RUNTIME_AGENT_BIN, [], {
    env: {
      ...process.env,
      OP_AGENT_GATEWAY_URL: GW,
      OP_AGENT_TOKEN: agentToken,
      OP_AGENT_INTERVAL: "500ms",
      OP_AGENT_RUNTIME_SOURCE: "gateway",
      OP_AGENT_RUNTIME_ALLOWED_BINARIES: RUNTIME_STUB_BIN,
      OP_AGENT_RUNTIME_ALLOWED_DIRS: workDir,
      // Explicit loopback. With no installed mesh leaf the derivation yields
      // "" and the router would bind ALL interfaces with a warning; this both
      // does the correct thing for a test and exercises the setting.
      OP_AGENT_RUNTIME_ROUTER_BIND: "127.0.0.1",
      OP_AGENT_RUNTIME_CACHE: path.join(workDir, "runtime.cache.json")
    },
    stdio: "inherit"
  });

  // The application. Its Port IS the agent's router port; the health path
  // keeps the form default /v1/health, which the router serves (and which,
  // per design §6.1, must answer 200 while NOTHING is loaded -- asserted
  // below). The health interval is pinned to the 5s minimum so a probe that
  // happens to fire before the router is bound is corrected quickly instead of
  // leaving the application unreachable for the 30s system default.
  await row(SERVER_NAME).getByRole("button", { name: t.applicationManage }).click();
  await page.getByRole("button", { name: t.applicationCreate }).click();
  await page.getByRole("combobox", { name: t.applicationType }).click();
  await page.getByRole("option", { name: "server_agent", exact: true }).click();
  await page.locator("#application-port").fill(String(RUNTIME_ROUTER_PORT));
  await page.getByRole("combobox", { name: t.applicationHealthIntervalLabel }).click();
  await page.getByRole("option", { name: t.applicationHealthIntervalCustom, exact: true }).click();
  await page.locator("#application-health-interval-seconds").fill("5");
  const [created] = await Promise.all([
    page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/portal/servers/${serverId}/applications` && r.request().method() === "POST"
    ),
    page.getByRole("button", { name: t.applicationCreate }).click()
  ]);
  expect(created.ok(), `creating the server_agent application: ${created.status()} ${await created.text()}`).toBe(true);
  const createdApp = (await created.json()) as { id: string; type: string; port: number; server_id: string };
  // The form really wrote what the runtime depends on: router_listen is
  // derived from exactly this application's Type + Port.
  expect(createdApp).toMatchObject({ type: "server_agent", port: RUNTIME_ROUTER_PORT, server_id: serverId });
  applicationId = createdApp.id;
});

// --- Scenario 2 -------------------------------------------------------------

test("2 — a spec written in the portal reaches the real agent: its router comes up, answers health with nothing loaded, and the spec reports stopped", async () => {
  // The runtime admin opens from the application's model view: for a
  // server_agent application, "Modell-Zuordnungen" renders RuntimeAdminSection
  // instead of the ordinary mapping form.
  await row(String(RUNTIME_ROUTER_PORT)).getByRole("button", { name: t.mappingManage }).click();
  await expect(page.getByRole("tablist", { name: t.runtimeAdmin })).toBeVisible();

  await createSpec({
    model: MODEL_A,
    upstream: UPSTREAM_A,
    tag: TAG_A,
    // A real, observable load: /health stays 503 for 2s after exec, so
    // `starting` lasts several telemetry samples instead of being raced past.
    readyAfter: "2s",
    vramMb: VRAM_PER_SPEC_MB
  });

  // The tight budget scenario 5 needs, set now so it is long settled by then.
  await setGpuBudget(BUDGET_TIGHT_MB);

  // The agent binds its router once a runtime-config carrying a non-zero
  // router_listen reaches it. Two writes can deliver that here, and BOTH now
  // push: creating the `server_agent` APPLICATION in scenario 1 (its Type +
  // Port are where router_listen comes from) and the spec write above --
  // portal/service_runtime.go's notifyRuntimeChanged fires for the
  // application write paths too, not only for spec/co-residency/budget
  // writes. Which one wins the race is not asserted; the point is that
  // neither leaves the router waiting on the agent's 60s poll backstop
  // (agent.runtimePollInterval), which is what the application create used to
  // do before that gate existed. The timeout below stays generous enough to
  // cover that backstop anyway, if a push is ever withheld.
  await expect.poll(routerHealthStatus, { timeout: 90000, intervals: [500] }).toBe(200);

  // Design §6.1, and the reason a managed server can warm up at all: the
  // router's health answer means "the router accepts", not "a model is warm".
  // Assert the gateway's OWN probe agrees -- last_checked_at proves a real
  // probe cycle ran (the registry's cold-start default is a lenient
  // "reachable" that no probe has confirmed), while /running proves nothing is
  // loaded at the moment it says so.
  expect(await runningUpstreams()).toEqual([]);
  await expect
    .poll(async () => await applicationState(), { timeout: 60000, intervals: [500] })
    .toEqual({ reachable: true, lastCheckedAt: expect.any(String) });
  expect(await runningUpstreams()).toEqual([]);

  await openRuntimeTab(t.runtimeLiveStatus);
  // The portal's own live view is SSE-fed; the chip says the stream is open.
  await expect(page.getByText(t.runtimeStreamOpen)).toBeVisible();
  // Delivered and known, but NOT running: the cross-process proof that the
  // gateway's document reached the agent's process manager.
  await expect(stateBadge(MODEL_A, t.runtimeStateStopped)).toBeVisible({ timeout: 30000 });
  expect(await runningUpstreams()).toEqual([]);
});

// --- Scenario 3 -------------------------------------------------------------

test("3 — cold start on demand: the inference itself starts the process, and the state comes back up", async () => {
  const stream = await openRuntimeStream();
  try {
    const completion = await inference(MODEL_A, "full circle");
    expect(completion.status, `inference body: ${completion.content}`).toBe(200);
    // The stub echoes its own -tag: this answer came from the child process
    // the AGENT started, not from anything this harness runs.
    expect(completion.content).toBe(`[${TAG_A}] echo: full circle`);

    // The whole visible load lifecycle, from the stream's own frames rather
    // than from a sampled badge: stopped (before), starting (the 2s readiness
    // window), running (loaded).
    await expect
      .poll(() => stream.statesOf(UPSTREAM_A), { timeout: 30000, intervals: [250] })
      .toEqual(["stopped", "starting", "running"]);

    const latest = stream.latest(UPSTREAM_A);
    expect(latest?.pid, "the agent must report the child's real PID").toBeGreaterThan(0);
    expect(latest?.port, "the agent must report the loopback port it assigned the child").toBeGreaterThan(0);

    // ${PORT} really was resolved to a port the AGENT picked, and the process
    // listening there really is the stub that served the completion.
    const stats = await api.get(`http://127.0.0.1:${latest!.port}/stats`);
    expect(stats.ok(), `the reported child port must serve the stub: ${stats.status()}`).toBe(true);
    expect(await stats.json()).toEqual({ tag: TAG_A, ready: true, completions: 1 });
  } finally {
    stream.close();
  }

  // The portal renders it, the router agrees, and the gateway's own
  // loaded-models view (fed by the agent's authoritative loaded_models) flips.
  await expect(stateBadge(MODEL_A, t.runtimeStateRunning)).toBeVisible({ timeout: 15000 });
  expect(await runningUpstreams()).toEqual([UPSTREAM_A]);
  await expect
    .poll(async () => {
      const resp = await page.request.get(`/api/portal/model-servers?name=${MODEL_A}`);
      if (!resp.ok()) return `model-servers ${resp.status()}`;
      const body = (await resp.json()) as { data: { loaded: boolean }[] };
      return body.data.map((r) => r.loaded);
    }, { timeout: 15000, intervals: [250] })
    .toEqual([true]);
});

// --- Scenario 4 -------------------------------------------------------------

test("4 — force stop from the portal really stops it, and a further inference starts it again", async () => {
  await openRuntimeTab(t.runtimeLiveStatus);

  const [stopped] = await Promise.all([
    page.waitForResponse((r) => /\/api\/portal\/mappings\/[^/]+\/runtime-spec$/.test(new URL(r.url()).pathname) && r.request().method() === "PUT"),
    row(MODEL_A).getByRole("button", { name: t.runtimeForceStop }).click()
  ]);
  expect(stopped.ok(), `force-stopping: ${stopped.status()} ${await stopped.text()}`).toBe(true);

  await expect(stateBadge(MODEL_A, t.runtimeStateStopped)).toBeVisible({ timeout: 30000 });
  await expect.poll(async () => await runningUpstreams(), { timeout: 30000, intervals: [250] }).toEqual([]);

  // force_stopped is a desired-state override, not a cosmetic badge: the
  // agent's router must refuse to start the model for a real request.
  const refused = await inference(MODEL_A, "still stopped?");
  expect(refused.status, `expected a refusal while force_stopped, got body: ${refused.content}`).not.toBe(200);
  expect(await runningUpstreams()).toEqual([]);

  const [cleared] = await Promise.all([
    page.waitForResponse((r) => /\/api\/portal\/mappings\/[^/]+\/runtime-spec$/.test(new URL(r.url()).pathname) && r.request().method() === "PUT"),
    row(MODEL_A).getByRole("button", { name: t.runtimeClearOverride }).click()
  ]);
  expect(cleared.ok(), `clearing the override: ${cleared.status()} ${await cleared.text()}`).toBe(true);

  // On-demand again, from a genuinely stopped process. Polled because the
  // cleared override has to reach the agent first; every round drives a real
  // inference, so a round can only pass by actually starting the model.
  await expect
    .poll(async () => (await inference(MODEL_A, "restart me")).content, { timeout: 60000, intervals: [500] })
    .toBe(`[${TAG_A}] echo: restart me`);
  await expect(stateBadge(MODEL_A, t.runtimeStateRunning)).toBeVisible({ timeout: 15000 });
  expect(await runningUpstreams()).toEqual([UPSTREAM_A]);
});

// --- Scenario 5 -------------------------------------------------------------

test("5 — admission arithmetic: with a budget for one, an inference for B evicts the idle A", async () => {
  await createSpec({
    model: MODEL_B,
    upstream: UPSTREAM_B,
    tag: TAG_B,
    // No readiness delay: scenario 3 already owns the cold-load proof, and
    // these two scenarios drive several starts each.
    readyAfter: "",
    vramMb: VRAM_PER_SPEC_MB
  });

  // Allow the pair in the co-residency matrix, so rule 1 (matrix) is OFF and
  // only the arithmetic can block. With exactly two specs the strictly-lower
  // triangle holds exactly one cell, so this locator is unambiguous without
  // depending on which spec the matrix happens to render as row vs column.
  await openRuntimeTab(t.runtimeMatrix);
  // role=checkbox, not button: the cell is a real Checkbox. It was an
  // IconButton with aria-pressed until the off state was made readable as
  // "you may tick this" rather than as a prohibition, and this locator was
  // not updated with it -- CI runs no Playwright suite, so nothing caught it.
  const cell = page.getByRole("checkbox", { name: new RegExp(`^${escapeRegExp(t.runtimeMatrixCell)}: `) });
  await expect(cell).toHaveCount(1);
  await expect(cell).not.toBeChecked();
  const [paired] = await Promise.all([
    page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/portal/applications/${applicationId}/runtime/coresidency` &&
        r.request().method() === "PUT"
    ),
    cell.click()
  ]);
  expect(paired.ok(), `allowing the co-residency pair: ${paired.status()} ${await paired.text()}`).toBe(true);
  await expect(cell).toBeChecked();

  // 700 + 700 > 1000: exactly one of the two may hold GPU 0. Each round drives
  // BOTH inferences, so whichever spec is currently stopped genuinely goes
  // through admission again -- the loop can only converge by re-running the
  // decision, never by re-reading a stale one. The round is expected to end
  // with B running and A evicted: A is idle (in_flight 0 after its completion
  // returned), and the design's rule is "evict idle, wait for busy".
  await expect
    .poll(
      async () => {
        const a = await inference(MODEL_A, "a-turn");
        const b = await inference(MODEL_B, "b-turn");
        return { a: `${a.status} ${a.content}`, b: `${b.status} ${b.content}`, running: await runningUpstreams() };
      },
      { timeout: 120000, intervals: [500] }
    )
    .toEqual({
      a: `200 [${TAG_A}] echo: a-turn`,
      b: `200 [${TAG_B}] echo: b-turn`,
      running: [UPSTREAM_B]
    });

  await openRuntimeTab(t.runtimeLiveStatus);
  await expect(stateBadge(MODEL_A, t.runtimeStateStopped)).toBeVisible({ timeout: 15000 });
  await expect(stateBadge(MODEL_B, t.runtimeStateRunning)).toBeVisible({ timeout: 15000 });
});

// --- Scenario 6 -------------------------------------------------------------

test("6 — raise the budget and nothing else, and the same two models become co-resident", async () => {
  // This scenario is what makes scenario 5 an assertion about the ARITHMETIC.
  // On its own, scenario 5's eviction is ambiguous: an un-delivered matrix pair
  // (rule 1) produces the identical outcome. Two models running side by side
  // here -- same specs, same matrix pair, only the budget number changed --
  // can only happen if the agent really holds that pair, which retroactively
  // pins scenario 5's eviction on the VRAM sum. It is also the headline the
  // feature exists for: several models, and operator control over which may
  // run together.
  await setGpuBudget(BUDGET_ROOMY_MB);

  await expect
    .poll(
      async () => {
        const a = await inference(MODEL_A, "together-a");
        const b = await inference(MODEL_B, "together-b");
        return { a: `${a.status} ${a.content}`, b: `${b.status} ${b.content}`, running: await runningUpstreams() };
      },
      { timeout: 120000, intervals: [500] }
    )
    .toEqual({
      a: `200 [${TAG_A}] echo: together-a`,
      b: `200 [${TAG_B}] echo: together-b`,
      running: [UPSTREAM_A, UPSTREAM_B]
    });

  await openRuntimeTab(t.runtimeLiveStatus);
  await expect(stateBadge(MODEL_A, t.runtimeStateRunning)).toBeVisible({ timeout: 15000 });
  await expect(stateBadge(MODEL_B, t.runtimeStateRunning)).toBeVisible({ timeout: 15000 });

  // Two DISTINCT child processes, each answering for its own spec.
  const resp = await page.request.get(`/api/portal/model-servers?name=${MODEL_B}`);
  expect(resp.ok()).toBe(true);
  expect(((await resp.json()) as { data: { loaded: boolean }[] }).data.map((r) => r.loaded)).toEqual([true]);
});
