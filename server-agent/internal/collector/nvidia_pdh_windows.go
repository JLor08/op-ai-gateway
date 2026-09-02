// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file is the SYSCALL half of the Windows per-process VRAM measurer. Its
// pure counterpart -- every grammar and all the arithmetic -- is nvidia_pdh.go,
// which carries no build tag precisely because CI (ubuntu-latest) never
// compiles, vets or tests this file. Everything here is either a Win32 call or
// a cache around one, and it is reviewed rather than tested.
//
// Two Win32 facilities, in sequence, both proven on the operator's 3-GPU host:
//
//   - PDH (pdh.dll) reads the `\GPU Process Memory(*)\Dedicated Usage` counter,
//     whose instance names carry the PID and the adapter LUID. This is what
//     Task Manager's per-process GPU memory column shows, and unlike
//     nvidia-smi's per-process query it works under WDDM.
//   - D3DKMT (gdi32.dll exports, user-mode callable) turns an adapter LUID into
//     a PCI address, which is the only identifier an adapter and an nvidia-smi
//     GPU have in common.
//
// No PowerShell, no typeperf, no WMI: the measurer runs on the runtime
// Manager's owner goroutine during an admission decision (buildSnapshot), where
// spawning a shell would stall every other lifecycle operation for hundreds of
// milliseconds. The single subprocess it does spawn, nvidia-smi, is cached and
// bounded by nvidiaMeasureTimeout.

// BOTH DLLs LOAD FROM SYSTEM32 ONLY, via windows.NewLazySystemDLL rather than
// syscall.NewLazyDLL. This is a security boundary, not a style preference.
// syscall.NewLazyDLL with a bare base name resolves through LoadLibraryExW's
// standard search order, which starts with the APPLICATION DIRECTORY, and it
// only bypasses that for the handful of DLLs Go itself registers as
// System32-only (internal/syscall/windows/sysdll: advapi32, bcryptprimitives,
// crypt32, dnsapi, iphlpapi, kernel32, mswsock, netapi32, ntdll, psapi,
// secur32, shell32, userenv, ws2_32). pdh.dll is not among them and is not a
// Windows KnownDLL either, so a file named pdh.dll dropped next to the agent
// binary would be loaded and its DllMain executed IN PROCESS -- inside the
// process that spawns the model children and holds the gateway mTLS client
// identity -- on the first VRAM measurement after a managed model starts. The
// agent ships as a single downloadable binary an operator installs wherever
// they like (07-deployment-view.md 7.5), so "the install directory is not
// writable" is not an assumption this code may make. Go's own syscall.LoadDLL
// doc names both the hazard and this fix.
//
// gdi32.dll IS a KnownDLL and so is not hijackable, but it goes through the
// same constructor anyway: one rule for both is one fewer thing to get right,
// and it keeps pdhMeasurerProcs a single type. golang.org/x/sys was already in
// the module graph.
var (
	pdhDLL                           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = pdhDLL.NewProc("PdhCloseQuery")

	gdi32DLL                      = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTOpenAdapterFromLuid = gdi32DLL.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo    = gdi32DLL.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter        = gdi32DLL.NewProc("D3DKMTCloseAdapter")
)

// pdhMeasurerProcs is every entry point the measurer needs. The constructor
// resolves all of them up front so a host missing any one of them gets NO
// measurer rather than a measurer that panics on its first call --
// a LazyProc resolves lazily and panics from Call when the export is absent,
// which on this code path would take down an admission decision.
var pdhMeasurerProcs = []*windows.LazyProc{
	procPdhOpenQueryW,
	procPdhAddEnglishCounterW,
	procPdhCollectQueryData,
	procPdhGetFormattedCounterArrayW,
	procPdhCloseQuery,
	procD3DKMTOpenAdapterFromLuid,
	procD3DKMTQueryAdapterInfo,
	procD3DKMTCloseAdapter,
}

