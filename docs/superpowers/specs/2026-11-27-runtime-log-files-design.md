# Runtime live log: tail declared log files (issue #430) — design

Status: draft for owner approval. Branch-local working file (removed before
the PR, per `CLAUDE.md`).

## 1. Intent

Some model servers write the diagnostics that explain a failure only to a
**log file**, not to stdout/stderr. The motivating case was a Strata spec that
crashed on a Windows host. The engine log named by the run config's `log` key
showed the commit-limit death, per-GPU free VRAM and page-file warnings, and
the owner had to copy that file off the host by hand. llama.cpp
(`--log-file`), vLLM and SGLang can do the same. The feature is therefore
generic.

A runtime spec can declare log files. The agent tails them, holds the text in
memory under the same rules as process output, and the portal shows each file
as its own tab in the existing live-log dialog. When a start fails, the end of
each declared file also goes into `last_error`, next to the stderr tail.

## 2. Goals, non-goals, anti-goals

Goals
- Declared log files are visible live and as scrollback in the live-log
  dialog, per instance, for native **and WSL** specs.
- A crash shows the end of each declared file in `last_error`.
- The agent decides what it may read (fail closed). The gateway only names
  paths.
- The design leaves room for log files outside the agent-owned directory
  (owner: "nichts ausschließen"), behind an agent-local allowlist.
- Best-effort masking of the agent's own resolved secrets, applied equally to
  process output and file text.

Non-goals
- Inferring log paths from argv or config-file content (`--log-file`,
  `"log"`). Only an explicit field counts.
- Persisting anything. File text, like process output, stays in memory only.
- General content masking. Only literal occurrences of the values the agent
  itself substituted are masked.
- A new SSE endpoint or a per-stream subscription.

Anti-goals (would be a failure even if it "works")
- The gateway can make the agent read an arbitrary host file (for example
  through `..`, a symlink or junction escape, a UNC or device path, or an
  empty allowlist that means "anything").
- A chatty engine log evicts the process-output history.
- Free text from the agent appears where the portal writes a sentence (status
  wording stays portal-owned, events stay a closed allow-list).
- A refusal message or log line on the gateway side reveals agent-local
  allowlist paths.
- A secret that was masked before reaches the gateway in clear text because of
  the new code path, for example through a split across two reads.
- Tailing blocks or disturbs the server's own writes or rotation (Windows
  share modes).

## 3. Decisions (made with the owner)

| # | Question | Decision |
|---|----------|----------|
| 1 | Allowlist or `${CONFIG_DIR}` only | Both. `${CONFIG_DIR}` of the instance is always allowed. A new agent-local `runtime_allowed_log_dirs` is built **now** and adds further directories. An empty list means only `${CONFIG_DIR}`. |
| 2 | External paths: start position | Start at end-of-file as of generation start. No replay of earlier runs. |
| 3 | Buffer sharing | Each stream gets its **own** share (a full-size ring per stream), see §6. |
| 4 | Content masking | Yes: literal occurrences of the agent's own resolved secrets, in process output and file text, see §8. |
| 5 | WSL | In scope for the first cut, see §5.4. |
| 6 | `last_error` | Yes: end of each declared file, see §9. |
| 7 | Portal layout | Variant B: instance row on top (as today), source row below; source row only when `log_files` is declared, see §11. |
| 8 | `${CONFIG_DIR}` inside config-file **content** on Windows | Substituted with **forward slashes** (`C:/…/strata`), see §4.1. No new placeholder. |

Calls marked **[builder call, settled]** below were made by the designer after
the builder check, not by the owner; they can be revisited in review.

## 4. Spec field

- New optional spec field `log_files: [{name, path}]`, at most **4** entries.
- `name`: tab label and stream id, `[A-Za-z0-9._-]{1,32}`, unique within the
  spec. The name `process` (case-insensitive) is reserved, because the wire uses
  an omitted `stream` for process output and the portal labels it.
- `path`: absolute after placeholder expansion. Only the **non-secret**
  placeholders that config-file content accepts are allowed (`${CONFIG_DIR}`,
  `${PORT}`, `${MODEL}`, …). `${API_TOKEN}` and `${AGENT_ENV…}` are hard
  validation errors (agent `ParseConfig` and gateway runtime-document
  validation both).
