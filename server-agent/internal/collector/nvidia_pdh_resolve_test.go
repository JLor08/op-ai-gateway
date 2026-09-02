// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"errors"
	"testing"
)

// This file covers resolvePDHLUIDs, the LUID -> GPU-index decision logic. It
// lived inside nvidia_pdh_windows.go until a review found a permanent
// negative-cache poisoning in it that CI structurally could not catch: the
// logic sat behind `//go:build windows`, which ubuntu-latest never compiles,
// vets or tests. It is now pure and tag-free, with its two external calls
// injected, exactly as this module's split rule requires.

// pdhResolveStub counts the external calls so the steady-state promise ("no
// syscall, no subprocess") is machine-checked rather than asserted in a
// comment, and so a fix for the poisoning cannot quietly pay for itself with a
// subprocess spawn on every cycle.
type pdhResolveStub struct {
	addrs     map[pdhLUID]pciAddress
	mapping   map[pciAddress]int
	complete  bool
	fetchFail bool

	d3dkmtCalls int
	fetchCalls  int
}

func (s *pdhResolveStub) adapterAddress(l pdhLUID) (pciAddress, error) {
	s.d3dkmtCalls++
	addr, ok := s.addrs[l]
	if !ok {
		return pciAddress{}, errors.New("D3DKMTOpenAdapterFromLuid: NTSTATUS 0xc000000d")
	}
	return addr, nil
}

func (s *pdhResolveStub) fetchPCIIndex() (map[pciAddress]int, bool) {
	s.fetchCalls++
	if s.fetchFail {
		return nil, false
	}
	return s.mapping, s.complete
}

var (
	pdhAddrGPU0 = pciAddress{Bus: 0x21}
	pdhAddrGPU1 = pciAddress{Bus: 0x49}
	pdhAddrGPU2 = pciAddress{Bus: 0x4a}
)

// TestResolvePDHLUIDsLearnsFromACompleteMapping is the happy path: a cold
// cache, a fresh and complete nvidia-smi reading, three adapters resolved and
// remembered.
func TestResolvePDHLUIDsLearnsFromACompleteMapping(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU0: pdhAddrGPU0, luidGPU1: pdhAddrGPU1, luidGPU2: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0, pdhAddrGPU1: 1, pdhAddrGPU2: 2},
		complete: true,
	}
	need := []pdhLUID{luidGPU0, luidGPU1, luidGPU2}

	out, caches := resolvePDHLUIDs(need, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex)

	if out[luidGPU0] != 0 || out[luidGPU1] != 1 || out[luidGPU2] != 2 {
		t.Fatalf("out = %v, want the three adapters at indexes 0/1/2", out)
	}
	if s.fetchCalls != 1 {
		t.Errorf("fetchPCIIndex called %d times, want exactly 1 per call", s.fetchCalls)
	}

	// Second cycle: the steady state. Nothing external may be touched.
	before := *s
	out, _ = resolvePDHLUIDs(need, caches, s.adapterAddress, s.fetchPCIIndex)
	if len(out) != 3 {
		t.Errorf("second cycle out = %v, want all three from cache", out)
	}
	if s.d3dkmtCalls != before.d3dkmtCalls || s.fetchCalls != before.fetchCalls {
		t.Errorf("steady state spent syscalls: d3dkmt %d->%d, fetch %d->%d",
			before.d3dkmtCalls, s.d3dkmtCalls, before.fetchCalls, s.fetchCalls)
	}
}

// TestResolvePDHLUIDsCachesAD3DKMTRefusal pins the measured fact the negative
// cache exists for: on the probe host LUID 0x0_0x16026 failed
// D3DKMTOpenAdapterFromLuid with STATUS_INVALID_PARAMETER and was retried once
// per counter instance. A refusal by the adapter itself is a durable property
// of that adapter, so it must be paid for exactly once.
func TestResolvePDHLUIDsCachesAD3DKMTRefusal(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU0: pdhAddrGPU0},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0},
		complete: true,
	}
	need := []pdhLUID{luidGPU0, luidUnresolved}

	out, caches := resolvePDHLUIDs(need, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex)
	if _, ok := out[luidUnresolved]; ok {
		t.Fatalf("out = %v, want no entry for the adapter D3DKMT refused", out)
	}
	if _, ok := caches.Unresolvable[luidUnresolved]; !ok {
		t.Fatalf("Unresolvable = %v, want the refused adapter cached", caches.Unresolvable)
	}

	calls := s.d3dkmtCalls
	if _, caches = resolvePDHLUIDs(need, caches, s.adapterAddress, s.fetchPCIIndex); s.d3dkmtCalls != calls {
		t.Errorf("d3dkmt calls %d -> %d: a cached refusal was retried", calls, s.d3dkmtCalls)
	}
}

