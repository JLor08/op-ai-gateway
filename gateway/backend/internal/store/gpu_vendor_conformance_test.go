// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestConformanceGPUVendorRoundTrip pins, on every routing store, that the
// four GPU row sets carry their vendor (#434): ('nvidia', 0) and ('amd', 0)
// are two rows, a duplicate (vendor, index) is a conflict, the rows read back
// in the documented order, and a measurement write-back addresses exactly one
// (vendor, index) row.
func TestConformanceGPUVendorRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		spec := seedRPCHeadSpec(ctx, t, s, "gv", now)
		server := "srv_head_gv"

		// Spec GPUs: both vendors' index 0, read back in position order.
		specGPUs := []routing.RuntimeSpecGPU{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 0, VRAMEstimateMB: 8000},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 1, VRAMEstimateMB: 3000},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, spec, specGPUs); err != nil {
			t.Fatalf("set spec gpus: %v", err)
		}
		got, err := s.RuntimeSpecGPUs(ctx, spec)
		if err != nil || len(got) != 2 ||
			got[0].Key() != (routing.GPUKey{Vendor: routing.GPUVendorNVIDIA, Index: 0}) || got[0].VRAMEstimateMB != 8000 ||
			got[1].Key() != (routing.GPUKey{Vendor: routing.GPUVendorAMD, Index: 0}) || got[1].VRAMEstimateMB != 3000 ||
			got[0].SpecID != spec || got[1].SpecID != spec {
			t.Fatalf("spec gpus did not round-trip with their vendors: %+v err=%v", got, err)
		}

		// The measurement lands on exactly the (vendor, index) row named.
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, spec, routing.GPUKey{Vendor: routing.GPUVendorAMD, Index: 0}, 3323); err != nil {
			t.Fatalf("measure amd 0: %v", err)
		}
		got, _ = s.RuntimeSpecGPUs(ctx, spec)
		if got[0].VRAMMeasuredMB != 0 || got[1].VRAMMeasuredMB != 3323 {
			t.Fatalf("the measurement must touch only the AMD row: %+v", got)
		}
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, spec, routing.GPUKey{Vendor: "", Index: 0}, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the empty vendor matches only an empty-vendor row: err=%v, want ErrNotFound", err)
		}
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, spec, routing.GPUKey{Vendor: routing.GPUVendorNVIDIA, Index: 1}, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unknown index: err=%v, want ErrNotFound", err)
		}

		// One (vendor, index) twice is the primary-key conflict; nothing changes.
		dup := []routing.RuntimeSpecGPU{
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 0},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 1},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, spec, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate spec gpu key: err=%v, want ErrConflict", err)
		}

		// Within one position the rows order by vendor, then index.
		samePos := []routing.RuntimeSpecGPU{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 1},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, spec, samePos); err != nil {
			t.Fatalf("set same-position spec gpus: %v", err)
		}
		got, _ = s.RuntimeSpecGPUs(ctx, spec)
		if keys := specGPUKeys(got); len(keys) != 3 || keys[0] != "AMD 0" || keys[1] != "AMD 1" || keys[2] != "NVIDIA 0" {
			t.Fatalf("spec gpus must order by position, vendor, index: %v", keys)
		}

		// Budgets: both vendors stored, ordered by vendor then index.
		budgets := []routing.ServerGPUBudget{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, BudgetMB: 8000, CreatedAt: now, UpdatedAt: now},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, BudgetMB: 4000, CreatedAt: now, UpdatedAt: now},
			{GPUVendor: "", GPUIndex: 1, BudgetMB: 2000, CreatedAt: now, UpdatedAt: now},
		}
		if err := s.SetServerGPUBudgets(ctx, server, budgets); err != nil {
			t.Fatalf("set budgets: %v", err)
		}
		gotB, err := s.ServerGPUBudgets(ctx, server)
		if err != nil || len(gotB) != 3 ||
			gotB[0].GPUVendor != "" || gotB[0].GPUIndex != 1 || gotB[0].BudgetMB != 2000 ||
			gotB[1].GPUVendor != routing.GPUVendorAMD || gotB[1].GPUIndex != 0 || gotB[1].BudgetMB != 4000 ||
			gotB[2].GPUVendor != routing.GPUVendorNVIDIA || gotB[2].GPUIndex != 0 || gotB[2].BudgetMB != 8000 {
			t.Fatalf("budgets must round-trip with their vendors, ordered by vendor, index: %+v err=%v", gotB, err)
		}
		dupB := []routing.ServerGPUBudget{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, CreatedAt: now, UpdatedAt: now},
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, CreatedAt: now, UpdatedAt: now},
		}
		if err := s.SetServerGPUBudgets(ctx, server, dupB); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate budget key: err=%v, want ErrConflict", err)
		}

		// Manual member spec GPUs, through both writers.
		member := routing.RPCMemberSpec{
			ID: "rpcm_gv", ServerID: server, Enabled: true, DeviceBackend: "auto",
			AllowedHeadGroups: "[]", CreatedAt: now, UpdatedAt: now,
		}
		memberGPUs := []routing.RPCMemberSpecGPU{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 0, VRAMEstimateMB: 7000},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 1, VRAMEstimateMB: 3500},
		}
		if err := s.ReplaceRPCMemberSpec(ctx, member, memberGPUs); err != nil {
			t.Fatalf("replace member spec: %v", err)
		}
		gotM, err := s.RPCMemberSpecGPUs(ctx, member.ID)
		if err != nil || len(gotM) != 2 ||
			gotM[0].GPUVendor != routing.GPUVendorNVIDIA || gotM[0].GPUIndex != 0 || gotM[0].VRAMEstimateMB != 7000 ||
			gotM[1].GPUVendor != routing.GPUVendorAMD || gotM[1].GPUIndex != 0 || gotM[1].VRAMEstimateMB != 3500 {
			t.Fatalf("member spec gpus did not round-trip with their vendors: %+v err=%v", gotM, err)
		}
		dupM := []routing.RPCMemberSpecGPU{
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 0},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 1},
		}
		if err := s.ReplaceRPCMemberSpec(ctx, member, dupM); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate member spec gpu key (replace): err=%v, want ErrConflict", err)
		}
		if err := s.SetRPCMemberSpecGPUs(ctx, member.ID, dupM); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate member spec gpu key (set): err=%v, want ErrConflict", err)
		}
		samePosM := []routing.RPCMemberSpecGPU{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0},
		}
		if err := s.SetRPCMemberSpecGPUs(ctx, member.ID, samePosM); err != nil {
			t.Fatalf("set member spec gpus: %v", err)
		}
		gotM, _ = s.RPCMemberSpecGPUs(ctx, member.ID)
		if len(gotM) != 2 || gotM[0].GPUVendor != routing.GPUVendorAMD || gotM[1].GPUVendor != routing.GPUVendorNVIDIA {
			t.Fatalf("member spec gpus must order by position, vendor, index: %+v", gotM)
		}
	})
}

