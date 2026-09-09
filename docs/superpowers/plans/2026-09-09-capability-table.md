# Capabilities become their own table — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** one row per (mapping, capability) carrying the verdict, who established it, and when — replacing eleven columns on `model_mappings`. Per-capability provenance makes the operator's rule expressible: **a manual or benchmark verdict is never overridden by a probe.**

**Architecture:** additive first, authoritative second, destructive last. Task 1 creates the table, its store API and the backfill while every old column stays authoritative — the tree is green and behaviour is unchanged. Tasks 2–5 move the wire, the writers, the decision path and the portal onto rows. Task 6 drops the eleven columns and the struct fields once nothing reads or writes them. Task 7 documents it.

**Tech Stack:** Go 1.26 (two modules), SQLite via `modernc.org/sqlite` v1.53.0 + PostgreSQL through the store's `dialect` layer, React/TS portal.

**Spec:** `docs/superpowers/specs/2026-09-09-capability-table-design.md` (approved).

## Global Constraints

- **Absence of a row is "unknown".** `verdict` is only ever `'yes'` or `'no'`; `''` is never stored, and "nothing determined" is expressed by not writing a row. This replaces the tri-state string everywhere.
- **The precedence rule, in one sentence:** a probe writes a capability row only when there is no row for it, or the existing row's `source` is itself a probe source (`llama_cpp_props`, `legacy`). A row whose `source` is `manual` or `vision_benchmark` is **never** overwritten by a probe. This is the whole point of the change; every write path must honour it and every write path must have a test that fails when it is removed.
- **No `metrics_locked` guard anywhere on the new table**, and it never touches `metrics_source`/`metrics_updated_at`. That is the `UpdateMappingLiveProgressSupport` rationale, which survives the deletion of that method as the table's own rationale (see Task 3's citation sweep).
- **Behaviour must not change** for: the scorer's `+30` MTP bonus, `wantsLiveProgress`' three-layer rule, and the models-list fail-closed vision fold. Their existing tests are the gate — they must keep passing with **no assertion changes**. If one needs editing, stop and report it.
- **The nil-vs-all-empty wire distinction is load-bearing** (a 401/403-protected child reports non-nil-but-empty). The pointer wrapper stays; `capabilitiesSample`'s always-return-a-pointer contract stays.
- **The two `detectCapabilities` copies stay byte-identical** across the modules; this change does not touch them. A drift check runs in the final review.
- Every new/changed test must FAIL with its production change reverted. Revert **production files only** — reverting a test makes `go test -run X` report "ok" with zero tests, a fake pass. Where a revert cannot discriminate, mutate the specific guard and record which mutation broke which test.
- **Per-module gates before every commit:** touched Go module → `~/go/bin/golangci-lint fmt --diff` (must print nothing) + `~/go/bin/golangci-lint run` + `go test ./... -count=1`. Frontend → `npm run format:check` + `lint` + `build` + `test` in `gateway/frontend`. Docs → `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- `go test ./internal/gateway/ -race` has a pre-existing failure on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (issue #53) — use plain `go test` there and ignore it.
- Branch `capability-table`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/capability-table`. Never commit to `main`. Never use bare `git stash`. Never `cp -R` the worktree (its `.git` is a gitlink; a copy shares this worktree's git index) — copy individual FILES to `/tmp` for scratch/restore.
- `docs/superpowers/` is branch-local and removed before the PR.

---

### Task 1: The table, the store API, the backfill

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (registry ~:110; new `migration78Up` after `migration77Up` ~:3336; new `dropColumnIfPresent` beside `addColumnIfMissing` ~:203)
- Modify: `gateway/backend/internal/routing/store.go` (new `CapabilityRow` type + four interface methods, beside `CapabilityVerdicts` ~:1021)
- Create: `gateway/backend/internal/store/sqlite_capabilities.go`
- Modify: `gateway/backend/internal/routing/memory_store.go` (the four methods + the `deleteMappingLocked` cascade mirror ~:946-975)
- Test: `gateway/backend/internal/store/routing_store_conformance_test.go`, `gateway/backend/internal/store/delete_ai_server_cascade_test.go`, and a new migration test beside the existing migration tests (find them with `grep -rln "migration7[0-9]Up\|schema_migrations" gateway/backend/internal/store/*_test.go`)

**Interfaces:**
- Produces: `routing.CapabilityRow`, `routing.Store.MappingCapabilities`, `MappingCapabilitiesForMappings`, `UpsertMappingCapabilities`, `DeleteMappingCapability`, and the capability-name constants. Consumed by Tasks 3, 4, 5.
- **This task is purely additive:** no existing column is dropped, no existing reader or writer changes. The tree must be green and behaviour identical at the end of it.

- [ ] **Step 1: Write the failing conformance test**

Read `TestUpdateMappingCapabilities` and `TestUpdateMappingLiveProgressSupportIgnoresMetricsLock` in `routing_store_conformance_test.go` FIRST and copy their `forEachRoutingStore(t, func(t *testing.T, s routing.Store) { … })` shape and fixture construction (`CreateAIServer`/`CreateApplication`/`CreateMapping`) verbatim.

```go
// Capability rows round-trip, upsert in place rather than duplicating, and
// express "unknown" by absence. Both drivers, one test.
func TestMappingCapabilityRows(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
		// server "srv1" + application "app1" + mapping "m1", exactly as the
		// sibling tests build them.

		rows := []routing.CapabilityRow{
			{Capability: "vision", Verdict: "yes", Source: "llama_cpp_props", CheckedAt: now},
			{Capability: "tools", Verdict: "no", Source: "llama_cpp_props", CheckedAt: now},
		}
		if err := s.UpsertMappingCapabilities(ctx, "m1", rows); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2: %+v", len(got), got)
		}
		byName := map[string]routing.CapabilityRow{}
		for _, r := range got {
			byName[r.Capability] = r
		}
		if byName["vision"].Verdict != "yes" || byName["vision"].Source != "llama_cpp_props" {
			t.Fatalf("vision row = %+v", byName["vision"])
		}
		if byName["vision"].CheckedAt.IsZero() {
			t.Fatal("checked_at not stored")
		}
		if _, present := byName["audio"]; present {
			t.Fatal("an unwritten capability must be ABSENT, not a row -- absence is how unknown is expressed")
		}

		// Upsert replaces in place: same capability, new verdict, no duplicate row.
		later := now.Add(time.Hour)
		if err := s.UpsertMappingCapabilities(ctx, "m1", []routing.CapabilityRow{
			{Capability: "vision", Verdict: "no", Source: "vision_benchmark", CheckedAt: later},
		}); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		got, _ = s.MappingCapabilities(ctx, "m1")
		if len(got) != 2 {
			t.Fatalf("upsert duplicated a row: %d rows %+v", len(got), got)
		}
		for _, r := range got {
			if r.Capability == "vision" && (r.Verdict != "no" || r.Source != "vision_benchmark") {
				t.Fatalf("upsert did not replace: %+v", r)
			}
		}

		// Delete returns a capability to unknown -- the state the old bool
		// could not express.
		if err := s.DeleteMappingCapability(ctx, "m1", "vision"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got, _ = s.MappingCapabilities(ctx, "m1")
		for _, r := range got {
			if r.Capability == "vision" {
				t.Fatal("delete left the row behind")
			}
		}

		// The batch reader returns exactly the requested parents, and an
		// unknown id contributes no key at all (not an empty slice).
		mustCreateMapping(t, s, "m2", "app1") // adapt to the sibling fixtures' helper/inline shape
		if err := s.UpsertMappingCapabilities(ctx, "m2", []routing.CapabilityRow{
			{Capability: "mtp", Verdict: "yes", Source: "legacy", CheckedAt: now},
		}); err != nil {
			t.Fatalf("upsert m2: %v", err)
		}
		batch, err := s.MappingCapabilitiesForMappings(ctx, []string{"m1", "m2", "nope"})
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if len(batch["m2"]) != 1 || batch["m2"][0].Capability != "mtp" {
			t.Fatalf("batch m2 = %+v", batch["m2"])
		}
		if _, present := batch["nope"]; present {
			t.Fatal("batch reader invented a key for an unknown mapping")
		}
		if len(batch["m1"]) != len(mustRows(t, s, "m1")) {
			t.Fatal("batch and single reader disagree")
		}
	})
}
```

Adapt the two `// adapt` lines to the file's own fixture style (there is no `mustCreateMapping`/`mustRows` helper today — either inline the `CreateMapping` call as the sibling tests do, or add local helpers in that file's idiom).

