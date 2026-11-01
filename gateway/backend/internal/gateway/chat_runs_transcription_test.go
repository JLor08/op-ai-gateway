// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"slices"
	"strings"
	"testing"
	"time"
)

// audioDataURL is the data: URL of a clip as the chat store hands the executor a
// resolved reference (portal.Service.PrepareChatRun, resolveTranscriptionRunSource).
func audioDataURL(mediaType string, audio []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(audio)
}

// audioUserContent is the content of a user message carrying one clip as an
// audio_url part, with an optional text part ahead of it (a caption is not the
// clip and must not be mistaken for it).
func audioUserContent(t *testing.T, caption, url string) json.RawMessage {
	t.Helper()
	parts := []any{}
	if caption != "" {
		parts = append(parts, map[string]any{"type": "text", "text": caption})
	}
	parts = append(parts, map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": url}})
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// transcriptionRunPrep is the prepared run every transcription test below starts
// from: the pinned transcription kind and a history whose last user message
// carries the clip to transcribe.
func transcriptionRunPrep(t *testing.T, mediaType string, audio []byte) PrepareRunResult {
	t.Helper()
	return PrepareRunResult{
		History: []portal.ChatAPIMessage{
			{Role: "user", Content: audioUserContent(t, "", audioDataURL(mediaType, audio))},
		},
		Settings: portal.ChatRunSettings{Model: transcriptionGatewayModel, Kind: chatRunKindTranscription},
	}
}

// formPart is one part of a decoded multipart body: its form name, the file name
// it carries (empty for a plain field), its Content-Type header and its bytes.
type formPart struct {
	name        string
	filename    string
	contentType string
	data        []byte
}

// decodeTranscriptionForm reads a recorded body back with the boundary of its own
// Content-Type, in the order the parts were written, failing when it is not a
// well-formed multipart/form-data body.
func decodeTranscriptionForm(t *testing.T, contentType string, raw []byte) []formPart {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("Content-Type %q is not multipart/form-data with a boundary (%v)", contentType, err)
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var parts []formPart
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return parts
		}
		if err != nil {
			t.Fatalf("the body is not well-formed multipart: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("reading part %q: %v", part.FormName(), err)
		}
		parts = append(parts, formPart{name: part.FormName(), filename: part.FileName(), contentType: part.Header.Get("Content-Type"), data: data})
	}
}

// formFields returns the plain (non-file) parts as name -> value, failing when a
// name occurs twice: every field the run owns is single-use (the endpoint refuses
// a repeat, parseTranscriptionForm).
func formFields(t *testing.T, parts []formPart) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, p := range parts {
		if p.filename != "" {
			continue
		}
		if _, dup := fields[p.name]; dup {
			t.Fatalf("form field %q appears twice", p.name)
		}
		fields[p.name] = string(p.data)
	}
	return fields
}

// onlyFile returns the single file part, failing when there is not exactly one,
// or when it is not the `file` field.
func onlyFile(t *testing.T, parts []formPart) formPart {
	t.Helper()
	var files []formPart
	for _, p := range parts {
		if p.filename != "" {
			files = append(files, p)
		}
	}
	if len(files) != 1 {
		t.Fatalf("want exactly one file part, got %d", len(files))
	}
	if files[0].name != "file" {
		t.Fatalf("the clip is the %q field, want file", files[0].name)
	}
	return files[0]
}

// buildTranscriptionForm runs buildTranscriptionBody and decodes what it built.
func buildTranscriptionForm(t *testing.T, prep PrepareRunResult, mediaType string, audio []byte) []formPart {
	t.Helper()
	raw, contentType, err := buildTranscriptionBody(prep, mediaType, audio)
	if err != nil {
		t.Fatalf("buildTranscriptionBody: %v", err)
	}
	return decodeTranscriptionForm(t, contentType, raw)
}

func fieldNames(fields map[string]string) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// --- buildTranscriptionBody ------------------------------------------------

// With no transcription settings the form is exactly the model, the run's own
// stream=true and the clip: a thread that set no knob sends nothing extra, so
// audio.cpp stays on its own defaults (language detection, no prompt, its own
// temperature). response_format is deliberately absent too -- the endpoint answers
// JSON when it is absent and refuses every other value (validateTranscriptionRequest).
func TestBuildTranscriptionBodyWithoutSettingsIsModelAndFile(t *testing.T) {
	audio := speechTestWAV()
	parts := buildTranscriptionForm(t, transcriptionRunPrep(t, "audio/wav", audio), "audio/wav", audio)

	fields := formFields(t, parts)
	if want := map[string]string{"model": transcriptionGatewayModel, "stream": "true"}; !equalStringMaps(fields, want) {
		t.Fatalf("want exactly the model and stream fields %v, got %v", want, fields)
	}
	file := onlyFile(t, parts)
	if file.filename != "audio.wav" || file.contentType != "audio/wav" || !bytes.Equal(file.data, audio) {
		t.Fatalf("file part = %q (%s, %d bytes), want audio.wav / audio/wav / the clip's own %d bytes", file.filename, file.contentType, len(file.data), len(audio))
	}
	if len(parts) != 3 {
		t.Fatalf("want exactly three parts (model, stream, file), got %d", len(parts))
	}
}

// An empty settings block is the same as none: nothing set, nothing sent.
func TestBuildTranscriptionBodyWithEmptySettingsIsModelAndFile(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{}
	parts := buildTranscriptionForm(t, prep, "audio/wav", audio)
	if got := fieldNames(formFields(t, parts)); !slices.Equal(got, []string{"model", "stream"}) {
		t.Fatalf("an empty transcription block must add nothing, got fields %v", got)
	}
}

// The set first-class knobs are forwarded as form fields; an unset one is omitted
// (audio.cpp keeps its default) rather than sent as an inert zero or empty string.
func TestBuildTranscriptionBodyForwardsOnlyTheSetSettings(t *testing.T) {
	audio := speechTestWAV()
	cases := []struct {
		name     string
		settings portal.ChatTranscriptionSettings
		want     map[string]string
	}{
		{
			name:     "all three",
			settings: portal.ChatTranscriptionSettings{Language: "de", Prompt: "Namen: Zoe, Jörg", Temperature: ptr(0.25)},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "language": "de", "prompt": "Namen: Zoe, Jörg", "temperature": "0.25"},
		},
		{
			name:     "only the language",
			settings: portal.ChatTranscriptionSettings{Language: "de"},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "language": "de"},
		},
		{
			name:     "only the prompt",
			settings: portal.ChatTranscriptionSettings{Prompt: "names"},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "prompt": "names"},
		},
		{
			// A pointer exists for exactly this: an explicit 0 is a real, deterministic
			// setting, so it is forwarded, and the empty language/prompt are not.
			name:     "an explicit zero temperature",
			settings: portal.ChatTranscriptionSettings{Temperature: ptr(0.0)},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "temperature": "0"},
		},
		{
			// The shortest decimal form, never an exponent a form reader may not take.
			name:     "a small temperature stays a plain decimal",
			settings: portal.ChatTranscriptionSettings{Temperature: ptr(0.00001)},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "temperature": "0.00001"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prep := transcriptionRunPrep(t, "audio/wav", audio)
			settings := tc.settings
			prep.Settings.Transcription = &settings
			fields := formFields(t, buildTranscriptionForm(t, prep, "audio/wav", audio))
			if !equalStringMaps(fields, tc.want) {
				t.Fatalf("fields = %v\nwant     %v", fields, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// The clip is named after its media type -- the upstream reads a part's file name
// and a server that sniffs by name falls back on its extension -- and its Content-Type
// is the media type itself. The table is exactly the audio allow-list
// portal.DecodeAudioDataURL enforces.
func TestBuildTranscriptionBodyNamesTheFileByMediaType(t *testing.T) {
	cases := map[string]string{
		"audio/wav":  "audio.wav",
		"audio/webm": "audio.webm",
		"audio/mpeg": "audio.mp3",
		"audio/mp4":  "audio.m4a",
		"audio/ogg":  "audio.ogg",
	}
	audio := []byte("clip-bytes")
	for mediaType, filename := range cases {
		t.Run(mediaType, func(t *testing.T) {
			file := onlyFile(t, buildTranscriptionForm(t, transcriptionRunPrep(t, mediaType, audio), mediaType, audio))
			if file.filename != filename || file.contentType != mediaType {
				t.Fatalf("file part = %q (%s), want %q (%s)", file.filename, file.contentType, filename, mediaType)
			}
		})
	}
	// The map and the allow-list cannot drift: every type the store accepts as audio
	// has a name, so a stored clip is never refused here for want of one.
	for mediaType := range transcriptionFilenames {
		if _, ok := cases[mediaType]; !ok {
			t.Errorf("transcriptionFilenames names %q, which the table above does not expect", mediaType)
		}
	}
	if len(transcriptionFilenames) != len(cases) {
		t.Errorf("transcriptionFilenames has %d entries, want %d", len(transcriptionFilenames), len(cases))
	}
}

// A media type with no file name is refused rather than sent under a guessed one.
func TestBuildTranscriptionBodyRefusesAMediaTypeItCannotName(t *testing.T) {
	audio := []byte("clip-bytes")
	_, _, err := buildTranscriptionBody(transcriptionRunPrep(t, "audio/flac", audio), "audio/flac", audio)
	if err == nil {
		t.Fatal("a media type with no file name must be an error")
	}
	if errors.Is(err, errTranscriptionPassthroughReserved) {
		t.Fatalf("an unnameable media type is not a reserved field: %v", err)
	}
}

// Passthrough values are raw JSON and a form field is text, so each is written as
// its natural text: a JSON string as the string itself, a number or a bool as its
// literal, and an object or array as its compact JSON. They are merged in sorted
// order with the first-class fields, ahead of the clip, so the body is
// deterministic.
func TestBuildTranscriptionBodyMergesThePassthrough(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Language: "de",
		Passthrough: map[string]json.RawMessage{
			"a_string":              json.RawMessage(`"plain text"`),
			"a_number":              json.RawMessage(`1.50`),
			"a_bool":                json.RawMessage(`true`),
			"an_object":             json.RawMessage(`{ "k" : [1, 2] }`),
			"timestamp_granularity": json.RawMessage(`["word", "segment"]`),
			"escaped":               json.RawMessage(`"say \"hi\"\nthere"`),
		},
	}
	parts := buildTranscriptionForm(t, prep, "audio/wav", audio)

	want := map[string]string{
		"model":                 transcriptionGatewayModel,
		"stream":                "true",
		"language":              "de",
		"a_string":              "plain text",
		"a_number":              "1.50",
		"a_bool":                "true",
		"an_object":             `{"k":[1,2]}`,
		"timestamp_granularity": `["word","segment"]`,
		"escaped":               "say \"hi\"\nthere",
	}
	if got := formFields(t, parts); !equalStringMaps(got, want) {
		t.Fatalf("fields = %v\nwant     %v", got, want)
	}

	// Sorted, and the clip last.
	var names []string
	for _, p := range parts[:len(parts)-1] {
		names = append(names, p.name)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("form fields are not in sorted order: %v", names)
	}
	if last := parts[len(parts)-1]; last.name != "file" {
		t.Fatalf("the last part is %q, want the file", last.name)
	}
}

// A passthrough key the run owns -- the model, the clip, the response format its
// answer is read in and the stream request it states itself -- is REJECTED, ignoring
// case, and no body is returned. stream_format is deliberately not reserved: the
// endpoint refuses it with its own code, which the run then surfaces.
func TestBuildTranscriptionBodyRejectsAReservedPassthroughKey(t *testing.T) {
	audio := speechTestWAV()
	for _, key := range []string{"model", "file", "response_format", "stream", "Model", "FILE", "Response_Format", "STREAM"} {
		t.Run(key, func(t *testing.T) {
			prep := transcriptionRunPrep(t, "audio/wav", audio)
			prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
				Language:    "de",
				Passthrough: map[string]json.RawMessage{"harmless": json.RawMessage(`"x"`), key: json.RawMessage(`"x"`)},
			}
			body, contentType, err := buildTranscriptionBody(prep, "audio/wav", audio)
			if !errors.Is(err, errTranscriptionPassthroughReserved) {
				t.Fatalf("err = %v, want errTranscriptionPassthroughReserved", err)
			}
			if body != nil || contentType != "" {
				t.Fatalf("a rejected body must not be returned, got %d bytes under %q", len(body), contentType)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("the error %q should name the offending key %q", err, key)
			}
		})
	}

	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Passthrough: map[string]json.RawMessage{"stream_format": json.RawMessage(`"sse"`)},
	}
	if got := formFields(t, buildTranscriptionForm(t, prep, "audio/wav", audio)); got["stream_format"] != "sse" {
		t.Fatalf("stream_format must reach the endpoint to be refused by it, got fields %v", got)
	}
}

