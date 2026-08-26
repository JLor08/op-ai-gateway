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
			// A duplicate GPUIndex within gpus hits the composite primary key
			// (spec_id, gpu_index) here — a unique violation, not an FK
			// violation (specID was already existence-checked above, and a
			// GPU row has no other FK column), so only isUniqueViolation
			// applies (mirrors UpsertRuntimeSpec/SetCoResidencyRules' error
			// classification in this file).
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
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

// SetCoResidencyRules atomically REPLACES the whole set of allowed
// co-residency pairs for appID (delete-then-insert in one transaction,
// mirroring SetRuntimeSpecGPUs above / SetGroupMembers). appID must exist —
// checked inside the transaction, before the delete — or ErrNotFound. The
// store does not enforce or rewrite the MappingAID < MappingBID canonical
// ordering; that is portal-level validation (Task 6).
func (s *SQLiteStore) SetCoResidencyRules(ctx context.Context, appID string, rules []routing.CoResidencyRule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set coresidency rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from applications where id = ?`), appID).Scan(&exists); err != nil {
		return fmt.Errorf("check application: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from agent_coresidency_rules where application_id = ?`), appID); err != nil {
		return fmt.Errorf("clear coresidency rules: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into agent_coresidency_rules (application_id, mapping_a_id, mapping_b_id, created_at)
			values (?, ?, ?, ?)`), appID, r.MappingAID, r.MappingBID, r.CreatedAt); err != nil {
			// FK before unique: sqlite's FK error text also matches the
			// unique-violation substring (see UpsertRuntimeSpec above /
			// sqlite_projects.go).
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert coresidency rule: %w", err)
		}
	}
	return tx.Commit()
}

// CoResidencyRulesByApplication lists appID's allowed co-residency pairs,
// ordered by mapping_a_id then mapping_b_id. Always non-nil, empty when none.
func (s *SQLiteStore) CoResidencyRulesByApplication(ctx context.Context, appID string) ([]routing.CoResidencyRule, error) {
	rows, err := s.query(ctx, `
		select application_id, mapping_a_id, mapping_b_id, created_at
		from agent_coresidency_rules where application_id = ? order by mapping_a_id, mapping_b_id`, appID)
	if err != nil {
		return nil, fmt.Errorf("list coresidency rules: %w", err)
	}
	defer rows.Close()
	out := make([]routing.CoResidencyRule, 0)
	for rows.Next() {
		var r routing.CoResidencyRule
		if err := rows.Scan(&r.ApplicationID, &r.MappingAID, &r.MappingBID, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan coresidency rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate coresidency rules: %w", err)
	}
	return out, nil
}

// SetServerGPUBudgets atomically REPLACES the whole set of per-GPU VRAM
// budgets for serverID (delete-then-insert in one transaction, mirroring
// SetRuntimeSpecGPUs/SetCoResidencyRules above). serverID must exist —
// checked inside the transaction, before the delete — or ErrNotFound.
func (s *SQLiteStore) SetServerGPUBudgets(ctx context.Context, serverID string, budgets []routing.ServerGPUBudget) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set gpu budgets: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from ai_servers where id = ?`), serverID).Scan(&exists); err != nil {
		return fmt.Errorf("check server: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from ai_server_gpu_budgets where server_id = ?`), serverID); err != nil {
		return fmt.Errorf("clear gpu budgets: %w", err)
	}
	for _, b := range budgets {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into ai_server_gpu_budgets (server_id, gpu_index, budget_mb, expected_uuid, expected_name, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?)`), serverID, b.GPUIndex, b.BudgetMB, b.ExpectedUUID, b.ExpectedName, b.CreatedAt, b.UpdatedAt); err != nil {
			// A duplicate GPUIndex within budgets hits the composite primary
			// key (server_id, gpu_index) here — a unique violation, not an FK
			// violation (serverID was already existence-checked above, and a
			// budget row has no other FK column), so only isUniqueViolation
			// applies (mirrors UpsertRuntimeSpec/SetCoResidencyRules' error
			// classification in this file).
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert gpu budget: %w", err)
		}
	}
	return tx.Commit()
}

// ServerGPUBudgets lists serverID's per-GPU VRAM budgets, ordered by GPU
// index. Always non-nil, empty when none.
func (s *SQLiteStore) ServerGPUBudgets(ctx context.Context, serverID string) ([]routing.ServerGPUBudget, error) {
	rows, err := s.query(ctx, `
		select server_id, gpu_index, budget_mb, expected_uuid, expected_name, created_at, updated_at
		from ai_server_gpu_budgets where server_id = ? order by gpu_index`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list gpu budgets: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ServerGPUBudget, 0)
	for rows.Next() {
		var b routing.ServerGPUBudget
		if err := rows.Scan(&b.ServerID, &b.GPUIndex, &b.BudgetMB, &b.ExpectedUUID, &b.ExpectedName, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan gpu budget: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate gpu budgets: %w", err)
	}
	return out, nil
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

// UpsertServerRuntimeReport stores the latest file-mode runtime report for
// its server (server_runtime_reports, migration 67). A rename-level copy of
// UpsertServerHardware: report.ReportJSON is an opaque, already-validated
// canonical JSON blob — the store never parses or inspects it.
func (s *SQLiteStore) UpsertServerRuntimeReport(ctx context.Context, report routing.ServerRuntimeReport) error {
	_, err := s.exec(ctx, `
		insert into server_runtime_reports (server_id, collected_at, report_json, updated_at)
		values (?, ?, ?, ?)
		on conflict(server_id) do update set
			collected_at = excluded.collected_at,
			report_json = excluded.report_json,
			updated_at = excluded.updated_at`,
		report.ServerID,
		report.CollectedAt,
		report.ReportJSON,
		report.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert server runtime report: %w", err)
	}
	return nil
}

// ServerRuntimeReportByServer returns the latest runtime report for
// serverID; ok is false when no report has ever been stored (not an error).
// A rename-level copy of ServerHardwareByServer.
func (s *SQLiteStore) ServerRuntimeReportByServer(ctx context.Context, serverID string) (routing.ServerRuntimeReport, bool, error) {
	row := s.queryRow(ctx, `
		select server_id, collected_at, report_json, updated_at
		from server_runtime_reports
		where server_id = ?`, serverID)
	report, err := scanServerRuntimeReport(row)
	if errors.Is(err, ErrNotFound) {
		return routing.ServerRuntimeReport{}, false, nil
	}
	if err != nil {
		return routing.ServerRuntimeReport{}, false, err
	}
	return report, true, nil
}

func scanServerRuntimeReport(row rowScanner) (routing.ServerRuntimeReport, error) {
	var report routing.ServerRuntimeReport
	err := row.Scan(&report.ServerID, &report.CollectedAt, &report.ReportJSON, &report.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ServerRuntimeReport{}, ErrNotFound
	}
	if err != nil {
		return routing.ServerRuntimeReport{}, fmt.Errorf("scan server runtime report: %w", err)
	}
	return report, nil
}
