// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"strings"
	"testing"
)

// newTokenVendorAccessFixture is the token fixture plus three vendor accounts in
// the routing store: acc_a (prefix "a/") and acc_b (prefix "b/") belong to the
// token owner (usr_1) and each serve upstream model gpt-4o; acc_other belongs to
// somebody else and also serves gpt-4o. Gateway ids carry the account's native
// prefix, as the discovery path stores them.
func newTokenVendorAccessFixture(t *testing.T) *tokenSettingsFixture {
	t.Helper()
	fx := newTokenSettingsFixture(t)
	ctx := context.Background()
	seed := []struct{ id, owner, prefix string }{
		{"acc_a", "usr_1", "a/"},
		{"acc_b", "usr_1", "b/"},
		{"acc_other", "usr_2", "o/"},
	}
	for _, s := range seed {
		if err := fx.rs.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: s.id, OwnerUserID: s.owner, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
			Name: s.id, Status: routing.VendorAccountStatusActive, ModelPrefix: s.prefix,
			CreatedAt: offeringTime, UpdatedAt: offeringTime,
		}); err != nil {
			t.Fatalf("CreateVendorAccount %s: %v", s.id, err)
		}
		if err := fx.rs.SetVendorAccountModels(ctx, s.id, []routing.VendorAccountModel{
			{GatewayModel: s.prefix + "gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI},
		}); err != nil {
			t.Fatalf("SetVendorAccountModels %s: %v", s.id, err)
		}
	}
	return fx
}

func TestCreateTokenVendorAccessValidation(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		va   *VendorAccessDTO
		want error
	}{
		{
			name: "unknown account id",
			va:   &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_not_mine"}}},
			want: ErrTokenVendorAccessInvalid,
		},
		{
			// An account that exists but is owned by someone else must be refused
			// exactly like an unknown id (no cross-owner grant, no existence leak).
			name: "foreign-owned account id",
			va:   &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_other"}}},
			want: ErrTokenVendorAccessInvalid,
		},
		{
			name: "blank account id",
			va:   &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "   "}}},
			want: ErrTokenVendorAccessInvalid,
		},
		{
			name: "bad prefix",
			va: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{
				AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "bad prefix with spaces"},
			}}},
			want: ErrTokenVendorAccessInvalid,
		},
		{
			// Two entries for one account would carry two competing prefixes, of
			// which only one can be effective; refused rather than guessed at.
			name: "duplicate account id",
			va:   &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}, {AccountID: "acc_a"}}},
			want: ErrTokenVendorAccessInvalid,
		},
		{
			// Both accounts overridden to the empty prefix: both would be
			// advertised as the bare "gpt-4o".
			name: "collision on empty prefixes",
			va: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
				{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
				{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			}},
			want: ErrTokenVendorAccessConflict,
		},
		{
			name: "collision on the same custom prefix",
			va: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
				{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "x/"}},
				{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: " x/ "}},
			}},
			want: ErrTokenVendorAccessConflict,
		},
		{
			// An override that lands on the other account's NATIVE prefix collides
			// too: effective names are compared, not override flags.
			name: "collision of an override with a native prefix",
			va: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
				{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "b/"}},
				{AccountID: "acc_b"},
			}},
			want: ErrTokenVendorAccessConflict,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{
				Name: "rejected-" + string(rune('a'+i)), Scopes: []string{"gateway:use"}, VendorAccess: tc.va,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// None of the rejected creates may have left a token behind.
	list, err := fx.svc.ListTokens(ctx, fx.owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, tok := range list.Data {
		if !tok.IsChatSession {
			t.Fatalf("a rejected create persisted token %q", tok.Name)
		}
	}
}

func TestCreateTokenVendorAccessRoundTrip(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()

	// An empty-value override on acc_a ("no prefix") plus acc_b's native prefix
	// yields the distinct names "gpt-4o" and "b/gpt-4o": no collision.
	resp, err := fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{
		Name: "t4", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: " acc_a ", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			{AccountID: "acc_b"},
		}},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	want := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
		{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
		{AccountID: "acc_b"},
	}}
	if !reflect.DeepEqual(resp.Token.VendorAccess, want) {
		t.Fatalf("create DTO = %+v, want %+v", resp.Token.VendorAccess, want)
	}

	// The record carries the encoded policy and the listing renders it back.
	rec, err := fx.dir.TokenByID(ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got := store.DecodeVendorAccess(rec.VendorProviderAccess); len(got.Accounts) != 2 || got.Accounts[0].AccountID != "acc_a" || !got.Accounts[0].OverrideEnabled {
		t.Fatalf("stored policy = %+v (raw %q)", got, rec.VendorProviderAccess)
	}
	list, err := fx.svc.ListTokens(ctx, fx.owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var listed *VendorAccessDTO
	for _, tok := range list.Data {
		if tok.ID == resp.Token.ID {
			listed = tok.VendorAccess
		}
	}
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("listed DTO = %+v, want %+v", listed, want)
	}
}

