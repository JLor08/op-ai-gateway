# Detecting `timings_per_token` support — Design

**Status:** draft for review
**Date:** 2026-09-08
**Implements:** issue #52
**Builds on:** #51 (live per-request tokens/sec), whose retry-and-memo is the safety
net this design depends on and must not weaken.

## Goal

#51 gets an exact mid-stream output-token count by adding `timings_per_token` and
`stream_options.continuous_usage_stats` to the streaming request body the gateway
builds itself. Not every upstream tolerates an unknown body field, so #51 sends
optimistically to an allow-list of application types and retries once without the
parameters on a 400/422.

That works, but it discovers support by paying for a rejection, and it cannot serve
the cases where the application type says nothing about the answering engine:

- **`server_agent`** — the child is whatever the runtime spec launches, including
  `custom`.
- **`llama_swap`** — a model resolves either to a free-text `cmd` or to a **`peer`**
  at an arbitrary base URL (llama-swap's own config example is OpenRouter).

This design detects support instead, so the wasted round trip disappears and a
`custom` child that really is llama.cpp gets its live number back.

## 1. The decision rule

Three layers, each with a different quality of evidence. **Observation beats
prediction, prediction beats guessing, guessing beats silence.**

| State | Decision |
| --- | --- |
| Memo says this mapping's upstream **rejected** the parameters (#51, volatile, TTL'd) | do not send |
| Detected **supported** | send — overrides the application type entirely |
| Detected **unsupported** | do not send |
| **Unknown**, and the shape genuinely implies a tolerant upstream | send |
| **Unknown** otherwise | do not send |

"The shape genuinely implies a tolerant upstream" means exactly:

- application type `llama_cpp` or `vllm`; **or**
- application type `server_agent` **and** `routing.EffectiveRuntimeSpecType(spec)` is
  `llama_cpp` or `vllm`.

Everything else is unknown-and-silent: `server_agent` with an effective type of
`tgi`, `ollama` or `custom`; `llama_swap`; `litellm`; `ollama`; `mock`.

Two properties of that rule are worth stating because they are the reason it is
cheap and the reason it is safe:

**`EffectiveRuntimeSpecType` resolves the empty type.** `RuntimeSpec.Type == ""`
means "derive from the binary", and it is the value on every row predating the
runtime-manager work — which is why a *type-only* gate was rejected during #51's
review as delivering nothing. `EffectiveRuntimeSpecType`
(`internal/routing/runtime_spec_type.go:56`) falls back to
`DetectRuntimeSpecType(spec.Binary)`, so an untyped spec running `llama-server`
resolves to `llama_cpp`. The objection does not apply to the *effective* type.

**Every layer still sits behind #51's net.** A wrong "supported" — from a stale
persisted value, or from a binary-name guess — produces a 400, which #51's retry
turns into a non-event and the memo suppresses for that mapping. Nothing here may
change that: the detector reduces how often the net is needed, it never replaces it.

## 2. What is detected

The **presence of the key `timings_per_token`** inside llama.cpp's
`GET /props` → `default_generation_settings.params`.

That object is a serialization of the *compiled* request-schema field list, so the
key's presence asserts "this build's completion schema has that field" — not merely
"this looks like llama.cpp". Only presence is informative: the value is always
`false`, because the handler default-constructs the params struct.

`GET /props` is ungated on every llama.cpp build (unlike `/metrics`, which needs
`--metrics`), and it costs no inference, no tokens and no GPU time.

Absence, a failed fetch, a non-llama.cpp body, or a body we cannot parse all mean
**unsupported**, not unknown — a body that answered and did not contain the key is
evidence. Only "we never got to ask" is unknown.

> Verify every field against llama.cpp **source**, never `tools/server/README.md`:
> that README documents a pre-refactor `/props` shape and has already produced one
> wrong assumption in this codebase.

## 3. Where the answer is stored

**Persisted per mapping, exactly like `context_size`.** `model_mappings` gains a
tri-state text column and its own timestamp:

- `live_progress_support`: `""` (never determined) | `"supported"` | `"unsupported"`
- `live_progress_checked_at`

This follows `UpdateMappingContextProbe` (`internal/store/sqlite_applications.go:274`),
which persists a stable probe result on the mapping. Storing per mapping duplicates
the fact across several mappings of one multi-mapping llama.cpp application; that is
the same property `context_size` already has, and each row is corrected by its own
probe, so they cannot disagree for long.

