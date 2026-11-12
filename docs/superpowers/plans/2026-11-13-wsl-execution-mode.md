# WSL2 Execution Mode (#381, Phases 1+2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A runtime spec with `execution.mode = "wsl"` runs its Linux model server inside WSL2 on a Windows agent with the native lifecycle, its RAM measured from its own cgroup, and its VRAM attributed from the WSL VM's worker process, so admission never under-charges.

**Architecture:** The agent wraps the operator's Linux binary in `wsl.exe -d <distro> -u <user> --cd <dir> --exec systemd-run --user --scope --unit=<unit> -- <binary> <args…>`, controls it through `systemctl --user kill` on that scope, reads RAM from the scope's cgroup through one fixed `wsl.exe --exec sh -c` sampling call per beat, and reads VRAM by pointing the existing PDH measurer at the `vmwp` PIDs, attributing per-workload start deltas under a one-start-at-a-time interlock and charging the rest of the VM's VRAM as unattributed. The gateway stores `execution_mode`/`wsl_distro`, pushes `execution` only to agents that declare `runtime_exec_wsl`, and the portal edits it.

**Tech Stack:** Go 1.26 (server-agent, own module), Go 1.25 backend (`gateway/backend`), SQLite/Postgres/memory stores, React/TypeScript/Vite portal.

