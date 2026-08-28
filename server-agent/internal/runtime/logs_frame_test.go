// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/gwapi"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// This file is about ONE property: the agent can never write a runtime_log
// frame the gateway will refuse to read.
//
// It shipped violating that property under DEFAULT configuration. Retention
// (DefaultLogBufferBytes) and the gateway's read limit were the same number,
// trimLocked holds a busy spec at exactly the retention cap, and the scrollback
// was then emitted whole -- so any spec that had ever printed a megabyte
// produced a frame structurally over the limit the moment an operator opened its
// log view. coder/websocket answers that by failing the read and closing 1009,
// which takes down the connection telemetry, the reports, the runtime_config
// push and the certificate doorbell all share, while the agent's own write
// returns nil.

// logFrame wraps a marshaled batch exactly as internal/client's WSSender does,
// because the gateway's limit applies to the whole frame, not to the payload.
func logFrame(t *testing.T, b LogBatch) []byte {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	frame, err := json.Marshal(struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data,omitempty"`
	}{Type: "runtime_log", Data: raw})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return frame
}

// fillSpec writes line to specID until the retention buffer is well past full,
// so the scrollback is exactly the worst case: a buffer trimLocked is holding
// at the per-spec cap.
func fillSpec(t *testing.T, s *LogStore, specID, line string, capacity int) *procLog {
	t.Helper()
	p := s.newProc(specID)
	p.Started(4242, ResolvedCommand{
		Binary: "/opt/models/llama-server",
		Args:   []string{"--port", "54331", "--ctx-size", "262144", "--model", "/srv/models/qwen3-32b.gguf"},
		Env:    []string{"CUDA_VISIBLE_DEVICES=2,3"},
	})
	for written := 0; written < capacity+(64<<10); written += len(line) {
		if _, err := p.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return p
}

// drainAll runs Drain until the whole replay has come out, returning every
// batch in order. Bounded so a bug that never finishes fails as a test rather
// than as a hang.
func drainAll(t *testing.T, s *LogStore) []LogBatch {
	t.Helper()
	var out []LogBatch
	for range 4096 {
		batches := s.Drain()
		if len(batches) == 0 {
			return out
		}
		out = append(out, batches...)
	}
	t.Fatal("Drain never ran dry")
	return nil
}

// TestLogScrollbackFitsTheFrameLimit is the D1 assertion, made against the
// MARSHALED FRAME rather than against the record bytes -- the record bytes were
// never the problem. The escape-heavy cases are the ones that made the overshoot
// large rather than marginal, and the raised-buffer case is the one that matters
// most: config-env.md and the README invite an operator to raise
// RuntimeLogBufferBytes, so a rule that held only at the default would not be a
// fix.
func TestLogScrollbackFitsTheFrameLimit(t *testing.T) {
	cases := []struct {
		name    string
		perSpec int
		line    string
	}{
		{"plain 86-column loader lines", DefaultLogBufferBytes, strings.Repeat("x", 85) + "\n"},
		{"ANSI coloured", DefaultLogBufferBytes, "\x1b[32mINFO\x1b[0m " + strings.Repeat("y", 60) + "\n"},
		{"chat template with <|im_start|> and &", DefaultLogBufferBytes, "<|im_start|>system\nyou & me <tag>\n"},
		{"invalid UTF-8", DefaultLogBufferBytes, strings.Repeat("\xff\xfe", 40) + "\n"},
		{"multi-byte UTF-8", DefaultLogBufferBytes, strings.Repeat("模型加载中", 16) + "\n"},
		{"raised buffer, 8 MiB", 8 << 20, strings.Repeat("x", 85) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestLogStore(t, tc.perSpec, tc.perSpec*4)
			fillSpec(t, s, "spec-a", tc.line, tc.perSpec)
			s.SetWatch([]string{"spec-a"}, nil)
			batches := drainAll(t, s)
			if len(batches) == 0 {
				t.Fatal("no batches at all")
			}
			for i, b := range batches {
				if n := int64(len(logFrame(t, b))); n > gwapi.MaxWSFrameBytes {
					t.Fatalf("batch %d (scrollback=%v more=%v) marshals to %d bytes, over the gateway's read limit of %d by %d",
						i, b.Scrollback, b.More, n, gwapi.MaxWSFrameBytes, n-gwapi.MaxWSFrameBytes)
				}
			}
		})
	}
}