const (
	// pdhFmtLarge is PDH_FMT_LARGE: format the counter as a LONGLONG. The
	// counter is a byte count, so the int64 reading is the exact value; the
	// double reading would be a lossy detour.
	pdhFmtLarge = 0x00000400
	// pdhMoreData is PDH_MORE_DATA, the status the sizing call is EXPECTED to
	// return (buffer too small). Anything else from that call means the array
	// could not be sized and there is nothing to read.
	pdhMoreData = 0x800007D2
	// kmtqaiTypeAdapterAddress is KMTQAITYPE_ADAPTERADDRESS, the 7th member of
	// the KMTQUERYADAPTERINFOTYPE enum, which declares no explicit values --
	// hence the literal 6, verified against a real adapter (the probe rejects
	// an implausible PCI address, which is what a wrong enum value produces).
	kmtqaiTypeAdapterAddress = 6
	// pdhDedicatedUsagePath is the wildcard counter path. ENGLISH, added via
	// PdhAddEnglishCounterW: performance-object and counter names are
	// LOCALIZED on Windows, so the localized-name API would need the German
	// (or Japanese, or ...) spelling of both halves. The English API resolves
	// the same counter on any locale and was confirmed to work directly on the
	// probe host.
	//
	// Only "Dedicated Usage" is read. "Shared Usage" and "Non Local Usage"
	// looked tempting as spillover detection, but on the probe host they
	// reported the SAME 4694 MiB on all three GPUs: they are not per-adapter
	// figures and would be worse than no number at all.
	pdhDedicatedUsagePath = `\GPU Process Memory(*)\Dedicated Usage`
)

// pdhFmtCounterValueItemW mirrors PDH_FMT_COUNTERVALUE_ITEM_W:
// LPWSTR szName (8) + PDH_FMT_COUNTERVALUE{DWORD CStatus (4), pad (4),
// union (8)} = 24 bytes. The union is read as its LONGLONG member because the
// array is requested with PDH_FMT_LARGE.
//
// CStatus is declared (it is part of the layout) but deliberately not
// consulted. The probe ignored it and still agreed with nvidia-smi on 15 of 15
// PIDs to within 0.8%, so gating on it would be an unverified guess about which
// status codes accompany a usable value -- and an item whose value really is
// unusable reads 0, which attributePDHDedicated drops anyway.
type pdhFmtCounterValueItemW struct {
	SzName     *uint16
	CStatus    uint32
	_          uint32
	LargeValue int64
}

// d3dkmtOpenAdapterFromLuid mirrors D3DKMT_OPENADAPTERFROMLUID:
// LUID{DWORD LowPart, LONG HighPart} then D3DKMT_HANDLE hAdapter (an out
// parameter the kernel fills in).
type d3dkmtOpenAdapterFromLuid struct {
	LowPart  uint32
	HighPart int32
	HAdapter uint32
}

// d3dkmtQueryAdapterInfo mirrors D3DKMT_QUERYADAPTERINFO: the pointer lands at
// offset 8 by natural alignment, matching the C layout. The trailing blank
// field documents the tail padding C adds after PrivateDataSize; Go's own
// alignment rules would add it anyway (the struct's alignment is the pointer's),
// so it is a note to the reader, not a fix -- removing it changes no offset and
// trips no assertion below.
//
// PrivateData is typed unsafe.Pointer rather than uintptr on purpose: the
// kernel writes the query result THROUGH it, so the buffer must stay reachable
// for the garbage collector across the call. A uintptr would hide it.
type d3dkmtQueryAdapterInfo struct {
	HAdapter        uint32
	Type            uint32
	PrivateData     unsafe.Pointer
	PrivateDataSize uint32
	_               uint32
}

// d3dkmtAdapterAddress mirrors D3DKMT_ADAPTERADDRESS: the adapter's PCI
// location. There is no domain member -- see pciAddress.
type d3dkmtAdapterAddress struct {
	Bus      uint32
	Device   uint32
	Function uint32
}

