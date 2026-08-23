// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func seedApplicationsTestServer(t *testing.T, st *SQLiteStore, ctx context.Context, id string, now time.Time) {
	t.Helper()
	if err := st.CreateAIServer(ctx, routing.AIServer{
		ID:           id,
		Name:         "Server " + id,
		Domain:       id + ".example.test",
		Status:       routing.ServerStatusActive,
		HealthStatus: routing.HealthUnknown,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateAIServer(%s) returned %v", id, err)
	}
}

func TestSQLiteApplicationCRUD(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	app := routing.Application{
		ID:                 "app_1",
		ServerID:           "srv_1",
		Type:               routing.ProviderVLLM,
		Port:               8000,
		Scheme:             "https",
		APIFlavors:         []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
		Priority:           1,
		Weight:             2,
		TimeoutMS:          30000,
		AffinityTTLSeconds: 1800,
		Status:             routing.ServerStatusActive,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}

	got, err := st.ApplicationByID(ctx, "app_1")
	if err != nil {
		t.Fatalf("ApplicationByID returned %v", err)
	}
	if got.ServerID != "srv_1" || got.Port != 8000 || got.Scheme != "https" || len(got.APIFlavors) != 2 {
		t.Fatalf("application = %#v", got)
	}
	if got.APIFlavors[0] != routing.APIFlavorOpenAI || got.APIFlavors[1] != routing.APIFlavorAnthropic {
		t.Fatalf("application api flavors = %#v", got.APIFlavors)
	}

	// mutating the returned slice must not corrupt subsequent reads.
	got.APIFlavors[0] = "mutated"
	again, err := st.ApplicationByID(ctx, "app_1")
	if err != nil {
		t.Fatalf("second ApplicationByID returned %v", err)
	}
	if again.APIFlavors[0] != routing.APIFlavorOpenAI {
		t.Fatalf("ApplicationByID returned mutable state: %#v", again.APIFlavors)
	}

	app.Scheme = "http"
	app.Port = 8001
	app.Status = routing.ServerStatusDisabled
	if err := st.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication returned %v", err)
	}
	updated, err := st.ApplicationByID(ctx, "app_1")
	if err != nil {
		t.Fatalf("ApplicationByID after update returned %v", err)
	}
	if updated.Scheme != "http" || updated.Port != 8001 || updated.Status != routing.ServerStatusDisabled {
		t.Fatalf("updated application = %#v", updated)
	}

	if err := st.DeleteApplication(ctx, "app_1"); err != nil {
		t.Fatalf("DeleteApplication returned %v", err)
	}
	if _, err := st.ApplicationByID(ctx, "app_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplicationByID after delete error = %v, want ErrNotFound", err)
	}
	if err := st.DeleteApplication(ctx, "app_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteApplication of missing app error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteApplicationHealthFieldsRoundtripAndDefaults(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	// Explicit non-default values round-trip through create + read.
	app := routing.Application{
		ID:              "app_health",
		ServerID:        "srv_1",
		Type:            routing.ProviderVLLM,
		Port:            8000,
		Scheme:          "https",
		APIFlavors:      []string{routing.APIFlavorOpenAI},
		Status:          routing.ServerStatusActive,
		AlwaysReachable: true,
		HealthCheckPath: "/custom/health",
		HealthCheckMode: routing.HealthCheckModeModelSync,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	got, err := st.ApplicationByID(ctx, "app_health")
	if err != nil {
		t.Fatalf("ApplicationByID returned %v", err)
	}
	if !got.AlwaysReachable || got.HealthCheckPath != "/custom/health" {
		t.Fatalf("health fields not persisted: %#v", got)
	}
	if got.HealthCheckMode != routing.HealthCheckModeModelSync {
		t.Fatalf("health_check_mode not persisted: %q", got.HealthCheckMode)
	}

	// Update flips both fields and they round-trip.
	app.AlwaysReachable = false
	app.HealthCheckPath = "/v1/ready"
	if err := st.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication returned %v", err)
	}
	updated, err := st.ApplicationByID(ctx, "app_health")
	if err != nil {
		t.Fatalf("ApplicationByID after update returned %v", err)
	}
	if updated.AlwaysReachable || updated.HealthCheckPath != "/v1/ready" {
		t.Fatalf("updated health fields = %#v", updated)
	}

	// ApplicationsByServer also hydrates the new fields.
	list, err := st.ApplicationsByServer(ctx, "srv_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ApplicationsByServer = %#v err=%v", list, err)
	}
	if list[0].HealthCheckPath != "/v1/ready" {
		t.Fatalf("ApplicationsByServer health path = %q", list[0].HealthCheckPath)
	}
}

