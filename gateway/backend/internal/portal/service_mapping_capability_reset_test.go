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
// The recording is the instrument for the AUTHORISATION tests: the store gives
// this path no existence signal at all (an unknown mapping id, an unknown
// capability and an empty capability are each a benign nil on both drivers),
// so "the caller was refused" and "the caller was allowed and deleted nothing"
// are indistinguishable from the store's contents. Only a call count separates
// them.
type capabilityResetRecorder struct {
	*routing.MemoryStore
	deletes []capabilityDelete
	// failWith, when non-nil, is returned INSTEAD of performing the delete.
	failWith error
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

// resetTestFixture is the shared setup: a server, an application and one
// mapping, over a route store the caller supplies (usually a recorder).
type resetTestFixture struct {
	svc       *Service
	mappingID string
	appID     string
}

func newResetTestFixture(t *testing.T, now time.Time, routes routing.Store) resetTestFixture {
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
	return resetTestFixture{svc: svc, mappingID: mapping.ID, appID: app.ID}
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

// formCapabilityFields is MappingForm's SUBMIT rule, likewise restated: send
// the boolean only for a yes/no the operator moved to, name the capability in
// reset_capabilities only when they moved it TO unknown from something else,
// and send NEITHER when the value is unchanged. Never both for one capability.
func formCapabilityFields(seeded, chosen string) (boolean *bool, reset bool) {
	if chosen == seeded {
		return nil, false
	}
	if chosen == "" {
		return nil, true
	}
	yes := chosen == routing.CapabilityYes
	return &yes, false
}

// TestUpdateMappingResetIsNotSelfUndoneByTheNextSave is the defect the whole
// shape of this feature exists to prevent, and it is invisible to any test
// that only asserts "the delete was called".
//
// MappingForm seeds ONCE and never re-syncs from props. So a reset whose
// response still carried the PRE-delete verdict would leave the next render
// seeded with the verdict the operator just relinquished, and their next
// unrelated edit -- a context-size fix -- would re-submit it, where
// UpdateMapping's differs-from-stored rule reads it as an operator decision
// and writes a PERMANENT manual row. An ordinary 200, nothing on screen, and
// the capability is frozen against every probe and the benchmark again.
//
// Both saves here are built by the two helpers above from the DTO the previous
// call actually returned, so the second body is the one the real form would
// produce -- not one chosen to pass.
func TestUpdateMappingResetIsNotSelfUndoneByTheNextSave(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	upserts := &capabilityWriteRecorder{MemoryStore: recorder.MemoryStore}
	fx := newResetTestFixture(t, now, recorder)

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
	visionBool, visionReset := formCapabilityFields(seededVerdict(before, routing.CapabilityVision), "")
	if visionBool != nil || !visionReset {
		t.Fatalf("form fields for yes -> unknown = (%v, %v), want (nil, true)", visionBool, visionReset)
	}
	reset, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		GatewayModelName: &before.GatewayModelName, AppModelName: &before.AppModelName,
		Status: &before.Status, ContextSize: &before.ContextSize,
		ResetCapabilities: []string{routing.CapabilityVision},
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
	// capability controls are seeded from SAVE 1's response, so an unchanged
	// unknown must send NEITHER field.
	visionBool, visionReset = formCapabilityFields(seededVerdict(reset, routing.CapabilityVision), "")
	if visionBool != nil || visionReset {
		t.Fatalf("form fields for unknown -> unknown = (%v, %v), want (nil, false) -- an unchanged control sends nothing", visionBool, visionReset)
	}
	// Re-wire the service so the second save's capability WRITES are counted:
	// the stored row surviving absent is necessary but not sufficient, since a
	// manual row written and then read back looks like any other row.
	svc2 := newServerTestServiceWithRoutes(t, now, upserts)
	newCtxSize := 262144
	after, err := svc2.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		GatewayModelName: &reset.GatewayModelName, AppModelName: &reset.AppModelName,
		Status: &reset.Status, ContextSize: &newCtxSize,
	})
	if err != nil {
		t.Fatalf("UpdateMapping (unrelated edit): %v", err)
	}
	if after.ContextSize != newCtxSize {
		t.Fatalf("context_size = %d, want %d (the edit the operator actually made)", after.ContextSize, newCtxSize)
	}
	if len(upserts.upserts) != 0 {
		t.Fatalf("capability upserts on the unrelated save = %+v, want NONE -- the reset must not be undone by the next edit", upserts.upserts)
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
	fx := newResetTestFixture(t, now, routeStore)

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
// rank 3, just as permanent as the "yes" it replaced. Only an explicit
// reset_capabilities entry returns the capability to unknown. Asserted so no
// future reader mistakes one for the other and "simplifies" the reset away as
// a duplicate of the false direction.
func TestUpdateMappingMovingAVerdictIsNotResettingIt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newResetTestFixture(t, now, routeStore)
	if err := routeStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("seed manual yes: %v", err)
	}

	// yes -> no, submitted the way the form would submit it.
	boolean, reset := formCapabilityFields(routing.CapabilityYes, routing.CapabilityNo)
	if boolean == nil || *boolean || reset {
		t.Fatalf("form fields for yes -> no = (%v, %v), want (&false, false)", boolean, reset)
	}
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{VisionCapable: boolean})
	if err != nil {
		t.Fatalf("UpdateMapping (yes -> no): %v", err)
	}
	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, fx.mappingID))[routing.CapabilityVision]
	if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("vision row after yes -> no = %+v, want no/manual -- unchecking is a VERDICT, not a clear", row)
	}
	if got := seededVerdict(dto, routing.CapabilityVision); got != routing.CapabilityNo {
		t.Fatalf("DTO seeds vision to %q after yes -> no, want %q (the row still exists)", got, routing.CapabilityNo)
	}

	// ...and only NOW, with an explicit reset, does the row go away.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ResetCapabilities: []string{routing.CapabilityVision},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset): %v", err)
	}
	if rows := mustMappingCapabilities(t, routeStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("capability rows after the reset = %+v, want none", rows)
	}
}