// A passthrough key that repeats a first-class setting which IS set is not sent a
// second time (the endpoint refuses a repeated field, and the first-class control is
// the visible one, so it wins); with it unset the passthrough key is the only
// statement of it and goes through. Both ignore case.
func TestBuildTranscriptionBodyFirstClassSettingWinsOverAPassthroughTwin(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Language:    "de",
		Temperature: ptr(0.0),
		Passthrough: map[string]json.RawMessage{
			"Language":    json.RawMessage(`"fr"`),
			"temperature": json.RawMessage(`0.9`),
			"prompt":      json.RawMessage(`"from the passthrough"`),
		},
	}
	fields := formFields(t, buildTranscriptionForm(t, prep, "audio/wav", audio))
	want := map[string]string{
		"model":       transcriptionGatewayModel,
		"stream":      "true",
		"language":    "de",
		"temperature": "0",
		"prompt":      "from the passthrough",
	}
	if !equalStringMaps(fields, want) {
		t.Fatalf("fields = %v\nwant     %v", fields, want)
	}
}

// A passthrough key that cannot be a form field name, or a value that is not JSON,
// fails the build with an error of its own (never the reserved-field one, and never
// a part header assembled from a key that holds a line break).
func TestBuildTranscriptionBodyRefusesAnUnusablePassthroughEntry(t *testing.T) {
	audio := speechTestWAV()
	cases := map[string]map[string]json.RawMessage{
		"an empty key":            {"": json.RawMessage(`"x"`)},
		"a key with a CRLF":       {"x\r\nContent-Type: text/html": json.RawMessage(`"x"`)},
		"a key with a NUL":        {"x\x00y": json.RawMessage(`"x"`)},
		"a value that is no JSON": {"extra": json.RawMessage(`{`)},
	}
	for name, passthrough := range cases {
		t.Run(name, func(t *testing.T) {
			prep := transcriptionRunPrep(t, "audio/wav", audio)
			prep.Settings.Transcription = &portal.ChatTranscriptionSettings{Passthrough: passthrough}
			body, _, err := buildTranscriptionBody(prep, "audio/wav", audio)
			if err == nil {
				t.Fatalf("want an error, got a %d byte body", len(body))
			}
			if errors.Is(err, errTranscriptionPassthroughReserved) {
				t.Fatalf("not a reserved field: %v", err)
			}
		})
	}
}

// What the run builds is what the gateway's own endpoint reads: the form parses
// under parseTranscriptionForm (one model, the set language/prompt, ONE non-empty
// file part, no repeat) and passes validateTranscriptionRequest.
func TestBuildTranscriptionBodyIsAcceptedByTheEndpointsOwnParser(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Language: "de", Prompt: "names", Temperature: ptr(0.2),
		Passthrough: map[string]json.RawMessage{"timestamp_granularity": json.RawMessage(`"word"`)},
	}
	raw, contentType, err := buildTranscriptionBody(prep, "audio/wav", audio)
	if err != nil {
		t.Fatal(err)
	}
	form, err := parseTranscriptionForm(contentType, raw)
	if err != nil {
		t.Fatalf("parseTranscriptionForm: %v", err)
	}
	if form.model != transcriptionGatewayModel || form.language != "de" || form.prompt != "names" || form.files != 1 || form.responseFormat != "" || form.stream != "true" {
		t.Fatalf("parsed form = %+v", form)
	}
	if verr := validateTranscriptionRequest(form); verr != nil {
		t.Fatalf("validateTranscriptionRequest: %v", verr)
	}
}

// A diarized run's form states NO stream -- the details endpoint is one-shot and
// refuses a request to stream (validateTranscriptionDetailsRequest) -- and no diarize
// field either: Diarize picks the endpoint, it is not a form field. Everything else is
// the plain run's: the model, the set settings and the passthrough, with the clip.
func TestBuildTranscriptionBodyOmitsStreamOnlyForADiarizedRun(t *testing.T) {
	audio := speechTestWAV()
	passthrough := map[string]json.RawMessage{"timestamp_granularity": json.RawMessage(`"word"`)}
	cases := []struct {
		name     string
		settings *portal.ChatTranscriptionSettings
		want     map[string]string
	}{
		{
			name:     "diarize on and nothing else set",
			settings: &portal.ChatTranscriptionSettings{Diarize: true},
			want:     map[string]string{"model": transcriptionGatewayModel},
		},
		{
			name:     "diarize on keeps the first-class settings",
			settings: &portal.ChatTranscriptionSettings{Diarize: true, Language: "de", Prompt: "names", Temperature: ptr(0.2)},
			want:     map[string]string{"model": transcriptionGatewayModel, "language": "de", "prompt": "names", "temperature": "0.2"},
		},
		{
			name:     "diarize on keeps the passthrough",
			settings: &portal.ChatTranscriptionSettings{Diarize: true, Passthrough: passthrough},
			want:     map[string]string{"model": transcriptionGatewayModel, "timestamp_granularity": "word"},
		},
		{
			name:     "an explicit diarize off still asks for a stream",
			settings: &portal.ChatTranscriptionSettings{Diarize: false, Language: "de"},
			want:     map[string]string{"model": transcriptionGatewayModel, "stream": "true", "language": "de"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prep := transcriptionRunPrep(t, "audio/wav", audio)
			prep.Settings.Transcription = tc.settings
			parts := buildTranscriptionForm(t, prep, "audio/wav", audio)
			if fields := formFields(t, parts); !equalStringMaps(fields, tc.want) {
				t.Fatalf("fields = %v\nwant     %v", fields, tc.want)
			}
			if file := onlyFile(t, parts); file.filename != "audio.wav" || !bytes.Equal(file.data, audio) {
				t.Fatalf("file part = %q (%d bytes), want audio.wav and the clip's own %d bytes", file.filename, len(file.data), len(audio))
			}
		})
	}
}

// The reserved fields stay the run's on a diarized run, stream included: the run states
// none there, but a thread that names one is still refused rather than forwarded (the
// details endpoint would refuse a stream of its own, and a passthrough must never
// override what the run owns).
func TestBuildTranscriptionBodyStillRejectsAReservedPassthroughKeyWhenDiarized(t *testing.T) {
	audio := speechTestWAV()
	for _, key := range []string{"model", "file", "response_format", "stream"} {
		t.Run(key, func(t *testing.T) {
			prep := transcriptionRunPrep(t, "audio/wav", audio)
			prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
				Diarize:     true,
				Passthrough: map[string]json.RawMessage{key: json.RawMessage(`"x"`)},
			}
			if _, _, err := buildTranscriptionBody(prep, "audio/wav", audio); !errors.Is(err, errTranscriptionPassthroughReserved) {
				t.Fatalf("error = %v, want errTranscriptionPassthroughReserved", err)
			}
		})
	}
}

// What a diarized run builds is what the DETAILS endpoint reads: the form parses, passes
// validateTranscriptionDetailsRequest -- which refuses any stream that reads as true --
// and carries no stream at all.
func TestBuildTranscriptionBodyOfADiarizedRunIsAcceptedByTheDetailsParser(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{Diarize: true, Language: "de", Prompt: "names"}
	raw, contentType, err := buildTranscriptionBody(prep, "audio/wav", audio)
	if err != nil {
		t.Fatal(err)
	}
	form, err := parseTranscriptionForm(contentType, raw)
	if err != nil {
		t.Fatalf("parseTranscriptionForm: %v", err)
	}
	if form.model != transcriptionGatewayModel || form.language != "de" || form.prompt != "names" || form.files != 1 || form.responseFormat != "" || form.stream != "" {
		t.Fatalf("parsed form = %+v", form)
	}
	if verr := validateTranscriptionDetailsRequest(form); verr != nil {
		t.Fatalf("validateTranscriptionDetailsRequest: %v", verr)
	}
}

// --- the loopback hop ------------------------------------------------------
//
// The recording stand-in for the gateway's own endpoint, the full run wiring and
// the outcome helpers are the speech run's (chat_runs_audio_test.go): none of them
// depends on the kind, only on the one loopback call a buffered run makes.

// transcriptionRunOKBody is the answer the recording stand-in gives: the document
// audio.cpp answers a transcription with. (The pinned model of the prepared runs
// is transcriptionGatewayModel of the handler tests; the stand-in never routes, so
// the name is only ever asserted on the wire.)
const transcriptionRunOKBody = `{"text":"hello","timing":{"audio_duration_ms":1000,"total_ms":420}}`

