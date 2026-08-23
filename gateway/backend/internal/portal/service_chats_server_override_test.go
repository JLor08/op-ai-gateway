// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// chatServerOverrideEnv wires a Service backed by a real *MemoryDirectory
// (Users), a real *routing.MemoryStore (servers, for AuthorizeServerManage /
// ServerOwners), and a real *store.MemoryChatStore, for exercising the
// PrepareChatRun per-chat server_override self-heal (mirrors
// serverOverrideTestEnv in service_server_override_test.go — kept as its own
// type here rather than extending that one, so this file stays independent of
// T3's test scaffolding).
type chatServerOverrideEnv struct {
	t          *testing.T
	ctx        context.Context
	dir        *MemoryDirectory
	routeStore *routing.MemoryStore
	chatStore  *store.MemoryChatStore
	svc        *Service
	now        time.Time
}

// newChatServerOverrideEnv builds the env; cipher may be nil (RAM-fallback
// plain chat storage, KeyVersion 0) or a real *capture.Cipher (sealed chat
// storage, KeyVersion > 0 — see capture.KeyVersion).
func newChatServerOverrideEnv(t *testing.T, cipher *capture.Cipher) *chatServerOverrideEnv {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	routeStore := routing.NewMemoryStore()
	chatStore := store.NewMemoryChatStore(0)
	svc := NewService(ServiceDeps{
		Users: dir, Tokens: dir, Routes: routeStore, Chats: chatStore, Cipher: cipher,
		Clock: func() time.Time { return now },
	})
	return &chatServerOverrideEnv{t: t, ctx: context.Background(), dir: dir, routeStore: routeStore, chatStore: chatStore, svc: svc, now: now}
}