// Compile-time layout assertions, one bracketing PAIR per fact.
//
// Why they exist: a wrong size or offset here would not crash and would not
// fail a test. It would hand the kernel a misaligned buffer and read back a
// garbage PCI address, which then matches no nvidia-smi GPU, so the measurer
// silently reports nothing and the whole approach looks like "this hardware is
// not supported". CI never compiles this file, and no Linux test can reach it,
// so the BUILD is the only place such a mistake can be caught -- and
// `GOOS=windows go build ./...` catches it on any machine, including the
// developer's and the reviewer's.
//
// How they work: `uint(x - N)` is a compile error when x < N, and
// `uint(N - x)` is a compile error when x > N, because a negative untyped
// constant cannot convert to an unsigned type. Both together pin the value to
// exactly N. Every number below is transcribed from the C declaration in
// d3dkmthk.h / pdh.h.
//
// Each struct pins its total size, then every NAMED field's offset AND width,
// with no exceptions -- a review found four fields (this struct's Bus and
// Device, and QUERYADAPTERINFO's HAdapter and Type) that had no width
// assertion, which is what prompted spelling the rule out as "no exceptions"
// rather than "every field".
//
// The widths are not redundant, and the gap was not theoretical: narrowing any
// of those four to a uint16 leaves the total size and every other offset
// intact -- alignment absorbs the two bytes -- so `GOOS=windows go build ./...`
// accepted a struct that reads half of a number the kernel wrote. HAdapter is
// the one with teeth: a truncated D3DKMT handle makes D3DKMTQueryAdapterInfo
// fail for every adapter, so every LUID caches as unresolvable and the whole
// measurer degrades to the silent "this hardware is not supported" outcome
// this block exists to make impossible. Only offsets plus widths together pin
// the reads.
//
// The first field's offset is asserted too, even though Go guarantees it is 0.
// It costs one line, and a blanket "every named field, both facts" is a rule a
// reader can check by counting; "every field except the ones where it is
// implied" is a rule that invites the next omission.
//
// A blank padding field cannot be asserted at all (it has no name to take the
// offset of), which is why the two structs that carry one pin their total size
// instead: the size is what the padding moves.
//
// The 8-byte pointer this assumes holds for both platforms the agent ships a
// Windows binary for, amd64 and arm64. A 32-bit Windows target would fail
// these assertions rather than mis-read the structs -- which is the intended
// outcome for a platform nobody has verified.
const (
	_ = uint(unsafe.Sizeof(pdhFmtCounterValueItemW{}) - 24)
	_ = uint(24 - unsafe.Sizeof(pdhFmtCounterValueItemW{}))
	_ = uint(unsafe.Offsetof(pdhFmtCounterValueItemW{}.SzName) - 0)
	_ = uint(0 - unsafe.Offsetof(pdhFmtCounterValueItemW{}.SzName))
	_ = uint(unsafe.Sizeof(pdhFmtCounterValueItemW{}.SzName) - 8)
	_ = uint(8 - unsafe.Sizeof(pdhFmtCounterValueItemW{}.SzName))
	_ = uint(unsafe.Offsetof(pdhFmtCounterValueItemW{}.CStatus) - 8)
	_ = uint(8 - unsafe.Offsetof(pdhFmtCounterValueItemW{}.CStatus))
	_ = uint(unsafe.Sizeof(pdhFmtCounterValueItemW{}.CStatus) - 4)
	_ = uint(4 - unsafe.Sizeof(pdhFmtCounterValueItemW{}.CStatus))
	_ = uint(unsafe.Offsetof(pdhFmtCounterValueItemW{}.LargeValue) - 16)
	_ = uint(16 - unsafe.Offsetof(pdhFmtCounterValueItemW{}.LargeValue))
	_ = uint(unsafe.Sizeof(pdhFmtCounterValueItemW{}.LargeValue) - 8)
	_ = uint(8 - unsafe.Sizeof(pdhFmtCounterValueItemW{}.LargeValue))

	_ = uint(unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}) - 12)
	_ = uint(12 - unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}))
	_ = uint(unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.LowPart) - 0)
	_ = uint(0 - unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.LowPart))
	_ = uint(unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.LowPart) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.LowPart))
	_ = uint(unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.HighPart) - 4)
	_ = uint(4 - unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.HighPart))
	_ = uint(unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.HighPart) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.HighPart))
	_ = uint(unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.HAdapter) - 8)
	_ = uint(8 - unsafe.Offsetof(d3dkmtOpenAdapterFromLuid{}.HAdapter))
	_ = uint(unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.HAdapter) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtOpenAdapterFromLuid{}.HAdapter))

	_ = uint(unsafe.Sizeof(d3dkmtQueryAdapterInfo{}) - 24)
	_ = uint(24 - unsafe.Sizeof(d3dkmtQueryAdapterInfo{}))
	_ = uint(unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.HAdapter) - 0)
	_ = uint(0 - unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.HAdapter))
	_ = uint(unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.HAdapter) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.HAdapter))
	_ = uint(unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.Type) - 4)
	_ = uint(4 - unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.Type))
	_ = uint(unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.Type) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.Type))
	_ = uint(unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.PrivateData) - 8)
	_ = uint(8 - unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.PrivateData))
	_ = uint(unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.PrivateData) - 8)
	_ = uint(8 - unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.PrivateData))
	_ = uint(unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.PrivateDataSize) - 16)
	_ = uint(16 - unsafe.Offsetof(d3dkmtQueryAdapterInfo{}.PrivateDataSize))
	_ = uint(unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.PrivateDataSize) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtQueryAdapterInfo{}.PrivateDataSize))

	_ = uint(unsafe.Sizeof(d3dkmtAdapterAddress{}) - 12)
	_ = uint(12 - unsafe.Sizeof(d3dkmtAdapterAddress{}))
	_ = uint(unsafe.Offsetof(d3dkmtAdapterAddress{}.Bus) - 0)
	_ = uint(0 - unsafe.Offsetof(d3dkmtAdapterAddress{}.Bus))
	_ = uint(unsafe.Sizeof(d3dkmtAdapterAddress{}.Bus) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtAdapterAddress{}.Bus))
	_ = uint(unsafe.Offsetof(d3dkmtAdapterAddress{}.Device) - 4)
	_ = uint(4 - unsafe.Offsetof(d3dkmtAdapterAddress{}.Device))
	_ = uint(unsafe.Sizeof(d3dkmtAdapterAddress{}.Device) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtAdapterAddress{}.Device))
	_ = uint(unsafe.Offsetof(d3dkmtAdapterAddress{}.Function) - 8)
	_ = uint(8 - unsafe.Offsetof(d3dkmtAdapterAddress{}.Function))
	_ = uint(unsafe.Sizeof(d3dkmtAdapterAddress{}.Function) - 4)
	_ = uint(4 - unsafe.Sizeof(d3dkmtAdapterAddress{}.Function))
)

