// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { expect, test } from "@playwright/test";
import { HEAD_ROUTER_PORT, MEMBER_ROUTER_PORT, RPC_AGENT_BIN } from "../playwright.rpc-cluster.config";

// A real llama.cpp RPC cluster across real process boundaries: the gateway,
// two server-agent processes (one per AI server), a llama-server cluster head
// on the head server and a ggml-rpc-server member on the member server. The
// in-process tests fake every one of those processes; this suite is where the
// whole members-first start runs against the real builds:
//
//   (a) relay transport, the head names its members through the environment
//       (LLAMA_ARG_RPC=${RPC_SERVERS}); stopped with stop_members, so the
//       member stops with the head.
//   (b) direct transport, the head names them in argv
//       (--rpc ${RPC_SERVERS} --device ${DEVICES}); stopped without
//       stop_members, so the member idles out on its own once its start
//       reservation has run out.
//   (c) the member process of a formed cluster is killed: the head's next
//       request fails, and the head's agent does not start the head again on
//       its own.
//
// Each scenario: manual start -> the gateway reports the cluster formed and
// the member reports the head's hold -> a completion through the gateway
// returns text -> stop.
//
// Steps that depend on gateway parts not built yet are test.fixme with a note
// saying what enables them: a request that starts a cold cluster (the routing
// gate), member usage stored on the head, the watchdog, and the cluster code
// on a failed request.
//
// Network: both servers are `lan` servers with lan_address 127.0.0.1 and
// lan_bind_only, so every member and relay binds loopback (binding all
// interfaces is neither needed nor allowed on every development machine) and
// the member sees the head at 127.0.0.1, the head address it admits.
//
// The head declares no GPUs, so ${DEVICES} resolves to the RPC devices alone:
// every offloaded layer runs on the member, on any build. The member declares
// none either and runs `-d CPU`. That keeps the suite deterministic on a Metal
// build too, where only the CPU fallback of the head (layers not offloaded,
// none here with -ngl 99) would differ. `-fa off` is set because flash
// attention for stories260K's head size is unsupported on Metal, and an RPC
// device cannot report an unsupported op to the head; it matters only for a
// Metal-backed member, and costs nothing on the CPU-only CI build.
//
// Local vs CI: CI builds llama-server and ggml-rpc-server CPU-only at the
// pinned commit with static libraries; locally any RPC-enabled build of that
// commit works (a shared-library build runs from its build tree). The member
// reports the host's total RAM as its free memory either way.

const GW = "http://127.0.0.1:8091";
const CSRF = { "X-OP-CSRF": "1" };

// Memory-mode dev principals (cmd/gateway memoryDeps).
const DEV_EMAIL = "dev@example.test";
const DEV_PASSWORD = "dev-secret";
const DEV_BEARER = "dev-secret";

const SYSTEM_GROUP = "E2E-RPC-System";
const ADMIN_GROUP = "E2E-RPC-Admin";
const HEAD_SERVER = "E2E-RPC-Head";
const MEMBER_SERVER = "E2E-RPC-Member";

const BIN_DIR = process.env.OP_E2E_LLAMA_BIN_DIR ?? "";
const MODEL = process.env.OP_E2E_RPC_MODEL ?? "";
const LLAMA_SERVER = BIN_DIR === "" ? "" : path.join(path.resolve(BIN_DIR), "llama-server");
const RPC_SERVER = BIN_DIR === "" ? "" : path.join(path.resolve(BIN_DIR), "ggml-rpc-server");

// The gateway's idle timeout for a cluster member process.
const MEMBER_IDLE_TIMEOUT_MS = 60000;

const GOCACHE = process.env.GOCACHE ?? "/private/tmp/op-ai-gateway-go-build-cache";

// --- A minimal portal client -------------------------------------------------

/**
 * The portal API with a session cookie. Node's fetch rather than Playwright's
 * request context, because the runtime status is an SSE stream that this
 * suite reads frame by frame.
 */
class PortalClient {
  private cookies = new Map<string, string>();

  private cookieHeader(): string {
    return [...this.cookies].map(([k, v]) => `${k}=${v}`).join("; ");
  }

