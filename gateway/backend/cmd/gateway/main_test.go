// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGatewayDevTokenRejectsDefaultTokenOnNonLoopbackBind(t *testing.T) {
	_, err := gatewayDevToken("0.0.0.0:8080", "")

	if err == nil {
		t.Fatalf("gatewayDevToken returned nil error for non-loopback default token")
	}
}

func TestGatewayDevTokenAllowsDefaultTokenOnLoopbackBind(t *testing.T) {
	token, err := gatewayDevToken("127.0.0.1:8080", "")
	if err != nil {
		t.Fatalf("gatewayDevToken returned %v", err)
	}
	if token != "dev-secret" {
		t.Fatalf("token = %q, want dev-secret", token)
	}
}

func TestGatewayDevTokenAllowsExplicitTokenOnNonLoopbackBind(t *testing.T) {
	token, err := gatewayDevToken("0.0.0.0:8080", "explicit-secret")
	if err != nil {
		t.Fatalf("gatewayDevToken returned %v", err)
	}
	if token != "explicit-secret" {
		t.Fatalf("token = %q, want explicit-secret", token)
	}
}

func TestGatewayDevAgentSecretRejectsDefaultSecretOnNonLoopbackBind(t *testing.T) {
	_, err := gatewayDevAgentSecret("0.0.0.0:8080", "")

	if err == nil {
		t.Fatalf("gatewayDevAgentSecret returned nil error for non-loopback default secret")
	}
}

func TestGatewayDevAgentSecretAllowsDefaultSecretOnLoopbackBind(t *testing.T) {
	secret, err := gatewayDevAgentSecret("127.0.0.1:8080", "")
	if err != nil {
		t.Fatalf("gatewayDevAgentSecret returned %v", err)
	}
	if secret != "dev-agent-secret" {
		t.Fatalf("secret = %q, want dev-agent-secret", secret)
	}
}

func TestGatewayDevAgentSecretAllowsExplicitSecretOnNonLoopbackBind(t *testing.T) {
	secret, err := gatewayDevAgentSecret("0.0.0.0:8080", "explicit-agent-secret")
	if err != nil {
		t.Fatalf("gatewayDevAgentSecret returned %v", err)
	}
	if secret != "explicit-agent-secret" {
		t.Fatalf("secret = %q, want explicit-agent-secret", secret)
	}
}

func TestNewHTTPServerConfiguresTimeouts(t *testing.T) {
	srv := newHTTPServer(context.Background(), "127.0.0.1:8080", nil)

	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %s, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout = %s, want > 0", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Fatalf("WriteTimeout = %s, want > 0", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %s, want > 0", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout > 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want bounded timeout", srv.ReadHeaderTimeout)
	}
}