// TestResolvePDHLUIDsCachesAnAddressACompleteMappingDoesNotKnow is the other
// legitimate negative conclusion: the adapter opened fine and reports a PCI
// address, but a FRESH and COMPLETE nvidia-smi reading has no GPU there. That
// is an integrated or software adapter -- a durable fact about the host, so it
// is cached and never paid for again.
func TestResolvePDHLUIDsCachesAnAddressACompleteMappingDoesNotKnow(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU0: pdhAddrGPU0, luidUnresolved: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0}, // the only NVIDIA GPU on the host
		complete: true,
	}
	need := []pdhLUID{luidGPU0, luidUnresolved}

	out, caches := resolvePDHLUIDs(need, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex)
	if _, ok := out[luidUnresolved]; ok {
		t.Fatalf("out = %v, want no entry for the non-NVIDIA adapter", out)
	}
	if _, ok := caches.Unresolvable[luidUnresolved]; !ok {
		t.Fatalf("Unresolvable = %v, want the non-NVIDIA adapter cached", caches.Unresolvable)
	}
	calls, fetches := s.d3dkmtCalls, s.fetchCalls
	if _, _ = resolvePDHLUIDs(need, caches, s.adapterAddress, s.fetchPCIIndex); s.d3dkmtCalls != calls || s.fetchCalls != fetches {
		t.Errorf("a settled negative conclusion was re-paid for: d3dkmt %d->%d, fetch %d->%d",
			calls, s.d3dkmtCalls, fetches, s.fetchCalls)
	}
}

// TestResolvePDHLUIDsSurvivesATransientFetchFailureOnAWarmCache is the review
// finding. A WARM cache (an earlier nvidia-smi fetch succeeded, so pciToIndex
// is non-empty) plus ONE failed refetch used to write the adapter into the
// negative half and never look at it again: the negative half is consulted
// BEFORE `unknown` is built, so a poisoned LUID could no longer trigger the
// refetch that is its only escape. That GPU's per-process VRAM was then never
// measured again for the life of the agent process, silently charging the
// operator's estimate instead -- recoverable only by a restart.
//
// The two conditions are correlated, not independent: a driver reset both
// renumbers adapters and is exactly when nvidia-smi is unavailable.
func TestResolvePDHLUIDsSurvivesATransientFetchFailureOnAWarmCache(t *testing.T) {
	s := &pdhResolveStub{
		addrs:     map[pdhLUID]pciAddress{luidGPU0: pdhAddrGPU0, luidGPU2: pdhAddrGPU2},
		mapping:   map[pciAddress]int{pdhAddrGPU0: 0, pdhAddrGPU2: 2},
		complete:  true,
		fetchFail: true, // nvidia-smi times out or exits non-zero this cycle
	}
	warm := pdhLUIDCaches{
		LUIDToIndex:  map[pdhLUID]int{luidGPU0: 0},
		Unresolvable: map[pdhLUID]struct{}{},
		PCIToIndex:   map[pciAddress]int{pdhAddrGPU0: 0}, // NON-EMPTY: the poisoning precondition
	}
	need := []pdhLUID{luidGPU0, luidGPU2}

	_, caches := resolvePDHLUIDs(need, warm, s.adapterAddress, s.fetchPCIIndex)
	if _, poisoned := caches.Unresolvable[luidGPU2]; poisoned {
		t.Fatalf("a transient nvidia-smi failure negative-cached a good adapter: Unresolvable = %v", caches.Unresolvable)
	}

	// nvidia-smi is healthy again on the next cycle: the adapter must resolve.
	s.fetchFail = false
	out, _ := resolvePDHLUIDs(need, caches, s.adapterAddress, s.fetchPCIIndex)
	if out[luidGPU2] != 2 {
		t.Fatalf("out = %v, want GPU 2 resolved once nvidia-smi recovered", out)
	}
}

