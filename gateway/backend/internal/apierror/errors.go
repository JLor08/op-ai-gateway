// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package apierror

type Body struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Type is an OpenAI-client-facing classification (e.g.
	// "invalid_request_error"). Response (below) never sets it -- every
	// existing caller's stable dotted code is the whole of this package's own
	// contract -- so it stays empty, and omitempty, for them. It exists for a
	// caller (images' upstream-error normalisation) that must state a type an
	// OpenAI client expects but an upstream never attested, and it is always
	// that caller's OWN value, never copied from unattested upstream data.
	Type      string `json:"type,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func Response(code string, message string, requestID string) Body {
	return Body{
		Error: ErrorBody{
			Code:      code,
			Message:   message,
			RequestID: requestID,
		},
	}
}