- Expansion is per instance: `${CONFIG_DIR}` resolves to `<config_files_dir>/<spec-id>[#k]`,
  so each instance of a replicable spec tails its own file.
- **Not launch-affecting**: a change to `log_files` changes what the agent
  tails, not the child. It must be added to the §5.6 "not launch-affecting"
  set explicitly (`manager.go:2304`: unknown fields default to launch-affecting).
  A change applies at the next generation start. Re-arming the tailers of a
  running generation is the builder's call; see §15.
- Recommended practice: point the server's log at `${CONFIG_DIR}/<file>` and
  declare it there. The Strata import does that automatically (§11.3).
- **[builder call, settled]** If a spec declares a `log_files` path inside
  `${CONFIG_DIR}` but no `config_files`, the agent still **creates the
  per-instance config directory** (empty) before exec, so a server's
  `--log-file ${CONFIG_DIR}/x.log` can open it. Today `planSpecConfigFiles`
  returns early without `config_files`. The directory is agent-owned, so this
  adds no new write location.

### 4.1 `${CONFIG_DIR}` in config-file content on Windows (owner decision 8)
`expandConfigFileContent` substitutes values verbatim (`config_files.go:79-91`).
A native Windows `${CONFIG_DIR}` (`C:\Users\…`) inside a JSON string is invalid
JSON (`\U`, `\s` are not JSON escapes), so the Strata `log` rewrite would stop
Strata from starting at all. Fix: on a Windows agent, `${CONFIG_DIR}` inside
**config-file content** is substituted with forward slashes (`C:/Users/…`),
which Win32 file APIs, Python and every common config format accept. In
**args and env** it keeps the native form. WSL specs are unaffected (already
POSIX). This is safe today because nothing uses `${CONFIG_DIR}` in content yet.
The difference between args and content is documented in §3.5 and in the
placeholder hint of the spec editor. A test covers a JSON config file with a
Windows `${CONFIG_DIR}` that must parse.

## 5. Path policy on the agent (the security boundary)

### 5.1 Allowed roots
A resolved path is tailable only if it lies inside:
- **(a)** the instance's own config directory (`${CONFIG_DIR}` as resolved for
  that instance), or
- **(b)** an entry of the new agent-local allowlist `runtime_allowed_log_dirs`
  (config file key) / `OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS` (env). It is local
  only and never gateway-supplied. It has the same entry syntax and containment
  semantics as `runtime_allowed_dirs` (`withinDir` / `allowedDirBase`: Clean
  plus separator-boundary containment, trailing `\*` / `/*` is a subtree
  synonym, not a glob, and `wsl://<distro>/…` entries apply to WSL specs only).
- **Empty list = (a) only.** `runtime_allowed_dirs` is deliberately not reused:
  empty there means "any `work_dir`", which here would mean "any file".

### 5.2 Checks (every open and every reopen)
1. `filepath.Clean`, must be absolute. Containment is checked on the cleaned
   path.
2. Resolve symlinks and junctions (`filepath.EvalSymlinks`; on Windows also
   the final path of the opened handle, `GetFinalPathNameByHandle`). Containment
   is checked **again on the resolved path**, and the path is refused if that
   leaves the allowed tree. The allowed roots are themselves resolved once per
   check, so a root that is itself a symlink compares correctly.
