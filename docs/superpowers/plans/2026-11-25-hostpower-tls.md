# Hostpower HTTP over TLS with a pinned certificate — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Optional TLS 1.3 on the `server-agent hostpower` HTTP endpoint with a self-generated (or operator-supplied) certificate that the guest agent pins by SHA-256.

**Architecture:** Host wraps its existing listener with `tls.NewListener` when `-tls` is set; certificate material lives in its own state directory. Guest's `NewHostpowerHTTPClient` gains a pin that replaces chain/name verification with a `VerifyConnection` fingerprint check; a pin forces https (fail closed).

**Tech Stack:** Go 1.26 stdlib (`crypto/tls`, `crypto/x509`, `crypto/ecdsa`), React/TS template string, Markdown docs.

**Spec:** `docs/superpowers/specs/2026-11-25-hostpower-tls-design.md`

## Global Constraints

- Server-agent module only imports allowed by `internal/archtest/arch_test.go` (`config` → only `wslentry`; `hostpower` → only `collector`).
- SPDX header `// SPDX-License-Identifier: AGPL-3.0-only` + `// Copyright (C) 2026 OnPrem AI Gateway contributors` on new Go files.
- Repo text in English, documentation IPs only (`192.0.2.0/24`), no local host names.
- TLS `MinVersion: tls.VersionTLS13` on both ends.
- Fingerprint display format: uppercase hex, `:` between bytes (95 chars). Accepted input: 64 hex digits, optional `:`, any case, trimmed.
- Default paths: `/var/lib/op-hostpower-tls/tls.crt`, `/var/lib/op-hostpower-tls/tls.key`; files `0600`, generated parent dir `0700`.
- Agent `const Version` → `"0.25.0"` once, with a history comment in the existing style.
- Lint: `make fmt`, `make lint-go` (gofumpt, gocritic via golangci-lint), frontend `npm run format:check`.

## Review Focus

- Pin set + `http://` URL: no TCP connection may be opened (token must never travel clear) — Task 1 test asserts the listener saw zero connections.
- Pin mismatch: the HTTP handler must never run (no request bytes after a failed handshake) — Task 1 test counts handler hits.
- Exactly one of cert/key present on host start: no file written, start fails exit 2 — Task 2 test.
- Key file with group/other bits from an operator: loads, Warn logged — Task 2 test.
- Plain HTTP client against the `-tls` listener gets no `200` (Go answers `400`) — Task 3 test.

---

### Task 1: Guest fingerprint helpers and pinned client (`collector`)

**Files:**
- Create: `server-agent/internal/collector/hostpower_tls.go`, `server-agent/internal/collector/hostpower_tls_test.go`
- Modify: `server-agent/internal/collector/hostpower_http.go`, `server-agent/internal/collector/hostpower_http_test.go`, `server-agent/internal/collector/collector.go` (PowerConfig + `nativeAndHostpowerSubs`), every caller of `NewHostpowerHTTPClient` / `newHostpowerHTTPCollector`

**Interfaces:**
- Produces:
  - `func FormatHostpowerFingerprint(der []byte) string` — SHA-256 of `der`, `AB:CD:…`.
  - `func ParseHostpowerFingerprint(s string) ([]byte, error)` — 32 bytes or error.
  - `type HostpowerPinError struct{ Want, Got string }` (formatted fingerprints; `Got` empty when no certificate) with `Error() string`.
  - `func NewHostpowerHTTPClient(pin []byte) *http.Client` — nil pin = today's client.
  - `newHostpowerHTTPCollector(baseURL, token, fingerprint string, client *http.Client) *hostpowerHTTPCollector` — `fingerprint` is the raw configured string; parsed once; parse error stored and returned by every `fetch` (fail closed); a nil client gets `NewHostpowerHTTPClient(pin)`.
  - `PowerConfig.HostpowerTLSFingerprint string`.

