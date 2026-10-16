// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
)

// This file pins the vendor-account opt-in on SERVICE tokens (#183, spec 2026-10-15
// sections 3 and 8). It is new plumbing: CreateServiceTokenRequest and ServiceTokenDTO
// carry vendor_access, CreateServiceToken writes it, RotateServiceToken preserves it,
// and it is CREATE-ONLY (a service token has no field-level update, so changing the
// opt-in means recreating the token). A service token reaches a shared account only
// when its SERVICE is provisioned into a containing resource group (the grant, not
// changed here) and the token names the account; reach is asked of the service, never
// of the admin who happens to be creating the token.

// serviceTokenEnv is the vendor listing fixture plus the two routing.Service records
// the grant's service principals stand for (grantServiceOK is provisioned into the
// granted resource group, grantServiceOther is provisioned nowhere) and the token
// service that can mint tokens for them.
type serviceTokenEnv struct {
	*vendorGrantEnv
	tokens *Service
	// reachingAdmin is a system admin who personally reaches every granted shared
	// account (provisioned directly into the granted group); plainAdmin reaches none.
	reachingAdmin, plainAdmin auth.Token
}

func newServiceTokenEnv(t *testing.T) *serviceTokenEnv {
	t.Helper()
	e := &serviceTokenEnv{vendorGrantEnv: newVendorListingEnv(t)}
	e.tokens = e.tokenService()
	for _, id := range []string{grantServiceOK, grantServiceOther} {
		if err := e.routes.CreateService(e.ctx, routing.Service{
			ID: id, Name: id, Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
		}); err != nil {
			t.Fatalf("CreateService(%s): %v", id, err)
		}
	}
	// plainAdmin is in no group at all (usr_s created the fixture's admin group and is
	// a member of it, so it would reach the shared accounts through that group).
	e.createUser("usr_plain_admin", "system_admin")
	admin := []string{"gateway:use", "admin", "system"}
	e.reachingAdmin = auth.Token{UserID: grantDirect, Scopes: admin}
	e.plainAdmin = auth.Token{UserID: "usr_plain_admin", Scopes: admin}
	return e
}

// lookup is the live auth.Token the gateway resolves a service token's secret to.
func (e *serviceTokenEnv) lookup(t *testing.T, secret string) auth.Token {
	t.Helper()
	tok, ok := e.dir.auth.LookupBearer("Bearer " + secret)
	if !ok {
		t.Fatalf("LookupBearer(%q): not found", secret)
	}
	if !tok.IsService() {
		t.Fatalf("looked-up token %+v is not a service token", tok)
	}
	return tok
}

func namedServiceAccess(ids ...string) *VendorAccessDTO {
	d := &VendorAccessDTO{}
	for _, id := range ids {
		d.Accounts = append(d.Accounts, VendorAccessEntryDTO{AccountID: id})
	}
	return d
}

