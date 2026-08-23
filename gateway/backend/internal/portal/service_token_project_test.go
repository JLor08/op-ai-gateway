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
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// --- Task 5: token -> project assignment (membership-checked) --------------
//
// Mirrors the projectTestEnv scaffolding in service_projects_test.go, but
// additionally wires Tokens: dir so CreateToken/UpdateToken/RotateToken run
// against the real MemoryDirectory token store (needed to exercise the new
// project_id threading end-to-end, not just against a fakeTokens stub), and
// Usage: a fresh in-memory usage.Recorder (rec) so ProjectTokens' per-token +
// project-total usage aggregation can be exercised by seeding usage.Event
// rows directly via e.rec.Record(...).

type tokenProjectTestEnv struct {
	t   *testing.T
	ctx context.Context
	dir *MemoryDirectory
	svc *Service
	rec *usage.Recorder
	now time.Time
}

func newTokenProjectTestEnv(t *testing.T) *tokenProjectTestEnv {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	rec := usage.NewRecorder()
	svc := NewService(ServiceDeps{
		Users:    dir,
		Groups:   dir,
		Projects: dir,
		Tokens:   dir,
		Usage:    rec,
		Routes:   routing.NewMemoryStore(),
		Clock:    func() time.Time { return now },
	})
	return &tokenProjectTestEnv{t: t, ctx: context.Background(), dir: dir, svc: svc, rec: rec, now: now}
}

func (e *tokenProjectTestEnv) createUser(id, role string) store.User {
	e.t.Helper()
	u := store.User{
		ID: id, Email: id + "@example.test", DisplayName: id, Role: role,
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.dir.CreateUser(e.ctx, u); err != nil {
		e.t.Fatalf("create user %s: %v", id, err)
	}
	return u
}

func (e *tokenProjectTestEnv) mustCreateProject(actor auth.Token, name string) ProjectDTO {
	e.t.Helper()
	dto, err := e.svc.CreateProject(e.ctx, actor, CreateProjectInput{Name: name})
	if err != nil {
		e.t.Fatalf("CreateProject(%s): %v", name, err)
	}
	return dto
}

func TestCreateToken_ProjectAssignment_MemberAllowed(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	got, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "CI Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	if got.Token.ProjectID != proj.ID {
		t.Fatalf("Token.ProjectID = %q, want %q", got.Token.ProjectID, proj.ID)
	}
	if got.Token.ProjectName != "Widgets" {
		t.Fatalf("Token.ProjectName = %q, want %q", got.Token.ProjectName, "Widgets")
	}

	// The persisted record itself carries project_id (not just the DTO).
	rec, err := e.dir.TokenByID(e.ctx, got.Token.ID)
	if err != nil || rec.ProjectID != proj.ID {
		t.Fatalf("stored record ProjectID = %q err=%v, want %q", rec.ProjectID, err, proj.ID)
	}
}

