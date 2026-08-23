// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestOpenAICompatibleUnloadModel(t *testing.T) {
	t.Run("2xx from llama-swap unload endpoint => unloaded", func(t *testing.T) {
		var (
			gotMethod string
			gotPath   string
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "cold-model")
		if err != nil {
			t.Fatalf("UnloadModel err: %v", err)
		}
		if !unloaded {
			t.Fatal("want unloaded=true on 200")
		}
		if gotMethod != http.MethodPost {
			t.Fatalf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/api/models/unload/cold-model" {
			t.Fatalf("path = %q, want /api/models/unload/cold-model", gotPath)
		}
	})

	t.Run("escapes the model name in the path", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		if _, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "org/model 7b"); err != nil {
			t.Fatalf("UnloadModel err: %v", err)
		}
		if want := "/api/models/unload/org%2Fmodel%207b"; gotPath != want {
			t.Fatalf("escaped path = %q, want %q", gotPath, want)
		}
	})

	t.Run("404 => not supported (false, nil)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
		if err != nil || unloaded {
			t.Fatalf("404 = (%v, %v), want (false, nil)", unloaded, err)
		}
	})

	t.Run("500 => not unloaded (false, nil)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
		if err != nil || unloaded {
			t.Fatalf("500 = (%v, %v), want (false, nil)", unloaded, err)
		}
	})

	t.Run("empty model is a no-op (false, nil), no request", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "   ")
		if err != nil || unloaded {
			t.Fatalf("empty model = (%v, %v), want (false, nil)", unloaded, err)
		}
		if called {
			t.Fatal("empty model must not issue a request")
		}
	})

	t.Run("transport error is surfaced", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		client := srv.Client()
		endpoint := srv.URL
		srv.Close() // now unreachable

		c := NewOpenAICompatibleClient(client)
		if _, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: endpoint}, "m"); err == nil {
			t.Fatal("expected a transport error against a closed endpoint")
		}
	})

	t.Run("target.Timeout bounds a wedged upstream (no hang)", func(t *testing.T) {
		// Handler never responds; it returns only on client-disconnect or a bounded fallback,
		// so srv.Close() can never deadlock regardless of whether the client's abort is
		// observed server-side. The client-side deadline (100ms) must bound UnloadModel.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		defer srv.Close()

		c := NewOpenAICompatibleClient(srv.Client())
		done := make(chan error, 1)
		go func() {
			_, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL, Timeout: 100 * time.Millisecond}, "m")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("want a deadline error from the bounded unload, got nil")
			}
		case <-time.After(1500 * time.Millisecond):
			t.Fatal("UnloadModel not bounded by target.Timeout: still running well past the 100ms deadline")
		}
	})
}

func TestOllamaUnloadModel(t *testing.T) {
	t.Run("POST /api/generate with keep_alive:0", func(t *testing.T) {
		var (
			gotMethod string
			gotPath   string
			gotBody   map[string]any
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &gotBody)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewOllamaClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "llama3")
		if err != nil {
			t.Fatalf("UnloadModel err: %v", err)
		}
		if !unloaded {
			t.Fatal("want unloaded=true on 200")
		}
		if gotMethod != http.MethodPost {
			t.Fatalf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/api/generate" {
			t.Fatalf("path = %q, want /api/generate", gotPath)
		}
		if gotBody["model"] != "llama3" {
			t.Fatalf("body model = %v, want llama3", gotBody["model"])
		}
		if ka, ok := gotBody["keep_alive"].(float64); !ok || ka != 0 {
			t.Fatalf("body keep_alive = %v, want 0", gotBody["keep_alive"])
		}
	})

	t.Run("empty model is a no-op (false, nil)", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewOllamaClient(srv.Client())
		unloaded, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL}, "")
		if err != nil || unloaded {
			t.Fatalf("empty model = (%v, %v), want (false, nil)", unloaded, err)
		}
		if called {
			t.Fatal("empty model must not issue a request")
		}
	})

	t.Run("target.Timeout bounds a wedged upstream (no hang)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		defer srv.Close()

		c := NewOllamaClient(srv.Client())
		done := make(chan error, 1)
		go func() {
			_, err := c.UnloadModel(context.Background(), routing.Target{Endpoint: srv.URL, Timeout: 100 * time.Millisecond}, "llama3")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("want a deadline error from the bounded unload, got nil")
			}
		case <-time.After(1500 * time.Millisecond):
			t.Fatal("UnloadModel not bounded by target.Timeout: still running well past the 100ms deadline")
		}
	})
}

func TestMultiplexerUnloadModel(t *testing.T) {
	t.Run("client without ModelUnloader yields (false, nil)", func(t *testing.T) {
		// Mock does not implement ModelUnloader.
		mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)
		unloaded, err := mux.UnloadModel(context.Background(), routing.Target{Provider: routing.ProviderMock}, "m")
		if err != nil || unloaded {
			t.Fatalf("non-unloader = (%v, %v), want (false, nil)", unloaded, err)
		}
	})

	t.Run("nil multiplexer => ErrUnavailable", func(t *testing.T) {
		var mux *Multiplexer
		if _, err := mux.UnloadModel(context.Background(), routing.Target{Provider: routing.ProviderMock}, "m"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("nil mux err = %v, want ErrUnavailable", err)
		}
	})

	t.Run("routes to a client that implements ModelUnloader", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		mux := NewMultiplexer(map[string]Client{routing.ProviderVLLM: NewOpenAICompatibleClient(srv.Client())}, nil)
		unloaded, err := mux.UnloadModel(context.Background(), routing.Target{Provider: routing.ProviderVLLM, Endpoint: srv.URL}, "routed-model")
		if err != nil {
			t.Fatalf("UnloadModel err: %v", err)
		}
		if !unloaded {
			t.Fatal("want unloaded=true")
		}
		if gotPath != "/api/models/unload/routed-model" {
			t.Fatalf("path = %q, want /api/models/unload/routed-model", gotPath)
		}
	})
}
