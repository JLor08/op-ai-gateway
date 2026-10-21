// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"os"
	"testing"
)

// audioCppLiveListing is the shape audio.cpp v0.9.1 really answers on
// GET /v1/models, probed live: an OpenAI-style list whose entries add family,
// task, mode, loaded and path. It may also name an unloaded model, with
// loaded:false. The task field is the signal for the speech, transcription and
// speech_clone verdicts and, for an asr model, the mode field is the signal for
// transcription_streaming; loaded is the health axis the prober also reports
// (AudioCppVerdicts.Loaded), which no verdict depends on.
const audioCppLiveListing = `{"object":"list","data":[
  {"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}
]}`

// audioCppModelCases is the verdict table for audio.cpp's /v1/models listing.
// It is duplicated VERBATIM in
// server-agent/internal/collector/audiocpp_test.go, because the two Go modules
// share no package and each keeps its own copy of the rule
// (parseAudioCppModels here, parseAudioCppModels there). The two copies must
// decide identically on identical input: if a case fails, fix the parser, not
// this table, and change both tables together.
// TestAudioCppModelsCasesMatchTheAgentsTable below fails when the two
// declarations differ by a byte. The declaration is self-contained on purpose
// (plain "yes"/"no" literals, every body inline) so that it can be compared.
var audioCppModelCases = []struct {
	name                       string
	body                       string
	model                      string
	wantSpeech                 string // "yes" | "no" | ""
	wantTranscription          string // "yes" | "no" | ""
	wantTranscriptionStreaming string // "yes" | "no" | ""
	wantSpeechClone            string // "yes" | "no" | ""
}{
	{"real v0.9.1 entry, task tts", `{"object":"list","data":[{"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}]}`, "pocket_tts_english_bf16", "yes", "no", "", "no"},
	{"task tts", `{"data":[{"id":"m","task":"tts"}]}`, "m", "yes", "no", "", "no"},
	{"task clon also produces speech", `{"data":[{"id":"m","task":"clon"}]}`, "m", "yes", "no", "", "yes"},
	{"the legacy clone spelling is not the task (audio.cpp emits clon)", `{"data":[{"id":"m","task":"clone"}]}`, "m", "no", "no", "", ""},
	{"task asr transcribes and does not speak", `{"data":[{"id":"m","task":"asr"}]}`, "m", "no", "yes", "", ""},
	{"an asr model the server lists as not loaded is still a yes", `{"data":[{"id":"m","task":"asr","loaded":false}]}`, "m", "no", "yes", "", ""},
	{"a music task is a no on both axes", `{"data":[{"id":"m","task":"music"}]}`, "m", "no", "no", "", ""},
	{"an unknown task is a no on both axes", `{"data":[{"id":"m","task":"some-future-task"}]}`, "m", "no", "no", "", ""},
	{"model absent from the listing", `{"data":[{"id":"other","task":"tts"}]}`, "m", "", "", "", ""},
	{"empty data", `{"object":"list","data":[]}`, "m", "", "", "", ""},
	{"data absent", `{"object":"list"}`, "m", "", "", "", ""},
	{"data null", `{"data":null}`, "m", "", "", "", ""},
	{"the entry is picked out by id among several", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"},{"id":"c","task":"asr"}]}`, "b", "yes", "no", "", "no"},
	{"the sibling's task does not leak", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"}]}`, "a", "no", "yes", "", ""},
	{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","task":"tts"}]}`, "pocket_tts", "", "", "", ""},
	{"id match is case-sensitive", `{"data":[{"id":"M","task":"tts"}]}`, "m", "", "", "", ""},
	{"a task padded with whitespace still maps", `{"data":[{"id":"m","task":" tts "}]}`, "m", "yes", "no", "", "no"},
	{"task match ignores case", `{"data":[{"id":"m","task":"TTS"}]}`, "m", "yes", "no", "", "no"},
	{"task match ignores case and padding for clon", `{"data":[{"id":"m","task":" Clon "}]}`, "m", "yes", "no", "", "yes"},
	{"task match ignores case for clon", `{"data":[{"id":"m","task":"Clon"}]}`, "m", "yes", "no", "", "yes"},
	{"task match ignores padding for clon", `{"data":[{"id":"m","task":" clon "}]}`, "m", "yes", "no", "", "yes"},
	{"a clone sibling does not leak onto a tts model", `{"data":[{"id":"a","task":"clon"},{"id":"b","task":"tts"}]}`, "b", "yes", "no", "", "no"},
	{"a tts sibling does not leak onto a clone model", `{"data":[{"id":"a","task":"clon"},{"id":"b","task":"tts"}]}`, "a", "yes", "no", "", "yes"},
	{"a differently-cased asr task is still a transcription yes", `{"data":[{"id":"m","task":"ASR"}]}`, "m", "no", "yes", "", ""},
	{"an asr task padded with whitespace still maps", `{"data":[{"id":"m","task":" asr "}]}`, "m", "no", "yes", "", ""},
	{"a named entry without a task says nothing", `{"data":[{"id":"m"}]}`, "m", "", "", "", ""},
	{"a named entry with an empty task says nothing", `{"data":[{"id":"m","task":""}]}`, "m", "", "", "", ""},
	{"a named entry with a blank task says nothing", `{"data":[{"id":"m","task":"  "}]}`, "m", "", "", "", ""},
	{"an empty model id never matches", `{"data":[{"id":"","task":"tts"}]}`, "", "", "", "", ""},
	{"unknown and extra fields are ignored", `{"object":"list","x":1,"data":[{"id":"m","task":"tts","loaded":false,"extra":{"k":[1,2]}}]}`, "m", "yes", "no", "", "no"},
	{"data of the wrong type", `{"data":"m"}`, "m", "", "", "", ""},
	{"a task of the wrong type", `{"data":[{"id":"m","task":1}]}`, "m", "", "", "", ""},
	{"not JSON", `<html>502</html>`, "m", "", "", "", ""},
	{"empty body", ``, "m", "", "", "", ""},
	{"mode streaming is a transcription streaming yes", `{"data":[{"id":"m","task":"asr","mode":"streaming"}]}`, "m", "no", "yes", "yes", ""},
	{"mode offline is a transcription streaming no", `{"data":[{"id":"m","task":"asr","mode":"offline"}]}`, "m", "no", "yes", "no", ""},
	{"a streaming model the server lists as not loaded is still a yes", `{"data":[{"id":"m","task":"asr","mode":"streaming","loaded":false}]}`, "m", "no", "yes", "yes", ""},
	{"mode match ignores case and padding for streaming", `{"data":[{"id":"a","task":"asr","mode":" Streaming "},{"id":"b","task":"asr","mode":"OFFLINE"}]}`, "a", "no", "yes", "yes", ""},
	{"mode match ignores case and padding for offline", `{"data":[{"id":"a","task":"asr","mode":" Streaming "},{"id":"b","task":"asr","mode":"OFFLINE"}]}`, "b", "no", "yes", "no", ""},
	{"an empty mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":""}]}`, "m", "no", "yes", "", ""},
	{"a blank mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":"  "}]}`, "m", "no", "yes", "", ""},
	{"a null mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":null}]}`, "m", "no", "yes", "", ""},
	{"an unknown mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":"realtime"}]}`, "m", "no", "yes", "", ""},
	{"a mode of the wrong type says nothing and leaves the task verdicts alone", `{"data":[{"id":"m","task":"asr","mode":1}]}`, "m", "no", "yes", "", ""},
	{"the sibling's mode does not leak", `{"data":[{"id":"a","task":"asr","mode":"streaming"},{"id":"b","task":"asr","mode":"offline"}]}`, "b", "no", "yes", "no", ""},
	{"a tts model gets no streaming verdict whatever its mode", `{"data":[{"id":"m","task":"tts","mode":"streaming"}]}`, "m", "yes", "no", "", "no"},
	{"a clone model gets no streaming verdict whatever its mode", `{"data":[{"id":"m","task":"clon","mode":"streaming"}]}`, "m", "yes", "no", "", "yes"},
	{"a music model gets no streaming verdict whatever its mode", `{"data":[{"id":"m","task":"music","mode":"streaming"}]}`, "m", "no", "no", "", ""},
	{"an offline non-asr model gets no streaming verdict either", `{"data":[{"id":"m","task":"music","mode":"offline"}]}`, "m", "no", "no", "", ""},
	{"an entry with no task gets no streaming verdict whatever its mode", `{"data":[{"id":"m","mode":"streaming"}]}`, "m", "", "", "", ""},
	{"an asr task is matched ignoring case and padding for the streaming verdict too", `{"data":[{"id":"m","task":" ASR ","mode":"streaming"}]}`, "m", "no", "yes", "yes", ""},
	{"a mode on a model the listing does not name says nothing", `{"data":[{"id":"other","task":"asr","mode":"streaming"}]}`, "m", "", "", "", ""},
}

