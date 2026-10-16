// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"testing"
	"time"
)

// This file is the subscription exception of the operate gate (#183, spec
// 2026-10-15 sections 6 note (1) and 11).
//
// For an api-key account the three operate calls that open the stored credential
// and send it upstream (connection test, model refresh, usage refresh) belong to
// the owner AND a CM while the account is shared/full. For a SUBSCRIPTION account
// they are OWNER-ONLY even at `shared`: they open the OAuth token set, and a model
// or usage refresh renews an expired access token through the gateway's refresher,
// which persists the rotated single-use refresh token (and can trip an
// account-wide needs_reconnect). A CM may not trigger that on the owner's behalf.
// A CM may still read, rename, pause and re-prefix a subscription account: those
// are not these three operations and are covered by the PATCH tests.
//
// The answer for a CM is 403, not 404: the CM can already see the account, so
// "not found" would be a lie. Everyone who cannot see it at all still gets the
// identical 404, so the subscription check must run AFTER the operate gate.

// upstreamCalls counts everything a credential-using operate call can do beyond the
// store: ask the gateway to renew the token, probe the credential, list models,
// pull usage. A denied call must leave every counter where it was.
type upstreamCalls struct {
	refresher, validator, discovery, usage, compatUsage int
}

// subscriptionOperateEnv is the authorization fixture (volatile sealing, so a
// token set can be stored and rotated) with the three vendor seams that an
// operate call reaches replaced by recording fakes.
type subscriptionOperateEnv struct {
	*vendorAuthzEnv
	validators *fakeVendorValidators
	refresher  *fakeTokenRefresher
}

const (
	operateTestOldAccess   = "tok-old-access-DO-NOT-ECHO-91aa"
	operateTestLiveAccess  = "tok-live-access-DO-NOT-ECHO-52bb"
	operateTestFreshAccess = "tok-fresh-access-DO-NOT-ECHO-37cc"
	operateTestRefresh     = "tok-refresh-DO-NOT-ECHO-18dd"
	operateTestAccountID   = "acct-operate-1"
)

func newSubscriptionOperateEnv(t *testing.T) *subscriptionOperateEnv {
	t.Helper()
	e := newVendorPatchEnv(t)
	validators := installFakeVendorValidators(e.svc)
	// The fake renews like the gateway's locked refresher: it reseals a NEW token
	// set (fresh access token, unexpired) into the account, so a rotation is a write
	// the snapshot can see.
	refresher := &fakeTokenRefresher{t: t, svc: e.svc, routeStore: e.routes, fresh: &vendorauth.TokenSet{
		AccessToken: operateTestFreshAccess, RefreshToken: operateTestRefresh,
		ExpiresAt: e.now.Add(time.Hour), AccountID: operateTestAccountID,
	}}
	e.svc.vendorDiscovery.tokenRefresher = refresher.refresh
	return &subscriptionOperateEnv{vendorAuthzEnv: e, validators: validators, refresher: refresher}
}

func (e *subscriptionOperateEnv) upstream() upstreamCalls {
	e.discoverers.mu.Lock()
	defer e.discoverers.mu.Unlock()
	return upstreamCalls{
		refresher:   len(e.refresher.recorded()),
		validator:   len(e.validators.recorded()),
		discovery:   len(e.discoverers.calls),
		usage:       len(e.discoverers.usageCalls),
		compatUsage: len(e.discoverers.compatUsageCalls),
	}
}

// subscriptionAccountAt creates an OpenAI subscription account owned by
// usr_va_owner at level (OpenAI, because only its subscription has an active usage
// pull) and stores a token set in it. expired stores an access token that is past
// its expiry with a refresh token that can renew it: the next token-opening call
// that reaches the vendor rotates it. Otherwise the access token is live.
func (e *subscriptionOperateEnv) subscriptionAccountAt(level string, expired bool) routing.VendorAccount {
	e.t.Helper()
	acc := e.newAccountOf(level, CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: "sub-operate-" + level,
	})
	ts := vendorauth.TokenSet{AccessToken: operateTestLiveAccess, AccountID: operateTestAccountID}
	if expired {
		ts = vendorauth.TokenSet{
			AccessToken: operateTestOldAccess, RefreshToken: operateTestRefresh,
			ExpiresAt: e.now.Add(-time.Hour), AccountID: operateTestAccountID,
		}
	}
	setDiscoveryTokens(e.t, e.svc, e.routes, acc.ID, ts)
	acc = e.reload(acc.ID)
	if acc.AuthType != routing.VendorAuthSubscription || acc.OAuthTokens == "" {
		e.t.Fatalf("fixture at %s is not a connected subscription account: %+v", level, acc)
	}
	return acc
}

