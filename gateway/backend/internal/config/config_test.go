// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustLoad calls Load and fails the test on error (the config file is well-formed
// in every test that reaches here).
func mustLoad(t *testing.T) Config {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// writeGatewayConfig writes a JSON config file in a temp dir and points
// OP_AI_GATEWAY_CONFIG at it for the duration of the test.
func writeGatewayConfig(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("OP_AI_GATEWAY_CONFIG", p)
}

func TestConfigFileProvidesValues(t *testing.T) {
	writeGatewayConfig(t, `{"OP_AI_GATEWAY_ADDR":"127.0.0.1:9999","OP_AI_GATEWAY_DB_DRIVER":"postgres","OP_AI_GATEWAY_AUTO_MIGRATE":false,"OP_AI_GATEWAY_CAPTURE_MAX_BYTES":2097152}`)
	cfg := mustLoad(t)
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want the file value", cfg.Addr)
	}
	if cfg.DBDriver != "postgres" {
		t.Errorf("DBDriver = %q, want the file value", cfg.DBDriver)
	}
	if cfg.AutoMigrate {
		t.Errorf("AutoMigrate = true, want false (JSON bool from file)")
	}
	if cfg.CaptureMaxBytes != 2097152 {
		t.Errorf("CaptureMaxBytes = %d, want 2097152 (JSON number from file)", cfg.CaptureMaxBytes)
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	writeGatewayConfig(t, `{"OP_AI_GATEWAY_ADDR":"127.0.0.1:9999"}`)
	t.Setenv("OP_AI_GATEWAY_ADDR", "127.0.0.1:7777")
	if cfg := mustLoad(t); cfg.Addr != "127.0.0.1:7777" {
		t.Errorf("Addr = %q, want the ENV value (env > file)", cfg.Addr)
	}
}

func TestMalformedConfigFileErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(p, []byte(`{ not json `), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("OP_AI_GATEWAY_CONFIG", p)
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for malformed config JSON")
	}
}

func TestExplicitMissingConfigErrors(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an explicitly-requested missing config")
	}
}