3. Open read-only. The target must be a **regular file**. Directories,
   devices, pipes, sockets, `\\.\` and `\\?\` device paths, UNC paths named in
   the spec, and `/proc` / `/sys` / `/dev` on Linux are all refused.
4. Windows: open with `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE`, so
   the tail never blocks the server's writes, renames or deletes.
5. These checks run again after every rotation, because the target can change.

A refused path becomes the typed per-stream status `not_permitted`. As with
today's refusal messages, it names the setting, never the allowlist paths. The
path itself goes to the agent's own log only.

### 5.3 `${CONFIG_DIR}` file lifecycle
For a declared file whose resolved path lies inside the instance's config
directory, the agent **removes the file before exec**, so the file of a
generation belongs to that generation and the tail reads from offset 0. A file
outside `${CONFIG_DIR}` is **never** removed or truncated by the agent.

### 5.4 WSL specs
- **`${CONFIG_DIR}` (the normal case):** for a WSL spec `${CONFIG_DIR}` is the
  agent's Windows `config_files_dir` seen through `/mnt/<drive>`
  (`policy_local.go:1150-1165`, `windowsPathToWSL`). The agent maps the
  distro path back to its own Windows path (the inverse of `windowsPathToWSL`)
  and tails it with the native tailer and the native checks.
- **`/mnt/<drive>/…` paths** are mapped back to `<Drive>:\…` and checked
  against the **Windows** entries of the allowlist, because the file physically
  lives there.
- **Distro-internal paths** (ext4 inside the distro, for example
  `/home/u/x.log`) are allowed only under a matching `wsl://<distro>/…` entry.
  They are read from the Windows agent through `\\wsl.localhost\<distro>\…`.
  This UNC form is **built by the agent**, never taken from the spec. It is the
  only UNC path the tailer opens.
- **To verify on a real host before the distro-internal path ships:** whether
  symlink resolution through `\\wsl.localhost` (Plan 9 share) is reliable
  enough for the resolved-path containment check. If it is not, the fallback is
  a small read helper started in the distro through `wsl.exe`, in the style of
  the existing UID and sampling helpers (`proc_control.go`). That would be a
  separate, larger task. Until it is verified, a distro-internal path must
  fail closed (`not_permitted`), not silently skip the symlink check.

## 6. Retention: one ring per stream

- Every spec has one **stream** for process output (today's buffer) plus one per
  declared file. Each stream gets its **own ring of `runtime_log_buffer_bytes`**
  (default 1 MiB, min 64 KiB). Splitting one ring was rejected: with 4 files at
  the 64 KiB floor each share would be ~13 KiB, below one 32 KiB pipe read,
  which is the reason the floor exists.
- The fleet ceiling `runtime_log_buffer_total_bytes` (16 MiB) counts **all**
  streams and is unchanged. Eviction under the total ceiling still drops whole
  unwatched specs (all their streams together). No new memory setting.
  Consequence to document: a spec with 4 files can hold up to 5 × the per-spec
  size, so fewer specs fit under the same total.
- **[builder call, settled] The total stays a hard byte bound.** Today
  `NewLogStore` bounds the number of spec buffers (`total / perSpec`); that
  becomes a byte-based accounting over all streams. If one spec's streams alone
  would exceed the total (possible with a raised `perSpec`), the process ring
  keeps its full `perSpec` and the **file rings are clamped** equally to the
  remainder, with the 64 KiB floor. If even that does not fit, `NewLogStore`
  raises the total to `perSpec + 4 × 64 KiB`, just as it already raises a
  total below `perSpec` today, and the agent logs the effective capacity. It
  does not refuse to start.
- Instances of a replicable spec share each stream's ring, as today (records
  carry the instance; process lines keep the `[#k] ` prefix). File lines of
  instance k are prefixed the same way.
- `dropped_bytes` is counted **per stream**, so each tab shows its own gap.
- Generation markers (`started` / `exited` / `start_failed`) stay in the
  process stream. A file stream gets its own boundary: tailing a generation
  starts with a typed `file_opened` marker (§7) carrying that generation, so the
  portal can show the generation boundary in a file tab without synthesized
  text.
- `tail()` (today the source of `StderrTail`) must filter to the process
  stream explicitly. Otherwise file text would leak into `stderr_tail`.

## 7. Tailer semantics

- **Start:** when the generation starts, after config files are written and the
  `${CONFIG_DIR}` file was removed (§5.3). Start offset: 0 for a `${CONFIG_DIR}`
  file, end-of-file as of generation start for an external path.
- **Stop:** after the generation exits, plus a **grace of 2 s** to catch the
  final lines. That is exactly the crash case.
- **[builder call, settled] One tailer per generation and declared file**, not
  per spec. The next generation does **not** wait for the previous grace:
  restart and admission timing stay as they are. Generation N's tailer keeps its
  open handle; removing the `${CONFIG_DIR}` file for N+1 (§5.3) unlinks the name
  only (Linux: the inode stays readable; Windows: the handle was opened with
  `FILE_SHARE_DELETE`, so delete succeeds and the old handle reads on). N reads
  its handle to EOF during its grace; N+1 opens the new file. Both write into
  the stream ring tagged with their own generation.
