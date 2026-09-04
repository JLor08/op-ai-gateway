// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/theme"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recordingBatchSettings wraps the real in-memory store and records whether each
// write arrived as one atomic batch (SetSystemSettings) or as separate per-key
// writes (SetSystemSetting), so a test can pin that UpdateSystemSettings persists
// a related group of keys atomically. Embedding the concrete *MemorySystemSettings
// (which implements the batch capability) means this fake also satisfies
// atomicSystemSettingsStore, so UpdateSystemSettings takes the atomic path.
type recordingBatchSettings struct {
	*MemorySystemSettings
	batches [][]string // keys carried by each atomic batch call
	perKey  []string   // keys written one at a time
}

func (r *recordingBatchSettings) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	r.perKey = append(r.perKey, key)
	return r.MemorySystemSettings.SetSystemSetting(ctx, key, value, now)
}

func (r *recordingBatchSettings) SetSystemSettings(ctx context.Context, values map[string]string, now time.Time) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	r.batches = append(r.batches, keys)
	return r.MemorySystemSettings.SetSystemSettings(ctx, values, now)
}

func batchContains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// TestUpdateSystemSettingsPersistsCertEnableAtomically pins the fix for the
// certificate-reconcile flake: a single enable+configure PUT must write
// cert_enabled together with cert_issuer_mode (and cert_base_domain) in ONE atomic
// batch, so a concurrent reconcile pass can never observe cert_enabled=true with a
// still-default (acme, unusable) issuer mode and persist a spurious cert_last_error.
func TestUpdateSystemSettingsPersistsCertEnableAtomically(t *testing.T) {
	ctx := context.Background()
	rec := &recordingBatchSettings{MemorySystemSettings: NewMemorySystemSettings()}
	svc := NewService(ServiceDeps{SystemSettings: rec, Clock: fixedClock()})

	on := true
	mode := "self_signed"
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings returned %v", err)
	}

	if len(rec.perKey) != 0 {
		t.Fatalf("cert settings were written per-key %v, want a single atomic batch", rec.perKey)
	}
	if len(rec.batches) != 1 {
		t.Fatalf("got %d atomic batches (%v), want exactly 1 -- the whole PUT is one atomic write", len(rec.batches), rec.batches)
	}
	batch := rec.batches[0]
	for _, key := range []string{certEnabledKey, certIssuerModeKey, certBaseDomainKey} {
		if !batchContains(batch, key) {
			t.Fatalf("key %q is missing from the atomic batch %v -- it could be observed without the others", key, batch)
		}
	}

	// And the batch actually landed as a consistent final state.
	values, err := rec.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings returned %v", err)
	}
	if values[certEnabledKey] != "true" || values[certIssuerModeKey] != "self_signed" || values[certBaseDomainKey] != "int.example.test" {
		t.Fatalf("final settings = %#v, want enabled=true, issuer=self_signed, base=int.example.test", values)
	}
}

func fixedClock() func() time.Time {
	ts := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	return func() time.Time { return ts }
}

// newTestServiceWithThemes builds a Service backed by a real theme.Registry
// loaded from a temp dir containing one external theme per id in
// externalIDs, each named strings.ToUpper(id) (so "acme" loads with
// Name == "ACME"). Load is given BuiltinThemeIDs() as its reserved id set,
// mirroring cmd/gateway/main.go's loadThemeRegistry, so a colliding
// externalID (e.g. "matrix") is skipped at load time exactly as it would be
// in production, rather than silently loading and shadowing the built-in.
func newTestServiceWithThemes(t *testing.T, externalIDs ...string) *Service {
	t.Helper()
	dir := t.TempDir()
	for _, id := range externalIDs {
		themeDir := filepath.Join(dir, id)
		if err := os.MkdirAll(themeDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", themeDir, err)
		}
		body := fmt.Sprintf(`{"name":%q}`, strings.ToUpper(id))
		if err := os.WriteFile(filepath.Join(themeDir, "theme.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write theme.json: %v", err)
		}
	}
	reg, err := theme.Load(dir, BuiltinThemeIDs()...)
	if err != nil {
		t.Fatalf("theme.Load: %v", err)
	}
	return NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock(), Themes: reg})
}

// idsOf extracts the ids from a []ThemeOption, preserving order.
func idsOf(opts []ThemeOption) []string {
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.ID
	}
	return ids
}

func TestThemeOptionsIncludeExternalAndDropCGI(t *testing.T) {
	svc := newTestServiceWithThemes(t, "acme")
	opts := svc.themeOptions(context.Background())
	ids := idsOf(opts)

	want := map[string]bool{"default": true, "matrix": true, "skynet": true, "acme": true}
	if len(ids) != len(want) {
		t.Fatalf("themeOptions ids = %v, want set-equal to %v", ids, want)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected theme id %q in %v", id, ids)
		}
		seen[id] = true
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("missing theme id %q in %v", id, ids)
		}
	}
	for _, id := range ids {
		if id == "cgi" {
			t.Fatalf("themeOptions() = %v must not include dropped built-in %q", ids, "cgi")
		}
	}
	for _, o := range opts {
		if o.ID == "acme" && o.Name != "ACME" {
			t.Fatalf("acme option name = %q, want %q", o.Name, "ACME")
		}
	}
}

func TestIsKnownThemeAcceptsExternal(t *testing.T) {
	svc := newTestServiceWithThemes(t, "acme")
	ctx := context.Background()
	if !svc.isKnownTheme(ctx, "acme") {
		t.Fatal("isKnownTheme(ctx, \"acme\") = false, want true (externally loaded theme)")
	}
	if svc.isKnownTheme(ctx, "cgi") {
		t.Fatal("isKnownTheme(ctx, \"cgi\") = true, want false (built-in removed; cgi is external-only now)")
	}
}

// TestThemeOptionsBuiltinWinsOverCollidingExternal is the FIX 1 regression
// test from the final whole-branch review: an external theme directory
// named "matrix" (colliding with the compiled built-in of the same id) must
// never produce a duplicate themeOptions entry, and activating "matrix"
// must resolve to the built-in (source "builtin", no Data payload) --
// never the external directory's data, which would shadow the compiled
// MatrixRain-driving theme. A non-colliding external id ("acme") must still
// load normally alongside it.
func TestThemeOptionsBuiltinWinsOverCollidingExternal(t *testing.T) {
	svc := newTestServiceWithThemes(t, "matrix", "acme")
	ctx := context.Background()

	opts := svc.themeOptions(ctx)
	var matrixCount int
	var matrixName string
	for _, o := range opts {
		if o.ID == "matrix" {
			matrixCount++
			matrixName = o.Name
		}
	}
	if matrixCount != 1 {
		t.Fatalf("themeOptions() has %d \"matrix\" entries, want exactly 1 (built-in must win, external dropped): %+v", matrixCount, opts)
	}
	if matrixName != "Matrix" {
		t.Fatalf("matrix option name = %q, want %q (the built-in name, not the colliding external dir's)", matrixName, "Matrix")
	}
	acmeFound := false
	for _, id := range idsOf(opts) {
		if id == "acme" {
			acmeFound = true
		}
	}
	if !acmeFound {
		t.Fatalf("themeOptions() = %v, missing non-colliding external theme %q", idsOf(opts), "acme")
	}

	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("matrix")}); err != nil {
		t.Fatalf("activate matrix theme: %v", err)
	}
	view := svc.PublicThemeView(ctx)
	if view.Theme != "matrix" {
		t.Fatalf("view.Theme = %q, want %q", view.Theme, "matrix")
	}
	if view.Source != "builtin" {
		t.Fatalf("view.Source = %q, want %q (built-in must not be shadowed by the colliding external dir)", view.Source, "builtin")
	}
	if view.Data != nil {
		t.Fatalf("view.Data = %+v, want nil (a built-in theme carries no external data payload)", view.Data)
	}
}

func TestSystemSettingsViewDefaults(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	want := SystemSettingsDTO{Theme: "default", AvailableThemes: []ThemeOption{{ID: "default", Name: "Default"}, {ID: "matrix", Name: "Matrix"}, {ID: "skynet", Name: "Skynet"}}, Language: "de", AvailableLanguages: []string{"de", "en"}, CaptureRetentionDays: 30, CaptureEnabled: true, HealthCheckIntervalSeconds: 30, AgentPresenceTimeoutSeconds: 15, TOTPMode: "off", VisionProbeMode: "accept", RouteAffinitySessionMode: "client_session", EnergyDefaultPriceUnit: "eur_cent", SMTPPort: 587, SMTPTLSMode: "starttls", SystemAdminModeRequirePassword: true, NetbirdPolicyScope: "auto", NetbirdEffectivePolicyScope: "selected", NetbirdPeerSyncIntervalSeconds: 30, NetbirdReconcileIntervalSeconds: 60, NetbirdTokenRotateBeforeDays: 14, CertIssuerMode: "acme", CertSelfSignedValidityDays: 365, CertCARenewBeforeDays: 365, ACMEDirectoryURL: DefaultACMEDirectoryURL, CertServerScope: "selected", CertRenewBeforeDays: 30, CertPublicDomains: []string{}, CertEdgeIssuerMode: "self_signed", CertEdgeNames: []string{}, CertEdgeACMEShared: true, CertPublicACMEShared: true, CertHTTPSSwitchMode: "manual", CertProxyListenPortBase: 8600}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SystemSettingsView() = %+v, want %+v", got, want)
	}
}

func TestUpdateSystemSettingsPersistsKnownTheme(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("default")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	want := SystemSettingsDTO{Theme: "default", AvailableThemes: []ThemeOption{{ID: "default", Name: "Default"}, {ID: "matrix", Name: "Matrix"}, {ID: "skynet", Name: "Skynet"}}, Language: "de", AvailableLanguages: []string{"de", "en"}, CaptureRetentionDays: 30, CaptureEnabled: true, HealthCheckIntervalSeconds: 30, AgentPresenceTimeoutSeconds: 15, TOTPMode: "off", VisionProbeMode: "accept", RouteAffinitySessionMode: "client_session", EnergyDefaultPriceUnit: "eur_cent", SMTPPort: 587, SMTPTLSMode: "starttls", SystemAdminModeRequirePassword: true, NetbirdPolicyScope: "auto", NetbirdEffectivePolicyScope: "selected", NetbirdPeerSyncIntervalSeconds: 30, NetbirdReconcileIntervalSeconds: 60, NetbirdTokenRotateBeforeDays: 14, CertIssuerMode: "acme", CertSelfSignedValidityDays: 365, CertCARenewBeforeDays: 365, ACMEDirectoryURL: DefaultACMEDirectoryURL, CertServerScope: "selected", CertRenewBeforeDays: 30, CertPublicDomains: []string{}, CertEdgeIssuerMode: "self_signed", CertEdgeNames: []string{}, CertEdgeACMEShared: true, CertPublicACMEShared: true, CertHTTPSSwitchMode: "manual", CertProxyListenPortBase: 8600}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UpdateSystemSettings() = %+v, want %+v", got, want)
	}
	// It persists: a subsequent view reflects the stored value.
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["theme"] != "default" {
		t.Fatalf("stored theme = %q, want %q", values["theme"], "default")
	}
}

// TestUpdateSystemSettingsForbidsNonSystem proves the PT-2 Part 2 internal
// authz guard: a principal without the "system" scope (even a plain admin)
// is rejected with ErrPrincipalForbidden and nothing is persisted -- the
// HTTP-level gate (requireWebScope("system")) is defense-in-depth on TOP of
// this, not instead of it. Never loosen: SystemSettingsView's read path is
// unaffected (it carries no principal at all), only the mutating write does.
func TestUpdateSystemSettingsForbidsNonSystem(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{Theme: strPtr("default")}); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("UpdateSystemSettings(admin, not system) err = %v, want ErrPrincipalForbidden", err)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if _, ok := values["theme"]; ok {
		t.Fatalf("theme was persisted despite ErrPrincipalForbidden: %+v", values)
	}
}

// TestUpdateSystemSettingsAllowsSystem proves the flip side of the same
// guard: a system-scoped principal succeeds exactly as before the PT-2
// Part 2 guard was added.
func TestUpdateSystemSettingsAllowsSystem(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("default")}); err != nil {
		t.Fatalf("UpdateSystemSettings(system): %v", err)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values["theme"] != "default" {
		t.Fatalf("stored theme = %q, want %q", values["theme"], "default")
	}
}

func TestUpdateSystemSettingsRejectsUnknownTheme(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("nope")})
	if !errors.Is(err, ErrThemeInvalid) {
		t.Fatalf("UpdateSystemSettings(nope) error = %v, want ErrThemeInvalid", err)
	}
}

