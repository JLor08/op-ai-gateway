// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package logbuffer provides an in-memory ring of recent log Records with an
// SSE-style fan-out and a runtime-adjustable slog level. A single *Buffer backs
// the gateway's default slog handler (tees to stderr AND appends to the ring),
// the stdlib-log bridge writer (captures the legacy log.Printf sites at Info),
// and the system "Logs" portal view (snapshot + live subscription + set-level).
//
// CRITICAL: the bearer/agent token must NEVER be placed into a Record. Callers
// build Records from log attributes that are known token-free; this package
// only stores and fans out whatever it is given.
package logbuffer

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// logSubBuffer is the per-subscriber channel buffer. A slow reader simply drops
// records once the buffer fills; it recovers on its next reconnect snapshot.
// Mirrors serverPerfSubBuffer.
const logSubBuffer = 256

// LevelTrace is a custom slog level BELOW Debug, used for the per-method span
// lines emitted by the tracing SpanProcessor. Selecting it via the Logs level
// endpoint makes those span lines appear in the portal Logs view.
const LevelTrace slog.Level = slog.LevelDebug - 4 // -8

// Record is a single captured log line, JSON-serialisable for the portal.
type Record struct {
	Time  time.Time      `json:"t"`
	Level string         `json:"level"` // "DEBUG"|"INFO"|"WARN"|"ERROR"
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"` // slog attrs, flattened; token never present
}

// Buffer is a bounded ring of recent log Records plus an SSE fan-out and the
// live slog level. All methods are safe for concurrent use, and a nil *Buffer's
// Append/Snapshot/Subscribe/Level/SetLevel are no-ops (Subscribe returns a
// closed channel) so a bare test Server without a Buffer keeps working.
type Buffer struct {
	mu   sync.RWMutex
	ring []Record
	cap  int
	subs map[chan Record]struct{}
	lvl  *slog.LevelVar
}

// NewBuffer builds an empty Buffer holding up to capacity records, with the
// given initial level.
func NewBuffer(capacity int, initial slog.Level) *Buffer {
	if capacity <= 0 {
		capacity = 1
	}
	lvl := new(slog.LevelVar)
	lvl.Set(initial)
	return &Buffer{
		cap:  capacity,
		subs: map[chan Record]struct{}{},
		lvl:  lvl,
	}
}

// Level returns the current live level (Info for a nil Buffer).
func (b *Buffer) Level() slog.Level {
	if b == nil {
		return slog.LevelInfo
	}
	return b.lvl.Level()
}

// SetLevel updates the live level; the shared slog.LevelVar makes it take
// effect immediately for the handler and the log-writer bridge.
func (b *Buffer) SetLevel(l slog.Level) {
	if b == nil {
		return
	}
	b.lvl.Set(l)
}

// Append adds a record to the ring (evicting the oldest beyond the cap) and
// non-blockingly fans it out to live subscribers. Subscriber channels are
// snapshotted under the lock, then delivered outside it so a slow reader never
// blocks the writer (its record is dropped when its buffer is full).
func (b *Buffer) Append(rec Record) {
	if b == nil {
		return
	}
	b.mu.Lock()
	ring := append(b.ring, rec) //nolint:gocritic // deliberately derives a new slice to check/trim against b.cap before storing it back
	if len(ring) > b.cap {
		// Copy into a fresh slice so the evicted prefix's backing array can be
		// reclaimed and the ring's capacity stays bounded.
		trimmed := make([]Record, b.cap)
		copy(trimmed, ring[len(ring)-b.cap:])
		ring = trimmed
	}
	b.ring = ring
	targets := make([]chan Record, 0, len(b.subs))
	for ch := range b.subs {
		targets = append(targets, ch)
	}
	b.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- rec:
		default:
		}
	}
}

// Snapshot returns an independent copy of the current ring (nil for a nil
// Buffer).
func (b *Buffer) Snapshot() []Record {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Record(nil), b.ring...)
}

