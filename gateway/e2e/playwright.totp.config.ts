// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:totp — proves TOTP 2FA end to end against the dev/memory gateway. No
// capture key is configured, so the store is volatile and the TOTP secret is
// held in plaintext RAM (the plain: seal path) — no key required. totp_mode is
// configured at runtime via System Settings, exactly like the SMTP suite. TOTP
// codes are computed in-test (e2e-totp/totp.ts) from the base32 secret the
// enroll/login/set-password APIs return. Ports 8091/4173 like the base config
// (the suites never run concurrently). Fresh servers each run.
const GATEWAY_BIN = "/tmp/op-ai-gateway-totp-e2e";

export default defineConfig({
  testDir: "./e2e-totp",
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
