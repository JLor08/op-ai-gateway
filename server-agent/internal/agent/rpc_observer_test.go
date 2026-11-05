// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"errors"
	"net/netip"
	"op-ai-server-agent/internal/collector"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

var (
	obsBind = netip.MustParseAddrPort("100.64.0.5:47001")
	obsHead = netip.MustParseAddr("100.64.0.2")
	obsOwn  = netip.MustParseAddr("100.64.0.5")
	obsFoe  = netip.MustParseAddr("100.64.0.9")
)

func connFrom(remote netip.Addr, port uint16) collector.Conn {
	return collector.Conn{Local: obsBind, Remote: netip.AddrPortFrom(remote, port)}
}

func TestClassifyConnsHeadHolds(t *testing.T) {
	obs := classifyConns([]collector.Conn{connFrom(obsHead, 5000)}, obsBind, []netip.Addr{obsHead}, []netip.Addr{obsOwn}, "rspec_h")
	if obs.HeldBy != "rspec_h" || len(obs.Breaches) != 0 {
		t.Fatalf("obs = %+v", obs)
	}
}

func TestClassifyConnsForeignPeerIsBreachNotHolder(t *testing.T) {
	obs := classifyConns([]collector.Conn{connFrom(obsFoe, 5000)}, obsBind, []netip.Addr{obsHead}, nil, "rspec_h")
	if obs.HeldBy != "" || !slices.Equal(obs.Breaches, []netip.Addr{obsFoe}) {
		t.Fatalf("a lone foreign peer must be a breach and never a holder: %+v", obs)
	}
	// A second head blocks and then takes over: with no head allowlisted the
	// peer stays a breach.
	obs = classifyConns([]collector.Conn{connFrom(obsFoe, 5000)}, obsBind, nil, nil, "rspec_h")
	if obs.HeldBy != "" || len(obs.Breaches) != 1 {
		t.Fatalf("empty allowlist: %+v", obs)
	}
}

func TestClassifyConnsOwnAddressIgnored(t *testing.T) {
	obs := classifyConns([]collector.Conn{connFrom(obsOwn, 40000)}, obsBind, []netip.Addr{obsHead}, []netip.Addr{obsOwn}, "rspec_h")
	if obs.HeldBy != "" || len(obs.Breaches) != 0 {
		t.Fatalf("obs = %+v", obs)
	}
}

func TestClassifyConnsOtherPortsAndDedupe(t *testing.T) {
	other := collector.Conn{Local: netip.MustParseAddrPort("100.64.0.5:9999"), Remote: netip.AddrPortFrom(obsFoe, 1)}
	obs := classifyConns([]collector.Conn{other, connFrom(obsFoe, 1), connFrom(obsFoe, 2), connFrom(obsHead, 3)},
		obsBind, []netip.Addr{obsHead}, nil, "rspec_h")
	if obs.HeldBy != "rspec_h" || !slices.Equal(obs.Breaches, []netip.Addr{obsFoe}) {
		t.Fatalf("obs = %+v", obs)
	}
}

type obsHolds struct {
	mu  sync.Mutex
	log []string
}

func (h *obsHolds) SetMemberHold(specID, holder string, since time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, specID+"="+holder)
}

func (h *obsHolds) last() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.log) == 0 {
		return "<none>"
	}
	return h.log[len(h.log)-1]
}

func directMember() runtimectl.Status {
	return runtimectl.Status{
		SpecID: "rpcm_a", Kind: runtimectl.SpecKindRPCServer, State: runtimectl.StateRunning, PID: 4242,
		RPCMember: &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "direct", BindIP: "100.64.0.5", Port: 47001, HeadIPs: []string{"100.64.0.2"}},
	}
}

type obsFixture struct {
	obs   *rpcObserver
	holds *obsHolds
	conns atomic.Pointer[[]collector.Conn]
	now   atomic.Int64
}

