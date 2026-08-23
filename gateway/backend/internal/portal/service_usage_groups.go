// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/usage"
	"sort"
	"time"
)

// UsageGroupDTO is one folded group-by aggregate row surfaced by
// /api/portal/usage/groups. The store returns per-(group value, host) buckets;
// the Service folds them by Key (summing tokens/energy/counts) and derives
// CostEUR by weighting each host's energy by that server's resolved price
// (server PricePerKwh when > 0, else the system default) — mirroring the
// per-server cost weighting in UsageStats. KeyLabel is a best-effort display
// label (only group-by-user and group-by-service resolve a name; every other
// dimension echoes Key).
type UsageGroupDTO struct {
	Key              string  `json:"key"`
	KeyLabel         string  `json:"key_label"`
	Count            int     `json:"count"`
	ErrorCount       int     `json:"error_count"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	EnergyWh         float64 `json:"energy_wh"`
	CostEUR          float64 `json:"cost_eur"`
	FirstAt          string  `json:"first_at"`
	LastAt           string  `json:"last_at"`
}

// ErrUsageGroupByInvalid is returned by UsageGroups for an unrecognized group_by
// dimension. The gateway handler maps it to HTTP 400; the whitelist also keeps
// the store from ever receiving an unknown dimension.
var ErrUsageGroupByInvalid = errors.New("usage.group_by_invalid")

// usageGroupsLimit caps the number of folded groups returned, keeping a
// pathological cardinality (e.g. group-by-session over a huge window) bounded.
const usageGroupsLimit = 500

// knownUsageGroupBy whitelists the group-by dimensions the store understands.
func knownUsageGroupBy(g string) bool {
	switch g {
	case "session", "server", "user", "token", "model", "service", "project":
		return true
	}
	return false
}

// UsageGroups returns the folded, cost-weighted group aggregates for the given
// dimension under the same own/all scope rule as UsageStats (applyUsageScope is
// the single authority gate — a non-admin only ever sees their own rows). An
// unknown group_by is rejected with ErrUsageGroupByInvalid before any store call.
func (s *Service) UsageGroups(principal auth.Token, q usage.Query, groupBy string) ([]UsageGroupDTO, error) {
	if !knownUsageGroupBy(groupBy) {
		return nil, ErrUsageGroupByInvalid
	}
	ctx := context.Background()
	s.applyUsageScope(ctx, &q, principal)
	buckets, err := s.usage.UsageGroups(ctx, q, groupBy)
	if err != nil {
		return nil, err
	}
	sysDefault := s.systemDefaultPricePerKwh(ctx)
	// Resolve each host's price ONCE (mirrors attachUsageCost): a group-by over
	// many groups sharing few servers must not issue an AIServerByID lookup per
	// (key, host) bucket.
	priceByHost := make(map[string]float64)
	priceFor := func(host string) float64 {
		if p, ok := priceByHost[host]; ok {
			return p
		}
		p := s.resolveUsagePrice(ctx, host, sysDefault)
		priceByHost[host] = p
		return p
	}
	type agg struct {
		dto   UsageGroupDTO
		first time.Time
		last  time.Time
	}
	byKey := make(map[string]*agg)
	for _, b := range buckets {
		a := byKey[b.Key]
		if a == nil {
			a = &agg{dto: UsageGroupDTO{Key: b.Key}, first: b.FirstAt, last: b.LastAt}
			byKey[b.Key] = a
		}
		a.dto.Count += b.Count
		a.dto.ErrorCount += b.ErrorCount
		a.dto.InputTokens += b.InputTokens
		a.dto.OutputTokens += b.OutputTokens
		a.dto.CachedTokens += b.CachedTokens
		a.dto.CacheWriteTokens += b.CacheWriteTokens
		a.dto.EnergyWh += b.EnergyWh
		// Cost is weighted PER HOST bucket: this host's energy at this host's
		// resolved price, summed across the group. A single blended rate would
		// mis-cost a session that spanned servers at different prices.
		a.dto.CostEUR += b.EnergyWh / 1000 * priceFor(b.Host)
		if !b.FirstAt.IsZero() && (a.first.IsZero() || b.FirstAt.Before(a.first)) {
			a.first = b.FirstAt
		}
		if b.LastAt.After(a.last) {
			a.last = b.LastAt
		}
	}
	out := make([]UsageGroupDTO, 0, len(byKey))
	for _, a := range byKey {
		d := a.dto
		d.TotalTokens = d.InputTokens + d.OutputTokens + d.CachedTokens + d.CacheWriteTokens
		d.KeyLabel = s.usageGroupLabel(ctx, groupBy, d.Key)
		if !a.first.IsZero() {
			d.FirstAt = a.first.UTC().Format(time.RFC3339)
		}
		if !a.last.IsZero() {
			d.LastAt = a.last.UTC().Format(time.RFC3339)
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > usageGroupsLimit {
		out = out[:usageGroupsLimit]
	}
	return out, nil
}

// usageGroupLabel resolves a display label for a group key. The default is the
// raw key; for group-by-user it best-effort resolves the user's display name
// (falling back to email, then the raw id); for group-by-service/group-by-project
// it best-effort resolves the service's/project's CURRENT Name (a rename is
// reflected immediately, unlike the denormalized usage_events.service_name/
// project_name, which is a snapshot at request time). An empty key (e.g. the
// empty-token / session-chat / non-service-or-project usage case) is returned
// as-is so the frontend can label it.
func (s *Service) usageGroupLabel(ctx context.Context, groupBy, key string) string {
	if key == "" {
		return key
	}
	if groupBy == "user" && s.users != nil {
		if u, err := s.users.UserByID(ctx, key); err == nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != "" {
				return u.Email
			}
		}
	}
	if groupBy == "service" && s.routes != nil {
		if svc, err := s.routes.ServiceByID(ctx, key); err == nil && svc.Name != "" {
			return svc.Name
		}
	}
	if groupBy == "project" && s.projects != nil {
		if p, err := s.projects.ProjectByID(ctx, key); err == nil && p.Name != "" {
			return p.Name
		}
	}
	return key
}
