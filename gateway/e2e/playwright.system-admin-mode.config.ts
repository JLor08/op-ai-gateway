// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the System-Admin step-up mode feature: a
// system_admin logs in NOT elevated (System/NetBird/Logs nav hidden), enters
// "System-Admin mode" via password re-entry, sees the gated nav appear + an
// active indicator, then leaves and loses it again. Driven through the real
// UI against a REAL SQLITE-BACKED gateway (mirrors playwright.groups.config.ts /
// playwright.limits.config.ts rather than the memory-mode suites) -- not
// strictly required for this scenario (no FK/cascade involved), but keeps the
// suite consistent with its sibling admin-focused suites and lets a bootstrap
// system_admin log in against a real store.
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const SAMODE_ADMIN_EMAIL = "samode-admin@example.test";
export const SAMODE_ADMIN_PASSWORD = "SAMode-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in system-admin-mode.spec.ts).
export const SAMODE_ADMIN_NAME = "SAMode Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-samode-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-samode-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token (verified
// 2026-08-13 via a grep across every playwright.*.config.ts BOOTSTRAP_API_TOKEN
// + CAPTURE_ENCRYPTION_KEY/CAPTURE_KEY value -- this suite's old value was
// byte-identical to playwright.resource-groups.config.ts's, F4.4).
const BOOTSTRAP_API_TOKEN = "86ce04cabe796dae942ebd3e50aaff43f4ec766f5ba14b9a2ada95667b4ad947";

export default defineConfig({
  testDir: "./e2e-system-admin-mode",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  timeout: 60000,
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: SAMODE_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: SAMODE_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: SAMODE_ADMIN_PASSWORD,
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
