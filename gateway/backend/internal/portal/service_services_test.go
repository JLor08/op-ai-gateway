// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// testServiceSystemGroupID/testServiceAdminGroupID are the fixed ids of the
// system/admin group pair seedServiceTestGroups plants: usr_admin OWNS
// testServiceAdminGroupID, so svcAdminToken()'s "system" scope bypasses
// CreateService's serviceManageGroupIDs gate entirely, but the mandatory
// AdminGroupIDs request-shape check (Phase C, spec 2026-08-10) still needs a
// real, existing admin-tier group id to reference -- mirrors
// testSystemGroupID/testAdminGroupID (service_test.go, Phase B's analog).
const (
	testServiceSystemGroupID = "ugrp_svctest_sys"
	testServiceAdminGroupID  = "ugrp_svctest_admin"
)

// seedServiceTestGroups plants the fixed system/admin group pair (see
// testServiceSystemGroupID/testServiceAdminGroupID) directly at the store
// layer (no FK checks on MemoryDirectory) into dir, owned by usr_admin.
// newServiceAccountsTestService calls this so every
// CreateServiceRequest{...} in this package that needs to succeed can
// reference AdminGroupIDs: []string{testServiceAdminGroupID}.
func seedServiceTestGroups(t *testing.T, dir *MemoryDirectory, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := dir.CreateUserGroup(ctx, store.UserGroup{
		ID: testServiceSystemGroupID, Tier: store.GroupTierSystem, Name: "Test System",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed system group: %v", err)
	}
	if err := dir.CreateUserGroup(ctx, store.UserGroup{
		ID: testServiceAdminGroupID, Tier: store.GroupTierAdmin, Name: "Test Admin",
		ParentGroupID: testServiceSystemGroupID, OwnerUserID: "usr_admin",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed admin group: %v", err)
	}
	// serviceManageGroupIDs enumerates via UserGroupsForUser, a MEMBERSHIP
	// query -- a bare OwnerUserID assignment above is not enough (mirrors
	// seedServerTestGroups' identical note, service_test.go).
	if err := dir.SetUserGroupMember(ctx, testServiceAdminGroupID, "usr_admin", store.GroupStateMember, ""); err != nil {
		t.Fatalf("seed admin group owner membership: %v", err)
	}
}

// newServiceAccountsTestService wires a Service backed by a real
// *MemoryDirectory (for Users+Tokens+Groups, sharing the SAME underlying
// *auth.TokenStore so the live-bearer mirror can be exercised) and a real
// *routing.MemoryStore (for Routes). It seeds four users
// (usr_admin/usr_full/usr_token/usr_stranger) plus one active gateway model
// ("model-a") so allowlist validation has something real to validate
// against, plus the fixed testServiceAdminGroupID pair (seedServiceTestGroups)
// so svcAdminToken()'s CreateService calls satisfy the Phase C admin-group-
// linkage requirement.
func newServiceAccountsTestService(t *testing.T, now time.Time) (*Service, *MemoryDirectory, *auth.TokenStore, *routing.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	tokenStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokenStore)
	for _, u := range []string{"usr_admin", "usr_full", "usr_token", "usr_stranger"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServiceTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	seedActiveModel(t, ctx, routeStore, "model-a", now)
	seedActiveModel(t, ctx, routeStore, "model-b", now)
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now }})
	return svc, dir, tokenStore, routeStore
}

// seedActiveModel creates a minimal active server+application+mapping so
// gatewayModel shows up in Service.Models() — the source of truth
// validateServiceAllowedModels checks an allowlist entry against.
func seedActiveModel(t *testing.T, ctx context.Context, routeStore *routing.MemoryStore, gatewayModel string, now time.Time) {
	t.Helper()
	serverID := "srv_" + gatewayModel
	appID := "app_" + gatewayModel
	mapID := "map_" + gatewayModel
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: gatewayModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mapID, err)
	}
}

// svcAdminToken represents an ELEVATED system_admin test identity (carries
// BOTH "admin" and "system", mirroring sessionPrincipal's real composition
// for role==system_admin && elevated — internal/gateway/auth.go) so every
// pre-existing test in this package that relies on it managing every
// service unconditionally keeps working after the admin-group permissions
// Phase C rewrite (authorizeServiceRead/Settings/ListServices now bypass
// group-scoping ONLY on HasScope("system"), not the plain "admin" scope —
// see service_service_groups_test.go for the group-scoped matrix).
func svcAdminToken() auth.Token {
	return auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin", "system"}}
}

