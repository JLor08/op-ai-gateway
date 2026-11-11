// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/portal"
	"slices"
	"testing"
	"time"
)

// The watchdog's tests drive the real orchestrator over the scripted portal
// and agent fleet of the orchestration tests, feeding telemetry frames as the
// ingest does (frame) and waiting for the watchdog's work (settle).

// frame publishes rows as serverID's next telemetry frame and hands it to the
// orchestration, as the ingest does.
func (f *orchFixture) frame(serverID string, rows ...RuntimeStatusDTO) {
	f.t.Helper()
	if rows == nil {
		rows = []RuntimeStatusDTO{}
	}
	prev := f.statuses.statusRows(serverID)
	f.statuses.publish(serverID, rows)
	f.o.observeStatus(serverID, prev, rows)
}

// settle waits until the watchdog has handled every frame and every action
// it began has ended.
func (f *orchFixture) settle() { f.o.wait() }

// confirm feeds a frame of the head's server that shows the head still
// running with its process: what confirms a member loss a member frame
// showed.
func (f *orchFixture) confirm() {
	f.t.Helper()
	f.settle()
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.settle()
}

func watchHeadRow(state string) RuntimeStatusDTO {
	return RuntimeStatusDTO{
		SpecID: orchHead, Model: orchHeadModel, State: state, PID: 100,
		Cluster: &RuntimeClusterStatusDTO{Launched: []string{"m0:47001"}},
	}
}

func watchMemberRow(i int, heldBy string, pid int) RuntimeStatusDTO {
	r := orchMemberRow(orchMemberSpec(i), heldBy)
	r.PID = pid
	return r
}

// formed forms the fixture's cluster and feeds the telemetry of a running
// cluster: the head running with its launch facts, each member held by it.
// It returns how many agent calls the formation made.
func (f *orchFixture) formed() int {
	f.t.Helper()
	f.ackAll()
	n := len(f.portal.plan.Members)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	f.holdOnHeadEnsure(idx...)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		f.t.Fatalf("Ensure: %v", err)
	}
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	for i := range n {
		f.frame(orchMemberServer(i), watchMemberRow(i, orchHead, 10+i))
	}
	f.settle()
	if st := f.o.State(orchHead); st != clusterFormed {
		f.t.Fatalf("state = %v, want formed", st)
	}
	return len(f.agent.recorded())
}

