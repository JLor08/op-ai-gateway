// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"math"
	"net/http"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
)

// transcriptionFrameDoneWithUsage builds the done event sglang-omni (v0.2.0
// speech_to_text.py speech_to_text_stream) ends a streamed transcription with: the whole
// text and a usage statement, whose raw JSON (usageJSON) the test chooses.
func transcriptionFrameDoneWithUsage(text, usageJSON string) string {
	return "data: {\"type\":\"transcript.text.done\",\"text\":" + jsonString(text) + ",\"usage\":" + usageJSON + "}\n\n"
}

// streamingTranscriptionMP3Parts is a valid streamed transcription request for model whose
// upload is an mp3: no WAV, so the upload's duration cannot be read from it.
func streamingTranscriptionMP3Parts(model string) []editPart {
	mp3 := append([]byte("ID3\x04\x00"), bytes.Repeat([]byte{0xFF, 0xFB}, 100)...)
	return []editPart{
		{name: "model", body: []byte(model)},
		{name: "file", filename: "clip.mp3", contentType: "audio/mpeg", body: mp3},
		{name: "stream", body: []byte("true")},
	}
}

// relayStreamedTranscription posts parts to a transcription server whose upstream streams
// frames, checks the client received them verbatim, and returns the one usage row of the
// relay and what it logged at Error.
func relayStreamedTranscription(t *testing.T, parts []editPart, frames ...string) (usage.Event, []logbuffer.Record) {
	t.Helper()
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up, _ := newSSETranscriptionUpstream(t, frames...)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, parts)

	if want := strings.Join(frames, ""); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("response = %d %q, want the upstream's stream, frame for frame", rec.Code, rec.Body.String())
	}
	ev := onlyUsageEvent(t, srv)
	if !ev.Stream {
		t.Fatalf("usage row stream = %v, want a streamed row", ev.Stream)
	}
	requireSpeechNonTokenRow(t, ev)
	return ev, errorLogs(buf)
}

// A completed stream whose upload's duration cannot be read from a WAV header is billed
// from the usage its done event states (sglang-omni, ADR-103): {"type":"duration",
// "seconds":N} with N a positive finite number, the one-shot rule. That closes the gaps
// the WAV measure leaves -- an upload that is no WAV, and a WAV whose header cannot be
// believed (the 0xFFFFFFFF data size a streaming writer leaves) -- and logs nothing.
func TestTranscriptionStreamBillsTheDoneEventsUsageWithoutAMeasurableWAV(t *testing.T) {
	streamedWAV := []editPart{
		{name: "model", body: []byte(transcriptionGatewayModel)},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: fallbackWAV(t, 0xFFFFFFFF, 0xFFFFFFFF, 48000)},
		{name: "stream", body: []byte("true")},
	}
	for _, tc := range []struct {
		name  string
		parts []editPart
		usage string
		want  float64
	}{
		{"mp3 upload, usage seconds 3", streamingTranscriptionMP3Parts(transcriptionGatewayModel), `{"type":"duration","seconds":3}`, 3},
		{"WAV with a 0xFFFFFFFF data size, usage seconds 2", streamedWAV, `{"type":"duration","seconds":2}`, 2},
		{"a fractional statement is a positive finite number too", streamingTranscriptionMP3Parts(transcriptionGatewayModel), `{"type":"duration","seconds":2.5}`, 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, errs := relayStreamedTranscription(t, tc.parts,
				transcriptionFrameDelta("hello"), transcriptionFrameDoneWithUsage("hello", tc.usage), transcriptionSSEEnd)
			if ev.Status != "success" || math.Abs(ev.BillingQuantity-tc.want) > 1e-9 {
				t.Fatalf("usage row = %q billing %v, want a success billing %v audio seconds from the done event's usage", ev.Status, ev.BillingQuantity, tc.want)
			}
			if len(errs) != 0 {
				t.Fatalf("a stream billed from its usage logged at Error: %+v", errs)
			}
		})
	}
}

// The uploaded WAV stays the first measure: a WAV whose header can be believed is billed
// by its own duration, whatever the done event's usage states.
func TestTranscriptionStreamBillsTheWAVOverTheDoneEventsUsage(t *testing.T) {
	half := buildPCMWav(t, 16000, 1, 16, 16000) // 0.5 s
	ev, errs := relayStreamedTranscription(t, streamingTranscriptionPartsOf(transcriptionGatewayModel, half),
		transcriptionFrameDelta("hello"), transcriptionFrameDoneWithUsage("hello", `{"type":"duration","seconds":9}`), transcriptionSSEEnd)
	if ev.Status != "success" || math.Abs(ev.BillingQuantity-0.5) > 1e-9 {
		t.Fatalf("usage row = %q billing %v, want a success billing the WAV's 0.5 s, not the usage's 9", ev.Status, ev.BillingQuantity)
	}
	if len(errs) != 0 {
		t.Fatalf("a stream billed from its WAV logged at Error: %+v", errs)
	}
}

