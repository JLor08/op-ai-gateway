// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

// The VRAM of wsl instances (#381).
//
// THE SOURCE. Windows charges all of the WSL VM's GPU memory to the VM's
// worker process, vmwp, as one PDH instance per guest GPU process, all named
// after the worker's pid; nothing inside the VM reports per-process GPU memory
// (Phase 0, B1/B2b). The VRAM measurer, asked about the worker's pid, already
// sums those instances per GPU, so the VM's per-GPU figure is one call away
// (sumVMVRAM). It is read only while a wsl instance is live.
//
// ATTRIBUTION BY START DELTA. The engines reserve their VRAM up front, so the
// VM's growth across one start is that workload's figure, provided nothing else
// starts or stops in the VM meanwhile. A wsl start therefore opens a window
// (wslStartWindow) with the VM's figure just before its exec as baseline, and
// the window closes on the first measurement dispatched after the instance's
// first passing health probe: the growth since the baseline is its attributed
// VRAM (wslAttrib), reported as its measured VRAM. Starts are serialized
// through the window -- a second wsl start waits (WSLStartInFlight) -- but
// stops never are: manual stops, idle unloads, evictions, removals and above
// all crashes cannot wait for another workload's window. A wsl exit during a
// window is corrected for instead (noteWSLExit): an attributed process's figure
// is added back to the delta, and one with no figure voids it.
//
// THE REMAINDER IS WHAT ADMISSION RELIES ON. Per-workload figures are best
// effort: a delta can come out low (growth after the window closed, a void
// window) or high. So at every admission, from a fresh reading, whatever the VM
// holds beyond the attributed figures is charged to each GPU as unattributed
// VRAM (wslRemainder, PolicySnapshot.UnattributedVRAM): with every attribution
// charged too (RunningProc.GPUs is max(attribution, demand)), the sum admission
// sees on a GPU is at least what the VM holds there -- it never sees free room
// the VM has taken. A figure that came out high only over-credits that
// workload's own eviction, and the candidate it was evicted for is admitted on
// the next fresh reading, after the exit.

// wslStartWindow is one wsl start's VRAM window: open from just before the
// exec of proc until the first measurement dispatched after proc's first
// passing health probe has returned (or proc exits). Owner-goroutine state.
type wslStartWindow struct {
	// proc is the generation the window measures.
	proc *runningProc
	// baseline is the VM's per-GPU figure read just before proc's exec.
	baseline map[int]int
	// exitedCredit is the attributed VRAM of the wsl processes that exited
	// while the window was open, summed per GPU: the VM's figure dropped by
	// that much for reasons that are not proc's, so it is added back.
	exitedCredit map[int]int
	// healthyDispatched is set when the measurement that settles the window
	// goes out: the first one dispatched once proc has passed a health probe.
	// A measurement already out when the probe passed was dispatched before it
	// and may have caught the load midway, so it never settles the window.
	healthyDispatched bool
	// void is set when a wsl process with no attribution exited during the
	// window: what it held left the VM's figure and cannot be added back, so
	// the delta would be too low by an unknown amount. A void window still
	// runs to its settling measurement -- the next start's delta must not
	// catch proc's own load -- and then closes without an attribution: proc is
	// covered by the unattributed remainder.
	void bool
}

// sumVMVRAM is the VM's per-GPU figure: the measurer's answer for the VM's
// worker pids, summed per GPU. A worker named twice is counted once, and a
// non-positive figure is none (realMeasurement's rule). Every worker the finder
// names is summed, the WSL VM's and any other Hyper-V VM's alike: nothing tells
// them apart (open question 1 of #381), and counting another VM's GPU memory as
// WSL's charges it to the unattributed remainder, so admission sees less free
// VRAM than there is, never more. nil when no worker holds anything.
func sumVMVRAM(byPID map[int]map[int]int, vmPIDs []int) map[int]int {
	var out map[int]int
	seen := make(map[int]bool, len(vmPIDs))
	for _, pid := range vmPIDs {
		if seen[pid] {
			continue
		}
		seen[pid] = true
		for g, mb := range byPID[pid] {
			if mb <= 0 {
				continue
			}
			if out == nil {
				out = make(map[int]int)
			}
			out[g] += mb
		}
	}
	return out
}

// wslDelta is a window's attribution: per GPU, the VM's figure after the start
// minus the one before, plus what exited processes took with them (credit),
// floored at 0. A GPU that did not grow is left out, and nil means the start
// grew the VM on no GPU at all.
func wslDelta(after, baseline, credit map[int]int) map[int]int {
	gpus := make(map[int]bool, len(after)+len(baseline)+len(credit))
	for _, m := range []map[int]int{after, baseline, credit} {
		for g := range m {
			gpus[g] = true
		}
	}
	var out map[int]int
	for g := range gpus {
		if v := after[g] - baseline[g] + credit[g]; v > 0 {
			if out == nil {
				out = make(map[int]int, len(gpus))
			}
			out[g] = v
		}
	}
	return out
}

