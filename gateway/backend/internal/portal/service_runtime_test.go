// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedServerAgentApplication inserts a server_agent-typed application
// directly at the store layer for serverID, bypassing CreateApplication's
// validation/timeout-default/policy-reconciliation path entirely. Runtime-spec
// tests use this to get a minimal, fully-controlled fixture application
// without depending on (or re-asserting) CreateApplication's own behavior --
// mirroring seedServerTestGroups' direct-store seeding of admin groups for
// the same reason. (Historically this was also the ONLY way to get a
// server_agent application at all, since CreateApplication.
// normalizeApplicationType did not accept "server_agent" as a creatable
// type before Task 10; that gate is gone now, but the direct-store seed
// remains the right tool for these tests' purposes.)
func seedServerAgentApplication(t *testing.T, routeStore *routing.MemoryStore, serverID string, now time.Time) routing.Application {
	t.Helper()
	app := routing.Application{
		ID:         "app_" + compactRandomHex(16),
		ServerID:   serverID,
		Type:       routing.ProviderServerAgent,
		Port:       9000,
		Scheme:     "http",
		APIFlavors: []string{routing.APIFlavorOpenAI},
		Status:     routing.ServerStatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := routeStore.CreateApplication(context.Background(), app); err != nil {
		t.Fatalf("seed server_agent application: %v", err)
	}
	return app
}

func TestPutRuntimeSpecRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/local/bin/llama-server",
		Args:    []string{"--model", "/models/q.gguf"},
		Env:     map[string]string{"CUDA_VISIBLE_DEVICES": "${AGENT_ENV:GPU_IDS}"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.Configured || dto.ID == "" {
		t.Fatalf("dto = %#v, want Configured with a non-empty ID", dto)
	}
	if dto.MappingID != mapping.ID {
		t.Fatalf("mapping id = %q, want %q", dto.MappingID, mapping.ID)
	}
	// Defaults applied on empty/zero fields.
	if dto.HealthPath != "/health" {
		t.Fatalf("health path default = %q, want /health", dto.HealthPath)
	}
	if dto.HealthTimeoutSeconds != 5 {
		t.Fatalf("health timeout default = %d, want 5", dto.HealthTimeoutSeconds)
	}
	if dto.StartupTimeoutSeconds != 180 {
		t.Fatalf("startup timeout default = %d, want 180", dto.StartupTimeoutSeconds)
	}
	// Args/env round-trip.
	if len(dto.Args) != 2 || dto.Args[0] != "--model" || dto.Args[1] != "/models/q.gguf" {
		t.Fatalf("args = %#v", dto.Args)
	}
	if dto.Env["CUDA_VISIBLE_DEVICES"] != "${AGENT_ENV:GPU_IDS}" {
		t.Fatalf("env = %#v, want the placeholder preserved verbatim", dto.Env)
	}
	if len(dto.GPUs) != 1 || dto.GPUs[0].Index != 0 || dto.GPUs[0].VRAMEstimateMB != 8000 || dto.GPUs[0].VRAMMeasuredMB != 0 {
		t.Fatalf("gpus = %#v", dto.GPUs)
	}

	// Read back via GetRuntimeSpec.
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.ID != dto.ID || got.Binary != dto.Binary || len(got.GPUs) != 1 {
		t.Fatalf("GetRuntimeSpec round trip = %#v, want %#v", got, dto)
	}

	// A second PUT (upsert) preserves id/created_at and never lets the
	// client clobber vram_measured_mb -- simulate a prior agent measurement,
	// then PUT again with a different estimate AND an explicit (bogus)
	// VRAMMeasuredMB on the wire, and confirm the measured value survives
	// untouched. The request deliberately sends a non-zero VRAMMeasuredMB
	// here (rather than leaving it at zero) so this assertion cannot be
	// satisfied by accident just because Go's zero value happens to look
	// like "field omitted" -- it pins that the field is read from the
	// stored row, never from the request, at all.
	if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, dto.ID, 0, 7500); err != nil {
		t.Fatalf("seed measured value: %v", err)
	}
	dto2, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/local/bin/llama-server",
		Args:    []string{"--model", "/models/q.gguf"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 9000, VRAMMeasuredMB: 99999}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (second): %v", err)
	}
	if dto2.ID != dto.ID {
		t.Fatalf("id changed on upsert: got %q, want %q", dto2.ID, dto.ID)
	}
	if len(dto2.GPUs) != 1 || dto2.GPUs[0].VRAMEstimateMB != 9000 {
		t.Fatalf("estimate not updated: gpus = %#v", dto2.GPUs)
	}
	if dto2.GPUs[0].VRAMMeasuredMB != 7500 {
		t.Fatalf("measured value clobbered: got %d, want preserved 7500 (request sent bogus 99999)", dto2.GPUs[0].VRAMMeasuredMB)
	}
}

// TestPutRuntimeSpecPersistsRAMEstimate pins the operator half of the per-spec
// RAM pair: a PUT's ram_estimate_mb reaches the stored spec and the returned
// DTO, and -- the PUT being a full-document upsert -- a later PUT that omits it
// stores 0 ("no estimate", the unknown sentinel), not the previous value.
func TestPutRuntimeSpecPersistsRAMEstimate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", RAMEstimateMB: 4096,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.RAMEstimateMB != 4096 || dto.RAMMeasuredMB != 0 {
		t.Fatalf("dto ram = estimate %d measured %d, want 4096 and 0 (nothing measured yet)", dto.RAMEstimateMB, dto.RAMMeasuredMB)
	}
	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping: ok=%v err=%v", ok, err)
	}
	if stored.RAMEstimateMB != 4096 {
		t.Fatalf("stored RAMEstimateMB = %d, want 4096", stored.RAMEstimateMB)
	}

	// The agent measures; the operator then re-saves with a new estimate. The
	// estimate moves, the agent-owned measurement survives.
	if err := routeStore.UpdateRuntimeSpecRAMMeasured(ctx, dto.ID, 5000); err != nil {
		t.Fatalf("seed measured ram: %v", err)
	}
	dto2, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", RAMEstimateMB: 8192,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (second): %v", err)
	}
	if dto2.ID != dto.ID || dto2.RAMEstimateMB != 8192 || dto2.RAMMeasuredMB != 5000 {
		t.Fatalf("second PUT = id %q estimate %d measured %d, want the same id, 8192 and the preserved 5000", dto2.ID, dto2.RAMEstimateMB, dto2.RAMMeasuredMB)
	}

	// A full-document PUT that omits the estimate clears it; the measurement
	// still survives.
	dto3, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (third): %v", err)
	}
	if dto3.RAMEstimateMB != 0 || dto3.RAMMeasuredMB != 5000 {
		t.Fatalf("third PUT = estimate %d measured %d, want 0 (cleared) and the preserved 5000", dto3.RAMEstimateMB, dto3.RAMMeasuredMB)
	}
}

// TestGetRuntimeSpecReturnsRAMFields pins the read side: GET carries the
// operator's estimate and the agent's measurement, and an unconfigured mapping
// reports both as 0 (unknown) rather than omitting the keys.
func TestGetRuntimeSpecReturnsRAMFields(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	empty, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec (unconfigured): %v", err)
	}
	if empty.Configured || empty.RAMEstimateMB != 0 || empty.RAMMeasuredMB != 0 {
		t.Fatalf("unconfigured dto = %#v, want Configured:false with both RAM fields 0", empty)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal unconfigured dto: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal unconfigured dto: %v", err)
	}
	for _, key := range []string{"ram_estimate_mb", "ram_measured_mb"} {
		if string(keys[key]) != "0" {
			t.Fatalf("unconfigured dto wire %s = %q, want the explicit 0 (the keys are never omitted)", key, keys[key])
		}
	}

	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", RAMEstimateMB: 4096,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if err := routeStore.UpdateRuntimeSpecRAMMeasured(ctx, put.ID, 5000); err != nil {
		t.Fatalf("seed measured ram: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.RAMEstimateMB != 4096 || got.RAMMeasuredMB != 5000 {
		t.Fatalf("GetRuntimeSpec ram = estimate %d measured %d, want 4096 and 5000", got.RAMEstimateMB, got.RAMMeasuredMB)
	}
	raw, err = json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal dto: %v", err)
	}
	if string(keys["ram_estimate_mb"]) != "4096" || string(keys["ram_measured_mb"]) != "5000" {
		t.Fatalf("dto wire ram_estimate_mb = %q ram_measured_mb = %q, want 4096 and 5000", keys["ram_estimate_mb"], keys["ram_measured_mb"])
	}
}

func TestPutRuntimeSpecValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	agentApp := seedServerAgentApplication(t, routeStore, server.ID, now)
	agentMapping, err := svc.CreateMapping(ctx, ownerToken(), agentApp.ID, CreateMappingRequest{GatewayModelName: "m1", AppModelName: "m1"})
	if err != nil {
		t.Fatalf("CreateMapping (agent): %v", err)
	}
	vllmApp, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("CreateApplication (vllm): %v", err)
	}
	vllmMapping, err := svc.CreateMapping(ctx, ownerToken(), vllmApp.ID, CreateMappingRequest{GatewayModelName: "m2", AppModelName: "m2"})
	if err != nil {
		t.Fatalf("CreateMapping (vllm): %v", err)
	}

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{Binary: "/usr/bin/thing"}
	}

	cases := []struct {
		name      string
		mappingID string
		mutate    func(PutRuntimeSpecRequest) PutRuntimeSpecRequest
		wantErr   error
	}{
		{
			name:      "empty binary",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.Binary = ""; return r },
			wantErr:   ErrRuntimeSpecBinaryRequired,
		},
		{
			name:      "relative binary",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.Binary = "bin/thing"; return r },
			wantErr:   ErrRuntimeSpecBinaryRequired,
		},
		{
			name:      "negative idle timeout",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.IdleTimeoutSeconds = -1; return r },
			wantErr:   ErrRuntimeSpecTuningInvalid,
		},
		{
			name:      "negative ram estimate",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.RAMEstimateMB = -1; return r },
			wantErr:   ErrRuntimeSpecTuningInvalid,
		},
		{
			name:      "bad admin state",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.AdminState = "bogus"; return r },
			wantErr:   ErrRuntimeSpecAdminStateInvalid,
		},
		{
			name:      "duplicate gpu index",
			mappingID: agentMapping.ID,
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.GPUs = []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 100}, {Index: 0, VRAMEstimateMB: 200}}
				return r
			},
			wantErr: ErrRuntimeSpecGPUInvalid,
		},
		{
			name:      "spec on non-server_agent app",
			mappingID: vllmMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { return r },
			wantErr:   ErrRuntimeSpecNotServerAgent,
		},
		{
			name:      "bad responses_mode",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.ResponsesMode = "bogus"; return r },
			wantErr:   ErrRuntimeSpecEndpointModeInvalid,
		},
		{
			name:      "bad api_flavor",
			mappingID: agentMapping.ID,
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APIFlavors = []string{"openai", "bogus"}
				return r
			},
			wantErr: ErrRuntimeSpecFlavorInvalid,
		},
		{
			// SSRF: "@evil:9999/x" appended to "http://127.0.0.1:PORT" makes
			// url.Parse resolve Host to the attacker (the "@" is the userinfo
			// boundary), so the agent would dial off-loopback. Must be rejected.
			name:      "context_probe_path with userinfo authority",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.ContextProbePath = "@evil:9999/x"; return r },
			wantErr:   ErrRuntimeSpecContextProbePathInvalid,
		},
		{
			name:      "metrics_path protocol-relative",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.MetricsPath = "//evil"; return r },
			wantErr:   ErrRuntimeSpecMetricsPathInvalid,
		},
		{
			name:      "metrics_path absolute url",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.MetricsPath = "http://evil"; return r },
			wantErr:   ErrRuntimeSpecMetricsPathInvalid,
		},
		{
			// A safe relative override passes: proves the guard does not reject
			// legitimate per-type paths.
			name:      "valid relative probe paths",
			mappingID: agentMapping.ID,
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.MetricsPath = "/metrics"
				r.ContextProbePath = "/v1/models"
				return r
			},
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), tc.mappingID, tc.mutate(base()))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRuntimeSpecDTOCarriesFlavorsAndModes pins that RuntimeSpecDTO/
// PutRuntimeSpecRequest carry api_flavors + responses_mode/messages_mode,
// and that both PutRuntimeSpec and GetRuntimeSpec round-trip them.
func TestRuntimeSpecDTOCarriesFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:       true,
		Binary:        "/usr/local/bin/llama-server",
		APIFlavors:    []string{routing.APIFlavorOpenAI},
		ResponsesMode: string(routing.EndpointModeTranslate),
		MessagesMode:  string(routing.EndpointModeDisabled),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if len(dto.APIFlavors) != 1 || dto.APIFlavors[0] != routing.APIFlavorOpenAI {
		t.Fatalf("api_flavors = %#v", dto.APIFlavors)
	}
	if dto.ResponsesMode != string(routing.EndpointModeTranslate) || dto.MessagesMode != string(routing.EndpointModeDisabled) {
		t.Fatalf("modes = %q/%q", dto.ResponsesMode, dto.MessagesMode)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if len(got.APIFlavors) != 1 || got.ResponsesMode != string(routing.EndpointModeTranslate) {
		t.Fatalf("read-back = %#v", got)
	}
}

// TestPutRuntimeSpecModeAndFlavorDefaults pins the backend defaults applied
// when a spec's first PUT omits api_flavors/responses_mode/messages_mode:
// flavors default to both, and each mode defaults to passthrough (mirrors the
// application create-path default, Task A2). The rewrite case is
// TestPutRuntimeSpecModeAndFlavorDefaultsOnARewrite.
func TestPutRuntimeSpecModeAndFlavorDefaults(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		// APIFlavors / ResponsesMode / MessagesMode absent
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if want := []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}; !slices.Equal(dto.APIFlavors, want) {
		t.Fatalf("api_flavors default = %#v, want %#v", dto.APIFlavors, want)
	}
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) ||
		dto.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("mode defaults = %q/%q, want passthrough", dto.ResponsesMode, dto.MessagesMode)
	}
}

// TestPutRuntimeSpecModeAndFlavorDefaultsOnARewrite pins that the defaults
// apply on every write, not only on a spec's first one: a re-PUT of a spec
// stored with non-default flavors and modes, with the trio absent or empty,
// lands on both text flavors with passthrough rather than keeping the stored
// values (the keep-if-absent rule responses_live_timings_enabled follows does
// not extend to them). The empty case is the shape an override or benchmark
// replay of a stored [] and "" sends.
func TestPutRuntimeSpecModeAndFlavorDefaultsOnARewrite(t *testing.T) {
	cases := []struct {
		name    string
		rewrite PutRuntimeSpecRequest
	}{
		{
			name:    "absent",
			rewrite: PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"},
		},
		{
			name: "empty",
			rewrite: PutRuntimeSpecRequest{
				Enabled: true, Binary: "/usr/local/bin/llama-server",
				APIFlavors: []string{}, ResponsesMode: "", MessagesMode: "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
			ctx := context.Background()
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			app := seedServerAgentApplication(t, routeStore, server.ID, now)
			mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
			if err != nil {
				t.Fatalf("CreateMapping: %v", err)
			}
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
				Enabled: true, Binary: "/usr/local/bin/llama-server",
				APIFlavors:    []string{routing.APIFlavorOpenAI},
				ResponsesMode: string(routing.EndpointModeTranslate),
				MessagesMode:  string(routing.EndpointModeDisabled),
			}); err != nil {
				t.Fatalf("first PutRuntimeSpec: %v", err)
			}
			dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, tc.rewrite)
			if err != nil {
				t.Fatalf("rewrite PutRuntimeSpec: %v", err)
			}
			got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
			if err != nil {
				t.Fatalf("GetRuntimeSpec: %v", err)
			}
			want := []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}
			for label, d := range map[string]RuntimeSpecDTO{"PUT answer": dto, "GET": got} {
				if !slices.Equal(d.APIFlavors, want) {
					t.Fatalf("%s api_flavors = %#v, want %#v (default on a rewrite)", label, d.APIFlavors, want)
				}
				if d.ResponsesMode != string(routing.EndpointModePassthrough) ||
					d.MessagesMode != string(routing.EndpointModePassthrough) {
					t.Fatalf("%s modes = %q/%q, want passthrough (default on a rewrite)", label, d.ResponsesMode, d.MessagesMode)
				}
			}
		})
	}
}

// TestPutRuntimeSpecDoesNotInheritAppModes pins the "no backend inheritance"
// contract (spec §5.4/§12): the runtime-spec PUT is a full-document upsert
// with no field inheritance from the parent server_agent application -- the
// portal form starts a spec's first write from the parent application
// (runtimeSpecTemplate: Create, and Edit of a spec-less mapping), but the
// backend only ever supplies its own passthrough/both defaults for an
// absent or empty field, on every write. The test forces the seeded app's
// APIFlavors to [openai] only (no anthropic) and its ResponsesMode/
// MessagesMode to disabled before the PUT, so an inherited value could
// never pass for the backend's own default; a PUT that omits the trio must
// land on the backend's OWN defaults, not the parent app's stored values.
func TestPutRuntimeSpecDoesNotInheritAppModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	// Seed a server_agent app whose flavors are [openai] only and whose modes
	// are both disabled, so an inherited value could not pass for the default.
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	app.APIFlavors = []string{routing.APIFlavorOpenAI}
	app.ResponsesMode = routing.EndpointModeDisabled
	app.MessagesMode = routing.EndpointModeDisabled
	if err := routeStore.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// Backend default is passthrough for BOTH modes, NOT the app's disabled.
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("responses_mode = %q, want passthrough (no backend inheritance)", dto.ResponsesMode)
	}
	if dto.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("messages_mode = %q, want passthrough (no backend inheritance)", dto.MessagesMode)
	}
	// Backend default flavors are exactly both text flavors, NOT the app's
	// [openai]-only value, and never openai_images.
	if want := []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}; !slices.Equal(dto.APIFlavors, want) {
		t.Fatalf("api_flavors = %#v, want %#v (no backend inheritance)", dto.APIFlavors, want)
	}
}

// TestAgentRuntimeConfigOmitsFlavorsAndModes GUARDS spec §9: the agent-wire
// AgentRuntimeConfig/AgentRuntimeSpecDTO must never gain api_flavors/
// responses_mode/messages_mode -- those three fields are gateway-side only,
// decided before dispatch to the agent, and never enter the runtime.Spec the
// agent's local router consumes. Marshals the JSON for a spec carrying
// non-default flavors/modes and asserts none of the three keys leak through.
func TestAgentRuntimeConfigOmitsFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		APIFlavors: []string{routing.APIFlavorOpenAI}, ResponsesMode: string(routing.EndpointModeDisabled),
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"api_flavors", "responses_mode", "messages_mode"} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("agent runtime-config leaked %q: %s", k, raw)
		}
	}
}

