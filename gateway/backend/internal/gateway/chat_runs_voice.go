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
	"sync"
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
//     <speak> scanner (voice_speak_scanner.go) taps the stream (runVoiceLLM); the
//     answer may carry N ordered <speak> regions (spec 5.1 Model A);
//  2. the MOMENT each <speak> region closes, fires that section's speech leg
//     CONCURRENTLY while the LLM keeps streaming (launchVoiceSpeech,
//     generateSpeechTurn reused from the speech run). At most the run's TTS
//     concurrency cap of legs synthesize at once (voiceSynthGate -- backpressure,
//     so a fast LLM cannot swamp the TTS), and their audio is relayed through ONE
//     ordered relay (relayVoiceSections) in document order; with no <speak> tag it
//     falls back to synthesizing the whole answer after the LLM finishes (spec
//     5.5); and
//  3. JOINS the LLM and EVERY section leg, then persists ONE assistant turn carrying
//     the full clean answer text, its spoken sections (text, span, audio) in document
//     order, ALONGSIDE the user turn, in a single commit (commitVoiceTurn).
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

	// The legs OVERLAP the LLM (spec 5.5), and a multi-<speak> answer fires ONE speech leg PER
	// region in document order (spec 5.1 Model A). A <speak> scanner (voice_speak_scanner.go)
	// taps the LLM stream: the clean, tag-stripped text is buffered and fanned out, and each time
	// a region closes its leg is launched CONCURRENTLY while the LLM keeps streaming. Only the
	// run's TTS concurrency cap of legs SYNTHESIZE at once (synth, below); the rest wait their turn,
	// so a fast LLM that closes many regions in a burst cannot swamp the TTS. The legs' audio is
	// RELAYED strictly in document order -- section k+1 waits for section k to finish relaying --
	// renumbered into ONE monotonic seq and section-tagged by relayVoiceSections, so the client
	// never sees seq restart mid-turn. ALL legs then join before the single commit. Nothing is
	// checkpointed or committed during the run -- a cancelled speculative run must leave no trace.
	speak := newSpeakScanner()
	// synth is the run's backpressure on the TTS: a bare/test Server (zero voiceTiming) reads as the
	// default cap, never 0. It is NOT the relay queue below -- that stays deep so document order never
	// depends on how many legs are synthesizing.
	synth := newVoiceSynthGate(s.voiceTiming.ttsConcurrencyCap())
	legsCh := make(chan *voiceSpeechLeg, voiceSectionQueue)
	var relayWG sync.WaitGroup
	relayWG.Add(1)
	go func() { defer relayWG.Done(); relayVoiceSections(run, legsCh) }()

	var legs []*voiceSpeechLeg
	// launchSection launches one section's speech leg: it takes the next section index, is handed
	// to the ordered relay (legsCh), queues for a synthesis slot (synth.queue, in document order --
	// it only registers the leg's place in line, so the LLM loop never blocks here) and synthesizes
	// once the slot is its. hasSection=false is the no-tag whole-answer fallback -- it carries audio
	// but records no spoken section.
	launchSection := func(text, lang string, hasSection bool, start, length int) {
		leg := newVoiceSpeechLeg(len(legs), text, hasSection, start, length, synth.queue())
		legs = append(legs, leg)
		legsCh <- leg
		s.launchVoiceSpeech(ctx, owner, run, vp, text, lang, leg)
	}

	// onRegionsClosed launches a leg for every region the scanner has closed but not yet handed to
	// a leg. ONE delta can close SEVERAL regions, so it iterates from the first unhandled region;
	// it runs in the LLM loop's OWN goroutine (runVoiceLLM calls it synchronously from the SSE
	// loop), so reading the scanner and the handled/legs state here races nothing -- the speech
	// goroutines it launches touch neither. An empty <speak></speak> region speaks nothing and
	// opens no section (spec 5.6); if NO region ever opens one, the whole answer is spoken by the
	// no-tag fallback below.
	handled := 0
	onRegionsClosed := func() {
		regions := speak.Regions()
		for ; handled < len(regions); handled++ {
			r := regions[handled]
			trimmed := strings.TrimSpace(r.Text)
			if trimmed == "" {
				continue
			}
			lang := s.voiceSpeechLanguage(ctx, owner, vp.tts.Model, r.Lang)
			launchSection(trimmed, lang, true, trimmedRegionStart(r), utf8.RuneCountInString(trimmed))
		}
	}

	// Leg 1: the speculative text completion, scanned and tag-stripped. answer is the FULL clean
	// text (every section + detail), even on an error path (a truncated answer). Each region
	// closing fires its leg above, concurrently with the still-streaming LLM.
	answer, status, errMsg := s.runVoiceLLM(ctx, owner, run, prep, speak, onRegionsClosed)

	// On a clean finish the scanner's Close() captured any UNCLOSED trailing region (spec 5.6);
	// fire its leg now so the captured part is spoken too. On an error/cancel Close() was not
	// called, so there is no new region and this is a no-op.
	onRegionsClosed()

	// No <speak> region produced speech on a completed answer: synthesize the WHOLE clean answer
	// (the phase-1/2 no-tag fallback), recorded with NO spoken section.
	if status == "completed" && len(legs) == 0 {
		launchSection(answer, "", false, 0, 0)
	}

	// Every leg is launched; close the relay feed and JOIN all legs (and the relay goroutine)
	// before the single commit/cancel arbitration. Joining is UNCONDITIONAL so a cancelled or
	// failed run never leaks a leg goroutine (each leg's context is derived from the run's, so a
	// cancel ends it promptly). The claimCommit CAS inside finalizeVoiceCommit stays the sole
	// arbitration point, now fired only after the LLM AND every leg have finished.
	close(legsCh)
	outcomes := joinVoiceLegs(legs)
	relayWG.Wait()

	switch {
	case status == "completed":
		finalStatus, finalErr := "completed", ""
		if code, failed := firstLegFailure(outcomes); failed {
			// A section's speech leg failed: the text turn STILL commits (with whatever sections
			// DID synthesize, their audio and spans), and the degrade is surfaced (spec 5.5). Any
			// audio synthesized before a failure was billed on the /v1/audio/speech leg.
			finalStatus, finalErr = "error", firstNonEmptyVoice(code, voiceRunFailedCode)
		}
		s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
			answer: answer, sections: outcomes, finalStatus: finalStatus, finalErr: finalErr,
		})

	case status == "error" && len(legs) > 0:
		// The LLM failed AFTER at least one </speak> (spec 5.5): the sections that closed were
		// already spoken, so the turn commits with what exists -- their text + audio, the detail
		// truncated -- and the LLM degrade is surfaced rather than leaving a half-turn.
		s.finalizeVoiceCommit(ctx, owner, run, vp, voiceTurnOutcome{
			answer: answer, sections: outcomes, finalStatus: "error", finalErr: errMsg,
		})

	default:
		// A cancel (the orchestrator's resume), or an LLM error BEFORE any region closed (no leg
		// fired): persist nothing, exactly as phase 1. The LLM tokens were still billed on the
		// /v1/chat/completions leg; any in-flight legs were already joined above.
		s.finishVoiceRunNoCommit(run, status, errMsg)
	}
}

