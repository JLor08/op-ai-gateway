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

// This file pins the RESOLVER half of the shared-vendor-account grant (#183, spec
// 2026-10-15 section 8): how resolveVendorAccount consumes the VendorAccountEnumerator
// seam. The grant itself (which accounts a principal may route to: own, then live
// shared/full in a provisioned resource group) is portal.Service.VendorAccountsForRouting
// and is pinned there, end to end through a real Resolver. Routing cannot import portal,
// so these tests use a fake enumerator and prove only what the resolver owns:
//
//   - the seam REPLACES the owner-only read, and the resolver iterates it in the order
//     it is given (own block first, so an own account wins a same-name collision);
//   - the ELIGIBILITY guard: a principal with neither a user nor a service identity never
//     reaches the seam, whatever the seam would answer;
//   - a nil seam is OWNER-ONLY, never allow-all (the inverse of ProvisioningGate's nil).

// fakeVendorEnumerator answers every principal with the same accounts and records who
// asked, so a test can assert both what the resolver did with the answer and whether it
// asked at all.
type fakeVendorEnumerator struct {
	accounts []VendorAccount
	err      error
	asked    []auth.Token
}

func (f *fakeVendorEnumerator) VendorAccountsForRouting(_ context.Context, principal auth.Token) ([]VendorAccount, error) {
	f.asked = append(f.asked, principal)
	return f.accounts, f.err
}

// spyOwnerStore records every owner id the resolver's own-accounts read is asked for,
// so a test can prove the nil-seam fallback never enumerates by an empty owner.
type spyOwnerStore struct {
	*MemoryStore
	owners []string
}

func (s *spyOwnerStore) VendorAccountsByOwner(ctx context.Context, userID string) ([]VendorAccount, error) {
	s.owners = append(s.owners, userID)
	return s.MemoryStore.VendorAccountsByOwner(ctx, userID)
}

const (
	sharingBorrower = "usr_borrower"
	sharingServiceA = "svc_a"
)

// optedIn is the explicit per-account policy: each named account opted in under its
// native prefix. A SHARED account is used only when the token's policy names it (all:true
// covers a principal's own accounts only), so a test whose borrower is to use an account
// the seam grants must name it; the seam decides REACH, the policy decides USE.
func optedIn(accountIDs ...string) auth.VendorAccess {
	access := auth.VendorAccess{}
	for _, id := range accountIDs {
		access.Accounts = append(access.Accounts, auth.VendorAccessEntry{AccountID: id})
	}
	return access
}

// borrowerToken is a user token (it owns none of the fixture's accounts) that opted in
// to the named accounts.
func borrowerToken(accountIDs ...string) auth.Token {
	return auth.Token{ID: "tok_borrower", UserID: sharingBorrower, Active: true, VendorAccess: optedIn(accountIDs...)}
}

// serviceToken is a service token that opted in to the named accounts.
func serviceToken(serviceID string, accountIDs ...string) auth.Token {
	return auth.Token{ID: "tok_svc", Kind: "service", ServiceID: serviceID, Active: true, VendorAccess: optedIn(accountIDs...)}
}

func sharingRequest() inference.Request {
	return inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"}
}

// seededAccounts seeds one account per id (all owned by vendorOwner, all serving
// gpt-4o) and returns them as the store holds them, in the order given.
func seededAccounts(t *testing.T, store *MemoryStore, now time.Time, ids ...string) []VendorAccount {
	t.Helper()
	out := make([]VendorAccount, 0, len(ids))
	for _, id := range ids {
		seedVendorAccount(t, store, now, id, vendorOwner, VendorOpenAI, "enc:key-"+id, "", "gpt-4o", APIFlavorOpenAI)
		acc, err := store.VendorAccountByID(context.Background(), id)
		if err != nil {
			t.Fatalf("VendorAccountByID(%s): %v", id, err)
		}
		out = append(out, acc)
	}
	return out
}

