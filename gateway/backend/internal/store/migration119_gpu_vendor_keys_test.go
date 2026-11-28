// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"testing"
	"time"
)

// migration119GPUTables lists the four GPU tables that gain gpu_vendor.
var migration119GPUTables = []string{
	"agent_runtime_spec_gpus", "ai_server_gpu_budgets", "rpc_member_spec_gpus", "rpc_cluster_member_gpus",
}

// seedMigration119 writes, with raw SQL against a genuine version-118
// database, one row set per table migration 119 rebuilds: a spec with two GPU
// rows, a budget, a manual member spec with a GPU row, and a head with two
// members on two servers whose positions do NOT follow their server ids (so a
// backfill that took the wrong member's position would show).
func seedMigration119(ctx context.Context, t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	seedRPCHeadSpecAtV105(ctx, t, s, "m119", now)
	seedRPCHeadSpecAtV105(ctx, t, s, "m119b", now)
	seedBaselineServerAt(ctx, t, s, "srv_m119_a", now)
	seedBaselineServerAt(ctx, t, s, "srv_m119_b", now)

	mustExec(ctx, t, s, `insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb, position)
		values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"rspec_m119", 0, 1000, 900, 1,
		"rspec_m119", 1, 2000, 0, 0)
	mustExec(ctx, t, s, `insert into ai_server_gpu_budgets (server_id, gpu_index, budget_mb, expected_uuid, expected_name, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		"srv_m119_a", 0, 8000, "GPU-a0", "Card A0", now, now)
	mustExec(ctx, t, s, `insert into rpc_member_specs (id, server_id, created_at, updated_at) values (?, ?, ?, ?)`,
		"rpcm_m119", "srv_m119_a", now, now)
	mustExec(ctx, t, s, `insert into rpc_member_spec_gpus (spec_id, gpu_index, position, vram_estimate_mb) values (?, ?, ?, ?)`,
		"rpcm_m119", 0, 0, 5000)
	mustExec(ctx, t, s, `insert into rpc_cluster_members (head_spec_id, position, server_id, member_spec_id, port)
		values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"rspec_m119", 0, "srv_m119_b", "", 47010,
		"rspec_m119", 1, "srv_m119_a", "rpcm_m119", 47011)
	mustExec(ctx, t, s, `insert into rpc_cluster_member_gpus (head_spec_id, server_id, gpu_index, position, vram_estimate_mb)
		values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"rspec_m119", "srv_m119_b", 0, 0, 3000,
		"rspec_m119", "srv_m119_b", 1, 1, 3100,
		"rspec_m119", "srv_m119_a", 0, 0, 4000)
}

// countRows counts a table's rows matching where (a fixed predicate, args bound).
func countRows(ctx context.Context, t *testing.T, s *SQLStore, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from `+table+` where `+where), args...).Scan(&n); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return n
}

// mustFailExec runs a statement that a constraint must reject.
func mustFailExec(ctx context.Context, t *testing.T, s *SQLStore, why, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(q), args...); err == nil {
		t.Fatalf("%s: %q succeeded, want a constraint violation", why, q)
	}
}

// TestMigration119GPUVendorKeys runs migration 119 against a genuine
// version-118 database holding rows in every table it rebuilds, and pins what
// mixed-vendor hosts (#434) depend on: every row survives with the empty
// gpu_vendor (the host's only vendor), member GPU rows point at their member
// by position, one GPU index may now exist once per vendor, one server may
// serve a head twice, the cascades still hold, the two ai_servers columns
// exist with their defaults, and a second Migrate changes nothing.
func TestMigration119GPUVendorKeys(t *testing.T) {
	forEachDialectMigratedTo(t, 118, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

		for _, table := range migration119GPUTables {
			if _, ok := tableColumnDecls(ctx, t, s, table)["gpu_vendor"]; ok {
				t.Fatalf("%s already has gpu_vendor at v118", table)
			}
		}
		seedMigration119(ctx, t, s, now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		// Column shapes: gpu_vendor like the table family's free-text column
		// (ai_server_gpu_budgets.expected_uuid: text, not null, default ''),
		// member_position like the GPU row's own position, the note flag like
		// the server's other integer settings.
		budgetDecls := tableColumnDecls(ctx, t, s, "ai_server_gpu_budgets")
		textDecl := budgetDecls["expected_uuid"]
		for _, table := range migration119GPUTables {
			decls := tableColumnDecls(ctx, t, s, table)
			if got, ok := decls["gpu_vendor"]; !ok || got != textDecl {
				t.Fatalf("%s.gpu_vendor = %q (present=%v), want %q", table, got, ok, textDecl)
			}
		}
		memberGPUDecls := tableColumnDecls(ctx, t, s, "rpc_cluster_member_gpus")
		if got, want := memberGPUDecls["member_position"], memberGPUDecls["position"]; got != want {
			t.Fatalf("rpc_cluster_member_gpus.member_position = %q, want %q", got, want)
		}
		if _, ok := memberGPUDecls["server_id"]; !ok {
			t.Fatalf("rpc_cluster_member_gpus.server_id must be kept as a plain column")
		}
		serverDecls := tableColumnDecls(ctx, t, s, "ai_servers")
		if got := serverDecls["gpu_legacy_vendor"]; got != textDecl {
			t.Fatalf("ai_servers.gpu_legacy_vendor = %q, want %q", got, textDecl)
		}
		if got, want := serverDecls["gpu_vendor_note"], serverDecls["runtime_max_processes"]; got != want {
			t.Fatalf("ai_servers.gpu_vendor_note = %q, want %q", got, want)
		}

		// Every seeded row survives, untouched, with the legacy empty vendor.
		assertMigration119Seed := func(stage string) {
			t.Helper()
			if n := countRows(ctx, t, s, "agent_runtime_spec_gpus",
				`spec_id = 'rspec_m119' and gpu_vendor = '' and ((gpu_index = 0 and vram_estimate_mb = 1000 and vram_measured_mb = 900 and position = 1)
				or (gpu_index = 1 and vram_estimate_mb = 2000 and vram_measured_mb = 0 and position = 0))`); n != 2 {
				t.Fatalf("%s: spec gpu rows = %d, want the 2 seeded rows with vendor ''", stage, n)
			}
			if n := countRows(ctx, t, s, "ai_server_gpu_budgets",
				`server_id = 'srv_m119_a' and gpu_vendor = '' and gpu_index = 0 and budget_mb = 8000 and expected_uuid = 'GPU-a0' and expected_name = 'Card A0'`); n != 1 {
				t.Fatalf("%s: budget rows = %d, want the seeded row with vendor ''", stage, n)
			}
			if n := countRows(ctx, t, s, "rpc_member_spec_gpus",
				`spec_id = 'rpcm_m119' and gpu_vendor = '' and gpu_index = 0 and vram_estimate_mb = 5000`); n != 1 {
				t.Fatalf("%s: member spec gpu rows = %d, want the seeded row with vendor ''", stage, n)
			}
			if n := countRows(ctx, t, s, "rpc_cluster_members",
				`head_spec_id = 'rspec_m119' and ((position = 0 and server_id = 'srv_m119_b' and member_spec_id = '' and port = 47010)
				or (position = 1 and server_id = 'srv_m119_a' and member_spec_id = 'rpcm_m119' and port = 47011))`); n != 2 {
				t.Fatalf("%s: member rows = %d, want the 2 seeded rows", stage, n)
			}
			// member_position is the position of the member on the same server.
			if n := countRows(ctx, t, s, "rpc_cluster_member_gpus",
				`head_spec_id = 'rspec_m119' and gpu_vendor = '' and (
					(server_id = 'srv_m119_b' and member_position = 0 and gpu_index = 0 and position = 0 and vram_estimate_mb = 3000)
					or (server_id = 'srv_m119_b' and member_position = 0 and gpu_index = 1 and position = 1 and vram_estimate_mb = 3100)
					or (server_id = 'srv_m119_a' and member_position = 1 and gpu_index = 0 and position = 0 and vram_estimate_mb = 4000))`); n != 3 {
				t.Fatalf("%s: member gpu rows = %d, want the 3 seeded rows with vendor '' and their member's position", stage, n)
			}
			var legacy string
			var note int
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select gpu_legacy_vendor, gpu_vendor_note from ai_servers where id = ?`),
				"srv_m119_a").Scan(&legacy, &note); err != nil {
				t.Fatalf("%s: read ai_servers vendor columns: %v", stage, err)
			}
			if legacy != "" || note != 0 {
				t.Fatalf("%s: a pre-existing server must read gpu_legacy_vendor '' and gpu_vendor_note 0, got %q/%d", stage, legacy, note)
			}
		}
		assertMigration119Seed("after migration")

		// A second Migrate is a no-op: no error, one ledger row, data as is.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		if n := countRows(ctx, t, s, "schema_migrations", `version = 119`); n != 1 {
			t.Fatalf("schema_migrations holds %d rows for 119, want 1", n)
		}
		assertMigration119Seed("after a second migrate")

		for _, name := range []string{
			"idx_rpc_cluster_members_server", "idx_rpc_cluster_members_member_spec", "idx_rpc_cluster_members_server_port",
		} {
			if !indexExists(t, s, name) {
				t.Fatalf("index %s is missing after migration 119", name)
			}
		}

		// The vendor is part of every GPU key: ('nvidia', 0) and ('amd', 0)
		// coexist next to the legacy ('', 0), a repeat of a full key does not.
		mustExec(ctx, t, s, `insert into agent_runtime_spec_gpus (spec_id, gpu_vendor, gpu_index) values (?, ?, ?), (?, ?, ?)`,
			"rspec_m119", "nvidia", 0, "rspec_m119", "amd", 0)
		mustFailExec(ctx, t, s, "repeated spec gpu key",
			`insert into agent_runtime_spec_gpus (spec_id, gpu_vendor, gpu_index) values (?, ?, ?)`, "rspec_m119", "amd", 0)
		mustExec(ctx, t, s, `insert into ai_server_gpu_budgets (server_id, gpu_vendor, gpu_index, created_at, updated_at)
			values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
			"srv_m119_a", "nvidia", 0, now, now, "srv_m119_a", "amd", 0, now, now)
		mustFailExec(ctx, t, s, "repeated budget key",
			`insert into ai_server_gpu_budgets (server_id, gpu_vendor, gpu_index, created_at, updated_at) values (?, ?, ?, ?, ?)`,
			"srv_m119_a", "nvidia", 0, now, now)
		mustExec(ctx, t, s, `insert into rpc_member_spec_gpus (spec_id, gpu_vendor, gpu_index) values (?, ?, ?), (?, ?, ?)`,
			"rpcm_m119", "nvidia", 0, "rpcm_m119", "amd", 0)
		mustFailExec(ctx, t, s, "repeated member spec gpu key",
			`insert into rpc_member_spec_gpus (spec_id, gpu_vendor, gpu_index) values (?, ?, ?)`, "rpcm_m119", "nvidia", 0)
		mustExec(ctx, t, s, `insert into rpc_cluster_member_gpus (head_spec_id, member_position, server_id, gpu_vendor, gpu_index)
			values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
			"rspec_m119", 0, "srv_m119_b", "nvidia", 0, "rspec_m119", 0, "srv_m119_b", "amd", 0)
		mustFailExec(ctx, t, s, "repeated member gpu key",
			`insert into rpc_cluster_member_gpus (head_spec_id, member_position, server_id, gpu_vendor, gpu_index) values (?, ?, ?, ?, ?)`,
			"rspec_m119", 0, "srv_m119_b", "amd", 0)

		// Members are keyed by position: one server serves a head twice, each
		// member with its own GPU rows under the same (vendor, index).
		mustExec(ctx, t, s, `insert into rpc_cluster_members (head_spec_id, position, server_id, port) values (?, ?, ?, ?), (?, ?, ?, ?)`,
			"rspec_m119b", 0, "srv_m119_a", 47020, "rspec_m119b", 1, "srv_m119_a", 47021)
		mustExec(ctx, t, s, `insert into rpc_cluster_member_gpus (head_spec_id, member_position, server_id, gpu_vendor, gpu_index)
			values (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
			"rspec_m119b", 0, "srv_m119_a", "nvidia", 0, "rspec_m119b", 1, "srv_m119_a", "nvidia", 0)
		mustFailExec(ctx, t, s, "repeated member position",
			`insert into rpc_cluster_members (head_spec_id, position, server_id) values (?, ?, ?)`, "rspec_m119b", 1, "srv_m119_b")
		mustFailExec(ctx, t, s, "a gpu row of a member that does not exist",
			`insert into rpc_cluster_member_gpus (head_spec_id, member_position, server_id, gpu_index) values (?, ?, ?, ?)`,
			"rspec_m119b", 5, "srv_m119_a", 0)
		// The recreated partial unique indexes still bite.
		mustFailExec(ctx, t, s, "a manual member spec serving a second head",
			`insert into rpc_cluster_members (head_spec_id, position, server_id, member_spec_id) values (?, ?, ?, ?)`,
			"rspec_m119b", 2, "srv_m119_a", "rpcm_m119")
		mustFailExec(ctx, t, s, "an allocated port taken twice on one server",
			`insert into rpc_cluster_members (head_spec_id, position, server_id, port) values (?, ?, ?, ?)`,
			"rspec_m119b", 2, "srv_m119_a", 47011)

		// Deleting a member row takes its own GPU rows, not its sibling's on
		// the same server; deleting the head takes everything.
		mustExec(ctx, t, s, `delete from rpc_cluster_members where head_spec_id = ? and position = ?`, "rspec_m119b", 1)
		if n := countRows(ctx, t, s, "rpc_cluster_member_gpus", `head_spec_id = 'rspec_m119b' and member_position = 1`); n != 0 {
			t.Fatalf("deleting a member must cascade its gpu rows, %d left", n)
		}
		if n := countRows(ctx, t, s, "rpc_cluster_member_gpus", `head_spec_id = 'rspec_m119b' and member_position = 0`); n != 1 {
			t.Fatalf("deleting a member must not touch its sibling's gpu rows, %d left of 1", n)
		}
		mustExec(ctx, t, s, `delete from agent_runtime_specs where id = ?`, "rspec_m119")
		for table, where := range map[string]string{
			"rpc_cluster_members":     `head_spec_id = 'rspec_m119'`,
			"rpc_cluster_member_gpus": `head_spec_id = 'rspec_m119'`,
			"agent_runtime_spec_gpus": `spec_id = 'rspec_m119'`,
		} {
			if n := countRows(ctx, t, s, table, where); n != 0 {
				t.Fatalf("deleting the head spec must cascade %s, %d rows left", table, n)
			}
		}
	})
}

// TestMigration119FreshInstall pins the shape a fresh install reaches (the
// rebuild then runs over empty tables): the store's own cluster writer and
// reader work on it.
func TestMigration119FreshInstall(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		head := seedRPCHeadSpec(ctx, t, s, "m119f", now)
		seedBinariesServer(ctx, t, s, "srv_m119f_a", now)
		seedBinariesServer(ctx, t, s, "srv_m119f_b", now)
		members := []routing.ClusterMember{
			{HeadSpecID: head, Position: 0, ServerID: "srv_m119f_b", DeviceBackend: "auto", Port: 47030},
			{HeadSpecID: head, Position: 1, ServerID: "srv_m119f_a", DeviceBackend: "auto", Port: 47031},
		}
		gpus := []routing.ClusterMemberGPU{
			{HeadSpecID: head, MemberPosition: 1, ServerID: "srv_m119f_a", GPUIndex: 0, VRAMEstimateMB: 1},
			{HeadSpecID: head, MemberPosition: 0, ServerID: "srv_m119f_b", GPUIndex: 0, VRAMEstimateMB: 2},
		}
		if err := s.SetClusterMembers(ctx, head, members, gpus); err != nil {
			t.Fatalf("set members: %v", err)
		}
		if n := countRows(ctx, t, s, "rpc_cluster_member_gpus",
			`(server_id = 'srv_m119f_a' and member_position = 1) or (server_id = 'srv_m119f_b' and member_position = 0)`); n != 2 {
			t.Fatalf("the writer must store each gpu row under its member's position, %d of 2 rows match", n)
		}
		_, got, err := s.ClusterMembers(ctx, head)
		if err != nil || len(got) != 2 {
			t.Fatalf("read back: %+v err=%v", got, err)
		}
	})
}

// TestMigration119IgnoresUnrelatedForeignKeyViolations pins that SQLite's
// post-rebuild foreign-key check is scoped to the five rebuilt tables: a
// dangling row elsewhere (here a manual member spec whose server is gone,
// written with foreign keys off as an old bug or a manual repair could have
// left it) is not the rebuild's doing and must not abort the migration and
// with it every later one.
func TestMigration119IgnoresUnrelatedForeignKeyViolations(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.migrateTo(ctx, 118); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA foreign_keys=OFF`,
		`insert into rpc_member_specs (id, server_id, created_at, updated_at) values ('rpcm_dangling', 'srv_gone', '` +
			now.Format(time.RFC3339) + `', '` + now.Format(time.RFC3339) + `')`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = conn.Close()

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("an unrelated dangling row must not abort migration 119: %v", err)
	}
	if n := countRows(ctx, t, s, "schema_migrations", "version = 119"); n != 1 {
		t.Fatalf("migration 119 recorded %d times, want 1", n)
	}
}
