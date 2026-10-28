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
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file is the Voice Chat run kind (spec 5.4, 6.1) -- "the one genuinely new
// chat-run behaviour". The orchestrator (voiceMachine, voice_chat_ws.go) starts one per
// chunk end with the accumulated ASR'd user text and three resolved targets; this run:
//
//  1. runs a text completion on the chosen LLM over the chat's history plus the
//     pending text, streaming its TAG-STRIPPED answer deltas to subscribers while a
//     <speak> scanner (voice_speak_scanner.go) taps the stream (runVoiceLLM);
//  2. the MOMENT the first <speak> region closes, fires the speech leg CONCURRENTLY on
//     the captured summary while the LLM keeps streaming the detail (launchVoiceSpeech,
//     generateSpeechTurn reused from the speech run); with no <speak> tag it falls back
//     to synthesizing the whole answer after the LLM finishes (spec 5.5); and
//  3. once BOTH legs are done, persists ONE assistant turn carrying the full clean
//     answer text, its spoken summary and its audio blobs, ALONGSIDE the user turn, in
//     a single commit (commitVoiceTurn).
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

	// The two legs OVERLAP (spec 5.5). A <speak> scanner (voice_speak_scanner.go) taps the
	// LLM stream: the clean, tag-stripped text is what is buffered and fanned out, and the
	// moment the first <speak> region closes, the speech leg is fired CONCURRENTLY on the
	// captured summary (onSummaryClosed below) while the LLM keeps streaming the detail.
	// Both legs then join before the single commit. Nothing is checkpointed or committed
	// during either leg -- a cancelled speculative run must leave no trace.
	speak := newSpeakScanner()
	speechResult := make(chan voiceSpeechResult, 1)
	var speechFired bool
	var commitSummary string
	var commitSummaryStart int

	// onSummaryClosed fires the speech leg at </speak>. It runs in the LLM loop's OWN
	// goroutine (runVoiceLLM calls it synchronously from the SSE loop), so reading the
	// scanner and setting these shared flags here races nothing -- the speech goroutine it
	// launches never touches either the scanner or the flags. An empty <speak></speak>
	// region speaks nothing early and falls through to the whole-answer fallback (spec 5.6).
	onSummaryClosed := func() {
		if speechFired {
			return
		}
		summaryText, summaryLang := speak.Summary()
		trimmed := strings.TrimSpace(summaryText)
		if trimmed == "" {
			return
		}
		commitSummary = trimmed
		commitSummaryStart = trimmedSummaryStart(speak, summaryText)
		speechFired = true
		lang := s.voiceSpeechLanguage(ctx, owner, vp.tts.Model, summaryLang)
		s.launchVoiceSpeech(ctx, owner, run, vp, trimmed, lang, speechResult)
	}

	// Leg 1: the speculative text completion, scanned and tag-stripped. answer is the FULL
	// clean text (summary + detail), even on an error path (a truncated answer).
	answer, status, errMsg := s.runVoiceLLM(ctx, owner, run, prep, speak, onSummaryClosed)

	switch {
	case status == "completed":
		if !speechFired {
			// The region never closed WITH content, so nothing fired early. A captured-
			// but-unclosed summary is still spoken (spec 5.6); otherwise -- no tag, or an
			// empty region -- the whole answer is (the phase-1 ordered fallback).
			summaryText, summaryLang := speak.Summary()
			trimmed := strings.TrimSpace(summaryText)
			speechFired = true
			if trimmed != "" {
				commitSummary = trimmed
				commitSummaryStart = trimmedSummaryStart(speak, summaryText)
				lang := s.voiceSpeechLanguage(ctx, owner, vp.tts.Model, summaryLang)
				s.launchVoiceSpeech(ctx, owner, run, vp, trimmed, lang, speechResult)
			} else {
				commitSummary = ""
				s.launchVoiceSpeech(ctx, owner, run, vp, answer, "", speechResult)
			}
		}
		res := <-speechResult
		if res.status != "completed" {
			// The speech leg failed: the text turn STILL commits (no audio block) and the
			// degrade is surfaced (spec 5.5). Any audio synthesized before the failure was
			// billed on the /v1/audio/speech leg.
			s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
				answer:      answer,
				finalStatus: "error",
				finalErr:    firstNonEmptyVoice(res.errMsg, voiceRunFailedCode),
			})
			return
		}
		s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
			answer:       answer,
			summary:      commitSummary,
			summaryStart: commitSummaryStart,
			audioParts:   res.parts,
			streamed:     res.streamed,
			finalStatus:  "completed",
		})

	case status == "error" && speechFired:
		// The LLM failed AFTER </speak> (spec 5.5): the summary was already spoken, so the
		// turn is committed with what exists -- the summary text + its audio, the detail
		// truncated -- and the LLM degrade is surfaced rather than leaving a half-turn. If
		// the speech leg also failed there is no audio to commit, only the truncated text.
		res := <-speechResult
		if res.status == "completed" {
			s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
				answer:       answer,
				summary:      commitSummary,
				summaryStart: commitSummaryStart,
				audioParts:   res.parts,
				streamed:     res.streamed,
				finalStatus:  "error",
				finalErr:     errMsg,
			})
		} else {
			s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
				answer:      answer,
				finalStatus: "error",
				finalErr:    errMsg,
			})
		}

	default:
		// A cancel (the orchestrator's resume), or an LLM error BEFORE </speak> (no speech
		// fired): persist nothing, exactly as phase 1. The LLM tokens were still billed on
		// the /v1/chat/completions leg. Join any in-flight speech leg first (its context is
		// derived from the run's, so a cancel ends it promptly) to avoid a goroutine leak.
		if speechFired {
			<-speechResult
		}
		s.finishVoiceRunNoCommit(run, status, errMsg)
	}
}

