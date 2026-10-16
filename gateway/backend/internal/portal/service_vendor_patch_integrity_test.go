// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
)

// This file is the INTEGRITY half of the field-level PATCH contract (#183).
//
// The credential gate decides who may change a credential, but a PATCH is a
// read-modify-write: it loads the account, applies fields in Go and writes the row
// back. If an OPERATE-only PATCH (name, status, model_prefix: the fields a CM of a
// `shared` account may send) wrote the WHOLE row back, it would carry the
// credential columns of its stale copy with it -- and a CM at `shared` could revert
// the owner's api key rotation, or a dispatch-time refresh-token rotation, without
// ever passing the credential gate. So an operate-only PATCH must write only the
// operate columns it was asked to change (plus updated_at), and never api_key,
// oauth_tokens or auth_type.

// racingRoutes is a routing.Store that lets a concurrent writer land BETWEEN a
// PATCH's load and its write: the first VendorAccountByID answers with the record
// as it was and only then runs the hook, so the PATCH goes on holding a stale copy
// of whatever the hook changed. It also counts full-row UpdateVendorAccount calls,
// the writer an operate-only PATCH must not use.
type racingRoutes struct {
	routing.Store

	mu         sync.Mutex
	after      func()
	fullWrites int
}

func (r *racingRoutes) VendorAccountByID(ctx context.Context, id string) (routing.VendorAccount, error) {
	acc, err := r.Store.VendorAccountByID(ctx, id)
	r.mu.Lock()
	hook := r.after
	r.after = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return acc, err
}

func (r *racingRoutes) UpdateVendorAccount(ctx context.Context, acc routing.VendorAccount) error {
	r.mu.Lock()
	r.fullWrites++
	r.mu.Unlock()
	return r.Store.UpdateVendorAccount(ctx, acc)
}

// interleaveAfterNextLoad arms the hook for the next VendorAccountByID.
func (r *racingRoutes) interleaveAfterNextLoad(hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.after = hook
}

func (r *racingRoutes) fullRowWrites() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fullWrites
}

// newVendorRacingEnv is the PATCH fixture with its service routed through a
// racingRoutes over the same memory store the test reads and writes directly.
func newVendorRacingEnv(t *testing.T) (*vendorAuthzEnv, *racingRoutes) {
	t.Helper()
	e := newVendorPatchEnv(t)
	racing := &racingRoutes{Store: e.routes}
	e.svc.routes = racing
	return e, racing
}

