# Hostpower HTTP over TLS with a pinned certificate — design

Branch-local working document (removed before the pull request).

## Intent

ADR-116 added an HTTP endpoint (`GET /v1/energy`) to the Proxmox
`server-agent hostpower` service, in MAC mode (VM identified by the tap its
connection entered) and token mode (per-VM bearer token). Both run over plain
HTTP, so ADR-116 declares a layer-2 man-in-the-middle out of scope: a LAN
device or guest that poisons the host's ARP cache or bridge forwarding table
for a VM's address and relays that VM's traffic can read the VM's counter and,
in token mode, copy and replay the token. This change adds optional TLS with a
server certificate the guest pins by SHA-256, so such a relay can neither read
nor alter the exchange.

## Goals

- Host: optional HTTPS on the existing `-listen` endpoint, with a certificate
  the service generates itself on first start or the operator supplies.
- Host: print the certificate fingerprint on start and through a CLI verb, so
  the operator can copy it into the guest.
- Guest: `https://` hostpower URLs with a SHA-256 certificate pin; any other
  certificate is refused.
- Docs: the threat model moves L2 MITM into scope when TLS + pin is used.

## Non-goals

- Changing MAC-mode identity. Ingress capture (`macresolve.go`,
  `capture_linux.go`) is untouched; the TLS handshake runs over the same TCP
  connection whose SYN and segments the capture observes. TLS adds
  confidentiality and integrity only.
- Mutual TLS / client certificates, certificate rotation without a restart,
  ACME, a CA, hostname verification.
- Any gateway/portal behaviour change beyond the agent-config template key.

## Anti-goals

- A pin that silently degrades: a configured pin must never lead to a
  clear-text request or to accepting another certificate.
- Putting the private key anywhere a guest can read (the out directory is the
  virtiofs share source).