func svcFullToken() auth.Token {
	return auth.Token{UserID: "usr_full", Scopes: []string{"gateway:use"}}
}

func svcTokenDelToken() auth.Token {
	return auth.Token{UserID: "usr_token", Scopes: []string{"gateway:use"}}
}

func svcStrangerToken() auth.Token {
	return auth.Token{UserID: "usr_stranger", Scopes: []string{"gateway:use"}}
}

// createTestService is a small helper: admin-creates a service with usr_full
// as a Full-Delegate and usr_token as a Token-Delegate.
func createTestService(t *testing.T, ctx context.Context, svc *Service) ServiceDTO {
	t.Helper()
	dto, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name:        "Billing Bot",
		Description: "automates invoices",
		Delegates: []ServiceDelegateInput{
			{UserID: "usr_full", CanManageSettings: true},
			{UserID: "usr_token", CanManageSettings: false},
		},
		AdminGroupIDs: []string{testServiceAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	return dto
}

// --- CreateService -----------------------------------------------------

func TestCreateServiceRequiresAdminScope(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())

	for _, tok := range []auth.Token{svcFullToken(), svcTokenDelToken(), svcStrangerToken()} {
		if _, err := svc.CreateService(ctx, tok, CreateServiceRequest{Name: "X"}); !errors.Is(err, ErrServiceForbidden) {
			t.Fatalf("CreateService(non-admin %s) = %v, want ErrServiceForbidden", tok.UserID, err)
		}
	}

	if _, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "  "}); !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("CreateService(blank name) = %v, want ErrServiceValidation", err)
	}

	dto, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "OK", AllowedModels: []string{"model-a"}, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if dto.ID == "" || dto.Name != "OK" || dto.Status != routing.ServerStatusActive {
		t.Fatalf("CreateService dto = %#v", dto)
	}
	if len(dto.AllowedModels) != 1 || dto.AllowedModels[0] != "model-a" {
		t.Fatalf("AllowedModels = %#v, want [model-a]", dto.AllowedModels)
	}
	if dto.TokenCount != 0 || len(dto.Delegates) != 0 {
		t.Fatalf("fresh service should have 0 tokens/delegates: %#v", dto)
	}
}

// --- The three-gate authorization matrix (§6.1) -------------------------

func TestServiceGateMatrixReadTokensSettings(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	cases := []struct {
		name           string
		principal      auth.Token
		wantRead       bool
		wantTokensList bool
		wantSettings   bool
	}{
		{"admin", svcAdminToken(), true, true, true},
		{"full_delegate", svcFullToken(), true, true, true},
		{"token_delegate", svcTokenDelToken(), true, true, false},
		{"stranger", svcStrangerToken(), false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Read gate: GetService.
			_, err := svc.GetService(ctx, tc.principal, created.ID)
			if tc.wantRead && err != nil {
				t.Fatalf("GetService: want success, got %v", err)
			}
			if !tc.wantRead && !errors.Is(err, ErrServiceNotFound) {
				t.Fatalf("GetService: want ErrServiceNotFound, got %v", err)
			}

			// Tokens gate: ListServiceTokens (read side) + CreateServiceToken (write side).
			_, err = svc.ListServiceTokens(ctx, tc.principal, created.ID)
			if tc.wantTokensList && err != nil {
				t.Fatalf("ListServiceTokens: want success, got %v", err)
			}
			if !tc.wantTokensList && !errors.Is(err, ErrServiceNotFound) {
				t.Fatalf("ListServiceTokens: want ErrServiceNotFound, got %v", err)
			}
			_, err = svc.CreateServiceToken(ctx, tc.principal, created.ID, CreateServiceTokenRequest{Name: "probe-" + tc.name})
			if tc.wantTokensList && err != nil {
				t.Fatalf("CreateServiceToken: want success, got %v", err)
			}
			if !tc.wantTokensList && !errors.Is(err, ErrServiceNotFound) {
				t.Fatalf("CreateServiceToken: want ErrServiceNotFound, got %v", err)
			}

			// Settings gate: UpdateService (a no-op rename to its own current name).
			_, err = svc.UpdateService(ctx, tc.principal, created.ID, UpdateServiceRequest{Description: strPtr("touched by " + tc.name)})
			if tc.wantSettings && err != nil {
				t.Fatalf("UpdateService: want success, got %v", err)
			}
			if !tc.wantSettings && !errors.Is(err, ErrServiceNotFound) {
				t.Fatalf("UpdateService: want ErrServiceNotFound, got %v", err)
			}
		})
	}
}

