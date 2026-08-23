// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/store"
	"time"
)

const (
	codeCaptureNotFound = "capture.not_found"
	msgCaptureNotFound  = "capture not found"
)

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": s.Usage.ByUser(token.UserID)})
}

func (s *Server) handlePortalUsage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	page, err := s.Portal.Usage(token, parseUsageQuery(r, time.Now().UTC()))
	if err != nil {
		log.Printf("portal: usage query failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.query_failed", "usage query failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handlePortalUsageCapture(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	id := pathID(r.URL.Path, "/api/portal/usage/captures/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("request.not_found", "not found", ""))
		return
	}
	switch r.Method {
	case http.MethodGet:
		detail, err := s.Portal.CaptureDetail(token, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, apierror.Response(codeCaptureNotFound, msgCaptureNotFound, ""))
				return
			}
			// Decrypt/parse failure (never ErrNotFound here). Log the usage-event id
			// and the error for diagnosis, but NEVER the plaintext or the sealed blob.
			log.Printf("capture: detail failed for usage event %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("capture.detail_failed", "capture detail failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, detail)
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var body struct {
			Secret bool `json:"secret"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, "invalid JSON body", ""))
			return
		}
		if err := s.Portal.SetCaptureSecret(token, id, body.Secret); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, apierror.Response(codeCaptureNotFound, msgCaptureNotFound, ""))
				return
			}
			log.Printf("capture: set secret failed for usage event %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("capture.secret_failed", "capture secret update failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := s.Portal.DeleteCapture(token, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, apierror.Response(codeCaptureNotFound, msgCaptureNotFound, ""))
				return
			}
			log.Printf("capture: delete failed for usage event %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("capture.delete_failed", "capture delete failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}
