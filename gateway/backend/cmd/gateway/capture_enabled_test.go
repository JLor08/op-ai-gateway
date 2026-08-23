// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/portal"
	"path/filepath"
	"testing"
	"time"
)

func TestNewCaptureFlagsHookCachesForTTL(t *testing.T) {
	settings := portal.NewMemorySystemSettings()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	hook := newCaptureFlagsHook(settings, clock)

	if got := hook(); !got.Enabled || got.Override {
		t.Fatalf("hook() = %+v, want {Enabled:true, Override:false} (defaults before any write)", got)
	}

	if err := settings.SetSystemSetting(context.Background(), "capture_enabled", "false", now); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	if err := settings.SetSystemSetting(context.Background(), "capture_override", "true", now); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	if got := hook(); !got.Enabled || got.Override {
		t.Fatalf("hook() = %+v, want the cached defaults to still hold before the TTL elapses", got)
	}

	now = now.Add(6 * time.Second)
	if got := hook(); got.Enabled || !got.Override {
		t.Fatalf("hook() = %+v, want {Enabled:false, Override:true} once the cache re-reads after the TTL", got)
	}
}

type erroringSystemSettings struct{}

func (erroringSystemSettings) SystemSettings(context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("boom")
}

func (erroringSystemSettings) SetSystemSetting(context.Context, string, string, time.Time) error {
	return fmt.Errorf("boom")
}

func TestNewCaptureFlagsHookFailsOpenOnReadError(t *testing.T) {
	hook := newCaptureFlagsHook(erroringSystemSettings{}, time.Now)
	if got := hook(); !got.Enabled || got.Override {
		t.Fatalf("hook() = %+v, want {Enabled:true, Override:false} (fail-open on a SystemSettings read error)", got)
	}
}

func TestBuildGatewayServerWiresCaptureHooksMemory(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"}

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	defer cleanup()

	if srv.CaptureEnabled == nil || srv.CaptureOverride == nil {
		t.Fatalf("CaptureEnabled nil=%v, CaptureOverride nil=%v, want both wired", srv.CaptureEnabled == nil, srv.CaptureOverride == nil)
	}
	if !srv.CaptureEnabled() {
		t.Fatalf("srv.CaptureEnabled() = false, want true (default-on, no setting written yet)")
	}
	if srv.CaptureOverride() {
		t.Fatalf("srv.CaptureOverride() = true, want false (default off, no setting written yet)")
	}
}

func TestBuildGatewayServerWiresCaptureHooksSqlite(t *testing.T) {
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

	if srv.CaptureEnabled == nil || srv.CaptureOverride == nil {
		t.Fatalf("CaptureEnabled nil=%v, CaptureOverride nil=%v, want both wired", srv.CaptureEnabled == nil, srv.CaptureOverride == nil)
	}
	if !srv.CaptureEnabled() {
		t.Fatalf("srv.CaptureEnabled() = false, want true (default-on, no setting written yet)")
	}
	if srv.CaptureOverride() {
		t.Fatalf("srv.CaptureOverride() = true, want false (default off, no setting written yet)")
	}
}