Then extend the cascade test. `TestRoutingStoreDeleteApplicationAndMappingCascade` (`delete_ai_server_cascade_test.go:454`) carries a comment demanding a reader per per-mapping table — add the capability reader to it, so a MemoryStore that forgets the mirror fails here.

- [ ] **Step 2: Run them, verify they fail**

Run: `cd gateway/backend && go test ./internal/store/ -run 'TestMappingCapabilityRows|TestRoutingStoreDeleteApplicationAndMappingCascade' -v 2>&1 | head -30`
Expected: compile failure (the type and the four methods are undefined) — the valid failing state.

- [ ] **Step 3: The type, the constants, the interface**

In `gateway/backend/internal/routing/store.go`, beside `CapabilityVerdicts`:

```go
// CapabilityRow is one (mapping, capability) verdict together with WHO
// established it and when — the unit the model_mapping_capabilities table
// stores, one row per capability.
//
// Verdict is "yes" or "no" and nothing else: "unknown" is the ABSENCE of a
// row, not a third value. That is what makes the never-overwrite-with-unknown
// discipline structural instead of a convention every writer has to remember
// — there is no empty verdict to accidentally write.
//
// Source names the writer, and it is what makes the operator's rule
// expressible at all: a probe must never overwrite a verdict a human or a
// real measurement established. See CapabilitySource* below and
// UpsertMappingCapabilities.
type CapabilityRow struct {
	Capability string
	Verdict    string
	Source     string
	CheckedAt  time.Time
}

// Capability names. The vocabulary is deliberately OPEN — an upstream may
// report capabilities this codebase has never heard of (Ollama passes
// manifest-declared names through verbatim), and those are stored and
// displayed as-is rather than dropped. These constants exist only for the
// capabilities the code itself reasons about.
const (
	CapabilityVision       = "vision"
	CapabilityVideo        = "video"
	CapabilityAudio        = "audio"
	CapabilityTools        = "tools"
	CapabilityMTP          = "mtp"
	CapabilityLiveProgress = "live_progress"
)

// Capability verdicts.
const (
	CapabilityYes = "yes"
	CapabilityNo  = "no"
)

// Capability sources. manual and vision_benchmark are AUTHORITATIVE: a probe
// never overwrites a row carrying one of them (the operator's rule). The
// probe sources are overwritable by a later probe, which is what lets a
// verdict re-establish itself after a build changes.
//
// CapabilitySourceLegacy marks a verdict inherited by migration 78 from a
// pre-78 column whose real origin is unknowable (is_mtp came from a NAME
// HEURISTIC or an operator; vision_capable's provenance was a mapping-wide
// string every writer overwrote). It is deliberately probe-overwritable:
// treating a guess as authoritative would freeze it in forever.
const (
	CapabilitySourceManual          = "manual"
	CapabilitySourceVisionBenchmark = "vision_benchmark"
	CapabilitySourceLlamaCppProps   = "llama_cpp_props"
	CapabilitySourceLegacy          = "legacy"
)

// CapabilitySourceIsAuthoritative reports whether source outranks a probe.
// The single place the precedence rule is spelled out; every write path asks
// it rather than repeating the comparison.
func CapabilitySourceIsAuthoritative(source string) bool {
	return source == CapabilitySourceManual || source == CapabilitySourceVisionBenchmark
}
```

