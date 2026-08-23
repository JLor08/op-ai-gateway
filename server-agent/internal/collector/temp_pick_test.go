// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

// deref renders a nullable float pointer for a test failure message.
func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestPickCPUTemp(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		in   []sensorTemp
		want *float64
	}{
		{"intel package", []sensorTemp{{"coretemp_core0", 40}, {"coretemp_packageid0", 55}}, f(55)},
		{"amd tctl", []sensorTemp{{"k10temp_tctl", 61}, {"k10temp_tccd1", 58}}, f(61)},
		{"generic cpu_thermal", []sensorTemp{{"cpu_thermal", 47}}, f(47)},
		{"none", []sensorTemp{{"acpitz", 30}, {"nvme_composite", 44}}, nil},
		{"empty", nil, nil},
		{"zero ignored", []sensorTemp{{"coretemp_packageid0", 0}}, nil},
	}
	for _, c := range cases {
		got := pickCPUTemp(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Fatalf("%s: got %v want %v", c.name, deref(got), deref(c.want))
		}
	}
}
