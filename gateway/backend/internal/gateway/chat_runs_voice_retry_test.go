// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A speech leg that fails BEFORE any audio reached the relay and for a transient reason is retried
// inside its held synthesis slot (#337): a briefly busy TTS must not cost the turn its voice. A
// failure that is the request's own fault, a permanent upstream condition, or one that came after
// audio was already relayed is never retried -- the first would only fail again, the last would
// duplicate audio and bill the partial synthesis twice.
//
// Every test below sets the resolved timing's ttsRetries EXPLICITLY. A hand-built voiceTiming reads
// ttsRetries as 0 (no retry), unlike a fully zero one, which resolves to the default of 2, so a
// fixture that relied on its implicit value would let the "not retried" cases pass vacuously.

// speechStep is one scripted answer of a speechAttemptScript.
type speechStep struct {
	// status, when non-zero, is the non-2xx status the TTS answers with, carrying body.
	status int
	body   string
	// cutStream answers 200 with an event stream that delivers ONE audio delta and then ends without
	// its terminal events: a failure AFTER audio was already relayed (mid-stream).
	cutStream bool
	// hold blocks the answer until the request ends; delay waits that long before answering.
	hold  bool
	delay time.Duration
}

// response builds the provider's answer for the step. A step that is none of the above is the
// normal streamed answer, which the caller does not script (a request past the script gets it).
func (st speechStep) response(ctx context.Context) (*provider.ProxyResponse, error) {
	if st.hold {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if st.delay > 0 {
		select {
		case <-time.After(st.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if st.cutStream {
		first, _ := voiceTestPCM()
		return &provider.ProxyResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(speechFrameDelta(first))),
		}, nil
	}
	return &provider.ProxyResponse{
		StatusCode: st.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(st.body)),
	}, nil
}

// speechAttemptScript hands out the scripted steps in request order and counts the requests.
type speechAttemptScript struct {
	mu    sync.Mutex
	steps []speechStep
	calls int
}

// next is the step the next speech request takes, and whether the script still has one. It is safe
// on a nil script (no programming), which reports no step.
func (s *speechAttemptScript) next() (speechStep, bool) {
	if s == nil {
		return speechStep{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.calls
	s.calls++
	if n >= len(s.steps) {
		return speechStep{}, false
	}
	return s.steps[n], true
}

// attempts is how many speech requests reached the TTS so far.
func (s *speechAttemptScript) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// oneRegionScript is an LLM answer with a single <speak> region: one section, one speech leg.
var oneRegionScript = []scriptedLLMDelta{{text: `<speak lang="de">Eins.</speak> Rest.`}}

// Bodies of the failures the TTS can answer with, in the shapes audio.cpp and the gateway use.
const (
	busyBody              = `{"error":{"code":"routing.no_healthy_host","message":"no healthy host"}}`
	transient500Body      = `{"error":{"message":"synthesis backend crashed, try again","type":"server_error"}}`
	permanentVoice500Body = `{"error":{"message":"Voice 'nobody' not found","type":"server_error"}}`
	permanentRef500Body   = `{"error":{"message":"failed to load reference audio","type":"server_error"}}`
	badRequestBody        = `{"error":{"code":"speech.input_required","message":"input is required"}}`
)

// retryRun is the outcome of one scripted voice run.
type retryRun struct {
	run      *ChatRun
	srv      *Server
	script   *speechAttemptScript
	audio    []runAudio
	status   string
	errCode  string
	bodies   <-chan []byte
	recorder *usage.Recorder
	rec      *recordingVoicePortal
}

// runRetryScenario runs a one-section voice run whose TTS answers the given steps in request order
// (then normally), under a resolved timing with exactly `retries` speech retries, to its end.
func runRetryScenario(t *testing.T, steps []speechStep, retries int) retryRun {
	t.Helper()
	prov := defaultVoiceProvider()
	prov.llmScript = oneRegionScript
	prov.speechBodies = make(chan []byte, 16)
	prov.speechAttempts = &speechAttemptScript{steps: steps}
	srv, owner, chatID, recorder, bodies := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsRetries: retries}
	rec := &recordingVoicePortal{API: srv.Portal}
	srv.Portal = rec

	run, ch := startSubscribedVoiceRun(t, srv, owner, chatID, "unused")
	audio := drainRunAudio(t, ch)
	waitFor(t, func() bool { return run.statusValue() != "running" })
	snap, _, unsub := run.subscribe()
	unsub()
	return retryRun{
		run: run, srv: srv, script: prov.speechAttempts, audio: audio,
		status: snap.Status, errCode: snap.Err, bodies: bodies, recorder: recorder, rec: rec,
	}
}

// A transient failure with no audio yet is retried and the retry's audio is relayed exactly once:
// one monotonic stream, no gap in the seq and no duplicate of anything the failed attempt did. The
// turn completes and commits the section's audio as it would have without the failure.
func TestVoiceRun_RetryTransientBusyThenSucceeds(t *testing.T) {
	r := runRetryScenario(t, []speechStep{{status: http.StatusServiceUnavailable, body: busyBody}}, 2)

	if got := r.script.attempts(); got != 2 {
		t.Fatalf("%d speech requests reached the TTS, want 2 (the busy one and its retry)", got)
	}
	if r.status != "completed" {
		t.Fatalf("status = %q (err %q), want completed: the retry should have saved the turn", r.status, r.errCode)
	}
	// The normal answer is two chunks; relayed ONCE, in one monotonic stream, all section 0.
	assertOneMonotonicStream(t, r.audio, []int{0, 0})
	turn, ok := r.rec.committed()
	if !ok {
		t.Fatal("no voice commit was captured")
	}
	if parts := contentPartSummary(t, turn.ContentParts); len(parts) != 2 || !strings.HasPrefix(parts[1], "audio:") {
		t.Fatalf("content parts = %v, want [text, one section audio]", parts)
	}
	// Billing: the busy attempt synthesized nothing and bills nothing; only the retry's audio does.
	billed := 0
	for _, ev := range r.recorder.All() {
		if (ev.BillingUnit == usage.BillingUnitAudioSecond || ev.BillingUnit == usage.BillingUnitAudioBytes) && ev.BillingQuantity > 0 {
			billed++
		}
	}
	if billed != 1 {
		t.Fatalf("%d audio rows carry a billed quantity, want 1 (the retry's; the failed attempt bills nothing): %+v", billed, r.recorder.All())
	}
}

// The classification of an upstream failure decides whether the leg retries: a 503 (busy), a 429
// (admission) and a 5xx whose message is not a recognized permanent one are transient; a 4xx and a
// 5xx that says the voice or its reference is missing are not. The retry LIMIT bounds the transient
// ones, and a limit of 0 turns the retry off. Each row sets the retry limit explicitly.
func TestVoiceRun_RetryClassification(t *testing.T) {
	busy := speechStep{status: http.StatusServiceUnavailable, body: busyBody}
	cases := []struct {
		name         string
		steps        []speechStep
		retries      int
		wantAttempts int
		wantStatus   string
	}{
		{"503 busy is retried", []speechStep{busy}, 2, 2, "completed"},
		{"429 admission is retried", []speechStep{{status: http.StatusTooManyRequests, body: busyBody}}, 2, 2, "completed"},
		{"a 500 with an unrecognized message is transient", []speechStep{{status: http.StatusInternalServerError, body: transient500Body}}, 2, 2, "completed"},
		{"a 502 is transient", []speechStep{{status: http.StatusBadGateway, body: `{"error":{"code":"provider.unavailable","message":"upstream unavailable"}}`}}, 2, 2, "completed"},
		{"a 500 for a missing voice is permanent", []speechStep{{status: http.StatusInternalServerError, body: permanentVoice500Body}}, 2, 1, "error"},
		{"a 500 for a missing reference is permanent", []speechStep{{status: http.StatusInternalServerError, body: permanentRef500Body}}, 2, 1, "error"},
		{"a 400 is the request's own fault", []speechStep{{status: http.StatusBadRequest, body: badRequestBody}}, 2, 1, "error"},
		{"a 404 is not retried", []speechStep{{status: http.StatusNotFound, body: `{"error":{"code":"routing.model_not_capable","message":"not capable"}}`}}, 2, 1, "error"},
		{"a 422 is not retried", []speechStep{{status: http.StatusUnprocessableEntity, body: badRequestBody}}, 2, 1, "error"},
		{"the retry limit bounds a persistent failure", []speechStep{busy, busy, busy, busy}, 2, 3, "error"},
		{"a limit of 1 retries once", []speechStep{busy, busy, busy}, 1, 2, "error"},
		{"a limit of 0 disables the retry", []speechStep{busy}, 0, 1, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRetryScenario(t, tc.steps, tc.retries)
			if got := r.script.attempts(); got != tc.wantAttempts {
				t.Fatalf("%d speech requests reached the TTS, want %d", got, tc.wantAttempts)
			}
			if r.status != tc.wantStatus {
				t.Fatalf("status = %q (err %q), want %q", r.status, r.errCode, tc.wantStatus)
			}
		})
	}
}

// A failure AFTER a chunk was already relayed is never retried: the relayed audio stays (existing
// behavior -- the client already played it), a retry would repeat it, and the partial synthesis was
// already billed. The leg fails with its own code and the run degrades on it.
func TestVoiceRun_RetryNeverAfterAudioWasRelayed(t *testing.T) {
	r := runRetryScenario(t, []speechStep{{cutStream: true}}, 2)

	if got := r.script.attempts(); got != 1 {
		t.Fatalf("%d speech requests reached the TTS, want 1: a mid-stream failure must not be retried", got)
	}
	if r.status != "error" || r.errCode != speechRunResponseUnreadableMessage {
		t.Fatalf("status %q err %q, want the broken stream (%q) surfaced as the degrade", r.status, r.errCode, speechRunResponseUnreadableMessage)
	}
	// The one chunk that was relayed is still there, exactly once.
	assertOneMonotonicStream(t, r.audio, []int{0})
}

// A leg that fails and retries holds its synthesis slot throughout: it does not give the slot back
// and requeue, which would let the NEXT section take it first and break document order at the TTS.
// With one slot and two sections, the TTS sees section 0 twice (the failure and its retry) before
// it ever sees section 1.
func TestVoiceRun_RetryStaysInsideTheHeldSlot(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = twoRegionScript
	prov.speechBodies = make(chan []byte, 16)
	prov.speechAttempts = &speechAttemptScript{steps: []speechStep{{status: http.StatusServiceUnavailable, body: busyBody}}}
	srv, owner, chatID, _, bodies := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsConcurrency: 1, ttsRetries: 2}

	run, ch := startSubscribedVoiceRun(t, srv, owner, chatID, "unused")
	audio := drainRunAudio(t, ch)
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}

	var order []string
	for {
		select {
		case b := <-bodies:
			order = append(order, speechInputOf(b))
			continue
		default:
		}
		break
	}
	want := []string{twoRegionSec0, twoRegionSec0, twoRegionSec1}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("the TTS saw %v, want %v: the retry must finish inside section 0's slot before section 1 starts", order, want)
	}
	assertOneMonotonicStream(t, audio, []int{0, 0, 1, 1})
}

