// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/auth"
	"strings"
	"testing"
	"time"
)

// These tests cover Task 5 of Voice Chat phase 3: the server-side playback-control up-frames
// pause/resume and stop (spec 5.2 / 5.7 Q2). They pin the safety path -- the drain floor/ceiling
// time-shift, the mic gate, and the stop-vs-commit CAS race -- so the half-duplex guarantee holds
// across a paused answer and a stopped one.

// TestParseVoiceUpControlPlaybackControls: the three new playback-control frames parse as known
// controls, and an unknown/old type still does not (the newer-client tolerance is unchanged).
func TestParseVoiceUpControlPlaybackControls(t *testing.T) {
	for _, typ := range []string{voiceUpPause, voiceUpResume, voiceUpStop} {
		got, ok := parseVoiceUpControl([]byte(fmt.Sprintf(`{"type":%q}`, typ)))
		if !ok || got.Type != typ {
			t.Errorf("parseVoiceUpControl(%q) = (%+v, %v), want (%q, true)", typ, got, ok, typ)
		}
	}
	// A newer client may add fields to a playback control; the parser tolerates them.
	if got, ok := parseVoiceUpControl([]byte(`{"type":"pause","at_ms":1234}`)); !ok || got.Type != voiceUpPause {
		t.Errorf("parseVoiceUpControl(pause+extra) = (%+v, %v), want (pause, true)", got, ok)
	}
	// An unknown control still does not parse (an old gateway path must never be forced to act).
	if _, ok := parseVoiceUpControl([]byte(`{"type":"rewind"}`)); ok {
		t.Error("parseVoiceUpControl(rewind) = ok, want it rejected as unknown")
	}
}

// TestVoicePlaybackControlsNoopWhenIdle: pause and stop are no-ops outside playback (idle,
// listening, thinking) -- they touch no state and need no writer, so a bare machine survives them.
func TestVoicePlaybackControlsNoopWhenIdle(t *testing.T) {
	for _, st := range []string{voiceStateIdle, voiceStateListening, voiceStateThinking} {
		m := &voiceMachine{state: st}
		m.onPlaybackPause()
		if m.paused {
			t.Fatalf("onPlaybackPause in %q latched paused, want a no-op (no audio is playing)", st)
		}
		m.onPlaybackStop()
		if m.state != st {
			t.Fatalf("onPlaybackStop in %q changed state to %q, want a no-op", st, m.state)
		}
	}
}

// TestVoicePauseHoldsDrainFloorAcrossWallClock pins the time-shift at the unit level: a pause
// during speaking (before the drain wait begins) freezes the floor, so after resume the floor
// REMAINING is still ~what it was at pause -- wall-clock elapsed during the pause does not eat into
// it. This is what later forbids the mic reopening over audio that was paused.
func TestVoicePauseHoldsDrainFloorAcrossWallClock(t *testing.T) {
	m := &voiceMachine{state: voiceStateSpeaking}
	m.firstAudioAt = time.Now() // audio is being relayed/played
	m.relayedPlaybackDur = time.Second

	before := m.playbackFloorRemaining()
	m.onPlaybackPause()
	if !m.paused {
		t.Fatal("onPlaybackPause during speaking must latch paused")
	}
	time.Sleep(200 * time.Millisecond) // wall-clock passes while paused
	m.onPlaybackResume()
	if m.paused {
		t.Fatal("onPlaybackResume must clear paused")
	}
	after := m.playbackFloorRemaining()
	if after+50*time.Millisecond < before {
		t.Fatalf("floor remaining shrank across the pause (before=%v after=%v): firstAudioAt was not shifted forward", before, after)
	}
}

