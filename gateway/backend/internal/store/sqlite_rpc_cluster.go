// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

func timeOrZero(n sql.NullTime) time.Time {
	if n.Valid {
		return n.Time
	}
	return time.Time{}
}

// classifyRPCWrite maps a constraint violation on a cluster write: a
// foreign-key violation (an unknown parent) is ErrNotFound, a unique violation
// ErrConflict. The foreign-key check goes first because SQLite's unique
// classifier matches any "constraint failed" message, foreign keys included.
func (s *SQLStore) classifyRPCWrite(err error, what string) error {
	switch {
	case s.dl.isForeignKeyViolation(err):
		return ErrNotFound
	case s.dl.isUniqueViolation(err):
		return ErrConflict
	}
	return fmt.Errorf("%s: %w", what, err)
}

const rpcMemberSpecColumns = `id, server_id, enabled, binary_path, args, env, cache_enabled, device_backend,
	ram_estimate_mb, allowed_head_groups, created_at, updated_at`

func scanRPCMemberSpec(row rowScanner) (routing.RPCMemberSpec, error) {
	var sp routing.RPCMemberSpec
	var enabled, cache int64
	err := row.Scan(&sp.ID, &sp.ServerID, &enabled, &sp.Binary, &sp.Args, &sp.Env, &cache, &sp.DeviceBackend,
		&sp.RAMEstimateMB, &sp.AllowedHeadGroups, &sp.CreatedAt, &sp.UpdatedAt)
	if err != nil {
		return routing.RPCMemberSpec{}, err
	}
	sp.Enabled, sp.CacheEnabled = enabled != 0, cache != 0
	return sp, nil
}

// UpsertRPCMemberSpec inserts or updates a manual member spec by id; an update
// keeps created_at. An unknown server is ErrNotFound.
func (s *SQLiteStore) UpsertRPCMemberSpec(ctx context.Context, sp routing.RPCMemberSpec) error {
	_, err := s.exec(ctx, `
		insert into rpc_member_specs (`+rpcMemberSpecColumns+`)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(id) do update set
			server_id = excluded.server_id, enabled = excluded.enabled, binary_path = excluded.binary_path,
			args = excluded.args, env = excluded.env, cache_enabled = excluded.cache_enabled,
			device_backend = excluded.device_backend, ram_estimate_mb = excluded.ram_estimate_mb,
			allowed_head_groups = excluded.allowed_head_groups, updated_at = excluded.updated_at`,
		sp.ID, sp.ServerID, sp.Enabled, sp.Binary, sp.Args, sp.Env, sp.CacheEnabled, sp.DeviceBackend,
		sp.RAMEstimateMB, sp.AllowedHeadGroups, sp.CreatedAt, sp.UpdatedAt)
	if err != nil {
		return s.classifyRPCWrite(err, "upsert rpc member spec")
	}
	return nil
}

