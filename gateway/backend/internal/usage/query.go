// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import "time"

// Query is the shared input for the parameterized list (Query) and the
// aggregate (Stats). Paging/Sort are only consulted by Query.
type Query struct {
	UserID   string // set = only this user's rows; ScopeAll must be false
	ScopeAll bool   // handler sets true ONLY when principal.HasScope("system") AND scope=="all"
	// TokenID is the token id to match when HasTokenFilter is true. "" is a VALID
	// value meaning "match rows with an empty token_id" (chat / no-token usage).
	TokenID string
	// HasTokenFilter gates the TokenID predicate: true applies it (even when
	// TokenID==""), false leaves token filtering off entirely.
	HasTokenFilter bool
	// ServiceID is the service id to match when HasServiceFilter is true (mirrors
	// TokenID/HasTokenFilter). "" is a VALID value meaning "match rows with an
	// empty service_id" (user-token/session usage, i.e. NOT service-attributed).
	ServiceID string
	// HasServiceFilter gates the ServiceID predicate: true applies it (even when
	// ServiceID==""), false leaves service filtering off entirely.
	HasServiceFilter bool
	// FilterUserID is the admin-only "Bestimmter Nutzer" pin, applied in
	// applyUsageScope. It is separate from the scope-pinning UserID so the two
	// concerns stay independent; a non-admin's FilterUserID is ignored.
	FilterUserID string
	Model        string // exact match on model
	Server       string // free-text: case-insensitive substring over server_name (host fallback)
	// ServerExact pins a single server by its EXACT (case-insensitive) server_name,
	// with NO host fallback and NO substring — used by the per-server Performance
	// view so a server named "prod" does not also match "prod-eu". Applied in
	// matchUsage (memory) and SQLiteStore.TimeSeries (sql).
	ServerExact string
	// GroupBy selects the group-by dimension for UsageGroups: "" (no grouping) |
	// "session" | "server" | "user" | "token" | "model".
	GroupBy string
	// SessionIDExact / ModelExact pin a single exact (case-insensitive) session_id /
	// model — used to expand a session/model group into its member rows (server/user/
	// token already have exact filters).
	SessionIDExact string
	ModelExact     string
	// Has*Exact distinguish "filter present, value may be empty" from "absent", so an
	// exact filter can match the EMPTY value (e.g. expanding an empty-session group).
	HasServerExact    bool
	HasSessionIDExact bool
	HasModelExact     bool
	// ProjectIDExact / HasProjectIDExact mirror SessionIDExact/HasSessionIDExact:
	// a drill-down pin on a single exact (case-insensitive) project_id, used both
	// to expand a group-by-project bucket into its member rows AND, per design
	// spec §8, by applyUsageScope to detect a project-scoped query in the first
	// place. HasProjectIDExact lets the EMPTY value ("" = no-project bucket) be
	// pinned explicitly, distinct from "no project filter at all".
	ProjectIDExact    string
	HasProjectIDExact bool
	// ProjectIDs is the project-scope IN-list applyUsageScope (§8) sets for a
	// non-admin's top-level group-by-project query (no ProjectIDExact filter):
	// restrict to rows whose project_id is one of the caller's member projects.
	// nil = no IN-list filter (the common case); a non-nil EMPTY slice means
	// "match zero rows" (a caller who is a member of no project) -- enforced by
	// both usage.Recorder.matchUsage and the SQL store's usageWhere.
	ProjectIDs []string
	Status     string // "success" | "error"; empty = no filter
	Q          string // free-text LIKE %..% over id, model, host, server_name, token_name
	Owner      string // case-insensitive substring over the owner: user_id, plus the
	// resolved user_name when it is available (ScopeAll list only)
	ReqPath       string // case-insensitive substring (LIKE %..%) over req_path
	ContentType   string // case-insensitive substring (LIKE %..%) over content_type
	ProviderModel string // case-insensitive substring (LIKE %..%) over provider_model
	ProviderPath  string // case-insensitive substring (LIKE %..%) over provider_path
	// SessionID/SessionSource/AgentID are case-insensitive substring (LIKE %..%)
	// filters over the protocol-aware session provenance columns surfaced in
	// Activity (session_source = the request protocol, agent_id = Claude Code
	// subagent id).
	SessionID     string
	SessionSource string
	AgentID       string
	// Stream is a tri-state boolean filter on the stream column: "true" = only
	// streamed rows, "false" = only non-streamed, "" (or anything else) = no filter.
	Stream   string
	From, To time.Time // window on created_at; zero = open end
	// TimeFrom/TimeTo are additional lower/upper created_at bounds, ANDed with the
	// range-derived From/To. Zero = no extra bound.
	TimeFrom, TimeTo time.Time
	// NumericMin/NumericMax hold per-column range bounds keyed by the column id in
	// NumericColumns: NumericMin[id] = value >= min, NumericMax[id] = value <= max.
	NumericMin map[string]float64
	NumericMax map[string]float64
	Sort       string // whitelist; default "created_at"
	Order      string // "asc" | "desc"; default "desc"
	Page       int    // 1-based (list only)
	Limit      int    // 25|50|100 (list only)
}

