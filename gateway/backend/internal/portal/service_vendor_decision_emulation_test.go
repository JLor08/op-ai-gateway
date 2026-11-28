// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestSetVendorAccountDecisionEmulation: the owner-only opt-in (#380) to answer
// /v1/decisions through a chat model of an OpenAI subscription account. The owner
// rule is the share-usage opt-in's; only an OpenAI subscription account accepts it.
func TestSetVendorAccountDecisionEmulation(t *testing.T) {
	e := newVendorMemberEnv(t)
	owner := token(vendorAuthzOwnerID)
	acc := e.newAccountOf(routing.VendorShareLevelShared, CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: "plus",
	})
	e.link(acc.ID)
	stored := func(id string) bool { return e.reload(id).DecisionEmulation }

	dto, err := e.svc.SetVendorAccountDecisionEmulation(e.ctx, owner, acc.ID, SetVendorAccountDecisionEmulationRequest{Enabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("owner on: %v", err)
	}
	if !dto.DecisionEmulation || !stored(acc.ID) {
		t.Errorf("owner on: dto %+v, stored %v", dto, stored(acc.ID))
	}
	dto, err = e.svc.SetVendorAccountDecisionEmulation(e.ctx, owner, acc.ID, SetVendorAccountDecisionEmulationRequest{Enabled: boolPtr(false)})
	if err != nil || dto.DecisionEmulation || stored(acc.ID) {
		t.Errorf("owner off: dto %+v err %v stored %v", dto, err, stored(acc.ID))
	}
	if _, err := e.svc.SetVendorAccountDecisionEmulation(e.ctx, owner, acc.ID, SetVendorAccountDecisionEmulationRequest{}); !errors.Is(err, ErrVendorAccountDecisionEmulationEnabledRequired) {
		t.Errorf("missing enabled err = %v", err)
	}

	for name, tc := range map[string]struct {
		tok  func() error
		want error
	}{
		"cm": {func() error {
			_, err := e.svc.SetVendorAccountDecisionEmulation(e.ctx, token("usr_cm", "admin"), acc.ID, SetVendorAccountDecisionEmulationRequest{Enabled: boolPtr(true)})
			return err
		}, ErrVendorAccountForbidden},
		"stranger": {func() error {
			_, err := e.svc.SetVendorAccountDecisionEmulation(e.ctx, token("usr_stranger"), acc.ID, SetVendorAccountDecisionEmulationRequest{Enabled: boolPtr(true)})
			return err
		}, ErrVendorAccountNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.tok(); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if stored(acc.ID) {
				t.Error("a refused call wrote the opt-in")
			}
		})
	}

	unsupported := map[string]CreateVendorAccountRequest{
		"openai api key":         {Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "k"},
		"anthropic subscription": {Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "a"},
	}
	for name, req := range unsupported {
		t.Run(name, func(t *testing.T) {
			other := e.newAccountOf(routing.VendorShareLevelNone, req)
			_, err := e.svc.SetVendorAccountDecisionEmulation(e.ctx, owner, other.ID, SetVendorAccountDecisionEmulationRequest{Enabled: boolPtr(true)})
			if !errors.Is(err, ErrVendorAccountDecisionEmulationUnsupported) {
				t.Errorf("err = %v, want unsupported", err)
			}
			if stored(other.ID) {
				t.Error("unsupported account was written")
			}
		})
	}
}
