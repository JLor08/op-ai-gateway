// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// InsertTelemetrySample appends one rich telemetry sample. server_id is an FK to
// ai_servers(id); a missing server classifies as ErrNotFound (mirrors
// UpsertTelemetry). gpus/net are stored as JSON columns.
func (s *SQLiteStore) InsertTelemetrySample(ctx context.Context, sample routing.TelemetrySample) error {
	gpus, err := json.Marshal(nonNilGPUs(sample.GPUs))
	if err != nil {
		return fmt.Errorf("encode gpus: %w", err)
	}
	net, err := json.Marshal(nonNilNet(sample.Net))
	if err != nil {
		return fmt.Errorf("encode net: %w", err)
	}
	_, err = s.exec(ctx, `
		insert into server_telemetry_samples (
			id, server_id, reported_at, cpu_util_pct, mem_used_bytes, mem_total_bytes,
			swap_used_bytes, swap_total_bytes, load1, load5, load15,
			active_requests, queue_depth, gpus_json, net_json, cpu_power_w, system_power_w,
			cpu_temp_c
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newTelemetrySampleID(),
		sample.ServerID,
		sample.ReportedAt,
		sample.CPUUtilPct,
		sample.MemUsedBytes,
		sample.MemTotalBytes,
		sample.SwapUsedBytes,
		sample.SwapTotalBytes,
		sample.Load1,
		sample.Load5,
		sample.Load15,
		sample.ActiveRequests,
		sample.QueueDepth,
		string(gpus),
		string(net),
		nullFloatArg(sample.CPUPowerW),
		nullFloatArg(sample.SystemPowerW),
		nullFloatArg(sample.CPUTempC),
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("insert telemetry sample: %w", err)
	}
	return nil
}

// TelemetrySamples returns the samples for serverID within [from,to] inclusive,
// ordered ascending by reported_at. When limit > 0 and more rows match, the
// result is decimated to at most limit evenly-spaced points spanning the window;
// the oldest and newest samples are always retained.
func (s *SQLiteStore) TelemetrySamples(ctx context.Context, serverID string, from, to time.Time, limit int) ([]routing.TelemetrySample, error) {
	rows, err := s.query(ctx, `
		select server_id, reported_at, cpu_util_pct, mem_used_bytes, mem_total_bytes,
			swap_used_bytes, swap_total_bytes, load1, load5, load15,
			active_requests, queue_depth, gpus_json, net_json, cpu_power_w, system_power_w,
			cpu_temp_c
		from server_telemetry_samples
		where server_id = ? and reported_at >= ? and reported_at <= ?
		order by reported_at asc`, serverID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list telemetry samples: %w", err)
	}
	defer rows.Close()
	all := make([]routing.TelemetrySample, 0)
	for rows.Next() {
		sample, err := scanTelemetrySample(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate telemetry samples: %w", err)
	}
	return routing.DecimateTelemetrySamples(all, limit), nil
}

// PruneTelemetrySamples deletes samples older than the cutoff (retention).
func (s *SQLiteStore) PruneTelemetrySamples(ctx context.Context, before time.Time) error {
	if _, err := s.exec(ctx, `delete from server_telemetry_samples where reported_at < ?`, before); err != nil {
		return fmt.Errorf("prune telemetry samples: %w", err)
	}
	return nil
}

func scanTelemetrySample(row rowScanner) (routing.TelemetrySample, error) {
	var sample routing.TelemetrySample
	var gpusJSON, netJSON string
	var cpuPower, sysPower, cpuTemp sql.NullFloat64
	if err := row.Scan(
		&sample.ServerID, &sample.ReportedAt, &sample.CPUUtilPct,
		&sample.MemUsedBytes, &sample.MemTotalBytes, &sample.SwapUsedBytes, &sample.SwapTotalBytes,
		&sample.Load1, &sample.Load5, &sample.Load15,
		&sample.ActiveRequests, &sample.QueueDepth, &gpusJSON, &netJSON, &cpuPower, &sysPower, &cpuTemp,
	); err != nil {
		return routing.TelemetrySample{}, fmt.Errorf("scan telemetry sample: %w", err)
	}
	if err := json.Unmarshal([]byte(gpusJSON), &sample.GPUs); err != nil {
		return routing.TelemetrySample{}, fmt.Errorf("decode gpus: %w", err)
	}
	if err := json.Unmarshal([]byte(netJSON), &sample.Net); err != nil {
		return routing.TelemetrySample{}, fmt.Errorf("decode net: %w", err)
	}
	if cpuPower.Valid {
		v := cpuPower.Float64
		sample.CPUPowerW = &v
	}
	if sysPower.Valid {
		v := sysPower.Float64
		sample.SystemPowerW = &v
	}
	if cpuTemp.Valid {
		v := cpuTemp.Float64
		sample.CPUTempC = &v
	}
	return sample, nil
}

func nonNilGPUs(g []routing.GPUSample) []routing.GPUSample {
	if g == nil {
		return []routing.GPUSample{}
	}
	return g
}

func nonNilNet(n []routing.NetSample) []routing.NetSample {
	if n == nil {
		return []routing.NetSample{}
	}
	return n
}

// nullFloatArg binds a nullable *float64 as a SQL parameter: a nil pointer becomes
// SQL NULL, a set pointer its float value. sql.NullFloat64 is a driver.Valuer, so
// this works identically on sqlite (modernc) and postgres (pgx) — unlike a bare
// *float64, whose encoding differs across drivers.
func nullFloatArg(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func newTelemetrySampleID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "tsmp_" + hex.EncodeToString(b[:])
}
