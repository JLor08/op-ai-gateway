# Design — Exclude clone-task speech models from the portal chat speech picker (#252)

## Intent and why

The portal chat's TTS composer (#248) offers every speech-capable model in its
picker. A model is offered because it carries a `(speech, yes)` capability
verdict. But audio.cpp's `GET /v1/models` maps **both** `task: "tts"` and
`task: "clone"` to `(speech, yes)` (ADR-061 (d)), and the verdict does not record
which task it came from, nor does the model DTO carry the task. So a **voice-clone**
model appears in the chat speech picker exactly like a plain TTS model.

A clone model needs a **reference voice**. The text-only chat speech composer has
no way to supply one (there is no attach/record control on a speech thread —
ADR-063 (f)), so every send to a clone model comes back as an upstream error. This
is the audio counterpart of #149 (a picker offering a model that every send then
fails on), and it is recorded today as an accepted limitation in ADR-063 (h) and
Risks §11.4.

Issue #252 removes the affordance that cannot work. We take **Option 1** (the
minimal fix named in the issue): **exclude clone-task models from the chat speech
picker**. Option 2 (supporting clone in the composer with an attached/recorded
reference voice) is **out of scope** — it overlaps the transcription composer's
audio-input work and is a full feature with its own ADR.

## Goals

- A **clone**-task speech model no longer appears in the portal chat speech
  picker.
- A **tts**-task speech model still appears and still works, unchanged.
- The exclusion works for **both** producers of the audio.cpp verdict: the
  gateway's health loop (static `audio_cpp` applications) and the ServerAgent
  (agent-launched `audio_cpp` children). Leaving either out would re-open the bug
  for that deployment shape and would break the two-parser case-table sync test.
- The direct public API (`POST /v1/audio/speech`) still serves clone models to a
  bearer client that **can** supply a reference voice. The fix is a portal-chat
  affordance change only, never a routing change.
- Covered by tests on both Go modules and the frontend; all quality gates green;
  docs/ADR updated in the same unit of work.

## Non-goals / anti-goals

- **Not** a routing gate. `speech_clone` must gate no route. A clone model stays a
  valid `/v1/audio/speech` candidate; if `speech_clone` ever refused a route it
  would break the direct API for clone users — a failure worse than the bug.
- **Not** a voices dropdown, a reference-voice upload, or any new composer control
  (that is Option 2 / ADR-063 (f)).
- **Not** a new storage path or DTO "task string". We reuse the existing capability
  machinery; a parallel task field would be a foreign pattern.
- No change to `handleOpenAISpeech`, the relay, billing, auth legs, or the WAV-only
  surface.

## The design decision (the one open axis in the issue)

The issue leaves the mechanism open: "refine the `(speech, yes)` verdict so the chat
can tell `tts` from `clone`, **or** add a task field to the model DTO."

**Decision: a new non-routing refinement capability `speech_clone`.** It is the
exact twin of `transcription_streaming` (ADR-065 / issue #262), which already
refines `transcription` from the same audio.cpp listing, gates no route, and is
produced by both the gateway health loop and the agent. We reuse that whole,
proven machinery:

- the ranked capability **source** (`audiocpp_models`, rank 1), with manual
  override still winning,
- the two-producer split (health loop + agent) with two parser copies pinned
  identical by one shared case table,
- the agent-ingest allow-list and reserved-manual-verdict guards,
- the fail-closed DTO fold pattern (`image`/`image_edit`/`speech`/`transcription`
  flags on `ModelDTO`).

Rejected alternative — **a `task` string on the model DTO**: it would need its own
storage and projection path outside the capability system, would not benefit from
the ranked-source / manual-override / change-detection guards, and would diverge
from every existing per-mapping capability fact. The capability-refinement approach
is strictly more idiomatic and smaller in net new concepts.

### Semantics of `speech_clone`

- **Meaning:** `(speech_clone, yes)` = "this is a clone-task speech model (needs a
  reference voice)". It **refines** `speech`: a row is written only for a model
  whose `speech` verdict is yes.
- **Derivation (per mapping, from the audio.cpp `task`):** `tts` → `(speech_clone,
  no)`; `clone` → `(speech_clone, yes)`; `asr` / music / any other task / no task /
  model-not-named → **no row** (unknown). This is bidirectional where the listing
  states the task, exactly like `speech`/`transcription`, because the listing names
  each model's task.
- **Gates no route.** No request ever declares it required; the `/v1/audio/speech`
  gate still asks only for `speech`.
