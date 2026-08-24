// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"cmp"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultUnpricedEventsLimit is the fallback cap UnpricedUsageEvents applies when
// the caller passes a non-positive limit (mirrors the store's other
// non-positive-limit-defaults convention, e.g. BenchmarkRunsByMapping).
const defaultUnpricedEventsLimit = 500

type Event struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	TokenID       string `json:"token_id"`
	SessionID     string `json:"session_id,omitempty"`
	SessionSource string `json:"session_source,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	APIFlavor     string `json:"api_flavor"`
	Model         string `json:"model"`
	// RequestedModel is the model name exactly as the client sent it, BEFORE
	// resolveModelOverride applied any token model override. Equal to Model when
	// no override fired. "" on rows recorded before migration 61 (unknown).
	RequestedModel string `json:"requested_model"`
	RouteID        string `json:"route_id,omitempty"`
	Provider       string `json:"provider"`
	Host           string `json:"host"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	TotalTokens    int    `json:"total_tokens"`
	// CachedTokens = prompt cache READ tokens; CacheWriteTokens = prompt cache WRITE
	// (Anthropic cache_creation, 0 for OpenAI/Responses). Disjoint from InputTokens
	// here (the accounting split happens in gateway.recordUsage), so
	// input + cached + write + output == total.
	CachedTokens     int     `json:"cached_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	PromptPerSecond  float64 `json:"prompt_per_second"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
	HTTPStatus       int     `json:"http_status"`
	ContentType      string  `json:"content_type"`
	ReqPath          string  `json:"req_path"`
	// ProviderPath is the endpoint PATH the gateway called on the upstream provider
	// (e.g. "/v1/chat/completions" when the built-in translation is used, or the
	// native path "/v1/responses"/"/v1/messages" for native passthrough). It differs
	// from ReqPath (the client-facing path) exactly when translation happened.
	ProviderPath  string `json:"provider_path"`
	ProviderModel string `json:"provider_model"`
	Stream        bool   `json:"stream"`
	TokenName     string `json:"token_name"`
	ServerName    string `json:"server_name"`
	// ServiceID / ServiceName attribute a request to a Service Account (Phase 1
	// service accounts) when it was served by a service token: ServiceID is the
	// service's id, ServiceName its display name at the time of the request
	// (denormalized, mirrors TokenName). Both "" for a user-token/session
	// request (the overwhelming default; existing rows read back "" via the
	// column's default).
	ServiceID   string `json:"service_id,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	// ProjectID / ProjectName attribute a request to a Project (design spec §7)
	// when it was served by a project-attributed USER token: ProjectID is the
	// project's id, ProjectName its display name at the time of the request
	// (denormalized, mirrors ServiceID/ServiceName). Both "" for a token/session
	// with no project attribution (the overwhelming default; existing rows read
	// back "" via the column's default).
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	LatencyMS   int64  `json:"latency_ms"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	// EnergyWh/EnergyMarginalWh/EnergySource are additive P1 storage for a later
	// energy-attribution computation engine. recordUsage does not populate them
	// yet, so every event today carries 0/0/"" (no-op invariant) — a future phase
	// will fill EnergyWh (attributed energy for this request, watt-hours),
	// EnergyMarginalWh (marginal energy vs. an idle baseline, watt-hours), and
	// EnergySource (how it was derived, e.g. "measured"/"estimated").
	EnergyWh         float64   `json:"energy_wh"`
	EnergyMarginalWh float64   `json:"energy_marginal_wh"`
	EnergySource     string    `json:"energy_source"`
	CreatedAt        time.Time `json:"created_at"`
	// CostEUR is a TRANSIENT, derived display field: (EnergyWh/1000) *
	// price(Host), where price is the serving AI-server's own price_per_kwh
	// when set (>0) else the system-wide energy_default_price_per_kwh
	// default (P3 §8). It is computed by the portal layer at response time
	// ONLY (see portal.Service.Usage/UsageStats) — it is NEVER a DB column,
	// NEVER part of usageEventColumns, and NEVER touched by any store
	// scanner (Record/scanUsageEvents/scanUsageRows). A store-returned Event
	// always carries CostEUR==0 until the portal layer sets it.
	CostEUR float64 `json:"cost_eur"`
}

