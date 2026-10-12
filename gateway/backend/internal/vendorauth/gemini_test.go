// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverGeminiModelsPagesFiltersStrips(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		if r.URL.Query().Get("pageToken") == "" {
			pages++
			_, _ = w.Write([]byte(`{"models":[
				{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","supportedGenerationMethods":["generateContent"]},
				{"name":"models/embedding-001","displayName":"Embedding","supportedGenerationMethods":["embedContent"]}],
				"nextPageToken":"p2"}`))
			return
		}
		pages++
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer srv.Close()
	models, status := DiscoverGeminiModels(context.Background(), srv.Client(), srv.URL, "k")
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want DiscoveryOK", status)
	}
	if pages != 2 {
		t.Fatalf("pages = %d, want 2 (followed nextPageToken)", pages)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.Slug] = true
	}
	if !ids["gemini-2.5-pro"] || !ids["gemini-2.5-flash"] || ids["embedding-001"] {
		t.Fatalf("models = %+v (want generateContent-only, models/ stripped)", models)
	}
}

func TestValidateGeminiAPIKeyOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	if check := ValidateGeminiAPIKey(context.Background(), srv.Client(), srv.URL, "k"); check.Status != StatusValid {
		t.Fatalf("status = %v (%s), want StatusValid", check.Status, check.Detail)
	}
}

// The request carries the page size, never the key (the key rides in the header
// only), and a display name is the vendor's own unless it merely repeats the
// prefixed resource name.
func TestDiscoverGeminiModelsRequestAndDisplayNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("pageSize"); got != "1000" {
			t.Errorf("pageSize = %q, want 1000", got)
		}
		if strings.Contains(r.URL.String(), "secret-key") {
			t.Errorf("the api key leaked into the request URL %q", r.URL)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/a","displayName":"Model A","supportedGenerationMethods":["generateContent","countTokens"]},
			{"name":"models/b","displayName":"models/b","supportedGenerationMethods":["generateContent"]},
			{"name":"models/c","supportedGenerationMethods":["generateContent"]},
			{"name":"bare","supportedGenerationMethods":["generateContent"]},
			{"name":"models/","supportedGenerationMethods":["generateContent"]},
			{"displayName":"nameless","supportedGenerationMethods":["generateContent"]},
			{"name":"models/no-methods"}]}`))
	}))
	defer srv.Close()

	models, status := DiscoverGeminiModels(context.Background(), srv.Client(), srv.URL+"/v1beta/models", "secret-key")
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want DiscoveryOK", status)
	}
	want := []DiscoveredModel{
		{Slug: "a", DisplayName: "Model A"},
		{Slug: "b", DisplayName: "b"},
		{Slug: "c", DisplayName: "c"},
		{Slug: "bare", DisplayName: "bare"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
}

// Anything short of a complete, non-empty listing is Unverifiable and returns no
// models: a failed first or later page, a listing with nothing chat-capable, a body
// that is not a catalog, and a listing that is still paging after the cap.
func TestDiscoverGeminiModelsUnverifiable(t *testing.T) {
	chat := `{"name":"models/m","supportedGenerationMethods":["generateContent"]}`
	cases := map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"bad request":  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
		"later page fails": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("pageToken") != "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"models":[` + chat + `],"nextPageToken":"p2"}`))
		},
		"no chat model": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/e","supportedGenerationMethods":["embedContent"]}]}`))
		},
		"empty list":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"not a catalog": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>nope</html>`)) },
		"endless paging": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"models":[` + chat + `],"nextPageToken":"again"}`))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			var requests int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				handler(w, r)
			}))
			defer srv.Close()

			models, status := DiscoverGeminiModels(context.Background(), srv.Client(), srv.URL, "k")
			if status != DiscoveryUnverifiable || models != nil {
				t.Fatalf("got %+v / %v, want nil / DiscoveryUnverifiable", models, status)
			}
			if name == "endless paging" && requests != maxGeminiModelPages {
				t.Fatalf("requests = %d, want the page cap %d", requests, maxGeminiModelPages)
			}
		})
	}
}

// The page token is URL-encoded into the follow-up request.
func TestDiscoverGeminiModelsEncodesThePageToken(t *testing.T) {
	const token = "a b&c=d/e+f"
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen = r.URL.Query().Get("pageToken"); seen == "" {
			_, _ = w.Write([]byte(`{"models":[],"nextPageToken":"` + token + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/m","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer srv.Close()

	if _, status := DiscoverGeminiModels(context.Background(), srv.Client(), srv.URL, "k"); status != DiscoveryOK {
		t.Fatalf("status = %v, want DiscoveryOK", status)
	}
	if seen != token {
		t.Fatalf("pageToken seen by the server = %q, want %q", seen, token)
	}
}

// Validation is the uniform rule (2xx valid, 401 invalid, anything else
// unverifiable) and its detail never echoes the key.
func TestValidateGeminiAPIKeyClassifies(t *testing.T) {
	cases := map[int]CredentialStatus{
		http.StatusOK:                  StatusValid,
		http.StatusUnauthorized:        StatusInvalid,
		http.StatusBadRequest:          StatusUnverifiable,
		http.StatusTooManyRequests:     StatusUnverifiable,
		http.StatusInternalServerError: StatusUnverifiable,
	}
	for code, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		check := ValidateGeminiAPIKey(context.Background(), srv.Client(), srv.URL, "secret-key")
		srv.Close()
		if check.Status != want {
			t.Errorf("HTTP %d: status = %v, want %v", code, check.Status, want)
		}
		if strings.Contains(check.Detail, "secret-key") {
			t.Errorf("HTTP %d: detail %q leaks the key", code, check.Detail)
		}
	}
}
