# Exclude Clone-Task Speech Models From The Chat Picker — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A clone-task audio.cpp model no longer appears in the portal chat speech picker, while tts models still do, via a new non-routing refinement capability `speech_clone`.

**Architecture:** `speech_clone` is the twin of the shipped `transcription_streaming` capability (#262/ADR-065). It is derived per mapping from audio.cpp's `/v1/models` `task` field (`tts`→no, `clone`→yes), written by both producers of the `audiocpp_models` source (the gateway health loop and the ServerAgent), folded into a new `ModelDTO.SpeechClone` flag, and read by the frontend `chatModels` filter to exclude clone models. It gates no route: the direct `/v1/audio/speech` API still serves clone models.

**Tech Stack:** Go (two modules: `op-ai-gateway`, `op-ai-server-agent`), React + TypeScript + Vite (portal). Stores: memory / sqlite / postgres.

**Spec:** `docs/superpowers/specs/2026-10-19-clone-speech-picker-design.md`

## Global Constraints

- Repo-facing text (code, comments, docs, commits, PR) in English; chat may be German.
- Never commit to or merge into `main`; work on this worktree's branch; human merges the PR.
- Keep the two `parseAudioCppModels` copies identical, pinned by the shared `audioCppModelCases` table duplicated in both modules' tests; `TestAudioCppModelsCasesMatchTheAgentsTable` must stay green.
- ServerAgent `const Version` (`server-agent/internal/agent/agent.go`) bumps exactly once, PATCH: `0.9.2` → `0.9.3`. No new `agent.Features` entry.
- No DB migration (capability is a string column; the name-set is open). Verify the Postgres store leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- `speech_clone` gates NO route: no request declares it required; `ModelOffering.Capable` / `capableNames` / the `/v1/audio/speech` gate are unchanged.
- i18n: add a German+English key pair only if a new user-facing string is introduced (expected: none).
- Remove `docs/superpowers/**` and `docs/implementation-status.md` before the PR.

## Review Focus

- **Mixed mappings (OR fold):** a gateway model with one tts and one clone offering mapping must fold `SpeechClone: true` (a chat send could land on the clone mapping) — pinned in Task 4.
- **Fail-open on unknown/old agent:** a speech model with no `speech_clone` row (absent task, or an agent < 0.9.3) folds `SpeechClone: false` → still offered, never refused or crashed — pinned in Task 3 (ingest accepts absence) and Task 4 (fold).
- **Reserved manual `(speech_clone, no)`:** an operator cannot hand-set it to force-offer a clone model — pinned in Task 4.
- **Direct API unchanged:** a clone model keeps `Speech: true` / `(speech, yes)` so the bearer `/v1/audio/speech` path and the unknown-model redirect still treat it as a valid candidate — pinned in Task 2 (speech row unchanged) and Task 4 (Speech flag unchanged).
- **Case/whitespace task value:** `"Clone"` / `" clone "` still classifies as clone — pinned in Task 1 (parser trims + lowercases).

---

### Task 1: The `speech_clone` parser (both modules) + capability constant

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (add `CapabilitySpeechClone`; extend `CapabilitySourceAudioCppModels` doc)
- Modify: `gateway/backend/internal/provider/audiocpp_models.go` (`AudioCppVerdicts.SpeechClone`; parser rule; docs)
- Test: `gateway/backend/internal/provider/audiocpp_models_test.go` (the `audioCppModelCases` table + expectations)
- Modify: `server-agent/internal/collector/audiocpp.go` (`parseAudioCppModels` returns speechClone)
- Modify: `server-agent/internal/collector/probe.go` (`Capabilities.SpeechClone`)
- Modify: `server-agent/internal/collector/audiocpp_test.go` (mirror the `audioCppModelCases` table)

**Interfaces:**
- Produces (gateway): `routing.CapabilitySpeechClone = "speech_clone"`; `provider.AudioCppVerdicts.SpeechClone string` (one of `routing.CapabilityYes`, `routing.CapabilityNo`, `""`), set to `no` for task `tts`, `yes` for task `clone`, `""` otherwise.
- Produces (agent): `collector.parseAudioCppModels(body, model) (speech, transcription, streaming, speechClone string)`; `collector.Capabilities.SpeechClone string`.

- [ ] **Step 1: Extend the shared case table with a `speechClone` expectation (failing test, gateway).** In `audiocpp_models_test.go`, add a `speechClone` column/field to the `audioCppModelCases` table and expectations: task `tts`→`""`? No — `tts`→`no`, `clone`→`yes`, `asr`/music/absent/not-named→`""`; `"Clone"` and `" clone "` (case/space)→`yes`. Assert `parseAudioCppModels(...).SpeechClone` equals the expected value for every case. Keep `TestAudioCppModelsCasesMatchTheAgentsTable` comparing the two table copies.

- [ ] **Step 2: Run to verify it fails.** `cd gateway/backend && go test ./internal/provider/ -run TestParseAudioCppModels -v` — Expected: FAIL (`SpeechClone` field/behavior missing).

- [ ] **Step 3: Add the capability constant + source doc.** In `routing/store.go`, add `CapabilitySpeechClone = "speech_clone"` beside `CapabilityTranscriptionStreaming`, with a doc comment modeled on it: refines `CapabilitySpeech`, gates no route, derived from audio.cpp `/v1/models` `task` (tts→no, clone→yes), written by the `audiocpp_models` source. Extend the `CapabilitySourceAudioCppModels` doc block to say it also writes the `speech_clone` row.

- [ ] **Step 4: Add `SpeechClone` to the gateway parser.** In `audiocpp_models.go`, add `SpeechClone string` to `AudioCppVerdicts`; in `parseAudioCppModels`'s `switch`, set `v.SpeechClone = routing.CapabilityNo` for `tts` and `routing.CapabilityYes` for `clone` (the `asr`/default/empty branches leave it `""`). Update the doc comments (the type and the function) to state the rule.

- [ ] **Step 5: Mirror in the agent parser.** In `server-agent/internal/collector/audiocpp.go`, change `parseAudioCppModels` to return a fourth value `speechClone`; `tts`→`"no"`, `clone`→`"yes"`, else `""`. Add `SpeechClone string` to `Capabilities` (`probe.go`) with a doc comment; set it in `ProbeAudioCppVerdicts`. Mirror the test table in `audiocpp_test.go` so it stays byte-identical to the gateway's.

- [ ] **Step 6: Run both modules' parser tests.** `cd gateway/backend && go test ./internal/provider/ -run AudioCpp -v` and `cd server-agent && go test ./internal/collector/ -run AudioCpp -v` — Expected: PASS, including `TestAudioCppModelsCasesMatchTheAgentsTable`.

- [ ] **Step 7: Commit.** `git commit -m "feat: derive a speech_clone verdict from audio.cpp task in both parsers"`

---

### Task 2: The gateway health loop writes the `speech_clone` row

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`audioCppCapabilityRows`)
- Test: `gateway/backend/cmd/gateway/app_health_test.go` (the `audioCppCapabilityRows` test)

