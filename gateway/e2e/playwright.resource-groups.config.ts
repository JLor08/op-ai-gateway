// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of Resource Groups (Phase 1, spec:
// docs/superpowers/specs/2026-08-11-resource-groups-phase-1-design.md):
// resource-group<->admin-group linkage + the group-scoped `can_manage_resources`
// co-manager flag + server membership (the dual authorizeResourceGroup +
// authorizeServer gate + same-system-group containment). Cloned from
// playwright.servers.config.ts / playwright.services.config.ts -- driven
// through the real backend (partly UI, partly the raw JSON API -- see the
// spec's own doc comment) against a REAL SQLITE-BACKED gateway (not the
// default memory mode), since the feature's core guarantees (authorizeResourceGroup's
// group-intersection gate, the resource_group_admin_groups/resource_group_servers
// join tables, the FK cascades) are only genuinely exercised once writes go
// through the sqlite store with foreign_keys=ON.
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const RESOURCE_GROUPS_ADMIN_EMAIL = "resource-groups-admin@example.test";
export const RESOURCE_GROUPS_ADMIN_PASSWORD = "Resource-Groups-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in resource-groups.spec.ts).
export const RESOURCE_GROUPS_ADMIN_NAME = "Resource Groups Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-resource-groups-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-resource-groups-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "aaaa1111bbbb2222aaaa1111bbbb2222aaaa1111bbbb2222aaaa1111bbbb2222";

export default defineConfig({
  testDir: "./e2e-resource-groups",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The scenario spans two principals (system_admin + admin A) across a
  // multi-step group + server + resource-group setup -- comfortably past the
  // 30s default.
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: RESOURCE_GROUPS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: RESOURCE_GROUPS_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: RESOURCE_GROUPS_ADMIN_PASSWORD,
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
