// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

func (s *SQLiteStore) CreateApplication(ctx context.Context, app routing.Application) error {
	apiFlavors, err := encodeAPIFlavors(app.APIFlavors)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `
		insert into applications (
			id, server_id, type, port, scheme, api_flavors, priority, weight,
			timeout_ms, affinity_ttl_seconds, admission_queue_timeout_seconds, status, always_reachable, health_check_path,
			health_check_mode, health_check_interval_seconds, native_responses, native_messages,
			loaded_models_path, loaded_models_format, context_probe_path, capacity_probe_path,
			app_path_suffix, api_token, api_token_header,
			benchmark_schedule_enabled, benchmark_schedule_interval_seconds, opportunistic_metrics_enabled,
			proxy_listen_port, proxy_excluded,
			created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ID,
		app.ServerID,
		app.Type,
		app.Port,
		app.Scheme,
		apiFlavors,
		app.Priority,
		app.Weight,
		app.TimeoutMS,
		app.AffinityTTLSeconds,
		app.AdmissionQueueTimeoutSeconds,
		app.Status,
		app.AlwaysReachable,
		app.HealthCheckPath,
		app.HealthCheckMode,
		app.HealthCheckIntervalSeconds,
		app.NativeResponses,
		app.NativeMessages,
		app.LoadedModelsPath,
		app.LoadedModelsFormat,
		app.ContextProbePath,
		app.CapacityProbePath,
		app.AppPathSuffix,
		app.APIToken,
		app.APITokenHeader,
		app.BenchmarkScheduleEnabled,
		app.BenchmarkScheduleIntervalSeconds,
		app.OpportunisticMetricsEnabled,
		app.ProxyListenPort,
		app.ProxyExcluded,
		app.CreatedAt,
		app.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create application: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateApplication(ctx context.Context, app routing.Application) error {
	apiFlavors, err := encodeAPIFlavors(app.APIFlavors)
	if err != nil {
		return err
	}
	result, err := s.exec(ctx, `
		update applications
		set server_id = ?, type = ?, port = ?, scheme = ?, api_flavors = ?,
			priority = ?, weight = ?, timeout_ms = ?, affinity_ttl_seconds = ?,
			admission_queue_timeout_seconds = ?,
			status = ?, always_reachable = ?, health_check_path = ?,
			health_check_mode = ?, health_check_interval_seconds = ?,
			native_responses = ?, native_messages = ?,
			loaded_models_path = ?, loaded_models_format = ?, context_probe_path = ?,
			capacity_probe_path = ?,
			app_path_suffix = ?, api_token = ?, api_token_header = ?,
			benchmark_schedule_enabled = ?, benchmark_schedule_interval_seconds = ?,
			opportunistic_metrics_enabled = ?,
			proxy_listen_port = ?, proxy_excluded = ?,
			updated_at = ?
		where id = ?`,
		app.ServerID,
		app.Type,
		app.Port,
		app.Scheme,
		apiFlavors,
		app.Priority,
		app.Weight,
		app.TimeoutMS,
		app.AffinityTTLSeconds,
		app.AdmissionQueueTimeoutSeconds,
		app.Status,
		app.AlwaysReachable,
		app.HealthCheckPath,
		app.HealthCheckMode,
		app.HealthCheckIntervalSeconds,
		app.NativeResponses,
		app.NativeMessages,
		app.LoadedModelsPath,
		app.LoadedModelsFormat,
		app.ContextProbePath,
		app.CapacityProbePath,
		app.AppPathSuffix,
		app.APIToken,
		app.APITokenHeader,
		app.BenchmarkScheduleEnabled,
		app.BenchmarkScheduleIntervalSeconds,
		app.OpportunisticMetricsEnabled,
		app.ProxyListenPort,
		app.ProxyExcluded,
		app.UpdatedAt,
		app.ID,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update application: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) ApplicationByID(ctx context.Context, id string) (routing.Application, error) {
	row := s.queryRow(ctx, `
		select id, server_id, type, port, scheme, api_flavors, priority, weight,
			timeout_ms, affinity_ttl_seconds, admission_queue_timeout_seconds, status, always_reachable, health_check_path,
			health_check_mode, health_check_interval_seconds, native_responses, native_messages,
			loaded_models_path, loaded_models_format, context_probe_path, capacity_probe_path,
			app_path_suffix, api_token, api_token_header,
			benchmark_schedule_enabled, benchmark_schedule_interval_seconds, opportunistic_metrics_enabled,
			proxy_listen_port, proxy_excluded,
			created_at, updated_at
		from applications
		where id = ?`, id)
	return scanApplication(row)
}

func (s *SQLiteStore) ApplicationsByServer(ctx context.Context, serverID string) ([]routing.Application, error) {
	rows, err := s.query(ctx, `
		select id, server_id, type, port, scheme, api_flavors, priority, weight,
			timeout_ms, affinity_ttl_seconds, admission_queue_timeout_seconds, status, always_reachable, health_check_path,
			health_check_mode, health_check_interval_seconds, native_responses, native_messages,
			loaded_models_path, loaded_models_format, context_probe_path, capacity_probe_path,
			app_path_suffix, api_token, api_token_header,
			benchmark_schedule_enabled, benchmark_schedule_interval_seconds, opportunistic_metrics_enabled,
			proxy_listen_port, proxy_excluded,
			created_at, updated_at
		from applications
		where server_id = ?
		order by id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	defer rows.Close()
	return scanApplications(rows)
}

