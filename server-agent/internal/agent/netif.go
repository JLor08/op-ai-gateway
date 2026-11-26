// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/sample"
	"sync"
	"time"
)

const (
	// interfaceStableFor is how long a changed interface list must stay
	// unchanged before it is reported, so a flapping link or a container
	// starting does not churn the gateway.
	interfaceStableFor = 10 * time.Second
	// interfaceCollectEvery throttles interface enumeration inside the
	// collect loop (which ticks once a second by default).
	interfaceCollectEvery = 5 * time.Second
)

// interfaceInventory decides when the host's LAN interface list rides on the
// telemetry sample: only when it differs from the last delivered list and has
// been observed unchanged for interfaceStableFor. It is modelled on
// binaryInventory. now is injectable for tests.
type interfaceInventory struct {
	now func() time.Time

	mu       sync.Mutex
	lastRun  time.Time // last time collection ran (throttle)
	ran      bool
	current  []sample.NetInterface
	curHash  [32]byte
	curSince time.Time
	observed bool
	sentHash [32]byte
	sentOnce bool
}

func newInterfaceInventory() *interfaceInventory {
	return &interfaceInventory{now: time.Now}
}

// interfaceSum is the canonical hash of a list: sha256 of its JSON.
func interfaceSum(list []sample.NetInterface) [32]byte {
	raw, err := json.Marshal(list)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(raw)
}

// observe records a freshly collected list. A list equal to the previous
// observation keeps its stability clock; any other restarts it.
func (i *interfaceInventory) observe(now time.Time, list []sample.NetInterface) {
	list = append([]sample.NetInterface{}, list...) // non-nil: an empty list is sent as []
	sum := interfaceSum(list)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.observed && sum == i.curHash {
		return
	}
	i.current, i.curHash, i.curSince, i.observed = list, sum, now, true
}

// pending returns the list to attach to the next sample, or nil when there is
// nothing new to say: nothing observed yet, the list equals the last delivered
// one, or it changed less than interfaceStableFor ago. The first list after
// start (or after resend) is pending at once.
func (i *interfaceInventory) pending() *[]sample.NetInterface {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.observed {
		return nil
	}
	if i.sentOnce {
		if i.sentHash == i.curHash || i.now().Sub(i.curSince) < interfaceStableFor {
			return nil
		}
	}
	list := append([]sample.NetInterface{}, i.current...)
	return &list
}

// delivered records that the sample carrying the list with hash sum was
// accepted by the transport.
func (i *interfaceInventory) delivered(sum [32]byte) {
	i.mu.Lock()
	i.sentHash, i.sentOnce = sum, true
	i.mu.Unlock()
}

// resend makes the next sample carry the list again though it did not change:
// a delivery is only the transport accepting the sample, so the agent calls
// this on reconnect and on the system-report cadence.
func (i *interfaceInventory) resend() {
	i.mu.Lock()
	i.sentOnce = false
	i.mu.Unlock()
}

// refresh collects through c at most once per interfaceCollectEvery and
// observes the result. A failure is logged at Debug and leaves the previous
// observation in place.
func (i *interfaceInventory) refresh(ctx context.Context, c *collector.InterfaceCollector) {
	now := i.now()
	i.mu.Lock()
	if i.ran && now.Sub(i.lastRun) < interfaceCollectEvery {
		i.mu.Unlock()
		return
	}
	i.lastRun, i.ran = now, true
	i.mu.Unlock()
	list, err := c.Collect(ctx)
	if err != nil {
		slog.Debug("interface collect failed", "err", err)
		return
	}
	i.observe(now, list)
}
