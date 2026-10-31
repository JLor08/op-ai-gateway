// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"os"
	"sync"
	"testing"
	"time"
)

// audioCppLiveListing is the shape audio.cpp v0.9.1 really answers on
// GET /v1/models, probed live: an OpenAI-style list whose entries add family,
// task, mode, loaded and path. It may also name an unloaded model, with
// loaded:false. The task field is the signal for the speech, transcription and
// speech_clone verdicts and the mode field is the signal for
// transcription_streaming (an asr model) and speech_streaming (a tts or clon
// model); loaded is the health axis the prober also reports
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
// The fifth, speech_streaming, is the speech twin of the third: it is read from the
// mode of a tts or clon entry only, and is no verdict for any other task.
func TestParseAudioCppModels(t *testing.T) {
	for _, tc := range audioCppModelCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioCppModels([]byte(tc.body), tc.model)
			want := AudioCppVerdicts{
				Speech: tc.wantSpeech, Transcription: tc.wantTranscription, TranscriptionStreaming: tc.wantTranscriptionStreaming,
				SpeechClone: tc.wantSpeechClone, SpeechStreaming: tc.wantSpeechStreaming,
			}
			if got != want {
				t.Fatalf("parseAudioCppModels = %+v, want %+v", got, want)
			}
		})
	}
}

// TestParseAudioCppModelsMode pins what the shared table cannot say in a row: how
// the mode axis and the task axes meet. Each streaming verdict is stated for ITS
// OWN kind of model ONLY -- transcription_streaming for an ASR model,
// speech_streaming for a speech model (tts or clon) -- because the capability is
// specific to what the model does: a model of any other task (music, no task) gets
// neither, and a speech model gets no transcription_streaming verdict nor a
// transcriber a speech_streaming one, whatever the mode, not even a "no". For a
// model of the right kind the entry's mode alone decides it ("streaming" is a yes,
// "offline" a no, anything else no answer). The mode never moves the speech,
// transcription or speech_clone verdict the task decides, so a listing whose mode
// field is odd, or an audio.cpp that adds modes, cannot switch a route off or turn
// a clone model into a plain tts one.
func TestParseAudioCppModelsMode(t *testing.T) {
	type taskCase struct {
		verdicts        [2]string // speech, transcription
		speechClone     string
		isASR, isSpeech bool
	}
	tasks := map[string]taskCase{
		"asr":   {[2]string{routing.CapabilityNo, routing.CapabilityYes}, "", true, false},
		"ASR":   {[2]string{routing.CapabilityNo, routing.CapabilityYes}, "", true, false},
		"tts":   {[2]string{routing.CapabilityYes, routing.CapabilityNo}, routing.CapabilityNo, false, true},
		"clon":  {[2]string{routing.CapabilityYes, routing.CapabilityNo}, routing.CapabilityYes, false, true},
		"music": {[2]string{routing.CapabilityNo, routing.CapabilityNo}, "", false, false},
		"":      {[2]string{"", ""}, "", false, false},
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
			wantTranscriptionStreaming, wantSpeechStreaming := "", ""
			if tc.isASR {
				wantTranscriptionStreaming = m.want
			}
			if tc.isSpeech {
				wantSpeechStreaming = m.want
			}
			got := parseAudioCppModels([]byte(body), "m")
			want := AudioCppVerdicts{
				Speech: tc.verdicts[0], Transcription: tc.verdicts[1], TranscriptionStreaming: wantTranscriptionStreaming,
				SpeechClone: tc.speechClone, SpeechStreaming: wantSpeechStreaming,
			}
			if got != want {
				t.Errorf("parseAudioCppModels(%s) = %+v, want %+v", body, got, want)
			}
		}
	}
}

