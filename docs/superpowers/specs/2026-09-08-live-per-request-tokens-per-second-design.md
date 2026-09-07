# Live per-request tokens/sec (and TTFT) — Design

**Status:** draft for review
**Date:** 2026-09-08
**Part of:** the issue #49 discussion, split into four sub-projects. This is
**sub-project 1** and is deliberately self-contained: it needs no `/metrics`
scraping, no probe, and no upstream capability we do not already reach.

## Goal

Two gaps, one cause.

1. **"Laufende Verbindungen" shows no throughput at all.** A row is registered at
   request start and carries only metadata; nothing about it changes until it
   disappears. An operator watching a long stream cannot tell a request that is
   generating from one that is stuck.
2. **"Aktivität" shows `0` where it means "not measured".** The completed-request
   column *Ausgabe (t/s)* is fed from the upstream's own `timings` object, which the
   translate path parses (`internal/provider/openai_compatible.go:131-133`,
   `:487-489`) but the **native passthrough** path never reads — so every
   `/v1/responses` and `/v1/messages` request persists `tokens_per_second = 0`.

Both are fixed by measuring per request. A per-request figure is also the only
*honest* one here: a `/metrics` throughput reading is per-process, so with several
concurrent requests on one model it is the same number on every row and belongs to
none of them.

## Non-goals

Explicitly out of scope, each tracked elsewhere in the #49 split: harvesting richer
telemetry from `/metrics` (live per-model throughput, prefix/KV cache); modality and
MTP/speculative-decoding detection; gateway-side probing of non-agent applications;
TTFT in the **completed** activity table; the portal Chat's `tps`, which counts
characters rather than tokens (`internal/gateway/chat_runs.go:550-557`); and the two
upstream defects found during research (Ollama's `/api/show` is a POST while the
agent probes it with GET; llama.cpp router mode returns a dummy `/props` with
`n_ctx: 0`).

---

## 1. The governing rule: never estimate

The obvious implementation — count streamed SSE deltas and call it tokens — is
rejected. Verified against upstream behaviour, its error is not a rounding
concern:

- **~100 % undercount on tool-call turns.** `openai_compatible.go:502` emits a text
  event only when `d.Content != "" || reasoning != ""`; tool-call argument
  fragments accumulate silently (`:506-524`) and surface only after the stream
  ends (`:534-537`). An agent turn that is all tool calls would count as zero.
- **50–75 % undercount under speculative decoding**, where one chunk carries several
  tokens.
- llama.cpp emits **no frame at all** for a token whose text ends mid-UTF-8 and
  flushes those bytes inside a later frame, and one decoded token maps to 0..N
  `chat.completion.chunk` frames.
- The Go backend has no tokenizer, so the gateway cannot count tokens itself.

Therefore: **a rate is shown only when the number of output tokens behind it was
reported exactly by the upstream.** Otherwise the cell shows the shared
"never measured" glyph `—`. This is the same discipline #50 established for
Aktiv/Warteschlange, where `probeOk` gates on whether the measurement happened
rather than on the value.

## 2. Where the numbers come from, per path

The gateway builds the upstream request body itself on the **translate** path
(`openai_compatible.go:376-381`), so adding a parameter there modifies nothing the
client sent. On the **native passthrough** path the client's body is forwarded
byte-for-byte and must stay that way — `rewriteModelField` is the single sanctioned,
value-lossless edit — so nothing is added there.

| Path / upstream | Mid-stream | Live figure |
| --- | --- | --- |
| Translate → `llama_cpp`, `llama_swap`, `server_agent` | `timings_per_token: true` makes llama.cpp attach its `timings` object to **every** chunk, carrying `predicted_n` (its exact cumulative output-token count) and `predicted_per_second` (its own rate) | tokens/s **upstream-reported**; TTFT gateway-stamped |
| Translate → `vllm` | `stream_options.continuous_usage_stats: true` puts vLLM's exact running `completion_tokens` on every chunk; vLLM reports no rate | exact count, rate **gateway-derived**; TTFT gateway-stamped |
| Translate → `ollama` | nothing — the count exists inside Ollama (`Timings.PredictN`) but no client-facing switch exposes it | `—` for t/s; TTFT gateway-stamped |
| Translate → `litellm` | parameters must **not** be sent (see §3) | `—` for t/s; TTFT gateway-stamped |
| Native passthrough (`/v1/responses`, `/v1/messages`) | body is forwarded verbatim; the copier reads raw 32 KiB buffers and decodes no frame | `—` for t/s and TTFT |
| Non-streaming (any) | one blocking round trip | `—` |

