// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/store"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// notifySpy records what the registry asks an agent to stream. The SET is the
// whole command, so the recorded history is exactly the sequence of commands
// the agent would have received.
type notifySpy struct {
	mu    sync.Mutex
	calls [][]string
}

func (n *notifySpy) fn(_ string, specIDs []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, append([]string(nil), specIDs...))
}

func (n *notifySpy) snapshot() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][]string(nil), n.calls...)
}

func newSpiedLogRegistry() (*runtimeLogRegistry, *notifySpy) {
	r := newRuntimeLogRegistry()
	spy := &notifySpy{}
	r.setNotify(spy.fn)
	return r, spy
}

// TestRuntimeLogSubscriptionDrivesTheAgent is the on-demand contract: the
// first viewer starts the stream, the last one stops it, and everything in
// between is one command carrying the full desired set.
func TestRuntimeLogSubscriptionDrivesTheAgent(t *testing.T) {
	r, spy := newSpiedLogRegistry()

	_, unsubA := r.subscribe("srv-1", "spec-a")
	_, unsubB := r.subscribe("srv-1", "spec-b")
	unsubB()
	unsubA()

	want := [][]string{{"spec-a"}, {"spec-a", "spec-b"}, {"spec-a"}, {}}
	got := spy.snapshot()
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("command %d = %v, want %v", i, got[i], want[i])
		}
	}
	// The last unsubscribe must ask for the EMPTY set, not merely stop
	// talking: that is what turns the agent's stream off again.
	if len(got[len(got)-1]) != 0 {
		t.Fatal("the last viewer leaving did not command an empty watch set")
	}
}

// TestRuntimeLogTwoViewersOneAgentStream: a second operator watching the same
// spec must not cause a second agent stream. The command is a SET, so the
// second subscribe is idempotent, and the first unsubscribe must not stop the
// stream the remaining viewer still needs.
func TestRuntimeLogTwoViewersOneAgentStream(t *testing.T) {
	r, spy := newSpiedLogRegistry()

	first, unsubFirst := r.subscribe("srv-1", "spec-a")
	second, unsubSecond := r.subscribe("srv-1", "spec-a")
	defer unsubSecond()

	for _, cmd := range spy.snapshot() {
		if len(cmd) != 1 || cmd[0] != "spec-a" {
			t.Fatalf("command %v, want every command to be exactly [spec-a]", cmd)
		}
	}

	batch := RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "shared"}}}
	r.publish("srv-1", batch)
	for i, sub := range []*runtimeLogSub{first, second} {
		select {
		case got := <-sub.ch:
			if got.Entries[0].Text != "shared" {
				t.Fatalf("subscriber %d got %+v", i, got)
			}
		default:
			t.Fatalf("subscriber %d received nothing; one agent stream must fan out to both", i)
		}
	}

	// The first viewer leaving must NOT stop the stream: the second is still
	// watching, so the set is unchanged.
	unsubFirst()
	last := spy.snapshot()
	if cmd := last[len(last)-1]; len(cmd) != 1 || cmd[0] != "spec-a" {
		t.Fatalf("after one of two viewers left, command = %v, want still [spec-a]", cmd)
	}
	r.publish("srv-1", batch)
	select {
	case <-second.ch:
	default:
		t.Fatal("the remaining viewer stopped receiving when the other one left")
	}
}

// TestRuntimeLogPublishWithNoSubscriberIsANoOp: an unsolicited frame (a
// straggler after the last unsubscribe, or a confused agent) costs a parse and
// nothing else -- in particular it is never buffered for a viewer who might
// arrive later, because the agent's own scrollback is the authoritative replay.
func TestRuntimeLogPublishWithNoSubscriberIsANoOp(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "nobody is watching"}}})

	sub, unsub := r.subscribe("srv-1", "spec-a")
	defer unsub()
	select {
	case got := <-sub.ch:
		t.Fatalf("a late subscriber received %+v; nothing may be buffered for a viewer who was not there", got)
	default:
	}
}

