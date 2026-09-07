# Live per-request tokens/sec (and TTFT) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a truthful per-request tokens/sec and time-to-first-token on the
running-connections panel, and stop the completed-activity table reporting `0`
tokens/sec for native-passthrough traffic.

**Architecture:** The gateway asks the upstreams that tolerate it for exact
mid-stream token counts (llama.cpp `timings_per_token`, vLLM
`stream_options.continuous_usage_stats`) on the request body it builds itself,
carries them on a new `StreamEvent.Progress` field, and stores them in a small
per-request struct of atomics reached by pointer from the in-flight registry. The
DTO resolves them into a rate plus a provenance string; the portal polls the
existing endpoint every 2 s while rows exist. Nothing is estimated: a rate appears
only when an exact upstream count is behind it.

**Tech Stack:** Go 1.x backend (`gateway/backend`), React + TypeScript + MUI portal
(`gateway/frontend`). No new dependencies.

**Design spec:** `docs/superpowers/specs/2026-09-08-live-per-request-tokens-per-second-design.md`

## Global Constraints

- **Never estimate.** A tokens/sec value may be produced ONLY from an output-token
  count the upstream reported exactly. Counting stream deltas, characters, or bytes
  as tokens is forbidden anywhere in this plan.
- `tokens_per_second_source` has exactly three values: `upstream`, `gateway`, `""`
  (not measured). It is a plain Go `string` with a snake_case json tag and **no**
  `omitempty`, so "not measured" is an explicit `""` on the wire.
- The live-progress request parameters go ONLY to these `routing.Provider*` values:
  `llama_cpp`, `llama_swap`, `vllm`, `server_agent`. Never to `litellm`, `ollama`,
  `mock`, or any value added later. Both parameters share one allow-list.
- **The native-passthrough request body is never modified** beyond the existing
  `rewriteModelField`. Everything on that path comes from reading the response.
- Everything here is advisory: no live figure may fail, delay, or alter a request.
  Absent per-chunk timings is a normal state, never an error and never logged as one.
- All new JSON fields are snake_case. Frontend types mirror them verbatim with no
  mapper.
- Frontend: every new user-visible string gets a key in BOTH `de` and `en`; a
  never-measured metric renders via `formatMetric` (the shared `—` glyph), never
  `0.0`.
- Every new or changed test must FAIL if its production change is reverted. Verify
  this by temporarily reverting, running the test, and restoring.
- Before finishing a task, run from each module you touched:
  `golangci-lint fmt --diff` and `golangci-lint run` (CI runs both and they are
  stricter than `gofmt`/`go vet`), plus `go test ./...`. Frontend:
  `npm run format:check`, `npm run lint`, `npm run build`, `npm test`.
- Never commit to `main`. `docs/superpowers/` is branch-local and is removed before
  the PR.

---

## File Structure

**Backend — created**
- `gateway/backend/internal/provider/live_progress.go` — the allow-list and its
  upstream rationale. One place, so the two parameters cannot drift apart.
- `gateway/backend/internal/provider/live_progress_test.go`
- `gateway/backend/internal/gateway/request_progress.go` — `requestProgress`, the
  per-request atomics, and their resolution into wire values.
- `gateway/backend/internal/gateway/request_progress_test.go`
- `gateway/backend/internal/gateway/passthrough_usage_scan.go` — the incremental
  usage scanner that frees usage accounting from the capture cap.
- `gateway/backend/internal/gateway/passthrough_usage_scan_test.go`

**Backend — modified**
- `internal/inference/types.go` — `StreamProgress` + `StreamEvent.Progress`.
- `internal/provider/openai_compatible.go` — parameters, `predicted_n`, the
  progress computation, `Progress` on delta events.
- `internal/gateway/active_requests.go` — `ActiveRequest.Progress`, four DTO fields,
  the resolution call, `Snapshot`'s doc comment.
- `internal/gateway/stream_session.go` — allocate the progress struct, thread it,
  observe deltas in `stream()`.
- `internal/gateway/inference_complete.go` — allocate at its `Add` site.
- `internal/gateway/native_passthrough.go` — allocate at its `Add` site; use the
  scanner; read `timings` for the Responses shape; the Anthropic fallback rate.

**Frontend — modified**
- `src/api/usage.ts` — four fields on `ActiveRequest`.
- `src/components/useActivityData.ts` — the 2 s poll.
- `src/components/ActiveRequestsPanel.tsx` — two columns and the provenance tooltip.
- `src/components/ActivityTable.tsx` — `—` instead of `0.0`.
- `src/i18n.ts` — five keys in `de` and `en`.

**Docs — modified**
- `docs/architecture/cross-cutting/telemetry-usage-observability.md`
- `docs/architecture/reference/api-surface.md`

---

### Task 1: Provider — request exact counts and surface them as progress

**Files:**
- Create: `gateway/backend/internal/provider/live_progress.go`
- Create: `gateway/backend/internal/provider/live_progress_test.go`
- Modify: `gateway/backend/internal/inference/types.go` (the `StreamEvent` block at
  `:189-197`)
- Modify: `gateway/backend/internal/provider/openai_compatible.go` (`CompleteStream`
  body at `:375-381`; chunk struct at `:454-465`; parse at `:476-490`; delta emit at
  `:502-504`)
- Test: `gateway/backend/internal/provider/openai_compatible_test.go`

**Interfaces:**
- Produces: `inference.StreamProgress{OutputTokens int; TokensPerSecond float64}` and
  `inference.StreamEvent.Progress *StreamProgress`, non-nil only on text-delta events
  that carried an exact upstream count. Task 2 consumes it.
- Consumes: `routing.Provider*` constants (`internal/routing/store.go:14-24`).