TTFT is available wherever the gateway sees a first content delta, i.e. on every
streaming translate path including Ollama and litellm — it needs no upstream
cooperation, only a timestamp.

## 3. The one dangerous parameter, and its allow-list

`timings_per_token` and `stream_options.continuous_usage_stats` are safe on
llama.cpp (its request schema is pull-based: it iterates its own field list and
looks each name up, so unknown keys are never inspected) and on vLLM
(`OpenAIBaseModel` is `ConfigDict(extra="allow")`; `continuous_usage_stats` is a
supported `StreamOptions` field).

They are **not** universally safe. LiteLLM forwards unknown body keys downstream
rather than ignoring them, packing them into `extra_body` for OpenAI/Azure, which
answer with a documented 400 *"Unrecognized request argument supplied"* — and this
client turns that into an unavailable-upstream failure for the **whole request**.
The same applies to an unknown sub-key of `stream_options`, which is otherwise a
known OpenAI field.

Both parameters are therefore gated by **one** allow-list, in one helper, so they
cannot drift apart:

```
liveProgressUpstreams = { ProviderLlamaCPP, ProviderLlamaSwap, ProviderVLLM, ProviderServerAgent }
```

keyed on `target.Provider` (which is `app.Type`, `internal/routing/resolver.go:1072`).
Every other provider value — `litellm`, `ollama`, `mock`, and anything added later —
gets neither parameter. The default is **off**; an unlisted type is never a silent
opt-in. The helper carries the upstream citation for each entry in a comment, in the
style of `routing/runtime_spec_type.go`'s probe-path table.

Both parameters are sent only on **streaming** requests. Non-streaming responses
already carry terminal timings (`openai_compatible.go:131-133`).

## 4. Data flow

```
provider stream loop (openai_compatible.go)
  per chunk: timings.predicted_n / predicted_per_second, or usage.completion_tokens
    -> inference.StreamEvent{ Type: TextDelta, Progress: *StreamProgress }
         -> streamSession.stream()'s wrapper (stream_session.go:184-191)
              -> requestProgress atomics on the ActiveRequest
                   -> activeRequestDTO (GET /api/portal/usage/active)
                        -> "Laufende Verbindungen": t/s + TTFT columns
```

### 4.1 A new, dedicated event field

`inference.StreamEvent` already carries `Usage *Usage` on every event type
(`internal/inference/types.go:189-197`), and reusing it would need no type change —
but today `Usage` is attached only to the terminal `StreamEventCompleted`
(`openai_compatible.go:538`) and the client-facing usage chunk is gated on
`req.IncludeUsage` at `internal/gateway/inference_complete.go:171`. Putting a
mid-stream value into the same field would overload a settled meaning.

Add instead:

```go
// StreamProgress is the running, upstream-reported progress of a stream, carried
// on intermediate events. It is advisory: absent whenever the upstream does not
// report an exact count, and never a substitute for the terminal Usage.
type StreamProgress struct {
    OutputTokens    int     // exact cumulative count reported by the upstream
    TokensPerSecond float64 // upstream-reported rate; 0 when the upstream reports none
}
```

and a `Progress *StreamProgress` field on `StreamEvent`. Nil on every event that
carries no upstream-reported count, which is the normal case.

### 4.2 The provider change

Two edits in `openai_compatible.go`'s stream loop, both additive:

1. **Hoist the per-chunk `Timings` read out of `if chunk.Usage != nil` (`:476`).**
   Today the timings parse sits inside that guard (`:487-489`), and llama.cpp
   attaches timings to partial chunks that carry *no* usage object — so per-chunk
   timings are discarded today. This is dead code being brought to life, which is
   why its test must be written to fail without the fix (§8).
2. **Extend the chunk struct** (`:462-465`) with `predicted_n`, and populate
   `Progress` on the emitted text-delta event from either `timings.predicted_n` +
   `predicted_per_second` (llama.cpp) or `usage.completion_tokens` (vLLM's
   continuous usage).

The terminal path is untouched: with `include_usage` set — which the repo already
sends unconditionally at `:379` — llama.cpp puts the final timings on the usage
chunk, exactly where `:487-489` reads them.

