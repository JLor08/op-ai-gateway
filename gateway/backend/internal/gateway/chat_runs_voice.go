// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"time"
)

// This file is the Voice Chat run kind (spec 5.4, 6.1) -- "the one genuinely new
// chat-run behaviour". The orchestrator (voiceMachine, voice_chat_ws.go) starts one per
// chunk end with the accumulated ASR'd user text and three resolved targets; this run:
//
//  1. runs a text completion on the chosen LLM over the chat's history plus the
//     pending text, streaming its answer deltas to subscribers (runVoiceLLM);
//  2. synthesizes the WHOLE answer to speech on the chosen TTS model, streaming the
//     audio to subscribers (generateSpeechTurn, reused from the speech run); and
//  3. persists ONE assistant turn carrying the answer text AND its audio blobs,
//     ALONGSIDE the user turn, in a single commit (commitVoiceTurn).
//
// THE load-bearing divergence from every other run: the user turn is NOT committed
// up front (PrepareChatRun's shape). The run is speculative and cancellable via its
// context -- the orchestrator cancels it when the user resumes speaking within the
// continuation window (spec 6.1) -- and a cancelled run persists NOTHING: no user
// turn, no assistant turn, so cancel-and-extend needs no undo. Only on an
// un-cancelled finish are the user turn (pendingText) and the assistant turn
// committed together (portal.Service.CommitVoiceTurn, one SaveChatWithBlobs).
//
// Metering is NOT deferred, only persistence: LLM tokens are billed on the
// /v1/chat/completions leg and TTS on /v1/audio/speech, by those endpoints
// themselves, exactly as a normal chat run -- even for a cancelled run, because the
// tokens (and any synthesized audio) were produced upstream.

// voiceRunParams carries a voice run's inputs beyond the LLM leg's History/Settings
// (PrepareRunResult), set by startVoiceRun and read by executeVoiceRun. The LLM leg's
// model is PrepareRunResult.Settings.Model (the llm target's gateway model name).
type voiceRunParams struct {
	// pendingText is the ASR'd user text: the latest user message the LLM answers,
	// and the user turn persisted on an un-cancelled finalize.
	pendingText string
	// tts is the resolved streaming-speech target; its gateway model name is what the
	// TTS leg synthesizes the answer on.
	tts routing.Target
	// audio is the chat's speech settings (voice/language, #282), applied to the TTS
	// leg exactly as a speech thread's are.
	audio portal.ChatAudioSettings
}

// voiceRunRequest is what startVoiceRun is asked to run: the principal and the chat, the
// ASR'd user text of the turn, the three resolved targets of the loop, and the chat's
// speech settings.
type voiceRunRequest struct {
	owner       auth.Token
	chatID      string
	pendingText string
	// asr is accepted for symmetry -- the orchestrator resolves all three legs together --
	// but the run acts only on llm and tts: the per-turn ASR relay is the orchestrator's
	// own (spec 5.4), not the run's.
	asr routing.Target
	llm routing.Target
	tts routing.Target
	// audio is the chat's speech settings (voice/language, #282).
	audio portal.ChatAudioSettings
}

// startVoiceRun reserves a run and launches the voice executor for one turn. It reads
// the chat's history and appends pendingText in-memory for the LLM leg, but commits
// NOTHING up front (VoiceRunHistory is a read): persistence is deferred so a cancelled
// speculative run leaves nothing to undo (spec 6.1). The three targets are already
// resolved and co-load-gated by the caller (voiceCoLoadGate); this run reads only
// their gateway model names for the loopback bodies and lets the gateway re-resolve
// each leg with the run's own identity, exactly as every other run does.
//
// The returned run is cancellable through the registry (s.ChatRuns.cancelChat): the
// orchestrator cancels it on resumed speech, and a cancelled run commits nothing.
func (s *Server) startVoiceRun(ctx context.Context, req voiceRunRequest) (*ChatRun, error) {
	history, err := s.Portal.VoiceRunHistory(ctx, req.owner, req.chatID, req.pendingText)
	if err != nil {
		return nil, err
	}
	run, runCtx, err := s.reserveRun(req.owner, req.chatID)
	if err != nil {
		return nil, err
	}
	prep := PrepareRunResult{
		History: history,
		Settings: portal.ChatRunSettings{
			Model: req.llm.Model,
			Kind:  chatRunKindVoice,
		},
		Voice: &voiceRunParams{
			pendingText: req.pendingText,
			tts:         req.tts,
			audio:       req.audio,
		},
	}
	s.launchRun(runCtx, req.owner, run, prep)
	return run, nil
}

