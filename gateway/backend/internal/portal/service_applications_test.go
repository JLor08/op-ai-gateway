// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"sync"
	"testing"
	"time"
)

func createTestServer(t *testing.T, svc *Service, name, domain string) ServerDTO {
	t.Helper()
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: name, Domain: domain, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	return dto
}

func TestCreateApplicationAdminOrOwnerHappyPath(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type:                         routing.ProviderVLLM,
		Port:                         8000,
		Scheme:                       "https",
		AdmissionQueueTimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("CreateApplication (owner): %v", err)
	}
	if dto.ID == "" || dto.ServerID != server.ID || dto.Type != routing.ProviderVLLM {
		t.Fatalf("dto = %#v", dto)
	}
	if dto.Port != 8000 || dto.Scheme != "https" {
		t.Fatalf("port/scheme = %d/%s", dto.Port, dto.Scheme)
	}
	if dto.Endpoint != "https://s.example.test:8000" {
		t.Fatalf("endpoint = %q, want derived endpoint", dto.Endpoint)
	}
	if len(dto.APIFlavors) != 2 {
		t.Fatalf("api flavors default = %#v, want openai + anthropic", dto.APIFlavors)
	}
	if dto.Status != routing.ServerStatusActive {
		t.Fatalf("status default = %q", dto.Status)
	}
	if dto.TimeoutMS != 30000 || dto.AffinityTTLSeconds != 1800 {
		t.Fatalf("tuning defaults = timeout=%d affinity=%d", dto.TimeoutMS, dto.AffinityTTLSeconds)
	}
	if dto.AdmissionQueueTimeoutSeconds != 45 {
		t.Fatalf("admission_queue_timeout_seconds = %d, want 45", dto.AdmissionQueueTimeoutSeconds)
	}
	if dto.Priority != 0 || dto.Weight != 0 {
		t.Fatalf("priority/weight defaults = %d/%d", dto.Priority, dto.Weight)
	}
	if dto.CreatedAt.IsZero() {
		t.Fatalf("created_at not set")
	}

	// admin can also create under someone else's server
	if _, err := svc.CreateApplication(context.Background(), systemAdminToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderOllama, Port: 8001, Scheme: "http",
	}); err != nil {
		t.Fatalf("CreateApplication (admin): %v", err)
	}
}

func TestCreateApplicationHealthFieldsDefaultsAndPersist(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Defaults: no health fields provided → always_reachable false, path "/v1/health".
	dflt, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication (defaults): %v", err)
	}
	if dflt.AlwaysReachable {
		t.Fatalf("always_reachable default = true, want false")
	}
	if dflt.HealthCheckPath != "/v1/health" {
		t.Fatalf("health_check_path default = %q, want /v1/health", dflt.HealthCheckPath)
	}

	// Explicit values persist through the DTO and a reload.
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		AlwaysReachable: true, HealthCheckPath: "/healthz",
	})
	if err != nil {
		t.Fatalf("CreateApplication (explicit): %v", err)
	}
	if !dto.AlwaysReachable || dto.HealthCheckPath != "/healthz" {
		t.Fatalf("health fields not persisted: %#v", dto)
	}
	got, err := svc.GetApplication(context.Background(), ownerToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if !got.AlwaysReachable || got.HealthCheckPath != "/healthz" {
		t.Fatalf("reloaded health fields = %#v", got)
	}
}

func TestCreateApplicationRejectsInvalidHealthPath(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", HealthCheckPath: "healthz",
	}); !errors.Is(err, ErrApplicationHealthPathInvalid) {
		t.Fatalf("err = %v, want ErrApplicationHealthPathInvalid", err)
	}
}

func TestUpdateApplicationHealthFields(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	always := true
	path := "/ready"
	updated, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
		AlwaysReachable: &always, HealthCheckPath: &path,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.AlwaysReachable || updated.HealthCheckPath != "/ready" {
		t.Fatalf("updated health fields = %#v", updated)
	}

	// An empty health path on update normalizes back to the default.
	empty := ""
	renorm, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
		HealthCheckPath: &empty,
	})
	if err != nil {
		t.Fatalf("update empty path: %v", err)
	}
	if renorm.HealthCheckPath != "/v1/health" {
		t.Fatalf("empty path did not normalize to default: %q", renorm.HealthCheckPath)
	}
	if !renorm.AlwaysReachable {
		t.Fatalf("always_reachable should be preserved across partial update: %#v", renorm)
	}

	// An invalid path on update is rejected and mutates nothing.
	bad := "nope"
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
		HealthCheckPath: &bad,
	}); !errors.Is(err, ErrApplicationHealthPathInvalid) {
		t.Fatalf("bad path update err = %v, want ErrApplicationHealthPathInvalid", err)
	}
	after, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetApplication after rejected update: %v", err)
	}
	if after.HealthCheckPath != "/v1/health" {
		t.Fatalf("rejected update mutated health path: %q", after.HealthCheckPath)
	}
}

func TestApplicationHealthCheckModeCreateUpdateAndValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// No mode + no always_reachable defaults to health_path on the wire.
	dflt, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if dflt.HealthCheckMode != routing.HealthCheckModeHealthPath {
		t.Fatalf("default health_check_mode = %q, want health_path", dflt.HealthCheckMode)
	}

	// Explicit model_sync persists and does NOT set always_reachable.
	ms, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "https", HealthCheckMode: routing.HealthCheckModeModelSync,
	})
	if err != nil {
		t.Fatalf("create model_sync: %v", err)
	}
	if ms.HealthCheckMode != routing.HealthCheckModeModelSync || ms.AlwaysReachable {
		t.Fatalf("model_sync dto = %#v, want mode=model_sync always_reachable=false", ms)
	}
	got, err := svc.GetApplication(context.Background(), ownerToken(), ms.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.HealthCheckMode != routing.HealthCheckModeModelSync {
		t.Fatalf("reloaded mode = %q, want model_sync", got.HealthCheckMode)
	}

	// An invalid mode on create is rejected.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8002, Scheme: "https", HealthCheckMode: "bogus",
	}); !errors.Is(err, ErrApplicationHealthModeInvalid) {
		t.Fatalf("create invalid mode err = %v, want ErrApplicationHealthModeInvalid", err)
	}

	// Switching an app to always_reachable via the mode keeps always_reachable
	// coherent; a subsequent invalid mode update changes nothing.
	always := routing.HealthCheckModeAlwaysReachable
	upd, err := svc.UpdateApplication(context.Background(), ownerToken(), ms.ID, UpdateApplicationRequest{
		HealthCheckMode: &always,
	})
	if err != nil {
		t.Fatalf("update to always: %v", err)
	}
	if upd.HealthCheckMode != routing.HealthCheckModeAlwaysReachable || !upd.AlwaysReachable {
		t.Fatalf("updated dto = %#v, want mode=always_reachable always_reachable=true", upd)
	}
	bogus := "nope"
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), ms.ID, UpdateApplicationRequest{
		HealthCheckMode: &bogus,
	}); !errors.Is(err, ErrApplicationHealthModeInvalid) {
		t.Fatalf("update invalid mode err = %v, want ErrApplicationHealthModeInvalid", err)
	}
	after, err := svc.GetApplication(context.Background(), ownerToken(), ms.ID)
	if err != nil {
		t.Fatalf("reload after rejected: %v", err)
	}
	if after.HealthCheckMode != routing.HealthCheckModeAlwaysReachable {
		t.Fatalf("rejected update mutated mode: %q", after.HealthCheckMode)
	}
}

func TestApplicationHealthCheckIntervalCreateUpdateAndValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Omitting the interval defaults to 0 (Default: follow the system-wide setting).
	dflt, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if dflt.HealthCheckIntervalSeconds != 0 {
		t.Fatalf("default health_check_interval_seconds = %d, want 0", dflt.HealthCheckIntervalSeconds)
	}

	// A custom value in range persists through the DTO and a reload.
	custom, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "https", HealthCheckIntervalSeconds: 45,
	})
	if err != nil {
		t.Fatalf("create custom: %v", err)
	}
	if custom.HealthCheckIntervalSeconds != 45 {
		t.Fatalf("custom health_check_interval_seconds = %d, want 45", custom.HealthCheckIntervalSeconds)
	}
	got, err := svc.GetApplication(context.Background(), ownerToken(), custom.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.HealthCheckIntervalSeconds != 45 {
		t.Fatalf("reloaded health_check_interval_seconds = %d, want 45", got.HealthCheckIntervalSeconds)
	}

	// Boundary values (min and max) are accepted on create.
	port := 8100
	for _, seconds := range []int{MinHealthCheckIntervalSeconds, MaxHealthCheckIntervalSeconds} {
		port++
		dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: port, Scheme: "https", HealthCheckIntervalSeconds: seconds,
		})
		if err != nil {
			t.Fatalf("create boundary %d: %v", seconds, err)
		}
		if dto.HealthCheckIntervalSeconds != seconds {
			t.Fatalf("boundary %d persisted = %d", seconds, dto.HealthCheckIntervalSeconds)
		}
	}

	// Out-of-range values are rejected on create.
	for _, seconds := range []int{MinHealthCheckIntervalSeconds - 1, MaxHealthCheckIntervalSeconds + 1, -1} {
		if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8200, Scheme: "https", HealthCheckIntervalSeconds: seconds,
		}); !errors.Is(err, ErrApplicationHealthIntervalInvalid) {
			t.Fatalf("create interval=%d err = %v, want ErrApplicationHealthIntervalInvalid", seconds, err)
		}
	}

	// Update accepts 0 and in-range values.
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8300, Scheme: "https", HealthCheckIntervalSeconds: 45,
	})
	if err != nil {
		t.Fatalf("create for update: %v", err)
	}
	for _, seconds := range []int{0, MinHealthCheckIntervalSeconds, MaxHealthCheckIntervalSeconds, 60} {
		s := seconds
		upd, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
			HealthCheckIntervalSeconds: &s,
		})
		if err != nil {
			t.Fatalf("update interval=%d: %v", seconds, err)
		}
		if upd.HealthCheckIntervalSeconds != seconds {
			t.Fatalf("updated interval = %d, want %d", upd.HealthCheckIntervalSeconds, seconds)
		}
	}

	// Update rejects out-of-range values and mutates nothing.
	set60 := 60
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{HealthCheckIntervalSeconds: &set60}); err != nil {
		t.Fatalf("set known interval: %v", err)
	}
	for _, seconds := range []int{MinHealthCheckIntervalSeconds - 1, MaxHealthCheckIntervalSeconds + 1, -1} {
		s := seconds
		if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{HealthCheckIntervalSeconds: &s}); !errors.Is(err, ErrApplicationHealthIntervalInvalid) {
			t.Fatalf("update interval=%d err = %v, want ErrApplicationHealthIntervalInvalid", seconds, err)
		}
	}
	after, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("reload after rejected update: %v", err)
	}
	if after.HealthCheckIntervalSeconds != 60 {
		t.Fatalf("rejected update mutated interval: %d, want 60", after.HealthCheckIntervalSeconds)
	}
}

