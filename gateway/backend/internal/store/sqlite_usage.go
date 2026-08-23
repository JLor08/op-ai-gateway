// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultUnpricedEventsLimit is the fallback cap UnpricedUsageEvents applies when
// the caller passes a non-positive limit (mirrors the store's other
// non-positive-limit-defaults convention, e.g. BenchmarkRunsByMapping, and the
// memory usage.Recorder's identical constant).
const defaultUnpricedEventsLimit = 500

// usageEventsFromClause is the shared "from" fragment appended to the several
// hand-built SELECTs below that read the usage_events table aliased as "e".
const usageEventsFromClause = " from usage_events as e"

// energyMaxRequestWindow bounds how far UsageEventsForServerWindow widens its
// underlying created_at fetch on BOTH sides of [from,to], to catch a
// long-running sibling request whose OWN [created_at-latency, created_at]
// window overlaps [from,to] without being contained in it. Postgres/SQLite
// interval arithmetic differs, so rather than expressing
// "created_at - latency_ms/1000 seconds <= to" in SQL, the query fetches every
// row created in [from-window, to+window] and the exact
// [created_at-latency, created_at] overlap-vs-[from,to] test runs in Go
// afterward. The upper widen matters just as much as the lower one: a request
// that STARTS inside [from,to] but is still running past `to` has
// created_at > to yet still overlaps (start <= to) — dropping it would
// under-count concurrency for the later-finishing half of every overlapping
// pair. A request whose own window starts more than this long before `from`,
// OR ends more than this long after `to`, is excluded from a foreign window's
// concurrency reconstruction — accepted, log-noted by the energy reconciler (a
// request that long-running is already an outlier for per-request energy
// attribution).
const energyMaxRequestWindow = 30 * time.Minute

