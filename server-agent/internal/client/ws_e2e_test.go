// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/agent"
	"op-ai-server-agent/internal/certinstall"
	"op-ai-server-agent/internal/client"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// recordingGateway accepts WS connections and counts telemetry frames; it closes the
// FIRST connection with GoingAway after one frame to force a reconnect.
type recordingGateway struct {
	mu        sync.Mutex
	frames    int
	conns     int
	killFirst bool
}

func (g *recordingGateway) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	g.mu.Lock()
	g.conns++
	kill := g.killFirst && g.conns == 1
	g.mu.Unlock()
	for {
		var raw map[string]any
		if err := wsjson.Read(r.Context(), c, &raw); err != nil {
			return
		}
		g.mu.Lock()
		g.frames++
		g.mu.Unlock()
		if kill {
			_ = c.Close(websocket.StatusGoingAway, "restart")
			return
		}
	}
}

func (g *recordingGateway) count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.frames }

func TestAgentWebSocketEndToEndWithReconnect(t *testing.T) {
	g := &recordingGateway{killFirst: true}
	ts := httptest.NewServer(http.HandlerFunc(g.handler))
	defer ts.Close()

	ws, err := client.NewWSSender("http"+ts.URL[len("http"):], "tok", nil)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	defer ws.Close()

	cfg := config.Config{GatewayURL: ts.URL, Token: "tok", Interval: 50 * time.Millisecond, Transport: config.TransportWebSocket}
	a := agent.New(cfg, fakeHost{}, nil, nil, nil, noPower{}, nil, ws, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go a.Run(ctx)

	// After the gateway kills the first connection, the agent must reconnect and keep
	// delivering: require >= 2 total frames across (at least) 2 connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if g.count() >= 2 {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("only %d frames delivered; reconnect did not resume streaming", g.count())
}

// killAfterGateway accepts WebSocket connections and, ONLY for the first one,
// closes it with GoingAway after a short deterministic delay -- long enough
// for the agent's startup certificate sync and the first connect's own wake
// to settle, so the count increase attributable to the SECOND connection
// (the reconnect) can be isolated from everything that happened around the
// first. It never pushes a cert_update frame itself: the whole point of this
// test is that a reconnect ALONE (no doorbell at all) wakes exactly one
// certificate sync.
type killAfterGateway struct {
	mu    sync.Mutex
	conns int
}

func (g *killAfterGateway) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	g.mu.Lock()
	g.conns++
	first := g.conns == 1
	g.mu.Unlock()
	if first {
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = c.Close(websocket.StatusGoingAway, "restart")
		}()
	}
	for {
		var raw map[string]any
		if err := wsjson.Read(r.Context(), c, &raw); err != nil {
			return
		}
	}
}

func (g *killAfterGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conns
}

// e2eCertSyncer is a minimal, near-instant certSyncer test double (structural
// interface satisfaction: agent.New's certSync parameter is an unexported
// interface type, but any value implementing its method set -- Sync/Report --
// satisfies it from any package, including this one). It counts Sync calls
// under a mutex.
type e2eCertSyncer struct {
	mu    sync.Mutex
	calls int
}

func (f *e2eCertSyncer) Sync(context.Context) (certinstall.Report, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return certinstall.Report{Mode: config.CertModeFiles}, true, nil
}

func (f *e2eCertSyncer) Report() certinstall.Report {
	return certinstall.Report{Mode: config.CertModeFiles}
}

func (f *e2eCertSyncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// waitForClientCondition polls cond until it is true or the deadline elapses.
func waitForClientCondition(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// waitStableCertSyncCount polls get() until it has not changed for quiet, or
// fails at timeout. Used to capture a settled baseline/after value around an
// async event (the startup sync, the first-connect wake, and a reconnect wake
// can all fire close together and are not individually orderable), so what
// this test actually measures is the DELTA a reconnect contributes, not any
// particular absolute total.
func waitStableCertSyncCount(t *testing.T, get func() int, quiet, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := get()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		cur := get()
		if cur != last {
			last = cur
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return last
		}
	}
	t.Fatalf("cert sync count never stabilized within %s (last=%d)", timeout, last)
	return 0
}

// TestAgentReconnectWithoutCertUpdateFrameTriggersExactlyOneCertSync is the
// Task 5b requirement "Reconnect ohne cert_update-Frame löst genau einen Sync
// aus": a WebSocket reconnect that carries NO cert_update doorbell at all
// (killAfterGateway never sends one) must still wake exactly one additional
// certificate sync -- proving the wake in maybeDial's success path, not
// readLoop's frame handling, is what triggers it.
func TestAgentReconnectWithoutCertUpdateFrameTriggersExactlyOneCertSync(t *testing.T) {
	g := &killAfterGateway{}
	ts := httptest.NewServer(http.HandlerFunc(g.handler))
	defer ts.Close()

	ws, err := client.NewWSSender("http"+ts.URL[len("http"):], "tok", nil)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	defer ws.Close()

	certSync := &e2eCertSyncer{}
	cfg := config.Config{
		GatewayURL:       ts.URL,
		Token:            "tok",
		Interval:         30 * time.Millisecond,
		Transport:        config.TransportWebSocket,
		CertMode:         config.CertModeFiles,
		CertPollInterval: time.Hour, // keep the periodic ticker out of this test's window entirely
	}
	a := agent.New(cfg, fakeHost{}, nil, nil, nil, noPower{}, nil, ws, certSync)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go a.Run(ctx)

	// Wait for the first connection, then let whatever it triggered (the
	// startup sync + the first-connect wake, which may or may not coalesce
	// depending on exact timing) settle to a stable count.
	waitForClientCondition(t, 3*time.Second, func() bool { return g.count() >= 1 })
	before := waitStableCertSyncCount(t, certSync.count, 300*time.Millisecond, 3*time.Second)

	// The gateway kills that first connection ~150ms in (no cert_update frame
	// ever sent); wait for the reconnect (second connection), then let its
	// wake settle.
	waitForClientCondition(t, 3*time.Second, func() bool { return g.count() >= 2 })
	after := waitStableCertSyncCount(t, certSync.count, 300*time.Millisecond, 3*time.Second)

	if after-before != 1 {
		t.Fatalf("cert sync count delta across the reconnect = %d (before=%d after=%d), want exactly 1", after-before, before, after)
	}
}

// fakeHost + noPower satisfy the collector interfaces for a minimal Agent.
type fakeHost struct{}

func (fakeHost) Collect(context.Context) (*sample.Host, error) {
	return &sample.Host{CPUUtilPct: 3}, nil
}

type noPower struct{}

func (noPower) Name() string    { return "none" }
func (noPower) Available() bool { return false }
func (noPower) Collect(context.Context) (*float64, *float64, error) {
	return nil, nil, fmt.Errorf("unavailable")
}

var (
	_ collector.HostCollector  = fakeHost{}
	_ collector.PowerCollector = noPower{}
)
