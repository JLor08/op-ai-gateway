// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package rpcrelay

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// recEvents records every relay event in order.
type recEvents struct {
	mu  sync.Mutex
	log []string
}

func (r *recEvents) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recEvents) Held(specID, headSpecID string, since time.Time) {
	if since.IsZero() {
		r.add("held-zero-since:" + specID)
		return
	}
	r.add("held:" + specID + ":" + headSpecID)
}
func (r *recEvents) Released(specID string) { r.add("released:" + specID) }
func (r *recEvents) Refused(specID string, peer net.IP) {
	r.add("refused:" + specID + ":" + peer.String())
}
func (r *recEvents) Stalled(specID string) { r.add("stalled:" + specID) }

func (r *recEvents) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.log)
}

func (r *recEvents) count(s string) int {
	n := 0
	for _, e := range r.snapshot() {
		if e == s {
			n++
		}
	}
	return n
}

func (r *recEvents) waitCount(t *testing.T, s string, want int) {
	t.Helper()
	waitUntil(t, 5*time.Second, fmt.Sprintf("%d x %q (events %v)", want, s, r.snapshot()), func() bool {
		return r.count(s) >= want
	})
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// target is a stand-in for the member's loopback rpc-server.
type target struct {
	ln       net.Listener
	accepted atomic.Int32
	mu       sync.Mutex
	received []byte
}

// newTarget starts a target whose connections are served by serve (nil
// echoes). Every byte a connection receives is also recorded.
func newTarget(t *testing.T, serve func(net.Conn, *target)) *target {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tg := &target{ln: ln}
	if serve == nil {
		serve = echo
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tg.accepted.Add(1)
			go serve(c, tg)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return tg
}

func (tg *target) record(p []byte) {
	tg.mu.Lock()
	tg.received = append(tg.received, p...)
	tg.mu.Unlock()
}

func (tg *target) bytes() []byte {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return slices.Clone(tg.received)
}

func (tg *target) addr() string { return tg.ln.Addr().String() }

// echo writes back everything it reads.
func echo(c net.Conn, tg *target) {
	defer c.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			tg.record(buf[:n])
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// sink reads and records but never answers.
func sink(c net.Conn, tg *target) {
	defer c.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			tg.record(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func allow(ips ...string) func() []net.IP {
	out := make([]net.IP, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.ParseIP(s))
	}
	return func() []net.IP { return out }
}

// switchable is an allowlist a test can change without an Apply, the way a
// re-resolved head domain does.
type switchable struct {
	mu  sync.Mutex
	ips []net.IP
}

func (s *switchable) set(ips ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ips = nil
	for _, ip := range ips {
		s.ips = append(s.ips, net.ParseIP(ip))
	}
}

func (s *switchable) get() []net.IP {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ips)
}

func always(string) bool { return true }

func newTestManager(t *testing.T, ev Events, running func(string) bool) *Manager {
	t.Helper()
	m := NewManager(ev, running)
	t.Cleanup(m.Close)
	return m
}

func relayConfig(t *testing.T, id string, tg *target, allowed func() []net.IP) Config {
	t.Helper()
	return Config{
		SpecID:     id,
		Bind:       net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t))),
		Target:     tg.addr(),
		AllowedIPs: allowed,
		HeadSpecID: "rspec_head",
	}
}

func mustApply(t *testing.T, m *Manager, cfgs ...Config) {
	t.Helper()
	if err := m.Apply(cfgs); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp4", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// roundTrip writes p and reads exactly len(p) bytes back.
func roundTrip(t *testing.T, c net.Conn, p []byte) []byte {
	t.Helper()
	if _, err := c.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(p))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	c.SetReadDeadline(time.Time{})
	return got
}

// wantClosed asserts the relay closes c without sending it a single byte.
func wantClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if n != 0 {
		t.Fatalf("read %d byte(s) %q from a connection the relay should have closed silently", n, buf[:n])
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("connection still open; want it closed by the relay")
	}
	if err == nil {
		t.Fatal("read returned no error; want the connection closed")
	}
}

// wantOpen asserts c stays open (no EOF) for d.
func wantOpen(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 1)
	_, err := c.Read(buf)
	c.SetReadDeadline(time.Time{})
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("connection ended (%v); want it still open", err)
	}
}

// --- behaviour ---------------------------------------------------------------

func TestRelayForwardsBytesBothWays(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	if st, ok := m.Status("rpcm_a"); !ok || !st.Listening || st.BindMismatch {
		t.Fatalf("Status = %+v, %v; want listening without a bind mismatch", st, ok)
	}
	c := dial(t, cfg.Bind)
	if got := roundTrip(t, c, []byte("hello")); string(got) != "hello" {
		t.Fatalf("echo = %q, want hello", got)
	}
	big := make([]byte, 1<<20)
	rand.Read(big)
	if got := roundTrip(t, c, big); !bytes.Equal(got, big) {
		t.Fatal("1 MiB payload came back altered; the relay must copy bytes unparsed")
	}
	if got := ev.count("held:rpcm_a:rspec_head"); got != 1 {
		t.Fatalf("held events = %d, want 1 (events %v)", got, ev.snapshot())
	}
}

