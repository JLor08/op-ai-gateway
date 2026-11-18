// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func startNvidiaTestRuntime(t *testing.T) (root, worker int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	worker, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, worker
}

func TestNvidiaMeasureRuntimeProcessGroups(t *testing.T) {
	root, worker := startNvidiaTestRuntime(t)
	otherRoot, otherWorker := startNvidiaTestRuntime(t)
	binDir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--query-compute-apps=*) printf '%s\\n' \"$TEST_NVIDIA_APPS\";;\n--query-gpu=*) printf '%s\\n' \"$TEST_NVIDIA_GPUS\";;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, nvidiaSMI), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("TEST_NVIDIA_GPUS", "0, GPU-a\n1, GPU-b")

	tests := []struct {
		name string
		rows string
		pids []int
		want map[int]map[int]int
	}{
		{
			name: "root process still measures normally",
			rows: fmt.Sprintf("%d, GPU-a, 8000", root),
			pids: []int{root},
			want: map[int]map[int]int{root: {0: 8000}},
		},
		{
			name: "EngineCore child owns all VRAM",
			rows: fmt.Sprintf("%d, GPU-a, 88214", worker),
			pids: []int{root},
			want: map[int]map[int]int{root: {0: 88214}},
		},
		{
			name: "sum root and worker per GPU without duplicate root charging",
			rows: fmt.Sprintf("%d, GPU-a, 100\n%d, GPU-a, 8000\n%d, GPU-b, 4000", root, worker, worker),
			pids: []int{root, root},
			want: map[int]map[int]int{root: {0: 8100, 1: 4000}},
		},
		{
			name: "separate managed runtimes",
			rows: fmt.Sprintf("%d, GPU-a, 8000\n%d, GPU-a, 2000", worker, otherWorker),
			pids: []int{root, otherRoot},
			want: map[int]map[int]int{root: {0: 8000}, otherRoot: {0: 2000}},
		},
		{
			name: "unmanaged group is excluded",
			rows: fmt.Sprintf("%d, GPU-a, 2000", otherWorker),
			pids: []int{root},
		},
		{
			name: "unknown child GPU is not guessed",
			rows: fmt.Sprintf("%d, GPU-missing, 2000", worker),
			pids: []int{root},
		},
		{
			name: "unreadable and invalid PIDs stay unknown",
			rows: fmt.Sprintf("0, GPU-a, 2000\n-1, GPU-a, 2000\n%d, GPU-a, 2000\n%d, GPU-a, 2000", math.MaxInt32, int64(1)<<32+int64(worker)),
			pids: []int{root},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_NVIDIA_APPS", tt.rows)
			m := &nvidiaComputeAppsMeasurer{}
			if got := m.measure(tt.pids); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("measure = %v, want %v", got, tt.want)
			}
		})
	}

	// A new GPU used only by a child must invalidate the UUID cache too.
	m := &nvidiaComputeAppsMeasurer{}
	m.setCachedIndex(map[string]int{"GPU-a": 0})
	t.Setenv("TEST_NVIDIA_APPS", fmt.Sprintf("%d, GPU-b, 4000", worker))
	if got := m.measure([]int{root}); !reflect.DeepEqual(got, map[int]map[int]int{root: {1: 4000}}) {
		t.Fatalf("child UUID cache refresh = %v, want GPU 1 with 4000 MiB", got)
	}
}