func newObsFixture(t *testing.T, rows ...runtimectl.Status) *obsFixture {
	t.Helper()
	f := &obsFixture{holds: &obsHolds{}}
	f.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	f.obs = newRPCObserver(func() []runtimectl.Status { return rows }, f.holds,
		func(int32) ([]collector.Conn, error) {
			if c := f.conns.Load(); c != nil {
				return *c, nil
			}
			return nil, nil
		}, newHeadResolver(nil, nil))
	f.obs.ownAddrs = func() []netip.Addr { return []netip.Addr{obsOwn} }
	f.obs.now = func() time.Time { return time.Unix(0, f.now.Load()) }
	return f
}

func (f *obsFixture) set(c ...collector.Conn) { f.conns.Store(&c) }

func TestObserverHeadConnHolds(t *testing.T) {
	f := newObsFixture(t, directMember())
	f.set(connFrom(obsHead, 5000))
	f.obs.pollAll(context.Background())
	if got := f.holds.last(); got != "rpcm_a=rspec_h" {
		t.Fatalf("hold = %s", got)
	}
}

func TestObserverForeignPeerIsBreachNotHolder(t *testing.T) {
	f := newObsFixture(t, directMember())
	f.set(connFrom(obsFoe, 5000))
	f.obs.pollAll(context.Background())
	if got := f.holds.last(); got != "<none>" {
		t.Fatalf("a foreign peer became a holder: %s", got)
	}
	tel, ok := f.obs.telemetry("rpcm_a")
	if !ok || len(tel.Breaches) != 1 || tel.Breaches[0].Peer != obsFoe.String() {
		t.Fatalf("telemetry = %+v", tel)
	}
	// Deduped per peer; expires after ten minutes without sightings.
	f.obs.pollAll(context.Background())
	tel, _ = f.obs.telemetry("rpcm_a")
	if len(tel.Breaches) != 1 {
		t.Fatalf("not deduped: %+v", tel.Breaches)
	}
	f.set()
	f.now.Add(int64(11 * time.Minute))
	f.obs.pollAll(context.Background())
	tel, _ = f.obs.telemetry("rpcm_a")
	if len(tel.Breaches) != 0 {
		t.Fatalf("breach not expired: %+v", tel.Breaches)
	}
}

func TestObserverBreachCapAndTakeover(t *testing.T) {
	f := newObsFixture(t, directMember())
	var cs []collector.Conn
	for i := 1; i <= 20; i++ {
		cs = append(cs, connFrom(netip.AddrFrom4([4]byte{100, 64, 1, byte(i)}), 1))
	}
	f.set(cs...)
	f.obs.pollAll(context.Background())
	tel, _ := f.obs.telemetry("rpcm_a")
	if len(tel.Breaches) != maxRelayBreaches {
		t.Fatalf("breaches = %d, want %d", len(tel.Breaches), maxRelayBreaches)
	}
	if f.holds.last() != "<none>" {
		t.Fatal("breach peers must never hold")
	}
}

func TestObserverOwnAddressIgnored(t *testing.T) {
	f := newObsFixture(t, directMember())
	f.set(connFrom(obsOwn, 40000))
	f.obs.pollAll(context.Background())
	if tel, ok := f.obs.telemetry("rpcm_a"); ok && len(tel.Breaches) != 0 {
		t.Fatalf("own address breached: %+v", tel)
	}
	if f.holds.last() != "<none>" {
		t.Fatal("own address held")
	}
}

func TestObserverReleaseNeedsTwoEmptyPolls(t *testing.T) {
	f := newObsFixture(t, directMember())
	ctx := context.Background()
	f.set(connFrom(obsHead, 5000))
	f.obs.pollAll(ctx)
	f.set()
	f.obs.pollAll(ctx)
	if got := f.holds.last(); got != "rpcm_a=rspec_h" {
		t.Fatalf("released after one empty poll: %s", got)
	}
	f.obs.pollAll(ctx)
	if got := f.holds.last(); got != "rpcm_a=" {
		t.Fatalf("not released after two empty polls: %s", got)
	}
	// A reappearing connection resets the count.
	f.set(connFrom(obsHead, 5001))
	f.obs.pollAll(ctx)
	f.set()
	f.obs.pollAll(ctx)
	if got := f.holds.last(); got != "rpcm_a=rspec_h" {
		t.Fatalf("empty count not reset: %s", got)
	}
}