func TestRelayRefusesUnlistedSource(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, sink)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("192.0.2.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	c.Write([]byte("secret payload"))
	wantClosed(t, c)
	ev.waitCount(t, "refused:rpcm_a:127.0.0.1", 1)
	time.Sleep(100 * time.Millisecond)
	if n := tg.accepted.Load(); n != 0 {
		t.Fatalf("target accepted %d connection(s); an unlisted source must never reach it", n)
	}
	if got := tg.bytes(); len(got) != 0 {
		t.Fatalf("target received %q from an unlisted source", got)
	}
	if ev.count("held:rpcm_a:rspec_head") != 0 {
		t.Fatalf("an unlisted source took the hold (events %v)", ev.snapshot())
	}
}

func TestRelayEmptyAllowlistRefusesAll(t *testing.T) {
	for name, allowed := range map[string]func() []net.IP{
		"nil func":   nil,
		"empty list": allow(),
		"wildcard":   allow("0.0.0.0"),
	} {
		t.Run(name, func(t *testing.T) {
			ev := &recEvents{}
			tg := newTarget(t, sink)
			m := newTestManager(t, ev, always)
			cfg := relayConfig(t, "rpcm_a", tg, allowed)
			mustApply(t, m, cfg)

			c := dial(t, cfg.Bind)
			c.Write([]byte("x"))
			wantClosed(t, c)
			ev.waitCount(t, "refused:rpcm_a:127.0.0.1", 1)
			if n := tg.accepted.Load(); n != 0 {
				t.Fatalf("target accepted %d connection(s) with an empty allowlist", n)
			}
		})
	}
}

func TestRelayRefusesSecondHead(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	allowed := &switchable{}
	allowed.set("127.0.0.1")
	cfg := relayConfig(t, "rpcm_a", tg, allowed.get)
	mustApply(t, m, cfg)

	first := dial(t, cfg.Bind)
	roundTrip(t, first, []byte("a"))
	second := dial(t, cfg.Bind)
	roundTrip(t, second, []byte("b"))
	// The newer connection from an allowed head replaces the older one.
	wantClosed(t, first)
	roundTrip(t, second, []byte("still here"))
	// The hold continues across the replacement: taken once, never released.
	if got := ev.snapshot(); ev.count("held:rpcm_a:rspec_head") != 1 || ev.count("released:rpcm_a") != 0 {
		t.Fatalf("events = %v, want exactly one held and no release across the replacement", got)
	}

	// A source that is not (or no longer) allowlisted is refused and leaves
	// the established session alone.
	allowed.set("192.0.2.1")
	third := dial(t, cfg.Bind)
	third.Write([]byte("REFUSED"))
	wantClosed(t, third)
	ev.waitCount(t, "refused:rpcm_a:127.0.0.1", 1)
	roundTrip(t, second, []byte("unaffected"))
	if strings.Contains(string(tg.bytes()), "REFUSED") {
		t.Fatalf("target received a refused source's byte: %q", tg.bytes())
	}
}

func TestRelayHoldsOnFirstByteOnlyWhenRunning(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	var running atomic.Bool
	m := newTestManager(t, ev, func(id string) bool { return id == "rpcm_a" && running.Load() })
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	// Not running: the byte is not forwarded and no hold is taken.
	c := dial(t, cfg.Bind)
	c.Write([]byte("x"))
	wantClosed(t, c)
	if n := tg.accepted.Load(); n != 0 {
		t.Fatalf("target dialled %d time(s) for a member that is not running", n)
	}
	if ev.count("held:rpcm_a:rspec_head") != 0 {
		t.Fatalf("hold taken on a member that is not running (events %v)", ev.snapshot())
	}

	// Running: a payload-less connection takes no hold and never reaches
	// the server ...
	running.Store(true)
	probe := dial(t, cfg.Bind)
	time.Sleep(150 * time.Millisecond)
	if ev.count("held:rpcm_a:rspec_head") != 0 || tg.accepted.Load() != 0 {
		t.Fatalf("a connection without payload took the hold or reached the target (events %v, accepted %d)", ev.snapshot(), tg.accepted.Load())
	}
	// ... and its first byte takes it.
	roundTrip(t, probe, []byte("y"))
	ev.waitCount(t, "held:rpcm_a:rspec_head", 1)
	if ev.count("held-zero-since:rpcm_a") != 0 {
		t.Fatal("hold reported without a since time")
	}
}

