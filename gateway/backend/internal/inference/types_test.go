// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package inference

import "testing"

func TestRequestValidateRequiresModel(t *testing.T) {
	req := Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}}},
	}

	err := req.Validate()

	if err == nil || err.Code != "request.model_required" {
		t.Fatalf("Validate error = %#v, want request.model_required", err)
	}
}

func TestRequestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		code string
	}{
		{
			name: "requires messages",
			req:  Request{Model: "gpt-oss-20b"},
			code: "request.messages_required",
		},
		{
			name: "requires message role",
			req: Request{
				Model:    "gpt-oss-20b",
				Messages: []Message{{Content: []ContentPart{{Type: ContentText, Text: "hello"}}}},
			},
			code: "request.role_required",
		},
		{
			name: "requires message content",
			req: Request{
				Model:    "gpt-oss-20b",
				Messages: []Message{{Role: RoleUser}},
			},
			code: "request.content_required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()

			if err == nil || err.Code != tt.code {
				t.Fatalf("Validate error = %#v, want %s", err, tt.code)
			}
		})
	}
}

func TestRequestValidateAcceptsTextMessage(t *testing.T) {
	req := Request{
		Model: "gpt-oss-20b",
		Messages: []Message{
			{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}},
		},
	}

	if err := req.Validate(); err != nil {
		t.Fatalf("Validate returned %#v", err)
	}
}

func TestTextReturnsJoinedTextParts(t *testing.T) {
	msg := Message{
		Role: RoleUser,
		Content: []ContentPart{
			{Type: ContentText, Text: "hello"},
			{Type: ContentText, Text: "world"},
		},
	}

	if got := msg.Text(); got != "hello\nworld" {
		t.Fatalf("Text() = %q, want hello newline world", got)
	}
}
