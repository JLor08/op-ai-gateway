# Privileged agent helper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A root-only `server-agent helper` mode that serves raw RAPL counters and the DIMM inventory over a peer-checked Unix socket, and an unprivileged agent that uses it automatically.

**Architecture:** Wire types, the SMBIOS parser, the sysfs RAPL reader and the Unix-socket HTTP client live in `internal/collector` (shared by agent and helper). The helper process lives in a new leaf package `internal/helper` (HTTP over a Unix listener that drops peers whose `SO_PEERCRED` uid is not the agent's). The agent adds a `helper` power sub-collector between native RAPL and a split-out hwmon collector, and fetches memory modules from the helper when not root.

**Tech Stack:** Go 1.26, stdlib `net/http`, `golang.org/x/sys/unix` (already a dependency), `os/user` (pure Go; builds are `CGO_ENABLED=0`).

**Spec:** `docs/superpowers/specs/2026-11-25-agent-privileged-helper-design.md` (read it first; this plan argues from it).

## Global Constraints

- Linux only for the helper and the agent's helper use. macOS and Windows behaviour unchanged.
- Helper never executes a process, never opens a non-Unix socket, accepts only GET.
- Names: mode `helper`; default socket `/run/op-agent-helper/agent.sock`; agent option `helper_socket` / `OP_AGENT_HELPER_SOCKET` / `-helper-socket`; value `off` disables; power sub-collector name `helper`; split hwmon collector name `hwmon`.
- Endpoints: `GET /v1/rapl` → `{"domains":[{"zone","name","energy_uj","max_energy_range_uj"}]}`; `GET /v1/memory-modules` → `{"modules":[sample.MemoryModule…]}`.
- RAPL served from a cache refreshed at most every **250 ms**. Memory modules parsed once at helper start.
- Client body caps: **4096** bytes for `/v1/rapl`, **65536** for `/v1/memory-modules`. Client timeout 1 s (hostpower's).
- Socket directory `0750 root:<agent-gid>`, socket `0660 root:<agent-gid>`. Peer uid must equal the agent uid; uid 0 is never implicitly allowed and may not be configured as the agent user.
- Helper rate limit: one token bucket, 20 req/s, burst 40 → 429.
- Denied-peer Warn at most once per 10 min per peer uid.
- SMBIOS: never decode serial number, asset tag, part number or UUID. Speed precedence: rated Speed (0x15, ext 0x54) first, Configured (0x20, ext 0x58) second. Type `Unknown` → empty; everything else (incl. `Other`) passed through.
- Agent `Version` bump: minor bump of whatever `main` has when the branch is finalized (today `0.23.0` → `0.24.0`); merge `origin/main` first.
- Every new Go file carries the SPDX/copyright header used in the repo. Repo-facing text in English.
- Gates per task: `cd server-agent && go test -race ./...`, `make fmt` and `make lint-go` from the repo root (gofumpt, gocritic via golangci-lint) before each commit.

## Review Focus

1. A `helper_socket` path that does not exist (helper not installed) on an unprivileged agent: one Debug line per failure, no Warn/Info spam, power and inventory degrade exactly as today. → Task 3 test `TestHelperPowerMissingSocketQuiet`, Task 6 test `TestMemoryRetryStopsQuietlyWithoutHelper`.
2. The helper restarted while the agent runs (counters continue, connection refused for a moment): the agent must re-baseline, never diff across the gap into a spike. → Task 3 test `TestHelperPowerFailureResetsBaseline`.
3. A host where a RAPL zone is added/removed or renumbered between requests (zone set changes): no delta across different zones. → Task 3 test `TestHelperPowerZoneSetChange`.
4. A socket path whose parent is a symlink or world-writable directory planted by another user: the helper must refuse to start. → Task 4 test `TestListenRefusesSymlinkDir` and `TestListenRefusesForeignOwnedDir` (owner check skipped when not root, as documented in the test).
5. Malformed or truncated SMBIOS tables (odd firmware): no panic, partial result. → Task 1 `FuzzParseSMBIOSMemory` plus `TestParseSMBIOSTruncatedStringSet`.

---

## Phase 1 — shared collector pieces

### Task 1: SMBIOS type-17 parser; root agent reads the DMI table in-process

**Files:**
- Create: `server-agent/internal/collector/smbios.go`, `server-agent/internal/collector/smbios_test.go`
- Modify: `server-agent/internal/collector/hwinfo_linux.go` (root path), `server-agent/internal/collector/hwinfo.go` (comment on `hwCollectTimeout`)
- Delete: `server-agent/internal/collector/dmidecode.go`, `server-agent/internal/collector/dmidecode_test.go`

**Interfaces:**
- Produces: `func ParseSMBIOSMemory(table []byte) []sample.MemoryModule`; `func ReadSMBIOSMemoryModules(path string) ([]sample.MemoryModule, error)` (reads the file, at most 1 MiB, then parses); `const DefaultDMITablePath = "/sys/firmware/dmi/tables/DMI"`.

Structure walk (DSP0134): header `type(1) length(1) handle(2)`; formatted area of `length` bytes; string set = NUL-terminated strings ending with an extra NUL (an empty set is two NULs). String index 0 = no string. Type 127 = end of table. Type 17 offsets: Size word `0x0C` (`0` empty → skip, `0xFFFF` unknown → skip, `0x7FFF` → Extended Size dword `0x1C` bits 0-30 in MiB, else bit 15 set = KiB units, clear = MiB), Locator string `0x10`, Memory Type byte `0x12`, Speed word `0x15` (`0xFFFF` → dword `0x54`), Configured Speed word `0x20` (`0xFFFF` → dword `0x58`). Read a field only when `length` covers it.

Memory type names (byte → string, as dmidecode prints): 0x01 Other, 0x02 Unknown, 0x03 DRAM, 0x04 EDRAM, 0x05 VRAM, 0x06 SRAM, 0x07 RAM, 0x08 ROM, 0x09 Flash, 0x0A EEPROM, 0x0B FEPROM, 0x0C EPROM, 0x0D CDRAM, 0x0E 3DRAM, 0x0F SDRAM, 0x10 SGRAM, 0x11 RDRAM, 0x12 DDR, 0x13 DDR2, 0x14 DDR2 FB-DIMM, 0x18 DDR3, 0x19 FBD2, 0x1A DDR4, 0x1B LPDDR, 0x1C LPDDR2, 0x1D LPDDR3, 0x1E LPDDR4, 0x1F Logical non-volatile device, 0x20 HBM, 0x21 HBM2, 0x22 DDR5, 0x23 LPDDR5, 0x24 HBM3. Unlisted → empty.

Fixtures: no Linux box is available. Tests build tables with a small in-test encoder `type17(t, opts) []byte` (+ `endOfTable()`), using real-world values: the existing dmidecode fixture (16384 MB DDR4 3200 "DIMM 0" + empty slot), a DDR5 desktop module (32 GB, rated 4800, configured 5600 → expect 4800), a 128 GiB module via Extended Size (`0x7FFF` + dword 131072), a KiB-granularity size (bit 15), a 2.6-era 27-byte structure lacking the speed fields, an `Other` type, interleaved non-17 structures (type 0, 16) with string sets. A real table from the container run (Task 8) is added as `testdata/smbios/*.bin` with its expected modules if capturing succeeds.

- [ ] **Step 1: Write failing tests** `TestParseSMBIOSMemory` (table-driven over the cases above, asserting exact `sample.MemoryModule` values, e.g. `{Locator:"DIMM 0", SizeBytes:16384<<20, Type:"DDR4", SpeedMHz:3200}`), `TestParseSMBIOSTruncatedStringSet` (structure whose string set has no double NUL → modules decoded before it are returned, no panic), `TestParseSMBIOSNoSensitiveFields` (reflect over `sample.MemoryModule` fields: exactly Locator, SizeBytes, Type, SpeedMHz), `FuzzParseSMBIOSMemory` (seed corpus = the table cases; must not panic), `TestReadSMBIOSMemoryModules` (temp file; missing file → error).
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector -run 'SMBIOS' -v` → FAIL (undefined).
- [ ] **Step 3: Implement** `smbios.go`. Then in `hwinfo_linux.go` replace `linuxMemoryModules`' dmidecode exec with `ReadSMBIOSMemoryModules(DefaultDMITablePath)` under `os.Geteuid() == 0` (error → nil). Delete `dmidecode.go` and its test. (The non-root branch is added in Task 6; keep the function signature `linuxMemoryModules(ctx context.Context) []sample.MemoryModule` for now.)
- [ ] **Step 4: Run** `go test -race ./internal/collector/...` and `go test ./internal/collector -run Fuzz -fuzz FuzzParseSMBIOSMemory -fuzztime 30s` → PASS.
- [ ] **Step 5: Commit** `feat(agent): parse SMBIOS memory devices in-process instead of running dmidecode`.

### Task 2: Helper wire types, sysfs RAPL reader, Unix-socket client

**Files:**
- Create: `server-agent/internal/collector/helper_client.go`, `server-agent/internal/collector/helper_client_test.go`, `server-agent/internal/collector/rapl_toplevel.go`, `server-agent/internal/collector/rapl_toplevel_test.go`
- Modify: `server-agent/internal/collector/hostpower_http.go` (extract shared transport setup)

**Interfaces:**
- Produces (all in package `collector`):
  - `const HelperRAPLPath = "/v1/rapl"`, `const HelperMemoryModulesPath = "/v1/memory-modules"`, `const HelperDefaultSocket = "/run/op-agent-helper/agent.sock"`.
  - `type HelperRAPLDomain struct { Zone string \`json:"zone"\`; Name string \`json:"name"\`; EnergyUJ uint64 \`json:"energy_uj"\`; MaxEnergyRangeUJ uint64 \`json:"max_energy_range_uj"\` }`; `type HelperRAPL struct { Domains []HelperRAPLDomain \`json:"domains"\` }`; `type HelperMemoryModules struct { Modules []sample.MemoryModule \`json:"modules"\` }`.
  - `func ReadTopLevelRAPLDomains(powercapRoot string) []HelperRAPLDomain` — every `intel-rapl:N` directory (exactly one colon, N decimal) with a non-empty `name` and a readable `energy_uj`; `Zone` = directory base name; sorted by zone; `max_energy_range_uj` 0 when unreadable.
  - `func NewHelperHTTPClient(socketPath string) *http.Client` — same settings as `NewHostpowerHTTPClient` (no proxy, no redirects, 1 s timeout) but `DialContext` always dials `unix` `socketPath`. Factor the common transport/client setup into one unexported function both constructors call.
  - `func helperURL(path string) string` → `"http://op-agent-helper" + path` (the single URL builder).
  - `func fetchHelperJSON(ctx context.Context, c *http.Client, path string, maxBody int64, out any) error` — GET, non-200 → `*hostpowerStatusError`-like typed error (reuse it), bounded drain, cap enforcement ("response body too large"), JSON decode.
  - `func FetchHelperMemoryModules(ctx context.Context, c *http.Client) ([]sample.MemoryModule, error)` (cap 65536).

- [ ] **Step 1: Write failing tests.** `TestReadTopLevelRAPLDomains` (synthetic tree: `intel-rapl:0` package-0, `intel-rapl:1` psys, `intel-rapl:0:0` core → excluded, `intel-rapl:2` with mode-000 `energy_uj` → excluded when not root (skip that sub-case under root), `intel-rapl:x` → excluded; assert exact slice). `TestHelperClientOverUnixSocket` (start `http.Server` on `net.Listen("unix", tmp/s.sock)` serving fixed JSON; `NewHelperHTTPClient(path)` fetches `HelperRAPLPath` and gets the body; request `Host` is `op-agent-helper`). `TestHelperClientIgnoresProxyEnv` (`t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")` → still reaches the socket). `TestHelperClientNoRedirect` (302 → status error). `TestFetchHelperMemoryModulesCap` (65537-byte body → "response body too large"; 60 KiB valid body → OK). Use `t.TempDir()` with a short path (macOS 104-byte sun_path limit: create the dir with `os.MkdirTemp("/tmp", "hc")`).
- [ ] **Step 2: Run** `go test ./internal/collector -run 'Helper|TopLevel' -v` → FAIL.
- [ ] **Step 3: Implement** the files above; keep `NewHostpowerHTTPClient`'s behaviour byte-for-byte (its tests stay green unchanged).
- [ ] **Step 4: Run** `go test -race ./internal/collector/...` → PASS.
- [ ] **Step 5: Commit** `feat(agent): helper wire types, top-level RAPL reader and Unix-socket client`.

### Task 3: `helper` power sub-collector, hwmon split, composite order

**Files:**
- Create: `server-agent/internal/collector/helper_power.go`, `server-agent/internal/collector/helper_power_test.go`, `server-agent/internal/collector/hwmon_power.go`, `server-agent/internal/collector/hwmon_power_test.go`
- Modify: `server-agent/internal/collector/rapl.go` (move hwmon input listing to a free function), `power_linux.go`, `power_darwin.go`, `power_other.go`, `collector.go` (`PowerConfig`, `nativeAndHostpowerSubs`), `power_linux_test.go` / `power_test.go` (ordering)

**Interfaces:**
- Consumes: Task 2 `HelperRAPL`, `NewHelperHTTPClient`, `fetchHelperJSON`, `HelperRAPLPath`; existing `raplDeltaJoules`.
- Produces: `PowerConfig.HelperSocket string` (empty = off); `func newHelperPowerCollector(c *http.Client) *helperPowerCollector` with `Name() == "helper"`, `Available() == true`; `func hwmonPowerInputs(root string) []string`; `type hwmonPowerCollector` (`Name() == "hwmon"`, CPU always nil, system = first readable `power*_input` µW → W); per-OS `func newNativeHwmonCollector() PowerCollector` (Linux: reads `/sys/class/hwmon`; elsewhere nil); `func helperEnabled(socket string) bool` (Linux && euid != 0 && socket != "") as a package var-backed func so tests can override euid.

Behaviour: `helperPowerCollector.Collect` fetches `/v1/rapl` (cap 4096); per zone keyed by `Zone`, same rules as `raplCollector`: sum zones whose `Name` has prefix `package` → CPU, zone named `psys` → system; first sample per zone yields nothing; a fetch failure drops all previous readings and returns `(nil, nil, nil)`; a zone absent from the response drops its previous reading; negative watts skipped. Failures logged at Debug only; the first success logs Info once ("helper: serving RAPL counters", with zone count). On Linux `newNativePowerCollector` now passes `hwmonRoot = ""` (RAPL only). New Linux composite order: native RAPL → helper (when `helperEnabled`) → hwmon → hostpower HTTP → LHM.

- [ ] **Step 1: Write failing tests** (fake server on a Unix socket, injectable `now`): `TestHelperPowerWatts` (two samples 1 s apart, package-0 +10 J and package-1 +5 J → CPU 15 W; psys +40 J → system 40 W), `TestHelperPowerWraparound` (energy drops, max range set → corrected delta), `TestHelperPowerFailureResetsBaseline` (sample, 500, sample → nil, then next sample → value again only after a fresh baseline; no delta across the failure), `TestHelperPowerZoneSetChange` (zone `intel-rapl:1` disappears then returns → no delta for it on return), `TestHelperPowerMissingSocketQuiet` (nonexistent socket: Collect returns nils and no error; with a capturing slog handler, no record above Debug over 3 cycles), `TestHwmonPowerCollector` (synthetic hwmon tree), `TestLinuxCompositeOrder` (Linux-tagged: `PowerSources` == `["rapl","helper","hwmon"]` with helper enabled, `["rapl","hwmon"]` when `helperEnabled` reports root or socket empty), `TestRootRAPLPsysBeatsHwmon` and `TestHelperPsysBeatsHwmon` (composite of fakes: native RAPL nil system + helper 40 W + hwmon 55 W → 40 W; native 30 W → 30 W).
- [ ] **Step 2: Run** `go test ./internal/collector -run 'HelperPower|Hwmon|Composite|BeatsHwmon' -v` → FAIL.
- [ ] **Step 3: Implement.** Existing rapl tests that relied on the built-in hwmon fallback move to `hwmon_power_test.go` or construct with an explicit hwmon root (the `raplCollector` hwmon fallback itself stays for the Windows/guest constructor paths that already pass `""`; keep it simple — if no remaining caller passes a non-empty hwmon root, remove the field and its fallback entirely).
- [ ] **Step 4: Run** `go test -race ./internal/collector/...` → PASS.
- [ ] **Step 5: Commit** `feat(agent): helper power source between native RAPL and hwmon`.

## Phase 2 — the helper process

### Task 4: Peer identity, `SO_PEERCRED` listener, socket setup

**Files:**
- Create: `server-agent/internal/helper/identity.go`, `identity_test.go` (any OS), `server-agent/internal/helper/listener_linux.go`, `listener_linux_test.go`, `server-agent/internal/helper/listener_other.go`

**Interfaces:**
- Produces:
  - `type Identity struct { UID, GID int }`; `func ResolveIdentity(user, group string) (Identity, error)` — `user` is a name or decimal uid (`os/user.Lookup`, then `LookupId`; a numeric id with no passwd entry is accepted as-is); `group` empty → the user's primary gid (error if the uid has no passwd entry); resolved uid 0 → error `"helper: -agent-user must not be root"`.
  - `func authorizePeer(peerUID int, want Identity) (ok bool, reason string)` — the single decision point; reasons `"uid mismatch"`. (gid is for socket ownership only, not authorization.)
  - Linux: `func Listen(path string, id Identity, log *slog.Logger) (net.Listener, error)` — prepares the directory (create `0750`; `Lstat` must be a real dir, not symlink; when running as root it must be owned by uid 0 and is `chown 0:id.GID` + `chmod 0750`; when not root (tests) ownership checks are skipped but mode is still tightened), removes a stale **socket** at `path` (anything else → error), `net.Listen("unix")`, `chown 0:GID` (root only; when not root `chown -1:GID`), `chmod 0660`, and wraps the listener in `peerListener`.
  - `peerListener.Accept()` loops: accept → `unix.GetsockoptUcred` via `SyscallConn().Control` → `authorizePeer`; denied → close conn, rate-limited Warn (`"helper: connection rejected"`, attrs `peer_uid`, `peer_pid`, `reason`; at most once per 10 min per peer uid, map capped at 1024 keys) and continue; accepted → return conn. Ucred read errors → close + Warn (same limiter, key uid -1).
  - Non-Linux: `Listen` returns `errors.New("helper: only supported on Linux")`.

- [ ] **Step 1: Write failing tests.** `identity_test.go`: `TestResolveIdentityNumeric` (current uid as string → that uid; gid = primary), `TestResolveIdentityRefusesRoot` (`"0"` and `"root"` → error), `TestResolveIdentityExplicitGroup`, `TestAuthorizePeer`. `listener_linux_test.go`: `TestPeerAccepted` (Identity{UID: os.Getuid()} → client dial + HTTP round trip succeeds), `TestPeerDenied` (Identity{UID: os.Getuid()+1} → client's read returns EOF/reset, server never sees the request, one Warn logged, second denied connection inside 10 min logs nothing), `TestSocketPermissions` (dir mode `0750`, socket mode `0660`, socket gid = configured gid), `TestListenRemovesStaleSocket`, `TestListenRefusesNonSocket` (regular file at path → error), `TestListenRefusesSymlinkDir`, `TestListenRefusesForeignOwnedDir` (`t.Skip` unless root). Short socket paths under `/tmp`.
- [ ] **Step 2: Run** `go test ./internal/helper/... -v` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./internal/helper/...` → PASS (on macOS only the identity tests run; the Linux tests run in CI and in Task 8's container — note that in the task report).
- [ ] **Step 5: Commit** `feat(agent): helper Unix socket with SO_PEERCRED authorization`.

### Task 5: Helper HTTP handler, CLI, `main.go` dispatch, archtest

**Files:**
- Create: `server-agent/internal/helper/server.go`, `server_test.go`, `server-agent/internal/helper/cli.go`, `cli_test.go`
- Modify: `server-agent/main.go` (dispatch next to `hostpower`), `server-agent/main_test.go` if it lists modes, `server-agent/internal/archtest/arch_test.go`

**Interfaces:**
- Consumes: Task 1 `ReadSMBIOSMemoryModules`, `DefaultDMITablePath`; Task 2 `ReadTopLevelRAPLDomains`, `HelperRAPL`, `HelperMemoryModules`, path constants, `HelperDefaultSocket`; Task 4 `ResolveIdentity`, `Listen`.
- Produces: `func Main(args []string, stdout, stderr io.Writer) int`; `type Config struct { Socket, AgentUser, AgentGroup, PowercapRoot, DMITable string; Verbose bool }`; `func ParseArgs(args []string) (Config, error)`; `func newHandler(rapl func() []collector.HelperRAPLDomain, modules []sample.MemoryModule, now func() time.Time, log *slog.Logger) http.Handler`.

Behaviour: flags per spec §4.1 (defaults: socket `HelperDefaultSocket`, powercap `/sys/class/powercap`, dmi `DefaultDMITablePath`); missing `-agent-user` → exit 2 with usage error; positional args → error. Startup: resolve identity (error → exit 2), read modules once (error → Warn `"helper: DMI table not readable; serving no memory modules"`, empty list), warn when no RAPL zone is readable, `Listen`, serve with the same timeouts and header limit as `hostpower.NewServer` (own constructor; keep-alives may stay on), shut down on SIGINT/SIGTERM and remove the socket. Handler: only the two paths (else 404), only GET (else 405 + `Allow: GET`), token bucket 20/s burst 40 → 429, JSON + `Cache-Control: no-store`; RAPL cache: if last read older than 250 ms (or never) call `rapl()`, else serve cached; modules: always the startup slice (`[]` not `null`).

Archtest: add `"internal/helper": {"internal/collector", "internal/sample"}` to `allowedDeps`, `internal/helper` to the main package's list, and a subtest mirroring the hostpower one: `internal/helper` must not import agent, client, config, proxy, runtime, trust or hostpower.

- [ ] **Step 1: Write failing tests.** `TestHelperRAPLCache` (fake `now`; 5 requests within 250 ms → `rapl` called once; after 251 ms → called again), `TestHelperMemoryModulesEndpoint` (body `{"modules":[…]}`; empty → `{"modules":[]}`), `TestHelperMethodAndPath` (POST → 405 with `Allow: GET`; `/v1/energy` → 404), `TestHelperRateLimit` (41 rapid requests → at least one 429), `TestParseArgs` (defaults, missing agent user, positional arg), `TestMainRefusesRoot` (`-agent-user 0` → exit 2), archtest subtest.
- [ ] **Step 2: Run** `go test ./internal/helper/... ./internal/archtest/... -v` → FAIL.
- [ ] **Step 3: Implement**; in `main.go` add `if len(os.Args) > 1 && os.Args[1] == "helper" { os.Exit(helper.Main(os.Args[2:], os.Stdout, os.Stderr)) }` beside the hostpower dispatch.
- [ ] **Step 4: Run** `go test -race ./...` (module) → PASS.
- [ ] **Step 5: Commit** `feat(agent): server-agent helper mode serving RAPL and memory modules`.

## Phase 3 — agent wiring

### Task 6: `helper_socket` option, power and inventory wiring, late-helper retry

**Files:**
- Modify: `server-agent/internal/config/config.go`, `config_test.go`; `server-agent/main.go` (PowerConfig, startup log line, hardware options); `server-agent/internal/collector/hwinfo.go`, `hwinfo_linux.go`, `hwinfo_darwin.go`, `hwinfo_windows.go`, `hwinfo_other.go`, `hwinfo_test.go`; `server-agent/internal/agent/agent.go` (+ its tests); `agent.Version` bump
- Create: `server-agent/internal/agent/memory_retry_test.go` (or extend an existing hardware test file)

**Interfaces:**
- Consumes: Task 2 `NewHelperHTTPClient`, `FetchHelperMemoryModules`, `HelperDefaultSocket`; Task 3 `PowerConfig.HelperSocket`, `helperEnabled`.
- Produces: `config.Config.HelperSocket string` (resolved: Linux default `collector.HelperDefaultSocket` unless set; `off` in any source → `""`; non-Linux default `""`); warnings `"helper_socket must be an absolute path; the helper source stays off"` (then treated as `""`) and `"helper_socket is set but the helper is only used on Linux; ignored"`; `type collector.HardwareOptions struct { HelperSocket string }`; `func CollectHardware(ctx context.Context, gpuCollectors []GPUCollector, agentVersion string, opts HardwareOptions) sample.SystemReport`; `platformHardware(ctx, opts)` on every OS; `func HelperMemoryModulesNeeded(r sample.SystemReport, opts HardwareOptions) bool` (Linux, not root, socket set, report has no modules).

Behaviour: Linux non-root `linuxMemoryModules(ctx, opts)` fetches from the helper within `hwCollectTimeout` (failure → nil). Agent: after the initial `collectHardware`, if `HelperMemoryModulesNeeded`, a retry ticker (30 s) in the run loop fetches modules; on the first success it sets `a.hw.Memory.Modules`, stops the ticker and calls `sendSystemReport`; failures at Debug. (Deviation from spec's "system-report cadence": that cadence defaults to 30 min, too slow for a helper that starts seconds late — 30 s instead; record in the ADR.) `main.go` passes `HelperSocket` to `PowerConfig` and the agent, and logs `helper_socket` in the startup config line beside `hostpower_url`. Config key is added to the file-config struct (`json:"helper_socket"`), flag `-helper-socket`, env `OP_AGENT_HELPER_SOCKET`.

- [ ] **Step 1: Write failing tests.** Config: `TestHelperSocketDefaultLinux` (runtime-gated: unset → default path), `TestHelperSocketOff` (`off` via env, flag and file → `""`), `TestHelperSocketRelativeWarns`, `TestHelperSocketNonLinuxWarns` (inject GOOS if the resolver allows; otherwise build-tag the test). Collector: `TestHelperMemoryModulesNeeded` (table), `TestLinuxMemoryModulesFromHelper` (Linux-tagged, euid override, fake Unix server). Agent: `TestMemoryRetryFillsModulesAndResends` (fake fetch fails twice then succeeds → modules set, one extra system report sent, ticker stopped), `TestMemoryRetryStopsQuietlyWithoutHelper` (not needed → no ticker, no fetch).
- [ ] **Step 2: Run** `go test ./internal/config/... ./internal/collector/... ./internal/agent/... -run 'Helper|MemoryRetry' -v` → FAIL.
- [ ] **Step 3: Implement**; inject the fetch as a field on the agent (default `collector.FetchHelperMemoryModules` with `NewHelperHTTPClient(cfg.HelperSocket)`) so the test needs no socket. Merge `origin/main`, then bump `agent.Version` (minor) and update any test pinning it.
- [ ] **Step 4: Run** `go test -race ./...` in `server-agent`, `make fmt`, `make lint-go` → PASS.
- [ ] **Step 5: Commit** `feat(agent): helper_socket option; unprivileged agent reads watts and DIMMs from the helper`.

## Phase 4 — docs and verification

### Task 7: README, architecture docs, ADR-117

**Files:**
- Modify: `server-agent/README.md` (config table row for `OP_AGENT_HELPER_SOCKET`; "Hardware inventory" — Linux DIMMs via the helper, no `dmidecode` dependency; "Power metrics" Linux item — helper instead of "run as root"; new section "Privileged helper (Linux)" with both unit files from spec §4.5, the caveat list, flags table, a hardware-acceptance checklist, unverified-capabilities note); `docs/architecture/09-architecture-decisions.md` (ADR-117: context, decision, threat model incl. 250 ms cache, alternatives rejected — `hostpower_url=unix://`, one merged root daemon, running `dmidecode`, per-pid checks — consequences, hardware acceptance **pending**, 30 s retry deviation); `docs/architecture/reference/config-env.md`; `docs/architecture/cross-cutting/configuration.md`; `docs/architecture/07-deployment-view.md`; `docs/architecture/cross-cutting/architecture-tests.md`; `docs/architecture/11-risks-and-technical-debt.md` (privileged-sources row → mitigated by the helper on Linux; new row: same-uid runtimes read coarse RAPL; new row: hardware acceptance pending); `docs/architecture/cross-cutting/telemetry-usage-observability.md` (Linux power/DIMM sources); `docs/architecture/05-building-block-view.md` if it lists server-agent packages.

- [ ] **Step 1:** Write the docs; every claim matches the code as merged in Tasks 1-6 (flag names, defaults, messages).
- [ ] **Step 2: Run** `make lint-docs` → PASS.
- [ ] **Step 3: Commit** `docs: privileged agent helper (README, ADR-117, architecture)`.

### Task 8: Linux container verification (needs the operator's OK to start Docker Desktop)

Not a subagent task; the controller runs it. In a `golang:1.26` container (as root): build the agent; create a synthetic powercap tree with `package-0` and `psys` whose `energy_uj` a background loop increments; copy the container's own `/sys/firmware/dmi/tables/DMI` if readable (else a Task 1 fixture); create user `op-agent` (uid 1000); run `server-agent helper -agent-user op-agent -powercap-root <tree> -dmi-table <file>`; run the Linux-tagged tests as root and as uid 1000 (`go test -race ./internal/helper/... ./internal/collector/...`); `curl --unix-socket` as uid 1000 → 200 JSON, as uid 1001 → connection closed; run the agent as uid 1000 against a stub gateway or with `-v` and check `power_sources` lists `helper` and the debug sample carries CPU/system watts. If the DMI table was real and `dmidecode` is installable, compare its output with the parser's and add the table as a fixture. Record results in the PR body.

### Task 9: Gates, fold-in, PR

- [ ] `make fmt`, `make lint-go`, `cd server-agent && go test -race ./...`, `make lint-docs`, local Sonar gate (`make sonar-scan` then findings + branch-findings in a worktree), all green.
- [ ] Fold durable content into `docs/architecture/` (Task 7), delete `docs/superpowers/`, verify `git diff --name-only origin/main...HEAD` lists neither `docs/superpowers` nor `docs/implementation-status.md`.
- [ ] Push over SSH, open the PR (English, substantive squash commit body), enable Auto-fix.
