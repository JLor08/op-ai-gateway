// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:rpc-cluster — a REAL llama.cpp RPC cluster, end to end: the gateway
// (memory store) and two real server-agent processes on one machine, each
// registered as its own AI server. The head server's agent runs a real
// llama-server; the member server's agent runs a real ggml-rpc-server; the
// gateway orchestrates the members-first start and a completion is served by
// layers that live on the member.
//
// Pure API, like playwright.agent.config.ts: the portal UI of the cluster has
// its own suites, and what only this layer can prove is the process circle
// (gateway -> two agents -> llama-server <-> ggml-rpc-server). No frontend
// build and no browser; every request is an absolute URL.
//
// The llama.cpp build is NOT built here. The suite reads two variables:
//
//   OP_E2E_LLAMA_BIN_DIR  directory holding llama-server and ggml-rpc-server of
//                         one build with -DGGML_RPC=ON (the agent derives the
//                         member's version from the llama-server beside it)
//   OP_E2E_RPC_MODEL      absolute path to stories260K.gguf
//
// CI builds a CPU-only binary pair at the pinned llama.cpp commit and caches
// it (.github/workflows/ci.yml, job e2e-rpc-cluster). Locally any RPC-enabled
// build of that commit works; e2e-rpc-cluster/rpc-cluster.spec.ts's header
// lists what differs on a Metal build.
//
// Memory mode on 127.0.0.1:8091 like every other suite (fresh gateway each
// run; suites never run concurrently).

// The two agents' router ports, and therefore the Port of each server's
// `server_agent` application (the gateway reaches an agent at
// server.Domain:application.Port). Distinct from e2e:runtime's 18081 and from
// the 809x band of the gateway listeners.
export const HEAD_ROUTER_PORT = 18281;
export const MEMBER_ROUTER_PORT = 18282;

// One agent binary, run twice with distinct configurations.
export const RPC_AGENT_BIN = "/tmp/op-e2e-rpc-cluster-agent";

const GATEWAY_BIN = "/tmp/op-ai-gateway-rpc-cluster-e2e";

export default defineConfig({
  testDir: "./e2e-rpc-cluster",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // A scenario performs real process starts (members, then a llama-server
  // that loads a model over RPC), a member idle-out of about a minute and
  // several telemetry round trips.
  timeout: 300000,
  // The whole run normally takes about five minutes. A hang fails here, with
  // Playwright's report and the agents' logs, well before CI's 30 minute job
  // timeout would kill the run without either.
  globalTimeout: 20 * 60 * 1000,
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
    }
  ]
});
