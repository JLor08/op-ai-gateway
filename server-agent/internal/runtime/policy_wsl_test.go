// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import "testing"

// Tests for the WSL VM RAM pool in Admit (#381): a wsl candidate must also fit
// the VM's own RAM ceiling, beside the host pool, and only wsl processes are
// charged against it or drawn as its victims.

// wslCandidate is a wsl spec with a known RAM demand for the policy tests.
func wslCandidate(id string, ramMB int) Spec {
	return Spec{ID: id, RAMMB: ramMB, Execution: &Execution{Mode: ExecutionModeWSL, WSL: &WSLExecution{Distro: wslTestDistro}}}
}

// wslPoolRunning is an idle native process and an idle wsl process of 6000 MB
// each; the native one is the OLDER, so a victim pick that ignored the WSL
// restriction would take it first.
func wslPoolRunning() []RunningProc {
	return []RunningProc{
		{SpecID: "native", RAMMB: 6000, LastUsed: t0},
		{SpecID: "wsl", RAMMB: 6000, LastUsed: t1, WSL: true},
	}
}

func TestAdmitWSLRAMFitsVMBudget(t *testing.T) {
	snap := PolicySnapshot{
		Running:        wslPoolRunning(),
		WSLRAMBudgetMB: 10000,
		Allowed:        allowPairs("cand", "native", "wsl"),
	}
	// 6000 (wsl) + 4000 = 10000 fits the VM pool, boundary inclusive; the
	// native 6000 is not in the VM and is not charged against it.
	assertDecision(t, "wsl candidate within the VM pool", Admit(snap, wslCandidate("cand", 4000)), Decision{OK: true, Evict: []string{}})
}

