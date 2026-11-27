# Runtime log files (#430) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A runtime spec can declare `log_files`. The agent tails them under an agent-local path policy and keeps them in memory in per-stream rings, masking its own resolved secrets. The portal shows each file as a source tab next to the process output, and a failed start carries each file's tail in `last_error`.

**Architecture:** The agent gains a stream dimension in `LogStore` (process output = stream `""`, each declared file = its name). A poll-based `fileTailer` per generation and file writes into that store through a stream sibling of the generation's `procLog`. Every write passes through a per-generation `secretMasker`. Both wire paths are extended without new endpoints: `LogEntry.stream`, file events in the closed event set, and `last_error.log_file_tails`. The gateway persists `log_files` as an opaque JSON column like `config_files`, validates it at portal write time, and clamps on ingest. The portal adds a second tab row.

**Tech Stack:** Go 1.26 (agent `server-agent/`, gateway `gateway/backend/`), SQLite + Postgres stores, React + MUI + vitest (`gateway/frontend/`).

**Spec:** `docs/superpowers/specs/2026-11-27-runtime-log-files-design.md`. Read it before any task; section numbers below (§n) refer to it.

## Global Constraints

- Branch `feat/runtime-log-files`. Never commit to `main`. Merge `origin/main` before the version bump and before the PR.
- Agent version: MINOR bump `const Version` in `server-agent/internal/agent/agent.go` from `0.25.2` to `0.26.0` (or main's current MINOR + 1 at finalization). Feature `runtime_log_files`, `Since: "0.26.0"`.
- `log_files`: at most **4**. Name pattern `^[A-Za-z0-9._-]{1,32}$`, unique case-insensitively, `process` reserved (case-insensitive). Path non-empty, ≤ 1024 bytes, no `${API_TOKEN}` and no `${AGENT_ENV:`.
- New agent-local allowlist: file key `runtime_allowed_log_dirs`, env `OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS`. Empty means `${CONFIG_DIR}` only. It is never gateway-supplied.
- Tailer: poll **250 ms**, post-exit grace **2 s**, max line **16 KiB**, read rate **256 KiB/s** per file (= 64 KiB per tick).
- Masking: only `${API_TOKEN}` / `${AGENT_ENV:…}` values of this generation, ≥ **8** bytes, replaced by their placeholder (`secretSpan.mask`).
- `log_file_tails`: ≤ 4 entries, ≤ **2048** bytes each, RAM only on the gateway.
- Per-stream ring = `runtime_log_buffer_bytes`. The total `runtime_log_buffer_total_bytes` is a hard **byte** bound over all streams.
- Nothing is persisted anywhere: no disk, no DB, no log line carrying file or output content. Refusals name settings, never allowlist paths.
- File events (closed set): `file_opened`, `file_rotated`, `file_truncated`, `file_line_cut`, `file_stale`, `file_waiting`, `file_not_permitted`, `file_binary`, `file_missing`.
- Repo-facing text in English. Portal strings in German and English together (`src/i18n.ts`).
- Gates before PR: per Go module `golangci-lint fmt --diff` and `golangci-lint run`, plus `go test ./...` (includes archtest; run it yourself after any new package or import). Frontend: `npm test`, `npm run lint`, `npm run format:check`, `npm run build`. Gateway store changes need the Postgres leg: `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set as in `.github/workflows` (local postgres container).

## Review Focus

1. **A path that escapes after resolution** (symlink or junction inside `${CONFIG_DIR}` pointing at `/etc/shadow` or `C:\Users\x\.ssh`) must be refused, and must be refused again after a rotation that swaps the file for such a link. Tests are in Task 3 and Task 7.
2. **A secret split across two pipe reads or two file reads** must still be masked; the ring never holds it in clear. The test is in Task 6.
3. **A fast crash loop** (generation N+1 starts during N's 2 s grace): N's final lines still arrive, N+1 starts from offset 0 of its own fresh file, and N's tails attach to N's `last_error` only. The test is in Task 8.
4. **A server that never creates the file, or writes a huge binary blob**, shows `file_waiting` → `file_missing` or `file_binary`. It must not spin, flood the WS, or evict the process stream. Tests are in Task 7 and Task 5.
5. **An old agent with a new gateway or portal, and a new agent with an old gateway**: the dialog shows the process tab unchanged and the file tabs explain "agent too old". File events from a new agent reaching an old gateway degrade to empty plain entries, which render nothing. Tests are in Task 10 and Task 12.

---

## Phase 1 — Agent: spec field, policy, content paths

### Task 1: `log_files` on the agent spec

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (Spec :68, ParseConfig :561, near `checkConfigFileNames` :815, ParseErrorCode consts :431-478)
- Modify: `server-agent/internal/runtime/manager.go` (`sameLaunchShape` :2308 `strip`)
- Test: `server-agent/internal/runtime/types_log_files_test.go` (new), `server-agent/internal/runtime/manager_test.go` (sameLaunchShape case)

**Interfaces:**
- Produces: `type LogFile struct { Name string \`json:"name"\`; Path string \`json:"path"\` }`; `Spec.LogFiles []LogFile \`json:"log_files,omitempty"\``; `const maxLogFiles = 4`; `func validLogFileName(name string) bool`; `func checkLogFiles(spec Spec) error` returning `*ParseError{Code: ParseErrorInvalidLogFiles}`; `const ParseErrorInvalidLogFiles ParseErrorCode = "invalid_log_files"`.

- [ ] **Step 1: Write failing tests** `TestCheckLogFiles`, a table with these cases: ok (`engine`, `${CONFIG_DIR}/engine.log`); 5 entries → error; name `""`, `a/b`, 33 chars → error; `Process` → error (reserved); duplicate `Engine`/`engine` → error; path `""` → error; path with `${API_TOKEN}` → error; path with `${AGENT_ENV:X}` → error; path of 1025 bytes → error. Also `TestParseConfigRejectsInvalidLogFiles` (the whole document fails with `ParseErrorInvalidLogFiles`, as for config files), and `TestSameLaunchShapeIgnoresLogFiles` (two specs differing only in `LogFiles` are the same shape).
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime -run 'TestCheckLogFiles|TestParseConfigRejectsInvalidLogFiles|TestSameLaunchShapeIgnoresLogFiles' -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Call `checkLogFiles` in ParseConfig's per-spec loop right after `checkConfigFileNames`. Add `s.LogFiles = nil` in `sameLaunchShape`'s `strip`, with a comment citing §4. Add a comment that absolute-after-expansion is checked at tail time (Task 3), not here.
- [ ] **Step 4: Run the tests again** and confirm they PASS. Then run `go test ./internal/runtime -count=1` and confirm it PASSES.
- [ ] **Step 5: Commit** `feat(agent): log_files spec field, validation, not launch-affecting (#430)`.

### Task 2: `runtime_allowed_log_dirs` setting

**Files:**
- Modify: `server-agent/internal/config/config.go` (Config :241 area, File :495 area, Load :636, `runtimeAllowlistWarnings` / `allowlistEntryWarning` :822)
- Modify: `server-agent/internal/runtime/policy_local.go` (`LocalPolicy` :373)
- Modify: `server-agent/cmd/.../main.go` (where `LocalPolicy{...}` is built, ≈:177; count log line ≈:550)
- Test: `server-agent/internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.RuntimeAllowedLogDirs []string`; `File.RuntimeAllowedLogDirs []string \`json:"runtime_allowed_log_dirs"\``; `LocalPolicy.AllowedLogDirs []string`.

- [ ] **Step 1: Write failing tests** `TestLoadRuntimeAllowedLogDirs`: the file value is used; the env `OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS="/a, /b"` wins and becomes `["/a","/b"]`. `TestRuntimeAllowedLogDirsWarnings`: a relative entry and a malformed `wsl:` entry produce warnings naming `runtime_allowed_log_dirs`, with the same dir semantics as `runtime_allowed_dirs` (pass `isDirs` true for the new list too).
- [ ] **Step 2: Run** `go test ./internal/config -run 'AllowedLogDirs' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement** with `resolveStringList`. Add the list to the warnings loop and wire it into `LocalPolicy` in main.go. Log the entry count beside the `runtime_allowed_dirs` count, never the paths.
- [ ] **Step 4: Run** `go test ./internal/config ./... -count=1` and confirm it PASSES.
- [ ] **Step 5: Commit** `feat(agent): runtime_allowed_log_dirs agent-local allowlist (#430)`.

### Task 3: Log-file path policy

**Files:**
- Create: `server-agent/internal/runtime/logfile_policy.go`, `logfile_open_windows.go`, `logfile_open_other.go`
- Modify: `server-agent/internal/runtime/config_files.go` (factor the expansion loop of `expandConfigFileContent` :69 into a shared helper)
- Modify: `server-agent/internal/runtime/wsl.go` (inverse of `windowsPathToWSL` :224)
- Test: `server-agent/internal/runtime/logfile_policy_test.go`, `wsl_test.go`

**Interfaces:**
- Consumes: `LogFile`, `LocalPolicy.AllowedLogDirs`, `resolveNonSecretToken` (policy_local.go:1091), `withinDir` (:710), `withinPosixDir` (:635), `allowedDirBase` (:747), `splitAllowlist` (wsl_allowlist.go:9).
- Produces:
  - `func expandNonSecretText(text, what string, spec Spec, port int, vendor GPUVendor, vendors GPUVendors, configFilesDir string) (string, error)`. This is the shared loop; `what` goes into error messages ("config file content", "log file path"). `expandConfigFileContent` becomes a thin caller.
  - `func wslPathToWindows(p string) (string, bool)`: `/mnt/<drive>/rest` → `<DRIVE>:\rest`, false for anything else.
  - `type logFileTarget struct { Name string; OpenPath string; Roots []string; InConfigDir bool }`. `OpenPath` is the agent-side path to open; `Roots` are the resolved allowed roots it was matched against.
  - `var errLogFileNotPermitted = errors.New("runtime: log file not permitted")`
  - `func (p LocalPolicy) resolveLogFile(spec Spec, lf LogFile, port int, vendor GPUVendor, vendors GPUVendors, configFilesDir string) (logFileTarget, error)`. It expands the path, maps WSL paths (§5.4), checks Clean/absolute/containment against the instance config dir (`filepath.Join(abs(configFilesDir), spec.ID)`) plus `AllowedLogDirs`, refuses `/proc`, `/sys`, `/dev`, `\\.\`, `\\?\` and spec-named UNC paths, and refuses distro-internal WSL paths (always, in this cut: §5.4 fail closed). Errors wrap `errLogFileNotPermitted` with a message naming `runtime_allowed_log_dirs` / `OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS` and a count, never paths. Paths go to `slog.Warn` only.
  - `func openLogFile(target logFileTarget) (*os.File, error)`: platform files. Windows uses `CreateFile` with `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE`, read-only; other platforms use `os.Open` with `O_RDONLY|O_NONBLOCK` and then clear non-blocking. After opening it checks `Stat().Mode().IsRegular()` and the final resolved path (`filepath.EvalSymlinks`; on Windows `GetFinalPathNameByHandle`) against `target.Roots` (each root also EvalSymlinks'd). On failure it closes the file and returns a wrapped `errLogFileNotPermitted`.
  - `func fileIdentity(fi os.FileInfo) (uint64, uint64, bool)`: inode+dev, or volume serial + file index on Windows. Used by Task 7 for rotation.

- [ ] **Step 1: Write failing tests.**
  - `TestResolveLogFile` table, both separators on the Linux runner (model on `TestAllowedDirBase`, policy_local_test.go:335):
    - `${CONFIG_DIR}/e.log` → ok, `InConfigDir`.
    - `${CONFIG_DIR}/../x.log` → not permitted.
    - `/var/log/x.log` with empty list → not permitted.
    - `/var/log/x.log` under `/var/log/*` → ok.
    - `/proc/self/environ` under `/*` → not permitted.
    - relative `x.log` → not permitted.
    - `\\server\share\x.log` → not permitted.
    - a WSL spec `${CONFIG_DIR}/e.log` → `OpenPath` is the agent-side native config dir path.
    - a WSL spec `/mnt/c/logs/x.log` checked against native entry `C:\logs\*` → ok, with `OpenPath` `C:\logs\x.log`.
    - a WSL spec `/home/u/x.log` with `wsl://Ubuntu/home/u/*` → not permitted (fail closed).
    - `${API_TOKEN}` in the path → error.
    - The error text never contains an allowlist path.
  - `TestOpenLogFileRefusesSymlinkEscape`: in a temp `${CONFIG_DIR}`, `e.log` is a symlink to a file outside → refused. A symlink to a file inside → ok. A directory → refused. A FIFO (`syscall.Mkfifo`, skipped on Windows) → refused without blocking.
  - `TestOpenLogFileRootIsSymlink`: the allowed root is itself a symlink → a file under it is ok.
  - `TestWSLPathToWindows`: `/mnt/c/a/b` → `C:\a\b`; `/mnt/c` → `C:\`; `/home/x` → false; `/mnt/cc/x` → false.
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'TestResolveLogFile|TestOpenLogFile|TestWSLPathToWindows' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement** the signatures above. Keep `expandConfigFileContent`'s behaviour identical; the existing config-files tests must stay green.
- [ ] **Step 4: Run** `go test ./internal/runtime -count=1` and `GOOS=windows go vet ./internal/runtime`, and confirm both PASS.
- [ ] **Step 5: Commit** `feat(agent): log-file path policy with resolved-path containment (#430)`.

### Task 4: `${CONFIG_DIR}` with forward slashes in config-file content on Windows

**Files:**
- Modify: `server-agent/internal/runtime/config_files.go` (the content caller of `expandNonSecretText`)
- Test: `server-agent/internal/runtime/config_files_test.go`

**Interfaces:**
- Produces: a package seam `var contentPathsUseSlashes = runtime.GOOS == "windows"` (reuse an existing GOOS seam in the package if there is one). When it is true and `!spec.IsWSL()`, the `CONFIG_DIR` value substituted into **content** has every `\` replaced by `/`. Args and env are unchanged.

- [ ] **Step 1: Write failing test** `TestConfigDirInContentUsesSlashesOnWindows`. Set the seam to true, use configFilesDir `C:\agent\cfg`, spec `strata`, content `{"log":"${CONFIG_DIR}/engine.log"}`. Expect `json.Valid` to be true and the value to be `C:/agent/cfg/strata/engine.log`. Also `TestConfigDirInArgsKeepsNativeForm`: the same seam, and `expandSpec` args keep the backslashes.
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'ConfigDirIn' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** Add a doc comment citing §4.1 and why (JSON escapes).
- [ ] **Step 4: Run** `go test ./internal/runtime -count=1` and confirm it PASSES.
- [ ] **Step 5: Commit** `fix(agent): ${CONFIG_DIR} in config-file content uses forward slashes on Windows (#430)`.

## Phase 2 — Agent: per-stream retention

### Task 5: Stream dimension in `LogStore`

**Files:**
- Modify: `server-agent/internal/runtime/logs.go` (all of LogStore/specLog/logRecord/LogEntry/Drain/replay/tail)
- Test: `server-agent/internal/runtime/logs_test.go`, `logs_streams_test.go` (new)

**Interfaces:**
- Consumes: today's API (see the file). Callers outside logs.go must keep compiling unchanged.
- Produces:
  - `logRecord.stream string` (`""` = process output).
  - `LogEntry.Stream string \`json:"stream,omitempty"\``.
  - `type streamRing struct { recs []logRecord; bytes int; dropped int64; cap int }`; `specLog` holds `streams map[string]*streamRing` plus `lastWrite`.
  - LogStore: `maxSpecs` is replaced by `total int` and `used int` (bytes over all rings). `pending`, `pendingBytes` and `pendingDropped` are keyed by `type streamKey struct{ spec, stream string }`.
  - `func (p *procLog) ForStream(name string) *procLog`: a sibling with the same store, specID, gen, instance and prefix, its own `midLine`, and `stream = name`.
  - `func (s *LogStore) gap(p *procLog, n int64)`: counts `n` skipped bytes on p's stream (ring `dropped` and, if watched, pending dropped).
  - `func (p *procLog) Mark(event string)`: a typed marker on p's stream (pid 0, no command).
  - `func (s *LogStore) tailStream(specID, stream string, gen uint64, n int) string`. `tail()` becomes `tailStream(specID, "", gen, n)`.
  - Event consts: `logEventFileOpened = "file_opened"`, `logEventFileRotated = "file_rotated"`, `logEventFileTruncated = "file_truncated"`, `logEventFileLineCut = "file_line_cut"`, `logEventFileStale = "file_stale"`, `logEventFileWaiting = "file_waiting"`, `logEventFileNotPermitted = "file_not_permitted"`, `logEventFileBinary = "file_binary"`, `logEventFileMissing = "file_missing"`.
- Rules (§6, the builder calls in §6 and §7):
  - Each stream ring is trimmed against its own `cap`. The process ring cap is `perSpec`. A file ring cap is `min(perSpec, max(minLogBufferBytes, (total-perSpec)/4))`.
  - `NewLogStore` raises the total to at least `perSpec + 4*minLogBufferBytes` and reports the effective capacity through `Capacity()`.
  - `evictLocked` drops whole unwatched specs (all streams), oldest first, while `used > total`. It never drops `keep` or a watched spec.
  - Coalescing only joins records of the same stream and gen.
  - `Drain` iterates sorted watched `streamKey`s round-robin, at most one batch per unit. The `logDrainBudget > logPendingBytes` invariant holds per unit.
  - A replay is **one** chunk sequence per spec: process stream first, then file streams by name. Each stream's ring `dropped` is stamped as a gap entry carrying that stream. A gap entry always carries its stream.

- [ ] **Step 1: Write failing tests** in `logs_streams_test.go`, using `newTestLogStore`:
  - `TestStreamRingsDoNotEvictEachOther`: perSpec 64 KiB; write 1 MiB to stream `engine`, then 1 KiB to process; process text is intact, and `engine` reports `dropped_bytes` > 0 on a gap entry with `Stream:"engine"`.
  - `TestTotalIsAByteBoundOverStreams`: total 256 KiB, perSpec 64 KiB, 3 specs × 2 full streams; `used <= total` after each write; the oldest unwatched spec is evicted.
  - `TestTotalRaisedToFitFileRings`: `NewLogStore(1<<20, 1<<20)` gives `Capacity()` total `1<<20 + 4*64<<10`.
  - `TestTailIgnoresFileStreams`: same gen, file text present; `tail(spec, gen, 2048)` has process text only.
  - `TestDrainIsPerStreamUnit`: a watched spec with a full `engine` queue (64 KiB) and a process queue; one `Drain` yields a process batch and an `engine` batch; process has no dropped bytes.
  - `TestReplayCoversAllStreamsInOrder`: the replay chunks in order are process entries, then `engine` entries; `Scrollback` is set only on the first; entries carry `Stream`.
  - `TestCoalesceKeepsStreamsApart`.
  - Existing logs_test.go tests must still pass unchanged (update `TestLogCapacityClamping` / `TestLogTotalCeilingEvictsSpecBuffers` only where the raised-total rule changes the expected number, with a comment).
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'Stream|Tail|Drain|Replay|Coalesce|Log' -count=1`. Expected: the new tests FAIL.
- [ ] **Step 3: Implement.** Keep `logs.go`'s explanatory comments current, especially the §14.3 worst-case paragraph and the drain-budget invariant paragraph.
- [ ] **Step 4: Run** `go test ./internal/runtime -count=1` and confirm it PASSES. Also run `-race` on the logs tests.
- [ ] **Step 5: Commit** `feat(agent): per-stream log rings with a hard byte total (#430)`.

## Phase 3 — Agent: masking

### Task 6: Mask the generation's own secrets in all streams

**Files:**
- Create: `server-agent/internal/runtime/secret_mask.go`
- Modify: `server-agent/internal/runtime/logs.go` (`procLog`, `write`), `manager.go` (`startProcess` between :4760 and :4795; `onProcExited`), `command.go` (the "CHILD's own output" comment ≈:103)
- Test: `server-agent/internal/runtime/secret_mask_test.go`, `manager_logs_test.go`

**Interfaces:**
- Consumes: `expandedSpec{args, env, argSpans, envSpans}` (policy_local.go:1060), `secretSpan.mask`.
- Produces:
  - `type secretMasker struct { pairs []maskPair; maxLen int }`, with `type maskPair struct{ value, mask string }`, sorted longest value first, deduplicated, values < 8 bytes skipped.
  - `func newSecretMasker(ex expandedSpec) *secretMasker` (nil-safe, so a nil masker masks nothing).
  - `func (m *secretMasker) mask(carry, b []byte, final bool) (out string, keep []byte)`. It joins carry+b and replaces every value with its mask. Unless `final` is set, it keeps the last `maxLen-1` bytes, never cutting inside a match.
  - `procLog.masker *secretMasker`, `procLog.carry []byte`; `func (s *LogStore) newInstanceProcMasked(specID string, k int, m *secretMasker) *procLog` (or a setter `p.setMasker(m)`). `ForStream` siblings share the masker and have their own carry.
  - `func (p *procLog) Flush()`: writes the remaining carry with `final=true`. It is called before `Exited` and by the tailer at stop.
  - Order in `write`: mask, then the instance prefix, then append.

- [ ] **Step 1: Write failing tests.**
  - `TestSecretMaskerMasks`: value `hf_abcdefgh123` → `${AGENT_ENV:HF_TOKEN}`. A 7-byte value is left as is. A plain env value (no span) is untouched. With two overlapping values the longest wins.
  - `TestSecretMaskerAcrossWrites`: `"tok=hf_abcd"` + `"efgh123\n"` → the ring holds `tok=${AGENT_ENV:HF_TOKEN}\n` and never the clear value (assert over the ring records, not only Drain).
  - `TestManagerMasksSecretInChildOutput`: the stub child prints its `--api-key` value (spec arg `${API_TOKEN}`); scrollback shows `${API_TOKEN}`, and `LastError.StderrTail` does not contain the token. Model on `TestManagerLastErrorStderrTailStillWorks` (manager_logs_test.go:264).
- [ ] **Step 2: Run** `go test ./internal/runtime -run 'SecretMasker|MasksSecret' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** Build the masker from `ex` in `startProcess` and attach it to `plog`. Call `proc.log.Flush()` in `onProcExited` before `Exited`. Rewrite the command.go bullet per §8 ("literal occurrences of the agent's own resolved secrets are masked, nothing else"), including what it does not catch.
- [ ] **Step 4: Run** `go test ./internal/runtime -count=1` and confirm it PASSES.
- [ ] **Step 5: Commit** `feat(agent): mask the generation's own resolved secrets in log output (#430)`.

## Phase 4 — Agent: tailer and manager integration

### Task 7: `fileTailer`

**Files:**
- Create: `server-agent/internal/runtime/logfile_tailer.go`
- Test: `server-agent/internal/runtime/logfile_tailer_test.go`

**Interfaces:**
- Consumes: `logFileTarget`, `openLogFile`, `fileIdentity` (Task 3); `procLog.ForStream`, `Mark`, `Flush`, `LogStore.gap` (Tasks 5–6).
- Produces:
  ```go
  type tailerConfig struct {
      Target     logFileTarget
      Log        *procLog      // stream sibling (ForStream(Target.Name))
      StartAtEOF bool          // external path: true; ${CONFIG_DIR}: false
      Interval   time.Duration // 250 ms
      Grace      time.Duration // 2 s
      TickBytes  int           // 64 KiB = 256 KiB/s at 250 ms
      MaxLine    int           // 16 KiB
      Open       func(logFileTarget) (*os.File, error) // openLogFile; tests may wrap it
  }
  func startFileTailer(cfg tailerConfig) *fileTailer
  func (t *fileTailer) Stop() <-chan struct{} // starts the grace; closed when the last read is done and Flush ran
  ```
- Behaviour (§7):
  - Mark `file_waiting` once while the file is absent.
  - Mark `file_opened` on the first open, reading from 0 or from the EOF at start.
  - Size below the offset → `file_truncated` and read from 0.
  - Identity change → read the old handle to EOF, reopen through `Open` (re-checking policy), `file_rotated`.
  - A refused open → `file_not_permitted` and stop.
  - Backlog above `TickBytes` → skip the excess, count it with `gap`.
  - A line longer than `MaxLine` → write the first `MaxLine` bytes, mark `file_line_cut`, discard to the next `\n`.
  - Invalid UTF-8 → U+FFFD; control bytes other than `\t\n\r` → `\xNN` escapes.
  - Binary heuristic (your call; document it) → `file_binary` and stop.
  - Never opened by the end of the grace → `file_missing`.
  - One goroutine per tailer, no busy loop.

- [ ] **Step 1: Write failing tests** with temp files and `Interval: 5ms, Grace: 50ms`:
  - `TestTailerAppend`
  - `TestTailerStartAtEOFSkipsOldContent`
  - `TestTailerMissingThenCreated` (`file_waiting` then `file_opened`)
  - `TestTailerMissingForever` (`file_missing` after Stop)
  - `TestTailerTruncate`
  - `TestTailerRotateRename` (rename plus a new file, old tail lines kept)
  - `TestTailerRotateToEscapingSymlinkIsRefused` (Review Focus 1)
  - `TestTailerLongLineCut`
  - `TestTailerBinaryStops`
  - `TestTailerRateCapCountsDropped` (write 1 MiB in one go; the ring receives ≤ TickBytes per tick and `dropped_bytes` covers the rest)
  - `TestTailerGraceCatchesFinalLines` (write after Stop, inside the grace → present)
  - `TestTailerMasksSecrets` (masker attached; a secret split across two writes)
- [ ] **Step 2: Run** `go test ./internal/runtime -run TestTailer -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/runtime -run TestTailer -count=3 -race` and confirm it PASSES (no flake).
- [ ] **Step 5: Commit** `feat(agent): poll-based log-file tailer (#430)`.

### Task 8: Manager integration, `last_error.log_file_tails`, feature flag, version

**Files:**
- Modify: `server-agent/internal/runtime/manager.go` (`startProcess` :4683, `runningProc` :1270, `onProcExited`, a new owner command), `config_files.go` (config dir creation without config files), `types.go` (LastError :905)
- Modify: `server-agent/internal/sample/sample.go` (:129), `server-agent/internal/agent/agent.go` (collectOnce :2767, `const Version` :506 + bump paragraph), `server-agent/internal/agent/features.go`
- Test: `server-agent/internal/runtime/manager_log_files_test.go` (new), `manager_logs_test.go`, `internal/agent/features_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 5–7.
- Produces:
  - `type LogFileTail struct { Name string \`json:"name"\`; Tail string \`json:"tail"\` }` and `LastError.LogFileTails []LogFileTail \`json:"log_file_tails,omitempty"\``.
  - `sample.LogFileTailSample{Name, Tail}` and `RuntimeErrorSample.LogFileTails []LogFileTailSample \`json:"log_file_tails,omitempty"\``, copied in collectOnce.
  - `func prepareLogFiles(spec Spec, pol LocalPolicy, port int, vendor GPUVendor, vendors GPUVendors, configFilesDir string) (targets []logFileTarget, refused []string, stale map[string]bool)`. It resolves each entry. If any target is `InConfigDir` it creates the instance config dir (0755). It removes each `InConfigDir` file; a removal failure other than not-exist marks it `stale`.
  - `runningProc.tailers []*fileTailer`.
  - The owner command `cmdLogFileTails{ key string; le *LastError; tails []LogFileTail }`. The handler replaces `st.lastError` with a copy carrying the tails **only if** `st.lastError == c.le`.
  - `FeatureRuntimeLogFiles = "runtime_log_files"` with `Since: "0.26.0"`; `Version = "0.26.0"`.
- Rules:
  - Call `prepareLogFiles` after `writeSpecConfigFiles` and before exec. After `plog.Started`, start one tailer per target (`StartAtEOF = !InConfigDir || stale[name]`, and mark `file_stale` if stale). Mark `file_not_permitted` on each refused name's stream.
  - On exit (any kind), Stop every tailer.
  - On the non-intentional path, after `recordFailure`/`setTerminalVerdict` has set `st.lastError`, capture `le := st.lastError` and start a goroutine that waits for every tailer's `Stop()` channel, then posts `cmdLogFileTails` with `tailStream(..., name, gen, 2048)` for each name that had text.
  - The next generation never waits for that goroutine.

- [ ] **Step 1: Write failing tests** in `manager_log_files_test.go`, using the stubchild (extend it with a flag that appends lines to a path and then exits non-zero, if it has none):
  - `TestManagerTailsConfigDirLogFile`: scrollback has `Stream:"engine"` text.
  - `TestManagerCreatesConfigDirWithoutConfigFiles`.
  - `TestManagerRemovesConfigDirLogBeforeExec`: the second generation's stream starts fresh.
  - `TestManagerLogFileTailsInLastError`: a crash, then eventually `LogFileTails[0].Name=="engine"`, the tail ends with the last line and is ≤ 2048 bytes; `StderrTail` has no engine text.
  - `TestManagerLogFileTailsNotAttachedToALaterError`: `last_error` replaced before the grace ends → the tails are dropped.
  - `TestManagerFastCrashLoopKeepsGenerationsApart` (Review Focus 3): backoff shrunk via `shrinkTimings`; N's last line is in N's tails, N+1's stream starts with `file_opened` for N+1.
  - `TestManagerRefusedLogFileMarksNotPermitted`.
  - `TestManagerLogFileRetentionIsNotWrittenToDisk`: model on :176; the needle is written by the child into the tailed file; WalkDir finds it only in that file itself.
  - `TestAgentFeaturesDeclaresRuntimeLogFiles`.
- [ ] **Step 2: Run** `go test ./internal/runtime ./internal/agent -run 'LogFile|RuntimeLogFiles' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** Merge `origin/main` first and base the version on main's current value. Add the version-bump paragraph in agent.go's comment block.
- [ ] **Step 4: Run** `cd server-agent && go test ./... -count=1 && golangci-lint fmt --diff && golangci-lint run` and confirm all PASS. Run the linux-tagged tests in the local `golang:1.26` container if runtime code with build tags changed.
- [ ] **Step 5: Commit** `feat(agent): tail declared log files per generation; log_file_tails in last_error; agent 0.26.0 (#430)`.

## Phase 5 — Gateway

### Task 9: `log_files` in the gateway spec model

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (migration **118** `runtime_spec_log_files`, after 117 :152), `store/sqlite_runtime.go` (the config_files column sites :45, 60, 94, 121, 137, 495), the Postgres store equivalent, `routing/store.go` (`RuntimeSpec` :3076)
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`LogFileDTO`, `PutRuntimeSpecRequest` :706, `RuntimeSpecDTO` :494, `AgentRuntimeSpecDTO` :3354, `agentRuntimeSpecDTO` :3921, `runtimeSpecFields` :1326, `validateRuntimeSpecRequest` :1360)
- Modify: `gateway/backend/internal/gateway/agent_runtime.go` (`agentRuntimeReportSpec` :383 mirror), `gateway/agent_features.go` (:88)
- Test: `store/migration118_runtime_spec_log_files_test.go`, `portal/service_runtime_log_files_test.go`, `gateway/portal_runtime_endpoints_test.go`, `gateway/agent_features_test.go`, `gateway/agent_runtime_report_ingest_test.go`

**Interfaces:**
- Produces:
  - Column `agent_runtime_specs.log_files text not null default ''`, via `addColumnIfMissing`.
  - `routing.RuntimeSpec.LogFiles string` (opaque JSON).
  - `portal.LogFileDTO{ Name string \`json:"name"\`; Path string \`json:"path"\` }`.
  - `PutRuntimeSpecRequest.LogFiles []LogFileDTO \`json:"log_files,omitempty"\``; absent clears, as with config_files.
  - `RuntimeSpecDTO.LogFiles []LogFileDTO \`json:"log_files"\`` (always non-nil).
  - `AgentRuntimeSpecDTO.LogFiles []LogFileDTO \`json:"log_files,omitempty"\`` (omitempty, so documents without it hash as before).
  - `const runtimeSpecLogFilesMax = 4`.
  - `var ErrRuntimeSpecLogFilesInvalid = errors.New("runtime_spec.log_files_invalid")`.
  - `func validateRuntimeSpecLogFiles(files []LogFileDTO) (string, error)`. It is appended **last** in `validateRuntimeSpecRequest`'s order. Rules: the Global Constraints, plus the path must start with `${CONFIG_DIR}` or pass `isAbsPOSIXBinaryPath` / `isAbsWindowsBinaryPath`; secret placeholders are matched with `runtimeSpecConfigFileSecretPlaceholders`. Messages name the entry index or name and never quote the path.
  - `func decodeRuntimeSpecLogFiles(stored string) ([]LogFileDTO, error)`.
  - `const runtimeLogFilesFeature = "runtime_log_files"` appended to `gatewayAgentFeatures`.
  - `agentRuntimeReportSpec.LogFiles` (non-secret mirror).

- [ ] **Step 1: Write failing tests.**
  - `TestMigration118AddsRuntimeSpecLogFiles` (model on migration 93's test).
  - `TestValidateRuntimeSpecLogFiles` table (every Global Constraints rule, `${CONFIG_DIR}/e.log` ok, `C:\logs\x.log` ok, `/var/log/x` ok, `logs/x` rejected).
  - `TestPutRuntimeSpecPersistsLogFiles` and `TestRuntimeSpecReadBackCarriesLogFiles`.
  - `TestAgentRuntimeSpecDTOCarriesLogFiles`, plus omitted when empty.
  - `TestHandlePortalMappingRuntimeSpecPutBadLogFilesReturns400`.
  - `TestAgentFeaturesDeclaresRuntimeLogFiles`.
  - The existing `TestAgentRuntimeReportSpecMirrorsEveryNonSecretDTOField` keeps passing because the mirror field was added.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store ./internal/portal ./internal/gateway -run 'LogFiles|Migration118|RuntimeLogFiles|Mirrors' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./... -count=1` with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (postgres container as in CI), then `golangci-lint fmt --diff && golangci-lint run`. Confirm all PASS.
- [ ] **Step 5: Commit** `feat(gateway): log_files runtime-spec field, validation, migration 118 (#430)`.

### Task 10: Relay of streams, file events and `log_file_tails`

**Files:**
- Modify: `gateway/backend/internal/gateway/runtime_logs.go` (`RuntimeLogEntryDTO` :153, `runtimeLogEvents` :674, `ingestRuntimeLog` :758, `runtimeLogSub` :259 and its `deliver`/`stampedLocked`/`take`)
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`agentRuntimeError` :153, mapping :665), `gateway/runtime_registry.go` (`RuntimeErrorDTO` :27)
- Test: `gateway/runtime_logs_test.go`, `runtime_logs_viewer_test.go`, `agent_runtime_status_ingest_test.go`

**Interfaces:**
- Produces:
  - `RuntimeLogEntryDTO.Stream string \`json:"stream,omitempty"\``.
  - `func validLogStreamName(s string) bool` (pattern `^[A-Za-z0-9._-]{1,32}$`).
  - `var runtimeLogFileEvents = map[string]bool{...9 names...}`.
  - Ingest: an entry whose non-empty `Stream` fails `validLogStreamName` is **dropped**. A file event on an entry without a stream degrades like an unknown event. A process event on a stream entry degrades too.
  - `runtimeLogSub.dropped` becomes `map[string]int64` under `mu`, keyed by stream. Overflow accounting sums text per stream. `stampedLocked` and `take` emit one gap entry per stream with a non-zero count (`Stream` set, `DroppedBytes`).
  - `type RuntimeLogFileTailDTO struct { Name string \`json:"name"\`; Tail string \`json:"tail"\` }`, used in both `agentRuntimeError.LogFileTails` and `RuntimeErrorDTO.LogFileTails` (`omitempty`).
  - `func clampRuntimeLogFileTails(in []RuntimeLogFileTailDTO) []RuntimeLogFileTailDTO`: ≤ 4 entries, invalid names dropped, each tail run through `clampRuntimeStderrTail`.

- [ ] **Step 1: Write failing tests.**
  - `TestRuntimeLogStreamIsRelayed`.
  - `TestRuntimeLogInvalidStreamIsDropped` (33 chars, `a/b`).
  - `TestRuntimeLogFileEventsAllowedOnlyOnStreams`.
  - `TestRuntimeLogGapIsPerStream`: a slow subscriber gets separate gap entries for `""` and `engine`.
  - `TestRuntimeLogFileStreamNeverReachesDiskOrDatabase`: extend the needle test (:495) with a `Stream:"engine"` entry and a `last_error.log_file_tails` needle.
  - `TestIngestTelemetrySampleRuntimeClampsLogFileTails`: 5 entries → 4; a 4 KiB tail → 2048; a bad name is dropped.
- [ ] **Step 2: Run** `go test ./internal/gateway -run 'RuntimeLog|LogFileTails' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** Update the allow-list comment (:665-673) with the file events.
- [ ] **Step 4: Run** `go test ./... -count=1 && golangci-lint fmt --diff && golangci-lint run` and confirm all PASS.
- [ ] **Step 5: Commit** `feat(gateway): relay log streams, file events and log_file_tails (#430)`.

## Phase 6 — Portal

### Task 11: API types, `log_files` editor, i18n

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeSpec` :212, `RuntimeLogEntry` :720, `RuntimeError` :557)
- Create: `gateway/frontend/src/components/shared/logFiles.ts`, `logFiles.test.ts`
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (state ≈:2885, load ≈:3085, handlers ≈:3207-3289, `buildSpecBody` :3472, JSX after the config-files block ≈:5510, `emptySpec` :345)
- Modify: `gateway/frontend/src/i18n.ts` (de ≈:1618-1650, en ≈:5841-5870)
- Test: `RuntimeAdminSection.test.tsx` (new describe `RuntimeAdminSection log files editor (#430)`), `i18n.test.ts`

**Interfaces:**
- Produces:
  - `export interface LogFile { name: string; path: string }`; `RuntimeSpec.log_files?: LogFile[]`; `RuntimeLogEntry.stream?: string`; `RuntimeError.log_file_tails?: { name: string; tail: string }[]`.
  - `export const LOG_FILES_MAX = 4`; `export type LogFileIssue = 'name_blank' | 'name_invalid' | 'name_reserved' | 'name_duplicate' | 'path_blank' | 'path_secret' | 'path_not_absolute'`; `export function logFileIssues(files: readonly LogFile[]): (LogFileIssue | null)[]`. Rules mirror Task 9; the later duplicate row is flagged.
  - Editor state `logFiles: LogFileRow[]` (rowKey/name/path), with `addLogFile`, `removeLogFile` and `updateLogFile`. `buildSpecBody` sends `log_files`. The submit error comes from `logFilesError()`, like `configFilesError()`.
  - A warning when `logFiles.length > 0 && reportReady && !agentFeatures.includes('runtime_log_files')`. Saving still works.
  - i18n keys (de + en): `runtimeSpecLogFiles`, `runtimeSpecLogFilesHint` (names `${CONFIG_DIR}/…` and the allowed placeholders, and that `${CONFIG_DIR}` in config files is written with `/` on Windows), `runtimeSpecLogFileName`, `runtimeSpecLogFilePath`, `runtimeSpecLogFileAdd`, `runtimeSpecLogFileRemove`, one key per `LogFileIssue`, and `runtimeSpecAgentNoLogFiles` (names the feature and agent ≥ 0.26.0).

- [ ] **Step 1: Write failing tests.** In `logFiles.test.ts`: every issue. In `RuntimeAdminSection.test.tsx`: round trip (load a spec with `log_files` → edit → PUT body carries it); the add button is disabled at 4; an inline error for `process`; the agent-too-old warning appears with `makeReport({ agent_features: ['runtime_manager'] })` and saving is still possible. The i18n test covers "defines all log-files keys in de and en".
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/shared/logFiles.test.ts src/components/RuntimeAdminSection.test.tsx -t "log files"`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test && npm run lint && npm run format:check && npm run build` and confirm all PASS.
- [ ] **Step 5: Commit** `feat(portal): log_files spec editor (#430)`.

### Task 12: Live-log source row, file statuses, `last_error` sections

**Files:**
- Modify: `gateway/frontend/src/components/RuntimeLogView.tsx` (props :577, derived values :677-689, empty-state :700-716, `LogLine` :331, tabs JSX :722-740)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (`logSpec` state :2265, `statusActions` :4721, mount :6801, `renderLastError` :4434)
- Modify: `gateway/frontend/src/i18n.ts` (runtimeLogs* keys ≈:1788 / :6004, `runtimeLastError*` ≈:1706 / :5924)
- Test: `RuntimeLogView.test.tsx`, `RuntimeAdminSection.test.tsx`

**Interfaces:**
- Consumes: Task 11 types; `RuntimeSpec.log_files` via `specByRuntimeId.get(row.spec_id)`; `agentFeatures`.
- Produces:
  - New `RuntimeLogView` props: `logFiles?: readonly string[]` (declared names, in order), `logFilesSupported?: boolean` (default true).
  - Internal state `source: string` (`''` = process, otherwise a declared name), kept across instance switches. The instance tab is kept across source switches. `source` resets to `''` when the dialog opens.
  - A pure helper `export function streamView(entries: readonly RuntimeLogEntry[], source: string): RuntimeLogEntry[]` keeps entries with `(e.stream ?? '') === source`. It runs **before** `allView` / `instanceView`.
  - A pure helper `export function fileStatus(entries: readonly RuntimeLogEntry[]): 'waiting' | 'not_permitted' | 'binary' | 'missing' | null` returns the last status event after the last `file_opened`/`file_waiting` in the shown entries.
  - The source row renders only when `logFiles.length > 0`. It is a second MUI `Tabs` below the instance row: `t.runtimeLogsSourceProcess` and one tab per name, with `aria-label={t.runtimeLogsSourceLabel}`.
  - In a file tab the notice priority follows today's, then: `!logFilesSupported` → `t.runtimeLogsFileUnsupported`; a `fileStatus` sentence; empty → `t.runtimeLogsFileEmpty`.
  - `LogLine` renders the file markers with `── label ──`, like process markers.
  - `renderLastError` adds, below stderr, one caption `t.runtimeLastErrorLogFile(name)` + `<Box component="pre">` per `log_file_tails` entry.
  - i18n (de + en): `runtimeLogsSourceLabel` ('Quelle'/'Source'), `runtimeLogsSourceProcess` ('Prozess'/'Process'), `runtimeLogsFileOpened`, `runtimeLogsFileRotated`, `runtimeLogsFileTruncated`, `runtimeLogsFileLineCut`, `runtimeLogsFileStale`, `runtimeLogsFileWaiting`, `runtimeLogsFileNotPermitted` (names `runtime_allowed_log_dirs`, no paths), `runtimeLogsFileBinary`, `runtimeLogsFileMissing`, `runtimeLogsFileUnsupported` (names `runtime_log_files`), `runtimeLogsFileEmpty`, and `runtimeLastErrorLogFile: (name: string) => string`.

- [ ] **Step 1: Write failing tests** in `RuntimeLogView.test.tsx`, inside the existing de/en loop and extending `renderLogView` with `logFiles` / `logFilesSupported`:
  - no `logFiles` → no source row, and the dialog is identical to today (an existing snapshot-style assertion, or the tab count);
  - `logFiles=['engine']` → two source tabs; `engine` shows only stream entries;
  - with instances: the source is kept when switching to `Instanz 1`, and the instance is kept when switching source;
  - under `Gesamt` the engine lines of instance 1 carry `[#1] `;
  - status sentences for waiting, not_permitted, binary and missing;
  - `logFilesSupported=false` → the source row is shown and the unsupported sentence appears in the engine tab (Review Focus 5);
  - a gap entry with `stream:'engine'` shows only in the engine tab.

  In `RuntimeAdminSection.test.tsx`: `last_error.log_file_tails` renders a section per file, as text; the log view receives `logFiles` from the spec.
- [ ] **Step 2: Run** `npx vitest run src/components/RuntimeLogView.test.tsx src/components/RuntimeAdminSection.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.** Update the RuntimeLogView header comment (the tabs paragraph) to describe the two rows.
- [ ] **Step 4: Run** `npm test && npm run lint && npm run format:check && npm run build` and confirm all PASS.
- [ ] **Step 5: Commit** `feat(portal): source tabs for declared log files in the live log (#430)`.

### Task 13: Strata import and template

**Files:**
- Modify: `gateway/frontend/src/components/shared/strataRunConfig.ts` (`StrataImport` :9, the key loop :108)
- Modify: `gateway/frontend/src/components/shared/strataTemplate.ts` (`RuntimeSpecDraft` :11, `strataSpecTemplate` :51)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (`applyStrataImport` :3217, `applyStrataTemplate` :3658)
- Modify: `gateway/frontend/src/i18n.ts` (`strataImportWarnLogFilesCapped`)
- Test: `strataRunConfig.test.ts`, `strataTemplate.test.ts`, `RuntimeAdminSection.test.tsx` (Strata import tests ≈:13950)

**Interfaces:**
- Produces:
  - `StrataImport` ok gets `logFiles: LogFile[]`; a new warning `'log_files_capped'`.
  - The import sets `out.log = '${CONFIG_DIR}/engine.log'` → `logFiles = [{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }]`.
  - If `out.replicas` is an array of length n ≥ 2, or an integer n ≥ 2: `out.log` stays `'${CONFIG_DIR}/engine.log'` (Strata suffixes it) and `logFiles = [{ name: 'engine-r<i>', path: '${CONFIG_DIR}/engine.r<i>.log' }]` for i < min(n, 4). n > 4 adds `'log_files_capped'`.
  - `RuntimeSpecDraft.logFiles: LogFile[]`, default `[{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }]`. `applyStrataTemplate` sets it only when the editor's log-files list is empty.
  - `applyStrataImport` replaces log-file rows whose names start with `engine`, and keeps others.

- [ ] **Step 1: Write failing tests.**
  - `strataRunConfig.test.ts`: `log` is rewritten (the fixture's `C:\Users\<user>\…` is gone); `logFiles` for the plain case; replicas 3 → `engine-r0..r2`; replicas 6 → 4 entries plus the warning.
  - `strataTemplate.test.ts`: update the exact `toEqual` with `logFiles`.
  - Admin: the import fills the log-files list.
- [ ] **Step 2: Run** `npx vitest run src/components/shared/strataRunConfig.test.ts src/components/shared/strataTemplate.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test && npm run lint && npm run format:check && npm run build` and confirm all PASS.
- [ ] **Step 5: Commit** `feat(portal): Strata import declares its engine log (#430)`.

## Phase 7 — Docs and final verification

### Task 14: Architecture docs, ADR-121, API surface

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md`: §3.1 (`runtime_allowed_log_dirs`, empty = `${CONFIG_DIR}` only, why `runtime_allowed_dirs` is not reused), §3.2 (masking in output, limits), §3.5 (`${CONFIG_DIR}` log recommendation, removal before exec, dir creation, forward slashes in content on Windows), §3.8 (WSL log paths; distro-internal paths refused pending the host check), §5.6 (`log_files` not launch-affecting), §7 (flag, 0.26.0), §14 (new subsection "Log files": tailer, per-stream rings, the hard byte total, per-unit drain, file events, `log_file_tails`), §15 (config reference rows)
- Modify: `docs/architecture/reference/api-surface.md` (entry `stream`, file events, `last_error.log_file_tails`, `log_files` on the spec PUT/GET)
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-121, "The agent may read files the gateway names, under these rules", in the style of ADR-120)

- [ ] **Step 1: Write the doc changes.** Each claim cites the code it describes.
- [ ] **Step 2: Verify** every function/const named in the docs exists: `grep -rn` each one. No local host names; no allowlist paths in examples beyond generic ones.
- [ ] **Step 3: Commit** `docs: runtime log files, ADR-121 (#430)`.

### Task 15: Whole-branch verification

- [ ] **Step 1:** `git fetch origin && git merge origin/main`. Re-check the agent Version against main.
- [ ] **Step 2:** Agent: `cd server-agent && go test ./... -count=1 && golangci-lint fmt --diff && golangci-lint run`. Linux-tagged tests run in the `golang:1.26` container. `GOOS=windows go build ./...`.
- [ ] **Step 3:** Gateway: `cd gateway/backend && go test ./... -count=1` (with the Postgres DSN) `&& golangci-lint fmt --diff && golangci-lint run`.
- [ ] **Step 4:** Portal: `cd gateway/frontend && npm test && npm run lint && npm run format:check && npm run build`.
- [ ] **Step 5:** `make sonar-gate` (background), judged on branch-attributed findings.
- [ ] **Step 6:** Whole-branch review by a fresh reviewer against the spec and the Review Focus list.
- [ ] **Step 7:** Remove `docs/superpowers/` and `docs/implementation-status.md` from the branch, then push and open the PR. The PR body includes a Windows-host acceptance checklist: a Strata `${CONFIG_DIR}` engine log, a crash with `log_file_tails`, rotation by the server, and the `\\wsl.localhost` symlink check from §5.4 as an open follow-up.

**Not in this plan (follow-up after the host check, §5.4):** reading distro-internal WSL paths through `\\wsl.localhost` or a distro helper. Until then they are refused as `file_not_permitted`.