// releases returns the member releases among calls, sorted.
func releases(calls []string) []string {
	var out []string
	for _, c := range calls {
		if len(c) > len("member:release:") && c[:len("member:release:")] == "member:release:" {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

func allReleases(members int) []string {
	out := make([]string, members)
	for i := range members {
		out[i] = "member:release:" + orchMemberSpec(i)
	}
	slices.Sort(out)
	return out
}

// expectWatchdogStop asserts that the calls after before stopped the head and
// then released every member, and that the head now cools down with the lost
// member named.
func (f *orchFixture) expectWatchdogStop(before, position int) {
	f.t.Helper()
	calls := f.agent.recorded()[before:]
	if len(calls) == 0 || calls[0] != "head:stop:"+orchHeadModel {
		f.t.Fatalf("calls = %q, want the head stopped first", calls)
	}
	if got, want := releases(calls[1:]), allReleases(len(f.portal.plan.Members)); !slices.Equal(got, want) {
		f.t.Fatalf("calls = %q, want every member released after the head", calls)
	}
	if st := f.o.State(orchHead); st != clusterCoolingDown {
		f.t.Fatalf("state = %v, want cooling down", st)
	}
	status := f.o.ClusterStatus(orchHead)
	if status.LastFailure == nil || status.LastFailure.Code != codeClusterMemberLost ||
		status.LastFailure.Position == nil || *status.LastFailure.Position != position {
		f.t.Fatalf("last failure = %+v, want %s naming member %d", status.LastFailure, codeClusterMemberLost, position)
	}
	if status.Members[position].State != clusterMemberFailed || status.Members[position].Code != codeClusterMemberLost {
		f.t.Fatalf("members = %+v, want member %d failed", status.Members, position)
	}
	ce := clusterErrOf(f.t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterStartFailed || ce.Cause != codeClusterMemberLost || ce.Position != position {
		f.t.Fatalf("Ensure in the cooldown = %+v, want the lost member's failure", ce)
	}
}

// TestWatchdogStopsHeadOnMemberExit pins the first positive evidence: a
// member whose process exits while its formed head runs aborts the head, so
// the watchdog stops the head, releases the members after it, and records the
// member as the cause of a cooldown.
func TestWatchdogStopsHeadOnMemberExit(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	f.frame(orchMemberServer(1), RuntimeStatusDTO{SpecID: orchMemberSpec(1), State: "crashed", Kind: "rpc_server"})
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want nothing before a head frame confirms the loss", calls)
	}
	f.confirm()
	f.expectWatchdogStop(before, 1)
}

// TestWatchdogStopsHeadOnPIDChange pins a member that restarted between two
// frames: the head lost its connection even though the member runs again.
func TestWatchdogStopsHeadOnPIDChange(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	f.frame(orchMemberServer(0), watchMemberRow(0, orchHead, 999))
	f.confirm()
	f.expectWatchdogStop(before, 0)
}

// TestWatchdogStopsHeadOnRelayStall pins the relay's stall signal.
func TestWatchdogStopsHeadOnRelayStall(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	stalled := watchMemberRow(1, orchHead, 11)
	stalled.RPC.Stalled = true
	f.frame(orchMemberServer(1), stalled)
	f.confirm()
	f.expectWatchdogStop(before, 1)
}

// TestWatchdogStopsHeadOnSessionClosed pins the closed session: a member
// that no longer reports its head's hold while the head runs has lost it.
// While the head drains, the same frame is no evidence.
func TestWatchdogStopsHeadOnSessionClosed(t *testing.T) {
	g := newOrchFixture(t, 1, "relay", "lan")
	beforeG := g.formed()
	g.frame(orchHeadServer, watchHeadRow(agentSpecStateDraining))
	g.frame(orchMemberServer(0), watchMemberRow(0, "", 10))
	g.settle()
	if n := g.agent.count("head:stop:"); n != 0 {
		t.Fatalf("calls = %q, want no stop while the head drains", g.agent.recorded()[beforeG:])
	}

	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()
	f.frame(orchMemberServer(1), watchMemberRow(1, "", 11))
	f.confirm()
	f.expectWatchdogStop(before, 1)
}

// TestWatchdogDropsLossWhenHeadStoppedItself pins the suspicion: a head that
// stops by itself (idle, eviction, crash) ends its members' sessions, and a
// member's frame may arrive before the head's. A later head frame that shows
// the head stopped, or another process, drops the suspicion: no failure, no
// cooldown, no stop.
func TestWatchdogDropsLossWhenHeadStoppedItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		head RuntimeStatusDTO
	}{
		{"stopped", RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped}},
		{"another process", func() RuntimeStatusDTO { r := watchHeadRow(agentSpecStateRunning); r.PID = 101; return r }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrchFixture(t, 2, "relay", "lan")
			before := f.formed()
			f.frame(orchMemberServer(1), watchMemberRow(1, "", 11))
			f.settle()
			f.frame(orchHeadServer, tc.head)
			f.settle()
			if n := f.agent.count("head:stop:"); n != 0 {
				t.Fatalf("calls = %q, want no stop of a head that stopped by itself", f.agent.recorded()[before:])
			}
			status := f.o.ClusterStatus(orchHead)
			if status.LastFailure != nil || status.CooldownUntil != nil {
				t.Fatalf("status = %+v, want no failure and no cooldown", status)
			}
			// A later head frame confirms nothing: the suspicion is gone.
			f.confirm()
			if n := f.agent.count("head:stop:"); n != 0 {
				t.Fatalf("calls = %q, want the dropped suspicion to stay dropped", f.agent.recorded()[before:])
			}
		})
	}
}

// TestWatchdogNeedsAHeldBaseline pins the baseline of the exit and PID
// evidence: a member that the frame before did not show held by the head
// (a frame from before the cluster formed) gives none.
func TestWatchdogNeedsAHeldBaseline(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	before := f.formed()
	f.statuses.publish(orchMemberServer(0), []RuntimeStatusDTO{watchMemberRow(0, "", 10)})
	f.frame(orchMemberServer(0), watchMemberRow(0, orchHead, 77))
	f.confirm()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want no loss without a held baseline", calls)
	}
}

