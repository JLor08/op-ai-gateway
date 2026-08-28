// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"sync"
	"testing"
	"time"
)

// logPoster is a poster that ALSO carries the two log-streaming halves a
// WebSocket sender provides: the gateway's watch command coming down, and log
// frames going up. Modelling both on one value is not a shortcut -- it is what
// production looks like (*client.WSSender is the only implementation of
// either), and it is what makes the all-or-nothing derivation in NewFromDeps
// testable.
type logPoster struct {
	mu     sync.Mutex
	frames []json.RawMessage
	err    error
	wake   chan json.RawMessage
	posted chan struct{}
}

func newLogPoster() *logPoster {
	return &logPoster{wake: make(chan json.RawMessage, 1), posted: make(chan struct{}, 64)}
}

func (p *logPoster) Post(_ context.Context, _ *sample.Sample) error { return nil }

func (p *logPoster) LogWatchUpdates() <-chan json.RawMessage { return p.wake }

func (p *logPoster) PostRuntimeLog(_ context.Context, raw json.RawMessage) error {
	p.mu.Lock()
	p.frames = append(p.frames, raw)
	err := p.err
	p.mu.Unlock()
	select {
	case p.posted <- struct{}{}:
	default:
	}
	return err
}

func (p *logPoster) sent() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.frames...)
}

func (p *logPoster) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

// logDriver is a runtime driver that ALSO implements the log port.
type logDriver struct {
	fakeRuntimeDriver
	mu       sync.Mutex
	commands []json.RawMessage
	queued   []json.RawMessage
	applied  chan struct{}
}

func newLogDriver() *logDriver {
	d := &logDriver{applied: make(chan struct{}, 16)}
	d.release = make(chan struct{})
	return d
}

func (d *logDriver) SetLogWatch(raw json.RawMessage) {
	d.mu.Lock()
	d.commands = append(d.commands, raw)
	d.mu.Unlock()
	select {
	case d.applied <- struct{}{}:
	default:
	}
}

func (d *logDriver) DrainLogFrames() []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.queued
	d.queued = nil
	return out
}

func (d *logDriver) queue(frames ...json.RawMessage) {
	d.mu.Lock()
	d.queued = append(d.queued, frames...)
	d.mu.Unlock()
}

func (d *logDriver) seenCommands() []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]json.RawMessage(nil), d.commands...)
}

// shrinkLogFlush drives the flush cadence down so a test does not wait out the
// production window.
func shrinkLogFlush(t *testing.T) {
	t.Helper()
	orig := runtimeLogFlushInterval
	runtimeLogFlushInterval = 5 * time.Millisecond
	t.Cleanup(func() { runtimeLogFlushInterval = orig })
}

func runAgent(t *testing.T, a *Agent) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return cancel
}

// TestRunAppliesTheGatewayLogWatchCommand: the command arrives on the poster's
// wake channel and must reach the driver, which is the only thing that can act
// on it.
func TestRunAppliesTheGatewayLogWatchCommand(t *testing.T) {
	shrinkLogFlush(t)
	poster := newLogPoster()
	drv := newLogDriver()
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour},
		Deps{Poster: poster, RuntimeDriver: drv})
	runAgent(t, a)

	cmd := json.RawMessage(`{"spec_ids":["spec-a"]}`)
	poster.wake <- cmd
	select {
	case <-drv.applied:
	case <-time.After(3 * time.Second):
		t.Fatal("the gateway's watch command never reached the driver")
	}
	got := drv.seenCommands()
	if len(got) != 1 || string(got[0]) != string(cmd) {
		t.Fatalf("commands = %v, want exactly the pushed one", got)
	}
}

// TestRunFlushesQueuedLogFrames: whatever the driver has queued goes up on the
// flush cadence, one PostRuntimeLog call per frame.
func TestRunFlushesQueuedLogFrames(t *testing.T) {
	shrinkLogFlush(t)
	poster := newLogPoster()
	drv := newLogDriver()
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour},
		Deps{Poster: poster, RuntimeDriver: drv})
	runAgent(t, a)

	drv.queue(
		json.RawMessage(`{"spec_id":"spec-a","entries":[{"text":"a"}]}`),
		json.RawMessage(`{"spec_id":"spec-b","entries":[{"text":"b"}]}`),
	)
	deadline := time.After(3 * time.Second)
	for len(poster.sent()) < 2 {
		select {
		case <-poster.posted:
		case <-deadline:
			t.Fatalf("only %d frames were sent, want 2", len(poster.sent()))
		}
	}
	sent := poster.sent()
	if string(sent[0]) != `{"spec_id":"spec-a","entries":[{"text":"a"}]}` {
		t.Fatalf("frame 0 = %s, want the driver's payload verbatim", sent[0])
	}
}