## Host design (`server-agent/internal/hostpower/`)

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-tls` | off | Serve the `-listen` endpoint over HTTPS instead of HTTP |
| `-tls-cert PATH` | `/var/lib/op-hostpower-tls/tls.crt` | Certificate (PEM) |
| `-tls-key PATH` | `/var/lib/op-hostpower-tls/tls.key` | Private key (PEM) |

- `-tls` without `-listen` is a usage error (exit 2). `-tls-cert`/`-tls-key`
  are only read with `-tls`. (My call: opt-in keeps existing deployments
  byte-identical; the path flags are validated only when used, like `-auth`.)
- Default location is a state directory of its own,
  `/var/lib/op-hostpower-tls/` (my call). Not the out directory
  `/var/lib/op-hostpower`, which is the source of the guests' virtiofs shares;
  not `/etc/op-hostpower`, which `ProtectSystem=strict` makes read-only for the
  service (the token store there is written by the CLI, never by the
  service). The README unit becomes
  `StateDirectory=op-hostpower op-hostpower-tls` with `StateDirectoryMode=0700`
  (applies to both) and lists both in `ReadWritePaths`.

### Certificate material (new file `tlscert.go`)

`LoadOrCreateCert(certPath, keyPath string, now func() time.Time) (tls.Certificate, created bool, err error)`:

- Both files exist → `tls.LoadX509KeyPair` (operator-provided or previously
  generated; key/cert mismatch is an error). If the key file's mode grants any
  group/other permission, log a Warn (operator files may legitimately be
  `0640 root:ssl-cert`; my call: warn, do not refuse).
- Neither exists → generate: ECDSA P-256 key, self-signed X.509 v3, random
  128-bit serial, subject CN `op-hostpower`, NotBefore now−1h, NotAfter
  now+100 years (my call: the guest pins the exact certificate and does not
  check validity, so expiry only matters to other clients; a century avoids a
  surprise outage), KeyUsage DigitalSignature, ExtKeyUsage ServerAuth,
  BasicConstraints CA=false, no SANs. Key as PKCS#8 `PRIVATE KEY` PEM,
  certificate as `CERTIFICATE` PEM. Parent directories created `0700` (an
  existing one with group/other bits is tightened, like `writeTokenFile`).
  Both files written atomically (temp + chmod `0600` + rename), key first,
  then certificate, so a crash between them leaves "key only" which the next
  rule reports.
- Exactly one exists → error naming the missing one; never overwrite half a
  pair.
- Paths that exist but are not regular files → error.

`CertFingerprint(der []byte) string` returns the SHA-256 of the leaf DER as
uppercase colon-separated hex (`AB:CD:…`, 95 chars) — the format
`openssl x509 -noout -fingerprint -sha256 -in tls.crt` prints, so the operator
can cross-check.

### Serving

- `serve` wraps the listener with `tls.NewListener(ln, cfg)` when `-tls` is
  set, `tls.Config{Certificates: [cert], MinVersion: tls.VersionTLS13}` (both
  ends are this binary). Handler, limits, keep-alive off, and timeouts are
  unchanged; Go's server bounds the handshake by the smallest of
  ReadHeaderTimeout/ReadTimeout/WriteTimeout (2 s). `RemoteAddr` and
  `LocalAddrContextKey` still come from the TCP connection, so MAC mode is
  unchanged (verified: the observation table de-duplicates per interface/MAC/
  SYN flag, so extra handshake segments do not grow it).
- `http.Server.ErrorLog` is routed to slog at Debug, so TLS handshake failures
  from scanners or mis-pinned guests do not flood the log (applies to the
  plain listener too; its only output there is the same class of
  connection-level error).
- Startup log: `hostpower: tls certificate` at Info with `cert` path,
  `sha256` fingerprint and `generated` bool, then the existing
  `hostpower: serving` line gains `tls=true|false`.

### CLI verb

`server-agent hostpower tls [-tls-cert PATH] fingerprint` prints the
fingerprint line of an existing certificate (exit 0), exit 1 when it cannot be
read/parsed, exit 2 on usage errors. It never generates (my call: read-only
verb, safe next to the running service; generation happens on service start).
Mirrors the `token` subcommand layout (flags before the verb).

## Guest design

### Option

`hostpower_tls_fingerprint` / `OP_AGENT_HOSTPOWER_TLS_FINGERPRINT` /
`-hostpower-tls-fingerprint`. Not a secret (logged in the resolved-config
Debug line). Accepted format: 64 hex digits, optional `:` separators,
case-insensitive, surrounding whitespace trimmed (so the host's printed value
pastes as is). Normalised in config to 64 lowercase hex digits; an invalid
value is kept as given (so the collector fails closed) and warned about.

### Behaviour matrix

| URL scheme | Pin | Behaviour |
|---|---|---|
| `http://` | none | unchanged (plain HTTP) |
| `https://` | valid | TLS 1.3; the server's leaf certificate SHA-256 must equal the pin (constant-time compare); chain, name and validity are not checked; any mismatch fails the request before a byte of HTTP (so the token never leaves) |
| `https://` | none | **decision:** unchanged from today — standard verification against the system roots and the URL host name. A self-signed hostpower certificate therefore fails every request; startup warning "hostpower_url is https:// without hostpower_tls_fingerprint: …". Rationale: today's client already speaks https with system-root verification, which is a secure configuration for an operator-provided CA-signed certificate; refusing it would break that and buy nothing. |
| `http://` | set | **fail closed:** every request fails without sending anything (the pin shows TLS was intended; a clear-text request would expose the token); startup warning |
| any | invalid format | fail closed, every request fails; startup warning |

### Client (`hostpower_http.go`)

- `NewHostpowerHTTPClient(pin []byte)` keeps building on
  `newLocalHTTPClient(nil)` (no proxy, no redirects, 1 s timeout) and, with a
  non-nil pin, sets `TLSClientConfig = &tls.Config{MinVersion: TLS13,
  InsecureSkipVerify: true, VerifyConnection: pin check}`.
  `InsecureSkipVerify` only disables the chain/name check that the pin
  replaces; the `VerifyConnection` callback is the verification (commented;
  Sonar S4830 justification). The helper client is untouched.
