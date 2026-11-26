# RPC protocol version in the binary inventory (#441) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Probe and store the llama.cpp RPC protocol version per binary and match RPC cluster heads/members on it, with explained rejections in the portal.

**Architecture:** The agent probes native `ggml-rpc-server` binaries for their banner protocol and exchanges version/protocol between sibling binaries; the gateway stores `rpc_protocol` per inventory row (migration 117), matches head and member on protocol first and build identity second, and returns a rejection reason per candidate that the portal renders.

**Tech Stack:** Go (server-agent module, gateway backend module; memory/sqlite/postgres stores), React/TypeScript/Vitest portal.

**Spec:** `docs/superpowers/specs/2026-11-26-rpc-protocol-version-design.md`

## Global Constraints

- Worktree: `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/feat-441-rpc-protocol-version`, branch `feat/441-rpc-protocol-version`. Never touch `main`.
- Protocol string format everywhere: `^\d{1,3}\.\d{1,3}\.\d{1,3}$`; anything else is treated as empty.
- Wire/DTO/column name: `rpc_protocol` (agent sample, gateway ingest, store column, `ServerBinaryDTO`).
- Compare the **full** protocol (`X.Y.Z` string equality).
- ServerAgent `Version` in `server-agent/internal/agent/agent.go`: `0.25.1` → `0.25.2`, once (no new feature flag).
- Migration: `{version: 117, name: "server_binaries_rpc_protocol"}`, `addColumnIfMissing(..., "server_binaries", "rpc_protocol text not null default ''")`.
- All three store drivers; verify the postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` (provision like CI).
- Portal strings: German and English together in `gateway/frontend/src/i18n.ts`.
- Do not add the protocol to `memberGenerationPart` (no head relaunch on upgrade).
- Go: `golangci-lint fmt --diff` and `golangci-lint run` per module must be clean (gofumpt, gocritic). Frontend: `npm test`, `npm run build`, `npm run lint`, `npm run format:check`.
- After touching imports in Go, run the archtest (`go test ./internal/archtest/...` in each module where it exists).

## Review Focus

1. A probe that never sees a banner (old rpc-server, crash, missing `-d` support) must end within the timeout, leave no process behind, and fall into the existing retry backoff — test with a stub that prints nothing and sleeps.
2. A host with two installations (`/opt/a/{llama-server,ggml-rpc-server}`, `/opt/b/...`) must pair siblings per directory, never across directories — agent test with two dirs and different protocols.
3. A member server with two rpc_server rows, one protocol-matching and one build-identical, must pick the build-identical one; one mismatching and one matching must pick the matching one — portal test.
4. Old agent rows (no protocol) must keep today's behavior except the `unknown`/`0` hygiene — portal test with protocol-less rows.
5. A `llama_server` that loses its rpc sibling must lose its protocol at the gateway (not kept) while an rpc_server row with a failed re-probe keeps its protocol — ingest test.

---

### Task 1: Agent — probe, sibling exchange, wire field

**Files:**
- Modify: `server-agent/internal/runtime/rpc_classify.go` (export the banner prefix)
- Modify: `server-agent/internal/agent/binaries.go`
- Modify: `server-agent/internal/sample/sample.go:598-607`
- Modify: `server-agent/internal/agent/agent.go:497` (Version `0.25.2`)
- Test: `server-agent/internal/agent/binaries_test.go`, `server-agent/internal/runtime/rpc_classify_test.go` (only if the rename touches it)

**Interfaces:**
- Produces: `sample.BinaryInfo.RPCProtocol string \`json:"rpc_protocol,omitempty"\``.
- Produces: `runtimectl.RPCServerBannerStart = "Starting RPC server v"` (rename of `memberBannerStart`; `parseMemberBanner` uses it).
- Produces (agent package): `parseRPCProtocol(out string) (string, bool)` — the first line starting with `RPCServerBannerStart`, trimmed, matching the protocol format; `execRPCProtocol(ctx context.Context, path string) (string, error)`.

