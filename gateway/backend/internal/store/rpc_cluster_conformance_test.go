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

// seedRPCHeadSpec creates a server, application, mapping and runtime spec and
// returns the spec id: the minimal chain a cluster head hangs off.
func seedRPCHeadSpec(ctx context.Context, t *testing.T, s routing.Store, key string, now time.Time) string {
	t.Helper()
	seedBinariesServer(ctx, t, s, "srv_head_"+key, now)
	if err := s.CreateApplication(ctx, routing.Application{
		ID: "app_" + key, ServerID: "srv_head_" + key, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application %s: %v", key, err)
	}
	if err := s.CreateMapping(ctx, routing.ModelMapping{
		ID: "map_" + key, ApplicationID: "app_" + key, GatewayModelName: "m-" + key, AppModelName: "u-" + key,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create mapping %s: %v", key, err)
	}
	if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: "rspec_" + key, MappingID: "map_" + key, Enabled: true, Binary: "/usr/bin/llama-server",
		Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert runtime spec %s: %v", key, err)
	}
	return "rspec_" + key
}

// TestConformanceRPCClusterMemberSpecs pins the manual member spec contract on
// every routing store: upsert/read/list/delete, the GPU row set, cascades from
// the spec and from the server, and the unknown-parent error.
func TestConformanceRPCClusterMemberSpecs(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		seedBinariesServer(ctx, t, s, "srv_ms", now)
		seedBinariesServer(ctx, t, s, "srv_ms2", now)

		spec := routing.RPCMemberSpec{
			ID: "rpcm_a", ServerID: "srv_ms", Enabled: true, Binary: "/opt/rpc-server", Args: "-t 8",
			Env: `{"A":"1"}`, CacheEnabled: true, DeviceBackend: "cuda", RAMEstimateMB: 2048,
			AllowedHeadGroups: `["ugrp_a"]`, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertRPCMemberSpec(ctx, routing.RPCMemberSpec{ID: "rpcm_x", ServerID: "srv_nope", CreatedAt: now, UpdatedAt: now}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("upsert on unknown server: err=%v, want ErrNotFound", err)
		}
		if _, ok, err := s.RPCMemberSpecByID(ctx, "rpcm_a"); err != nil || ok {
			t.Fatalf("missing spec: ok=%v err=%v", ok, err)
		}
		if err := s.UpsertRPCMemberSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := s.RPCMemberSpecByID(ctx, "rpcm_a")
		if err != nil || !ok {
			t.Fatalf("read: ok=%v err=%v", ok, err)
		}
		if got.ServerID != "srv_ms" || !got.Enabled || got.Binary != "/opt/rpc-server" || got.Args != "-t 8" ||
			got.Env != `{"A":"1"}` || !got.CacheEnabled || got.DeviceBackend != "cuda" || got.RAMEstimateMB != 2048 ||
			got.AllowedHeadGroups != `["ugrp_a"]` || !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
			t.Fatalf("spec did not round-trip: %+v", got)
		}

		later := now.Add(time.Hour)
		spec.Enabled, spec.Args, spec.CacheEnabled, spec.RAMEstimateMB = false, "-t 4", false, 1024
		spec.CreatedAt, spec.UpdatedAt = later, later
		if err := s.UpsertRPCMemberSpec(ctx, spec); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		got, _, _ = s.RPCMemberSpecByID(ctx, "rpcm_a")
		if got.Enabled || got.Args != "-t 4" || got.CacheEnabled || got.RAMEstimateMB != 1024 ||
			!got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(later) {
			t.Fatalf("update must overwrite settings, keep created_at and move updated_at: %+v", got)
		}

		if err := s.UpsertRPCMemberSpec(ctx, routing.RPCMemberSpec{ID: "rpcm_b", ServerID: "srv_ms", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("second spec: %v", err)
		}
		if err := s.UpsertRPCMemberSpec(ctx, routing.RPCMemberSpec{ID: "rpcm_c", ServerID: "srv_ms2", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("other server spec: %v", err)
		}
		list, err := s.RPCMemberSpecsByServer(ctx, "srv_ms")
		if err != nil || len(list) != 2 || list[0].ID != "rpcm_a" || list[1].ID != "rpcm_b" {
			t.Fatalf("by server must list the server's specs by id: %+v err=%v", list, err)
		}
		if none, err := s.RPCMemberSpecsByServer(ctx, "srv_none"); err != nil || none == nil || len(none) != 0 {
			t.Fatalf("empty listing must be non-nil: %v err=%v", none, err)
		}

		// GPU rows: ordered by position then index, replaced atomically.
		if err := s.SetRPCMemberSpecGPUs(ctx, "rpcm_nope", nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("gpus on unknown spec: err=%v, want ErrNotFound", err)
		}
		if err := s.SetRPCMemberSpecGPUs(ctx, "rpcm_a", []routing.RPCMemberSpecGPU{
			{SpecID: "rpcm_a", GPUIndex: 1, Position: 0, VRAMEstimateMB: 8000},
			{SpecID: "rpcm_a", GPUIndex: 0, Position: 1, VRAMEstimateMB: 6000},
		}); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gpus, err := s.RPCMemberSpecGPUs(ctx, "rpcm_a")
		if err != nil || len(gpus) != 2 || gpus[0].GPUIndex != 1 || gpus[0].VRAMEstimateMB != 8000 || gpus[1].GPUIndex != 0 || gpus[1].Position != 1 {
			t.Fatalf("gpus must be ordered by position: %+v err=%v", gpus, err)
		}
		if err := s.SetRPCMemberSpecGPUs(ctx, "rpcm_a", []routing.RPCMemberSpecGPU{
			{SpecID: "rpcm_a", GPUIndex: 2}, {SpecID: "rpcm_a", GPUIndex: 2},
		}); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate gpu index: err=%v, want ErrConflict", err)
		}
		if gpus, _ := s.RPCMemberSpecGPUs(ctx, "rpcm_a"); len(gpus) != 2 {
			t.Fatalf("a failed replace must leave the previous gpus, got %+v", gpus)
		}
		if err := s.SetRPCMemberSpecGPUs(ctx, "rpcm_b", []routing.RPCMemberSpecGPU{{SpecID: "rpcm_b", GPUIndex: 0, VRAMEstimateMB: 1}}); err != nil {
			t.Fatalf("set gpus b: %v", err)
		}
		if empty, err := s.RPCMemberSpecGPUs(ctx, "rpcm_c"); err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty gpu listing must be non-nil: %v err=%v", empty, err)
		}

		// Delete: ErrNotFound when absent, GPU rows go with the spec.
		if err := s.DeleteRPCMemberSpec(ctx, "rpcm_a"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := s.DeleteRPCMemberSpec(ctx, "rpcm_a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete: err=%v, want ErrNotFound", err)
		}
		if gpus, _ := s.RPCMemberSpecGPUs(ctx, "rpcm_a"); len(gpus) != 0 {
			t.Fatalf("spec delete must cascade to its gpus: %+v", gpus)
		}

		// Server delete cascades to its specs and their gpus, not to others.
		if err := s.DeleteAIServer(ctx, "srv_ms"); err != nil {
			t.Fatalf("delete server: %v", err)
		}
		if _, ok, _ := s.RPCMemberSpecByID(ctx, "rpcm_b"); ok {
			t.Fatalf("server delete must cascade to its member specs")
		}
		if gpus, _ := s.RPCMemberSpecGPUs(ctx, "rpcm_b"); len(gpus) != 0 {
			t.Fatalf("server delete must cascade to member spec gpus: %+v", gpus)
		}
		if _, ok, _ := s.RPCMemberSpecByID(ctx, "rpcm_c"); !ok {
			t.Fatalf("another server's spec must survive")
		}
	})
}

