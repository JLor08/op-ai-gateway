// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// capabilityResetRecorder wraps a *routing.MemoryStore and records every
// DeleteMappingCapability call made through it, optionally failing them.
//
// The recording is the instrument for two things a store's CONTENTS cannot
// show. For the AUTHORISATION tests: this path gets no existence signal at all
// (an unknown mapping id, an unknown capability and an empty capability are
// each a benign nil on both drivers), so "the caller was refused" and "the
// caller was allowed and deleted nothing" are indistinguishable without a call
// count. For the INERT-SAVE test: an already-absent row stays absent whether a
// delete was issued or skipped, and only the count says which.
type capabilityResetRecorder struct {
	*routing.MemoryStore
	deletes []capabilityDelete
	// upserts is recorded by the SAME wrapper on purpose. Two wrappers
	// composed around one MemoryStore do not compose: the outer one promotes
	// the inner one's OTHER methods off the embedded store instead, so the
	// inner counter never sees them and an assertion on it passes trivially.
	upserts []capabilityUpsert
	// failWith, when non-nil, is returned INSTEAD of performing the delete.
	failWith error
}

func (c *capabilityResetRecorder) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	c.upserts = append(c.upserts, capabilityUpsert{mappingID: mappingID, rows: rows})
	return c.MemoryStore.UpsertMappingCapabilities(ctx, mappingID, rows)
}

type capabilityDelete struct {
	mappingID  string
	capability string
}

func (c *capabilityResetRecorder) DeleteMappingCapability(ctx context.Context, mappingID, capability string) error {
	c.deletes = append(c.deletes, capabilityDelete{mappingID: mappingID, capability: capability})
	if c.failWith != nil {
		return c.failWith
	}
	return c.MemoryStore.DeleteMappingCapability(ctx, mappingID, capability)
}

// failingCapabilityUpsertStore fails only UpsertMappingCapabilities, so a test
// can pin the deliberate ASYMMETRY between the two capability writers in
// UpdateMapping: the upsert is best-effort (the mapping update it accompanies
// already landed), the delete is not (it IS the operator's whole effect).
type failingCapabilityUpsertStore struct {
	*routing.MemoryStore
	err error
}

func (f *failingCapabilityUpsertStore) UpsertMappingCapabilities(context.Context, string, []routing.CapabilityRow) error {
	return f.err
}

// capabilityTestFixture is the shared setup: a server, an application and one
// mapping, over a route store the caller supplies (usually a recorder).
type capabilityTestFixture struct {
	svc       *Service
	mappingID string
	appID     string
}

func newCapabilityTestFixture(t *testing.T, now time.Time, routes routing.Store) capabilityTestFixture {
	t.Helper()
	ctx := context.Background()
	svc := newServerTestServiceWithRoutes(t, now, routes)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "g", AppModelName: "a", ContextSize: 4096,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return capabilityTestFixture{svc: svc, mappingID: mapping.ID, appID: app.ID}
}

// seededVerdict is MappingForm's SEEDING rule, restated here so the tests
// below submit what the real UI would submit rather than a hand-picked body:
// each capability control is seeded from the DTO's capability ROWS -- the
// matching entry's verdict, or "" when there is no entry -- never from the
// folded boolean, which cannot express unknown.
func seededVerdict(dto ModelMappingDTO, capability string) string {
	for _, row := range dto.Capabilities {
		if row.Capability == capability {
			return row.Verdict
		}
	}
	return ""
}

// formControl is one of the form's two capability selects: what it was seeded
// with when the form opened, and what the operator left it on.
type formControl struct {
	capability   string
	seed, chosen string
}

// formCapabilityVerdicts is MappingForm's SUBMIT rule, likewise restated: one
// `capability_verdicts` entry per control the operator actually MOVED, holding
// the value they moved it to ("yes", "no", or "" for unknown), and NO entry at
// all for an untouched control. The legacy is_mtp/vision_capable booleans are
// never sent for these two capabilities.
//
// The helper is used to DERIVE request bodies, never asserted on: a test that
// checks its own helper against its own doc comment proves nothing about
// either side of the wire.
func formCapabilityVerdicts(controls ...formControl) map[string]string {
	out := map[string]string{}
	for _, control := range controls {
		if control.chosen == control.seed {
			continue
		}
		out[control.capability] = control.chosen
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TestUpdateMappingUnknownToNoWritesAManualNoRow is the defect this field
// exists to close, and it is the reason `capability_verdicts` is compared
// against the stored ROW rather than against the two-state fold.
//
// A mapping with no `vision` row (every fresh mapping on a vLLM/Ollama
// application) reads UNKNOWN in the form. An operator who picks *Nein* is
// stating something real: a `manual` "no" is rank 3, and it is what stops a
// later llama_cpp_props probe from writing "yes" on a model whose vision they
// have judged unusable. Compared against the fold -- where no row and a
// verdict of "no" are the same `false` -- that transition found no difference,
// wrote nothing, answered 200, and the re-opened form read *Unbekannt* again.
func TestUpdateMappingUnknownToNoWritesAManualNoRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	seed, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	before := seed.Data[0]
	if got := seededVerdict(before, routing.CapabilityVision); got != "" {
		t.Fatalf("seeded vision verdict = %q, want \"\" -- this test starts from UNKNOWN", got)
	}

	// The operator moves the control from unknown to Nein, and edits nothing
	// else. Body derived through the form's own submit rule.
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: formCapabilityVerdicts(
			formControl{capability: routing.CapabilityVision, seed: seededVerdict(before, routing.CapabilityVision), chosen: routing.CapabilityNo},
			formControl{capability: routing.CapabilityMTP, seed: seededVerdict(before, routing.CapabilityMTP), chosen: seededVerdict(before, routing.CapabilityMTP)},
		),
	})
	if err != nil {
		t.Fatalf("UpdateMapping (unknown -> no): %v", err)
	}

	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, fx.mappingID))
	got, ok := rows[routing.CapabilityVision]
	if !ok {
		t.Fatalf("capability rows after unknown -> no = %+v, want a vision row -- the operator's NEGATIVE verdict must be STORED, not discarded", rows)
	}
	if got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual || !got.CheckedAt.Equal(now) {
		t.Fatalf("vision row = %+v, want no/manual@%v (rank 3, which is what keeps a later probe from writing yes)", got, now)
	}
	// The UNTOUCHED control must not have written anything.
	if _, ok := rows[routing.CapabilityMTP]; ok {
		t.Fatalf("mtp row = %+v, want NONE -- an untouched control states nothing", rows[routing.CapabilityMTP])
	}

	// And the response is what the form re-seeds from, so it must say "no"
	// rather than the unknown it started at -- the visible half of the same
	// defect (re-opening the form showed Unbekannt again).
	if seeded := seededVerdict(dto, routing.CapabilityVision); seeded != routing.CapabilityNo {
		t.Fatalf("response seeds vision to %q, want %q", seeded, routing.CapabilityNo)
	}
	if dto.VisionCapable {
		t.Fatalf("vision_capable = true, want false -- the FOLD of a \"no\" row is still false")
	}
}