Interface methods on the routing store, doc-commented in that interface's style:

```go
	// MappingCapabilities lists one mapping's capability rows. An absent
	// capability is UNKNOWN and simply has no row.
	MappingCapabilities(ctx context.Context, mappingID string) ([]CapabilityRow, error)
	// MappingCapabilitiesForMappings is the BULK reader: one query for many
	// parents, keyed by mapping id. A mapping with no rows contributes no key.
	//
	// This repo has no other batch child-collection reader — child collections
	// are N+1 loops in Go. This one is not gratuitous: the model-servers
	// listing already costs ~31 queries at the documented scale, and two
	// multipliers would turn a per-row capability read into a real cost — the
	// SSE stream recomputes the entire listing on every loaded-registry
	// change, and the model-group endpoint calls the whole listing once per
	// group member.
	MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]CapabilityRow, error)
	// UpsertMappingCapabilities writes rows, replacing any row for the same
	// (mapping, capability). It does NOT apply the precedence rule — callers
	// do, because only they know whether they are a probe (see
	// CapabilitySourceIsAuthoritative). It carries no metrics_locked guard and
	// never touches metrics_source/metrics_updated_at: a capability is not a
	// number an operator pins against automation.
	UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []CapabilityRow) error
	// DeleteMappingCapability returns one capability to UNKNOWN. Deleting a
	// row is how "not determined" is expressed — the state the pre-78 bool
	// columns could not represent.
	DeleteMappingCapability(ctx context.Context, mappingID, capability string) error
```

- [ ] **Step 4: The SQLite implementation**

New file `gateway/backend/internal/store/sqlite_capabilities.go`. The upsert follows `user_ui_preferences`' cross-dialect statement exactly (`sqlite_user_ui_preferences.go:30-37`) — read it first:

```go
func (s *SQLiteStore) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	for _, r := range rows {
		if _, err := s.exec(ctx, `
			insert into model_mapping_capabilities (mapping_id, capability, verdict, source, checked_at)
			values (?, ?, ?, ?, ?)
			on conflict(mapping_id, capability) do update set
				verdict = excluded.verdict,
				source = excluded.source,
				checked_at = excluded.checked_at`,
			mappingID, r.Capability, r.Verdict, r.Source, r.CheckedAt,
		); err != nil {
			return fmt.Errorf("upsert mapping capability %q: %w", r.Capability, err)
		}
	}
	return nil
}
```

