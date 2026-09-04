# Plan material — AREA: Backend persistence for GPU ORDER (the `position` column end to end)

Scope: the `agent_runtime_spec_gpus.position` column through schema, migration
(both drivers), CRUD (sqlite + memory), and conformance. Everything below is
quoted from the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/gpu-selection`
(branch `gpu-selection`, current origin/main content). All paths are relative to
`gateway/backend/` unless stated otherwise.

Coordination note up front: the design (§4.1, §3.4) puts BOTH new columns —
`agent_runtime_spec_gpus.position` (this area) and
`agent_runtime_specs.visible_devices_mode` (area B) — in the SAME new migration
`73`. So `migration73Up` is a shared function. This area owns the `position`
half (column + backfill + CRUD + memory sort + conformance). Area B appends the
`visible_devices_mode` column + `'env'` default to the same `migration73Up`. The
full function is drafted below with the area-B line clearly flagged so the two
tasks do not collide on the migration number or the function body.

---

## 1. CURRENT STATE (exact excerpts, file:line)

### 1.1 The struct — `internal/routing/store.go:1295-1305`

```go
// RuntimeSpecGPU is one per-GPU VRAM demand row for a RuntimeSpec
// (agent_runtime_spec_gpus, migration 65), keyed by (SpecID, GPUIndex).
// VRAMEstimateMB is operator-owned (the portal writes it); VRAMMeasuredMB is
// agent-owned and written ONLY by UpdateRuntimeSpecGPUMeasured — the two
// never clobber each other.
type RuntimeSpecGPU struct {
	SpecID         string
	GPUIndex       int
	VRAMEstimateMB int
	VRAMMeasuredMB int
}
```
No `Position` field today.

### 1.2 The interface + doc comments — `internal/routing/store.go:1398-1408`

```go
	// SetRuntimeSpecGPUs atomically REPLACES the whole set of per-GPU VRAM
	// rows for specID (delete-then-insert in one transaction, mirroring
	// SetGroupMembers). An empty gpus clears the set. specID must exist
	// (ErrNotFound otherwise).
	SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []RuntimeSpecGPU) error
	// RuntimeSpecGPUs lists specID's per-GPU VRAM rows, ordered by GPU
	// index. Always non-nil, empty when none.
	RuntimeSpecGPUs(ctx context.Context, specID string) ([]RuntimeSpecGPU, error)
	// UpdateRuntimeSpecGPUMeasured writes back one agent measurement; ErrNotFound
	// when the (spec,gpu) row does not exist. Callers skip specs with VRAMLocked.
	UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex int, measuredMB int) error
```
Doc says "ordered by GPU index" — must become "ordered by position".

### 1.3 Create-table (migration 65) — `internal/store/migrate.go:2879-2885`

```go
		`create table if not exists agent_runtime_spec_gpus (
			spec_id text not null references agent_runtime_specs(id) on delete cascade,
			gpu_index integer not null,
			vram_estimate_mb integer not null default 0,
			vram_measured_mb integer not null default 0,
			primary key (spec_id, gpu_index)
		)`,
```
Do NOT edit this create-table (append-only rule — migration 65 already ran on
every dev DB and both CI legs; the new column arrives via a new migration).

### 1.4 Migration registration slice + CURRENT HIGHEST number — `internal/store/migrate.go:34, 98-105`

The slice begins at line 34 (`{version: 1, name: "baseline", up: baselineUp},`).
The current highest registered migration is:

```go
	{version: 68, name: "application_single_server_agent", up: migration68Up},
	{version: 69, name: "runtime_spec_set_visible_devices", up: migration69Up},
	{version: 70, name: "application_proxy_excluded", up: migration70Up},
	{version: 71, name: "model_mapping_benchmarks_vram_json", up: migration71Up},
	{version: 72, name: "application_endpoint_modes", up: migration72Up},   // <- line 105, HIGHEST
