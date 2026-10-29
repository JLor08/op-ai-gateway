// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"strings"
	"sync"
	"sync/atomic"
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

// assertOneMonotonicStream fails unless the relayed audio is one contiguous seq from 0 whose REAL
// chunks carry the given per-chunk section tags -- the client-visible invariant that seq never
// restarts mid-turn. The relay's keepalive silence (#339) is numbered into the same seq, so it takes
// part in the contiguity check, but it is not one of the chunks these tests are about: a run whose
// TTS is slow for a moment (a retry's backoff, a section waiting for its synthesis slot) legitimately
// has some. What it must never do is break the stream, and that is checked here.
func assertOneMonotonicStream(t *testing.T, audio []runAudio, wantSections []int) {
	t.Helper()
	var sections []int
	for i, a := range audio {
		if a.Seq != i {
			t.Fatalf("audio[%d].Seq = %d, want %d (one monotonic stream, never restarting at 0)", i, a.Seq, i)
		}
		if !isSilenceFrame(t, a) {
			sections = append(sections, a.Section)
		}
	}
	if len(sections) != len(wantSections) {
		t.Fatalf("relayed %d real audio chunks, want %d (%d frames in all, silence included)", len(sections), len(wantSections), len(audio))
	}
	for i, section := range sections {
		if section != wantSections[i] {
			t.Fatalf("real audio chunk %d is section %d, want %d", i, section, wantSections[i])
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

// burstSections is how many <speak> regions the fast-LLM burst below closes in one delta -- well
// past any cap a test sets, and still inside the audio subscriber's buffer (two chunks a section).
const burstSections = 6

// burstScript is a fast LLM: ONE delta that closes burstSections <speak> regions at once, so every
// section's speech leg is launched inside a single scanner pass. It returns the script and the
// sections' spoken texts in document order.
func burstScript() ([]scriptedLLMDelta, []string) {
	texts := make([]string, burstSections)
	var delta strings.Builder
	for i := range texts {
		texts[i] = fmt.Sprintf("Satz %d.", i)
		delta.WriteString("<speak>" + texts[i] + "</speak> ")
	}
	return []scriptedLLMDelta{{text: delta.String()}}, texts
}

// startSubscribedVoiceRun reserves and launches a voice run the way collectRunAudio does, but hands
// back the run and its subscription so a test can act mid-run before draining it.
func startSubscribedVoiceRun(t *testing.T, srv *Server, owner auth.Token, chatID, pending string) (*ChatRun, <-chan runEvent) {
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
	t.Cleanup(unsub)
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: pending, tts: voiceTTSTarget},
	})
	return run, ch
}

