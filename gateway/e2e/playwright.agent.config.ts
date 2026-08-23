// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:agent — live proof of the Phase B ServerAgent binary. This suite builds
// and spawns the REAL agent (`server-agent/`), pointing a fake `nvidia-smi` on
// its PATH at a canned 1-GPU CSV row, and proves that the agent's cross-process
// telemetry POST to /api/agent/v1/telemetry surfaces in the gateway's portal
// perf history (GET /api/portal/servers/mock-server/perf). Pure API — no
// frontend build and no `use.baseURL`; every request is an absolute
// http://127.0.0.1:8091 URL and the auth flow uses the `request` fixture's
// persisted session cookie (loopback → non-Secure cookie, stored + resent).
//
// Memory mode on loopback, so the seeded "mock-server" carries the dev agent
// token secret "dev-agent-secret" (cmd/gateway seedDefaultServer +
// gatewayDevAgentSecret) and the dev user "dev@example.test"/"dev-secret" is a
// system_admin with totp_mode=off. Port 8091 like the base config; the suites
// never run concurrently. Fresh server each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-agent-e2e";

export default defineConfig({
  testDir: "./e2e-agent",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
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
    }
  ]
});