// TestOperateOnlyPatchNeverRevertsAConcurrentCredentialWrite interleaves, between
// an operate-only PATCH's load and its write, the writes that really happen to a
// live account (the owner's api key rotation, a dispatch-time refresh-token
// rotation, the needs_reconnect flip), then lets the PATCH finish and asserts that
// none of them was reverted by the stale copy. It runs as a CM of a `shared`
// account (who may never write a credential column) and as the owner.
func TestOperateOnlyPatchNeverRevertsAConcurrentCredentialWrite(t *testing.T) {
	e, racing := newVendorRacingEnv(t)
	ctx := context.Background()

	rotateKey := func(id string) func() {
		return func() {
			cur, err := e.routes.VendorAccountByID(ctx, id)
			if err != nil {
				t.Errorf("interleave: load: %v", err)
				return
			}
			cur.APIKey = "plain:" + patchNewKey
			if err := e.routes.UpdateVendorAccount(ctx, cur); err != nil {
				t.Errorf("interleave: rotate key: %v", err)
			}
		}
	}
	rotateTokens := func(id string) func() {
		return func() {
			if err := e.routes.SetVendorAccountOAuthTokens(ctx, id, "plain:rotated-token-set"); err != nil {
				t.Errorf("interleave: rotate tokens: %v", err)
			}
		}
	}
	needsReconnect := func(id string) func() {
		return func() {
			if err := e.routes.SetVendorAccountStatus(ctx, id, routing.VendorAccountStatusNeedsReconnect); err != nil {
				t.Errorf("interleave: needs_reconnect: %v", err)
			}
		}
	}

	scenarios := []struct {
		name       string
		newAccount func(level string) routing.VendorAccount
		interleave func(id string) func()
		req        UpdateVendorAccountRequest
		check      func(t *testing.T, got routing.VendorAccount)
	}{
		{
			name: "status change vs the owner's api key rotation", newAccount: e.keyedAccountAt, interleave: rotateKey,
			req: UpdateVendorAccountRequest{Status: strPtr(routing.VendorAccountStatusDisabled)},
			check: func(t *testing.T, got routing.VendorAccount) {
				if got.APIKey != "plain:"+patchNewKey {
					t.Errorf("api_key = %q, want the rotated key kept", got.APIKey)
				}
				if got.Status != routing.VendorAccountStatusDisabled {
					t.Errorf("status = %q, want the PATCH applied", got.Status)
				}
			},
		},
		{
			name: "rename vs a refresh-token rotation", newAccount: e.connectedSubscriptionAt, interleave: rotateTokens,
			req: UpdateVendorAccountRequest{Name: strPtr("Renamed by the CM")},
			check: func(t *testing.T, got routing.VendorAccount) {
				if got.OAuthTokens != "plain:rotated-token-set" {
					t.Errorf("oauth_tokens = %q, want the rotated set kept", got.OAuthTokens)
				}
				if got.Name != "Renamed by the CM" {
					t.Errorf("name = %q, want the PATCH applied", got.Name)
				}
			},
		},
		{
			name: "model prefix vs a refresh-token rotation", newAccount: e.connectedSubscriptionAt, interleave: rotateTokens,
			req: UpdateVendorAccountRequest{ModelPrefix: strPtr("cm-")},
			check: func(t *testing.T, got routing.VendorAccount) {
				if got.OAuthTokens != "plain:rotated-token-set" {
					t.Errorf("oauth_tokens = %q, want the rotated set kept", got.OAuthTokens)
				}
				if got.ModelPrefix != "cm-" {
					t.Errorf("model_prefix = %q, want the PATCH applied", got.ModelPrefix)
				}
			},
		},
		{
			name: "rename vs the needs_reconnect flip", newAccount: e.connectedSubscriptionAt, interleave: needsReconnect,
			req: UpdateVendorAccountRequest{Name: strPtr("Renamed by the CM")},
			check: func(t *testing.T, got routing.VendorAccount) {
				if got.Status != routing.VendorAccountStatusNeedsReconnect {
					t.Errorf("status = %q, want needs_reconnect kept (a rename does not write the status)", got.Status)
				}
				if got.Name != "Renamed by the CM" {
					t.Errorf("name = %q, want the PATCH applied", got.Name)
				}
			},
		},
		{
			// A client that round-trips the account it read sends the status it saw.
			// That equals the loaded value, so it is no change and must not be written
			// back over the needs_reconnect the dispatcher set in the meantime.
			name: "status echo vs the needs_reconnect flip", newAccount: e.connectedSubscriptionAt, interleave: needsReconnect,
			req: UpdateVendorAccountRequest{Status: strPtr(routing.VendorAccountStatusActive), Name: strPtr("Renamed by the CM")},
			check: func(t *testing.T, got routing.VendorAccount) {
				if got.Status != routing.VendorAccountStatusNeedsReconnect {
					t.Errorf("status = %q, want needs_reconnect kept (an echoed status is not written)", got.Status)
				}
			},
		},
	}

	principals := []struct {
		name  string
		level string
		tok   auth.Token
	}{
		{"CM at shared", routing.VendorShareLevelShared, token("usr_cm", "admin")},
		{"CM at full", routing.VendorShareLevelFull, token("usr_cm", "admin")},
		{"owner", routing.VendorShareLevelNone, token(vendorAuthzOwnerID)},
	}
	for _, sc := range scenarios {
		for _, p := range principals {
			label := sc.name + "/" + p.name
			acc := sc.newAccount(p.level)
			before := racing.fullRowWrites()
			racing.interleaveAfterNextLoad(sc.interleave(acc.ID))

			if err := e.patch(p.tok, acc.ID, sc.req); err != nil {
				t.Errorf("%s: PATCH: %v", label, err)
				continue
			}
			sc.check(t, e.reload(acc.ID))
			if got := racing.fullRowWrites() - before; got != 0 {
				t.Errorf("%s: an operate-only PATCH made %d full-row writes, want 0 (it must write only the operate columns)", label, got)
			}
		}
	}
}

// TestCredentialPatchStillWritesTheCredential is the control for the test above: a
// PATCH that carries the api key (credential gate) is the one that may write the
// credential columns, and it still does.
func TestCredentialPatchStillWritesTheCredential(t *testing.T) {
	e, racing := newVendorRacingEnv(t)
	acc := e.keyedAccountAt(routing.VendorShareLevelFull)
	if err := e.patch(token("usr_cm", "admin"), acc.ID, UpdateVendorAccountRequest{APIKey: strPtr(patchNewKey), Status: strPtr(routing.VendorAccountStatusDisabled)}); err != nil {
		t.Fatalf("CM at full PATCH with the key: %v", err)
	}
	got := e.reload(acc.ID)
	if got.APIKey != "plain:"+patchNewKey || got.Status != routing.VendorAccountStatusDisabled {
		t.Fatalf("stored key/status = %q/%q, want both applied", got.APIKey, got.Status)
	}
	if racing.fullRowWrites() == 0 {
		t.Fatal("a credential PATCH wrote no full row; the credential columns went nowhere")
	}
}
