// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// fakeLoadedModels is a test LoadedModelReader: an appID maps to the set of
// upstream model names currently loaded for it (serverID is ignored). Any other
// appID reports nothing loaded. Mirrors the *gateway.LoadedModelRegistry contract
// without importing internal/gateway (which imports internal/portal -> cycle).
type fakeLoadedModels struct {
	byApp map[string][]string
}

func (f fakeLoadedModels) LoadedAppModels(appID, _ string) []string {
	return f.byApp[appID]
}

// newModelServersTestService builds a Service backed by an in-memory routing
// store and the given (optional) loaded-model reader. It returns the service and
// the store so tests can seed servers/apps/mappings directly.
func newModelServersTestService(t *testing.T, now time.Time, loaded LoadedModelReader) (*Service, *routing.MemoryStore) {
	t.Helper()
	routeStore := routing.NewMemoryStore()
	svc := newModelServersTestServiceWithRoutes(t, now, loaded, routeStore)
	return svc, routeStore
}

// newModelServersTestServiceWithRoutes mirrors newModelServersTestService but
// takes the routing.Store directly, so a test can wrap it first (e.g.
// countingCapabilityStore, the N+1 guard's instrument) before wiring it into
// the Service.
func newModelServersTestServiceWithRoutes(t *testing.T, now time.Time, loaded LoadedModelReader, routes routing.Store) *Service {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(context.Background(), store.User{ID: "usr_admin", Email: "admin@example.test", DisplayName: "admin", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return NewService(ServiceDeps{Users: dir, Routes: routes, LoadedModels: loaded, Clock: func() time.Time { return now }})
}

// seedOffering creates an active server (name == serverID) with one active
// application and one active mapping (gatewayModel -> appModel) carrying genTPS.
func seedOffering(t *testing.T, routeStore *routing.MemoryStore, now time.Time, serverID, appID, mappingID, gatewayModel, appModel string, genTPS float64) {
	t.Helper()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: appModel, Status: routing.ServerStatusActive, GenTokensPerSecond: genTPS, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
}

// ModelServers returns one row per (server, mapping) that offers the model,
// sorted by server name, enriched with live loaded-state and can_load (admin =
// true for every row). An unknown model resolves to an empty slice.
func TestModelServers(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	loaded := fakeLoadedModels{byApp: map[string][]string{"app-a": {"up-a"}}}
	svc, routeStore := newModelServersTestService(t, now, loaded)
	seedOffering(t, routeStore, now, "srv-a", "app-a", "map-a", "shared", "up-a", 42)
	seedOffering(t, routeStore, now, "srv-b", "app-b", "map-b", "shared", "up-b", 0)

	rows, err := svc.ModelServers(context.Background(), adminToken(), "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (%+v)", len(rows), rows)
	}
	if rows[0].ServerName != "srv-a" || rows[1].ServerName != "srv-b" {
		t.Fatalf("rows not sorted by ServerName: %q, %q", rows[0].ServerName, rows[1].ServerName)
	}
	if !rows[0].Loaded {
		t.Fatalf("rows[0].Loaded = false, want true (up-a loaded on app-a)")
	}
	if rows[1].Loaded {
		t.Fatalf("rows[1].Loaded = true, want false (up-b not loaded)")
	}
	if !rows[0].CanLoad || !rows[1].CanLoad {
		t.Fatalf("admin CanLoad = (%v, %v), want (true, true)", rows[0].CanLoad, rows[1].CanLoad)
	}
	if rows[0].GenTokensPerSecond != 42 {
		t.Fatalf("rows[0].GenTokensPerSecond = %v, want 42", rows[0].GenTokensPerSecond)
	}
	if rows[0].ServerID != "srv-a" || rows[0].ApplicationID != "app-a" || rows[0].MappingID != "map-a" {
		t.Fatalf("rows[0] identity = (%q, %q, %q), want (srv-a, app-a, map-a)", rows[0].ServerID, rows[0].ApplicationID, rows[0].MappingID)
	}

	empty, err := svc.ModelServers(context.Background(), adminToken(), "does-not-exist")
	if err != nil {
		t.Fatalf("ModelServers(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown model rows = %d, want 0", len(empty))
	}
}

// can_load is per-row: for a non-admin, true only on a server the caller owns.
// The row SET stays global (both servers appear regardless of ownership).
func TestModelServersCanLoadOwnership(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-a", "app-a", "map-a", "shared", "up-a", 0)
	seedOffering(t, routeStore, now, "srv-b", "app-b", "map-b", "shared", "up-b", 0)
	if err := routeStore.SetServerOwners(context.Background(), "srv-a", []string{"user-1"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}

	principal := auth.Token{UserID: "user-1", Scopes: []string{"gateway:use"}}
	rows, err := svc.ModelServers(context.Background(), principal, "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (global, not owner-filtered)", len(rows))
	}
	byServer := map[string]ModelServerDTO{}
	for _, r := range rows {
		byServer[r.ServerID] = r
	}
	if !byServer["srv-a"].CanLoad {
		t.Fatalf("owner's server-a CanLoad = false, want true")
	}
	if byServer["srv-b"].CanLoad {
		t.Fatalf("non-owned server-b CanLoad = true, want false")
	}
}

// TestModelServersHiddenLockedSuppression: a non-admin gateway:use principal
// gets an EMPTY slice from ModelServers for a hidden or locked model --
// exactly the same suppression Models() already applies to the top-level
// listing, now closing the by-name detail-view leak too (security fix; see
// the VISIBILITY-SURFACE MATRIX doc-comment on visibleMappingViews in
// service.go). A shown model is unaffected. An admin bypasses the
// suppression entirely -- the ModelServersSection management flow, same as
// ManageModels() -- and sees every row regardless of visibility.
func TestModelServersHiddenLockedSuppression(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-hidden", "app-hidden", "map-hidden", "hidden-model", "hidden-up", 0)
	seedOffering(t, routeStore, now, "srv-locked", "app-locked", "map-locked", "locked-model", "locked-up", 0)
	seedOffering(t, routeStore, now, "srv-shown", "app-shown", "map-shown", "shown-model", "shown-up", 0)
	if err := routeStore.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: "hidden-model", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting hidden: %v", err)
	}
	if err := routeStore.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: "locked-model", Visibility: "locked", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting locked: %v", err)
	}

	nonAdmin := auth.Token{UserID: "usr_plain_ms", Scopes: []string{"gateway:use"}}

	rowsHidden, err := svc.ModelServers(context.Background(), nonAdmin, "hidden-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, hidden-model): %v", err)
	}
	if len(rowsHidden) != 0 {
		t.Fatalf("ModelServers(non-admin, hidden-model) = %+v, want empty", rowsHidden)
	}

	rowsLocked, err := svc.ModelServers(context.Background(), nonAdmin, "locked-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, locked-model): %v", err)
	}
	if len(rowsLocked) != 0 {
		t.Fatalf("ModelServers(non-admin, locked-model) = %+v, want empty", rowsLocked)
	}

	rowsShown, err := svc.ModelServers(context.Background(), nonAdmin, "shown-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, shown-model): %v", err)
	}
	if len(rowsShown) != 1 {
		t.Fatalf("ModelServers(non-admin, shown-model) = %+v, want 1 row (unaffected)", rowsShown)
	}

	admin := adminToken()
	rowsHiddenAdmin, err := svc.ModelServers(context.Background(), admin, "hidden-model")
	if err != nil {
		t.Fatalf("ModelServers(admin, hidden-model): %v", err)
	}
	if len(rowsHiddenAdmin) != 1 {
		t.Fatalf("ModelServers(admin, hidden-model) = %+v, want 1 row (admin bypass, unfiltered)", rowsHiddenAdmin)
	}

	rowsLockedAdmin, err := svc.ModelServers(context.Background(), admin, "locked-model")
	if err != nil {
		t.Fatalf("ModelServers(admin, locked-model): %v", err)
	}
	if len(rowsLockedAdmin) != 1 {
		t.Fatalf("ModelServers(admin, locked-model) = %+v, want 1 row (admin bypass, unfiltered)", rowsLockedAdmin)
	}
}

