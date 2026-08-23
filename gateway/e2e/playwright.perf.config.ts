// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live proof of Phase A (per-server performance telemetry spine). Memory mode on
// loopback, so the seeded "mock-server" carries the dev agent token secret
// "dev-agent-secret" (see cmd/gateway seedDefaultServer + gatewayDevAgentSecret).
// The suite drives the real cross-process wire: a ServerAgent-style POST of a
// rich host/GPU telemetry sample to /api/agent/v1/telemetry surfaces in the
// portal history read (GET /api/portal/servers/{id}/perf) and fans out live on
// the per-server SSE stream (/perf/events). Ports 8091/4173 like the base config;
// the suites never run concurrently. Fresh servers each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-perf-e2e";

export default defineConfig({
  testDir: "./e2e-perf",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
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
      timeout: 120000
    }
  ]
});