func TestApplicationBenchmarkModesCreatePatchAndValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Omitting the fields defaults to the zero "feature off" values.
	dflt, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if dflt.BenchmarkScheduleEnabled || dflt.BenchmarkScheduleIntervalSeconds != 0 || dflt.OpportunisticMetricsEnabled {
		t.Fatalf("default benchmark modes = %v/%d/%v, want false/0/false",
			dflt.BenchmarkScheduleEnabled, dflt.BenchmarkScheduleIntervalSeconds, dflt.OpportunisticMetricsEnabled)
	}

	// Explicit values persist through the DTO and a reload.
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		BenchmarkScheduleEnabled: true, BenchmarkScheduleIntervalSeconds: 3600, OpportunisticMetricsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !app.BenchmarkScheduleEnabled || app.BenchmarkScheduleIntervalSeconds != 3600 || !app.OpportunisticMetricsEnabled {
		t.Fatalf("created benchmark modes = %v/%d/%v, want true/3600/true",
			app.BenchmarkScheduleEnabled, app.BenchmarkScheduleIntervalSeconds, app.OpportunisticMetricsEnabled)
	}
	got, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.BenchmarkScheduleEnabled || got.BenchmarkScheduleIntervalSeconds != 3600 || !got.OpportunisticMetricsEnabled {
		t.Fatalf("reloaded benchmark modes = %v/%d/%v", got.BenchmarkScheduleEnabled, got.BenchmarkScheduleIntervalSeconds, got.OpportunisticMetricsEnabled)
	}

	// A partial PATCH updating only the benchmark fields flips them and leaves
	// the rest untouched.
	enabled := false
	interval := 900
	opp := false
	upd, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
		BenchmarkScheduleEnabled:         &enabled,
		BenchmarkScheduleIntervalSeconds: &interval,
		OpportunisticMetricsEnabled:      &opp,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.BenchmarkScheduleEnabled || upd.BenchmarkScheduleIntervalSeconds != 900 || upd.OpportunisticMetricsEnabled {
		t.Fatalf("updated benchmark modes = %v/%d/%v, want false/900/false",
			upd.BenchmarkScheduleEnabled, upd.BenchmarkScheduleIntervalSeconds, upd.OpportunisticMetricsEnabled)
	}
	if upd.Port != 8001 {
		t.Fatalf("partial PATCH mutated port: %d, want 8001", upd.Port)
	}

	// A negative interval is rejected on create and on update, mutating nothing.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8002, Scheme: "https", BenchmarkScheduleIntervalSeconds: -1,
	}); !errors.Is(err, ErrApplicationBenchmarkIntervalInvalid) {
		t.Fatalf("create interval=-1 err = %v, want ErrApplicationBenchmarkIntervalInvalid", err)
	}
	neg := -1
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{
		BenchmarkScheduleIntervalSeconds: &neg,
	}); !errors.Is(err, ErrApplicationBenchmarkIntervalInvalid) {
		t.Fatalf("update interval=-1 err = %v, want ErrApplicationBenchmarkIntervalInvalid", err)
	}
	after, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("reload after rejected update: %v", err)
	}
	if after.BenchmarkScheduleIntervalSeconds != 900 {
		t.Fatalf("rejected update mutated interval: %d, want 900", after.BenchmarkScheduleIntervalSeconds)
	}
}

func TestCreateApplicationNonOwnerNonAdminForbidden(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.CreateApplication(context.Background(), otherToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner create err = %v, want ErrServerNotFound", err)
	}
}

func TestCreateApplicationValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	base := func() CreateApplicationRequest {
		return CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"}
	}

	cases := []struct {
		name    string
		mutate  func(req CreateApplicationRequest) CreateApplicationRequest
		wantErr error
	}{
		{
			name:    "bad type",
			mutate:  func(req CreateApplicationRequest) CreateApplicationRequest { req.Type = "bogus"; return req },
			wantErr: ErrApplicationTypeInvalid,
		},
		{
			name:    "bad scheme",
			mutate:  func(req CreateApplicationRequest) CreateApplicationRequest { req.Scheme = "ftp"; return req },
			wantErr: ErrApplicationSchemeInvalid,
		},
		{
			name:    "port zero",
			mutate:  func(req CreateApplicationRequest) CreateApplicationRequest { req.Port = 0; return req },
			wantErr: ErrApplicationPortInvalid,
		},
		{
			name:    "port too large",
			mutate:  func(req CreateApplicationRequest) CreateApplicationRequest { req.Port = 65536; return req },
			wantErr: ErrApplicationPortInvalid,
		},
		{
			name: "bad flavor",
			mutate: func(req CreateApplicationRequest) CreateApplicationRequest {
				req.APIFlavors = []string{"openai", "bogus"}
				return req
			},
			wantErr: ErrApplicationFlavorInvalid,
		},
		{
			name:    "bad status",
			mutate:  func(req CreateApplicationRequest) CreateApplicationRequest { req.Status = "bogus"; return req },
			wantErr: ErrApplicationStatusInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, tc.mutate(base())); !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateApplication error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCreateApplicationAllowsLlamaSwapTypeAndAcceptsAllValidTypes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	port := 9000
	for _, typ := range []string{routing.ProviderOllama, routing.ProviderVLLM, routing.ProviderLlamaCPP, routing.ProviderLlamaSwap, routing.ProviderLiteLLM} {
		port++
		if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
			Type: typ, Port: port, Scheme: "http",
		}); err != nil {
			t.Fatalf("CreateApplication type=%s: %v", typ, err)
		}
	}
}

func TestNormalizeApplicationTypeLiteLLM(t *testing.T) {
	got, err := normalizeApplicationType("litellm")
	if err != nil {
		t.Fatalf("normalizeApplicationType(litellm) err: %v", err)
	}
	if got != routing.ProviderLiteLLM {
		t.Fatalf("normalizeApplicationType(litellm) = %q, want %q", got, routing.ProviderLiteLLM)
	}
}