`MappingCapabilities` is a plain `select … where mapping_id = ? order by capability`. `DeleteMappingCapability` is a plain delete (0 rows affected is a benign no-op, matching the file's convention). `MappingCapabilitiesForMappings` builds a `where mapping_id in (…)` with placeholders, **chunked at 1000** so a pathological deployment cannot exceed a driver parameter limit, and returns a map with no key for a parent that has no rows. Chunking gets its own short doc comment saying why.

- [ ] **Step 5: Migration 78 — create, backfill, and NOTHING else**

Registry: `{version: 78, name: "model_mapping_capabilities_table", up: migration78Up},`

The table follows `agent_runtime_spec_gpus`' composite-PK shape (`migrate.go:2884-2897`):

```go
// migration78Up creates model_mapping_capabilities — one row per (mapping,
// capability) carrying the verdict, its SOURCE and when it was established —
// and backfills it from the columns it supersedes.
//
// It deliberately does NOT drop those columns: that happens in a later
// migration once nothing reads or writes them, so this migration is safe to
// apply to a database an older binary still serves.
//
// Absence of a row means UNKNOWN. That is why two backfill cases write NO
// row: a vision_capable of 0 whose metrics_source proves no measurement
// happened (the column conflated "no" with "never probed"), and an is_mtp of
// 0 (the column is seeded from a NAME HEURISTIC, so its false means "the name
// did not match", not "measured no"). Writing those as "no" would enter a
// guess into the record as a measurement.
func migration78Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if err := execTx(ctx, tx, dl, `
		create table if not exists model_mapping_capabilities (
			mapping_id text not null references model_mappings(id) on delete cascade,
			capability text not null,
			verdict text not null,
			source text not null,
			checked_at `+dl.timestampType()+` not null,
			primary key (mapping_id, capability)
		)`); err != nil {
		return err
	}
	// Backfill. One INSERT … SELECT per source column keeps each rule
	// readable and independently reviewable; `on conflict do nothing` makes
	// the whole migration idempotent.
	// …
	return nil
}
```

The backfill statements, one per row of the spec's §4 table. Each is an `insert into model_mapping_capabilities (…) select …, 'vision', case … end, …, coalesce(m.capabilities_checked_at, ?) from model_mappings m where …` with `on conflict(mapping_id, capability) do nothing`. Write them out explicitly rather than looping in Go:

1. `cap_vision`/`cap_video`/`cap_audio`/`cap_tools`: one statement each, `where m.cap_X <> ''`, verdict `m.cap_X`, source `'llama_cpp_props'`, `checked_at` = `coalesce(m.capabilities_checked_at, <migration timestamp>)`.
2. `live_progress_support`: `where m.live_progress_support <> ''`, capability `'live_progress'`, verdict `case m.live_progress_support when 'supported' then 'yes' else 'no' end`, source `'llama_cpp_props'`, `checked_at` = `coalesce(m.live_progress_checked_at, <ts>)`.
3. `vision_capable = 1`: capability `'vision'`, verdict `'yes'`, source `case m.metrics_source when 'vision' then 'vision_benchmark' when 'manual' then 'manual' else 'legacy' end`, `checked_at` = `coalesce(m.metrics_updated_at, <ts>)`. Runs AFTER (1) so a `cap_vision` row already present wins via `do nothing` — the newer, better-provenanced verdict.
4. `vision_capable = 0 and m.metrics_source in ('vision','manual')`: verdict `'no'`, source mapped as in (3).
5. `is_mtp = 1`: capability `'mtp'`, verdict `'yes'`, source `'legacy'`, `checked_at` = `coalesce(m.metrics_updated_at, <ts>)`.
6. `cap_extra`: a JSON array of names. SQL cannot portably split it, so do this one in Go — `select id, cap_extra from model_mappings where cap_extra <> '' and cap_extra <> '[]'`, `json.Unmarshal` each, and insert one `'yes'`/`'llama_cpp_props'` row per name. In practice empty today (llama.cpp never populates it), so a malformed value must be skipped with no error rather than failing the migration.

The migration timestamp is one `time.Now().UTC()` captured at the top and passed as a parameter, so every backfilled row shares it.

- [ ] **Step 6: MemoryStore parity and the cascade mirror**

The four methods with identical semantics (upsert replaces, batch omits empty parents, delete is a no-op when absent). Then extend `deleteMappingLocked` (`memory_store.go:946-975`) to drop the mapping's capability rows, and extend its doc comment's FK-graph enumeration — that comment is the map the cascade test checks against.

- [ ] **Step 7: Write the migration test**

Beside the existing migration tests: seed a pre-78 database (insert `model_mappings` rows directly with the old column values), run the migrations, and assert every row of the spec's §4 table — including the **two cases that must produce no row** (`vision_capable = 0` with `metrics_source = 'probe'`; `is_mtp = 0`) and the provenance mapping for `metrics_source` ∈ {`vision`, `manual`, other}. Assert idempotence by running the migration twice.

- [ ] **Step 8: Run everything, verify green and unchanged**

```bash
cd gateway/backend && go test ./internal/store/ ./internal/routing/ -count=1 && go test ./... -count=1
```
Expected: PASS, and **every pre-existing test unchanged** — this task adds a table and writes nothing else, so a failure elsewhere means the backfill or the cascade mirror broke something.

- [ ] **Step 9: Revert-verify + mutations**

Revert the production files (tests stay) → the new tests fail to compile. Restore. Then: (a) make the backfill write `'no'` for `is_mtp = 0` → the migration test's no-row assertion must fail; (b) make `UpsertMappingCapabilities` insert instead of upsert → the no-duplicate assertion must fail; (c) drop the capability rows from `deleteMappingLocked` → the cascade test must fail. Record each.

- [ ] **Step 10: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./... -count=1
```

```bash
git add gateway/backend/internal/store gateway/backend/internal/routing
git commit -m "feat(gateway): a capability table, its store API, and the backfill

One row per (mapping, capability) with the verdict, its source and when it
was established -- absence of a row IS unknown, so the
never-overwrite-with-unknown discipline stops being a convention every
writer has to remember. Migration 78 creates and backfills it; the columns
it supersedes stay authoritative until nothing reads them. Two backfill
cases deliberately write no row: a vision_capable of 0 without a measuring
provenance, and an is_mtp of 0, whose false only means a name heuristic
did not match."
```

---

### Task 2: The wire carries an open vocabulary

**Files:**
- Modify: `server-agent/internal/sample/sample.go` (`Capabilities`/`CapabilityVerdict` ~:164-177, `RuntimeSample.Capabilities` ~:159, `Normalize` ~:296-312)
- Modify: `server-agent/internal/agent/agent.go` (`capabilitiesSample` ~:1288-1303)
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (the mirror `agentRuntimeCapabilitiesSample` ~:150-167 and the field ~:214) — **decode only**; the write-back still consumes the old fixed fields via a shim until Task 3
- Test: `server-agent/internal/sample/sample_test.go` (or wherever `Normalize` is tested — grep it), `server-agent/internal/agent/agent_test.go`, `gateway/backend/internal/gateway/agent_runtime_status_ingest_test.go`

**Interfaces:**
- Consumes: `collector.Capabilities` (unchanged — the detector is not touched).
- Produces: the wire shape Task 3's write-back reads.

- [ ] **Step 1: Write the failing tests**

1. `TestCapabilitiesSampleCarriesEveryVerdictAsARow` — `capabilitiesSample(collector.Capabilities{Vision: "yes", Tools: "no"})` yields a non-nil pointer whose `Verdicts` holds exactly those two, by name, and **no** entry for the undetermined ones.
2. `TestCapabilitiesSampleAllEmptyIsNonNilAndEmpty` — an all-empty `collector.Capabilities` yields a **non-nil** pointer with an empty (non-nil) `Verdicts`. This is the 401/403 case and the distinction the whole pointer exists for.
3. `TestNormalizeForcesVerdictsNonNil` — a `RuntimeSample` with a non-nil `Capabilities` whose `Verdicts` is nil marshals as `[]`, never `null` (the rule `Normalize` already applies to `Net`/`GPUs`/`ProxyRoutes`).
4. Gateway: a sample carrying `{"capabilities":{"verdicts":[{"name":"vision","verdict":"yes"}]}}` decodes into the mirror; a sample with **no** `capabilities` key decodes to a nil pointer; one with `{"capabilities":{"verdicts":[]}}` decodes to non-nil-empty. Also: an **unknown name** decodes and is carried, not dropped (forward compatibility, the `parseAgentCapabilities` precedent).

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement**

`sample.go` — read the doc comment on today's `Capabilities` (:164-170) and carry its nil-vs-empty reasoning across verbatim, extending it with why the shape is a keyed list:

```go
// Capabilities is one managed child's auto-detected capability set (#49-2).
//
// A POINTER on RuntimeSample, and that is load-bearing: nil means "this agent
// predates capability detection", while non-nil with an empty Verdicts means
// "detection ran and determined nothing" — which is exactly what an
// api-key-protected child reports, since a 401/403 is a CONCLUSIVE refusal.
// A bare map with omitempty could not carry that distinction: an empty map
// marshals away and the two cases collapse.
//
// Verdicts is a keyed LIST rather than a map because that is this file's own
// idiom — Net, GPUs and ProxyRoutes are all keyed lists, there is no map on
// this wire — and because Normalize can force a slice non-nil so it never
// marshals as null.
//
// The vocabulary is OPEN: a name this binary has never heard of is carried
// through unchanged rather than dropped, the same forward-compatibility rule
// the declared-feature list already follows.
type Capabilities struct {
	Verdicts []CapabilityVerdict `json:"verdicts"`
}

// CapabilityVerdict is one capability's answer: Verdict is "yes" or "no" and
// nothing else — an undetermined capability has NO entry, mirroring the
// gateway's row-absence-means-unknown model.
type CapabilityVerdict struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
}
```

`capabilitiesSample` maps the four `collector.Capabilities` fields plus `Extra` onto entries, skipping every empty verdict, and **always returns a non-nil pointer** (keep that doc comment). `Extra` names become `yes` entries. `Normalize` gains the slice-non-nil line beside its siblings.

Gateway mirror: the same two structs with identical JSON tags and the "separate Go modules, mirrored field for field" doc note the existing pair already carries. To keep this task's diff honest and the tree green, add a small shim the old write-back keeps using — e.g. a method that projects `Verdicts` back onto the four names — and mark it with a comment saying Task 3 deletes it. **Do not change the write-back's behaviour in this task.**

- [ ] **Step 4: Run the tests; then both modules' suites.**

- [ ] **Step 5: Revert-verify + the load-bearing mutation** — revert production files (tests stay) → new tests fail. Restore. Then make `capabilitiesSample` return `nil` for an all-empty set → test 2 must fail. That mutation is the one that matters: it is the distinction three doc comments depend on.

- [ ] **Step 6: Lint both modules + commit**

```bash
git add server-agent/internal/sample server-agent/internal/agent gateway/backend/internal/gateway
git commit -m "feat: the capability wire carries an open vocabulary of named verdicts

