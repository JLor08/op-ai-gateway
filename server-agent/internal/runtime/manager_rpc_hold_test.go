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
	m.Apply(Config{Specs: []Spec{reserved, held}, Coresident: [][2]string{{"rpcm_r", "rpcm_h"}}})

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

// TestHeldMemberNeverEvicted: a local start that could only proceed by
// evicting a held member waits, names the member on its row, and proceeds once
// the hold ends; the name then leaves its row.
func TestHeldMemberNeverEvicted(t *testing.T) {
	shrinkTimings(t)
	model := baseSpec("spec-a", "model-a")
	model.AdmissionWaitTimeoutSeconds = 0 // wait as long as the caller does
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{model, onDemandMember(t, "rpcm_a")}})

	ensureMember(t, m, "rpcm_a", 0)
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, release, err := m.EnsureRunning(ctx, "model-a")
		if err == nil {
			release()
		}
		done <- err
	}()

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
	waitUntil(t, 3*time.Second, "released member evicted", func() bool {
		return statusFor(m, "rpcm_a").State == StateStopped
	})
}

// TestAdmissionBlockedByNamesOnlyHeldMembers: a wait behind an ordinary pinned
// model names no member.
func TestAdmissionBlockedByNamesOnlyHeldMembers(t *testing.T) {
	shrinkTimings(t)
	pinned := baseSpec("spec-p", "model-p")
	pinned.Pinned = true
	model := baseSpec("spec-a", "model-a")
	model.AdmissionWaitTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{pinned, model}})
	waitUntil(t, 3*time.Second, "pinned spec running", func() bool {
		return statusFor(m, "spec-p").State == StateRunning
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, release, err := m.EnsureRunning(ctx, "model-a")
		if err == nil {
			release()
		}
	}()
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := statusFor(m, "spec-a").AdmissionBlockedBy; got != "" {
			t.Fatalf("admission_blocked_by = %q, want empty behind a pinned model", got)
		}
		time.Sleep(20 * time.Millisecond)
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
	if err := m.ReleaseMember("spec-a"); !errors.Is(err, ErrModelNotManaged) {
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