**Interfaces:**
- Consumes: `provider.AudioCppVerdicts.SpeechClone`, `routing.CapabilitySpeechClone` (Task 1).
- Produces: `audioCppCapabilityRows` emits a `(speech_clone, yes|no)` row whenever `v.Speech == CapabilityYes` and `v.SpeechClone` is a real verdict.

- [ ] **Step 1: Failing test.** In `app_health_test.go`, extend the `audioCppCapabilityRows` table test: a `clone` verdict (`Speech: yes, SpeechClone: yes`) yields a `(speech_clone, yes)` row; a `tts` verdict (`Speech: yes, SpeechClone: no`) a `(speech_clone, no)` row; an `asr` verdict (`Speech: no`) yields no `speech_clone` row; an empty verdict yields none. Assert the row's `Source == routing.CapabilitySourceAudioCppModels`.

- [ ] **Step 2: Run to verify it fails.** `cd gateway/backend && go test ./cmd/gateway/ -run AudioCppCapabilityRows -v` — Expected: FAIL.

- [ ] **Step 3: Add the axis.** In `audioCppCapabilityRows`, add `{routing.CapabilitySpeechClone, v.SpeechClone}` to the axis loop, guarded like the streaming axis: written only when `v.Speech == routing.CapabilityYes` (so a non-speech model never carries a speech_clone row). Update the function doc to list the fourth row and its refinement guard.

- [ ] **Step 4: Run to verify it passes.** Same command — Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat: health loop writes the speech_clone row for audio.cpp mappings"`

---

### Task 3: The agent reports `speech_clone`; the gateway ingest accepts it; version bump

**Files:**
- Modify: `server-agent/internal/agent/agent.go` (verdict projection ~L1922-1935; `const Version`)
- Test: `server-agent/internal/agent/agent_test.go` (the verdict-projection test)
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`audioCppProbeCapabilities`; rule-4 doc)
- Test: `gateway/backend/internal/gateway/agent_ingest_test.go`

**Interfaces:**
- Consumes: `collector.Capabilities.SpeechClone` (Task 1); `routing.CapabilitySpeechClone` (Task 1).
- Produces: the agent emits `sample.CapabilityVerdict{Name: "speech_clone", Verdict: c.SpeechClone}` when `c.SpeechClone != "" && c.Speech == "yes"`; the gateway's `audioCppProbeCapabilities` admits `routing.CapabilitySpeechClone`.

