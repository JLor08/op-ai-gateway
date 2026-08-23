// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"time"
)

// NoTokenWire is the wire sentinel the Activity token filter sends to select
// rows with an empty token_id (chat / no-token usage). parseUsageQuery and
// parseUsageTimeSeriesQuery translate it to TokenID="" with HasTokenFilter=true.
const NoTokenWire = "__none__"

// parseUsageQuery maps the raw HTTP query params to a usage.Query, clamping every
// field to its whitelist/default and mapping range -> From. It records the scope INTENT
// (ScopeAll = scope=="all"); authority is enforced later in Service.Usage/UsageStats.
func parseUsageQuery(r *http.Request, now time.Time) usage.Query {
	q := r.URL.Query()
	out := usage.Query{
		Model:         strings.TrimSpace(q.Get("model")),
		Server:        strings.TrimSpace(q.Get("server")),
		Q:             strings.TrimSpace(q.Get("q")),
		Owner:         strings.TrimSpace(q.Get("owner")),
		ReqPath:       strings.TrimSpace(q.Get("req_path")),
		ContentType:   strings.TrimSpace(q.Get("content_type")),
		ProviderModel: strings.TrimSpace(q.Get("provider_model")),
		ProviderPath:  strings.TrimSpace(q.Get("provider_path")),
		SessionID:     strings.TrimSpace(q.Get("session_id")),
		SessionSource: strings.TrimSpace(q.Get("session_source")),
		AgentID:       strings.TrimSpace(q.Get("agent_id")),

		GroupBy: strings.TrimSpace(q.Get("group_by")),
	}

	// Exact filters used to expand a group into its member rows. Parsed with
	// presence flags + a __empty__ sentinel so expanding the EMPTY-key group (e.g.
	// the empty-session bucket) matches only empty-value rows rather than dropping
	// the (empty) value and applying no filter. (Token uses its own NoTokenWire
	// sentinel + HasTokenFilter, below.)
	if q.Has("server_exact") {
		out.HasServerExact = true
		if raw := q.Get("server_exact"); raw != "__empty__" {
			out.ServerExact = strings.TrimSpace(raw)
		}
	}
	if q.Has("session_id_exact") {
		out.HasSessionIDExact = true
		if raw := q.Get("session_id_exact"); raw != "__empty__" {
			out.SessionIDExact = strings.TrimSpace(raw)
		}
	}
	if q.Has("model_exact") {
		out.HasModelExact = true
		if raw := q.Get("model_exact"); raw != "__empty__" {
			out.ModelExact = strings.TrimSpace(raw)
		}
	}
	if q.Has("project_id_exact") {
		out.HasProjectIDExact = true
		if raw := q.Get("project_id_exact"); raw != "__empty__" {
			out.ProjectIDExact = strings.TrimSpace(raw)
		}
	}

	// Tri-state stream filter: only "true"/"false" set a filter; anything else
	// (absent/empty/junk) leaves Stream empty = no filter.
	switch q.Get("stream") {
	case "true", "false":
		out.Stream = q.Get("stream")
	}

	// Extra created_at bounds (ANDed with the range-derived From). Accept RFC3339
	// and the HTML datetime-local short forms; unparseable values are ignored.
	if t, ok := parseUsageBoundTime(q.Get("time_from")); ok {
		out.TimeFrom = t
	}
	if t, ok := parseUsageBoundTime(q.Get("time_to")); ok {
		out.TimeTo = t
	}

	// Per-column numeric range filters: <col>_min / <col>_max over the whitelist.
	for id := range usage.NumericColumns {
		if v, ok := parseUsageFloat(q.Get(id + "_min")); ok {
			if out.NumericMin == nil {
				out.NumericMin = make(map[string]float64)
			}
			out.NumericMin[id] = v
		}
		if v, ok := parseUsageFloat(q.Get(id + "_max")); ok {
			if out.NumericMax == nil {
				out.NumericMax = make(map[string]float64)
			}
			out.NumericMax[id] = v
		}
	}

	page, _ := strconv.Atoi(q.Get("page"))
	out.Page = usage.NormalizePage(page)

	limit, _ := strconv.Atoi(q.Get("limit"))
	out.Limit = usage.NormalizeLimit(limit)

	out.Sort = usage.NormalizeSort(q.Get("sort"))
	out.Order = usage.NormalizeOrder(q.Get("order"))

	switch q.Get("status") {
	case "success", "error":
		out.Status = q.Get("status")
	}

	// range -> From (To stays zero = open end). all = no lower bound.
	switch q.Get("range") {
	case "24h":
		out.From = now.Add(-24 * time.Hour)
	case "7d":
		out.From = now.Add(-7 * 24 * time.Hour)
	case "all":
		// leave From zero
	default: // "30d" and any unknown value
		out.From = now.Add(-30 * 24 * time.Hour)
	}

	// Token filter: presence of token_id turns it on; the NoTokenWire sentinel
	// selects rows with an empty token_id (chat). FilterUserID is the admin-only
	// user pin, honored later in applyUsageScope.
	if q.Has("token_id") {
		out.HasTokenFilter = true
		if tid := q.Get("token_id"); tid != NoTokenWire {
			out.TokenID = tid
		}
	}
	// Service filter (Phase 1 service accounts): presence of service_id turns it
	// on; "" is a valid value meaning "rows with an empty service_id" (user-token/
	// session usage). Mirrors the token filter above (no wire sentinel needed —
	// unlike token_id, an empty service_id is directly representable on the wire).
	if q.Has("service_id") {
		out.HasServiceFilter = true
		out.ServiceID = strings.TrimSpace(q.Get("service_id"))
	}
	out.FilterUserID = strings.TrimSpace(q.Get("user_id"))

	// Scope intent only; Service.Usage enforces HasScope("admin").
	out.ScopeAll = q.Get("scope") == "all"
	return out
}