// TestResolvePDHLUIDsSurvivesAPartialMapping is the same poisoning reached
// through a second door, and the one a "did nvidia-smi answer at all?" guard
// misses entirely: nvidia-smi DOES answer, but one GPU's pci.bus_id field is
// `[N/A]` (a card in ERR! state, or fallen off the bus), so the reading is
// non-empty yet incomplete. Concluding "no GPU sits at that address" from a
// reading that is missing rows is not a conclusion, and must not be cached as
// one.
func TestResolvePDHLUIDsSurvivesAPartialMapping(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU2: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0, pdhAddrGPU1: 1}, // GPU 2's row was [N/A]
		complete: false,
	}
	need := []pdhLUID{luidGPU2}

	out, caches := resolvePDHLUIDs(need, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex)
	if _, ok := out[luidGPU2]; ok {
		t.Fatalf("out = %v, want nothing resolved from an incomplete mapping", out)
	}
	if _, poisoned := caches.Unresolvable[luidGPU2]; poisoned {
		t.Fatalf("an INCOMPLETE mapping negative-cached a good adapter: Unresolvable = %v", caches.Unresolvable)
	}

	// nvidia-smi reports every row on a later cycle: the adapter must resolve.
	s.mapping, s.complete = map[pciAddress]int{pdhAddrGPU0: 0, pdhAddrGPU1: 1, pdhAddrGPU2: 2}, true
	out, _ = resolvePDHLUIDs(need, caches, s.adapterAddress, s.fetchPCIIndex)
	if out[luidGPU2] != 2 {
		t.Fatalf("out = %v, want GPU 2 resolved once the mapping was complete", out)
	}
}

// TestResolvePDHLUIDsResolvesFromAnIncompleteMappingThatDoesKnowTheAddress
// keeps the incompleteness rule narrow. Incompleteness withholds only the
// NEGATIVE conclusion; a row that did parse is still a fact, and a measurement
// it can attribute must not be thrown away because a SIBLING row was
// unreadable.
func TestResolvePDHLUIDsResolvesFromAnIncompleteMappingThatDoesKnowTheAddress(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU1: pdhAddrGPU1},
		mapping:  map[pciAddress]int{pdhAddrGPU1: 1}, // GPU 0's and GPU 2's rows were unreadable
		complete: false,
	}
	out, caches := resolvePDHLUIDs([]pdhLUID{luidGPU1}, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex)
	if out[luidGPU1] != 1 {
		t.Fatalf("out = %v, want GPU 1 resolved from the row that did parse", out)
	}
	if caches.LUIDToIndex[luidGPU1] != 1 {
		t.Errorf("LUIDToIndex = %v, want the resolution remembered", caches.LUIDToIndex)
	}
}

// TestResolvePDHLUIDsRefetchesAtMostOncePerCall pins the cost ceiling that
// makes this measurer runnable on the Manager's serialized owner goroutine:
// however many adapters miss the cached mapping, nvidia-smi is spawned once.
func TestResolvePDHLUIDsRefetchesAtMostOncePerCall(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU0: pdhAddrGPU0, luidGPU1: pdhAddrGPU1, luidGPU2: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0}, // GPU 1 and GPU 2 both miss
		complete: true,
	}
	if _, _ = resolvePDHLUIDs([]pdhLUID{luidGPU0, luidGPU1, luidGPU2}, pdhLUIDCaches{}, s.adapterAddress, s.fetchPCIIndex); s.fetchCalls != 1 {
		t.Errorf("fetchPCIIndex called %d times, want 1 -- one subprocess per call, whatever the miss count", s.fetchCalls)
	}
}

// TestResolvePDHLUIDsDiscardsTheNegativeHalfOnATopologyChange keeps the
// existing invalidation rule: a changed GPU topology is the only thing that
// can turn a previously unresolvable adapter into a real GPU, so a successful
// refetch drops the negative half it was derived from.
func TestResolvePDHLUIDsDiscardsTheNegativeHalfOnATopologyChange(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU1: pdhAddrGPU1, luidGPU2: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU1: 0, pdhAddrGPU2: 1}, // GPU 2 has appeared
		complete: true,
	}
	stale := pdhLUIDCaches{
		Unresolvable: map[pdhLUID]struct{}{luidGPU2: {}},
		// The reading predates the change: it knows only the card that has
		// since been renumbered away from bus 0x49.
		PCIToIndex: map[pciAddress]int{pdhAddrGPU0: 0},
	}
	// luidGPU1 is unknown and its address MISSES the stale reading, which is
	// what triggers the refetch; luidGPU2 must not still be shut out
	// afterwards by a negative half the superseded reading produced.
	_, caches := resolvePDHLUIDs([]pdhLUID{luidGPU1}, stale, s.adapterAddress, s.fetchPCIIndex)
	if _, ok := caches.Unresolvable[luidGPU2]; ok {
		t.Fatalf("Unresolvable = %v, want the stale negative half discarded by the refetch", caches.Unresolvable)
	}
}

