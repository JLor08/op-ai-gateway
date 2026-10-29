// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"testing"
	"time"
)

// The mute-teardown knob is the one voice timing whose 0 means something (keepalive), and
// config.Load keeps 0 distinct from the 10s default. This pins the last hop: the config value
// reaches gateway.VoiceTiming as a duration (>0) or as the explicit MuteKeepalive flag (0), so a
// keepalive cannot be lost to the gateway's "zero means default" rule for the other knobs.
func TestVoiceTimingFromConfigMuteTeardown(t *testing.T) {
	cases := []struct {
		name          string
		teardown      time.Duration
		wantTeardown  time.Duration
		wantKeepalive bool
	}{
		{"configured duration", 30 * time.Second, 30 * time.Second, false},
		{"default as config loads it", 10 * time.Second, 10 * time.Second, false},
		{"explicit zero is keepalive", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := voiceTimingFromConfig(config.Config{VoiceMuteTeardown: tc.teardown})
			if got.MuteTeardown != tc.wantTeardown || got.MuteKeepalive != tc.wantKeepalive {
				t.Fatalf("voiceTimingFromConfig(%v) = {MuteTeardown:%v MuteKeepalive:%v}, want {%v %v}",
					tc.teardown, got.MuteTeardown, got.MuteKeepalive, tc.wantTeardown, tc.wantKeepalive)
			}
		})
	}
}

// The existing knobs are carried across unchanged (a non-positive config value is already
// resolved to its default by config.Load, so the mapping is a plain copy).
func TestVoiceTimingFromConfigCopiesTheOtherKnobs(t *testing.T) {
	got := voiceTimingFromConfig(config.Config{
		VoiceSilenceTimeout:    1 * time.Second,
		VoiceMinUtterance:      2 * time.Second,
		VoiceMaxUtterance:      3 * time.Second,
		VoiceMicResumeTail:     4 * time.Second,
		VoiceContinuationGrace: 5 * time.Second,
	})
	if got.SilenceTimeout != 1*time.Second || got.MinUtterance != 2*time.Second ||
		got.MaxUtterance != 3*time.Second || got.MicResumeTail != 4*time.Second ||
		got.ContinuationGrace != 5*time.Second {
		t.Fatalf("voiceTimingFromConfig dropped or swapped a knob: %+v", got)
	}
}
