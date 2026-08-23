// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// SP-C+ RAM-fallback capture e2e. Same shape as playwright.capture.config.ts but
// with NO OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY: in SP-C+ the "sqlite driver
// without a key" path no longer disables capture — it falls back to the volatile
// in-RAM MemoryCaptureStore (gzip, unencrypted, KeyVersion 0, process-lifetime).
// This suite proves that RAM-captured communications are viewable + redacted, the
// substring model/server filter works, and a capture can be manually deleted
// (metadata stays). Captures never touch the sqlite file on this path — the
// on-disk-plaintext guarantee is separately proven by the Go test
// TestVerifyNoPlaintextCaptureOnDiskWithoutKey.
//
// Distinct sqlite file + binary from the sqlite+key suite; ports 8091/4173 are
// shared (the suites never run concurrently). reuseExistingServer:false + rm -f so
// every run starts a fresh, freshly-bootstrapped gateway.

// Known test-only values (never used in production). The bootstrap password must
// satisfy the >= 10 char policy in internal/auth.
export const CAPTURE_RAM_ADMIN_EMAIL = "capture-ram-admin@example.test";
export const CAPTURE_RAM_ADMIN_PASSWORD = "Capture-RAM-E2E-Pass-1";

const SQLITE_PATH = "/tmp/op-ai-gateway-capture-ram-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-capture-ram-e2e";
const BOOTSTRAP_API_TOKEN = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0";

export default defineConfig({
  testDir: "./e2e-capture-ram",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry"
  },
  webServer: [
    {
      command: `rm -f ${SQLITE_PATH} && go build -o ${GATEWAY_BIN} ./cmd/gateway && ${GATEWAY_BIN}`,
      cwd: "../backend",
      url: "http://127.0.0.1:8091/healthz",
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        OP_AI_GATEWAY_ADDR: "127.0.0.1:8091",
        OP_AI_GATEWAY_PUBLIC_URL: "http://127.0.0.1:4173/portal",
        OP_AI_GATEWAY_DB_DRIVER: "sqlite",
        OP_AI_GATEWAY_SQLITE_PATH: SQLITE_PATH,
        OP_AI_GATEWAY_AUTO_MIGRATE: "true",
        // Intentionally NO OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY -> RAM fallback.
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: CAPTURE_RAM_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: "Capture RAM Admin",
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: CAPTURE_RAM_ADMIN_PASSWORD,
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
