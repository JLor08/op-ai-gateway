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
//       stop_members, so the gateway only releases the member, which then
//       idles out on its own short timeout.
//   (c) the member process of a formed cluster is killed: the gateway's
//       watchdog stops the head without any request, the head cools down, a
//       request meanwhile is refused with the cluster's own code, and once the
//       cooldown ended a request forms the cluster again, members first.
//
// Each of (a) and (b): a request for the cold head parks while the gateway
// starts the cluster, and is answered once it formed -> a manual start finds
// it formed, and the member reports the head's hold -> a completion through
// the gateway returns text -> the member's measured usage is stored on the
// head -> stop.
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

// The agent's idle timeout for a cluster member process nothing claims.
const MEMBER_IDLE_TIMEOUT_MS = 60000;

// How long the gateway lets a head rest after it lost a member.
const CLUSTER_COOLDOWN_MS = 60000;

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
  cluster?: { launched: string[] };
  last_error?: { code?: string; message?: string };
};

type ClusterStatus = {
  state: string;
  phase?: string;
  members: { position: number; state: string; code?: string }[];
  last_failure?: { code: string; position?: number; head?: boolean };
  cooldown_until?: string;
};

type RuntimeSpec = {
  id: string;
  cluster_withheld_reason?: string;
  cluster_status?: ClusterStatus;
  cluster_members: { server_id: string; port: number; ram_measured_mb: number; measured_at?: string; stale: boolean }[];
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
  const alive = (a: ChildProcess) => a.exitCode === null && a.signalCode === null;
  for (const a of agents) if (alive(a)) a.kill("SIGTERM");
  // The agents drain-stop their children on SIGTERM; give them the moment.
  await Promise.all(
    agents.map(
      (a) =>
        new Promise<void>((resolve) => {
          if (!alive(a)) return resolve();
          a.once("exit", () => resolve());
          setTimeout(resolve, 10000);
        })
    )
  );
  // An agent that did not exit in time must not outlive the suite (it would
  // hold its router port for the next run).
  for (const a of agents) if (alive(a)) a.kill("SIGKILL");
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
  // A spawn failure (a missing or non-executable binary) is reported as an
  // event; unhandled it would crash the test runner instead of failing the
  // setup, whose next waits then time out with the log line below.
  child.on("error", (err) => fs.writeSync(log, `# e2e: spawning agent ${name} failed: ${String(err)}\n`));
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

/** A completion's answer: the text of a 200, the error code of a refusal. */
type Completion = { status: number; text: string; content: string; code: string };

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
  let code = "";
  if (resp.ok) {
    const body = JSON.parse(text) as { choices?: { message?: { content?: string } }[] };
    content = body.choices?.[0]?.message?.content ?? "";
  } else {
    try {
      code = (JSON.parse(text) as { error?: { code?: string } }).error?.code ?? "";
    } catch {
      // Not JSON; the caller reads text.
    }
  }
  return { status: resp.status, text, content, code };
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

    // The routing gate never dispatches to a head whose cluster has not
    // formed: the request parks (bounded by the head's startup timeout plus
    // its member's), its demand starts the cluster members first, and the
    // request is served by the formed cluster. Nothing else starts this head:
    // it is neither pinned nor kept up, and its agent refuses any start
    // without the gateway's cluster-start mark.
    test("a request for the cold head parks, the cluster forms, and the request is answered", async () => {
      const { mappingId, headSpecId } = mappings.get(sc.label)!;
      expect(await headState(headSpecId), "the head before the request").toMatch(/^(stopped|absent)$/);
      const c = await complete(sc.gatewayModel);
      expect(c.status, c.text).toBe(200);
      expect(c.content.trim(), "the parked request's completion text").not.toBe("");
      // The request was answered only once the cluster formed.
      expect((await runtimeSpec(mappingId)).cluster_status?.state).toBe("formed");
    });

    test("a manual start of the formed cluster keeps it, and the member reports the head's hold", async () => {
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

    // The member's own agent measures its resident RAM, and the gateway
    // stores it on the head's membership row: only samples inside the head's
    // run that show the member held by this head count, so a positive value
    // here was measured while the cluster served.
    test("the member's measured usage is stored on the head", async () => {
      const { mappingId } = mappings.get(sc.label)!;
      await expect
        .poll(async () => (await runtimeSpec(mappingId)).cluster_members[0].ram_measured_mb, { timeout: 30000, intervals: [1000] })
        .toBeGreaterThan(0);
      const member = (await runtimeSpec(mappingId)).cluster_members[0];
      expect(member.measured_at, "when the member's usage was measured").toBeTruthy();
      expect(member.stale, "a value measured on the current distribution").toBe(false);
    });

    test(`stop ${sc.stopMembers ? "with stop_members stops the member with the head" : "without stop_members releases the member, which idles out"}`, async () => {
      const { mappingId, headSpecId } = mappings.get(sc.label)!;
      await stopCluster(mappingId, sc.stopMembers);
      await expect
        .poll(async () => (await row(headServerId, (r) => r.spec_id === headSpecId))?.state ?? "absent", { timeout: 60000, intervals: [1000] })
        .toMatch(/^(stopped|absent)$/);
      await expect.poll(async () => (await runtimeSpec(mappingId)).cluster_status?.state, { timeout: 30000, intervals: [1000] }).toBe("cold");
      // The run formed, so its end replaces the stored usage by the run's
      // peak: still a measured value, not reset by the stop.
      expect((await runtimeSpec(mappingId)).cluster_members[0].ram_measured_mb, "the member's usage after the stop").toBeGreaterThan(0);
      // Every stop of the head releases its member: the hold and the start
      // reservation end together. stop_members then stops the member right
      // away. Without it the member runs on unclaimed, and its idle timeout
      // counts from the release, not from the end of the reservation the
      // members-first start armed (the head's startup bound plus the
      // orchestration's margins, two minutes here).
      let timeout = 60000;
      if (!sc.stopMembers) {
        await expect
          .poll(
            async () => {
              const m = await row(memberServerId, memberRowOf(headSpecId));
              return m === undefined || ((m.rpc?.held_by ?? "") === "" && (m.rpc?.reserved_until ?? "") === "");
            },
            { timeout: 30000, intervals: [1000] }
          )
          .toBe(true);
        timeout = MEMBER_IDLE_TIMEOUT_MS + 30000;
      }
      await expect
        .poll(async () => (await row(memberServerId, memberRowOf(headSpecId)))?.state ?? "absent", { timeout, intervals: [2000] })
        .toMatch(/^(stopped|absent)$/);
    });
  });
}