func TestNormalizeLoadedModelsFormatLiteLLM(t *testing.T) {
	if got := normalizeLoadedModelsFormat("litellm"); got != "litellm" {
		t.Fatalf("normalizeLoadedModelsFormat(litellm) = %q, want litellm", got)
	}
	// existing formats unchanged; an unknown one still degrades to auto.
	for in, want := range map[string]string{"openai": "openai", "llama_swap": "llama_swap", "llama_cpp": "llama_cpp", "auto": "auto", "": "", "bogus": "auto"} {
		if got := normalizeLoadedModelsFormat(in); got != want {
			t.Fatalf("normalizeLoadedModelsFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateApplicationDuplicatePortConflicts(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderOllama, Port: 8000, Scheme: "http",
	}); !errors.Is(err, ErrApplicationConflict) {
		t.Fatalf("duplicate port err = %v, want ErrApplicationConflict", err)
	}
}

// TestApplicationProxyListenPortRoundTrip covers the P4 gateway-assigned TLS
// proxy-listen port end to end: 0 (auto) is the default and is allowed;
// create/update persist + round-trip an explicit value through the DTO;
// updating leaves it unchanged when the request omits it; validation rejects
// an out-of-range value and a duplicate within the same server (0 never
// conflicts with itself across multiple applications).
func TestApplicationProxyListenPortRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Default (omitted) is 0 = auto, and multiple apps may share it.
	appA, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create appA: %v", err)
	}
	if appA.ProxyListenPort != 0 {
		t.Fatalf("appA.ProxyListenPort = %d, want 0 (default/auto)", appA.ProxyListenPort)
	}
	appB, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderOllama, Port: 8001, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("create appB (second 0/auto app on the same server): %v", err)
	}
	if appB.ProxyListenPort != 0 {
		t.Fatalf("appB.ProxyListenPort = %d, want 0", appB.ProxyListenPort)
	}

	// Create with an explicit ProxyListenPort persists + round-trips via the DTO.
	appC, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8002, Scheme: "https", ProxyListenPort: 8600,
	})
	if err != nil {
		t.Fatalf("create appC with explicit ProxyListenPort: %v", err)
	}
	if appC.ProxyListenPort != 8600 {
		t.Fatalf("appC.ProxyListenPort = %d, want 8600", appC.ProxyListenPort)
	}
	reloaded, err := svc.GetApplication(context.Background(), ownerToken(), appC.ID)
	if err != nil {
		t.Fatalf("GetApplication appC: %v", err)
	}
	if reloaded.ProxyListenPort != 8600 {
		t.Fatalf("reloaded appC.ProxyListenPort = %d, want 8600 (persisted)", reloaded.ProxyListenPort)
	}

	// A duplicate ProxyListenPort within the same server is rejected on create.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderOllama, Port: 8003, Scheme: "http", ProxyListenPort: 8600,
	}); !errors.Is(err, ErrApplicationProxyListenPortConflict) {
		t.Fatalf("duplicate ProxyListenPort create err = %v, want ErrApplicationProxyListenPortConflict", err)
	}

	// Out-of-range values are rejected (0 stays valid/auto).
	for _, bad := range []int{-1, 65536} {
		req := CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8004, Scheme: "https", ProxyListenPort: bad}
		if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, req); !errors.Is(err, ErrApplicationProxyListenPortInvalid) {
			t.Fatalf("ProxyListenPort=%d create err = %v, want ErrApplicationProxyListenPortInvalid", bad, err)
		}
	}

	// Update: omitting the field leaves it unchanged.
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{
		Priority: intPtr(1),
	}); err != nil {
		t.Fatalf("update appC (unrelated field): %v", err)
	}
	unchanged, err := svc.GetApplication(context.Background(), ownerToken(), appC.ID)
	if err != nil {
		t.Fatalf("GetApplication appC after unrelated update: %v", err)
	}
	if unchanged.ProxyListenPort != 8600 {
		t.Fatalf("appC.ProxyListenPort after unrelated update = %d, want unchanged 8600", unchanged.ProxyListenPort)
	}

	// Update can change it to a new valid, non-conflicting value, and round-trips.
	newPort := 8601
	updated, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{ProxyListenPort: &newPort})
	if err != nil {
		t.Fatalf("update appC ProxyListenPort: %v", err)
	}
	if updated.ProxyListenPort != 8601 {
		t.Fatalf("updated appC.ProxyListenPort = %d, want 8601", updated.ProxyListenPort)
	}

	// Re-applying an application's OWN current ProxyListenPort on update is not
	// a self-conflict (the exclude-self behavior mirrors the real Port check).
	ownPort := 8601
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{ProxyListenPort: &ownPort}); err != nil {
		t.Fatalf("update appC to its own current port must be a no-op, not a conflict: %v", err)
	}

	// Update rejects a duplicate against ANOTHER application's ProxyListenPort
	// on the same server.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderOllama, Port: 8005, Scheme: "http", ProxyListenPort: 8700,
	}); err != nil {
		t.Fatalf("create appD: %v", err)
	}
	otherAppsPort := 8700
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{ProxyListenPort: &otherAppsPort}); !errors.Is(err, ErrApplicationProxyListenPortConflict) {
		t.Fatalf("update to appD's ProxyListenPort err = %v, want ErrApplicationProxyListenPortConflict", err)
	}

	// Update rejects an out-of-range value too.
	badPort := 70000
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{ProxyListenPort: &badPort}); !errors.Is(err, ErrApplicationProxyListenPortInvalid) {
		t.Fatalf("update out-of-range ProxyListenPort err = %v, want ErrApplicationProxyListenPortInvalid", err)
	}

	// Update can reset back to 0 (auto).
	zero := 0
	resetDTO, err := svc.UpdateApplication(context.Background(), ownerToken(), appC.ID, UpdateApplicationRequest{ProxyListenPort: &zero})
	if err != nil {
		t.Fatalf("update appC reset to 0: %v", err)
	}
	if resetDTO.ProxyListenPort != 0 {
		t.Fatalf("appC.ProxyListenPort after reset = %d, want 0", resetDTO.ProxyListenPort)
	}
}

func TestListApplicationsScopedToServer(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server1 := createTestServer(t, svc, "S1", "s1.example.test")
	server2 := createTestServer(t, svc, "S2", "s2.example.test")

	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server1.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"}); err != nil {
		t.Fatalf("create app1: %v", err)
	}
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server1.ID, CreateApplicationRequest{Type: routing.ProviderOllama, Port: 8001, Scheme: "http"}); err != nil {
		t.Fatalf("create app2: %v", err)
	}
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server2.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"}); err != nil {
		t.Fatalf("create app3: %v", err)
	}

	list, err := svc.ListApplications(context.Background(), ownerToken(), server1.ID)
	if err != nil {
		t.Fatalf("ListApplications: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("list = %#v, want 2 apps scoped to server1", list.Data)
	}

	if _, err := svc.ListApplications(context.Background(), otherToken(), server1.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner list err = %v, want ErrServerNotFound", err)
	}
}

func TestGetUpdateDeleteApplicationRBACNoExistenceLeak(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// non-owner non-admin: not found (no leak of server.not_found)
	if _, err := svc.GetApplication(context.Background(), otherToken(), app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("other get err = %v, want ErrApplicationNotFound", err)
	}
	newPort := 8100
	if _, err := svc.UpdateApplication(context.Background(), otherToken(), app.ID, UpdateApplicationRequest{Port: &newPort}); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("other update err = %v, want ErrApplicationNotFound", err)
	}
	if err := svc.DeleteApplication(context.Background(), otherToken(), app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("other delete err = %v, want ErrApplicationNotFound", err)
	}

	// unknown application id: not found
	if _, err := svc.GetApplication(context.Background(), ownerToken(), "app_ghost"); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("ghost get err = %v, want ErrApplicationNotFound", err)
	}

	// owner may get/update
	got, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if got.ID != app.ID {
		t.Fatalf("got = %#v", got)
	}
	updated, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Port: &newPort})
	if err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if updated.Port != newPort {
		t.Fatalf("updated port = %d, want %d", updated.Port, newPort)
	}
	if updated.Endpoint != "https://s.example.test:8100" {
		t.Fatalf("updated endpoint = %q", updated.Endpoint)
	}

	// admin may delete
	if err := svc.DeleteApplication(context.Background(), systemAdminToken(), app.ID); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
	if _, err := svc.GetApplication(context.Background(), systemAdminToken(), app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("get after delete err = %v, want ErrApplicationNotFound", err)
	}
}

func TestUpdateApplicationValidatesFields(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	badType := "bogus"
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Type: &badType}); !errors.Is(err, ErrApplicationTypeInvalid) {
		t.Fatalf("bad type err = %v", err)
	}
	badScheme := "ftp"
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Scheme: &badScheme}); !errors.Is(err, ErrApplicationSchemeInvalid) {
		t.Fatalf("bad scheme err = %v", err)
	}
	badPort := 0
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Port: &badPort}); !errors.Is(err, ErrApplicationPortInvalid) {
		t.Fatalf("bad port err = %v", err)
	}
	badFlavors := []string{"bogus"}
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{APIFlavors: &badFlavors}); !errors.Is(err, ErrApplicationFlavorInvalid) {
		t.Fatalf("bad flavor err = %v", err)
	}
	badStatus := "bogus"
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Status: &badStatus}); !errors.Is(err, ErrApplicationStatusInvalid) {
		t.Fatalf("bad status err = %v", err)
	}

	// unchanged after all the failed validations
	got, err := svc.GetApplication(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Type != routing.ProviderVLLM || got.Scheme != "https" || got.Port != 8000 || got.Status != routing.ServerStatusActive {
		t.Fatalf("app mutated despite failed validation: %#v", got)
	}
}

func TestUpdateApplicationChangingPortToTakenConflicts(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app1, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app1: %v", err)
	}
	app2, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderOllama, Port: 8001, Scheme: "http"})
	if err != nil {
		t.Fatalf("create app2: %v", err)
	}

	takenPort := app1.Port
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app2.ID, UpdateApplicationRequest{Port: &takenPort}); !errors.Is(err, ErrApplicationConflict) {
		t.Fatalf("update to taken port err = %v, want ErrApplicationConflict", err)
	}

	// updating an app to keep its own current port is not a self-conflict
	ownPort := app1.Port
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app1.ID, UpdateApplicationRequest{Port: &ownPort}); err != nil {
		t.Fatalf("update to same own port should not conflict: %v", err)
	}
}

func TestUpdateApplicationPartialUpdatePreservesOtherFields(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", Priority: 5, Weight: 7,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	disabled := routing.ServerStatusDisabled
	updated, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, UpdateApplicationRequest{Status: &disabled})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if updated.Status != routing.ServerStatusDisabled {
		t.Fatalf("status = %q", updated.Status)
	}
	if updated.Port != 8000 || updated.Scheme != "https" || updated.Priority != 5 || updated.Weight != 7 {
		t.Fatalf("other fields mutated: %#v", updated)
	}
}

