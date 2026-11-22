# Strata support — design

Issue: #424. Branch: `feat/strata-support`. Base: `main` @ 5a9a10db.
Upstream research notes (outside the repo, session-local): `strata-upstream-research.md`,
`gateway-integration-touchpoints.md`, `strata-live-probe.md`.

## 1. Intent

The owner runs [Strata](https://github.com/Niko1221/Strata) (MIT; a local inference
engine for Qwen3.8-Flash-Next / Qwen3.6-35B-A3B with OpenAI- and Anthropic-compatible
APIs) on a Windows + NVIDIA multi-GPU host. They want **Claude Code and Codex, with a
gateway token, to use Qwen3.8-Flash-Next on Strata through the gateway**, with model
routing, usage accounting and **correct RAM and per-GPU VRAM measurement**, exactly
like every other backend. Two shapes:

1. **Application type `strata`** — an operator-run Strata (the desktop app's LAN share
   or a hand-started `strata serve`) registered by URL.
2. **Runtime spec type `strata`** — the server agent launches and supervises
   `strata serve` itself.

## 2. Goals, non-goals, anti-goals

Goals
- G1 Application type `strata` that works against both upstream shapes: `strata serve`
  (full route set) and the desktop-app share listener (only `/v1/models`,
  `/v1/chat/completions`, `/v1/messages`, `/v1/responses`; everything else 404).
- G2 Runtime spec type `strata` launchable from a portal template, configured by
  importing a `strata launch` run config.
- G3 GPU roles derived from the spec: **the first assigned GPU is main, all others
  helpers** (owner rule).
- G4 Windows native specs can opt into a **process-tree mode** that stops the whole tree
  (graceful first) and measures RAM and per-GPU VRAM over the whole tree (owner
  requirement: "RAM und VRAM Messung je GPU korrekt").
- G5 Existing behaviour unchanged for every existing spec and application.

Non-goals
- Intel Arc (separate SYCL server upstream), macOS/Metal and AMD/HIP runspecs: not
  blocked by the design, not verified, not documented as supported.
- Building engine args from portal fields (model path, context, KV…): the run config
  comes from `strata launch`; the portal only imports and edits it as a file.
- Live timings for Strata (stays off until measured, like every new kind).
- Windows graceful stop for specs that do not opt in.

Anti-goals (would be a failure even if it "works")
- A secret (the Strata API key, the gateway token) written into a config file or shown
  unmasked in the reported command.
- Stopping a Strata spec leaves the engine running or VRAM held.
- The owner's host name, Windows user name, LAN details or key in any repo text,
  issue, PR or doc. Examples use `C:\Users\<user>\…`.
- A changed default for any existing spec (process-tree mode defaults to `direct`).

## 3. Verified facts the design rests on

Upstream (source at `c6f6427`, release 0.2.17 / Control+engine 0.1.63; the share
listener measured live):

- `strata serve --config <json> --host H --port P --output jsonl
  [--shutdown-timeout S] [--gpu LIST]` runs the server in the foreground
  (`terminal/strata_cli/cli.py`); JSON lifecycle events on stdout, logs on stderr.
  Without `--lazy` the socket binds only after the engine is READY. The owner's real
  load takes 1–2 min. A run config's `engine_ready_s` is **not** a measurement: it is
  the server's own ceiling for waiting on the engine (default 900 s; launch raises it
  for slow starts; `app/host/server/native-control.mjs`, `strata_cli/launch/runner.py`).
- `GET /health` (no key) → `{"status":"ok","service":"strata","loaded":…}` on
  `strata serve` only. `/v1/models` (key) lists one model, id = config `model_name`;
  newer releases add `status.value`, `context_length`, `architecture.input_modalities`.
  The request's `model` field is ignored (one model per process).
- Auth: `--api-key` / `STRATA_API_KEY` / config `api_key`; client sends
  `Authorization: Bearer` or `x-api-key`. Keyless + non-IP Host header → 403.
