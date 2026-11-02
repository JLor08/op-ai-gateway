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
	"testing"
	"time"
)

// seedRoleMapping seeds one server + application of appType + one active mapping
// named gateway with the given capability rows and, when specType or binary is
// set, a runtime spec of that explicit type (or, with an empty type, an
// auto-detect spec launching binary). The server, application and mapping ids
// derive from srvID, so two calls with the same gateway name and different srvID
// seed two mappings of ONE name -- which the per-name role fold needs.
func seedRoleMapping(t *testing.T, rs *routing.MemoryStore, srvID, appType, specType, binary, gateway string, rows ...routing.CapabilityRow) {
	t.Helper()
	ctx := context.Background()
	appID, mappingID := "app_"+srvID, "map_"+srvID
	port := 8000
	if appType == routing.ProviderServerAgent {
		port = 8081
	}
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: appType, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	if len(rows) > 0 {
		if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
	if specType != "" || binary != "" {
		if err := rs.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec_" + srvID, MappingID: mappingID, Enabled: true, Binary: binary, Args: "[]", Env: "{}", Type: specType, APIFlavors: []string{routing.APIFlavorOpenAI}}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
		}
	}
}

// ModelDTO.Role is the listing's derived role (routing.DeriveRole): per
// mapping, the effective type of its runtime spec (or, with no spec, the type
// its application declares) and its yes verdicts; per name, the best-ranked
// role among its mappings; per group, the best-ranked role among its offerable
// members. Every shape that is a distinct decision is pinned.
func TestModelsResponseRole(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	agent := routing.ProviderServerAgent
	transcriptionYes, speechYes := transcriptionRow(routing.CapabilityYes), speechRow(routing.CapabilityYes)

	// A spec's effective type wins over everything but a capability.
	seedRoleMapping(t, rs, "s_asr", agent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "asr-model", transcriptionYes)
	seedRoleMapping(t, rs, "s_tts", agent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "tts-model", speechYes)
	seedRoleMapping(t, rs, "s_audio_bare", agent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "audio-bare")
	seedRoleMapping(t, rs, "s_sd", agent, string(routing.RuntimeSpecTypeStableDiffusionCpp), "/opt/sd/sd-server", "sd-bare")
	seedRoleMapping(t, rs, "s_vllm", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "vllm-bare")
	seedRoleMapping(t, rs, "s_auto", agent, "", "/usr/local/bin/llama-server", "auto-detected-llama")
	seedRoleMapping(t, rs, "s_custom", agent, string(routing.RuntimeSpecTypeCustom), "/opt/x/serve.sh", "custom-bare")
	seedRoleMapping(t, rs, "s_emb", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "embed-on-vllm", embeddingRow(routing.CapabilityYes))
	seedRoleMapping(t, rs, "s_rr", agent, string(routing.RuntimeSpecTypeLlamaCpp), "/usr/local/bin/llama-server", "rerank-on-llama", rerankRow(routing.CapabilityYes))
	// A "no" verdict is not evidence: the bare type decides.
	seedRoleMapping(t, rs, "s_no", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "vllm-with-no", embeddingRow(routing.CapabilityNo))

	// No spec: the application's own type is the coarse fallback, so a plain
	// vLLM or stable-diffusion.cpp application is not "other" merely because no
	// agent manages it; a proxy application (litellm) hosts anything and says
	// nothing, and a capability still outranks the type.
	seedRoleMapping(t, rs, "a_vllm", routing.ProviderVLLM, "", "", "app-vllm")
	seedRoleMapping(t, rs, "a_sd", routing.ProviderStableDiffusionCpp, "", "", "app-sd")
	seedRoleMapping(t, rs, "a_audio", routing.ProviderAudioCpp, "", "", "app-audio-bare")
	seedRoleMapping(t, rs, "a_proxy", routing.ProviderLiteLLM, "", "", "app-proxy")
	seedRoleMapping(t, rs, "a_proxy_emb", routing.ProviderLiteLLM, "", "", "app-proxy-embed", embeddingRow(routing.CapabilityYes))
	seedRoleMapping(t, rs, "a_vllm_asr", routing.ProviderVLLM, "", "", "app-vllm-asr", transcriptionYes)

	// One name, several mappings: the best-ranked role wins, in either order, so
	// a mapping that knows nothing never overrules one that establishes a role.
	seedRoleMapping(t, rs, "m1", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "split-chat-other")
	seedRoleMapping(t, rs, "m2", routing.ProviderLiteLLM, "", "", "split-chat-other")
	seedRoleMapping(t, rs, "m3", routing.ProviderLiteLLM, "", "", "split-other-chat")
	seedRoleMapping(t, rs, "m4", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "split-other-chat")
	seedRoleMapping(t, rs, "m5", agent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "split-chat-asr")
	seedRoleMapping(t, rs, "m6", agent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "split-chat-asr", transcriptionYes)

	offerGroup(t, rs, "grp_chat_asr", "group-chat-asr", "vllm-bare", "asr-model")
	offerGroup(t, rs, "grp_chat_chat", "group-chat-chat", "vllm-bare", "auto-detected-llama")
	offerGroup(t, rs, "grp_emb_other", "group-emb-other", "custom-bare", "embed-on-vllm")

	svc := offerSvc(rs, nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-asr":   {To: "asr-model", Offer: true},
		"alias-embed": {To: "embed-on-vllm", Offer: true},
		"alias-group": {To: "group-chat-asr", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))

	for name, want := range map[string]routing.ModelRole{
		"asr-model":           routing.RoleASR,
		"tts-model":           routing.RoleTTS,
		"audio-bare":          routing.RoleOther, // audio_cpp alone cannot say asr or tts
		"sd-bare":             routing.RoleImage,
		"vllm-bare":           routing.RoleChat,
		"auto-detected-llama": routing.RoleChat, // Type "" resolves through the binary
		"custom-bare":         routing.RoleOther,
		"embed-on-vllm":       routing.RoleEmbedding,
		"rerank-on-llama":     routing.RoleReranker,
		"vllm-with-no":        routing.RoleChat,
		"app-vllm":            routing.RoleChat,
		"app-sd":              routing.RoleImage,
		"app-audio-bare":      routing.RoleOther,
		"app-proxy":           routing.RoleOther,
		"app-proxy-embed":     routing.RoleEmbedding,
		"app-vllm-asr":        routing.RoleASR, // a verdict outranks the application's type
		"split-chat-other":    routing.RoleChat,
		"split-other-chat":    routing.RoleChat,
		"split-chat-asr":      routing.RoleASR,
		"group-chat-asr":      routing.RoleASR,
		"group-chat-chat":     routing.RoleChat,
		"group-emb-other":     routing.RoleEmbedding,
		"alias-asr":           routing.RoleASR, // an alias carries its target's role
		"alias-embed":         routing.RoleEmbedding,
		"alias-group":         routing.RoleASR,
	} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if got.Role != string(want) {
			t.Errorf("%s role = %q, want %q", name, got.Role, want)
		}
	}
	// Every listed row names a real role, so a consumer never sees "".
	for name, dto := range byID {
		if dto.Role == "" {
			t.Errorf("%s has an empty role", name)
		}
	}
}

