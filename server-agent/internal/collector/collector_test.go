// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

func TestDetectReturnsOnlyAvailable(t *testing.T) {
	// DetectGPUCollectors must only return collectors whose Available() is true.
	// The count is environment-dependent (which CLIs/OS are present), so we only
	// assert the availability invariant, not a fixed length.
	for _, c := range DetectGPUCollectors() {
		if !c.Available() {
			t.Errorf("DetectGPUCollectors returned %q with Available()==false", c.Name())
		}
	}
}