// TestUpdateMappingCapabilityVerdictResetAndInertSave covers the other two
// transitions of the tri-state field:
//
//   - "no" -> unknown DELETES the row. A determined negative is not the same
//     state as nothing determined, so returning to unknown has to remove the
//     row rather than write anything.
//   - unknown -> unknown writes NOTHING AT ALL -- not even the benign delete
//     an unconditional reset would issue. An intent equal to what is stored is
//     inert, which is what keeps a save made for an unrelated reason from
//     touching the capability table.
//
// The delete COUNT is the only instrument for the second claim: an
// already-absent row stays absent whether the delete was skipped or issued.
func TestUpdateMappingCapabilityVerdictResetAndInertSave(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("seed no row: %v", err)
	}
	recorder.upserts = nil // the seed went round the wrapper; it is not under test

	// "no" -> unknown.
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: formCapabilityVerdicts(
			formControl{capability: routing.CapabilityVision, seed: routing.CapabilityNo, chosen: ""},
		),
	})
	if err != nil {
		t.Fatalf("UpdateMapping (no -> unknown): %v", err)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after no -> unknown = %+v, want NONE (unknown is the ABSENCE of a row)", rows)
	}
	if got := seededVerdict(dto, routing.CapabilityVision); got != "" {
		t.Fatalf("response seeds vision to %q, want \"\"", got)
	}
	if len(recorder.deletes) != 1 {
		t.Fatalf("deletes = %+v, want exactly 1", recorder.deletes)
	}

	// unknown -> unknown, from a client that restates the state it read. The
	// form itself would omit the key entirely; a script need not, and either
	// way NOTHING may reach the store -- neither an upsert nor the benign
	// delete an unconditional reset would issue.
	recorder.upserts = nil
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (unknown -> unknown): %v", err)
	}
	if len(recorder.deletes) != 1 {
		t.Fatalf("deletes after an unknown -> unknown save = %+v, want STILL 1 -- an intent equal to what is stored is inert", recorder.deletes)
	}
	if len(recorder.upserts) != 0 {
		t.Fatalf("capability upserts on an unknown -> unknown save = %+v, want NONE", recorder.upserts)
	}
}

// TestUpdateMappingCapabilityVerdictsApplyInADeterministicOrder: a Go map
// iterates in a RANDOMISED order, and two resets in one request are not atomic
// with each other (the store method is per-capability and takes no
// transaction). Without a deterministic order, which capability a partial
// failure leaves applied -- and which one's store error the operator sees --
// would differ between two runs of the identical request, which is not
// something a bug report could ever be reproduced from.
//
// Twenty iterations, because one run of an unsorted map has an even chance of
// coming out alphabetical anyway.
func TestUpdateMappingCapabilityVerdictsApplyInADeterministicOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	for i := range 20 {
		if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
			{Capability: routing.CapabilityTools, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
			{Capability: routing.CapabilityMTP, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		}); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
		recorder.deletes = nil
		if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
			CapabilityVerdicts: map[string]string{
				routing.CapabilityVision: "", routing.CapabilityTools: "", routing.CapabilityMTP: "",
			},
		}); err != nil {
			t.Fatalf("UpdateMapping (three resets): %v", err)
		}
		got := make([]string, 0, len(recorder.deletes))
		for _, del := range recorder.deletes {
			got = append(got, del.capability)
		}
		want := []string{routing.CapabilityMTP, routing.CapabilityTools, routing.CapabilityVision}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("iteration %d: deletes reached the store as %v, want %v (alphabetical, so an identical request behaves identically)", i, got, want)
		}
	}
}

