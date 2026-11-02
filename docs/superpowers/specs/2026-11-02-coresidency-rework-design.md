# Co-Residency Rework: Opt-out Default, Resource-Driven Role-Aware Eviction, and a Role-Grouped Editor — Design

**Status:** Draft for approval (brainstormed 2026-11-02).
**Kind:** Project design document. It feeds a phased implementation plan
(`superpowers:writing-plans`), then subagent-driven development — the same
flow used for the RAM resource-accounting feature (#359/#360).

---

## 1. Intent & Why

The server-agent's co-residency control is a **manual N×N triangular
matrix**: an operator must explicitly list every pair of models allowed to
run together, and the rule is **default-deny** (a pair may not co-reside
unless listed). At the user's current scale (~20 models) that is ~190 cells
and has become unmanageable and hard to read.

Two things make this the right moment to rework it:

1. The scheduler now **measures and accounts for host RAM and per-GPU
   VRAM** (shipped in #359/#360). The matrix originally existed *because*
   system RAM was not computable; that reason is now gone. Capacity is a
   computed fact, not something an operator must encode pairwise.
2. The user runs composite pipelines (voice chat = ASR + TTS; later RAG =
   chat + embedding) where *which* model gets evicted under pressure matters
   more than a static allow-list.

So the rework inverts the model: **resource fit becomes the primary gate,
and the manual matrix becomes the rare exception.**

- A new per-server **mode**: *opt-in* (today's default-deny) or **opt-out
  (the new default)** — everything may co-reside unless explicitly
  forbidden, always subject to resources.
- Placement is **resource-driven**: a model starts if it fits; if not, idle
  models are **evicted by intelligent rules** — same functional role first
  (role swap), then least-recently-used — and **never** a model that is
  in use or was used very recently.
- The triangular matrix UI is replaced by a **role-grouped, per-model
  editor** that stays legible at 20+ models.

## 2. Goals / Non-Goals / Anti-Goals

### Goals
- Per-server **co-residency mode** (opt-in / opt-out), opt-out the default.
- **Tri-state per pair**: explicit *allow* / explicit *forbid* / *default*;
  *default* resolves by the mode (opt-out → allow, opt-in → deny). Explicit
  settings survive a mode switch unchanged. Pairs stay symmetric.
- **Resource-driven admission** reusing the shipped RAM/VRAM accounting,
  with **role-aware, grace-protected eviction** of idle models.
- A **single derived role per model** that is simultaneously the UI group
  and the eviction "type", derived from the model's capabilities and
  auto-extending as new capabilities appear.
- **Embedding and reranker as first-class roles**, built in now (no such
  model runs today, but they are coming), so they are protected distinct
  roles beside chat LLMs.
- A **new editor UI** (role-grouped master-detail) replacing the triangular
  matrix, scalable to many models and robust to very long model names.

### Non-Goals
- Changing the RAM/VRAM measurement or the resource budgets themselves
  (reuse #359/#360 as-is).
- Cross-server placement/scheduling. Co-residency is **per server-agent**;
  nothing here moves work between servers.
- Auto-**probing** whether a model is an embedding/reranker. The role is
  recognized from a capability flag that is operator-settable now; an
  automatic probe is deferred (future work).
- Reworking the admission pipeline's structure (it was decomposed in #360);
  we extend it, not restructure it.

### Anti-Goals (a failure even if it technically works)
- **Evicting an in-use or recently-used model.** A model serving a request,
  or used within the grace window, is never a victim — breaking an active
  voice chat / RAG pipeline is the thing to prevent.
- **OOM-ing a neighbour or the agent** (inherited hard anti-goal from the
  RAM feature).
- **Surprising existing servers.** Turning opt-out on must not make a
  running, matrix-restricted server suddenly co-locate everything.
- An editor that is still unmanageable at scale.

## 3. Current State (grounded — verified in code, 2026-11-02)

- **Matrix storage:** `agent_coresidency_rules(application_id,
  mapping_a_id, mapping_b_id, created_at)` — a plain **symmetric pair
  table**, per application, PK on all three, FKs `on delete cascade`
  (`gateway/backend/internal/store/migrate.go`), full-replace writes
  (`SetCoResidencyRules`, `gateway/backend/internal/store/sqlite_runtime.go`).
  Go type `CoResidencyRule{ApplicationID, MappingAID, MappingBID, CreatedAt}`
  with canonical `MappingAID < MappingBID` (`routing/store.go`). **Presence
  of a row = allowed; absence = denied** (default-deny).
- **Admission:** `ruleMatrix` in `server-agent/internal/runtime/policy.go`
  reads `PolicySnapshot.Allowed map[[2]string]bool`, built from the pushed
  `Config.Coresident [][2]string` via `AllowedPairs()` / `PairKey`
  canonicalisation. For each running process incompatible with the
  candidate, it must be evicted (if idle/evictable) or the admission
  blocks/waits. Symmetric, irreflexive. The admission pipeline is the
  `admission` struct (decomposed in #360). Resource rules (per-GPU VRAM,
  host-RAM pool) and eviction primitives shipped in #359:
  `isEvictable = !Pinned && !Starting && InFlight == 0`, `sortOldestFirst`
  (oldest `LastUsed` first).
- **Model classification:** there is **no single role field**.
  `RuntimeSpec.Type` is the *runtime-server kind*
  (`vllm`, `llama_cpp`, `tgi`, `ollama`, `stable_diffusion_cpp`,
  `audio_cpp`, `custom`; `""` → auto-detect). Functional abilities live as
  **additive yes/no capabilities** in `model_mapping_capabilities`
  (`CapabilityRow{Capability, Verdict, Source, CheckedAt}`, verdict
  `yes`/`no`, absence = unknown): `vision`, `video`, `audio`, `tools`,
  `mtp`, `image` (generates), `image_edit`, `speech` (TTS), `transcription`
  (ASR), `transcription_streaming`, `speech_clone`, `speech_streaming`, …
  **No `embedding`, no `rerank`.** `/v1/models` exposes only
  `{vision, image, image_edit, trained_for_tool_use}`; the portal
  `ModelDTO` (`gateway/backend/internal/portal/service.go`) carries the
  richer booleans (`Vision`, `Image`, `ImageEdit`, `Speech`,
  `Transcription`, `SpeechClone`).
  - **image vs image_edit are not independently distinguishable**:
    `image_edit` is derived (`ImpliedImageEdit = image:yes AND NOT operator
    image_edit:no`), a subset of `image`, not an independent class.
- **Frontend editor:** `gateway/frontend/src/components/RuntimeMatrix.tsx`
  (strictly-lower-triangle grid, `canonicalPair` on toggle), wired in
  `RuntimeAdminSection.tsx` (live render; file-mode render is read-only).
  Pairs loaded via `api.runtimeCoresidency`, saved via
  `api.putRuntimeCoresidency` as a full replacement set.

## 4. Design

### 4.1 Per-server mode + tri-state pairs

- Add a per-application **`coresidency_mode`** setting, enum
  `opt_in | opt_out`. It lives with the server-agent's other per-application
  settings. **Default `opt_out`** for new applications.
- Change the pair store from "presence = allowed" to a **verdict per pair**:
  add a **`verdict`** column to `agent_coresidency_rules`, enum
  `allow | forbid`. A row is an *explicit* setting; **absence of a row =
  default**.
- **Effective co-residency** of a pair `(a, b)`:
  - explicit row present → its verdict (`allow` / `forbid`);
  - no row → `opt_out ? allow : deny`.
- Pairs remain **symmetric** (canonical `a < b`) and **irreflexive**.
- A mode switch changes only how *default* (no-row) pairs resolve; explicit
  rows are untouched — the switch is lossless.

### 4.2 Model role (single derived value) + embedding/reranker

- Introduce a **single role per model**, used identically for (a) the UI
  grouping and (b) the eviction "same type" rule. Roles:
  `asr`, `tts`, `image`, `embedding`, `reranker`, `chat`, `other`.
- **Derivation** (first match wins), from capabilities, with runtime type as
  a coarse fallback:
  1. `transcription` → `asr`
  2. `speech` → `tts`
  3. `image` or `image_edit` → `image`
  4. `embedding` → `embedding`
  5. `rerank` → `reranker`
  6. otherwise, an LLM-server type (`vllm`/`llama_cpp`/`tgi`/`ollama`) or
     chat-ish capabilities → `chat`
  7. else → `other`
  - A model with both `transcription` and `speech` resolves to `asr` by
    priority (rare; a single model rarely does both). Document the tie-break.
- Add **`embedding`** and **`rerank`** to the capability vocabulary, set
  through the existing operator-verdict mechanism (the verdict write-path
  already accepts any non-empty capability name, so no allowlist change is
  needed to store them; automatic probing is deferred). This is the "build
  it in now" item: it makes `embedding`/`reranker` protected distinct roles
  so a chat LLM does not evict an embedding model (the RAG analogue of the
  voice-chat ASR/TTS case).
- **In scope now (operator UI):** two toggles in the model/mapping editor so
  a model can actually be marked `embedding` / `reranker`. The capability
  controls there (`gateway/frontend/src/components/MappingForm.tsx`,
  `ModelServersSection.tsx`) are currently a fixed enumeration and must gain
  these two (with tests + i18n) — otherwise no model ever acquires the role
  and the protection cannot be exercised end-to-end.
- `image` generation and editing collapse into one `image` role — correct
  and sufficient for both grouping and eviction (image evicts image).
- The role is **derived server-side** (one source of truth) and surfaced to
  the UI on the model/spec DTO, so the editor's grouping and the agent's
  eviction never diverge. (The agent needs each running model's role for
  eviction; see 4.3 for how it travels.)

### 4.3 Resource-driven admission & intelligent eviction

Reuse the shipped admission pipeline; make two behavioural extensions.

- **Grace window (hard protection).** Extend evictability so a model is a
  candidate victim only if `!Pinned && !Starting && InFlight == 0 && now −
  LastUsed > grace`. Within the grace window a model counts as *in use* and
  is never evicted, regardless of pressure. Knob
  **`OP_AGENT_EVICTION_GRACE_SECONDS`**, default **30** (env-only). This
  protects bursty pipelines between requests (e.g. voice-chat gaps).
- **Role-aware victim ordering.** When admitting candidate `M` needs room,
  order idle victims by a key of `(role == M.role ? 0 : 1, LastUsed asc)`:
  prefer a same-role idle model (role swap — keeps one of each role alive),
  then least-recently-used across other roles. Cross-role eviction is
  normal and expected when a large model needs much room (a big LLM may
  evict idle ASR/TTS/image). When only protected models remain, the start
  **waits** (existing wait/wake path).
- **Matrix interaction is orthogonal and unchanged in spirit.** An explicit
  `forbid` for a running partner of `M` still forces that partner's
  eviction (if evictable) or a wait; in opt-out, default-allow means this
  path rarely fires. The effective verdict (4.1) feeds the existing matrix
  rule.
- **Protocol (backward-compatible).** Co-residency stays on the **existing
  wire field**: the gateway computes the **effective allowed-pair list**
  from mode + verdicts (opt-out → all enabled spec pairs minus explicit
  forbids; opt-in → explicit allows) and sends it in the existing
  `coresident` list, exactly as today. The agent keeps consuming
  `Allowed map[[2]string]bool` unchanged, so **both old and new agents get
  correct co-residency** and opt-out never degrades to the wrong (deny)
  default on a pre-0.15.0 agent. The one genuinely new wire datum is **each
  spec's role**, added additively to the pushed spec; an agent that does not
  understand it falls back to today's LRU-only eviction (`sortOldestFirst`)
  — a safe degradation, never an OOM. The config document therefore stays
  one shared, non-feature-gated document per server. Mode and verdicts live
  gateway/DB-side only; the agent does not need them.

### 4.4 UI: role-grouped master-detail editor

Replace `RuntimeMatrix.tsx` with a new component inside the server-agent
settings (no server picker — the view already belongs to exactly one
server-agent).

- **Mode toggle at the top**, per server (opt-out / opt-in), with a hint
  stating what *default* currently means ("Default = allowed").
- **Master-detail:** left = searchable model list, each item showing an
  **exception count** (number of explicit allow/forbid pairs). Right =
  the selected model's relationships **grouped by role**:
  - a **group-level tri-state control** sets all members of that role at
    once;
  - **per-member override** of the group; a member shows inheritance
    ("from group") and a group shows a **"mixed"** marker when a member
    deviates;
  - groups are **collapsible**.
- **Long names:** truncate with ellipsis + full name on hover in the list
  and in rows; the selected model shown in full at the top; the tri-state
  control stays pinned and always visible on the right. The **divider
  between the model list and the detail panel is draggable** and remembers
  its width (per-viewer, e.g. localStorage).
- A toggle edits exactly one symmetric pair (editing A↔B from either side is
  the same pair). Saves continue as a full replacement set (now carrying
  verdicts) plus the mode.

### 4.5 Persistence & migration (forward-only)

- **Migration (pairs):** add `verdict` (`allow`/`forbid`) to
  `agent_coresidency_rules`; set every existing row to **`allow`** (they
  were allowed pairs). Follows the shipped `addColumnIfMissing` pattern;
  must work on both sqlite and postgres (provision the Postgres leg like
  CI for any store/migration change).
- **Migration (mode):** add `coresidency_mode` to the per-application
  settings; set every **existing** application to **`opt_in`** (preserves
  today's exact behaviour: present row → allow, absent → deny). New
  applications default to **`opt_out`**. This makes opt-out the standard
  going forward without changing any running server's behaviour until the
  operator flips it.
- Capability additions (`embedding`, `rerank`) use the existing
  `model_mapping_capabilities` table; no schema change beyond new vocabulary
  constants.

### 4.6 Config, agent features, versioning

- New knob **`OP_AGENT_EVICTION_GRACE_SECONDS`** (default 30, env-only;
  confirm the exact spelling/pattern against
  `server-agent/internal/config/config.go`).
- **One** ServerAgent `Version` bump for the whole branch: **0.14.0 →
  0.15.0** (per repo norm: one bump per branch).
- One new agent feature flag **`runtime_role_eviction`**, `Since 0.15.0`,
  declared by the agent in `agent.Features` for **telemetry/visibility only**
  (so the portal can show that an agent does role-aware, grace-protected
  eviction). It does **not** gate the config document — co-residency is
  backward-compatible (§4.3) and the role field degrades safely — so
  `AgentRuntimeConfig` stays non-feature-aware (one shared document per
  server). The gateway-side feature constant lives in `agent_features.go`
  and is **not** added to `gatewayAgentFeatures`. The grace window is an
  agent-side config knob, not wire.

## 5. Constraints (repo norms — Global Constraints for the plan)

- Feature branch + PR only; **never commit to or merge `main`**.
- All repo-facing text (PRs, issues, commits, docs, code comments) in
  **English**; no LAN host names anywhere in repo text (privacy).
- No attribution lines in commit messages or PR descriptions.
- Substantive commit message bodies (squash-merge pre-fills its description
  from the commit body).
- Go: `golangci-lint fmt --diff` + `run` per module; `GOOS=windows` vet
  where platform code is touched; full `go test ./...` including the
  **Postgres leg** for any store/migration change.
- Frontend: `vitest`, `tsc`, `eslint`, **`prettier format:check`**.
- `check-docs`; local SonarQube `sonar-gate` + `sonar-branch-findings`
  (judge by branch-attributed findings).
- `docs/superpowers/` is branch-local and removed before the PR; permanent
  docs go in `docs/architecture/` (ADRs + the relevant chapters).

## 6. What's left to the builder

Decisions deliberately left to the plan/implementer (sensible within this
design):
- Exact migration SQL and column types; where `coresidency_mode` physically
  lives among per-application settings.
- Exact placement of the `role()` derivation function and how each running
  spec's role reaches the agent snapshot.
- Precise integration points of the grace window and role-aware ordering
  into the existing `admission` pipeline / eviction selection, without
  disturbing the shipped resource rules or the OOM anti-goal.
- New frontend component structure, DTO field names, and i18n keys;
  reuse of existing table/row primitives in `RuntimeAdminSection.tsx`.
- Permanent-doc updates (new ADR(s) for the mode/tri-state, role derivation,
  and role-aware eviction; chapter updates in
  `docs/architecture/cross-cutting/agent-runtime-manager.md`,
  `reference/config-env.md`, `reference/data-model.md`,
  `reference/api-surface.md`).

## 7. Open questions

A fresh-builder review (2026-11-02) raised two; both are now resolved and
folded in:
- **Wire behaviour for pre-0.15.0 agents** → the gateway sends the computed
  effective allowed-pair list on the existing field; the role field is
  additive and degrades to LRU (§4.3, §4.6). *(Author's call — it also
  removes the need for a feature-aware config document.)*
- **Embedding/reranker operator UI** → in scope now: add the two capability
  toggles to the model/mapping editor (§4.2). *(Operator's decision.)*

The migration default (existing servers → opt-in, new → opt-out) is the one
operator-facing judgement call and is decided in §4.5.
