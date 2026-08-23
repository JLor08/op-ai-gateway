// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"strings"

	"github.com/donyori/gogo/copyright/agpl3"
)

const (
	programName     = "OP AI Gateway Server-Agent"
	copyrightYear   = "2026"
	copyrightAuthor = "OnPrem AI Gateway contributors"
	sourceURL       = "https://github.com/JLor08/op-ai-gateway"
)

// licenseNotice returns the AGPL "Appropriate Legal Notices" for the agent: the
// standard copyright notice (rendered by the AGPL-3.0-licensed
// github.com/donyori/gogo), the license identifier, and the source offer
// required by AGPL section 13. Printed by the `-license` flag.
func licenseNotice() string {
	var b strings.Builder
	_, _ = agpl3.PrintCopyrightNotice(&b, programName, copyrightYear, copyrightAuthor, sourceURL)
	b.WriteString("    License: GNU AGPL-3.0-only <https://www.gnu.org/licenses/agpl-3.0.html>.\n")
	return b.String()
}
