// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ensureMember starts the member through Manager.EnsureMember and fails the
// test on any error.
func ensureMember(t *testing.T, m *Manager, id string, reserve time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.EnsureMember(ctx, id, reserve); err != nil {
		t.Fatalf("EnsureMember(%s): %v", id, err)
	}
}

// TestReservedMemberSurvivesIdleScan: a reserved member and a held one both
// outlive their idle timeout; once the hold ends the member idles out.
func TestReservedMemberSurvivesIdleScan(t *testing.T) {
	shrinkTimings(t)
	reserved, held := onDemandMember(t, "rpcm_r"), onDemandMember(t, "rpcm_h")
	reserved.IdleTimeoutSeconds, held.IdleTimeoutSeconds = 1, 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{reserved, held}})

	ensureMember(t, m, "rpcm_r", 30*time.Second)
	ensureMember(t, m, "rpcm_h", 0)
	m.SetMemberHold("rpcm_h", "rspec_h", time.Now())

	time.Sleep(1600 * time.Millisecond) // past the 1 s idle timeout, many idle ticks
	for _, id := range []string{"rpcm_r", "rpcm_h"} {
		if s := statusFor(m, id); s.State != StateRunning {
			t.Fatalf("%s state = %s, want running: a claimed member is never idle-unloaded", id, s.State)
		}
	}

	m.SetMemberHold("rpcm_h", "", time.Time{})
	waitUntil(t, 5*time.Second, "released member idles out", func() bool {
		return statusFor(m, "rpcm_h").State == StateStopped
	})
	if s := statusFor(m, "rpcm_r"); s.State != StateRunning {
		t.Fatalf("reserved member state = %s, want running", s.State)
	}
}

// TestReservationExpires: the reservation ends at its deadline on its own, and
// the member then idles out on its ordinary timeout.
func TestReservationExpires(t *testing.T) {
	shrinkTimings(t)
	spec := onDemandMember(t, "rpcm_a")
	spec.IdleTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})

	ensureMember(t, m, "rpcm_a", time.Second)
	if memberRPC(t, m, "rpcm_a").ReservedUntil.IsZero() {
		t.Fatal("no reservation right after the ensure")
	}
	waitUntil(t, 3*time.Second, "reservation expired", func() bool {
		return memberRPC(t, m, "rpcm_a").ReservedUntil.IsZero()
	})
	waitUntil(t, 4*time.Second, "member idles out after its reservation", func() bool {
		return statusFor(m, "rpcm_a").State == StateStopped
	})
}

// TestReservationIsNeverShortened: a second ensure with a shorter reservation
// keeps the later deadline; a longer one extends it.
func TestReservationIsNeverShortened(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})

	ensureMember(t, m, "rpcm_a", 60*time.Second)
	first := memberRPC(t, m, "rpcm_a").ReservedUntil
	ensureMember(t, m, "rpcm_a", 5*time.Second)
	if got := memberRPC(t, m, "rpcm_a").ReservedUntil; !got.Equal(first) {
		t.Fatalf("reserved_until = %v, want the earlier, later deadline %v kept", got, first)
	}
	ensureMember(t, m, "rpcm_a", 120*time.Second)
	if got := memberRPC(t, m, "rpcm_a").ReservedUntil; !got.After(first) {
		t.Fatalf("reserved_until = %v, want it extended past %v", got, first)
	}
}

// gpuMember is an on-demand member lending GPU 0 with a known VRAM demand.
func gpuMember(t *testing.T, id string, vramMB int) Spec {
	t.Helper()
	spec := onDemandMember(t, id)
	spec.DeviceBackend = "cuda"
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: vramMB}}
	return spec
}

// gpuModel is a model spec on GPU 0; vramMB 0 leaves its demand unknown.
func gpuModel(id, upstream string, vramMB int) Spec {
	spec := baseSpec(id, upstream)
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: vramMB}}
	spec.AdmissionWaitTimeoutSeconds = 0 // wait as long as the caller does
	return spec
}

// tenGiBOnGPU0 is a host whose GPU 0 holds one 6 GiB process, not two.
var tenGiBOnGPU0 = []GPUBudget{{Index: 0, BudgetMB: 10000}, {Index: 1, BudgetMB: 10000}}

// ensureModelAsync runs EnsureRunning(model) on its own goroutine, releasing
// at once, and delivers its error.
func ensureModelAsync(ctx context.Context, m *Manager, model string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, release, err := m.EnsureRunning(ctx, model)
		if err == nil {
			release()
		}
		done <- err
	}()
	return done
}