Four fixed fields plus an unused Extra become a keyed list, so a
capability this binary has never heard of survives the trip instead of
being dropped. The pointer wrapper stays: nil still means an agent that
predates detection, non-nil-but-empty still means detection ran and
determined nothing -- what an api-key-protected child reports."
```

---

### Task 3: The write paths become row writers, and honour precedence

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (the whole write-back family ~:845-1157: `resolveRuntimeSpecCapabilities`, `writeBackRuntimeCapabilities`, `writeBackOneRuntimeCapabilities`, `resolvedCapabilities`, `capabilityResolution`, `stored`, `applyWritten`, `storedCapabilityVerdicts`, `changedCapabilityVerdicts`, `noCapabilityEvidence`, `syncVisionCapable`; plus `writeBackRuntimeLiveProgress` ~:800 and its call site)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`healthStore` ~:53-86; `applyCapabilityWrite` ~:492-508; `diffCapabilityVerdicts` ~:522-543; the two capability call sites ~:924 and ~:982; the two live-progress write sites ~:910-911 and ~:1008-1017; `syncVisionCapable`'s app-health twin ~:569-578)
- Modify: `gateway/backend/internal/gateway/benchmark_runner.go` (~:521-537 — the vision benchmark writes a `vision` row with source `vision_benchmark` instead of the bool)
- Modify: the eight files whose doc comments cite `UpdateMappingLiveProgressSupport` by name (see Step 5)
- Test: `gateway/backend/internal/gateway/agent_runtime_status_ingest_test.go`, `gateway/backend/cmd/gateway/app_health_test.go`, `gateway/backend/internal/gateway/benchmark_*_test.go`

**Interfaces:**
- Consumes: Task 1's store API and `CapabilitySourceIsAuthoritative`; Task 2's wire shape.
- Produces: rows. Old columns stop being written; Task 4/5 move the readers, Task 6 drops the columns.

- [ ] **Step 1: Write the failing tests**

Read the existing capability write-back tests in `agent_runtime_status_ingest_test.go` and `app_health_test.go` and reuse their harness. The behaviour tests carry over (a probe writes what it determined; unchanged verdicts issue no write; nil vs all-empty; the feature gate; cross-server rejection) but **now assert rows** — those assertion changes are expected and legitimate in this task, unlike everywhere else in this plan.

New tests, the ones this task exists for:

1. `TestIngestProbeDoesNotOverwriteAManualVerdict` — a `vision` row with source `manual` survives a telemetry sample reporting the opposite verdict; the writer is called zero times for that capability.
2. `TestIngestProbeDoesNotOverwriteABenchmarkVerdict` — same for `vision_benchmark`.
3. `TestIngestProbeOverwritesItsOwnAndLegacyVerdicts` — a row with source `llama_cpp_props` or `legacy` **is** replaced when the verdict differs, and is **not** rewritten when it agrees.
4. The same three for the app-health path.
5. `TestVisionBenchmarkWritesAnAuthoritativeRow` — a definitive benchmark verdict lands as a `vision` row with source `vision_benchmark`, and a subsequent probe leaves it alone.
6. `TestIngestLiveProgressLandsAsARow` — the `live_progress` verdict from the same document becomes a row (`supported` → `yes`), sharing the capability path rather than its own writer.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Rewrite the ingest write-back**

Shape, keeping every guard the current family has:

- `resolveRuntimeSpecCapabilities` loses its eight-value tuple in favour of `(mappingID string, stored map[string]routing.CapabilityRow, ok bool)` — the ownership resolution (`RuntimeSpecByID` → `MappingByID` → `ApplicationByID`, cross-server `slog.Warn` rejection) is unchanged; only what it loads alongside changes, now via `MappingCapabilities`.
- `writeBackOneRuntimeCapabilities` keeps its triage (nil → return, no verdicts → return), the memoized resolution, best-effort error handling, and the runtimes cap. Its middle becomes: for each reported verdict, skip when `CapabilitySourceIsAuthoritative(stored[name].Source)`; skip when the stored verdict already agrees; otherwise collect a `CapabilityRow` with source `llama_cpp_props`. One `UpsertMappingCapabilities` for whatever remains; nothing collected means no call.
- `syncVisionCapable` is **deleted** — with one source of truth there is nothing to sync. So are `changedCapabilityVerdicts`, `storedCapabilityVerdicts`, `applyWritten`, `noCapabilityEvidence` and `stored()` in their current forms; replace them with whatever the row shape actually needs (a single "which rows changed and may I write them" helper, pure and table-testable, is the natural landing place for the precedence rule).
- `writeBackRuntimeLiveProgress` is deleted; its verdict rides the capability path.
- Task 2's shim is deleted here.

The per-sample `runtime_model_probe` feature gate stays exactly where it is, with its comment.

- [ ] **Step 4: Rewrite the app-health write path**

`applyCapabilityWrite` takes the same shape: load the mapping's rows once (`MappingCapabilities`), apply precedence, upsert what changed. `diffCapabilityVerdicts` becomes the row-shaped equivalent. The two live-progress write sites fold into it — same document, same pass, one writer. `healthStore` loses `UpdateMappingLiveProgressSupport`, `UpdateMappingCapabilities` and `UpdateMappingVisionCapable` and gains `MappingCapabilities` + `UpsertMappingCapabilities`; every fake in `app_health_test.go` follows.

- [ ] **Step 5: The citation sweep**

`UpdateMappingLiveProgressSupport` is cited **by name** as the canonical rationale for "a capability is not a metric an operator pins numbers against" in doc comments across eight files: `routing/store.go`, `store/sqlite_applications.go`, `routing/memory_store.go`, `cmd/gateway/app_health.go`, `internal/gateway/agent_ingest.go`, `store/migrate.go`, `provider/model_info.go`, `portal/service_model_servers.go`. Find them with `grep -rn UpdateMappingLiveProgressSupport gateway/backend --include=*.go`. **Repoint each at the table's own rationale — do not just delete the sentence.** The argument is still load-bearing: it is now why `model_mapping_capabilities` carries no `metrics_locked` guard. A comment pointing at a removed method is drift this repo treats as a defect.

Delete `UpdateMappingLiveProgressSupport`, `UpdateMappingCapabilities` and `UpdateMappingVisionCapable` from the interface and both drivers. `internal/tracing/routingstore_gen.go` is **gowrap-generated and marked DO NOT EDIT** — regenerate it instead of hand-editing, with the documented directive in `internal/tracing/generate.go`:

```
go generate ./internal/tracing/
```

(equivalently `gowrap gen -p op-ai-gateway/internal/routing -i Store -t ./templates/tracing.gowrap.tmpl -o routingstore_gen.go -v Prefix=routing.Store -v DecoratorName=RoutingStoreWithTracing`). `generate.go` notes that gowrap does not preserve the per-file licence header — read it and follow whatever it says to run afterwards.

- [ ] **Step 6: Run the tests; then the module.**

Existing tests for `wantsLiveProgress` and the scorer must still pass **unchanged** — they read `Target`/`Route`, which this task does not touch.

- [ ] **Step 7: Revert-verify + mutations** — revert production files (tests stay). Then: (a) delete the `CapabilitySourceIsAuthoritative` check → tests 1, 2 and 4 must fail; (b) drop the agrees-already check → test 3's no-rewrite half must fail. Record both.

- [ ] **Step 8: Lint + commit**

```bash
git add gateway/backend/internal/gateway gateway/backend/cmd/gateway gateway/backend/internal/routing gateway/backend/internal/store gateway/backend/internal/provider gateway/backend/internal/portal
git commit -m "feat(gateway): probes write capability rows, and never outrank a human