**Not under `metrics_locked`, and it must not touch `metrics_source` or
`metrics_updated_at`.** All six existing automated writers on that table carry the
`where id = ? and metrics_locked = 0` guard. This one deliberately does not, because
`metrics_locked` exists so an operator can pin *numbers they answer for* — throughput,
context size. A build capability is not such a number: pinning it can only ever
produce a wrong answer, and unlike a pinned throughput a wrong capability has an
operational consequence — the feature silently stays off, with no visible reason,
until someone thinks to unlock a mapping's metrics. And because it is a capability
rather than a metric, writing it must not restamp the *metrics* provenance columns;
doing so would misattribute the mapping's throughput figures to `"probe"`.

**The deviation from the six-for-six convention must be stated in the code**, next to
the writer, or it will read as an oversight later.

**It is a refreshing cache, not a verdict.** The probe keeps running on its cadence
and keeps overwriting. Persistence exists only so that a gateway restart does not
start from nothing — which is what makes "silent until detected" cost nothing after
the first successful probe ever, rather than after every restart.

## 4. The gateway half — non-agent applications

`/props` is **already fetched today** for exactly the two ambiguous non-agent types:
a `llama_cpp` application defaults `context_probe_path` to `/props`, and a
`llama_swap` application defaults it to `/upstream/{model}/props` — per model, gated
on the loaded-model registry. The context pass runs on its own `"ctx:"+app.ID`
cadence key (`cmd/gateway/app_health.go:569`), expands `{model}` via
`provider.ExpandModelPath` (`:617`), and writes through
`UpdateMappingContextProbe` (`:634`, `:670`).

So the work is:

- `provider.ModelInfo` gains one field carrying the tri-state (it currently holds
  only the model name and `n_ctx`), and `parseModelInfo`
  (`internal/provider/model_info.go:69`) reads the key out of the body it already
  decodes and discards.
- the context pass writes it alongside the context size, through a **new** store
  method that does not carry the `metrics_locked` guard and does not touch the
  metrics provenance columns. `UpdateMappingContextProbe` keeps its own semantics
  unchanged.

An application whose operator blanked `context_probe_path` is never probed, so it
stays unknown — and §1's shape rule is what keeps a plain `llama_cpp` application
working there. That is the case the rule exists for.

## 5. The agent half — agent-managed children

This signal is **unreachable from the gateway** for an agent-managed child: the
agent's router answers 404 to any request whose JSON body names no managed model, so
`/props` cannot be proxied. Only the agent can read it, on the loopback probe it
already issues every collect cycle.

Two shape problems make this half larger than the gateway half, and the plan must
budget for them rather than treating it as one more field:

- **`collector.ProbeContext` is hard-typed to a number**:
  `ProbeContext(ctx, client, baseURL, specType, contextPath) (int, error)`
  (`server-agent/internal/collector/probe.go:94`), and every extractor beneath it
  returns `(int, bool)`. A tri-state capability cannot ride that chain. It needs a
  sibling probe with its own return contract, reusing the same HTTP fetch — not a new
  case in `extractContext`.
- **The per-child cache carries one success signal.** `runtimeCtxEntry{pid,
  specType, contextProbePath, size}` treats a cache hit as proof the context probe
  succeeded. Two values that can succeed and fail independently need separate hit
  tracking, or a second cache keyed the same way (pid + type + path), so a cached
  context size cannot imply a capability verdict it never established.

Then:

- report it as a new string field on `sample.RuntimeSample`, beside `MetricsProbe`
  and `ContextProbe` (`server-agent/internal/sample/sample.go:142-143`). Additive and
  byte-neutral for an older agent, which simply never populates it — and `""` already
  means unknown.
- mirror it onto `RuntimeStatusDTO` on ingest, and persist it against the mapping the
  spec belongs to, so it survives a gateway restart like the gateway-half value does.
- **probe `/props` even for a `custom`-typed child**, where the per-type table gives
  no context path. This is the case that a type-based rule refuses and the detector
  recovers, and it is one extra loopback GET per child lifetime, cached on the pid
  exactly like the context size. Without it, the agent half only confirms what the
  effective type already guessed.