- `ParseHostpowerFingerprint(s string) ([]byte, error)` in `collector`
  decodes the accepted format into 32 bytes. `config` may import only
  `wslentry` (frozen archtest rule), so config carries its own ~10-line
  `normalizeHostpowerFingerprint` (trim, drop `:`, lowercase, require 64 hex);
  a cross-check test feeds the same table to both. The host formats the
  fingerprint with `collector.FormatHostpowerFingerprint` (hostpower already
  imports collector), so host output and guest parser live side by side.
- A pin mismatch returns `*HostpowerPinError` (wrapped by `url.Error`). The
  collector logs it at **Warn** once when the mismatch begins (and again
  after a success), every attempt stays at Debug as today (my call: a wrong
  certificate is either a misconfiguration or an attack, both deserve an
  operator-visible line, but not one per second). Message names the expected
  and the presented fingerprint (public values).
- `PowerConfig` gains `HostpowerTLSFingerprint string`; `main.go` passes it.

## Gateway templates

`hostpower_tls_fingerprint` added after `hostpower_token` in
`agent_binaries.go` (Go template), `AgentTokenSection.tsx` (TSX template) and
the shared golden `server-agent/testdata/server-agent.config.jsonc`, with a
comment: "Optional SHA-256 fingerprint of the hostpower TLS certificate (with
an https:// hostpower_url). Print it on the host with: server-agent hostpower
tls fingerprint".

## Version

`const Version` 0.24.0 → 0.25.0 (MINOR, as for the two previous hostpower /
helper branches: new host flags and a new agent option; no `agent.Features`
entry, no wire change).

## Docs

- `server-agent/README.md`: env table row; host options table and usage line;
  a new "HTTPS with a pinned certificate" subsection in "Linux or Windows guest
  on a Proxmox VE host" (unit `ExecStart` example with `-tls`,
  `ReadWritePaths`, fingerprint verb, guest settings); threat-model paragraphs
  in MAC and token mode updated.
- ADR-118 (new) "Hostpower HTTP over TLS 1.3 with a guest-pinned self-signed
  certificate" + an "Amended by ADR-118" note on ADR-116 whose threat-model
  and consequences bullets point at it (ADRs are append-only; the amendment
  note pattern exists for ADR-115/116).
- `reference/config-env.md`, `cross-cutting/configuration.md`,
  `cross-cutting/telemetry-usage-observability.md`, `07-deployment-view.md`
  where they describe the hostpower HTTP transport.
- `cross-cutting/certificates-tls.md`: a short section that the hostpower
  endpoint has its own self-signed, pinned certificate, separate from the
  gateway-managed certificates.

## Threat model after the change

With `-tls` and a guest pin: an L2 MITM that relays a VM's traffic sees only
TLS 1.3 records; it cannot read the counter or the token, cannot alter the
response, and cannot impersonate the host without its private key. It can
still drop or delay traffic (denial of service; the guest reports `null`).
In MAC mode a relay that forwards the connection unchanged still enters the
host on the relay's own port, so identity behaves as before (the relaying VM
or uplink gets its own counter or `401`); TLS does not change who is
identified. Out of scope remains: a compromised host, theft of the host key
file (root on the host), and a guest without a pin (https without a pin falls
back to system-root verification, which a self-signed certificate never
passes).

## Testing

- `tlscert_test.go`: generation writes both files `0600`, parent `0700`,
  reloads to the same fingerprint; existing pair is loaded unchanged; one file
  missing → error and nothing written; fingerprint matches an independent
  sha256 of the PEM block DER; format of fingerprint; mismatching pair → error.
- `cli_test.go`: `-tls` without `-listen` → usage error; `tls fingerprint`
  verb prints the value / exit 1 on missing file / exit 2 on bad verb; serve
  with `-tls` on 127.0.0.1:0 answers over TLS (token mode) and refuses plain.
- `hostpower_http_test.go`: pinned match (httptest TLS server, pin = its
  cert) → 200 decoded; pinned mismatch → failure, request never reaches the
  handler, Warn once then Debug; https without pin against a self-signed
  server → failure (system roots); http with pin → fails without a
  connection; invalid pin → fails closed; parser accepts colon/no-colon/
  case/whitespace and rejects wrong length/non-hex.
- `config_test.go`: precedence flag > env > file, normalisation, warnings.
- Golden/template guards (Go + TSX) green after regeneration.