// TestParseAudioCppModels pins the mapping rule: the loaded entry's own task is
// authoritative, for BOTH task axes at once. A synthesis task (tts, clone)
// is a speech "yes" and a transcription "no"; asr is a transcription "yes" and
// a speech "no"; any other task is a "no" on both; and a model the listing does
// not name -- or a listing this code cannot read -- is no verdict on either.
// The third axis, transcription_streaming, is read from the mode of an asr entry
// only and is no verdict for any other task (see the streaming rows of the
// table). The fourth, speech_clone, refines speech and is read from the task alone:
// tts is a "no", clone a "yes", and every other task, none, or no entry is "" -- an
// asr or music model is no "no" on a speech refinement, it does not speak at all.
func TestParseAudioCppModels(t *testing.T) {
	for _, tc := range audioCppModelCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioCppModels([]byte(tc.body), tc.model)
			want := AudioCppVerdicts{Speech: tc.wantSpeech, Transcription: tc.wantTranscription, TranscriptionStreaming: tc.wantTranscriptionStreaming, SpeechClone: tc.wantSpeechClone}
			if got != want {
				t.Fatalf("parseAudioCppModels = %+v, want %+v", got, want)
			}
		})
	}
}

// TestParseAudioCppModelsMode pins what the shared table cannot say in a row: how
// the mode axis and the task axes meet. The transcription streaming verdict is
// stated for an ASR model ONLY -- the capability is transcription-specific, so a
// model that does not transcribe (tts, clone, music, no task) gets no verdict on it
// whatever its mode, not even a "no" -- and for an ASR model the entry's mode alone
// decides it ("streaming" is a yes, "offline" a no, anything else no answer). The
// mode never moves the speech, transcription or speech_clone verdict the task
// decides, so a listing whose mode field is odd, or an audio.cpp that adds modes,
// cannot switch a route off or turn a clone model into a plain tts one.
func TestParseAudioCppModelsMode(t *testing.T) {
	type taskCase struct {
		verdicts    [2]string // speech, transcription
		speechClone string
		isASR       bool
	}
	tasks := map[string]taskCase{
		"asr":   {[2]string{routing.CapabilityNo, routing.CapabilityYes}, "", true},
		"ASR":   {[2]string{routing.CapabilityNo, routing.CapabilityYes}, "", true},
		"tts":   {[2]string{routing.CapabilityYes, routing.CapabilityNo}, routing.CapabilityNo, false},
		"clon":  {[2]string{routing.CapabilityYes, routing.CapabilityNo}, routing.CapabilityYes, false},
		"music": {[2]string{routing.CapabilityNo, routing.CapabilityNo}, "", false},
		"":      {[2]string{"", ""}, "", false},
	}
	modes := []struct{ field, want string }{
		{``, ""},
		{`,"mode":"streaming"`, routing.CapabilityYes},
		{`,"mode":"offline"`, routing.CapabilityNo},
		{`,"mode":""`, ""},
		{`,"mode":"realtime"`, ""},
		{`,"mode":null`, ""},
		{`,"mode":["streaming"]`, ""},
	}
	for task, tc := range tasks {
		for _, m := range modes {
			body := `{"data":[{"id":"m","task":"` + task + `"` + m.field + `}]}`
			wantStreaming := ""
			if tc.isASR {
				wantStreaming = m.want
			}
			got := parseAudioCppModels([]byte(body), "m")
			want := AudioCppVerdicts{Speech: tc.verdicts[0], Transcription: tc.verdicts[1], TranscriptionStreaming: wantStreaming, SpeechClone: tc.speechClone}
			if got != want {
				t.Errorf("parseAudioCppModels(%s) = %+v, want %+v", body, got, want)
			}
		}
	}
}