type Store interface {
	// Record persists event. The memory Recorder always returns nil (an
	// in-memory append cannot fail); *store.SQLiteStore returns the real
	// INSERT error instead of swallowing it — a caller that ignores the
	// returned error gets exactly the old silent-drop behavior, but now the
	// error is available to log/propagate.
	Record(event Event) error
	ByUser(userID string) []Event
	All() []Event
	// Query returns one filtered/sorted/paginated page. On success the
	// returned Page is identical to what the pre-error-return signature
	// produced; a non-nil error means the page could not be computed at all
	// (the caller must not treat a zero-value Page as "no matching rows").
	Query(q Query) (Page, error)
	// Stats aggregates the filtered set. See Query for the error contract.
	Stats(q Query) (Stats, error)
	// TimeSeries buckets the filtered set into an activity series. See Query
	// for the error contract.
	TimeSeries(q Query, bucketSecs int) (TimeSeries, error)
	// UpdateUsageEventEnergy sets an event's energy_wh/energy_marginal_wh/energy_source
	// by id. There is no lock column on usage_events — the energy reconciler's
	// idempotency instead comes from UnpricedUsageEvents only ever selecting
	// energy_source=="" events, so a stamped event (even Wh==0, source=="modeled")
	// is never reprocessed. A missing id is a benign no-op (mirrors a 0-row SQL
	// UPDATE), letting the reconciler retry blindly without an existence check.
	UpdateUsageEventEnergy(ctx context.Context, id string, energyWh, marginalWh float64, source string) error
	// UnpricedUsageEvents returns events with energy_source=="" whose CreatedAt
	// falls within [notBefore, notAfter] (both inclusive), oldest-first, capped at
	// limit (a non-positive limit defaults to defaultUnpricedEventsLimit). The
	// reconciler passes notAfter = now-settleDelay (let telemetry catch up) and
	// notBefore = now-backfillWindow (bounded retry horizon).
	UnpricedUsageEvents(ctx context.Context, notBefore, notAfter time.Time, limit int) ([]Event, error)
	// UsageEventsForServerWindow returns events on serverID (matched against
	// Event.Host, which carries the resolved server id — see gateway recordUsage)
	// whose own [CreatedAt-latency, CreatedAt] window overlaps [from, to]. Used by
	// the energy reconciler to reconstruct how many concurrent requests a server
	// was serving during a target event's window.
	UsageEventsForServerWindow(ctx context.Context, serverID string, from, to time.Time) ([]Event, error)
	// EnergyByServer sums EnergyWh grouped by Host (server id) over the SAME
	// filtered set Stats(q) would compute (same predicate as matchUsage/
	// usageWhere) — used by the portal layer to weight the aggregate cost
	// total per server price (see portal.Service.UsageStats). A host with no
	// matching rows is simply absent from the returned map (never a
	// zero-valued entry).
	EnergyByServer(ctx context.Context, q Query) (map[string]float64, error)
	// UsageGroups aggregates the filtered set GROUP BY (dimension, host). groupBy is
	// one of session|server|user|token|model (validated by the caller). Returns one
	// bucket per (group value, host).
	UsageGroups(ctx context.Context, q Query, groupBy string) ([]GroupBucket, error)
}

type Recorder struct {
	mu     sync.RWMutex
	events []Event

	// ResolveUserName, when non-nil, maps a user_id to a human-readable owner
	// name for ScopeAll queries. Default nil -> UserName stays empty.
	ResolveUserName func(userID string) string
}

func NewRecorder() *Recorder {
	return &Recorder{}
}

