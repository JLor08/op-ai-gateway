// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux && !darwin

package collector

// newNativePowerCollector returns the guest RAPL collector reading a host-provided
// powercap tree (e.g. a virtiofs drive like Z:\ carrying the colon-free
// intel-rapl_0 domain), or nil when powercapRoot is empty: there is no CGO-free
// native power path on this OS (Windows RAPL is ring-0/MSR). Without a root, Windows
// relies on the optional LHM-HTTP source composed by DetectPowerCollector.
func newNativePowerCollector(powercapRoot string) PowerCollector {
	if powercapRoot == "" {
		return nil // a literal nil interface, not a typed-nil *raplCollector
	}
	return newRAPLCollector(powercapRoot, raplDomainGlobWindows)
}

// newNativeHwmonCollector returns nil: there is no hwmon on this OS.
func newNativeHwmonCollector() PowerCollector { return nil }