func TestCreateApplicationRejectsNegativeTuning(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	_, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", Priority: -1,
	})
	if !errors.Is(err, ErrApplicationTuningInvalid) {
		t.Fatalf("err = %v, want ErrApplicationTuningInvalid", err)
	}
}

func TestUpdateApplicationRejectsNegativeTuning(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}

	negative := -5
	cases := []struct {
		name string
		req  UpdateApplicationRequest
	}{
		{name: "priority", req: UpdateApplicationRequest{Priority: &negative}},
		{name: "weight", req: UpdateApplicationRequest{Weight: &negative}},
		{name: "timeout_ms", req: UpdateApplicationRequest{TimeoutMS: &negative}},
		{name: "affinity_ttl_seconds", req: UpdateApplicationRequest{AffinityTTLSeconds: &negative}},
		{name: "admission_queue_timeout_seconds", req: UpdateApplicationRequest{AdmissionQueueTimeoutSeconds: &negative}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A rejected update mutates nothing, so reusing one app is safe.
			if _, err := svc.UpdateApplication(context.Background(), ownerToken(), app.ID, tc.req); !errors.Is(err, ErrApplicationTuningInvalid) {
				t.Fatalf("err = %v, want ErrApplicationTuningInvalid", err)
			}
		})
	}
}

func TestDeleteApplicationCascadesMappings(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := routeStore.CreateMapping(context.Background(), routing.ModelMapping{
		ID: "map_1", ApplicationID: app.ID, GatewayModelName: "qwen", AppModelName: "qwen2.5",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	if err := svc.DeleteApplication(context.Background(), ownerToken(), app.ID); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	if len(mappings) != 0 {
		t.Fatalf("mappings should cascade-delete, got %#v", mappings)
	}
}

// ---- T5: application CRUD reconciles the server's NetBird access policy -------
//
// These tests use the NetBird-aware harness from netbird_server_test.go /
// service_netbird_policy_test.go (newNetbirdServerTestService, newFakeNetbird,
// enableNetbird[Policies], seedManagedNetbirdServer, seedApp) since
// newServerTestService does not wire a SystemSettings store or cipher.

// TestCreateApplicationReconcilesPolicy: creating a new active application on a
// managed, policy-managed server issues a CreatePolicy reflecting the new port.
func TestCreateApplicationReconcilesPolicy(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srv-capp", "S", "track-capp", "", now)

	dto, err := svc.CreateApplication(context.Background(), systemAdminToken(), "srv-capp", CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8080, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if dto.Port != 8080 {
		t.Fatalf("dto.Port = %d, want 8080", dto.Port)
	}
	if fake.policyCreateCount() != 1 {
		t.Fatalf("policy creates = %d, want 1", fake.policyCreateCount())
	}
	body := fake.lastCreatedPolicy()
	if ports := policyRuleField(t, body, "ports"); len(ports) != 1 || ports[0] != "8080" {
		t.Fatalf("policy ports = %v, want [8080]", ports)
	}
}

// TestDeleteApplicationReconcilesPolicy: deleting the LAST active application on a
// server drops its only port from the active set, so the reconcile deletes the
// now-unneeded access policy. The server is captured BEFORE the row delete inside
// DeleteApplication, and reconcileServerPolicy re-derives the (now empty) active
// port set from the store afterward.
func TestDeleteApplicationReconcilesPolicy(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srv-dapp", "S", "track-dapp", "", now)
	seedApp(t, routeStore, "app-dapp", "srv-dapp", 8080, routing.ServerStatusActive, now)
	fake.seedPolicy("pol-dapp", "op-gw-access-srv-dapp", true, []string{"8080"}, "gw-portal", "track-dapp")

	if err := svc.DeleteApplication(context.Background(), systemAdminToken(), "app-dapp"); err != nil {
		t.Fatalf("DeleteApplication: %v", err)
	}
	if _, err := routeStore.ApplicationByID(context.Background(), "app-dapp"); err == nil {
		t.Fatalf("application still present after delete")
	}
	if !fake.wasPolicyDeleted("pol-dapp") {
		t.Fatalf("policy pol-dapp was not deleted after removing the last active app")
	}
}

// TestUpdateApplicationReconcilesPolicy: changing an application's port from P to Q
// updates the server's access policy to reflect the new port set.
func TestUpdateApplicationReconcilesPolicy(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srv-uapp", "S", "track-uapp", "", now)
	seedApp(t, routeStore, "app-uapp", "srv-uapp", 8080, routing.ServerStatusActive, now)
	fake.seedPolicy("pol-uapp", "op-gw-access-srv-uapp", true, []string{"8080"}, "gw-portal", "track-uapp")

	newPort := 9090
	dto, err := svc.UpdateApplication(context.Background(), systemAdminToken(), "app-uapp", UpdateApplicationRequest{Port: &newPort})
	if err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	if dto.Port != 9090 {
		t.Fatalf("dto.Port = %d, want 9090", dto.Port)
	}
	if fake.policyUpdateCount() != 1 {
		t.Fatalf("policy updates = %d, want 1", fake.policyUpdateCount())
	}
	body := fake.lastUpdatedPolicy()
	if ports := policyRuleField(t, body, "ports"); len(ports) != 1 || ports[0] != "9090" {
		t.Fatalf("updated policy ports = %v, want [9090]", ports)
	}
}

// TestApplicationCrudReconcileBestEffort: a NetBird 500 (ListPolicies failing, the
// first call any reconcile makes) never fails the CRUD operation itself — the
// application row change is still persisted and the service call returns nil error.
func TestApplicationCrudReconcileBestEffort(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failListPolicies = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srv-best", "S", "track-best", "", now)

	t.Run("create", func(t *testing.T) {
		dto, err := svc.CreateApplication(context.Background(), systemAdminToken(), "srv-best", CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8080, Scheme: "http",
		})
		if err != nil {
			t.Fatalf("CreateApplication should succeed despite a NetBird 500: %v", err)
		}
		stored, gerr := routeStore.ApplicationByID(context.Background(), dto.ID)
		if gerr != nil || stored.Port != 8080 {
			t.Fatalf("application not persisted: err=%v stored=%+v", gerr, stored)
		}
	})

	t.Run("delete", func(t *testing.T) {
		app, cerr := svc.CreateApplication(context.Background(), systemAdminToken(), "srv-best", CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8081, Scheme: "http",
		})
		if cerr != nil {
			t.Fatalf("seed application for delete: %v", cerr)
		}
		if err := svc.DeleteApplication(context.Background(), systemAdminToken(), app.ID); err != nil {
			t.Fatalf("DeleteApplication should succeed despite a NetBird 500: %v", err)
		}
		if _, gerr := routeStore.ApplicationByID(context.Background(), app.ID); gerr == nil {
			t.Fatalf("application still present after delete")
		}
	})
}

// TestApplicationCrudNoReconcileWhenManageOff: with the NetBird module enabled but
// policy management OFF (the default), create/update/delete issue ZERO NetBird
// requests — reconcileServerPolicy returns at the manage-off gate.
func TestApplicationCrudNoReconcileWhenManageOff(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true) // module ON, manage-policies OFF (default)
	seedManagedNetbirdServer(t, routeStore, "srv-off", "S", "track-off", "", now)

	dto, err := svc.CreateApplication(context.Background(), systemAdminToken(), "srv-off", CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8080, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("netbird requests after create = %d, want 0 (policy management off)", fake.count())
	}

	newPort := 9090
	if _, err := svc.UpdateApplication(context.Background(), systemAdminToken(), dto.ID, UpdateApplicationRequest{Port: &newPort}); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("netbird requests after update = %d, want 0 (policy management off)", fake.count())
	}

	if err := svc.DeleteApplication(context.Background(), systemAdminToken(), dto.ID); err != nil {
		t.Fatalf("DeleteApplication: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("netbird requests after delete = %d, want 0 (policy management off)", fake.count())
	}
	if _, gerr := routeStore.ApplicationByID(context.Background(), dto.ID); gerr == nil {
		t.Fatalf("application still present after delete")
	}
}

func TestCreateListUpdateDeleteMappingAdminOrOwner(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "qwen",
		AppModelName:     "qwen2.5",
	})
	if err != nil {
		t.Fatalf("CreateMapping (owner): %v", err)
	}
	if dto.ID == "" || dto.ApplicationID != app.ID || dto.GatewayModelName != "qwen" || dto.AppModelName != "qwen2.5" {
		t.Fatalf("dto = %#v", dto)
	}
	if dto.Status != routing.ServerStatusActive {
		t.Fatalf("status default = %q", dto.Status)
	}
	if dto.CreatedAt.IsZero() {
		t.Fatalf("created_at not set")
	}

	list, err := svc.ListMappings(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != dto.ID {
		t.Fatalf("list = %#v", list.Data)
	}

	newAppName := "qwen2.5-new"
	updated, err := svc.UpdateMapping(context.Background(), systemAdminToken(), dto.ID, UpdateMappingRequest{AppModelName: &newAppName})
	if err != nil {
		t.Fatalf("UpdateMapping (admin): %v", err)
	}
	if updated.AppModelName != newAppName {
		t.Fatalf("updated = %#v", updated)
	}

	if err := svc.DeleteMapping(context.Background(), ownerToken(), dto.ID); err != nil {
		t.Fatalf("DeleteMapping: %v", err)
	}
	list2, err := svc.ListMappings(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("ListMappings after delete: %v", err)
	}
	if len(list2.Data) != 0 {
		t.Fatalf("mapping not deleted: %#v", list2.Data)
	}
}