func TestSystemSettingsNilStoreDefaults(t *testing.T) {
	svc := NewService(ServiceDeps{Clock: fixedClock()})
	if theme := svc.PublicTheme(context.Background()); theme != "default" {
		t.Fatalf("PublicTheme() with nil store = %q, want %q", theme, "default")
	}
	got := svc.SystemSettingsView(context.Background())
	want := SystemSettingsDTO{Theme: "default", AvailableThemes: []ThemeOption{{ID: "default", Name: "Default"}, {ID: "matrix", Name: "Matrix"}, {ID: "skynet", Name: "Skynet"}}, Language: "de", AvailableLanguages: []string{"de", "en"}, CaptureRetentionDays: 30, CaptureEnabled: true, HealthCheckIntervalSeconds: 30, AgentPresenceTimeoutSeconds: 15, TOTPMode: "off", VisionProbeMode: "accept", RouteAffinitySessionMode: "client_session", SMTPPort: 587, SMTPTLSMode: "starttls"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SystemSettingsView() with nil store = %+v, want %+v", got, want)
	}
}

// TestUpdateSystemSettingsRejectsCgiAsBuiltin guards the behavior change of
// this task: "cgi" used to be a Go-side built-in theme; it is now available
// only as an externally deployed theme (see internal/theme), so a gateway
// with no such external theme loaded must reject it like any other unknown
// id.
func TestUpdateSystemSettingsRejectsCgiAsBuiltin(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("cgi")})
	if !errors.Is(err, ErrThemeInvalid) {
		t.Fatalf("UpdateSystemSettings(cgi) error = %v, want ErrThemeInvalid", err)
	}
}

func TestUpdateSystemSettingsPersistsSkynetTheme(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("skynet")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(skynet) returned error: %v", err)
	}
	if got.Theme != "skynet" {
		t.Fatalf("theme = %q, want %q", got.Theme, "skynet")
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["theme"] != "skynet" {
		t.Fatalf("stored theme = %q, want %q", values["theme"], "skynet")
	}
}

func TestUpdateSystemSettingsPersistsMatrixTheme(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("matrix")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(matrix) returned error: %v", err)
	}
	if got.Theme != "matrix" {
		t.Fatalf("theme = %q, want %q", got.Theme, "matrix")
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["theme"] != "matrix" {
		t.Fatalf("stored theme = %q, want %q", values["theme"], "matrix")
	}
}

func TestUpdateSystemSettingsPersistsLanguage(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Language: strPtr("en")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(en) returned error: %v", err)
	}
	if got.Language != "en" {
		t.Fatalf("language = %q, want %q", got.Language, "en")
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["language"] != "en" {
		t.Fatalf("stored language = %q, want %q", values["language"], "en")
	}
}

func TestUpdateSystemSettingsRejectsUnknownLanguage(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Language: strPtr("fr")})
	if !errors.Is(err, ErrLanguageInvalid) {
		t.Fatalf("UpdateSystemSettings(fr) error = %v, want ErrLanguageInvalid", err)
	}
}

func intPtr(i int) *int { return &i }

func TestSystemSettingsViewDefaultRetention(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if got.CaptureRetentionDays != 30 {
		t.Fatalf("CaptureRetentionDays = %d, want 30", got.CaptureRetentionDays)
	}
}

func TestUpdateSystemSettingsPersistsRetention(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CaptureRetentionDays: intPtr(45)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(45) returned error: %v", err)
	}
	if got.CaptureRetentionDays != 45 {
		t.Fatalf("CaptureRetentionDays = %d, want 45", got.CaptureRetentionDays)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["capture_retention_days"] != "45" {
		t.Fatalf("stored capture_retention_days = %q, want %q", values["capture_retention_days"], "45")
	}
}

func TestUpdateSystemSettingsRejectsRetentionBelowMin(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CaptureRetentionDays: intPtr(0)})
	if !errors.Is(err, ErrRetentionInvalid) {
		t.Fatalf("UpdateSystemSettings(0) error = %v, want ErrRetentionInvalid", err)
	}
}

func TestUpdateSystemSettingsRejectsRetentionAboveMax(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CaptureRetentionDays: intPtr(366)})
	if !errors.Is(err, ErrRetentionInvalid) {
		t.Fatalf("UpdateSystemSettings(366) error = %v, want ErrRetentionInvalid", err)
	}
}

func TestCaptureRetentionDaysHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want int
	}{
		{"absent -> default", map[string]string{}, 30},
		{"blank -> default", map[string]string{"capture_retention_days": ""}, 30},
		{"garbage -> default", map[string]string{"capture_retention_days": "abc"}, 30},
		{"below min -> default", map[string]string{"capture_retention_days": "0"}, 30},
		{"above max -> default", map[string]string{"capture_retention_days": "999"}, 30},
		{"valid", map[string]string{"capture_retention_days": "7"}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CaptureRetentionDays(tc.in); got != tc.want {
				t.Fatalf("CaptureRetentionDays(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestCaptureEnabledHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"absent -> default true", map[string]string{}, true},
		{"blank -> default true", map[string]string{"capture_enabled": ""}, true},
		{"garbage -> default true", map[string]string{"capture_enabled": "nope"}, true},
		{"explicit true", map[string]string{"capture_enabled": "true"}, true},
		{"explicit false", map[string]string{"capture_enabled": "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CaptureEnabled(tc.in); got != tc.want {
				t.Fatalf("CaptureEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCaptureOverrideHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"absent -> default false", map[string]string{}, false},
		{"blank -> default false", map[string]string{"capture_override": ""}, false},
		{"garbage -> default false", map[string]string{"capture_override": "nope"}, false},
		{"explicit true", map[string]string{"capture_override": "true"}, true},
		{"explicit false", map[string]string{"capture_override": "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CaptureOverride(tc.in); got != tc.want {
				t.Fatalf("CaptureOverride(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHealthCheckIntervalSecondsHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want int
	}{
		{"absent -> default", map[string]string{}, 30},
		{"blank -> default", map[string]string{"health_check_interval_seconds": ""}, 30},
		{"garbage -> default", map[string]string{"health_check_interval_seconds": "abc"}, 30},
		{"below min -> default", map[string]string{"health_check_interval_seconds": "4"}, 30},
		{"above max -> default", map[string]string{"health_check_interval_seconds": "3601"}, 30},
		{"min", map[string]string{"health_check_interval_seconds": "5"}, 5},
		{"max", map[string]string{"health_check_interval_seconds": "3600"}, 3600},
		{"valid", map[string]string{"health_check_interval_seconds": "60"}, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HealthCheckIntervalSeconds(tc.in); got != tc.want {
				t.Fatalf("HealthCheckIntervalSeconds(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestSystemSettingsViewDefaultHealthCheckInterval(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if got.HealthCheckIntervalSeconds != 30 {
		t.Fatalf("HealthCheckIntervalSeconds = %d, want 30", got.HealthCheckIntervalSeconds)
	}
}

func TestUpdateSystemSettingsPersistsHealthCheckInterval(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{HealthCheckIntervalSeconds: intPtr(120)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(120) returned error: %v", err)
	}
	if got.HealthCheckIntervalSeconds != 120 {
		t.Fatalf("HealthCheckIntervalSeconds = %d, want 120", got.HealthCheckIntervalSeconds)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["health_check_interval_seconds"] != "120" {
		t.Fatalf("stored health_check_interval_seconds = %q, want %q", values["health_check_interval_seconds"], "120")
	}
}

func TestUpdateSystemSettingsRejectsHealthCheckIntervalBelowMin(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{HealthCheckIntervalSeconds: intPtr(4)})
	if !errors.Is(err, ErrHealthCheckIntervalInvalid) {
		t.Fatalf("UpdateSystemSettings(4) error = %v, want ErrHealthCheckIntervalInvalid", err)
	}
}

func TestUpdateSystemSettingsRejectsHealthCheckIntervalAboveMax(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{HealthCheckIntervalSeconds: intPtr(3601)})
	if !errors.Is(err, ErrHealthCheckIntervalInvalid) {
		t.Fatalf("UpdateSystemSettings(3601) error = %v, want ErrHealthCheckIntervalInvalid", err)
	}
}

func TestAgentPresenceTimeoutSecondsHelper(t *testing.T) {
	svc := NewService(ServiceDeps{Clock: fixedClock()})
	cases := []struct {
		name string
		in   map[string]string
		want int
	}{
		{"absent -> default", map[string]string{}, 15},
		{"blank -> default", map[string]string{"agent_presence_timeout_seconds": ""}, 15},
		{"garbage -> default", map[string]string{"agent_presence_timeout_seconds": "abc"}, 15},
		{"below min -> default", map[string]string{"agent_presence_timeout_seconds": "2"}, 15},
		{"above max -> default", map[string]string{"agent_presence_timeout_seconds": "3601"}, 15},
		{"min", map[string]string{"agent_presence_timeout_seconds": "3"}, 3},
		{"max", map[string]string{"agent_presence_timeout_seconds": "3600"}, 3600},
		{"valid", map[string]string{"agent_presence_timeout_seconds": "20"}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.AgentPresenceTimeoutSeconds(tc.in); got != tc.want {
				t.Fatalf("AgentPresenceTimeoutSeconds(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestAgentPresenceTimeoutSecondsUsesServiceDepsDefault confirms the KV-absent
// fallback is the env-derived ServiceDeps default (like
// NetbirdTokenRotateBeforeDays), not the hardcoded DefaultAgentPresenceTimeoutSeconds.
func TestAgentPresenceTimeoutSecondsUsesServiceDepsDefault(t *testing.T) {
	svc := NewService(ServiceDeps{Clock: fixedClock(), AgentPresenceTimeoutDefault: 5})
	if got := svc.AgentPresenceTimeoutSeconds(map[string]string{}); got != 5 {
		t.Fatalf("AgentPresenceTimeoutSeconds(absent) = %d, want 5 (ServiceDeps default)", got)
	}
}

func TestSystemSettingsViewDefaultAgentPresenceTimeout(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if got.AgentPresenceTimeoutSeconds != 15 {
		t.Fatalf("AgentPresenceTimeoutSeconds = %d, want 15", got.AgentPresenceTimeoutSeconds)
	}
}

// TestActiveAgentPresenceTimeoutSecondsMethod exercises the public
// ctx-carrying method (distinct from the values-map method
// AgentPresenceTimeoutSeconds(values) added in Task 2 — Go forbids two methods
// of the same name/receiver, so this is the portal.API-exposed accessor used by
// the /api/portal/agent-presence-timeout endpoint) — it must reflect the live
// KV value, not just the env-derived default.
func TestActiveAgentPresenceTimeoutSecondsMethod(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	if got := svc.ActiveAgentPresenceTimeoutSeconds(context.Background()); got != 15 {
		t.Fatalf("ActiveAgentPresenceTimeoutSeconds() = %d, want 15 (default)", got)
	}
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{AgentPresenceTimeoutSeconds: intPtr(42)}); err != nil {
		t.Fatalf("UpdateSystemSettings(42): %v", err)
	}
	if got := svc.ActiveAgentPresenceTimeoutSeconds(context.Background()); got != 42 {
		t.Fatalf("ActiveAgentPresenceTimeoutSeconds() after update = %d, want 42", got)
	}
}

func TestUpdateSystemSettingsPersistsAgentPresenceTimeout(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{AgentPresenceTimeoutSeconds: intPtr(20)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(20) returned error: %v", err)
	}
	if got.AgentPresenceTimeoutSeconds != 20 {
		t.Fatalf("AgentPresenceTimeoutSeconds = %d, want 20", got.AgentPresenceTimeoutSeconds)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["agent_presence_timeout_seconds"] != "20" {
		t.Fatalf("stored agent_presence_timeout_seconds = %q, want %q", values["agent_presence_timeout_seconds"], "20")
	}
}

func TestUpdateSystemSettingsRejectsAgentPresenceTimeoutBelowMin(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{AgentPresenceTimeoutSeconds: intPtr(2)})
	if !errors.Is(err, ErrAgentPresenceTimeoutInvalid) {
		t.Fatalf("UpdateSystemSettings(2) error = %v, want ErrAgentPresenceTimeoutInvalid", err)
	}
}

func TestUpdateSystemSettingsRejectsAgentPresenceTimeoutAboveMax(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{AgentPresenceTimeoutSeconds: intPtr(3601)})
	if !errors.Is(err, ErrAgentPresenceTimeoutInvalid) {
		t.Fatalf("UpdateSystemSettings(3601) error = %v, want ErrAgentPresenceTimeoutInvalid", err)
	}
}

func TestSystemSettingsViewDefaultCaptureOverride(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if got.CaptureOverride {
		t.Fatalf("CaptureOverride = %v, want false (default opt-in preserved)", got.CaptureOverride)
	}
}

func TestUpdateSystemSettingsPersistsCaptureOverride(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CaptureOverride: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(true) returned error: %v", err)
	}
	if !got.CaptureOverride {
		t.Fatalf("CaptureOverride = %v, want true", got.CaptureOverride)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["capture_override"] != "true" {
		t.Fatalf("stored capture_override = %q, want %q", values["capture_override"], "true")
	}
}

func TestSystemSettingsViewDefaultCaptureEnabled(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if !got.CaptureEnabled {
		t.Fatalf("CaptureEnabled = %v, want true (default-on kill switch)", got.CaptureEnabled)
	}
}

func TestUpdateSystemSettingsPersistsCaptureEnabled(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CaptureEnabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(false) returned error: %v", err)
	}
	if got.CaptureEnabled {
		t.Fatalf("CaptureEnabled = %v, want false", got.CaptureEnabled)
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values["capture_enabled"] != "false" {
		t.Fatalf("stored capture_enabled = %q, want %q", values["capture_enabled"], "false")
	}
}

func TestUpdateSystemSettingsTOTPMode(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})

	if got := TOTPMode(map[string]string{}); got != "off" {
		t.Fatalf("TOTPMode(default) = %q, want %q", got, "off")
	}

	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{TOTPMode: strPtr("required")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(required) returned error: %v", err)
	}
	if got.TOTPMode != "required" {
		t.Fatalf("TOTPMode = %q, want %q", got.TOTPMode, "required")
	}
	if mode := svc.TOTPMode(context.Background()); mode != "required" {
		t.Fatalf("svc.TOTPMode() = %q, want %q", mode, "required")
	}

	_, err = svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{TOTPMode: strPtr("nope")})
	if !errors.Is(err, ErrTotpModeInvalid) {
		t.Fatalf("UpdateSystemSettings(nope) error = %v, want ErrTotpModeInvalid", err)
	}
}

func TestUpdateSystemSettingsVisionProbeMode(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})

	if mode := svc.VisionProbeMode(context.Background()); mode != "accept" {
		t.Fatalf("svc.VisionProbeMode(default) = %q, want %q", mode, "accept")
	}

	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{VisionProbeMode: strPtr("verify")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(verify) returned error: %v", err)
	}
	if got.VisionProbeMode != "verify" {
		t.Fatalf("VisionProbeMode = %q, want %q", got.VisionProbeMode, "verify")
	}
	if mode := svc.VisionProbeMode(context.Background()); mode != "verify" {
		t.Fatalf("svc.VisionProbeMode() = %q, want %q", mode, "verify")
	}

	_, err = svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{VisionProbeMode: strPtr("nonsense")})
	if !errors.Is(err, ErrVisionProbeModeInvalid) {
		t.Fatalf("UpdateSystemSettings(nonsense) error = %v, want ErrVisionProbeModeInvalid", err)
	}
}

func TestNetbirdPolicyGettersDefaults(t *testing.T) {
	v := map[string]string{}
	if NetbirdManagePolicies(v) || NetbirdDenyByDefault(v) || NetbirdDenyByDefaultEnforce(v) {
		t.Fatal("bool defaults must be false")
	}
	if NetbirdPolicyScope(v) != "auto" {
		t.Fatalf("scope default = %q", NetbirdPolicyScope(v))
	}
	if NetbirdPeerSyncIntervalSeconds(v) != 30 || NetbirdReconcileIntervalSeconds(v) != 60 {
		t.Fatalf("interval defaults wrong: %d %d", NetbirdPeerSyncIntervalSeconds(v), NetbirdReconcileIntervalSeconds(v))
	}
	if NetbirdPolicyScope(map[string]string{"netbird_policy_scope": "bogus"}) != "auto" {
		t.Fatal("unknown scope must fall back to auto")
	}
	if NetbirdPeerSyncIntervalSeconds(map[string]string{"netbird_peer_sync_interval_seconds": "3"}) != 30 {
		t.Fatal("below-floor peer interval must fall back to default")
	}
}

func TestNetbirdAllowPingGettersDefaults(t *testing.T) {
	// Absent -> false for both readers.
	empty := map[string]string{}
	if NetbirdAllowPingGateway(empty) || NetbirdAllowPingAllServers(empty) {
		t.Fatal("allow-ping bool defaults must be false when absent")
	}
	// Blank + unparseable -> false; "true" -> true; "false" -> false. Independent keys.
	cases := []struct {
		gatewayRaw, allRaw   string
		wantGateway, wantAll bool
	}{
		{"", "", false, false},
		{"nope", "meh", false, false},
		{"true", "false", true, false},
		{"false", "true", false, true},
		{"  true  ", "  true  ", true, true},
	}
	for _, tc := range cases {
		v := map[string]string{
			netbirdAllowPingGatewayKey:    tc.gatewayRaw,
			netbirdAllowPingAllServersKey: tc.allRaw,
		}
		if got := NetbirdAllowPingGateway(v); got != tc.wantGateway {
			t.Fatalf("NetbirdAllowPingGateway(%q) = %v, want %v", tc.gatewayRaw, got, tc.wantGateway)
		}
		if got := NetbirdAllowPingAllServers(v); got != tc.wantAll {
			t.Fatalf("NetbirdAllowPingAllServers(%q) = %v, want %v", tc.allRaw, got, tc.wantAll)
		}
	}
}

func TestUpdateSystemSettingsPersistsAllowPing(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdAllowPingGateway:    boolPtr(true),
		NetbirdAllowPingAllServers: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if !got.NetbirdAllowPingGateway || !got.NetbirdAllowPingAllServers {
		t.Fatalf("DTO = %+v, want both allow-ping flags true", got)
	}

	// Round-trips via a fresh view AND the policy-settings snapshot.
	view := svc.SystemSettingsView(ctx)
	if !view.NetbirdAllowPingGateway || !view.NetbirdAllowPingAllServers {
		t.Fatalf("SystemSettingsView() = %+v, want both allow-ping flags true", view)
	}
	snap := svc.NetbirdPolicySettings(ctx, 0)
	if !snap.AllowPingGateway || !snap.AllowPingAllServers {
		t.Fatalf("NetbirdPolicySettings() = %+v, want both allow-ping flags true", snap)
	}

	// The two flags are independent: clearing one leaves the other set.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdAllowPingAllServers: boolPtr(false)}); err != nil {
		t.Fatalf("UpdateSystemSettings(clear all-servers) returned error: %v", err)
	}
	view = svc.SystemSettingsView(ctx)
	if !view.NetbirdAllowPingGateway || view.NetbirdAllowPingAllServers {
		t.Fatalf("after clearing all-servers, view = %+v, want gateway=true all-servers=false", view)
	}
}

// TestUpdateSystemSettingsAllowPingTriggersPolicySideEffects proves a settings
// PUT carrying ONLY a ping bool (EITHER one) schedules the background fleet
// reconcile (applyPolicySettingsSideEffects -> reconcileAllServerPolicies),
// evidenced by a per-server access policy being created for a managed server.
// Each ping bool is covered independently so removing either trigger term from
// the condition regresses a case (both terms are guarded).
func TestUpdateSystemSettingsAllowPingTriggersPolicySideEffects(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		req  UpdateSystemSettingsRequest
	}{
		{"gateway only", UpdateSystemSettingsRequest{NetbirdAllowPingGateway: boolPtr(true)}},
		{"all servers only", UpdateSystemSettingsRequest{NetbirdAllowPingAllServers: boolPtr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore := newNetbirdServerTestService(t, now)
			fake := newFakeNetbird(t)
			fake.seedGroup("gw-portal", "op-gw-portal")
			// Turn policy management on; this settles its own side effect against the
			// still-empty fleet, so the access-policy create count starts at 0.
			enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

			seedManagedNetbirdServer(t, routeStore, "srv-ping", "PingSrv", "track-ping", "", now)
			seedApp(t, routeStore, "app-ping", "srv-ping", 8080, routing.ServerStatusActive, now)

			if before := fake.policyCreateCountByName("op-gw-access-srv-ping"); before != 0 {
				t.Fatalf("baseline op-gw-access-srv-ping creates = %d, want 0", before)
			}

			// A PUT with ONLY this ping bool must still schedule the fleet reconcile.
			if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), tc.req); err != nil {
				t.Fatalf("UpdateSystemSettings(ping-only) returned error: %v", err)
			}
			svc.waitPolicySideEffects()

			if got := fake.policyCreateCountByName("op-gw-access-srv-ping"); got != 1 {
				t.Fatalf("op-gw-access-srv-ping creates = %d, want 1 (ping-only PUT must trigger the fleet reconcile)", got)
			}
		})
	}
}

func TestEffectiveNetbirdPolicyScope(t *testing.T) {
	if EffectiveNetbirdPolicyScope("auto", true) != "all" {
		t.Fatal("auto + deny -> all")
	}
	if EffectiveNetbirdPolicyScope("auto", false) != "selected" {
		t.Fatal("auto + !deny -> selected")
	}
	if EffectiveNetbirdPolicyScope("all", false) != "all" || EffectiveNetbirdPolicyScope("selected", true) != "selected" {
		t.Fatal("explicit scope must win")
	}
}

func TestUpdateSystemSettingsIntervalOrder(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	ctx := context.Background()
	a, b := 90, 30
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdPeerSyncIntervalSeconds: &a, NetbirdReconcileIntervalSeconds: &b,
	}); !errors.Is(err, ErrNetbirdIntervalOrder) {
		t.Fatalf("expected ErrNetbirdIntervalOrder, got %v", err)
	}
	a2, b2 := 30, 30
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdPeerSyncIntervalSeconds: &a2, NetbirdReconcileIntervalSeconds: &b2,
	}); err != nil {
		t.Fatalf("A==B should be valid: %v", err)
	}
	big := 120
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdPeerSyncIntervalSeconds: &big}); !errors.Is(err, ErrNetbirdIntervalOrder) {
		t.Fatalf("A alone above stored B(=60 default) must reject: %v", err)
	}
}

func TestUpdateSystemSettingsRejectsIntervalBelowFloor(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	ctx := context.Background()
	tooLow := 5
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdPeerSyncIntervalSeconds: &tooLow}); !errors.Is(err, ErrNetbirdIntervalOrder) {
		t.Fatalf("below-floor peer interval must reject: %v", err)
	}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdReconcileIntervalSeconds: &tooLow}); !errors.Is(err, ErrNetbirdIntervalOrder) {
		t.Fatalf("below-floor reconcile interval must reject: %v", err)
	}
}

func TestUpdateSystemSettingsPersistsNetbirdPolicySettings(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	manage := true
	scope := "all"
	deny := true
	enforce := true
	peer := 15
	reconcile := 45

	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdManagePolicies:           &manage,
		NetbirdPolicyScope:              &scope,
		NetbirdDenyByDefault:            &deny,
		NetbirdDenyByDefaultEnforce:     &enforce,
		NetbirdPeerSyncIntervalSeconds:  &peer,
		NetbirdReconcileIntervalSeconds: &reconcile,
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if !got.NetbirdManagePolicies || got.NetbirdPolicyScope != "all" || got.NetbirdEffectivePolicyScope != "all" ||
		!got.NetbirdDenyByDefault || !got.NetbirdDenyByDefaultEnforce ||
		got.NetbirdPeerSyncIntervalSeconds != 15 || got.NetbirdReconcileIntervalSeconds != 45 {
		t.Fatalf("UpdateSystemSettings() DTO = %+v, want all six fields applied", got)
	}

	// Round-trip via a fresh view (subsequent PUT/GET reflect the stored values).
	view := svc.SystemSettingsView(ctx)
	if !view.NetbirdManagePolicies || view.NetbirdPolicyScope != "all" || view.NetbirdEffectivePolicyScope != "all" ||
		!view.NetbirdDenyByDefault || !view.NetbirdDenyByDefaultEnforce ||
		view.NetbirdPeerSyncIntervalSeconds != 15 || view.NetbirdReconcileIntervalSeconds != 45 {
		t.Fatalf("SystemSettingsView() = %+v, want the six fields round-tripped", view)
	}

	// auto scope resolves through deny-by-default.
	autoScope := "auto"
	noDeny := false
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdPolicyScope: &autoScope, NetbirdDenyByDefault: &noDeny}); err != nil {
		t.Fatalf("UpdateSystemSettings(auto) returned error: %v", err)
	}
	view = svc.SystemSettingsView(ctx)
	if view.NetbirdPolicyScope != "auto" || view.NetbirdEffectivePolicyScope != "selected" {
		t.Fatalf("auto+!deny effective scope = %+v, want selected", view)
	}
}