// TestRuntimeLogSlowSubscriberGetsAnExplicitGap: when a browser falls behind
// far enough that batches are dropped, the loss is REPORTED on the next batch
// that gets through. A gap that renders as silence would be a lie about what
// the process printed.
func TestRuntimeLogSlowSubscriberGetsAnExplicitGap(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	sub, unsub := r.subscribe("srv-1", "spec-a")
	defer unsub()

	// Overrun the queue without reading. Each batch carries 10 bytes of text.
	const overrun = runtimeLogSubBuffer + 5
	for range overrun {
		r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "0123456789"}}})
	}
	// Drain what did fit, then publish once more: the drop count must ride on
	// that batch.
	for range runtimeLogSubBuffer {
		<-sub.ch
	}
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "resumed"}}})
	got := sub.take(<-sub.ch)
	if got.Entries[0].DroppedBytes != int64((overrun-runtimeLogSubBuffer)*10) {
		t.Fatalf("DroppedBytes = %d, want %d (the exact bytes the slow reader missed)",
			got.Entries[0].DroppedBytes, (overrun-runtimeLogSubBuffer)*10)
	}
	if got.Entries[0].Text != "resumed" {
		t.Fatalf("the gap marker replaced the entry it should have annotated: %+v", got.Entries[0])
	}
}

// TestRuntimeLogGapIsPerSubscriber: publish fans ONE batch value out to every
// subscriber, so stamping a drop count must not be visible to a subscriber
// that never dropped anything.
func TestRuntimeLogGapIsPerSubscriber(t *testing.T) {
	r, _ := newSpiedLogRegistry()
	slow, unsubSlow := r.subscribe("srv-1", "spec-a")
	defer unsubSlow()
	fast, unsubFast := r.subscribe("srv-1", "spec-a")
	defer unsubFast()

	for range runtimeLogSubBuffer + 3 {
		batch := RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "0123456789"}}}
		r.publish("srv-1", batch)
		select { // the fast subscriber keeps up
		case <-fast.ch:
		default:
		}
	}
	r.publish("srv-1", RuntimeLogBatchDTO{SpecID: "spec-a", Entries: []RuntimeLogEntryDTO{{Text: "next"}}})

	// Drain the slow one to its newest batch and stamp it.
	var slowBatch RuntimeLogBatchDTO
	for len(slow.ch) > 0 {
		slowBatch = <-slow.ch
	}
	stamped := slow.take(slowBatch)
	if stamped.Entries[0].DroppedBytes == 0 {
		t.Fatal("the slow subscriber's gap was not reported")
	}
	fastBatch := fast.take(<-fast.ch)
	if fastBatch.Entries[0].DroppedBytes != 0 {
		t.Fatalf("the fast subscriber was told about someone else's gap: %+v", fastBatch.Entries[0])
	}
}

// TestRuntimeLogIngestSanitizes: an agent's frame is data, not truth. The
// event kind is an allow-list because the portal renders it as its own
// localized sentence -- free text there would be gateway-authored-looking
// text an agent chose.
func TestRuntimeLogIngestSanitizes(t *testing.T) {
	srv := &Server{RuntimeLogs: newRuntimeLogRegistry()}
	sub, unsub := srv.RuntimeLogs.subscribe("srv-1", "spec-a")
	defer unsub()

	raw := json.RawMessage(`{"spec_id":"spec-a","entries":[
		{"event":"started","pid":7},
		{"event":"YOUR ACCOUNT IS COMPROMISED, CALL 555","exit_code":9,"text":"real output"},
		{"at":"` + strings.Repeat("x", 200) + `","text":"stamped"},
		{"text":"negative","dropped_bytes":-5}
	]}`)
	srv.ingestRuntimeLog("srv-1", raw)

	got := <-sub.ch
	if got.Entries[0].Event != "started" || got.Entries[0].PID != 7 {
		t.Errorf("allow-listed event was altered: %+v", got.Entries[0])
	}
	if got.Entries[1].Event != "" || got.Entries[1].ExitCode != 0 {
		t.Errorf("unknown event survived sanitization: %+v", got.Entries[1])
	}
	if got.Entries[1].Text != "real output" {
		t.Errorf("sanitizing the event must not discard the entry's own text: %+v", got.Entries[1])
	}
	if len(got.Entries[2].At) != runtimeLogMaxAtLen {
		t.Errorf("At = %d bytes, want clamped to %d", len(got.Entries[2].At), runtimeLogMaxAtLen)
	}
	if got.Entries[3].DroppedBytes != 0 {
		t.Errorf("negative DroppedBytes survived: %+v", got.Entries[3])
	}
}