- [ ] **Step 1: Write failing tests** in `binaries_test.go`:
  - `TestParseRPCProtocol`: `"ggml_cuda_init: found 1 CUDA devices\nStarting RPC server v10.0.2\n  endpoint ..."` → `"10.0.2", true`; `"Starting RPC server v10.0"` → false; no banner → false; `"\r"` line ending tolerated.
  - `TestRPCServerProtocolProbed`: scan with `llama-server` + `ggml-rpc-server` in `/opt/llama/b1/`, fake llama `--version` output and a fake protocol runner returning the banner `10.0.2` → the rpc_server row has `RPCProtocol "10.0.2"` and the llama row's version/build/commit; the llama_server row has `RPCProtocol "10.0.2"`.
  - `TestRPCServerWithoutLlamaSibling`: only `/opt/rpc/ggml-rpc-server` → `RPCProtocol "10.0.2"`, Version/Build/Commit empty (path has no `b<N>` token).
  - `TestLlamaServerWithoutRPCSibling`: only a llama_server → `RPCProtocol ""`.
  - `TestSiblingsPairPerDirectory`: `/opt/a/` (protocol `10.0.2`) and `/opt/b/` (protocol `9.0.0`) → each llama row gets its own directory's protocol.
  - `TestRPCProtocolProbeCachedAndBackoff`: the protocol runner is called once per (size, mtime); a run without banner is retried only after `retryBackoff(1)`, like the version probe.
  - `TestWSLRPCServerNotProbed`: a `wsl://` rpc_server entry is never passed to the protocol runner and carries no protocol.
  - `TestExecRPCProtocolKillsAfterBanner` and `TestExecRPCProtocolTimeout`: use the file's existing helper-process pattern for exec tests (see `execVersion` tests ~:412-487): a helper that prints the banner then blocks → returns quickly with the banner and the process is gone; a helper that prints nothing and blocks → returns at the context deadline. Also assert the argv the helper receives is `-d CPU -H 127.0.0.1 -p <n>` with `0 < n < 65536`.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/agent/ -run 'RPCProtocol|RPCServer|LlamaServerWithout|SiblingsPair|WSLRPCServer' -count=1` → FAIL (undefined symbols).
- [ ] **Step 3: Implement.**
  - `prober` gains `runProtocol func(ctx context.Context, path string) (string, error)`; native prober uses `execRPCProtocol` (injectable on `binaryInventory` like `runVersion`, and a parameter of `scanBinariesCtx`/`scanBinaries` tests use); WSL probers leave it nil → rpc_server not probed there.
  - Generalise the probe result: `cachedVersion` gains `protocol string`; `versionProbe.parse` returns a struct `probedIdentity{version, build, commit, protocol string}` plus ok; `versionProbe` gains a `run func(*prober) func(ctx context.Context, path string) (string, error)` so `probeFor(binaryKindRPCServer)` selects `runProtocol` with `binaryVersionTimeout` and `parseRPCProtocol`. `addOffline` restores a cached protocol like it restores the version.
  - `execRPCProtocol`: pick a free port by `net.Listen("tcp", "127.0.0.1:0")` + close; `exec.CommandContext(ctx, path, "-d", "CPU", "-H", "127.0.0.1", "-p", port)`, `Env = minimalEnv()`, `WaitDelay = binaryWaitDelay`; read combined stdout+stderr through one pipe line by line (cap `binaryOutputLimit` bytes); on the banner line kill the process, wait, return the text read so far with nil error; otherwise return the text and the run/context error.
  - `inheritSiblingVersions` → also pair protocols per `siblingDirKey`: llama_server takes the first protocol-carrying rpc_server of its directory; rpc_server keeps its own protocol and still inherits version/build/commit as today.
  - `sample.BinaryInfo.RPCProtocol`, `Version = "0.25.2"`.
  - Update the doc comments of `scanBinaries` ("Only a llama_server or vllm is ever executed") to the new rule.