// voiceSectionQueue bounds how many section legs can be queued for the ordered relay before the
// LLM loop (which launches them as regions close) applies backpressure -- far more than any real
// answer's <speak> count, so in practice the LLM never blocks handing a leg to the relay.
const voiceSectionQueue = 64

// voiceSectionAudioQueue bounds one section leg's audio handoff to the ordered relay. A leg that is
// not yet the relay's current section buffers up to this many chunks (so it can synthesize ahead --
// "ready early") before it applies backpressure to its own upstream read. Comfortably larger than a
// short section's chunk count, so the common case never blocks.
const voiceSectionAudioQueue = 64

// voiceSpeechLeg is one section's speech leg: its section index and spoken-section metadata (known
// at launch), its place in line for a synthesis slot (synth), its ordered audio handoff to
// relayVoiceSections (audioCh), and its terminal outcome (resultCh, buffered to one so the leg
// goroutine never blocks sending it). hasSection is false for the no-tag whole-answer fallback,
// which carries audio but records no spoken section.
type voiceSpeechLeg struct {
	section    int
	text       string
	start      int
	length     int
	hasSection bool
	synth      *voiceSynthTurn
	audioCh    chan runAudio
	resultCh   chan voiceSpeechResult
}

// newVoiceSpeechLeg builds a section leg with its buffered audio and result channels and the place
// in line (synth) it was queued at.
func newVoiceSpeechLeg(section int, text string, hasSection bool, start, length int, synth *voiceSynthTurn) *voiceSpeechLeg {
	return &voiceSpeechLeg{
		section: section, text: text, hasSection: hasSection, start: start, length: length,
		synth:    synth,
		audioCh:  make(chan runAudio, voiceSectionAudioQueue),
		resultCh: make(chan voiceSpeechResult, 1),
	}
}

