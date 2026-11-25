// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/collector"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Config is the parsed helper command line.
type Config struct {
	Socket, AgentUser, AgentGroup, PowercapRoot, DMITable string
	Verbose                                               bool
}

const defaultPowercapRoot = "/sys/class/powercap"

const usageText = "usage: server-agent helper -agent-user user [-agent-group group] [-socket path] [-powercap-root dir] [-dmi-table file] [-v]\n" +
	"       The socket directory's parent must already exist; the directory itself is\n" +
	"       created if missing and must be dedicated to the helper.\n"

func newFlagSet() (*flag.FlagSet, *Config) {
	cfg := &Config{}
	fs := flag.NewFlagSet("helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Socket, "socket", collector.HelperDefaultSocket, "Unix socket path; its directory's parent must exist and the directory must be dedicated to the helper")
	fs.StringVar(&cfg.AgentUser, "agent-user", "", "unprivileged agent user (name or numeric uid) allowed to connect (required)")
	fs.StringVar(&cfg.AgentGroup, "agent-group", "", "group that owns the socket directory (default: the user's primary group)")
	fs.StringVar(&cfg.PowercapRoot, "powercap-root", defaultPowercapRoot, "powercap sysfs root")
	fs.StringVar(&cfg.DMITable, "dmi-table", collector.DefaultDMITablePath, "raw SMBIOS table file")
	fs.BoolVar(&cfg.Verbose, "v", false, "verbose (debug) logging")
	return fs, cfg
}

// ParseArgs parses the helper command line. -agent-user is required and
// positional arguments are rejected.
func ParseArgs(args []string) (Config, error) {
	fs, cfg := newFlagSet()
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("helper: unexpected argument %q", fs.Arg(0))
	}
	if cfg.AgentUser == "" {
		return Config{}, errors.New("helper: -agent-user is required")
	}
	return *cfg, nil
}

// Main runs the helper mode and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	return run(context.Background(), args, stdout, stderr)
}

func run(parent context.Context, args []string, _, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := ParseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fs, _ := newFlagSet()
		fs.SetOutput(stderr)
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "server-agent: %v\n%s", err, usageText)
		return 2
	}
	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	id, err := ResolveIdentity(cfg.AgentUser, cfg.AgentGroup)
	if err != nil {
		fmt.Fprintf(stderr, "server-agent: %v\n", err)
		return 2
	}
	modules, err := collector.ReadSMBIOSMemoryModules(cfg.DMITable)
	if err != nil {
		log.Warn("helper: DMI table not readable; serving no memory modules", "err", err)
		modules = nil
	}
	if len(collector.ReadTopLevelRAPLDomains(cfg.PowercapRoot)) == 0 {
		log.Warn("helper: no readable RAPL zone", "powercap_root", cfg.PowercapRoot)
	}
	ln, err := Listen(cfg.Socket, id, log) //nolint:staticcheck // SA4023: the non-Linux stub always fails; the Linux build can succeed
	if err != nil {                        //nolint:staticcheck // SA4023: see above
		fmt.Fprintf(stderr, "server-agent: %v\n", err)
		return 2
	}
	root := cfg.PowercapRoot
	h := newHandler(func() []collector.HelperRAPLDomain { return collector.ReadTopLevelRAPLDomains(root) }, modules, time.Now, log)
	srv := newServer(h)
	log.Info("helper: serving", "socket", cfg.Socket, "agent_uid", id.UID, "agent_gid", id.GID)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	code := 0
	select {
	case <-ctx.Done():
		stop() // a second SIGINT/SIGTERM during shutdown kills the process
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "server-agent: helper: serve: %v\n", err)
			code = 1
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Shutdown closes the listener, and Go's unix listener unlinks its socket
	// on Close; inside the dedicated root-owned directory only root could have
	// replaced the entry, so no explicit removal is needed.
	_ = srv.Shutdown(shutCtx)
	return code
}