// A transcription run makes exactly ONE POST to the gateway's own
// /v1/audio/transcriptions as multipart/form-data, carries the shared loopback
// header set, and sends the form -- the model, the clip of the LAST user message
// byte for byte, the set settings and the passthrough -- and nothing else (no chat
// parameters, no response_format). The JSON answer's text is the assistant turn.
func TestTranscriptionRunPostsOneMultipartRequestToAudioTranscriptions(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	older, clip := []byte("an older clip"), speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", clip)
	prep.Settings.Temperature = 0.7
	prep.Settings.MaxTokens = 1234
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Language: "de", Prompt: "Namen: Zoe", Temperature: ptr(0.2),
		Passthrough: map[string]json.RawMessage{"timestamp_granularity": json.RawMessage(`"word"`)},
	}
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: audioUserContent(t, "", audioDataURL("audio/webm", older))},
		{Role: "assistant", Content: json.RawMessage(`"an earlier transcript"`)},
		{Role: "user", Content: audioUserContent(t, "please transcribe", audioDataURL("audio/wav", clip))},
	}

	run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "completed" || snap.Err != "" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	got := takeSpeechRunCall(t, calls)
	if got.method != http.MethodPost || got.path != "/v1/audio/transcriptions" {
		t.Fatalf("request = %s %s, want POST /v1/audio/transcriptions", got.method, got.path)
	}
	if !strings.HasPrefix(got.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q, want multipart/form-data with a boundary (not the JSON the shared header set states)", got.contentType)
	}
	if got.header.Get(internalUserHeaderName) != owner.UserID || got.header.Get(sessionHeaderName) != chatID || got.header.Get(csrfHeaderName) != "1" || got.header.Get(internalAuthHeaderName) != "test-internal-secret" {
		t.Fatalf("the shared loopback header set is missing: %v", got.header)
	}

	parts := decodeTranscriptionForm(t, got.contentType, got.body)
	want := map[string]string{
		"model":                 transcriptionGatewayModel,
		"stream":                "true",
		"language":              "de",
		"prompt":                "Namen: Zoe",
		"temperature":           "0.2",
		"timestamp_granularity": "word",
	}
	if fields := formFields(t, parts); !equalStringMaps(fields, want) {
		t.Fatalf("form fields = %v\nwant          %v", fields, want)
	}
	file := onlyFile(t, parts)
	if file.filename != "audio.wav" || file.contentType != "audio/wav" || !bytes.Equal(file.data, clip) {
		t.Fatalf("file part = %q (%s, %d bytes): want the LAST user message's audio.wav, byte for byte (%d bytes)", file.filename, file.contentType, len(file.data), len(clip))
	}

	if snap.Content != "" {
		t.Fatalf("content = %q, want empty -- the transcript rides the structured parts", snap.Content)
	}
	if want := `[{"type":"text","text":"hello"}]`; string(snap.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, want)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want none", got)
	}

	// The turn is committed complete as an ordinary text turn: one text part, not an
	// audio one, and no blob is created for it.
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Content), `"text":"hello"`) || !strings.Contains(string(stored.Content), `"status":"complete"`) || strings.Contains(string(stored.Content), `"status":"pending"`) {
		t.Fatalf("the stored turn must be a complete text turn: %s", stored.Content)
	}
	if strings.Contains(string(stored.Content), "audio_url") {
		t.Fatalf("a transcription turn carries no audio part: %s", stored.Content)
	}
}

// Without a transcription block the form is just model and clip, still one request,
// and response_format is never sent.
func TestTranscriptionRunNeverSendsResponseFormat(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	clip := speechTestWAV()
	_, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", clip))
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	got := takeSpeechRunCall(t, calls)
	fields := formFields(t, decodeTranscriptionForm(t, got.contentType, got.body))
	if _, sent := fields["response_format"]; sent {
		t.Fatalf("response_format must never be sent, got fields %v", fields)
	}
	if got := fieldNames(fields); !slices.Equal(got, []string{"model", "stream"}) {
		t.Fatalf("want exactly the model and stream fields, got %v", got)
	}
}

// The run-as token and the server override ride the loopback headers exactly as on
// every other hop.
func TestTranscriptionRunLoopbackCarriesRunAsAndServerOverride(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	prep := transcriptionRunPrep(t, "audio/wav", speechTestWAV())
	prep.Settings.RunAsTokenID = "tok_dev"
	prep.Settings.ServerOverride = "srv-audio"
	prep.Settings.ServerOverrideForceUnreachable = true
	if _, snap := speechRunOutcome(t, srv, owner, chatID, prep); snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	got := takeSpeechRunCall(t, calls)
	if got.header.Get(runAsHeaderName) != "tok_dev" {
		t.Fatalf("%s = %q, want tok_dev", runAsHeaderName, got.header.Get(runAsHeaderName))
	}
	if got.header.Get(serverOverrideHeaderName) != "srv-audio" || got.header.Get(serverOverrideForceHeaderName) != "1" {
		t.Fatalf("server override headers = %q / %q, want srv-audio / 1", got.header.Get(serverOverrideHeaderName), got.header.Get(serverOverrideForceHeaderName))
	}
}

// A passthrough key the run owns fails the run with a code of its own, and NOTHING
// is sent: forwarding the rest would answer a request the user did not make.
func TestTranscriptionRunRejectsAReservedPassthroughKeyWithoutSendingAnything(t *testing.T) {
	for _, key := range []string{"model", "file", "response_format", "stream"} {
		t.Run(key, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			prep := transcriptionRunPrep(t, "audio/wav", speechTestWAV())
			prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
				Passthrough: map[string]json.RawMessage{key: json.RawMessage(`"x"`)},
			}
			run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != transcriptionRunReservedFieldMessage {
				t.Fatalf("error = %q, want %q", got, transcriptionRunReservedFieldMessage)
			}
			requireNoSpeechRunCall(t, calls)
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a failed run commits no transcript, got %s", snap.ContentParts)
			}
		})
	}
}

// A passthrough entry that cannot be sent at all is the dispatch code, not the
// reserved-field one, and nothing is sent either.
func TestTranscriptionRunUnusablePassthroughIsADispatchFailure(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	prep := transcriptionRunPrep(t, "audio/wav", speechTestWAV())
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{
		Passthrough: map[string]json.RawMessage{"a\r\nb": json.RawMessage(`"x"`)},
	}
	run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error", snap.Status)
	}
	if got := runError(t, run); got != transcriptionRunDispatchFailedMessage {
		t.Fatalf("error = %q, want %q", got, transcriptionRunDispatchFailedMessage)
	}
	requireNoSpeechRunCall(t, calls)
}

// A turn whose last user message holds no clip the run may send fails with the
// source code and sends NOTHING: the run never fetches a url, and sending the turn
// without its clip -- or with one of two it had to guess between -- would answer a
// request the user did not make.
func TestTranscriptionRunWithoutADecodableClipFailsAsSourceUnreadable(t *testing.T) {
	clip := speechTestWAV()
	wav := audioDataURL("audio/wav", clip)
	content := func(parts ...map[string]any) json.RawMessage {
		raw, err := json.Marshal(parts)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	audioPart := func(url string) map[string]any {
		return map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": url}}
	}
	cases := []struct {
		name    string
		history []portal.ChatAPIMessage
	}{
		{"a plain string content", []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"just words"`)}}},
		{"text parts only", []portal.ChatAPIMessage{{Role: "user", Content: content(map[string]any{"type": "text", "text": "hi"})}}},
		{"an empty audio url", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart(""))}}},
		{"an external url", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart("https://example.test/clip.wav"))}}},
		{"an unresolved gateway reference", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart(chatBlobRoutePrefix + "blob_0123456789abcdef"))}}},
		{"a type that is not audio", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart("data:image/png;base64,iVBORw0KGgo="))}}},
		{"an audio type outside the allow-list", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart("data:audio/flac;base64,ZmxhYw=="))}}},
		{"a payload that is not base64", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart("data:audio/wav;base64,@@not base64@@"))}}},
		{"an empty payload", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart("data:audio/wav;base64,"))}}},
		{"two clips", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart(wav), audioPart(wav))}}},
		{"a good clip beside an unreadable one", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart(wav), audioPart("https://example.test/clip.wav"))}}},
		{"a good clip beside an empty url", []portal.ChatAPIMessage{{Role: "user", Content: content(audioPart(wav), audioPart(""))}}},
		{"no user message at all", []portal.ChatAPIMessage{{Role: "assistant", Content: json.RawMessage(`"hello"`)}}},
		{"an empty history", nil},
		{
			// Only the LAST user message counts: an earlier clip is an earlier turn's.
			name: "the clip belongs to an earlier turn",
			history: []portal.ChatAPIMessage{
				{Role: "user", Content: content(audioPart(wav))},
				{Role: "assistant", Content: json.RawMessage(`"an earlier transcript"`)},
				{Role: "user", Content: json.RawMessage(`"and now?"`)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			prep := transcriptionRunPrep(t, "audio/wav", clip)
			prep.History = tc.history
			run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != transcriptionRunSourceUnreadableMessage {
				t.Fatalf("error = %q, want %q", got, transcriptionRunSourceUnreadableMessage)
			}
			requireNoSpeechRunCall(t, calls)
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a failed run commits no transcript, got %s", snap.ContentParts)
			}
		})
	}
}

// An upstream failure ends the run as a FINISHED error turn that names the failure,
// never a blank or pending turn: the gateway's own error code is kept (it is what
// the portal can label), and a body that is not that envelope falls back to the
// status line.
func TestTranscriptionRunUpstreamErrorFinishesAnErrorTurn(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "gateway envelope", status: http.StatusBadGateway, contentType: "application/json",
			body:    `{"error":{"message":"upstream blew up","type":"upstream_error","code":"transcription.upstream_error"}}`,
			wantErr: "transcription.upstream_error",
		},
		{
			name: "a request error code", status: http.StatusBadRequest, contentType: "application/json",
			body:    `{"error":{"message":"stream is not supported","type":"invalid_request_error","code":"transcription.stream_unsupported"}}`,
			wantErr: "transcription.stream_unsupported",
		},
		{
			name: "not an envelope", status: http.StatusInternalServerError, contentType: "text/plain",
			body:    "boom",
			wantErr: "upstream status 500 Internal Server Error",
		},
		{
			// A refusal that carries a JSON body of the OK shape is still a refusal: the
			// status decides, not the body.
			name: "a non-2xx with a transcript-shaped body", status: http.StatusServiceUnavailable, contentType: "application/json",
			body:    transcriptionRunOKBody,
			wantErr: "upstream status 503 Service Unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, tc.status, tc.contentType, []byte(tc.body))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			_, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
			takeSpeechRunCall(t, calls)
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if snap.Err != tc.wantErr {
				t.Fatalf("error = %q, want %q", snap.Err, tc.wantErr)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a failed run commits no transcript, got %s", snap.ContentParts)
			}
			stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stored.Content), `"status":"error"`) || strings.Contains(string(stored.Content), `"status":"pending"`) {
				t.Fatalf("the stored turn must be a finished error turn: %s", stored.Content)
			}
		})
	}
}

// A 2xx the run cannot turn into a transcript is an ERROR, never a blank bubble:
// one that is not the answer's JSON is unreadable, and one that is but holds no
// text is its own fact -- the clip produced nothing -- which the user can act on
// (record again) where "unreadable" they cannot.
func TestTranscriptionRunTreatsAnUnusable2xxAsAnError(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantErr     string
	}{
		{"empty text", "application/json", `{"text":"","timing":{"audio_duration_ms":10}}`, transcriptionRunNoTranscriptMessage},
		{"absent text", "application/json", `{"timing":{"audio_duration_ms":10}}`, transcriptionRunNoTranscriptMessage},
		{"null text", "application/json", `{"text":null}`, transcriptionRunNoTranscriptMessage},
		{"whitespace-only text", "application/json", `{"text":"  \n\t "}`, transcriptionRunNoTranscriptMessage},
		{"an empty object", "application/json", `{}`, transcriptionRunNoTranscriptMessage},
		{"an empty body", "application/json", ``, transcriptionRunResponseUnreadableMessage},
		{"plain text", "text/plain", `hello`, transcriptionRunResponseUnreadableMessage},
		{"an HTML error page", "text/html", `<html>bad gateway</html>`, transcriptionRunResponseUnreadableMessage},
		{"a JSON array", "application/json", `["hello"]`, transcriptionRunResponseUnreadableMessage},
		{"a JSON string", "application/json", `"hello"`, transcriptionRunResponseUnreadableMessage},
		{"a JSON null", "application/json", `null`, transcriptionRunResponseUnreadableMessage},
		{"a text that is no string", "application/json", `{"text":5}`, transcriptionRunResponseUnreadableMessage},
		{"a truncated body", "application/json", `{"text":"hel`, transcriptionRunResponseUnreadableMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, _ := newSpeechRunLoopback(t, http.StatusOK, tc.contentType, []byte(tc.body))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL
			run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != tc.wantErr {
				t.Fatalf("error = %q, want %q", got, tc.wantErr)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("an unusable response commits no transcript, got %s", snap.ContentParts)
			}
		})
	}
}

// The transcript is committed exactly as the model wrote it: the part carries the
// text untouched -- inner line breaks, quotes and non-ASCII included -- and is not
// reshaped into anything else.
func TestTranscriptionRunKeepsTheTranscriptVerbatim(t *testing.T) {
	text := " Grüße, \"Zoe\"!\nZweite Zeile."
	body, err := json.Marshal(map[string]any{"text": text, "timing": map[string]any{"audio_duration_ms": 5}})
	if err != nil {
		t.Fatal(err)
	}
	loopback, _ := newSpeechRunLoopback(t, http.StatusOK, "application/json", body)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	_, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(snap.ContentParts, &parts); err != nil {
		t.Fatalf("content parts %s: %v", snap.ContentParts, err)
	}
	if len(parts) != 1 || parts[0].Type != "text" || parts[0].Text != text {
		t.Fatalf("parts = %+v, want one text part holding %q", parts, text)
	}
}

// A gateway that cannot reach ITSELF must not show a user Go's transport error
// (which embeds the internal URL): the run ends with the dispatch code, and the full
// error stays in the operator's log.
func TestTranscriptionRunDispatchFailureIsACodeNotARawGoError(t *testing.T) {
	loopback, _ := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	dead := httptest.NewServer(http.NotFoundHandler())
	srv.selfBaseURL = dead.URL
	dead.Close()

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error", snap.Status)
	}
	got := runError(t, run)
	if got != transcriptionRunDispatchFailedMessage {
		t.Fatalf("error = %q, want the mapped code %q", got, transcriptionRunDispatchFailedMessage)
	}
	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "dial") {
		t.Fatalf("error %q leaks the internal URL / transport detail", got)
	}
}

