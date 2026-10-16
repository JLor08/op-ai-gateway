// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// Every existing recordUsage path must write a token-metered row: usageMeta is
// built as a KEYED literal at all ten call sites, so the pair is the Go zero
// value unless a producer sets it. This is what makes the whole change a no-op
// for LLM traffic, independently of migration v81's column defaults.
func TestRecordUsageWritesTokenMeteredByDefault(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-100*time.Millisecond),
		auth.Token{ID: "tok_user", UserID: "usr_billing"},
		inference.Request{Model: "qwen-coder"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}},
		"", "success",
		usageMeta{ReqPath: "/v1/chat/completions", HTTPStatus: 200, ContentType: "application/json"},
		"req_billing_default", nil, nil,
	)

	events := srv.Usage.ByUser("usr_billing")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitTokens {
		t.Errorf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitTokens)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}

// A usageMeta carrying a unit reaches the row unchanged -- the seam #71/#68/#69
// will use.
func TestRecordUsageCarriesBillingPairFromUsageMeta(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-2*time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_img"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{},
		"", "success",
		usageMeta{
			ReqPath:         "/v1/images/generations",
			HTTPStatus:      200,
			ContentType:     "application/json",
			BillingUnit:     usage.BillingUnitImage,
			BillingQuantity: 2,
		},
		"req_billing_image", nil, nil,
	)

	events := srv.Usage.ByUser("usr_billing_img")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitImage || got.BillingQuantity != 2 {
		t.Errorf("got (%q, %v), want (%q, 2)", got.BillingUnit, got.BillingQuantity, usage.BillingUnitImage)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}

// A violating row is still recorded -- unmodified -- AND the violation is logged
// at Error, because dropping the row would lose a request from billing and
// repairing it would destroy the evidence, while staying silent would leave a
// wrong producer undetectable.
//
// The log assertion is what gives this test teeth. The other two tests here call
// usage.ValidateBillingXOR on the recorded row themselves, so they pass whether
// or not recordUsage validates anything; "recorded unmodified" is likewise
// satisfied by a recordUsage that never validates at all. Deleting recordUsage's
// ValidateBillingXOR block -- the single enforcement point ADR-041 and
// telemetry-usage-observability.md §8.4.1 both promise -- fails only this
// assertion.
func TestRecordUsageRecordsAnXORViolationUnmodified(t *testing.T) {
	srv := NewTestServer()
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)

	srv.recordUsage(
		time.Now().Add(-time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_bad"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{OutputTokens: 7, TotalTokens: 7}},
		"", "success",
		usageMeta{ReqPath: "/v1/images/generations", HTTPStatus: 200, BillingUnit: usage.BillingUnitImage, BillingQuantity: 1},
		"req_billing_violation", nil, nil,
	)

	events := srv.Usage.ByUser("usr_billing_bad")
	if len(events) != 1 {
		t.Fatalf("want the violating row recorded, got %d events", len(events))
	}
	if events[0].OutputTokens != 7 || events[0].BillingUnit != usage.BillingUnitImage {
		t.Errorf("the row must be recorded unmodified, got %+v", events[0])
	}

	// The violation itself must be observable to an operator, at Error. The
	// message is matched by substring and the request id by attr, so a violation
	// is traceable back to the one request that caused it.
	recs := buf.Snapshot()
	var logged *logbuffer.Record
	for i := range recs {
		if recs[i].Level == "ERROR" && strings.Contains(recs[i].Msg, "usage billing contract violated") {
			logged = &recs[i]
			break
		}
	}
	if logged == nil {
		t.Fatalf("want an ERROR log for the XOR violation, got %d records: %+v", len(recs), recs)
	}
	if logged.Attrs["id"] != "req_billing_violation" {
		t.Errorf("id attr = %v, want req_billing_violation", logged.Attrs["id"])
	}
	if logged.Attrs["req_path"] != "/v1/images/generations" {
		t.Errorf("req_path attr = %v, want /v1/images/generations", logged.Attrs["req_path"])
	}
}