// TestDeleteRuntimeSpecAllowedAfterApplicationRetyped pins a fix (review
// finding on the first cut of this file): a runtime spec must remain
// deletable even after its owning application is retyped away from
// server_agent through the ordinary UpdateApplication path, which has no
// check against the application's CURRENT type. Before the fix,
// DeleteRuntimeSpec mirrored PutRuntimeSpec's server_agent gate, which made
// such a spec permanently stuck: PutRuntimeSpec would refuse to touch it
// (gated) and DeleteRuntimeSpec would too, with DeleteApplication itself
// never cascade-cleaning specs -- the only remaining escape was
// DeleteMapping, far more destructive than clearing one stray spec. A
// cleanup operation must never be blockable by the same gate that prevents
// creating a NEW dependency on server_agent semantics.
func TestDeleteRuntimeSpecAllowedAfterApplicationRetyped(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}

	retype := routing.ProviderVLLM
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Type: &retype}); err != nil {
		t.Fatalf("UpdateApplication (retype to vllm): %v", err)
	}

	// GET stays permissive regardless of the (now non-server_agent) type.
	if dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil || !dto.Configured {
		t.Fatalf("GetRuntimeSpec after retype = %#v, %v, want Configured:true, nil err", dto, err)
	}

	// PutRuntimeSpec stays gated -- creating/replacing still needs server_agent.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/y"}); !errors.Is(err, ErrRuntimeSpecNotServerAgent) {
		t.Fatalf("PutRuntimeSpec after retype = %v, want ErrRuntimeSpecNotServerAgent", err)
	}

	// DeleteRuntimeSpec must still succeed: this is the fix under test.
	if err := svc.DeleteRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil {
		t.Fatalf("DeleteRuntimeSpec after retype = %v, want success", err)
	}
	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec after delete: %v", err)
	}
	if dto.Configured {
		t.Fatalf("Configured = true after delete, want false")
	}
}

// TestRuntimeSpecAuthorization proves the runtime-spec endpoints share
// authorizeMapping's exact chain: system scope / owner / admin-group
// delegate (co-manager with CanManageServers) succeed, and everyone else --
// including a plain "admin"-scope principal with NO ownership or delegation
// on this particular server -- collapses to ErrMappingNotFound (404-no-leak,
// never a distinguishable 403).
func TestRuntimeSpecAuthorization(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore, dir := newServerTestServiceWithDir(t, now)

	if err := dir.CreateUser(ctx, store.User{ID: "usr_manager", Email: "usr_manager@example.test", DisplayName: "Manager", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_manager: %v", err)
	}
	// A FRESH admin group, deliberately distinct from testAdminGroupID (which
	// usr_admin/adminToken() owns): usr_admin must have NO relationship to
	// this one, so adminToken() proves it needs actual delegation, not just
	// the "admin" scope. usr_manager is granted full co-manager rights
	// (SetUserGroupManager's default, including CanManageServers) on it.
	const delegateGroupID = "ugrp_runtime_delegate"
	if err := dir.CreateUserGroup(ctx, store.UserGroup{
		ID: delegateGroupID, Tier: store.GroupTierAdmin, Name: "Runtime Delegate",
		ParentGroupID: testSystemGroupID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create delegate group: %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, delegateGroupID, "usr_manager", store.GroupStateMember, ""); err != nil {
		t.Fatalf("add usr_manager to delegate group: %v", err)
	}
	if err := dir.SetUserGroupManager(ctx, delegateGroupID, "usr_manager"); err != nil {
		t.Fatalf("promote usr_manager: %v", err)
	}

	// systemToken() bypasses the admin-group-linkage "must already manage
	// this group" check (validateAdminGroupScope), so it can create a server
	// linked to delegateGroupID even though usr_system manages nothing.
	server, err := svc.CreateServer(ctx, systemToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test",
		OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{delegateGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// otherToken(): no ownership, no group relationship at all -> not-found
	// on every method, before any spec exists.
	if _, err := svc.GetRuntimeSpec(ctx, otherToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken GET = %v, want ErrMappingNotFound", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, otherToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken PUT = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteRuntimeSpec(ctx, otherToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken DELETE = %v, want ErrMappingNotFound", err)
	}

	// adminToken() alone: "admin" scope only, no "system" scope, and no
	// ownership/delegation on THIS server (unlike testAdminGroupID) -> also
	// not-found on every method.
	if _, err := svc.GetRuntimeSpec(ctx, adminToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken GET = %v, want ErrMappingNotFound", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, adminToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken PUT = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteRuntimeSpec(ctx, adminToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken DELETE = %v, want ErrMappingNotFound", err)
	}

	// ownerToken() (usr_owner is in OwnerIDs) succeeds -- creates the spec.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("ownerToken PUT: %v", err)
	}

	// The admin-group delegate (usr_manager, co-manager with CanManageServers
	// on delegateGroupID) also succeeds, on both a read and a write.
	managerToken := auth.Token{UserID: "usr_manager", Scopes: []string{"gateway:use"}}
	spec, err := svc.GetRuntimeSpec(ctx, managerToken, mapping.ID)
	if err != nil {
		t.Fatalf("manager GET: %v", err)
	}
	if !spec.Configured {
		t.Fatalf("manager GET spec.Configured = false, want true (owner already created it)")
	}
	if err := svc.DeleteRuntimeSpec(ctx, managerToken, mapping.ID); err != nil {
		t.Fatalf("manager DELETE: %v", err)
	}
}

func TestGetRuntimeSpecUnconfigured(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if dto.Configured {
		t.Fatalf("Configured = true, want false (no spec created yet)")
	}
	if dto.MappingID != mapping.ID {
		t.Fatalf("mapping id = %q, want %q", dto.MappingID, mapping.ID)
	}
	if dto.GPUs == nil || len(dto.GPUs) != 0 {
		t.Fatalf("gpus = %#v, want non-nil empty", dto.GPUs)
	}
	if dto.Args == nil || len(dto.Args) != 0 {
		t.Fatalf("args = %#v, want non-nil empty", dto.Args)
	}
	if dto.Env == nil || len(dto.Env) != 0 {
		t.Fatalf("env = %#v, want non-nil empty", dto.Env)
	}
}

func TestPutRuntimeSpecFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})

	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	// Store-seeded: going through svc.CreateMapping would fire the hook too
	// (see notifyRuntimeChangedForMapping), and this test is about the SPEC
	// write paths alone.
	mapping := seedMapping(t, routeStore, app.ID, "m", now)

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	mu.Lock()
	gotAfterPut := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotAfterPut) != 1 || gotAfterPut[0] != server.ID {
		t.Fatalf("calls after PUT = %#v, want exactly [%q]", gotAfterPut, server.ID)
	}

	if err := svc.DeleteRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil {
		t.Fatalf("DeleteRuntimeSpec: %v", err)
	}
	mu.Lock()
	gotAfterDelete := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotAfterDelete) != 2 || gotAfterDelete[1] != server.ID {
		t.Fatalf("calls after DELETE = %#v, want exactly [%q, %q]", gotAfterDelete, server.ID, server.ID)
	}
}

// --- Task 6: co-residency matrix -------------------------------------------

// seedMapping writes one ACTIVE mapping straight to the store, exactly as
// CreateMapping would for a request carrying no metrics (no MetricsSource
// stamp, status defaulted to active, IsMTP derived from the name). Used
// wherever a test needs a mapping to EXIST rather than to exercise the
// create path -- notably in the runtime-config-changed hook tests, where
// going through svc.CreateMapping would itself fire the hook (a mapping
// under the server_agent application is a runtime-config input; see
// notifyRuntimeChangedForMapping) and blur what the assertion is about.
// Mirrors seedServerAgentApplication, which exists for the same reason.
func seedMapping(t *testing.T, routeStore *routing.MemoryStore, appID, name string, now time.Time) ModelMappingDTO {
	t.Helper()
	mapping := routing.ModelMapping{
		ID:               "map_" + compactRandomHex(16),
		ApplicationID:    appID,
		GatewayModelName: name,
		AppModelName:     name,
		Status:           routing.ServerStatusActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := routeStore.CreateMapping(context.Background(), mapping); err != nil {
		t.Fatalf("seed mapping %q: %v", name, err)
	}
	// No capability rows: these fixtures are co-residency/runtime-spec
	// subjects, and their names are not MTP-suggesting, so the two
	// capability-derived DTO fields are false either way. (A mapping has no
	// capability FIELD to seed instead -- migration 79 dropped the columns.)
	return mappingDTO(mapping, nil, mappingFlavorMarkers{})
}

// seedTwoMappings creates a server_agent application on server and two
// mappings on it, for co-residency tests that need a pair of mapping ids
// belonging to the SAME application.
func seedTwoMappings(t *testing.T, routeStore *routing.MemoryStore, serverID string, now time.Time) (routing.Application, ModelMappingDTO, ModelMappingDTO) {
	t.Helper()
	app := seedServerAgentApplication(t, routeStore, serverID, now)
	return app, seedMapping(t, routeStore, app.ID, "m1", now), seedMapping(t, routeStore, app.ID, "m2", now)
}

func TestSetCoResidencyCanonicalizesPair(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	// Submit the pair with the higher id first -- SetCoResidency must sort it
	// server-side so the client never has to care about ordering.
	a, b := m1.ID, m2.ID
	if a > b {
		a, b = b, a
	}
	dto, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{Pairs: []CoResidencyPairDTO{{A: b, B: a}}})
	if err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	if len(dto.Pairs) != 1 || dto.Pairs[0].A != a || dto.Pairs[0].B != b {
		t.Fatalf("pairs = %#v, want canonical [[%q,%q]]", dto.Pairs, a, b)
	}

	got, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if len(got.Pairs) != 1 || got.Pairs[0].A != a || got.Pairs[0].B != b {
		t.Fatalf("GetCoResidency pairs = %#v, want canonical [[%q,%q]]", got.Pairs, a, b)
	}
}

func TestGetCoResidencyEmptyIsNonNil(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	got, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if got.Pairs == nil || len(got.Pairs) != 0 {
		t.Fatalf("pairs = %#v, want non-nil empty", got.Pairs)
	}
}

// TestSetCoResidencyRejectsDuplicateAfterNormalization pins the exact case
// called out by the task brief: submitting the same unordered pair twice,
// once in each order, must be rejected as a duplicate AFTER normalization --
// not silently accepted as "two different pairs".
func TestSetCoResidencyRejectsDuplicateAfterNormalization(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: []CoResidencyPairDTO{{A: m1.ID, B: m2.ID}, {A: m2.ID, B: m1.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}

	// The EXACT duplicate (same pair, same order) must be rejected by the same
	// validation. Asserted explicitly rather than assumed from the reversed
	// case above: the store layer now also rejects it (the composite primary
	// key on the SQL side, a matching guard in MemoryStore), and this is the
	// portal-level half of that pair of guards -- the one an operator
	// actually sees, with the honest 400 code instead of a store conflict.
	_, err = svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: []CoResidencyPairDTO{{A: m1.ID, B: m2.ID}, {A: m1.ID, B: m2.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("exact duplicate pair: err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

func TestSetCoResidencyRejectsSameMappingTwice(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, _ := seedTwoMappings(t, routeStore, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: []CoResidencyPairDTO{{A: m1.ID, B: m1.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

// TestSetCoResidencyRejectsForeignMapping pins that a pair naming a mapping
// belonging to a DIFFERENT application is invalid -- verified against the
// application's own mappings, not merely a global existence check.
func TestSetCoResidencyRejectsForeignMapping(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, _ := seedTwoMappings(t, routeStore, server.ID, now)

	// A separate server (seedServerAgentApplication hardcodes port 9000, so a
	// second application on the SAME server would collide on port instead)
	// with its own server_agent application and mapping -- "foreign" only in
	// the sense that matters here: a mapping id that exists but belongs to a
	// DIFFERENT application than app.
	otherServer := createTestServer(t, svc, "S2", "s2.example.test")
	otherApp := seedServerAgentApplication(t, routeStore, otherServer.ID, now)
	foreign, err := svc.CreateMapping(ctx, ownerToken(), otherApp.ID, CreateMappingRequest{GatewayModelName: "f", AppModelName: "f"})
	if err != nil {
		t.Fatalf("CreateMapping foreign: %v", err)
	}

	_, err = svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: []CoResidencyPairDTO{{A: m1.ID, B: foreign.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

func TestSetCoResidencyFiresRuntimeChangedHookAndClearsOnEmpty(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{Pairs: []CoResidencyPairDTO{{A: m1.ID, B: m2.ID}}}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	// Clearing the set (empty Pairs) is a valid write too and must also fire
	// the hook -- an empty PUT is how an operator removes every rule.
	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{}); err != nil {
		t.Fatalf("SetCoResidency (clear): %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 2 || got[0] != server.ID || got[1] != server.ID {
		t.Fatalf("calls = %#v, want exactly [%q, %q]", got, server.ID, server.ID)
	}
	final, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if len(final.Pairs) != 0 {
		t.Fatalf("pairs after clear = %#v, want empty", final.Pairs)
	}
}

// --- Task 6: per-GPU VRAM budgets -------------------------------------------

func TestSetServerGPUBudgetsRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 1, BudgetMB: 4000}, {Index: 0, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	if len(dto) != 2 || dto[0].Index != 0 || dto[0].BudgetMB != 8000 || dto[1].Index != 1 || dto[1].BudgetMB != 4000 {
		t.Fatalf("dto = %#v, want ordered by index", dto)
	}
	// No telemetry sample exists -- expected_* stay empty rather than failing.
	if dto[0].ExpectedUUID != "" || dto[0].ExpectedName != "" {
		t.Fatalf("dto[0] = %#v, want empty expected_* (no telemetry sample)", dto[0])
	}

	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if len(got) != 2 || got[0].BudgetMB != 8000 || got[1].BudgetMB != 4000 {
		t.Fatalf("GetServerGPUBudgets round trip = %#v", got)
	}
}

func TestGetServerGPUBudgetsEmptyIsNonNil(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("budgets = %#v, want non-nil empty", got)
	}
}

func TestSetServerGPUBudgetsValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	cases := []struct {
		name    string
		budgets []GPUBudgetDTO
	}{
		{"negative index", []GPUBudgetDTO{{Index: -1, BudgetMB: 100}}},
		{"negative budget_mb", []GPUBudgetDTO{{Index: 0, BudgetMB: -1}}},
		{"duplicate index", []GPUBudgetDTO{{Index: 0, BudgetMB: 100}, {Index: 0, BudgetMB: 200}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{Budgets: tc.budgets})
			if !errors.Is(err, ErrGPUBudgetInvalid) {
				t.Fatalf("err = %v, want ErrGPUBudgetInvalid", err)
			}
		})
	}
}

// TestSetServerGPUBudgetsZeroIsAcceptedAndMeansUnconstrained pins the other
// half of the validation contract, which TestSetServerGPUBudgetsValidation
// only implies: `BudgetMB < 0` is refused, and `BudgetMB == 0` is therefore
// ACCEPTED on purpose. It is not an oversight to be tightened later --
// `routing.ServerGPUBudget.BudgetMB` defines 0 as "no budget for this GPU" =
// unconstrained, identical to a GPU with no row at all, and the agent's
// admission policy (runtime.Admit in server-agent/internal/runtime/policy.go)
// honours that by skipping any index whose budget is <= 0.
//
// So a row an operator cleared to 0 is a no-op, not an error and not a hard
// refusal of every model on that card. Making this method reject 0 instead
// would break the runtime screen, whose limits form seeds a brand-new budget
// row at 0 MB and turns a cleared MB input into 0, and it would leave the
// gateway and the agent disagreeing about a value the store already persists.
func TestSetServerGPUBudgetsZeroIsAcceptedAndMeansUnconstrained(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 0}, {Index: 1, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets with budget_mb 0 = %v, want nil: 0 is unconstrained, not invalid", err)
	}
	if len(dto) != 2 || dto[0].Index != 0 || dto[0].BudgetMB != 0 {
		t.Fatalf("dto = %#v, want the 0 budget stored verbatim at index 0", dto)
	}

	// It must also SURVIVE the round trip as 0 -- silently dropping the row
	// would reach the same admission outcome today by accident, while
	// destroying the operator's row and the expected_* drift snapshot on it.
	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if len(got) != 2 || got[0].Index != 0 || got[0].BudgetMB != 0 || got[1].BudgetMB != 8000 {
		t.Fatalf("round trip = %#v, want [{0 0} {1 8000}]", got)
	}
}

// TestSetServerGPUBudgetsSnapshotsExpectedAndNeverOverwrites pins the drift-
// detector contract: expected_uuid/expected_name are captured from the
// latest telemetry sample ONLY when the budget row is first created, and a
// later PUT -- even one whose request carries different (here: deliberately
// bogus) values, and even after a NEWER telemetry sample reports a
// different card at the same index -- must never overwrite them.
func TestSetServerGPUBudgetsSnapshotsExpectedAndNeverOverwrites(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if err := routeStore.InsertTelemetrySample(ctx, routing.TelemetrySample{
		ServerID: server.ID, ReportedAt: now,
		GPUs: []routing.GPUSample{{Index: 0, UUID: "GPU-111", Name: "RTX 4090"}},
	}); err != nil {
		t.Fatalf("seed telemetry sample: %v", err)
	}

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets (create): %v", err)
	}
	if len(dto) != 1 || dto[0].ExpectedUUID != "GPU-111" || dto[0].ExpectedName != "RTX 4090" {
		t.Fatalf("dto = %#v, want expected_uuid/expected_name snapshotted from telemetry", dto)
	}

	// A later telemetry sample reports a DIFFERENT card at the same index
	// (the renumbering/renaming scenario the drift detector exists for).
	if err := routeStore.InsertTelemetrySample(ctx, routing.TelemetrySample{
		ServerID: server.ID, ReportedAt: now.Add(time.Minute),
		GPUs: []routing.GPUSample{{Index: 0, UUID: "GPU-222", Name: "RTX 5090"}},
	}); err != nil {
		t.Fatalf("seed second telemetry sample: %v", err)
	}

	// The second PUT changes budget_mb and sends bogus expected_* values on
	// the wire (a client would normally just echo back the GET response, but
	// this pins that the server NEVER trusts the request for these fields,
	// on a create OR an update).
	dto2, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 9000, ExpectedUUID: "bogus-uuid", ExpectedName: "bogus-name"}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets (update): %v", err)
	}
	if len(dto2) != 1 || dto2[0].BudgetMB != 9000 {
		t.Fatalf("budget_mb not updated: dto2 = %#v", dto2)
	}
	if dto2[0].ExpectedUUID != "GPU-111" || dto2[0].ExpectedName != "RTX 4090" {
		t.Fatalf("expected_* clobbered: got %#v, want the ORIGINAL snapshot (GPU-111/RTX 4090) preserved", dto2[0])
	}
}

func TestSetServerGPUBudgetsFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 1000}}}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != server.ID {
		t.Fatalf("calls = %#v, want exactly [%q]", got, server.ID)
	}
}

// --- Task 6: server runtime flags -------------------------------------------

func TestServerRuntimeFlagsCreateAndUpdateRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)

	maxProc := 3
	managed := true
	dto, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs:       []string{testAdminGroupID},
		RuntimeMaxProcesses: &maxProc, ManagedRuntimeOnly: &managed,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.RuntimeMaxProcesses != 3 || !dto.ManagedRuntimeOnly {
		t.Fatalf("create dto = %#v, want RuntimeMaxProcesses=3, ManagedRuntimeOnly=true", dto)
	}

	newMax := 5
	unmanaged := false
	updated, err := svc.UpdateServer(ctx, ownerToken(), dto.ID, UpdateServerRequest{
		RuntimeMaxProcesses: &newMax, ManagedRuntimeOnly: &unmanaged,
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if updated.RuntimeMaxProcesses != 5 || updated.ManagedRuntimeOnly {
		t.Fatalf("update dto = %#v, want RuntimeMaxProcesses=5, ManagedRuntimeOnly=false", updated)
	}
}

func TestCreateServerRuntimeMaxProcessesNegativeInvalid(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	neg := -1
	_, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID}, RuntimeMaxProcesses: &neg,
	})
	if !errors.Is(err, ErrServerRuntimeLimitInvalid) {
		t.Fatalf("err = %v, want ErrServerRuntimeLimitInvalid", err)
	}
}

