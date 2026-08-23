// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package logbuffer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// An error-valued attr must serialize to its message in the JSON/portal view, not
// to "{}" (json.Marshal of a wrapped stdlib error drops the unexported message).
func TestHandlerErrorAttrSerializesToMessage(t *testing.T) {
	b := NewBuffer(10, slog.LevelInfo)
	logger := slog.New(b.Handler(&bytes.Buffer{}))
	logger.Error("lookup failed", "err", fmt.Errorf("wrap: %w", fmt.Errorf("database is locked")))

	recs := b.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if got, ok := recs[0].Attrs["err"].(string); !ok || got != "wrap: database is locked" {
		t.Fatalf("err attr = %#v, want the message string", recs[0].Attrs["err"])
	}
	out, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"err":{}`) {
		t.Fatalf("error attr serialized to {} (message dropped): %s", out)
	}
	if !strings.Contains(string(out), "database is locked") {
		t.Fatalf("error message missing from JSON: %s", out)
	}
}

func recWithMsg(msg string) Record {
	return Record{Time: time.Now(), Level: "INFO", Msg: msg}
}

func TestAppendRingsAndDropsOldest(t *testing.T) {
	b := NewBuffer(3, slog.LevelInfo)
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		b.Append(recWithMsg(m))
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	got := []string{snap[0].Msg, snap[1].Msg, snap[2].Msg}
	want := []string{"c", "d", "e"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ring order = %v, want %v", got, want)
		}
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	b := NewBuffer(4, slog.LevelInfo)
	b.Append(recWithMsg("x"))
	snap := b.Snapshot()
	snap[0].Msg = "mutated"
	if b.Snapshot()[0].Msg != "x" {
		t.Fatalf("Snapshot did not return an independent copy")
	}
}

func TestSubscribeSnapshotThenLiveThenUnsub(t *testing.T) {
	b := NewBuffer(10, slog.LevelInfo)
	b.Append(recWithMsg("seed"))

	snap, ch, unsub := b.Subscribe()
	if len(snap) != 1 || snap[0].Msg != "seed" {
		t.Fatalf("subscribe snapshot = %+v, want one seed record", snap)
	}

	b.Append(recWithMsg("live"))
	select {
	case got := <-ch:
		if got.Msg != "live" {
			t.Fatalf("live record msg = %q, want live", got.Msg)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive live record")
	}

	unsub()
	b.Append(recWithMsg("after-unsub"))
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("received %q after unsubscribe", got.Msg)
		}
	case <-time.After(50 * time.Millisecond):
		// No delivery after unsub is the expected outcome.
	}
}

func TestSubscribeDropsOnFullBuffer(t *testing.T) {
	b := NewBuffer(10, slog.LevelInfo)
	_, ch, unsub := b.Subscribe()
	defer unsub()
	// Overflow the subscriber buffer; Append must never block.
	for i := 0; i < logSubBuffer+50; i++ {
		b.Append(recWithMsg("flood"))
	}
	// Channel holds at most its buffer capacity; the rest were dropped.
	if len(ch) > logSubBuffer {
		t.Fatalf("channel len %d exceeds buffer %d", len(ch), logSubBuffer)
	}
}

func TestNilBufferIsNoOp(t *testing.T) {
	var b *Buffer
	b.Append(recWithMsg("ignored")) // must not panic
	if snap := b.Snapshot(); snap != nil {
		t.Fatalf("nil Snapshot = %v, want nil", snap)
	}
	snap, ch, unsub := b.Subscribe()
	if snap != nil {
		t.Fatalf("nil Subscribe snapshot = %v, want nil", snap)
	}
	unsub() // must not panic
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil Subscribe channel should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("nil Subscribe channel should be closed, not blocking")
	}
	if b.Level() != slog.LevelInfo {
		t.Fatalf("nil Level = %v, want Info", b.Level())
	}
	b.SetLevel(slog.LevelDebug) // must not panic
}

func TestLevelRoundTrip(t *testing.T) {
	b := NewBuffer(10, slog.LevelWarn)
	if b.Level() != slog.LevelWarn {
		t.Fatalf("initial level = %v, want Warn", b.Level())
	}
	b.SetLevel(slog.LevelDebug)
	if b.Level() != slog.LevelDebug {
		t.Fatalf("after SetLevel = %v, want Debug", b.Level())
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		"Info":     slog.LevelInfo,
		"info":     slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"WARN":     slog.LevelWarn,
		"error":    slog.LevelError,
		"ERROR":    slog.LevelError,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLevelString(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "debug",
		slog.LevelInfo:  "info",
		slog.LevelWarn:  "warn",
		slog.LevelError: "error",
	}
	for in, want := range cases {
		if got := LevelString(in); got != want {
			t.Fatalf("LevelString(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLevelStringRoundTrip(t *testing.T) {
	for _, s := range []string{"debug", "info", "warn", "error"} {
		if got := LevelString(ParseLevel(s)); got != s {
			t.Fatalf("round trip %q -> %q", s, got)
		}
	}
}

func TestHandlerGatesOnLevelAndTeesToStderr(t *testing.T) {
	var stderr bytes.Buffer
	b := NewBuffer(100, slog.LevelInfo)
	logger := slog.New(b.Handler(&stderr))

	logger.Debug("suppressed-debug")
	if len(b.Snapshot()) != 0 {
		t.Fatalf("debug record appended while level=Info")
	}
	if strings.Contains(stderr.String(), "suppressed-debug") {
		t.Fatalf("debug line reached stderr while level=Info")
	}

	logger.Info("hello-info", "server_id", "srv-1")
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("info record not appended: len=%d", len(snap))
	}
	if snap[0].Msg != "hello-info" {
		t.Fatalf("record msg = %q", snap[0].Msg)
	}
	if snap[0].Level != "INFO" {
		t.Fatalf("record level = %q, want INFO", snap[0].Level)
	}
	if snap[0].Attrs["server_id"] != "srv-1" {
		t.Fatalf("record attrs = %+v, want server_id=srv-1", snap[0].Attrs)
	}
	if !strings.Contains(stderr.String(), "hello-info") {
		t.Fatalf("info line did not reach stderr: %q", stderr.String())
	}

	b.SetLevel(slog.LevelDebug)
	logger.Debug("now-visible-debug")
	snap = b.Snapshot()
	if snap[len(snap)-1].Msg != "now-visible-debug" {
		t.Fatalf("debug record not appended after SetLevel(Debug)")
	}
}

func TestParseLevelTrace(t *testing.T) {
	if got := ParseLevel("trace"); got != LevelTrace {
		t.Errorf("ParseLevel(trace) = %v, want %v", got, LevelTrace)
	}
	if got := LevelString(LevelTrace); got != "trace" {
		t.Errorf("LevelString(LevelTrace) = %q, want trace", got)
	}
}

func TestTraceLevelGating(t *testing.T) {
	b := NewBuffer(10, slog.LevelInfo) // info: trace lines must NOT be recorded
	logger := slog.New(b.Handler(io.Discard))
	logger.Log(context.Background(), LevelTrace, "span", "name", "X")
	if n := len(b.Snapshot()); n != 0 {
		t.Fatalf("trace line recorded at info level: %d records", n)
	}
	b.SetLevel(LevelTrace)
	logger.Log(context.Background(), LevelTrace, "span", "name", "X")
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("trace line not recorded at trace level: %d", len(snap))
	}
	if snap[0].Level != "TRACE" {
		t.Errorf("record level = %q, want TRACE", snap[0].Level)
	}
}

func TestHandleEnrichesTraceID(t *testing.T) {
	b := NewBuffer(10, slog.LevelDebug)
	logger := slog.New(b.Handler(io.Discard))
	tid, _ := oteltrace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := oteltrace.SpanIDFromHex("0102030405060708")
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: oteltrace.FlagsSampled})
	ctx := oteltrace.ContextWithSpanContext(context.Background(), sc)
	logger.DebugContext(ctx, "hello")
	snap := b.Snapshot()
	if len(snap) != 1 || snap[0].Attrs["trace_id"] != tid.String() || snap[0].Attrs["span_id"] != sid.String() {
		t.Fatalf("trace_id/span_id not stamped: %+v", snap)
	}
}

func TestNewLogWriterAppendsInfoRecord(t *testing.T) {
	var stderr bytes.Buffer
	b := NewBuffer(100, slog.LevelInfo)
	w := b.NewLogWriter(&stderr)

	if _, err := w.Write([]byte("legacy log line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 record, got %d", len(snap))
	}
	if snap[0].Level != "INFO" {
		t.Fatalf("record level = %q, want INFO", snap[0].Level)
	}
	if snap[0].Msg != "legacy log line" {
		t.Fatalf("record msg = %q, want trimmed line", snap[0].Msg)
	}
	if !strings.Contains(stderr.String(), "legacy log line") {
		t.Fatalf("line did not reach stderr")
	}

	// Above the level, the bridge still writes to stderr but does not append.
	b.SetLevel(slog.LevelError)
	if _, err := w.Write([]byte("quiet line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(b.Snapshot()) != 1 {
		t.Fatalf("line appended while level=Error")
	}
	if !strings.Contains(stderr.String(), "quiet line") {
		t.Fatalf("line did not reach stderr while level=Error")
	}
}
