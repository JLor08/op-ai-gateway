// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestSpecUpstreamAuthOffOverridesAppToken(t *testing.T) {
	spec := RuntimeSpec{APITokenMode: string(RuntimeAPITokenModeOff)}
	app := Application{APIToken: "enc:app", APITokenHeader: "Authorization"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != "" || hdr != "" {
		t.Fatalf("SpecUpstreamAuth(off) = (%q, %q), want (\"\", \"\") even though app has a token", tok, hdr)
	}
}

func TestSpecUpstreamAuthAppMode(t *testing.T) {
	spec := RuntimeSpec{APITokenMode: string(RuntimeAPITokenModeApp)}
	app := Application{APIToken: "enc:app", APITokenHeader: "Authorization"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != "enc:app" || hdr != "Authorization" {
		t.Fatalf("SpecUpstreamAuth(app) = (%q, %q), want (\"enc:app\", \"Authorization\")", tok, hdr)
	}
}

func TestSpecUpstreamAuthSetModeCustomHeader(t *testing.T) {
	spec := RuntimeSpec{
		APITokenMode:         string(RuntimeAPITokenModeSet),
		APIToken:             "enc:spec",
		APITokenHeaderSource: string(RuntimeAPITokenHeaderSourceCustom),
		APITokenHeader:       "X-Api-Key",
	}
	app := Application{APIToken: "enc:app", APITokenHeader: "Authorization"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != "enc:spec" || hdr != "X-Api-Key" {
		t.Fatalf("SpecUpstreamAuth(set, custom header) = (%q, %q), want (\"enc:spec\", \"X-Api-Key\")", tok, hdr)
	}
}

func TestSpecUpstreamAuthSetModeAppHeader(t *testing.T) {
	spec := RuntimeSpec{
		APITokenMode:         string(RuntimeAPITokenModeSet),
		APIToken:             "enc:spec",
		APITokenHeaderSource: string(RuntimeAPITokenHeaderSourceApp),
	}
	app := Application{APIToken: "enc:app", APITokenHeader: "X-App"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != "enc:spec" || hdr != "X-App" {
		t.Fatalf("SpecUpstreamAuth(set, app header) = (%q, %q), want (\"enc:spec\", \"X-App\")", tok, hdr)
	}
}

func TestSpecUpstreamAuthRandomModeBehavesLikeSet(t *testing.T) {
	spec := RuntimeSpec{APITokenMode: string(RuntimeAPITokenModeRandom), APIToken: "enc:rand"}
	app := Application{APIToken: "enc:app", APITokenHeader: "Authorization"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != "enc:rand" || hdr != "Authorization" {
		t.Fatalf("SpecUpstreamAuth(random) = (%q, %q), want (\"enc:rand\", \"Authorization\")", tok, hdr)
	}
}

func TestSpecUpstreamAuthEmptyModeFallsBackToApp(t *testing.T) {
	var spec RuntimeSpec // zero value: empty APITokenMode
	app := Application{APIToken: "enc:app", APITokenHeader: "Authorization"}
	tok, hdr := SpecUpstreamAuth(spec, app)
	if tok != app.APIToken || hdr != app.APITokenHeader {
		t.Fatalf("SpecUpstreamAuth(zero spec) = (%q, %q), want app fallback (%q, %q)", tok, hdr, app.APIToken, app.APITokenHeader)
	}
}
