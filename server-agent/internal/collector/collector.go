// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package collector gathers host and GPU performance metrics into sample types.
// Collectors sit behind small interfaces so the agent loop can compose a real
// gopsutil host collector with auto-detected GPU collectors and an optional
// inference /metrics scraper.
package collector

import (
	"context"
	"net/http"
	"op-ai-server-agent/internal/sample"
	"strings"
	"time"
)

// GPUCollector reports zero or more GPUs. Available() gates auto-detection.
type GPUCollector interface {
	Name() string    // "nvidia" | "amd" | "apple"
	Available() bool // the backing CLI/OS is present
	Collect(ctx context.Context) ([]sample.GPU, error)
}

// HostCollector reports the host portion of a Sample.
type HostCollector interface {
	Collect(ctx context.Context) (*sample.Host, error)
}

// Scraper (optional) augments active/queue counters from an inference /metrics
// endpoint.
type Scraper interface {
	Scrape(ctx context.Context) (active int, queue int, err error)
}

// DetectGPUCollectors returns the subset of the built-in GPU collectors
// (nvidia, amd, apple in that order) whose Available() reports true.
func DetectGPUCollectors() []GPUCollector {
	var out []GPUCollector
	for _, c := range []GPUCollector{NewNvidia(), NewAMD(), NewApple()} {
		if c.Available() {
			out = append(out, c)
		}
	}
	return out
}

// PowerCollector reports host-level power draw. Both returned pointers are nullable:
// a nil pointer means that metric is unavailable this cycle. Stateful implementations
// (RAPL energy deltas) return nil on their first Collect.
type PowerCollector interface {
	Name() string
	Available() bool
	Collect(ctx context.Context) (cpuWatts *float64, systemWatts *float64, err error)
}

// multiPowerCollector composes ordered sub-collectors and, per metric, takes the
// FIRST non-nil value (CPU and system independently). With the native collector first
// and the optional LHM-HTTP source second, native wins and LHM fills gaps (D9).
type multiPowerCollector struct {
	subs []PowerCollector
}

// Name identifies the composite.
func (m *multiPowerCollector) Name() string { return "power" }

// Available reports whether any sub-collector is available.
func (m *multiPowerCollector) Available() bool {
	for _, c := range m.subs {
		if c.Available() {
			return true
		}
	}
	return false
}

// Collect runs each sub-collector and keeps the first non-nil CPU and system value.
// A sub-collector error is swallowed (best-effort); the composite never errors.
func (m *multiPowerCollector) Collect(ctx context.Context) (*float64, *float64, error) {
	var cpu, system *float64
	for _, c := range m.subs {
		cw, sw, err := c.Collect(ctx)
		if err != nil {
			continue
		}
		if cpu == nil && cw != nil {
			cpu = cw
		}
		if system == nil && sw != nil {
			system = sw
		}
	}
	return cpu, system, nil
}

// PowerSources returns the names of the active power sub-collectors (e.g.
// ["rapl", "lhm"], ["powermetrics"], or [] when none apply), for a startup
// diagnostic line so the operator can see at a glance whether the optional LHM
// source is even configured. A non-composite collector reports its own Name().
func PowerSources(pc PowerCollector) []string {
	if m, ok := pc.(*multiPowerCollector); ok {
		out := make([]string, 0, len(m.subs))
		for _, c := range m.subs {
			out = append(out, c.Name())
		}
		return out
	}
	if pc == nil {
		return nil
	}
	return []string{pc.Name()}
}

// PowerConfig carries the power-source options the collectors are composed from.
type PowerConfig struct {
	// LHMURL is the optional LibreHardwareMonitor /data.json URL.
	LHMURL string
	// PowercapRoot optionally replaces the native powercap root; read on Linux
	// and Windows (Windows: the virtiofs drive root), ignored on macOS.
	PowercapRoot string
	// HostpowerURL is the optional hostpower HTTP endpoint on the Proxmox host.
	HostpowerURL string
	// HostpowerToken is that endpoint's optional per-VM token (a secret).
	HostpowerToken string
	// HostpowerTLSFingerprint optionally pins the hostpower server certificate
	// (SHA-256, as ParseHostpowerFingerprint accepts it) for an https://
	// HostpowerURL. Empty: standard TLS verification. Not a secret.
	HostpowerTLSFingerprint string
	// HelperSocket is the privileged helper's Unix socket; empty disables the
	// helper power source.
	HelperSocket string
}

// nativeAndHostpowerSubs returns the leading power sub-collectors shared by both
// Detect* entry points: native RAPL, the privileged helper (Linux, non-root,
// socket configured), hwmon (Linux), then the hostpower HTTP source (when
// HostpowerURL != ""). The LHM source follows in the callers.
func nativeAndHostpowerSubs(cfg PowerConfig) []PowerCollector {
	var subs []PowerCollector
	if native := newNativePowerCollector(cfg.PowercapRoot); native != nil { //nolint:staticcheck // newNativePowerCollector returns nil only on the !linux && !darwin build (power_other.go); always-true on this build is an intentional cross-platform guard
		subs = append(subs, native)
	}
	if helperEnabled(cfg.HelperSocket) {
		subs = append(subs, newHelperPowerCollector(NewHelperHTTPClient(cfg.HelperSocket)))
	}
	if hw := newNativeHwmonCollector(); hw != nil { //nolint:staticcheck // nil only off Linux; always-true on this build is an intentional cross-platform guard
		subs = append(subs, hw)
	}
	if hp := strings.TrimSpace(cfg.HostpowerURL); hp != "" {
		subs = append(subs, newHostpowerHTTPCollector(hp, cfg.HostpowerToken, cfg.HostpowerTLSFingerprint, nil))
	}
	return subs
}