// TestRuntimeLogIngestRejectsUnusableFrames: a malformed or unaddressable
// frame is skipped, never a reason to tear down a healthy telemetry
// connection.
func TestRuntimeLogIngestRejectsUnusableFrames(t *testing.T) {
	srv := &Server{RuntimeLogs: newRuntimeLogRegistry()}
	sub, unsub := srv.RuntimeLogs.subscribe("srv-1", "spec-a")
	defer unsub()

	srv.ingestRuntimeLog("srv-1", nil)
	srv.ingestRuntimeLog("srv-1", json.RawMessage(`not json`))
	srv.ingestRuntimeLog("srv-1", json.RawMessage(`{"entries":[{"text":"no spec id"}]}`))
	srv.ingestRuntimeLog("srv-1", json.RawMessage(`{"spec_id":"other-spec","entries":[{"text":"wrong spec"}]}`))

	select {
	case got := <-sub.ch:
		t.Fatalf("an unusable frame reached a subscriber: %+v", got)
	default:
	}

	// A well-formed frame for the right spec still arrives afterwards: the
	// tolerance above must not have poisoned anything.
	srv.ingestRuntimeLog("srv-1", json.RawMessage(`{"spec_id":"spec-a","entries":[{"text":"ok"}]}`))
	if got := <-sub.ch; got.Entries[0].Text != "ok" {
		t.Fatalf("got %+v after tolerated bad frames", got)
	}
}

// TestRuntimeLogIngestClampsEntryCount bounds the fan-out against a frame of
// absurdly many entries.
func TestRuntimeLogIngestClampsEntryCount(t *testing.T) {
	srv := &Server{RuntimeLogs: newRuntimeLogRegistry()}
	sub, unsub := srv.RuntimeLogs.subscribe("srv-1", "spec-a")
	defer unsub()

	entries := make([]string, 0, runtimeLogMaxEntries+10)
	for range runtimeLogMaxEntries + 10 {
		entries = append(entries, `{"text":"x"}`)
	}
	srv.ingestRuntimeLog("srv-1", json.RawMessage(`{"spec_id":"spec-a","entries":[`+strings.Join(entries, ",")+`]}`))
	if got := <-sub.ch; len(got.Entries) != runtimeLogMaxEntries {
		t.Fatalf("entries = %d, want clamped to %d", len(got.Entries), runtimeLogMaxEntries)
	}
}

// TestRuntimeLogStateSeparatesTheThreeSilences is the feature-negotiation
// payoff: an empty log window has three causes and they need three different
// things from the operator. Getting this wrong is the branch's most-repeated
// defect class -- an empty view that reads as "this model prints nothing".
func TestRuntimeLogStateSeparatesTheThreeSilences(t *testing.T) {
	srv := &Server{AgentStreams: NewAgentStreamRegistry(), AgentFeatures: newAgentFeaturesRegistry()}

	if got := srv.runtimeLogState("srv-1"); got != runtimeLogStateOffline {
		t.Errorf("no agent connection = %q, want %q", got, runtimeLogStateOffline)
	}

	// A connected agent whose declared features do not include runtime_logs
	// (an older binary) -- the case that would otherwise be an empty window
	// forever.
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add("srv-1", conn)
	srv.AgentFeatures.Set("srv-1", []string{"runtime_manager"})
	if got := srv.runtimeLogState("srv-1"); got != runtimeLogStateUnsupported {
		t.Errorf("connected agent without the feature = %q, want %q", got, runtimeLogStateUnsupported)
	}

	srv.AgentFeatures.Set("srv-1", []string{"runtime_manager", "runtime_logs"})
	if got := srv.runtimeLogState("srv-1"); got != runtimeLogStateStreaming {
		t.Errorf("connected agent with the feature = %q, want %q", got, runtimeLogStateStreaming)
	}

	// A stale feature declaration from an agent that has since disconnected
	// must NOT report streaming: the connection check has to come first, or
	// "streaming" becomes the empty-window lie again.
	srv.AgentStreams.remove("srv-1", conn)
	if got := srv.runtimeLogState("srv-1"); got != runtimeLogStateOffline {
		t.Errorf("disconnected agent with stale features = %q, want %q", got, runtimeLogStateOffline)
	}
}

// --- HTTP surface ----------------------------------------------------------