- [ ] **Step 1: Write the failing allow-list test**

`live_progress_test.go` — a table over every provider constant, so a value added
later fails loudly rather than silently opting in:

```go
func TestWantsLiveProgressAllowList(t *testing.T) {
	cases := map[string]bool{
		routing.ProviderLlamaCPP:    true,
		routing.ProviderLlamaSwap:   true,
		routing.ProviderVLLM:        true,
		routing.ProviderServerAgent: true,
		// LiteLLM forwards unknown body keys to OpenAI/Azure, which answer 400
		// and fail the WHOLE request. This entry is the point of the test.
		routing.ProviderLiteLLM: false,
		routing.ProviderOllama:  false,
		routing.ProviderMock:    false,
		"":                      false,
		"something_new":         false,
	}
	for provider, want := range cases {
		if got := wantsLiveProgress(routing.Target{Provider: provider}); got != want {
			t.Fatalf("wantsLiveProgress(%q) = %v, want %v", provider, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/provider/ -run TestWantsLiveProgressAllowList`
Expected: FAIL, `undefined: wantsLiveProgress`.

- [ ] **Step 3: Create the allow-list**

`live_progress.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import "op-ai-gateway/internal/routing"

// liveProgressUpstreams lists the application types verified to tolerate the two
// extra parameters this gateway adds to the STREAMING body it builds itself:
// llama.cpp's `timings_per_token` and `stream_options.continuous_usage_stats`.
// Both exist to get an EXACT mid-stream output-token count; without one, the
// portal shows "not measured" rather than a guess.
//
// The gate exists because tolerance is not universal:
//   - llama.cpp: its request schema is PULL-based (it iterates its own field list
//     and looks each name up), so a key nobody asks for is never inspected. Its
//     `stream_options` is a nested field that reads only its own subfields, so the
//     unknown `continuous_usage_stats` subkey is inert there.
//   - vLLM: OpenAIBaseModel is ConfigDict(extra="allow") and merely debug-logs
//     unknown keys; `continuous_usage_stats` is a first-class StreamOptions field
//     (inert unless `include_usage` is also set, which this client always sets).
//   - LiteLLM: FORWARDS unknown body keys downstream, packing them into
//     `extra_body` for OpenAI/Azure, which answer 400 "Unrecognized request
//     argument supplied" -- and this client turns that into an
//     unavailable-upstream failure for the ENTIRE request. Never send them there.
//
// The default is therefore OFF. A provider value that is not listed gets neither
// parameter, so a type added later is never a silent opt-in.
var liveProgressUpstreams = map[string]struct{}{
	routing.ProviderLlamaCPP:    {},
	routing.ProviderLlamaSwap:   {},
	routing.ProviderVLLM:        {},
	routing.ProviderServerAgent: {},
}

// wantsLiveProgress reports whether the streaming request body for target may
// carry the live-progress parameters.
func wantsLiveProgress(target routing.Target) bool {
	_, ok := liveProgressUpstreams[target.Provider]
	return ok
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/provider/ -run TestWantsLiveProgressAllowList`
Expected: PASS.

- [ ] **Step 5: Add the progress type to the inference contract**

In `internal/inference/types.go`, beside `StreamEvent`:

```go
// StreamProgress is a running, UPSTREAM-REPORTED measurement of a stream in
// flight. It is advisory and is attached only to intermediate events whose chunk
// actually carried an exact count -- it is never derived, never estimated, and
// never a substitute for the terminal Usage on StreamEventCompleted.
type StreamProgress struct {
	// OutputTokens is the upstream's own cumulative generated-token count
	// (llama.cpp `timings.predicted_n`, vLLM's continuous `completion_tokens`).
	OutputTokens int
	// TokensPerSecond is the upstream's own rate. 0 when the upstream reports a
	// count but no rate (vLLM); the consumer then derives a rate from the count.
	TokensPerSecond float64
}
```

and one field on `StreamEvent`:

```go
	Progress     *StreamProgress `json:"progress,omitempty"`
```

- [ ] **Step 6: Write the failing per-chunk-timings test**

This is the second high-risk test: today the timings parse is nested inside
`if chunk.Usage != nil`, so a chunk with timings and no usage is discarded. Write
the fixture so it FAILS against current code.

In `openai_compatible_test.go`:

```go
// llama.cpp with timings_per_token attaches its timings object to PARTIAL chunks,
// which carry no usage object at all. Before this change that branch was dead.
func TestCompleteStreamProgressFromChunkTimingsWithoutUsage(t *testing.T) {
	stream := "data: " + `{"choices":[{"delta":{"content":"Hallo"}}],"timings":{"predicted_n":7,"predicted_per_second":42.5}}` + "\n\n" +
		"data: [DONE]\n\n"
	// ... serve `stream`, call CompleteStream against a llama_cpp target ...
	var got *inference.StreamProgress
	// in the emit callback: if ev.Type == inference.StreamEventTextDelta { got = ev.Progress }
	if got == nil {
		t.Fatal("no progress on the text delta: per-chunk timings were dropped")
	}
	if got.OutputTokens != 7 || got.TokensPerSecond != 42.5 {
		t.Fatalf("progress = %+v, want {7 42.5}", *got)
	}
}
```

Add a sibling for vLLM's shape (an exact count, no rate):

```go
func TestCompleteStreamProgressFromContinuousUsage(t *testing.T) {
	// chunk: {"choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}
	// want: Progress{OutputTokens: 3, TokensPerSecond: 0}
}
```

And the parameter assertions, mirroring the existing `stream_options` assertion at
`openai_compatible_test.go:307-309`:

```go
func TestCompleteStreamSendsLiveProgressParamsOnlyForAllowedUpstreams(t *testing.T) {
	// For Provider llama_cpp: body has timings_per_token == true and
	//   stream_options == {"include_usage": true, "continuous_usage_stats": true}
	// For Provider litellm: body has NO "timings_per_token" key and
	//   stream_options == {"include_usage": true} exactly.
}
```

- [ ] **Step 7: Run them and confirm they fail**

Run: `go test ./internal/provider/ -run 'CompleteStreamProgress|LiveProgressParams'`
Expected: FAIL — no `Progress` field is ever set, and no parameter is sent.

- [ ] **Step 8: Send the parameters**

In `CompleteStream`, immediately after the `body` literal at `:375-381`:

```go
	if wantsLiveProgress(target) {
		// Ask for an EXACT running output-token count mid-stream. llama.cpp then
		// attaches its timings object (predicted_n + predicted_per_second) to every
		// partial; vLLM puts its running completion_tokens on every chunk.
		// continuous_usage_stats is inert without include_usage, which is set above
		// and must stay set. See live_progress.go for why this is gated.
		body["timings_per_token"] = true
		body["stream_options"] = map[string]any{
			"include_usage":          true,
			"continuous_usage_stats": true,
		}
	}
```

- [ ] **Step 9: Read the count and emit the progress**

Extend the chunk's `Timings` struct at `:462-465` with the count:

```go
			Timings *struct {
				PromptPerSecond    float64 `json:"prompt_per_second"`
				PredictedPerSecond float64 `json:"predicted_per_second"`
				PredictedN         int     `json:"predicted_n"`
			} `json:"timings"`
```

Then, after the existing `if chunk.Usage != nil { ... }` block (leave that block
exactly as it is — it owns the terminal usage), add:

```go
		// Running, upstream-reported progress. Computed OUTSIDE the usage branch on
		// purpose: llama.cpp attaches timings to partial chunks that carry no usage
		// object, which is why this used to be dropped. Never derived here -- a
		// chunk that reports no exact count produces no progress at all.
		var progress *inference.StreamProgress
		switch {
		case chunk.Timings != nil && (chunk.Timings.PredictedN > 0 || chunk.Timings.PredictedPerSecond > 0):
			progress = &inference.StreamProgress{
				OutputTokens:    chunk.Timings.PredictedN,
				TokensPerSecond: chunk.Timings.PredictedPerSecond,
			}
		case chunk.Usage != nil && chunk.Usage.CompletionTokens > 0:
			// vLLM's continuous usage: an exact running count, no rate.
			progress = &inference.StreamProgress{OutputTokens: chunk.Usage.CompletionTokens}
		}
```

and attach it to the text-delta emit at `:502-504`:

```go
			if d.Content != "" || reasoning != "" {
				if err := emit(inference.StreamEvent{
					Type:      inference.StreamEventTextDelta,
					Text:      d.Content,
					Reasoning: reasoning,
					Progress:  progress,
				}); err != nil {
					return err
				}
			}
```

Progress rides on content deltas only. A chunk that carries a count but no content
(the terminal usage chunk, a role-only first delta) simply does not forward one; the
next content delta carries a fresh value, and the terminal `Usage` is unaffected.

- [ ] **Step 10: Pin the truncated-stream consequence**

With `continuous_usage_stats`, `chunk.Usage` is now non-nil on EVERY vLLM chunk, so
the existing terminal-usage assignment runs per chunk and last-write-wins. For a
complete stream nothing changes (the final chunk arrives last and is complete). For
a stream truncated mid-flight, `usage` now holds the last partial figures where it
previously stayed nil. That is more accurate accounting, not less — but it is a
behaviour change, so pin it:

```go
func TestCompleteStreamTruncatedVLLMStreamKeepsLastPartialUsage(t *testing.T) {
	// Two content chunks with continuous usage, then the connection ends without a
	// final usage chunk: the returned error is the existing truncation error AND
	// the usage observed by the caller is the last partial one, not zero.
}
```

Also assert the complete-stream case is unchanged:

```go
func TestCompleteStreamTerminalUsageUnchangedWithContinuousUsage(t *testing.T) {
	// Intermediate chunks carry partial usage without prompt_tokens_details; the
	// final chunk carries the complete usage + timings. The terminal event's Usage
	// must equal the FINAL chunk's values, including CachedTokens.
}
```

- [ ] **Step 11: Run the suite and the gates**

Run: `go test ./... && golangci-lint fmt --diff && golangci-lint run` (from
`gateway/backend`)
Expected: all pass, `0 issues.`

- [ ] **Step 12: Verify the tests fail without the fix**

Temporarily revert Step 9's hoist (move the progress computation back inside
`if chunk.Usage != nil`), confirm
`TestCompleteStreamProgressFromChunkTimingsWithoutUsage` fails, then restore.

- [ ] **Step 13: Commit**

```bash
git add internal/provider/live_progress.go internal/provider/live_progress_test.go internal/inference/types.go internal/provider/openai_compatible.go internal/provider/openai_compatible_test.go
git commit -m "feat(provider): ask allowed upstreams for exact mid-stream token counts"
```

---

### Task 2: The in-flight counter