// Record appends event and always returns nil: an in-memory append cannot
// fail. The error return exists only to satisfy Store — it mirrors
// *store.SQLiteStore, which CAN fail on the underlying INSERT.
func (r *Recorder) Record(event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

// UpdateUsageEventEnergy sets the energy fields of the event matching id. A
// missing id is a benign no-op, mirroring the SQL store (a 0-row UPDATE).
func (r *Recorder) UpdateUsageEventEnergy(_ context.Context, id string, energyWh, marginalWh float64, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.events {
		if r.events[i].ID == id {
			r.events[i].EnergyWh = energyWh
			r.events[i].EnergyMarginalWh = marginalWh
			r.events[i].EnergySource = source
			break
		}
	}
	return nil
}

// UnpricedUsageEvents returns a copy of the un-priced (EnergySource=="") events
// within [notBefore, notAfter], oldest CreatedAt first, capped at limit.
func (r *Recorder) UnpricedUsageEvents(_ context.Context, notBefore, notAfter time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultUnpricedEventsLimit
	}
	r.mu.RLock()
	matched := make([]Event, 0)
	for _, e := range r.events {
		if e.EnergySource != "" {
			continue
		}
		if e.CreatedAt.Before(notBefore) || e.CreatedAt.After(notAfter) {
			continue
		}
		matched = append(matched, e)
	}
	r.mu.RUnlock()

	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].CreatedAt.Before(matched[j].CreatedAt)
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

// UsageEventsForServerWindow returns the events on serverID (Host==serverID)
// whose own [CreatedAt-latency, CreatedAt] window overlaps [from, to]
// (inclusive on both ends, matching a closed-interval overlap test). Unlike the
// SQL store, this scans the full in-memory set directly — no widen-then-filter
// margin is needed since every event is already resident.
func (r *Recorder) UsageEventsForServerWindow(_ context.Context, serverID string, from, to time.Time) ([]Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Event, 0)
	for _, e := range r.events {
		if e.Host != serverID {
			continue
		}
		start := e.CreatedAt.Add(-time.Duration(e.LatencyMS) * time.Millisecond)
		end := e.CreatedAt
		if start.After(to) || end.Before(from) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *Recorder) ByUser(userID string) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Event, 0)
	for _, event := range r.events {
		if event.UserID == userID {
			out = append(out, event)
		}
	}
	return out
}

func (r *Recorder) All() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Query returns one filtered/sorted/paginated page. UserName is resolved only
// for ScopeAll queries and only when ResolveUserName is set. Always returns a
// nil error (an in-memory scan cannot fail) — the error return exists only to
// satisfy Store, whose SQL implementation can fail.
func (r *Recorder) Query(q Query) (Page, error) {
	r.mu.RLock()
	resolver := r.ResolveUserName
	// The owner filter matches the resolved name only where SQLite's users join is
	// present: the ScopeAll list. Own scope and the aggregate match user_id only.
	var nameFor func(string) string
	if q.ScopeAll && resolver != nil {
		nameFor = resolver
	}
	filtered := make([]Event, 0, len(r.events))
	for _, e := range r.events {
		if matchUsage(q, e, nameFor) {
			filtered = append(filtered, e)
		}
	}
	r.mu.RUnlock()

	sortEvents(filtered, NormalizeSort(q.Sort), NormalizeOrder(q.Order))

	total := len(filtered)
	limit := NormalizeLimit(q.Limit)
	page := NormalizePage(q.Page)
	// Bound page against the page count before multiplying. NormalizePage only
	// clamps the lower bound, so an unbounded user-supplied page would overflow
	// int in (page-1)*limit and wrap to a negative start (slice-bounds panic).
	// A page past the last one yields start=total -> empty page (unchanged).
	start := total
	if page <= TotalPages(total, limit) {
		start = (page - 1) * limit
	}
	end := start + limit
	if end > total {
		end = total
	}

	rows := make([]Row, 0, end-start)
	for _, e := range filtered[start:end] {
		row := Row{Event: e}
		if q.ScopeAll && resolver != nil {
			row.UserName = resolver(e.UserID)
		}
		rows = append(rows, row)
	}
	return Page{
		Data:       rows,
		Page:       page,
		Limit:      limit,
		Total:      total,
		TotalPages: TotalPages(total, limit),
	}, nil
}