- [ ] **Step 1: Failing tests** in `hostpower_tls_test.go`: `TestFormatHostpowerFingerprint` (known DER → `strings.ToUpper` colon form of `sha256.Sum256`, len 95); `TestParseHostpowerFingerprint` table: colon upper, plain lower, mixed case with surrounding spaces → same 32 bytes; 63/65 hex digits, `zz…`, empty, `sha256:` prefix → error; round-trip `Parse(Format(der)) == sum`.
- [ ] **Step 2: Failing tests** in `hostpower_http_test.go` using `httptest.NewTLSServer` (counting handler):
  - `TestHostpowerHTTPPinnedMatch`: pin = `FormatHostpowerFingerprint(srv.Certificate().Raw)`, https URL → two collects give non-nil watts on the second.
  - `TestHostpowerHTTPPinnedMismatch`: wrong pin → nil reading, handler hits 0, `errors.As(err, &*HostpowerPinError)` on `fetch`, Warn logged exactly once over 3 collects (capture slog), and again once after a success in between.
  - `TestHostpowerHTTPSWithoutPinUsesSystemRoots`: no pin against the self-signed test server → fails, handler hits 0.
  - `TestHostpowerHTTPPinRequiresHTTPS`: pin + `http://` URL to a `net.Listen` counting accepts → fetch error, 0 accepts, token never sent.
  - `TestHostpowerHTTPInvalidPinFailsClosed`: pin `"nope"` + https → error, 0 handler hits.
  - Existing tests adjusted to the new signatures; behaviour unchanged.
- [ ] **Step 3: Run** `cd server-agent && go test ./internal/collector -run 'Hostpower' -count=1` → FAIL (undefined).
- [ ] **Step 4: Implement.** Client: on `newLocalHTTPClient(nil)` result set `tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: …}` with a comment that the callback replaces chain/name verification (pinning) — add `// NOSONAR` only if Sonar flags it later. Callback: no peer cert → `&HostpowerPinError{Want: …}`; `subtle.ConstantTimeCompare(sha256(leaf.Raw), pin) != 1` → `&HostpowerPinError{Want, Got}`. Collector: pin parse error or (`pin != nil` and URL scheme != `https`) → error before building a request (`"hostpower_tls_fingerprint requires an https:// hostpower_url"` / `"invalid hostpower_tls_fingerprint"`). Logging: add `pinWarned bool`; on a `HostpowerPinError` and `!pinWarned` → Warn `hostpower: server certificate does not match hostpower_tls_fingerprint` with `want`, `got`, `url`, set `pinWarned`; any success clears it; Debug line stays for every failure. Update the doc comment of `NewHostpowerHTTPClient`.
- [ ] **Step 5: Run** the focused tests, then `go test ./internal/collector ./internal/archtest -count=1` → PASS.
- [ ] **Step 6: Commit** `feat(agent): pin the hostpower server certificate by SHA-256 in the guest client`.

### Task 2: Host certificate material and the `tls fingerprint` verb (`hostpower`)

**Files:**
- Create: `server-agent/internal/hostpower/tlscert.go`, `server-agent/internal/hostpower/tlscert_test.go`
- Modify: `server-agent/internal/hostpower/cli.go` (dispatch `tls` like `token`, usage text), `server-agent/internal/hostpower/cli_test.go`

**Interfaces:**
- Consumes: `collector.FormatHostpowerFingerprint`.
- Produces:
  - `const DefaultTLSCertPath = "/var/lib/op-hostpower-tls/tls.crt"`, `DefaultTLSKeyPath = "/var/lib/op-hostpower-tls/tls.key"`.
  - `func LoadOrCreateCert(certPath, keyPath string, now func() time.Time, log *slog.Logger) (cert tls.Certificate, created bool, err error)`.
  - `func TLSMain(args []string, stdout, stderr io.Writer) int` — `tls [-tls-cert PATH] fingerprint`.