func TestUpdateServerRuntimeMaxProcessesNegativeInvalid(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	neg := -1
	_, err := svc.UpdateServer(ctx, ownerToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &neg})
	if !errors.Is(err, ErrServerRuntimeLimitInvalid) {
		t.Fatalf("err = %v, want ErrServerRuntimeLimitInvalid", err)
	}
}

// TestCreateApplicationManagedRuntimeOnlyGate pins the managed-runtime-only
// gate: a non-server_agent application create on a ManagedRuntimeOnly server
// is rejected with ErrServerManagedRuntimeOnly, while a "server_agent" create
// on that same server is exempt from the gate and now (Task 10 registered
// "server_agent" as a creatable application type) genuinely SUCCEEDS -- that
// is the whole point of the ManagedRuntimeOnly flag: such a server hosts
// agent-managed model processes and nothing else. (Before Task 10 this test
// could only assert that the server_agent create failed for the separate,
// pre-existing reason that CreateApplication did not yet accept
// "server_agent" as a creatable type at all; now that gate is gone, the test
// asserts the real, positive behavior instead.)
func TestCreateApplicationManagedRuntimeOnlyGate(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	managed := true
	server, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID}, ManagedRuntimeOnly: &managed,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	_, err = svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if !errors.Is(err, ErrServerManagedRuntimeOnly) {
		t.Fatalf("vllm create on managed-only server: err = %v, want ErrServerManagedRuntimeOnly", err)
	}

	dto, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http"})
	if err != nil {
		t.Fatalf("server_agent create on managed-only server: err = %v, want success", err)
	}
	if dto.Type != routing.ProviderServerAgent {
		t.Fatalf("dto.Type = %q, want server_agent", dto.Type)
	}

	// A normal (non-managed-only) server is unaffected.
	plain := createTestServer(t, svc, "S2", "s2.example.test")
	if _, err := svc.CreateApplication(ctx, ownerToken(), plain.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"}); err != nil {
		t.Fatalf("vllm create on a plain server: %v, want success", err)
	}
}

// --- Task 6: runtime timeout warning ----------------------------------------

func TestRuntimeWarningsTimeoutBelowStartup(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// No runtime spec yet -- non-nil empty, no warning.
	warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if warnings == nil || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want non-nil empty", warnings)
	}

	// An ENABLED spec with a 60s startup timeout (60000ms) exceeds the
	// application's stored TimeoutMS (seedServerAgentApplication leaves it at
	// the zero value) -- must warn. The spec names only the application's own
	// flavor, so api_flavors_not_on_application stays out of the exact list.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/bin/x", Enabled: true, StartupTimeoutSeconds: 60, APIFlavors: []string{routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	warnings, err = svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 1 || warnings[0] != "timeout_ms_below_startup_timeout" {
		t.Fatalf("warnings = %#v, want [timeout_ms_below_startup_timeout]", warnings)
	}

	// Raising the application's TimeoutMS to/above the 60000ms threshold
	// clears the warning.
	raised := 90000
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{TimeoutMS: &raised}); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	warnings, err = svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings after raising timeout_ms = %#v, want none", warnings)
	}
}

// TestRuntimeWarningsIgnoresDisabledSpecs pins the "ENABLED specs" wording:
// a disabled spec's startup timeout must never count toward the max.
func TestRuntimeWarningsIgnoresDisabledSpecs(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Enabled:false with a huge startup timeout -- must not trigger the
	// warning even though the application's TimeoutMS (zero value here) is
	// nowhere near it. The spec names only the application's own flavor: the
	// flavor warnings count disabled specs, and this test pins the timeout
	// warning alone.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/bin/x", Enabled: false, StartupTimeoutSeconds: 600, APIFlavors: []string{routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want none (spec is disabled)", warnings)
	}
}

// --- Task 7: agent runtime-config assembly ----------------------------------

// TestAgentRuntimeConfigAssembly pins the GET /api/agent/v1/runtime-config
// wire contract (spec §11): RouterListen is the server_agent application's
// Port, only ENABLED specs appear, coresidency pairs are translated from
// mapping ids to spec ids (dropping any pair touching a disabled/missing
// spec), per-GPU VRAMMB is max(measured, estimate), and a
// server with no server_agent application yet gets the fully empty document
// -- never an error.
func TestAgentRuntimeConfigAssembly(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Before any server_agent application exists, the agent must be able to
	// poll harmlessly: a fully empty, non-nil, stably-etagged document.
	empty, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (no app): %v", err)
	}
	if empty.RouterListen != 0 || empty.MaxProcesses != 0 {
		t.Fatalf("empty dto = %#v, want RouterListen=0, MaxProcesses=0", empty)
	}
	if empty.Specs == nil || len(empty.Specs) != 0 {
		t.Fatalf("empty.Specs = %#v, want non-nil empty", empty.Specs)
	}
	if empty.Coresident == nil || len(empty.Coresident) != 0 {
		t.Fatalf("empty.Coresident = %#v, want non-nil empty", empty.Coresident)
	}
	if empty.GPUBudgets == nil || len(empty.GPUBudgets) != 0 {
		t.Fatalf("empty.GPUBudgets = %#v, want non-nil empty", empty.GPUBudgets)
	}
	if empty.ETag == "" {
		t.Fatalf("empty dto must still carry a stable etag")
	}
	emptyAgain, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (no app, second call): %v", err)
	}
	if emptyAgain.ETag != empty.ETag {
		t.Fatalf("empty etag not stable: %q vs %q", empty.ETag, emptyAgain.ETag)
	}

	app := seedServerAgentApplication(t, routeStore, server.ID, now) // Port 9000
	maxProc := 3
	if _, err := svc.UpdateServer(ctx, ownerToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &maxProc}); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if _, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 1, BudgetMB: 21500}, {Index: 0, BudgetMB: 46000}},
	}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}

	mappingA, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen-coder", AppModelName: "qwen2.5-coder-32b"})
	if err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	specA, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingA.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/bin/vllm",
		Args:    []string{"--tensor-parallel-size", "2"},
		Env:     map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 22000}, {Index: 1, VRAMEstimateMB: 21500}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec A: %v", err)
	}
	// GPU 0 has a measured value on file -- must win over the estimate. GPU 1
	// has no measurement -- must fall back to the estimate.
	if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, specA.ID, 0, 22500); err != nil {
		t.Fatalf("seed measured value: %v", err)
	}

	mappingB, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "llama-small", AppModelName: "llama-3-8b"})
	if err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}
	specB, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingB.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/bin/llama-server",
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec B: %v", err)
	}

	mappingDisabled, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "disabled-model", AppModelName: "disabled-model"})
	if err != nil {
		t.Fatalf("CreateMapping disabled: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingDisabled.ID, PutRuntimeSpecRequest{
		Enabled: false,
		Binary:  "/usr/bin/disabled-server",
	}); err != nil {
		t.Fatalf("PutRuntimeSpec disabled: %v", err)
	}

	// Coresidency: A+B is a real allowed pair; A+disabled must be dropped
	// (its spec is disabled, hence absent from the wire document). opt_in so the
	// explicit allow rows are what put a pair on the wire: under the default
	// opt_out the A+B pair would be enumerated whether or not it had a row.
	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Mode:  string(routing.CoResidencyModeOptIn),
		Pairs: []CoResidencyPairDTO{{A: mappingA.ID, B: mappingB.ID, Verdict: routing.CoResidencyVerdictAllow}, {A: mappingA.ID, B: mappingDisabled.ID, Verdict: routing.CoResidencyVerdictAllow}},
	}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}

	dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if dto.RouterListen != app.Port {
		t.Fatalf("router_listen = %d, want the server_agent application's port %d", dto.RouterListen, app.Port)
	}
	if dto.MaxProcesses != 3 {
		t.Fatalf("max_processes = %d, want 3", dto.MaxProcesses)
	}
	if len(dto.GPUBudgets) != 2 || dto.GPUBudgets[0].Index != 0 || dto.GPUBudgets[0].BudgetMB != 46000 ||
		dto.GPUBudgets[1].Index != 1 || dto.GPUBudgets[1].BudgetMB != 21500 {
		t.Fatalf("gpu_budgets = %#v, want ordered by index [{0 46000} {1 21500}]", dto.GPUBudgets)
	}
	if len(dto.Specs) != 2 {
		t.Fatalf("specs = %#v, want exactly 2 (the disabled spec must be absent)", dto.Specs)
	}
	var gotA, gotB *AgentRuntimeSpecDTO
	for i := range dto.Specs {
		switch dto.Specs[i].ID {
		case specA.ID:
			gotA = &dto.Specs[i]
		case specB.ID:
			gotB = &dto.Specs[i]
		}
	}
	if gotA == nil || gotB == nil {
		t.Fatalf("specs = %#v, want both specA (%s) and specB (%s)", dto.Specs, specA.ID, specB.ID)
	}
	if gotA.Model != "qwen-coder" || gotA.UpstreamModel != "qwen2.5-coder-32b" {
		t.Fatalf("specA model/upstream_model = %q/%q, want qwen-coder/qwen2.5-coder-32b", gotA.Model, gotA.UpstreamModel)
	}
	if gotA.Binary != "/usr/bin/vllm" || len(gotA.Args) != 2 || gotA.Args[0] != "--tensor-parallel-size" {
		t.Fatalf("specA binary/args = %#v", gotA)
	}
	if gotA.Env["HF_TOKEN"] != "${AGENT_ENV:HF_TOKEN}" {
		t.Fatalf("specA env = %#v, want the placeholder preserved verbatim", gotA.Env)
	}
	if len(gotA.GPUs) != 2 || gotA.GPUs[0].Index != 0 || gotA.GPUs[0].VRAMMB != 22500 ||
		gotA.GPUs[1].Index != 1 || gotA.GPUs[1].VRAMMB != 21500 {
		t.Fatalf("specA gpus = %#v, want [{0 22500} {1 21500}] (measured overrides estimate on GPU 0, estimate used on GPU 1)", gotA.GPUs)
	}
	if gotA.HealthPath != "/health" || gotA.HealthTimeoutSeconds != 5 || gotA.StartupTimeoutSeconds != 180 {
		t.Fatalf("specA defaults = %#v", gotA)
	}
	if gotB.Model != "llama-small" || gotB.UpstreamModel != "llama-3-8b" {
		t.Fatalf("specB model/upstream_model = %q/%q", gotB.Model, gotB.UpstreamModel)
	}

	if len(dto.Coresident) != 1 {
		t.Fatalf("coresident = %#v, want exactly 1 pair (the disabled-spec pair must be dropped)", dto.Coresident)
	}
	pair := dto.Coresident[0]
	if !((pair[0] == specA.ID && pair[1] == specB.ID) || (pair[0] == specB.ID && pair[1] == specA.ID)) {
		t.Fatalf("coresident[0] = %#v, want {%s,%s} in either order", pair, specA.ID, specB.ID)
	}
	for _, p := range dto.Coresident {
		if p[0] == mappingDisabled.ID || p[1] == mappingDisabled.ID {
			t.Fatalf("coresident leaked a MAPPING id instead of a SPEC id: %#v", p)
		}
	}

	if dto.ETag == "" {
		t.Fatalf("dto.ETag must not be empty")
	}
	dtoAgain, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (second call): %v", err)
	}
	if dtoAgain.ETag != dto.ETag {
		t.Fatalf("etag not stable across identical calls: %q vs %q", dto.ETag, dtoAgain.ETag)
	}
	if dtoAgain.ETag == empty.ETag {
		t.Fatalf("a populated config must not share the empty config's etag")
	}
}

// TestAgentRuntimeConfigUnknownServerIsEmptyNotError mirrors AgentProxyRoutes'
// "a missing/unreadable server resolves to the safe empty default" posture: a
// serverID naming no AIServer at all must never surface as an error to the
// agent-token holder.
func TestAgentRuntimeConfigUnknownServerIsEmptyNotError(t *testing.T) {
	svc, _ := newServerTestService(t, time.Now())
	dto, err := svc.AgentRuntimeConfig(context.Background(), "no-such-server")
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if dto.Specs == nil || len(dto.Specs) != 0 || dto.RouterListen != 0 {
		t.Fatalf("dto = %#v, want the fully empty document", dto)
	}
}

// TestAgentRuntimeConfigEnvNullNormalizedToEmptyObject pins the nil-vs-empty
// wire-shape defect class (already caught twice on this branch, and fixed a
// third time in agentRuntimeSpecDTO/runtimeSpecDTO/the gateway handler's
// defensive loop as part of this test): a stored spec whose env column holds
// the literal JSON "null" -- something PutRuntimeSpec itself can never
// produce (it always marshals a non-nil map before storing), but a
// plausible outcome of a direct store write or a future writer that
// bypasses PutRuntimeSpec's normalization -- must still marshal as
// "env":{} on the wire, never "env":null. Asserted on the marshaled JSON
// BYTES, not the Go map, since the whole point is the wire shape a
// JSON-strict agent parser sees.
func TestAgentRuntimeConfigEnvNullNormalizedToEmptyObject(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Write directly at the store layer: PutRuntimeSpec always marshals a
	// non-nil map for Env, so the literal "null" string can only be produced
	// by bypassing it.
	spec := routing.RuntimeSpec{
		ID:        "rspec_" + compactRandomHex(16),
		MappingID: mapping.ID,
		Enabled:   true,
		Binary:    "/usr/bin/x",
		Args:      "[]",
		Env:       "null",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("seed spec with literal null env: %v", err)
	}

	dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), `"env":null`) {
		t.Fatalf("wire body = %s, want \"env\":{} not \"env\":null", raw)
	}
	if !strings.Contains(string(raw), `"env":{}`) {
		t.Fatalf("wire body = %s, want an explicit \"env\":{}", raw)
	}
}

