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

// TestMappingCapabilityVerdictReservedPairsAreRefused is issue #81's third
// unimplemented R1 ask: "consider refusing live_progress as a MANUAL capability
// name". Considering it is what produced the shape pinned here, which is NOT a
// name rule.
//
// The harm is not that the name is unknown -- the vocabulary is open on purpose
// -- but that a MANUAL row outranks every automated writer permanently
// (capabilitySourceRank puts "manual" at rank 3) while this row has TWO
// consumers that read it with OPPOSITE semantics. A manual
// `live_progress: "no"` reaches Target.LiveProgressSupport as "unsupported",
// which gateway's Responses passthrough gate reads as a VETO, so it silently and
// permanently disables the operator's own responses-live-timings switch on an
// endpoint they were not thinking about -- no probe can repair it (rank 1 loses
// to rank 3) and the mapping form cannot clear it, since that form submits "mtp"
// and "vision" only.
//
// `live_progress: "yes"` is a different matter and stays ALLOWED, which is why
// the rule is keyed on the PAIR: internal/provider's wantsLiveProgress decides
// on this verdict in BOTH directions ("supported" returns true ahead of its
// shape clause, which covers only llama_cpp and vllm), and no probe writes this
// row for llama_swap, litellm, tgi or custom -- so a manual "yes" is the only
// mechanism that ever existed for opting such an upstream into an exact
// mid-stream token count on /v1/chat/completions. Refusing it would have removed
// a real capability to prevent nothing: on the Responses side condition 5 is a
// veto, so a positive verdict permits nothing the veto has not already allowed.
// TestManualLiveProgressYesStaysAllowed below is that half.
//
// speculation_observed is reserved in BOTH directions because it costs nothing:
// its only writer is the gateway's own observation of relayed traffic, at rank 1
// and at most once per mapping per process lifetime; nothing routes, scores or
// filters on it, so no control is being taken away; and a "no" is a claim
// routing.CapabilitySpeculationObserved says cannot exist ("no evidence" is not
// "does not speculate").
func TestMappingCapabilityVerdictReservedPairsAreRefused(t *testing.T) {
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
			name: "a manual no on speculation_observed -- a claim its vocabulary cannot mean",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilitySpeculationObserved: routing.CapabilityNo}},
			want: ErrMappingCapabilityReserved,
		},
		{
			// The name is trimmed BEFORE the reservation is consulted, or the
			// refusal would be one space away from being bypassed.
			name: "a padded reserved pair is still reserved",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{"  " + routing.CapabilityLiveProgress + "  ": routing.CapabilityNo}},
			want: ErrMappingCapabilityReserved,
		},
		{
			name: "a manual yes on speculation_observed -- only the gateway's own observation may write it",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilitySpeculationObserved: routing.CapabilityYes}},
			want: ErrMappingCapabilityReserved,
		},
		{
			// Ordering matters and is pinned: an invalid VALUE on a reserved
			// name reports the value error, because that check shipped first and
			// a new refusal must not mask a validation that preceded it.
			name: "an invalid verdict on a reserved capability still reports the value error",
			req:  UpdateMappingRequest{CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: "maybe"}},
			want: ErrMappingCapabilityVerdictInvalid,
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

// TestMappingCapabilityVerdictReservedPairsStayResettable is the half that makes
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
func TestMappingCapabilityVerdictReservedPairsStayResettable(t *testing.T) {
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

// TestManualLiveProgressYesStaysAllowed pins the half of the reservation that is
// deliberately NOT refused, and it is the more valuable half to pin, because the
// natural "tidy this up" edit is to make the rule symmetric on the name.
//
// internal/provider's wantsLiveProgress reads this verdict in BOTH directions:
// `case "supported": return true` runs AHEAD of its shape clause, which covers
// only llama_cpp and vllm. And no probe writes this row for llama_swap, litellm,
// tgi or custom -- the /props detector needs a llama.cpp document. So for those
// kinds a manual "yes" is the ONLY mechanism that ever existed to opt a
// tolerant-but-unlisted upstream into an exact mid-stream token count on
// /v1/chat/completions. Refusing it would have removed a real, documented
// capability to prevent nothing: on the Responses side the same verdict is read
// as a veto, where a positive value permits nothing the veto has not already
// allowed.
//
// The row it writes is asserted, not just the absence of an error, because a
// refusal implemented as a silent drop would otherwise look identical here.
func TestManualLiveProgressYesStaysAllowed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
	fx := newCapabilityTestFixture(t, now, recorder)

	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: routing.CapabilityYes},
	}); err != nil {
		t.Fatalf("UpdateMapping (manual live_progress yes): %v -- this is the translate path's only opt-in for an upstream no probe writes a verdict for", err)
	}

	row := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityLiveProgress]
	if row.Verdict != routing.CapabilityYes || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("live_progress row = %+v, want manual yes", row)
	}
	// And it is still resettable, so allowing it does not create a row the
	// operator cannot take back.
	if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
		CapabilityVerdicts: map[string]string{routing.CapabilityLiveProgress: ""},
	}); err != nil {
		t.Fatalf("UpdateMapping (reset the manual yes): %v", err)
	}
	if _, ok := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilityLiveProgress]; ok {
		t.Fatal("live_progress row survived its reset")
	}
}