// TestHeldMemberNeverEvicted: a local start that could only proceed by
// evicting a held member (here: GPU 0 has room for one of them) waits, names the
// member on its row, and -- without an eviction grace -- proceeds as soon as
// the hold ends, evicting the member; the name then leaves its row. With an
// eviction grace the member is named all the same: the grace its start stamped
// does not hide that the claim is what blocks.
func TestHeldMemberNeverEvicted(t *testing.T) {
	for _, grace := range []time.Duration{0, 30 * time.Second} {
		t.Run(grace.String(), func(t *testing.T) {
			shrinkTimings(t)
			m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }, EvictionGrace: grace})
			t.Cleanup(m.Close)
			m.Apply(Config{
				Specs:      []Spec{gpuModel("spec-a", "model-a", 6000), gpuMember(t, "rpcm_a", 6000)},
				GPUBudgets: tenGiBOnGPU0,
			})

			ensureMember(t, m, "rpcm_a", 0)
			m.SetMemberHold("rpcm_a", "rspec_h", time.Now())

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := ensureModelAsync(ctx, m, "model-a")

			waitUntil(t, 3*time.Second, "waiting spec names the held member", func() bool {
				return statusFor(m, "spec-a").AdmissionBlockedBy == "rpcm_a"
			})
			time.Sleep(200 * time.Millisecond)
			if s := statusFor(m, "rpcm_a"); s.State != StateRunning {
				t.Fatalf("held member state = %s, want running: it is never an eviction victim", s.State)
			}
			select {
			case err := <-done:
				t.Fatalf("EnsureRunning returned %v while the member was held, want it to wait", err)
			default:
			}
			if grace > 0 {
				return
			}

			m.SetMemberHold("rpcm_a", "", time.Time{})
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("EnsureRunning after the hold ended: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the waiting start did not proceed after the hold ended")
			}
			if s := statusFor(m, "spec-a"); s.AdmissionBlockedBy != "" {
				t.Fatalf("admission_blocked_by = %q, want it cleared once the wait ended", s.AdmissionBlockedBy)
			}
			if s := statusFor(m, "rpcm_a"); s.State == StateRunning {
				t.Fatalf("released member still running beside the model that needed its room")
			}
		})
	}
}

// TestAdmissionBlockedByNamesOnlyHeldMembers: a wait behind an ordinary pinned
// model names no member, even with a held member on the host, as long as that
// member is not what the start needs gone.
func TestAdmissionBlockedByNamesOnlyHeldMembers(t *testing.T) {
	shrinkTimings(t)
	pinned := gpuModel("spec-p", "model-p", 6000)
	pinned.Pinned = true
	member := gpuMember(t, "rpcm_b", 6000)
	member.GPUs[0].Index = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{pinned, gpuModel("spec-a", "model-a", 6000), member}, GPUBudgets: tenGiBOnGPU0})
	waitUntil(t, 3*time.Second, "pinned spec running", func() bool {
		return statusFor(m, "spec-p").State == StateRunning
	})
	ensureMember(t, m, "rpcm_b", 0)
	m.SetMemberHold("rpcm_b", "rspec_h", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ensureModelAsync(ctx, m, "model-a")
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := statusFor(m, "spec-a").AdmissionBlockedBy; got != "" {
			t.Fatalf("admission_blocked_by = %q, want empty behind a pinned model", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := statusFor(m, "spec-a"); s.State != StateStopped {
		t.Fatalf("spec-a state = %s, want stopped while it waits", s.State)
	}
}

// TestUnknownDemandStartWaitsForHeldMember: a start of unknown VRAM demand on
// a GPU a held member uses is not refused as pending_vram_unknown -- a claim
// ends, unlike a pin -- but waits, naming the member, and starts once the
// member has gone after its hold ended.
func TestUnknownDemandStartWaitsForHeldMember(t *testing.T) {
	shrinkTimings(t)
	member := gpuMember(t, "rpcm_a", 6000)
	member.IdleTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{gpuModel("spec-u", "model-u", 0), member}, GPUBudgets: tenGiBOnGPU0})

	ensureMember(t, m, "rpcm_a", 0)
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := ensureModelAsync(ctx, m, "model-u")
	waitUntil(t, 3*time.Second, "unknown-demand start names the held member", func() bool {
		return statusFor(m, "spec-u").AdmissionBlockedBy == "rpcm_a"
	})
	time.Sleep(200 * time.Millisecond)
	if s := statusFor(m, "spec-u"); s.State != StateStopped {
		t.Fatalf("spec-u state = %s, want stopped (waiting), not a refusal", s.State)
	}
	select {
	case err := <-done:
		t.Fatalf("EnsureRunning returned %v while the member was held, want it to wait", err)
	default:
	}

	m.SetMemberHold("rpcm_a", "", time.Time{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRunning after the hold ended: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the unknown-demand start did not proceed after the hold ended")
	}
}

// TestMemberCoresidesWithoutPair: the co-residency matrix does not apply to a
// member, in either direction: a model with no pair starts beside a running
// member, and a member starts beside a running model, neither evicting the
// other.
func TestMemberCoresidesWithoutPair(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{baseSpec("spec-a", "model-a"), onDemandMember(t, "rpcm_a"), baseSpec("spec-b", "model-b")}})

	ensureMember(t, m, "rpcm_a", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := <-ensureModelAsync(ctx, m, "model-a"); err != nil {
		t.Fatalf("EnsureRunning(model-a) beside a member: %v", err)
	}
	ensureMember(t, m, "rpcm_a", 0) // already up: answered at once
	for _, id := range []string{"spec-a", "rpcm_a"} {
		if s := statusFor(m, id); s.State != StateRunning {
			t.Fatalf("%s state = %s, want running side by side", id, s.State)
		}
	}

	// A second model still obeys the matrix towards the first: with no pair,
	// the first (idle) one is evicted, and the member stays.
	if err := <-ensureModelAsync(ctx, m, "model-b"); err != nil {
		t.Fatalf("EnsureRunning(model-b): %v", err)
	}
	waitUntil(t, 3*time.Second, "spec-a evicted by the matrix", func() bool {
		return statusFor(m, "spec-a").State == StateStopped
	})
	if s := statusFor(m, "rpcm_a"); s.State != StateRunning {
		t.Fatalf("member state = %s, want running: the matrix does not apply to it", s.State)
	}
}

