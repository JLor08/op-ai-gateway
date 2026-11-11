// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"testing"
)

// The JSON body cap of POST /v1/systemone is an operator setting
// (config.MaxDecisionsBodyBytes, OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES). The
// gateway package defaults a ServerDeps value it was never handed, so a missed
// wiring line in buildRuntime would leave every operator's setting silently
// ignored. This drives the real deps builder and asks that the configured value
// arrives in the ServerDeps handed to gateway.New.
func TestBuildRuntimeWiresTheConfiguredDecisionsBodyCap(t *testing.T) {
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
				MaxDecisionsBodyBytes: tc.cfg,
			})
			if err != nil {
				t.Fatalf("memoryDeps returned %v", err)
			}
			t.Cleanup(func() { _ = cleanup() })
			if deps.MaxDecisionsBodyBytes != tc.cfg {
				t.Fatalf("ServerDeps.MaxDecisionsBodyBytes = %d, want %d (config.MaxDecisionsBodyBytes)", deps.MaxDecisionsBodyBytes, tc.cfg)
			}
		})
	}
}

// The decisions cap must not be fed from the audio setting: each cap follows its
// own config field.
func TestBuildRuntimeKeepsTheDecisionsAndAudioBodyCapsApart(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	deps, cleanup, err := memoryDeps(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "memory",
		MaxAudioTranscriptionBodyBytes: 1024,
		MaxDecisionsBodyBytes:          8192,
	})
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if deps.MaxAudioTranscriptionBodyBytes != 1024 || deps.MaxDecisionsBodyBytes != 8192 {
		t.Fatalf("ServerDeps caps = audio %d / decisions %d, want 1024 / 8192", deps.MaxAudioTranscriptionBodyBytes, deps.MaxDecisionsBodyBytes)
	}
}
