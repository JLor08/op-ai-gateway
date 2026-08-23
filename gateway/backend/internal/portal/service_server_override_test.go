// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// serverOverrideTestEnv wires a Service backed by a real *MemoryDirectory
// (Users + Tokens, sharing one *auth.TokenStore so the live-bearer mirror is
// exercised) and a real *routing.MemoryStore (servers/applications/mappings),
// for exercising AuthorizeServerManage, ServerModels, and the token
// server_override create/update self-heal. Mirrors newModelServersTestService
// / newTokenProjectTestEnv's style.
// nextServerOverrideTestPort backs seedMapping's per-application port
// allocation (see its doc comment). Package-scoped is fine: tests in this
// file never run concurrently with each other (no t.Parallel()).
var nextServerOverrideTestPort = 8000

type serverOverrideTestEnv struct {
	t          *testing.T
	ctx        context.Context
	dir        *MemoryDirectory
	routeStore *routing.MemoryStore
	svc        *Service
	now        time.Time
}

func newServerOverrideTestEnv(t *testing.T) *serverOverrideTestEnv {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Users:  dir,
		Tokens: dir,
		Routes: routeStore,
		Clock:  func() time.Time { return now },
	})
	return &serverOverrideTestEnv{t: t, ctx: context.Background(), dir: dir, routeStore: routeStore, svc: svc, now: now}
}