- [ ] **Step 1: Failing tests** `tlscert_test.go`: `TestLoadOrCreateCertGenerates` (temp dir `sub/` missing → both files mode `0600`, `sub` mode `0700`, `created=true`, leaf ECDSA P-256, CN `op-hostpower`, NotAfter > now+99y, `IsCA=false`, ExtKeyUsage ServerAuth); `TestLoadOrCreateCertReloads` (second call `created=false`, same `Certificate[0]`); `TestLoadOrCreateCertHalfPair` (only key / only cert → error naming the missing path, directory listing unchanged); `TestLoadOrCreateCertMismatchedPair` (two generated pairs crossed → error); `TestLoadOrCreateCertLooseKeyWarns` (chmod key `0640` → loads, Warn captured); `TestLoadOrCreateCertNotRegular` (cert path is a dir → error); `TestLoadOrCreateCertTightensParent` (existing parent `0755` → `0700` after generation).
- [ ] **Step 2: Failing tests** in `cli_test.go`: `tls fingerprint` on a generated cert prints `FormatHostpowerFingerprint(leaf)` + `\n`, exit 0; missing file → exit 1, stderr mentions path; `tls` with no verb / `tls frob` / extra arg → exit 2 with usage.
- [ ] **Step 3: Run** `go test ./internal/hostpower -run 'Cert|TLS' -count=1` → FAIL.
- [ ] **Step 4: Implement** per spec §"Certificate material": `ecdsa.GenerateKey(elliptic.P256(), rand.Reader)`, `x509.CreateCertificate`, `x509.MarshalPKCS8PrivateKey`, atomic write helper (temp in same dir, chmod 0600, rename; key first) — reuse the `writeTokenFile` dir-tightening logic by extracting `ensurePrivateDir(dir string) error` from it rather than copying. Lstat both paths; non-regular → error. Wrap a generation error that `errors.Is(err, syscall.EROFS)` with the hint `read-only file system: add StateDirectory=op-hostpower-tls and ReadWritePaths=/var/lib/op-hostpower-tls to the unit (see the agent README)` (unit-test the hint with a fake write error injected through an unexported `writeFile` var, not a real EROFS).
- [ ] **Step 5: Run** focused tests → PASS; `go test ./internal/hostpower -count=1` → PASS.
- [ ] **Step 6: Commit** `feat(agent): hostpower generates or loads its TLS certificate and prints its fingerprint`.

### Task 3: Serve HTTPS with `-tls` (`hostpower`)

**Files:**
- Modify: `server-agent/internal/hostpower/cli.go` (flags, `Config` fields `TLS bool; TLSCert, TLSKey string`, `ParseArgs` validation, `serve`), `server-agent/internal/hostpower/server.go` (`NewServer` gets `ErrorLog`), `server-agent/internal/hostpower/cli_test.go`

**Interfaces:**
- Consumes: `LoadOrCreateCert`, `collector.FormatHostpowerFingerprint`, `collector.NewHostpowerHTTPClient` (test only).

- [ ] **Step 1: Failing tests**: `TestParseArgsTLSNeedsListen` (`-tls` alone → error); `TestParseArgsTLSDefaults` (`-listen x -tls` → default paths); `TestServeTLSTokenMode` (`run` with `-listen 127.0.0.1:0 -auth token -tls -tls-cert/-tls-key` in temp dir, temp `-out`/powercap fixtures as existing serve tests do; wait for the cert file, then a pinned `collector.NewHostpowerHTTPClient(pin)` GET with a valid token reaches the handler (`503` not ready or `200` is fine — assert not a transport error), a plain `http://` GET gets status `400`, the stderr log contains `sha256=` with the fingerprint and `tls=true`); a cert-load failure (half pair) → exit 2 before listening.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**: in `serve`, before `net.Listen`, load the cert when `cfg.TLS` (failure → stderr + exit 2); log Info `hostpower: tls certificate` (`cert`, `sha256`, `generated`); after the MAC capture setup wrap `ln = tls.NewListener(ln, &tls.Config{Certificates: …, MinVersion: tls.VersionTLS13})` — the capture needs only the port, taken from the raw listener first; `hostpower: serving` gains `"tls", cfg.TLS`. `NewServer` sets `ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug)`. Usage text and flag help updated.
- [ ] **Step 4: Run** `go test ./internal/hostpower -count=1` → PASS.
- [ ] **Step 5: Commit** `feat(agent): serve the hostpower endpoint over TLS 1.3 with -tls`.