- [ ] **Step 1: Failing test (agent projection).** In `agent_test.go`, assert the capability-verdict projection emits a `speech_clone` verdict when `Capabilities{Speech: "yes", SpeechClone: "yes"}`, and emits none when `SpeechClone == ""` or `Speech != "yes"`.

- [ ] **Step 2: Failing test (gateway ingest).** In `agent_ingest_test.go`, assert a `(speech_clone, yes)` row attributed to `audiocpp_models` is accepted (not refused), and that a `speech_clone` row attributed to a different source is still refused.

- [ ] **Step 3: Run both to verify they fail.** `cd server-agent && go test ./internal/agent/ -run Verdict -v` and `cd gateway/backend && go test ./internal/gateway/ -run AudioCpp -v` — Expected: FAIL.

- [ ] **Step 4: Project the verdict (agent).** After the `transcription_streaming` block in `agent.go`, append `if c.SpeechClone != "" && c.Speech == "yes" { verdicts = append(verdicts, sample.CapabilityVerdict{Name: "speech_clone", Verdict: c.SpeechClone}) }`.

- [ ] **Step 5: Allow-list the capability (gateway).** Add `routing.CapabilitySpeechClone: true` to `audioCppProbeCapabilities` in `agent_ingest.go`; update the rule-4 doc comment (now four names) and the `audioCppProbeCapabilities` doc.

- [ ] **Step 6: Bump the agent version.** Set `const Version = "0.9.3"` in `server-agent/internal/agent/agent.go`.

- [ ] **Step 7: Run to verify they pass.** Same commands as Step 3, plus the `agent.Features` guard test (`cd server-agent && go test ./internal/agent/ -v`) — Expected: PASS.

- [ ] **Step 8: Commit.** `git commit -m "feat: agent reports speech_clone, gateway ingest accepts it, bump agent 0.9.3"`

---

### Task 4: `ModelDTO.SpeechClone` fold + reserved manual verdict

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (`ModelDTO.SpeechClone`; `speechCloneFlagsByName`; listing assembly ~L2603; group/alias plumbing)
- Modify: `gateway/backend/internal/portal/service_applications.go` (reserve `(speech_clone, no)`)
- Test: `gateway/backend/internal/portal/service_test.go` (or the existing speech-fold/model-listing test file) + `service_mapping_capability_reserved_test.go`

**Interfaces:**
- Consumes: `routing.CapabilitySpeechClone` (Task 1); the capability rows written by Tasks 2/3.
- Produces: `ModelDTO.SpeechClone bool`, JSON `speech_clone`; true when ANY offering mapping (or, for a group, any offerable member) carries `(speech_clone, yes)`.

- [ ] **Step 1: Failing test (fold).** In the portal model-listing test, inject a clone mapping (`speech:yes`, `speech_clone:yes`) and assert its `ModelDTO.SpeechClone == true` and `ModelDTO.Speech == true`; a tts mapping (`speech:yes`, `speech_clone:no`) asserts `SpeechClone == false`, `Speech == true`; a mixed group (one tts member, one clone member) asserts the group row's `SpeechClone == true`; a speech model with no `speech_clone` row asserts `SpeechClone == false` (fail-open).

- [ ] **Step 2: Failing test (reserved verdict).** In `service_mapping_capability_reserved_test.go`, assert a manual `(speech_clone, no)` write is rejected (mirroring the `(transcription_streaming, no)` case), while a `(speech_clone, yes)` manual write and a probe-source `(speech_clone, no)` are unaffected.

- [ ] **Step 3: Run to verify they fail.** `cd gateway/backend && go test ./internal/portal/ -run 'SpeechClone|Reserved' -v` — Expected: FAIL.

- [ ] **Step 4: Add the DTO field + fold.** Add `SpeechClone bool \`json:"speech_clone"\`` to `ModelDTO` with a doc comment (the deliberate inverse of the fail-closed speech fold: "hide if any mapping is clone"). Add `speechCloneFlagsByName(views, speechCloneRows, idx) map[string]bool` — an OR over mappings (true if any mapping's `(speech_clone)` verdict is yes; mirror the speech fold's `viewServesAudio` half for structural consistency). Add the group half (any offerable member true) beside `groupCapabilityFlags`, or an OR variant. Wire the flag into the listing assembly next to `Speech` (~L2603) and the alias inheritance path.

- [ ] **Step 5: Reserve the manual verdict.** In `service_applications.go`, add `routing.CapabilitySpeechClone: {routing.CapabilityNo: true}` to the reserved map, with a comment mirroring the `CapabilityTranscriptionStreaming + CapabilityNo` reservation.