  private remember(resp: Response): void {
    for (const line of resp.headers.getSetCookie()) {
      const [pair] = line.split(";");
      const eq = pair.indexOf("=");
      if (eq > 0) this.cookies.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim());
    }
  }

  async call(method: string, url: string, body?: unknown): Promise<{ status: number; text: string; json: any }> {
    const resp = await fetch(`${GW}${url}`, {
      method,
      headers: { ...CSRF, Cookie: this.cookieHeader(), ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
      body: body === undefined ? undefined : JSON.stringify(body)
    });
    this.remember(resp);
    const text = await resp.text();
    let json: any = undefined;
    try {
      json = text === "" ? undefined : JSON.parse(text);
    } catch {
      // Not JSON; the caller reads text.
    }
    return { status: resp.status, text, json };
  }

  /** call() that fails the test on a non-2xx answer and returns the body. */
  async ok(method: string, url: string, body?: unknown): Promise<any> {
    const r = await this.call(method, url, body);
    expect(r.status, `${method} ${url}: ${r.status} ${r.text}`).toBeLessThan(300);
    return r.json;
  }

  /**
   * One snapshot of a server's runtime status: the first frame of
   * GET /api/portal/servers/{id}/runtime/events, then the stream is closed.
   * Transport failures come back as an empty list so expect.poll retries.
   */
  async runtimeRows(serverId: string): Promise<RuntimeRow[]> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10000);
    try {
      const resp = await fetch(`${GW}/api/portal/servers/${serverId}/runtime/events`, {
        headers: { ...CSRF, Cookie: this.cookieHeader(), Accept: "text/event-stream" },
        signal: controller.signal
      });
      if (!resp.ok || resp.body === null) return [];
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffered = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return [];
        buffered += decoder.decode(value, { stream: true });
        // Complete lines only: a frame may arrive in pieces.
        for (const line of buffered.split("\n").slice(0, -1)) {
          if (line.startsWith("data:")) {
            return (JSON.parse(line.slice("data:".length).trim()) as { runtimes: RuntimeRow[] }).runtimes;
          }
        }
      }
    } catch {
      return [];
    } finally {
      clearTimeout(timer);
      controller.abort();
    }
  }
}

type RuntimeRow = {
  spec_id: string;
  model: string;
  state: string;
  pid?: number;
  kind?: string;
  rpc?: { held_by?: string; reserved_until?: string; device_count: number; protocol?: string };
  cluster?: { launched: string[]; version: string };
  last_error?: { code?: string; message?: string };
};

type ClusterStatus = {
  state: string;
  phase?: string;
  members: { position: number; state: string; code?: string }[];
  last_failure?: { code: string };
};

type RuntimeSpec = {
  id: string;
  cluster_withheld_reason?: string;
  cluster_status?: ClusterStatus;
  cluster_members: { server_id: string; port: number; ram_measured_mb: number }[];
};

// --- Suite state ---------------------------------------------------------------

const portal = new PortalClient();
const agents: ChildProcess[] = [];
let workRoot = "";
let headServerId = "";
let memberServerId = "";
let headAppId = "";

test.describe.configure({ mode: "serial" });

test.beforeAll(() => {
  // Fail loudly, never skip: a suite that skips when its build is missing
  // would let CI stay green without ever forming a cluster.
  if (BIN_DIR === "" || MODEL === "") {
    throw new Error("e2e:rpc-cluster needs OP_E2E_LLAMA_BIN_DIR (llama-server + ggml-rpc-server) and OP_E2E_RPC_MODEL (stories260K.gguf)");
  }
  for (const f of [LLAMA_SERVER, RPC_SERVER, MODEL]) {
    if (!fs.existsSync(f)) throw new Error(`e2e:rpc-cluster: ${f} does not exist`);
  }
  execSync(`go build -o ${RPC_AGENT_BIN} .`, { cwd: "../../server-agent", stdio: "inherit", env: { ...process.env, GOCACHE } });
  workRoot = fs.mkdtempSync(path.join(os.tmpdir(), "op-e2e-rpc-cluster-"));
});

