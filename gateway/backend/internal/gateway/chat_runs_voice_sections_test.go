// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
	"time"
)

// Phase 3 Task 4: N ordered speech legs, one serialized audio stream, commit-all.
//
// A voice answer with several <speak> regions fires ONE speech leg per region in document
// order. The legs synthesize concurrently, but their audio is RELAYED strictly in order --
// section k+1 waits for section k to finish relaying -- renumbered into a single monotonic seq
// (so the client never sees seq restart mid-turn) and tagged with the section it belongs to.
// The single turn commits only once the LLM AND every leg have finished.

// twoRegionScript is one LLM delta carrying two <speak> regions (closed in the SAME delta, the
// several-regions-per-delta case): clean answer "Eins. Mitte. Zwei. Schluss.", section 0 "Eins."
// at rune 0 and section 1 "Zwei." at rune 13.
var twoRegionScript = []scriptedLLMDelta{
	{text: `<speak lang="de">Eins.</speak> Mitte. <speak lang="de">Zwei.</speak> Schluss.`},
}

const (
	twoRegionAnswer = "Eins. Mitte. Zwei. Schluss."
	twoRegionSec0   = "Eins."
	twoRegionSec1   = "Zwei."
)

// collectRunAudio subscribes to a reserved voice run, launches it and drains its events to the
// terminal, returning the audio chunks it relayed in order.
func collectRunAudio(t *testing.T, srv *Server, owner auth.Token, chatID, pending string) []runAudio {
	t.Helper()
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, pending)
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	run, runCtx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	defer unsub()
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: pending, tts: voiceTTSTarget},
	})
	return drainRunAudio(t, ch)
}

// drainRunAudio reads a run's subscription to the terminal and returns the audio chunks relayed.
func drainRunAudio(t *testing.T, ch <-chan runEvent) []runAudio {
	t.Helper()
	var audio []runAudio
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				return audio
			}
			if ev.Event == "audio" && ev.Audio != nil {
				audio = append(audio, *ev.Audio)
			}
		case <-timeout:
			t.Fatal("the voice run published no terminal event within 10s")
		}
	}
}

// waitForSpeechInput drains speech request bodies until it sees one synthesizing `want`, so a test
// can wait for a specific section's leg to have fired. Bodies it reads on the way are discarded.
func waitForSpeechInput(t *testing.T, bodies <-chan []byte, want string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case b := <-bodies:
			if speechInputOf(b) == want {
				return
			}
		case <-timeout:
			t.Fatalf("no speech leg synthesized %q within 10s", want)
		}
	}
}

// assertOneMonotonicStream fails unless the relayed audio is one contiguous seq from 0 with the
// given per-chunk section tags -- the client-visible invariant that seq never restarts mid-turn.
func assertOneMonotonicStream(t *testing.T, audio []runAudio, wantSections []int) {
	t.Helper()
	if len(audio) != len(wantSections) {
		t.Fatalf("relayed %d audio chunks, want %d", len(audio), len(wantSections))
	}
	for i, a := range audio {
		if a.Seq != i {
			t.Fatalf("audio[%d].Seq = %d, want %d (one monotonic stream, never restarting at 0)", i, a.Seq, i)
		}
		if a.Section != wantSections[i] {
			t.Fatalf("audio[%d].Section = %d, want %d", i, a.Section, wantSections[i])
		}
	}
}

// Two <speak> regions fire two speech legs in document order; their audio relays as ONE monotonic
// seq (0..3, never restarting) tagged section 0,0,1,1; and the single committed turn carries both
// spoken sections in order (with their spans) and both sections' audio after the text, in order.
func TestVoiceRun_TwoRegionsRelayOneStreamAndCommitBoth(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = twoRegionScript
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)
	rec := &recordingVoicePortal{API: srv.Portal}
	srv.Portal = rec

	audio := collectRunAudio(t, srv, owner, chatID, "unused")

	// One monotonic stream, section-tagged in document order.
	assertOneMonotonicStream(t, audio, []int{0, 0, 1, 1})

	// Both legs fired, one per region's text.
	inputs := map[string]bool{}
	for {
		select {
		case b := <-speechBodies:
			inputs[speechInputOf(b)] = true
			continue
		default:
		}
		break
	}
	if !inputs[twoRegionSec0] || !inputs[twoRegionSec1] {
		t.Fatalf("speech inputs = %v, want one leg per region (%q and %q)", inputs, twoRegionSec0, twoRegionSec1)
	}

	// The committed turn: two spoken sections in document order with exact spans.
	turn, ok := rec.committed()
	if !ok {
		t.Fatal("no voice commit was captured")
	}
	wantSpans := []portal.SpokenSection{
		{Text: twoRegionSec0, Start: 0, Len: 5},
		{Text: twoRegionSec1, Start: 13, Len: 5},
	}
	if len(turn.SpokenSections) != 2 || turn.SpokenSections[0] != wantSpans[0] || turn.SpokenSections[1] != wantSpans[1] {
		t.Fatalf("SpokenSections = %+v, want %+v (document order with spans)", turn.SpokenSections, wantSpans)
	}

	// The content parts: the full clean answer, then each section's audio IN ORDER.
	parts := contentPartSummary(t, turn.ContentParts)
	if len(parts) != 3 || parts[0] != "text:"+twoRegionAnswer || !strings.HasPrefix(parts[1], "audio:") || !strings.HasPrefix(parts[2], "audio:") {
		t.Fatalf("content parts = %v, want [text, sec0 audio, sec1 audio] in order", parts)
	}

	// The committed document really carries both audio parts as blobs.
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(stored.Content), `"type":"audio_url"`); n != 2 {
		t.Fatalf("stored turn carries %d audio parts, want 2 (one per section): %s", n, stored.Content)
	}
}