**Files:**
- Create: `gateway/backend/internal/gateway/request_progress.go`
- Create: `gateway/backend/internal/gateway/request_progress_test.go`
- Modify: `gateway/backend/internal/gateway/active_requests.go` (`ActiveRequest` at
  `:19-54`; `Snapshot`'s doc comment at `:155-169`)
- Modify: `gateway/backend/internal/gateway/stream_session.go` (the `Add` at `:111`;
  `stream()` at `:184-191`; the `streamSession` struct)
- Modify: `gateway/backend/internal/gateway/inference_complete.go` (the `Add` at
  `:52`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the `Add` at
  `:302`)

**Interfaces:**
- Consumes: `inference.StreamEvent.Progress` from Task 1.
- Produces: `ActiveRequest.Progress *requestProgress` with `observeDelta`, plus
  `liveProgressDTO(row ActiveRequest, now time.Time)` for Task 3.

- [ ] **Step 1: Write the failing counter test**

`request_progress_test.go`:

```go
func TestRequestProgressObserveDelta(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	p := &requestProgress{}

	// A first content delta with no upstream count still stamps TTFT.
	p.observeDelta(start.Add(300*time.Millisecond), nil)
	// A later delta carrying an upstream count and rate.
	p.observeDelta(start.Add(2*time.Second), &inference.StreamProgress{OutputTokens: 40, TokensPerSecond: 20})
	// The first-token stamp must NOT move.
	if got := p.firstTokenUnixNano.Load(); got != start.Add(300*time.Millisecond).UnixNano() {
		t.Fatalf("first-token stamp moved: %d", got)
	}
	if got := p.outputTokens.Load(); got != 40 {
		t.Fatalf("outputTokens = %d, want 40", got)
	}
	if got := p.upstreamTPSMilli.Load(); got != 20000 {
		t.Fatalf("upstreamTPSMilli = %d, want 20000", got)
	}
}

func TestRequestProgressNilReceiverIsSafe(t *testing.T) {
	var p *requestProgress
	p.observeDelta(time.Now(), &inference.StreamProgress{OutputTokens: 1})
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/gateway/ -run TestRequestProgress`
Expected: FAIL, `undefined: requestProgress`.

- [ ] **Step 3: Create the counter**

`request_progress.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync/atomic"
	"time"

	"op-ai-gateway/internal/inference"
)

// requestProgress is one in-flight request's live counters. Written by the single
// goroutine that owns the stream and read lock-free while the DTO is built, so it
// is deliberately NOT a field of ActiveRequest itself: activeRegistry stores
// ActiveRequest by VALUE and its mutex sits on the routing hot path
// (routing.Resolver calls ServerActivity several times per resolution, inside
// per-candidate loops), so a write lock per token would serialize routing behind
// the token rate -- and an atomic embedded in a copied struct would trip
// `copylocks` in every existing Snapshot/range. Reached by pointer, an
// ActiveRequest copy shares the one live counter, which is exactly what the DTO
// builder needs.
//
// Same shape as the repo's two existing hot-path counters: chatRunRegistry holds
// *ChatRun and mutates per delta through the run's OWN lock (chat_runs.go), and
// runtimeLogSub.dropped is an atomic on a per-subscriber object (runtime_logs.go).
type requestProgress struct {
	// outputTokens is the upstream's own cumulative count. 0 means the upstream
	// reported none -- never a gateway guess.
	outputTokens atomic.Int64
	// upstreamTPSMilli is the upstream's own rate x1000 (milli-tokens/second: one
	// decimal is displayed, so this is ample, and it avoids float bit-punning in
	// an atomic). 0 means the upstream reported no rate.
	upstreamTPSMilli atomic.Int64
	// firstTokenUnixNano stamps the first delta that carried content. 0 = none yet.
	firstTokenUnixNano atomic.Int64
}

// observeDelta records the first content delta's timestamp (once) and any exact,
// upstream-reported progress carried with it. Nil-safe: every non-streaming path
// and every test literal has no progress struct at all.
func (p *requestProgress) observeDelta(at time.Time, prog *inference.StreamProgress) {
	if p == nil {
		return
	}
	p.firstTokenUnixNano.CompareAndSwap(0, at.UnixNano())
	if prog == nil {
		return
	}
	if prog.OutputTokens > 0 {
		p.outputTokens.Store(int64(prog.OutputTokens))
	}
	if prog.TokensPerSecond > 0 {
		p.upstreamTPSMilli.Store(int64(prog.TokensPerSecond * 1000))
	}
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/gateway/ -run TestRequestProgress`
Expected: PASS.

- [ ] **Step 5: Hang it off the in-flight request**

In `active_requests.go`, add to `ActiveRequest` (after `StartedAt`):

```go
	// Progress carries this request's live counters. Non-nil only where there is
	// something to count (the streaming paths); nil on every non-streaming path and
	// in test literals, so every read must be nil-safe.
	Progress *requestProgress
```

Extend `Snapshot`'s doc comment — its copies are no longer frozen:

```go
// Snapshot returns a copy of the current in-flight requests. A nil registry
// returns nil. The returned slice is safe for the caller to mutate/sort.
//
// The copies are frozen EXCEPT for Progress, which is a pointer to live counters
// still being written by the request's own goroutine. That is intentional: the DTO
// builder runs outside the registry lock and needs current values.
```

- [ ] **Step 6: Allocate only where something writes**

Only the streaming translate path has anything to count, so only
`stream_session.go:111` gains `Progress: p` in its `ActiveRequest` literal, where
`p` is a `&requestProgress{}` allocated on the line above. The same pointer must
also reach the session, so add a `progress *requestProgress` field to the
`streamSession` struct and set it where the session is constructed.

`inference_complete.go:52` (non-streaming: one blocking round trip) and
`native_passthrough.go:302` (the client's body is forwarded and no frame is
decoded) are left alone — `Progress` stays nil there, which resolves to "not
measured", which is the correct answer for both. Allocating a struct nothing ever
writes would be dead weight, and nil is already the case Task 3 pins.

- [ ] **Step 7: Write the failing observation test**

```go
// The single hook in streamSession.stream must cover all three translate flavors.
func TestStreamSessionObservesProgressOnContentDeltas(t *testing.T) {
	// Drive a fake streamer emitting:
	//   1. StreamEventTextDelta{Text: ""} with Progress{OutputTokens: 1}  -> ignored
	//   2. StreamEventTextDelta{Text: "a", Progress: {OutputTokens: 5, TokensPerSecond: 10}}
	//   3. StreamEventTextDelta{Text: "b", Progress: {OutputTokens: 9, TokensPerSecond: 12}}
	// Assert: firstTokenUnixNano is stamped by (2), NOT (1);
	//         outputTokens == 9; upstreamTPSMilli == 12000.
}
```

- [ ] **Step 8: Run it and confirm it fails, then wire the hook**

In `stream_session.go`'s `stream()`:

```go
func (ss *streamSession) stream(handler func(inference.StreamEvent) error) error {
	return ss.streamer.CompleteStream(ss.ctx, ss.target, ss.providerReq, func(ev inference.StreamEvent) error {
		if ss.watchdog != nil {
			ss.watchdog.Reset(ss.idle)
		}
		// One hook for all three translate flavors: this wrapper is what
		// chat-completions, Responses and Anthropic translate all stream through.
		// Only a delta that actually carried content counts as "first token".
		if ev.Type == inference.StreamEventTextDelta && (ev.Text != "" || ev.Reasoning != "") {
			ss.progress.observeDelta(time.Now(), ev.Progress)
		}
		return handler(ev)
	})
}
```

- [ ] **Step 9: Run the tests and the gates**

Run: `go test ./... -race && golangci-lint fmt --diff && golangci-lint run`
Expected: all pass. The `-race` run must cover a test that reads the registry while
a stream writes; add one if none exists.

- [ ] **Step 10: Commit**

```bash
git add internal/gateway/request_progress.go internal/gateway/request_progress_test.go internal/gateway/active_requests.go internal/gateway/stream_session.go internal/gateway/inference_complete.go internal/gateway/native_passthrough.go
git commit -m "feat(gateway): per-request live counters on the in-flight registry"
```

---

### Task 3: Resolve the counters onto the wire

**Files:**
- Modify: `gateway/backend/internal/gateway/request_progress.go` (add the resolver)
- Modify: `gateway/backend/internal/gateway/active_requests.go` (`activeRequestDTO`
  at `:171-192`; the DTO fill loop at `:254-277`)
- Test: `gateway/backend/internal/gateway/request_progress_test.go`,
  `gateway/backend/internal/gateway/active_requests_test.go`

**Interfaces:**
- Consumes: `ActiveRequest.Progress` from Task 2.
- Produces: `output_tokens`, `tokens_per_second`, `tokens_per_second_source`,
  `ttft_ms` on `GET /api/portal/usage/active`. Tasks 5 and 6 consume them.

- [ ] **Step 1: Write the failing resolution test**

```go
func TestLiveProgressDTO(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	first := start.Add(500 * time.Millisecond)
	now := first.Add(2 * time.Second)

	upstream := &requestProgress{}
	upstream.firstTokenUnixNano.Store(first.UnixNano())
	upstream.outputTokens.Store(40)
	upstream.upstreamTPSMilli.Store(21500)

	gateway := &requestProgress{}
	gateway.firstTokenUnixNano.Store(first.UnixNano())
	gateway.outputTokens.Store(50) // 50 tokens over 2s -> 25.0

	// An exact count was never reported: no rate may be invented, however much
	// time has passed.
	noCount := &requestProgress{}
	noCount.firstTokenUnixNano.Store(first.UnixNano())

	cases := []struct {
		name       string
		p          *requestProgress
		wantTokens int
		wantTPS    float64
		wantSource string
		wantTTFT   int64
	}{
		{"upstream reported", upstream, 40, 21.5, "upstream", 500},
		{"gateway derived", gateway, 50, 25, "gateway", 500},
		{"no exact count", noCount, 0, 0, "", 500},
		{"no progress at all", nil, 0, 0, "", 0},
	}
	for _, tc := range cases {
		row := ActiveRequest{StartedAt: start, Progress: tc.p}
		tokens, tps, source, ttft := liveProgressDTO(row, now)
		if tokens != tc.wantTokens || tps != tc.wantTPS || source != tc.wantSource || ttft != tc.wantTTFT {
			t.Fatalf("%s: got (%d, %v, %q, %d)", tc.name, tokens, tps, source, ttft)
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/gateway/ -run TestLiveProgressDTO`
Expected: FAIL, `undefined: liveProgressDTO`.

- [ ] **Step 3: Implement the resolver**

Append to `request_progress.go`:

```go
// liveProgressDTO resolves one in-flight request's counters into its wire values.
// Nil-safe throughout: a request with no progress struct resolves to "not
// measured" -- an explicit "" source rather than a fabricated zero.
//
// The provenance rule is structural, not merely documented: a gateway-derived rate
// is only ever computed over an EXACT upstream count, so there is no code path in
// which a guessed number becomes a displayed rate.
func liveProgressDTO(row ActiveRequest, now time.Time) (outputTokens int, tps float64, source string, ttftMS int64) {
	p := row.Progress
	if p == nil {
		return 0, 0, "", 0
	}
	outputTokens = int(p.outputTokens.Load())
	first := p.firstTokenUnixNano.Load()
	if first == 0 {
		return outputTokens, 0, "", 0
	}
	firstAt := time.Unix(0, first)
	if ttftMS = firstAt.Sub(row.StartedAt).Milliseconds(); ttftMS < 0 {
		ttftMS = 0
	}
	if milli := p.upstreamTPSMilli.Load(); milli > 0 {
		return outputTokens, float64(milli) / 1000, "upstream", ttftMS
	}
	if secs := now.Sub(firstAt).Seconds(); outputTokens > 0 && secs > 0 {
		return outputTokens, float64(outputTokens) / secs, "gateway", ttftMS
	}
	return outputTokens, 0, "", ttftMS
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/gateway/ -run TestLiveProgressDTO`
Expected: PASS.

- [ ] **Step 5: Add the wire fields**

To `activeRequestDTO` (after `StartedAt`):

```go
	// Live per-request progress. output_tokens is the upstream's own cumulative
	// count (0 = none reported); tokens_per_second is 0 when not measured; and
	// tokens_per_second_source says how the rate was obtained -- "upstream"
	// (reported by the inference server), "gateway" (computed here from the
	// upstream's exact count), or "" (not measured). Plain string, deliberately
	// NOT omitempty, so "not measured" is explicit on the wire.
	OutputTokens          int     `json:"output_tokens"`
	TokensPerSecond       float64 `json:"tokens_per_second"`
	TokensPerSecondSource string  `json:"tokens_per_second_source"`
	// TTFTMs is the time from request start to the first content delta. 0 = not
	// measured.
	TTFTMs int64 `json:"ttft_ms"`
```

- [ ] **Step 6: Fill them**

In the DTO loop, before the loop take `now := time.Now()` (one clock reading for the
whole response so rows are mutually consistent), then per row:

```go
		outputTokens, tps, tpsSource, ttftMS := liveProgressDTO(row, now)
```

and add the four assignments to the literal.

- [ ] **Step 7: Add the endpoint test**

In `active_requests_test.go`, a handler-level test asserting the four keys are
present in the JSON for a row with an upstream-reported rate, and that a row with no
progress serialises `"tokens_per_second_source":""` (the key present, not omitted).

- [ ] **Step 8: Run the suite and the gates**

Run: `go test ./... && golangci-lint fmt --diff && golangci-lint run`

- [ ] **Step 9: Commit**

```bash
git add internal/gateway/request_progress.go internal/gateway/request_progress_test.go internal/gateway/active_requests.go internal/gateway/active_requests_test.go
git commit -m "feat(gateway): live tokens/sec and TTFT on the running-connections DTO"
```

---

### Task 4: Native passthrough — free usage accounting from the capture cap

**Files:**
- Create: `gateway/backend/internal/gateway/passthrough_usage_scan.go`
- Create: `gateway/backend/internal/gateway/passthrough_usage_scan_test.go`
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`writeChunk` at
  `:389-404`; the parse call at `:347`; `parsePassthroughUsage` at `:472-545`)