// TestParseAudioCppLoaded pins the health axis the prober reports beside the
// verdicts: whether the listing states THIS model as currently loaded. Only a
// boolean true on the named entry is "loaded"; everything the listing does not
// say (no entry, no field, a null, a field of the wrong type, a body that is not
// the listing) is "not loaded", because the voices cache is written only for a
// model observed loaded, and an unreadable statement must never read as one.
//
// It is deliberately a SEPARATE table from audioCppModelCases: that one is the
// capability rule, duplicated verbatim in the agent's module, and loaded is not
// part of it.
func TestParseAudioCppLoaded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		model string
		want  bool
	}{
		{"loaded true", `{"data":[{"id":"m","task":"tts","loaded":true}]}`, "m", true},
		{"loaded false", `{"data":[{"id":"m","task":"tts","loaded":false}]}`, "m", false},
		{"loaded absent", `{"data":[{"id":"m","task":"tts"}]}`, "m", false},
		{"loaded null", `{"data":[{"id":"m","task":"tts","loaded":null}]}`, "m", false},
		{"loaded as the string true is not a boolean", `{"data":[{"id":"m","task":"tts","loaded":"true"}]}`, "m", false},
		{"loaded as the number 1 is not a boolean", `{"data":[{"id":"m","task":"tts","loaded":1}]}`, "m", false},
		{"loaded on an entry with no task still counts", `{"data":[{"id":"m","loaded":true}]}`, "m", true},
		{"the real v0.9.1 entry", audioCppLiveListing, "pocket_tts_english_bf16", true},
		{"the entry is picked out by id", `{"data":[{"id":"a","loaded":false},{"id":"b","loaded":true},{"id":"c","loaded":false}]}`, "b", true},
		{"a loaded sibling does not leak", `{"data":[{"id":"a","loaded":true},{"id":"b","loaded":false}]}`, "b", false},
		{"model absent from the listing", `{"data":[{"id":"other","loaded":true}]}`, "m", false},
		{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","loaded":true}]}`, "pocket_tts", false},
		{"id match is case-sensitive", `{"data":[{"id":"M","loaded":true}]}`, "m", false},
		{"an empty model id never matches", `{"data":[{"id":"","loaded":true}]}`, "", false},
		{"empty data", `{"data":[]}`, "m", false},
		{"data null", `{"data":null}`, "m", false},
		{"data of the wrong type", `{"data":"m"}`, "m", false},
		{"not JSON", `<html>502</html>`, "m", false},
		{"empty body", ``, "m", false},
		{"the first entry carrying the id answers", `{"data":[{"id":"m","loaded":false},{"id":"m","loaded":true}]}`, "m", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAudioCppLoaded([]byte(tc.body), tc.model); got != tc.want {
				t.Fatalf("parseAudioCppLoaded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProbeAudioCppModelsOddLoadedCostsOnlyLoaded: a loaded field of a type this
// code does not expect must cost only Loaded, never the verdicts the task
// decides (the mode precedent), because a capability "no" or "yes" must not
// hang on an unrelated health field.
func TestProbeAudioCppModelsOddLoadedCostsOnlyLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","task":"tts","loaded":"yes"}]}`))
	}))
	defer srv.Close()
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if want := (AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo}); got != want {
		t.Fatalf("got %+v, want %+v (an odd loaded type must not cost the task verdicts)", got, want)
	}
}