// DetectPowerCollector composes, in this order: the OS-native collector (RAPL
// on Linux, powermetrics on macOS, a host-provided powercap tree elsewhere when
// PowercapRoot is set), the privileged helper (Linux, non-root agent,
// HelperSocket set), hwmon (Linux), the hostpower HTTP source (HostpowerURL
// set) and the LHM-HTTP source (LHMURL set). The result is always non-nil (an
// empty composite when nothing applies) and safe to Collect.
func DetectPowerCollector(cfg PowerConfig) PowerCollector {
	subs := nativeAndHostpowerSubs(cfg)
	if strings.TrimSpace(cfg.LHMURL) != "" {
		subs = append(subs, newLHMPowerCollector(cfg.LHMURL, &http.Client{Timeout: 5 * time.Second}))
	}
	return &multiPowerCollector{subs: subs}
}

// TempCollector reports the host CPU package temperature in °C. The returned
// pointer is nullable: nil = "not measured" (best-effort).
type TempCollector interface {
	Name() string
	Available() bool
	Collect(ctx context.Context) (*float64, error)
}

// multiTempCollector composes ordered sub-collectors; the first non-nil reading wins.
type multiTempCollector struct{ subs []TempCollector }

// Name identifies the composite.
func (m *multiTempCollector) Name() string { return "temp" }

// Available reports whether any sub-collector is available.
func (m *multiTempCollector) Available() bool {
	for _, c := range m.subs {
		if c.Available() {
			return true
		}
	}
	return false
}

// Collect runs each available sub-collector in order and returns the first
// non-nil reading. A sub-collector error is tolerated as long as a later sub
// yields a value; if none do, the first error is surfaced (for diagnostics) —
// the composite never fails a cycle just because one source errored.
func (m *multiTempCollector) Collect(ctx context.Context) (*float64, error) {
	var firstErr error
	for _, c := range m.subs {
		if !c.Available() {
			continue
		}
		t, err := c.Collect(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, firstErr
}

// TempSources lists the composed sub-collector names (for -v diagnostics), mirroring PowerSources.
func TempSources(tc TempCollector) []string {
	if m, ok := tc.(*multiTempCollector); ok {
		names := make([]string, 0, len(m.subs))
		for _, s := range m.subs {
			names = append(names, s.Name())
		}
		return names
	}
	return nil
}

// DetectTempCollector composes the OS-native CPU-temp collector (Linux only
// today; nil on other OSes, see temp_other.go), native first, with the optional
// LHM-HTTP source (when lhmURL != "") — the Windows CPU-temperature source, and
// a Linux fallback when hwmon is unreadable. The result is always non-nil (an
// empty composite when nothing applies) and safe to Collect.
func DetectTempCollector(lhmURL string) TempCollector {
	var subs []TempCollector
	if native := newNativeTempCollector(); native != nil {
		subs = append(subs, native)
	}
	if strings.TrimSpace(lhmURL) != "" {
		subs = append(subs, newLHMTempCollector(lhmURL, &http.Client{Timeout: 5 * time.Second}))
	}
	return &multiTempCollector{subs: subs}
}

// DetectPowerAndTempCollectors is DetectPowerCollector and DetectTempCollector
// combined: same composition (native first, then the optional LHM-HTTP
// source), but when lhmURL != "" both composites' LHM sub-collectors share ONE
// lhmSource instead of each building their own. main.go calls the power and
// temp collectors back-to-back every telemetry cycle, so without this they'd
// independently GET + parse the identical LibreHardwareMonitor /data.json
// tree twice per cycle; sharing the source (lhm_source.go) collapses that to
// one GET+parse, with each collector still doing its own sensor-tree walk
// (findLHMPower / findLHMTemp) over the shared result. Use this instead of
// calling DetectPowerCollector/DetectTempCollector separately whenever both
// are needed.
func DetectPowerAndTempCollectors(cfg PowerConfig) (PowerCollector, TempCollector) {
	powerSubs := nativeAndHostpowerSubs(cfg)
	var tempSubs []TempCollector
	if native := newNativeTempCollector(); native != nil {
		tempSubs = append(tempSubs, native)
	}
	if strings.TrimSpace(cfg.LHMURL) != "" {
		source := newLHMSource(cfg.LHMURL, &http.Client{Timeout: 5 * time.Second})
		powerSubs = append(powerSubs, newLHMPowerCollectorFromSource(source))
		tempSubs = append(tempSubs, newLHMTempCollectorFromSource(source))
	}
	return &multiPowerCollector{subs: powerSubs}, &multiTempCollector{subs: tempSubs}
}
