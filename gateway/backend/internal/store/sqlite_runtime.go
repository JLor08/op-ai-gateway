// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Runtime-manager persistence (T1): launch specs, per-GPU demand rows, the
// co-residency matrix (Task 2), GPU budgets (Task 3), runtime reports (Task 4).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
)

// UpsertRuntimeSpec inserts or replaces the spec for spec.MappingID. Note:
// the binary_path column backs the RuntimeSpec.Binary field — it is not
// named `binary` because BINARY is a reserved PostgreSQL keyword (see
// migration65Up's doc comment).
func (s *SQLiteStore) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	_, err := s.exec(ctx, `
		insert into agent_runtime_specs (
			id, mapping_id, enabled, binary_path, args, env, work_dir, listen_port,
			health_path, health_timeout_seconds, startup_timeout_seconds,
			idle_timeout_seconds, admission_wait_timeout_seconds, pinned,
			admin_state, vram_locked, created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(mapping_id) do update set
			enabled = excluded.enabled, binary_path = excluded.binary_path,
			args = excluded.args, env = excluded.env, work_dir = excluded.work_dir,
			listen_port = excluded.listen_port, health_path = excluded.health_path,
			health_timeout_seconds = excluded.health_timeout_seconds,
			startup_timeout_seconds = excluded.startup_timeout_seconds,
			idle_timeout_seconds = excluded.idle_timeout_seconds,
			admission_wait_timeout_seconds = excluded.admission_wait_timeout_seconds,
			pinned = excluded.pinned, admin_state = excluded.admin_state,
			vram_locked = excluded.vram_locked, updated_at = excluded.updated_at`,
		spec.ID, spec.MappingID, spec.Enabled, spec.Binary, spec.Args, spec.Env,
		spec.WorkDir, spec.ListenPort, spec.HealthPath, spec.HealthTimeoutSeconds,
		spec.StartupTimeoutSeconds, spec.IdleTimeoutSeconds,
		spec.AdmissionWaitTimeoutSeconds, spec.Pinned, spec.AdminState,
		spec.VRAMLocked, spec.CreatedAt, spec.UpdatedAt,
	)
	if err != nil {
		// FK before unique: sqlite's FK error text also matches the
		// unique-violation substring (see sqlite_projects.go).
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert runtime spec: %w", err)
	}
	return nil
}

const runtimeSpecCols = `id, mapping_id, enabled, binary_path, args, env, work_dir,
	listen_port, health_path, health_timeout_seconds, startup_timeout_seconds,
	idle_timeout_seconds, admission_wait_timeout_seconds, pinned, admin_state,
	vram_locked, created_at, updated_at`

// runtimeSpecColsPrefixed is runtimeSpecCols qualified with the `s` alias, for
// the RuntimeSpecsByApplication join below where an unqualified column list
// would be ambiguous against the joined model_mappings table (mirrors
// projectColsPrefixed in sqlite_projects.go).
const runtimeSpecColsPrefixed = `s.id, s.mapping_id, s.enabled, s.binary_path, s.args, s.env, s.work_dir,
	s.listen_port, s.health_path, s.health_timeout_seconds, s.startup_timeout_seconds,
	s.idle_timeout_seconds, s.admission_wait_timeout_seconds, s.pinned, s.admin_state,
	s.vram_locked, s.created_at, s.updated_at`

func (s *SQLiteStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	row := s.queryRow(ctx, `select `+runtimeSpecCols+` from agent_runtime_specs where mapping_id = ?`, mappingID)
	spec, err := scanRuntimeSpec(row)
	if errors.Is(err, ErrNotFound) {
		return routing.RuntimeSpec{}, false, nil
	}
	if err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	return spec, true, nil
}

func (s *SQLiteStore) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	rows, err := s.query(ctx, `
		select `+runtimeSpecColsPrefixed+`
		from agent_runtime_specs s
		join model_mappings m on m.id = s.mapping_id
		where m.application_id = ?
		order by s.id`, appID)
	if err != nil {
		return nil, fmt.Errorf("list runtime specs: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RuntimeSpec, 0)
	for rows.Next() {
		spec, err := scanRuntimeSpec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime specs: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteRuntimeSpec(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `delete from agent_runtime_specs where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete runtime spec: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set spec gpus: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from agent_runtime_specs where id = ?`), specID).Scan(&exists); err != nil {
		return fmt.Errorf("check spec: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from agent_runtime_spec_gpus where spec_id = ?`), specID); err != nil {
		return fmt.Errorf("clear spec gpus: %w", err)
	}
	for _, g := range gpus {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb)
			values (?, ?, ?, ?)`), specID, g.GPUIndex, g.VRAMEstimateMB, g.VRAMMeasuredMB); err != nil {
			return fmt.Errorf("insert spec gpu: %w", err)
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) RuntimeSpecGPUs(ctx context.Context, specID string) ([]routing.RuntimeSpecGPU, error) {
	rows, err := s.query(ctx, `
		select spec_id, gpu_index, vram_estimate_mb, vram_measured_mb
		from agent_runtime_spec_gpus where spec_id = ? order by gpu_index`, specID)
	if err != nil {
		return nil, fmt.Errorf("list spec gpus: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RuntimeSpecGPU, 0)
	for rows.Next() {
		var g routing.RuntimeSpecGPU
		if err := rows.Scan(&g.SpecID, &g.GPUIndex, &g.VRAMEstimateMB, &g.VRAMMeasuredMB); err != nil {
			return nil, fmt.Errorf("scan spec gpu: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spec gpus: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex int, measuredMB int) error {
	result, err := s.exec(ctx, `
		update agent_runtime_spec_gpus set vram_measured_mb = ?
		where spec_id = ? and gpu_index = ?`, measuredMB, specID, gpuIndex)
	if err != nil {
		return fmt.Errorf("update measured vram: %w", err)
	}
	return requireAffected(result)
}

func scanRuntimeSpec(row rowScanner) (routing.RuntimeSpec, error) {
	var spec routing.RuntimeSpec
	var enabled, pinned, vramLocked int64
	err := row.Scan(&spec.ID, &spec.MappingID, &enabled, &spec.Binary, &spec.Args,
		&spec.Env, &spec.WorkDir, &spec.ListenPort, &spec.HealthPath,
		&spec.HealthTimeoutSeconds, &spec.StartupTimeoutSeconds,
		&spec.IdleTimeoutSeconds, &spec.AdmissionWaitTimeoutSeconds, &pinned,
		&spec.AdminState, &vramLocked, &spec.CreatedAt, &spec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.RuntimeSpec{}, ErrNotFound
	}
	if err != nil {
		return routing.RuntimeSpec{}, fmt.Errorf("scan runtime spec: %w", err)
	}
	spec.Enabled, spec.Pinned, spec.VRAMLocked = enabled != 0, pinned != 0, vramLocked != 0
	return spec, nil
}