// The role rides the listing's JSON as "role" -- the key the portal's editor
// reads -- and the unconfigured-routing seed list carries a valid one too.
func TestModelsResponseRoleIsOnTheWire(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	seedRoleMapping(t, rs, "s_asr", routing.ProviderServerAgent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "asr-model", transcriptionRow(routing.CapabilityYes))
	seedRoleMapping(t, rs, "s_vllm", routing.ProviderServerAgent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "vllm-bare")

	raw, err := json.Marshal(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	roles := map[string]any{}
	for _, row := range wire.Data {
		roles[row["id"].(string)] = row["role"]
	}
	if roles["asr-model"] != "asr" || roles["vllm-bare"] != "chat" {
		t.Errorf("wire roles = %v, want asr-model=asr and vllm-bare=chat", roles)
	}

	seed := NewService(ServiceDeps{Usage: nil, Clock: func() time.Time { return offeringTime }}).Models(ctx, auth.Token{UserID: "usr_1"})
	if len(seed.Data) == 0 {
		t.Fatal("precondition: the seed listing is empty")
	}
	for _, dto := range seed.Data {
		if dto.Role != string(routing.RoleOther) {
			t.Errorf("seed model %s role = %q, want %q", dto.ID, dto.Role, routing.RoleOther)
		}
	}
}

// A degraded capability read (a store error) lists every model with the role its
// runtime type alone supports -- never an empty one -- and says so in the log.
func TestModelsResponseRoleDegradesToTheTypeOnACapabilityReadFailure(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	seedRoleMapping(t, rs, "s_asr", routing.ProviderServerAgent, string(routing.RuntimeSpecTypeAudioCpp), "/opt/audiocpp/audiocpp_server", "asr-model", transcriptionRow(routing.CapabilityYes))
	seedRoleMapping(t, rs, "s_vllm", routing.ProviderServerAgent, string(routing.RuntimeSpecTypeVLLM), "/opt/vllm/bin/vllm", "vllm-bare")

	svc := offerSvc(&failingCapabilityStore{MemoryStore: rs, err: errors.New("capability store down")}, nil)
	var byID map[string]ModelDTO
	logged := captureSlog(t, func() { byID = modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"})) })

	if got := byID["vllm-bare"].Role; got != string(routing.RoleChat) {
		t.Errorf("vllm-bare role = %q, want chat from its type", got)
	}
	if got := byID["asr-model"].Role; got != string(routing.RoleOther) {
		t.Errorf("asr-model role = %q, want other: the verdict that made it asr could not be read", got)
	}
	if logged == "" {
		t.Error("the degraded role listing logged nothing")
	}
}

// AgentRuntimeSpecDTO.Role is the SAME derivation the listing uses, shipped on
// the pushed config so the agent's eviction and the editor's grouping read one
// value. It is derived from the spec's EFFECTIVE type (an auto-detect spec
// resolves through its binary) and its mapping's yes verdicts.
func TestAgentRuntimeConfigPushesDerivedRole(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	seedSpec := func(model, binary, specType string, rows ...routing.CapabilityRow) string {
		t.Helper()
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: model, AppModelName: model})
		if err != nil {
			t.Fatalf("CreateMapping(%s): %v", model, err)
		}
		if len(rows) > 0 {
			if err := routeStore.UpsertMappingCapabilities(ctx, mapping.ID, rows); err != nil {
				t.Fatalf("UpsertMappingCapabilities(%s): %v", model, err)
			}
		}
		specID := "rspec_" + compactRandomHex(16)
		if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: specID, MappingID: mapping.ID, Enabled: true, Binary: binary, Args: "[]", Env: "{}", Type: specType, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed spec(%s): %v", model, err)
		}
		return specID
	}

	visionYes := routing.CapabilityRow{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now}
	want := map[string]routing.ModelRole{}
	for _, tc := range []struct {
		model, binary, specType string
		rows                    []routing.CapabilityRow
		role                    routing.ModelRole
	}{
		{"m-asr", "/opt/audiocpp/audiocpp_server", "audio_cpp", []routing.CapabilityRow{transcriptionRow(routing.CapabilityYes)}, routing.RoleASR},
		{"m-tts", "/opt/audiocpp/audiocpp_server", "audio_cpp", []routing.CapabilityRow{speechRow(routing.CapabilityYes)}, routing.RoleTTS},
		{"m-image", "/opt/sd/sd-server", "stable_diffusion_cpp", nil, routing.RoleImage},
		{"m-embed", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{embeddingRow(routing.CapabilityYes)}, routing.RoleEmbedding},
		{"m-rerank", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{rerankRow(routing.CapabilityYes)}, routing.RoleReranker},
		{"m-chat", "/opt/vllm/bin/vllm", "vllm", nil, routing.RoleChat},
		{"m-auto", "/usr/local/bin/llama-server", "", nil, routing.RoleChat}, // Type "" resolves through the binary
		{"m-vision-custom", "/opt/x/serve.sh", "custom", []routing.CapabilityRow{visionYes}, routing.RoleChat},
		{"m-other", "/opt/x/serve.sh", "custom", nil, routing.RoleOther},
		{"m-audio-bare", "/opt/audiocpp/audiocpp_server", "audio_cpp", nil, routing.RoleOther},
		{"m-no-verdict", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{embeddingRow(routing.CapabilityNo)}, routing.RoleChat},
	} {
		want[seedSpec(tc.model, tc.binary, tc.specType, tc.rows...)] = tc.role
	}

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != len(want) {
		t.Fatalf("pushed %d specs, want %d", len(cfg.Specs), len(want))
	}
	raw, err := json.Marshal(cfg.Specs)
	if err != nil {
		t.Fatalf("Marshal specs: %v", err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("Unmarshal specs: %v", err)
	}
	for i, spec := range cfg.Specs {
		if got := spec.Role; got != string(want[spec.ID]) {
			t.Errorf("spec %s (%s) role = %q, want %q", spec.ID, spec.Model, got, want[spec.ID])
		}
		if got, _ := wire[i]["role"].(string); got != string(want[spec.ID]) {
			t.Errorf("spec %s wire role = %q, want %q", spec.ID, got, want[spec.ID])
		}
	}
}

// The role is derived, never stored: an operator's verdict change moves it on the
// very next config build, and because the role is part of the pushed document
// its ETag moves too -- which is how a running agent learns of it.
func TestAgentRuntimeConfigRoleFollowsAVerdictChange(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "bge", AppModelName: "bge"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_bge", MappingID: mapping.ID, Enabled: true, Binary: "/opt/vllm/bin/vllm", Args: "[]", Env: "{}", Type: "vllm", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	before, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if got := before.Specs[0].Role; got != string(routing.RoleChat) {
		t.Fatalf("role before the verdict = %q, want chat", got)
	}

	if err := routeStore.UpsertMappingCapabilities(ctx, mapping.ID, []routing.CapabilityRow{embeddingRow(routing.CapabilityYes)}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	after, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if got := after.Specs[0].Role; got != string(routing.RoleEmbedding) {
		t.Errorf("role after the verdict = %q, want embedding", got)
	}
	if before.ETag == after.ETag {
		t.Errorf("etag unchanged (%s) across a role change: a running agent would never be pushed the new role", after.ETag)
	}
}

// A failed capability read must PROPAGATE out of AgentRuntimeConfig like every
// other store error there (see its doc comment): a role guessed from the type
// alone would flip an embedding model to chat for one push, and a 500 costs the
// agent one skipped poll while it keeps its last known-good config.
func TestAgentRuntimeConfigPropagatesCapabilityReadFailure(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_m", MappingID: mapping.ID, Enabled: true, Binary: "/opt/vllm/bin/vllm", Args: "[]", Env: "{}", Type: "vllm", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	boom := errors.New("capability store down")
	failing := NewService(ServiceDeps{
		Users:  svc.users,
		Groups: svc.groups,
		Routes: &failingCapabilityStore{MemoryStore: routeStore, err: boom},
		Clock:  func() time.Time { return now },
	})
	dto, err := failing.AgentRuntimeConfig(ctx, server.ID)
	if err == nil {
		t.Fatalf("AgentRuntimeConfig returned dto = %#v with a nil error; a capability read failure must be propagated", dto)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}
