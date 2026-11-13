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
		// Host: 6000 + 6000 + 5000 = 17000 > 7000 needs both processes gone;
		// the VM then has room. Each victim appears once.
		tight := snap
		tight.HostRAMBudgetMB = 7000
		assertDecision(t, "both pools", Admit(tight, wslCandidate("cand", 5000)), Decision{Evict: []string{"native", "wsl"}})

		// When the host pool's own pick is the wsl process (the older one
		// now) and it is enough there (-> 11000 <= 12000), the VM pool sees it
		// gone and asks for nothing more.
		running := wslPoolRunning()
		running[0].LastUsed, running[1].LastUsed = t1, t0
		tight.Running = running
		tight.HostRAMBudgetMB = 12000
		assertDecision(t, "host's wsl victim serves the VM too", Admit(tight, wslCandidate("cand", 5000)), Decision{Evict: []string{"wsl"}})
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
