// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

func TestLoadedModelListerUnavailableWhenNoURL(t *testing.T) {
	l := NewLoadedModelLister("", "auto", nil)
	if l.Available() {
		t.Fatal("empty URL must be unavailable")
	}
}

func TestLoadedModelListerCollect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"running":[{"model":"a"},{"model":"b"}]}`))
	}))
	defer srv.Close()

	l := NewLoadedModelLister(srv.URL, "llama_swap", srv.Client())
	if !l.Available() {
		t.Fatal("configured URL must be available")
	}
	got, err := l.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Collect = %v, want [a b]", got)
	}
}

func TestLoadedModelListerCollectNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	l := NewLoadedModelLister(srv.URL, "auto", srv.Client())
	if _, err := l.Collect(context.Background()); err == nil {
		t.Fatal("expected an error on 500")
	}
}

func TestParseLoadedModelsAgentSide(t *testing.T) {
	cases := []struct {
		body, format string
		want         []string
	}{
		{`{"data":[{"id":"x"}]}`, "openai", []string{"x"}},
		{`{"model_path":"/m/qwen.gguf"}`, "llama_cpp", []string{"qwen.gguf"}},
		{`{"data":[{"id":"d"}],"running":["r"]}`, "auto", []string{"d", "r"}},
		{`{"healthy_endpoints":[{"model":"x"}]}`, "litellm", []string{"x"}},
		{`{"healthy_endpoints":[{"model":"a"}],"unhealthy_endpoints":[{"model":"b"}]}`, "litellm", []string{"a"}},
		{`garbage`, "auto", nil},
	}
	for _, tc := range cases {
		got := parseLoadedModels([]byte(tc.body), tc.format)
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parse(%q,%q) = %v, want %v", tc.body, tc.format, got, want)
		}
	}
}
