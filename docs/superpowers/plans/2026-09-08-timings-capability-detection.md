# Detecting `timings_per_token` support — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop discovering whether an upstream tolerates the live-progress parameters by paying for a rejection. Detect it, persist the verdict, show it, and let it override the application type.

**Architecture:** A tri-state verdict per mapping, persisted like `context_size` but deliberately outside the `metrics_locked` guard, written by two independent detectors — the gateway's existing `/props` context pass for non-agent applications, and the agent's own loopback probe for managed children. The decision at request time is a three-layer rule: an observed rejection beats a detected verdict, a detected verdict beats the application's shape, and the shape beats silence.

**Tech Stack:** Go backend (`gateway/backend`), Go `server-agent`, React + TypeScript portal (`gateway/frontend`). No new dependencies.

**Design spec:** `docs/superpowers/specs/2026-09-08-timings-capability-detection-design.md` — read §1 and §2 before starting any task; they are the whole design.

**Implements:** issue #52. **Builds on** #51, whose retry-and-memo is the safety net this work depends on.

## Global Constraints

- **The three-layer rule is the design.** Send iff: the memo has NOT recorded a rejection for this mapping, AND (the verdict is `supported`) OR (the verdict is `""` AND the shape implies a tolerant upstream). Never send on `unsupported`.
- "The shape implies a tolerant upstream" means EXACTLY: application type `llama_cpp` or `vllm`; or application type `server_agent` whose `routing.EffectiveRuntimeSpecType(spec)` is `llama_cpp` or `vllm`. Nothing else — not `llama_swap`, not `litellm`, not `ollama`, not `mock`, not a `server_agent` child of any other effective type.
- The persisted verdict has exactly three values: `""` (never determined), `"supported"`, `"unsupported"`. Plain string, snake_case json tag, **no `omitempty`**.
- **Only a llama.cpp `/props` document is evidence.** With the key → `supported`; that same document without the key → `unsupported`; anything else (a failed fetch, vLLM's `/v1/models`, TGI's `/info`, Ollama's `/api/show`, an unparseable body) → unknown, and the persisted value is **left untouched**. Getting this wrong marks every vLLM application unsupported and silently removes a working live number — see spec §2.
- **The verdict is a capability, not a metric.** Its writer must NOT carry the `metrics_locked = 0` guard and must NOT touch `metrics_source` or `metrics_updated_at`. It is the first writer on that table without the guard; say why in a comment beside it, or it reads as an oversight.
- **An unchanged value is never rewritten.** Compare against the mapping's currently stored verdict before issuing an UPDATE, memoized per distinct spec/mapping, exactly as `writeBackRuntimeContext` documents. A stable capability must not drive one UPDATE per second per mapping forever.
- `live_progress_checked_at` is for the operator's tooltip and diagnostics only. **No decision logic may read it** — no TTL, no staleness gate. The probe's own cadence is the refresh.
- Nothing here may weaken #51's retry or memo. They are what make a stale `supported` safe.
- Everything is advisory: a failed probe, an unparseable body, an older agent that never reports the field are all ordinary states — never errors, never logged as such.
- Frontend: every new user-visible string gets a key in BOTH `de` and `en`. Refer to badges **by key, never by colour**.
- Every new or changed test must FAIL if its production change is reverted. Verify by reverting with an editor, running the test, and restoring.
- Gates, per touched module. `gateway/backend` and `server-agent`: `golangci-lint fmt --diff`, `golangci-lint run`, `go test ./...`. `gateway/frontend`: `npm run format:check`, `npm run lint`, `npm run build`, `npm test`. Root: `./scripts/check-docs.sh`, `sh scripts/check-docs.test.sh`. Both Go linters are stricter than `gofmt`/`go vet`; `format:check` is a hard CI gate the other three do not cover.
- Never use `git stash` — the stash stack is shared across worktrees and sessions.
- Note: `go test ./internal/gateway/ -race` has one pre-existing unrelated failure (`TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure`, tracked as #53). Ignore it.
- Never commit to `main`. `docs/superpowers/` is branch-local and removed before the PR.

---

## File Structure

**Backend — modified**
- `internal/store/migrate.go` — two columns on `model_mappings`.
- `internal/routing/store.go` — the `ModelMapping` fields, the new store-interface method, and one field on `Target`.
- `internal/store/sqlite_applications.go` + `internal/routing/memory_store.go` — the writer, without the metrics guard.
- `internal/provider/model_info.go` — `ModelInfo` gains the verdict; `parseModelInfo` reads the key.
- `cmd/gateway/app_health.go` — the context pass persists it for non-agent applications.
- `internal/routing/resolver.go` — `targetFrom` carries the verdict and the effective spec type.
- `internal/provider/live_progress.go` — `wantsLiveProgress` becomes the three-layer rule.
- `internal/gateway/agent_ingest.go` — the sample field, and the write-back.
- `internal/gateway/runtime_registry.go` — `RuntimeStatusDTO` gains the verdict.
- `internal/portal/service_model_servers.go` — `ModelServerDTO` gains it, read straight off `view.mapping`.
- `internal/gateway/passthrough_usage_scan.go` — the window floor (spec §10).

**server-agent — modified**
- `internal/collector/probe.go` — a sibling capability probe.
- `internal/agent/agent.go` — the per-child cache with its own hit tracking; probe `/props` for a `custom` child too.
- `internal/sample/sample.go` — one field beside `MetricsProbe` / `ContextProbe`.

**Frontend — modified**
- `src/api/models.ts`, `src/components/ModelServersSection.tsx`, `src/i18n.ts`.

**Docs — modified**
- `docs/architecture/cross-cutting/agent-runtime-manager.md`, `.../telemetry-usage-observability.md`, `docs/architecture/reference/api-surface.md`, `docs/architecture/reference/data-model.md`.

---

### Task 1: The persisted verdict

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go`
- Modify: `gateway/backend/internal/routing/store.go` (the `ModelMapping` block around `:607-617`; the store interface around `:977`)
- Modify: `gateway/backend/internal/store/sqlite_applications.go` (beside `UpdateMappingContextProbe`, `:269-285`)
- Modify: `gateway/backend/internal/routing/memory_store.go` (beside its counterpart, `:1003`)
- Test: `gateway/backend/internal/store/conformance_test.go`

**Interfaces:**
- Produces: `ModelMapping.LiveProgressSupport string` + `LiveProgressCheckedAt *time.Time`, and
  `UpdateMappingLiveProgressSupport(ctx, id string, support string, at time.Time) error` on the routing store. Tasks 2, 3 and 5 consume these.

- [ ] **Step 1: Write the failing conformance test**

The load-bearing assertion is the deliberate deviation, so test it against a **locked** mapping:

```go
func TestUpdateMappingLiveProgressSupportIgnoresMetricsLock(t *testing.T) {
	// seed a mapping with metrics_locked = 1, metrics_source = "benchmark",
	// a known metrics_updated_at and a known gen_tokens_per_second
	// then: UpdateMappingLiveProgressSupport(ctx, id, "supported", at)
	// assert: live_progress_support == "supported"        (the lock did NOT block it)
	//         live_progress_checked_at == at
	//         metrics_source == "benchmark"               (untouched)
	//         metrics_updated_at == the seeded value      (untouched)
	//         gen_tokens_per_second == the seeded value   (untouched)
}
```

Add a second case asserting the three accepted values round-trip and that `""` is the default on a freshly created mapping.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/store/ -run TestUpdateMappingLiveProgressSupport`
Expected: FAIL, undefined method.

- [ ] **Step 3: Add the columns**

In `migrate.go`, following the established shape (`alter table model_mappings add column ...`; see `vision_capable` at `:1465` and the nullable-timestamp handling at `:878-888`):

```
live_progress_support   text not null default ''
live_progress_checked_at  <dl.timestampType()>   -- nullable, mirroring metrics_updated_at
```

- [ ] **Step 4: Add the model fields**

In `internal/routing/store.go`'s `ModelMapping`, **below** the metrics block and with their own comment, so the separation is visible:

```go
	// LiveProgressSupport records whether this mapping's upstream tolerates the
	// live-progress request parameters (#51): "" = never determined,
	// "supported", "unsupported". It is a CAPABILITY of the upstream build, not
	// a metric, so it deliberately sits outside the MetricsLocked group above --
	// see UpdateMappingLiveProgressSupport for why.
	LiveProgressSupport   string
	// LiveProgressCheckedAt is when that verdict was last determined. Operator
	// diagnostics and the portal tooltip ONLY -- no decision logic reads it.
	LiveProgressCheckedAt *time.Time
```

- [ ] **Step 5: Add the writer**

`sqlite_applications.go`, beside `UpdateMappingContextProbe` so the contrast is visible:

```go
// UpdateMappingLiveProgressSupport records whether this mapping's upstream
// tolerates the live-progress request parameters (#51).
//
// UNLIKE every other automated writer on this table, this one carries NO
// `and metrics_locked = 0` guard and does not touch metrics_source /
// metrics_updated_at. That is deliberate: metrics_locked exists so an operator
// can pin NUMBERS THEY ANSWER FOR -- throughput, context size -- against
// automation. A build capability is not such a number: pinning it could only
// ever produce a wrong answer, and unlike a pinned throughput a wrong
// capability has an operational consequence -- the live figure silently stays
// off, with no visible reason, until someone thinks to unlock a mapping's
// metrics. And because it is a capability rather than a metric, writing it must
// not restamp the metrics provenance columns; doing so would misattribute this
// mapping's throughput figures to a capability probe.
func (s *SQLiteStore) UpdateMappingLiveProgressSupport(ctx context.Context, id, support string, at time.Time) error {
	_, err := s.exec(ctx, `
		update model_mappings
		set live_progress_support = ?, live_progress_checked_at = ?
		where id = ?`,
		support, at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping live progress support: %w", err)
	}
	return nil // 0 rows affected (missing mapping) is a benign no-op
}
```

Mirror it in `MemoryStore` and declare it on the routing store interface.

- [ ] **Step 6: Run the gates and commit**

Run: `go test ./... && golangci-lint fmt --diff && golangci-lint run`
Then verify the test fails with the guard added back (`and metrics_locked = 0`), restore, and commit.

---

### Task 2: The gateway-half detector

**Files:**
- Modify: `gateway/backend/internal/provider/model_info.go` (`ModelInfo` at `:20`, `parseModelInfo` at `:69`)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (the context pass, `:555-676`)
- Test: `gateway/backend/internal/provider/model_info_test.go`, `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: `UpdateMappingLiveProgressSupport` from Task 1.
- Produces: `ModelInfo.LiveProgressSupport string` — `""` unless the body was a llama.cpp `/props` document.

- [ ] **Step 1: Write the failing parser test**

Three cases, and the third is the one that protects vLLM:

```go
func TestParseModelInfoLiveProgressSupport(t *testing.T) {
	// (a) a /props body whose default_generation_settings.params CONTAINS the key
	//     "timings_per_token"                     -> "supported"
	// (b) the same shape WITHOUT that key          -> "unsupported"
	// (c) a vLLM /v1/models body                   -> "" (unknown)
	// (d) an Ollama /api/show body                 -> "" (unknown)
	// (e) a body with no default_generation_settings.params at all -> "" (unknown)
	// (f) unparseable bytes                        -> "" (unknown)
}
```

Case (b) must assert `"unsupported"` and cases (c)-(f) must assert `""`. If (c) ever returns `"unsupported"`, every vLLM application goes dark — that is the regression this test exists to prevent, and its name should say so.

- [ ] **Step 2: Run it, confirm it fails, then implement**

`ModelInfo` gains `LiveProgressSupport string`. In `parseModelInfo`, on the body it already decodes: locate `default_generation_settings.params`; if that object is absent, leave the field `""`. If present, set `"supported"` when it contains the key `timings_per_token` and `"unsupported"` otherwise. **Presence of the key is the signal — never its value**, which is always `false`.

- [ ] **Step 3: Persist it from the context pass**

In `app_health.go`'s context pass, at both write sites (`:634` per-model `{model}` branch, `:670` single branch), call the Task 1 writer when `info.LiveProgressSupport != ""`, and skip the call entirely when it is `""` — an unknown must not overwrite a stored verdict.

Follow the pass's existing discipline: it already runs on its own `"ctx:"+app.ID` cadence key, is skipped for an off-mesh server, and carries the app's credential.

- [ ] **Step 4: Do not rewrite an unchanged verdict**

Compare against the mapping's currently stored `LiveProgressSupport` before calling the writer. The pass runs every 30 s per application by default; without this it would issue an UPDATE per application per cadence tick forever. `writeBackRuntimeContext` in `internal/gateway/agent_ingest.go:580-600` documents this reasoning at length — follow it.

- [ ] **Step 5: Test the pass**

Assert: a `/props` body with the key persists `"supported"` once and **not again** on a second identical cycle; a vLLM-shaped body persists nothing at all; and a body that flips from with-key to without-key persists the new verdict.

- [ ] **Step 6: Gates, revert-verification, commit**

---

### Task 3: The decision rule

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (`Target`)
- Modify: `gateway/backend/internal/routing/resolver.go` (`targetFrom`)
- Modify: `gateway/backend/internal/provider/live_progress.go` (`wantsLiveProgress`, the allow-list's doc comment)
- Test: `gateway/backend/internal/provider/live_progress_test.go`

**Interfaces:**
- Consumes: `ModelMapping.LiveProgressSupport` (Task 1), `routing.EffectiveRuntimeSpecType`.
- Produces: the rule. Nothing later depends on its internals.

- [ ] **Step 1: Write the failing table test — this is the heart of the plan**

A table over every combination that can occur, asserting the decision:

```go
func TestWantsLiveProgressThreeLayerRule(t *testing.T) {
	// verdict "supported"    + provider litellm            -> true   (verdict overrides the shape)
	// verdict "unsupported"  + provider llama_cpp          -> false  (verdict overrides the shape)
	// verdict ""             + provider llama_cpp          -> true   (shape implies it)
	// verdict ""             + provider vllm               -> true
	// verdict ""             + provider llama_swap         -> false  (type says nothing about the child)
	// verdict ""             + provider litellm            -> false
	// verdict ""             + provider ollama / mock      -> false
	// verdict ""             + server_agent, effective llama_cpp -> true
	// verdict ""             + server_agent, effective vllm      -> true
	// verdict ""             + server_agent, effective tgi/ollama/custom -> false
	// verdict ""             + server_agent, spec Type "" but Binary "llama-server" -> true
	//                                        (EffectiveRuntimeSpecType resolves it)
	// AND, layered over all of the above: a memo rejection for the RouteID -> false,
	//     including when the verdict is "supported" (observation beats prediction)
}
```

The `server_agent` + empty-`Type` + `llama-server`-binary row is load-bearing: it is the case that retired the objection against a type-based gate, so a test must pin that `EffectiveRuntimeSpecType` is used and not the raw `Type`.

- [ ] **Step 2: Run it, confirm it fails, then carry the data onto `Target`**

`Target` gains two fields:

```go
	// LiveProgressSupport is the mapping's PERSISTED verdict about whether this
	// upstream tolerates the live-progress parameters: "", "supported",
	// "unsupported". See wantsLiveProgress for how it combines with the shape.
	LiveProgressSupport string
	// LiveProgressSpecType is EffectiveRuntimeSpecType(spec) for a server_agent
	// application, "" otherwise -- the only shape evidence available for a child
	// whose application type says nothing about it.
	LiveProgressSpecType string
```

Fill both in `targetFrom`, which already receives the whole `ModelMapping` by value and already loads the `RuntimeSpec` for a `server_agent` application — so this costs **no extra store lookup**. Set `LiveProgressSpecType` only for `ProviderServerAgent`.

- [ ] **Step 3: Rewrite the rule**

`wantsLiveProgress` becomes the three layers. Keep the memo consultation exactly where it is. Rewrite the allow-list's doc comment: it is no longer a fallback for *sending*, it is the "shape implies a tolerant upstream" clause, and it now has a sibling clause for `server_agent`'s effective spec type.

- [ ] **Step 4: Gates, revert-verification, commit**

Verify specifically that swapping `EffectiveRuntimeSpecType(spec)` for `spec.Type` makes the empty-Type row fail.

---

### Task 4: The agent's capability probe

**Files:**
- Modify: `server-agent/internal/collector/probe.go` (beside `ProbeContext` at `:94`)
- Modify: `server-agent/internal/agent/agent.go` (the `runtimeCtxEntry` cache; the probe call site)
- Modify: `server-agent/internal/sample/sample.go` (beside `MetricsProbe` / `ContextProbe` at `:142-143`)
- Test: `server-agent/internal/collector/probe_test.go`, `server-agent/internal/agent/agent_test.go`

**Interfaces:**
- Produces: `sample.RuntimeSample.LiveProgressSupport string`, json `live_progress_support`. Task 5 consumes it.

- [ ] **Step 1: A sibling probe, not a new case in the context chain**

`ProbeContext` is `(int, error)` and every extractor beneath it returns `(int, bool)` — a tri-state cannot ride that chain, and widening it would touch every extractor. Add a sibling that reuses the same HTTP fetch and returns the verdict, with the same evidence rule as Task 2 (only a llama.cpp `/props` document counts).

- [ ] **Step 2: Separate hit tracking in the cache**

`runtimeCtxEntry{pid, specType, contextProbePath, size}` treats a cache hit as proof the **context** probe succeeded. Two values that can succeed and fail independently need their own hit tracking — either a second field pair recording that the capability was determined, or a second cache keyed identically (pid + type + path). A cached context size must never imply a capability verdict that was never established. Test this directly.

- [ ] **Step 3: Probe `/props` for a `custom` child too**

`DeriveProbePaths` gives a `custom` spec no context path, so today nothing is fetched. This is the case a type-based rule refuses and the detector recovers, so the capability probe must GET `/props` regardless, cached on the pid exactly like the context size — one extra loopback GET per child lifetime. Without this the agent half only confirms what the effective type already guessed.

- [ ] **Step 4: The wire field**

One string on `RuntimeSample` beside the two probe states. Additive and byte-neutral: an older agent never populates it, and `""` already means unknown.

- [ ] **Step 5: Tests**

A `custom`-typed child whose `/props` carries the key reports `"supported"`; one whose `/props` lacks it reports `"unsupported"`; a child whose `/props` is unreachable reports `""`; and a cached context size does not produce a capability verdict.

- [ ] **Step 6: Gates (from `server-agent`), revert-verification, commit**

---

### Task 5: Ingest and persist the agent's verdict

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (the sample struct at `:169`, the DTO mapping at `:254`, and a write-back beside `writeBackRuntimeContext` at `:580-650`)
- Modify: `gateway/backend/internal/gateway/runtime_registry.go` (`RuntimeStatusDTO`)
- Test: `gateway/backend/internal/gateway/agent_ingest_test.go`

**Interfaces:**
- Consumes: the sample field (Task 4) and the writer (Task 1).

- [ ] **Step 1: Thread the field**

`agentRuntimeSample` gains it, `runtimeStatusDTOsFromSamples` maps it onto `RuntimeStatusDTO`.

- [ ] **Step 2: Write it back, mirroring `writeBackRuntimeContext`'s discipline exactly**

That function documents the three properties this one needs, and the plan is to copy them rather than rediscover them: the runtimes slice is length-capped before any store call; resolution is memoized per **distinct** spec id, because a full snapshot arrives roughly once a second; and **an unchanged value is not rewritten**, compared against the mapping's currently stored verdict (read once per distinct writable spec id, memoized alongside the resolution).

Two deliberate differences from that template, both of which need a comment:
- it writes through the Task 1 method, so **no `metrics_locked` check** — neither in SQL nor the in-Go pre-check `resolveRuntimeSpecMapping` performs at `:574`. A locked mapping still gets its capability updated.
- a sample whose field is `""` (an older agent, or a child not yet determined) must **skip the write entirely** rather than storing `""` over a known verdict.

- [ ] **Step 3: Tests**

Assert: a `"supported"` sample persists once and not again on an identical second sample; a `""` sample never writes; a locked mapping IS updated (the deliberate difference); and the resolution is memoized (a call-count spy over a snapshot repeating one spec id).

- [ ] **Step 4: Gates, revert-verification, commit**

---

### Task 6: Show it in the portal

**Files:**
- Modify: `gateway/backend/internal/portal/service_model_servers.go` (`ModelServerDTO`; the fill beside `ContextSize: view.mapping.ContextSize` at `:147`)
- Modify: `gateway/frontend/src/api/models.ts` (`ModelServerRow`)
- Modify: `gateway/frontend/src/components/ModelServersSection.tsx` (beside the Kontext column at `:257-274`)
- Modify: `gateway/frontend/src/i18n.ts`
- Test: `gateway/frontend/src/components/ModelServersSection.test.tsx`

- [ ] **Step 1: The wire**

`ModelServerDTO` gains `live_progress_support` (plain string, no `omitempty`) and the checked-at timestamp, both read straight off `view.mapping` — no gateway injection, because the value is persisted. This is the same reason the Kontext column's comment at `ModelServersSection.tsx:88` gives for not probe-gating it.

- [ ] **Step 2: The column**

Beside Kontext, rendered with the shared chip vocabulary:

- `"supported"` → the positive badge
- `"unsupported"` → the **neutral** badge, deliberately not the attention badge: an older llama.cpp build is not broken, it lacks a nicety, and flagging it would put a warning on every such server
- `""` → nothing rendered

Carry a comment saying this is a persisted value, not probe-derived — the distinction has already caused one bug on this surface. A tooltip states what the verdict means and, for `unsupported`, that the live figure is unavailable because this build's request schema has no such field; include the checked-at time.

- [ ] **Step 3: Tests and i18n**

A row per verdict asserting the rendered badge **by key** (`data-status`), not by colour, and that `""` renders no chip. Keys in both `de` and `en`.

- [ ] **Step 4: Frontend gates (including `format:check`), revert-verification, commit**

---

### Task 7: The passthrough window floor

**Files:**
- Modify: `gateway/backend/internal/gateway/passthrough_usage_scan.go` (the fallback's `genSecs > 0` guard)
- Test: `gateway/backend/internal/gateway/passthrough_usage_scan_test.go`

Independent of Tasks 1-6; may be done at any point.

- [ ] **Step 1: The failing test**

An Anthropic passthrough stream whose authoritative `message_delta` arrives microseconds after the first content frame must not record an implausible rate. Assert a concrete bound.

- [ ] **Step 2: Apply the same floor `liveProgressDTO` uses**

`>= 50 ms`, with a comment naming the reason: this path feeds the mapping's opportunistic throughput EWMA, which is a routing input, so a nonsense sample is not merely a display glitch. #51's final re-review raised this; it was outside that wave's scope.

- [ ] **Step 3: Gates, revert-verification, commit**

---

### Task 8: Documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md`
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md`
- Modify: `docs/architecture/reference/api-surface.md`
- Modify: `docs/architecture/reference/data-model.md`

- [ ] **Step 1: Document what ships**

Read the shipped code first; document that, not this plan. Cover: the three-layer rule and its ordering; what counts as evidence and why a non-`/props` body must be unknown (naming the vLLM regression it prevents); the persisted columns, why they sit outside `metrics_locked`, and that no decision logic reads the timestamp; the agent's extra `/props` GET for a `custom` child; and the portal column with its badge keys.

Refer to badges and states **by key, never by colour**.

- [ ] **Step 2: Verify and commit**

Run: `./scripts/check-docs.sh && sh scripts/check-docs.test.sh`

---

## Self-review notes

- **Spec coverage.** §1 → Task 3; §2 → Tasks 2 and 4 (the evidence rule is implemented twice, once per detector, and both need the vLLM-protecting test); §3 → Task 1; §4 → Task 6; §5 → Task 2; §6 → Tasks 4 and 5; §7 → Task 3; §8 → distributed; §9 → distributed; §10 → Task 7; §11 → nothing, by design.
- **The evidence rule lives in two places** (the gateway parser and the agent probe) and must not drift. Task 4 Step 1 says "the same evidence rule as Task 2"; if a reviewer finds them diverging, that is a real finding, and a shared helper is the fix if one can cross the module boundary — the agent and the gateway are separate modules, so duplication with a pointing comment may be the honest answer.
- **Type consistency.** `LiveProgressSupport` is the field name in `ModelMapping`, `ModelInfo`, `Target`, `RuntimeSample`, `RuntimeStatusDTO` and `ModelServerDTO`; `live_progress_support` is the json/column name throughout. The three values are `""`, `"supported"`, `"unsupported"` everywhere.
- **Known ripple.** Adding a required field to `ModelServerRow` in TypeScript will break fixtures that build one; Task 6 owns that.
