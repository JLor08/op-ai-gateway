// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"context"

	"github.com/shirou/gopsutil/v4/sensors"
)

// gopsutilTempCollector reads the CPU package temperature from Linux hwmon
// sensors via gopsutil (no CGO, no root — the hwmon temp*_input files are
// world-readable). The picking heuristic itself lives in the OS-independent
// pickCPUTemp (temp_pick.go) so it stays unit-testable on any host.
type gopsutilTempCollector struct{}

// newNativeTempCollector returns the Linux gopsutil hwmon CPU-temperature collector.
func newNativeTempCollector() TempCollector { return &gopsutilTempCollector{} }

func (g *gopsutilTempCollector) Name() string    { return "gopsutil" }
func (g *gopsutilTempCollector) Available() bool { return true }

// Collect reads all hwmon temperature sensors and picks the CPU package reading
// via pickCPUTemp. A hard failure (no sensors readable at all) returns the
// error; a partial failure (gopsutil reports a Warnings error alongside some
// readable sensors — common on hosts with a few unreadable/optional hwmon
// files) is tolerated.
func (g *gopsutilTempCollector) Collect(ctx context.Context) (*float64, error) {
	stats, err := sensors.TemperaturesWithContext(ctx)
	if err != nil && len(stats) == 0 {
		return nil, err
	}
	proj := make([]sensorTemp, 0, len(stats))
	for _, s := range stats {
		proj = append(proj, sensorTemp{Key: s.SensorKey, Temp: s.Temperature})
	}
	return pickCPUTemp(proj), nil
}
