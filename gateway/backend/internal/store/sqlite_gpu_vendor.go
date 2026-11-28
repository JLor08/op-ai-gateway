// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/routing"
)

// BackfillServerGPUVendor makes the legacy empty GPU vendor of serverID
// explicit; see routing.RuntimeStore. Per table the empty row is deleted when
// a row of vendor already has the same key (the explicit row wins), then the
// remaining empty rows are rewritten; both run in one transaction with the
// server's gpu_legacy_vendor / gpu_vendor_note write.
func (s *SQLiteStore) BackfillServerGPUVendor(ctx context.Context, serverID, vendor string, noteMixed bool) (int, error) {
	if vendor == "" || !routing.ValidGPUVendor(vendor) {
		return 0, fmt.Errorf("backfill gpu vendor: invalid vendor %q", vendor)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin backfill gpu vendor: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	run := func(stmt string, args ...any) (int, error) {
		res, err := tx.ExecContext(ctx, s.dl.rebind(stmt), args...)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		return int(n), err
	}
	// The legacy vendor is written on every call; it also proves the server.
	if n, err := run(`update ai_servers set gpu_legacy_vendor = ? where id = ?`, vendor, serverID); err != nil {
		return 0, fmt.Errorf("set gpu legacy vendor: %w", err)
	} else if n == 0 {
		return 0, ErrNotFound
	}

	rewritten := 0
	for _, t := range gpuVendorBackfillTables {
		n, err := backfillGPUVendorTable(run, t, serverID, vendor)
		if err != nil {
			return 0, err
		}
		rewritten += n
	}
	if rewritten > 0 && noteMixed {
		if _, err := run(`update ai_servers set gpu_vendor_note = 1 where id = ?`, serverID); err != nil {
			return 0, fmt.Errorf("set gpu vendor note: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit backfill gpu vendor: %w", err)
	}
	return rewritten, nil
}

// ClearServerGPUVendorNote lowers gpu_vendor_note and keeps gpu_legacy_vendor.
func (s *SQLiteStore) ClearServerGPUVendorNote(ctx context.Context, serverID string) error {
	result, err := s.exec(ctx, `update ai_servers set gpu_vendor_note = 0 where id = ?`, serverID)
	if err != nil {
		return fmt.Errorf("clear gpu vendor note: %w", err)
	}
	return requireAffected(result)
}

// gpuVendorBackfillTable is one table BackfillServerGPUVendor rewrites: scope
// selects the server's rows (one serverID argument), same matches a row e to
// the row t of the same owner.
type gpuVendorBackfillTable struct {
	table, scope, same string
}

// gpuVendorBackfillTables are the GPU-keyed tables of a server.
var gpuVendorBackfillTables = []gpuVendorBackfillTable{
	{
		"agent_runtime_spec_gpus", `spec_id in (select s.id from agent_runtime_specs s
		join model_mappings m on m.id = s.mapping_id
		join applications a on a.id = m.application_id
		where a.server_id = ?)`,
		`e.spec_id = t.spec_id`,
	},
	{
		"ai_server_gpu_budgets", `server_id = ?`,
		`e.server_id = t.server_id`,
	},
	{
		"rpc_member_spec_gpus", `spec_id in (select id from rpc_member_specs where server_id = ?)`,
		`e.spec_id = t.spec_id`,
	},
	{
		"rpc_cluster_member_gpus", `server_id = ?`,
		`e.head_spec_id = t.head_spec_id and e.member_position = t.member_position`,
	},
}

// backfillGPUVendorTable backfills one table of serverID in the caller's
// transaction (run): it deletes the empty-vendor rows a row of vendor already
// shadows, then rewrites the remaining empty ones to vendor, and returns the
// rows both changed.
func backfillGPUVendorTable(run func(stmt string, args ...any) (int, error), t gpuVendorBackfillTable, serverID, vendor string) (int, error) {
	del := `delete from ` + t.table + ` as t where t.gpu_vendor = '' and ` + t.scope + `
			and exists (select 1 from ` + t.table + ` e where ` + t.same + ` and e.gpu_index = t.gpu_index and e.gpu_vendor = ?)`
	dropped, err := run(del, serverID, vendor)
	if err != nil {
		return 0, fmt.Errorf("backfill gpu vendor: drop shadowed %s rows: %w", t.table, err)
	}
	upd := `update ` + t.table + ` set gpu_vendor = ? where gpu_vendor = '' and ` + t.scope
	updated, err := run(upd, vendor, serverID)
	if err != nil {
		return 0, fmt.Errorf("backfill gpu vendor: rewrite %s rows: %w", t.table, err)
	}
	return dropped + updated, nil
}