- Test: `gateway/backend/internal/gateway/native_passthrough_test.go`

**Interfaces:**
- Consumes: the existing `parsePassthroughUsage` and its monotonic `take` helper
  (`:479-483`), and `jsonPayloads` (`:589-607`).
- Produces: a `usageScanner` whose `usage()` is what `:347` uses instead of parsing
  the capped capture buffer, plus a first-content-frame timestamp for the Anthropic
  fallback rate.

- [ ] **Step 1: Write the failing over-cap test**

```go
// A response larger than the capture cap currently loses its trailing usage frame
// -- so even the FINAL token count is dropped today, independently of throughput.
func TestPassthroughUsageSurvivesBeyondCaptureCap(t *testing.T) {
	// Build an SSE body whose content frames exceed defaultCaptureMaxBytes, ending
	// with the terminal usage frame. Assert the recorded usage carries the real
	// output token count, not 0.
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/gateway/ -run TestPassthroughUsageSurvivesBeyondCaptureCap`
Expected: FAIL — usage is zero because the frame fell outside the cap.

- [ ] **Step 3: Build the scanner**

`passthrough_usage_scan.go`. Contract:

- `feed(chunk []byte, at time.Time)` appends to a carry buffer, splits on `\n`,
  hands every COMPLETE line to the existing parse, and keeps only the trailing
  partial line. The carry is bounded by the same `capBytes`; if a single line ever
  exceeds it the carry is dropped (a pathological stream must not grow memory).
