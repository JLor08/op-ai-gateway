# /v1/realtime WebSocket through the agent (#395 Phase 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A client opens `wss://<gateway>/v1/realtime?model=<gateway model>` with a gateway API key. The gateway relays the WebSocket through the agent to the realtime endpoint of an agent-launched model process (sglang-omni with `--enable-realtime`, or SGLang). The spec stays loaded for the whole session, and the session leaves one usage row.

**Architecture:**
- Agent: a new router route `GET /realtime/{model}` accepts the gateway's WebSocket, ensures the spec, dials the child's `/v1/realtime` and pumps frames both ways. A new feature flag `realtime_proxy` announces it (agent 0.20.0, MINOR).
- Gateway: a new public endpoint authenticates before the upgrade, resolves the model with a new capability `realtime`, and dials the agent through a new provider seam. It pumps frames, and the usage row comes from what it saw.
- nginx: carries the upgrade.

**Tech Stack:** Go with `github.com/coder/websocket` v1.8.15 (already in both modules), React/MUI/vitest, nginx.

**Spec:** GitHub issue #395, Phase 3. Shared notes:
- `/Users/jlor08/Library/Application Support/Claude/side-session-notes/local_23aad4de-1ce7-43cb-8342-d60c6e566fa9/sglang-omni-realtime-research-NOTES.md` (protocol)
- `…/sglang-codebase-seams-NOTES.md` (seams; section C covers WebSockets, D covers usage, E covers features)