// TestServiceTokenVendorAccessCreateWritesTheOptInAndRoutes: a service token created
// with an entry for a shared account its service is provisioned into stores the policy,
// reads it back, and routes to the account (under the prefix override) end to end.
func TestServiceTokenVendorAccessCreateWritesTheOptInAndRoutes(t *testing.T) {
	e := newServiceTokenEnv(t)
	r := e.liveResolver(e.svc)

	resp, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
		Name: "svc-shared",
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: grantShared, PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "svc/"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	if got := resp.Token.VendorAccess; got == nil || len(got.Accounts) != 1 || got.Accounts[0].AccountID != grantShared ||
		got.Accounts[0].PrefixOverride == nil || got.Accounts[0].PrefixOverride.Value != "svc/" {
		t.Fatalf("create response policy = %+v", got)
	}
	rec, err := e.dir.TokenByID(e.ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if want := `{"accounts":[{"account_id":"` + grantShared + `","prefix_override":{"enabled":true,"value":"svc/"}}]}`; rec.VendorProviderAccess != want {
		t.Fatalf("stored column = %s, want %s", rec.VendorProviderAccess, want)
	}

	list, err := e.tokens.ListServiceTokens(e.ctx, e.plainAdmin, grantServiceOK)
	if err != nil || len(list) != 1 || list[0].VendorAccess == nil || list[0].VendorAccess.Accounts[0].AccountID != grantShared {
		t.Fatalf("ListServiceTokens = %+v, err=%v, want the policy read back", list, err)
	}

	tok := e.lookup(t, resp.Secret)
	e.requireResolves("service token, named shared account under its override", r, tok, "svc/"+grantModel(grantShared), grantShared)
	e.requireNoRoute("service token, native name under an override", r, tok, grantModel(grantShared))
	e.requireNoRoute("service token, reachable but not named", r, tok, grantModel(grantFull))
	e.requireAdvertisedEqualsRoutable("service token, named shared account", e.svc, tok)
}

// TestServiceTokenVendorAccessAllTrueReachesNothing: a service owns no vendor account,
// so all:true on a service token covers nothing -- it never auto-borrows the shared
// accounts its service is provisioned into.
func TestServiceTokenVendorAccessAllTrueReachesNothing(t *testing.T) {
	e := newServiceTokenEnv(t)
	r := e.liveResolver(e.svc)
	resp, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
		Name: "svc-all", VendorAccess: &VendorAccessDTO{All: true},
	})
	if err != nil {
		t.Fatalf("CreateServiceToken(all:true): %v", err)
	}
	tok := e.lookup(t, resp.Secret)
	if !tok.VendorAccess.All {
		t.Fatalf("looked-up policy = %+v, want all:true carried", tok.VendorAccess)
	}
	for _, id := range grantedShared {
		e.requireNoRoute("service all:true "+id, r, tok, grantModel(id))
	}
	e.requireAdvertisedNames("service all:true", e.svc, tok, nil)
}

// TestServiceTokenVendorAccessReachIsTheServicesNotTheAdmins: the save-time reach check
// is computed against the SERVICE identity. An admin who personally reaches the shared
// accounts cannot lend that reach to a token of a service that is provisioned nowhere,
// and an admin with no reach can still mint a token for a provisioned service.
func TestServiceTokenVendorAccessReachIsTheServicesNotTheAdmins(t *testing.T) {
	e := newServiceTokenEnv(t)
	if got := e.routable(e.svc, e.reachingAdmin); len(got) == 0 {
		t.Fatalf("fixture: the reaching admin reaches nothing (%v); the test would prove nothing", got)
	}

	// The admin's reach does not carry over to a service provisioned nowhere.
	_, err := e.tokens.CreateServiceToken(e.ctx, e.reachingAdmin, grantServiceOther, CreateServiceTokenRequest{
		Name: "lent-reach", VendorAccess: namedServiceAccess(grantShared),
	})
	if !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("CreateServiceToken(unprovisioned service, reaching admin) = %v, want ErrTokenVendorAccessInvalid", err)
	}

	// The admin's lack of reach does not stop a provisioned service.
	if got := e.routable(e.svc, e.plainAdmin); len(got) != 0 {
		t.Fatalf("fixture: the plain admin reaches %v, want nothing", got)
	}
	if _, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
		Name: "own-reach", VendorAccess: namedServiceAccess(grantShared),
	}); err != nil {
		t.Fatalf("CreateServiceToken(provisioned service, plain admin): %v", err)
	}

	// Accounts the grant does not reach for the service are refused like unknown ids.
	for name, id := range map[string]string{
		"private, no link":              grantPrivate,
		"none with a surviving link":    grantStale,
		"shared, group not provisioned": grantOtherRG,
	} {
		if _, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
			Name: "u-" + id, VendorAccess: namedServiceAccess(id),
		}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Errorf("%s: err = %v, want ErrTokenVendorAccessInvalid", name, err)
		}
	}

	// A malformed prefix and a colliding pair are refused as for a user token.
	bad := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: grantShared, PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "no spaces"}}}}
	if _, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{Name: "bad-prefix", VendorAccess: bad}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Errorf("bad prefix: err = %v, want ErrTokenVendorAccessInvalid", err)
	}
	if _, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
		Name: "clash", VendorAccess: namedServiceAccess(grantShared, grantFull),
	}); !errors.Is(err, ErrTokenVendorAccessConflict) {
		t.Errorf("colliding pair: err = %v, want ErrTokenVendorAccessConflict", err)
	}
}

