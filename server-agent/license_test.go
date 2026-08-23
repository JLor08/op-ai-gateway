// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"strings"
	"testing"
)

func TestAgentLicenseNoticeMentionsAGPLAndSource(t *testing.T) {
	s := licenseNotice()
	if !strings.Contains(s, "AGPL") {
		t.Errorf("notice missing AGPL: %q", s)
	}
	if !strings.Contains(s, "https://github.com/JLor08/op-ai-gateway") {
		t.Errorf("notice missing source URL: %q", s)
	}
	if !strings.Contains(s, "OnPrem AI Gateway contributors") {
		t.Errorf("notice missing copyright holder: %q", s)
	}
}
