// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"op-ai-gateway/internal/apierror"
	"testing"
)

// requireErrorCode asserts that body is an apierror response whose
// error.code is EXACTLY wantCode.
//
// It replaces `strings.Contains(body, "some.code")`, which this package used
// in 52 places and which cannot catch the drift the assertion exists to
// catch. A substring match stays green when the wire contract changes
// underneath it: a code that grows a suffix still CONTAINS the wanted string,
// so `some.code_v2` satisfies a test pinned to `some.code`. It also passes
// when the code appears anywhere in the body at all -- inside the human
// message, or in some nested field -- rather than specifically as the error's
// code.
//
// That is not a hypothetical. In #125 three assertions of this shape were
// found in chat_run_endpoints_test.go, and one of them was covering a surface
// that turned out to be genuinely unpinned: `portal.chat_run_active` has two
// wire surfaces sharing one errRow, and mutating the code to a suffixed
// variant left the run-start test green.
//
// The parameter is a string rather than []byte so that converting a call site
// leaves its body expression exactly as it was (`rec.Body.String()`,
// `string(raw)`, a plain `body`), which keeps the sweep reviewable as a
// mechanical change.
//
// Use strings.Contains instead when substring really is the right operator:
// asserting a fragment is ABSENT (server_stream_timeout_test.go checks that a
// timeout code does not appear), or matching free text rather than a code
// (edge_certificates_test.go looks for a hostname inside a generated nginx
// configuration).
func requireErrorCode(t *testing.T, body, wantCode string) {
	t.Helper()
	var parsed apierror.Body
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("error body did not decode as apierror.Body: %v (body = %s)", err, body)
	}
	if parsed.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q (body = %s)", parsed.Error.Code, wantCode, body)
	}
}