// TestGetRuntimeSpecEnvNullNormalizedToEmptyObject covers the sibling
// portal-facing path (runtimeSpecDTO, Task 5) that has the identical
// nil-vs-empty gap the review flagged: GetRuntimeSpec must never marshal
// "env":null either.
func TestGetRuntimeSpecEnvNullNormalizedToEmptyObject(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	spec := routing.RuntimeSpec{
		ID:        "rspec_" + compactRandomHex(16),
		MappingID: mapping.ID,
		Enabled:   true,
		Binary:    "/usr/bin/x",
		Args:      "[]",
		Env:       "null",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("seed spec with literal null env: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), `"env":null`) {
		t.Fatalf("wire body = %s, want \"env\":{} not \"env\":null", raw)
	}
	if !strings.Contains(string(raw), `"env":{}`) {
		t.Fatalf("wire body = %s, want an explicit \"env\":{}", raw)
	}
}

// --- Task 9: ServerRuntimeReportView -----------------------------------------

// TestServerRuntimeReportViewAbsent proves the hardware-panel model: a
// server with no runtime report EVER stored returns Available:false (not an
// error), with a non-nil-but-empty AgentFeatures even though no telemetry
// has been reported either.
func TestServerRuntimeReportViewAbsent(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if dto.Available {
		t.Fatalf("dto = %#v, want Available=false", dto)
	}
	if dto.CollectedAt != "" || dto.UpdatedAt != "" || len(dto.Report) != 0 {
		t.Fatalf("absent report must leave the report fields empty: %#v", dto)
	}
	if dto.AgentVersion != "" {
		t.Fatalf("agent_version = %q, want empty (no telemetry)", dto.AgentVersion)
	}
	if dto.AgentFeatures == nil || len(dto.AgentFeatures) != 0 {
		t.Fatalf("agent_features = %#v, want non-nil empty", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewFound proves a stored report is surfaced
// verbatim (this method never parses/rewrites Report -- that is the ingest
// layer's job) alongside agent_version/agent_features parsed from the
// server's latest telemetry row, independent of the report itself.
func TestServerRuntimeReportViewFound(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: server.ID, ReportedAt: now, AgentVersion: "2.0.1",
		Capabilities: `{"features":["runtime_manager"]}`, ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	reportJSON := `{"source":"file","collected_at":"2026-07-11T12:00:00Z","config":{"specs":[]}}`
	if err := routeStore.UpsertServerRuntimeReport(ctx, routing.ServerRuntimeReport{
		ServerID: server.ID, CollectedAt: now, ReportJSON: reportJSON, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertServerRuntimeReport: %v", err)
	}

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if !dto.Available || dto.CollectedAt == "" || dto.UpdatedAt == "" {
		t.Fatalf("dto = %#v, want Available with timestamps", dto)
	}
	if string(dto.Report) != reportJSON {
		t.Fatalf("report = %s, want the stored blob verbatim: %s", dto.Report, reportJSON)
	}
	if dto.AgentVersion != "2.0.1" {
		t.Fatalf("agent_version = %q, want 2.0.1", dto.AgentVersion)
	}
	if len(dto.AgentFeatures) != 1 || dto.AgentFeatures[0] != "runtime_manager" {
		t.Fatalf("agent_features = %#v", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewMalformedCapabilitiesYieldsEmptyFeatures proves
// a garbled/forward-incompatible telemetry capabilities blob never rejects
// the whole read -- it degrades to an empty (non-nil) feature list, the same
// tolerant-parse discipline the gateway ingest layer's parseAgentCapabilities
// uses.
func TestServerRuntimeReportViewMalformedCapabilitiesYieldsEmptyFeatures(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: server.ID, ReportedAt: now, AgentVersion: "1.0.0",
		Capabilities: `"not an object"`, ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if dto.AgentVersion != "1.0.0" {
		t.Fatalf("agent_version = %q, want 1.0.0 (unaffected by the malformed capabilities)", dto.AgentVersion)
	}
	if dto.AgentFeatures == nil || len(dto.AgentFeatures) != 0 {
		t.Fatalf("agent_features = %#v, want non-nil empty on malformed capabilities", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewForeignUserForbidden proves authorizeServer
// gates this read like every other server-scoped one: a non-owner gets
// ErrServerNotFound (404-no-leak upstream), never the report contents.
func TestServerRuntimeReportViewForeignUserForbidden(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpsertServerRuntimeReport(ctx, routing.ServerRuntimeReport{
		ServerID: server.ID, CollectedAt: now, ReportJSON: `{}`, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertServerRuntimeReport: %v", err)
	}

	if _, err := svc.ServerRuntimeReportView(ctx, otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

// failAIServerByIDStore injects an AIServerByID failure onto an otherwise-real
// routing.Store, mirroring service_agent_proxy_routes_test.go's
// failApplicationsByServerStore.
type failAIServerByIDStore struct {
	routing.Store
	err error
}

func (s *failAIServerByIDStore) AIServerByID(context.Context, string) (routing.AIServer, error) {
	return routing.AIServer{}, s.err
}

// TestAgentRuntimeConfigPropagatesTransientServerReadFailure is C1's covering
// test. A TRANSIENT store failure on the AIServer read -- a dropped Postgres
// connection, a SQLite contention window, or the context.DeadlineExceeded
// PushRuntimeConfig's own 5s bound produces -- must surface as an error, NOT
// as the empty document with err == nil.
//
// The empty-document-with-nil-error answer is not merely imprecise, it is the
// most destructive value this function can return: the agent parses it
// happily, overwrites its on-disk cache with it, tears down its bound router
// listener and drains every running spec. And because it comes back with
// err == nil, it also walks straight through gateway.PushRuntimeConfig's
// never-push-on-error guard, so the WS path pushes the teardown with no HTTP
// status involved at all. See AgentRuntimeConfig's own doc comment.
func TestAgentRuntimeConfigPropagatesTransientServerReadFailure(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	transient := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"connection failure", transient},
		// The PushRuntimeConfig trigger specifically: that method bounds this
		// call with pushRuntimeConfigTimeout, so a slow store surfaces here as
		// a deadline, not as a store-shaped error.
		{"context deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			seedServerAgentApplication(t, routeStore, server.ID, now)

			failing := NewService(ServiceDeps{
				Users:  svc.users,
				Groups: svc.groups,
				Routes: &failAIServerByIDStore{Store: routeStore, err: tc.err},
				Clock:  func() time.Time { return now },
			})
			dto, err := failing.AgentRuntimeConfig(context.Background(), server.ID)
			if err == nil {
				t.Fatalf("AgentRuntimeConfig returned dto = %#v with a nil error; a transient store failure must be propagated, never collapsed into the empty document", dto)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.err)
			}
		})
	}
}

// TestAgentRuntimeConfigPushesMaxOfMeasuredAndEstimate pins the precedence of
// the VRAM demand the gateway pushes to the agent: max(measured, estimate).
//
// The operator's estimate is a FLOOR and the agent's measurement raises it when
// reality is higher -- never the other way round. That is the "never
// under-reserve" rule, and it is load-bearing rather than cosmetic: this DTO is
// not just what the portal displays, it is the spec's own declared demand that
// the agent feeds into Admit, so a value that is too low here is a co-resident
// pair that reaches an OOM.
//
// It replaces the old "measured wins unless the spec's VRAM is locked" rule.
// Under that rule a measurement BELOW the estimate silently lowered the
// reservation, and the only way to hold it up was a separate per-spec lock
// flag; max() makes the floor unconditional, so that flag no longer exists.
//
// A 0 on either side is "unknown", never "needs none": measured 0 (never
// measured) yields the estimate, estimate 0 yields the measurement, and both 0
// stays 0 so the agent still sees an unknown demand and bootstraps it.
func TestAgentRuntimeConfigPushesMaxOfMeasuredAndEstimate(t *testing.T) {
	cases := []struct {
		name     string
		measured int
		estimate int
		want     int
	}{
		{name: "measured above estimate raises it", measured: 9000, estimate: 8000, want: 9000},
		{name: "never measured yields the estimate", measured: 0, estimate: 8000, want: 8000},
		{name: "measured below estimate is floored at the estimate", measured: 7000, estimate: 8000, want: 8000},
		{name: "no estimate yields the measurement", measured: 9000, estimate: 0, want: 9000},
		{name: "both unknown stays unknown", measured: 0, estimate: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
			ctx := context.Background()
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			app := seedServerAgentApplication(t, routeStore, server.ID, now)
			mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "llama", AppModelName: "llama-3-8b"})
			if err != nil {
				t.Fatalf("CreateMapping: %v", err)
			}
			spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
				Enabled: true,
				Binary:  "/usr/bin/llama-server",
				GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: tc.estimate}},
			})
			if err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			if tc.measured > 0 {
				// The agent measured its own child and wrote it back.
				if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, spec.ID, 0, tc.measured); err != nil {
					t.Fatalf("seed measured value: %v", err)
				}
			}

			dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
			if err != nil {
				t.Fatalf("AgentRuntimeConfig: %v", err)
			}
			if len(dto.Specs) != 1 || len(dto.Specs[0].GPUs) != 1 {
				t.Fatalf("dto.Specs = %#v, want one spec with one GPU", dto.Specs)
			}
			if got := dto.Specs[0].GPUs[0].VRAMMB; got != tc.want {
				t.Fatalf("pushed vram_mb = %d for measured=%d estimate=%d, want max(measured, estimate) = %d", got, tc.measured, tc.estimate, tc.want)
			}

			// Serving the larger of the two must not rewrite either stored
			// figure: the estimate stays the operator's and the measurement
			// stays the agent's.
			gpus, err := routeStore.RuntimeSpecGPUs(ctx, spec.ID)
			if err != nil || len(gpus) != 1 {
				t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
			}
			if gpus[0].VRAMMeasuredMB != tc.measured || gpus[0].VRAMEstimateMB != tc.estimate {
				t.Fatalf("stored (measured, estimate) = (%d, %d), want (%d, %d) untouched", gpus[0].VRAMMeasuredMB, gpus[0].VRAMEstimateMB, tc.measured, tc.estimate)
			}
		})
	}
}

// TestAgentConfigCarriesRAMDemand pins the host-RAM demand the gateway pushes
// to the agent as the spec-level ram_mb: max(ram_measured_mb, ram_estimate_mb),
// the same "estimate is a floor, a larger measurement raises it" rule the
// per-GPU VRAM figure follows (TestAgentRuntimeConfigPushesMaxOfMeasuredAndEstimate).
//
// RAM is host-scoped, not per-card, so the figure rides on the spec itself and
// never on a gpus[] row. A 0 on either side is "unknown", never "needs none": a
// never-measured spec yields the estimate, an estimate of 0 yields the
// measurement, and both 0 stays 0 so the agent still sees an unknown demand.
// Neither stored figure is rewritten by serving the larger of the two.
func TestAgentConfigCarriesRAMDemand(t *testing.T) {
	cases := []struct {
		name     string
		measured int
		estimate int
		want     int
	}{
		{name: "measured above estimate raises it", measured: 5000, estimate: 4096, want: 5000},
		{name: "never measured yields the estimate", measured: 0, estimate: 4096, want: 4096},
		{name: "measured below estimate is floored at the estimate", measured: 3000, estimate: 4096, want: 4096},
		{name: "no estimate yields the measurement", measured: 5000, estimate: 0, want: 5000},
		{name: "both unknown stays unknown", measured: 0, estimate: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			ctx := context.Background()
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			app := seedServerAgentApplication(t, routeStore, server.ID, now)
			mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "llama", AppModelName: "llama-3-8b"})
			if err != nil {
				t.Fatalf("CreateMapping: %v", err)
			}
			spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
				Enabled:       true,
				Binary:        "/usr/bin/llama-server",
				GPUs:          []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
				RAMEstimateMB: tc.estimate,
			})
			if err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			if tc.measured > 0 {
				// The agent measured its own child's resident RAM and wrote it back.
				if err := routeStore.UpdateRuntimeSpecRAMMeasured(ctx, spec.ID, tc.measured); err != nil {
					t.Fatalf("seed measured RAM: %v", err)
				}
			}

			dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
			if err != nil {
				t.Fatalf("AgentRuntimeConfig: %v", err)
			}
			if len(dto.Specs) != 1 {
				t.Fatalf("dto.Specs = %#v, want exactly one spec", dto.Specs)
			}
			if got := dto.Specs[0].RAMMB; got != tc.want {
				t.Fatalf("pushed ram_mb = %d for measured=%d estimate=%d, want max(measured, estimate) = %d", got, tc.measured, tc.estimate, tc.want)
			}

			// The wire key is the spec-level ram_mb, and it is not a per-GPU
			// field: RAM is a host resource, so a gpus[] row must not carry it.
			raw, err := json.Marshal(dto.Specs[0])
			if err != nil {
				t.Fatalf("marshal spec dto: %v", err)
			}
			var wire struct {
				RAMMB *int             `json:"ram_mb"`
				GPUs  []map[string]any `json:"gpus"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("unmarshal wire spec: %v", err)
			}
			if wire.RAMMB == nil || *wire.RAMMB != tc.want {
				t.Fatalf("wire ram_mb = %v, want an explicit %d on the spec (never omitted, so 0 stays 'unknown' on the wire)", wire.RAMMB, tc.want)
			}
			for _, g := range wire.GPUs {
				if _, ok := g["ram_mb"]; ok {
					t.Fatalf("a gpus[] row carries ram_mb (%v); RAM is host-scoped and belongs on the spec only", g)
				}
			}

			// Serving the larger of the two must not rewrite either stored
			// figure: the estimate stays the operator's and the measurement
			// stays the agent's.
			stored, ok, err := routeStore.RuntimeSpecByID(ctx, spec.ID)
			if err != nil || !ok {
				t.Fatalf("RuntimeSpecByID: ok=%v err=%v", ok, err)
			}
			if stored.RAMMeasuredMB != tc.measured || stored.RAMEstimateMB != tc.estimate {
				t.Fatalf("stored (measured, estimate) = (%d, %d), want (%d, %d) untouched", stored.RAMMeasuredMB, stored.RAMEstimateMB, tc.measured, tc.estimate)
			}
		})
	}
}

// --- set_visible_devices -----------------------------------------------------

// seedVisibleDevicesMapping builds the server + server_agent application +
// mapping the set_visible_devices tests below all need, and returns the
// mapping id.
func seedVisibleDevicesMapping(t *testing.T, svc *Service, routeStore *routing.MemoryStore, now time.Time) string {
	t.Helper()
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "qwen", AppModelName: "qwen-upstream",
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return mapping.ID
}

// TestPutRuntimeSpecSetVisibleDevicesRoundTrip proves the flag survives the
// whole write path — request -> store -> portal DTO -> the agent's
// runtime-config document. The last hop is the one that matters: the flag is
// useless unless the AGENT sees it, and the agent is the only party that knows
// which visibility variable this host's hardware wants.
func TestPutRuntimeSpecSetVisibleDevicesRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:           true,
		Binary:            "/usr/local/bin/llama-server",
		SetVisibleDevices: true,
		GPUs:              []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}, {Index: 5, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.SetVisibleDevices {
		t.Fatalf("dto.SetVisibleDevices = false, want true")
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if !got.SetVisibleDevices {
		t.Fatalf("GetRuntimeSpec().SetVisibleDevices = false, want true")
	}

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != 1 {
		t.Fatalf("runtime-config specs = %#v, want exactly one", cfg.Specs)
	}
	if !cfg.Specs[0].SetVisibleDevices {
		t.Fatalf("the runtime-config document dropped set_visible_devices: %#v", cfg.Specs[0])
	}
	// The gateway carries the flag and NOTHING about the variable name: it is
	// hardware-agnostic and cannot know whether this host runs NVIDIA or AMD.
	if len(cfg.Specs[0].Env) != 0 {
		t.Errorf("the gateway injected env %#v; resolving the visibility variable is the agent's job alone", cfg.Specs[0].Env)
	}

	// Turning it back off is an ordinary full-document upsert.
	off, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server",
		GPUs:   []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (off): %v", err)
	}
	if off.SetVisibleDevices {
		t.Errorf("set_visible_devices survived a PUT that did not ask for it; this endpoint is a full-document replace")
	}
}

// TestPutRuntimeSpecSetVisibleDevicesRequiresGPUs is trap 1 at the save
// boundary: an empty visible-devices value means NOTHING is visible, so the
// option with no GPU rows is refused rather than persisted and handed to the
// agent. (The agent refuses it again at launch, for the file-mode path that
// never passes through here — see validateRuntimeSpecVisibleDevices.)
func TestPutRuntimeSpecSetVisibleDevicesRequiresGPUs(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	for _, gpus := range [][]RuntimeSpecGPUDTO{nil, {}} {
		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			GPUs:              gpus,
		})
		if !errors.Is(err, ErrRuntimeSpecVisibleDevicesNoGPUs) {
			t.Fatalf("PutRuntimeSpec(gpus=%#v) error = %v, want ErrRuntimeSpecVisibleDevicesNoGPUs", gpus, err)
		}
	}

	// Validate-before-mutate: the refused write must have persisted nothing.
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.Configured {
		t.Fatalf("a refused write created a spec row: %#v", got)
	}

	// The same option with a GPU row is accepted — the refusal is about the
	// empty list, not about the option.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:            "/usr/local/bin/llama-server",
		SetVisibleDevices: true,
		GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
	}); err != nil {
		t.Fatalf("PutRuntimeSpec with one gpu row: %v", err)
	}

	// And a spec with NO gpus and the option OFF stays perfectly valid: that
	// is every CPU-only and every pre-feature spec.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server",
	}); err != nil {
		t.Fatalf("PutRuntimeSpec with no gpus and the option off: %v", err)
	}
}

// TestPutRuntimeSpecSetVisibleDevicesConflictsWithEnv is trap 3 at the save
// boundary: the option and a hand-set visibility variable are two sources for
// one value, and a spec where both are present reads identically whichever one
// wins. Refused, not silently resolved.
//
// The refusal is vendor-independent — all three owned variables, on every
// host — because this gateway cannot know what hardware the target server has,
// and the agent's rule is deliberately written to be the same one.
func TestPutRuntimeSpecSetVisibleDevicesConflictsWithEnv(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"} {
		t.Run(key, func(t *testing.T) {
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
				Binary:            "/usr/local/bin/llama-server",
				SetVisibleDevices: true,
				Env:               map[string]string{key: "0,1"},
				GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
			})
			if !errors.Is(err, ErrRuntimeSpecVisibleDevicesConflict) {
				t.Fatalf("PutRuntimeSpec with env %s error = %v, want ErrRuntimeSpecVisibleDevicesConflict", key, err)
			}
		})
		t.Run(key+" is fine with the option off", func(t *testing.T) {
			dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
				Binary: "/usr/local/bin/llama-server",
				Env:    map[string]string{key: "0,1"},
				GPUs:   []RuntimeSpecGPUDTO{{Index: 0}},
			})
			if err != nil {
				t.Fatalf("a hand-set %s with the option off must still save: %v", key, err)
			}
			if dto.Env[key] != "0,1" {
				t.Errorf("env = %#v, want the operator's own %s preserved verbatim", dto.Env, key)
			}
		})
	}

	// ONEAPI_DEVICE_SELECTOR is deliberately NOT owned: composing it with the
	// option (built from ${HOST_GPU_IDS}) is the documented escape hatch for a
	// runtime the agent has no vendor mapping for.
	t.Run("ONEAPI_DEVICE_SELECTOR composes", func(t *testing.T) {
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			Env:               map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"},
			GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
		}); err != nil {
			t.Fatalf("ONEAPI_DEVICE_SELECTOR must compose with set_visible_devices: %v", err)
		}
	})
}

// TestRuntimeSpecDTOCarriesVisibleDevicesMode pins that RuntimeSpecDTO /
// PutRuntimeSpecRequest / the agent-wire AgentRuntimeSpecDTO all carry
// visible_devices_mode, that PutRuntimeSpec+GetRuntimeSpec round-trip it, and
// that an omitted mode defaults to "env".
func TestRuntimeSpecDTOCarriesVisibleDevicesMode(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// args mode round-trips (with a placeholder present so validation passes).
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:            true,
		Binary:             "/usr/local/bin/llama-server",
		Args:               []string{"--device", "${CUDA_DEVICES}"},
		SetVisibleDevices:  true,
		VisibleDevicesMode: string(routing.VisibleDevicesModeArgs),
		GPUs:               []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("dto.VisibleDevicesMode = %q, want args", dto.VisibleDevicesMode)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("GetRuntimeSpec().VisibleDevicesMode = %q, want args", got.VisibleDevicesMode)
	}

	// The agent-wire document carries the mode too (the agent needs it to
	// decide whether to inject the visibility env var).
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != 1 || cfg.Specs[0].VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("agent wire dropped the mode: %#v", cfg.Specs)
	}

	// An omitted mode defaults to env.
	def, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (default): %v", err)
	}
	if def.VisibleDevicesMode != string(routing.VisibleDevicesModeEnv) {
		t.Fatalf("default mode = %q, want env", def.VisibleDevicesMode)
	}
}

// TestGetRuntimeSpecAPITokenFieldsAndAppEchoes covers the DTO's read-only
// api-token surface: the per-spec mode/header-source/header, the
// presence-only booleans for both the spec's own sealed token and the parent
// application's sealed token, and the app's header name -- with neither
// sealed VALUE ever crossing onto the wire. The spec is seeded directly at
// the store layer (like TestGetRuntimeSpecEnvNullNormalizedToEmptyObject
// above) to exercise GetRuntimeSpec's read/DTO-mapping in isolation from
// PutRuntimeSpec's own write/validation logic, which is covered separately
// by TestPutRuntimeSpecAPITokenValidation and the seal/rotate tests below.
func TestGetRuntimeSpecAPITokenFieldsAndAppEchoes(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	const appSealedToken = "sealed:app-token-do-not-leak"
	app.APIToken = appSealedToken
	app.APITokenHeader = "Authorization"
	if err := routeStore.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	const specSealedToken = "sealed:spec-token-do-not-leak"
	spec := routing.RuntimeSpec{
		ID:                   "rspec_" + compactRandomHex(16),
		MappingID:            mapping.ID,
		Enabled:              true,
		Binary:               "/usr/bin/x",
		Args:                 "[]",
		Env:                  "{}",
		APITokenMode:         string(routing.RuntimeAPITokenModeSet),
		APIToken:             specSealedToken,
		APITokenHeaderSource: string(routing.RuntimeAPITokenHeaderSourceCustom),
		APITokenHeader:       "X-Api-Key",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if dto.APITokenMode != "set" {
		t.Fatalf("APITokenMode = %q, want set", dto.APITokenMode)
	}
	if !dto.APITokenSet {
		t.Fatalf("APITokenSet = false, want true")
	}
	if dto.APITokenHeaderSource != "custom" {
		t.Fatalf("APITokenHeaderSource = %q, want custom", dto.APITokenHeaderSource)
	}
	if dto.APITokenHeader != "X-Api-Key" {
		t.Fatalf("APITokenHeader = %q, want X-Api-Key", dto.APITokenHeader)
	}
	if !dto.AppAPITokenSet {
		t.Fatalf("AppAPITokenSet = false, want true")
	}
	if dto.AppAPITokenHeader != "Authorization" {
		t.Fatalf("AppAPITokenHeader = %q, want Authorization", dto.AppAPITokenHeader)
	}

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), specSealedToken) {
		t.Fatalf("wire body leaked the spec's sealed token: %s", raw)
	}
	if strings.Contains(string(raw), appSealedToken) {
		t.Fatalf("wire body leaked the app's sealed token: %s", raw)
	}
}

// TestGetRuntimeSpecAPITokenModeDefaultsToApp pins the "app" default from
// both directions the brief calls out: the unconfigured DTO (no spec row —
// the literal at GetRuntimeSpec's ~:414) and a spec written through
// PutRuntimeSpec with no api_token_mode in the request, which normalizes the
// omitted mode before persisting. Neither may surface "" on the wire.
func TestGetRuntimeSpecAPITokenModeDefaultsToApp(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	unconfigured, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec (unconfigured): %v", err)
	}
	if unconfigured.APITokenMode != "app" || unconfigured.APITokenHeaderSource != "app" {
		t.Fatalf("unconfigured defaults = mode %q source %q, want app/app", unconfigured.APITokenMode, unconfigured.APITokenHeaderSource)
	}
	if unconfigured.AppAPITokenSet || unconfigured.AppAPITokenHeader != "" {
		t.Fatalf("unconfigured app echoes = set %v header %q, want false/\"\"", unconfigured.AppAPITokenSet, unconfigured.AppAPITokenHeader)
	}

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.APITokenMode != "app" || got.APITokenHeaderSource != "app" {
		t.Fatalf("configured defaults = mode %q source %q, want app/app", got.APITokenMode, got.APITokenHeaderSource)
	}
}

// TestAgentRuntimeConfigPushesDecryptedAPIToken pins resolvePushToken's
// contract: the agent-wire runtime config carries the DECRYPTED per-spec
// upstream token in api_token, resolved per mode (set/random -> the spec's
// own sealed token; app/"" -> the parent app's sealed token; off -> ""), and
// FAILS CLOSED -- an undecryptable
// sealed value pushes "" (never the sealed bytes), so the agent hard-errors on
// an unresolved ${API_TOKEN} rather than booting the child with a garbled
// secret. The plaintext exists only as this wire field; the SEALED form is
// never carried, and nothing decrypted is stored.
func TestAgentRuntimeConfigPushesDecryptedAPIToken(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cipher := newTestCipher(t)
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	// The parent app carries a sealed token that decrypts to "tok-app"; the
	// "app"/unset modes push exactly this.
	appSealed, err := capture.SealSecret(cipher, false, "tok-app")
	if err != nil {
		t.Fatalf("seal app token: %v", err)
	}
	app.APIToken = appSealed
	if err := routeStore.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}

	// The set-mode spec carries a sealed token that decrypts to "tok-set".
	specSealed, err := capture.SealSecret(cipher, false, "tok-set")
	if err != nil {
		t.Fatalf("seal spec token: %v", err)
	}

	// seedSpec creates a mapping + a directly-stored enabled spec (bypassing
	// PutRuntimeSpec, which is covered separately by
	// TestPutRuntimeSpecAPITokenValidation and the seal/rotate tests),
	// returning the spec id so the pushed document can be matched back to its
	// mode.
	seedSpec := func(model, mode, sealedToken string) string {
		t.Helper()
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: model, AppModelName: model})
		if err != nil {
			t.Fatalf("CreateMapping(%s): %v", model, err)
		}
		specID := "rspec_" + compactRandomHex(16)
		spec := routing.RuntimeSpec{
			ID:           specID,
			MappingID:    mapping.ID,
			Enabled:      true,
			Binary:       "/usr/bin/x",
			Args:         "[]",
			Env:          "{}",
			APITokenMode: mode,
			APIToken:     sealedToken,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("seed spec(%s): %v", model, err)
		}
		return specID
	}

	setID := seedSpec("m-set", string(routing.RuntimeAPITokenModeSet), specSealed)
	appModeID := seedSpec("m-app", string(routing.RuntimeAPITokenModeApp), "")
	offID := seedSpec("m-off", string(routing.RuntimeAPITokenModeOff), specSealed)
	// Fail-closed: mode set, but the sealed value cannot be decrypted (invalid
	// base64 under the "enc:" prefix). Must push "" -- never the sealed bytes.
	const undecryptable = "enc:garbage"
	failID := seedSpec("m-fail", string(routing.RuntimeAPITokenModeSet), undecryptable)

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	pushed := map[string]string{}
	for _, s := range cfg.Specs {
		pushed[s.ID] = s.APIToken
	}
	if got := pushed[setID]; got != "tok-set" {
		t.Fatalf("set-mode api_token = %q, want %q", got, "tok-set")
	}
	if got := pushed[appModeID]; got != "tok-app" {
		t.Fatalf("app-mode api_token = %q, want %q", got, "tok-app")
	}
	if got := pushed[offID]; got != "" {
		t.Fatalf("off-mode api_token = %q, want empty", got)
	}
	if got := pushed[failID]; got != "" {
		t.Fatalf("fail-closed api_token = %q, want empty (must never push the sealed bytes)", got)
	}

	// No SEALED token may appear anywhere on the pushed document -- only the
	// resolved plaintext ("tok-set"/"tok-app") is legitimate wire content.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	for _, sealed := range []string{undecryptable, appSealed, specSealed} {
		if strings.Contains(string(raw), sealed) {
			t.Fatalf("pushed document leaked a SEALED token %q: %s", sealed, raw)
		}
	}
}

// TestAgentRuntimeConfigPushesResolvedTypeAndProbePaths pins that the
// agent-wire document carries the RESOLVED effective type + probe paths
// (routing.EffectiveRuntimeSpecType / routing.DeriveProbePaths), never the
// raw stored spec.Type/MetricsPath/ContextProbePath: an unset Type on a
// llama-server binary resolves to "llama_cpp" with that kind's default probe
// paths; explicit overrides ride through unchanged; and a "custom" spec with
// no overrides pushes empty paths rather than any other kind's defaults.
func TestAgentRuntimeConfigPushesResolvedTypeAndProbePaths(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	seedSpec := func(model, binary, specType, metricsOverride, contextOverride string) string {
		t.Helper()
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: model, AppModelName: model})
		if err != nil {
			t.Fatalf("CreateMapping(%s): %v", model, err)
		}
		specID := "rspec_" + compactRandomHex(16)
		spec := routing.RuntimeSpec{
			ID:               specID,
			MappingID:        mapping.ID,
			Enabled:          true,
			Binary:           binary,
			Args:             "[]",
			Env:              "{}",
			Type:             specType,
			MetricsPath:      metricsOverride,
			ContextProbePath: contextOverride,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("seed spec(%s): %v", model, err)
		}
		return specID
	}

	// Auto-detect: Type unset, binary is llama-server -> resolves to
	// llama_cpp's default probe paths.
	autoID := seedSpec("m-auto", "/usr/local/bin/llama-server", "", "", "")
	// Explicit overrides ride through unchanged regardless of type.
	overrideID := seedSpec("m-override", "/usr/local/bin/llama-server", "vllm", "/custom-metrics", "/custom-context")
	// custom with no overrides -> empty paths (not another kind's defaults).
	customID := seedSpec("m-custom", "/usr/local/bin/my-server", "custom", "", "")

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	type resolved struct {
		specType, metrics, context string
	}
	pushed := map[string]resolved{}
	for _, s := range cfg.Specs {
		pushed[s.ID] = resolved{s.Type, s.MetricsPath, s.ContextProbePath}
	}

	if got, want := pushed[autoID], (resolved{"llama_cpp", "/metrics", "/props"}); got != want {
		t.Fatalf("auto-detected spec = %+v, want %+v", got, want)
	}
	if got, want := pushed[overrideID], (resolved{"vllm", "/custom-metrics", "/custom-context"}); got != want {
		t.Fatalf("overridden spec = %+v, want %+v", got, want)
	}
	if got, want := pushed[customID], (resolved{"custom", "", ""}); got != want {
		t.Fatalf("custom spec = %+v, want %+v", got, want)
	}
}

// TestPutRuntimeSpecPreservesGPUArrayOrder pins that the request array order of
// gpus is the stored + response + agent-wire order (not re-sorted by index).
func TestPutRuntimeSpecPreservesGPUArrayOrder(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Deliberately NOT ascending by index.
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		GPUs: []RuntimeSpecGPUDTO{{Index: 5, VRAMEstimateMB: 1}, {Index: 2, VRAMEstimateMB: 2}, {Index: 3, VRAMEstimateMB: 3}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	wantOrder := []int{5, 2, 3}
	gotOrder := func(gs []RuntimeSpecGPUDTO) []int {
		out := make([]int, len(gs))
		for i, g := range gs {
			out[i] = g.Index
		}
		return out
	}
	if o := gotOrder(dto.GPUs); !slicesEqualInt(o, wantOrder) {
		t.Fatalf("response gpu order = %v, want %v", o, wantOrder)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if o := gotOrder(got.GPUs); !slicesEqualInt(o, wantOrder) {
		t.Fatalf("read-back gpu order = %v, want %v", o, wantOrder)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	agentOrder := make([]int, len(cfg.Specs[0].GPUs))
	for i, g := range cfg.Specs[0].GPUs {
		agentOrder[i] = g.Index
	}
	if !slicesEqualInt(agentOrder, wantOrder) {
		t.Fatalf("agent-wire gpu order = %v, want %v", agentOrder, wantOrder)
	}
}

func slicesEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPutRuntimeSpecVisibleDevicesModeValidation pins the two new mode traps:
// an invalid mode value, and args mode with no ${..._DEVICES} placeholder in
// args. The conflict + no-gpus traps still fire in BOTH modes.
func TestPutRuntimeSpecVisibleDevicesModeValidation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			GPUs:              []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
		}
	}
	cases := []struct {
		name    string
		mutate  func(PutRuntimeSpecRequest) PutRuntimeSpecRequest
		wantErr error
	}{
		{
			name:    "bad mode value",
			mutate:  func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.VisibleDevicesMode = "bogus"; return r },
			wantErr: ErrRuntimeSpecVisibleDevicesModeInvalid,
		},
		{
			name: "args mode with no device placeholder",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
				r.Args = []string{"--ctx-size", "4096"}
				return r
			},
			wantErr: ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, tc.mutate(base())); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Each of the three placeholder tokens satisfies args mode.
	for _, ph := range []string{"${CUDA_DEVICES}", "${VULKAN_DEVICES}", "${METAL_DEVICES}"} {
		t.Run("accepts "+ph, func(t *testing.T) {
			r := base()
			r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
			r.Args = []string{"--device", ph}
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); err != nil {
				t.Fatalf("args mode with %s must save: %v", ph, err)
			}
		})
	}

	// The conflict trap still fires in args mode (a hand-set CUDA_VISIBLE_DEVICES
	// remaps the CUDA namespace and would break --device numbering).
	t.Run("conflict trap holds in args mode", func(t *testing.T) {
		r := base()
		r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
		r.Args = []string{"--device", "${CUDA_DEVICES}"}
		r.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"}
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); !errors.Is(err, ErrRuntimeSpecVisibleDevicesConflict) {
			t.Fatalf("err = %v, want ErrRuntimeSpecVisibleDevicesConflict", err)
		}
	})

	// Mode is inert when the option is OFF: a bogus mode with the flag off is
	// still rejected as a malformed enum, but args-with-no-placeholder is NOT
	// (the placeholder requirement only applies with the flag on).
	t.Run("args no-placeholder ok when flag off", func(t *testing.T) {
		r := base()
		r.SetVisibleDevices = false
		r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
		r.Args = []string{"--ctx-size", "4096"}
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); err != nil {
			t.Fatalf("args mode with flag off must save: %v", err)
		}
	})
}

// TestPutRuntimeSpecAPITokenValidation covers validateRuntimeSpecAPIToken:
// the four api_token_* error sentinels and the ${API_TOKEN} placeholder
// matrix (required for set/random, optional for app, forbidden for off),
// plus the header-source/header-shape check. It does NOT cover
// sealing/rotation (that's putRuntimeSpec's write path, below) or the
// app-token resolver (resolvePushToken) -- every case here is a pure
// validation decision on the request alone.
func TestPutRuntimeSpecAPITokenValidation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary: "/usr/local/bin/llama-server",
		}
	}
	cases := []struct {
		name    string
		mutate  func(PutRuntimeSpecRequest) PutRuntimeSpecRequest
		wantErr error // nil means "must save without error"
	}{
		{
			name:    "bogus mode",
			mutate:  func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.APITokenMode = "bogus"; return r },
			wantErr: ErrRuntimeSpecAPITokenModeInvalid,
		},
		{
			name: "set with no placeholder anywhere",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeSet)
				return r
			},
			wantErr: ErrRuntimeSpecAPITokenNoPlaceholder,
		},
		{
			name: "random with no placeholder",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeRandom)
				return r
			},
			wantErr: ErrRuntimeSpecAPITokenNoPlaceholder,
		},
		{
			name: "app with no placeholder is fine -- optional for app",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeApp)
				return r
			},
			wantErr: nil,
		},
		{
			name: "off with a placeholder left in env",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeOff)
				r.Env = map[string]string{"X": "${API_TOKEN}"}
				return r
			},
			wantErr: ErrRuntimeSpecAPITokenPlaceholderWithoutMode,
		},
		{
			name: "set with placeholder but a malformed custom header",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeSet)
				r.Env = map[string]string{"VLLM_API_KEY": "${API_TOKEN}"}
				r.APITokenHeaderSource = string(routing.RuntimeAPITokenHeaderSourceCustom)
				r.APITokenHeader = "Bad Header:"
				return r
			},
			wantErr: ErrRuntimeSpecAPITokenHeaderInvalid,
		},
		{
			name: "bogus header source",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenHeaderSource = "bogus"
				return r
			},
			wantErr: ErrRuntimeSpecAPITokenHeaderInvalid,
		},
		{
			name: "set with placeholder in args, header source app -- valid",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeSet)
				r.Args = []string{"--api-key", "${API_TOKEN}"}
				r.APITokenHeaderSource = string(routing.RuntimeAPITokenHeaderSourceApp)
				return r
			},
			wantErr: nil,
		},
		{
			// No app_unset error exists (validateRuntimeSpecAPIToken does not
			// consume the parent application's token at all -- that is
			// resolvePushToken's job): mode "app" never fails validation
			// regardless of the app's own token.
			name: "app mode never trips on the app's own (empty) token",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APITokenMode = string(routing.RuntimeAPITokenModeApp)
				return r
			},
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, tc.mutate(base()))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPutRuntimeSpecTypeValidation covers validRuntimeSpecType: every
// routing.RuntimeSpecType value (seven since audio_cpp, which an operator must
// be able to pick explicitly) plus "" (auto-detect) is accepted, anything else
// is ErrRuntimeSpecTypeInvalid. Mirrors TestPutRuntimeSpecAPITokenValidation's
// shape for the sibling api_token_mode validator.
func TestPutRuntimeSpecTypeValidation(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary: "/usr/local/bin/llama-server",
		}
	}

	t.Run("bogus type", func(t *testing.T) {
		r := base()
		r.Type = "bogus"
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); !errors.Is(err, ErrRuntimeSpecTypeInvalid) {
			t.Fatalf("err = %v, want ErrRuntimeSpecTypeInvalid", err)
		}
	})

	validTypes := []string{
		"",
		string(routing.RuntimeSpecTypeVLLM),
		string(routing.RuntimeSpecTypeLlamaCpp),
		string(routing.RuntimeSpecTypeTGI),
		string(routing.RuntimeSpecTypeOllama),
		string(routing.RuntimeSpecTypeStableDiffusionCpp),
		string(routing.RuntimeSpecTypeAudioCpp),
		string(routing.RuntimeSpecTypeCustom),
	}
	for _, typ := range validTypes {
		t.Run("accepts type "+typ, func(t *testing.T) {
			r := base()
			r.Type = typ
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); err != nil {
				t.Fatalf("type %q must save: %v", typ, err)
			}
		})
	}
}

// TestPutRuntimeSpecSavesAnAudioCppSpec is the audio counterpart of an
// sd-server spec save: an explicit audio_cpp type and an openai_audio flavor
// are stored and echoed back, neither refused as ErrRuntimeSpecTypeInvalid or
// ErrRuntimeSpecFlavorInvalid. The health path stays the stock /health
// (runtimeSpecHealthPathDefault falls through for audio_cpp, and audio.cpp
// serves GET /health), unlike sd-server's /v1/models.
func TestPutRuntimeSpecSavesAnAudioCppSpec(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:     "/opt/audiocpp/audiocpp_server",
		Type:       string(routing.RuntimeSpecTypeAudioCpp),
		APIFlavors: []string{routing.APIFlavorOpenAIAudio},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec(audio_cpp, [openai_audio]): %v", err)
	}
	if dto.Type != string(routing.RuntimeSpecTypeAudioCpp) || dto.EffectiveType != string(routing.RuntimeSpecTypeAudioCpp) {
		t.Fatalf("type/effective_type = %q/%q, want audio_cpp for both", dto.Type, dto.EffectiveType)
	}
	if want := []string{routing.APIFlavorOpenAIAudio}; !slices.Equal(dto.APIFlavors, want) {
		t.Fatalf("api_flavors = %#v, want %#v", dto.APIFlavors, want)
	}
	if dto.HealthPath != defaultRuntimeSpecHealthPath {
		t.Fatalf("health_path = %q, want the stock %q", dto.HealthPath, defaultRuntimeSpecHealthPath)
	}
}

// TestRuntimeSpecDTOEffectiveTypeAndResolvedPaths pins the read-only
// effective_type/resolved_metrics_path/resolved_context_probe_path echoes
// (design 2026-09-07): when type is "" (auto), effective_type is what
// routing.DetectRuntimeSpecType derives from the binary and the resolved
// paths are that type's routing.DeriveProbePaths defaults; an explicit type
// or an explicit metrics_path/context_probe_path always wins over detection.
func TestRuntimeSpecDTOEffectiveTypeAndResolvedPaths(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	newMapping := func(name string) string {
		t.Helper()
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: name, AppModelName: name})
		if err != nil {
			t.Fatalf("CreateMapping(%s): %v", name, err)
		}
		return mapping.ID
	}

	t.Run("auto-detects from binary, default paths", func(t *testing.T) {
		mappingID := newMapping("m-auto")
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Enabled: true, Binary: "/usr/local/bin/llama-server",
		})
		if err != nil {
			t.Fatalf("PutRuntimeSpec: %v", err)
		}
		if dto.Type != "" {
			t.Fatalf("Type = %q, want empty (stored as-is)", dto.Type)
		}
		if dto.EffectiveType != string(routing.RuntimeSpecTypeLlamaCpp) {
			t.Fatalf("EffectiveType = %q, want %q", dto.EffectiveType, routing.RuntimeSpecTypeLlamaCpp)
		}
		if dto.ResolvedMetricsPath != "/metrics" || dto.ResolvedContextProbePath != "/props" {
			t.Fatalf("resolved paths = %q/%q, want /metrics //props", dto.ResolvedMetricsPath, dto.ResolvedContextProbePath)
		}
		// GetRuntimeSpec must echo the same resolved values as the PutRuntimeSpec response.
		got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec: %v", err)
		}
		if got.EffectiveType != dto.EffectiveType || got.ResolvedMetricsPath != dto.ResolvedMetricsPath || got.ResolvedContextProbePath != dto.ResolvedContextProbePath {
			t.Fatalf("GetRuntimeSpec echoes = %+v, want to match PutRuntimeSpec response", got)
		}
	})

	t.Run("explicit type overrides detection; explicit paths override type defaults", func(t *testing.T) {
		mappingID := newMapping("m-explicit")
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Enabled:          true,
			Binary:           "/usr/local/bin/llama-server",
			Type:             string(routing.RuntimeSpecTypeVLLM),
			MetricsPath:      "/custom-metrics",
			ContextProbePath: "/custom-context",
		})
		if err != nil {
			t.Fatalf("PutRuntimeSpec: %v", err)
		}
		if dto.Type != string(routing.RuntimeSpecTypeVLLM) {
			t.Fatalf("Type = %q, want vllm", dto.Type)
		}
		if dto.EffectiveType != string(routing.RuntimeSpecTypeVLLM) {
			t.Fatalf("EffectiveType = %q, want vllm", dto.EffectiveType)
		}
		if dto.MetricsPath != "/custom-metrics" || dto.ContextProbePath != "/custom-context" {
			t.Fatalf("stored paths = %q/%q, want the overrides verbatim", dto.MetricsPath, dto.ContextProbePath)
		}
		if dto.ResolvedMetricsPath != "/custom-metrics" || dto.ResolvedContextProbePath != "/custom-context" {
			t.Fatalf("resolved paths = %q/%q, want the overrides", dto.ResolvedMetricsPath, dto.ResolvedContextProbePath)
		}
	})

	t.Run("no spec yet: empty-spec default DTO detects custom from empty binary", func(t *testing.T) {
		mappingID := newMapping("m-none")
		dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec: %v", err)
		}
		if dto.Configured {
			t.Fatalf("Configured = true, want false for a mapping with no spec row")
		}
		if dto.Type != "" || dto.MetricsPath != "" || dto.ContextProbePath != "" {
			t.Fatalf("zero-spec DTO fields = %q/%q/%q, want all empty", dto.Type, dto.MetricsPath, dto.ContextProbePath)
		}
		if dto.EffectiveType != string(routing.RuntimeSpecTypeCustom) {
			t.Fatalf("EffectiveType = %q, want custom (detected from an empty binary)", dto.EffectiveType)
		}
		if dto.ResolvedMetricsPath != "" || dto.ResolvedContextProbePath != "" {
			t.Fatalf("resolved paths = %q/%q, want empty for custom with no overrides", dto.ResolvedMetricsPath, dto.ResolvedContextProbePath)
		}
	})
}

// --- seal-on-write, random generation, rotate -------------------------------
//
// These tests pin the WRITE side of the per-spec API token in PutRuntimeSpec:
// mode "set" seals the operator-supplied value, mode "random" generates and
// seals a fresh secret, the write-only *string sentinel keeps/clears, rotate
// regenerates, and a keyless disk store fails closed (ErrKeyRequired) BEFORE
// anything is persisted. The sealed VALUE never crosses the wire.

// TestPutRuntimeSpecAPITokenSealSetStoresSealed: mode "set" with a value stores
// a SEALED envelope that decrypts back to the operator's token, flips
// APITokenSet true, and never echoes the raw value in the DTO JSON.
func TestPutRuntimeSpecAPITokenSealSetStoresSealed(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cipher := newTestCipher(t)
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	const rawToken = "s3cr3t"
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:       "/usr/local/bin/llama-server",
		Env:          map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
		APITokenMode: string(routing.RuntimeAPITokenModeSet),
		APIToken:     strPtr(rawToken),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.APITokenSet {
		t.Fatalf("APITokenSet = false, want true after setting a token")
	}
	if dto.APITokenMode != string(routing.RuntimeAPITokenModeSet) {
		t.Fatalf("APITokenMode = %q, want set", dto.APITokenMode)
	}

	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping: ok=%v err=%v", ok, err)
	}
	if !(strings.HasPrefix(stored.APIToken, "enc:") || strings.HasPrefix(stored.APIToken, "plain:")) {
		t.Fatalf("stored APIToken = %q, want a sealed envelope (enc:/plain:)", stored.APIToken)
	}
	if strings.Contains(stored.APIToken, rawToken) {
		t.Fatalf("stored APIToken leaks the raw token: %q", stored.APIToken)
	}
	opened, err := capture.OpenSecret(cipher, stored.APIToken)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if opened != rawToken {
		t.Fatalf("decrypted token = %q, want %q", opened, rawToken)
	}

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), rawToken) {
		t.Fatalf("DTO JSON leaked the raw token: %s", raw)
	}
}

// TestPutRuntimeSpecAPITokenSealRandomGenerates: mode "random" with no
// api_token supplied generates a fresh secret, seals it, and reports
// APITokenSet true. The stored value is a real generated secret (opaigw_
// prefix), not a caller-supplied string.
func TestPutRuntimeSpecAPITokenSealRandomGenerates(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cipher := newTestCipher(t)
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:       "/usr/local/bin/llama-server",
		Env:          map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
		APITokenMode: string(routing.RuntimeAPITokenModeRandom),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.APITokenSet {
		t.Fatalf("APITokenSet = false, want true after random generation")
	}

	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping: ok=%v err=%v", ok, err)
	}
	if stored.APIToken == "" {
		t.Fatalf("random mode stored an empty token")
	}
	if !strings.HasPrefix(stored.APIToken, "enc:") {
		t.Fatalf("stored APIToken = %q, want a sealed enc: envelope", stored.APIToken)
	}
	opened, err := capture.OpenSecret(cipher, stored.APIToken)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if !strings.HasPrefix(opened, "opaigw_") {
		t.Fatalf("generated token = %q, want a generateSecret() opaigw_ value", opened)
	}

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), opened) {
		t.Fatalf("DTO JSON leaked the generated token: %s", raw)
	}
}

// TestPutRuntimeSpecAPITokenSealUpdateSentinel: on a subsequent PUT, a nil
// api_token pointer KEEPS the stored sealed value byte-for-byte (no needless
// re-seal), while "" CLEARS it.
func TestPutRuntimeSpecAPITokenSealUpdateSentinel(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cipher := newTestCipher(t)
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	setReq := func(tok *string) PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary:       "/usr/local/bin/llama-server",
			Env:          map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
			APITokenMode: string(routing.RuntimeAPITokenModeSet),
			APIToken:     tok,
		}
	}

	const rawToken = "keepme"
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, setReq(strPtr(rawToken))); err != nil {
		t.Fatalf("PutRuntimeSpec (initial set): %v", err)
	}
	before, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("precondition RuntimeSpecByMapping: ok=%v err=%v", ok, err)
	}

	// nil ⇒ KEEP: the sealed envelope must be identical (no re-seal), so the
	// stored ciphertext is byte-for-byte equal to before.
	keptDTO, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, setReq(nil))
	if err != nil {
		t.Fatalf("PutRuntimeSpec (keep): %v", err)
	}
	if !keptDTO.APITokenSet {
		t.Fatalf("nil api_token should KEEP the token; APITokenSet = false")
	}
	kept, _, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if kept.APIToken != before.APIToken {
		t.Fatalf("nil api_token changed the sealed value: %q -> %q", before.APIToken, kept.APIToken)
	}
	if opened, err := capture.OpenSecret(cipher, kept.APIToken); err != nil || opened != rawToken {
		t.Fatalf("kept token opened to %q (err %v), want %q", opened, err, rawToken)
	}

	// "" ⇒ CLEAR.
	clearedDTO, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, setReq(strPtr("")))
	if err != nil {
		t.Fatalf("PutRuntimeSpec (clear): %v", err)
	}
	if clearedDTO.APITokenSet {
		t.Fatalf(`"" api_token should CLEAR the token; APITokenSet = true`)
	}
	cleared, _, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if cleared.APIToken != "" {
		t.Fatalf("cleared token = %q, want empty", cleared.APIToken)
	}
}

// TestPutRuntimeSpecAPITokenSealRotate: mode "random" keeps the existing token
// across an ordinary re-PUT, but api_token_rotate:true regenerates it (a
// different plaintext ends up stored).
func TestPutRuntimeSpecAPITokenSealRotate(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cipher := newTestCipher(t)
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	randomReq := func(rotate bool) PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary:         "/usr/local/bin/llama-server",
			Env:            map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
			APITokenMode:   string(routing.RuntimeAPITokenModeRandom),
			APITokenRotate: rotate,
		}
	}

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, randomReq(false)); err != nil {
		t.Fatalf("PutRuntimeSpec (first random): %v", err)
	}
	first, _, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	firstPlain, err := capture.OpenSecret(cipher, first.APIToken)
	if err != nil {
		t.Fatalf("OpenSecret (first): %v", err)
	}

	// A second random PUT WITHOUT rotate keeps the same sealed value verbatim.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, randomReq(false)); err != nil {
		t.Fatalf("PutRuntimeSpec (random, no rotate): %v", err)
	}
	same, _, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if same.APIToken != first.APIToken {
		t.Fatalf("random without rotate changed the stored token: %q -> %q", first.APIToken, same.APIToken)
	}

	// rotate:true regenerates -- a DIFFERENT plaintext is stored.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, randomReq(true)); err != nil {
		t.Fatalf("PutRuntimeSpec (rotate): %v", err)
	}
	rotated, _, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	rotatedPlain, err := capture.OpenSecret(cipher, rotated.APIToken)
	if err != nil {
		t.Fatalf("OpenSecret (rotated): %v", err)
	}
	if rotatedPlain == firstPlain {
		t.Fatalf("rotate did not change the stored token (still %q)", firstPlain)
	}
	if !strings.HasPrefix(rotatedPlain, "opaigw_") {
		t.Fatalf("rotated token = %q, want a generateSecret() opaigw_ value", rotatedPlain)
	}
}

// TestPutRuntimeSpecAPITokenSealKeylessDiskRejected: on a disk store with no
// cipher, a set/random write fails closed with capture.ErrKeyRequired and
// persists NOTHING -- the prior spec is left byte-for-byte unchanged (no
// half-applied mode, port, or token).
func TestPutRuntimeSpecAPITokenSealKeylessDiskRejected(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestServiceWithCipher(t, now, nil, false) // no cipher, not volatile = keyless disk
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	// Seed a prior spec in app mode (no token ⇒ no seal needed even keyless),
	// with distinctive fields so a half-applied write would be visible.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:       "/usr/local/bin/llama-server",
		ListenPort:   9111,
		APITokenMode: string(routing.RuntimeAPITokenModeApp),
	}); err != nil {
		t.Fatalf("seed app-mode spec: %v", err)
	}
	before, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("precondition: expected a seeded spec (ok=%v err=%v)", ok, err)
	}

	assertUnchanged := func(label string) {
		t.Helper()
		after, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
		if err != nil || !ok {
			t.Fatalf("%s: spec vanished (ok=%v err=%v)", label, ok, err)
		}
		if after.APIToken != "" {
			t.Fatalf("%s: failed write persisted a token: %q", label, after.APIToken)
		}
		if after.Binary != before.Binary || after.ListenPort != before.ListenPort ||
			after.APITokenMode != before.APITokenMode {
			t.Fatalf("%s: failed write half-applied\n before=%+v\n after =%+v", label, before, after)
		}
	}

	// set with a real value on a keyless disk store: fail closed, change nothing.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:       "/usr/local/bin/OTHER",
		ListenPort:   9222,
		Env:          map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
		APITokenMode: string(routing.RuntimeAPITokenModeSet),
		APIToken:     strPtr("s3cr3t"),
	}); !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("set on keyless disk store err = %v, want capture.ErrKeyRequired", err)
	}
	assertUnchanged("after failed set")

	// random likewise.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:       "/usr/local/bin/OTHER",
		ListenPort:   9333,
		Env:          map[string]string{"VLLM_API_KEY": "${API_TOKEN}"},
		APITokenMode: string(routing.RuntimeAPITokenModeRandom),
	}); !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("random on keyless disk store err = %v, want capture.ErrKeyRequired", err)
	}
	assertUnchanged("after failed random")
}

// liveTimingsSpecFixture seeds the server -> server_agent application ->
// service chain the responses-live-timings spec tests share, and returns a
// mapping factory with it. Several of those tests turn on the difference
// between a FIRST spec write and a later save of the same spec, so they need
// more than one mapping -- one spec each -- inside a single service. The
// parent application is returned because one test has to seed a value onto it
// that the application API refuses to store.
func liveTimingsSpecFixture(t *testing.T) (*Service, *routing.MemoryStore, routing.Application, func(string) string) {
	t.Helper()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	newMapping := func(name string) string {
		t.Helper()
		mapping, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: name, AppModelName: name})
		if err != nil {
			t.Fatalf("CreateMapping(%q): %v", name, err)
		}
		return mapping.ID
	}
	return svc, routeStore, app, newMapping
}

// TestPutRuntimeSpecAcceptsStableDiffusionCppType pins that an upsert typed
// stable_diffusion_cpp is accepted like any other kind, and that both the
// spec's own Type and its resolved EffectiveType echo it back.
func TestPutRuntimeSpecAcceptsStableDiffusionCppType(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("sd"), PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/sd-server",
		Type:   string(routing.RuntimeSpecTypeStableDiffusionCpp),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.Type != string(routing.RuntimeSpecTypeStableDiffusionCpp) {
		t.Fatalf("type = %q, want %q", dto.Type, routing.RuntimeSpecTypeStableDiffusionCpp)
	}
	if dto.EffectiveType != string(routing.RuntimeSpecTypeStableDiffusionCpp) {
		t.Fatalf("effective_type = %q, want %q", dto.EffectiveType, routing.RuntimeSpecTypeStableDiffusionCpp)
	}
}

// TestPutRuntimeSpecDefaultsHealthPathPerType pins that an empty HealthPath
// defaults by the spec's EFFECTIVE type (explicit, or detected from the
// binary): the agent kills a child whose health path keeps failing once
// startup_timeout_seconds elapses, and sd-server answers GET /health with
// 404 (measured), so an sd spec that names no health path must not get the
// stock /health default -- while every other type still does, and an
// explicit value always wins regardless of type.
func TestPutRuntimeSpecDefaultsHealthPathPerType(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	cases := []struct {
		name string
		req  PutRuntimeSpecRequest
		want string
	}{
		{
			name: "explicit-stable-diffusion-cpp",
			req:  PutRuntimeSpecRequest{Binary: "/usr/local/bin/sd-server", Type: string(routing.RuntimeSpecTypeStableDiffusionCpp)},
			want: "/v1/models",
		},
		{
			name: "auto-detected-stable-diffusion-cpp",
			req:  PutRuntimeSpecRequest{Binary: "/opt/sd/sd-server"},
			want: "/v1/models",
		},
		{
			name: "llama-cpp-keeps-stock-default",
			req:  PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp)},
			want: "/health",
		},
		{
			name: "explicit-health-path-wins-on-stable-diffusion-cpp",
			req: PutRuntimeSpecRequest{
				Binary: "/usr/local/bin/sd-server", Type: string(routing.RuntimeSpecTypeStableDiffusionCpp),
				HealthPath: "/",
			},
			want: "/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping(tc.name), tc.req)
			if err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			if dto.HealthPath != tc.want {
				t.Fatalf("health_path = %q, want %q", dto.HealthPath, tc.want)
			}
		})
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind pins the
// create default: a FIRST write (no existing spec row) that omits
// responses_live_timings_enabled stores what the spec's own EFFECTIVE kind
// implies -- on for llama_cpp, off for every other kind (vllm included since
// 2026-09-12; see routing.liveTimingsCapableKinds).
//
// The middle case pins WHICH resolver decides that, and nothing else in this
// file does. Its type is "" and its binary is llama-server, so
// routing.EffectiveRuntimeSpecType falls back to
// routing.DetectRuntimeSpecType and answers llama_cpp, while
// routing.LiveTimingsCapableKind("") is false: a default asked about the raw
// req.Type would store false for it -- the commonest managed llama.cpp
// configuration there is -- with both explicitly-typed cases still green.
func TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	cases := []struct {
		name string
		req  PutRuntimeSpecRequest
		want bool
	}{
		{
			name: "explicit-llama-cpp",
			req:  PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp)},
			want: true,
		},
		{
			name: "auto-detected-llama-cpp",
			req:  PutRuntimeSpecRequest{Binary: "/usr/bin/llama-server"},
			want: true,
		},
		{
			name: "explicit-ollama",
			req:  PutRuntimeSpecRequest{Binary: "/usr/local/bin/ollama", Type: string(routing.RuntimeSpecTypeOllama)},
			want: false,
		},
	}
	for _, tc := range cases {
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping(tc.name), tc.req)
		if err != nil {
			t.Fatalf("%s: PutRuntimeSpec: %v", tc.name, err)
		}
		if dto.ResponsesLiveTimingsEnabled != tc.want {
			t.Fatalf("%s: responses_live_timings_enabled = %v, want %v (a first write with the key absent takes the effective kind's default)",
				tc.name, dto.ResponsesLiveTimingsEnabled, tc.want)
		}
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsDoesNotInheritTheParentApplication
// extends PutRuntimeSpec's standing "no backend inheritance" contract (the
// same contract TestPutRuntimeSpecDoesNotInheritAppModes pins for the
// flavors/modes trio) to the live-timings opt-in: the spec's own effective
// kind decides, never the parent server_agent application's stored flag.
//
// The FIRST leg is the pin, and it is the only direction that can fail. Its
// parent is a server_agent application whose own flag is false -- the only
// value that kind can hold, and what seedServerAgentApplication already
// writes -- under a capable llama_cpp spec, so an implementation seeding from
// app.ResponsesLiveTimingsEnabled (app is a putRuntimeSpec parameter, right
// there to be misused) answers false where the kind default is true.
//
// The second leg is a COHERENCE leg, not a pin, and must not be read as one:
// on an incapable kind the resolution forces false whatever the seed was, so
// false there is satisfied by the kind default, by the clear of a stored
// value, and by a complete inheritance defect alike. Its true seed has to
// bypass the service because CreateApplication/UpdateApplication refuse to
// store true on a server_agent application at all -- that refusal is itself
// the invariant this feature establishes, not a gap in this test -- so the
// fixture state is written straight through routing.MemoryStore.
func TestPutRuntimeSpecResponsesLiveTimingsDoesNotInheritTheParentApplication(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, app, newMapping := liveTimingsSpecFixture(t)
	if app.ResponsesLiveTimingsEnabled {
		t.Fatalf("precondition: the seeded server_agent application's flag is on, want off")
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("pin"), PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = false, want true: a llama_cpp spec took its parent application's false instead of its own kind default")
	}

	seeded := app
	seeded.ResponsesLiveTimingsEnabled = true
	if err := routeStore.UpdateApplication(ctx, seeded); err != nil {
		t.Fatalf("seed the parent application's flag directly: %v", err)
	}
	dto, err = svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("coherence"), PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/ollama", Type: string(routing.RuntimeSpecTypeOllama),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = true on an ollama spec, want false")
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsPreservesAnExistingValueWhenAbsent
// pins the later-save half of the four-way resolution, in both directions: on
// a full-document PUT that omits the key, an EXISTING spec keeps what the
// operator last stored. A kind-dependent default firing on every save would
// re-enable a flag they turned off; an over-eager clear would drop one they
// turned on.
func TestPutRuntimeSpecResponsesLiveTimingsPreservesAnExistingValueWhenAbsent(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)
	mappingID := newMapping("m")
	capable := PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp)}

	off := capable
	off.ResponsesLiveTimingsEnabled = boolPtr(false)
	if dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, off); err != nil {
		t.Fatalf("PutRuntimeSpec(false): %v", err)
	} else if dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("an explicit false was not stored")
	}
	if dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, capable); err != nil {
		t.Fatalf("PutRuntimeSpec(absent after false): %v", err)
	} else if dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = true, want false: a later save with the key absent re-enabled a flag the operator turned off")
	}

	on := capable
	on.ResponsesLiveTimingsEnabled = boolPtr(true)
	if dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, on); err != nil {
		t.Fatalf("PutRuntimeSpec(true): %v", err)
	} else if !dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("an explicit true was not stored")
	}
	if dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, capable); err != nil {
		t.Fatalf("PutRuntimeSpec(absent after true): %v", err)
	} else if !dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = false, want true: a later save with the key absent cleared a flag the operator turned on")
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsAbsentIsNotFalseOnAFirstWrite is the
// pointer's whole purpose on the spec side: with a plain bool on
// PutRuntimeSpecRequest the two calls below would be the SAME request. The
// absent-key leg would arrive as false, the kind-dependent default could
// never fire, and the preserve branch would have nothing to distinguish
// either.
//
// This test cannot measure that by swapping the field's type -- boolPtr(...)
// into a bool field is a compile failure in this very file, and editing a
// test to measure a mutation is not a measurement. The behavioural
// equivalent -- replacing the whole resolution below with
// "absent means false" -- is measured as a production-only edit with this
// change instead.
func TestPutRuntimeSpecResponsesLiveTimingsAbsentIsNotFalseOnAFirstWrite(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)
	capable := PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp)}

	absent, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("absent"), capable)
	if err != nil {
		t.Fatalf("PutRuntimeSpec(absent): %v", err)
	}
	if !absent.ResponsesLiveTimingsEnabled {
		t.Fatalf("absent: responses_live_timings_enabled = false, want true (the llama_cpp create default)")
	}

	explicitFalse := capable
	explicitFalse.ResponsesLiveTimingsEnabled = boolPtr(false)
	off, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("explicit-false"), explicitFalse)
	if err != nil {
		t.Fatalf("PutRuntimeSpec(false): %v", err)
	}
	if off.ResponsesLiveTimingsEnabled {
		t.Fatalf("explicit false: responses_live_timings_enabled = true, want false (an explicit off is a deliberate off, on any kind)")
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsHonoursAnExplicitValue: a non-nil
// value is the caller's, stored verbatim and echoed by the returned DTO, and
// a reload reports the same thing.
//
// The third leg is the ACCEPTING direction of the effective-kind resolution,
// and the only test here that catches a refusal written against the raw
// req.Type: routing.LiveTimingsCapableKind("") is false, so such a refusal
// would reject an explicit true on a {"binary": ".../llama-server"} spec,
// while every explicitly-typed leg above stayed green.
func TestPutRuntimeSpecResponsesLiveTimingsHonoursAnExplicitValue(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)
	mappingID := newMapping("typed")
	typed := PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp)}

	for _, want := range []bool{true, false} {
		req := typed
		req.ResponsesLiveTimingsEnabled = boolPtr(want)
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req)
		if err != nil {
			t.Fatalf("PutRuntimeSpec(%v): %v", want, err)
		}
		if dto.ResponsesLiveTimingsEnabled != want {
			t.Fatalf("echoed responses_live_timings_enabled = %v, want %v", dto.ResponsesLiveTimingsEnabled, want)
		}
		reloaded, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec: %v", err)
		}
		if reloaded.ResponsesLiveTimingsEnabled != want {
			t.Fatalf("stored responses_live_timings_enabled = %v, want %v", reloaded.ResponsesLiveTimingsEnabled, want)
		}
	}

	autoDetected, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("auto"), PutRuntimeSpecRequest{
		Binary: "/usr/bin/llama-server", ResponsesLiveTimingsEnabled: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("an explicit true on a type-less llama-server spec was refused: %v", err)
	}
	if !autoDetected.ResponsesLiveTimingsEnabled {
		t.Fatalf("auto-detect: responses_live_timings_enabled = false, want true")
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsRejectsTrueOnAnIncapableKind pins
// decision (e) on this surface: an EXPLICIT true on a spec whose effective
// kind cannot honour it is REFUSED, naming that kind, with nothing written --
// not quietly stored as false, because a 200 that stores something other than
// what it was asked to store is a write that lies about its result.
//
// The two legs are jointly the evidence for this surface having ONE sentinel
// where the application surface has two. The first refuses against a stored
// row, the second against no row at all, and both are the same 400: in both,
// the caller supplied the offending kind in this very body -- by typing it,
// or by naming the binary it is detected from. There is no spec shape where
// the refused kind comes from state the request left alone, which is the one
// shape the application surface answers 409 on.
//
// t.Errorf on the error-shape legs, with an err == nil guard before every
// err.Error(): each leg reports two independent facts about one call, and the
// "nothing was written" fact is the one that needs reporting in exactly the
// run where the refusal is missing -- the silent-normalisation mutation this
// feature replaced, measured with this change. With t.Fatalf that run would
// abort first, and an unguarded err.Error() on a nil err would panic rather
// than fail.
func TestPutRuntimeSpecResponsesLiveTimingsRejectsTrueOnAnIncapableKind(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	// Leg 1: a stored llama_cpp spec with the flag on, retyped to ollama in
	// the same body that asserts the flag. The seed's Type is EXPLICIT rather
	// than auto-detected because the assertion below reads the DTO's raw
	// stored Type -- runtimeSpecDTO echoes spec.Type and reports the resolved
	// kind separately as EffectiveType -- so a type-less seed would store ""
	// and fail for a reason with nothing to do with this feature.
	stored := newMapping("stored")
	seed, err := svc.PutRuntimeSpec(ctx, ownerToken(), stored, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp),
		ResponsesLiveTimingsEnabled: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("seed PutRuntimeSpec: %v", err)
	}
	if !seed.ResponsesLiveTimingsEnabled {
		t.Fatalf("precondition: the seeded llama_cpp spec has the flag off")
	}
	_, err = svc.PutRuntimeSpec(ctx, ownerToken(), stored, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/ollama", Type: string(routing.RuntimeSpecTypeOllama),
		ResponsesLiveTimingsEnabled: boolPtr(true),
	})
	if !errors.Is(err, ErrRuntimeSpecResponsesLiveTimingsUnsupported) {
		t.Errorf("err = %v, want ErrRuntimeSpecResponsesLiveTimingsUnsupported", err)
	}
	if err == nil || !strings.Contains(err.Error(), string(routing.RuntimeSpecTypeOllama)) {
		t.Errorf("err = %v, want the offending effective kind %q named in the message", err, routing.RuntimeSpecTypeOllama)
	}
	reloaded, err := svc.GetRuntimeSpec(ctx, ownerToken(), stored)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if reloaded.Type != string(routing.RuntimeSpecTypeLlamaCpp) {
		t.Errorf("a refused PUT wrote the type anyway: %q, want %q", reloaded.Type, routing.RuntimeSpecTypeLlamaCpp)
	}
	if !reloaded.ResponsesLiveTimingsEnabled {
		t.Errorf("a refused PUT overwrote the stored flag")
	}

	// Leg 2: a FIRST write, with no type at all. The message must still name
	// ollama -- the kind the caller never typed -- precisely because an empty
	// type means the binary decided.
	fresh := newMapping("fresh")
	_, err = svc.PutRuntimeSpec(ctx, ownerToken(), fresh, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/ollama", ResponsesLiveTimingsEnabled: boolPtr(true),
	})
	if !errors.Is(err, ErrRuntimeSpecResponsesLiveTimingsUnsupported) {
		t.Errorf("err = %v, want ErrRuntimeSpecResponsesLiveTimingsUnsupported", err)
	}
	if err == nil || !strings.Contains(err.Error(), string(routing.RuntimeSpecTypeOllama)) {
		t.Errorf("err = %v, want the DETECTED kind %q named in the message", err, routing.RuntimeSpecTypeOllama)
	}
	unconfigured, err := svc.GetRuntimeSpec(ctx, ownerToken(), fresh)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if unconfigured.Configured {
		t.Errorf("a refused first write created a spec row: %#v", unconfigured)
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsClearsAStoredTrueWhenTheDocumentsKindCannotHonourIt
// is the other half of decision (e), and the asymmetry is the point: an
// ASSERTION against an incapable kind is refused, a NON-MENTION is
// normalised. A PUT that retypes a llama_cpp spec to ollama without
// mentioning the flag cannot leave a stale true behind -- and clearing it
// contradicts nothing the caller asked for, because they asked for nothing.
//
// The precondition assertion is what makes this test mean anything: without
// it, false at the end would also be what a spec that never had the flag on
// reports.
func TestPutRuntimeSpecResponsesLiveTimingsClearsAStoredTrueWhenTheDocumentsKindCannotHonourIt(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)
	mappingID := newMapping("m")

	seed, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server", Type: string(routing.RuntimeSpecTypeLlamaCpp),
		ResponsesLiveTimingsEnabled: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("seed PutRuntimeSpec: %v", err)
	}
	if !seed.ResponsesLiveTimingsEnabled {
		t.Fatalf("precondition: the seeded llama_cpp spec has the flag off, so this test could not observe a change")
	}

	retyped, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/ollama", Type: string(routing.RuntimeSpecTypeOllama),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec(absent on an incapable kind): %v", err)
	}
	if retyped.ResponsesLiveTimingsEnabled {
		t.Fatalf("echoed responses_live_timings_enabled = true, want false: a stored true survived a retype to ollama")
	}
	reloaded, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if reloaded.ResponsesLiveTimingsEnabled {
		t.Fatalf("stored responses_live_timings_enabled = true, want false")
	}
}

// TestRuntimeSpecDTOCarriesResponsesLiveTimings reads a spec written OUTSIDE
// the service -- straight into the routes store, with no putRuntimeSpec in
// the path -- so it still speaks if the whole value resolution is gone. That
// is its only exclusive claim: it is NOT the only guard on the hand-written
// runtimeSpecDTO mapper, because putRuntimeSpec returns runtimeSpecDTO too,
// so every test above reads that mapper as well (deleting the mapper's line
// fails all eight, not only this one).
func TestRuntimeSpecDTOCarriesResponsesLiveTimings(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, _, newMapping := liveTimingsSpecFixture(t)
	mappingID := newMapping("m")

	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID:        "rspec_live_timings",
		MappingID: mappingID,
		Binary:    "/usr/local/bin/llama-server",
		Type:      string(routing.RuntimeSpecTypeLlamaCpp),
		// Args/Env are stored JSON TEXT, and runtimeSpecDTO parses both: a
		// zero-value "" is not valid JSON and the mapper answers
		// ErrRuntimeSpecArgsInvalid before it ever reaches the field under
		// test. Every spec written through the service carries "[]"/"{}"
		// here; a hand-written store seed has to say so itself.
		Args:                        "[]",
		Env:                         "{}",
		ResponsesLiveTimingsEnabled: true,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if !dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = false, want true: the DTO mapper drops the stored value")
	}
}

// TestPutRuntimeSpecResponsesLiveTimingsRefusalRunsAfterThePreExistingValidations
// pins the POSITION of the live-timings refusal, and is the only test that
// can: every other test in this file passes wherever in the
// validate-before-mutate block the refusal sits.
//
// Each leg is a body that is invalid for a SHIPPED reason and also carries an
// impossible true, on an ollama binary and an explicit ollama type so the
// refusal really is armed. A refusal placed beside the type check would
// answer runtime_spec.responses_live_timings_unsupported to all three, where
// they used to answer runtime_spec.metrics_path_invalid,
// runtime_spec.endpoint_mode_invalid and runtime_spec.flavor_invalid -- a
// behaviour change to three shipped codes dressed up as an addition. Modelled
// on TestCreateApplicationLiveTimingsRefusalRunsAfterThePreExistingValidations,
// which exists because the application surface shipped that defect once.
//
// Both halves of each leg are t.Errorf: one leg reports both facts about one
// call -- that the shipped sentinel came back, and that the new one did not.
func TestPutRuntimeSpecResponsesLiveTimingsRefusalRunsAfterThePreExistingValidations(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	legs := []struct {
		name string
		req  PutRuntimeSpecRequest
		want error
	}{
		{name: "metrics-path", req: PutRuntimeSpecRequest{MetricsPath: "//evil"}, want: ErrRuntimeSpecMetricsPathInvalid},
		{name: "endpoint-mode", req: PutRuntimeSpecRequest{ResponsesMode: "bogus"}, want: ErrRuntimeSpecEndpointModeInvalid},
		{name: "flavor", req: PutRuntimeSpecRequest{APIFlavors: []string{"nope"}}, want: ErrRuntimeSpecFlavorInvalid},
	}
	for _, leg := range legs {
		req := leg.req
		req.Binary = "/usr/local/bin/ollama"
		req.Type = string(routing.RuntimeSpecTypeOllama)
		req.ResponsesLiveTimingsEnabled = boolPtr(true)

		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping(leg.name), req)
		if !errors.Is(err, leg.want) {
			t.Errorf("%s: err = %v, want %v: the live-timings refusal must not pre-empt a shipped validation", leg.name, err, leg.want)
		}
		if errors.Is(err, ErrRuntimeSpecResponsesLiveTimingsUnsupported) {
			t.Errorf("%s: a body invalid for a PRE-EXISTING reason reported the brand-new live-timings refusal instead", leg.name)
		}
	}
}

// TestAgentRuntimeSpecDTOCarriesConfigFiles pins that the agent-wire DTO
// carries the stored config_files (#257): the opaque JSON column unmarshals
// onto AgentRuntimeSpecDTO.ConfigFiles verbatim (placeholders such as ${MODEL}
// are the agent's to resolve, never rewritten here), an empty stored value --
// every row written before the column existed -- yields a non-nil empty slice
// so the wire carries "config_files":[] and never null, and a corrupt stored
// value surfaces as the domain sentinel rather than a raw JSON error.
func TestAgentRuntimeSpecDTOCarriesConfigFiles(t *testing.T) {
	mapping := routing.ModelMapping{GatewayModelName: "qwen", AppModelName: "qwen-upstream"}
	build := func(stored string) (AgentRuntimeSpecDTO, error) {
		return agentRuntimeSpecDTO(routing.RuntimeSpec{
			ID:          "rspec_x",
			Binary:      "/usr/bin/x",
			Args:        "[]",
			Env:         "{}",
			ConfigFiles: stored,
		}, mapping, nil, 0)
	}

	t.Run("stored files appear on the DTO", func(t *testing.T) {
		dto, err := build(`[{"name":"server.json","content":"id ${MODEL}"},{"name":"b.yaml","content":"x: 1"}]`)
		if err != nil {
			t.Fatalf("agentRuntimeSpecDTO: %v", err)
		}
		want := []ConfigFileDTO{
			{Name: "server.json", Content: "id ${MODEL}"},
			{Name: "b.yaml", Content: "x: 1"},
		}
		if !slices.Equal(dto.ConfigFiles, want) {
			t.Fatalf("ConfigFiles = %#v, want %#v", dto.ConfigFiles, want)
		}
	})

	t.Run("empty stored value yields a non-nil empty slice", func(t *testing.T) {
		dto, err := build("")
		if err != nil {
			t.Fatalf("agentRuntimeSpecDTO: %v", err)
		}
		if dto.ConfigFiles == nil || len(dto.ConfigFiles) != 0 {
			t.Fatalf("ConfigFiles = %#v, want a non-nil empty slice", dto.ConfigFiles)
		}
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal dto: %v", err)
		}
		if !strings.Contains(string(raw), `"config_files":[]`) {
			t.Fatalf("wire body = %s, want an explicit \"config_files\":[]", raw)
		}
	})

	t.Run("literal null stored value yields a non-nil empty slice", func(t *testing.T) {
		dto, err := build("null")
		if err != nil {
			t.Fatalf("agentRuntimeSpecDTO: %v", err)
		}
		if dto.ConfigFiles == nil || len(dto.ConfigFiles) != 0 {
			t.Fatalf("ConfigFiles = %#v, want a non-nil empty slice", dto.ConfigFiles)
		}
	})

	t.Run("corrupt stored value is the domain sentinel", func(t *testing.T) {
		if _, err := build("{not json"); !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
			t.Fatalf("err = %v, want ErrRuntimeSpecConfigFilesInvalid", err)
		}
	})

	t.Run("reaches the runtime-config document the agent fetches", func(t *testing.T) {
		now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		ctx := context.Background()
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app := seedServerAgentApplication(t, routeStore, server.ID, now)
		m, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen-upstream"})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), m.ID, PutRuntimeSpecRequest{
			Enabled:     true,
			Binary:      "/usr/local/bin/llama-server",
			ConfigFiles: []ConfigFileDTO{{Name: "server.json", Content: "id ${MODEL}"}},
		}); err != nil {
			t.Fatalf("PutRuntimeSpec: %v", err)
		}
		cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
		if err != nil {
			t.Fatalf("AgentRuntimeConfig: %v", err)
		}
		if len(cfg.Specs) != 1 {
			t.Fatalf("specs = %#v, want 1", cfg.Specs)
		}
		want := []ConfigFileDTO{{Name: "server.json", Content: "id ${MODEL}"}}
		if !slices.Equal(cfg.Specs[0].ConfigFiles, want) {
			t.Fatalf("agent wire ConfigFiles = %#v, want %#v", cfg.Specs[0].ConfigFiles, want)
		}
	})
}

// --- model-instance settings (#340) ------------------------------------------

// instanceSpecFixture seeds a server with a server_agent application and one
// mapping on it, and returns the service, the store, the mapping id and the
// server id. With svc and routeStore nil it builds the default test service;
// otherwise routeStore is the store behind svc.
func instanceSpecFixture(t *testing.T, svc *Service, routeStore *routing.MemoryStore) (*Service, *routing.MemoryStore, string, string) {
	t.Helper()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if svc == nil {
		svc, routeStore = newServerTestService(t, now)
	}
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "tts", AppModelName: "tts-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// The server's agent runs instances, so the pushed document carries them;
	// TestAgentRuntimeDocInstanceFieldsOnlyForAnAgentThatRunsInstances covers
	// the agents that do not.
	svc.agentFeatures = fakeAgentFeatures{server.ID: {"runtime_manager", AgentFeatureRuntimeInstances}}
	return svc, routeStore, mapping.ID, server.ID
}

// fakeAgentFeatures is an AgentFeatureReader over a fixed declared set per
// server id. A server with an entry -- an empty one included -- has declared
// its set; one without has not been heard from.
type fakeAgentFeatures map[string][]string

func (f fakeAgentFeatures) Has(serverID, feature string) bool {
	return slices.Contains(f[serverID], feature)
}

func (f fakeAgentFeatures) Known(serverID string) bool {
	_, ok := f[serverID]
	return ok
}

// newInstanceIdleTestService is newServerTestService with the two idle-timeout
// knobs set through ServiceDeps, as cmd/gateway sets them from the config.
func newInstanceIdleTestService(t *testing.T, factor float64, floor int) (*Service, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(context.Background(), store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		InstanceIdleFactor: factor, InstanceIdleFloorSeconds: floor,
	})
	return svc, routeStore
}

// wireKeys returns v's JSON object as raw values by key.
func wireKeys(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return keys
}

// TestPutRuntimeSpecInstancesRoundTrip pins the four operator-owned instance
// settings through the full-document PUT and the GET, by their JSON names,
// and the two read-only echoes beside them: the agent's measured startup and
// the idle timeout the runtime-config document pushes for the spec. A PUT
// body carrying the read-only keys changes nothing -- the request has no such
// fields -- and a re-save keeps the agent's measurement while the settings
// follow the document, cleared when it omits them like any other field.
func TestPutRuntimeSpecInstancesRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)

	empty, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec (unconfigured): %v", err)
	}
	emptyKeys := wireKeys(t, empty)
	for key, want := range map[string]string{
		"replicable": "false", "min_instances": "0", "max_instances": "0", "instance_parallelism": "0",
		"startup_measured_ms": "0", "effective_idle_timeout_seconds": "0",
	} {
		if got := string(emptyKeys[key]); got != want {
			t.Fatalf("unconfigured wire %s = %q, want %s (the keys are always present)", key, got, want)
		}
	}

	var req PutRuntimeSpecRequest
	body := `{"enabled":true,"binary":"/usr/local/bin/llama-server","replicable":true,"min_instances":1,"max_instances":3,` +
		`"instance_parallelism":2,"startup_measured_ms":99999,"effective_idle_timeout_seconds":7}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req)
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !put.Replicable || put.MinInstances != 1 || put.MaxInstances != 3 || put.InstanceParallelism != 2 {
		t.Fatalf("PUT echo = replicable %v min %d max %d parallelism %d, want true 1 3 2",
			put.Replicable, put.MinInstances, put.MaxInstances, put.InstanceParallelism)
	}
	if put.StartupMeasuredMS != 0 {
		t.Fatalf("PUT echo startup_measured_ms = %d, want 0: nothing is measured yet and a request never sets it", put.StartupMeasuredMS)
	}
	// Nothing measured, no manual value: the floor, never 0 ("never unload").
	if put.EffectiveIdleTimeoutSeconds != DefaultInstanceIdleFloorSeconds {
		t.Fatalf("PUT echo effective_idle_timeout_seconds = %d, want the floor %d", put.EffectiveIdleTimeoutSeconds, DefaultInstanceIdleFloorSeconds)
	}
	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping: ok=%v err=%v", ok, err)
	}
	if !stored.Replicable || stored.MinInstances != 1 || stored.MaxInstances != 3 || stored.InstanceParallelism != 2 ||
		stored.StartupMeasuredMS != 0 || stored.IdleTimeoutSeconds != 0 {
		t.Fatalf("stored spec = %+v, want the four settings, no measurement and the manual idle timeout 0 (the effective value is never stored)", stored)
	}

	if err := routeStore.UpdateRuntimeSpecStartupMeasured(ctx, put.ID, 30000); err != nil {
		t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	keys := wireKeys(t, got)
	for key, want := range map[string]string{
		"replicable": "true", "min_instances": "1", "max_instances": "3", "instance_parallelism": "2",
		"startup_measured_ms": "30000", "effective_idle_timeout_seconds": "150",
	} {
		if gotValue := string(keys[key]); gotValue != want {
			t.Fatalf("GET wire %s = %q, want %s", key, gotValue, want)
		}
	}

	again, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, MinInstances: 2, MaxInstances: 4, InstanceParallelism: 1,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (re-save): %v", err)
	}
	if again.MinInstances != 2 || again.MaxInstances != 4 || again.InstanceParallelism != 1 ||
		again.StartupMeasuredMS != 30000 || again.EffectiveIdleTimeoutSeconds != 150 {
		t.Fatalf("re-save echo = %+v, want the new settings, the kept measurement 30000 and its idle timeout 150", again)
	}

	cleared, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (omitting the settings): %v", err)
	}
	if cleared.Replicable || cleared.MinInstances != 0 || cleared.MaxInstances != 0 || cleared.InstanceParallelism != 0 ||
		cleared.StartupMeasuredMS != 30000 || cleared.EffectiveIdleTimeoutSeconds != 0 {
		t.Fatalf("a PUT omitting the settings = %+v, want them cleared, the measurement kept, and the stored idle timeout 0 (never unload) for a spec that is not replicable", cleared)
	}
}

