// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/theme"
	"op-ai-gateway/internal/tracing"
	"op-ai-gateway/internal/usage"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck())
	}
	if len(os.Args) > 1 && os.Args[1] == "-license" {
		fmt.Print(licenseNotice())
		os.Exit(0)
	}
	log.Printf("%s (AGPL-3.0-only) — source: %s", programName, sourceURL)

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// Warn once if AGENT_TLS_PORT is malformed: the panel/policy and the bind both
	// fall back to 8443 (effectiveAgentTLSPort), so nothing diverges, but the
	// operator should know the configured value was ignored.
	warnInvalidAgentTLSPort(cfg)

	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			log.Printf("gateway cleanup failed: %v", err)
		}
	}()

	fmt.Printf("OP AI Gateway listening on %s\n", cfg.Addr)
	log.Printf("background chat runs call back over loopback base %s", loopbackSelfBase())

	// The server shutdown context: agent WebSocket handlers derive each open
	// connection's lifetime from it (via gateway.Server.baseCtx). Cancelling it on
	// SIGTERM makes each stream send 1001 GoingAway and return, and is set as the
	// BaseContext on both listeners so a rolling restart does not hang on Shutdown.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.SetBaseContext(baseCtx)

	// Bring up the NetBird agent listener (inbound-isolation half) before the main
	// listener blocks. Fail-safe: any problem leaves the single-listener behavior
	// intact (the public agent path stays open) — it never crashes the process.
	mgr := startAgentListener(baseCtx, srv, cfg)
	// Auto-select the gateway peer + rebind the agent listener to its current IP on
	// the NetBird sync cadence, so a freshly-enrolled sidecar becomes the live agent
	// listener within one interval without a restart.
	stopPeerReconcile := startGatewayPeerReconcileLoop(srv, cfg, mgr, time.Duration(cfg.NetbirdSyncIntervalSeconds)*time.Second)
	defer stopPeerReconcile()

	mainSrv := newHTTPServer(baseCtx, cfg.Addr, srv)
	// Graceful shutdown: on SIGINT/SIGTERM cancel baseCtx FIRST (open agent WebSockets
	// send GoingAway + return), then drain the main listener. Today (no signal handler)
	// SIGTERM kills the process instantly, so this is strictly a graceful improvement.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutdown signal received; draining")
		cancelBase()
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = mainSrv.Shutdown(shutCtx)
	}()
	if err := serveMainListener(mainSrv, cfg.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err) //nolint:gocritic // process is exiting; defer cleanup is moot
	}
}

// certEdgeGateTestRemoteAddrEnv, when set, makes the main listener report a
// FIXED, caller-supplied address as the RemoteAddr of every accepted
// connection instead of the genuine TCP peer address.
//
// This exists SOLELY so the e2e:certificates suite (Plan B, the plaintext-
// refusal gate, scenario 4) can prove the gate's actual refuse/observe
// behavior against a REAL running gateway process. That suite's own test
// client and the gateway run on the SAME machine with no reverse proxy in
// front (deploy/README-certificates.md §8's own item 7: "a deployment where
// nginx reaches the gateway over loopback makes the gate inert AND
// unarmable"), so every genuine connection between them is, unavoidably,
// loopback (127.0.0.0/8 is ALL loopback to net.IP.IsLoopback, regardless of
// which of the 127.x.x.x addresses is used) -- and internal/gateway/
// edge_scheme.go's countsAsObservation/edgeGateInternalCaller PERMANENTLY
// exempt loopback (the gateway's own background chat runs call back over
// http://127.0.0.1:<port>/v1/chat/completions and must never be locked out).
// Without this override there is no way, from a same-machine harness, to
// exercise EITHER half of that gate: the arming precondition's observation
// and the refusal itself both read the identical RemoteAddr and would both
// silently no-op regardless of any header the test sends.
//
// Safe: a GENUINE internal caller (chat_runs.go) is exempted by
// internalAuthHeaderName FIRST, independent of RemoteAddr (see
// edgeGateInternalCaller's own doc comment), so it is UNAFFECTED by this
// override; every other use of RemoteAddr in this codebase is logging only
// (grep-verified against internal/gateway and cmd/gateway). Unset in every
// real deployment -- with it unset, main() takes the exact ListenAndServe()
// path it always has, byte-identical to before this existed.
//
// FAIL-SAFE-ONLY: wrapListenerWithFakeRemoteAddr REJECTS a loopback or
// unspecified value outright (see its own doc comment) -- setting this to
// "127.0.0.1:1" (or any other loopback/0.0.0.0/::/bare-":port" value) would
// otherwise make every connection look like a genuine internal caller,
// simultaneously making the gate INERT (it can never see a plaintext refusal)
// and PERMANENTLY UNARMABLE (it can never register an observation either) --
// exactly README-certificates.md §8.7's failure mode, now reachable from one
// misconfigured env var instead of a network-topology mistake -- and it would
// falsify every remote_addr in the audit log to boot. A non-loopback,
// non-unspecified value can only ever make the gate's checks MORE
// representative of a real hop, never less, so this lever can strengthen the
// gate's exercise but never weaken or disable it. Also, whenever this DOES
// engage, serveMainListener emits one slog.Warn naming this variable, so it
// cannot go unnoticed in a real deployment's logs.
const certEdgeGateTestRemoteAddrEnv = "OP_AI_GATEWAY_CERT_EDGE_GATE_TEST_REMOTE_ADDR"

// serveMainListener runs srv on addr, optionally wrapped so every accepted
// connection reports a fixed fake RemoteAddr -- see
// certEdgeGateTestRemoteAddrEnv's doc comment for why this exists and why it
// is safe. The env var is read here (not threaded through config.Config)
// because it is a narrow, test-only lever, matching how OP_AI_GATEWAY_DEV_AGENT_TOKEN
// is read directly with os.Getenv elsewhere in this file rather than becoming
// a first-class config field.
func serveMainListener(srv *http.Server, addr string) error {
	fake := strings.TrimSpace(os.Getenv(certEdgeGateTestRemoteAddrEnv))
	if fake == "" {
		return srv.ListenAndServe()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	wrapped, err := wrapListenerWithFakeRemoteAddr(ln, fake)
	if err != nil {
		ln.Close()
		return err
	}
	// One WARN at the moment this lever actually engages, naming the
	// variable (mirrors warnIfEncryptionKeysShared) -- an operator who finds
	// this set in a real deployment (it should never be) needs to see it
	// without having to already suspect it.
	slog.Warn(certEdgeGateTestRemoteAddrEnv+" is set; every connection accepted on the main listener "+
		"will report a fixed fake RemoteAddr instead of the genuine TCP peer -- this is a test-only lever "+
		"for e2e:certificates and must never be set in a real deployment",
		"fake_remote_addr", fake)
	return srv.Serve(wrapped)
}

// wrapListenerWithFakeRemoteAddr wraps ln so every net.Conn it Accepts reports
// fake (parsed as a host:port) as its RemoteAddr. Pure and separately testable
// from serveMainListener's ListenAndServe/Serve split.
//
// Loopback and unspecified (0.0.0.0/::, or a bare ":port" with no host) fake
// addresses are REJECTED. Both internal/gateway/edge_scheme.go's
// countsAsObservation and edgeGateInternalCaller treat loopback as "not a real
// hop" (the same exemption background chat runs depend on), so a loopback fake
// value would make the gate simultaneously INERT (it can never see a plaintext
// refusal -- exactly README-certificates.md §8.7's failure mode) and
// PERMANENTLY UNARMABLE (it can never register an observation either),
// silently, from one env var -- with no way to tell from the outside that the
// lever is even active, since every RemoteAddr it produces looks exactly like
// the one it would produce without the fake at all. Restricting this lever to
// non-loopback, non-unspecified values means it can only ever make the gate's
// checks MORE representative of a real reverse-proxy hop than genuine loopback
// traffic would, never less -- it cannot be used to disable the gate.
func wrapListenerWithFakeRemoteAddr(ln net.Listener, fake string) (net.Listener, error) {
	addr, err := net.ResolveTCPAddr("tcp", fake)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certEdgeGateTestRemoteAddrEnv, err)
	}
	if addr.IP == nil || addr.IP.IsUnspecified() || addr.IP.IsLoopback() {
		return nil, fmt.Errorf("%s: %q must be a non-loopback, non-unspecified address "+
			"(a loopback or unspecified value would make the plaintext gate both inert and permanently unarmable)",
			certEdgeGateTestRemoteAddrEnv, fake)
	}
	return &fakeRemoteAddrListener{Listener: ln, addr: addr}, nil
}

