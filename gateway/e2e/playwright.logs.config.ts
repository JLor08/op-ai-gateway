// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:logs — live proof of the system-admin Logs view (in-memory gateway log ring
// + SSE fan-out + runtime log-level switch). Memory mode on loopback: the dev user
// "dev@example.test"/"dev-secret" is a system_admin (totp_mode=off) so the gated
// bottom "Logs" nav item is visible, and the seeded "mock-server" carries the dev
// agent token secret "dev-agent-secret" (cmd/gateway seedDefaultServer +
// gatewayDevAgentSecret) so a valid agent telemetry POST drives a real gateway log
// line. Ports 8091/4173 like the base config; the suites never run concurrently.
// Fresh servers each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-logs-e2e";

export default defineConfig({
  testDir: "./e2e-logs",
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