// TestWatchdogIgnoresStaleInGrace pins stale telemetry: a member server that
// stopped reporting counts only past three agent intervals, never inside the
// gateway's boot grace, and only while the head's own fresh frame shows it
// running.
func TestWatchdogIgnoresStaleInGrace(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	before := f.formed()

	f.advance(30 * time.Second) // stale, but inside the 60 s boot grace
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls inside the boot grace = %q, want none", calls)
	}

	f.advance(40 * time.Second) // past the grace; the head drains
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateDraining))
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls while the head drains = %q, want none", calls)
	}
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want formed", st)
	}

	// The member reports every second again, so its interval reads 1 s; then
	// it is silent for 10 s: more than three intervals, less than the 15 s
	// floor.
	for range watchGapSamples + 1 {
		f.advance(time.Second)
		f.frame(orchMemberServer(0), watchMemberRow(0, orchHead, 10))
	}
	f.advance(10 * time.Second)
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls after 10 s of silence = %q, want none below the floor", calls)
	}

	f.advance(10 * time.Second)
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.settle()
	f.expectWatchdogStop(before, 0)
}

// TestWatchdogRestartsAHeadThatWasInUse pins the re-orchestration: a head
// that served a request when the watchdog stopped it is started again, members
// first, once the cooldown ends -- although it is not kept up.
func TestWatchdogRestartsAHeadThatWasInUse(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.formed()
	f.frame(orchMemberServer(0), RuntimeStatusDTO{SpecID: orchMemberSpec(0), State: "crashed", Kind: "rpc_server"})
	f.settle()
	busy := watchHeadRow(agentSpecStateRunning)
	busy.InFlight = 1
	f.frame(orchHeadServer, busy)
	f.settle()
	if st := f.o.State(orchHead); st != clusterCoolingDown {
		t.Fatalf("state = %v, want cooling down", st)
	}

	before := len(f.agent.recorded())
	f.o.Reconcile(context.Background())
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls inside the cooldown = %q, want none", calls)
	}
	f.advance(61 * time.Second)
	f.o.Reconcile(context.Background())
	f.settle()
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v (calls %q), want formed again", st, f.agent.recorded()[before:])
	}
	if n := f.agent.count("head:ensure:"); n != 2 {
		t.Fatalf("head ensures = %d, want the restart", n)
	}

	// Formed again and no longer in use: a second loss is not restarted.
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.frame(orchMemberServer(0), RuntimeStatusDTO{SpecID: orchMemberSpec(0), State: "crashed", Kind: "rpc_server"})
	f.confirm()
	f.advance(61 * time.Second)
	f.o.Reconcile(context.Background())
	f.settle()
	if n := f.agent.count("head:ensure:"); n != 2 {
		t.Fatalf("head ensures = %d, want no restart of a head nobody used", n)
	}
}

// blockedFrame is a member server's frame in which a local spec waits for
// room that only the held member i could give.
func blockedFrame(i int) []RuntimeStatusDTO {
	return []RuntimeStatusDTO{
		watchMemberRow(i, orchHead, 10+i),
		{SpecID: "rspec_local", Model: "local-model", State: agentSpecStateStarting, AdmissionBlockedBy: orchMemberSpec(i)},
	}
}

// TestBlockedAdmissionTriggersConditionalHeadStop pins the removal for a
// local start: a local start on a member's server that waits only for the held member makes the
// gateway ask the head's agent for a conditional stop, and once the head is
// gone the members are released -- the head first, then its members. Nothing
// failed, so nothing cools down.
func TestBlockedAdmissionTriggersConditionalHeadStop(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	f.frame(orchMemberServer(1), blockedFrame(1)...)
	f.settle()
	calls := f.agent.recorded()[before:]
	if len(calls) == 0 || calls[0] != "head:stop:"+orchHeadModel+":if_evictable" {
		t.Fatalf("calls = %q, want the head's conditional stop first", calls)
	}
	if got := releases(calls[1:]); !slices.Equal(got, allReleases(2)) {
		t.Fatalf("calls = %q, want the members released after the head", calls)
	}
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold", st)
	}
	if status := f.o.ClusterStatus(orchHead); status.LastFailure != nil || status.CooldownUntil != nil {
		t.Fatalf("status = %+v, want no failure and no cooldown", status)
	}
}