// TestUpdateMappingResetCapabilityRejections covers the two 400s the STORE
// would never produce: DeleteMappingCapability neither trims nor validates,
// and an unknown mapping id, an unknown capability and an empty capability are
// all a benign nil on both drivers -- so each of these would otherwise return
// 200 having done nothing (or, for the contradiction, having done something
// nobody asked for).
//
// Both are checked BEFORE anything is mutated, so a rejected request leaves
// the mapping's own fields untouched too.
func TestUpdateMappingResetCapabilityRejections(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newResetTestFixture(t, now, recorder)
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
			req:  UpdateMappingRequest{ContextSize: &newCtxSize, ResetCapabilities: []string{""}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "whitespace-only name",
			req:  UpdateMappingRequest{ContextSize: &newCtxSize, ResetCapabilities: []string{"   "}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "an empty name beside a valid one still rejects the whole request",
			req:  UpdateMappingRequest{ResetCapabilities: []string{routing.CapabilityVision, ""}},
			want: ErrMappingCapabilityNameRequired,
		},
		{
			name: "reset and set the same capability (true)",
			req:  UpdateMappingRequest{VisionCapable: &yes, ResetCapabilities: []string{routing.CapabilityVision}},
			want: ErrMappingCapabilityConflict,
		},
		{
			name: "reset and set the same capability (false)",
			req:  UpdateMappingRequest{IsMTP: &no, ResetCapabilities: []string{routing.CapabilityMTP}},
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
	// Validated BEFORE the mapping is mutated: the context size in two of the
	// rejected bodies must not have landed.
	list, err := fx.svc.ListMappings(ctx, ownerToken(), fx.appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if got := list.Data[0].ContextSize; got != 4096 {
		t.Fatalf("context_size after rejected requests = %d, want the created 4096 -- validation must run before any mutation", got)
	}

	// The MIRROR of the contradiction rule, so it cannot be read as "a reset
	// and any boolean in one request is illegal": resetting one capability
	// while SETTING a different one is a perfectly coherent instruction and
	// must work.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		IsMTP: &no, ResetCapabilities: []string{routing.CapabilityVision},
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

// TestUpdateMappingResetCapabilityOpenVocabulary: the capability vocabulary is
// open on purpose -- an Ollama/agent-reported name gets its own row like any
// other -- so a reset must accept ANY name, not just the six constants the
// code reasons about. Whitelisting them would leave exactly the rows nothing
// else can correct permanently stuck.
func TestUpdateMappingResetCapabilityOpenVocabulary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	fx := newResetTestFixture(t, now, routeStore)
	const exotic = "structured_outputs" // no routing.Capability* constant
	if err := routeStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: exotic, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// Trimmed, too: the name arrives from a wire body, not from a constant.
	dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ResetCapabilities: []string{"  " + exotic + "  "},
	})
	if err != nil {
		t.Fatalf("UpdateMapping (reset an open-vocabulary name): %v", err)
	}
	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, routeStore, fx.mappingID))
	if _, ok := rows[exotic]; ok {
		t.Fatalf("%q row = %+v, want deleted", exotic, rows[exotic])
	}
	if rows[routing.CapabilityVision].Verdict != routing.CapabilityYes {
		t.Fatalf("vision row = %+v, want untouched yes -- only the NAMED capability is reset", rows[routing.CapabilityVision])
	}
	if got := seededVerdict(dto, exotic); got != "" {
		t.Fatalf("DTO still reports %q as %q, want absent", exotic, got)
	}

	// Deleting a row that is not there is a no-op, not an error: "unknown" is
	// the state, and it is already reached.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ResetCapabilities: []string{exotic, "never_determined_at_all"},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset absent rows): %v", err)
	}
}