func TestServiceGateMatrixUnknownServiceIsNotFoundForEveryone(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())

	for _, tok := range []auth.Token{svcAdminToken(), svcFullToken(), svcStrangerToken()} {
		if _, err := svc.GetService(ctx, tok, "svc_missing"); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("GetService(unknown, %s) = %v, want ErrServiceNotFound", tok.UserID, err)
		}
	}
}

// --- ListServices (admin=all / delegate=own) -----------------------------

func TestListServicesAdminSeesAllDelegateSeesOwn(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	// A second service with no delegates at all.
	if _, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Other", AdminGroupIDs: []string{testServiceAdminGroupID}}); err != nil {
		t.Fatalf("CreateService (other): %v", err)
	}

	admin, err := svc.ListServices(ctx, svcAdminToken())
	if err != nil {
		t.Fatalf("ListServices(admin): %v", err)
	}
	if len(admin) != 2 {
		t.Fatalf("ListServices(admin) = %d services, want 2", len(admin))
	}

	full, err := svc.ListServices(ctx, svcFullToken())
	if err != nil {
		t.Fatalf("ListServices(full delegate): %v", err)
	}
	if len(full) != 1 || full[0].ID != created.ID {
		t.Fatalf("ListServices(full delegate) = %#v, want just %s", full, created.ID)
	}

	tokenDel, err := svc.ListServices(ctx, svcTokenDelToken())
	if err != nil {
		t.Fatalf("ListServices(token delegate): %v", err)
	}
	if len(tokenDel) != 1 || tokenDel[0].ID != created.ID {
		t.Fatalf("ListServices(token delegate) = %#v, want just %s", tokenDel, created.ID)
	}

	stranger, err := svc.ListServices(ctx, svcStrangerToken())
	if err != nil {
		t.Fatalf("ListServices(stranger): %v", err)
	}
	if len(stranger) != 0 {
		t.Fatalf("ListServices(stranger) = %#v, want empty", stranger)
	}
}

// --- Delegate/allowlist validation ----------------------------------------

func TestCreateServiceRejectsUnknownDelegateUser(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	_, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name:      "X",
		Delegates: []ServiceDelegateInput{{UserID: "usr_ghost", CanManageSettings: true}},
	})
	if !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("CreateService(unknown delegate) = %v, want ErrServiceValidation", err)
	}
}

func TestCreateServiceRejectsDuplicateDelegateInSameRequest(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	_, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name: "X",
		Delegates: []ServiceDelegateInput{
			{UserID: "usr_full", CanManageSettings: true},
			{UserID: "usr_full", CanManageSettings: false},
		},
	})
	if !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("CreateService(duplicate delegate) = %v, want ErrServiceValidation", err)
	}
}

func TestCreateServiceRejectsUnknownAllowlistModel(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	_, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name:          "X",
		AllowedModels: []string{"model-a", "does-not-exist"},
	})
	if !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("CreateService(unknown model) = %v, want ErrServiceValidation", err)
	}
}

func TestUpdateServiceValidatesDelegatesAndAllowlist(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{
		AllowedModels: &[]string{"nope"},
	}); !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("UpdateService(unknown model) = %v, want ErrServiceValidation", err)
	}
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{
		Delegates: &[]ServiceDelegateInput{{UserID: "usr_ghost"}},
	}); !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("UpdateService(unknown delegate) = %v, want ErrServiceValidation", err)
	}

	// Happy path: replace both wholesale.
	updated, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{
		AllowedModels: &[]string{"model-b"},
		Delegates:     &[]ServiceDelegateInput{{UserID: "usr_full", CanManageSettings: true}},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if len(updated.AllowedModels) != 1 || updated.AllowedModels[0] != "model-b" {
		t.Fatalf("AllowedModels = %#v, want [model-b]", updated.AllowedModels)
	}
	if len(updated.Delegates) != 1 || updated.Delegates[0].UserID != "usr_full" {
		t.Fatalf("Delegates = %#v, want [usr_full]", updated.Delegates)
	}
	// usr_token was dropped by the wholesale replace -> loses even Read access.
	if _, err := svc.GetService(ctx, svcTokenDelToken(), created.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("GetService(dropped delegate) = %v, want ErrServiceNotFound", err)
	}
}

