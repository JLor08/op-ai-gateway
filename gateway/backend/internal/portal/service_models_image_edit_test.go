// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// editRow is one manual capability row, the shape an operator's mapping form
// writes.
func editRow(capability, verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{
		Capability: capability, Verdict: verdict,
		Source: routing.CapabilitySourceManual, CheckedAt: offeringTime,
	}
}

// offerEditModel seeds a server + application + one active mapping for a
// gateway model with exactly the given capability rows, so a test states the
// image and image_edit rows it cares about directly. The mapping id is
// appID+"_map", like offerModelImage's.
func offerEditModel(t *testing.T, rs *routing.MemoryStore, srvID, appID string, flavors []string, gateway string, rows ...routing.CapabilityRow) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	mappingID := appID + "_map"
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway + "-up", Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
	if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
	}
}

var (
	rowImageYes = editRow(routing.CapabilityImage, routing.CapabilityYes)
	rowImageNo  = editRow(routing.CapabilityImage, routing.CapabilityNo)
	rowEditNo   = editRow(routing.CapabilityImageEdit, routing.CapabilityNo)
	rowEditYes  = editRow(routing.CapabilityImageEdit, routing.CapabilityYes)
)

// ModelDTO.ImageEdit is the listing's image_edit flag: the derivation
// (routing.ImpliedImageEdit) ANDed across a model's mappings. Every shape the
// derivation tells apart is pinned, because each is a distinct decision -- and
// image stays independent of it (the opt-out removes edit only).
func TestModelsResponseImageEditFlag(t *testing.T) {
	rs := routing.NewMemoryStore()
	offerEditModel(t, rs, "srv_d", "app_d", imageServingFlavors, "derived", rowImageYes)
	offerEditModel(t, rs, "srv_o", "app_o", imageServingFlavors, "opted-out", rowImageYes, rowEditNo)
	offerEditModel(t, rs, "srv_y", "app_y", imageServingFlavors, "inert-yes", rowImageYes, rowEditYes)
	offerEditModel(t, rs, "srv_n", "app_n", imageServingFlavors, "not-a-generator", rowImageNo)
	offerEditModel(t, rs, "srv_g", "app_g", imageServingFlavors, "yes-alone", rowEditYes)
	offerEditModel(t, rs, "srv_u", "app_u", imageServingFlavors, "no-rows")
	offerEditModel(t, rs, "srv_v", "app_v", imageServingFlavors, "vision-only", editRow(routing.CapabilityVision, routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name            string
		image, imageEdt bool
	}{
		{"derived", true, true},           // image:yes, no image_edit row: derived from image
		{"opted-out", true, false},        // the operator opt-out removes edit, never generation
		{"inert-yes", true, true},         // image_edit:yes adds nothing the derivation did not already say
		{"not-a-generator", false, false}, // image:no never edits
		{"yes-alone", false, false},       // image_edit:yes is inert: it cannot grant edit without image:yes
		{"no-rows", false, false},         // nothing to derive from
		{"vision-only", false, false},     // a vision row says nothing about edit
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Image != tc.image || got.ImageEdit != tc.imageEdt {
			t.Errorf("%s image=%v image_edit=%v, want image=%v image_edit=%v", tc.name, got.Image, got.ImageEdit, tc.image, tc.imageEdt)
		}
	}
}

// The flag is an AND across a model's mappings, fail-closed, like image: a
// model on server A (image_edit:no) and server B (derived) is ADVERTISED false
// while the routing gate (a per-candidate any-of) would still SERVE it from B.
// That asymmetry is the safe direction and is deliberate -- the invariant is
// "the composer never offers what the gate refuses", not "they agree exactly"
// -- so it is pinned here, with Image staying true to show it is the opt-out
// and not the generation verdict that lowers the flag.
//
// "both-derived" is what pins the fail-closed SEED: two mappings only reach
// true because the accumulator starts true on a model's first view.
func TestModelsResponseImageEditAndAcrossMappings(t *testing.T) {
	rs := routing.NewMemoryStore()
	offerEditModel(t, rs, "srv_a", "app_a", imageServingFlavors, "split", rowImageYes, rowEditNo)
	offerEditModel(t, rs, "srv_b", "app_b", imageServingFlavors, "split", rowImageYes)
	offerEditModel(t, rs, "srv_c", "app_c", imageServingFlavors, "both-derived", rowImageYes)
	offerEditModel(t, rs, "srv_d", "app_d", imageServingFlavors, "both-derived", rowImageYes)
	byID := modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))

	if got := byID["split"]; !got.Image || got.ImageEdit {
		t.Fatalf("split image=%v image_edit=%v, want image=true image_edit=false -- one mapping opted out, and the fold is an AND", got.Image, got.ImageEdit)
	}
	if got := byID["both-derived"]; !got.Image || !got.ImageEdit {
		t.Fatalf("both-derived image=%v image_edit=%v, want both true", got.Image, got.ImageEdit)
	}
}

