// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for the WSL RAM sampler (#381): the agent's one fixed sampling script,
// the parse of what it prints, and the helper call that carries it into a
// distro.

func TestParseWSLSample(t *testing.T) {
	out := "unit opagent-a-m-1 8419880960 4535152640\nunit opagent-a-n-2 -\nmeminfo 28763116 21000000\n"
	ram, vm, err := parseWSLSample([]byte(out))
	if err != nil {
		t.Fatalf("parseWSLSample: %v", err)
	}
	// (8419880960 - 4535152640) B = 3704.7 MiB, floored: memory.current minus
	// inactive_file, the docker stats definition.
	if got := ram["opagent-a-m-1"]; got != 3704 {
		t.Errorf("ram[opagent-a-m-1] = %d, want 3704", got)
	}
	if _, ok := ram["opagent-a-n-2"]; ok {
		t.Errorf("a unit printed as - (no cgroup files) must be absent, got %v", ram)
	}
	if want := (wslVMInfo{TotalMB: 28088, AvailableMB: 20507}); vm != want {
		t.Errorf("vm = %+v, want %+v (kB floored to MiB)", vm, want)
	}

	// A unit whose inactive_file is at least memory.current is not a
	// measurement (realRAM's rule): absent, never a 0 or a negative figure.
	ram, _, err = parseWSLSample([]byte("unit u 4096 8192\nunit v 100 100\nmeminfo 2048 1024\n"))
	if err != nil {
		t.Fatalf("parseWSLSample: %v", err)
	}
	if len(ram) != 0 {
		t.Errorf("non-positive figures must be absent, got %v", ram)
	}

	// Windows line endings and a missing trailing newline are not garbage.
	if _, _, err := parseWSLSample([]byte("unit u 2097152 0\r\nmeminfo 2048 1024")); err != nil {
		t.Errorf("CRLF / no trailing newline: %v", err)
	}
}

func TestParseWSLSampleRefusesGarbage(t *testing.T) {
	for name, out := range map[string]string{
		"unknown line":            "unit u 1 0\nhello world\nmeminfo 2048 1024\n",
		"unit without figures":    "unit u\nmeminfo 2048 1024\n",
		"unit with one figure":    "unit u 1\nmeminfo 2048 1024\n",
		"unit with extra field":   "unit u 1 0 7\nmeminfo 2048 1024\n",
		"non-numeric current":     "unit u x 0\nmeminfo 2048 1024\n",
		"negative inactive":       "unit u 1 -5\nmeminfo 2048 1024\n",
		"no meminfo line":         "unit u 1048576 0\n",
		"meminfo without numbers": "meminfo\n",
		"meminfo non-numeric":     "meminfo a b\n",
		"meminfo zero total":      "meminfo 0 0\n",
		"two meminfo lines":       "meminfo 2048 1024\nmeminfo 2048 1024\n",
		"empty output":            "",
		"wsl.exe error text":      "There is no distribution with the supplied name.\n",
	} {
		t.Run(name, func(t *testing.T) {
			if ram, vm, err := parseWSLSample([]byte(out)); err == nil {
				t.Errorf("parseWSLSample(%q) = %v, %+v; want an error", out, ram, vm)
			}
		})
	}
}