// TestVendorSharingSeamBorrowerResolvesSharedAccount: an account the seam returns for a
// principal who does not own it IS routed, and the Target carries the account's SEALED
// key verbatim (the cipher-holding dispatch edge opens it; a borrower never sees it).
func TestVendorSharingSeamBorrowerResolvesSharedAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	accounts := seededAccounts(t, store, now, "acc_shared")
	enum := &fakeVendorEnumerator{accounts: accounts}
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(enum)

	target, err := resolver.Resolve(ctx, borrowerToken("acc_shared"), sharingRequest())
	if err != nil {
		t.Fatalf("Resolve = %v, want the shared account's vendor target", err)
	}
	if target.VendorAccountID != "acc_shared" {
		t.Errorf("VendorAccountID = %q, want acc_shared", target.VendorAccountID)
	}
	if target.APIToken != "enc:key-acc_shared" {
		t.Errorf("APIToken = %q, want the sealed key carried verbatim", target.APIToken)
	}
	if len(enum.asked) != 1 || enum.asked[0].UserID != sharingBorrower {
		t.Errorf("seam asked %+v, want exactly one call for the borrower's token", enum.asked)
	}
}

// TestVendorSharingSeamEmptyAnswerDeniesEvenWhenStoreHoldsAccounts: the seam is the
// grant. The store holds an account the seam does not return, so the resolver must not
// find it by any other path.
func TestVendorSharingSeamEmptyAnswerDeniesEvenWhenStoreHoldsAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seededAccounts(t, store, now, "acc_private")
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{})

	if _, err := resolver.Resolve(ctx, borrowerToken("acc_private"), sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve = %v, want ErrNoModelRoute (the seam returned nothing)", err)
	}
	// The seam REPLACES the owner-only read: the owner is not resolved by a fallback
	// read either, so an enumerator that omits an own account is authoritative.
	owner := ownerToken()
	if _, err := resolver.Resolve(ctx, owner, sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(owner) = %v, want ErrNoModelRoute (the seam is the only source)", err)
	}
}

// TestVendorSharingSeamOwnBlockWinsNameCollision: the resolver iterates the seam's answer
// in order and the first active account serving the name wins. The enumerator emits the
// principal's OWN accounts first, so an own account beats a shared one serving the same
// name even when the shared account's id sorts first.
func TestVendorSharingSeamOwnBlockWinsNameCollision(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	// acc_a (shared, id sorts first) and acc_z (the borrower's own) both serve gpt-4o.
	all := seededAccounts(t, store, now, "acc_a", "acc_z")
	shared, own := all[0], all[1]

	t.Run("own first", func(t *testing.T) {
		resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
		resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: []VendorAccount{own, shared}})
		target, err := resolver.Resolve(ctx, borrowerToken("acc_a", "acc_z"), sharingRequest())
		if err != nil {
			t.Fatalf("Resolve = %v", err)
		}
		if target.VendorAccountID != "acc_z" {
			t.Errorf("VendorAccountID = %q, want the OWN account acc_z to win the collision", target.VendorAccountID)
		}
	})

	t.Run("among shared accounts the first by the seam's order wins", func(t *testing.T) {
		resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
		resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: []VendorAccount{shared, own}})
		target, err := resolver.Resolve(ctx, borrowerToken("acc_a", "acc_z"), sharingRequest())
		if err != nil {
			t.Fatalf("Resolve = %v", err)
		}
		if target.VendorAccountID != "acc_a" {
			t.Errorf("VendorAccountID = %q, want acc_a: the resolver serves the seam's order, first active wins", target.VendorAccountID)
		}
	})
}

// TestVendorSharingSeamErrorPropagates: a seam failure is a store-class failure, like
// the owner-only read's, not a silent no-route.
func TestVendorSharingSeamErrorPropagates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seededAccounts(t, store, now, "acc_shared")
	boom := errors.New("enumerator down")
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{err: boom})

	_, err := resolver.Resolve(ctx, borrowerToken("acc_shared"), sharingRequest())
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve = %v, want the seam's error propagated", err)
	}
}

