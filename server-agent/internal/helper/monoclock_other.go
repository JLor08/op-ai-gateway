// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package helper

// monotonicNS is 0 off Linux, where the helper cannot run; 0 means "no read
// timestamp" on the wire.
func monotonicNS() uint64 { return 0 }