- [ ] **Step 4: Run** `cd server-agent && go test ./... -count=1` → PASS; `golangci-lint fmt --diff ./... && golangci-lint run ./...` clean; archtest green.
- [ ] **Step 5: Update** `docs/architecture/cross-cutting/agent-runtime-manager.md` "#### Binary inventory" (~:2077-2132): the rpc-server probe (argv, `-d CPU`, kill after banner, no WSL probe), the `rpc_protocol` wire field, the sibling pairing in both directions.
- [ ] **Step 6: Commit** `feat(agent): probe the RPC protocol version of ggml-rpc-server binaries (#441)`.

### Task 2: Gateway store — `rpc_protocol` column

**Files:**
- Modify: `gateway/backend/internal/routing/store.go:3379-3399` (`ServerBinary.RPCProtocol string`)
- Modify: `gateway/backend/internal/store/migrate.go` (migration 117), `internal/store/sqlite_runtime.go:599-735` (select/scan, agent upsert insert + `do update set`, manual upsert)
- Modify: `gateway/backend/internal/routing/memory_store.go:3206-3298` only if a field-by-field copy drops the new field
- Create: `gateway/backend/internal/store/migration117_server_binaries_rpc_protocol_test.go`
- Test: `gateway/backend/internal/store/server_binaries_conformance_test.go`
- Docs: `docs/architecture/reference/data-model.md` (`server_binaries` row :92, migration history heading count and a row for 117; also fix the stale "115 migrations" heading)

**Interfaces:**
- Produces: `routing.ServerBinary.RPCProtocol` persisted by `ReplaceAgentServerBinaries` (agent rows) and returned by `ServerBinaries`; manual rows store `""` (the manual API does not set it).

- [ ] **Step 1: Failing tests**: migration test in the style of `migration113_*_test.go` (`forEachDialectMigratedTo(t, 116, ...)`: column absent before, present after with default `''`, round-trip, replay no-op); conformance case: `ReplaceAgentServerBinaries` with `RPCProtocol "10.0.2"` round-trips, a second replace with `"10.0.1"` updates it, `VersionOverride` still survives.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ -run 'Migration117|ConformanceServerBinaries' -count=1` → FAIL.
- [ ] **Step 3: Implement** the field, migration 117 and the SQL column lists.
- [ ] **Step 4: Run** the same tests, then with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (provision like CI) → PASS on all three drivers.
- [ ] **Step 5: Commit** `feat(store): rpc_protocol column for the binary inventory (#441)`.

### Task 3: Gateway ingest

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest_binaries.go` (`agentBinarySample.RPCProtocol \`json:"rpc_protocol"\``, `sanitizeBinarySamples`, `binaryInventoryChanged`, `keepKnownBinaryVersions`)
- Test: `gateway/backend/internal/gateway/agent_ingest_binaries_test.go`

**Interfaces:**
- Consumes: `routing.ServerBinary.RPCProtocol` (Task 2).
- Produces: `routing.ValidRPCProtocol(s string) bool` in `internal/routing/store.go` next to `ServerBinary` (shared by ingest and portal).

