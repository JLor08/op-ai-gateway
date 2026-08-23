// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestIsMTPModelName(t *testing.T) {
	mtp := []string{
		"DeepSeek-V3", "deepseek-v3.1", "DeepSeek-V3.2-Exp", "GLM-4.6", "glm-4.5-air",
		"some-model-MTP", "Qwen3-mtp-30b", "model.mtp.gguf",
	}
	notMTP := []string{
		"qwen-coder", "gpt-oss-20b", "llama-3.1-8b-instruct", "deepseek-r1",
		"deepseek-coder-v2", "glm-4", "mistral-large", "",
		// In-word "mtp": contains the substring but is NOT a standalone token, so a
		// naive strings.Contains(lower, "mtp") regression would wrongly flag these.
		"mtproto", "amtpx", "xmtp",
	}
	for _, n := range mtp {
		if !IsMTPModelName(n) {
			t.Errorf("IsMTPModelName(%q) = false, want true", n)
		}
	}
	for _, n := range notMTP {
		if IsMTPModelName(n) {
			t.Errorf("IsMTPModelName(%q) = true, want false", n)
		}
	}
}