```
**Next migration number to use: `73`.** (Spec §3.4 predicted 73 — confirmed.)

### 1.5 `addColumnIfMissing` helper — `internal/store/migrate.go:198-210`

```go
func addColumnIfMissing(ctx context.Context, tx *sql.Tx, dl dialect, table, colDef string) error {
	stmt := "alter table " + table + " add column " + colDef
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}
```

### 1.6 Backfill precedents

- `migration70Up` (`migrate.go:3077-3084`) — add-column-then-deterministic-UPDATE,
  aborts boot on failure, no idempotency guard (version-gated → runs once):
```go
func migration70Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if err := addColumnIfMissing(ctx, tx, dl, "applications",
		"proxy_excluded integer not null default 0"); err != nil {
		return err
	}
	return execTx(ctx, tx, dl, `update applications set proxy_excluded = 1
		where scheme = 'https' and proxy_listen_port = 0`)
}
```
- `migration72Up` (`migrate.go:3137-3194`) — the cross-driver split precedent:
  postgres `UPDATE ... FROM join`, sqlite **correlated subqueries** (its comment:
  "modernc sqlite supports UPDATE ... FROM only since 3.33 and not across this
  join shape reliably — correlated subqueries are universally supported and
  identical semantics").
- `migration20Up` (`migrate.go:1174-1192`) — single-column add + backfill from a
  prior state.

### 1.7 sqlite CRUD — `internal/store/sqlite_runtime.go`

`SetRuntimeSpecGPUs` (delete-then-insert), INSERT column list at **:161-163**:
```go
		for _, g := range gpus {
			if _, err := tx.ExecContext(ctx, s.dl.rebind(`
				insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb)
				values (?, ?, ?, ?)`), specID, g.GPUIndex, g.VRAMEstimateMB, g.VRAMMeasuredMB); err != nil {
```
`RuntimeSpecGPUs` SELECT + `ORDER BY gpu_index` at **:179-190**:
```go
func (s *SQLiteStore) RuntimeSpecGPUs(ctx context.Context, specID string) ([]routing.RuntimeSpecGPU, error) {
	rows, err := s.query(ctx, `
		select spec_id, gpu_index, vram_estimate_mb, vram_measured_mb
		from agent_runtime_spec_gpus where spec_id = ? order by gpu_index`, specID)
	...
		var g routing.RuntimeSpecGPU
		if err := rows.Scan(&g.SpecID, &g.GPUIndex, &g.VRAMEstimateMB, &g.VRAMMeasuredMB); err != nil {
```
(`s.exec`/`s.query`/`s.queryRow` all run through `s.dl.rebind(...)` +
`sanitizeArgs(...)` — `sqlite.go:70,74,78` — so `?` placeholders auto-convert to
`$N` on postgres. Both `SetRuntimeSpecGPUs`' loop uses explicit `s.dl.rebind`.)

### 1.8 memory store — `internal/routing/memory_store.go`

`SetRuntimeSpecGPUs` (**:2332-2355**) already deep-copies the WHOLE struct via
`copyRuntimeSpecGPUs` (**:2392-2396**, `make`+`copy`), so it will persist a new
`Position` field with no change to the setter. `RuntimeSpecGPUs` (**:2360-2366**)
sorts:
```go
func (m *MemoryStore) RuntimeSpecGPUs(_ context.Context, specID string) ([]RuntimeSpecGPU, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := copyRuntimeSpecGPUs(m.runtimeSpecGPUs[specID])
	sort.Slice(out, func(i, j int) bool { return out[i].GPUIndex < out[j].GPUIndex })
	return out, nil
}
```
Field comment at **:100-104** ("`RuntimeSpecGPUs` sorts by GPUIndex on read")
should be refreshed to "sorts by Position on read".

### 1.9 Conformance test GPU round-trip — `internal/store/routing_store_conformance_test.go`

Harness `forEachRoutingStore` (**:32-59**) runs each subtest against `memory`
and a fresh migrated `sqlite` *SQLStore*; the postgres leg is added by the
`forEachDialect` suite in `conformance_test.go` gated on
`OP_AI_GATEWAY_TEST_POSTGRES_DSN` (**:40-42**). The GPU round-trip lives in
`TestRoutingStoreRuntimeSpecs` (**:481-711**). The relevant block, **:641-659**,
currently sets NO position and asserts ascending-`gpu_index` order:
```go
		gpus := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_rt2", GPUIndex: 1, VRAMEstimateMB: 21500},
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 22000},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", gpus); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gotGPUs, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if err != nil || len(gotGPUs) != 2 || gotGPUs[0].GPUIndex != 0 || gotGPUs[1].GPUIndex != 1 {
			t.Fatalf("gpus must read ordered by index: %v %+v", err, gotGPUs)
		}

		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_rt2", 0, 21800); err != nil {
			t.Fatalf("measured: %v", err)
		}
		gotGPUs, _ = s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if gotGPUs[0].VRAMMeasuredMB != 21800 || gotGPUs[0].VRAMEstimateMB != 22000 {
			t.Fatalf("measured must not clobber estimate: %+v", gotGPUs[0])
		}
```
The dup-index, clear, and cascade assertions further down (**:664-706**) use
single or same-index rows and do NOT assert order, so they are position-agnostic
and need no change.

Other GPU test spots that need NO change (single-row, no order assertion):
`internal/store/delete_ai_server_cascade_test.go:213-214, 397-402, 497-524`.

### 1.10 Portal write-side (CONSUMED, area boundary) — `internal/portal/service_runtime.go:605-614`

```go
	gpuRows := make([]routing.RuntimeSpecGPU, 0, len(req.GPUs))
	for _, g := range req.GPUs {
		gpuRows = append(gpuRows, routing.RuntimeSpecGPU{
			SpecID:         spec.ID,
			GPUIndex:       g.Index,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: measuredByIndex[g.Index],
		})
	}
	if err := s.routes.SetRuntimeSpecGPUs(ctx, spec.ID, gpuRows); err != nil {
```
The portal (a DIFFERENT area) will change this loop to `for i, g := range req.GPUs`
and set `Position: i`. **This store area does NOT touch this file** — it only
persists whatever `Position` the caller put on each element. Recommendation
below: `SetRuntimeSpecGPUs` TRUSTS `spec[i].Position` (does not renumber).

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized)

Run commands from `gateway/backend/`. Whole-area gate:
```
cd gateway/backend && go test ./internal/store/... ./internal/routing/...
```
Focused store round-trip (memory + sqlite):
```
cd gateway/backend && go test ./internal/store/ -run TestRoutingStoreRuntimeSpecs -v
```
Postgres leg (requires a DSN; runs the `forEachDialect` conformance in the same package):
```
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://…' go test ./internal/store/ -run 'TestRoutingStore|TestConformance'
```

### Task S1 — add `Position int` to the struct + refresh interface docs
(prerequisite so the RED test compiles; no behaviour yet)

`internal/routing/store.go` — struct (:1300-1305):
```go
type RuntimeSpecGPU struct {
	SpecID   string
	GPUIndex int
	// Position is the operator-chosen order of this GPU within its spec
	// (0-based, dense per spec). It is the sole ordering contract: both stores
	// read the rows `order by position`, and the DTO/wire layers keep the
	// resulting ARRAY order (no position field crosses the wire). The portal's
	// putRuntimeSpec sets Position = the index in the received req.GPUs array;
	// SetRuntimeSpecGPUs persists it verbatim (it does not renumber).
	Position       int
	VRAMEstimateMB int
	VRAMMeasuredMB int
}
```
Interface doc comments (:1403-1405): change "ordered by GPU index" → "ordered by
position (the operator order)". Update the field comment in
`internal/routing/memory_store.go:100-104` ("sorts by GPUIndex on read" → "sorts
by Position on read").

Compile check (no test asserts Position yet, so this alone stays green):
```
cd gateway/backend && go build ./...
```
Expected: builds.

### Task S2 — RED: extend the conformance round-trip to a NON-ascending order

Replace the block at `routing_store_conformance_test.go:641-659` with (real
identifiers, same file/framework/style):
```go
		// GPU rows read back in POSITION order, which is independent of
		// gpu_index. Written with the higher gpu_index (1) at position 0, so a
		// store that still orders by gpu_index reads them in the wrong order.
		gpus := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_rt2", GPUIndex: 1, VRAMEstimateMB: 21500, Position: 0},
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 22000, Position: 1},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", gpus); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gotGPUs, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if err != nil || len(gotGPUs) != 2 {
			t.Fatalf("set/read gpus: %v %+v", err, gotGPUs)
		}
		if gotGPUs[0].GPUIndex != 1 || gotGPUs[0].Position != 0 ||
			gotGPUs[1].GPUIndex != 0 || gotGPUs[1].Position != 1 {
			t.Fatalf("gpus must read ordered by position, not gpu_index: %+v", gotGPUs)
		}

		// The measured write targets gpu_index 0, which now sits at position 1
		// (read index 1) — proving the write path keys on gpu_index while the
		// read path keys on position.
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_rt2", 0, 21800); err != nil {
			t.Fatalf("measured: %v", err)
		}
		gotGPUs, _ = s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if gotGPUs[1].GPUIndex != 0 || gotGPUs[1].VRAMMeasuredMB != 21800 || gotGPUs[1].VRAMEstimateMB != 22000 {
			t.Fatalf("measured must not clobber estimate: %+v", gotGPUs[1])
		}
