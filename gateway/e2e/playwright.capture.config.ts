// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// SP-C capture e2e. Capture is SQLite-only (the Cipher is wired only in
// sqliteDeps), so this SEPARATE config runs the gateway in sqlite mode with a
// capture encryption key plus the Part-A bootstrap admin/password — the base
// playwright.config.ts (memory mode, 12 specs) is left untouched.
//
// Ports 8091 (gateway) + 4173 (frontend preview) are the same as the base
// config; the two suites never run concurrently. reuseExistingServer is false so
// every run starts a fresh gateway. The sqlite file is deleted before the gateway
// starts (rm -f in the command) so bootstrap re-seeds a login-able admin each run.

// Known test-only values (never used in production). The bootstrap password must
// satisfy the >= 10 char policy in internal/auth.
export const CAPTURE_ADMIN_EMAIL = "capture-admin@example.test";
export const CAPTURE_ADMIN_PASSWORD = "Capture-E2E-Pass-1";

const SQLITE_PATH = "/tmp/op-ai-gateway-capture-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-capture-e2e";
// 64 hex chars = 32 bytes (AES-256).
const CAPTURE_KEY = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const BOOTSTRAP_API_TOKEN = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210";

export default defineConfig({
  testDir: "./e2e-capture",
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
        OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY: CAPTURE_KEY,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: CAPTURE_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: "Capture Admin",
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: CAPTURE_ADMIN_PASSWORD,
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
