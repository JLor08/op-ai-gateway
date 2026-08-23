// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNetbirdEnabledHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"netbird_enabled": ""}, false},
		{map[string]string{"netbird_enabled": "nope"}, false},
		{map[string]string{"netbird_enabled": "true"}, true},
		{map[string]string{"netbird_enabled": "false"}, false},
	}
	for _, tc := range cases {
		if got := NetbirdEnabled(tc.in); got != tc.want {
			t.Fatalf("NetbirdEnabled(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNetbirdURLHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want string
	}{
		{map[string]string{}, ""},
		{map[string]string{"netbird_url": "  https://api.netbird.io/  "}, "https://api.netbird.io"},
		{map[string]string{"netbird_url": "https://api.netbird.io///"}, "https://api.netbird.io"},
		{map[string]string{"netbird_url": "http://host:8080"}, "http://host:8080"},
	}
	for _, tc := range cases {
		if got := NetbirdURL(tc.in); got != tc.want {
			t.Fatalf("NetbirdURL(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNetbirdGroupsHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{"absent", map[string]string{}, nil},
		{"blank", map[string]string{"netbird_group": "   "}, nil},
		{"empty json array", map[string]string{"netbird_group": "[]"}, nil},
		{"json null", map[string]string{"netbird_group": "null"}, nil},
		{"json list", map[string]string{"netbird_group": `["a","b"]`}, []string{"a", "b"}},
		{"json list trims/dedupes", map[string]string{"netbird_group": `[" a ","a","","b"]`}, []string{"a", "b"}},
		{"legacy single value", map[string]string{"netbird_group": "gateways"}, []string{"gateways"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NetbirdGroups(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("NetbirdGroups(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NetbirdGroups(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestUpdateNetbirdBasics(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr("https://api.netbird.io/"),
		NetbirdGroups:  &[]string{"gateways", "prod"},
		NetbirdToken:   strPtr("nbtok"),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if !got.NetbirdEnabled || got.NetbirdURL != "https://api.netbird.io" || !got.NetbirdTokenSet {
		t.Fatalf("DTO = %+v, want enabled/trimmed-url/token-set", got)
	}
	if len(got.NetbirdGroups) != 2 || got.NetbirdGroups[0] != "gateways" || got.NetbirdGroups[1] != "prod" {
		t.Fatalf("DTO NetbirdGroups = %v, want [gateways prod]", got.NetbirdGroups)
	}
	values, _ := settings.SystemSettings(context.Background())
	if values["netbird_enabled"] != "true" || values["netbird_url"] != "https://api.netbird.io" || values["netbird_group"] != `["gateways","prod"]` {
		t.Fatalf("stored = %v", values)
	}
	if !strings.HasPrefix(values["netbird_token"], "enc:") {
		t.Fatalf("stored token = %q, want enc: prefix", values["netbird_token"])
	}
}

func TestUpdateNetbirdTokenKeepClearReplace(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})

	// replace: non-empty -> stored + netbird_token_set true
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdToken: strPtr("secret1")})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !got.NetbirdTokenSet {
		t.Fatalf("after replace NetbirdTokenSet = false, want true")
	}
	values, _ := settings.SystemSettings(context.Background())
	if !strings.HasPrefix(values["netbird_token"], "enc:") {
		t.Fatalf("stored token = %q, want enc: prefix", values["netbird_token"])
	}

	// keep: nil -> unchanged
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdGroups: &[]string{"g"}}); err != nil {
		t.Fatalf("keep: %v", err)
	}
	after, _ := settings.SystemSettings(context.Background())
	if after["netbird_token"] != values["netbird_token"] {
		t.Fatalf("keep changed the token: %q -> %q", values["netbird_token"], after["netbird_token"])
	}

	// clear: "" -> stored "" + netbird_token_set false
	got, err = svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdToken: strPtr("")})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got.NetbirdTokenSet {
		t.Fatalf("after clear NetbirdTokenSet = true, want false")
	}
	cleared, _ := settings.SystemSettings(context.Background())
	if cleared["netbird_token"] != "" {
		t.Fatalf("stored token after clear = %q, want empty", cleared["netbird_token"])
	}
}

func TestSystemSettingsDTONeverExposesNetbirdToken(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdToken: strPtr("topsecret")}); err != nil {
		t.Fatalf("set token: %v", err)
	}
	blob, err := json.Marshal(svc.SystemSettingsView(context.Background()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "topsecret") || strings.Contains(string(blob), "netbird_token\"") {
		t.Fatalf("DTO JSON leaks the token: %s", blob)
	}
	if !strings.Contains(string(blob), "\"netbird_token_set\":true") {
		t.Fatalf("DTO JSON missing netbird_token_set: %s", blob)
	}
}