// billingUnitFor is the one chokepoint every native-passthrough recordUsage site
// reads the endpoint's unit from, so each flavor must answer with its own unit
// and every other flavor must stay token-metered: a speech request billed as
// "image" (or a chat request billed as "audio_second") is the lie the
// (unit, quantity) pair exists to prevent.
func TestBillingUnitForNamesEachNonTokenFlavorsUnit(t *testing.T) {
	cases := []struct {
		flavor string
		want   string
	}{
		{apiFlavorImages, usage.BillingUnitImage},
		{apiFlavorSpeech, usage.BillingUnitAudioSecond},
		{"openai_responses", usage.BillingUnitTokens},
		{"anthropic_messages", usage.BillingUnitTokens},
		{"openai", usage.BillingUnitTokens},
		{"", usage.BillingUnitTokens},
	}
	for _, tc := range cases {
		if got := billingUnitFor(tc.flavor); got != tc.want {
			t.Errorf("billingUnitFor(%q) = %q, want %q", tc.flavor, got, tc.want)
		}
	}
}

// speechBillingUpstream starts an audio.cpp stub that answers every request with
// status and a WAV body carrying exactly the given headers, so a test controls
// whether X-AudioCPP-Audio-Duration-Ms is present and what it says.
func speechBillingUpstream(t *testing.T, status int, headers map[string]string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "audio/wav")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(speechTestWAV())
	}))
	t.Cleanup(up.Close)
	return up
}

// requireSpeechNonTokenRow asserts the shape every speech row has whatever its
// outcome: the audio_second unit, ZERO in all five token columns and both
// per-second rates, and a row ValidateBillingXOR accepts. The explicit zero
// checks are not redundant with the XOR call: they name the column that leaked.
func requireSpeechNonTokenRow(t *testing.T, ev usage.Event) {
	t.Helper()
	if ev.BillingUnit != usage.BillingUnitAudioSecond {
		t.Errorf("BillingUnit = %q, want %q", ev.BillingUnit, usage.BillingUnitAudioSecond)
	}
	for name, v := range map[string]float64{
		"input_tokens":       float64(ev.InputTokens),
		"output_tokens":      float64(ev.OutputTokens),
		"total_tokens":       float64(ev.TotalTokens),
		"cached_tokens":      float64(ev.CachedTokens),
		"cache_write_tokens": float64(ev.CacheWriteTokens),
		"prompt_per_second":  ev.PromptPerSecond,
		"tokens_per_second":  ev.TokensPerSecond,
	} {
		if v != 0 {
			t.Errorf("%s = %v on a speech row, want 0", name, v)
		}
	}
	if err := usage.ValidateBillingXOR(ev); err != nil {
		t.Errorf("recorded speech row violates the XOR: %v", err)
	}
}

// errorLogs returns the ERROR-level records of buf.
func errorLogs(buf *logbuffer.Buffer) []logbuffer.Record {
	var out []logbuffer.Record
	for _, rec := range buf.Snapshot() {
		if rec.Level == "ERROR" {
			out = append(out, rec)
		}
	}
	return out
}

// A successful speech relay bills the audio it produced, in seconds, read off
// the upstream's duration header (milliseconds) -- and nothing else: no token
// count and no tokens/second leaks onto the row, so ValidateBillingXOR holds and
// recordUsage logs nothing at Error.
func TestSpeechUsageBillsAudioSecondsFromTheDurationHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   float64
	}{
		{"a whole number of seconds, one decimal", "2000.0", 2},
		{"audio.cpp's own six-decimal shape", "2000.000000", 2},
		{"an integral value", "1500", 1.5},
		{"a sub-second fraction", "250.5", 0.2505},
		{"a long synthesis", "93456.789", 93.456789},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up := speechBillingUpstream(t, http.StatusOK, map[string]string{"X-AudioCPP-Audio-Duration-Ms": tc.header})
			srv, _ := newSpeechCapableTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
			}
			got := lastUsageEvent(t, srv)
			if got.Status != "success" {
				t.Fatalf("Status = %q, want success", got.Status)
			}
			if math.Abs(got.BillingQuantity-tc.want) > 1e-9 {
				t.Errorf("BillingQuantity = %v, want %v (header %q is milliseconds)", got.BillingQuantity, tc.want, tc.header)
			}
			requireSpeechNonTokenRow(t, got)
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Errorf("a well-formed speech relay logged at Error: %+v", errs)
			}
		})
	}
}

