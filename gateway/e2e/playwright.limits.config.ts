// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// Live end-to-end proof of the Principal Limits (Phase 2) enforcement guarantees:
// a configured rate limit / request quota / token quota actually blocks the
// SECOND request against a real gateway.
//
// Runs in SQLITE mode (not the default memory mode), for two reasons:
//   1. seedDefaultServerIfEmpty seeds the SAME mock server/app/mappings
//      (qwen-coder / gpt-oss-20b) on a fresh sqlite DB as it does in memory
//      mode, so the login-able bootstrap admin + mock model routing work
//      identically (see cmd/gateway/main.go bootstrapSQLite/seedDefaultServerIfEmpty).
//   2. request-quota/token-quota enforcement reads PrincipalLimiter.aggregate,
//      which calls store.UsageAggregateSince -- an HONEST NO-OP on the memory
//      driver (routing.MemoryStore.UsageAggregateSince always returns
//      0,0,0,nil; see internal/gateway/principal_limits.go's doc comment and
//      cmd/gateway/main.go's memoryDeps comment on principalLimiter). Only
//      the rate limit is a pure in-memory bucket that would also work under
//      memory mode; running the WHOLE suite against sqlite is what makes the
//      quota assertions read real persisted usage_events rather than lean on
//      PrincipalLimiter.Record's optimistic same-process cache bump (which
//      would still pass today, but only within the 10s cache TTL -- a much
//      weaker and more timing-fragile guarantee).
//
// Mirrors playwright.capture.config.ts (also sqlite + a bootstrap admin).
// Ports 8091/4173 like every other suite; suites never run concurrently.
// Fresh gateway + fresh sqlite file every run.

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const LIMITS_ADMIN_EMAIL = "limits-admin@example.test";
export const LIMITS_ADMIN_PASSWORD = "Limits-E2E-Pass-1";

const SQLITE_PATH = "/tmp/op-ai-gateway-limits-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-limits-e2e";
// 64 hex chars, distinct from every other suite's bootstrap token.
const BOOTSTRAP_API_TOKEN = "1111222233334444111122223333444411112222333344441111222233334444";

export default defineConfig({
  testDir: "./e2e-limits",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The per-user-limit test drives a SECOND browser context (its own login +
  // personal-token creation) on top of the admin flow, well past the 30s
  // default for a single test.
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
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: LIMITS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: "Limits Admin",
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: LIMITS_ADMIN_PASSWORD,
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