// A fast LLM closes many <speak> regions in one burst, and each fires its own speech leg. The run
// must not let them all hit the TTS at once (#337): only the run's TTS concurrency cap synthesize
// together, the rest queue behind them, yet every section is still spoken and the relayed audio
// stays ONE monotonic stream in document order -- the cap gates when a leg starts synthesizing,
// never the order of the relay. A bare timing (cap 0) means the default cap, never "no synthesis".
func TestVoiceRun_SpeechConcurrencyCapBackpressure(t *testing.T) {
	cases := []struct {
		name        string
		concurrency int // the resolved timing's ttsConcurrency; 0 leaves the WHOLE timing unset
		want        int // how many legs may synthesize at once
	}{
		{"cap of one serializes the legs", 1, 1},
		{"cap of two", 2, 2},
		{"cap of three", 3, 3},
		{"an unset timing uses the default cap", 0, VoiceTTSConcurrencyDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script, sections := burstScript()
			prov := defaultVoiceProvider()
			prov.llmScript = script
			prov.speechBodies = make(chan []byte, 2*burstSections)
			prov.speechRelease = make(chan struct{}) // hold every response until the test lets them go
			prov.speechInflight, prov.speechPeak = new(atomic.Int32), new(atomic.Int32)
			srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)
			if tc.concurrency > 0 {
				srv.voiceTiming = voiceTiming{ttsConcurrency: tc.concurrency}
			} else {
				srv.voiceTiming = voiceTiming{}
			}
			rec := &recordingVoicePortal{API: srv.Portal}
			srv.Portal = rec
			released := false
			release := func() {
				if !released {
					released = true
					close(prov.speechRelease)
				}
			}
			// A safety net so a failing assertion never leaves the loopback server waiting on a held handler.
			t.Cleanup(release)

			run, ch := startSubscribedVoiceRun(t, srv, owner, chatID, "unused")

			// The cap's worth of legs reach the TTS and are held there; the burst's other legs must be
			// queued behind them, NOT in flight. Wait for the first batch, then give any leg that
			// wrongly slipped past the cap time to show up.
			if !voicePoll(10*time.Second, func() bool { return prov.speechInflight.Load() >= int32(tc.want) }) {
				t.Fatalf("only %d syntheses reached the TTS, want %d", prov.speechInflight.Load(), tc.want)
			}
			if voicePoll(150*time.Millisecond, func() bool { return prov.speechInflight.Load() > int32(tc.want) }) {
				t.Fatalf("%d syntheses in flight at once with the cap at %d: the burst of %d sections overran the TTS",
					prov.speechInflight.Load(), tc.want, burstSections)
			}

			// The slots go out in document order: the legs in flight are the FIRST sections, not
			// whichever goroutines happened to run first.
			seen := map[string]int{}
			firstBatch := map[string]bool{}
			for range tc.want {
				select {
				case b := <-speechBodies:
					in := speechInputOf(b)
					seen[in]++
					firstBatch[in] = true
				case <-time.After(5 * time.Second):
					t.Fatal("an in-flight synthesis never recorded its request")
				}
			}
			for _, text := range sections[:tc.want] {
				if !firstBatch[text] {
					t.Fatalf("in flight = %v, want the first %d sections %v (slots are granted in document order)", firstBatch, tc.want, sections[:tc.want])
				}
			}

			release()
			audio := drainRunAudio(t, ch)

			// Every section was still synthesized, exactly once.
			for {
				select {
				case b := <-speechBodies:
					seen[speechInputOf(b)]++
					continue
				default:
				}
				break
			}
			for _, text := range sections {
				if seen[text] != 1 {
					t.Fatalf("section %q synthesized %d times, want once (seen %v)", text, seen[text], seen)
				}
			}
			if got := run.statusValue(); got != "completed" {
				t.Fatalf("status = %q, want completed", got)
			}

			// The cap held for the whole run, was actually used, and every slot came back.
			if peak := prov.speechPeak.Load(); peak != int32(tc.want) {
				t.Fatalf("peak concurrent syntheses = %d, want exactly %d (the cap: never above it, and not serialized below it)", peak, tc.want)
			}
			if !voicePoll(2*time.Second, func() bool { return prov.speechInflight.Load() == 0 }) {
				t.Fatalf("%d syntheses still in flight after the run ended", prov.speechInflight.Load())
			}

			// The relay is untouched: ONE monotonic stream, two chunks a section, in document order.
			wantSections := make([]int, 0, 2*burstSections)
			for i := range burstSections {
				wantSections = append(wantSections, i, i)
			}
			assertOneMonotonicStream(t, audio, wantSections)
			turn, ok := rec.committed()
			if !ok {
				t.Fatal("no voice commit was captured")
			}
			if len(turn.SpokenSections) != burstSections {
				t.Fatalf("committed %d spoken sections, want %d", len(turn.SpokenSections), burstSections)
			}
			for i, sec := range turn.SpokenSections {
				if sec.Text != sections[i] {
					t.Fatalf("SpokenSections[%d] = %q, want %q (document order)", i, sec.Text, sections[i])
				}
			}
		})
	}
}