// tokenOperateOp is one of the three operations the subscription exception covers.
type tokenOperateOp struct {
	name string
	// rotates: the operation renews an expired token through the refresher, so its
	// subscription fixture holds an expired token and an owner call must rotate it.
	// The connection test only opens the set and asks the vendor about it, so its
	// fixture holds a live token and an owner call must reach the validator.
	rotates bool
	call    func(e *vendorAuthzEnv, p auth.Token, id string) error
	// reached reports whether delta (the upstream calls one call made) shows the
	// operation really ran for a subscription account.
	reached func(delta upstreamCalls) bool
}

func tokenOperateOps() []tokenOperateOp {
	return []tokenOperateOp{
		{
			name: "TestVendorAccountConnection",
			call: func(e *vendorAuthzEnv, p auth.Token, id string) error {
				_, err := e.svc.TestVendorAccountConnection(e.ctx, p, id)
				return err
			},
			reached: func(d upstreamCalls) bool { return d.validator == 1 && d.refresher == 0 },
		},
		{
			name: "RefreshVendorAccountModels", rotates: true,
			call: func(e *vendorAuthzEnv, p auth.Token, id string) error {
				_, _, err := e.svc.RefreshVendorAccountModels(e.ctx, p, id)
				return err
			},
			reached: func(d upstreamCalls) bool { return d.refresher == 1 && d.discovery == 1 },
		},
		{
			// maxAge 0 is the manual refresh, which always asks the vendor; the lazy
			// on-view pull is the same method with a positive TTL, so the same gate.
			name: "RefreshVendorAccountUsage", rotates: true,
			call: func(e *vendorAuthzEnv, p auth.Token, id string) error {
				_, _, err := e.svc.RefreshVendorAccountUsage(e.ctx, p, id, 0)
				return err
			},
			reached: func(d upstreamCalls) bool { return d.refresher == 1 && d.usage == 1 },
		},
	}
}

// tokenOperateWant is the spec table, written out for the three token-opening
// operate calls: the operate gate first (a principal who cannot see the account is
// 404 whatever it is), then the subscription exception (a CM who can see it is 403).
func tokenOperateWant(subscription bool, level, principal string) authzOutcome {
	if !vendorGateAllowed(false, level, principal) {
		return authzNotFound
	}
	if subscription && principal != "owner" {
		return authzForbidden
	}
	return authzAllow
}

// TestTokenOperateOpsAreOwnerOnlyForASubscriptionAccount is the subscription-rotation
// pin. For every kind of principal, at every share level, on a subscription and an
// api-key account, through the real service methods:
//
//   - a subscription account: only the owner is let through; a CM (the flagged
//     co-manager and the admin-group owner, who counts as one) is 403 at shared AND
//     at full (a subscription cannot be raised to full, so full is only reachable
//     by a stored row, and it fails closed); everyone else is 404;
//   - an api-key account: unchanged, the owner and a CM at shared/full;
//   - a denied call writes nothing and reaches no vendor and no refresher (the
//     account row with its sealed token set, its models and its usage are compared
//     byte for byte, and every upstream counter is unchanged);
//   - the owner's call is not turned away and really runs (it rotates the expired
//     token / asks the vendor), so the no-write assertion has something to catch.
func TestTokenOperateOpsAreOwnerOnlyForASubscriptionAccount(t *testing.T) {
	e := newSubscriptionOperateEnv(t)
	levels := []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull}

	for _, op := range tokenOperateOps() {
		for _, level := range levels {
			for _, subscription := range []bool{true, false} {
				kind := "api-key"
				if subscription {
					kind = "subscription"
				}
				for _, p := range vendorAuthzPrincipals() {
					// A pristine account per principal: the owner's call rotates the
					// expired token, which would leave the callers after it nothing
					// left to rotate and make their no-write assertion vacuous.
					var acc routing.VendorAccount
					if subscription {
						acc = e.subscriptionAccountAt(level, op.rotates)
					} else {
						acc = e.keyedAccountAt(level)
					}
					label := op.name + "/" + kind + "/" + level + "/" + p.name
					want := tokenOperateWant(subscription, level, p.name)
					before := e.snapshot(acc.ID)
					calls := e.upstream()

					err := op.call(e.vendorAuthzEnv, p.tok, acc.ID)

					requireAuthzOutcome(t, label, err, want)
					after := e.upstream()
					if want != authzAllow {
						e.requireUnchanged(label, before)
						if after != calls {
							t.Errorf("%s: a denied call reached upstream: before %+v, after %+v", label, calls, after)
						}
						continue
					}
					if subscription {
						e.requireOwnerRanTheOperation(label, op, before, calls, after)
					}
				}
			}
		}
	}
}

