// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

// The multipart read cap of POST /v1/audio/transcriptions is the server's
// setting, not a constant: ServerDeps carries it (config
// OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES), a value <= 0 falls back to
// the default, and the default is the agent router's own 32 MiB. It is its own
// field, independent of the image-edit cap.
func TestTranscriptionBodyCapIsASetting(t *testing.T) {
	if defaultMaxAudioTranscriptionBodyBytes != 32<<20 {
		t.Fatalf("defaultMaxAudioTranscriptionBodyBytes = %d, want 32 MiB (the agent router's own cap)", defaultMaxAudioTranscriptionBodyBytes)
	}
	if defaultMaxAudioTranscriptionBodyBytes != agentRouterMaxBodyBytes {
		t.Fatalf("defaultMaxAudioTranscriptionBodyBytes = %d, want agentRouterMaxBodyBytes (%d)", defaultMaxAudioTranscriptionBodyBytes, agentRouterMaxBodyBytes)
	}
	if maxTranscriptionFieldBytes != 1<<20 {
		t.Fatalf("maxTranscriptionFieldBytes = %d, want 1 MiB", maxTranscriptionFieldBytes)
	}
	cases := []struct {
		name string
		deps int
		want int64
	}{
		{"unset uses the default", 0, defaultMaxAudioTranscriptionBodyBytes},
		{"negative uses the default", -5, defaultMaxAudioTranscriptionBodyBytes},
		{"configured value is honoured", 4096, 4096},
		{"a value above the default is honoured", 64 << 20, 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(ServerDeps{MaxAudioTranscriptionBodyBytes: tc.deps})
			if srv.maxAudioTranscriptionBodyBytes != tc.want {
				t.Fatalf("maxAudioTranscriptionBodyBytes = %d, want %d", srv.maxAudioTranscriptionBodyBytes, tc.want)
			}
			if got := srv.transcriptionBodyLimit(); got != tc.want {
				t.Fatalf("transcriptionBodyLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A Server that was never built through New (a bare &Server{}) reads as the
// default, like imageEditBodyLimit does.
func TestTranscriptionBodyLimitDefaultsOnABareServer(t *testing.T) {
	if got := (&Server{}).transcriptionBodyLimit(); got != defaultMaxAudioTranscriptionBodyBytes {
		t.Fatalf("bare Server transcriptionBodyLimit() = %d, want %d", got, defaultMaxAudioTranscriptionBodyBytes)
	}
}

// The two upload caps do not leak into one another.
func TestTranscriptionBodyCapIsIndependentOfTheImageEditCap(t *testing.T) {
	srv := New(ServerDeps{MaxImageEditBodyBytes: 1024, MaxAudioTranscriptionBodyBytes: 8192})
	if got := srv.imageEditBodyLimit(); got != 1024 {
		t.Fatalf("imageEditBodyLimit() = %d, want 1024", got)
	}
	if got := srv.transcriptionBodyLimit(); got != 8192 {
		t.Fatalf("transcriptionBodyLimit() = %d, want 8192", got)
	}
}