func TestCreateToken_ProjectAssignment_DirectMemberAllowed(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	proj := e.mustCreateProject(owner, "Widgets")

	// usr_member is a MEMBER (not owner) via a direct project_members row
	// (spec §4 rule 2) -- their own token can still be attributed to the
	// project even though they didn't create it. AddProjectMembers itself
	// additionally requires the assigner to see the assignee (VisibleUserIDs,
	// spec §4's "Benutzer/Gruppen zuordnen" gate) -- unrelated to the
	// membership check under test here, so the "system" scope is used only to
	// clear that assigner-visibility gate, not to bypass anything we're
	// testing.
	sysOwner := token("usr_owner", "system")
	if err := e.svc.AddProjectMembers(e.ctx, sysOwner, proj.ID, []string{"usr_member"}); err != nil {
		t.Fatalf("AddProjectMembers: %v", err)
	}

	got, err := e.svc.CreateToken(e.ctx, member, CreateTokenRequest{Name: "Member Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	if got.Token.ProjectID != proj.ID {
		t.Fatalf("Token.ProjectID = %q, want %q", got.Token.ProjectID, proj.ID)
	}
}

func TestCreateToken_ProjectAssignment_NonMemberRejected(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_outsider", "user")
	owner := token("usr_owner")
	outsider := token("usr_outsider")
	proj := e.mustCreateProject(owner, "Widgets")

	_, err := e.svc.CreateToken(e.ctx, outsider, CreateTokenRequest{Name: "Outsider Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if !errors.Is(err, ErrProjectNotMember) {
		t.Fatalf("CreateToken error = %v, want ErrProjectNotMember", err)
	}

	// Nothing was persisted on the rejected attempt.
	list, err := e.svc.ListTokens(e.ctx, outsider)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, tok := range list.Data {
		if tok.Name == "Outsider Token" {
			t.Fatalf("a token was created despite the membership rejection: %+v", tok)
		}
	}
}

func TestCreateToken_ProjectAssignment_NotFoundRejected(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	_, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Ghost Token", Scopes: []string{"gateway:use"}, ProjectID: "proj_does_not_exist"})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("CreateToken error = %v, want ErrProjectNotFound", err)
	}
}

func TestCreateToken_ProjectAssignment_EmptyIsNoOp(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	// An empty project_id never triggers a membership check and never fails,
	// even with zero projects in existence.
	got, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Plain Token", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	if got.Token.ProjectID != "" || got.Token.ProjectName != "" {
		t.Fatalf("Token.ProjectID/ProjectName = %q/%q, want empty/empty", got.Token.ProjectID, got.Token.ProjectName)
	}
}

func TestUpdateToken_ProjectAssignment_ReassignAndClear(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	projA := e.mustCreateProject(owner, "Alpha")
	projB := e.mustCreateProject(owner, "Beta")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Roaming Token", Scopes: []string{"gateway:use"}, ProjectID: projA.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	// Reassign A -> B.
	projB2 := projB.ID
	updated, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{ProjectID: &projB2})
	if err != nil {
		t.Fatalf("UpdateToken (reassign) returned %v", err)
	}
	if updated.ProjectID != projB.ID || updated.ProjectName != "Beta" {
		t.Fatalf("after reassign: ProjectID/ProjectName = %q/%q, want %q/Beta", updated.ProjectID, updated.ProjectName, projB.ID)
	}

	// Clear via "".
	empty := ""
	cleared, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{ProjectID: &empty})
	if err != nil {
		t.Fatalf("UpdateToken (clear) returned %v", err)
	}
	if cleared.ProjectID != "" || cleared.ProjectName != "" {
		t.Fatalf("after clear: ProjectID/ProjectName = %q/%q, want empty/empty", cleared.ProjectID, cleared.ProjectName)
	}
}

func TestUpdateToken_ProjectAssignment_NonMemberRejected(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_outsider", "user")
	owner := token("usr_owner")
	outsider := token("usr_outsider")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, outsider, CreateTokenRequest{Name: "Outsider Token", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	target := proj.ID
	_, err = e.svc.UpdateToken(e.ctx, outsider, created.Token.ID, UpdateTokenRequest{ProjectID: &target})
	if !errors.Is(err, ErrProjectNotMember) {
		t.Fatalf("UpdateToken error = %v, want ErrProjectNotMember", err)
	}

	// The token's project attribution is untouched by the rejected attempt.
	rec, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil || rec.ProjectID != "" {
		t.Fatalf("stored record ProjectID = %q err=%v, want empty (unchanged)", rec.ProjectID, err)
	}
}

func TestUpdateToken_ProjectAssignment_NotFoundRejected(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Plain Token", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	ghost := "proj_does_not_exist"
	_, err = e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{ProjectID: &ghost})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("UpdateToken error = %v, want ErrProjectNotFound", err)
	}
}