// --- DeleteService ---------------------------------------------------------

func TestDeleteServiceRequiresSettingsGateAndRemovesTokens(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	if _, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "T1"}); err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}

	if err := svc.DeleteService(ctx, svcTokenDelToken(), created.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("DeleteService(token delegate) = %v, want ErrServiceNotFound", err)
	}
	if err := svc.DeleteService(ctx, svcFullToken(), created.ID); err != nil {
		t.Fatalf("DeleteService(full delegate): %v", err)
	}
	if _, err := svc.GetService(ctx, svcAdminToken(), created.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("GetService after delete = %v, want ErrServiceNotFound", err)
	}
}

// --- Service-token CRUD + rotation + one-time secret ----------------------

func TestServiceTokenCRUDAndRotation(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	resp, err := svc.CreateServiceToken(ctx, svcFullToken(), created.ID, CreateServiceTokenRequest{
		Name: "T1", LogCommunication: true, ModelOverride: "model-a",
	})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	if resp.Secret == "" {
		t.Fatalf("CreateServiceToken did not return a secret")
	}
	if resp.Token.ServiceID != created.ID || resp.Token.Name != "T1" {
		t.Fatalf("token dto = %#v", resp.Token)
	}
	if len(resp.Token.Scopes) != 1 || resp.Token.Scopes[0] != "llm:invoke" {
		t.Fatalf("Scopes = %#v, want exactly [llm:invoke]", resp.Token.Scopes)
	}
	if resp.Token.ModelOverride != "model-a" || !resp.Token.LogCommunication {
		t.Fatalf("token dto = %#v", resp.Token)
	}

	// blank name -> ErrServiceValidation.
	if _, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "  "}); !errors.Is(err, ErrServiceValidation) {
		t.Fatalf("CreateServiceToken(blank name) = %v, want ErrServiceValidation", err)
	}

	list, err := svc.ListServiceTokens(ctx, svcTokenDelToken(), created.ID)
	if err != nil {
		t.Fatalf("ListServiceTokens: %v", err)
	}
	if len(list) != 1 || list[0].ID != resp.Token.ID {
		t.Fatalf("ListServiceTokens = %#v", list)
	}

	rotated, err := svc.RotateServiceToken(ctx, svcAdminToken(), created.ID, resp.Token.ID)
	if err != nil {
		t.Fatalf("RotateServiceToken: %v", err)
	}
	if rotated.Secret == "" || rotated.Secret == resp.Secret {
		t.Fatalf("RotateServiceToken did not return a fresh secret: old=%q new=%q", resp.Secret, rotated.Secret)
	}
	// Rotation keeps the token's identity but issues a fresh secret. The
	// freshly-returned plaintext differs (checked above); here we assert the
	// stable identity and that the DTO's display prefix was refreshed to match
	// the NEW secret. (We deliberately do NOT assert the new prefix differs from
	// the old one: secretPrefix is "opaigw_" + a single variable char, so two
	// independent secrets legitimately share a prefix ~1-in-alphabet of the time
	// -- that made this test flaky.)
	if rotated.Token.ID != resp.Token.ID || rotated.Token.ServiceID != resp.Token.ServiceID {
		t.Fatalf("RotateServiceToken must keep token id and service stable: %#v", rotated.Token)
	}
	if rotated.Token.SecretPrefix != secretPrefix(rotated.Secret) {
		t.Fatalf("RotateServiceToken: DTO prefix %q does not match the new secret %q", rotated.Token.SecretPrefix, rotated.Secret)
	}

	if err := svc.DeleteServiceToken(ctx, svcTokenDelToken(), created.ID, resp.Token.ID); err != nil {
		t.Fatalf("DeleteServiceToken: %v", err)
	}
	list, err = svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListServiceTokens after delete = %#v err=%v, want empty", list, err)
	}
}

// --- Cross-service token access denied ------------------------------------

