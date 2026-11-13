# SGLang and sglang-omni runtime spec types (#395 Phases 1+2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The portal can configure agent-launched `sglang` and `sglang_omni` run specs that are detected, probed, scraped and routed correctly. Speech and transcription through a non-audio.cpp target get correct audio-second usage rows.

**Architecture:** Each type follows the `vllm_sr` precedent (ADR-099):
- the gateway owns the type string, its detection by binary name and its probe paths, and pushes the resolved values to the agent;
- the agent adds metric names, a context extractor and a capability-probe skip;
- the portal offers the type with a launch-template note.

The audio relay gains duration fallbacks that read the WAV audio itself when the upstream does not send audio.cpp's metering signals.

**Tech Stack:** Go (gateway/backend, server-agent), React/MUI/vitest (gateway/frontend).

**Spec:** GitHub issue #395 (Phases 1 and 2), plus the verified facts in the shared notes:
- `/Users/jlor08/Library/Application Support/Claude/side-session-notes/local_23aad4de-1ce7-43cb-8342-d60c6e566fa9/sglang-research-NOTES.md`
- `…/sglang-omni-realtime-research-NOTES.md`
- `…/sglang-codebase-seams-NOTES.md` (file:line seams).

## Design

### D1 Type strings and detection
- New `routing.RuntimeSpecType` values `sglang` and `sglang_omni`. The agent's copies are literal strings, because the agent never imports routing.
- `DetectRuntimeSpecType` (case-insensitive base name, first match wins):
  - a name containing `sgl-omni`, `sgl_omni`, `sglang-omni` or `sglang_omni` → `sglang_omni`;
  - otherwise a name containing `sglang` → `sglang`.
  - Both cases come before the `vllm` case.
- An explicit `type` still wins. `python -m sglang.launch_server` needs `type: sglang`, and `sglang serve --model-type omni` needs `type: sglang_omni`.

### D2 Probe paths (`DeriveProbePaths`)
- `sglang`: metrics `/metrics`, context probe `/v1/models`. Health keeps the stock `/health`.
- `sglang_omni`: no metrics path and no context probe (both ""), because it has no `/metrics` and `/v1/models` carries no context. Health `/health`.

### D3 Agent
- **Context extractor:** for `sglang`, read the first `data[].max_model_len` (a positive integer) from `/v1/models`, else fall back to the existing best-effort scan. For `sglang_omni` the probe path is "", so nothing is probed.
- **Metrics scrape:** `sglang:num_queue_reqs` is the queue depth and `sglang:num_running_reqs` is the running count, placed after the existing names in the first-present lists. Only series with `tp_rank="0"`, or with no `tp_rank` label, count, summed over the remaining series (data-parallel ranks). This rule applies only to the `sglang:` names; existing names keep their current aggregation.
- **Capability probe:** the llama.cpp `/props` probe is skipped for both types, as for `vllm_sr`.
- **Version:** `0.19.1` → `0.19.2` (PATCH; no feature flag).

### D4 Gateway routing and portal backend
- **Role:** `DeriveRole` falls back to chat for `sglang`. `sglang_omni` gets no type fallback, so capabilities decide and an ASR-only server stays ASR.
- **Validation:** `validRuntimeSpecType` accepts both types. Its comments and the hard-coded error message in `portal_runtime_endpoints.go` list them.
- **Live features:** live timings and live progress stay off for both. The two kind-parity tests and the live_timings doc comment list them.
- **Warning:** a `sglang_omni` spec whose flavors include no `openai_audio` gets `api_flavors_no_audio_on_sglang_omni`. It is advisory, following the vllm_sr warning pattern.

### D5 Audio relay for non-audio.cpp targets
These fallbacks apply only when audio.cpp's signal is missing, so audio.cpp behaviour is unchanged.

- **Speech, one-shot:** with no usable `X-AudioCPP-Audio-Duration-Ms` on a successful relay, duration = audio data bytes ÷ byte rate. The byte rate comes from the WAV `fmt ` chunk of the returned body. The data bytes are counted from what was relayed, not read from the header's size field, because a streamed WAV may carry 0 or 0xFFFFFFFF there.
- **Speech, pcm:** pcm is derived from upstream WAV as today. Its duration comes from the same WAV header and byte count. Allowed formats stay wav and pcm.
- **Speech, streamed:** keeps today's rule (cached byte rate, else `audio_bytes`).
- **Transcription:** when the response JSON carries no positive `timing.audio_duration_ms`, duration = the uploaded file's WAV duration, measured with the existing `wavDurationFromReader` over the buffered upload.
- **Failure:** a fallback that cannot determine a duration keeps today's Error log and quantity 0 (ADR-041).

### D6 Portal
- `RuntimeSpec['type']` gains both strings. The type select offers "SGLang" and "sglang-omni".
- Type notes (de/en):
  - `sglang`: launch template `serve ${MODEL} --host 127.0.0.1 --port ${PORT} --enable-metrics`. Set `--mem-fraction-static` when the card is shared (the auto default takes most of it). `--api-key` leaves `/health` and `/metrics` open.
  - `sglang_omni`: launch template `serve --model-path ${MODEL} --model-name ${MODEL} --host 127.0.0.1 --port ${PORT}`. Keep `--host 127.0.0.1`, because sglang-omni authenticates nothing and listens on 0.0.0.0 by default. Speech and transcription need manual capability verdicts. On Windows, run it in the WSL execution mode.
