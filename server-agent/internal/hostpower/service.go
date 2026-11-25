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
	// writeErr holds each VM's last logged write error, so a failure that
	// repeats every tick (a guest can cause one in its own share) is logged
	// once, not at the tick rate.
	writeErr map[string]string
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
		writeErr:  map[string]string{},
	}
}

// Tick runs one iteration. Every baseline advances on every tick, even when
// nothing can be attributed, so a later tick never spreads several intervals of
// CPU time over one interval of energy. It returns the joined tree-write errors.
func (s *Service) Tick() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	joules, haveEnergy := s.energy.DeltaJoules() // always called: advances its baseline
	busy, haveBusy := s.readBusy()
	vms := DiscoverVMs(s.opts.CgroupRoot)

	usage := make(map[string]uint64, len(vms))
	for _, vm := range vms {
		if u, ok := ReadVMUsageUsec(s.opts.CgroupRoot, vm); ok {
			usage[vm] = u
		}
	}

	if haveEnergy && haveBusy && s.hasBusy && busy >= s.prevBusy {
		deltas := make(map[string]uint64, len(usage))
		for vm, u := range usage {
			if prev, ok := s.prevUsage[vm]; ok && u >= prev {
				deltas[vm] = u - prev
			}
		}
		for vm, share := range Shares(busy-s.prevBusy, deltas) {
			s.ensureCounter(vm)
			s.counters[vm] += AttributeUJ(joules, share)
		}
	} else {
		s.log.Debug("hostpower: nothing to attribute this tick",
			"energy_delta", haveEnergy, "busy_read", haveBusy, "busy_baseline", s.hasBusy)
	}

	if haveBusy {
		s.prevBusy, s.hasBusy = busy, true
	} else {
		// No baseline: the next readable /proc/stat re-baselines instead of
		// spreading two intervals of busy time over one interval of energy.
		s.hasBusy = false
	}
	s.prevUsage = usage // unreadable and vanished VMs drop out and re-baseline

	var errs []error
	live := make(map[string]bool, len(vms))
	for _, vm := range vms {
		live[vm] = true
		s.ensureCounter(vm)
		err := WriteTree(filepath.Join(s.opts.OutDir, vm), s.counters[vm])
		s.logWriteResult(vm, err)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for vm := range s.writeErr {
		if !live[vm] {
			delete(s.writeErr, vm)
		}
	}
	return errors.Join(errs...)
}

// logWriteResult logs a VM's write failure at Warn when it first occurs or its
// message changes, and once at Info when the VM's writes recover.
func (s *Service) logWriteResult(vm string, err error) {
	last, failing := s.writeErr[vm]
	switch {
	case err != nil && (!failing || last != err.Error()):
		s.writeErr[vm] = err.Error()
		s.log.Warn("hostpower: write failed", "vm", vm, "err", err)
	case err == nil && failing:
		delete(s.writeErr, vm)
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
// is not logged here: Tick already logs each VM's write failure on change.
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