// A switched-off override is not an override: its value is neither validated nor
// stored, and the account keeps its own prefix (so acc_a/acc_b do not collide
// here even though the disabled values would be invalid / identical).
func TestCreateTokenVendorAccessDisabledOverrideIsInert(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	resp, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
		Name: "inert", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: false, Value: "bad prefix"}},
			{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: false, Value: ""}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	want := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}, {AccountID: "acc_b"}}}
	if !reflect.DeepEqual(resp.Token.VendorAccess, want) {
		t.Fatalf("DTO = %+v, want %+v", resp.Token.VendorAccess, want)
	}
}

func TestCreateTokenVendorAccessStrictDefault(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	resp, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{Name: "plain", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if resp.Token.VendorAccess != nil {
		t.Fatalf("default DTO = %+v, want nil (strict: no vendor access)", resp.Token.VendorAccess)
	}
	rec, err := fx.dir.TokenByID(context.Background(), resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.VendorProviderAccess != "" {
		t.Fatalf("stored policy = %q, want the empty strict default", rec.VendorProviderAccess)
	}
}

// With all=true the account list is ignored by the resolver, so it is not
// validated or collision-checked: the SAME pair that conflicts under all=false
// (see TestCreateTokenVendorAccessValidation, "collision on empty prefixes") must
// still save, and a foreign id riding along is stored inert. Sending the colliding
// pair (not an empty list) is what makes this guard the all=true short-circuit.
func TestCreateTokenVendorAccessAllSkipsPerAccountChecks(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	pair := []VendorAccessEntryDTO{
		{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
		{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
	}
	// The same pair under all=false conflicts: the skip below is the only reason
	// the all=true create succeeds.
	_, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
		Name: "not-all", Scopes: []string{"gateway:use"}, VendorAccess: &VendorAccessDTO{Accounts: pair},
	})
	if !errors.Is(err, ErrTokenVendorAccessConflict) {
		t.Fatalf("all=false control: err = %v, want ErrTokenVendorAccessConflict", err)
	}

	for _, tc := range []struct {
		name     string
		accounts []VendorAccessEntryDTO
	}{
		{"colliding pair", pair},
		{"foreign id", []VendorAccessEntryDTO{{AccountID: "acc_other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
				Name: "all-" + tc.name, Scopes: []string{"gateway:use"},
				VendorAccess: &VendorAccessDTO{All: true, Accounts: tc.accounts},
			})
			if err != nil {
				t.Fatalf("CreateToken: %v", err)
			}
			if resp.Token.VendorAccess == nil || !resp.Token.VendorAccess.All {
				t.Fatalf("DTO = %+v, want all=true", resp.Token.VendorAccess)
			}
		})
	}
}

// An all=true policy with no explicit accounts must serialize its list as an
// array, not null: the frontend iterates it unconditionally.
func TestVendorAccessDTOAllSerializesEmptyAccountsArray(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	resp, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
		Name: "all-json", Scopes: []string{"gateway:use"}, VendorAccess: &VendorAccessDTO{All: true},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	raw, err := json.Marshal(resp.Token.VendorAccess)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, want := string(raw), `{"all":true,"accounts":[]}`; got != want {
		t.Fatalf("vendor_access JSON = %s, want %s", got, want)
	}
	// Encode/decode stay symmetric: the empty array re-encodes to the same column.
	if col := encodeVendorAccessDTO(resp.Token.VendorAccess); col != `{"all":true}` {
		t.Fatalf("re-encoded column = %q, want %q", col, `{"all":true}`)
	}
}

// failingVendorReadsStore makes the two per-owner vendor-account reads the token
// validator performs fail on demand, to pin how each failure is classified.
type failingVendorReadsStore struct {
	*routing.MemoryStore
	ownerErr  error // VendorAccountsByOwner
	modelsErr error // VendorAccountModels
}

func (f *failingVendorReadsStore) VendorAccountsByOwner(ctx context.Context, userID string) ([]routing.VendorAccount, error) {
	if f.ownerErr != nil {
		return nil, f.ownerErr
	}
	return f.MemoryStore.VendorAccountsByOwner(ctx, userID)
}

func (f *failingVendorReadsStore) VendorAccountModels(ctx context.Context, accountID string) ([]routing.VendorAccountModel, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.MemoryStore.VendorAccountModels(ctx, accountID)
}

// A failing owner-account read is an infrastructure error, not a client mistake:
// it must come back raw (the endpoints turn it into a 500), never wrapped as
// ErrTokenVendorAccessInvalid (a 400), on create and on update alike.
func TestVendorAccessOwnerReadFailureIsNotAClientError(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()
	created, err := fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{Name: "base", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	boom := errors.New("owner read boom")
	fx.svc.routes = &failingVendorReadsStore{MemoryStore: fx.rs, ownerErr: boom}
	policy := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}}}

	_, err = fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{Name: "c", Scopes: []string{"gateway:use"}, VendorAccess: policy})
	if !errors.Is(err, boom) || errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("create: err = %v, want the raw read error and not ErrTokenVendorAccessInvalid", err)
	}
	_, err = fx.svc.UpdateToken(ctx, fx.owner, created.Token.ID, UpdateTokenRequest{VendorAccess: policy})
	if !errors.Is(err, boom) || errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("update: err = %v, want the raw read error and not ErrTokenVendorAccessInvalid", err)
	}
}

