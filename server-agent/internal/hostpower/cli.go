// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"op-ai-server-agent/internal/collector"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Write cadence bounds for the host-side service.
const (
	DefaultInterval = 250 * time.Millisecond
	MinInterval     = 100 * time.Millisecond
)

// Config is the parsed hostpower command line.
type Config struct {
	Options
	Interval   time.Duration
	Verbose    bool
	Listen     string // empty: no HTTP endpoint
	Auth       AuthModes
	TokensPath string
	TLS        bool   // serve -listen over HTTPS
	TLSCert    string // certificate (PEM), read only with TLS
	TLSKey     string // private key (PEM), read only with TLS
}

func newFlagSet() (fs *flag.FlagSet, cfg *Config, auth *string) {
	cfg = &Config{}
	fs = flag.NewFlagSet("hostpower", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.OutDir, "out", DefaultOutDir, "directory for the synthetic powercap tree")
	fs.DurationVar(&cfg.Interval, "interval", DefaultInterval, "write interval (minimum 100ms)")
	fs.BoolVar(&cfg.Verbose, "v", false, "verbose (debug) logging")
	fs.StringVar(&cfg.Listen, "listen", "", "serve /v1/energy over HTTP (HTTPS with -tls) on host:port (off when empty)")
	auth = fs.String("auth", "mac,token", "HTTP identification modes: comma list of mac, token")
	fs.StringVar(&cfg.TokensPath, "tokens", DefaultTokensPath, "token store file for -auth token")
	fs.BoolVar(&cfg.TLS, "tls", false, "serve the -listen endpoint over HTTPS (TLS 1.3)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", DefaultTLSCertPath, "TLS certificate (PEM) for -tls; generated with -tls-key when both are absent")
	fs.StringVar(&cfg.TLSKey, "tls-key", DefaultTLSKeyPath, "TLS private key (PEM) for -tls")
	return fs, cfg, auth
}

// ParseArgs parses the arguments after "hostpower". Besides the flag.FlagSet
// errors it rejects positional arguments and an interval below MinInterval,
// which also guards the ticker in Run against a non-positive duration. -auth is
// validated only when -listen enables HTTP, and -tls needs -listen.
func ParseArgs(args []string) (Config, error) {
	fs, cfg, auth := newFlagSet()
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("hostpower: unexpected argument %q", fs.Arg(0))
	}
	if cfg.Interval < MinInterval {
		return Config{}, fmt.Errorf("hostpower: -interval must be at least %s", MinInterval)
	}
	if cfg.TLS && cfg.Listen == "" {
		return Config{}, errors.New("hostpower: -tls needs -listen")
	}
	cfg.Auth = AuthModes{MAC: true, Token: true} // the -auth default
	if cfg.Listen != "" {
		modes, err := ParseAuthModes(*auth)
		if err != nil {
			return Config{}, err
		}
		cfg.Auth = modes
	}
	return *cfg, nil
}

// msgNotReadable is the startup warning for a service that cannot read RAPL.
const msgNotReadable = "hostpower: RAPL energy_uj is not readable (run as root); guests would report 0 W"

// startupWarnings returns the startup checks' warnings for opts. notRoot is the
// caller's effective-UID check (only meaningful on Linux); the readability check
// covers a root service that still cannot read a package domain's energy_uj.
func startupWarnings(opts Options, notRoot bool) []string {
	var warns []string
	domains, _ := filepath.Glob(filepath.Join(opts.PowercapRoot, "intel-rapl:*"))
	if len(domains) == 0 {
		warns = append(warns, fmt.Sprintf("hostpower: no RAPL package domain under %s; is intel_rapl_common loaded and are we root?", opts.PowercapRoot))
	}
	if notRoot || (len(domains) > 0 && !packageEnergyReadable(domains)) {
		warns = append(warns, msgNotReadable)
	}
	qemuSlice := filepath.Join(opts.CgroupRoot, "qemu.slice")
	if fi, err := os.Stat(qemuSlice); err != nil || !fi.IsDir() {
		warns = append(warns, fmt.Sprintf("hostpower: no qemu.slice under %s; no running QEMU VMs or cgroup v1?", opts.CgroupRoot))
	}
	return warns
}

// packageEnergyReadable reports whether any package domain's energy_uj can be
// read.
func packageEnergyReadable(domains []string) bool {
	for _, dir := range domains {
		name, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(name)), "package") {
			continue
		}
		if _, err := os.ReadFile(filepath.Join(dir, "energy_uj")); err == nil {
			return true
		}
	}
	return false
}