### Task 4: Guest option, wiring, templates, version

**Files:**
- Modify: `server-agent/internal/config/config.go` (+`_test.go`), `server-agent/main.go` (+`main_test.go`), `server-agent/internal/agent/agent.go` (Version), `gateway/backend/internal/gateway/agent_binaries.go`, `gateway/frontend/src/components/AgentTokenSection.tsx`, `server-agent/testdata/server-agent.config.jsonc`, `server-agent/README.md` env table row (and any README key guard)

**Interfaces:**
- Consumes: `collector.PowerConfig.HostpowerTLSFingerprint`.
- Produces: `config.Config.HostpowerTLSFingerprint string` (normalised 64 lowercase hex, or the trimmed raw value when invalid); `fileConfig` tag `hostpower_tls_fingerprint`; flag `-hostpower-tls-fingerprint`; env `OP_AGENT_HOSTPOWER_TLS_FINGERPRINT`.

- [ ] **Step 1: Failing tests** in `config_test.go`: precedence flag > env > file; normalisation (`" AB:cd:…"` → lowercase plain); warnings: invalid format → `"hostpower_tls_fingerprint is not a SHA-256 fingerprint …; every hostpower request will fail"`; set with empty URL → unused warning; set with `http://` URL → `"… requires an https:// hostpower_url; every hostpower request will fail"`; `https://` URL without pin → `"hostpower_url is https:// without hostpower_tls_fingerprint: the certificate must chain to a system CA and match the host name; a self-signed hostpower certificate fails every request"`; no warning for https+valid pin. `TestNormalizeHostpowerFingerprintMatchesCollector` (config test may import collector in a `_test` file only if archtest allows test imports — check; otherwise duplicate the table in both packages). `main_test.go`: resolved attrs contain `hostpower_tls_fingerprint` value. Wiring: `PowerConfig` receives it.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**; templates: key after `hostpower_token` with the spec's comment text in Go and TSX identically; regenerate golden with `cd gateway/backend && go test ./internal/gateway -run TestBuildAgentConfigJSONMatchesSharedGolden -update-agent-config-golden`; Version `0.25.0` with history comment ("0.24.0 -> 0.25.0 is the single bump for hostpower TLS. MINOR: …").
- [ ] **Step 4: Run** `cd server-agent && go test ./... -count=1`; `cd gateway/backend && go test ./internal/gateway -count=1`; `cd gateway/frontend && npx vitest run src/components/AgentTokenSection.test.tsx && npm run format:check` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower_tls_fingerprint option and agent-config templates`.

### Task 5: Documentation

**Files:**
- Modify: `server-agent/README.md` (Proxmox section: unit with second state dir, `-tls` options table rows + usage line, new "#### HTTPS with a pinned certificate (both modes)" subsection, threat-model paragraphs in MAC and token mode, "The HTTP transport is plain HTTP" paragraph), `docs/architecture/09-architecture-decisions.md` (ADR-118 + "Amended by ADR-118" note at top of ADR-116 and ADR log index if one exists), `docs/architecture/reference/config-env.md`, `docs/architecture/cross-cutting/configuration.md`, `docs/architecture/cross-cutting/telemetry-usage-observability.md` (collector table row + env table), `docs/architecture/07-deployment-view.md`, `docs/architecture/cross-cutting/certificates-tls.md`, `docs/architecture/05-building-block-view.md` (hostpower row), `docs/architecture/11-risks-and-technical-debt.md` if it lists the L2 MITM gap.

- [ ] **Step 1:** Write ADR-118 (Context / Decision / Alternatives rejected: SPKI pin, TOFU, gateway-issued certs, mTLS, https-without-pin refusal / Consequences) from the spec's threat model and behaviour matrix.
- [ ] **Step 2:** Update the other files; every new anchor link resolves.
- [ ] **Step 3: Run** `make lint-docs` → PASS.
- [ ] **Step 4: Commit** `docs: hostpower TLS with a pinned certificate (ADR-118)`.