// TestParseAudioCppModelsSpeechStreaming pins the speech_streaming axis on its own,
// the twin of the transcription_streaming rows: a tts or clon entry's mode alone
// states it ("streaming" a yes, "offline" a no, anything else no answer), an asr
// entry never carries it, and it moves neither the task's verdicts nor
// transcription_streaming. It is what the speech handler decides stream-versus-buffered on.
func TestParseAudioCppModelsSpeechStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, task, mode, wantSpeechStreaming string
	}{
		{"a tts entry in streaming mode is a yes", "tts", `"streaming"`, routing.CapabilityYes},
		{"a tts entry in offline mode is a no", "tts", `"offline"`, routing.CapabilityNo},
		{"a clon entry in streaming mode is a yes", "clon", `"streaming"`, routing.CapabilityYes},
		{"a clon entry in offline mode is a no", "clon", `"offline"`, routing.CapabilityNo},
		{"a tts entry with an unknown mode says nothing", "tts", `"realtime"`, ""},
		{"a tts entry with a null mode says nothing", "tts", `null`, ""},
		{"an asr entry in streaming mode carries no speech_streaming", "asr", `"streaming"`, ""},
		{"an asr entry in offline mode carries no speech_streaming", "asr", `"offline"`, ""},
		{"a music entry in streaming mode carries no speech_streaming", "music", `"streaming"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioCppModels([]byte(`{"data":[{"id":"m","task":"`+tc.task+`","mode":`+tc.mode+`}]}`), "m")
			if got.SpeechStreaming != tc.wantSpeechStreaming {
				t.Fatalf("SpeechStreaming = %q, want %q (verdicts %+v)", got.SpeechStreaming, tc.wantSpeechStreaming, got)
			}
			if tc.task != "asr" && got.TranscriptionStreaming != "" {
				t.Fatalf("TranscriptionStreaming = %q on a %s entry, want \"\": speech_streaming never borrows the transcriber's axis", got.TranscriptionStreaming, tc.task)
			}
		})
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

// TestParseAudioCppPath pins the identity axis the prober reports beside the verdicts:
// the model file the listing states for THIS id. The health loop compares it tick to
// tick to notice that the model behind one name changed, so it must be the entry's own
// string, verbatim, and everything the listing does not say (no entry, no field, a
// null, a field of the wrong type, a blank path, a body that is not the listing) is
// the empty string -- "no statement", never a guessed identity.
//
// Like the loaded flag it is a SEPARATE table from audioCppModelCases: it is not part
// of the capability rule the agent's copy shares.
func TestParseAudioCppPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		model string
		want  string
	}{
		{"the path of the named entry", `{"data":[{"id":"m","task":"tts","path":"/models/a.gguf"}]}`, "m", "/models/a.gguf"},
		{"the real v0.9.1 entry", audioCppLiveListing, "pocket_tts_english_bf16", "/models/pocket_tts_english_bf16.gguf"},
		{"an unloaded entry states its path too", `{"data":[{"id":"m","loaded":false,"path":"/models/a.gguf"}]}`, "m", "/models/a.gguf"},
		{"the entry is picked out by id", `{"data":[{"id":"a","path":"/a"},{"id":"b","path":"/b"},{"id":"c","path":"/c"}]}`, "b", "/b"},
		{"a sibling's path does not leak", `{"data":[{"id":"a","path":"/a"},{"id":"b"}]}`, "b", ""},
		{"the path is kept verbatim", `{"data":[{"id":"m","path":" /Models/A b.gguf "}]}`, "m", " /Models/A b.gguf "},
		{"path absent", `{"data":[{"id":"m","task":"tts"}]}`, "m", ""},
		{"path null", `{"data":[{"id":"m","path":null}]}`, "m", ""},
		{"path blank", `{"data":[{"id":"m","path":"   "}]}`, "m", ""},
		{"path as a number is not a path", `{"data":[{"id":"m","path":7}]}`, "m", ""},
		{"path as an object is not a path", `{"data":[{"id":"m","path":{"p":"/a"}}]}`, "m", ""},
		{"model absent from the listing", `{"data":[{"id":"other","path":"/o"}]}`, "m", ""},
		{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","path":"/p"}]}`, "pocket_tts", ""},
		{"an empty model id never matches", `{"data":[{"id":"","path":"/p"}]}`, "", ""},
		{"empty data", `{"data":[]}`, "m", ""},
		{"not JSON", `<html>502</html>`, "m", ""},
		{"empty body", ``, "m", ""},
		{"the first entry carrying the id answers", `{"data":[{"id":"m"},{"id":"m","path":"/second"}]}`, "m", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAudioCppPath([]byte(tc.body), tc.model); got != tc.want {
				t.Fatalf("parseAudioCppPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseAudioCppLanguageParam pins the language axis the prober reports beside the
// verdicts: whether the listing's entry for THIS id declares a request param named
// "language" (GET /v1/models?include_params=true adds each model's params array,
// objects of name, type, default and required), and that param's default. The
// speech-language feature acts on it, so a statement the listing does not make is "no
// language param, no default" -- never a guessed one: no entry, no params field, a
// params field of the wrong shape, a body that is not the listing.
//
// Like the loaded flag and the path it is a SEPARATE table from audioCppModelCases: it
// is not part of the capability rule the agent's copy shares.
func TestParseAudioCppLanguageParam(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		model       string
		wantAccepts bool
		wantDefault string
	}{
		{"a tts model's language param and its default", `{"data":[{"id":"m","task":"tts","params":[{"name":"text","type":"string","required":true},{"name":"language","type":"string","default":"zh","required":false}]}]}`, "m", true, "zh"},
		{"qwen3_tts defaults to auto", `{"data":[{"id":"qwen3_tts","task":"tts","params":[{"name":"language","type":"string","default":"auto","required":false}]}]}`, "qwen3_tts", true, "auto"},
		{"a language param with no default", `{"data":[{"id":"m","task":"tts","params":[{"name":"language","type":"string","required":false}]}]}`, "m", true, ""},
		{"a language param with a null default", `{"data":[{"id":"m","params":[{"name":"language","default":null}]}]}`, "m", true, ""},
		{"a language param with a non-string default keeps the param and loses the default", `{"data":[{"id":"m","params":[{"name":"language","default":7}]}]}`, "m", true, ""},
		{"a language param with an object default keeps the param and loses the default", `{"data":[{"id":"m","params":[{"name":"language","default":{"a":"b"}}]}]}`, "m", true, ""},
		{"params with no language entry", `{"data":[{"id":"m","task":"tts","params":[{"name":"text","type":"string"},{"name":"speed","type":"number","default":1}]}]}`, "m", false, ""},
		{"an empty params array", `{"data":[{"id":"m","task":"tts","params":[]}]}`, "m", false, ""},
		{"no params field at all", `{"data":[{"id":"m","task":"tts","mode":"offline","loaded":true}]}`, "m", false, ""},
		{"params null", `{"data":[{"id":"m","params":null}]}`, "m", false, ""},
		{"params as a string is not a params array", `{"data":[{"id":"m","params":"language"}]}`, "m", false, ""},
		{"params as an object is not a params array", `{"data":[{"id":"m","params":{"name":"language","default":"zh"}}]}`, "m", false, ""},
		{"a param that is not an object is skipped, the language entry after it still counts", `{"data":[{"id":"m","params":["language",{"name":"language","default":"de"}]}]}`, "m", true, "de"},
		{"a param whose name is not a string is skipped", `{"data":[{"id":"m","params":[{"name":7,"default":"x"},{"name":"language","default":"de"}]}]}`, "m", true, "de"},
		{"the name matches ignoring case and surrounding whitespace", `{"data":[{"id":"m","params":[{"name":" Language ","default":"fr"}]}]}`, "m", true, "fr"},
		{"the name match is not a prefix or a substring", `{"data":[{"id":"m","params":[{"name":"language_code","default":"x"},{"name":"lang","default":"y"}]}]}`, "m", false, ""},
		{"the first language param answers", `{"data":[{"id":"m","params":[{"name":"language","default":"first"},{"name":"language","default":"second"}]}]}`, "m", true, "first"},
		{"a blank default is no default, like a blank path", `{"data":[{"id":"m","params":[{"name":"language","default":"  "}]}]}`, "m", true, ""},
		{"the default is kept verbatim", `{"data":[{"id":"m","params":[{"name":"language","default":" Zh-CN "}]}]}`, "m", true, " Zh-CN "},
		{"the entry is picked out by id", `{"data":[{"id":"a","params":[{"name":"language","default":"a"}]},{"id":"b","params":[{"name":"language","default":"b"}]}]}`, "b", true, "b"},
		{"a sibling's language param does not leak", `{"data":[{"id":"a","params":[{"name":"language","default":"a"}]},{"id":"b","params":[{"name":"text"}]}]}`, "b", false, ""},
		{"model absent from the listing", `{"data":[{"id":"other","params":[{"name":"language","default":"zh"}]}]}`, "m", false, ""},
		{"id match is exact, not a prefix", `{"data":[{"id":"qwen3_tts","params":[{"name":"language","default":"auto"}]}]}`, "qwen3", false, ""},
		{"id match is case-sensitive", `{"data":[{"id":"M","params":[{"name":"language","default":"zh"}]}]}`, "m", false, ""},
		{"an empty model id never matches", `{"data":[{"id":"","params":[{"name":"language","default":"zh"}]}]}`, "", false, ""},
		{"the first entry carrying the id answers", `{"data":[{"id":"m"},{"id":"m","params":[{"name":"language","default":"zh"}]}]}`, "m", false, ""},
		{"empty data", `{"data":[]}`, "m", false, ""},
		{"data null", `{"data":null}`, "m", false, ""},
		{"data of the wrong type", `{"data":"m"}`, "m", false, ""},
		{"not JSON", `<html>502</html>`, "m", false, ""},
		{"empty body", ``, "m", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepts, def := parseAudioCppLanguageParam([]byte(tc.body), tc.model)
			if accepts != tc.wantAccepts || def != tc.wantDefault {
				t.Fatalf("parseAudioCppLanguageParam = (%v, %q), want (%v, %q)", accepts, def, tc.wantAccepts, tc.wantDefault)
			}
		})
	}
}

// TestProbeAudioCppModelsLanguageParam: the prober asks for the listing WITH its params
// (include_params=true) and reports the language axis beside the five verdicts, the
// loaded flag and the path, which the new query must leave exactly as they were. The
// handler answers params ONLY when the query reaches it, so the case also proves the
// query survives to the wire.
func TestProbeAudioCppModelsLanguageParam(t *testing.T) {
	const withParams = `{"data":[{"id":"qwen3_tts","family":"qwen3_tts","task":"tts","mode":"streaming","loaded":true,"path":"/models/qwen3_tts.gguf","params":[{"name":"text","type":"string","required":true},{"name":"language","type":"string","default":"auto","required":false}]}]}`
	const withoutParams = `{"data":[{"id":"qwen3_tts","family":"qwen3_tts","task":"tts","mode":"streaming","loaded":true,"path":"/models/qwen3_tts.gguf"}]}`
	var method, path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		if r.URL.Query().Get("include_params") == "true" {
			_, _ = w.Write([]byte(withParams))
			return
		}
		_, _ = w.Write([]byte(withoutParams))
	}))
	defer srv.Close()
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: srv.URL}, "qwen3_tts")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	want := AudioCppVerdicts{
		Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes,
		Loaded: true, Path: "/models/qwen3_tts.gguf", AcceptsLanguage: true, LanguageDefault: "auto",
	}
	if got != want {
		t.Fatalf("got %+v, want %+v (the language axis is read from the params; the five verdicts, loaded and path are unchanged by the new query)", got, want)
	}
	if method != http.MethodGet || path != "/v1/models" || query != "include_params=true" {
		t.Fatalf("request = %s %s?%s, want GET /v1/models?include_params=true", method, path, query)
	}
}