func runtimeLogRequest(t *testing.T, ts *httptest.Server, secret, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+runtimeEventsServerID+"/runtime/logs"+query, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// TestRuntimeLogEventsStreamsStatusThenScrollbackThenLive is the end-to-end
// portal shape: the operator learns whether a live stream is possible at all,
// then receives the agent's retained history, then whatever the process prints
// next.
func TestRuntimeLogEventsStreamsStatusThenScrollbackThenLive(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := runtimeLogRequest(t, ts, runtimeEventsOwnerSecret, "?spec_id=spec-a")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "status" {
		t.Fatalf("first event = %q, want status", event)
	}
	var st runtimeLogStatusEventDTO
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		t.Fatalf("unmarshal status: %v (%s)", err, data)
	}
	// No agent is connected in this test, so the honest answer is "offline",
	// NOT an empty stream with no explanation.
	if st.State != runtimeLogStateOffline {
		t.Fatalf("state = %q, want %q", st.State, runtimeLogStateOffline)
	}

	// Subscribing is what starts the agent's stream, so by now the registry
	// must be asking for this spec.
	if got := srv.RuntimeLogs.watched(runtimeEventsServerID); len(got) != 1 || got[0] != "spec-a" {
		t.Fatalf("watched = %v, want [spec-a] while a view is open", got)
	}

	srv.ingestRuntimeLog(runtimeEventsServerID, json.RawMessage(
		`{"spec_id":"spec-a","scrollback":true,"entries":[{"event":"started","pid":11},{"text":"loading weights\n"}]}`))
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "log" {
		t.Fatalf("event = %q, want log", event)
	}
	var batch RuntimeLogBatchDTO
	if err := json.Unmarshal([]byte(data), &batch); err != nil {
		t.Fatalf("unmarshal batch: %v (%s)", err, data)
	}
	if !batch.Scrollback || batch.SpecID != "spec-a" {
		t.Fatalf("batch = %+v, want the scrollback for spec-a", batch)
	}
	if batch.Entries[0].Event != "started" || batch.Entries[1].Text != "loading weights\n" {
		t.Fatalf("scrollback entries = %+v", batch.Entries)
	}

	srv.ingestRuntimeLog(runtimeEventsServerID, json.RawMessage(
		`{"spec_id":"spec-a","entries":[{"text":"CUDA error: out of memory\n"},{"event":"exited","exit_code":1}]}`))
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "log" {
		t.Fatalf("event = %q, want log", event)
	}
	batch = RuntimeLogBatchDTO{}
	if err := json.Unmarshal([]byte(data), &batch); err != nil {
		t.Fatalf("unmarshal live batch: %v (%s)", err, data)
	}
	if batch.Scrollback {
		t.Fatal("the live batch is marked as scrollback")
	}
	if batch.Entries[1].Event != "exited" || batch.Entries[1].ExitCode != 1 {
		t.Fatalf("live entries = %+v, want the exit marker with code 1", batch.Entries)
	}
}

// TestRuntimeLogEventsStopsTheAgentWhenTheViewCloses: the deferred unsubscribe
// is not merely cleanup, it is the stop command -- without it a closed browser
// tab would leave an agent streaming forever.
func TestRuntimeLogEventsStopsTheAgentWhenTheViewCloses(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := runtimeLogRequest(t, ts, runtimeEventsOwnerSecret, "?spec_id=spec-a")
	reader := bufio.NewReader(resp.Body)
	readPerfSSEFrame(t, reader, 3*time.Second) // the status frame
	if got := srv.RuntimeLogs.watched(runtimeEventsServerID); len(got) != 1 {
		t.Fatalf("watched = %v, want the open view's spec", got)
	}
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(srv.RuntimeLogs.watched(runtimeEventsServerID)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent was still asked to stream after the last view closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeLogEventsAuthorization: the same boundary as the rest of the
// feature, with the 404-no-leak collapse -- not a laxer path because it is
// "just logs".
func TestRuntimeLogEventsAuthorization(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/logs?spec_id=spec-a", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found (no existence leak)", code)
	}
	// And a non-owner must not have started an agent stream on the way to
	// being refused.
	if got := srv.RuntimeLogs.watched(runtimeEventsServerID); len(got) != 0 {
		t.Fatalf("a refused request subscribed anyway: %v", got)
	}
}

// TestRuntimeLogEventsRequiresSpec: without a spec there is nothing to stream,
// and answering 200 with an empty stream would be the empty-window lie again.
func TestRuntimeLogEventsRequiresSpec(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/logs", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_logs.spec_required" {
		t.Fatalf("error code = %q, want runtime_logs.spec_required", code)
	}
}