// TestVendorSharingEligibilityGuard is the Review-Focus "eligibility" case. A principal
// is eligible for the vendor branch only with a user identity, or a SERVICE token with a
// service id. The seam here answers EVERY caller with an account, so a guard that merely
// dropped the UserID check (or let an empty principal through) would route; the seam must
// not even be asked.
func TestVendorSharingEligibilityGuard(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	accounts := seededAccounts(t, store, now, "acc_shared")

	denied := []struct {
		name  string
		token auth.Token
	}{
		{"no user and no service identity", auth.Token{ID: "tok_empty", Active: true, VendorAccess: auth.VendorAccess{All: true}}},
		{"service token without a service id", auth.Token{ID: "tok_svc", Kind: "service", Active: true, VendorAccess: auth.VendorAccess{All: true}}},
		{"service id on a token that is not a service token", auth.Token{ID: "tok_x", ServiceID: sharingServiceA, Active: true, VendorAccess: auth.VendorAccess{All: true}}},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			enum := &fakeVendorEnumerator{accounts: accounts}
			resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
			resolver.SetVendorAccountEnumerator(enum)
			if _, err := resolver.Resolve(ctx, tc.token, sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("Resolve = %v, want ErrNoModelRoute (an empty principal is not eligible)", err)
			}
			if len(enum.asked) != 0 {
				t.Errorf("seam was asked %d time(s) for an ineligible principal, want none", len(enum.asked))
			}
		})
	}

	t.Run("a service token with a service id is eligible", func(t *testing.T) {
		enum := &fakeVendorEnumerator{accounts: accounts}
		resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
		resolver.SetVendorAccountEnumerator(enum)
		target, err := resolver.Resolve(ctx, serviceToken(sharingServiceA, "acc_shared"), sharingRequest())
		if err != nil {
			t.Fatalf("Resolve = %v, want the account the seam granted the service", err)
		}
		if target.VendorAccountID != "acc_shared" {
			t.Errorf("VendorAccountID = %q, want acc_shared", target.VendorAccountID)
		}
		if len(enum.asked) != 1 || enum.asked[0].ServiceID != sharingServiceA {
			t.Errorf("seam asked %+v, want one call carrying the service id", enum.asked)
		}
	})
}

// TestVendorSharingNilSeamIsOwnerOnly: with no enumerator wired the resolver falls back
// to the principal's OWN accounts. It must never fall open to every account (the nil of
// ProvisioningGate), and it must not enumerate by an empty owner for a service token.
func TestVendorSharingNilSeamIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	memory := NewMemoryStore()
	seededAccounts(t, memory, now, "acc_owned")
	store := &spyOwnerStore{MemoryStore: memory}
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst) // no SetVendorAccountEnumerator

	t.Run("the owner still resolves their own account", func(t *testing.T) {
		target, err := resolver.Resolve(ctx, ownerToken(), sharingRequest())
		if err != nil {
			t.Fatalf("Resolve(owner) = %v, want the owner's own account", err)
		}
		if target.VendorAccountID != "acc_owned" {
			t.Errorf("VendorAccountID = %q, want acc_owned", target.VendorAccountID)
		}
	})

	t.Run("another user resolves nothing (not allow-all)", func(t *testing.T) {
		if _, err := resolver.Resolve(ctx, borrowerToken("acc_owned"), sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(borrower) = %v, want ErrNoModelRoute: a nil seam is owner-only, never allow-all", err)
		}
	})

	t.Run("a service token resolves nothing and never reads by an empty owner", func(t *testing.T) {
		store.owners = nil
		if _, err := resolver.Resolve(ctx, serviceToken(sharingServiceA, "acc_owned"), sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(service) = %v, want ErrNoModelRoute (a service owns no account)", err)
		}
		for _, owner := range store.owners {
			if owner == "" {
				t.Errorf("VendorAccountsByOwner was asked for the empty owner %q", owner)
			}
		}
	})
}