const usageText = "usage: server-agent hostpower [-out dir] [-interval duration] [-listen host:port] [-auth mac,token] [-tokens path] [-tls] [-tls-cert path] [-tls-key path] [-v]\n" +
	"       server-agent hostpower token [-tokens path] create|revoke|list [vmid]\n" +
	"       server-agent hostpower tls [-tls-cert path] fingerprint\n"

// Main runs the hostpower mode and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

// run is Main with an injectable lifetime context.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "token" {
		return TokenMain(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "tls" {
		return TLSMain(args[1:], stdout, stderr)
	}
	cfg, err := ParseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fs, _, _ := newFlagSet()
		fs.SetOutput(stderr)
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "server-agent: %v\n", err)
		return 2
	}

	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})))

	opts := cfg.Options.withDefaults()
	if err := ensureOutDir(opts.OutDir); err != nil {
		fmt.Fprintf(stderr, "server-agent: hostpower: out dir: %v\n", err)
		return 2
	}
	slog.Info("hostpower: starting", "out", opts.OutDir, "interval", cfg.Interval)

	notRoot := runtime.GOOS == "linux" && os.Geteuid() != 0
	for _, w := range startupWarnings(opts, notRoot) {
		slog.Warn(w)
	}
	if err := RemoveStaleTemps(opts.OutDir); err != nil {
		slog.Warn("hostpower: removing stale temp files", "err", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	svc := New(opts)
	if cfg.Listen == "" {
		return runTicks(ctx, svc, cfg.Interval, stderr)
	}
	return serve(ctx, cancel, cfg, svc, stderr)
}

// serve runs the HTTP listener (HTTPS with -tls) next to the tick loop until
// either ends and returns the exit code.
func serve(ctx context.Context, cancel context.CancelFunc, cfg Config, svc *Service, stderr io.Writer) int {
	var tlsConfig *tls.Config
	if cfg.TLS {
		cert, generated, err := LoadOrCreateCert(cfg.TLSCert, cfg.TLSKey, time.Now, slog.Default())
		if err != nil {
			fmt.Fprintf(stderr, "server-agent: %v\n", err)
			return 2
		}
		slog.Info("hostpower: tls certificate", "cert", cfg.TLSCert,
			"sha256", collector.FormatHostpowerFingerprint(cert.Certificate[0]), "generated", generated)
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "server-agent: hostpower: listen: %v\n", err)
		return 2
	}
	var table *ObservationTable
	if cfg.Auth.MAC {
		table = NewObservationTable(30*time.Second, time.Now)
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		if err := StartCapture(ctx, port, table, slog.Default()); err != nil { //nolint:staticcheck // SA4023: the non-Linux stub always fails; the Linux build can succeed
			_ = ln.Close()
			fmt.Fprintf(stderr, "server-agent: hostpower: MAC mode capture: %v\n", err)
			return 2
		}
	}
	// The capture filters on the TCP port above; RemoteAddr and the local
	// address that MAC mode resolves still come from the TCP connection.
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	srv := NewServer(ln.Addr().String(), newHandler(cfg.Auth, svc, &IngressResolver{Table: table},
		NewTokenStore(cfg.TokensPath), slog.Default(), time.Now))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("hostpower: serving", "listen", ln.Addr().String(), "auth", authSummary(cfg.Auth), "tls", cfg.TLS)

	code := 0
	tickDone := make(chan int, 1)
	go func() { tickDone <- runTicks(ctx, svc, cfg.Interval, stderr) }()
	select {
	case code = <-tickDone:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("hostpower: http server stopped", "err", err)
			code = 1
		}
		cancel()
		<-tickDone
	}
	shutdownCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("hostpower: http shutdown", "err", err)
	}
	return code
}

// ensureOutDir creates the out directory with mode 0700, or tightens an
// existing one to 0700, so no unprivileged host process can read a VM's tree
// or swap a <vmid> entry (openVMRoot relies on it). An existing path must be a
// real directory: a symlink or anything else is an error.
func ensureOutDir(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o700)
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink, not a directory", dir)
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case fi.Mode().Perm()&^0o700 != 0:
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func runTicks(ctx context.Context, svc *Service, interval time.Duration, stderr io.Writer) int {
	if err := svc.Run(ctx, interval); err != nil {
		fmt.Fprintf(stderr, "server-agent: hostpower: %v\n", err)
		return 1
	}
	return 0
}

func authSummary(m AuthModes) string {
	var parts []string
	if m.MAC {
		parts = append(parts, "mac")
	}
	if m.Token {
		parts = append(parts, "token")
	}
	return strings.Join(parts, ",")
}
