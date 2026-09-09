// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
)

// capabilityBatchChunk caps how many mapping ids go into a single
// `where mapping_id in (…)` list. Both drivers have a bound-parameter limit
// (SQLite's SQLITE_MAX_VARIABLE_NUMBER, Postgres' 65535 per statement), and
// the bulk reader is called with "every mapping in the listing" — a number no
// caller bounds. Chunking keeps a pathological deployment a few extra
// round-trips rather than a failed query.
const capabilityBatchChunk = 1000

// MappingCapabilities lists one mapping's capability rows, ordered by
// capability. A mapping with no rows yields an empty (non-nil) slice — every
// capability is simply unknown.
func (s *SQLiteStore) MappingCapabilities(ctx context.Context, mappingID string) ([]routing.CapabilityRow, error) {
	rows, err := s.query(ctx, `
		select capability, verdict, source, checked_at
		from model_mapping_capabilities where mapping_id = ? order by capability`, mappingID)
	if err != nil {
		return nil, fmt.Errorf("list mapping capabilities: %w", err)
	}
	defer rows.Close()
	out := make([]routing.CapabilityRow, 0)
	for rows.Next() {
		var r routing.CapabilityRow
		if err := rows.Scan(&r.Capability, &r.Verdict, &r.Source, &r.CheckedAt); err != nil {
			return nil, fmt.Errorf("scan mapping capability: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mapping capabilities: %w", err)
	}
	return out, nil
}

// MappingCapabilitiesForMappings is the bulk reader: one query per chunk of
// ids instead of one per mapping. A mapping with no rows contributes NO key,
// so a caller's zero-value lookup is "every capability unknown" without an
// empty slice having to be manufactured for it.
func (s *SQLiteStore) MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]routing.CapabilityRow, error) {
	out := make(map[string][]routing.CapabilityRow, len(mappingIDs))
	for start := 0; start < len(mappingIDs); start += capabilityBatchChunk {
		end := min(start+capabilityBatchChunk, len(mappingIDs))
		chunk := mappingIDs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.query(ctx,
			"select mapping_id, capability, verdict, source, checked_at "+
				"from model_mapping_capabilities where mapping_id in ("+
				strings.Join(placeholders, ",")+") order by mapping_id, capability", args...)
		if err != nil {
			return nil, fmt.Errorf("list capabilities for mappings: %w", err)
		}
		if err := scanCapabilityRowsInto(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanCapabilityRowsInto drains one chunk's rows into out, keyed by mapping
// id. Split out so the chunk loop can close each result set on every exit
// path (a deferred Close inside the loop would hold them all open).
func scanCapabilityRowsInto(rows *sql.Rows, out map[string][]routing.CapabilityRow) error {
	defer rows.Close()
	for rows.Next() {
		var mappingID string
		var r routing.CapabilityRow
		if err := rows.Scan(&mappingID, &r.Capability, &r.Verdict, &r.Source, &r.CheckedAt); err != nil {
			return fmt.Errorf("scan capability for mappings: %w", err)
		}
		out[mappingID] = append(out[mappingID], r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate capabilities for mappings: %w", err)
	}
	return nil
}

// UpsertMappingCapabilities writes one row per verdict, replacing any row for
// the same (mapping, capability). It applies NO precedence rule — the caller
// decides whether its source may overwrite what is there (see
// routing.CapabilitySourceIsAuthoritative) — and, like
// UpdateMappingCapabilities, carries no metrics_locked guard and never
// touches metrics_source/metrics_updated_at: a capability is not a number an
// operator pins against automation.
//
// Every row is validated (routing.ValidateCapabilityRow) before anything is
// written, and the whole set is written inside ONE transaction (mirroring
// SetCoResidencyRules): a caller such as a probe writer that hands over a
// full verdict set must never leave the mapping with some capabilities from
// the new set and some from the old — a half-written capability picture is
// worse than an unchanged one.
func (s *SQLiteStore) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	for _, r := range rows {
		if err := routing.ValidateCapabilityRow(r); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert mapping capabilities: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into model_mapping_capabilities (mapping_id, capability, verdict, source, checked_at)
			values (?, ?, ?, ?, ?)
			on conflict(mapping_id, capability) do update set
				verdict = excluded.verdict,
				source = excluded.source,
				checked_at = excluded.checked_at`),
			mappingID, r.Capability, r.Verdict, r.Source, r.CheckedAt,
		); err != nil {
			return fmt.Errorf("upsert mapping capability %q: %w", r.Capability, err)
		}
	}
	return tx.Commit()
}

// DeleteMappingCapability returns one capability to UNKNOWN by removing its
// row. Deleting a row that is not there affects 0 rows and is a benign no-op
// (the file's convention for a delete whose target may already be gone) —
// "unknown" is the state, and it is already reached.
func (s *SQLiteStore) DeleteMappingCapability(ctx context.Context, mappingID, capability string) error {
	if _, err := s.exec(ctx,
		`delete from model_mapping_capabilities where mapping_id = ? and capability = ?`,
		mappingID, capability); err != nil {
		return fmt.Errorf("delete mapping capability %q: %w", capability, err)
	}
	return nil
}