// The edit flag is also gated on the flavor the edits endpoint is served under:
// edits share the openai_images flavor with generations, so an application
// that does not declare it is never routed an edit, whatever its verdicts.
func TestModelsImageEditRequiresTheImagesFlavor(t *testing.T) {
	rs := routing.NewMemoryStore()
	offerEditModel(t, rs, "srv_t", "app_t", []string{routing.APIFlavorOpenAI}, "text-flavor", rowImageYes)
	offerEditModel(t, rs, "srv_m", "app_m", imageServingFlavors, "images-flavor", rowImageYes)
	byID := modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))

	if byID["text-flavor"].ImageEdit {
		t.Fatal("text-flavor image_edit = true, want false -- its application does not declare openai_images, so the images endpoints never route to it")
	}
	if !byID["images-flavor"].ImageEdit {
		t.Fatal("images-flavor image_edit = false, want true")
	}
}

// An agent-launched model is served an edit only when both flavor stages admit
// it, exactly as for generation: the application's flavors AND the resolved
// runtime spec's. image_edit must say what image says.
func TestModelsImageEditOfAnAgentMappingNeedsTheFlavorAtBothStages(t *testing.T) {
	both := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}
	textOnly := []string{routing.APIFlavorOpenAI}
	imagesOnly := []string{routing.APIFlavorOpenAIImages}
	rs := routing.NewMemoryStore()
	offerAgentModelImage(t, rs, "srv_a1", "app_a1", both, imagesOnly, "agent-both")
	offerAgentModelImage(t, rs, "srv_a2", "app_a2", both, textOnly, "agent-spec-narrows")
	offerAgentModelImage(t, rs, "srv_a3", "app_a3", textOnly, imagesOnly, "agent-app-lacks")
	byID := modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))

	for name, want := range map[string]bool{
		"agent-both":         true,
		"agent-spec-narrows": false,
		"agent-app-lacks":    false,
	} {
		if got := byID[name]; got.ImageEdit != want || got.ImageEdit != got.Image {
			t.Errorf("%s image_edit=%v image=%v, want image_edit=%v and equal to image", name, got.ImageEdit, got.Image, want)
		}
	}
}