// Stats aggregates the filtered set: tile totals over ALL rows, plus speed
// histograms over the non-zero values only. Always returns a nil error (an
// in-memory scan cannot fail) — see Query.
func (r *Recorder) Stats(q Query) (Stats, error) {
	r.mu.RLock()
	filtered := make([]Event, 0, len(r.events))
	for _, e := range r.events {
		if matchUsage(q, e, nil) {
			filtered = append(filtered, e)
		}
	}
	r.mu.RUnlock()

	var totals StatTotals
	prompt := make([]float64, 0, len(filtered))
	tokens := make([]float64, 0, len(filtered))
	for _, e := range filtered {
		totals.TotalRequests++
		if IsError(e.Status, e.HTTPStatus) {
			totals.ErrorCount++
		}
		totals.CachedTokens += e.CachedTokens
		totals.CacheWriteTokens += e.CacheWriteTokens
		totals.InputTokens += e.InputTokens
		totals.OutputTokens += e.OutputTokens
		totals.TotalEnergyWh += e.EnergyWh
		prompt = append(prompt, e.PromptPerSecond)
		tokens = append(tokens, e.TokensPerSecond)
	}
	return Stats{
		Totals:          totals,
		PromptPerSecond: ComputeHistogram(prompt),
		TokensPerSecond: ComputeHistogram(tokens),
	}, nil
}

// EnergyByServer sums EnergyWh grouped by Host over the events matching q
// (same matchUsage predicate Stats/Query use), for the portal layer to
// derive a per-server-price-weighted total cost. A host with no matching
// rows is absent from the returned map.
func (r *Recorder) EnergyByServer(_ context.Context, q Query) (map[string]float64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]float64)
	for _, e := range r.events {
		if !matchUsage(q, e, nil) {
			continue
		}
		out[e.Host] += e.EnergyWh
	}
	return out, nil
}

// usageGroupKey returns the group-by value of e for the given dimension, and
// whether the dimension is recognized (session|server|user|token|model).
func usageGroupKey(groupBy string, e Event) (string, bool) {
	switch groupBy {
	case "session":
		return e.SessionID, true
	case "server":
		return e.ServerName, true
	case "user":
		return e.UserID, true
	case "token":
		return e.TokenID, true
	case "model":
		return e.Model, true
	case "service":
		return e.ServiceID, true
	case "project":
		return e.ProjectID, true
	}
	return "", false
}