type fakeRemoteAddrListener struct {
	net.Listener
	addr net.Addr
}

func (l *fakeRemoteAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &fakeRemoteAddrConn{Conn: c, addr: l.addr}, nil
}

type fakeRemoteAddrConn struct {
	net.Conn
	addr net.Addr
}

func (c *fakeRemoteAddrConn) RemoteAddr() net.Addr { return c.addr }

// resolveAgentAddr computes the desired agent-listener bind address. Returns
// (addr, ok): ok=false means the address could not be determined THIS tick (e.g. a
// transient NetBird control-plane error or the module being off) — callers must
// NOT touch the current listener (a control-plane blip must never drop a valid
// data-plane listener); ok=true with addr=="" means no listener is wanted
// (genuinely unconfigured — no gateway peer selected). Precedence: explicit
// OP_AI_GATEWAY_AGENT_ADDR > selected gateway peer IP + AGENT_PORT > "".
// effectiveAgentPort returns the TCP port the agent-ingest listener binds: the port
// component of OP_AI_GATEWAY_AGENT_ADDR when that explicit override is set (it wins),
// else OP_AI_GATEWAY_AGENT_PORT (default 8081). The portal Service uses it to build
// the managed op-gw-agent-ingest NetBird policy (server->gateway telemetry ingress).
func effectiveAgentPort(cfg config.Config) string {
	port := cfg.AgentPort
	// Trim like resolveAgentAddr does before binding, so the policy port matches
	// the port the listener actually binds (a stray whitespace-suffixed env value
	// would otherwise churn an UpdatePolicy every fleet pass on a never-matching port).
	if addr := strings.TrimSpace(cfg.AgentAddr); addr != "" {
		if _, p, err := net.SplitHostPort(addr); err == nil {
			port = p
		}
	}
	return port
}

