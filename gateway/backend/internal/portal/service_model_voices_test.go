// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// seedVoiceServer creates an active, healthy server (name == serverID) with one
// active application declaring flavors, and one active mapping (gatewayModel)
// under it. The application id is "app-"+serverID and the mapping id
// "map-"+serverID, which is what a test asserts on.
func seedVoiceServer(t *testing.T, rs routing.Store, now time.Time, serverID, gatewayModel string, flavors ...string) (mappingID string) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	return seedVoiceApp(t, rs, now, serverID, "app-"+serverID, "map-"+serverID, gatewayModel, 8000, flavors...)
}

// seedVoiceApp adds an active audio.cpp application (declaring flavors, listening
// on port) and one active mapping to the existing server serverID.
func seedVoiceApp(t *testing.T, rs routing.Store, now time.Time, serverID, appID, mappingID, gatewayModel string, port int, flavors ...string) string {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderAudioCpp, Port: port, Scheme: "http", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: gatewayModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	return mappingID
}

// seedVoiceGroup creates a model group with the given status and its members in
// priority order (a member may be a model or another group).
func seedVoiceGroup(t *testing.T, rs routing.Store, now time.Time, name, status string, members ...string) {
	t.Helper()
	ctx := context.Background()
	id := "grp-" + name
	if err := rs.CreateModelGroup(ctx, routing.ModelGroup{ID: id, GatewayModelName: name, DisplayName: name, Status: status, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModelGroup %s: %v", name, err)
	}
	ms := make([]routing.GroupMember, len(members))
	for i, m := range members {
		ms[i] = routing.GroupMember{MemberGatewayName: m, Priority: i}
	}
	if err := rs.SetGroupMembers(ctx, id, ms); err != nil {
		t.Fatalf("SetGroupMembers %s: %v", name, err)
	}
}

func setVoiceVisibility(t *testing.T, rs routing.Store, now time.Time, name, visibility string) {
	t.Helper()
	if err := rs.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: name, Visibility: visibility, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting %s: %v", name, err)
	}
}

// requireVoiceMappingIDs asserts ModelVoiceMappingIDs(principal, name) is exactly
// want (sorted) and, when empty, a non-nil slice.
func requireVoiceMappingIDs(t *testing.T, svc *Service, principal auth.Token, name string, want ...string) {
	t.Helper()
	got, err := svc.ModelVoiceMappingIDs(context.Background(), principal, name)
	if err != nil {
		t.Fatalf("ModelVoiceMappingIDs(%s, %q): %v", principal.UserID, name, err)
	}
	if got == nil {
		t.Fatalf("ModelVoiceMappingIDs(%s, %q) = nil, want a non-nil slice", principal.UserID, name)
	}
	if !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("ModelVoiceMappingIDs(%s, %q) = %v, want %v", principal.UserID, name, got, want)
	}
}

var voiceNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// A hidden or locked model resolves to nothing for a non-admin, exactly as in
// ModelServers; a shown model is unaffected and an admin bypasses the check.
func TestModelVoicesMappingIDsHiddenLockedSuppression(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	seedVoiceServer(t, rs, voiceNow, "srv-hidden", "hidden-tts", routing.APIFlavorOpenAIAudio)
	seedVoiceServer(t, rs, voiceNow, "srv-locked", "locked-tts", routing.APIFlavorOpenAIAudio)
	seedVoiceServer(t, rs, voiceNow, "srv-shown", "shown-tts", routing.APIFlavorOpenAIAudio)
	setVoiceVisibility(t, rs, voiceNow, "hidden-tts", "hidden")
	setVoiceVisibility(t, rs, voiceNow, "locked-tts", "locked")
	nonAdmin := auth.Token{UserID: "usr_plain_voice", Scopes: []string{"gateway:use"}}

	requireVoiceMappingIDs(t, svc, nonAdmin, "hidden-tts")
	requireVoiceMappingIDs(t, svc, nonAdmin, "locked-tts")
	requireVoiceMappingIDs(t, svc, nonAdmin, "shown-tts", "map-srv-shown")

	requireVoiceMappingIDs(t, svc, adminToken(), "hidden-tts", "map-srv-hidden")
	requireVoiceMappingIDs(t, svc, adminToken(), "locked-tts", "map-srv-locked")
}