vLLM's per-chunk continuous usage omits `prompt_tokens_details`, so `CachedTokens`
would read 0 mid-stream. This is harmless because the existing terminal parse
overwrites the whole usage struct from the final chunk, which arrives last — but the
mid-stream path must write only `Progress`, never `usage`.

### 4.3 The counter

`activeRegistry` holds `items map[string]ActiveRequest` — a **value** map with no
update method (`internal/gateway/active_requests.go:62-69`) — and its single
`RWMutex` is on the routing hot path: `routing.Resolver` calls `ServerActivity`
at four sites (`resolver.go:797, 907, 984, 1012`), three of them inside
per-candidate loops. A write lock per token would serialize routing behind the
token rate. Independently, `atomic.Int64` embeds `noCopy`, so an atomic field in a
value map would make every existing copy and range (`Snapshot`, `CountByServerName`,
`ServerActivity`, the DTO loops) a `copylocks` violation under the repo's
golangci-lint gate.

So the atomics must live **behind a pointer**, never as fields of a struct that is
copied. That needs exactly one change, not two: `ActiveRequest` gains
`Progress *requestProgress`, allocated at each of the three `Add` sites
(`stream_session.go:111`, `inference_complete.go:52`,
`native_passthrough.go:302`) and held by the writer as well.

`items` stays `map[string]ActiveRequest`. Copying an `ActiveRequest` copies the
pointer, not the atomics, so `copylocks` is satisfied and `Snapshot` keeps returning
values — every copy simply shares the one live counter, which is precisely what the
DTO builder needs. Turning the map into `map[string]*ActiveRequest` would work too
but is unnecessary churn on `Add`, `Remove`, `Snapshot`, `CountByServerName` and
`ServerActivity`.

```go
// requestProgress is one in-flight request's live counters. Written by the single
// goroutine that owns the stream, read lock-free while the DTO is built.
type requestProgress struct {
    outputTokens       atomic.Int64 // exact upstream-reported count; 0 = none reported
    upstreamTPSMilli   atomic.Int64 // upstream-reported rate x1000; 0 = none reported
    firstTokenUnixNano atomic.Int64 // first content delta; 0 = none yet
}
```

The rate is stored as milli-tokens/second in an integer rather than float bits in an
`atomic.Uint64` — one decimal is displayed, so milli precision is ample, and it
avoids bit-punning.

Precedent for this shape: `chatRunRegistry` holds `map[string]*ChatRun` and mutates
per delta through the **run's own** lock, never the registry's
(`chat_runs.go:121, 168-184, 221-226`); `runtimeLogSub.dropped atomic.Int64` is a
hot-path counter on a per-subscriber object (`runtime_logs.go:252-254`).

`Snapshot` (`:158-169`) currently returns frozen copies; with a pointer field it no
longer does, and its doc comment must say so. Seven test sites construct
`ActiveRequest{}` literals with no progress pointer, so every read must be nil-safe —
consistent with the file's stated nil-safe posture (`:59-61`).

### 4.4 Where the writer runs

- **Translate, all three flavors at once:** `streamSession.stream()`
  (`stream_session.go:184-191`) wraps the provider callback for chat-completions,
  Responses and Anthropic translate alike, and a session is documented as
  single-goroutine (`:38-41`). One hook there covers all three: stamp
  `firstTokenUnixNano` on the first event with non-empty `Text` or `Reasoning`, and
  store `outputTokens` / `upstreamTPSMilli` whenever `ev.Progress != nil`.
- **Native passthrough:** nothing. No parameter may be added to the forwarded body,
  and the copier decodes no frames.
- **Non-streaming:** nothing to write.

### 4.5 Resolution at DTO build time

`handlePortalUsageActive` builds DTOs outside any registry lock, over `Snapshot`'s
result. Per row:

- `output_tokens` = `outputTokens` (0 when nothing was reported).
- If `upstreamTPSMilli > 0` → `tokens_per_second = upstreamTPSMilli/1000`,
  `tokens_per_second_source = "upstream"`.
- Else if `outputTokens > 0 && firstTokenUnixNano > 0` →
  `tokens_per_second = outputTokens / (now − firstToken)`,
  `tokens_per_second_source = "gateway"`.
- Else → `tokens_per_second = 0`, `tokens_per_second_source = ""`.
- `ttft_ms` = `firstToken − StartedAt` in milliseconds when `firstTokenUnixNano > 0`,
  else 0.

