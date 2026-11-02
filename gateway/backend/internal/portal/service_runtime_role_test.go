// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// RuntimeSpecDTO.Role is the portal-facing twin of AgentRuntimeSpecDTO.Role: the
// SAME routing.DeriveRole over the spec's effective type and its mapping's yes
// verdicts, so the editor's per-spec grouping and the agent's role-aware eviction
// read one derivation. It is a response-only echo: derived on every read, never
// stored, and not part of PutRuntimeSpecRequest.

// seedRoleSpecMapping creates a server_agent mapping of model on the app and,
// when binary is non-empty, a spec of the given explicit type launching it.
func seedRoleSpecMapping(t *testing.T, svc *Service, routeStore *routing.MemoryStore, appID, model, binary, specType string, now time.Time, rows ...routing.CapabilityRow) string {
	t.Helper()
	ctx := context.Background()
	mapping, err := svc.CreateMapping(ctx, ownerToken(), appID, CreateMappingRequest{GatewayModelName: model, AppModelName: model})
	if err != nil {
		t.Fatalf("CreateMapping(%s): %v", model, err)
	}
	if len(rows) > 0 {
		if err := routeStore.UpsertMappingCapabilities(ctx, mapping.ID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities(%s): %v", model, err)
		}
	}
	if binary != "" {
		if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_" + model, MappingID: mapping.ID, Enabled: true, Binary: binary, Args: "[]", Env: "{}", Type: specType, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed spec(%s): %v", model, err)
		}
	}
	return mapping.ID
}

// A spec with transcription:yes is an asr; the other roles follow DeriveRole,
// and an UNCONFIGURED mapping (no spec row) reads its role off the synthesized
// document's effective type ("custom") plus its verdicts.
func TestGetRuntimeSpecCarriesTheDerivedRole(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	for _, tc := range []struct {
		name, binary, specType string
		rows                   []routing.CapabilityRow
		want                   routing.ModelRole
	}{
		{"m-asr", "/opt/audiocpp/audiocpp_server", "audio_cpp", []routing.CapabilityRow{transcriptionRow(routing.CapabilityYes)}, routing.RoleASR},
		{"m-tts", "/opt/audiocpp/audiocpp_server", "audio_cpp", []routing.CapabilityRow{speechRow(routing.CapabilityYes)}, routing.RoleTTS},
		{"m-image", "/opt/sd/sd-server", "stable_diffusion_cpp", nil, routing.RoleImage},
		{"m-embed", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{embeddingRow(routing.CapabilityYes)}, routing.RoleEmbedding},
		{"m-rerank", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{rerankRow(routing.CapabilityYes)}, routing.RoleReranker},
		{"m-chat", "/opt/vllm/bin/vllm", "vllm", nil, routing.RoleChat},
		{"m-auto", "/usr/local/bin/llama-server", "", nil, routing.RoleChat}, // Type "" resolves through the binary
		{"m-other", "/opt/x/serve.sh", "custom", nil, routing.RoleOther},
		{"m-no-verdict", "/opt/vllm/bin/vllm", "vllm", []routing.CapabilityRow{embeddingRow(routing.CapabilityNo)}, routing.RoleChat},
		// No spec row at all: the synthesized document still names a role.
		{"m-unconfigured-asr", "", "", []routing.CapabilityRow{transcriptionRow(routing.CapabilityYes)}, routing.RoleASR},
		{"m-unconfigured", "", "", nil, routing.RoleOther},
	} {
		mappingID := seedRoleSpecMapping(t, svc, routeStore, app.ID, tc.name, tc.binary, tc.specType, now, tc.rows...)
		dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec(%s): %v", tc.name, err)
		}
		if dto.Role != string(tc.want) {
			t.Errorf("%s: GetRuntimeSpec role = %q, want %q", tc.name, dto.Role, tc.want)
		}
	}
}