## 6. The decision point

`Target` gains one field carrying the persisted tri-state. It costs **no extra
lookup**: `Resolver.targetFrom` (`internal/routing/resolver.go`) already receives the
whole `ModelMapping` by value, and already loads the `RuntimeSpec` for a
`server_agent` application — so both the persisted state and
`EffectiveRuntimeSpecType(spec)` are pure reads on data in hand, exactly like
`OpportunisticMetrics: app.OpportunisticMetricsEnabled` today.

The decision itself stays in `internal/provider`, where `wantsLiveProgress` and the
memo already live: routing carries data, the provider decides. This respects the
layering the arch tests enforce (`internal/routing` may not import
`store`/`portal`/`gateway`/`compat`).

`wantsLiveProgress` therefore becomes a function of the target's persisted state, its
provider type, and its effective spec type — with the memo consulted as it is today.
Its doc comment must be rewritten: after this change the allow-list is no longer even
a fallback for *sending*, it is the "shape implies a tolerant upstream" clause of §1.

## 7. Error handling and edge cases

- Everything stays advisory. A failed probe, an unparseable body, a missing column
  value and an older agent are all ordinary states, never errors, never logged as
  such.
- A **stale persisted "supported"** — an operator downgrades to a build without the
  field — produces a 400, which #51's retry absorbs, the memo suppresses for that
  mapping, and the next probe corrects. This is the reason persistence is safe at
  all, and the reason nothing in this design may weaken the retry.
- A **stale persisted "unsupported"** — an operator upgrades to a build that has the
  field — costs only a missing advisory number until the next probe overwrites it.
- llama-swap can **strip** request parameters (`stripParams` accepts any JSON key
  except `model`). So a detector may correctly report "supported" while the parameter
  never reaches the child. Harmless: no timings arrive and the cell shows the
  "never measured" em-dash. Do not treat it as a detector bug.
- A `llama_swap` model that is not currently loaded is not probed (the per-model path
  is loaded-gated), so it stays unknown and silent until it has been loaded and
  probed once. Self-correcting.
- The value is per mapping; deleting and recreating a mapping resets it to unknown,
  which is correct.

## 8. Testing

- The decision rule as a table over every combination of persisted state × provider
  type × effective spec type, including the pre-feature `""` spec type resolving from
  the binary. This is the heart of the design and the table is the test.
- The memo still wins over a persisted "supported" — the observation-beats-prediction
  property, asserted directly.
- `parseModelInfo` reports `supported` for a body containing the key,
  `unsupported` for a llama.cpp-shaped body without it, and leaves the value
  unknown only when nothing was fetched.
- The new store writer ignores `metrics_locked` and leaves `metrics_source` and
  `metrics_updated_at` untouched — asserted against a locked row, because that is
  the deliberate deviation from the table's convention.
- The agent reports the tri-state for a `custom`-typed child whose `/props` carries
  the key — the case the effective type refuses.
- An older agent that never populates the field leaves the persisted value alone
  rather than overwriting it with unknown.
- A cached context size does not imply a capability verdict (the separate-hit-tracking
  property from §5).
- Every new test must fail if its production change is reverted.

## 9. Also in scope

#51's final re-review found this and it was outside that wave's scope: the
gateway-derived rate in `liveProgressDTO` got a 50 ms floor on its generation window,
but the **passthrough** scanner's equivalent still guards only `genSecs > 0`. A very
fast Anthropic response whose authoritative `message_delta` arrives microseconds
after the first content frame could record an implausibly high rate — and that path
feeds the mapping's opportunistic throughput EWMA, which is a routing input. Add the
same floor.

## 10. Out of scope

- An operator-triggered capability benchmark that settles the question for any
  upstream, including ones with no `/props` at all (the shape `vision_capable` uses).
  That is the honest end state if this is extended past the llama.cpp family; it
  costs a real inference per probe and is not what #52 asks for.
- Reading anything else out of `/props`. Modality/capability auto-detect is #49's
  part 2 and has its own provenance questions.
- The parked defects filed separately: #53 (the `-race` failure on `main`), #54
  (Ollama's `/api/show` method), #55 (llama.cpp router mode's dummy `/props`), #56
  (the portal Chat's bytes-per-second `tps`).