// TestPutRuntimeSpecInstancesInvalid pins the two refusals of the instance
// counts and that nothing is stored for either: a floor above the cap, when
// both are set, is runtime_spec.instances_invalid -- whether or not the spec is
// replicable, so an inconsistent pair is never stored to come back to life when
// it is switched on -- and a negative count is the existing
// runtime_spec.tuning_invalid. A floor equal to the cap, or either one unset
// (0), is a valid pair.
func TestPutRuntimeSpecInstancesInvalid(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)
	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true}
	}

	refused := []struct {
		name   string
		mutate func(*PutRuntimeSpecRequest)
		want   error
	}{
		{"min above max", func(r *PutRuntimeSpecRequest) { r.MinInstances, r.MaxInstances = 3, 2 }, ErrRuntimeSpecInstancesInvalid},
		{"min above max on a spec that is not replicable", func(r *PutRuntimeSpecRequest) {
			r.Replicable, r.MinInstances, r.MaxInstances = false, 3, 2
		}, ErrRuntimeSpecInstancesInvalid},
		{"negative min", func(r *PutRuntimeSpecRequest) { r.MinInstances = -1 }, ErrRuntimeSpecTuningInvalid},
		{"negative min below a max", func(r *PutRuntimeSpecRequest) { r.MinInstances, r.MaxInstances = -1, 2 }, ErrRuntimeSpecTuningInvalid},
		{"negative max", func(r *PutRuntimeSpecRequest) { r.MaxInstances = -1 }, ErrRuntimeSpecTuningInvalid},
		{"negative parallelism", func(r *PutRuntimeSpecRequest) { r.InstanceParallelism = -1 }, ErrRuntimeSpecTuningInvalid},
	}
	for _, tc := range refused {
		req := base()
		tc.mutate(&req)
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if _, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID); err != nil || ok {
			t.Fatalf("%s: a refused PUT stored a spec (ok=%v err=%v)", tc.name, ok, err)
		}
	}

	for _, pair := range []struct{ min, max int }{{2, 2}, {3, 0}, {0, 2}, {0, 0}} {
		req := base()
		req.MinInstances, req.MaxInstances = pair.min, pair.max
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req)
		if err != nil {
			t.Fatalf("min %d max %d: err = %v, want the pair accepted", pair.min, pair.max, err)
		}
		if dto.MinInstances != pair.min || dto.MaxInstances != pair.max {
			t.Fatalf("min %d max %d: stored %d / %d", pair.min, pair.max, dto.MinInstances, dto.MaxInstances)
		}
	}
}