// voiceSpeechResult is the outcome of the speech leg launched by launchVoiceSpeech, joined
// on the speechResult channel before the single voice commit.
type voiceSpeechResult struct {
	parts    json.RawMessage
	status   string
	errMsg   string
	streamed bool
}

// launchVoiceSpeech runs the TTS leg CONCURRENTLY with the still-streaming LLM leg: it
// synthesizes text -- the captured <speak> summary, or, on the no-tag fallback, the whole
// answer -- on the voice run's TTS target, streaming the audio to subscribers, and sends its
// outcome on result (buffered to one, so the goroutine never blocks). The leg carries its
// own first-chunk deadline, exactly like a standalone streaming speech run (executeAudioRun),
// and derives its context from the run's so a cancel ends it too.
func (s *Server) launchVoiceSpeech(ctx context.Context, owner auth.Token, run *ChatRun, vp *voiceRunParams, text, language string, result chan<- voiceSpeechResult) {
	go func() {
		ttsCtx, cancelTTS := context.WithCancelCause(ctx)
		defer cancelTTS(nil)
		liftDeadline := armRunDeadline(chatRunKindSpeech, cancelTTS)
		defer liftDeadline()
		prep := voiceSpeechPrep(text, vp.tts, vp.audio, language)
		// stampTTFT=false: the LLM leg is streaming into the same run.metrics concurrently, so
		// the speech leg must NOT also write it (a lost-update on the perf metrics). The LLM
		// leg is the sole metrics writer for a voice run, keeping the persisted metrics
		// deterministic.
		parts, status, errMsg, streamed := s.generateSpeechTurn(ttsCtx, owner, run, prep, liftDeadline, false)
		result <- voiceSpeechResult{parts: parts, status: status, errMsg: errMsg, streamed: streamed}
	}()
}

// voiceSpeechLanguage resolves the language the speech leg is asked to speak: the per-answer
// language the LLM requested via <speak lang>, kept only when the TTS model can take a
// `language` parameter (the cached portalSpeechLanguage lookup). Its error is treated as a
// "no" -- the field is omitted rather than failing the run -- exactly as the composer's gate
// degrades a lookup failure to a closed control.
func (s *Server) voiceSpeechLanguage(ctx context.Context, owner auth.Token, ttsModel, requested string) string {
	accepts, _, err := s.portalSpeechLanguage(ctx, owner, ttsModel)
	if err != nil {
		accepts = false
	}
	return effectiveSpeechLanguage(requested, accepts)
}