A gateway-derived rate is therefore only ever computed over an **exact** upstream
count — the §1 rule made structural rather than merely documented.

New `activeRequestDTO` fields (the struct is at `active_requests.go:171-192`):

```
output_tokens              int     // 0 = not reported
tokens_per_second          float64 // 0 = not measured
tokens_per_second_source   string  // "upstream" | "gateway" | "" (not measured)
ttft_ms                    int64   // 0 = not measured
```

`tokens_per_second_source` follows the repo's `*_source` provenance convention
(`usage_events.energy_source` is `measured|estimated|modeled`). It is a plain string,
not `omitempty`, so an absent measurement is an explicit `""` on the wire rather than
a missing key — the same choice #50 made for `metrics_probe`.

## 5. Refresh cadence

`/api/portal/usage/active` is refetched only on the payload-free SSE pokes fired at
request start and end; the panel's 1 s timer merely re-renders the elapsed column
(`ActiveRequestsPanel.tsx:36-47`). During a single long stream there are no pokes at
all, so an injected value would freeze for exactly the case this feature exists for.

**Chosen: a client-side interval poll of `/api/portal/usage/active` every 2 s, gated
on there being at least one running row.** The panel already has that gate for its
elapsed ticker, so an idle gateway costs nothing and the existing start-poke re-arms
it. The poll lives in `useActivityData` beside `loadActive` (`:150-162`), which
already assembles the scope/user/token filters and holds the monotonic
latest-wins guard; putting it in the panel would require new props it does not have.

Cost does **not** scale with the number of running connections — one poll returns
every row the caller may see. It scales with open Activity tabs: 30 requests/minute
per tab while something is running, zero otherwise. Server-side each poll is one
scope check, one `RLock` snapshot copy, an in-memory sort/filter, and one display-name
lookup per *distinct* in-flight user — no `usage_events` query.

Interval: the repo's polling precedent is 3 s (`ModelServersSection.tsx:149-161`),
but the elapsed column in the same row ticks every second, and at 3 s the throughput
cell would visibly lag its neighbour. 2 s halves that without tripling the request
rate.

**Rejected — more frequent SSE pokes.** One payload-free `Publish()` is a scope-blind
server-wide fan-out, and each poke costs 3–4 HTTP requests *per open Activity tab*,
two of them aggregate `usage_events` queries; it also increments the "new requests"
pill once per signal, which would then lie. It is the most expensive option, not the
cheapest.