// TestMemberHoldNeedsAProcess: a hold reported for a member that is not running
// is ignored, and one for an unknown id or a model spec changes nothing.
func TestMemberHoldNeedsAProcess(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a"), baseSpec("spec-a", "model-a")}})
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	m.SetMemberHold("rpcm_nope", "rspec_h", time.Now())
	m.SetMemberHold("spec-a", "rspec_h", time.Now())
	if r := memberRPC(t, m, "rpcm_a"); r.HeldBy != "" {
		t.Fatalf("rpc = %+v, want no hold on a member without a process", r)
	}
	if s := statusFor(m, "spec-a"); s.RPC != nil {
		t.Fatalf("model row rpc = %+v, want none", s.RPC)
	}
}

// TestEnsureMemberRefusesModelSpecs: EnsureMember never starts a model spec,
// and EnsureRunning never starts a member.
func TestEnsureMemberRefusesModelSpecs(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a"), baseSpec("spec-a", "model-a")}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.EnsureMember(ctx, "spec-a", time.Second); !errors.Is(err, ErrModelNotManaged) {
		t.Fatalf("EnsureMember(model spec) = %v, want ErrModelNotManaged", err)
	}
	if _, _, err := m.EnsureRunning(ctx, "rpcm_a"); !errors.Is(err, ErrModelNotManaged) {
		t.Fatalf("EnsureRunning(member id) = %v, want ErrModelNotManaged", err)
	}
	if _, err := m.StopMember(ctx, "spec-a", true); !errors.Is(err, ErrModelNotManaged) {
		t.Fatalf("StopMember(model spec) = %v, want ErrModelNotManaged", err)
	}
	if err := m.ReleaseMember(ctx, "spec-a"); !errors.Is(err, ErrModelNotManaged) {
		t.Fatalf("ReleaseMember(model spec) = %v, want ErrModelNotManaged", err)
	}
}

// TestClaimedMemberIsPinnedForAdmission: a hold or a reservation is what makes
// a member's admission flag read pinned, so every victim choice skips it.
func TestClaimedMemberIsPinnedForAdmission(t *testing.T) {
	st := &specState{spec: memberSpec("rpcm_a", "rspec_h")}
	if st.pinnedForAdmission() {
		t.Fatal("an unclaimed member reads pinned")
	}
	st.claim.heldBy = "rspec_h"
	if !st.pinnedForAdmission() {
		t.Fatal("a held member does not read pinned")
	}
	st.claim = memberClaim{reservedUntil: time.Now().Add(time.Minute)}
	if !st.pinnedForAdmission() {
		t.Fatal("a reserved member does not read pinned")
	}
}

// TestReleaseRecordsReleasedAt: every release of a member stamps the time on
// its status row (RPCReleasedAt), which the direct-mode observer reads to tell
// the connections the release ended from a head's new one; the stamp moves on
// with each release.
func TestReleaseRecordsReleasedAt(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})
	ensureMember(t, m, "rpcm_a", 30*time.Second)
	if at := statusFor(m, "rpcm_a").RPCReleasedAt; !at.IsZero() {
		t.Fatalf("RPCReleasedAt = %v before any release, want zero", at)
	}

	before := time.Now()
	if err := m.ReleaseMember(context.Background(), "rpcm_a"); err != nil {
		t.Fatal(err)
	}
	first := statusFor(m, "rpcm_a").RPCReleasedAt
	if first.Before(before) {
		t.Fatalf("RPCReleasedAt = %v, want at or after %v", first, before)
	}

	// A release of a member nothing claims still counts: a hold the observer
	// reported just before may land after it.
	time.Sleep(5 * time.Millisecond)
	if err := m.ReleaseMember(context.Background(), "rpcm_a"); err != nil {
		t.Fatal(err)
	}
	if second := statusFor(m, "rpcm_a").RPCReleasedAt; !second.After(first) {
		t.Fatalf("RPCReleasedAt = %v after a second release, want later than %v", second, first)
	}
}