// voiceTurnOutcome is the committed voice turn's content and terminal status -- the payload
// finalizeVoiceCommit hands commitVoiceTurn, grouping the inputs that describe the turn being
// stored (as opposed to who/where it is stored for). answer is the FULL clean answer text
// (summary and detail); summary is the spoken <speak> part ("" when there is none); summaryStart
// is the rune offset of summary within answer (so the portal slices it out by position, meaningful
// only with a non-empty summary); audioParts
// are the synthesized audio parts (nil when none) and streamed records how they were delivered;
// finalStatus/finalErr are the run's terminal outcome on a SUCCESSFUL store ("completed", or
// "error" with a degrade code for a committed-but-degraded turn).
type voiceTurnOutcome struct {
	answer       string
	summary      string
	summaryStart int
	audioParts   json.RawMessage
	streamed     bool
	finalStatus  string
	finalErr     string
}

// finalizeVoiceCommit is the voice run's point of no return: ONE compare-and-swap
// (running->committing) that is both the final cancellation gate and the commit claim, so a
// cancel and this commit arbitrate on a single atomic word and exactly one wins. A cancel
// that arrived while the legs finished -- the continuation window can close on the very last
// frame -- has already claimed the run (cancelContext claims BEFORE it cancels the context,
// and so does the orchestrator's resume), so the claim fails and the chat stays untouched. A
// claim that succeeds means no cancel won; from there the commit runs under
// context.WithoutCancel, and the orchestrator's own claimCancel fails, so it stops accepting
// a resume-driven cancel-and-extend (the run's status stays "running" for the whole write, so
// status alone would leave a window in which a resume double-commits).
//
// There is deliberately NO separate ctx.Err() read before the claim: such a check is a read,
// and a cancel landing between it and the claim would still commit -- the check-then-act gap
// the single CAS closes.
//
// finalStatus/finalErr are the run's terminal outcome once the turn is STORED: "completed" on
// the happy path, or "error" with the degrade code for a committed-but-degraded turn (the LLM
// cut off after the summary, or the speech leg failed). A failed STORE write overrides either
// to its own mapped error code (commitVoiceTurn).
func (s *Server) finalizeVoiceCommit(ctx context.Context, owner auth.Token, run *ChatRun, vp *voiceRunParams, outcome voiceTurnOutcome) {
	if s.voiceBeforeCommitClaim != nil {
		s.voiceBeforeCommitClaim(run)
	}
	if !run.claimCommit() {
		s.finishVoiceRunNoCommit(run, "canceled", "")
		return
	}
	s.commitVoiceTurn(ctx, owner, run, vp.pendingText, outcome)
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
//
// It also TAPS the stream with speak (voice_speak_scanner.go): each content delta is fed
// through the scanner and only the clean, tag-stripped text is published (so run.content
// buffers the clean full text and NO <speak> markup is fanned out to subscribers);
// reasoning deltas are untouched. When the first <speak> region closes on a delta it calls
// onSummaryClosed, which the caller uses to fire the speech leg concurrently. The returned
// answer is the clean buffered text, including on an error path (a truncated answer the
// error-after-close commit needs), with the scanner's tail flushed only on a clean finish.
func (s *Server) runVoiceLLM(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult, speak *speakScanner, onSummaryClosed func()) (answer, status, errMsg string) {
	resp, status, errMsg := s.dispatchVoiceLLM(ctx, owner, run, prep)
	if resp == nil {
		return "", status, errMsg
	}
	defer resp.Body.Close()
	return consumeVoiceLLMStream(ctx, run, resp.Body, speak, onSummaryClosed)
}

// dispatchVoiceLLM POSTs the chat-completions body to the gateway's own
// /v1/chat/completions (the shared loopback header set) and returns the streaming response
// to consume. On any failure before the stream it returns a nil response and the run's
// terminal (status, errMsg): "canceled" on the orchestrator's cancel (an empty message), the
// timed-out message on a deadline (a deadline is NOT a cancel -- the voice kind arms none of
// its own here, but a future one might), or "error" with the upstream's code. A non-200
// closes the body before returning, exactly as the happy path's deferred close does.
func (s *Server) dispatchVoiceLLM(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) (resp *http.Response, status, errMsg string) {
	body, err := buildChatCompletionsBody(prep)
	if err != nil {
		return nil, "error", err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "error", err.Error()
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, "error", runTimedOutMessage
			}
			return nil, "canceled", ""
		}
		return nil, "error", err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		// Bounded exactly as executeRun bounds a non-200 error body, and the
		// gateway's own error code is pulled out of its envelope.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
		resp.Body.Close()
		return nil, "error", upstreamErrorCode(b, resp.Status)
	}
	return resp, "completed", ""
}

