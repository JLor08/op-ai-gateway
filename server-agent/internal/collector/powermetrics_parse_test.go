// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

const powermetricsFixture = `*** Sampled system activity (Mon Aug  3 12:00:00 2026) ***

**** Processor usage ****

E-Cluster Power: 45 mW
P-Cluster Power: 210 mW

CPU Power: 255 mW
GPU Power: 30 mW
ANE Power: 0 mW
Combined Power (CPU + GPU + ANE): 285 mW
`

func TestParsePowermetricsCPUWatts(t *testing.T) {
	w := parsePowermetricsCPUWatts([]byte(powermetricsFixture))
	if w == nil {
		t.Fatal("CPU watts = nil, want 0.255 (255 mW)")
	}
	// Must parse the anchored "CPU Power:" line, NOT "E-Cluster Power" or "Combined Power".
	if *w < 0.2549 || *w > 0.2551 {
		t.Fatalf("CPU watts = %v, want 0.255", *w)
	}
}

func TestParsePowermetricsCPUWattsMissing(t *testing.T) {
	if w := parsePowermetricsCPUWatts([]byte("no cpu power here\n")); w != nil {
		t.Fatalf("want nil for a missing CPU Power line, got %v", *w)
	}
}