**Rejected — values on the usage SSE frame.** The broker is payload-free by design
precisely so no data crosses a user boundary; the frame is literally `data: {}`. This
is a documented invariant. (If push is ever wanted, the blessed shape is a separate
endpoint on the `/api/portal/model-servers/events` pattern, which recomputes per
subscriber under that subscriber's own token.)

## 6. Portal

Two new columns in `ActiveRequestsPanel`'s `activeColumns` catalogue, before the
existing elapsed column:

- **Tokens/s (live)** — `formatMetric(tokens_per_second, 1)`, so a not-measured row
  renders `—` and sorts as missing in both directions rather than as zero
  (`shared/format.ts:129-146`, `shared/ListTable.tsx:254-267`). A tooltip states the
  provenance: upstream-reported, or computed by the gateway — and in the gateway
  case it names the exact upstream token count the rate was computed from, which is
  what `output_tokens` on the wire is for. The provenance lives in the tooltip,
  never baked into the number.
- **TTFT** — `ttft_ms` when > 0, else `—`.

New i18n keys in both locales (`de`, `en`), following the existing
`activityColTokenSpeed` / `activityActiveElapsed` naming.

Also on this surface, for consistency rather than as a separate feature: the
completed table renders `tokens_per_second` and `prompt_per_second` as `0.0` today
(`ActivityTable.tsx:88-91`) while its own sibling energy and cost cells use `—`
(`:110-112`). Both are switched to the shared `—` convention. Without this the same
metric would render two different ways on one screen.

## 7. Native passthrough: the completed figure

Two changes on that path, neither of which touches the request:

1. **Read `timings` in `parsePassthroughUsage`** (`native_passthrough.go:472-545`)
   for the Responses shape. llama.cpp attaches its `timings` object to the terminal
   `response.completed` frame, so `/v1/responses` stops persisting `0`.

   The Anthropic shape carries no timings on any frame, so `/v1/messages` falls back
   to a gateway-derived rate, following the precedent in
   `benchmark_runner.go:113-118`. That rate is computed over the **generation
   window** — from the first content frame to completion — not over the whole
   request, so it measures the same quantity as every other path rather than
   silently folding in queueing and prompt processing. The incremental scanner in
   change 2 is what makes the first content frame observable; without it this
   fallback would have to be dropped rather than computed over the wrong window.
2. **Decouple usage scanning from the capture budget.** `respBuf` is capped at 1 MiB
   (`server.go:42`, applied at `native_passthrough.go:400`) and the file's own
   comment admits that a response larger than the cap "may lose a trailing usage
   frame" — so today even the **final** token count is silently dropped on long
   passthrough streams, independently of throughput. Usage parsing moves to an
   incremental scan over the chunks as they pass, carrying only the trailing partial
   line between chunks (bounded, discarded if it ever exceeds the cap, so a
   pathological stream cannot grow it without limit). `parsePassthroughUsage`'s
   `take` is already monotonic (a max, `:479-483`), so re-parsing overlapping data is
   safe by construction. Capture keeps its own cap and its own purpose.

Once that scanner exists, a **live** TTFT on the passthrough path becomes a small
further step — the first content frame is already being observed, and it would only
need writing into `requestProgress`. It is deliberately not taken here: §2's boundary
is that the passthrough path stays untouched on the request side and gains no live
row values in this sub-project, and widening it would pull the passthrough copier
into the live-counter contract for one field. Noting it so a reviewer reads the `—`
in §2 as a choice rather than an oversight.

## 8. Testing

Every test below must fail if its logic is removed. Two need particular care because
they would otherwise pass vacuously:

- **The allow-list.** A table test over every `Provider*` constant asserting the two
  parameters are present for `llama_cpp`, `llama_swap`, `vllm`, `server_agent` and
  **absent** for `litellm`, `ollama` and `mock`. This is the one change that can
  break real traffic, and a test that only checks the positive cases would not catch
  the regression that matters.
- **Per-chunk timings without usage.** A stream fixture whose intermediate chunks
  carry `timings` and **no** `usage` object must produce a non-zero live figure.
  Written against today's code this test fails, because the parse is nested inside
  `if chunk.Usage != nil` — that failure is the point.

Further:

- The terminal `tokens_per_second` is unchanged for a stream that carries usage and
  timings only in the final chunk (regression guard on the existing behaviour).
- `tokens_per_second_source` is `""` and the rate 0 for Ollama, litellm, native
  passthrough and non-streaming requests; `"upstream"` for llama.cpp; `"gateway"`
  for vLLM.
- A gateway-derived rate is never produced from an unreported count: with
  `outputTokens == 0` the source stays `""` however much time has passed.
- TTFT is stamped on the first delta with content or reasoning, and **not** by a
  role-only or empty delta.
- A native-passthrough response larger than the capture cap still yields its final
  token count.
- `ActivityTable` renders `—`, not `0.0`, for an unmeasured rate.
- A race test (`-race`) with the DTO builder reading while a stream writes.
- Frontend: the live column renders `—` for a row with no measurement and the number
  with one decimal otherwise; the poll is armed only while rows exist and is torn
  down when the list empties.

## 9. Error handling

Everything here is advisory. No live figure may fail, delay, or alter a request.

- An upstream that ignores the parameters simply reports nothing; the row shows `—`.
  llama.cpp had a window in September 2025 (PR #15827 → #15879) in which
  `timings_per_token` was a silent no-op, so **absent per-chunk timings is a normal
  state, never an error** and never logged as one.
- A nil `Progress` pointer, a nil `requestProgress`, and a zero counter are all
  ordinary; the DTO builder is nil-safe throughout.
- The poll inherits the panel's existing error handling; a failed poll leaves the
  last values on screen and the next tick retries.
- Scope is unchanged: `/usage/active` already returns only the caller's own rows
  unless they hold the admin scope. Because the figure is per request rather than a
  model-wide aggregate, it discloses nothing about other users' load — which a
  `/metrics`-derived number would have.

## 10. Documentation

`docs/architecture/cross-cutting/telemetry-usage-observability.md` gains the live
figure's provenance rule and the poll cadence beside its existing description of the
running-connections view; `docs/architecture/reference/api-surface.md` gains the four
new `/api/portal/usage/active` fields.