// TestPutRuntimeSpecReplicableNeedsEphemeralPort pins
// runtime_spec.instances_need_ephemeral_port: every instance of a replicable
// spec is its own process and needs its own port, so a replicable spec must
// leave listen_port at 0 for the agent to pick one per instance. A fixed port
// on a spec that is not replicable stays what it is today, and the refusal
// stores nothing -- also over an existing spec, whose row stays as it was.
func TestPutRuntimeSpecReplicableNeedsEphemeralPort(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, ListenPort: 9000,
	}); !errors.Is(err, ErrRuntimeSpecInstancesNeedEphemeralPort) {
		t.Fatalf("replicable with listen_port 9000: err = %v, want ErrRuntimeSpecInstancesNeedEphemeralPort", err)
	}
	if _, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID); err != nil || ok {
		t.Fatalf("a refused PUT stored a spec (ok=%v err=%v)", ok, err)
	}

	fixed, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", ListenPort: 9000,
	})
	if err != nil || fixed.ListenPort != 9000 || fixed.Replicable {
		t.Fatalf("a fixed port on a spec that is not replicable: dto %+v err %v, want it stored as today", fixed, err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, ListenPort: 9000,
	}); !errors.Is(err, ErrRuntimeSpecInstancesNeedEphemeralPort) {
		t.Fatalf("switching a fixed-port spec to replicable: err = %v, want ErrRuntimeSpecInstancesNeedEphemeralPort", err)
	}
	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok || stored.Replicable || stored.ListenPort != 9000 {
		t.Fatalf("after a refused re-save the stored spec = %+v (ok=%v err=%v), want it unchanged", stored, ok, err)
	}

	ephemeral, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true,
	})
	if err != nil || !ephemeral.Replicable || ephemeral.ListenPort != 0 {
		t.Fatalf("replicable with listen_port 0: dto %+v err %v, want it stored", ephemeral, err)
	}
}