// voiceSynthGate is a voice run's backpressure on its TTS: it caps how many section legs
// SYNTHESIZE at once (#337). Every <speak> region fires its own speech leg the moment it closes, so
// a fast LLM can close dozens in a burst; left alone they would all call /v1/audio/speech together
// and swamp the speech model. The cap is deliberately separate from voiceSectionQueue: that queue
// is the ordered relay's and stays deep, so document order never depends on how many legs are
// active; the gate only decides how many of the already-queued legs are talking to the TTS now.
//
// The slots are a plain buffered channel, but they are granted in DOCUMENT ORDER (voiceSynthTurn).
// Grabbing them in goroutine-scheduling order could hand every slot to LATER sections: those
// would fill their audio buffer (voiceSectionAudioQueue), block on the relay, and sit on the slots
// the EARLIER section -- the one the relay is waiting for -- needs in order to start at all.
type voiceSynthGate struct {
	slots chan struct{}
	// tail is the settled signal of the most recently queued leg: the next leg waits on it. Only the
	// goroutine that launches the legs calls queue, so it needs no lock.
	tail <-chan struct{}
}

// newVoiceSynthGate builds a gate admitting limit concurrent syntheses; limit is the run timing's
// ttsConcurrencyCap, which is always >= 1 (a gate of 0 would admit nothing and stall the run).
func newVoiceSynthGate(limit int) *voiceSynthGate {
	return &voiceSynthGate{slots: make(chan struct{}, max(limit, 1))}
}

// queue puts a new leg at the BACK of the line. It does not block -- the LLM loop calls it as
// regions close -- it only fixes the leg's place, so legs reach the slots in the order they were
// queued, which is document order.
func (g *voiceSynthGate) queue() *voiceSynthTurn {
	t := &voiceSynthTurn{gate: g, after: g.tail, settled: make(chan struct{})}
	g.tail = t.settled
	return t
}

// voiceSynthTurn is one leg's place in a voiceSynthGate's line. It is used by that leg's goroutine
// alone (acquire, then release), apart from settled, which the NEXT leg's goroutine waits on.
type voiceSynthTurn struct {
	gate *voiceSynthGate
	// after is the previous leg's settled signal (nil for the first leg): this leg does not queue for
	// a slot before the one ahead of it has settled its own.
	after   <-chan struct{}
	settled chan struct{}
	held    bool
}

