// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/portal"
	"testing"
)

// The run executor splits long speech audio across several chat blobs (#279), so
// it needs both ceilings the portal enforces at store time: the per-blob cap
// (config OP_AI_GATEWAY_MAX_CHAT_BLOB_BYTES) and the total-audio cap (config
// OP_AI_GATEWAY_MAX_CHAT_AUDIO_BYTES). ServerDeps carries both; a value <= 0
// falls back to the default; a bare &Server{} is left at zero.
func TestServerAudioCapsDefaulted(t *testing.T) {
	if DefaultMaxChatAudioBytes != 256<<20 {
		t.Fatalf("DefaultMaxChatAudioBytes = %d, want 256 MiB", DefaultMaxChatAudioBytes)
	}
	cases := []struct {
		name      string
		audio     int
		blob      int
		wantAudio int
		wantBlob  int
	}{
		{"unset uses the defaults", 0, 0, DefaultMaxChatAudioBytes, portal.DefaultMaxChatBlobBytes},
		{"negative uses the defaults", -5, -7, DefaultMaxChatAudioBytes, portal.DefaultMaxChatBlobBytes},
		{"configured values are honoured", 4096, 1024, 4096, 1024},
		{"the two caps are independent", 0, 1024, DefaultMaxChatAudioBytes, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(ServerDeps{MaxChatAudioBytes: tc.audio, MaxChatBlobBytes: tc.blob})
			if srv.maxChatAudioBytes != tc.wantAudio {
				t.Fatalf("maxChatAudioBytes = %d, want %d", srv.maxChatAudioBytes, tc.wantAudio)
			}
			if srv.maxChatBlobBytes != tc.wantBlob {
				t.Fatalf("maxChatBlobBytes = %d, want %d", srv.maxChatBlobBytes, tc.wantBlob)
			}
		})
	}
}

// A run executor reads the caps through chatAudioCaps, which keeps the field comment's
// promise: a Server that was not built by New (its caps 0) means the defaults, never "a
// zero-byte audio budget" or "a part per sample". Positive values pass through unchanged.
func TestChatAudioCapsResolvesZeroToTheDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		srv       *Server
		wantAudio int64
		wantBlob  int
	}{
		{"a bare Server has the defaults", &Server{}, DefaultMaxChatAudioBytes, portal.DefaultMaxChatBlobBytes},
		{"negative values are the defaults", &Server{maxChatAudioBytes: -1, maxChatBlobBytes: -1}, DefaultMaxChatAudioBytes, portal.DefaultMaxChatBlobBytes},
		{"configured values pass through", &Server{maxChatAudioBytes: 4096, maxChatBlobBytes: 1024}, 4096, 1024},
		{"each cap defaults on its own", &Server{maxChatBlobBytes: 1024}, DefaultMaxChatAudioBytes, 1024},
		{"a Server built by New has what New defaulted", New(ServerDeps{}), DefaultMaxChatAudioBytes, portal.DefaultMaxChatBlobBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioCap, blobCap := tc.srv.chatAudioCaps()
			if audioCap != tc.wantAudio || blobCap != tc.wantBlob {
				t.Fatalf("chatAudioCaps() = (%d, %d), want (%d, %d)", audioCap, blobCap, tc.wantAudio, tc.wantBlob)
			}
		})
	}
}