// A transcription run is BOUNDED like a speech run, and the deadline is not a
// cancel: the user pressed nothing, so the run ends with the timeout code, not the
// empty message a Stop carries.
func TestTranscriptionRunDeadlineIsBoundedAndIsNotACancel(t *testing.T) {
	if d, bounded := runDeadlineFor(chatRunKindTranscription); !bounded || d != transcriptionRunDeadline {
		t.Fatalf("runDeadlineFor(transcription) = (%v, %v), want (%v, true)", d, bounded, transcriptionRunDeadline)
	}
	restore := transcriptionRunDeadline
	transcriptionRunDeadline = 50 * time.Millisecond
	t.Cleanup(func() { transcriptionRunDeadline = restore })

	loopback, release := stalledSpeechLoopback(t)
	defer release()
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", snap.Status)
	}
	if got := runError(t, run); got != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q", got, runTimedOutMessage)
	}
}

// A user Stop on a transcription run stays a CANCEL: status canceled with an EMPTY
// message, which is what the UI reads as "the user pressed Stop".
func TestTranscriptionRunUserCancelStaysACancel(t *testing.T) {
	loopback, release := stalledSpeechLoopback(t)
	defer release()
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want empty -- a Stop carries no message", got)
	}
}

// The executor commits exactly once and only through the terminal step the other
// buffered kinds use: launched as a run of its own kind (executeRun's branch), it
// ends the run, and the run's kind is the one stamped from the prepared settings.
func TestExecuteRunBranchesATranscriptionKindToTheTranscriptionExecutor(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunOKBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	if run.kindValue() != chatRunKindTranscription {
		t.Fatalf("run kind = %q, want %q", run.kindValue(), chatRunKindTranscription)
	}
	if got := takeSpeechRunCall(t, calls); got.path != "/v1/audio/transcriptions" {
		t.Fatalf("the run reached %q, not the transcription endpoint (the text path would have posted /v1/chat/completions)", got.path)
	}
}

// --- a streamed transcription ------------------------------------------------
//
// The gateway answers the run's stream=true request with an event stream or, when the
// mapped model is not known to stream, with the one-shot JSON of a plain
// transcription. The stand-ins below play the gateway's own endpoint for both; what is
// asserted is what the run puts on the wire and what it does with the answer.

// streamingStub is what a scripted event-stream stand-in hands its script: the means
// to write frames (each flushed, so the run sees it as it is written) and to hold the
// stream back. wait and until report false when the request ended, which a script
// takes as its cue to stop.
type streamingStub struct {
	w       http.ResponseWriter
	flusher http.Flusher
	r       *http.Request
}

func (s streamingStub) emit(frames ...string) {
	for _, frame := range frames {
		_, _ = io.WriteString(s.w, frame)
	}
	s.flusher.Flush()
}

func (s streamingStub) wait(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.r.Context().Done():
		return false
	}
}

func (s streamingStub) until(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-s.r.Context().Done():
		return false
	}
}

// newStreamingRunLoopback is newSpeechRunLoopback's streaming twin: it records the
// request, answers 200 text/event-stream with the headers flushed at once, and leaves
// the body to script. The stream ends when script returns.
func newStreamingRunLoopback(t *testing.T, script func(s streamingStub)) (*httptest.Server, <-chan speechRunLoopbackCall) {
	t.Helper()
	calls := make(chan speechRunLoopbackCall, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls <- speechRunLoopbackCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: raw}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		script(streamingStub{w: w, flusher: flusher, r: r})
	}))
	t.Cleanup(ts.Close)
	return ts, calls
}

// transcriptDeltaFrame and transcriptDoneFrame are the frames audio.cpp streams, in
// its own shape; transcriptStreamEnd is the [DONE] sentinel after them.
func transcriptDeltaFrame(text string) string {
	raw, _ := json.Marshal(map[string]string{"type": "transcript.text.delta", "delta": text})
	return "data: " + string(raw) + "\n\n"
}

func transcriptDoneFrame(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "transcript.text.done", "text": text, "timing": map[string]any{"ttft_ms": 12}})
	return "data: " + string(raw) + "\n\n"
}

const transcriptStreamEnd = "data: [DONE]\n\n"

// watchRun subscribes to the run and returns the function that waits for it to end and
// hands back the delta events it published, in order, and its terminal event. The
// subscription is taken before the caller lets the stand-in write anything, so no
// event is missed.
func watchRun(t *testing.T, run *ChatRun) func() ([]runEvent, runEvent) {
	t.Helper()
	snapshot, events, unsubscribe := run.subscribe()
	if snapshot.Status != "running" || snapshot.Content != "" {
		t.Fatalf("the run had already moved before it was watched: %+v", snapshot)
	}
	return func() ([]runEvent, runEvent) {
		t.Helper()
		defer unsubscribe()
		var deltas []runEvent
		var terminal runEvent
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, open := <-events:
				if !open {
					if terminal.Event != "done" {
						t.Fatal("the run's event channel closed without a done event")
					}
					return deltas, terminal
				}
				switch ev.Event {
				case "delta":
					deltas = append(deltas, ev)
				case "done":
					terminal = ev
				}
			case <-timeout:
				t.Fatal("the run did not end within 5s")
			}
		}
	}
}

// storedChatContent is the chat's stored transcript, for the assertions on what was
// committed.
func storedChatContent(t *testing.T, srv *Server, owner auth.Token, chatID string) string {
	t.Helper()
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	return string(stored.Content)
}

