// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/your-org/onprem-ai-gateway/gateway/backend/pkg/auditlog"
)

// NewHandler returns the gateway's HTTP entry point.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", method(http.MethodGet, health))
	mux.HandleFunc("/api/v1/status", method(http.MethodGet, status))
	mux.HandleFunc("/api/v1/audit/record", method(http.MethodPost, recordAuditEvent))
	mux.HandleFunc("/api/v1/agent/reports", method(http.MethodPost, acceptAgentReport))
	return securityHeaders(mux)
}

func acceptAgentReport(w http.ResponseWriter, r *http.Request) {
	if expected := strings.TrimSpace(httpEnv("GATEWAY_AGENT_TOKEN")); expected != "" && r.Header.Get("Authorization") != "Bearer "+expected {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()
	var report struct { AgentID string `json:"agent_id"`; RecordedAt string `json:"recorded_at"` }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&report); err != nil || strings.TrimSpace(report.AgentID) == "" {
		http.Error(w, "invalid agent report", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func recordAuditEvent(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "request body must not exceed 1 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	writeJSON(w, http.StatusCreated, auditlog.Record("gateway.request.received", payload))
}

func method(expected string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected {
			w.Header().Set("Allow", expected)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func status(w http.ResponseWriter, _ *http.Request) {
	upstreams := strings.Split(strings.TrimSpace(defaultValue("GATEWAY_UPSTREAMS", "")), ",")
	if len(upstreams) == 1 && upstreams[0] == "" {
		upstreams = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"service":   "onprem-ai-gateway",
		"upstreams": upstreams,
	})
}

func defaultValue(key, fallback string) string {
	if value := strings.TrimSpace(httpEnv(key)); value != "" {
		return value
	}
	return fallback
}

// httpEnv is assigned in a small function to keep configuration access local.
func httpEnv(key string) string { return getenv(key) }

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