func (e *serverOverrideTestEnv) createUser(id, role string) {
	e.t.Helper()
	u := store.User{ID: id, Email: id + "@example.test", DisplayName: id, Role: role, Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.dir.CreateUser(e.ctx, u); err != nil {
		e.t.Fatalf("create user %s: %v", id, err)
	}
}

// seedServer creates an active, healthy server named serverID, owned
// (ServerOwners) by ownerUserID ("" = no owner set).
func (e *serverOverrideTestEnv) seedServer(serverID, ownerUserID string) {
	e.t.Helper()
	if err := e.routeStore.CreateAIServer(e.ctx, routing.AIServer{
		ID: serverID, Name: serverID, Domain: serverID + ".test",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if ownerUserID != "" {
		if err := e.routeStore.SetServerOwners(e.ctx, serverID, []string{ownerUserID}); err != nil {
			e.t.Fatalf("SetServerOwners %s: %v", serverID, err)
		}
	}
}

// seedMapping creates an active application + mapping offering gatewayModel
// (upstream name appModel) on serverID. Each call allocates a fresh port off
// a package-scoped counter so several applications on the SAME server never
// collide on the store's per-server port-uniqueness check.
func (e *serverOverrideTestEnv) seedMapping(serverID, appID, mappingID, gatewayModel, appModel string) {
	e.t.Helper()
	nextServerOverrideTestPort++
	if err := e.routeStore.CreateApplication(e.ctx, routing.Application{
		ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: nextServerOverrideTestPort, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := e.routeStore.CreateMapping(e.ctx, routing.ModelMapping{
		ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: appModel,
		Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
}

// --- AuthorizeServerManage ---------------------------------------------------

func TestAuthorizeServerManage(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_other", "user")
	e.createUser("usr_sys", "system_admin")
	e.seedServer("srv-a", "usr_owner")

	manager := token("usr_owner")
	nonManager := token("usr_other")
	sysAdmin := token("usr_sys", "system")

	if err := e.svc.AuthorizeServerManage(e.ctx, manager, "srv-a"); err != nil {
		t.Fatalf("AuthorizeServerManage(manager) = %v, want nil", err)
	}
	if err := e.svc.AuthorizeServerManage(e.ctx, nonManager, "srv-a"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("AuthorizeServerManage(non-manager) = %v, want ErrServerNotFound", err)
	}
	if err := e.svc.AuthorizeServerManage(e.ctx, sysAdmin, "srv-a"); err != nil {
		t.Fatalf("AuthorizeServerManage(system) = %v, want nil", err)
	}
	if err := e.svc.AuthorizeServerManage(e.ctx, manager, "srv-unknown"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("AuthorizeServerManage(unknown server) = %v, want ErrServerNotFound", err)
	}
}

// --- ServerModels -------------------------------------------------------

// TestServerModels proves the offered-model listing: manager gets the
// distinct gateway model names srv-a offers (deduped across two mappings of
// the same name, sorted), a non-manager and an unknown server both get
// ErrServerNotFound (404-no-leak), and a sibling server's models never leak
// in.
func TestServerModels(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_other", "user")
	e.seedServer("srv-a", "usr_owner")
	e.seedServer("srv-b", "")
	e.seedMapping("srv-a", "app-a1", "map-a1", "model-x", "up-x")
	e.seedMapping("srv-a", "app-a2", "map-a2", "model-y", "up-y")
	// A second mapping on srv-a offering the SAME gateway model name as
	// map-a1 (different application/upstream) proves the dedup.
	e.seedMapping("srv-a", "app-a3", "map-a3", "model-x", "up-x2")
	e.seedMapping("srv-b", "app-b1", "map-b1", "model-z", "up-z")

	manager := token("usr_owner")
	rows, err := e.svc.ServerModels(e.ctx, manager, "srv-a")
	if err != nil {
		t.Fatalf("ServerModels(manager): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (deduped model-x + model-y), got %+v", len(rows), rows)
	}
	if rows[0].ID != "model-x" || rows[1].ID != "model-y" {
		t.Fatalf("rows = %+v, want sorted [model-x, model-y]", rows)
	}
	for _, r := range rows {
		if r.DisplayName != r.ID {
			t.Fatalf("row %+v: DisplayName should equal ID", r)
		}
		if r.ID == "model-z" {
			t.Fatalf("srv-b's model-z leaked into srv-a's rows: %+v", rows)
		}
	}

	nonManager := token("usr_other")
	if _, err := e.svc.ServerModels(e.ctx, nonManager, "srv-a"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("ServerModels(non-manager) = %v, want ErrServerNotFound", err)
	}

	if _, err := e.svc.ServerModels(e.ctx, manager, "srv-unknown"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("ServerModels(unknown server) = %v, want ErrServerNotFound", err)
	}
}

// --- Token create: server_override persisted when manageable ---------------

func TestCreateTokenServerOverridePersisted(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := token("usr_owner")

	resp, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{
		Name: "tok1", Scopes: []string{"gateway:use"},
		ServerOverride: "srv-a", ServerOverrideForceUnreachable: true,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if resp.Token.ServerOverride != "srv-a" {
		t.Fatalf("Token.ServerOverride = %q, want srv-a", resp.Token.ServerOverride)
	}
	if !resp.Token.ServerOverrideForceUnreachable {
		t.Fatalf("Token.ServerOverrideForceUnreachable = false, want true")
	}
	rec, err := e.dir.TokenByID(e.ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.ServerOverride != "srv-a" || !rec.ServerOverrideForceUnreachable {
		t.Fatalf("persisted record = %+v, want ServerOverride=srv-a ForceUnreachable=true", rec)
	}
}

// TestCreateTokenServerOverrideSelfHealsWhenNotManaged proves the create-time
// self-heal: an override the owner cannot manage at create time is silently
// cleared (both the override AND the force flag), never rejecting the whole
// create.
func TestCreateTokenServerOverrideSelfHealsWhenNotManaged(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_other", "user")
	e.seedServer("srv-a", "usr_other") // NOT owned/managed by usr_owner
	owner := token("usr_owner")

	resp, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{
		Name: "tok1", Scopes: []string{"gateway:use"},
		ServerOverride: "srv-a", ServerOverrideForceUnreachable: true,
	})
	if err != nil {
		t.Fatalf("CreateToken should self-heal an unmanageable override, not reject; got err: %v", err)
	}
	if resp.Token.ServerOverride != "" {
		t.Fatalf("Token.ServerOverride = %q, want cleared to \"\"", resp.Token.ServerOverride)
	}
	if resp.Token.ServerOverrideForceUnreachable {
		t.Fatalf("Token.ServerOverrideForceUnreachable = true, want cleared alongside the override")
	}
	rec, err := e.dir.TokenByID(e.ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.ServerOverride != "" || rec.ServerOverrideForceUnreachable {
		t.Fatalf("persisted record = %+v, want both cleared", rec)
	}
}

// --- Token update: self-heal against a REVOKED manage grant -----------------

// TestUpdateTokenServerOverrideSelfHealsWhenManagementRevoked is the core
// mutation-sensitive self-heal scenario: create with a manageable override,
// then revoke the owner's management of that server, then perform an
// UNRELATED update (rename) — the persisted/returned override (and force
// flag) must come back cleared, and the update must still succeed and apply
// the unrelated change (not reject the whole request).
func TestUpdateTokenServerOverrideSelfHealsWhenManagementRevoked(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := token("usr_owner")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{
		Name: "tok1", Scopes: []string{"gateway:use"}, ServerOverride: "srv-a",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.Token.ServerOverride != "srv-a" {
		t.Fatalf("precondition: created override = %q, want srv-a", created.Token.ServerOverride)
	}

	// Revoke usr_owner's management of srv-a between create and update.
	if err := e.routeStore.SetServerOwners(e.ctx, "srv-a", nil); err != nil {
		t.Fatalf("SetServerOwners(revoke): %v", err)
	}

	newName := "tok1-renamed"
	updated, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if updated.ServerOverride != "" {
		t.Fatalf("ServerOverride after revoke+update = %q, want cleared", updated.ServerOverride)
	}
	if updated.ServerOverrideForceUnreachable {
		t.Fatalf("ServerOverrideForceUnreachable after revoke+update = true, want cleared")
	}
	if updated.Name != newName {
		t.Fatalf("Name = %q, want %q (the unrelated update must still apply)", updated.Name, newName)
	}

	rec, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.ServerOverride != "" || rec.ServerOverrideForceUnreachable {
		t.Fatalf("persisted record after self-heal = %+v, want both cleared", rec)
	}
}

// TestUpdateTokenServerOverrideSelfHealsOnNewUnmanageableValue proves the
// self-heal also applies to a NEWLY requested override value (not only a
// pre-existing one carried over untouched).
func TestUpdateTokenServerOverrideSelfHealsOnNewUnmanageableValue(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_other", "user")
	e.seedServer("srv-a", "usr_owner")
	e.seedServer("srv-b", "usr_other") // not managed by usr_owner
	owner := token("usr_owner")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "tok1", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	override := "srv-b"
	updated, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{
		ServerOverride: &override, ServerOverrideForceUnreachable: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateToken should self-heal, not reject; got err: %v", err)
	}
	if updated.ServerOverride != "" {
		t.Fatalf("ServerOverride = %q, want cleared (owner does not manage srv-b)", updated.ServerOverride)
	}
	if updated.ServerOverrideForceUnreachable {
		t.Fatalf("ServerOverrideForceUnreachable = true, want cleared alongside the override")
	}
}

// TestUpdateTokenServerOverrideSurvivesUnrelatedUpdateWhenStillManaged proves
// the non-regression counterpart: a still-manageable override survives an
// unrelated update untouched.
func TestUpdateTokenServerOverrideSurvivesUnrelatedUpdateWhenStillManaged(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := token("usr_owner")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{
		Name: "tok1", Scopes: []string{"gateway:use"}, ServerOverride: "srv-a", ServerOverrideForceUnreachable: true,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	newName := "tok1-renamed"
	updated, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if updated.ServerOverride != "srv-a" {
		t.Fatalf("ServerOverride = %q, want srv-a to survive an unrelated update while still manageable", updated.ServerOverride)
	}
	if !updated.ServerOverrideForceUnreachable {
		t.Fatalf("ServerOverrideForceUnreachable = false, want true to survive")
	}
}

// --- AuthorizeRunAsToken carries the override onto the run-as token --------

func TestAuthorizeRunAsTokenCarriesServerOverride(t *testing.T) {
	e := newServerOverrideTestEnv(t)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := token("usr_owner")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{
		Name: "tok1", Scopes: []string{"gateway:use"}, ServerOverride: "srv-a", ServerOverrideForceUnreachable: true,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	runAs, err := e.svc.AuthorizeRunAsToken(e.ctx, owner, created.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if runAs.ServerOverride != "srv-a" {
		t.Fatalf("run-as ServerOverride = %q, want srv-a", runAs.ServerOverride)
	}
	if !runAs.ServerOverrideForceUnreachable {
		t.Fatalf("run-as ServerOverrideForceUnreachable = false, want true")
	}
}

// --- Service tokens: server_override is structurally never persisted -------

// TestCreateServiceTokenNeverPersistsServerOverride proves the invariant
// end-to-end through the REAL CreateServiceToken path: CreateServiceTokenRequest
// carries no server_override field at all, so a service token can never
// acquire one, however it is created.
func TestCreateServiceTokenNeverPersistsServerOverride(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	svc, dir, _, _ := newServiceAccountsTestService(t, now)
	ctx := context.Background()
	created := createTestService(t, ctx, svc)

	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "svc-tok"})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	rec, err := dir.TokenByID(ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.Kind != store.TokenKindService {
		t.Fatalf("precondition: Kind = %q, want %q", rec.Kind, store.TokenKindService)
	}
	if rec.ServerOverride != "" || rec.ServerOverrideForceUnreachable {
		t.Fatalf("service token persisted a server_override = %+v, want none (CreateServiceTokenRequest has no such field)", rec)
	}
}