- **[builder call, settled] Removal fails** (for example a lingering child holds
  the file without `FILE_SHARE_DELETE`): the start is **not** failed. The new
  generation's tailer starts at the current end-of-file instead and emits a
  typed `stale_file` marker, so the tab does not show the previous run's lines
  as this run's.
- **Follow:** poll by stat every **250 ms** (aligned with the existing flush),
  no fsnotify, for portability. A missing file is polled further with status
  `waiting`.
- **Truncation:** size < offset → read from 0 and emit a `truncated` marker.
- **Rotation:** file identity change (inode / Windows file id + volume serial)
  → reopen (with the §5.2 checks) and emit a `rotated` marker. The remainder of
  the old handle is read to EOF first.
- **Caps:** max line length **16 KiB** (cut with a visible typed marker, not
  text). Per-file read rate **256 KiB/s**; bytes skipped by the cap count
  as `dropped_bytes`.
- **[builder call, settled] Live queue and drain are per (spec, stream).** Each
  stream has its own `logPendingBytes` (64 KiB) queue, so a file at its rate
  cap cannot cause drops in the live process tab. `Drain` visits
  **(spec, stream) units** round-robin and sends at most one batch per unit; the
  load-bearing invariant `logDrainBudget (128 KiB) > logPendingBytes (64 KiB)`
  then holds per unit unchanged, and the fleet-wide WebSocket budget stays
  128 KiB per drain. The gateway subscriber queue (`runtimeLogSub`) counts
  `dropped_bytes` per stream as well.
- **Binary:** invalid UTF-8 → U+FFFD. NUL and control bytes other than
  `\t\n\r` are escaped. A file that is mostly binary (heuristic and threshold
  are the builder's call) gets the status `binary`, and tailing stops for that
  generation.
- **Masking** (§8) is applied before any byte enters the ring.

### Closed event / status set (new members)
Markers: `file_opened`, `file_rotated`, `file_truncated`, `file_line_cut`,
`file_stale`. Statuses (also carried as typed events on the stream):
`file_waiting`, `file_not_permitted`, `file_binary`, `file_missing` (the
generation ended and the file never appeared). The agent-lacks-the-flag case is
portal-side only. File events are valid only on an entry with a `stream`.
(Elsewhere in this document the short names `rotated`, `waiting`, … mean these.) The gateway allow-lists them. The portal owns their wording.

## 8. Secret masking (applies to process output too)

- **What:** the values the agent substituted for `${API_TOKEN}` and
  `${AGENT_ENV:…}` in this generation's launch (the `secretSpan` values from
  `expandSpec`). Plain env values are **not** masked (for example
  `CUDA_VISIBLE_DEVICES=0` would otherwise mask every "0").
- **Replacement:** the placeholder that produced the value (`secretSpan.mask`,
  for example `${AGENT_ENV:HF_TOKEN}`), the same as in the resolved command.
- **Minimum length:** values shorter than **8 bytes** are not masked (they
  would shred the output). Documented.
- **Where:** in the write path, **before** the ring. The ring, the stream,
  `stderr_tail` and `log_file_tails` therefore only ever hold masked text. This
  is the same "retention is already-masked" principle as the resolved command.
- **Split across reads:** each stream keeps a carry of up to
  `len(longest secret) - 1` bytes until the next read, flush-on-exit, or the
  end of the grace period. Live delivery lags by at most that many bytes.
  Masking must hold across process-output pipe reads and file reads alike.
- **Honest limits** (stated in code and docs): encoded forms (URL, base64,
  JSON-escaped, split by the child across lines) are not detected. The comment
  in `command.go:103-106` changes from "the child's output is not masked" to
  "literal occurrences of the agent's own resolved secrets are masked, nothing
  else".
- This is agent-local behavior and needs no feature flag. It changes what
  current gateways receive in process output (only masked values), which is
  strictly safer.

## 9. `last_error`

