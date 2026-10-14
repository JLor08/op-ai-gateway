// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// editRows builds a mapping's capability rows from (capability, verdict)
// pairs; "" as a verdict omits the row, which is how "unknown" is stored.
func editRows(t *testing.T, now time.Time, image, imageEdit string) []CapabilityRow {
	t.Helper()
	var rows []CapabilityRow
	if image != "" {
		rows = append(rows, CapabilityRow{Capability: CapabilityImage, Verdict: image, Source: CapabilitySourceManual, CheckedAt: now})
	}
	if imageEdit != "" {
		rows = append(rows, CapabilityRow{Capability: CapabilityImageEdit, Verdict: imageEdit, Source: CapabilitySourceManual, CheckedAt: now})
	}
	return rows
}

// ImpliedImageEdit is THE derivation: image:yes AND NOT image_edit:no. It is
// asserted over every combination of the two rows, because each cell is a
// distinct decision and two of them are the deliberate exceptions to this
// gateway's "unknown refuses" rule:
//
//   - image:yes with NO image_edit row is capable. Absence of the opt-out row
//     means "derive from image", NOT "unknown" (ADR-042's refuse-on-unknown
//     does not apply to a derived capability).
//   - image_edit:yes is INERT. The gate derives the capability, so an operator
//     "yes" adds nothing -- in particular it cannot make a model that does not
//     generate images an edit target.
//
// The opt-out only removes edit: image generation is a different capability.
func TestImpliedImageEditTruthTable(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, image, imageEdit string
		want                   bool
	}{
		{"image yes, no edit row: derived capable", CapabilityYes, "", true},
		{"image yes, edit no: the operator opt-out", CapabilityYes, CapabilityNo, false},
		{"image yes, edit yes: inert, still capable", CapabilityYes, CapabilityYes, true},
		{"no rows at all: nothing to derive from", "", "", false},
		{"image no: a non-generator never edits", CapabilityNo, "", false},
		{"image no, edit yes: yes is inert, cannot grant", CapabilityNo, CapabilityYes, false},
		{"edit yes alone: yes is inert, cannot grant", "", CapabilityYes, false},
		{"edit no alone: still false", "", CapabilityNo, false},
		{"image no, edit no", CapabilityNo, CapabilityNo, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImpliedImageEdit(editRows(t, now, tc.image, tc.imageEdit)); got != tc.want {
				t.Fatalf("ImpliedImageEdit(image=%q, image_edit=%q) = %v, want %v", tc.image, tc.imageEdit, got, tc.want)
			}
		})
	}
	if ImpliedImageEdit(nil) {
		t.Fatal("ImpliedImageEdit(nil) = true, want false -- a failed capability read folds to no rows and must refuse")
	}
}

// capabilityRowsSatisfy is where the gate consumes the derivation. The
// image_edit requirement is answered by ImpliedImageEdit; every other required
// name is still a literal "yes" row, so image generation, vision and the open
// vocabulary are untouched by it. Both directions of the opt-out are pinned:
// it removes edit and ONLY edit.
func TestCapabilityRowsSatisfyDerivesImageEdit(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, image, imageEdit string
		required               []string
		want                   bool
	}{
		{"edit is derived from image:yes", CapabilityYes, "", []string{CapabilityImageEdit}, true},
		{"an operator image_edit:no removes it", CapabilityYes, CapabilityNo, []string{CapabilityImageEdit}, false},
		{"the opt-out does not touch generation", CapabilityYes, CapabilityNo, []string{CapabilityImage}, true},
		{"an image_edit:yes row alone is not a grant", "", CapabilityYes, []string{CapabilityImageEdit}, false},
		{"no image verdict, no edit", "", "", []string{CapabilityImageEdit}, false},
		{"image no, no edit", CapabilityNo, "", []string{CapabilityImageEdit}, false},
		{"both required, both derived", CapabilityYes, "", []string{CapabilityImage, CapabilityImageEdit}, true},
		{"both required, opted out of edit", CapabilityYes, CapabilityNo, []string{CapabilityImage, CapabilityImageEdit}, false},
		{"nothing required is satisfied by anything", "", "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilityRowsSatisfy(editRows(t, now, tc.image, tc.imageEdit), tc.required); got != tc.want {
				t.Fatalf("capabilityRowsSatisfy(image=%q, image_edit=%q, %v) = %v, want %v", tc.image, tc.imageEdit, tc.required, got, tc.want)
			}
		})
	}
	// A different, non-derived name is still literal: image:yes must not leak
	// into any capability but image_edit.
	if capabilityRowsSatisfy(editRows(t, now, CapabilityYes, ""), []string{CapabilityVision}) {
		t.Fatal("image:yes satisfied a vision requirement -- the derivation must be special-cased to image_edit alone")
	}
}

// imageEditReq is an image-edit request: the endpoint's identity, expressed as
// the required-capability list the handler sets.
func imageEditReq(model string) inference.Request {
	return inference.Request{
		Model:                model,
		APIFlavor:            "openai_images",
		RequiredCapabilities: []string{CapabilityImageEdit},
	}
}