// Each attempt gets its OWN first-chunk deadline. The deadline is armed when the leg takes its slot;
// a retry that inherited the first attempt's already-running clock would be cut off by time the
// failed attempt and the backoff spent. Here the first attempt takes most of the deadline to fail,
// and the retry -- which only just fits if the clock were shared -- must still get a full budget.
func TestVoiceRun_RetryGetsAFreshFirstChunkDeadline(t *testing.T) {
	restore := speechRunDeadline
	speechRunDeadline = 600 * time.Millisecond
	t.Cleanup(func() { speechRunDeadline = restore })

	// The failed attempt answers after 450ms; with the 250ms backoff, a shared clock would have
	// expired at 600ms, before the retry could even be dispatched.
	steps := []speechStep{{status: http.StatusServiceUnavailable, body: busyBody, delay: 450 * time.Millisecond}}
	r := runRetryScenario(t, steps, 2)

	if got := r.script.attempts(); got != 2 {
		t.Fatalf("%d speech requests reached the TTS, want 2", got)
	}
	if r.status != "completed" {
		t.Fatalf("status = %q (err %q), want completed: the retry was cut off by the first attempt's deadline", r.status, r.errCode)
	}
}

// A cancel during the retry backoff stops the leg at once: no further attempt reaches the TTS, and
// the run ends canceled rather than waiting out the backoff or the retries.
func TestVoiceRun_RetryBackoffEndsOnCancel(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = oneRegionScript
	prov.speechBodies = make(chan []byte, 16)
	// The first attempt fails; a retry (which must not happen) would hold, so it could not finish on
	// its own either.
	script := &speechAttemptScript{steps: []speechStep{
		{status: http.StatusServiceUnavailable, body: busyBody},
		{hold: true},
	}}
	prov.speechAttempts = script
	srv, owner, chatID, _, _ := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsRetries: 2}
	rec := &recordingVoicePortal{API: srv.Portal}
	srv.Portal = rec

	run, err := srv.startVoiceRun(context.Background(), voiceRunRequest{owner: owner, chatID: chatID, pendingText: "unused", asr: voiceASRTarget, llm: voiceLLMTarget, tts: voiceTTSTarget})
	if err != nil {
		t.Fatalf("startVoiceRun: %v", err)
	}
	if !voicePoll(10*time.Second, func() bool { return script.attempts() >= 1 }) {
		t.Fatal("the first speech attempt never reached the TTS")
	}
	// The failed attempt answers at once and the leg then waits out its first backoff; cancel in it.
	time.Sleep(50 * time.Millisecond)
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	if got := script.attempts(); got != 1 {
		t.Fatalf("%d speech requests reached the TTS, want 1: a cancel during the backoff must stop the retry", got)
	}
	if _, ok := rec.committed(); ok {
		t.Fatal("a cancelled run committed a turn")
	}
}