// A completed stream with no WAV to measure and no usable usage statement is the loud
// zero of ADR-041, said once at Error -- whether the done event states no usage at all
// (audio.cpp's never does) or one that cannot be believed. A usage of a shape the gateway
// does not read never costs the done event itself: the stream still completed (the log is
// the one about the duration, not about a missing done event), and the client still got
// every frame. A delta's usage is no statement about the transcription: only the done
// event's counts.
func TestTranscriptionStreamLoudZeroOnAnUnusableDoneUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
		stated string // the log's usage field
	}{
		{"audio.cpp's done event: no usage", []string{transcriptionFrameDone("hi")}, "absent"},
		{"usage null", []string{transcriptionFrameDoneWithUsage("hi", `null`)}, "absent"},
		{"usage type tokens", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"tokens","seconds":3}`)}, "unusable"},
		{"usage without a type", []string{transcriptionFrameDoneWithUsage("hi", `{"seconds":3}`)}, "unusable"},
		{"usage seconds 0", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":0}`)}, "unusable"},
		{"usage seconds negative", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":-3}`)}, "unusable"},
		{"usage seconds a string", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":"3"}`)}, "unusable"},
		{"usage seconds null", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":null}`)}, "unusable"},
		{"usage seconds past float range", []string{transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":1e999}`)}, "unusable"},
		{"usage no object", []string{transcriptionFrameDoneWithUsage("hi", `"3 seconds"`)}, "unusable"},
		{"only a delta states a usage", []string{
			"data: {\"type\":\"transcript.text.delta\",\"delta\":\"hi\",\"usage\":{\"type\":\"duration\",\"seconds\":3}}\n\n",
			transcriptionFrameDone("hi"),
		}, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := append(append([]string{transcriptionFrameDelta("hi")}, tc.frames...), transcriptionSSEEnd)
			ev, errs := relayStreamedTranscription(t, streamingTranscriptionMP3Parts(transcriptionGatewayModel), frames...)
			if ev.Status != "success" || ev.BillingQuantity != 0 {
				t.Fatalf("usage row = %q billing %v, want a success that billed 0", ev.Status, ev.BillingQuantity)
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed transcription completed") || !strings.Contains(errs[0].Msg, "usage") {
				t.Fatalf("Error logs = %+v, want exactly one saying the completed stream stated no usable duration", errs)
			}
			if got := errs[0].Attrs["usage"]; got != tc.stated {
				t.Fatalf("the log's usage field = %v, want %q", got, tc.stated)
			}
		})
	}
}

// The done event's usage bills only a stream that COMPLETED (the done event, then
// [DONE]), as the WAV measure does: a stream that stopped after its done event, or sent
// [DONE] ahead of it, is the loud zero of an incomplete stream.
func TestTranscriptionStreamUsageBillsOnlyACompletedStream(t *testing.T) {
	done := transcriptionFrameDoneWithUsage("hi", `{"type":"duration","seconds":3}`)
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"the done event without [DONE]", []string{transcriptionFrameDelta("hi"), done}},
		{"[DONE] ahead of the done event", []string{transcriptionFrameDelta("hi"), transcriptionSSEEnd, done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, errs := relayStreamedTranscription(t, streamingTranscriptionMP3Parts(transcriptionGatewayModel), tc.frames...)
			if ev.Status != "success" || ev.BillingQuantity != 0 {
				t.Fatalf("usage row = %q billing %v, want a success that billed 0", ev.Status, ev.BillingQuantity)
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed transcription") || !strings.Contains(errs[0].Msg, "without") {
				t.Fatalf("Error logs = %+v, want exactly one saying the stream ended without its terminal events", errs)
			}
		})
	}
}

// The scan keeps the usage of the done event only, and of the last done event: a usage a
// delta carries is not read, and a later done event's statement -- none included --
// replaces an earlier one's.
func TestTranscriptionStreamScanReadsTheLastDoneEventsUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stream  string
		seconds float64
		stated  bool
	}{
		{"a usable statement", transcriptionFrameDoneWithUsage("x", `{"type":"duration","seconds":4}`) + transcriptionSSEEnd, 4, true},
		{"no statement", transcriptionFrameDone("x") + transcriptionSSEEnd, 0, false},
		{"an unusable statement", transcriptionFrameDoneWithUsage("x", `{"type":"duration","seconds":0}`) + transcriptionSSEEnd, 0, true},
		{"a delta's usage", "data: {\"type\":\"transcript.text.delta\",\"delta\":\"x\",\"usage\":{\"type\":\"duration\",\"seconds\":4}}\n\n" + transcriptionFrameDone("x") + transcriptionSSEEnd, 0, false},
		{"the last done event's statement", transcriptionFrameDoneWithUsage("x", `{"type":"duration","seconds":3}`) + transcriptionFrameDoneWithUsage("x", `{"type":"duration","seconds":4}`) + transcriptionSSEEnd, 4, true},
		{"a last done event that states none", transcriptionFrameDoneWithUsage("x", `{"type":"duration","seconds":3}`) + transcriptionFrameDone("x") + transcriptionSSEEnd, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{0, 1} {
				scan := scanOf(newTestTranscriptionScan(), tc.stream, size)
				if !scan.complete() || scan.usageSeconds != tc.seconds || scan.usageStated != tc.stated {
					t.Fatalf("complete = %v, usage = %v stated %v (chunks of %d), want a completed stream with usage %v stated %v",
						scan.complete(), scan.usageSeconds, scan.usageStated, size, tc.seconds, tc.stated)
				}
			}
		})
	}
}

