// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"fmt"
	"op-ai-gateway/internal/provider"
	"testing"
)

// TestEveryReaderPicksTheSameRepresentativeRow pins the one rule that decides
// which of a spec's status rows on a server speaks for the spec -- the running
// instance-0 row, else the running row with the lowest instance index, else
// the instance-0 row, else the row with the lowest instance index -- and that
// every reader applies it: the context and capability write-backs
// (foldInstanceRows' Primary, primaryRowIndexes over the raw samples), the
// model-servers row (runtimeRowRepresentative), the image options
// (specImageOptions) and a benchmark's reading of its target
// (benchmarkSpecStatus). A crashed instance 0 beside a burst instance that
// serves must not stand for the spec in any of them, and no two of them may
// pick different rows for one frame.
func TestEveryReaderPicksTheSameRepresentativeRow(t *testing.T) {
	cases := []struct {
		name string
		rows []RuntimeStatusDTO
		want int // the instance index of the row that speaks
	}{
		{"instance 0 crashed, a burst instance serves", []RuntimeStatusDTO{
			instanceStatus("S", 0, "crashed", 0), instanceStatus("S", 2, "running", 4242),
		}, 2},
		{"instance 0 runs beside a burst instance, listed second", []RuntimeStatusDTO{
			instanceStatus("S", 1, "running", 4242), instanceStatus("S", 0, "running", 77),
		}, 0},
		{"two burst instances run, instance 0 idled out", []RuntimeStatusDTO{
			instanceStatus("S", 3, "running", 4242), instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "running", 77),
		}, 1},
		{"nothing runs: instance 0 before a starting burst instance", []RuntimeStatusDTO{
			instanceStatus("S", 1, "starting", 4242), instanceStatus("S", 0, "stopped", 0),
		}, 0},
		{"nothing runs and no instance-0 row: the lowest index", []RuntimeStatusDTO{
			instanceStatus("S", 2, "starting", 4242), instanceStatus("S", 1, "draining", 77),
		}, 1},
		{"a spec that is not replicable", []RuntimeStatusDTO{instanceStatus("S", 0, "stopped", 0)}, 0},
	}
	for _, tc := range cases {
		rows := make([]RuntimeStatusDTO, len(tc.rows))
		samples := make([]agentRuntimeSample, len(tc.rows))
		wantIndex := -1
		for i, row := range tc.rows {
			opts := provider.SdcppImageOptions{Samplers: []string{fmt.Sprintf("instance-%d", row.Instance)}}
			row.ImageOptions = &opts
			row.ContextSize = 1000 + row.Instance
			rows[i] = row
			samples[i] = agentRuntimeSample{SpecID: row.SpecID, Instance: row.Instance, State: row.State}
			if row.Instance == tc.want {
				wantIndex = i
			}
		}

		if got, ok := benchmarkSpecStatus(rows, "S"); !ok || got.Instance != tc.want {
			t.Errorf("%s: benchmarkSpecStatus = instance %d (ok %v), want instance %d", tc.name, got.Instance, ok, tc.want)
		}
		if got, ok := runtimeRowRepresentative(rows); !ok || got.Instance != tc.want {
			t.Errorf("%s: runtimeRowRepresentative = instance %d (ok %v), want instance %d", tc.name, got.Instance, ok, tc.want)
		}
		if got := specImageOptions(rows); got != rows[wantIndex].ImageOptions {
			t.Errorf("%s: specImageOptions = %+v, want instance %d's", tc.name, got, tc.want)
		}
		if primary := foldInstanceRows(rows)["S"].Primary; primary == nil || primary.Instance != tc.want {
			t.Errorf("%s: foldInstanceRows Primary = %+v, want instance %d's row", tc.name, primary, tc.want)
		}
		picked := primaryRowIndexes(len(samples), func(i int) (string, int, string) {
			return samples[i].SpecID, samples[i].Instance, samples[i].State
		})
		if got, ok := picked["S"]; !ok || got != wantIndex {
			t.Errorf("%s: primaryRowIndexes[S] = %d (ok %v), want sample %d (instance %d)", tc.name, got, ok, wantIndex, tc.want)
		}
	}
	if _, ok := runtimeRowRepresentative(nil); ok {
		t.Error("runtimeRowRepresentative(nil) reported a row, want none")
	}
	if _, ok := benchmarkSpecStatus([]RuntimeStatusDTO{instanceStatus("T", 0, "running", 1)}, "S"); ok {
		t.Error("benchmarkSpecStatus found a row for S in a frame without one, want none")
	}
}