// TestVoiceStopRunArbitratesWithCommit pins stop's run handling against the one-way commit CAS: a
// stop of a still-running run claims the cancel (the run then cannot commit) and frees the chat
// slot, exactly as cancel does; a stop of a run that already won its commit leaves it alone to
// finish its WithoutCancel write and retire itself (no force-remove, so the in-flight commit and
// its slot are never stranded).
func TestVoiceStopRunArbitratesWithCommit(t *testing.T) {
	reg := NewChatRunRegistry(5)
	srv := &Server{ChatRuns: reg}

	t.Run("stop wins: the run cannot commit and the slot is freed", func(t *testing.T) {
		run, err := reg.add("usr_a", "chat_a", func() {})
		if err != nil {
			t.Fatal(err)
		}
		m := &voiceMachine{s: srv, curRun: run}
		if !m.stopRun() {
			t.Fatal("stopRun on a still-running run must report it cancelled")
		}
		if run.claimCommit() {
			t.Fatal("after stop cancelled the run it must NOT be able to claim its commit")
		}
		if reg.Get("usr_a", "chat_a") != nil {
			t.Fatal("stop must free the chat slot when it cancels the run")
		}
	})

	t.Run("commit wins: stop leaves the committing run to retire itself", func(t *testing.T) {
		run, err := reg.add("usr_b", "chat_b", func() {})
		if err != nil {
			t.Fatal(err)
		}
		if !run.claimCommit() {
			t.Fatal("claimCommit on an undecided run must win")
		}
		m := &voiceMachine{s: srv, curRun: run}
		if m.stopRun() {
			t.Fatal("stopRun must NOT report a cancel for a run that already won its commit")
		}
		if reg.Get("usr_b", "chat_b") != run {
			t.Fatal("stop must not remove a committing run from the registry: it retires itself")
		}
	})
}

// TestVoicePauseHoldsMicGatedPastFloorThenResume is the half-duplex pin for pause: a pause during
// the drain freezes the playout floor, so the mic STAYS gated past the point the un-paused floor
// would have elapsed; only after resume + the paused-shifted floor does the session return to
// listening -- never during the pause.
func TestVoicePauseHoldsMicGatedPastFloorThenResume(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"hold while paused"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// ~2s of audio at the fixture byte rate (88200 B/s) => a ~2s floor to hold the mic past.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200*2, 1)},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21) // the relayed audio_delta is large; read it whole
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "hold while paused" {
		t.Fatalf("asr_final = %q, want %q", f, "hold while paused")
	}
	// Count the relayed audio chunks so the playback_done carries an honest counter.
	relayed := 0
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == voiceDownAudioDelta {
			relayed++
		}
		if m.Type == voiceDownAudioDone {
			break
		}
	}
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	beforeConns, beforeBytes := child.snapshot()
	if beforeBytes != 320 {
		t.Fatalf("the one real chunk delivered %d bytes, want 320 (harness sanity)", beforeBytes)
	}

	// A valid playout-done signal (remembered, floor-clamped) plus a pause at the start of the drain.
	sendVoicePlaybackDone(t, ctx, conn, relayed)
	sendVoiceControl(t, ctx, conn, "pause")

	// Sleep PAST the un-paused ~2s floor. Because the pause froze the floor, the drain must not end
	// and the mic must stay gated: PCM sent here opens no loopback and reaches no ASR bytes.
	time.Sleep(2300 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"a pause did not hold the drain: the mic reopened past the un-paused floor")

	// Resume: the floor was shifted by the paused interval, so ~2s of floor remains from here.
	tResume := time.Now()
	sendVoiceControl(t, ctx, conn, "resume")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	if waited := time.Since(tResume); waited < time.Second {
		t.Fatalf("returned to listening %v after resume, want it held to the paused-shifted floor (~2s): the shift did not re-extend the floor", waited)
	}
}

// TestVoiceStopReturnsToListeningAndCommits: a stop while the answer is playing out (draining, after
// the run has committed) reopens the mic (returns to listening) and the turn stays committed with the
// sections synthesized so far -- the committed blobs are the full answer regardless of playback
// position.
func TestVoiceStopReturnsToListeningAndCommits(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"stop me"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "stop me" {
		t.Fatalf("asr_final = %q, want %q", f, "stop me")
	}
	// The run completes and commits; the session enters draining (mic gated, playing out).
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)

	// Stop: drop the rest of the queued audio and reopen the mic.
	sendVoiceControl(t, ctx, conn, "stop")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// The turn is committed with the sections synthesized so far: [user, assistant].
	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("stored messages = %+v, want [user, assistant] committed before the stop: %s", msgs, stored.Content)
	}
	if !strings.Contains(string(msgs[0].Content), "stop me") {
		t.Fatalf("user turn = %s, want the spoken text committed", msgs[0].Content)
	}

	// The mic is open again: a fresh PCM frame opens a new turn's ASR loopback and streams a partial.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
}

