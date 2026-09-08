# Capability auto-detect, PR A: llama.cpp modalities and tools — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a llama.cpp child's `/props` document yields its vision/video/audio/tools verdicts, they persist per mapping as three-state values, they show in the portal, and a definitive vision verdict additionally drives the existing `vision_capable` bool — so the chat image gate starts working from probe evidence with no change to its consumers.

**Architecture:** one new detector, duplicated byte-identically across the two Go modules (the `detectLiveProgressSupport` precedent). The agent's two existing `/props` GETs per cycle collapse into one multi-verdict probe, so adding capabilities costs zero extra requests. Six new tri-state columns (migration 77) outside the `metrics_locked` group, written by a new lock-free store writer, plus a lock-guarded sync onto `vision_capable`. Both existing probe paths — the agent's telemetry write-back and the gateway's `{model}` app-health pass through the #58 `/upstream/{model}/props` route — grow one capabilities write beside their live-progress write.

**Tech Stack:** Go 1.26 (two modules: `op-ai-server-agent`, `op-ai-gateway`), SQLite/Postgres via the store's dialect layer, React/TS portal (vitest + Testing Library).

**Spec:** `docs/superpowers/specs/2026-09-08-capability-autodetect-design.md` (approved). Issue: #49 sub-project 2. PRs B (Ollama + #54) and C (MTP via `/slots`) are separate plans.

## Global Constraints