// TestUpdateMappingResetCapabilityAuthorization: a mapping the caller may not
// see must behave EXACTLY like a missing one -- and, since the store cannot
// tell the two apart (every failing shape is a benign nil), the delete must
// never be reached at all. authorizeMapping is the only thing standing between
// a `gateway:use` token and blanking another server's verdicts.
func TestUpdateMappingResetCapabilityAuthorization(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newResetTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	req := UpdateMappingRequest{ResetCapabilities: []string{routing.CapabilityVision}}
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

// TestUpdateMappingResetCapabilityPropagatesTheStoreError pins the deliberate
// ASYMMETRY between UpdateMapping's two capability writers, in both
// directions. The upsert is best-effort because the mapping update it
// accompanies has already landed -- the request's primary effect is real. A
// delete has no such effect to salvage: relinquishing the verdict IS the whole
// point of the action, so swallowing the failure would report success for
// nothing and the operator would walk away believing a permanent manual row
// was gone when it still stands.
func TestUpdateMappingResetCapabilityPropagatesTheStoreError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	deleteErr := errors.New("capability table unavailable")
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore(), failWith: deleteErr}
	fx := newResetTestFixture(t, now, recorder)
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		ResetCapabilities: []string{routing.CapabilityVision},
	}); !errors.Is(err, deleteErr) {
		t.Fatalf("UpdateMapping err = %v, want the delete's own error %v -- a swallowed delete reports success for nothing", err, deleteErr)
	}

	// The other half of the asymmetry, restated here so the two sit together:
	// a failing UPSERT still succeeds, and the DTO it returns does not claim
	// the verdict the store refused to hold.
	upsertErr := errors.New("upsert unavailable")
	best := &failingCapabilityUpsertStore{MemoryStore: routing.NewMemoryStore(), err: upsertErr}
	bestFx := newResetTestFixture(t, now, best)
	yes := true
	dto, err := bestFx.svc.UpdateMapping(ctx, ownerToken(), bestFx.mappingID, UpdateMappingRequest{VisionCapable: &yes})
	if err != nil {
		t.Fatalf("UpdateMapping with a failing capability upsert = %v, want nil (best-effort: the mapping update already landed)", err)
	}
	if dto.VisionCapable || seededVerdict(dto, routing.CapabilityVision) != "" {
		t.Fatalf("DTO after a failed upsert reports vision %q/%v, want unknown/false -- the response must never claim a verdict the store does not hold", seededVerdict(dto, routing.CapabilityVision), dto.VisionCapable)
	}
}