- `stderr_tail` stays **process output only** (§6).
- New `log_file_tails: [{name, tail}]` on `LastError` and
  `sample.RuntimeErrorSample`: the end of each declared file for the failed
  generation, at most 2 KiB each, at most 4 entries. The gateway clamps each
  tail to 2048 bytes, keeps at most 4 entries, drops entries whose name fails
  the §4 name pattern, and keeps them in RAM only, like `stderr_tail` (`agent_ingest.go:587-598`,
  `runtime_registry.go:26`).
- **Timing:** `last_error` is recorded at exit. The file tails are added once
  the tailer has finished its post-exit grace. Briefly there is a `last_error`
  without file tails, then one with them. **[builder call, settled]** The tails
  are attached only if `last_error` still describes that generation (failure
  count / generation unchanged); if a later event already replaced it, they are
  dropped.
- Refused, missing or binary files get no entry (their status is in the tab).
- An agent without the feature flag sends no field. Nothing changes then.

## 10. Wire, feature flag, versioning

- **Feature flag** `runtime_log_files` in `agent.Features`
  (`server-agent/internal/agent/features.go`), added to `gatewayAgentFeatures`.
  **MINOR** bump of `const Version` (`server-agent/internal/agent/agent.go`,
  `0.25.2` on main today → `0.26.0`, or whatever main holds when the branch is
  finalized; merge `origin/main` first).
- An agent without the flag ignores `log_files`. The gateway keeps the field in
  the runtime document regardless.
- **`LogEntry` / `RuntimeLogEntryDTO`** gain `stream` (omitted = process output,
  which is what every current agent sends). Otherwise it is a declared `name`.
  The gateway clamps `stream` on ingest (it must match the §4 name pattern,
  else the entry is dropped) in addition to the existing clamps
  (`runtime_logs.go:54-87`, `683-703`). **[builder call, settled]** The gateway
  does **not** check the name against the spec's declaration: neither the
  ingest path nor the registry knows the runtime document (a DB read per frame
  otherwise). The portal only shows tabs for declared names, so an undeclared
  stream is never rendered.
