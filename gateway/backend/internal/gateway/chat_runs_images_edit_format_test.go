// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/portal"
	"testing"
)

// The edit route of sd-server produces png or jpeg only, so an output format it
// cannot produce is left out of an EDIT's form -- the upstream then answers in its
// default instead of refusing the whole edit with a generic upstream error. The
// thread's setting is untouched (the same thread's generations still honor it,
// see TestImageRunWithoutASourceStillPostsGenerations), and the compression that
// belonged to the dropped format goes with it.
func TestImageRunEditOmitsAnOutputFormatTheEditRouteCannotProduce(t *testing.T) {
	compression := 80
	for _, tc := range []struct {
		name        string
		format      string
		compression *int
		wantFormat  string // "" = the field must be absent
		wantComp    string // "" = the field must be absent
	}{
		{"webp is dropped, with its compression", "webp", &compression, "", ""},
		{"jpeg is kept, with its compression", "jpeg", &compression, "jpeg", "80"},
		{"png is kept", "png", nil, "png", ""},
		{"no format stays no format", "", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			prep := editRunPrep("make it blue", imageDataURL("image/png", editSourceBytes(200, 6)))
			prep.Settings.Image = &portal.ChatImageSettings{Size: "512x512", OutputFormat: tc.format, OutputCompression: tc.compression}

			run, got, ok := runToTerminal(t, srv, owner, chatID, prep, seen)
			if run.statusValue() != "completed" || !ok {
				t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
			}
			if got.path != "/v1/images/edits" {
				t.Fatalf("upstream path = %q, want the edit", got.path)
			}
			parts := parseEditBody(t, got)
			for name, want := range map[string]string{"output_format": tc.wantFormat, "output_compression": tc.wantComp} {
				v, present := editField(t, parts, name)
				if want == "" && present {
					t.Fatalf("field %q = %q was sent, want it absent", name, v)
				}
				if want != "" && (!present || v != want) {
					t.Fatalf("field %q = %q (present %v), want %q", name, v, present, want)
				}
			}
			// The rest of the thread's settings are not touched by it.
			if v, _ := editField(t, parts, "size"); v != "512x512" {
				t.Fatalf("size = %q, want it kept", v)
			}
		})
	}
}