// A failing per-account models read stays fail-open (the resolver backstop covers
// a collision the check could not see) but is no longer silent: the skip is
// logged with the account id.
func TestVendorAccessModelsReadFailureFailsOpenAndLogs(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	fx.svc.routes = &failingVendorReadsStore{MemoryStore: fx.rs, modelsErr: errors.New("models read boom")}

	// The pair would conflict if the models could be read; unreadable models
	// cannot be checked, so the save goes through.
	var createErr error
	logged := captureSlog(t, func() {
		_, createErr = fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
			Name: "failopen", Scopes: []string{"gateway:use"},
			VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
				{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
				{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			}},
		})
	})
	if createErr != nil {
		t.Fatalf("CreateToken: %v, want fail-open", createErr)
	}
	for _, id := range []string{"acc_a", "acc_b"} {
		if !strings.Contains(logged, id) || !strings.Contains(logged, "models read boom") {
			t.Fatalf("log does not name %s and the cause:\n%s", id, logged)
		}
	}
}

func TestCreateTokenVendorAccessWithoutRoutesIsInvalid(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	fx.svc.routes = nil
	_, err := fx.svc.CreateToken(context.Background(), fx.owner, CreateTokenRequest{
		Name: "noroutes", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}}},
	})
	if !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("err = %v, want ErrTokenVendorAccessInvalid", err)
	}
}

