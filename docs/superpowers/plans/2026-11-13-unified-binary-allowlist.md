# Unified binary allowlist (native + WSL) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator lists every binary the agent may run, native or inside a WSL2 distro, in ONE place (`runtime_allowed_binaries`), so WSL binaries appear in the Binaries tab and in the run-spec binary picker, with `--version` detection for llama-server and vLLM on both paths.

**Architecture:** A WSL binary becomes an allowlist entry of the form `wsl://<distro>/<absolute Linux path>` in the existing native lists; the three `runtime_wsl_allowed_*` settings go away. The agent's local policy splits the lists by scheme, the binary inventory reports `wsl://` entries verbatim (probing versions through `wsl.exe` only when the distro already runs), the gateway derives `execution`/`distro`/`exec_path` from the stored path (no migration), and the portal shows an execution column and lets the picker set mode, distro and Linux path in one selection.

**Tech Stack:** Go (server-agent module, gateway/backend module), React + MUI + vitest (gateway/frontend).

**Spec:** the "Design" section below (agreed with the owner in chat on 2026-11-13: "ja das gefällt mir. grundsätzlich vllm --version wäre schon interessant. für nativ und auch für wsl.").

## Design

### D1 Entry form

`wsl://<distro>/<absolute POSIX path>`, e.g. `wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm`.

