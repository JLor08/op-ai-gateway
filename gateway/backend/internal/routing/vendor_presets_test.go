// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestIsOpenAICompatibleVendor(t *testing.T) {
	compat := []string{VendorXAI, VendorOpenRouter, VendorKilo, VendorGoogle, VendorOpenAICompatible}
	for _, v := range compat {
		if !IsOpenAICompatibleVendor(v) {
			t.Errorf("IsOpenAICompatibleVendor(%q) = false, want true", v)
		}
	}
	for _, v := range []string{VendorOpenAI, VendorAnthropic, "", "unknown"} {
		if IsOpenAICompatibleVendor(v) {
			t.Errorf("IsOpenAICompatibleVendor(%q) = true, want false", v)
		}
	}
}

func TestVendorPresetForComposesURLs(t *testing.T) {
	cases := []struct {
		vendor, wantChat, wantModels string
	}{
		{VendorXAI, "https://api.x.ai/v1/chat/completions", "https://api.x.ai/v1/models"},
		{VendorOpenRouter, "https://openrouter.ai/api/v1/chat/completions", "https://openrouter.ai/api/v1/models"},
		{VendorKilo, "https://api.kilo.ai/api/gateway/chat/completions", "https://api.kilo.ai/api/gateway/models"},
		{VendorGoogle, "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "https://generativelanguage.googleapis.com/v1beta/openai/models"},
	}
	for _, c := range cases {
		p, ok := VendorPresetFor(c.vendor)
		if !ok {
			t.Fatalf("VendorPresetFor(%q) ok=false", c.vendor)
		}
		gotChat := p.DefaultBaseURL + OpenAIPathPrefixFor(c.vendor) + "/chat/completions"
		if gotChat != c.wantChat {
			t.Errorf("%s chat = %q, want %q", c.vendor, gotChat, c.wantChat)
		}
		gotModels := p.DefaultBaseURL + OpenAIPathPrefixFor(c.vendor) + "/models"
		if gotModels != c.wantModels {
			t.Errorf("%s models = %q, want %q", c.vendor, gotModels, c.wantModels)
		}
	}
}

func TestVendorPresetCustomHasNoDefaultBaseURL(t *testing.T) {
	p, ok := VendorPresetFor(VendorOpenAICompatible)
	if !ok {
		t.Fatal("custom preset missing")
	}
	if p.DefaultBaseURL != "" {
		t.Errorf("custom DefaultBaseURL = %q, want empty", p.DefaultBaseURL)
	}
	if OpenAIPathPrefixFor(VendorOpenAICompatible) != "/v1" {
		t.Errorf("custom prefix = %q, want /v1", OpenAIPathPrefixFor(VendorOpenAICompatible))
	}
}

func TestOpenAIPathPrefixForBespokeVendorsIsEmpty(t *testing.T) {
	for _, v := range []string{VendorOpenAI, VendorAnthropic, "unknown"} {
		if got := OpenAIPathPrefixFor(v); got != "" {
			t.Errorf("OpenAIPathPrefixFor(%q) = %q, want empty (⇒ /v1 default)", v, got)
		}
	}
}

func TestVendorPresetValidateVia(t *testing.T) {
	cases := map[string]string{
		VendorXAI:              "models",
		VendorGoogle:           "models",
		VendorOpenAICompatible: "models",
		VendorOpenRouter:       "key",
		VendorKilo:             "none",
	}
	for vendor, want := range cases {
		p, _ := VendorPresetFor(vendor)
		if p.ValidateVia != want {
			t.Errorf("%s ValidateVia = %q, want %q", vendor, p.ValidateVia, want)
		}
	}
	if p, _ := VendorPresetFor(VendorOpenRouter); p.ValidatePath != "/key" {
		t.Errorf("openrouter ValidatePath = %q, want /key", p.ValidatePath)
	}
	if p, _ := VendorPresetFor(VendorGoogle); p.StripModelsPrefix != "models/" {
		t.Errorf("google StripModelsPrefix = %q, want models/", p.StripModelsPrefix)
	}
}