// TestConformanceRPCClusterMembership pins the membership contract: atomic
// replace, ordering, both unique rules, the reverse index, usage-only updates,
// and the cascades from the head spec and from the member server.
func TestConformanceRPCClusterMembership(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "h1", now)
		head2 := seedRPCHeadSpec(ctx, t, s, "h2", now)
		seedBinariesServer(ctx, t, s, "srv_m1", now)
		seedBinariesServer(ctx, t, s, "srv_m2", now)
		if err := s.UpsertRPCMemberSpec(ctx, routing.RPCMemberSpec{ID: "rpcm_1", ServerID: "srv_m1", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("member spec: %v", err)
		}

		members := []routing.ClusterMember{
			{HeadSpecID: head, Position: 1, ServerID: "srv_m2", Threads: 4, CacheEnabled: true, DeviceBackend: "vulkan", RAMEstimateMB: 512, Port: 47002},
			{HeadSpecID: head, Position: 0, ServerID: "srv_m1", MemberSpecID: "rpcm_1", DeviceBackend: "auto", Port: 47001},
		}
		gpus := []routing.ClusterMemberGPU{
			{HeadSpecID: head, ServerID: "srv_m2", GPUIndex: 1, Position: 1, VRAMEstimateMB: 4000},
			{HeadSpecID: head, ServerID: "srv_m2", GPUIndex: 0, Position: 0, VRAMEstimateMB: 6000},
		}
		if err := s.SetClusterMembers(ctx, "rspec_nope", members, gpus); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown head: err=%v, want ErrNotFound", err)
		}
		bad := append([]routing.ClusterMember(nil), members...)
		bad[0].ServerID = "srv_nope"
		if err := s.SetClusterMembers(ctx, head, bad, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown member server: err=%v, want ErrNotFound", err)
		}
		if err := s.SetClusterMembers(ctx, head, members, gpus); err != nil {
			t.Fatalf("set members: %v", err)
		}
		gotM, gotG, err := s.ClusterMembers(ctx, head)
		if err != nil || len(gotM) != 2 || len(gotG) != 2 {
			t.Fatalf("read: members=%v gpus=%v err=%v", gotM, gotG, err)
		}
		if gotM[0].ServerID != "srv_m1" || gotM[0].MemberSpecID != "rpcm_1" || gotM[0].Port != 47001 || !gotM[0].MeasuredAt.IsZero() || !gotM[0].RunStartedAt.IsZero() {
			t.Fatalf("members must be ordered by position, zero times round-trip: %+v", gotM)
		}
		if m := gotM[1]; m.ServerID != "srv_m2" || m.Threads != 4 || !m.CacheEnabled || m.DeviceBackend != "vulkan" || m.RAMEstimateMB != 512 || m.MemberSpecID != "" {
			t.Fatalf("automatic member did not round-trip: %+v", m)
		}
		if gotG[0].GPUIndex != 0 || gotG[0].VRAMEstimateMB != 6000 || gotG[1].GPUIndex != 1 || gotG[0].HeadSpecID != head || gotG[0].ServerID != "srv_m2" {
			t.Fatalf("gpus must be ordered by position: %+v", gotG)
		}
		if em, eg, err := s.ClusterMembers(ctx, head2); err != nil || em == nil || eg == nil || len(em) != 0 || len(eg) != 0 {
			t.Fatalf("empty head must read non-nil empties: %v %v err=%v", em, eg, err)
		}

		// Reverse index and by-spec lookup.
		rev, err := s.ClusterMembershipsByServer(ctx, "srv_m2")
		if err != nil || len(rev) != 1 || rev[0].HeadSpecID != head {
			t.Fatalf("reverse index: %+v err=%v", rev, err)
		}
		if none, err := s.ClusterMembershipsByServer(ctx, "srv_none"); err != nil || none == nil || len(none) != 0 {
			t.Fatalf("empty reverse index must be non-nil: %v err=%v", none, err)
		}
		bySpec, ok, err := s.ClusterMemberBySpec(ctx, "rpcm_1")
		if err != nil || !ok || bySpec.ServerID != "srv_m1" || bySpec.HeadSpecID != head {
			t.Fatalf("by spec: %+v ok=%v err=%v", bySpec, ok, err)
		}
		if _, ok, err := s.ClusterMemberBySpec(ctx, "rpcm_none"); err != nil || ok {
			t.Fatalf("by spec miss: ok=%v err=%v", ok, err)
		}

		// Unique rules.
		dupPos := []routing.ClusterMember{
			{HeadSpecID: head2, Position: 0, ServerID: "srv_m1"}, {HeadSpecID: head2, Position: 0, ServerID: "srv_m2"},
		}
		if err := s.SetClusterMembers(ctx, head2, dupPos, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate position: err=%v, want ErrConflict", err)
		}
		dupSrv := []routing.ClusterMember{
			{HeadSpecID: head2, Position: 0, ServerID: "srv_m1"}, {HeadSpecID: head2, Position: 1, ServerID: "srv_m1"},
		}
		if err := s.SetClusterMembers(ctx, head2, dupSrv, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate server: err=%v, want ErrConflict", err)
		}
		reuse := []routing.ClusterMember{{HeadSpecID: head2, Position: 0, ServerID: "srv_m1", MemberSpecID: "rpcm_1"}}
		if err := s.SetClusterMembers(ctx, head2, reuse, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("manual spec on a second head: err=%v, want ErrConflict", err)
		}
		if em, _, _ := s.ClusterMembers(ctx, head2); len(em) != 0 {
			t.Fatalf("a failed set must leave head2 empty: %+v", em)
		}
		if em, _, _ := s.ClusterMembers(ctx, head); len(em) != 2 {
			t.Fatalf("a failed set elsewhere must not touch head: %+v", em)
		}
		// Automatic members ('' spec) never collide with each other.
		autos := []routing.ClusterMember{
			{HeadSpecID: head2, Position: 0, ServerID: "srv_m1"}, {HeadSpecID: head2, Position: 1, ServerID: "srv_m2"},
		}
		if err := s.SetClusterMembers(ctx, head2, autos, nil); err != nil {
			t.Fatalf("two automatic members on one head: %v", err)
		}

		// A failed replace of head keeps its previous membership (atomic).
		if err := s.SetClusterMembers(ctx, head, dupPos2(head), nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate on replace: err=%v, want ErrConflict", err)
		}
		if em, eg, _ := s.ClusterMembers(ctx, head); len(em) != 2 || len(eg) != 2 {
			t.Fatalf("a failed replace must keep the old membership and gpus: %+v %+v", em, eg)
		}

		// Usage update touches only the measured fields, and only while the
		// row is in the run the caller read (none here).
		measured := now.Add(time.Minute)
		upd := members[0] // the srv_m2 member
		upd.RAMMeasuredMB, upd.MeasuredAt, upd.Stale = 700, measured, true
		upd.RunPeakRAMMB, upd.RunHeadInstance = 900, 2
		upd.Threads, upd.Port, upd.RAMEstimateMB = 99, 99, 99 // must be ignored
		ug := []routing.ClusterMemberGPU{{HeadSpecID: head, ServerID: "srv_m2", GPUIndex: 0, VRAMMeasuredMB: 5500, RunPeakVRAMMB: 5800, VRAMEstimateMB: 1}}
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd, ug); err != nil || !applied {
			t.Fatalf("usage update: applied=%v err=%v", applied, err)
		}
		gotM, gotG, _ = s.ClusterMembers(ctx, head)
		m := gotM[1]
		if m.RAMMeasuredMB != 700 || !m.MeasuredAt.Equal(measured) || !m.Stale || m.RunPeakRAMMB != 900 || m.RunHeadInstance != 0 || !m.RunStartedAt.IsZero() {
			t.Fatalf("usage fields not written, or the run identity touched: %+v", m)
		}
		if m.Threads != 4 || m.Port != 47002 || m.RAMEstimateMB != 512 || m.DeviceBackend != "vulkan" {
			t.Fatalf("usage update must not touch settings: %+v", m)
		}
		if gotG[0].VRAMMeasuredMB != 5500 || gotG[0].RunPeakVRAMMB != 5800 || gotG[0].VRAMEstimateMB != 6000 || gotG[1].VRAMMeasuredMB != 0 {
			t.Fatalf("gpu usage must touch only the given row's measured fields: %+v", gotG)
		}
		ghost := upd
		ghost.ServerID = "srv_ghost"
		if _, err := s.UpdateClusterMemberUsage(ctx, ghost, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("usage on a missing row: err=%v, want ErrNotFound", err)
		}

		// Replacing drops the old rows and gpus.
		if err := s.SetClusterMembers(ctx, head, members[:1], nil); err != nil {
			t.Fatalf("shrink: %v", err)
		}
		if em, eg, _ := s.ClusterMembers(ctx, head); len(em) != 1 || len(eg) != 0 {
			t.Fatalf("replace must drop the other member and its gpus: %+v %+v", em, eg)
		}
		if err := s.SetClusterMembers(ctx, head, members, gpus); err != nil {
			t.Fatalf("restore: %v", err)
		}

		// Member server delete cascades its membership (and gpus), nothing else.
		if err := s.DeleteAIServer(ctx, "srv_m2"); err != nil {
			t.Fatalf("delete member server: %v", err)
		}
		em, eg, _ := s.ClusterMembers(ctx, head)
		if len(em) != 1 || em[0].ServerID != "srv_m1" || len(eg) != 0 {
			t.Fatalf("member server delete must remove its membership and gpus only: %+v %+v", em, eg)
		}
		// Head spec delete cascades the membership.
		if err := s.DeleteRuntimeSpec(ctx, head); err != nil {
			t.Fatalf("delete head spec: %v", err)
		}
		if em, _, _ := s.ClusterMembers(ctx, head); len(em) != 0 {
			t.Fatalf("head delete must cascade membership: %+v", em)
		}
		if _, ok, _ := s.ClusterMemberBySpec(ctx, "rpcm_1"); ok {
			t.Fatalf("head delete must free the manual spec")
		}
		if em, _, _ := s.ClusterMembers(ctx, head2); len(em) != 1 {
			t.Fatalf("the other head must keep its membership: %+v", em)
		}
		// Deleting the head's server (cascading application, mapping, spec) too.
		if err := s.DeleteAIServer(ctx, "srv_head_h2"); err != nil {
			t.Fatalf("delete head server: %v", err)
		}
		if em, _, _ := s.ClusterMembers(ctx, head2); len(em) != 0 {
			t.Fatalf("head server delete must cascade through the spec to membership: %+v", em)
		}
	})
}