// A streamed answer fills the turn in as the deltas arrive: each transcript.text.delta
// is published as a run delta the moment it is read (anything else on the stream -- a
// comment, another event type, a line that is no JSON -- is ignored), the stream ends
// in the terminal commit of the full text, and the turn is the ordinary text turn the
// buffered answer commits. The run asked for the stream.
func TestTranscriptionRunStreamsDeltas(t *testing.T) {
	gate := make(chan struct{})
	loopback, calls := newStreamingRunLoopback(t, func(s streamingStub) {
		if !s.until(gate) {
			return
		}
		s.emit(": keepalive\n\n", transcriptDeltaFrame("Hello "))
		s.wait(30 * time.Millisecond)
		s.emit("event: noise\n\n", "data: {\"type\":\"transcript.text.segment\",\"text\":\"x\"}\n\n", "data: {not json\n\n")
		s.emit(transcriptDeltaFrame("streaming "), transcriptDeltaFrame("world"))
		s.wait(30 * time.Millisecond)
		s.emit(transcriptDoneFrame("Hello streaming world"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	wait := watchRun(t, run)
	close(gate)
	deltas, done := wait()

	var got []string
	for _, ev := range deltas {
		got = append(got, ev.Content)
	}
	if want := []string{"Hello ", "streaming ", "world"}; !slices.Equal(got, want) {
		t.Fatalf("published deltas = %q, want %q: one per transcript.text.delta, nothing for the rest", got, want)
	}
	if done.Status != "completed" || done.Err != "" {
		t.Fatalf("status = %q (err %q), want completed", done.Status, done.Err)
	}
	if want := `[{"type":"text","text":"Hello streaming world"}]`; string(done.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", done.ContentParts, want)
	}

	// The turn is committed complete, once, as an ordinary text turn holding the full
	// text -- the shape the buffered answer commits, so nothing downstream learns a new one.
	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"text":"Hello streaming world"`) || !strings.Contains(stored, `"status":"complete"`) || strings.Contains(stored, `"status":"pending"`) {
		t.Fatalf("the stored turn must be the complete text turn: %s", stored)
	}
	if strings.Contains(stored, "audio_url") {
		t.Fatalf("a transcription turn carries no audio part: %s", stored)
	}

	got1 := takeSpeechRunCall(t, calls)
	fields := formFields(t, decodeTranscriptionForm(t, got1.contentType, got1.body))
	if fields["stream"] != "true" || fields["model"] != transcriptionGatewayModel {
		t.Fatalf("form fields = %v, want the run to ask for stream=true", fields)
	}
}

// A run's periodic checkpoint is the text run's: while the stream is still open the
// partial transcript is written to the chat as a pending turn, and the terminal commit
// replaces it -- the commit is the last writer, so no checkpoint leaves the turn
// pending.
func TestTranscriptionRunCheckpointsThePartialTranscript(t *testing.T) {
	restore := runCheckpointInterval
	runCheckpointInterval = 10 * time.Millisecond
	t.Cleanup(func() { runCheckpointInterval = restore })

	proceed := make(chan struct{})
	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		s.emit(transcriptDeltaFrame("partial text"))
		if !s.until(proceed) {
			return
		}
		s.emit(transcriptDoneFrame("partial text, finished"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool {
		stored := storedChatContent(t, srv, owner, chatID)
		return strings.Contains(stored, `"status":"pending"`) && strings.Contains(stored, "partial text")
	})
	if got := run.statusValue(); got != "running" {
		t.Fatalf("status = %q while the stream is open, want running", got)
	}
	close(proceed)
	waitFor(t, func() bool { return run.statusValue() != "running" })

	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"status":"complete"`) || strings.Contains(stored, `"status":"pending"`) || !strings.Contains(stored, "partial text, finished") {
		t.Fatalf("the terminal commit must be the last writer: %s", stored)
	}
}

// The gateway answers a model it does not know to stream with the one-shot JSON of a
// plain transcription. The run takes it as it always has: no partial deltas, one
// terminal commit of the whole transcript. (It asked for a stream all the same --
// choosing between the two is the gateway's.)
func TestTranscriptionRunHandlesBufferedAnswer(t *testing.T) {
	gate := make(chan struct{})
	calls := make(chan speechRunLoopbackCall, 8)
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls <- speechRunLoopbackCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: raw}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, transcriptionRunOKBody)
	}))
	t.Cleanup(loopback.Close)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	wait := watchRun(t, run)
	close(gate)
	deltas, done := wait()

	if len(deltas) != 0 {
		t.Fatalf("a buffered answer has nothing to stream, got %d deltas: %+v", len(deltas), deltas)
	}
	if done.Status != "completed" || done.Err != "" || done.Content != "" {
		t.Fatalf("done = %q/%q with content %q, want completed with the transcript in the parts only", done.Status, done.Err, done.Content)
	}
	if want := `[{"type":"text","text":"hello"}]`; string(done.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", done.ContentParts, want)
	}
	stored := storedChatContent(t, srv, owner, chatID)
	if strings.Count(stored, `"role":"assistant"`) != 1 || !strings.Contains(stored, `"status":"complete"`) || strings.Contains(stored, `"status":"pending"`) {
		t.Fatalf("a buffered answer is one complete assistant turn, never a pending one: %s", stored)
	}
	got := takeSpeechRunCall(t, calls)
	if fields := formFields(t, decodeTranscriptionForm(t, got.contentType, got.body)); fields["stream"] != "true" {
		t.Fatalf("form fields = %v, want the run to ask for stream=true even when the answer is one-shot", fields)
	}
}

// A streaming model's first delta can be a long time coming (a cold start, a model
// still loading), and the run waits for it: nothing is published, the run stays
// running and shows nothing, however long the stream stays silent before it. The
// stand-in (which plays the gateway's own endpoint, so no relay watchdog is
// involved) answers the headers at once and holds the first delta back for 300ms
// behind nothing but heartbeat comments. What is pinned is the run's side of it:
// silence before the first delta does not cancel or fail the run, nothing is
// published meanwhile, and the wait is measured as the turn's time to first token.
func TestTranscriptionRunWaitsForASlowFirstDelta(t *testing.T) {
	first := make(chan struct{})
	slow, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		for i := 0; i < 3; i++ {
			// Comments are the agent router's heartbeat before its first body byte: a
			// sign of life, but not a delta.
			s.emit(": keepalive\n\n")
			if !s.wait(80 * time.Millisecond) {
				return
			}
		}
		if !s.until(first) {
			return
		}
		s.emit(transcriptDeltaFrame("late "), transcriptDoneFrame("late hello"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, slow.URL)
	srv.selfBaseURL = slow.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // a long silence: heartbeats only, no delta in sight
	if got := run.statusValue(); got != "running" {
		t.Fatalf("status = %q (err %q) before the first delta, want the run still waiting", got, runError(t, run))
	}
	if _, content := run.buffered(); content != "" {
		t.Fatalf("content = %q before the first delta, want none", content)
	}
	close(first)
	waitFor(t, func() bool { return run.statusValue() != "running" })
	snap, _, unsub := run.subscribe()
	unsub()
	if snap.Status != "completed" || snap.Err != "" {
		t.Fatalf("status = %q (err %q), want completed once the slow first delta came", snap.Status, snap.Err)
	}
	if want := `[{"type":"text","text":"late hello"}]`; string(snap.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, want)
	}
	// What the wait cost is the turn's time to first token, measured from the dispatch.
	if snap.Metrics == nil || snap.Metrics.TTFTMs < 240 {
		t.Fatalf("metrics = %+v, want a time to first delta of at least the 240ms of silence", snap.Metrics)
	}
}

// The run's deadline is the budget for the wait for the FIRST delta: a stream that
// never delivers one -- here one that does nothing but send its heartbeat -- ends at
// the deadline, and as a timeout, not a cancel.
func TestTranscriptionRunDeadlineBoundsAStreamThatNeverDeliversADelta(t *testing.T) {
	restore := transcriptionRunDeadline
	transcriptionRunDeadline = 150 * time.Millisecond
	t.Cleanup(func() { transcriptionRunDeadline = restore })

	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		for {
			s.emit(": keepalive\n\n")
			if !s.wait(20 * time.Millisecond) {
				return
			}
		}
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", snap.Status)
	}
	if got := runError(t, run); got != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q", got, runTimedOutMessage)
	}
}

// Once deltas flow the end-to-end deadline no longer applies -- the stream shows for
// itself that it is alive, as a text run's does, and a long transcription must not be
// cut at a ceiling meant for a wait with nothing to show. The deadline here is 300ms,
// the first delta comes at ~50ms and the stream runs past 600ms.
func TestTranscriptionRunLiftsItsDeadlineAtTheFirstDelta(t *testing.T) {
	restore := transcriptionRunDeadline
	transcriptionRunDeadline = 300 * time.Millisecond
	t.Cleanup(func() { transcriptionRunDeadline = restore })

	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		if !s.wait(50 * time.Millisecond) {
			return
		}
		s.emit(transcriptDeltaFrame("one "))
		if !s.wait(250 * time.Millisecond) {
			return
		}
		s.emit(transcriptDeltaFrame("two "))
		if !s.wait(350 * time.Millisecond) {
			return
		}
		s.emit(transcriptDoneFrame("one two three"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "completed" || runError(t, run) != "" {
		t.Fatalf("status = %q (err %q), want completed: the deadline ends at the first delta", snap.Status, runError(t, run))
	}
	if want := `[{"type":"text","text":"one two three"}]`; string(snap.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, want)
	}
}

// What the commit holds is the done event's text when it states one -- the
// authoritative statement of the whole transcript, which may differ from the deltas
// before it -- else the deltas, and a stream with neither is the empty-transcript
// error, never a blank turn.
func TestTranscriptionRunCommitsTheDoneText(t *testing.T) {
	cases := []struct {
		name     string
		frames   []string
		wantText string
		wantErr  string
	}{
		{"the done text restates the deltas", []string{transcriptDeltaFrame("Hel"), transcriptDeltaFrame("lo"), transcriptDoneFrame("Hello")}, "Hello", ""},
		{"the done text wins over the deltas", []string{transcriptDeltaFrame("Helo wrld"), transcriptDoneFrame("Hello world.")}, "Hello world.", ""},
		{"a done with no text leaves the deltas standing", []string{transcriptDeltaFrame("Hel"), transcriptDeltaFrame("lo"), transcriptDoneFrame("")}, "Hello", ""},
		{"a done with no deltas before it", []string{transcriptDoneFrame("All at once.")}, "All at once.", ""},
		{"the text kept verbatim", []string{transcriptDeltaFrame(" Grüße,"), transcriptDoneFrame(" Grüße, \"Zoe\"!\nZweite Zeile.")}, " Grüße, \"Zoe\"!\nZweite Zeile.", ""},
		{"nothing heard", []string{transcriptDoneFrame("")}, "", transcriptionRunNoTranscriptMessage},
		{"nothing but blanks heard", []string{transcriptDeltaFrame("  "), transcriptDoneFrame(" \n")}, "", transcriptionRunNoTranscriptMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
				s.emit(tc.frames...)
				s.emit(transcriptStreamEnd)
			})
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
			if tc.wantErr != "" {
				if snap.Status != "error" || runError(t, run) != tc.wantErr {
					t.Fatalf("status = %q, error = %q; want error %q", snap.Status, runError(t, run), tc.wantErr)
				}
				if len(snap.ContentParts) != 0 {
					t.Fatalf("a failed run commits no transcript, got %s", snap.ContentParts)
				}
				return
			}
			if snap.Status != "completed" {
				t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
			}
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(snap.ContentParts, &parts); err != nil {
				t.Fatalf("content parts %s: %v", snap.ContentParts, err)
			}
			if len(parts) != 1 || parts[0].Type != "text" || parts[0].Text != tc.wantText {
				t.Fatalf("parts = %+v, want one text part holding %q", parts, tc.wantText)
			}
		})
	}
}

// A stream that ends before its done event -- the upstream dropped, or the gateway cut
// it at its own idle timeout -- is NOT a transcript: committing what arrived as a
// finished turn would present a truncated text as the whole one. The run ends in the
// unreadable-response error, and the turn keeps the text that was published, as an error turn.
func TestTranscriptionRunStreamCutBeforeDoneIsAnError(t *testing.T) {
	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		s.emit(transcriptDeltaFrame("the start of "), transcriptDeltaFrame("a sentence"))
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "error" || runError(t, run) != transcriptionRunResponseUnreadableMessage {
		t.Fatalf("status = %q, error = %q; want error %q", snap.Status, runError(t, run), transcriptionRunResponseUnreadableMessage)
	}
	if len(snap.ContentParts) != 0 {
		t.Fatalf("a cut stream commits no finished transcript, got %s", snap.ContentParts)
	}
	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"content":"the start of a sentence"`) || !strings.Contains(stored, `"status":"error"`) || strings.Contains(stored, `"status":"pending"`) {
		t.Fatalf("the turn must be a finished error turn that keeps the published text: %s", stored)
	}
}

// A bare [DONE] is not a done event. The sentinel only says the stream has ended; the
// transcript's end is the transcript.text.done event, which alone states the whole text,
// so a stream of deltas followed by [DONE] and no done event is read as a stream that
// ended before its done event -- the same unreadable-response error and the same error
// turn that keeps the published text as a cut stream gets -- and not as a completed
// transcript assembled from the deltas. (The relay reads it the same way and bills it
// as a loud zero: TestTranscriptionStreamLoudZeroOnIncompleteStream.) A run that took
// the sentinel for a completion would commit a text nothing had vouched for as whole.
func TestTranscriptionRunBareDoneSentinelWithoutADoneEventIsAnError(t *testing.T) {
	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		s.emit(transcriptDeltaFrame("the start of "), transcriptDeltaFrame("a sentence"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if snap.Status != "error" || runError(t, run) != transcriptionRunResponseUnreadableMessage {
		t.Fatalf("status = %q, error = %q; want error %q -- [DONE] without a done event is no completion", snap.Status, runError(t, run), transcriptionRunResponseUnreadableMessage)
	}
	if len(snap.ContentParts) != 0 {
		t.Fatalf("a stream without its done event commits no finished transcript, got %s", snap.ContentParts)
	}
	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"content":"the start of a sentence"`) || !strings.Contains(stored, `"status":"error"`) || strings.Contains(stored, `"status":"pending"`) {
		t.Fatalf("the turn must be a finished error turn that keeps the published text: %s", stored)
	}
}

