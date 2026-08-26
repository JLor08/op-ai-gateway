// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package config

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// captureWarn redirects the default slog logger to a buffer at Warn level for the
// duration of the test (mirrors internal/collector/power_logging_test.go's
// captureDebug), restoring the previous logger afterward.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// warnLineCount counts non-empty output lines in buf — each slog.TextHandler
// record is exactly one line, so this is an exact call count.
func warnLineCount(buf *bytes.Buffer) int {
	n := 0
	for _, ln := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// writeConfig writes a JSON config file in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "server-agent.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadFromConfigFile(t *testing.T) {
	getenv := func(string) string { return "" }
	path := writeConfig(t, `{"gateway_url":"https://file.example","token":"file-token","interval":"7s","tls_insecure":true}`)

	cfg, err := Load([]string{"-config", path}, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://file.example" {
		t.Errorf("GatewayURL = %q, want the file value", cfg.GatewayURL)
	}
	if cfg.Token != "file-token" {
		t.Errorf("Token = %q, want the file value", cfg.Token)
	}
	if cfg.Interval != 7*time.Second {
		t.Errorf("Interval = %v, want 7s (from file)", cfg.Interval)
	}
	if !cfg.TLSInsecure {
		t.Errorf("TLSInsecure = false, want true (from file)")
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://file.example","token":"file-token","interval":"7s"}`)
	getenv := func(k string) string {
		if k == "OP_AGENT_GATEWAY_URL" {
			return "https://env.example"
		}
		return ""
	}
	cfg, err := Load([]string{"-config", path}, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://env.example" {
		t.Errorf("GatewayURL = %q, want the ENV value (env > file)", cfg.GatewayURL)
	}
	if cfg.Token != "file-token" {
		t.Errorf("Token = %q, want the file value (env did not set it)", cfg.Token)
	}
}

func TestFlagOverridesEnvAndConfigFile(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://file.example","token":"file-token"}`)
	getenv := func(k string) string {
		if k == "OP_AGENT_GATEWAY_URL" {
			return "https://env.example"
		}
		return ""
	}
	cfg, err := Load([]string{"-config", path, "-gateway-url=https://flag.example"}, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://flag.example" {
		t.Errorf("GatewayURL = %q, want the FLAG value (flag > env > file)", cfg.GatewayURL)
	}
}

func TestConfigFileViaEnvPath(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://file.example","token":"file-token"}`)
	getenv := func(k string) string {
		if k == "OP_AGENT_CONFIG" {
			return path
		}
		return ""
	}
	cfg, err := Load(nil, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://file.example" || cfg.Token != "file-token" {
		t.Errorf("config via OP_AGENT_CONFIG not loaded: %+v", cfg)
	}
}

func TestVerboseResolution(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// default: off
	if cfg, err := Load(base, noenv); err != nil || cfg.Verbose {
		t.Fatalf("default Verbose = %v (err %v), want false", cfg.Verbose, err)
	}
	// -v flag
	if cfg, err := Load(append([]string{"-v"}, base...), noenv); err != nil || !cfg.Verbose {
		t.Fatalf("-v Verbose = %v (err %v), want true", cfg.Verbose, err)
	}
	// --verbose alias
	if cfg, err := Load(append([]string{"-verbose"}, base...), noenv); err != nil || !cfg.Verbose {
		t.Fatalf("-verbose Verbose = %v (err %v), want true", cfg.Verbose, err)
	}
	// env
	envOn := func(k string) string {
		if k == "OP_AGENT_VERBOSE" {
			return "true"
		}
		return ""
	}
	if cfg, err := Load(base, envOn); err != nil || !cfg.Verbose {
		t.Fatalf("env OP_AGENT_VERBOSE Verbose = %v (err %v), want true", cfg.Verbose, err)
	}
	// config file
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","verbose":true}`)
	if cfg, err := Load([]string{"-config", path}, noenv); err != nil || !cfg.Verbose {
		t.Fatalf("config verbose Verbose = %v (err %v), want true", cfg.Verbose, err)
	}
}

func TestMalformedConfigFileErrors(t *testing.T) {
	path := writeConfig(t, `{ this is not json `)
	if _, err := Load([]string{"-config", path}, func(string) string { return "" }); err == nil {
		t.Fatal("expected an error for malformed config JSON")
	}
}

func TestExplicitMissingConfigErrors(t *testing.T) {
	getenv := func(string) string { return "" }
	// An explicitly-requested config that does not exist is an error...
	if _, err := Load([]string{"-config", filepath.Join(t.TempDir(), "nope.json")}, getenv); err == nil {
		t.Fatal("expected an error for an explicitly-requested missing config")
	}
	// ...but a missing DEFAULT (next-to-binary) file is fine: point the default at
	// a temp dir with no config and confirm Load falls through to env.
	orig := executable
	t.Cleanup(func() { executable = orig })
	executable = func() (string, error) { return filepath.Join(t.TempDir(), "server-agent"), nil }
	cfg, err := Load(nil, func(k string) string {
		switch k {
		case "OP_AGENT_GATEWAY_URL":
			return "https://env.example"
		case "OP_AGENT_TOKEN":
			return "env-token"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load with a missing default config should succeed via env: %v", err)
	}
	if cfg.GatewayURL != "https://env.example" {
		t.Errorf("GatewayURL = %q, want env value", cfg.GatewayURL)
	}
}

// Regression guard: the flag package prints non-empty string-flag DEFAULTS in
// its usage text (on -h or a bad flag). The bearer token must therefore NEVER be
// the -token flag's default, or it leaks to stderr (journald/docker logs/CI).
func TestLoadDoesNotLeakTokenInUsage(t *testing.T) {
	const secret = "SECRET-TOKEN-abc123"
	getenv := func(k string) string {
		switch k {
		case "OP_AGENT_TOKEN":
			return secret
		case "OP_AGENT_GATEWAY_URL":
			return "http://gw.example.test"
		}
		return ""
	}

	// The FlagSet's default Output() is os.Stderr, read at print time — redirect
	// it so we can inspect the usage dump.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	_, err := Load([]string{"-h"}, getenv) // -h triggers the usage print
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)

	if err == nil {
		t.Fatalf("Load(-h) should return a (help) error")
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("bearer token leaked into usage/stderr output:\n%s", out)
	}
	// The env-fallback must still work when the flag is absent.
	cfg, err := Load(nil, getenv)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if cfg.Token != secret {
		t.Fatalf("Token = %q, want the env value (env fallback broken)", cfg.Token)
	}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	env := map[string]string{
		"OP_AGENT_GATEWAY_URL": "https://gw.example.test",
		"OP_AGENT_TOKEN":       "secret-token",
	}
	getenv := func(k string) string { return env[k] }

	cfg, err := Load(nil, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://gw.example.test" {
		t.Errorf("GatewayURL = %q, want https://gw.example.test", cfg.GatewayURL)
	}
	if cfg.Token != "secret-token" {
		t.Errorf("Token = %q, want secret-token", cfg.Token)
	}
	if cfg.Interval != 1*time.Second {
		t.Errorf("Interval = %v, want 1s (default)", cfg.Interval)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"OP_AGENT_GATEWAY_URL": "https://gw.example.test",
		"OP_AGENT_TOKEN":       "secret-token",
		"OP_AGENT_INTERVAL":    "3s",
	}
	getenv := func(k string) string { return env[k] }

	cfg, err := Load([]string{"-interval=10s"}, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s (flag overrides env)", cfg.Interval)
	}
}

func TestValidateRejectsMissing(t *testing.T) {
	getenv := func(string) string { return "" }

	// missing token
	if _, err := Load([]string{"-gateway-url=https://gw.example.test"}, getenv); err == nil {
		t.Error("expected error for missing token")
	}
	// bad (non-absolute) URL
	if _, err := Load([]string{"-gateway-url=notaurl", "-token=x"}, getenv); err == nil {
		t.Error("expected error for bad gateway URL")
	}
	// interval 0: no longer an error; falls back to the 1s default.
	if cfg, err := Load([]string{"-gateway-url=https://gw.example.test", "-token=x", "-interval=0s"}, getenv); err != nil || cfg.Interval != 1*time.Second {
		t.Errorf("interval 0 -> Interval=%v err=%v, want 1s default", cfg.Interval, err)
	}
}

func TestTransportResolution(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// default: websocket
	if cfg, err := Load(base, noenv); err != nil || cfg.Transport != "websocket" {
		t.Fatalf("default Transport = %q (err %v), want websocket", cfg.Transport, err)
	}
	// flag can still select post
	if cfg, err := Load(append([]string{"-transport=post"}, base...), noenv); err != nil || cfg.Transport != "post" {
		t.Fatalf("-transport=post -> %q (err %v)", cfg.Transport, err)
	}
	// flag
	if cfg, err := Load(append([]string{"-transport=websocket"}, base...), noenv); err != nil || cfg.Transport != "websocket" {
		t.Fatalf("-transport=websocket -> %q (err %v)", cfg.Transport, err)
	}
	// env
	env := func(k string) string {
		if k == "OP_AGENT_TRANSPORT" {
			return "websocket"
		}
		return ""
	}
	if cfg, err := Load(base, env); err != nil || cfg.Transport != "websocket" {
		t.Fatalf("env OP_AGENT_TRANSPORT -> %q (err %v)", cfg.Transport, err)
	}
	// invalid rejected
	if _, err := Load(append([]string{"-transport=grpc"}, base...), noenv); err == nil {
		t.Fatal("expected error for invalid transport")
	}
}

func TestConfigFileWithLineComments(t *testing.T) {
	// Whole-line // comments must be tolerated (the portal-generated config is
	// annotated), while a // inside a value (https:// URL) must be preserved.
	path := writeConfig(t, `{
  // the gateway base URL
  "gateway_url": "https://file.example",
  // the per-server agent token
  "token": "file-token",
  // telemetry transport
  "transport": "post"
}`)
	cfg, err := Load([]string{"-config=" + path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load with comments: %v", err)
	}
	if cfg.GatewayURL != "https://file.example" {
		t.Fatalf("GatewayURL = %q, want https://file.example (the // in https:// must survive)", cfg.GatewayURL)
	}
	if cfg.Token != "file-token" || cfg.Transport != "post" {
		t.Fatalf("Token/Transport = %q/%q, want file-token/post", cfg.Token, cfg.Transport)
	}
}

func TestTransportFromConfigFile(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://file.example","token":"file-token","transport":"websocket"}`)
	cfg, err := Load([]string{"-config=" + path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport != "websocket" {
		t.Fatalf("Transport = %q, want websocket (from file)", cfg.Transport)
	}
}

func TestIntervalFloorClamp(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// below the floor -> clamped to 250ms
	if cfg, err := Load(append([]string{"-interval=50ms"}, base...), noenv); err != nil || cfg.Interval != 250*time.Millisecond {
		t.Fatalf("-interval=50ms -> %v (err %v), want 250ms", cfg.Interval, err)
	}
	// exactly at the floor -> unchanged
	if cfg, err := Load(append([]string{"-interval=250ms"}, base...), noenv); err != nil || cfg.Interval != 250*time.Millisecond {
		t.Fatalf("-interval=250ms -> %v (err %v)", cfg.Interval, err)
	}
	// above the floor -> unchanged
	if cfg, err := Load(append([]string{"-interval=1s"}, base...), noenv); err != nil || cfg.Interval != time.Second {
		t.Fatalf("-interval=1s -> %v (err %v)", cfg.Interval, err)
	}
}

func TestLHMURLResolution(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// default: empty (disabled)
	if cfg, err := Load(base, noenv); err != nil || cfg.LHMURL != "" {
		t.Fatalf("default LHMURL = %q (err %v), want \"\"", cfg.LHMURL, err)
	}
	// flag
	if cfg, err := Load(append([]string{"-lhm-url=http://flag:8085/data.json"}, base...), noenv); err != nil || cfg.LHMURL != "http://flag:8085/data.json" {
		t.Fatalf("-lhm-url LHMURL = %q (err %v)", cfg.LHMURL, err)
	}
	// env
	envURL := func(k string) string {
		if k == "OP_AGENT_LHM_URL" {
			return "http://env:8085/data.json"
		}
		return ""
	}
	if cfg, err := Load(base, envURL); err != nil || cfg.LHMURL != "http://env:8085/data.json" {
		t.Fatalf("env OP_AGENT_LHM_URL LHMURL = %q (err %v)", cfg.LHMURL, err)
	}
	// config file
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","lhm_url":"http://file:8085/data.json"}`)
	if cfg, err := Load([]string{"-config", path}, noenv); err != nil || cfg.LHMURL != "http://file:8085/data.json" {
		t.Fatalf("config lhm_url LHMURL = %q (err %v)", cfg.LHMURL, err)
	}
}

func TestSystemReportIntervalDefault(t *testing.T) {
	cfg, err := Load([]string{"-gateway-url", "http://gw", "-token", "t"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SystemReportInterval != 30*time.Minute {
		t.Fatalf("SystemReportInterval = %v, want 30m", cfg.SystemReportInterval)
	}
}

func TestSystemReportIntervalEnvAndFloor(t *testing.T) {
	env := map[string]string{
		"OP_AGENT_GATEWAY_URL":            "http://gw",
		"OP_AGENT_TOKEN":                  "t",
		"OP_AGENT_SYSTEM_REPORT_INTERVAL": "5s", // below the 1m floor -> clamped up
	}
	cfg, err := Load(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SystemReportInterval != time.Minute {
		t.Fatalf("SystemReportInterval = %v, want 1m (floor)", cfg.SystemReportInterval)
	}
}

// --- Certificate installation config (Phase 2 distribution, Task 4) ---

func TestCertModeDefaultsToOff(t *testing.T) {
	cfg, err := Load([]string{"-gateway-url=https://gw.example", "-token=x"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CertMode != CertModeOff {
		t.Fatalf("CertMode = %q, want %q (default)", cfg.CertMode, CertModeOff)
	}
	if cfg.CertDir != "" || cfg.CertReloadCommand != "" {
		t.Fatalf("CertDir/CertReloadCommand = %q/%q, want empty defaults", cfg.CertDir, cfg.CertReloadCommand)
	}
	if cfg.CertPollInterval != 0 {
		t.Fatalf("CertPollInterval = %v, want 0 (automatic) by default", cfg.CertPollInterval)
	}
}

func TestCertModePrecedence(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","cert_mode":"files","cert_dir":"/from-file"}`)
	// file only
	if cfg, err := Load([]string{"-config", path}, func(string) string { return "" }); err != nil || cfg.CertMode != "files" || cfg.CertDir != "/from-file" {
		t.Fatalf("file cert_mode/cert_dir = %q/%q (err %v), want files//from-file", cfg.CertMode, cfg.CertDir, err)
	}
	// env overrides file
	envOnly := func(k string) string {
		switch k {
		case "OP_AGENT_CERT_MODE":
			return "off"
		case "OP_AGENT_CERT_DIR":
			return "/from-env"
		}
		return ""
	}
	if cfg, err := Load([]string{"-config", path}, envOnly); err != nil || cfg.CertMode != "off" || cfg.CertDir != "/from-env" {
		t.Fatalf("env cert_mode/cert_dir = %q/%q (err %v), want off//from-env (env > file)", cfg.CertMode, cfg.CertDir, err)
	}
	// flag overrides env and file
	if cfg, err := Load([]string{"-config", path, "-cert-mode=proxy", "-cert-dir=/from-flag"}, envOnly); err != nil || cfg.CertMode != "proxy" || cfg.CertDir != "/from-flag" {
		t.Fatalf("flag cert_mode/cert_dir = %q/%q (err %v), want proxy//from-flag (flag > env > file)", cfg.CertMode, cfg.CertDir, err)
	}
}

func TestCertReloadCommandPrecedence(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x", "-cert-mode=files", "-cert-dir=/certs"}
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","cert_reload_command":"systemctl reload nginx"}`)

	// file
	if cfg, err := Load(append([]string{"-config", path}, base...), func(string) string { return "" }); err != nil || cfg.CertReloadCommand != "systemctl reload nginx" {
		t.Fatalf("file CertReloadCommand = %q (err %v), want the file value", cfg.CertReloadCommand, err)
	}
	// env overrides file
	env := func(k string) string {
		if k == "OP_AGENT_CERT_RELOAD_COMMAND" {
			return "reload-from-env.sh"
		}
		return ""
	}
	if cfg, err := Load(append([]string{"-config", path}, base...), env); err != nil || cfg.CertReloadCommand != "reload-from-env.sh" {
		t.Fatalf("env CertReloadCommand = %q (err %v), want reload-from-env.sh (env > file)", cfg.CertReloadCommand, err)
	}
	// flag overrides env and file
	args := append(append([]string{"-config", path}, base...), "-cert-reload-command=reload-from-flag.sh")
	if cfg, err := Load(args, env); err != nil || cfg.CertReloadCommand != "reload-from-flag.sh" {
		t.Fatalf("flag CertReloadCommand = %q (err %v), want reload-from-flag.sh (flag > env > file)", cfg.CertReloadCommand, err)
	}
}

func TestCertPollIntervalPrecedence(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","cert_poll_interval":"2h"}`)

	if cfg, err := Load(append([]string{"-config", path}, base...), func(string) string { return "" }); err != nil || cfg.CertPollInterval != 2*time.Hour {
		t.Fatalf("file CertPollInterval = %v (err %v), want 2h", cfg.CertPollInterval, err)
	}
	env := func(k string) string {
		if k == "OP_AGENT_CERT_POLL_INTERVAL" {
			return "3h"
		}
		return ""
	}
	if cfg, err := Load(append([]string{"-config", path}, base...), env); err != nil || cfg.CertPollInterval != 3*time.Hour {
		t.Fatalf("env CertPollInterval = %v (err %v), want 3h (env > file)", cfg.CertPollInterval, err)
	}
	args := append(append([]string{"-config", path}, base...), "-cert-poll-interval=4h")
	if cfg, err := Load(args, env); err != nil || cfg.CertPollInterval != 4*time.Hour {
		t.Fatalf("flag CertPollInterval = %v (err %v), want 4h (flag > env > file)", cfg.CertPollInterval, err)
	}
}

func TestLoadResolvesRelativeCAPathsAgainstSelectedConfig(t *testing.T) {
	dir := t.TempDir()
	absCA := filepath.Join(t.TempDir(), "absolute-ca.pem")
	path := filepath.Join(dir, "selected-agent.json")
	body := `{"gateway_url":"https://gw.example","token":"x","ca_file":"operator.pem","ca_cache_file":"cache/server-agent-ca.pem"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "operator.pem"); cfg.CAFile != want {
		t.Errorf("CAFile = %q, want %q", cfg.CAFile, want)
	}
	if want := filepath.Join(dir, "cache", "server-agent-ca.pem"); cfg.CACacheFile != want {
		t.Errorf("CACacheFile = %q, want %q", cfg.CACacheFile, want)
	}

	cfg, err = Load([]string{"-config", path, "-ca-file", absCA}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load absolute CA path: %v", err)
	}
	if cfg.CAFile != absCA {
		t.Errorf("absolute CAFile = %q, want unchanged %q", cfg.CAFile, absCA)
	}
}

func TestLoadCAFieldsRespectFlagEnvFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "precedence.json")
	body := `{"gateway_url":"https://gw.example","token":"x","ca_file":"file-ca.pem","ca_cache_file":"file-cache.pem","ca_pem":"file-pem"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := func(k string) string {
		switch k {
		case "OP_AGENT_CA_FILE":
			return "env-ca.pem"
		case "OP_AGENT_CA_CACHE_FILE":
			return "env-cache.pem"
		case "OP_AGENT_CA_PEM":
			return "env-pem"
		default:
			return ""
		}
	}

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load file precedence: %v", err)
	}
	if want := filepath.Join(dir, "file-ca.pem"); cfg.CAFile != want {
		t.Errorf("file CAFile = %q, want %q", cfg.CAFile, want)
	}
	if want := filepath.Join(dir, "file-cache.pem"); cfg.CACacheFile != want {
		t.Errorf("file CACacheFile = %q, want %q", cfg.CACacheFile, want)
	}
	if cfg.CAPEM != "file-pem" {
		t.Errorf("file CAPEM = %q, want file-pem", cfg.CAPEM)
	}

	cfg, err = Load([]string{"-config", path}, env)
	if err != nil {
		t.Fatalf("Load env precedence: %v", err)
	}
	if want := filepath.Join(dir, "env-ca.pem"); cfg.CAFile != want {
		t.Errorf("env CAFile = %q, want %q", cfg.CAFile, want)
	}
	if want := filepath.Join(dir, "env-cache.pem"); cfg.CACacheFile != want {
		t.Errorf("env CACacheFile = %q, want %q", cfg.CACacheFile, want)
	}
	if cfg.CAPEM != "env-pem" {
		t.Errorf("env CAPEM = %q, want env-pem", cfg.CAPEM)
	}

	cfg, err = Load([]string{
		"-config", path,
		"-ca-file", "flag-ca.pem",
		"-ca-cache-file", "flag-cache.pem",
		"-ca-pem", "flag-pem",
	}, env)
	if err != nil {
		t.Fatalf("Load flag precedence: %v", err)
	}
	if want := filepath.Join(dir, "flag-ca.pem"); cfg.CAFile != want {
		t.Errorf("flag CAFile = %q, want %q", cfg.CAFile, want)
	}
	if want := filepath.Join(dir, "flag-cache.pem"); cfg.CACacheFile != want {
		t.Errorf("flag CACacheFile = %q, want %q", cfg.CACacheFile, want)
	}
	if cfg.CAPEM != "flag-pem" {
		t.Errorf("flag CAPEM = %q, want flag-pem", cfg.CAPEM)
	}
}

// TestCertPollIntervalAutomaticAndFloor pins the "0 = automatic" contract: Task 5b
// picks the real cadence (websocket -> 6h, post -> 15m) from a ZERO value, so 0
// must stay 0 here — it must NEVER be floored to 1m, or "unset" would be
// indistinguishable from "configured to the floor".
func TestCertPollIntervalAutomaticAndFloor(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// unset -> automatic (0)
	if cfg, err := Load(base, noenv); err != nil || cfg.CertPollInterval != 0 {
		t.Fatalf("default CertPollInterval = %v (err %v), want 0 (automatic)", cfg.CertPollInterval, err)
	}
	// explicit "0" -> automatic, NOT floored
	if cfg, err := Load(append([]string{"-cert-poll-interval=0"}, base...), noenv); err != nil || cfg.CertPollInterval != 0 {
		t.Fatalf("-cert-poll-interval=0 -> %v (err %v), want 0 (automatic, not the 1m floor)", cfg.CertPollInterval, err)
	}
	// explicit "0s" -> automatic, NOT floored
	if cfg, err := Load(append([]string{"-cert-poll-interval=0s"}, base...), noenv); err != nil || cfg.CertPollInterval != 0 {
		t.Fatalf("-cert-poll-interval=0s -> %v (err %v), want 0 (automatic, not the 1m floor)", cfg.CertPollInterval, err)
	}
	// negative -> error
	if _, err := Load(append([]string{"-cert-poll-interval=-5m"}, base...), noenv); err == nil {
		t.Fatal("expected an error for a negative cert-poll-interval")
	}
	// below the 1m floor -> clamped up
	if cfg, err := Load(append([]string{"-cert-poll-interval=30s"}, base...), noenv); err != nil || cfg.CertPollInterval != time.Minute {
		t.Fatalf("-cert-poll-interval=30s -> %v (err %v), want 1m (floor)", cfg.CertPollInterval, err)
	}
	// exactly at the floor -> unchanged
	if cfg, err := Load(append([]string{"-cert-poll-interval=1m"}, base...), noenv); err != nil || cfg.CertPollInterval != time.Minute {
		t.Fatalf("-cert-poll-interval=1m -> %v (err %v), want 1m", cfg.CertPollInterval, err)
	}
	// above the floor -> unchanged
	if cfg, err := Load(append([]string{"-cert-poll-interval=15m"}, base...), noenv); err != nil || cfg.CertPollInterval != 15*time.Minute {
		t.Fatalf("-cert-poll-interval=15m -> %v (err %v), want 15m", cfg.CertPollInterval, err)
	}
	// invalid duration text -> error
	if _, err := Load(append([]string{"-cert-poll-interval=not-a-duration"}, base...), noenv); err == nil {
		t.Fatal("expected an error for an unparseable cert-poll-interval")
	}
}

// TestValidateCertModeMatrix pins the full cert-mode Validate() matrix, including
// the deliberate non-fatal case: a stray cert_dir/cert_reload_command set while
// cert_mode=off must NOT block agent startup (an unused certificate stanza would
// otherwise take down all telemetry) — it logs exactly one warning and continues.
func TestValidateCertModeMatrix(t *testing.T) {
	valid := func(mode, dir, reload string) Config {
		return Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: mode, CertDir: dir, CertReloadCommand: reload}
	}

	// unknown mode -> error
	if err := valid("bogus", "", "").Validate(); err == nil {
		t.Error("unknown cert_mode should error")
	}
	// "files" without cert_dir -> error
	if err := valid(CertModeFiles, "", "").Validate(); err == nil {
		t.Error("cert_mode=files without cert_dir should error")
	}
	// "files" with cert_dir -> valid, no warning
	buf := captureWarn(t)
	if err := valid(CertModeFiles, "/certs", "").Validate(); err != nil {
		t.Errorf("cert_mode=files with cert_dir should validate: %v", err)
	}
	if n := warnLineCount(buf); n != 0 {
		t.Errorf("cert_mode=files with cert_dir logged %d warnings, want 0:\n%s", n, buf.String())
	}
	// "proxy" without cert_dir -> error (same requirement as "files")
	if err := valid(CertModeProxy, "", "").Validate(); err == nil {
		t.Error("cert_mode=proxy without cert_dir should error")
	}
	// "proxy" with cert_dir -> valid, no warning (proxy is now implemented; the
	// former "not implemented yet" warning is gone).
	buf = captureWarn(t)
	if err := valid(CertModeProxy, "/certs", "").Validate(); err != nil {
		t.Errorf("cert_mode=proxy with cert_dir should validate: %v", err)
	}
	if n := warnLineCount(buf); n != 0 {
		t.Errorf("cert_mode=proxy with cert_dir logged %d warnings, want 0:\n%s", n, buf.String())
	}
	// "off" with nothing set -> valid, no warning
	buf = captureWarn(t)
	if err := valid(CertModeOff, "", "").Validate(); err != nil {
		t.Errorf("cert_mode=off with nothing set should validate: %v", err)
	}
	if n := warnLineCount(buf); n != 0 {
		t.Errorf("cert_mode=off with nothing set logged %d warnings, want 0:\n%s", n, buf.String())
	}
	// "off" with cert_dir AND cert_reload_command set -> NO error, exactly ONE warning
	buf = captureWarn(t)
	if err := valid(CertModeOff, "/stray-dir", "stray-reload.sh").Validate(); err != nil {
		t.Fatalf("cert_mode=off with a stray cert_dir/cert_reload_command must NOT error, got: %v", err)
	}
	if n := warnLineCount(buf); n != 1 {
		t.Fatalf("cert_mode=off with cert_dir+cert_reload_command logged %d warnings, want exactly 1:\n%s", n, buf.String())
	}
	// "off" with ONLY cert_dir set (no reload command) -> still exactly one warning
	buf = captureWarn(t)
	if err := valid(CertModeOff, "/stray-dir", "").Validate(); err != nil {
		t.Fatalf("cert_mode=off with only cert_dir set must NOT error, got: %v", err)
	}
	if n := warnLineCount(buf); n != 1 {
		t.Fatalf("cert_mode=off with only cert_dir set logged %d warnings, want exactly 1:\n%s", n, buf.String())
	}
}

// TestValidateProxyRoutes pins cert_proxy_routes validation: the listen boundary
// (1..65535 inclusive), the mode allow-list (empty resolves to fallback), and the
// upstream URL shape.
func TestValidateProxyRoutes(t *testing.T) {
	base := func() Config {
		return Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: CertModeProxy, CertDir: "/certs"}
	}

	// Valid upper boundary: listen 65535 with a well-formed upstream passes.
	c := base()
	c.CertProxyRoutes = []ProxyRoute{{Listen: 65535, Upstream: "http://127.0.0.1:9000"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("listen 65535 should be valid, got %v", err)
	}

	// Out-of-range listen ports are rejected (below 1 and above 65535).
	for _, bad := range []int{0, -1, 65536} {
		c := base()
		c.CertProxyRoutes = []ProxyRoute{{Listen: bad, Upstream: "http://127.0.0.1:9000"}}
		if err := c.Validate(); err == nil {
			t.Fatalf("listen %d should be rejected", bad)
		}
	}

	// Unknown mode rejected.
	c = base()
	c.CertProxyRoutesMode = "sideways"
	if err := c.Validate(); err == nil {
		t.Fatalf("cert_proxy_routes_mode=sideways should be rejected")
	}

	// Malformed upstream rejected (host without a port).
	c = base()
	c.CertProxyRoutes = []ProxyRoute{{Listen: 8600, Upstream: "http://127.0.0.1"}}
	if err := c.Validate(); err == nil {
		t.Fatalf("upstream without a port should be rejected")
	}

	// Empty mode resolves to fallback (byte-neutral) and validates with a good route.
	c = base()
	c.CertProxyRoutesMode = ""
	c.CertProxyRoutes = []ProxyRoute{{Listen: 8600, Upstream: "http://127.0.0.1:9000"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("empty mode + valid route should validate, got %v", err)
	}
}

// TestLoadEnforcesCertModeValidation proves Validate() is actually reached from
// Load() (not just unit-tested in isolation) for the new fields.
func TestLoadEnforcesCertModeValidation(t *testing.T) {
	if _, err := Load([]string{"-gateway-url=https://gw.example", "-token=x", "-cert-mode=files"}, func(string) string { return "" }); err == nil {
		t.Fatal("Load should reject cert_mode=files without a cert_dir")
	}
}

// TestConfigParsesProxyRoutesAndMode covers the parsing layer:
// cert_proxy_routes + cert_proxy_routes_mode parse from the config file and the
// validator rejects an unknown mode or a malformed route. Loading config never
// starts a proxy or calls the gateway itself -- that wiring lives in main.go
// (only for cert_mode=proxy).
func TestConfigParsesProxyRoutesAndMode(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x",`+
		`"cert_proxy_routes":[{"listen":8600,"upstream":"http://127.0.0.1:8080"}],`+
		`"cert_proxy_routes_mode":"override"}`)

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CertProxyRoutes) != 1 {
		t.Fatalf("CertProxyRoutes len = %d, want 1", len(cfg.CertProxyRoutes))
	}
	if cfg.CertProxyRoutes[0].Listen != 8600 {
		t.Errorf("Listen = %d, want 8600", cfg.CertProxyRoutes[0].Listen)
	}
	if cfg.CertProxyRoutes[0].Upstream != "http://127.0.0.1:8080" {
		t.Errorf("Upstream = %q, want %q", cfg.CertProxyRoutes[0].Upstream, "http://127.0.0.1:8080")
	}
	if cfg.CertProxyRoutesMode != "override" {
		t.Errorf("CertProxyRoutesMode = %q, want %q", cfg.CertProxyRoutesMode, "override")
	}
}

// TestConfigProxyRoutesDefaultWhenAbsent proves the byte-neutral default: a
// config file that never mentions cert_proxy_routes/_mode resolves to nil
// routes and mode "fallback" (no behavior change for existing config files).
func TestConfigProxyRoutesDefaultWhenAbsent(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x"}`)

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CertProxyRoutes != nil {
		t.Errorf("CertProxyRoutes = %#v, want nil when absent", cfg.CertProxyRoutes)
	}
	if cfg.CertProxyRoutesMode != CertProxyRoutesModeFallback {
		t.Errorf("CertProxyRoutesMode = %q, want %q", cfg.CertProxyRoutesMode, CertProxyRoutesModeFallback)
	}
}

// TestConfigRejectsInvalidProxyRoutesMode proves an unknown
// cert_proxy_routes_mode fails Load (via Validate).
func TestConfigRejectsInvalidProxyRoutesMode(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes_mode":"sideways"}`)

	if _, err := Load([]string{"-config", path}, func(string) string { return "" }); err == nil {
		t.Fatal("Load should reject an unknown cert_proxy_routes_mode")
	}
}

// TestConfigRejectsInvalidProxyRoutesEntries proves the validator checks each
// route's Listen (1..65535) and Upstream (absolute http(s)://host:port).
func TestConfigRejectsInvalidProxyRoutesEntries(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"listen zero", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":0,"upstream":"http://127.0.0.1:8080"}]}`},
		{"listen negative", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":-1,"upstream":"http://127.0.0.1:8080"}]}`},
		{"listen too big", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":65536,"upstream":"http://127.0.0.1:8080"}]}`},
		{"upstream wrong scheme", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":8600,"upstream":"ftp://127.0.0.1:8080"}]}`},
		{"upstream missing port", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":8600,"upstream":"http://127.0.0.1"}]}`},
		{"upstream not a URL", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":8600,"upstream":"not-a-url"}]}`},
		{"upstream empty", `{"gateway_url":"https://gw.example","token":"x","cert_proxy_routes":[{"listen":8600,"upstream":""}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			if _, err := Load([]string{"-config", path}, func(string) string { return "" }); err == nil {
				t.Fatalf("Load should reject invalid route: %s", tc.body)
			}
		})
	}
}

// TestValidateProxyRoutesModeZeroValueIsFallback proves a Config built
// directly (not via Load), leaving CertProxyRoutesMode at its Go zero value,
// still validates -- mirroring TestValidateCertModeMatrix's direct-construct
// style and guaranteeing the new field doesn't break byte-neutrality for
// callers that never set it.
func TestValidateProxyRoutesModeZeroValueIsFallback(t *testing.T) {
	cfg := Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: CertModeOff}
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero-value CertProxyRoutesMode should validate as fallback: %v", err)
	}
}

// TestRuntimeSourceDefaultsToGateway proves an unconfigured runtime source
// resolves to RuntimeSourceGateway -- the gateway is the default supplier of
// the runtime-config document.
func TestRuntimeSourceDefaultsToGateway(t *testing.T) {
	cfg, err := Load([]string{"-gateway-url=https://gw.example", "-token=x"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RuntimeSource != RuntimeSourceGateway {
		t.Fatalf("RuntimeSource = %q, want %q (default)", cfg.RuntimeSource, RuntimeSourceGateway)
	}
	if cfg.RuntimeConfigPath != "" {
		t.Errorf("RuntimeConfigPath = %q, want empty default", cfg.RuntimeConfigPath)
	}
}

// TestRuntimeSourcePrecedence mirrors TestCertModePrecedence's shape: file,
// then env-over-file, then flag-over-env, proven together with the paired
// RuntimeConfigPath field the same way CertMode is proven together with
// CertDir.
func TestRuntimeSourcePrecedence(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","runtime_source":"file","runtime_config":"/from-file/runtime.json"}`)
	// file only
	if cfg, err := Load([]string{"-config", path}, func(string) string { return "" }); err != nil || cfg.RuntimeSource != "file" || cfg.RuntimeConfigPath != "/from-file/runtime.json" {
		t.Fatalf("file runtime_source/runtime_config = %q/%q (err %v), want file//from-file/runtime.json", cfg.RuntimeSource, cfg.RuntimeConfigPath, err)
	}
	// env overrides file
	envOnly := func(k string) string {
		switch k {
		case "OP_AGENT_RUNTIME_SOURCE":
			return "gateway"
		case "OP_AGENT_RUNTIME_CONFIG":
			return "/from-env/runtime.json"
		}
		return ""
	}
	if cfg, err := Load([]string{"-config", path}, envOnly); err != nil || cfg.RuntimeSource != "gateway" || cfg.RuntimeConfigPath != "/from-env/runtime.json" {
		t.Fatalf("env runtime_source/runtime_config = %q/%q (err %v), want gateway//from-env/runtime.json (env > file)", cfg.RuntimeSource, cfg.RuntimeConfigPath, err)
	}
	// flag overrides env and file
	if cfg, err := Load([]string{"-config", path, "-runtime-source=file", "-runtime-config=/from-flag/runtime.json"}, envOnly); err != nil || cfg.RuntimeSource != "file" || cfg.RuntimeConfigPath != "/from-flag/runtime.json" {
		t.Fatalf("flag runtime_source/runtime_config = %q/%q (err %v), want file//from-flag/runtime.json (flag > env > file)", cfg.RuntimeSource, cfg.RuntimeConfigPath, err)
	}
}

// TestValidateRuntimeSourceEnum pins the exact enum error format, matching
// the house pattern (e.g. Transport's error).
func TestValidateRuntimeSourceEnum(t *testing.T) {
	cfg := Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: CertModeOff, RuntimeSource: "bogus"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("unknown runtime-source should error")
	}
	want := `runtime-source must be "gateway" or "file", got "bogus"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestRuntimeSourceZeroValueIsGateway proves a directly-constructed
// zero-value Config (RuntimeSource never set) still validates, mirroring
// TestValidateProxyRoutesModeZeroValueIsFallback's byte-neutrality guarantee.
func TestRuntimeSourceZeroValueIsGateway(t *testing.T) {
	cfg := Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: CertModeOff}
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero-value RuntimeSource should validate as gateway: %v", err)
	}
}

// TestRuntimeFileModeRequiresConfigPath proves runtime-source=file without a
// runtime-config path fails Validate -- the file source is useless without
// knowing which file.
func TestRuntimeFileModeRequiresConfigPath(t *testing.T) {
	cfg := Config{GatewayURL: "https://gw.example", Token: "x", Interval: time.Second, Transport: TransportWebSocket, CertMode: CertModeOff, RuntimeSource: RuntimeSourceFile}
	if err := cfg.Validate(); err == nil {
		t.Fatal("runtime-source=file without runtime-config should error")
	}
	cfg.RuntimeConfigPath = "/some/runtime.json"
	if err := cfg.Validate(); err != nil {
		t.Errorf("runtime-source=file with runtime-config set should validate: %v", err)
	}
}

// TestRuntimeAllowedBinariesFromEnvCommaSeparated proves the comma-separated
// env encoding for the structured (no-flag) RuntimeAllowedBinaries field:
// entries are split on commas and trimmed of surrounding whitespace.
func TestRuntimeAllowedBinariesFromEnvCommaSeparated(t *testing.T) {
	env := func(k string) string {
		if k == "OP_AGENT_RUNTIME_ALLOWED_BINARIES" {
			return "/usr/bin/ollama, /usr/local/bin/llama-server ,/opt/vllm/bin/vllm"
		}
		return ""
	}
	cfg, err := Load([]string{"-gateway-url=https://gw.example", "-token=x"}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"/usr/bin/ollama", "/usr/local/bin/llama-server", "/opt/vllm/bin/vllm"}
	if !reflect.DeepEqual(cfg.RuntimeAllowedBinaries, want) {
		t.Errorf("RuntimeAllowedBinaries = %#v, want %#v", cfg.RuntimeAllowedBinaries, want)
	}
}

// TestRuntimeAllowedDirsFromEnvCommaSeparated proves RuntimeAllowedDirs
// follows the identical comma-separated env pattern as RuntimeAllowedBinaries.
func TestRuntimeAllowedDirsFromEnvCommaSeparated(t *testing.T) {
	env := func(k string) string {
		if k == "OP_AGENT_RUNTIME_ALLOWED_DIRS" {
			return "/srv/models, /srv/other-models "
		}
		return ""
	}
	cfg, err := Load([]string{"-gateway-url=https://gw.example", "-token=x"}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"/srv/models", "/srv/other-models"}
	if !reflect.DeepEqual(cfg.RuntimeAllowedDirs, want) {
		t.Errorf("RuntimeAllowedDirs = %#v, want %#v", cfg.RuntimeAllowedDirs, want)
	}
}

// TestRuntimeAllowedBinariesAndDirsFromConfigFile proves the JSON-array file
// encoding, and that an env value overrides the file (env > file, same
// precedence as every other tri-source-minus-flag field in this package).
func TestRuntimeAllowedBinariesAndDirsFromConfigFile(t *testing.T) {
	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x",`+
		`"runtime_allowed_binaries":["/usr/bin/ollama"],`+
		`"runtime_allowed_dirs":["/srv/models"]}`)

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"/usr/bin/ollama"}; !reflect.DeepEqual(cfg.RuntimeAllowedBinaries, want) {
		t.Errorf("RuntimeAllowedBinaries = %#v, want %#v", cfg.RuntimeAllowedBinaries, want)
	}
	if want := []string{"/srv/models"}; !reflect.DeepEqual(cfg.RuntimeAllowedDirs, want) {
		t.Errorf("RuntimeAllowedDirs = %#v, want %#v", cfg.RuntimeAllowedDirs, want)
	}

	env := func(k string) string {
		if k == "OP_AGENT_RUNTIME_ALLOWED_BINARIES" {
			return "/opt/vllm/bin/vllm"
		}
		return ""
	}
	cfg, err = Load([]string{"-config", path}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"/opt/vllm/bin/vllm"}; !reflect.DeepEqual(cfg.RuntimeAllowedBinaries, want) {
		t.Errorf("RuntimeAllowedBinaries (env-over-file) = %#v, want %#v", cfg.RuntimeAllowedBinaries, want)
	}
	// runtime_allowed_dirs untouched by env: still the file value.
	if want := []string{"/srv/models"}; !reflect.DeepEqual(cfg.RuntimeAllowedDirs, want) {
		t.Errorf("RuntimeAllowedDirs (file, env did not set it) = %#v, want %#v", cfg.RuntimeAllowedDirs, want)
	}
}

// TestRuntimeAllowedBinariesAndDirsNilWhenAbsent proves the byte-neutral
// default (mirroring TestConfigProxyRoutesDefaultWhenAbsent): a config that
// never mentions either key resolves to nil, not an allocated-but-empty
// slice, since neither env var nor file key was present at all.
func TestRuntimeAllowedBinariesAndDirsNilWhenAbsent(t *testing.T) {
	cfg, err := Load([]string{"-gateway-url=https://gw.example", "-token=x"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RuntimeAllowedBinaries != nil {
		t.Errorf("RuntimeAllowedBinaries = %#v, want nil when absent", cfg.RuntimeAllowedBinaries)
	}
	if cfg.RuntimeAllowedDirs != nil {
		t.Errorf("RuntimeAllowedDirs = %#v, want nil when absent", cfg.RuntimeAllowedDirs)
	}
}

// TestRuntimeCachePathPrecedence proves flag > env > file > the
// next-to-binary default for RuntimeCachePath, mirroring the defaultConfigName
// precedent (TestExplicitMissingConfigErrors's use of the executable() seam).
func TestRuntimeCachePathPrecedence(t *testing.T) {
	base := []string{"-gateway-url=https://gw.example", "-token=x"}
	noenv := func(string) string { return "" }

	// default: next to the (stubbed) binary.
	orig := executable
	t.Cleanup(func() { executable = orig })
	binDir := t.TempDir()
	executable = func() (string, error) { return filepath.Join(binDir, "server-agent"), nil }
	if cfg, err := Load(base, noenv); err != nil || cfg.RuntimeCachePath != filepath.Join(binDir, "server-agent-runtime.cache.json") {
		t.Fatalf("default RuntimeCachePath = %q (err %v), want %q", cfg.RuntimeCachePath, err, filepath.Join(binDir, "server-agent-runtime.cache.json"))
	}

	path := writeConfig(t, `{"gateway_url":"https://gw.example","token":"x","runtime_cache":"/from-file/runtime.cache.json"}`)
	if cfg, err := Load([]string{"-config", path}, noenv); err != nil || cfg.RuntimeCachePath != "/from-file/runtime.cache.json" {
		t.Fatalf("file RuntimeCachePath = %q (err %v), want /from-file/runtime.cache.json", cfg.RuntimeCachePath, err)
	}
	env := func(k string) string {
		if k == "OP_AGENT_RUNTIME_CACHE" {
			return "/from-env/runtime.cache.json"
		}
		return ""
	}
	if cfg, err := Load([]string{"-config", path}, env); err != nil || cfg.RuntimeCachePath != "/from-env/runtime.cache.json" {
		t.Fatalf("env RuntimeCachePath = %q (err %v), want /from-env/runtime.cache.json (env > file)", cfg.RuntimeCachePath, err)
	}
	args := append([]string{"-config", path}, "-runtime-cache=/from-flag/runtime.cache.json")
	if cfg, err := Load(args, env); err != nil || cfg.RuntimeCachePath != "/from-flag/runtime.cache.json" {
		t.Fatalf("flag RuntimeCachePath = %q (err %v), want /from-flag/runtime.cache.json (flag > env > file)", cfg.RuntimeCachePath, err)
	}
}

// agentConfigJSONCFixture is a byte-for-byte copy of what
// gateway/backend/internal/gateway/agent_binaries.go's buildAgentConfigJSON
// produces for gateway_url="https://gw.example.test", token="fixture-token" (as
// of the certificate-installation and mesh-trust additions). It is a COPY, not an import — the
// gateway is a separate Go module this package cannot import — so it can drift;
// the gateway side additionally pins the exact KEY SET of its own template
// against a maintained list (TestBuildAgentConfigJSONKeySet in
// gateway/backend/internal/gateway/agent_binaries_test.go) so an added/removed
// key is caught there even if this fixture is not updated. This test instead
// proves the OTHER direction: feeding that exact text through the REAL Load()
// yields the documented default for every field.
const agentConfigJSONCFixture = `{
  // The gateway base URL the agent sends telemetry to (origin only, no path).
  // Required. Example: https://gateway.example.com. Under a NetBird-only
  // restriction, use the gateway's mesh URL here instead.
  "gateway_url": "https://gw.example.test",

  // The per-server agent bearer token, shown once when generated in the portal.
  // It identifies this server to the gateway. Required. Keep this file private
  // (e.g. chmod 600) because it holds the token.
  "token": "fixture-token",

  // Telemetry transport: "websocket" (default; one persistent connection, cheap
  // for high-frequency sending) or "post" (one HTTP POST per sample).
  "transport": "websocket",

  // Collection cadence as a Go duration, e.g. "500ms", "1s", "5s". Clamped up to
  // a 250ms floor. Default "1s".
  "interval": "1s",

  // POST-mode re-send cadence for the static hardware inventory (self-heals a
  // gateway restart). Floored at "1m"; ignored under the websocket transport.
  // Default "30m".
  "system_report_interval": "30m",

  // Optional inference /metrics (Prometheus) URL to scrape for active/queued
  // request counts. Empty disables it.
  "metrics_url": "",

  // Optional endpoint polled each cycle for currently-loaded models, e.g.
  // "/running" for llama-swap, "/props" for llama.cpp, "/v1/models" for vLLM.
  // Empty disables it.
  "model_status_url": "",

  // How to parse model_status_url: "openai" | "llama_swap" | "llama_cpp" |
  // "litellm" | "" or "auto" (tolerant union of all shapes). Empty = auto.
  "model_status_format": "",

  // Optional LibreHardwareMonitor Remote Web Server /data.json URL for CPU (and
  // best-effort system) power watts. Empty disables it.
  "lhm_url": "",

  // Certificate installation mode: "off" (default, never fetch a certificate),
  // "files" (write fullchain/cert/chain/ca/privkey into cert_dir and run
  // cert_reload_command on change), or "proxy" (accepted for a future release;
  // NOT IMPLEMENTED YET, behaves like "files"). Required cert_dir when not "off".
  "cert_mode": "off",

  // Directory certificate files are written to. Required when cert_mode is not
  // "off".
  "cert_dir": "",

  // Local command run after a changed certificate is fully installed on disk.
  // This value comes ONLY from this local file -- the gateway can never deliver
  // a command to run. On Windows, keep the value quote-free (no embedded quotes).
  "cert_reload_command": "",

  // Certificate poll cadence as a Go duration, e.g. "15m". Empty or "0"/"0s" means
  // automatic (websocket transport polls every 6h, post every 15m). A configured
  // positive value below "1m" is clamped up to "1m".
  "cert_poll_interval": "",

  // Optional operator-managed CA bundle. Generated configs leave this empty;
  // the agent never writes this file.
  "ca_file": "",

  // Optional agent-managed CA cache, relative to this config file when not
  // absolute. Self-signed gateway configs use "server-agent-ca.pem".
  "ca_cache_file": "",

  // Optional inline CA bootstrap bundle. Present only when the gateway's
  // currently served leaf is signed by the internal CA.
  "ca_pem": "",

  // Skip TLS certificate verification. Self-signed dev gateways only. Default false.
  "tls_insecure": false,

  // Verbose mode: emit detailed debug logs to stderr. Default false.
  "verbose": false
}
`

func TestAgentConfigJSONCFixtureLoadsToDocumentedDefaults(t *testing.T) {
	path := writeConfig(t, agentConfigJSONCFixture)
	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load(fixture): %v", err)
	}
	if cfg.GatewayURL != "https://gw.example.test" {
		t.Errorf("GatewayURL = %q", cfg.GatewayURL)
	}
	if cfg.Token != "fixture-token" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.Transport != TransportWebSocket {
		t.Errorf("Transport = %q, want websocket", cfg.Transport)
	}
	if cfg.Interval != time.Second {
		t.Errorf("Interval = %v, want 1s", cfg.Interval)
	}
	if cfg.SystemReportInterval != 30*time.Minute {
		t.Errorf("SystemReportInterval = %v, want 30m", cfg.SystemReportInterval)
	}
	if cfg.MetricsURL != "" || cfg.ModelStatusURL != "" || cfg.ModelStatusFormat != "" || cfg.LHMURL != "" {
		t.Errorf("optional endpoints not empty: metrics=%q model_status_url=%q model_status_format=%q lhm=%q",
			cfg.MetricsURL, cfg.ModelStatusURL, cfg.ModelStatusFormat, cfg.LHMURL)
	}
	if cfg.CertMode != CertModeOff {
		t.Errorf("CertMode = %q, want %q", cfg.CertMode, CertModeOff)
	}
	if cfg.CertDir != "" {
		t.Errorf("CertDir = %q, want empty", cfg.CertDir)
	}
	if cfg.CertReloadCommand != "" {
		t.Errorf("CertReloadCommand = %q, want empty", cfg.CertReloadCommand)
	}
	if cfg.CertPollInterval != 0 {
		t.Errorf("CertPollInterval = %v, want 0 (automatic)", cfg.CertPollInterval)
	}
	if cfg.TLSInsecure {
		t.Error("TLSInsecure = true, want false")
	}
	if cfg.Verbose {
		t.Error("Verbose = true, want false")
	}
}

func TestGeneratedAgentConfigFixtureResolvesMeshTrustDefaults(t *testing.T) {
	const bootstrap = "-----BEGIN CERTIFICATE-----\\nbootstrap\\n-----END CERTIFICATE-----"
	body := strings.Replace(agentConfigJSONCFixture,
		`"ca_cache_file": ""`, `"ca_cache_file": "server-agent-ca.pem"`, 1)
	body = strings.Replace(body,
		`"ca_pem": ""`, `"ca_pem": "-----BEGIN CERTIFICATE-----\\nbootstrap\\n-----END CERTIFICATE-----"`, 1)
	path := writeConfig(t, body)

	cfg, err := Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load generated fixture: %v", err)
	}
	if cfg.CAFile != "" {
		t.Errorf("CAFile = %q, want empty operator override", cfg.CAFile)
	}
	if want := filepath.Join(filepath.Dir(path), "server-agent-ca.pem"); cfg.CACacheFile != want {
		t.Errorf("CACacheFile = %q, want %q", cfg.CACacheFile, want)
	}
	if cfg.CAPEM != bootstrap {
		t.Errorf("CAPEM = %q, want generated inline bootstrap", cfg.CAPEM)
	}
}