// TestUpdateMappingLegacyBooleanFromAnAlwaysSubmitClientNeverMintsAVerdict is
// the regression guard for the COMPATIBILITY path, and the reason the two
// comparison rules must NOT be collapsed into one however redundant they look.
//
// The old mapping form re-submitted `is_mtp` and `vision_capable` on EVERY
// save whatever field the operator came to edit; an old cached portal bundle
// during a rollout, or any script built against that shape, still does. Those
// booleans are therefore compared against the two-state FOLD the client was
// seeded with, where no row and a verdict of "no" are both `false` -- so an
// unconditional `vision_capable: false` against a mapping with no row is an
// unchanged submission and must write nothing.
//
// Re-pointing them at the stored row -- the obvious "fix" for the unknown ->
// "no" defect above -- would turn every one of those saves into a permanent
// `manual` "no" on a capability nobody ever judged, freezing out every probe
// and the vision benchmark. That is the critical defect this branch already
// closed once. An operator states a negative through `capability_verdicts`,
// whose present keys cannot be an artefact of the form having been submitted;
// the contrast is asserted at the bottom of this test.
func TestUpdateMappingLegacyBooleanFromAnAlwaysSubmitClientNeverMintsAVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityWriteRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	// Nothing is determined for either capability.
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows before = %+v, want none", rows)
	}
	recorder.upserts = nil

	// The always-submit client's body: both booleans restated as the folded
	// DTO handed them over (false, because nothing is determined), plus the
	// edit the operator actually came for.
	no := false
	newCtxSize := 262144
	patched, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ContextSize: &newCtxSize, VisionCapable: &no, IsMTP: &no,
	})
	if err != nil {
		t.Fatalf("UpdateMapping (always-submit client): %v", err)
	}
	if patched.ContextSize != newCtxSize {
		t.Fatalf("context_size = %d, want %d (the edit the operator actually made)", patched.ContextSize, newCtxSize)
	}
	if len(recorder.upserts) != 0 {
		t.Fatalf("capability upserts = %+v, want NONE -- an unconditional false against no row is an unchanged submission, not a verdict", recorder.upserts)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the always-submit save = %+v, want STILL none", rows)
	}

	// The contrast, on the same fixture: the AUTHORITATIVE field states the
	// same negative and it DOES land. If the two paths are ever merged, one of
	// these two halves has to break.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: routing.CapabilityNo},
	}); err != nil {
		t.Fatalf("UpdateMapping (stated no): %v", err)
	}
	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityVision]
	if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row after a STATED no = %+v, want no/manual", row)
	}
}

// TestUpdateMappingResetIsNotSelfUndoneByTheNextSave is the defect the whole
// shape of this feature exists to prevent, and it is invisible to any test
// that only asserts "the delete was called".
//
// MappingForm seeds ONCE and never re-syncs from props. So a reset whose
// response still carried the PRE-delete verdict would leave the next render
// seeded with the verdict the operator just relinquished, and their next
// unrelated edit -- a context-size fix -- would re-submit it as a difference
// from unknown, which is an operator decision and writes a PERMANENT manual
// row. An ordinary 200, nothing on screen, and the capability is frozen
// against every probe and the benchmark again.
//
// Both saves here have their capability half DERIVED by the two helpers above
// from the DTO the previous call actually returned, so the second body is the
// one the real form would produce rather than one chosen to pass. The helpers
// themselves are not asserted on -- a test that checks its own helper against
// its own doc comment proves nothing about either side of the wire.
func TestUpdateMappingResetIsNotSelfUndoneByTheNextSave(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	// A probe determined vision. probedAt is deliberately not the service
	// clock's `now`, so any manual re-write would be visible in checked_at.
	probedAt := now.Add(-3 * time.Hour)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: probedAt},
	}); err != nil {
		t.Fatalf("seed probe row: %v", err)
	}

	seed, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	before := seed.Data[0]
	if got := seededVerdict(before, routing.CapabilityVision); got != routing.CapabilityYes {
		t.Fatalf("seeded vision verdict = %q, want %q -- the DTO must publish the probe's ROW", got, routing.CapabilityYes)
	}

	// SAVE 1: the operator moves vision to unknown. Everything else is
	// re-submitted exactly as the form was seeded.
	reset, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		GatewayModelName: &before.GatewayModelName, AppModelName: &before.AppModelName,
		Status: &before.Status, ContextSize: &before.ContextSize,
		CapabilityVerdicts: formCapabilityVerdicts(
			formControl{capability: routing.CapabilityVision, seed: seededVerdict(before, routing.CapabilityVision), chosen: ""},
		),
	})
	if err != nil {
		t.Fatalf("UpdateMapping (reset vision): %v", err)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the reset = %+v, want NONE (unknown is the absence of a row)", rows)
	}
	// The response must be post-delete truth: this is what the form re-seeds
	// from, and getting it wrong is what makes the reset self-undoing.
	if got := seededVerdict(reset, routing.CapabilityVision); got != "" {
		t.Fatalf("reset response seeds vision to %q, want \"\" (unknown) -- a stale DTO is what re-mints the manual row on the NEXT save", got)
	}
	if reset.VisionCapable {
		t.Fatalf("reset response vision_capable = true, want false")
	}

	// SAVE 2: an entirely unrelated edit -- only the context size moves. The
	// capability control is seeded from SAVE 1's response and the operator
	// leaves it alone, so whatever the form's own rule makes of that seed is
	// what goes on the wire -- derived, not hand-picked.
	//
	// The second save's capability WRITES are counted, not just the stored
	// rows: a row surviving absent is necessary but not sufficient, since a
	// manual row written and then read back looks like any other row.
	recorder.upserts = nil
	newCtxSize := 262144
	after, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		GatewayModelName: &reset.GatewayModelName, AppModelName: &reset.AppModelName,
		Status: &reset.Status, ContextSize: &newCtxSize,
		CapabilityVerdicts: formCapabilityVerdicts(
			formControl{capability: routing.CapabilityVision, seed: seededVerdict(reset, routing.CapabilityVision), chosen: ""},
		),
	})
	if err != nil {
		t.Fatalf("UpdateMapping (unrelated edit): %v", err)
	}
	if after.ContextSize != newCtxSize {
		t.Fatalf("context_size = %d, want %d (the edit the operator actually made)", after.ContextSize, newCtxSize)
	}
	if len(recorder.upserts) != 0 {
		t.Fatalf("capability upserts on the unrelated save = %+v, want NONE -- the reset must not be undone by the next edit", recorder.upserts)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the unrelated save = %+v, want STILL none", rows)
	}
	if after.VisionCapable || seededVerdict(after, routing.CapabilityVision) != "" {
		t.Fatalf("after the unrelated save the DTO reports vision %q/%v, want unknown/false", seededVerdict(after, routing.CapabilityVision), after.VisionCapable)
	}
}

