// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
)

// This file is the PURE half of the Windows per-process VRAM measurer: the
// counter-instance name grammar, the pci.bus_id grammar, the LUID -> GPU-index
// resolution and its two caches, and the aggregation into the nested map
// runtime.Manager.SetMeasurer expects. It carries NO build tag on purpose,
// exactly like wmi_map.go next to hwinfo_windows.go: CI runs on ubuntu-latest
// and never builds, vets or tests GOOS=windows, so anything behind
// `//go:build windows` is verified by review alone. Everything that can be
// wrong about a number -- a swapped LUID field, a hex/decimal mix-up, a
// mis-summed multi-GPU model, a 0 escaping as if it were a measurement, an
// adapter written off as unmeasurable on no evidence -- lives here, where a
// unit test on Linux exercises the very code Windows runs. Only the syscalls
// themselves are in nvidia_pdh_windows.go.
//
// The last of those was not on the list originally. resolvePDHLUIDs started
// out inside the Windows file and permanently negative-cached good adapters
// after a single nvidia-smi timeout; review caught it, no test could have.
// The line to hold is that a DECISION belongs here even when it is reached
// only from Windows -- what the file is tagged for is the syscall, not the
// reasoning around it.
//
// Why this measurer exists at all: on Windows the WDDM driver model puts the
// OS, not the NVIDIA driver, in charge of GPU memory, so
// `nvidia-smi --query-compute-apps=...,used_memory` reports `[N/A]`. The chain
// used instead was proven end to end on a 3-GPU host (driver 610.62):
//
//  1. PDH counter `\GPU Process Memory(*)\Dedicated Usage`, whose instances are
//     named `pid_<PID>_luid_0x<HighPart>_0x<LowPart>_phys_<N>` -> bytes per PID
//     per adapter LUID.
//  2. `D3DKMTOpenAdapterFromLuid` + `D3DKMTQueryAdapterInfo` -> that adapter's
//     PCI address.
//  3. `nvidia-smi --query-gpu=index,pci.bus_id` -> the same PCI address -> the
//     GPU index specs and budgets are written in terms of.

// nvidiaPCIBusIDFields is the ordered --query-gpu field list the PDH measurer
// requests: the GPU index, and the PCI address that is the only thing a
// D3DKMT-resolved adapter and an nvidia-smi GPU have in common (compute-apps'
// UUID is unavailable from D3DKMT, and D3DKMT's LUID is unavailable from
// nvidia-smi). parseNvidiaPCIIndexCSV assumes exactly this column order.
// Deliberately its own constant, independent of nvidiaQueryFields and of the
// measurer-only nvidiaGPUIndexFields: three queries with three parsers, none of
// which may be changed in step with the others.
const nvidiaPCIBusIDFields = "index,pci.bus_id"

// bytesPerMB converts the PDH counter's BYTES to the MB the measurer contract
// (pid -> gpuIndex -> MB) and every spec's VRAMMB estimate are written in.
const bytesPerMB int64 = 1024 * 1024

// pdhLUID is a Windows display-adapter LUID as it appears in a `GPU Process
// Memory` counter instance name and in D3DKMT_OPENADAPTERFROMLUID: a locally
// unique id, stable for the life of a boot, that identifies one adapter. The
// field types mirror the C LUID (DWORD LowPart, LONG HighPart) so the Windows
// half can pass them straight through.
type pdhLUID struct {
	HighPart int32
	LowPart  uint32
}

// pdhProcessMemory is one `\GPU Process Memory(*)\Dedicated Usage` counter
// instance: how many bytes of dedicated VRAM one process holds on one physical
// segment of one adapter.
type pdhProcessMemory struct {
	PID  int
	LUID pdhLUID
	// Phys is the adapter's physical memory segment. It is parsed but never
	// interpreted: a process holds VRAM across several segments of the same
	// adapter, and the measurement wanted is the per-GPU total, so
	// attributePDHDedicated sums the segments away. Kept as a field because
	// dropping it would make two distinct instances look identical.
	Phys           int
	DedicatedBytes int64
}

