// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"io/fs"
	"op-ai-server-agent/internal/sample"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// cacheSizeTTL is how long a member's cache size is reused before it is
// measured again.
const cacheSizeTTL = 60 * time.Second

// cacheSizer reports the size of directories, measured off the caller's
// goroutine: a read returns the last measurement at once and, when it is older
// than cacheSizeTTL, starts one background walk, so the telemetry collect loop
// never waits on the disk and a large cache is never walked every sample.
type cacheSizer struct {
	walk func(dir string) int64
	now  func() time.Time

	mu   sync.Mutex
	ents map[string]*cacheEntry
}

type cacheEntry struct {
	mb   int
	at   time.Time
	busy bool
}

func newCacheSizer(walk func(string) int64) *cacheSizer {
	if walk == nil {
		walk = dirBytes
	}
	return &cacheSizer{walk: walk, now: time.Now, ents: map[string]*cacheEntry{}}
}

// dirBytes is the total size of the regular files below dir; a missing
// directory has size 0.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// mb returns dir's last measured size in whole MiB (0 until the first
// measurement finishes).
func (c *cacheSizer) mb(dir string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.ents[dir]
	if e == nil {
		e = &cacheEntry{}
		c.ents[dir] = e
	}
	if !e.busy && (e.at.IsZero() || c.now().Sub(e.at) >= cacheSizeTTL) {
		e.busy = true
		go c.measure(dir, e)
	}
	return e.mb
}

func (c *cacheSizer) measure(dir string, e *cacheEntry) {
	n := c.walk(dir)
	c.mu.Lock()
	e.mb, e.at, e.busy = int(n>>20), c.now(), false
	c.mu.Unlock()
}

// fillClusterTelemetry copies the cluster-related state of one runtime row
// onto its sample: the kind, a member's merged live state, a head's launch
// facts and the member that blocks a waiting spec.
func (a *Agent) fillClusterTelemetry(rs *sample.RuntimeSample, st runtimectl.Status, cacheRoot string) {
	rs.Kind = st.Kind
	rs.AdmissionBlockedBy = st.AdmissionBlockedBy
	if st.Cluster != nil {
		rs.Cluster = &sample.RuntimeClusterSample{Launched: slices.Clone(st.Cluster.Launched)}
	}
	if st.Kind != runtimectl.SpecKindRPCServer {
		return
	}
	r := &sample.RuntimeRPCSample{Protocol: st.RPCProtocol}
	var breaches []runtimectl.RPCBreach
	if st.RPC != nil {
		r.HeldBy, r.HeldSince, r.ReservedUntil = st.RPC.HeldBy, st.RPC.HeldSince, st.RPC.ReservedUntil
		r.DeviceCount, r.BindMismatch = st.RPC.DeviceCount, st.RPC.BindMismatch
		if r.Protocol == "" {
			r.Protocol = st.RPC.Protocol
		}
	}
	if a.relays != nil {
		if tel, ok := a.relays.telemetry(st.SpecID); ok {
			r.BindMismatch = r.BindMismatch || tel.BindMismatch
			r.Stalled = tel.Stalled
			breaches = append(breaches, tel.Breaches...)
		}
	}
	if tel, ok := a.rpcObs.telemetry(st.SpecID); ok {
		breaches = append(breaches, tel.Breaches...)
	}
	r.Breaches = mergeBreaches(breaches)
	if st.RPCMember != nil && st.RPCMember.CacheEnabled && st.RPCMember.HeadSpecID != "" && a.cacheSizes != nil {
		r.CacheMB = a.cacheSizes.mb(filepath.Join(cacheRoot, "rpc", st.RPCMember.HeadSpecID))
	}
	rs.RPC = r
}

// mergeBreaches dedupes breaches by peer (the newest sighting wins) and keeps
// the most recent maxRelayBreaches, oldest first.
func mergeBreaches(in []runtimectl.RPCBreach) []sample.RuntimeBreachSample {
	if len(in) == 0 {
		return nil
	}
	newest := map[string]time.Time{}
	for _, b := range in {
		if b.At.After(newest[b.Peer]) || newest[b.Peer].IsZero() {
			newest[b.Peer] = b.At
		}
	}
	out := make([]sample.RuntimeBreachSample, 0, len(newest))
	for peer, at := range newest {
		out = append(out, sample.RuntimeBreachSample{Peer: peer, At: at})
	}
	slices.SortFunc(out, func(x, y sample.RuntimeBreachSample) int {
		if c := x.At.Compare(y.At); c != 0 {
			return c
		}
		return strings.Compare(x.Peer, y.Peer)
	})
	if n := len(out); n > maxRelayBreaches {
		out = slices.Clone(out[n-maxRelayBreaches:])
	}
	return out
}