func dupPos2(head string) []routing.ClusterMember {
	return []routing.ClusterMember{
		{HeadSpecID: head, Position: 0, ServerID: "srv_m1"}, {HeadSpecID: head, Position: 0, ServerID: "srv_m2"},
	}
}

// TestConformanceRPCSessions pins session open/close and the point-in-time
// read: closed bounds are inclusive, only open sessions close, scoped per
// (head, instance, member).
func TestConformanceRPCSessions(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for i, m := range []string{"srv_a", "srv_b"} {
			if err := s.OpenRPCSession(ctx, routing.RPCSession{HeadSpecID: "rspec_h", Instance: 0, MemberServerID: m, Since: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
				t.Fatalf("open %s: %v", m, err)
			}
		}
		if err := s.OpenRPCSession(ctx, routing.RPCSession{HeadSpecID: "rspec_h", Instance: 1, MemberServerID: "srv_a", Since: t0}); err != nil {
			t.Fatalf("open instance 1: %v", err)
		}
		ids := func(rows []routing.RPCSession) []string {
			out := []string{}
			for _, r := range rows {
				out = append(out, r.MemberServerID)
			}
			return out
		}
		at := func(inst int, when time.Time) []routing.RPCSession {
			rows, err := s.RPCSessionsAt(ctx, "rspec_h", inst, when)
			if err != nil || rows == nil {
				t.Fatalf("sessions at: rows=%v err=%v", rows, err)
			}
			return rows
		}
		if got := at(0, t0.Add(-time.Second)); len(got) != 0 {
			t.Fatalf("before every since: %v", ids(got))
		}
		if got := at(0, t0); len(got) != 1 || got[0].MemberServerID != "srv_a" || got[0].Until != nil || !got[0].Since.Equal(t0) {
			t.Fatalf("inclusive since, open session: %+v", got)
		}
		if got := at(0, t0.Add(time.Hour)); len(got) != 2 || got[0].MemberServerID != "srv_a" || got[1].MemberServerID != "srv_b" {
			t.Fatalf("both open, ordered by since: %v", ids(got))
		}

		until := t0.Add(10 * time.Minute)
		if err := s.CloseRPCSessions(ctx, "rspec_h", 0, "srv_a", until); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := s.CloseRPCSessions(ctx, "rspec_h", 0, "srv_none", until); err != nil {
			t.Fatalf("closing none must not fail: %v", err)
		}
		if got := at(0, until); len(got) != 2 {
			t.Fatalf("until is inclusive: %v", ids(got))
		}
		got := at(0, until.Add(time.Second))
		if len(got) != 1 || got[0].MemberServerID != "srv_b" {
			t.Fatalf("after until only the open session remains: %v", ids(got))
		}
		if inst1 := at(1, until.Add(time.Second)); len(inst1) != 1 || inst1[0].Until != nil {
			t.Fatalf("closing instance 0 must not touch instance 1: %+v", inst1)
		}
		// A closed session is not closed again: its until stays.
		if err := s.CloseRPCSessions(ctx, "rspec_h", 0, "srv_a", until.Add(time.Hour)); err != nil {
			t.Fatalf("re-close: %v", err)
		}
		closed := at(0, t0.Add(5*time.Minute))
		if len(closed) != 2 || closed[0].Until == nil || !closed[0].Until.Equal(until) {
			t.Fatalf("a re-close must not move an existing until: %+v", closed)
		}
		if got := at(0, until.Add(30*time.Minute)); len(got) != 1 {
			t.Fatalf("a re-close must not extend the closed session: %v", ids(got))
		}
	})
}