- Re-parsing overlapping data is safe by construction: `parsePassthroughUsage`'s
  `take` is a max, so values only ever move upward.
- It also stamps `firstContentUnixNano` on the first frame that carries generated
  content, which is what gives the Anthropic fallback a generation window rather
  than a whole-request window.
- `usage()` returns the accumulated result for the recording path.

Its doc comment must state why it exists: the capture buffer has its own budget and
its own purpose, and token accounting must not ride on it.

- [ ] **Step 4: Use it**

In `writeChunk`, feed every chunk to the scanner **before** the capture-cap check,
so accounting no longer depends on the capture budget. At `:347`, take the usage
from the scanner instead of re-parsing `respBuf`.

- [ ] **Step 5: Run the test and confirm it passes**

Run: `go test ./internal/gateway/ -run TestPassthroughUsageSurvivesBeyondCaptureCap`
Expected: PASS.

- [ ] **Step 6: Read `timings` on the Responses shape**

Extend `parsePassthroughUsage` to read the `timings` object llama.cpp attaches to
the terminal `response.completed` frame, setting `PromptPerSecond` and
`TokensPerSecond` from `prompt_per_second` / `predicted_per_second`. Test:

```go
func TestParsePassthroughUsageReadsResponsesTimings(t *testing.T) {
	// A /v1/responses stream whose response.completed frame carries
	// "timings":{"prompt_per_second":120.5,"predicted_per_second":38.25}
	// must yield those rates -- today it yields 0, which is the bug.
}
```

- [ ] **Step 7: The Anthropic fallback rate**

The Anthropic shape carries no timings on any frame, so derive the rate from the
EXACT terminal output-token count over the generation window (first content frame →
completion), following `benchmark_runner.go:113-118`. Never over the whole request:
that would fold in queueing and prompt processing and would not be the same quantity
as every other path.