// Subscribe atomically returns a copy of the current ring plus a channel of
// subsequent records (so nothing is lost between snapshot and registration) and
// an idempotent unsubscribe. A nil Buffer returns a nil snapshot and an
// already-closed channel.
func (b *Buffer) Subscribe() (snapshot []Record, ch <-chan Record, unsub func()) {
	if b == nil {
		closed := make(chan Record)
		close(closed)
		return nil, closed, func() { /* no-op: nil Buffer has no subscriber map to remove from */ }
	}

	out := make(chan Record, logSubBuffer)
	b.mu.Lock()
	snap := append([]Record(nil), b.ring...)
	b.subs[out] = struct{}{}
	b.mu.Unlock()

	unsub = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, out)
	}
	return snap, out, unsub
}

// bufHandler is a slog.Handler that tees to an inner text handler (stderr) and
// appends each handled record to the Buffer. Level gating happens via Enabled,
// which delegates to the inner handler (built with the Buffer's LevelVar), so
// SetLevel changes take effect immediately for both sinks.
type bufHandler struct {
	b     *Buffer
	inner slog.Handler
}

// Handler returns a slog.Handler that writes text lines to stderr AND appends a
// Record to the Buffer, both gated by the Buffer's live level.
func (b *Buffer) Handler(stderr io.Writer) slog.Handler {
	var lvl *slog.LevelVar
	if b != nil {
		lvl = b.lvl
	}
	inner := slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lv, ok := a.Value.Any().(slog.Level); ok && lv <= LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	})
	return &bufHandler{b: b, inner: inner}
}

func (h *bufHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *bufHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	rec := Record{
		Time:  r.Time,
		Level: levelLabel(r.Level),
		Msg:   r.Message,
	}
	attrs := map[string]any{}
	if sc := oteltrace.SpanContextFromContext(ctx); sc.HasTraceID() {
		attrs["trace_id"] = sc.TraceID().String()
		if sc.HasSpanID() {
			attrs["span_id"] = sc.SpanID().String()
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.Resolve().Any()
		// Coerce error values to their message string. json.Marshal of a
		// wrapped stdlib error (*fmt.wrapError etc.) emits only exported fields
		// → "{}" in the portal Logs view, dropping the message; the stderr text
		// sink calls Error(). Stringify here so both sinks agree.
		if e, ok := v.(error); ok {
			v = e.Error()
		}
		attrs[a.Key] = v
		return true
	})
	if len(attrs) > 0 {
		rec.Attrs = attrs
	}
	h.b.Append(rec)
	return err
}

// WithAttrs/WithGroup delegate to the inner handler so the accumulated context
// still decorates the stderr output. For the ring the accumulated attrs/groups
// are best-effort ignored: each appended Record carries only its call-site
// attributes.
func (h *bufHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bufHandler{b: h.b, inner: h.inner.WithAttrs(attrs)}
}

func (h *bufHandler) WithGroup(name string) slog.Handler {
	return &bufHandler{b: h.b, inner: h.inner.WithGroup(name)}
}

// logWriter bridges the stdlib log package into the Buffer: it always tees the
// raw line to stderr and, when Info passes the current level, appends an Info
// Record (the line as Msg). This captures the legacy log.Printf sites.
type logWriter struct {
	b      *Buffer
	stderr io.Writer
}

// NewLogWriter returns an io.Writer suitable for log.SetOutput.
func (b *Buffer) NewLogWriter(stderr io.Writer) io.Writer {
	return &logWriter{b: b, stderr: stderr}
}

func (w *logWriter) Write(p []byte) (int, error) {
	n, err := w.stderr.Write(p)
	if slog.LevelInfo >= w.b.Level() {
		msg := strings.TrimRight(string(p), "\n")
		w.b.Append(Record{Time: time.Now(), Level: slog.LevelInfo.String(), Msg: msg})
	}
	return n, err
}

// ParseLevel maps "debug"/"info"/"warn"/"error" (case-insensitive) to a
// slog.Level, defaulting to Info for anything unrecognised.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LevelString maps a slog.Level to the lowercase "debug"/"info"/"warn"/"error"
// wire string used by the config and the level endpoints.
func LevelString(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "trace"
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// levelLabel is like slog.Level.String but names LevelTrace "TRACE" (slog would
// print "DEBUG-4"). Used for the Record.Level wire string.
func levelLabel(l slog.Level) string {
	if l <= LevelTrace {
		return "TRACE"
	}
	return l.String()
}
