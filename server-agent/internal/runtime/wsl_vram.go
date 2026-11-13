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
// ONLY A SPEC THAT DECLARES A GPU HAS A WINDOW. A wsl spec with no GPU is taken
// to hold no VRAM: its start is not serialized and opens no window, and it is
// attributed nothing, so its exit during another start's window changes
// nothing there. Should it use a GPU after all, that memory is in the VM's
// figure and so in the remainder. Likewise an attribution covers only the GPUs
// its spec declares: growth elsewhere during the window may be anyone's.
//
// A READING THAT IS NOT THE VM'S IS NEVER A DELTA'S END. The measurer answers
// nothing both when the VM holds nothing and when the read failed, so a
// reading is taken as failed where the VM cannot be empty (vmReadingFailed): no
// worker found while a wsl process lives in the VM, or nothing held while a
// live wsl process is attributed VRAM -- and, at a window's settle, nothing held
// after a baseline that held something. A window whose baseline failed opens
// void, one whose settling reading failed closes void: a delta against a
// failed reading would credit one instance with the whole VM, which is written
// back as its measured demand (ADR-084), and a demand over its budget is
// refused for good. At admission a failed reading falls back to the last good
// one of the current period of WSL activity (owner.vmVRAM), and to nothing in
// a period that has had none yet: once no wsl generation is live the last
// reading is forgotten (endVMPeriodIfIdle). A failure that looks like an empty
// VM -- a failed read at the first start, with nothing wsl live -- cannot be
// told apart from one.
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
// actually charged on (buildSnapshot), not of every attribution. A settle keeps
// an attribution to the spec's declared GPUs already, and a change of the GPU
// list relaunches the generation, so the two agree; summing only what is
// charged makes the invariant hold by construction all the same: a figure no
// RunningProc carries, taken off the remainder, would be charged nowhere. Left
// in, it is charged as unattributed, and the sum admission sees stays at least
// the VM's figure.
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

// wslDeclaresGPU reports whether a wsl spec declares a GPU, which is what gives
// its start a window and an attribution (see above).
func wslDeclaresGPU(spec Spec) bool {
	return len(spec.GPUs) > 0
}

// vmReadingFailed reports whether a VM reading -- the worker pids the finder
// named and the figure they summed to -- is detectably a failed one rather
// than the VM's state: no worker at all while a wsl process is live (the VM it
// runs in has one), or nothing held while a live wsl process is attributed
// VRAM (the VM holds at least that). Attributions are deleted on exit, so
// every one in wslAttrib is a live generation's.
func (o *owner) vmReadingFailed(vmPIDs []int, vm map[int]int) bool {
	if len(vmPIDs) == 0 {
		return o.wslLive()
	}
	if vm != nil {
		return false
	}
	for _, a := range o.wslAttrib {
		if len(a) > 0 {
			return true
		}
	}
	return false
}

// readVMVRAM reads the VM's figure now, synchronously on the owner, and records
// a good reading as the last good one (owner.vmVRAM). ok is false when no
// reading can be taken -- no VRAM measurer or no worker finder -- or when it is
// detectably failed (vmReadingFailed). A finder that names no worker with
// nothing wsl live reads as an empty VM: a stopped VM holds nothing.
func (o *owner) readVMVRAM() (vm map[int]int, ok bool) {
	mp, vf := o.m.measurer.Load(), o.m.wslVMPIDs.Load()
	if mp == nil || vf == nil {
		return nil, false
	}
	pids := (*vf)()
	if len(pids) > 0 {
		vm = sumVMVRAM((*mp)(pids), pids)
	}
	if o.vmReadingFailed(pids, vm) {
		return nil, false
	}
	o.vmVRAM = vm
	return vm, true
}

// endVMPeriodIfIdle ends the period of WSL activity once no wsl generation is
// live: the last good VM reading (owner.vmVRAM) is forgotten, and a
// measurement still out from that period will not record its reading
// (owner.vmPeriod). The fallback to the last good reading then spans only the
// current period. Kept across periods, an earlier period's figure would be
// charged as the remainder whenever the next period's reading fails -- above
// all a CPU-only wsl generation's first one, taken while its VM boots and has
// no worker yet (vmReadingFailed) -- although the booting VM holds nothing; a
// native candidate would be charged a whole earlier period's VM VRAM, and could
// evict an idle occupant or wait with nothing to wake it. Forgotten, such a
// failed reading falls back to nothing, which is what the booting VM holds.
// Called where a period can end: a wsl generation's exit, and an admission
// snapshot that finds nothing wsl live.
func (o *owner) endVMPeriodIfIdle() {
	if o.wslLive() {
		return
	}
	o.vmVRAM = nil
	o.vmPeriod++
}

// beginWSLVRAM sets up the VRAM accounting of proc, a wsl generation of spec
// that has just been exec'd. A spec with no GPU is attributed nothing at once:
// it holds no VRAM, so its exit never voids or credits a window. One with a GPU
// gets a window when its VRAM can be measured (window): baseline is the VM's
// figure read just before the exec, and a baseline that could not be read
// (!baselineOK) opens it void -- the next start still waits for it, but it
// closes without an attribution.
//
// Admission never lets a GPU wsl start through while a window is open
// (WSLStartInFlight), so there is none here; were there one, the new start
// would be growing the VM inside it, so that window is voided, and proc goes
// without a window of its own -- unmeasured, covered by the remainder.
func (o *owner) beginWSLVRAM(proc *runningProc, spec Spec, window bool, baseline map[int]int, baselineOK bool) {
	if !wslDeclaresGPU(spec) {
		if o.wslAttrib == nil {
			o.wslAttrib = make(map[*runningProc]map[int]int)
		}
		o.wslAttrib[proc] = nil
		return
	}
	if !window {
		return
	}
	if o.wslWindow != nil {
		o.wslWindow.void = true
		return
	}
	o.wslWindow = &wslStartWindow{proc: proc, baseline: baseline, void: !baselineOK}
}