func TestUpdateTokenVendorAccess(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()
	created, err := fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{
		Name: "upd", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}}},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	id := created.Token.ID
	selectA := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a"}}}

	// nil = keep the stored policy.
	newName := "upd2"
	dto, err := fx.svc.UpdateToken(ctx, fx.owner, id, UpdateTokenRequest{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateToken(name only): %v", err)
	}
	if !reflect.DeepEqual(dto.VendorAccess, selectA) {
		t.Fatalf("policy after an unrelated update = %+v, want %+v", dto.VendorAccess, selectA)
	}

	// A conflicting policy is refused and leaves the stored one untouched.
	_, err = fx.svc.UpdateToken(ctx, fx.owner, id, UpdateTokenRequest{VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
		{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
		{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
	}}})
	if !errors.Is(err, ErrTokenVendorAccessConflict) {
		t.Fatalf("conflict: err = %v, want ErrTokenVendorAccessConflict", err)
	}
	_, err = fx.svc.UpdateToken(ctx, fx.owner, id, UpdateTokenRequest{VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_other"}}}})
	if !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("foreign id: err = %v, want ErrTokenVendorAccessInvalid", err)
	}
	rec, err := fx.dir.TokenByID(ctx, id)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got := decodeVendorAccessDTO(rec.VendorProviderAccess); !reflect.DeepEqual(got, selectA) {
		t.Fatalf("stored policy after rejected updates = %+v, want %+v", got, selectA)
	}

	// A valid replacement is stored.
	dto, err = fx.svc.UpdateToken(ctx, fx.owner, id, UpdateTokenRequest{VendorAccess: &VendorAccessDTO{All: true}})
	if err != nil {
		t.Fatalf("UpdateToken(all): %v", err)
	}
	if dto.VendorAccess == nil || !dto.VendorAccess.All {
		t.Fatalf("policy after all=true = %+v", dto.VendorAccess)
	}

	// A non-nil empty policy is an explicit reset to the strict default.
	dto, err = fx.svc.UpdateToken(ctx, fx.owner, id, UpdateTokenRequest{VendorAccess: &VendorAccessDTO{}})
	if err != nil {
		t.Fatalf("UpdateToken(empty): %v", err)
	}
	if dto.VendorAccess != nil {
		t.Fatalf("policy after reset = %+v, want nil", dto.VendorAccess)
	}
	if rec, err = fx.dir.TokenByID(ctx, id); err != nil || rec.VendorProviderAccess != "" {
		t.Fatalf("stored policy after reset = %q (err %v), want empty", rec.VendorProviderAccess, err)
	}
}

// The wire "enabled" flag is honored in both directions: an override object with
// enabled=false is NOT an active override (presence AND enabled <=> override on),
// and the owner-supplied account id is trimmed on the way in.
func TestVendorAccessDTOCodec(t *testing.T) {
	enc := encodeVendorAccessDTO(&VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
		{AccountID: "  acc_a  ", PrefixOverride: &PrefixOverrideDTO{Enabled: false, Value: "x/"}},
		{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "y/"}},
	}})
	got := store.DecodeVendorAccess(enc)
	if len(got.Accounts) != 2 {
		t.Fatalf("decoded %+v from %q, want 2 accounts", got, enc)
	}
	if got.Accounts[0].AccountID != "acc_a" {
		t.Fatalf("account id = %q, want the trimmed %q", got.Accounts[0].AccountID, "acc_a")
	}
	if got.Accounts[0].OverrideEnabled {
		t.Fatalf("enabled=false override encoded as active: %+v", got.Accounts[0])
	}
	if !got.Accounts[1].OverrideEnabled || got.Accounts[1].OverridePrefix != "y/" {
		t.Fatalf("enabled override lost: %+v", got.Accounts[1])
	}

	if encodeVendorAccessDTO(nil) != "" || encodeVendorAccessDTO(&VendorAccessDTO{}) != "" {
		t.Fatal("nil / empty DTO must encode to the empty strict default")
	}
	if decodeVendorAccessDTO("") != nil || decodeVendorAccessDTO("not json") != nil {
		t.Fatal("blank / malformed column must decode to a nil DTO")
	}

	// A stored enabled=false override reads back as no override on the DTO.
	dto := decodeVendorAccessDTO(`{"accounts":[{"account_id":"acc_a","prefix_override":{"enabled":false,"value":"x/"}}]}`)
	if dto == nil || len(dto.Accounts) != 1 || dto.Accounts[0].PrefixOverride != nil {
		t.Fatalf("decoded DTO = %+v, want one account without a prefix override", dto)
	}
}