// A group's image_edit flag is the AND of its offerable members' flags, and an
// override alias carries its target's flag under the alias name -- both are
// their own wiring beside image's, so a build that skipped either would pass
// every image test unchanged while leaving image_edit silently false there.
func TestModelsImageEditGroupAndAliasFold(t *testing.T) {
	rs := routing.NewMemoryStore()
	offerEditModel(t, rs, "srv_1", "app_1", imageServingFlavors, "edit-ok", rowImageYes)
	offerEditModel(t, rs, "srv_2", "app_2", imageServingFlavors, "edit-optout", rowImageYes, rowEditNo)
	offerGroup(t, rs, "grp_mixed", "mixed-edit-group", "edit-ok", "edit-optout")
	offerGroup(t, rs, "grp_pure", "pure-edit-group", "edit-ok")
	svc := offerSvc(rs, nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-ok":     {To: "edit-ok", Offer: true},
		"alias-optout": {To: "edit-optout", Offer: true},
	})
	byID := modelsByID(svc.Models(context.Background(), token))

	for name, want := range map[string]bool{
		"mixed-edit-group": false, // one member opted out
		"pure-edit-group":  true,
		"alias-ok":         true,
		"alias-optout":     false,
	} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if got.ImageEdit != want {
			t.Errorf("%s image_edit = %v, want %v", name, got.ImageEdit, want)
		}
	}
	// The group is still an IMAGE group: the opt-out lowered edit only.
	if !byID["mixed-edit-group"].Image {
		t.Error("mixed-edit-group image = false, want true -- both members generate; only one opted out of edit")
	}
}

// A failed capability read degrades every flag to false (fail-closed): the
// composer must not offer an edit it cannot show the gate would serve.
func TestModelsImageEditIsWithheldWhenTheCapabilityReadFails(t *testing.T) {
	rs := routing.NewMemoryStore()
	offerEditModel(t, rs, "srv_d", "app_d", imageServingFlavors, "derived", rowImageYes)
	failing := &capabilityReadStore{MemoryStore: rs, err: errors.New("capability read down")}
	byID := modelsByID(offerSvc(failing, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))

	got, ok := byID["derived"]
	if !ok {
		t.Fatalf("derived missing from Models(): %#v", byID)
	}
	if got.ImageEdit || got.Image {
		t.Fatalf("derived image=%v image_edit=%v after a failed capability read, want both withheld", got.Image, got.ImageEdit)
	}
}

// editOfferingStore is capableOfferingStore plus the shapes the edit
// derivation adds: a model whose operator opted out of edit, and one that
// opted out on one of two servers.
func editOfferingStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	rs := capableOfferingStore(t)
	offerEditModel(t, rs, "srv_oo", "app_oo", imageServingFlavors, "img-optout", rowImageYes, rowEditNo)
	offerEditModel(t, rs, "srv_s1", "app_s1", imageServingFlavors, "img-split", rowImageYes, rowEditNo)
	offerEditModel(t, rs, "srv_s2", "app_s2", imageServingFlavors, "img-split", rowImageYes)
	offerGroup(t, rs, "grp_optout", "optout-group", "img-plain", "img-optout")
	return rs
}

// capableNames must DERIVE image_edit. Before this capability it returned an
// empty set for any required capability but image, which made the unknown-model
// redirect dead for edits: an unknown edit model would 404 instead of being
// redirected to a candidate. The set is decided by the SAME fold as the
// listing's ImageEdit -- the shared derivation -- never by a second rule, so
// every callable name is capable exactly when the listing says ImageEdit, and
// the explicit expectations pin each shape so that agreement cannot come from
// both sides being wrong together (e.g. both empty).
func TestModelOfferingCapableDerivesImageEdit(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(editOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImageEdit})
	byID := modelsByID(svc.Models(ctx, token))

	want := map[string]bool{
		"img-plain":        true,
		"img-only":         true,
		"agent-both":       true,
		"agent-no-spec":    true,
		"img-no":           false, // callable, but image:no -- nothing to derive from
		"img-optout":       false, // callable, image:yes, but the operator opted out of edit
		"img-split":        false, // the fold ANDs: one of its two servers opted out
		"pure-image-group": true,
		"optout-group":     false, // one member opted out
	}
	for name, capable := range want {
		if _, ok := off.Callable[name]; !ok {
			t.Errorf("%s missing from Callable(openai_images): %#v", name, off.Callable)
			continue
		}
		if _, got := off.Capable[name]; got != capable {
			t.Errorf("%s capable = %v, want %v", name, got, capable)
		}
	}
	for name := range off.Callable {
		if _, got := off.Capable[name]; got != byID[name].ImageEdit {
			t.Errorf("%s capable = %v, listing ImageEdit = %v -- the two must be one fold", name, got, byID[name].ImageEdit)
		}
	}
	// An opted-out model still EXISTS and is CALLABLE: the offering tells the
	// redirect "this name is real but not capable", which is the 404-vs-redirect
	// distinction the preflight draws.
	if _, ok := off.Existing["img-optout"]; !ok {
		t.Errorf("img-optout missing from Existing(openai_images): %#v", off.Existing)
	}
}

