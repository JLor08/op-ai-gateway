// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestMappingCapabilityVerdictReservedNamesAreRefusedOnSet is issue #81's third
// unimplemented R1 ask: "consider refusing live_progress as a MANUAL capability
// name".
//
// The harm is not that the name is unknown -- the vocabulary is open on purpose
// -- but that a MANUAL row outranks every automated writer permanently
// (capabilitySourceRank puts "manual" at rank 3) and this particular row is read
// by the request path as a VETO. A manual `live_progress: "no"` therefore
// reaches Target.LiveProgressSupport as "unsupported" and silently, permanently
// disables the operator's own responses-live-timings switch, with no probe able
// to repair it and no control on the mapping form able to clear it. That is
// exactly the "silently dead switch" failure the gate's veto design cites as the
// worst outcome available to a control someone deliberately switched on.
//
// speculation_observed is reserved on the same argument minus the functional
// half: its only legitimate writer is the gateway's own observation of relayed
// traffic, at rank 1, written at most once per mapping per process lifetime, and
// a "no" is a claim routing.CapabilitySpeculationObserved says cannot exist
// ("no evidence" is not "does not speculate").
func TestMappingCapabilityVerdictReservedNamesAreRefusedOnSet(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	for _, tc := range []struct {
		name string
		req  UpdateMappingRequest
		want error
	}{
		{
			name: "a manual no on live_progress -- the permanent veto",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: routing.CapabilityNo}},
			want: ErrMappingCapabilityReserved,
		},
		{
			name: "a manual yes on live_progress is refused too -- an operator cannot attest a build's request schema",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: routing.CapabilityYes}},
			want: ErrMappingCapabilityReserved,
		},
		{
			// The name is trimmed BEFORE the reservation is consulted, or the
			// refusal would be one space away from being bypassed.
			name: "a padded reserved name is still reserved",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{"  " + routing.CapabilityLiveProgress + "  ": routing.CapabilityNo}},
			want: ErrMappingCapabilityReserved,
		},
		{
			name: "a manual verdict on speculation_observed",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilitySpeculationObserved: routing.CapabilityYes}},
			want: ErrMappingCapabilityReserved,
		},
		{
			// Ordering matters and is pinned: an invalid VALUE on a reserved
			// name reports the value error, because that check shipped first and
			// a new refusal must not mask a validation that preceded it.
			name: "an invalid verdict on a reserved name still reports the value error",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: "maybe"}},
			want: ErrMappingCapabilityVerdictInvalid,
		},
		{
			// A blank name is still the blank-name error even though the map
			// also carries a reserved one: the whole request is refused either
			// way, but the CODE a client sees must not depend on map order.
			name: "a blank name beside a reserved one reports the blank name",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{"": routing.CapabilityYes, routing.CapabilityLiveProgress: routing.CapabilityNo}},
			want: ErrMappingCapabilityNameRequired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("UpdateMapping err = %v, want %v", err, tc.want)
			}
		})
	}
	// A refused request writes nothing at all -- not even the rows it stated
	// beside the reserved one.
	if len(recorder.deletes) != 0 {
		t.Fatalf("a refused request performed %d deletes: %v", len(recorder.deletes), recorder.deletes)
	}
	if rows := mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID); len(rows) != 0 {
		t.Fatalf("a refused request wrote %d capability rows: %+v", len(rows), rows)
	}
}

// TestMappingCapabilityVerdictReservedNamesStayResettable is the half that makes
// the refusal above safe, and it is NOT symmetric with it on purpose.
//
// normalizeCapabilityVerdicts' own doc comment raises the objection this test
// answers: "a name-whitelisting check would leave exactly those rows
// uncorrectable". §11.1 records the "a manual verdict has no way back" risk as
// CLOSED specifically because an empty verdict deletes the row, and ADR-039
// records that the reset propagates its store error because relinquishing the
// verdict is the whole effect of the action. So the refusal is verdict-dependent,
// never name-dependent: a SET is refused, the RESET still works.
//
// Without this, a deployment that already carries a manual live_progress row --
// mintable through the API for as long as the reservation did not exist -- would
// be permanently stuck with a disabled live-timings switch and no way to clear
// it, which is a worse state than the one the refusal prevents.
func TestMappingCapabilityVerdictReservedNamesStayResettable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	// The state a pre-reservation deployment can be in: a manual "no" vetoing
	// the operator's own switch, beside an unrelated row that must survive.
	if err := recorder.MemoryStore.UpsertMappingCapabilities(ctx, fx.mappingID, []routing.CapabilityRow{
		{Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// Padded, because the reset path trims the same way the refusal does.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{"  " + routing.CapabilityLiveProgress + "  ": ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset a reserved name): %v", err)
	}

	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
	if row, ok := rows[routing.CapabilityLiveProgress]; ok {
		t.Fatalf("live_progress row = %+v, want deleted -- a stored bad row must stay correctable", row)
	}
	if rows[routing.CapabilityVision].Verdict != routing.CapabilityYes {
		t.Fatalf("vision row = %+v, want untouched yes -- only the named capability is touched", rows[routing.CapabilityVision])
	}
	if len(recorder.deletes) != 1 || recorder.deletes[0].capability != routing.CapabilityLiveProgress {
		t.Fatalf("deletes = %v, want exactly [%s]", recorder.deletes, routing.CapabilityLiveProgress)
	}
}

// TestCreateMappingRefusesAReservedCapabilityVerdict pins that the create path
// shares the refusal. Both paths call normalizeCapabilityVerdicts, so this is
// one line of coverage against someone adding a second, narrower validator on
// one of them -- the same reason the sibling rejection tables cover both.
func TestCreateMappingRefusesAReservedCapabilityVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	_, err := fx.svc.CreateMapping(ctx, ownerToken(), fx.appID, CreateMappingRequest{
		GatewayModelName:   "another-model",
		AppModelName:       "upstream-another",
		CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: routing.CapabilityNo},
	})
	if !errors.Is(err, ErrMappingCapabilityReserved) {
		t.Fatalf("CreateMapping err = %v, want %v", err, ErrMappingCapabilityReserved)
	}
}

// TestMappingCapabilityVerdictOpenVocabularySurvivesTheReservation is the
// regression guard for the reservation's SCOPE. The vocabulary must stay open
// for every name the reservation does not list -- including "mtp", which is
// deliberately NOT reserved because it is the one of the three internal names
// that HAS an operator control on the mapping form, and reserving it would break
// that control.
func TestMappingCapabilityVerdictOpenVocabularySurvivesTheReservation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{
			routing.CapabilityMTP: routing.CapabilityYes,
			"structured_outputs":  routing.CapabilityNo,
		},
	}); err != nil {
		t.Fatalf("UpdateMapping (unreserved names): %v", err)
	}

	rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
	if rows[routing.CapabilityMTP].Verdict != routing.CapabilityYes {
		t.Fatalf("mtp row = %+v, want manual yes -- the mapping form's own control writes this name", rows[routing.CapabilityMTP])
	}
	if rows["structured_outputs"].Verdict != routing.CapabilityNo {
		t.Fatalf("structured_outputs row = %+v, want manual no -- the vocabulary stays open", rows["structured_outputs"])
	}
}