// The gate grants its slots in the order the legs were QUEUED (document order), whatever order their
// goroutines happen to start in. Later sections taking the slots first would be a deadlock hazard
// (they can fill their audio buffer, block on the relay, and starve the section the relay waits for).
func TestVoiceSynthGate_GrantsSlotsInQueueOrder(t *testing.T) {
	const legs = 8
	gate := newVoiceSynthGate(1)
	turns := make([]*voiceSynthTurn, legs)
	for i := range turns {
		turns[i] = gate.queue()
	}
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	// Start the goroutines BACKWARDS: the last queued leg runs first.
	for i := legs - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !turns[i].acquire(context.Background()) {
				t.Errorf("turn %d: acquire reported a cancel on a live context", i)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			turns[i].release()
		}()
	}
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("slots were granted in order %v, want queue order 0..%d", order, legs-1)
		}
	}
	if len(order) != legs {
		t.Fatalf("%d legs got a slot, want %d", len(order), legs)
	}
}

// No more than the limit hold a slot at once, and a released slot goes to the next leg in line.
func TestVoiceSynthGate_CapsHoldersAndPassesSlotsOn(t *testing.T) {
	const limit, legs = 3, 9
	gate := newVoiceSynthGate(limit)
	var holding, peak atomic.Int32
	proceed := make(chan struct{})
	var wg sync.WaitGroup
	for range legs {
		turn := gate.queue()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !turn.acquire(context.Background()) {
				t.Error("acquire reported a cancel on a live context")
				return
			}
			n := holding.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-proceed
			holding.Add(-1)
			turn.release()
		}()
	}
	if !voicePoll(5*time.Second, func() bool { return holding.Load() == limit }) {
		t.Fatalf("%d legs hold a slot, want the limit %d", holding.Load(), limit)
	}
	if voicePoll(100*time.Millisecond, func() bool { return holding.Load() > limit }) {
		t.Fatalf("%d legs hold a slot at once, over the limit %d", holding.Load(), limit)
	}
	close(proceed)
	wg.Wait()
	if p := peak.Load(); p != limit {
		t.Fatalf("peak holders = %d, want %d", p, limit)
	}
}