// TestServiceTokenVendorAccessRotatePreservesTheOptIn: rotating the secret keeps the
// policy: the DTO, the stored column and the live bearer under the NEW secret all carry
// it, the old secret stops resolving, and the rotated token still routes.
func TestServiceTokenVendorAccessRotatePreservesTheOptIn(t *testing.T) {
	e := newServiceTokenEnv(t)
	r := e.liveResolver(e.svc)
	created, err := e.tokens.CreateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, CreateServiceTokenRequest{
		Name: "rotating", VendorAccess: namedServiceAccess(grantFull),
	})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	before, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}

	rotated, err := e.tokens.RotateServiceToken(e.ctx, e.plainAdmin, grantServiceOK, created.Token.ID)
	if err != nil {
		t.Fatalf("RotateServiceToken: %v", err)
	}
	if !reflect.DeepEqual(rotated.Token.VendorAccess, created.Token.VendorAccess) {
		t.Fatalf("rotate response policy = %+v, want %+v", rotated.Token.VendorAccess, created.Token.VendorAccess)
	}
	after, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID after rotate: %v", err)
	}
	if after.VendorProviderAccess == "" || after.VendorProviderAccess != before.VendorProviderAccess {
		t.Fatalf("stored column after rotate = %q, want %q unchanged", after.VendorProviderAccess, before.VendorProviderAccess)
	}
	if _, ok := e.dir.auth.LookupBearer("Bearer " + created.Secret); ok {
		t.Fatal("the pre-rotation secret still resolves")
	}
	tok := e.lookup(t, rotated.Secret)
	e.requireResolves("rotated token", r, tok, grantModel(grantFull), grantFull)
	e.requireNoRoute("rotated token, not named", r, tok, grantModel(grantShared))
}

// TestServiceTokenVendorAccessWireShapeMirrorsTheUserToken: the field is the user
// token's `vendor_access` on request and response, omitted while the token has none.
func TestServiceTokenVendorAccessWireShapeMirrorsTheUserToken(t *testing.T) {
	var req CreateServiceTokenRequest
	if err := json.Unmarshal([]byte(`{"name":"n","vendor_access":{"all":false,"accounts":[{"account_id":"a","prefix_override":{"enabled":true,"value":"x/"}}]}}`), &req); err != nil {
		t.Fatalf("Unmarshal request: %v", err)
	}
	if req.VendorAccess == nil || len(req.VendorAccess.Accounts) != 1 || req.VendorAccess.Accounts[0].PrefixOverride == nil {
		t.Fatalf("request policy = %+v", req.VendorAccess)
	}

	raw, err := json.Marshal(ServiceTokenDTO{ID: "t"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "vendor_access") {
		t.Errorf("a token with no policy serialized vendor_access: %s", raw)
	}
	raw, err = json.Marshal(ServiceTokenDTO{ID: "t", VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "a"}}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"vendor_access":{"all":false,"accounts":[{"account_id":"a"}]}`) {
		t.Errorf("DTO JSON = %s, want the user token's vendor_access shape", raw)
	}
}

// TestServiceTokenVendorAccessIsCreateOnly pins the model the spec states: a service
// token has no field-level update, so its vendor opt-in changes only by recreating the
// token. If an update method is ever added, the opt-in needs the same reach validation
// on it, so this fails to force that decision to be made deliberately.
func TestServiceTokenVendorAccessIsCreateOnly(t *testing.T) {
	api := reflect.TypeOf((*API)(nil)).Elem()
	for i := 0; i < api.NumMethod(); i++ {
		name := api.Method(i).Name
		if strings.Contains(name, "ServiceToken") && strings.HasPrefix(name, "Update") {
			t.Errorf("API.%s exists: a service token's vendor opt-in is create-only, so an update path must validate it too", name)
		}
	}
	// Nothing on the service-token request type besides create's own fields can carry
	// a policy change: there is no update request type.
	if _, found := reflect.TypeOf(CreateServiceTokenRequest{}).FieldByName("VendorAccess"); !found {
		t.Error("CreateServiceTokenRequest has no VendorAccess field")
	}
}