func TestNetbirdPolicyScopeLenientUnknownIsNotAnError(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()
	bogus := "bogus"
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdPolicyScope: &bogus})
	if err != nil {
		t.Fatalf("bogus scope must not error (lenient): %v", err)
	}
	if got.NetbirdPolicyScope != "auto" {
		t.Fatalf("stored bogus scope reads back as %q, want fallback to auto", got.NetbirdPolicyScope)
	}
}

func TestNetbirdPolicySettingsSnapshot(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	// Defaults, no env fallback.
	snap := svc.NetbirdPolicySettings(ctx, 0)
	if snap.PeerSyncInterval != 30*time.Second || snap.ReconcileInterval != 60*time.Second {
		t.Fatalf("default snapshot intervals = %+v, want 30s/60s", snap)
	}
	if snap.EffectiveScope != "selected" {
		t.Fatalf("default effective scope = %q, want selected (auto + !deny)", snap.EffectiveScope)
	}

	// Env fallback applies only when the peer KV is unset.
	snap = svc.NetbirdPolicySettings(ctx, 20)
	if snap.PeerSyncInterval != 20*time.Second {
		t.Fatalf("env fallback peer interval = %v, want 20s", snap.PeerSyncInterval)
	}

	// Once the KV is set, the env fallback no longer applies.
	peer := 12
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdPeerSyncIntervalSeconds: &peer}); err != nil {
		t.Fatalf("update: %v", err)
	}
	snap = svc.NetbirdPolicySettings(ctx, 20)
	if snap.PeerSyncInterval != 12*time.Second {
		t.Fatalf("stored peer interval = %v, want 12s (env fallback must not override a set KV)", snap.PeerSyncInterval)
	}
}

func TestNetbirdPolicySettingsEnvFallbackClampsReconcile(t *testing.T) {
	// Both interval KVs unset: an env fallback that exceeds the default
	// reconcile interval (60s) must clamp reconcile UP to the peer interval,
	// preserving A<=B at runtime even though the DTO/UI would still show the
	// stored (unset -> default) reconcile value.
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	ctx := context.Background()
	snap := svc.NetbirdPolicySettings(ctx, 90)
	if snap.PeerSyncInterval != 90*time.Second {
		t.Fatalf("PeerSyncInterval = %v, want 90s (env fallback)", snap.PeerSyncInterval)
	}
	if snap.ReconcileInterval != 90*time.Second {
		t.Fatalf("ReconcileInterval = %v, want 90s (clamped up to the env-derived peer interval)", snap.ReconcileInterval)
	}

	// An env fallback below the stored/default reconcile interval must NOT
	// clamp anything.
	svc2 := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	snap2 := svc2.NetbirdPolicySettings(ctx, 20)
	if snap2.PeerSyncInterval != 20*time.Second {
		t.Fatalf("PeerSyncInterval = %v, want 20s (env fallback)", snap2.PeerSyncInterval)
	}
	if snap2.ReconcileInterval != 60*time.Second {
		t.Fatalf("ReconcileInterval = %v, want 60s (default, no clamp)", snap2.ReconcileInterval)
	}
}

func TestGatewayPeerNameRoundTrip(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	ctx := context.Background()
	name := "my-gateway"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdGatewayPeerName: &name}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := svc.SystemSettingsView(ctx).NetbirdGatewayPeerName; got != name {
		t.Fatalf("NetbirdGatewayPeerName = %q, want %q", got, name)
	}
	empty := "  "
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdGatewayPeerName: &empty}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := svc.SystemSettingsView(ctx).NetbirdGatewayPeerName; got != "" {
		t.Fatalf("after clear = %q, want empty (trimmed)", got)
	}
}

func TestNetbirdTokenRotateBeforeDaysDefault(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	if got := svc.SystemSettingsView(context.Background()).NetbirdTokenRotateBeforeDays; got != DefaultNetbirdTokenRotateBeforeDays {
		t.Fatalf("default NetbirdTokenRotateBeforeDays = %d, want %d", got, DefaultNetbirdTokenRotateBeforeDays)
	}
}

func TestNetbirdTokenRotateBeforeDaysEnvFallbackDefault(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock(), NetbirdTokenRotateBeforeDaysDefault: intPtr(30)})
	if got := svc.SystemSettingsView(context.Background()).NetbirdTokenRotateBeforeDays; got != 30 {
		t.Fatalf("env-fallback default NetbirdTokenRotateBeforeDays = %d, want 30", got)
	}
}

// An explicit env-level 0 (auto-rotation off) must NOT be clamped back to the
// 14-day default by NewService — the *int sentinel distinguishes "operator set 0"
// from "field never provided" (nil → default 14).
func TestNetbirdTokenRotateBeforeDaysExplicitZeroDefaultOff(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock(), NetbirdTokenRotateBeforeDaysDefault: intPtr(0)})
	if got := svc.SystemSettingsView(context.Background()).NetbirdTokenRotateBeforeDays; got != 0 {
		t.Fatalf("explicit-zero env default NetbirdTokenRotateBeforeDays = %d, want 0 (off)", got)
	}
}

func TestUpdateSystemSettingsRejectsNegativeNetbirdTokenRotateBeforeDays(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	n := -1
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdTokenRotateBeforeDays: &n}); !errors.Is(err, ErrNetbirdTokenRotateBeforeInvalid) {
		t.Fatalf("want ErrNetbirdTokenRotateBeforeInvalid, got %v", err)
	}
}

func TestUpdateSystemSettingsPersistsNetbirdTokenRotateBeforeDaysZero(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()
	z := 0
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdTokenRotateBeforeDays: &z})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if dto.NetbirdTokenRotateBeforeDays != 0 {
		t.Fatalf("NetbirdTokenRotateBeforeDays = %d, want 0", dto.NetbirdTokenRotateBeforeDays)
	}
	if got := svc.SystemSettingsView(ctx).NetbirdTokenRotateBeforeDays; got != 0 {
		t.Fatalf("round-trip NetbirdTokenRotateBeforeDays = %d, want 0", got)
	}
}

func TestUpdateSystemSettingsPersistsNetbirdTokenRotateBeforeDaysPositive(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()
	n := 21
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdTokenRotateBeforeDays: &n})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if dto.NetbirdTokenRotateBeforeDays != 21 {
		t.Fatalf("NetbirdTokenRotateBeforeDays = %d, want 21", dto.NetbirdTokenRotateBeforeDays)
	}
	if got := svc.SystemSettingsView(ctx).NetbirdTokenRotateBeforeDays; got != 21 {
		t.Fatalf("round-trip NetbirdTokenRotateBeforeDays = %d, want 21", got)
	}
}

func floatPtr(f float64) *float64 { return &f }

// TestSystemSettingsViewDefaultEnergyDefaults asserts the three energy-default
// settings read 0 ("no default") when unset.
func TestSystemSettingsViewDefaultEnergyDefaults(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got := svc.SystemSettingsView(context.Background())
	if got.EnergyDefaultPricePerKwh != 0 || got.EnergyDefaultPue != 0 || got.EnergyDefaultWhPerToken != 0 {
		t.Fatalf("default energy defaults = (%v,%v,%v), want (0,0,0)", got.EnergyDefaultPricePerKwh, got.EnergyDefaultPue, got.EnergyDefaultWhPerToken)
	}
}

// TestUpdateSystemSettingsPersistsEnergyDefaults round-trips the three
// energy-default settings through the settings PUT->GET.
func TestUpdateSystemSettingsPersistsEnergyDefaults(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()
	price, pue, wh := 0.32, 1.4, 0.0025
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		EnergyDefaultPricePerKwh: floatPtr(price),
		EnergyDefaultPue:         floatPtr(pue),
		EnergyDefaultWhPerToken:  floatPtr(wh),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if dto.EnergyDefaultPricePerKwh != price || dto.EnergyDefaultPue != pue || dto.EnergyDefaultWhPerToken != wh {
		t.Fatalf("UpdateSystemSettings() energy defaults = (%v,%v,%v), want (%v,%v,%v)",
			dto.EnergyDefaultPricePerKwh, dto.EnergyDefaultPue, dto.EnergyDefaultWhPerToken, price, pue, wh)
	}
	got := svc.SystemSettingsView(ctx)
	if got.EnergyDefaultPricePerKwh != price || got.EnergyDefaultPue != pue || got.EnergyDefaultWhPerToken != wh {
		t.Fatalf("round-trip energy defaults = (%v,%v,%v), want (%v,%v,%v)",
			got.EnergyDefaultPricePerKwh, got.EnergyDefaultPue, got.EnergyDefaultWhPerToken, price, pue, wh)
	}
}

// TestUpdateSystemSettingsRejectsNegativeEnergyDefault asserts each of the
// three energy-default settings rejects a negative value.
func TestUpdateSystemSettingsRejectsNegativeEnergyDefault(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	neg := -0.1
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{EnergyDefaultPricePerKwh: floatPtr(neg)}); !errors.Is(err, ErrEnergyDefaultInvalid) {
		t.Fatalf("negative energy_default_price_per_kwh err = %v, want ErrEnergyDefaultInvalid", err)
	}
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{EnergyDefaultPue: floatPtr(neg)}); !errors.Is(err, ErrEnergyDefaultInvalid) {
		t.Fatalf("negative energy_default_pue err = %v, want ErrEnergyDefaultInvalid", err)
	}
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{EnergyDefaultWhPerToken: floatPtr(neg)}); !errors.Is(err, ErrEnergyDefaultInvalid) {
		t.Fatalf("negative energy_default_wh_per_token err = %v, want ErrEnergyDefaultInvalid", err)
	}
}

func TestRouteAffinitySessionModeHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"nil -> default", nil, "client_session"},
		{"absent -> default", map[string]string{}, "client_session"},
		{"blank -> default", map[string]string{routeAffinitySessionModeKey: ""}, "client_session"},
		{"unknown -> default", map[string]string{routeAffinitySessionModeKey: "bogus"}, "client_session"},
		{"explicit client_session", map[string]string{routeAffinitySessionModeKey: "client_session"}, "client_session"},
		{"explicit legacy_header", map[string]string{routeAffinitySessionModeKey: "legacy_header"}, "legacy_header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RouteAffinitySessionMode(tc.in); got != tc.want {
				t.Fatalf("RouteAffinitySessionMode(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUpdateSystemSettingsRejectsUnknownRouteAffinitySessionMode(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{RouteAffinitySessionMode: strPtr("bogus")})
	if !errors.Is(err, ErrRouteAffinitySessionModeInvalid) {
		t.Fatalf("UpdateSystemSettings(bogus) error = %v, want ErrRouteAffinitySessionModeInvalid", err)
	}
}

func TestUpdateSystemSettingsPersistsRouteAffinitySessionMode(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{RouteAffinitySessionMode: strPtr("legacy_header")})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if got.RouteAffinitySessionMode != "legacy_header" {
		t.Fatalf("returned DTO RouteAffinitySessionMode = %q, want %q", got.RouteAffinitySessionMode, "legacy_header")
	}
	// It persists: the stored value + the concrete accessor both reflect it.
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings returned error: %v", err)
	}
	if values[routeAffinitySessionModeKey] != "legacy_header" {
		t.Fatalf("stored route_affinity_session_mode = %q, want %q", values[routeAffinitySessionModeKey], "legacy_header")
	}
	if mode := svc.RouteAffinitySessionMode(context.Background()); mode != "legacy_header" {
		t.Fatalf("Service.RouteAffinitySessionMode() = %q, want %q", mode, "legacy_header")
	}
}

func TestNetbirdAgentDownloadOnlySetting(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	if svc.NetbirdAgentDownloadOnly(ctx) {
		t.Fatal("default should be false")
	}
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdAgentDownloadOnly: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if !got.NetbirdAgentDownloadOnly {
		t.Fatalf("returned DTO NetbirdAgentDownloadOnly = %v, want true", got.NetbirdAgentDownloadOnly)
	}
	if !svc.NetbirdAgentDownloadOnly(ctx) {
		t.Fatal("should be true after update")
	}
	dto := svc.SystemSettingsView(ctx)
	if !dto.NetbirdAgentDownloadOnly {
		t.Fatalf("SystemSettingsView() NetbirdAgentDownloadOnly = %v, want true", dto.NetbirdAgentDownloadOnly)
	}
}