// TestLogChunkedReplayIsWholeAndInOrder: chunking is only acceptable if the
// pieces reassemble into exactly the history a single batch would have carried,
// in the same order, with the flags a reader needs to reset once and append
// after.
func TestLogChunkedReplayIsWholeAndInOrder(t *testing.T) {
	const perSpec = 4 << 20
	s := newTestLogStore(t, perSpec, perSpec*2)
	// Distinct, ordered lines, so a lost or reordered chunk is visible rather
	// than plausible.
	p := s.newProc("spec-a")
	p.Started(7, ResolvedCommand{Binary: "/opt/x"})
	var want strings.Builder
	for i := range 40000 {
		line := "line-" + itoa(i) + "-" + strings.Repeat("z", 100) + "\n"
		if _, err := p.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
		want.WriteString(line)
	}

	s.SetWatch([]string{"spec-a"}, nil)
	batches := drainAll(t, s)
	if len(batches) < 2 {
		t.Fatalf("got %d batches; this history is meant to need chunking", len(batches))
	}
	var got strings.Builder
	for i, b := range batches {
		if !b.Scrollback {
			t.Fatalf("batch %d is not flagged Scrollback; every chunk of a replay must be, so the gateway can route it", i)
		}
		if wantMore := i < len(batches)-1; b.More != wantMore {
			t.Fatalf("batch %d More = %v, want %v", i, b.More, wantMore)
		}
		got.WriteString(batchText(b))
	}
	// The buffer trimmed its front, so the replay is a SUFFIX of what was
	// written -- and the eviction has to be reported on the very first chunk.
	if batches[0].Entries[0].DroppedBytes <= 0 {
		t.Fatal("the first chunk carries no dropped_bytes; the evicted prefix would read as silence")
	}
	if !strings.HasSuffix(want.String(), got.String()) {
		t.Fatalf("the reassembled replay is not a suffix of what the process printed (%d bytes reassembled)", got.Len())
	}
	if got.Len() < perSpec/2 {
		t.Fatalf("reassembled only %d bytes of a %d-byte buffer", got.Len(), perSpec)
	}
}

// TestLogSplitNeverBreaksARune: a chunk boundary that landed mid-rune would
// hand encoding/json invalid UTF-8, which it replaces with U+FFFD -- corrupting
// a character the process actually printed, in a stream whose whole value is
// being an exact record of what it printed.
func TestLogSplitNeverBreaksARune(t *testing.T) {
	r := logRecord{text: strings.Repeat("模", 4096)}
	head, tail, ok := splitRecord(r, logEntryWireOverhead+2+3*777)
	if !ok {
		t.Fatal("splitRecord refused a splittable record")
	}
	if !utf8.ValidString(head.text) || !utf8.ValidString(tail.text) {
		t.Fatal("a split produced invalid UTF-8")
	}
	if head.text+tail.text != r.text {
		t.Fatal("a split lost or duplicated text")
	}
}

// TestJSONStringMaxBytesIsNeverUnderTheTruth is the load-bearing property of
// the whole frame guarantee: takeChunk's arithmetic is only a guarantee if this
// bound is never optimistic, for any input. Checked against encoding/json
// itself, over every byte value, every escape, invalid UTF-8, and the two
// line/paragraph separators it treats specially.
func TestJSONStringMaxBytesIsNeverUnderTheTruth(t *testing.T) {
	var all strings.Builder
	for b := range 256 {
		all.WriteByte(byte(b))
	}
	cases := []string{
		"", "plain ascii output", "tab\tnewline\ncarriage\rquote\"back\\slash",
		"<|im_start|>", "a & b", "\x00\x01\x1f", "\xff\xfe\xfd", "\xe2\x80\xa8\xe2\x80\xa9",
		"模型加载中", "🚀 emoji", "mixed \xff 模 <tag> \n", all.String(),
		strings.Repeat("\xf0\x9f\x9a\x80", 64),
	}
	for _, s := range cases {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		if got, want := jsonStringMaxBytes(s), len(raw); got < want {
			t.Fatalf("jsonStringMaxBytes(%q) = %d, UNDER the %d bytes encoding/json emits", s, got, want)
		}
	}
}

