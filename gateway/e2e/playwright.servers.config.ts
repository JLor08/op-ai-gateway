// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the admin-group permissions Phase B feature
// (spec: docs/superpowers/specs/2026-08-10-admin-group-permissions-phase-b-design.md):
// server<->admin-group linkage + the group-scoped `can_manage_servers`
// co-manager flag. Cloned from playwright.groups.config.ts -- driven through
// the real UI against a REAL SQLITE-BACKED gateway (not the default memory
// mode), since the design's core guarantee (authorizeServer's group-
// intersection gate, containment via server_admin_groups rows) is only
// genuinely exercised once writes go through the sqlite store with
// foreign_keys=ON.
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const SERVERS_ADMIN_EMAIL = "servers-admin@example.test";
export const SERVERS_ADMIN_PASSWORD = "Servers-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in servers.spec.ts).
export const SERVERS_ADMIN_NAME = "Servers Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-servers-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-servers-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "3333444455556666333344445555666633334444555566663333444455556666";

export default defineConfig({
  testDir: "./e2e-servers",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The scenario spans two principals (system_admin + admin A) across a
  // multi-step group + server setup -- comfortably past the 30s default.
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: SERVERS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: SERVERS_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: SERVERS_ADMIN_PASSWORD,
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