// TestBlockedAdmissionWaitsWhenHeadBusy pins the waiting half: a head its
// agent may not let go answers runtime.instance_busy, and then nothing more
// happens -- the members keep their holds, the local start waits, and the
// next frames do not ask again at once.
func TestBlockedAdmissionWaitsWhenHeadBusy(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	before := f.formed()
	f.agent.headStopErr = orchRefusal(http.StatusConflict, "runtime.instance_busy")

	f.frame(orchMemberServer(0), blockedFrame(0)...)
	f.settle()
	f.frame(orchMemberServer(0), blockedFrame(0)...)
	f.settle()
	calls := f.agent.recorded()[before:]
	if !slices.Equal(calls, []string{"head:stop:" + orchHeadModel + ":if_evictable"}) {
		t.Fatalf("calls = %q, want one conditional stop and nothing else", calls)
	}
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want still formed", st)
	}

	// A member reserved by a start that runs is the start's: no stop.
	g := newOrchFixture(t, 1, "relay", "lan")
	g.ackAll()
	gate := make(chan struct{})
	g.agent.memberGate[orchMemberSpec(0)] = gate
	g.portal.plan.Members[0].StartupTimeoutSeconds = 500
	done := make(chan error, 1)
	go func() { done <- g.o.Ensure(context.Background(), orchHead) }()
	<-g.agent.arrived
	reserved := []RuntimeStatusDTO{
		{SpecID: orchMemberSpec(0), State: agentSpecStateRunning, Kind: "rpc_server", RPC: &RuntimeRPCStatusDTO{}},
		{SpecID: "rspec_local", State: agentSpecStateStarting, AdmissionBlockedBy: orchMemberSpec(0)},
	}
	prev := g.statuses.statusRows(orchMemberServer(0))
	g.statuses.publish(orchMemberServer(0), reserved)
	g.o.observeStatus(orchMemberServer(0), prev, reserved)
	time.Sleep(20 * time.Millisecond)
	if n := g.agent.count("head:stop:"); n != 0 {
		t.Fatalf("calls = %q, want no stop of a head that is forming", g.agent.recorded())
	}
	g.holdOnHeadEnsure(0)
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Ensure: %v", err)
	}
}

// TestHeadStopReleasesDirectMembers pins the hold release: a head that is no
// longer running holds nothing, so its members are released when its exit is
// seen; a direct member that still reports the hold later on holds a dead
// connection, which only a forced stop ends.
func TestHeadStopReleasesDirectMembers(t *testing.T) {
	f := newOrchFixture(t, 2, "direct", "lan")
	before := f.formed()

	f.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	f.settle()
	calls := f.agent.recorded()[before:]
	if got := releases(calls); !slices.Equal(got, allReleases(2)) || len(calls) != 2 {
		t.Fatalf("calls = %q, want both members released and nothing else", calls)
	}
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold", st)
	}
	select {
	case <-f.o.TriggerChannel():
	default:
		t.Fatal("the head's exit did not wake the reconciler")
	}

	f.advance(31 * time.Second)
	before = len(f.agent.recorded())
	f.frame(orchMemberServer(1), watchMemberRow(1, orchHead, 11))
	f.settle()
	if calls := f.agent.recorded()[before:]; !slices.Equal(calls, []string{"member:stop:" + orchMemberSpec(1) + ":force"}) {
		t.Fatalf("calls = %q, want member 1's dead hold ended by a forced stop", calls)
	}
}