// agentAudioCppCasesPath is the agent's copy of audioCppModelCases, relative to
// this package's directory, where go test runs it. Both Go modules live in one
// repository, and CI checks the whole repository out before it tests this
// module (the same prefix as agentSdcppCasesPath).
const agentAudioCppCasesPath = "../../../../server-agent/internal/collector/audiocpp_test.go"

// TestAudioCppModelsCasesMatchTheAgentsTable holds the two copies of
// audioCppModelCases identical. Each copy is pinned against its own module's
// parseAudioCppModels, so a change to one parser together with its own table
// would pass both suites while the two rules drift apart. This reads the
// agent's test file off disk and compares the two declarations byte for byte.
// Both extracts must be non-empty: a renamed or reshaped table would otherwise
// compare nothing with nothing and pass.
func TestAudioCppModelsCasesMatchTheAgentsTable(t *testing.T) {
	const ownPath = "audiocpp_models_test.go"
	own, err := os.ReadFile(ownPath)
	if err != nil {
		t.Fatalf("read %s: %v", ownPath, err)
	}
	agent, err := os.ReadFile(agentAudioCppCasesPath)
	if err != nil {
		t.Fatalf("read the agent's copy %s: %v (the two modules are one repository, and this check needs both)", agentAudioCppCasesPath, err)
	}
	ownBlock, agentBlock := sdcppVarBlock(own, "audioCppModelCases"), sdcppVarBlock(agent, "audioCppModelCases")
	if len(ownBlock) == 0 {
		t.Fatalf("no `var audioCppModelCases = ...` declaration ending in a line \"}\" found in %s", ownPath)
	}
	if len(agentBlock) == 0 {
		t.Fatalf("no `var audioCppModelCases = ...` declaration ending in a line \"}\" found in %s", agentAudioCppCasesPath)
	}
	if !bytes.Equal(ownBlock, agentBlock) {
		t.Fatalf("audioCppModelCases differs between gateway/backend/internal/provider/audiocpp_models_test.go and "+
			"server-agent/internal/collector/audiocpp_test.go. The two parseAudioCppModels copies must decide identically: "+
			"change both tables, and both parsers, together.\n--- gateway\n%s\n--- agent\n%s", ownBlock, agentBlock)
	}
}

