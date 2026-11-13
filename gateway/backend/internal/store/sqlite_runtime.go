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
//
// The two agent-owned measurements, ram_measured_mb and startup_measured_ms,
// are written by the insert and deliberately absent from the update set list:
// the portal's save is a full-document upsert built from the request alone,
// so it always carries 0 for them and would otherwise erase the agent's
// figures on every operator save.
func (s *SQLiteStore) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	apiFlavors, err := encodeAPIFlavors(spec.APIFlavors)
	if err != nil {
		return err
	}
	// A spec built outside the portal (a test fixture, the cluster head) may
	// leave the mode unset; the column is NOT NULL with a 'native' default, and
	// an explicit "" insert would bypass that default and store a value no
	// reader treats as a mode.
	if spec.ExecutionMode == "" {
		spec.ExecutionMode = routing.ExecutionModeNative
	}
	_, err = s.exec(ctx, `
		insert into agent_runtime_specs (
			id, mapping_id, enabled, binary_path, args, env, config_files, work_dir, listen_port,
			health_path, health_timeout_seconds, startup_timeout_seconds,
			idle_timeout_seconds, admission_wait_timeout_seconds, pinned,
			admin_state, set_visible_devices, api_flavors, responses_mode, messages_mode, decisions_mode, systemone_mode,
			visible_devices_mode, api_token_mode, api_token, api_token_header_source, api_token_header,
			type, metrics_path, context_probe_path, responses_live_timings_enabled,
			ram_estimate_mb, ram_measured_mb,
			replicable, min_instances, max_instances, instance_parallelism, startup_measured_ms,
			cluster_enabled, member_mode, cluster_network, rpc_transport, lan_exposure_ack, device_backend,
			execution_mode, wsl_distro,
			created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(mapping_id) do update set
			enabled = excluded.enabled, binary_path = excluded.binary_path,
			args = excluded.args, env = excluded.env,
			config_files = excluded.config_files, work_dir = excluded.work_dir,
			listen_port = excluded.listen_port, health_path = excluded.health_path,
			health_timeout_seconds = excluded.health_timeout_seconds,
			startup_timeout_seconds = excluded.startup_timeout_seconds,
			idle_timeout_seconds = excluded.idle_timeout_seconds,
			admission_wait_timeout_seconds = excluded.admission_wait_timeout_seconds,
			pinned = excluded.pinned, admin_state = excluded.admin_state,
			set_visible_devices = excluded.set_visible_devices,
			api_flavors = excluded.api_flavors, responses_mode = excluded.responses_mode,
			messages_mode = excluded.messages_mode,
			decisions_mode = excluded.decisions_mode, systemone_mode = excluded.systemone_mode,
			visible_devices_mode = excluded.visible_devices_mode,
			api_token_mode = excluded.api_token_mode, api_token = excluded.api_token,
			api_token_header_source = excluded.api_token_header_source,
			api_token_header = excluded.api_token_header,
			type = excluded.type, metrics_path = excluded.metrics_path,
			context_probe_path = excluded.context_probe_path,
			responses_live_timings_enabled = excluded.responses_live_timings_enabled,
			ram_estimate_mb = excluded.ram_estimate_mb,
			replicable = excluded.replicable,
			min_instances = excluded.min_instances,
			max_instances = excluded.max_instances,
			instance_parallelism = excluded.instance_parallelism,
			cluster_enabled = excluded.cluster_enabled,
			member_mode = excluded.member_mode,
			cluster_network = excluded.cluster_network,
			rpc_transport = excluded.rpc_transport,
			lan_exposure_ack = excluded.lan_exposure_ack,
			device_backend = excluded.device_backend,
			execution_mode = excluded.execution_mode,
			wsl_distro = excluded.wsl_distro,
			updated_at = excluded.updated_at`,
		spec.ID, spec.MappingID, spec.Enabled, spec.Binary, spec.Args, spec.Env,
		spec.ConfigFiles, spec.WorkDir, spec.ListenPort, spec.HealthPath, spec.HealthTimeoutSeconds,
		spec.StartupTimeoutSeconds, spec.IdleTimeoutSeconds,
		spec.AdmissionWaitTimeoutSeconds, spec.Pinned, spec.AdminState,
		spec.SetVisibleDevices, apiFlavors, string(spec.ResponsesMode), string(spec.MessagesMode),
		string(spec.DecisionsMode), string(spec.SystemOneMode),
		string(spec.VisibleDevicesMode), spec.APITokenMode, spec.APIToken, spec.APITokenHeaderSource, spec.APITokenHeader,
		spec.Type, spec.MetricsPath, spec.ContextProbePath, spec.ResponsesLiveTimingsEnabled,
		spec.RAMEstimateMB, spec.RAMMeasuredMB,
		spec.Replicable, spec.MinInstances, spec.MaxInstances, spec.InstanceParallelism, spec.StartupMeasuredMS,
		spec.ClusterEnabled, spec.MemberMode, spec.ClusterNetwork, spec.RPCTransport, spec.LANExposureAck, spec.DeviceBackend,
		spec.ExecutionMode, spec.WSLDistro,
		spec.CreatedAt, spec.UpdatedAt,
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

const runtimeSpecCols = `id, mapping_id, enabled, binary_path, args, env, config_files, work_dir,
	listen_port, health_path, health_timeout_seconds, startup_timeout_seconds,
	idle_timeout_seconds, admission_wait_timeout_seconds, pinned, admin_state,
	set_visible_devices, api_flavors, responses_mode, messages_mode, decisions_mode, systemone_mode,
	visible_devices_mode, api_token_mode, api_token, api_token_header_source, api_token_header,
	type, metrics_path, context_probe_path, responses_live_timings_enabled,
	ram_estimate_mb, ram_measured_mb,
	replicable, min_instances, max_instances, instance_parallelism, startup_measured_ms,
	cluster_enabled, member_mode, cluster_network, rpc_transport, lan_exposure_ack, device_backend,
	execution_mode, wsl_distro,
	created_at, updated_at`

// runtimeSpecColsPrefixed is runtimeSpecCols qualified with the `s` alias, for
// the RuntimeSpecsByApplication join below where an unqualified column list
// would be ambiguous against the joined model_mappings table (mirrors
// projectColsPrefixed in sqlite_projects.go).
const runtimeSpecColsPrefixed = `s.id, s.mapping_id, s.enabled, s.binary_path, s.args, s.env, s.config_files, s.work_dir,
	s.listen_port, s.health_path, s.health_timeout_seconds, s.startup_timeout_seconds,
	s.idle_timeout_seconds, s.admission_wait_timeout_seconds, s.pinned, s.admin_state,
	s.set_visible_devices, s.api_flavors, s.responses_mode, s.messages_mode, s.decisions_mode, s.systemone_mode,
	s.visible_devices_mode, s.api_token_mode, s.api_token, s.api_token_header_source, s.api_token_header,
	s.type, s.metrics_path, s.context_probe_path, s.responses_live_timings_enabled,
	s.ram_estimate_mb, s.ram_measured_mb,
	s.replicable, s.min_instances, s.max_instances, s.instance_parallelism, s.startup_measured_ms,
	s.cluster_enabled, s.member_mode, s.cluster_network, s.rpc_transport, s.lan_exposure_ack, s.device_backend,
	s.execution_mode, s.wsl_distro,
	s.created_at, s.updated_at`

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

// RuntimeSpecByID returns the spec for id (the primary-key lookup the
// telemetry VRAM write-back path uses -- see RuntimeStore.RuntimeSpecByID).
// Absent -> (zero, false, nil), mirroring RuntimeSpecByMapping.
func (s *SQLiteStore) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	row := s.queryRow(ctx, `select `+runtimeSpecCols+` from agent_runtime_specs where id = ?`, id)
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
			insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb, position)
			values (?, ?, ?, ?, ?)`), specID, g.GPUIndex, g.VRAMEstimateMB, g.VRAMMeasuredMB, g.Position); err != nil {
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
		select spec_id, gpu_index, vram_estimate_mb, vram_measured_mb, position
		from agent_runtime_spec_gpus where spec_id = ? order by position, gpu_index`, specID)
	if err != nil {
		return nil, fmt.Errorf("list spec gpus: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RuntimeSpecGPU, 0)
	for rows.Next() {
		var g routing.RuntimeSpecGPU
		if err := rows.Scan(&g.SpecID, &g.GPUIndex, &g.VRAMEstimateMB, &g.VRAMMeasuredMB, &g.Position); err != nil {
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

// UpdateRuntimeSpecRAMMeasured writes back the agent's measured host RAM for
// the spec. Spec-level (RAM is host-scoped, so the column is on
// agent_runtime_specs, not on a per-GPU row) and one-column on purpose: it never
// touches ram_estimate_mb, the operator's figure. ErrNotFound when no spec has
// that id (requireAffected).
func (s *SQLiteStore) UpdateRuntimeSpecRAMMeasured(ctx context.Context, specID string, measuredMB int) error {
	result, err := s.exec(ctx, `
		update agent_runtime_specs set ram_measured_mb = ?
		where id = ?`, measuredMB, specID)
	if err != nil {
		return fmt.Errorf("update measured ram: %w", err)
	}
	return requireAffected(result)
}

// UpdateRuntimeSpecStartupMeasured writes back the agent's measured startup
// time for the spec. One column on purpose, like UpdateRuntimeSpecRAMMeasured:
// it never touches the operator-owned instance settings beside it.
// ErrNotFound when no spec has that id (requireAffected).
func (s *SQLiteStore) UpdateRuntimeSpecStartupMeasured(ctx context.Context, specID string, ms int64) error {
	result, err := s.exec(ctx, `
		update agent_runtime_specs set startup_measured_ms = ?
		where id = ?`, ms, specID)
	if err != nil {
		return fmt.Errorf("update measured startup: %w", err)
	}
	return requireAffected(result)
}

// SetCoResidencyRules atomically REPLACES the whole set of explicit
// co-residency pair rows for appID (delete-then-insert in one transaction,
// mirroring SetRuntimeSpecGPUs above / SetGroupMembers). Each rule's verdict is
// checked up front -- an empty one is stored as allow, an unknown one fails the
// call before the transaction opens, so a bad rule can never leave the set
// half-replaced. appID must exist — checked inside the transaction, before the
// delete — or ErrNotFound. The store does not enforce or rewrite the
// MappingAID < MappingBID canonical ordering; that is portal-level validation
// (Task 6).
func (s *SQLiteStore) SetCoResidencyRules(ctx context.Context, appID string, rules []routing.CoResidencyRule) error {
	verdicts := make([]string, len(rules))
	for i, r := range rules {
		v, ok := routing.NormalizeCoResidencyVerdict(r.Verdict)
		if !ok {
			return fmt.Errorf("coresidency rule (%s, %s): invalid verdict %q", r.MappingAID, r.MappingBID, r.Verdict)
		}
		verdicts[i] = v
	}
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
	for i, r := range rules {
		if err := s.insertCoResidencyRule(ctx, tx, appID, r, verdicts[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// insertCoResidencyRule inserts one explicit pair row inside SetCoResidencyRules'
// transaction, with the already-normalised verdict, and maps the driver's
// constraint failures onto the store's sentinel errors.
func (s *SQLiteStore) insertCoResidencyRule(ctx context.Context, tx *sql.Tx, appID string, r routing.CoResidencyRule, verdict string) error {
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`
		insert into agent_coresidency_rules (application_id, mapping_a_id, mapping_b_id, verdict, created_at)
		values (?, ?, ?, ?, ?)`), appID, r.MappingAID, r.MappingBID, verdict, r.CreatedAt); err != nil {
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
	return nil
}

// CoResidencyRulesByApplication lists appID's explicit co-residency pair rows
// of both verdicts, ordered by mapping_a_id then mapping_b_id. Always non-nil,
// empty when none.
func (s *SQLiteStore) CoResidencyRulesByApplication(ctx context.Context, appID string) ([]routing.CoResidencyRule, error) {
	rows, err := s.query(ctx, `
		select application_id, mapping_a_id, mapping_b_id, verdict, created_at
		from agent_coresidency_rules where application_id = ? order by mapping_a_id, mapping_b_id`, appID)
	if err != nil {
		return nil, fmt.Errorf("list coresidency rules: %w", err)
	}
	defer rows.Close()
	out := make([]routing.CoResidencyRule, 0)
	for rows.Next() {
		var r routing.CoResidencyRule
		if err := rows.Scan(&r.ApplicationID, &r.MappingAID, &r.MappingBID, &r.Verdict, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan coresidency rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate coresidency rules: %w", err)
	}
	return out, nil
}

// GetCoResidencyMode returns appID's co-residency mode (applications.
// coresidency_mode, migration 99): how a pair with no explicit rule resolves.
// ErrNotFound when the application does not exist. It reads the column
// directly rather than going through ApplicationByID because the mode is not a
// field of routing.Application -- no application reader selects it.
func (s *SQLiteStore) GetCoResidencyMode(ctx context.Context, appID string) (routing.CoResidencyMode, error) {
	var mode string
	err := s.queryRow(ctx, `select coresidency_mode from applications where id = ?`, appID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read coresidency mode: %w", err)
	}
	return routing.CoResidencyMode(mode), nil
}

// SetCoResidencyMode writes appID's co-residency mode and nothing else: the
// explicit pair rows are not touched (so switching is lossless) and neither is
// the application's updated_at. A mode outside routing.CoResidencyMode.Valid is
// rejected before any write; an unknown application is ErrNotFound
// (requireAffected). It is a single-column UPDATE, which is also why
// UpdateApplication -- whose statement does not name the column -- can never
// clobber it.
func (s *SQLiteStore) SetCoResidencyMode(ctx context.Context, appID string, mode routing.CoResidencyMode) error {
	if !mode.Valid() {
		return fmt.Errorf("invalid coresidency mode %q", mode)
	}
	result, err := s.exec(ctx, `update applications set coresidency_mode = ? where id = ?`, string(mode), appID)
	if err != nil {
		return fmt.Errorf("update coresidency mode: %w", err)
	}
	return requireAffected(result)
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
	var enabled, pinned, setVisibleDevices, liveTimings, replicable, clusterEnabled, lanExposureAck int64
	var apiFlavors string
	err := row.Scan(&spec.ID, &spec.MappingID, &enabled, &spec.Binary, &spec.Args,
		&spec.Env, &spec.ConfigFiles, &spec.WorkDir, &spec.ListenPort, &spec.HealthPath,
		&spec.HealthTimeoutSeconds, &spec.StartupTimeoutSeconds,
		&spec.IdleTimeoutSeconds, &spec.AdmissionWaitTimeoutSeconds, &pinned,
		&spec.AdminState, &setVisibleDevices,
		&apiFlavors, &spec.ResponsesMode, &spec.MessagesMode,
		&spec.DecisionsMode, &spec.SystemOneMode,
		&spec.VisibleDevicesMode,
		&spec.APITokenMode, &spec.APIToken, &spec.APITokenHeaderSource, &spec.APITokenHeader,
		&spec.Type, &spec.MetricsPath, &spec.ContextProbePath, &liveTimings,
		&spec.RAMEstimateMB, &spec.RAMMeasuredMB,
		&replicable, &spec.MinInstances, &spec.MaxInstances, &spec.InstanceParallelism, &spec.StartupMeasuredMS,
		&clusterEnabled, &spec.MemberMode, &spec.ClusterNetwork, &spec.RPCTransport, &lanExposureAck, &spec.DeviceBackend,
		&spec.ExecutionMode, &spec.WSLDistro,
		&spec.CreatedAt, &spec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.RuntimeSpec{}, ErrNotFound
	}
	if err != nil {
		return routing.RuntimeSpec{}, fmt.Errorf("scan runtime spec: %w", err)
	}
	spec.Enabled, spec.Pinned = enabled != 0, pinned != 0
	spec.DecisionsMode, spec.SystemOneMode = decisionModesOrDefault(spec.DecisionsMode, spec.SystemOneMode)
	spec.SetVisibleDevices = setVisibleDevices != 0
	spec.ResponsesLiveTimingsEnabled = liveTimings != 0
	spec.Replicable = replicable != 0
	spec.ClusterEnabled, spec.LANExposureAck = clusterEnabled != 0, lanExposureAck != 0
	flavors, err := decodeAPIFlavors(apiFlavors)
	if err != nil {
		return routing.RuntimeSpec{}, err
	}
	spec.APIFlavors = flavors
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

// DeleteServerRuntimeReport removes serverID's stored runtime report, if any.
// Idempotent: a delete that matches no row is not an error (issue #260).
func (s *SQLiteStore) DeleteServerRuntimeReport(ctx context.Context, serverID string) error {
	if _, err := s.exec(ctx, `delete from server_runtime_reports where server_id = ?`, serverID); err != nil {
		return fmt.Errorf("delete server runtime report: %w", err)
	}
	return nil
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

// ServerBinaries lists serverID's binary inventory ordered by path. Always
// non-nil, empty when none.
func (s *SQLiteStore) ServerBinaries(ctx context.Context, serverID string) ([]routing.ServerBinary, error) {
	rows, err := s.query(ctx, `
		select server_id, path, kind, version, version_override, build, "commit", version_source, source, allowed, reported_at
		from server_binaries where server_id = ? order by path`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list server binaries: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ServerBinary, 0)
	for rows.Next() {
		var b routing.ServerBinary
		var allowed int64
		if err := rows.Scan(&b.ServerID, &b.Path, &b.Kind, &b.Version, &b.VersionOverride, &b.Build, &b.Commit,
			&b.VersionSource, &b.Source, &allowed, &b.ReportedAt); err != nil {
			return nil, fmt.Errorf("scan server binary: %w", err)
		}
		b.Allowed = allowed != 0
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate server binaries: %w", err)
	}
	return out, nil
}

// ReplaceAgentServerBinaries makes rows the complete set of source='agent'
// entries for serverID in one transaction. Agent rows whose path is no longer
// reported are deleted; the rest are inserted or refreshed in place, so a
// surviving row keeps its version_override. A manual row is never touched: a
// reported path it already owns is skipped (the conflict update is guarded by
// source = 'agent').
func (s *SQLiteStore) ReplaceAgentServerBinaries(ctx context.Context, serverID string, rows []routing.ServerBinary) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace server binaries: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from ai_servers where id = ?`), serverID).Scan(&exists); err != nil {
		return fmt.Errorf("check server: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	stale, err := s.staleAgentBinaryPaths(ctx, tx, serverID, rows)
	if err != nil {
		return err
	}
	for _, path := range stale {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from server_binaries where server_id = ? and path = ? and source = 'agent'`), serverID, path); err != nil {
			return fmt.Errorf("delete stale agent binary: %w", err)
		}
	}
	if err := s.upsertAgentBinaries(ctx, tx, serverID, rows); err != nil {
		return err
	}
	return tx.Commit()
}

// staleAgentBinaryPaths lists, inside tx, the paths of serverID's
// source='agent' rows that rows no longer report. The cursor is closed
// before it returns, so the caller's writes in tx follow a finished read.
func (s *SQLiteStore) staleAgentBinaryPaths(ctx context.Context, tx *sql.Tx, serverID string, rows []routing.ServerBinary) ([]string, error) {
	reported := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		reported[r.Path] = struct{}{}
	}
	existing, err := tx.QueryContext(ctx, s.dl.rebind(`select path from server_binaries where server_id = ? and source = 'agent'`), serverID)
	if err != nil {
		return nil, fmt.Errorf("list agent binaries: %w", err)
	}
	defer func() { _ = existing.Close() }()
	var stale []string
	for existing.Next() {
		var path string
		if err := existing.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan agent binary: %w", err)
		}
		if _, keep := reported[path]; !keep {
			stale = append(stale, path)
		}
	}
	if err := existing.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent binaries: %w", err)
	}
	return stale, nil
}

// upsertAgentBinaries inserts or refreshes, inside tx, each of rows as a
// source='agent' row of serverID; a path a manual row owns is left alone
// (the conflict update is guarded by source = 'agent').
func (s *SQLiteStore) upsertAgentBinaries(ctx context.Context, tx *sql.Tx, serverID string, rows []routing.ServerBinary) error {
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into server_binaries (server_id, path, kind, version, build, "commit", version_source, source, allowed, reported_at)
			values (?, ?, ?, ?, ?, ?, ?, 'agent', ?, ?)
			on conflict(server_id, path) do update set
				kind = excluded.kind, version = excluded.version, build = excluded.build,
				"commit" = excluded."commit", version_source = excluded.version_source,
				allowed = excluded.allowed, reported_at = excluded.reported_at
			where server_binaries.source = 'agent'`),
			sanitizeArgs([]any{serverID, r.Path, r.Kind, r.Version, r.Build, r.Commit, r.VersionSource, r.Allowed, r.ReportedAt})...); err != nil {
			return fmt.Errorf("upsert agent binary: %w", err)
		}
	}
	return nil
}

// UpsertManualServerBinary inserts or updates the operator-entered row at
// (b.ServerID, b.Path). The conflict update is guarded by source = 'manual',
// so a path an agent row owns affects no row and surfaces as ErrConflict; an
// existing manual row keeps its version_override.
func (s *SQLiteStore) UpsertManualServerBinary(ctx context.Context, b routing.ServerBinary) error {
	res, err := s.exec(ctx, `
		insert into server_binaries (server_id, path, kind, version, version_override, build, "commit", version_source, source, allowed, reported_at)
		values (?, ?, ?, ?, ?, ?, ?, 'manual', 'manual', ?, ?)
		on conflict(server_id, path) do update set
			kind = excluded.kind, version = excluded.version, build = excluded.build,
			"commit" = excluded."commit", allowed = excluded.allowed, reported_at = excluded.reported_at
		where server_binaries.source = 'manual'`,
		b.ServerID, b.Path, b.Kind, b.Version, b.VersionOverride, b.Build, b.Commit, b.Allowed, b.ReportedAt)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("upsert manual server binary: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	return nil
}

// DeleteManualServerBinary removes the manual row at (serverID, path); an
// agent row is never deleted here, and a missing row is ErrNotFound.
func (s *SQLiteStore) DeleteManualServerBinary(ctx context.Context, serverID, path string) error {
	res, err := s.exec(ctx, `delete from server_binaries where server_id = ? and path = ? and source = 'manual'`, serverID, path)
	if err != nil {
		return fmt.Errorf("delete manual server binary: %w", err)
	}
	return requireAffected(res)
}

// SetServerBinaryVersionOverride writes only version_override on the row at
// (serverID, path), whichever source owns it.
func (s *SQLiteStore) SetServerBinaryVersionOverride(ctx context.Context, serverID, path, version string) error {
	res, err := s.exec(ctx, `update server_binaries set version_override = ? where server_id = ? and path = ?`, version, serverID, path)
	if err != nil {
		return fmt.Errorf("set server binary version override: %w", err)
	}
	return requireAffected(res)
}