// TestPutRuntimeSpecInstanceChecksRunAfterThePreExistingValidations pins where
// the instance checks sit in the request validation, whose order is part of
// the API: after every validation that shipped before them. A body invalid for
// a shipped reason AND for an instance reason keeps reporting the shipped
// one -- before the instance settings existed, encoding/json dropped their
// keys and such a body got exactly that answer.
func TestPutRuntimeSpecInstanceChecksRunAfterThePreExistingValidations(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	legs := []struct {
		name   string
		mutate func(*PutRuntimeSpecRequest)
		want   error
	}{
		{"binary", func(r *PutRuntimeSpecRequest) { r.Binary = ""; r.MinInstances, r.MaxInstances = 3, 2 }, ErrRuntimeSpecBinaryRequired},
		{"admin-state", func(r *PutRuntimeSpecRequest) { r.AdminState = "bogus"; r.MinInstances = -1 }, ErrRuntimeSpecAdminStateInvalid},
		{"gpus", func(r *PutRuntimeSpecRequest) {
			r.GPUs = []RuntimeSpecGPUDTO{{Index: -1}}
			r.Replicable, r.ListenPort = true, 9000
		}, ErrRuntimeSpecGPUInvalid},
		{"endpoint-mode", func(r *PutRuntimeSpecRequest) { r.ResponsesMode = "bogus"; r.MinInstances, r.MaxInstances = 3, 2 }, ErrRuntimeSpecEndpointModeInvalid},
		{"flavor", func(r *PutRuntimeSpecRequest) {
			r.APIFlavors = []string{"nope"}
			r.Replicable, r.ListenPort = true, 9000
		}, ErrRuntimeSpecFlavorInvalid},
		{"live-timings", func(r *PutRuntimeSpecRequest) {
			r.Binary, r.Type, r.ResponsesLiveTimingsEnabled = "/usr/local/bin/ollama", string(routing.RuntimeSpecTypeOllama), boolPtr(true)
			r.InstanceParallelism = -1
		}, ErrRuntimeSpecResponsesLiveTimingsUnsupported},
		{"config-files", func(r *PutRuntimeSpecRequest) {
			r.ConfigFiles = []ConfigFileDTO{{Name: "a/b.json", Content: "{}"}}
			r.Replicable, r.ListenPort = true, 9000
		}, ErrRuntimeSpecConfigFilesInvalid},
	}
	for _, leg := range legs {
		req := PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"}
		leg.mutate(&req)
		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping(leg.name), req)
		if !errors.Is(err, leg.want) {
			t.Errorf("%s: err = %v, want %v: an instance check must not pre-empt a shipped validation", leg.name, err, leg.want)
		}
	}
}