// A user Stop in the middle of a stream is a CANCEL (the empty message the UI reads as
// "the user pressed Stop"), and the turn keeps the text that was published.
func TestTranscriptionRunUserCancelMidStreamStaysACancel(t *testing.T) {
	loopback, _ := newStreamingRunLoopback(t, func(s streamingStub) {
		s.emit(transcriptDeltaFrame("so far"))
		s.until(nil) // until the request ends
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, transcriptionRunPrep(t, "audio/wav", speechTestWAV()))
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { _, content := run.buffered(); return content == "so far" })
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want empty -- a Stop carries no message", got)
	}
	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"content":"so far"`) || !strings.Contains(stored, `"status":"canceled"`) {
		t.Fatalf("the canceled turn must keep the published text: %s", stored)
	}
}

// The deadline a run arms is only for a kind runDeadlineFor bounds: a kind it leaves
// unbounded arms no timer at all, rather than a zero-length one that would cancel the
// run on the next tick, and lifting it is harmless. A bounded kind's timer cancels the
// context with the deadline cause, and lifting it stops that.
func TestArmRunDeadlineArmsOnlyForABoundedKind(t *testing.T) {
	restore := transcriptionRunDeadline
	transcriptionRunDeadline = 30 * time.Millisecond
	t.Cleanup(func() { transcriptionRunDeadline = restore })

	arm := func(kind string) (context.Context, func()) {
		ctx, cancel := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancel(nil) })
		return ctx, armRunDeadline(kind, cancel)
	}

	unbounded, liftUnbounded := arm("") // the text kind: runDeadlineFor leaves it unbounded
	time.Sleep(100 * time.Millisecond)
	if unbounded.Err() != nil {
		t.Fatalf("an unbounded kind's context ended (%v), want no deadline armed at all", context.Cause(unbounded))
	}
	liftUnbounded()
	liftUnbounded() // lifting a deadline that was never armed, twice, is harmless

	lifted, lift := arm(chatRunKindTranscription)
	lift()
	time.Sleep(100 * time.Millisecond)
	if lifted.Err() != nil {
		t.Fatalf("a lifted deadline still ended the context (%v)", context.Cause(lifted))
	}

	bounded, _ := arm(chatRunKindTranscription)
	waitFor(t, func() bool { return bounded.Err() != nil })
	if !errors.Is(context.Cause(bounded), context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", context.Cause(bounded))
	}
}

// --- a diarized transcription --------------------------------------------------
//
// With the thread's Diarize setting on, the run makes its one request to the gateway's
// /v1/audio/transcriptions/details instead -- one-shot, so it states no stream and reads
// one JSON document -- and commits what the answer holds: a speaker_transcript part when
// at least two distinct speakers took part, the ordinary text part otherwise. The
// stand-ins are the plain run's recording loopback; what is asserted is what the run
// puts on the wire and what it commits from the answer.

// transcriptionRunDetailsBody is the document audio.cpp's details endpoint answers a
// diarizing model with: the transcript, its two speakers' turns (offsets in samples of the
// stated sample_rate, speaker_id a STRING) and the timing block. It is the body the mock
// provider answers with.
const transcriptionRunDetailsBody = `{"text":"Mock transcription. Second speaker.","speaker_turns":[` +
	`{"start_sample":0,"end_sample":16000,"speaker_id":"0","confidence":0,"text":"Mock transcription."},` +
	`{"start_sample":16000,"end_sample":32000,"speaker_id":"1","confidence":0,"text":"Second speaker."}` +
	`],"sample_rate":16000,"timing":{"audio_duration_ms":2000,"total_ms":420}}`

// transcriptionRunSpeakerParts is what the run commits for transcriptionRunDetailsBody:
// ONE speaker_transcript part -- the full text and the turns in seconds, the speaker the
// audio.cpp id verbatim -- and no text part beside it.
const transcriptionRunSpeakerParts = `[{"type":"speaker_transcript","text":"Mock transcription. Second speaker.","turns":[` +
	`{"speaker":"0","start":0,"end":1,"text":"Mock transcription."},` +
	`{"speaker":"1","start":1,"end":2,"text":"Second speaker."}]}]`

// detailsTurn is one speaker turn as audio.cpp writes it.
func detailsTurn(start, end int, speaker, text string) string {
	return fmt.Sprintf(`{"start_sample":%d,"end_sample":%d,"speaker_id":%q,"confidence":0,"text":%q}`, start, end, speaker, text)
}

// detailsDocument is a details document of text, the given turns and (when non-empty)
// the given sample_rate member, spelled as written -- `"sample_rate":16000` -- so a test
// can leave it out or give it a value audio.cpp never would.
func detailsDocument(text, sampleRate string, turns ...string) string {
	doc := `{"text":` + fmt.Sprintf("%q", text) + `,"speaker_turns":[` + strings.Join(turns, ",") + `]`
	if sampleRate != "" {
		doc += `,` + sampleRate
	}
	return doc + `,"timing":{"audio_duration_ms":3000}}`
}

// diarizedRunPrep is the prepared transcription run with the thread's Diarize setting on
// and the given settings (the zero value for none), which Diarize is added to.
func diarizedRunPrep(t *testing.T, settings portal.ChatTranscriptionSettings) PrepareRunResult {
	t.Helper()
	prep := transcriptionRunPrep(t, "audio/wav", speechTestWAV())
	settings.Diarize = true
	prep.Settings.Transcription = &settings
	return prep
}

// diarizedRunOutcome runs a diarized transcription against a loopback that answers
// status / contentType / body, and returns the run's terminal event and the ONE request
// it made. Every diarized run it drives is also held to the request shape that makes it
// one: a POST to the details endpoint whose form states no stream and no diarize.
func diarizedRunOutcome(t *testing.T, status int, contentType, body string) (runEvent, speechRunLoopbackCall) {
	t.Helper()
	loopback, calls := newSpeechRunLoopback(t, status, contentType, []byte(body))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	_, snap := speechRunOutcome(t, srv, owner, chatID, diarizedRunPrep(t, portal.ChatTranscriptionSettings{}))
	got := takeSpeechRunCall(t, calls)
	requireDetailsRequest(t, got)
	return snap, got
}

// requireDetailsRequest fails unless call is the diarized run's request: a multipart POST
// to /v1/audio/transcriptions/details whose form is the model and the clip and nothing that
// asks for a stream or states the Diarize choice.
func requireDetailsRequest(t *testing.T, call speechRunLoopbackCall) {
	t.Helper()
	if call.method != http.MethodPost || call.path != "/v1/audio/transcriptions/details" {
		t.Fatalf("request = %s %s, want POST /v1/audio/transcriptions/details", call.method, call.path)
	}
	fields := formFields(t, decodeTranscriptionForm(t, call.contentType, call.body))
	for _, name := range []string{"stream", "diarize"} {
		if value, sent := fields[name]; sent {
			t.Fatalf("the diarized run sent %s=%q; the details endpoint is one-shot and Diarize is not a form field (fields %v)", name, value, fields)
		}
	}
}

// A diarized run makes exactly ONE POST to the gateway's own details endpoint: a
// multipart form of the model, the set settings and the passthrough, the clip of the LAST
// user message byte for byte -- and NO stream and no diarize field -- under the shared
// loopback header set. The answer's speaker turns are the turn: one speaker_transcript
// part, committed complete as the whole turn.
func TestTranscriptionRunDiarizedPostsOneShotToTheDetailsEndpoint(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunDetailsBody))
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	older, clip := []byte("an older clip"), speechTestWAV()
	prep := diarizedRunPrep(t, portal.ChatTranscriptionSettings{
		Language: "de", Prompt: "Namen: Zoe", Temperature: ptr(0.2),
		Passthrough: map[string]json.RawMessage{"timestamp_granularity": json.RawMessage(`"word"`)},
	})
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: audioUserContent(t, "", audioDataURL("audio/webm", older))},
		{Role: "assistant", Content: json.RawMessage(`"an earlier transcript"`)},
		{Role: "user", Content: audioUserContent(t, "please transcribe", audioDataURL("audio/wav", clip))},
	}

	run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "completed" || snap.Err != "" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	got := takeSpeechRunCall(t, calls)
	requireDetailsRequest(t, got)
	if !strings.HasPrefix(got.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q, want multipart/form-data with a boundary (not the JSON the shared header set states)", got.contentType)
	}
	if got.header.Get(internalUserHeaderName) != owner.UserID || got.header.Get(sessionHeaderName) != chatID || got.header.Get(csrfHeaderName) != "1" || got.header.Get(internalAuthHeaderName) != "test-internal-secret" {
		t.Fatalf("the shared loopback header set is missing: %v", got.header)
	}
	parts := decodeTranscriptionForm(t, got.contentType, got.body)
	want := map[string]string{
		"model":                 transcriptionGatewayModel,
		"language":              "de",
		"prompt":                "Namen: Zoe",
		"temperature":           "0.2",
		"timestamp_granularity": "word",
	}
	if fields := formFields(t, parts); !equalStringMaps(fields, want) {
		t.Fatalf("form fields = %v\nwant          %v", fields, want)
	}
	if file := onlyFile(t, parts); file.filename != "audio.wav" || file.contentType != "audio/wav" || !bytes.Equal(file.data, clip) {
		t.Fatalf("file part = %q (%s, %d bytes): want the LAST user message's audio.wav, byte for byte (%d bytes)", file.filename, file.contentType, len(file.data), len(clip))
	}

	if snap.Content != "" {
		t.Fatalf("content = %q, want empty -- the transcript rides the structured parts", snap.Content)
	}
	if string(snap.ContentParts) != transcriptionRunSpeakerParts {
		t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, transcriptionRunSpeakerParts)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want none", got)
	}

	// Committed complete as the turn's only content, and no audio part with it.
	stored := storedChatContent(t, srv, owner, chatID)
	if !strings.Contains(stored, `"type":"speaker_transcript"`) || !strings.Contains(stored, `"speaker":"1"`) || !strings.Contains(stored, `"status":"complete"`) || strings.Contains(stored, `"status":"pending"`) {
		t.Fatalf("the stored turn must be the complete speaker_transcript turn: %s", stored)
	}
	if strings.Contains(stored, "audio_url") {
		t.Fatalf("a transcription turn carries no audio part: %s", stored)
	}
}

// Two or more DISTINCT speaker ids are the speaker transcript, however many turns there
// are and whatever the ids are called. The offsets are samples of the document's own
// sample_rate and are committed as seconds; a rate that is absent, zero or negative (or a
// turn whose span is no span) leaves the times OUT rather than dividing by zero or
// committing garbage -- the part is still the speaker view, only without times.
func TestTranscriptionRunDiarizedCommitsASpeakerTranscript(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "two speakers",
			body: transcriptionRunDetailsBody,
			want: transcriptionRunSpeakerParts,
		},
		{
			name: "a speaker who returns, in fractions of a second",
			body: detailsDocument("Hi. Hello. Bye.", `"sample_rate":16000`,
				detailsTurn(0, 8000, "0", "Hi."), detailsTurn(8000, 20000, "1", "Hello."), detailsTurn(20000, 24000, "0", "Bye.")),
			want: `[{"type":"speaker_transcript","text":"Hi. Hello. Bye.","turns":[` +
				`{"speaker":"0","start":0,"end":0.5,"text":"Hi."},` +
				`{"speaker":"1","start":0.5,"end":1.25,"text":"Hello."},` +
				`{"speaker":"0","start":1.25,"end":1.5,"text":"Bye."}]}]`,
		},
		{
			name: "three speakers",
			body: detailsDocument("A. B. C.", `"sample_rate":16000`,
				detailsTurn(0, 16000, "0", "A."), detailsTurn(16000, 32000, "1", "B."), detailsTurn(32000, 48000, "2", "C.")),
			want: `[{"type":"speaker_transcript","text":"A. B. C.","turns":[` +
				`{"speaker":"0","start":0,"end":1,"text":"A."},` +
				`{"speaker":"1","start":1,"end":2,"text":"B."},` +
				`{"speaker":"2","start":2,"end":3,"text":"C."}]}]`,
		},
		{
			// An id is any string; nothing assumes integers, an order or a count.
			name: "free-form speaker labels, verbatim",
			body: detailsDocument("Hallo. Hi.", `"sample_rate":16000`,
				detailsTurn(0, 16000, "anna", "Hallo."), detailsTurn(16000, 32000, "SPEAKER_01", "Hi.")),
			want: `[{"type":"speaker_transcript","text":"Hallo. Hi.","turns":[` +
				`{"speaker":"anna","start":0,"end":1,"text":"Hallo."},` +
				`{"speaker":"SPEAKER_01","start":1,"end":2,"text":"Hi."}]}]`,
		},
		{
			name: "another sample rate",
			body: detailsDocument("A. B.", `"sample_rate":8000`,
				detailsTurn(0, 4000, "0", "A."), detailsTurn(4000, 8000, "1", "B.")),
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","start":0,"end":0.5,"text":"A."},` +
				`{"speaker":"1","start":0.5,"end":1,"text":"B."}]}]`,
		},
		{
			name: "no sample rate leaves the times out",
			body: detailsDocument("A. B.", "", detailsTurn(0, 16000, "0", "A."), detailsTurn(16000, 32000, "1", "B.")),
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","text":"A."},{"speaker":"1","text":"B."}]}]`,
		},
		{
			name: "a zero sample rate leaves the times out",
			body: detailsDocument("A. B.", `"sample_rate":0`, detailsTurn(0, 16000, "0", "A."), detailsTurn(16000, 32000, "1", "B.")),
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","text":"A."},{"speaker":"1","text":"B."}]}]`,
		},
		{
			name: "a negative sample rate leaves the times out",
			body: detailsDocument("A. B.", `"sample_rate":-16000`, detailsTurn(0, 16000, "0", "A."), detailsTurn(16000, 32000, "1", "B.")),
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","text":"A."},{"speaker":"1","text":"B."}]}]`,
		},
		{
			// Only the turn whose offsets are no span loses its times; the others keep theirs.
			name: "a turn whose span is no span has no times",
			body: detailsDocument("A. B. C.", `"sample_rate":16000`,
				detailsTurn(16000, 8000, "0", "A."), detailsTurn(-16000, 16000, "1", "B."), detailsTurn(16000, 32000, "0", "C.")),
			want: `[{"type":"speaker_transcript","text":"A. B. C.","turns":[` +
				`{"speaker":"0","text":"A."},{"speaker":"1","text":"B."},` +
				`{"speaker":"0","start":1,"end":2,"text":"C."}]}]`,
		},
		{
			// A positive but denormal rate overflows a large offset to +Inf, which cannot be
			// marshalled: the times are left out (the span's start and end both), not the transcript lost.
			name: "a rate whose division overflows leaves the times out",
			body: detailsDocument("A. B.", `"sample_rate":5e-324`, detailsTurn(0, 16000, "0", "A."), detailsTurn(16000, 32000, "1", "B.")),
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","text":"A."},{"speaker":"1","text":"B."}]}]`,
		},
		{
			// The document's other members (segments, words, a language) are not read.
			name: "other members of the document are ignored",
			body: `{"text":"A. B.","language":"en","segments":[{"start":0,"end":1,"text":"A. B."}],"words":[{"word":"A.","start":0,"end":0.5}],` +
				`"speaker_turns":[` + detailsTurn(0, 16000, "0", "A.") + `,` + detailsTurn(16000, 32000, "1", "B.") + `],"sample_rate":16000}`,
			want: `[{"type":"speaker_transcript","text":"A. B.","turns":[` +
				`{"speaker":"0","start":0,"end":1,"text":"A."},` +
				`{"speaker":"1","start":1,"end":2,"text":"B."}]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _ := diarizedRunOutcome(t, http.StatusOK, "application/json", tc.body)
			if snap.Status != "completed" || snap.Err != "" {
				t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
			}
			if string(snap.ContentParts) != tc.want {
				t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, tc.want)
			}
		})
	}
}

// The transcript of a speaker transcript is committed exactly as the model wrote it,
// like the plain part's: inner line breaks, quotes and non-ASCII untouched, in the part's
// text and in each turn's.
func TestTranscriptionRunDiarizedKeepsTheTranscriptVerbatim(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"text": " Grüße, \"Zoe\"!\nZweite Zeile.",
		"speaker_turns": []map[string]any{
			{"start_sample": 0, "end_sample": 16000, "speaker_id": "0", "text": " Grüße, \"Zoe\"!"},
			{"start_sample": 16000, "end_sample": 32000, "speaker_id": "1", "text": "\nZweite Zeile."},
		},
		"sample_rate": 16000,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := diarizedRunOutcome(t, http.StatusOK, "application/json", string(body))
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	var parts []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Turns []struct {
			Speaker string `json:"speaker"`
			Text    string `json:"text"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(snap.ContentParts, &parts); err != nil {
		t.Fatalf("content parts %s: %v", snap.ContentParts, err)
	}
	if len(parts) != 1 || parts[0].Type != "speaker_transcript" || parts[0].Text != " Grüße, \"Zoe\"!\nZweite Zeile." {
		t.Fatalf("parts = %+v, want one speaker_transcript part holding the text verbatim", parts)
	}
	if len(parts[0].Turns) != 2 || parts[0].Turns[0].Text != " Grüße, \"Zoe\"!" || parts[0].Turns[1].Text != "\nZweite Zeile." {
		t.Fatalf("turns = %+v, want both turns' text verbatim", parts[0].Turns)
	}
}

