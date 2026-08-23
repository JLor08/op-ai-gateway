// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

const t = messages.de;
const GW = "http://127.0.0.1:8091";
const SERVER = "mock-server"; // seeded in memory mode (cmd/gateway seedDefaultServer)
const AGENT = "dev-agent-secret"; // seeded agent-token secret bound to mock-server on loopback

// A rich telemetry body shaped like what the ServerAgent posts: legacy scalar
// fields PLUS the new host object and gpus array. server_id is intentionally
// omitted — the intake derives the target from the agent token.
function richBody(cpuUtil: number, gpuName: string, gpuTemp: number): string {
  return JSON.stringify({
    agent_version: "e2e",
    reported_at: new Date().toISOString(),
    os: "linux",
    arch: "amd64",
    cpu_load: cpuUtil / 100,
    active_requests: 3,
    queue_depth: 1,
    latency_ms: 50,
    error_rate: 0,
    provider_health: {},
    capabilities: {},
    host: {
      cpu_util_pct: cpuUtil,
      mem_used_bytes: 8_000_000_000,
      mem_total_bytes: 16_000_000_000,
      swap_used_bytes: 0,
      swap_total_bytes: 0,
      load1: 1.5,
      load5: 1.2,
      load15: 1.0,
      net: [{ name: "eth0", rx_bytes: 1000, tx_bytes: 2000 }]
    },
    gpus: [
      {
        index: 0,
        name: gpuName,
        uuid: "gpu-uuid-0",
        util_pct: 88,
        mem_used_bytes: 12_000_000_000,
        mem_total_bytes: 24_000_000_000,
        temp_c: gpuTemp,
        vram_temp_c: gpuTemp + 5,
        power_w: 320.5,
        fan_pct: 60
      }
    ]
  });
}

// A ServerAgent-style rich telemetry POST is persisted and readable back through
// the portal history endpoint with the full host/GPU/net structure intact.
test("agent telemetry is persisted and surfaces in the portal perf history", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  // Post a rich sample as the agent (Bearer token; no CSRF on the machine endpoint).
  const posted = await page.request.post(`${GW}/api/agent/v1/telemetry`, {
    headers: { Authorization: `Bearer ${AGENT}`, "Content-Type": "application/json" },
    data: richBody(42, "Tesla T4", 71)
  });
  expect(posted.status()).toBe(200);
  expect((await posted.json()).accepted).toBe(true);

  // Read it back through the owner/admin-gated history endpoint (dev is system_admin).
  const hist = await page.request.get(`${GW}/api/portal/servers/${SERVER}/perf?window=15m`);
  expect(hist.status()).toBe(200);
  const body = await hist.json();
  expect(Array.isArray(body.points)).toBe(true);
  expect(typeof body.from).toBe("string");
  expect(typeof body.to).toBe("string");

  // Our sample is present with the nested GPU/host structure round-tripped.
  const mine = body.points.find((p: any) => p.cpu_util_pct === 42);
  expect(mine, "the posted cpu_util_pct=42 sample should be in the window").toBeTruthy();
  expect(mine.mem_total_bytes).toBe(16_000_000_000);
  expect(mine.load1).toBe(1.5);
  expect(Array.isArray(mine.gpus)).toBe(true);
  expect(mine.gpus[0].name).toBe("Tesla T4");
  expect(mine.gpus[0].temp_c).toBe(71);
  expect(mine.gpus[0].uuid).toBe("gpu-uuid-0");
  expect(Array.isArray(mine.net)).toBe(true);
  expect(mine.net[0].rx_bytes).toBe(1000);
});

// A non-existent server id is a 404 (no existence leak) — the same authz shape
// that guards a non-owner (unit-tested) also covers unknown ids.
test("perf history for an unknown server is 404", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  const resp = await page.request.get(`${GW}/api/portal/servers/does-not-exist/perf?window=15m`);
  expect(resp.status()).toBe(404);
});

// The live SSE stream delivers a snapshot then fans out a freshly-posted sample.
// Driven entirely in-browser (same-origin via the /api preview proxy) so the
// session cookie authenticates the EventSource and the fetch.
test("a live telemetry sample fans out over the perf SSE stream", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  const sample = await page.evaluate(async (server) => {
    const es = new EventSource(`/api/portal/servers/${server}/perf/events`);
    // Wait for the initial snapshot — proves the subscriber is attached before
    // we publish (no publish-before-subscribe race).
    await new Promise<void>((resolve, reject) => {
      es.addEventListener("snapshot", () => resolve(), { once: true });
      es.addEventListener("error", () => reject(new Error("SSE error before snapshot")));
      setTimeout(() => reject(new Error("no snapshot within 8s")), 8000);
    });

    const samplePromise = new Promise<any>((resolve, reject) => {
      es.addEventListener("sample", (e: MessageEvent) => resolve(JSON.parse(e.data)), { once: true });
      setTimeout(() => reject(new Error("no sample within 8s")), 8000);
    });

    // Publish a fresh sample as the agent, now that we are subscribed.
    const r = await fetch("/api/agent/v1/telemetry", {
      method: "POST",
      headers: { Authorization: "Bearer dev-agent-secret", "Content-Type": "application/json" },
      body: JSON.stringify({
        reported_at: new Date().toISOString(),
        cpu_load: 0.77,
        active_requests: 2,
        queue_depth: 0,
        provider_health: {},
        capabilities: {},
        host: { cpu_util_pct: 77, mem_used_bytes: 1, mem_total_bytes: 2 },
        gpus: [{ index: 0, name: "RTX 4090", util_pct: 55, temp_c: 66 }]
      })
    });
    if (!r.ok) throw new Error("telemetry post failed: " + r.status);

    const s = await samplePromise;
    es.close();
    return s;
  }, SERVER);

  expect(sample.cpu_util_pct).toBe(77);
  expect(sample.gpus[0].name).toBe("RTX 4090");
  expect(sample.gpus[0].util_pct).toBe(55);
});