// A successful relay whose upstream gave no usable duration cannot say how much
// audio it produced. The (unit, quantity) pair has no "unknown", so the row
// records 0 -- and the gap is logged ONCE at Error, as the images relay does for
// a counted zero, because a silent zero would be indistinguishable from a
// genuine empty clip. Zero, negative, non-numeric and non-finite values are all
// unusable: ParseFloat accepts "NaN" and "Inf", and a quantity that is either
// would poison every sum the usage views take.
func TestSpeechUsageWithoutAUsableDurationRecordsZeroAndLogsOnce(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"the header is absent", nil},
		{"the header is empty", map[string]string{"X-AudioCPP-Audio-Duration-Ms": ""}},
		{"the duration is zero", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "0"}},
		{"the duration is a six-decimal zero", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "0.000000"}},
		{"the duration is negative", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "-5"}},
		{"the duration is not a number", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "abc"}},
		{"the duration carries a unit suffix", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "1500ms"}},
		{"the duration is NaN", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "NaN"}},
		{"the duration is infinite", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "Inf"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up := speechBillingUpstream(t, http.StatusOK, tc.headers)
			srv, _ := newSpeechCapableTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the audio still reaches the client); body = %q", rec.Code, rec.Body.Bytes())
			}
			got := lastUsageEvent(t, srv)
			if got.Status != "success" {
				t.Fatalf("Status = %q, want success", got.Status)
			}
			if got.BillingQuantity != 0 {
				t.Errorf("BillingQuantity = %v, want 0 (no usable duration)", got.BillingQuantity)
			}
			requireSpeechNonTokenRow(t, got)
			errs := errorLogs(buf)
			if len(errs) != 1 || !strings.Contains(errs[0].Msg, "no usable audio duration") {
				t.Fatalf("Error logs = %+v, want exactly one saying the relay reported no usable audio duration", errs)
			}
			if errs[0].Attrs["path"] != "/v1/audio/speech" {
				t.Errorf("path attr = %v, want /v1/audio/speech", errs[0].Attrs["path"])
			}
		})
	}
}

// A speech relay that did not finish cleanly bills nothing, even when the
// upstream sent a duration header: the quantity is only trusted once the whole
// body reached the client (status == "success"). The row still carries the unit
// -- it is the endpoint's identity, not a measure -- and, being an ordinary
// failure, logs nothing at Error.
func TestSpeechUsageOfAnUpstreamErrorBillsZeroSeconds(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up := speechBillingUpstream(t, http.StatusInternalServerError, map[string]string{"X-AudioCPP-Audio-Duration-Ms": "2000.0"})
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("row = %q/%d, want error/500", got.Status, got.HTTPStatus)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0 for a failed relay", got.BillingQuantity)
	}
	requireSpeechNonTokenRow(t, got)
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Errorf("an upstream error must not log the no-duration line: %+v", errs)
	}
}