// seedMappingLiveProgress stamps a mapping's persisted live-progress verdict
// through UpdateMapping (the full-row writer), standing in for the targeted
// writer #49-3 removed when every probe moved onto
// model_mapping_capabilities rows. The DTO fill under test reads the column
// either way, which is the seam these tests pin.
func seedMappingLiveProgress(t *testing.T, routeStore *routing.MemoryStore, mappingID, support string, at time.Time) {
	t.Helper()
	verdict := routing.LiveProgressCapabilityVerdict(support)
	if verdict == "" {
		t.Fatalf("seedMappingLiveProgress(%s): support %q has no capability verdict", mappingID, support)
	}
	if err := routeStore.UpsertMappingCapabilities(context.Background(), mappingID, []routing.CapabilityRow{{
		Capability: routing.CapabilityLiveProgress, Verdict: verdict,
		Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: at,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities(%s): %v", mappingID, err)
	}
}

// seedMappingLiveProgressColumn writes the FROZEN
// ModelMapping.LiveProgressSupport/LiveProgressCheckedAt columns and nothing
// else -- migration 78's snapshot, with no capability row behind it. Only the
// "the DTO must NOT read this any more" case uses it.
func seedMappingLiveProgressColumn(t *testing.T, routeStore *routing.MemoryStore, mappingID, support string, at time.Time) {
	t.Helper()
	mapping, err := routeStore.MappingByID(context.Background(), mappingID)
	if err != nil {
		t.Fatalf("MappingByID(%s): %v", mappingID, err)
	}
	mapping.LiveProgressSupport = support
	mapping.LiveProgressCheckedAt = &at
	if err := routeStore.UpdateMapping(context.Background(), mapping); err != nil {
		t.Fatalf("UpdateMapping(%s): %v", mappingID, err)
	}
}

// TestModelServersLiveProgressSupportPersisted: LiveProgressSupport/
// LiveProgressCheckedAt are read from the mapping's "live_progress"
// CAPABILITY ROW (out of the same single batch the Capabilities array already
// costs), NOT from the frozen ModelMapping.LiveProgressSupport column -- and
// NOT left zero/empty for a gateway-layer injection pass the way
// State/ActiveRequests/QueueDepth/MetricsProbe/ContextProbe are. One row per
// verdict, including the "never determined" default (no write at all), so a
// dropped fill in ModelServers (leaving the DTO field at its Go zero value)
// cannot coincidentally satisfy this: the seeded "" row must ALSO carry a nil
// CheckedAt, which only holds if the fill genuinely reads the store.
//
// The "frozen" row is the reason this test moved onto rows at all: #49-3
// retired every writer of that column, so a DTO still reading it would pin
// this portal column to migration 78's snapshot -- an em-dash forever for
// every mapping created afterwards. That row has the column set and NO row,
// and must report "" / nil.
func TestModelServersLiveProgressSupportPersisted(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-supported", "app-supported", "map-supported", "shared", "up-supported", 0)
	seedOffering(t, routeStore, now, "srv-unsupported", "app-unsupported", "map-unsupported", "shared", "up-unsupported", 0)
	seedOffering(t, routeStore, now, "srv-unknown", "app-unknown", "map-unknown", "shared", "up-unknown", 0)
	seedOffering(t, routeStore, now, "srv-frozen", "app-frozen", "map-frozen", "shared", "up-frozen", 0)
	seedOffering(t, routeStore, now, "srv-timeless", "app-timeless", "map-timeless", "shared", "up-timeless", 0)

	// Seeded as capability ROWS, which is where #49-3's detectors write now.
	checkedAt := now.Add(-time.Hour)
	seedMappingLiveProgress(t, routeStore, "map-supported", "supported", checkedAt)
	seedMappingLiveProgress(t, routeStore, "map-unsupported", "unsupported", checkedAt)
	// map-unknown is not seeded at all: "never determined" is the absence of
	// a row, not a write with an empty verdict.
	// map-frozen carries migration 78's COLUMN value and no row -- the DTO
	// must ignore it entirely (that column has no writer any more).
	seedMappingLiveProgressColumn(t, routeStore, "map-frozen", "supported", checkedAt)
	// map-timeless has a real verdict but no timestamp: the DTO must report
	// the verdict and a NIL checked-at, never Go's zero time.Time (which the
	// portal would render as a year-0001 "determined at").
	if err := routeStore.UpsertMappingCapabilities(context.Background(), "map-timeless", []routing.CapabilityRow{{
		Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityYes,
		Source: routing.CapabilitySourceLlamaCppProps,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities(map-timeless): %v", err)
	}

	rows, err := svc.ModelServers(context.Background(), adminToken(), "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("len(rows) = %d, want 5 (%+v)", len(rows), rows)
	}
	byServer := map[string]ModelServerDTO{}
	for _, r := range rows {
		byServer[r.ServerID] = r
	}

	supported := byServer["srv-supported"]
	if supported.LiveProgressSupport != "supported" {
		t.Fatalf("supported row LiveProgressSupport = %q, want \"supported\"", supported.LiveProgressSupport)
	}
	if supported.LiveProgressCheckedAt == nil || !supported.LiveProgressCheckedAt.Equal(checkedAt) {
		t.Fatalf("supported row LiveProgressCheckedAt = %v, want %v", supported.LiveProgressCheckedAt, checkedAt)
	}

	unsupported := byServer["srv-unsupported"]
	if unsupported.LiveProgressSupport != "unsupported" {
		t.Fatalf("unsupported row LiveProgressSupport = %q, want \"unsupported\"", unsupported.LiveProgressSupport)
	}
	if unsupported.LiveProgressCheckedAt == nil || !unsupported.LiveProgressCheckedAt.Equal(checkedAt) {
		t.Fatalf("unsupported row LiveProgressCheckedAt = %v, want %v", unsupported.LiveProgressCheckedAt, checkedAt)
	}

	unknown := byServer["srv-unknown"]
	if unknown.LiveProgressSupport != "" {
		t.Fatalf("never-determined row LiveProgressSupport = %q, want \"\"", unknown.LiveProgressSupport)
	}
	if unknown.LiveProgressCheckedAt != nil {
		t.Fatalf("never-determined row LiveProgressCheckedAt = %v, want nil", unknown.LiveProgressCheckedAt)
	}

	frozen := byServer["srv-frozen"]
	if frozen.LiveProgressSupport != "" || frozen.LiveProgressCheckedAt != nil {
		t.Fatalf("frozen-column row LiveProgressSupport/CheckedAt = (%q, %v), want (\"\", nil) -- the DTO must read the live_progress ROW, never ModelMapping.LiveProgressSupport (no writer since #49-3)", frozen.LiveProgressSupport, frozen.LiveProgressCheckedAt)
	}

	timeless := byServer["srv-timeless"]
	if timeless.LiveProgressSupport != "supported" {
		t.Fatalf("timeless row LiveProgressSupport = %q, want \"supported\"", timeless.LiveProgressSupport)
	}
	if timeless.LiveProgressCheckedAt != nil {
		t.Fatalf("timeless row LiveProgressCheckedAt = %v, want nil -- a row with no timestamp must not put Go's zero time on the wire", timeless.LiveProgressCheckedAt)
	}

	// The wire encoding of "never determined" must carry the key with an
	// explicit "" value, not omit it (no `omitempty` on live_progress_support) --
	// a missing key is indistinguishable from a client that doesn't know the
	// field yet, which is exactly the ambiguity this feature exists to remove.
	blob, err := json.Marshal(unknown)
	if err != nil {
		t.Fatalf("json.Marshal(unknown): %v", err)
	}
	if !strings.Contains(string(blob), `"live_progress_support":""`) {
		t.Fatalf("wire JSON = %s, want an explicit \"live_progress_support\":\"\"", blob)
	}
	if strings.Contains(string(blob), `"live_progress_checked_at"`) {
		t.Fatalf("wire JSON = %s, want live_progress_checked_at OMITTED when nil", blob)
	}
}

// countingCapabilityStore wraps a *routing.MemoryStore and counts calls to
// MappingCapabilitiesForMappings -- the N+1 guard test's instrument. Every
// other method is the embedded store's own (promoted), unchanged.
type countingCapabilityStore struct {
	*routing.MemoryStore
	capabilityCalls int
}

func (c *countingCapabilityStore) MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]routing.CapabilityRow, error) {
	c.capabilityCalls++
	return c.MemoryStore.MappingCapabilitiesForMappings(ctx, mappingIDs)
}

// failingCapabilityStore wraps a *routing.MemoryStore and fails ONLY the
// bulk capability read, leaving every other store call working -- the
// instrument for the two best-effort read sites (Service.ModelServers and
// modelsResponse), which must degrade rather than error AND must say so in
// the log. Shared with the models-listing test in
// service_models_vision_test.go.
type failingCapabilityStore struct {
	*routing.MemoryStore
	err error
}

func (f *failingCapabilityStore) MappingCapabilitiesForMappings(context.Context, []string) (map[string][]routing.CapabilityRow, error) {
	return nil, f.err
}

// captureSlog runs fn with the default slog logger redirected to a buffer and
// returns what it emitted.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestModelServersCapabilityReadFailureDegradesAndLogs: a failing bulk
// capability read must NOT fail the whole listing -- the rows still come back
// with their metrics, just with nothing determined (empty Capabilities,
// IsMtp/VisionCapable false, live-progress "") -- and it must LOG. Without
// the log this degrade renders as a perfectly ordinary page whose capability,
// MTP, vision and live-progress columns are all simply blank, which is
// indistinguishable from "never probed" and leaves an operator no trail at
// all.
func TestModelServersCapabilityReadFailureDegradesAndLogs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	failing := &failingCapabilityStore{MemoryStore: routeStore, err: errors.New("capability table unavailable")}
	svc := newModelServersTestServiceWithRoutes(t, now, fakeLoadedModels{}, failing)
	seedOffering(t, routeStore, now, "srv-a", "app-a", "map-a", "shared", "up-a", 42)
	if err := routeStore.UpsertMappingCapabilities(ctx, "map-a", []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: now},
		{Capability: routing.CapabilityMTP, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLegacy, CheckedAt: now},
		{Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	var rows []ModelServerDTO
	var err error
	logged := captureSlog(t, func() {
		rows, err = svc.ModelServers(ctx, adminToken(), "shared")
	})
	if err != nil {
		t.Fatalf("ModelServers must DEGRADE on a capability read error, not fail: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (the listing itself must survive)", len(rows))
	}
	row := rows[0]
	if row.GenTokensPerSecond != 42 {
		t.Fatalf("gen_tokens_per_second = %v, want 42 -- everything that does not come from the capability read must be unaffected", row.GenTokensPerSecond)
	}
	if len(row.Capabilities) != 0 || row.IsMtp || row.VisionCapable || row.LiveProgressSupport != "" {
		t.Fatalf("degraded row = %+v, want nothing determined (empty capabilities, false flags, \"\" live-progress)", row)
	}
	if !strings.Contains(logged, "capability read failed") || !strings.Contains(logged, "capability table unavailable") {
		t.Fatalf("log output = %q, want a warning naming the failure -- a silent degrade leaves no diagnostic trail", logged)
	}
}

// TestModelServersCapabilitiesFromRows: ModelServerDTO.Capabilities/IsMtp/
// VisionCapable are read from the model_mapping_capabilities ROWS batch
// (routing.MappingCapabilitiesForMappings), not from ModelMapping's frozen
// cap_vision/cap_video/cap_audio/cap_tools/is_mtp/vision_capable columns --
// NOT left zero/empty for a gateway-layer injection pass, exactly like
// LiveProgressSupport. One mapping carries a full row set (four known
// capabilities plus an open-vocabulary "thinking" entry the codebase has no
// constant for); a sibling mapping carries NO rows at all, so a dropped fill
// in ModelServers cannot coincidentally satisfy this: the untouched row must
// carry an EMPTY (non-nil) Capabilities slice and IsMtp/VisionCapable both
// false, which only holds if the fill genuinely reads the rows rather than
// defaulting.
func TestModelServersCapabilitiesFromRows(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-determined", "app-determined", "map-determined", "shared", "up-determined", 0)
	seedOffering(t, routeStore, now, "srv-unknown", "app-unknown", "map-unknown", "shared", "up-unknown", 0)

	checkedAt := now.Add(-time.Hour)
	if err := routeStore.UpsertMappingCapabilities(context.Background(), "map-determined", []routing.CapabilityRow{
		{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: checkedAt},
		{Capability: routing.CapabilityVideo, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: checkedAt},
		{Capability: routing.CapabilityTools, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: checkedAt},
		{Capability: routing.CapabilityMTP, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLegacy, CheckedAt: checkedAt},
		{Capability: "thinking", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: checkedAt},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities(map-determined): %v", err)
	}
	// map-unknown is not seeded at all: "never determined" is a total absence
	// of rows, not a write of all-empty verdicts.

	rows, err := svc.ModelServers(context.Background(), adminToken(), "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (%+v)", len(rows), rows)
	}
	byServer := map[string]ModelServerDTO{}
	for _, r := range rows {
		byServer[r.ServerID] = r
	}

	determined := byServer["srv-determined"]
	if len(determined.Capabilities) != 5 {
		t.Fatalf("determined row Capabilities = %+v, want 5 entries", determined.Capabilities)
	}
	byName := map[string]ModelServerCapabilityDTO{}
	for _, c := range determined.Capabilities {
		byName[c.Capability] = c
	}
	if v := byName["vision"]; v.Verdict != "yes" || v.Source != "llama_cpp_props" || !v.CheckedAt.Equal(checkedAt) {
		t.Fatalf("vision entry = %+v, want yes/llama_cpp_props/%v", v, checkedAt)
	}
	if v := byName["video"]; v.Verdict != "no" {
		t.Fatalf("video entry = %+v, want no", v)
	}
	if v := byName["tools"]; v.Verdict != "yes" {
		t.Fatalf("tools entry = %+v, want yes", v)
	}
	if _, ok := byName["audio"]; ok {
		t.Fatalf("audio entry present = %+v, want ABSENT (never determined, no row)", byName["audio"])
	}
	// The open-vocabulary "thinking" entry surfaces verbatim, not dropped --
	// this codebase has no CapabilityThinking constant.
	if v := byName["thinking"]; v.Verdict != "yes" {
		t.Fatalf("thinking entry = %+v, want yes", v)
	}
	// IsMtp/VisionCapable fold from the SAME rows batch (mtp="yes" -> true;
	// vision="yes" -> true), not from the frozen ModelMapping columns.
	if !determined.IsMtp {
		t.Fatalf("determined.IsMtp = false, want true (mtp row is yes)")
	}
	if !determined.VisionCapable {
		t.Fatalf("determined.VisionCapable = false, want true (vision row is yes)")
	}

	unknown := byServer["srv-unknown"]
	if unknown.Capabilities == nil {
		t.Fatalf("unknown row Capabilities = nil, want a non-nil EMPTY slice (never null on the wire)")
	}
	if len(unknown.Capabilities) != 0 {
		t.Fatalf("unknown row Capabilities = %+v, want empty", unknown.Capabilities)
	}
	if unknown.IsMtp || unknown.VisionCapable {
		t.Fatalf("unknown row IsMtp/VisionCapable = (%v, %v), want (false, false) -- a missing row is not capable", unknown.IsMtp, unknown.VisionCapable)
	}

	// The wire encoding of "no rows" must carry `"capabilities":[]`, never
	// `null` -- a client's array-processing code must not need a nil check.
	blob, err := json.Marshal(unknown)
	if err != nil {
		t.Fatalf("json.Marshal(unknown): %v", err)
	}
	if !strings.Contains(string(blob), `"capabilities":[]`) {
		t.Fatalf("wire JSON = %s, want an explicit \"capabilities\":[]", blob)
	}
}

// TestModelServersCapabilitiesSingleBatchQuery is the N+1 guard: ModelServers
// must issue exactly ONE MappingCapabilitiesForMappings query regardless of
// how many mappings offer the model. This is the reason the batch reader
// exists at all (see ModelServerDTO.Capabilities' own doc-comment) -- the
// listing already costs ~31 queries at the documented scale, and it is
// recomputed on every loaded-model-registry change over SSE, and once PER
// GROUP MEMBER by handlePortalModelGroupServers, so a per-row capability read
// would have multiplied both.
func TestModelServersCapabilitiesSingleBatchQuery(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	memStore := routing.NewMemoryStore()
	const n = 5
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("map-%d", i)
		seedOffering(t, memStore, now, fmt.Sprintf("srv-%d", i), fmt.Sprintf("app-%d", i), id, "shared", fmt.Sprintf("up-%d", i), 0)
		if err := memStore.UpsertMappingCapabilities(context.Background(), id, []routing.CapabilityRow{{
			Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes,
			Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: now,
		}}); err != nil {
			t.Fatalf("seed capability %s: %v", id, err)
		}
	}
	counting := &countingCapabilityStore{MemoryStore: memStore}
	svc := newModelServersTestServiceWithRoutes(t, now, fakeLoadedModels{}, counting)

	rows, err := svc.ModelServers(context.Background(), adminToken(), "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("len(rows) = %d, want %d", len(rows), n)
	}
	if counting.capabilityCalls != 1 {
		t.Fatalf("capability batch calls = %d, want exactly 1 regardless of %d offering mappings (N+1 guard)", counting.capabilityCalls, n)
	}
	for _, r := range rows {
		if len(r.Capabilities) != 1 || r.Capabilities[0].Capability != routing.CapabilityVision || r.Capabilities[0].Verdict != routing.CapabilityYes {
			t.Fatalf("row %+v capabilities = %+v, want one vision/yes entry", r.MappingID, r.Capabilities)
		}
	}
}
