// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This file covers the per-VIEWER half of the relay: which subscriber a batch
// is for, what each of them is told, and what a subscriber that has fallen
// behind is allowed to lose.

// drainSub takes everything currently queued for sub, stamped as the SSE writer
// would stamp it.
func drainSub(sub *runtimeLogSub) []RuntimeLogBatchDTO {
	var out []RuntimeLogBatchDTO
	for len(sub.ch) > 0 {
		out = append(out, sub.take(<-sub.ch))
	}
	return out
}

func batchTextOf(batches []RuntimeLogBatchDTO) string {
	var sb strings.Builder
	for _, b := range batches {
		for _, e := range b.Entries {
			sb.WriteString(e.Text)
		}
	}
	return sb.String()
}

// TestRuntimeLogSecondViewerGetsHistoryUndisturbed is D2/D3.
//
// Two operators on the same crashed spec: the arriving one must get the
// history, and the one already watching must not be disturbed by the replay
// produced for it -- no reset, no re-rendered history, nothing but its own live
// stream. That constraint is why the replay is ROUTED rather than broadcast,
// and why the id is not dropped and re-added (which would race the watching
// viewer's output and tear it).
func TestRuntimeLogSecondViewerGetsHistoryUndisturbed(t *testing.T) {
	r, spy := newSpiedLogRegistry()
	first, unsubFirst, ok := r.subscribe("srv-1", "spec-a")
	if !ok {
		t.Fatal("first subscribe refused")
	}
	defer unsubFirst()
	// The agent's replay for the first viewer, then live output.
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Scrollback: true, Entries: []RuntimeLogEntryDTO{{Text: "history\n"}}})
	drainSub(first)

	second, unsubSecond, ok := r.subscribe("srv-1", "spec-a")
	if !ok {
		t.Fatal("second subscribe refused")
	}
	defer unsubSecond()

	// The watch SET is unchanged; only the epoch moved. That is the command the
	// agent needs in order to know a viewer arrived.
	calls := spy.snapshot()
	if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0] != "spec-a" {
		t.Fatalf("commands = %+v, want the same one-spec set twice", calls)
	}
	if e := spy.epochsAt(1); e["spec-a"] <= spy.epochsAt(0)["spec-a"] {
		t.Fatalf("epoch did not advance for the arriving viewer: %v then %v", spy.epochsAt(0), e)
	}

	// The agent's re-snapshot, chunked.
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Scrollback: true, More: true, Entries: []RuntimeLogEntryDTO{{Text: "history\n"}}})
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Scrollback: true, Entries: []RuntimeLogEntryDTO{{Text: "and-more\n"}}})

	got := drainSub(second)
	if text := batchTextOf(got); text != "history\nand-more\n" {
		t.Fatalf("the arriving viewer got %q, want the whole history", text)
	}
	if len(got) != 2 || !got[0].Scrollback || got[1].Scrollback {
		t.Fatalf("scrollback flags = %+v, want reset on the first chunk only", got)
	}
	for i, b := range got {
		if b.More {
			t.Fatalf("chunk %d leaked scrollback_more to the portal; chunking is not the portal's concern", i)
		}
	}
	if disturbed := drainSub(first); len(disturbed) != 0 {
		t.Fatalf("the viewer already watching was handed %+v; a replay it did not ask for resets its view", disturbed)
	}

	// And its live stream still works.
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "live\n"}}})
	if text := batchTextOf(drainSub(first)); text != "live\n" {
		t.Fatalf("the watching viewer's live stream = %q, want it uninterrupted", text)
	}
}

// TestRuntimeLogFullQueueKeepsMarkers is D5. queueLocked in the agent protects
// exactly this one layer down -- "a MARKER is never dropped, because losing 'the
// process exited, code 1' is losing the very fact the operator is reading the
// stream to find" -- and the relay used to drop whole batches, markers included.
//
// The concrete loss: a spec crash-loops while a browser stalls, the dropped
// batch carries generation 1's `exited(code 1)` and generation 2's
// `started(pid, command)`, and the operator then reads two generations as one
// continuous run with no exit code and no command for the second attempt.
func TestRuntimeLogFullQueueKeepsMarkers(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	sub, unsub, _ := r.subscribe("srv-1", "spec-a")
	defer unsub()

	for range runtimeLogSubBuffer {
		r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "0123456789"}}})
	}
	// The queue is full. This batch carries the generation boundary.
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{
		{Text: "lost text"},
		{Event: "exited", ExitCode: 1},
		{Event: "started", PID: 4242, Command: &RuntimeLogCommandDTO{Binary: "/opt/llama"}},
	}})
	seen := drainSub(sub) // the reader catches up
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "generation 2\n"}}})
	last := drainSub(sub)
	if len(last) != 1 {
		t.Fatalf("got %d batches after catching up, want one", len(last))
	}
	seen = append(seen, last...)

	// The markers ride the first batch that FITS after the drop, which is the
	// only position they are in order at -- in front of generation 2's output,
	// after everything queued before the drop.
	var exited, started bool
	for _, e := range last[0].Entries {
		switch e.Event {
		case "exited":
			exited = true
			if e.ExitCode != 1 {
				t.Fatalf("the exit marker survived but lost its code: %+v", e)
			}
		case "started":
			started = true
			if e.Command == nil || e.Command.Binary != "/opt/llama" {
				t.Fatalf("the opening marker survived but lost its command: %+v", e)
			}
		}
	}
	if !exited || !started {
		t.Fatalf("a boundary marker was dropped (exited=%v started=%v); the operator would read two generations as one run: %+v",
			exited, started, last[0].Entries)
	}
	var dropped int64
	for _, b := range seen {
		for _, e := range b.Entries {
			dropped += e.DroppedBytes
		}
	}
	if dropped != int64(len("lost text")) {
		t.Fatalf("dropped_bytes across everything the reader saw = %d, want %d", dropped, len("lost text"))
	}
	if strings.Contains(batchTextOf(seen), "lost text") {
		t.Fatal("text was carried over rather than dropped; dropped_bytes is what accounts for text")
	}
}

