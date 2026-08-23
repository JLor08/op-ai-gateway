// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live proof of the headline server-health behaviour: with
// OP_AI_GATEWAY_MOCK_UNREACHABLE=true the seeded mock application's probe (and its
// single retry) fails, so the mock server derives to Unavailable and the gateway
// offers only reachable applications' models — i.e. the mock's models disappear
// from the Modelle list. Memory mode; separate config so the base suite is not
// affected by the forced-unreachable seam. Ports 8091/4173 like the base config;
// the suites never run concurrently. Fresh servers each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-health-e2e";

export default defineConfig({
  testDir: "./e2e-health",
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
        OP_AI_GATEWAY_MOCK_UNREACHABLE: "true",
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
