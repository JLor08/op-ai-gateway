# Decisions follow-ups (#384) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clear issue #384: the code follow-ups and cosmetic/robustness items left over from the reviews of #373 Phases 1–4.

**Architecture:** Small, independent fixes in the decision handlers, the compat translators, the portal folds and offering, the vllm-sr version surface, plus tests and docs. No migration and no agent change.

**Tech Stack:** Go (gateway/backend), React + TypeScript + Vitest (gateway/frontend).

**Spec:** `docs/superpowers/specs/2026-11-12-decisions-followups-384.md` (the issue body). File references in it are at `main` @ `f35484f1`.

## Global Constraints

- Work on branch `fix/decisions-followups-384`. Never commit to `main`. Commit messages are English, conventional style, with a `(#384)` suffix and no attribution lines.
- No LAN host names. Code and docs never cite `docs/superpowers/`; cite ADR-097…100 where a reference is needed.
- No migration. No server-agent change.
- Go: golangci-lint v2.15.0 clean. Each touched or new function stays at cognitive complexity ≤ 15. Frontend: `npm test`, `npm run build`, `npm run lint`, `npm run format:check`.
- Known local-only failures that are not to be chased: `TestVoice*`, `TestLiveTranscriptionLoopback*`, the load flake `TestVRAMRunUnstableWindowsAreInconclusive`, and the `make lint-docs` agent-extractor line (#377).
- Behavior changes visible to clients must stay fail-closed. Nothing may widen vendor reach.

## Controller rulings (decided up front)

- **Item 2** (no re-selection for a `server_agent` candidate after the post-resolve route check) and **item 4** (a group whose members all lack a route answers `routing.no_model_route`) are **documented as intended** in ADR-100, not changed. Both are fail-closed. Re-selection would mean a new exclude-and-re-resolve loop in two handlers for a configuration-error case.
- **Item 7** (two sequential 30 s probes on a blackholed host) is **documented**. The pass is detached and guarded per application.
- **Empty choice value:** a choice value `""` is refused on the request side with `decisions.question_invalid`. It has no meaning as an option name, and System One rejects empty option names too.
- **Empty input:** `input: []`, or messages yielding no text, no JSON and no images, is refused with `decisions.input_unsupported`, message "input carries no content".
- **The 422 `systemone.refused` error type** becomes `refusal`, a new `decisionsRefusalErrorType`. It is no longer `upstream_error`.
- **The vllm-sr supported major** reaches the portal as a model-servers row field `runtime_api_supported_major` (int, only set for `vllm_sr` rows). The portal interpolates it; the text no longer hard-codes 5.
- **`refuseIncapableDecisionModes`** reports both errors when both modes are invalid. It joins them with `errors.Join`, and the HTTP mapping uses the first sentinel's status and code. The message names both.
- **Helper renames** in app_health.go: `logSystemOneNoVerdict` → `logDecisionProbeNoVerdict`; `systemOneRetryBackoff` → `decisionProbeRetryBackoff`; `systemOneRefusedBackoffCap` → `decisionProbeRefusedBackoffCap`. Pure renames.

## Review Focus

1. The redirect hint (item 3) must name only a model the resolver's route-aware filter would accept for that endpoint. It must never add a vendor model beyond today's rules.
2. A refusal row recorded after resolve (item 1) must carry the resolved target's server and route. A refusal before resolve still records an empty target.
3. Score `value` `1.0` must be accepted. Score `value` `1.5` must be refused.
4. A non-default `priority` written in exponent form (`1e-400`) must be refused. `0` and `0.0` still count as absent.
5. Every renamed helper and constant leaves no stale reference in code, tests or docs.

---

### Task 1: Gateway — refusal rows, refusal error type, 5.0 envelope test, test gaps

**Files:** `gateway/backend/internal/gateway/systemone_handler.go` (`recordDecisionsRefusal`, `refuseDecisionsRequest`, `recordDecisionsRoutingFailure` call sites), `decisions_handler.go`, `decisions_response.go` (error type), `native_passthrough.go` / `decisions_response.go` (normalizing passthrough `Content-Type`), tests in `decisions_handler_test.go`, `systemone_handler_test.go`, `decisions_response_test.go`.

- [ ] **Step 1: Write failing tests.**
  - A post-resolve refusal records the resolved target's `ServerID` and `RouteID` (or the row fields recordUsage derives from them). Cover `decisions.endpoint_disabled` / `systemone.endpoint_disabled` via a server_agent spec, the post-resolve `routing.model_not_capable` and a translator refusal.
  - A pre-resolve routing failure still records an empty target.
  - 422 `systemone.refused` carries `type: "refusal"`.
  - A flat vllm-sr 5.0 529 body `{"detail":"overloaded","code":"overloaded"}` through translate gives 529 `decisions.upstream_error`, and the message contains `overloaded`.
  - The 502 over-bound and unreadable paths assert their usage row: error code, status 502, no tokens.
  - A body that breaks off mid-read (a test server that closes early) gives 502 with nothing of the body written.
  - A normalizing passthrough whose body is unchanged keeps the upstream's `Content-Type`. When the body is rewritten it is `application/json`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run 'Decisions|SystemOne'`. Expect FAIL.
- [ ] **Step 3: Implement.** Thread the resolved target into the refusal recorder where it exists.
- [ ] **Step 4: Run** the focused tests, `go test ./internal/gateway/ -skip 'TestVoice|TestLiveTranscriptionLoopback'`, and lint. Expect PASS.
- [ ] **Step 5: Commit** `fix(gateway): attribute decision refusals to their target and type the System One refusal (#384)`

### Task 2: Compat translators

**Files:** `gateway/backend/internal/compat/decisions.go`, `decisions_reverse.go`, and their tests.

- [ ] **Step 1: Write failing tests.**
  - Score probability `value` `1.0` is accepted; `1.5` and `"1"` are refused (reverse direction, `decisions_reverse.go` ~:733).
  - A choice value `""` on the request is refused as `decisions.question_invalid`.
  - The "names no choice" sentinel logic no longer depends on the empty string: a presence flag.
  - An `input_text` instruction part without `text` reports "an input_text part has no text".
  - A non-string choice `description` reports "choice N description must be a string".
  - `input: []`, and messages with no content pieces, are refused as `decisions.input_unsupported` with "input carries no content".
  - `isDefaultJSON`: `priority: 1e-400` is refused and `0` / `0.0` count as absent.
  - The reverse refusal test covers multiple refused keys.
  - A raw-byte test checks answer order when the upstream reorders named answers.
  - A positional-fallback test covers an answer whose name matches no question.
- [ ] **Step 2: Run** `go test ./internal/compat/`. Expect FAIL.
- [ ] **Step 3: Implement.** Bring `foldNoulCriteria` below 15 with a small helper.
- [ ] **Step 4: Run** `go test ./internal/compat/` and lint. Expect PASS.
- [ ] **Step 5: Commit** `fix(compat): tighten decisions translation edge cases (#384)`

### Task 3: Portal — route-aware redirect hint and fold consolidation

**Files:** `gateway/backend/internal/portal/service_model_offering.go` (`capableNames`, `addVendorDecisionCandidates`), `internal/portal/service.go` (`decisionFlagsByName`, `decisionOpenAIFlagsByName`, `anyDecisionFlagsByName`, the `ModelDTO.DecisionOpenAI` doc comment), and tests.

**Interfaces:** reuse `routing.EffectiveDecisionsRoute` / `routing.EffectiveSystemOneRoute` and the resolver's semantics (`decisionRouteServes` in routing/resolver.go). If it is unexported, export a small `routing.DecisionRouteServes(kind, flavor string, app/spec modes, decision, decisionOpenAI bool) bool`, or reuse the existing helper, so the portal and the resolver share one rule.

- [ ] **Step 1: Write failing tests.**
  - For `/v1/systemone` and `/v1/decisions` (the `ModelOffering` Capable / redirect), an ordinary mapping with a forced mode lacking its verdict is NOT capable.
  - An auto mapping with the matching verdict is capable.
  - A server_agent mapping follows its spec's effective modes.
  - Vendor decision candidates are unchanged.
  - The three folds give the same results as before for the existing cases. Pin them with the current tests.
- [ ] **Step 2: Run** `go test ./internal/portal/ -run 'Offering|Capable|Decision|Redirect'`. Expect FAIL.
- [ ] **Step 3: Implement.** Consolidate the three folds into one loop helper parameterized by the per-view predicate. Fix the `DecisionOpenAI` doc comment.
- [ ] **Step 4: Run** `go test ./internal/portal/ ./internal/gateway/ -skip …`. Expect PASS. Lint clean.
- [ ] **Step 5: Commit** `fix(portal): route-aware decision redirect hint and one decision fold (#384)`

### Task 4: vllm-sr supported major and portal notes

**Files:** `gateway/backend/internal/portal/service_model_servers.go` and the row builder (`internal/gateway/portal_model_endpoints.go` per Phase 3), `gateway/frontend/src/api/*.ts` (the model-servers row type), `ModelServersSection.tsx`, `i18n.ts`, `api/models.ts` (drop the unused `decision_openai` field on the model type, or use it — ruling: drop it), the decisions-mode note (`applicationDecisionsModeNote`, DE+EN: add that an OpenAI api-key account serves `gpt-6-luna` on both decision endpoints), and tests.

- [ ] **Step 1: Write failing tests.**
  - Backend: a `vllm_sr` row carries `runtime_api_supported_major: 5` (equal to `routing.VllmSRSupportedAPIMajor`). Other rows omit it.
  - Frontend: the tooltip and the spec note interpolate the row's or the constant's major. Ruling: the spec note may use a frontend constant fed from a new `/api/portal/…` field only if one exists. Otherwise keep the note generic ("the supported major") and show the number only in the row tooltip.
  - The i18n keys exist in both languages.
  - The `decision_openai` field is gone from the model type, and tsc is clean.
- [ ] **Step 2: Run** the focused Go and vitest files. Expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go focused tests and the four frontend gates. Expect PASS.
- [ ] **Step 5: Commit** `fix(portal): show the supported vllm-sr major from the gateway (#384)`

### Task 5: Portal, probe and test hygiene

**Files:**
- `internal/portal/service_applications.go` (`refuseIncapableDecisionModes` + both HTTP endpoint mappings);
- `internal/portal/service_vendor_discovery.go` (document that the re-added decision row sits outside `maxDiscoveredModels`, in its comment);
- `cmd/gateway/app_health.go` (the renames in the rulings);
- test-name leftovers: `internal/store/conformance_test.go` ~:634 (`cec@example.test` → a decisions-specific address), `internal/portal/service_token_test.go` ~:709/:819/:881 (`effort-off` → `normalize-off` or similar), and their misdescribing comments;
- `internal/routing/live_timings.go` (doc comment counts: name the strings or verify the count).

- [ ] **Step 1: Write failing tests** for `refuseIncapableDecisionModes` with both modes invalid. The error names both modes, and the HTTP code and status are the first sentinel's.
- [ ] **Step 2: Run.** Expect FAIL.
- [ ] **Step 3: Implement** the renames and test-name fixes. `git grep` must find no stale name.
- [ ] **Step 4: Run** `go test ./internal/portal/ ./internal/store/ ./cmd/gateway/ ./internal/routing/` and lint. Expect PASS.
- [ ] **Step 5: Commit** `chore: decision probe naming, test hygiene and both mode errors (#384)`

### Task 6: Docs

**Files:** `docs/architecture/09-architecture-decisions.md` (ADR-100: confirm items 2, 4, 7 as intended, with one sentence each), `cross-cutting/compatibility-and-inference.md` (§3.9/§3.10: the refusal type `refusal`; the empty-input and empty-choice refusals; score `value` accepted as an integral number; the redirect hint is now route-aware), `reference/openapi.yaml` (the `systemone.refused` type, if documented), `reference/api-surface.md` (the `runtime_api_supported_major` row field; error rows), `cross-cutting/agent-runtime-manager.md` or `routing-and-model-selection.md` (the probe naming and the two-probes note).

- [ ] **Step 1:** Write every claim from the code as merged in Tasks 1–5.
- [ ] **Step 2: Run** `make lint-docs`. Only the #377 line and the branch-local plan reference may remain.
- [ ] **Step 3: Commit** `docs(architecture): decisions follow-ups from #384 (#384)`