// newNvidiaPDHMeasurer returns the Windows per-process, per-GPU VRAM measurer
// for runtime.Manager.SetMeasurer, or nil when this host cannot support it.
//
// nil is the same answer NewNvidiaComputeApps gives on a host without
// nvidia-smi, for the same reason: measurement is a HARDWARE capability, not a
// negotiated protocol feature (design doc §5). No measurer means the manager
// keeps using each spec's operator-entered VRAM estimate, exactly as it does
// today.
//
// What is checked here: nvidia-smi on PATH (without it a LUID can never be
// turned into a GPU index, so a measurement could never be attributed), and
// every pdh.dll/gdi32.dll export the measurer calls.
//
// What is deliberately NOT checked here: whether the `GPU Process Memory`
// performance object itself exists. Both available probes for that are wrong at
// startup. Enumerating performance objects returns LOCALIZED names, so
// searching for the English string would reject every non-English Windows --
// the exact trap PdhAddEnglishCounterW exists to avoid. And adding the counter
// needs at least one live instance, which a freshly started agent with no model
// running does not have, so a boot-time attempt would permanently disable
// measurement on a perfectly capable host. Instead, a failed counter add is
// handled per cycle: measure returns nil, the manager falls back to estimates
// for that cycle, and the next cycle tries again. The cost of the retry is one
// failed API call, with no subprocess.
func newNvidiaPDHMeasurer() func(pids []int) map[int]map[int]int {
	if _, err := exec.LookPath(nvidiaSMI); err != nil {
		return nil
	}
	for _, p := range pdhMeasurerProcs {
		if err := p.Find(); err != nil {
			slog.Debug("windows vram measurer unavailable", "proc", p.Name, "err", err)
			return nil
		}
	}
	m := &nvidiaPDHMeasurer{}
	return m.measure
}

