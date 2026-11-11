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

// TestConformanceReplaceRPCMemberSpec pins the one-transaction member spec
// write: the spec and its GPU rows land together, and a refused GPU set
// leaves the stored spec as it was.
func TestConformanceReplaceRPCMemberSpec(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		seedBinariesServer(ctx, t, s, "srv_rr", now)
		spec := routing.RPCMemberSpec{
			ID: "rpcm_r", ServerID: "srv_rr", Enabled: true, Binary: "/opt/rpc-server", Args: "[]", Env: "{}",
			DeviceBackend: "auto", AllowedHeadGroups: "[]", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.ReplaceRPCMemberSpec(ctx, routing.RPCMemberSpec{ID: "rpcm_x", ServerID: "srv_nope", CreatedAt: now, UpdatedAt: now}, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown server: err=%v, want ErrNotFound", err)
		}
		gpus := []routing.RPCMemberSpecGPU{{SpecID: "rpcm_r", GPUIndex: 1, Position: 0, VRAMEstimateMB: 4000}, {SpecID: "rpcm_r", GPUIndex: 0, Position: 1}}
		if err := s.ReplaceRPCMemberSpec(ctx, spec, gpus); err != nil {
			t.Fatalf("replace: %v", err)
		}
		got, ok, err := s.RPCMemberSpecByID(ctx, "rpcm_r")
		if err != nil || !ok || got.Binary != "/opt/rpc-server" {
			t.Fatalf("spec = %+v ok=%v err=%v", got, ok, err)
		}
		rows, err := s.RPCMemberSpecGPUs(ctx, "rpcm_r")
		if err != nil || len(rows) != 2 || rows[0].GPUIndex != 1 || rows[0].VRAMEstimateMB != 4000 || rows[1].GPUIndex != 0 {
			t.Fatalf("gpus = %+v err=%v", rows, err)
		}
		changed := spec
		changed.Binary, changed.UpdatedAt = "/opt/other-rpc-server", now.Add(time.Hour)
		dup := []routing.RPCMemberSpecGPU{{SpecID: "rpcm_r", GPUIndex: 2}, {SpecID: "rpcm_r", GPUIndex: 2, Position: 1}}
		if err := s.ReplaceRPCMemberSpec(ctx, changed, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate gpu index: err=%v, want ErrConflict", err)
		}
		if got, _, _ := s.RPCMemberSpecByID(ctx, "rpcm_r"); got.Binary != "/opt/rpc-server" {
			t.Fatalf("a refused replace changed the spec: %+v", got)
		}
		if rows, _ := s.RPCMemberSpecGPUs(ctx, "rpcm_r"); len(rows) != 2 {
			t.Fatalf("a refused replace changed the gpus: %+v", rows)
		}
		if err := s.ReplaceRPCMemberSpec(ctx, changed, nil); err != nil {
			t.Fatalf("replace without gpus: %v", err)
		}
		if rows, _ := s.RPCMemberSpecGPUs(ctx, "rpcm_r"); len(rows) != 0 {
			t.Fatalf("gpus after an empty replace = %+v", rows)
		}
		if got, _, _ := s.RPCMemberSpecByID(ctx, "rpcm_r"); got.Binary != "/opt/other-rpc-server" || !got.CreatedAt.Equal(now) {
			t.Fatalf("spec after replace = %+v, want the new binary and the first created_at", got)
		}
	})
}

// TestConformanceSetClusterMembersIf pins the conditional membership
// rewrite: it applies over the rows the caller read and stores nothing once
// a usage write, a run begun or another rewrite has changed them.
func TestConformanceSetClusterMembersIf(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		head := seedRPCHeadSpec(ctx, t, s, "cif", now)
		seedBinariesServer(ctx, t, s, "srv_c1", now)
		seedBinariesServer(ctx, t, s, "srv_c2", now)
		members := []routing.ClusterMember{
			{HeadSpecID: head, Position: 0, ServerID: "srv_c1", Port: 26001},
			{HeadSpecID: head, Position: 1, ServerID: "srv_c2", Port: 26001},
		}
		gpus := []routing.ClusterMemberGPU{{HeadSpecID: head, ServerID: "srv_c1", GPUIndex: 0}}
		if applied, err := s.SetClusterMembersIf(ctx, "rspec_nope", nil, nil, members, gpus); !errors.Is(err, ErrNotFound) || applied {
			t.Fatalf("unknown head: applied=%v err=%v, want ErrNotFound", applied, err)
		}
		// First write over an empty membership.
		if applied, err := s.SetClusterMembersIf(ctx, head, nil, nil, members, gpus); err != nil || !applied {
			t.Fatalf("first write: applied=%v err=%v", applied, err)
		}
		read := func() ([]routing.ClusterMember, []routing.ClusterMemberGPU) {
			t.Helper()
			m, g, err := s.ClusterMembers(ctx, head)
			if err != nil {
				t.Fatal(err)
			}
			return m, g
		}
		readM, readG := read()
		// A stale read of an empty membership no longer applies.
		if applied, err := s.SetClusterMembersIf(ctx, head, nil, nil, members[:1], nil); err != nil || applied {
			t.Fatalf("write over a changed membership: applied=%v err=%v", applied, err)
		}

		// A run begins and the gateway writes usage after the read.
		run := now.Add(time.Minute).Truncate(time.Millisecond)
		if err := s.BeginClusterMemberRun(ctx, head, run, 0); err != nil {
			t.Fatal(err)
		}
		next := []routing.ClusterMember{{HeadSpecID: head, Position: 0, ServerID: "srv_c1", Port: 26001, Threads: 8}}
		if applied, err := s.SetClusterMembersIf(ctx, head, readM, readG, next, nil); err != nil || applied {
			t.Fatalf("write over a begun run: applied=%v err=%v", applied, err)
		}
		readM, readG = read()
		upd := readM[0]
		upd.RAMMeasuredMB, upd.RunPeakRAMMB = 4000, 4000
		if applied, err := s.UpdateClusterMemberUsage(ctx, upd, nil); err != nil || !applied {
			t.Fatalf("usage write: applied=%v err=%v", applied, err)
		}
		if applied, err := s.SetClusterMembersIf(ctx, head, readM, readG, next, nil); err != nil || applied {
			t.Fatalf("write over a usage write: applied=%v err=%v", applied, err)
		}
		readM, readG = read()
		g := readG[0]
		if applied, err := s.UpdateClusterMemberUsage(ctx, readM[0], []routing.ClusterMemberGPU{{GPUIndex: g.GPUIndex, VRAMMeasuredMB: 9000, RunPeakVRAMMB: 9000}}); err != nil || !applied {
			t.Fatalf("gpu usage write: applied=%v err=%v", applied, err)
		}
		if applied, err := s.SetClusterMembersIf(ctx, head, readM, readG, next, nil); err != nil || applied {
			t.Fatalf("write over a gpu usage write: applied=%v err=%v", applied, err)
		}
		if m, g := read(); len(m) != 2 || len(g) != 1 || g[0].VRAMMeasuredMB != 9000 || m[0].RAMMeasuredMB != 4000 {
			t.Fatalf("a refused write changed the rows: %+v %+v", m, g)
		}

		// Over a fresh read it applies.
		readM, readG = read()
		if applied, err := s.SetClusterMembersIf(ctx, head, readM, readG, next, nil); err != nil || !applied {
			t.Fatalf("write over a fresh read: applied=%v err=%v", applied, err)
		}
		if m, g := read(); len(m) != 1 || m[0].Threads != 8 || len(g) != 0 {
			t.Fatalf("rows after the write = %+v %+v", m, g)
		}
	})
}