// TestIngestForeignHeldByIsBreach pins the breaches: a member held by another
// head, or reporting a peer it does not allow, is logged and listed in the
// head's cluster status by position; a foreign hold is never this head's, so
// on a formed cluster it is the lost session it means.
func TestIngestForeignHeldByIsBreach(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()
	at := time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC)

	peer := watchMemberRow(0, orchHead, 10)
	peer.RPC.Breaches = []RuntimeBreachStatus{{Peer: "100.64.0.9", At: at}}
	f.frame(orchMemberServer(0), peer)
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want a peer breach to stop nothing", calls)
	}
	status := f.o.ClusterStatus(orchHead)
	if status.State != portal.ClusterStateFormed || len(status.Breaches) != 1 ||
		status.Breaches[0] != (portal.ClusterBreachDTO{Position: 0, Kind: portal.ClusterBreachPeer, Peer: "100.64.0.9", At: at}) {
		t.Fatalf("status = %+v, want formed with member 0's peer breach", status)
	}

	f.frame(orchMemberServer(1), watchMemberRow(1, "rspec_other", 11))
	f.confirm()
	status = f.o.ClusterStatus(orchHead)
	if len(status.Breaches) != 2 || status.Breaches[1].Position != 1 || status.Breaches[1].Kind != portal.ClusterBreachForeignHold ||
		status.Breaches[1].Peer != "" {
		t.Fatalf("breaches = %+v, want member 1's foreign hold beside member 0's peer", status.Breaches)
	}
	f.expectWatchdogStop(before, 1)

	// A breach that ends leaves the list.
	f.frame(orchMemberServer(1), watchMemberRow(1, "", 11))
	f.settle()
	if status := f.o.ClusterStatus(orchHead); len(status.Breaches) != 1 || status.Breaches[0].Position != 0 {
		t.Fatalf("breaches = %+v, want only member 0's once member 1 reports none", status.Breaches)
	}
	// A position the plan no longer has leaves the list as well.
	f.portal.mu.Lock()
	f.portal.plan.Members = f.portal.plan.Members[1:]
	f.portal.plan.Members[0].Position = 0
	f.portal.mu.Unlock()
	if _, err := f.o.plan(context.Background(), orchHead); err != nil {
		t.Fatalf("plan: %v", err)
	}
	f.frame(orchMemberServer(1), watchMemberRow(1, "", 11))
	f.settle()
	if status := f.o.ClusterStatus(orchHead); len(status.Breaches) != 0 {
		t.Fatalf("breaches = %+v, want none: member 0's server left the plan", status.Breaches)
	}

	// Telemetry that shows the head running and its members held by another
	// head is no formed cluster: a start runs.
	g := newOrchFixture(t, 1, "relay", "lan")
	g.ackAll()
	g.statuses.publish(orchHeadServer, []RuntimeStatusDTO{watchHeadRow(agentSpecStateRunning)})
	g.statuses.publish(orchMemberServer(0), []RuntimeStatusDTO{watchMemberRow(0, "rspec_other", 10)})
	g.holdOnHeadEnsure(0)
	if err := g.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if n := g.agent.count("member:ensure:"); n != 1 {
		t.Fatalf("calls = %q, want a start, not a cluster taken as formed", g.agent.recorded())
	}
}

// TestMeshDriftStopsDirectClusters pins the drift: once NetBird's Default
// policy is seen enabled again the derivation withholds every direct mesh
// cluster, and the gateway stops each one head first, then releases and
// stops its members. A cluster the drift does not withhold is left alone.
func TestMeshDriftStopsDirectClusters(t *testing.T) {
	f := newOrchFixture(t, 2, "direct", "mesh")
	before := f.formed()
	f.portal.mu.Lock()
	f.portal.withheld = portal.ClusterWithheldDirectUnavailable
	f.portal.mu.Unlock()

	f.o.StopDirectMeshClusters(context.Background())
	calls := f.agent.recorded()[before:]
	want := []string{
		"head:stop:" + orchHeadModel,
		"member:release:" + orchMemberSpec(0), "member:stop:" + orchMemberSpec(0),
		"member:release:" + orchMemberSpec(1), "member:stop:" + orchMemberSpec(1),
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}

	g := newOrchFixture(t, 1, "relay", "mesh")
	beforeG := g.formed()
	g.o.StopDirectMeshClusters(context.Background())
	if calls := g.agent.recorded()[beforeG:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want a relay cluster left alone", calls)
	}
}