- Scheme is exactly lowercase `wsl://`. Anything else (`WSL://`, `wsl:/`) is not a WSL entry.
- `<distro>` is everything up to the first `/` after the scheme and must match the existing `wslDistroPattern` (`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, `server-agent/internal/runtime/types.go:227`).
- The Linux path is the rest INCLUDING that `/`; it must start with `/` and contain no NUL and no backslash. It is compared verbatim (no cleaning), exactly like a native entry.
- A string that starts with `wsl://` but breaks a rule above is a **malformed WSL entry**: it permits nothing, the agent logs a startup warning naming the setting and index, and the inventory does not list it.
- The same form is valid in `runtime_allowed_dirs`; there a trailing `/*` keeps its existing meaning (`allowedDirBase`).

### D2 Local policy

- Native specs: only entries that are not WSL entries (well-formed or malformed) count. "Allowlist is empty" means no native entry. Native dir containment uses only native dir entries; "no native dir entries and `runtime_allow_binary_dirs` off" still means any work_dir. `AllowBinaryDirs` adds native binaries' parent dirs only.
- WSL specs (`spec.IsWSL()`), in this order: `runtime_wsl_exe` set; `runtime_wsl_user` not refused (unchanged rules); the spec's distro is named by at least one well-formed `wsl://` BINARY entry; `spec.Binary` starts with `/` and equals the Linux path of a `wsl://` binary entry for THAT distro; empty work_dir permitted; work_dir must start with `/`; the allowed dir set for the distro is its well-formed `wsl://` dir entries plus, when `runtime_allow_binary_dirs` is on, `path.Dir` of each of its `wsl://` binary entries; an empty set means any absolute work_dir; otherwise `withinPosixDir(workDir, allowedDirBase(dir))` for some member.
- Refusal texts keep the #381 rule: they name the setting and a count, never a path, distro or user.
- `WSLPolicy` shrinks to `{Exe, User}`. The orphan sweep/pre-warm (`maybeWarmWSL`) calls into a distro only when the policy's binary list names it.

### D3 Config

- Removed: `runtime_wsl_allowed_distros`, `runtime_wsl_allowed_binaries`, `runtime_wsl_allowed_dirs` and their env vars `OP_AGENT_RUNTIME_WSL_ALLOWED_DISTROS/_BINARIES/_DIRS`. Kept: `runtime_wsl_exe`, `runtime_wsl_user`.
- The agent still starts when an old config carries a removed setting; it logs one warning per removed setting found (file key present or env var non-empty) that names the replacement (`wsl://<distro>/<path>` entries in `runtime_allowed_binaries` / `runtime_allowed_dirs`).
- The shared config golden and its two other generators (gateway template, portal builder) drop the three keys and describe the `wsl://` form in the comments of `runtime_allowed_binaries` and `runtime_allowed_dirs`.

### D4 Inventory

- New binary kind `vllm` (file base name `vllm`, `.exe` suffix ignored as today).
- Version-probed kinds: `llama_server` (existing parser, 10 s timeout) and `vllm` (new parser, 60 s timeout), for native AND WSL entries. `rpc_server`/`other` are still never executed.
- vLLM parser: the LAST non-empty trimmed output line matching `^v?(\d+\.\d+(?:\.\d+)?[0-9A-Za-z.+-]*)$` gives the version (without `v`); build/commit stay empty; no match = no version.
- WSL entries are listed with `Path` = the entry string verbatim and the kind of the Linux path's base name.
- WSL probing never boots the VM. Per scan, when at least one well-formed WSL entry exists, `runtime_wsl_exe` is set and the user is not refused: one `wsl.exe --list --running --quiet` call (env: minimal env + `WSL_UTF8=1`). For an entry whose distro is in that list (case-insensitive compare): stat via `wsl.exe -d D -u U --cd / --exec stat -L -c %s:%Y:%F -- <path>`, then the cached-or-probed version via `wsl.exe -d D -u U --cd / --exec <path> --version`. Same `(size, mtime)` cache and retry backoff as native.
- Distro not running (or the list call fails): no call targets the distro; the entry keeps its last good cached version if any (source `agent`), else path-derived version as today. The cache entry is NOT deleted.
- Exe empty / user refused: WSL entries are listed, never probed.
- rpc_server sibling inheritance keys WSL entries by `wsl://<distro>` + `path.Dir(linuxPath)`.

### D5 Gateway

- Kind `vllm` accepted at ingest and on manual writes.
- `ServerBinaryDTO` gains `execution` (`"native"`/`"wsl"`), `distro` (`""` for native) and `exec_path` (Linux path for WSL, `path` for native), derived from `path` on read. No schema change.
- `validBinaryPath` additionally accepts a well-formed `wsl://` path (same rules as D1) and rejects a malformed one.

### D6 Portal

- Binaries tab: new column "Ausführung"/"Execution" right after the path: native rows show the existing "Nativ"/"Native" text, WSL rows show "WSL2" with the distro as caption. Kind label "vLLM"; the manual-add kind select offers it.
- Run-spec binary picker (`kind="llama_server"`): options are `llama_server`, `vllm` and `other`; a WSL option's label is its Linux path, with "WSL2 · <distro>" shown in the option row. Selecting an option sets the binary field to the option's `exec_path`, the execution mode to `wsl`/`native` and, for WSL, the distro. Typing a free path changes nothing but the binary field. The RPC-member picker (`kind="rpc_server"`) never offers WSL entries.
- `runtimeSpecWSLHint` (de/en) points at `wsl://<Distro>/<Pfad>` in `runtime_allowed_binaries`.

### D7 Version

Agent `Version` 0.19.0 -> 0.19.1 (PATCH: no feature flag is added).

## Global Constraints

- Never commit to or merge into `main`; work on branch `claude/unified-binary-allowlist`.
- Repo text (code comments, docs, commit messages) in English; no attribution lines in commits.
- No LAN host names of the owner in any repo text; examples use `Ubuntu-24.04` and `/home/ai/...`.
- Refusal and status texts never contain a path, distro or user name (agent-runtime-manager §3.1 rule).
- The config golden `server-agent/testdata/server-agent.config.jsonc`, the gateway template (`gateway/backend/internal/gateway/agent_binaries.go`) and the portal builder (`buildServerAgentConfig` in `gateway/frontend/src/components/AgentTokenSection.tsx`) must stay byte-identical generators; their three parity tests must pass in the same commit.
- Go lint gate: from each touched Go module, `golangci-lint fmt --diff` and `GOTOOLCHAIN=go1.26.5 golangci-lint run ./...` report nothing.
- Frontend gate: `npm run lint`, `npm run format:check`, `npx tsc --noEmit -p .` (or the repo's typecheck script) and the touched vitest files pass.
- Known local-only failures to ignore: `TestVoice*`/`TestLiveTranscriptionLoopback` in the gateway (Go 1.27 net/http, #387) — run gateway tests with `-skip 'TestVoice|TestLiveTranscriptionLoopback'`.

## Review Focus

1. Old config still carrying `runtime_wsl_allowed_binaries` (the owner set it while testing #390): agent must start, warn naming the replacement, and refuse WSL specs with a text that points at `wsl://` entries — test in Task 1.
2. Operator types `WSL://Ubuntu-24.04/...` or a Windows-style `wsl://Ubuntu-24.04\home\...`: treated as malformed — startup warning, permits nothing, not in the inventory — tests in Task 1 (parse + warning) and Task 2 (not listed).
3. The same Linux path allowlisted for two distros: two inventory rows; a spec in distro A is not permitted by distro B's entry; the picker sets the selected row's distro — tests in Task 1 and Task 4.
4. Distro stopped by the WSL idle timeout during a rescan: no wsl.exe call may target it (it would boot the VM), and the last known version stays visible — test in Task 2.
5. `vllm --version` printing log/warning lines around the version, or taking tens of seconds: the last version-shaped line wins; 60 s timeout — test in Task 2. Also: a native spec whose work_dir is literally `wsl://...` must not pass via a `wsl://` dir entry — test in Task 1.

---

### Task 1: Agent allowlist with `wsl://` entries, config, generators, operator docs

**Files:**
- Create: `server-agent/internal/runtime/wsl_allowlist.go`, `server-agent/internal/runtime/wsl_allowlist_test.go`
- Modify: `server-agent/internal/runtime/policy_local.go` (`LocalPolicy` doc, `WSLPolicy`, `isZero`, `Permit`, `permitWSL`), `server-agent/internal/runtime/proc_control.go` (`maybeWarmWSL`), `server-agent/internal/runtime/policy_local_wsl_test.go`, manager WSL tests that build `WSLPolicy` with the removed fields (`manager_wsl_test.go`: `wslTestPolicy`, `TestWSLPrewarmOnlyDistrosNamedBySpecs`, others the compiler finds)
- Modify: `server-agent/internal/config/config.go` (fields, `fileConfig`, `Load`), `server-agent/internal/config/config_test.go` (`TestLoadRuntimeWSLSettings` and golden parity), `server-agent/main.go:160-171` and its `ManagerOptions.WSL`, plus wherever main logs startup warnings
- Modify: `server-agent/internal/agent/agent.go:419-424` and `server-agent/internal/agent/features_test.go:618,642` (Version 0.19.1)
- Modify generators: `server-agent/testdata/server-agent.config.jsonc`, `gateway/backend/internal/gateway/agent_binaries.go` (+ its test ~L536-570), `gateway/frontend/src/components/AgentTokenSection.tsx` (+ `AgentTokenSection.test.tsx` ~L415-436)
- Modify operator docs: `server-agent/README.md` (config rows ~L64-71, WSL2 section ~L533-592), `docs/architecture/reference/config-env.md` (~L252-259)

**Interfaces:**
- Produces (package `runtime`, imported as `runtimectl` elsewhere):
  - `const WSLEntryScheme = "wsl://"`
  - `func IsWSLEntry(entry string) bool` — has the `wsl://` prefix (well-formed or not)
  - `func ParseWSLEntry(entry string) (distro, linuxPath string, ok bool)` — D1 rules
  - `type WSLPolicy struct { Exe, User string }` (fields removed: `AllowedDistros`, `AllowedBinaries`, `AllowedDirs`)
- Produces (package `config`): `Config.Warnings []string` — human-readable startup warnings (removed settings, malformed `wsl://` entries) that `main` logs with `slog.Warn` once at startup.

- [ ] **Step 1: Write failing tests**
  - `TestParseWSLEntry` (table): `wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm` -> (`Ubuntu-24.04`, `/home/ai/vllm-env/bin/vllm`, true); `wsl://Ubuntu-24.04/` -> (`Ubuntu-24.04`, `/`, true); not ok: `WSL://Ubuntu-24.04/x`, `wsl:/Ubuntu/x`, `wsl://Ubuntu-24.04`, `wsl:///x`, `wsl://-bad/x`, `wsl://Ubuntu-24.04\home\x`, `wsl://Ubuntu-24.04/home\x`, `wsl://Ubuntu/a\x00b`, `/usr/bin/x`, `C:\x.exe`. `IsWSLEntry` true for every `wsl://...` row above, false otherwise.
  - In `policy_local_wsl_test.go`, rework the helper `wslPolicy()` to `LocalPolicy{AllowedBinaries: []string{"/usr/bin/native", "wsl://Ubuntu-24.04/home/ai/venv/bin/vllm"}, WSL: WSLPolicy{Exe: ..., User: "ai"}}` and cover: permitted spec; distro not named -> refused, message contains `runtime_allowed_binaries` and no distro name; same path in another distro (`wsl://Debian/home/ai/venv/bin/vllm` listed, spec in `Ubuntu-24.04` with only that entry) -> refused; native-only allowlist -> WSL spec refused; malformed `WSL://Ubuntu-24.04/...` entry permits nothing; work_dir: no dir entries -> any absolute permitted, relative refused; `wsl://Ubuntu-24.04/srv/models/*` in `AllowedDirs` -> `/srv/models/a` ok, `/srv/other` refused; a `wsl://Debian/srv` dir entry does not restrict an `Ubuntu-24.04` spec; `AllowBinaryDirs` on -> `/home/ai/venv/bin/sub` ok, `/tmp` refused.
  - Native leg (existing `policy_local_test.go` or new cases): allowlist with only `wsl://` entries -> native spec refused with the "allowlist is empty" text; `AllowedDirs: ["wsl://Ubuntu-24.04/srv"]` alone -> native spec with work_dir `/anything` permitted (no native dir entries); native spec with work_dir `wsl://Ubuntu-24.04/srv/x` and `AllowedDirs: ["/srv", "wsl://Ubuntu-24.04/srv"]` -> refused.
  - Manager: `TestWSLPrewarmOnlyDistrosNamedBySpecs` / pre-warm test reworked so a distro named by a `wsl://` binary entry is warmed and one that is not is never passed to `wsl.exe -d`.
  - Config: `TestLoadRuntimeWSLSettings` reworked — `runtime_wsl_exe`/`runtime_wsl_user` still load; a file with `"runtime_wsl_allowed_binaries": ["/x"]` loads without error and `cfg.Warnings` has one entry containing `runtime_wsl_allowed_binaries` and `wsl://`; env `OP_AGENT_RUNTIME_WSL_ALLOWED_DISTROS=Ubuntu` likewise; `runtime_allowed_binaries: ["WSL://Ubuntu/x"]` yields a warning containing `runtime_allowed_binaries[0]`.
  - Version tests expect `0.19.1`.

- [ ] **Step 2: Run them and see them fail** — `cd server-agent && go test ./internal/runtime/ ./internal/config/ ./internal/agent/` (expected: compile errors / FAIL on the new cases).

- [ ] **Step 3: Implement** `wsl_allowlist.go`; rewrite `Permit`/`permitWSL` per D2 (split the lists per call; they are tiny); shrink `WSLPolicy` and `isZero`; derive the pre-warm distro check from `o.policy.AllowedBinaries` via a small helper on `LocalPolicy` (e.g. `wslDistroAllowed(distro string) bool`); remove the three config fields and env names, keep the old keys only as detection fields in `fileConfig` that feed `Warnings`; validate `wsl://` entries of both lists into `Warnings`; log `cfg.Warnings` in `main.go`; bump Version and its comment (`0.19.0 -> 0.19.1` for the unified allowlist).

- [ ] **Step 4: Generators and operator docs** — drop the three keys from the golden; extend the comments of `runtime_allowed_binaries` and `runtime_allowed_dirs` with the `wsl://<distro>/<absolute Linux path>` form and one example each; mirror byte-identically in the gateway template and the portal builder; update README rows and WSL2 section bullets and `config-env.md` (remove the three env vars, describe `wsl://` entries and the startup warning).

- [ ] **Step 5: Verify** — `cd server-agent && go test ./... && golangci-lint fmt --diff && GOTOOLCHAIN=go1.26.5 golangci-lint run ./...`; `cd gateway/backend && go test ./internal/gateway/ -run 'AgentConfig|Golden|Template' -skip 'TestVoice|TestLiveTranscriptionLoopback'` (use the real test names around `agent_binaries_test.go`); `cd gateway/frontend && npx vitest run src/components/AgentTokenSection.test.tsx && npm run format:check`; `make lint-docs` from the repo root. All green.

- [ ] **Step 6: Commit** — `feat(agent): wsl:// entries in the unified runtime allowlists`

### Task 2: Inventory lists `wsl://` entries; vLLM kind and `--version` probing

**Files:**
- Modify: `server-agent/internal/agent/binaries.go`, `server-agent/internal/agent/binaries_test.go`, `server-agent/internal/agent/agent.go:~1045` (constructor call)

**Interfaces:**
- Consumes: `runtimectl.IsWSLEntry`, `runtimectl.ParseWSLEntry` (Task 1); `cfg.RuntimeWSLExe`, `cfg.RuntimeWSLUser`.
- Produces: `const binaryKindVLLM = "vllm"`; `func parseVLLMVersion(out string) (version string, ok bool)`; `newBinaryInventory(allowed []string, wsl wslProbeConfig) *binaryInventory` with `type wslProbeConfig struct { Exe, User string }`; a test seam for wsl.exe calls shaped `func(ctx context.Context, exe string, args, env []string) ([]byte, error)` (field on `binaryInventory`, default runs the command with `exec.CommandContext`, `WaitDelay = binaryWaitDelay`, output capped by `limitedBuffer`). Restructure `scanBinariesCtx`'s `stat`/`runVersion` parameters as needed (e.g. a small prober struct); keep native behavior and existing test intent.

- [ ] **Step 1: Write failing tests** (fake wsl runner that records argv and answers by command)
  - `TestBinaryKindVLLM`: `/opt/venv/bin/vllm` and `C:\x\vllm.exe` -> `vllm`; `wsl://Ubuntu-24.04/home/ai/venv/bin/vllm` -> `vllm`; `wsl://Ubuntu-24.04/opt/llama/llama-server` -> `llama_server`.
  - `TestParseVLLMVersion`: `"0.11.0\n"` -> `0.11.0`; `"INFO 11-13 10:00:00 [__init__.py:216] Automatically detected platform cuda.\n0.11.1rc2.dev123+gabc1234\n"` -> `0.11.1rc2.dev123+gabc1234`; `"v0.6.3.post1\nWARNING something\n"` -> `0.6.3.post1`; `"Traceback ...\nModuleNotFoundError: No module named 'vllm'\n"` -> not ok.
  - `TestNativeVLLMIsProbedWithLongTimeout`: a native `vllm` path is run with `--version`; the context deadline handed to the runner is >= 50 s away; an `rpc-server` and an `other` binary are still never run (extend `TestOnlyLlamaServerIsExecuted` accordingly, renaming it if its name becomes false).
  - `TestWSLEntryProbedOnlyWhenDistroRuns`: allowed `wsl://Ubuntu-24.04/home/ai/venv/bin/vllm` and `wsl://Debian/opt/llama/llama-server`; the list call answers `"Ubuntu-24.04\r\n"`; expect argv `--list --running --quiet` once, then for Ubuntu `-d Ubuntu-24.04 -u ai --cd / --exec stat -L -c %s:%Y:%F -- /home/ai/venv/bin/vllm` and `-d Ubuntu-24.04 -u ai --cd / --exec /home/ai/venv/bin/vllm --version`; NO argv contains `Debian`; the inventory has both rows with `Path` verbatim, the Ubuntu row `Version` from the fake output, `VersionSource` `agent`; env of every call contains `WSL_UTF8=1`.
  - `TestWSLStoppedDistroKeepsLastVersion`: scan 1 with the distro running caches `0.11.0`; scan 2 with an empty running list makes no distro-targeted call and still reports `0.11.0`.
  - `TestWSLEntriesNotProbedWithoutExeOrUser`: Exe `""` (and separately user `root`) -> no runner call at all, rows listed.
  - `TestMalformedWSLEntryNotListed`: `WSL://Ubuntu-24.04/x` and `wsl://Ubuntu-24.04\x` are absent from the inventory.
  - `TestWSLStatCacheSkipsSecondProbe`: unchanged `size:mtime` from stat -> the `--version` call happens once across two scans.

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/agent/ -run 'Binar|VLLM|WSL'` — expected FAIL.

- [ ] **Step 3: Implement** per D4. Per-kind parser and timeout (`llama_server`: `parseLlamaVersion`, 10 s; `vllm`: `parseVLLMVersion`, `binaryVLLMVersionTimeout = 60 * time.Second`). WSL stat output `"<size>:<mtime-epoch>:<type>"` is regular when type is `regular file` or `regular empty file`. Running-list output: split lines, trim spaces/`\r`/NUL/BOM, compare case-insensitively. Wire `agent.go` with `wslProbeConfig{Exe: cfg.RuntimeWSLExe, User: cfg.RuntimeWSLUser}`; refused-user rule = `wslUserRefused` semantics (empty, `root`, all-zero UID) — duplicate the tiny check or export it from `runtime` (`runtimectl.WSLUserRefused`) if that is cleaner.

- [ ] **Step 4: Verify** `cd server-agent && go test ./... && golangci-lint fmt --diff && GOTOOLCHAIN=go1.26.5 golangci-lint run ./...` — green.

- [ ] **Step 5: Commit** — `feat(agent): inventory lists wsl:// binaries and versions vLLM`

### Task 3: Gateway accepts `vllm` and derives the execution of a binary row

**Files:**
- Modify: `gateway/backend/internal/portal/service_binaries.go` (kind const, `ServerBinaryDTO`, `serverBinaryDTO`, `validBinaryKind`, `validBinaryPath`), `gateway/backend/internal/gateway/agent_ingest_binaries.go` (kind switch); their tests (`service_binaries_test.go`, `agent_ingest_binaries_test.go` or the existing test files covering them)

**Interfaces:**
- Produces: `portal.BinaryKindVLLM = "vllm"`; JSON fields `execution` (`"native"|"wsl"`), `distro` (string, `""` native), `exec_path` (string) on every `/api/portal/servers/{id}/binaries` row; an unexported `parseWSLBinaryPath(p string) (distro, linuxPath string, ok bool)` with D1 rules and a comment naming `runtime.ParseWSLEntry` in the agent as its twin. If the gateway already has a WSL distro pattern for run specs (#381), reuse it.

- [ ] **Step 1: Write failing tests** — DTO of `/usr/bin/llama-server` -> execution `native`, distro `""`, exec_path = path; DTO of `wsl://Ubuntu-24.04/home/ai/venv/bin/vllm` -> `wsl`, `Ubuntu-24.04`, `/home/ai/venv/bin/vllm`; `validBinaryPath` accepts that `wsl://` path and rejects `wsl://Ubuntu-24.04`, `WSL://Ubuntu-24.04/x`, `wsl://-x/y`, `wsl://Ubuntu-24.04\x`; manual POST with kind `vllm` succeeds; ingest of kind `vllm` stores `vllm` (an unknown kind still becomes `other`).
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal/ ./internal/gateway/ -run 'Binar' -skip 'TestVoice|TestLiveTranscriptionLoopback'` — FAIL.
- [ ] **Step 3: Implement** per D5.
- [ ] **Step 4: Verify** `cd gateway/backend && go test ./internal/portal/ ./internal/gateway/ -skip 'TestVoice|TestLiveTranscriptionLoopback' && golangci-lint fmt --diff && GOTOOLCHAIN=go1.26.5 golangci-lint run ./...` — green.
- [ ] **Step 5: Commit** — `feat(gateway): vllm binary kind and derived execution of inventory rows`

### Task 4: Portal Binaries tab column and picker that sets mode, distro and path

**Files:**
- Modify: `gateway/frontend/src/api/binaries.ts`, `gateway/frontend/src/components/BinariesSection.tsx` (+ test), `gateway/frontend/src/components/shared/BinaryPicker.tsx` (+ test if one exists, else cover via the sections), `gateway/frontend/src/components/RuntimeAdminSection.tsx` (~L4990 picker use), `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`, `gateway/frontend/src/i18n.ts` (de and en), any test fixtures constructing `ServerBinary` objects (tsc finds them)

**Interfaces:**
- Consumes: Task 3 JSON (`execution`, `distro`, `exec_path`, kind `vllm`).
- Produces: `ServerBinaryKind` gains `'vllm'`; `ServerBinary` gains `execution: 'native' | 'wsl'; distro: string; exec_path: string`; `BinaryPicker` gains optional prop `onSelect?: (binary: ServerBinary) => void`, called when an option (not free text) is chosen; new i18n keys `binariesColExecution` (de "Ausführung", en "Execution") and `binariesKindVllm` ("vLLM" in both).

- [ ] **Step 1: Write failing tests**
  - Binaries tab: a WSL row renders "WSL2" and its distro in the execution column; a native row renders "Nativ" (de) / "Native" (en) via `runtimeSpecExecutionModeNative`; kind `vllm` renders "vLLM"; the manual-add kind select offers `vllm`.
  - Run-spec form: with inventory `[native llama-server, wsl://Ubuntu-24.04/home/ai/venv/bin/vllm (kind vllm, execution wsl)]`, opening the picker lists both; choosing the WSL option sets the binary input to `/home/ai/venv/bin/vllm`, the execution-mode select to `wsl` and the distro field to `Ubuntu-24.04`, and the saved spec carries them; choosing the native option afterwards sets mode `native`. Typing a free path leaves mode and distro unchanged.
  - Two rows with the same Linux path in `Ubuntu-24.04` and `Debian`: choosing the Debian row sets distro `Debian`.
  - RPC-member picker never lists a WSL row.
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/BinariesSection.test.tsx src/components/RuntimeAdminSection.test.tsx` — FAIL.
- [ ] **Step 3: Implement** per D6. In `BinaryPicker`, `getOptionLabel` returns `exec_path` for a WSL option (so MUI's input reset after a selection writes the Linux path, never the `wsl://` string), `isOptionEqualToValue` compares `path`, options for `kind="rpc_server"` exclude `execution === 'wsl'`, and `onChange` of the Autocomplete calls `onSelect` for a `ServerBinary` value. Treat a missing `execution` as native. In `RuntimeAdminSection`, `onSelect` sets binary to `exec_path`, mode, and (WSL) distro. Update `runtimeSpecWSLHint` (de/en) to name `wsl://<Distro>/<Pfad>` / `wsl://<distro>/<path>` entries in the agent's `runtime_allowed_binaries`.
- [ ] **Step 4: Verify** `cd gateway/frontend && npx vitest run src/components/BinariesSection.test.tsx src/components/RuntimeAdminSection.test.tsx src/components/RpcMemberSection.test.tsx && npm run lint && npm run format:check && npm run build` — green.
- [ ] **Step 5: Commit** — `feat(portal): execution column in the Binaries tab and a picker that fills WSL fields`

### Task 5: Architecture docs and ADR-102

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§3.1 table ~L121-131, "Binary inventory" ~L1837, §3.8 local policy ~L2199-2213 and "The portal", §15 config rows ~L10651-10658), `docs/architecture/09-architecture-decisions.md` (new ADR-102 after ADR-101, its index row if the file keeps one, and a "partly superseded by ADR-102" note on ADR-101 (b) ~L12437)

- [ ] **Step 1:** Describe D1–D6 where the docs describe the old three settings, the inventory and the picker; ADR-102 "One runtime allowlist with `wsl://` entries" — context (WSL binaries invisible in the inventory and picker because they lived in a separate list), decision (D1–D3, D4's never-boot rule), consequences (old keys warn instead of fail; gateway derives execution from the path, no migration; vLLM version detection).
- [ ] **Step 2: Verify** `make lint-docs` from the repo root — green; `grep -rn 'runtime_wsl_allowed' docs server-agent/README.md` shows only historical/ADR mentions that say they were removed.
- [ ] **Step 3: Commit** — `docs: unified runtime allowlist with wsl:// entries (ADR-102)`