// TestLogWireReservesAreNeverUnderTheTruth checks the FIXED half of the frame
// arithmetic the way TestJSONStringMaxBytesIsNeverUnderTheTruth checks the
// variable half: against encoding/json itself. logEntryWireOverhead,
// logCommandWireOverhead and logBatchWireOverhead are hand-counted reserves, and
// a hand-counted reserve that is one byte short is exactly the class of mistake
// that produced this whole round.
func TestLogWireReservesAreNeverUnderTheTruth(t *testing.T) {
	widest := logRecord{
		gen:      1,
		pid:      1 << 62,
		at:       mustParseWidestTime(t),
		text:     "output <&> \"quoted\"\n\x00\xff",
		exitCode: -2147483648,
	}
	marker := widest
	marker.event = logEventStartFailed
	marker.text = ""
	marker.command = &ResolvedCommand{
		Binary:    strings.Repeat("b", 300),
		WorkDir:   strings.Repeat("w", 300),
		Args:      []string{"--a", "<x>", strings.Repeat("q", 500)},
		Env:       []string{"K=V", "J=\x00"},
		Masked:    true,
		Truncated: true,
	}
	for name, recs := range map[string][]logRecord{
		"output":          {widest},
		"marker":          {marker},
		"both":            {widest, marker},
		"empty":           nil,
		"repeated":        {widest, widest, marker, widest},
		"drop-only":       nil,
		"long spec ident": {widest},
	} {
		t.Run(name, func(t *testing.T) {
			specID := strings.Repeat("s", 128)
			entries, rest, used := takeChunk(specID, recs, 1<<62, maxLogBatchBytes)
			if rest != nil {
				t.Fatalf("unexpected remainder: %+v", rest)
			}
			raw, err := json.Marshal(LogBatch{SpecID: specID, Scrollback: true, More: true, Entries: entries})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if len(raw) > used {
				t.Fatalf("the batch marshals to %d bytes but takeChunk reserved only %d; a fixed reserve is short", len(raw), used)
			}
			if n := len(logFrame(t, LogBatch{SpecID: specID, Entries: entries})); n > used+logFrameEnvelopeBytes {
				t.Fatalf("the frame comes to %d bytes, over the %d reserved for it", n, used+logFrameEnvelopeBytes)
			}
		})
	}
}

func mustParseWidestTime(t *testing.T) time.Time {
	t.Helper()
	// The widest RFC3339Nano encoding/json emits: full nanoseconds, and a
	// numeric offset rather than "Z".
	ts, err := time.Parse(time.RFC3339Nano, "2026-08-28T12:34:56.123456789-07:00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return ts
}

// TestLogEpochReSnapshotsAnAlreadyWatchedSpec is the agent half of D2: a
// replay is owed to a VIEWER, and a viewer arriving does not change the watch
// set. Without the epoch, the second SetWatch here yields nothing at all.
func TestLogEpochReSnapshotsAnAlreadyWatchedSpec(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes*4)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{Binary: "/opt/x"})
	p.Write([]byte("history\n"))

	s.SetWatch([]string{"spec-a"}, map[string]uint64{"spec-a": 1})
	if got := scrollbackText(drainAll(t, s)); !strings.Contains(got, "history") {
		t.Fatalf("first viewer got %q, want the history", got)
	}
	p.Write([]byte("more\n"))

	// A second viewer arrives: the set is byte-identical, only the epoch moved.
	s.SetWatch([]string{"spec-a"}, map[string]uint64{"spec-a": 2})
	got := drainAll(t, s)
	replay := scrollbackText(got)
	if !strings.Contains(replay, "history") || !strings.Contains(replay, "more") {
		t.Fatalf("second viewer got %q, want the whole retained history", replay)
	}
	// And the same command re-sent changes nothing, so a restate is free.
	s.SetWatch([]string{"spec-a"}, map[string]uint64{"spec-a": 2})
	if b := s.Drain(); len(b) != 0 {
		t.Fatalf("an unchanged command produced %+v; every frame must be idempotent", b)
	}
}