// Record/ByUser/All/TimeSeries/Query/Stats implement usage.Recorder, whose
// methods take no context; they route through the ctx-shaped s.exec/s.query/
// s.queryRow helpers with context.Background(), which is behavior-identical
// to the plain sql.DB.Exec/Query/QueryRow they replace (those simply forward
// to the *Context variant with context.Background() internally) while still
// running the query through the dialect's rebind.
// Record persists event, returning the real INSERT error (previously
// swallowed into the lastUsageError side-channel only) so a caller can react
// instead of the event silently vanishing from billing/energy accounting.
func (s *SQLiteStore) Record(event usage.Event) error {
	_, err := s.exec(context.Background(), `
		insert into usage_events (
			id, request_id, user_id, token_id, session_id, session_source, agent_id, api_flavor, model, requested_model,
			route_id, provider, host, status, error_code, input_tokens, output_tokens,
			total_tokens, latency_ms, cached_tokens, cache_write_tokens, prompt_per_second, tokens_per_second,
			http_status, content_type, req_path, provider_path, provider_model, stream, token_name,
			server_name, service_id, service_name, project_id, project_name, energy_wh, energy_marginal_wh, energy_source, created_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		event.ID,
		event.UserID,
		event.TokenID,
		event.SessionID,
		event.SessionSource,
		event.AgentID,
		event.APIFlavor,
		event.Model,
		event.RequestedModel,
		event.RouteID,
		event.Provider,
		event.Host,
		event.Status,
		event.ErrorCode,
		event.InputTokens,
		event.OutputTokens,
		event.TotalTokens,
		event.LatencyMS,
		event.CachedTokens,
		event.CacheWriteTokens,
		event.PromptPerSecond,
		event.TokensPerSecond,
		event.HTTPStatus,
		event.ContentType,
		event.ReqPath,
		event.ProviderPath,
		event.ProviderModel,
		event.Stream,
		event.TokenName,
		event.ServerName,
		event.ServiceID,
		event.ServiceName,
		event.ProjectID,
		event.ProjectName,
		event.EnergyWh,
		event.EnergyMarginalWh,
		event.EnergySource,
		event.CreatedAt,
	)
	s.setLastUsageError("record", err)
	if err != nil {
		return fmt.Errorf("record usage event: %w", err)
	}
	return nil
}

// UpdateUsageEventEnergy sets an event's energy_wh/energy_marginal_wh/
// energy_source columns by id. usage_events has no lock column — the energy
// reconciler's idempotency instead comes from UnpricedUsageEvents only ever
// selecting energy_source="" rows, so a stamped row (even Wh=0,
// source="modeled") is never reprocessed. A missing id matches 0 rows, a
// benign no-op (not an error), letting the reconciler retry blindly without an
// existence check.
func (s *SQLiteStore) UpdateUsageEventEnergy(ctx context.Context, id string, energyWh, marginalWh float64, source string) error {
	_, err := s.exec(ctx, `
		update usage_events
		set energy_wh = ?, energy_marginal_wh = ?, energy_source = ?
		where id = ?`,
		energyWh, marginalWh, source, id,
	)
	if err != nil {
		return fmt.Errorf("update usage event energy: %w", err)
	}
	return nil // 0 rows affected (missing id) is a benign no-op
}

// UnpricedUsageEvents returns the un-priced (energy_source="") events whose
// created_at falls in [notBefore, notAfter] (both inclusive), oldest-first,
// capped at limit (a non-positive limit defaults to defaultUnpricedEventsLimit).
// The reconciler passes notAfter = now-settleDelay (let telemetry samples catch
// up before attributing energy) and notBefore = now-backfillWindow (a bounded
// retry horizon).
func (s *SQLiteStore) UnpricedUsageEvents(ctx context.Context, notBefore, notAfter time.Time, limit int) ([]usage.Event, error) {
	if limit <= 0 {
		limit = defaultUnpricedEventsLimit
	}
	rows, err := s.query(ctx, `
		select `+usageEventColumns+`
		from usage_events as e
		where e.energy_source = '' and e.created_at >= ? and e.created_at <= ?
		order by e.created_at asc, e.id asc
		limit ?`, notBefore, notAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("unpriced usage events: %w", err)
	}
	defer rows.Close()
	events, err := scanUsageEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("unpriced usage events: %w", err)
	}
	return events, nil
}

// UsageEventsForServerWindow returns the events on serverID (matched against
// Event.Host, which the gateway sets to the resolved server id — see
// recordUsage) whose own [created_at-latency, created_at] window overlaps
// [from, to]. Used by the energy reconciler to reconstruct how many concurrent
// requests a server was serving during a target event's window.
//
// To stay dialect-neutral (sqlite and postgres differ on interval arithmetic),
// the underlying fetch widens BOTH bounds by energyMaxRequestWindow — the lower
// bound so a sibling that started before `from` but is still running is not
// missed, and the UPPER bound so a sibling that starts inside [from,to] but is
// still running past `to` (created_at > to, yet start <= to still overlaps) is
// not missed either — and the exact overlap test runs in Go over the fetched
// rows.
func (s *SQLiteStore) UsageEventsForServerWindow(ctx context.Context, serverID string, from, to time.Time) ([]usage.Event, error) {
	rows, err := s.query(ctx, `
		select `+usageEventColumns+`
		from usage_events as e
		where e.host = ? and e.created_at >= ? and e.created_at <= ?
		order by e.created_at asc, e.id asc`,
		serverID, from.Add(-energyMaxRequestWindow), to.Add(energyMaxRequestWindow))
	if err != nil {
		return nil, fmt.Errorf("usage events for server window: %w", err)
	}
	defer rows.Close()
	fetched, err := scanUsageEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("usage events for server window: %w", err)
	}

	out := make([]usage.Event, 0, len(fetched))
	for _, e := range fetched {
		start := e.CreatedAt.Add(-time.Duration(e.LatencyMS) * time.Millisecond)
		end := e.CreatedAt
		if start.After(to) || end.Before(from) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) ByUser(userID string) []usage.Event {
	rows, err := s.query(context.Background(), `
		select id, user_id, token_id, session_id, session_source, agent_id, api_flavor, model, requested_model, provider,
			route_id, host, input_tokens, output_tokens, total_tokens, latency_ms, status,
			error_code, cached_tokens, cache_write_tokens, prompt_per_second, tokens_per_second, http_status,
			content_type, req_path, provider_path, provider_model, stream, token_name, server_name,
			service_id, service_name, project_id, project_name, energy_wh, energy_marginal_wh, energy_source, created_at
		from usage_events
		where user_id = ?
		order by created_at, id`, userID)
	if err != nil {
		s.setLastUsageError("by_user.query", err)
		return nil
	}
	defer rows.Close()
	events, err := scanUsageEvents(rows)
	s.setLastUsageError("by_user.scan", err)
	if err != nil {
		return nil
	}
	return events
}

func (s *SQLiteStore) All() []usage.Event {
	rows, err := s.query(context.Background(), `
		select id, user_id, token_id, session_id, session_source, agent_id, api_flavor, model, requested_model, provider,
			route_id, host, input_tokens, output_tokens, total_tokens, latency_ms, status,
			error_code, cached_tokens, cache_write_tokens, prompt_per_second, tokens_per_second, http_status,
			content_type, req_path, provider_path, provider_model, stream, token_name, server_name,
			service_id, service_name, project_id, project_name, energy_wh, energy_marginal_wh, energy_source, created_at
		from usage_events
		order by created_at, id`)
	if err != nil {
		s.setLastUsageError("all.query", err)
		return nil
	}
	defer rows.Close()
	events, err := scanUsageEvents(rows)
	s.setLastUsageError("all.scan", err)
	if err != nil {
		return nil
	}
	return events
}

func (s *SQLiteStore) LastUsageError() error {
	s.usageErrMu.RLock()
	defer s.usageErrMu.RUnlock()
	return s.lastUsageError
}

// setLastUsageError records err (nil clears it) in the lastUsageError
// side-channel that LastUsageError exposes. op names the failing operation
// (e.g. "record", "query.count") for the log line below — none of
// Record/Query/Stats/TimeSeries can return an error through usage.Store's
// current (infallible-shaped) signatures, so this is the only place a DB
// failure here becomes visible: without this log, a failed insert silently
// drops a usage event (billing/energy under-counts with no signal) and a
// failed Query/Stats/TimeSeries silently returns an empty result as if there
// were genuinely no matching rows.
func (s *SQLiteStore) setLastUsageError(op string, err error) {
	s.usageErrMu.Lock()
	defer s.usageErrMu.Unlock()
	if err != nil {
		s.lastUsageError = fmt.Errorf("sqlite usage store: %w", err)
		slog.Error("usage store operation failed", "op", op, "err", err)
		return
	}
	s.lastUsageError = nil
}

// TimeSeries selects the minimal columns needed for the bucketed activity
// aggregate over the [From, To) window (created_at >= From AND created_at < To),
// scoped by the SAME predicate usageWhere applies to Query/Stats/EnergyByServer/
// UsageGroups, then delegates to usage.ComputeTimeSeries. The window is built
// separately from usageWhere: a copy of q with From/To zeroed is passed to
// usageWhere (suppressing ITS window emission, which is keyed off
// From.IsZero()/To.IsZero()) so the caller's own [From, To) bounds can be ANDed
// on afterward with TimeSeries' half-open `< To` (exclusive) upper bound —
// usageWhere's Query/Stats window is closed (`<= To`, inclusive), and that
// difference must be preserved so consecutive buckets never double-count the
// boundary instant. On any error it returns a non-nil-Points empty series
// ALONGSIDE the non-nil error — a defensive belt-and-suspenders should a
// caller ignore the error (never a bare zero-value TimeSeries, whose nil
// Points would marshal to JSON null and crash the frontend's Histogram type)
// — but the caller MUST check the error rather than treat that empty series
// as "genuinely no matching rows".
func (s *SQLiteStore) TimeSeries(q usage.Query, bucketSecs int) (usage.TimeSeries, error) {
	qCopy := q
	qCopy.From = time.Time{}
	qCopy.To = time.Time{}
	where, args := usageWhere(s.dl, qCopy, "")
	windowCond := "e.created_at >= ? and e.created_at < ?"
	if where == "" {
		where = " where " + windowCond
	} else {
		where = where + " and " + windowCond
	}
	args = append(args, q.From, q.To)

	// NOTE: this SELECT has no LIMIT, so a very large window streams every matching
	// row into ComputeTimeSeries; a SQL GROUP BY into buckets would scale better,
	// but that is out of scope here (ComputeTimeSeries already coarsens/bounds the
	// output side).
	query := "select e.created_at, e.latency_ms, e.input_tokens, e.output_tokens, e.energy_wh" +
		usageEventsFromClause + where

	rows, err := s.query(context.Background(), query, args...)
	if err != nil {
		s.setLastUsageError("timeseries.query", err)
		return usage.ComputeTimeSeries(nil, q.From, q.To, bucketSecs), fmt.Errorf("usage timeseries: %w", err)
	}
	defer rows.Close()

	events := make([]usage.Event, 0)
	for rows.Next() {
		var e usage.Event
		if err := rows.Scan(&e.CreatedAt, &e.LatencyMS, &e.InputTokens, &e.OutputTokens, &e.EnergyWh); err != nil {
			wrapped := fmt.Errorf("scan usage timeseries: %w", err)
			s.setLastUsageError("timeseries.scan", wrapped)
			return usage.ComputeTimeSeries(nil, q.From, q.To, bucketSecs), wrapped
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		wrapped := fmt.Errorf("iterate usage timeseries: %w", err)
		s.setLastUsageError("timeseries.iterate", wrapped)
		return usage.ComputeTimeSeries(nil, q.From, q.To, bucketSecs), wrapped
	}
	s.setLastUsageError("timeseries", nil)
	return usage.ComputeTimeSeries(events, q.From, q.To, bucketSecs), nil
}

func scanUsageEvents(rows *sql.Rows) ([]usage.Event, error) {
	events := make([]usage.Event, 0)
	for rows.Next() {
		var event usage.Event
		var streamInt int64
		if err := rows.Scan(
			&event.ID,
			&event.UserID,
			&event.TokenID,
			&event.SessionID,
			&event.SessionSource,
			&event.AgentID,
			&event.APIFlavor,
			&event.Model,
			&event.RequestedModel,
			&event.Provider,
			&event.RouteID,
			&event.Host,
			&event.InputTokens,
			&event.OutputTokens,
			&event.TotalTokens,
			&event.LatencyMS,
			&event.Status,
			&event.ErrorCode,
			&event.CachedTokens,
			&event.CacheWriteTokens,
			&event.PromptPerSecond,
			&event.TokensPerSecond,
			&event.HTTPStatus,
			&event.ContentType,
			&event.ReqPath,
			&event.ProviderPath,
			&event.ProviderModel,
			&streamInt,
			&event.TokenName,
			&event.ServerName,
			&event.ServiceID,
			&event.ServiceName,
			&event.ProjectID,
			&event.ProjectName,
			&event.EnergyWh,
			&event.EnergyMarginalWh,
			&event.EnergySource,
			&event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan usage event: %w", err)
		}
		event.Stream = streamInt != 0
		events = append(events, event)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("iterate usage events: %w", err)
	}
	return events, nil
}

// usageEventColumns is the e.-aliased select list matching scanUsageRows order,
// identical to the ByUser/All column order.
const usageEventColumns = `e.id, e.user_id, e.token_id, e.session_id, e.session_source, e.agent_id, e.api_flavor, e.model, e.requested_model, e.provider,
	e.route_id, e.host, e.input_tokens, e.output_tokens, e.total_tokens, e.latency_ms, e.status,
	e.error_code, e.cached_tokens, e.cache_write_tokens, e.prompt_per_second, e.tokens_per_second, e.http_status,
	e.content_type, e.req_path, e.provider_path, e.provider_model, e.stream, e.token_name, e.server_name,
	e.service_id, e.service_name, e.project_id, e.project_name, e.energy_wh, e.energy_marginal_wh, e.energy_source, e.created_at`

var usageSortColumns = map[string]string{
	"created_at":        "e.created_at",
	"latency_ms":        "e.latency_ms",
	"total_tokens":      "e.total_tokens",
	"prompt_per_second": "e.prompt_per_second",
	"tokens_per_second": "e.tokens_per_second",
	"http_status":       "e.http_status",
	"model":             "e.model",
	"requested_model":   "e.requested_model",
	"server_name":       "e.server_name",
	"token_name":        "e.token_name",
}

// usageWhere builds the parameterized WHERE shared by Query and Stats. dl is
// the caller's dialect (for the ilike() operator, since sqlite LIKE and
// postgres ILIKE are the case-insensitive spellings on their respective
// dialects — dl.ilike() lets these filters match case-insensitively on both).
// ownerNameExpr, when non-empty, is the SQL expression yielding the resolved
// owner name (available only when the caller has joined users — the ScopeAll
// list); the Owner filter then LIKE-matches it in addition to e.user_id.
// Numeric filters iterate NumericColumns in a sorted key order so the
// positional args are deterministic.
func usageWhere(dl dialect, q usage.Query, ownerNameExpr string) (string, []any) {
	il := dl.ilike()
	conds := make([]string, 0)
	args := make([]any, 0)
	if !q.ScopeAll {
		conds = append(conds, "e.user_id = ?")
		args = append(args, q.UserID)
	}
	if q.HasTokenFilter {
		conds = append(conds, "e.token_id = ?")
		args = append(args, q.TokenID)
	}
	if q.HasServiceFilter {
		conds = append(conds, "e.service_id = ?")
		args = append(args, q.ServiceID)
	}
	if q.Model != "" {
		conds = append(conds, "e.model "+il+" ?")
		args = append(args, "%"+q.Model+"%")
	}
	if q.Server != "" {
		like := "%" + q.Server + "%"
		conds = append(conds, "(e.server_name "+il+" ? or e.host "+il+" ?)")
		args = append(args, like, like)
	}
	// Exact (case-insensitive equality via lower()=lower(), NOT ilike — a value
	// with a `_`/`%` must match literally, so LIKE-wildcard semantics are wrong)
	// filters used to expand a group into its member rows. Mirrors matchUsage's
	// EqualFold and is standard on both sqlite and postgres.
	// Fire on the presence flag OR a non-empty value: the flag path lets an exact
	// filter match the EMPTY value (empty-key group expansion) while existing
	// callers that set only the value still fire (backward-compatible).
	if q.HasServerExact || q.ServerExact != "" {
		conds = append(conds, "lower(e.server_name) = lower(?)")
		args = append(args, q.ServerExact)
	}
	if q.HasSessionIDExact || q.SessionIDExact != "" {
		conds = append(conds, "lower(e.session_id) = lower(?)")
		args = append(args, q.SessionIDExact)
	}
	if q.HasModelExact || q.ModelExact != "" {
		conds = append(conds, "lower(e.model) = lower(?)")
		args = append(args, q.ModelExact)
	}
	// ProjectIDExact: same exact-match convention as ServerExact/SessionIDExact/
	// ModelExact above (design spec §7 drill-down + §8 project-scope pin) --
	// fires on the presence flag OR a non-empty value, so it can pin the EMPTY
	// (no-project) bucket too.
	if q.HasProjectIDExact || q.ProjectIDExact != "" {
		conds = append(conds, "lower(e.project_id) = lower(?)")
		args = append(args, q.ProjectIDExact)
	}
	// ProjectIDs: the project-scope IN-list applyUsageScope (§8) sets for a
	// non-admin's top-level group-by-project query. nil = no filter; a non-nil
	// EMPTY slice (a caller who is a member of zero projects) must match ZERO
	// rows -- an empty SQL `in ()` list is invalid on both dialects, so this uses
	// a literal always-false predicate instead.
	if q.ProjectIDs != nil {
		if len(q.ProjectIDs) == 0 {
			conds = append(conds, "1 = 0")
		} else {
			placeholders := make([]string, len(q.ProjectIDs))
			for i, id := range q.ProjectIDs {
				placeholders[i] = "lower(?)"
				args = append(args, id)
			}
			conds = append(conds, "lower(e.project_id) in ("+strings.Join(placeholders, ", ")+")")
		}
	}
	switch q.Status {
	case "error":
		conds = append(conds, "(e.status = 'error' or e.http_status >= 400)")
	case "success":
		conds = append(conds, "not (e.status = 'error' or e.http_status >= 400)")
	}
	if q.Q != "" {
		like := "%" + q.Q + "%"
		conds = append(conds, "(e.id "+il+" ? or e.model "+il+" ? or e.requested_model "+il+" ? or e.host "+il+" ? or e.server_name "+il+" ? or e.token_name "+il+" ?)")
		args = append(args, like, like, like, like, like, like)
	}
	if q.ReqPath != "" {
		conds = append(conds, "e.req_path "+il+" ?")
		args = append(args, "%"+q.ReqPath+"%")
	}
	if q.ContentType != "" {
		conds = append(conds, "e.content_type "+il+" ?")
		args = append(args, "%"+q.ContentType+"%")
	}
	if q.ProviderPath != "" {
		conds = append(conds, "e.provider_path "+il+" ?")
		args = append(args, "%"+q.ProviderPath+"%")
	}
	if q.SessionID != "" {
		conds = append(conds, "e.session_id "+il+" ?")
		args = append(args, "%"+q.SessionID+"%")
	}
	if q.SessionSource != "" {
		conds = append(conds, "e.session_source "+il+" ?")
		args = append(args, "%"+q.SessionSource+"%")
	}
	if q.AgentID != "" {
		conds = append(conds, "e.agent_id "+il+" ?")
		args = append(args, "%"+q.AgentID+"%")
	}
	if q.ProviderModel != "" {
		conds = append(conds, "e.provider_model "+il+" ?")
		args = append(args, "%"+q.ProviderModel+"%")
	}
	switch q.Stream {
	case "true":
		conds = append(conds, "e.stream = ?")
		args = append(args, 1)
	case "false":
		conds = append(conds, "e.stream = ?")
		args = append(args, 0)
	}
	if q.Owner != "" {
		like := "%" + q.Owner + "%"
		if ownerNameExpr != "" {
			conds = append(conds, "(e.user_id "+il+" ? or "+ownerNameExpr+" "+il+" ?)")
			args = append(args, like, like)
		} else {
			conds = append(conds, "e.user_id "+il+" ?")
			args = append(args, like)
		}
	}
	if !q.From.IsZero() {
		conds = append(conds, "e.created_at >= ?")
		args = append(args, q.From)
	}
	if !q.To.IsZero() {
		conds = append(conds, "e.created_at <= ?")
		args = append(args, q.To)
	}
	if !q.TimeFrom.IsZero() {
		conds = append(conds, "e.created_at >= ?")
		args = append(args, q.TimeFrom)
	}
	if !q.TimeTo.IsZero() {
		conds = append(conds, "e.created_at <= ?")
		args = append(args, q.TimeTo)
	}
	for _, id := range sortedUsageNumericIDs() {
		col := "e." + usage.NumericColumns[id]
		if minVal, ok := q.NumericMin[id]; ok {
			conds = append(conds, col+" >= ?")
			args = append(args, minVal)
		}
		if maxVal, ok := q.NumericMax[id]; ok {
			conds = append(conds, col+" <= ?")
			args = append(args, maxVal)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " where " + strings.Join(conds, " and "), args
}

// sortedUsageNumericIDs returns the NumericColumns keys in a stable sorted
// order so usageWhere emits its numeric predicates (and thus positional args)
// deterministically.
func sortedUsageNumericIDs() []string {
	ids := make([]string, 0, len(usage.NumericColumns))
	for id := range usage.NumericColumns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func usageOrderBy(sortKey, order string) string {
	col := usageSortColumns[usage.NormalizeSort(sortKey)]
	dir := "desc"
	if usage.NormalizeOrder(order) == "asc" {
		dir = "asc"
	}
	return " order by " + col + " " + dir + ", e.id " + dir
}

// Query returns one filtered/sorted/paginated page. UserName is resolved via a
// LEFT JOIN users only for ScopeAll; otherwise it is the empty literal. On any
// error it returns a non-nil-Data empty Page ALONGSIDE the non-nil error — a
// defensive belt-and-suspenders should a caller ignore the error — but the
// caller MUST check the error rather than treat that empty page as "genuinely
// no matching rows".
func (s *SQLiteStore) Query(q usage.Query) (usage.Page, error) {
	// The users join is present only for ScopeAll. Build it (and the resolved
	// name expression) once so the count, the WHERE, and the list all agree —
	// the Owner filter LIKE-matches userNameSQL, so the count must join too.
	userNameExpr := "'' as user_name"
	userNameSQL := ""
	join := ""
	if q.ScopeAll {
		userNameSQL = "coalesce(nullif(u.display_name, ''), nullif(u.email, ''), e.user_id)"
		userNameExpr = userNameSQL + " as user_name"
		join = " left join users as u on u.id = e.user_id"
	}

	where, args := usageWhere(s.dl, q, userNameSQL)
	limit := usage.NormalizeLimit(q.Limit)
	page := usage.NormalizePage(q.Page)

	total := 0
	if err := s.queryRow(context.Background(), "select count(*) from usage_events as e"+join+where, args...).Scan(&total); err != nil {
		s.setLastUsageError("query.count", err)
		return usage.Page{Data: make([]usage.Row, 0), Page: page, Limit: limit}, fmt.Errorf("usage query count: %w", err)
	}

	// Bound page against the page count before multiplying, mirroring the memory
	// Recorder (recorder.go): an unbounded user-supplied page overflows
	// (page-1)*limit to a negative int64 OFFSET, which SQLite clamps to 0 and thus
	// wrongly returns the FIRST page while the envelope reports the huge Page. A
	// page past the last must instead yield an empty Data page (offset past the end).
	totalPages := usage.TotalPages(total, limit)
	if page > totalPages {
		return usage.Page{Data: make([]usage.Row, 0), Page: page, Limit: limit, Total: total, TotalPages: totalPages}, nil
	}

	listSQL := "select " + usageEventColumns + ", " + userNameExpr +
		usageEventsFromClause + join + where +
		usageOrderBy(q.Sort, q.Order) + " limit ? offset ?"
	listArgs := append(append([]any{}, args...), limit, (page-1)*limit)

	rows, err := s.query(context.Background(), listSQL, listArgs...)
	if err != nil {
		s.setLastUsageError("query.list", err)
		return usage.Page{Data: make([]usage.Row, 0), Page: page, Limit: limit, Total: total, TotalPages: usage.TotalPages(total, limit)}, fmt.Errorf("usage query list: %w", err)
	}
	defer rows.Close()

	data, err := scanUsageRows(rows)
	s.setLastUsageError("query.scan", err)
	if err != nil {
		return usage.Page{Data: make([]usage.Row, 0), Page: page, Limit: limit, Total: total, TotalPages: usage.TotalPages(total, limit)}, fmt.Errorf("usage query scan: %w", err)
	}
	return usage.Page{
		Data:       data,
		Page:       page,
		Limit:      limit,
		Total:      total,
		TotalPages: usage.TotalPages(total, limit),
	}, nil
}

func scanUsageRows(rows *sql.Rows) ([]usage.Row, error) {
	out := make([]usage.Row, 0)
	for rows.Next() {
		var row usage.Row
		var streamInt int64
		if err := rows.Scan(
			&row.ID,
			&row.UserID,
			&row.TokenID,
			&row.SessionID,
			&row.SessionSource,
			&row.AgentID,
			&row.APIFlavor,
			&row.Model,
			&row.RequestedModel,
			&row.Provider,
			&row.RouteID,
			&row.Host,
			&row.InputTokens,
			&row.OutputTokens,
			&row.TotalTokens,
			&row.LatencyMS,
			&row.Status,
			&row.ErrorCode,
			&row.CachedTokens,
			&row.CacheWriteTokens,
			&row.PromptPerSecond,
			&row.TokensPerSecond,
			&row.HTTPStatus,
			&row.ContentType,
			&row.ReqPath,
			&row.ProviderPath,
			&row.ProviderModel,
			&streamInt,
			&row.TokenName,
			&row.ServerName,
			&row.ServiceID,
			&row.ServiceName,
			&row.ProjectID,
			&row.ProjectName,
			&row.EnergyWh,
			&row.EnergyMarginalWh,
			&row.EnergySource,
			&row.CreatedAt,
			&row.UserName,
		); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		row.Stream = streamInt != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("iterate usage rows: %w", err)
	}
	return out, nil
}

// emptyUsageStats is a zeroed stats value whose histograms carry non-nil (empty)
// Bins. Error/early-exit paths must return this rather than a bare UsageStats{}:
// a nil Bins marshals to JSON "bins":null, which crashes the Activity view (the
// frontend Histogram type declares bins as a non-null array).
func emptyUsageStats() usage.Stats {
	return usage.Stats{
		PromptPerSecond: usage.ComputeHistogram(nil),
		TokensPerSecond: usage.ComputeHistogram(nil),
	}
}

// Stats aggregates the filtered set: tile totals over ALL rows, speed histograms
// over the non-zero values. Uses the same WHERE and error predicate as Query. On
// any error it returns emptyUsageStats() (non-nil Bins) ALONGSIDE the non-nil
// error — a defensive belt-and-suspenders should a caller ignore the error —
// but the caller MUST check the error rather than treat that empty result as
// "genuinely no matching rows".
func (s *SQLiteStore) Stats(q usage.Query) (usage.Stats, error) {
	// Stats never joins users, so the Owner filter matches user_id only (parity
	// with the memory recorder, which passes a nil name resolver for Stats).
	where, args := usageWhere(s.dl, q, "")
	rows, err := s.query(context.Background(),
		"select e.status, e.http_status, e.cached_tokens, e.cache_write_tokens, e.input_tokens, e.output_tokens, e.prompt_per_second, e.tokens_per_second, e.energy_wh from usage_events as e"+where,
		args...,
	)
	if err != nil {
		s.setLastUsageError("stats.query", err)
		return emptyUsageStats(), fmt.Errorf("usage stats query: %w", err)
	}
	defer rows.Close()

	var totals usage.StatTotals
	prompt := make([]float64, 0)
	tokens := make([]float64, 0)
	for rows.Next() {
		var (
			status                         string
			httpStatus                     int
			cached, cacheWrite, input, out int
			pps, tps, energyWh             float64
		)
		if err := rows.Scan(&status, &httpStatus, &cached, &cacheWrite, &input, &out, &pps, &tps, &energyWh); err != nil {
			wrapped := fmt.Errorf("scan usage stats: %w", err)
			s.setLastUsageError("stats.scan", wrapped)
			return emptyUsageStats(), wrapped
		}
		totals.TotalRequests++
		if usage.IsError(status, httpStatus) {
			totals.ErrorCount++
		}
		totals.CachedTokens += cached
		totals.CacheWriteTokens += cacheWrite
		totals.InputTokens += input
		totals.OutputTokens += out
		totals.TotalEnergyWh += energyWh
		prompt = append(prompt, pps)
		tokens = append(tokens, tps)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		wrapped := fmt.Errorf("iterate usage stats: %w", err)
		s.setLastUsageError("stats.iterate", wrapped)
		return emptyUsageStats(), wrapped
	}
	s.setLastUsageError("stats", nil)
	return usage.Stats{
		Totals:          totals,
		PromptPerSecond: usage.ComputeHistogram(prompt),
		TokensPerSecond: usage.ComputeHistogram(tokens),
	}, nil
}

// EnergyByServer sums energy_wh grouped by host over the SAME filtered set
// usageWhere(q) applies to Stats, for the portal layer to derive a
// per-server-price-weighted total cost (see portal.Service.UsageStats). A
// host with no matching rows is simply absent from the returned map.
func (s *SQLiteStore) EnergyByServer(ctx context.Context, q usage.Query) (map[string]float64, error) {
	where, args := usageWhere(s.dl, q, "")
	rows, err := s.query(ctx, "select e.host, sum(e.energy_wh) from usage_events as e"+where+" group by e.host", args...)
	if err != nil {
		return nil, fmt.Errorf("energy by server: %w", err)
	}
	defer rows.Close()

	out := make(map[string]float64)
	for rows.Next() {
		var host string
		var wh float64
		if err := rows.Scan(&host, &wh); err != nil {
			return nil, fmt.Errorf("scan energy by server: %w", err)
		}
		out[host] = wh
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("iterate energy by server: %w", err)
	}
	return out, nil
}

// principalUsageColumn maps a principal_limits principal type to its
// usage_events column, mirroring usageGroupColumn: a FIXED literal (never
// interpolated from a request) — defense-in-depth against injection on top of
// the caller's own whitelist.
func principalUsageColumn(principalType string) (string, bool) {
	switch principalType {
	case routing.PrincipalTypeService:
		return "service_id", true
	case routing.PrincipalTypeUser:
		return "user_id", true
	}
	return "", false
}

// energyDefaultPricePerKwhKey mirrors portal's system_settings key of the
// same name (the store package cannot import portal — portal imports store —
// so this is a narrow, deliberately duplicated constant, not a shared one).
const energyDefaultPricePerKwhKey = "energy_default_price_per_kwh"

// systemDefaultPricePerKwh reads + parses the energy_default_price_per_kwh
// system setting the SAME lenient way portal's energyDefaultFloat does: a
// missing/blank/unparseable/negative value reads back as 0 (never an error —
// the system default is optional).
func (s *SQLStore) systemDefaultPricePerKwh(ctx context.Context) (float64, error) {
	values, err := s.SystemSettings(ctx)
	if err != nil {
		return 0, fmt.Errorf("system default price: %w", err)
	}
	raw := strings.TrimSpace(values[energyDefaultPricePerKwhKey])
	if raw == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		return 0, nil
	}
	return f, nil
}

// UsageAggregateSince sums, for the principal identified by (principalType,
// principalID), the request COUNT and total TOKEN count recorded in
// usage_events with created_at >= since, plus a price-weighted COST: energy_wh
// is summed grouped by host, then each host's contribution is weighted by
// (energy_wh/1000) * price(host) — that server's own ai_servers.price_per_kwh
// when set (> 0), else the system-wide energy_default_price_per_kwh default
// (mirrors portal.Service.attachUsageCost/resolveUsagePrice's exact fallback,
// so a cost_budget threshold compares apples-to-apples with the rest of the
// app's cost displays). An unrecognized principalType is an error.
func (s *SQLStore) UsageAggregateSince(ctx context.Context, principalType, principalID string, since time.Time) (int64, int64, float64, error) {
	col, ok := principalUsageColumn(principalType)
	if !ok {
		return 0, 0, 0, fmt.Errorf("usage aggregate: invalid principal type %q", principalType)
	}

	var requests, tokens int64
	row := s.queryRow(ctx,
		"select count(*), coalesce(sum(total_tokens), 0) from usage_events where "+col+" = ? and created_at >= ?",
		principalID, since)
	if err := row.Scan(&requests, &tokens); err != nil {
		return 0, 0, 0, fmt.Errorf("usage aggregate: %w", err)
	}

	rows, err := s.query(ctx,
		"select host, coalesce(sum(energy_wh), 0) from usage_events where "+col+" = ? and created_at >= ? group by host",
		principalID, since)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("usage aggregate energy: %w", err)
	}
	energyByHost := make(map[string]float64)
	for rows.Next() {
		var host string
		var wh float64
		if err := rows.Scan(&host, &wh); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("usage aggregate energy scan: %w", err)
		}
		energyByHost[host] = wh
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return 0, 0, 0, fmt.Errorf("usage aggregate energy iterate: %w", rowsErr)
	}
	if len(energyByHost) == 0 {
		return requests, tokens, 0, nil
	}

	sysDefault, err := s.systemDefaultPricePerKwh(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	var cost float64
	for host, wh := range energyByHost {
		price := sysDefault
		if server, err := s.AIServerByID(ctx, host); err == nil && server.PricePerKwh > 0 {
			price = server.PricePerKwh
		}
		cost += wh / 1000 * price
	}
	return requests, tokens, cost, nil
}

// aggTime scans a min()/max()(created_at) aggregate across dialects. Postgres
// (pgx) returns a time.Time for a timestamptz aggregate, but modernc sqlite loses
// the column's declared "timestamp" type on an aggregate EXPRESSION and returns
// the stored value as a STRING in Go's time.Time.String() layout — so a plain
// sql.NullTime scan fails there ("unsupported Scan, storing driver.Value type
// string into type *time.Time"). This Scanner accepts a time.Time, a string, or
// []byte (parsing the string via aggTimeLayouts), so FirstAt/LastAt work on both
// dialects. Compare the result with time.Time.Equal (a parsed "+0000 UTC" zone is
// instant-equal to a UTC time even if the location object differs).
type aggTime struct {
	Time  time.Time
	Valid bool
}

// aggTimeLayouts is the ordered set of layouts aggTime.parse tries. The first is
// modernc's stored representation (time.Time.String()); the rest are defensive
// fallbacks for driver/version drift.
var aggTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST", // modernc time.Time.String()
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05",
}

func (a *aggTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		a.Time, a.Valid = time.Time{}, false
		return nil
	case time.Time:
		a.Time, a.Valid = v, true
		return nil
	case []byte:
		return a.parse(string(v))
	case string:
		return a.parse(v)
	}
	return fmt.Errorf("aggTime: unsupported scan type %T", src)
}

func (a *aggTime) parse(s string) error {
	if s == "" {
		a.Time, a.Valid = time.Time{}, false
		return nil
	}
	for _, layout := range aggTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			a.Time, a.Valid = t, true
			return nil
		}
	}
	return fmt.Errorf("aggTime: cannot parse %q", s)
}

// usageGroupColumn maps a group-by dimension to its usage_events column. The column
// name is a FIXED literal (never interpolated from the request) — defense-in-depth
// against injection on top of the caller's whitelist.
func usageGroupColumn(groupBy string) (string, bool) {
	switch groupBy {
	case "session":
		return "session_id", true
	case "server":
		return "server_name", true
	case "user":
		return "user_id", true
	case "token":
		return "token_id", true
	case "model":
		return "model", true
	case "service":
		return "service_id", true
	case "project":
		return "project_id", true
	}
	return "", false
}

// UsageGroups aggregates the SAME filtered set usageWhere(q) applies GROUP BY
// (dimension, host) — one row per (group value, host) — so the portal layer can
// fold buckets by dimension while weighting cost per host price. groupBy is a
// fixed dimension keyword (session|server|user|token|model), NEVER interpolated
// from user input; an unknown value is an error.
func (s *SQLiteStore) UsageGroups(ctx context.Context, q usage.Query, groupBy string) ([]usage.GroupBucket, error) {
	col, ok := usageGroupColumn(groupBy)
	if !ok {
		return nil, fmt.Errorf("usage groups: invalid group_by %q", groupBy)
	}
	where, args := usageWhere(s.dl, q, "")
	sqlText := "select e." + col + " as gkey, e.host," +
		" count(*)," +
		" sum(case when e.status = 'error' or e.http_status >= 400 then 1 else 0 end)," +
		" sum(e.input_tokens), sum(e.output_tokens), sum(e.cached_tokens), sum(e.cache_write_tokens)," +
		" sum(e.energy_wh), min(e.created_at), max(e.created_at)" +
		usageEventsFromClause + where +
		" group by e." + col + ", e.host"
	rows, err := s.query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("usage groups: %w", err)
	}
	defer rows.Close()
	out := make([]usage.GroupBucket, 0)
	for rows.Next() {
		var b usage.GroupBucket
		var count, errCount, in, outTok, cached, cacheWrite int64
		var energy float64
		var first, last aggTime
		if err := rows.Scan(&b.Key, &b.Host, &count, &errCount, &in, &outTok, &cached, &cacheWrite, &energy, &first, &last); err != nil {
			return nil, fmt.Errorf("scan usage group: %w", err)
		}
		b.Count, b.ErrorCount = int(count), int(errCount)
		b.InputTokens, b.OutputTokens = int(in), int(outTok)
		b.CachedTokens, b.CacheWriteTokens = int(cached), int(cacheWrite)
		b.EnergyWh = energy
		if first.Valid {
			b.FirstAt = first.Time
		}
		if last.Valid {
			b.LastAt = last.Time
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("iterate usage groups: %w", err)
	}
	return out, nil
}