// Every speech row that never reached a finished relay still names its unit: a
// refusal produced no audio, but "audio_second, 0" is the truthful statement
// where the default "" would claim a token-metered request with a zero measure.
// The two routing refusals go through recordSpeechRoutingFailure; the other two
// rows are proxyNative's shared recordUsage sites (the provider cannot proxy,
// the upstream call itself fails), which read the unit through billingUnitFor.
func TestSpeechRowsThatNeverFinishedARelayStillCarryTheUnit(t *testing.T) {
	t.Run("a model without a speech verdict", func(t *testing.T) {
		up := speechBillingUpstream(t, http.StatusOK, nil)
		srv, _ := newSpeechCapableTestServer(t, up.URL)

		postSpeech(t, srv, `{"model":"asr-model","input":"hi"}`)

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "routing.model_not_capable" {
			t.Fatalf("ErrorCode = %q, want routing.model_not_capable", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
		if got.BillingQuantity != 0 {
			t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
		}
	})
	t.Run("a resolved target whose effective flavors exclude speech", func(t *testing.T) {
		srv := newServerAgentSpeechSpecTestServer(t, &recordingProxyProvider{respBody: "RIFF"}, []string{routing.APIFlavorOpenAI})

		postSpeech(t, srv, `{"model":"agent-voice","input":"hi"}`)

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "routing.no_model_route" {
			t.Fatalf("ErrorCode = %q, want routing.no_model_route", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
	})
	t.Run("a provider that cannot proxy", func(t *testing.T) {
		srv := newServerAgentSpeechSpecTestServer(t, nonProxyCapableProvider{}, []string{routing.APIFlavorOpenAIAudio})

		postSpeech(t, srv, `{"model":"agent-voice","input":"hi"}`)

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "provider.unavailable" {
			t.Fatalf("ErrorCode = %q, want provider.unavailable", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
	})
	t.Run("an upstream call that fails before any response", func(t *testing.T) {
		srv := newServerAgentSpeechSpecTestServer(t, erroringProxyProvider{err: errors.New("dial tcp: connection refused")}, []string{routing.APIFlavorOpenAIAudio})

		postSpeech(t, srv, `{"model":"agent-voice","input":"hi"}`)

		got := lastUsageEvent(t, srv)
		if got.Status != "error" {
			t.Fatalf("Status = %q, want error", got.Status)
		}
		requireSpeechNonTokenRow(t, got)
	})
}

// --- Transcription: the quantity comes from the RESPONSE BODY ---------------

// transcriptionDoc is the document audio.cpp answers a transcription with, its
// timing block trailing the text as the real server writes it.
func transcriptionDoc(durationMs string) string {
	return `{"text":"hello there","timing":{"wall_ms":1200,"audio_duration_ms":` + durationMs + `,"rtf":0.2}}`
}

// The timing block's audio_duration_ms (milliseconds) is the billable quantity of a
// transcription. The body reaches the client byte for byte, timing block included
// (the gateway proxies, it does not translate), and the row carries nothing but the
// audio_second pair: zero in every token column, the transcriptions path on both
// path columns, and not one record at Error -- in particular not the speech relay's
// "no usable audio duration" line, which a transcription (no duration HEADER at all)
// used to trip on every request.
func TestTranscriptionUsageBillsAudioSecondsFromTheResponseBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"audio.cpp's own shape", transcriptionDoc("5961.94"), 5.96194},
		{"a whole number of seconds", transcriptionDoc("2000"), 2},
		{"a sub-second fraction", transcriptionDoc("250.5"), 0.2505},
		{"a long recording", transcriptionDoc("93456.789"), 93.456789},
		{"exponent notation", transcriptionDoc("1.5e3"), 1.5},
		{"timing ahead of the text", `{"timing":{"audio_duration_ms":1500},"text":"hello there"}`, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newTranscriptionUpstream(t, http.StatusOK, tc.body)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("client body = %q, want the upstream's document byte for byte (timing block included)", rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want the upstream's application/json", got)
			}
			got := lastUsageEvent(t, srv)
			if got.Status != "success" || got.ErrorCode != "" {
				t.Fatalf("row = %q/%q, want success with no error code", got.Status, got.ErrorCode)
			}
			if math.Abs(got.BillingQuantity-tc.want) > 1e-9 {
				t.Errorf("BillingQuantity = %v, want %v (audio_duration_ms is milliseconds)", got.BillingQuantity, tc.want)
			}
			if got.ProviderPath != "/v1/audio/transcriptions" || got.ReqPath != "/v1/audio/transcriptions" {
				t.Errorf("ProviderPath/ReqPath = %q/%q, want /v1/audio/transcriptions for both", got.ProviderPath, got.ReqPath)
			}
			requireSpeechNonTokenRow(t, got)
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Errorf("a well-formed transcription relay logged at Error: %+v", errs)
			}
		})
	}
}

// A successful relay whose body states no usable duration cannot say how much audio
// it transcribed. The pair has no "unknown", so the row records 0 and the gap is
// logged ONCE at Error (ADR-041: made loud, not repaired) -- and the client still
// gets its transcript. Absent, null, non-numeric (a numeric STRING included),
// zero, negative, out-of-range and structurally broken bodies are all unusable.
func TestTranscriptionUsageWithoutAUsableDurationRecordsZeroAndLogsOnce(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"there is no timing block", `{"text":"hello there"}`},
		{"the timing block has no duration", `{"text":"hello there","timing":{"wall_ms":1200}}`},
		{"the duration is null", `{"text":"hello there","timing":{"audio_duration_ms":null}}`},
		{"the duration is zero", transcriptionDoc("0")},
		{"the duration is a six-decimal zero", transcriptionDoc("0.000000")},
		{"the duration is negative", transcriptionDoc("-5")},
		{"the duration is a numeric string", `{"text":"hello there","timing":{"audio_duration_ms":"2000"}}`},
		{"the duration is not a number", `{"text":"hello there","timing":{"audio_duration_ms":"abc"}}`},
		{"the duration is a bare NaN, which is no JSON", transcriptionDoc("NaN")},
		{"the duration is out of float range", transcriptionDoc("1e999")},
		{"the timing block is no object", `{"text":"hello there","timing":"fast"}`},
		{"the body is plain text", "hello there"},
		{"the body is empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newTranscriptionUpstream(t, http.StatusOK, tc.body)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

			if rec.Code != http.StatusOK || rec.Body.String() != tc.body {
				t.Fatalf("status/body = %d/%q, want 200 and the upstream's bytes (the transcript still reaches the client)", rec.Code, rec.Body.String())
			}
			got := lastUsageEvent(t, srv)
			if got.Status != "success" {
				t.Fatalf("Status = %q, want success", got.Status)
			}
			if got.BillingQuantity != 0 {
				t.Errorf("BillingQuantity = %v, want 0 (no usable duration)", got.BillingQuantity)
			}
			requireSpeechNonTokenRow(t, got)
			errs := errorLogs(buf)
			if len(errs) != 1 || !strings.Contains(errs[0].Msg, "transcription relay succeeded but the upstream reported no usable audio duration") {
				t.Fatalf("Error logs = %+v, want exactly one saying the transcription relay reported no usable audio duration", errs)
			}
			if errs[0].Attrs["path"] != "/v1/audio/transcriptions" {
				t.Errorf("path attr = %v, want /v1/audio/transcriptions", errs[0].Attrs["path"])
			}
		})
	}
}

