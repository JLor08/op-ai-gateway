// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the Projekte (Projects) feature (spec:
// docs/superpowers/specs/2026-08-08-projects-design.md), driven through the
// real UI (+ a few raw API/bearer calls) against a REAL SQLITE-BACKED
// gateway -- NOT the default memory mode. Mirrors playwright.groups.config.ts
// / playwright.limits.config.ts (also sqlite + a bootstrap admin) rather than
// playwright.health.config.ts (memory mode), for the same two reasons those
// suites are sqlite-backed:
//   1. Memory mode does not enforce FK/cascade constraints -- the project's
//      membership/group tables (project_members/project_groups, FK-cascade
//      on delete) and the eligibility check backing the token-assign 403
//      (portal.ErrProjectNotMember) are only genuinely exercised once writes
//      go through the sqlite store with foreign_keys=ON.
//   2. The project-scope widening this suite proves (design spec §8 --
//      Service.applyUsageScope's ProjectIDs IN-list) reads real persisted
//      usage_events rows via the store's UsageGroups/matchUsage path; the
//      in-memory usage.Recorder used by driver=memory would make the
//      cross-member "B sees the aggregate, a non-member sees zero rows"
//      assertion far weaker (see
//      [[memory-mode-e2e-misses-fk-and-usage-store-bugs]]).
// seedDefaultServerIfEmpty seeds the SAME mock server/app/mappings on a fresh
// sqlite DB as memory mode does (qwen-coder / gpt-oss-20b), so the login-able
// bootstrap admin + mock model routing work identically.
//
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run (rm -f before the gateway
// starts, so bootstrap re-seeds a login-able system_admin each run).

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const PROJECTS_ADMIN_EMAIL = "projects-admin@example.test";
export const PROJECTS_ADMIN_PASSWORD = "Projects-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in projects.spec.ts).
export const PROJECTS_ADMIN_NAME = "Projects Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-projects-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-projects-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "9999888877776666999988887777666699998888777766669999888877776666";

export default defineConfig({
  testDir: "./e2e-projects",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The scenario spans four distinct principals (a system_admin owner + 3
  // invited users, each redeeming an invite in its own browser context),
  // plus group creation, project CRUD/membership, a token round-trip, a real
  // inference call, and several raw usage/groups API assertions -- well past
  // the 30s default for a single test.
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: PROJECTS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: PROJECTS_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: PROJECTS_ADMIN_PASSWORD,
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