func TestTrailingContentConfigErrors(t *testing.T) {
	// Two pasted objects / trailing garbage must be a hard error, not silently
	// dropped after the first object (matches the documented "malformed = error").
	p := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(p, []byte(`{"OP_AI_GATEWAY_ADDR":"127.0.0.1:1"}{"OP_AI_GATEWAY_DB_DRIVER":"postgres"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("OP_AI_GATEWAY_CONFIG", p)
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for trailing content after the JSON object")
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ADDR", "")
	t.Setenv("OP_AI_GATEWAY_PUBLIC_URL", "")
	t.Setenv("OP_AI_GATEWAY_DEFAULT_LANGUAGE", "")

	cfg := mustLoad(t)

	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("Addr = %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if cfg.PublicURL != "http://localhost:8080" {
		t.Fatalf("PublicURL = %q, want http://localhost:8080", cfg.PublicURL)
	}
	if cfg.DefaultLanguage != "de" {
		t.Fatalf("DefaultLanguage = %q, want de", cfg.DefaultLanguage)
	}
	if cfg.DBDriver != "memory" {
		t.Fatalf("DBDriver = %q, want memory", cfg.DBDriver)
	}
	if cfg.SQLitePath != "./data/op-ai-gateway.db" {
		t.Fatalf("SQLitePath = %q, want ./data/op-ai-gateway.db", cfg.SQLitePath)
	}
	if !cfg.AutoMigrate {
		t.Fatalf("AutoMigrate = false, want true")
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ADDR", ":9090")
	t.Setenv("OP_AI_GATEWAY_PUBLIC_URL", "https://gateway.example.test")
	t.Setenv("OP_AI_GATEWAY_DEFAULT_LANGUAGE", "en")
	t.Setenv("OP_AI_GATEWAY_DB_DRIVER", "sqlite")
	t.Setenv("OP_AI_GATEWAY_SQLITE_PATH", "/tmp/op-ai-gateway-test.db")
	t.Setenv("OP_AI_GATEWAY_AUTO_MIGRATE", "false")
	t.Setenv("OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL", "admin@example.test")
	t.Setenv("OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME", "Admin User")
	t.Setenv("OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN", "bootstrap-secret")

	cfg := mustLoad(t)

	if cfg.Addr != ":9090" {
		t.Fatalf("Addr = %q, want :9090", cfg.Addr)
	}
	if cfg.PublicURL != "https://gateway.example.test" {
		t.Fatalf("PublicURL = %q, want https://gateway.example.test", cfg.PublicURL)
	}
	if cfg.DefaultLanguage != "en" {
		t.Fatalf("DefaultLanguage = %q, want en", cfg.DefaultLanguage)
	}
	if cfg.DBDriver != "sqlite" {
		t.Fatalf("DBDriver = %q, want sqlite", cfg.DBDriver)
	}
	if cfg.SQLitePath != "/tmp/op-ai-gateway-test.db" {
		t.Fatalf("SQLitePath = %q, want /tmp/op-ai-gateway-test.db", cfg.SQLitePath)
	}
	if cfg.AutoMigrate {
		t.Fatalf("AutoMigrate = true, want false")
	}
	if cfg.BootstrapAdminEmail != "admin@example.test" {
		t.Fatalf("BootstrapAdminEmail = %q", cfg.BootstrapAdminEmail)
	}
	if cfg.BootstrapAdminName != "Admin User" {
		t.Fatalf("BootstrapAdminName = %q", cfg.BootstrapAdminName)
	}
	if cfg.BootstrapAPIToken != "bootstrap-secret" {
		t.Fatalf("BootstrapAPIToken = %q", cfg.BootstrapAPIToken)
	}
}

func TestLoadSessionDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SESSION_IDLE_TTL", "")
	t.Setenv("OP_AI_GATEWAY_SESSION_MAX_TTL", "")
	t.Setenv("OP_AI_GATEWAY_SESSION_COOKIE_SECURE", "")
	cfg := mustLoad(t)
	if cfg.SessionIdleTTL != 12*time.Hour {
		t.Fatalf("idle ttl = %v, want 12h", cfg.SessionIdleTTL)
	}
	if cfg.SessionMaxTTL != 168*time.Hour {
		t.Fatalf("max ttl = %v, want 168h", cfg.SessionMaxTTL)
	}
	if cfg.SessionCookieSecure != "" {
		t.Fatalf("cookie secure = %q, want empty (auto)", cfg.SessionCookieSecure)
	}
}

func TestLoadSessionOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SESSION_IDLE_TTL", "30m")
	t.Setenv("OP_AI_GATEWAY_SESSION_MAX_TTL", "48h")
	t.Setenv("OP_AI_GATEWAY_SESSION_COOKIE_SECURE", "true")
	cfg := mustLoad(t)
	if cfg.SessionIdleTTL != 30*time.Minute {
		t.Fatalf("idle ttl = %v, want 30m", cfg.SessionIdleTTL)
	}
	if cfg.SessionMaxTTL != 48*time.Hour {
		t.Fatalf("max ttl = %v, want 48h", cfg.SessionMaxTTL)
	}
	if cfg.SessionCookieSecure != "true" {
		t.Fatalf("cookie secure = %q, want true", cfg.SessionCookieSecure)
	}
}

func TestLoadStreamIdleTimeoutDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT", "")
	if cfg := mustLoad(t); cfg.StreamIdleTimeout != 120*time.Second {
		t.Fatalf("stream idle timeout = %v, want 120s", cfg.StreamIdleTimeout)
	}
}

func TestLoadStreamIdleTimeoutOverrides(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"0s", 0},                      // explicitly disabled
		{"-5s", 0},                     // negative -> disabled
		{"garbage", 120 * time.Second}, // invalid -> default
	}
	for _, tc := range cases {
		t.Setenv("OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT", tc.value)
		if got := mustLoad(t).StreamIdleTimeout; got != tc.want {
			t.Fatalf("value %q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestLoadAppHealthProbeTimeoutDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_APP_HEALTH_PROBE_TIMEOUT", "")
	if cfg := mustLoad(t); cfg.AppHealthProbeTimeout != 3*time.Second {
		t.Fatalf("app health probe timeout = %v, want 3s", cfg.AppHealthProbeTimeout)
	}
}

func TestLoadAppHealthProbeTimeoutOverrides(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"5s", 5 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"0s", 3 * time.Second},      // non-positive -> default
		{"garbage", 3 * time.Second}, // invalid -> default
	}
	for _, tc := range cases {
		t.Setenv("OP_AI_GATEWAY_APP_HEALTH_PROBE_TIMEOUT", tc.value)
		if got := mustLoad(t).AppHealthProbeTimeout; got != tc.want {
			t.Fatalf("value %q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestLoadCaptureDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", "")
	t.Setenv("OP_AI_GATEWAY_CAPTURE_MAX_BYTES", "")

	cfg := mustLoad(t)

	if cfg.CaptureEncryptionKey != "" {
		t.Fatalf("CaptureEncryptionKey = %q, want empty", cfg.CaptureEncryptionKey)
	}
	if cfg.CaptureMaxBytes != 1048576 {
		t.Fatalf("CaptureMaxBytes = %d, want 1048576", cfg.CaptureMaxBytes)
	}
}

// TestLoadCertEncryptionKeyIsIndependentOfTheCaptureKey pins that the
// certificate key is its OWN variable: it is read from
// OP_AI_GATEWAY_CERT_ENCRYPTION_KEY only, never derived from (or defaulted to)
// the capture key, and vice versa. Certificate private keys are sealed with the
// certificate key alone, so a leak between these two fields here would silently
// re-introduce exactly the coupling this variable exists to remove.
func TestLoadCertEncryptionKeyIsIndependentOfTheCaptureKey(t *testing.T) {
	const captureKey = "aa11"
	const certKey = "bb22"

	// Only the capture key set: the certificate key must stay EMPTY (no fallback).
	t.Setenv("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", captureKey)
	t.Setenv("OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", "")
	cfg := mustLoad(t)
	if cfg.CertEncryptionKey != "" {
		t.Fatalf("CertEncryptionKey = %q, want empty (it must NOT fall back to the capture key)", cfg.CertEncryptionKey)
	}
	if cfg.CaptureEncryptionKey != captureKey {
		t.Fatalf("CaptureEncryptionKey = %q, want %q", cfg.CaptureEncryptionKey, captureKey)
	}

	// Only the certificate key set: the capture key must stay EMPTY.
	t.Setenv("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", "")
	t.Setenv("OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", certKey)
	cfg = mustLoad(t)
	if cfg.CaptureEncryptionKey != "" {
		t.Fatalf("CaptureEncryptionKey = %q, want empty", cfg.CaptureEncryptionKey)
	}
	if cfg.CertEncryptionKey != certKey {
		t.Fatalf("CertEncryptionKey = %q, want %q", cfg.CertEncryptionKey, certKey)
	}

	// Both set to DIFFERENT values: each field carries its own variable verbatim.
	t.Setenv("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", captureKey)
	t.Setenv("OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", certKey)
	cfg = mustLoad(t)
	if cfg.CaptureEncryptionKey != captureKey || cfg.CertEncryptionKey != certKey {
		t.Fatalf("capture=%q cert=%q, want capture=%q cert=%q (fields crossed?)",
			cfg.CaptureEncryptionKey, cfg.CertEncryptionKey, captureKey, certKey)
	}
}

func TestLoadCaptureOverridesWithoutValidation(t *testing.T) {
	// Load reads the key as a raw string and never validates it (parse+validate
	// happens fail-fast in the server-build path, not here). A malformed key must
	// pass through unchanged and Load must not error (it returns Config only).
	t.Setenv("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", "not-hex-and-too-short")
	t.Setenv("OP_AI_GATEWAY_CAPTURE_MAX_BYTES", "2048")

	cfg := mustLoad(t)

	if cfg.CaptureEncryptionKey != "not-hex-and-too-short" {
		t.Fatalf("CaptureEncryptionKey = %q, want raw passthrough", cfg.CaptureEncryptionKey)
	}
	if cfg.CaptureMaxBytes != 2048 {
		t.Fatalf("CaptureMaxBytes = %d, want 2048", cfg.CaptureMaxBytes)
	}
}

func TestLoadCaptureMaxBytesFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_CAPTURE_MAX_BYTES", v)
		if got := mustLoad(t).CaptureMaxBytes; got != 1048576 {
			t.Fatalf("value %q: CaptureMaxBytes = %d, want 1048576", v, got)
		}
	}
}

func TestLoadCaptureMemoryMaxBytesDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES", "")

	cfg := mustLoad(t)

	if cfg.CaptureMemoryMaxBytes != 67108864 {
		t.Fatalf("CaptureMemoryMaxBytes = %d, want 67108864", cfg.CaptureMemoryMaxBytes)
	}
}

func TestLoadCaptureMemoryMaxBytesOverride(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES", "1048576")

	cfg := mustLoad(t)

	if cfg.CaptureMemoryMaxBytes != 1048576 {
		t.Fatalf("CaptureMemoryMaxBytes = %d, want 1048576", cfg.CaptureMemoryMaxBytes)
	}
}

func TestLoadCaptureMemoryMaxBytesFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES", v)
		if got := mustLoad(t).CaptureMemoryMaxBytes; got != 67108864 {
			t.Fatalf("value %q: CaptureMemoryMaxBytes = %d, want 67108864", v, got)
		}
	}
}

func TestLoadLogLevelDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_LOG_LEVEL", "")
	if got := mustLoad(t).LogLevel; got != "info" {
		t.Fatalf("LogLevel = %q, want info (default)", got)
	}
}

func TestLoadLogLevelOverride(t *testing.T) {
	for _, v := range []string{"debug", "info", "warn", "error"} {
		t.Setenv("OP_AI_GATEWAY_LOG_LEVEL", v)
		if got := mustLoad(t).LogLevel; got != v {
			t.Fatalf("value %q: LogLevel = %q, want %q", v, got, v)
		}
	}
}

func TestLoadNetbirdKeyFileDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_NETBIRD_KEY_FILE", "")
	if got := mustLoad(t).NetbirdKeyFile; got != "" {
		t.Fatalf("NetbirdKeyFile = %q, want \"\" (default off)", got)
	}
}

func TestLoadNetbirdKeyFileOverride(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_NETBIRD_KEY_FILE", "/shared/netbird-setup-key")
	if got := mustLoad(t).NetbirdKeyFile; got != "/shared/netbird-setup-key" {
		t.Fatalf("NetbirdKeyFile = %q, want the override value", got)
	}
}

func TestLoadMockDelayDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_MOCK_DELAY_MS", "")
	if got := mustLoad(t).MockDelay; got != 0 {
		t.Fatalf("MockDelay = %v, want 0 (default off)", got)
	}
}

func TestLoadMockDelayOverride(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_MOCK_DELAY_MS", "250")
	if got := mustLoad(t).MockDelay; got != 250*time.Millisecond {
		t.Fatalf("MockDelay = %v, want 250ms", got)
	}
}

func TestLoadMockDelayFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_MOCK_DELAY_MS", v)
		if got := mustLoad(t).MockDelay; got != 0 {
			t.Fatalf("value %q: MockDelay = %v, want 0", v, got)
		}
	}
}

func TestLoadSwapProtectWindowSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS", "")
	if got := mustLoad(t).SwapProtectWindowSeconds; got != 30 {
		t.Fatalf("SwapProtectWindowSeconds = %d, want 30 (default)", got)
	}
}

func TestLoadSwapProtectWindowSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS", "45")
	if got := mustLoad(t).SwapProtectWindowSeconds; got != 45 {
		t.Fatalf("SwapProtectWindowSeconds = %d, want 45", got)
	}
}

func TestLoadSwapProtectWindowSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS", v)
		if got := mustLoad(t).SwapProtectWindowSeconds; got != 30 {
			t.Fatalf("value %q: SwapProtectWindowSeconds = %d, want 30", v, got)
		}
	}
}

func TestLoadSessionReservationWindowSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS", "")
	if got := mustLoad(t).SessionReservationWindowSeconds; got != 60 {
		t.Fatalf("SessionReservationWindowSeconds = %d, want 60 (default)", got)
	}
}

func TestLoadSessionReservationWindowSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS", "90")
	if got := mustLoad(t).SessionReservationWindowSeconds; got != 90 {
		t.Fatalf("SessionReservationWindowSeconds = %d, want 90", got)
	}
}

func TestLoadSessionReservationWindowSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS", v)
		if got := mustLoad(t).SessionReservationWindowSeconds; got != 60 {
			t.Fatalf("value %q: SessionReservationWindowSeconds = %d, want 60", v, got)
		}
	}
}

func TestLoadAgentPresenceTimeoutSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS", "")
	if got := mustLoad(t).AgentPresenceTimeoutSeconds; got != 15 {
		t.Fatalf("AgentPresenceTimeoutSeconds = %d, want 15 (default)", got)
	}
}

func TestLoadAgentPresenceTimeoutSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS", "20")
	if got := mustLoad(t).AgentPresenceTimeoutSeconds; got != 20 {
		t.Fatalf("AgentPresenceTimeoutSeconds = %d, want 20", got)
	}
}

func TestLoadAgentPresenceTimeoutSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS", v)
		if got := mustLoad(t).AgentPresenceTimeoutSeconds; got != 15 {
			t.Fatalf("value %q: AgentPresenceTimeoutSeconds = %d, want 15", v, got)
		}
	}
}

func TestLoadAdmissionQueueMaxDepthDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH", "")
	if got := mustLoad(t).AdmissionQueueMaxDepth; got != 128 {
		t.Fatalf("AdmissionQueueMaxDepth = %d, want 128 (default)", got)
	}
}

func TestLoadAdmissionQueueMaxDepthOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH", "256")
	if got := mustLoad(t).AdmissionQueueMaxDepth; got != 256 {
		t.Fatalf("AdmissionQueueMaxDepth = %d, want 256", got)
	}
}

func TestLoadAdmissionQueueMaxDepthFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH", v)
		if got := mustLoad(t).AdmissionQueueMaxDepth; got != 128 {
			t.Fatalf("value %q: AdmissionQueueMaxDepth = %d, want 128", v, got)
		}
	}
}

func TestLoadCapacityConfigDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT", "")
	t.Setenv("OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY", "")
	t.Setenv("OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS", "")
	cfg := mustLoad(t)
	if cfg.CapacityVRAMSafetyMarginPercent != 10 {
		t.Fatalf("CapacityVRAMSafetyMarginPercent = %d, want 10 (default)", cfg.CapacityVRAMSafetyMarginPercent)
	}
	if cfg.CapacityMaxConcurrency != 64 {
		t.Fatalf("CapacityMaxConcurrency = %d, want 64 (default)", cfg.CapacityMaxConcurrency)
	}
	if cfg.CapacitySettleSeconds != 5 {
		t.Fatalf("CapacitySettleSeconds = %d, want 5 (default)", cfg.CapacitySettleSeconds)
	}
}

func TestLoadCapacityConfigOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT", "15")
	t.Setenv("OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY", "128")
	t.Setenv("OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS", "8")
	cfg := mustLoad(t)
	if cfg.CapacityVRAMSafetyMarginPercent != 15 {
		t.Fatalf("CapacityVRAMSafetyMarginPercent = %d, want 15", cfg.CapacityVRAMSafetyMarginPercent)
	}
	if cfg.CapacityMaxConcurrency != 128 {
		t.Fatalf("CapacityMaxConcurrency = %d, want 128", cfg.CapacityMaxConcurrency)
	}
	if cfg.CapacitySettleSeconds != 8 {
		t.Fatalf("CapacitySettleSeconds = %d, want 8", cfg.CapacitySettleSeconds)
	}
}

func TestLoadCapacityConfigFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT", v)
		t.Setenv("OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY", v)
		t.Setenv("OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS", v)
		cfg := mustLoad(t)
		if cfg.CapacityVRAMSafetyMarginPercent != 10 || cfg.CapacityMaxConcurrency != 64 || cfg.CapacitySettleSeconds != 5 {
			t.Fatalf("value %q: capacity config = %d/%d/%d, want 10/64/5", v,
				cfg.CapacityVRAMSafetyMarginPercent, cfg.CapacityMaxConcurrency, cfg.CapacitySettleSeconds)
		}
	}
}

func TestLoadBenchmarkScheduleDefaultSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS", "")
	if got := mustLoad(t).BenchmarkScheduleDefaultSeconds; got != 86400 {
		t.Fatalf("BenchmarkScheduleDefaultSeconds = %d, want 86400 (default)", got)
	}
}

func TestLoadBenchmarkScheduleDefaultSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS", "3600")
	if got := mustLoad(t).BenchmarkScheduleDefaultSeconds; got != 3600 {
		t.Fatalf("BenchmarkScheduleDefaultSeconds = %d, want 3600", got)
	}
}

func TestLoadBenchmarkScheduleDefaultSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS", v)
		if got := mustLoad(t).BenchmarkScheduleDefaultSeconds; got != 86400 {
			t.Fatalf("value %q: BenchmarkScheduleDefaultSeconds = %d, want 86400", v, got)
		}
	}
}

func TestLoadMockUnreachableDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_MOCK_UNREACHABLE", "")
	if mustLoad(t).MockUnreachable {
		t.Fatalf("MockUnreachable = true, want false (default off)")
	}
}

func TestLoadMockUnreachableOverride(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Setenv("OP_AI_GATEWAY_MOCK_UNREACHABLE", v)
		if !mustLoad(t).MockUnreachable {
			t.Fatalf("value %q: MockUnreachable = false, want true", v)
		}
	}
}

// TestLoadCertEdgeRequireHTTPSDisableDefault pins the fail-safe default: an
// unset kill switch means the switch is NOT engaged, so a normal deployment
// that never sets this variable is unaffected by it.
func TestLoadCertEdgeRequireHTTPSDisableDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE", "")
	if mustLoad(t).CertEdgeRequireHTTPSDisable {
		t.Fatalf("CertEdgeRequireHTTPSDisable = true, want false (default off)")
	}
}

func TestLoadCertEdgeRequireHTTPSDisableOverride(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Setenv("OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE", v)
		if !mustLoad(t).CertEdgeRequireHTTPSDisable {
			t.Fatalf("value %q: CertEdgeRequireHTTPSDisable = false, want true", v)
		}
	}
}

// TestLoadCertMeshRequireTLSDisableDefault pins the fail-safe default of the
// mesh-listener kill switch: unset means NOT engaged.
func TestLoadCertMeshRequireTLSDisableDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE", "")
	if mustLoad(t).CertMeshRequireTLSDisable {
		t.Fatalf("CertMeshRequireTLSDisable = true, want false (default off)")
	}
}

func TestLoadCertMeshRequireTLSDisableOverride(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Setenv("OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE", v)
		if !mustLoad(t).CertMeshRequireTLSDisable {
			t.Fatalf("value %q: CertMeshRequireTLSDisable = false, want true", v)
		}
	}
}

func TestLoadAgentAddrDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_AGENT_ADDR", "")
	t.Setenv("OP_AI_GATEWAY_AGENT_PORT", "")
	cfg := mustLoad(t)
	if cfg.AgentAddr != "" {
		t.Fatalf("AgentAddr = %q, want empty (default)", cfg.AgentAddr)
	}
	if cfg.AgentPort != "8081" {
		t.Fatalf("AgentPort = %q, want 8081 (default)", cfg.AgentPort)
	}
}

func TestLoadAgentAddrOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_AGENT_ADDR", "100.90.1.2:8081")
	t.Setenv("OP_AI_GATEWAY_AGENT_PORT", "9091")
	cfg := mustLoad(t)
	if cfg.AgentAddr != "100.90.1.2:8081" {
		t.Fatalf("AgentAddr = %q, want the env value", cfg.AgentAddr)
	}
	if cfg.AgentPort != "9091" {
		t.Fatalf("AgentPort = %q, want the env value", cfg.AgentPort)
	}
}

func TestLoadNetbirdSyncInterval(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 60},     // default
		{"0", 60},    // junk / <= 0 -> default
		{"-5", 60},   // negative -> default
		{"nope", 60}, // unparseable -> default
		{"10", 30},   // below floor -> floored to 30
		{"30", 30},   // at floor
		{"120", 120}, // above floor -> passthrough
	}
	for _, tc := range cases {
		t.Setenv("OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS", tc.in)
		if got := mustLoad(t).NetbirdSyncIntervalSeconds; got != tc.want {
			t.Fatalf("NetbirdSyncIntervalSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLoadNetbirdTokenRotateBeforeDays(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 14},     // unset -> default
		{"nope", 14}, // unparseable -> default
		{"-5", 14},   // negative -> default (explicit "off" is a setting, not the env)
		{"0", 0},     // explicit 0 -> honored (auto-rotation off)
		{"7", 7},     // positive -> passthrough
		{"30", 30},
	}
	for _, tc := range cases {
		t.Setenv("OP_AI_GATEWAY_NETBIRD_TOKEN_ROTATE_BEFORE_DAYS", tc.in)
		if got := mustLoad(t).NetbirdTokenRotateBeforeDays; got != tc.want {
			t.Fatalf("NetbirdTokenRotateBeforeDays(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLoadTracingDefaults(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CONFIG", "") // ensure no next-to-binary file interferes
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TracingEnabled {
		t.Errorf("TracingEnabled default = true, want false")
	}
	if cfg.TracingSampleRatio != 1.0 {
		t.Errorf("TracingSampleRatio default = %v, want 1.0", cfg.TracingSampleRatio)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint default = %q, want empty", cfg.OTLPEndpoint)
	}
	if cfg.LogBufferSize != 5000 {
		t.Errorf("LogBufferSize default = %d, want 5000", cfg.LogBufferSize)
	}
}

func TestLoadTracingOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_TRACING_ENABLED", "true")
	t.Setenv("OP_AI_GATEWAY_TRACING_SAMPLE_RATIO", "0.25")
	t.Setenv("OP_AI_GATEWAY_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OP_AI_GATEWAY_LOG_BUFFER_SIZE", "20000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TracingEnabled || cfg.TracingSampleRatio != 0.25 || cfg.OTLPEndpoint != "http://collector:4318" || cfg.LogBufferSize != 20000 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadTracingSampleRatioClamped(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_TRACING_SAMPLE_RATIO", "5") // > 1 clamps to 1
	cfg, _ := Load()
	if cfg.TracingSampleRatio != 1.0 {
		t.Errorf("ratio 5 clamped = %v, want 1.0", cfg.TracingSampleRatio)
	}
	t.Setenv("OP_AI_GATEWAY_TRACING_SAMPLE_RATIO", "-1") // < 0 clamps to 0
	cfg, _ = Load()
	if cfg.TracingSampleRatio != 0 {
		t.Errorf("ratio -1 clamped = %v, want 0", cfg.TracingSampleRatio)
	}
}

func TestLoadEnergyReconcileIntervalSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_RECONCILE_INTERVAL_SECONDS", "")
	if got := mustLoad(t).EnergyReconcileIntervalSeconds; got != 15 {
		t.Fatalf("EnergyReconcileIntervalSeconds = %d, want 15 (default)", got)
	}
}

func TestLoadEnergyReconcileIntervalSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_RECONCILE_INTERVAL_SECONDS", "30")
	if got := mustLoad(t).EnergyReconcileIntervalSeconds; got != 30 {
		t.Fatalf("EnergyReconcileIntervalSeconds = %d, want 30", got)
	}
}

func TestLoadEnergyReconcileIntervalSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_ENERGY_RECONCILE_INTERVAL_SECONDS", v)
		if got := mustLoad(t).EnergyReconcileIntervalSeconds; got != 15 {
			t.Fatalf("value %q: EnergyReconcileIntervalSeconds = %d, want 15", v, got)
		}
	}
}

func TestLoadEnergySettleSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_SETTLE_SECONDS", "")
	if got := mustLoad(t).EnergySettleSeconds; got != 10 {
		t.Fatalf("EnergySettleSeconds = %d, want 10 (default)", got)
	}
}

func TestLoadEnergySettleSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_SETTLE_SECONDS", "20")
	if got := mustLoad(t).EnergySettleSeconds; got != 20 {
		t.Fatalf("EnergySettleSeconds = %d, want 20", got)
	}
}

func TestLoadEnergySettleSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_ENERGY_SETTLE_SECONDS", v)
		if got := mustLoad(t).EnergySettleSeconds; got != 10 {
			t.Fatalf("value %q: EnergySettleSeconds = %d, want 10", v, got)
		}
	}
}

func TestLoadEnergyIdleWindowSecondsDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_IDLE_WINDOW_SECONDS", "")
	if got := mustLoad(t).EnergyIdleWindowSeconds; got != 3600 {
		t.Fatalf("EnergyIdleWindowSeconds = %d, want 3600 (default)", got)
	}
}

func TestLoadEnergyIdleWindowSecondsOverrides(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_ENERGY_IDLE_WINDOW_SECONDS", "7200")
	if got := mustLoad(t).EnergyIdleWindowSeconds; got != 7200 {
		t.Fatalf("EnergyIdleWindowSeconds = %d, want 7200", got)
	}
}

func TestLoadEnergyIdleWindowSecondsFallsBackOnJunk(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-5"} {
		t.Setenv("OP_AI_GATEWAY_ENERGY_IDLE_WINDOW_SECONDS", v)
		if got := mustLoad(t).EnergyIdleWindowSeconds; got != 3600 {
			t.Fatalf("value %q: EnergyIdleWindowSeconds = %d, want 3600", v, got)
		}
	}
}

func TestLoadAgentBinaryDirDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_AGENT_BINARY_DIR", "")
	cfg := mustLoad(t)
	if cfg.AgentBinaryDir != "/agents" {
		t.Fatalf("default AgentBinaryDir = %q, want /agents", cfg.AgentBinaryDir)
	}
	t.Setenv("OP_AI_GATEWAY_AGENT_BINARY_DIR", "/custom")
	cfg = mustLoad(t)
	if cfg.AgentBinaryDir != "/custom" {
		t.Fatalf("AgentBinaryDir = %q, want /custom", cfg.AgentBinaryDir)
	}
}

func TestLoadThemesDirDefault(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_THEMES_DIR", "")
	cfg := mustLoad(t)
	if cfg.ThemesDir != "/themes" {
		t.Fatalf("default ThemesDir = %q, want /themes", cfg.ThemesDir)
	}
}

func TestLoadThemesDirOverride(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_THEMES_DIR", "/x/themes")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ThemesDir != "/x/themes" {
		t.Fatalf("got %q", cfg.ThemesDir)
	}
}

func TestCertReconcileIntervalDefaultAndFloor(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS", "")
	if got := mustLoad(t).CertReconcileIntervalSeconds; got != 900 {
		t.Fatalf("default = %d, want 900", got)
	}
	t.Setenv("OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS", "5")
	if got := mustLoad(t).CertReconcileIntervalSeconds; got != 60 {
		t.Fatalf("floored = %d, want 60", got)
	}
}

// TestLoadReadsCertEdgeOutputDir pins that
// OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR is read into
// Config.CertEdgeOutputDir, trimmed of surrounding whitespace.
func TestLoadReadsCertEdgeOutputDir(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR", " /var/lib/op-certs ")
	cfg := mustLoad(t)
	if cfg.CertEdgeOutputDir != "/var/lib/op-certs" {
		t.Fatalf("CertEdgeOutputDir = %q, want the trimmed path", cfg.CertEdgeOutputDir)
	}
}

// TestCertEdgeOutputDirDefaultsToEmpty pins that leaving the variable unset
// means the gateway cannot deliver the edge certificate locally -- it must
// NEVER default to a path (empty is a first-class value, not a misconfig).
func TestCertEdgeOutputDirDefaultsToEmpty(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR", "")
	cfg := mustLoad(t)
	if cfg.CertEdgeOutputDir != "" {
		t.Fatalf("CertEdgeOutputDir = %q, want empty (= no local delivery)", cfg.CertEdgeOutputDir)
	}
}

// TestLoadReadsCertEdgeProbeTarget pins that
// OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET is read into
// Config.CertEdgeProbeTarget, trimmed of surrounding whitespace.
func TestLoadReadsCertEdgeProbeTarget(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET", " web:443 ")
	cfg := mustLoad(t)
	if cfg.CertEdgeProbeTarget != "web:443" {
		t.Fatalf("CertEdgeProbeTarget = %q, want the trimmed address", cfg.CertEdgeProbeTarget)
	}
}

// TestCertEdgeProbeTargetDefaultsToEmpty pins that leaving the variable unset
// disables the probe endpoint (409) -- it must NEVER default to a guessed
// address, since nginx is always a different container/pod than the backend.
func TestCertEdgeProbeTargetDefaultsToEmpty(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET", "")
	cfg := mustLoad(t)
	if cfg.CertEdgeProbeTarget != "" {
		t.Fatalf("CertEdgeProbeTarget = %q, want empty (= probe endpoint disabled)", cfg.CertEdgeProbeTarget)
	}
}
