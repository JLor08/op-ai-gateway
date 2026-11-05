// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"op-ai-server-agent/internal/collector"
	"slices"
	"sync"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

const (
	// observerInterval is how often each direct-mode member's connections are
	// listed.
	observerInterval = 2 * time.Second
	// observerReleasePolls is how many consecutive polls without a head
	// connection end a hold: one empty poll can be a listing racing a
	// reconnect.
	observerReleasePolls = 2
	// breachKeep is how long a peer stays listed after it was last seen.
	breachKeep = 10 * time.Minute
)

// observation is what one poll saw of a member's connections.
type observation struct {
	// HeldBy is the head spec ID when a connection from an allowed head
	// address exists.
	HeldBy string
	// Breaches are the distinct remote addresses that are neither an allowed
	// head nor this host.
	Breaches []netip.Addr
}

// classifyConns sorts the member's connections on the bind port. A remote in
// headIPs is the head; a remote that is one of this host's own addresses (the
// agent's readiness probe, a local tool) is ignored; any other remote is a
// breach and never a holder, even when it is the only connection: a second
// head that blocks and later takes over must not be mistaken for the head.
func classifyConns(conns []collector.Conn, bind netip.AddrPort, headIPs, ownIPs []netip.Addr, headSpecID string) observation {
	var obs observation
	for _, c := range conns {
		if c.Local.Port() != bind.Port() {
			continue
		}
		if b := bind.Addr(); b.IsValid() && !b.IsUnspecified() && c.Local.Addr() != b {
			continue
		}
		remote := c.Remote.Addr().Unmap()
		switch {
		case slices.Contains(headIPs, remote):
			obs.HeldBy = headSpecID
		case slices.Contains(ownIPs, remote):
		case !slices.Contains(obs.Breaches, remote):
			obs.Breaches = append(obs.Breaches, remote)
		}
	}
	return obs
}

// observerTelemetry is what the observer knows of one member, for telemetry.
type observerTelemetry struct {
	Breaches []runtimectl.RPCBreach
}

// observedMember is the observer's state for one member.
type observedMember struct {
	pid      int
	holding  bool
	since    time.Time
	empty    int
	inflight bool
	errLog   bool
	breaches []runtimectl.RPCBreach // oldest first; At is first seen
	lastSeen map[string]time.Time
}

// rpcHolds is the runtime surface the observer reports holds to.
type rpcHolds interface {
	SetMemberHold(specID, holder string, since time.Time)
}

// rpcObserver watches the connections of direct-mode rpc_server members and
// reports which head holds each. (Relay-mode members are held by their relay.)
// Listing a process's connections can be slow, so every member is polled on
// its own goroutine, never on the telemetry collect loop, and a poll that is
// still running is not overlapped.
type rpcObserver struct {
	status   func() []runtimectl.Status
	holds    rpcHolds
	conns    func(pid int32) ([]collector.Conn, error)
	heads    *headResolver
	ownAddrs func() []netip.Addr
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	members map[string]*observedMember
	wg      sync.WaitGroup
}

func newRPCObserver(status func() []runtimectl.Status, holds rpcHolds, conns func(int32) ([]collector.Conn, error), heads *headResolver) *rpcObserver {
	return &rpcObserver{
		status: status, holds: holds, conns: conns, heads: heads,
		ownAddrs: hostAddrs, now: time.Now, interval: observerInterval,
		members: map[string]*observedMember{},
	}
}

// hostAddrs lists this host's own interface addresses.
func hostAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// observable reports whether st is a running direct-mode member with a process.
func observable(st runtimectl.Status) bool {
	rpc := st.RPCMember
	return st.Instance == 0 && st.Kind == runtimectl.SpecKindRPCServer && rpc != nil &&
		rpc.Transport == "direct" && st.State == runtimectl.StateRunning && st.PID > 0 && rpc.Port > 0
}

// run polls until ctx ends, then waits for the polls in flight.
func (o *rpcObserver) run(ctx context.Context) {
	if o == nil {
		return
	}
	defer o.wg.Wait()
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		o.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick starts a poll goroutine for every observable member that has none in
// flight.
func (o *rpcObserver) tick(ctx context.Context) {
	statuses := o.status()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(statuses)
	for _, st := range statuses {
		if !observable(st) {
			continue
		}
		m := o.memberLocked(st)
		if m.inflight {
			continue
		}
		m.inflight = true
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			o.poll(ctx, st)
		}()
	}
}

// pollAll polls every observable member once, synchronously.
func (o *rpcObserver) pollAll(ctx context.Context) {
	statuses := o.status()
	o.mu.Lock()
	o.pruneLocked(statuses)
	o.mu.Unlock()
	for _, st := range statuses {
		if observable(st) {
			o.poll(ctx, st)
		}
	}
}

func (o *rpcObserver) pruneLocked(statuses []runtimectl.Status) {
	for id := range o.members {
		if !slices.ContainsFunc(statuses, func(s runtimectl.Status) bool { return s.SpecID == id && s.Instance == 0 }) {
			delete(o.members, id)
		}
	}
}

