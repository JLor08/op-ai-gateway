// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import "testing"

// TestUnwrapServiceRecoversWrappedService pins the one thing cmd/gateway
// needs from this helper (see Service.SetRuntimeConfigChangedHook's doc):
// given the SAME wrapping NewAPIWithTracing produces for ServerDeps.Portal,
// UnwrapService must recover the exact underlying *Service.
func TestUnwrapServiceRecoversWrappedService(t *testing.T) {
	svc := NewService(ServiceDeps{})
	wrapped := NewAPIWithTracing(svc)
	if got := UnwrapService(wrapped); got != svc {
		t.Fatalf("UnwrapService(wrapped) = %p, want the original %p", got, svc)
	}
}

// An UNwrapped *Service must also be recovered as-is (defensive: nothing
// requires the caller to have gone through NewAPIWithTracing).
func TestUnwrapServiceHandlesUnwrappedService(t *testing.T) {
	svc := NewService(ServiceDeps{})
	if got := UnwrapService(svc); got != svc {
		t.Fatalf("UnwrapService(svc) = %p, want %p", got, svc)
	}
}

// Neither a nil API nor some other API implementation is a *Service --
// UnwrapService must return nil, never panic.
func TestUnwrapServiceReturnsNilForNeitherShape(t *testing.T) {
	if got := UnwrapService(nil); got != nil {
		t.Fatalf("UnwrapService(nil) = %v, want nil", got)
	}
}