// TestParseNvidiaPCIIndexCSVRefusesAnAmbiguousAddress covers the second review
// finding on this join key. parseNvidiaBusID discards the PCI DOMAIN, because
// D3DKMT_ADAPTERADDRESS reports no domain and so the join key cannot carry
// one. Two GPUs in different PCI segments therefore collapse onto the same
// bus:device.function key, and the last row silently won: every adapter
// D3DKMT reported at that address resolved to ONE of the two cards, chosen by
// CSV order. Since D3DKMT genuinely cannot tell the two apart there is no
// right answer, so the mapping must refuse to answer for that address at all
// -- the same "skip rather than guess an index" rule the rest of this module
// applies -- and must report itself INCOMPLETE so the address is not
// negative-cached as "no GPU here" either.
func TestParseNvidiaPCIIndexCSVRefusesAnAmbiguousAddress(t *testing.T) {
	got, complete := parseNvidiaPCIIndexCSV([]byte(
		"0, 00000000:01:00.0\n" +
			"1, 00010000:01:00.0\n" + // a DIFFERENT domain, the same bus:device.function
			"2, 00000000:4a:00.0\n"))

	if _, ok := got[pciAddress{Bus: 1}]; ok {
		t.Errorf("got[bus 1] = %d, present -- an ambiguous address must resolve to NO index", got[pciAddress{Bus: 1}])
	}
	if complete {
		t.Error("complete = true, want false -- an ambiguous address must not support a negative conclusion")
	}
	// The unambiguous row is untouched: one bad address costs one address.
	if idx, ok := got[pciAddress{Bus: 0x4a}]; !ok || idx != 2 {
		t.Errorf("got[bus 0x4a] = (%d, %v), want (2, true)", idx, ok)
	}
}

// TestParseNvidiaPCIIndexCSVRefusesAThriceRepeatedAddress proves the refusal
// is not merely a delete: a third row at the same address must not re-insert
// it, which a delete-on-duplicate without a memory of the collision would do.
func TestParseNvidiaPCIIndexCSVRefusesAThriceRepeatedAddress(t *testing.T) {
	got, complete := parseNvidiaPCIIndexCSV([]byte(
		"0, 00000000:01:00.0\n1, 00010000:01:00.0\n2, 00020000:01:00.0\n"))
	if len(got) != 0 {
		t.Errorf("got = %v, want empty -- the address stays refused however many rows claim it", got)
	}
	if complete {
		t.Error("complete = true, want false")
	}
}

// TestResolvePDHLUIDsKeepsTheNegativeHalfWhenTheRefetchIsIncomplete is the
// other half of the completeness rule, and the reason it gates the
// INVALIDATION as well as the conclusion. An incomplete reading is not
// evidence of a changed topology any more than it is evidence of an absent
// GPU, so it must not discard settled findings either. Without this, a host
// whose nvidia-smi permanently reports one row as `[N/A]` refetches every
// cycle (correctly -- see TestResolvePDHLUIDsSurvivesAPartialMapping) and each
// of those refetches would wipe the negative half, re-probing every
// D3DKMT-refused adapter forever.
func TestResolvePDHLUIDsKeepsTheNegativeHalfWhenTheRefetchIsIncomplete(t *testing.T) {
	s := &pdhResolveStub{
		addrs:    map[pdhLUID]pciAddress{luidGPU2: pdhAddrGPU2},
		mapping:  map[pciAddress]int{pdhAddrGPU0: 0}, // still missing GPU 2's row
		complete: false,
	}
	settled := pdhLUIDCaches{
		Unresolvable: map[pdhLUID]struct{}{luidUnresolved: {}}, // D3DKMT refused it
		PCIToIndex:   map[pciAddress]int{pdhAddrGPU1: 1},
	}
	_, caches := resolvePDHLUIDs([]pdhLUID{luidGPU2}, settled, s.adapterAddress, s.fetchPCIIndex)
	if s.fetchCalls != 1 {
		t.Fatalf("fetchPCIIndex called %d times, want 1 -- the refetch must still fire", s.fetchCalls)
	}
	if _, ok := caches.Unresolvable[luidUnresolved]; !ok {
		t.Errorf("Unresolvable = %v, want the D3DKMT refusal kept: an incomplete reading invalidates nothing", caches.Unresolvable)
	}
}
