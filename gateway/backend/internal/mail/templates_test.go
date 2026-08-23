// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package mail

import (
	"strings"
	"testing"
)

func TestInviteGermanIsDefault(t *testing.T) {
	for _, lang := range []string{"de", "", "fr-CA", "DE"} {
		got := Invite(lang, "Alice", "https://gw/set-password?token=abc")
		if !strings.Contains(got.Subject, "OP AI Gateway") {
			t.Fatalf("lang %q subject = %q, want it to name the product", lang, got.Subject)
		}
		if !strings.Contains(got.Body, "Hallo Alice,") {
			t.Fatalf("lang %q body missing German greeting: %q", lang, got.Body)
		}
		if !strings.Contains(got.Body, "https://gw/set-password?token=abc") {
			t.Fatalf("lang %q body missing invite URL", lang)
		}
		if !strings.Contains(got.Body, "einmal") {
			t.Fatalf("lang %q body missing one-time note: %q", lang, got.Body)
		}
	}
}

func TestInviteEnglish(t *testing.T) {
	got := Invite("en", "Bob", "https://gw/x")
	if !strings.Contains(got.Body, "Hello Bob,") {
		t.Fatalf("en body = %q, want English greeting", got.Body)
	}
	if !strings.Contains(got.Body, "once") {
		t.Fatalf("en body missing one-time note: %q", got.Body)
	}
}

func TestInviteEmptyName(t *testing.T) {
	if got := Invite("de", "  ", "u"); !strings.Contains(got.Body, "Hallo,") {
		t.Fatalf("empty name de = %q, want generic greeting", got.Body)
	}
	if got := Invite("en", "", "u"); !strings.Contains(got.Body, "Hello,") {
		t.Fatalf("empty name en = %q, want generic greeting", got.Body)
	}
}

func TestTestMailContent(t *testing.T) {
	de := Test("de")
	if !strings.Contains(de.Subject, "SMTP-Test") || !strings.Contains(de.Body, "Test-E-Mail") {
		t.Fatalf("de test mail = %+v", de)
	}
	en := Test("en")
	if !strings.Contains(en.Subject, "SMTP test") || !strings.Contains(en.Body, "test email") {
		t.Fatalf("en test mail = %+v", en)
	}
}