The write-backs become row writers with one new guard: a verdict whose
source is manual or vision_benchmark is never overwritten by a probe --
the operator's rule, now expressible because provenance is per capability.
syncVisionCapable is deleted rather than fixed: with one source of truth
there is nothing to sync, which is what made its convergence subtle in the
first place. The live-progress verdict rides the same path instead of
keeping a writer, and the rationale UpdateMappingLiveProgressSupport was
cited for across eight files is repointed at the table."
```

---

### Task 4: The decision path reads the table

**Files:**
- Modify: `gateway/backend/internal/store/sqlite_applications.go` (`ActiveMappingsForModel` ~:584-630, `scanMappingCandidate` ~:670-691)
- Modify: `gateway/backend/internal/routing/store.go` (`MappingCandidate` — add the two fields)
- Modify: `gateway/backend/internal/routing/memory_store.go` (`ActiveMappingsForModel` at ~:1254 must fill the same two fields from its capability map)
- Modify: `gateway/backend/internal/routing/resolver.go` (`scoringRoute` ~:69-97 and `targetFrom` ~:1088)
- Test: `gateway/backend/internal/routing/resolver_*_test.go`, `scorer_test.go`, the store conformance suite

**Interfaces:**
- Produces: `MappingCandidate.IsMTP bool` and `MappingCandidate.LiveProgressSupport string`, filled from the joins. **`Route`, `Target`, the scorer and `wantsLiveProgress` are untouched.**

- [ ] **Step 1: Write the failing tests**

1. Conformance: `ActiveMappingsForModel` returns a candidate whose `IsMTP` is true when an `mtp`/`yes` row exists, false with no row, **and false with an `mtp`/`no` row**; likewise `LiveProgressSupport` is `supported`/`unsupported`/`""` for a `yes`/`no`/absent `live_progress` row. Both drivers.
2. `TestScorerStillAwardsTheMTPBonusFromTheJoinedRow` — end to end through the scorer: a mapping with an `mtp` `yes` row scores 30 higher than one without. Asserted **through the scorer**, so it pins the behaviour rather than the plumbing.
3. `TestWantsLiveProgressReadsTheJoinedVerdict` — the three-layer rule behaves identically for a `yes` row / `no` row / no row as it did for `supported`/`unsupported`/`''`.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement**

`ActiveMappingsForModel` gains two filtered joins and drops the eleven columns from its select list:

```sql
left join model_mapping_capabilities mtp
       on mtp.mapping_id = m.id and mtp.capability = 'mtp'