func TestUpdateToken_ProjectAssignment_NilKeepsExisting(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Sticky Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	// An unrelated update (rename) with req.ProjectID left nil must NOT touch
	// the existing project attribution.
	newName := "Renamed Token"
	updated, err := e.svc.UpdateToken(e.ctx, owner, created.Token.ID, UpdateTokenRequest{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateToken (rename) returned %v", err)
	}
	if updated.Name != "Renamed Token" {
		t.Fatalf("Name = %q, want %q", updated.Name, "Renamed Token")
	}
	if updated.ProjectID != proj.ID {
		t.Fatalf("ProjectID after unrelated update = %q, want %q (must be preserved)", updated.ProjectID, proj.ID)
	}
}

func TestRotateToken_PreservesProjectAssignment(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Rotating Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	rotated, err := e.svc.RotateToken(e.ctx, owner, created.Token.ID)
	if err != nil {
		t.Fatalf("RotateToken returned %v", err)
	}
	if rotated.Token.ProjectID != proj.ID || rotated.Token.ProjectName != "Widgets" {
		t.Fatalf("after rotate: ProjectID/ProjectName = %q/%q, want %q/Widgets", rotated.Token.ProjectID, rotated.Token.ProjectName, proj.ID)
	}
	if rotated.Secret == "" {
		t.Fatal("RotateToken returned an empty secret")
	}
}

func TestListTokens_SurfacesProjectName(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	if _, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Listed Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID}); err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	list, err := e.svc.ListTokens(e.ctx, owner)
	if err != nil {
		t.Fatalf("ListTokens returned %v", err)
	}
	var found bool
	for _, tok := range list.Data {
		if tok.Name != "Listed Token" {
			continue
		}
		found = true
		if tok.ProjectID != proj.ID || tok.ProjectName != "Widgets" {
			t.Fatalf("listed token project fields = %q/%q, want %q/Widgets", tok.ProjectID, tok.ProjectName, proj.ID)
		}
	}
	if !found {
		t.Fatal("Listed Token not found in ListTokens output")
	}
}

func TestAuthorizeRunAsToken_SurfacesProjectAssignment(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Run-As Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	runAs, err := e.svc.AuthorizeRunAsToken(e.ctx, owner, created.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken returned %v", err)
	}
	if runAs.ProjectID != proj.ID || runAs.ProjectName != "Widgets" {
		t.Fatalf("runAs ProjectID/ProjectName = %q/%q, want %q/Widgets", runAs.ProjectID, runAs.ProjectName, proj.ID)
	}
}

// --- ProjectTokens / DetachProjectToken (owner/admin, no-secret, no-leak) ---

// TestProjectTokens_OwnerSeesAttachedTokensNoSecret proves ProjectTokens
// returns exactly the tokens attached to the project (across DIFFERENT
// owners), with owner names resolved and never a secret/hash field, and
// excludes a token attached to a different project.
func TestProjectTokens_OwnerSeesAttachedTokensNoSecret(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	proj := e.mustCreateProject(owner, "Widgets")
	other := e.mustCreateProject(owner, "Gadgets")

	// usr_member must be a project member before their OWN token can be
	// attached to it (assignTokenProject's membership check).
	sysOwner := token("usr_owner", "system")
	if err := e.svc.AddProjectMembers(e.ctx, sysOwner, proj.ID, []string{"usr_member"}); err != nil {
		t.Fatalf("AddProjectMembers: %v", err)
	}

	ownerTok, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Owner Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken(owner) returned %v", err)
	}
	memberTok, err := e.svc.CreateToken(e.ctx, member, CreateTokenRequest{Name: "Member Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken(member) returned %v", err)
	}
	// Attached to a DIFFERENT project -- must not appear.
	if _, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Other Project Token", Scopes: []string{"gateway:use"}, ProjectID: other.ID}); err != nil {
		t.Fatalf("CreateToken(other project) returned %v", err)
	}

	view, err := e.svc.ProjectTokens(e.ctx, owner, proj.ID)
	if err != nil {
		t.Fatalf("ProjectTokens(owner) returned %v", err)
	}
	got := view.Tokens
	if len(got) != 2 {
		t.Fatalf("ProjectTokens.Tokens = %+v, want exactly 2 (owner + member tokens)", got)
	}
	byID := map[string]ProjectTokenDTO{}
	for _, row := range got {
		byID[row.ID] = row
	}
	ownerRow, ok := byID[ownerTok.Token.ID]
	if !ok {
		t.Fatalf("owner token %s missing from ProjectTokens: %+v", ownerTok.Token.ID, got)
	}
	if ownerRow.OwnerUserID != "usr_owner" || ownerRow.OwnerName != "usr_owner" {
		t.Fatalf("owner row owner fields = %+v", ownerRow)
	}
	if ownerRow.Name != "Owner Token" || ownerRow.Status != string(store.TokenStatusActive) {
		t.Fatalf("owner row = %+v", ownerRow)
	}
	if ownerRow.SecretPrefix == "" {
		t.Fatalf("owner row SecretPrefix is empty, want a non-secret display prefix: %+v", ownerRow)
	}
	memberRow, ok := byID[memberTok.Token.ID]
	if !ok {
		t.Fatalf("member token %s missing from ProjectTokens: %+v", memberTok.Token.ID, got)
	}
	if memberRow.OwnerUserID != "usr_member" || memberRow.OwnerName != "usr_member" {
		t.Fatalf("member row owner fields = %+v", memberRow)
	}

	// Never leaks a secret/hash: marshal the WHOLE view and assert none of
	// the forbidden substrings/keys appear.
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal ProjectTokens: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{"secret_hash", "\"secret\":", ownerTok.Secret, memberTok.Secret} {
		if forbidden == "" {
			continue
		}
		if strings.Contains(body, forbidden) {
			t.Fatalf("ProjectTokens JSON leaks a secret (%q found): %s", forbidden, body)
		}
	}
}

