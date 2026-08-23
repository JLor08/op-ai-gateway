// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package mail

import (
	"fmt"
	"strings"
)

// productName is the backend-visible product name. The backend has no theme
// context (themes rename the product only in the browser), so email always
// uses the neutral default.
const productName = "OP AI Gateway"

// Content is a localized subject + plaintext body for one message.
type Content struct {
	Subject string
	Body    string
}

// normalizeLang maps an arbitrary preferred-language string (e.g. a
// store.User.PreferredLanguage such as "de" or "en") to a supported template
// language, defaulting to German.
func normalizeLang(lang string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "en"):
		return "en"
	default:
		return "de"
	}
}

// Invite renders the activation/invite email for the recipient's language.
// displayName may be empty (falls back to a generic greeting); inviteURL is the
// one-time set-password link.
func Invite(lang, displayName, inviteURL string) Content {
	name := strings.TrimSpace(displayName)
	if normalizeLang(lang) == "en" {
		greeting := "Hello,"
		if name != "" {
			greeting = fmt.Sprintf("Hello %s,", name)
		}
		return Content{
			Subject: fmt.Sprintf("Your %s account", productName),
			Body: strings.Join([]string{
				greeting,
				"",
				fmt.Sprintf("An account has been created for you on %s.", productName),
				"Please set your password using the one-time link below:",
				"",
				inviteURL,
				"",
				"This link can be used only once. If you did not expect this email, you can ignore it.",
				"",
				fmt.Sprintf("-- %s", productName),
			}, "\n"),
		}
	}
	greeting := "Hallo,"
	if name != "" {
		greeting = fmt.Sprintf("Hallo %s,", name)
	}
	return Content{
		Subject: fmt.Sprintf("Ihr Zugang zum %s", productName),
		Body: strings.Join([]string{
			greeting,
			"",
			fmt.Sprintf("Für Sie wurde ein Zugang zum %s angelegt.", productName),
			"Bitte legen Sie Ihr Passwort über den folgenden Einmal-Link fest:",
			"",
			inviteURL,
			"",
			"Der Link kann nur einmal verwendet werden. Falls Sie diese E-Mail nicht erwartet haben, können Sie sie ignorieren.",
			"",
			fmt.Sprintf("-- %s", productName),
		}, "\n"),
	}
}

// Test renders the fixed test-email message for the recipient's language, sent
// by the "Testmail senden" endpoint to verify a saved SMTP configuration.
func Test(lang string) Content {
	if normalizeLang(lang) == "en" {
		return Content{
			Subject: fmt.Sprintf("%s SMTP test", productName),
			Body: strings.Join([]string{
				fmt.Sprintf("This is a test email from %s.", productName),
				"If you received it, your SMTP settings are working.",
			}, "\n"),
		}
	}
	return Content{
		Subject: fmt.Sprintf("%s SMTP-Test", productName),
		Body: strings.Join([]string{
			fmt.Sprintf("Dies ist eine Test-E-Mail vom %s.", productName),
			"Wenn Sie sie erhalten haben, funktionieren Ihre SMTP-Einstellungen.",
		}, "\n"),
	}
}
