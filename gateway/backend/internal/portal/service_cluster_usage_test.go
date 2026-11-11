// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// headDTO reads the head's runtime spec as the owner sees it.
func (f *clusterFixture) headDTO() RuntimeSpecDTO {
	f.t.Helper()
	dto, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		f.t.Fatalf("GetRuntimeSpec: %v", err)
	}
	return dto
}

// TestClusterTotalsLowerBound pins the cluster totals on the head: every part
// is max(measured, estimate), an unknown part adds 0 and marks its axis (and
// the totals as a whole) a lower bound, and a measurement that arrives makes
// the total complete again.
func TestClusterTotalsLowerBound(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	unknownGPU := autoMember(b.ID, 0)
	unknownGPU.GPUs[0].VRAMEstimateMB = 0
	unknownGPU.RAMEstimateMB = 1500
	req := clusterHeadRequest(autoMember(a.ID, 0), unknownGPU)
	req.RAMEstimateMB = 1000
	head := f.mustPut(req)
	f.usage(head.ID, a.ID, 2000, map[int]int{0: 9000})

	got := f.headDTO()
	if got.ClusterVRAMTotalMB != 9000 || got.ClusterVRAMTotalComplete {
		t.Fatalf("vram = %d complete %v, want 9000 as a lower bound (member B's GPU is unknown)", got.ClusterVRAMTotalMB, got.ClusterVRAMTotalComplete)
	}
	if got.ClusterRAMTotalMB != 4500 || !got.ClusterRAMTotalComplete {
		t.Fatalf("ram = %d complete %v, want 1000 + 2000 + 1500, complete", got.ClusterRAMTotalMB, got.ClusterRAMTotalComplete)
	}
	if got.ClusterTotalsComplete {
		t.Fatal("totals complete with an unknown part")
	}

	f.usage(head.ID, b.ID, 500, map[int]int{0: 6000})
	got = f.headDTO()
	if got.ClusterVRAMTotalMB != 15000 || got.ClusterRAMTotalMB != 4500 || !got.ClusterTotalsComplete ||
		!got.ClusterVRAMTotalComplete || !got.ClusterRAMTotalComplete {
		t.Fatalf("after B measured: vram %d ram %d complete %v/%v/%v, want 15000/4500 complete",
			got.ClusterVRAMTotalMB, got.ClusterRAMTotalMB, got.ClusterTotalsComplete, got.ClusterVRAMTotalComplete, got.ClusterRAMTotalComplete)
	}
}

// TestCPUMemberRAMOnly pins a CPU member's part of the totals: RAM only. It
// adds nothing to VRAM, and an unknown RAM part makes the RAM total a lower
// bound but never the VRAM total.
func TestCPUMemberRAMOnly(t *testing.T) {
	f := newClusterFixture(t)
	a, c := f.member("A"), f.member("C")
	gpuMember := autoMember(a.ID, 0)
	gpuMember.RAMEstimateMB = 500
	req := clusterHeadRequest(gpuMember, autoMember(c.ID))
	req.RAMEstimateMB = 1000
	head := f.mustPut(req)

	got := f.headDTO()
	if got.ClusterVRAMTotalMB != 8000 || !got.ClusterVRAMTotalComplete {
		t.Fatalf("vram = %d complete %v, want 8000 complete: the CPU member has no VRAM part", got.ClusterVRAMTotalMB, got.ClusterVRAMTotalComplete)
	}
	if got.ClusterRAMTotalMB != 1500 || got.ClusterRAMTotalComplete || got.ClusterTotalsComplete {
		t.Fatalf("ram = %d complete %v totals %v, want 1500 as a lower bound", got.ClusterRAMTotalMB, got.ClusterRAMTotalComplete, got.ClusterTotalsComplete)
	}

	f.usage(head.ID, c.ID, 3000, nil)
	got = f.headDTO()
	if got.ClusterVRAMTotalMB != 8000 || got.ClusterRAMTotalMB != 4500 || !got.ClusterTotalsComplete {
		t.Fatalf("after the CPU member measured: vram %d ram %d complete %v, want 8000/4500 complete",
			got.ClusterVRAMTotalMB, got.ClusterRAMTotalMB, got.ClusterTotalsComplete)
	}
}