// TestVoiceStopLosingCommitRaceDoesNotStrandSlot is the stop-vs-commit race where the COMMIT wins:
// stop lands while the run is inside its WithoutCancel commit write (claimCommit already won). Stop
// must NOT force-detach it -- the commit finishes, the run retires itself, the turn is committed
// exactly once (no double-commit), and the chat slot is freed so the next turn reserves it.
func TestVoiceStopLosingCommitRaceDoesNotStrandSlot(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one", "two"}
	slow := newSlowCommitChatStore()
	timing := turnGraceTiming()
	timing.continuationGrace = 120 * time.Millisecond // close the window while the commit is blocked
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing, chatStore: slow})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Turn 1: the run reaches commit and BLOCKS in SaveChatWithBlobs -- its commit is claimed.
	if f := drainAChunk(t, ctx, conn); f != "one" {
		t.Fatalf("turn 1 final = %q, want %q", f, "one")
	}
	select {
	case <-slow.commitStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the commit write never started")
	}
	// The grace closes the window: the session is speaking while the commit is blocked.
	readVoiceUntilState(t, ctx, conn, voiceStateSpeaking)

	// Stop during the commit write: the committing run must be left to finish (no force-detach).
	sendVoiceControl(t, ctx, conn, "stop")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Release the commit; it completes and the run retires itself, freeing the slot (not stranded).
	close(slow.release)
	deadline := time.Now().Add(10 * time.Second)
	for h.srv.ChatRuns.Get("usr_dev", h.chatID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the committing run never retired: the chat slot is stranded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Exactly one committed turn so far (no double-commit).
	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, stored.Content)); n != 2 {
		t.Fatalf("after the stopped-but-committed turn the chat has %d messages, want 2 [user, assistant]: %s", n, stored.Content)
	}

	// The slot is reusable: a fresh turn runs and commits, for four messages total.
	if f := drainAChunk(t, ctx, conn); f != "two" {
		t.Fatalf("turn 2 final = %q, want %q", f, "two")
	}
	finishSpokenTurn(t, ctx, conn)
	stored, err = h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, stored.Content)); n != 4 {
		t.Fatalf("after a second turn the chat has %d messages, want 4: the slot was stranded or the turn double-committed: %s", n, stored.Content)
	}
}

// TestVoiceStopWinningCommitRacePersistsNothing is the stop-vs-commit race where STOP wins: stop
// lands while the run is at its commit gate but before it claimed the commit (the voiceBeforeCommitClaim
// seam holds it there). Stop claims the cancel, so the run's own claimCommit then fails and it
// persists NOTHING -- and the freed slot lets the next turn run.
func TestVoiceStopWinningCommitRacePersistsNothing(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one", "two"}
	timing := turnGraceTiming()
	timing.continuationGrace = 120 * time.Millisecond
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	release := make(chan struct{})
	h.srv.voiceBeforeCommitClaim = func(_ *ChatRun) { <-release } // hold the run just before claimCommit
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "one" {
		t.Fatalf("turn 1 final = %q, want %q", f, "one")
	}
	// The grace closes the window: speaking, with the run parked at its commit gate (undecided).
	readVoiceUntilState(t, ctx, conn, voiceStateSpeaking)

	// Stop: it claims the cancel before the run claims its commit.
	sendVoiceControl(t, ctx, conn, "stop")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Let the run proceed to its commit claim, which now loses: it must persist nothing.
	close(release)
	// Give the run time to reach claimCommit (fails) and finish its no-commit path.
	time.Sleep(200 * time.Millisecond)

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, stored.Content)); n != 0 {
		t.Fatalf("a stop that won the commit race committed %d messages, want none: %s", n, stored.Content)
	}

	// The slot was freed: a fresh turn runs and commits [user, assistant].
	if f := drainAChunk(t, ctx, conn); f != "two" {
		t.Fatalf("turn 2 final = %q, want %q", f, "two")
	}
	finishSpokenTurn(t, ctx, conn)
	stored, err = h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, stored.Content)); n != 2 {
		t.Fatalf("after the next turn the chat has %d messages, want 2: the slot was stranded by the stopped run: %s", n, stored.Content)
	}
}