// nvidiaPDHMeasurer carries the LUID -> GPU-index bridge across measurement
// cycles. The bridge costs three D3DKMT syscalls plus (once) an nvidia-smi
// spawn per adapter, and it describes the host's fixed GPU topology, which
// does not change between one cycle and the next -- so it is cached, modelled
// on nvidiaComputeAppsMeasurer's uuidToIndex cache. Both halves are cached,
// including the negative one; the rules for what may enter it are with
// resolvePDHLUIDs, in the build-tag-free nvidia_pdh.go, because CI can test
// them there and cannot here.
//
// This struct's only job is the mutex. The caches are copy-on-write: whole
// maps in, whole maps out, never mutated in place, because SetMeasurer's
// contract allows two overlapping calls (buildSnapshot on the owner goroutine
// and the recurring dispatchMeasurement off it) and the worst a lost update
// can cost is one repeated resolution on the next cycle.
type nvidiaPDHMeasurer struct {
	mu     sync.Mutex
	cached pdhLUIDCaches
}

func (m *nvidiaPDHMeasurer) caches() pdhLUIDCaches {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cached
}

func (m *nvidiaPDHMeasurer) setCaches(c pdhLUIDCaches) {
	m.mu.Lock()
	m.cached = c
	m.mu.Unlock()
}

// measure is the func(pids []int) map[int]map[int]int shape
// runtime.Manager.SetMeasurer expects: pids is the manager's own live
// managed-process set, and the result is pid -> gpuIndex -> MB.
//
// nil at every failure, and that is the whole error strategy. buildSnapshot
// already treats a nil map as "nothing measured this cycle" and falls back to
// each spec's static estimate, so a PDH hiccup degrades to the behaviour of a
// host with no measurer at all -- never to a wrong number, and never to a 0
// (see attributePDHDedicated, which drops those). Failures are logged at debug
// level, matching the LHM collectors: a measurement that cannot be taken is not
// an operator-visible fault.
//
// In the steady state this is ONE PDH read and no subprocess at all: every
// adapter is already in one half of the cache.
func (m *nvidiaPDHMeasurer) measure(pids []int) map[int]map[int]int {
	if len(pids) == 0 {
		return nil
	}

	instances, err := readPDHDedicatedUsage()
	if err != nil {
		slog.Debug("pdh gpu process memory read failed", "err", err)
		return nil
	}

	// Resolve only the adapters the WANTED pids actually hold memory on. Every
	// GPU process on the host produces counter instances -- the desktop
	// compositor, a browser, another user's session -- and resolving their
	// adapters would spend syscalls on numbers attributePDHDedicated then
	// discards.
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}
	var need []pdhLUID
	seen := make(map[pdhLUID]bool)
	for _, in := range instances {
		if !wanted[in.PID] || in.DedicatedBytes <= 0 || seen[in.LUID] {
			continue
		}
		seen[in.LUID] = true
		need = append(need, in.LUID)
	}
	if len(need) == 0 {
		return nil // no managed process holds dedicated VRAM anywhere
	}

	luidToIndex := m.resolveLUIDs(need)
	if len(luidToIndex) == 0 {
		return nil
	}
	return attributePDHDedicated(instances, luidToIndex, pids)
}

// resolveLUIDs maps each needed adapter LUID to its nvidia-smi GPU index via
// resolvePDHLUIDs, supplying the two platform-specific calls that logic needs
// and installing the caches it returns. The decision logic itself is
// deliberately NOT here: it is pure, it is where a wrong negative conclusion
// silently costs a GPU its measurement for the life of the process, and this
// file is the one CI never compiles, vets or tests.
func (m *nvidiaPDHMeasurer) resolveLUIDs(need []pdhLUID) map[pdhLUID]int {
	out, next := resolvePDHLUIDs(need, m.caches(), luidPCIAddressLogged, nvidiaPCIIndex)
	m.setCaches(next)
	return out
}

// luidPCIAddressLogged is luidPCIAddress plus the debug line resolvePDHLUIDs
// cannot emit (it takes no logger, and a pure function should not). An error
// here is normal, not a fault: an adapter with no NVIDIA GPU behind it, or a
// counter instance left over from a process that has exited.
func luidPCIAddressLogged(l pdhLUID) (pciAddress, error) {
	addr, err := luidPCIAddress(l)
	if err != nil {
		slog.Debug("d3dkmt adapter address lookup failed",
			"luid_high", l.HighPart, "luid_low", l.LowPart, "err", err)
	}
	return addr, err
}