```go
func TestPassthroughAnthropicRateUsesGenerationWindow(t *testing.T) {
	// First content frame at t+1s, completion at t+3s, terminal output_tokens 40.
	// want 20.0 (40 over the 2s generation window), NOT 13.3 (40 over 3s).
}
```

- [ ] **Step 8: Run the suite and the gates**

Run: `go test ./... && golangci-lint fmt --diff && golangci-lint run`

- [ ] **Step 9: Commit**

```bash
git add internal/gateway/passthrough_usage_scan.go internal/gateway/passthrough_usage_scan_test.go internal/gateway/native_passthrough.go internal/gateway/native_passthrough_test.go
git commit -m "fix(gateway): passthrough token accounting no longer rides the capture cap"
```

---

### Task 5: Frontend — the type and the poll

**Files:**
- Modify: `gateway/frontend/src/api/usage.ts` (`ActiveRequest` at `:128-154`)
- Modify: `gateway/frontend/src/components/useActivityData.ts` (beside `loadActive`
  at `:150-162`; `queryRef` exists at `:213`)
- Test: `gateway/frontend/src/components/useActivityData.test.tsx` (or the existing
  test file for that hook)

**Interfaces:**
- Consumes: the four DTO fields from Task 3.
- Produces: an `active` array whose rows carry live values, refreshed every 2 s while
  rows exist. Task 6 renders them.

- [ ] **Step 1: Mirror the wire type**

In `src/api/usage.ts`, on `ActiveRequest` (after `started_at`):

```ts
  // Live per-request progress. `output_tokens` is the upstream's own cumulative
  // count (0 = none reported). `tokens_per_second` is 0 when not measured, and
  // `tokens_per_second_source` says how it was obtained: 'upstream' (reported by
  // the inference server), 'gateway' (computed here from the upstream's exact
  // count), or '' (not measured). `ttft_ms` is 0 when not measured.
  output_tokens: number;
  tokens_per_second: number;
  tokens_per_second_source: string;
  ttft_ms: number;
```

These are required, not optional — the backend always sends them. Fix the ripple in
any test fixture or factory that builds an `ActiveRequest` literal.

- [ ] **Step 2: Write the failing poll test**

```tsx
it('polls the active list while requests are running and stops when the list empties', async () => {
  // fake timers; render the hook with one active row
  // advance 2s -> api.activeRequests called again
  // set the list empty -> advance 10s -> no further calls
});
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `npm test -- useActivityData`
Expected: FAIL — no second call; there is no poll.

- [ ] **Step 4: Add the poll**

Near the other constants in `useActivityData.ts`:

```ts
// The running-connections list is otherwise refetched only on the payload-free
// start/end SSE pokes, so during ONE long stream the live tokens/s and TTFT cells
// would freeze for the whole request -- exactly the case they exist for. Polling
// only while something is running keeps an idle gateway at zero cost, and the
// start poke re-arms it. 2s rather than the repo's usual 3s because the elapsed
// column in the same row ticks every second, and a 3s data poll makes the
// throughput cell visibly lag its neighbour.
const ACTIVE_POLL_MS = 2000;
```

and the effect:

```ts
  useEffect(() => {
    if (active.length === 0) return;
    const id = window.setInterval(() => {
      const q = queryRef.current;
      void loadActive(q.scope ?? 'own', q.user_id, q.token_id);
    }, ACTIVE_POLL_MS);
    return () => window.clearInterval(id);
  }, [active.length, loadActive]);
```

`loadActive` is already best-effort and already holds the monotonic latest-wins
guard, so a slow poll can never overwrite a newer result.

- [ ] **Step 5: Run the test and confirm it passes**

Run: `npm test -- useActivityData`

- [ ] **Step 6: Run the frontend gates**

Run: `npm run format:check && npm run lint && npm run build && npm test`

- [ ] **Step 7: Commit**

```bash
git add src/api/usage.ts src/components/useActivityData.ts src/components/useActivityData.test.tsx
git commit -m "feat(portal): poll the running-connections list while requests run"
```

---

### Task 6: Frontend — the two columns and the em-dash convention

**Files:**
- Modify: `gateway/frontend/src/components/ActiveRequestsPanel.tsx` (the
  `activeColumns` catalogue; the elapsed column is last, at `:144-152`)
- Modify: `gateway/frontend/src/components/ActivityTable.tsx` (`:88-91`)
- Modify: `gateway/frontend/src/i18n.ts` (`de` near `:1729-1783`, `en` near
  `:3899-3953`)
- Test: `gateway/frontend/src/components/ActiveRequestsPanel.test.tsx`,
  `gateway/frontend/src/components/ActivityTable.test.tsx`

**Interfaces:**
- Consumes: the `ActiveRequest` fields from Task 5.

- [ ] **Step 1: Add the i18n keys**

In both locales, beside the existing `activityColTokenSpeed` /
`activityActiveElapsed`:

```
de: activityColLiveTokenSpeed: 'Tokens/s (live)'
    activityColTTFT: 'TTFT'
    activityLiveTpsUpstream: 'Vom Inferenzserver gemeldet'
    activityLiveTpsGateway: 'Vom Gateway berechnet aus {n} vom Server gemeldeten Tokens'
    activityLiveTpsNone: 'Nicht gemessen — dieser Upstream meldet mitten im Stream keine exakte Tokenzahl'

en: activityColLiveTokenSpeed: 'Tokens/s (live)'
    activityColTTFT: 'TTFT'
    activityLiveTpsUpstream: 'Reported by the inference server'
    activityLiveTpsGateway: 'Computed by the gateway from {n} tokens the server reported'
    activityLiveTpsNone: 'Not measured — this upstream reports no exact token count mid-stream'
