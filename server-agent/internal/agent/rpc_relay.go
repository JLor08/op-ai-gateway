// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"errors"
	"log/slog"
	"net"
	"op-ai-server-agent/internal/rpcrelay"
	"slices"
	"strconv"
	"sync"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// rpcMemberSink is the runtime's member surface the relay drives:
// *runtimectl.Manager satisfies it. SetMemberHold carries what the relay sees
// of the head's connection; SetMemberRelayGate holds a relay member's
// readiness until its relay listens.
type rpcMemberSink interface {
	SetMemberHold(specID, holder string, since time.Time)
	SetMemberRelayGate(gate func(specID string) error)
}

// maxRelayBreaches is how many refused peers a member's telemetry keeps (the
// most recent ones).
const maxRelayBreaches = 16

// relayTelemetry is what the relay observed of one member, for its telemetry.
type relayTelemetry struct {
	// BindMismatch: the member's relay bind address is not an address of
	// this host. It stays set after the member's start gave up on it, for as
	// long as the member is listed, so the failure keeps its explanation.
	BindMismatch bool
	// Breaches are the peers refused for being off the allowlist, kept like
	// the observer's (breachLog).
	Breaches []runtimectl.RPCBreach
	// Stalled: the head's last bytes went unanswered for the stall threshold.
	// Cleared when the session ends or a new one takes the hold.
	Stalled bool
}

// relayLink runs the relays of this host's relay-mode rpc_server members. The
// relay set is derived from the runtime status, which carries each member's
// settings as last applied: a relay for every relay-mode member whose child
// process exists (starting or running), forwarding to the child's loopback
// port. It is re-derived on every runtime-config sync, on every state
// transition, and from the readiness gate.
//
// Readiness: the runtime asks the gate (ready) before it declares a starting
// relay member running, so the member's /rpc/ensure answers only once the head
// can actually connect. The gate derives the relay set itself rather than
// waiting for the next transition, which keeps the start free of any agent
// loop latency; while the relay cannot bind, the member stays starting.
type relayLink struct {
	status func() []runtimectl.Status
	holds  rpcMemberSink
	relay  *rpcrelay.Manager
	// headDomainIPs resolves a member's head_domain to the head's addresses,
	// by the member's spec ID; nil allows head_ips only. It is asked for every
	// connection to a member with a head domain, so it must answer from a
	// cache.
	headDomainIPs func(specID string) []net.IP

	// now is the breach log's clock.
	now func() time.Time

	syncMu  sync.Mutex
	lastErr string // the last Apply error logged, so a repeated one is not

	telMu    sync.Mutex
	tel      map[string]*relayTelemetry
	breaches map[string]*breachLog
}

func newRelayLink(status func() []runtimectl.Status, holds rpcMemberSink) *relayLink {
	l := &relayLink{status: status, holds: holds, now: time.Now, tel: map[string]*relayTelemetry{}, breaches: map[string]*breachLog{}}
	l.relay = rpcrelay.NewManager(l, l.running)
	return l
}

// sync brings the relays in line with the runtime status.
func (l *relayLink) sync() {
	if l == nil {
		return
	}
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	statuses := l.status()
	var cfgs []rpcrelay.Config
	for _, st := range statuses {
		if cfg, ok := l.relayConfig(st); ok {
			cfgs = append(cfgs, cfg)
		}
	}
	err := l.relay.Apply(cfgs)
	switch {
	case errors.Is(err, rpcrelay.ErrClosed):
		return
	case err != nil && err.Error() != l.lastErr:
		slog.Warn("agent: rpc relay config refused", "err", err)
	}
	l.lastErr = ""
	if err != nil {
		l.lastErr = err.Error()
	}
	l.noteListeners(statuses, cfgs)
}

// relayConfig is the relay of the member st describes, if it should have one.
func (l *relayLink) relayConfig(st runtimectl.Status) (rpcrelay.Config, bool) {
	rpc := st.RPCMember
	if st.Instance != 0 || st.Kind != runtimectl.SpecKindRPCServer || rpc == nil || rpc.Transport != "relay" {
		return rpcrelay.Config{}, false
	}
	if (st.State != runtimectl.StateStarting && st.State != runtimectl.StateRunning) || st.Port <= 0 {
		return rpcrelay.Config{}, false
	}
	return rpcrelay.Config{
		SpecID:     st.SpecID,
		Bind:       net.JoinHostPort(rpc.BindIP, strconv.Itoa(rpc.Port)),
		Target:     net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port)),
		AllowedIPs: l.allowlist(st.SpecID, *rpc),
		HeadSpecID: rpc.HeadSpecID,
	}, true
}