// TestProjectTokens_AdminSeesAttachedTokens proves an admin (not the owner)
// can also read a project's attached tokens via authorizeProjectManage's
// owner-OR-admin gate.
func TestProjectTokens_AdminSeesAttachedTokens(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_admin", "admin")
	owner := token("usr_owner")
	admin := token("usr_admin", "admin")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Owner Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	view, err := e.svc.ProjectTokens(e.ctx, admin, proj.ID)
	if err != nil {
		t.Fatalf("ProjectTokens(admin) returned %v", err)
	}
	got := view.Tokens
	if len(got) != 1 || got[0].ID != created.Token.ID {
		t.Fatalf("ProjectTokens(admin).Tokens = %+v, want [%s]", got, created.Token.ID)
	}
}

// TestProjectTokens_NonManagerNoLeak proves a non-owner, non-admin (incl. a
// mere project MEMBER) gets ErrProjectNotFound (404-no-leak), mirroring
// authorizeProjectManage's own no-leak contract.
func TestProjectTokens_NonManagerNoLeak(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	stranger := token("usr_stranger")
	proj := e.mustCreateProject(owner, "Widgets")

	sysOwner := token("usr_owner", "system")
	if err := e.svc.AddProjectMembers(e.ctx, sysOwner, proj.ID, []string{"usr_member"}); err != nil {
		t.Fatalf("AddProjectMembers: %v", err)
	}

	if _, err := e.svc.ProjectTokens(e.ctx, member, proj.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ProjectTokens(mere member) = %v, want ErrProjectNotFound (no-leak)", err)
	}
	if _, err := e.svc.ProjectTokens(e.ctx, stranger, proj.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ProjectTokens(stranger) = %v, want ErrProjectNotFound (no-leak)", err)
	}
	if _, err := e.svc.ProjectTokens(e.ctx, owner, "proj_missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ProjectTokens(missing project) = %v, want ErrProjectNotFound", err)
	}
}

// TestDetachProjectToken_ClearsAttachment proves a successful detach clears
// project_id on the token (verified at the store) while leaving every other
// field (secret hash, status, name) untouched, and that the token no longer
// appears in ProjectTokens afterward.
func TestDetachProjectToken_ClearsAttachment(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Detach Me", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	before, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID(before): %v", err)
	}

	if err := e.svc.DetachProjectToken(e.ctx, owner, proj.ID, created.Token.ID); err != nil {
		t.Fatalf("DetachProjectToken returned %v", err)
	}

	after, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID(after): %v", err)
	}
	if after.ProjectID != "" {
		t.Fatalf("ProjectID after detach = %q, want empty", after.ProjectID)
	}
	if after.SecretHash != before.SecretHash || after.Status != before.Status || after.Name != before.Name {
		t.Fatalf("detach mutated unrelated fields: before=%+v after=%+v", before, after)
	}

	view, err := e.svc.ProjectTokens(e.ctx, owner, proj.ID)
	if err != nil {
		t.Fatalf("ProjectTokens after detach: %v", err)
	}
	got := view.Tokens
	if len(got) != 0 {
		t.Fatalf("ProjectTokens after detach = %+v, want empty", got)
	}
}

