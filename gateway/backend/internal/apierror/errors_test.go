// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package apierror

import (
	"encoding/json"
	"testing"
)

func TestResponseJSONUsesStableCode(t *testing.T) {
	body := Response("routing.no_healthy_host", "no healthy host", "req_123")

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal returned %v", err)
	}

	want := `{"error":{"code":"routing.no_healthy_host","message":"no healthy host","request_id":"req_123"}}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", string(data), want)
	}
}

func TestResponseJSONOmitsEmptyRequestID(t *testing.T) {
	body := Response("auth.invalid_token", "invalid bearer token", "")

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal returned %v", err)
	}

	want := `{"error":{"code":"auth.invalid_token","message":"invalid bearer token"}}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", string(data), want)
	}
}