// The quantity is only trusted once the whole body reached the client: a client
// whose connection breaks mid-write bills 0 (as speech does), and the row is an
// ordinary copy-error failure, not a success.
func TestTranscriptionUsageOfABrokenWriteBillsZeroSeconds(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionDoc("2000"))
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer dev-secret")

	srv.ServeHTTP(brokenWriteRecorder{httptest.NewRecorder()}, req)

	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.ErrorCode != "provider.stream_copy_error" {
		t.Fatalf("row = %q/%q, want error/provider.stream_copy_error", got.Status, got.ErrorCode)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0: the transcript did not reach the client", got.BillingQuantity)
	}
	requireSpeechNonTokenRow(t, got)
}

// brokenWriteRecorder is a response writer whose body writes fail, the way a client
// that went away mid-response does.
type brokenWriteRecorder struct{ *httptest.ResponseRecorder }

func (brokenWriteRecorder) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// transcriptionDurationSeconds is the parser every billed second goes through, so
// its edge cases are pinned directly: a finite number of milliseconds greater than
// zero is a duration, and everything else is not.
func TestTranscriptionDurationSeconds(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   float64
		wantOK bool
	}{
		{"audio.cpp's shape", transcriptionDoc("5961.94"), 5.96194, true},
		{"an integer", transcriptionDoc("2000"), 2, true},
		{"timing before text", `{"timing":{"audio_duration_ms":500},"text":"x"}`, 0.5, true},
		{"unrelated fields of other types are ignored", `{"text":5,"language":["de"],"timing":{"audio_duration_ms":500}}`, 0.5, true},
		{"no timing", `{"text":"x"}`, 0, false},
		{"empty timing", `{"text":"x","timing":{}}`, 0, false},
		{"null timing", `{"text":"x","timing":null}`, 0, false},
		{"null duration", `{"timing":{"audio_duration_ms":null}}`, 0, false},
		{"zero", transcriptionDoc("0"), 0, false},
		{"negative", transcriptionDoc("-0.5"), 0, false},
		{"a numeric string", `{"timing":{"audio_duration_ms":"2000"}}`, 0, false},
		{"a boolean", `{"timing":{"audio_duration_ms":true}}`, 0, false},
		{"a bare NaN", transcriptionDoc("NaN"), 0, false},
		{"a bare Infinity", transcriptionDoc("Infinity"), 0, false},
		{"out of float range", transcriptionDoc("1e999"), 0, false},
		{"timing is a string", `{"timing":"fast"}`, 0, false},
		{"the body is an array", `[{"timing":{"audio_duration_ms":500}}]`, 0, false},
		{"the body is not JSON", "hello", 0, false},
		{"the body is empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := transcriptionDurationSeconds([]byte(tc.body))
			if ok != tc.wantOK || (ok && math.Abs(got-tc.want) > 1e-9) || (!ok && got != 0) {
				t.Fatalf("transcriptionDurationSeconds(%q) = (%v, %v), want (%v, %v)", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// setTranscriptionBillingQuantity records the quantity on a clean success of a
// transcription and nothing else: not a speech exchange, not a relay that did not
// finish, not a success without a usable duration (that one logs).
func TestSetTranscriptionBillingQuantity(t *testing.T) {
	exchange := func(endpoint sessionEndpoint) nativeExchange {
		return nativeExchange{req: inference.Request{APIFlavor: apiFlavorSpeech, Model: "asr-model"}, endpoint: endpoint, id: "req-1", serverName: "srv"}
	}
	body := []byte(transcriptionDoc("3000"))
	t.Run("a clean transcription success sets the quantity", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		t.Cleanup(restore)
		meta := usageMeta{ReqPath: "/v1/audio/transcriptions"}
		setTranscriptionBillingQuantity(&meta, exchange(endpointTranscription), body, "success")
		if meta.BillingQuantity != 3 {
			t.Fatalf("BillingQuantity = %v, want 3", meta.BillingQuantity)
		}
		if errs := errorLogs(buf); len(errs) != 0 {
			t.Fatalf("Error logs = %+v, want none", errs)
		}
	})
	for _, tc := range []struct {
		name     string
		endpoint sessionEndpoint
		status   string
	}{
		{"a speech exchange is not a transcription", endpointSpeech, "success"},
		{"a failed relay bills nothing", endpointTranscription, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			meta := usageMeta{ReqPath: "/v1/audio/transcriptions"}
			setTranscriptionBillingQuantity(&meta, exchange(tc.endpoint), body, tc.status)
			if meta.BillingQuantity != 0 {
				t.Fatalf("BillingQuantity = %v, want 0", meta.BillingQuantity)
			}
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Fatalf("Error logs = %+v, want none", errs)
			}
		})
	}
}

// The speech header setter keys on the ENDPOINT as well as the flavor: a
// transcription shares the openai_audio flavor and carries no duration header, so a
// flavor-only guard would log an Error for every successful transcription and leave
// the quantity at 0. A speech exchange (the endpoint is the placeholder zero value
// in older call sites too) still bills from its header.
func TestSetSpeechBillingQuantityIgnoresATranscriptionExchange(t *testing.T) {
	header := http.Header{}
	header.Set(speechDurationHeader, "2000.0")
	exchange := func(endpoint sessionEndpoint) nativeExchange {
		return nativeExchange{req: inference.Request{APIFlavor: apiFlavorSpeech, Model: "m"}, endpoint: endpoint}
	}
	t.Run("a transcription exchange is left alone and silent", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		t.Cleanup(restore)
		meta := usageMeta{ReqPath: "/v1/audio/transcriptions"}
		setSpeechBillingQuantity(&meta, exchange(endpointTranscription), http.Header{}, "success")
		setSpeechBillingQuantity(&meta, exchange(endpointTranscription), header, "success")
		if meta.BillingQuantity != 0 {
			t.Fatalf("BillingQuantity = %v, want 0", meta.BillingQuantity)
		}
		if errs := errorLogs(buf); len(errs) != 0 {
			t.Fatalf("Error logs = %+v, want none: no duration HEADER is expected on a transcription", errs)
		}
	})
	for _, endpoint := range []sessionEndpoint{endpointSpeech, endpointChat} {
		meta := usageMeta{ReqPath: "/v1/audio/speech"}
		setSpeechBillingQuantity(&meta, exchange(endpoint), header, "success")
		if meta.BillingQuantity != 2 {
			t.Fatalf("endpoint %v: BillingQuantity = %v, want 2 from the header", endpoint, meta.BillingQuantity)
		}
	}
}