// TestMappingDTOCapabilitiesDistinguishNoFromUnknown: without the DTO's
// capability ARRAY, a verdict of "no" and no row at all are the same `false`
// on the wire, and a form seeded from that boolean can only ever offer two
// states -- so it could neither SHOW unknown nor return a capability to it.
// This is the test that proves the array is actually used rather than merely
// present.
func TestMappingDTOCapabilitiesDistinguishNoFromUnknown(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	// A mapping whose vision verdict is a determined NO, plus a second one
	// with nothing determined at all.
	determined, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{GatewayModelName: "no-vision", AppModelName: "no-vision-up"})
	if err != nil {
		t.Fatalf("CreateMapping (determined): %v", err)
	}
	checkedAt := now.Add(-time.Hour)
	if err := routeStore.UpsertMappingCapabilities(ctx, determined.ID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: checkedAt},
	}); err != nil {
		t.Fatalf("seed no row: %v", err)
	}

	list, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	byID := map[string]ModelMappingDTO{}
	for _, row := range list.Data {
		byID[row.ID] = row
	}

	// The "no" mapping: same folded boolean as the unknown one, DIFFERENT row.
	no := byID[determined.ID]
	if no.VisionCapable {
		t.Fatalf("vision_capable for a determined \"no\" = true, want false")
	}
	if got := seededVerdict(no, routing.CapabilityVision); got != routing.CapabilityNo {
		t.Fatalf("capabilities entry for a determined \"no\" = %q, want %q -- the form must seed \"no\", not unknown", got, routing.CapabilityNo)
	}
	if len(no.Capabilities) != 1 {
		t.Fatalf("capabilities = %+v, want exactly the one determined row", no.Capabilities)
	}
	if row := no.Capabilities[0]; row.Source != routing.CapabilitySourceLlamaCppProps || !row.CheckedAt.Equal(checkedAt) {
		t.Fatalf("capability row = %+v, want source %q at %v (provenance is what the tooltip renders)", row, routing.CapabilitySourceLlamaCppProps, checkedAt)
	}

	// The nothing-determined mapping: the SAME false boolean, and no entry.
	unknown := byID[fx.mappingID]
	if unknown.VisionCapable {
		t.Fatalf("vision_capable for an undetermined capability = true, want false")
	}
	if got := seededVerdict(unknown, routing.CapabilityVision); got != "" {
		t.Fatalf("capabilities entry for an undetermined capability = %q, want \"\" (absent) -- absence IS unknown", got)
	}

	// `[]` on the wire, never `null`: the frontend must not need a
	// nil-vs-empty branch for a distinction that does not exist.
	encoded, err := json.Marshal(unknown)
	if err != nil {
		t.Fatalf("marshal DTO: %v", err)
	}
	if !strings.Contains(string(encoded), `"capabilities":[]`) {
		t.Fatalf("encoded DTO = %s, want \"capabilities\":[] (an empty ARRAY, never null)", encoded)
	}
}

// TestUpdateMappingMovingAVerdictIsNotResettingIt: moving a control from yes
// to NO is an operator verdict like any other -- it writes a manual "no" row,
// rank 3, just as permanent as the "yes" it replaced. Only an EMPTY verdict
// returns the capability to unknown. Asserted so no future reader mistakes one
// for the other and "simplifies" the reset away as a duplicate of the "no"
// direction.
func TestUpdateMappingMovingAVerdictIsNotResettingIt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)
	if err := routeStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("seed manual yes: %v", err)
	}

	// yes -> no, submitted the way the form would submit it.
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: formCapabilityVerdicts(
			formControl{capability: routing.CapabilityVision, seed: routing.CapabilityYes, chosen: routing.CapabilityNo},
		),
	})
	if err != nil {
		t.Fatalf("UpdateMapping (yes -> no): %v", err)
	}
	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, fx.mappingID))[routing.CapabilityVision]
	if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row after yes -> no = %+v, want no/manual -- moving to Nein is a VERDICT, not a clear", row)
	}
	if got := seededVerdict(dto, routing.CapabilityVision); got != routing.CapabilityNo {
		t.Fatalf("DTO seeds vision to %q after yes -> no, want %q (the row still exists)", got, routing.CapabilityNo)
	}

	// ...and only NOW, with an explicit empty verdict, does the row go away.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset): %v", err)
	}
	if rows := mustMappingCapabilities(t, routeStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the reset = %+v, want none", rows)
	}
}