// memberLocked returns the member's state; a new process starts it afresh.
func (o *rpcObserver) memberLocked(st runtimectl.Status) *observedMember {
	m := o.members[st.SpecID]
	if m == nil {
		m = &observedMember{lastSeen: map[string]time.Time{}}
		o.members[st.SpecID] = m
	}
	if m.pid != st.PID {
		m.pid, m.holding, m.empty, m.errLog = st.PID, false, 0, false
	}
	return m
}

// poll lists one member's connections and applies the result.
func (o *rpcObserver) poll(_ context.Context, st runtimectl.Status) {
	rpc := st.RPCMember
	conns, err := o.conns(int32(st.PID))

	o.mu.Lock()
	m := o.memberLocked(st)
	m.inflight = false
	if err != nil {
		// Keep the last observation: a failed listing says nothing about
		// the head.
		first := !m.errLog
		m.errLog = true
		o.mu.Unlock()
		if first {
			slog.Debug("agent: rpc member connection listing failed", "spec", st.SpecID, "err", err)
		}
		return
	}
	m.errLog = false
	o.mu.Unlock()

	bind, _ := netip.ParseAddr(rpc.BindIP)
	obs := classifyConns(conns, netip.AddrPortFrom(bind, uint16(rpc.Port)), o.headIPs(st.SpecID, rpc), o.ownAddrs(), rpc.HeadSpecID)

	now := o.now()
	var holder string
	var since time.Time
	var release bool
	o.mu.Lock()
	o.noteBreachesLocked(m, obs.Breaches, now)
	switch {
	case obs.HeldBy != "":
		if !m.holding {
			m.holding, m.since = true, now
		}
		m.empty = 0
		holder, since = obs.HeldBy, m.since
	case m.holding:
		if m.empty++; m.empty >= observerReleasePolls {
			m.holding, m.empty = false, 0
			release = true
		}
	}
	o.mu.Unlock()

	switch {
	case release:
		o.holds.SetMemberHold(st.SpecID, "", time.Time{})
	case holder != "":
		o.holds.SetMemberHold(st.SpecID, holder, since)
	}
}

// headIPs is the member's allowed head addresses: head_ips plus the resolved
// head domain.
func (o *rpcObserver) headIPs(specID string, rpc *runtimectl.RPCMember) []netip.Addr {
	var out []netip.Addr
	for _, s := range rpc.HeadIPs {
		if ip, err := netip.ParseAddr(s); err == nil {
			out = append(out, ip.Unmap())
		}
	}
	if rpc.HeadDomain != "" && o.heads != nil {
		out = append(out, o.heads.IPs(specID)...)
	}
	return out
}

// noteBreachesLocked records the peers seen now and expires those not seen
// for breachKeep; the list is capped at its most recent maxRelayBreaches.
func (o *rpcObserver) noteBreachesLocked(m *observedMember, seen []netip.Addr, now time.Time) {
	for _, ip := range seen {
		key := ip.String()
		if _, ok := m.lastSeen[key]; !ok {
			m.breaches = append(m.breaches, runtimectl.RPCBreach{Peer: key, At: now})
		}
		m.lastSeen[key] = now
	}
	m.breaches = slices.DeleteFunc(m.breaches, func(b runtimectl.RPCBreach) bool {
		if now.Sub(m.lastSeen[b.Peer]) >= breachKeep {
			delete(m.lastSeen, b.Peer)
			return true
		}
		return false
	})
	if n := len(m.breaches); n > maxRelayBreaches {
		for _, b := range m.breaches[:n-maxRelayBreaches] {
			delete(m.lastSeen, b.Peer)
		}
		m.breaches = slices.Clone(m.breaches[n-maxRelayBreaches:])
	}
}

// telemetry returns a copy of what the observer saw of member specID.
func (o *rpcObserver) telemetry(specID string) (observerTelemetry, bool) {
	if o == nil {
		return observerTelemetry{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	m := o.members[specID]
	if m == nil {
		return observerTelemetry{}, false
	}
	return observerTelemetry{Breaches: slices.Clone(m.breaches)}, true
}

// newMemberObserver wires the observer and head resolver of this host's
// members: the resolver's answers feed the relay's allowlist, and a changed
// answer re-syncs the relays so sessions from no-longer-allowed addresses end.
func newMemberObserver(status func() []runtimectl.Status, holds rpcHolds, relays *relayLink) *rpcObserver {
	heads := newHeadResolver(nil, func(string) { relays.sync() })
	relays.headDomainIPs = func(specID string) []net.IP {
		ips := heads.IPs(specID)
		out := make([]net.IP, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.IP(ip.AsSlice()))
		}
		return out
	}
	o := newRPCObserver(status, holds, collector.EstablishedConns, heads)
	return o
}

// headDomains reports the head domain of every listed member that has one.
func (o *rpcObserver) headDomains() map[string]string {
	out := map[string]string{}
	for _, st := range o.status() {
		if st.Instance == 0 && st.Kind == runtimectl.SpecKindRPCServer && st.RPCMember != nil && st.RPCMember.HeadDomain != "" {
			out[st.SpecID] = st.RPCMember.HeadDomain
		}
	}
	return out
}