- GPU order: engine stage order `"gpu": [helper…, main]`, main **last**;
  `"gpu_order": "as_given"` disables speed reordering; a hand-set `gpu`/`--gpu` wins
  over `gpu_roles` (`strata_control/gpu_roles.py`, `serve/server.py ordered_gpus`,
  `serve/server_parts/engines.py gpu_list`). `--gpu` takes nvidia-smi numbering, i.e.
  host indices.
- Windows: the Terminal installs `%LOCALAPPDATA%\Strata\terminal\current\bin\strata.cmd`
  (finds `..\python\python.exe`, sets `PYTHONUTF8=1`, `PYTHONNOUSERSITE=1`, clears
  `PYTHONHOME`/`PYTHONPATH`, runs `strata.py`). The engine runs in Strata's own
  kill-on-close job object without breakaway; graceful stop on SIGBREAK / console
  events, then drain, then engine QUIT.
- Anthropic usage reports `input_tokens: 1` with the prompt in the cache fields.

Repo (`main` @ 5a9a10db):
- `llama_cpp` is the behavioural template (port 8080, shared OpenAI-compatible client,
  passthrough `responses_mode`/`messages_mode`, `/props` + `/metrics` probing); PR #379
  (`vllm_sr`) is the structural checklist for "application type + spec type";
  PR #396 for agent-side probe edits. Types are plain text columns: **no migration and
  no OpenAPI change for the type itself.** Use the identical string `strata` for both,
  several kind checks rely on it.
- Anthropic passthrough usage already sums `input + cache_read + cache_creation`
  (`internal/gateway/native_passthrough.go`, `internal/provider/anthropic_messages.go`).
- Runtime specs support agent-written config files (ADR-066, §3.5 of
  `agent-runtime-manager.md`) with non-secret placeholders, and preserve declared GPU
  order (migration 73).
- Windows native processes are kill-only (`server-agent/internal/runtime/proc_windows.go`,
  documented KNOWN LIMITATION); VRAM grouping by process group exists only on Linux
  (#404, `collector/nvidia_groups_linux.go`); Windows RAM is the root PID's private
  working set (`collector/ram_measurer_windows.go`). Stop grace is the fixed
  `killGrace = 5s` (`runtime/manager.go`).
- Agent version 0.20.1; last migration 112; last ADR-110.

## 4. Design

### 4.1 Application type `strata`

Add `strata` as an application type the way #379 added `vllm_sr` (routing provider
constant and kind predicates, portal normalization/defaults, `providerClients` entry
reusing the shared OpenAI-compatible client, portal form + type note, i18n DE+EN, tests
that pin type lists, docs).

Defaults:
- port 8080; flavors `openai` + `anthropic`; `responses_mode` = `passthrough`,
  `messages_mode` = `passthrough`; coding-agent modes as for `llama_cpp`.
- health path and loaded-models path **`/v1/models`** (both upstream shapes serve it;
  it needs the key, which the application's `APIToken` supplies as
  `Authorization: Bearer`). The builder must verify the health probe and discovery
  attach `APIToken`; if the health probe does not, that is a bug to fix here.
- context length and image input from `/v1/models` (`context_length` / `meta.n_ctx`,
  `architecture.input_modalities`) when present; `/props` and `/metrics` are probed
  only as optional extras — a 404 is "not offered", never unhealthy and never an error
  in the portal.
- live timings off.
- type note (DE+EN): an API key is required on the share listener and recommended
  always (keyless Strata refuses non-IP Host headers); older Strata releases return 404
  on `/v1/responses` → set `responses_mode` to `translate`; one model per Strata
  process; image input needs a vision-enabled run config.
- If Strata reads local file paths for image input (as SGLang does), add it to the
  media-URL guard (`mediaURLsRefused`, `inference_resolve.go`). The builder checks
  upstream `serve/` image handling and decides; record the decision in the ADR.

### 4.2 Runtime spec type `strata`

Same string `strata` as spec type. Portal template (the "Strata" choice in the runspec
form) pre-fills:

- binary: `%LOCALAPPDATA%\Strata\terminal\current\bin\strata.cmd` shown as a hint with
  `<user>`; the operator must allowlist the absolute path on the agent
  (`runtime_allowed_binaries`), as for every binary.
- args (whole list; `--shutdown-timeout` rationale in §4.5): `serve --config ${CONFIG_DIR}/strata.json --host 0.0.0.0 --port ${PORT}
  --gpu ${HOST_GPU_IDS_MAIN_LAST} --output jsonl --shutdown-timeout 3`
  (bind address follows whatever the existing templates use for non-loopback
  agents; the builder mirrors the llama.cpp/sglang template).
- env: `STRATA_API_KEY=${API_TOKEN}` (the existing secret placeholder; masked in the
  reported command; never in the file).
- `set_visible_devices` **off** (Strata numbers cards like nvidia-smi; renumbering
  would break `--gpu`).
- `process_tree` = `job` (§4.5).
- startup timeout: `max(engine_ready_s, 900) + 60` s (so the agent never gives up
  before Strata's own engine wait does), default **960 s**; editable. The builder
  checks the gateway's allowed range and clamps.
- scheduling: like the llama.cpp template — not pinned, starts on demand, normal idle
  timeout (decision: mine, since a load is 1–2 min). The application timeout default
  must not trigger `timeout_ms_below_startup_timeout` for the template; the builder
  aligns the template with how the llama.cpp template avoids it.
- health path `/health`, probe/loaded-models path `/v1/models`; `/props` probing as
  for llama.cpp but tolerant of absence (PR #401 precedent for skipping `/props`).
- api flavors / modes as the application type.

If Windows acceptance (§6) shows that `strata.cmd` misbehaves under the agent (Go's
`.cmd` handling, or cmd.exe's "Terminate batch job" on Ctrl-Break), the fallback is
binary = the bundled `…\terminal\current\python\python.exe`, args prefixed with
`…\terminal\current\strata.py`, env adding `PYTHONUTF8=1`, `PYTHONNOUSERSITE=1`. The
template documents whichever works; the decision goes into the ADR.

### 4.3 Run-config import (portal)

In the Strata runspec form, an "Import run config" action accepts the pasted JSON a
`strata launch` writes (`%LOCALAPPDATA%\Strata\launch\<name>.json`). It produces the
config file `strata.json` (config-files facet) and proposes spec fields:

- keep: `exe`, `args`, `cwd`, `tokenizer`, `log`, `lib_dirs`, `vision`, `layer_split`,
  `vram_reserve_mib`, and any key not in the drop list (forward compatibility).
- drop: `args_written`, `port`, `host`, `open_browser`, `gpus_asked`,
  `gpu`, `gpu_roles`, `gpu_cards`, `gpu_order`, every `ui_*` key, `api_key`.
- set: `"model_name": "${MODEL}"`, `"gpu_order": "as_given"`.
- propose GPU rows: from `gpu_roles` if present (main first, then helpers in their
  listed order), else from `gpu` reversed-rotated so that its last entry (the engine's
  main) comes first. The operator can reorder; position 1 is main.
- propose startup timeout: `max(engine_ready_s, 900) + 60`, clamped to the allowed
  range; keep `engine_ready_s` itself **in** the file (it is Strata's engine wait
  ceiling, not app bookkeeping) — remove it from the drop list.
- propose upstream model / client-facing model name from the old `model_name` if the
  form fields are empty.
- reject (inline error, nothing written) a JSON that does not parse, has no `exe` or
  no `args` array, or contains `api_key` with a value (tell the operator the key goes
  in the spec's token, not the file — and drop it).
- the import is a pure frontend transform with unit tests; the stored file is a normal
  config file afterwards, editable as text. Content size limit is the existing 64 KiB.

The `choices.json` sibling is not used.

Real shape (owner's launch file, 3-GPU split, paths redacted to `<user>`): `exe`
`component:strata-engine`; `args` carry pack/GGUF/MTP paths, `--max-context`,
`--resident-budget-gib`, `--vision`, `--vram-reserve-mib`, `--kv` — **no per-GPU
positional values**; `layer_split` is the scalar `"auto"`; `vram_reserve_mib` a scalar;
`gpu_roles` / `gpu_cards` are keyed by **host card index**, not position; `vision` is an
object with `exe`, `mmproj`, `model`, `gpu`, `max_tokens`. So reordering GPU rows needs
no permutation of other keys. The import's unit tests use a fixture of exactly this
shape with redacted paths (never the real user name). A manual numeric `layer_split`
(e.g. `"24"`) is positional upstream: the import keeps it but shows a warning that
reordering GPUs changes its meaning (decision: mine).

### 4.4 Agent placeholder `${HOST_GPU_IDS_MAIN_LAST}`

New exact-match, non-secret placeholder, sibling of `${HOST_GPU_IDS}`: the spec's
declared, deduplicated host GPU index list with the **first** entry moved to the
**end**, comma-separated (`2,0,1` declared → `0,1,2`; one GPU → itself). Hard error on
a spec with no GPUs (same rule and reasoning as `${HOST_GPU_IDS}`). Allowed in args,
env and config-file content. Gateway-side placeholder validation, portal hints and the
placeholder reference table learn it. Generic name and semantics ("main last"), not
Strata-specific.

### 4.5 Windows process-tree mode (`process_tree`)

New runtime spec field `process_tree`: `direct` | `job`, default `direct`.

- Gateway: append-only migration **113** (`agent_runtime_specs.process_tree text not
  null default 'direct'`), all three stores, the postgres leg verified; spec API
  read/write; validation (closed set, 400 `runtime_spec.process_tree_invalid`);
  launch-affecting (a change relaunches, §5.6 table); pushed to the agent in the spec
  document. If another PR lands migration 113 first, renumber at merge time.
- Portal: a control in the runspec form, shown for native specs, labelled as Windows
  only (it is inert elsewhere: Linux/macOS already use a process group; `wsl` specs
  have their own scope); Strata template selects `job`. Warn when the target agent
  does not advertise the feature flag (§4.6).
- Agent, Windows, native spec with `job`:
  - start with `CREATE_NEW_PROCESS_GROUP` and assign the child to a new job object with
    `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` immediately after start (start suspended and
    resume after assignment, so no grandchild escapes the job). Nested jobs are
    required (Windows 8+); Strata's own job nests inside ours.
  - stdio stays piped to the live log exactly as today.
  - terminate = deliver `CTRL_BREAK_EVENT` to the child's process group; the agent may
    run as a service without a console, so the builder must make delivery work in
    both cases (e.g. the child gets a hidden console of its own and the agent attaches
    to it briefly to generate the event, guarding its own handler). If delivery fails,
    terminate falls through to kill immediately (logged, not an error state).
  - kill (after the existing `killGrace`, 5 s, unchanged) = `TerminateJobObject` → the
    whole tree. Decision (mine, consistent with the owner's "stop as fast as native"):
    no longer grace. In-flight requests are drained by the agent's own `drainGrace`
    (10 s, before the stop signal), so the template passes `--shutdown-timeout 3`
    (Strata's second drain is redundant); the engine's own unmap/QUIT may be cut off by
    the job kill, which frees VRAM immediately and is acceptable.
  - when the root exits on its own, the job handle is closed → any leftover
    descendants die (kill-on-close). The agent holds the handle for the process's
    lifetime and closes it on reap.
  - measurement: per-GPU VRAM rows (PDH, per PID) whose PID is in a managed job are
    attributed to that job's root PID — the Windows counterpart of
    `groupNvidiaComputeApps` on Linux; RAM = sum of private working set over the
    job's process list (`QueryInformationJobObject` /
    `JobObjectBasicProcessIdList`). Processes not in a managed job keep today's
    per-PID attribution.
- Agent, Windows, `direct` (default) and every non-Windows platform: unchanged.
- Docs: replace the KNOWN LIMITATION note in `proc_windows.go` and the architecture
  doc with the opt-in mode.

### 4.6 Agent versioning and negotiation

One branch = one bump: `Version` 0.20.1 → **0.21.0** (MINOR: new flags). Two flags in
`agent.Features`, `Since: "0.21.0"`:
- `runtime_gpu_main_last` — the agent resolves `${HOST_GPU_IDS_MAIN_LAST}`.
- `runtime_process_tree_job` — the agent honours `process_tree = job` on Windows.
Decision (mine): for `process_tree = job` the gateway **refuses** the save (400
`runtime_spec.process_tree_unsupported`) when the spec's agent has reported
capabilities and they lack `runtime_process_tree_job` or its OS is not Windows —
silently running in `direct` mode would leave the engine alive on stop (anti-goal). If
the agent has not reported yet, the save succeeds with a portal warning. For
`${HOST_GPU_IDS_MAIN_LAST}` a warning suffices (an old agent fails the launch visibly
as `not_permitted`, nothing leaks).

Service accounts: supported configuration is the agent running under the account that
installed Strata (the owner's setup, and the WSL precedent). An agent under
LocalSystem or another account is documented as unsupported for Strata
(`%LOCALAPPDATA%`-relative installs and `component:` resolution); Ctrl-Break delivery
must still not hang there — it falls back to the job kill. Rebase onto main's current version before
finalizing (serial PRs collide on the version).

### 4.7 Usage accounting

No code change: passthrough already sums Anthropic cache fields. Add a test that pins
Strata's shape (`input_tokens: 1` + cache fields) for `/v1/messages`, streaming and
non-streaming, so the behaviour cannot regress silently.

### 4.8 Docs

ADR-111 (Strata as application + spec type; run-config import; main-last placeholder;
opt-in Windows process-tree mode). Update `agent-runtime-manager.md` (placeholders,
config files example, Windows process tree, §5.6 launch-affecting table, §7 flags,
§11.5 portal warnings, §15 if a setting is added), `routing-and-model-selection.md` /
`compatibility-and-inference.md` (type, modes, probes), `reference/data-model.md`
(migration 113, type lists), `05-building-block-view.md` if a package changes shape,
README feature claims and `AGENTS.md` project summary (backend list). Screenshots only
if a captured screen changes visibly.

## 5. What is left to the builder

- Exact touchpoint edits (the touchpoint map lists ~70 sites); test-table extensions.
- The Ctrl-Break delivery mechanism for a console-less agent (§4.5), as long as the
  fallback is immediate kill.
- Whether `/props` probing for spec type `strata` runs (it exists on `strata serve`)
  or is skipped like the router types — pick by what keeps a 404 harmless.
- The media-URL guard decision (§4.1).
- Bind-address and template wording, consistent with existing templates.

## 6. Verification and acceptance

CI gates (per Go module `golangci-lint fmt --diff` + `run`, `go test`; portal test,
build, lint, `format:check`; docs check; postgres leg for migration 113; local
`make sonar-scan` + branch-attributed findings). Windows code is compiled by
`GOOS=windows go build`/`go vet` for the agent (CI runs Linux only) — the builder adds
a cross-compile check to local verification.

Windows acceptance on the owner's host (agent running under the owner's account),
done with the owner since the desktop app currently holds the GPUs:
1. The Strata application type against the running desktop-app share: healthy,
   model discovered with context length, Claude Code (`/v1/messages`) and Codex
   (`/v1/responses`) round-trips through the gateway, usage recorded.
2. With the desktop app's model unloaded: the Strata runspec (3 GPUs, main first)
   starts and becomes ready; the engine runs on the intended main card; per-GPU VRAM
   and RAM in the portal match `nvidia-smi` / Task Manager for the whole tree; the
   live log shows server output throughout; Claude Code and Codex round-trips work.
3. Stop: no `python.exe`/`strata.exe`/`strata-vision.exe` of that spec remains, VRAM
   is freed; stop with an in-flight request behaves as drain-then-stop.
4. A `direct` spec on the same agent behaves exactly as before.
