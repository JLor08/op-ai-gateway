// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	NewHandler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, response.Code)
	}
}

func TestAuditRecordDoesNotReturnPrompt(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/audit/record", bytes.NewBufferString("private prompt"))
	response := httptest.NewRecorder()

	NewHandler().ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d", http.StatusCreated, response.Code)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("private prompt")) {
		t.Fatal("audit response exposed the request payload")
	}
}