func TestBuildGatewayServerUsesMemoryStoreByDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	rec := performChatCompletion(t, srv, "dev-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBuildGatewayServerDispatchesConfiguredProviderRoutes(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Errorf, not Fatalf: this runs on the server's goroutine, where
		// Fatalf's FailNow is not allowed to stop the test. Kept strict — with
		// always_reachable on the application below, nothing but the dispatched
		// completion may reach this upstream, so an unexpected request is a
		// real finding rather than background noise to tolerate.
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Decode upstream request returned %v", err)
			return
		}
		upstreamModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"routed through vllm"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"}
	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	now := time.Now().UTC()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	upstreamHost, upstreamPortStr, err := net.SplitHostPort(upstreamURL.Host)
	if err != nil {
		t.Fatalf("split upstream host: %v", err)
	}
	upstreamPort, err := strconv.Atoi(upstreamPortStr)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	// The resolver routes via applications + mappings; reconstruct the upstream
	// origin as the application endpoint (scheme://domain:port) so the vLLM
	// client dispatches to the httptest upstream.
	if err := srv.Routes.CreateAIServer(context.Background(), routing.AIServer{ID: "host_vllm", Name: "vLLM Host", Domain: upstreamHost, Provider: "vllm", Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed AI server: %v", err)
	}
	// always_reachable keeps the app-health loop from probing this upstream.
	// buildGatewayServer's health loop runs one pass IMMEDIATELY at startup
	// (runAppHealthLoop) and this application is seeded right after that call
	// returns, so whether the pass observes it is pure scheduling — and when it
	// did, the probe GET landed on the httptest handler below (path "/", the
	// empty HealthCheckPath) and failed the test. This scenario is about route
	// dispatch, not health probing; the same fixture flag is why the sibling
	// native-passthrough e2e test never saw this.
	if err := srv.Routes.CreateApplication(context.Background(), routing.Application{ID: "app_vllm", ServerID: "host_vllm", Type: routing.ProviderVLLM, Port: upstreamPort, Scheme: upstreamURL.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 100, Weight: 100, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, AlwaysReachable: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := srv.Routes.CreateMapping(context.Background(), routing.ModelMapping{ID: "map_vllm", ApplicationID: "app_vllm", GatewayModelName: "vllm-model", AppModelName: "actual-vllm", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	rec := performChatCompletionForModel(t, srv, "dev-secret", "vllm-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("routed through vllm")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if upstreamModel != "actual-vllm" {
		t.Fatalf("upstream model = %q, want actual-vllm", upstreamModel)
	}
}

func TestBuildGatewayServerExposesMemoryPortalMe(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory", DefaultLanguage: "de"}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID                string `json:"id"`
		Email             string `json:"email"`
		PreferredLanguage string `json:"preferred_language"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.ID != "usr_dev" || body.Email != "dev@example.test" || body.PreferredLanguage != "de" {
		t.Fatalf("body = %#v", body)
	}
}

func TestBuildGatewayServerUsesSQLiteStoreAndBootstrapToken(t *testing.T) {
	cfg := config.Config{
		Addr:                "127.0.0.1:8080",
		DBDriver:            "sqlite",
		SQLitePath:          filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:         true,
		BootstrapAdminEmail: "admin@example.test",
		BootstrapAdminName:  "Admin User",
		BootstrapAPIToken:   "sqlite-secret",
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	rec := performChatCompletion(t, srv, "sqlite-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBuildGatewayServerAllowsSameSQLiteBootstrapTokenOnRestart(t *testing.T) {
	cfg := config.Config{
		Addr:                "127.0.0.1:8080",
		DBDriver:            "sqlite",
		SQLitePath:          filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:         true,
		BootstrapAdminEmail: "admin@example.test",
		BootstrapAdminName:  "Admin User",
		BootstrapAPIToken:   "sqlite-secret",
	}
	first, firstCleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("first buildGatewayServer returned %v", err)
	}
	if rec := performChatCompletion(t, first, "sqlite-secret"); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := firstCleanup(); err != nil {
		t.Fatalf("first cleanup returned %v", err)
	}

	second, secondCleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("second buildGatewayServer returned %v", err)
	}
	defer secondCleanup()
	if rec := performChatCompletion(t, second, "sqlite-secret"); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrapSQLiteSeedsLoginablePasswordWhenSet(t *testing.T) {
	sqliteStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite returned %v", err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned %v", err)
	}
	cfg := config.Config{
		BootstrapAdminEmail:    "capture-admin@example.test",
		BootstrapAdminName:     "Capture Admin",
		BootstrapAPIToken:      "bootstrap-secret",
		BootstrapAdminPassword: "sup3r-secret-pass",
		DefaultLanguage:        "de",
	}
	if err := bootstrapAdmin(context.Background(), sqliteStore, cfg); err != nil {
		t.Fatalf("bootstrapAdmin returned %v", err)
	}

	user, err := sqliteStore.UserByEmail(context.Background(), cfg.BootstrapAdminEmail)
	if err != nil {
		t.Fatalf("UserByEmail returned %v", err)
	}
	if user.PasswordHash == "" {
		t.Fatalf("PasswordHash is empty, want a hash that verifies against the bootstrap password")
	}
	// Verify against the exact primitive the login path uses.
	if !auth.VerifyPassword(user.PasswordHash, cfg.BootstrapAdminPassword) {
		t.Fatalf("VerifyPassword failed for the bootstrapped admin password")
	}
	if auth.VerifyPassword(user.PasswordHash, "wrong-password") {
		t.Fatalf("VerifyPassword succeeded for a wrong password")
	}
	if user.PasswordSetAt == nil {
		t.Fatalf("PasswordSetAt is nil, want a timestamp")
	}
}

func TestBootstrapSQLiteLeavesPasswordEmptyWithoutBootstrapPassword(t *testing.T) {
	sqliteStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite returned %v", err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned %v", err)
	}
	cfg := config.Config{
		BootstrapAdminEmail: "capture-admin@example.test",
		BootstrapAdminName:  "Capture Admin",
		BootstrapAPIToken:   "bootstrap-secret",
		DefaultLanguage:     "de",
	}
	if err := bootstrapAdmin(context.Background(), sqliteStore, cfg); err != nil {
		t.Fatalf("bootstrapAdmin returned %v", err)
	}

	user, err := sqliteStore.UserByEmail(context.Background(), cfg.BootstrapAdminEmail)
	if err != nil {
		t.Fatalf("UserByEmail returned %v", err)
	}
	if user.PasswordHash != "" {
		t.Fatalf("PasswordHash = %q, want empty (invite path unchanged)", user.PasswordHash)
	}
	if user.PasswordSetAt != nil {
		t.Fatalf("PasswordSetAt = %v, want nil", user.PasswordSetAt)
	}
}

func TestBuildGatewayServerRejectsChangedSQLiteBootstrapToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	cfg := config.Config{
		Addr:                "127.0.0.1:8080",
		DBDriver:            "sqlite",
		SQLitePath:          path,
		AutoMigrate:         true,
		BootstrapAdminEmail: "admin@example.test",
		BootstrapAdminName:  "Admin User",
		BootstrapAPIToken:   "first-secret",
	}
	_, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("first buildGatewayServer returned %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup returned %v", err)
	}
	cfg.BootstrapAPIToken = "second-secret"

	_, _, err = buildGatewayServer(cfg)

	if err == nil {
		t.Fatalf("buildGatewayServer returned nil error for changed bootstrap token")
	}
}

func TestBuildGatewayServerRejectsBootstrapEmailOwnedByDifferentUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	sqliteStore, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned %v", err)
	}
	if err := sqliteStore.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned %v", err)
	}
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	if err := sqliteStore.CreateUser(context.Background(), store.User{
		ID:                "usr_other",
		Email:             "admin@example.test",
		DisplayName:       "Other Admin",
		Role:              "admin",
		Status:            store.UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}
	if err := sqliteStore.Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	cfg := config.Config{
		Addr:                "127.0.0.1:8080",
		DBDriver:            "sqlite",
		SQLitePath:          path,
		AutoMigrate:         true,
		BootstrapAdminEmail: "admin@example.test",
		BootstrapAdminName:  "Admin User",
		BootstrapAPIToken:   "sqlite-secret",
	}

	_, _, err = buildGatewayServer(cfg)

	if err == nil {
		t.Fatalf("buildGatewayServer returned nil error for conflicting bootstrap email")
	}
}

func TestBuildGatewayServerRejectsUnknownDBDriver(t *testing.T) {
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "oracle"}

	_, _, err := buildGatewayServer(cfg)

	if err == nil {
		t.Fatalf("buildGatewayServer returned nil error")
	}
}

func TestMemoryModeDevLogin(t *testing.T) {
	cfg := config.Config{
		Addr:            "127.0.0.1:0",
		PublicURL:       "http://localhost:8080",
		DefaultLanguage: "de",
		DBDriver:        "memory",
		SessionIdleTTL:  time.Hour,
		SessionMaxTTL:   24 * time.Hour,
	}
	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	defer cleanup()

	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"dev@example.test","password":"dev-secret"}`))
	login.Header.Set("X-OP-CSRF", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev login should be 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "op_ai_gateway_session" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("dev login should set a session cookie")
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8080":   "8080",
		"127.0.0.1:8091": "8091",
		":9000":          "9000",
		"":               "8080",
		"garbage":        "8080",
	}
	for in, want := range cases {
		if got := portFromAddr(in); got != want {
			t.Fatalf("portFromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSqliteDepsRejectsMalformedCaptureKey(t *testing.T) {
	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "sqlite",
		SQLitePath:           filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:          true,
		CaptureEncryptionKey: "not-hex",
	}

	if _, _, err := buildGatewayServer(cfg); err == nil {
		t.Fatalf("buildGatewayServer returned nil error for malformed capture key")
	}
}

func TestSqliteDepsWiresCaptureCipherWhenKeySet(t *testing.T) {
	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "sqlite",
		SQLitePath:           filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:          true,
		CaptureEncryptionKey: strings.Repeat("ab", 32), // 64 hex chars = 32 bytes
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	if srv.Cipher == nil {
		t.Fatalf("srv.Cipher = nil, want configured cipher")
	}
}

func TestSqliteDepsLeavesCaptureCipherNilWithoutKey(t *testing.T) {
	cfg := config.Config{
		Addr:        "127.0.0.1:8080",
		DBDriver:    "sqlite",
		SQLitePath:  filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate: true,
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	if srv.Cipher != nil {
		t.Fatalf("srv.Cipher = %v, want nil (no key -> RAM fallback, not disabled)", srv.Cipher)
	}
}

// TestSqliteDepsStartsWithoutTheCertEncryptionKey pins that the certificate key
// is NOT a boot requirement. The certificate module is optional and off by
// default, so a deployment that does not use certificates must keep starting
// with OP_AI_GATEWAY_CERT_ENCRYPTION_KEY unset; the requirement binds later, at
// sealCertSecret (which refuses on a disk store) and is surfaced through
// cert_last_error. Nobody may turn this into a startup gate by accident.
func TestSqliteDepsStartsWithoutTheCertEncryptionKey(t *testing.T) {
	cfg := config.Config{
		Addr:        "127.0.0.1:8080",
		DBDriver:    "sqlite",
		SQLitePath:  filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate: true,
		// no CertEncryptionKey (and no capture key either) on purpose
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v, want a successful start without a certificate key", err)
	}
	defer cleanup()
	if srv.Portal == nil {
		t.Fatal("srv.Portal = nil, want a wired portal service")
	}
}

// TestSqliteDepsRejectsMalformedCertKey pins that buildCertCipher is actually
// CALLED on the sqlite path (a malformed key is fatal, mirroring the capture
// key) -- if the wiring were dropped, a broken key would go unnoticed.
func TestSqliteDepsRejectsMalformedCertKey(t *testing.T) {
	cfg := config.Config{
		Addr:              "127.0.0.1:8080",
		DBDriver:          "sqlite",
		SQLitePath:        filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:       true,
		CertEncryptionKey: "not-hex",
	}

	if _, _, err := buildGatewayServer(cfg); err == nil {
		t.Fatalf("buildGatewayServer returned nil error for a malformed certificate key")
	}
}

// TestMalformedCertKeyErrorNamesTheCertificateVariable pins the operator-facing
// wording of that fatal. capture.New prefixes its errors with "capture: ", so
// returning it unwrapped told the operator to fix the CAPTURE key while the
// CERTIFICATE key was the broken one -- at the exact moment the right variable
// name matters most. The message must name the certificate variable, must not
// mention capture at all, and must never carry the key value.
func TestMalformedCertKeyErrorNamesTheCertificateVariable(t *testing.T) {
	const badKey = "not-hex-Hxyz"
	for name, key := range map[string]string{
		"not hex":      badKey,
		"wrong length": strings.Repeat("ab", 16), // valid hex, only 16 bytes
	} {
		_, err := buildCertCipher(config.Config{CertEncryptionKey: key})
		if err == nil {
			t.Fatalf("%s: buildCertCipher returned nil error", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
			t.Fatalf("%s: error = %q, want it to name OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", name, msg)
		}
		if strings.Contains(strings.ToLower(msg), "capture") {
			t.Fatalf("%s: error = %q, want no mention of capture (it points at the wrong variable)", name, msg)
		}
		if strings.Contains(msg, key) {
			t.Fatalf("%s: error leaks the key value: %q", name, msg)
		}
	}

	// A malformed key stays FATAL on the driver path (unchanged behaviour); this
	// only re-labels the message the operator reads.
	cfg := config.Config{
		Addr:              "127.0.0.1:8080",
		DBDriver:          "sqlite",
		SQLitePath:        filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:       true,
		CertEncryptionKey: badKey,
	}
	_, _, err := buildGatewayServer(cfg)
	if err == nil {
		t.Fatal("buildGatewayServer returned nil error for a malformed certificate key")
	}
	if !strings.Contains(err.Error(), "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") || strings.Contains(strings.ToLower(err.Error()), "capture") {
		t.Fatalf("buildGatewayServer error = %q, want it to name the certificate variable and not capture", err)
	}
}

// TestPostgresDepsWiresTheCertCipher pins postgresDeps' CertCipher wiring, the
// twin of TestSqliteDepsWiresCertCipherIntoThePortalService. Postgres is the
// DEFAULT production driver, but its deps builder needs a live server, so the
// behavioural end-to-end proof used for sqlite is unavailable here; this pins the
// wiring STRUCTURALLY instead. Dropping `CertCipher: certCipher` (or the
// buildCertCipher call feeding it) would otherwise leave the portal service with
// a nil certificate cipher on a non-volatile store -- self_signed issues nothing,
// ACME refuses, and the portal reports the variable as unset even though the
// operator did set it -- with the whole Go suite still green.
//
// Since CMP-1, sqliteDeps and postgresDeps share one body (sqlDeps), which in
// turn hands the cipher to buildRuntime for the actual portal.ServiceDeps
// wiring -- so this now pins the whole chain: postgresDeps -> sqlDeps (builds
// the cipher) -> buildRuntime (wires it into the portal service).
func TestPostgresDepsWiresTheCertCipher(t *testing.T) {
	if !strings.Contains(funcSource(t, "main.go", "postgresDeps"), "sqlDeps(cfg,") {
		t.Fatal("postgresDeps does not call sqlDeps -- it no longer reaches the shared certificate-cipher wiring")
	}
	sqlBody := funcSource(t, "main.go", "sqlDeps")
	if !strings.Contains(sqlBody, "buildCertCipher(cfg)") {
		t.Fatalf("sqlDeps does not contain %q -- the certificate cipher is not built for the postgres path", "buildCertCipher(cfg)")
	}
	if !containsWired(sqlBody, "CertCipher: certCipher") {
		t.Fatalf("sqlDeps does not contain %q -- the certificate cipher is not handed to buildRuntime on the postgres path", "CertCipher: certCipher")
	}
	if !containsWired(funcSource(t, "main.go", "buildRuntime"), "CertCipher: b.CertCipher") {
		t.Fatal("buildRuntime does not contain \"CertCipher: b.CertCipher\" -- the certificate cipher is not wired into the portal service")
	}
}

// funcSource returns the source text of the named top-level function in the
// given file of this package.
func funcSource(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Recv != nil {
			continue
		}
		return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
	}
	t.Fatalf("function %s not found in %s", fn, file)
	return ""
}

// TestMemoryDepsWiresTheCertEdgeOutputDir, TestSqliteDepsWiresTheCertEdgeOutputDir,
// and TestPostgresDepsWiresTheCertEdgeOutputDir pin, one per driver path, that
// config.Config.CertEdgeOutputDir reaches portal.ServiceDeps.CertEdgeOutputDir
// (which NewService stores unexported on Service, invisible to this package).
// Task 3 gives CertEdgeOutputDir no consumer yet (a later task reads it to
// decide local delivery vs. the key-download endpoint), so there is no
// observable behavior to assert against for ANY of the three drivers -- a
// source-text assertion is the only technique available here, mirroring
// TestPostgresDepsWiresTheCertCipher's structural pin. Dropping
// `CertEdgeOutputDir: cfg.CertEdgeOutputDir` from buildRuntime's ServiceDeps
// literal would silently leave every driver's Service.certEdgeOutputDir empty
// regardless of the configured variable, with the rest of the Go suite still
// green.
//
// Since CMP-1, the wiring itself lives once in buildRuntime, shared by all
// three drivers; each test below additionally pins its own driver's call
// chain into that shared body (memoryDeps -> buildRuntime directly;
// sqliteDeps/postgresDeps -> sqlDeps -> buildRuntime).
func TestMemoryDepsWiresTheCertEdgeOutputDir(t *testing.T) {
	if !strings.Contains(funcSource(t, "main.go", "memoryDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("memoryDeps does not call buildRuntime -- it no longer reaches the shared CertEdgeOutputDir wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "buildRuntime"), "CertEdgeOutputDir: cfg.CertEdgeOutputDir") {
		t.Fatalf("buildRuntime does not contain %q -- the edge output directory is not wired into the portal service", "CertEdgeOutputDir: cfg.CertEdgeOutputDir")
	}
}

func TestSqliteDepsWiresTheCertEdgeOutputDir(t *testing.T) {
	if !strings.Contains(funcSource(t, "main.go", "sqliteDeps"), "sqlDeps(cfg,") {
		t.Fatal("sqliteDeps does not call sqlDeps -- it no longer reaches the shared CertEdgeOutputDir wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "sqlDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("sqlDeps does not call buildRuntime -- it no longer reaches the shared CertEdgeOutputDir wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "buildRuntime"), "CertEdgeOutputDir: cfg.CertEdgeOutputDir") {
		t.Fatalf("buildRuntime does not contain %q -- the edge output directory is not wired into the portal service", "CertEdgeOutputDir: cfg.CertEdgeOutputDir")
	}
}

func TestPostgresDepsWiresTheCertEdgeOutputDir(t *testing.T) {
	if !strings.Contains(funcSource(t, "main.go", "postgresDeps"), "sqlDeps(cfg,") {
		t.Fatal("postgresDeps does not call sqlDeps -- it no longer reaches the shared CertEdgeOutputDir wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "sqlDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("sqlDeps does not call buildRuntime -- it no longer reaches the shared CertEdgeOutputDir wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "buildRuntime"), "CertEdgeOutputDir: cfg.CertEdgeOutputDir") {
		t.Fatalf("buildRuntime does not contain %q -- the edge output directory is not wired into the portal service", "CertEdgeOutputDir: cfg.CertEdgeOutputDir")
	}
}

// containsWired reports whether body contains want once every run of
// whitespace in both is collapsed to a single space. gateway.ServerDeps{...}
// literals (unlike the single-line portal.ServiceDeps{...} ones
// TestMemoryDepsWiresTheCertEdgeOutputDir and friends check above) are
// multi-line and gofmt column-aligns their field colons -- so the literal
// whitespace after a field name shifts whenever a longer/shorter field name
// joins the same aligned block. A plain strings.Contains on an exact
// single-space string would make these tests spuriously fail (or, worse,
// require a hand-tuned space count) on the very next unrelated field added
// nearby; collapsing whitespace makes the check robust to that.
func containsWired(body, want string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return strings.Contains(norm(body), norm(want))
}

// assertAllDriversReachBuildRuntime pins, for every CMP-1-era source-scan
// completeness guard in this package, that memoryDeps/sqliteDeps/postgresDeps
// still funnel into the ONE shared buildRuntime body (memoryDeps calls it
// directly; sqliteDeps/postgresDeps each call it via sqlDeps). Before CMP-1
// these guards counted "3 driver paths" because each driver inlined its own
// copy of the wiring; now the wiring lives once in buildRuntime (or, for the
// SQL-only pieces, once in sqlDeps) and every guard's count checks were
// updated to expect 1 instead of 3. This helper is the other half of that
// update: a driver that stopped reaching buildRuntime would silently drop
// whatever a specific guard checks, with that guard's count-based assertions
// (now satisfied by the single shared call site) still green.
func assertAllDriversReachBuildRuntime(t *testing.T) {
	t.Helper()
	if !strings.Contains(funcSource(t, "main.go", "memoryDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("memoryDeps does not call buildRuntime -- it no longer reaches the shared wiring")
	}
	for _, fn := range []string{"sqliteDeps", "postgresDeps"} {
		if !strings.Contains(funcSource(t, "main.go", fn), "sqlDeps(cfg,") {
			t.Fatalf("%s does not call sqlDeps -- it no longer reaches the shared wiring", fn)
		}
	}
	if !strings.Contains(funcSource(t, "main.go", "sqlDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("sqlDeps does not call buildRuntime -- sqlite/postgres no longer reach the shared wiring")
	}
}

// TestMemoryDepsWiresTheCertEdgeRequireHTTPSKillSwitch and
// TestSqliteDepsWiresTheCertEdgeRequireHTTPSKillSwitch call their driver builder
// directly and assert on the RETURNED gateway.ServerDeps -- a strictly stronger,
// BEHAVIOURAL pin than a source-text check, and the pattern this file already uses
// for both drivers (see e.g. TestMemoryDepsWiresSharedMemoryCaptureStore /
// TestSqliteDepsFallsBackToSharedMemoryCaptureStoreWithoutKey): both builders are
// plain functions callable in-process with a config.Config fixture, so there is no
// need to fall back to funcSource+containsWired here. TestPostgresDepsWiresThe...
// below stays source-text-only because postgresDeps needs a LIVE DSN it cannot
// fabricate in-process (mirrors the existing TestPostgresDepsWiresTheCertCipher).
//
// This value belongs on gateway.ServerDeps (not portal.ServiceDeps): its consumer
// (a later task, the plaintext-refusal gate in the serveWith path) lives entirely
// in internal/gateway and must be able to read it as a plain in-process bool on
// *Server, without going through s.Portal or any store lookup -- that is the whole
// point of an env-var kill switch that must keep working even when the portal
// itself is unreachable. The *Server-side copy in New() is separately pinned by
// TestNewCopiesCertEdgeRequireHTTPSDisableFromDeps (internal/gateway). Dropping
// `CertEdgeRequireHTTPSDisable: cfg.CertEdgeRequireHTTPSDisable` from a driver's
// ServerDeps literal would silently leave that driver's kill switch permanently
// disengaged regardless of the configured env var, with the rest of the Go suite
// still green.
func TestMemoryDepsWiresTheCertEdgeRequireHTTPSKillSwitch(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{
		Addr:                        "127.0.0.1:8080",
		DBDriver:                    "memory",
		CertEdgeRequireHTTPSDisable: true,
	}
	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if !deps.CertEdgeRequireHTTPSDisable {
		t.Fatal("ServerDeps.CertEdgeRequireHTTPSDisable = false, want true -- the memory driver did not wire the kill switch through")
	}
}

func TestSqliteDepsWiresTheCertEdgeRequireHTTPSKillSwitch(t *testing.T) {
	cfg := config.Config{
		Addr:                        "127.0.0.1:8080",
		DBDriver:                    "sqlite",
		SQLitePath:                  filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:                 true,
		CertEdgeRequireHTTPSDisable: true,
	}
	deps, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	defer cleanup()
	if !deps.CertEdgeRequireHTTPSDisable {
		t.Fatal("ServerDeps.CertEdgeRequireHTTPSDisable = false, want true -- the sqlite driver did not wire the kill switch through")
	}
}

// TestPostgresDepsWiresTheCertEdgeRequireHTTPSKillSwitch stays source-text-only:
// postgresDeps needs a live OP_AI_GATEWAY_POSTGRES_DSN it cannot fabricate
// in-process, mirroring TestPostgresDepsWiresTheCertCipher above. Since CMP-1
// the wiring itself lives once in buildRuntime (shared by all three drivers),
// so this also pins postgresDeps' call chain into that shared body
// (postgresDeps -> sqlDeps -> buildRuntime).
func TestPostgresDepsWiresTheCertEdgeRequireHTTPSKillSwitch(t *testing.T) {
	if !strings.Contains(funcSource(t, "main.go", "postgresDeps"), "sqlDeps(cfg,") {
		t.Fatal("postgresDeps does not call sqlDeps -- it no longer reaches the shared kill-switch wiring")
	}
	if !strings.Contains(funcSource(t, "main.go", "sqlDeps"), "buildRuntime(cfg, depsBackend{") {
		t.Fatal("sqlDeps does not call buildRuntime -- it no longer reaches the shared kill-switch wiring")
	}
	body := funcSource(t, "main.go", "buildRuntime")
	if !containsWired(body, "CertEdgeRequireHTTPSDisable: cfg.CertEdgeRequireHTTPSDisable") {
		t.Fatalf("buildRuntime does not contain %q -- the plaintext-refusal kill switch is not wired into the gateway server", "CertEdgeRequireHTTPSDisable: cfg.CertEdgeRequireHTTPSDisable")
	}
}

// TestSqliteDepsWiresCertCipherIntoThePortalService proves the end-to-end
// wiring: on a DISK-backed store, the internal CA can only be created when the
// portal service actually received the certificate cipher (sealCertSecret
// otherwise refuses with ErrCertKeyRequired and the pass stores nothing). The
// capture key is deliberately left UNSET so the CA can only come from
// CertEncryptionKey -- a fallback or a dropped CertCipher wiring both fail here.
func TestSqliteDepsWiresCertCipherIntoThePortalService(t *testing.T) {
	cfg := config.Config{
		Addr:              "127.0.0.1:8080",
		DBDriver:          "sqlite",
		SQLitePath:        filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:       true,
		CertEncryptionKey: strings.Repeat("cd", 32), // 64 hex chars = 32 bytes
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	ctx := context.Background()
	on := true
	mode := "self_signed"
	base := "int.example.test"
	if _, err := srv.Portal.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable the certificate module: %v", err)
	}

	srv.Portal.ReconcileCertificates(ctx)

	ca, err := srv.Portal.CertificateCAView(ctx)
	if err != nil {
		t.Fatalf("CertificateCAView: %v", err)
	}
	if !ca.Present {
		t.Fatalf("CertificateCAView.Present = false (last_error %q), want a CA -- the certificate cipher did not reach the portal service", ca.LastError)
	}
	if ca.LastError != "" {
		t.Fatalf("CertificateCAView.LastError = %q, want empty", ca.LastError)
	}
}

func TestMemoryDepsLeavesCaptureCipherNilEvenWithKey(t *testing.T) {
	// Capture is SQLite-only; the memory path never wires a cipher, even if a key
	// is configured.
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "memory",
		CaptureEncryptionKey: strings.Repeat("ab", 32),
	}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	if srv.Cipher != nil {
		t.Fatalf("srv.Cipher = %v, want nil on memory driver", srv.Cipher)
	}
}

func TestSqliteDepsWarnsWhenLoggingTokenButNoKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gateway.db")

	// Seed the DB with an ACTIVE token that opted into log_communication, then close
	// it so buildGatewayServer re-opens the same file.
	seedLoggingToken(t, dbPath)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cfg := config.Config{
		Addr:        "127.0.0.1:8080",
		DBDriver:    "sqlite",
		SQLitePath:  dbPath,
		AutoMigrate: true,
		// no CaptureEncryptionKey -> cipher stays nil -> RAM-fallback capture (not disabled)
	}
	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	// Cipher stays nil without a key; capture still runs, just via the RAM
	// fallback (see TestSqliteDepsFallsBackToSharedMemoryCaptureStoreWithoutKey).
	if srv.Cipher != nil {
		t.Fatalf("srv.Cipher = %v, want nil (no key -> RAM fallback, not fail-closed)", srv.Cipher)
	}
	if got := buf.String(); !strings.Contains(got, "log_communication enabled") {
		t.Fatalf("expected capture-disabled warning, got %q", got)
	}
}

func TestSqliteDepsNoWarnWhenKeySetDespiteLoggingToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	seedLoggingToken(t, dbPath)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "sqlite",
		SQLitePath:           dbPath,
		AutoMigrate:          true,
		CaptureEncryptionKey: strings.Repeat("ab", 32), // key present -> capture enabled
	}
	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	if srv.Cipher == nil {
		t.Fatalf("srv.Cipher = nil, want configured cipher")
	}
	if got := buf.String(); strings.Contains(got, "log_communication enabled") {
		t.Fatalf("did not expect capture-disabled warning when key is set, got %q", got)
	}
}

// seedLoggingToken creates a store at dbPath containing one ACTIVE token whose
// log_communication is enabled, then closes it so buildGatewayServer can re-open it.
func seedLoggingToken(t *testing.T, dbPath string) {
	t.Helper()
	seed, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate seed store: %v", err)
	}
	now := time.Now().UTC()
	if err := seed.CreateUser(context.Background(), store.User{
		ID: "usr_log", Email: "log@example.test", DisplayName: "Log User",
		Role: "user", Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create seed user: %v", err)
	}
	if err := seed.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: "tok_log", UserID: "usr_log", Name: "logging-token",
		Status: store.TokenStatusActive, LogCommunication: true,
	}, "seed-secret-value-1234567890"); err != nil {
		t.Fatalf("create seed token: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

func performChatCompletion(t *testing.T, handler http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	return performChatCompletionForModel(t, handler, token, "qwen-coder")
}

func performChatCompletionForModel(t *testing.T, handler http.Handler, token string, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestProviderClientsWireModelListerForEveryApplicationType(t *testing.T) {
	mux := providerClients(0, false, nil)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"m1"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	// Every application type selectable in the drill-down UI must be wired to a
	// functioning ModelLister so SyncApplicationModels works against a reachable
	// upstream. Mock is exercised elsewhere; this guards the real client seam.
	types := []string{
		routing.ProviderOllama,
		routing.ProviderVLLM,
		routing.ProviderLlamaCPP,
		routing.ProviderLlamaSwap,
		routing.ProviderLiteLLM,
	}
	for _, providerType := range types {
		t.Run(providerType, func(t *testing.T) {
			models, err := mux.ListModels(context.Background(), routing.Target{Provider: providerType, Endpoint: upstream.URL})
			if err != nil {
				t.Fatalf("ListModels(%q) error = %v, want nil", providerType, err)
			}
			if len(models) == 0 {
				t.Fatalf("ListModels(%q) returned empty model list", providerType)
			}
		})
	}
}

func TestMemoryDepsWiresSharedMemoryCaptureStore(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{
		Addr:                  "127.0.0.1:8080",
		DBDriver:              "memory",
		CaptureMemoryMaxBytes: 4096,
	}

	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if deps.Cipher != nil {
		t.Fatalf("ServerDeps.Cipher = %v, want nil on memory driver", deps.Cipher)
	}
	mem, ok := deps.Captures.(*store.MemoryCaptureStore)
	if !ok || mem == nil {
		t.Fatalf("ServerDeps.Captures = %T, want a non-nil *store.MemoryCaptureStore", deps.Captures)
	}

	raw, err := json.Marshal(map[string]any{
		"req_headers": map[string][]string{}, "req_body": "hi",
		"resp_headers": map[string][]string{}, "resp_body": "yo", "truncated": false,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := mem.SaveCapture(context.Background(), store.Capture{
		UsageEventID: "req_shared", OwnerUserID: "usr_dev", KeyVersion: 0,
		Blob: gzBuf.Bytes(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	// Portal must have received the SAME instance: reading through Portal
	// must see the write made directly through the ServerDeps-side handle.
	detail, err := deps.Portal.CaptureDetail(auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}, "req_shared")
	if err != nil {
		t.Fatalf("Portal.CaptureDetail via shared store: %v", err)
	}
	if detail.RespBody != "yo" {
		t.Fatalf("RespBody = %q, want yo", detail.RespBody)
	}
}

func TestSqliteDepsFallsBackToSharedMemoryCaptureStoreWithoutKey(t *testing.T) {
	cfg := config.Config{
		Addr:        "127.0.0.1:8080",
		DBDriver:    "sqlite",
		SQLitePath:  filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate: true,
		// no CaptureEncryptionKey -> falls back to MemoryCaptureStore, not sqliteStore.
		CaptureMemoryMaxBytes: 4096,
	}

	deps, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	defer cleanup()

	if deps.Cipher != nil {
		t.Fatalf("Cipher = %v, want nil without a key", deps.Cipher)
	}
	mem, ok := deps.Captures.(*store.MemoryCaptureStore)
	if !ok || mem == nil {
		t.Fatalf("Captures = %T, want *store.MemoryCaptureStore (RAM fallback)", deps.Captures)
	}

	// Same shared-instance identity check as the memory path
	// (TestMemoryDepsWiresSharedMemoryCaptureStore): a write through the
	// ServerDeps-side handle must be visible through Portal, proving sqliteDeps
	// handed the SAME MemoryCaptureStore to both sides rather than building two
	// separate instances (the B4 defect a type-only assertion cannot catch).
	// KeyVersion 0 + nil cipher -> plain gzip, so Portal.CaptureDetail gunzips
	// the blob directly.
	raw, err := json.Marshal(map[string]any{
		"req_headers": map[string][]string{}, "req_body": "hi",
		"resp_headers": map[string][]string{}, "resp_body": "yo", "truncated": false,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := mem.SaveCapture(context.Background(), store.Capture{
		UsageEventID: "req_shared", OwnerUserID: "usr_dev", KeyVersion: 0,
		Blob: gzBuf.Bytes(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	// Portal must have received the SAME instance: reading through Portal must
	// see the write made directly through the ServerDeps-side handle.
	detail, err := deps.Portal.CaptureDetail(auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}, "req_shared")
	if err != nil {
		t.Fatalf("Portal.CaptureDetail via shared store: %v", err)
	}
	if detail.RespBody != "yo" {
		t.Fatalf("RespBody = %q, want yo", detail.RespBody)
	}
}

func TestSqliteDepsKeepsSQLiteCaptureStoreWithKey(t *testing.T) {
	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "sqlite",
		SQLitePath:           filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:          true,
		CaptureEncryptionKey: strings.Repeat("ab", 32),
	}

	deps, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	defer cleanup()

	if _, ok := deps.Captures.(*store.MemoryCaptureStore); ok {
		t.Fatal("Captures = *store.MemoryCaptureStore, want *store.SQLiteStore when a key is configured")
	}
	if deps.Cipher == nil {
		t.Fatal("Cipher = nil, want configured cipher when a key is set")
	}
}

// TestWarnIfEncryptionKeysShared pins the separation advisory: reusing one value
// for BOTH encryption keys silently defeats the whole point of giving
// certificates their own key (they could no longer be scoped or rotated
// independently of captures, chat transcripts, the SMTP password and the NetBird
// token), and nothing else in the process notices -- each cipher is built
// separately and works fine on its own. The warning must also never print any
// part of a key.
func TestWarnIfEncryptionKeysShared(t *testing.T) {
	const shared = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	other := strings.Repeat("cd", 32)

	captureLog := func(t *testing.T, cfg config.Config) string {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		warnIfEncryptionKeysShared(cfg)
		return buf.String()
	}

	// Same value for both -> warn, naming BOTH variables, leaking NO key material.
	out := captureLog(t, config.Config{CaptureEncryptionKey: shared, CertEncryptionKey: shared})
	if !strings.Contains(out, "OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY") || !strings.Contains(out, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("log = %q, want a warning naming BOTH key variables", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Fatalf("log = %q, want it at WARN level (Debug is invisible by default)", out)
	}
	if strings.Contains(out, shared) || strings.Contains(out, shared[:16]) {
		t.Fatalf("the warning leaks key material: %q", out)
	}
	// Whitespace differences are not a different key.
	if out := captureLog(t, config.Config{CaptureEncryptionKey: " " + shared, CertEncryptionKey: shared + "\n"}); !strings.Contains(out, "same value") {
		t.Fatalf("log = %q, want the keys compared after trimming", out)
	}
	// Neither is CASE: hex.DecodeString accepts both cases, so an uppercased
	// value (a secret-manager UI, a retyped key) decodes to the very same AES key
	// -- the separation would be silently ineffective while the guard stayed quiet.
	if out := captureLog(t, config.Config{CaptureEncryptionKey: shared, CertEncryptionKey: strings.ToUpper(shared)}); !strings.Contains(out, "same value") {
		t.Fatalf("log = %q, want the keys compared case-insensitively (hex is case-insensitive)", out)
	}

	// Every non-shared combination stays silent.
	for name, cfg := range map[string]config.Config{
		"different":    {CaptureEncryptionKey: shared, CertEncryptionKey: other},
		"only capture": {CaptureEncryptionKey: shared},
		"only cert":    {CertEncryptionKey: shared},
		"neither (both empty must NOT count as equal)": {},
	} {
		if out := captureLog(t, cfg); out != "" {
			t.Fatalf("%s: log = %q, want no warning", name, out)
		}
	}
}

// TestWrapListenerWithFakeRemoteAddrOverridesAcceptedConnRemoteAddr proves the
// override serveMainListener relies on: a REAL TCP connection through the
// wrapped listener reports the fixed fake address, not the genuine loopback
// peer -- exactly what lets the e2e:certificates suite exercise the plaintext
// gate's refuse/observe paths from a same-machine harness (see
// certEdgeGateTestRemoteAddrEnv's doc comment).
func TestWrapListenerWithFakeRemoteAddrOverridesAcceptedConnRemoteAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	wrapped, err := wrapListenerWithFakeRemoteAddr(ln, "203.0.113.50:1")
	if err != nil {
		t.Fatalf("wrapListenerWithFakeRemoteAddr: %v", err)
	}

	accepted := make(chan string, 1)
	go func() {
		c, err := wrapped.Accept()
		if err != nil {
			accepted <- ""
			return
		}
		defer c.Close()
		accepted <- c.RemoteAddr().String()
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()

	if got := <-accepted; got != "203.0.113.50:1" {
		t.Fatalf("accepted conn RemoteAddr = %q, want the fake %q (real peer would have been loopback)", got, "203.0.113.50:1")
	}
}

// TestWrapListenerWithFakeRemoteAddrRejectsAnUnparseableAddr: a malformed
// value must fail fast at setup time (not silently fall through to the real
// RemoteAddr, which would make a misconfigured test look armed against
// nothing).
func TestWrapListenerWithFakeRemoteAddrRejectsAnUnparseableAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	if _, err := wrapListenerWithFakeRemoteAddr(ln, "not-an-address"); err == nil {
		t.Fatal("expected an error for an unparseable fake remote address")
	}
}

// TestWrapListenerWithFakeRemoteAddrRejectsLoopbackAndUnspecifiedValues is the
// fail-safe-only guarantee: a loopback or unspecified fake value would make
// every connection look like the exact "not a real hop" case
// countsAsObservation/edgeGateInternalCaller already treat specially, making
// the plaintext gate simultaneously inert and permanently unarmable (the
// README-certificates.md §8.7 failure mode) -- so every one of these must be
// rejected at setup time, never silently accepted.
func TestWrapListenerWithFakeRemoteAddrRejectsLoopbackAndUnspecifiedValues(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	for _, fake := range []string{
		"127.0.0.1:1", // the exact value a careless copy-paste would produce
		"localhost:1", // resolves to a loopback IP too
		"0.0.0.0:1",   // unspecified IPv4
		"[::]:1",      // unspecified IPv6
		"[::1]:1",     // loopback IPv6
		":1",          // bare port, no host -- resolves with a nil/unspecified IP
	} {
		t.Run(fake, func(t *testing.T) {
			if _, err := wrapListenerWithFakeRemoteAddr(ln, fake); err == nil {
				t.Fatalf("wrapListenerWithFakeRemoteAddr(%q): expected a rejection, got nil error", fake)
			}
		})
	}
}

// TestWrapListenerWithFakeRemoteAddrAcceptsANonLoopbackValue is the positive
// counterpart: a genuine non-loopback, non-unspecified value (what
// playwright.certificates.config.ts actually sets) must still be accepted --
// the rejection above must not have become over-broad.
func TestWrapListenerWithFakeRemoteAddrAcceptsANonLoopbackValue(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	if _, err := wrapListenerWithFakeRemoteAddr(ln, "203.0.113.90:1"); err != nil {
		t.Fatalf("wrapListenerWithFakeRemoteAddr(%q): unexpected error: %v", "203.0.113.90:1", err)
	}
}

// TestServeMainListenerUsesListenAndServeWhenTheEnvVarIsUnset pins the
// no-op-by-default invariant: with certEdgeGateTestRemoteAddrEnv unset (every
// real deployment), serveMainListener must take the plain ListenAndServe path
// -- proven indirectly by observing that a real TCP client connecting to it
// sees the ACTUAL loopback peer address the standard library would report,
// not any override.
func TestServeMainListenerUsesListenAndServeWhenTheEnvVarIsUnset(t *testing.T) {
	t.Setenv(certEdgeGateTestRemoteAddrEnv, "")

	var gotRemoteAddr string
	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
		w.WriteHeader(http.StatusOK)
		close(done)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // serveMainListener re-listens on the same addr via ListenAndServe.

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		_ = serveMainListener(srv, addr)
	}()
	defer srv.Close()

	// Poll until the listener is actually up (ListenAndServe binds
	// asynchronously relative to this goroutine).
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get("http://" + addr + "/probe")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()
	<-done

	host, _, splitErr := net.SplitHostPort(gotRemoteAddr)
	if splitErr != nil {
		t.Fatalf("SplitHostPort(%q): %v", gotRemoteAddr, splitErr)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("RemoteAddr = %q, want the GENUINE loopback peer (no override applied)", gotRemoteAddr)
	}
}
