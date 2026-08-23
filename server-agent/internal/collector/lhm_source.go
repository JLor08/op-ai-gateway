// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// lhmSourceTTL bounds how long a fetched+parsed LHM tree is reused before the
// next getTree call re-fetches. It only needs to bridge the power and temp
// sub-collectors' back-to-back Collect calls within ONE telemetry cycle
// (adjacent in agent.collectOnce, sub-millisecond apart via the shared
// DetectPowerAndTempCollectors instance) — it must NOT span consecutive
// cycles. It is therefore kept comfortably BELOW the fastest supported cadence
// (agentMinInterval = 250ms): at the 250ms floor the next cycle is >=250ms
// later, so with a 100ms TTL every cycle re-fetches a fresh reading (a larger
// TTL, e.g. 500ms, would straddle two 250ms cycles and make alternating cycles
// report the previous cycle's stale/duplicated LHM power+temp — halving the
// effective sampling resolution).
const lhmSourceTTL = 100 * time.Millisecond

// lhmSource GETs and decodes a LibreHardwareMonitor Remote Web Server
// /data.json tree, memoizing the result (success or failure) for
// lhmSourceTTL so that multiple LHM sub-collectors sharing one instance
// (power + temp) issue a single GET+parse per cycle instead of one each.
// Safe for concurrent use.
type lhmSource struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	cached    *lhmNode
	cachedErr error
	fetchedAt time.Time
}

// newLHMSource builds an LHM tree source for url. A nil client defaults to
// http.DefaultClient, mirroring newLHMPowerCollector/newLHMTempCollector. An
// empty (or whitespace-only) url yields an unavailable source.
func newLHMSource(url string, client *http.Client) *lhmSource {
	if client == nil {
		client = http.DefaultClient
	}
	return &lhmSource{url: strings.TrimSpace(url), client: client}
}

// Available reports whether an LHM URL is configured.
func (s *lhmSource) Available() bool { return s.url != "" }

// getTree returns the decoded /data.json sensor tree, fetching fresh only
// when the previous outcome (a tree or an error) is older than
// lhmSourceTTL. A cached error is replayed rather than retried within the
// window: callers (lhmPowerCollector/lhmTempCollector) already degrade any
// error to a nil metric, so replaying it changes nothing observable while
// still avoiding a second network round-trip. The fetch + parse themselves
// are unchanged from fetchLHMTree (same URL, same client/timeout, same
// decode).
func (s *lhmSource) getTree(ctx context.Context) (*lhmNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Since(s.fetchedAt) < lhmSourceTTL && (s.cached != nil || s.cachedErr != nil) {
		return s.cached, s.cachedErr
	}

	root, err := fetchLHMTree(ctx, s.url, s.client)
	s.fetchedAt = time.Now()
	s.cached = root
	s.cachedErr = err
	return root, err
}
