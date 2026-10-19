# Design — Agent-provisioned config files for managed runtimes (#257)

- **Date:** 2026-10-19
- **Status:** Draft design — not yet approved, nothing built.
- **Scope note:** Branch-local working document under `docs/superpowers/specs/`.
  Its durable outcomes are folded into `docs/architecture/` (+ ADR-066) and this
  file is deleted before the pull request. It never reaches `main`.
- **Issue:** #257. Related shipped work: #254 (manual speech/transcription verdict),
  #255 (agent-side auto-detection). Related open: #264 (server-side transcoding —
  the agent-side config location relates to this facet).

## 1. Intent and why

The agent launches each managed child from a launch spec that can only express a
**command line**: `Binary`, `Args`, `Env`, `WorkDir` (`server-agent/internal/runtime/types.go`,
`Spec`). The only file the agent writes today is its own runtime-config cache
(`config_client.go`) — never a config file *for* the launched process.

A server that cannot be driven entirely by argv therefore cannot be managed
end-to-end from the run-spec UI. **audio.cpp is the concrete case:** it reads the
models it serves from a JSON config (`server.json` — an array of `{id, path, task,
mode}`) that the operator must create **by hand on the host**, outside the run-spec
workflow.

Two problems follow:

1. **Operability.** The `server.json` lives outside the gateway, hand-made and
   hand-maintained per host. The portal cannot provision it, so audio.cpp is not
   manageable end-to-end from the RunSpec form. The current workaround is exactly
   this: create the file manually on the server and pass it as a fixed `--config`
   argument.
2. **Drift → 500s.** Because the model id lives in that hand-made file, decoupled
   from the spec's `upstream_model`, it drifts from the name the gateway rewrites
   requests to (`mapping.AppModelName`; `routing/resolver.go`,
   `gateway/native_passthrough.go`). A mismatch yields
   `{"error":{"message":"unknown model id: <name>", ...}}`, surfaced as HTTP 500.

**Goal.** Let an operator provide a managed child's config-file **content directly
in the portal RunSpec**. The agent **writes the file before launch** and the spec
references it from `Args` (e.g. `--config ${CONFIG_DIR}/server.json`). The id in the
file derives from the spec (via `${MODEL}`), so the drift is **impossible by
construction**, and the whole audio.cpp lifecycle is driven from the gateway.

## 2. The load-bearing decision — Variant A (one child per spec)

The issue weighed two shapes. This design chooses **A** and explicitly rejects **B**.

- **A (chosen) — one audio.cpp per model spec.** Each spec provisions its own config
  file(s), its own port, its own GPU. Maps cleanly onto the current
  one-child-per-spec runtime. Running N processes (more memory/VRAM, repeated
  loading, N health checks) is an accepted cost.
- **B (rejected) — one shared audio.cpp for several models.** A single `server.json`
  assembled from several specs by a reconciler. More efficient, but it breaks the
  one-child-per-spec assumption (lifecycle, admission, GPU accounting, idle/stop all
  become server-scoped) and needs a conflict-free many-specs→one-file reconciler.
  Out of scope for this work.

**Rationale.** A needs no new lifecycle model — it reuses the per-spec process,
port, and GPU accounting unchanged. B's efficiency is not worth a lifecycle rewrite
for the current need. The operator's stated model is one file + one unique port per
RunSpec, which is A.

## 3. The feature — a "config files" facet on the spec