test.afterAll(async () => {
  for (const a of agents) a.kill("SIGTERM");
  // The agents drain-stop their children on SIGTERM; give them the moment.
  await Promise.all(
    agents.map(
      (a) =>
        new Promise<void>((resolve) => {
          if (a.exitCode !== null) return resolve();
          a.once("exit", () => resolve());
          setTimeout(resolve, 10000);
        })
    )
  );
  if (workRoot !== "") {
    for (const name of ["head", "member"]) {
      const log = path.join(workRoot, name, "agent.log");
      if (fs.existsSync(log)) {
        const lines = fs.readFileSync(log, "utf8").split("\n");
        console.log(`--- last lines of the ${name} agent's log (${log}) ---\n${lines.slice(-40).join("\n")}`);
      }
    }
    if (process.env.OP_E2E_KEEP_LOGS !== "1") fs.rmSync(workRoot, { recursive: true, force: true });
  }
});

// --- Helpers ---------------------------------------------------------------------

/** Spawns one real agent with its own state directory and log file. */
function spawnAgent(name: string, token: string, routerPort: number): void {
  const dir = path.join(workRoot, name);
  fs.mkdirSync(dir, { recursive: true });
  // An explicit, empty config file: without one the agent reads the
  // server-agent.json beside its binary, which both agents would share.
  fs.writeFileSync(path.join(dir, "server-agent.json"), "{}\n");
  const log = fs.openSync(path.join(dir, "agent.log"), "a");
  const child = spawn(RPC_AGENT_BIN, [], {
    env: {
      ...process.env,
      OP_AGENT_GATEWAY_URL: GW,
      OP_AGENT_TOKEN: token,
      OP_AGENT_INTERVAL: "500ms",
      // No shared state: both agents run from one binary, whose directory
      // would otherwise hold both agents' defaults.
      OP_AGENT_CONFIG: path.join(dir, "server-agent.json"),
      OP_AGENT_CONFIG_FILES_DIR: path.join(dir, "config-files"),
      OP_AGENT_RUNTIME_CACHE: path.join(dir, "runtime.cache.json"),
      OP_AGENT_RUNTIME_SOURCE: "gateway",
      // Both binaries on both agents: the inventory versions an
      // ggml-rpc-server by the llama-server in the same directory.
      OP_AGENT_RUNTIME_ALLOWED_BINARIES: `${LLAMA_SERVER},${RPC_SERVER}`,
      OP_AGENT_RUNTIME_ROUTER_BIND: "127.0.0.1",
      OP_AGENT_EVICTION_GRACE_SECONDS: "0"
    },
    stdio: ["ignore", log, log]
  });
  agents.push(child);
  // The router port is the application's; kept here so the log names it.
  fs.writeSync(log, `# e2e: agent ${name}, router port ${routerPort}\n`);
}

async function createServer(name: string, adminGroupId: string, systemGroupId: string, rpcMember: boolean): Promise<string> {
  const body = await portal.ok("POST", "/api/portal/servers", {
    name,
    domain: "127.0.0.1",
    admin_group_ids: [adminGroupId],
    system_group_id: systemGroupId,
    lan_address: "127.0.0.1",
    lan_bind_only: true,
    rpc_member_enabled: rpcMember
  });
  return body.id as string;
}

async function createAgentApplication(serverId: string, port: number): Promise<string> {
  const body = await portal.ok("POST", `/api/portal/servers/${serverId}/applications`, {
    type: "server_agent",
    port,
    scheme: "http",
    health_check_interval_seconds: 5
  });
  return body.id as string;
}

async function binaries(serverId: string): Promise<{ path: string; kind: string; commit: string; build: string; allowed: boolean }[]> {
  const r = await portal.call("GET", `/api/portal/servers/${serverId}/binaries`);
  return r.status === 200 ? r.json.data : [];
}

async function runtimeSpec(mappingId: string): Promise<RuntimeSpec> {
  return (await portal.ok("GET", `/api/portal/mappings/${mappingId}/runtime-spec`)) as RuntimeSpec;
}