func TestMappingNonOwnerNonAdminNotFound(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen2.5"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	if _, err := svc.ListMappings(context.Background(), otherToken(), app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("non-owner list err = %v, want ErrApplicationNotFound", err)
	}
	if _, err := svc.CreateMapping(context.Background(), otherToken(), app.ID, CreateMappingRequest{GatewayModelName: "g", AppModelName: "a"}); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("non-owner create err = %v, want ErrApplicationNotFound", err)
	}
	newAppName := "x"
	if _, err := svc.UpdateMapping(context.Background(), otherToken(), dto.ID, UpdateMappingRequest{AppModelName: &newAppName}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("non-owner update err = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteMapping(context.Background(), otherToken(), dto.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("non-owner delete err = %v, want ErrMappingNotFound", err)
	}
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), "map_ghost", UpdateMappingRequest{AppModelName: &newAppName}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("ghost update err = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteMapping(context.Background(), ownerToken(), "map_ghost"); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("ghost delete err = %v, want ErrMappingNotFound", err)
	}
}

func TestCreateMappingValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "  ", AppModelName: "a"}); !errors.Is(err, ErrMappingGatewayNameRequired) {
		t.Fatalf("blank gateway name err = %v, want ErrMappingGatewayNameRequired", err)
	}
	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "g", AppModelName: "  "}); !errors.Is(err, ErrMappingAppNameRequired) {
		t.Fatalf("blank app name err = %v, want ErrMappingAppNameRequired", err)
	}
	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "g", AppModelName: "a", Status: "bogus"}); !errors.Is(err, ErrMappingStatusInvalid) {
		t.Fatalf("bad status err = %v, want ErrMappingStatusInvalid", err)
	}

	dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "g", AppModelName: "a"})
	if err != nil {
		t.Fatalf("create valid mapping: %v", err)
	}
	blank := "   "
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), dto.ID, UpdateMappingRequest{GatewayModelName: &blank}); !errors.Is(err, ErrMappingGatewayNameRequired) {
		t.Fatalf("update blank gateway name err = %v, want ErrMappingGatewayNameRequired", err)
	}
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), dto.ID, UpdateMappingRequest{AppModelName: &blank}); !errors.Is(err, ErrMappingAppNameRequired) {
		t.Fatalf("update blank app name err = %v, want ErrMappingAppNameRequired", err)
	}
	badStatus := "bogus"
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), dto.ID, UpdateMappingRequest{Status: &badStatus}); !errors.Is(err, ErrMappingStatusInvalid) {
		t.Fatalf("update bad status err = %v, want ErrMappingStatusInvalid", err)
	}
}

func TestMappingGatewayNameUniquePerServerAcrossApps(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app1, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app1: %v", err)
	}
	app2, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderOllama, Port: 8001, Scheme: "http"})
	if err != nil {
		t.Fatalf("create app2: %v", err)
	}

	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app1.ID, CreateMappingRequest{GatewayModelName: "Qwen", AppModelName: "qwen2.5"}); err != nil {
		t.Fatalf("create mapping app1: %v", err)
	}
	// case-insensitive clash on a DIFFERENT app of the same server
	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app2.ID, CreateMappingRequest{GatewayModelName: "qwEN", AppModelName: "qwen-app2"}); !errors.Is(err, ErrMappingGatewayNameConflict) {
		t.Fatalf("clash create err = %v, want ErrMappingGatewayNameConflict", err)
	}

	mapping2, err := svc.CreateMapping(context.Background(), ownerToken(), app2.ID, CreateMappingRequest{GatewayModelName: "llama", AppModelName: "llama-app2"})
	if err != nil {
		t.Fatalf("create mapping2: %v", err)
	}
	clash := "qwen"
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), mapping2.ID, UpdateMappingRequest{GatewayModelName: &clash}); !errors.Is(err, ErrMappingGatewayNameConflict) {
		t.Fatalf("clash update err = %v, want ErrMappingGatewayNameConflict", err)
	}
	// updating a mapping to keep its own current gateway name is not a self-conflict
	own := mapping2.GatewayModelName
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), mapping2.ID, UpdateMappingRequest{GatewayModelName: &own}); err != nil {
		t.Fatalf("update to same own gateway name should not conflict: %v", err)
	}
}

func TestCreateMappingStoresMetricsAndProvenance(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName:   "qwen",
		AppModelName:       "qwen2.5",
		ContextSize:        131072,
		GenTokensPerSecond: 40,
		IsMTP:              true,
		MetricsLocked:      true,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if dto.ContextSize != 131072 {
		t.Fatalf("context_size = %d, want 131072", dto.ContextSize)
	}
	if dto.GenTokensPerSecond != 40 {
		t.Fatalf("gen_tokens_per_second = %v, want 40", dto.GenTokensPerSecond)
	}
	if !dto.IsMtp {
		t.Fatalf("is_mtp = false, want true")
	}
	if !dto.MetricsLocked {
		t.Fatalf("metrics_locked = false, want true")
	}
	if dto.MetricsSource != "manual" {
		t.Fatalf("metrics_source = %q, want manual", dto.MetricsSource)
	}
	if dto.MetricsUpdatedAt == nil {
		t.Fatalf("metrics_updated_at not stamped")
	}

	// Lock-only create is policy, not a measurement: it must NOT stamp provenance.
	lockOnly, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "lock-only",
		AppModelName:     "lock-only-app",
		MetricsLocked:    true,
	})
	if err != nil {
		t.Fatalf("CreateMapping (lock-only): %v", err)
	}
	if !lockOnly.MetricsLocked {
		t.Fatalf("lock-only metrics_locked = false, want true")
	}
	if lockOnly.MetricsSource != "" {
		t.Fatalf("lock-only metrics_source = %q, want empty (no measured value)", lockOnly.MetricsSource)
	}
	if lockOnly.MetricsUpdatedAt != nil {
		t.Fatalf("lock-only metrics_updated_at = %v, want nil (no measured value)", lockOnly.MetricsUpdatedAt)
	}

	// IsMTP alone IS a measured capability, so it DOES stamp provenance.
	mtpOnly, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "mtp-only",
		AppModelName:     "mtp-only-app",
		IsMTP:            true,
	})
	if err != nil {
		t.Fatalf("CreateMapping (mtp-only): %v", err)
	}
	if !mtpOnly.IsMtp {
		t.Fatalf("mtp-only is_mtp = false, want true")
	}
	if mtpOnly.MetricsSource != "manual" {
		t.Fatalf("mtp-only metrics_source = %q, want manual", mtpOnly.MetricsSource)
	}
	if mtpOnly.MetricsUpdatedAt == nil {
		t.Fatalf("mtp-only metrics_updated_at not stamped")
	}
}

func TestCreateMappingDefaultsIsMTPFromName(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "deepseek-v3", AppModelName: "deepseek-v3",
	})
	if err != nil || !dto.IsMtp {
		t.Fatalf("expected is_mtp defaulted true for deepseek-v3, dto=%+v err=%v", dto, err)
	}
	// A name-derived default is NOT a manual measurement -> no provenance stamp.
	if dto.MetricsSource != "" || dto.MetricsUpdatedAt != nil {
		t.Fatalf("heuristic default must not stamp provenance: source=%q updated=%v", dto.MetricsSource, dto.MetricsUpdatedAt)
	}

	plain, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "qwen-coder", AppModelName: "qwen-coder",
	})
	if err != nil || plain.IsMtp {
		t.Fatalf("expected is_mtp false for qwen-coder, dto=%+v err=%v", plain, err)
	}
}

func TestUpdateMappingPatchesMetricsAndRejectsNegative(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	created, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "g", AppModelName: "a"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	patched, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{ContextSize: intPtr(8192)})
	if err != nil {
		t.Fatalf("UpdateMapping (context_size): %v", err)
	}
	if patched.ContextSize != 8192 {
		t.Fatalf("context_size = %d, want 8192", patched.ContextSize)
	}
	if patched.MetricsSource != "manual" {
		t.Fatalf("metrics_source = %q, want manual", patched.MetricsSource)
	}

	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{LoadTimeMS: intPtr(-5)}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("negative load_time_ms err = %v, want ErrMappingMetricInvalid", err)
	}

	// Reset-to-zero: an explicit 0 pointer resets ContextSize to "unknown" AND is a
	// value change (distinguished from "not provided"), so it stamps provenance.
	reset, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{ContextSize: intPtr(0)})
	if err != nil {
		t.Fatalf("UpdateMapping (reset context_size to 0): %v", err)
	}
	if reset.ContextSize != 0 {
		t.Fatalf("context_size after reset = %d, want 0", reset.ContextSize)
	}
	if reset.MetricsSource != "manual" {
		t.Fatalf("reset metrics_source = %q, want manual", reset.MetricsSource)
	}

	// Combined patch of all four numerics to DISTINCT values catches any
	// value/pointer transposition among them.
	gen := 40.5
	prompt := 200.25
	load := 1500
	ctx := 32768
	combined, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{
		GenTokensPerSecond:    &gen,
		PromptTokensPerSecond: &prompt,
		LoadTimeMS:            &load,
		ContextSize:           &ctx,
	})
	if err != nil {
		t.Fatalf("UpdateMapping (combined): %v", err)
	}
	if combined.GenTokensPerSecond != 40.5 {
		t.Fatalf("gen_tokens_per_second = %v, want 40.5", combined.GenTokensPerSecond)
	}
	if combined.PromptTokensPerSecond != 200.25 {
		t.Fatalf("prompt_tokens_per_second = %v, want 200.25", combined.PromptTokensPerSecond)
	}
	if combined.LoadTimeMS != 1500 {
		t.Fatalf("load_time_ms = %d, want 1500", combined.LoadTimeMS)
	}
	if combined.ContextSize != 32768 {
		t.Fatalf("context_size = %d, want 32768", combined.ContextSize)
	}
}