// TestStopReleasesMembersWithoutStoppingThem pins the release on a stop: a
// stop that leaves the members running still ends their reservations and
// holds once the head is gone, so they idle out on their short timeout
// rather than after their whole reservation.
func TestStopReleasesMembersWithoutStoppingThem(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	if err := f.o.Stop(context.Background(), orchHead, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	calls := f.agent.recorded()[before:]
	if len(calls) != 3 || calls[0] != "head:stop:"+orchHeadModel || !slices.Equal(releases(calls[1:]), allReleases(2)) {
		t.Fatalf("calls = %q, want the head stopped, then both members released", calls)
	}
}

// TestFailedStartReleasesItsMembers pins the release after a failed start:
// the reservations served that start only, so they end once its head is not
// running -- after a member failed, and after a formation that never came
// (the head stopped first).
func TestFailedStartReleasesItsMembers(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.agent.memberErr[orchMemberSpec(1)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	f.settle()
	if got := releases(f.agent.recorded()); !slices.Equal(got, allReleases(2)) {
		t.Fatalf("calls = %q, want both members released", f.agent.recorded())
	}

	g := newOrchFixture(t, 2, "relay", "lan")
	g.ackAll()
	g.holdOnHeadEnsure(0) // member 1 is never attached
	clusterErrOf(t, g.o.Ensure(context.Background(), orchHead))
	g.settle()
	calls := g.agent.recorded()
	stop := slices.Index(calls, "head:stop:"+orchHeadModel)
	if stop < 0 || !slices.Equal(releases(calls[stop:]), allReleases(2)) {
		t.Fatalf("calls = %q, want the head stopped, then both members released", calls)
	}

	// A start whose configuration was never acknowledged started no member.
	h := newOrchFixture(t, 1, "relay", "lan")
	h.statuses.SetAppliedConfigETag(orchHeadServer, "an-older-document")
	clusterErrOf(t, h.o.Ensure(context.Background(), orchHead))
	h.settle()
	if calls := h.agent.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %q, want none", calls)
	}
}

// TestFormedClusterKeepsItsHolds pins that formation releases nothing: the
// agent's release ends the hold along with the reservation, and a relay
// reports a hold once per session, so a release of a formed member would
// leave the member unclaimed while its head uses it.
func TestFormedClusterKeepsItsHolds(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.formed()
	if n := f.agent.count("member:release:"); n != 0 {
		t.Fatalf("calls = %q, want no release of a formed cluster's members", f.agent.recorded())
	}
}

// TestWatchdogEnqueueDoesNotBlock pins that the ingest only hands a frame
// over: observeStatus returns while the watchdog is busy with an earlier
// action, and later frames of one server coalesce.
func TestWatchdogEnqueueDoesNotBlock(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	before := f.formed()
	gate := make(chan struct{})
	f.agent.mu.Lock()
	f.agent.onHeadStop = func() { <-gate }
	f.agent.mu.Unlock()

	f.frame(orchMemberServer(0), RuntimeStatusDTO{SpecID: orchMemberSpec(0), State: "crashed", Kind: "rpc_server"})
	f.settle()
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	for range 50 {
		returned := make(chan struct{})
		go func() {
			f.frame(orchMemberServer(0), RuntimeStatusDTO{SpecID: orchMemberSpec(0), State: "crashed", Kind: "rpc_server"})
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("observeStatus blocked behind the watchdog's action")
		}
	}
	close(gate)
	f.settle()
	if n := f.agent.count("head:stop:"); n != 1 {
		t.Fatalf("calls = %q, want one stop", f.agent.recorded()[before:])
	}
}

// TestBenchmarkDrainOnMemberServerRefused pins removal through the head for
// the benchmarks' drains: a member is removed only through its head, and a drain force-stops only the
// server's own launch specs, so a VRAM run on a server where a cluster holds
// (or has reserved) a member is refused before anything is written, naming
// the reason; an unclaimed member, which idles out by itself, refuses
// nothing. The speed benchmark's stop-all stops nothing on such a server.
func TestBenchmarkDrainOnMemberServerRefused(t *testing.T) {
	member := func(heldBy string, reservedUntil time.Time) RuntimeStatusDTO {
		return RuntimeStatusDTO{
			SpecID: "rpca_rspec_head_srv1", State: agentSpecStateRunning, PID: 7, Kind: "rpc_server",
			RPC: &RuntimeRPCStatusDTO{HeldBy: heldBy, ReservedUntil: reservedUntil},
		}
	}
	for _, tc := range []struct {
		name string
		row  RuntimeStatusDTO
	}{
		{"held", member("rspec_head", time.Time{})},
		{"reserved", member("", time.Now().Add(time.Minute))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVRAMFixture(t, vramFixtureOpts{})
			f.seedLatestSample()
			f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{tc.row})
			_, err := f.srv.vramRunPlan(context.Background(), f.target)
			if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMClusterMemberHeld {
				t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMClusterMemberHeld)
			}
			for specID, state := range f.allAdminStates(t) {
				if state != "" {
					t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
				}
			}
		})
	}
	t.Run("unclaimed", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{member("", time.Time{})})
		if _, err := f.srv.vramRunPlan(context.Background(), f.target); err != nil {
			t.Fatalf("vramRunPlan with an unclaimed member = %v, want no refusal", err)
		}
	})
	if codeBenchmarkVRAMClusterMemberHeld != "benchmark.vram_cluster_member_held" {
		t.Fatalf("stable error code drift: %q", codeBenchmarkVRAMClusterMemberHeld)
	}

	rows := []RuntimeStatusDTO{
		{SpecID: "S", State: agentSpecStateRunning, PID: 3},
		member("rspec_head", time.Time{}),
	}
	if sel, reason := benchmarkStopSelectionOf(rows, "S", nil); reason != benchmarkStopReasonClusterMember || len(sel.stopSet) != 0 {
		t.Fatalf("stop selection = (%+v, %q), want nothing stopped for a cluster member", sel, reason)
	}
}

