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