// allowlist is the member's allowed head addresses: its head_ips and, when it
// names a head domain, the IPv4 addresses that resolves to. Entries that are
// not IPv4 addresses are dropped.
func (l *relayLink) allowlist(specID string, rpc runtimectl.RPCMember) func() []net.IP {
	var static []net.IP
	for _, s := range rpc.HeadIPs {
		if ip := net.ParseIP(s).To4(); ip != nil {
			static = append(static, ip)
		}
	}
	resolve := l.headDomainIPs
	if rpc.HeadDomain == "" || resolve == nil {
		return func() []net.IP { return static }
	}
	return func() []net.IP {
		out := slices.Clone(static)
		for _, ip := range resolve(specID) {
			if v4 := ip.To4(); v4 != nil {
				out = append(out, v4)
			}
		}
		return out
	}
}

// noteListeners records each relay's bind mismatch, and drops the telemetry of
// members that are no longer listed.
func (l *relayLink) noteListeners(statuses []runtimectl.Status, cfgs []rpcrelay.Config) {
	l.telMu.Lock()
	defer l.telMu.Unlock()
	for _, cfg := range cfgs {
		st, _ := l.relay.Status(cfg.SpecID)
		l.telFor(cfg.SpecID).BindMismatch = st.BindMismatch
	}
	for id := range l.tel {
		if !slices.ContainsFunc(statuses, func(s runtimectl.Status) bool { return s.SpecID == id }) {
			delete(l.tel, id)
			delete(l.breaches, id)
		}
	}
}

// telFor returns the member's telemetry record, creating it; telMu is held.
func (l *relayLink) telFor(specID string) *relayTelemetry {
	t := l.tel[specID]
	if t == nil {
		t = &relayTelemetry{}
		l.tel[specID] = t
	}
	return t
}

// telemetry returns a copy of what the relay observed of member specID.
func (l *relayLink) telemetry(specID string) (relayTelemetry, bool) {
	l.telMu.Lock()
	defer l.telMu.Unlock()
	t := l.tel[specID]
	if t == nil {
		return relayTelemetry{}, false
	}
	out := *t
	if b := l.breaches[specID]; b != nil {
		out.Breaches = b.snapshot(l.now())
	}
	return out, true
}

// ready is the runtime's relay gate: it brings the relays up to date and
// returns nil when member specID's relay listens, else why it does not (the
// relay's last listen error when there is one). The runtime reports that text
// if the member's start gives up on it.
func (l *relayLink) ready(specID string) error {
	l.sync()
	st, ok := l.relay.Status(specID)
	switch {
	case !ok:
		return errors.New("no relay is configured for the member")
	case st.Listening:
		return nil
	case st.Err != "":
		return errors.New(st.Err)
	default:
		return errors.New("the relay is not listening yet")
	}
}

// running reports whether member specID is running now, read live: a head
// that connects right after its member's ensure answered must find it so.
func (l *relayLink) running(specID string) bool {
	for _, st := range l.status() {
		if st.SpecID == specID && st.Instance == 0 {
			return st.State == runtimectl.StateRunning
		}
	}
	return false
}

func (l *relayLink) close() {
	if l != nil {
		l.relay.Close()
	}
}

// Held implements rpcrelay.Events.
func (l *relayLink) Held(specID, headSpecID string, since time.Time) {
	l.holds.SetMemberHold(specID, headSpecID, since)
	l.telMu.Lock()
	l.telFor(specID).Stalled = false
	l.telMu.Unlock()
}

// Released implements rpcrelay.Events.
func (l *relayLink) Released(specID string) {
	l.holds.SetMemberHold(specID, "", time.Time{})
	l.telMu.Lock()
	l.telFor(specID).Stalled = false
	l.telMu.Unlock()
}

// Refused implements rpcrelay.Events.
func (l *relayLink) Refused(specID string, peer net.IP) {
	l.telMu.Lock()
	defer l.telMu.Unlock()
	l.telFor(specID)
	b := l.breaches[specID]
	if b == nil {
		b = &breachLog{}
		l.breaches[specID] = b
	}
	b.note(l.now(), peer.String())
}

// Stalled implements rpcrelay.Events.
func (l *relayLink) Stalled(specID string) {
	l.telMu.Lock()
	l.telFor(specID).Stalled = true
	l.telMu.Unlock()
}
