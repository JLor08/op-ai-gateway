// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
)

// audioRefusingPortal answers the mapping writes with the refusal a store's
// full-row writer returns for an audio figure no measurement yields
// (routing.ValidateMappingAudioMetrics), wrapped as a store error would be.
type audioRefusingPortal struct {
	portal.API
}

func (audioRefusingPortal) CreateMapping(context.Context, auth.Token, string, portal.CreateMappingRequest) (portal.ModelMappingDTO, error) {
	return portal.ModelMappingDTO{}, fmt.Errorf("create mapping: %w", routing.ValidateMappingAudioMetrics(routing.ModelMapping{AudioRateX: math.Inf(1)}))
}

func (audioRefusingPortal) UpdateMapping(context.Context, auth.Token, string, portal.UpdateMappingRequest) (portal.ModelMappingDTO, error) {
	return portal.ModelMappingDTO{}, fmt.Errorf("update mapping: %w", routing.ValidateMappingAudioMetrics(routing.ModelMapping{AudioTTFAMS: -1}))
}

// TestPortalMappingAudioMetricRefusalReturns400 pins that a mapping write the
// store refuses for an invalid audio figure (routing.ErrAudioMetricsInvalid)
// reaches the caller as the 400 mapping.metric_invalid every other invalid
// mapping metric gets, on the create and the update path, instead of the
// fallback 500 a store failure gets.
func TestPortalMappingAudioMetricRefusalReturns400(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8030,"scheme":"https"}`)
	srv.Portal = audioRefusingPortal{API: srv.Portal}

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"create", newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/mappings", `{"gateway_model_name":"tts","app_model_name":"tts-1"}`)},
		{"update", newJSONRequest(http.MethodPatch, "/api/portal/mappings/map_any", `{"gateway_model_name":"tts"}`)},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, tc.req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body = %s", tc.name, rec.Code, rec.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: unmarshal error body: %v", tc.name, err)
		}
		if body.Error.Code != "mapping.metric_invalid" {
			t.Fatalf("%s: error code = %q, want mapping.metric_invalid (body = %s)", tc.name, body.Error.Code, rec.Body.String())
		}
	}
}

// The mapping update carries the audio metrics over HTTP under their wire keys:
// a valid edit is answered with them, and a count sent without its limit is the
// 400 mapping.metric_invalid an invalid text metric gets.
func TestPortalMappingAudioMetricsEditOverHTTP(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8030,"scheme":"https"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/mappings", `{"gateway_model_name":"tts","app_model_name":"tts-1"}`))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create mapping: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create mapping body: %v (%s)", err, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/mappings/"+created.ID, `{"audio_rate_x":1.5,"audio_realtime_concurrency":2,"audio_concurrency_limit":"cap"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid audio edit: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		RateX float64 `json:"audio_rate_x"`
		Count int     `json:"audio_realtime_concurrency"`
		Limit string  `json:"audio_concurrency_limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil || dto.RateX != 1.5 || dto.Count != 2 || dto.Limit != routing.AudioLimitCap {
		t.Fatalf("valid audio edit answered %+v (%v), want 1.5, 2, cap", dto, err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/mappings/"+created.ID, `{"audio_realtime_concurrency":2}`))
	if rec.Code != http.StatusBadRequest || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("count without a limit: status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "mapping.metric_invalid" {
		t.Fatalf("count without a limit: code = %q (%v), want mapping.metric_invalid", body.Error.Code, err)
	}
}
