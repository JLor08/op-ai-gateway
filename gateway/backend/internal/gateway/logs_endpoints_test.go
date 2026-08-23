// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"strings"
	"testing"
	"time"
)

// newLogsTestServer builds a *Server whose dev token carries the "system" scope
// and whose Logs buffer is seeded with the given records. The bearer secret is
// "dev-secret" (from NewTestServerWithTokenScopes).
func newLogsTestServer(t *testing.T, seed ...logbuffer.Record) *Server {
	t.Helper()
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	buf := logbuffer.NewBuffer(2000, slog.LevelInfo)
	for _, rec := range seed {
		buf.Append(rec)
	}
	srv.Logs = buf
	return srv
}

func TestSystemLogsSnapshot(t *testing.T) {
	seed := logbuffer.Record{Time: time.Now().UTC(), Level: "INFO", Msg: "hello world", Attrs: map[string]any{"server_id": "srv_1"}}
	srv := newLogsTestServer(t, seed)

	req := httptest.NewRequest(http.MethodGet, "/api/system/logs", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto logsSnapshotDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if dto.Level != "info" {
		t.Fatalf("level = %q, want info", dto.Level)
	}
	if len(dto.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(dto.Records))
	}
	if dto.Records[0].Msg != "hello world" {
		t.Fatalf("records[0].msg = %q, want hello world", dto.Records[0].Msg)
	}
	if dto.Records[0].Attrs["server_id"] != "srv_1" {
		t.Fatalf("records[0].attrs[server_id] = %v, want srv_1", dto.Records[0].Attrs["server_id"])
	}
}

func TestSystemLogsSnapshotEmptyRecordsNonNil(t *testing.T) {
	srv := newLogsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/logs", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// records must serialize as [] not null so the frontend can map over it.
	if !strings.Contains(rec.Body.String(), `"records":[]`) {
		t.Fatalf("body = %s, want records:[]", rec.Body.String())
	}
}

func TestSystemLogLevelGetAndSet(t *testing.T) {
	srv := newLogsTestServer(t)

	// GET returns the current level.
	getReq := httptest.NewRequest(http.MethodGet, "/api/system/logs/level", nil)
	getReq.Header.Set("Authorization", "Bearer dev-secret")
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var got logLevelDTO
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if got.Level != "info" {
		t.Fatalf("GET level = %q, want info", got.Level)
	}

	// PUT debug flips the live level.
	putReq := httptest.NewRequest(http.MethodPut, "/api/system/logs/level", strings.NewReader(`{"level":"debug"}`))
	putReq.Header.Set("Authorization", "Bearer dev-secret")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", putRec.Code, putRec.Body.String())
	}
	var put logLevelDTO
	if err := json.Unmarshal(putRec.Body.Bytes(), &put); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if put.Level != "debug" {
		t.Fatalf("PUT level = %q, want debug", put.Level)
	}
	if srv.Logs.Level() != slog.LevelDebug {
		t.Fatalf("buffer level = %v, want debug", srv.Logs.Level())
	}

	// GET now reflects debug.
	getReq2 := httptest.NewRequest(http.MethodGet, "/api/system/logs/level", nil)
	getReq2.Header.Set("Authorization", "Bearer dev-secret")
	getRec2 := httptest.NewRecorder()
	srv.ServeHTTP(getRec2, getReq2)
	var got2 logLevelDTO
	if err := json.Unmarshal(getRec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("unmarshal GET2: %v", err)
	}
	if got2.Level != "debug" {
		t.Fatalf("GET2 level = %q, want debug", got2.Level)
	}
}

func TestSystemLogLevelAcceptsTrace(t *testing.T) {
	srv := newLogsTestServer(t)
	putReq := httptest.NewRequest(http.MethodPut, "/api/system/logs/level", strings.NewReader(`{"level":"trace"}`))
	putReq.Header.Set("Authorization", "Bearer dev-secret")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", putRec.Code, putRec.Body.String())
	}
	var got logLevelDTO
	if err := json.Unmarshal(putRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Level != "trace" {
		t.Fatalf("level = %q, want trace", got.Level)
	}
	if srv.Logs.Level() != logbuffer.LevelTrace {
		t.Fatalf("buffer level = %v, want LevelTrace", srv.Logs.Level())
	}
}

func TestSystemLogLevelRejectsUnknown(t *testing.T) {
	srv := newLogsTestServer(t)
	putReq := httptest.NewRequest(http.MethodPut, "/api/system/logs/level", strings.NewReader(`{"level":"verbose"}`))
	putReq.Header.Set("Authorization", "Bearer dev-secret")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", putRec.Code, putRec.Body.String())
	}
	if srv.Logs.Level() != slog.LevelInfo {
		t.Fatalf("buffer level = %v, want unchanged info", srv.Logs.Level())
	}
}

func TestSystemLogsForbiddenForNonSystem(t *testing.T) {
	// NewTestServer's token has gateway:use + admin but NOT system.
	srv := NewTestServer()
	srv.Logs = logbuffer.NewBuffer(2000, slog.LevelInfo)
	for _, path := range []string{"/api/system/logs", "/api/system/logs/level"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403, body = %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestSystemLogsNilBufferReturns500(t *testing.T) {
	// A system-scoped token but no Logs buffer wired -> 500 (not a panic).
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	req := httptest.NewRequest(http.MethodGet, "/api/system/logs", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSystemLogEventsSnapshotAndRecord(t *testing.T) {
	srv := newLogsTestServer(t, logbuffer.Record{Time: time.Now().UTC(), Level: "INFO", Msg: "seed line"})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/system/logs/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	// First frame: snapshot with the seeded ring + level.
	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap logsSnapshotDTO
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if snap.Level != "info" {
		t.Fatalf("snapshot level = %q, want info", snap.Level)
	}
	if len(snap.Records) != 1 || snap.Records[0].Msg != "seed line" {
		t.Fatalf("snapshot records = %#v, want one seed line", snap.Records)
	}

	// A live Append arrives as a `record` frame.
	srv.Logs.Append(logbuffer.Record{Time: time.Now().UTC(), Level: "WARN", Msg: "live line"})
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "record" {
		t.Fatalf("delta event = %q, want record", event)
	}
	var rec logbuffer.Record
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatalf("unmarshal record: %v (%s)", err, data)
	}
	if rec.Msg != "live line" || rec.Level != "WARN" {
		t.Fatalf("record = %#v, want live line WARN", rec)
	}
}