// TestConformanceRPCSessionsPrune pins the retention prune: a closed session
// whose until is before the cutoff goes, one closed at or after it stays, and
// an open session stays however old it is.
func TestConformanceRPCSessionsPrune(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for _, m := range []string{"srv_old", "srv_edge", "srv_open"} {
			if err := s.OpenRPCSession(ctx, routing.RPCSession{HeadSpecID: "rspec_h", MemberServerID: m, Since: t0}); err != nil {
				t.Fatalf("open %s: %v", m, err)
			}
		}
		cutoff := t0.Add(time.Hour)
		if err := s.CloseRPCSessions(ctx, "rspec_h", 0, "srv_old", cutoff.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseRPCSessions(ctx, "rspec_h", 0, "srv_edge", cutoff); err != nil {
			t.Fatal(err)
		}
		if err := s.PruneRPCSessions(ctx, cutoff); err != nil {
			t.Fatalf("prune: %v", err)
		}
		rows, err := s.RPCSessionsAt(ctx, "rspec_h", 0, t0)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, r := range rows {
			got = append(got, r.MemberServerID)
		}
		if len(got) != 2 || got[0] != "srv_edge" || got[1] != "srv_open" {
			t.Fatalf("after prune = %v, want the session closed at the cutoff and the open one", got)
		}
	})
}