// End-to-end proof of the Phase C Performance view: the REAL server-agent binary
// (with a fake nvidia-smi on its PATH) POSTs live telemetry, and the portal
// Performance sub-view renders a live per-server graph from it. Isolated in its
// own describe so the agent spawns only AFTER the exact-sample SSE test above —
// a stray agent sample must not race the manually-posted cpu_util_pct=77 frame.
test.describe("live agent Performance tab", () => {
  const AGENT_BIN = "/tmp/op-ai-server-agent-perf";
  let child: ChildProcess | undefined;
  let fakeBinDir = "";

  test.beforeAll(() => {
    // Build the real agent binary from its own module (sibling of gateway/).
    execSync(`go build -o ${AGENT_BIN} .`, { cwd: "../../server-agent", stdio: "inherit" });

    // A fake `nvidia-smi` on PATH echoing a canned 9-field 1-GPU CSV row in the
    // Task 4 --query-gpu column order (index,name,uuid,util,mem_used,mem_total,
    // temp,power,fan). A short row (<9 fields) would be skipped by parseNvidiaCSV.
    fakeBinDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-agent-perf-fakebin-"));
    const script = path.join(fakeBinDir, "nvidia-smi");
    fs.writeFileSync(script, '#!/bin/sh\necho "0, E2E-PERF-GPU, GPU-e2e-0, 63, 8000, 16000, 58, 90, 35"\n');
    fs.chmodSync(script, 0o755);

    // Spawn the agent: the fake nvidia-smi first on PATH makes NVIDIA.Available()
    // true; the token identifies mock-server on the gateway side. The gateway
    // webServer is already healthy (config waits on /healthz before any test).
    child = spawn(AGENT_BIN, [], {
      env: {
        ...process.env,
        PATH: `${fakeBinDir}:${process.env.PATH}`,
        OP_AGENT_GATEWAY_URL: GW,
        OP_AGENT_TOKEN: AGENT,
        OP_AGENT_INTERVAL: "500ms"
      },
      stdio: "inherit"
    });
  });

  test.afterAll(() => {
    child?.kill("SIGTERM");
    if (fakeBinDir) fs.rmSync(fakeBinDir, { recursive: true, force: true });
  });

  test("the Performance tab renders a live graph from the running agent", async ({ page }) => {
    await login(page, "dev@example.test", "dev-secret");

    // Navigate to the AI-Servers view and open the mock-server row's Leistung
    // action. The five row actions render inline (icons), so it is a button whose
    // accessible name is t.serverPerformance; only one server is seeded.
    await page.getByRole("link", { name: t.servers }).click();
    await page.getByRole("button", { name: t.serverPerformance }).click();

    // The sub-view breadcrumb + panel carry the seeded server name.
    await expect(page.getByText("Mock Server").first()).toBeVisible();

    // Agent-gated: poll the history until the RUNNING agent's DISTINCTIVE fake
    // GPU ("E2E-PERF-GPU", produced only by this test's fake nvidia-smi) appears.
    // The earlier specs in this suite pushed Tesla T4 / RTX 4090 samples to the
    // same mock-server, so a generic "some GPU / point-count > 0 / overlay"
    // assertion would pass on that RESIDUAL data even if the agent never ran.
    // Asserting the distinctive name is what actually proves the live agent.
    await expect
      .poll(
        async () => {
          const r = await page.request.get(`${GW}/api/portal/servers/${SERVER}/perf?window=15m`);
          if (!r.ok()) return false;
          const b = await r.json();
          return Boolean(b.points?.some((p: any) => p.gpus?.some((g: any) => g.name === "E2E-PERF-GPU")));
        },
        { timeout: 20000, intervals: [500] }
      )
      .toBeTruthy();

    // The UI reflects the live agent data: no "no agent reporting" empty state,
    // the GPU-util chart renders (only when the latest point carries GPUs), and a
    // non-zero series shows the interactive overlay.
    await expect(page.getByText(t.serverPerfNoAgent)).toHaveCount(0);
    await expect(page.getByRole("img", { name: t.serverPerfGpuUtil })).toBeVisible();
    await expect(page.getByTestId("ts-overlay").first()).toBeVisible({ timeout: 20000 });
    // The per-core CPU chart renders too — the real gopsutil host reports per-core.
    await expect(page.getByRole("img", { name: t.serverPerfCpuCores })).toBeVisible();
  });
});