// imageEditFixture seeds the package's two-member store with the given image
// and image_edit rows on map_a, and a plain image:yes on map_b.
func imageEditFixture(t *testing.T, now time.Time, imageA, imageEditA string) *capabilityGateStore {
	t.Helper()
	mem := seededGroupStore(t, now)
	if rows := editRows(t, now, imageA, imageEditA); len(rows) > 0 {
		if err := mem.UpsertMappingCapabilities(context.Background(), "map_a", rows); err != nil {
			t.Fatalf("seed map_a: %v", err)
		}
	}
	if err := mem.UpsertMappingCapabilities(context.Background(), "map_b", editRows(t, now, CapabilityYes, "")); err != nil {
		t.Fatalf("seed map_b: %v", err)
	}
	return &capabilityGateStore{resolverStore: mem}
}

// The gate end to end: filterCapable keeps a candidate whose mapping derives
// edit and drops one whose operator opted out, whatever else it carries.
func TestFilterCapableAppliesTheImageEditDerivation(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, imageA, imageEditA string
		wantA                    bool
	}{
		{"image:yes derives edit for map_a", CapabilityYes, "", true},
		{"image:yes + image_edit:no drops map_a", CapabilityYes, CapabilityNo, false},
		{"no image row drops map_a (nothing to derive)", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := imageEditFixture(t, now, tc.imageA, tc.imageEditA)
			r := NewResolver(store, func() time.Time { return now }, nil)
			got, err := r.filterCapable(context.Background(), imageGateCandidates(), []string{CapabilityImageEdit})
			if err != nil {
				t.Fatalf("filterCapable: %v", err)
			}
			ids := map[string]bool{}
			for _, c := range got {
				ids[c.Mapping.ID] = true
			}
			if ids["map_a"] != tc.wantA || !ids["map_b"] {
				t.Fatalf("candidates = %v, want map_a kept=%v and map_b always kept (plain image:yes)", ids, tc.wantA)
			}
		})
	}
}

// A model offered by two servers where one opted out is SERVED by the gate (it
// is a per-candidate any-of), routed to the server that did not opt out. The
// portal's fold ANDs across mappings and advertises such a model false -- the
// safe direction; the invariant is "the composer never offers what the gate
// refuses", not "they agree exactly".
func TestResolveRoutesAnEditToTheMappingThatDidNotOptOut(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	// coder-a (map_a) and coder-b (map_b) are two different models in the
	// seeded store, so point both at one gateway name to model "one model on
	// two servers".
	for _, id := range []string{"map_a", "map_b"} {
		m, err := mem.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.GatewayModelName = "sd-shared"
		if err := mem.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	if err := mem.UpsertMappingCapabilities(ctx, "map_a", editRows(t, now, CapabilityYes, CapabilityNo)); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	if err := mem.UpsertMappingCapabilities(ctx, "map_b", editRows(t, now, CapabilityYes, "")); err != nil {
		t.Fatalf("seed map_b: %v", err)
	}
	r := NewResolver(mem, func() time.Time { return now }, nil)

	target, err := r.Resolve(ctx, auth.Token{}, imageEditReq("sd-shared"))
	if err != nil {
		t.Fatalf("Resolve: %v, want the mapping that did not opt out", err)
	}
	if target.ServerID != "srv_b" {
		t.Fatalf("target server = %q, want srv_b (srv_a opted out of edit)", target.ServerID)
	}
}

// When EVERY candidate opted out, the request is refused with the dedicated
// sentinel -- the model exists but may not edit -- not a bare "no route".
func TestResolveRefusesAnEditWhenEveryMappingOptedOut(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	if err := mem.UpsertMappingCapabilities(context.Background(), "map_a", editRows(t, now, CapabilityYes, CapabilityNo)); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	r := NewResolver(mem, func() time.Time { return now }, nil)

	_, err := r.Resolve(context.Background(), auth.Token{}, imageEditReq("coder-a"))
	if !errors.Is(err, ErrModelNotCapable) {
		t.Fatalf("err = %v, want ErrModelNotCapable for an opted-out model", err)
	}
	if errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v also matches ErrNoModelRoute -- the two facts must stay distinguishable", err)
	}
	// And the same mapping still SERVES generation: the opt-out removes edit only.
	if _, err := r.Resolve(context.Background(), auth.Token{}, imagesReq("coder-a")); err != nil {
		t.Fatalf("generation Resolve for an edit-opted-out model: %v, want it served", err)
	}
}

// The affinity branch gates off its own keyed read, which is the other caller
// of capabilityRowsSatisfy. It must use the same derivation, or an edit pinned
// to an opted-out mapping would be served by the pin while the candidate path
// refuses it.
func TestAffinityGateAppliesTheImageEditDerivation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, imageEdit string
		wantSatisfied   bool
	}{
		{"derived from image:yes", "", true},
		{"opted out", CapabilityNo, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := seededGroupStore(t, now)
			if err := mem.UpsertMappingCapabilities(ctx, "map_a", editRows(t, now, CapabilityYes, tc.imageEdit)); err != nil {
				t.Fatalf("seed map_a: %v", err)
			}
			r := NewResolver(mem, func() time.Time { return now }, nil)
			_, satisfied := r.affinityCapabilityVerdict(ctx, "map_a", []string{CapabilityImageEdit})
			if satisfied != tc.wantSatisfied {
				t.Fatalf("affinityCapabilityVerdict satisfied = %v, want %v", satisfied, tc.wantSatisfied)
			}
		})
	}
}
