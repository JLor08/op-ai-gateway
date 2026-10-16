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

// audioCppLiveListing is the shape audio.cpp v0.9.1 really answers on
// GET /v1/models, probed live: an OpenAI-style list whose entries add family,
// task, mode, loaded and path. The list names LOADED models only. The task
// field is the capability signal this prober reads.
const audioCppLiveListing = `{"object":"list","data":[
  {"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/models/pocket_tts_english_bf16.gguf"}
]}`

// TestParseAudioCppModels pins the mapping rule: the loaded entry's own task is
// authoritative. A synthesis task (tts, clone) is a speech "yes", any other
// task is a speech "no", and a model the listing does not name -- or a listing
// this code cannot read -- is no verdict.
func TestParseAudioCppModels(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		modelID string
		want    string // "yes" | "no" | ""
	}{
		{"real v0.9.1 entry, task tts", audioCppLiveListing, "pocket_tts_english_bf16", "yes"},
		{"task tts", `{"data":[{"id":"m","task":"tts"}]}`, "m", "yes"},
		{"task clone also produces speech", `{"data":[{"id":"m","task":"clone"}]}`, "m", "yes"},
		{"task asr is a no", `{"data":[{"id":"m","task":"asr"}]}`, "m", "no"},
		{"a music task is a no", `{"data":[{"id":"m","task":"music"}]}`, "m", "no"},
		{"model absent from the listing", `{"data":[{"id":"other","task":"tts"}]}`, "m", ""},
		{"empty data", `{"object":"list","data":[]}`, "m", ""},
		{"data absent", `{"object":"list"}`, "m", ""},
		{"data null", `{"data":null}`, "m", ""},
		{"the entry is picked out by id among several", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"},{"id":"c","task":"asr"}]}`, "b", "yes"},
		{"the sibling's task does not leak", `{"data":[{"id":"a","task":"asr"},{"id":"b","task":"tts"}]}`, "a", "no"},
		{"id match is exact, not a prefix", `{"data":[{"id":"pocket_tts_english_bf16","task":"tts"}]}`, "pocket_tts", ""},
		{"id match is case-sensitive", `{"data":[{"id":"M","task":"tts"}]}`, "m", ""},
		{"a task padded with whitespace still maps", `{"data":[{"id":"m","task":" tts "}]}`, "m", "yes"},
		{"a case variant of the task is not tts", `{"data":[{"id":"m","task":"TTS"}]}`, "m", "no"},
		{"a named entry without a task says nothing", `{"data":[{"id":"m"}]}`, "m", ""},
		{"a named entry with an empty task says nothing", `{"data":[{"id":"m","task":""}]}`, "m", ""},
		{"an empty model id never matches", `{"data":[{"id":"","task":"tts"}]}`, "", ""},
		{"unknown and extra fields are ignored", `{"object":"list","x":1,"data":[{"id":"m","task":"tts","loaded":false,"extra":{"k":[1,2]}}]}`, "m", "yes"},
		{"data of the wrong type", `{"data":"m"}`, "m", ""},
		{"a task of the wrong type", `{"data":[{"id":"m","task":1}]}`, "m", ""},
		{"not JSON", `<html>502</html>`, "m", ""},
		{"empty body", ``, "m", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAudioCppModels([]byte(tc.body), tc.modelID)
			if got.Speech != tc.want {
				t.Fatalf("Speech = %q, want %q", got.Speech, tc.want)
			}
		})
	}
}

