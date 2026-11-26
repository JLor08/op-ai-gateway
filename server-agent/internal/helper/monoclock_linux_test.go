// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import (
	"testing"
	"time"
)

func TestMonotonicNSAdvances(t *testing.T) {
	a := monotonicNS()
	time.Sleep(2 * time.Millisecond)
	b := monotonicNS()
	if a == 0 || b <= a {
		t.Fatalf("monotonicNS: %d then %d, want non-zero and advancing", a, b)
	}
}