func TestMappingConcurrencyCapacityMetrics(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Create carries the three concurrency-capacity fields onto the DTO, and
	// setting any of them is a measured write -> stamps manual provenance.
	created, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName:             "g",
		AppModelName:                 "a",
		MaxConcurrency:               8,
		RecommendedConcurrency:       4,
		GenTokensPerSecondAtCapacity: 320.5,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if created.MaxConcurrency != 8 {
		t.Fatalf("max_concurrency = %d, want 8", created.MaxConcurrency)
	}
	if created.RecommendedConcurrency != 4 {
		t.Fatalf("recommended_concurrency = %d, want 4", created.RecommendedConcurrency)
	}
	if created.GenTokensPerSecondAtCapacity != 320.5 {
		t.Fatalf("gen_tokens_per_second_at_capacity = %v, want 320.5", created.GenTokensPerSecondAtCapacity)
	}
	if created.MetricsSource != "manual" {
		t.Fatalf("metrics_source = %q, want manual (a capacity metric was set)", created.MetricsSource)
	}
	if created.MetricsUpdatedAt == nil {
		t.Fatalf("metrics_updated_at not stamped after a capacity metric write")
	}

	// Partial PATCH of all three to DISTINCT values survives round-trip and
	// catches any value/pointer transposition among them.
	maxC := 16
	recC := 12
	genCap := 640.25
	patched, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{
		MaxConcurrency:               &maxC,
		RecommendedConcurrency:       &recC,
		GenTokensPerSecondAtCapacity: &genCap,
	})
	if err != nil {
		t.Fatalf("UpdateMapping (capacity patch): %v", err)
	}
	if patched.MaxConcurrency != 16 {
		t.Fatalf("patched max_concurrency = %d, want 16", patched.MaxConcurrency)
	}
	if patched.RecommendedConcurrency != 12 {
		t.Fatalf("patched recommended_concurrency = %d, want 12", patched.RecommendedConcurrency)
	}
	if patched.GenTokensPerSecondAtCapacity != 640.25 {
		t.Fatalf("patched gen_tokens_per_second_at_capacity = %v, want 640.25", patched.GenTokensPerSecondAtCapacity)
	}
	if patched.MetricsSource != "manual" {
		t.Fatalf("patched metrics_source = %q, want manual", patched.MetricsSource)
	}

	// Negative rejection on the CREATE path, each field independently.
	negCreates := []struct {
		name string
		req  CreateMappingRequest
	}{
		{"max_concurrency", CreateMappingRequest{GatewayModelName: "n1", AppModelName: "n1a", MaxConcurrency: -1}},
		{"recommended_concurrency", CreateMappingRequest{GatewayModelName: "n2", AppModelName: "n2a", RecommendedConcurrency: -1}},
		{"gen_tokens_per_second_at_capacity", CreateMappingRequest{GatewayModelName: "n3", AppModelName: "n3a", GenTokensPerSecondAtCapacity: -0.5}},
	}
	for _, tc := range negCreates {
		if _, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, tc.req); !errors.Is(err, ErrMappingMetricInvalid) {
			t.Fatalf("create negative %s err = %v, want ErrMappingMetricInvalid", tc.name, err)
		}
	}

	// Negative rejection on the UPDATE path, each field independently.
	negMax := -1
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{MaxConcurrency: &negMax}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("update negative max_concurrency err = %v, want ErrMappingMetricInvalid", err)
	}
	negRec := -1
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{RecommendedConcurrency: &negRec}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("update negative recommended_concurrency err = %v, want ErrMappingMetricInvalid", err)
	}
	negGen := -0.5
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{GenTokensPerSecondAtCapacity: &negGen}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("update negative gen_tokens_per_second_at_capacity err = %v, want ErrMappingMetricInvalid", err)
	}
}

// TestCreateMappingVisionCapableRoundTrip: a `vision_capable: true` create
// round-trips onto ModelMappingDTO.VisionCapable, is a measured write (stamps
// manual provenance), and an UpdateMapping patch of the flag round-trips too.
func TestCreateMappingVisionCapableRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	created, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "g",
		AppModelName:     "a",
		VisionCapable:    true,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if !created.VisionCapable {
		t.Fatalf("vision_capable = false, want true")
	}
	if created.MetricsSource != "manual" {
		t.Fatalf("metrics_source = %q, want manual (vision_capable was set)", created.MetricsSource)
	}
	if created.MetricsUpdatedAt == nil {
		t.Fatalf("metrics_updated_at not stamped after a vision_capable write")
	}

	// A mapping created without vision_capable defaults to false.
	plain, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "plain",
		AppModelName:     "plain-app",
	})
	if err != nil {
		t.Fatalf("CreateMapping (plain): %v", err)
	}
	if plain.VisionCapable {
		t.Fatalf("plain vision_capable = true, want false (unset)")
	}

	// UpdateMapping can flip it back to false, and the change stamps provenance.
	falseVal := false
	patched, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{VisionCapable: &falseVal})
	if err != nil {
		t.Fatalf("UpdateMapping (vision_capable): %v", err)
	}
	if patched.VisionCapable {
		t.Fatalf("patched vision_capable = true, want false")
	}
	if patched.MetricsSource != "manual" {
		t.Fatalf("patched metrics_source = %q, want manual (vision_capable change)", patched.MetricsSource)
	}
}

// TestMappingEnergyWhPerTokenRoundTrip: an `energy_wh_per_token` create
// round-trips onto ModelMappingDTO.EnergyWhPerToken, is a measured write
// (stamps manual provenance), an UpdateMapping patch of it round-trips too,
// and a negative value is rejected on both the create and update paths.
func TestMappingEnergyWhPerTokenRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	created, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "g",
		AppModelName:     "a",
		EnergyWhPerToken: 0.0025,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if created.EnergyWhPerToken != 0.0025 {
		t.Fatalf("energy_wh_per_token = %v, want 0.0025", created.EnergyWhPerToken)
	}
	if created.MetricsSource != "manual" {
		t.Fatalf("metrics_source = %q, want manual (energy_wh_per_token was set)", created.MetricsSource)
	}
	if created.MetricsUpdatedAt == nil {
		t.Fatalf("metrics_updated_at not stamped after an energy_wh_per_token write")
	}

	// A mapping created without energy_wh_per_token defaults to 0 (unknown).
	plain, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "plain",
		AppModelName:     "plain-app",
	})
	if err != nil {
		t.Fatalf("CreateMapping (plain): %v", err)
	}
	if plain.EnergyWhPerToken != 0 {
		t.Fatalf("plain energy_wh_per_token = %v, want 0 (unset)", plain.EnergyWhPerToken)
	}

	// UpdateMapping patches it to a distinct value and stamps provenance.
	energyVal := 0.0091
	patched, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{EnergyWhPerToken: &energyVal})
	if err != nil {
		t.Fatalf("UpdateMapping (energy_wh_per_token): %v", err)
	}
	if patched.EnergyWhPerToken != 0.0091 {
		t.Fatalf("patched energy_wh_per_token = %v, want 0.0091", patched.EnergyWhPerToken)
	}
	if patched.MetricsSource != "manual" {
		t.Fatalf("patched metrics_source = %q, want manual (energy_wh_per_token change)", patched.MetricsSource)
	}

	// Negative rejection on the CREATE path.
	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "neg", AppModelName: "neg-a", EnergyWhPerToken: -0.5,
	}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("create negative energy_wh_per_token err = %v, want ErrMappingMetricInvalid", err)
	}

	// Negative rejection on the UPDATE path.
	negEnergy := -0.5
	if _, err := svc.UpdateMapping(context.Background(), ownerToken(), created.ID, UpdateMappingRequest{EnergyWhPerToken: &negEnergy}); !errors.Is(err, ErrMappingMetricInvalid) {
		t.Fatalf("update negative energy_wh_per_token err = %v, want ErrMappingMetricInvalid", err)
	}
}

// fakeLister is a test double for provider.ModelLister. It captures the upstream auth
// carried on the ctx so a test can assert the model-discovery call authenticates. The
// capture fields are mutex-guarded because a concurrent-reconcile test drives ListModels
// from several goroutines against one shared lister.
type fakeLister struct {
	models []string
	err    error

	mu            sync.Mutex
	gotAuthHeader string
	gotAuthToken  string
	gotAuthOK     bool
}

func (f *fakeLister) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	auth, ok := provider.UpstreamAuthFrom(ctx)
	f.mu.Lock()
	f.gotAuthOK = ok
	f.gotAuthHeader = auth.Header
	f.gotAuthToken = auth.Token
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

// auth returns the captured upstream credential under the lock (for sequential assertions).
func (f *fakeLister) auth() (header, token string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotAuthHeader, f.gotAuthToken, f.gotAuthOK
}