// --- the load-bearing safety property --------------------------------------

// TestRuntimeLogRelayNeverReachesDiskOrDatabase is the assertion the whole
// feature rests on, made rather than argued.
//
// A real SQLite store is attached to the server and a real relay is performed
// through the real ingest path with a real subscriber attached, so anything
// that persisted a fragment would have had both the opportunity and a place to
// put it. Then every byte of the database -- and of every other file in the
// directory, including SQLite's WAL and any temp file -- is searched for the
// needle. Captured model-process output can contain prompt text; this project
// forbids persisting prompts outside the opt-in payload-capture feature, and
// nothing in this path is opt-in.
func TestRuntimeLogRelayNeverReachesDiskOrDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	srv := &Server{Routes: st, RuntimeLogs: newRuntimeLogRegistry()}

	sub, unsub := srv.RuntimeLogs.subscribe("srv-1", "spec-a")
	defer unsub()

	const needle = "OP-AI-GATEWAY-PROMPT-NEEDLE-4b7c"
	frame, err := json.Marshal(RuntimeLogBatchDTO{
		SpecID:     "spec-a",
		Scrollback: true,
		Entries: []RuntimeLogEntryDTO{
			{Event: "started", PID: 42},
			{Text: "prompt fragment: " + needle + "\n"},
			{Event: "exited", ExitCode: 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	// Relay it many times: a "persist every Nth" or buffered writer would
	// still have flushed by now.
	for range 50 {
		srv.ingestRuntimeLog("srv-1", frame)
		select {
		case <-sub.ch: // keep the subscriber draining so nothing is merely dropped
		default:
		}
	}
	// Close so SQLite checkpoints its WAL into the main database file: a
	// search that ran before this could pass simply because the bytes had not
	// been written out yet.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var offenders []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not evidence of a write
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr // same
		}
		if strings.Contains(string(raw), needle) {
			offenders = append(offenders, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("relayed managed-process output was persisted to %v -- it may contain prompt text and must never reach disk or the database", offenders)
	}
}

// TestAgentStreamRestatesTheWatchSetOnConnect: a fresh agent connection is
// told what to stream, unconditionally, so a watch set can never outlive the
// connection it was issued on -- neither by going stale (an agent that keeps
// streaming to nobody) nor by going missing (a log view left open across a
// reconnect that never fills again).
func TestAgentStreamRestatesTheWatchSetOnConnect(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_logs", "mock-host-qwen", "agent-secret")

	// An operator already has a log view open when the agent (re)connects.
	_, unsub := srv.RuntimeLogs.subscribe("mock-host-qwen", "spec-open")
	defer unsub()

	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read the connect-time frame: %v", err)
	}
	var f streamFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal frame: %v (%s)", err, raw)
	}
	if f.Type != "runtime_log_config" {
		t.Fatalf("first server->agent frame type = %q, want runtime_log_config", f.Type)
	}
	var cmd runtimeLogWatchFrame
	if err := json.Unmarshal(f.Data, &cmd); err != nil {
		t.Fatalf("unmarshal command: %v (%s)", err, f.Data)
	}
	if len(cmd.SpecIDs) != 1 || cmd.SpecIDs[0] != "spec-open" {
		t.Fatalf("watch command = %v, want [spec-open]", cmd.SpecIDs)
	}
}

// TestAgentStreamRestatesAnEmptyWatchSetOnConnect: the empty set is a
// COMMAND, not a no-op. An agent that was streaming before a drop, whose
// viewers have since gone, must be told to stop rather than left streaming to
// nobody for the rest of its uptime.
func TestAgentStreamRestatesAnEmptyWatchSetOnConnect(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_logs_empty", "mock-host-qwen", "agent-secret")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read the connect-time frame: %v", err)
	}
	var f streamFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal frame: %v (%s)", err, raw)
	}
	if f.Type != "runtime_log_config" {
		t.Fatalf("frame type = %q, want runtime_log_config even with nothing watched", f.Type)
	}
	// `[]`, never `null`: the agent parses this into a slice and an explicit
	// empty list is the difference between "watch nothing" and "no field".
	if !strings.Contains(string(f.Data), `"spec_ids":[]`) {
		t.Fatalf("payload = %s, want an explicit empty spec_ids array", f.Data)
	}
}
