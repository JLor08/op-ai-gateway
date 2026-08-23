// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:smtp — proves the SMTP email-invite feature end to end against the
// dev/memory gateway (no capture key → the settings store is volatile, so the
// SMTP password lives in plaintext RAM; no key needed). A minimal stdlib-only
// SMTP sink (portal-ui/e2e-smtp/mailcatcher) catches what the gateway sends and
// exposes it over HTTP so the test can assert the invite email + its
// set-password link. The gateway is unchanged memory mode — SMTP is configured
// at runtime via System Settings. Ports 8091/4173 like the base config (the
// suites never run concurrently); the catcher adds 2525 (SMTP) + 8092 (HTTP).
// Fresh servers each run. The catcher's HTTP port is deliberately NOT the
// conventional MailHog/MailCatcher 8025 — that port is a common default for
// other locally-running mail-testing containers, and a stray one on the dev
// host silently steals connections meant for ours (no error, just wrong
// responses), producing a flaky "Timed out waiting ... webServer" failure.
const GATEWAY_BIN = "/tmp/op-ai-gateway-smtp-e2e";

export const SMTP_HTTP_BASE = "http://127.0.0.1:8092";
export const SMTP_HOST = "127.0.0.1";
export const SMTP_PORT = "2525";

export default defineConfig({
  testDir: "./e2e-smtp",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry"
  },
  webServer: [
    {
      command: "rm -f /tmp/op-ai-gateway-mailcatcher-e2e; go build -o /tmp/op-ai-gateway-mailcatcher-e2e . && /tmp/op-ai-gateway-mailcatcher-e2e",
      cwd: "e2e-smtp/mailcatcher",
      url: `${SMTP_HTTP_BASE}/healthz`,
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        MAILCATCHER_SMTP_ADDR: `${SMTP_HOST}:${SMTP_PORT}`,
        MAILCATCHER_HTTP_ADDR: "127.0.0.1:8092",
        GOCACHE: "/private/tmp/op-ai-gateway-go-build-cache"
      }
    },
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