test.describe("(c) member process killed", () => {
  // The member process the test kills, so the recovery can tell a new one.
  let killedPid = 0;

  test("a formed cluster loses its member process", async () => {
    // The direct cluster of (b) again; its head is cold since (b)'s stop.
    const { mappingId, headSpecId } = mappings.get(DIRECT.label)!;
    await startCluster(mappingId);
    await awaitFormed(mappingId);
    // The status stream lags the formed state by a telemetry frame, and until
    // it catches up it may still show (b)'s stopped member with its old pid.
    // Only a running member holding this head names the process to kill, and
    // the watchdog needs that frame too: a loss is evidence only against a
    // frame that showed the member held.
    await expect
      .poll(
        async () => {
          const m = await row(memberServerId, memberRowOf(headSpecId));
          killedPid = m?.state === "running" && m.rpc?.held_by === headSpecId ? (m.pid ?? 0) : 0;
          return killedPid;
        },
        { timeout: 30000, intervals: [1000] }
      )
      .toBeGreaterThan(0);
    await expect.poll(async () => headState(headSpecId), { timeout: 30000, intervals: [1000] }).toBe("running");
    process.kill(killedPid, "SIGKILL");
  });

  // The head itself notices a lost member only when it next talks to it. The
  // gateway's watchdog does not wait for that: the member's exit is a
  // suspicion, which the next frame of the head's server -- the head still
  // running with the same process -- confirms. The head is stopped, the
  // member recorded as the cause, and the head cools down.
  test("the watchdog stops the head without a request, and the member is recorded as the cause", async () => {
    const { mappingId, headSpecId } = mappings.get(DIRECT.label)!;
    await expect
      .poll(async () => headState(headSpecId), { timeout: 60000, intervals: [1000] })
      .not.toMatch(/^(running|starting|draining)$/);
    const st = (await runtimeSpec(mappingId)).cluster_status!;
    expect(st.state, JSON.stringify(st)).toBe("cooling_down");
    expect(st.last_failure, JSON.stringify(st)).toMatchObject({ code: "runtime.cluster_member_lost", position: 0 });
    expect(st.members, JSON.stringify(st)).toEqual([{ position: 0, state: "failed", code: "runtime.cluster_member_lost" }]);
    expect(Date.parse(st.cooldown_until ?? ""), "the end of the cooldown").toBeGreaterThan(Date.now());
  });

  // While the head cools down the routing gate excludes it, so the request
  // never reaches the head's agent, and nothing starts the head for it: the
  // gateway answers at once, 503, with the cluster's stable code, and the
  // message names the failure the cluster recorded and where.
  test("a request during the cooldown is refused with the cluster's code, and the head stays down", async () => {
    const { headSpecId } = mappings.get(DIRECT.label)!;
    const c = await complete(DIRECT.gatewayModel);
    expect(c.status, c.text).toBe(503);
    expect(c.code, c.text).toBe("runtime.cluster_start_failed");
    const message = (JSON.parse(c.text) as { error: { message: string } }).error.message;
    expect(message).toContain("lost a member");
    expect(message).toContain("(cause: runtime.cluster_member_lost at member 0)");
    expect(await headState(headSpecId)).not.toMatch(/^(running|starting|draining)$/);
  });

  // The cooldown ends the refusals, not the cluster: the head is cold again,
  // and the next request parks while the gateway starts the cluster members
  // first -- a new member process, then the head.
  test("once the cooldown ended, a request forms the cluster again with a new member process", async () => {
    const { mappingId, headSpecId } = mappings.get(DIRECT.label)!;
    await expect
      .poll(async () => (await runtimeSpec(mappingId)).cluster_status?.state, { timeout: CLUSTER_COOLDOWN_MS + 30000, intervals: [2000] })
      .toBe("cold");
    const c = await complete(DIRECT.gatewayModel);
    expect(c.status, c.text).toBe(200);
    expect(c.content.trim(), "the completion text after the recovery").not.toBe("");
    const st = (await runtimeSpec(mappingId)).cluster_status!;
    expect(st.state, JSON.stringify(st)).toBe("formed");
    expect(st.members, JSON.stringify(st)).toEqual([{ position: 0, state: "held" }]);
    await expect
      .poll(
        async () => {
          const m = await row(memberServerId, memberRowOf(headSpecId));
          return m?.state === "running" && m.rpc?.held_by === headSpecId ? (m.pid ?? 0) : 0;
        },
        { timeout: 30000, intervals: [1000] }
      )
      .not.toBe(0);
    const member = (await row(memberServerId, memberRowOf(headSpecId)))!;
    expect(member.pid, "the member process after the recovery").not.toBe(killedPid);
  });

  test("cleanup: the head is stopped with its members", async () => {
    const { mappingId } = mappings.get(DIRECT.label)!;
    await stopCluster(mappingId, true);
  });
});