func TestSystemAdminModeRequirePasswordSetting(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	// Default true when unset (fail safe — never silently drop the step-up
	// password requirement).
	if !svc.SystemAdminModeRequirePassword(ctx) {
		t.Fatalf("default = false, want true")
	}
	dto := svc.SystemSettingsView(ctx)
	if !dto.SystemAdminModeRequirePassword {
		t.Fatalf("SystemSettingsView() SystemAdminModeRequirePassword = %v, want true", dto.SystemAdminModeRequirePassword)
	}

	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{SystemAdminModeRequirePassword: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if got.SystemAdminModeRequirePassword {
		t.Fatalf("returned DTO SystemAdminModeRequirePassword = %v, want false", got.SystemAdminModeRequirePassword)
	}
	if svc.SystemAdminModeRequirePassword(ctx) {
		t.Fatal("after set false = true, want false")
	}
	dto = svc.SystemSettingsView(ctx)
	if dto.SystemAdminModeRequirePassword {
		t.Fatalf("SystemSettingsView() SystemAdminModeRequirePassword = %v, want false", dto.SystemAdminModeRequirePassword)
	}
}

func TestSystemAdminModeRequirePasswordNilStoreDefaultsTrue(t *testing.T) {
	svc := NewService(ServiceDeps{Clock: fixedClock()})
	if !svc.SystemAdminModeRequirePassword(context.Background()) {
		t.Fatalf("Service.SystemAdminModeRequirePassword() with nil store = false, want true")
	}
}

func TestSystemAdminModeRequirePasswordHelperDefaults(t *testing.T) {
	if !SystemAdminModeRequirePassword(map[string]string{}) {
		t.Fatal("absent key should default to true")
	}
	if !SystemAdminModeRequirePassword(map[string]string{systemAdminModeRequirePasswordKey: "  "}) {
		t.Fatal("blank value should default to true")
	}
	if !SystemAdminModeRequirePassword(map[string]string{systemAdminModeRequirePasswordKey: "not-a-bool"}) {
		t.Fatal("unparseable value should default to true")
	}
	if SystemAdminModeRequirePassword(map[string]string{systemAdminModeRequirePasswordKey: "false"}) {
		t.Fatal(`explicit "false" should be false`)
	}
	if !SystemAdminModeRequirePassword(map[string]string{systemAdminModeRequirePasswordKey: "true"}) {
		t.Fatal(`explicit "true" should be true`)
	}
}

// newSettingsService wraps the construction style already established by
// every test above (NewService with a fresh MemorySystemSettings + a fixed
// clock, a background context) so the certificate tests below don't repeat
// it inline.
func newSettingsService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	svc := NewService(ServiceDeps{
		SystemSettings:          NewMemorySystemSettings(),
		Clock:                   fixedClock(),
		AgentTLSPort:            8443,
		AgentTLSSeparateDefault: false,
	})
	return svc, context.Background()
}

func TestACMEReadersDefaults(t *testing.T) {
	values := map[string]string{}
	if CertEnabled(values) {
		t.Fatal("acme must default to OFF")
	}
	if got := ACMEDirectoryURL(values); got != DefaultACMEDirectoryURL {
		t.Fatalf("directory = %q, want the Let's Encrypt production URL", got)
	}
	if got := CertServerScope(values); got != "selected" {
		t.Fatalf("scope = %q, want selected", got)
	}
	if got := CertRenewBeforeDays(values); got != 30 {
		t.Fatalf("renew-before = %d, want 30", got)
	}
	// Out-of-range / garbage values fall back to the documented default.
	if got := CertRenewBeforeDays(map[string]string{"cert_renew_before_days": "2"}); got != 30 {
		t.Fatalf("below-floor renew-before = %d, want the 30 default", got)
	}
	if got := CertServerScope(map[string]string{"cert_server_scope": "nonsense"}); got != "selected" {
		t.Fatalf("unknown scope = %q, want selected", got)
	}
	if got := CertPublicDomains(map[string]string{"cert_public_domains": " a.example.net , ,b.example.net "}); len(got) != 2 || got[0] != "a.example.net" || got[1] != "b.example.net" {
		t.Fatalf("public domains = %v, want the two trimmed names", got)
	}
	// F1.6: CertPublicDomains must be non-nil even when empty -- it feeds
	// SystemSettingsDTO.CertPublicDomains, typed string[] on the frontend, and
	// a nil Go slice marshals to JSON null, breaking that contract.
	for _, values := range []map[string]string{
		nil,
		{},
		{"cert_public_domains": ""},
		{"cert_public_domains": " , , "},
	} {
		got := CertPublicDomains(values)
		if got == nil {
			t.Fatalf("CertPublicDomains(%v) = nil, want a non-nil empty slice", values)
		}
		if raw, err := json.Marshal(got); err != nil || string(raw) != "[]" {
			t.Fatalf("CertPublicDomains(%v) marshaled = %q (err %v), want []", values, raw, err)
		}
	}
	// Issuer mode + the self-signed knobs.
	if got := CertIssuerMode(values); got != "acme" {
		t.Fatalf("issuer mode = %q, want acme by default", got)
	}
	if got := CertIssuerMode(map[string]string{"cert_issuer_mode": "self_signed"}); got != "self_signed" {
		t.Fatalf("issuer mode = %q, want self_signed", got)
	}
	if got := CertIssuerMode(map[string]string{"cert_issuer_mode": "nonsense"}); got != "acme" {
		t.Fatalf("unknown issuer mode = %q, want the acme default", got)
	}
	if got := CertSelfSignedValidityDays(values); got != 365 {
		t.Fatalf("self-signed validity = %d, want 365", got)
	}
	for _, bad := range []string{"0", "4000", "junk"} {
		if got := CertSelfSignedValidityDays(map[string]string{"cert_self_signed_validity_days": bad}); got != 365 {
			t.Fatalf("out-of-range validity %q = %d, want the 365 default", bad, got)
		}
	}
	if got := CertCARenewBeforeDays(values); got != 365 {
		t.Fatalf("CA renew-before = %d, want 365", got)
	}
	if got := CertCARenewBeforeDays(map[string]string{"cert_ca_renew_before_days": "5"}); got != 365 {
		t.Fatalf("below-floor CA renew-before = %d, want the 365 default", got)
	}
}

// TestCertPublicIssuerModeDefaultsEmpty pins CertPublicIssuerMode's "follow
// the global mode" sentinel: absent, blank, and unknown values must ALL read
// back "" (never a fallback like CertIssuerMode's "acme" or
// CertEdgeIssuerMode's "self_signed" -- see modeFor).
func TestCertPublicIssuerModeDefaultsEmpty(t *testing.T) {
	if got := CertPublicIssuerMode(map[string]string{}); got != "" {
		t.Fatalf("blank default = %q, want empty", got)
	}
	if got := CertPublicIssuerMode(map[string]string{"cert_public_issuer_mode": "sideways"}); got != "" {
		t.Fatalf("unknown-value default = %q, want empty", got)
	}
	if got := CertPublicIssuerMode(map[string]string{"cert_public_issuer_mode": "acme"}); got != IssuerModeACME {
		t.Fatalf("stored acme = %q, want acme preserved", got)
	}
	if got := CertPublicIssuerMode(map[string]string{"cert_public_issuer_mode": "self_signed"}); got != IssuerModeSelfSigned {
		t.Fatalf("stored self_signed = %q, want self_signed preserved", got)
	}
}

// TestACMESharedReadersDefaultTrue pins the DELIBERATELY inverted default (vs.
// every other cert_* on/off switch, which defaults false): absent or
// unparseable must both mean "shared", the byte-neutral state -- an upgraded
// deployment keeps using the ONE global ACME account it always used.
func TestACMESharedReadersDefaultTrue(t *testing.T) {
	for name, reader := range map[string]func(map[string]string) bool{
		"edge":   CertEdgeACMEShared,
		"public": CertPublicACMEShared,
	} {
		if got := reader(map[string]string{}); !got {
			t.Fatalf("%s: absent = false, want true (shared is the default)", name)
		}
	}
	if got := CertEdgeACMEShared(map[string]string{certEdgeACMESharedKey: "not-a-bool"}); !got {
		t.Fatal("edge: unparseable = false, want true")
	}
	if got := CertEdgeACMEShared(map[string]string{certEdgeACMESharedKey: "false"}); got {
		t.Fatal("edge: explicit false = true, want false")
	}
	if got := CertPublicACMEShared(map[string]string{certPublicACMESharedKey: "false"}); got {
		t.Fatal("public: explicit false = true, want false")
	}
}

// TestACMEWeeklyLimitReadersDefaultZero covers the three weekly-limit readers
// (global + the two per-context ones): absent, unparseable, and negative all
// fall back to 0 ("no limit set here") -- reads never fail.
func TestACMEWeeklyLimitReadersDefaultZero(t *testing.T) {
	for name, reader := range map[string]func(map[string]string) int{
		"global": ACMEWeeklyLimit,
		"edge":   CertEdgeACMEWeeklyLimit,
		"public": CertPublicACMEWeeklyLimit,
	} {
		if got := reader(map[string]string{}); got != 0 {
			t.Fatalf("%s: absent = %d, want 0", name, got)
		}
	}
	if got := ACMEWeeklyLimit(map[string]string{acmeWeeklyLimitKey: "junk"}); got != 0 {
		t.Fatalf("unparseable = %d, want 0", got)
	}
	if got := ACMEWeeklyLimit(map[string]string{acmeWeeklyLimitKey: "-1"}); got != 0 {
		t.Fatalf("negative = %d, want 0", got)
	}
	if got := ACMEWeeklyLimit(map[string]string{acmeWeeklyLimitKey: "7"}); got != 7 {
		t.Fatalf("stored 7 = %d, want 7 preserved", got)
	}
}

func TestUpdateSystemSettingsACMERoundTripAndValidation(t *testing.T) {
	svc, ctx := newSettingsService(t)
	enabled := true
	email := " ops@example.test "
	scope := "all"
	days := 45
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:         &enabled,
		ACMEEmail:           &email,
		CertServerScope:     &scope,
		CertRenewBeforeDays: &days,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !dto.CertEnabled || dto.ACMEEmail != "ops@example.test" || dto.CertServerScope != "all" || dto.CertRenewBeforeDays != 45 {
		t.Fatalf("dto = %+v", dto)
	}
	// Enabling with NO email is allowed (module on, not yet usable) -- the
	// chicken-and-egg fix: the nav item must be reachable before configuration.
	svc2, ctx2 := newSettingsService(t)
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{CertEnabled: &enabled}); err != nil {
		t.Fatalf("enabling without email must be allowed, got %v", err)
	}
	if !svc2.CertModuleChecked(ctx2) {
		t.Fatal("CertModuleChecked must report the raw checkbox")
	}
	if _, ok, err := svc2.CertSettings(ctx2); err != nil || ok {
		t.Fatalf("CertSettings ok = %v (err %v), want false while the email is missing", ok, err)
	}
	// A bad scope / bad renew window is rejected with 400-mapped ErrCertInvalid.
	bad := "sideways"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertServerScope: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad scope err = %v, want ErrCertInvalid", err)
	}
	tooSmall := 3
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertRenewBeforeDays: &tooSmall}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("below-floor renew days err = %v, want ErrCertInvalid", err)
	}
	badURL := "not a url"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{ACMEDirectoryURL: &badURL}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad directory url err = %v, want ErrCertInvalid", err)
	}
	// The full settings become usable once the email is set.
	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("CertSettings ok = %v err = %v, want usable", ok, err)
	}
	if set.ServerScope != "all" || set.RenewBeforeDays != 45 || set.Email != "ops@example.test" {
		t.Fatalf("settings = %+v", set)
	}
	if set.IssuerMode != "acme" {
		t.Fatalf("issuer mode = %q, want the acme default", set.IssuerMode)
	}
}

func TestSelfSignedModeIsUsableWithoutAnEmail(t *testing.T) {
	svc, ctx := newSettingsService(t)
	on := true
	mode := "self_signed"
	days := 90
	caDays := 400
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:                &on,
		CertIssuerMode:             &mode,
		CertSelfSignedValidityDays: &days,
		CertCARenewBeforeDays:      &caDays,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if dto.CertIssuerMode != "self_signed" || dto.CertSelfSignedValidityDays != 90 || dto.CertCARenewBeforeDays != 400 {
		t.Fatalf("dto = %+v", dto)
	}
	// No ACME email is needed in this mode -- the internal CA has no registrar.
	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("CertSettings ok = %v err = %v, want usable without an email", ok, err)
	}
	if set.IssuerMode != "self_signed" || set.SelfSignedValidityDays != 90 {
		t.Fatalf("settings = %+v", set)
	}
	// Out-of-range writes are rejected, not silently clamped.
	badMode := "sideways"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &badMode}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad issuer mode err = %v, want ErrCertInvalid", err)
	}
	for _, bad := range []int{0, 4000} {
		v := bad
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertSelfSignedValidityDays: &v}); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("validity %d err = %v, want ErrCertInvalid", bad, err)
		}
	}
	tooSmallCA := 10
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertCARenewBeforeDays: &tooSmallCA}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("CA renew-before err = %v, want ErrCertInvalid", err)
	}
}

