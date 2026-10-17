// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"testing"
)

// The upload cap of POST /v1/audio/transcriptions is an operator setting
// (config.MaxAudioTranscriptionBodyBytes,
// OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES). The gateway package defaults
// a ServerDeps value it was never handed, so a missed wiring line in buildRuntime
// would leave every operator's setting silently ignored -- the endpoint would
// keep its default and nothing would fail. So this drives the real production
// deps builder (memoryDeps -> buildRuntime, the same call every DB driver ends
// in) and asks that the configured value arrives in the ServerDeps handed to
// gateway.New. The rest of the chain (ServerDeps -> Server field -> default when
// <= 0) is pinned in the gateway package by TestTranscriptionBodyCapIsASetting;
// the end-to-end 413 is the handler's test, not this one's.
func TestBuildRuntimeWiresTheConfiguredAudioTranscriptionBodyCap(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cases := []struct {
		name string
		cfg  int
	}{
		{"a configured cap is passed through", 2048},
		{"an unset cap stays unset for gateway.New to default", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, cleanup, err := memoryDeps(config.Config{
				Addr: "127.0.0.1:8080", DBDriver: "memory",
				MaxAudioTranscriptionBodyBytes: tc.cfg,
			})
			if err != nil {
				t.Fatalf("memoryDeps returned %v", err)
			}
			t.Cleanup(func() { _ = cleanup() })
			if deps.MaxAudioTranscriptionBodyBytes != tc.cfg {
				t.Fatalf("ServerDeps.MaxAudioTranscriptionBodyBytes = %d, want %d (config.MaxAudioTranscriptionBodyBytes)", deps.MaxAudioTranscriptionBodyBytes, tc.cfg)
			}
		})
	}
}

// The audio cap must not be fed from the image-edit setting (a copy/paste slip
// in the ServerDeps literal): each cap follows its own config field.
func TestBuildRuntimeKeepsTheAudioAndImageEditBodyCapsApart(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	deps, cleanup, err := memoryDeps(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "memory",
		MaxImageEditBodyBytes:          1024,
		MaxAudioTranscriptionBodyBytes: 8192,
	})
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if deps.MaxImageEditBodyBytes != 1024 || deps.MaxAudioTranscriptionBodyBytes != 8192 {
		t.Fatalf("ServerDeps caps = image %d / audio %d, want 1024 / 8192", deps.MaxImageEditBodyBytes, deps.MaxAudioTranscriptionBodyBytes)
	}
}
