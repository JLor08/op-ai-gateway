// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/logbuffer"
	"strings"
	"time"
)

const (
	codeLogsUnavailable = "logs.unavailable"
	msgLogsUnavailable  = "log buffer unavailable"
)

// logsSnapshotDTO is the GET /api/system/logs body AND the SSE `snapshot` frame:
// the current ring plus the live level. Records is always non-nil so it
// serializes as [] not null.
type logsSnapshotDTO struct {
	Records []logbuffer.Record `json:"records"`
	Level   string             `json:"level"`
}

// logLevelDTO is the GET/PUT /api/system/logs/level body: the current wire level
// string ("debug"|"info"|"warn"|"error").
type logLevelDTO struct {
	Level string `json:"level"`
}

// setLogLevelRequest is the PUT /api/system/logs/level request body.
type setLogLevelRequest struct {
	Level string `json:"level"`
}

// validLogLevel reports whether s is one of the accepted wire levels
// (case-insensitive). ParseLevel defaults unknown input to Info, so the PUT
// handler validates the raw string here to reject an unknown level with 400.
func validLogLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "info", "warn", "error":
		return true
	}
	return false
}

// recordsOrEmpty returns a non-nil records slice so the JSON body carries [] not
// null (the frontend maps over it directly).
func recordsOrEmpty(recs []logbuffer.Record) []logbuffer.Record {
	if recs == nil {
		return []logbuffer.Record{}
	}
	return recs
}

// handleSystemLogs serves GET /api/system/logs: a snapshot of the log ring plus
// the current level. System-scope only; 500 if the Logs buffer is not wired.
func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if s.Logs == nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(codeLogsUnavailable, msgLogsUnavailable, ""))
		return
	}
	writeJSON(w, http.StatusOK, logsSnapshotDTO{
		Records: recordsOrEmpty(s.Logs.Snapshot()),
		Level:   logbuffer.LevelString(s.Logs.Level()),
	})
}

// handleSystemLogLevel serves GET/PUT /api/system/logs/level. GET returns the
// current level; PUT with body {level} sets the live level (rejecting an unknown
// level with 400) and returns the new level. System-scope only; 500 if the Logs
// buffer is not wired.
func (s *Server) handleSystemLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		return
	}
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if s.Logs == nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(codeLogsUnavailable, msgLogsUnavailable, ""))
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, logLevelDTO{Level: logbuffer.LevelString(s.Logs.Level())})
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req setLogLevelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if !validLogLevel(req.Level) {
		writeJSON(w, http.StatusBadRequest, apierror.Response("logs.invalid_level", "level must be one of trace, debug, info, warn, error", ""))
		return
	}
	s.Logs.SetLevel(logbuffer.ParseLevel(req.Level))
	writeJSON(w, http.StatusOK, logLevelDTO{Level: logbuffer.LevelString(s.Logs.Level())})
}

// handleSystemLogEvents streams GET /api/system/logs/events over SSE: a
// `snapshot` frame with the current ring + level, then a `record` frame per live
// Append, with a 25s heartbeat. System-scope only (checked before any stream
// bytes are written); 500 if the Logs buffer is not wired. Mirrors
// handleServerPerfEvents (flusher guard + write-deadline clear + heartbeat).
func (s *Server) handleSystemLogEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if s.Logs == nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(codeLogsUnavailable, msgLogsUnavailable, ""))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.stream_unsupported", "streaming unsupported", ""))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Clear the server WriteTimeout for this long-lived response. An unsupported
	// writer (httptest recorder) returns an error we intentionally ignore.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	snapshot, ch, unsub := s.Logs.Subscribe()
	defer unsub()
	if !writePerfEvent(w, flusher, "snapshot", logsSnapshotDTO{
		Records: recordsOrEmpty(snapshot),
		Level:   logbuffer.LevelString(s.Logs.Level()),
	}) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case rec, open := <-ch:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "record", rec) {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