// TestDetachProjectToken_CrossProjectNoLeak proves detaching a token attached
// to a DIFFERENT project (via the manager's OWN project id) is
// ErrTokenNotFound -- the cross-project guard -- and that the token's
// attachment to its real project is left untouched (no side effect from the
// rejected call).
func TestDetachProjectToken_CrossProjectNoLeak(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	projA := e.mustCreateProject(owner, "Alpha")
	projB := e.mustCreateProject(owner, "Beta")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Alpha Token", Scopes: []string{"gateway:use"}, ProjectID: projA.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	// owner manages BOTH projects (owns both), so this exercises the
	// cross-project guard itself, not authorizeProjectManage.
	if err := e.svc.DetachProjectToken(e.ctx, owner, projB.ID, created.Token.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("DetachProjectToken(wrong project) = %v, want ErrTokenNotFound", err)
	}

	rec, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil || rec.ProjectID != projA.ID {
		t.Fatalf("token attachment changed by the rejected cross-project detach: %+v err=%v, want ProjectID=%s", rec, err, projA.ID)
	}

	// A token attached to NO project is likewise ErrTokenNotFound.
	unattached, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Unattached", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken(unattached) returned %v", err)
	}
	if err := e.svc.DetachProjectToken(e.ctx, owner, projA.ID, unattached.Token.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("DetachProjectToken(unattached token) = %v, want ErrTokenNotFound", err)
	}

	// An unknown token id is likewise ErrTokenNotFound (no existence leak).
	if err := e.svc.DetachProjectToken(e.ctx, owner, projA.ID, "tok_ghost"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("DetachProjectToken(unknown token) = %v, want ErrTokenNotFound", err)
	}
}

// TestDetachProjectToken_NonManagerNoLeak proves a non-owner, non-admin
// caller gets ErrProjectNotFound (404-no-leak) BEFORE the cross-project
// check ever runs, mirroring authorizeProjectManage's own contract.
func TestDetachProjectToken_NonManagerNoLeak(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")
	stranger := token("usr_stranger")
	proj := e.mustCreateProject(owner, "Widgets")

	created, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Owner Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}

	if err := e.svc.DetachProjectToken(e.ctx, stranger, proj.ID, created.Token.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("DetachProjectToken(stranger) = %v, want ErrProjectNotFound (no-leak)", err)
	}
	if err := e.svc.DetachProjectToken(e.ctx, owner, "proj_missing", created.Token.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("DetachProjectToken(missing project) = %v, want ErrProjectNotFound", err)
	}

	// Untouched by the rejected calls.
	rec, err := e.dir.TokenByID(e.ctx, created.Token.ID)
	if err != nil || rec.ProjectID != proj.ID {
		t.Fatalf("token attachment changed by rejected calls: %+v err=%v", rec, err)
	}
}

// --- ProjectTokens usage aggregation (per-token + project TRUE total) ------