func TestProbeAudioCppModels(t *testing.T) {
	probe := func(t *testing.T, srv *httptest.Server, ctx context.Context, modelID string) (AudioCppVerdicts, error) {
		t.Helper()
		return NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(ctx, routing.Target{Endpoint: srv.URL}, modelID)
	}

	t.Run("a tts model yields yes from GET /v1/models", func(t *testing.T) {
		var method, path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if want := (AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, Loaded: true}); got != want {
			t.Fatalf("got %+v, want %+v (the live entry is a loaded tts model: speech_clone no, mode offline, but no transcription streaming verdict)", got, want)
		}
		if method != http.MethodGet || path != "/v1/models" {
			t.Fatalf("request = %s %s, want GET /v1/models", method, path)
		}
	})

	t.Run("an asr model yields a transcription yes and a speech no", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"whisper_small","task":"asr","loaded":true}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "whisper_small")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if want := (AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, Loaded: true}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a streaming asr model yields a transcription streaming yes from the same listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"vibevoice_asr_streaming_7b_bf16","family":"vibevoice_asr","task":"asr","mode":"streaming","loaded":true}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "vibevoice_asr_streaming_7b_bf16")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityYes, Loaded: true}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a clone model yields a speech clone yes from the same listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"voxcpm_clone","family":"voxcpm","task":"clon","mode":"offline","loaded":true}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "voxcpm_clone")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes, Loaded: true}
		if got != want {
			t.Fatalf("got %+v, want %+v (a clone model is still a speech yes: speech_clone refines speech, it never replaces it)", got, want)
		}
	})

	t.Run("an unloaded entry still states its verdicts and is not Loaded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"pocket_tts_english_bf16","task":"tts","loaded":false}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo}
		if got != want {
			t.Fatalf("got %+v, want %+v (loaded is the health axis: an unloaded model keeps its capability verdicts and is simply not Loaded)", got, want)
		}
	})

	t.Run("a model the listing does not name yields no verdict and no error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "not_loaded")
		if err != nil {
			t.Fatalf("error = %v, want nil -- the server answered, it just does not name the model", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict on either axis -- a missing entry says nothing about the model", got)
		}
	})

	t.Run("a non-2xx status is an error even when the body would decode to a yes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable for a 503", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict -- a failing server's body is not its answer", got)
		}
	})

	t.Run("a transport failure is an error and no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := NewOpenAICompatibleClient(srv.Client())
		endpoint := srv.URL
		srv.Close() // nothing listens any more
		got, err := client.ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: endpoint}, "m")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict", got)
		}
	})

	t.Run("a body that is not JSON yields no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not the listing</html>"))
		}))
		defer srv.Close()
		got, _ := probe(t, srv, context.Background(), "m")
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict on either axis -- an unreadable body must not read as a verdict", got)
		}
	})

	t.Run("the application's credential is attached", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "audio-secret")
		if _, err := probe(t, srv, ctx, "pocket_tts_english_bf16"); err != nil {
			t.Fatalf("error = %v", err)
		}
		if gotAuth != "audio-secret" {
			t.Fatalf("X-Api-Key = %q, want audio-secret", gotAuth)
		}
	})
}

