// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sdcppImageCases is the verdict table for the stable-diffusion.cpp
// capability document. It is duplicated VERBATIM in
// gateway/backend/internal/provider/sdcpp_capabilities_test.go, because the
// two Go modules share no package and each keeps its own copy of the rule
// (parseSdcppCapabilities there, parseSdcppCapabilities here). The two
// copies must decide identically on identical input: if a case fails, fix
// the parser, not this table, and change both tables together. The gateway's
// TestSdcppImageCasesMatchTheAgentsTable fails when the two declarations
// differ by a byte.
var sdcppImageCases = []struct {
	name      string
	body      string
	wantImage string // "yes" | "no" | ""
	wantErr   bool
}{
	{"img_gen listed", `{"supported_modes":["img_gen"]}`, "yes", false},
	{"img_gen among others", `{"supported_modes":["vid_gen","img_gen"]}`, "yes", false},
	{"img_gen padded", `{"supported_modes":[" img_gen "]}`, "yes", false},
	{"exhaustive list without img_gen", `{"supported_modes":["vid_gen"]}`, "no", false},
	{"case variant is not img_gen", `{"supported_modes":["IMG_GEN"]}`, "no", false},
	{"longer mode is not img_gen", `{"supported_modes":["img_gen_edit"]}`, "no", false},
	{"empty list is exhaustive", `{"supported_modes":[]}`, "no", false},
	{"absent list is no answer", `{"model":{"stem":"flux1-dev"}}`, "", false},
	{"null list is no answer", `{"supported_modes":null}`, "", false},
	{"unrelated fields ignored", `{"supported_modes":["img_gen"],"limits":{"max":4},"output_formats":["png"]}`, "yes", false},
	{"list of the wrong type", `{"supported_modes":"img_gen"}`, "", true},
	{"non-string entry", `{"supported_modes":[1]}`, "", true},
	{"model of the wrong type", `{"supported_modes":["img_gen"],"model":"flux1-dev"}`, "", true},
	{"stem of the wrong type", `{"supported_modes":["img_gen"],"model":{"stem":1}}`, "", true},
	{"not JSON", `<html>502</html>`, "", true},
}

func TestParseSdcppCapabilitiesTwinTable(t *testing.T) {
	for _, tc := range sdcppImageCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSdcppCapabilities([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got.image != tc.wantImage {
				t.Fatalf("image = %q, want %q", got.image, tc.wantImage)
			}
		})
	}
}

// TestProbeSdcppVerdicts pins the stable/transient split. It is the SAME
// contract as ProbePropsVerdicts and ProbeOllamaVerdicts: a readable
// document, or a status that is a fixed property of the binary (404, 401,
// 403, 405), is conclusive and may be cached for the pid's lifetime.
// Anything else (5xx above all, no response, a body that is not JSON) is
// transient and must be retried.
func TestProbeSdcppVerdicts(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The literal, not SdcppCapabilitiesPath: comparing the constant
			// with itself would not notice the constant drifting away from
			// the path a real sd-server serves.
			if r.URL.Path != "/sdcpp/v1/capabilities" || r.Method != http.MethodGet {
				t.Errorf("request %s %s, want GET /sdcpp/v1/capabilities", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	cases := []struct {
		name       string
		status     int
		body       string
		wantImage  string
		wantStable bool
	}{
		{"yes", 200, `{"supported_modes":["img_gen"]}`, "yes", true},
		{"no", 200, `{"supported_modes":["vid_gen"]}`, "no", true},
		{"absent list", 200, `{"model":{"stem":"x"}}`, "", true},
		{"wrong shape is conclusive", 200, `{"supported_modes":"img_gen"}`, "", true},
		{"not JSON is transient", 200, `{"supported_modes":[`, "", false},
		{"404 is conclusive", 404, ``, "", true},
		{"401 is conclusive", 401, ``, "", true},
		{"403 is conclusive", 403, ``, "", true},
		{"405 is conclusive", 405, ``, "", true},
		{"503 is transient", 503, `{"supported_modes":["img_gen"]}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.body)
			defer srv.Close()
			got, stable := ProbeSdcppVerdicts(context.Background(), srv.Client(), srv.URL)
			if stable != tc.wantStable || got.Caps.Image != tc.wantImage || got.LiveProgress != "" {
				t.Fatalf("got image=%q live=%q stable=%v, want image=%q live=\"\" stable=%v",
					got.Caps.Image, got.LiveProgress, stable, tc.wantImage, tc.wantStable)
			}
		})
	}

	t.Run("no response is transient", func(t *testing.T) {
		srv := serve(200, `{}`)
		url := srv.URL
		srv.Close()
		if _, stable := ProbeSdcppVerdicts(context.Background(), http.DefaultClient, url); stable {
			t.Fatal("stable = true for a closed port, want false")
		}
	})
}