// Fewer than two speakers is not a speaker transcript: it is the ordinary transcript, the
// text part the plain run commits -- for a single speaker, and for a model that does not
// diarize at all and so answers no speaker_turns (a document of words only, a null or an
// empty list). The result decides, never an error and never a blank turn.
func TestTranscriptionRunDiarizedFallsBackToThePlainTextPart(t *testing.T) {
	const plain = `[{"type":"text","text":"Hello world"}]`
	cases := []struct {
		name string
		body string
	}{
		{
			name: "one speaker across two turns",
			body: detailsDocument("Hello world", `"sample_rate":16000`, detailsTurn(0, 16000, "0", "Hello"), detailsTurn(16000, 32000, "0", "world")),
		},
		{
			name: "a single turn",
			body: detailsDocument("Hello world", `"sample_rate":16000`, detailsTurn(0, 32000, "0", "Hello world")),
		},
		{
			name: "no speaker_turns, words only",
			body: `{"text":"Hello world","words":[{"word":"Hello","start":0,"end":0.4},{"word":"world","start":0.5,"end":0.9}],"timing":{"audio_duration_ms":1000}}`,
		},
		{
			name: "the plain document",
			body: `{"text":"Hello world","timing":{"audio_duration_ms":1000}}`,
		},
		{
			name: "speaker_turns null",
			body: `{"text":"Hello world","speaker_turns":null,"sample_rate":16000}`,
		},
		{
			name: "speaker_turns empty",
			body: detailsDocument("Hello world", `"sample_rate":16000`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _ := diarizedRunOutcome(t, http.StatusOK, "application/json", tc.body)
			if snap.Status != "completed" || snap.Err != "" {
				t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
			}
			if string(snap.ContentParts) != plain {
				t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, plain)
			}
		})
	}
}

// A transcript that is empty is "no transcript", the plain run's error, whatever turns
// the document carries: a speaker view of nothing would be the blank bubble that error
// exists to refuse.
func TestTranscriptionRunDiarizedEmptyTranscriptIsNoTranscript(t *testing.T) {
	turns := detailsTurn(0, 16000, "0", "a") + `,` + detailsTurn(16000, 32000, "1", "b")
	cases := []struct{ name, body string }{
		{"empty text with two speakers", `{"text":"","speaker_turns":[` + turns + `],"sample_rate":16000}`},
		{"whitespace-only text with two speakers", `{"text":"  \n\t ","speaker_turns":[` + turns + `],"sample_rate":16000}`},
		{"absent text with two speakers", `{"speaker_turns":[` + turns + `],"sample_rate":16000}`},
		{"empty text, no turns", `{"text":"","timing":{"audio_duration_ms":10}}`},
		{"an empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _ := diarizedRunOutcome(t, http.StatusOK, "application/json", tc.body)
			if snap.Status != "error" || snap.Err != transcriptionRunNoTranscriptMessage {
				t.Fatalf("status = %q, error = %q, want error %q", snap.Status, snap.Err, transcriptionRunNoTranscriptMessage)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("no transcript commits no parts, got %s", snap.ContentParts)
			}
		})
	}
}