// TestUpdateMappingCapabilityVerdictRejections covers the three 400s the STORE
// would never produce. DeleteMappingCapability neither trims nor validates,
// and an unknown mapping id, an unknown capability and an empty capability are
// all a benign nil on both drivers -- so a blank key would otherwise return
// 200 having done nothing, and a mistyped verdict would fall into the only
// sensible default branch, the reset, DELETING an established row nobody asked
// it to.
//
// All three are checked BEFORE anything is mutated, so a rejected request
// leaves the mapping's own fields untouched too.
func TestUpdateMappingCapabilityVerdictRejections(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityMTP, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLegacy, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	yes, no := true, false
	newCtxSize := 999999
	for _, tc := range []struct {
		name string
		req  UpdateMappingRequest
		want error
	}{
		{
			name: "empty name",
			req:  UpdateMappingRequest{ContextSize: &newCtxSize, CapabilityVerdicts: map[string]string{"": routing.CapabilityYes}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "whitespace-only name",
			req:  UpdateMappingRequest{ContextSize: &newCtxSize, CapabilityVerdicts: map[string]string{"   ": ""}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "an empty name beside a valid one still rejects the whole request",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityVision: "", "": ""}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			// A typo must not be read as a reset: that would DELETE the row.
			name: "a verdict that is neither yes, no nor empty",
			req:  UpdateMappingRequest{ContextSize: &newCtxSize, CapabilityVerdicts: map[string]string{routing.CapabilityVision: "true"}},
			want: ErrMappingCapabilityVerdictInvalid,
		},
		{
			name: "a verdict whose case is wrong",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityVision: "Yes"}},
			want: ErrMappingCapabilityVerdictInvalid,
		},
		{
			// The value is deliberately NOT trimmed, unlike the name: reading
			// " " as "hand this capability back to detection" would destroy a
			// verdict on a stray space.
			name: "a whitespace verdict is not an empty one",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityVision: " "}},
			want: ErrMappingCapabilityVerdictInvalid,
		},
		{
			name: "state a capability and send its legacy boolean (true)",
			req:  UpdateMappingRequest{VisionCapable: &yes, CapabilityVerdicts: map[string]string{routing.CapabilityVision: routing.CapabilityNo}},
			want: ErrMappingCapabilityConflict,
		},
		{
			name: "state a capability and send its legacy boolean (false)",
			req:  UpdateMappingRequest{IsMTP: &no, CapabilityVerdicts: map[string]string{routing.CapabilityMTP: ""}},
			want: ErrMappingCapabilityConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("UpdateMapping err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(recorder.deletes) != 0 {
		t.Fatalf("deletes on rejected requests = %+v, want NONE", recorder.deletes)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
	if rows[routing.CapabilityVision].Verdict != routing.CapabilityYes || rows[routing.CapabilityMTP].Verdict != routing.CapabilityYes {
		t.Fatalf("rows after rejected requests = %+v, want both untouched yes", rows)
	}
	// Validated BEFORE the mapping is mutated: the context size in the
	// rejected bodies must not have landed.
	list, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if got := list.Data[0].ContextSize; got != 4096 {
		t.Fatalf("context_size after rejected requests = %d, want the created 4096 -- validation must run before any mutation", got)
	}

	// The MIRROR of the conflict rule, so it cannot be read as "a stated
	// verdict and any boolean in one request is illegal": stating one
	// capability while sending a DIFFERENT one's boolean is a perfectly
	// coherent instruction and must work.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		IsMTP: &no, CapabilityVerdicts: map[string]string{routing.CapabilityVision: ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset vision, set mtp): %v", err)
	}
	rows = routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
	if _, ok := rows[routing.CapabilityVision]; ok {
		t.Fatalf("vision row = %+v, want deleted", rows[routing.CapabilityVision])
	}
	if got := rows[routing.CapabilityMTP]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("mtp row = %+v, want no/manual", got)
	}
}

// TestUpdateMappingCapabilityVerdictOpenVocabulary: the capability vocabulary
// is open on purpose -- an Ollama/agent-reported name gets its own row like any
// other -- so a stated verdict must accept ANY name, not just the six
// constants the code reasons about. Whitelisting them would leave exactly the
// rows nothing else can correct permanently stuck.
func TestUpdateMappingCapabilityVerdictOpenVocabulary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)
	const exotic = "structured_outputs" // no routing.Capability* constant
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: exotic, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// Trimmed, too: the name arrives from a wire body, not from a constant.
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{"  " + exotic + "  ": ""},
	})
	if err != nil {
		t.Fatalf("UpdateMapping (reset an open-vocabulary name): %v", err)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
	if _, ok := rows[exotic]; ok {
		t.Fatalf("%q row = %+v, want deleted", exotic, rows[exotic])
	}
	if rows[routing.CapabilityVision].Verdict != routing.CapabilityYes {
		t.Fatalf("vision row = %+v, want untouched yes -- only the NAMED capability is touched", rows[routing.CapabilityVision])
	}
	if got := seededVerdict(dto, exotic); got != "" {
		t.Fatalf("DTO still reports %q as %q, want absent", exotic, got)
	}

	// A VERDICT on an open-vocabulary name works the same way, in both
	// directions -- the write path is not narrower than the reset path.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{exotic: routing.CapabilityNo},
	}); err != nil {
		t.Fatalf("UpdateMapping (state an open-vocabulary no): %v", err)
	}
	if got := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[exotic]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("%q row = %+v, want no/manual", exotic, got)
	}

	// Resetting a row that is not there is a no-op, not an error: "unknown" is
	// the state, and it is already reached. Nothing reaches the store for it.
	deletesBefore := len(recorder.deletes)
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{"never_determined_at_all": ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset an absent row): %v", err)
	}
	if len(recorder.deletes) != deletesBefore {
		t.Fatalf("deletes = %+v, want no new one for an already-unknown capability", recorder.deletes)
	}
}

