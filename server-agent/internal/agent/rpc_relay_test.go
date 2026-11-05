// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"op-ai-server-agent/internal/config"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// fakeMembers records the holds the relay reports and the gate it installs.
type fakeMembers struct {
	mu    sync.Mutex
	holds []string
	gate  func(string) error
}

func (f *fakeMembers) SetMemberHold(specID, holder string, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if holder != "" && since.IsZero() {
		holder += "(no since)"
	}
	f.holds = append(f.holds, specID+"="+holder)
}

func (f *fakeMembers) SetMemberRelayGate(gate func(string) error) {
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
}

func (f *fakeMembers) holdLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.holds)
}

// statusTable is a settable runtime status.
type statusTable struct {
	mu   sync.Mutex
	rows []runtimectl.Status
}

func (s *statusTable) set(rows ...runtimectl.Status) {
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
}

func (s *statusTable) get() []runtimectl.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rows)
}

// echoChild is a stand-in for a member's loopback rpc-server.
func echoChild(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func relayPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func relayMemberRow(id string, state runtimectl.State, childPort, relayPort int, headIPs ...string) runtimectl.Status {
	return runtimectl.Status{
		SpecID: id, State: state, Port: childPort, Kind: runtimectl.SpecKindRPCServer,
		RPCMember: &runtimectl.RPCMember{
			HeadSpecID: "rspec_head", Transport: "relay", BindIP: "127.0.0.1", Port: relayPort, HeadIPs: headIPs,
		},
	}
}

func newTestRelayLink(t *testing.T, table *statusTable, members *fakeMembers) *relayLink {
	t.Helper()
	l := newRelayLink(table.get, members)
	t.Cleanup(l.close)
	return l
}

func relayRoundTrip(t *testing.T, port int, msg string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	c.Write([]byte(msg))
	got := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
		t.Fatalf("relay echo = %q, %v; want %q", got, err, msg)
	}
	return c
}

func relayAccepts(port int) bool {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRelayLinkRelaysOnlyRelayMembers: relays come up for relay-mode members
// with a child (starting or running) and for nothing else.
func TestRelayLinkRelaysOnlyRelayMembers(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	child := echoChild(t)
	running, starting, stopped, direct := relayPort(t), relayPort(t), relayPort(t), relayPort(t)
	directRow := relayMemberRow("rpcm_direct", runtimectl.StateRunning, child, direct, "127.0.0.1")
	directRow.RPCMember.Transport = "direct"
	table.set(
		relayMemberRow("rpcm_run", runtimectl.StateRunning, child, running, "127.0.0.1"),
		relayMemberRow("rpcm_start", runtimectl.StateStarting, child, starting, "127.0.0.1"),
		relayMemberRow("rpcm_stopped", runtimectl.StateStopped, 0, stopped, "127.0.0.1"),
		directRow,
		runtimectl.Status{SpecID: "model", State: runtimectl.StateRunning, Port: child},
	)
	l := newTestRelayLink(t, table, members)
	l.sync()

	for name, port := range map[string]int{"running": running, "starting": starting} {
		if !relayAccepts(port) {
			t.Fatalf("no relay for the %s member", name)
		}
	}
	for name, port := range map[string]int{"stopped": stopped, "direct": direct} {
		if relayAccepts(port) {
			t.Fatalf("a relay listens for the %s member", name)
		}
	}

	// The member drains: its relay goes, and its sessions with it.
	c := relayRoundTrip(t, running, "x")
	table.set(relayMemberRow("rpcm_run", runtimectl.StateDraining, child, running, "127.0.0.1"))
	l.sync()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("session survived its member draining (n=%d, err=%v)", n, err)
	}
	if relayAccepts(running) {
		t.Fatal("relay still listens after its member drained")
	}
}

// TestRelayLinkReportsHolds: the head's first byte holds the member through
// SetMemberHold, and the session's end releases it.
func TestRelayLinkReportsHolds(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	port := relayPort(t)
	table.set(relayMemberRow("rpcm_a", runtimectl.StateRunning, echoChild(t), port, "127.0.0.1"))
	l := newTestRelayLink(t, table, members)
	l.sync()

	c := relayRoundTrip(t, port, "hello")
	waitFor(t, "hold reported", func() bool { return slices.Contains(members.holdLog(), "rpcm_a=rspec_head") })
	c.Close()
	waitFor(t, "hold released", func() bool { return slices.Contains(members.holdLog(), "rpcm_a=") })
}