func TestValidateEdgeNameRejectsNginxInjection(t *testing.T) {
	for _, bad := range []string{
		"edge.lan; return 301 http://evil",
		"edge.lan}",
		"edge.lan #comment",
		"edge.lan\nserver {",
		"-edge.lan",
		"edge..lan",
		"",
		// edge;lan isolates the ';' as the ONLY illegal character (every
		// other case above also carries a space/brace/newline/leading-hyphen
		// violation of its own) -- without it, widening the character
		// whitelist to also accept ';' would not flip any case above to
		// "valid" and this test would stay green despite a real regression.
		"edge;lan",
	} {
		if err := ValidateEdgeName(bad); err == nil {
			t.Errorf("ValidateEdgeName(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"edge.lan", "op-gw.intern", "10.0.0.5", "fe80::1", "a"} {
		if err := ValidateEdgeName(good); err != nil {
			t.Errorf("ValidateEdgeName(%q) = %v, want nil", good, err)
		}
	}
}

func TestUpdateSystemSettingsRejectsAnIPInEdgeAcmeMode(t *testing.T) {
	svc, ctx := newSettingsService(t)
	mode := IssuerModeACME
	names := []string{"10.0.0.5"}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode: &mode,
		CertEdgeNames:      &names,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("err = %v, want ErrCertInvalid", err)
	}
}

// TestUpdateSystemSettingsRejectsAnAlreadyStoredIPWhenSwitchingToAcme covers
// the effective-mode case the brief calls out explicitly: switching the mode
// to acme while an IP is ALREADY stored (CertEdgeNames not touched by this
// PUT) must still be rejected -- not just the combined-write case above.
func TestUpdateSystemSettingsRejectsAnAlreadyStoredIPWhenSwitchingToAcme(t *testing.T) {
	svc, ctx := newSettingsService(t)
	selfSigned := IssuerModeSelfSigned
	names := []string{"10.0.0.5"}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode: &selfSigned,
		CertEdgeNames:      &names,
	}); err != nil {
		t.Fatalf("seed self_signed write: %v", err)
	}
	acme := IssuerModeACME
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeIssuerMode: &acme}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("switch-to-acme err = %v, want ErrCertInvalid", err)
	}
}

func TestEdgeSettingsRoundTrip(t *testing.T) {
	svc, ctx := newSettingsService(t)
	enabled := true
	// The internal cert_enabled master toggle + issuer mode gate whether
	// CertSettings() reports ok=true at all (existing, untouched-by-this-task
	// behavior -- see CertSettings' doc comment); self_signed needs no ACME
	// email, so this keeps the fixture minimal while still exercising the
	// full "usable" path this test asserts against.
	internalMode := IssuerModeSelfSigned
	mode := IssuerModeSelfSigned
	names := []string{"Edge.lan", " 10.0.0.5"}
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:        &enabled,
		CertIssuerMode:     &internalMode,
		CertEdgeEnabled:    &enabled,
		CertEdgeIssuerMode: &mode,
		CertEdgeNames:      &names,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !dto.CertEdgeEnabled || dto.CertEdgeIssuerMode != IssuerModeSelfSigned {
		t.Fatalf("dto = %+v", dto)
	}
	if len(dto.CertEdgeNames) != 2 || dto.CertEdgeNames[0] != "edge.lan" || dto.CertEdgeNames[1] != "10.0.0.5" {
		t.Fatalf("dto.CertEdgeNames = %v (must be trimmed + lowercased, order kept)", dto.CertEdgeNames)
	}

	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("CertSettings: ok=%v err=%v", ok, err)
	}
	if !set.EdgeEnabled || set.EdgeIssuerMode != IssuerModeSelfSigned {
		t.Fatalf("edge = %+v", set)
	}
	if len(set.EdgeNames) != 2 || set.EdgeNames[0] != "edge.lan" || set.EdgeNames[1] != "10.0.0.5" {
		t.Fatalf("EdgeNames = %v (must be trimmed + lowercased, order kept)", set.EdgeNames)
	}
}

// TestCertEdgeRequireHTTPSRoundTrip is the plan-B analogue of
// TestEdgeSettingsRoundTrip: it proves cert_edge_require_https travels
// UpdateSystemSettings -> SystemSettingsDTO -> CertEdgeRequireHTTPSChecked
// unmodified. The arming precondition is NOT exercised here -- it needs the
// edgeSchemeTracker living in internal/gateway.
func TestCertEdgeRequireHTTPSRoundTrip(t *testing.T) {
	svc, ctx := newSettingsService(t)
	enabled := true
	// The internal cert_enabled master toggle + issuer mode gate whether
	// CertSettings() reports ok=true at all (existing, untouched-by-this-task
	// behavior -- see TestEdgeSettingsRoundTrip's identical fixture note).
	internalMode := IssuerModeSelfSigned
	mode := IssuerModeSelfSigned
	requireHTTPS := true
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:          &enabled,
		CertIssuerMode:       &internalMode,
		CertEdgeEnabled:      &enabled,
		CertEdgeIssuerMode:   &mode,
		CertEdgeRequireHTTPS: &requireHTTPS,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !dto.CertEdgeRequireHTTPS {
		t.Fatalf("dto.CertEdgeRequireHTTPS = false, want true")
	}

	// Read back through the ONE correct reader. CertSettings deliberately carries
	// no EdgeRequireHTTPS field (it is cert_enabled-gated, which is wrong for this
	// switch -- see CertEdgeRequireHTTPSChecked's doc and the test below).
	if !svc.CertEdgeRequireHTTPSChecked(ctx) {
		t.Fatal("CertEdgeRequireHTTPSChecked = false, want true")
	}

	// A PUT that omits the field (nil) must leave the stored value untouched --
	// mirrors every other *bool field in UpdateSystemSettingsRequest.
	otherMode := IssuerModeSelfSigned
	dto2, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeIssuerMode: &otherMode})
	if err != nil {
		t.Fatalf("no-touch update: %v", err)
	}
	if !dto2.CertEdgeRequireHTTPS {
		t.Fatalf("dto2.CertEdgeRequireHTTPS = false after an unrelated write, want true (nil must mean keep)")
	}

	// And the inverse: switching it back off must actually clear it.
	disabled := false
	dto3, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeRequireHTTPS: &disabled})
	if err != nil {
		t.Fatalf("disable update: %v", err)
	}
	if dto3.CertEdgeRequireHTTPS {
		t.Fatalf("dto3.CertEdgeRequireHTTPS = true after explicitly disabling, want false")
	}
}

// TestCertMeshRequireTLSRoundTrip is P3's analogue: cert_mesh_require_tls travels
// UpdateSystemSettings -> SystemSettingsDTO -> CertMeshRequireTLSChecked unmodified,
// nil means keep, and false clears. Like the edge switch it is NOT part of
// CertSettings (it must act even when the gateway declines its own issuance), so
// the only reader is CertMeshRequireTLSChecked. The arming precondition lives in
// internal/gateway (it needs the AgentTransportRegistry) and is not exercised here.
func TestCertMeshRequireTLSRoundTrip(t *testing.T) {
	svc, ctx := newSettingsService(t)
	enabled := true
	internalMode := IssuerModeSelfSigned
	requireTLS := true
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:        &enabled,
		CertIssuerMode:     &internalMode,
		CertMeshRequireTLS: &requireTLS,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !dto.CertMeshRequireTLS {
		t.Fatalf("dto.CertMeshRequireTLS = false, want true")
	}
	if !svc.CertMeshRequireTLSChecked(ctx) {
		t.Fatal("CertMeshRequireTLSChecked = false, want true")
	}

	// nil must keep the stored value.
	otherMode := IssuerModeSelfSigned
	dto2, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &otherMode})
	if err != nil {
		t.Fatalf("no-touch update: %v", err)
	}
	if !dto2.CertMeshRequireTLS {
		t.Fatalf("dto2.CertMeshRequireTLS = false after an unrelated write, want true (nil must mean keep)")
	}

	// false must clear it.
	disabled := false
	dto3, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertMeshRequireTLS: &disabled})
	if err != nil {
		t.Fatalf("disable update: %v", err)
	}
	if dto3.CertMeshRequireTLS {
		t.Fatalf("dto3.CertMeshRequireTLS = true after explicitly disabling, want false")
	}
}

func TestCertMeshTLSModeRoundTripAndValidation(t *testing.T) {
	svc, ctx := newSettingsService(t)
	sep := "separate"
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &sep})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if dto.CertMeshTLSMode != "separate" {
		t.Fatalf("dto.CertMeshTLSMode = %q, want separate", dto.CertMeshTLSMode)
	}
	// nil keeps it.
	other := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEnabled: &other}); err != nil {
		t.Fatalf("no-touch update: %v", err)
	}
	got := svc.SystemSettingsView(ctx)
	if got.CertMeshTLSMode != "separate" {
		t.Fatalf("SystemSettingsView().CertMeshTLSMode = %q after an unrelated write, want separate (nil must mean keep)", got.CertMeshTLSMode)
	}
	// invalid value rejected.
	bad := "sideways"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad mode err = %v, want ErrCertInvalid", err)
	}
	// reader: only exact combined/separate survive; anything else -> "".
	if CertMeshTLSMode(map[string]string{"cert_mesh_tls_mode": "combined"}) != "combined" {
		t.Fatal("combined not read back")
	}
	if CertMeshTLSMode(map[string]string{"cert_mesh_tls_mode": "sideways"}) != "" {
		t.Fatal("unknown must read as empty (follow env)")
	}
	if CertMeshTLSMode(map[string]string{}) != "" {
		t.Fatal("absent must read as empty")
	}
}

// TestCertHTTPSSwitchModeRoundTripAndValidation covers P4's two new settings
// (cert_https_switch_mode, cert_proxy_listen_port_base): both round-trip
// through UpdateSystemSettings -> SystemSettingsDTO, both are gated by
// touchesCert (a PUT carrying only one of them still reaches the cert-view
// validation/write block), and both reject an invalid value with
// ErrCertInvalid while leaving the prior stored value intact.
func TestCertHTTPSSwitchModeRoundTripAndValidation(t *testing.T) {
	svc, ctx := newSettingsService(t)

	// Absent/blank -> manual (byte-neutral default).
	if got := svc.SystemSettingsView(ctx).CertHTTPSSwitchMode; got != "manual" {
		t.Fatalf("default CertHTTPSSwitchMode = %q, want manual", got)
	}
	if got := svc.SystemSettingsView(ctx).CertProxyListenPortBase; got != 8600 {
		t.Fatalf("default CertProxyListenPortBase = %d, want 8600", got)
	}

	auto := "auto"
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &auto})
	if err != nil {
		t.Fatalf("update mode: %v", err)
	}
	if dto.CertHTTPSSwitchMode != "auto" {
		t.Fatalf("dto.CertHTTPSSwitchMode = %q, want auto", dto.CertHTTPSSwitchMode)
	}
	// touchesCert recognizes a PUT carrying ONLY CertHTTPSSwitchMode.
	if !(UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &auto}).touchesCert() {
		t.Fatal("touchesCert() = false for a request carrying only CertHTTPSSwitchMode, want true")
	}

	selected := "selected"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &selected}); err != nil {
		t.Fatalf("update mode to selected: %v", err)
	}
	if got := svc.SystemSettingsView(ctx).CertHTTPSSwitchMode; got != "selected" {
		t.Fatalf("CertHTTPSSwitchMode = %q, want selected preserved", got)
	}

	// nil keeps it.
	base := 8700
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertProxyListenPortBase: &base}); err != nil {
		t.Fatalf("update port base: %v", err)
	}
	if got := svc.SystemSettingsView(ctx).CertHTTPSSwitchMode; got != "selected" {
		t.Fatalf("CertHTTPSSwitchMode = %q after an unrelated write, want selected (nil must mean keep)", got)
	}
	if got := svc.SystemSettingsView(ctx).CertProxyListenPortBase; got != 8700 {
		t.Fatalf("CertProxyListenPortBase = %d, want 8700", got)
	}
	// touchesCert recognizes a PUT carrying ONLY CertProxyListenPortBase.
	if !(UpdateSystemSettingsRequest{CertProxyListenPortBase: &base}).touchesCert() {
		t.Fatal("touchesCert() = false for a request carrying only CertProxyListenPortBase, want true")
	}

	// invalid mode rejected, prior value untouched.
	bad := "sometimes"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad mode err = %v, want ErrCertInvalid", err)
	}
	if got := svc.SystemSettingsView(ctx).CertHTTPSSwitchMode; got != "selected" {
		t.Fatalf("a rejected write must not persist: %q", got)
	}

	// out-of-range port base rejected (both directions), prior value untouched.
	for _, bad := range []int{1023, 65536} {
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertProxyListenPortBase: &bad}); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("port base %d err = %v, want ErrCertInvalid", bad, err)
		}
	}
	if got := svc.SystemSettingsView(ctx).CertProxyListenPortBase; got != 8700 {
		t.Fatalf("a rejected port-base write must not persist: %d", got)
	}

	// reader: only exact auto/selected survive; anything else -> manual.
	if CertHTTPSSwitchMode(map[string]string{"cert_https_switch_mode": "auto"}) != "auto" {
		t.Fatal("auto not read back")
	}
	if CertHTTPSSwitchMode(map[string]string{"cert_https_switch_mode": "sideways"}) != "manual" {
		t.Fatal("unknown must read as manual")
	}
	if CertHTTPSSwitchMode(map[string]string{}) != "manual" {
		t.Fatal("absent must read as manual")
	}
	// reader: out-of-range/unparseable port base -> the 8600 default.
	for _, bad := range []string{"1023", "65536", "junk"} {
		if got := CertProxyListenPortBase(map[string]string{"cert_proxy_listen_port_base": bad}); got != 8600 {
			t.Fatalf("out-of-range port base %q = %d, want the 8600 default", bad, got)
		}
	}
	if got := CertProxyListenPortBase(map[string]string{"cert_proxy_listen_port_base": "1024"}); got != 1024 {
		t.Fatalf("floor 1024 = %d, want 1024 preserved", got)
	}
	if got := CertProxyListenPortBase(map[string]string{"cert_proxy_listen_port_base": "65535"}); got != 65535 {
		t.Fatalf("ceiling 65535 = %d, want 65535 preserved", got)
	}
}