// Every transcription row that never reached a finished relay still names its unit
// and an audio_second pair with quantity 0 -- "audio_second, 0" is the truthful
// statement where the default "" would claim a token-metered request. The first
// two go through recordTranscriptionRoutingFailure; the last two are proxyNative's
// shared recordUsage sites, which read the unit through billingUnitFor.
func TestTranscriptionRowsThatNeverFinishedARelayStillCarryTheUnit(t *testing.T) {
	t.Run("a model without a transcription verdict", func(t *testing.T) {
		up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)

		postTranscriptionParts(t, srv, validTranscriptionParts("asr-denied"))

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "routing.model_not_capable" {
			t.Fatalf("ErrorCode = %q, want routing.model_not_capable", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
	})
	t.Run("an agent that has not declared multipart_proxy", func(t *testing.T) {
		srv := newServerAgentTranscriptionTestServer(t, &recordingProxyProvider{respBody: transcriptionOKBody}, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})

		postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "transcription.unavailable" {
			t.Fatalf("ErrorCode = %q, want transcription.unavailable", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
	})
	t.Run("a provider that cannot proxy", func(t *testing.T) {
		srv := newServerAgentTranscriptionTestServer(t, nonProxyCapableProvider{}, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
		declareTranscriptionMultipartProxy(srv)

		postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

		got := lastUsageEvent(t, srv)
		if got.ErrorCode != "provider.unavailable" {
			t.Fatalf("ErrorCode = %q, want provider.unavailable", got.ErrorCode)
		}
		requireSpeechNonTokenRow(t, got)
	})
	t.Run("an upstream call that fails before any response", func(t *testing.T) {
		srv := newServerAgentTranscriptionTestServer(t, erroringProxyProvider{err: errors.New("dial tcp: connection refused")}, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
		declareTranscriptionMultipartProxy(srv)

		postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

		got := lastUsageEvent(t, srv)
		if got.Status != "error" {
			t.Fatalf("Status = %q, want error", got.Status)
		}
		if got.ReqPath != "/v1/audio/transcriptions" {
			t.Errorf("ReqPath = %q, want /v1/audio/transcriptions", got.ReqPath)
		}
		requireSpeechNonTokenRow(t, got)
	})
}