func TestUpdateNetbirdEnableValidation(t *testing.T) {
	newSvc := func() *Service {
		return NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: fixedClock()})
	}
	// enable with an empty url (not yet configured) -> allowed, no error
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("enable with empty url: %v, want nil", err)
	}
	// enable with a valid url but no token -> allowed, no error (module is
	// on but not yet usable; NetbirdConfig ok stays false until url+token)
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true), NetbirdURL: strPtr("https://api.netbird.io"),
	}); err != nil {
		t.Fatalf("enable with url but no token: %v, want nil", err)
	}
	// enable with a bad url (no scheme) -> url invalid
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true), NetbirdURL: strPtr("not-a-url"), NetbirdToken: strPtr("tok"),
	}); !errors.Is(err, ErrNetbirdURLInvalid) {
		t.Fatalf("no-scheme url error = %v, want ErrNetbirdURLInvalid", err)
	}
	// enable with a non-http scheme -> url invalid
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true), NetbirdURL: strPtr("ftp://host"), NetbirdToken: strPtr("tok"),
	}); !errors.Is(err, ErrNetbirdURLInvalid) {
		t.Fatalf("ftp url error = %v, want ErrNetbirdURLInvalid", err)
	}
	// disabled -> nil regardless of url/token
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(false), NetbirdURL: strPtr("not-a-url"),
	}); err != nil {
		t.Fatalf("disabled: %v, want nil", err)
	}
	// valid enable (url + token) succeeds
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true), NetbirdURL: strPtr("https://api.netbird.io"), NetbirdToken: strPtr("tok"),
	}); err != nil {
		t.Fatalf("valid enable: %v", err)
	}
}

// TestValidateNetbirdEmptyURLAllowed pins the exact behavior change: enabling
// the module before url/token are configured must not error, while a
// NON-EMPTY malformed url is still rejected.
func TestValidateNetbirdEmptyURLAllowed(t *testing.T) {
	if err := validateNetbird(map[string]string{"netbird_enabled": "true"}); err != nil {
		t.Fatalf("enabled + empty url: %v, want nil", err)
	}
	if err := validateNetbird(map[string]string{
		"netbird_enabled": "true",
		"netbird_url":     "https://api.netbird.io",
	}); err != nil {
		t.Fatalf("enabled + valid url + no token: %v, want nil", err)
	}
	if err := validateNetbird(map[string]string{
		"netbird_enabled": "true",
		"netbird_url":     "not-a-url",
	}); !errors.Is(err, ErrNetbirdURLInvalid) {
		t.Fatalf("enabled + malformed url error = %v, want ErrNetbirdURLInvalid", err)
	}
	if err := validateNetbird(map[string]string{}); err != nil {
		t.Fatalf("disabled: %v, want nil", err)
	}
}

func TestUpdateNetbirdTokenDiskWithoutKeyRejected(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()}) // no cipher, not volatile
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{NetbirdToken: strPtr("secret")})
	if !errors.Is(err, ErrNetbirdKeyRequired) {
		t.Fatalf("error = %v, want ErrNetbirdKeyRequired", err)
	}
}

func TestNetbirdConfigDisabled(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: fixedClock()})
	// Store url + token but leave the module disabled.
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdURL: strPtr("https://api.netbird.io"), NetbirdToken: strPtr("tok"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, ok, err := svc.NetbirdConfig(context.Background())
	if err != nil {
		t.Fatalf("NetbirdConfig: %v", err)
	}
	if ok {
		t.Fatalf("NetbirdConfig ok = true when disabled, want false (cfg=%+v)", cfg)
	}
}

func TestNetbirdModuleChecked(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: fixedClock()})

	// Off by default.
	if svc.NetbirdModuleChecked(context.Background()) {
		t.Fatalf("NetbirdModuleChecked = true before any settings write, want false")
	}

	// Enabled with NO url/token -> still reports the raw checkbox as true
	// (unlike NetbirdModuleEnabled, which stays false until fully configured).
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !svc.NetbirdModuleChecked(context.Background()) {
		t.Fatalf("NetbirdModuleChecked = false with netbird_enabled=true (no url/token), want true")
	}
	if svc.NetbirdModuleEnabled(context.Background()) {
		t.Fatalf("NetbirdModuleEnabled = true with no url/token configured, want false")
	}

	// Disabled again -> false.
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(false),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if svc.NetbirdModuleChecked(context.Background()) {
		t.Fatalf("NetbirdModuleChecked = true after disabling, want false")
	}
}

func TestNetbirdModuleCheckedNilSettings(t *testing.T) {
	svc := NewService(ServiceDeps{})
	if svc.NetbirdModuleChecked(context.Background()) {
		t.Fatalf("NetbirdModuleChecked with nil settings = true, want false")
	}
}

func TestNetbirdConfigEnabledReadsToken(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr("https://api.netbird.io"),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   strPtr("nbtok"),
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	cfg, ok, err := svc.NetbirdConfig(context.Background())
	if err != nil {
		t.Fatalf("NetbirdConfig: %v", err)
	}
	if !ok {
		t.Fatalf("NetbirdConfig ok = false, want true")
	}
	if cfg.URL != "https://api.netbird.io" || cfg.Token != "nbtok" || len(cfg.Groups) != 1 || cfg.Groups[0] != "gateways" {
		t.Fatalf("cfg = %+v, want url/nbtok/[gateways]", cfg)
	}
	if !svc.NetbirdModuleEnabled(context.Background()) {
		t.Fatalf("NetbirdModuleEnabled = false, want true")
	}
}