func (e *chatServerOverrideEnv) createUser(id, role string) {
	e.t.Helper()
	u := store.User{ID: id, Email: id + "@example.test", DisplayName: id, Role: role, Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.dir.CreateUser(e.ctx, u); err != nil {
		e.t.Fatalf("create user %s: %v", id, err)
	}
}

// seedServer creates an active, healthy server named serverID, owned
// (ServerOwners) by ownerUserID ("" = no owner set, so no plain-user token can
// manage it).
func (e *chatServerOverrideEnv) seedServer(serverID, ownerUserID string) {
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

// runSettingsFromContent parses the "settings" field out of a chat's opaque
// content blob into ChatRunSettings, as PrepareChatRun's own doc.Settings
// round-trip does.
func runSettingsFromContent(t *testing.T, content json.RawMessage) ChatRunSettings {
	t.Helper()
	var doc struct {
		Settings ChatRunSettings `json:"settings"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("unmarshal chat content settings: %v (content: %s)", err, content)
	}
	return doc.Settings
}

// TestPrepareChatRunServerOverrideRoundTripsThroughEncryptedBlob proves a
// manageable server_override survives PrepareChatRun's self-heal UNCHANGED,
// is returned as the effective settings, AND is durably persisted through a
// SEALED (KeyVersion > 0, i.e. actually encrypted-at-rest, not merely
// gzipped) chat blob — reopened via a fresh GetChat call reads back the same
// values, proving the round trip through encryption, not just through
// in-process state.
func TestPrepareChatRunServerOverrideRoundTripsThroughEncryptedBlob(t *testing.T) {
	cipher, err := capture.New(captureTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	e := newChatServerOverrideEnv(t, cipher)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := chatToken("usr_owner")

	created, err := e.svc.CreateChat(e.ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	settings := ChatRunSettings{Model: "m", ServerOverride: "srv-a", ServerOverrideForceUnreachable: true}
	_, effective, err := e.svc.PrepareChatRun(e.ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"hallo"`),
		Settings:    settings,
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if effective.ServerOverride != "srv-a" || !effective.ServerOverrideForceUnreachable {
		t.Fatalf("returned effective settings = %+v, want ServerOverride=srv-a ForceUnreachable=true (manageable, must survive unchanged)", effective)
	}

	// The stored blob must actually be sealed (KeyVersion capture.KeyVersion),
	// not plain — proving this is a REAL encrypted-at-rest round trip.
	row, err := e.chatStore.ChatByID(e.ctx, created.ID)
	if err != nil {
		t.Fatalf("store ChatByID: %v", err)
	}
	if row.KeyVersion != capture.KeyVersion {
		t.Fatalf("stored KeyVersion = %d, want %d (sealed)", row.KeyVersion, capture.KeyVersion)
	}

	// Reopen via a FRESH GetChat (decrypts the sealed blob) and read the
	// persisted settings back out of the content, independent of the
	// PrepareChatRun return value above.
	got, err := e.svc.GetChat(e.ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	persisted := runSettingsFromContent(t, got.Content)
	if persisted.ServerOverride != "srv-a" {
		t.Fatalf("persisted ServerOverride = %q, want srv-a (round-tripped through the encrypted blob)", persisted.ServerOverride)
	}
	if !persisted.ServerOverrideForceUnreachable {
		t.Fatalf("persisted ServerOverrideForceUnreachable = false, want true (round-tripped through the encrypted blob)")
	}
}

// TestPrepareChatRunSelfHealsUnmanageableServerOverride is the core
// mutation-sensitive self-heal scenario for the CHAT-level override
// (mirrors T3's token self-heal, service_server_override_test.go): the owner
// does NOT manage srv-a (it is owned by a different user), so PrepareChatRun
// must (a) return the effective settings with BOTH ServerOverride and
// ServerOverrideForceUnreachable cleared, (b) persist the chat WITHOUT the
// override (a later reopen must not see it either), and (c) still succeed —
// never rejecting/failing the run over an unmanageable override.
func TestPrepareChatRunSelfHealsUnmanageableServerOverride(t *testing.T) {
	e := newChatServerOverrideEnv(t, nil)
	e.createUser("usr_owner", "user")
	e.createUser("usr_other", "user")
	e.seedServer("srv-a", "usr_other") // NOT owned/managed by usr_owner
	owner := chatToken("usr_owner")

	created, err := e.svc.CreateChat(e.ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	_, effective, err := e.svc.PrepareChatRun(e.ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"hallo"`),
		Settings:    ChatRunSettings{Model: "m", ServerOverride: "srv-a", ServerOverrideForceUnreachable: true},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun should self-heal an unmanageable override, not fail the run; got err: %v", err)
	}
	if effective.ServerOverride != "" {
		t.Fatalf("effective.ServerOverride = %q, want cleared to \"\"", effective.ServerOverride)
	}
	if effective.ServerOverrideForceUnreachable {
		t.Fatalf("effective.ServerOverrideForceUnreachable = true, want cleared alongside the override")
	}
	// The model (an unrelated field) must still come through untouched.
	if effective.Model != "m" {
		t.Fatalf("effective.Model = %q, want m (the self-heal must not disturb unrelated settings)", effective.Model)
	}

	got, err := e.svc.GetChat(e.ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	persisted := runSettingsFromContent(t, got.Content)
	if persisted.ServerOverride != "" || persisted.ServerOverrideForceUnreachable {
		t.Fatalf("persisted settings = %+v, want both cleared (self-heal must be durably written, not just returned)", persisted)
	}
}

// TestPrepareChatRunSelfHealsAfterManagementRevoked proves the self-heal
// re-checks CURRENT rights on every call, not just at chat-creation time: a
// server_override that was manageable and got persisted on an earlier run is
// silently cleared on the NEXT run once the owner's management grant is
// revoked in between.
func TestPrepareChatRunSelfHealsAfterManagementRevoked(t *testing.T) {
	e := newChatServerOverrideEnv(t, nil)
	e.createUser("usr_owner", "user")
	e.seedServer("srv-a", "usr_owner")
	owner := chatToken("usr_owner")

	created, err := e.svc.CreateChat(e.ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// First run: manageable, kept.
	_, effective, err := e.svc.PrepareChatRun(e.ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"turn one"`),
		Settings:    ChatRunSettings{Model: "m", ServerOverride: "srv-a", ServerOverrideForceUnreachable: true},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun (turn one): %v", err)
	}
	if effective.ServerOverride != "srv-a" {
		t.Fatalf("precondition: turn one effective.ServerOverride = %q, want srv-a", effective.ServerOverride)
	}

	// Revoke usr_owner's management of srv-a between the two runs.
	if err := e.routeStore.SetServerOwners(e.ctx, "srv-a", nil); err != nil {
		t.Fatalf("SetServerOwners(revoke): %v", err)
	}

	// Second run re-submits the SAME (now stale) settings, as a real client
	// resubmitting its last-known ChatRunSettings would.
	_, effective2, err := e.svc.PrepareChatRun(e.ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"turn two"`),
		Settings:    ChatRunSettings{Model: "m", ServerOverride: "srv-a", ServerOverrideForceUnreachable: true},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun (turn two): %v", err)
	}
	if effective2.ServerOverride != "" {
		t.Fatalf("turn two effective.ServerOverride = %q, want cleared (management was revoked)", effective2.ServerOverride)
	}
	if effective2.ServerOverrideForceUnreachable {
		t.Fatalf("turn two effective.ServerOverrideForceUnreachable = true, want cleared")
	}

	got, err := e.svc.GetChat(e.ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	persisted := runSettingsFromContent(t, got.Content)
	if persisted.ServerOverride != "" || persisted.ServerOverrideForceUnreachable {
		t.Fatalf("persisted settings after revoke = %+v, want both cleared", persisted)
	}
}

// TestPrepareChatRunServerOverrideBlankStaysBlank proves the "blank in stays
// blank out" carve-out (validateServerOverride's own early return): a chat
// with no configured override never even calls AuthorizeServerManage and its
// effective settings simply have an empty ServerOverride, exercised here via
// a user with NO servers at all (any authorization call would 404 rather
// than pass, so a passing PrepareChatRun call proves the call was skipped).
func TestPrepareChatRunServerOverrideBlankStaysBlank(t *testing.T) {
	e := newChatServerOverrideEnv(t, nil)
	e.createUser("usr_owner", "user")
	owner := chatToken("usr_owner")

	created, err := e.svc.CreateChat(e.ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	_, effective, err := e.svc.PrepareChatRun(e.ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"hallo"`),
		Settings:    ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if effective.ServerOverride != "" || effective.ServerOverrideForceUnreachable {
		t.Fatalf("effective = %+v, want both blank/false", effective)
	}
}
