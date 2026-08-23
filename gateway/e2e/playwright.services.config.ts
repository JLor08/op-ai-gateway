// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the Service Accounts (Phase 1) security
// guarantees AND, additively, the admin-group permissions Phase C feature
// (spec: docs/superpowers/specs/2026-08-10-admin-group-permissions-phase-c-design.md):
// service<->admin-group linkage + the group-scoped `can_manage_services`
// co-manager flag. SQLITE-BACKED (not the default memory mode): Phase C's
// core guarantee (authorizeServiceRead/authorizeServiceSettings's group-
// intersection gate, containment via service_admin_groups rows) is only
// genuinely exercised once writes go through the sqlite store with
// foreign_keys=ON -- mirrors playwright.servers.config.ts exactly (its own
// group-scoped-SERVER analog). Switching this suite off memory mode is ALSO
// what fixes a real pre-existing break in the original two tests below: a
// bare memory-mode dev principal owns zero admin groups, so the (now
// Phase-B/C-mandatory) admin-group picker on both the create-service and
// invite-user forms stayed permanently disabled and neither test could ever
// get past its first click. The first test below now creates ONE admin
// group up front (under its own elevated system_admin) so both original
// flows keep working via plain auto-select (exactly one candidate -> no
// picker rendered at all) -- their bodies are otherwise unchanged.
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const SERVICES_ADMIN_EMAIL = "services-admin@example.test";
export const SERVICES_ADMIN_PASSWORD = "Services-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in services.spec.ts).
export const SERVICES_ADMIN_NAME = "Services Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-services-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-services-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "7777888899990000777788889999000077778888999900007777888899990000";

export default defineConfig({
  testDir: "./e2e-services",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The suite spans several principals (system_admin + admin A) across a
  // multi-step group + service setup -- comfortably past the 30s default.
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: SERVICES_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: SERVICES_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: SERVICES_ADMIN_PASSWORD,
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
