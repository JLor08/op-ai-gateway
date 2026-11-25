# Hostpower Transports Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Linux and Windows guests on a single Proxmox VE host receive their CPU package power share over virtiofs or over HTTP (MAC mode or token mode), selectable per VM.

**Architecture:** The existing `server-agent hostpower` service keeps its 250 ms attribution tick and per-VM counter. It additionally writes a colon-free Windows tree per VM and, when `-listen` is set, serves `GET /v1/energy` that identifies the caller by bearer token or by ARP + bridge FDB port + VM config. The guest agent gains a RAPL domain pattern per platform (Windows reads `intel-rapl_*`) and a new HTTP power sub-collector configured by `hostpower_url` / `hostpower_token`.

**Tech Stack:** Go 1.26 stdlib only (`net/http`, `net/netip`, `crypto/sha256`, `crypto/subtle`, `os.Root`), Linux procfs/sysfs, PVE 9.2 config files, virtio-win virtiofs on Windows.

**Spec:** `docs/superpowers/specs/2026-11-25-hostpower-transports-design.md`

## Global Constraints

- Module `op-ai-server-agent`, Go 1.26, stdlib only, CGO-free; SPDX + copyright header lines on every new `.go` file.
- `internal/hostpower` may import only `internal/collector` among module packages (archtest allowlist + forbidden-edge rule stay unchanged).
- HTTP is off unless `-listen` is set; `-auth` default `mac,token`; `-tokens` default `/etc/op-hostpower/tokens.json`.
- Endpoint `GET /v1/energy` → `200` JSON `{"vmid":"<id>","energy_uj":<n>,"max_energy_range_uj":0}` with `Cache-Control: no-store`; bearer present → token mode only (`401` on any failure, no MAC fallback); no header → MAC mode; identified but not live → `503`; other method `405`, other path `404`; rate limit per source IP → `429`.
- MAC mode IPv4 only (IPv4-mapped IPv6 counts); port names `^tap(\d+)i(\d+)$` or `^fwpr(\d+)p(\d+)$`; VM config `/etc/pve/qemu-server/<vmid>.conf`, only the lines before the first `[section]` count; `net<n>:` MAC and `bridge=` must both match.
- Token format `ophp_` + base64url(32 random bytes) without padding; store `{"<vmid>":{"sha256":"<hex>","created":"<RFC3339>"}}`, file mode `0600`, dir `0700`; never log a token or an `Authorization` value.
- Windows tree: `<out>/<vmid>/windows/intel-rapl_0/{name,energy_uj,max_energy_range_uj}`, same content as the Linux tree; no `:` in any name under `windows/`.
- State directory `<out>` is created `0700` by the service; README unit gets `StateDirectoryMode=0700`.
- Guest HTTP client: own `http.Transport` with `Proxy: nil`, client timeout `1s`, response body read capped at 4 KiB.
- Guest collector order: native (RAPL incl. `powercap_root`) → hostpower HTTP → LHM.
- Agent `Version` bump once on the branch: `0.22.0` → `0.23.0` (recheck main's current value before the bump task; base the bump on it).
- New ADR number: the next free one on main (currently ADR-116; recheck).
- Repo-facing text English; no LAN host names, VM names or IPs (use `<vmid>`, `<node>`, `192.0.2.x` documentation addresses).
- CI gates: `golangci-lint fmt --diff` + `golangci-lint run` per module (`make fmt` then `git diff --exit-code`, `make lint-go`), frontend `npm run format:check`, `make lint-docs`, archtest, README config guard tests.
- `docs/superpowers/**` is branch-local; removed before the PR.

## Review Focus

1. **Incomplete or stale ARP entry** (flags `0x0`, or no entry at all): MAC mode returns `401` with reason `no ARP entry`, never panics. → test in Task 7.
2. **VM config with snapshot or `[PENDING]` sections** that carry a different `net0` MAC: only the current config (before the first `[`) is used. → test in Task 7.
3. **Token store created after the service started** (missing at startup, then `token create`): picked up within about a second without restart. → test in Task 6.
4. **`hostpower_url` with a trailing slash or a path prefix** (`http://192.0.2.10:9873/`, `http://192.0.2.10/hp`): the guest requests exactly `<base>/v1/energy` once. → test in Task 4.
5. **Host restarts while a guest polls** (connection refused, then `503`, then a counter equal to the persisted value): the guest yields nil and re-baselines, never a spike. → test in Task 4.

---

### Task 1: Windows tree and a 0700 state directory (host)

**Files:**
- Modify: `server-agent/internal/hostpower/tree.go`
- Modify: `server-agent/internal/hostpower/cli.go` (`Main`: create `<out>` with `0o700`)
- Test: `server-agent/internal/hostpower/tree_test.go`

**Interfaces:**
- Produces: `const WindowsDir = "windows"`, `const WindowsDomainDir = "intel-rapl_0"`; `WriteTree(vmDir string, energyUJ uint64) error` now also writes `vmDir/windows/intel-rapl_0/` (same three files, same rules: static files rewritten when not a small regular file with the expected content, `energy_uj` replaced every call by temp+rename). The Windows subtree is opened as its own confined Root (Lstat must show a real directory, then `OpenRoot`), so a symlinked `windows` or `intel-rapl_0` escapes nothing. `ReadCounter` is unchanged (reads only the Linux tree). `RemoveStaleTemps` sweeps both domain directories. `writeAtomic` gains a domain-directory parameter instead of the hard-coded `DomainDir`.
- An error writing the Windows subtree is returned from `WriteTree` (wrapped with its path) after the Linux tree has been written, so a guest breaking its Windows subtree never freezes its Linux tree.

- [ ] **Step 1: Write the failing tests**

```go
func TestWriteTreeWritesWindowsTree(t *testing.T)            // WriteTree(dir, 42): windows/intel-rapl_0/{name="package-0\n", max_energy_range_uj="0\n", energy_uj="42\n"}
func TestWindowsTreeHasNoColonNames(t *testing.T)            // walk dir/windows: no entry name contains ":"
func TestWindowsSubtreeSymlinkEscapesNothing(t *testing.T)   // dir/windows -> outside dir: WriteTree returns error, outside dir untouched, Linux tree still has energy_uj "42\n"
func TestWindowsDomainSymlinkEscapesNothing(t *testing.T)    // dir/windows/intel-rapl_0 -> outside dir: same assertions
func TestRemoveStaleTempsSweepsWindowsTree(t *testing.T)     // ".energy_uj.abc" in both domain dirs removed, real files kept
```

Use the existing symlink/skip helpers in `tree_test.go`.

- [ ] **Step 2: Run them, expect FAIL** — `cd server-agent && go test ./internal/hostpower -run 'Windows|SweepsWindows' -v` → undefined `WindowsDir` / assertion failures.
- [ ] **Step 3: Implement** in `tree.go`; in `Main` change `os.MkdirAll(opts.OutDir, 0o755)` to `0o700` (an existing directory keeps its mode; the README unit sets `StateDirectoryMode=0700`).
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower` → PASS (all old tree tests unchanged and green).
- [ ] **Step 5: Commit** `feat(agent): hostpower writes a colon-free Windows tree per VM` (body: why `windows/intel-rapl_0`, confinement, 0700).

---

### Task 2: Live counter accessor (host)

**Files:**
- Modify: `server-agent/internal/hostpower/service.go`
- Test: `server-agent/internal/hostpower/service_test.go`

**Interfaces:**
- Produces: `func (s *Service) LiveCounter(vmid string) (uint64, bool)` — returns the counter only if `vmid` was in the VM set discovered by the most recent `Tick` (a `live map[string]bool` refreshed in `writeTrees`, under `s.mu`); `false` before the first tick and for stopped VMs (whose counters stay in `counters`, deliberately unpruned).

- [ ] **Step 1: Failing tests**

```go
func TestLiveCounterBeforeFirstTick(t *testing.T)        // New(...).LiveCounter("100") → (0,false)
func TestLiveCounterForRunningVM(t *testing.T)           // after the attribution ticks of TestTickAttributesByActiveShare: ("100") → (2_500_000,true)
func TestLiveCounterFalseAfterVMStops(t *testing.T)      // remove 100's scope dir, Tick → ("100") false; Counters()["100"] still 2_500_000
```

- [ ] **Step 2: Run, expect FAIL** (undefined `LiveCounter`).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower exposes the live per-VM counter`.

---

### Task 3: RAPL domain pattern per platform and Windows native wiring (guest)

**Files:**
- Modify: `server-agent/internal/collector/rapl.go` (constructor + glob + empty-hwmon guard)
- Modify: `server-agent/internal/collector/power_linux.go`, `power_other.go` (and the call in `power_darwin.go` only if it constructs a RAPL collector — it does not today)
- Test: `server-agent/internal/collector/rapl_test.go`, new `server-agent/internal/collector/power_other_test.go` with `//go:build !linux && !darwin` (compile-checked on darwin via `GOOS=windows go vet`)

**Interfaces:**
- Produces: `const raplDomainGlobLinux = "intel-rapl:*"`, `const raplDomainGlobWindows = "intel-rapl_*"`; `newRAPLCollector(powercapRoot, hwmonRoot, domainGlob string) *raplCollector`. With `hwmonRoot == ""` neither `Available` nor the system-watts fallback globs hwmon (no relative glob against the working directory). Linux: `newRAPLCollector(root, defaultHwmonRoot, raplDomainGlobLinux)` (behaviour unchanged). Windows/other: `newNativePowerCollector(powercapRoot)` returns `nil` when empty, else `newRAPLCollector(powercapRoot, "", raplDomainGlobWindows)`. `rapl_package.go` (host side) keeps `intel-rapl:*`.
- Update every existing `newRAPLCollector(` call (tests included) to pass the Linux glob.

- [ ] **Step 1: Failing tests**

```go
func TestRAPLWindowsGlobReadsUnderscoreDomain(t *testing.T) // root with intel-rapl_0 {package-0, 1_000_000 → 16_000_000 over 1s} → 15 W with raplDomainGlobWindows
func TestRAPLEachPlatformCountsOnce(t *testing.T)          // root with BOTH intel-rapl:0 and intel-rapl_0 (same counters, +10 J/1s): Linux glob → 10 W, Windows glob → 10 W (never 20)
func TestRAPLEmptyHwmonRootNeverGlobsCwd(t *testing.T)     // t.Chdir into a temp dir containing x/power1_input = "5000000"; newRAPLCollector(emptyRoot, "", glob): Available()==false and system watts nil
// power_other_test.go (!linux && !darwin):
func TestNativePowerCollectorWindowsWiring(t *testing.T)   // newNativePowerCollector("") == nil; newNativePowerCollector(`Z:\`) is *raplCollector with domainGlob intel-rapl_* and hwmonRoot ""
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/collector && GOOS=windows GOARCH=amd64 go vet ./internal/collector && GOOS=windows GOARCH=amd64 go test -c -o /dev/null ./internal/collector` → PASS / clean.
- [ ] **Step 5: Commit** `feat(agent): read a colon-free powercap tree on Windows guests`.

---

### Task 4: Guest HTTP power collector and the shared response type

**Files:**
- Create: `server-agent/internal/collector/hostpower_http.go`
- Test: `server-agent/internal/collector/hostpower_http_test.go`

**Interfaces:**
- Produces:
  - `type HostpowerEnergy struct { VMID string \`json:"vmid"\`; EnergyUJ uint64 \`json:"energy_uj"\`; MaxEnergyRangeUJ uint64 \`json:"max_energy_range_uj"\` }` — the single wire definition; the host (Task 8) imports it.
  - `const HostpowerEnergyPath = "/v1/energy"`.
  - `func NewHostpowerHTTPClient() *http.Client` — `Timeout: 1 * time.Second`, `Transport: &http.Transport{Proxy: nil, ...}` (clone of `http.DefaultTransport` settings minus proxy).
  - `func newHostpowerHTTPCollector(baseURL, token string, client *http.Client) *hostpowerHTTPCollector` implementing `PowerCollector`; `Name()` = `"hostpower"`; `Available()` = base URL non-empty. `Collect` builds `strings.TrimRight(baseURL, "/") + HostpowerEnergyPath`, sets `Authorization: Bearer <token>` only when token non-empty, reads at most 4 KiB, decodes `HostpowerEnergy`, computes watts with `raplDeltaJoules(prev, cur, max)` over Δt (injectable `now`). Returns system watts always nil. First success → nil. Any error / non-200 / bad JSON / changed `VMID` → nil and drop the previous reading. Never returns an error.

- [ ] **Step 1: Failing tests** (all with `httptest.Server`, injected clock)

```go
func TestHostpowerHTTPFirstSampleNil(t *testing.T)
func TestHostpowerHTTPWatts(t *testing.T)                    // 1_000_000 → 16_000_000 over 1 s → 15 W
func TestHostpowerHTTPDecreaseWithZeroRangeIsZero(t *testing.T) // 5_000_000 → 1_000_000, range 0 → 0 W (non-nil)
func TestHostpowerHTTPErrorRebaselines(t *testing.T)         // ok(1e6) → 503 → ok(16e6) → ok(31e6): third nil, fourth 15 W
func TestHostpowerHTTPVMIDChangeRebaselines(t *testing.T)    // vmid "100" then "101" → nil
func TestHostpowerHTTPSendsTokenOnlyWhenSet(t *testing.T)    // header "Bearer ophp_x" with token; absent without
func TestHostpowerHTTPBaseURLForms(t *testing.T)             // Review Focus 4: "<srv>/", "<srv>/hp" → requested paths "/v1/energy" and "/hp/v1/energy"
func TestHostpowerHTTPHostRestart(t *testing.T)              // Review Focus 5: ok(5e6) → server closed (conn refused) → 503 → ok(5e6) → ok(20e6 after 1 s): all nil until the last, which is 15 W
func TestHostpowerHTTPIgnoresProxyEnv(t *testing.T)          // t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") — request still reaches the httptest server
func TestHostpowerHTTPCapsBody(t *testing.T)                 // 1 MiB body → nil, no hang
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/collector` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower HTTP power collector for guests`.

---

### Task 5: Agent options, collector composition, gateway templates, README rows

**Files:**
- Modify: `server-agent/internal/config/config.go`, `config_test.go`
- Modify: `server-agent/internal/collector/collector.go` (+ the six existing test call sites of `DetectPowerCollector`/`DetectPowerAndTempCollectors` in `power_test.go`, `power_logging_test.go`, `lhm_source_test.go`)
- Modify: `server-agent/main.go` (wiring + startup diagnostic)
- Modify: `gateway/backend/internal/gateway/agent_binaries.go`, `gateway/frontend/src/components/AgentTokenSection.tsx`, `server-agent/testdata/server-agent.config.jsonc`
- Modify: `server-agent/README.md` (configuration table rows only; the Proxmox prose is Task 10)

**Interfaces:**
- Produces:
  - `Config.HostpowerURL string`, `Config.HostpowerToken string`; file keys `hostpower_url`, `hostpower_token`; flags `-hostpower-url`, `-hostpower-token`; env `OP_AGENT_HOSTPOWER_URL`, `OP_AGENT_HOSTPOWER_TOKEN`; both trimmed like `lhm_url`.
  - `type PowerConfig struct { LHMURL, PowercapRoot, HostpowerURL, HostpowerToken string }`; `func DetectPowerCollector(cfg PowerConfig) PowerCollector`; `func DetectPowerAndTempCollectors(cfg PowerConfig) (PowerCollector, TempCollector)`. Composition order native → hostpower HTTP (when `HostpowerURL != ""`, client from `NewHostpowerHTTPClient()`) → LHM. Temp composition unchanged.
  - Startup diagnostic (Debug resolved-config line): `hostpower_url` and `hostpower_token_set` (bool); never the token.
  - Template block after `powercap_root`, identical in Go template, TSX template and golden:

```text
  // Optional hostpower HTTP endpoint on the Proxmox host (e.g.
  // http://<host>:9873). Empty disables it. Without a token the host
  // identifies this VM by its network port (MAC mode).
  "hostpower_url": "",

  // Optional per-VM token for the hostpower HTTP endpoint (token mode).
  // Create it on the host with: server-agent hostpower token create <vmid>
  "hostpower_token": "",
```

- [ ] **Step 1: Failing tests**

```go
func TestHostpowerOptionsResolution(t *testing.T)            // default ""; flag; env; file; flag > env > file — for both options (mirror TestPowercapRootResolution)
func TestDetectPowerCollectorOrderWithHostpower(t *testing.T) // PowerConfig{HostpowerURL: srv.URL, LHMURL: lhm.URL}: PowerSources(...) lists "hostpower" before "lhm" (and after the native name when one exists on the OS)
// main_test.go — extract the Debug resolved-config attributes in main.go into func resolvedConfigAttrs(cfg config.Config) []any and test it:
func TestResolvedConfigAttrsNeverCarryToken(t *testing.T)     // cfg.HostpowerToken = "ophp_secret": fmt.Sprint(attrs) lacks "ophp_secret", contains hostpower_token_set=true and the hostpower_url
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**; update the six test call sites to `PowerConfig{...}`; add both README config-table rows (env / flag / file key / default `—` / description) so `TestGeneratedAgentConfigDocumentsEverySettingInTheREADME` stays green.
- [ ] **Step 4: Run** `cd server-agent && go test -race ./... && cd ../gateway/backend && go test ./internal/gateway -run AgentConfig && cd ../frontend && npx vitest run src/components/AgentTokenSection && npm run format:check` → all PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower_url and hostpower_token agent options` (body: options, order, templates, README guard).

---

### Task 6: Token store and token CLI (host)

**Files:**
- Create: `server-agent/internal/hostpower/tokens.go`
- Test: `server-agent/internal/hostpower/tokens_test.go`

**Interfaces:**
- Produces:
  - `const DefaultTokensPath = "/etc/op-hostpower/tokens.json"`, `const tokenPrefix = "ophp_"`.
  - `func CreateToken(path, vmid string) (string, error)` — validates numeric vmid, generates the token, stores only `sha256` hex + `created` (UTC RFC 3339), replaces an existing entry, writes atomically (temp + rename in the same dir, `0600`; creates the dir `0700`), returns the plaintext once.
  - `func RevokeToken(path, vmid string) error` (no error if absent), `func ListTokens(path string) ([]TokenInfo, error)` with `type TokenInfo struct { VMID string; Created time.Time }`, sorted numerically by VMID.
  - `type TokenStore struct` with `func NewTokenStore(path string) *TokenStore` and `func (s *TokenStore) Lookup(token string) (vmid string, ok bool)` — reloads when the file's mtime or size changed, checked at most once per second (injectable clock); missing file = empty set; unparsable file keeps the last good set and logs once at Warn (`hostpower: token store unreadable, keeping previous tokens`) until it parses again; compares SHA-256 of the presented token against every entry with `subtle.ConstantTimeCompare`; rejects tokens without the prefix before hashing. Goroutine-safe.
  - `func TokenMain(args []string, stdout, stderr io.Writer) int` — usage `server-agent hostpower token [-tokens PATH] create|revoke|list [vmid]`; `create` prints only the token and a newline to stdout; exit 2 on usage errors, 1 on I/O errors.

- [ ] **Step 1: Failing tests**

```go
func TestCreateTokenStoresOnlyHash(t *testing.T)          // file has no "ophp_" substring; mode 0600; dir 0700; token has prefix, 43 chars after prefix
func TestCreateTokenReplacesExisting(t *testing.T)        // old token no longer looks up
func TestRevokeAndList(t *testing.T)
func TestCreateTokenRejectsNonNumericVMID(t *testing.T)
func TestTokenStoreLookup(t *testing.T)                   // created token → ("100", true); wrong / unprefixed / empty → false
func TestTokenStorePicksUpLateFile(t *testing.T)          // Review Focus 3: store on missing path → false; CreateToken; advance clock 1s → true
func TestTokenStoreReloadRateLimited(t *testing.T)        // change within the same second not seen; after 1s seen
func TestTokenStoreCorruptKeepsLastGood(t *testing.T)     // valid → overwrite with "{" → old token still ok; one Warn record (recording handler)
func TestTokenMainCreatePrintsTokenOnly(t *testing.T)     // stdout == token+"\n"; list shows vmid and time, never "sha256" values
func TestTokenMainUsageErrors(t *testing.T)               // no verb / unknown verb / missing vmid → 2
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower per-VM token store and token CLI`.

---

### Task 7: MAC-mode resolver (host)

**Files:**
- Create: `server-agent/internal/hostpower/macresolve.go`
- Test: `server-agent/internal/hostpower/macresolve_test.go`

**Interfaces:**
- Produces:
  - `const DefaultARPPath = "/proc/net/arp"`, `DefaultSysClassNet = "/sys/class/net"`, `DefaultPVEConfDir = "/etc/pve/qemu-server"`.
  - `type MACResolver struct { ARPPath, SysClassNet, PVEConfDir string }` (empty fields → defaults).
  - `func (r MACResolver) Resolve(remote netip.Addr) (vmid string, err error)`; errors are `*ResolveError{Reason string}` with reasons exactly: `ipv6 not supported`, `no ARP entry`, `not a bridge`, `no FDB entry`, `not a VM bridge port`, `no VM config`, `MAC mismatch`.
  - Algorithm per spec §4.4: `/proc/net/arp` line with IP == remote (after `Unmap`), flags `0x2`, device D; D is a bridge iff `SysClassNet/D/bridge` exists; FDB from `SysClassNet/D/brforward` as 16-byte records `{mac[6], port_no u8, is_local u8, ageing u32, port_hi u8, pad u8, unused u16}` with port = `port_hi<<8 | port_no`, skipping `is_local != 0`; port interface = the entry of `SysClassNet/D/brif/*` whose `port_no` file (hex, e.g. `0x3`) equals port; name regex → vmid, NIC n; config `PVEConfDir/<vmid>.conf`, lines before the first line starting with `[`, `net<n>:` value split on `,`, first `model=MAC` pair whose value parses as a MAC, and `bridge=` equal to D; MAC compare case-insensitive.
  - No caching in this task (the server may add the tuple-keyed cache later only if profiling asks for it — not planned).

- [ ] **Step 1: Failing tests** (synthetic trees built by a helper `fakeNet(t, ...)` that writes `arp`, `sys/class/net/vmbr0/{bridge/, brforward, brif/<port>/port_no}`, `qemu-server/<vmid>.conf`)

```go
func TestResolveTapPort(t *testing.T)                 // 192.0.2.50 → bc:24:11:00:00:64 on tap100i0 → "100"
func TestResolveFirewallPort(t *testing.T)            // learned on fwpr101p0 → "101"
func TestResolveUplinkRejected(t *testing.T)          // learned on enp3s0 → "not a VM bridge port"
func TestResolveVethRejected(t *testing.T)            // veth200i0 → "not a VM bridge port"
func TestResolveMACMismatch(t *testing.T)             // tap100i0 but conf net0 MAC differs → "MAC mismatch"
func TestResolveWrongNICIndex(t *testing.T)           // learned on tap100i1, conf has only net0 → "MAC mismatch"
func TestResolveWrongBridge(t *testing.T)             // conf bridge=vmbr1 → "MAC mismatch"
func TestResolveIncompleteARP(t *testing.T)           // Review Focus 1: flags 0x0 → "no ARP entry"; IP absent → "no ARP entry"
func TestResolveIgnoresSnapshotSections(t *testing.T) // Review Focus 2: current net0 MAC A; [snap1] net0 MAC B; request with B → "MAC mismatch"; with A → "100"
func TestResolveLocalFDBEntrySkipped(t *testing.T)    // only an is_local entry for the MAC → "no FDB entry"
func TestResolveIPv6(t *testing.T)                    // 2001:db8::1 → "ipv6 not supported"; ::ffff:192.0.2.50 → resolves like IPv4
func TestResolveNotABridge(t *testing.T)              // ARP device eth0 without bridge dir → "not a bridge"
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** (stdlib only; read files with a size cap — `brforward` up to 1 MiB, others 64 KiB).
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower && GOOS=linux GOARCH=amd64 go vet ./internal/hostpower` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower identifies a VM from ARP, bridge port and VM config`.

---

### Task 8: HTTP server (host)

**Files:**
- Create: `server-agent/internal/hostpower/server.go`
- Test: `server-agent/internal/hostpower/server_test.go`

**Interfaces:**
- Consumes: `collector.HostpowerEnergy`, `collector.HostpowerEnergyPath` (Task 4); `(*Service).LiveCounter` (Task 2); `*TokenStore` (Task 6); `MACResolver` (Task 7).
- Produces:
  - `type AuthModes struct { MAC, Token bool }`; `func ParseAuthModes(s string) (AuthModes, error)` — comma list of `mac`, `token`; empty or unknown → error.
  - `type counterSource interface { LiveCounter(string) (uint64, bool) }`, `type vmResolver interface { Resolve(netip.Addr) (string, error) }`, `type tokenLookup interface { Lookup(string) (string, bool) }`.
  - `func newHandler(modes AuthModes, counters counterSource, resolver vmResolver, tokens tokenLookup, log *slog.Logger, now func() time.Time) http.Handler` implementing the global-constraint rules; per-IP token bucket 10 req/s, burst 20; limiter and log-state maps evict entries idle for more than 1 minute.
  - Rejection logging: one Warn per source IP when its reason changes (`hostpower: request rejected`, attrs `remote`, `reason`), one Info when it later succeeds; never the token or header.
  - `func NewServer(listen string, h http.Handler) *http.Server` with `ReadHeaderTimeout: 2s`, `ReadTimeout: 5s`, `WriteTimeout: 5s`, `IdleTimeout: 30s`, `MaxHeaderBytes: 4096`.

- [ ] **Step 1: Failing tests** (fakes for the three interfaces; `httptest.NewRecorder` with `req.RemoteAddr` set)

```go
func TestHandlerTokenModeServesOwnVM(t *testing.T)        // Bearer → vmid "100" counter 7 → 200, JSON {"vmid":"100","energy_uj":7,"max_energy_range_uj":0}, Cache-Control no-store
func TestHandlerBadTokenNoMACFallback(t *testing.T)       // Bearer wrong, resolver would succeed → 401
func TestHandlerTokenModeDisabled(t *testing.T)           // modes{MAC:true}: Bearer valid → 401
func TestHandlerMACMode(t *testing.T)                     // no header, resolver → "101" → 200 with 101's counter
func TestHandlerMACModeDisabled(t *testing.T)             // modes{Token:true}, no header → 401, resolver not called
func TestHandlerNotLive(t *testing.T)                     // identified but LiveCounter false → 503
func TestHandlerMethodAndPath(t *testing.T)               // POST → 405; GET /x → 404
func TestHandlerRateLimit(t *testing.T)                   // 21 requests at one instant from one IP → last 429; other IP 200
func TestHandlerNeverLogsToken(t *testing.T)              // recording handler: "ophp_secret" absent from every record
func TestHandlerLogsRejectionOncePerChange(t *testing.T)  // 3× same reason → 1 Warn; then success → 1 Info
func TestParseAuthModes(t *testing.T)                     // "mac,token", "token", "mac" ok; "", "foo" error
```

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower` → PASS.
- [ ] **Step 5: Commit** `feat(agent): hostpower serves each VM its energy counter over HTTP`.

---

### Task 9: CLI wiring: flags, token subcommand, server lifecycle

**Files:**
- Modify: `server-agent/internal/hostpower/cli.go`, `cli_test.go`
- Modify: `server-agent/main.go` (`hostpower.Main(os.Args[2:], os.Stdout, os.Stderr)`)

**Interfaces:**
- Produces:
  - `type Config struct { Options; Interval time.Duration; Verbose bool; Listen string; Auth AuthModes; TokensPath string }`; `func ParseArgs(args []string) (Config, error)` (replaces the 4-tuple; existing tests migrate) with new flags `-listen` (default ""), `-auth` (default `mac,token`, validated by `ParseAuthModes` only when `-listen` is set), `-tokens` (default `DefaultTokensPath`).
  - `func Main(args []string, stdout, stderr io.Writer) int` — if `args[0] == "token"` → `TokenMain(args[1:], stdout, stderr)`. Otherwise as today, plus: when `Listen != ""`, `net.Listen("tcp", Listen)` before starting the tick loop (failure → stderr message + exit 2), log Info `hostpower: serving` with `listen` and `auth`, serve in a goroutine with the handler from Task 8 (`MACResolver{}`, `NewTokenStore(TokensPath)`, the Service), and on shutdown `srv.Shutdown` with a 2 s timeout. Usage text updated.

- [ ] **Step 1: Failing tests**

```go
func TestParseArgsHTTPDefaults(t *testing.T)            // Listen "", Auth {MAC,Token} , TokensPath DefaultTokensPath
func TestParseArgsHTTPCustom(t *testing.T)              // -listen 127.0.0.1:0 -auth token -tokens /tmp/t.json
func TestParseArgsBadAuth(t *testing.T)                 // -listen x -auth foo → error
func TestMainTokenSubcommand(t *testing.T)              // Main(["token","-tokens",p,"create","100"], &out, &err) == 0, out has the token
func TestMainListenFailureExits2(t *testing.T)          // occupy a port with net.Listen, Main(["-out",tmp,"-listen",thatAddr]) == 2
```

Keep the existing CLI tests green after migrating them to `Config`.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test -race ./internal/hostpower . && go build ./... && GOOS=windows GOARCH=amd64 go build ./... && GOOS=linux GOARCH=amd64 go vet ./...`; manual smoke test: build to `/tmp`, run `hostpower -out <tmp> -interval 100ms -listen 127.0.0.1:<free port>`, then `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:<port>/v1/energy` → `401` (no ARP entry on macOS, so MAC mode rejects) and with `-H 'Authorization: Bearer ophp_x'` → `401`; SIGTERM → exit 0; delete the binary and tmpdir.
- [ ] **Step 5: Commit** `feat(agent): hostpower -listen/-auth/-tokens and the token subcommand`.

---

### Task 10: Documentation, ADR, version bump

**Files:**
- Modify: `server-agent/README.md` (Proxmox section), `server-agent/internal/agent/agent.go` (`Version` + history paragraph), `server-agent/internal/agent/features_test.go` (version pins and their messages)
- Modify: `docs/architecture/09-architecture-decisions.md` (new ADR), `docs/architecture/cross-cutting/telemetry-usage-observability.md`, `docs/architecture/cross-cutting/configuration.md`, `docs/architecture/reference/config-env.md`, `docs/architecture/07-deployment-view.md` §7.5

- [ ] **Step 1: Version** — recheck `git show origin/main:server-agent/internal/agent/agent.go | grep '^const Version'`; bump MINOR from that value (expected `0.22.0` → `0.23.0`) with a history paragraph in the existing style; update the pins in `features_test.go` including their failure messages.
- [ ] **Step 2: README** — restructure "Linux guest on a Proxmox VE host (virtiofs-attributed share)" into "Linux or Windows guest on a Proxmox VE host": common host part (service, unit with `StateDirectoryMode=0700` and an optional `-listen <vmbr0-address>:9873`, ro bind mount), a transport decision table (spec §4.7), then three subsections: virtiofs (Linux mapping `<vmid>`, Windows mapping `<vmid>/windows` with VirtIO-FS service + WinFsp, drive letter, `powercap_root = Z:\`, ACLs not exposed, `cache=never`), HTTP MAC mode (only `hostpower_url`; requirements: same Linux bridge, IPv4; how identity works in three sentences; PVE firewall MAC filter recommended), HTTP token mode (`token create|revoke|list`, `hostpower_url` + `hostpower_token`, plaintext HTTP caveat). Failure modes: single host only (no cluster, migration reports null), service stopped → frozen counter → measured 0 W. Keep all existing correct content (cadence rule, limitations).
- [ ] **Step 3: ADR** — next free number (recheck main): "Hostpower transports: a colon-free Windows virtiofs tree and an HTTP endpoint that identifies the VM by bridge port or per-VM token". Decision, the MAC-mode identity argument (spec §4.4 "Why this is safe"), token storage, TLS deferred, proxy-free guest client, single-host scope, alternatives considered (UUID-based identification, alias next to the Linux tree), consequences.
- [ ] **Step 4: Architecture docs** — telemetry: hostpower section gains the HTTP endpoint and Windows tree, power-source table gains the `hostpower` collector, agent env table gains both options; configuration.md telemetry bullet; config-env rows for `OP_AGENT_HOSTPOWER_URL` / `OP_AGENT_HOSTPOWER_TOKEN` (token marked secret); deployment §7.5 (listener on the bridge address, token store path).
- [ ] **Step 5: Verify** `cd server-agent && go test ./internal/agent ./internal/config -count=1` and from the root `make lint-docs` (only the branch-local docs check may fail).
- [ ] **Step 6: Commit** `docs(agent): hostpower transports for Linux and Windows guests; bump agent to 0.23.0`.

---

### Task 11: Full verification

- [ ] **Step 1:** `make fmt && git diff --exit-code && make lint-go` from the repo root → no diff, 0 issues.
- [ ] **Step 2:** `cd server-agent && go test -race -count=1 ./...` → all ok (includes archtest and README guards).
- [ ] **Step 3:** `cd server-agent && GOOS=linux GOARCH=amd64 go vet ./... && GOOS=windows GOARCH=amd64 go vet ./... && GOOS=windows GOARCH=amd64 go test -c -o /dev/null ./internal/collector && GOOS=darwin GOARCH=arm64 go build ./...`.
- [ ] **Step 4:** gateway `go test ./internal/gateway`; frontend `npx vitest run --retry=2`, `npm run format:check`, `npx tsc --noEmit` (run `npm ci` first in a fresh worktree).
- [ ] **Step 5:** Record the operator acceptance checklist for the PR (spec §4.9 manual list), then hand over to the finishing flow (Sonar gate, remove `docs/superpowers/`, push over SSH, PR, Auto-fix).