```
Run:
```
cd gateway/backend && go test ./internal/store/ -run TestRoutingStoreRuntimeSpecs -v
```
Expected: **FAIL** on both `memory` and `sqlite` subtests at
`"gpus must read ordered by position, not gpu_index"` — both stores still sort
by `gpu_index`, returning `[gpu0, gpu1]` while the test wants `[gpu1(pos0),
gpu0(pos1)]`. (Compiles because Task S1 added the field.)

### Task S3 — migration 73: add `position` column + per-spec backfill (SHARED fn)

Register in the slice after line 105 (`internal/store/migrate.go`):
```go
	{version: 73, name: "runtime_spec_gpu_position_and_visible_devices_mode", up: migration73Up},
```
Add the function at the end of the migration block (after `migration72Up`,
~line 3195). The `position` half is this area; the flagged line is area B's:
```go
// migration73Up adds two additive columns that ship together with the
// GPU-selection feature:
//   - agent_runtime_spec_gpus.position: the operator-chosen GPU order. Backfilled
//     to the ascending-gpu_index rank per spec, so NO existing spec's env var /
//     ${…} placeholder order changes on upgrade — the order only moves when an
//     operator actively reorders (design §3.4).
//   - agent_runtime_specs.visible_devices_mode: the env/args visibility mechanism
//     (design §4.1), default 'env' = today's behaviour. [OWNED BY AREA B.]
//
// Append-only: it does NOT touch migration65Up's create-table or the v60-frozen
// baseline (a fresh install replays this; the backfill is a no-op on empty
// tables). It ABORTS the boot on failure (deterministic writes, no possibly-dirty
// pre-check), following migration70Up/72Up rather than migration68Up's skip.
//
// Backfill portability: `position = (count of same-spec rows with a smaller
// gpu_index)` is a correlated subquery identical on sqlite (modernc) and
// postgres, mirroring migration72Up's choice of correlated subqueries over
// UPDATE…FROM for cross-driver parity. It reads only gpu_index (never the column
// being written), so update order cannot perturb the counts.
func migration73Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	// --- GPU order (this area) ---
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_spec_gpus",
		"position integer not null default 0"); err != nil {
		return err
	}
	if err := execTx(ctx, tx, dl, `update agent_runtime_spec_gpus
		set position = (
			select count(*) from agent_runtime_spec_gpus g2
			where g2.spec_id = agent_runtime_spec_gpus.spec_id
			  and g2.gpu_index < agent_runtime_spec_gpus.gpu_index
		)`); err != nil {
		return err
	}

	// --- Visibility mechanism (AREA B — added by that task, shown for context) ---
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs",
		"visible_devices_mode text not null default 'env'"); err != nil {
		return err
	}
	return nil
}
```
Notes for the plan author:
- The `default 'env'` on `visible_devices_mode` means existing rows read back
  `env` with no backfill UPDATE needed (area B), symmetric to how migration69Up
  needed no backfill for its default-0 flag.
- If area B is sequenced separately and this area lands first, drop the area-B
  block and area B appends it — but the canonical decision (spec §4.1) is ONE
  migration; prefer the combined body above and make it a single plan task with
  two owners, or have area B's task edit `migration73Up` in place.

No standalone command; validated by S5's green run. (Optionally a schema-only
sanity check exists via the migrate suite — see GOTCHAS.)

### Task S4 — sqlite CRUD: write + read `position`

`internal/store/sqlite_runtime.go` INSERT (:161-163):
```go
		for _, g := range gpus {
			if _, err := tx.ExecContext(ctx, s.dl.rebind(`
				insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb, position)
				values (?, ?, ?, ?, ?)`), specID, g.GPUIndex, g.VRAMEstimateMB, g.VRAMMeasuredMB, g.Position); err != nil {
```
`RuntimeSpecGPUs` SELECT + ORDER BY + scan (:179-192):
```go
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
```
`ORDER BY position, gpu_index` — `gpu_index` is a deterministic tiebreak (positions
are dense/unique per spec after backfill and on every write, but the secondary
key keeps the read stable and matches the memory comparator). `SetRuntimeSpecGPUs`
TRUSTS `g.Position` (no renumber) — the simplest split and what the portal
contract in §1.10 expects. Do not add position to `UpdateRuntimeSpecGPUMeasured`
(it targets a row by `(spec_id, gpu_index)` and touches only `vram_measured_mb`).

Must land AFTER S3 (the INSERT references the new column).

### Task S5 — GREEN: memory store sorts by `position`

`internal/routing/memory_store.go` `RuntimeSpecGPUs` (:2360-2366):
```go
func (m *MemoryStore) RuntimeSpecGPUs(_ context.Context, specID string) ([]RuntimeSpecGPU, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := copyRuntimeSpecGPUs(m.runtimeSpecGPUs[specID])
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].GPUIndex < out[j].GPUIndex
	})
	return out, nil
}
```
`SetRuntimeSpecGPUs` needs NO change — `copyRuntimeSpecGPUs` (`make`+`copy`)
already carries the whole struct incl. `Position`, and the setter must keep
trusting the caller's `Position` (parity with sqlite).

Run:
```
cd gateway/backend && go test ./internal/store/ ./internal/routing/ -run 'TestRoutingStoreRuntimeSpecs'
```
Expected: **PASS** (memory + sqlite). Then the full area gate:
```
cd gateway/backend && go test ./internal/store/... ./internal/routing/...
```
Expected: PASS. With a DSN set, the postgres leg of `TestRoutingStoreRuntimeSpecs`
(via `forEachDialect`/`forEachRoutingStore`… note: the memory-vs-SQL suite only
runs memory+sqlite; postgres round-trip is covered by writing an equivalent
assertion into a `forEachDialect` conformance case if the plan wants an explicit
postgres position round-trip — see GOTCHAS) also passes.

---

## 3. INTERFACES

### PRODUCES (this area → other areas)
- Go field: `routing.RuntimeSpecGPU.Position int` (0-based, dense per spec).
- DB column: `agent_runtime_spec_gpus.position` — `integer not null default 0`.
- Read contract: BOTH stores return `RuntimeSpecGPUs(...)` ordered by
  `position` (sqlite `order by position, gpu_index`; memory `sort.Slice` Position
  then GPUIndex). The **array order IS the contract** for every downstream (DTO,
  agent wire, `hostGPUIDs`, `${…_DEVICES}` placeholders). No `position` key is
  added to `RuntimeSpecGPUDTO` / `AgentRuntimeSpecGPUDTO` (design §3.1).
- Migration: `migration73Up` (version `73`, name
  `runtime_spec_gpu_position_and_visible_devices_mode`). Shared with area B.
- Write contract: `SetRuntimeSpecGPUs(ctx, specID, gpus)` persists each
  `gpus[i].Position` verbatim — it does NOT assign/renumber. (Recommended:
  trust the caller; simplest, matches the portal setting `Position = i`.)

### CONSUMES (other areas → this area)
- Portal `putRuntimeSpec` (`internal/portal/service_runtime.go:605-613`, AREA:
  portal) must set `Position: i` when building `gpuRows` from `req.GPUs`. Until
  it does, every write stores `Position = 0` for all rows and the read order
  falls back to the `gpu_index` tiebreak — harmless but order-losing, so the
  portal change is a hard dependency for the end-to-end ordering feature.
- Area B appends `agent_runtime_specs.visible_devices_mode` (`text not null
  default 'env'`) to `migration73Up`; `routing.RuntimeSpec.VisibleDevicesMode`
  (typed string enum `"env"`/`"args"`, mirroring `routing.EndpointMode` in
  `internal/routing/endpoint_mode.go:17-22`) and its scan/insert in
  `sqlite_runtime.go` `UpsertRuntimeSpec`/`scanRuntimeSpec` are area B's, not
  this area's.

---

## 4. GOTCHAS

- **Test framework & how to run.** Standard Go `testing`, table/subtest style.
  Store conformance is memory-vs-SQL via `forEachRoutingStore`
  (`routing_store_conformance_test.go:32-59`) — runs `memory` + a fresh migrated
  `sqlite`. The dialect (sqlite-vs-postgres) conformance is a SEPARATE suite,
  `forEachDialect` in `conformance_test.go` (`:40-42`), postgres-gated on
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN`. Command:
  `cd gateway/backend && go test ./internal/store/... ./internal/routing/...`.
