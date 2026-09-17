// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/routing"
	"testing"
)

// The single-model default path's capability refusal must not look like an
// unknown model: its own code, its own 404, and its own client-visible
// message (Task 5's images handler tests depend on this exact string).
func TestCompletionErrorMappingForCapabilityRefusal(t *testing.T) {
	if got := completionErrorCode(routing.ErrModelNotCapable); got != "routing.model_not_capable" {
		t.Errorf("code = %q, want routing.model_not_capable", got)
	}
	if got := completionHTTPStatus(routing.ErrModelNotCapable); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
	if got := completionErrorResponse(routing.ErrModelNotCapable).Error.Message; got != "the requested model is not capable of this endpoint" {
		t.Errorf("message = %q, want %q", got, "the requested model is not capable of this endpoint")
	}
}

// Before this change, both ErrNoModelRoute and ErrNoHealthyHost had no case in
// completionHTTPStatus and fell through to its StatusBadGateway (502)
// default. This test pins the fix (404 / 503) rather than the old value.
// It matters beyond the single-model path: the group path's own capability
// refusal surfaces as ErrNoModelRoute too (eligibleCandidates' pre-existing
// live-bool/memberNoMapping split reads a capability-emptied member the same
// as a genuinely unmapped one, by design -- see resolver.go), so this same
// fix is what stops a group-path capability refusal from reading as a 502
// upstream outage. It does not give the group path its own
// routing.model_not_capable code -- see task-3-report.md for that residual
// gap.
func TestCompletionStatusForRoutingRefusals(t *testing.T) {
	if got := completionHTTPStatus(routing.ErrNoModelRoute); got != http.StatusNotFound {
		t.Errorf("ErrNoModelRoute status = %d, want 404", got)
	}
	if got := completionHTTPStatus(routing.ErrNoHealthyHost); got != http.StatusServiceUnavailable {
		t.Errorf("ErrNoHealthyHost status = %d, want 503", got)
	}
}