// NumericColumns whitelists the numeric usage_events columns that accept a
// <col>_min / <col>_max range filter, mapping the public column id (also the
// query-param prefix) to the usage_events SQL column name. Both stores iterate
// this map so an unknown key in NumericMin/NumericMax is ignored everywhere.
var NumericColumns = map[string]string{
	"total_tokens":       "total_tokens",
	"latency_ms":         "latency_ms",
	"input_tokens":       "input_tokens",
	"output_tokens":      "output_tokens",
	"cached_tokens":      "cached_tokens",
	"cache_write_tokens": "cache_write_tokens",
	"prompt_per_second":  "prompt_per_second",
	"tokens_per_second":  "tokens_per_second",
	"energy_wh":          "energy_wh",
	"energy_marginal_wh": "energy_marginal_wh",
}

// NumericValue returns the numeric value of the given whitelisted column id
// for an event as a float64, mirroring the SQL column read. Unknown ids yield 0;
// callers gate on NumericColumns so unknown ids never reach a comparison.
func NumericValue(e Event, colID string) float64 {
	switch colID {
	case "total_tokens":
		return float64(e.TotalTokens)
	case "latency_ms":
		return float64(e.LatencyMS)
	case "input_tokens":
		return float64(e.InputTokens)
	case "output_tokens":
		return float64(e.OutputTokens)
	case "cached_tokens":
		return float64(e.CachedTokens)
	case "cache_write_tokens":
		return float64(e.CacheWriteTokens)
	case "prompt_per_second":
		return e.PromptPerSecond
	case "tokens_per_second":
		return e.TokensPerSecond
	case "energy_wh":
		return e.EnergyWh
	case "energy_marginal_wh":
		return e.EnergyMarginalWh
	}
	return 0
}

// GroupBucket is one (group-value, host) aggregate row from UsageGroups. The
// portal layer folds buckets by Key (summing) and weights cost per Host price.
type GroupBucket struct {
	Key              string
	Host             string
	Count            int
	ErrorCount       int
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	CacheWriteTokens int
	EnergyWh         float64
	FirstAt          time.Time
	LastAt           time.Time
}

// Row is a read DTO: the full Event plus the resolved owner name, which is
// filled only for ScopeAll queries. Event stays untouched; UserName is read-only.
type Row struct {
	Event
	UserName   string `json:"user_name,omitempty"`
	HasCapture bool   `json:"has_capture"`
	// CaptureLocked is true when a capture exists but is secret and the viewer
	// is an admin who is not the owner: the Activity list shows a lock (SP-2e),
	// never the content. Mutually exclusive with HasCapture per row.
	CaptureLocked bool `json:"capture_locked"`
}

// Page is one page of the parameterized list.
type Page struct {
	Data       []Row `json:"data"`
	Page       int   `json:"page"`
	Limit      int   `json:"limit"`
	Total      int   `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// HistogramBin is one [x0, x1) bucket with a count.
type HistogramBin struct {
	X0    float64 `json:"x0"`
	X1    float64 `json:"x1"`
	Count int     `json:"count"`
}

// Histogram is a Sturges-binned distribution of the non-zero metric values plus
// nearest-rank percentiles.
type Histogram struct {
	Bins    []HistogramBin `json:"bins"`
	Min     float64        `json:"min"`
	Max     float64        `json:"max"`
	BinSize float64        `json:"bin_size"`
	P50     float64        `json:"p50"`
	P95     float64        `json:"p95"`
	P99     float64        `json:"p99"`
}

// StatTotals are the tile sums over ALL filtered rows (zeros included).
type StatTotals struct {
	TotalRequests    int `json:"total_requests"`
	ErrorCount       int `json:"error_count"`
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	// TotalEnergyWh is a plain SUM(energy_wh) over the filtered set — no price
	// weighting, always available even when no price is configured anywhere.
	TotalEnergyWh float64 `json:"total_energy_wh"`
	// TotalCostEUR is the per-server-price-weighted cost derived from
	// EnergyByServer + AI-server/system-default pricing (P3 §8). It is set
	// ONLY by the portal layer (Service.UsageStats) — usage.Store.Stats
	// implementations (SQL/memory) never populate it, since pricing needs
	// the routing + system-settings stores this package does not depend on.
	TotalCostEUR float64 `json:"total_cost_eur"`
}

// Stats is the aggregate response: tile totals plus two speed histograms.
type Stats struct {
	Totals          StatTotals `json:"totals"`
	PromptPerSecond Histogram  `json:"prompt_per_second"`
	TokensPerSecond Histogram  `json:"tokens_per_second"`
}
