// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// audioCppLiveListing is the shape audio.cpp v0.9.1 really answers on
// GET /v1/models: an OpenAI-style list whose entries add family, task, mode,
// loaded and path. The task field is the capability signal (speech, transcription,
// speech_clone) and, for an asr model, the mode field is the streaming signal.
const audioCppLiveListing = `{"object":"list","data":[
  {"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}
]}`

// audioCppModelCases is the verdict table for audio.cpp's /v1/models listing.
// It is duplicated VERBATIM in
// gateway/backend/internal/provider/audiocpp_models_test.go, because the two
// Go modules share no package and each keeps its own copy of the rule
// (parseAudioCppModels there, parseAudioCppModels here). The two copies must
// decide identically on identical input: if a case fails, fix the parser, not
// this table, and change both tables together.
// TestAudioCppModelsCasesMatchTheAgentsTable there fails when the two
// declarations differ by a byte.
var audioCppModelCases = []struct {
	name                       string
	body                       string
	model                      string
	wantSpeech                 string // "yes" | "no" | ""
	wantTranscription          string // "yes" | "no" | ""
	wantTranscriptionStreaming string // "yes" | "no" | ""
	wantSpeechClone            string // "yes" | "no" | ""
	wantSpeechStreaming        string // "yes" | "no" | ""
}{
	{"real v0.9.1 entry, task tts", `{"object":"list","data":[{"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}]}`, "pocket_tts_english_bf16", "yes", "no", "", "no", "no"},
	{"task tts", `{"data":[{"id":"m","task":"tts"}]}`, "m", "yes", "no", "", "no", ""},
	{"task clon also produces speech", `{"data":[{"id":"m","task":"clon"}]}`, "m", "yes", "no", "", "yes", ""},
	{"the legacy clone spelling is not the task (audio.cpp emits clon)", `{"data":[{"id":"m","task":"clone"}]}`, "m", "no", "no", "", "", ""},
	{"task asr transcribes and does not speak", `{"data":[{"id":"m","task":"asr"}]}`, "m", "no", "yes", "", "", ""},
	{"an asr model the server lists as not loaded is still a yes", `{"data":[{"id":"m","task":"asr","loaded":false}]}`, "m", "no", "yes", "", "", ""},
	{"a music task is a no on both axes", `{"data":[{"id":"m","task":"music"}]}`, "m", "no", "no", "", "", ""},
	{"an unknown task is a no on both axes", `{"data":[{"id":"m","task":"some-future-task"}]}`, "m", "no", "no", "", "", ""},
	{"model absent from the listing", `{"data":[{"id":"other","task":"tts"}]}`, "m", "", "", "", "", ""},
	{"empty data", `{"object":"list","data":[]}`, "m", "", "", "", "", ""},
	{"data absent", `{"object":"list"}`, "m", "", "", "", "", ""},
	{"data null", `{"data":null}`, "m", "", "", "", "", ""},
	{"the entry is picked out by id among several", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"},{"id":"c","task":"asr"}]}`, "b", "yes", "no", "", "no", ""},
	{"the sibling's task does not leak", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"}]}`, "a", "no", "yes", "", "", ""},
	{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","task":"tts"}]}`, "pocket_tts", "", "", "", "", ""},
	{"id match is case-sensitive", `{"data":[{"id":"M","task":"tts"}]}`, "m", "", "", "", "", ""},
	{"a task padded with whitespace still maps", `{"data":[{"id":"m","task":" tts "}]}`, "m", "yes", "no", "", "no", ""},
	{"task match ignores case", `{"data":[{"id":"m","task":"TTS"}]}`, "m", "yes", "no", "", "no", ""},
	{"task match ignores case and padding for clon", `{"data":[{"id":"m","task":" Clon "}]}`, "m", "yes", "no", "", "yes", ""},
	{"task match ignores case for clon", `{"data":[{"id":"m","task":"Clon"}]}`, "m", "yes", "no", "", "yes", ""},
	{"task match ignores padding for clon", `{"data":[{"id":"m","task":" clon "}]}`, "m", "yes", "no", "", "yes", ""},
	{"a clone sibling does not leak onto a tts model", `{"data":[{"id":"a","task":"clon"},{"id":"b","task":"tts"}]}`, "b", "yes", "no", "", "no", ""},
	{"a tts sibling does not leak onto a clone model", `{"data":[{"id":"a","task":"clon"},{"id":"b","task":"tts"}]}`, "a", "yes", "no", "", "yes", ""},
	{"a differently-cased asr task is still a transcription yes", `{"data":[{"id":"m","task":"ASR"}]}`, "m", "no", "yes", "", "", ""},
	{"an asr task padded with whitespace still maps", `{"data":[{"id":"m","task":" asr "}]}`, "m", "no", "yes", "", "", ""},
	{"a named entry without a task says nothing", `{"data":[{"id":"m"}]}`, "m", "", "", "", "", ""},
	{"a named entry with an empty task says nothing", `{"data":[{"id":"m","task":""}]}`, "m", "", "", "", "", ""},
	{"a named entry with a blank task says nothing", `{"data":[{"id":"m","task":"  "}]}`, "m", "", "", "", "", ""},
	{"an empty model id never matches", `{"data":[{"id":"","task":"tts"}]}`, "", "", "", "", "", ""},
	{"unknown and extra fields are ignored", `{"object":"list","x":1,"data":[{"id":"m","task":"tts","loaded":false,"extra":{"k":[1,2]}}]}`, "m", "yes", "no", "", "no", ""},
	{"data of the wrong type", `{"data":"m"}`, "m", "", "", "", "", ""},
	{"a task of the wrong type", `{"data":[{"id":"m","task":1}]}`, "m", "", "", "", "", ""},
	{"not JSON", `<html>502</html>`, "m", "", "", "", "", ""},
	{"empty body", ``, "m", "", "", "", "", ""},
	{"mode streaming is a transcription streaming yes", `{"data":[{"id":"m","task":"asr","mode":"streaming"}]}`, "m", "no", "yes", "yes", "", ""},
	{"mode offline is a transcription streaming no", `{"data":[{"id":"m","task":"asr","mode":"offline"}]}`, "m", "no", "yes", "no", "", ""},
	{"a streaming model the server lists as not loaded is still a yes", `{"data":[{"id":"m","task":"asr","mode":"streaming","loaded":false}]}`, "m", "no", "yes", "yes", "", ""},
	{"mode match ignores case and padding for streaming", `{"data":[{"id":"a","task":"asr","mode":" Streaming "},{"id":"b","task":"asr","mode":"OFFLINE"}]}`, "a", "no", "yes", "yes", "", ""},
	{"mode match ignores case and padding for offline", `{"data":[{"id":"a","task":"asr","mode":" Streaming "},{"id":"b","task":"asr","mode":"OFFLINE"}]}`, "b", "no", "yes", "no", "", ""},
	{"an empty mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":""}]}`, "m", "no", "yes", "", "", ""},
	{"a blank mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":"  "}]}`, "m", "no", "yes", "", "", ""},
	{"a null mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":null}]}`, "m", "no", "yes", "", "", ""},
	{"an unknown mode says nothing about streaming", `{"data":[{"id":"m","task":"asr","mode":"realtime"}]}`, "m", "no", "yes", "", "", ""},
	{"a mode of the wrong type says nothing and leaves the task verdicts alone", `{"data":[{"id":"m","task":"asr","mode":1}]}`, "m", "no", "yes", "", "", ""},
	{"the sibling's mode does not leak", `{"data":[{"id":"a","task":"asr","mode":"streaming"},{"id":"b","task":"asr","mode":"offline"}]}`, "b", "no", "yes", "no", "", ""},
	{"a tts model in streaming mode is a speech streaming yes and no transcription streaming one", `{"data":[{"id":"m","task":"tts","mode":"streaming"}]}`, "m", "yes", "no", "", "no", "yes"},
	{"a clone model in streaming mode is a speech streaming yes too", `{"data":[{"id":"m","task":"clon","mode":"streaming"}]}`, "m", "yes", "no", "", "yes", "yes"},
	{"a tts model in offline mode is a speech streaming no", `{"data":[{"id":"m","task":"tts","mode":"offline"}]}`, "m", "yes", "no", "", "no", "no"},
	{"a clone model in offline mode is a speech streaming no", `{"data":[{"id":"m","task":"clon","mode":"offline"}]}`, "m", "yes", "no", "", "yes", "no"},
	{"a tts model the server lists as not loaded is still a speech streaming yes", `{"data":[{"id":"m","task":"tts","mode":"streaming","loaded":false}]}`, "m", "yes", "no", "", "no", "yes"},
	{"a tts task and mode match ignore case and padding for streaming", `{"data":[{"id":"m","task":" TTS ","mode":" Streaming "}]}`, "m", "yes", "no", "", "no", "yes"},
	{"a clone task and mode match ignore case and padding for offline", `{"data":[{"id":"m","task":" Clon ","mode":"OFFLINE"}]}`, "m", "yes", "no", "", "yes", "no"},
	{"an empty mode says nothing about speech streaming", `{"data":[{"id":"m","task":"tts","mode":""}]}`, "m", "yes", "no", "", "no", ""},
	{"a blank mode says nothing about speech streaming", `{"data":[{"id":"m","task":"clon","mode":"  "}]}`, "m", "yes", "no", "", "yes", ""},
	{"a null mode says nothing about speech streaming", `{"data":[{"id":"m","task":"tts","mode":null}]}`, "m", "yes", "no", "", "no", ""},
	{"an unknown mode says nothing about speech streaming", `{"data":[{"id":"m","task":"tts","mode":"realtime"}]}`, "m", "yes", "no", "", "no", ""},
	{"a mode of the wrong type says nothing about speech streaming and leaves the task verdicts alone", `{"data":[{"id":"m","task":"clon","mode":1}]}`, "m", "yes", "no", "", "yes", ""},
	{"the sibling's mode does not leak onto a speech model", `{"data":[{"id":"a","task":"tts","mode":"streaming"},{"id":"b","task":"tts","mode":"offline"}]}`, "b", "yes", "no", "", "no", "no"},
	{"a streaming asr sibling does not leak a speech streaming verdict onto a tts model", `{"data":[{"id":"a","task":"asr","mode":"streaming"},{"id":"b","task":"tts"}]}`, "b", "yes", "no", "", "no", ""},
	{"a streaming tts sibling does not leak a transcription streaming verdict onto an asr model", `{"data":[{"id":"a","task":"tts","mode":"streaming"},{"id":"b","task":"asr"}]}`, "b", "no", "yes", "", "", ""},
	{"a streaming asr model gets no speech streaming verdict", `{"data":[{"id":"m","task":"asr","mode":"streaming"}]}`, "m", "no", "yes", "yes", "", ""},
	{"an offline asr model gets no speech streaming verdict either", `{"data":[{"id":"m","task":"asr","mode":"offline"}]}`, "m", "no", "yes", "no", "", ""},
	{"a music model gets no streaming verdict whatever its mode", `{"data":[{"id":"m","task":"music","mode":"streaming"}]}`, "m", "no", "no", "", "", ""},
	{"an offline non-asr model gets no streaming verdict either", `{"data":[{"id":"m","task":"music","mode":"offline"}]}`, "m", "no", "no", "", "", ""},
	{"an entry with no task gets no streaming verdict whatever its mode", `{"data":[{"id":"m","mode":"streaming"}]}`, "m", "", "", "", "", ""},
	{"an asr task is matched ignoring case and padding for the streaming verdict too", `{"data":[{"id":"m","task":" ASR ","mode":"streaming"}]}`, "m", "no", "yes", "yes", "", ""},
	{"a mode on a model the listing does not name says nothing", `{"data":[{"id":"other","task":"asr","mode":"streaming"}]}`, "m", "", "", "", "", ""},
}

func TestParseAudioCppModelsTwinTable(t *testing.T) {
	for _, tc := range audioCppModelCases {
		t.Run(tc.name, func(t *testing.T) {
			speech, transcription, streaming, speechClone, speechStreaming := parseAudioCppModels([]byte(tc.body), tc.model)
			if speech != tc.wantSpeech || transcription != tc.wantTranscription || streaming != tc.wantTranscriptionStreaming ||
				speechClone != tc.wantSpeechClone || speechStreaming != tc.wantSpeechStreaming {
				t.Fatalf("speech=%q transcription=%q streaming=%q speechClone=%q speechStreaming=%q, want speech=%q transcription=%q streaming=%q speechClone=%q speechStreaming=%q",
					speech, transcription, streaming, speechClone, speechStreaming,
					tc.wantSpeech, tc.wantTranscription, tc.wantTranscriptionStreaming, tc.wantSpeechClone, tc.wantSpeechStreaming)
			}
		})
	}
}

// serveAudioCppModels serves body with status on GET /v1/models and fails the
// test on any other request. The path is the literal, not AudioCppModelsPath:
// comparing the constant with itself would not notice the constant drifting
// away from the path a real audio.cpp serves.
func serveAudioCppModels(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			t.Errorf("request %s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestProbeAudioCppVerdictsTaskCases drives every row of the shared table
// through the prober over HTTP. A 2xx body that is syntactically JSON is
// conclusive (stable), whatever it says about the model: the same build
// will answer the same way. A body that is not JSON reads as a child still
// mid-response, so it is transient and carries no verdict.
func TestProbeAudioCppVerdictsTaskCases(t *testing.T) {
	for _, tc := range audioCppModelCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveAudioCppModels(t, 200, tc.body)
			defer srv.Close()
			got, stable := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, tc.model)
			wantStable := json.Valid([]byte(tc.body))
			if stable != wantStable {
				t.Fatalf("stable = %v, want %v", stable, wantStable)
			}
			if got.Caps.Speech != tc.wantSpeech || got.Caps.Transcription != tc.wantTranscription ||
				got.Caps.TranscriptionStreaming != tc.wantTranscriptionStreaming || got.Caps.SpeechClone != tc.wantSpeechClone ||
				got.Caps.SpeechStreaming != tc.wantSpeechStreaming {
				t.Fatalf("speech=%q transcription=%q streaming=%q speechClone=%q speechStreaming=%q, want speech=%q transcription=%q streaming=%q speechClone=%q speechStreaming=%q",
					got.Caps.Speech, got.Caps.Transcription, got.Caps.TranscriptionStreaming, got.Caps.SpeechClone, got.Caps.SpeechStreaming,
					tc.wantSpeech, tc.wantTranscription, tc.wantTranscriptionStreaming, tc.wantSpeechClone, tc.wantSpeechStreaming)
			}
			assertAudioCppOnlyCaps(t, got)
		})
	}
}

// assertAudioCppOnlyCaps pins that the audio.cpp prober fills Speech,
// Transcription, TranscriptionStreaming, SpeechClone and SpeechStreaming and nothing else: audio.cpp has no
// timings surface (no LiveProgress), no image options, and states nothing about
// the /props-derived capabilities.
func assertAudioCppOnlyCaps(t *testing.T, got PropsVerdicts) {
	t.Helper()
	if got.LiveProgress != "" {
		t.Fatalf("LiveProgress = %q, want empty", got.LiveProgress)
	}
	if got.ImageOptions != nil {
		t.Fatalf("ImageOptions = %+v, want nil", got.ImageOptions)
	}
	c := got.Caps
	if c.Vision != "" || c.Video != "" || c.Audio != "" || c.Tools != "" || c.Image != "" || c.Extra != nil {
		t.Fatalf("Caps = %+v, want only Speech/Transcription/TranscriptionStreaming/SpeechClone/SpeechStreaming set", c)
	}
}

// TestProbeAudioCppVerdicts pins the stable/transient split. It is the SAME
// contract as ProbeSdcppVerdicts, ProbePropsVerdicts and ProbeOllamaVerdicts:
// a readable document, or a status that is a fixed property of the binary
// (404, 401, 403, 405), is conclusive and may be cached for the pid's
// lifetime. Anything else (5xx above all, no response, a body that is not
// JSON) is transient and must be retried.
func TestProbeAudioCppVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantSpeech string
		wantTrans  string
		wantStable bool
	}{
		{"tts is speech yes", 200, audioCppLiveListing, "yes", "no", true},
		{"asr is transcription yes", 200, `{"data":[{"id":"pocket_tts_english_bf16","task":"asr"}]}`, "no", "yes", true},
		{"model not in the listing is conclusive, no verdict", 200, `{"data":[{"id":"other","task":"tts"}]}`, "", "", true},
		{"empty data is conclusive, no verdict", 200, `{"object":"list","data":[]}`, "", "", true},
		{"named without a task is conclusive, no verdict", 200, `{"data":[{"id":"pocket_tts_english_bf16"}]}`, "", "", true},
		{"wrong shape is conclusive, no verdict", 200, `{"data":"pocket_tts_english_bf16"}`, "", "", true},
		{"not JSON is transient", 200, `{"data":[`, "", "", false},
		{"html body is transient", 200, `<html>502</html>`, "", "", false},
		{"404 is conclusive", 404, ``, "", "", true},
		{"401 is conclusive", 401, ``, "", "", true},
		{"403 is conclusive", 403, ``, "", "", true},
		{"405 is conclusive", 405, ``, "", "", true},
		{"500 is transient", 500, audioCppLiveListing, "", "", false},
		{"503 is transient", 503, audioCppLiveListing, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveAudioCppModels(t, tc.status, tc.body)
			defer srv.Close()
			got, stable := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "pocket_tts_english_bf16")
			if stable != tc.wantStable || got.Caps.Speech != tc.wantSpeech || got.Caps.Transcription != tc.wantTrans {
				t.Fatalf("got speech=%q transcription=%q stable=%v, want speech=%q transcription=%q stable=%v",
					got.Caps.Speech, got.Caps.Transcription, stable, tc.wantSpeech, tc.wantTrans, tc.wantStable)
			}
			assertAudioCppOnlyCaps(t, got)
		})
	}

	t.Run("no response is transient", func(t *testing.T) {
		srv := serveAudioCppModels(t, 200, `{}`)
		url := srv.URL
		srv.Close()
		got, stable := ProbeAudioCppVerdicts(context.Background(), http.DefaultClient, url, "pocket_tts_english_bf16")
		if stable {
			t.Fatal("stable = true for a closed port, want false")
		}
		if !reflect.DeepEqual(got, PropsVerdicts{}) {
			t.Fatalf("got %+v for a closed port, want the zero value", got)
		}
	})

	t.Run("a base URL with a trailing slash is probed at /v1/models", func(t *testing.T) {
		srv := serveAudioCppModels(t, 200, audioCppLiveListing)
		defer srv.Close()
		got, stable := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL+"/", "pocket_tts_english_bf16")
		if !stable || got.Caps.Speech != "yes" || got.Caps.Transcription != "no" {
			t.Fatalf("got speech=%q transcription=%q stable=%v, want yes/no/true",
				got.Caps.Speech, got.Caps.Transcription, stable)
		}
	})
}

// audioCppStreamingBody is the /v1/models body of one model (id "m") with the
// given raw JSON task and mode, to drive the streaming verdict through the
// prober over HTTP. The shared table pins the parser; the tests below pin that
// the verdict reaches Capabilities.TranscriptionStreaming and that the
// speech/transcription verdicts do not move with it.
func audioCppStreamingBody(task, mode string) string {
	return `{"data":[{"id":"m","task":` + task + `,"mode":` + mode + `}]}`
}

// TestProbeAudioCppVerdictsReportsStreamingMode proves the child's mode reaches
// Capabilities.TranscriptionStreaming for an ASR model: streaming is a yes,
// offline is a no, and a mode the rule does not know (or one of the wrong
// type) states nothing. The task verdicts are the same in every row.
func TestProbeAudioCppVerdictsReportsStreamingMode(t *testing.T) {
	cases := []struct {
		name          string
		task, mode    string
		wantStreaming string
	}{
		{"streaming is a yes", `"asr"`, `"streaming"`, "yes"},
		{"offline is a no", `"asr"`, `"offline"`, "no"},
		{"case and padding are ignored", `"asr"`, `" Streaming "`, "yes"},
		{"an unknown mode states nothing", `"asr"`, `"realtime"`, ""},
		{"a null mode states nothing", `"asr"`, `null`, ""},
		{"a mode of the wrong type states nothing", `"asr"`, `1`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveAudioCppModels(t, 200, audioCppStreamingBody(tc.task, tc.mode))
			defer srv.Close()
			got, stable := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "m")
			if !stable {
				t.Fatal("stable = false, want true (a readable listing is conclusive)")
			}
			if got.Caps.TranscriptionStreaming != tc.wantStreaming {
				t.Errorf("TranscriptionStreaming = %q, want %q", got.Caps.TranscriptionStreaming, tc.wantStreaming)
			}
			if got.Caps.Speech != "no" || got.Caps.Transcription != "yes" {
				t.Errorf("speech=%q transcription=%q, want no/yes (the mode never moves the task verdicts)",
					got.Caps.Speech, got.Caps.Transcription)
			}
			assertAudioCppOnlyCaps(t, got)
		})
	}
}

// TestProbeAudioCppVerdictsStatesStreamingForAsrOnly pins the ASR gate: a model
// whose task is not asr gets NO streaming verdict, not even a no, whatever its
// mode says. A tts model that is "streaming" (or the live v0.9.1 entry, whose
// mode is "offline") must not put a transcription_streaming capability on a
// route that does not transcribe.
func TestProbeAudioCppVerdictsStatesStreamingForAsrOnly(t *testing.T) {
	for _, task := range []string{`"tts"`, `"clon"`, `"music"`, `"some-future-task"`, `""`} {
		for _, mode := range []string{`"streaming"`, `"offline"`} {
			t.Run(task+" "+mode, func(t *testing.T) {
				srv := serveAudioCppModels(t, 200, audioCppStreamingBody(task, mode))
				defer srv.Close()
				got, _ := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "m")
				if got.Caps.TranscriptionStreaming != "" {
					t.Errorf("TranscriptionStreaming = %q, want \"\" for a non-asr model", got.Caps.TranscriptionStreaming)
				}
			})
		}
	}
	t.Run("the live v0.9.1 tts entry", func(t *testing.T) {
		srv := serveAudioCppModels(t, 200, audioCppLiveListing)
		defer srv.Close()
		got, _ := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "pocket_tts_english_bf16")
		if got.Caps.TranscriptionStreaming != "" {
			t.Errorf("TranscriptionStreaming = %q, want \"\" (a tts model with mode offline)", got.Caps.TranscriptionStreaming)
		}
	})
}

// TestProbeAudioCppVerdictsReportsSpeechStreamingMode is the speech twin of
// TestProbeAudioCppVerdictsReportsStreamingMode: the child's mode reaches
// Capabilities.SpeechStreaming for a tts or clon model (streaming is a yes,
// offline a no, and a mode the rule does not know, or one of the wrong type,
// states nothing), never Capabilities.TranscriptionStreaming, and the task
// verdicts (and speech_clone) are what the task alone decides in every row.
func TestProbeAudioCppVerdictsReportsSpeechStreamingMode(t *testing.T) {
	cases := []struct {
		name          string
		task, mode    string
		wantStreaming string
		wantClone     string
	}{
		{"a tts model in streaming mode is a yes", `"tts"`, `"streaming"`, "yes", "no"},
		{"a tts model in offline mode is a no", `"tts"`, `"offline"`, "no", "no"},
		{"a clone model in streaming mode is a yes", `"clon"`, `"streaming"`, "yes", "yes"},
		{"a clone model in offline mode is a no", `"clon"`, `"offline"`, "no", "yes"},
		{"case and padding are ignored", `"tts"`, `" Streaming "`, "yes", "no"},
		{"an unknown mode states nothing", `"tts"`, `"realtime"`, "", "no"},
		{"a null mode states nothing", `"clon"`, `null`, "", "yes"},
		{"a mode of the wrong type states nothing", `"tts"`, `1`, "", "no"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveAudioCppModels(t, 200, audioCppStreamingBody(tc.task, tc.mode))
			defer srv.Close()
			got, stable := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "m")
			if !stable {
				t.Fatal("stable = false, want true (a readable listing is conclusive)")
			}
			if got.Caps.SpeechStreaming != tc.wantStreaming {
				t.Errorf("SpeechStreaming = %q, want %q", got.Caps.SpeechStreaming, tc.wantStreaming)
			}
			if got.Caps.TranscriptionStreaming != "" {
				t.Errorf("TranscriptionStreaming = %q, want \"\" (a speech model has no answer on the transcriber's axis)", got.Caps.TranscriptionStreaming)
			}
			if got.Caps.Speech != "yes" || got.Caps.Transcription != "no" || got.Caps.SpeechClone != tc.wantClone {
				t.Errorf("speech=%q transcription=%q speechClone=%q, want yes/no/%s (the mode never moves the task verdicts)",
					got.Caps.Speech, got.Caps.Transcription, got.Caps.SpeechClone, tc.wantClone)
			}
			assertAudioCppOnlyCaps(t, got)
		})
	}
}

// TestProbeAudioCppVerdictsStatesSpeechStreamingForSpeechOnly pins the speech
// gate, the twin of the ASR gate above: a model whose task is not tts or clon
// gets NO speech_streaming verdict, not even a no, whatever its mode says. An
// asr model that is "streaming" must not put a speech_streaming capability on a
// route that does not synthesise.
func TestProbeAudioCppVerdictsStatesSpeechStreamingForSpeechOnly(t *testing.T) {
	for _, task := range []string{`"asr"`, `"music"`, `"some-future-task"`, `""`} {
		for _, mode := range []string{`"streaming"`, `"offline"`} {
			t.Run(task+" "+mode, func(t *testing.T) {
				srv := serveAudioCppModels(t, 200, audioCppStreamingBody(task, mode))
				defer srv.Close()
				got, _ := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "m")
				if got.Caps.SpeechStreaming != "" {
					t.Errorf("SpeechStreaming = %q, want \"\" for a non-speech model", got.Caps.SpeechStreaming)
				}
			})
		}
	}
	t.Run("the live v0.9.1 tts entry, mode offline, is a no", func(t *testing.T) {
		srv := serveAudioCppModels(t, 200, audioCppLiveListing)
		defer srv.Close()
		got, _ := ProbeAudioCppVerdicts(context.Background(), srv.Client(), srv.URL, "pocket_tts_english_bf16")
		if got.Caps.SpeechStreaming != "no" {
			t.Errorf("SpeechStreaming = %q, want \"no\" (a tts model with mode offline)", got.Caps.SpeechStreaming)
		}
	})
}
