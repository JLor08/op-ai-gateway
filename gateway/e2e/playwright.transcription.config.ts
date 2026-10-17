// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:transcription — POST /v1/audio/transcriptions end to end, across a real
// process boundary:
//
//   portal UI -> gateway (memory mode) -> app-health capability probe
//     -> the stub audio.cpp that e2e-transcription/transcription.spec.ts starts
//        in-process
//     -> the transcription verdicts it writes -> a multipart upload relayed back
//     -> the usage row, rendered by the portal's Activity view.
//
// Memory mode on loopback, exactly like e2e:speech / e2e:logs / e2e:runtime: the
// dev user "dev@example.test"/"dev-secret" is a system_admin (totp_mode=off) who
// owns the seeded "Dev Admin Group", which is all the spec needs to create an AI
// server. The audio.cpp stand-in is NOT a webServer entry: its port is chosen by
// the spec at runtime (a free one, so it can never collide) and is then typed into
// the portal's application form, which is the very path an operator takes. The
// application type is the same audio_cpp one speech uses; this suite adds none.
//
// Ports 8091/4173 like the base config; the suites never run concurrently. Fresh
// servers each run. Local-only: CI runs e2e:runtime alone.
const GATEWAY_BIN = "/tmp/op-ai-gateway-transcription-e2e";

export default defineConfig({
  testDir: "./e2e-transcription",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // The capability probe runs on the app-health loop's own tick, which can be up
  // to the 30s system cadence away from the moment the application is created
  // (the first spec step waits for it), so the default 30s test budget is too
  // tight for that step.
  timeout: 120000,
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry"
  },
  webServer: [
    {
      command: `rm -f ${GATEWAY_BIN}; go build -o ${GATEWAY_BIN} ./cmd/gateway && ${GATEWAY_BIN}`,
      cwd: "../backend",
      url: "http://127.0.0.1:8091/healthz",
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        OP_AI_GATEWAY_ADDR: "127.0.0.1:8091",
        OP_AI_GATEWAY_PUBLIC_URL: "http://127.0.0.1:4173/portal",
        GOCACHE: process.env.GOCACHE ?? "/private/tmp/op-ai-gateway-go-build-cache"
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
