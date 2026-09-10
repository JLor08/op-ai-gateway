// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "strings"

// mtpModelSubstrings are lowercased fragments whose presence marks a
// Multi-Token-Prediction-capable model. BEST-EFFORT seed only: it sets the default
// "mtp" capability row at mapping-creation (portal.legacyMTPCapabilityRow) and is
// always overridable through the operator's own three-state control in the UI.
// The row is DISPLAY and operator-seed only -- it does not feed routing or
// scoring -- so a false positive now costs a wrong chip an operator can flip,
// not a routing decision. Keep it conservative anyway: a wrong guess an
// operator never notices is still a wrong fact on the mapping. Extend as new
// MTP families appear.
var mtpModelSubstrings = []string{
	"deepseek-v3", // DeepSeek-V3 / V3.1 / V3.2 ship an MTP head
	"glm-4.5",     // GLM-4.5 / 4.6 expose MTP
	"glm-4.6",
}

// IsMTPModelName reports whether a model NAME looks like a Multi-Token-Prediction
// model: a standalone "mtp" token (bounded by non-alphanumerics/string ends) OR a
// known MTP family substring. Case-insensitive.
func IsMTPModelName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	for _, sub := range mtpModelSubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return containsStandaloneToken(lower, "mtp")
}

func containsStandaloneToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); {
		idx := strings.Index(s[i:], tok)
		if idx < 0 {
			return false
		}
		start := i + idx
		end := start + len(tok)
		leftOK := start == 0 || !isAlnum(s[start-1])
		rightOK := end == len(s) || !isAlnum(s[end])
		if leftOK && rightOK {
			return true
		}
		i = start + 1
	}
	return false
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