// A cancel unblocks every queued leg (none takes a slot, all settle so nothing waits on them), a leg
// that never held a slot cannot release one it does not own, and a context that is already done
// takes no slot even when one is free.
func TestVoiceSynthGate_CancelUnblocksQueuedTurns(t *testing.T) {
	gate := newVoiceSynthGate(1)
	holder := gate.queue()
	if !holder.acquire(context.Background()) {
		t.Fatal("the first leg could not take the free slot")
	}

	ctx, cancel := context.WithCancel(context.Background())
	queued := []*voiceSynthTurn{gate.queue(), gate.queue(), gate.queue()}
	results := make(chan bool, len(queued))
	for _, turn := range queued {
		go func() { results <- turn.acquire(ctx) }()
	}
	cancel()
	for range queued {
		select {
		case got := <-results:
			if got {
				t.Fatal("a queued leg took a slot after the cancel")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a queued leg stayed blocked after the cancel (leaked goroutine)")
		}
	}
	for i, turn := range queued {
		select {
		case <-turn.settled:
		default:
			t.Fatalf("queued turn %d never settled, so the leg behind it would wait forever", i)
		}
		turn.release() // never held: must not take the holder's slot
	}
	if got := len(gate.slots); got != 1 {
		t.Fatalf("%d slots held, want exactly the holder's (a release without a hold stole or leaked one)", got)
	}

	holder.release()
	holder.release() // idempotent
	if got := len(gate.slots); got != 0 {
		t.Fatalf("%d slots held after the holder released, want 0", got)
	}
	// Free slot, dead context: the leg gives up rather than starting a synthesis for a cancelled run.
	late := gate.queue()
	if late.acquire(ctx) {
		t.Fatal("a leg took a slot on an already-cancelled context")
	}
	if got := len(gate.slots); got != 0 {
		t.Fatalf("%d slots held after a refused acquire, want 0", got)
	}
}

// A cancel while legs are queued behind a held synthesis ends the run cleanly and leaves nothing
// behind: only the leg that held the slot ever reached the TTS (the queued ones give up instead of
// synthesizing for a cancelled run), the slot comes back, no leg goroutine leaks, and nothing is
// persisted.
func TestVoiceRun_CancelWhileLegsQueuedLeavesNothingBehind(t *testing.T) {
	script, _ := burstScript()
	script[0].holdUntilCancel = true // the LLM is still streaming when the cancel lands
	prov := defaultVoiceProvider()
	prov.llmScript = script
	prov.speechBodies = make(chan []byte, 2*burstSections)
	prov.speechRelease = make(chan struct{}) // never released: the cancel is what ends the held leg
	prov.speechInflight, prov.speechPeak = new(atomic.Int32), new(atomic.Int32)
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsConcurrency: 1}
	t.Cleanup(func() { close(prov.speechRelease) })

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := goroutineStacks()
	run, err := srv.startVoiceRun(context.Background(), voiceRunRequest{owner: owner, chatID: chatID, pendingText: "unused", asr: voiceASRTarget, llm: voiceLLMTarget, tts: voiceTTSTarget})
	if err != nil {
		t.Fatalf("startVoiceRun: %v", err)
	}

	// One leg holds the only slot at the TTS; the other five sections are queued behind it.
	if !voicePoll(10*time.Second, func() bool { return prov.speechInflight.Load() == 1 }) {
		t.Fatalf("%d syntheses in flight, want the one that holds the slot", prov.speechInflight.Load())
	}
	if voicePoll(150*time.Millisecond, func() bool { return prov.speechInflight.Load() > 1 }) {
		t.Fatalf("%d syntheses in flight with the cap at 1", prov.speechInflight.Load())
	}
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}

	if n := len(speechBodies); n != 1 {
		t.Fatalf("%d speech requests reached the TTS, want 1: the queued legs must give up on a cancel, not synthesize", n)
	}
	if !voicePoll(2*time.Second, func() bool { return prov.speechInflight.Load() == 0 }) {
		t.Fatalf("%d syntheses still in flight after the cancel", prov.speechInflight.Load())
	}
	var leaked []string
	if !voicePoll(2*time.Second, func() bool {
		leaked = newRepoGoroutines(baseline, "launchVoiceSpeech")
		return len(leaked) == 0
	}) {
		t.Fatalf("speech leg goroutines leaked after the cancel:\n%s", strings.Join(leaked, "\n\n"))
	}
	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Content) != string(after.Content) {
		t.Fatalf("a cancelled run persisted:\nbefore %s\nafter  %s", before.Content, after.Content)
	}
}

// The first-chunk deadline is the SYNTHESIS's, not the queue's: a leg that waits behind an earlier
// section's slot must not spend its deadline waiting. Section 0 hangs at the TTS until its own
// deadline ends it (a timeout); section 1 was queued the whole time -- as long as that deadline --
// yet then gets its own full deadline from the moment it takes the slot, and speaks.
func TestVoiceRun_QueuedLegDeadlineStartsWhenItTakesTheSlot(t *testing.T) {
	restore := speechRunDeadline
	speechRunDeadline = 250 * time.Millisecond
	t.Cleanup(func() { speechRunDeadline = restore })

	prov := defaultVoiceProvider()
	prov.llmScript = twoRegionScript
	prov.speechHoldByInput = map[string]chan struct{}{twoRegionSec0: make(chan struct{})} // never released
	srv, owner, chatID, _, _ := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsConcurrency: 1}
	rec := &recordingVoicePortal{API: srv.Portal}
	srv.Portal = rec

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "unused")
	if snap.Status != "error" || snap.Err != runTimedOutMessage {
		t.Fatalf("status %q err %q, want the first section's timeout (%q) surfaced as the degrade", snap.Status, snap.Err, runTimedOutMessage)
	}
	turn, ok := rec.committed()
	if !ok {
		t.Fatal("no voice commit was captured")
	}
	if len(turn.SpokenSections) != 1 || turn.SpokenSections[0].Text != twoRegionSec1 {
		t.Fatalf("SpokenSections = %+v, want only the queued section %q (it must still synthesize in its own, fresh deadline)", turn.SpokenSections, twoRegionSec1)
	}
}
