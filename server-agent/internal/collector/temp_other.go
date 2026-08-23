// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package collector

// newNativeTempCollector returns nil on non-Linux: Windows CPU temp comes from
// the composed LHM collector (added additively by a later task); macOS has no
// CGO-free source wired in today (see the design spec's known limits) and
// reports nothing.
func newNativeTempCollector() TempCollector { return nil }
