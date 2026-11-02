# Co-Residency Rework Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the manual triangular co-residency matrix with a
resource-first model — per-server opt-in/opt-out mode, tri-state per-pair
rules, role-aware grace-protected eviction, a derived model role, and a
role-grouped editor.

**Architecture:** Extend the shipped admission pipeline (`admission` struct
in `policy.go`) and the RAM/VRAM accounting (#359/#360). Co-residency stays
backward-compatible on the wire: the gateway computes the effective
allowed-pair list from mode+verdicts into the existing `coresident` field;
the only new wire datum is a per-spec **role**, additive and degrading to
LRU on older agents. A single derived role is both the UI group and the
eviction "type".

**Tech Stack:** Go (modules `server-agent`, `gateway/backend`), SQLite +
PostgreSQL (one `SQLStore` via a dialect seam), TS/React/MUI + Vitest
(`gateway/frontend`).

**Spec:** `docs/superpowers/specs/2026-11-02-coresidency-rework-design.md`
(read it — the plan argues from it).

## Global Constraints

- Feature branch + PR only; never commit to or merge `main`.
- All repo-facing text (PRs, issues, commits, docs, code comments) in
  **English**; no LAN host names anywhere in repo text.
- No attribution lines in commit messages or PR descriptions.
- Substantive commit-message bodies (squash-merge pre-fills from the body).
- Go: `golangci-lint fmt --diff` + `run` per module; `GOOS=windows go vet`
  where platform code is touched; full `go test ./...` **including the
  Postgres leg** for any store/migration change.
- Frontend: `vitest`, `tsc`, `eslint`, `prettier format:check`.
- `check-docs`; local SonarQube `sonar-gate` + `sonar-branch-findings`
  (judge by branch-attributed findings).
- **One** ServerAgent `Version` bump for the whole branch: 0.14.0 → 0.15.0.
- `docs/superpowers/` is branch-local and removed before the PR; permanent
  docs live under `docs/architecture/`.

## Review Focus

Input classes / failure modes the spec implies but that no single task's
happy-path test covers — each is pinned to a task below:

- **Opt-out on a pre-0.15.0 agent** must still co-locate: the gateway must
  emit the fully-enumerated effective allow-list, not rely on the new role
  field → Task 2.4.
- **Never evict an in-use or within-grace model**, even at maximum pressure;
  when nothing evictable frees enough, the start **waits** (no OOM, no
  breaking an active pipeline) → Task 3.3.
- **A large model needing more than all idle victims can free** waits rather
  than evicting an in-use model → Task 3.3.
- **Mode switch preserves explicit verdicts** (lossless) and recomputes the
  effective list correctly → Task 2.4.
- **Migration leaves existing servers behaving exactly as today** (existing
  pairs → `allow`, existing apps → `opt_in`; no sudden co-location) → Task
  2.1.
- **A model with no recognized role** (`other`) groups and evicts sanely →
  Tasks 1.2 / 3.3.

---

## File Structure

- `gateway/backend/internal/routing/` — role derivation, `CoResidencyRule`
  verdict, `RuntimeSpec`/DTO role, co-residency mode on the application.
- `gateway/backend/internal/store/migrate.go`, `sqlite_runtime.go` —
  migrations + read/write for verdict + mode.
- `gateway/backend/internal/portal/service_runtime.go`,
  `service.go`, `service_applications.go` — DTOs, mode/verdict API,
  capability vocabulary, `ModelDTO` role.
- `gateway/backend/internal/gateway/` — effective allow-list build in the
  agent runtime config; per-spec role on the pushed config; agent-features.
- `server-agent/internal/runtime/` — `types.go` (spec role), `policy.go` +
  `manager.go` (grace window, role-aware victim ordering).
- `server-agent/internal/config/config.go` — `OP_AGENT_EVICTION_GRACE_SECONDS`.
- `gateway/frontend/src/` — `api/runtime.ts`, new `CoResidencyEditor.tsx`
  (replaces `RuntimeMatrix.tsx`), capability toggles in `MappingForm.tsx` /
  `ModelServersSection.tsx`, i18n.
- `docs/architecture/` — ADRs + chapter updates.

---

## Phase 1 — Model role + embedding/reranker capability + operator UI

### Task 1.1: Add `embedding` / `rerank` capabilities + ModelDTO booleans

**Files:**
- Modify: the capability-name constants block
  (`gateway/backend/internal/routing/store.go`, the `~1552-1678` vocabulary).
- Modify: `gateway/backend/internal/portal/service.go` (`ModelDTO`: add
  `Embedding`, `Rerank` booleans alongside `Vision`/`Image`/…).
- Test: the corresponding `_test.go` for the capability/DTO mapping.

**Interfaces:**
- Produces: capability constants `CapabilityEmbedding = "embedding"`,
  `CapabilityRerank = "rerank"`; `ModelDTO.Embedding bool`,
  `ModelDTO.Rerank bool`.

- [ ] **Step 1:** Write a failing test asserting the two new constants exist
  and that a `CapabilityRow{Capability:"embedding",Verdict:"yes"}` maps to
  `ModelDTO.Embedding == true` (and `rerank` → `Rerank`).
- [ ] **Step 2:** Run it; expect FAIL.
- [ ] **Step 3:** Add the constants and extend the DTO mapping (mirror the
  existing `Speech`/`Transcription` wiring; the verdict write-path already
  accepts any name, so no allowlist change is needed).
- [ ] **Step 4:** Run tests; expect PASS.
- [ ] **Step 5:** Commit.

### Task 1.2: Derive a single model role

**Files:**
- Create: `gateway/backend/internal/routing/model_role.go`
- Test: `gateway/backend/internal/routing/model_role_test.go`

**Interfaces:**
- Produces: `type ModelRole string` with
  `RoleASR, RoleTTS, RoleImage, RoleEmbedding, RoleReranker, RoleChat,
  RoleOther`; `func DeriveRole(specType RuntimeSpecType, caps
  map[string]bool) ModelRole`.
- Consumes: `EffectiveRuntimeSpecType`, the capability verdicts for a model.

Derivation (first match wins): `transcription`→ASR; `speech`→TTS;
`image`||`image_edit`→Image; `embedding`→Embedding; `rerank`→Reranker;
else LLM-server type (`vllm`/`llama_cpp`/`tgi`/`ollama`) or chat-ish caps
→Chat; else→Other.

- [ ] **Step 1:** Write failing table-tests: one per role incl. the
  `transcription`+`speech` tie-break → ASR, an unknown/`audio_cpp`-only
  model, and a bare `stable_diffusion_cpp` → Image. **(Review Focus: `other`
  role.)**
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement `DeriveRole` with the priority order.
- [ ] **Step 4:** Run; expect PASS.
- [ ] **Step 5:** Commit.

### Task 1.3: Surface `role` on the model / spec DTOs

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (`ModelDTO.Role`),
  `service_runtime.go` (`AgentRuntimeSpecDTO.Role`); wire `DeriveRole` where
  the DTOs are assembled.
- Test: the DTO-assembly test.

**Interfaces:**
- Produces: `ModelDTO.Role string`, `AgentRuntimeSpecDTO.Role string`
  (one source of truth consumed by the editor grouping and, via the pushed
  config, by the agent).

- [ ] **Step 1:** Failing test: a spec/model with `transcription:yes`
  serialises with `role == "asr"`.
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Add the fields; populate from `DeriveRole`.
- [ ] **Step 4:** Run; PASS.
- [ ] **Step 5:** Commit.

### Task 1.4: Operator UI — mark a model embedding / reranker

**Files:**
- Modify: `gateway/frontend/src/components/MappingForm.tsx` (add two
  capability toggles mirroring `vision`/`speech`),
  `ModelServersSection.tsx` (extend the fixed capability list), i18n files.
- Test: `MappingForm` test asserting the two toggles render and submit the
  `embedding`/`rerank` verdicts.

- [ ] **Step 1:** Failing component test: toggling "Embedding" includes an
  `embedding` verdict in the submitted payload (and `rerank` likewise).
- [ ] **Step 2:** Run vitest; FAIL.
- [ ] **Step 3:** Add the toggles + state + i18n keys.
- [ ] **Step 4:** Run vitest + `tsc` + `eslint` + `prettier`; PASS.
- [ ] **Step 5:** Commit.

---

## Phase 2 — Co-residency mode + tri-state verdict (persistence, DTO, effective list)

### Task 2.1: Migrations — pair `verdict` + per-application `coresidency_mode`

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (two new forward-only
  migrations — use the next free numbers after the current highest, expected
  **98** then **99**; verify in-file).
- Test: migration tests (sqlite **and** the Postgres leg).

Migration A: `ALTER TABLE agent_coresidency_rules ADD COLUMN verdict`
(text, values `allow`/`forbid`), **backfill existing rows to `allow`**
(use `addColumnIfMissing` + an UPDATE, matching the shipped pattern).
Migration B: add `coresidency_mode` per application (column or settings key
following the existing per-application settings pattern — placement per
design §6), **existing applications → `opt_in`**, new default `opt_out`.

- [ ] **Step 1:** Failing test: after migrating a DB that had a pre-existing
  co-residency row and application, the row has `verdict='allow'` and the
  application has `coresidency_mode='opt_in'`. **(Review Focus: no-surprise
  migration.)**
- [ ] **Step 2:** Run (sqlite + Postgres leg); FAIL.
- [ ] **Step 3:** Add both migrations.
- [ ] **Step 4:** Run; PASS (both dialects).
- [ ] **Step 5:** Commit.

### Task 2.2: Store + types — verdict + mode read/write

**Files:**
- Modify: `routing/store.go` (`CoResidencyRule.Verdict`; a `CoResidencyMode`
  type + accessor on the application), `store/sqlite_runtime.go`
  (`SetCoResidencyRules` carries verdict; get/set mode).
- Test: round-trip store tests (sqlite + Postgres leg).

**Interfaces:**
- Produces: `CoResidencyRule{…, Verdict string}`; `GetCoResidencyMode(appID)`
  / `SetCoResidencyMode(appID, mode)`; `SetCoResidencyRules` persists
  verdicts (still full-replace).

- [ ] Steps: failing round-trip test (write allow+forbid pairs and a mode,
  read them back) → FAIL → implement → PASS → commit.

### Task 2.3: Portal DTO + API — mode + per-pair verdict

**Files:**
- Modify: `portal/service_runtime.go` (`CoResidencyDTO`/`SetCoResidencyRequest`
  gain `mode` + per-pair `verdict`; `GetCoResidency`/`SetCoResidency`
  validation — reject unknown mode/verdict, keep the empty/identical/foreign
  checks).
- Test: service test for the DTO round-trip + validation.

- [ ] Steps: failing test (set mode=opt_out with a forbid pair; read back) →
  FAIL → implement → PASS → commit.

### Task 2.4: Gateway — effective allow-list build (backward-compatible)

**Files:**
- Modify: `gateway/backend/internal/gateway/` config build (the
  `AgentRuntimeConfig` path, `service_runtime.go:~2477/2586`): compute the
  effective allowed pairs from mode + verdicts and emit into the existing
  `Config.Coresident [][2]string`.
- Test: effective-list unit tests.

**Interfaces:**
- Produces: `effectiveAllowedPairs(mode, specs, rules) [][2]string` =
  opt-out → every enabled spec pair minus `forbid` pairs; opt-in → `allow`
  pairs only.

- [ ] **Step 1:** Failing tests: (a) opt-out with one forbid over 4 specs →
  5 of 6 pairs; (b) opt-in → only explicit allows; (c) **flipping mode does
  not change the stored verdicts**, only the computed list. **(Review Focus:
  opt-out-on-old-agent + lossless mode switch.)**
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Implement the computation; feed the existing `coresident`.
- [ ] **Step 4:** Run; PASS.
- [ ] **Step 5:** Commit.

---

## Phase 3 — Agent: role-aware, grace-protected eviction

### Task 3.1: Carry `role` to the agent (additive wire field)

**Files:**
- Modify: the pushed config spec entry (gateway side, set from
  `AgentRuntimeSpecDTO.Role`); `server-agent/internal/runtime/types.go`
  (`Spec`/snapshot spec gains `Role string`, JSON-optional).
- Test: config build includes role; agent unmarshals it (and tolerates its
  absence).

- [ ] Steps: failing test (pushed spec carries `role`; agent snapshot
  exposes it; missing role → `RoleOther`/empty, no error) → FAIL →
  implement → PASS → commit.

### Task 3.2: Grace window in evictability

**Files:**
- Modify: `server-agent/internal/config/config.go`
  (`OP_AGENT_EVICTION_GRACE_SECONDS`, default **30**, env-only; confirm the
  knob pattern against neighbours); `policy.go`/`manager.go` `isEvictable`
  (add `now − LastUsed > grace`).
- Test: policy test.

- [ ] **Step 1:** Failing test: a model used within the grace window is NOT
  evictable even under pressure; outside it, it is. **(Review Focus:
  within-grace protection.)**
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Thread the grace into the snapshot/clock and `isEvictable`
  (reuse the policy's existing clock seam).
- [ ] **Step 4:** Run (`-race`); PASS.
- [ ] **Step 5:** Commit.

### Task 3.3: Role-aware victim ordering

**Files:**
- Modify: the victim selection used by the resource rules in `policy.go`
  (today `sortOldestFirst`): order idle victims by
  `(role == candidate.role ? 0 : 1, LastUsed asc)`.
- Test: policy tests.

- [ ] **Step 1:** Failing tests pinning the scenarios: (a) a new ASR with a
  running idle ASR + idle TTS evicts the **ASR**, not the TTS (voice-chat
  protection); (b) a large LLM needing much room evicts idle models across
  roles, oldest-first; (c) when the only way to fit would evict an in-use /
  within-grace model, the admission **waits** (no eviction, no OOM). **(Review
  Focus: cross-role under pressure + never-evict-in-use.)**
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Implement the ordering key; keep the shipped resource
  arithmetic and wait/wake path untouched.
- [ ] **Step 4:** Run (`-race`) + the full existing `TestAdmit*`/eviction
  suite unchanged; PASS.
- [ ] **Step 5:** Commit.

### Task 3.4: Version bump + feature flag

**Files:**
- Modify: ServerAgent `Version` 0.14.0 → **0.15.0** (one bump for the
  branch); `agent.Features` += `runtime_role_eviction` (Since 0.15.0);
  gateway constant in `gateway/backend/internal/gateway/agent_features.go`
  (NOT added to `gatewayAgentFeatures`).
- Test: feature-declaration test.

- [ ] Steps: failing test (agent advertises `runtime_role_eviction`; gateway
  constant exists) → FAIL → implement → PASS → commit.

---

## Phase 4 — Frontend: role-grouped co-residency editor

### Task 4.1: API client — mode + verdicts + role

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (co-residency get/put carry
  `mode` + per-pair `verdict`; model/spec type gains `role`).
- Test: api client test (serialisation).

- [ ] Steps: failing test → FAIL → implement → PASS → commit.

### Task 4.2: `CoResidencyEditor` component

**Files:**
- Create: `gateway/frontend/src/components/CoResidencyEditor.tsx` (+ test).
- Modify: `RuntimeAdminSection.tsx` to render it in place of
  `RuntimeMatrix` (live mode); i18n keys.

Behaviour (design §4.4): master-detail (searchable model list with exception
counts | selected model's relationships grouped by role); mode toggle at the
top; group-level tri-state bulk-set + per-member override with an inheritance
hint and a "mixed" marker; collapsible groups; long names truncated + full
on hover, selected model shown in full, control pinned right; **draggable
divider** persisting its width (localStorage, wrapped in try/catch). A
toggle edits one symmetric pair; save sends the full set + mode.

- [ ] **Step 1:** Failing component tests: (a) setting a member to "forbid"
  emits a canonical pair with `verdict:"forbid"`; (b) a group bulk-set marks
  all members and shows "mixed" after one member is overridden; (c) switching
  the mode toggle re-labels "default" without clearing explicit cells.
- [ ] **Step 2:** Run vitest; FAIL.
- [ ] **Step 3:** Build the component (reuse `RuntimeAdminSection` table/row
  primitives where possible).
- [ ] **Step 4:** Run vitest + `tsc` + `eslint` + `prettier`; PASS.
- [ ] **Step 5:** Commit.

### Task 4.3: Retire `RuntimeMatrix`

**Files:**
- Delete: `RuntimeMatrix.tsx` + its test once no longer imported; remove the
  file-mode render path; prune dead i18n.
- Test: existing suite stays green; a grep shows no remaining import.

- [ ] Steps: remove → run full frontend gates → PASS → commit.

---

## Phase 5 — Permanent documentation

### Task 5.1: ADRs + chapter updates

**Files:**
- `docs/architecture/09-architecture-decisions.md` — new ADR(s): per-server
  mode + tri-state co-residency; derived model role; role-aware
  grace-protected eviction (Context/Decision/Rejected/Consequence).
- `docs/architecture/cross-cutting/agent-runtime-manager.md` — §5 (mode +
  effective list, role) and §6/eviction (grace window, role-aware order).
- `reference/config-env.md` — `OP_AGENT_EVICTION_GRACE_SECONDS`.
- `reference/data-model.md` — migrations 98/99, the `verdict` column, the
  `coresidency_mode` setting; `embedding`/`rerank` capabilities.
- `reference/api-surface.md` — mode + verdict on the co-residency DTO,
  `role` on the model/spec DTO, `runtime_role_eviction` (Since 0.15.0).

- [ ] Steps: write docs → `check-docs` + grep for stale `RuntimeMatrix` /
  default-deny wording → reconcile → commit (single docs commit, English,
  no attribution, no host names).

---

## Self-Review

- **Spec coverage:** mode+tri-state (P2), resource-driven role-aware
  grace-protected eviction (P3), derived role + embedding/reranker + UI
  (P1/P1.4), new editor (P4), migration default + backward-compat (P2.1/2.4),
  docs (P5). All §2 goals mapped.
- **Type consistency:** `ModelRole`/`role` string is one concept across
  backend derivation (1.2), DTOs (1.3), wire (3.1), eviction (3.3), UI
  grouping (4.2). `verdict` (`allow`/`forbid`) consistent across store (2.2),
  DTO (2.3), effective list (2.4), editor (4.2).
- **Review Focus:** each line has an owning task's test (2.4, 3.3, 2.1,
  1.2/3.3).
- **Proportion:** the plan pins decisions (signatures, enum values, test
  scenarios, file paths) and leaves bodies to the implementer, per design §6.
- **Anti-goals:** the never-evict-in-use/within-grace and no-OOM behaviours
  are pinned by Task 3.3's waiting test; opt-out-no-surprise by 2.1.