// nvidiaPCIIndex fetches the PCI-address -> GPU-index mapping from nvidia-smi.
// (nil, false) on any failure, which resolvePDHLUIDs treats as "learn nothing
// this cycle" -- pointedly NOT as "no GPU sits at that address", which is the
// conclusion the second return value licenses and a failed spawn cannot
// support.
//
// Bounded by nvidiaMeasureTimeout, the same knob the compute-apps measurer
// uses, and for the same load-bearing reason: this runs on the Manager's
// serialized owner goroutine during an admission, so a wedged nvidia-smi would
// stall every other lifecycle operation, not just this measurement. One spawn
// per cache miss, none in the steady state.
func nvidiaPCIIndex() (map[pciAddress]int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaMeasureTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, nvidiaSMI,
		"--query-gpu="+nvidiaPCIBusIDFields,
		nvidiaCSVFormat,
	).Output()
	if err != nil {
		slog.Debug("nvidia-smi pci.bus_id query failed", "err", err)
		return nil, false
	}
	return parseNvidiaPCIIndexCSV(out)
}

// readPDHDedicatedUsage collects one sample of `\GPU Process Memory(*)\
// Dedicated Usage` and returns every instance whose name matches the expected
// grammar, with its byte count attached.
//
// ONE PdhCollectQueryData, deliberately. Whether this counter is a raw gauge
// (one collection suffices) or behaves like a rate counter (needing two
// collections and therefore a sleep between them) is not documented; the probe
// answered it on hardware -- a single collection already returned non-zero
// values for every GPU process. A second collection plus a settling delay
// would cost hundreds of milliseconds on the owner goroutine for nothing.
func readPDHDedicatedUsage() ([]pdhProcessMemory, error) {
	path, err := syscall.UTF16PtrFromString(pdhDedicatedUsagePath)
	if err != nil {
		return nil, err
	}

	var hQuery uintptr
	if st, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&hQuery))); st != 0 {
		return nil, fmt.Errorf("PdhOpenQueryW: 0x%08x", uint32(st))
	}
	defer procPdhCloseQuery.Call(hQuery) //nolint:errcheck // status ignored: nothing to do about a failed close

	var hCounter uintptr
	if st, _, _ := procPdhAddEnglishCounterW.Call(hQuery,
		uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&hCounter))); st != 0 {
		// Expected on a Windows build without the GPU counters, and on any
		// host with no GPU process running at all -- see newNvidiaPDHMeasurer
		// on why this is a per-cycle condition rather than a startup gate.
		return nil, fmt.Errorf("PdhAddEnglishCounterW(%s): 0x%08x", pdhDedicatedUsagePath, uint32(st))
	}
	if st, _, _ := procPdhCollectQueryData.Call(hQuery); st != 0 {
		return nil, fmt.Errorf("PdhCollectQueryData: 0x%08x", uint32(st))
	}

	// Two-call sizing, the standard PDH pattern: the first call reports the
	// buffer it needs and MUST return PDH_MORE_DATA.
	var size, count uint32
	st, _, _ := procPdhGetFormattedCounterArrayW.Call(hCounter, pdhFmtLarge,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(st) != pdhMoreData {
		return nil, fmt.Errorf("PdhGetFormattedCounterArrayW sizing: 0x%08x, want PDH_MORE_DATA", uint32(st))
	}
	if size == 0 || count == 0 {
		return nil, nil // no instances: no GPU process on the host
	}
	item := unsafe.Sizeof(pdhFmtCounterValueItemW{})
	if uintptr(size) < uintptr(count)*item {
		// PDH's own two numbers disagree. Reading count items out of a
		// size-byte buffer would be an out-of-bounds read, so refuse instead.
		return nil, fmt.Errorf("PdhGetFormattedCounterArrayW: %d items do not fit in %d bytes", count, size)
	}

	buf := make([]byte, size)
	if st, _, _ := procPdhGetFormattedCounterArrayW.Call(hCounter, pdhFmtLarge,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&buf[0]))); st != 0 {
		return nil, fmt.Errorf("PdhGetFormattedCounterArrayW: 0x%08x", uint32(st))
	}

	items := unsafe.Slice((*pdhFmtCounterValueItemW)(unsafe.Pointer(&buf[0])), int(count))
	out := make([]pdhProcessMemory, 0, len(items))
	for i := range items {
		in, ok := parsePDHInstanceName(utf16PtrToString(items[i].SzName))
		if !ok {
			continue
		}
		in.DedicatedBytes = items[i].LargeValue
		// Instances are appended, never deduplicated: PDH can report the same
		// instance name twice, and attributePDHDedicated sums by (pid,
		// adapter) anyway, which is the correct reading of a split instance.
		out = append(out, in)
	}
	// Every item's szName points INTO buf, and PDH writes those pointers, so
	// the garbage collector cannot see the reference. Keep buf alive until the
	// last name has been copied out.
	runtime.KeepAlive(buf)
	return out, nil
}