// TestProjectTokens_UsageAggregation proves ProjectTokens' usage semantics
// end-to-end: (a) each CURRENTLY-ATTACHED token's row carries exactly its OWN
// project-attributed usage (usage_events where project_id=P AND
// token_id=that token, all-time); (b) a token attached but never used reads
// all zeros, never omitted; (c) Total is the project's TRUE total -- the SUM
// over EVERY usage_events row with project_id=P, regardless of whether the
// row's token is still attached -- so it INCLUDES usage from a token that
// has since been detached and therefore STRICTLY EXCEEDS the sum of the
// per-token rows; (d) usage from a DIFFERENT project never leaks in; (e) the
// 404-no-leak gate for a non-manager still holds.
func TestProjectTokens_UsageAggregation(t *testing.T) {
	e := newTokenProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	stranger := token("usr_stranger")
	proj := e.mustCreateProject(owner, "Widgets")
	other := e.mustCreateProject(owner, "Gadgets")

	sysOwner := token("usr_owner", "system")
	if err := e.svc.AddProjectMembers(e.ctx, sysOwner, proj.ID, []string{"usr_member"}); err != nil {
		t.Fatalf("AddProjectMembers: %v", err)
	}

	ownerTok, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Owner Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken(owner) returned %v", err)
	}
	memberTok, err := e.svc.CreateToken(e.ctx, member, CreateTokenRequest{Name: "Member Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken(member) returned %v", err)
	}
	// Attached, never used -- must show zeros, not be omitted from the list.
	idleTok, err := e.svc.CreateToken(e.ctx, owner, CreateTokenRequest{Name: "Idle Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken(idle) returned %v", err)
	}

	now := e.now
	// Owner token: 2 requests, 30 input + 40 output = 70 total.
	e.rec.Record(usage.Event{ID: "e1", UserID: "usr_owner", TokenID: ownerTok.Token.ID, ProjectID: proj.ID, ProjectName: proj.Name, Host: "srv1", InputTokens: 10, OutputTokens: 15, CreatedAt: now})
	e.rec.Record(usage.Event{ID: "e2", UserID: "usr_owner", TokenID: ownerTok.Token.ID, ProjectID: proj.ID, ProjectName: proj.Name, Host: "srv2", InputTokens: 20, OutputTokens: 25, CreatedAt: now.Add(time.Minute)})
	// Member token: 1 request, 5 input + 5 output = 10 total.
	e.rec.Record(usage.Event{ID: "e3", UserID: "usr_member", TokenID: memberTok.Token.ID, ProjectID: proj.ID, ProjectName: proj.Name, Host: "srv1", InputTokens: 5, OutputTokens: 5, CreatedAt: now})
	// Attributed to THIS project but from a token id that does NOT match any
	// currently-attached token (simulates: attached at request time, later
	// detached/deleted) -- must be excluded from every per-token row below
	// but INCLUDED in Total. 100 input + 200 output = 300 total.
	e.rec.Record(usage.Event{ID: "e4", UserID: "usr_owner", TokenID: "tok_since_detached", ProjectID: proj.ID, ProjectName: proj.Name, Host: "srv1", InputTokens: 100, OutputTokens: 200, CreatedAt: now})
	// A DIFFERENT project's usage on the owner's own token -- must not leak
	// into this project's per-token row OR Total.
	e.rec.Record(usage.Event{ID: "e5", UserID: "usr_owner", TokenID: ownerTok.Token.ID, ProjectID: other.ID, ProjectName: other.Name, Host: "srv1", InputTokens: 999, OutputTokens: 999, CreatedAt: now})

	view, err := e.svc.ProjectTokens(e.ctx, owner, proj.ID)
	if err != nil {
		t.Fatalf("ProjectTokens returned %v", err)
	}
	if len(view.Tokens) != 3 {
		t.Fatalf("ProjectTokens.Tokens = %+v, want exactly 3 rows (owner+member+idle)", view.Tokens)
	}
	byID := map[string]ProjectTokenDTO{}
	for _, row := range view.Tokens {
		byID[row.ID] = row
	}

	ownerRow, ok := byID[ownerTok.Token.ID]
	if !ok {
		t.Fatalf("owner token missing from ProjectTokens: %+v", view.Tokens)
	}
	if ownerRow.RequestCount != 2 || ownerRow.InputTokens != 30 || ownerRow.OutputTokens != 40 || ownerRow.TotalTokens != 70 {
		t.Fatalf("owner row usage = %+v, want count=2 input=30 output=40 total=70 (the other project's 999/999 must not leak in)", ownerRow)
	}

	memberRow, ok := byID[memberTok.Token.ID]
	if !ok {
		t.Fatalf("member token missing from ProjectTokens: %+v", view.Tokens)
	}
	if memberRow.RequestCount != 1 || memberRow.InputTokens != 5 || memberRow.OutputTokens != 5 || memberRow.TotalTokens != 10 {
		t.Fatalf("member row usage = %+v, want count=1 input=5 output=5 total=10", memberRow)
	}

	idleRow, ok := byID[idleTok.Token.ID]
	if !ok {
		t.Fatalf("idle token missing from ProjectTokens (must not be omitted): %+v", view.Tokens)
	}
	if idleRow.RequestCount != 0 || idleRow.InputTokens != 0 || idleRow.OutputTokens != 0 || idleRow.TotalTokens != 0 {
		t.Fatalf("idle row usage = %+v, want all zeros", idleRow)
	}

	// Total is the project's TRUE total: e1+e2 (70) + e3 (10) + e4 (300) =
	// 380 -- STRICTLY MORE than the sum of the three per-token rows above
	// (70+10+0=80), because it includes the detached token's 300. It must
	// NEVER include e5's 1998 (a different project).
	const wantTotalRequests = 4 // e1, e2, e3, e4 (not e5)
	const wantTotalTokens = 70 + 10 + 300
	if view.Total.RequestCount != wantTotalRequests {
		t.Fatalf("Total.RequestCount = %d, want %d", view.Total.RequestCount, wantTotalRequests)
	}
	if view.Total.InputTokens != 10+20+5+100 {
		t.Fatalf("Total.InputTokens = %d, want %d", view.Total.InputTokens, 10+20+5+100)
	}
	if view.Total.OutputTokens != 15+25+5+200 {
		t.Fatalf("Total.OutputTokens = %d, want %d", view.Total.OutputTokens, 15+25+5+200)
	}
	if view.Total.TotalTokens != wantTotalTokens {
		t.Fatalf("Total.TotalTokens = %d, want %d", view.Total.TotalTokens, wantTotalTokens)
	}
	sumOfPerTokenRows := ownerRow.TotalTokens + memberRow.TotalTokens + idleRow.TotalTokens
	if view.Total.TotalTokens <= sumOfPerTokenRows {
		t.Fatalf("Total.TotalTokens (%d) must STRICTLY EXCEED the sum of the per-token rows (%d) -- usage from a since-detached token belongs in Total but not in any per-token row", view.Total.TotalTokens, sumOfPerTokenRows)
	}

	// 404-no-leak still holds for a non-manager (mere stranger), even with
	// usage present.
	if _, err := e.svc.ProjectTokens(e.ctx, stranger, proj.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ProjectTokens(stranger) = %v, want ErrProjectNotFound (no-leak)", err)
	}
}

