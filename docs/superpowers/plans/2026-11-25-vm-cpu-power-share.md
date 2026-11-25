# VM CPU Power Share Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Linux guest on a Proxmox VE host reports its share of the host's CPU package power through the existing `cpu_power_w` field, measured on the host and published per VM as a synthetic RAPL powercap tree over virtiofs.

**Architecture:** A new `server-agent hostpower` mode (root, on the PVE host) reads RAPL package energy and per-VM cgroup CPU time every 250 ms, attributes the energy delta by active CPU time, and writes `<out>/<vmid>/intel-rapl:0/{name,energy_uj,max_energy_range_uj}` atomically. The guest agent gains an `OP_AGENT_POWERCAP_ROOT` option that points the unchanged RAPL collector at the mounted share. Gateway, wire format and portal stay untouched.

**Tech Stack:** Go (server-agent module `op-ai-server-agent`, stdlib only), Linux sysfs/procfs/cgroup v2, PVE 9.2 virtiofs. Tests: `go test` with synthetic sysfs/proc/cgroup trees under `t.TempDir()` (style of `internal/collector/rapl_test.go`).

**Spec:** `docs/superpowers/specs/2026-11-25-vm-cpu-power-share-design.md`

## Global Constraints

- Module path `op-ai-server-agent`; every new `.go` file starts with the two SPDX/copyright comment lines used throughout `server-agent/`.
- CGO-free, stdlib only; no new dependencies.
- Host write cadence default `250ms`, floor `100ms` (spec §4.2 cadence).
- Synthetic tree: `name` = `package-0`, `max_energy_range_uj` = `9223372036854775807`, `energy_uj` = uint64 microjoules; writes via temp file + rename in the same directory.
- Host busy time from `/proc/stat` line `cpu`: `user + nice + system + irq + softirq + steal` in USER_HZ jiffies (100 Hz, so ×10000 → µs); guest/guest_nice are NOT added. VM time from `/sys/fs/cgroup/qemu.slice/<vmid>.scope/cpu.stat` `usage_usec`.
- Shares are clamped to [0,1] and normalised so their sum is ≤ 1; a zero host busy delta yields all-zero shares.
- The `energy_uj` file is the persistent counter: on start, read it back and continue.
- Agent `Version` bumps once on this branch: `0.20.1` → `0.21.0` (MINOR: new mode + new option).
- Repo-facing text in English; no operator host names, VM names or IPs in docs (use `<vmid>`, `pve-host`, generic paths).
- `golangci-lint fmt --diff` and `golangci-lint run` clean in `server-agent/` (CI gates: gofumpt, gocritic). Run via the repo-root `make fmt` (golangci-lint fmt, check with `git diff --exit-code` afterwards) and `make lint-go`.
- `docs/superpowers/**` are branch-local; removed before the PR (not part of this plan's tasks; the finishing flow does it).

## Review Focus

1. **Corrupt or empty `energy_uj` on host start** (e.g. a crash mid-write left a zero-byte file): the service must start that VM's counter at 0 and keep running, never panic or refuse to start. → test in Task 3.
2. **A VM stopping mid-tick** (its `cpu.stat` vanishes between discovery and read): that VM is skipped for the tick, all other VMs still get their share. → test in Task 5.
3. **Zero host busy delta** (fully idle host between two ticks): shares are all 0, no NaN/Inf, counters unchanged. → test in Task 2.
4. **Guest option set but share not mounted yet** (agent starts before the virtiofs mount): `Available()` false, `cpu_power_w` nil, no error, and the reading appears once the directory shows up because the agent calls `Collect` every tick. → test in Task 7.
5. **Sum of VM deltas exceeding the host delta** (sampling skew between `/proc/stat` and cgroup reads): normalised so the VMs never get more than the package in total. → test in Task 2.

---

### Task 1: Exported package-energy delta reader in `collector`

**Files:**
- Create: `server-agent/internal/collector/rapl_package.go`
- Test: `server-agent/internal/collector/rapl_package_test.go`

**Interfaces:**
- Consumes: `readSysfsString`, `readSysfsUint`, `raplDeltaJoules` (unexported helpers in `rapl.go`).
- Produces: `type PackageEnergyReader struct` with `func NewPackageEnergyReader(powercapRoot string) *PackageEnergyReader` and `func (r *PackageEnergyReader) DeltaJoules() (joules float64, ok bool)`. Sums every `intel-rapl:*` domain whose `name` starts with `package`; `ok` is false on the first call (no previous reading) and when no package domain is readable. Wraparound-corrected per domain via `max_energy_range_uj`. Stateful, not goroutine-safe (single caller).

- [ ] **Step 1: Write the failing tests**

```go
func TestPackageEnergyReaderFirstCallNotOK(t *testing.T) // one domain energy 1_000_000 → DeltaJoules() ok==false
func TestPackageEnergyReaderSumsPackagesSkipsSubdomains(t *testing.T)
// domains: intel-rapl:0 name=package-0 1_000_000→4_000_000 (3 J); intel-rapl:1 name=package-1 500_000→2_500_000 (2 J);
// intel-rapl:0:0 name=core 0→9_000_000 (ignored). Second call: joules==5.0 (±1e-9), ok==true.
func TestPackageEnergyReaderWraparound(t *testing.T)
// max 262143328850, prev 262143328000, cur 1150 → 0.002 J (±1e-9).
func TestPackageEnergyReaderNoDomainsNotOK(t *testing.T) // empty root → both calls ok==false
```

Use the existing `writeSysfs` helper from `rapl_test.go` (same package).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/collector -run PackageEnergyReader -v`
Expected: FAIL, `undefined: NewPackageEnergyReader`.

- [ ] **Step 3: Implement `rapl_package.go`**

Keep per-domain `raplReading` state keyed by domain dir (same struct as `raplCollector`). Domain filtering identical to `raplCollector.Collect` (prefix `package`), but do NOT touch `rapl.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd server-agent && go test ./internal/collector -run PackageEnergyReader -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/collector/rapl_package.go server-agent/internal/collector/rapl_package_test.go
git commit -m "feat(agent): exported RAPL package-energy delta reader for the hostpower mode"
```

---

### Task 2: Share arithmetic and parsers (`hostpower` package, pure functions)

**Files:**
- Create: `server-agent/internal/hostpower/share.go`
- Test: `server-agent/internal/hostpower/share_test.go`

**Interfaces:**
- Produces:
  - `func ParseProcStatBusyUsec(procStat string) (uint64, bool)` — takes the whole `/proc/stat` content, finds the line starting with `cpu ` (aggregate), returns `(user+nice+system+irq+softirq+steal) * 10000`. Fields (1-based after the label): user=1, nice=2, system=3, idle=4, iowait=5, irq=6, softirq=7, steal=8, guest=9, guest_nice=10. `false` when the line or a needed field is missing.
  - `func ParseCgroupUsageUsec(cpuStat string) (uint64, bool)` — value of the `usage_usec` key in a cgroup v2 `cpu.stat`.
  - `func Shares(hostBusyDeltaUsec uint64, vmDeltaUsec map[string]uint64) map[string]float64` — share per key, each clamped to [0,1]; if the sum of clamped shares exceeds 1, every share is divided by that sum; `hostBusyDeltaUsec == 0` → every share 0. Returns a key for every input VM.
  - `func AttributeUJ(deltaJoules float64, share float64) uint64` — `uint64(math.Round(deltaJoules * share * 1e6))`, 0 for non-finite or negative input.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseProcStatBusyUsec(t *testing.T)
// "cpu  100 5 50 1000 20 3 2 10 7 1\ncpu0 ..." → (100+5+50+3+2+10)*10000 == 1_700_000, ok
// no cpu line → ok==false
func TestParseCgroupUsageUsec(t *testing.T) // "usage_usec 123456\nuser_usec 100\n" → 123456; missing → false
func TestSharesPlain(t *testing.T)          // host 1_000_000, vm a 250_000, b 500_000 → a 0.25, b 0.5
func TestSharesNormalisedWhenSumExceedsHost(t *testing.T) // host 1_000_000, a 800_000, b 600_000 → a 4/7, b 3/7 (±1e-9), sum ≤ 1
func TestSharesZeroHostDelta(t *testing.T)  // host 0, a 10 → a 0 (and not NaN)
func TestAttributeUJ(t *testing.T)           // 2.5 J × 0.4 → 1_000_000; NaN → 0; -1 → 0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/hostpower -v`
Expected: FAIL, package has no non-test files / undefined symbols.

- [ ] **Step 3: Implement `share.go`**

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd server-agent && go test ./internal/hostpower -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/hostpower/share.go server-agent/internal/hostpower/share_test.go
git commit -m "feat(agent): hostpower share arithmetic and /proc/stat, cgroup cpu.stat parsers"
```

---

### Task 3: Synthetic powercap tree writer with persistent counter

**Files:**
- Create: `server-agent/internal/hostpower/tree.go`
- Test: `server-agent/internal/hostpower/tree_test.go`

**Interfaces:**
- Produces:
  - `const DomainDir = "intel-rapl:0"`, `const MaxEnergyRangeUJ uint64 = 9223372036854775807`
  - `func WriteTree(vmDir string, energyUJ uint64) error` — ensures `vmDir/intel-rapl:0/` exists (0o755), writes `name` (`package-0\n`) and `max_energy_range_uj` only if absent or different, writes `energy_uj` as `"%d\n"` via a temp file in the same directory + `os.Rename`. File mode 0o644.
  - `func ReadCounter(vmDir string) uint64` — parses `vmDir/intel-rapl:0/energy_uj`; 0 when absent, empty, or unparsable.

- [ ] **Step 1: Write the failing tests**

```go
func TestWriteTreeCreatesAllThreeFiles(t *testing.T)
// WriteTree(dir, 42) → name=="package-0\n", max_energy_range_uj=="9223372036854775807\n", energy_uj=="42\n"
func TestWriteTreeLeavesNoTempFiles(t *testing.T) // after two writes, only the three files exist in intel-rapl:0/
func TestReadCounterRoundTrip(t *testing.T)       // WriteTree(dir, 7_000_000); ReadCounter(dir)==7_000_000
func TestReadCounterCorruptOrMissingIsZero(t *testing.T)
// missing dir → 0; empty file → 0; "garbage\n" → 0  (Review Focus 1)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/hostpower -run 'Tree|Counter' -v`
Expected: FAIL, undefined `WriteTree`/`ReadCounter`.

- [ ] **Step 3: Implement `tree.go`**

Temp file: `os.CreateTemp(domainDir, ".energy_uj.*")`, write, `Sync`, `Close`, `Rename`; remove the temp on any error path.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd server-agent && go test ./internal/hostpower -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/hostpower/tree.go server-agent/internal/hostpower/tree_test.go
git commit -m "feat(agent): hostpower synthetic powercap tree writer with read-back of the counter"
```

---

### Task 4: VM discovery and per-VM CPU time from cgroup v2

**Files:**
- Create: `server-agent/internal/hostpower/cgroup.go`
- Test: `server-agent/internal/hostpower/cgroup_test.go`

**Interfaces:**
- Consumes: `ParseCgroupUsageUsec` (Task 2).
- Produces:
  - `func DiscoverVMs(cgroupRoot string) []string` — VM IDs from `cgroupRoot/qemu.slice/<vmid>.scope` directories, numeric `<vmid>` only, sorted ascending; empty slice (not nil-error) when the slice is absent.
  - `func ReadVMUsageUsec(cgroupRoot, vmid string) (uint64, bool)` — reads `cgroupRoot/qemu.slice/<vmid>.scope/cpu.stat`; `false` when unreadable.

- [ ] **Step 1: Write the failing tests**

```go
func TestDiscoverVMsSortedNumericOnly(t *testing.T)
// dirs: qemu.slice/101.scope, qemu.slice/100.scope, qemu.slice/foo.scope, qemu.slice/notascope → ["100","101"]
func TestDiscoverVMsNoSlice(t *testing.T)   // empty root → len 0
func TestReadVMUsageUsec(t *testing.T)      // cpu.stat "usage_usec 555\n" → 555, ok; missing → false
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/hostpower -run 'DiscoverVMs|ReadVMUsage' -v`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement `cgroup.go`**

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd server-agent && go test ./internal/hostpower -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/hostpower/cgroup.go server-agent/internal/hostpower/cgroup_test.go
git commit -m "feat(agent): hostpower discovers QEMU VMs and reads their cgroup CPU time"
```

---

### Task 5: The service tick

**Files:**
- Create: `server-agent/internal/hostpower/service.go`
- Test: `server-agent/internal/hostpower/service_test.go`

**Interfaces:**
- Consumes: `collector.NewPackageEnergyReader` (Task 1), `ParseProcStatBusyUsec`, `Shares`, `AttributeUJ` (Task 2), `WriteTree`, `ReadCounter` (Task 3), `DiscoverVMs`, `ReadVMUsageUsec` (Task 4).
- Produces:
  - `type Options struct { PowercapRoot, ProcStatPath, CgroupRoot, OutDir string }` — defaults applied by `New` when empty: `/sys/class/powercap`, `/proc/stat`, `/sys/fs/cgroup`, `/var/lib/op-hostpower`.
  - `func New(opts Options) *Service`
  - `func (s *Service) Tick() error` — one iteration: read host busy, read package delta, discover VMs, read each VM's usage, compute shares, add `AttributeUJ` to each VM's counter, `WriteTree`. The first tick (no previous host busy / no energy delta) only establishes baselines and writes each discovered VM's tree with its read-back counter (so directories exist for the mapping). A VM seen for the first time gets its counter from `ReadCounter(outDir/vmid)`. A VM whose `cpu.stat` read fails is skipped for this tick (its previous usage baseline is dropped so the next successful read re-baselines). Errors: writing a tree returns the error after processing all VMs (joined); an unreadable `/proc/stat` or no energy delta returns `nil` and logs at Debug.
  - `func (s *Service) Run(ctx context.Context, interval time.Duration) error` — ticker loop until ctx done; logs each `Tick` error at Warn, never exits on one.
  - `func (s *Service) Counters() map[string]uint64` — snapshot for tests/diagnostics.

- [ ] **Step 1: Write the failing tests**

Build a synthetic root per test: `powercap/intel-rapl:0/{name,energy_uj,max_energy_range_uj}`, a `proc_stat` file, `cgroup/qemu.slice/<vmid>.scope/cpu.stat`, and an `out` dir.

```go
func TestTickFirstTickBaselinesAndCreatesDirs(t *testing.T)
// VMs 100, 101; after Tick(): out/100/intel-rapl:0/energy_uj == "0\n", out/101 likewise; Counters all 0
func TestTickAttributesByActiveShare(t *testing.T)
// tick 1 baseline. Then: energy +10 J; host busy +100 jiffies (1_000_000 µs); vm100 +250_000 µs; vm101 +500_000 µs.
// tick 2 → counters 100: 2_500_000, 101: 5_000_000; files match.
func TestTickPersistsAcrossRestart(t *testing.T)
// run the two ticks above, New() a second Service on the same roots, first tick → Counters()["100"]==2_500_000
func TestTickSkipsVMWhoseCPUStatVanished(t *testing.T) // Review Focus 2
// after baseline, delete cgroup/qemu.slice/101.scope/cpu.stat, advance 100 → counter 100 grows, 101 unchanged, Tick() returns nil
func TestTickZeroHostDeltaLeavesCountersUnchanged(t *testing.T) // Review Focus 3: energy +10 J but busy +0 → counters unchanged
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/hostpower -run Tick -v`
Expected: FAIL, undefined `New`/`Service`.

- [ ] **Step 3: Implement `service.go`**

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd server-agent && go test ./internal/hostpower -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/hostpower/service.go server-agent/internal/hostpower/service_test.go
git commit -m "feat(agent): hostpower service tick attributes RAPL package energy to VMs by CPU time"
```

---

### Task 6: `server-agent hostpower` command line and wiring in `main.go`

**Files:**
- Create: `server-agent/internal/hostpower/cli.go`
- Test: `server-agent/internal/hostpower/cli_test.go`
- Modify: `server-agent/main.go:42-45` (early-argument mode block)

**Interfaces:**
- Consumes: `New`, `Options`, `(*Service).Run` (Task 5).
- Produces:
  - `const DefaultInterval = 250 * time.Millisecond`, `const MinInterval = 100 * time.Millisecond`
  - `func ParseArgs(args []string) (Options, time.Duration, bool, error)` — flags `-out` (default `/var/lib/op-hostpower`), `-interval` (default `250ms`), `-v`; returns `(opts, interval, verbose, err)`; `-interval` below `MinInterval` → error `hostpower: -interval must be at least 100ms`. Uses `flag.NewFlagSet("hostpower", flag.ContinueOnError)`.
  - `func Main(args []string, stderr io.Writer) int` — parses, sets slog default (Debug when `-v`), creates `OutDir` (0o755), logs one Info line with out dir + interval, then one Warn each (once, at startup) when `PowercapRoot` has no `intel-rapl:*` domain (`hostpower: no RAPL package domain under <root>; is intel_rapl_common loaded and are we root?`) or `CgroupRoot/qemu.slice` is absent (`hostpower: no qemu.slice under <root>; no running QEMU VMs or cgroup v1?`), and keeps running regardless; runs `Run` until SIGINT/SIGTERM; returns 2 on parse/mkdir error, 0 on clean shutdown.
  - `main.go`: before `config.Load`, `if len(os.Args) > 1 && os.Args[1] == "hostpower" { os.Exit(hostpower.Main(os.Args[2:], os.Stderr)) }`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseArgsDefaults(t *testing.T)        // [] → OutDir "/var/lib/op-hostpower", 250ms, verbose false
func TestParseArgsCustom(t *testing.T)          // ["-out","/tmp/x","-interval","500ms","-v"] → as given
func TestParseArgsIntervalFloor(t *testing.T)   // ["-interval","50ms"] → err containing "at least 100ms"
func TestMainRejectsBadInterval(t *testing.T)   // Main(["-interval","1ms"], &buf) == 2, buf contains "at least 100ms"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/hostpower -run 'ParseArgs|MainRejects' -v`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement `cli.go` and the `main.go` dispatch**

- [ ] **Step 4: Run tests and build**

Run: `cd server-agent && go test ./internal/hostpower -v && go build ./... && go run . hostpower -interval 1ms; echo "exit $?"`
Expected: tests PASS; build ok; the run prints the floor error and `exit 2`.

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/hostpower/cli.go server-agent/internal/hostpower/cli_test.go server-agent/main.go
git commit -m "feat(agent): server-agent hostpower mode publishing per-VM CPU power shares"
```

---

### Task 7: Guest option `OP_AGENT_POWERCAP_ROOT`

**Files:**
- Modify: `server-agent/internal/config/config.go` (Config struct near line 148, file struct line 447, flags line 499, resolution line 574)
- Modify: `server-agent/internal/collector/collector.go:117-125` and `:215-218` (`DetectPowerCollector`, `DetectPowerAndTempCollectors`)
- Modify: `server-agent/internal/collector/power_linux.go`, `power_darwin.go`, `power_other.go` (`newNativePowerCollector` signature)
- Modify: `server-agent/main.go:88` (pass the option) and the startup diagnostic near `:349` (log `powercap_root`)
- Test: `server-agent/internal/config/config_test.go`, `server-agent/internal/collector/power_test.go` (or a new `power_root_test.go` in package `collector`)

**Interfaces:**
- Produces:
  - `Config.PowercapRoot string` (trimmed; empty = platform default), config file key `powercap_root`, flag `-powercap-root`, env `OP_AGENT_POWERCAP_ROOT`. Flag help: `optional powercap sysfs root for CPU power, e.g. a Proxmox hostpower virtiofs mount (env OP_AGENT_POWERCAP_ROOT / config powercap_root); Linux only, ignored elsewhere`.
  - `func newNativePowerCollector(powercapRoot string) PowerCollector` on all three build files; Linux uses `powercapRoot` when non-empty else `defaultPowercapRoot`, hwmon root unchanged; darwin/other ignore it.
  - `func DetectPowerCollector(lhmURL, powercapRoot string) PowerCollector`, `func DetectPowerAndTempCollectors(lhmURL, powercapRoot string) (PowerCollector, TempCollector)`. Callers to update: `server-agent/main.go:88` (pass `cfg.PowercapRoot`), and the tests `internal/collector/power_logging_test.go:65,69`, `internal/collector/lhm_source_test.go:75,101`, `internal/collector/power_test.go:75,86` (pass `""` as the new second argument).

- [ ] **Step 1: Write the failing tests**

```go
// config_test.go, mirror TestLHMURLResolution:
func TestPowercapRootResolution(t *testing.T)
// default "" ; -powercap-root=/run/op-hostpower ; env OP_AGENT_POWERCAP_ROOT=/mnt/hp ; config file {"powercap_root":"/cfg"} ; flag beats env beats file

// collector package (build-tag-free):
func TestRAPLCollectorCustomRootAppearsLater(t *testing.T) // Review Focus 4
// root := filepath.Join(t.TempDir(), "notyet"); c := newRAPLCollector(root, nohwmon)
// Available()==false; Collect → cpu nil, err nil. Then create root/intel-rapl:0 with energy 1_000_000, Collect (baseline), +15 J at +1 s → ~15 W.
```

On Linux only (`power_linux_test.go` with `//go:build linux`): `TestNativePowerCollectorUsesConfiguredRoot` — `newNativePowerCollector(customRoot)` is a `*raplCollector` whose `powercapRoot == customRoot`; `newNativePowerCollector("")` has `defaultPowercapRoot`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd server-agent && go test ./internal/config -run PowercapRoot -v; go test ./internal/collector -run 'CustomRoot|ConfiguredRoot' -v`
Expected: FAIL / compile errors on the new names.

- [ ] **Step 3: Implement the option, the signature change and the wiring**

- [ ] **Step 4: Run the whole agent test suite and cross-compile**

Run: `cd server-agent && go test ./... && GOOS=darwin GOARCH=arm64 go build ./... && GOOS=windows GOARCH=amd64 go build ./...`
Expected: all PASS; all three builds succeed.

- [ ] **Step 5: Commit**

```bash
git add server-agent/internal/config server-agent/internal/collector server-agent/main.go
git commit -m "feat(agent): OP_AGENT_POWERCAP_ROOT points the RAPL collector at a host-provided powercap tree"
```

---

### Task 8: Version bump and durable documentation

**Files:**
- Modify: `server-agent/internal/agent/agent.go:436-444` (`Version` and its history comment)
- Modify: `server-agent/README.md` (§ "Power metrics", after the Linux bullet; and the configuration table near line 52)
- Modify: `docs/architecture/reference/config-env.md` (agent table, after `OP_AGENT_LHM_URL`)
- Modify: `docs/architecture/09-architecture-decisions.md` (append `ADR-106`)
- Modify: `docs/architecture/07-deployment-view.md` (§7.5 Server-Agent deployment)
- Test: existing version/feature tests in `server-agent/internal/agent` (run the package).

- [ ] **Step 1: Bump `Version` to `"0.21.0"`** with a comment paragraph in the existing style: `0.20.1 -> 0.21.0` is the single bump for the VM CPU power share (MINOR: new `hostpower` mode and the `powercap_root` option; the ONE bump for the whole branch).

- [ ] **Step 2: Agent README**

Add a row for `OP_AGENT_POWERCAP_ROOT` / `-powercap-root` / `powercap_root` (default: `/sys/class/powercap`, Linux only) to the configuration table. In "Power metrics" add a subsection **Linux guest on a Proxmox VE host (virtiofs-attributed share)** covering, in this order: why RAPL is absent in a guest; the host service (`apt install virtiofsd`, download the Linux agent binary, systemd unit `op-hostpower.service` with `ExecStart=/usr/local/bin/server-agent hostpower -out /var/lib/op-hostpower`, `Restart=always`, runs as root; the `-interval` default and the "at most a quarter of the guest's `OP_AGENT_INTERVAL`" rule); per VM the directory mapping (`pvesh create /cluster/mapping/dir --id hostpower-<vmid> --map node=<node>,path=/var/lib/op-hostpower/<vmid>`) and attachment (`qm set <vmid> --virtiofs0 dirid=hostpower-<vmid>`), noting the service must have run once so the directory exists; the guest mount (`hostpower-<vmid> /run/op-hostpower virtiofs ro,nofail 0 0`) and the agent option; the attribution policy in two sentences (active CPU time, idle floor included, sum of shares ≤ package); the PVE limitations (no live migration, RAM snapshots, hibernate or memory hotplug for a VM with a virtiofs device); what stays `null` (system watts) and what is out of scope (Windows guests, LXC).

- [ ] **Step 3: `config-env.md`** — add `| OP_AGENT_POWERCAP_ROOT | string | Optional powercap sysfs root the Linux RAPL collector reads instead of /sys/class/powercap (a Proxmox hostpower virtiofs mount); ignored on other OSes | `` (platform default) |` in the agent table.

- [ ] **Step 4: ADR-106** — title `ADR-106 — VM CPU power via a host-attributed synthetic RAPL tree over virtiofs`. Decision (transport, attribution by active CPU time with idle floor, 250 ms host cadence vs 1 s guest cadence and the Tier-1 2 s gap rule, counter persisted in `energy_uj`, no gateway contact), Consequences (PVE virtiofs limitations accepted; host service is root; sum of shares ≤ package; alternatives rejected: HTTP exporter with SMBIOS-UUID identification, vsock). Same heading/format as ADR-105.

- [ ] **Step 5: Deployment view §7.5** — one paragraph: optional `server-agent hostpower` systemd service on a hypervisor host, what it reads and writes, that it is not an enrolled agent.

- [ ] **Step 6: Verify**

Run: `cd server-agent && go test ./internal/agent && grep -n "0.21.0" internal/agent/agent.go && grep -c "OP_AGENT_POWERCAP_ROOT" README.md ../docs/architecture/reference/config-env.md`
Expected: tests PASS; version line present; both docs mention the option.

- [ ] **Step 7: Commit**

```bash
git add server-agent/internal/agent/agent.go server-agent/README.md docs/architecture
git commit -m "docs(agent): document the Proxmox hostpower mode and OP_AGENT_POWERCAP_ROOT; bump agent to 0.21.0"
```

---

### Task 9: Lint, full verification and manual acceptance checklist

**Files:**
- No code changes expected; fixes only if lint reports.

- [ ] **Step 1: Lint exactly as CI does**

Run from the repo root: `make fmt && git diff --exit-code && make lint-go`
Expected: formatting leaves no diff; lint reports no findings. Fix and amend into a `style(agent): ...` commit if anything shows.

- [ ] **Step 2: Full agent tests with race detector**

Run: `cd server-agent && go test -race ./...`
Expected: all `ok`.

- [ ] **Step 3: Record the manual acceptance steps** (not executable here; goes into the PR description, performed by the operator on the PVE 9.2 host):

1. Host: install `virtiofsd`, install the binary, enable `op-hostpower.service`, confirm `/var/lib/op-hostpower/<vmid>/intel-rapl:0/energy_uj` grows.
2. Create the mapping and attach `virtiofs0` to one guest; mount in the guest; set `OP_AGENT_POWERCAP_ROOT=/run/op-hostpower`; restart the agent; confirm the startup diagnostic lists `rapl` under `power_sources`.
3. Portal "Leistung" view shows a CPU-watt chart for the guest within a few samples; with the guest loaded, the value rises; idle, it falls toward 0 W or the idle floor share.
4. With two guests attached, the sum of their `cpu_power_w` stays at or below the host's package watts read by hand from `/sys/class/powercap/intel-rapl:0/energy_uj` deltas.
5. Stop and start `op-hostpower.service`: the guest chart shows no spike (counter persisted).

- [ ] **Step 4: Hand over** to the finishing flow (`superpowers:finishing-a-development-branch`): sonar gate, remove `docs/superpowers/`, push over SSH, PR in English, enable Auto-fix.
