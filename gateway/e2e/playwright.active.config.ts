// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live "running connections" (in-flight) e2e. Memory mode with the default-off
// mock delay turned ON (OP_AI_GATEWAY_MOCK_DELAY_MS=2500) so a single inference
// request stays in-flight long enough to observe it in the "Laufende
// Verbindungen" panel and then watch it clear. Separate config so the base
// suite (12 specs) is not slowed by the delay. Ports 8091/4173 like the base
// config; the suites never run concurrently. Fresh servers each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-active-e2e";

export default defineConfig({
  testDir: "./e2e-active",
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
        OP_AI_GATEWAY_MOCK_DELAY_MS: "2500",
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