func TestProbeAudioCppModels(t *testing.T) {
	probe := func(t *testing.T, srv *httptest.Server, ctx context.Context, modelID string) (AudioCppVerdicts, error) {
		t.Helper()
		return NewOpenAICompatibleClient(srv.Client()).ProbeAudioCppModels(ctx, routing.Target{Endpoint: srv.URL}, modelID)
	}

	t.Run("a tts model yields yes from GET /v1/models", func(t *testing.T) {
		var method, path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got.Speech != routing.CapabilityYes {
			t.Fatalf("got %+v, want {Speech:yes}", got)
		}
		if method != http.MethodGet || path != "/v1/models" {
			t.Fatalf("request = %s %s, want GET /v1/models", method, path)
		}
	})

	t.Run("a loaded asr model yields no", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"whisper_small","task":"asr","loaded":true}]}`))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "whisper_small")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got.Speech != routing.CapabilityNo {
			t.Fatalf("got %+v, want {Speech:no}", got)
		}
	})

	t.Run("a model the listing does not name yields no verdict and no error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "not_loaded")
		if err != nil {
			t.Fatalf("error = %v, want nil -- the server answered, it just does not name the model", err)
		}
		if got.Speech != "" {
			t.Fatalf("Speech = %q, want empty -- a missing entry says nothing about the model", got.Speech)
		}
	})

	t.Run("a non-2xx status is an error even when the body would decode to a yes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		got, err := probe(t, srv, context.Background(), "pocket_tts_english_bf16")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable for a 503", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict -- a failing server's body is not its answer", got)
		}
	})

	t.Run("a transport failure is an error and no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := NewOpenAICompatibleClient(srv.Client())
		endpoint := srv.URL
		srv.Close() // nothing listens any more
		got, err := client.ProbeAudioCppModels(context.Background(), routing.Target{Endpoint: endpoint}, "m")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if got != (AudioCppVerdicts{}) {
			t.Fatalf("got %+v, want no verdict", got)
		}
	})

	t.Run("a body that is not JSON yields no verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not the listing</html>"))
		}))
		defer srv.Close()
		got, _ := probe(t, srv, context.Background(), "m")
		if got.Speech != "" {
			t.Fatalf("Speech = %q, want empty -- an unreadable body must not read as a verdict", got.Speech)
		}
	})

	t.Run("the application's credential is attached", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(audioCppLiveListing))
		}))
		defer srv.Close()
		ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "audio-secret")
		if _, err := probe(t, srv, ctx, "pocket_tts_english_bf16"); err != nil {
			t.Fatalf("error = %v", err)
		}
		if gotAuth != "audio-secret" {
			t.Fatalf("X-Api-Key = %q, want audio-secret", gotAuth)
		}
	})
}

// TestMultiplexerDispatchesAudioCppModelsToOpenAICompatibleClient mirrors the
// sdcpp dispatch test: a target whose provider maps to the shared
// OpenAI-compatible client reaches its ProbeAudioCppModels -- so the probe
// rides the outbound app transport -- while a provider whose client lacks the
// capability answers ErrUnavailable and sends nothing (no cross-route).
func TestMultiplexerDispatchesAudioCppModelsToOpenAICompatibleClient(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(audioCppLiveListing))
	}))
	defer srv.Close()
	shared := NewOpenAICompatibleClient(srv.Client())
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:   shared,
		routing.ProviderOllama: &recordingClient{},
	}, nil)

	got, err := mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderVLLM, Endpoint: srv.URL}, "pocket_tts_english_bf16")
	if err != nil {
		t.Fatalf("ProbeAudioCppModels(vllm) returned %v", err)
	}
	if got.Speech != routing.CapabilityYes {
		t.Fatalf("got %+v, want {Speech:yes}", got)
	}
	if len(paths) != 1 || paths[0] != "/v1/models" {
		t.Fatalf("upstream paths = %v, want exactly [/v1/models]", paths)
	}

	got, err = mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}, "pocket_tts_english_bf16")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProbeAudioCppModels(ollama) error = %v, want ErrUnavailable -- a client without the capability has no verdict", err)
	}
	if got != (AudioCppVerdicts{}) {
		t.Fatalf("ProbeAudioCppModels(ollama) = %+v, want the zero value", got)
	}
	if len(paths) != 1 {
		t.Fatalf("upstream paths = %v, want no request for a client without the capability", paths)
	}
}

// TestMultiplexerAudioCppModelsWithoutProberIsUnavailable pins the bare
// posture: with no client that implements AudioCppModelsProber -- neither the
// provider's nor a fallback -- the multiplexer answers ErrUnavailable. A zero
// value with a nil error would read as a successful probe that found nothing.
func TestMultiplexerAudioCppModelsWithoutProberIsUnavailable(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)

	got, err := mux.ProbeAudioCppModels(context.Background(), routing.Target{Provider: routing.ProviderMock, Endpoint: "http://x:8000"}, "m")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if got != (AudioCppVerdicts{}) {
		t.Fatalf("got %+v, want the zero value", got)
	}

	var nilMux *Multiplexer
	if _, err := nilMux.ProbeAudioCppModels(context.Background(), routing.Target{}, "m"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil multiplexer error = %v, want ErrUnavailable", err)
	}
}