// TestAgentRuntimeDocCarriesInstanceFields pins the pushed document's side:
// a replicable spec carries replicable, min_instances and max_instances, and
// the effective idle timeout as idle_timeout_seconds; a spec that is not
// replicable carries none of the three keys -- even with counts stored, which
// mean nothing without replicable -- and its stored idle timeout as ever, so
// its document is byte-for-byte what it was before instances existed. The
// gateway-only settings never reach the agent: instance_parallelism (the
// gateway's own per-instance cap) and the portal's two read-only echoes.
func TestAgentRuntimeDocCarriesInstanceFields(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, replicableMapping, serverID := instanceSpecFixture(t, nil, nil)
	apps, err := routeStore.ApplicationsByServer(ctx, serverID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("ApplicationsByServer: %d apps, err %v", len(apps), err)
	}
	plain, err := svc.CreateMapping(ctx, ownerToken(), apps[0].ID, CreateMappingRequest{GatewayModelName: "chat", AppModelName: "chat-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	replicable, err := svc.PutRuntimeSpec(ctx, ownerToken(), replicableMapping, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, MinInstances: 1, MaxInstances: 3, InstanceParallelism: 2,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (replicable): %v", err)
	}
	if err := routeStore.UpdateRuntimeSpecStartupMeasured(ctx, replicable.ID, 30000); err != nil {
		t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), plain.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", IdleTimeoutSeconds: 900, MinInstances: 1, MaxInstances: 2, InstanceParallelism: 4,
	}); err != nil {
		t.Fatalf("PutRuntimeSpec (not replicable): %v", err)
	}

	doc, err := svc.AgentRuntimeConfig(ctx, serverID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(doc.Specs) != 2 {
		t.Fatalf("document specs = %d, want 2", len(doc.Specs))
	}
	byModel := map[string]map[string]json.RawMessage{}
	for _, spec := range doc.Specs {
		byModel[spec.Model] = wireKeys(t, spec)
	}

	rep := byModel["tts"]
	for key, want := range map[string]string{"replicable": "true", "min_instances": "1", "max_instances": "3", "idle_timeout_seconds": "150"} {
		if got := string(rep[key]); got != want {
			t.Fatalf("replicable spec wire %s = %q, want %s", key, got, want)
		}
	}
	notPushed := []string{"instance_parallelism", "startup_measured_ms", "effective_idle_timeout_seconds"}
	for _, key := range notPushed {
		if v, ok := rep[key]; ok {
			t.Fatalf("replicable spec wire carries %s = %s, which is never pushed", key, v)
		}
	}

	other := byModel["chat"]
	for _, key := range append([]string{"replicable", "min_instances", "max_instances"}, notPushed...) {
		if v, ok := other[key]; ok {
			t.Fatalf("a spec that is not replicable carries %s = %s; its document must have none of the instance keys", key, v)
		}
	}
	if got := string(other["idle_timeout_seconds"]); got != "900" {
		t.Fatalf("spec that is not replicable: idle_timeout_seconds = %s, want its stored 900 unchanged", got)
	}
}

// TestEffectiveIdleTimeout pins the idle timeout pushed for a spec, here at the
// default factor 5 and floor 120: for a replicable spec the largest of its
// manual value (0 = no manual override), factor times the slower of the two
// startup figures in whole seconds rounded up -- the agent's measured startup
// and the benchmark's cold start -- and the floor, so it is never 0; for a
// spec that is not replicable the stored value as it is, 0 still meaning
// "never unload".
func TestEffectiveIdleTimeout(t *testing.T) {
	cases := []struct {
		name   string
		spec   routing.RuntimeSpec
		loadMS int64
		want   int
	}{
		{"no manual value, measured startup", routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: 30000}, 0, 150},
		{"a manual value above the derived one wins", routing.RuntimeSpec{Replicable: true, IdleTimeoutSeconds: 600, StartupMeasuredMS: 30000}, 0, 600},
		{"a short startup is lifted to the floor", routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: 10000}, 0, 120},
		{"the slower startup figure counts", routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: 20000}, 40000, 200},
		{"the benchmark's cold start alone", routing.RuntimeSpec{Replicable: true}, 40000, 200},
		{"a manual value below the derived one is raised", routing.RuntimeSpec{Replicable: true, IdleTimeoutSeconds: 60, StartupMeasuredMS: 30000}, 0, 150},
		{"a part of a second rounds up", routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: 30001}, 0, 151},
		{"nothing measured is the floor, never 0", routing.RuntimeSpec{Replicable: true}, 0, 120},
		{"not replicable, 0 still means never", routing.RuntimeSpec{StartupMeasuredMS: 30000}, 40000, 0},
		{"not replicable, the stored value as it is", routing.RuntimeSpec{IdleTimeoutSeconds: 45, StartupMeasuredMS: 30000}, 0, 45},
	}
	for _, tc := range cases {
		if got := effectiveIdleTimeoutSeconds(tc.spec, tc.loadMS, 5, 120); got != tc.want {
			t.Errorf("%s: effectiveIdleTimeoutSeconds(%+v, load %d) = %d, want %d", tc.name, tc.spec, tc.loadMS, got, tc.want)
		}
	}
	if got := effectiveIdleTimeoutSeconds(routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: 20000}, 0, 2.5, 30); got != 50 {
		t.Errorf("factor 2.5, floor 30, startup 20 s: got %d, want 50", got)
	}
	// An absurd figure saturates instead of wrapping: an overflowed conversion
	// could come out negative or small, and a pushed timeout too large for the
	// agent's duration arithmetic wraps there, both an instance unloaded at once.
	if got := effectiveIdleTimeoutSeconds(routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: math.MaxInt64}, 0, 1e300, 120); got != math.MaxInt32 {
		t.Errorf("an absurd startup and factor: got %d, want the saturated %d", got, math.MaxInt32)
	}
}

// TestEffectiveIdleTimeoutIsWhatTheDocumentPushes pins that the portal's
// read-only echo and the pushed document carry one value, derived under the
// factor and floor the service was configured with (ServiceDeps, from
// OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR / _FLOOR_SECONDS), from the spec's measured
// startup and the mapping's benchmark cold start -- and that deriving it never
// rewrites the stored manual value.
func TestEffectiveIdleTimeoutIsWhatTheDocumentPushes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	baseSvc, baseStore := newInstanceIdleTestService(t, 2, 30)
	svc, routeStore, mappingID, serverID := instanceSpecFixture(t, baseSvc, baseStore)

	if err := routeStore.UpdateMappingBenchmarkMetrics(ctx, mappingID, 0, 0, 40000, now); err != nil {
		t.Fatalf("UpdateMappingBenchmarkMetrics: %v", err)
	}
	spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if err := routeStore.UpdateRuntimeSpecStartupMeasured(ctx, spec.ID, 20000); err != nil {
		t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
	}

	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	doc, err := svc.AgentRuntimeConfig(ctx, serverID)
	if err != nil || len(doc.Specs) != 1 {
		t.Fatalf("AgentRuntimeConfig: %d specs, err %v", len(doc.Specs), err)
	}
	// factor 2 x the slower startup (the 40 s cold start) = 80 s, above the floor 30.
	if got.EffectiveIdleTimeoutSeconds != 80 || doc.Specs[0].IdleTimeoutSeconds != 80 {
		t.Fatalf("effective idle timeout: portal %d, pushed %d, want both 80", got.EffectiveIdleTimeoutSeconds, doc.Specs[0].IdleTimeoutSeconds)
	}
	stored, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok || stored.IdleTimeoutSeconds != 0 {
		t.Fatalf("stored idle timeout = %d (ok=%v err=%v), want the manual 0 untouched", stored.IdleTimeoutSeconds, ok, err)
	}
}

// TestNewServiceDefaultsTheInstanceIdleSettings pins NewService's fallback for
// the two idle knobs, the deps-literal rule the other config-derived deps
// follow: a value that is not positive -- the zero value of a deps literal that
// omits it included -- or a factor that is not a finite number uses the
// default, DefaultInstanceIdleFactor and DefaultInstanceIdleFloorSeconds.
func TestNewServiceDefaultsTheInstanceIdleSettings(t *testing.T) {
	if DefaultInstanceIdleFactor != 5 || DefaultInstanceIdleFloorSeconds != 120 {
		t.Fatalf("defaults = %v / %d, want 5 / 120", DefaultInstanceIdleFactor, DefaultInstanceIdleFloorSeconds)
	}
	cases := []struct {
		name       string
		factor     float64
		floor      int
		wantFactor float64
		wantFloor  int
	}{
		{"omitted", 0, 0, 5, 120},
		{"configured", 2.5, 30, 2.5, 30},
		{"negative", -1, -5, 5, 120},
		{"factor not a number", math.NaN(), 30, 5, 30},
		{"factor infinite", math.Inf(1), 30, 5, 30},
	}
	for _, tc := range cases {
		svc := NewService(ServiceDeps{InstanceIdleFactor: tc.factor, InstanceIdleFloorSeconds: tc.floor})
		if svc.instanceIdleFactor != tc.wantFactor || svc.instanceIdleFloorSeconds != tc.wantFloor {
			t.Errorf("%s: factor %v floor %d, want %v / %d", tc.name, svc.instanceIdleFactor, svc.instanceIdleFloorSeconds, tc.wantFactor, tc.wantFloor)
		}
	}
}