// TestWSLSampleScriptReadsTheScopeCgroup runs the agent's real sampling script
// with this host's sh against a fake cgroup tree and meminfo: the two absolute
// roots are swapped for temporary ones, nothing else is touched. It pins that
// the script is valid POSIX sh, reads exactly the scope's two files under the
// caller's own uid, reports a unit without them as "-", treats a unit name
// that a shell would expand as an inert word, and prints the parseable shape.
func TestWSLSampleScriptReadsTheScopeCgroup(t *testing.T) {
	skipOnWindows(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	root := t.TempDir()
	uid := strconv.Itoa(os.Getuid())
	appSlice := filepath.Join(root, "cg", "user.slice", "user-"+uid+".slice", "user@"+uid+".service", "app.slice")
	scope := filepath.Join(appSlice, "opagent-n-m-1.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(scope, "memory.current"), "8419880960\n")
	write(filepath.Join(scope, "memory.stat"), "anon 3379527680\nfile 4933902336\nactive_file 398749696\ninactive_file 4535152640\n")
	meminfo := filepath.Join(root, "meminfo")
	write(meminfo, "MemTotal:       28763116 kB\nMemFree:        19000000 kB\nMemAvailable:   21000000 kB\n")

	script := wslSampleScript
	for _, swap := range [][2]string{
		{"/sys/fs/cgroup/", filepath.Join(root, "cg") + "/"},
		{"/proc/meminfo", meminfo},
	} {
		if n := strings.Count(script, swap[0]); n != 1 {
			t.Fatalf("wslSampleScript names %q %d times, want exactly once", swap[0], n)
		}
		script = strings.Replace(script, swap[0], swap[1], 1)
	}

	// A unit name a shell would expand must stay an inert positional word.
	// The agent never builds one (wslUnitName), so this guards the script's
	// quoting, not the input.
	hostile := "$(touch${IFS}" + filepath.Join(root, "pwned") + ")"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, sh, "-c", script, "sh", "opagent-n-m-1", "opagent-n-gone-2", hostile).Output()
	if err != nil {
		t.Fatalf("sampling script failed: %v\n%s", err, out)
	}
	want := "unit opagent-n-m-1 8419880960 4535152640\nunit opagent-n-gone-2 -\nunit " + hostile + " -\nmeminfo 28763116 21000000\n"
	if string(out) != want {
		t.Fatalf("script output:\n%s\nwant:\n%s", out, want)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a positional unit name was evaluated by the shell")
	}
	ram, vm, err := parseWSLSample(out)
	if err != nil {
		t.Fatalf("the script's own output does not parse: %v", err)
	}
	if ram["opagent-n-m-1"] != 3704 || vm != (wslVMInfo{TotalMB: 28088, AvailableMB: 20507}) {
		t.Errorf("parsed %v, %+v", ram, vm)
	}
}

// TestSampleWSLCallShape: one helper call per distro, the fixed script as ONE
// argv element after sh -c, "sh" as $0 and the units as positional arguments
// after it, the configured user, and every distro's answers merged.
func TestSampleWSLCallShape(t *testing.T) {
	type call struct {
		args []string
		env  []string
	}
	var calls []call
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the sample call carries no deadline")
		}
		calls = append(calls, call{args, env})
		switch args[1] {
		case "Debian":
			return []byte("unit opagent-n-d-1 2097152 1048576\nmeminfo 4096 2048\n"), nil
		default:
			return []byte("unit opagent-n-a-1 3145728 0\nunit opagent-n-b-2 -\nmeminfo 4096 1024\n"), nil
		}
	}
	pol := wslTestPolicy()
	env := wslHelperEnv(func(string) string { return "" }, -1)
	ram, vm := sampleWSL(run, pol, env, map[string][]string{
		wslTestDistro: {"opagent-n-a-1", "opagent-n-b-2"},
		"Debian":      {"opagent-n-d-1"},
	})
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want one per distro", len(calls))
	}
	// Distros are asked in sorted order, so the call log is reproducible.
	wantArgs := [][]string{
		{"-d", "Debian", "-u", "ai", "--exec", "sh", "-c", wslSampleScript, "sh", "opagent-n-d-1"},
		{"-d", wslTestDistro, "-u", "ai", "--exec", "sh", "-c", wslSampleScript, "sh", "opagent-n-a-1", "opagent-n-b-2"},
	}
	for i, c := range calls {
		if !slices.Equal(c.args, wantArgs[i]) {
			t.Errorf("call %d args = %q\nwant %q", i, c.args, wantArgs[i])
		}
		if !slices.Equal(c.env, env) {
			t.Errorf("call %d env = %q, want the helper env %q", i, c.env, env)
		}
	}
	if want := map[string]int{"opagent-n-a-1": 3, "opagent-n-d-1": 1}; !maps.Equal(ram, want) {
		t.Errorf("ram = %v, want %v", ram, want)
	}
	if vm == nil || vm.TotalMB != 4 {
		t.Errorf("vm = %+v, want one distro's meminfo (the VM is shared)", vm)
	}
}

// TestSampleWSLFailuresSayNothing: a failed call or an unparseable answer
// contributes nothing -- no figure, no VM reading -- and the other distros'
// answers still count. A root user is refused before anything runs.
func TestSampleWSLFailuresSayNothing(t *testing.T) {
	run := func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
		switch args[1] {
		case "Broken":
			return nil, errors.New("exit status 1: no such distribution")
		case "Garbled":
			return []byte("unit opagent-n-g-1 1048576 0\nnot a sample line\nmeminfo 4096 1024\n"), nil
		default:
			return []byte("unit opagent-n-a-1 3145728 0\nmeminfo 4096 1024\n"), nil
		}
	}
	pol := wslTestPolicy()
	ram, vm := sampleWSL(run, pol, nil, map[string][]string{
		"Broken": {"opagent-n-x-1"}, "Garbled": {"opagent-n-g-1"}, wslTestDistro: {"opagent-n-a-1"},
	})
	if want := map[string]int{"opagent-n-a-1": 3}; !maps.Equal(ram, want) {
		t.Errorf("ram = %v, want only the good distro's %v", ram, want)
	}
	if vm == nil {
		t.Error("the good distro's VM reading was dropped")
	}

	ram, vm = sampleWSL(run, pol, nil, map[string][]string{"Broken": {"opagent-n-x-1"}})
	if ram != nil || vm != nil {
		t.Errorf("an all-failed sample = %v, %+v; want nothing", ram, vm)
	}

	pol.User = "root"
	ran := false
	ram, vm = sampleWSL(func(context.Context, string, []string, []string) ([]byte, error) {
		ran = true
		return nil, nil
	}, pol, nil, map[string][]string{wslTestDistro: {"opagent-n-a-1"}})
	if ran || ram != nil || vm != nil {
		t.Errorf("a root user ran=%v ram=%v vm=%+v; want refused before the call", ran, ram, vm)
	}
}