// TestProbeAudioCppModelsOddParamsCostsOnlyTheLanguageAxis: a params field of a shape
// this code does not expect must cost only the language axis, never the verdicts the
// task decides, the loaded flag or the path (the mode and path precedent), because a
// capability "yes" must not hang on an unrelated param description.
func TestProbeAudioCppModelsOddParamsCostsOnlyTheLanguageAxis(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","task":"tts","mode":"streaming","loaded":true,"path":"/models/m.gguf","params":"language"}]}`))
	}))
	defer srv.Close()
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes, Loaded: true, Path: "/models/m.gguf"}
	if got != want {
		t.Fatalf("got %+v, want %+v (an odd params shape must not cost the verdicts, the loaded flag or the path)", got, want)
	}
}

// TestProbeAudioCppModelsOddPathCostsOnlyPath: a path of a type this code does not
// expect must cost only Path, never the verdicts the task decides or the loaded flag.
func TestProbeAudioCppModelsOddPathCostsOnlyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","task":"tts","mode":"streaming","loaded":true,"path":["/a"]}]}`))
	}))
	defer srv.Close()
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes, Loaded: true}
	if got != want {
		t.Fatalf("got %+v, want %+v (an odd path type must not cost the verdicts or the loaded flag)", got, want)
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

// audioCppSeenRequest is one request a test server received: its method, path and RAW
// query, so a case can tell GET /v1/models?include_params=true from the bare GET
// /v1/models and not merely count requests.
type audioCppSeenRequest struct {
	method, path, query string
}

// audioCppDeadlineRecorder is a RoundTripper that notes each request's context
// deadline before handing it to the real transport, so a test can tell whether two
// requests were issued under ONE timeout context.
type audioCppDeadlineRecorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	log  []audioCppDeadline
}

