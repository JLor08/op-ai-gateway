// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// InsertBenchmarkRun appends one benchmark-run history row for its mapping.
// mapping_id is an FK to model_mappings(id); a missing mapping classifies as
// ErrNotFound (mirrors InsertTelemetrySample). server_id has no FK (a run
// survives server churn). An empty run.ID gets a generated one.
func (s *SQLiteStore) InsertBenchmarkRun(ctx context.Context, run routing.BenchmarkRun) error {
	id := run.ID
	if id == "" {
		id = newBenchmarkRunID()
	}
	kind := run.Kind
	if kind == "" {
		kind = "speed"
	}
	_, err := s.exec(ctx, `
		insert into model_mapping_benchmarks (
			id, mapping_id, server_id, created_at,
			gen_tokens_per_second, prompt_tokens_per_second,
			load_time_ms, context_size, error, kind, capacity_curve, vision_capable,
			vram_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		run.MappingID,
		run.ServerID,
		run.CreatedAt,
		run.GenTokensPerSecond,
		run.PromptTokensPerSecond,
		run.LoadTimeMS,
		run.ContextSize,
		run.Error,
		kind,
		run.CapacityCurve,
		run.VisionCapable,
		run.VRAMJSON,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("insert benchmark run: %w", err)
	}
	return nil
}

// BenchmarkRunsByMapping returns the benchmark-run history for mappingID,
// newest-first (created_at desc). A non-positive limit defaults to 50.
func (s *SQLiteStore) BenchmarkRunsByMapping(ctx context.Context, mappingID string, limit int) ([]routing.BenchmarkRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.query(ctx, `
		select id, mapping_id, server_id, created_at,
			gen_tokens_per_second, prompt_tokens_per_second,
			load_time_ms, context_size, error, kind, capacity_curve, vision_capable,
			vram_json
		from model_mapping_benchmarks
		where mapping_id = ?
		order by created_at desc
		limit ?`, mappingID, limit)
	if err != nil {
		return nil, fmt.Errorf("list benchmark runs: %w", err)
	}
	defer rows.Close()
	out := make([]routing.BenchmarkRun, 0)
	for rows.Next() {
		var run routing.BenchmarkRun
		var visionCapable int64
		if err := rows.Scan(
			&run.ID, &run.MappingID, &run.ServerID, &run.CreatedAt,
			&run.GenTokensPerSecond, &run.PromptTokensPerSecond,
			&run.LoadTimeMS, &run.ContextSize, &run.Error, &run.Kind, &run.CapacityCurve, &visionCapable,
			&run.VRAMJSON,
		); err != nil {
			return nil, fmt.Errorf("scan benchmark run: %w", err)
		}
		run.VisionCapable = visionCapable != 0
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate benchmark runs: %w", err)
	}
	return out, nil
}

// PruneBenchmarkRuns deletes benchmark-run rows older than the cutoff (retention).
func (s *SQLiteStore) PruneBenchmarkRuns(ctx context.Context, before time.Time) error {
	if _, err := s.exec(ctx, `delete from model_mapping_benchmarks where created_at < ?`, before); err != nil {
		return fmt.Errorf("prune benchmark runs: %w", err)
	}
	return nil
}

func newBenchmarkRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "bmk_" + hex.EncodeToString(b[:])
}