```

- [ ] **Step 2: Write the failing column test**

```tsx
it('shows the em-dash for a request with no measurement and the rate with one decimal otherwise', () => {
  // rows: one with tokens_per_second 0 / source '' -> '—'
  //       one with tokens_per_second 21.5 / source 'upstream' -> '21.5'
  // and the TTFT cell: 0 -> '—', 480 -> '480 ms'
});

it('names the provenance in the tooltip, including the token count for a gateway-computed rate', () => {
  // source 'gateway', output_tokens 50 -> title contains '50'
});
```

- [ ] **Step 3: Run it and confirm it fails, then add the columns**

Import `formatMetric` from `./shared/format`, and add before the `elapsed` column:

```tsx
    {
      id: 'live_tps',
      label: t.activityColLiveTokenSpeed,
      // formatMetric renders the shared "never measured" em-dash for 0 and keeps
      // the cell sortable as a number; ListTable sinks a non-numeric cell in BOTH
      // sort directions, so '—' never ranks as zero.
      value: (a) => formatMetric(a.tokens_per_second, 1),
      searchable: false,
      numeric: true,
      render: (a) => <span title={liveTpsTitle(t, a)}>{formatMetric(a.tokens_per_second, 1)}</span>,
    },
    {
      id: 'ttft',
      label: t.activityColTTFT,
      value: (a) => (a.ttft_ms > 0 ? String(a.ttft_ms) : ''),
      searchable: false,
      numeric: true,
      render: (a) => (a.ttft_ms > 0 ? `${a.ttft_ms} ms` : '—'),
    },
```

with, above the component:

```tsx
// The provenance belongs in the tooltip, never baked into the number: an
// upstream-reported rate and a gateway-computed one are different measurements of
// the same thing and must not be silently mixed.
function liveTpsTitle(t: Translation, a: ActiveRequest): string {
  if (a.tokens_per_second_source === 'upstream') return t.activityLiveTpsUpstream;
  if (a.tokens_per_second_source === 'gateway') {
    return t.activityLiveTpsGateway.replace('{n}', String(a.output_tokens));
  }
  return t.activityLiveTpsNone;
}
```

- [ ] **Step 4: Align the completed table**

In `ActivityTable.tsx`, replace the two `toFixed(1)` cases with `formatMetric`, so
the same metric does not render two different ways on one screen (its sibling energy
and cost cells at `:110-112` already use the em-dash):

```tsx
    case 'prompt_per_second':
      return formatMetric(row.prompt_per_second, 1);
    case 'tokens_per_second':
      return formatMetric(row.tokens_per_second, 1);
```

with a test asserting a never-measured completed row renders `—`, not `0.0`.

- [ ] **Step 5: Run the frontend gates**

Run: `npm run format:check && npm run lint && npm run build && npm test`

- [ ] **Step 6: Verify the tests fail without the change**

Temporarily restore `toFixed(1)` and confirm the ActivityTable test fails; restore.

- [ ] **Step 7: Commit**

```bash
git add src/components/ActiveRequestsPanel.tsx src/components/ActivityTable.tsx src/i18n.ts src/components/ActiveRequestsPanel.test.tsx src/components/ActivityTable.test.tsx
git commit -m "feat(portal): live tokens/s and TTFT columns on the running-connections panel"
```

---

### Task 7: Documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (the
  running-connections view, §8.4.3)
- Modify: `docs/architecture/reference/api-surface.md` (`/api/portal/usage/active`,
  around `:89`)

- [ ] **Step 1: Document the provenance rule and the cadence**

In `telemetry-usage-observability.md`, beside the existing running-connections
description: what the live figure is, the rule that a rate appears only behind an
exact upstream count, the three `tokens_per_second_source` values, why the two
request parameters are gated by an allow-list (naming the LiteLLM 400), and why the
refresh is a 2 s poll rather than an SSE — including that the broker's payload-free
invariant is the reason values are not carried on the usage frame.

Refer to badges and states by key, never by colour.

- [ ] **Step 2: Document the wire fields**

In `api-surface.md`, add `output_tokens`, `tokens_per_second`,
`tokens_per_second_source` and `ttft_ms` to `/api/portal/usage/active`, each with its
"not measured" value.

- [ ] **Step 3: Verify**

Run: `./scripts/check-docs.sh && sh scripts/check-docs.test.sh`
Expected: `check-docs: OK`.

- [ ] **Step 4: Commit**

```bash
git add docs/architecture
git commit -m "docs: live per-request throughput on the running-connections view"
```

---

## Self-review notes

- **Spec coverage.** §1 (never estimate) → Task 1 Steps 9 and Task 3 Step 3;
  §2 (per-path sources) → Task 1; §3 (allow-list) → Task 1 Steps 1-4; §4.1-4.2
  (event field, provider) → Task 1; §4.3-4.4 (counter, writer) → Task 2; §4.5 (DTO)
  → Task 3; §5 (cadence) → Task 5; §6 (portal) → Task 6; §7 (passthrough) → Task 4;
  §8 (tests) distributed across every task; §9 (error handling) → Task 1 Step 10,
  Task 2 Step 3, Task 3 Step 3; §10 → Task 7.
- **Type consistency.** `requestProgress` field names are identical in Tasks 2 and 3;
  `liveProgressDTO`'s four return values match the four DTO fields, which match the
  four TypeScript fields, which match the two column renderers.
- **Known ripple.** Making the four TypeScript fields required will break any
  `ActiveRequest` literal in frontend tests; Task 5 Step 1 owns that fix. On the Go
  side seven test files build `ActiveRequest{}` literals with no `Progress` — they
  need no change because every read is nil-safe, which Task 3's `"no progress at
  all"` case pins.