type audioCppDeadline struct {
	at time.Time
	ok bool
}

func (d *audioCppDeadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	at, ok := r.Context().Deadline()
	d.mu.Lock()
	d.log = append(d.log, audioCppDeadline{at, ok})
	d.mu.Unlock()
	return d.base.RoundTrip(r)
}

func (d *audioCppDeadlineRecorder) seen() []audioCppDeadline {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]audioCppDeadline(nil), d.log...)
}

func TestProbeAudioCppModels(t *testing.T) {
	probe := func(t *testing.T, srv *httptest.Server, ctx context.Context, modelID string) (AudioCppVerdicts, error) {
		t.Helper()
		return NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(ctx, routing.Target{Endpoint: srv.URL}, modelID)
	}

	t.Run("a tts model yields yes from GET /v1/models", func(t *testing.T) {
		var method, path, query string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if want := (AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityNo, Loaded: true, Path: "/models/pocket_tts_english_bf16.gguf"}); got != want {
			t.Fatalf("got %+v, want %+v (the live entry is a loaded tts model: speech_clone no, mode offline so speech_streaming no, but no transcription streaming verdict; its path is the identity axis)", got, want)
		}
		if method != http.MethodGet || path != "/v1/models" || query != "include_params=true" {
			t.Fatalf("request = %s %s?%s, want GET /v1/models?include_params=true (the params carry the language axis)", method, path, query)
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
		want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes, SpeechStreaming: routing.CapabilityNo, Loaded: true}
		if got != want {
			t.Fatalf("got %+v, want %+v (a clone model is still a speech yes: speech_clone refines speech, it never replaces it)", got, want)
		}
	})

	t.Run("a streaming tts model yields a speech streaming yes from the same listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"vibevoice_tts_streaming","family":"vibevoice","task":"tts","mode":"streaming","loaded":true}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "vibevoice_tts_streaming")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes, Loaded: true}
		if got != want {
			t.Fatalf("got %+v, want %+v (speech_streaming is the speech twin of transcription_streaming, read from the same mode field)", got, want)
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

	// listingServer answers the include_params request with paramStatus and the bare
	// /v1/models request with bareStatus (a 200 carries the live listing), and records
	// every request it receives, so a case can assert WHICH requests were made and not
	// just how many. The returned func reads the recording under the handler's lock.
	listingServer := func(t *testing.T, paramStatus, bareStatus int) (*httptest.Server, func() []audioCppSeenRequest) {
		t.Helper()
		var mu sync.Mutex
		var seen []audioCppSeenRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, audioCppSeenRequest{r.Method, r.URL.Path, r.URL.RawQuery})
			mu.Unlock()
			status, body := bareStatus, audioCppLiveListing
			if r.URL.Query().Get("include_params") == "true" {
				status = paramStatus
			}
			if status < 200 || status >= 300 {
				body = `{"error":"rejected"}`
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv, func() []audioCppSeenRequest {
			mu.Lock()
			defer mu.Unlock()
			return append([]audioCppSeenRequest(nil), seen...)
		}
	}

	// A conclusive rejection of the include_params QUERY (400/404/422) is retried once
	// against the bare listing: the verdicts, loaded and path come from it, and only
	// the language axis is lost (it stays at its zero value, i.e. unknown), instead of
	// the whole probe failing for an audio.cpp that does not take the query.
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprintf("a %d on include_params falls back to the bare listing and keeps the verdicts, loaded and path", status), func(t *testing.T) {
			srv, requests := listingServer(t, status, http.StatusOK)
			got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
			if err != nil {
				t.Fatalf("error = %v, want the bare listing's answer", err)
			}
			want := AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityNo, Loaded: true, Path: "/models/pocket_tts_english_bf16.gguf"}
			if got != want {
				t.Fatalf("got %+v, want %+v (the verdicts, loaded and path survive the fallback; AcceptsLanguage is false and LanguageDefault empty: the bare listing carries no params)", got, want)
			}
			wantRequests := []audioCppSeenRequest{
				{http.MethodGet, "/v1/models", "include_params=true"},
				{http.MethodGet, "/v1/models", ""},
			}
			if seen := requests(); fmt.Sprint(seen) != fmt.Sprint(wantRequests) {
				t.Fatalf("requests = %+v, want %+v (the retry is the bare path with an EMPTY query)", seen, wantRequests)
			}
		})
	}

	// Every other non-2xx is not a verdict on the query: a credential refusal, a rate
	// limit or a failing server would fail the bare path just the same, so it is an
	// ErrUnavailable straight away and NO second request is made.
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrAuthRejected},
		{http.StatusForbidden, ErrAuthRejected},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusBadGateway, ErrUnavailable},
		{http.StatusServiceUnavailable, ErrUpstreamStarting},
	} {
		t.Run(fmt.Sprintf("a %d on include_params is unavailable and is not retried", tc.status), func(t *testing.T) {
			srv, requests := listingServer(t, tc.status, http.StatusOK)
			got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want ErrUnavailable (tagged %v)", err, tc.want)
			}
			if got != (AudioCppVerdicts{}) {
				t.Fatalf("got %+v, want no verdict", got)
			}
			wantRequests := []audioCppSeenRequest{{http.MethodGet, "/v1/models", "include_params=true"}}
			if seen := requests(); fmt.Sprint(seen) != fmt.Sprint(wantRequests) {
				t.Fatalf("requests = %+v, want only %+v (the bare path must never be hit for a %d)", seen, wantRequests, tc.status)
			}
		})
	}

	// The fallback is ONE retry: whatever the bare listing answers that is not a 2xx is
	// an ErrUnavailable, and a second rejection is not retried again.
	for _, tc := range []struct {
		name       string
		bareStatus int
		want       error
	}{
		{"a 500", http.StatusInternalServerError, ErrUnavailable},
		{"a 503", http.StatusServiceUnavailable, ErrUpstreamStarting},
		{"another 400", http.StatusBadRequest, ErrUnavailable},
	} {
		t.Run(fmt.Sprintf("a 400 on include_params and %s on the bare retry is unavailable", tc.name), func(t *testing.T) {
			srv, requests := listingServer(t, http.StatusBadRequest, tc.bareStatus)
			got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want ErrUnavailable (tagged %v)", err, tc.want)
			}
			if got != (AudioCppVerdicts{}) {
				t.Fatalf("got %+v, want no verdict", got)
			}
			if seen := requests(); len(seen) != 2 {
				t.Fatalf("requests = %+v, want exactly the include_params request and ONE bare retry", seen)
			}
		})
	}

	t.Run("a transport failure on the bare retry is unavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("include_params") == "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Drop the connection instead of answering the retry.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict", got)
		}
	})

	// The include_params request and its bare retry are bounded by ONE target.Timeout:
	// the retry does not start a fresh clock, so a probe never runs longer than the
	// timeout the caller set. Both requests must therefore carry the same deadline.
	t.Run("the include_params request and the bare retry share one timeout deadline", func(t *testing.T) {
		srv, _ := listingServer(t, http.StatusBadRequest, http.StatusOK)
		rec := &audioCppDeadlineRecorder{base: srv.Client().Transport}
		client := NewOpenAICompatibleClient(&http.Client{Transport: rec})
		if _, err := client.ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: srv.URL, Timeout: time.Minute}, "pocket_tts_english_bf16"); err != nil {
			t.Fatalf("error = %v", err)
		}
		deadlines := rec.seen()
		if len(deadlines) != 2 {
			t.Fatalf("recorded %d requests, want 2 (include_params, then the bare retry)", len(deadlines))
		}
		if !deadlines[0].ok || !deadlines[1].ok {
			t.Fatalf("deadlines = %+v, want a deadline on both requests (target.Timeout is set)", deadlines)
		}
		if !deadlines[0].at.Equal(deadlines[1].at) {
			t.Fatalf("deadlines = %v and %v, want ONE shared timeout context for both requests", deadlines[0].at, deadlines[1].at)
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
	var paths, queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.RawQuery)
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
	if want := (AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityNo, Loaded: true, Path: "/models/pocket_tts_english_bf16.gguf"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(paths) != 1 || paths[0] != "/v1/models" || queries[0] != "include_params=true" {
		t.Fatalf("upstream paths = %v, queries = %v, want exactly [/v1/models] with include_params=true", paths, queries)
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