// consumeVoiceLLMStream reads the voice run's LLM SSE to its end, tapping it through the
// speak scanner (voice_speak_scanner.go): each content delta is tag-stripped before it is
// published and buffered, and the first </speak> fires onSummaryClosed so the caller can
// start the speech leg concurrently (see relayVoiceDelta). It measures the run's metrics but
// does NOT checkpoint or commit. It returns the clean buffered answer (including a truncated
// one on an error path, the error-after-close commit needs it) and the run's terminal
// (status, errMsg), flushing the scanner's held-back tail only on a clean finish.
func consumeVoiceLLMStream(ctx context.Context, run *ChatRun, body io.Reader, speak *speakScanner, onSummaryClosed func()) (answer, status, errMsg string) {
	timings := runStreamTimings{start: time.Now()}
	status, errMsg = "completed", ""
	sse := bufio.NewScanner(body)
	sse.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	stopped := false
	for sse.Scan() {
		ev, kind := parseChatSSELine(sse.Text())
		switch kind {
		case sseDone:
			stopped = true
		case sseError:
			// Prefer the upstream's STABLE code (what the voice orchestrator surfaces as the
			// degradation error frame's code, spec 6.6) over the human message, falling back
			// to the message when the stream carried no code.
			status, errMsg = "error", firstNonEmptyVoice(ev.ErrCode, ev.ErrMsg)
			stopped = true
		case sseDelta:
			relayVoiceDelta(run, speak, onSummaryClosed, ev, &timings)
		case sseUsage:
			timings.outputTokens = ev.OutputTokens
		}
		if stopped {
			break
		}
	}
	// A clean end of the loop (no [DONE] and no error frame) is a read outcome: a read
	// error or a cut stream. Reached only when the loop was NOT stopped by a terminal frame,
	// mirroring the original goto that skipped this on [DONE]/error.
	if !stopped {
		if err := sse.Err(); err != nil {
			status, errMsg = runStreamReadFailure(ctx, err)
		}
	}
	if status == "completed" {
		// Flush any bytes the scanner held back for a tag that never completed (an unclosed
		// "<spea" at EOF): they are plain text and belong in the clean answer. Only on a
		// clean finish -- an error/cancel truncates the answer where it stopped.
		if tail := speak.Close(); tail != "" {
			run.publish(sseDeltaEvent{Content: tail})
		}
	}
	timings.recordFinalRates(run)
	_, content := run.buffered()
	return content, status, errMsg
}

// relayVoiceDelta taps one content delta through the speak scanner: the scanner returns the
// clean text to fan out (and to buffer) and whether the first <speak> region just closed.
// Reasoning rides through unchanged. An all-held-back delta (clean "", no reasoning) is not
// published at all, so no empty delta and no <speak> prefix ever reaches a subscriber; the
// first </speak> fires onSummaryClosed so the caller can start the speech leg concurrently.
func relayVoiceDelta(run *ChatRun, speak *speakScanner, onSummaryClosed func(), ev sseDeltaEvent, timings *runStreamTimings) {
	clean, closed := speak.Write(ev.Content)
	if clean != "" || ev.Reasoning != "" {
		scanned := ev
		scanned.Content = clean
		timings.observeDelta(run, scanned)
	}
	if closed && onSummaryClosed != nil {
		onSummaryClosed()
	}
}

// voiceSpeechPrep builds the speech run's prep for the TTS leg: the model is the tts
// target's gateway name, the input is the LLM ANSWER (the last user message's text is
// what speechBodyFields reads as the input, so the answer rides as a single user
// message), and the audio settings are the ticket's voice and speed with the per-answer
// language. The language is the one the caller already resolved (see
// effectiveSpeechLanguage), set verbatim: "" leaves Language unset so speechBodyFields
// sends no `language` member and the model speaks in its own default. The kind is
// speech so generateSpeechTurn's streaming gate and billing behave exactly as a
// standalone speech run's.
func voiceSpeechPrep(answer string, tts routing.Target, audio portal.ChatAudioSettings, language string) PrepareRunResult {
	settingsAudio := audio
	settingsAudio.Language = language
	return PrepareRunResult{
		History: []portal.ChatAPIMessage{{Role: "user", Content: jsonStringRaw(answer)}},
		Settings: portal.ChatRunSettings{
			Model: tts.Model,
			Kind:  chatRunKindSpeech,
			Audio: &settingsAudio,
		},
	}
}