// acquire waits for this leg's turn and then for a free slot, reporting false if ctx ended first
// (nothing is then held). It ALWAYS settles the turn, even on a cancel, so the legs queued behind
// never wait on one that gave up -- they see the cancel themselves and give up too. ctx is the RUN's
// context, not the leg's: the leg's first-chunk deadline must only start once it is synthesizing, so
// the time spent waiting behind earlier sections is not charged to it.
func (t *voiceSynthTurn) acquire(ctx context.Context) bool {
	defer close(t.settled)
	if ctx.Err() != nil {
		return false
	}
	if t.after != nil {
		select {
		case <-t.after:
		case <-ctx.Done():
			return false
		}
	}
	select {
	case t.gate.slots <- struct{}{}:
		t.held = true
		return true
	case <-ctx.Done():
		return false
	}
}

// release hands the slot back; a no-op for a turn that never held one, so it is safe on every exit
// path (including calling it twice).
func (t *voiceSynthTurn) release() {
	if t.held {
		t.held = false
		<-t.gate.slots
	}
}

// relayVoiceSections is the single ordered relay for a voice run's sections (spec 5.1 Model A): it
// drains each leg's audio IN DOCUMENT ORDER -- section k+1's audio is held in its buffer until
// section k's leg has finished relaying (its audioCh closed) -- assigning ONE monotonic seq across
// every leg and tagging each chunk with its section, then publishing it onto the run. Numbering the
// seq here, BEFORE the lossy fan-out, keeps a dropped chunk a visible gap (the client then stops
// live playback and falls back to the committed WAV), exactly as a single leg's reader did. It runs
// in its own goroutine, started before any leg, and returns once legsCh is closed and every leg's
// audio has drained.
func relayVoiceSections(run *ChatRun, legsCh <-chan *voiceSpeechLeg) {
	seq := 0
	for leg := range legsCh {
		for a := range leg.audioCh {
			a.Seq = seq
			a.Section = leg.section
			seq++
			run.publishAudio(a)
		}
	}
}

// joinVoiceLegs waits for every section leg to finish, in order, and gathers the ordered section
// outcomes (metadata + the leg's audio and terminal status). It blocks until the last leg reports,
// so it is the barrier that holds the commit until the LLM AND all legs have finished.
func joinVoiceLegs(legs []*voiceSpeechLeg) []sectionOutcome {
	outcomes := make([]sectionOutcome, 0, len(legs))
	for _, leg := range legs {
		res := <-leg.resultCh
		outcomes = append(outcomes, sectionOutcome{
			text: leg.text, start: leg.start, length: leg.length, hasSection: leg.hasSection,
			audioParts: res.parts, streamed: res.streamed, status: res.status, errMsg: res.errMsg,
		})
	}
	return outcomes
}

// firstLegFailure reports the first section leg that did not complete, and its error code, so a
// completed LLM with a failed speech leg degrades on that code (spec 5.5).
func firstLegFailure(outcomes []sectionOutcome) (code string, failed bool) {
	for _, o := range outcomes {
		if o.status != "completed" {
			return o.errMsg, true
		}
	}
	return "", false
}

// voiceSpeechResult is the outcome of the speech leg launched by launchVoiceSpeech, joined
// on the speechResult channel before the single voice commit.
type voiceSpeechResult struct {
	parts    json.RawMessage
	status   string
	errMsg   string
	streamed bool
}