func TestObserverSkipsRelayAndNonRunning(t *testing.T) {
	relay := directMember()
	relay.RPCMember = &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "relay", Port: 47001}
	starting := directMember()
	starting.SpecID = "rpcm_b"
	starting.State = runtimectl.StateStarting
	f := newObsFixture(t, relay, starting)
	f.set(connFrom(obsHead, 5000))
	f.obs.pollAll(context.Background())
	if f.holds.last() != "<none>" {
		t.Fatal("observed a member it must not")
	}
}

func TestObserverErrorKeepsLastObservation(t *testing.T) {
	f := newObsFixture(t, directMember())
	ctx := context.Background()
	f.set(connFrom(obsHead, 5000))
	f.obs.pollAll(ctx)
	f.obs.conns = func(int32) ([]collector.Conn, error) { return nil, errors.New("lsof failed") }
	f.obs.pollAll(ctx)
	f.obs.pollAll(ctx)
	f.obs.pollAll(ctx)
	if got := f.holds.last(); got != "rpcm_a=rspec_h" {
		t.Fatalf("errors released the hold: %s", got)
	}
}

func TestObserverUsesResolvedHeadDomain(t *testing.T) {
	m := directMember()
	m.RPCMember = &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "direct", BindIP: "100.64.0.5", Port: 47001, HeadDomain: "head.example"}
	f := newObsFixture(t, m)
	f.obs.heads = newHeadResolver(func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{obsHead}, nil }, nil)
	f.obs.heads.refresh(context.Background(), map[string]string{"rpcm_a": "head.example"})
	f.set(connFrom(obsHead, 5000))
	f.obs.pollAll(context.Background())
	if got := f.holds.last(); got != "rpcm_a=rspec_h" {
		t.Fatalf("hold = %s", got)
	}
}

// TestObserverRunsOffCollectLoop: a connection listing that blocks for three
// seconds neither delays another loop's cadence nor piles up overlapping polls.
func TestObserverRunsOffCollectLoop(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	rows := []runtimectl.Status{directMember()}
	o := newRPCObserver(func() []runtimectl.Status { return rows }, &obsHolds{},
		func(int32) ([]collector.Conn, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-time.After(3 * time.Second):
			}
			return nil, nil
		}, newHeadResolver(nil, nil))
	o.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.run(ctx); close(done) }()

	// The "collect loop": a 50 ms ticker that must keep its cadence.
	start := time.Now()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var ticks int
	var maxGap time.Duration
	last := start
	for time.Since(start) < 500*time.Millisecond {
		<-tick.C
		ticks++
		if g := time.Since(last); g > maxGap {
			maxGap = g
		}
		last = time.Now()
	}
	if ticks < 8 || maxGap > 200*time.Millisecond {
		t.Fatalf("collect cadence disturbed: ticks=%d maxGap=%s", ticks, maxGap)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("overlapping polls piled up behind the blocked one: %d calls", c)
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not stop")
	}
}

// TestMemberObserverFeedsRelayAllowlistAndResyncs: the resolver's cache backs
// the relay's head-domain lookup, and a changed answer re-syncs the relays.
func TestMemberObserverFeedsRelayAllowlistAndResyncs(t *testing.T) {
	var statusCalls atomic.Int32
	link := newRelayLink(func() []runtimectl.Status { statusCalls.Add(1); return nil }, &fakeMembers{})
	t.Cleanup(link.close)
	o := newMemberObserver(func() []runtimectl.Status { return nil }, &obsHolds{}, link)
	o.heads.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{obsHead}, nil }
	o.heads.refresh(context.Background(), map[string]string{"rpcm_a": "head.lan"})
	if got := link.headDomainIPs("rpcm_a"); len(got) != 1 || got[0].String() != obsHead.String() {
		t.Fatalf("relay lookup = %v", got)
	}
	if statusCalls.Load() == 0 {
		t.Fatal("a changed resolution did not re-sync the relays")
	}
}