// usageBoundTimeLayouts is the accepted time_from/time_to formats: RFC3339 first
// (carries its own zone), then the two HTML datetime-local short forms parsed in
// UTC.
var usageBoundTimeLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04"}

// parseUsageBoundTime parses a time_from/time_to value, returning ok=false for an
// empty or unparseable value (so the caller leaves the bound zero = no filter).
func parseUsageBoundTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range usageBoundTimeLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseUsageFloat parses a numeric filter bound, returning ok=false for an empty
// or unparseable value.
func parseUsageFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// usageTimeSeriesWindows maps the wire `window=` token to the lookback duration
// used for From=now-window (To=now). A missing/unknown token falls back to the
// default ("5m"). Durations are the wall-clock spans the frontend chart controls
// offer, from 5 minutes up to a full year.
var usageTimeSeriesWindows = map[string]time.Duration{
	"5m":  5 * time.Minute,      // 300s
	"15m": 15 * time.Minute,     // 900s
	"30m": 30 * time.Minute,     // 1800s
	"1h":  time.Hour,            // 3600s
	"6h":  6 * time.Hour,        // 21600s
	"12h": 12 * time.Hour,       // 43200s
	"1d":  24 * time.Hour,       // 86400s
	"1w":  7 * 24 * time.Hour,   // 604800s
	"2w":  14 * 24 * time.Hour,  // 1209600s
	"1mo": 30 * 24 * time.Hour,  // 2592000s
	"3mo": 90 * 24 * time.Hour,  // 7776000s
	"6mo": 180 * 24 * time.Hour, // 15552000s
	"1y":  365 * 24 * time.Hour, // 31536000s
}

// defaultUsageTimeSeriesWindow is used when `window=` is missing or unknown.
const defaultUsageTimeSeriesWindow = 5 * time.Minute