func TestServiceTokenCrossServiceAccessDenied(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	svcA := createTestService(t, ctx, svc)
	svcB, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Other Service", AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService (B): %v", err)
	}

	tok, err := svc.CreateServiceToken(ctx, svcAdminToken(), svcA.ID, CreateServiceTokenRequest{Name: "A1"})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}

	// Addressing svcA's token through svcB's path is rejected (no cross-service access).
	if _, err := svc.RotateServiceToken(ctx, svcAdminToken(), svcB.ID, tok.Token.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("RotateServiceToken(cross-service) = %v, want ErrTokenNotFound", err)
	}
	if err := svc.DeleteServiceToken(ctx, svcAdminToken(), svcB.ID, tok.Token.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("DeleteServiceToken(cross-service) = %v, want ErrTokenNotFound", err)
	}

	// The token is untouched (still resolvable, still under svcA).
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), svcA.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListServiceTokens(svcA) after cross-service attempts = %#v err=%v, want 1 token", list, err)
	}

	// A rotate/delete against an unrelated random id under the right service is
	// also ErrTokenNotFound (not found, no leak).
	if _, err := svc.RotateServiceToken(ctx, svcAdminToken(), svcA.ID, "tok_missing"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("RotateServiceToken(unknown token) = %v, want ErrTokenNotFound", err)
	}
}

// --- DTO never leaks a secret ----------------------------------------------

func TestServiceTokenDTOJSONNeverLeaksSecret(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "T1"})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}

	dtoJSON, err := json.Marshal(resp.Token)
	if err != nil {
		t.Fatalf("marshal ServiceTokenDTO: %v", err)
	}
	if strings.Contains(string(dtoJSON), resp.Secret) {
		t.Fatalf("ServiceTokenDTO JSON leaks the plaintext secret: %s", dtoJSON)
	}
	if strings.Contains(strings.ToLower(string(dtoJSON)), "secret_hash") {
		t.Fatalf("ServiceTokenDTO JSON exposes secret_hash: %s", dtoJSON)
	}

	// The list/read path never carries the secret at all.
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("ListServiceTokens: %v", err)
	}
	listJSON, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if strings.Contains(string(listJSON), resp.Secret) {
		t.Fatalf("ListServiceTokens JSON leaks the plaintext secret: %s", listJSON)
	}
}

// --- driver=memory service-token smoke (mint + disabled-gate mirror) -----

func TestServiceTokenMemoryDriverMintAndDisabledGate(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _, tokenStore, _ := newServiceAccountsTestService(t, now)
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Memory Svc", AllowedModels: []string{"model-a"}, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "T1"})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}

	// Minted with Kind/ServiceID/scope + the service's CURRENT allowlist,
	// resolvable via the RAW *auth.TokenStore (what the gateway's inference
	// auth path actually calls under driver=memory).
	live, ok := tokenStore.LookupBearer("Bearer " + resp.Secret)
	if !ok {
		t.Fatalf("LookupBearer: token not found under the memory driver")
	}
	if !live.IsService() || live.ServiceID != created.ID {
		t.Fatalf("live token = %#v, want a service token for %s", live, created.ID)
	}
	if len(live.Scopes) != 1 || live.Scopes[0] != "llm:invoke" {
		t.Fatalf("live token scopes = %#v, want [llm:invoke]", live.Scopes)
	}
	if len(live.AllowedModels) != 1 || live.AllowedModels[0] != "model-a" {
		t.Fatalf("live token AllowedModels = %#v, want [model-a]", live.AllowedModels)
	}

	// Disabling the service mirrors onto the ALREADY-MINTED token's live cache
	// entry: it stops resolving as active, without touching the token row itself.
	disabled := routing.ServerStatusDisabled
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Status: &disabled}); err != nil {
		t.Fatalf("UpdateService(disable): %v", err)
	}
	if _, ok := tokenStore.LookupBearer("Bearer " + resp.Secret); ok {
		t.Fatalf("LookupBearer: token still resolves after its service was disabled")
	}

	// Re-enabling restores it.
	active := routing.ServerStatusActive
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Status: &active}); err != nil {
		t.Fatalf("UpdateService(re-enable): %v", err)
	}
	if _, ok := tokenStore.LookupBearer("Bearer " + resp.Secret); !ok {
		t.Fatalf("LookupBearer: token did not resolve again after its service was re-enabled")
	}

	// Deleting the service removes the token from the live cache too (the
	// memory driver has no DB-level FK cascade to rely on).
	if err := svc.DeleteService(ctx, svcAdminToken(), created.ID); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, ok := tokenStore.LookupBearer("Bearer " + resp.Secret); ok {
		t.Fatalf("LookupBearer: token still resolves after its service was deleted")
	}
}