- **`forEachRoutingStore` does NOT run postgres.** `TestRoutingStoreRuntimeSpecs`
  proves memory-vs-sqlite parity only. For an explicit postgres `position`
  round-trip, add a small assertion into a `forEachDialect` conformance test
  (e.g. beside `TestConformanceRoutingServerApplicationMapping` /
  `TestConformanceApplicationEndpointModes` in `conformance_test.go`) that writes
  two GPU rows with reversed position and reads them back — that case runs on
  sqlite AND postgres when the DSN is set. The migration itself IS exercised on
  postgres by every `forEachDialect` test (each opens a freshly migrated DB), so
  a broken `migration73Up` on postgres fails the whole package under a DSN.
- **Append-only migration discipline.** Never edit migration 65's create-table
  or the frozen v60 baseline. The new column arrives ONLY via `migration73Up`
  (`migrate.go` comment at `:3026-3033`/`:3074-3076` documents exactly this
  reasoning). `addColumnIfMissing` is duplicate-column tolerant on both dialects.
- **Cross-driver backfill.** Use the correlated-subquery form (portable to both
  sqlite/modernc and postgres) — NOT `row_number() over (partition by …)` and NOT
  `UPDATE…FROM`. Precedent + rationale: `migration72Up` (`:3168-3193`). The
  subquery counts same-spec rows with a smaller `gpu_index`, yielding the 0-based
  ascending-`gpu_index` rank, which preserves today's order exactly (design §3.4:
  "no existing spec's env var / placeholder order changes on upgrade").