// executeVoiceRun is the deferred-commit, cancellable voice flow (see the file
// header). It calls into run persistence EXACTLY ONCE, on its single terminal path:
// a cancelled or failed run finishes with nothing written, and only an un-cancelled
// finish commits the user turn and the assistant turn together.
func (s *Server) executeVoiceRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	vp := prep.Voice
	if vp == nil {
		// Defensive: a voice-kind run with no voice params cannot be built, and
		// committing one would be guesswork. Fail without touching the chat.
		s.finishVoiceRunNoCommit(run, "error", "gateway.chat_run_voice_params_missing")
		return
	}

	// Leg 1: the speculative text completion. Deltas stream to subscribers; nothing
	// is checkpointed or committed (a cancelled run must leave no trace).
	answer, status, errMsg := s.runVoiceLLM(ctx, owner, run, prep)
	if status != "completed" {
		// A cancel (the orchestrator's resume) or an upstream error: persist nothing.
		// The LLM tokens were still billed on the /v1/chat/completions leg.
		s.finishVoiceRunNoCommit(run, status, errMsg)
		return
	}

	// Leg 2: synthesize the WHOLE answer to speech, streaming the audio to
	// subscribers. Reuses the speech run's generateSpeechTurn verbatim -- the only
	// difference is that the text it speaks is the LLM ANSWER, not a user prompt (see
	// voiceSpeechPrep). The TTS leg carries its own first-chunk deadline, like a
	// standalone streaming speech run (executeAudioRun).
	ttsCtx, cancelTTS := context.WithCancelCause(ctx)
	defer cancelTTS(nil)
	liftDeadline := armRunDeadline(chatRunKindSpeech, cancelTTS)
	defer liftDeadline()

	ttsPrep := voiceSpeechPrep(answer, vp.tts, vp.audio)
	audioParts, ttsStatus, ttsErr, streamed := s.generateSpeechTurn(ttsCtx, owner, run, ttsPrep, liftDeadline)
	if ttsStatus != "completed" {
		// Same rule as the LLM leg: a cancel or a TTS failure persists nothing. Any
		// audio already synthesized was billed on the /v1/audio/speech leg.
		s.finishVoiceRunNoCommit(run, ttsStatus, ttsErr)
		return
	}

	// The point of no return: ONE compare-and-swap (running->committing) that is both the
	// final cancellation gate and the commit claim, so a cancel and this commit arbitrate on
	// a single atomic word and exactly one wins. A cancel that arrived while the TTS leg
	// finished -- the continuation window can close on the very last frame -- has already
	// claimed the run (cancelContext claims BEFORE it cancels the context, and so does the
	// orchestrator's resume), so the claim fails and the chat stays untouched. A claim that
	// succeeds means no cancel won; from here the commit below runs under
	// context.WithoutCancel, and the orchestrator's own claimCancel fails, so it stops
	// accepting a resume-driven cancel-and-extend (the run's status stays "running" for the
	// whole write, so status alone would leave a window in which a resume double-commits).
	//
	// There is deliberately NO separate ctx.Err() read before this: such a check is a
	// read, and a cancel landing between it and the claim would still commit -- the
	// check-then-act gap the single CAS closes.
	if s.voiceBeforeCommitClaim != nil {
		s.voiceBeforeCommitClaim(run)
	}
	if !run.claimCommit() {
		s.finishVoiceRunNoCommit(run, "canceled", "")
		return
	}

	// The un-cancelled finish: commit the user turn and the assistant turn together.
	s.commitVoiceTurn(ctx, owner, run, vp.pendingText, answer, audioParts, streamed)
}

// runVoiceLLM runs the voice run's LLM leg: it POSTs the chat-completions body to the
// gateway's own /v1/chat/completions (the shared loopback header set) and consumes the
// SSE, publishing the answer deltas to the run's subscribers and measuring the run's
// metrics -- but it does NOT checkpoint and does NOT commit. It returns the assembled
// answer text and the run's terminal (status, errMsg): "completed" with the answer, a
// "canceled" on the orchestrator's cancel (an empty message), or "error" with the
// upstream's code.
//
// It is consumeRunStream's loop WITHOUT the checkpoint goroutine and the terminal
// commit: a voice run defers all persistence, and a checkpoint (an assistant-pending
// write) or a commit here would persist exactly what a cancelled run must not.
func (s *Server) runVoiceLLM(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) (answer, status, errMsg string) {
	body, err := buildChatCompletionsBody(prep)
	if err != nil {
		return "", "error", err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "error", err.Error()
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// A deadline is NOT a cancel (the voice kind arms none of its own here,
			// but a future one might), and reporting one as the other would tell the
			// user they pressed Stop when they pressed nothing.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", "error", runTimedOutMessage
			}
			return "", "canceled", ""
		}
		return "", "error", err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Bounded exactly as executeRun bounds a non-200 error body, and the
		// gateway's own error code is pulled out of its envelope.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
		return "", "error", upstreamErrorCode(b, resp.Status)
	}

	timings := runStreamTimings{start: time.Now()}
	status, errMsg = "completed", ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		ev, kind := parseChatSSELine(scanner.Text())
		switch kind {
		case sseDone:
			goto finish
		case sseError:
			// Prefer the upstream's STABLE code (what the voice orchestrator surfaces as the
			// degradation error frame's code, spec 6.6) over the human message, falling back
			// to the message when the stream carried no code.
			status, errMsg = "error", firstNonEmptyVoice(ev.ErrCode, ev.ErrMsg)
			goto finish
		case sseDelta:
			timings.observeDelta(run, ev)
		case sseUsage:
			timings.outputTokens = ev.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		status, errMsg = runStreamReadFailure(ctx, err)
	}
