// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// helperRAPLMaxBody caps the /v1/rapl response body.
const helperRAPLMaxBody = 4096

// Overridable in tests.
var (
	helperGOOS    = runtime.GOOS
	helperGeteuid = os.Geteuid
)

// helperEnabled reports whether the privileged-helper power source applies: only
// a non-root Linux agent with a configured socket needs it (root reads RAPL
// directly).
func helperEnabled(socket string) bool {
	return helperGOOS == "linux" && helperGeteuid() != 0 && strings.TrimSpace(socket) != ""
}

// helperPowerCollector derives CPU/system watts from the RAPL counters the
// privileged helper serves, with the same per-zone rules as raplCollector:
// package* zones sum to CPU, psys is system, the first sample per zone yields
// nothing. A failed fetch or a vanished zone drops its previous reading so no
// delta ever spans a gap.
//
// Δt is the difference of the helper's read_ns timestamps, so a reading served
// from the helper's 250 ms cache (same read_ns as the previous one) yields nil
// and keeps the baselines instead of a false 0 W followed by a doubled value.
// When either response lacks read_ns (an older helper), the agent clock times
// the delta.
//
// Logging: failures at Debug only (the helper may simply not be installed);
// Info once on the first success.
type helperPowerCollector struct {
	client *http.Client
	now    func() time.Time
	log    *slog.Logger // nil: slog.Default() at the time of logging

	mu     sync.Mutex
	prev   map[string]raplReading // keyed by zone
	prevNS uint64                 // read_ns of the response prev came from; 0: none
	logged bool
}

// newHelperPowerCollector builds the collector around a helper HTTP client.
func newHelperPowerCollector(c *http.Client) *helperPowerCollector {
	return &helperPowerCollector{client: c, now: time.Now, prev: map[string]raplReading{}}
}

// Name identifies this collector.
func (c *helperPowerCollector) Name() string { return "helper" }

// Available is always true: whether the helper answers is only known per request.
func (c *helperPowerCollector) Available() bool { return true }

// Collect fetches the counters once. Never returns an error.
func (c *helperPowerCollector) Collect(ctx context.Context) (*float64, *float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var body HelperRAPL
	if err := fetchHelperJSON(ctx, c.client, HelperRAPLPath, helperRAPLMaxBody, &body); err != nil {
		c.logger().Debug("helper: rapl request failed", "err", err.Error())
		clear(c.prev)
		c.prevNS = 0
		return nil, nil, nil
	}
	if !c.logged {
		c.logged = true
		c.logger().Info("helper: serving RAPL counters", "zones", len(body.Domains))
	}

	if body.ReadNS != 0 && body.ReadNS == c.prevNS {
		return nil, nil, nil // the helper's cached reading: nothing new, keep the baselines
	}
	cur, cpu, psys := c.sumWatts(body.Domains, body.ReadNS, c.now())
	c.prev = cur // zones absent from this response lose their baseline
	c.prevNS = body.ReadNS
	return cpu, psys, nil
}

// sumWatts records the current reading of every package* and psys zone and
// derives watts for the zones that have a baseline: package zones sum to cpu,
// psys is system.
func (c *helperPowerCollector) sumWatts(domains []HelperRAPLDomain, readNS uint64, now time.Time) (cur map[string]raplReading, cpu, psys *float64) {
	cur = make(map[string]raplReading, len(domains))
	var pkgWatts float64
	havePkg := false
	for _, d := range domains {
		isPkg := strings.HasPrefix(d.Name, "package")
		if !isPkg && d.Name != "psys" {
			continue
		}
		r := raplReading{energyUJ: d.EnergyUJ, maxUJ: d.MaxEnergyRangeUJ, at: now}
		cur[d.Zone] = r
		watts, ok := c.zoneWatts(d.Zone, r, readNS)
		if !ok {
			continue
		}
		if isPkg {
			pkgWatts += watts
			havePkg = true
		} else {
			w := watts
			psys = &w
		}
	}
	if havePkg {
		cpu = &pkgWatts
	}
	return cur, cpu, psys
}

// zoneWatts is the average power of one zone since its baseline; ok is false
// without a baseline, a positive time delta or a non-negative rate.
func (c *helperPowerCollector) zoneWatts(zone string, r raplReading, readNS uint64) (float64, bool) {
	prev, seen := c.prev[zone]
	if !seen {
		return 0, false
	}
	dt := c.deltaSeconds(prev.at, r.at, readNS)
	if dt <= 0 {
		return 0, false
	}
	watts := raplDeltaJoules(prev.energyUJ, r.energyUJ, prev.maxUJ) / dt
	return watts, watts >= 0
}

// deltaSeconds is the time between the previous and the current reading: from
// the helper's read timestamps when both responses carry one, otherwise from the
// agent clock. A non-advancing timestamp yields 0 (no rate).
func (c *helperPowerCollector) deltaSeconds(prevAt, now time.Time, curNS uint64) float64 {
	if c.prevNS == 0 || curNS == 0 {
		return now.Sub(prevAt).Seconds()
	}
	if curNS <= c.prevNS {
		return 0
	}
	return float64(curNS-c.prevNS) / 1e9
}

func (c *helperPowerCollector) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}