- **No idempotency guard needed.** Migrations are version-gated (run exactly
  once), so the deterministic backfill needs no `where position = 0` guard — and
  `0` is a legitimate position for the first row anyway, so such a guard would be
  wrong. Matches `migration70Up` (also unguarded).
- **`ORDER BY position, gpu_index` (secondary key).** Positions are unique/dense
  per spec after backfill and on every portal write, so the tiebreak is
  defensive; keep it so a hypothetical duplicate/zero position still reads
  deterministically and identically to the memory comparator. Keep the two
  comparators in lock-step (this is exactly the memory-vs-SQL divergence class
  the conformance suite exists to catch — see the nil-vs-empty and dup-index
  cases already in `TestRoutingStoreRuntimeSpecs`).
- **`SetRuntimeSpecGPUs` trust-vs-assign.** Confirmed signature:
  `SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error`
  (store.go:1402; sqlite_runtime.go:144; memory_store.go:2332). Recommendation:
  **trust the caller's `Position`** (persist `g.Position`), do not have the store
  assign `Position = loop index`. Rationale: the store is a dumb persistence
  layer everywhere else (e.g. it does not sort/rewrite co-residency pairs); the
  portal already owns request→row shaping and will set `Position = i`; assigning
  inside the store would silently mask a portal bug and duplicate the ordering
  authority. Simplest and consistent.
- **Memory deep-copy is safe.** `copyRuntimeSpecGPUs` (`make`+`copy`,
  memory_store.go:2392-2396) copies the full struct, so `Position` round-trips
  with zero setter changes; the value type means no aliasing of internal state
  (design §3.4: "memory store deep-copied unaffected").
- **Untouched GPU tests.** `delete_ai_server_cascade_test.go` GPU rows are
  single/same-index with no order assertion → position-agnostic, no edits.
- **Spec ambiguity resolved.** (1) Migration number → **73** (confirmed highest
  is 72 at `migrate.go:105`). (2) `position`/`visible_devices_mode` share
  `migration73Up` (spec §3.4/§4.1 say one migration) — this bundle drafts the
  combined body with the area-B line flagged. (3) Backfill SQL → correlated
  subquery (portable), not the window function the spec floated as an option.