left join model_mapping_capabilities lp
       on lp.mapping_id = m.id and lp.capability = 'live_progress'
```

selecting `mtp.verdict` and `lp.verdict` (nullable — scan into `sql.NullString`). Two filtered joins rather than one unfiltered one: measured at ≈ +6 µs each against a per-request query that already costs ~17 µs, and it keeps **one row per mapping** — an unfiltered join multiplies rows and costs +79 µs. Put that reasoning in a comment at the joins; it is the question a reader will have.

`MappingCandidate` gains the two fields with doc comments saying where they come from and why they are not on `ModelMapping` (a struct field populated only on this one path would read as populated everywhere — a footgun the moment a mapping loaded via `MappingByID` reported `false` while the resolver's reported the truth). `scoringRoute` reads `c.IsMTP`; `targetFrom` reads `c.LiveProgressSupport`. Map the verdict at the boundary: `yes` → `true`/`supported`, `no` → `false`/`unsupported`, absent → `false`/`""`.

`MemoryStore.ActiveMappingsForModel` (~:1254) mirrors it from its capability map, with the identical boundary conversion — the conformance test runs against both drivers, so a divergence surfaces there.

- [ ] **Step 4: Run the tests; then the module.** The scorer's and `wantsLiveProgress`' own tests must pass **with no assertion changes**.

- [ ] **Step 5: Revert-verify + mutation** — revert production files. Then map `no` to `true` in the MTP boundary conversion → test 1's `no`-row case must fail (the case a three-state-to-bool conversion gets wrong most easily).

- [ ] **Step 6: Lint + commit**

```bash
git add gateway/backend/internal/store gateway/backend/internal/routing
git commit -m "feat(gateway): the request path reads MTP and live-progress from the table

Two filtered joins on the candidate query -- measured at about 6 microseconds
each against a query that already costs seventeen, and they keep one row per
mapping where an unfiltered join would multiply rows for 79. The verdicts land
on MappingCandidate rather than ModelMapping on purpose: a struct field filled
only on this path would read as filled everywhere. The scorer and the
three-layer live-progress rule are untouched."
```

---

### Task 5: The portal reads the table

**Files:**
- Modify: `gateway/backend/internal/portal/service_model_servers.go` (`ModelServerDTO` ~:60-110, the fill ~:210-216, `ModelServers` ~:134-151)
- Modify: `gateway/backend/internal/portal/service.go` (`modelsResponse`'s vision fold ~:2033-2065)
- Modify: `gateway/backend/internal/portal/service_applications.go` (the manual vision path: `CreateMapping` ~:1459-1493, `UpdateMapping` ~:1607-1622, and `metricValuesPresent`/`metricValueChanged` lose their `IsMTP`/`VisionCapable` participation)
- Modify: `gateway/frontend/src/api/models.ts` (`ModelServerRow` ~:389-407), `gateway/frontend/src/components/ModelServersSection.tsx` (`capabilityChips`/`capabilitiesTooltip` ~:161-220), `gateway/frontend/src/i18n.ts`
- Test: `gateway/backend/internal/portal/service_model_servers_test.go`, `service_test.go` (the vision fold), `service_applications_test.go`, `gateway/frontend/src/components/ModelServersSection.test.tsx`

- [ ] **Step 1: Write the failing tests**

1. `ModelServerDTO` carries `capabilities` as an array of `{capability, verdict, source, checked_at}`; a mapping with no rows carries an empty array (not null on the wire).
2. **The N+1 guard:** `ModelServers` issues **one** capability query regardless of row count. Use a counting store fake; this test is the reason the batch reader exists.
3. The models-list vision fold: a `vision`/`yes` row on every offering mapping advertises vision; a `no` row anywhere does not; **a missing row anywhere does not** (fail-closed, unchanged from the bool's behaviour).
4. Manual path: setting `vision_capable` true through the mapping update writes a `vision`/`yes` row with source `manual`; setting it false writes `no`/`manual`; and the operator's value is not overwritten by a subsequent probe (the end-to-end proof of the operator's rule).
5. Frontend, column-scoped via `cellForColumn`: chip order (known capabilities in fixed order, then unknown names verbatim with the neutral status), the em-dash when there are none, and the tooltip naming **source and date per capability** — the gain the shared-provenance tooltip could not give.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement**

DTO + fill via `MappingCapabilitiesForMappings` over the mapping ids `ModelServers` already has. The vision fold reads the `vision` row from the same batch. The mapping-update path writes/deletes the `vision` row with source `manual` (clearing it — `DeleteMappingCapability` — when the operator makes it indeterminate, if the DTO can express that; if the checkbox is strictly boolean, write `yes`/`no` and note that "unknown" has no UI yet).

Frontend: `capabilityChips` reads the array; the per-capability tooltip replaces the shared one. `ModelServerRow` follows. i18n keys in **both** locales.

- [ ] **Step 4: Run backend + frontend gates** (including `npm run format:check`, which CI enforces and `test`+`build` do not cover).

- [ ] **Step 5: Revert-verify + mutation** — revert production files. Then make the vision fold treat a missing row as capable → test 3 must fail (the fail-closed regression that would silently enable image attach on unprobed models).

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/portal gateway/frontend/src
git commit -m "feat(portal): capabilities come from the table, with per-capability provenance

The model-servers row carries one entry per determined capability, loaded
for every mapping in a single query -- the listing is recomputed on every
loaded-model change and once per group member, so a per-row read would
have multiplied. The tooltip can finally say who established each verdict
and when. The vision fold stays fail-closed: a missing row is not a yes.
The operator's checkbox now writes an authoritative manual row."
```

