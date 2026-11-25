// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"testing"
	"time"
)

func TestCaptureHealth(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	if table.captureHealthy() {
		t.Fatal("new table reports a healthy capture")
	}
	h := &captureHealth{table: table}
	if h.ok() {
		t.Error("first successful read reported as a recovery")
	}
	if !table.captureHealthy() {
		t.Fatal("capture not healthy after a successful read")
	}

	t0 := time.Unix(1_000_000, 0)
	if !h.failed(t0) {
		t.Error("first failure not warned")
	}
	if table.captureHealthy() {
		t.Error("capture healthy after a read failure")
	}
	if h.failed(t0.Add(30 * time.Second)) {
		t.Error("warned again within a minute")
	}
	if !h.failed(t0.Add(61 * time.Second)) {
		t.Error("persistent failure not warned again after a minute")
	}
	if !h.ok() {
		t.Error("recovery not reported")
	}
	if !table.captureHealthy() {
		t.Error("capture not healthy after recovery")
	}
	if h.ok() {
		t.Error("recovery reported twice")
	}
	if !h.failed(t0.Add(62 * time.Second)) {
		t.Error("a new failure period after recovery must warn at once")
	}
}