// explicitAgentBindHost returns only the host component of an explicitly
// configured agent listener address. An empty or malformed override is not a
// bind-host claim; the runtime listener may still resolve a NetBird peer later.
func explicitAgentBindHost(cfg config.Config) string {
	addr := strings.TrimSpace(cfg.AgentAddr)
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

// effectiveAgentTLSPort returns the VALIDATED TCP port the separate encrypted
// agent listener binds: the port component of OP_AI_GATEWAY_AGENT_TLS_ADDR when
// that explicit override is set (it wins), else OP_AI_GATEWAY_AGENT_TLS_PORT
// (default "8443"). This one helper is the single source shared by the panel/
// policy display dep (atoiOr over it) AND the bind (resolveAgentTLSAddr), so a
// malformed value can never make them diverge -- the panel/policy would otherwise
// advertise the atoiOr fallback (8443) while the bind tried the malformed port
// and failed, claiming a port nothing serves. An empty (unset) value keeps the
// 8443 default; a malformed or out-of-range value falls back to 8443 too
// (warnInvalidAgentTLSPort logs that once at startup). Kept as a string so the
// bind can net.JoinHostPort it directly.
func effectiveAgentTLSPort(cfg config.Config) string {
	port := rawAgentTLSPort(cfg)
	if !validTCPPort(port) {
		return "8443"
	}
	return port
}

// rawAgentTLSPort returns the configured separate-agent TLS port BEFORE
// validation, applying the ONE precedence rule shared by effectiveAgentTLSPort
// (the bind/display/policy value) and warnInvalidAgentTLSPort (the startup
// warning): the port component of OP_AI_GATEWAY_AGENT_TLS_ADDR when that explicit
// override is set, else OP_AI_GATEWAY_AGENT_TLS_PORT. Keeping this in one place is
// what guarantees the warning can never drift from the actual fallback.
func rawAgentTLSPort(cfg config.Config) string {
	port := strings.TrimSpace(cfg.AgentTLSPort)
	if addr := strings.TrimSpace(cfg.AgentTLSAddr); addr != "" {
		if _, p, err := net.SplitHostPort(addr); err == nil {
			port = strings.TrimSpace(p)
		}
	}
	return port
}

// validTCPPort reports whether s is a decimal TCP port in the usable range
// 1..65535. Empty, non-numeric, non-positive, and out-of-range values are all
// invalid -- effectiveAgentTLSPort maps every one of them to the 8443 default so
// display, policy, and bind agree.
func validTCPPort(s string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil && n >= 1 && n <= 65535
}

// warnInvalidAgentTLSPort logs ONE clear startup warning when the configured
// separate-agent TLS port is present but malformed/out-of-range, so an operator
// learns the panel, the op-gw-agent-ingest-tls policy, and the bind all fall back
// to 8443 rather than silently binding nothing. Called once from main (not from
// effectiveAgentTLSPort, which runs every reconcile tick via resolveAgentTLSAddr).
// An empty (unset) value is the documented default and warns nothing.
func warnInvalidAgentTLSPort(cfg config.Config) {
	raw := rawAgentTLSPort(cfg)
	if raw == "" || validTCPPort(raw) {
		return
	}
	slog.Warn("configured agent TLS port is invalid; the separate encrypted agent listener, the panel, and the op-gw-agent-ingest-tls policy all fall back to the default",
		"configured", raw, "fallback", "8443")
}

// atoiOr parses raw as a decimal int, falling back to def when raw is empty,
// malformed, or <= 0 -- the same permissive-fallback shape as config's own
// integer() loader helper, for a value that is only ever consumed after
// config.Load has already run (so it cannot reuse that loader directly).
func atoiOr(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func buildGatewayServer(cfg config.Config) (*gateway.Server, func() error, error) {
	// The one log buffer backs the default slog handler, the stdlib-log bridge,
	// and the Server's Logs dep (all tee to stderr) — it is what the portal Logs
	// view snapshots and streams.
	logs := logbuffer.NewBuffer(cfg.LogBufferSize, logbuffer.ParseLevel(cfg.LogLevel))

	var (
		deps    gateway.ServerDeps
		cleanup func() error
		err     error
	)
	switch strings.ToLower(strings.TrimSpace(cfg.DBDriver)) {
	case "", "memory":
		deps, cleanup, err = memoryDeps(cfg)
	case "sqlite":
		deps, cleanup, err = sqliteDeps(cfg)
	case "postgres":
		deps, cleanup, err = postgresDeps(cfg)
	default:
		return nil, nil, fmt.Errorf("unsupported OP_AI_GATEWAY_DB_DRIVER %q", cfg.DBDriver)
	}
	if err != nil {
		return nil, nil, err
	}
	deps.Logs = logs
	deps.AgentBinaryDir = cfg.AgentBinaryDir
	// Energy-attribution reconciler + idle-tracker tuning (shared across all 3
	// DB drivers, threaded here rather than duplicated in memoryDeps/
	// sqliteDeps/postgresDeps since they all funnel through this common
	// builder). EnergyBackfillWindow mirrors the telemetry retention window: a
	// usage event outside it has no telemetry left to attribute against anyway.
	deps.EnergySettleSeconds = cfg.EnergySettleSeconds
	deps.EnergyIdleWindowSeconds = cfg.EnergyIdleWindowSeconds
	deps.EnergyBackfillWindow = time.Duration(cfg.TelemetryRetentionHours) * time.Hour

	// Install the process-wide loggers now that deps construction (with its own
	// startup log sink) is done: from here on every slog record and stdlib-log
	// line tees to stderr AND appends to the buffer backing the portal Logs view.
	// The runtime request path — including handleAgentTelemetry's debug/warn
	// lines and everything main() logs after this returns — is fully captured.
	slog.SetDefault(slog.New(logs.Handler(os.Stderr)))
	log.SetOutput(logs.NewLogWriter(os.Stderr))

	warnIfEncryptionKeysShared(cfg)

	// Opt-in tracing. Default disabled ⇒ the dynamic sampler drops every span
	// (~zero overhead). The provider is installed as the OTel global and its
	// span-mirror processor writes TRACE-level lines through the slog handler
	// installed just above. Threaded into deps so /api/system/tracing can flip it
	// live; its Shutdown is folded into cleanup.
	tracer, err := tracing.Setup(tracing.Options{
		Enabled:      cfg.TracingEnabled,
		SampleRatio:  cfg.TracingSampleRatio,
		OTLPEndpoint: cfg.OTLPEndpoint,
	}, logs)
	if err != nil {
		return nil, nil, err
	}
	deps.Tracing = tracer
	deps.TracingOTLPSet = cfg.OTLPEndpoint != ""

	srv := gateway.New(deps)
	// Wire the affinity session-mode to the resolver from the stored setting
	// before serving. deps.Portal is the tracing decorator (not the concrete
	// *portal.Service, and RouteAffinitySessionMode is intentionally off the
	// portal.API interface), so the stored mode is read off the DTO via the
	// interface's existing SystemSettingsView — a single settings load. The PUT
	// handler updates it live thereafter. The resolver's zero value already
	// means client_session, so this only matters when legacy_header is stored.
	if srv.Resolver != nil {
		srv.Resolver.SetAffinitySessionMode(srv.Portal.SystemSettingsView(context.Background()).RouteAffinitySessionMode == "legacy_header")
	}
	// Start the scheduled-benchmark loop (a self-contained sibling of the health
	// loop) now that the Server exists, and fold its cancel into cleanup so it
	// stops with the rest. It is a no-op until an application enables the schedule.
	stopScheduler := srv.StartBenchmarkScheduler(cfg.BenchmarkScheduleDefaultSeconds)
	// Start the energy-attribution reconciler (energy_reconciler.go): a fixed-
	// interval background pass that attributes energy to un-priced usage events.
	// It needs the fully-built Server (Usage/Routes/Portal + the EnergyIdle
	// tracker gateway.New just constructed from the EnergySettleSeconds/
	// EnergyBackfillWindow/EnergyIdleWindowSeconds deps set above), so — like the
	// scheduler — it is started here rather than duplicated in each of the 3 DB
	// driver builders (they all funnel through this one buildGatewayServer call).
	energyCtx, cancelEnergy := context.WithCancel(context.Background())
	go srv.StartEnergyReconciler(energyCtx, time.Duration(cfg.EnergyReconcileIntervalSeconds)*time.Second)
	prevCleanup := cleanup
	cleanup = func() error {
		stopScheduler()
		cancelEnergy()
		_ = tracer.Shutdown(context.Background())
		if prevCleanup != nil {
			return prevCleanup()
		}
		return nil
	}
	return srv, cleanup, nil
}

// warnIfEncryptionKeysShared warns when the capture and certificate encryption
// keys are set to the SAME value. That silently defeats the separation the
// dedicated OP_AI_GATEWAY_CERT_ENCRYPTION_KEY exists to create: certificate
// private keys could no longer be scoped or rotated independently of payload
// captures, chat transcripts, the SMTP password and the NetBird admin token.
// Nothing else notices, because both ciphers are built independently and each
// works fine on its own.
//
// Advisory only -- it never blocks startup (an operator with one key today is
// still better off running than not). It logs the variable NAMES only, never any
// part of a key, and is placed here (after slog.SetDefault) so it covers all
// three DB drivers and lands in the portal Logs view too.
func warnIfEncryptionKeysShared(cfg config.Config) {
	capKey := strings.TrimSpace(cfg.CaptureEncryptionKey)
	certKey := strings.TrimSpace(cfg.CertEncryptionKey)
	// Compared case-INSENSITIVELY: hex.DecodeString accepts both cases, so
	// "abab…" and "ABAB…" decode to the SAME AES key -- the separation would be
	// silently ineffective while a case-sensitive compare stayed quiet.
	if capKey == "" || certKey == "" || !strings.EqualFold(capKey, certKey) {
		return
	}
	slog.Warn("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY and OP_AI_GATEWAY_CERT_ENCRYPTION_KEY are set to the same value; " +
		"give the certificate key its own value so certificate private keys can be scoped and rotated independently")
}

// buildCaptureCipher parses the hex capture encryption key into an AES-256-GCM
// cipher, or returns (nil, nil) when no key is configured. Capture is
// fail-closed: without a key nothing is ever captured. A malformed key is a
// fatal misconfiguration surfaced here (and by main's log.Fatal), never in
// config.Load. Only wired on the SQLite path (capture is SQLite-only).
func buildCaptureCipher(cfg config.Config) (*capture.Cipher, error) {
	key := strings.TrimSpace(cfg.CaptureEncryptionKey)
	if key == "" {
		return nil, nil
	}
	return capture.New(key)
}

// buildCertCipher parses the hex CERTIFICATE encryption key into an AES-256-GCM
// cipher, or returns (nil, nil) when no key is configured. It is the exact
// sibling of buildCaptureCipher for a DIFFERENT key
// (OP_AI_GATEWAY_CERT_ENCRYPTION_KEY): every certificate private key -- leaf
// keys, the ACME account key, the internal CA key -- is sealed with this cipher
// and with nothing else. There is deliberately NO fallback to the capture
// cipher.
//
// A missing key is NOT a startup failure: the certificate module is optional and
// off by default (cert_enabled=false), so requiring the key at boot would break
// every deployment that does not use certificates. The requirement binds where
// the secret is actually needed instead -- on a disk-backed store
// sealCertSecret refuses (ErrCertKeyRequired) rather than persist a private key
// in plaintext, and the reconcile surfaces that in cert_last_error. A MALFORMED
// key is a fatal misconfiguration surfaced here (and by main's log.Fatal),
// mirroring the capture cipher.
func buildCertCipher(cfg config.Config) (*capture.Cipher, error) {
	key := strings.TrimSpace(cfg.CertEncryptionKey)
	if key == "" {
		return nil, nil
	}
	c, err := capture.New(key)
	if err != nil {
		// capture.New prefixes its errors with "capture:", which would name the
		// WRONG variable at the exact moment the operator needs the right one.
		// Re-label with the certificate variable and strip that prefix; the key
		// VALUE is never included.
		return nil, fmt.Errorf("OP_AI_GATEWAY_CERT_ENCRYPTION_KEY: %s", strings.TrimPrefix(err.Error(), "capture: "))
	}
	return c, nil
}

// loadThemeRegistry loads the externally supplied theme registry from
// cfg.ThemesDir (see config.Config.ThemesDir / internal/theme.Load). A load
// error (anything other than the directory being empty/missing, which
// theme.Load already treats as "no external themes") is logged and
// swallowed here: external themes are an optional deployment feature, so a
// misreadable themes directory must never block gateway startup. The
// returned value may be nil on error; portal.NewService treats a nil
// ServiceDeps.Themes as an empty registry.
//
// portal.BuiltinThemeIDs() is passed as the reserved id set so an external
// theme directory named e.g. "matrix" is skipped (with a warning) at load
// time rather than shadowing the compiled built-in theme of the same id.
func loadThemeRegistry(cfg config.Config) *theme.Registry {
	reg, err := theme.Load(cfg.ThemesDir, portal.BuiltinThemeIDs()...)
	if err != nil {
		slog.Warn("failed to load external theme registry; continuing without external themes",
			"dir", cfg.ThemesDir, "error", err)
		return nil
	}
	return reg
}

// tokenLister is the store surface countActiveLoggingTokens needs to scan
// every user's tokens. *store.SQLStore satisfies it.
type tokenLister interface {
	ListUsers(ctx context.Context) ([]store.User, error)
	TokensByUser(ctx context.Context, userID string) ([]store.TokenRecord, error)
}

// countActiveLoggingTokens returns how many ACTIVE tokens have log_communication
// enabled, using only the store's existing per-user listers (no new store surface).
// It is a best-effort startup advisory: any query error yields 0 so a transient read
// never blocks the server build.
func countActiveLoggingTokens(ctx context.Context, s tokenLister) int {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, u := range users {
		tokens, err := s.TokensByUser(ctx, u.ID)
		if err != nil {
			return 0
		}
		for _, tok := range tokens {
			if tok.Status == store.TokenStatusActive && tok.LogCommunication {
				n++
			}
		}
	}
	return n
}

// captureEnabledCacheTTL bounds how long a CaptureEnabled hook trusts its last
// SystemSettings read before re-querying the store. Every proxied inference
// request calls the hook (via Server.capturingEnabled), and the sqlite path is
// non-WAL, so an uncached read would compete with the per-request usage/capture
// writes; 5s keeps the setting responsive to an operator's PUT while keeping
// steady-state read pressure negligible.
const captureEnabledCacheTTL = 5 * time.Second

// captureFlags carries the two capture-related system flags resolved from one
// SystemSettings read: the global kill switch (Enabled) and the opt-in override
// (Override). Bundling them lets the hot request path derive BOTH the
// CaptureEnabled and CaptureOverride hooks from a single cached settings read
// per TTL window (no second table scan per request).
type captureFlags struct{ Enabled, Override bool }

// newCaptureFlagsHook returns a reader that resolves capture_enabled and
// capture_override from ONE SystemSettings read through settings, fails open to
// the defaults (Enabled=true, Override=false) on a read error (consistent with
// the existing default-on-error activeTheme / activeCaptureRetentionDays), and
// caches the result for captureEnabledCacheTTL so the hot request path does not
// re-read the settings table on every call. now is injected so tests can
// advance the clock deterministically; production callers pass time.Now.
func newCaptureFlagsHook(settings portal.SystemSettingsStore, now func() time.Time) func() captureFlags {
	var (
		mu        sync.Mutex
		cached    captureFlags
		fetchedAt time.Time
	)
	return func() captureFlags {
		mu.Lock()
		defer mu.Unlock()
		if !fetchedAt.IsZero() && now().Sub(fetchedAt) < captureEnabledCacheTTL {
			return cached
		}
		values, err := settings.SystemSettings(context.Background())
		if err != nil {
			cached = captureFlags{Enabled: true, Override: false}
		} else {
			cached = captureFlags{Enabled: portal.CaptureEnabled(values), Override: portal.CaptureOverride(values)}
		}
		fetchedAt = now()
		return cached
	}
}

func memoryDeps(cfg config.Config) (gateway.ServerDeps, func() error, error) {
	tokens := auth.NewTokenStore()
	devToken, err := gatewayDevToken(cfg.Addr, os.Getenv("OP_AI_GATEWAY_DEV_TOKEN"))
	if err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	devAgentSecret, err := gatewayDevAgentSecret(cfg.Addr, os.Getenv("OP_AI_GATEWAY_DEV_AGENT_TOKEN"))
	if err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	now := time.Now().UTC()
	directory := portal.NewMemoryDirectory(tokens)
	language := cfg.DefaultLanguage
	if language == "" {
		language = "de"
	}
	devPassword := strings.TrimSpace(os.Getenv("OP_AI_GATEWAY_DEV_PASSWORD"))
	if devPassword == "" {
		devPassword = "dev-secret"
	}
	devPasswordHash, err := auth.HashPassword(devPassword)
	if err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	directory.AddUser(store.User{
		ID:                "usr_dev",
		Email:             "dev@example.test",
		DisplayName:       "Dev User",
		Role:              "system_admin",
		Status:            store.UserStatusActive,
		PreferredLanguage: language,
		PasswordHash:      devPasswordHash,
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{
		ID:        "tok_dev",
		UserID:    "usr_dev",
		Name:      "Dev Token",
		Status:    store.TokenStatusActive,
		Scopes:    `["gateway:use","admin"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}, devToken); err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	// recorder is this driver's usage.Store: the memory driver has no
	// usage_events table, so UsageAggregateSince is an honest no-op (quota/
	// budget enforcement is a persistent-store feature by design).
	recorder := usage.NewRecorder()
	recorder.ResolveUserName = func(userID string) string {
		user, err := directory.UserByID(context.Background(), userID)
		if err != nil {
			return userID
		}
		if user.DisplayName != "" {
			return user.DisplayName
		}
		if user.Email != "" {
			return user.Email
		}
		return userID
	}
	routeStore := routing.NewMemoryStore()
	systemSettings := portal.NewMemorySystemSettings()
	uiPreferences := portal.NewMemoryUIPreferences()
	if err := seedDefaultServer(context.Background(), routeStore, now, devAgentSecret, cfg.SeedAppHealthMode); err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	acct := account.NewService(account.Deps{Users: directory, Sessions: directory, SetPasswordTokens: directory, SettingsVolatile: true}, account.Config{
		IdleTTL:            cfg.SessionIdleTTL,
		MaxTTL:             cfg.SessionMaxTTL,
		DefaultLanguage:    language,
		SystemAdminModeTTL: time.Duration(cfg.SystemAdminModeTTLSeconds) * time.Second,
	})
	// Memory (dev) driver has no cipher: captures/chats are volatile-RAM-only
	// (KeyVersion 0, plain gzip, gone on process exit) -- there is no
	// persistent-vs-fallback branch to resolve, unlike sqlDeps.
	captures := store.NewMemoryCaptureStore(cfg.CaptureMemoryMaxBytes)
	chats := store.NewMemoryChatStore(cfg.CaptureMemoryMaxBytes)

	return buildRuntime(cfg, depsBackend{
		Users:                 directory,
		Tokens:                directory,
		Groups:                directory,
		Projects:              directory,
		ServerTokens:          tokens,
		SetTokenLastUsedModel: directory.SetTokenLastUsedModel,
		Usage:                 recorder,
		Routes:                routeStore,
		SystemSettings:        systemSettings,
		UIPrefs:               uiPreferences,
		Captures:              captures,
		Chats:                 chats,
		SettingsVolatile:      true,
		Account:               account.NewAPIWithTracing(acct),
		Cleanup:               func() error { return nil },
	})
}

// captureBackend is any store usable as both the write side (gateway) and the
// read side (portal) of capture. Both *store.SQLStore and
// *store.MemoryCaptureStore satisfy it.
type captureBackend interface {
	gateway.CaptureStore
	portal.CaptureReader
}

// depsBackend bundles the per-driver seams buildRuntime needs so memoryDeps
// and sqlDeps can each assemble their own driver-specific pieces (directory
// vs. *store.SQLStore, capture/chat persistent-vs-volatile fallback, cipher,
// the account service, cleanup) and then share ONE body for the registries,
// portal.Service, the ServerDeps literal, and the common background loops.
type depsBackend struct {
	// Users/Tokens/Groups/Projects/Usage/Routes/SystemSettings/UIPrefs are the
	// directory-shaped stores portal.Service (and, via Routes, the background
	// loops) read/write through. Users/Tokens/Groups/Projects all point at the
	// SAME underlying object per driver (*portal.MemoryDirectory for memory,
	// *store.SQLStore for sqlite/postgres) -- they are separate fields only
	// because each portal.ServiceDeps role needs its own static interface
	// type.
	Users          portal.UserReader
	Tokens         portal.TokenRepository
	Groups         portal.GroupStore
	Projects       portal.ProjectStore
	Usage          usage.Store
	Routes         routing.Store
	SystemSettings portal.SystemSettingsStore
	UIPrefs        portal.UIPreferencesStore
	// ServerTokens is gateway.ServerDeps.Tokens's narrower auth.BearerStore
	// role. The memory driver satisfies it with the raw auth.TokenStore
	// (*portal.MemoryDirectory itself does not implement LookupBearer); the
	// sqlite/postgres driver reuses the same *store.SQLStore as Tokens above.
	ServerTokens auth.BearerStore
	// SetTokenLastUsedModel is gateway.ServerDeps.LastUsedModelWriter's driver
	// seam: the memory driver's *portal.MemoryDirectory keeps its own
	// TokenRecord map AND the ServerTokens mirror in sync (its
	// SetTokenLastUsedModel), while the sqlite/postgres driver's
	// *store.SQLStore writes the row directly. Both satisfy the same
	// signature, so this field just carries the bound method through.
	SetTokenLastUsedModel func(ctx context.Context, tokenID, model string) error

	// Captures/Chats are the ONE shared instance for this process handed to
	// both the portal read side and the gateway write side; each driver has
	// already resolved its persistent-vs-volatile-RAM fallback before calling
	// buildRuntime.
	Captures captureBackend
	Chats    portal.ChatStore

	// Cipher/CertCipher are nil for the memory driver (no encryption key
	// concept there).
	Cipher     *capture.Cipher
	CertCipher *capture.Cipher

	// CaptureMaxBytes mirrors cfg.CaptureMaxBytes for the sqlite/postgres
	// driver; the memory driver leaves it 0 -- a pre-existing asymmetry
	// (memory's capture store is always the byte-bounded MemoryCaptureStore,
	// never gated by this field) preserved as-is.
	CaptureMaxBytes int

	// SettingsVolatile is true only for the memory driver (see
	// portal.ServiceDeps.SettingsVolatile's doc).
	SettingsVolatile bool

	// Account is the already-built account API. Building it is a driver seam,
	// not shared code: account.NewService's Deps (Cipher vs. SettingsVolatile)
	// and Config.DefaultLanguage default (memory's "de" fallback) differ per
	// driver, and sqlDeps additionally runs logBootstrapInviteIfNeeded against
	// it before handing it here.
	Account account.API

	// Cleanup is the driver's own base cleanup: a no-op for memory, or
	// store.Close plus any already-started SQL-only prune-loop cancels for
	// sqlite/postgres (see sqlDeps). buildRuntime wraps it with the health/
	// netbird/cert loop cancels shared by all three drivers.
	Cleanup func() error
}

// buildRuntime assembles the registries, portal.Service, background loops,
// and gateway.ServerDeps shared identically by all three drivers (memoryDeps,
// and sqlDeps which backs both sqliteDeps and postgresDeps). Every "ONE
// shared instance" registry/channel below is constructed exactly once here
// and handed to every consumer that needs it -- the invariant each driver
// used to maintain independently.
func buildRuntime(cfg config.Config, b depsBackend) (gateway.ServerDeps, func() error, error) {
	// internalAuthSecret is a fresh per-process secret gating the internal
	// trusted-loopback auth branch that background chat runs use to call back
	// into this same server (see gateway.ServerDeps.InternalAuthSecret).
	internalAuthSecret, err := randomHex(32)
	if err != nil {
		_ = b.Cleanup()
		return gateway.ServerDeps{}, nil, fmt.Errorf("generate internal auth secret: %w", err)
	}
	// appCAPool + appHTTPClient: the CA-trusting outbound gateway->application
	// transport (verifies an internal-CA-signed application leaf served by the
	// agent's TLS proxy, WITHOUT InsecureSkipVerify). Starts system-roots-only;
	// primed with the real internal CA bundle just below, once portalService
	// exists (CertificateCABundlePEM needs it, but portalService needs mux --
	// see providerClients -- as its ModelLister, so this must come first).
	appCAPool, appHTTPClient := newOutboundAppCAClient()
	mux := providerClients(cfg.MockDelay, cfg.MockUnreachable, appHTTPClient)
	usageBroker := usage.NewBroker()
	// appHealth is ONE shared registry for this process; the probe loop
	// populates it and routing/model-offering consume it.
	appHealth := gateway.NewAppHealthRegistry(usageBroker)
	// loadedModels is ONE shared registry: the app-health loop writes the
	// gateway-poll source, the agent-telemetry handler (ServerDeps) writes the
	// agent-report source, and the portal Service reads it. All three MUST
	// hold the same instance.
	loadedModels := gateway.NewLoadedModelRegistry()
	// agentPresence is ONE shared registry: the agent-telemetry handler
	// (ServerDeps) stamps per-server report recency and the app-health loop
	// reads it to derive the availability sample's agent_reporting flag. Both
	// MUST hold the same instance.
	agentPresence := gateway.NewAgentPresenceRegistry(time.Duration(cfg.AgentPresenceTimeoutSeconds) * time.Second)
	// agentCertReports is ONE shared registry too: the agent-telemetry ingest
	// path writes what each agent reports as INSTALLED, the portal reads it
	// (certificate "installed" column + the CA-rotation propagation brake),
	// and the app-health loop prunes it to live servers.
	agentCertReports := gateway.NewAgentCertReportRegistry()
	// agentTransport is ONE shared registry too: authenticateAgent stamps the
	// mesh hop (TLS vs. plain) into it on every successful agent auth against
	// the mesh listener, the portal reads it (per-server transport column +
	// the mesh gate's arming precondition), and the app-health loop prunes it
	// to live servers.
	agentTransport := gateway.NewAgentTransportRegistry()
	// agentProxyStatus is ONE shared registry too: the agent-telemetry ingest
	// path writes what each agent reports as ACTUALLY running on its local
	// TLS-proxy routes (agent_proxy_status.go), the switch reconcile reads it,
	// and the app-health loop prunes it to live servers.
	agentProxyStatus := gateway.NewAgentProxyStatusRegistry()
	// agentStreams is ONE shared registry too: handleAgentStream (ServerDeps)
	// registers/deregisters each open agent WebSocket connection, and the
	// OnCertificateIssued hook below pushes a cert_update doorbell to it
	// through the SAME instance -- otherwise the push side would have nothing
	// to push to.
	agentStreams := gateway.NewAgentStreamRegistry()
	// groups is ONE shared model-group registry: the portal Service refreshes
	// it after each group / model-setting write (as its GroupCache) and the
	// resolver reads it on the hot path (via ServerDeps.Groups). Both MUST
	// hold the same instance. Backed by the raw b.Routes (like the background
	// loops).
	groups := gateway.NewGroupRegistry(b.Routes)
	// tracedRoutes instruments the request-path routing store with per-method
	// spans (a stateless decorator); the raw b.Routes still feeds the
	// background loops + seeding so they stay out of request traces.
	tracedRoutes := tracing.NewRoutingStoreWithTracing(b.Routes)
	// principalLimiter is ONE shared instance for this process: it is set on
	// gateway.ServerDeps.Limiter below and consulted by every inference
	// handler's admission gate (Admit) AND recordUsage's post-response hook
	// (Record) -- Record's optimistic in-memory bump only has an effect when
	// the SAME instance served the preceding Admit call for that principal
	// (see principal_limits.go). Backed by tracedRoutes.
	principalLimiter := gateway.NewPrincipalLimiter(tracedRoutes, gateway.PrincipalLimiterOptions{})
	// netbirdDomainSyncTrigger fires an immediate extra Loop-A pass whenever
	// the NetBird account dns_domain changes (SetNetbirdNetwork's
	// OnNetbirdDomainChanged hook below) -- every peer's dns_label changes
	// with the account domain, so stored server domains would otherwise go
	// stale until the next peer-sync tick. Buffered(1) + non-blocking send: a
	// pending trigger already covers a burst of changes, so coalescing is
	// correct and the hook can never block.
	netbirdDomainSyncTrigger := make(chan struct{}, 1)
	// certReconcileTrigger fires an immediate extra certificate reconcile pass
	// whenever a cert_*/acme_* setting changes (OnCertSettingsChanged below).
	// Without it, a CORRECTIVE settings change (e.g. switching
	// cert_issuer_mode to self_signed) leaves the stale cert_last_error note
	// the panel renders standing for up to one reconcile interval (default
	// 900s) -- only a pass writes/clears that note. Same buffered(1) +
	// non-blocking-send shape as the NetBird trigger above; the loop that
	// consumes it is the only caller of runCertPass, so the trigger can never
	// overlap a pass or pile up on portal.Service's certMu.
	certReconcileTrigger := make(chan struct{}, 1)
	// acmeChallenges is ONE shared HTTP-01 challenge store for this process:
	// the portal Service PUTs a token's key authorization while an ACME order
	// is in flight, and the gateway's public
	// /.well-known/acme-challenge/{token} handler READS it. Both sides MUST
	// hold the same instance -- mirrors the shared LoadedModelRegistry wiring
	// above.
	acmeChallenges := certissue.NewMemoryChallengeStore()
	// Declared (not :=) so the OnCABundleChanged closure below can call back
	// into the very Service it is a dependency of, once that Service exists --
	// portalService is nil while ServiceDeps is being built, but the closure
	// is only ever invoked later, from inside a certificate reconcile pass, by
	// which point the assignment below has completed.
	var portalService *portal.Service
	portalService = portal.NewService(portal.ServiceDeps{
		Users: b.Users, Tokens: b.Tokens, Usage: b.Usage, Routes: tracedRoutes, ModelLister: mux,
		SystemSettings: b.SystemSettings, UIPrefs: b.UIPrefs, Captures: b.Captures, Chats: b.Chats,
		Cipher: b.Cipher, CertCipher: b.CertCipher,
		CertEdgeOutputDir: cfg.CertEdgeOutputDir, CertEdgeProbeTarget: cfg.CertEdgeProbeTarget,
		Reachability: appHealth, LoadedModels: loadedModels, AgentPresence: agentPresence,
		AgentCertReports: agentCertReports, AgentTransport: agentTransport,
		ProxyStatus: proxyStatusReader{reg: agentProxyStatus},
		Groups:      b.Groups, GroupCache: groups, Projects: b.Projects,
		NetbirdKeyFile: cfg.NetbirdKeyFile, Themes: loadThemeRegistry(cfg),
		AgentPort: effectiveAgentPort(cfg), AgentBindHost: explicitAgentBindHost(cfg),
		AgentTLSPort: atoiOr(effectiveAgentTLSPort(cfg), 8443), AgentTLSSeparateDefault: cfg.AgentTLSSeparate,
		NetbirdTokenRotateBeforeDaysDefault: &cfg.NetbirdTokenRotateBeforeDays,
		AgentPresenceTimeoutDefault:         cfg.AgentPresenceTimeoutSeconds,
		SettingsVolatile:                    b.SettingsVolatile, ACMEChallenges: acmeChallenges,
		OnCertSettingsChanged: certReconcileTriggerFunc(certReconcileTrigger),
		OnCertificateIssued:   agentStreams.NotifyCertUpdate,
		OnCABundleChanged: func(fingerprint string) {
			agentStreams.NotifyCAUpdate(fingerprint)
			// Rotation refresh: rebuild the outbound-app CA trust pool NOW,
			// rather than waiting for the next cert-reconcile tick -- see
			// refreshOutboundAppCAPool's doc for why no periodic backstop is
			// needed.
			refreshOutboundAppCAPool(context.Background(), portalService, appCAPool)
		},
		OnNetbirdDomainChanged: func() {
			select {
			case netbirdDomainSyncTrigger <- struct{}{}:
			default: // coalesce: a pending trigger already covers this change
			}
		},
	})
	// Prime the outbound-app CA pool with the real internal CA bundle now
	// that portalService exists (it started system-roots-only -- see
	// newOutboundAppCAClient). Best-effort/fail-safe, same as the rotation
	// path.
	refreshOutboundAppCAPool(context.Background(), portalService, appCAPool)
	// Prime the group registry once at startup so groups route immediately
	// (the app-health loop refreshes it periodically thereafter).
	// Best-effort.
	if err := groups.RefreshGroups(context.Background()); err != nil {
		log.Printf("model groups: initial refresh failed: %v", err)
	}
	// appHealthServerTrigger lets the agent-reactivation coordinator (below)
	// poke an immediate scoped health pass for one server, out of band from
	// the fleet cadence.
	appHealthServerTrigger := make(chan string, 64)
	cancelHealth := startAppHealthLoop(&appHealthRunner{
		store:        b.Routes,
		prober:       mux,
		syncer:       portalService,
		registry:     appHealth,
		loaded:       loadedModels,
		agents:       agentRegistries{presence: agentPresence, certReports: agentCertReports, transport: agentTransport, proxyStatus: agentProxyStatus},
		groups:       groups,
		settings:     b.SystemSettings,
		probeTimeout: cfg.AppHealthProbeTimeout,
		cipher:       b.Cipher,
	}, appHealthServerTrigger)
	// NetBird sync -- two loops, both cadences read live from
	// NetbirdPolicySettings: Loop A (peer sync) keeps each NetBird server's
	// peer name / domain / connected status reconciled and fires an online
	// event (false->true) that reconciles that server's group/policy state
	// promptly; Loop B (group+policy) runs the fleet group-mirror +
	// access-policy reconcile. Both are no-ops until the NetBird module is
	// enabled. The online callback is async so an online burst never blocks
	// Loop A (ReconcileServerNetbird is concurrency-safe).
	cancelNetbirdA := startNetbirdSyncLoop(b.Routes, portalService, func(id string) {
		go portalService.ReconcileServerNetbird(context.Background(), id)
	}, portalService, cfg.NetbirdSyncIntervalSeconds, netbirdCallTimeout, netbirdDomainSyncTrigger)
	cancelNetbirdB := startNetbirdReconcileLoop(portalService, portalService, cfg.NetbirdSyncIntervalSeconds)
	// onAgentReactivated reacts to a ServerAgent inactive->active edge (see
	// gateway.ServerDeps.OnAgentReactivated) by immediately syncing+
	// reconciling that server's NetBird peer (if any) and, once online (or
	// peer-less), poking appHealthServerTrigger for an out-of-band scoped
	// health pass -- instead of waiting for the periodic NetBird/health
	// loops. Run in a goroutine: the gateway fires this from the
	// telemetry-ingest hot path, so it must never block.
	onAgentReactivated := func(serverID string) {
		go handleAgentReactivation(context.Background(), serverID, reactivationDeps{
			store: b.Routes, settings: portalService, reconciler: portalService,
			syncOne: syncServerNetbirdOnce, timeout: netbirdCallTimeout,
		}, appHealthServerTrigger)
	}
	// Certificate reconcile loop: drives portal.Service.ReconcileCertificates
	// on its own cadence (a no-op while the module is disabled, so this costs
	// nothing until an operator turns it on).
	cancelCerts := startCertReconcileLoop(portalService, time.Duration(cfg.CertReconcileIntervalSeconds)*time.Second, certReconcileTrigger)
	baseCleanup := b.Cleanup
	cleanup := func() error {
		cancelHealth()
		cancelNetbirdA()
		cancelNetbirdB()
		cancelCerts()
		return baseCleanup()
	}
	captureFlagsHook := newCaptureFlagsHook(b.SystemSettings, time.Now)
	return gateway.ServerDeps{
		Tokens:                          b.ServerTokens,
		LastUsedModelWriter:             b.SetTokenLastUsedModel,
		Usage:                           b.Usage,
		UsageEvents:                     usageBroker,
		Provider:                        mux,
		Routes:                          tracedRoutes,
		Portal:                          portal.NewAPIWithTracing(portalService),
		Account:                         b.Account,
		CookieSecure:                    resolveCookieSecure(cfg),
		SessionMaxAge:                   cfg.SessionMaxTTL,
		PublicURL:                       cfg.PublicURL,
		StreamIdleTimeout:               cfg.StreamIdleTimeout,
		SwapProtectWindow:               time.Duration(cfg.SwapProtectWindowSeconds) * time.Second,
		SessionReservationWindow:        time.Duration(cfg.SessionReservationWindowSeconds) * time.Second,
		CapacityVRAMSafetyMarginPercent: cfg.CapacityVRAMSafetyMarginPercent,
		CapacityMaxConcurrency:          cfg.CapacityMaxConcurrency,
		CapacitySettleSeconds:           cfg.CapacitySettleSeconds,
		AdmissionQueueMaxDepth:          cfg.AdmissionQueueMaxDepth,
		NetbirdSyncInterval:             time.Duration(cfg.NetbirdSyncIntervalSeconds) * time.Second,
		ACMEChallenges:                  acmeChallenges,
		CertEdgeRequireHTTPSDisable:     cfg.CertEdgeRequireHTTPSDisable,
		CertMeshRequireTLSDisable:       cfg.CertMeshRequireTLSDisable,
		Cipher:                          b.Cipher,
		Captures:                        b.Captures,
		CaptureMaxBytes:                 b.CaptureMaxBytes,
		CaptureEnabled:                  func() bool { return captureFlagsHook().Enabled },
		CaptureOverride:                 func() bool { return captureFlagsHook().Override },
		AppHealth:                       appHealth,
		SelfBaseURL:                     loopbackSelfBase(),
		InternalAuthSecret:              internalAuthSecret,
		ChatRuns:                        gateway.NewChatRunRegistry(5),
		ServerPerf:                      gateway.NewServerPerfRegistry(),
		LoadedModels:                    loadedModels,
		AgentPresence:                   agentPresence,
		AgentCertReports:                agentCertReports,
		AgentTransport:                  agentTransport,
		AgentProxyStatus:                agentProxyStatus,
		AgentStreams:                    agentStreams,
		Benchmarks:                      gateway.NewBenchmarkRegistry(),
		Groups:                          groups,
		Users:                           b.Users,
		OnAgentReactivated:              onAgentReactivated,
		Limiter:                         principalLimiter,
	}, cleanup, nil
}

// sqlDeps holds the body shared identically by the sqlite and postgres
// drivers -- they differ from each other only in how sqlStore was opened
// (store.OpenSQLite vs. store.OpenPostgres, see sqliteDeps/postgresDeps
// below), a difference already resolved by the caller into sqlStore +
// driverCleanup. Everything from here on (migrate, bootstrap, seed, capture/
// chat fallback, the SQL-only prune loops, and the block shared with
// memoryDeps via buildRuntime) is identical for both.
func sqlDeps(cfg config.Config, sqlStore *store.SQLStore, driverCleanup func() error) (gateway.ServerDeps, func() error, error) {
	cleanup := driverCleanup
	if cfg.AutoMigrate {
		if err := sqlStore.Migrate(context.Background()); err != nil {
			_ = cleanup()
			return gateway.ServerDeps{}, nil, err
		}
	}
	if err := bootstrapAdmin(context.Background(), sqlStore, cfg); err != nil {
		_ = cleanup()
		return gateway.ServerDeps{}, nil, err
	}
	if err := seedDefaultServerIfEmpty(context.Background(), sqlStore, time.Now().UTC(), cfg.Addr, os.Getenv("OP_AI_GATEWAY_DEV_AGENT_TOKEN"), cfg.SeedAppHealthMode); err != nil {
		_ = cleanup()
		return gateway.ServerDeps{}, nil, err
	}
	captureCipher, err := buildCaptureCipher(cfg)
	if err != nil {
		_ = cleanup()
		return gateway.ServerDeps{}, nil, err
	}
	// The certificate module owns its OWN key; a missing one leaves
	// certCipher nil (module unusable on a disk store, gateway still
	// starts), a malformed one is fatal.
	certCipher, err := buildCertCipher(cfg)
	if err != nil {
		_ = cleanup()
		return gateway.ServerDeps{}, nil, err
	}
	acct := account.NewService(account.Deps{Users: sqlStore, Sessions: sqlStore, SetPasswordTokens: sqlStore, Cipher: captureCipher}, account.Config{
		IdleTTL:            cfg.SessionIdleTTL,
		MaxTTL:             cfg.SessionMaxTTL,
		DefaultLanguage:    cfg.DefaultLanguage,
		SystemAdminModeTTL: time.Duration(cfg.SystemAdminModeTTLSeconds) * time.Second,
	})
	if err := logBootstrapInviteIfNeeded(context.Background(), acct, sqlStore, cfg); err != nil {
		_ = cleanup()
		return gateway.ServerDeps{}, nil, err
	}
	// captures is ONE shared instance for this whole process, handed to BOTH
	// the read side (Portal ServiceDeps) and the write side (ServerDeps) --
	// SP-C+ P4. Key present -> persistent encrypted capture on sqlStore (as
	// before, plus the retention prune loop below). Key absent -> capture is
	// NOT off anymore: it falls back to a volatile in-RAM store
	// (MemoryCaptureStore, KeyVersion 0, gone on process exit) instead of
	// failing closed.
	var captures captureBackend = sqlStore
	if captureCipher == nil {
		captures = store.NewMemoryCaptureStore(cfg.CaptureMemoryMaxBytes)
		// Informational, not a warning: capture still runs for any token with
		// log_communication enabled, just volatile-in-RAM instead of
		// persistent.
		if n := countActiveLoggingTokens(context.Background(), sqlStore); n > 0 {
			log.Printf("capture: OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY not set but %d token(s) have log_communication enabled — capture runs in volatile RAM mode (not off)", n)
		}
	}
	// Chats mirror captures: persistent on sqlStore when an encryption key is
	// present, else a volatile in-RAM store (KeyVersion 0, gone on process
	// exit). They reuse the same captureCipher for seal/open, so the switch
	// is the same cipher-present test.
	var chats portal.ChatStore = sqlStore
	if captureCipher == nil {
		chats = store.NewMemoryChatStore(cfg.CaptureMemoryMaxBytes)
	}
	// Start the capture retention prune loop only when capture is persistent
	// (key set + sql) -- the RAM store has no disk rows to prune (byte-FIFO
	// eviction is its own retention). startCapturePruneLoop returns the
	// goroutine's cancel; fold it into cleanup so tests/embedded callers can
	// stop it. Production has no graceful shutdown and relies on process
	// exit. This SQL-only loop has no memoryDeps counterpart, so it stays
	// here rather than in buildRuntime.
	if captureCipher != nil {
		cancelPrune := startCapturePruneLoop(sqlStore, capturePruneInterval)
		baseCleanup := cleanup
		cleanup = func() error {
			cancelPrune()
			return baseCleanup()
		}
	}
	// Telemetry retention prune loop: runs unconditionally on a SQL store
	// (rich telemetry samples are always persisted, unlike the cipher-gated
	// capture rows). Retention is the fixed
	// OP_AI_GATEWAY_TELEMETRY_RETENTION_HOURS config; pruneTelemetryOnce
	// disables itself when that is <= 0. Fold its cancel into cleanup like
	// the capture loop. Also SQL-only; no memoryDeps counterpart.
	cancelTelemetryPrune := startTelemetryPruneLoop(sqlStore, time.Duration(cfg.TelemetryRetentionHours)*time.Hour, time.Duration(cfg.AvailabilityRetentionHours)*time.Hour, telemetryPruneInterval)
	baseCleanupForTelemetry := cleanup
	cleanup = func() error {
		cancelTelemetryPrune()
		return baseCleanupForTelemetry()
	}

	return buildRuntime(cfg, depsBackend{
		Users:                 sqlStore,
		Tokens:                sqlStore,
		Groups:                sqlStore,
		Projects:              sqlStore,
		ServerTokens:          sqlStore,
		SetTokenLastUsedModel: sqlStore.SetTokenLastUsedModel,
		Usage:                 sqlStore,
		Routes:                sqlStore,
		SystemSettings:        sqlStore,
		UIPrefs:               sqlStore,
		Captures:              captures,
		Chats:                 chats,
		Cipher:                captureCipher,
		CertCipher:            certCipher,
		CaptureMaxBytes:       cfg.CaptureMaxBytes,
		SettingsVolatile:      false,
		Account:               account.NewAPIWithTracing(acct),
		Cleanup:               cleanup,
	})
}

func sqliteDeps(cfg config.Config) (gateway.ServerDeps, func() error, error) {
	sqliteStore, err := store.OpenSQLite(cfg.SQLitePath)
	if err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	return sqlDeps(cfg, sqliteStore, sqliteStore.Close)
}

func postgresDeps(cfg config.Config) (gateway.ServerDeps, func() error, error) {
	if strings.TrimSpace(cfg.PostgresDSN) == "" {
		return gateway.ServerDeps{}, nil, fmt.Errorf("OP_AI_GATEWAY_POSTGRES_DSN is required for the postgres driver")
	}
	pgStore, err := store.OpenPostgres(context.Background(), cfg.PostgresDSN)
	if err != nil {
		return gateway.ServerDeps{}, nil, err
	}
	return sqlDeps(cfg, pgStore, pgStore.Close)
}

// providerClients builds the gateway->application Multiplexer. appHTTPClient is
// the CA-trusting client outbound calls verify through (see
// newOutboundAppCAClient); nil falls back to a system-roots-only client rather
// than provider.NewOpenAICompatibleClient/NewOllamaClient's own nil ->
// http.DefaultClient fallback, so every production/test caller gets a real
// *http.Transport with an explicit (never-skip-verify) TLS config.
func providerClients(mockDelay time.Duration, mockUnreachable bool, appHTTPClient *http.Client) *provider.Multiplexer {
	if appHTTPClient == nil {
		appHTTPClient = &http.Client{Transport: newOutboundAppTransport("")}
	}
	openAICompatible := provider.NewOpenAICompatibleClient(appHTTPClient)
	return provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderMock:      provider.NewMockWithDelay(mockDelay).WithUnreachable(mockUnreachable),
		routing.ProviderOllama:    provider.NewOllamaClient(appHTTPClient),
		routing.ProviderVLLM:      openAICompatible,
		routing.ProviderLlamaCPP:  openAICompatible,
		routing.ProviderLlamaSwap: openAICompatible,
		routing.ProviderLiteLLM:   openAICompatible,
	}, nil)
}

func seedDefaultServer(ctx context.Context, routes routing.Store, now time.Time, agentSecret string, healthMode string) error {
	server := routing.AIServer{
		ID: "mock-server", Name: "Mock Server", Domain: "mock.local",
		Provider: routing.ProviderMock, Endpoint: "mock://local",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := routes.CreateAIServer(ctx, server); err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("seed server: %w", err)
	}
	app := routing.Application{
		ID: "app-mock", ServerID: server.ID, Type: routing.ProviderMock, Port: 8000, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
		Priority:   10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800,
		Status: routing.ServerStatusActive, HealthCheckMode: healthMode, CreatedAt: now, UpdatedAt: now,
	}
	if err := routes.CreateApplication(ctx, app); err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("seed application: %w", err)
	}
	mappings := []routing.ModelMapping{
		{ID: "map-qwen-coder", ApplicationID: app.ID, GatewayModelName: "qwen-coder", AppModelName: "qwen-coder", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "map-gpt-oss-20b", ApplicationID: app.ID, GatewayModelName: "gpt-oss-20b", AppModelName: "gpt-oss-20b", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
	}
	for _, mapping := range mappings {
		if err := routes.CreateMapping(ctx, mapping); err != nil && !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("seed mapping %s: %w", mapping.ID, err)
		}
	}
	if err := routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: server.ID, ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		return fmt.Errorf("seed telemetry: %w", err)
	}
	if agentSecret != "" {
		if err := routes.UpsertAgentToken(ctx, routing.AgentToken{
			ID: "agt-mock", ServerID: server.ID, SecretPrefix: agentTokenPrefix(agentSecret),
			CreatedAt: now, UpdatedAt: now,
		}, auth.HashSecret(agentSecret)); err != nil {
			return fmt.Errorf("seed agent token: %w", err)
		}
	}
	return nil
}

func agentTokenPrefix(secret string) string {
	if len(secret) <= 8 {
		return secret
	}
	return secret[:8]
}

func seedDefaultServerIfEmpty(ctx context.Context, routes routing.Store, now time.Time, addr string, configuredAgentSecret string, healthMode string) error {
	servers, err := routes.AIServers(ctx)
	if err != nil {
		return fmt.Errorf("list existing servers: %w", err)
	}
	if len(servers) > 0 {
		return nil
	}
	agentSecret, err := gatewayDevAgentSecret(addr, configuredAgentSecret)
	if err != nil {
		return err
	}
	return seedDefaultServer(ctx, routes, now, agentSecret, healthMode)
}

// bootstrapStore is the store surface bootstrapAdmin needs to seed (or adopt)
// the bootstrap admin user and API token. *store.SQLStore satisfies it; the
// same call runs on both the sqlite and postgres driver paths (see sqlDeps).
type bootstrapStore interface {
	CreateUser(ctx context.Context, user store.User) error
	UserByEmail(ctx context.Context, email string) (store.User, error)
	UpdateUser(ctx context.Context, user store.User) error
	CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error
	TokenByID(ctx context.Context, id string) (store.TokenRecord, error)
}

func bootstrapAdmin(ctx context.Context, s bootstrapStore, cfg config.Config) error {
	email := strings.TrimSpace(cfg.BootstrapAdminEmail)
	secret := strings.TrimSpace(cfg.BootstrapAPIToken)
	if email == "" && secret == "" {
		return nil
	}
	if email == "" || secret == "" {
		return fmt.Errorf("both OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL and OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN are required for sqlite bootstrap")
	}
	name := strings.TrimSpace(cfg.BootstrapAdminName)
	if name == "" {
		name = email
	}
	now := time.Now().UTC()
	// An optional bootstrap password seeds a LOGIN-ABLE admin so automated/container
	// deploys don't need the invite-link-in-logs flow. Requiring email+token is
	// unchanged; the password is additive. Empty password -> PasswordHash stays "" and
	// the invite path (logBootstrapInviteIfNeeded) is preserved.
	password := cfg.BootstrapAdminPassword
	var passwordHash string
	if password != "" {
		hashed, err := auth.HashPassword(password)
		if err != nil {
			return fmt.Errorf("hash bootstrap admin password: %w", err)
		}
		passwordHash = hashed
	}
	user := store.User{
		ID:                "usr_bootstrap_admin",
		Email:             email,
		DisplayName:       name,
		Role:              "system_admin",
		Status:            store.UserStatusActive,
		PreferredLanguage: cfg.DefaultLanguage,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if user.PreferredLanguage == "" {
		user.PreferredLanguage = "de"
	}
	if passwordHash != "" {
		user.PasswordHash = passwordHash
		user.PasswordSetAt = &now
	}
	if err := s.CreateUser(ctx, user); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		existing, lookupErr := s.UserByEmail(ctx, email)
		if lookupErr != nil {
			return fmt.Errorf("validate bootstrap admin user: %w", lookupErr)
		}
		if existing.ID != user.ID {
			return fmt.Errorf("bootstrap admin email %q already belongs to user %q", email, existing.ID)
		}
		// The admin already exists. If a bootstrap password is now provided and the
		// existing admin has no password yet, adopt it (so a redeploy can seed a
		// login-able admin). Never overwrite an already-set password here.
		if passwordHash != "" && existing.PasswordHash == "" {
			existing.PasswordHash = passwordHash
			existing.PasswordSetAt = &now
			existing.UpdatedAt = now
			if err := s.UpdateUser(ctx, existing); err != nil {
				return fmt.Errorf("set bootstrap admin password: %w", err)
			}
		}
	}
	token := store.TokenRecord{
		ID:        "tok_bootstrap_admin",
		UserID:    user.ID,
		Name:      "Bootstrap Admin Token",
		Status:    store.TokenStatusActive,
		Scopes:    `["gateway:use","admin"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreatePlainToken(ctx, token, secret); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		existing, lookupErr := s.TokenByID(ctx, token.ID)
		if lookupErr != nil {
			return fmt.Errorf("validate bootstrap api token: %w", lookupErr)
		}
		if existing.UserID != user.ID {
			return fmt.Errorf("bootstrap api token belongs to user %q, want %q", existing.UserID, user.ID)
		}
		if existing.SecretHash != auth.HashSecret(secret) {
			return fmt.Errorf("bootstrap api token already exists with a different secret")
		}
		if existing.Status != store.TokenStatusActive {
			return fmt.Errorf("bootstrap api token status = %q, want active", existing.Status)
		}
	}
	return nil
}

func gatewayDevToken(addr string, configuredToken string) (string, error) {
	token := strings.TrimSpace(configuredToken)
	if token != "" {
		return token, nil
	}
	if isLoopbackAddr(addr) {
		return "dev-secret", nil
	}
	return "", fmt.Errorf("OP_AI_GATEWAY_DEV_TOKEN is required when binding to non-loopback address %q", addr)
}

func gatewayDevAgentSecret(addr string, configuredSecret string) (string, error) {
	secret := strings.TrimSpace(configuredSecret)
	if secret != "" {
		return secret, nil
	}
	if isLoopbackAddr(addr) {
		return "dev-agent-secret", nil
	}
	return "", fmt.Errorf("OP_AI_GATEWAY_DEV_AGENT_TOKEN is required when binding to non-loopback address %q", addr)
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolveCookieSecure(cfg config.Config) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.SessionCookieSecure)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return !isLoopbackAddr(cfg.Addr)
	}
}

// inviteLookup is the store surface logBootstrapInviteIfNeeded needs to check
// whether the bootstrap admin already has a password set. *store.SQLStore
// satisfies it.
type inviteLookup interface {
	UserByEmail(ctx context.Context, email string) (store.User, error)
}

func logBootstrapInviteIfNeeded(ctx context.Context, acct *account.Service, s inviteLookup, cfg config.Config) error {
	email := strings.TrimSpace(cfg.BootstrapAdminEmail)
	if email == "" {
		return nil
	}
	user, err := s.UserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("lookup bootstrap admin for invite: %w", err)
	}
	if user.PasswordHash != "" {
		return nil
	}
	_, secret, err := acct.ReissueInvite(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("issue bootstrap admin invite: %w", err)
	}
	base := strings.TrimRight(cfg.PublicURL, "/")
	log.Printf("bootstrap admin has no password yet; set one at %s/set-password?token=%s", base, secret)
	return nil
}

// runHealthcheck probes the running gateway's /healthz for the Docker HEALTHCHECK
// on distroless (which has no shell). It exits 0 on HTTP 200, else 1.
func runHealthcheck() int {
	port := portFromAddr(os.Getenv("OP_AI_GATEWAY_ADDR"))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// portFromAddr extracts the port from a host:port address, defaulting to 8080.
func portFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "8080"
	}
	return port
}

// loopbackSelfBase returns the loopback base URL the gateway uses to call
// itself for background chat runs (see gateway.ServerDeps.SelfBaseURL). It
// always targets 127.0.0.1 regardless of the configured bind host, because
// the executor's HTTP calls originate from the same process/host.
func loopbackSelfBase() string {
	return "http://127.0.0.1:" + portFromAddr(os.Getenv("OP_AI_GATEWAY_ADDR"))
}

// randomHex returns n random bytes hex-encoded, used to mint a per-process
// secret (e.g. the internal trusted-loopback auth secret) at startup.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newHTTPServer(baseCtx context.Context, addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