// TestLogReconnectRepairsOutputLostWhileDisconnected is D4, which collapses
// into D2's mechanism rather than needing one of its own.
//
// The 250 ms flush ticker keeps firing while the WebSocket is down. Drain has
// already removed those records from the send queue -- and their drop counters
// with them -- by the time PostRuntimeLog fails, so nothing downstream will
// ever emit a gap marker for them. What repairs it is the gateway restating the
// watch set with a bumped epoch on the next connection: the retention buffer
// still holds every byte, because Drain never touches it.
func TestLogReconnectRepairsOutputLostWhileDisconnected(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes*4)
	p := s.newProc("spec-a")
	p.Started(9, ResolvedCommand{Binary: "/opt/x"})
	s.SetWatch([]string{"spec-a"}, map[string]uint64{"spec-a": 1})
	drainAll(t, s) // the subscribe's replay, delivered

	p.Write([]byte("during-the-outage\n"))
	if b := s.Drain(); len(b) == 0 {
		t.Fatal("nothing was drained during the outage; the test is not exercising the loss")
	} // ...and the caller could not send it: discarded.

	// The connection comes back; the gateway restates with a bumped epoch.
	s.SetWatch([]string{"spec-a"}, map[string]uint64{"spec-a": 2})
	if got := scrollbackText(drainAll(t, s)); !strings.Contains(got, "during-the-outage") {
		t.Fatalf("the reconnect replay = %q, want the output lost while the connection was down", got)
	}
}

// TestLogFrameFitsTheGatewayReadLimit is the highest-value test of this round:
// the gateway ACTUALLY READS what the agent ACTUALLY WRITES. A real
// coder/websocket peer, with a real SetReadLimit, fed real full-buffer frames.
//
// It could not be written as a single test spanning both halves: the gateway is
// a separate Go module with no dependency on this one, so nothing here can
// import its handler. What this does instead is stand up the identical peer --
// same library, same version, same limit -- and then pin the gateway's own
// literal by reading its source, so a change on either side of the seam fails
// here. (Skipped, loudly, when the gateway tree is not checked out beside this
// one; in CI and in a normal clone it always is.)
func TestLogFrameFitsTheGatewayReadLimit(t *testing.T) {
	s := newTestLogStore(t, DefaultLogBufferBytes, DefaultLogBufferTotalBytes)
	fillSpec(t, s, "spec-a", "<|im_start|>"+strings.Repeat("x", 74)+"\n", DefaultLogBufferBytes)
	s.SetWatch([]string{"spec-a"}, nil)
	batches := drainAll(t, s)
	frames := make([][]byte, 0, len(batches))
	for _, b := range batches {
		frames = append(frames, logFrame(t, b))
	}

	readErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			readErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		// Exactly what handleAgentStream does.
		conn.SetReadLimit(gwapi.MaxWSFrameBytes)
		for range frames {
			if _, _, err := conn.Read(context.Background()); err != nil {
				readErr <- err
				return
			}
		}
		readErr <- nil
	}))
	defer srv.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for i, f := range frames {
		if err := conn.Write(ctx, websocket.MessageText, f); err != nil {
			t.Fatalf("agent write of frame %d (%d bytes) failed: %v", i, len(f), err)
		}
	}
	if err := <-readErr; err != nil {
		t.Fatalf("the gateway's read loop rejected a frame the agent wrote: %v", err)
	}
}

// TestGatewayReadLimitMatchesThisModulesCap pins the other end of the contract
// gwapi.MaxWSFrameBytes documents. Two Go modules, so no compiler can hold them
// together; this reads the gateway's constant out of its source instead. Its
// counterpart there is TestAgentFrameLimitMatchesTheAgentsOwnCap.
func TestGatewayReadLimitMatchesThisModulesCap(t *testing.T) {
	const path = "../../../gateway/backend/internal/gateway/agent_stream.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("gateway module not checked out beside this one (%v); the seam is unverified in this run", err)
	}
	m := regexp.MustCompile(`(?m)^const maxAgentFrameBytes int64 = (.+)$`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("maxAgentFrameBytes is no longer declared as a plain const in %s; this pin needs updating, and so does gwapi.MaxWSFrameBytes's doc", path)
	}
	if got, want := strings.TrimSpace(string(m[1])), "1 << 20"; got != want {
		t.Fatalf("the gateway reads at most %s per frame but this module builds frames against %s (gwapi.MaxWSFrameBytes = %d). "+
			"They are one contract: change both, and never lower the gateway's.", got, want, gwapi.MaxWSFrameBytes)
	}
}

// scrollbackText joins the text of every replay chunk in order.
func scrollbackText(batches []LogBatch) string {
	var sb strings.Builder
	for _, b := range batches {
		if b.Scrollback {
			sb.WriteString(batchText(b))
		}
	}
	return sb.String()
}

// itoa avoids pulling strconv into this file for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