/** Row of spec specId on serverId, or undefined. */
async function row(serverId: string, match: (r: RuntimeRow) => boolean): Promise<RuntimeRow | undefined> {
  return (await portal.runtimeRows(serverId)).find(match);
}

/** The head's state on the head server, "absent" without a row. */
async function headState(headSpecId: string): Promise<string> {
  return (await row(headServerId, (r) => r.spec_id === headSpecId))?.state ?? "absent";
}

function memberRowOf(headSpecId: string): (r: RuntimeRow) => boolean {
  return (r) => r.kind === "rpc_server" && r.spec_id.includes(headSpecId);
}

type Completion = { status: number; text: string; content: string };

/** One chat completion through the gateway with the dev bearer token. */
async function complete(model: string): Promise<Completion> {
  const resp = await fetch(`${GW}/openai/v1/chat/completions`, {
    method: "POST",
    headers: { Authorization: `Bearer ${DEV_BEARER}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      model,
      messages: [{ role: "user", content: "Once upon a time" }],
      max_tokens: 32,
      temperature: 0
    }),
    signal: AbortSignal.timeout(120000)
  });
  const text = await resp.text();
  let content = "";
  if (resp.ok) {
    const body = JSON.parse(text) as { choices?: { message?: { content?: string } }[] };
    content = body.choices?.[0]?.message?.content ?? "";
  }
  return { status: resp.status, text, content };
}

/** POST .../runtime/start, answered 200 running or 202 starting. */
async function startCluster(mappingId: string): Promise<void> {
  const r = await portal.call("POST", `/api/portal/mappings/${mappingId}/runtime/start`, {});
  expect([200, 202], `starting the cluster head: ${r.status} ${r.text}`).toContain(r.status);
}

/** POST .../runtime/stop for the head, answered 200 stopped or 202 stopping. */
async function stopCluster(mappingId: string, stopMembers: boolean): Promise<void> {
  const r = await portal.call("POST", `/api/portal/mappings/${mappingId}/runtime/stop`, { instance: 0, stop_members: stopMembers });
  expect([200, 202], `stopping the cluster head: ${r.status} ${r.text}`).toContain(r.status);
}

/**
 * Waits for the gateway to report the cluster formed. A failed start shows in
 * the timeout message with the whole cluster_status (cause and position).
 */
async function awaitFormed(mappingId: string): Promise<RuntimeSpec> {
  let last: RuntimeSpec | undefined;
  await expect
    .poll(
      async () => {
        last = await runtimeSpec(mappingId);
        const st = last.cluster_status;
        if (st?.state === "cooling_down" || st?.state === "invalid") return `failed: ${JSON.stringify(st)}`;
        return st?.state ?? "none";
      },
      { timeout: 180000, intervals: [1000] }
    )
    .toBe("formed");
  return last!;
}

// --- Setup -----------------------------------------------------------------------

test("setup — two AI servers, each with a real agent, and the llama.cpp binaries in both inventories", async () => {
  await portal.ok("POST", "/api/auth/login", { email: DEV_EMAIL, password: DEV_PASSWORD });
  // Step up into System-Admin mode: creating a system-tier group needs it.
  await portal.ok("POST", "/api/portal/system-admin-mode", { password: DEV_PASSWORD });

  const systemGroup = await portal.ok("POST", "/api/portal/groups", { name: SYSTEM_GROUP, tier: "system" });
  const adminGroup = await portal.ok("POST", "/api/portal/groups", {
    name: ADMIN_GROUP,
    tier: "admin",
    parent_group_id: systemGroup.id
  });

  // Both servers in the same admin group: a member's default consent is its
  // own admin groups, which then admit the head's server.
  headServerId = await createServer(HEAD_SERVER, adminGroup.id, systemGroup.id, false);
  memberServerId = await createServer(MEMBER_SERVER, adminGroup.id, systemGroup.id, true);

  const headToken = (await portal.ok("POST", `/api/portal/servers/${headServerId}/agent-token`)).secret as string;
  const memberToken = (await portal.ok("POST", `/api/portal/servers/${memberServerId}/agent-token`)).secret as string;
  expect(headToken, "the head agent token").toBeTruthy();
  expect(memberToken, "the member agent token").toBeTruthy();

  // The agents start before their applications exist, so each binds its
  // router within one poll of router_listen appearing.
  spawnAgent("head", headToken, HEAD_ROUTER_PORT);
  spawnAgent("member", memberToken, MEMBER_ROUTER_PORT);
  headAppId = await createAgentApplication(headServerId, HEAD_ROUTER_PORT);
  await createAgentApplication(memberServerId, MEMBER_ROUTER_PORT);

  // The automatic member mode needs a ggml-rpc-server on the member server
  // whose version matches the head's llama-server; both are read from the
  // agents' inventories, which the agent derives from `llama-server
  // --version` (the rpc-server inherits the version of the llama-server
  // beside it).
  await expect
    .poll(async () => (await binaries(headServerId)).find((b) => b.path === LLAMA_SERVER)?.commit ?? "", {
      timeout: 60000,
      intervals: [1000]
    })
    .not.toBe("");
  await expect
    .poll(
      async () => {
        const rpc = (await binaries(memberServerId)).find((b) => b.path === RPC_SERVER);
        return rpc !== undefined && rpc.kind === "rpc_server" && rpc.allowed && rpc.commit !== "";
      },
      { timeout: 60000, intervals: [1000] }
    )
    .toBe(true);
});

// --- Scenarios ---------------------------------------------------------------------

type Scenario = {
  label: string;
  gatewayModel: string;
  appModel: string;
  transport: "relay" | "direct";
  args: string[];
  env: Record<string, string>;
  stopMembers: boolean;
};

const COMMON_ARGS = ["-m", MODEL, "--host", "127.0.0.1", "--port", "${PORT}", "--ctx-size", "256", "-fa", "off", "-ngl", "99"];

const RELAY: Scenario = {
  label: "(a) relay transport, head in env mode",
  gatewayModel: "e2e-rpc-relay",
  appModel: "stories-relay",
  transport: "relay",
  args: COMMON_ARGS,
  // LLAMA_ARG_DEVICE in the environment too: llama-server parses every
  // variable before argv, and LLAMA_ARG_RPC is a variable here, so the
  // devices resolve after the RPC servers registered.
  env: { LLAMA_ARG_RPC: "${RPC_SERVERS}", LLAMA_ARG_DEVICE: "${DEVICES}" },
  stopMembers: true
};

const DIRECT: Scenario = {
  label: "(b) direct transport, head in args mode",
  gatewayModel: "e2e-rpc-direct",
  appModel: "stories-direct",
  transport: "direct",
  args: [...COMMON_ARGS, "--rpc", "${RPC_SERVERS}", "--device", "${DEVICES}"],
  env: {},
  stopMembers: false
};

const mappings = new Map<string, { mappingId: string; headSpecId: string }>();

for (const sc of [RELAY, DIRECT]) {
  test.describe(sc.label, () => {
    test("the head spec saves as a cluster head and reaches both agents", async () => {
      const mapping = await portal.ok("POST", `/api/portal/applications/${headAppId}/mappings`, {
        gateway_model_name: sc.gatewayModel,
        app_model_name: sc.appModel
      });
      const spec = (await portal.ok("PUT", `/api/portal/mappings/${mapping.id}/runtime-spec`, {
        enabled: true,
        type: "llama_cpp",
        binary: LLAMA_SERVER,
        args: sc.args,
        env: sc.env,
        work_dir: "",
        listen_port: 0,
        health_path: "/health",
        // A cluster head needs an explicit startup bound: a cold start waits
        // for every member, then for the head. stories260K loads in about a
        // second; the bound also sizes the members' start reservation, which
        // scenario (b) waits out.
        startup_timeout_seconds: 30,
        // Never idle the head out mid-scenario; the stop is explicit.
        idle_timeout_seconds: 3600,
        admission_wait_timeout_seconds: 30,
        gpus: [],
        cluster_enabled: true,
        member_mode: "automatic",
        cluster_network: "lan",
        rpc_transport: sc.transport,
        // A direct lan member listens on the LAN address; the acknowledgement
        // is the operator's. Loopback here, but the rule is the same.
        lan_exposure_ack: sc.transport === "direct",
        cluster_members: [{ server_id: memberServerId, gpus: [], device_backend: "auto" }]
      })) as RuntimeSpec;
      expect(spec.id, "the saved head spec id").toBeTruthy();
      mappings.set(sc.label, { mappingId: mapping.id, headSpecId: spec.id });

      // Pushed to both agents only once both declared rpc_cluster: until then
      // the derivation withholds the head (features pending).
      await expect
        .poll(async () => (await runtimeSpec(mapping.id)).cluster_withheld_reason ?? "", { timeout: 60000, intervals: [1000] })
        .toBe("");
      expect((await runtimeSpec(mapping.id)).cluster_status?.state).toBe("cold");
    });

    // The routing gate parks a request for a cold cluster head and starts the
    // cluster for it (members first); the request is then served by the
    // formed cluster. Enable once the gate is in the gateway: until then a
    // request for a cold head is refused by the head's agent with
    // runtime.cluster_not_ready. It must run first in its scenario: a manual
    // stop parks the head until a manual start, so after one a request never
    // starts the cluster.
    test.fixme("a request for the cold head parks, the cluster forms, and the request is answered", async () => {
      const { mappingId } = mappings.get(sc.label)!;
      const c = await complete(sc.gatewayModel);
      expect(c.status, c.text).toBe(200);
      expect(c.content.trim()).not.toBe("");
      await awaitFormed(mappingId);
    });

    test("a manual start forms the cluster: member first, then the head, and the member reports the head's hold", async () => {
      const { mappingId, headSpecId } = mappings.get(sc.label)!;
      await startCluster(mappingId);
      const formed = await awaitFormed(mappingId);
      expect(formed.cluster_status!.members.map((m) => m.position)).toEqual([0]);

      // The member's own agent sees the head on its RPC port.
      await expect
        .poll(async () => (await row(memberServerId, memberRowOf(headSpecId)))?.rpc?.held_by ?? "", { timeout: 30000, intervals: [1000] })
        .toBe(headSpecId);
      const member = (await row(memberServerId, memberRowOf(headSpecId)))!;
      expect(member.state).toBe("running");
      // A CPU member exposes exactly one device (-d CPU), the head's RPC0.
      expect(member.rpc?.device_count).toBe(1);

      // The head runs with the member's endpoint on its command line. Polled:
      // the gateway learns the head is up from the agent's ensure answer,
      // the status stream from the next telemetry frame.
      await expect
        .poll(async () => (await row(headServerId, (r) => r.spec_id === headSpecId))?.state ?? "absent", { timeout: 30000, intervals: [1000] })
        .toBe("running");
      const head = (await row(headServerId, (r) => r.spec_id === headSpecId))!;
      const port = formed.cluster_members[0].port;
      expect(port, "the member's allocated port").toBeGreaterThan(0);
      expect(head.cluster?.launched).toEqual([`127.0.0.1:${port}`]);
    });

    test("a completion through the gateway is served by the cluster and returns text", async () => {
      const c = await complete(sc.gatewayModel);
      expect(c.status, c.text).toBe(200);
      expect(c.content.trim(), "the cluster's completion text").not.toBe("");
    });

    // The gateway measures each member's usage per run and stores it on the
    // head's membership row. Enable once member-usage ingest is in the
    // gateway: until then nothing writes ram_measured_mb.
    test.fixme("the member's measured usage is stored on the head", async () => {
      const { mappingId } = mappings.get(sc.label)!;
      await expect
        .poll(async () => (await runtimeSpec(mappingId)).cluster_members[0].ram_measured_mb, { timeout: 30000, intervals: [1000] })
        .toBeGreaterThan(0);
    });

    test(`stop ${sc.stopMembers ? "with stop_members stops the member with the head" : "without stop_members lets the member idle out"}`, async () => {
      const { mappingId, headSpecId } = mappings.get(sc.label)!;
      await stopCluster(mappingId, sc.stopMembers);
      await expect
        .poll(async () => (await row(headServerId, (r) => r.spec_id === headSpecId))?.state ?? "absent", { timeout: 60000, intervals: [1000] })
        .toMatch(/^(stopped|absent)$/);
      await expect.poll(async () => (await runtimeSpec(mappingId)).cluster_status?.state, { timeout: 30000, intervals: [1000] }).toBe("cold");
      // stop_members releases and stops the member right after the head.
      // Without it the gateway leaves the member alone: its hold ends with the
      // head's connection, but the reservation the members-first start armed
      // (the head's startup bound plus the orchestration's margins) still
      // runs, and only once that ends does the member idle timeout (60 s)
      // start. The bound is read from the member's own reservation.
      let timeout = 60000;
      if (!sc.stopMembers) {
        const member = await row(memberServerId, memberRowOf(headSpecId));
        const reservedUntil = Date.parse(member?.rpc?.reserved_until ?? "") || Date.now();
        timeout = Math.max(reservedUntil - Date.now(), 0) + MEMBER_IDLE_TIMEOUT_MS + 30000;
      }
      await expect
        .poll(async () => (await row(memberServerId, memberRowOf(headSpecId)))?.state ?? "absent", { timeout, intervals: [2000] })
        .toMatch(/^(stopped|absent)$/);
    });
  });
}

test.describe("(c) member process killed", () => {
  test("a formed cluster loses its member process", async () => {
    // The direct cluster of (b) again; a manual start ends the park its stop set.
    const { mappingId, headSpecId } = mappings.get(DIRECT.label)!;
    await startCluster(mappingId);
    await awaitFormed(mappingId);
    const member = (await row(memberServerId, memberRowOf(headSpecId)))!;
    expect(member.pid, "the member's pid").toBeGreaterThan(0);
    process.kill(member.pid!, "SIGKILL");
  });

  // The gateway's watchdog stops the head once a member it relies on exits,
  // before any request reaches the head. Enable once the watchdog is in the
  // gateway: until then the head only ends when it next talks to the lost
  // member (the test below).
  test.fixme("the watchdog stops the head without a request", async () => {
    const { headSpecId } = mappings.get(DIRECT.label)!;
    await expect
      .poll(async () => headState(headSpecId), { timeout: 60000, intervals: [1000] })
      .not.toMatch(/^(running|starting)$/);
  });

  test("the head's next request fails, and the head's agent does not start it again on its own", async () => {
    const { headSpecId } = mappings.get(DIRECT.label)!;
    // A head that loses a member aborts as soon as it talks to it
    // ("Remote RPC server crashed"), so the request fails rather than being
    // answered by a head without its member.
    await expect
      .poll(async () => (await complete(DIRECT.gatewayModel)).status, { timeout: 60000, intervals: [2000] })
      .toBeGreaterThanOrEqual(500);
    await expect
      .poll(async () => headState(headSpecId), { timeout: 30000, intervals: [1000] })
      .not.toMatch(/^(running|starting)$/);
    // Only the gateway's members-first start may start a cold head: a request
    // reaching the head's agent directly is refused (runtime.cluster_not_ready)
    // and the head stays down.
    const again = await complete(DIRECT.gatewayModel);
    expect(again.status, again.text).toBeGreaterThanOrEqual(500);
    expect(await headState(headSpecId)).not.toMatch(/^(running|starting)$/);
  });

  // The client sees the cluster's own code (runtime.cluster_*) rather than a
  // generic provider.unavailable. Enable once the routing gate maps cluster
  // refusals to their codes: until then the agent's runtime.cluster_not_ready
  // appears only in the gateway's log. When enabling it together with the
  // gate, check the expectation still holds: the gate may start the cluster
  // again for this request, members first.
  test.fixme("the failed request carries a cluster code", async () => {
    const c = await complete(DIRECT.gatewayModel);
    expect(c.status, c.text).toBeGreaterThanOrEqual(400);
    expect(c.text).toMatch(/"runtime\.cluster_[a-z_]+"/);
  });

  test("cleanup: the head is stopped with its members", async () => {
    const { mappingId } = mappings.get(DIRECT.label)!;
    await stopCluster(mappingId, true);
  });
});