// luidPCIAddress resolves one adapter LUID to its PCI address via the gdi32
// D3DKMT entry points. An error means "no usable address for this adapter" and
// is the caller's cue to cache the LUID as unresolvable; it is not a fault.
func luidPCIAddress(l pdhLUID) (pciAddress, error) {
	open := d3dkmtOpenAdapterFromLuid{LowPart: l.LowPart, HighPart: l.HighPart}
	if st, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); st != 0 {
		// STATUS_INVALID_PARAMETER (0xC000000D) here is the normal answer for
		// an adapter that is not a real GPU; it was seen once on the probe
		// host, which is why the caller caches the refusal.
		return pciAddress{}, fmt.Errorf("D3DKMTOpenAdapterFromLuid: NTSTATUS 0x%08x", uint32(st))
	}
	defer func() {
		// D3DKMT_CLOSEADAPTER is a bare D3DKMT_HANDLE. Every adapter opened
		// MUST be closed: the handles are per-process kernel objects and a
		// measurement runs on every housekeeping beat, so leaking one per
		// adapter per cycle would grow without bound for the agent's lifetime.
		h := open.HAdapter
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&h))) //nolint:errcheck // status ignored: nothing to do about a failed close
	}()

	var addr d3dkmtAdapterAddress
	q := d3dkmtQueryAdapterInfo{
		HAdapter:        open.HAdapter,
		Type:            kmtqaiTypeAdapterAddress,
		PrivateData:     unsafe.Pointer(&addr),
		PrivateDataSize: uint32(unsafe.Sizeof(addr)),
	}
	if st, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); st != 0 {
		return pciAddress{}, fmt.Errorf("D3DKMTQueryAdapterInfo(ADAPTERADDRESS): NTSTATUS 0x%08x", uint32(st))
	}

	// Field by field, not `pciAddress(addr)`. The two structs happen to be
	// convertible today, and staticcheck's S1016 suggests exactly that, but a
	// Win32 mirror struct and this module's join key are different things that
	// must be free to diverge: pciAddress gaining a Domain field (nvidia-smi
	// does report one; parseNvidiaBusID discards it) would turn an identity
	// conversion into a compile error here for no reason.
	p := pciAddress{Bus: addr.Bus, Device: addr.Device, Function: addr.Function} //nolint:staticcheck // S1016: see above -- deliberate, and CI does not lint for Windows anyway
	// Plausibility gate: PCI allows bus 0-255, device 0-31, function 0-7. A
	// value outside that says the struct layout or the enum value is wrong,
	// and a confident-looking wrong address is worse than no address -- it
	// could collide with another GPU's entry in the nvidia-smi mapping and
	// charge a model's VRAM to the wrong GPU. The compile-time assertions
	// above are the first line of defence; this is the second, at runtime.
	if p.Bus > 255 || p.Device > 31 || p.Function > 7 {
		return pciAddress{}, fmt.Errorf("implausible PCI address %d:%d.%d from D3DKMT", p.Bus, p.Device, p.Function)
	}
	return p, nil
}

// utf16PtrToString copies a NUL-terminated UTF-16 string out of memory Windows
// owns (here: a counter instance name inside the PDH array buffer). The length
// is unknown up front, so it is walked one code unit at a time; syscall's own
// helpers all need a bounded slice.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for q := unsafe.Pointer(p); *(*uint16)(q) != 0; n++ {
		q = unsafe.Add(q, 2)
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