func TestRelayReleasesOnClose(t *testing.T) {
	t.Run("client closes", func(t *testing.T) {
		ev := &recEvents{}
		tg := newTarget(t, nil)
		m := newTestManager(t, ev, always)
		cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
		mustApply(t, m, cfg)

		c := dial(t, cfg.Bind)
		roundTrip(t, c, []byte("x"))
		c.Close()
		ev.waitCount(t, "released:rpcm_a", 1)
		if got := ev.snapshot(); got[len(got)-1] != "released:rpcm_a" {
			t.Fatalf("events = %v, want the release last", got)
		}
	})
	t.Run("server closes", func(t *testing.T) {
		ev := &recEvents{}
		tg := newTarget(t, func(c net.Conn, tg *target) {
			buf := make([]byte, 1)
			c.Read(buf)
			c.Close()
		})
		m := newTestManager(t, ev, always)
		cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
		mustApply(t, m, cfg)

		c := dial(t, cfg.Bind)
		c.Write([]byte("x"))
		wantClosed(t, c)
		ev.waitCount(t, "released:rpcm_a", 1)
	})
}

func TestRelayStallSignal(t *testing.T) {
	t.Run("unanswered bytes stall", func(t *testing.T) {
		ev := &recEvents{}
		tg := newTarget(t, sink)
		m := newTestManager(t, ev, always)
		cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
		cfg.StallAfter = 150 * time.Millisecond
		mustApply(t, m, cfg)

		c := dial(t, cfg.Bind)
		c.Write([]byte("request"))
		ev.waitCount(t, "stalled:rpcm_a", 1)
		time.Sleep(400 * time.Millisecond)
		if n := ev.count("stalled:rpcm_a"); n != 1 {
			t.Fatalf("stalled signalled %d times for one unanswered request, want once", n)
		}
	})
	t.Run("answered bytes do not stall", func(t *testing.T) {
		ev := &recEvents{}
		tg := newTarget(t, nil)
		m := newTestManager(t, ev, always)
		cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
		cfg.StallAfter = 150 * time.Millisecond
		mustApply(t, m, cfg)

		c := dial(t, cfg.Bind)
		for range 10 {
			roundTrip(t, c, []byte("ping"))
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond) // idle after the last answer
		if n := ev.count("stalled:rpcm_a"); n != 0 {
			t.Fatalf("stalled signalled %d time(s) on an answered session", n)
		}
	})
	t.Run("a steady upload is not a stall", func(t *testing.T) {
		ev := &recEvents{}
		tg := newTarget(t, sink)
		m := newTestManager(t, ev, always)
		cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
		cfg.StallAfter = 150 * time.Millisecond
		mustApply(t, m, cfg)

		c := dial(t, cfg.Bind)
		for range 12 {
			c.Write([]byte("tensor chunk"))
			time.Sleep(50 * time.Millisecond)
		}
		if n := ev.count("stalled:rpcm_a"); n != 0 {
			t.Fatalf("stalled signalled while the head was still sending (no-reply commands stream)")
		}
	})
}

func TestRelayClosesOnAllowlistChange(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	roundTrip(t, c, []byte("x"))

	// The same allowlist, through a new function value, is no change.
	cfg.AllowedIPs = allow("127.0.0.1")
	mustApply(t, m, cfg)
	roundTrip(t, c, []byte("same allowlist"))

	cfg.AllowedIPs = allow("127.0.0.1", "192.0.2.7")
	mustApply(t, m, cfg)
	wantClosed(t, c)
	ev.waitCount(t, "released:rpcm_a", 1)

	// The listener itself stays up for the head's reconnect.
	again := dial(t, cfg.Bind)
	roundTrip(t, again, []byte("reconnected"))
}

func TestRelayBindMismatchRetries(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	var local atomic.Bool
	m.isLocal = func(net.IP) bool { return local.Load() }
	m.retryEvery = 30 * time.Millisecond
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	if st, ok := m.Status("rpcm_a"); !ok || st.Listening || !st.BindMismatch {
		t.Fatalf("Status = %+v, %v; want a bind mismatch while the address is not local", st, ok)
	}
	if c, err := net.DialTimeout("tcp4", cfg.Bind, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("something listens on the bind address although the relay reported a mismatch")
	}

	local.Store(true)
	waitUntil(t, 3*time.Second, "relay bound after the address became local", func() bool {
		st, _ := m.Status("rpcm_a")
		return st.Listening && !st.BindMismatch
	})
	c := dial(t, cfg.Bind)
	roundTrip(t, c, []byte("bound"))
}