A spec may carry **zero or more** config files. Each is a `{name, content}` pair.
The facet is **general** (not audio.cpp-specific), so it also covers the per-model /
per-voice config another server needs (e.g. PocketTTS's `--voice-ref`): **0..N files
per spec**, built that way from the start so other multi-file programs are covered
without a second pass.

### 3.1 Where the content is authored

In the **portal RunSpec form**: a repeatable editor of `{file name, content}` rows
(content is a multi-line text area). It is stored on the spec and carried to the
agent in the runtime-config document. Zero rows is the default and means "no files"
— unchanged behavior for every existing spec.

### 3.2 Placeholder expansion inside the content

The file content runs through a **content expander that shares the agent's existing
placeholder resolution** (the `Args`/`Env` path, `ExpandPlaceholders` / `expandSpec`
in `runtime/policy_local.go`) but over a **non-secret token set only**: `${PORT}`,
`${MODEL}`, `${HOST_GPU_IDS}`, `${CUDA_DEVICES}` / `${VULKAN_DEVICES}` /
`${METAL_DEVICES}`, and `${CONFIG_DIR}` (§3.4).

This is what makes the audio.cpp `id` derive from `${MODEL}` (= `upstream_model`) and
the listen port from `${PORT}`, which removes the drift.

The **secret-bearing** placeholders `${API_TOKEN}` and `${AGENT_ENV:NAME}` are, by
default, **not** expanded inside written content — placing a decrypted secret in a
file on disk is exactly the concern of open call (A) in §7, and the two are one
decision. (`${AGENT_ENV:NAME}` is the agent's primary secret-delivery channel, so it
belongs on the same side of that line as `${API_TOKEN}`, not in the allowed list.)
This also means content **cannot literally reuse `expandSpec`**: that function
*resolves* the two secret placeholders and records secret spans for command-report
masking, neither of which applies to a written file. A small content-specific expander
over the shared non-secret resolver is required.

### 3.3 Where the agent writes the files — **not** WorkDir

WorkDir is operator-chosen and may **not be agent-writable** (the binary's own
directory, a read-only mount). So the agent writes into a **dedicated, agent-owned,
writable base directory**, configurable in the agent config (§4), under a
**per-spec subdirectory keyed by spec ID**:

```
<base-dir>/<spec-id>/<name>
```

The per-spec subdir **guarantees uniqueness** — two specs that both name their file
`server.json` land in different directories and never collide. Writes reuse the
existing atomic pattern (`config_client.go` `writeCacheFile`: temp file created
inside the target dir, `chmod 0600`, rename into place). Files are **(re)written on
every launch** (idempotent overwrite), **after** the listen port is chosen and the
placeholders are resolved, **before** `exec`.

### 3.4 Referencing the written files from Args — `${CONFIG_DIR}`

A new placeholder **`${CONFIG_DIR}`** resolves to the absolute per-spec directory
`<base-dir>/<spec-id>`. The operator writes e.g. `--config ${CONFIG_DIR}/server.json`
in `Args`. *(My call: the placeholder resolves to the directory. A per-file form such
as `${CONFIG:server.json}` was considered and rejected as redundant — the operator
already names the file in the content editor and joins it in Args.)*

`${CONFIG_DIR}` is a **first-class** placeholder resolvable in `Args`, `Env`, **and**
config-file content — so a multi-file config can reference a sibling file by absolute
path. Resolving it threads the `(base-dir, spec-id)` into the placeholder resolver,
which is otherwise pure over the spec; the Args/Env expansion and the content expander
therefore share one source for the directory.

### 3.5 Failure classification and the pre-launch dry run

`startProcess` already runs a dry-run `ExpandPlaceholders(spec, 0, …)` **before**
grabbing the port, so a malformed spec fails cheaply and is rate-limited
(`StateNotPermitted` re-evaluation) rather than hammering syscalls. **Content
expansion joins that dry run** — a bad placeholder or an empty `${MODEL}` in content
is the same class of operator error as in `Args`/`Env` and must self-throttle the
same way.

A **write failure** at launch (non-writable base dir, disk full, rename failure) is an
environmental launch failure, not a policy denial, so it takes the `StateStartFailed`
+ `recordFailure` + crash-backoff path (like an unusable `work_dir` today), with a
message naming the file/dir — never `StateNotPermitted`. The stable error taxonomy is
preserved: content-expansion errors reuse the existing expansion-error path; write
failures reuse the start-failed path.

## 4. Agent config — the base-directory knob

A new agent configuration value following the **existing triple-source pattern**
(flag / env / config-file, with relative paths resolved against the config file),
exactly like `RuntimeCachePath` (`server-agent/internal/config/config.go`):

| Source | Value |
|---|---|
| flag | `-config-files-dir` |
| env | `OP_AGENT_CONFIG_FILES_DIR` |
| config key | `config_files_dir` |
| default (unset) | a subdirectory beside the runtime cache / binary — *(my call:* `server-agent-config-files/`*)* |

This is the operator's answer to "don't clutter the agent root directory": they
point it at a directory of their choosing, and the agent creates per-spec subdirs
beneath it.

## 5. Agent wire contract + version

Writing a config file for the child is a **new observable agent behavior**, so it
goes through ADR-025 feature negotiation:

- New `agent.Features` entry: **`runtime_config_files`**, `Since: "0.10.0"`
  (`server-agent/internal/agent/features.go`).
- **ServerAgent `Version` 0.9.2 → 0.10.0** — MINOR, because `agent.Features` gains a
  flag (`TestFeatureRegistry` enforces the bump). One bump for the whole branch.
- **Declared for the portal's benefit.** An older agent ignores `ConfigFiles`
  entirely — nothing is written, and a spec whose `Args` reference `${CONFIG_DIR}`
  would then launch against a missing file. The portal reads the negotiated feature
  set and **warns** when the connected agent predates the flag. The agent always
  honors what it receives; the flag only states the fact to the gateway/portal.

### 5.1 File-mode report redaction

In file mode (`OP_AGENT_RUNTIME_SOURCE=file`), the agent's upward report already
strips every spec's `Env` **values** (`report.go` `redactConfigEnv`) because a local,
operator-authored `runtime.json` may carry plaintext secrets the gateway must never
receive. `ConfigFiles` **content** rides the same upward report and is equally
secret-bearing, so it is **redacted identically** — content values are stripped from
the file-mode report. The write step itself is unchanged in file mode: the files are
written for the child exactly as in gateway mode.

## 6. Data flow (end to end)

```
portal RunSpec form  (repeatable name+content editor)
  → request DTO        (RuntimeSpecRequest — new config_files field + validation)
  → stored spec        (routing.RuntimeSpec — new ConfigFiles field; append-only migration)
  → AgentRuntimeSpecDTO (new config_files field) in GET /api/agent/v1/runtime-config
  → agent runtime.Spec  (new ConfigFiles field)
  → startProcess:  grab port → expandSpec (resolve ${…})
                   → write <base>/<spec-id>/<name> (atomic, 0600, content-expanded)
                   → expand ${CONFIG_DIR} in Args/Env → exec
```

## 7. Constraints, security, and the open calls

**Constraints (from AGENTS.md / architecture):**

- Keep all three store drivers (memory/sqlite/postgres) working; the new spec field
  is an append-only versioned migration; wide Go values need wide Postgres columns
  (ADR-005). Verify with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Preserve stable error codes. The audio.cpp `unknown model id` → HTTP 500 path is
  unchanged; this removes a *cause* of it, not the handler.
- Feature-gated and backward compatible: with no config files, behavior is
  byte-for-byte unchanged for existing deployments.
- Portal localization: new form strings added in German **and** English together
  (`i18n.ts`, type-checked parity).

**Security:**

- The file **name** is sanitized to a single path segment (basename): reject empty,
  `/`, `\`, `..`, absolute paths, and control characters; names unique within a spec.
- Content **size** and file-**count** limits — *(my call to set concretely in the
  plan:* ≤ 64 KiB per file, ≤ 16 files per spec*)*.
- Files written `0600` into an agent-owned dir; never into WorkDir; decoding/writing
  operator-authored content only (the content arrives over the authenticated portal
  → gateway → agent channel).

**Open calls for the human (the single round after the builder-check):**

- **(A) Secrets inside written content.** Should the **secret-bearing** placeholders
  `${API_TOKEN}` and `${AGENT_ENV:NAME}` be expandable **inside** a written config
  file? Doing so places a decrypted secret on disk (`0600`). The known use cases
  (audio.cpp `server.json`, PocketTTS voice-ref) need no secret in the file. *Default
  if unanswered: **no** — content expands only the non-secret token set (§3.2), and
  secrets stay out of files.*
- **(B) Cleanup of a removed spec's directory.** GC `<base>/<spec-id>` when the spec is
  deleted/disabled, or leave it (overwrite-on-launch only, never removed)? *Default if
  unanswered: overwrite-on-launch; GC deferred.*

**Calls settled after the builder-check (listed so they can be checked while reading):**

- `${CONFIG_DIR}` is first-class — resolvable in `Args`, `Env`, and content (§3.4).
- Content uses a content-specific expander over the shared **non-secret** resolver; it
  does not reuse `expandSpec`, which resolves and masks secrets (§3.2).
- Content-expansion errors join the pre-port dry run and rate-limit like `Args`/`Env`
  errors; a file **write** failure takes the start-failed + backoff path, not
  not-permitted (§3.5).
- In file mode, config-file **content** is redacted from the agent's upward report
  exactly as `Env` values are (§5.1).

## 8. Goals / non-goals / anti-goals

**Goals.** Provision 0..N config files per managed child from the portal RunSpec;
derive the audio.cpp id/port from the spec so the `unknown model id` drift cannot
happen; a configurable, agent-owned, uniqueness-safe write location.

**Non-goals.** Variant B (shared-server reconciler); agent-side auto-detection of
audio capabilities (#255, shipped); server-side transcoding (#264).

**Anti-goals (a failure even if it technically "works").** Writing into WorkDir; any
design in which two specs can overwrite each other's file; a behavior/wire change
shipped without a feature flag + version bump.

## 9. What is left to the builder

- **Gateway:** thread `ConfigFiles` through the request DTO (+ validation: basename,
  uniqueness, size/count), the store (migration + three drivers), the
  `AgentRuntimeSpecDTO`, and the portal RunSpec form (DE/EN i18n).
- **Agent:** the config knob (`config.go`); `${CONFIG_DIR}` + content expansion
  (`policy_local.go`); the write step in `startProcess` (`manager.go`) placed after
  the port grab / `expandSpec` and before `exec`; the `runtime_config_files` feature
  flag + the single `Version` bump.
- **Tests:** content/`${CONFIG_DIR}` expansion units; write-before-exec; per-spec
  uniqueness; basename sanitization + limits; the postgres store leg; portal form +
  i18n parity; a local e2e suite for the RunSpec config-files editor.
- **Docs (fold durable outcomes here, then remove this spec):** **ADR-066** recording
  Variant A + the facet + `${CONFIG_DIR}` + the feature flag (reference ADR-061/062,
  ADR-025, #257); `cross-cutting/agent-runtime-manager.md` (the facet, the
  placeholder, the base-dir config); `reference/config-env.md` (the new agent knob);
  `reference/api-surface.md` (the new DTO field).