// requireOwnerRanTheOperation checks an allowed subscription call genuinely did its
// job: the vendor/refresher was reached as the operation implies, and a rotating
// operation replaced the stored token set. Without this a gate that turned
// everybody away would pass the denied-writes-nothing assertions vacuously.
func (e *subscriptionOperateEnv) requireOwnerRanTheOperation(label string, op tokenOperateOp, before vendorAccountState, calls, after upstreamCalls) {
	e.t.Helper()
	delta := upstreamCalls{
		refresher:   after.refresher - calls.refresher,
		validator:   after.validator - calls.validator,
		discovery:   after.discovery - calls.discovery,
		usage:       after.usage - calls.usage,
		compatUsage: after.compatUsage - calls.compatUsage,
	}
	if !op.reached(delta) {
		e.t.Errorf("%s: the owner's call did not run as expected, upstream delta = %+v", label, delta)
	}
	rotated := e.reload(before.Account.ID).OAuthTokens != before.Account.OAuthTokens
	if rotated != op.rotates {
		e.t.Errorf("%s: token set rotated = %v, want %v", label, rotated, op.rotates)
	}
}

// TestRequireOwnerForSubscriptionOperate pins the one rule the three operations
// share, on its own: a subscription account answers only its owner; an api-key
// account (no token set to rotate) is untouched by it; a principal with no user
// identity owns nothing; and the answer is the 403, never the gate's 404 (that
// stays the operate gate's, which runs first).
func TestRequireOwnerForSubscriptionOperate(t *testing.T) {
	owner := token(vendorAuthzOwnerID)
	cm := token("usr_cm", "admin")
	service := auth.Token{Scopes: []string{"gateway:use"}}
	sub := routing.VendorAccount{ID: "va_s", OwnerUserID: vendorAuthzOwnerID, AuthType: routing.VendorAuthSubscription}
	key := routing.VendorAccount{ID: "va_k", OwnerUserID: vendorAuthzOwnerID, AuthType: routing.VendorAuthAPIKey}
	// A row that names no owner must not make the empty user id of a service token
	// an owner (isVendorAccountOwner already refuses it; this keeps it that way).
	ownerless := routing.VendorAccount{ID: "va_o", AuthType: routing.VendorAuthSubscription}

	cases := []struct {
		name      string
		principal auth.Token
		acc       routing.VendorAccount
		want      authzOutcome
	}{
		{"owner of a subscription", owner, sub, authzAllow},
		{"co-manager of a subscription", cm, sub, authzForbidden},
		{"service token on a subscription", service, sub, authzForbidden},
		{"service token on an ownerless subscription", service, ownerless, authzForbidden},
		{"owner of an api-key account", owner, key, authzAllow},
		{"co-manager of an api-key account", cm, key, authzAllow},
		{"service token on an api-key account", service, key, authzAllow},
	}
	for _, tc := range cases {
		requireAuthzOutcome(t, tc.name, requireOwnerForSubscriptionOperate(tc.principal, tc.acc), tc.want)
	}
}

// TestCMKeepsTheNonTokenOperateCallsOnASharedSubscription is the other edge of the
// exception: it narrows exactly the three token-opening calls and nothing else. A CM
// of a shared subscription account still reads it and still renames it, pauses it
// and changes its model prefix (the operate allowlist of the PATCH), without ever
// touching the sealed token set.
func TestCMKeepsTheNonTokenOperateCallsOnASharedSubscription(t *testing.T) {
	e := newSubscriptionOperateEnv(t)
	cm := token("usr_cm", "admin")
	acc := e.subscriptionAccountAt(routing.VendorShareLevelShared, true)

	if _, err := e.svc.GetVendorAccount(e.ctx, cm, acc.ID); err != nil {
		t.Fatalf("CM read of the shared subscription: %v", err)
	}
	if _, err := e.svc.UpdateVendorAccount(e.ctx, cm, acc.ID, UpdateVendorAccountRequest{
		Name: strPtr("Renamed by CM"), Status: strPtr(routing.VendorAccountStatusDisabled), ModelPrefix: strPtr("cm-"),
	}); err != nil {
		t.Fatalf("CM name/status/model_prefix PATCH of the shared subscription: %v", err)
	}
	got := e.reload(acc.ID)
	if got.Name != "Renamed by CM" || got.Status != routing.VendorAccountStatusDisabled || got.ModelPrefix != "cm-" {
		t.Errorf("PATCH not applied: name/status/prefix = %q/%q/%q", got.Name, got.Status, got.ModelPrefix)
	}
	if got.OAuthTokens != acc.OAuthTokens {
		t.Error("an operate-only PATCH by a CM touched the sealed token set")
	}
	if calls := e.upstream(); calls != (upstreamCalls{}) {
		t.Errorf("an operate-only PATCH reached upstream: %+v", calls)
	}
}