// An early-finishing LATER section still relays after the earlier one: section 1's leg synthesizes
// and buffers its whole clip while section 0's response is HELD, yet the first relayed chunk is
// section 0's -- the relay is serialized in document order, not completion order.
func TestVoiceRun_EarlyReadyLaterSectionRelaysAfterEarlier(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = twoRegionScript
	hold := make(chan struct{})
	prov.speechHoldByInput = map[string]chan struct{}{twoRegionSec0: hold} // hold section 0's response
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)

	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, "unused")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	run, runCtx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	defer unsub()
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: "unused", tts: voiceTTSTarget},
	})

	// Section 1's leg has fired (its request arrived) while section 0 is held: it synthesizes and
	// buffers its clip -- "ready early" -- but may not relay before section 0.
	waitForSpeechInput(t, speechBodies, twoRegionSec1)
	close(hold) // release section 0; now both sections can relay, in order

	audio := drainRunAudio(t, ch)
	if len(audio) == 0 {
		t.Fatal("no audio was relayed")
	}
	if audio[0].Section != 0 {
		t.Fatalf("first relayed chunk Section = %d, want 0: the later section was ready early but must relay after the earlier one", audio[0].Section)
	}
	assertOneMonotonicStream(t, audio, []int{0, 0, 1, 1})
}

// The single turn commits only after the LLM AND every leg have finished: with section 1's leg held
// in flight (the LLM done, section 0 done), the run must stay running and persist nothing; releasing
// section 1 then lets the one commit go through.
func TestVoiceRun_CommitWaitsForAllLegs(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = twoRegionScript
	hold := make(chan struct{})
	prov.speechHoldByInput = map[string]chan struct{}{twoRegionSec1: hold} // hold the SECOND leg
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)
	released := false
	t.Cleanup(func() {
		if !released {
			close(hold)
		}
	})

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.startVoiceRun(context.Background(), voiceRunRequest{owner: owner, chatID: chatID, pendingText: "unused", asr: voiceASRTarget, llm: voiceLLMTarget, tts: voiceTTSTarget})
	if err != nil {
		t.Fatalf("startVoiceRun: %v", err)
	}

	// Section 2's leg has fired and is held; the LLM and section 1's leg finish on their own. The
	// run cannot commit while section 2 is joined, so it stays running with nothing persisted.
	waitForSpeechInput(t, speechBodies, twoRegionSec1)
	time.Sleep(150 * time.Millisecond)
	if got := run.statusValue(); got != "running" {
		t.Fatalf("status = %q while a leg is still in flight, want running (the commit must wait for ALL legs)", got)
	}
	mid, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Content) != string(mid.Content) {
		t.Fatalf("the turn committed before all legs finished:\nbefore %s\nmid    %s", before.Content, mid.Content)
	}

	close(hold)
	released = true
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q after releasing the last leg, want completed", got)
	}
	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, after.Content)); n != 2 {
		t.Fatalf("after the turn the chat has %d messages, want [user, assistant]: %s", n, after.Content)
	}
}

// An unclosed trailing <speak> region (the model forgot to close it) is spoken as a final section:
// the captured part is synthesized and recorded as its own spoken section.
func TestVoiceRun_UnclosedTrailingRegionIsAFinalSection(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = []scriptedLLMDelta{
		{text: `<speak lang="de">Eins.</speak> Mitte. <speak lang="de">Zwei.`},
	}
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)
	rec := &recordingVoicePortal{API: srv.Portal}
	srv.Portal = rec

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "unused")
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	inputs := map[string]bool{}
	for {
		select {
		case b := <-speechBodies:
			inputs[speechInputOf(b)] = true
			continue
		default:
		}
		break
	}
	if !inputs[twoRegionSec0] || !inputs[twoRegionSec1] {
		t.Fatalf("speech inputs = %v, want both the closed and the unclosed trailing region spoken", inputs)
	}
	turn, ok := rec.committed()
	if !ok {
		t.Fatal("no voice commit was captured")
	}
	if len(turn.SpokenSections) != 2 || turn.SpokenSections[0].Text != twoRegionSec0 || turn.SpokenSections[1].Text != twoRegionSec1 {
		t.Fatalf("SpokenSections = %+v, want the closed region and the unclosed trailing region", turn.SpokenSections)
	}
}