// launchVoiceSpeech runs ONE section's TTS leg CONCURRENTLY with the still-streaming LLM leg and the
// other sections' legs: it synthesizes the section's text (a captured <speak> region, or, on the
// no-tag fallback, the whole answer) on the voice run's TTS target, routing the audio through the
// leg's ordered channel (drained in document order by relayVoiceSections) and sending its outcome on
// leg.resultCh (buffered to one, so the goroutine never blocks). The leg first waits for its
// synthesis slot (voiceSynthGate -- backpressure on the TTS, in document order), holds it for exactly
// the synthesis and gives it back on every way out, before the relay has drained the audio. It then
// carries its own first-chunk deadline, exactly like a standalone streaming speech run
// (executeAudioRun), armed only once the slot is its, and derives its context from the run's so a
// cancel ends it too.
func (s *Server) launchVoiceSpeech(ctx context.Context, owner auth.Token, run *ChatRun, vp *voiceRunParams, text, language string, leg *voiceSpeechLeg) {
	go func() {
		if !leg.synth.acquire(ctx) {
			// The run ended while this leg was still queued: it never synthesized, so it reports the
			// run's own outcome (a cancel, or a deadline) like a leg that was cut off mid-request.
			status, errMsg, _ := runContextOutcome(ctx)
			close(leg.audioCh)
			leg.resultCh <- voiceSpeechResult{status: status, errMsg: errMsg}
			return
		}
		defer leg.synth.release()
		ttsCtx, cancelTTS := context.WithCancelCause(ctx)
		defer cancelTTS(nil)
		liftDeadline := armRunDeadline(chatRunKindSpeech, cancelTTS)
		defer liftDeadline()
		prep := voiceSpeechPrep(text, vp.tts, vp.audio, language)
		// The leg hands each audio chunk to its ordered channel rather than straight onto the run,
		// so relayVoiceSections can serialize the sections into ONE monotonic stream. A send that
		// cannot be taken before the leg is cancelled is dropped (the live relay is best-effort --
		// the committed WAV is authoritative). stampTTFT=false: the LLM leg is streaming into the
		// same run.metrics concurrently, so the speech leg must NOT also write it (a lost update on
		// the perf metrics); the LLM leg is the sole metrics writer, keeping them deterministic.
		sink := func(a runAudio) {
			select {
			case leg.audioCh <- a:
			case <-ttsCtx.Done():
			}
		}
		parts, status, errMsg, streamed := s.generateSpeechTurn(ttsCtx, owner, run, prep, liftDeadline, false, sink)
		// Close the audio channel first (ending the relay's drain of this leg so it advances to the
		// next section), then report the terminal outcome.
		close(leg.audioCh)
		leg.resultCh <- voiceSpeechResult{parts: parts, status: status, errMsg: errMsg, streamed: streamed}
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

// sectionOutcome is one spoken section's committed result: its spoken text and rune span into the
// clean answer (known at launch) plus its leg's synthesized audio and terminal outcome (known at
// join). hasSection is false for the no-tag whole-answer fallback -- it carries audio but records
// no spoken section. A section whose leg produced no audio (status != completed, or an empty clip)
// has audioParts empty and contributes neither a spoken section nor an audio part at commit time.
type sectionOutcome struct {
	text       string
	start      int
	length     int
	hasSection bool
	audioParts json.RawMessage
	streamed   bool
	status     string
	errMsg     string
}

// voiceTurnOutcome is the committed voice turn's content and terminal status -- the payload
// finalizeVoiceCommit hands commitVoiceTurn, grouping the inputs that describe the turn being
// stored (as opposed to who/where it is stored for). answer is the FULL clean answer text (every
// section and the detail between them); sections are the turn's spoken sections IN DOCUMENT ORDER,
// each with its audio and span; finalStatus/finalErr are the run's terminal outcome on a SUCCESSFUL
// store ("completed", or "error" with a degrade code for a committed-but-degraded turn).
type voiceTurnOutcome struct {
	answer      string
	sections    []sectionOutcome
	finalStatus string
	finalErr    string
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
// reasoning deltas are untouched. Whenever a delta closes one or more <speak> regions it calls
// onRegionsClosed, which the caller uses to fire each newly closed section's speech leg
// concurrently, in document order. The returned answer is the clean buffered text, including on
// an error path (a truncated answer the error-after-close commit needs), with the scanner's tail
// flushed only on a clean finish.
func (s *Server) runVoiceLLM(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult, speak *speakScanner, onRegionsClosed func()) (answer, status, errMsg string) {
	resp, status, errMsg := s.dispatchVoiceLLM(ctx, owner, run, prep)
	if resp == nil {
		return "", status, errMsg
	}
	defer resp.Body.Close()
	return consumeVoiceLLMStream(ctx, run, resp.Body, speak, onRegionsClosed)
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
// published and buffered, and each </speak> fires onRegionsClosed so the caller can
// start that section's speech leg concurrently (see relayVoiceDelta). It measures the run's metrics but
// does NOT checkpoint or commit. It returns the clean buffered answer (including a truncated
// one on an error path, the error-after-close commit needs it) and the run's terminal
// (status, errMsg), flushing the scanner's held-back tail only on a clean finish.
func consumeVoiceLLMStream(ctx context.Context, run *ChatRun, body io.Reader, speak *speakScanner, onRegionsClosed func()) (answer, status, errMsg string) {
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
			relayVoiceDelta(run, speak, onRegionsClosed, ev, &timings)
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
// clean text to fan out (and to buffer) and whether a <speak> region just closed.
// Reasoning rides through unchanged. An all-held-back delta (clean "", no reasoning) is not
// published at all, so no empty delta and no <speak> prefix ever reaches a subscriber; whenever a
// </speak> closes on this delta it fires onRegionsClosed so the caller can launch the newly-closed
// sections' speech legs concurrently.
func relayVoiceDelta(run *ChatRun, speak *speakScanner, onRegionsClosed func(), ev sseDeltaEvent, timings *runStreamTimings) {
	// closedIdx is unused: onRegionsClosed reads speak.Regions() itself and launches a leg for every
	// region closed since its last call, so ONE delta closing several regions is handled in order.
	clean, _, closed := speak.Write(ev.Content)
	if clean != "" || ev.Reasoning != "" {
		scanned := ev
		scanned.Content = clean
		timings.observeDelta(run, scanned)
	}
	if closed && onRegionsClosed != nil {
		onRegionsClosed()
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

// trimmedRegionStart is the rune offset of a region's TRIMMED text within the clean answer. The
// scanner records where the region's content begins (RuneOffset), but each spoken section is
// persisted TrimSpace'd, so the offset is advanced past the leading whitespace the trim removed --
// so answer_runes[start : start+runeLen(trimmed)] is exactly the persisted section text, wherever
// the region sits in a multi-section answer.
func trimmedRegionStart(r speakRegion) int {
	leading := utf8.RuneCountInString(r.Text) - utf8.RuneCountInString(strings.TrimLeftFunc(r.Text, unicode.IsSpace))
	return r.RuneOffset + leading
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
// parts (each section's, in section order), so one turn carries both -- and persists it
// together with the user turn (pendingText) via CommitVoiceTurn, in one
// SaveChatWithBlobs. On success the run's `done` event carries the committed parts; on a
// store failure the run ends an error turn with a mapped code and the parts are
// withheld, exactly as finishRunWithParts does for every other kind.
//
// The commit context is FRESH (WithoutCancel + a bound), like every other run's
// terminal commit (finishRunWithParts): once the un-cancelled finish is reached, the
// turn must be saved even though everything that makes the run terminal is below the
// commit. The "cancelled persists nothing" guarantee is enforced by the caller's
// cancellation gates, which never let a cancelled run reach this call.
//
// outcome.sections are the turn's spoken sections IN DOCUMENT ORDER, each with its synthesized
// audio and span. The audio parts and the spoken-section spans are built TOGETHER so they stay
// aligned: the portal renderer pairs the n-th audio part to the n-th spoken section, so a section
// that produced no audio contributes NEITHER (never an audioless section wedged between audio-bearing
// ones). The no-tag fallback is the sole audio-without-section case: its whole-answer audio rides as
// the turn's only audio with no spoken_sections, exactly as a phase-1/2 turn. answer stays the FULL
// clean text (every section and the detail between them).
//
// finalStatus/finalErr are the run's terminal outcome on a SUCCESSFUL store write:
// "completed" with no error on the happy path, or "error" with a degrade code for a
// committed-but-degraded turn (the LLM cut off after a section, or a speech leg failed --
// spec 5.5). A FAILED store write ignores both and reports its own mapped code instead, and
// withholds the parts, exactly as finishRunWithParts does for every other kind.
func (s *Server) commitVoiceTurn(ctx context.Context, owner auth.Token, run *ChatRun, pendingText string, outcome voiceTurnOutcome) {
	// AudioAnswer is whether this turn actually carries synthesized audio. It is true on the
	// happy path and false on a degraded commit that carries only text (every speech leg failed,
	// or the LLM was cut off before any usable audio) -- so the #281 audio-answer retention never
	// acts on a turn that has no audio. AudioStreamed is true only when every audio-bearing section
	// streamed (a buffered-fallback section makes it false), so the lifecycle stores a not-heard
	// clip. A voice thread sets no keep_last_audio, so with audio present it is stored.
	var sections []portal.SpokenSection
	audioBySection := make([]json.RawMessage, 0, len(outcome.sections))
	audioAnswer, allStreamed := false, true
	for _, sec := range outcome.sections {
		if len(sec.audioParts) == 0 {
			continue // a section with no audio records no span either, keeping audio<->section aligned
		}
		audioBySection = append(audioBySection, sec.audioParts)
		audioAnswer = true
		if !sec.streamed {
			allStreamed = false
		}
		if sec.hasSection {
			sections = append(sections, portal.SpokenSection{
				Text:  sec.text,
				Start: sec.start,
				Len:   sec.length,
			})
		}
	}

	parts, err := voiceContentParts(outcome.answer, audioBySection)
	if err != nil {
		log.Printf("voice run %s: build content parts failed: %v", run.ID, err)
		s.finishVoiceRunNoCommit(run, "error", chatRunCommitFailedMessage)
		return
	}

	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), chatRunCommitTimeout)
	defer cancelCommit()

	m := run.currentMetrics()
	if err := s.Portal.CommitVoiceTurn(commitCtx, owner, run.ChatID, pendingText, portal.AssistantTurn{
		Content:         outcome.answer,
		ContentParts:    parts,
		TTFTMs:          m.TTFTMs,
		ReasoningMs:     m.ReasoningMs,
		CharsPerSecond:  m.CharsPerSecond,
		TokensPerSecond: m.TokensPerSecond,
		SpokenSections:  sections,
		AudioAnswer:     audioAnswer,
		AudioStreamed:   allStreamed && audioAnswer,
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
// followed by each spoken section's synthesized audio parts IN SECTION ORDER --
// [text, sec0 audio..., sec1 audio..., ...]. One assistant turn thus carries BOTH the
// answer text and all its audio, the shape the blob extraction (the audio_url parts
// become chat_blobs) and the portal renderer read; the renderer pairs the audio back to
// its section by that order (the sections themselves are recorded as spoken_sections).
//
// audioBySection has one entry per section: the json array generateSpeechTurn returned
// for that section's leg (one or more audio_url parts). A section with no audio (nil,
// empty, JSON null, or an empty array) contributes nothing and leaves its neighbours'
// order intact; an entry that is not a JSON array is an error.
func voiceContentParts(answer string, audioBySection []json.RawMessage) (json.RawMessage, error) {
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	text, err := json.Marshal(textPart{Type: "text", Text: answer})
	if err != nil {
		return nil, err
	}
	combined := []json.RawMessage{json.RawMessage(text)}
	for _, sectionAudio := range audioBySection {
		if len(sectionAudio) == 0 {
			continue
		}
		var audio []json.RawMessage
		if err := json.Unmarshal(sectionAudio, &audio); err != nil {
			return nil, err
		}
		combined = append(combined, audio...)
	}
	return json.Marshal(combined)
}

// jsonStringRaw is a JSON-encoded string as a RawMessage (a message content that is a
// plain string). It cannot fail for a Go string, so the error is dropped.
func jsonStringRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
