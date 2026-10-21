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

// This file is the SQL side of the per-mapping cache (model_mapping_cache,
// migration 94, #251): one SQLStore implementation that serves BOTH the sqlite
// and the postgres driver through the dialect seam, mirroring
// sqlite_capabilities.go (the sibling child table of model_mappings) and
// routing.MemoryStore's in-memory twin. See routing.ModelMappingCacheRow for
// what a row means.

// ModelMappingCache lists one mapping's cache rows of one cache type, ordered
// by scope_key. A mapping with no such rows yields an empty (non-nil) slice --
// nothing cached is simply nothing cached.
func (s *SQLiteStore) ModelMappingCache(ctx context.Context, mappingID, cacheType string) ([]routing.ModelMappingCacheRow, error) {
	rows, err := s.query(ctx, `
		select mapping_id, cache_type, scope_key, payload, updated_at
		from model_mapping_cache where mapping_id = ? and cache_type = ? order by scope_key`,
		mappingID, cacheType)
	if err != nil {
		return nil, fmt.Errorf("list model mapping cache: %w", err)
	}
	out := make([]routing.ModelMappingCacheRow, 0)
	if err := scanModelMappingCacheRowsInto(rows, func(r routing.ModelMappingCacheRow) {
		out = append(out, r)
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ModelMappingCacheForMappings is the bulk reader: one query per chunk of
// DISTINCT ids instead of one per mapping. A mapping with no rows of the type
// contributes NO key.
//
// The ids are deduplicated first for the reason MappingCapabilitiesForMappings
// spells out: the scan APPENDS per mapping id, so the same id in TWO chunks
// would append that mapping's rows twice where routing.MemoryStore -- which
// assigns per id -- returns them once. The two drivers must not differ on an
// input either accepts. The chunk size is capabilityBatchChunk's, for the same
// bound-parameter-limit reason (one slot per id, plus the cache type).
func (s *SQLiteStore) ModelMappingCacheForMappings(ctx context.Context, mappingIDs []string, cacheType string) (map[string][]routing.ModelMappingCacheRow, error) {
	ids := make([]string, 0, len(mappingIDs))
	seen := make(map[string]struct{}, len(mappingIDs))
	for _, id := range mappingIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	out := make(map[string][]routing.ModelMappingCacheRow, len(ids))
	for start := 0; start < len(ids); start += capabilityBatchChunk {
		end := min(start+capabilityBatchChunk, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, cacheType)
		rows, err := s.query(ctx,
			"select mapping_id, cache_type, scope_key, payload, updated_at "+
				"from model_mapping_cache where mapping_id in ("+
				strings.Join(placeholders, ",")+") and cache_type = ? order by mapping_id, scope_key", args...)
		if err != nil {
			return nil, fmt.Errorf("list model mapping cache for mappings: %w", err)
		}
		if err := scanModelMappingCacheRowsInto(rows, func(r routing.ModelMappingCacheRow) {
			out[r.MappingID] = append(out[r.MappingID], r)
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanModelMappingCacheRowsInto drains one result set through emit and closes
// it. Split out so the bulk reader's chunk loop closes each result set on every
// exit path (a deferred Close inside the loop would hold them all open).
func scanModelMappingCacheRowsInto(rows *sql.Rows, emit func(routing.ModelMappingCacheRow)) error {
	defer rows.Close()
	for rows.Next() {
		var r routing.ModelMappingCacheRow
		if err := rows.Scan(&r.MappingID, &r.CacheType, &r.ScopeKey, &r.Payload, &r.UpdatedAt); err != nil {
			return fmt.Errorf("scan model mapping cache: %w", err)
		}
		emit(r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate model mapping cache: %w", err)
	}
	return nil
}

// UpsertModelMappingCache writes one cache row, replacing any row for the same
// (mapping, cache type, scope key) in place via `on conflict ... do update`.
// One statement is already atomic, so unlike UpsertMappingCapabilities (a SET
// of rows) it needs no transaction. The row is validated
// (routing.ValidateModelMappingCacheRow) before anything is written.
//
// An unknown MappingID is NOT a benign no-op: model_mapping_cache.mapping_id is
// a real foreign key, so the insert fails. The violation is reclassified to
// ErrNotFound (on both drivers, via the dialect's isForeignKeyViolation) so a
// caller that lost a race with a mapping delete can tell "the mapping is gone"
// from a real failure, and so the error class matches routing.MemoryStore's
// hand-rolled existence check.
func (s *SQLiteStore) UpsertModelMappingCache(ctx context.Context, row routing.ModelMappingCacheRow) error {
	if err := routing.ValidateModelMappingCacheRow(row); err != nil {
		return err
	}
	if _, err := s.exec(ctx, `
		insert into model_mapping_cache (mapping_id, cache_type, scope_key, payload, updated_at)
		values (?, ?, ?, ?, ?)
		on conflict(mapping_id, cache_type, scope_key) do update set
			payload = excluded.payload,
			updated_at = excluded.updated_at`,
		row.MappingID, row.CacheType, row.ScopeKey, row.Payload, row.UpdatedAt,
	); err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("upsert model mapping cache %q/%q: %w", row.MappingID, row.CacheType, err)
	}
	return nil
}

// DeleteModelMappingCache removes every cache row of one mapping, whatever its
// cache type. Deleting where there is nothing affects 0 rows and is a benign
// no-op, like DeleteMappingCapability.
func (s *SQLiteStore) DeleteModelMappingCache(ctx context.Context, mappingID string) error {
	if _, err := s.exec(ctx, `delete from model_mapping_cache where mapping_id = ?`, mappingID); err != nil {
		return fmt.Errorf("delete model mapping cache: %w", err)
	}
	return nil
}

// deleteModelMappingCacheIfRenamed drops a mapping's cache rows inside tx when
// the stored app_model_name differs from appModelName -- the invalidation hook
// UpdateMapping and UpdateMappingEditable run, in the SAME transaction as their
// update, BEFORE it (so the stored name is still the old one). The comparison
// is done by the database (an `exists` over the stored row) rather than by a
// separate read, so there is no read-then-write window and no extra round trip.
// A missing mapping or an unchanged name deletes nothing, and a later failure
// of the update rolls the delete back with the rest of tx.
func (s *SQLiteStore) deleteModelMappingCacheIfRenamed(ctx context.Context, tx *sql.Tx, mappingID, appModelName string) error {
	if _, err := s.execOn(ctx, tx, `
		delete from model_mapping_cache
		where mapping_id = ?
		  and exists (select 1 from model_mappings where id = ? and app_model_name <> ?)`,
		mappingID, mappingID, appModelName,
	); err != nil {
		return fmt.Errorf("invalidate model mapping cache on rename: %w", err)
	}
	return nil
}