// TestClusterStatusFlagsPeakAboveBudget pins the cluster status naming a
// stored member peak above what the member host admits: a GPU's stored VRAM
// above its operator budget, a CPU member's stored RAM above the host's
// memory. A stale value is held at that ceiling as demand anyway, so it is
// not named; a member the reader may not see shows its position only.
func TestClusterStatusFlagsPeakAboveBudget(t *testing.T) {
	f := newClusterFixture(t)
	a, c := f.member("A"), f.member("C")
	foreign := f.foreignServer("Foreign")
	f.hardware(a.ID, `{"os":"linux","gpus":[{"index":0,"name":"NVIDIA RTX A6000","pci_bus_id":"0:1","memory_total_bytes":51539607552}]}`)
	if err := f.routes.SetServerGPUBudgets(f.ctx, a.ID, []routing.ServerGPUBudget{{ServerID: a.ID, GPUIndex: 0, BudgetMB: 20000}}); err != nil {
		t.Fatal(err)
	}
	f.hardware(c.ID, `{"os":"linux","memory":{"total_bytes":17179869184}}`)
	f.hardware(foreign.ID, `{"os":"linux","memory":{"total_bytes":17179869184}}`)
	req := clusterHeadRequest(autoMember(a.ID, 0), autoMember(c.ID), autoMember(foreign.ID))
	req.ClusterMembers[2].RAMEstimateMB = 1000
	head, err := f.put(systemToken(), req)
	if err != nil {
		t.Fatalf("system save: %v", err)
	}
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 24000})
	f.usage(head.ID, c.ID, 20000, nil)
	f.usage(head.ID, foreign.ID, 30000, nil)

	gpu0 := 0
	want := []ClusterPeakAboveBudgetDTO{
		{Position: 0, Resource: ClusterPeakResourceVRAM, GPUIndex: &gpu0, StoredMB: 24000, CeilingMB: 20000},
		{Position: 1, Resource: ClusterPeakResourceRAM, StoredMB: 20000, CeilingMB: 16384},
		{Position: 2},
	}
	got := f.headDTO()
	if got.ClusterStatus == nil || !peaksEqual(got.ClusterStatus.PeaksAboveBudget, want) {
		t.Fatalf("owner peaks = %+v, want %+v", got.ClusterStatus, want)
	}
	sys, err := f.svc.GetRuntimeSpec(f.ctx, systemToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	if p := sys.ClusterStatus.PeaksAboveBudget; len(p) != 3 || p[2].Resource != ClusterPeakResourceRAM || p[2].StoredMB != 30000 {
		t.Fatalf("system peaks = %+v, want the foreign member's RAM named", p)
	}

	// Within the ceilings, nothing is named; a stale value above them is not
	// either.
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 19000})
	members, _ := membersByServer(t, f, head.ID)
	stale := members[c.ID]
	stale.Stale = true
	if _, err := f.routes.UpdateClusterMemberUsage(f.ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	f.usage(head.ID, foreign.ID, 900, nil)
	if p := f.headDTO().ClusterStatus.PeaksAboveBudget; len(p) != 0 {
		t.Fatalf("peaks = %+v, want none", p)
	}
}

func peaksEqual(a, b []ClusterPeakAboveBudgetDTO) bool {
	return slices.EqualFunc(a, b, func(x, y ClusterPeakAboveBudgetDTO) bool {
		sameIndex := (x.GPUIndex == nil) == (y.GPUIndex == nil) && (x.GPUIndex == nil || *x.GPUIndex == *y.GPUIndex)
		return sameIndex && x.Position == y.Position && x.Resource == y.Resource && x.StoredMB == y.StoredMB && x.CeilingMB == y.CeilingMB
	})
}

// TestAuthorizeClusterMemberUsageReset pins who may reset a member's stored
// usage: management rights on the head (through its mapping) and on the
// member's server. A server the caller may not manage answers server not
// found, a mapping it may not manage mapping not found, and a server that is
// no member of the head the member's own not-found code.
func TestAuthorizeClusterMemberUsageReset(t *testing.T) {
	f := newClusterFixture(t)
	mine := f.member("Mine")
	other := f.member("Other")
	foreign := f.foreignServer("Foreign")
	head, err := f.put(systemToken(), clusterHeadRequest(autoMember(mine.ID), autoMember(foreign.ID)))
	if err != nil {
		t.Fatalf("system save: %v", err)
	}

	m, err := f.svc.AuthorizeClusterMemberUsageReset(f.ctx, ownerToken(), f.mappingID, mine.ID)
	if err != nil || m.HeadSpecID != head.ID || m.ServerID != mine.ID {
		t.Fatalf("own member = %+v err=%v", m, err)
	}
	if _, err := f.svc.AuthorizeClusterMemberUsageReset(f.ctx, ownerToken(), f.mappingID, foreign.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("foreign member err = %v, want server not found", err)
	}
	if _, err := f.svc.AuthorizeClusterMemberUsageReset(f.ctx, systemToken(), f.mappingID, foreign.ID); err != nil {
		t.Fatalf("system on the foreign member: %v", err)
	}
	if _, err := f.svc.AuthorizeClusterMemberUsageReset(f.ctx, ownerToken(), f.mappingID, other.ID); !errors.Is(err, ErrClusterMemberNotFound) {
		t.Fatalf("no member err = %v, want the member's not-found", err)
	}
	if _, err := f.svc.AuthorizeClusterMemberUsageReset(f.ctx, otherToken(), f.mappingID, mine.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("foreign head err = %v, want mapping not found", err)
	}
}

