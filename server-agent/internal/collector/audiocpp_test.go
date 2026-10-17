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
// loaded and path. The task field is the capability signal.
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
	name              string
	body              string
	model             string
	wantSpeech        string // "yes" | "no" | ""
	wantTranscription string // "yes" | "no" | ""
}{
	{"real v0.9.1 entry, task tts", `{"object":"list","data":[{"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}]}`, "pocket_tts_english_bf16", "yes", "no"},
	{"task tts", `{"data":[{"id":"m","task":"tts"}]}`, "m", "yes", "no"},
	{"task clone also produces speech", `{"data":[{"id":"m","task":"clone"}]}`, "m", "yes", "no"},
	{"task asr transcribes and does not speak", `{"data":[{"id":"m","task":"asr"}]}`, "m", "no", "yes"},
	{"an asr model the server lists as not loaded is still a yes", `{"data":[{"id":"m","task":"asr","loaded":false}]}`, "m", "no", "yes"},
	{"a music task is a no on both axes", `{"data":[{"id":"m","task":"music"}]}`, "m", "no", "no"},
	{"an unknown task is a no on both axes", `{"data":[{"id":"m","task":"some-future-task"}]}`, "m", "no", "no"},
	{"model absent from the listing", `{"data":[{"id":"other","task":"tts"}]}`, "m", "", ""},
	{"empty data", `{"object":"list","data":[]}`, "m", "", ""},
	{"data absent", `{"object":"list"}`, "m", "", ""},
	{"data null", `{"data":null}`, "m", "", ""},
	{"the entry is picked out by id among several", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"},{"id":"c","task":"asr"}]}`, "b", "yes", "no"},
	{"the sibling's task does not leak", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"}]}`, "a", "no", "yes"},
	{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","task":"tts"}]}`, "pocket_tts", "", ""},
	{"id match is case-sensitive", `{"data":[{"id":"M","task":"tts"}]}`, "m", "", ""},
	{"a task padded with whitespace still maps", `{"data":[{"id":"m","task":" tts "}]}`, "m", "yes", "no"},
	{"task match ignores case", `{"data":[{"id":"m","task":"TTS"}]}`, "m", "yes", "no"},
	{"task match ignores case and padding for clone", `{"data":[{"id":"m","task":" Clone "}]}`, "m", "yes", "no"},
	{"a differently-cased asr task is still a transcription yes", `{"data":[{"id":"m","task":"ASR"}]}`, "m", "no", "yes"},
	{"an asr task padded with whitespace still maps", `{"data":[{"id":"m","task":" asr "}]}`, "m", "no", "yes"},
	{"a named entry without a task says nothing", `{"data":[{"id":"m"}]}`, "m", "", ""},
	{"a named entry with an empty task says nothing", `{"data":[{"id":"m","task":""}]}`, "m", "", ""},
	{"a named entry with a blank task says nothing", `{"data":[{"id":"m","task":"  "}]}`, "m", "", ""},
	{"an empty model id never matches", `{"data":[{"id":"","task":"tts"}]}`, "", "", ""},
	{"unknown and extra fields are ignored", `{"object":"list","x":1,"data":[{"id":"m","task":"tts","loaded":false,"extra":{"k":[1,2]}}]}`, "m", "yes", "no"},
	{"data of the wrong type", `{"data":"m"}`, "m", "", ""},
	{"a task of the wrong type", `{"data":[{"id":"m","task":1}]}`, "m", "", ""},
	{"not JSON", `<html>502</html>`, "m", "", ""},
	{"empty body", ``, "m", "", ""},
}

func TestParseAudioCppModelsTwinTable(t *testing.T) {
	for _, tc := range audioCppModelCases {
		t.Run(tc.name, func(t *testing.T) {
			speech, transcription := parseAudioCppModels([]byte(tc.body), tc.model)
			if speech != tc.wantSpeech || transcription != tc.wantTranscription {
				t.Fatalf("speech=%q transcription=%q, want speech=%q transcription=%q",
					speech, transcription, tc.wantSpeech, tc.wantTranscription)
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
			if got.Caps.Speech != tc.wantSpeech || got.Caps.Transcription != tc.wantTranscription {
				t.Fatalf("speech=%q transcription=%q, want speech=%q transcription=%q",
					got.Caps.Speech, got.Caps.Transcription, tc.wantSpeech, tc.wantTranscription)
			}
			assertAudioCppOnlyCaps(t, got)
		})
	}
}

// assertAudioCppOnlyCaps pins that the audio.cpp prober fills Speech and
// Transcription and nothing else: audio.cpp has no timings surface (no
// LiveProgress), no image options, and states nothing about the /props-derived
// capabilities.
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
		t.Fatalf("Caps = %+v, want only Speech/Transcription set", c)
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
