// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"op-ai-gateway/internal/routing"
)

// ReplaceRPCMemberSpec upserts the spec and replaces its GPU rows in one
// transaction (see the interface).
func (s *SQLiteStore) ReplaceRPCMemberSpec(ctx context.Context, sp routing.RPCMemberSpec, gpus []routing.RPCMemberSpecGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace rpc member spec: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.upsertRPCMemberSpecOn(ctx, tx, sp); err != nil {
		return err
	}
	if err := s.setRPCMemberSpecGPUsOn(ctx, tx, sp.ID, gpus); err != nil {
		return err
	}
	return tx.Commit()
}

// SetClusterMembersIf replaces headSpecID's membership while the stored rows
// are still the ones the caller read (see the interface). The comparison is
// a set of no-op updates that match only an unchanged row: they run first,
// so the transaction holds the rows' write locks (SQLite: the database's)
// from its first statement on, and a usage write cannot slip in between the
// comparison and the replace.
func (s *SQLiteStore) SetClusterMembersIf(ctx context.Context, headSpecID string, expect []routing.ClusterMember, expectGPUs []routing.ClusterMemberGPU, members []routing.ClusterMember, gpus []routing.ClusterMemberGPU) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin set cluster members if: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	same, err := s.clusterMembersAsRead(ctx, tx, headSpecID, expect, expectGPUs)
	if err != nil {
		return false, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from agent_runtime_specs where id = ?`), headSpecID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check head spec: %w", err)
	}
	if exists == 0 {
		return false, ErrNotFound
	}
	if !same {
		return false, nil
	}
	if err := s.setClusterMembersOn(ctx, tx, headSpecID, members, gpus); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit set cluster members if: %w", err)
	}
	return true, nil
}

// clusterMembersAsRead reports, inside tx, whether headSpecID's stored rows
// carry exactly the measured and run fields of expect and expectGPUs.
func (s *SQLiteStore) clusterMembersAsRead(ctx context.Context, tx *sql.Tx, headSpecID string, expect []routing.ClusterMember, expectGPUs []routing.ClusterMemberGPU) (bool, error) {
	for _, e := range expect {
		where, args := inRun(headSpecID, e.ServerID, e.RunStartedAt)
		res, err := s.execOn(ctx, tx, `
			update rpc_cluster_members set port = port
			where ram_measured_mb = ? and stale = ? and run_peak_ram_mb = ? and run_head_instance = ? and `+where,
			append([]any{e.RAMMeasuredMB, e.Stale, e.RunPeakRAMMB, e.RunHeadInstance}, args...)...)
		if err != nil {
			return false, fmt.Errorf("compare cluster member: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return false, err
		}
	}
	for _, g := range expectGPUs {
		res, err := s.execOn(ctx, tx, `
			update rpc_cluster_member_gpus set position = position
			where head_spec_id = ? and server_id = ? and gpu_index = ? and vram_measured_mb = ? and run_peak_vram_mb = ?`,
			headSpecID, g.ServerID, g.GPUIndex, g.VRAMMeasuredMB, g.RunPeakVRAMMB)
		if err != nil {
			return false, fmt.Errorf("compare cluster member gpu: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return false, err
		}
	}
	var members, memberGPUs int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from rpc_cluster_members where head_spec_id = ?`), headSpecID).Scan(&members); err != nil {
		return false, fmt.Errorf("count cluster members: %w", err)
	}
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from rpc_cluster_member_gpus where head_spec_id = ?`), headSpecID).Scan(&memberGPUs); err != nil {
		return false, fmt.Errorf("count cluster member gpus: %w", err)
	}
	return members == len(expect) && memberGPUs == len(expectGPUs), nil
}