// TestConformanceRPCClusterMemberRuns pins the run identity of a head's
// membership rows: a begin stamps every row with the run's start and clears
// the run peaks, a usage write applies only while the row is still in the run
// the caller read (a membership rewrite that reset the run refuses it), and
// an end clears the run only for the run it names.
func TestConformanceRPCClusterMemberRuns(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "runs", now)
		seedBinariesServer(ctx, t, s, "srv_r1", now)
		seedBinariesServer(ctx, t, s, "srv_r2", now)
		members := []routing.ClusterMember{
			{HeadSpecID: head, Position: 0, ServerID: "srv_r1", Port: 47001, RunPeakRAMMB: 50},
			{HeadSpecID: head, Position: 1, ServerID: "srv_r2", Port: 47001},
		}
		gpus := []routing.ClusterMemberGPU{{HeadSpecID: head, ServerID: "srv_r1", GPUIndex: 0, RunPeakVRAMMB: 70}}
		if err := s.SetClusterMembers(ctx, head, members, gpus); err != nil {
			t.Fatal(err)
		}
		read := func() (map[string]routing.ClusterMember, routing.ClusterMemberGPU) {
			t.Helper()
			m, g, err := s.ClusterMembers(ctx, head)
			if err != nil || len(g) != 1 {
				t.Fatalf("read: %v %v", g, err)
			}
			out := map[string]routing.ClusterMember{}
			for _, row := range m {
				out[row.ServerID] = row
			}
			return out, g[0]
		}

		run := now.Add(time.Minute).Truncate(time.Millisecond)
		if err := s.BeginClusterMemberRun(ctx, head, run, 0); err != nil {
			t.Fatalf("begin: %v", err)
		}
		got, g := read()
		for _, id := range []string{"srv_r1", "srv_r2"} {
			if !got[id].RunStartedAt.Equal(run) || got[id].RunPeakRAMMB != 0 {
				t.Fatalf("%s after begin = %+v, want the run stamped and its peak cleared", id, got[id])
			}
		}
		if g.RunPeakVRAMMB != 0 {
			t.Fatalf("gpu after begin = %+v, want its peak cleared", g)
		}

		// A write for the run the caller read applies; one for another run
		// does not, and writes nothing.
		upd := got["srv_r1"]
		upd.RAMMeasuredMB, upd.RunPeakRAMMB = 4000, 4000
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd, []routing.ClusterMemberGPU{{GPUIndex: 0, VRAMMeasuredMB: 9000, RunPeakVRAMMB: 9000}}); err != nil || !applied {
			t.Fatalf("write in the run: applied=%v err=%v", applied, err)
		}
		other := upd
		other.RunStartedAt, other.RAMMeasuredMB, other.Stale = run.Add(time.Second), 1, true
		if applied, err := s.UpdateClusterMemberUsage(ctx, other, []routing.ClusterMemberGPU{{GPUIndex: 0, VRAMMeasuredMB: 1}}); err != nil || applied {
			t.Fatalf("write for another run: applied=%v err=%v, want refused", applied, err)
		}
		got, g = read()
		if r := got["srv_r1"]; r.RAMMeasuredMB != 4000 || r.Stale || g.VRAMMeasuredMB != 9000 || g.RunPeakVRAMMB != 9000 {
			t.Fatalf("after the refused write = %+v %+v, want the run's write only", r, g)
		}

		// A membership rewrite that resets the run (a distribution change)
		// refuses a write read before it.
		rewritten, _, _ := s.ClusterMembers(ctx, head)
		for i := range rewritten {
			rewritten[i].RunStartedAt, rewritten[i].RunPeakRAMMB, rewritten[i].Stale = time.Time{}, 0, true
		}
		if err := s.SetClusterMembers(ctx, head, rewritten, []routing.ClusterMemberGPU{{HeadSpecID: head, ServerID: "srv_r1", GPUIndex: 0, VRAMMeasuredMB: 9000}}); err != nil {
			t.Fatal(err)
		}
		upd.Stale = false
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd, nil); err != nil || applied {
			t.Fatalf("write across a rewrite: applied=%v err=%v, want refused", applied, err)
		}
		if ended, err := s.EndClusterMemberRun(ctx, head, "srv_r1", run); err != nil || ended {
			t.Fatalf("end of a reset run: ended=%v err=%v, want nothing ended", ended, err)
		}
		if got, _ = read(); !got["srv_r1"].Stale {
			t.Fatalf("the rewrite's stale mark was overwritten: %+v", got["srv_r1"])
		}

		// An end names its run: another run's end leaves it alone.
		run2 := run.Add(time.Hour)
		if err := s.BeginClusterMemberRun(ctx, head, run2, 0); err != nil {
			t.Fatal(err)
		}
		if ended, err := s.EndClusterMemberRun(ctx, head, "srv_r2", run); err != nil || ended {
			t.Fatalf("end of the old run: ended=%v err=%v", ended, err)
		}
		if ended, err := s.EndClusterMemberRun(ctx, head, "srv_r2", run2); err != nil || !ended {
			t.Fatalf("end of the current run: ended=%v err=%v", ended, err)
		}
		got, _ = read()
		if !got["srv_r2"].RunStartedAt.IsZero() || !got["srv_r1"].RunStartedAt.Equal(run2) {
			t.Fatalf("after the end = %+v", got)
		}
		if _, err := s.EndClusterMemberRun(ctx, head, "srv_ghost", run2); !errors.Is(err, ErrNotFound) {
			t.Fatalf("end on a missing row: err=%v, want ErrNotFound", err)
		}
	})
}