// specGPUKeys renders rows' keys in read order.
func specGPUKeys(rows []routing.RuntimeSpecGPU) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key().String())
	}
	return out
}

// TestConformanceTwoMembersOnOneServer pins, on every routing store, that a
// cluster member is addressed by its position (#434): one server may serve a
// head twice (one member per vendor), each member keeps its own GPU rows, and
// usage writes, run ends and the compare-and-replace touch only the member
// they name.
func TestConformanceTwoMembersOnOneServer(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "two", now)
		seedBinariesServer(ctx, t, s, "srv_two_a", now)
		seedBinariesServer(ctx, t, s, "srv_two_b", now)

		members := []routing.ClusterMember{
			{HeadSpecID: head, Position: 0, ServerID: "srv_two_a", DeviceBackend: "cuda", Port: 47100},
			{HeadSpecID: head, Position: 1, ServerID: "srv_two_a", DeviceBackend: "rocm", Port: 47101},
		}
		gpus := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 1, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, VRAMEstimateMB: 3000},
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, VRAMEstimateMB: 7000},
		}
		if err := s.SetClusterMembers(ctx, head, members, gpus); err != nil {
			t.Fatalf("two members on one server: %v", err)
		}
		gotM, gotG, err := s.ClusterMembers(ctx, head)
		if err != nil || len(gotM) != 2 || len(gotG) != 2 {
			t.Fatalf("read back: %+v %+v err=%v", gotM, gotG, err)
		}
		if gotM[0].Position != 0 || gotM[0].DeviceBackend != "cuda" || gotM[1].Position != 1 || gotM[1].DeviceBackend != "rocm" ||
			gotM[0].ServerID != "srv_two_a" || gotM[1].ServerID != "srv_two_a" {
			t.Fatalf("members must read back by position: %+v", gotM)
		}
		if gotG[0].MemberPosition != 0 || gotG[0].GPUVendor != routing.GPUVendorNVIDIA || gotG[0].VRAMEstimateMB != 7000 || gotG[0].ServerID != "srv_two_a" ||
			gotG[1].MemberPosition != 1 || gotG[1].GPUVendor != routing.GPUVendorAMD || gotG[1].VRAMEstimateMB != 3000 || gotG[1].ServerID != "srv_two_a" {
			t.Fatalf("each member must keep its own gpu rows, ordered by member position: %+v", gotG)
		}

		// A run, then a usage write on position 1 only.
		run := now.Add(time.Minute)
		if err := s.BeginClusterMemberRun(ctx, head, run, 3); err != nil {
			t.Fatalf("begin run: %v", err)
		}
		gotM, _, _ = s.ClusterMembers(ctx, head)
		upd0 := gotM[0]
		upd0.RunPeakRAMMB = 111
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd0, []routing.ClusterMemberGPU{
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, VRAMMeasuredMB: 7100, RunPeakVRAMMB: 7200},
		}); err != nil || !applied {
			t.Fatalf("usage on position 0: applied=%v err=%v", applied, err)
		}
		upd1 := gotM[1]
		upd1.RAMMeasuredMB, upd1.MeasuredAt, upd1.RunPeakRAMMB = 500, run, 600
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd1, []routing.ClusterMemberGPU{
			{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, VRAMMeasuredMB: 3323, RunPeakVRAMMB: 3400},
			// position 1 has no NVIDIA 0: skipped, position 0's row untouched
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, VRAMMeasuredMB: 1, RunPeakVRAMMB: 1},
		}); err != nil || !applied {
			t.Fatalf("usage on position 1: applied=%v err=%v", applied, err)
		}
		gotM, gotG, _ = s.ClusterMembers(ctx, head)
		if gotM[0].RAMMeasuredMB != 0 || gotM[0].RunPeakRAMMB != 111 || gotM[1].RAMMeasuredMB != 500 || gotM[1].RunPeakRAMMB != 600 {
			t.Fatalf("the usage write must touch only position 1's row: %+v", gotM)
		}
		if gotG[0].VRAMMeasuredMB != 7100 || gotG[0].RunPeakVRAMMB != 7200 || gotG[1].VRAMMeasuredMB != 3323 || gotG[1].RunPeakVRAMMB != 3400 {
			t.Fatalf("each member's usage must land on its own gpu rows: %+v", gotG)
		}

		// Ending position 1's run leaves position 0 in the run with its peaks.
		if ended, err := s.EndClusterMemberRun(ctx, head, 1, run); err != nil || !ended {
			t.Fatalf("end run of position 1: ended=%v err=%v", ended, err)
		}
		gotM, gotG, _ = s.ClusterMembers(ctx, head)
		if !gotM[1].RunStartedAt.IsZero() || gotM[1].RunPeakRAMMB != 0 || gotG[1].RunPeakVRAMMB != 0 {
			t.Fatalf("position 1's run must be cleared: %+v %+v", gotM[1], gotG[1])
		}
		if !gotM[0].RunStartedAt.Equal(run) || gotM[0].RunHeadInstance != 3 || gotM[0].RunPeakRAMMB != 111 || gotG[0].RunPeakVRAMMB != 7200 {
			t.Fatalf("position 0 must stay in its run: %+v %+v", gotM[0], gotG[0])
		}
		if ended, err := s.EndClusterMemberRun(ctx, head, 0, run.Add(time.Second)); err != nil || ended {
			t.Fatalf("another run's end must not end position 0: ended=%v err=%v", ended, err)
		}
		if _, err := s.EndClusterMemberRun(ctx, head, 5, run); !errors.Is(err, ErrNotFound) {
			t.Fatalf("end run of a missing position: err=%v, want ErrNotFound", err)
		}
		ghost := gotM[0]
		ghost.Position = 5
		if _, err := s.UpdateClusterMemberUsage(ctx, ghost, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("usage on a missing position: err=%v, want ErrNotFound", err)
		}

		// The compare-and-replace compares by position: an unchanged read
		// applies, a read from before a usage write does not.
		stale, staleG := gotM, gotG
		upd0 = gotM[0]
		upd0.RAMMeasuredMB = 900
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd0, nil); err != nil || !applied {
			t.Fatalf("usage on position 0: applied=%v err=%v", applied, err)
		}
		if applied, err := s.SetClusterMembersIf(ctx, head, stale, staleG, members, gpus); err != nil || applied {
			t.Fatalf("a replace over an outdated read must not apply: applied=%v err=%v", applied, err)
		}
		fresh, freshG, _ := s.ClusterMembers(ctx, head)
		if applied, err := s.SetClusterMembersIf(ctx, head, fresh, freshG, fresh, freshG); err != nil || !applied {
			t.Fatalf("a replace over the current read must apply: applied=%v err=%v", applied, err)
		}
		if again, againG, _ := s.ClusterMembers(ctx, head); len(again) != 2 || len(againG) != 2 || again[0].RAMMeasuredMB != 900 {
			t.Fatalf("the replace must store what it was given: %+v %+v", again, againG)
		}

		// A GPU row needs a member of its position and server in the same call.
		before, beforeG, _ := s.ClusterMembers(ctx, head)
		noMember := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 7, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorAMD, GPUIndex: 0},
		}
		if err := s.SetClusterMembers(ctx, head, members, noMember); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a gpu row of a position without a member: err=%v, want ErrNotFound", err)
		}
		wrongServer := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_two_b", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
		}
		if err := s.SetClusterMembers(ctx, head, members, wrongServer); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a gpu row of a server the member at its position does not run on: err=%v, want ErrNotFound", err)
		}
		dupKey := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 1},
		}
		if err := s.SetClusterMembers(ctx, head, members, dupKey); !errors.Is(err, ErrConflict) {
			t.Fatalf("one (member, vendor, index) twice: err=%v, want ErrConflict", err)
		}
		if after, afterG, _ := s.ClusterMembers(ctx, head); len(after) != len(before) || len(afterG) != len(beforeG) ||
			after[0].RAMMeasuredMB != before[0].RAMMeasuredMB {
			t.Fatalf("a failed set must keep the membership: %+v %+v", after, afterG)
		}

		// Two members on one server still need two ports (the (server, port)
		// uniqueness holds within one head too).
		samePort := []routing.ClusterMember{
			{HeadSpecID: head, Position: 0, ServerID: "srv_two_a", DeviceBackend: "cuda", Port: 47100},
			{HeadSpecID: head, Position: 1, ServerID: "srv_two_a", DeviceBackend: "rocm", Port: 47100},
		}
		if err := s.SetClusterMembers(ctx, head, samePort, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("one port twice on one server: err=%v, want ErrConflict", err)
		}
		if after, afterG, _ := s.ClusterMembers(ctx, head); len(after) != 2 || after[1].Port != 47101 || len(afterG) != len(beforeG) {
			t.Fatalf("a refused same-port set must keep the membership: %+v %+v", after, afterG)
		}

		// The same (vendor, index) on two members of one server is two rows.
		sameKey := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
			{HeadSpecID: head, MemberPosition: 1, ServerID: "srv_two_a", GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0},
		}
		if err := s.SetClusterMembers(ctx, head, members, sameKey); err != nil {
			t.Fatalf("one gpu key on two members: %v", err)
		}
		if rev, err := s.ClusterMembershipsByServer(ctx, "srv_two_a"); err != nil || len(rev) != 2 {
			t.Fatalf("the reverse index must list both members of the server: %+v err=%v", rev, err)
		}
	})
}