func TestCertMeshTLSSeparateActiveFollowsEnvWhenUnset(t *testing.T) {
	// default dep AgentTLSSeparateDefault=false: "" mode -> not active.
	svc, ctx := newSettingsService(t) // helper builds deps with defaults (false)
	if active, err := svc.CertMeshTLSSeparateActive(ctx); err != nil || active {
		t.Fatalf("unset+env-false must be inactive, got active=%v err=%v", active, err)
	}
	sep := "separate"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &sep}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if active, err := svc.CertMeshTLSSeparateActive(ctx); err != nil || !active {
		t.Fatalf("explicit separate must be active, got active=%v err=%v", active, err)
	}
	comb := "combined"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &comb}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if active, err := svc.CertMeshTLSSeparateActive(ctx); err != nil || active {
		t.Fatalf("explicit combined must be inactive, got active=%v err=%v", active, err)
	}
}

// TestCertPublicACMERoundTrip is TestCertEdgeRequireHTTPSRoundTrip's analogue
// for the public-domain issuer mode + own ACME config fields this task adds:
// cert_public_issuer_mode, cert_public_acme_shared/_email/_directory_url/
// _weekly_limit all travel UpdateSystemSettings -> SystemSettingsDTO
// unmodified, nil means keep, and touchesCert recognizes a PUT carrying only
// CertPublicACMEShared.
func TestCertPublicACMERoundTrip(t *testing.T) {
	svc, ctx := newSettingsService(t)
	enabled := true
	internalMode := IssuerModeSelfSigned
	publicMode := IssuerModeACME
	shared := false
	email := "pub@example.test"
	dirURL := "https://pub.example.test/directory"
	weeklyLimit := 5
	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:                &enabled,
		CertIssuerMode:             &internalMode,
		CertPublicIssuerMode:       &publicMode,
		CertPublicACMEShared:       &shared,
		CertPublicACMEEmail:        &email,
		CertPublicACMEDirectoryURL: &dirURL,
		CertPublicACMEWeeklyLimit:  &weeklyLimit,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if dto.CertPublicIssuerMode != publicMode {
		t.Fatalf("dto.CertPublicIssuerMode = %q, want %q", dto.CertPublicIssuerMode, publicMode)
	}
	if dto.CertPublicACMEShared {
		t.Fatalf("dto.CertPublicACMEShared = true, want false")
	}
	if dto.CertPublicACMEEmail != email {
		t.Fatalf("dto.CertPublicACMEEmail = %q, want %q", dto.CertPublicACMEEmail, email)
	}
	if dto.CertPublicACMEDirectoryURL != dirURL {
		t.Fatalf("dto.CertPublicACMEDirectoryURL = %q, want %q", dto.CertPublicACMEDirectoryURL, dirURL)
	}
	if dto.CertPublicACMEWeeklyLimit != weeklyLimit {
		t.Fatalf("dto.CertPublicACMEWeeklyLimit = %d, want %d", dto.CertPublicACMEWeeklyLimit, weeklyLimit)
	}

	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("settings not usable: ok=%v err=%v", ok, err)
	}
	if set.modeFor("public") != IssuerModeACME {
		t.Fatalf("modeFor(public) = %q, want acme (explicit override)", set.modeFor("public"))
	}
	if dir, e, limit := set.certAcmeConfigFor("public"); dir != dirURL || e != email || limit != weeklyLimit {
		t.Fatalf("certAcmeConfigFor(public) = (%q,%q,%d), want (%q,%q,%d)", dir, e, limit, dirURL, email, weeklyLimit)
	}

	// nil must keep every field untouched.
	otherMode := IssuerModeSelfSigned
	dto2, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &otherMode})
	if err != nil {
		t.Fatalf("no-touch update: %v", err)
	}
	if dto2.CertPublicIssuerMode != publicMode || dto2.CertPublicACMEShared ||
		dto2.CertPublicACMEEmail != email || dto2.CertPublicACMEDirectoryURL != dirURL ||
		dto2.CertPublicACMEWeeklyLimit != weeklyLimit {
		t.Fatalf("an unrelated write must not disturb the public ACME block, got %+v", dto2)
	}

	// touchesCert must recognize a PUT carrying ONLY CertPublicACMEShared.
	only := UpdateSystemSettingsRequest{CertPublicACMEShared: &shared}
	if !only.touchesCert() {
		t.Fatal("touchesCert() = false for a request carrying only CertPublicACMEShared, want true")
	}
}

// TestUpdateSystemSettingsPublicSharedIgnoresBareIPDirectoryGuard is the
// final-review regression: the bare-IP ACME-directory guard must fire ONLY for a
// context that actually USES its own directory (cert_public_acme_shared=false).
// While the context is SHARED, its stored own directory is inert -- issuance
// reads the global acme_directory_url -- so an inert bare-IP value must never
// block a save, least of all a panel-1 PUT that merely flips the GLOBAL issuer
// mode to acme (the public block re-validates on its cert_issuer_mode trigger
// because public with cert_public_issuer_mode="" follows the global mode). The
// UI hides the own-directory field while shared, so such a value is otherwise
// unclearable.
func TestUpdateSystemSettingsPublicSharedIgnoresBareIPDirectoryGuard(t *testing.T) {
	svc, ctx := newSettingsService(t)
	on := true
	selfSigned := IssuerModeSelfSigned
	acme := IssuerModeACME
	followGlobal := ""
	notShared := false
	shared := true
	bareIPDir := "https://10.0.0.5/directory"

	// Step 1: store a bare-IP OWN public directory while it is inert -- public
	// follows the global mode, which is self_signed, so the guard is skipped and
	// the value persists.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:                &on,
		CertIssuerMode:             &selfSigned,
		CertPublicIssuerMode:       &followGlobal,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &bareIPDir,
	}); err != nil {
		t.Fatalf("step 1 (store an inert bare-IP own directory under self_signed) must succeed, got: %v", err)
	}

	// Step 2: flip the public context back to SHARED -- the bare-IP own directory
	// is now inert (issuance will use the global acme_directory_url).
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertPublicACMEShared: &shared}); err != nil {
		t.Fatalf("step 2 (flip public to shared) must succeed, got: %v", err)
	}

	// Step 3: THE REGRESSION -- flipping the GLOBAL issuer mode to acme must not
	// be blocked by the now-inert, shared-context bare-IP own directory.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &acme}); err != nil {
		t.Fatalf("step 3 (global mode -> acme with an inert shared-context bare-IP own directory) must succeed, got: %v", err)
	}

	// Guard intact: the SAME bare-IP directory under an OWN (shared=false) public
	// context with effective mode acme is still rejected -- that directory would
	// really be used for issuance. Sent as the full public partition, exactly as
	// the UI's savePublic does.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertPublicIssuerMode:       &acme,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &bareIPDir,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("own (shared=false) bare-IP directory under effective acme must still be rejected, got: %v", err)
	}
}

// TestUpdateSystemSettingsEdgeSharedIgnoresBareIPDirectoryGuard is the edge
// analogue of the public regression above: while cert_edge_acme_shared is true
// the edge's stored own directory is inert, so a PUT that switches the edge mode
// to acme must not be rejected by an inert bare-IP cert_edge_acme_directory_url.
func TestUpdateSystemSettingsEdgeSharedIgnoresBareIPDirectoryGuard(t *testing.T) {
	svc, ctx := newSettingsService(t)
	selfSigned := IssuerModeSelfSigned
	acme := IssuerModeACME
	notShared := false
	shared := true
	bareIPDir := "https://10.0.0.5/directory"

	// Step 1: store a bare-IP OWN edge directory while the edge mode is
	// self_signed (guard skipped, value persists).
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode:       &selfSigned,
		CertEdgeACMEShared:       &notShared,
		CertEdgeACMEDirectoryURL: &bareIPDir,
	}); err != nil {
		t.Fatalf("step 1 (store an inert bare-IP own edge directory under self_signed) must succeed, got: %v", err)
	}

	// Step 2: flip the edge context back to SHARED (its own directory is now inert).
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeACMEShared: &shared}); err != nil {
		t.Fatalf("step 2 (flip edge to shared) must succeed, got: %v", err)
	}

	// Step 3: THE REGRESSION -- switching the edge mode to acme must not be blocked
	// by the now-inert, shared-context bare-IP own directory.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeIssuerMode: &acme}); err != nil {
		t.Fatalf("step 3 (edge mode -> acme with an inert shared-context bare-IP own directory) must succeed, got: %v", err)
	}

	// Guard intact: an OWN (shared=false) edge context under acme with the same
	// bare-IP directory is still rejected.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode:       &acme,
		CertEdgeACMEShared:       &notShared,
		CertEdgeACMEDirectoryURL: &bareIPDir,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("own (shared=false) bare-IP edge directory under acme must still be rejected, got: %v", err)
	}
}

// TestUpdateSystemSettingsSharedFlipToOwnRevalidatesBareIPDirectory is the
// final-review HARDENING: flipping a context back to its OWN account via a
// PARTIAL PUT carrying only *_acme_shared (no mode/directory field) must
// re-validate the now-LIVE stored directory. The outer triggers used to omit
// *_acme_shared, so such a partial flip slipped an inert bare-IP own directory
// into live use unchecked (only reachable via the raw API -- the real UI always
// sends the full partition, which re-validates). The triggers now include
// *_acme_shared so the bare-IP guard fires on a shared->own flip too.
func TestUpdateSystemSettingsSharedFlipToOwnRevalidatesBareIPDirectory(t *testing.T) {
	on := true
	selfSigned := IssuerModeSelfSigned
	acme := IssuerModeACME
	followGlobal := ""
	notShared := false
	shared := true
	bareIPDir := "https://10.0.0.5/directory"

	// --- public ---
	svc, ctx := newSettingsService(t)
	// Store an inert bare-IP own public directory (public follows the self_signed
	// global mode, so the guard is skipped) ...
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:                &on,
		CertIssuerMode:             &selfSigned,
		CertPublicIssuerMode:       &followGlobal,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &bareIPDir,
	}); err != nil {
		t.Fatalf("public seed (own bare-IP dir under self_signed): %v", err)
	}
	// ... then reach the inert-but-acme state: shared=true while the global mode
	// is acme (public follows it). The primary fix lets this persist.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertIssuerMode:       &acme,
		CertPublicACMEShared: &shared,
	}); err != nil {
		t.Fatalf("public seed (mode acme, shared true, dir inert): %v", err)
	}
	// The PARTIAL flip back to own must now be re-validated and rejected.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertPublicACMEShared: &notShared}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("public: a shared->own flip carrying only cert_public_acme_shared must re-validate the now-live bare-IP directory, got: %v", err)
	}

	// --- edge ---
	svc2, ctx2 := newSettingsService(t)
	// Store an inert bare-IP own edge directory under the self_signed edge mode ...
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode:       &selfSigned,
		CertEdgeACMEShared:       &notShared,
		CertEdgeACMEDirectoryURL: &bareIPDir,
	}); err != nil {
		t.Fatalf("edge seed (own bare-IP dir under self_signed): %v", err)
	}
	// ... then reach edge mode acme while shared=true (dir inert). The primary
	// fix lets this persist.
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode: &acme,
		CertEdgeACMEShared: &shared,
	}); err != nil {
		t.Fatalf("edge seed (mode acme, shared true, dir inert): %v", err)
	}
	// The PARTIAL flip back to own must now be re-validated and rejected.
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{CertEdgeACMEShared: &notShared}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("edge: a shared->own flip carrying only cert_edge_acme_shared must re-validate the now-live bare-IP directory, got: %v", err)
	}
}