// pdhInstanceRe is the counter-instance grammar. Case-insensitive because
// nothing documents the casing (Microsoft does not document the grammar at
// all -- it was read off a live host), and deliberately NOT anchored at the
// end: the probe run matched every instance on that machine with this
// expression, and a future Windows build that appends a further segment should
// still yield the PID and LUID it does report rather than nothing.
//
// The two hex fields are HighPart FIRST. That ordering is a measured fact, not
// a documented one, and it is the single mistake this file could make with no
// visible symptom: the swapped reading yields a LUID that D3DKMT simply refuses
// to open, which is indistinguishable from the perfectly normal case of an
// adapter with no NVIDIA GPU behind it, so the measurer would report nothing
// and look like "unsupported hardware" forever.
var pdhInstanceRe = regexp.MustCompile(`(?i)^pid_(\d+)_luid_0x([0-9a-f]+)_0x([0-9a-f]+)_phys_(\d+)`)

// parsePDHInstanceName parses one counter instance name. The returned struct's
// DedicatedBytes is left at 0 -- the counter VALUE arrives separately, out of
// the PDH item struct, and only the Windows half has it.
//
// ok is false for anything that does not match the grammar, including numbers
// too large for their type. Every unexpected shape is SKIPPED: a wrong PID
// would charge another process's VRAM to a managed model, and a wrong LUID
// would charge the wrong GPU, so guessing is strictly worse than reporting
// nothing and letting the manager fall back to the operator's estimate.
func parsePDHInstanceName(name string) (pdhProcessMemory, bool) {
	m := pdhInstanceRe.FindStringSubmatch(name)
	if m == nil {
		return pdhProcessMemory{}, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return pdhProcessMemory{}, false
	}
	high, err := strconv.ParseUint(m[2], 16, 32)
	if err != nil {
		return pdhProcessMemory{}, false
	}
	low, err := strconv.ParseUint(m[3], 16, 32)
	if err != nil {
		return pdhProcessMemory{}, false
	}
	phys, err := strconv.Atoi(m[4])
	if err != nil {
		return pdhProcessMemory{}, false
	}
	return pdhProcessMemory{
		PID:  pid,
		LUID: pdhLUID{HighPart: int32(high), LowPart: uint32(low)},
		Phys: phys,
	}, true
}

// pciAddress is a PCI bus location, the join key between an adapter LUID and an
// nvidia-smi GPU index. Deliberately a struct of plain numbers rather than a
// formatted string: nvidia-smi writes `00000000:21:00.0` in HEX while D3DKMT
// reports the same location as three decimal ints, so comparing the two as text
// would silently never match (bus 0x21 vs bus 33). The PCI domain is not part
// of it -- D3DKMT_ADAPTERADDRESS does not report one.
type pciAddress struct {
	Bus      uint32
	Device   uint32
	Function uint32
}