// TestConformanceOpenRPCSessionsByMember pins the listings of open sessions:
// per member every head's open session on it, none closed, none of another
// member; and every open session, by member server.
func TestConformanceOpenRPCSessionsByMember(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for _, sess := range []routing.RPCSession{
			{HeadSpecID: "rspec_b", MemberServerID: "srv_a", Since: t0.Add(time.Minute)},
			{HeadSpecID: "rspec_a", MemberServerID: "srv_a", Since: t0},
			{HeadSpecID: "rspec_c", MemberServerID: "srv_a", Since: t0},
			{HeadSpecID: "rspec_a", MemberServerID: "srv_b", Since: t0},
		} {
			if err := s.OpenRPCSession(ctx, sess); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CloseRPCSessions(ctx, "rspec_c", 0, "srv_a", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := s.OpenRPCSessionsByMember(ctx, "srv_a")
		if err != nil || len(got) != 2 || got[0].HeadSpecID != "rspec_a" || got[1].HeadSpecID != "rspec_b" || got[0].Until != nil {
			t.Fatalf("open sessions of srv_a = %+v err=%v, want rspec_a then rspec_b", got, err)
		}
		if none, err := s.OpenRPCSessionsByMember(ctx, "srv_none"); err != nil || none == nil || len(none) != 0 {
			t.Fatalf("no sessions = %v err=%v, want an empty list", none, err)
		}
		all, err := s.OpenRPCSessions(ctx)
		if err != nil || len(all) != 3 || all[0].MemberServerID != "srv_a" || all[0].HeadSpecID != "rspec_a" ||
			all[1].HeadSpecID != "rspec_b" || all[2].MemberServerID != "srv_b" {
			t.Fatalf("all open sessions = %+v err=%v, want srv_a's two then srv_b's", all, err)
		}
	})
}