// TestRuntimeLogHopelesslyBehindSubscriberIsResynced: markers are carried, not
// accumulated without limit. Past the carry ceiling the honest move is to end
// the stream -- dropped_bytes means "bytes the process printed are missing" and
// cannot carry "a generation boundary was lost", and a reconnect now delivers a
// complete history rather than a stale one.
func TestRuntimeLogHopelesslyBehindSubscriberIsResynced(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	sub, unsub, _ := r.subscribe("srv-1", "spec-a")
	defer unsub()

	for range runtimeLogSubBuffer + runtimeLogSubMarkerCarry + 8 {
		r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{
			{Event: "exited", ExitCode: 1},
			{Event: "started", PID: 1},
		}})
	}
	select {
	case <-sub.resync:
	default:
		t.Fatal("a subscriber past the marker-carry ceiling was left silently incomplete instead of resynced")
	}
}

// TestRuntimeLogEventsRejectsAnOverlongSpecID is D6a. The id is not only a
// fan-out key: every subscribed id for a server is marshaled into the outbound
// runtime_log_config command, so an unbounded one produced a frame the AGENT's
// own read limit refused -- on a connection the gateway then re-sent it on at
// every reconnect. Rejected rather than clamped, because ingest clamps to the
// same length and a longer subscription could therefore never match a published
// batch.
func TestRuntimeLogEventsRejectsAnOverlongSpecID(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	long := strings.Repeat("A", 600<<10)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/logs?spec_id="+long, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_logs.spec_invalid" {
		t.Fatalf("error code = %q, want runtime_logs.spec_invalid", code)
	}
	if watched := srv.RuntimeLogs.watched(runtimeEventsServerID); len(watched) != 0 {
		t.Fatalf("the rejected id still reached the watch set: %v", watched)
	}
}

// TestRuntimeLogWatchSetIsCapped: the agent silently truncates a watch set past
// its own maxWatchedSpecs, so a subscription the gateway accepted past the same
// ceiling would be a window that streams nothing, forever, with nothing said.
// Refused, visibly, instead.
func TestRuntimeLogWatchSetIsCapped(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	for i := range runtimeLogMaxWatchedSpecs {
		if _, unsub, ok := r.subscribe("srv-1", "spec-"+string(rune('a'+i%26))+string(rune('a'+i/26))); !ok {
			t.Fatalf("subscribe %d refused below the ceiling", i)
		} else {
			defer unsub()
		}
	}
	if _, _, ok := r.subscribe("srv-1", "one-too-many"); ok {
		t.Fatalf("subscribe past %d distinct specs was accepted; the agent would silently ignore it", runtimeLogMaxWatchedSpecs)
	}
	// A second viewer on an ALREADY watched spec is never refused: it costs the
	// outbound command nothing.
	if _, unsub, ok := r.subscribe("srv-1", "spec-aa"); !ok {
		t.Fatal("a second viewer on an already-watched spec was refused")
	} else {
		unsub()
	}
}

// TestAgentStreamRestateBumpsTheEpoch is the gateway half of D4. A plain
// WebSocket reconnect leaves the agent's watch map intact, so a restate naming
// the same specs made it skip the snapshot -- and everything the agent drained
// and could not send while the connection was down was then never reported at
// all, contiguous with what came after, with nothing marking the hole.
func TestAgentStreamRestateBumpsTheEpoch(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	_, unsub, _ := r.subscribe("srv-1", "spec-a")
	defer unsub()
	before := r.epochs["srv-1"]["spec-a"]

	watched, epochs := r.restate("srv-1")
	if len(watched) != 1 || watched[0] != "spec-a" {
		t.Fatalf("restate set = %v, want the open view's spec", watched)
	}
	if epochs["spec-a"] <= before {
		t.Fatalf("restate epoch = %d, want more than %d: without a bump the reconnect replays nothing",
			epochs["spec-a"], before)
	}
	// A server with no open view restates an empty set and no epochs, which is
	// still a meaningful command ("stop streaming").
	if watched, epochs := r.restate("srv-2"); len(watched) != 0 || len(epochs) != 0 {
		t.Fatalf("restate of an unwatched server = %v/%v, want empty", watched, epochs)
	}
}

// TestAgentFrameLimitMatchesTheAgentsOwnCap pins the gateway end of the
// contract maxAgentFrameBytes documents. The agent is a separate Go module, so
// no compiler holds the two ends together; this reads its constant out of its
// source instead. Its counterpart there is TestGatewayReadLimitMatchesThisModulesCap.
func TestAgentFrameLimitMatchesTheAgentsOwnCap(t *testing.T) {
	const path = "../../../../server-agent/internal/gwapi/gwapi.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("agent module not checked out beside this one (%v); the seam is unverified in this run", err)
	}
	m := regexp.MustCompile(`(?m)^const MaxWSFrameBytes int64 = (\S+ \S+ \S+)`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("MaxWSFrameBytes is no longer declared as a plain const in %s; this pin needs updating, and so does maxAgentFrameBytes's doc", path)
	}
	if got, want := strings.TrimSpace(string(m[1])), "1 << 20"; got != want {
		t.Fatalf("the agent builds frames against %s but this gateway reads at most %s (maxAgentFrameBytes = %d). "+
			"They are one contract: change both, and never lower this one.", got, want, maxAgentFrameBytes)
	}
}