// UsageGroups aggregates the events matching q (same matchUsage predicate
// Stats/Query use) GROUP BY (dimension, host), mirroring the SQL store. Returns
// one bucket per (group value, host); an unknown groupBy is an error.
func (r *Recorder) UsageGroups(_ context.Context, q Query, groupBy string) ([]GroupBucket, error) {
	if _, ok := usageGroupKey(groupBy, Event{}); !ok {
		return nil, fmt.Errorf("usage groups: invalid group_by %q", groupBy)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	type gk struct{ key, host string }
	acc := make(map[gk]*GroupBucket)
	for _, e := range r.events {
		if !matchUsage(q, e, nil) {
			continue
		}
		key, _ := usageGroupKey(groupBy, e)
		id := gk{key, e.Host}
		b := acc[id]
		if b == nil {
			b = &GroupBucket{Key: key, Host: e.Host, FirstAt: e.CreatedAt, LastAt: e.CreatedAt}
			acc[id] = b
		}
		b.Count++
		if IsError(e.Status, e.HTTPStatus) {
			b.ErrorCount++
		}
		b.InputTokens += e.InputTokens
		b.OutputTokens += e.OutputTokens
		b.CachedTokens += e.CachedTokens
		b.CacheWriteTokens += e.CacheWriteTokens
		b.EnergyWh += e.EnergyWh
		if e.CreatedAt.Before(b.FirstAt) {
			b.FirstAt = e.CreatedAt
		}
		if e.CreatedAt.After(b.LastAt) {
			b.LastAt = e.CreatedAt
		}
	}
	out := make([]GroupBucket, 0, len(acc))
	for _, b := range acc {
		out = append(out, *b)
	}
	return out, nil
}

// TimeSeries buckets the events matching q (From/To window + UserID/ScopeAll
// scoping via matchUsage) into a per-bucket activity series. The caller must set
// q.From/q.To; ComputeTimeSeries guards to<=from (a zero q.To < q.From yields a
// non-nil empty Points). Always returns a nil error (an in-memory scan cannot
// fail) — see Query.
func (r *Recorder) TimeSeries(q Query, bucketSecs int) (TimeSeries, error) {
	r.mu.RLock()
	matched := make([]Event, 0, len(r.events))
	for _, e := range r.events {
		if matchUsage(q, e, nil) {
			matched = append(matched, e)
		}
	}
	r.mu.RUnlock()
	return ComputeTimeSeries(matched, q.From, q.To, bucketSecs), nil
}

// matchUsage reports whether e satisfies q. resolveName, when non-nil, supplies
// the resolved owner name so the Owner filter can match it in addition to
// user_id (the ScopeAll list); pass nil where SQLite would have no users join.
func matchUsage(q Query, e Event, resolveName func(string) string) bool {
	if !q.ScopeAll && e.UserID != q.UserID {
		return false
	}
	if q.HasTokenFilter && e.TokenID != q.TokenID {
		return false
	}
	if q.HasServiceFilter && e.ServiceID != q.ServiceID {
		return false
	}
	if q.Model != "" && !strings.Contains(strings.ToLower(e.Model), strings.ToLower(q.Model)) {
		return false
	}
	if q.Server != "" {
		needle := strings.ToLower(q.Server)
		if !strings.Contains(strings.ToLower(e.ServerName), needle) && !strings.Contains(strings.ToLower(e.Host), needle) {
			return false
		}
	}
	if (q.HasServerExact || q.ServerExact != "") && !strings.EqualFold(e.ServerName, q.ServerExact) {
		return false
	}
	if (q.HasSessionIDExact || q.SessionIDExact != "") && !strings.EqualFold(e.SessionID, q.SessionIDExact) {
		return false
	}
	if (q.HasModelExact || q.ModelExact != "") && !strings.EqualFold(e.Model, q.ModelExact) {
		return false
	}
	// ProjectIDExact: an exact (case-insensitive) drill-down filter (design spec
	// §7/§8), mirroring SessionIDExact/ModelExact -- fires on the presence flag
	// OR a non-empty value, so it can match the EMPTY (no-project) bucket too.
	if (q.HasProjectIDExact || q.ProjectIDExact != "") && !strings.EqualFold(e.ProjectID, q.ProjectIDExact) {
		return false
	}
	// ProjectIDs: the project-scope IN-list applyUsageScope (§8) sets for a
	// non-admin's top-level group-by-project query. nil = no IN-list filter; a
	// non-nil EMPTY slice (a caller who is a member of zero projects) must match
	// ZERO rows rather than being treated as "no filter".
	if q.ProjectIDs != nil {
		if len(q.ProjectIDs) == 0 {
			return false
		}
		matched := false
		for _, id := range q.ProjectIDs {
			if strings.EqualFold(e.ProjectID, id) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	switch q.Status {
	case "error":
		if !IsError(e.Status, e.HTTPStatus) {
			return false
		}
	case "success":
		if IsError(e.Status, e.HTTPStatus) {
			return false
		}
	}
	if q.Q != "" && !usageMatchesText(e, strings.ToLower(q.Q)) {
		return false
	}
	if q.ReqPath != "" && !strings.Contains(strings.ToLower(e.ReqPath), strings.ToLower(q.ReqPath)) {
		return false
	}
	if q.ContentType != "" && !strings.Contains(strings.ToLower(e.ContentType), strings.ToLower(q.ContentType)) {
		return false
	}
	if q.ProviderPath != "" && !strings.Contains(strings.ToLower(e.ProviderPath), strings.ToLower(q.ProviderPath)) {
		return false
	}
	if q.SessionID != "" && !strings.Contains(strings.ToLower(e.SessionID), strings.ToLower(q.SessionID)) {
		return false
	}
	if q.SessionSource != "" && !strings.Contains(strings.ToLower(e.SessionSource), strings.ToLower(q.SessionSource)) {
		return false
	}
	if q.AgentID != "" && !strings.Contains(strings.ToLower(e.AgentID), strings.ToLower(q.AgentID)) {
		return false
	}
	if q.ProviderModel != "" && !strings.Contains(strings.ToLower(e.ProviderModel), strings.ToLower(q.ProviderModel)) {
		return false
	}
	switch q.Stream {
	case "true":
		if !e.Stream {
			return false
		}
	case "false":
		if e.Stream {
			return false
		}
	}
	if q.Owner != "" {
		needle := strings.ToLower(q.Owner)
		match := strings.Contains(strings.ToLower(e.UserID), needle)
		if !match && resolveName != nil {
			match = strings.Contains(strings.ToLower(resolveName(e.UserID)), needle)
		}
		if !match {
			return false
		}
	}
	if !q.From.IsZero() && e.CreatedAt.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && e.CreatedAt.After(q.To) {
		return false
	}
	if !q.TimeFrom.IsZero() && e.CreatedAt.Before(q.TimeFrom) {
		return false
	}
	if !q.TimeTo.IsZero() && e.CreatedAt.After(q.TimeTo) {
		return false
	}
	for id := range NumericColumns {
		v := NumericValue(e, id)
		if minVal, ok := q.NumericMin[id]; ok && v < minVal {
			return false
		}
		if maxVal, ok := q.NumericMax[id]; ok && v > maxVal {
			return false
		}
	}
	return true
}

func usageMatchesText(e Event, needle string) bool {
	return strings.Contains(strings.ToLower(e.ID), needle) ||
		strings.Contains(strings.ToLower(e.Model), needle) ||
		strings.Contains(strings.ToLower(e.RequestedModel), needle) ||
		strings.Contains(strings.ToLower(e.Host), needle) ||
		strings.Contains(strings.ToLower(e.ServerName), needle) ||
		strings.Contains(strings.ToLower(e.TokenName), needle)
}

func sortEvents(events []Event, sortKey, order string) {
	desc := order == "desc"
	sort.SliceStable(events, func(i, j int) bool {
		c := compareUsage(sortKey, events[i], events[j])
		if c == 0 {
			c = cmp.Compare(events[i].ID, events[j].ID)
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
}

func compareUsage(sortKey string, a, b Event) int {
	switch sortKey {
	case "latency_ms":
		return cmp.Compare(a.LatencyMS, b.LatencyMS)
	case "total_tokens":
		return cmp.Compare(a.TotalTokens, b.TotalTokens)
	case "prompt_per_second":
		return cmp.Compare(a.PromptPerSecond, b.PromptPerSecond)
	case "tokens_per_second":
		return cmp.Compare(a.TokensPerSecond, b.TokensPerSecond)
	case "energy_wh":
		return cmp.Compare(a.EnergyWh, b.EnergyWh)
	case "energy_marginal_wh":
		return cmp.Compare(a.EnergyMarginalWh, b.EnergyMarginalWh)
	case "http_status":
		return cmp.Compare(a.HTTPStatus, b.HTTPStatus)
	case "model":
		return cmp.Compare(a.Model, b.Model)
	case "requested_model":
		return cmp.Compare(a.RequestedModel, b.RequestedModel)
	case "server_name":
		return cmp.Compare(a.ServerName, b.ServerName)
	case "token_name":
		return cmp.Compare(a.TokenName, b.TokenName)
	default: // created_at
		return a.CreatedAt.Compare(b.CreatedAt)
	}
}