- **Three-state honesty, everywhere:** every verdict is `""` (unknown) | `"yes"` | `"no"`. `""` must **never** overwrite a stored verdict, and must never be written as a value. This includes the partial case: a `/props` with `modalities` but no `chat_template_caps` must leave `cap_tools` untouched.
- **Detector duplication:** `detectCapabilities` exists twice, byte-identically (`server-agent/internal/collector/probe.go` and `gateway/backend/internal/provider/model_info.go`), each carrying the "DUPLICATED on purpose / must never drift" doc comment in the style of the existing `detectLiveProgressSupport` twins. Changing one without the other is a defect.
- **The `"role":"router"` gate is mandatory** in the detector: a router-mode `/props` describes the router's own build; under the never-rewrite discipline, reading it as the child's evidence is permanent and self-reinforcing (#55).
- **Upstream truths that must appear in code comments, verbatim in meaning:** (a) `modalities.video: true` means "this binary was built with video support **and** the model has a vision encoder" (`mtmd_helper_support_video` returns `mtmd_support_vision` under `#ifdef MTMD_VIDEO`) — not "the model understands video"; (b) `chat_template_caps.supports_tools: false` means "no native tool template", **not** "tool calls fail" (with `--jinja`, default-on since 2025-11-27, llama.cpp accepts tools for every model via a generic handler).
- **A key absent from a present object is `""`, never `"no"`** — an older server that predates a key has not answered the question.
- **Regression anchors (unchanged in substance):** `ProbeLiveProgressSupport`'s conclusive set `{404, 401, 403, 405}`, its transient/retry rule and its stable-`""` caching (all carried into the widened probe); `runtimeCapabilityCache` keying by `SpecID` with the PID checked on lookup, and PID-change re-arm; `writeBackRuntimeLiveProgress`/`writeBackRuntimeContext` disciplines (`""` skip, compare-to-stored, per-sample `runtime_model_probe` gate, cross-server `slog.Warn`, no `metrics_locked` on the live-progress column); `UpdateMappingLiveProgressSupport`'s SQL; `UpdateMappingVisionCapable`'s `and metrics_locked = 0` guard; the `/upstream/{model}/props` route (this PR does **not** touch the router). Existing tests for all of these must keep passing with **no assertion changes**.
- **Every new/changed test must FAIL with its production change reverted.** Revert only production files, never the test file (reverting the test makes `go test -run X` report "ok" with zero tests — a fake pass). Where a revert cannot discriminate, mutate the specific guard instead and record which mutation broke which test.
- **Per-module gates before every commit:** touched Go module → `~/go/bin/golangci-lint fmt --diff` (must print nothing) + `~/go/bin/golangci-lint run` + `go test ./...`. Frontend → `npm run format:check` + `lint` + `build` + `test` in `gateway/frontend`. Docs → `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- Known pre-existing failure to ignore: `go test ./internal/gateway/ -race` fails on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (issue #53). Plain `go test` passes everywhere.
- Branch `capability-autodetect`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/capability-autodetect`. Never commit to `main`. Never use bare `git stash` (shared stack across worktrees).
- `docs/superpowers/` is branch-local and removed before the PR — never referenced from `docs/architecture/`.

---

### Task 1: Schema and store writers

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (registry ~:109, new `migration77Up` after `migration76Up` ~:3308)
- Modify: `gateway/backend/internal/routing/store.go` (`ModelMapping` struct ~:598-635; store interface ~:1023-1029 area)
- Modify: `gateway/backend/internal/store/sqlite_applications.go` (new writer after `UpdateMappingLiveProgressSupport` ~:318)
- Modify: `gateway/backend/internal/routing/memory_store.go` (parity, after its `UpdateMappingLiveProgressSupport` ~:1042)
- Modify: `gateway/backend/internal/store/sqlite_applications.go` mapping scan/select (wherever `live_progress_support` is selected and scanned — grep it and add the six columns in the same places)
- Test: `gateway/backend/internal/store/routing_store_conformance_test.go` — the both-stores suite that already covers `live_progress_support`. Read `TestUpdateMappingLiveProgressSupportIgnoresMetricsLock` (:385) and `TestUpdateMappingLiveProgressSupportRoundTrip` (:463) FIRST and copy their shape: `forEachRoutingStore(t, func(t *testing.T, s routing.Store) { … })`, fixtures built with `CreateAIServer`/`CreateApplication`/`CreateMapping`, locking done by mutating the mapping and calling `UpdateMapping` (there is no lock-setting helper), reads via `MappingByID`.

**Interfaces:**
- Produces: `routing.CapabilityVerdicts` (consumed by Tasks 4 and 5), `ModelMapping.CapVision/CapVideo/CapAudio/CapTools/CapExtra/CapabilitiesSource/CapabilitiesCheckedAt`, and `routing.Store.UpdateMappingCapabilities`.

- [ ] **Step 1: Write the failing store-parity test**

Add to `routing_store_conformance_test.go`, beside the two live-progress cases named above and in their exact shape:

```go
// Capability verdicts round-trip, are written only for non-empty fields, and
// are NOT guarded by metrics_locked (a capability is not a pinned metric --
// the live_progress_support precedent this mirrors).
func TestUpdateMappingCapabilities(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// server + app + mapping "m1" exactly as
	// TestUpdateMappingLiveProgressSupportIgnoresMetricsLock builds them
	caps := routing.CapabilityVerdicts{
		Vision: "yes", Video: "no", Audio: "", Tools: "yes",
		Extra: []string{"thinking"}, Source: "llama_cpp_props",
	}
	at := now.Add(time.Hour)
	if err := s.UpdateMappingCapabilities(ctx, "m1", caps, at); err != nil {
		t.Fatalf("UpdateMappingCapabilities: %v", err)
	}
	got, err := s.MappingByID(ctx, "m1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.CapVision != "yes" || got.CapVideo != "no" || got.CapTools != "yes" {
		t.Fatalf("verdicts not stored: %+v", got)
	}
	if got.CapAudio != "" {
		t.Fatalf("an empty verdict must not be written: cap_audio = %q", got.CapAudio)
	}
	if got.CapExtra != `["thinking"]` {
		t.Fatalf("cap_extra = %q, want a JSON array", got.CapExtra)
	}
	if got.CapabilitiesSource != "llama_cpp_props" {
		t.Fatalf("capabilities_source = %q", got.CapabilitiesSource)
	}
	if got.CapabilitiesCheckedAt == nil {
		t.Fatal("capabilities_checked_at not stamped")
	}

	// An empty verdict must not CLEAR a stored one (the central discipline).
	if err := s.UpdateMappingCapabilities(ctx, "m1",
		routing.CapabilityVerdicts{Audio: "yes", Source: "llama_cpp_props"}, at); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, _ = s.MappingByID(ctx, "m1")
	if got.CapVision != "yes" || got.CapTools != "yes" {
		t.Fatalf("an empty verdict cleared a stored one: %+v", got)
	}
	if got.CapAudio != "yes" {
		t.Fatalf("cap_audio = %q, want the newly determined yes", got.CapAudio)
	}

	// metrics_locked does NOT block a capability write. There is no
	// lock-setting helper: mutate the mapping and call UpdateMapping, exactly
	// as TestUpdateMappingLiveProgressSupportIgnoresMetricsLock does.
	locked := routing.ModelMapping{
		ID: "m2", ApplicationID: "app1", GatewayModelName: "gpt-4o", AppModelName: "up2",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateMapping(ctx, locked); err != nil {
		t.Fatalf("create locked mapping: %v", err)
	}
	locked.MetricsLocked = true
	locked.MetricsSource = "benchmark"
	locked.UpdatedAt = now.Add(2 * time.Hour)
	if err := s.UpdateMapping(ctx, locked); err != nil {
		t.Fatalf("lock mapping: %v", err)
	}
	if err := s.UpdateMappingCapabilities(ctx, "m2",
		routing.CapabilityVerdicts{Video: "yes", Source: "llama_cpp_props"}, at); err != nil {
		t.Fatalf("locked write: %v", err)
	}
	gotLocked, _ := s.MappingByID(ctx, "m2")
	if gotLocked.CapVideo != "yes" {
		t.Fatal("metrics_locked blocked a capability write -- it must not (a capability is not a pinned metric)")
	}
	})
}
```

`forEachRoutingStore` runs the body against BOTH stores, so the SQLite writer and the MemoryStore mirror are covered by this one test — that is why parity cannot silently drift.

- [ ] **Step 2: Run it, verify it fails**

Run: `cd gateway/backend && go test ./internal/store/ -run TestUpdateMappingCapabilities -v 2>&1 | head -30`
Expected: compile failure (`UpdateMappingCapabilities` and the new fields undefined) — the valid failing state.

- [ ] **Step 3: Add the type, struct fields, interface method**

In `gateway/backend/internal/routing/store.go`, beside `ModelMapping`'s live-progress fields:

```go
	// CapVision/CapVideo/CapAudio/CapTools are the auto-detected capability
	// verdicts (#49 sub-project 2), each "" (never determined) | "yes" | "no".
	// Three states, not a bool, for the reason vision_capable's own history
	// shows: a bool conflates "not probed" with "no", and the models list
	// AND-aggregates it fail-closed, so one unprobed mapping silently disables
	// a whole model's image attachment. "" must never overwrite a stored
	// verdict -- see UpdateMappingCapabilities.
	//
	// Deliberately OUTSIDE the metrics_locked group, like
	// LiveProgressSupport above and for the same reason: a capability is not
	// a number an operator answers for. The one exception is the vision SYNC
	// onto VisionCapable, which goes through the lock-guarded
	// UpdateMappingVisionCapable on purpose (see the write-back callers).
	//
	// CapVideo carries an upstream subtlety worth knowing before acting on
	// it: llama.cpp's modalities.video is true when the BINARY was built with
	// video support AND the model has a vision encoder -- it is not a claim
	// that the model understands video.
	//
	// CapTools is "the chat template natively supports tool calls", NOT "tool
	// calls work": llama.cpp with --jinja (its default) accepts tools for
	// every model through a generic handler.
	CapVision string
	CapVideo  string
	CapAudio  string
	CapTools  string
	// CapExtra is a JSON array of capability names the upstream reported that
	// have no column here ("thinking", "insert", "embedding", ...). Stored
	// verbatim rather than mapped onto an enum because the vocabulary is
	// open-ended upstream (Ollama passes manifest-declared capabilities
	// through unchanged), so an enum would silently drop future values. ""
	// when nothing extra was reported.
	CapExtra string
	// CapabilitiesSource is which probe produced the current verdicts:
	// "llama_cpp_props" | "ollama_show" | "". Per-capability-group
	// provenance, deliberately NOT the mapping-wide MetricsSource, which one
	// writer would otherwise stamp over another's.
	CapabilitiesSource string
	// CapabilitiesCheckedAt is when the verdicts were last determined; nil
	// when never. Diagnostics and the portal tooltip ONLY -- no decision
	// logic may read it (the LiveProgressCheckedAt rule).
	CapabilitiesCheckedAt *time.Time
```

And the write value object plus the interface method (in the same file, beside `UpdateMappingLiveProgressSupport`'s declaration):

```go
// CapabilityVerdicts is one probe's capability answer set. Every verdict is
// "" (this probe determined nothing about it) | "yes" | "no"; "" fields are
// NOT written, so a partial answer (an older llama.cpp with modalities but no
// chat_template_caps) cannot clear what another probe established.
type CapabilityVerdicts struct {
	Vision string
	Video  string
	Audio  string
	Tools  string
	Extra  []string
	Source string
}

	// UpdateMappingCapabilities records the auto-detected capability verdicts
	// (#49-2). Writes only the non-empty verdicts, stamps
	// capabilities_source/capabilities_checked_at, and -- like
	// UpdateMappingLiveProgressSupport and unlike every metric writer on this
	// table -- carries NO metrics_locked guard and never touches
	// metrics_source/metrics_updated_at.
	UpdateMappingCapabilities(ctx context.Context, id string, caps CapabilityVerdicts, at time.Time) error
```

- [ ] **Step 4: Migration 77**

Registry entry after 76: `{version: 77, name: "model_mappings_capabilities", up: migration77Up},`

```go
// migration77Up adds the auto-detected capability columns (#49 sub-project
// 2): cap_vision/cap_video/cap_audio/cap_tools (each "" | "yes" | "no",
// zero-value-means-unknown like live_progress_support in migration76Up),
// cap_extra (a JSON array of capability names with no column of their own),
// capabilities_source (which probe produced them), and the nullable
// capabilities_checked_at. Append-only, no backfill.
//
// Like migration76Up's columns and for the same reason, these are NOT part of
// the metrics_locked group: see SQLiteStore.UpdateMappingCapabilities.
func migration77Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{
		"cap_vision text not null default ''",
		"cap_video text not null default ''",
		"cap_audio text not null default ''",
		"cap_tools text not null default ''",
		"cap_extra text not null default ''",
		"capabilities_source text not null default ''",
	} {
		if err := addColumnIfMissing(ctx, tx, dl, "model_mappings", col); err != nil {
			return err
		}
	}
	return addColumnIfMissing(ctx, tx, dl, "model_mappings",
		"capabilities_checked_at "+dl.timestampType())
}
```

- [ ] **Step 5: The SQLite writer, the select/scan, and MemoryStore parity**

Writer (after `UpdateMappingLiveProgressSupport`), building the SET clause from the non-empty verdicts only:

```go
// UpdateMappingCapabilities records auto-detected capability verdicts (#49-2).
//
// Only NON-EMPTY verdicts are written. That is the whole point: a probe that
// determined vision but nothing about tools (an older llama.cpp answering
// modalities without chat_template_caps) must leave cap_tools exactly as it
// was, not clear it. "" is never a value, only ever "nothing to say".
//
// Like UpdateMappingLiveProgressSupport -- read its doc for the full
// argument -- this carries NO `and metrics_locked = 0` guard and does not
// touch metrics_source/metrics_updated_at: metrics_locked exists so an
// operator can pin numbers they answer for, and a capability is not such a
// number. The one place a capability DOES respect the lock is the vision sync
// onto vision_capable, which deliberately goes through the lock-guarded
// UpdateMappingVisionCapable instead (see the write-back callers).
func (s *SQLiteStore) UpdateMappingCapabilities(ctx context.Context, id string, caps routing.CapabilityVerdicts, at time.Time) error {
	sets := []string{}
	args := []any{}
	for _, f := range []struct {
		col     string
		verdict string
	}{
		{"cap_vision", caps.Vision},
		{"cap_video", caps.Video},
		{"cap_audio", caps.Audio},
		{"cap_tools", caps.Tools},
	} {
		if f.verdict == "" {
			continue
		}
		sets = append(sets, f.col+" = ?")
		args = append(args, f.verdict)
	}
	if len(caps.Extra) > 0 {
		encoded, err := json.Marshal(caps.Extra)
		if err != nil {
			return fmt.Errorf("update mapping capabilities: encode extra: %w", err)
		}
		sets = append(sets, "cap_extra = ?")
		args = append(args, string(encoded))
	}
	if len(sets) == 0 {
		return nil // nothing determined: not an error, and not a write
	}
	sets = append(sets, "capabilities_source = ?", "capabilities_checked_at = ?")
	args = append(args, caps.Source, at, id)
	_, err := s.exec(ctx, `update model_mappings set `+strings.Join(sets, ", ")+` where id = ?`, args...)
	if err != nil {
		return fmt.Errorf("update mapping capabilities: %w", err)
	}
	return nil // 0 rows affected (missing mapping) is a benign no-op
}
```

(Add `encoding/json` / `strings` imports if absent. If the file's `exec` helper or query style differs, follow the file.) Then: add the seven columns to every `select`/scan of `model_mappings` that already lists `live_progress_support` (`grep -n live_progress_support gateway/backend/internal/store/sqlite_applications.go` and any sibling query files) — column order in the select must match the scan order exactly. Mirror the writer in `MemoryStore` with the same non-empty-only semantics.

- [ ] **Step 6: Run the test, verify it passes; then the full store package**

Run: `cd gateway/backend && go test ./internal/store/ ./internal/routing/`
Expected: PASS, including every pre-existing mapping test (a broken select/scan pairing shows up here).

- [ ] **Step 7: Revert-verify**

`git diff HEAD -- gateway/backend/internal/store/ gateway/backend/internal/routing/store.go gateway/backend/internal/routing/memory_store.go > /tmp/a1.patch`, then `git checkout HEAD --` those paths (test file stays), run the new test → compile failure. `git apply /tmp/a1.patch`, re-run → PASS. Additionally mutate the writer's `if f.verdict == "" { continue }` to write empty verdicts → the "empty verdict cleared a stored one" assertion must fail. Restore.

- [ ] **Step 8: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/store gateway/backend/internal/routing
git commit -m "feat(gateway): capability verdict columns and a lock-free writer

Migration 77 adds cap_vision/cap_video/cap_audio/cap_tools (three-state
\"\"|yes|no), cap_extra, capabilities_source and capabilities_checked_at.
UpdateMappingCapabilities writes only the verdicts a probe actually
determined, so a partial answer cannot clear what another probe
established, and carries no metrics_locked guard -- the
live_progress_support precedent (#49-2)."
```

---

### Task 2: The detector, twice

**Files:**
- Modify: `gateway/backend/internal/provider/model_info.go` (after `detectLiveProgressSupport` ~:193)
- Modify: `server-agent/internal/collector/probe.go` (after its `detectLiveProgressSupport` ~:314)
- Test: `gateway/backend/internal/provider/model_info_test.go` and `server-agent/internal/collector/probe_test.go` (both beside the existing `TestDetectLiveProgressSupport` cases)

**Interfaces:**
- Produces: `Capabilities` (provider) / `Capabilities` (collector) — the same struct, both unexported-consumed; and `detectCapabilities(body []byte) Capabilities` in both. Task 3 consumes the collector copy, Task 5 the provider copy.

- [ ] **Step 1: Write the failing tests (both modules, same table)**

Table-driven, in each module's own style. The cases are the contract:

```go
func TestDetectCapabilities(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Capabilities
	}{
		{
			name: "full modalities and tool caps",
			body: `{"modalities":{"vision":true,"video":false,"audio":true},
			        "chat_template_caps":{"supports_tools":true}}`,
			want: Capabilities{Vision: "yes", Video: "no", Audio: "yes", Tools: "yes"},
		},
		{
			// An older server predates chat_template_caps (2026-01-22): tools
			// is UNKNOWN, never "no" -- it was not asked.
			name: "modalities without tool caps",
			body: `{"modalities":{"vision":true,"video":false,"audio":false}}`,
			want: Capabilities{Vision: "yes", Video: "no", Audio: "no"},
		},
		{
			// A key missing from a PRESENT modalities object is unknown too:
			// an older server predates that key (audio 2025-05-23, video
			// 2026-06-08).
			name: "partial modalities object",
			body: `{"modalities":{"vision":true}}`,
			want: Capabilities{Vision: "yes"},
		},
		{
			name: "tool caps without modalities",
			body: `{"chat_template_caps":{"supports_tools":false}}`,
			want: Capabilities{Tools: "no"},
		},
		{
			// llama.cpp router mode answers /props with its OWN build's dummy
			// (#55). Reading it as the child's evidence would be permanent
			// under the no-rewrite discipline.
			name: "router dummy yields nothing",
			body: `{"role":"router","modalities":{"vision":true},"chat_template_caps":{"supports_tools":true}}`,
			want: Capabilities{},
		},
		{name: "not a props document", body: `{"data":[{"id":"m"}]}`, want: Capabilities{}},
		{name: "invalid json", body: `{`, want: Capabilities{}},
		{name: "null body", body: `null`, want: Capabilities{}},
		{
			name: "non-bool modality values are not evidence",
			body: `{"modalities":{"vision":"yes","audio":1}}`,
			want: Capabilities{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectCapabilities([]byte(tc.body))
			if got.Vision != tc.want.Vision || got.Video != tc.want.Video ||
				got.Audio != tc.want.Audio || got.Tools != tc.want.Tools {
				t.Fatalf("detectCapabilities = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The two module copies must not drift: this pins the shared contract from
// the sibling module's side too (the detectLiveProgressSupport precedent).
func TestDetectCapabilitiesRouterGateMatchesLiveProgressGate(t *testing.T) {
	body := []byte(`{"role":"router","modalities":{"vision":true}}`)
	if got := detectCapabilities(body); got != (Capabilities{}) {
		t.Fatalf("router dummy yielded %+v", got)
	}
	if got := detectLiveProgressSupport(body); got != "" {
		t.Fatalf("sibling detector disagrees on the router gate: %q", got)
	}
}
```

- [ ] **Step 2: Run them, verify they fail** — `cd gateway/backend && go test ./internal/provider/ -run TestDetectCapabilities` and `cd server-agent && go test ./internal/collector/ -run TestDetectCapabilities`: compile failure (undefined), the valid failing state.

- [ ] **Step 3: Implement, byte-identically in both files**

```go
// Capabilities is the capability verdict set one probe document yields. Every
// field is "" (this document said nothing about it) | "yes" | "no". The zero
// value means "nothing determined", and no field may be written to storage
// when it is "" -- see routing.CapabilityVerdicts.
type Capabilities struct {
	Vision string
	Video  string
	Audio  string
	Tools  string
	Extra  []string
}

// detectCapabilities is the capability detector for a llama.cpp /props
// document (#49 sub-project 2). It reads two objects and nothing else:
//
//   - modalities{vision,video,audio}: the server's own per-modality input
//     support. A key PRESENT as a bool answers yes/no; a key ABSENT from an
//     otherwise present modalities object stays "" -- an older build simply
//     predates it (audio landed 2025-05-23, video 2026-06-08), and absence is
//     not a denial.
//   - chat_template_caps.supports_tools: whether the model's chat template
//     NATIVELY supports tool calls. It is not a claim that tool calls work:
//     with --jinja (llama.cpp's default since 2025-11-27) tools are accepted
//     for every model through a generic handler, so "no" here means degraded
//     prompt quality, not a rejected request. The whole object is absent on
//     servers older than 2026-01-22, which is "" -- not "no".
//
// A caveat that must travel with cap_video wherever it is shown: upstream's
// modalities.video is true when the BINARY was built with video support AND
// the model has a vision encoder (mtmd_helper_support_video returns
// mtmd_support_vision under #ifdef MTMD_VIDEO). It is a build-plus-vision
// fact, not "this model understands video".
//
// The router gate is the same one detectLiveProgressSupport carries and for
// the same reason (#55): llama.cpp's ROUTER mode answers /props with a dummy
// describing the ROUTER's build, not the child's. A wrong verdict read from
// it would be permanent and self-reinforcing, because every later probe
// returns the same dummy and the no-rewrite guard then keeps it.
//
// This is a DUPLICATE, on purpose, of detectCapabilities in
// <the sibling path> -- the two are separate Go modules and cannot share
// code, mirroring the "DUPLICATED locally on purpose" precedent at
// gateway/backend/internal/provider/memory_probe.go:111. Whoever changes this
// rule must change that copy identically, or the two halves of this feature
// will drift. A reviewer finding them divergent is a real finding; finding
// them duplicated is expected.
func detectCapabilities(body []byte) Capabilities {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return Capabilities{}
	}
	if role, ok := obj["role"].(string); ok && role == "router" {
		return Capabilities{}
	}
	out := Capabilities{}
	if mods, ok := obj["modalities"].(map[string]any); ok {
		out.Vision = capVerdict(mods["vision"])
		out.Video = capVerdict(mods["video"])
		out.Audio = capVerdict(mods["audio"])
	}
	if caps, ok := obj["chat_template_caps"].(map[string]any); ok {
		out.Tools = capVerdict(caps["supports_tools"])
	}
	return out
}

// capVerdict maps a JSON bool to "yes"/"no" and everything else -- absent,
// null, a string, a number -- to "" (not evidence).
func capVerdict(v any) string {
	b, ok := v.(bool)
	if !ok {
		return ""
	}
	if b {
		return "yes"
	}
	return "no"
}
```

Each copy's doc comment names the *other* file as the sibling path. Bodies must be byte-identical.

- [ ] **Step 4: Run both, verify they pass**; then both packages: `go test ./internal/provider/` and `go test ./internal/collector/`.

- [ ] **Step 5: Revert-verify + drift check**

Revert each detector separately (production file only) → that module's test fails. Restore. Then prove the copies are identical:

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/capability-autodetect
diff <(sed -n '/^func detectCapabilities/,/^}/p' gateway/backend/internal/provider/model_info.go) \
     <(sed -n '/^func detectCapabilities/,/^}/p' server-agent/internal/collector/probe.go) && echo IDENTICAL
```

Expected: `IDENTICAL`. Record the output in the report; also mutate the router gate in ONE copy and confirm the diff catches it.

- [ ] **Step 6: Lint both modules + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./internal/provider/
cd ../../server-agent && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./internal/collector/
```

```bash
git add gateway/backend/internal/provider/model_info.go gateway/backend/internal/provider/model_info_test.go \
        server-agent/internal/collector/probe.go server-agent/internal/collector/probe_test.go
git commit -m "feat: detectCapabilities reads llama.cpp modalities and tool-template caps

Two byte-identical copies, one per Go module, carrying the router-dummy
gate its detectLiveProgressSupport sibling already carries (#55). A key
absent from a present object stays unknown -- an older build predates it,
and absence is not a denial (#49-2)."
```

---

### Task 3: One `/props` fetch, many verdicts (agent)

**Files:**
- Modify: `server-agent/internal/collector/probe.go` (`ProbeLiveProgressSupport` ~:207-249 becomes `ProbePropsVerdicts`)
- Modify: `server-agent/internal/agent/agent.go` (`runtimeCapabilityEntry` ~:1071, `probeRuntimeChildLiveProgress` ~:1250-1270, cache doc ~:497)
- Modify: `server-agent/internal/sample/sample.go` (`RuntimeSample` ~:130-155)
- Test: `server-agent/internal/collector/probe_test.go`, `server-agent/internal/agent/agent_test.go`

**Interfaces:**
- Consumes: Task 2's collector `detectCapabilities`/`Capabilities`.
- Produces: `collector.PropsVerdicts` + `collector.ProbePropsVerdicts`; `sample.RuntimeSample.Capabilities *SampleCapabilities` (Task 4 consumes the wire shape).

- [ ] **Step 1: Write the failing tests**

In `probe_test.go`, beside `TestProbeLiveProgressSupport_ConclusiveRefusals` (read it first and mirror its httptest setup):

```go
// One GET, every verdict: the whole point of ProbePropsVerdicts is that a
// llama_cpp child is not asked for /props twice (once for live progress, once
// for capabilities) -- the hit counter is the assertion that matters.
func TestProbePropsVerdictsFetchesOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != LiveProgressProbePath {
			t.Errorf("probed %q, want %q", r.URL.Path, LiveProgressProbePath)
		}
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":false}},
		                        "modalities":{"vision":true,"video":false,"audio":false},
		                        "chat_template_caps":{"supports_tools":true}}`))
	}))
	defer srv.Close()

	v, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
	if !stable {
		t.Fatal("a parsed /props document must be stable")
	}
	if v.LiveProgress != "supported" {
		t.Fatalf("LiveProgress = %q, want supported", v.LiveProgress)
	}
	if v.Caps.Vision != "yes" || v.Caps.Video != "no" || v.Caps.Tools != "yes" {
		t.Fatalf("Caps = %+v", v.Caps)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want exactly 1", got)
	}
}

// The conclusive/transient contract is unchanged -- it is a pinned regression
// anchor, restated here on the widened function.
func TestProbePropsVerdictsKeepsTheConclusiveSet(t *testing.T) {
	for _, status := range []int{404, 401, 403, 405} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		v, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if !stable {
			t.Fatalf("status %d must be conclusive", status)
		}
		if v.LiveProgress != "" || v.Caps != (Capabilities{}) {
			t.Fatalf("status %d yielded verdicts: %+v", status, v)
		}
	}
	for _, status := range []int{500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		_, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if stable {
			t.Fatalf("status %d must be transient", status)
		}
	}
}
```

In `agent_test.go`, beside `TestCollectOnceRuntimeLiveProgressCustomTypeSupported` (copy its harness verbatim):

```go
// The cache carries capabilities too, so a second cycle re-probes nothing --
// and the sample still reports them.
func TestCollectOnceRuntimeCapabilitiesCachedAcrossCycles(t *testing.T)

// A PID change re-arms the question for capabilities exactly as it does for
// the live-progress verdict.
func TestCollectOnceRuntimeCapabilitiesPidChangeRearms(t *testing.T)
```

Both assert `/props` hit counts numerically (1 after two cycles; 2 after a PID change) and that `sample.Runtimes[0].Capabilities` carries the verdicts.

- [ ] **Step 2: Run them, verify they fail** — compile failure for the new symbols.

- [ ] **Step 3: Widen the probe**

Rename `ProbeLiveProgressSupport` → `ProbePropsVerdicts`, returning:

```go
// PropsVerdicts is everything one /props document tells us. Widened from a
// single live-progress verdict (#51) so that a llama_cpp child is fetched
// ONCE per cache miss rather than once per verdict kind (#49-2): the context
// probe already GETs /props separately, and a third GET for capabilities
// would have made three requests per cycle for the same document.
type PropsVerdicts struct {
	// LiveProgress is the unchanged #51 verdict: "supported" | "unsupported"
	// | "" (unknown).
	LiveProgress string
	// Caps is the capability verdict set from the same document (#49-2).
	Caps Capabilities
}
```

Keep the function's entire existing doc comment about the conclusive set (`{404, 401, 403, 405}`), the transient statuses, and the invalid-JSON rule **verbatim** — only the return type changes:

```go
func ProbePropsVerdicts(ctx context.Context, client *http.Client, baseURL string) (verdicts PropsVerdicts, stable bool) {
	body, status, err := fetchProbeBody(ctx, client, baseURL, LiveProgressProbePath)
	if err != nil {
		stable := status == http.StatusNotFound ||
			status == http.StatusUnauthorized ||
			status == http.StatusForbidden ||
			status == http.StatusMethodNotAllowed
		return PropsVerdicts{}, stable
	}
	if !json.Valid(body) {
		return PropsVerdicts{}, false
	}
	return PropsVerdicts{
		LiveProgress: detectLiveProgressSupport(body),
		Caps:         detectCapabilities(body),
	}, true
}
```

- [ ] **Step 4: Widen the cache and the sample**

`runtimeCapabilityEntry` becomes `{pid int; verdicts collector.PropsVerdicts}`; update the cache's doc comment to say it now caches every `/props`-derived verdict for this PID generation (keeping its "deliberately separate from `runtimeCtxCache`" paragraph intact). Rename `probeRuntimeChildLiveProgress` → `probeRuntimeChildProps` and fill both `rs.LiveProgressSupport` and `rs.Capabilities`; the transient branch and its `slog.Debug` stay as they are.

Wire field, in `sample.RuntimeSample` after `LiveProgressSupport`:

```go
	// Capabilities is the auto-detected capability verdict set from the same
	// /props document that yields LiveProgressSupport above (#49-2). A
	// POINTER with omitempty, deliberately: nil distinguishes "this agent
	// predates capability detection" from "detected, nothing determined"
	// (an all-empty struct) -- a distinction the string fields above cannot
	// make for themselves.
	Capabilities *SampleCapabilities `json:"capabilities,omitempty"`
```

```go
// SampleCapabilities is RuntimeSample.Capabilities' payload: each verdict is
// "" | "yes" | "no", and Extra carries capability names with no field of
// their own (empty for a llama.cpp child; Ollama's open vocabulary fills it).
type SampleCapabilities struct {
	Vision string   `json:"vision"`
	Video  string   `json:"video"`
	Audio  string   `json:"audio"`
	Tools  string   `json:"tools"`
	Extra  []string `json:"extra,omitempty"`
}
```

- [ ] **Step 5: Run the tests; then the whole module**

Run: `cd server-agent && go test ./...`
Expected: PASS, with **every** pre-existing live-progress test passing unchanged in substance. Call-site renames are permitted; assertion changes are not — if an assertion seems to need changing, stop and report it.

- [ ] **Step 6: Revert-verify + the one-fetch mutation**

Revert `probe.go`+`agent.go`+`sample.go` (tests stay) → new tests fail. Restore. Then mutate `ProbePropsVerdicts` to fetch twice (call `fetchProbeBody` a second time for the capability parse) → `TestProbePropsVerdictsFetchesOnce` must fail on the hit count. Restore.

- [ ] **Step 7: Lint + commit**

```bash
cd server-agent && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./...
```

```bash
git add server-agent/internal/collector server-agent/internal/agent server-agent/internal/sample
git commit -m "feat(agent): one /props fetch yields every verdict

ProbeLiveProgressSupport becomes ProbePropsVerdicts: same conclusive set,
same transient rule, same stable-empty caching -- widened payload. The
capability verdicts ride the document the live-progress probe already
fetched, so a llama_cpp child is not asked a third time per cycle (#49-2)."
```

---

### Task 4: Ingest write-back, and the `vision_capable` sync

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`agentRuntimeSample` ~:183; new `writeBackRuntimeCapabilities` after `writeBackRuntimeLiveProgress` ~:812; the gate call site ~:1170)
- Test: `gateway/backend/internal/gateway/agent_ingest_test.go`

**Interfaces:**
- Consumes: Task 1's `routing.CapabilityVerdicts` + `UpdateMappingCapabilities`; Task 3's wire shape; the existing `UpdateMappingVisionCapable`.

- [ ] **Step 1: Write the failing tests**

Beside the existing live-progress write-back tests (find with `grep -n "writeBackRuntimeLiveProgress\|LiveProgressSupport" gateway/backend/internal/gateway/agent_ingest_test.go` and copy the harness):

1. `TestIngestWritesBackCapabilities` — a sample with `capabilities:{vision:"yes",tools:"no"}` and the `runtime_model_probe` capability writes them once; a second identical sample writes nothing more (compare-to-stored; numeric write-count assertion on the fake store).
2. `TestIngestCapabilitiesEmptyNeverClears` — a sample with an all-empty capabilities object leaves stored verdicts untouched and calls the writer zero times.
3. `TestIngestCapabilitiesNilIsNotAClear` — a sample with **no** `capabilities` key (an older agent) calls the writer zero times.
4. `TestIngestCapabilitiesWithoutFeatureNeverWrites` — capabilities present but the sample's `capabilities.features` lacks `runtime_model_probe` → zero writes.
5. `TestIngestCapabilitiesCrossServerRejected` — a `spec_id` owned by another server → zero writes (and the `slog.Warn` path, asserted the way the sibling test asserts it).
6. `TestIngestVisionSyncWritesTheBoolThroughTheLockedWriter` — `vision: "yes"` calls `UpdateMappingVisionCapable(id, true, …)` exactly once; `"no"` calls it with `false`; `""` never calls it; and an unchanged bool is not rewritten.

- [ ] **Step 2: Run them, verify they fail** (compile failure for the wire field / the new writer).

- [ ] **Step 3: Implement**

Wire mirror in `agentRuntimeSample`, doc-commented like its `LiveProgressSupport` neighbour (nil = older agent, decoding to no write). Then:

```go
// writeBackRuntimeCapabilities persists each managed child's auto-detected
// capability verdicts onto its owning mapping (#49 sub-project 2).
//
// Structurally identical to writeBackRuntimeLiveProgress above -- read its
// doc for the reasoning behind every guard here: the runtimes cap, ownership
// memoized per DISTINCT spec_id with the cross-server rejection,
// compare-to-stored so an unchanged verdict is never rewritten, best-effort
// so a write failure never rejects the telemetry sample, and no
// metrics_locked check (a capability is not a pinned metric).
//
// Two differences worth stating, because both are deliberate:
//
//   - A nil Capabilities (an agent predating capability detection) and an
//     all-empty one (detection ran, nothing determined) are both "no write" --
//     but they are different facts, which is why the wire field is a pointer.
//   - A DEFINITIVE vision verdict additionally syncs onto the mapping's
//     vision_capable bool, through UpdateMappingVisionCapable and therefore
//     through its `and metrics_locked = 0` guard. That is not an
//     inconsistency with the lock-free write above: cap_vision is the honest
//     three-state record, while vision_capable is the compatibility surface
//     the models list and the portal chat's image gate already read, and an
//     operator who locked a mapping's metrics has pinned exactly that kind of
//     consumer-visible answer. "" syncs nothing -- unknown is not a clear.
func (s *Server) writeBackRuntimeCapabilities(ctx context.Context, serverID string, runtimes []agentRuntimeSample)
```

Its body follows `writeBackRuntimeLiveProgress` line for line, resolving ownership with the same helper, comparing each verdict against the mapping's stored `CapVision`/`CapVideo`/`CapAudio`/`CapTools` and building a `routing.CapabilityVerdicts` from **only the verdicts that differ and are non-empty** (so an unchanged verdict contributes nothing and an all-unchanged sample issues no write at all), `Source: "llama_cpp_props"`. After a successful capabilities write, the vision sync:

```go
		// Vision sync: see the doc above for why this one respects the lock.
		if caps.Vision != "" {
			want := caps.Vision == "yes"
			if want != r.storedVisionCapable {
				if err := s.Routes.UpdateMappingVisionCapable(ctx, r.mappingID, want, now); err != nil {
					slog.Debug("vision-capable sync failed", "server_id", serverID, "spec_id", specID, "mapping_id", r.mappingID, "err", err)
				} else {
					r.storedVisionCapable = want
				}
			}
		}
```

The resolution struct grows the stored capability verdicts and `storedVisionCapable`; extend the existing `resolveRuntimeSpecLiveProgress` (or add a sibling in its style) to return them, keeping its cross-server guard and `slog.Warn` unchanged.

Call site, inside the existing `if slices.Contains(caps, runtimeModelProbeFeature)` block, after `writeBackRuntimeLiveProgress`, with a comment saying it shares that gate for the same reason (the verdicts ride the same probe pass and therefore the same trust boundary).

- [ ] **Step 4: Run the tests, verify they pass**; then `go test ./internal/gateway/` (without `-race`; see the known #53 failure).

- [ ] **Step 5: Revert-verify + mutations**

Revert `agent_ingest.go` → new tests fail. Restore. Then: (a) drop the `caps.Vision != ""` guard → test 6's `""`-never-syncs assertion must fail; (b) drop the compare-to-stored on the verdicts → test 1's second-cycle write count must fail; (c) move the call outside the feature gate → test 4 must fail.

- [ ] **Step 6: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./internal/gateway/
```

```bash
git add gateway/backend/internal/gateway/agent_ingest.go gateway/backend/internal/gateway/agent_ingest_test.go
git commit -m "feat(gateway): telemetry write-back for capability verdicts

Mirrors writeBackRuntimeLiveProgress guard for guard, and additionally
syncs a definitive vision verdict onto the vision_capable bool through its
existing lock-guarded writer -- so the models list and the chat image gate
start working from probe evidence with no change of their own (#49-2)."
```

---

### Task 5: The gateway probe pass

**Files:**
- Modify: `gateway/backend/internal/provider/model_info.go` (`ModelInfo` ~:18-31, `parseModelInfo` ~:97-123, new `PickModelCapabilities` after `PickModelLiveProgressSupport` ~:252)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (the `{model}` branch's write block ~:756-779, and the single-probe branch's equivalent ~:803-850)
- Test: `gateway/backend/internal/provider/model_info_test.go`, `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: Task 2's provider detector, Task 1's writer.
- Produces: `ModelInfo.Caps`, `PickModelCapabilities(infos, model) Capabilities`.

- [ ] **Step 1: Write the failing tests**

`model_info_test.go`: `parseModelInfo` fills `Caps` from a `/props` body; a **nameless** document with capabilities but no live-progress verdict still yields one nameless entry carrying the caps (mirroring the existing nameless-verdict rule — read `parseModelInfo`'s doc first, the rule is subtle and the existing test names it); `PickModelCapabilities` prefers an exact `Name` match and otherwise takes the first entry with any non-empty verdict.

`app_health_test.go`, beside `TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle` (copy the harness): the `{model}` pass persists capabilities once and not again on an identical cycle (numeric write-count on the fake store's new `capabilitiesSetCount()`); an all-empty verdict set never calls the writer; the single-probe pass fans a nameless document's capabilities out to every mapping of the app.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement**

`ModelInfo` grows `Caps Capabilities` with a doc comment pointing at `detectCapabilities` and repeating the `""`-never-overwrites rule. `parseModelInfo` calls `detectCapabilities(body)` once and attaches it; the nameless-entry branch's condition widens from "a determinable live-progress verdict" to "a determinable live-progress verdict **or** any determined capability", and its comment explains that a capability, like the live-progress verdict, is a property of the server build rather than of a named model — which is why the nameless entry may carry it while deliberately still carrying no `n_ctx`.

`PickModelCapabilities` mirrors `PickModelLiveProgressSupport`: exact `Name` match wins; otherwise the first entry with any non-empty field; zero value when nothing is usable.

Both `app_health.go` write sites grow, beside the existing live-progress write:

```go
						// Additive capability write (#49-2), independent of both
						// outcomes above and read off the SAME probe response --
						// no extra request. Only the verdicts that differ from
						// what is stored are sent, so an unchanged capability set
						// issues no UPDATE on this ~30s cadence, and an unknown
						// verdict is never sent at all (unknown must never
						// overwrite a stored verdict). Deliberately not gated on
						// mp.MetricsLocked, for the reason
						// UpdateMappingCapabilities documents; the vision sync
						// below DOES respect the lock, because vision_capable is
						// the consumer-visible bool an operator pins.
```

with the changed-verdicts-only construction and the same vision sync as Task 4 (extract that sync into one small helper used by both call sites if the shape allows it without threading extra state; otherwise duplicate it with a comment naming the sibling).

- [ ] **Step 4: Run the tests; then both packages** (`./internal/provider/ ./cmd/gateway/`). Existing tests must pass with no assertion changes; fakes may only grow methods.

- [ ] **Step 5: Revert-verify + mutation** — revert each production file separately; then mutate the changed-verdicts-only construction to always send every verdict → the "not again on an identical cycle" assertion must fail.

- [ ] **Step 6: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/provider gateway/backend/cmd/gateway
git commit -m "feat(gateway): the app-health props pass persists capability verdicts

parseModelInfo carries the capability set, and both probe passes write the
verdicts that changed -- off the response they already fetched, so an
api-key-protected server_agent child gets its capabilities through the
#58 route with no extra request (#49-2)."
```

---

### Task 6: Portal

**Files:**
- Modify: `gateway/backend/internal/portal/service_model_servers.go` (DTO ~:60-77, fill ~:168-180)
- Modify: `gateway/frontend/src/components/ModelServersSection.tsx` (helpers ~:95-160, column list ~:336-370)
- Modify: `gateway/frontend/src/i18n.ts` (de + en)
- Test: `gateway/backend/internal/portal/service_model_servers_test.go`, `gateway/frontend/src/components/ModelServersSection.test.tsx`

**Interfaces:** consumes the mapping fields from Task 1. Produces no Go API.

- [ ] **Step 1: Write the failing tests**

Backend: the DTO carries `cap_vision`/`cap_video`/`cap_audio`/`cap_tools` (**no** `omitempty` — `""` must be an explicit empty string on the wire, the rule `live_progress_support` already follows), `cap_extra` as a decoded `[]string` (`omitempty`), `capabilities_source`, and `capabilities_checked_at` (`omitempty`), read straight off `view.mapping`.

Frontend, in `ModelServersSection.test.tsx` using the file's existing `cellForColumn` helper (a positional index provably false-passes here — the neighbouring columns render the same em-dash):
1. a row with `cap_vision: 'yes'`, `cap_tools: 'yes'` renders exactly two chips with those labels;
2. `cap_video: 'no'` renders **no** chip for video (negatives are not chips);
3. all-empty renders the em-dash `—`;
4. `cap_extra: ['thinking']` renders a chip labelled `thinking` verbatim;
5. the tooltip contains the provenance and the checked-at timestamp.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement**

DTO + fill in the service, doc-commented like the `LiveProgressSupport` neighbour (including why there is no gateway-injection seam: a background detector writes these to the mapping directly).

Frontend: a `capabilityChips(row, t)` helper returning one `{status: 'success', label}` per `yes` verdict in a fixed order (Vision, Video, Audio, Tools) followed by `cap_extra` entries verbatim, and a `capabilitiesTooltip(row, t)` folding in `capabilities_source` and `capabilities_checked_at` plus the two upstream caveats (video = build + vision; tools = native template quality). One new column after `liveProgress`:

```tsx
    {
      id: 'capabilities',
      label: t.modelServerColCapabilities,
      value: (r) => capabilityChips(r, t).map((c) => c.label).join(' '),
      searchable: true,
      render: (r) => {
        const chips = capabilityChips(r, t);
        // The em-dash, not an empty cell: "the column exists, nothing
        // determined yet" must be distinguishable from "the column is
        // missing" -- an indistinguishable empty cell has already cost a real
        // support report on a sibling table (see liveProgressChipInfo's own
        // note, and issue #57).
        if (chips.length === 0) return '—';
        return (
          <Tooltip title={capabilitiesTooltip(r, t)}>
            <span>
              {chips.map((c) => (
                <StatusChip key={c.label} status={c.status} label={c.label} />
              ))}
            </span>
          </Tooltip>
        );
      },
    },
```

Chips carry `data-status`, never a colour (house rule); `Tooltip` wraps a `<span>` because `StatusChip` is not a `forwardRef`. i18n keys (de/en): `modelServerColCapabilities` (`Fähigkeiten`/`Capabilities`), `capabilityVision` (`Vision`/`Vision`), `capabilityVideo` (`Video`/`Video`), `capabilityAudio` (`Audio`/`Audio`), `capabilityTools` (`Tools`/`Tools`), `modelServerCapabilitiesTooltip`, `modelServerCapabilitiesSource(source)`, `modelServerCapabilitiesCheckedAt(when)` — mirroring the live-progress key family's shapes.

- [ ] **Step 4: Run the tests + the frontend gates**

```bash
cd gateway/backend && go test ./internal/portal/
cd ../frontend && npm run format:check && npm run lint && npm run build && npm test
```

- [ ] **Step 5: Revert-verify** — revert the production files (tests stay): the backend DTO test fails on the missing fields; each frontend test fails for its own reason (missing chips / a rendered chip for `no` / a missing em-dash). Restore.

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/portal gateway/frontend/src
git commit -m "feat(portal): a Capabilities column on the model-servers table

One chip per determined capability, nothing for a negative, an em-dash
when nothing was determined -- and a tooltip carrying the provenance plus
the two upstream caveats (video is a build-plus-vision fact; tools is
native-template quality, not availability) (#49-2)."
```

---

### Task 7: Documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (the §8.4.3 region that documents the `/props` detectors and the live-progress verdict)
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§10's probe list; §7's feature registry only if a new flag were added — it is not, so leave it)
- Modify: `docs/architecture/reference/data-model.md` (migration 77 + the new columns, beside migration 76's entry)
- Modify: `docs/architecture/reference/api-surface.md` (the `/api/portal/model-servers` row fields)
- Modify: `docs/architecture/09-architecture-decisions.md` (append ADR-038)

(Re-locate every section by heading text, not line number.)

- [ ] **Step 1: Document the detector and the two probe paths**

In telemetry §8.4.3, beside the live-progress detector's description: the capability detector, its two source objects, the "absent key is unknown, never no" rule, the router gate, and the one-fetch property (the agent's `/props` is fetched once per PID generation for every verdict it yields). State the two upstream caveats exactly as the code comments state them.

- [ ] **Step 2: Data model + API surface**

Migration 77's seven columns with their three-state semantics and the explicit note that they sit outside the `metrics_locked` group while the `vision_capable` sync deliberately goes through the lock. The model-servers DTO's new fields.

- [ ] **Step 3: ADR-038**

Append in the log's context→decision→consequence shape, status Accepted: **ADR-038 — Auto-detected capabilities are three-state, outside the metrics lock, and sync onto the legacy vision bool.** Context: two contradictory precedents (`vision_capable`, a bool inside the lock stamped `metrics_source='vision'`; `live_progress_support`, three-state outside it), and a bool that cannot say "not probed" while the models list AND-aggregates it fail-closed. Decision: new capabilities follow the `live_progress_support` precedent with their own provenance columns, and a definitive vision verdict additionally writes the legacy bool through its lock-guarded writer. Consequence: the honest record and the compatibility surface are separate columns; the chat image gate gains probe evidence with no change of its own; `metrics_source` is not further overloaded; vLLM and TGI remain uncovered by capability probing (no HTTP surface exposes it — recorded in §8.4.3), with the vision benchmark as their only path.

- [ ] **Step 4: Verify the docs gates**

```bash
./scripts/check-docs.sh && bash ./scripts/check-docs.test.sh
```

- [ ] **Step 5: Commit**

```bash
git add docs/architecture
git commit -m "docs: capability auto-detect, migration 77, and ADR-038

The detector's evidence rule and its two upstream caveats, the new
columns' three-state semantics and their position outside the metrics
lock, the deliberate vision-bool sync, and the recorded vLLM/TGI gap
(#49-2)."
```

---

## Final verification (after all tasks, before the PR)

- Both Go modules: `golangci-lint fmt --diff` + `run` + `go test ./...`.
- Frontend: `format:check` + `lint` + `build` + `test`.
- Docs: `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- Detector drift check (Task 2 Step 5's `diff` one-liner) must print `IDENTICAL`.
- Sonar from the repo root: `make sonar-up sonar-gate sonar-findings sonar-branch-findings sonar-down` — judge by `Attributed: N on lines this branch changed` (want 0). Note: `sonar-gate` needs `gateway/frontend/node_modules` (run `npm ci` there first) or it dies in the coverage step and `branch-findings` then judges against a stale `findings.json` — the exact trap that hid three findings on PR #62.
- `docs/superpowers/` is removed in the PR-preparation step, never in these tasks.
