// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:chat-transcription — a speech-to-text turn in the PORTAL CHAT, end to end,
// across a real process boundary:
//
//   portal chat UI (an attached or recorded clip) -> POST /api/portal/chats/{id}/runs
//     (kind "transcription") -> the server-side run executor
//     -> loopback multipart POST /v1/audio/transcriptions
//     -> the stub audio.cpp that e2e-chat-transcription/transcription-chat.spec.ts
//        starts in-process
//     -> the transcript into the chat store, the input clip into the chat blob
//        store (/api/portal/chat-blobs/{id})
//     -> the transcript turn the portal renders, and the usage row.
//
// Memory mode on loopback, exactly like e2e:transcription / e2e:chat-speech: the dev
// user "dev@example.test"/"dev-secret" is a system_admin (totp_mode=off) who owns the
// seeded "Dev Admin Group", which is all the spec needs to create an AI server. The
// audio.cpp stand-in is NOT a webServer entry: its port is chosen by the spec at
// runtime (a free one, so it can never collide) and is then typed into the portal's
// application form, which is the very path an operator takes. The transcription
// capability verdict that makes the chat OFFER the model is written by the gateway's
// own app-health probe against that stub; nothing is seeded.
//
// Ports 8091/4173 like the base config (the frontend's dev/preview proxy is wired to
// 8091 and the preview port is strict, so no suite can use others); the suites never
// run concurrently. Fresh servers each run. Local-only: CI runs e2e:runtime alone.
const GATEWAY_BIN = "/tmp/op-ai-gateway-chat-transcription-e2e";

export default defineConfig({
  testDir: "./e2e-chat-transcription",
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