- [ ] **Step 6: Run to verify they pass.** Same command — Expected: PASS.

- [ ] **Step 7: Commit.** `git commit -m "feat: fold speech_clone into ModelDTO and reserve the manual no"`

---

### Task 5: The chat picker excludes clone models

**Files:**
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx` (`ModelOption.speechClone`; the `chatModels` filter ~L551)
- Test: `gateway/frontend/src/components/chat/ChatStore.test.tsx`
- Check: `gateway/frontend/src/api.ts` (the `ModelOption`/models DTO type mapping) if `speech_clone` must be threaded there.

**Interfaces:**
- Consumes: `ModelDTO.speech_clone` (Task 4).
- Produces: a clone model (`speech:true, speechClone:true`) is absent from `chatModels`; a tts model (`speech:true, speechClone:false`) is present.

- [ ] **Step 1: Failing test.** In `ChatStore.test.tsx`, assert a model with `{flavors:['openai_audio'], speech:true, speechClone:true}` is NOT in `chatModels`, a `{...speech:true}` (no speechClone) and a `{...speech:true, speechClone:false}` ARE, and a `{flavors:['openai_audio'], transcription:true}` is (transcription unaffected).

- [ ] **Step 2: Run to verify it fails.** `cd gateway/frontend && npx vitest run src/components/chat/ChatStore.test.tsx` — Expected: FAIL.

- [ ] **Step 3: Thread the field.** Add `speechClone?: boolean` to `ModelOption` (and map `speech_clone` from the API DTO wherever `speech`/`transcription` are mapped — check `api.ts`). Change the `chatModels` speech half to `(option.speech === true && option.speechClone !== true)`. Document the exclusion with a short comment referencing #252.

- [ ] **Step 4: Run to verify it passes.** Same command — Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(portal): exclude clone-task speech models from the chat picker"`

---

### Task 6: Docs & ADR

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-061 (d), ADR-063 (e)/(h); a dated amendment noting #252 resolved the ADR-063 (h) limitation)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (the picker/clone note ~§12 and the model-discovery §9 flag list)
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (§11 clone-task note — the excluded-from-chat half is resolved)
- Modify: `docs/architecture/reference/data-model.md` and `docs/architecture/reference/api-surface.md` (+ `openapi.yaml`) if they enumerate the `ModelDTO` capability flags or the capability names

**Interfaces:** none (documentation).

- [ ] **Step 1: Update the ADR log.** ADR-061 (d): note the task also decides the `speech_clone` refinement row. ADR-063 (e): the chat's speech half now also excludes `speech_clone`. ADR-063 (h): mark the "clone models are offered and may fail" acceptance as resolved by #252 via the `speech_clone` refinement, with a dated amendment line (do not rewrite history — amend).

- [ ] **Step 2: Update the compat doc.** In the §12 picker description, change the clone-task note (currently "A clone-task model carries the verdict too … so it is offered") to the new behavior: a clone model carries a `(speech_clone, yes)` refinement and is excluded from the chat speech picker, while still served by the direct endpoint. Add `speech_clone` to the §9 `ModelDTO` flag enumeration if present.

- [ ] **Step 3: Update Risks §11.** In the "speech composer … offers clone-task models that may fail" row, mark the clone half resolved (the `speech_clone` fact now excludes them from the chat), leaving the free-text `voice` half as the remaining accepted limitation.

- [ ] **Step 4: Update the reference docs** if they list the capability names or the DTO flags (`data-model.md`, `api-surface.md`, `openapi.yaml`'s `ModelDTO`).

- [ ] **Step 5: Verify the docs link/anchor check.** `make lint-docs` — Expected: PASS.

- [ ] **Step 6: Commit.** `git commit -m "docs: record speech_clone and resolve the ADR-063(h) clone-picker limitation"`

---

## Final verification (after all tasks, before the PR)

- `make lint-go` (both modules' golangci-lint: fmt --diff + run).
- `cd gateway/frontend && npm run test && npm run build && npm run format:check` (prettier).
- Go tests with the Postgres leg: provision a local postgres and run `OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./...` in `gateway/backend` (the new capability row is a string column; confirm the store round-trips it on all three drivers).
- `make sonar-gate` (advisory) then `make sonar-findings && make sonar-branch-findings` (authoritative) — act on branch-attributed findings.
- Whole-branch code review (fresh reviewer).
- Remove `docs/superpowers/**` and `docs/implementation-status.md`; verify with `git diff --name-only main...HEAD` that neither path appears.
- Push; open a PR against `main`; enable Auto-fix. Do not merge.