// TestUpdateMappingCapabilityVerdictAuthorization: a mapping the caller may not
// see must behave EXACTLY like a missing one -- and, since the store cannot
// tell the two apart (every failing shape is a benign nil), the delete must
// never be reached at all. authorizeMapping is the only thing standing between
// a `gateway:use` token and blanking another server's verdicts.
func TestUpdateMappingCapabilityVerdictAuthorization(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	req := UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityVision: ""}}
	if _, err := fx.svc.UpdateMapping(ctx, otherToken(), fx.mappingID, req); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("non-owner reset err = %v, want ErrMappingNotFound (nothing may leak, existence included)", err)
	}
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), "map_ghost", req); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("ghost-mapping reset err = %v, want ErrMappingNotFound", err)
	}
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), "  ", req); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("blank-id reset err = %v, want ErrMappingNotFound", err)
	}
	if len(recorder.deletes) != 0 {
		t.Fatalf("deletes reached the store on refused requests = %+v, want NONE", recorder.deletes)
	}
	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityVision]
	if row.Verdict != routing.CapabilityYes || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row after refused requests = %+v, want untouched yes/manual", row)
	}
	// The owner still gets through, so the test above cannot pass by refusing
	// everyone.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, req); err != nil {
		t.Fatalf("owner reset: %v", err)
	}
	if len(recorder.deletes) != 1 {
		t.Fatalf("owner deletes = %+v, want exactly 1", recorder.deletes)
	}
}

// TestUpdateMappingCapabilityResetPropagatesTheStoreError pins the deliberate
// ASYMMETRY between UpdateMapping's two capability writers, in both
// directions. The upsert is best-effort because the mapping update it
// accompanies has already landed -- the request's primary effect is real. A
// delete has no such effect to salvage: relinquishing the verdict IS the whole
// point of the action, so swallowing the failure would report success for
// nothing and the operator would walk away believing a permanent manual row
// was gone when it still stands.
func TestUpdateMappingCapabilityResetPropagatesTheStoreError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	deleteErr := errors.New("capability table unavailable")
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore(), failWith: deleteErr}
	fx := newCapabilityTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: ""},
	}); !errors.Is(err, deleteErr) {
		t.Fatalf("UpdateMapping err = %v, want the delete's own error %v -- a swallowed delete reports success for nothing", err, deleteErr)
	}

	// The other half of the asymmetry, restated here so the two sit together:
	// a failing UPSERT still succeeds, and the DTO it returns does not claim
	// the verdict the store refused to hold.
	upsertErr := errors.New("upsert unavailable")
	best := &failingCapabilityUpsertStore{MemoryStore: routing.NewMemoryStore(), err: upsertErr}
	bestFx := newCapabilityTestFixture(t, now, best)
	dto, err := bestFx.svc.UpdateMapping(ctx, ownerToken(), bestFx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: routing.CapabilityYes},
	})
	if err != nil {
		t.Fatalf("UpdateMapping with a failing capability upsert = %v, want nil (best-effort: the mapping update already landed)", err)
	}
	if dto.VisionCapable || seededVerdict(dto, routing.CapabilityVision) != "" {
		t.Fatalf("DTO after a failed upsert reports vision %q/%v, want unknown/false -- the response must never claim a verdict the store does not hold", seededVerdict(dto, routing.CapabilityVision), dto.VisionCapable)
	}
}

// TestCreateMappingStatesANegativeCapabilityVerdict: the create path has to
// accept a stated NO for the same reason the update path does -- a `manual`
// "no" is what stops a later probe from writing "yes" on a capability an
// operator has already judged. The legacy CreateMappingRequest booleans cannot
// express it at all: they are plain bools, so an unset `false` is
// indistinguishable from a control nobody looked at and only `true` writes.
//
// It also pins the MTP name heuristic's place in that order. The heuristic
// writes a `legacy` "yes" row for an MTP-shaped model name, and it must not do
// that over an operator's own statement -- a create that answered 200 with
// `mtp: yes/legacy` after being told "no" would be the same class of lie as an
// update that discards the verdict.
func TestCreateMappingStatesANegativeCapabilityVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	created, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "stated-no", AppModelName: "stated-no-up",
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: routing.CapabilityNo},
	})
	if err != nil {
		t.Fatalf("CreateMapping (stated no): %v", err)
	}
	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, created.ID))[routing.CapabilityVision]
	if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual || !row.CheckedAt.Equal(now) {
		t.Fatalf("vision row after a created \"no\" = %+v, want no/manual@%v", row, now)
	}
	if got := seededVerdict(created, routing.CapabilityVision); got != routing.CapabilityNo {
		t.Fatalf("create response seeds vision to %q, want %q", got, routing.CapabilityNo)
	}
	if created.VisionCapable {
		t.Fatalf("created vision_capable = true, want false")
	}

	// An MTP-SHAPED model name, where the heuristic would otherwise write
	// legacy/yes, plus an operator saying "no".
	const mtpName = "glm-4.5-air-mtp"
	if !routing.IsMTPModelName(mtpName) {
		t.Fatalf("fixture model name %q is not MTP-shaped -- this subtest would prove nothing", mtpName)
	}
	overridden, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "stated-mtp-no", AppModelName: mtpName,
		CapabilityVerdicts: map[string]string{routing.CapabilityMTP: routing.CapabilityNo},
	})
	if err != nil {
		t.Fatalf("CreateMapping (stated mtp no): %v", err)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, overridden.ID))
	if len(rows) != 1 {
		t.Fatalf("capability rows = %+v, want exactly one mtp row (the heuristic must not add a second)", rows)
	}
	if got := rows[routing.CapabilityMTP]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("mtp row = %+v, want no/manual -- the NAME heuristic must not talk over the operator", got)
	}
	if overridden.IsMtp {
		t.Fatalf("created is_mtp = true, want false")
	}

	// The heuristic is untouched where nothing was stated, so this test cannot
	// pass by having disabled it.
	plain, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "plain-mtp", AppModelName: mtpName,
	})
	if err != nil {
		t.Fatalf("CreateMapping (no statement): %v", err)
	}
	if got := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, plain.ID))[routing.CapabilityMTP]; got.Verdict != routing.CapabilityYes || got.Source != routing.CapabilitySourceLegacy {
		t.Fatalf("mtp row for an unstated MTP-shaped name = %+v, want yes/legacy (the heuristic still runs)", got)
	}

	// The create path validates the map with the same rules, BEFORE it writes
	// anything: a rejected create leaves no mapping behind.
	before, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	for _, tc := range []struct {
		name string
		req  CreateMappingRequest
		want error
	}{
		{
			name: "blank capability name",
			req:  CreateMappingRequest{GatewayModelName: "rejected-1", AppModelName: "rejected-1-up", CapabilityVerdicts: map[string]string{" ": routing.CapabilityNo}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "invalid verdict value",
			req:  CreateMappingRequest{GatewayModelName: "rejected-2", AppModelName: "rejected-2-up", CapabilityVerdicts: map[string]string{routing.CapabilityVision: "false"}},
			want: ErrMappingCapabilityVerdictInvalid,
		},
		{
			// Two keys naming one capability: refused here too, since both
			// paths share normalizeCapabilityVerdicts.
			name: "two keys that trim to the same capability",
			req:  CreateMappingRequest{GatewayModelName: "rejected-3", AppModelName: "rejected-3-up", CapabilityVerdicts: map[string]string{routing.CapabilityMTP: routing.CapabilityNo, "mtp ": routing.CapabilityYes}},
			want: ErrMappingCapabilityDuplicate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("CreateMapping err = %v, want %v", err, tc.want)
			}
		})
	}
	after, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if len(after.Data) != len(before.Data) {
		t.Fatalf("mappings after the rejected creates = %d, want the %d there were -- validation must run before the store write", len(after.Data), len(before.Data))
	}
}

