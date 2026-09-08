// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"errors"
	"net/http"
	"testing"
)

// TestUnavailableStatusTagsAuthRejection pins the #58 contract: 401/403 are
// programmatically distinguishable (the app-health pass logs a
// misconfigured-token signal on them) while still being ErrUnavailable to
// every existing caller; nothing else carries the tag.
func TestUnavailableStatusTagsAuthRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := unavailableStatus(status)
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("status %d: not ErrAuthRejected: %v", status, err)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("status %d: lost the ErrUnavailable wrap: %v", status, err)
		}
	}
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if errors.Is(unavailableStatus(status), ErrAuthRejected) {
			t.Fatalf("status %d must NOT be ErrAuthRejected", status)
		}
	}
	if !errors.Is(unavailableStatus(http.StatusServiceUnavailable), ErrUpstreamStarting) {
		t.Fatal("503 lost its ErrUpstreamStarting tag")
	}
}
