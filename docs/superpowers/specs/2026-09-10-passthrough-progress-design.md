# Live progress for native-passthrough requests

**Status:** approved (operator, 2026-09-10). Analysis and the code anchors live in issue #76; this
records the decisions, which the issue deliberately left open.

## 1. What is wrong

A translated streaming request shows TTFT and live tokens/sec in the running-connections panel; a
passthrough streaming request of the same model shows neither. Not missing data — a missing wire:
`requestProgress` is allocated only in `internal/gateway/stream_session.go` and `observeDelta` has
only that one caller, both on the translate path. So `ActiveRequest.Progress` is nil for passthrough
and `liveProgressDTO` returns zeros.

Meanwhile the passthrough scanner already parses every frame with its arrival timestamp, already
stamps the first content frame per flavor, and already merges whatever usage the upstream reports as
it arrives.

## 2. Decisions

**(a) A partially populated row, with the absence named — not an empty row and not a zero.**
This repo already has the vocabulary and the doctrine: a measured value must never be
indistinguishable from a measured zero, which is why `formatLiveTps` exists. The dual applies here —
a *not applicable* must never look like a zero either. So: render what is honestly available, and
render `—` for what has no source. An empty row would read as "unsupported"; a zero would read as
"slow".

**(b) Read the client's `timings_per_token` when it is present; never inject it.**
`liveProgressDTO` already prefers an upstream-reported rate over a derived one and already returns a
`source` string, so a client-dependent difference in completeness is *visible* rather than
mysterious — that is what makes this acceptable. Injecting the flag into a relayed body is
forbidden: passthrough means the client's body reaches the upstream unchanged apart from
`rewriteModelField`, and that rule gets a comment at the one place someone would be tempted to
break it.

**(c) The in-flight figure is display only.** The end-of-request rate already feeds
`UpdateMappingOpportunisticMetrics`' EWMA, which the scorer and a model group's `MinTokensPerSecond`
gate read. An in-flight sample is computed over a shorter window mid-generation and would not
self-correct if blended into a routing input. No write from this path.

## 3. What each flavor can honestly deliver

This asymmetry is the design, not a shortfall to be papered over.

| | `anthropic_messages` | `openai_responses` |
|---|---|---|
| TTFT | yes — the first-content stamp exists | yes — same |
| in-flight output tokens | yes — `message_delta` carries a cumulative `output_tokens`, already merged frame by frame | **no honest source**: the `*.delta` partials carry no usage, and counting deltas as tokens is rejected policy here ("no tokenizer to count with") |
| in-flight rate | derived over the window from the first content frame — llama.cpp attaches no `timings` to any Anthropic frame, so this is the only source, exactly as for this flavor's end-of-request rate | only when the **client** set `timings_per_token`, which makes llama.cpp attach `timings` to partial frames |

A test must pin both rows, so that a later change cannot quietly satisfy the Responses column by
counting deltas.

## 4. Out of scope

- Injecting `timings_per_token` (decision b).
- Any routing or EWMA input (decision c).
- Buffered passthrough: there are no frames to time and no first-content stamp can form, so it keeps
  today's behaviour — no progress at all.
- Chat completions: it has no passthrough mode; `endpointModeFor` resolves one only for
  `openai_responses` and `anthropic_messages`.
- Ollama-backed applications: native passthrough needs a provider implementing `NativeProxyClient`,
  which `OllamaClient` does not, so they never reach this path.

## 5. Verification

- A streaming passthrough request shows TTFT, and a rate wherever one is honestly available; a
  translated request's panel behaviour is byte-identical to today.
- A buffered passthrough request still produces no progress and does not panic on the nil path.
- The per-flavor completeness table is pinned by tests.
- **No EWMA or scorer write originates from this path** — asserted on the call count, not the value,
  because an identical write would otherwise hide.
- Per touched Go module: `golangci-lint fmt --diff` (must print nothing) + `run` +
  `go test ./... -count=1`. Frontend: `format:check` + `lint` + `build` + `test`. Docs:
  `check-docs.sh` + `check-docs.test.sh`. The PostgreSQL leg only if something store-shaped changes
  — it skips silently without the DSN.
