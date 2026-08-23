// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the Benutzergruppen (user groups) feature
// (spec: docs/superpowers/specs/2026-08-08-user-groups-design.md), driven
// through the real UI against a REAL SQLITE-BACKED gateway -- NOT the default
// memory mode. Memory mode does not enforce FK/cascade constraints and its
// group store is the same in-memory mirror either way, but the design's core
// guarantees (containment via real membership rows, cascade-on-remove, owner
// succession persisted across requests) are only genuinely exercised once
// writes go through the sqlite store with foreign_keys=ON -- mirrors
// playwright.limits.config.ts / playwright.capture.config.ts (also sqlite +
// a bootstrap admin) rather than playwright.health.config.ts (memory mode).
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const GROUPS_ADMIN_EMAIL = "groups-admin@example.test";
export const GROUPS_ADMIN_PASSWORD = "Groups-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in groups.spec.ts).
export const GROUPS_ADMIN_NAME = "Groups Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-groups-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-groups-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "5555666677778888555566667777888855556666777788885555666677778888";

export default defineConfig({
  testDir: "./e2e-groups",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The scenario spans four distinct principals (system_admin + 2 admins +
  // 2 users), each redeeming an invite in its own browser context, plus a
  // multi-step group hierarchy build-out -- comfortably past the 30s default.
  timeout: 120000,
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: GROUPS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: GROUPS_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: GROUPS_ADMIN_PASSWORD,
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