// TestConformanceAIServerGPUVendorFields pins, on every routing store, that
// the server's legacy GPU vendor and vendor note round-trip through
// CreateAIServer and the readers, and that the full-row UpdateAIServer never
// writes them (only the GPU-vendor backfill writers do), so a portal save
// from a read taken before a backfill cannot undo it.
func TestConformanceAIServerGPUVendorFields(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		srv := routing.AIServer{
			ID: "srv_gvf", Name: "srv_gvf", Provider: routing.ProviderVLLM, Endpoint: "http://srv-gvf:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
			GPULegacyVendor: routing.GPUVendorAMD, GPUVendorNote: true,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil || got.GPULegacyVendor != routing.GPUVendorAMD || !got.GPUVendorNote {
			t.Fatalf("create must store the gpu vendor fields: %+v err=%v", got, err)
		}
		got.GPULegacyVendor, got.GPUVendorNote, got.Name = "", false, "renamed"
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("update: %v", err)
		}
		again, err := s.AIServerByID(ctx, srv.ID)
		if err != nil || again.Name != "renamed" || again.GPULegacyVendor != routing.GPUVendorAMD || !again.GPUVendorNote {
			t.Fatalf("UpdateAIServer must keep the gpu vendor fields: %+v err=%v", again, err)
		}
	})
}