// trimmedSummaryStart is the rune offset of the TRIMMED spoken summary within the clean answer.
// The scanner records where the <speak> region's content begins (SummaryStart), but the persisted
// summary is TrimSpace'd, so the offset is advanced past the leading whitespace that trim removed
// -- so answer_runes[offset : offset+len(summary)] is exactly the persisted summary, wherever the
// region sits. raw is the untrimmed region text (speak.Summary()); the caller only reaches here
// when it is non-empty, so SummaryStart is >= 0.
func trimmedSummaryStart(speak *speakScanner, raw string) int {
	start := speak.SummaryStart()
	if start < 0 {
		return start
	}
	leading := utf8.RuneCountInString(raw) - utf8.RuneCountInString(strings.TrimLeftFunc(raw, unicode.IsSpace))
	return start + leading
}

// effectiveSpeechLanguage is the language the speech leg is actually asked to speak: the
// per-answer language the LLM requested via <speak lang>, but only when the TTS model can
// take a `language` parameter (modelAccepts, the cached portalSpeechLanguage lookup). A model
// that cannot gets "" so the field is omitted rather than sent to be dropped or rejected
// upstream, and nothing requested stays "". The code is returned verbatim -- "de-DE" is not
// normalized to "de" -- since which spellings a model honors is the model's business.
func effectiveSpeechLanguage(requested string, modelAccepts bool) string {
	if !modelAccepts {
		return ""
	}
	return requested
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
//
// summary is the text of the answer's spoken summary (the <speak> part), recorded on the
// assistant turn as additional metadata so the portal can re-render it on reload; answer
// stays the FULL clean text, summary and detail both. "" -- no summary -- leaves the stored
// turn exactly as a turn without one (the no-tag fallback, and every commit that carries no
// audio, passes "").
//
// finalStatus/finalErr are the run's terminal outcome on a SUCCESSFUL store write:
// "completed" with no error on the happy path, or "error" with a degrade code for a
// committed-but-degraded turn (the LLM cut off after the summary, or the speech leg failed --
// spec 5.5). A FAILED store write ignores both and reports its own mapped code instead, and
// withholds the parts, exactly as finishRunWithParts does for every other kind.
func (s *Server) commitVoiceTurn(ctx context.Context, owner auth.Token, run *ChatRun, pendingText string, outcome voiceTurnOutcome) {
	parts, err := voiceContentParts(outcome.answer, outcome.audioParts)
	if err != nil {
		log.Printf("voice run %s: build content parts failed: %v", run.ID, err)
		s.finishVoiceRunNoCommit(run, "error", chatRunCommitFailedMessage)
		return
	}

	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), chatRunCommitTimeout)
	defer cancelCommit()

	m := run.currentMetrics()
	// AudioAnswer is whether this turn actually carries synthesized audio. It is true on the
	// happy path and false on a degraded commit that carries only text (the speech leg failed,
	// or the LLM was cut off after </speak> with no usable audio) -- so the #281 audio-answer
	// retention never acts on a turn that has no audio. A voice thread sets no keep_last_audio,
	// so with audio present it is stored (never the N=0 marker); should a future voice thread
	// set it, retention then applies to the right turns. streamed records how it was delivered.
	audioAnswer := len(outcome.audioParts) > 0
	if err := s.Portal.CommitVoiceTurn(commitCtx, owner, run.ChatID, pendingText, portal.AssistantTurn{
		Content:            outcome.answer,
		ContentParts:       parts,
		TTFTMs:             m.TTFTMs,
		ReasoningMs:        m.ReasoningMs,
		CharsPerSecond:     m.CharsPerSecond,
		TokensPerSecond:    m.TokensPerSecond,
		SpokenSummary:      outcome.summary,
		SpokenSummaryStart: outcome.summaryStart,
		AudioAnswer:        audioAnswer,
		AudioStreamed:      outcome.streamed && audioAnswer,
	}, "complete"); err != nil {
		// A failed store write withholds the parts from the terminal event (nothing
		// was stored to render) and surfaces a mapped code, exactly as
		// finishRunWithParts does for every other kind.
		log.Printf("voice run %s: commit voice turn failed: %v", run.ID, err)
		run.finish("error", commitFailureCode(err), nil)
		s.ChatRuns.retire(run)
		return
	}
	run.finish(outcome.finalStatus, outcome.finalErr, parts)
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