func TestSQLiteApplicationLoadedModelsRoundtripAndDefaults(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	// Default (unset) round-trips as empty strings.
	def := routing.Application{
		ID: "app_def", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateApplication(ctx, def); err != nil {
		t.Fatalf("CreateApplication default: %v", err)
	}
	gotDef, err := st.ApplicationByID(ctx, "app_def")
	if err != nil {
		t.Fatalf("ApplicationByID default: %v", err)
	}
	if gotDef.LoadedModelsPath != "" || gotDef.LoadedModelsFormat != "" || gotDef.ContextProbePath != "" || gotDef.CapacityProbePath != "" {
		t.Fatalf("unset probe fields = %q/%q/%q/%q, want empty", gotDef.LoadedModelsPath, gotDef.LoadedModelsFormat, gotDef.ContextProbePath, gotDef.CapacityProbePath)
	}

	// Explicit values round-trip through create + read + list.
	app := routing.Application{
		ID: "app_loaded", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		LoadedModelsPath: "/running", LoadedModelsFormat: "llama_swap", ContextProbePath: "/props", CapacityProbePath: "/metrics",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	got, err := st.ApplicationByID(ctx, "app_loaded")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	if got.LoadedModelsPath != "/running" || got.LoadedModelsFormat != "llama_swap" || got.ContextProbePath != "/props" || got.CapacityProbePath != "/metrics" {
		t.Fatalf("probe fields not persisted: %#v", got)
	}

	// Update changes all of them and they round-trip.
	app.LoadedModelsPath = "/props"
	app.LoadedModelsFormat = "llama_cpp"
	app.ContextProbePath = "/slots"
	app.CapacityProbePath = "/props"
	if err := st.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	updated, err := st.ApplicationByID(ctx, "app_loaded")
	if err != nil {
		t.Fatalf("ApplicationByID after update: %v", err)
	}
	if updated.LoadedModelsPath != "/props" || updated.LoadedModelsFormat != "llama_cpp" || updated.ContextProbePath != "/slots" || updated.CapacityProbePath != "/props" {
		t.Fatalf("updated probe fields = %#v", updated)
	}

	// ApplicationsByServer hydrates the fields too.
	list, err := st.ApplicationsByServer(ctx, "srv_1")
	if err != nil {
		t.Fatalf("ApplicationsByServer: %v", err)
	}
	for _, a := range list {
		if a.ID == "app_loaded" && (a.LoadedModelsPath != "/props" || a.LoadedModelsFormat != "llama_cpp" || a.ContextProbePath != "/slots" || a.CapacityProbePath != "/props") {
			t.Fatalf("ApplicationsByServer probe fields = %q/%q/%q/%q", a.LoadedModelsPath, a.LoadedModelsFormat, a.ContextProbePath, a.CapacityProbePath)
		}
	}
}

func TestSQLiteApplicationBenchmarkModesRoundtripAndDefaults(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	// Default (unset) round-trips as the zero "feature off" values.
	def := routing.Application{
		ID: "app_def", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateApplication(ctx, def); err != nil {
		t.Fatalf("CreateApplication default: %v", err)
	}
	gotDef, err := st.ApplicationByID(ctx, "app_def")
	if err != nil {
		t.Fatalf("ApplicationByID default: %v", err)
	}
	if gotDef.BenchmarkScheduleEnabled || gotDef.BenchmarkScheduleIntervalSeconds != 0 || gotDef.OpportunisticMetricsEnabled {
		t.Fatalf("unset benchmark-mode fields = %v/%d/%v, want false/0/false",
			gotDef.BenchmarkScheduleEnabled, gotDef.BenchmarkScheduleIntervalSeconds, gotDef.OpportunisticMetricsEnabled)
	}

	// Explicit values round-trip through create + read.
	app := routing.Application{
		ID: "app_bench", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		BenchmarkScheduleEnabled: true, BenchmarkScheduleIntervalSeconds: 3600, OpportunisticMetricsEnabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	got, err := st.ApplicationByID(ctx, "app_bench")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	if !got.BenchmarkScheduleEnabled || got.BenchmarkScheduleIntervalSeconds != 3600 || !got.OpportunisticMetricsEnabled {
		t.Fatalf("benchmark-mode fields not persisted: %#v", got)
	}

	// Update changes all of them and they round-trip.
	app.BenchmarkScheduleEnabled = false
	app.BenchmarkScheduleIntervalSeconds = 900
	app.OpportunisticMetricsEnabled = false
	if err := st.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	updated, err := st.ApplicationByID(ctx, "app_bench")
	if err != nil {
		t.Fatalf("ApplicationByID after update: %v", err)
	}
	if updated.BenchmarkScheduleEnabled || updated.BenchmarkScheduleIntervalSeconds != 900 || updated.OpportunisticMetricsEnabled {
		t.Fatalf("updated benchmark-mode fields = %#v", updated)
	}

	// ApplicationsByServer hydrates the fields too.
	list, err := st.ApplicationsByServer(ctx, "srv_1")
	if err != nil {
		t.Fatalf("ApplicationsByServer: %v", err)
	}
	for _, a := range list {
		if a.ID == "app_bench" && (a.BenchmarkScheduleEnabled || a.BenchmarkScheduleIntervalSeconds != 900 || a.OpportunisticMetricsEnabled) {
			t.Fatalf("ApplicationsByServer benchmark-mode = %v/%d/%v", a.BenchmarkScheduleEnabled, a.BenchmarkScheduleIntervalSeconds, a.OpportunisticMetricsEnabled)
		}
	}
}

func TestSQLiteApplicationHealthIntervalRoundtripAndDefault(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	// A custom per-application interval round-trips through create + read.
	app := routing.Application{
		ID:                         "app_interval",
		ServerID:                   "srv_1",
		Type:                       routing.ProviderVLLM,
		Port:                       8000,
		Scheme:                     "https",
		APIFlavors:                 []string{routing.APIFlavorOpenAI},
		Status:                     routing.ServerStatusActive,
		HealthCheckIntervalSeconds: 45,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	got, err := st.ApplicationByID(ctx, "app_interval")
	if err != nil {
		t.Fatalf("ApplicationByID returned %v", err)
	}
	if got.HealthCheckIntervalSeconds != 45 {
		t.Fatalf("health_check_interval_seconds = %d, want 45", got.HealthCheckIntervalSeconds)
	}
	list, err := st.ApplicationsByServer(ctx, "srv_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ApplicationsByServer = %#v err=%v", list, err)
	}
	if list[0].HealthCheckIntervalSeconds != 45 {
		t.Fatalf("ApplicationsByServer health_check_interval_seconds = %d, want 45", list[0].HealthCheckIntervalSeconds)
	}

	// Update changes the interval and it round-trips.
	app.HealthCheckIntervalSeconds = 5
	if err := st.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication returned %v", err)
	}
	updated, err := st.ApplicationByID(ctx, "app_interval")
	if err != nil {
		t.Fatalf("ApplicationByID after update returned %v", err)
	}
	if updated.HealthCheckIntervalSeconds != 5 {
		t.Fatalf("updated health_check_interval_seconds = %d, want 5", updated.HealthCheckIntervalSeconds)
	}

	// A create that omits the interval reads back the 0 default.
	dflt := routing.Application{
		ID: "app_default", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateApplication(ctx, dflt); err != nil {
		t.Fatalf("CreateApplication (default) returned %v", err)
	}
	gotDflt, err := st.ApplicationByID(ctx, "app_default")
	if err != nil {
		t.Fatalf("ApplicationByID (default) returned %v", err)
	}
	if gotDflt.HealthCheckIntervalSeconds != 0 {
		t.Fatalf("default health_check_interval_seconds = %d, want 0", gotDflt.HealthCheckIntervalSeconds)
	}
}

func TestSQLiteApplicationHealthColumnMigrationDefaults(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	// Simulate a row written before the always_reachable/health_check_path
	// columns existed by inserting only the original column set; the ALTER
	// defaults (0 / '/v1/health') must fill the new columns.
	if _, err := st.db.ExecContext(ctx, `insert into applications (
		id, server_id, type, port, scheme, api_flavors, priority, weight,
		timeout_ms, affinity_ttl_seconds, status, created_at, updated_at
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app_legacy", "srv_1", routing.ProviderVLLM, 8000, "https", `["openai"]`,
		0, 0, 30000, 1800, routing.ServerStatusActive, now, now); err != nil {
		t.Fatalf("insert legacy application row: %v", err)
	}

	got, err := st.ApplicationByID(ctx, "app_legacy")
	if err != nil {
		t.Fatalf("ApplicationByID returned %v", err)
	}
	if got.AlwaysReachable {
		t.Fatalf("always_reachable default = true, want false")
	}
	if got.HealthCheckPath != "/v1/health" {
		t.Fatalf("health_check_path default = %q, want /v1/health", got.HealthCheckPath)
	}
	// The health_check_mode column defaults to empty for legacy rows; the domain
	// helper derives the effective mode from always_reachable (here: health_path).
	if got.HealthCheckMode != "" {
		t.Fatalf("health_check_mode default = %q, want empty (legacy row)", got.HealthCheckMode)
	}
	if mode := routing.EffectiveHealthCheckMode(got); mode != routing.HealthCheckModeHealthPath {
		t.Fatalf("effective mode = %q, want health_path", mode)
	}
}

func TestSQLiteApplicationDuplicatePortConflict(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	first := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, first); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	dup := first
	dup.ID = "app_dup"
	err := st.CreateApplication(ctx, dup)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate port CreateApplication error = %v, want ErrConflict", err)
	}

	// A different server may reuse the same port.
	seedApplicationsTestServer(t, st, ctx, "srv_2", now)
	other := first
	other.ID = "app_other_server"
	other.ServerID = "srv_2"
	if err := st.CreateApplication(ctx, other); err != nil {
		t.Fatalf("CreateApplication on other server returned %v", err)
	}

	// UpdateApplication onto an already-taken port on the same server also
	// conflicts.
	second := routing.Application{ID: "app_2", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8001, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, second); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	second.Port = 8000
	if err := st.UpdateApplication(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateApplication onto taken port error = %v, want ErrConflict", err)
	}
}

func TestSQLiteApplicationUnknownServerNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

	app := routing.Application{ID: "app_missing_server", ServerID: "srv_missing", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateApplication with unknown server error = %v, want ErrNotFound", err)
	}

	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app.ServerID = "srv_1"
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	app.ServerID = "srv_missing"
	if err := st.UpdateApplication(ctx, app); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateApplication with unknown server error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteApplicationConflictVsNotFoundDistinction(t *testing.T) {
	// This test locks in store parity with MemoryStore: a UNIQUE(server_id,
	// port) violation must map to ErrConflict, while a FOREIGN KEY violation
	// on server_id must map to ErrNotFound, even though both are raised by
	// SQLite as "constraint failed" errors under isSQLiteConstraintError.
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)

	base := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, base); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}

	dupPort := base
	dupPort.ID = "app_dup_port"
	if err := st.CreateApplication(ctx, dupPort); !errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate port error = %v, want ErrConflict (and not ErrNotFound)", err)
	}

	noServer := base
	noServer.ID = "app_no_server"
	noServer.ServerID = "srv_missing"
	noServer.Port = 9999
	if err := st.CreateApplication(ctx, noServer); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		t.Fatalf("missing server error = %v, want ErrNotFound (and not ErrConflict)", err)
	}
}

func TestSQLiteApplicationsByServer(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	seedApplicationsTestServer(t, st, ctx, "srv_2", now)

	for _, a := range []routing.Application{
		{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "app_2", ServerID: "srv_1", Type: routing.ProviderOllama, Port: 8001, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "app_3", ServerID: "srv_2", Type: routing.ProviderOllama, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.CreateApplication(ctx, a); err != nil {
			t.Fatalf("CreateApplication(%s) returned %v", a.ID, err)
		}
	}

	apps, err := st.ApplicationsByServer(ctx, "srv_1")
	if err != nil {
		t.Fatalf("ApplicationsByServer returned %v", err)
	}
	if len(apps) != 2 || apps[0].ID != "app_1" || apps[1].ID != "app_2" {
		t.Fatalf("apps = %#v, want ordered [app_1 app_2]", apps)
	}

	none, err := st.ApplicationsByServer(ctx, "srv_missing")
	if err != nil {
		t.Fatalf("ApplicationsByServer(missing) returned %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("apps for missing server = %#v, want empty", none)
	}
}

func TestSQLiteMappingCRUD(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}

	mapping := routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMapping(ctx, mapping); err != nil {
		t.Fatalf("CreateMapping returned %v", err)
	}

	got, err := st.MappingByID(ctx, "map_1")
	if err != nil {
		t.Fatalf("MappingByID returned %v", err)
	}
	if got.ApplicationID != "app_1" || got.GatewayModelName != "qwen" || got.AppModelName != "qwen2.5" {
		t.Fatalf("mapping = %#v", got)
	}

	mapping.AppModelName = "qwen2.5-instruct"
	mapping.Status = routing.ServerStatusDisabled
	if err := st.UpdateMapping(ctx, mapping); err != nil {
		t.Fatalf("UpdateMapping returned %v", err)
	}
	updated, err := st.MappingByID(ctx, "map_1")
	if err != nil {
		t.Fatalf("MappingByID after update returned %v", err)
	}
	if updated.AppModelName != "qwen2.5-instruct" || updated.Status != routing.ServerStatusDisabled {
		t.Fatalf("updated mapping = %#v", updated)
	}

	if err := st.DeleteMapping(ctx, "map_1"); err != nil {
		t.Fatalf("DeleteMapping returned %v", err)
	}
	if _, err := st.MappingByID(ctx, "map_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MappingByID after delete error = %v, want ErrNotFound", err)
	}
	if err := st.DeleteMapping(ctx, "map_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteMapping of missing mapping error = %v, want ErrNotFound", err)
	}
}

// TestSQLiteMappingConcurrencyCapacity verifies the per-mapping concurrency-capacity
// metric columns (migration v13): they default to 0 when a mapping is created without
// them, round-trip when set on create, and persist a change through UpdateMapping —
// read back via both MappingByID and MappingsByApplication.
func TestSQLiteMappingConcurrencyCapacity(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}

	// Created without the new fields -> defaults are 0.
	bare := routing.ModelMapping{ID: "map_bare", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMapping(ctx, bare); err != nil {
		t.Fatalf("CreateMapping(bare) returned %v", err)
	}
	gotBare, err := st.MappingByID(ctx, "map_bare")
	if err != nil {
		t.Fatalf("MappingByID(bare) returned %v", err)
	}
	if gotBare.MaxConcurrency != 0 || gotBare.RecommendedConcurrency != 0 || gotBare.GenTokensPerSecondAtCapacity != 0 {
		t.Fatalf("bare mapping capacity metrics = %d/%d/%v, want all 0", gotBare.MaxConcurrency, gotBare.RecommendedConcurrency, gotBare.GenTokensPerSecondAtCapacity)
	}

	// Set on create -> round-trips via MappingByID and MappingsByApplication.
	set := routing.ModelMapping{
		ID: "map_set", ApplicationID: "app_1", GatewayModelName: "llama", AppModelName: "llama3", Status: routing.ServerStatusActive,
		MaxConcurrency:               16,
		RecommendedConcurrency:       12,
		GenTokensPerSecondAtCapacity: 640.5,
		CreatedAt:                    now, UpdatedAt: now,
	}
	if err := st.CreateMapping(ctx, set); err != nil {
		t.Fatalf("CreateMapping(set) returned %v", err)
	}
	gotSet, err := st.MappingByID(ctx, "map_set")
	if err != nil {
		t.Fatalf("MappingByID(set) returned %v", err)
	}
	if gotSet.MaxConcurrency != 16 || gotSet.RecommendedConcurrency != 12 || gotSet.GenTokensPerSecondAtCapacity != 640.5 {
		t.Fatalf("set mapping capacity metrics = %d/%d/%v, want 16/12/640.5", gotSet.MaxConcurrency, gotSet.RecommendedConcurrency, gotSet.GenTokensPerSecondAtCapacity)
	}

	byApp, err := st.MappingsByApplication(ctx, "app_1")
	if err != nil {
		t.Fatalf("MappingsByApplication returned %v", err)
	}
	var found bool
	for _, m := range byApp {
		if m.ID == "map_set" {
			found = true
			if m.MaxConcurrency != 16 || m.RecommendedConcurrency != 12 || m.GenTokensPerSecondAtCapacity != 640.5 {
				t.Fatalf("MappingsByApplication set metrics = %d/%d/%v, want 16/12/640.5", m.MaxConcurrency, m.RecommendedConcurrency, m.GenTokensPerSecondAtCapacity)
			}
		}
	}
	if !found {
		t.Fatalf("map_set not returned by MappingsByApplication: %#v", byApp)
	}

	// Changed on update -> persists.
	set.MaxConcurrency = 32
	set.RecommendedConcurrency = 24
	set.GenTokensPerSecondAtCapacity = 900
	set.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateMapping(ctx, set); err != nil {
		t.Fatalf("UpdateMapping returned %v", err)
	}
	updated, err := st.MappingByID(ctx, "map_set")
	if err != nil {
		t.Fatalf("MappingByID after update returned %v", err)
	}
	if updated.MaxConcurrency != 32 || updated.RecommendedConcurrency != 24 || updated.GenTokensPerSecondAtCapacity != 900 {
		t.Fatalf("updated capacity metrics = %d/%d/%v, want 32/24/900", updated.MaxConcurrency, updated.RecommendedConcurrency, updated.GenTokensPerSecondAtCapacity)
	}
}

func TestSQLiteMappingUnknownApplicationNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

	mapping := routing.ModelMapping{ID: "map_missing_app", ApplicationID: "app_missing", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMapping(ctx, mapping); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateMapping with unknown application error = %v, want ErrNotFound", err)
	}

	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	mapping.ApplicationID = "app_1"
	if err := st.CreateMapping(ctx, mapping); err != nil {
		t.Fatalf("CreateMapping returned %v", err)
	}
	mapping.ApplicationID = "app_missing"
	if err := st.UpdateMapping(ctx, mapping); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateMapping with unknown application error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteMappingsByApplicationAndServer(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	seedApplicationsTestServer(t, st, ctx, "srv_2", now)

	apps := []routing.Application{
		{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "app_2", ServerID: "srv_1", Type: routing.ProviderOllama, Port: 8001, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "app_3", ServerID: "srv_2", Type: routing.ProviderOllama, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
	}
	for _, a := range apps {
		if err := st.CreateApplication(ctx, a); err != nil {
			t.Fatalf("CreateApplication(%s) returned %v", a.ID, err)
		}
	}

	mappings := []routing.ModelMapping{
		{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "map_2", ApplicationID: "app_1", GatewayModelName: "llama", AppModelName: "llama3", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "map_3", ApplicationID: "app_2", GatewayModelName: "mistral", AppModelName: "mistral-7b", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "map_4", ApplicationID: "app_3", GatewayModelName: "phi", AppModelName: "phi-3", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
	}
	for _, m := range mappings {
		if err := st.CreateMapping(ctx, m); err != nil {
			t.Fatalf("CreateMapping(%s) returned %v", m.ID, err)
		}
	}

	byApp, err := st.MappingsByApplication(ctx, "app_1")
	if err != nil {
		t.Fatalf("MappingsByApplication returned %v", err)
	}
	if len(byApp) != 2 || byApp[0].ID != "map_1" || byApp[1].ID != "map_2" {
		t.Fatalf("byApp = %#v, want ordered [map_1 map_2]", byApp)
	}

	byServer, err := st.MappingsByServer(ctx, "srv_1")
	if err != nil {
		t.Fatalf("MappingsByServer returned %v", err)
	}
	if len(byServer) != 3 {
		t.Fatalf("byServer = %#v, want 3 mappings across srv_1's apps", byServer)
	}
	for i := 1; i < len(byServer); i++ {
		if byServer[i-1].ID >= byServer[i].ID {
			t.Fatalf("byServer not ordered by id: %#v", byServer)
		}
	}

	byServer2, err := st.MappingsByServer(ctx, "srv_2")
	if err != nil {
		t.Fatalf("MappingsByServer(srv_2) returned %v", err)
	}
	if len(byServer2) != 1 || byServer2[0].ID != "map_4" {
		t.Fatalf("byServer2 = %#v, want [map_4]", byServer2)
	}
}

func TestSQLiteDeleteApplicationCascadesMappings(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	mapping := routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMapping(ctx, mapping); err != nil {
		t.Fatalf("CreateMapping returned %v", err)
	}

	if err := st.DeleteApplication(ctx, "app_1"); err != nil {
		t.Fatalf("DeleteApplication returned %v", err)
	}

	if _, err := st.MappingByID(ctx, "map_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mapping should be cascade-deleted with its application, MappingByID error = %v", err)
	}
	byApp, err := st.MappingsByApplication(ctx, "app_1")
	if err != nil {
		t.Fatalf("MappingsByApplication returned %v", err)
	}
	if len(byApp) != 0 {
		t.Fatalf("mappings should cascade on app delete, got %#v", byApp)
	}
}

func TestSQLiteDeleteAIServerCascadesApplicationsAndMappings(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	seedApplicationsTestServer(t, st, ctx, "srv_1", now)
	app := routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	mapping := routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMapping(ctx, mapping); err != nil {
		t.Fatalf("CreateMapping returned %v", err)
	}

	if err := st.DeleteAIServer(ctx, "srv_1"); err != nil {
		t.Fatalf("DeleteAIServer returned %v", err)
	}

	if _, err := st.ApplicationByID(ctx, "app_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("application should be cascade-deleted with its server, ApplicationByID error = %v", err)
	}
	if _, err := st.MappingByID(ctx, "map_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mapping should be cascade-deleted with its server, MappingByID error = %v", err)
	}
}

func TestSQLiteActiveMappingsForModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	s := openMigratedTestSQLite(t)
	defer s.Close()
	if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Provider: routing.ProviderVLLM, Endpoint: "", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{ID: "app_ok", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication ok: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{ID: "app_anthropic", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8001, Scheme: "https", APIFlavors: []string{routing.APIFlavorAnthropic}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication anthropic: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{ID: "app_disabled", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8002, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication disabled: %v", err)
	}
	if err := s.CreateMapping(ctx, routing.ModelMapping{ID: "map_ok", ApplicationID: "app_ok", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping ok: %v", err)
	}
	if err := s.CreateMapping(ctx, routing.ModelMapping{ID: "map_off", ApplicationID: "app_ok", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5-off", Status: routing.ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping off: %v", err)
	}
	if err := s.CreateMapping(ctx, routing.ModelMapping{ID: "map_anthropic", ApplicationID: "app_anthropic", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping anthropic: %v", err)
	}
	// active mapping on a disabled application (excluded by the a.status predicate)
	if err := s.CreateMapping(ctx, routing.ModelMapping{ID: "map_disabled_app", ApplicationID: "app_disabled", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping disabled app: %v", err)
	}

	got, err := s.ActiveMappingsForModel(ctx, "qwen-coder", routing.APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ActiveMappingsForModel: %v", err)
	}
	if len(got) != 1 || got[0].Mapping.ID != "map_ok" || got[0].Application.ID != "app_ok" || got[0].Server.ID != "srv_1" {
		t.Fatalf("candidates = %#v", got)
	}
	if got[0].Application.Scheme != "https" || got[0].Mapping.AppModelName != "qwen2.5" {
		t.Fatalf("candidate fields not hydrated: %#v", got[0])
	}
}