// settleWSLWindow closes w with the measurement c, which was dispatched after
// w's process first passed a health probe. Unless w is void or c's VM reading
// is missing or unreadable, the VM's growth since the baseline, corrected by
// what exited processes took with them, on the GPUs the spec declares, is the
// process's attribution and its measured VRAM -- written back to the gateway
// as vram_measured_mb, a ready figure siblings inherit, and the resolution of
// its VRAM bootstrap, exactly as a native process's first measurement is.
// landed reports a recorded figure (the demand wake's trigger, as in
// applyMeasurement). The caller wakes the wsl starts that waited for the
// window.
//
// An unreadable reading leaves NO attribution entry, not an empty one: the
// process is then unattributed, so its exit during a later window voids that
// window instead of crediting it nothing.
func (o *owner) settleWSLWindow(w *wslStartWindow, c cmdMeasured) (landed bool) {
	o.wslWindow = nil
	st := o.specs[w.proc.specID]
	if w.void || !c.vmRead || st == nil || st.proc != w.proc {
		return false
	}
	after := sumVMVRAM(c.byPID, c.vmPIDs)
	// Nothing held after a baseline that held something is not this start's
	// growth: the workloads in the baseline cannot all have left without their
	// exits crediting the window, so the reading failed.
	if o.vmReadingFailed(c.vmPIDs, after) || (after == nil && len(w.baseline) > 0) {
		return false
	}
	attrib := onGPUs(wslDelta(after, w.baseline, w.exitedCredit), st.spec.GPUs)
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

// provisionalWSLAttribution is what the open window w's own process is taken to
// account for of the VM's figure vm while w is open, against the remainder
// only: per GPU it is charged on (charged, its RunningProc.GPUs), its growth so
// far -- the window's delta (wslDelta) from w's baseline and credit -- capped
// at that charge. nil for a void window, whose delta is known to be wrong.
//
// WHY. Until the window settles the process has no attribution, so it is
// charged its demand, while its growth is already in the VM's figure; left
// whole in the remainder, the same memory would be charged twice, and a native
// candidate on a shared GPU would wait, or evict, for room that exists. Capped
// at the charge, the figure keeps the invariant: the remainder is reduced by
// no more than the process is charged, so the sum admission sees on a GPU
// stays at least what the VM holds there. Whatever the process holds beyond
// its demand stays in the remainder until the settle attributes it.
func provisionalWSLAttribution(vm map[int]int, w *wslStartWindow, charged map[int]int) map[int]int {
	if w.void {
		return nil
	}
	var out map[int]int
	for g, mb := range wslDelta(vm, w.baseline, w.exitedCredit) {
		c, ok := charged[g]
		if !ok || c <= 0 {
			continue
		}
		if out == nil {
			out = make(map[int]int, len(charged))
		}
		out[g] = min(mb, c)
	}
	return out
}

// onGPUs keeps the entries of byGPU on the GPUs gpus names, nil when none is
// left.
func onGPUs(byGPU map[int]int, gpus []SpecGPU) map[int]int {
	var out map[int]int
	for _, g := range gpus {
		if mb, ok := byGPU[g.Index]; ok {
			if out == nil {
				out = make(map[int]int, len(gpus))
			}
			out[g.Index] = mb
		}
	}
	return out
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
//     is added back as credit (wslDelta). A CPU-only generation is attributed
//     nothing (beginWSLVRAM), so its exit adds nothing and voids nothing.
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

// wakeOnWSLSettle is the wake a window's close by its settling measurement
// causes (applyMeasurement). Nothing else announces that close -- no exit,
// release or sample -- and two kinds of spec may have waited for it:
//
//   - the wsl starts the window held back (wantsWSLStartWake), all of them,
//     including a force_running spec with no request, which every other event
//     wake leaves out (wantsWakeAfterStart). That one may only start into room
//     that is free now (admitAndStartWith, mayEvict false): the narrowing exists
//     because two force_running specs that cannot run together would evict
//     each other at load speed if a wake let them evict, and a re-ask that
//     cannot evict cannot do that. Without it, such a spec waiting behind a
//     window would stay down until an unrelated release, exit or Apply,
//     although nothing else stood in its way;
//   - any spec with a queued request or a pin (wantsWakeAfterStart), native
//     ones included: the settle changes what the window's process is charged.
//     An unknown demand becomes its attribution, a known occupant at last, so
//     a request that waited behind an unknown-demand occupant may fit now, and
//     nothing else would re-ask it while that occupant stays busy (or, with an
//     admission_wait_timeout of 0, ever). The narrowing is wakeAfterStart's, so
//     this wake, once per start, cannot cause an eviction storm.
//
// One pass, oldest request first; a spec in both sets is asked once, as a
// spec with a request or a pin is asked by any event wake.
func (o *owner) wakeOnWSLSettle() {
	o.wakeAdmissionWith(func(st *specState) bool {
		return wantsWSLStartWake(st) || wantsWakeAfterStart(st)
	}, func(id string) {
		if st := o.specs[id]; st != nil && !wantsWakeAfterStart(st) {
			o.reaskWith(id, false)
			return
		}
		o.reask(id)
	})
}

// wantsWSLStartWake selects, for wakeOnWSLSettle, a wsl spec whose last
// verdict was a Wait behind a window (specState.waitingOnWSLStart) and that
// still wants up (wantsWake).
func wantsWSLStartWake(st *specState) bool {
	return st.waitingOnWSLStart && wantsWake(st)
}
