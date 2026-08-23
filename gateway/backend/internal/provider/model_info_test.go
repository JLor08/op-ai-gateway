// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestParseModelInfoProps(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []ModelInfo
	}{
		{"nested n_ctx + model", `{"model":"deepseek-v3","default_generation_settings":{"n_ctx":131072}}`, []ModelInfo{{Name: "deepseek-v3", ContextSize: 131072}}},
		{"top-level n_ctx + model_path", `{"model_path":"/models/qwen-coder.gguf","n_ctx":32768}`, []ModelInfo{{Name: "qwen-coder.gguf", ContextSize: 32768}}},
		{"n_ctx as float", `{"model":"m","default_generation_settings":{"n_ctx":8192.0}}`, []ModelInfo{{Name: "m", ContextSize: 8192}}},
		{"no ctx -> name only (ctx 0)", `{"model":"m"}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"no model -> empty", `{"default_generation_settings":{"n_ctx":123}}`, nil},
		{"garbage -> empty", `not json`, nil},
		{"nested wins over top-level", `{"model":"m","n_ctx":4096,"default_generation_settings":{"n_ctx":9000}}`, []ModelInfo{{Name: "m", ContextSize: 9000}}},
		{"string n_ctx not coerced", `{"model":"m","default_generation_settings":{"n_ctx":"131072"}}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"dgs not an object -> falls through", `{"model":"m","default_generation_settings":"nope"}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"negative n_ctx -> 0", `{"model":"m","default_generation_settings":{"n_ctx":-5}}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"top-level array -> empty", `[1,2,3]`, nil},
	}
	for _, tc := range cases {
		got := parseModelInfo([]byte(tc.body))
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %+v, want %+v", tc.name, got[i], tc.want[i])
			}
		}
	}
}

func TestProbeModelInfoClient(t *testing.T) {
	t.Run("GETs the probe path and parses props", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"up","default_generation_settings":{"n_ctx":131072}}`))
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeModelInfo(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "/props")
		if err != nil {
			t.Fatalf("ProbeModelInfo err: %v", err)
		}
		if gotPath != "/props" {
			t.Fatalf("GET path = %q, want /props", gotPath)
		}
		want := ModelInfo{Name: "up", ContextSize: 131072}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("ProbeModelInfo = %+v, want [%+v]", got, want)
		}
	})

	t.Run("blank probe path is a no-op with no HTTP request", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeModelInfo(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "")
		if err != nil || got != nil {
			t.Fatalf("blank probe path = (%v, %v), want (nil, nil)", got, err)
		}
		if hits != 0 {
			t.Fatalf("blank probe path made %d HTTP request(s), want 0", hits)
		}
	})
}

func TestPickModelContextSize(t *testing.T) {
	cases := []struct {
		name  string
		infos []ModelInfo
		model string
		want  int
	}{
		{"name-match wins even when a different info precedes", []ModelInfo{{Name: "other", ContextSize: 1}, {Name: "m", ContextSize: 8192}}, "m", 8192},
		{"first-positive fallback when no name matches", []ModelInfo{{Name: "basename", ContextSize: 4096}}, "m", 4096},
		{"skips non-positive", []ModelInfo{{Name: "m", ContextSize: 0}, {Name: "x", ContextSize: 2048}}, "m", 2048},
		{"empty -> 0", nil, "m", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickModelContextSize(tc.infos, tc.model); got != tc.want {
				t.Fatalf("PickModelContextSize(%+v, %q) = %d, want %d", tc.infos, tc.model, got, tc.want)
			}
		})
	}
}

func TestExpandModelPath(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		model string
		want  string
	}{
		{"simple substitution", "/upstream/{model}/props", "llama-7b", "/upstream/llama-7b/props"},
		{"slash in name stays multi-segment", "/u/{model}/props", "openai/gpt-4o", "/u/openai/gpt-4o/props"},
		{"space is escaped", "/u/{model}/props", "my model", "/u/my%20model/props"},
		{"no placeholder returned unchanged", "/props", "x", "/props"},
		{"repeated placeholder", "/{model}/{model}", "m", "/m/m"},
		{"unrelated brace left literal", "/{other}/props", "m", "/{other}/props"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandModelPath(tc.path, tc.model); got != tc.want {
				t.Fatalf("ExpandModelPath(%q, %q) = %q, want %q", tc.path, tc.model, got, tc.want)
			}
		})
	}
}
