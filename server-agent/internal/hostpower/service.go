// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-server-agent/internal/collector"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Default locations used when an Options field is empty.
const (
	DefaultPowercapRoot = "/sys/class/powercap"
	DefaultProcStatPath = "/proc/stat"
	DefaultCgroupRoot   = "/sys/fs/cgroup"
	DefaultOutDir       = "/var/lib/op-hostpower"
)

// Options locates the host inputs and the output directory; empty fields take
// the Default* values.
type Options struct {
	PowercapRoot, ProcStatPath, CgroupRoot, OutDir string
}

func (o Options) withDefaults() Options {
	if o.PowercapRoot == "" {
		o.PowercapRoot = DefaultPowercapRoot
	}
	if o.ProcStatPath == "" {
		o.ProcStatPath = DefaultProcStatPath
	}
	if o.CgroupRoot == "" {
		o.CgroupRoot = DefaultCgroupRoot
	}
	if o.OutDir == "" {
		o.OutDir = DefaultOutDir
	}
	return o
}

// Service attributes the host's CPU package energy to QEMU VMs by CPU time and
// publishes each VM's share as a synthetic powercap tree.
type Service struct {
	opts   Options
	energy *collector.PackageEnergyReader
	log    *slog.Logger

	mu        sync.Mutex
	prevBusy  uint64
	hasBusy   bool
	prevUsage map[string]uint64 // last successful cpu.stat reading per VM
	// counters holds the monotonic energy counter per VM in microjoules. A
	// vanished VM's counter is deliberately never pruned, so a VM that stops and
	// starts again continues from its previous value instead of jumping.
	counters map[string]uint64
	// failing marks the VMs whose last tree write failed. Logging keys on the
	// transition only, never on the error text (which can carry a random temp
	// name), so a failure a guest causes in its own share every tick is logged
	// once, not at the tick rate.
	failing map[string]bool
	// live is the VM set the most recent Tick discovered; counters of stopped
	// VMs stay in counters but are not live.
	live map[string]bool
}

// New builds a Service; empty option fields take their defaults.
func New(opts Options) *Service {
	opts = opts.withDefaults()
	return &Service{
		opts:      opts,
		energy:    collector.NewPackageEnergyReader(opts.PowercapRoot),
		log:       slog.Default(),
		prevUsage: map[string]uint64{},
		counters:  map[string]uint64{},
		failing:   map[string]bool{},
		live:      map[string]bool{},
	}
}

// Tick runs one iteration. Every baseline advances on every tick, even when
// nothing can be attributed, so a later tick never spreads several intervals of
// CPU time over one interval of energy. It returns the joined tree-write errors.
// Tick holds s.mu for the whole iteration; the phase helpers below assume it is
// held.
func (s *Service) Tick() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	joules, haveEnergy := s.energy.DeltaJoules() // always called: advances its baseline
	busy, haveBusy := s.readBusy()
	vms := DiscoverVMs(s.opts.CgroupRoot)
	usage := s.readUsages(vms)

	if haveEnergy && haveBusy && s.hasBusy && busy >= s.prevBusy {
		s.attribute(joules, busy-s.prevBusy, usage)
	} else {
		s.log.Debug("hostpower: nothing to attribute this tick",
			"energy_delta", haveEnergy, "busy_read", haveBusy, "busy_baseline", s.hasBusy)
	}
	s.advanceBaselines(busy, haveBusy, usage)
	return s.writeTrees(vms)
}

// readUsages returns the cumulative CPU time of every VM whose cpu.stat is
// readable. Requires s.mu.
func (s *Service) readUsages(vms []string) map[string]uint64 {
	usage := make(map[string]uint64, len(vms))
	for _, vm := range vms {
		if u, ok := ReadVMUsageUsec(s.opts.CgroupRoot, vm); ok {
			usage[vm] = u
		}
	}
	return usage
}

// attribute adds each VM's share of joules to its counter, by its CPU time
// since the previous tick against the host's busy delta. VMs without a previous
// reading (new or returning) get no share this tick. Requires s.mu.
func (s *Service) attribute(joules float64, busyDelta uint64, usage map[string]uint64) {
	deltas := make(map[string]uint64, len(usage))
	for vm, u := range usage {
		if prev, ok := s.prevUsage[vm]; ok && u >= prev {
			deltas[vm] = u - prev
		}
	}
	for vm, share := range Shares(busyDelta, deltas) {
		s.ensureCounter(vm)
		s.counters[vm] += AttributeUJ(joules, share)
	}
}

// advanceBaselines records this tick's busy time and VM usages as the next
// tick's baselines. Requires s.mu.
func (s *Service) advanceBaselines(busy uint64, haveBusy bool, usage map[string]uint64) {
	if haveBusy {
		s.prevBusy, s.hasBusy = busy, true
	} else {
		// No baseline: the next readable /proc/stat re-baselines instead of
		// spreading two intervals of busy time over one interval of energy.
		s.hasBusy = false
	}
	s.prevUsage = usage // unreadable and vanished VMs drop out and re-baseline
}

// writeTrees publishes every live VM's counter, logs write-state changes and
// returns the joined write errors. Requires s.mu.
func (s *Service) writeTrees(vms []string) error {
	var errs []error
	live := make(map[string]bool, len(vms))
	for _, vm := range vms {
		live[vm] = true
		s.ensureCounter(vm)
		err := WriteTree(filepath.Join(s.opts.OutDir, vm), s.counters[vm])
		s.logWriteState(vm, err)
		if err != nil {
			errs = append(errs, err)
		}
	}
	s.live = live
	for vm := range s.failing {
		if !live[vm] {
			delete(s.failing, vm)
		}
	}
	return errors.Join(errs...)
}

// logWriteState logs at Warn (with the first error) when a VM's writes start
// failing, stays silent while they keep failing whatever the error, and logs
// once at Info when they recover. Requires s.mu.
func (s *Service) logWriteState(vm string, err error) {
	switch {
	case err != nil && !s.failing[vm]:
		s.failing[vm] = true
		s.log.Warn("hostpower: write failed", "vm", vm, "err", err)
	case err == nil && s.failing[vm]:
		delete(s.failing, vm)
		s.log.Info("hostpower: write recovered", "vm", vm)
	}
}

func (s *Service) readBusy() (uint64, bool) {
	b, err := os.ReadFile(s.opts.ProcStatPath)
	if err != nil {
		return 0, false
	}
	return ParseProcStatBusyUsec(string(b))
}

// ensureCounter seeds a first-seen VM from its persisted energy_uj.
func (s *Service) ensureCounter(vm string) {
	if _, ok := s.counters[vm]; !ok {
		s.counters[vm] = ReadCounter(filepath.Join(s.opts.OutDir, vm))
	}
}

// Run ticks every interval until ctx is done. A Tick error is never fatal and
// is not logged here: Tick already logs each VM's write-state changes.
func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = s.Tick()
		}
	}
}

// Counters returns a snapshot of the per-VM energy counters in microjoules.
func (s *Service) Counters() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.counters))
	for k, v := range s.counters {
		out[k] = v
	}
	return out
}

// LiveCounter returns vmid's energy counter in microjoules only when the VM was
// in the set the most recent Tick discovered. It reports false before the first
// Tick and for stopped VMs, whose counters stay in Counters().
func (s *Service) LiveCounter(vmid string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live[vmid] {
		return 0, false
	}
	return s.counters[vmid], true
}