func TestRelayCapsAcceptedSockets(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	var open []net.Conn
	for range MaxSockets {
		open = append(open, dial(t, cfg.Bind))
	}
	for _, c := range open {
		wantOpen(t, c, 50*time.Millisecond)
	}
	over := dial(t, cfg.Bind)
	wantClosed(t, over)

	open[0].Close()
	waitUntil(t, 3*time.Second, "a socket slot freed", func() bool {
		c, err := net.DialTimeout("tcp4", cfg.Bind, time.Second)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = c.Read(make([]byte, 1))
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout()
	})
}

func TestRelayFirstByteDeadline(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	m.firstByteTimeout = 100 * time.Millisecond
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	start := time.Now()
	c := dial(t, cfg.Bind)
	wantClosed(t, c)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("idle socket closed after %v, want about the first-byte deadline", waited)
	}
	if n := tg.accepted.Load(); n != 0 {
		t.Fatalf("target dialled %d time(s) for a socket that never sent a byte", n)
	}
}

func TestRelayRemovedMemberClosesSessions(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	roundTrip(t, c, []byte("x"))
	mustApply(t, m) // the member drains or stops
	wantClosed(t, c)
	ev.waitCount(t, "released:rpcm_a", 1)
	if _, ok := m.Status("rpcm_a"); ok {
		t.Fatal("Status still reports a removed relay")
	}
	if c, err := net.DialTimeout("tcp4", cfg.Bind, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("removed relay still accepts connections")
	}
}

func TestRelayReplacedTargetClosesSessions(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	roundTrip(t, c, []byte("x"))
	relaunched := newTarget(t, nil) // the member's child came back on a new port
	cfg.Target = relaunched.addr()
	mustApply(t, m, cfg)
	wantClosed(t, c)
	again := dial(t, cfg.Bind)
	roundTrip(t, again, []byte("y"))
	if relaunched.accepted.Load() != 1 {
		t.Fatalf("the new target accepted %d connection(s), want 1", relaunched.accepted.Load())
	}
}

func TestRelayCloseEndsSessions(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := NewManager(ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	roundTrip(t, c, []byte("x"))
	m.Close()
	wantClosed(t, c)
	if ev.count("released:rpcm_a") != 1 {
		t.Fatalf("events = %v, want the hold released on shutdown", ev.snapshot())
	}
	if err := m.Apply([]Config{cfg}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Apply after Close = %v, want ErrClosed", err)
	}
	m.Close() // idempotent
}

func TestRelayApplyRejectsInvalidConfigs(t *testing.T) {
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	good := relayConfig(t, "rpcm_good", tg, allow("127.0.0.1"))
	bad := []Config{
		{SpecID: "", Bind: good.Bind, Target: tg.addr()},
		{SpecID: "rpcm_host", Bind: "localhost:47001", Target: tg.addr()},
		{SpecID: "rpcm_v6", Bind: "[::1]:47001", Target: tg.addr()},
		{SpecID: "rpcm_port", Bind: "127.0.0.1:0", Target: tg.addr()},
		{SpecID: "rpcm_remote", Bind: "127.0.0.1:47002", Target: "192.0.2.1:9000"},
		{SpecID: "rpcm_dup", Bind: "127.0.0.1:47003", Target: tg.addr()},
		{SpecID: "rpcm_dup", Bind: "127.0.0.1:47004", Target: tg.addr()},
	}
	err := m.Apply(append([]Config{good}, bad...))
	if err == nil {
		t.Fatal("Apply accepted invalid configs")
	}
	for _, id := range []string{"rpcm_host", "rpcm_v6", "rpcm_port", "rpcm_remote", "rpcm_dup"} {
		if _, ok := m.Status(id); ok {
			t.Fatalf("invalid config %s was applied", id)
		}
	}
	if st, ok := m.Status("rpcm_good"); !ok || !st.Listening {
		t.Fatalf("valid config beside invalid ones not applied: %+v, %v", st, ok)
	}
}

func TestRelaySoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped under -short")
	}
	ev := &recEvents{}
	tg := newTarget(t, nil)
	m := newTestManager(t, ev, always)
	cfg := relayConfig(t, "rpcm_a", tg, allow("127.0.0.1"))
	mustApply(t, m, cfg)

	c := dial(t, cfg.Bind)
	msg := make([]byte, 512)
	got := make([]byte, len(msg))
	c.SetDeadline(time.Now().Add(2 * time.Minute))
	for i := range 10_000 {
		rand.Read(msg)
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("round trip %d: write: %v", i, err)
		}
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("round trip %d: read: %v", i, err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("round trip %d: payload altered", i)
		}
	}
	if ev.count("held:rpcm_a:rspec_head") != 1 || ev.count("stalled:rpcm_a") != 0 {
		t.Fatalf("events = %v, want one hold and no stall over the soak", ev.snapshot())
	}
}