func (s *SQLiteStore) DeleteApplication(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `delete from applications where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete application: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) CreateMapping(ctx context.Context, mapping routing.ModelMapping) error {
	_, err := s.exec(ctx, `
		insert into model_mappings (
			id, application_id, gateway_model_name, app_model_name, status,
			gen_tokens_per_second, prompt_tokens_per_second, load_time_ms, context_size,
			is_mtp, vision_capable, energy_wh_per_token, metrics_locked, metrics_updated_at, metrics_source,
			max_concurrency, recommended_concurrency, gen_tokens_per_second_at_capacity,
			created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mapping.ID,
		mapping.ApplicationID,
		mapping.GatewayModelName,
		mapping.AppModelName,
		mapping.Status,
		mapping.GenTokensPerSecond,
		mapping.PromptTokensPerSecond,
		mapping.LoadTimeMS,
		mapping.ContextSize,
		mapping.IsMTP,
		mapping.VisionCapable,
		mapping.EnergyWhPerToken,
		mapping.MetricsLocked,
		mapping.MetricsUpdatedAt,
		mapping.MetricsSource,
		mapping.MaxConcurrency,
		mapping.RecommendedConcurrency,
		mapping.GenTokensPerSecondAtCapacity,
		mapping.CreatedAt,
		mapping.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create mapping: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateMapping(ctx context.Context, mapping routing.ModelMapping) error {
	result, err := s.exec(ctx, `
		update model_mappings
		set application_id = ?, gateway_model_name = ?, app_model_name = ?,
			status = ?, gen_tokens_per_second = ?, prompt_tokens_per_second = ?,
			load_time_ms = ?, context_size = ?, is_mtp = ?, vision_capable = ?, energy_wh_per_token = ?, metrics_locked = ?,
			metrics_updated_at = ?, metrics_source = ?,
			max_concurrency = ?, recommended_concurrency = ?, gen_tokens_per_second_at_capacity = ?,
			updated_at = ?
		where id = ?`,
		mapping.ApplicationID,
		mapping.GatewayModelName,
		mapping.AppModelName,
		mapping.Status,
		mapping.GenTokensPerSecond,
		mapping.PromptTokensPerSecond,
		mapping.LoadTimeMS,
		mapping.ContextSize,
		mapping.IsMTP,
		mapping.VisionCapable,
		mapping.EnergyWhPerToken,
		mapping.MetricsLocked,
		mapping.MetricsUpdatedAt,
		mapping.MetricsSource,
		mapping.MaxConcurrency,
		mapping.RecommendedConcurrency,
		mapping.GenTokensPerSecondAtCapacity,
		mapping.UpdatedAt,
		mapping.ID,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update mapping: %w", err)
	}
	return requireAffected(result)
}

// UpdateMappingContextProbe sets a mapping's context_size + provenance from a
// context probe. The metrics_locked = 0 guard makes the lock atomic in SQL: a
// locked (or missing) row matches 0 rows and is left untouched, which is a benign
// no-op (not an error). Only the three columns are written, so a concurrent edit
// of other fields cannot be clobbered.
func (s *SQLiteStore) UpdateMappingContextProbe(ctx context.Context, id string, contextSize int, at time.Time) error {
	_, err := s.exec(ctx, `
		update model_mappings
		set context_size = ?, metrics_source = ?, metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		contextSize, "probe", at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping context probe: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

// UpdateMappingVisionCapable sets a mapping's vision_capable flag + provenance
// from a vision-capability check. The metrics_locked = 0 guard makes the lock
// atomic in SQL: a locked (or missing) row matches 0 rows and is left untouched,
// which is a benign no-op (not an error). A definitive "not capable" (false)
// result can also be written. Only the flag + provenance are written, so a
// concurrent edit of other fields cannot be clobbered.
func (s *SQLiteStore) UpdateMappingVisionCapable(ctx context.Context, id string, capable bool, at time.Time) error {
	_, err := s.exec(ctx, `
		update model_mappings
		set vision_capable = ?, metrics_source = ?, metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		capable, "vision", at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping vision capable: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

// UpdateMappingBenchmarkMetrics sets a mapping's measured throughput + load time +
// provenance from a benchmark run. The metrics_locked = 0 guard makes the lock
// atomic in SQL: a locked (or missing) row matches 0 rows and is left untouched,
// which is a benign no-op (not an error). Only the four metric columns + provenance
// are written, so a concurrent edit of other fields cannot be clobbered.
func (s *SQLiteStore) UpdateMappingBenchmarkMetrics(ctx context.Context, id string, genTPS, promptTPS float64, loadMS int, at time.Time) error {
	_, err := s.exec(ctx, `
		update model_mappings
		set gen_tokens_per_second = ?, prompt_tokens_per_second = ?, load_time_ms = ?,
			metrics_source = ?, metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		genTPS, promptTPS, loadMS, "benchmark", at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping benchmark metrics: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

// UpdateMappingOpportunisticMetrics EWMA-updates a mapping's gen/prompt throughput
// from a single live usage sample. Per column a 3-branch CASE: a non-positive sample
// leaves the column unchanged; a stored 0 is seeded directly by the first positive
// sample; otherwise the value is blended as alpha*sample + (1-alpha)*old. The
// metrics_locked = 0 guard makes the lock atomic in SQL: a locked (or missing) row
// matches 0 rows and is left untouched, a benign no-op (not an error). Only the two
// metric columns + provenance are written, so a concurrent edit of other fields
// cannot be clobbered.
func (s *SQLiteStore) UpdateMappingOpportunisticMetrics(ctx context.Context, id string, genSample, promptSample, alpha float64, at time.Time) error {
	// Every float bind param is CAST to double precision. Postgres cannot type-resolve
	// arithmetic on bare placeholders — `? * ?` is `unknown * unknown` → "operator is not
	// unique (42725)"; the comparisons/assignments are likewise cast so postgres never has to
	// infer a param's type from an integer literal (which would coerce a fractional alpha/sample
	// to 0). double precision (float64) matches both the column type and the Go float64 the
	// value came from, so the EWMA blend keeps full precision — casting to `real` (float32)
	// would silently round it (a real-typed alpha drifts a clean 120 to 119.99999970197678 once
	// the column is double precision). SQLite applies REAL affinity (always 8-byte) to
	// `cast(? as double precision)`, so the float value round-trips unchanged there too.
	_, err := s.exec(ctx, `
		update model_mappings
		set gen_tokens_per_second = case
		      when cast(? as double precision) <= 0 then gen_tokens_per_second
		      when gen_tokens_per_second <= 0 then cast(? as double precision)
		      else cast(? as double precision) * cast(? as double precision) + (1 - cast(? as double precision)) * gen_tokens_per_second
		    end,
		    prompt_tokens_per_second = case
		      when cast(? as double precision) <= 0 then prompt_tokens_per_second
		      when prompt_tokens_per_second <= 0 then cast(? as double precision)
		      else cast(? as double precision) * cast(? as double precision) + (1 - cast(? as double precision)) * prompt_tokens_per_second
		    end,
		    metrics_source = ?, metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		genSample, genSample, alpha, genSample, alpha,
		promptSample, promptSample, alpha, promptSample, alpha,
		"opportunistic", at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping opportunistic metrics: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

// UpdateMappingEnergyEWMA EWMA-blends a mapping's energy_wh_per_token coefficient
// from a single live per-request energy sample (watt-hours/token), mirroring
// UpdateMappingOpportunisticMetrics's 3-branch CASE exactly: a non-positive
// sample leaves the column unchanged; a stored non-positive value is seeded
// directly by the first positive sample; otherwise the value is blended as
// alpha*sample + (1-alpha)*old. The metrics_locked = 0 guard makes the lock
// atomic in SQL: a locked (or missing) row matches 0 rows and is left
// untouched, a benign no-op (not an error). Only the coefficient + provenance
// are written, so a concurrent edit of other fields cannot be clobbered.
func (s *SQLiteStore) UpdateMappingEnergyEWMA(ctx context.Context, id string, sampleWhPerToken, alpha float64, at time.Time) error {
	// Every float bind param is CAST to double precision for the same reason as
	// UpdateMappingOpportunisticMetrics: postgres cannot type-resolve arithmetic on
	// bare placeholders (`? * ?` -> "operator is not unique (42725)"), and an
	// uncast comparison could let postgres infer an integer type from a literal and
	// coerce a fractional sample/alpha to 0. double precision (float64) keeps the blend
	// full-precision to match the float64 column; casting to `real` (float32) would round it.
	// SQLite applies REAL affinity (always 8-byte) to `cast(? as double precision)`, so the
	// float value round-trips unchanged there. Bind order mirrors
	// UpdateMappingOpportunisticMetrics's per-column pattern exactly:
	// sample, sample, alpha, sample, alpha (for the CASE), then at, id.
	_, err := s.exec(ctx, `
		update model_mappings
		set energy_wh_per_token = case
		      when cast(? as double precision) <= 0 then energy_wh_per_token
		      when energy_wh_per_token <= 0 then cast(? as double precision)
		      else cast(? as double precision) * cast(? as double precision) + (1 - cast(? as double precision)) * energy_wh_per_token
		    end,
		    metrics_source = 'energy', metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		sampleWhPerToken, sampleWhPerToken, alpha, sampleWhPerToken, alpha,
		at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping energy ewma: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

// UpdateMappingCapacityMetrics sets a mapping's measured concurrency capacity +
// provenance from a capacity benchmark. The metrics_locked = 0 guard makes the lock
// atomic in SQL: a locked (or missing) row matches 0 rows and is left untouched,
// which is a benign no-op (not an error). Only the three capacity columns + provenance
// are written, so a concurrent edit of other fields cannot be clobbered.
func (s *SQLiteStore) UpdateMappingCapacityMetrics(ctx context.Context, id string, maxConcurrency, recommendedConcurrency int, genTPSAtCapacity float64, at time.Time) error {
	_, err := s.exec(ctx, `
		update model_mappings
		set max_concurrency = ?, recommended_concurrency = ?, gen_tokens_per_second_at_capacity = ?,
			metrics_source = ?, metrics_updated_at = ?
		where id = ? and metrics_locked = 0`,
		maxConcurrency, recommendedConcurrency, genTPSAtCapacity, "capacity", at, id,
	)
	if err != nil {
		return fmt.Errorf("update mapping capacity metrics: %w", err)
	}
	return nil // 0 rows affected (missing or locked) is a benign no-op
}

func (s *SQLiteStore) MappingByID(ctx context.Context, id string) (routing.ModelMapping, error) {
	row := s.queryRow(ctx, `
		select id, application_id, gateway_model_name, app_model_name, status,
			gen_tokens_per_second, prompt_tokens_per_second, load_time_ms, context_size,
			is_mtp, vision_capable, energy_wh_per_token, metrics_locked, metrics_updated_at, metrics_source,
			max_concurrency, recommended_concurrency, gen_tokens_per_second_at_capacity,
			created_at, updated_at
		from model_mappings
		where id = ?`, id)
	return scanMapping(row)
}

func (s *SQLiteStore) MappingsByApplication(ctx context.Context, applicationID string) ([]routing.ModelMapping, error) {
	rows, err := s.query(ctx, `
		select id, application_id, gateway_model_name, app_model_name, status,
			gen_tokens_per_second, prompt_tokens_per_second, load_time_ms, context_size,
			is_mtp, vision_capable, energy_wh_per_token, metrics_locked, metrics_updated_at, metrics_source,
			max_concurrency, recommended_concurrency, gen_tokens_per_second_at_capacity,
			created_at, updated_at
		from model_mappings
		where application_id = ?
		order by id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list mappings by application: %w", err)
	}
	defer rows.Close()
	return scanMappings(rows)
}

func (s *SQLiteStore) MappingsByServer(ctx context.Context, serverID string) ([]routing.ModelMapping, error) {
	rows, err := s.query(ctx, `
		select m.id, m.application_id, m.gateway_model_name, m.app_model_name, m.status,
			m.gen_tokens_per_second, m.prompt_tokens_per_second, m.load_time_ms, m.context_size,
			m.is_mtp, m.vision_capable, m.energy_wh_per_token, m.metrics_locked, m.metrics_updated_at, m.metrics_source,
			m.max_concurrency, m.recommended_concurrency, m.gen_tokens_per_second_at_capacity,
			m.created_at, m.updated_at
		from model_mappings m
		join applications a on a.id = m.application_id
		where a.server_id = ?
		order by m.id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list mappings by server: %w", err)
	}
	defer rows.Close()
	return scanMappings(rows)
}

func (s *SQLiteStore) ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]routing.MappingCandidate, error) {
	rows, err := s.query(ctx, `
		select
			srv.id, srv.name, srv.domain, srv.server_path_suffix, srv.provider, srv.endpoint, srv.status,
			srv.health_status, srv.last_seen_at, srv.created_at, srv.updated_at,
			a.id, a.server_id, a.type, a.port, a.scheme, a.api_flavors, a.priority,
			a.weight, a.timeout_ms, a.affinity_ttl_seconds, a.admission_queue_timeout_seconds, a.status, a.always_reachable,
			a.health_check_path, a.health_check_mode, a.health_check_interval_seconds,
			a.native_responses, a.native_messages, a.loaded_models_path, a.loaded_models_format,
			a.context_probe_path, a.capacity_probe_path,
			a.app_path_suffix, a.api_token, a.api_token_header,
			a.benchmark_schedule_enabled, a.benchmark_schedule_interval_seconds, a.opportunistic_metrics_enabled,
			a.proxy_listen_port, a.proxy_excluded,
			a.created_at, a.updated_at,
			m.id, m.application_id, m.gateway_model_name, m.app_model_name, m.status,
			m.gen_tokens_per_second, m.prompt_tokens_per_second, m.load_time_ms, m.context_size,
			m.is_mtp, m.vision_capable, m.energy_wh_per_token, m.metrics_locked, m.metrics_updated_at, m.metrics_source,
			m.max_concurrency, m.recommended_concurrency, m.gen_tokens_per_second_at_capacity,
			m.created_at, m.updated_at
		from model_mappings m
		join applications a on a.id = m.application_id
		join ai_servers srv on srv.id = a.server_id
		where m.gateway_model_name = ? and m.status = ? and a.status = ?
		order by m.id`, gatewayModel, routing.ServerStatusActive, routing.ServerStatusActive)
	if err != nil {
		return nil, fmt.Errorf("list active mappings: %w", err)
	}
	defer rows.Close()
	candidates := make([]routing.MappingCandidate, 0)
	for rows.Next() {
		candidate, err := scanMappingCandidate(rows)
		if err != nil {
			return nil, err
		}
		if !applicationServesFlavor(candidate.Application, apiFlavor) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active mappings: %w", err)
	}
	return candidates, nil
}

func scanMappingCandidate(row rowScanner) (routing.MappingCandidate, error) {
	var (
		c                    routing.MappingCandidate
		lastSeen             sql.NullTime
		appFlavors           string
		alwaysReachable      int64
		nativeResponses      int64
		nativeMessages       int64
		benchScheduleEnabled int64
		oppMetricsEnabled    int64
		proxyExcluded        int64
		mapIsMTP             int64
		mapVisionCapable     int64
		mapLocked            int64
		mapUpdatedNil        sql.NullTime
	)
	err := row.Scan(
		&c.Server.ID, &c.Server.Name, &c.Server.Domain, &c.Server.ServerPathSuffix, &c.Server.Provider, &c.Server.Endpoint,
		&c.Server.Status, &c.Server.HealthStatus, &lastSeen, &c.Server.CreatedAt, &c.Server.UpdatedAt,
		&c.Application.ID, &c.Application.ServerID, &c.Application.Type, &c.Application.Port, &c.Application.Scheme,
		&appFlavors, &c.Application.Priority, &c.Application.Weight, &c.Application.TimeoutMS,
		&c.Application.AffinityTTLSeconds, &c.Application.AdmissionQueueTimeoutSeconds, &c.Application.Status, &alwaysReachable, &c.Application.HealthCheckPath,
		&c.Application.HealthCheckMode, &c.Application.HealthCheckIntervalSeconds,
		&nativeResponses, &nativeMessages,
		&c.Application.LoadedModelsPath, &c.Application.LoadedModelsFormat,
		&c.Application.ContextProbePath, &c.Application.CapacityProbePath,
		&c.Application.AppPathSuffix, &c.Application.APIToken, &c.Application.APITokenHeader,
		&benchScheduleEnabled, &c.Application.BenchmarkScheduleIntervalSeconds, &oppMetricsEnabled,
		&c.Application.ProxyListenPort, &proxyExcluded,
		&c.Application.CreatedAt, &c.Application.UpdatedAt,
		&c.Mapping.ID, &c.Mapping.ApplicationID, &c.Mapping.GatewayModelName, &c.Mapping.AppModelName,
		&c.Mapping.Status,
		&c.Mapping.GenTokensPerSecond, &c.Mapping.PromptTokensPerSecond, &c.Mapping.LoadTimeMS, &c.Mapping.ContextSize,
		&mapIsMTP, &mapVisionCapable, &c.Mapping.EnergyWhPerToken, &mapLocked, &mapUpdatedNil, &c.Mapping.MetricsSource,
		&c.Mapping.MaxConcurrency, &c.Mapping.RecommendedConcurrency, &c.Mapping.GenTokensPerSecondAtCapacity,
		&c.Mapping.CreatedAt, &c.Mapping.UpdatedAt,
	)
	if err != nil {
		return routing.MappingCandidate{}, fmt.Errorf("scan mapping candidate: %w", err)
	}
	c.Application.AlwaysReachable = alwaysReachable != 0
	c.Application.NativeResponses = nativeResponses != 0
	c.Application.NativeMessages = nativeMessages != 0
	c.Application.BenchmarkScheduleEnabled = benchScheduleEnabled != 0
	c.Application.OpportunisticMetricsEnabled = oppMetricsEnabled != 0
	c.Application.ProxyExcluded = proxyExcluded != 0
	c.Mapping.IsMTP = mapIsMTP != 0
	c.Mapping.VisionCapable = mapVisionCapable != 0
	c.Mapping.MetricsLocked = mapLocked != 0
	if mapUpdatedNil.Valid {
		t := mapUpdatedNil.Time
		c.Mapping.MetricsUpdatedAt = &t
	}
	if lastSeen.Valid {
		t := lastSeen.Time
		c.Server.LastSeenAt = &t
	}
	flavors, err := decodeAPIFlavors(appFlavors)
	if err != nil {
		return routing.MappingCandidate{}, err
	}
	c.Application.APIFlavors = flavors
	return c, nil
}

func applicationServesFlavor(app routing.Application, apiFlavor string) bool {
	for _, candidate := range app.APIFlavors {
		if candidate == apiFlavor {
			return true
		}
	}
	return false
}

func (s *SQLiteStore) DeleteMapping(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `delete from model_mappings where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete mapping: %w", err)
	}
	return requireAffected(result)
}

func scanApplication(row rowScanner) (routing.Application, error) {
	var app routing.Application
	var apiFlavors string
	var alwaysReachable int64
	var nativeResponses, nativeMessages int64
	var benchScheduleEnabled, oppMetricsEnabled int64
	var proxyExcluded int64
	err := row.Scan(
		&app.ID,
		&app.ServerID,
		&app.Type,
		&app.Port,
		&app.Scheme,
		&apiFlavors,
		&app.Priority,
		&app.Weight,
		&app.TimeoutMS,
		&app.AffinityTTLSeconds,
		&app.AdmissionQueueTimeoutSeconds,
		&app.Status,
		&alwaysReachable,
		&app.HealthCheckPath,
		&app.HealthCheckMode,
		&app.HealthCheckIntervalSeconds,
		&nativeResponses,
		&nativeMessages,
		&app.LoadedModelsPath,
		&app.LoadedModelsFormat,
		&app.ContextProbePath,
		&app.CapacityProbePath,
		&app.AppPathSuffix,
		&app.APIToken,
		&app.APITokenHeader,
		&benchScheduleEnabled,
		&app.BenchmarkScheduleIntervalSeconds,
		&oppMetricsEnabled,
		&app.ProxyListenPort,
		&proxyExcluded,
		&app.CreatedAt,
		&app.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.Application{}, ErrNotFound
	}
	if err != nil {
		return routing.Application{}, fmt.Errorf("scan application: %w", err)
	}
	app.AlwaysReachable = alwaysReachable != 0
	app.NativeResponses = nativeResponses != 0
	app.NativeMessages = nativeMessages != 0
	app.BenchmarkScheduleEnabled = benchScheduleEnabled != 0
	app.OpportunisticMetricsEnabled = oppMetricsEnabled != 0
	app.ProxyExcluded = proxyExcluded != 0
	flavors, err := decodeAPIFlavors(apiFlavors)
	if err != nil {
		return routing.Application{}, err
	}
	app.APIFlavors = flavors
	return app, nil
}

func scanApplications(rows *sql.Rows) ([]routing.Application, error) {
	apps := make([]routing.Application, 0)
	for rows.Next() {
		app, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applications: %w", err)
	}
	return apps, nil
}

func scanMapping(row rowScanner) (routing.ModelMapping, error) {
	var mapping routing.ModelMapping
	var isMTP, visionCapable, locked int64
	var updatedNil sql.NullTime
	err := row.Scan(
		&mapping.ID,
		&mapping.ApplicationID,
		&mapping.GatewayModelName,
		&mapping.AppModelName,
		&mapping.Status,
		&mapping.GenTokensPerSecond,
		&mapping.PromptTokensPerSecond,
		&mapping.LoadTimeMS,
		&mapping.ContextSize,
		&isMTP,
		&visionCapable,
		&mapping.EnergyWhPerToken,
		&locked,
		&updatedNil,
		&mapping.MetricsSource,
		&mapping.MaxConcurrency,
		&mapping.RecommendedConcurrency,
		&mapping.GenTokensPerSecondAtCapacity,
		&mapping.CreatedAt,
		&mapping.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ModelMapping{}, ErrNotFound
	}
	if err != nil {
		return routing.ModelMapping{}, fmt.Errorf("scan mapping: %w", err)
	}
	mapping.IsMTP = isMTP != 0
	mapping.VisionCapable = visionCapable != 0
	mapping.MetricsLocked = locked != 0
	if updatedNil.Valid {
		t := updatedNil.Time
		mapping.MetricsUpdatedAt = &t
	}
	return mapping, nil
}

func scanMappings(rows *sql.Rows) ([]routing.ModelMapping, error) {
	mappings := make([]routing.ModelMapping, 0)
	for rows.Next() {
		mapping, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mappings: %w", err)
	}
	return mappings, nil
}

func decodeAPIFlavors(raw string) ([]string, error) {
	var flavors []string
	if err := json.Unmarshal([]byte(raw), &flavors); err != nil {
		return nil, fmt.Errorf("decode api flavors: %w", err)
	}
	return append([]string(nil), flavors...), nil
}
