// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command fakewsl is a test-only stand-in for wsl.exe, built by TestMain beside
// stubchild. It understands exactly the invocations the agent's WSL launcher and
// its helper calls make -- "-d D -u U --cd DIR --exec <command...>" -- and
// plays the part of the distro's systemd user manager for them:
//
//   - systemd-run --user --scope --quiet --unit=U -- BIN ARGS...: records its
//     own pid as the unit's process in $FAKEWSL_STATE/U, changes into DIR and
//     execs BIN in place, so that pid IS the workload, as the scope's process
//     is under a real relay;
//   - systemctl --user kill --signal=SIG U.scope...: signals each unit's
//     recorded pid; an unknown unit exits 5, as systemctl does;
//   - systemctl --user list-units --plain --no-legend --all opagent-*.scope:
//     prints "U.scope loaded active running" for every recorded unit whose
//     process is alive;
//   - id -u: prints 1000;
//   - sh -c <script> sh <units...>: prints the contents of $FAKEWSL_SAMPLE (the
//     agent's own fixed sampling script; the fake does not run it) verbatim, as
//     all of wsl.exe's stdout: a test writes the script's sentinel line into
//     it, and anything wsl.exe itself would print ahead of that.
//
// Every invocation first appends one JSON line {argv, env} to $FAKEWSL_LOG, so a
// test can read back exactly what the agent ran and with which of the
// boundary-relevant variables. "-u root" and the shell form ("--" instead of
// "--exec") are refused: the agent must never use either. So is a call without
// "--cd": without it a real wsl.exe translates the caller's Windows working
// directory, which need not map into the distro, so every call names one.
//
// Never part of the module's production build: "testdata" directories are
// excluded from `go build ./...`, `go vet ./...`, and internal/archtest's package
// graph by the go tool itself.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// loggedEnv are the variables each log line reports, when set.
var loggedEnv = []string{"WSLENV", "XDG_RUNTIME_DIR", "WSL_UTF8", "PYTHONUNBUFFERED", "CUDA_DEVICE_ORDER"}

type logLine struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

func main() {
	writeLog()

	args := os.Args[1:]
	var user, dir string
	for len(args) > 0 && args[0] != "--exec" {
		if len(args) < 2 {
			fail(2, "fakewsl: option %q has no value", args[0])
		}
		switch args[0] {
		case "-d":
		case "-u":
			user = args[1]
		case "--cd":
			dir = args[1]
		default:
			fail(2, "fakewsl: unsupported option %q (only -d, -u, --cd before --exec)", args[0])
		}
		args = args[2:]
	}
	if len(args) == 0 {
		fail(2, "fakewsl: no --exec: the shell form must never be used")
	}
	if dir == "" {
		fail(2, "fakewsl: no --cd: wsl.exe would have to translate the caller's Windows working directory")
	}
	if user == "" || user == "root" || strings.Trim(user, "0") == "" {
		fail(2, "fakewsl: refusing user %q: the agent must name a non-root user", user)
	}
	rest := args[1:]
	if len(rest) == 0 {
		fail(2, "fakewsl: --exec without a command")
	}

	switch {
	case rest[0] == "systemd-run":
		systemdRun(rest[1:], dir)
	case len(rest) >= 3 && rest[0] == "systemctl" && rest[1] == "--user" && rest[2] == "kill":
		systemctlKill(rest[3:])
	case len(rest) >= 3 && rest[0] == "systemctl" && rest[1] == "--user" && rest[2] == "list-units":
		listUnits()
	case len(rest) == 2 && rest[0] == "id" && rest[1] == "-u":
		fmt.Println("1000")
	case len(rest) >= 2 && rest[0] == "sh" && rest[1] == "-c":
		sample()
	default:
		fail(127, "fakewsl: unsupported command %q", strings.Join(rest, " "))
	}
}

func writeLog() {
	path := os.Getenv("FAKEWSL_LOG")
	if path == "" {
		return
	}
	line := logLine{Argv: os.Args[1:], Env: map[string]string{}}
	for _, name := range loggedEnv {
		if v, ok := os.LookupEnv(name); ok {
			line.Env[name] = v
		}
	}
	b, err := json.Marshal(line)
	if err != nil {
		fail(1, "fakewsl: marshal log line: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fail(1, "fakewsl: open log: %v", err)
	}
	// One write per line: concurrent invocations append whole lines.
	if _, err := f.Write(append(b, '\n')); err != nil {
		fail(1, "fakewsl: write log: %v", err)
	}
	_ = f.Close()
}

func stateDir() string {
	dir := os.Getenv("FAKEWSL_STATE")
	if dir == "" {
		fail(1, "fakewsl: FAKEWSL_STATE is not set")
	}
	return dir
}

// systemdRun handles "--user --scope --quiet --unit=U -- BIN ARGS...".
func systemdRun(args []string, dir string) {
	var unit string
	for len(args) > 0 && args[0] != "--" {
		if u, ok := strings.CutPrefix(args[0], "--unit="); ok {
			unit = u
		}
		args = args[1:]
	}
	if unit == "" || len(args) < 2 {
		fail(1, "fakewsl: systemd-run needs --unit=U -- BIN")
	}
	bin, argv := args[1], args[1:]
	if err := os.WriteFile(filepath.Join(stateDir(), unit), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fail(1, "fakewsl: record unit: %v", err)
	}
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			fail(1, "fakewsl: cd %s: %v", dir, err)
		}
	}
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		fail(1, "fakewsl: exec %s: %v", bin, err)
	}
}

// systemctlKill handles "--signal=SIG U.scope...".
func systemctlKill(args []string) {
	var sig syscall.Signal
	var units []string
	for _, a := range args {
		if s, ok := strings.CutPrefix(a, "--signal="); ok {
			switch s {
			case "SIGTERM":
				sig = syscall.SIGTERM
			case "SIGKILL":
				sig = syscall.SIGKILL
			default:
				fail(1, "fakewsl: unsupported signal %q", s)
			}
			continue
		}
		units = append(units, a)
	}
	if sig == 0 || len(units) == 0 {
		fail(1, "fakewsl: systemctl kill needs --signal=SIG and a unit")
	}
	for _, u := range units {
		pid, ok := unitPID(strings.TrimSuffix(u, ".scope"))
		if !ok {
			fail(5, "Failed to kill unit %s: Unit %s not loaded.", u, u)
		}
		// A process that is already gone is the goal reached, not an error.
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			fail(1, "fakewsl: kill %d: %v", pid, err)
		}
	}
}

func listUnits() {
	entries, err := os.ReadDir(stateDir())
	if err != nil {
		fail(1, "fakewsl: read state: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		pid, ok := unitPID(name)
		if ok && syscall.Kill(pid, 0) == nil {
			fmt.Printf("%s.scope loaded active running %s\n", name, name)
		}
	}
}

func sample() {
	path := os.Getenv("FAKEWSL_SAMPLE")
	if path == "" {
		fail(1, "fakewsl: FAKEWSL_SAMPLE is not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fail(1, "fakewsl: read sample: %v", err)
	}
	_, _ = os.Stdout.Write(b)
}

func unitPID(unit string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir(), unit))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(code)
}
