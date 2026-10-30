// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// These tests pin failTurn to the degradation contract (ADR-080 (h), #334): a recoverable
// leg failure (an ASR or loopback leg, as opposed to the run/LLM/TTS failures degrade
// handles) ends the turn exactly as degrade does -- the speculative run is cancelled and
// removed, the grace timer is disarmed, and the accumulated pendingText is dropped -- so
// neither an orphaned run nor a stale transcript can leak into the next turn.

// queuedVoiceErrorFrames drains the machine writer's outbound queue (the writer's run loop is
// never started in these machine-level tests, so every sent frame just sits in it) and returns
// the error down-frames, in order.
func queuedVoiceErrorFrames(t *testing.T, w *voiceWriter) []voiceDownMsg {
	t.Helper()
	var errs []voiceDownMsg
	for len(w.out) > 0 {
		var f voiceDownMsg
		if err := json.Unmarshal(<-w.out, &f); err != nil {
			t.Fatalf("down-frame is not JSON: %v", err)
		}
		if f.Type == voiceDownError {
			errs = append(errs, f)
		}
	}
	return errs
}

// TestVoiceFailTurnFromThinkingCancelsRunGraceAndPending: a leg failure while a speculative
// run is in flight (thinking) must cancel and detach that run, disarm the armed grace timer
// and drop pendingText, exactly like degrade -- before the fix it orphaned the run, left the
// grace timer feeding the run loop and let the abandoned transcript bleed into the next turn.
func TestVoiceFailTurnFromThinkingCancelsRunGraceAndPending(t *testing.T) {
	reg := NewChatRunRegistry(5)
	srv := &Server{ChatRuns: reg}
	run, err := reg.add("usr_a", "chat_a", func() {})
	if err != nil {
		t.Fatal(err)
	}
	sock := &fakeVoiceSocket{}
	m := &voiceMachine{
		s:      srv,
		state:  voiceStateThinking,
		ctx:    context.Background(),
		writer: newVoiceWriter(sock, func() {}),
		curRun: run,
	}
	m.runCh = make(chan runEvent, 1)
	unsubbed := false
	m.runUnsub = func() { unsubbed = true }
	// Arm the grace timer and leave stale run latches + pending text, as a real
	// thinking-state turn would have.
	m.graceTimer = time.NewTimer(time.Hour)
	m.graceC = m.graceTimer.C
	m.answerStreamed, m.graceElapsed = true, true
	m.pendingText = "abandoned transcript"

	m.failTurn("transcription.stream_error", "the transcription stream ended")

	if m.state != voiceStateIdle {
		t.Fatalf("state = %q, want idle", m.state)
	}
	if !unsubbed {
		t.Fatal("failTurn must unsubscribe the run (detachRun)")
	}
	if m.curRun != nil || m.runCh != nil || m.runUnsub != nil {
		t.Fatalf("run not detached: curRun=%v runCh=%v runUnsub set=%v", m.curRun, m.runCh, m.runUnsub != nil)
	}
	if reg.Get("usr_a", "chat_a") != nil {
		t.Fatal("failTurn must remove the cancelled run from the registry")
	}
	if run.claimCommit() {
		t.Fatal("the detached run's context must be cancelled so it can no longer commit")
	}
	if m.graceTimer != nil || m.graceC != nil {
		t.Fatal("failTurn must disarm the grace timer (stopGrace)")
	}
	if m.pendingText != "" {
		t.Fatalf("pendingText = %q, want empty (turn abandoned, never silently re-run)", m.pendingText)
	}
	if m.answerStreamed || m.graceElapsed || m.answerDoneSent || m.windowClosed || m.runFinished {
		t.Fatal("failTurn via detachRun must reset the run latches")
	}
	// Exactly one error frame, carrying the typed code.
	if got := queuedVoiceErrorFrames(t, m.writer); len(got) != 1 || got[0].Code != "transcription.stream_error" {
		t.Fatalf("error frames = %+v, want exactly one with code transcription.stream_error", got)
	}
}

// TestVoiceFailTurnFromListeningClearsPendingAndIsSafe: with no run in flight failTurn stays
// safe (it invents no run or grace timer) but now also drops the half-accumulated pendingText.
func TestVoiceFailTurnFromListeningClearsPendingAndIsSafe(t *testing.T) {
	srv := &Server{ChatRuns: NewChatRunRegistry(5)}
	sock := &fakeVoiceSocket{}
	m := &voiceMachine{
		s:           srv,
		state:       voiceStateListening,
		ctx:         context.Background(),
		writer:      newVoiceWriter(sock, func() {}),
		pendingText: "half a turn",
	}

	m.failTurn("transcription.unavailable", "live transcription unavailable")

	if m.state != voiceStateIdle {
		t.Fatalf("state = %q, want idle", m.state)
	}
	if m.curRun != nil || m.graceTimer != nil {
		t.Fatal("no run/grace existed; failTurn must not invent one")
	}
	if m.pendingText != "" {
		t.Fatalf("pendingText = %q, want empty", m.pendingText)
	}
	if got := queuedVoiceErrorFrames(t, m.writer); len(got) != 1 || got[0].Code != "transcription.unavailable" {
		t.Fatalf("error frames = %+v, want exactly one with code transcription.unavailable", got)
	}
}
