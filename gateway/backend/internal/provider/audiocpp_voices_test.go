// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/tracing"
	"reflect"
	"testing"
)

// audioCppLiveVoices is the shape audio.cpp v0.9.1 really answers on
// GET /v1/audio/voices (live-verified): a flat array of voice-name strings for the
// currently loaded model, with no per-voice metadata.
const audioCppLiveVoices = `{"voices":["demo_1_man","demo_2_man","demo_3_woman","demo_4_woman"]}`

func TestParseAudioCppVoices(t *testing.T) {
	// A valid listing -- even one that lists no voices -- is a success; a body
	// that is not a decodable listing is an error and carries no voices, so a
	// garbage body can never read as "the model has no voices".
	cases := []struct {
		name    string
		body    string
		want    []Voice
		wantErr bool
	}{
		{"the live flat name list", audioCppLiveVoices, []Voice{{Name: "demo_1_man"}, {Name: "demo_2_man"}, {Name: "demo_3_woman"}, {Name: "demo_4_woman"}}, false},
		{"two names", `{"voices":["a","b"]}`, []Voice{{Name: "a"}, {Name: "b"}}, false},
		{"an empty list", `{"voices":[]}`, []Voice{}, false},
		{"an absent voices field", `{}`, []Voice{}, false},
		{"a null voices field", `{"voices":null}`, []Voice{}, false},
		{"an unknown extra field is ignored", `{"model":"m","voices":["a"]}`, []Voice{{Name: "a"}}, false},
		{"a blank name is dropped", `{"voices":["a","","  ","b"]}`, []Voice{{Name: "a"}, {Name: "b"}}, false},
		{"an HTML body", `<html>not the listing</html>`, nil, true},
		{"an empty body", ``, nil, true},
		{"a truncated body", `{"voices":["a","b`, nil, true},
		{"a voices field of the wrong type", `{"voices":"demo_1_man"}`, nil, true},
		{"an entry of the wrong type", `{"voices":["a",7,"b"]}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAudioCppVoices([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAudioCppVoices(%s) = %+v, nil; want an error", tc.body, got)
				}
				if got != nil {
					t.Fatalf("parseAudioCppVoices(%s) = %+v with an error; want no voices", tc.body, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAudioCppVoices(%s) error = %v, want nil", tc.body, err)
			}
			if got == nil {
				t.Fatalf("parseAudioCppVoices(%s) = nil, want a non-nil slice", tc.body)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseAudioCppVoices(%s) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestListVoices(t *testing.T) {
	list := func(t *testing.T, srv *httptest.Server, ctx context.Context, model string) ([]Voice, error) {
		t.Helper()
		return NewOpenAICompatibleClient(srv.Client()).ListVoices(ctx, routing.Target{Endpoint: srv.URL}, model)
	}

	t.Run("lists the names from GET /v1/audio/voices", func(t *testing.T) {
		var method, path, rawQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path, rawQuery = r.Method, r.URL.Path, r.URL.RawQuery
			_, _ = w.Write([]byte(audioCppLiveVoices))
		}))
		defer srv.Close()
		got, err := list(t, srv, context.Background(), "audio8-tts")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := []Voice{{Name: "demo_1_man"}, {Name: "demo_2_man"}, {Name: "demo_3_woman"}, {Name: "demo_4_woman"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		if method != http.MethodGet || path != "/v1/audio/voices" {
			t.Fatalf("request = %s %s, want GET /v1/audio/voices", method, path)
		}
		if rawQuery != "" {
			t.Fatalf("request query = %q, want none: audio.cpp ignores ?model=, so the model is never sent upstream", rawQuery)
		}
	})

	t.Run("a valid empty listing is a success with an empty non-nil slice", func(t *testing.T) {
		for _, body := range []string{`{"voices":[]}`, `{}`, `{"voices":null}`} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			got, err := list(t, srv, context.Background(), "m")
			srv.Close()
			if err != nil {
				t.Fatalf("body %s: error = %v, want nil -- a genuine empty listing is a success", body, err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("body %s: got %#v, want an empty non-nil slice", body, got)
			}
		}
	})

	t.Run("a non-2xx status is an ErrUnavailable and no voices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(audioCppLiveVoices))
		}))
		defer srv.Close()
		got, err := list(t, srv, context.Background(), "m")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable for a 503", err)
		}
		if got != nil {
			t.Fatalf("got %+v, want nil -- a failing server's body is not its answer", got)
		}
	})

	t.Run("a transport failure is an ErrUnavailable and no voices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := NewOpenAICompatibleClient(srv.Client())
		endpoint := srv.URL
		srv.Close() // nothing listens any more
		got, err := client.ListVoices(context.Background(), routing.Target{Endpoint: endpoint}, "m")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("a 2xx body that is not a decodable listing is an ErrUnavailable and no voices", func(t *testing.T) {
		for _, body := range []string{"<html>not the listing</html>", `{"voices":["a","b`, `{"voices":"demo_1_man"}`, ``} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			got, err := list(t, srv, context.Background(), "m")
			srv.Close()
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("body %q: error = %v, want ErrUnavailable -- an undecodable body must not read as \"the model has no voices\"", body, err)
			}
			if got != nil {
				t.Fatalf("body %q: got %+v, want nil", body, got)
			}
		}
	})

	t.Run("the application's credential is attached", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(audioCppLiveVoices))
		}))
		defer srv.Close()
		ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "audio-secret")
		if _, err := list(t, srv, ctx, "m"); err != nil {
			t.Fatalf("error = %v", err)
		}
		if gotAuth != "audio-secret" {
			t.Fatalf("X-Api-Key = %q, want audio-secret", gotAuth)
		}
	})
}