---

### Task 6: Drop the eleven columns

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (registry; new `migration79Up`; `dropColumnIfPresent` if Task 1 did not already add it)
- Modify: `gateway/backend/internal/routing/store.go` (`ModelMapping` loses `IsMTP`, `VisionCapable`, `LiveProgressSupport`, `LiveProgressCheckedAt`, `CapVision`, `CapVideo`, `CapAudio`, `CapTools`, `CapExtra`, `CapabilitiesSource`, `CapabilitiesCheckedAt`)
- Modify: `gateway/backend/internal/store/sqlite_applications.go` (`CreateMapping` insert, `UpdateMapping` set, and the four select/scan sites), `gateway/backend/internal/routing/memory_store.go`
- Test: the migration test from Task 1's file; the conformance suite

- [ ] **Step 1: Write the failing test** — after migration 79 the eleven columns are gone (query the schema on both dialects: SQLite `pragma table_info(model_mappings)`, Postgres `information_schema.columns`), and every capability still reads correctly from the table.

- [ ] **Step 2: Run it, verify it fails.**

- [ ] **Step 3: Implement**

`dropColumnIfPresent(ctx, tx, dl, table, column)`: Postgres takes `alter table … drop column if exists …`; SQLite takes `alter table … drop column …` and swallows the benign "no such column" error, mirroring `addColumnIfMissing`'s shape (`migrate.go:203`). Verified safe here: SQLite refuses to drop an indexed column, and the only index on `model_mappings` is `idx_model_mappings_application on (application_id)` — none of the eleven appears in it.

Then remove the struct fields and every insert/update/select/scan reference. The compiler is the checklist; work until `go build ./...` is clean.

- [ ] **Step 4: Run everything** — both modules, full suites. This is the task where a forgotten reader surfaces.

- [ ] **Step 5: Revert-verify** — revert the migration only: the schema test fails while everything else still passes (proof that nothing depends on the columns any more, which is the real assertion of this task).

- [ ] **Step 6: Lint + commit**

```bash
git add gateway/backend
git commit -m "refactor(gateway): drop the eleven capability columns

is_mtp, vision_capable, live_progress_support/_checked_at and migration
77's seven cap_* columns are gone; the table is the only place a
capability lives. This departs from the repo's practice of leaving
superseded columns inert -- ADR-039 records why the case differs: these
shipped days ago with no reader outside the feature being rewritten,
unlike the long-established native_* booleans that practice was written
for."
```

---

### Task 7: Documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (rewrite ADR-038; append ADR-039)
- Modify: `docs/architecture/reference/data-model.md` (migrations 78 + 79; rewrite the capability field semantics; the `model_mappings` summary row; the migration-count heading and its inbound anchors)
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (§8.4.3's capability paragraphs, including PR #66's vision-override paragraph)
- Modify: `docs/architecture/reference/api-surface.md` (the DTO fields)
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (the wire-shape and nil-vs-empty paragraphs)
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md` (the MTP bonus's source of truth)

- [ ] **Step 1: ADR-038 is rewritten, not amended.** It currently records the wide-column design *and* enshrines the probe-overrides-everything sync with `metrics_locked` as the escape hatch. Both are gone. It should now record what actually shipped and why the first shape did not survive contact with the operator's requirement.

- [ ] **Step 2: ADR-039** — the table, the precedence rule, and the column drop, in context→decision→consequence shape, status Accepted. It must draw the line the spec draws: a column superseded within days with no reader outside its own feature is dropped; a long-established column stays inert. Otherwise the next reader concludes drops are routine.

- [ ] **Step 3: The remaining documents**, each fact verified against the code before writing it. Preserve every heading and anchor; the migration-count heading in `data-model.md` changes and its inbound links must be updated consistently (four such links existed last time this happened).

- [ ] **Step 4: Gates** — `./scripts/check-docs.sh` and `bash ./scripts/check-docs.test.sh`.

- [ ] **Step 5: Commit**

```bash
git add docs/architecture
git commit -m "docs: the capability table, the precedence rule, and ADR-039

ADR-038 is rewritten: it recorded a wide-column design and enshrined a
probe override that no longer exists. ADR-039 records the table, the rule
that a manual or benchmark verdict outranks a probe, and the column drop
-- with the line drawn so the drop does not read as a new norm."
```

---

## Final verification (after all tasks, before the PR)

- Both Go modules: `golangci-lint fmt --diff` + `run` + `go test ./... -count=1`.
- Frontend: `format:check` + `lint` + `build` + `test`.
- Docs: `check-docs.sh` + `check-docs.test.sh`.
- Detector drift: `diff <(sed -n '/^func detectCapabilities/,/^}/p' gateway/backend/internal/provider/model_info.go) <(sed -n '/^func detectCapabilities/,/^}/p' server-agent/internal/collector/probe.go)` must print nothing.
- **A migration replay from empty**, both dialects: a fresh database reaches the same schema as an upgraded one. Migration 78's backfill reads columns that migration 79 then drops, so the ORDER is load-bearing — a fresh install replays both and must end in the same place.
- Sonar from the repo root: `npm ci` in `gateway/frontend` first, then `make sonar-up`, **poll `curl -s http://localhost:9000/api/system/status` until it reports `"status":"UP"`**, then `sonar-gate` → `sonar-findings` → `sonar-branch-findings` → `sonar-down`. Judge by `Attributed: N on lines this branch changed` (want 0), and **check `sonar-gate`'s own exit code first** — a failed gate makes `branch-findings` judge against stale findings, and the tell is a finding that comes back byte-identical after a real fix.
- `docs/superpowers/` removed in the PR-preparation step, never in these tasks.