// TestCreateMappingStatedVerdictBeatsTheLegacyVisionBoolean pins the first of
// the create path's three documented precedence rules: a capability the
// caller STATES is settled, so req.VisionCapable must not add a second row
// for it.
//
// Both writers land in the same capRows slice and the same single upsert, and
// the store's upsert is last-write-wins per capability -- so without the
// `!stated[vision]` guard a create that was told "no" answers 200 with a
// `manual` "yes", which outranks every probe and the vision benchmark for as
// long as it stands. The rule is stated at the request field, at the call
// site and in api-surface.md; until now nothing failed when it was removed.
func TestCreateMappingStatedVerdictBeatsTheLegacyVisionBoolean(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	created, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "stated-over-vision-bool", AppModelName: "stated-over-vision-bool-up",
		VisionCapable:      true,
		CapabilityVerdicts: map[string]string{routing.CapabilityVision: routing.CapabilityNo},
	})
	if err != nil {
		t.Fatalf("CreateMapping (stated no beside vision_capable true): %v", err)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, created.ID))
	if len(rows) != 1 {
		t.Fatalf("capability rows = %+v, want exactly the one stated vision row", rows)
	}
	if got := rows[routing.CapabilityVision]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row = %+v, want no/manual -- the legacy BOOLEAN must not talk over the operator's own statement", got)
	}
	if created.VisionCapable || seededVerdict(created, routing.CapabilityVision) != routing.CapabilityNo {
		t.Fatalf("create response reports vision %q/%v, want no/false -- the response must not claim a verdict the caller declined", seededVerdict(created, routing.CapabilityVision), created.VisionCapable)
	}
}

// TestCreateMappingStatedVerdictBeatsTheLegacyMTPBoolean pins the same rule
// for the OTHER legacy boolean, where it is carried by the ORDER of the
// switch arms rather than by a guard: `case stated[mtp]` comes before
// `case req.IsMTP`, and swapping the two lets the boolean append a second
// `manual` "yes" after the stated "no" -- last-write-wins again.
//
// The model name here is deliberately NOT MTP-shaped, asserted below, so the
// only two writers in play are the statement and the boolean: the name
// heuristic (pinned separately) cannot be what supplies the row.
func TestCreateMappingStatedVerdictBeatsTheLegacyMTPBoolean(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	const plainName = "qwen3-32b-instruct"
	if routing.IsMTPModelName(plainName) {
		t.Fatalf("fixture model name %q is MTP-shaped -- this test would not isolate the BOOLEAN's precedence", plainName)
	}
	created, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "stated-over-mtp-bool", AppModelName: plainName,
		IsMTP:              true,
		CapabilityVerdicts: map[string]string{routing.CapabilityMTP: routing.CapabilityNo},
	})
	if err != nil {
		t.Fatalf("CreateMapping (stated no beside is_mtp true): %v", err)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, created.ID))
	if len(rows) != 1 {
		t.Fatalf("capability rows = %+v, want exactly the one stated mtp row", rows)
	}
	if got := rows[routing.CapabilityMTP]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("mtp row = %+v, want no/manual -- the legacy BOOLEAN must not talk over the operator's own statement", got)
	}
	if created.IsMtp || seededVerdict(created, routing.CapabilityMTP) != routing.CapabilityNo {
		t.Fatalf("create response reports mtp %q/%v, want no/false", seededVerdict(created, routing.CapabilityMTP), created.IsMtp)
	}
}

