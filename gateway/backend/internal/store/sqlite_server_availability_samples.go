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

// InsertServerAvailabilitySample appends one availability sample. server_id is an
// FK to ai_servers(id); a missing server classifies as ErrNotFound.
func (s *SQLiteStore) InsertServerAvailabilitySample(ctx context.Context, sample routing.ServerAvailabilitySample) error {
	_, err := s.exec(ctx, `
		insert into server_availability_samples (
			id, server_id, reported_at, health, reachable_count, active_count, agent_reporting, netbird_connected
		) values (?, ?, ?, ?, ?, ?, ?, ?)`,
		newAvailabilitySampleID(),
		sample.ServerID,
		sample.ReportedAt,
		sample.Health,
		sample.ReachableCount,
		sample.ActiveCount,
		sample.AgentReporting,
		sample.NetbirdConnected,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("insert availability sample: %w", err)
	}
	return nil
}

// ServerAvailabilitySamples returns the samples for serverID within [from,to]
// inclusive, ordered ascending by reported_at, with contiguous same-state runs
// collapsed (transition + gap boundaries + endpoints kept). limit>0 applies an
// even-index safety cap after reduction.
func (s *SQLiteStore) ServerAvailabilitySamples(ctx context.Context, serverID string, from, to time.Time, limit int) ([]routing.ServerAvailabilitySample, error) {
	rows, err := s.query(ctx, `
		select server_id, reported_at, health, reachable_count, active_count, agent_reporting, netbird_connected
		from server_availability_samples
		where server_id = ? and reported_at >= ? and reported_at <= ?
		order by reported_at asc`, serverID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list availability samples: %w", err)
	}
	defer rows.Close()
	all := make([]routing.ServerAvailabilitySample, 0)
	for rows.Next() {
		sample, err := scanServerAvailabilitySample(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate availability samples: %w", err)
	}
	return routing.ReduceAvailabilitySamples(all, routing.AvailabilityGapFloor, limit), nil
}

// PruneServerAvailabilitySamples deletes samples older than the cutoff.
func (s *SQLiteStore) PruneServerAvailabilitySamples(ctx context.Context, before time.Time) error {
	if _, err := s.exec(ctx, `delete from server_availability_samples where reported_at < ?`, before); err != nil {
		return fmt.Errorf("prune availability samples: %w", err)
	}
	return nil
}

func scanServerAvailabilitySample(row rowScanner) (routing.ServerAvailabilitySample, error) {
	var sample routing.ServerAvailabilitySample
	var agentReporting, netbirdConnected int64
	if err := row.Scan(
		&sample.ServerID, &sample.ReportedAt, &sample.Health,
		&sample.ReachableCount, &sample.ActiveCount, &agentReporting, &netbirdConnected,
	); err != nil {
		return routing.ServerAvailabilitySample{}, fmt.Errorf("scan availability sample: %w", err)
	}
	sample.AgentReporting = agentReporting != 0
	sample.NetbirdConnected = netbirdConnected != 0
	return sample, nil
}

func newAvailabilitySampleID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "asmp_" + hex.EncodeToString(b[:])
}