// Requiring BOTH capabilities intersects the two folds, and requiring any name
// without a fold beside image_edit still fails closed. Both directions matter:
// an opted-out model still passes an image-only requirement (the opt-out
// removes edit, not generation), and only the intersection is returned when
// both are asked.
func TestModelOfferingCapableIntersectsRequiredCapabilities(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(editOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	image := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImage})
	if _, ok := image.Capable["img-optout"]; !ok {
		t.Errorf("img-optout missing from Capable([image]): %#v -- the opt-out must not touch generation", image.Capable)
	}

	both := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImage, routing.CapabilityImageEdit})
	if _, ok := both.Capable["img-plain"]; !ok {
		t.Errorf("img-plain missing from Capable([image image_edit]): %#v", both.Capable)
	}
	if _, ok := both.Capable["img-optout"]; ok {
		t.Errorf("img-optout in Capable([image image_edit]): %#v, want it out -- it opted out of edit", both.Capable)
	}

	unknown := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImageEdit, "teleportation"})
	if len(unknown.Capable) != 0 {
		t.Fatalf("Capable([image_edit teleportation]) = %#v, want empty -- a name without a fold fails closed even beside one with a fold", unknown.Capable)
	}
}

// A failed capability read gives the all-or-nothing empty offering for an
// edit, like it does for generation, so the redirect declines instead of
// guessing which candidate may edit.
func TestModelOfferingForImageEditIsEmptyWhenTheCapabilityReadFails(t *testing.T) {
	failing := &capabilityReadStore{MemoryStore: editOfferingStore(t), err: errors.New("capability read down")}
	off := offerSvc(failing, nil).ModelOfferingFor(context.Background(), auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImageEdit})
	if len(off.Callable) != 0 || len(off.Capable) != 0 || len(off.Existing) != 0 {
		t.Fatalf("offering = %#v, want every set empty on a failed capability read", off)
	}
}

// (image_edit, no) is the operator's opt-out and is ALREADY writable: the
// capability vocabulary is open, and reservedManualVerdicts is a DENY list of
// pairs an operator may not state. Adding the pair there would forbid the
// opt-out the derivation relies on -- while (image, no) IS reserved, which is
// exactly why the opt-out is a separate image_edit row. The round trip is
// asserted end to end: the manual row lands, the listing reads it, and the
// reset (the empty verdict) returns the model to derived-capable.
func TestManualImageEditNoIsAWritableOptOutAndResettable(t *testing.T) {
	if reservedManualVerdict(routing.CapabilityImageEdit, routing.CapabilityNo) {
		t.Fatal("(image_edit, no) is reserved, want writable -- it is the operator's opt-out, the whole mechanism")
	}
	if reservedManualVerdict(routing.CapabilityImageEdit, routing.CapabilityYes) {
		t.Fatal("(image_edit, yes) is reserved, want writable (it is inert, and refusing it would say something the gate does not)")
	}
	if !reservedManualVerdict(routing.CapabilityImage, routing.CapabilityNo) {
		t.Fatal("(image, no) must stay reserved -- that is why the opt-out is a separate image_edit row")
	}

	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)
	if err := recorder.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{rowImageYes}); err != nil {
		t.Fatalf("seed image:yes: %v", err)
	}
	if !routing.ImpliedImageEdit(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID)) {
		t.Fatal("precondition: an image:yes mapping with no opt-out must derive edit")
	}

	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityImageEdit: routing.CapabilityNo},
	}); err != nil {
		t.Fatalf("UpdateMapping (image_edit:no opt-out): %v", err)
	}
	rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID)
	if row := routing.CapabilityRowsByName(rows)[routing.CapabilityImageEdit]; row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("image_edit row = %+v, want manual no", row)
	}
	if routing.ImpliedImageEdit(rows) {
		t.Fatal("ImpliedImageEdit = true after the opt-out, want false")
	}

	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityImageEdit: ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset the opt-out): %v", err)
	}
	rows = mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID)
	if _, ok := routing.CapabilityRowsByName(rows)[routing.CapabilityImageEdit]; ok {
		t.Fatal("image_edit row survived its reset")
	}
	if !routing.ImpliedImageEdit(rows) {
		t.Fatal("ImpliedImageEdit = false after the reset, want derived-capable again")
	}
}