// TestCertEdgeRequireHTTPSCheckedBypassesTheModuleGate is the fix-round-1 test: the design
// intends the plaintext gate to be usable even for a HAND-INSTALLED certificate -- an
// operator who declines the gateway's own issuance entirely (cert_enabled=false),
// terminates TLS with a certificate they installed themselves, and still wants
// plaintext refused. CertSettings() is the WRONG read path for that: it
// short-circuits to the zero-value struct with ok=false the moment
// cert_enabled is off (see CertSettings' doc comment), which is exactly why it
// carries no EdgeRequireHTTPS field at all. The dedicated
// CertEdgeRequireHTTPSChecked reader must see the TRUE stored value in that exact
// state.
func TestCertEdgeRequireHTTPSCheckedBypassesTheModuleGate(t *testing.T) {
	svc, ctx := newSettingsService(t)
	requireHTTPS := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeRequireHTTPS: &requireHTTPS}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Sanity: the internal cert module was never enabled, so the gated reader
	// reports not-ok/false -- this is the exact state the ungated reader must
	// see through.
	if svc.CertModuleChecked(ctx) {
		t.Fatal("CertModuleChecked = true, want false (cert_enabled was never set)")
	}
	if _, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("CertSettings ok = %v (err %v), want false while cert_enabled is off", ok, err)
	}
	if !svc.CertEdgeRequireHTTPSChecked(ctx) {
		t.Fatal("CertEdgeRequireHTTPSChecked = false, want true -- it must read the raw stored switch, ungated by cert_enabled")
	}
}

// TestCertEdgeRequireHTTPSDefaultsFalse mirrors TestCertEdgeIssuerModeDefaultsToSelfSignedNotAcme's
// shape for the boolean reader: absent, blank, or unparseable must all read
// back false -- an unset env-independent kill switch means the setting has
// never been armed, never a silent lockout.
func TestCertEdgeRequireHTTPSDefaultsFalse(t *testing.T) {
	if got := CertEdgeRequireHTTPS(map[string]string{}); got {
		t.Fatalf("CertEdgeRequireHTTPS(absent) = true, want false")
	}
	if got := CertEdgeRequireHTTPS(map[string]string{"cert_edge_require_https": "not-a-bool"}); got {
		t.Fatalf("CertEdgeRequireHTTPS(unparseable) = true, want false")
	}
	if got := CertEdgeRequireHTTPS(map[string]string{"cert_edge_require_https": "true"}); !got {
		t.Fatalf("CertEdgeRequireHTTPS(stored true) = false, want true")
	}
}

// TestCertEdgeIssuerModeDefaultsToSelfSignedNotAcme pins the deliberately
// DIFFERENT default from the internal cert_issuer_mode (which defaults to
// acme): an edge name is typically an internal hostname or bare IP that a
// public CA cannot validate, so an unconfigured/unknown stored value must
// resolve to self_signed, not the acme default the sibling reader uses.
func TestCertEdgeIssuerModeDefaultsToSelfSignedNotAcme(t *testing.T) {
	if got := CertEdgeIssuerMode(map[string]string{}); got != IssuerModeSelfSigned {
		t.Fatalf("blank default = %q, want self_signed", got)
	}
	if got := CertEdgeIssuerMode(map[string]string{"cert_edge_issuer_mode": "sideways"}); got != IssuerModeSelfSigned {
		t.Fatalf("unknown-value default = %q, want self_signed", got)
	}
	if got := CertEdgeIssuerMode(map[string]string{"cert_edge_issuer_mode": "acme"}); got != IssuerModeACME {
		t.Fatalf("stored acme = %q, want acme preserved", got)
	}
}

// TestCertEdgeNamesNeverJSONNull mirrors the CertPublicDomains contract:
// SystemSettingsDTO.CertEdgeNames is typed string[] on the frontend, so an
// absent/blank setting must decode to a non-nil empty slice, never nil (which
// marshals to JSON null).
func TestCertEdgeNamesNeverJSONNull(t *testing.T) {
	got := CertEdgeNames(map[string]string{})
	if got == nil {
		t.Fatal("CertEdgeNames() = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("CertEdgeNames() = %v, want empty", got)
	}
}

// TestUpdateSystemSettingsRejectsAnIPACMEDirectoryHost is
// TestUpdateSystemSettingsRejectsAnIPInEdgeAcmeMode's analogue for the new
// per-context ACME directory URLs: a bare IP host cannot be validated over
// HTTP-01, so it is rejected for cert_edge_acme_directory_url /
// cert_public_acme_directory_url whenever that context's EFFECTIVE issuer
// mode is acme -- mirroring the existing cert_edge_names rule exactly.
//
// Final-review fix: the guard applies ONLY when the own directory is actually
// LIVE (cert_edge_acme_shared / cert_public_acme_shared == false); while a
// context is shared, its own directory is inert and must not be validated. Every
// rejection case below therefore sets the context to own (shared=false) so the
// bare-IP directory is the one issuance would really use. The inert (shared)
// case -- where the same value must NOT block a save -- is covered by
// TestUpdateSystemSettings{Public,Edge}SharedIgnoresBareIPDirectoryGuard.
func TestUpdateSystemSettingsRejectsAnIPACMEDirectoryHost(t *testing.T) {
	notShared := false
	svc, ctx := newSettingsService(t)
	edgeMode := IssuerModeACME
	edgeDir := "https://10.0.0.5/directory"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode:       &edgeMode,
		CertEdgeACMEShared:       &notShared,
		CertEdgeACMEDirectoryURL: &edgeDir,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("edge: err = %v, want ErrCertInvalid", err)
	}

	svc2, ctx2 := newSettingsService(t)
	publicMode := IssuerModeACME
	publicDir := "https://198.51.100.7/directory"
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{
		CertPublicIssuerMode:       &publicMode,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &publicDir,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("public: err = %v, want ErrCertInvalid", err)
	}

	// The SAME bare-IP directory is fine in self_signed mode (it is simply
	// never consumed) -- only acme mode rejects it -- even when it is the
	// context's own (shared=false) directory.
	svc3, ctx3 := newSettingsService(t)
	selfSigned := IssuerModeSelfSigned
	if _, err := svc3.UpdateSystemSettings(ctx3, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeIssuerMode:       &selfSigned,
		CertEdgeACMEShared:       &notShared,
		CertEdgeACMEDirectoryURL: &edgeDir,
	}); err != nil {
		t.Fatalf("self_signed edge: unexpected error %v", err)
	}

	// A public domain's mode FOLLOWING the global (cert_public_issuer_mode
	// left unset) must resolve the effective mode from cert_issuer_mode --
	// setting the global to acme while writing only the (own) public directory
	// must still be rejected.
	svc4, ctx4 := newSettingsService(t)
	globalACME := IssuerModeACME
	if _, err := svc4.UpdateSystemSettings(ctx4, systemToken(), UpdateSystemSettingsRequest{
		CertIssuerMode:             &globalACME,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &publicDir,
	}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("public-follows-global: err = %v, want ErrCertInvalid", err)
	}
}

// TestUpdateSystemSettingsRejectsAnAlreadyStoredPublicBareIPWhenGlobalModeSwitchesToAcme
// is review round-1's regression test: the outer trigger for the public
// bare-IP-directory check used to be `req.CertPublicIssuerMode != nil ||
// req.CertPublicACMEDirectoryURL != nil`, which skipped the check entirely for
// a PUT that touches ONLY cert_issuer_mode (the global mode) -- even though
// the check's own body already resolves the effective public mode from
// req.CertIssuerMode when present. A public row left FOLLOWING the global
// mode, with an already-stored bare-IP directory that was legal while the
// global mode was self_signed, must be caught the moment the global mode
// switches to acme -- in its OWN PUT, not just when the directory field is
// touched in the same request (TestUpdateSystemSettingsRejectsAnIPACMEDirectoryHost's
// "public-follows-global" case only covered the combined-write shape).
//
// Final-review fix: the guard is now correctly gated on the EFFECTIVE
// cert_public_acme_shared being false -- it applies ONLY when the own directory
// is actually LIVE. So the seed here sets cert_public_acme_shared=false; the
// stored bare-IP directory is the account issuance would really use, and the
// global-mode switch to acme must still be rejected. The complementary
// SHARED-context (inert directory) case -- where the same switch must SUCCEED --
// is TestUpdateSystemSettingsPublicSharedIgnoresBareIPDirectoryGuard.
func TestUpdateSystemSettingsRejectsAnAlreadyStoredPublicBareIPWhenGlobalModeSwitchesToAcme(t *testing.T) {
	svc, ctx := newSettingsService(t)
	selfSigned := IssuerModeSelfSigned
	notShared := false
	badDir := "https://198.51.100.9/directory"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertIssuerMode:             &selfSigned,
		CertPublicACMEShared:       &notShared,
		CertPublicACMEDirectoryURL: &badDir,
	}); err != nil {
		t.Fatalf("seed self_signed write: %v", err)
	}
	globalACME := IssuerModeACME
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &globalACME}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("err = %v, want ErrCertInvalid", err)
	}
}

// TestUpdateSystemSettingsRejectsNegativeACMEWeeklyLimit covers the three
// weekly-limit fields this task adds: each must reject a negative value with
// ErrCertInvalid, mirroring every other numeric cert_*/acme_* floor check.
func TestUpdateSystemSettingsRejectsNegativeACMEWeeklyLimit(t *testing.T) {
	neg := -1
	for name, req := range map[string]UpdateSystemSettingsRequest{
		"global": {ACMEWeeklyLimit: &neg},
		"edge":   {CertEdgeACMEWeeklyLimit: &neg},
		"public": {CertPublicACMEWeeklyLimit: &neg},
	} {
		svc, ctx := newSettingsService(t)
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), req); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("%s: err = %v, want ErrCertInvalid", name, err)
		}
	}
	// 0 is legal (== "no limit set").
	svc, ctx := newSettingsService(t)
	zero := 0
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{ACMEWeeklyLimit: &zero}); err != nil {
		t.Fatalf("zero weekly limit: unexpected error %v", err)
	}
}

// TestUpdateSystemSettingsRejectsBadPerContextACMEEmail mirrors the existing
// ACMEEmail validation (must contain '@' when non-empty) for the two new
// per-context email fields.
func TestUpdateSystemSettingsRejectsBadPerContextACMEEmail(t *testing.T) {
	bad := "not-an-email"
	svc, ctx := newSettingsService(t)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeACMEEmail: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("edge: err = %v, want ErrCertInvalid", err)
	}
	svc2, ctx2 := newSettingsService(t)
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{CertPublicACMEEmail: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("public: err = %v, want ErrCertInvalid", err)
	}
}

// TestUpdateSystemSettingsRejectsMalformedPerContextACMEDirectoryURL mirrors
// the existing ACMEDirectoryURL shape validation (http/https scheme, non-empty
// host) for the two new per-context directory URL fields.
func TestUpdateSystemSettingsRejectsMalformedPerContextACMEDirectoryURL(t *testing.T) {
	bad := "not a url"
	svc, ctx := newSettingsService(t)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEdgeACMEDirectoryURL: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("edge: err = %v, want ErrCertInvalid", err)
	}
	svc2, ctx2 := newSettingsService(t)
	if _, err := svc2.UpdateSystemSettings(ctx2, systemToken(), UpdateSystemSettingsRequest{CertPublicACMEDirectoryURL: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("public: err = %v, want ErrCertInvalid", err)
	}
}

// TestUpdateSystemSettingsRejectsUnknownCertPublicIssuerMode mirrors
// TestSelfSignedModeIsUsableWithoutAnEmail's rejection style: an unknown
// cert_public_issuer_mode value is rejected outright, never silently
// defaulted -- but "" (clearing back to "follow the global mode") is legal,
// unlike cert_issuer_mode/cert_edge_issuer_mode which both reject "".
func TestUpdateSystemSettingsRejectsUnknownCertPublicIssuerMode(t *testing.T) {
	svc, ctx := newSettingsService(t)
	bad := "sideways"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertPublicIssuerMode: &bad}); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("err = %v, want ErrCertInvalid", err)
	}
	empty := ""
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertPublicIssuerMode: &empty}); err != nil {
		t.Fatalf("clearing to empty (follow global) must be legal, got %v", err)
	}
}