func TestAdmitWSLRAMEvictsIdleWSLOnly(t *testing.T) {
	snap := PolicySnapshot{
		Running:         wslPoolRunning(),
		WSLRAMBudgetMB:  10000,
		HostRAMBudgetMB: 64000,
		Allowed:         allowPairs("cand", "native", "wsl"),
	}
	// 6000 + 5000 = 11000 > 10000: the wsl process goes, never the older
	// native one, whose RAM is not in the VM.
	assertDecision(t, "wsl candidate over the VM pool", Admit(snap, wslCandidate("cand", 5000)), Decision{Evict: []string{"wsl"}})

	// A native candidate is not charged against the VM pool at all.
	assertDecision(t, "native candidate", Admit(snap, Spec{ID: "cand", RAMMB: 5000}), Decision{OK: true, Evict: []string{}})

	t.Run("a busy wsl holder makes it a RAM wait, never a native victim", func(t *testing.T) {
		busy := snap
		busy.Running = wslPoolRunning()
		busy.Running[1].InFlight = 1
		assertDecision(t, "busy wsl holder", Admit(busy, wslCandidate("cand", 5000)), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("both pools draw, and a victim is drawn once", func(t *testing.T) {
		// VM: 6000 + 5000 > 10000 takes the wsl process; host: 6000 + 5000 =
		// 11000 > 7000 then still needs the native one. Each appears once.
		tight := snap
		tight.HostRAMBudgetMB = 7000
		assertDecision(t, "both pools", Admit(tight, wslCandidate("cand", 5000)), Decision{Evict: []string{"native", "wsl"}})
	})

	t.Run("a native candidate is neither blocked by nor evicts for the VM pool", func(t *testing.T) {
		// The VM is over its pool already (a busy wsl process of 6000 MB
		// against 5000), which would block a wsl candidate; a native one
		// starts, and the wsl processes are left alone.
		full := snap
		full.WSLRAMBudgetMB = 5000
		full.Running = wslPoolRunning()
		full.Running[1].InFlight = 1
		assertDecision(t, "native beside a full VM", Admit(full, Spec{ID: "cand", RAMMB: 5000}), Decision{OK: true, Evict: []string{}})
		full.Running[1].InFlight = 0
		assertDecision(t, "native beside a full VM, idle wsl", Admit(full, Spec{ID: "cand", RAMMB: 5000}), Decision{OK: true, Evict: []string{}})
	})

	t.Run("an unknown-RAM wsl candidate is charged the bootstrap guess", func(t *testing.T) {
		guess := snap
		guess.RAMBootstrapGuessMB = 5000
		assertDecision(t, "guess over the VM pool", Admit(guess, wslCandidate("cand", 0)), Decision{Evict: []string{"wsl"}})
		guess.RAMBootstrapGuessMB = 0
		assertDecision(t, "guess off", Admit(guess, wslCandidate("cand", 0)), Decision{OK: true, Evict: []string{}})
	})

	t.Run("a RAM bootstrap in flight makes a wsl candidate wait", func(t *testing.T) {
		boot := snap
		boot.HostRAMBudgetMB = 0 // the VM pool alone must hold the interlock
		boot.RAMBootstrapInFlight = true
		assertDecision(t, "bootstrap in flight", Admit(boot, wslCandidate("cand", 1000)), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})
}

// TestAdmitWSLRAMVictimServesTheHostPoolToo pins the order of the two RAM
// pools: the VM pool draws first, and its wsl victim frees host RAM as well,
// so the older idle native process survives. Host first, the host pool would
// take the native process (the oldest) and the VM pool then the wsl one too.
func TestAdmitWSLRAMVictimServesTheHostPoolToo(t *testing.T) {
	snap := PolicySnapshot{
		Running:         wslPoolRunning(), // native 6000 at t0 (older), wsl 6000 at t1
		WSLRAMBudgetMB:  10000,
		HostRAMBudgetMB: 12000,
		Allowed:         allowPairs("cand", "native", "wsl"),
	}
	// VM: 6000 + 5000 = 11000 > 10000 -> wsl goes. Host: with it gone,
	// 6000 + 5000 = 11000 <= 12000, so the native process stays.
	assertDecision(t, "VM victim serves the host", Admit(snap, wslCandidate("cand", 5000)), Decision{Evict: []string{"wsl"}})
}

func TestAdmitWSLRAMOwnDemandRefusal(t *testing.T) {
	snap := PolicySnapshot{WSLRAMBudgetMB: 6000, WSLRAMCeilingMB: 8000}
	assertDecision(t, "wsl demand over the VM ceiling", Admit(snap, wslCandidate("X", 9000)), Decision{
		Reason:  StateNotPermitted,
		Message: "spec X: RAM demand 9000 MB exceeds the WSL VM RAM budget 8000 MB on its own",
		Evict:   []string{},
	})

	// Between the budget and the ceiling is not hopeless: the arithmetic
	// waits for the pool instead of refusing for good.
	assertDecision(t, "between budget and ceiling", Admit(snap, wslCandidate("X", 7000)), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})

	// A native candidate is never measured against the VM.
	assertDecision(t, "native demand", Admit(snap, Spec{ID: "X", RAMMB: 9000}), Decision{OK: true, Evict: []string{}})

	// A spec over both pools keeps reporting the host cause.
	snap.HostRAMBudgetMB = 4000
	got := Admit(snap, wslCandidate("X", 9000))
	if want := "spec X: RAM demand 9000 MB exceeds the host RAM budget 4000 MB on its own"; got.Message != want {
		t.Errorf("over both pools: Message = %q, want %q", got.Message, want)
	}
}

func TestAdmitWSLRAMUnconstrainedWithoutSample(t *testing.T) {
	snap := PolicySnapshot{
		Running:         wslPoolRunning(),
		WSLRAMBudgetMB:  0,
		WSLRAMCeilingMB: 0,
		Allowed:         allowPairs("cand", "native", "wsl"),
	}
	assertDecision(t, "no VM sample", Admit(snap, wslCandidate("cand", 50000)), Decision{OK: true, Evict: []string{}})
}

// TestAdmitWSLRAMWaitOnWSLVM: only the VM pool's arithmetic marks a Wait as
// one a fresh VM sample may end -- not the host pool, and not the bootstrap
// interlock, which no sample resolves.
func TestAdmitWSLRAMWaitOnWSLVM(t *testing.T) {
	busy := wslPoolRunning()
	busy[0].InFlight, busy[1].InFlight = 1, 1
	snap := PolicySnapshot{Running: busy, WSLRAMBudgetMB: 10000, HostRAMBudgetMB: 64000, Allowed: allowPairs("cand", "native", "wsl")}
	if dec := Admit(snap, wslCandidate("cand", 5000)); !dec.Wait || !dec.WaitOnRAM || !dec.WaitOnWSLVM {
		t.Errorf("VM pool full: %+v, want Wait, WaitOnRAM and WaitOnWSLVM", dec)
	}

	host := snap
	host.WSLRAMBudgetMB, host.HostRAMBudgetMB = 64000, 10000
	if dec := Admit(host, wslCandidate("cand", 5000)); !dec.Wait || !dec.WaitOnRAM || dec.WaitOnWSLVM {
		t.Errorf("host pool full: %+v, want a RAM wait without WaitOnWSLVM", dec)
	}

	boot := snap
	boot.HostRAMBudgetMB, boot.RAMBootstrapInFlight = 0, true
	if dec := Admit(boot, wslCandidate("cand", 1000)); !dec.Wait || dec.WaitOnWSLVM {
		t.Errorf("bootstrap in flight: %+v, want a wait without WaitOnWSLVM", dec)
	}
}