- **DTO fold (`ModelDTO.SpeechClone`):** a gateway model name is `SpeechClone:
  true` when **any** of its offering mappings carries `(speech_clone, yes)`. This is
  the deliberate **inverse** of the fail-closed `speech`/`transcription` folds
  (which AND-down "can every mapping do X?"): `speech_clone` answers "should we
  **hide** this?", so it errs toward excluding a model that could land a chat send
  on a clone mapping. In practice `SpeechClone` only matters when `Speech` is true
  (all mappings tts-or-clone), and then it is true iff at least one is clone. For a
  group, true iff any offerable member is clone; inherited by an alias as the other
  flags are.
- **Fail-open to offering.** A speech model with no `speech_clone` row (unknown
  task, an agent too old to report it) folds `SpeechClone: false` → still offered.
  We only hide a model we **know** is clone; this is the status quo for anything we
  cannot classify, and the minimal change.

### Frontend

`chatModels` (ChatStore.tsx) admits an `openai_audio` model behind `speech === true
|| transcription === true`. The speech half becomes `speech === true &&
speechClone !== true`. `ModelOption` gains `speechClone?: boolean` from
`ModelDTO.SpeechClone`. A pure clone model is `speech:true, transcription:false`
(one task decides both), so excluding the speech half removes it entirely — there
is no transcription fallback to a broken speech composer. (`prospectiveKind`'s
speech branch reads the same `speech` flag, but an excluded clone model is no
longer in `chatModels`, so it cannot become the effective chat model that offers a
speech thread.)

## Constraints

- Keep the two `parseAudioCppModels` copies (gateway `internal/provider` and
  ServerAgent `internal/collector`) identical, pinned by the shared
  `audioCppModelCases` table duplicated in both modules' tests
  (`TestAudioCppModelsCasesMatchTheAgentsTable`). Changing the rule means changing
  both parsers and both table copies, identically.
- Bump ServerAgent `const Version` **once** (PATCH, 0.9.2 → 0.9.3): the agent's
  wire output gains a verdict row, but adds no `agent.Features` entry
  (`capability_source_audiocpp` already exists). This mirrors the
  `transcription_streaming` bump (0.9.1 → 0.9.2, ADR-065 / #262).
- All three store drivers must keep working; the capability row is a string column,
  so **no migration** is needed (the capability-name set is open). Verify the
  Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set, as CI does.
- i18n: only add German+English keys together if a new user-facing string is
  introduced (expected: none — the fix removes options, it adds no label).
- Repo-facing text English; never commit to or merge into `main`; remove
  `docs/superpowers/**` and `docs/implementation-status.md` before the PR.

## What is left to the builder (the plan will phase this)

1. **Capability constant + source doc** — `routing.CapabilitySpeechClone =
   "speech_clone"`; extend the `CapabilitySourceAudioCppModels` doc comment to say
   it writes the speech_clone row too.
2. **Gateway parser** — `AudioCppVerdicts.SpeechClone`; the `tts`→no / `clone`→yes
   rule; update the shared case table + sync test.
3. **Gateway row projection** — `audioCppCapabilityRows` writes the `speech_clone`
   row, guarded on `v.Speech == yes` (the refinement guard, as streaming is guarded
   on transcription yes).
4. **Agent-ingest allow-list** — add `CapabilitySpeechClone` to
   `audioCppProbeCapabilities`; update the rule-4 doc comment.
5. **Reserved manual verdict** — reserve `(speech_clone, no)` the way
   `(transcription_streaming, no)` is reserved (a derived refinement an operator
   should not hand-set to force-offer).
6. **DTO + fold** — `ModelDTO.SpeechClone`; a `speechCloneFlagsByName` (any-mapping
   OR) + its group/alias plumbing; wire into the listing assembly.
7. **ServerAgent** — mirror the parser + case table; report the `speech_clone`
   verdict under `audiocpp_models`; bump `const Version` to 0.9.3.
8. **Frontend** — `ModelOption.speechClone`; the `chatModels` speech-half filter;
   tests (ChatStore.test.tsx).
9. **Docs/ADR** — update ADR-061 (d), ADR-063 (e)/(h), the compat doc clone-task
   note (~§12 and §9), Risks §11.4, data-model/api-surface if they enumerate the
   flags; add a short ADR amendment recording that #252 resolved the ADR-063 (h)
   limitation via a `speech_clone` refinement.

Each phase gets a reviewer-gate (subagent) before the next; a whole-branch review
at the end; then full verification (both Go modules' golangci-lint, frontend
format:check + vitest + build, the Postgres store leg, local sonar
branch-findings), docs cleanup, and a PR against main with Auto-fix enabled.