// TestMultiplexerDispatchesAudioCppModelsToOpenAICompatibleClient mirrors the
// sdcpp dispatch test: a target whose provider maps to the shared
// OpenAI-compatible client reaches its ProbeAudioCppModels -- so the probe
// rides the outbound app transport -- while a provider whose client lacks the
// capability answers ErrUnavailable and sends nothing (no cross-route).
func TestMultiplexerDispatchesAudioCppModelsToOpenAICompatibleClient(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(audioCppLiveListing))
	}))
	defer srv.Close()
	shared := NewOpenAICompatibleClient(srv.Client())
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:     shared,
		routing.ProviderAudioCpp: shared, // same instance, exactly as providerClients wires it
		routing.ProviderOllama:   &recordingClient{},
	}, nil)

	got, err := mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderAudioCpp, Endpoint: srv.URL}, "pocket_tts_english_bf16")
	if err != nil {
		t.Fatalf("ProbeAudioCppModels(audio_cpp) returned %v", err)
	}
	if want := (AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, Loaded: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(paths) != 1 || paths[0] != "/v1/models" {
		t.Fatalf("upstream paths = %v, want exactly [/v1/models]", paths)
	}

	got, err = mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}, "pocket_tts_english_bf16")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProbeAudioCppModels(ollama) error = %v, want ErrUnavailable -- a client without the capability has no verdict", err)
	}
	if got != (AudioCppVerdicts{}) {
		t.Fatalf("ProbeAudioCppModels(ollama) = %+v, want the zero value", got)
	}
	if len(paths) != 1 {
		t.Fatalf("upstream paths = %v, want no request for a client without the capability", paths)
	}
}

// TestMultiplexerAudioCppModelsWithoutProberIsUnavailable pins the bare
// posture: with no client that implements AudioCppModelsProber -- neither the
// provider's nor a fallback -- the multiplexer answers ErrUnavailable. A zero
// value with a nil error would read as a successful probe that found nothing.
func TestMultiplexerAudioCppModelsWithoutProberIsUnavailable(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)

	got, err := mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderMock, Endpoint: "http://x:8000"}, "m")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if got != (AudioCppVerdicts{}) {
		t.Fatalf("got %+v, want the zero value", got)
	}

	var nilMux *Multiplexer
	if _, err := nilMux.ProbeAudioCppModels(context.Background(), routing.Target{}, "m"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil multiplexer error = %v, want ErrUnavailable", err)
	}
}
