// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
)

// tracingStatusDTO is the GET /api/system/tracing body and the PUT response.
type tracingStatusDTO struct {
	Enabled         bool `json:"enabled"`
	OTLPEndpointSet bool `json:"otlp_endpoint_set"`
}

// setTracingRequest is the PUT /api/system/tracing request body.
type setTracingRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSystemTracing serves GET/PUT /api/system/tracing (system scope). GET
// reports the live master state; PUT {enabled} flips the sampler at runtime
// (no restart). A nil Tracing provider reports disabled and no-ops the toggle.
func (s *Server) handleSystemTracing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		return
	}
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if r.Method == http.MethodPut {
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req setTracingRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		s.Tracing.SetEnabled(req.Enabled) // nil-safe
	}
	writeJSON(w, http.StatusOK, tracingStatusDTO{
		Enabled:         s.Tracing.Enabled(), // nil-safe -> false
		OTLPEndpointSet: s.tracingOTLPSet,
	})
}