// TestCreateMappingStatedUnknownSuppressesTheMTPNameHeuristic pins the third
// rule, and it is the one that most needed pinning: a PRESENT key holding ""
// is a caller saying "I am telling you about mtp: nothing is determined", so
// it settles the capability even though it is not a verdict. The name
// heuristic must not answer that with a `legacy` "yes".
//
// `stated` is therefore recorded for every entry, BEFORE the empty-verdict
// `continue` -- record it only for a non-empty verdict and this create
// answers 200 with `mtp: yes/legacy`, a verdict the caller explicitly
// declined to make, on the one capability nothing ever re-probes.
//
// The contrast in the same body is what keeps the assertion honest: the very
// same MTP-shaped name still gets the heuristic's row where nothing was
// stated, so this test cannot pass by having disabled the heuristic.
func TestCreateMappingStatedUnknownSuppressesTheMTPNameHeuristic(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newCapabilityTestFixture(t, now, routeStore)

	const mtpName = "glm-4.5-air-mtp"
	if !routing.IsMTPModelName(mtpName) {
		t.Fatalf("fixture model name %q is not MTP-shaped -- this test would prove nothing", mtpName)
	}
	stated, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "stated-unknown-mtp", AppModelName: mtpName,
		CapabilityVerdicts: map[string]string{routing.CapabilityMTP: ""},
	})
	if err != nil {
		t.Fatalf("CreateMapping (stated unknown mtp): %v", err)
	}
	if rows := mustMappingCapabilities(t, routeStore, stated.ID); len(rows) != 0 {
		t.Fatalf("capability rows after a stated unknown = %+v, want NONE -- unknown is the ABSENCE of a row, and a legacy/yes row is not absence", rows)
	}
	if stated.IsMtp || len(stated.Capabilities) != 0 {
		t.Fatalf("create response reports is_mtp %v with capabilities %+v, want false/none", stated.IsMtp, stated.Capabilities)
	}

	unstated, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName: "unstated-mtp", AppModelName: mtpName,
	})
	if err != nil {
		t.Fatalf("CreateMapping (nothing stated): %v", err)
	}
	if got := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, unstated.ID))[routing.CapabilityMTP]; got.Verdict != routing.CapabilityYes || got.Source != routing.CapabilitySourceLegacy {
		t.Fatalf("mtp row for the SAME name with nothing stated = %+v, want yes/legacy -- the heuristic must still run", got)
	}
}

// TestUpdateMappingTwoKeysForOneCapabilityAreRejected: a map holding two keys
// that TRIM to the same capability is two different instructions about one
// row, and it is refused rather than resolved.
//
// Resolving it was never really an option, because there is no rule to
// resolve it BY. Both intents survive normalization, both compare equal in
// its sort (sort.Slice is not stable), and the one upsert they reach is
// last-write-wins -- so the verdict that landed followed Go's map iteration
// order, measured at "no" 7 / "yes" 33 over 40 identical requests, inside the
// function whose own doc comment promises a deterministic order.
//
// The pin is that the REQUEST is refused (without the check it answers 200,
// so this assertion is not itself a coin flip) and that it wrote NOTHING: no
// upsert, no delete, no row, and not even the unrelated context_size the same
// body carried. The two contrasts below are what keep the check honest -- a
// single padded key is still perfectly good input, and an exactly-duplicate
// JSON key is a different thing entirely, collapsed by encoding/json to one
// entry long before this code runs.
func TestUpdateMappingTwoKeysForOneCapabilityAreRejected(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	newCtxSize := 999999
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ContextSize: &newCtxSize,
		CapabilityVerdicts: map[string]string{
			routing.CapabilityVision:       routing.CapabilityNo,
			" " + routing.CapabilityVision: routing.CapabilityYes,
		},
	}); !errors.Is(err, ErrMappingCapabilityDuplicate) {
		t.Fatalf("UpdateMapping err = %v, want %v", err, ErrMappingCapabilityDuplicate)
	}
	if len(recorder.upserts) != 0 || len(recorder.deletes) != 0 {
		t.Fatalf("capability writes on the rejected request = upserts %+v / deletes %+v, want NONE -- the collision is detected before anything is mutated", recorder.upserts, recorder.deletes)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the rejected request = %+v, want NONE", rows)
	}
	list, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if got := list.Data[0].ContextSize; got != 4096 {
		t.Fatalf("context_size after the rejected request = %d, want the created 4096 -- validation must run before any mutation", got)
	}

	// A single PADDED key is not a collision and must still work: the name
	// arrives from an open vocabulary an upstream may pad, which is why it is
	// trimmed at all.
	single, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{"  " + routing.CapabilityVision + "  ": routing.CapabilityNo},
	})
	if err != nil {
		t.Fatalf("UpdateMapping (one padded key): %v", err)
	}
	if got := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityVision]; got.Verdict != routing.CapabilityNo || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row after one padded key = %+v, want no/manual", got)
	}
	if seededVerdict(single, routing.CapabilityVision) != routing.CapabilityNo {
		t.Fatalf("response seeds vision to %q, want %q", seededVerdict(single, routing.CapabilityVision), routing.CapabilityNo)
	}

	// An EXACTLY duplicate JSON key never reaches the check: the decoder
	// collapses it to one map entry, last value wins. Decoded here rather
	// than hand-built, because that collapse is the whole point.
	var decoded UpdateMappingRequest
	if err := json.Unmarshal([]byte(`{"capability_verdicts":{"mtp":"no","mtp":"yes"}}`), &decoded); err != nil {
		t.Fatalf("unmarshal a body with a repeated JSON key: %v", err)
	}
	if len(decoded.CapabilityVerdicts) != 1 {
		t.Fatalf("decoded capability_verdicts = %+v, want ONE entry -- encoding/json collapses a repeated key", decoded.CapabilityVerdicts)
	}
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, decoded); err != nil {
		t.Fatalf("UpdateMapping (repeated JSON key, decoded): %v", err)
	}
	if got := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityMTP]; got.Verdict != routing.CapabilityYes || got.Source != routing.CapabilitySourceManual {
		t.Fatalf("mtp row after the decoded repeated key = %+v, want yes/manual (the decoder's surviving value)", got)
	}
}