// speechFailureTransient is the classifier behind the retry, pinned directly so the status/message
// rules read as a table. The status decides first: a 503 and a 429 are transient whatever they say
// (routing.voice_unavailable is a 503 that names "voice" and is exactly the busy case), and a 4xx
// never is. Only for another 5xx does the upstream's message decide, because audio.cpp answers 500
// both for a transient hiccup and for a bad voice or reference.
func TestSpeechFailureTransient(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"503 busy", 503, busyBody, true},
		{"503 voice_unavailable names the voice and is still transient", 503, `{"error":{"code":"routing.voice_unavailable","message":"no usable server offers voice anna"}}`, true},
		{"429 admission", 429, `{"error":{"code":"routing.admission_queue_full","message":"queue full"}}`, true},
		{"500 unknown message", 500, transient500Body, true},
		{"500 plain-string error wrapped by the relay", 500, `{"error":{"code":"speech.upstream_error","message":"the speech upstream failed","type":"upstream_error"}}`, true},
		{"500 with an empty body", 500, ``, true},
		{"500 with a non-JSON body", 500, `upstream connect error`, true},
		{"502", 502, `{"error":{"message":"bad gateway"}}`, true},
		{"504", 504, ``, true},
		{"500 missing voice", 500, permanentVoice500Body, false},
		{"500 missing voice, upper case", 500, `{"error":{"message":"VOICE NOT FOUND"}}`, false},
		{"500 reference audio", 500, permanentRef500Body, false},
		{"500 speaker", 500, `{"error":{"message":"unknown speaker id 7"}}`, false},
		{"500 not found", 500, `{"error":{"message":"model file not found"}}`, false},
		{"500 no such", 500, `{"error":{"message":"no such preset"}}`, false},
		{"500 does not exist", 500, `{"error":{"message":"preset does not exist"}}`, false},
		{"500 permanent message in a non-JSON body", 500, `voice not found`, false},
		{"400", 400, badRequestBody, false},
		{"401", 401, `{"error":{"message":"unauthorized"}}`, false},
		{"404", 404, `{"error":{"message":"not found"}}`, false},
		{"408 is a 4xx", 408, ``, false},
		{"422", 422, badRequestBody, false},
		{"200 is not a failure", 200, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := speechFailureTransient(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("speechFailureTransient(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// The backoff grows from its base, doubles, and is capped so a generous retry count can never hang a
// turn on one section.
func TestVoiceTTSRetryBackoffSchedule(t *testing.T) {
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for retry, w := range want {
		if got := voiceTTSRetryBackoff(retry); got != w {
			t.Fatalf("voiceTTSRetryBackoff(%d) = %v, want %v", retry, got, w)
		}
	}
	if got := voiceTTSRetryBackoff(1000); got != voiceTTSRetryBackoffMax {
		t.Fatalf("voiceTTSRetryBackoff(1000) = %v, want the cap %v (and no overflow)", got, voiceTTSRetryBackoffMax)
	}
}

// The "audio already went out" guard is the retry's invariant and holds independent of the
// classification: an attempt that relayed a chunk and then reports a transient failure is NOT
// retried, however many retries remain. (No real attempt is both today, so the loop is driven
// directly.)
func TestRetrySpeechAttemptsNeverRetriesOnceAudioWasRelayed(t *testing.T) {
	var relayed atomic.Bool
	calls := 0
	out := retrySpeechAttempts(context.Background(), "test", 5, &relayed, func() speechAttemptResult {
		calls++
		relayed.Store(true) // the attempt's sink emitted a chunk before it failed
		return speechAttemptResult{status: "error", errMsg: "boom", transient: true}
	})
	if calls != 1 {
		t.Fatalf("attempt ran %d times, want 1: a failure after relayed audio must not be retried", calls)
	}
	if out.status != "error" || out.errMsg != "boom" {
		t.Fatalf("result = %+v, want the attempt's own failure", out)
	}
}

// Across attempts, not within one: audio relayed by an EARLIER attempt bars every later retry, and a
// transient failure with nothing relayed is retried up to the limit and no further.
func TestRetrySpeechAttemptsLimitsAndGuards(t *testing.T) {
	var relayed atomic.Bool
	calls := 0
	out := retrySpeechAttempts(context.Background(), "test", 2, &relayed, func() speechAttemptResult {
		calls++
		return speechAttemptResult{status: "error", errMsg: "busy", transient: true}
	})
	if calls != 3 || out.errMsg != "busy" {
		t.Fatalf("ran %d attempts (result %+v), want 3: the first plus two retries, then give up with the last failure", calls, out)
	}

	calls = 0
	out = retrySpeechAttempts(context.Background(), "test", 2, &relayed, func() speechAttemptResult {
		calls++
		if calls == 2 {
			return speechAttemptResult{status: "completed"}
		}
		return speechAttemptResult{status: "error", errMsg: "busy", transient: true}
	})
	if calls != 2 || out.status != "completed" {
		t.Fatalf("ran %d attempts (result %+v), want 2 and the retry's success", calls, out)
	}

	// Cancelled while waiting out the backoff: no further attempt, and the run's own outcome.
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	out = retrySpeechAttempts(ctx, "test", 2, &relayed, func() speechAttemptResult {
		calls++
		cancel() // the run is cancelled right after the failed attempt, i.e. during the backoff
		return speechAttemptResult{status: "error", errMsg: "busy", transient: true}
	})
	if calls != 1 || out.status != "canceled" {
		t.Fatalf("ran %d attempts (result %+v), want 1 and a canceled outcome", calls, out)
	}
}