// wslRemainder is the VM's VRAM no attribution accounts for, per GPU:
// max(0, vm - charged), zero entries dropped, nil when there is none.
//
// charged is the sum of the attributions on the GPUs their instance is
// actually charged on (buildSnapshot), not of every attribution: a delta can
// land on a GPU the spec does not declare -- another process in the VM grew
// there during the window -- and no RunningProc carries that figure, so taking
// it off the remainder would leave it charged nowhere. Left in, it is charged
// as unattributed, and the sum admission sees stays at least the VM's figure.
// A remainder below zero (attributions that came out high) is no credit:
// the attributions stand, as the max rule keeps any figure above its demand.
func wslRemainder(vm, charged map[int]int) map[int]int {
	var out map[int]int
	for g, mb := range vm {
		if r := mb - charged[g]; r > 0 {
			if out == nil {
				out = make(map[int]int, len(vm))
			}
			out[g] = r
		}
	}
	return out
}

// vramMeasurable reports whether spec's VRAM can be measured on this host: a
// VRAM measurer is installed (SetMeasurer) and, for a wsl spec, a finder for
// the VM's worker too (SetWSLVMPIDs), since its figure is the VM's.
func (o *owner) vramMeasurable(spec Spec) bool {
	if o.m.measurer.Load() == nil {
		return false
	}
	return !spec.IsWSL() || o.m.wslVMPIDs.Load() != nil
}

// readVMVRAM reads the VM's figure now, synchronously on the owner, and records
// it as the latest (owner.vmVRAM). ok is false when no reading can be taken: no
// VRAM measurer or no worker finder. A finder that names no worker reads as an
// empty VM -- a stopped VM holds nothing -- and so, unavoidably, does a reading
// that failed: the measurer answers nothing either way.
func (o *owner) readVMVRAM() (vm map[int]int, ok bool) {
	mp, vf := o.m.measurer.Load(), o.m.wslVMPIDs.Load()
	if mp == nil || vf == nil {
		return nil, false
	}
	if pids := (*vf)(); len(pids) > 0 {
		vm = sumVMVRAM((*mp)(pids), pids)
	}
	o.vmVRAM = vm
	return vm, true
}

// openWSLWindow opens proc's window, with baseline the VM's figure read just
// before its exec. Admission never lets a wsl start through while a window is
// open (WSLStartInFlight), so there is none here; were there one, the new start
// would be growing the VM inside it, so that window is voided, and proc goes
// without a window of its own -- unmeasured, covered by the remainder.
func (o *owner) openWSLWindow(proc *runningProc, baseline map[int]int) {
	if o.wslWindow != nil {
		o.wslWindow.void = true
		return
	}
	o.wslWindow = &wslStartWindow{proc: proc, baseline: baseline}
}

// settleWSLWindow closes w with the measurement c, which was dispatched after
// w's process first passed a health probe: unless w is void or c carries no VM
// reading, the VM's growth since the baseline, corrected by what exited
// processes took with them, is the process's attribution and its measured VRAM
// -- written back to the gateway as vram_measured_mb, a ready figure siblings
// inherit, and the resolution of its VRAM bootstrap, exactly as a native
// process's first measurement is. landed reports a recorded figure (the
// demand wake's trigger, as in applyMeasurement). The caller wakes the wsl
// starts that waited for the window.
func (o *owner) settleWSLWindow(w *wslStartWindow, c cmdMeasured) (landed bool) {
	o.wslWindow = nil
	st := o.specs[w.proc.specID]
	if w.void || !c.vmRead || st == nil || st.proc != w.proc {
		return false
	}
	attrib := wslDelta(sumVMVRAM(c.byPID, c.vmPIDs), w.baseline, w.exitedCredit)
	if o.wslAttrib == nil {
		o.wslAttrib = make(map[*runningProc]map[int]int)
	}
	o.wslAttrib[w.proc] = attrib
	if attrib == nil {
		return false
	}
	st.measuredVRAM = attrib
	st.readyVRAM = attrib
	st.vramBootstrapDone = true
	return true
}

// noteWSLExit corrects the open window for the exit of the wsl generation
// proc, and deletes proc's attribution. Stops are never held back for a
// window, so an exit can fall inside one at any time:
//
//   - proc is the window's own process: the window closes without an
//     attribution (it measured nothing that still exists). The exit's own
//     admission wake re-asks the starts that waited for it.
//   - proc was attributed: the VM's figure drops by about that much during the
//     window, which is not the window's process shrinking, so the attribution
//     is added back as credit (wslDelta).
//   - proc was not attributed: what it held is unknown, so the delta cannot be
//     corrected, and the window is void (wslStartWindow.void).
func (o *owner) noteWSLExit(proc *runningProc) {
	if w := o.wslWindow; w != nil {
		attrib, attributed := o.wslAttrib[proc]
		switch {
		case w.proc == proc:
			o.wslWindow = nil
		case attributed:
			if w.exitedCredit == nil {
				w.exitedCredit = make(map[int]int, len(attrib))
			}
			for g, mb := range attrib {
				w.exitedCredit[g] += mb
			}
		default:
			w.void = true
		}
	}
	delete(o.wslAttrib, proc)
}

// wantsWSLStartWake is the selection rule for the wake a window's close causes
// (applyMeasurement): a wsl spec that waited for the window
// (specState.waitingOnWSLStart), narrowed like the other event wakes to one
// with a queued request or a pinned one (wantsWakeAfterStart).
func wantsWSLStartWake(st *specState) bool {
	return st.waitingOnWSLStart && wantsWakeAfterStart(st)
}