This plan builds on branch `claude/sglang-runtime-types-395` (PR #396), which carries the `sglang`/`sglang_omni` types and agent 0.19.2.

## Design

### R1 Agent router route
- **Route:** `GET /realtime/{model}`. The path model is URL-decoded, and instance addresses (`m#k`) are handled exactly like `/live/{model}`: `readAddress`, `childModel`, `ensureServing`, then hold `release()` until the session ends.
- **Handshake:** a request that is not a WebSocket handshake gets 400 `runtime.realtime_upgrade_required`. An unknown model gets the same error as `/live`.
- **Child dial:** dial `ws://<child endpoint>/v1/realtime?<inbound query with model= replaced by childModel>`. Use the `liveChildQuery` rules, but URL-encode normally; there is no audio.cpp verbatim quirk here. If the dial fails, answer HTTP 502 `runtime.upstream_gone` before accepting. Accept the inbound socket only after the child dial has succeeded.
- **Pump:** relay both directions and keep the message type (text or binary).
  - Read limit per message is 4 MiB on each side.
  - The idle timeout is 15 minutes without a frame in either direction. Pings do not count; sglang-omni's own idle is 300 s.
  - When one side closes, close the other with the same status code and reason. A code that is not valid to send becomes 1011.
- **Shutdown:** the router tracks open sessions. Router `Close()` closes them with 1001 and does not wait longer than 2 s.
- **Hijack:** the route must not run through `deadlineWriter`, whose `Hijack` refuses by design. coder/websocket must reach the real writer. Keep the existing ban everywhere else.
- **Feature:** feature `realtime_proxy`, `Since: "0.20.0"`. `Version` goes from 0.19.2 to 0.20.0 (MINOR), with a bump comment.

### R2 Gateway capability and routing
- **Capability:** a new capability `routing.CapabilityRealtime = "realtime"`. The model may be the target of `GET /v1/realtime`. It takes manual verdicts only; no probe source writes it, and the agent-ingest reserved list keeps it.
- **Endpoint:** `endpointRealtime`, in the session_extract consts. It sits in the coarse `openai_audio` flavor family. A realtime request needs the `openai_audio` flavor on the application/spec plus `realtime: yes`.
- **Target:** only `server_agent` targets whose agent reports `realtime_proxy` (`s.AgentFeatures.Has`; fails closed). This is the same pin-test pattern as `fullduplex_proxy`. Any other target is not a candidate.

### R3 Gateway endpoint `GET /v1/realtime` (alias `/openai/v1/realtime`)
- **Before the upgrade:**
  - The key is `Authorization: Bearer <key>` with scope `gateway:use` or `llm:invoke`. Alternatively the client offers the subprotocols `realtime` plus `openai-insecure-api-key.<key>` (the OpenAI browser convention); the gateway then selects `realtime`.
  - `model` is required (400 `realtime.model_required`).
  - Admission (rate, quota, budget) runs once, followed by target resolution.
  - The session is registered in Active Requests until it ends.
- **Upstream dial:** a new provider seam, `provider.NativeWebSocketDialer` with `DialWebSocket(ctx, target, path, rawQuery string, header http.Header) (*websocket.Conn, *http.Response, error)`, implemented on the OpenAI-compatible client with the outbound app transport.
  - `http.Client.Timeout` is 0.
  - Upstream auth is applied as `applyUpstreamAuth` does.
  - The Multiplexer dispatches to the seam.
  - Path `/realtime/<url-escaped upstream model>`, query = the client's query minus `model`.
  - If the upstream dial fails, the gateway answers 502 or 503 before accepting.
- **Pump:** both directions, message type kept, 4 MiB limit, one goroutine per direction, ctx teardown. Close codes are passed through; 1011 for an invalid one.
- **Usage (one row per session, at the end):**
  - Tokens, if any `response.done` with a usage object was seen. Accept `response.usage` with `prompt_tokens`/`completion_tokens` (sglang-omni conversation flavor) or `input_tokens`/`output_tokens`, and sum them.
  - Otherwise `audio_second` from the client's `input_audio_buffer.append` events: decoded base64 `audio` bytes ÷ 32000, which is PCM16 mono at 16 kHz, sglang-omni's documented input format.
  - Otherwise a logged zero.
  - Usage endpoint `realtime`, status 101 or the closing code.
- **Go 1.27 (#387):** a real WebSocket is not affected. Do not loop it through an HTTP full-duplex POST.

### R4 Deployment
nginx gets a `/v1/realtime` and `/openai/v1/realtime` location with `proxy_http_version 1.1`, the `Upgrade` and `Connection` headers, and `proxy_read_timeout 3600s`. Put it in all three hand-kept copies: `gateway/deploy/nginx/locations.conf`, `gateway/deploy/k8s/nginx-configmap.yaml`, and the matching k8s ingress/README mentions. The existing nginx config tests (`gateway/deploy/nginx-config.test.sh`) must pass, and new locations get asserts.

### R5 Portal
- The capability-verdict UI offers `realtime` with label "Realtime (WebSocket)", same in de and en.
- The sglang_omni type note adds: `--enable-realtime` enables `/v1/realtime`, and the `realtime` verdict must be set to yes.

### R6 Docs and ADR-104
- agent-runtime-manager:
  - §4.5 and §13: the WebSocket limitation becomes "only `/realtime/{model}`";
  - §7: the feature list and its count;
  - the sglang_omni facts block.
- api-surface and openapi: `/v1/realtime`.
- compat: realtime usage.
- ADR-104: the route, the feature, the usage rule, and the alternatives rejected (WS→HTTP translation, a generic upgrade passthrough in `serveProxy`).

## Global Constraints
- Branch `claude/realtime-395`. Never commit to `main`.
- English repo text, no attribution, no host names.
- Go gates per module: `golangci-lint fmt --diff`, `GOTOOLCHAIN=go1.26.5 golangci-lint run ./...`, and `go test -count=1 ./internal/archtest/`. Gateway tests use `GOTOOLCHAIN=go1.26.5` and `-skip 'TestVoice|TestLiveTranscriptionLoopback'`.
- Frontend: lint (0 errors), format:check, build. `make lint-docs`.
- The feature name `realtime_proxy` is identical in both modules, and each module pins it with a test.

## Review Focus
1. A long session must keep the spec admitted, and a session end of any kind (client close, child close, idle, router stop) must release it. Test each end.
2. An agent without `realtime_proxy` is never dialed. Test with a fake agent that lacks the feature.
3. Frames over 4 MiB must close the session cleanly. They must not grow memory or hang a goroutine.
4. The key in a subprotocol must never be logged or echoed: the selected subprotocol is `realtime` only.
5. A session that ends abnormally still writes exactly one usage row.

---

### Task 1: Agent router realtime route, feature, version
**Files:**
- `server-agent/internal/runtime/router.go` (+ a new `router_realtime.go` if cleaner, + tests);
- `server-agent/internal/runtime/driver.go` (session drain on `Close`);
- `server-agent/internal/agent/features.go`, `server-agent/internal/agent/agent.go` (`Version`), `server-agent/internal/agent/features_test.go`.

- [ ] Write the failing tests first, against a fake child WebSocket server (httptest + coder/websocket):
  - frames relay both ways, text and binary;
  - the query is forwarded with `model` rewritten to the child model;
  - an instance address `m#1` dials instance 1's child;
  - a dial failure gives 502 before upgrade;
  - a non-upgrade request gives 400;
  - the session holds the spec admitted, and release runs once on client close, child close, idle timeout (with an injected short timeout) and router `Close`;
  - a 4 MiB + 1 frame closes with 1009;
  - the feature registry carries `realtime_proxy` Since 0.20.0, and `Version` is 0.20.0.
- [ ] Implement R1. Run the agent gates and commit `feat(agent): realtime WebSocket route (realtime_proxy, 0.20.0)`.

### Task 2: Gateway capability, endpoint routing and feature gate
**Files:**
- `gateway/backend/internal/routing/store.go` (capability const + reserved list), plus resolver and capability-gate tests;
- `gateway/backend/internal/gateway/session_extract.go` (endpoint);
- `gateway/backend/internal/gateway/agent_features.go` (feature const + pin test);
- `portal` capability validation lists, if any.

- [ ] Write the failing tests first:
  - a realtime request resolves only to a server_agent target with `openai_audio`, `realtime: yes` and an agent reporting `realtime_proxy`;
  - every other case is no candidate;
  - the portal accepts a manual `realtime` verdict.
- [ ] Implement R2. Run the gateway gates and commit `feat(gateway): realtime capability and agent feature gate`.

### Task 3: Gateway `/v1/realtime` handler, provider seam, pump, usage
**Files:**
- a new `gateway/backend/internal/gateway/realtime_ws.go` (+ test);
- `gateway/backend/internal/provider/proxy.go` + `openai_compatible.go` + `multiplexer.go` (seam);
- `gateway/backend/internal/gateway/server.go` (routes);
- usage recording.

- [ ] Write the failing tests first (an end-to-end test against a fake agent router that speaks WS):
  - Bearer auth works and a wrong key gets 401 before upgrade;
  - subprotocol auth selects `realtime` and never echoes the key;
  - a missing model gives 400;
  - frames relay both ways;
  - the query is forwarded minus `model`, with the upstream model in the path;
  - a dial failure gives 502 before upgrade;
  - the usage row records summed tokens from `response.done` usage;
  - with no usage, `audio_second` comes from the append events (32000 B = 1 s);
  - a malformed frame stream gives a logged zero;
  - exactly one row per session on every end path;
  - Active Requests shows the session while it is open;
  - a 4 MiB + 1 frame closes with 1009.
- [ ] Implement R3. Run the gateway gates and commit `feat(gateway): /v1/realtime WebSocket relay`.

### Task 4: nginx and portal
**Files:** `gateway/deploy/nginx/locations.conf`, `gateway/deploy/k8s/nginx-configmap.yaml` (+ ingress or README mentions), `gateway/deploy/nginx-config.test.sh`, the portal capability UI, `gateway/frontend/src/i18n.ts` (+ tests).
- [ ] Write the tests first (nginx-config.test.sh asserts and vitest), then implement R4 and R5. Run the deploy test script and the frontend gates. Commit `feat(deploy,portal): realtime location and capability`.

### Task 5: Docs and ADR-104
- [ ] Implement R6. Run `make lint-docs`. Commit `docs: /v1/realtime through the agent (ADR-104)`.