**Spec:** `docs/superpowers/specs/2026-11-12-issue-wsl2-execution-mode.md` (= issue #381, including the Phase 0 measurements). Read it before any task.

## Global Constraints

- Work on branch `claude/issue-wsl-docker-runspec-82f6ca`. Never commit to or merge into `main`. No attribution lines in commits. All repo text in English. No LAN host names, user names or GPU UUIDs in repo text.
- Agent version: one MINOR bump for the branch, `const Version` in `server-agent/internal/agent/agent.go` from `0.18.2` to `0.19.0`. New feature flag `runtime_exec_wsl` with `Since: "0.19.0"`.
- `internal/runtime` may import only `internal/gwapi` (`internal/archtest/arch_test.go:86`). Anything Windows-process-specific (finding `vmwp` PIDs) lives in `internal/collector` and is injected as a func, like the measurers.
- Windows-specific code: logic untagged and unit-tested on Linux; only syscalls behind `//go:build windows`. CI runs no Windows job, so every task touching the agent ends with `GOOS=windows go build ./... && GOOS=windows go vet ./...` from `server-agent/`.
- **No shell with gateway input.** Always `wsl.exe … --exec`, never `--`. The only `sh -c` is the agent's own fixed sampling script, with unit names passed as positional arguments, never interpolated.
- **No gateway-chosen env or argv ever reaches a root process.** The agent never passes `-u root`; every `wsl.exe` call uses `-u <runtime_wsl_user>`, and `root`/UID 0 is refused as that user.
- Store changes: append-only migration **109**, all three drivers, Postgres leg verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Portal: German and English i18n keys added together; run `npm run format:check` before pushing.
- Go lint: `golangci-lint fmt --diff` and `golangci-lint run` per Go module (gofumpt, gocritic).
- No new third-party dependencies (gopsutil and `golang.org/x/sys` are already present; check `server-agent/go.mod`).
- Values pinned by the spec: unit prefix `opagent-`; `WSL_UTF8=1`; `PYTHONUNBUFFERED=1`; `CUDA_DEVICE_ORDER=PCI_BUS_ID` whenever the agent sets `CUDA_VISIBLE_DEVICES`; `XDG_RUNTIME_DIR=/run/user/<uid>`; RAM figure `memory.current − inactive_file`; WSL VM RAM reserve default `1024` MB (`OP_AGENT_WSL_RAM_RESERVE_MB`); default `runtime_wsl_exe` `C:\Program Files\WSL\wsl.exe` on Windows and empty elsewhere.

## Review Focus

1. **A spec env key that collides with an agent-owned WSL name** (`WSLENV`, `WSL_UTF8`, `XDG_RUNTIME_DIR`, any case) must be refused as `not_permitted`, never silently overridden. A spec-set `PYTHONUNBUFFERED` wins over the agent default. → Task 2 test `TestCheckSpecEnvRefusesWSLOwnedNames`, Task 3 test `TestBuildWSLLaunchSpecPythonUnbufferedWins`.
2. **A `wsl` spec on an agent where WSL is not configured** (Linux host, empty `runtime_wsl_exe`) must land in `not_permitted` with a message naming the setting, not in a start/crash loop. → Task 2 test `TestPermitWSLUnconfigured`.
3. **An agent restart while WSL workloads run** must kill the previous run's scopes and never the current run's. → Task 4 test `TestWSLOrphanSweepKillsOnlyForeignNonce`.
4. **Two WSL starts requested at the same moment** must not share one measurement window, and a crash of another WSL workload during the window must correct the delta, not shrink it. → Task 6 tests `TestWSLSecondStartWaitsForSettle`, `TestWSLExitDuringWindowCorrectsDelta`.
5. **An older agent without `runtime_exec_wsl`** must never receive a `wsl` spec (it would run the Linux path natively), and while the agent's features are unknown the document is held. → Task 7 test `TestAgentRuntimeDocWSLSpecOnlyForAnAgentThatDeclaresIt`.

---

### Task 1: Agent spec seam, parse validation, feature flag

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (Spec struct 67-186; `ParseConfig` 474-535; `withoutInvalidKinds` 586-601)
- Modify: `server-agent/internal/agent/features.go` (new entry after `rpc_cluster`, constant beside `FeatureRPCCluster` at 385)
- Modify: `server-agent/internal/agent/agent.go:418` (`Version`, plus the bump-history comment)
- Test: `server-agent/internal/runtime/types_execution_test.go` (new), existing `TestFeatureRegistry`

**Interfaces:**
- Produces:
  - `type Execution struct { Mode string \`json:"mode"\`; WSL *WSLExecution \`json:"wsl,omitempty"\` }`
  - `type WSLExecution struct { Distro string \`json:"distro"\` }`
  - `const ExecutionModeNative = "native"`, `const ExecutionModeWSL = "wsl"`
  - `Spec.Execution *Execution \`json:"execution,omitempty"\`` (last field of `Spec`)
  - `func (s Spec) IsWSL() bool` and `func (s Spec) WSLDistro() string` (`""` when not WSL)
  - `func invalidExecutionReason(spec Spec) string`
  - `agent.FeatureRuntimeExecWSL = "runtime_exec_wsl"`

- [ ] **Step 1: Write the failing tests** in `types_execution_test.go`:
  - `TestParseConfigKeepsValidWSLSpec`: a document with one spec `"execution":{"mode":"wsl","wsl":{"distro":"Ubuntu-24.04"}}` parses; `cfg.Specs[0].IsWSL()` is true and `WSLDistro()` is `"Ubuntu-24.04"`.
  - `TestParseConfigDropsInvalidExecution`: table, each case dropped (spec absent from `cfg.Specs`, no error): mode `"docker"`; mode `"wsl"` without `wsl`; distro `""`; distro `"Ubuntu 24.04"` (space); distro of 65 chars; `wsl` with `"kind":"rpc_server"` plus a valid `rpc` block; `wsl` with a `cluster` block.
  - `TestParseConfigNativeExecutionIsNative`: `"execution":{"mode":"native"}` and an absent `execution` both give `IsWSL() == false`.
  - `TestSameLaunchShapeTreatsExecutionAsLaunchShape`: two specs differing only in `WSLDistro` are not the same launch shape; differing only in `Execution == nil` vs `{Mode:"native"}` are the same launch shape.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime -run 'Execution|WSLSpec' -v` → FAIL (undefined: `Execution`).
- [ ] **Step 3: Implement.** Types and helpers in `types.go`. Distro rule: `^[A-Za-z0-9._-]{1,64}$`. `invalidExecutionReason`: nil or `""`/`"native"` → `""`; `"wsl"` → checks above (`IsRPCServer()` or `Cluster != nil` → `"wsl execution does not support rpc cluster members or heads"`); other modes → `"unknown execution mode"`. Call it from `withoutInvalidKinds` (same Warn, same drop semantics, reason in the log). In `sameLaunchShape` (`manager.go:2059`), normalise `Execution{Mode:"native"}` to nil inside `strip`, nothing else (the field stays launch-affecting).
- [ ] **Step 4: Add the feature flag** (comment in the style of the `rpc_cluster` entry: "the agent runs specs whose execution block says `wsl` inside WSL2 … The gateway sends such specs only to an agent declaring it. Since is 0.19.0: a MINOR bump.") and bump `Version` to `"0.19.0"` with a history line.
- [ ] **Step 5: Run** `go test ./internal/runtime ./internal/agent -run 'Execution|WSLSpec|FeatureRegistry|LaunchShape' -v` → PASS; then `GOOS=windows go build ./... && GOOS=windows go vet ./...` → no output.
- [ ] **Step 6: Commit** `feat(agent): wsl execution block in the runtime spec, runtime_exec_wsl feature (#381)`.

### Task 2: Agent configuration and local policy for WSL

**Files:**
- Modify: `server-agent/internal/config/config.go` (Config fields near 200-213 and 319-340; `fileConfig` 389-428; `Load` 538-550; defaults near 360-369)
- Modify: `server-agent/testdata/server-agent.config.jsonc` (golden; parity test `TestGeneratedAgentConfigCoversEverySettingTheAgentCanRead`)
- Modify: `server-agent/internal/runtime/policy_local.go` (`LocalPolicy` 344-368, `Permit` 403-479, `checkSpecEnv` 1105)
- Modify: `server-agent/main.go:160`
- Test: `server-agent/internal/config/config_test.go`, `server-agent/internal/runtime/policy_local_wsl_test.go` (new)

**Interfaces:**
- Consumes: `Spec.IsWSL()`, `Spec.WSLDistro()` (Task 1).
- Produces:
  - config: `RuntimeWSLExe string`, `RuntimeWSLAllowedDistros []string`, `RuntimeWSLAllowedBinaries []string`, `RuntimeWSLAllowedDirs []string`, `RuntimeWSLUser string`, `WSLRAMReserveMB int`; `const DefaultWSLRAMReserveMB = 1024`; `func defaultWSLExe() string`.
  - File keys `runtime_wsl_exe`, `runtime_wsl_allowed_distros`, `runtime_wsl_allowed_binaries`, `runtime_wsl_allowed_dirs`, `runtime_wsl_user`; env `OP_AGENT_RUNTIME_WSL_EXE`, `OP_AGENT_RUNTIME_WSL_ALLOWED_DISTROS`, `OP_AGENT_RUNTIME_WSL_ALLOWED_BINARIES`, `OP_AGENT_RUNTIME_WSL_ALLOWED_DIRS`, `OP_AGENT_RUNTIME_WSL_USER`, and env-only `OP_AGENT_WSL_RAM_RESERVE_MB`.
  - runtime: `type WSLPolicy struct { Exe string; AllowedDistros, AllowedBinaries, AllowedDirs []string; User string }`, field `LocalPolicy.WSL WSLPolicy`.
  - `func (p LocalPolicy) permitWSL(spec Spec) error`, `func withinPosixDir(candidate, dir string) bool`, `func effectiveWSLWorkDir(spec Spec) string`, `var wslOwnedEnvNames = []string{"WSLENV", "WSL_UTF8", "XDG_RUNTIME_DIR"}`.

- [ ] **Step 1: Write the failing tests.**
  - `TestPermitWSLUnconfigured`: `Exe == ""` → error containing `runtime_wsl_exe`.
  - `TestPermitWSLRefusesRootUser`: `User` `""`, `"root"` → error containing `runtime_wsl_user`.
  - `TestPermitWSLDistroAllowlist`: empty list → refused; `"Ubuntu-24.04"` allowed only with an exact entry; the message names `runtime_wsl_allowed_distros` and the count, never the distro names.
  - `TestPermitWSLBinaryAllowlist`: empty list → refused; `vllm` (relative), `C:\x\vllm.exe` (Windows path) → refused; `/home/ai/vllm/.venv/bin/vllm` allowed only by an exact entry; the native `AllowedBinaries` is never consulted for a WSL spec.
  - `TestPermitWSLWorkDir`: `""` → allowed; `relative` → refused; with `AllowedDirs: ["/home/ai/*"]`: `/home/ai/vllm` allowed, `/home/aix` refused, `/home/ai/../etc` refused.
  - `TestCheckSpecEnvRefusesWSLOwnedNames`: a WSL spec with env key `WSLENV`, `wsl_utf8` or `Xdg_Runtime_Dir` → error; a native spec with `XDG_RUNTIME_DIR` → accepted.
  - Config: `TestLoadRuntimeWSLSettings` (env and file forms, list splitting as `resolveStringList`), `TestWSLRAMReserveDefaultAndOverride` (default 1024; `0` accepted; bad input → default), `TestDefaultWSLExe` (non-Windows → `""`).
- [ ] **Step 2: Run** `go test ./internal/runtime ./internal/config -run 'WSL' -v` → FAIL.
- [ ] **Step 3: Implement.** `Permit` returns `p.permitWSL(spec)` first when `spec.IsWSL()` (native checks skipped). Check order and messages exactly as the tests pin them; refusal messages name the setting and the count, never paths or distro names (agent-runtime-manager §3.1). `withinPosixDir` = `path.Clean` both sides, equality or prefix up to and including `/`; reuse `allowedDirBase` for the trailing `/*`. `checkSpecEnv` refuses `wslOwnedEnvNames` (upper-cased compare) only when `spec.IsWSL()`. Config: mirror `RuntimeAllowedBinaries`/`HostRAMReserveMB`; `defaultWSLExe` returns `C:\Program Files\WSL\wsl.exe` when `runtime.GOOS == "windows"`. Wire `LocalPolicy.WSL` in `main.go:160`; add the five keys to the golden `.jsonc`.
- [ ] **Step 4: Run** the tests → PASS; then the full `go test ./internal/config ./internal/runtime`, then `GOOS=windows go build ./... && GOOS=windows go vet ./...`.
- [ ] **Step 5: Commit** `feat(agent): local policy and settings for WSL execution (#381)`.

### Task 3: WSL launch builder, env forwarding, resolved command, `${CONFIG_DIR}`

**Files:**
- Create: `server-agent/internal/runtime/wsl.go` (no build tag)
- Modify: `server-agent/internal/runtime/policy_local.go` (`resolveNonSecretToken` 906, `CONFIG_DIR` arm at 955)
- Modify: `server-agent/internal/runtime/command.go` (`resolvedCommand` 226)
- Test: `server-agent/internal/runtime/wsl_test.go` (new), `command_test.go`

**Interfaces:**
- Consumes: `WSLPolicy`, `effectiveWSLWorkDir` (Task 2); `expandedSpec` (`policy_local.go:875`).
- Produces:
  - `type wslLaunch struct { Exe string; Args []string; Env []string; Unit string; PrefixLen int }`. `PrefixLen` is the number of wrapper args before the operator's args.
  - `func wslUnitName(nonce, instanceKey string, gen uint64) string`
  - `func buildWSLLaunch(pol WSLPolicy, uid int, distro, unit, workDir, binary string, args, env []string) wslLaunch`
  - `func windowsPathToWSL(p string) (string, error)`
  - `func (ex expandedSpec) withWrapper(l wslLaunch) expandedSpec`, which prepends nil spans so `resolvedCommand` masks the operator's args at their shifted indices.

- [ ] **Step 1: Write the failing tests.**
  - `TestBuildWSLLaunchArgv`: exact `Args == ["-d","Ubuntu-24.04","-u","ai","--cd","/home/ai/vllm","--exec","systemd-run","--user","--scope","--quiet","--unit=opagent-ab12cd34-m-7","--","/home/ai/vllm/.venv/bin/vllm","serve","m","--port","41234"]` and `PrefixLen == 14`.
  - `TestBuildWSLLaunchEnv`: given env `PATH=…`, `SYSTEMROOT=…`, `HF_TOKEN=x`, `CUDA_VISIBLE_DEVICES=1`, the result contains `CUDA_DEVICE_ORDER=PCI_BUS_ID`, `PYTHONUNBUFFERED=1`, `XDG_RUNTIME_DIR=/run/user/1000`, `WSL_UTF8=1`, and exactly one `WSLENV=` whose value is `CUDA_DEVICE_ORDER/u:CUDA_VISIBLE_DEVICES/u:HF_TOKEN/u:PYTHONUNBUFFERED/u:XDG_RUNTIME_DIR/u` (sorted; no `PATH`, `SYSTEMROOT` or other base-env names, no `WSL_UTF8`).
  - `TestBuildWSLLaunchSpecPythonUnbufferedWins`: env `PYTHONUNBUFFERED=0` stays `0` and appears once.
  - `TestWSLUnitName`: `("ab12cd34","m#2",7)` → `"opagent-ab12cd34-m_2-7"`; any char outside `[A-Za-z0-9_-]` becomes `_`; the key part is truncated to 64 chars.
  - `TestWindowsPathToWSL`: `C:\a\b` → `/mnt/c/a/b`; `d:/x` → `/mnt/d/x`; `/already/posix` → unchanged; `\\server\share\x` and `relative\x` → error.
  - `TestResolvedCommandMasksSecretUnderWSLWrapper`: a WSL spec with `--api-key ${API_TOKEN}` shows the mask at the shifted arg index and the wrapper args verbatim; `Binary` is the `wsl.exe` path.
  - `TestConfigDirPlaceholderIsLinuxPathForWSL`: `${CONFIG_DIR}` in a WSL spec with `configFilesDir` `C:\agent\cfg` → `/mnt/c/agent/cfg/<spec id>`; a native spec is unchanged.
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'WSLLaunch|WSLUnit|WindowsPathToWSL|WSLWrapper|ConfigDirPlaceholder' -v` → FAIL.
- [ ] **Step 3: Implement** in `wsl.go`. Env assembly appends agent-owned entries only when the key is absent (except `WSLENV`, which the agent always owns, refused for specs in Task 2). `WSLENV` lists every non-base-env name except `WSL_UTF8`, each with `/u`, sorted, joined by `:`. The `CONFIG_DIR` arm calls `windowsPathToWSL` when `spec.IsWSL()`; an error surfaces as a placeholder error, which `startProcess`'s dry run already maps to `not_permitted`.
- [ ] **Step 4: Run** → PASS; `GOOS=windows go build ./... && GOOS=windows go vet ./...`.
- [ ] **Step 5: Commit** `feat(agent): wsl.exe launch builder with WSLENV forwarding (#381)`.

### Task 4: WSL lifecycle: start, graceful stop, kill, exit mapping, orphan sweep

**Files:**
- Create: `server-agent/internal/runtime/proc_control.go` (no build tag)
- Create: `server-agent/internal/runtime/testdata/fakewsl/main.go`
- Modify: `server-agent/internal/runtime/manager.go` (`ManagerOptions` 241-339, `NewManager` 434, `runningProc` 1151-1179, `startProcess` 4029-4188, `terminateNow` 4998-5019, `handleKillGraceExpired` 4980-4986, `onProcExited` 4446, `applyConfig` 1696)
- Modify: `server-agent/main.go` (pass `WSL` and the reserve into `ManagerOptions`)
- Test: `server-agent/internal/runtime/manager_test.go` (`TestMain` builds `fakewsl` like `stubchild`), `server-agent/internal/runtime/manager_wsl_test.go` (new)

**Interfaces:**
- Consumes: Tasks 1-3.
- Produces:
  - `type procControl interface { terminate() error; kill() error }`; `type nativeControl struct{ cmd *exec.Cmd }` (wraps `terminateGroup`/`killGroup`); `type wslControl struct{ run wslRunner; pol WSLPolicy; distro, unit string; env []string; cmd *exec.Cmd }`.
  - `type wslRunner func(ctx context.Context, exe string, args, env []string) ([]byte, error)` and `func execWSLRunner(ctx context.Context, exe string, args, env []string) ([]byte, error)` (stdout; a non-zero exit is an error carrying stderr).
  - `ManagerOptions.WSL WSLPolicy`, `ManagerOptions.WSLRAMReserveMB int`, `ManagerOptions.WSLRunner wslRunner` (nil → `execWSLRunner`).
  - `runningProc.ctl procControl`, `runningProc.wslUnit string`, `runningProc.wslDistro string`.
  - `owner.wslNonce string` (8 hex chars from `crypto/rand`, set in `NewManager`); `func (o *owner) wslUID(distro string) (int, error)` (cached per distro; runs `id -u` with a 10 s timeout).
  - `func wslLaunchFailed(exitCode int) bool` (true for `-1` and `4294967295`).

- [ ] **Step 1: Write `testdata/fakewsl/main.go`.** It parses `-d D -u U --cd DIR --exec <rest>` and appends one JSON line `{argv, env:{WSLENV,XDG_RUNTIME_DIR,WSL_UTF8,PYTHONUNBUFFERED,CUDA_DEVICE_ORDER}}` to `$FAKEWSL_LOG`. It then handles `<rest>`, with state in `$FAKEWSL_STATE`:
  - `systemd-run --user --scope --quiet --unit=U -- BIN ARGS…`: write the file `$FAKEWSL_STATE/U` containing its own pid (decimal), `chdir DIR`, `syscall.Exec(BIN, …)`, so that pid becomes the workload.
  - `systemctl --user kill --signal=SIG U.scope`: signal the recorded pid; unknown unit → exit 5.
  - `systemctl --user list-units --plain --no-legend --all opagent-*.scope`: print one `U.scope loaded active running` line per recorded unit whose pid is alive.
  - `id -u`: print `1000`.
  - `sh -c <script> sh <units…>`: print the contents of `$FAKEWSL_SAMPLE` (used in Task 5).
  Build it in `TestMain` beside `stubchild` (`fakewslPath`).
- [ ] **Step 2: Write the failing tests** (all `skipOnWindows`):
  - `TestWSLSpecStartsAndServes`: a WSL spec whose `binary` is `stubchildPath` (allowlisted in `WSLPolicy`) reaches `running`. The fakewsl log's argv starts with `-d Ubuntu-24.04 -u ai`, contains `--unit=opagent-<nonce>-…`, and its `WSLENV` contains `XDG_RUNTIME_DIR/u`.
  - `TestWSLSpecGracefulStopSignalsScope`: `Stop` → the fakewsl log contains `systemctl --user kill --signal=SIGTERM <unit>.scope`; the instance rests at `stopped` with no `last_error`.
  - `TestWSLSpecKillEscalation`: stubchild with `-ignore-sigterm` → after the kill grace the log contains `--signal=SIGKILL`; the process is gone.
  - `TestWSLOrphanSweepKillsOnlyForeignNonce`: pre-record a unit `opagent-deadbeef-x-1` with a live sleeper pid and start a manager (nonce ≠ `deadbeef`) whose first `applyConfig` carries a WSL spec → the sleeper is killed, the new spec's own unit stays alive.
  - `TestWSLLaunchFailedExitCode`: `wslLaunchFailed(-1)`, `wslLaunchFailed(4294967295)` true; `0`, `1`, `9`, `15` false. A start-failed WSL instance with such an exit gets `last_error.message` containing `wsl.exe could not run the workload`.
  - `TestWSLUIDLookupFailureIsStartFailure`: a runner returning an error for `id -u` → `start_failed` with a message naming `runtime_wsl_user`, and no exec.
- [ ] **Step 3: Run** `go test ./internal/runtime -run 'WSL' -v` → FAIL.
- [ ] **Step 4: Implement.**
  - `startProcess`, after `expandSpec` and the config files: when `spec.IsWSL()` → `uid, err := o.wslUID(distro)` (error → the existing start-failure path with that message); `unit := wslUnitName(o.wslNonce, spec.ID, plog.gen)` (create `plog` before this); `l := buildWSLLaunch(…)`; `cmd := exec.Command(l.Exe, l.Args...)`; `cmd.Env = l.Env`; leave `cmd.Dir` empty; `resolved` from `ex.withWrapper(l)`. Native stays byte-identical.
  - Set `proc.ctl`: `nativeControl{cmd}` or `wslControl{…}`. `terminateNow` calls `proc.ctl.terminate()`; `handleKillGraceExpired` calls `proc.ctl.kill()`.
  - `wslControl.terminate` runs `systemctl --user kill --signal=SIGTERM <unit>.scope` in a goroutine tracked by `o.m.wg`, with a 10 s timeout, logging a Warn on error. `kill` runs the same with `SIGKILL`, then `killGroup(cmd)` as the backstop.
  - Orphan sweep: once per manager lifetime, on the first `applyConfig` (pattern `maybeSweepMemberCaches`, off-owner via `o.m.wg.Go`), when `WSLPolicy.Exe != ""` and `User != ""`: for every allowed distro, list `opagent-*.scope`; for each unit whose nonce segment ≠ `o.wslNonce`, run `systemctl --user kill --signal=SIGKILL`. Bounded by 10 s per call; failures are Warn only.
  - In `onProcExited`, a WSL instance that never became healthy and whose exit code satisfies `wslLaunchFailed` gets the message `wsl.exe could not run the workload (exit <code>); see the runtime log`.
- [ ] **Step 5: Run** → PASS; full `go test ./internal/runtime` (memory note: `TestVRAMRunUnstableWindowsAreInconclusive` can flake under full-package load; rerun it alone before chasing it); `GOOS=windows go build ./... && GOOS=windows go vet ./...`.
- [ ] **Step 6: Commit** `feat(agent): run wsl specs under a systemd user scope with graceful stop and an orphan sweep (#381)`.

### Task 5: WSL RAM from the scope's cgroup, the VM RAM pool

**Files:**
- Modify: `server-agent/internal/runtime/wsl.go` (sampler)
- Modify: `server-agent/internal/runtime/manager.go` (`measureTarget` 1092, `cmdMeasured` 1110, `measurementTargets` 3442, `dispatchMeasurement` 3362, `applyMeasurement` 3545, `buildSnapshot` 3672-3870, `onProcExited` 4462 `ramBudgetAtDeath`)
- Modify: `server-agent/internal/runtime/policy.go` (`RunningProc` 18-74, `PolicySnapshot` 80-228, `admitSteps` 1263-1310, new rule after `ruleHostRAM` 1661-1724)
- Test: `server-agent/internal/runtime/wsl_sample_test.go` (new), `policy_wsl_test.go` (new), `manager_wsl_test.go`

**Interfaces:**
- Consumes: Task 4 (`runningProc.wslUnit/wslDistro`, `wslRunner`, fakewsl `$FAKEWSL_SAMPLE`).
- Produces:
  - `const wslSampleScript = "…"` (fixed; see Step 3).
  - `type wslVMInfo struct { TotalMB, AvailableMB int }`
  - `func parseWSLSample(out []byte) (ramByUnit map[string]int, vm wslVMInfo, err error)`
  - `measureTarget.wslUnit, measureTarget.wslDistro string`; `cmdMeasured.ramByUnit map[string]int`, `cmdMeasured.wslVM *wslVMInfo`
  - `RunningProc.WSL bool`; `PolicySnapshot.WSLRAMBudgetMB int`, `PolicySnapshot.WSLRAMCeilingMB int`
  - `func ruleWSLRAM(a *admission)` (name the receiver as the existing rules do) and `func wslRAMOwnDemandRefusal(snap PolicySnapshot, spec Spec) (Decision, bool)`.

- [ ] **Step 1: Write the failing tests.**
  - `TestParseWSLSample`: the input `unit opagent-a-m-1 8419880960 4535152640\nunit opagent-a-n-2 -\nmeminfo 28763116 21000000\n` (memory in bytes, meminfo in kB) gives `ramByUnit["opagent-a-m-1"] == 3704` (MiB, floor), `opagent-a-n-2` absent, `vm == {TotalMB: 28088, AvailableMB: 20507}`. Garbage lines → error.
  - `TestWSLTargetsNeverReachThePIDRAMMeasurer`: a manager with one native and one WSL instance live; a `SetRAMMeasurer` fake records the pids it got → only the native pid. The WSL instance's `ram_measured_mb` equals the fakewsl sample.
  - `TestAdmitWSLRAMFitsVMBudget` / `TestAdmitWSLRAMEvictsIdleWSLOnly`: VM budget 10000. Running: an idle native 6000 and an idle WSL 6000. A WSL candidate 5000 → evicts the WSL proc only. A native candidate is unaffected by the VM budget.
  - `TestAdmitWSLRAMOwnDemandRefusal`: `WSLRAMCeilingMB` 8000, WSL candidate 9000 → `not_permitted`, message `spec X: RAM demand 9000 MB exceeds the WSL VM RAM budget 8000 MB on its own`.
  - `TestAdmitWSLRAMUnconstrainedWithoutSample`: `WSLRAMBudgetMB == 0` → the rule is skipped.
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'WSLSample|WSLTargets|AdmitWSL' -v` → FAIL.
- [ ] **Step 3: Implement.**
  - Sampler call: `wsl.exe -d D -u U --exec sh -c "$wslSampleScript" sh <unit…>`, once per distro with live WSL targets, off-owner inside `dispatchMeasurement`, under the measurer's 2 s deadline.
  - The script, for each `$u` in `"$@"`, reads `/sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/app.slice/$u.scope/{memory.current,memory.stat}`. It prints `unit <u> <memory.current> <inactive_file>`, or `unit <u> -` when the files are missing. It then prints `meminfo <MemTotal kB> <MemAvailable kB>` from `/proc/meminfo`. Use only POSIX `sh`, `cat` and `awk`.
  - The RAM figure is `(current − inactive_file)` in MiB; `<= 0` is unknown, as `realRAM` treats it.
  - `applyMeasurement` sets `st.measuredRAM` for WSL targets from `ramByUnit`, with the same generation check, and stores `o.wslVM`.
  - `buildSnapshot`: `RunningProc.WSL = st.spec.IsWSL()`. `WSLRAMBudgetMB = max(1, vm.AvailableMB + Σ WSL liveRAM − o.wslRAMReserveMB)`, or 0 without a sample. `WSLRAMCeilingMB = max(1, vm.TotalMB − o.wslRAMReserveMB)`, or 0.
  - `admitSteps`: `wslRAMOwnDemandRefusal` right after `ramOwnDemandRefusal`; `ruleWSLRAM` right after `ruleHostRAM` and before `ruleProcessCount`, with the same victim discipline (`toEvict` already notionally removed, `evictableOf` → `orderVictims`, restricted to `r.WSL`).
  - `onProcExited`: for a WSL instance, the 90 %-of-pool memory-pressure criterion reads the VM budget, not the host budget.
- [ ] **Step 4: Run** → PASS; full package; `GOOS=windows …`.
- [ ] **Step 5: Commit** `feat(agent): per-scope RAM for wsl instances and a WSL VM RAM pool (#381)`.

### Task 6: VRAM attribution from the WSL VM worker (`vmwp`)

**Files:**
- Create: `server-agent/internal/collector/vmworker.go` (pure filter, untagged) and `vmworker_windows.go` / `vmworker_other.go`
- Modify: `server-agent/internal/runtime/manager.go` (setter beside `SetMeasurer` 791; `dispatchMeasurement`; `applyMeasurement`; `buildSnapshot` VRAM part 3681-3778; `startProcess`; `onProcExited`; `vramBootstrapEligible` 2719)
- Modify: `server-agent/internal/runtime/policy.go` (`PolicySnapshot`; `rulePerGPUVRAM`; a WSL start-interlock step)
- Modify: `server-agent/main.go` (wire `collector.NewWSLVMWorkerPIDs()`)
- Test: `server-agent/internal/collector/vmworker_test.go`, `server-agent/internal/runtime/manager_wsl_vram_test.go` (new), `policy_wsl_test.go`

**Interfaces:**
- Consumes: Tasks 4-5; the existing VRAM measurer `func(pids []int) map[int]map[int]int`.
- Produces:
  - collector: `func NewWSLVMWorkerPIDs() func() []int` (Windows: every process whose image name equals `vmwp.exe`, case-insensitive; other OS: nil) and `func filterVMWorkerPIDs(procs map[int]string) []int` (sorted).
  - runtime: `func (m *Manager) SetWSLVMPIDs(f func() []int)` (an `atomic.Pointer`, like `SetMeasurer`).
  - `type wslStartWindow struct { proc *runningProc; baseline, exitedCredit map[int]int; healthyDispatched bool; void bool }`; owner fields `vmVRAM map[int]int`, `wslAttrib map[*runningProc]map[int]int`, `wslWindow *wslStartWindow`.
  - `func sumVMVRAM(byPID map[int]map[int]int, vmPIDs []int) map[int]int`
  - `func wslDelta(after, baseline, credit map[int]int) map[int]int` (per GPU `max(0, after−baseline+credit)`, zero entries dropped)
  - `PolicySnapshot.UnattributedVRAM map[int]int`, `PolicySnapshot.WSLStartInFlight bool`
  - `func (o *owner) vramMeasurable(spec Spec) bool` (native: a measurer is set; WSL: a measurer and a VM-PID finder are both set)

- [ ] **Step 1: Write the failing tests.**
  - `TestFilterVMWorkerPIDs`: `{10:"vmwp.exe", 11:"VMWP.EXE", 12:"vmmem", 13:"vmwp.exe.bak"}` → `[10 11]`.
  - `TestSumVMVRAM`: two vmwp pids with `{0:12318}` and `{0:7432, 1:22}` → `{0:19750, 1:22}`.
  - `TestWSLDelta`: after `{0:19772}`, baseline `{0:12340}`, credit `{}` → `{0:7432}`; with credit `{0:12318}` and after `{0:7454}` → `{0:7432}`; a negative result → dropped.
  - `TestWSLStartDeltaAttributesVMGrowth`: owner-level with a scripted VRAM measurer whose vmwp reading is `{0:22}` before the exec and `{0:12340}` at the first measurement dispatched after the first healthy probe. The instance's `Status.MeasuredVRAM == {0:12318}`; the window is closed.
  - `TestWSLSecondStartWaitsForSettle`: with a window open, `Admit` for another WSL spec gives `Wait` (not an eviction); after the window closes the queued start proceeds.
  - `TestWSLExitDuringWindowCorrectsDelta`: an attributed WSL proc (`{0:12318}`) exits during another's window; after `{0:7454}`, baseline `{0:12340}` → the new instance gets `{0:7432}`.
  - `TestWSLExitOfUnattributedProcVoidsWindow`: the exiting proc had no attribution → no attribution is recorded for the window's proc.
  - `TestRemainderChargedAsUnattributedVRAM`: VM `{0:20000}`, Σ attributions `{0:12000}` → snapshot `UnattributedVRAM == {0:8000}`. Policy: budget 24000, candidate 6000 on GPU 0, running 12000 → `Wait`/evict, because 12000+8000+6000 > 24000. `UnattributedVRAM` is never in `Decision.Evict` and does not change `ownDemandRefusal`.
  - `TestVRAMBootstrapNotGrantedWithoutVMFinder`: a WSL spec with unknown VRAM, a VRAM measurer set but no `SetWSLVMPIDs` → `vramBootstrapEligible` false; with both set → true.
- [ ] **Step 2: Run** `go test ./internal/collector ./internal/runtime -run 'VMWorker|SumVMVRAM|WSLDelta|WSLStart|WSLSecond|WSLExit|Remainder|VRAMBootstrapNotGranted' -v` → FAIL.
- [ ] **Step 3: Implement.**
  - **VM figure.** Whenever a WSL instance is live or a window is open, both VRAM call sites (the synchronous one in `buildSnapshot` and the off-owner one in `dispatchMeasurement`) append the finder's PIDs to the PID list and compute `vmVRAM = sumVMVRAM(...)`. **All** `vmwp` PIDs are summed. Another Hyper-V VM's GPU memory then counts as WSL remainder, which is the safe direction; record it in §11.4 in Task 9. WSL instances' own PIDs are never sent to the VRAM measurer.
  - **Window.** Open the window in `startProcess` just before the exec of a WSL instance when `vramMeasurable`: a synchronous VM reading gives the baseline. Set `healthyDispatched` when the measurement dispatched by the first passing health probe (`measureSoon`) is sent. Close the window in `applyMeasurement` on the first result with `healthyDispatched`: `attrib := wslDelta(vm, baseline, exitedCredit)`, `o.wslAttrib[proc] = attrib`, `st.measuredVRAM = attrib`. Then wake the queued starts the same way the RAM-bootstrap interlock wakes them when it resolves.
  - **Exits.** On any WSL exit during an open window: an attributed proc adds its attribution to `exitedCredit`; an unattributed one sets `void`. If the window's own proc exits, the window closes without attribution. Every exit deletes `o.wslAttrib[proc]`.
  - **Snapshot.** `WSLStartInFlight = o.wslWindow != nil`. `UnattributedVRAM[g] = max(0, vmVRAM[g] − Σ wslAttrib[g])`, taken from the synchronous admission-time reading. The `RunningProc.GPUs` of a WSL proc are `max(attrib, pushed demand)` per GPU.
  - **Policy.** A WSL candidate with `WSLStartInFlight` → `Wait` (no eviction), placed after the own-demand refusals. `rulePerGPUVRAM` adds `UnattributedVRAM[g]` to the sum for each GPU the candidate touches.
  - **Bootstrap.** `vramBootstrapEligible` gains `&& o.vramMeasurable(st.spec)`.
- [ ] **Step 4: Run** → PASS; full packages; `GOOS=windows …`.
- [ ] **Step 5: Commit** `feat(agent): attribute WSL VRAM from the VM worker by start delta, charge the remainder (#381)`.

### Task 7: Gateway: store, validation, wire `execution`, feature gating

**Files:**
- Create: `gateway/backend/internal/routing/execution_mode.go`
- Modify: `gateway/backend/internal/routing/store.go:2974-3136` (`RuntimeSpec`)
- Modify: `gateway/backend/internal/store/migrate.go` (registry after line 143; `migration109Up` after `migration108Up` 4794)
- Modify: `gateway/backend/internal/store/sqlite_runtime.go` (26-102 upsert, 104-113, 119-128, 471-505)
- Modify: `gateway/backend/internal/portal/service_runtime.go` (sentinels 39-189; `RuntimeSpecDTO` 466-653; `GetRuntimeSpec` 783/813; `PutRuntimeSpecRequest` 669-775; `validateRuntimeSpecRequest` 1217, append the new check last; `runtimeSpecRow` 1626-1663; `runtimeSpecDTO` 2166; `AgentRuntimeSpecDTO` 3052-3150; `AgentRuntimeConfig` 3359-3419; `agentRuntimeSpecDTO` 3570)
- Modify: `gateway/backend/internal/portal/service_runtime_benchmark.go:46-63` (`putRequestFromDTO`)
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go:130` (error rows)
- Modify: `gateway/backend/internal/gateway/agent_ingest.go:2215-2244` (re-push on a change of `runtime_exec_wsl`)
- Modify: `gateway/backend/internal/gateway/agent_features.go` (exported `RuntimeExecWSLFeature`, agent-only; not on `gatewayAgentFeatures`)
- Test: `internal/store/migration109_runtime_spec_execution_test.go` (new), `runtime_spec_column_parity_test.go` (seeded list + distinct values), `internal/portal/service_runtime_test.go`, `service_runtime_instances_test.go`, `internal/gateway/agent_features_test.go`

**Interfaces:**
- Produces:
  - `type ExecutionMode string`; `ExecutionModeNative = "native"`, `ExecutionModeWSL = "wsl"`
  - `RuntimeSpec.ExecutionMode ExecutionMode`, `RuntimeSpec.WSLDistro string`
  - Migration 109 `runtime_spec_execution_mode`: columns `execution_mode text not null default 'native'` and `wsl_distro text not null default ''`
  - JSON `execution_mode`, `wsl_distro` on `RuntimeSpecDTO` and `PutRuntimeSpecRequest`
  - `type AgentExecutionDTO struct { Mode string \`json:"mode"\`; WSL *AgentWSLExecutionDTO \`json:"wsl,omitempty"\` }`; `type AgentWSLExecutionDTO struct { Distro string \`json:"distro"\` }`; `AgentRuntimeSpecDTO.Execution *AgentExecutionDTO \`json:"execution,omitempty"\``
  - `const AgentFeatureRuntimeExecWSL = "runtime_exec_wsl"`
  - Errors (400): `runtime_spec.execution_mode_invalid`, `runtime_spec.wsl_distro_invalid`, `runtime_spec.wsl_path_not_linux`, `runtime_spec.wsl_cluster_unsupported`

- [ ] **Step 1: Write the failing tests.**
  - `TestMigration109AddsRuntimeSpecExecutionColumns` (pattern of `TestMigration96AddsRuntimeSpecRAMColumns`): both columns exist with their defaults on every dialect; an existing row reads `native`/`""`.
  - Parity: add both columns to `seeded` in `TestAgentRuntimeSpecsSchemaColumnsAllCovered` and distinct per-row values in `TestConformanceRuntimeSpecReadersAgreeOnEveryColumn`.
  - `TestPutRuntimeSpecExecutionValidation`: rows for `""` → stored `native`; `"docker"` → `ErrRuntimeSpecExecutionModeInvalid`; `wsl` + distro `""` or `"a b"` → `ErrRuntimeSpecWSLDistroInvalid`; `wsl` + binary `C:\x\vllm.exe` → `ErrRuntimeSpecWSLPathNotLinux`; `wsl` + `work_dir` `relative` → the same; `wsl` + `cluster_enabled` → `ErrRuntimeSpecWSLClusterUnsupported`; `native` with a non-empty distro → stored `""`. A valid `wsl` spec round-trips through GET.
  - `TestAgentRuntimeDocWSLSpecOnlyForAnAgentThatDeclaresIt` (case-table style of `TestAgentRuntimeDocInstanceFieldsOnlyForAnAgentThatRunsInstances`):
    - declared → the spec is pushed with `execution == {mode:"wsl", wsl:{distro}}`;
    - known but not declared → the spec is **absent** from `doc.Specs`, and coresident pairs naming it are dropped;
    - not heard from → `doc.AwaitsAgentFeatures == true`;
    - nil reader → pushed.
  - `TestAgentRuntimeDocNativeSpecHasNoExecutionKey`: `wireKeys` of a native spec do not contain `execution`.
  - `TestRuntimeExecWSLFeatureName`: pins `"runtime_exec_wsl"`.
  - An ingest test: flipping the declared features of a server with a WSL spec triggers `PushRuntimeConfig`, mirroring the existing `runtime_instances` re-push test.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store ./internal/portal ./internal/gateway -run 'Migration109|Execution|WSL|ColumnsAllCovered|ReadersAgree' -v` → FAIL.
- [ ] **Step 3: Implement.**
  - Thread both columns through the four store lists. The placeholder count goes from 46 to 48. Both columns go in the `do update set` list, because they are operator-owned.
  - Add the check `validateRuntimeSpecExecution` **last** in `validateRuntimeSpecRequest` (the order is part of the API). Distro rule `^[A-Za-z0-9._-]{1,64}$`. A Linux path is non-empty and starts with `/`.
  - In `AgentRuntimeConfig`, a WSL spec whose server is known and lacks the feature is skipped. A WSL spec with unknown features sets `awaitsFeatures`. `agentRuntimeSpecDTO` sets `Execution` only for `wsl`.
  - Add the HTTP rows and the ingest arm.
- [ ] **Step 4: Run** → PASS; then the Postgres leg: `OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./internal/store -run 'Migration109|ColumnsAllCovered|ReadersAgree|RuntimeSpec'` (provision Postgres as CI does; ask before starting Docker/colima).
- [ ] **Step 5: Commit** `feat(gateway): store and push the wsl execution mode, withheld from agents without runtime_exec_wsl (#381)`.

### Task 8: Portal: execution-mode editor and WSL markers

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeSpec` 182-391: optional `execution_mode?: 'native' | 'wsl'`, `wsl_distro?: string`)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (`emptySpec` 284-345; state near 2732; `resetSpecFields` 2876; `hydrateSpecFields` 2918; `buildSpecBody` 3309-3412; the editor near the Type select 4974-5006; the per-spec status row that renders the measured VRAM)
- Modify: `gateway/frontend/src/components/shared/format.ts:202` (four error codes)
- Modify: `gateway/frontend/src/components/shared/runtimeActions.ts` (`RUNTIME_EXEC_WSL_FEATURE = 'runtime_exec_wsl'`)
- Modify: `gateway/frontend/src/i18n.ts` (de and en)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`

**Interfaces:**
- Consumes: the JSON fields and error codes from Task 7.
- Produces: i18n keys:
  - `runtimeSpecExecutionMode`, `runtimeSpecExecutionModeNative`, `runtimeSpecExecutionModeWSL`
  - `runtimeSpecWSLDistro`, `runtimeSpecWSLHint`, `runtimeSpecWSLAgentUnsupported`
  - `runtimeSpecBinaryLinux`, `runtimeSpecWorkDirLinux`
  - `runtimeStatusWSLMarker`, `runtimeStatusVRAMFromVM`
  - `errorRuntimeSpecExecutionModeInvalid`, `errorRuntimeSpecWSLDistroInvalid`, `errorRuntimeSpecWSLPathNotLinux`, `errorRuntimeSpecWSLClusterUnsupported`

- [ ] **Step 1: Write the failing tests:**
  - `it('sends execution_mode and wsl_distro in the spec PUT body')`
  - `it('defaults execution_mode to native')`
  - `it('shows the execution-mode select only when the agent declares runtime_exec_wsl')`: hidden for a native spec without the feature; shown with it.
  - `it('keeps a wsl spec editable and warns when the agent lacks runtime_exec_wsl')`
  - `it('labels binary and work dir as Linux paths in WSL mode')`
  - `it('marks a wsl spec row and labels its measured VRAM as attributed from the WSL VM')`
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/RuntimeAdminSection.test.tsx -t 'execution|wsl'` → FAIL.
- [ ] **Step 3: Implement**, following the `visible_devices_mode` select pattern (`SelectField id="runtime-spec-execution-mode"`, then `TextField id="runtime-spec-wsl-distro"` when the mode is `wsl`, then the hint `runtimeSpecWSLHint`: binary and work dir are Linux paths and must be in the agent's `runtime_wsl_allowed_*`). The Models catalog marker is **not** in this PR; record it as a follow-up in the PR description.
- [ ] **Step 4: Run** `npm test`, `npm run build`, `npm run lint`, `npm run format:check` → all pass.
- [ ] **Step 5: Commit** `feat(portal): edit the wsl execution mode and mark wsl specs (#381)`.

### Task 9: Documentation and ADR

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md`:
  - a new `### 3.8 Execution modes: WSL2` (launch chain, invariant, policy, env forwarding, lifecycle mapping, orphan sweep, RAM, VM pool, VRAM attribution with the Phase 0 measurements, limits);
  - §5.3 (VRAM on Windows now also reads `vmwp`);
  - §7 (agent-only list: add `runtime_exec_wsl`, update the count);
  - §13 (limitations);
  - §15 (configuration reference).
- Modify: `docs/architecture/09-architecture-decisions.md`: add **ADR-101**, "WSL2 execution mode: a rootless systemd user scope, cgroup RAM, and VRAM attributed from the VM worker by start delta with the remainder charged".
- Modify: `docs/architecture/11-risks-and-technical-debt.md` §11.3 (Windows paths untested in CI; the fake `wsl` harness) and §11.4 (start-delta attribution; all `vmwp` summed; linger-only acceptance pending).
- Modify: `docs/architecture/reference/data-model.md` (`agent_runtime_specs` row L60; migration 109 row; count "109 migrations").
- Modify: `docs/architecture/reference/api-surface.md` (the runtime-spec row L675; PUT validation prose ~1086-1115; error table L1023; §5.1 per-spec wire keys L1910-1926 with `execution`).
- Modify: `server-agent/README.md`:
  - six new rows in the settings table after line 74;
  - a new `#### WSL2 workloads` subsection under `### Managed model runtime` covering: the agent as the distro-owning user, as a service or interactively, never LocalSystem; `loginctl enable-linger`; `.wslconfig` `vmIdleTimeout=-1` and `instanceIdleTimeout=-1`; systemd in the distro; models in ext4; `build-essential` for vLLM's Triton; a dedicated distro with `[interop] enabled=false` where possible.

- [ ] **Step 1: Write the docs.** They must state only behaviour the code now has, and quote the Phase 0 numbers from the spec.
- [ ] **Step 2: Run** `make lint-docs` → PASS.
- [ ] **Step 3: Commit** `docs: WSL2 execution mode, ADR-101 (#381)`.

### Task 10: Full verification, cleanup, PR

- [ ] **Step 1: Run** `make test-go`; `cd server-agent && go test ./... && GOOS=windows go build ./... && GOOS=windows go vet ./...`; `make lint` (golangci per module, docs); the Postgres leg for `internal/store`; `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check`. All pass; record any known flake re-runs.
- [ ] **Step 2: Run** the Sonar gate in the background (`make sonar-scan`, then `make sonar-findings && make sonar-branch-findings`). Act on branch-attributed findings.
- [ ] **Step 3: Remove** `docs/superpowers/` from the branch; `git diff --name-only origin/main...HEAD` shows no `docs/superpowers` path.
- [ ] **Step 4: Push** over SSH and open the PR (English, a substantive commit body for the squash). It closes #381. The PR states:
  - the acceptance run still due on the Windows host (the native Windows build exercises `wsl.exe`, `systemctl --user`, the PDH `vmwp` reading, and linger without a logged-on user);
  - the Models catalog marker follow-up.

  Enable Auto-fix on the PR.
