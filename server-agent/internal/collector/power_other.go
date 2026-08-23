// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux && !darwin

package collector

// newNativePowerCollector returns nil: there is no CGO-free native power path on this
// OS (Windows RAPL is ring-0/MSR). Windows relies entirely on the optional LHM-HTTP
// source composed by DetectPowerCollector.
func newNativePowerCollector() PowerCollector { return nil }