// The reason the derivation is ONE function (ADR-045: one function both read):
// the composer must never offer an edit the routing gate refuses. This test
// drives the REAL resolver beside the listing over the same rows, so a second
// copy of the rule drifting on either side fails here rather than in
// production. For a model with a single mapping the two agree exactly; for a
// model on two servers the listing (an AND) is stricter than the gate (a
// per-candidate any-of), so the invariant there is the one-way implication --
// advertised implies served -- and the split model pins the deliberate
// asymmetry: served, but advertised false.
func TestImageEditListingNeverOffersWhatTheGateRefuses(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	type shape struct {
		name string
		rows [][]routing.CapabilityRow // one entry per mapping/server
	}
	shapes := []shape{
		{"derived", [][]routing.CapabilityRow{{rowImageYes}}},
		{"opted-out", [][]routing.CapabilityRow{{rowImageYes, rowEditNo}}},
		{"inert-yes", [][]routing.CapabilityRow{{rowImageYes, rowEditYes}}},
		{"not-a-generator", [][]routing.CapabilityRow{{rowImageNo}}},
		{"yes-alone", [][]routing.CapabilityRow{{rowEditYes}}},
		{"no-rows", [][]routing.CapabilityRow{{}}},
		{"split", [][]routing.CapabilityRow{{rowImageYes, rowEditNo}, {rowImageYes}}},
		{"both-optout", [][]routing.CapabilityRow{{rowImageYes, rowEditNo}, {rowImageYes, rowEditNo}}},
	}
	for _, sh := range shapes {
		for j, rows := range sh.rows {
			id := sh.name + "_" + string(rune('a'+j))
			offerEditModel(t, rs, "srv_"+id, "app_"+id, imageServingFlavors, sh.name, rows...)
		}
	}
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))
	resolver := routing.NewResolver(rs, func() time.Time { return offeringTime }, nil)

	for _, sh := range shapes {
		_, err := resolver.Resolve(ctx, auth.Token{UserID: "usr_1"}, inference.Request{
			Model: sh.name, APIFlavor: routing.APIFlavorOpenAIImages,
			RequiredCapabilities: []string{routing.CapabilityImageEdit},
		})
		served := err == nil
		advertised := byID[sh.name].ImageEdit
		switch {
		case advertised && !served:
			t.Errorf("%s: listing offers an edit the gate refuses (Resolve err = %v)", sh.name, err)
		case len(sh.rows) == 1 && advertised != served:
			t.Errorf("%s (single mapping): advertised = %v, served = %v, want exact agreement", sh.name, advertised, served)
		}
		if sh.name == "split" && (advertised || !served) {
			t.Errorf("split: advertised = %v served = %v, want advertised=false served=true -- the AND is stricter than the any-of, the safe direction", advertised, served)
		}
	}
}