// TestMultiplexerDispatchesAudioCppVoicesToOpenAICompatibleClient mirrors the
// audio.cpp models dispatch test: a target whose provider maps to the shared
// OpenAI-compatible client reaches its ListVoices, inside a provider.ListVoices
// span, while a provider whose client lacks the capability answers
// ErrUnavailable and sends nothing (no cross-route).
func TestMultiplexerDispatchesAudioCppVoicesToOpenAICompatibleClient(t *testing.T) {
	logs := logbuffer.NewBuffer(20, logbuffer.LevelTrace)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	defer slog.SetDefault(prev)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(audioCppLiveVoices))
	}))
	defer srv.Close()
	shared := NewOpenAICompatibleClient(srv.Client())
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:     shared,
		routing.ProviderAudioCpp: shared, // same instance, exactly as providerClients wires it
		routing.ProviderOllama:   &recordingClient{},
	}, nil)

	got, err := mux.ListVoices(context.Background(), routing.Target{Provider: routing.ProviderAudioCpp, Endpoint: srv.URL}, "audio8-tts")
	if err != nil {
		t.Fatalf("ListVoices(audio_cpp) returned %v", err)
	}
	want := []Voice{{Name: "demo_1_man"}, {Name: "demo_2_man"}, {Name: "demo_3_woman"}, {Name: "demo_4_woman"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(paths) != 1 || paths[0] != "/v1/audio/voices" {
		t.Fatalf("upstream paths = %v, want exactly [/v1/audio/voices]", paths)
	}

	var found bool
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" && r.Attrs["span"] == "provider.ListVoices" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider.ListVoices span not emitted: %+v", logs.Snapshot())
	}

	got, err = mux.ListVoices(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}, "audio8-tts")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ListVoices(ollama) error = %v, want ErrUnavailable -- a client without the capability has no voices", err)
	}
	if got != nil {
		t.Fatalf("ListVoices(ollama) = %+v, want nil", got)
	}
	if len(paths) != 1 {
		t.Fatalf("upstream paths = %v, want no request for a client without the capability", paths)
	}
}

// TestMultiplexerAudioCppVoicesWithoutListerIsUnavailable pins the bare posture:
// with no client that implements AudioCppVoicesLister -- neither the provider's
// nor a fallback -- the multiplexer answers ErrUnavailable. An empty list with a
// nil error would read as a successful listing that found no voices.
func TestMultiplexerAudioCppVoicesWithoutListerIsUnavailable(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)

	got, err := mux.ListVoices(context.Background(), routing.Target{Provider: routing.ProviderMock, Endpoint: "http://x:8000"}, "m")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}

	var nilMux *Multiplexer
	if _, err := nilMux.ListVoices(context.Background(), routing.Target{}, "m"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil multiplexer error = %v, want ErrUnavailable", err)
	}
}
