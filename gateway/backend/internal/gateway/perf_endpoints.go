// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"time"
)

// serverPerfWindows maps the wire `window=` token to the lookback duration for
// GET /api/portal/servers/{id}/perf (From=now-window, To=now). A missing or
// unknown token falls back to defaultServerPerfWindow.
var serverPerfWindows = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"1d":  24 * time.Hour,
}

// defaultServerPerfWindow is used when `window=` is missing or unknown.
const defaultServerPerfWindow = 15 * time.Minute

// resolveServerPerfWindow maps a raw `window=` token to its duration, defaulting
// to defaultServerPerfWindow for a missing/unknown value.
func resolveServerPerfWindow(token string) time.Duration {
	if d, ok := serverPerfWindows[token]; ok {
		return d
	}
	return defaultServerPerfWindow
}

// perfGPUDTO is one GPU's slice of a telemetry point.
type perfGPUDTO struct {
	Index         int     `json:"index"`
	Name          string  `json:"name"`
	UUID          string  `json:"uuid"`
	UtilPct       float64 `json:"util_pct"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	TempC         int     `json:"temp_c"`
	VRAMTempC     int     `json:"vram_temp_c"`
	PowerW        float64 `json:"power_w"`
	FanPct        float64 `json:"fan_pct"`
}

// perfNetDTO is one nic's counters of a telemetry point.
type perfNetDTO struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

// perfPointDTO renders one telemetry sample for the chart: host scalars plus the
// per-GPU and per-nic slices. `t` is the sample timestamp at millisecond
// resolution (see perfPointTimeLayout).
type perfPointDTO struct {
	T              string       `json:"t"`
	CPUUtilPct     float64      `json:"cpu_util_pct"`
	CPUCores       []float64    `json:"cpu_cores"`
	MemUsedBytes   int64        `json:"mem_used_bytes"`
	MemTotalBytes  int64        `json:"mem_total_bytes"`
	SwapUsedBytes  int64        `json:"swap_used_bytes"`
	SwapTotalBytes int64        `json:"swap_total_bytes"`
	Load1          float64      `json:"load1"`
	Load5          float64      `json:"load5"`
	Load15         float64      `json:"load15"`
	ActiveRequests int          `json:"active_requests"`
	QueueDepth     int          `json:"queue_depth"`
	GPUs           []perfGPUDTO `json:"gpus"`
	Net            []perfNetDTO `json:"net"`
	// Nullable host power watts; nil marshals to JSON null ("not measured").
	CPUPowerW    *float64 `json:"cpu_power_w"`
	SystemPowerW *float64 `json:"system_power_w"`
	// Nullable CPU package temperature (°C); nil marshals to JSON null ("not measured").
	CPUTempC *float64 `json:"cpu_temp_c"`
}

// perfHistoryDTO is the GET /perf body: the decimated window of points plus its
// [from,to] RFC3339 bounds.
type perfHistoryDTO struct {
	Points []perfPointDTO `json:"points"`
	From   string         `json:"from"`
	To     string         `json:"to"`
}

// perfSnapshotDTO is the initial SSE `snapshot` frame carrying the current ring.
type perfSnapshotDTO struct {
	Points []perfPointDTO `json:"points"`
}

// perfPointTimeLayout renders a telemetry sample timestamp at millisecond
// resolution (RFC3339 with a fixed 3-digit fractional second). The agent's
// default collection cadence is 1s, so whole-second (RFC3339) resolution would
// make two consecutive samples indistinguishable on the wire; millisecond
// resolution keeps them distinct for downstream consumers (e.g. the energy
// reconciler integrating power over a request's time window).
const perfPointTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// perfPointFromSample maps a routing.TelemetrySample to its wire point, always
// returning non-nil gpus/net slices so they serialize to [] not null.
func perfPointFromSample(sample routing.TelemetrySample) perfPointDTO {
	gpus := make([]perfGPUDTO, 0, len(sample.GPUs))
	for _, g := range sample.GPUs {
		gpus = append(gpus, perfGPUDTO{
			Index:         g.Index,
			Name:          g.Name,
			UUID:          g.UUID,
			UtilPct:       g.UtilPct,
			MemUsedBytes:  g.MemUsedBytes,
			MemTotalBytes: g.MemTotalBytes,
			TempC:         g.TempC,
			VRAMTempC:     g.VRAMTempC,
			PowerW:        g.PowerW,
			FanPct:        g.FanPct,
		})
	}
	net := make([]perfNetDTO, 0, len(sample.Net))
	for _, n := range sample.Net {
		net = append(net, perfNetDTO{Name: n.Name, RxBytes: n.RxBytes, TxBytes: n.TxBytes})
	}
	cores := sample.CPUCores
	if cores == nil {
		cores = []float64{} // never serialize null (DB-history rows carry no per-core)
	}
	return perfPointDTO{
		T:              sample.ReportedAt.UTC().Format(perfPointTimeLayout),
		CPUUtilPct:     sample.CPUUtilPct,
		CPUCores:       cores,
		MemUsedBytes:   sample.MemUsedBytes,
		MemTotalBytes:  sample.MemTotalBytes,
		SwapUsedBytes:  sample.SwapUsedBytes,
		SwapTotalBytes: sample.SwapTotalBytes,
		Load1:          sample.Load1,
		Load5:          sample.Load5,
		Load15:         sample.Load15,
		ActiveRequests: sample.ActiveRequests,
		QueueDepth:     sample.QueueDepth,
		GPUs:           gpus,
		Net:            net,
		CPUPowerW:      sample.CPUPowerW,
		SystemPowerW:   sample.SystemPowerW,
		CPUTempC:       sample.CPUTempC,
	}
}

// perfPointsFromSamples maps a slice of samples to their wire points, preserving
// order and always returning a non-nil slice.
func perfPointsFromSamples(samples []routing.TelemetrySample) []perfPointDTO {
	points := make([]perfPointDTO, 0, len(samples))
	for _, sample := range samples {
		points = append(points, perfPointFromSample(sample))
	}
	return points
}

// handleServerPerfHistory serves GET /api/portal/servers/{id}/perf: the decimated
// persisted telemetry window for the resolved ?window=. Owner/admin-gated through
// Portal.ServerPerfHistory -> authorizeServer (404 no existence leak).
func (s *Server) handleServerPerfHistory(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	window := resolveServerPerfWindow(r.URL.Query().Get("window"))
	samples, err := s.Portal.ServerPerfHistory(r.Context(), token, serverID, window)
	if err != nil {
		writePortalServerError(w, err, "server.perf_failed")
		return
	}
	to := time.Now().UTC()
	from := to.Add(-window)
	writeJSON(w, http.StatusOK, perfHistoryDTO{
		Points: perfPointsFromSamples(samples),
		From:   from.Format(time.RFC3339),
		To:     to.Format(time.RFC3339),
	})
}

// handleServerPerfEvents streams GET /api/portal/servers/{id}/perf/events over
// SSE: a `snapshot` frame with the current ring, then a `sample` frame per live
// telemetry publish, with a 25s heartbeat. Gated through Portal.GetServer (same
// authorizeServer 404-no-leak as history) before any stream bytes are written.
func (s *Server) handleServerPerfEvents(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, err := s.Portal.GetServer(r.Context(), token, serverID); err != nil {
		writePortalServerError(w, err, "server.perf_failed")
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

	snapshot, ch, unsub := s.ServerPerf.subscribe(serverID)
	defer unsub()
	if !writePerfEvent(w, flusher, "snapshot", perfSnapshotDTO{Points: perfPointsFromSamples(snapshot)}) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case sample, open := <-ch:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "sample", perfPointFromSample(sample)) {
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

// handleBenchmarkEvents streams GET /api/portal/servers/{id}/benchmark/events over
// SSE: a `snapshot` frame with the server's current benchmark status, then a
// `progress` frame per runner publish (after each measured mapping and the terminal
// finish), with a 25s heartbeat. Gated through Portal.GetServer (same authorizeServer
// 404-no-leak as the status endpoint) before any stream bytes are written. Mirrors
// handleServerPerfEvents exactly.
func (s *Server) handleBenchmarkEvents(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, err := s.Portal.GetServer(r.Context(), token, serverID); err != nil {
		writePortalServerError(w, err, "server.not_found")
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

	snap, ch, unsub := s.Benchmarks.Subscribe(serverID)
	defer unsub()
	if !writePerfEvent(w, flusher, "snapshot", snap) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case status, open := <-ch:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "progress", status) {
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

// writePerfEvent serializes payload as an SSE frame (`event: <name>` + `data:
// <json>`) and flushes. Returns false on a marshal/write failure so the caller
// can stop the stream (mirrors writeRunEvent).
func writePerfEvent(w http.ResponseWriter, flusher http.Flusher, name string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := io.WriteString(w, "event: "+name+"\ndata: "+string(data)+"\n\n"); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