// A 2xx the run cannot read as the details document is the plain run's "response
// unreadable": a body with no usable PRIMARY content -- no document at all, or a text that
// is no string. (A valid text beside a malformed speaker_turns or sample_rate is not
// here: that transcript is kept, see
// TestTranscriptionRunDiarizedDegradesWhenOnlyTheEnrichmentIsMalformed.)
func TestTranscriptionRunDiarizedTreatsAnUnusable2xxAsAnError(t *testing.T) {
	cases := []struct{ name, contentType, body string }{
		{"an empty body", "application/json", ``},
		{"plain text", "text/plain", `hello`},
		{"an HTML error page", "text/html", `<html>bad gateway</html>`},
		{"a JSON array", "application/json", `["hello"]`},
		{"a JSON null", "application/json", `null`},
		{"a text that is no string", "application/json", `{"text":5}`},
		{
			// Valid speaker turns do not rescue a text that is no string: there is no transcript.
			name: "a text that is no string, beside valid speaker turns", contentType: "application/json",
			body: `{"text":5,"speaker_turns":[` + detailsTurn(0, 16000, "0", "a") + `,` + detailsTurn(16000, 32000, "1", "b") + `],"sample_rate":16000}`,
		},
		{"a truncated body", "application/json", `{"text":"hello","speaker_turns":[{"start_sample":0,`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _ := diarizedRunOutcome(t, http.StatusOK, tc.contentType, tc.body)
			if snap.Status != "error" || snap.Err != transcriptionRunResponseUnreadableMessage {
				t.Fatalf("status = %q, error = %q, want error %q", snap.Status, snap.Err, transcriptionRunResponseUnreadableMessage)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("an unusable response commits no transcript, got %s", snap.ContentParts)
			}
		})
	}
}

// speaker_turns and sample_rate are ENRICHMENT: a transcript that is valid (a non-empty
// text string) is never thrown away because of them. A speaker_turns that cannot be read
// as a list of turns -- not a list, entries that are no objects, a field of the wrong type
// -- degrades to the ordinary text part of a plain run, whatever else the document says; a
// sample_rate that is no number is no rate, so the times are left out and the speaker
// view stands as long as two speakers still read. Neither is an error, and neither logs
// the transcript.
func TestTranscriptionRunDiarizedDegradesWhenOnlyTheEnrichmentIsMalformed(t *testing.T) {
	const marker = "zebra-drift-marker"
	good := detailsTurn(0, 16000, "0", marker) + `,` + detailsTurn(16000, 32000, "1", "b")
	plain := `[{"type":"text","text":"` + marker + `"}]`
	noTimes := `[{"type":"speaker_transcript","text":"` + marker + `","turns":[` +
		`{"speaker":"0","text":"` + marker + `"},{"speaker":"1","text":"b"}]}]`
	cases := []struct{ name, body, want string }{
		{"speaker_turns that is a string", `{"text":"` + marker + `","speaker_turns":"nope"}`, plain},
		{"speaker_turns that is an object", `{"text":"` + marker + `","speaker_turns":{"a":1},"sample_rate":16000}`, plain},
		{"speaker_turns that is a number", `{"text":"` + marker + `","speaker_turns":3}`, plain},
		{"a turn that is no object", `{"text":"` + marker + `","speaker_turns":["a","b"]}`, plain},
		{
			// One bad turn among good ones drops the whole view: a speaker view with a hole in it is not the conversation.
			name: "one turn among good ones with a speaker_id that is no string",
			body: `{"text":"` + marker + `","speaker_turns":[` + good + `,{"start_sample":32000,"end_sample":48000,"speaker_id":2,"text":"c"}],"sample_rate":16000}`,
			want: plain,
		},
		{
			name: "a turn whose offset is no number",
			body: `{"text":"` + marker + `","speaker_turns":[` + good + `,{"start_sample":"32000","end_sample":48000,"speaker_id":"2","text":"c"}],"sample_rate":16000}`,
			want: plain,
		},
		{"a sample_rate that is a string", `{"text":"` + marker + `","speaker_turns":[` + good + `],"sample_rate":"16000"}`, noTimes},
		{"a sample_rate that is an object", `{"text":"` + marker + `","speaker_turns":[` + good + `],"sample_rate":{"hz":16000}}`, noTimes},
		{"a sample_rate that is null", `{"text":"` + marker + `","speaker_turns":[` + good + `],"sample_rate":null}`, noTimes},
		{"a sample_rate past float range", `{"text":"` + marker + `","speaker_turns":[` + good + `],"sample_rate":1e999}`, noTimes},
		{"a malformed sample_rate beside one speaker", `{"text":"` + marker + `","speaker_turns":[` + detailsTurn(0, 16000, "0", marker) + `],"sample_rate":"16000"}`, plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			restored := false
			restore := func() {
				if !restored {
					restored = true
					log.SetOutput(previous)
				}
			}
			t.Cleanup(restore)
			snap, _ := diarizedRunOutcome(t, http.StatusOK, "application/json", tc.body)
			restore() // before logged is read: no write can reach it after this

			if snap.Status != "completed" || snap.Err != "" {
				t.Fatalf("status = %q (err %q), want completed: the transcript is valid and must be kept", snap.Status, snap.Err)
			}
			if string(snap.ContentParts) != tc.want {
				t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, tc.want)
			}
			if strings.Contains(logged.String(), marker) {
				t.Fatalf("the log carries the transcript: %s", logged.String())
			}
		})
	}
}

// An upstream failure of the details endpoint ends the run exactly as the plain one's
// does: the gateway's own code is kept, and a body that is not that envelope -- or a
// refusal that carries a document -- falls back to the status line.
func TestTranscriptionRunDiarizedUpstreamErrorFinishesAnErrorTurn(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "gateway envelope", status: http.StatusBadGateway, contentType: "application/json",
			body:    `{"error":{"message":"upstream blew up","type":"upstream_error","code":"transcription.upstream_error"}}`,
			wantErr: "transcription.upstream_error",
		},
		{
			name: "a refused stream", status: http.StatusBadRequest, contentType: "application/json",
			body:    `{"error":{"message":"stream is not supported","type":"invalid_request_error","code":"transcription.stream_unsupported"}}`,
			wantErr: "transcription.stream_unsupported",
		},
		{
			name: "not an envelope", status: http.StatusInternalServerError, contentType: "text/plain",
			body:    "boom",
			wantErr: "upstream status 500 Internal Server Error",
		},
		{
			// The status decides, not the body: a refusal holding a two-speaker document is a refusal.
			name: "a non-2xx with a details document", status: http.StatusServiceUnavailable, contentType: "application/json",
			body:    transcriptionRunDetailsBody,
			wantErr: "upstream status 503 Service Unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _ := diarizedRunOutcome(t, tc.status, tc.contentType, tc.body)
			if snap.Status != "error" || snap.Err != tc.wantErr {
				t.Fatalf("status = %q, error = %q, want error %q", snap.Status, snap.Err, tc.wantErr)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a failed run commits no transcript, got %s", snap.ContentParts)
			}
		})
	}
}

// The diarized run never reads an event stream: the details endpoint is one-shot, so an
// answer that is one is no document it can use -- it is read whole and found unreadable,
// not consumed as the plain run's transcript.text.* frames, and nothing is published.
func TestTranscriptionRunDiarizedNeverConsumesAnEventStream(t *testing.T) {
	loopback, calls := newStreamingRunLoopback(t, func(s streamingStub) {
		s.emit(transcriptDeltaFrame("Hello "), transcriptDeltaFrame("world"), transcriptDoneFrame("Hello world"), transcriptStreamEnd)
	})
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	_, snap := speechRunOutcome(t, srv, owner, chatID, diarizedRunPrep(t, portal.ChatTranscriptionSettings{}))
	requireDetailsRequest(t, takeSpeechRunCall(t, calls))
	if snap.Status != "error" || snap.Err != transcriptionRunResponseUnreadableMessage {
		t.Fatalf("status = %q, error = %q, want error %q", snap.Status, snap.Err, transcriptionRunResponseUnreadableMessage)
	}
	if snap.Content != "" || len(snap.ContentParts) != 0 {
		t.Fatalf("nothing of a stream may be published or committed, got content %q, parts %s", snap.Content, snap.ContentParts)
	}
}

// A passthrough key the run owns fails a diarized run with the same code and sends
// NOTHING, as it does a plain one.
func TestTranscriptionRunDiarizedRejectsAReservedPassthroughKeyWithoutSendingAnything(t *testing.T) {
	for _, key := range []string{"model", "file", "response_format", "stream"} {
		t.Run(key, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(transcriptionRunDetailsBody))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			prep := diarizedRunPrep(t, portal.ChatTranscriptionSettings{
				Passthrough: map[string]json.RawMessage{key: json.RawMessage(`"x"`)},
			})
			run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != transcriptionRunReservedFieldMessage {
				t.Fatalf("error = %q, want %q", got, transcriptionRunReservedFieldMessage)
			}
			requireNoSpeechRunCall(t, calls)
		})
	}
}

// With Diarize OFF the run is the plain one, whatever the answer holds: the ordinary
// endpoint, a stream asked for, and the transcript as the text part -- an answer that
// happens to carry speaker turns (or a speaker_turns that is not even a list) is read for
// its text alone, as it always was.
func TestTranscriptionRunWithDiarizeOffIsThePlainRun(t *testing.T) {
	cases := []struct {
		name     string
		settings *portal.ChatTranscriptionSettings
		body     string
		want     string
	}{
		{"a two-speaker document", &portal.ChatTranscriptionSettings{Diarize: false, Language: "de"}, transcriptionRunDetailsBody, `[{"type":"text","text":"Mock transcription. Second speaker."}]`},
		{"a malformed speaker_turns", &portal.ChatTranscriptionSettings{Diarize: false}, `{"text":"hello","speaker_turns":"nope","sample_rate":"x"}`, `[{"type":"text","text":"hello"}]`},
		{"no transcription settings at all", nil, transcriptionRunDetailsBody, `[{"type":"text","text":"Mock transcription. Second speaker."}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "application/json", []byte(tc.body))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			prep := transcriptionRunPrep(t, "audio/wav", speechTestWAV())
			prep.Settings.Transcription = tc.settings
			_, snap := speechRunOutcome(t, srv, owner, chatID, prep)
			if snap.Status != "completed" || snap.Err != "" {
				t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
			}
			if string(snap.ContentParts) != tc.want {
				t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, tc.want)
			}
			got := takeSpeechRunCall(t, calls)
			if got.method != http.MethodPost || got.path != "/v1/audio/transcriptions" {
				t.Fatalf("request = %s %s, want POST /v1/audio/transcriptions", got.method, got.path)
			}
			fields := formFields(t, decodeTranscriptionForm(t, got.contentType, got.body))
			if fields["stream"] != "true" {
				t.Fatalf("form fields = %v, want the plain run to ask for stream=true", fields)
			}
			if _, sent := fields["diarize"]; sent {
				t.Fatalf("form fields = %v, Diarize is never a form field", fields)
			}
		})
	}
}