// TestConformanceRPCClusterMemberPortUnique pins the (server, port) rule on
// every routing store: a non-zero port is unique on its server across heads,
// a head rewriting its own rows keeps its ports, port 0 repeats freely, and
// the same port on another server is fine.
func TestConformanceRPCClusterMemberPortUnique(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "p1", now)
		head2 := seedRPCHeadSpec(ctx, t, s, "p2", now)
		seedBinariesServer(ctx, t, s, "srv_pm1", now)
		seedBinariesServer(ctx, t, s, "srv_pm2", now)
		row := func(h, srv string, port int) []routing.ClusterMember {
			return []routing.ClusterMember{{HeadSpecID: h, Position: 0, ServerID: srv, DeviceBackend: "auto", Port: port}}
		}
		if err := s.SetClusterMembers(ctx, head, row(head, "srv_pm1", 47000), nil); err != nil {
			t.Fatalf("first head: %v", err)
		}
		if err := s.SetClusterMembers(ctx, head, row(head, "srv_pm1", 47000), nil); err != nil {
			t.Fatalf("rewrite of the same head: %v", err)
		}
		if err := s.SetClusterMembers(ctx, head2, row(head2, "srv_pm1", 47000), nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("same port on the same server: err=%v, want ErrConflict", err)
		}
		if em, _, _ := s.ClusterMembers(ctx, head2); len(em) != 0 {
			t.Fatalf("a refused write stored rows: %+v", em)
		}
		if err := s.SetClusterMembers(ctx, head2, row(head2, "srv_pm2", 47000), nil); err != nil {
			t.Fatalf("same port on another server: %v", err)
		}
		if err := s.SetClusterMembers(ctx, head, row(head, "srv_pm2", 0), nil); err != nil {
			t.Fatalf("port 0 next to a used port: %v", err)
		}
		head3 := seedRPCHeadSpec(ctx, t, s, "p3", now)
		if err := s.SetClusterMembers(ctx, head3, row(head3, "srv_pm1", 0), nil); err != nil {
			t.Fatalf("port 0 repeats: %v", err)
		}
		// Port 47000 on srv_pm1 is free again now that the first head moved.
		if err := s.SetClusterMembers(ctx, head2, row(head2, "srv_pm1", 47000), nil); err != nil {
			t.Fatalf("freed port: %v", err)
		}
	})
}