// parseNvidiaBusID parses nvidia-smi's pci.bus_id
// (`<domain>:<bus>:<device>.<function>`, hex) into the comparable form. The
// domain is accepted and discarded; a bus_id without one parses too. ok is
// false for `[N/A]`, an empty field, or any other shape -- an address that
// cannot be parsed simply never joins to an adapter, which costs a measurement,
// never a wrong one.
func parseNvidiaBusID(s string) (pciAddress, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 {
		return pciAddress{}, false
	}
	devFn := strings.Split(parts[len(parts)-1], ".")
	if len(devFn) != 2 {
		return pciAddress{}, false
	}
	bus, err := strconv.ParseUint(parts[len(parts)-2], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	dev, err := strconv.ParseUint(devFn[0], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	fn, err := strconv.ParseUint(devFn[1], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	return pciAddress{Bus: uint32(bus), Device: uint32(dev), Function: uint32(fn)}, true
}

// parseNvidiaPCIIndexCSV parses `nvidia-smi --query-gpu=index,pci.bus_id
// --format=csv,noheader,nounits` output into a PCI-address -> GPU-index map.
// Rows with fewer than 2 fields, or an unparseable bus_id, are skipped --
// parseNvidiaCSV's own tolerance for a malformed line. Reuses
// splitNvidiaCSVRows and naInt (nvidia.go) so this module keeps exactly one
// nvidia-smi CSV reader.
//
// pci.bus_id is safe to carry through a comma-split: its form is
// `00000000:21:00.0` -- colons and a dot, never a comma.
//
// AN AMBIGUOUS ADDRESS RESOLVES TO NOTHING. parseNvidiaBusID discards the PCI
// DOMAIN, and it has to: D3DKMT_ADAPTERADDRESS reports no domain, so the join
// key cannot carry one (see pciAddress). Two GPUs in different PCI segments
// therefore land on the same bus:device.function key. Letting the last row win
// would make the join key non-unique and attribute every adapter D3DKMT
// reports at that address to whichever card happened to come last in the CSV
// -- a confident wrong GPU index, which this module's whole error strategy
// rejects (see attributePDHDedicated's "skip rather than guess an index").
// D3DKMT genuinely cannot tell the two cards apart, so there is no right
// answer and the honest one is to refuse: the address is removed and stays
// removed, both adapters fall through to the operator's estimate, and no
// measurement is misfiled.
//
// The second return value reports whether this reading is COMPLETE: every row
// parsed, and no address claimed twice. It is NOT a health signal for the
// caller to log -- it is the licence to draw a PERMANENT negative conclusion.
// resolvePDHLUIDs may cache "no NVIDIA GPU sits at that address" only from a
// complete reading, because an address missing from a reading that is itself
// missing rows says nothing at all. A row that DID parse stays usable either
// way: incompleteness withholds the negative conclusion, never a positive one.
func parseNvidiaPCIIndexCSV(data []byte) (map[pciAddress]int, bool) {
	out := make(map[pciAddress]int)
	// ambiguous remembers refused addresses so a THIRD row at the same
	// address cannot re-insert one that a second row already removed.
	var ambiguous map[pciAddress]bool
	complete := true
	for _, row := range splitNvidiaCSVRows(data) {
		if len(row) < 2 {
			complete = false
			continue
		}
		addr, ok := parseNvidiaBusID(row[1])
		if !ok {
			complete = false // e.g. `[N/A]` from a card in ERR! state
			continue
		}
		if ambiguous[addr] {
			continue
		}
		if _, dup := out[addr]; dup {
			if ambiguous == nil {
				ambiguous = make(map[pciAddress]bool)
			}
			ambiguous[addr] = true
			delete(out, addr)
			complete = false
			continue
		}
		out[addr] = naInt(row[0])
	}
	return out, complete
}

// --- What a failed adapter probe means -----------------------------------
//
// luidPCIAddress (nvidia_pdh_windows.go) fails in three distinguishable ways,
// and the negative cache must treat them differently: one is a verdict about
// the ADAPTER and may be remembered for the life of the process, the others
// are the ABSENCE of an answer and may not. So the probe's errors are TYPED
// rather than fmt.Errorf strings: the classification is then a switch on a
// value, in the file CI compiles and tests, instead of a guess about a
// message written on the side CI never sees.

// d3dkmtEntry names which of the two D3DKMT calls the probe makes produced an
// NTSTATUS. The classification turns on it, because the same status means
// different things from the two calls -- see mayCacheUnresolvable.
type d3dkmtEntry uint8

const (
	// entryOpenAdapter is D3DKMTOpenAdapterFromLuid: "give me a handle for
	// this LUID". Its verdict is about the adapter's IDENTITY.
	entryOpenAdapter d3dkmtEntry = iota
	// entryQueryAdapterAddress is
	// D3DKMTQueryAdapterInfo(KMTQAITYPE_ADAPTERADDRESS), made on a handle the
	// open call has already produced. Its verdict is about the QUERY, and this
	// module's own enum literal and mirror struct are among the things that
	// can make it fail.
	entryQueryAdapterAddress
)

func (e d3dkmtEntry) String() string {
	if e == entryOpenAdapter {
		return "D3DKMTOpenAdapterFromLuid"
	}
	return "D3DKMTQueryAdapterInfo(KMTQAITYPE_ADAPTERADDRESS)"
}

// The NTSTATUS values this module names. Only the first is a verdict; the
// other two are the shapes of "no answer right now" that the classification
// exists to keep OUT of the negative cache, and they are named so the tests
// can pin exactly that (production reads them nowhere -- the rule is an
// allowlist, so a status is excluded by not being in it).
const (
	statusInvalidParameter uint32 = 0xc000000d // STATUS_INVALID_PARAMETER
	statusDeviceRemoved    uint32 = 0xc00002b6 // STATUS_DEVICE_REMOVED
	statusNoMemory         uint32 = 0xc0000017 // STATUS_NO_MEMORY
)

// d3dkmtStatusError is a D3DKMT call that returned a failing NTSTATUS.
type d3dkmtStatusError struct {
	Entry  d3dkmtEntry
	Status uint32
}

func (e *d3dkmtStatusError) Error() string {
	return fmt.Sprintf("%s: NTSTATUS 0x%08x", e.Entry, e.Status)
}

// implausiblePCIAddressError is luidPCIAddress's plausibility gate refusing an
// address outside the PCI ranges (bus 0-255, device 0-31, function 0-7). The
// call SUCCEEDED and the reading is repeatable; it is the reading that cannot
// be used.
type implausiblePCIAddressError struct {
	Addr pciAddress
}

func (e *implausiblePCIAddressError) Error() string {
	return fmt.Sprintf("implausible PCI address %d:%d.%d from D3DKMT",
		e.Addr.Bus, e.Addr.Device, e.Addr.Function)
}

// mayCacheUnresolvable reports whether err from the adapter probe licenses a
// PERMANENT negative conclusion about that adapter -- an entry in
// pdhLUIDCaches.Unresolvable, which is consulted before the unknown set is
// built and so can never be revisited except by a topology change.
//
// IT IS AN ALLOWLIST, and deliberately a two-entry one, because the two
// mistakes cost wildly different amounts. Failing to remember a durable
// verdict costs three syscalls per measurement cycle on an adapter that will
// never resolve -- the measured waste this cache was added for. Remembering a
// TRANSIENT failure costs a working GPU its measurement for the life of the
// agent process, silently: attributePDHDedicated omits the (pid, gpu) pair,
// buildSnapshot charges the operator's estimate instead, and nothing above
// debug level says so. So a status nobody has classified is retried, never
// written off.
//
// WHAT QUALIFIES.
//
//   - STATUS_INVALID_PARAMETER from D3DKMTOpenAdapterFromLuid: the answer the
//     operator's own hardware gave for LUID 0x0_0x16026. The LUID is
//     well-formed and the call is the same one that succeeds for the three
//     real GPUs on that host, so the refusal is about that adapter and
//     nothing else. It cannot change while the LUID exists either: a LUID is
//     minted per adapter per boot, and a driver restart that gives a card a
//     new identity gives it a new LUID -- a different cache key.
//   - An implausible PCI address: the query SUCCEEDED, so this is a completed
//     reading rather than a missing one, and repeating a deterministic read of
//     the same struct from the same adapter cannot produce a different answer.
//     Whatever the cause -- a wrong enum literal, a wrong struct layout, an
//     adapter reporting nonsense -- there is no usable address here until the
//     code itself changes, which means a new process.
//
// WHAT DOES NOT, AND WHY EACH IS THE ABSENCE OF AN ANSWER.
//
//   - STATUS_DEVICE_REMOVED (0xc00002b6) after a TDR or a driver reset, and
//     STATUS_NO_MEMORY (0xc0000017) or STATUS_INSUFFICIENT_RESOURCES
//     (0xc000009a) under momentary pressure. Each describes the MOMENT, not
//     the adapter, and the first is correlated with exactly the topology
//     change that produced an unknown LUID in the first place.
//   - Any status from the ADDRESS query, STATUS_INVALID_PARAMETER included.
//     That call is made on a handle just opened successfully, so the adapter
//     has already answered for its identity; what can be wrong instead is
//     THIS MODULE -- the KMTQUERYADAPTERINFOTYPE literal, or the mirror
//     struct's layout -- which is a defect affecting every adapter equally
//     and must not be recorded as a property of one. The compile-time layout
//     assertions in nvidia_pdh_windows.go are what guard that half; a
//     negative cache entry would only hide it.
//   - STATUS_NOT_SUPPORTED / STATUS_NOT_IMPLEMENTED, even though "this
//     adapter has no PCI address to report" is a plausible reading of them.
//     Nothing has observed either here, and an unobserved durable verdict is
//     the one guess this cache cannot afford; the cost of leaving them out is
//     the bounded one above.
//
// An error of any other shape is likewise not a verdict: there is no error
// this module produces that means "not this adapter" without saying so in one
// of the two types above.
func mayCacheUnresolvable(err error) bool {
	var st *d3dkmtStatusError
	if errors.As(err, &st) {
		return st.Entry == entryOpenAdapter && st.Status == statusInvalidParameter
	}
	var bad *implausiblePCIAddressError
	return errors.As(err, &bad)
}

// pdhLUIDCaches is the adapter-LUID -> GPU-index bridge the Windows measurer
// carries across measurement cycles: both halves of the LUID cache, plus the
// nvidia-smi PCI mapping they were derived from. Passed and returned by value
// and never mutated in place -- the measurer swaps whole maps under its mutex,
// so an overlapping buildSnapshot and dispatchMeasurement can at worst lose an
// update and repeat one resolution next cycle.
type pdhLUIDCaches struct {
	// LUIDToIndex is the POSITIVE half: adapters that resolved to an
	// nvidia-smi GPU index. nil until the first successful resolution.
	LUIDToIndex map[pdhLUID]int
	// Unresolvable is the NEGATIVE half: adapters that will never resolve.
	// Membership IS the answer, so the values are never read. Only two
	// findings may enter it, and both are durable facts rather than the
	// absence of an answer -- see resolvePDHLUIDs.
	Unresolvable map[pdhLUID]struct{}
	// PCIToIndex is the last nvidia-smi reading. nil until the first
	// successful fetch.
	PCIToIndex map[pciAddress]int
}

// resolvePDHLUIDs maps each needed adapter LUID to its nvidia-smi GPU index,
// consulting and extending both halves of the cache, and returns the caches to
// install. adapterAddress is the D3DKMT lookup and fetchPCIIndex the nvidia-smi
// spawn; both are parameters so this decision logic is testable on Linux CI.
//
// It lived inside nvidia_pdh_windows.go and was moved here after review: a
// permanent negative-cache poisoning had gone unnoticed in it precisely
// because `//go:build windows` code is never compiled, vetted or tested by CI
// (ubuntu-latest), so review was its only guard. Everything here is a
// decision; only the two calls it makes are platform-specific.
//
// WHEN AN ADAPTER MAY BE WRITTEN OFF PERMANENTLY. The negative half must hold
// only durable facts, because it is consulted BEFORE the unknown set is built:
// a LUID in it can no longer trigger the refetch that is its only escape, so a
// wrong entry is wrong until the agent process restarts, and its cost is
// silent -- attributePDHDedicated omits the (pid, gpu) pair, buildSnapshot
// charges the operator's estimate instead, and nothing above debug level says
// so. Exactly two findings qualify:
//
//   - THE ADAPTER PROBE RETURNED A VERDICT ABOUT THE ADAPTER. D3DKMT refusing
//     to open the LUID at all, or an address the adapter reports that no PCI
//     bus could hold. The first is the measured reason this cache exists: on
//     the probe host LUID 0x0_0x16026 failed D3DKMTOpenAdapterFromLuid with
//     STATUS_INVALID_PARAMETER and was retried once per counter instance --
//     six wasted syscalls per cycle on the Manager's serialized owner
//     goroutine for an answer that cannot change. Which probe failures
//     qualify, and why every other one does not, is mayCacheUnresolvable.
//   - A FRESH AND COMPLETE nvidia-smi READING HAS NO GPU AT THE ADAPTER'S PCI
//     ADDRESS. A property of the host's topology: an integrated GPU, or a
//     software/render adapter.
//
// Everything else is the ABSENCE of an answer, not a negative one, and is
// retried on the next cycle. Both halves of that had to be fixed here, in
// turn, and each was a permanently lost measurement:
//
//   - nvidia-smi failing, timing out, or answering with rows missing (see
//     parseNvidiaPCIIndexCSV). The earlier guard asked only whether
//     nvidia-smi had EVER answered, so on a warm cache a single 2s timeout
//     (routine while a driver is reinitialising, and correlated with the very
//     topology change that produced the unknown LUID) wrote a perfectly good
//     GPU off for good.
//   - A TRANSIENT adapter-probe failure -- a TDR returning
//     STATUS_DEVICE_REMOVED, momentary pressure returning STATUS_NO_MEMORY,
//     or any failure of the address query. This branch used to write EVERY
//     error from adapterAddress into the negative half, on a comment
//     asserting it was a refusal, while both this file's own rule and ADR-031
//     said only a refusal qualifies.
//
// The retry is bounded: at most one nvidia-smi spawn per call however many
// adapters miss, and none at all in the steady state where every needed
// adapter is already in one half of the cache.
//
// The three steps are pdhSplitNeeded (what the caches already answer), the
// per-adapter loop below, and pdhRefetchPCI (the one nvidia-smi spawn and the
// licence it grants).
func resolvePDHLUIDs(
	need []pdhLUID,
	in pdhLUIDCaches,
	adapterAddress func(pdhLUID) (pciAddress, error),
	fetchPCIIndex func() (map[pciAddress]int, bool),
) (map[pdhLUID]int, pdhLUIDCaches) {
	out, unknown := pdhSplitNeeded(need, in)
	if len(unknown) == 0 {
		return out, in // the steady state: no syscall, no subprocess, no cache write
	}

	next := pdhLUIDCaches{
		LUIDToIndex:  make(map[pdhLUID]int, len(in.LUIDToIndex)+len(unknown)),
		Unresolvable: make(map[pdhLUID]struct{}, len(in.Unresolvable)+len(unknown)),
		PCIToIndex:   in.PCIToIndex,
	}
	maps.Copy(next.LUIDToIndex, in.LUIDToIndex)
	maps.Copy(next.Unresolvable, in.Unresolvable)

	// trustNegative is the licence described above: true only once THIS call
	// has obtained a fresh, complete reading. Deliberately not derived from
	// the cached reading -- a miss always attempts a refetch first, so by the
	// time a negative conclusion is on the table the freshest obtainable
	// reading is already in hand.
	trustNegative := false
	refetched := false
	for _, l := range unknown {
		addr, err := adapterAddress(l)
		switch {
		case err != nil && mayCacheUnresolvable(err):
			// A verdict about the ADAPTER, and the only kind of probe failure
			// that may be remembered.
			next.Unresolvable[l] = struct{}{}
			continue
		case err != nil:
			// Every other probe failure is the ABSENCE of an answer and is
			// left out of BOTH halves, so the next cycle asks again -- which
			// is the whole distinction mayCacheUnresolvable exists to draw,
			// and which this branch used to ignore by caching every error
			// alike.
			continue
		}
		idx, ok := next.PCIToIndex[addr]
		if !ok && !refetched {
			refetched = true
			next, trustNegative = pdhRefetchPCI(next, len(unknown), fetchPCIIndex)
			idx, ok = next.PCIToIndex[addr]
		}
		switch {
		case ok:
			next.LUIDToIndex[l] = idx
			out[l] = idx
		case trustNegative:
			// A FRESH AND COMPLETE reading with no GPU at this adapter's PCI
			// address: a property of the host's topology -- an integrated GPU,
			// or a software/render adapter -- and the second of the two
			// findings that may be remembered permanently.
			next.Unresolvable[l] = struct{}{}
		}
		// Neither: no answer was obtained this cycle, so the adapter enters
		// no half of the cache and is asked about again next cycle.
	}
	return out, next
}

// pdhSplitNeeded answers as much of need as the caches already can, and
// reports the adapters that must be probed.
//
// An empty unknown set is THE STEADY STATE this cache exists for, and the
// caller's whole reason to return early: no syscall, no subprocess, no cache
// write.
//
// The negative half is consulted HERE, before the unknown set is built, and
// that is exactly why only durable facts may enter it: a LUID in it can no
// longer reach the refetch that is its only escape. See resolvePDHLUIDs for
// which two findings qualify.
func pdhSplitNeeded(need []pdhLUID, in pdhLUIDCaches) (resolved map[pdhLUID]int, unknown []pdhLUID) {
	resolved = make(map[pdhLUID]int, len(need))
	for _, l := range need {
		if idx, ok := in.LUIDToIndex[l]; ok {
			resolved[l] = idx
			continue
		}
		if _, ok := in.Unresolvable[l]; ok {
			continue // a durable finding; do not pay for it again
		}
		unknown = append(unknown, l)
	}
	return resolved, unknown
}

// pdhRefetchPCI re-reads the nvidia-smi PCI mapping into the caches and
// reports whether the reading it obtained licenses a NEGATIVE conclusion.
//
// THE ONE CASE A STALE READING ACTUALLY MATTERS is an adapter at an address
// the cached reading does not know -- a GPU added, removed or reindexed since
// the last fetch (a driver reset, for instance). The caller pays for this at
// most once per call, which is the same invalidation rule
// nvidiaComputeAppsMeasurer applies to its uuidToIndex cache.
//
// An EMPTY reading changes nothing -- not the mapping, not the negative half,
// not the licence -- because an nvidia-smi that answered nothing is the
// absence of an answer, and the caches in hand are still the best available.
//
// COMPLETENESS decides two separate things, and both are gated on it for the
// same reason: a reading missing rows supersedes nothing.
//
//   - The returned licence. An address missing from a reading that is itself
//     missing rows says nothing at all about whether a GPU sits there.
//   - Whether the negative half is DISCARDED. A changed topology is the only
//     thing that can turn a previously unresolvable adapter into a real GPU,
//     so a superseding reading drops the conclusions derived from the reading
//     before it; anything still unresolvable is re-learned next cycle, at the
//     cost of one round of syscalls. Ungated, a host whose nvidia-smi
//     permanently reports one row as `[N/A]` would refetch every cycle
//     (correctly) and wipe the negative half every cycle (pointlessly),
//     re-probing every D3DKMT-refused adapter for the life of the process.
//
// The caches are taken and returned BY VALUE, and the maps inside them are
// replaced rather than mutated -- pdhLUIDCaches' own contract, which is what
// lets the measurer swap whole maps under its mutex.
func pdhRefetchPCI(caches pdhLUIDCaches, unknownCount int, fetchPCIIndex func() (map[pciAddress]int, bool)) (pdhLUIDCaches, bool) {
	fresh, complete := fetchPCIIndex()
	if len(fresh) == 0 {
		return caches, false
	}
	caches.PCIToIndex = fresh
	if complete {
		caches.Unresolvable = make(map[pdhLUID]struct{}, unknownCount)
	}
	return caches, complete
}

// attributePDHDedicated is the aggregation the measurer contract needs: filter
// the counter instances to the manager's own PIDs, resolve each instance's
// adapter LUID to a GPU index, sum the adapter's physical segments, and convert
// bytes to MB. The result is map[pid]map[gpuIndex]MB; nil means nothing was
// measured.
//
// A standalone pure function, following attributeComputeApps (nvidia.go) for
// the same reason: the headline tests call the SAME code the production path
// runs, instead of re-implementing the filter/sum/convert loop in the test body
// where a real bug -- wrong filter direction, a dropped GPU index, segments not
// summed, bytes reported as MB -- would leave them green regardless.
//
// THE RULE THIS FUNCTION EXISTS TO ENFORCE: a (pid, gpu) whose summed usage is
// 0 MB is DROPPED, not reported as 0. runtime/manager.go buildSnapshot reads
// `if v, ok := byGPU[g.Index]; ok`, so a present key is authoritative and a
// measured 0 overrides the operator's VRAM estimate -- the GPU budget then
// looks entirely free and co-residency admission loses the OOM protection it
// exists for. That is the live bug on Windows today (nvidia-smi's `[N/A]` ->
// naInt -> 0 -> a non-nil map of zeros), and this measurer must not reproduce
// it in a new shape. An absent key means "not measured", which falls back to
// the estimate.
//
// Falling back is the BETTER direction, not a safe one, and the difference
// matters. An operator-entered estimate is generally larger than reality --
// people round up -- so charging it usually over-books the GPU rather than
// under-booking it. But `0` is the documented default for an estimate and
// means "unknown", and a running occupant charged 0 is indistinguishable in
// Admit's rule-3 sum from one that genuinely needs 0 MB (policy.go: `sum +=
// v`). Rule 4's "unknown VRAM starts alone" only fires when the unknown spec
// is the CANDIDATE. So on a host where no measurement has yet round-tripped
// through the gateway, admission can still under-count -- which is a reason to
// keep this function honest about what it does not know, not a reason to let
// it emit a 0 that would make the same hole permanent.
// The two halves are pdhSumDedicatedBytes and pdhBytesToMB, and the order is
// the point: SUM IN BYTES FIRST, then convert once per (pid, gpu). Converting
// each instance would round every physical segment down separately and lose up
// to a MB per segment.
func attributePDHDedicated(instances []pdhProcessMemory, luidToIndex map[pdhLUID]int, pids []int) map[int]map[int]int {
	return pdhBytesToMB(pdhSumDedicatedBytes(instances, luidToIndex, pids))
}

// pdhSumDedicatedBytes is attributePDHDedicated's first half: filter the
// counter instances to the manager's own PIDs, resolve each instance's adapter
// LUID to a GPU index, and sum the adapter's physical segments -- still in
// BYTES, because rounding is the caller's job and doing it here would round
// every segment down separately.
//
// Three shapes are skipped, and each would otherwise charge VRAM to the wrong
// place: an instance belonging to a process the manager does not own, a
// non-positive byte count (nothing to add, and a nonsense negative must not
// REDUCE a real segment), and an adapter that resolved to no NVIDIA GPU. That
// last one is normal and was observed on the probe host -- a software/render
// adapter, an iGPU, or a stale counter instance -- and skipping beats guessing
// an index, index 0 being the guess a zero value would make.
func pdhSumDedicatedBytes(instances []pdhProcessMemory, luidToIndex map[pdhLUID]int, pids []int) map[int]map[int]int64 {
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}

	sums := make(map[int]map[int]int64)
	for _, in := range instances {
		if !wanted[in.PID] || in.DedicatedBytes <= 0 {
			continue
		}
		idx, ok := luidToIndex[in.LUID]
		if !ok {
			continue
		}
		if sums[in.PID] == nil {
			sums[in.PID] = make(map[int]int64)
		}
		sums[in.PID][idx] += in.DedicatedBytes
	}
	return sums
}

// pdhBytesToMB is attributePDHDedicated's second half: the byte-to-MB
// conversion and the drop rule, per process.
//
// A process left with NO measurable card contributes no key of its own, and
// nothing measurable at all returns nil rather than an empty map. That is the
// same rule pdhCardsToMB applies one level down, for the same reason: an empty
// map[gpuIndex]MB under a present pid key would tell buildSnapshot the process
// was measured and found to hold nothing.
func pdhBytesToMB(sums map[int]map[int]int64) map[int]map[int]int {
	var out map[int]map[int]int
	for pid, byGPU := range sums {
		byIndex := pdhCardsToMB(byGPU)
		if len(byIndex) == 0 {
			continue
		}
		if out == nil {
			out = make(map[int]map[int]int, len(sums))
		}
		out[pid] = byIndex
	}
	return out
}

// pdhCardsToMB converts ONE process's per-card byte sums to MB, and is where
// THE RULE stated on attributePDHDedicated is actually enforced: a card whose
// summed usage rounds to 0 MB is DROPPED, never reported as 0.
//
// runtime/manager.go buildSnapshot reads `if v, ok := byGPU[g.Index]; ok`, so
// a present key is authoritative and a measured 0 overrides the operator's
// VRAM estimate -- the GPU budget then looks entirely free and co-residency
// admission loses the OOM protection it exists for. An absent key means "not
// measured", which falls back to the estimate.
func pdhCardsToMB(byGPU map[int]int64) map[int]int {
	var out map[int]int
	for idx, bytes := range byGPU {
		mb := int(bytes / bytesPerMB)
		if mb <= 0 {
			continue
		}
		if out == nil {
			out = make(map[int]int, len(byGPU))
		}
		out[idx] = mb
	}
	return out
}
