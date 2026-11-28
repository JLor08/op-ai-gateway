// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
)

// TestVendorModelServedFlavorsDecisionEmulation (#380): an OpenAI subscription
// account's decision-model row is listed under the decisions flavor only for a
// token the resolver would route to it -- account and token opt-in, the
// decision model's upstream -- and its chat dialect stays listed either way.
func TestVendorModelServedFlavorsDecisionEmulation(t *testing.T) {
	luna := routing.VendorAccountModel{GatewayModel: "oai/gpt-6-luna", UpstreamModel: "gpt-6-luna", APIFlavor: routing.APIFlavorOpenAI}
	chat := routing.VendorAccountModel{GatewayModel: "oai/gpt-5", UpstreamModel: "gpt-5", APIFlavor: routing.APIFlavorOpenAI}
	sub := func(on bool) routing.VendorAccount {
		return routing.VendorAccount{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, DecisionEmulation: on}
	}
	cases := []struct {
		name        string
		acc         routing.VendorAccount
		m           routing.VendorAccountModel
		token       bool
		wantDecides bool
	}{
		{"both opt-ins", sub(true), luna, true, true},
		{"account only", sub(true), luna, false, false},
		{"token only", sub(false), luna, true, false},
		{"neither", sub(false), luna, false, false},
		{"chat-only row", sub(true), chat, true, false},
		{"anthropic subscription", routing.VendorAccount{Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, DecisionEmulation: true}, luna, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vendorModelServedFlavors(tc.acc, tc.m, auth.Token{DecisionsEmulation: tc.token})
			if slices.Contains(got, routing.APIFlavorDecisions) != tc.wantDecides {
				t.Fatalf("flavors = %v, want decisions listed = %v", got, tc.wantDecides)
			}
			if !slices.Contains(got, routing.APIFlavorOpenAI) {
				t.Fatalf("flavors = %v, want the chat dialect kept", got)
			}
		})
	}
}
