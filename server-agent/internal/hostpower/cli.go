// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Write cadence bounds for the host-side service.
const (
	DefaultInterval = 250 * time.Millisecond
	MinInterval     = 100 * time.Millisecond
)

func newFlagSet() (*flag.FlagSet, *Options, *time.Duration, *bool) {
	opts := &Options{}
	fs := flag.NewFlagSet("hostpower", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.OutDir, "out", DefaultOutDir, "directory for the synthetic powercap tree")
	interval := fs.Duration("interval", DefaultInterval, "write interval (minimum 100ms)")
	verbose := fs.Bool("v", false, "verbose (debug) logging")
	return fs, opts, interval, verbose
}

// ParseArgs parses the arguments after "hostpower". Besides the flag.FlagSet
// errors it rejects an interval below MinInterval, which also guards the
// ticker in Run against a non-positive duration.
func ParseArgs(args []string) (Options, time.Duration, bool, error) {
	fs, opts, interval, verbose := newFlagSet()
	if err := fs.Parse(args); err != nil {
		return Options{}, 0, false, err
	}
	if *interval < MinInterval {
		return Options{}, 0, false, fmt.Errorf("hostpower: -interval must be at least %s", MinInterval)
	}
	return *opts, *interval, *verbose, nil
}

// Main runs the hostpower mode and returns the process exit code.
func Main(args []string, stderr io.Writer) int {
	opts, interval, verbose, err := ParseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fs, _, _, _ := newFlagSet()
		fs.SetOutput(stderr)
		fmt.Fprintln(stderr, "usage: server-agent hostpower [-out dir] [-interval duration] [-v]")
		fs.PrintDefaults()
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "server-agent: %v\n", err)
		return 2
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})))

	opts = opts.withDefaults()
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "server-agent: hostpower: create out dir: %v\n", err)
		return 2
	}
	slog.Info("hostpower: starting", "out", opts.OutDir, "interval", interval)

	if m, _ := filepath.Glob(filepath.Join(opts.PowercapRoot, "intel-rapl:*")); len(m) == 0 {
		slog.Warn(fmt.Sprintf("hostpower: no RAPL package domain under %s; is intel_rapl_common loaded and are we root?", opts.PowercapRoot))
	}
	qemuSlice := filepath.Join(opts.CgroupRoot, "qemu.slice")
	if fi, err := os.Stat(qemuSlice); err != nil || !fi.IsDir() {
		slog.Warn(fmt.Sprintf("hostpower: no qemu.slice under %s; no running QEMU VMs or cgroup v1?", opts.CgroupRoot))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := New(opts).Run(ctx, interval); err != nil {
		fmt.Fprintf(stderr, "server-agent: hostpower: %v\n", err)
		return 1
	}
	return 0
}
