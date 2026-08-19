// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auditlog

import "testing"

func TestRecordDoesNotExposePayload(t *testing.T) {
	event := Record("chat.completion.requested", []byte("private prompt"))
	if event.PayloadSHA256 == "private prompt" || len(event.PayloadSHA256) != 64 {
		t.Fatalf("expected SHA-256 digest, got %q", event.PayloadSHA256)
	}
}