// TestProjectTokens_NoUsageStoreIsZeroNotPanic proves a Service built WITHOUT
// a Usage dependency (s.usage == nil) never panics -- ProjectTokens degrades
// to an all-zero usage view (the nil-guard), never a 500/crash.
func TestProjectTokens_NoUsageStoreIsZeroNotPanic(t *testing.T) {
	dir := NewMemoryDirectory(auth.NewTokenStore())
	svc := NewService(ServiceDeps{
		Users:    dir,
		Groups:   dir,
		Projects: dir,
		Tokens:   dir,
		Routes:   routing.NewMemoryStore(),
		// Usage deliberately left nil.
	})
	ctx := context.Background()
	if err := dir.CreateUser(ctx, store.User{ID: "usr_owner", Email: "usr_owner@example.test", DisplayName: "usr_owner", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	owner := token("usr_owner")
	proj, err := svc.CreateProject(ctx, owner, CreateProjectInput{Name: "Widgets"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	created, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "Owner Token", Scopes: []string{"gateway:use"}, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	view, err := svc.ProjectTokens(ctx, owner, proj.ID)
	if err != nil {
		t.Fatalf("ProjectTokens (no usage store): %v", err)
	}
	if len(view.Tokens) != 1 || view.Tokens[0].ID != created.Token.ID {
		t.Fatalf("ProjectTokens.Tokens = %+v, want [%s]", view.Tokens, created.Token.ID)
	}
	if view.Tokens[0].RequestCount != 0 || view.Tokens[0].TotalTokens != 0 {
		t.Fatalf("token usage with a nil Usage dep = %+v, want zero", view.Tokens[0])
	}
	if view.Total != (ProjectTokenUsageTotalDTO{}) {
		t.Fatalf("Total with a nil Usage dep = %+v, want zero value", view.Total)
	}
}
