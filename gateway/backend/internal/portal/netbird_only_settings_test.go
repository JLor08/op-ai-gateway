// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// erroringSettings is a SystemSettingsStore whose SystemSettings always fails,
// so the Service accessors can be exercised on the fail-open (default) path.
type erroringSettings struct{}

func (erroringSettings) SystemSettings(context.Context) (map[string]string, error) {
	return nil, errors.New("boom")
}

func (erroringSettings) SetSystemSetting(context.Context, string, string, time.Time) error {
	return errors.New("boom")
}

func TestNetbirdOnlyHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"netbird_only": ""}, false},
		{map[string]string{"netbird_only": "nope"}, false},
		{map[string]string{"netbird_only": "true"}, true},
		{map[string]string{"netbird_only": "false"}, false},
		{map[string]string{"netbird_only": "1"}, true},
	}
	for _, tc := range cases {
		if got := NetbirdOnly(tc.in); got != tc.want {
			t.Fatalf("NetbirdOnly(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNetbirdGatewayPeerIDHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want string
	}{
		{map[string]string{}, ""},
		{map[string]string{"netbird_gateway_peer_id": "  peer-x  "}, "peer-x"},
		{map[string]string{"netbird_gateway_peer_id": "peer-y"}, "peer-y"},
	}
	for _, tc := range cases {
		if got := NetbirdGatewayPeerID(tc.in); got != tc.want {
			t.Fatalf("NetbirdGatewayPeerID(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpdateNetbirdOnlyAndGatewayPeerRoundTrip(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	// Defaults before any write.
	if dto := svc.SystemSettingsView(context.Background()); dto.NetbirdOnly || dto.NetbirdGatewayPeerID != "" {
		t.Fatalf("defaults = {only:%v peer:%q}, want {false \"\"}", dto.NetbirdOnly, dto.NetbirdGatewayPeerID)
	}

	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdOnly:          boolPtr(true),
		NetbirdGatewayPeerID: strPtr("  peer-x  "),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if !got.NetbirdOnly {
		t.Fatalf("DTO NetbirdOnly = false, want true")
	}
	if got.NetbirdGatewayPeerID != "peer-x" {
		t.Fatalf("DTO NetbirdGatewayPeerID = %q, want peer-x (trimmed)", got.NetbirdGatewayPeerID)
	}

	// Persisted values.
	values, _ := settings.SystemSettings(context.Background())
	if values["netbird_only"] != "true" {
		t.Fatalf("stored netbird_only = %q, want true", values["netbird_only"])
	}
	if values["netbird_gateway_peer_id"] != "peer-x" {
		t.Fatalf("stored netbird_gateway_peer_id = %q, want peer-x", values["netbird_gateway_peer_id"])
	}

	// Re-fetch round-trips.
	refetch := svc.SystemSettingsView(context.Background())
	if !refetch.NetbirdOnly || refetch.NetbirdGatewayPeerID != "peer-x" {
		t.Fatalf("refetch = {only:%v peer:%q}, want {true peer-x}", refetch.NetbirdOnly, refetch.NetbirdGatewayPeerID)
	}
}

func TestServiceNetbirdOnlyAndGatewayPeerAccessors(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	// Empty store -> safe defaults.
	if svc.NetbirdOnly(context.Background()) {
		t.Fatalf("NetbirdOnly on empty store = true, want false")
	}
	if got := svc.NetbirdGatewayPeerID(context.Background()); got != "" {
		t.Fatalf("NetbirdGatewayPeerID on empty store = %q, want empty", got)
	}

	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdOnly:          boolPtr(true),
		NetbirdGatewayPeerID: strPtr("peer-x"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !svc.NetbirdOnly(context.Background()) {
		t.Fatalf("NetbirdOnly = false after set true, want true")
	}
	if got := svc.NetbirdGatewayPeerID(context.Background()); got != "peer-x" {
		t.Fatalf("NetbirdGatewayPeerID = %q, want peer-x", got)
	}

	// Errored store -> safe defaults, never propagated.
	errSvc := NewService(ServiceDeps{SystemSettings: erroringSettings{}, Clock: fixedClock()})
	if errSvc.NetbirdOnly(context.Background()) {
		t.Fatalf("NetbirdOnly on errored store = true, want false (safe default)")
	}
	if got := errSvc.NetbirdGatewayPeerID(context.Background()); got != "" {
		t.Fatalf("NetbirdGatewayPeerID on errored store = %q, want empty (safe default)", got)
	}
}