// TestRunSendsNothingWhenNothingIsQueued is the "no traffic on an unwatched
// fleet" property at the loop level: the flush ticker runs regardless (it is a
// wakeup, not traffic), and must produce no frames at all when the driver has
// nothing.
func TestRunSendsNothingWhenNothingIsQueued(t *testing.T) {
	shrinkLogFlush(t)
	poster := newLogPoster()
	drv := newLogDriver()
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour},
		Deps{Poster: poster, RuntimeDriver: drv})
	runAgent(t, a)

	time.Sleep(150 * time.Millisecond) // ~30 flush windows
	if got := poster.sent(); len(got) != 0 {
		t.Fatalf("%d frames were sent with nothing queued: %v", len(got), got)
	}
}

// TestRunStopsFlushingAfterASendFailure: one failure means the connection is
// gone, so the rest of that batch would fail identically -- and hammering a
// dead connection with the remainder is exactly what would delay the
// reconnect the frames are waiting for.
func TestRunStopsFlushingAfterASendFailure(t *testing.T) {
	shrinkLogFlush(t)
	poster := newLogPoster()
	poster.setErr(context.DeadlineExceeded)
	drv := newLogDriver()
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour},
		Deps{Poster: poster, RuntimeDriver: drv})
	runAgent(t, a)

	drv.queue(
		json.RawMessage(`{"spec_id":"a","entries":[]}`),
		json.RawMessage(`{"spec_id":"b","entries":[]}`),
		json.RawMessage(`{"spec_id":"c","entries":[]}`),
	)
	select {
	case <-poster.posted:
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was attempted")
	}
	time.Sleep(100 * time.Millisecond)
	if got := poster.sent(); len(got) != 1 {
		t.Fatalf("%d frames attempted after the first failure, want exactly 1", len(got))
	}
}

// TestLogStreamingIsAllOrNothing: a poster with no log support, or a driver
// with no log port, must disable the whole path -- including the ticker --
// rather than leaving Run's select ready to fire on a half-wired feature.
// Getting this wrong is a nil-interface panic in the agent's hot loop.
func TestLogStreamingIsAllOrNothing(t *testing.T) {
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}

	t.Run("poster without log support", func(t *testing.T) {
		a := NewFromDeps(cfg, Deps{Poster: &fakeReporterPoster{}, RuntimeDriver: newLogDriver()})
		if a.runtimeLogWake != nil || a.runtimeLogPort != nil || a.runtimeLogPoster != nil {
			t.Fatal("log streaming was half-wired against a poster that cannot carry frames")
		}
		if ticker, ch := a.newRuntimeLogTicker(); ticker != nil || ch != nil {
			t.Fatal("a flush ticker was created with nothing to flush to")
		}
	})

	t.Run("driver without the log port", func(t *testing.T) {
		a := NewFromDeps(cfg, Deps{Poster: newLogPoster(), RuntimeDriver: newFakeRuntimeDriver()})
		if a.runtimeLogWake != nil || a.runtimeLogPort != nil || a.runtimeLogPoster != nil {
			t.Fatal("log streaming was half-wired against a driver that cannot produce frames")
		}
	})

	t.Run("no driver at all", func(t *testing.T) {
		a := NewFromDeps(cfg, Deps{Poster: newLogPoster()})
		if a.runtimeLogPort != nil {
			t.Fatal("log streaming was wired with no runtime driver")
		}
	})

	t.Run("both present", func(t *testing.T) {
		a := NewFromDeps(cfg, Deps{Poster: newLogPoster(), RuntimeDriver: newLogDriver()})
		if a.runtimeLogWake == nil || a.runtimeLogPort == nil || a.runtimeLogPoster == nil {
			t.Fatal("log streaming stayed disabled with everything present")
		}
	})
}