// TestRelayLinkHoldsOnlyRunningMembers: a starting member's relay listens but
// takes no hold and forwards nothing.
func TestRelayLinkHoldsOnlyRunningMembers(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	port := relayPort(t)
	table.set(relayMemberRow("rpcm_a", runtimectl.StateStarting, echoChild(t), port, "127.0.0.1"))
	l := newTestRelayLink(t, table, members)
	l.sync()

	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("early"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := c.Read(make([]byte, 8)); n != 0 {
		t.Fatal("a starting member's relay forwarded bytes")
	}
	if got := members.holdLog(); len(got) != 0 {
		t.Fatalf("holds = %v, want none for a starting member", got)
	}
}

// TestRelayLinkGateWaitsForListener: the readiness gate brings the relay up
// itself and answers true only while it listens; a bind address that is not
// local answers false and is reported as a bind mismatch.
func TestRelayLinkGateWaitsForListener(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	port := relayPort(t)
	table.set(relayMemberRow("rpcm_a", runtimectl.StateStarting, echoChild(t), port, "127.0.0.1"))
	l := newTestRelayLink(t, table, members)

	if err := l.ready("rpcm_a"); err != nil {
		t.Fatalf("gate = %v for a member whose relay can bind", err)
	}
	if !relayAccepts(port) {
		t.Fatal("gate answered true but nothing listens")
	}
	if l.ready("rpcm_unknown") == nil {
		t.Fatal("gate passed a member without a relay")
	}

	mismatch := relayMemberRow("rpcm_b", runtimectl.StateStarting, echoChild(t), relayPort(t), "127.0.0.1")
	mismatch.RPCMember.BindIP = "192.0.2.10" // TEST-NET-1, never an address of this host
	table.set(mismatch)
	if err := l.ready("rpcm_b"); err == nil || !strings.Contains(err.Error(), "192.0.2.10") {
		t.Fatalf("gate = %v for a relay whose bind address is not local; want an error naming it", err)
	}
	if tel, ok := l.telemetry("rpcm_b"); !ok || !tel.BindMismatch {
		t.Fatalf("telemetry = %+v, %v; want bind_mismatch", tel, ok)
	}
	// The member gives up (start timeout): the mismatch stays visible while
	// its row remains, and goes with it.
	mismatch.State = runtimectl.StateStartFailed
	table.set(mismatch)
	l.sync()
	if tel, _ := l.telemetry("rpcm_b"); !tel.BindMismatch {
		t.Fatal("bind_mismatch cleared while the failed member is still listed")
	}
	table.set()
	l.sync()
	if _, ok := l.telemetry("rpcm_b"); ok {
		t.Fatal("telemetry kept for a member that left the status")
	}
}

// TestRelayLinkRecordsRefusals: a connection from outside the head's
// addresses is refused and recorded as a breach.
func TestRelayLinkRecordsRefusals(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	port := relayPort(t)
	table.set(relayMemberRow("rpcm_a", runtimectl.StateRunning, echoChild(t), port, "192.0.2.1"))
	l := newTestRelayLink(t, table, members)
	l.sync()

	for range maxRelayBreaches + 3 {
		c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		c.Read(make([]byte, 1))
		c.Close()
	}
	waitFor(t, "refusals recorded", func() bool {
		tel, _ := l.telemetry("rpcm_a")
		return tel.Refusals == maxRelayBreaches+3
	})
	tel, _ := l.telemetry("rpcm_a")
	if len(tel.Breaches) != maxRelayBreaches || tel.Breaches[0].Peer != "127.0.0.1" || tel.Breaches[0].At.IsZero() {
		t.Fatalf("breaches = %+v, want the last %d, each naming the peer and time", tel.Breaches, maxRelayBreaches)
	}
	if got := members.holdLog(); len(got) != 0 {
		t.Fatalf("holds = %v, want none from a refused peer", got)
	}
}

// TestRelayLinkHeadDomainResolver: with a head domain the resolver's IPv4
// addresses are allowed too; without one it is never asked.
func TestRelayLinkHeadDomainResolver(t *testing.T) {
	table, members := &statusTable{}, &fakeMembers{}
	port := relayPort(t)
	row := relayMemberRow("rpcm_a", runtimectl.StateRunning, echoChild(t), port)
	row.RPCMember.HeadDomain = "head.example"
	table.set(row)
	l := newTestRelayLink(t, table, members)
	var askedFor sync.Map
	l.headDomainIPs = func(specID string) []net.IP {
		askedFor.Store(specID, true)
		return []net.IP{net.ParseIP("::1"), net.ParseIP("127.0.0.1")}
	}
	l.sync()
	relayRoundTrip(t, port, "via domain")
	if _, ok := askedFor.Load("rpcm_a"); !ok {
		t.Fatal("resolver not asked for the member")
	}

	other := relayMemberRow("rpcm_b", runtimectl.StateRunning, echoChild(t), relayPort(t), "192.0.2.1")
	table.set(other)
	askedFor.Delete("rpcm_b")
	l.sync()
	c, _ := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(other.RPCMember.Port)), 2*time.Second)
	c.Write([]byte("x"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	c.Read(make([]byte, 1))
	c.Close()
	if _, ok := askedFor.Load("rpcm_b"); ok {
		t.Fatal("resolver asked for a member without a head domain")
	}
}

// TestRelayLinkStallTelemetry: a stall is recorded and cleared when the
// session ends.
func TestRelayLinkStallTelemetry(t *testing.T) {
	l := newRelayLink(func() []runtimectl.Status { return nil }, &fakeMembers{})
	defer l.close()
	l.Held("rpcm_a", "rspec_head", time.Now())
	l.Stalled("rpcm_a")
	if tel, _ := l.telemetry("rpcm_a"); !tel.Stalled {
		t.Fatal("stall not recorded")
	}
	l.Released("rpcm_a")
	if tel, _ := l.telemetry("rpcm_a"); tel.Stalled {
		t.Fatal("stall outlived its session")
	}
}

// TestAgentWiresRelayGate: an agent given the member sink installs the relay
// gate, and a runtime sync brings up the relays of the applied members.
func TestAgentWiresRelayGate(t *testing.T) {
	drv := newFakeRuntimeDriver()
	port := relayPort(t)
	drv.setStatuses([]runtimectl.Status{relayMemberRow("rpcm_a", runtimectl.StateRunning, echoChild(t), port, "127.0.0.1")})
	members := &fakeMembers{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{RuntimeDriver: drv, RPCMembers: members})
	t.Cleanup(a.relays.close)

	members.mu.Lock()
	gate := members.gate
	members.mu.Unlock()
	if gate == nil {
		t.Fatal("the agent installed no relay gate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.triggerRuntimeSync(ctx, json.RawMessage(nil))
	waitFor(t, "relay up after a runtime sync", func() bool { return relayAccepts(port) })
	if err := gate("rpcm_a"); err != nil {
		t.Fatalf("gate = %v for a listening relay", err)
	}
}