- The same `runtime_log_config` watch, epochs, scrollback replay and chunking
  cover every stream of a spec: one SSE connection, one watch entry, the
  64-spec bound unchanged. Scrollback replays all streams of the spec (order
  between streams: the builder's call, within a stream the order is kept).
- Per-stream status updates (`waiting`, `not_permitted`, …) travel as typed
  entries on the same stream.
- `reference/api-surface.md`: document the entry shape and the new fields.

## 11. Portal

### 11.1 Live-log dialog (variant B)
- **Top row: instance** (`Gesamt` / `Instanz 0` / `Instanz 1` …, the labels
  as today), shown exactly when it is shown today (replicable specs).
- **Second row: source** (`Prozess` / `Process` and one tab per declared file).
  It is shown **only when the spec declares `log_files`**. Without log files
  only process output exists, it is selected implicitly and the dialog looks
  exactly as today.
- The selected source is kept when switching instance. The selected instance is
  kept when switching source.
- Under `Gesamt`, a file source shows the file text of all instances
  interleaved with `[#k]` prefixes, the same rule as process output today.
- Opened from an instance's row, the dialog starts on that instance's tab with
  source `Prozess`, as today.
- An empty file tab always says why (`waiting`, `not_permitted`, `binary`,
  `missing`, or the agent lacks `runtime_log_files`). For an agent that lacks
  the flag the source row is still shown, so that the reason is visible.
- Text is never rendered as HTML (unchanged).

### 11.2 `last_error` block
Below the stderr tail, one section per `log_file_tails` entry, headed by the
file name, rendered as text.

### 11.3 Spec editor and Strata import
- A `log_files` list editor (name + path, max 4) with a placeholder hint and the
  recommendation `${CONFIG_DIR}/…`. Validation mirrors §4 (name pattern,
  uniqueness, reserved `process`, secret placeholders rejected).
- If the server's agent lacks `runtime_log_files`: the hint "needs agent with
  feature `runtime_log_files`". Saving stays possible.
- Strata import (`strataRunConfig.ts`): rewrite the run config's `log` key to
  `${CONFIG_DIR}/engine.log` and pre-fill `log_files: [{name: "engine", path:
  "${CONFIG_DIR}/engine.log"}]`. The Strata template default gets the same
  entry. This relies on §4.1 on Windows.
- **[builder call, settled]** Strata suffixes the log path itself when its run
  config has `replicas` (`terminal/serve/replicas.py`, `_suffixed`:
  `engine.log` → `engine.r0.log`, `engine.r1.log`, …). If the imported run
  config has `replicas`, the import declares one file per replica
  (`engine-r0` … `engine-r3`, paths `${CONFIG_DIR}/engine.r<i>.log`), at most 4;
  beyond 4 it declares the first 4 and the editor says so.
- All new strings in German and English together.

## 12. Constraints

- AGENTS.md rules: feature branch plus PR, never commit to main; the ServerAgent
  Version Rule; architecture docs are canonical and are updated in the same
  change.
- Nothing persisted: the existing "nothing reaches disk" sandbox test (agent)
  and the SQLite needle test (gateway) are extended to file streams and
  `log_file_tails`.
- Refusals name settings, never paths, on everything that leaves the agent.
- golangci-lint (gofumpt, gocritic) per module; prettier `format:check` for the
  portal; archtest allowlist if new packages or imports are added.

## 13. Tests

- **Agent path policy** table tests: inside / outside / `..` / symlink escape
  / junction / UNC / device / `/proc` / WSL entries / `/mnt/<drive>` mapping,
  both separators on the Linux runner (like `TestAllowedDirBase`). A root that
  is itself a symlink. Re-check after rotation to a symlink that leaves the
  tree.
- **Tailer:** append, truncate, rotate, missing-then-created, `${CONFIG_DIR}`
  file removed before exec, external file starts at EOF, long line cut, binary
  stop, rate cap → `dropped_bytes`, post-exit grace captures the final lines.
- **Retention:** per-stream rings do not evict each other; the total ceiling
  counts all streams; `tail()` for `stderr_tail` excludes file streams.
- **Masking:** a secret in process output and in a file; a secret split across
  two reads/writes; secrets under 8 bytes are left alone; plain env values are
  not masked; the ring never holds clear text.
- **`last_error`:** `log_file_tails` filled after the grace, bounded, absent
  for refused files; attached only while `last_error` still describes that
  generation (it is replaced in place by `recordFailure`, `setTerminalVerdict`
  and `noteAdmissionWait`).
- **Generation overlap:** N+1 starts during N's grace; N still delivers its
  final lines; removal failure → `stale_file` and start at EOF.
- **Drain:** per-(spec, stream) queues, round-robin, the budget invariant.
- **`${CONFIG_DIR}` in content on Windows:** forward slashes, JSON parses;
  args keep the native form.
- **Feature flag** guard test; `log_files` not launch-affecting.
- **Gateway:** ingest clamps for `stream` and the new events; entries with an
  undeclared stream dropped; `log_file_tails` clamp and name filter; relay test;
  SQLite needle test extended.
- **Portal:** `RuntimeLogView.test.tsx` instance × source rows, the hidden
  source row without `log_files`, kept selection across rows, status sentences,
  agent-too-old; `last_error` sections; spec editor round-trip and validation;
  Strata import rewriting `log`.

## 14. Docs to update

- `docs/architecture/cross-cutting/agent-runtime-manager.md`: §3.1 (new
  allowlist), §3.2 (secret masking in output), §3.5 (`${CONFIG_DIR}` log
  recommendation, removal before exec), §3.8 (WSL log paths), §5.6
  (`log_files` not launch-affecting), §7 (flag), §14 (new subsection "Log
  files", per-stream retention, masking), §15 (config reference).
- `docs/architecture/reference/api-surface.md` (entry shape, `last_error`).
- `docs/architecture/09-architecture-decisions.md`: **ADR-121** "The agent may
  read files the gateway names, under these rules" (first time the agent reads
  gateway-named files).

## 15. Left to the builder (sensible defaults, not owner decisions)

- Package layout of the tailer and the path-policy code.
- The binary heuristic and its threshold.
- Whether a `log_files` change re-arms the tailers of a running generation or
  only applies at the next start (either is fine; the next start is simpler).
- Cross-stream ordering during scrollback replay.
- Exact portal styling of the second row.
- Phasing (suggested: agent policy + tailer + retention; masking; wire +
  gateway; `last_error`; portal; WSL distro-internal paths last, after the
  host check in §5.4).