// The role rides the JSON as "role" -- the key the portal's editor reads -- and
// the PUT response carries it too: the portal commits that response into the
// same per-mapping cache a GET fills, so a role-less PUT body would drop a spec
// out of its group the moment it is saved.
func TestRuntimeSpecRoleIsOnTheWireOfGetAndPut(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mappingID := seedRoleSpecMapping(t, svc, routeStore, app.ID, "whisper", "", "", now, transcriptionRow(routing.CapabilityYes))

	wireRole := func(label string, dto RuntimeSpecDTO) {
		t.Helper()
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("%s: marshal: %v", label, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("%s: unmarshal: %v", label, err)
		}
		if got := string(keys["role"]); got != `"asr"` {
			t.Errorf("%s: wire role = %s, want \"asr\"", label, got)
		}
	}

	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	wireRole("GET (unconfigured)", got)

	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{Enabled: true, Binary: "/opt/audiocpp/audiocpp_server", Type: "audio_cpp"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	wireRole("PUT response", put)

	got, err = svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec (configured): %v", err)
	}
	wireRole("GET (configured)", got)
}

// The portal's role and the agent's pushed role are one derivation: for the same
// spec they can never differ.
func TestRuntimeSpecRoleMatchesThePushedAgentRole(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	ids := map[string]string{}
	ids["m-asr"] = seedRoleSpecMapping(t, svc, routeStore, app.ID, "m-asr", "/opt/audiocpp/audiocpp_server", "audio_cpp", now, transcriptionRow(routing.CapabilityYes))
	ids["m-embed"] = seedRoleSpecMapping(t, svc, routeStore, app.ID, "m-embed", "/opt/vllm/bin/vllm", "vllm", now, embeddingRow(routing.CapabilityYes))
	ids["m-chat"] = seedRoleSpecMapping(t, svc, routeStore, app.ID, "m-chat", "/opt/vllm/bin/vllm", "vllm", now)

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != len(ids) {
		t.Fatalf("pushed %d specs, want %d", len(cfg.Specs), len(ids))
	}
	for _, pushed := range cfg.Specs {
		dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), ids[pushed.Model])
		if err != nil {
			t.Fatalf("GetRuntimeSpec(%s): %v", pushed.Model, err)
		}
		if dto.Role != pushed.Role {
			t.Errorf("%s: portal role %q != pushed role %q", pushed.Model, dto.Role, pushed.Role)
		}
	}
}

// The role is derived, never stored: an operator's verdict change moves it on the
// next read.
func TestRuntimeSpecRoleFollowsAVerdictChange(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mappingID := seedRoleSpecMapping(t, svc, routeStore, app.ID, "bge", "/opt/vllm/bin/vllm", "vllm", now)

	before, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if before.Role != string(routing.RoleChat) {
		t.Fatalf("role before the verdict = %q, want chat", before.Role)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, mappingID, []routing.CapabilityRow{embeddingRow(routing.CapabilityYes)}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	after, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if after.Role != string(routing.RoleEmbedding) {
		t.Errorf("role after the verdict = %q, want embedding", after.Role)
	}
}

// failingMappingCapabilityStore fails the single-mapping capability read the
// spec endpoints use (failingCapabilityStore fails the bulk one).
type failingMappingCapabilityStore struct {
	*routing.MemoryStore
	err error
}

func (f *failingMappingCapabilityStore) MappingCapabilities(context.Context, string) ([]routing.CapabilityRow, error) {
	return nil, f.err
}

// A failed capability read propagates, like every other store read behind these
// endpoints: a role guessed from the type alone would put an embedding model in
// the chat group for one render, and the editor's group bulk-set would then write
// verdicts for the wrong set of models.
func TestGetRuntimeSpecPropagatesACapabilityReadFailure(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mappingID := seedRoleSpecMapping(t, svc, routeStore, app.ID, "m", "/opt/vllm/bin/vllm", "vllm", now)

	boom := errors.New("capability store down")
	failing := NewService(ServiceDeps{
		Users:  svc.users,
		Groups: svc.groups,
		Routes: &failingMappingCapabilityStore{MemoryStore: routeStore, err: boom},
		Clock:  func() time.Time { return now },
	})
	dto, err := failing.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err == nil {
		t.Fatalf("GetRuntimeSpec returned dto = %#v with a nil error; a capability read failure must be propagated", dto)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}