// The suppression read fails CLOSED: a store blip must surface as an error for a
// non-admin, never as a leaked hidden/locked mapping, for a model name and for a
// group name alike. An admin never reads the settings.
func TestModelVoicesMappingIDsFailsClosedOnSettingsError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("settings table unavailable")
	routes := &failingModelSettingsStore{MemoryStore: routing.NewMemoryStore(), err: boom}
	svc := newModelServersTestServiceWithRoutes(t, voiceNow, fakeLoadedModels{}, routes)
	seedVoiceServer(t, routes, voiceNow, "srv-a", "tts", routing.APIFlavorOpenAIAudio)
	seedVoiceGroup(t, routes, voiceNow, "tts-group", routing.ServerStatusActive, "tts")
	nonAdmin := auth.Token{UserID: "usr_plain_voice", Scopes: []string{"gateway:use"}}

	for _, name := range []string{"tts", "tts-group"} {
		got, err := svc.ModelVoiceMappingIDs(ctx, nonAdmin, name)
		if !errors.Is(err, boom) {
			t.Fatalf("ModelVoiceMappingIDs(non-admin, %q) error = %v, want %v", name, err, boom)
		}
		if len(got) != 0 {
			t.Fatalf("ModelVoiceMappingIDs(non-admin, %q) = %v alongside the error, want none", name, got)
		}
	}
	requireVoiceMappingIDs(t, svc, adminToken(), "tts", "map-srv-a")
	requireVoiceMappingIDs(t, svc, adminToken(), "tts-group", "map-srv-a")
}

// A server inside a provisioned resource group the caller is not provisioned into
// is excluded, even though the model is offered there; a provisioned caller sees
// it. A model only the restricted server offers is empty.
func TestModelVoicesMappingIDsResourceGroupFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e) // srvX restricted (provisioned for usr_u), srvY unrestricted
	audio := routing.APIFlavorOpenAIAudio
	seedVoiceApp(t, e.routes, e.now, f.srvX.ID, "app_x_tts", "map_x_tts", "tts-shared", 8100, audio)
	seedVoiceApp(t, e.routes, e.now, f.srvY.ID, "app_y_tts", "map_y_tts", "tts-shared", 8100, audio)
	seedVoiceApp(t, e.routes, e.now, f.srvX.ID, "app_x_only", "map_x_only", "tts-restricted-only", 8101, audio)
	seedVoiceGroup(t, e.routes, e.now, "tts-mix", routing.ServerStatusActive, "tts-restricted-only", "tts-shared")

	requireVoiceMappingIDs(t, e.svc, token("usr_u"), "tts-shared", "map_x_tts", "map_y_tts")
	requireVoiceMappingIDs(t, e.svc, token("usr_v"), "tts-shared", "map_y_tts")
	requireVoiceMappingIDs(t, e.svc, token("usr_v"), "tts-restricted-only")
	// A group is cut the same way, member by member.
	requireVoiceMappingIDs(t, e.svc, token("usr_u"), "tts-mix", "map_x_only", "map_x_tts", "map_y_tts")
	requireVoiceMappingIDs(t, e.svc, token("usr_v"), "tts-mix", "map_y_tts")
}