- [ ] **Step 1: Failing tests**: a sample with `rpc_protocol "10.0.2"` is stored; `"10.0"`, `"v10.0.2"`, `"1000.0.0"` are stored as `""`; a protocol-only change notifies clusters (`binaryInventoryChanged` true); a re-report of an `rpc_server` row without protocol keeps the stored one; a re-report of a `llama_server` row without protocol does **not** keep it.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run 'Binar' -count=1` → FAIL.
- [ ] **Step 3: Implement**; extend the `keepKnownBinaryVersions` doc comment with the protocol rule.
- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `feat(gateway): ingest the rpc_protocol of agent binaries (#441)`.

### Task 4: Portal matching, candidates, DTOs

**Files:**
- Modify: `gateway/backend/internal/portal/service_binaries.go` (`ServerBinaryDTO.RPCProtocol \`json:"rpc_protocol"\``, mapper, `binaryIdentity` hygiene, new compatibility function, `MatchingRPCBinary` preference)
- Modify: `gateway/backend/internal/portal/service_cluster.go` (`binaryVersionKnown`, `memberVersionDiffers`, `manualMemberVersionUnknown`, `candidateHeadFor`, `clusterCandidate`, `ClusterCandidateDTO`, `ClusterCandidateList`, `listClusterCandidates`)
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go:75-86` and `portal_application_endpoints.go:286-295` (add `head_rpc_protocol` to the response)
- Test: `internal/portal/service_binaries_test.go`, `internal/portal/service_cluster_test.go`, `internal/gateway/agent_ingest_binaries_test.go` (`TestBinariesAPI` JSON shape)
- Docs: `docs/architecture/reference/api-surface.md:706,708`, `openapi.yaml` descriptions (:3156-3199, :3294-3326), `agent-runtime-manager.md` §3.7 (:2214, :2232-2238, :2316-2326), new ADR in `09-architecture-decisions.md` amending ADR-096 (d)

**Interfaces:**
- Consumes: `routing.ServerBinary.RPCProtocol`, `routing.ValidRPCProtocol`.
- Produces:
  - `type rpcCompat int` with `rpcCompatIdentical`, `rpcCompatProtocol`, `rpcMismatchProtocol`, `rpcMismatchBuild`, `rpcCompatUnknown`; `func rpcCompatibility(head, member routing.ServerBinary) rpcCompat` implementing spec decision 5; `(rpcCompat).compatible() bool` (identical or protocol), `(rpcCompat).mismatch() bool` (protocol or build mismatch).
  - `binaryIdentity`: commit `unknown` (case-insensitive) → `""`, build `0` → `""` (also when read from an override).
  - `binaryVersionKnown(b)` = build identity known **or** `b.RPCProtocol != ""`.
  - `MatchingRPCBinary(ctx, serverID, head) (routing.ServerBinary, bool, error)` keeps its signature; picks compatible rows, identical before protocol, then by path.
  - `func (s *Service) rpcBinaryRejection(ctx, serverID string, head routing.ServerBinary) (reason, memberProtocol string, err error)` for a server without a match: `"no_rpc_binary"` (no eligible rpc_server row), else `"protocol_mismatch"` (with the first mismatching row's protocol by path), else `"build_mismatch"`, else `"version_unknown"`; the head-unknown case is `"head_version_unknown"`.
  - `ClusterCandidateDTO` gains `RPCBinaryProtocol string \`json:"rpc_binary_protocol,omitempty"\``, `IncompatibleReason string \`json:"incompatible_reason,omitempty"\``, `MemberRPCProtocol string \`json:"member_rpc_protocol,omitempty"\``; `ClusterCandidateList` gains `HeadRPCProtocol string` → JSON `head_rpc_protocol` (omitempty).
  - `memberVersionDiffers(member, listed, head)` = `listed && rpcCompatibility(head, member).mismatch()`; `manualMemberVersionUnknown` true when either is unlisted or `rpcCompatibility == rpcCompatUnknown`; `mismatchedMemberSpecs` follows `memberVersionDiffers`. The save error `ErrClusterVersionMismatch` names both protocols when the mismatch is a protocol mismatch, else both effective versions.

- [ ] **Step 1: Failing tests** (table-driven `TestRPCCompatibility`): equal protocols, different commits → protocol; equal commit → identical; protocols `10.0.2` vs `10.0.1` → protocol mismatch; head no protocol + same commit → identical; head no protocol + different commit → build mismatch; head no protocol, member commit `unknown` build `0` → unknown; old rows (no protocol both) behave as `binariesSameVersion` did. `TestMatchingRPCBinaryPrefersIdentical` (Review Focus 3), `TestMatchingRPCBinarySkipsProtocolMismatch`, `TestBinaryIdentityUnknownHygiene` (incl. override `"build 0, commit unknown"`), `TestClusterCandidatesExplainRejection` (automatic candidate with mismatching protocol → listed, `compatible false`, `incompatible_reason "protocol_mismatch"`, `member_rpc_protocol`, list `head_rpc_protocol`), `TestClusterCandidatesHeadKnownByProtocolOnly`, a manual save refused on protocol mismatch, and `TestBinariesAPI` asserting `rpc_protocol` in the JSON.
- [ ] **Step 2: Run** `go test ./internal/portal/ ./internal/gateway/ -run 'RPCCompat|MatchingRPCBinary|BinaryIdentity|ClusterCandidates|BinariesAPI|ClusterVersion' -count=1` → FAIL.
- [ ] **Step 3: Implement**; keep `binariesSameVersion` only if callers remain, otherwise fold it into `rpcCompatibility`. Do not change `memberGenerationPart`.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./... -count=1` → PASS; golangci fmt/run clean; archtest green.
- [ ] **Step 5: Docs** as listed (ADR: "Match RPC cluster binaries on the probed protocol version", context = research results from the spec, decision = spec decisions 1-12, consequences incl. no WSL probe and no generation change).
- [ ] **Step 6: Commit** `feat(portal): match RPC cluster binaries on the protocol version and explain rejections (#441)`.

### Task 5: Portal UI

**Files:**
- Modify: `gateway/frontend/src/api/binaries.ts` (`rpc_protocol?: string`), `src/api/runtime.ts:140-164` (`rpc_binary_protocol?`, `incompatible_reason?`, `member_rpc_protocol?` on `ClusterCandidate`; `head_rpc_protocol?` on `ClusterCandidateList`)
- Modify: `src/components/BinariesSection.tsx` (new column "RPC protocol" after Version, empty shows `—`), `src/components/shared/BinaryPicker.tsx` (caption appends ` · RPC <protocol>` when set), `src/components/shared/clusterCandidates.ts` (export `rejectedCandidates(state, taken)`: automatic candidates with `compatible === false` and not taken; keep `offeredCandidates` unchanged), `src/components/ClusterHeadEditor.tsx` (render rejected candidates below the offered ones as disabled entries with the reason; show the chosen candidate's `rpc_binary_protocol` next to its version)
- Modify: `src/i18n.ts` (DE+EN): `binariesRpcProtocol` ("RPC-Protokoll" / "RPC protocol"), `clusterCandidateRejected` (heading), and one key per reason: `clusterRejectProtocolMismatch(member, head)` ("Protokoll {member} statt {head}" / "protocol {member} vs head {head}"), `clusterRejectBuildMismatch`, `clusterRejectVersionUnknown`, `clusterRejectNoRpcBinary`, `clusterRejectHeadVersionUnknown`
- Test: `BinariesSection.test.tsx`, `shared/BinaryPicker.test.tsx`, `shared/clusterCandidates.test.ts`, `ClusterHeadEditor.test.tsx`, `api/binaries.test.ts`

- [ ] **Step 1: Failing tests**: Binaries table shows `10.0.2` in the RPC protocol column; picker caption contains `RPC 10.0.2`; `rejectedCandidates` returns only automatic `compatible:false` entries not taken; the editor shows a rejected server with the protocol-mismatch text containing both protocols and it is not selectable.
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/BinariesSection.test.tsx src/components/shared src/components/ClusterHeadEditor.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test && npm run build && npm run lint && npm run format:check` → PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): show the RPC protocol and explain rejected cluster candidates (#441)`.

### Task 6: Final docs and verification (controller)

- [ ] Glossary/building-block touch-ups if needed (`12-glossary.md:160`, `05-building-block-view.md:349`); `make lint-docs`.
- [ ] Full: `make test-go`, both modules' golangci, frontend suite, postgres leg, `make sonar-scan` + findings + branch-findings.
- [ ] Whole-branch review (Opus); fix findings.
- [ ] Merge `origin/main` if it moved (agent `Version`, migration number); remove `docs/superpowers/` and `docs/implementation-status.md`; verify `git diff --name-only origin/main...HEAD`.
</content>
</invoke>