// TestStartWaitsForAPendingRelease pins the order of a release and a start:
// a start asked for while a failed start's members are being released is not
// refused, and it ensures the members only after the release ended, so the
// release cannot end the reservation the new start arms.
func TestStartWaitsForAPendingRelease(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	gate := make(chan struct{})
	f.agent.mu.Lock()
	f.agent.releaseGate = gate
	f.agent.mu.Unlock()
	clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	for call := range f.agent.arrived {
		if call == "member:release:"+orchMemberSpec(0) {
			break
		}
	}

	delete(f.agent.memberErr, orchMemberSpec(0))
	f.holdOnHeadEnsure(0)
	done := make(chan error, 1)
	go func() { done <- f.o.StartManual(context.Background(), orchHead) }()
	time.Sleep(30 * time.Millisecond)
	if n := f.agent.count("member:ensure:"); n != 1 {
		t.Fatalf("calls = %q, want no member ensured while the release runs", f.agent.recorded())
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("StartManual during a release = %v, want the start to wait and form", err)
	}
	calls := f.agent.recorded()
	if slices.Index(calls, "member:release:"+orchMemberSpec(0)) > slices.Index(calls[1:], "member:ensure:"+orchMemberSpec(0))+1 {
		t.Fatalf("calls = %q, want the release before the second ensure", calls)
	}
}