// Every active mapping offering the name counts, on any number of servers: the
// ids are the SET the voices union is taken over. An inactive mapping, an
// unhealthy server and a mapping of another model contribute nothing.
func TestModelVoicesMappingIDsCoversEveryOfferingMapping(t *testing.T) {
	ctx := context.Background()
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	audio := routing.APIFlavorOpenAIAudio
	// Seeded out of id order on purpose.
	seedVoiceServer(t, rs, voiceNow, "srv-c", "tts", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-a", "tts", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-b", "tts", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-other", "other-tts", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-sick", "tts", audio)
	if err := rs.UpdateAIServer(ctx, routing.AIServer{ID: "srv-sick", Name: "srv-sick", Domain: "srv-sick.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnhealthy, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("UpdateAIServer: %v", err)
	}
	seedVoiceApp(t, rs, voiceNow, "srv-a", "app-a2", "map-a2-disabled", "tts", 8001, audio)
	if err := rs.UpdateMapping(ctx, routing.ModelMapping{ID: "map-a2-disabled", ApplicationID: "app-a2", GatewayModelName: "tts", AppModelName: "tts", Status: routing.ServerStatusDisabled, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}

	requireVoiceMappingIDs(t, svc, adminToken(), "tts", "map-srv-a", "map-srv-b", "map-srv-c")
}

// Only a mapping a speech request could be routed to counts: an application that
// does not declare openai_audio is out (viewServesAudio, the listing's own rule),
// though it offers the very same name.
func TestModelVoicesMappingIDsDropsMappingsThatDoNotServeSpeech(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	seedVoiceServer(t, rs, voiceNow, "srv-audio", "tts", routing.APIFlavorOpenAIAudio)
	seedVoiceServer(t, rs, voiceNow, "srv-text", "tts", routing.APIFlavorOpenAI)

	requireVoiceMappingIDs(t, svc, adminToken(), "tts", "map-srv-audio")
}

// A group name expands to its leaf members, nested subgroups included, and the
// result is the mappings of ALL of them (a union's input, not an intersection's).
func TestModelVoicesMappingIDsExpandsAGroupToItsMembers(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	audio := routing.APIFlavorOpenAIAudio
	seedVoiceServer(t, rs, voiceNow, "srv-1", "tts-one", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-2", "tts-two", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-3", "tts-three", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-x", "tts-outside", audio)
	seedVoiceGroup(t, rs, voiceNow, "tts-inner", routing.ServerStatusActive, "tts-three")
	seedVoiceGroup(t, rs, voiceNow, "tts-group", routing.ServerStatusActive, "tts-one", "tts-two", "tts-inner")
	nonAdmin := auth.Token{UserID: "usr_plain_voice", Scopes: []string{"gateway:use"}}

	requireVoiceMappingIDs(t, svc, nonAdmin, "tts-group", "map-srv-1", "map-srv-2", "map-srv-3")
	requireVoiceMappingIDs(t, svc, nonAdmin, "tts-inner", "map-srv-3")
}

// The hidden/locked gate applies to the name that was ASKED ABOUT: a group's own
// hidden/locked setting suppresses the whole group for a non-admin, but a member's
// own setting does not narrow a visible group. A hidden or locked model is still a
// full member of it ("locked" means reachable through a group, not directly), so a
// non-admin gets the member's mapping THROUGH the group while the model's own name
// stays suppressed. An admin bypasses the gate.
func TestModelVoicesMappingIDsGroupVisibility(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	audio := routing.APIFlavorOpenAIAudio
	seedVoiceServer(t, rs, voiceNow, "srv-shown", "tts-shown", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-hidden", "tts-hidden", audio)
	seedVoiceServer(t, rs, voiceNow, "srv-locked", "tts-locked", audio)
	seedVoiceGroup(t, rs, voiceNow, "grp-open", routing.ServerStatusActive, "tts-shown", "tts-hidden", "tts-locked")
	seedVoiceGroup(t, rs, voiceNow, "grp-closed", routing.ServerStatusActive, "tts-shown")
	setVoiceVisibility(t, rs, voiceNow, "tts-hidden", "hidden")
	setVoiceVisibility(t, rs, voiceNow, "tts-locked", "locked")
	setVoiceVisibility(t, rs, voiceNow, "grp-closed", "locked")
	nonAdmin := auth.Token{UserID: "usr_plain_voice", Scopes: []string{"gateway:use"}}

	requireVoiceMappingIDs(t, svc, nonAdmin, "grp-open", "map-srv-hidden", "map-srv-locked", "map-srv-shown")
	requireVoiceMappingIDs(t, svc, nonAdmin, "tts-hidden")
	requireVoiceMappingIDs(t, svc, nonAdmin, "tts-locked")
	requireVoiceMappingIDs(t, svc, nonAdmin, "grp-closed")

	requireVoiceMappingIDs(t, svc, adminToken(), "grp-open", "map-srv-hidden", "map-srv-locked", "map-srv-shown")
	requireVoiceMappingIDs(t, svc, adminToken(), "grp-closed", "map-srv-shown")
}

// An inactive group offers nothing, as it is not resolvable; neither does a group
// with no member that has a mapping.
func TestModelVoicesMappingIDsInactiveOrEmptyGroup(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	seedVoiceServer(t, rs, voiceNow, "srv-1", "tts-one", routing.APIFlavorOpenAIAudio)
	seedVoiceGroup(t, rs, voiceNow, "grp-off", routing.ServerStatusDisabled, "tts-one")
	seedVoiceGroup(t, rs, voiceNow, "grp-empty", routing.ServerStatusActive, "no-such-model")

	requireVoiceMappingIDs(t, svc, adminToken(), "grp-off")
	requireVoiceMappingIDs(t, svc, adminToken(), "grp-empty")
}

// An unknown name and a blank one are an empty (non-nil) slice, not an error.
func TestModelVoicesMappingIDsUnknownNameIsEmpty(t *testing.T) {
	svc, rs := newModelServersTestService(t, voiceNow, fakeLoadedModels{})
	seedVoiceServer(t, rs, voiceNow, "srv-1", "tts", routing.APIFlavorOpenAIAudio)

	for _, name := range []string{"does-not-exist", "", "  "} {
		requireVoiceMappingIDs(t, svc, adminToken(), name)
	}
}