// [DONE] ends a completed stream, and what follows it is no part of the transcription: a
// done event after it is not read, so it neither replaces the usage nor restates the text
// of the done event the stream completed with -- whether it states another usage or none.
func TestTranscriptionStreamScanIgnoresADoneEventAfterDONE(t *testing.T) {
	completed := transcriptionFrameDoneWithUsage("first", `{"type":"duration","seconds":3}`) + transcriptionSSEEnd
	for _, tc := range []struct {
		name  string
		after string
	}{
		{"one stating another usage", transcriptionFrameDoneWithUsage("second", `{"type":"duration","seconds":9}`)},
		{"one stating no usage", transcriptionFrameDone("second")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{0, 1} {
				scan := scanOf(newTestTranscriptionScan(), completed+tc.after, size)
				if !scan.complete() || scan.usageSeconds != 3 || !scan.usageStated || string(scan.transcript()) != "first" {
					t.Fatalf("complete = %v, usage = %v stated %v, transcript = %q (chunks of %d); want the completed stream's usage 3 and text %q",
						scan.complete(), scan.usageSeconds, scan.usageStated, scan.transcript(), size, "first")
				}
			}
		})
	}
}

// A done event whose usage is of a shape the gateway does not read is still the done
// event, to every reader of the shared decoder: the relay's scan (it completes the stream
// and keeps the text), the portal run's consumer (parseTranscriptionSSELine) and the live
// relay (liveDownMsgFromSSELine). The usage is read for billing only, and never makes the
// event it rides on unreadable.
func TestTranscriptionDoneEventWithAnUnreadableUsageStillDecodes(t *testing.T) {
	for _, usageJSON := range []string{`"3 seconds"`, `{"type":"duration","seconds":"3"}`, `{"type":7}`, `{"type":"duration","seconds":1e999}`, `[]`} {
		t.Run(usageJSON, func(t *testing.T) {
			frame := transcriptionFrameDoneWithUsage("hello", usageJSON)

			scan := scanOf(newTestTranscriptionScan(), frame+transcriptionSSEEnd, 0)
			if !scan.complete() || string(scan.transcript()) != "hello" || scan.usageSeconds != 0 || !scan.usageStated {
				t.Fatalf("scan complete = %v, transcript = %q, usage = %v stated %v; want a completed stream of %q with an unusable usage",
					scan.complete(), scan.transcript(), scan.usageSeconds, scan.usageStated, "hello")
			}

			line := strings.TrimSuffix(frame, "\n\n")
			event, kind := parseTranscriptionSSELine(line)
			if kind != transcriptionLineDone || event.Text == nil || *event.Text != "hello" {
				t.Fatalf("parseTranscriptionSSELine = %+v, %v; want the done event with text %q", event, kind, "hello")
			}
			msg, ok := liveDownMsgFromSSELine([]byte(line))
			if !ok || msg.Type != "done" || msg.Text != "hello" {
				t.Fatalf("liveDownMsgFromSSELine = %+v, %v; want a done message with text %q", msg, ok, "hello")
			}
		})
	}
}

// The usage statement adds nothing to what the shared decoder reports for audio.cpp's
// own frames: a delta and a done event without usage decode as they always did.
func TestTranscriptionEventDecodesAudioCppFramesUnchanged(t *testing.T) {
	event, ok := decodeTranscriptionEvent([]byte(`{"type":"transcript.text.done","text":"hi","timing":{"ttft_ms":12}}`))
	if !ok || event.Type != transcriptionEventDone || event.Text == nil || *event.Text != "hi" || event.Usage != nil {
		t.Fatalf("done event = %+v, %v; want type done, text hi and no usage", event, ok)
	}
	event, ok = decodeTranscriptionEvent([]byte(`{"type":"transcript.text.delta","delta":"h"}`))
	if !ok || event.Type != transcriptionEventDelta || event.Delta != "h" || event.Text != nil || event.Usage != nil {
		t.Fatalf("delta event = %+v, %v; want type delta, delta h, no text and no usage", event, ok)
	}
}
