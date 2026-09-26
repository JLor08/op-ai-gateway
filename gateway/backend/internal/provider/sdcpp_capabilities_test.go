// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestProbeSdcppCapabilities uses the real capability document. supported_modes
// is EXHAUSTIVE -- the server lists every mode it serves -- which is what makes
// this source bidirectional, unlike ollama_api_show, whose own doc records that
// its array is not exhaustive and so can only ever produce "yes" or nothing.
func TestProbeSdcppCapabilities(t *testing.T) {
	const doc = `{"current_mode":"img_gen","model":{"name":"flux1-dev.safetensors","stem":"flux1-dev"},"supported_modes":["img_gen"],"output_formats":["png","jpeg","webp"]}`

	t.Run("img_gen present yields yes and the model identity", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/sdcpp/v1/capabilities" {
				t.Errorf("path = %q", r.URL.Path)
			}
			_, _ = w.Write([]byte(doc))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got.Image != routing.CapabilityYes || got.ModelStem != "flux1-dev" {
			t.Fatalf("got %+v, want {Image:yes ModelStem:flux1-dev}", got)
		}
	})

	t.Run("img_gen absent yields no", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"supported_modes":["vid_gen"],"model":{"stem":"x"}}`))
		}))
		defer srv.Close()
		got, _ := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if got.Image != routing.CapabilityNo {
			t.Fatalf("Image = %q, want %q -- an exhaustive list without img_gen is a NO, not an absence", got.Image, routing.CapabilityNo)
		}
	})

	t.Run("no document yields no verdict at all", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if err == nil {
			t.Fatal("a 404 must be an error, so no row is written")
		}
		if got.Image != "" {
			t.Fatalf("Image = %q, want empty -- an unreachable probe must not state a verdict", got.Image)
		}
	})

	t.Run("missing supported_modes yields no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":{"stem":"x"}}`))
		}))
		defer srv.Close()
		got, _ := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if got.Image != "" {
			t.Fatalf("Image = %q, want empty -- an absent list is not an exhaustive one", got.Image)
		}
	})
}

// TestProbeSdcppCapabilitiesEdges pins the rest of the probe's contract, beyond
// the four answers above: what counts as unreadable, what an empty list means,
// and that the request carries the application's credential like every other
// probe on this client.
func TestProbeSdcppCapabilitiesEdges(t *testing.T) {
	serve := func(t *testing.T, body string) SdcppVerdicts {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("error = %v, want nil for %s", err, body)
		}
		return got
	}

	t.Run("a body that is not JSON is an error and no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not the document</html>"))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("error = %v, want ErrInvalidResponse -- an unreadable body must write no row", err)
		}
		if got.Image != "" {
			t.Fatalf("Image = %q, want empty", got.Image)
		}
	})

	t.Run("a non-2xx status is an error even when the body is a valid document", func(t *testing.T) {
		// The body alone would decode to a yes, so only the status check stands
		// between a failing server and a verdict: the 404 subtest above sends
		// an empty body, which fails decoding anyway and so cannot tell.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"supported_modes":["img_gen"],"model":{"stem":"flux1-dev"}}`))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable for a 503", err)
		}
		if got != (SdcppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict -- a failing server's body is not its answer", got)
		}
	})

	t.Run("a supported_modes of the wrong type is an error, not a no", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"supported_modes":"img_gen"}`))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(context.Background(), routing.Target{Endpoint: srv.URL})
		if err == nil || got.Image != "" {
			t.Fatalf("got %+v, err %v; want an error and no verdict -- a list this code cannot read must not read as one without img_gen", got, err)
		}
	})

	t.Run("an empty list is still exhaustive, so a no", func(t *testing.T) {
		if got := serve(t, `{"supported_modes":[]}`); got.Image != routing.CapabilityNo {
			t.Fatalf("Image = %q, want %q", got.Image, routing.CapabilityNo)
		}
	})

	t.Run("a null list is an absent one, so no verdict", func(t *testing.T) {
		if got := serve(t, `{"supported_modes":null,"model":{"stem":"x"}}`); got.Image != "" || got.ModelStem != "x" {
			t.Fatalf("got %+v, want {Image: ModelStem:x}", got)
		}
	})

	t.Run("img_gen beside other modes is a yes, with no model named", func(t *testing.T) {
		if got := serve(t, `{"supported_modes":["vid_gen","img_gen"]}`); got.Image != routing.CapabilityYes || got.ModelStem != "" {
			t.Fatalf("got %+v, want {Image:yes ModelStem:}", got)
		}
	})

	t.Run("the application's credential is attached", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(`{"supported_modes":["img_gen"]}`))
		}))
		defer srv.Close()
		ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "sd-secret")
		if _, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppCapabilities(ctx, routing.Target{Endpoint: srv.URL}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if gotAuth != "sd-secret" {
			t.Fatalf("X-Api-Key = %q, want sd-secret", gotAuth)
		}
	})
}