// backfillSeed is one server's legacy rows (empty vendor) in all four tables.
type backfillSeed struct{ server, head, memberSpec string }

// seedBackfillServer creates a server with a runtime spec (two empty-vendor GPU
// rows and one explicit AMD row), an empty-vendor budget, a manual member spec
// with one empty-vendor GPU, and a cluster member of a separate head running on
// the server with one empty-vendor GPU row. It returns the row count the
// backfill must rewrite (5).
func seedBackfillServer(ctx context.Context, t *testing.T, s routing.Store, key string, now time.Time, port int) backfillSeed {
	t.Helper()
	head := seedRPCHeadSpec(ctx, t, s, key, now)
	server := "srv_head_" + key
	if err := s.SetRuntimeSpecGPUs(ctx, head, []routing.RuntimeSpecGPU{
		{GPUIndex: 0, Position: 0, VRAMEstimateMB: 1000},
		{GPUIndex: 1, Position: 1, VRAMEstimateMB: 1100},
		{GPUVendor: routing.GPUVendorAMD, GPUIndex: 0, Position: 2, VRAMEstimateMB: 1200},
	}); err != nil {
		t.Fatalf("seed spec gpus: %v", err)
	}
	if err := s.SetServerGPUBudgets(ctx, server, []routing.ServerGPUBudget{
		{GPUIndex: 0, BudgetMB: 5000, CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("seed budgets: %v", err)
	}
	member := routing.RPCMemberSpec{
		ID: "rpcm_" + key, ServerID: server, Enabled: true, DeviceBackend: "auto",
		AllowedHeadGroups: "[]", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.ReplaceRPCMemberSpec(ctx, member, []routing.RPCMemberSpecGPU{{GPUIndex: 0, VRAMEstimateMB: 700}}); err != nil {
		t.Fatalf("seed member spec: %v", err)
	}
	other := seedRPCHeadSpec(ctx, t, s, key+"_other", now)
	if err := s.SetClusterMembers(ctx, other,
		[]routing.ClusterMember{{HeadSpecID: other, Position: 0, ServerID: server, DeviceBackend: "auto", Port: port}},
		[]routing.ClusterMemberGPU{{HeadSpecID: other, MemberPosition: 0, ServerID: server, GPUIndex: 0, VRAMEstimateMB: 800}},
	); err != nil {
		t.Fatalf("seed cluster member: %v", err)
	}
	return backfillSeed{server: server, head: head, memberSpec: member.ID}
}

// backfillVendors lists every GPU vendor of the server's four row sets, in
// read order, for compact assertions.
func backfillVendors(ctx context.Context, t *testing.T, s routing.Store, seed backfillSeed, key string) (spec, budgets, member, cluster []string) {
	t.Helper()
	specRows, err := s.RuntimeSpecGPUs(ctx, seed.head)
	if err != nil {
		t.Fatalf("spec gpus: %v", err)
	}
	for _, r := range specRows {
		spec = append(spec, r.Key().String())
	}
	budgetRows, err := s.ServerGPUBudgets(ctx, seed.server)
	if err != nil {
		t.Fatalf("budgets: %v", err)
	}
	for _, r := range budgetRows {
		budgets = append(budgets, routing.GPUKey{Vendor: r.GPUVendor, Index: r.GPUIndex}.String())
	}
	memberRows, err := s.RPCMemberSpecGPUs(ctx, seed.memberSpec)
	if err != nil {
		t.Fatalf("member spec gpus: %v", err)
	}
	for _, r := range memberRows {
		member = append(member, routing.GPUKey{Vendor: r.GPUVendor, Index: r.GPUIndex}.String())
	}
	_, clusterRows, err := s.ClusterMembers(ctx, "rspec_"+key+"_other")
	if err != nil {
		t.Fatalf("cluster members: %v", err)
	}
	for _, r := range clusterRows {
		cluster = append(cluster, routing.GPUKey{Vendor: r.GPUVendor, Index: r.GPUIndex}.String())
	}
	return spec, budgets, member, cluster
}

// TestConformanceBackfillServerGPUVendor pins, on every routing store, the
// backfill of legacy empty GPU vendors (#434): only the named server's rows
// are rewritten, an explicit row shadows its empty twin, the legacy vendor is
// recorded on every call, the review note only when a mixed report rewrites
// rows, a call with no empty rows left is a no-op, and clearing the note keeps
// the legacy vendor.
func TestConformanceBackfillServerGPUVendor(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		a := seedBackfillServer(ctx, t, s, "bfa", now, 47200)
		b := seedBackfillServer(ctx, t, s, "bfb", now, 47201)

		if _, err := s.BackfillServerGPUVendor(ctx, a.server, "", true); err == nil {
			t.Fatal("an empty vendor must be refused")
		}
		if _, err := s.BackfillServerGPUVendor(ctx, a.server, "intel", true); err == nil {
			t.Fatal("an unknown vendor must be refused")
		}
		if _, err := s.BackfillServerGPUVendor(ctx, "srv_nope", routing.GPUVendorNVIDIA, true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown server: err=%v, want ErrNotFound", err)
		}
		if srv, _ := s.AIServerByID(ctx, a.server); srv.GPULegacyVendor != "" || srv.GPUVendorNote {
			t.Fatalf("a refused backfill must write nothing: %+v", srv)
		}

		n, err := s.BackfillServerGPUVendor(ctx, a.server, routing.GPUVendorNVIDIA, true)
		if err != nil || n != 5 {
			t.Fatalf("backfill A: rewritten=%d err=%v, want 5", n, err)
		}
		spec, budgets, member, cluster := backfillVendors(ctx, t, s, a, "bfa")
		if len(spec) != 3 || spec[0] != "NVIDIA 0" || spec[1] != "NVIDIA 1" || spec[2] != "AMD 0" ||
			len(budgets) != 1 || budgets[0] != "NVIDIA 0" || len(member) != 1 || member[0] != "NVIDIA 0" ||
			len(cluster) != 1 || cluster[0] != "NVIDIA 0" {
			t.Fatalf("A's rows must carry the vendor: %v %v %v %v", spec, budgets, member, cluster)
		}
		spec, budgets, member, cluster = backfillVendors(ctx, t, s, b, "bfb")
		if len(spec) != 3 || spec[0] == "NVIDIA 0" || spec[1] == "NVIDIA 1" || budgets[0] == "NVIDIA 0" ||
			member[0] == "NVIDIA 0" || cluster[0] == "NVIDIA 0" {
			t.Fatalf("B's rows must stay untouched: %v %v %v %v", spec, budgets, member, cluster)
		}
		srv, _ := s.AIServerByID(ctx, a.server)
		if srv.GPULegacyVendor != routing.GPUVendorNVIDIA || !srv.GPUVendorNote {
			t.Fatalf("A must record the legacy vendor and the note: %+v", srv)
		}

		// Idempotent; the note stays as it is.
		if n, err := s.BackfillServerGPUVendor(ctx, a.server, routing.GPUVendorNVIDIA, true); err != nil || n != 0 {
			t.Fatalf("second backfill: rewritten=%d err=%v, want 0", n, err)
		}
		if srv, _ = s.AIServerByID(ctx, a.server); !srv.GPUVendorNote {
			t.Fatal("a second backfill must leave the note raised")
		}

		// No mixed report: the legacy vendor is recorded, the note is not.
		if n, err := s.BackfillServerGPUVendor(ctx, b.server, routing.GPUVendorNVIDIA, false); err != nil || n != 5 {
			t.Fatalf("backfill B: rewritten=%d err=%v, want 5", n, err)
		}
		if srv, _ = s.AIServerByID(ctx, b.server); srv.GPULegacyVendor != routing.GPUVendorNVIDIA || srv.GPUVendorNote {
			t.Fatalf("B must record the legacy vendor without the note: %+v", srv)
		}

		// Clearing the note keeps the legacy vendor; a further backfill does
		// not raise it again.
		if err := s.ClearServerGPUVendorNote(ctx, a.server); err != nil {
			t.Fatalf("clear note: %v", err)
		}
		if srv, _ = s.AIServerByID(ctx, a.server); srv.GPUVendorNote || srv.GPULegacyVendor != routing.GPUVendorNVIDIA {
			t.Fatalf("clear must keep the legacy vendor: %+v", srv)
		}
		if n, err := s.BackfillServerGPUVendor(ctx, a.server, routing.GPUVendorNVIDIA, true); err != nil || n != 0 {
			t.Fatalf("backfill after clear: rewritten=%d err=%v, want 0", n, err)
		}
		if srv, _ = s.AIServerByID(ctx, a.server); srv.GPUVendorNote {
			t.Fatal("a no-op backfill must not raise the cleared note")
		}
		if err := s.ClearServerGPUVendorNote(ctx, "srv_nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("clear on an unknown server: err=%v, want ErrNotFound", err)
		}
	})
}

// TestConformanceBackfillExplicitRowWins pins, on every routing store, that
// when an empty-vendor row and an explicit row share a key the empty row is
// deleted and the explicit row keeps its values, in every table (the cluster
// key includes the member position).
func TestConformanceBackfillExplicitRowWins(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "bfw", now)
		server := "srv_head_bfw"
		if err := s.SetRuntimeSpecGPUs(ctx, head, []routing.RuntimeSpecGPU{
			{GPUIndex: 0, Position: 0, VRAMEstimateMB: 1000},
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 1, VRAMEstimateMB: 2000},
		}); err != nil {
			t.Fatalf("seed spec gpus: %v", err)
		}
		if err := s.SetServerGPUBudgets(ctx, server, []routing.ServerGPUBudget{
			{GPUIndex: 0, BudgetMB: 1, CreatedAt: now, UpdatedAt: now},
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, BudgetMB: 2, CreatedAt: now, UpdatedAt: now},
		}); err != nil {
			t.Fatalf("seed budgets: %v", err)
		}
		member := routing.RPCMemberSpec{
			ID: "rpcm_bfw", ServerID: server, Enabled: true, DeviceBackend: "auto",
			AllowedHeadGroups: "[]", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.ReplaceRPCMemberSpec(ctx, member, []routing.RPCMemberSpecGPU{
			{GPUIndex: 0, VRAMEstimateMB: 10},
			{GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 1, VRAMEstimateMB: 20},
		}); err != nil {
			t.Fatalf("seed member spec: %v", err)
		}
		other := seedRPCHeadSpec(ctx, t, s, "bfw_other", now)
		// Position 0 holds both rows (explicit wins); position 1 only an
		// empty row of the same index, which is rewritten, not dropped.
		if err := s.SetClusterMembers(ctx, other,
			[]routing.ClusterMember{
				{HeadSpecID: other, Position: 0, ServerID: server, DeviceBackend: "auto", Port: 47300},
				{HeadSpecID: other, Position: 1, ServerID: server, DeviceBackend: "auto", Port: 47301},
			},
			[]routing.ClusterMemberGPU{
				{HeadSpecID: other, MemberPosition: 0, ServerID: server, GPUIndex: 0, VRAMEstimateMB: 100},
				{HeadSpecID: other, MemberPosition: 0, ServerID: server, GPUVendor: routing.GPUVendorNVIDIA, GPUIndex: 0, Position: 1, VRAMEstimateMB: 200},
				{HeadSpecID: other, MemberPosition: 1, ServerID: server, GPUIndex: 0, VRAMEstimateMB: 300},
			},
		); err != nil {
			t.Fatalf("seed cluster members: %v", err)
		}

		n, err := s.BackfillServerGPUVendor(ctx, server, routing.GPUVendorNVIDIA, true)
		if err != nil || n != 5 {
			t.Fatalf("backfill: rewritten=%d err=%v, want 5 (rewritten or dropped rows)", n, err)
		}
		specRows, _ := s.RuntimeSpecGPUs(ctx, head)
		if len(specRows) != 1 || specRows[0].GPUVendor != routing.GPUVendorNVIDIA || specRows[0].VRAMEstimateMB != 2000 {
			t.Fatalf("spec gpus: explicit row must win: %+v", specRows)
		}
		budgetRows, _ := s.ServerGPUBudgets(ctx, server)
		if len(budgetRows) != 1 || budgetRows[0].GPUVendor != routing.GPUVendorNVIDIA || budgetRows[0].BudgetMB != 2 {
			t.Fatalf("budgets: explicit row must win: %+v", budgetRows)
		}
		memberRows, _ := s.RPCMemberSpecGPUs(ctx, member.ID)
		if len(memberRows) != 1 || memberRows[0].GPUVendor != routing.GPUVendorNVIDIA || memberRows[0].VRAMEstimateMB != 20 {
			t.Fatalf("member spec gpus: explicit row must win: %+v", memberRows)
		}
		_, clusterRows, _ := s.ClusterMembers(ctx, other)
		if len(clusterRows) != 2 ||
			clusterRows[0].MemberPosition != 0 || clusterRows[0].GPUVendor != routing.GPUVendorNVIDIA || clusterRows[0].VRAMEstimateMB != 200 ||
			clusterRows[1].MemberPosition != 1 || clusterRows[1].GPUVendor != routing.GPUVendorNVIDIA || clusterRows[1].VRAMEstimateMB != 300 {
			t.Fatalf("cluster gpus: per member, explicit wins, the lone empty row is rewritten: %+v", clusterRows)
		}
	})
}
