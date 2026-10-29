// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
)

// voiceTimingFromConfig maps the OP_AI_GATEWAY_VOICE_* config onto the gateway's
// VoiceTiming. The turn-detection knobs are a plain copy (config.Load already resolved a
// non-positive value to its default).
//
// The mute teardown is the exception. config.Load keeps an explicit 0 as 0 (and an absent
// variable as the 10s default), but the gateway's VoiceTiming reads a zero duration as "use
// the default" like every other field, so a bare 0 would be lost there. The operator's 0 is
// therefore carried as the explicit MuteKeepalive flag (never tear the ASR loopback down while
// muted); any positive value is the teardown duration.
func voiceTimingFromConfig(cfg config.Config) gateway.VoiceTiming {
	return gateway.VoiceTiming{
		SilenceTimeout:    cfg.VoiceSilenceTimeout,
		MinUtterance:      cfg.VoiceMinUtterance,
		MaxUtterance:      cfg.VoiceMaxUtterance,
		MicResumeTail:     cfg.VoiceMicResumeTail,
		ContinuationGrace: cfg.VoiceContinuationGrace,
		MuteTeardown:      cfg.VoiceMuteTeardown,
		MuteKeepalive:     cfg.VoiceMuteTeardown == 0,
	}
}