finish:
	timings.recordFinalRates(run)
	_, content := run.buffered()
	if status != "completed" {
		return "", status, errMsg
	}
	return content, status, errMsg
}

// voiceSpeechPrep builds the speech run's prep for the TTS leg: the model is the tts
// target's gateway name, the input is the LLM ANSWER (the last user message's text is
// what speechBodyFields reads as the input, so the answer rides as a single user
// message), and the audio settings are the ticket's voice/language. The kind is
// speech so generateSpeechTurn's streaming gate and billing behave exactly as a
// standalone speech run's.
func voiceSpeechPrep(answer string, tts routing.Target, audio portal.ChatAudioSettings) PrepareRunResult {
	settingsAudio := audio
	return PrepareRunResult{
		History: []portal.ChatAPIMessage{{Role: "user", Content: jsonStringRaw(answer)}},
		Settings: portal.ChatRunSettings{
			Model: tts.Model,
			Kind:  chatRunKindSpeech,
			Audio: &settingsAudio,
		},
	}
}

// commitVoiceTurn is the voice run's single terminal commit: it builds the assistant
// turn's content as [text, audio...] -- the answer text AND its synthesized audio
// parts, so one turn carries both -- and persists it together with the user turn
// (pendingText) via CommitVoiceTurn, in one SaveChatWithBlobs. On success the run's
// `done` event carries the committed parts; on a store failure the run ends an error
// turn with a mapped code and the parts are withheld, exactly as finishRunWithParts
// does for every other kind.
//
// The commit context is FRESH (WithoutCancel + a bound), like every other run's
// terminal commit (finishRunWithParts): once the un-cancelled finish is reached, the
// turn must be saved even though everything that makes the run terminal is below the
// commit. The "cancelled persists nothing" guarantee is enforced by the caller's
// cancellation gates, which never let a cancelled run reach this call.
func (s *Server) commitVoiceTurn(ctx context.Context, owner auth.Token, run *ChatRun, pendingText, answer string, audioParts json.RawMessage, streamed bool) {
	parts, err := voiceContentParts(answer, audioParts)
	if err != nil {
		log.Printf("voice run %s: build content parts failed: %v", run.ID, err)
		s.finishVoiceRunNoCommit(run, "error", chatRunCommitFailedMessage)
		return
	}

	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), chatRunCommitTimeout)
	defer cancelCommit()

	m := run.currentMetrics()
	if err := s.Portal.CommitVoiceTurn(commitCtx, owner, run.ChatID, pendingText, portal.AssistantTurn{
		Content:         answer,
		ContentParts:    parts,
		TTFTMs:          m.TTFTMs,
		ReasoningMs:     m.ReasoningMs,
		CharsPerSecond:  m.CharsPerSecond,
		TokensPerSecond: m.TokensPerSecond,
		// A voice answer IS an audio answer: it rides the #281 retention the same way
		// a speech turn does, and streamed records how it was delivered. A voice
		// thread sets no keep_last_audio, so the audio is stored (never the N=0
		// marker); should a future voice thread set it, retention applies correctly.
		AudioAnswer:   true,
		AudioStreamed: streamed,
	}, "complete"); err != nil {
		// A failed store write withholds the parts from the terminal event (nothing
		// was stored to render) and surfaces a mapped code, exactly as
		// finishRunWithParts does for every other kind.
		log.Printf("voice run %s: commit voice turn failed: %v", run.ID, err)
		run.finish("error", commitFailureCode(err), nil)
		s.ChatRuns.retire(run)
		return
	}
	run.finish("completed", "", parts)
	s.ChatRuns.retire(run)
}

// finishVoiceRunNoCommit ends a voice run WITHOUT any chat-document write: the cancel
// and error paths, where a speculative run must leave the chat untouched. It mirrors
// finishRunWithParts' terminal bookkeeping (run.finish then retire) minus the commit.
func (s *Server) finishVoiceRunNoCommit(run *ChatRun, status, errMsg string) {
	run.finish(status, errMsg, nil)
	s.ChatRuns.retire(run)
}

// voiceContentParts assembles a voice turn's content: the answer as a text part,
// followed by the synthesized audio parts (one or more audio_url parts from the TTS
// leg). One assistant turn thus carries BOTH the answer text and its audio, the shape
// the blob extraction (the audio_url parts become chat_blobs) and the portal renderer
// read. audioParts is the json array generateSpeechTurn returned.
func voiceContentParts(answer string, audioParts json.RawMessage) (json.RawMessage, error) {
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	text, err := json.Marshal(textPart{Type: "text", Text: answer})
	if err != nil {
		return nil, err
	}
	var audio []json.RawMessage
	if len(audioParts) > 0 {
		if err := json.Unmarshal(audioParts, &audio); err != nil {
			return nil, err
		}
	}
	combined := append([]json.RawMessage{json.RawMessage(text)}, audio...)
	return json.Marshal(combined)
}

// jsonStringRaw is a JSON-encoded string as a RawMessage (a message content that is a
// plain string). It cannot fail for a Go string, so the error is dropped.
func jsonStringRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
