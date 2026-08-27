// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:runtime — the agent-managed model runtime, end to end, ACROSS PROCESS
// BOUNDARIES. Every other suite on this feature is in-process; this one is the
// only proof of the whole circle:
//
//   portal UI -> gateway DB -> GET /api/agent/v1/runtime-config
//     -> the REAL server-agent binary -> exec() of a REAL child process
//     -> the agent's router port -> that child's HTTP server
//     -> back up as telemetry -> the portal's runtime SSE stream.
//
// Shape: cloned from playwright.certificates.config.ts, which is this repo's
// precedent for "portal UI tests PLUS an external Go binary" (gateway +
// `npm run build && npm run preview` + a separately built helper program).
// Deliberately NOT playwright.agent.config.ts's pure-API shape: the runtime
// admin (spec form, co-residency matrix, VRAM budgets, live-status badges,
// force start/stop) is a portal UI area, so the frontend preview is required.
//
// THE ONE STRUCTURAL POINT, and the reason this suite exists: the stub model
// server (e2e-runtime/fixtures/stubserver) is NOT a `webServer` entry here,
// unlike e2e-certificates' fakeacme. It is BUILT by runtime.spec.ts's
// beforeAll and never started by the harness. Starting it is the AGENT's job,
// and that is precisely what is under test — a `webServer` entry for it would
// silently hollow out the entire suite while every assertion still passed.
//
// The real server-agent binary is likewise built and spawned from the spec
// (mirroring e2e-agent/agent.spec.ts), not from here: it needs the agent token
// that the spec mints for the AI server it creates, so it cannot exist before
// the test body runs.
//
// Memory mode on 127.0.0.1:8091 like every other suite (fresh gateway each
// run; suites never run concurrently, so the shared port is fine). Memory mode
// seeds the dev user dev@example.test / dev-secret as a system_admin with
// totp_mode=off, and the dev bearer token `dev-secret` for the inference
// calls. It does NOT seed any user groups (the migration-v44 "Standard"
// system/admin pair is a SQL-driver artifact), so the suite creates the one
// system + one admin group that CreateServer's mandatory admin_group_ids gate
// requires.

// The agent's router port, and therefore the `server_agent` application's
// Port: AgentRuntimeConfig derives router_listen from exactly that column
// (portal/service_runtime.go), and the gateway reaches the application at
// server.Domain:app.Port (routing.ApplicationEndpoint). Deliberately far from
// the 809x band the gateway/mesh listeners of the other suites use.
export const RUNTIME_ROUTER_PORT = 18081;

// Absolute so it can equally be a `-o` build target, the agent's
// OP_AGENT_RUNTIME_ALLOWED_BINARIES entry, and the runtime spec's `binary`
// field — the three places that must name byte-for-byte the same path for the
// agent's allowlist (LocalPolicy.Permit does an exact match) to permit it.
export const RUNTIME_AGENT_BIN = "/tmp/op-e2e-runtime-agent";
export const RUNTIME_STUB_BIN = "/tmp/op-e2e-runtime-stubserver";

const GATEWAY_BIN = "/tmp/op-ai-gateway-runtime-e2e";

export default defineConfig({
  testDir: "./e2e-runtime",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // This suite performs a dozen REAL cold model starts (each one a process
  // exec plus a deliberate 2s readiness delay so the `starting` state is
  // observable rather than raced past), several gateway app-health probe
  // cycles, and two eviction rounds — comfortably past the 30s default.
  timeout: 300000,
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry"
  },
  webServer: [
    {
      command: `rm -f ${GATEWAY_BIN}; go build -o ${GATEWAY_BIN} ./cmd/gateway && ${GATEWAY_BIN}`,
      cwd: "../backend",
      url: "http://127.0.0.1:8091/healthz",
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        OP_AI_GATEWAY_ADDR: "127.0.0.1:8091",
        OP_AI_GATEWAY_PUBLIC_URL: "http://127.0.0.1:4173/portal",
        GOCACHE: "/private/tmp/op-ai-gateway-go-build-cache"
      }
    },
    {
      command: "npm run build && npm run preview",
      cwd: "../frontend",
      url: "http://127.0.0.1:4173/portal/",
      reuseExistingServer: false,
      timeout: 180000
    }
  ]
});