// usageTimeSeriesBuckets is the allowed `bucket=` set in seconds. A missing,
// non-integer, or out-of-set value falls back to defaultUsageTimeSeriesBucket.
// The set spans 1s up to a full month so a coarse window still has a sensible
// requested resolution (ComputeTimeSeries additionally coarsens when the
// resulting bucket count would be excessive).
var usageTimeSeriesBuckets = map[int]bool{
	1: true, 5: true, 10: true, 30: true, 60: true, 180: true, 900: true,
	3600: true, 21600: true, 43200: true, 86400: true, 604800: true,
	1209600: true, 2592000: true,
}

// defaultUsageTimeSeriesBucket is used when `bucket=` is missing/unknown.
const defaultUsageTimeSeriesBucket = 5

// parseUsageTimeSeriesQuery maps the raw HTTP query to a usage.Query plus the
// bucket size (seconds) for the activity time-series charts. Unlike the table list,
// only the shared chart controls are honored — window, bucket, and scope; the
// per-column text filters (model/server/status/q) do not apply. window is looked up
// in usageTimeSeriesWindows -> From=now-window, To=now (default 5m); bucket is an
// integer second count validated against usageTimeSeriesBuckets (default 5). It
// records the scope INTENT only; authority is enforced in Service.UsageTimeSeries.
func parseUsageTimeSeriesQuery(r *http.Request, now time.Time) (usage.Query, int) {
	q := r.URL.Query()

	window, ok := usageTimeSeriesWindows[q.Get("window")]
	if !ok {
		window = defaultUsageTimeSeriesWindow
	}

	bucketSecs := defaultUsageTimeSeriesBucket
	if n, err := strconv.Atoi(q.Get("bucket")); err == nil && usageTimeSeriesBuckets[n] {
		bucketSecs = n
	}

	out := usage.Query{
		From:     now.Add(-window),
		To:       now,
		ScopeAll: q.Get("scope") == "all",
	}
	if q.Has("token_id") {
		out.HasTokenFilter = true
		if tid := q.Get("token_id"); tid != NoTokenWire {
			out.TokenID = tid
		}
	}
	if q.Has("service_id") {
		out.HasServiceFilter = true
		out.ServiceID = strings.TrimSpace(q.Get("service_id"))
	}
	out.FilterUserID = strings.TrimSpace(q.Get("user_id"))
	out.Server = strings.TrimSpace(q.Get("server"))
	out.ServerExact = strings.TrimSpace(q.Get("server_exact"))
	return out, bucketSecs
}

func (s *Server) handlePortalUsageTimeSeries(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	q, bucketSecs := parseUsageTimeSeriesQuery(r, time.Now().UTC())
	series, err := s.Portal.UsageTimeSeries(token, q, bucketSecs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.timeseries_failed", "usage timeseries failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, series)
}

func (s *Server) handlePortalUsageStats(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	stats, err := s.Portal.UsageStats(token, parseUsageQuery(r, time.Now().UTC()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.stats_failed", "usage stats failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handlePortalUsageGroups(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	q := parseUsageQuery(r, time.Now().UTC())
	groups, err := s.Portal.UsageGroups(token, q, q.GroupBy)
	if err != nil {
		if errors.Is(err, portal.ErrUsageGroupByInvalid) {
			writeJSON(w, http.StatusBadRequest, apierror.Response("usage.group_by_invalid", "group_by must be session, server, user, token or model", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.groups_failed", "usage groups failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": groups, "group_by": q.GroupBy})
}

func (s *Server) handlePortalUsageEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
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
	// Clear the 30s WriteTimeout that newHTTPServer arms at request start; a long-lived
	// SSE response would otherwise be cut after ~30s. httptest has no deadline, so this
	// is unverifiable in a unit test — TestPortalUsageEventsSurvivesServerWriteTimeout
	// exercises it against a real net/http server. Best-effort: an unsupported writer
	// (httptest) returns an error we ignore.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	flusher.Flush() // flush headers so the client's onopen fires immediately

	signal := s.UsageEvents.Register()
	defer s.UsageEvents.Unregister(signal)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
			if _, err := io.WriteString(w, "event: activity\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