// TestDistributionChangeEndsTheRun pins the portal's half of the member usage
// peak rule: a run that began before a change of the distribution must never
// replace the stored values or clear the stale mark, so the change that marks
// the kept values stale also resets every member's run (its start and peaks).
// A save that keeps the distribution keeps the run.
func TestDistributionChangeEndsTheRun(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	req := clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID))
	head := f.mustPut(req)
	run := clusterTestNow.Add(time.Minute)
	if err := f.routes.BeginClusterMemberRun(f.ctx, head.ID, run, 0); err != nil {
		t.Fatal(err)
	}
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 7000})

	f.mustPut(req)
	rows, gpus := membersByServer(t, f, head.ID)
	if !rows[a.ID].RunStartedAt.Equal(run) || rows[a.ID].RunPeakRAMMB != 1000 || gpus[a.ID][0].RunPeakVRAMMB != 7000 || rows[a.ID].Stale {
		t.Fatalf("an unchanged save = %+v %+v, want the run kept", rows[a.ID], gpus[a.ID])
	}

	req.ClusterMembers = []ClusterMemberDTO{autoMember(b.ID), autoMember(a.ID, 0)}
	f.mustPut(req)
	rows, gpus = membersByServer(t, f, head.ID)
	for _, id := range []string{a.ID, b.ID} {
		if !rows[id].RunStartedAt.IsZero() || rows[id].RunPeakRAMMB != 0 {
			t.Fatalf("member %s after a reorder = %+v, want its run reset", id, rows[id])
		}
	}
	if gpus[a.ID][0].RunPeakVRAMMB != 0 || gpus[a.ID][0].VRAMMeasuredMB != 7000 || rows[a.ID].RAMMeasuredMB != 1000 || !rows[a.ID].Stale {
		t.Fatalf("member A after a reorder = %+v %+v, want its values kept, stale, and its run peaks gone", rows[a.ID], gpus[a.ID])
	}
}

// TestManualMemberGPUChangeEndsTheRun: re-ordering a manual member spec's
// GPUs changes the split of the head that uses it, so every member's run of
// that head is reset with the stale mark.
func TestManualMemberGPUChangeEndsTheRun(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	spec := f.manualSpec(a.ID, RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 8000}, RuntimeSpecGPUDTO{Index: 1, VRAMEstimateMB: 8000})
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID}, autoMember(b.ID))
	req.MemberMode = "manual"
	req.ClusterMembers[1] = ClusterMemberDTO{ServerID: b.ID, MemberSpecID: f.manualSpec(b.ID).ID}
	head := f.mustPut(req)
	run := clusterTestNow.Add(time.Minute)
	if err := f.routes.BeginClusterMemberRun(f.ctx, head.ID, run, 0); err != nil {
		t.Fatal(err)
	}
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 7000, 1: 6000})
	f.usage(head.ID, b.ID, 2000, nil)

	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID, PutRPCMemberSpecRequest{
		Binary: clusterRPCBinary, GPUs: []RuntimeSpecGPUDTO{{Index: 1, VRAMEstimateMB: 8000}, {Index: 0, VRAMEstimateMB: 8000}},
	}); err != nil {
		t.Fatalf("reorder the member spec's GPUs: %v", err)
	}
	rows, gpus := membersByServer(t, f, head.ID)
	for _, id := range []string{a.ID, b.ID} {
		if !rows[id].RunStartedAt.IsZero() || rows[id].RunPeakRAMMB != 0 {
			t.Fatalf("member %s = %+v, want its run reset", id, rows[id])
		}
	}
	if !rows[a.ID].Stale || gpus[a.ID][0].RunPeakVRAMMB != 0 || gpus[a.ID][0].VRAMMeasuredMB != 7000 {
		t.Fatalf("member A = %+v %+v, want stale with its values kept and no run peak", rows[a.ID], gpus[a.ID])
	}
}