- Default flavors for `sglang_omni` are `openai` and `openai_audio`. `sglang` keeps the stock defaults.
- The live-timings exhaustiveness test (`liveTimings.test.ts`) covers both.

### D7 Docs and ADR
- agent-runtime-manager.md: type list and detection order, probe table, context matrix, the per-type facts block (like vllm_sr's), §7 version note 0.19.2.
- Also: glossary, data-model type list, api-surface type list, routing-and-model-selection lists, compatibility-and-inference audio billing fallback.
- ADR-103 "SGLang and sglang-omni runtime spec types; duration fallbacks for non-audio.cpp audio".

## Global Constraints
- Branch `claude/sglang-runtime-types-395`. Never commit to `main`.
- English repo text; no attribution lines; no LAN host names (use `Ubuntu-24.04`, `/home/ai/...`).
- Upstream facts are pinned: SGLang `v0.5.23` (297e54fd105e974cdf401e5b4d20253a22c16f90), sglang-omni `v0.2.0` (e8c635ecb9c68bcf2fbc32ceb3c40f85f3590657). Cite them in code comments where a fact is used.
- Go gates per touched module:
  - `golangci-lint fmt --diff`;
  - `GOTOOLCHAIN=go1.26.5 golangci-lint run ./...`;
  - `go test -count=1 ./internal/archtest/`.
  - Gateway tests run with `GOTOOLCHAIN=go1.26.5`.
- Frontend gates: `npm run lint` (0 errors), `npm run format:check`, `npm run build`, touched vitest files.
- `make lint-docs` passes.

## Review Focus
1. `sglang-omni` named binaries must not detect as `sglang`, and nothing that detected before may change type. Test the full detection table, old and new names.
2. SGLang with `--tp-size 2` emits one series per rank. The scraped queue and running counts must not double. Test multi-rank exposition text.
3. audio.cpp targets must behave exactly as before: header present means the header wins, and no new logging. Test both paths.
4. A WAV whose header size field is 0 or 0xFFFFFFFF, as streamed WAV writers produce, must still yield the right seconds. Test it.
5. A non-WAV transcription upload (e.g. mp3) to a non-audio.cpp target, with no reported duration, stays a logged zero and is never a guess. Test it.

---

### Task 1: Gateway types, detection, probe paths, role, validation, warning

**Files:** `gateway/backend/internal/routing/runtime_spec_type.go` (+ `_test.go`), `gateway/backend/internal/routing/model_role.go` (+ test), `gateway/backend/internal/routing/live_timings.go` (doc comment), `gateway/backend/internal/provider/live_progress_kind_parity_test.go`, `gateway/backend/internal/portal/service_runtime.go` (`validRuntimeSpecType`, comments, the new warning), `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (error message + test).

**Interfaces — produces:** `routing.RuntimeSpecTypeSGLang = "sglang"`, `routing.RuntimeSpecTypeSGLangOmni = "sglang_omni"`; warning code `api_flavors_no_audio_on_sglang_omni`.

- [ ] Step 1: failing tests.
  - Detection table: `/home/ai/sglang-env/bin/sglang` → sglang; `sgl-omni`, `/x/sglang-omni`, `SGL_OMNI` → sglang_omni; `vllm`, `vllm-srun`, `llama-server`, `ollama`, `text-generation-launcher`, `sd-server`, `audiocpp_server` unchanged; `python3` → custom.
  - `DeriveProbePaths`: sglang `/metrics` + `/v1/models`, sglang_omni "" + "". Explicit overrides still win.
  - `DeriveRole`: sglang → chat; sglang_omni with no capabilities → not chat.
  - Validation: a PUT with type `sglang` / `sglang_omni` is accepted.
  - Warning: present for a sglang_omni spec with flavors `["openai"]`, absent with `["openai","openai_audio"]`.
- [ ] Step 2: run, see FAIL. Step 3: implement D1/D2/D4. Step 4: `cd gateway/backend && GOTOOLCHAIN=go1.26.5 go test ./internal/routing/ ./internal/portal/ ./internal/gateway/ ./internal/provider/ -skip 'TestVoice|TestLiveTranscriptionLoopback'` + Go gates. Step 5: commit `feat(gateway): sglang and sglang_omni runtime spec types`.

### Task 2: Agent scrape, context, probe skip, version

**Files:** `server-agent/internal/collector/scrape.go` (+ test), `server-agent/internal/collector/probe.go` (+ test), `server-agent/internal/agent/agent.go` (`probeRuntimeChild` skip, `Version`), `server-agent/internal/agent/features_test.go` (version pin).

- [ ] Step 1: failing tests.
  - Scrape exposition with `sglang:num_queue_reqs{tp_rank="0",dp_rank="0"} 3`, `{tp_rank="1",dp_rank="0"} 3`, `{tp_rank="0",dp_rank="1"} 2` → queue 5. Same shape for running.
  - Unlabelled `sglang:num_queue_reqs 4` → 4.
  - Existing vLLM, llama.cpp and TGI names unchanged.
  - Context: `{"data":[{"id":"m","max_model_len":32768}]}` with type sglang → 32768.
  - Capability probe: a sglang and a sglang_omni child never GET `/props`.
  - Version is `0.19.2`.
- [ ] Step 2: FAIL. Step 3: implement D3. Step 4: `cd server-agent && go test ./... && go test -count=1 ./internal/archtest/` + Go gates. Step 5: commit `feat(agent): sglang metrics, context and probe skip (0.19.2)`.

### Task 3: Audio duration fallbacks for non-audio.cpp targets

**Files:** `gateway/backend/internal/gateway/speech_handler.go`, `gateway/backend/internal/gateway/transcription_handler.go`, possibly `wavduration.go`, plus their tests.

- [ ] Step 1: failing tests.
  - Speech one-shot wav without the audio.cpp header: 24000 Hz mono 16-bit with 48000 data bytes → 1.0 s billed.
  - The same with header size fields 0 and 0xFFFFFFFF → 1.0 s.
  - pcm derived from such a WAV → 1.0 s.
  - Header present → the header value wins, and no new log.
  - A non-WAV body without the header → Error log + 0, as today.
  - Transcription response without `timing` + a WAV upload of 2.5 s → 2.5 s.
  - Response `timing.audio_duration_ms` present → wins.
  - An mp3 upload without timing → logged zero.
- [ ] Step 2: FAIL. Step 3: implement D5 with the smallest change at the two setters. Step 4: `GOTOOLCHAIN=go1.26.5 go test ./internal/gateway/ -run 'Speech|Transcription|WAV|Billing' -skip 'TestVoice|TestLiveTranscriptionLoopback'` and then the whole package + Go gates. Step 5: commit `feat(gateway): audio duration fallbacks for non-audio.cpp targets`.

### Task 3b: Speech to sglang_omni refuses server-local voice references

Owner target model: Ming-3.0-TTS. In sglang-omni v0.2.0 (`docs/cookbook/ming3_tts.md`) voice cloning takes `ref_audio`, and a bare path is read by the SERVER from its working directory. A `data:` URL is also accepted. The speech body otherwise reaches the upstream unexamined (`validateSpeechRequest`).

**Files:** `gateway/backend/internal/gateway/speech_handler.go` (+ test).

- [ ] Step 1: failing tests. For a speech request routed to a target whose effective runtime spec type is `sglang_omni`:
  - `ref_audio: "data:audio/wav;base64,..."` is relayed;
  - `ref_audio` values `"tests/data/x.wav"`, `"/etc/passwd"`, `"file:///x.wav"` and `"https://host/x.wav"` are refused with a 400 `speech.reference_audio_unsupported`;
  - the same rule applies to each `references[].audio_path`;
  - a request to an audio.cpp target is relayed unchanged whatever it carries.
- [ ] Step 2: FAIL. Step 3: implement. The check runs where the resolved target is known; the refusal text names the field, never echoes the value. Step 4: gateway gates. Step 5: commit `feat(gateway): sglang_omni speech accepts only inline voice references`.

### Task 4: Portal

**Files:** `gateway/frontend/src/api/runtime.ts`, `gateway/frontend/src/components/RuntimeAdminSection.tsx` (select options, `runtimeSpecTypeNote`, warning map), `gateway/frontend/src/components/shared/runtimeSpecTypeFlavors.ts`, `gateway/frontend/src/components/shared/liveTimings.test.ts`, `gateway/frontend/src/i18n.ts` (+ `i18n.test.ts`), `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`.

- [ ] Step 1: failing tests.
  - Both options are offered.
  - Choosing sglang_omni shows its note and sets the flavors to `openai` + `openai_audio`; sglang keeps the stock flavors.
  - The warning code renders its de/en text.
  - The i18n keys exist in de and en and differ.
- [ ] Step 2: FAIL. Step 3: implement D6. Step 4: frontend gates. Step 5: commit `feat(portal): SGLang and sglang-omni run spec types`.

### Task 5: Docs and ADR-103

**Files:** `docs/architecture/cross-cutting/agent-runtime-manager.md`, `docs/architecture/09-architecture-decisions.md`, `docs/architecture/12-glossary.md`, `docs/architecture/reference/data-model.md`, `docs/architecture/reference/api-surface.md`, `docs/architecture/cross-cutting/routing-and-model-selection.md`, `docs/architecture/cross-cutting/compatibility-and-inference.md`.

- [ ] Step 1: describe D1–D7 where the docs enumerate types and audio billing. Add ADR-103 with context, decision, rejected alternatives (a capability probe now; relaxing response formats) and consequences.
- [ ] Step 2: `make lint-docs`. Step 3: commit `docs: sglang and sglang_omni runtime types (ADR-103)`.