// TestClaimedMemberRelaunchWaitsForClaimEnd: a launch-shape change of a member
// its head holds or has reserved does not kill it under the running head (the
// head would abort mid-request); the member keeps its process until the claim
// ends, and is relaunched with the new shape only then. Each claim ending
// counts: the hold going while the reservation stands defers further.
func TestClaimedMemberRelaunchWaitsForClaimEnd(t *testing.T) {
	shrinkTimings(t)
	spec := onDemandMember(t, "rpcm_a")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}, ETag: "1"})
	ensureMember(t, m, "rpcm_a", 30*time.Second)
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	pid := statusFor(m, "rpcm_a").PID

	changed := spec
	changed.Env = map[string]string{"OP_TEST_MEMBER_EDIT": "1"}
	m.Apply(Config{Specs: []Spec{changed}, ETag: "2"})
	time.Sleep(200 * time.Millisecond)
	if s := statusFor(m, "rpcm_a"); s.State != StateRunning || s.PID != pid {
		t.Fatalf("member = %s pid %d after a launch-shape change while claimed, want running pid %d", s.State, s.PID, pid)
	}

	m.SetMemberHold("rpcm_a", "", time.Time{}) // the reservation still stands
	time.Sleep(200 * time.Millisecond)
	if s := statusFor(m, "rpcm_a"); s.State != StateRunning || s.PID != pid {
		t.Fatalf("member = %s pid %d with its reservation standing, want running pid %d", s.State, s.PID, pid)
	}

	if err := m.ReleaseMember(context.Background(), "rpcm_a"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, "the old member process leaves once nothing claims it", func() bool {
		s := statusFor(m, "rpcm_a")
		return s.PID != pid
	})
	ensureMember(t, m, "rpcm_a", 0)
	if s := statusFor(m, "rpcm_a"); s.State != StateRunning || s.PID == pid {
		t.Fatalf("member = %s pid %d, want running on a new process", s.State, s.PID)
	}
}

// TestUnclaimedMemberRelaunchesAtOnce: without a claim a member's launch-shape
// change relaunches it like any spec.
func TestUnclaimedMemberRelaunchesAtOnce(t *testing.T) {
	shrinkTimings(t)
	spec := onDemandMember(t, "rpcm_a")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}, ETag: "1"})
	ensureMember(t, m, "rpcm_a", 0)
	pid := statusFor(m, "rpcm_a").PID

	changed := spec
	changed.Env = map[string]string{"OP_TEST_MEMBER_EDIT": "1"}
	m.Apply(Config{Specs: []Spec{changed}, ETag: "2"})
	waitUntil(t, 5*time.Second, "unclaimed member drained", func() bool {
		return statusFor(m, "rpcm_a").PID != pid
	})
}

// TestEnsureRelaunchesDeferredMemberWithoutHold: an ensure for a member whose
// relaunch waits for its claim, while no head holds it (only a reservation
// remains), drains it and answers with a process on the current launch, so the
// head the gateway starts next forms against the new shape. With the hold
// standing the ensure keeps the running process.
func TestEnsureRelaunchesDeferredMemberWithoutHold(t *testing.T) {
	shrinkTimings(t)
	spec := onDemandMember(t, "rpcm_a")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}, ETag: "1"})
	ensureMember(t, m, "rpcm_a", 30*time.Second)
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	pid := statusFor(m, "rpcm_a").PID

	changed := spec
	changed.Env = map[string]string{"OP_TEST_MEMBER_EDIT": "1"}
	m.Apply(Config{Specs: []Spec{changed}, ETag: "2"})

	ensureMember(t, m, "rpcm_a", 30*time.Second)
	if s := statusFor(m, "rpcm_a"); s.PID != pid {
		t.Fatalf("pid = %d after an ensure while held, want the running %d", s.PID, pid)
	}

	m.SetMemberHold("rpcm_a", "", time.Time{}) // the reservation still stands
	ensureMember(t, m, "rpcm_a", 30*time.Second)
	s := statusFor(m, "rpcm_a")
	if s.State != StateRunning || s.PID == pid || s.PID == 0 {
		t.Fatalf("member = %s pid %d after an ensure with no hold, want running on a new process", s.State, s.PID)
	}
	if memberRPC(t, m, "rpcm_a").ReservedUntil.IsZero() {
		t.Fatal("the relaunched member carries no reservation from the ensure")
	}
}