// RPCMemberSpecByID returns the spec; ok is false when it does not exist.
func (s *SQLiteStore) RPCMemberSpecByID(ctx context.Context, id string) (routing.RPCMemberSpec, bool, error) {
	sp, err := scanRPCMemberSpec(s.queryRow(ctx, `select `+rpcMemberSpecColumns+` from rpc_member_specs where id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return routing.RPCMemberSpec{}, false, nil
	}
	if err != nil {
		return routing.RPCMemberSpec{}, false, fmt.Errorf("read rpc member spec: %w", err)
	}
	return sp, true, nil
}

// RPCMemberSpecsByServer lists serverID's member specs ordered by id.
func (s *SQLiteStore) RPCMemberSpecsByServer(ctx context.Context, serverID string) ([]routing.RPCMemberSpec, error) {
	rows, err := s.query(ctx, `select `+rpcMemberSpecColumns+` from rpc_member_specs where server_id = ? order by id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list rpc member specs: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RPCMemberSpec, 0)
	for rows.Next() {
		sp, err := scanRPCMemberSpec(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rpc member spec: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rpc member specs: %w", err)
	}
	return out, nil
}

// DeleteRPCMemberSpec removes the spec (its GPU rows cascade); a missing spec
// is ErrNotFound.
func (s *SQLiteStore) DeleteRPCMemberSpec(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from rpc_member_specs where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete rpc member spec: %w", err)
	}
	return requireAffected(res)
}

// SetRPCMemberSpecGPUs atomically replaces specID's GPU rows. An unknown spec
// is ErrNotFound, a duplicate gpu_index ErrConflict.
func (s *SQLiteStore) SetRPCMemberSpecGPUs(ctx context.Context, specID string, gpus []routing.RPCMemberSpecGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set rpc member spec gpus: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from rpc_member_specs where id = ?`), specID).Scan(&exists); err != nil {
		return fmt.Errorf("check rpc member spec: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := s.execOn(ctx, tx, `delete from rpc_member_spec_gpus where spec_id = ?`, specID); err != nil {
		return fmt.Errorf("clear rpc member spec gpus: %w", err)
	}
	for _, g := range gpus {
		if _, err := s.execOn(ctx, tx, `
			insert into rpc_member_spec_gpus (spec_id, gpu_index, position, vram_estimate_mb)
			values (?, ?, ?, ?)`, specID, g.GPUIndex, g.Position, g.VRAMEstimateMB); err != nil {
			return s.classifyRPCWrite(err, "insert rpc member spec gpu")
		}
	}
	return tx.Commit()
}

// RPCMemberSpecGPUs lists specID's GPU rows ordered by position, then index.
func (s *SQLiteStore) RPCMemberSpecGPUs(ctx context.Context, specID string) ([]routing.RPCMemberSpecGPU, error) {
	rows, err := s.query(ctx, `
		select spec_id, gpu_index, position, vram_estimate_mb
		from rpc_member_spec_gpus where spec_id = ? order by position, gpu_index`, specID)
	if err != nil {
		return nil, fmt.Errorf("list rpc member spec gpus: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RPCMemberSpecGPU, 0)
	for rows.Next() {
		var g routing.RPCMemberSpecGPU
		if err := rows.Scan(&g.SpecID, &g.GPUIndex, &g.Position, &g.VRAMEstimateMB); err != nil {
			return nil, fmt.Errorf("scan rpc member spec gpu: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rpc member spec gpus: %w", err)
	}
	return out, nil
}

// SetClusterMembers atomically replaces headSpecID's membership and GPU rows.
// A failed insert rolls the whole replace back. An unknown head or member
// server is ErrNotFound (foreign key); a duplicate position, server, or a
// manual member spec already used elsewhere is ErrConflict (unique).
func (s *SQLiteStore) SetClusterMembers(ctx context.Context, headSpecID string, members []routing.ClusterMember, gpus []routing.ClusterMemberGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set cluster members: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from agent_runtime_specs where id = ?`), headSpecID).Scan(&exists); err != nil {
		return fmt.Errorf("check head spec: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	// The GPU rows cascade from their membership row.
	if _, err := s.execOn(ctx, tx, `delete from rpc_cluster_members where head_spec_id = ?`, headSpecID); err != nil {
		return fmt.Errorf("clear cluster members: %w", err)
	}
	for _, m := range members {
		if _, err := s.execOn(ctx, tx, `
			insert into rpc_cluster_members (head_spec_id, position, server_id, member_spec_id, threads, cache_enabled,
				device_backend, ram_estimate_mb, port, ram_measured_mb, measured_at, stale, run_peak_ram_mb,
				run_head_instance, run_started_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			headSpecID, m.Position, m.ServerID, m.MemberSpecID, m.Threads, m.CacheEnabled, m.DeviceBackend,
			m.RAMEstimateMB, m.Port, m.RAMMeasuredMB, nullableTime(m.MeasuredAt), m.Stale, m.RunPeakRAMMB,
			m.RunHeadInstance, nullableTime(m.RunStartedAt)); err != nil {
			return s.classifyRPCWrite(err, "insert cluster member")
		}
	}
	for _, g := range gpus {
		if _, err := s.execOn(ctx, tx, `
			insert into rpc_cluster_member_gpus (head_spec_id, server_id, gpu_index, position, vram_estimate_mb,
				vram_measured_mb, run_peak_vram_mb)
			values (?, ?, ?, ?, ?, ?, ?)`,
			headSpecID, g.ServerID, g.GPUIndex, g.Position, g.VRAMEstimateMB, g.VRAMMeasuredMB, g.RunPeakVRAMMB); err != nil {
			return s.classifyRPCWrite(err, "insert cluster member gpu")
		}
	}
	return tx.Commit()
}

const clusterMemberColumns = `head_spec_id, position, server_id, member_spec_id, threads, cache_enabled, device_backend,
	ram_estimate_mb, port, ram_measured_mb, measured_at, stale, run_peak_ram_mb, run_head_instance, run_started_at`

func scanClusterMember(row rowScanner) (routing.ClusterMember, error) {
	var m routing.ClusterMember
	var cache, stale int64
	var measuredAt, runStartedAt sql.NullTime
	err := row.Scan(&m.HeadSpecID, &m.Position, &m.ServerID, &m.MemberSpecID, &m.Threads, &cache, &m.DeviceBackend,
		&m.RAMEstimateMB, &m.Port, &m.RAMMeasuredMB, &measuredAt, &stale, &m.RunPeakRAMMB, &m.RunHeadInstance, &runStartedAt)
	if err != nil {
		return routing.ClusterMember{}, err
	}
	m.CacheEnabled, m.Stale = cache != 0, stale != 0
	m.MeasuredAt, m.RunStartedAt = timeOrZero(measuredAt), timeOrZero(runStartedAt)
	return m, nil
}

func (s *SQLiteStore) clusterMembersWhere(ctx context.Context, where, order string, arg any) ([]routing.ClusterMember, error) {
	rows, err := s.query(ctx, `select `+clusterMemberColumns+` from rpc_cluster_members where `+where+` order by `+order, arg)
	if err != nil {
		return nil, fmt.Errorf("list cluster members: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ClusterMember, 0)
	for rows.Next() {
		m, err := scanClusterMember(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster members: %w", err)
	}
	return out, nil
}

// ClusterMembers returns headSpecID's membership by position and its GPU rows
// by server, position, index.
func (s *SQLiteStore) ClusterMembers(ctx context.Context, headSpecID string) ([]routing.ClusterMember, []routing.ClusterMemberGPU, error) {
	members, err := s.clusterMembersWhere(ctx, `head_spec_id = ?`, `position`, headSpecID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.query(ctx, `
		select head_spec_id, server_id, gpu_index, position, vram_estimate_mb, vram_measured_mb, run_peak_vram_mb
		from rpc_cluster_member_gpus where head_spec_id = ? order by server_id, position, gpu_index`, headSpecID)
	if err != nil {
		return nil, nil, fmt.Errorf("list cluster member gpus: %w", err)
	}
	defer rows.Close()
	gpus := make([]routing.ClusterMemberGPU, 0)
	for rows.Next() {
		var g routing.ClusterMemberGPU
		if err := rows.Scan(&g.HeadSpecID, &g.ServerID, &g.GPUIndex, &g.Position, &g.VRAMEstimateMB, &g.VRAMMeasuredMB, &g.RunPeakVRAMMB); err != nil {
			return nil, nil, fmt.Errorf("scan cluster member gpu: %w", err)
		}
		gpus = append(gpus, g)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate cluster member gpus: %w", err)
	}
	return members, gpus, nil
}

// ClusterMembershipsByServer is the reverse index, ordered by head spec id.
func (s *SQLiteStore) ClusterMembershipsByServer(ctx context.Context, serverID string) ([]routing.ClusterMember, error) {
	return s.clusterMembersWhere(ctx, `server_id = ?`, `head_spec_id`, serverID)
}

// ClusterMemberBySpec returns the membership row using the manual member spec.
func (s *SQLiteStore) ClusterMemberBySpec(ctx context.Context, memberSpecID string) (routing.ClusterMember, bool, error) {
	if memberSpecID == "" {
		return routing.ClusterMember{}, false, nil
	}
	m, err := scanClusterMember(s.queryRow(ctx, `select `+clusterMemberColumns+` from rpc_cluster_members where member_spec_id = ?`, memberSpecID))
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ClusterMember{}, false, nil
	}
	if err != nil {
		return routing.ClusterMember{}, false, fmt.Errorf("read cluster member by spec: %w", err)
	}
	return m, true, nil
}

// UpdateClusterMemberUsage writes only the measured / run-peak fields of the
// membership row m and of each given GPU row, in one transaction. A missing
// membership row is ErrNotFound; a GPU row that does not exist is skipped (the
// usage of a card that left the membership is of no interest).
func (s *SQLiteStore) UpdateClusterMemberUsage(ctx context.Context, m routing.ClusterMember, gpus []routing.ClusterMemberGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update cluster member usage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := s.execOn(ctx, tx, `
		update rpc_cluster_members set ram_measured_mb = ?, measured_at = ?, stale = ?, run_peak_ram_mb = ?,
			run_head_instance = ?, run_started_at = ?
		where head_spec_id = ? and server_id = ?`,
		m.RAMMeasuredMB, nullableTime(m.MeasuredAt), m.Stale, m.RunPeakRAMMB, m.RunHeadInstance,
		nullableTime(m.RunStartedAt), m.HeadSpecID, m.ServerID)
	if err != nil {
		return fmt.Errorf("update cluster member usage: %w", err)
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	for _, g := range gpus {
		if _, err := s.execOn(ctx, tx, `
			update rpc_cluster_member_gpus set vram_measured_mb = ?, run_peak_vram_mb = ?
			where head_spec_id = ? and server_id = ? and gpu_index = ?`,
			g.VRAMMeasuredMB, g.RunPeakVRAMMB, m.HeadSpecID, m.ServerID, g.GPUIndex); err != nil {
			return fmt.Errorf("update cluster member gpu usage: %w", err)
		}
	}
	return tx.Commit()
}

// OpenRPCSession appends an open session.
func (s *SQLiteStore) OpenRPCSession(ctx context.Context, sess routing.RPCSession) error {
	var until any
	if sess.Until != nil {
		until = *sess.Until
	}
	if _, err := s.exec(ctx, `
		insert into rpc_sessions (head_spec_id, instance, member_server_id, since, until) values (?, ?, ?, ?, ?)`,
		sess.HeadSpecID, sess.Instance, sess.MemberServerID, sess.Since, until); err != nil {
		return fmt.Errorf("open rpc session: %w", err)
	}
	return nil
}

// CloseRPCSessions sets until on every open session of (head, instance,
// member); closing none is not an error.
func (s *SQLiteStore) CloseRPCSessions(ctx context.Context, headSpecID string, instance int, memberServerID string, until time.Time) error {
	if _, err := s.exec(ctx, `
		update rpc_sessions set until = ?
		where head_spec_id = ? and instance = ? and member_server_id = ? and until is null`,
		until, headSpecID, instance, memberServerID); err != nil {
		return fmt.Errorf("close rpc sessions: %w", err)
	}
	return nil
}

// RPCSessionsAt lists the sessions of (head, instance) in force at at.
func (s *SQLiteStore) RPCSessionsAt(ctx context.Context, headSpecID string, instance int, at time.Time) ([]routing.RPCSession, error) {
	rows, err := s.query(ctx, `
		select head_spec_id, instance, member_server_id, since, until
		from rpc_sessions
		where head_spec_id = ? and instance = ? and since <= ? and (until is null or until >= ?)
		order by since, member_server_id`, headSpecID, instance, at, at)
	if err != nil {
		return nil, fmt.Errorf("list rpc sessions: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RPCSession, 0)
	for rows.Next() {
		var sess routing.RPCSession
		var until sql.NullTime
		if err := rows.Scan(&sess.HeadSpecID, &sess.Instance, &sess.MemberServerID, &sess.Since, &until); err != nil {
			return nil, fmt.Errorf("scan rpc session: %w", err)
		}
		if until.Valid {
			u := until.Time
			sess.Until = &u
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rpc sessions: %w", err)
	}
	return out, nil
}