// TestWatchdogCodesAreStableWireStrings pins the codes the watchdog writes
// and reads: the portal maps the first, the agent answers the second.
func TestWatchdogCodesAreStableWireStrings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{codeClusterMemberLost, "runtime.cluster_member_lost"},
		{codeRuntimeInstanceBusy, "runtime.instance_busy"},
		{codeClusterHeadCrashed, "runtime.cluster_head_crashed"},
	} {
		if tc.got != tc.want {
			t.Errorf("stable error code drift: got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestWatchdogRetriesAFailedStop pins the one retry of the watchdog's stop.
func TestWatchdogRetriesAFailedStop(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	before := f.formed()
	f.agent.headStopErr = orchRefusal(http.StatusBadGateway, "runtime.upstream_gone")
	f.frame(orchMemberServer(0), RuntimeStatusDTO{SpecID: orchMemberSpec(0), State: "crashed", Kind: "rpc_server"})
	f.confirm()
	if n := f.agent.count("head:stop:"); n != 2 {
		t.Fatalf("calls = %q, want the stop tried twice", f.agent.recorded()[before:])
	}
}

// TestWatchdogIgnoresAHeadExitFromAnotherServer pins whose frames can end a
// head's run: only its own server's. Another server that reports the head's
// spec running and then drops it neither ends the run nor releases a member.
func TestWatchdogIgnoresAHeadExitFromAnotherServer(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()

	f.frame("srv_other", watchHeadRow(agentSpecStateRunning))
	f.settle()
	f.frame("srv_other")
	f.settle()
	if calls := f.agent.recorded()[before:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want none for another server's frames", calls)
	}
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want still formed", st)
	}
	select {
	case <-f.o.TriggerChannel():
		t.Fatal("another server's frame woke the reconciler")
	default:
	}
}

// TestWatchdogCoolsDownAHeadThatCrashed pins a formed head that ends on its
// own with a new last error: a crash, not an idle stop. The members are
// released and the head cools down with the agent's code, or
// runtime.cluster_head_crashed when the agent named none, so a kept-up head
// is not started again at once.
func TestWatchdogCoolsDownAHeadThatCrashed(t *testing.T) {
	for _, tc := range []struct{ name, state, agentCode, want string }{
		{"unclassified crash", "crashed", "", codeClusterHeadCrashed},
		{"classified crash", "crashed", codeClusterMemberUnreachable, codeClusterMemberUnreachable},
		{"already in backoff", "backoff", "", codeClusterHeadCrashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrchFixture(t, 2, "relay", "lan")
			before := f.formed()

			f.frame(orchHeadServer, RuntimeStatusDTO{
				SpecID: orchHead, Model: orchHeadModel, State: tc.state,
				LastError: &RuntimeErrorDTO{Message: "runtime: process exited unexpectedly (exit code 1)", At: f.clock(), ExitCode: 1, Failures: 1, Code: tc.agentCode},
			})
			f.settle()
			if got := releases(f.agent.recorded()[before:]); !slices.Equal(got, allReleases(2)) {
				t.Fatalf("calls = %q, want every member released", f.agent.recorded()[before:])
			}
			if st := f.o.State(orchHead); st != clusterCoolingDown {
				t.Fatalf("state = %v, want cooling down after a crash", st)
			}
			status := f.o.ClusterStatus(orchHead)
			if status.LastFailure == nil || status.LastFailure.Code != tc.want || !status.LastFailure.Head || status.CooldownUntil == nil {
				t.Fatalf("status = %+v, want a failure %s at the head and a cooldown", status, tc.want)
			}
			ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
			if ce.Code != codeClusterStartFailed || ce.Cause != tc.want || !ce.Head {
				t.Fatalf("Ensure in the cooldown = %+v, want the crash", ce)
			}
		})
	}
}

// TestWatchdogEndsAHeadThatLostItsLaunchFacts pins a head switched to a
// plain spec while it ran: its server still reports a process for the spec,
// but without the launch facts it is no cluster head any more. The run ends,
// the members are released, and a head the plan no longer knows is
// forgotten, so a later member frame is no lost member of it.
func TestWatchdogEndsAHeadThatLostItsLaunchFacts(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	before := f.formed()
	f.portal.mu.Lock()
	f.portal.gone = true
	f.portal.mu.Unlock()

	plain := watchHeadRow(agentSpecStateStarting)
	plain.Cluster, plain.PID = nil, 101
	f.frame(orchHeadServer, plain)
	f.settle()
	if got := releases(f.agent.recorded()[before:]); !slices.Equal(got, allReleases(2)) {
		t.Fatalf("calls = %q, want every member released", f.agent.recorded()[before:])
	}
	f.o.mu.Lock()
	_, known := f.o.heads[orchHead]
	f.o.mu.Unlock()
	if known {
		t.Fatal("the orchestration still follows a head the plan no longer knows")
	}

	n := len(f.agent.recorded())
	plain.State = agentSpecStateRunning
	f.frame(orchMemberServer(0), watchMemberRow(0, "", 10))
	f.frame(orchHeadServer, plain)
	f.settle()
	if calls := f.agent.recorded()[n:]; len(calls) != 0 {
		t.Fatalf("calls = %q, want nothing for a head that is gone", calls)
	}
}

// TestBlockedAdmissionForceStopsADeadDirectHold pins the direct transport's
// half-open connection: a member that still reports the hold of a head that
// is gone holds a dead connection, which a release does not end (the agent
// lists the connection again), so a local start waiting for the member has it
// force-stopped once the head is down.
func TestBlockedAdmissionForceStopsADeadDirectHold(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "lan")
	f.formed()
	f.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	f.settle()

	before := len(f.agent.recorded())
	f.frame(orchMemberServer(0), blockedFrame(0)...)
	f.settle()
	calls := f.agent.recorded()[before:]
	want := []string{"head:stop:" + orchHeadModel + ":if_evictable", "member:stop:" + orchMemberSpec(0) + ":force"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}

	// Over a relay the session ends with the head, so a release is enough.
	g := newOrchFixture(t, 1, "relay", "lan")
	g.formed()
	g.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	g.settle()
	before = len(g.agent.recorded())
	g.frame(orchMemberServer(0), blockedFrame(0)...)
	g.settle()
	if calls := g.agent.recorded()[before:]; g.agent.count("member:stop:") != 0 || len(releases(calls)) != 1 {
		t.Fatalf("calls = %q, want the relay member released, not stopped", calls)
	}
}
