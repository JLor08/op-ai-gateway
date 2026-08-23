// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { expect, test } from "@playwright/test";

// Live proof: the REAL server-agent binary collects metrics (host + a fake
// nvidia-smi GPU) and POSTs them to the gateway, and the sample surfaces in the
// portal perf history. Pure API — all URLs absolute, auth via the `request`
// fixture's persisted session cookie.
const GW = "http://127.0.0.1:8091";
const SERVER = "mock-server"; // seeded in memory mode (cmd/gateway seedDefaultServer)
const AGENT_BIN = "/tmp/op-ai-server-agent";

let child: ChildProcess | undefined;
let fakeBinDir = "";
// Phase 2 certificate distribution defaults to OFF. This suite sets
// OP_AGENT_CERT_DIR but deliberately leaves cert_mode UNSET, and asserts the
// directory stays empty — the only way this suite actually proves the default
// installs nothing (an agent with cert_mode=off must never even ask the gateway).
let certDir = "";

test.beforeAll(() => {
  // Build the real agent binary from its own module.
  execSync(`go build -o ${AGENT_BIN} .`, { cwd: "../../server-agent", stdio: "inherit" });

  // A fake `nvidia-smi` on PATH that echoes a canned 9-field 1-GPU CSV row
  // exactly matching the Task 4 --query-gpu column order
  // (index,name,uuid,util,mem_used,mem_total,temp,power,fan). A short row (<9
  // fields) would be skipped by parseNvidiaCSV's guard → the poll would hang.
  fakeBinDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-agent-fakebin-"));
  const script = path.join(fakeBinDir, "nvidia-smi");
  fs.writeFileSync(script, '#!/bin/sh\necho "0, E2E-GPU, GPU-e2e-0, 77, 8000, 16000, 60, 100, 40"\n');
  fs.chmodSync(script, 0o755);

  certDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-agent-certdir-"));
});

test.afterAll(() => {
  child?.kill("SIGTERM");
  if (fakeBinDir) fs.rmSync(fakeBinDir, { recursive: true, force: true });
  if (certDir) fs.rmSync(certDir, { recursive: true, force: true });
});

test("the real agent binary pushes host + fake-nvidia GPU telemetry to the gateway", async ({ request }) => {
  // Spawn the agent with the fake nvidia-smi first on PATH so NVIDIA.Available()
  // is true; the token identifies mock-server on the gateway side.
  child = spawn(AGENT_BIN, [], {
    env: {
      ...process.env,
      PATH: `${fakeBinDir}:${process.env.PATH}`,
      OP_AGENT_GATEWAY_URL: GW,
      OP_AGENT_TOKEN: "dev-agent-secret",
      OP_AGENT_INTERVAL: "500ms",
      // A writable cert_dir but NO OP_AGENT_CERT_MODE: the default (off) must
      // leave it completely untouched — see the assertion at the end.
      OP_AGENT_CERT_DIR: certDir
    },
    stdio: "inherit"
  });

  // Obtain an admin session; the `request` fixture persists the Set-Cookie for
  // the subsequent history reads. The dev user is a system_admin (totp off).
  const loginResp = await request.post(`${GW}/api/auth/login`, {
    headers: { "X-OP-CSRF": "1" },
    data: { email: "dev@example.test", password: "dev-secret" }
  });
  expect(loginResp.ok(), "admin login should succeed").toBeTruthy();

  // Step up into System-Admin mode. A fresh system_admin session is NOT elevated
  // (2026-08-10 step-up feature), and without the `system` scope
  // authorizeServer() falls through to the owner/admin-group branches — the
  // seeded "mock-server" has neither, so the perf read below 404s
  // ("server.not_found") and the poll can never succeed. Every sibling suite that
  // touches a seeded server does this same step-up; this one predates it.
  const elevateResp = await request.post(`${GW}/api/portal/system-admin-mode`, {
    headers: { "X-OP-CSRF": "1" },
    data: { password: "dev-secret" }
  });
  expect(elevateResp.ok(), "entering system-admin mode should succeed").toBeTruthy();

  // Poll the portal history until the agent's fake-NVIDIA sample lands. On darwin
  // the detection order is nvidia→apple and the fake nvidia-smi makes NVIDIA
  // Available() true, so gpus[0] is the fake NVIDIA GPU.
  await expect
    .poll(
      async () => {
        const r = await request.get(`${GW}/api/portal/servers/${SERVER}/perf?window=15m`);
        if (!r.ok()) return false;
        const b = await r.json();
        return Boolean(b.points?.some((p: any) => p.gpus?.[0]?.name === "E2E-GPU" && p.gpus[0].util_pct === 77));
      },
      { timeout: 20000, intervals: [500] }
    )
    .toBeTruthy();

  // Fetch once more and assert the matching point also carries real host data
  // collected by gopsutil (not just the fake GPU).
  const hist = await request.get(`${GW}/api/portal/servers/${SERVER}/perf?window=15m`);
  expect(hist.ok()).toBeTruthy();
  const body = await hist.json();
  const mine = body.points.find((p: any) => p.gpus?.[0]?.name === "E2E-GPU" && p.gpus[0].util_pct === 77);
  expect(mine, "the fake-NVIDIA sample should be present").toBeTruthy();
  expect(mine.mem_total_bytes).toBeGreaterThan(0);
  expect(mine.cpu_util_pct).toBeGreaterThanOrEqual(0);

  // Certificate distribution is opt-in: with cert_mode unset (= "off") the agent
  // must never fetch, never write, and never even create anything in cert_dir —
  // even though a perfectly usable OP_AGENT_CERT_DIR was handed to it. By now the
  // agent has been running for many collect cycles (and past its startup sync
  // trigger, which the off-mode guard short-circuits), so an empty directory here
  // is a real observation, not a race.
  expect(fs.readdirSync(certDir), "cert_mode defaults to off: nothing may be installed").toEqual([]);
});