func newServerTestServiceWithLister(t *testing.T, now time.Time, lister *fakeLister) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(context.Background(), store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, ModelLister: lister, Clock: func() time.Time { return now }})
	return svc, routeStore
}

func TestSyncApplicationModelsAddsFreshMappings(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"a", "b"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	result, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("SyncApplicationModels: %v", err)
	}
	if result.Added != 2 || result.Disabled != 0 || result.Unchanged != 0 || result.Conflicted != 0 {
		t.Fatalf("result = %#v", result)
	}
	list, err := svc.ListMappings(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("mappings = %#v", list.Data)
	}
	for _, m := range list.Data {
		if m.Status != routing.ServerStatusActive {
			t.Fatalf("mapping not active: %#v", m)
		}
		if m.GatewayModelName != m.AppModelName {
			t.Fatalf("expected 1:1 mapping, got %#v", m)
		}
	}
}

func TestReconcileCarriesUpstreamToken(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	lister := &fakeLister{models: []string{"a"}}
	svc, routeStore := newServerTestServiceWithLister(t, now, lister)
	server := createTestServer(t, svc, "S", "s.example.test")
	appDTO, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	routeServer, err := routeStore.AIServerByID(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	app, err := routeStore.ApplicationByID(context.Background(), appDTO.ID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}

	// Default header: a plain: token (openable without a cipher) → Authorization: Bearer.
	app.APIToken = "plain:sk-9"
	app.APITokenHeader = ""
	if _, err := svc.SyncApplicationModelsForApp(context.Background(), routeServer, app); err != nil {
		t.Fatalf("sync default header: %v", err)
	}
	if h, tok, ok := lister.auth(); !ok || tok != "sk-9" || h != "" {
		t.Fatalf("discovery auth = ok:%v header:%q token:%q, want ok:true header:\"\" token:sk-9", ok, h, tok)
	}

	// Custom header variant.
	app.APITokenHeader = "x-api-key"
	if _, err := svc.SyncApplicationModelsForApp(context.Background(), routeServer, app); err != nil {
		t.Fatalf("sync custom header: %v", err)
	}
	if h, tok, _ := lister.auth(); h != "x-api-key" || tok != "sk-9" {
		t.Fatalf("custom-header discovery auth = header:%q token:%q, want x-api-key/sk-9", h, tok)
	}

	// No token → no auth attached (fail-open / no-op — the pre-feature behavior).
	app.APIToken = ""
	app.APITokenHeader = ""
	if _, err := svc.SyncApplicationModelsForApp(context.Background(), routeServer, app); err != nil {
		t.Fatalf("sync no token: %v", err)
	}
	if h, tok, ok := lister.auth(); ok {
		t.Fatalf("expected no auth when token empty, got header:%q token:%q", h, tok)
	}
}

func TestSyncApplicationModelsForAppReconcilesWithoutPrincipal(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	lister := &fakeLister{models: []string{"a", "b"}}
	svc, routeStore := newServerTestServiceWithLister(t, now, lister)
	server := createTestServer(t, svc, "S", "s.example.test")
	appDTO, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	routeServer, err := routeStore.AIServerByID(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	app, err := routeStore.ApplicationByID(context.Background(), appDTO.ID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}

	// System path: no auth.Token; server + app supplied directly (as the probe
	// loop has them in hand).
	result, err := svc.SyncApplicationModelsForApp(context.Background(), routeServer, app)
	if err != nil {
		t.Fatalf("SyncApplicationModelsForApp: %v", err)
	}
	if result.Added != 2 {
		t.Fatalf("result = %#v, want added=2", result)
	}
	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mappings = %d, want 2", len(mappings))
	}

	// A second cycle with the same upstream is idempotent (all unchanged),
	// proving the reconcile is safe to run every interval.
	result, err = svc.SyncApplicationModelsForApp(context.Background(), routeServer, app)
	if err != nil {
		t.Fatalf("second SyncApplicationModelsForApp: %v", err)
	}
	if result.Unchanged != 2 || result.Added != 0 {
		t.Fatalf("second result = %#v, want unchanged=2 added=0", result)
	}

	// A listing error fails closed: ErrApplicationSyncFailed and no further
	// mapping changes.
	lister.err = errors.New("upstream down")
	if _, err := svc.SyncApplicationModelsForApp(context.Background(), routeServer, app); !errors.Is(err, ErrApplicationSyncFailed) {
		t.Fatalf("want ErrApplicationSyncFailed, got %v", err)
	}
}

func TestSyncApplicationModelsForAppSerializesGatewayNameConflict(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	// Two apps on one server whose upstreams serve the SAME model id (a redundant
	// backend). Concurrent reconciles must not both create an ACTIVE mapping for
	// it — the reconcileMu makes the per-server gateway-name check-then-act atomic.
	lister := &fakeLister{models: []string{"shared"}}
	svc, routeStore := newServerTestServiceWithLister(t, now, lister)
	server := createTestServer(t, svc, "S", "s.example.test")
	appA, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8001, Scheme: "https"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	appB, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8002, Scheme: "https"})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	rs, err := routeStore.AIServerByID(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	ra, err := routeStore.ApplicationByID(context.Background(), appA.ID)
	if err != nil {
		t.Fatalf("ApplicationByID A: %v", err)
	}
	rb, err := routeStore.ApplicationByID(context.Background(), appB.ID)
	if err != nil {
		t.Fatalf("ApplicationByID B: %v", err)
	}

	var wg sync.WaitGroup
	for _, app := range []routing.Application{ra, rb} {
		wg.Add(1)
		go func(a routing.Application) {
			defer wg.Done()
			_, _ = svc.SyncApplicationModelsForApp(context.Background(), rs, a)
		}(app)
	}
	wg.Wait()

	mappings, err := routeStore.MappingsByServer(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("MappingsByServer: %v", err)
	}
	active := 0
	for _, m := range mappings {
		if m.GatewayModelName == "shared" && m.Status == routing.ServerStatusActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf(`active mappings for "shared" on the server = %d, want exactly 1 (conflict serialized)`, active)
	}
}

func TestSyncApplicationModelsDedupesUpstream(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"a", "a", "b"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	result, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("SyncApplicationModels: %v", err)
	}
	if result.Added != 2 || result.Disabled != 0 || result.Unchanged != 0 || result.Conflicted != 0 {
		t.Fatalf("result = %#v, want added=2 (duplicate collapsed)", result)
	}

	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mappings = %#v, want 2 (one per distinct upstream id)", mappings)
	}
	countA := 0
	for _, m := range mappings {
		if m.AppModelName == "a" {
			countA++
		}
	}
	if countA != 1 {
		t.Fatalf("app_model_name %q rows = %d, want exactly 1", "a", countA)
	}
}

func TestSyncApplicationModelsUnchangedAndDisablesStale(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"a", "c"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := routeStore.CreateMapping(context.Background(), routing.ModelMapping{
		ID: "map_seed_a", ApplicationID: app.ID, GatewayModelName: "a", AppModelName: "a",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed mapping a: %v", err)
	}
	if err := routeStore.CreateMapping(context.Background(), routing.ModelMapping{
		ID: "map_seed_stale", ApplicationID: app.ID, GatewayModelName: "stale", AppModelName: "stale",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed mapping stale: %v", err)
	}

	result, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("SyncApplicationModels: %v", err)
	}
	if result.Added != 1 || result.Unchanged != 1 || result.Disabled != 1 || result.Conflicted != 0 {
		t.Fatalf("result = %#v", result)
	}

	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	if len(mappings) != 3 {
		t.Fatalf("mappings = %#v", mappings)
	}
	byAppName := map[string]routing.ModelMapping{}
	for _, m := range mappings {
		byAppName[m.AppModelName] = m
	}
	if byAppName["a"].Status != routing.ServerStatusActive {
		t.Fatalf("a should remain active: %#v", byAppName["a"])
	}
	if byAppName["c"].Status != routing.ServerStatusActive {
		t.Fatalf("c should be added active: %#v", byAppName["c"])
	}
	if byAppName["stale"].Status != routing.ServerStatusDisabled {
		t.Fatalf("stale should be disabled: %#v", byAppName["stale"])
	}
}

func TestSyncApplicationModelsGatewayNameClashWithAnotherAppDisablesAndConflicts(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"shared"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app1, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app1: %v", err)
	}
	app2, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderOllama, Port: 8001, Scheme: "http"})
	if err != nil {
		t.Fatalf("create app2: %v", err)
	}
	if _, err := svc.CreateMapping(context.Background(), ownerToken(), app1.ID, CreateMappingRequest{GatewayModelName: "shared", AppModelName: "shared-app1"}); err != nil {
		t.Fatalf("seed mapping app1: %v", err)
	}

	result, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app2.ID)
	if err != nil {
		t.Fatalf("SyncApplicationModels: %v", err)
	}
	if result.Added != 0 || result.Conflicted != 1 || result.Unchanged != 0 || result.Disabled != 0 {
		t.Fatalf("result = %#v", result)
	}
	list, err := svc.ListMappings(context.Background(), ownerToken(), app2.ID)
	if err != nil {
		t.Fatalf("ListMappings app2: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].Status != routing.ServerStatusDisabled {
		t.Fatalf("app2 mappings = %#v", list.Data)
	}
}

