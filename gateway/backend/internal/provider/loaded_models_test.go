// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sort"
	"testing"
)

func TestParseLoadedModels(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		format string
		want   []string
	}{
		{"openai data ids", `{"data":[{"id":"m1"},{"id":"m2"}]}`, "openai", []string{"m1", "m2"}},
		{"llama_swap running objects", `{"running":[{"model":"a"},{"model":"b"}]}`, "llama_swap", []string{"a", "b"}},
		{"llama_swap models strings", `{"models":["x","y"]}`, "llama_swap", []string{"x", "y"}},
		{"llama_swap bare array", `["one","two"]`, "llama_swap", []string{"one", "two"}},
		{"llama_cpp model", `{"model":"qwen"}`, "llama_cpp", []string{"qwen"}},
		{"llama_cpp model_path basename", `{"model_path":"/models/qwen-7b.gguf"}`, "llama_cpp", []string{"qwen-7b.gguf"}},
		{"auto unions openai+swap", `{"data":[{"id":"d"}],"running":["r"]}`, "auto", []string{"d", "r"}},
		{"auto empty on garbage", `not json`, "auto", nil},
		{"auto empty object", `{}`, "", nil},
		{"dedup", `{"data":[{"id":"dup"},{"id":"dup"}]}`, "openai", []string{"dup"}},
		{"skips empties", `{"data":[{"id":""},{"id":"keep"}]}`, "openai", []string{"keep"}},
		{"unknown format falls back to union", `{"model":"cpp"}`, "totally-unknown", []string{"cpp"}},
		{"litellm healthy excludes unhealthy", `{"healthy_endpoints":[{"model":"a"},{"model":"b"}],"unhealthy_endpoints":[{"model":"c"}]}`, "litellm", []string{"a", "b"}},
		{"litellm under auto union", `{"healthy_endpoints":[{"model":"a"},{"model":"b"}],"unhealthy_endpoints":[{"model":"c"}]}`, "auto", []string{"a", "b"}},
		{"litellm wrong shape is tolerant nil", `{"data":[{"id":"x"}]}`, "litellm", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLoadedModels([]byte(tc.body), tc.format)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parseLoadedModels(%q, %q) = %v, want %v", tc.body, tc.format, got, want)
			}
		})
	}
}

func TestFetchLoadedModels(t *testing.T) {
	t.Run("blank status path is a no-op", func(t *testing.T) {
		got, err := fetchLoadedModels(context.Background(), http.DefaultClient, routing.Target{Endpoint: "http://unused"}, "", "auto")
		if err != nil || got != nil {
			t.Fatalf("blank path = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("GETs the status path and parses", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"running":["loaded-a","loaded-b"]}`))
		}))
		defer srv.Close()
		got, err := fetchLoadedModels(context.Background(), srv.Client(), routing.Target{Endpoint: srv.URL}, "running", "llama_swap")
		if err != nil {
			t.Fatalf("fetchLoadedModels err: %v", err)
		}
		if gotPath != "/running" {
			t.Fatalf("GET path = %q, want /running", gotPath)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"loaded-a", "loaded-b"}) {
			t.Fatalf("models = %v", got)
		}
	})

	t.Run("non-2xx is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		if _, err := fetchLoadedModels(context.Background(), srv.Client(), routing.Target{Endpoint: srv.URL}, "/status", "auto"); err == nil {
			t.Fatal("expected an error on 503")
		}
	})
}