// TestVendorSharingOptInAllTrueNeverAutoBorrows is the Review-Focus all:true case at the
// resolver: the seam GRANTS the account (reach), but a principal that does not own it and
// holds only an all:true policy does not use it. all:true covers the principal's own
// accounts; a shared account must be named.
func TestVendorSharingOptInAllTrueNeverAutoBorrows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	accounts := seededAccounts(t, store, now, "acc_shared") // owned by vendorOwner
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: accounts})

	allTrue := auth.VendorAccess{All: true}
	for name, token := range map[string]auth.Token{
		"a user that does not own the account": {ID: "tok_b", UserID: sharingBorrower, Active: true, VendorAccess: allTrue},
		"a service token":                      {ID: "tok_s", Kind: "service", ServiceID: sharingServiceA, Active: true, VendorAccess: allTrue},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolver.Resolve(ctx, token, sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("Resolve = %v, want ErrNoModelRoute: all:true must not auto-borrow a shared account", err)
			}
		})
	}

	t.Run("the account's own owner is covered by all:true", func(t *testing.T) {
		target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_o", UserID: vendorOwner, Active: true, VendorAccess: allTrue}, sharingRequest())
		if err != nil || target.VendorAccountID != "acc_shared" {
			t.Fatalf("Resolve(owner, all:true) = (%q, %v), want acc_shared", target.VendorAccountID, err)
		}
	})

	t.Run("all:true plus a named shared account uses it", func(t *testing.T) {
		token := auth.Token{
			ID: "tok_b", UserID: sharingBorrower, Active: true,
			VendorAccess: auth.VendorAccess{All: true, Accounts: []auth.VendorAccessEntry{{AccountID: "acc_shared"}}},
		}
		target, err := resolver.Resolve(ctx, token, sharingRequest())
		if err != nil || target.VendorAccountID != "acc_shared" {
			t.Fatalf("Resolve = (%q, %v), want acc_shared through the explicit entry", target.VendorAccountID, err)
		}
	})
}

// TestVendorSharingOptInNamesAndPrefixOverride: a token that names a granted shared
// account uses it, only it, and under the override prefix when one is set; naming an
// account the seam did not grant routes nothing (the grant is the gate).
func TestVendorSharingOptInNamesAndPrefixOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	accounts := seededAccounts(t, store, now, "acc_a", "acc_b") // both serve gpt-4o
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: accounts[:1]}) // the seam grants acc_a only

	t.Run("a named granted account", func(t *testing.T) {
		target, err := resolver.Resolve(ctx, borrowerToken("acc_a"), sharingRequest())
		if err != nil || target.VendorAccountID != "acc_a" {
			t.Fatalf("Resolve = (%q, %v), want acc_a", target.VendorAccountID, err)
		}
	})

	t.Run("a named account the seam did not grant routes nothing", func(t *testing.T) {
		if _, err := resolver.Resolve(ctx, borrowerToken("acc_b"), sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve = %v, want ErrNoModelRoute (naming an ungranted account is harmless)", err)
		}
	})

	t.Run("the override prefix is the public name; the native name stops routing", func(t *testing.T) {
		token := borrowerToken()
		token.VendorAccess = auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: "team/"}}}
		target, err := resolver.Resolve(ctx, token, inference.Request{Model: "team/gpt-4o", APIFlavor: "openai_chat"})
		if err != nil || target.VendorAccountID != "acc_a" {
			t.Fatalf("Resolve(team/gpt-4o) = (%q, %v), want acc_a", target.VendorAccountID, err)
		}
		if _, err := resolver.Resolve(ctx, token, sharingRequest()); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(gpt-4o) = %v, want ErrNoModelRoute under an override prefix", err)
		}
	})
}
