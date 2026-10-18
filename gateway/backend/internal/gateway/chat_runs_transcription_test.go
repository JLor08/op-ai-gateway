// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// With no transcription settings the form is exactly the model and the clip: a
// thread that set no knob sends nothing extra, so audio.cpp stays on its own
// defaults (language detection, no prompt, its own temperature). response_format is
// deliberately absent too -- the endpoint answers JSON when it is absent and
// refuses every other value (validateTranscriptionRequest).
func TestBuildTranscriptionBodyWithoutSettingsIsModelAndFile(t *testing.T) {
	audio := speechTestWAV()
	parts := buildTranscriptionForm(t, transcriptionRunPrep(t, "audio/wav", audio), "audio/wav", audio)

	fields := formFields(t, parts)
	if got := fieldNames(fields); len(got) != 1 || fields["model"] != transcriptionGatewayModel {
		t.Fatalf("want exactly the model field %q, got %v", transcriptionGatewayModel, fields)
	}
	file := onlyFile(t, parts)
	if file.filename != "audio.wav" || file.contentType != "audio/wav" || !bytes.Equal(file.data, audio) {
		t.Fatalf("file part = %q (%s, %d bytes), want audio.wav / audio/wav / the clip's own %d bytes", file.filename, file.contentType, len(file.data), len(audio))
	}
	if len(parts) != 2 {
		t.Fatalf("want exactly two parts, got %d", len(parts))
	}
}

// An empty settings block is the same as none: nothing set, nothing sent.
func TestBuildTranscriptionBodyWithEmptySettingsIsModelAndFile(t *testing.T) {
	audio := speechTestWAV()
	prep := transcriptionRunPrep(t, "audio/wav", audio)
	prep.Settings.Transcription = &portal.ChatTranscriptionSettings{}
	parts := buildTranscriptionForm(t, prep, "audio/wav", audio)
	if got := fieldNames(formFields(t, parts)); len(got) != 1 || got[0] != "model" {
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
			want:     map[string]string{"model": transcriptionGatewayModel, "language": "de", "prompt": "Namen: Zoe, Jörg", "temperature": "0.25"},
		},
		{
			name:     "only the language",
			settings: portal.ChatTranscriptionSettings{Language: "de"},
			want:     map[string]string{"model": transcriptionGatewayModel, "language": "de"},
		},
		{
			name:     "only the prompt",
			settings: portal.ChatTranscriptionSettings{Prompt: "names"},
			want:     map[string]string{"model": transcriptionGatewayModel, "prompt": "names"},
		},
		{
			// A pointer exists for exactly this: an explicit 0 is a real, deterministic
			// setting, so it is forwarded, and the empty language/prompt are not.
			name:     "an explicit zero temperature",
			settings: portal.ChatTranscriptionSettings{Temperature: ptr(0.0)},
			want:     map[string]string{"model": transcriptionGatewayModel, "temperature": "0"},
		},
		{
			// The shortest decimal form, never an exponent a form reader may not take.
			name:     "a small temperature stays a plain decimal",
			settings: portal.ChatTranscriptionSettings{Temperature: ptr(0.00001)},
			want:     map[string]string{"model": transcriptionGatewayModel, "temperature": "0.00001"},
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

// A passthrough key the run owns -- the model, the clip, and the two the endpoint's
// contract (one complete JSON answer) depends on -- is REJECTED, ignoring case, and
// no body is returned. stream_format is deliberately not reserved: the endpoint
// refuses it with its own code, which the run then surfaces.
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
	if form.model != transcriptionGatewayModel || form.language != "de" || form.prompt != "names" || form.files != 1 || form.responseFormat != "" || form.stream != "" {
		t.Fatalf("parsed form = %+v", form)
	}
	if verr := validateTranscriptionRequest(form); verr != nil {
		t.Fatalf("validateTranscriptionRequest: %v", verr)
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
	if got := fieldNames(fields); len(got) != 1 || got[0] != "model" {
		t.Fatalf("want exactly the model field, got %v", got)
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