func TestSyncApplicationModelsListerErrorAppliesNoChanges(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	listerErr := errors.New("boom")
	svc, routeStore := newServerTestServiceWithLister(t, now, &fakeLister{err: listerErr})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := routeStore.CreateMapping(context.Background(), routing.ModelMapping{
		ID: "map_seed", ApplicationID: app.ID, GatewayModelName: "a", AppModelName: "a",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	if _, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app.ID); !errors.Is(err, ErrApplicationSyncFailed) {
		t.Fatalf("sync err = %v, want ErrApplicationSyncFailed", err)
	}
	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	if len(mappings) != 1 || mappings[0].Status != routing.ServerStatusActive {
		t.Fatalf("mapping changed despite lister error: %#v", mappings)
	}
}

func TestSyncApplicationModelsNilListerFails(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := svc.SyncApplicationModels(context.Background(), ownerToken(), app.ID); !errors.Is(err, ErrApplicationSyncFailed) {
		t.Fatalf("nil lister sync err = %v, want ErrApplicationSyncFailed", err)
	}
}

func TestSyncApplicationModelsNonOwnerNonAdminNotFound(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"a"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := svc.SyncApplicationModels(context.Background(), otherToken(), app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("non-owner sync err = %v, want ErrApplicationNotFound", err)
	}
}

// newServerTestServiceWithCipher mirrors newServerTestService but wires a real
// cipher (and optional volatile flag) so SealSecret uses the enc:/plain: branches.
func newServerTestServiceWithCipher(t *testing.T, now time.Time, cipher *capture.Cipher, volatile bool) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(context.Background(), store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, Cipher: cipher, SettingsVolatile: volatile, Clock: func() time.Time { return now }})
	return svc, routeStore
}

// TestApplicationPathSuffixAndTokenRoundTripEnc: with a cipher the app path
// suffix + header round-trip through the DTO, api_token_set flips true, the
// token is sealed (enc:) at rest, and the raw token NEVER appears in the DTO.
func TestApplicationPathSuffixAndTokenRoundTripEnc(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestServiceWithCipher(t, now, newTestCipher(t), false)
	server := createTestServer(t, svc, "S", "s.example.test")

	const rawToken = "sk-secret-xyz-789"
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type:           routing.ProviderVLLM,
		Port:           8000,
		Scheme:         "https",
		AppPathSuffix:  "  v1beta/ ",
		APIToken:       rawToken,
		APITokenHeader: "x-api-key",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if dto.AppPathSuffix != "v1beta/" {
		t.Fatalf("app_path_suffix = %q, want trimmed %q", dto.AppPathSuffix, "v1beta/")
	}
	if !dto.APITokenSet {
		t.Fatalf("api_token_set = false, want true after setting a token")
	}
	if dto.APITokenHeader != "x-api-key" {
		t.Fatalf("api_token_header = %q, want x-api-key", dto.APITokenHeader)
	}
	assertNoRawToken(t, dto, rawToken)

	// Endpoint reflects the app path suffix appended to the origin.
	if dto.Endpoint != "https://s.example.test:8000/v1beta" {
		t.Fatalf("endpoint = %q, want app-path-suffix appended", dto.Endpoint)
	}

	// The token is sealed (enc:) at rest — never stored in plaintext.
	stored, err := routeStore.ApplicationByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	if !strings.HasPrefix(stored.APIToken, "enc:") {
		t.Fatalf("stored APIToken = %q, want enc: prefix (sealed)", stored.APIToken)
	}
	if strings.Contains(stored.APIToken, rawToken) {
		t.Fatalf("stored APIToken leaks the raw token")
	}

	// Reload via GetApplication → same DTO shape, still no raw token.
	got, err := svc.GetApplication(context.Background(), ownerToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if got.AppPathSuffix != "v1beta/" || !got.APITokenSet || got.APITokenHeader != "x-api-key" {
		t.Fatalf("reloaded dto = %#v", got)
	}
	assertNoRawToken(t, got, rawToken)
}

// TestApplicationTokenPlainFallbackVolatile: without a cipher on a volatile
// store, the token is stored plain: (never enc:), api_token_set is still true,
// and the DTO never exposes the raw value.
func TestApplicationTokenPlainFallbackVolatile(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestServiceWithCipher(t, now, nil, true)
	server := createTestServer(t, svc, "S", "s.example.test")

	const rawToken = "sk-plain-abc"
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIToken: rawToken,
	})
	if err != nil {
		t.Fatalf("CreateApplication (volatile): %v", err)
	}
	if !dto.APITokenSet {
		t.Fatalf("api_token_set = false, want true")
	}
	assertNoRawToken(t, dto, rawToken)

	stored, err := routeStore.ApplicationByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	if stored.APIToken != "plain:"+rawToken {
		t.Fatalf("stored APIToken = %q, want plain:%s", stored.APIToken, rawToken)
	}
}

// TestCreateApplicationTokenDiskWithoutKeyRejected: a disk store (non-volatile)
// with no cipher refuses to persist a plaintext token BEFORE any store write.
func TestCreateApplicationTokenDiskWithoutKeyRejected(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithCipher(t, now, nil, false) // no cipher, not volatile
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIToken: "sk-x",
	}); !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("disk-without-key create err = %v, want capture.ErrKeyRequired", err)
	}
}

// TestUpdateApplicationTokenSentinel exercises the keep/clear/replace sentinel on
// the write-only token and the keep/clear semantics on the header.
func TestUpdateApplicationTokenSentinel(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithCipher(t, now, newTestCipher(t), false)
	server := createTestServer(t, svc, "S", "s.example.test")

	const firstToken = "sk-first"
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIToken: firstToken, APITokenHeader: "x-api-key", AppPathSuffix: "a",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if !dto.APITokenSet {
		t.Fatalf("precondition: api_token_set should be true")
	}

	// nil token pointer + a benign field change ⇒ KEEP the stored token.
	kept, err := svc.UpdateApplication(context.Background(), ownerToken(), dto.ID, UpdateApplicationRequest{
		Priority: intPtr(3),
	})
	if err != nil {
		t.Fatalf("UpdateApplication (keep): %v", err)
	}
	if !kept.APITokenSet {
		t.Fatalf("nil token pointer should KEEP the token; api_token_set = false")
	}
	if kept.APITokenHeader != "x-api-key" {
		t.Fatalf("nil header pointer should KEEP the header; got %q", kept.APITokenHeader)
	}

	// value ⇒ REPLACE.
	const secondToken = "sk-second-replaced"
	replaced, err := svc.UpdateApplication(context.Background(), ownerToken(), dto.ID, UpdateApplicationRequest{
		APIToken: strPtr(secondToken),
	})
	if err != nil {
		t.Fatalf("UpdateApplication (replace): %v", err)
	}
	if !replaced.APITokenSet {
		t.Fatalf("value should keep api_token_set true")
	}
	assertNoRawToken(t, replaced, secondToken)
	assertNoRawToken(t, replaced, firstToken)

	// "" ⇒ CLEAR the token; "" ⇒ CLEAR the header.
	cleared, err := svc.UpdateApplication(context.Background(), ownerToken(), dto.ID, UpdateApplicationRequest{
		APIToken: strPtr(""), APITokenHeader: strPtr(""),
	})
	if err != nil {
		t.Fatalf("UpdateApplication (clear): %v", err)
	}
	if cleared.APITokenSet {
		t.Fatalf("empty token should CLEAR; api_token_set = true")
	}
	if cleared.APITokenHeader != "" {
		t.Fatalf("empty header should CLEAR; got %q", cleared.APITokenHeader)
	}
}

func TestApplicationPathAndHeaderValidation(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithCipher(t, now, newTestCipher(t), false)
	server := createTestServer(t, svc, "S", "s.example.test")

	// create: path suffix that looks like a URL is rejected.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", AppPathSuffix: "http://evil.example",
	}); !errors.Is(err, ErrPathSuffixInvalid) {
		t.Fatalf("create bad path err = %v, want ErrPathSuffixInvalid", err)
	}
	// create: illegal header name is rejected.
	if _, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "https", APITokenHeader: "bad header!",
	}); !errors.Is(err, ErrApplicationTokenHeaderInvalid) {
		t.Fatalf("create bad header err = %v, want ErrApplicationTokenHeaderInvalid", err)
	}

	// A valid app to update.
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8002, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	// update: bad path suffix.
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), dto.ID, UpdateApplicationRequest{
		AppPathSuffix: strPtr("ftp://x"),
	}); !errors.Is(err, ErrPathSuffixInvalid) {
		t.Fatalf("update bad path err = %v, want ErrPathSuffixInvalid", err)
	}
	// update: bad header name.
	if _, err := svc.UpdateApplication(context.Background(), ownerToken(), dto.ID, UpdateApplicationRequest{
		APITokenHeader: strPtr("x api key"),
	}); !errors.Is(err, ErrApplicationTokenHeaderInvalid) {
		t.Fatalf("update bad header err = %v, want ErrApplicationTokenHeaderInvalid", err)
	}
}

// assertNoRawToken marshals the DTO to JSON (the wire representation) and fails
// if the raw token appears anywhere — the token value must be write-only.
func assertNoRawToken(t *testing.T, dto ApplicationDTO, rawToken string) {
	t.Helper()
	blob, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("json.Marshal(dto): %v", err)
	}
	if strings.Contains(string(blob), rawToken) {
		t.Fatalf("DTO JSON leaks the raw token: %s", blob)
	}
}
