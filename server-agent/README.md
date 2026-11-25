# Server-Agent

> Part of **OnPrem AI Gateway** (OP AI Gateway). For the architecture context, see
> [Telemetry & Observability](../docs/architecture/cross-cutting/telemetry-usage-observability.md)
> and [Certificates & TLS](../docs/architecture/cross-cutting/certificates-tls.md).

A standalone Go binary that collects host + GPU performance metrics (plus an
optional inference `/metrics` scrape) from an AI-server and sends them to the
gateway on an interval, authenticating with a per-server bearer **agent token**.
It also sends a one-time **hardware inventory** report on connect. Telemetry
travels over one of two transports — a persistent **WebSocket** connection (the
default) or a plain HTTP `POST` per sample — see [Transport](#transport-post-vs-websocket).

It is its own Go module (`op-ai-server-agent`) and imports nothing from the
gateway — the wire payload matches the gateway's telemetry contract, nothing
else is shared. Third-party dependencies (both permissive, CGO-free):
[`gopsutil/v4`](https://github.com/shirou/gopsutil) (BSD-3) for host metrics and
[`coder/websocket`](https://github.com/coder/websocket) (ISC) for the WebSocket
transport.

## Getting a per-server agent token

The agent authenticates as a specific AI-server, and that server identity is
derived by the gateway from the **token** — it is never put in the request body
and never logged.

In the portal: **AI-Server → (select the server) → Agent-Token → generate**.
Copy the token once (it is shown only at creation) and hand it to the agent via
`OP_AGENT_TOKEN` / `-token`.

## Configuration

Almost every setting can be given three ways. Precedence, highest first:
**command-line flag > environment variable > config file > built-in default.**
`OP_AGENT_GATEWAY_URL` and `OP_AGENT_TOKEN` are required (from any source).
Four settings are narrower, as their rows say: the two `runtime_allowed_*`
lists have no flag form (env, comma-separated, or the file), and
`cert_proxy_routes` / `cert_proxy_routes_mode` are structured values with
**no env or flag form at all** — config file only.

| Env                     | Flag             | Config key     | Default | Description                                                        |
| ----------------------- | ---------------- | -------------- | ------- | ------------------------------------------------------------------ |
| `OP_AGENT_GATEWAY_URL`  | `-gateway-url`   | `gateway_url`  | —       | Gateway base URL (absolute `http`/`https`), e.g. `https://gw.example`. **Required.** |
| `OP_AGENT_TOKEN`        | `-token`         | `token`        | —       | Per-server agent bearer token. **Required.**                       |
| `OP_AGENT_TRANSPORT`    | `-transport`     | `transport`    | `websocket` | Telemetry transport: `websocket` (one persistent connection) or `post` (one HTTP POST per sample). See [Transport](#transport-post-vs-websocket). |
| `OP_AGENT_INTERVAL`     | `-interval`      | `interval`     | `1s`    | Collection cadence as a Go duration (e.g. `500ms`, `5s`). Clamped up to a **250ms** floor; a non-positive value falls back to `1s`. |
| `OP_AGENT_SYSTEM_REPORT_INTERVAL` | `-system-report-interval` | `system_report_interval` | `30m` | POST-mode re-send cadence for the static hardware inventory (self-heals a gateway restart). Floored at `1m`. Ignored under `websocket` (which re-sends on each reconnect). |
| `OP_AGENT_METRICS_URL`  | `-metrics-url`   | `metrics_url`  | —       | Optional inference `/metrics` (Prometheus text) URL to scrape for active/queued requests. Metric names auto-detected: vLLM or llama.cpp. |
| `OP_AGENT_MODEL_STATUS_URL` | `-model-status-url` | `model_status_url` | — | Optional endpoint polled each cycle for the models currently **loaded**, e.g. `/running` (llama-swap), `/props` (llama.cpp), `/v1/models` (vLLM). Empty disables it. |
| `OP_AGENT_MODEL_STATUS_FORMAT` | `-model-status-format` | `model_status_format` | — (auto) | How to parse `model_status_url`: `openai`, `llama_swap`, `llama_cpp`, `litellm`, or empty / `auto` (a tolerant union of all shapes). |
| `OP_AGENT_TLS_INSECURE` | `-tls-insecure`  | `tls_insecure` | `false` | Skip TLS certificate verification (self-signed dev gateways). `true`/`1`. |
| `OP_AGENT_POWERCAP_ROOT` | `-powercap-root` | `powercap_root` | `/sys/class/powercap` | Optional powercap root the RAPL collector reads instead of the default, e.g. a Proxmox `hostpower` virtiofs mount (`/run/op-hostpower` on Linux, a drive root such as `Z:\` on Windows). Empty uses the platform default (Linux) or disables it (Windows); ignored on macOS. See [Linux or Windows guest on a Proxmox VE host](#linux-or-windows-guest-on-a-proxmox-ve-host). |
| `OP_AGENT_HOSTPOWER_URL` | `-hostpower-url` | `hostpower_url` | — | Optional hostpower HTTP endpoint on the Proxmox host (e.g. `http://<host>:9873`) the guest reads its share of host CPU power from. Empty disables it. Without a token the host identifies the VM by its network port (MAC mode). A value without an `http://` or `https://` scheme or without a host logs a startup warning. |
| `OP_AGENT_HOSTPOWER_TOKEN` | `-hostpower-token` | `hostpower_token` | — | Optional per-VM token for the hostpower HTTP endpoint (token mode; secret; never logged). Create it on the host with `server-agent hostpower token create <vmid>`. Set without `hostpower_url`, it logs a startup warning. |
| `OP_AGENT_HOSTPOWER_TLS_FINGERPRINT` | `-hostpower-tls-fingerprint` | `hostpower_tls_fingerprint` | — | Optional SHA-256 fingerprint of the hostpower TLS certificate (64 hex digits, `:` separators allowed). With an `https://` `hostpower_url` the guest accepts only that certificate. Print it on the host with `server-agent hostpower tls fingerprint`. An invalid value, or one set with an `http://` URL, logs a startup warning and every hostpower request fails; set without `hostpower_url`, it logs a startup warning. |
| `OP_AGENT_HELPER_SOCKET` | `-helper-socket` | `helper_socket` | `/run/op-agent-helper/agent.sock` (Linux) | Unix socket of the privileged `server-agent helper`, from which an agent that does not run as root reads RAPL counters (CPU and system watts) and memory modules. Linux only: on macOS and Windows it is empty, and a configured path logs a startup warning and is ignored. `off` disables it (an empty env var means unset). A relative path logs a startup warning and turns the helper off. A root agent reads both natively and never uses it. Without a helper installed, set `off` to stop the per-sample attempts and the 30 s memory-module retry (both logged at Debug). See [Privileged helper (Linux)](#privileged-helper-linux). |
| `OP_AGENT_LHM_URL`      | `-lhm-url`       | `lhm_url`      | —       | Optional LibreHardwareMonitor Remote Web Server `/data.json` URL for CPU (and best-effort system) power watts, e.g. `http://127.0.0.1:8085/data.json`. Empty disables it. A Windows CPU-watt path (alongside `powercap_root` and `hostpower_url`); a Linux fallback when RAPL is unreadable. |
| `OP_AGENT_CERT_MODE`    | `-cert-mode`     | `cert_mode`    | `off`   | Certificate install mode: `off` (never fetch), `files` (write cert files + run the reload command), or `proxy` (install like `files` **and** run a TLS-terminating reverse proxy in front of the application). See [Certificate installation](#certificate-installation). |
| `OP_AGENT_CERT_DIR`     | `-cert-dir`      | `cert_dir`     | —       | Directory certificate files are written into. **Required** when `cert_mode` is not `off`. |
| `OP_AGENT_CERT_RELOAD_COMMAND` | `-cert-reload-command` | `cert_reload_command` | — | Local shell command run after a changed certificate is fully installed. Comes **only** from this local setting — the gateway can never deliver a command to run. |
| `OP_AGENT_CERT_POLL_INTERVAL` | `-cert-poll-interval` | `cert_poll_interval` | automatic | How often the agent checks the gateway for a new certificate, as a Go duration (e.g. `15m`). Empty or `0`/`0s` means automatic (`websocket` transport polls every 6h, `post` every 15m). A configured positive value is floored at **1 minute** (a faster poll against a key-serving endpoint is a self-inflicted DoS). |
| — (file only) | — (file only) | `cert_proxy_routes` | — (empty) | Local routes for the `cert_mode: proxy` listener, each `{"listen": <port>, "upstream": "http://host:port"}`. Empty means the agent serves only the routes the gateway publishes. Ignored unless `cert_mode` is `proxy`. |
| — (file only) | — (file only) | `cert_proxy_routes_mode` | `fallback` | How a `cert_proxy_routes` entry is merged with a gateway-published route on the **same** listen port: `fallback` (the local route fills only a port the gateway did not publish) or `override` (the local route wins). |
| `OP_AGENT_CA_FILE` | `-ca-file` | `ca_file` | — | Optional operator-managed public CA bundle. Read-only: the agent never overwrites it. |
| `OP_AGENT_CA_CACHE_FILE` | `-ca-cache-file` | `ca_cache_file` | — | Optional agent-managed public CA cache. Generated self-signed mesh configs use `server-agent-ca.pem`. |
| `OP_AGENT_CA_PEM` | `-ca-pem` | `ca_pem` | — | Optional inline public CA bootstrap bundle, generated only when the currently served gateway leaf uses the internal CA. |
| `OP_AGENT_RUNTIME_SOURCE` | `-runtime-source` | `runtime_source` | `gateway` | Where the agent-managed model runtime's launch specs come from: `gateway` (fetched from `GET /api/agent/v1/runtime-config`, portal-maintained) or `file` (read from `runtime_config` locally and reported upward read-only). See [Managed model runtime](#managed-model-runtime). |
| `OP_AGENT_RUNTIME_CONFIG` | `-runtime-config` | `runtime_config` | — | Path to the local runtime-config JSON file. **Required** when `runtime_source` is `file`; ignored otherwise. |
| `OP_AGENT_RUNTIME_ALLOWED_BINARIES` | — (file/env only) | `runtime_allowed_binaries` | — (empty) | Absolute paths a launch spec's `binary` must match **exactly** to be permitted. **Empty means nothing may start at all** — a deliberate hard refusal, not a permissive default. This is the operator's boundary: the gateway decides *when and how* a model process runs, this list decides *whether it may run at all*. Env value is comma-separated. A binary inside a WSL2 distro is an entry `wsl://<distro>/<absolute Linux path>` (see [WSL2 workloads](#wsl2-workloads)); native specs ignore such entries, and a malformed one permits nothing and is logged at startup. |
| `OP_AGENT_RUNTIME_ALLOWED_DIRS` | — (file/env only) | `runtime_allowed_dirs` | — (empty) | Permitted `work_dir` prefixes for launch specs. Unlike the binary allowlist, **empty means any `work_dir`** — an operator who does not care is not forced to enumerate one. Containment is a lexical, path-boundary check; symlinks are not resolved (see `withinDir` in `internal/runtime/policy_local.go` for the reasoning). A bare entry already permits the directory **and everything beneath it**; a trailing whole-segment wildcard — `/srv/models/*` or `C:\models\*` — is an accepted, exactly-equivalent spelling of that subtree, honoured so a config written that way does what its author expects. It is **not** a glob: a glued `/srv/models*` stays literal (it permits no `/srv/models-evil` sibling), and a bare `*` permits nothing. Env value is comma-separated. `wsl://<distro>/<path>` entries bound the `work_dir` of WSL specs in that distro only (no entry for a distro: any absolute `work_dir`, unless `runtime_allow_binary_dirs` restricts it to the binary directories; a malformed `wsl:` dir entry keeps WSL `work_dir` containment on, so only well-formed entries permit one). |
| `OP_AGENT_RUNTIME_ALLOW_BINARY_DIRS` | — (file/env only) | `runtime_allow_binary_dirs` | `false` | When on, each `runtime_allowed_binaries` entry's **parent directory** is treated as an allowed `work_dir` prefix without listing it in `runtime_allowed_dirs` — convenient when a model's `work_dir` sits beside its binary. Same operator-only provenance as the allowlists. The auto-added dirs use the same subtree containment as explicit entries. **Footgun:** turning it on while `runtime_allowed_dirs` is empty flips `work_dir` from "any is accepted" to "only the binary directories and their subtrees" (a spec with no `work_dir` still runs beside its binary either way). |
| `OP_AGENT_RUNTIME_WSL_EXE` | — (file/env only) | `runtime_wsl_exe` | `C:\Program Files\WSL\wsl.exe` on Windows, empty elsewhere | The `wsl.exe` the agent launches WSL2-hosted model processes through (specs whose execution mode is `wsl`). Empty means WSL is not configured and every WSL spec is refused. Operator-only. |
| `OP_AGENT_RUNTIME_WSL_USER` | — (file/env only) | `runtime_wsl_user` | empty | The distro user every WSL process runs as (`wsl.exe -u`). Required for WSL specs; `root` is refused, since no gateway-supplied command line or environment may run with UID 0. |
| `OP_AGENT_WSL_RAM_RESERVE_MB` | — (env only) | — (env only) | `1024` | RAM (MB) held back from the WSL2 VM's pool that WSL specs are admitted against. `0` is a real value; a negative or malformed value falls back to the default. Operator-only. |
| `OP_AGENT_RUNTIME_CACHE` | `-runtime-cache` | `runtime_cache` | `server-agent-runtime.cache.json` next to the binary | Where the last known-good runtime-config document is cached, so the agent can start (and keep) model processes before its first successful gateway contact. A relative config-file value is resolved beside that config file. |
| `OP_AGENT_CONFIG_FILES_DIR` | `-config-files-dir` | `config_files_dir` | `server-agent-config-files` next to the binary | Base directory under which the agent writes the config files a managed runtime spec asks for, one subdirectory per spec. Operator-only: the gateway supplies a file's relative name and content, never where on this host it is written. Point it at a directory of your choosing to keep these files out of the agent's own directory. A relative config-file value is resolved beside that config file. |
| `OP_AGENT_RUNTIME_ROUTER_BIND` | `-runtime-router-bind` | `runtime_router_bind` | — (derive) | Bind host for the managed runtime's router port — the port the gateway sends inference requests to. Operator-only: the gateway supplies the router **port**, never its bind host. Empty means derive: the agent's own mesh identity, read from the **installed mesh leaf in `cert_dir`** — that directory is the only thing consulted (`cert_mode` is not, so a `cert_dir` still populated after the mode went back to `off` does derive an address). With no loadable leaf there, **all interfaces**, with a warning in the agent log. Since the portal's generated config ships `cert_mode: "off"` and `cert_dir: ""`, the default configuration always lands on all interfaces: set this explicitly (mesh IP, or `127.0.0.1`) on any host that is not mesh-only. **Under `cert_mode: "proxy"` it is the other way round** — that mode makes `cert_dir` mandatory, so the empty default resolves to the leaf's mesh address, never all interfaces and never loopback. **Resolved once at process start**, so a boot before the first certificate install binds all interfaces until the agent restarts. See [Managed model runtime](#managed-model-runtime). |
| `OP_AGENT_RUNTIME_LOG_BUFFER_BYTES` | — (file/env only) | `runtime_log_buffer_bytes` | `1048576` (1 MiB) | How much of each managed model process's stdout+stderr the agent keeps **in memory**, in bytes, so an operator can read it after the fact. The buffer belongs to the **spec**, not the process, so a crashed model's output survives it, and a restart appends after a visible boundary marker rather than wiping the history. Below `65536` is raised to it. Operator-only, like the allowlists: memory on this host is the operator's tradeoff and the gateway can never raise it. **Never written to disk** — this content can include prompt text. |
| `OP_AGENT_RUNTIME_LOG_BUFFER_TOTAL_BYTES` | — (file/env only) | `runtime_log_buffer_total_bytes` | `16777216` (16 MiB) | Ceiling on the **sum** of those buffers across every spec, so a server with twenty specs is not twenty times the per-spec number. The agent keeps at most `total / per-spec` buffers, evicting the least-recently-written one nobody is watching. This is the number to reason about when sizing the agent's memory. |
| `OP_AGENT_VRAM_HEADROOM_MB` | — (env only) | — (env only) | `0` | MB held back from each GPU's **auto-detected** VRAM budget, for the driver, a display or another tenant. The agent derives a budget for every GPU it can read from that GPU's total memory, so VRAM admission is enforced by default; a positive per-GPU budget set in the portal wins verbatim and gets no headroom subtracted. `0` is a real value. Operator-only: the gateway can never supply it. |
| `OP_AGENT_HOST_RAM_RESERVE_MB` | — (env only) | — (env only) | `2048` | Host RAM (MB) held back from the pool managed models are admitted against. The pool is the host's live available memory plus what the managed processes already hold, minus this reserve, so it follows what the host has right now (a co-located container stack that grows lowers it). `0` is a real value (reserve nothing). Operator-only. |
| `OP_AGENT_RAM_BOOTSTRAP_GUESS_MB` | — (env only) | — (env only) | `4096` | Conservative host RAM (MB) charged to a model of unknown RAM demand on its first start, until its real footprint has been measured. It errs high on purpose: with headroom the model starts and is measured, on a tight host idle models are evicted (or the start waits) rather than the new one starting blind. `0` turns the charge off. Raise it for a RAM-heavy model whose demand you have not yet entered in the portal. Operator-only. |
| `OP_AGENT_EVICTION_GRACE_SECONDS` | — (env only) | — (env only) | `30` | Seconds after a managed model was last used during which it is **never evicted** to make room for another, so a bursty pipeline (a voice chat's speech models, idle between turns) is not broken by a start that needs the space. A start that could only be made room for by evicting such a model waits, as it does behind a busy one. It adds to the existing rule that a busy, pinned or still-loading model is never evicted, and loosens nothing. `0` turns the window off (an idle model is evictable at once); a negative, fractional, malformed or above-`86400` value falls back to the default. Operator-only. |
| `OP_AGENT_VERBOSE`      | `-v` / `-verbose`| `verbose`      | `false` | Verbose mode: emit detailed **debug** logs to stderr — resolved config (token never logged), each collect cycle, and every telemetry POST with URL, HTTP status, duration, and retry/backoff. Use this to diagnose why the agent can't reach the gateway. |

### Config file

A JSON config file is an alternative to env vars / flags. By default the agent
looks for **`server-agent.json` next to the binary**; override the path with
`-config <path>` or `OP_AGENT_CONFIG`. All keys are optional; an env var or flag of
the same setting overrides the file. A missing default file is fine; an explicitly
requested file that is missing or malformed is a startup error.

Relative `ca_file`, `ca_cache_file`, `runtime_config`, `runtime_cache` and
`config_files_dir` values are resolved beside the actually selected config file (after
flag/environment/file precedence), not against the process working directory.
Absolute paths stay unchanged.

Whole-line `//` comments are tolerated (the file is JSONC-lenient — only lines whose
first non-whitespace characters are `//` are stripped, so a `//` inside a value such
as an `https://` URL is preserved). Block comments and trailing comments are not
supported.

The gateway portal offers a ready-to-use, **annotated** `server-agent.json` to
download (per server, in the "Server-Reporting-Agent" area) with `gateway_url` +
`token` pre-filled and every other key pre-set to its default with an explanatory
comment — the easiest way to start. The same file is available headless via `curl`
with the per-server agent token (the gateway fills `token` from your bearer and
`gateway_url` from the request):

```sh
curl -fL -H "Authorization: Bearer <per-server-agent-token>" \
  https://gw.example/api/agent/v1/download/config -o server-agent.json
```

The example below is a hand-written **abbreviation** with a few values filled
in, not that generated document: the generated one carries *every* key in the
table above, each at its own default and with a comment on what its value — and
its **empty** value — means. A checked-in copy of it lives at
`testdata/server-agent.config.jsonc`.

```jsonc
{
  "gateway_url": "https://gw.example",
  "token": "<per-server-agent-token>",
  "transport": "websocket",
  "interval": "1s",
  "system_report_interval": "30m",
  "metrics_url": "http://127.0.0.1:8000/metrics",
  "lhm_url": "http://127.0.0.1:8085/data.json",
  "cert_mode": "off",
  "cert_dir": "",
  "cert_reload_command": "",
  "cert_poll_interval": "",
  "ca_file": "",
  "ca_cache_file": "",
  "ca_pem": "",
  "runtime_source": "gateway",
  "runtime_allowed_binaries": ["/usr/local/bin/llama-server"],
  "runtime_allowed_dirs": ["/srv/models"],
  "runtime_router_bind": "",
  "runtime_log_buffer_bytes": 0,
  "runtime_log_buffer_total_bytes": 0,
  "tls_insecure": false
}
```

Because the file holds the bearer token, restrict its permissions (e.g.
`chmod 600 server-agent.json`).

### Gateway trust

Gateway HTTPS/WSS trust is additive: the agent starts with the operating
system roots and adds every valid configured source — operator-managed
`ca_file`, P2's `cert_dir/ca.pem`, agent-managed `ca_cache_file`, and inline
`ca_pem`. A missing source is skipped. A changed source is detected from its
content, and unreadable or invalid replacement material leaves the last good
root pool active.

Only `ca_cache_file` is written by the agent. It contains public certificates,
is replaced atomically, and is stored with mode `0644`; `ca_file` and
`cert_dir/ca.pem` are never overwritten by gateway-trust refreshes. POST,
WebSocket, and certificate installation all use this same dynamic root store
and standard hostname/chain/expiry verification. A failed HTTPS or WSS
verification is reported and retried with the existing backoff — the agent
never falls back to HTTP. Only an explicitly configured `tls_insecure=true`
disables verification, for emergency/development use.

### Managed model runtime

When the gateway's portal defines an application of type `server_agent` for this
server, the agent itself starts, health-waits, drains, restarts and idle-unloads
the model-server processes (llama.cpp server, vLLM, …) and reverse-proxies each
inference request to the right one, on a single router port.

Two things stay entirely under the server operator's control, and come **only**
from this local config — never from the gateway:

- **`runtime_allowed_binaries`** — the boundary. A launch spec may only exec an
  absolute path that is in this list, matched exactly. With the list empty
  (the default) **nothing starts**, and every spec reports `not_permitted` with
  that reason. The gateway decides *when and how* a model process runs; this
  decides *whether it may run at all*.
- **`runtime_router_bind`** — where the router port listens. Leaving it empty
  means the agent picks: its own mesh identity if it has one, otherwise **all
  interfaces**, which it warns about at startup. "If it has one" is narrow:
  the identity is read from a loadable mesh leaf in `cert_dir`, and that
  directory is the *only* thing consulted — `cert_mode` is not, so a
  `cert_dir` still populated after the mode was set back to `off` does derive
  an address. The portal's generated config ships `cert_mode: "off"` with an
  **empty `cert_dir`**, and it is that empty directory, not the mode, that
  makes the shipped default bind all interfaces. On a host that is not
  mesh-only, set this explicitly.

  **Under `cert_mode: "proxy"` the empty default is the opposite.** That mode
  makes `cert_dir` mandatory, so once a leaf is installed there the empty
  default resolves to that leaf's **mesh address** — its first IP SAN, else
  its first DNS SAN. Not all interfaces, and not loopback. Read the paragraph
  above as describing `cert_mode: "off"`, which is where the all-interfaces
  fallback actually lives.

  **It is resolved once, at process start.** The value is computed before the
  runtime driver is built and is then fixed for the life of the process. So a
  first boot whose `cert_dir` is still empty binds **all interfaces until the
  agent is restarted**, no matter how many certificates arrive in between —
  and a renewal that changes the leaf's SAN does not move the router until a
  restart either.

  **The proxied path no longer depends on it.** A `server_agent` application
  reached through the agent's own TLS proxy is served by the router **in
  process**, so `runtime_router_bind` cannot make that path unreachable any
  more (it used to: the proxy dialled `http://127.0.0.1:<router port>`, which
  a mesh-derived bind never answered). What still depends on it is where the
  router **listens** — an application routed as plain `http`, and anything
  else that reaches the router directly.

`runtime_allowed_dirs` additionally restricts a spec's `work_dir`; it is
convenience/defence-in-depth, not a boundary (containment is a lexical
path-prefix check and does not resolve symlinks). A bare entry already means
"this directory and its whole subtree"; a trailing whole-segment wildcard
(`/srv/models/*`, `C:\models\*`) is accepted as an exactly-equivalent spelling
of that subtree — it is not a glob, so a glued `/srv/models*` stays literal and
a bare `*` permits nothing.

A spec that sets **no** `work_dir` runs in **the directory its binary lives in**,
whether or not `runtime_allowed_dirs` is set: the binary is on the exact
`runtime_allowed_binaries` allowlist, so its own directory is trusted by
construction, and the spec is permitted on that basis alone — a narrow
`runtime_allowed_dirs` never rejects it. Only a spec that *does* set a `work_dir`
has to place it inside a permitted subtree.

`runtime_allow_binary_dirs` extends that same trust to spec-set `work_dir`s: turn
it on and each allowed binary's **parent directory** becomes an allowed subtree
without listing it in `runtime_allowed_dirs`, so a model whose `work_dir` sits
beside its binary needs no directory entry at all. The auto-added directories go
through the same containment check as explicit entries. Turning it on while
`runtime_allowed_dirs` is empty flips `work_dir` from permissive (any) to
restrictive (the binary subtrees only) — the same flip adding the first
`runtime_allowed_dirs` entry causes, and intended.

#### Managed-process logs

Every managed process's **stdout and stderr** are captured continuously,
whether or not anyone is watching — the point is that an operator arrives
*after* the incident. The buffer belongs to the **spec**, not to the process,
so it survives a crash: opening the log view on a row that says `crashed`
shows the output that led to the crash, not an empty window. Restarts
**append** after a boundary marker carrying the exit code and the new pid, so
a crash loop reads as a sequence of attempts, which is usually the diagnosis.

Sizing is yours: `runtime_log_buffer_bytes` per spec (default 1 MiB) and
`runtime_log_buffer_total_bytes` across all of them (default 16 MiB, i.e.
sixteen retained specs; the least-recently-written unwatched buffer is dropped
to stay inside it).

**Streaming is on demand.** The gateway asks this agent to stream a spec only
while a portal log view is open on it, and stops when the last viewer leaves,
so a fleet nobody is looking at sends no log traffic at all. That direction
needs the WebSocket transport: under `transport: "post"` there is no
gateway→agent channel, so nothing can ask, and the portal says so instead of
showing an empty view.

**What actually ran travels with the output.** Each generation's start marker
carries the **resolved** launch command — binary, argv, working directory and the
complete environment the child received — with `${PORT}`, `${MODEL}`,
`${HOST_GPU_IDS}` and `${AGENT_ENV:NAME}` already substituted, because a template
is not what an operator needs to see when a spec will not start. It rides the
marker rather than a panel of its own, so in a crash loop each attempt shows its
own command inline (`${PORT}` differs every time). An exec that failed outright
gets its own marker kind and no pid: nothing was printed, so the command is all
there is.

Every `${AGENT_ENV:NAME}`-derived value is masked — in **args** as well as in
**env** — as the placeholder that produced it, so the resolved secret never
leaves this host while `--port 54331` and `CUDA_VISIBLE_DEVICES=2,3` stay
readable. In **file mode** the spec's own `env` values are masked too, matching
what the upward report already withholds from a local document; the agent's own
base environment and the values it computed stay visible either way. Masking
happens where the substitution happens, so the agent never retains the plaintext
resolved command at all.

**Nothing is persisted, anywhere.** Captured output can contain prompt text,
so it is held in memory, bounded, and never written to disk, never stored in
the gateway's database, and never put into a log line on either side. The
reported command is treated identically — it is argv and an environment, and has
no more business on disk than the output does. It is
also why retention does not survive an **agent restart** — the portal shows an
explicit "the agent's buffer is empty" rather than letting that read as "the
process printed nothing".

Output the buffer had to drop is never a silent gap: the stream carries an
explicit count of the missing bytes at the point they were lost.

#### Placeholders in `args` and `env` values

Resolved by the agent at launch time in **both** `args` and `env`
values (the table lists the ones an operator reaches for first; the device-list
placeholders `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}` and
`${API_TOKEN}` are described in the architecture documentation). Everything else
shaped like `${…}` is passed through byte-for-byte, so a model server's own
templating still works.

| Placeholder | Becomes | If it cannot be resolved |
|---|---|---|
| `${PORT}` | the port the agent assigned this process | — |
| `${MODEL}` | the spec's `upstream_model` — the **application-side** model name (the mapping's *app model name*), i.e. what the model server itself calls the model | hard error if `upstream_model` is empty |
| `${HOST_GPU_IDS}` | the spec's own GPU indices **as this host numbers them**, in the order the spec declares them, deduplicated and comma-separated (`2,5`) | hard error if the spec declares no GPUs |
| `${HOST_GPU_IDS_MAIN_LAST}` | the same list with the **first** declared GPU (the main card) moved to the end (`2,0,1` → `0,1,2`), for an engine that wants its main card last — Strata's config `gpu` (agent 0.21.0 and later) | hard error if the spec declares no GPUs |
| `${CONFIG_DIR}` | the absolute per-spec directory the agent writes the spec's config files into, `<config_files_dir>/<spec-id>` — see [Config files](#config-files-for-a-managed-process) | — (a plain path; always resolves) |
| `${AGENT_ENV:NAME}` | `NAME` from the **agent's own** process environment | hard error naming the variable; never a silent empty value |

`${MODEL}` is the one to reach for when a command line has to repeat the model
name — `["--alias", "${MODEL}"]`, `"/srv/models/${MODEL}/weights.gguf"`. It is
deliberately the *application-side* name, not the gateway-facing one clients
send, because the child is the thing that has to recognise it. There is no
placeholder for the gateway-facing name.

`${HOST_GPU_IDS}` is for a runtime this agent has no built-in mapping for:
`{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"}` builds the value from
the same GPU rows the admission arithmetic uses, so the two cannot drift. For
NVIDIA and AMD you want `set_visible_devices` (below) instead — this is the
escape hatch, not the normal route. **The name says `HOST` because the numbers
are the host's**: a child launched with those indices renumbers its devices from
`0`, so the same digits mean different cards inside the process.

Two shapes are refused rather than passed through: `${AGENT_ENV:OP_AGENT_*}`
(the agent's own credentials), and a **typo of `${PORT}` or `${AGENT_ENV:…}`** —
anything whose inner text starts with `PORT` or `AGENT_ENV` but is not one of
the exact forms (`${PORTX}`, `${port}`, `${AGENT_ENV:}`). `${MODEL}` and
`${HOST_GPU_IDS}` have no such rule, deliberately: `${MODEL_PATH}`,
`${MODELS_DIR}`, `${MODEL_ID}` and `${GPU_IDS_FILE}` are plausible things to
pass through, so only the exact spellings are substituted — the cost being that
`${MDOEL}`, `${model}` or the shorter `${GPU_IDS}` reaches the child as literal
text instead of erroring. `${CONFIG_DIR}` and `${HOST_GPU_IDS_MAIN_LAST}` follow the
same exact-match rule, so `${CONFIG_DIR_X}` is passed through.

#### Config files for a managed process

A spec can carry up to 16 **config files**, each a name and some text, for a model
server that cannot be driven by its command line alone (audio.cpp reads the models
it serves from a JSON file, for example). The portal's launch-spec form edits them;
the agent writes them **before it starts the process** to
`<config_files_dir>/<spec-id>/<name>` — an agent-owned directory you choose with
`config_files_dir` (see the settings table above), never the spec's `work_dir`,
which may not be writable — and the spec's `args` point the server at them:
`["--config", "${CONFIG_DIR}/server.json"]`. Each file is rewritten on every
launch, atomically, with mode `0600`.

A file's content may use `${PORT}`, `${MODEL}`, `${HOST_GPU_IDS}`,
`${HOST_GPU_IDS_MAIN_LAST}`, the three
`*_DEVICES` lists and `${CONFIG_DIR}`, so a model id written as `${MODEL}` always
matches the spec. **It may never use `${API_TOKEN}` or `${AGENT_ENV:…}`**: a secret
is not written to disk, and the spec is refused (gateway) or reported
`not_permitted` (agent) if one appears. A name that is not one plain file name (a
path separator, `..`) is refused by the gateway and, in a hand-written file-mode
document, fails the whole document load (`invalid_config_file_name`); a name only
this host would mangle — a Windows device name such as `NUL`, a trailing dot — makes
that one spec `not_permitted`. Only a real filesystem failure (an unwritable
directory, a full disk) is a start failure that backs off and retries. Nothing deletes a file you later drop from a
spec, nor a removed spec's directory; clear those by hand. An agent older than
`0.10.0` ignores config files altogether (the portal warns).

In **file mode** the agent's upward report masks every file's content (the name
stays visible), the same way it masks `env` values; the files themselves are still
written for the child.

#### `set_visible_devices`: pinning a model to its GPUs

A spec's GPU rows tell the gateway how much VRAM this model wants on which
cards. On their own they do **not** stop the process from starting on a
different card — and if it does, the accounting is wrong and nothing warns.

Switch **`set_visible_devices`** on (the checkbox in the portal, the
`set_visible_devices` field in a file-mode config) and the agent sets the
variable *your hardware* uses, to *this spec's* indices:

| The agent found | It sets |
|---|---|
| `nvidia-smi` | `CUDA_VISIBLE_DEVICES` |
| `rocm-smi` | `ROCR_VISIBLE_DEVICES` — and **only** that one |
| Apple, or no GPU tooling | nothing at all, and that is not an error |

On AMD only the ROCR level is set on purpose. `HIP_VISIBLE_DEVICES` selects from
what ROCR already left visible, so setting both to the same list leaves the
process with no usable GPU.

Three things worth knowing before you turn it on:

- **Your process renumbers from 0.** Given cards `3,4` it sees devices `0,1`.
  Any argument that names a device number — `--main-gpu`, `--tensor-split` —
  means the *process's* numbering from then on; the GPU rows in the spec keep
  meaning the host's.
- **Turning it on with no GPU rows is refused**, not treated as "no
  restriction". An empty value means *no device is visible*, which would leave
  the model with no GPU at all.
- **Setting one of those variables in `env` yourself while it is on is
  refused** (`CUDA_VISIBLE_DEVICES`, `ROCR_VISIBLE_DEVICES`,
  `HIP_VISIBLE_DEVICES`, any capitalisation). Pick one source. Other selectors
  — `ONEAPI_DEVICE_SELECTOR`, `GPU_DEVICE_ORDINAL` — are yours and compose
  freely with it.

Each spec gets its own value; one model on three cards and another on two do not
interfere. Leaving it off changes nothing about how a spec launches.

#### Memory budgets: VRAM and host RAM are enforced by default

Before it starts a model the agent checks that the models already running, plus the
new one, fit in **GPU memory** (per card) and in **host RAM** (one pool for the whole
machine). Both limits exist without any configuration:

- **VRAM.** Each GPU's budget is its total memory as the agent reads it, minus
  `OP_AGENT_VRAM_HEADROOM_MB` (default `0`). A per-GPU budget you saved in the portal
  wins exactly as you typed it, even above the card; a saved budget of `0` means "none
  set" and is replaced by the detected total. Apple GPUs are skipped (their reported
  "total" is the memory currently allocated, not a ceiling), and on an AMD APU the
  reported VRAM may be a BIOS carve-out smaller than what the GPU can really use: set a
  positive budget there.
- **Host RAM.** The pool is the host's live available memory plus what the managed models
  already hold, minus `OP_AGENT_HOST_RAM_RESERVE_MB` (default `2048`), so a co-located
  container stack that grows tightens it on the next sample with nothing to configure.
  A model is charged the larger of what the agent measured (resident set size; the private
  working set on Windows) and the RAM estimate you entered; with neither it is charged
  `OP_AGENT_RAM_BOOTSTRAP_GUESS_MB` (default `4096`) on its first start, which then runs
  and is measured. Idle models are evicted, oldest first, to make room; other starts wait
  while a first start is still being measured.

This is a behaviour change on existing hosts: a model whose demand is larger than its
card (or than the host's RAM pool) is now refused with `not_permitted` instead of being
started and failing at load. Enter a RAM estimate in the portal for a RAM-heavy model, or
adjust the budget, the reserve or the guess described above.

#### What environment a model process gets

A child gets a **minimal, agent-built environment**, never the agent's own
one — that would hand every model server the agent's gateway token and every
other model's `${AGENT_ENV:…}` secret. It contains exactly:

1. the spec's own `env`, with the placeholders below expanded, and
2. a small **base** copied from the agent's own environment, and only the
   variables the agent actually has (nothing is invented):

| Variable | Copied when set | Why |
|---|---|---|
| `PATH` | always | binary and (on Windows) DLL resolution |
| `HOME` | unix | home/config/cache root |
| `USERPROFILE` | Windows | the Windows home root — `~` for the Hugging Face cache |
| `LOCALAPPDATA` | Windows | per-user cache root; llama.cpp reads it directly |
| `SYSTEMROOT` | Windows | system DLLs **and Winsock init** — a child without it cannot open a socket |
| `WINDIR` | Windows | the same directory under its legacy name |

The Windows entries are not decoration. Windows sets no `HOME`, so before they
existed a Windows child received no home indicator at all and anything
resolving a per-user cache failed — `llama-server` reports that as `failed to
initialize router models: Failed to determine HF cache directory`.

**Every name in that table is reserved: a spec `env` key matching one, in any
capitalisation (`Path`, `SystemRoot`, `userprofile`), is refused** and the spec
reports `not_permitted`. They are agent-owned — a gateway-supplied `PATH` or
`SystemRoot` would choose where a permitted binary resolves its libraries,
which is what the absolute-path allowlist exists to prevent.

To point a model server somewhere else, set **its own** variable in the spec's
`env` — `HF_HOME`, `HF_HUB_CACHE`, `XDG_CACHE_HOME`, `LLAMA_CACHE`, `TEMP`/`TMP`,
or `HOMEDRIVE`+`HOMEPATH`. None of those is reserved, and they are deliberately
left out of the base so they stay yours to set. Overriding `HOME` (or
`USERPROFILE`) to achieve the same thing is the one route that does not work.

#### What the router port serves

Apart from five control paths, **the router routes exclusively on a `model`
field in a JSON request body.** The control paths are four `GET`s — `/health`
(and `/v1/health`), `/running` (llama-swap shape, so the gateway's existing
loaded-model detection works unchanged), `/v1/models` and
`/upstream/{model}/props` (a running child's `/props`; it never starts one) —
and one bodiless `POST`, `/ensure/{model}`, which starts the model's child if
needed and answers `{"status":"running"}` once it is healthy, without
forwarding anything to it. Everything else is a proxied inference request, and
to be proxied it must carry a body that parses as JSON and names a model this
agent manages. Consequences worth knowing before pointing an application at
it:

- A request with no body, a non-JSON body, or no `model` field gets
  **`404 runtime.model_not_managed`** — including WebSocket handshakes, which
  are bodiless `GET`s. There is no `/ws`, no streaming-socket endpoint, and no
  path that upgrades: **protocol upgrades are never proxied**, and a child that
  answers `101 Switching Protocols` anyway has its response refused.
- So a model server whose API is WebSocket-first — text-generation-webui's
  streaming socket, koboldcpp's — cannot be driven through a `server_agent`
  application. Its HTTP/JSON endpoints work; its socket endpoints answer 404.
  This is a deliberate limitation, not an oversight: the router owns a write
  deadline and a per-request in-flight count for every proxied request, and a
  long-lived tunnel is by definition outside both, so supporting one would need
  its own design (an in-flight model for long-lived connections, an idle-based
  deadline, and a drain that can close them) rather than an added upgrade path.
- Request bodies are read fully before routing, capped at **32 MiB**; beyond
  that the answer is `413 runtime.request_too_large`. Responses are never
  buffered — every write is flushed straight through, streaming or not.

Set `runtime_source: "file"` plus `runtime_config: <path>` to own the launch
specs locally instead of in the portal. The file uses the identical JSON schema
the gateway serves; the agent reports the effective document upward with every
env **value** and every config file's **content** redacted (keys and file names
stay visible) and the portal renders it read-only,
including hiding start/stop — whoever owns the config owns the operations.

> **Never write a secret's value into a spec — reference it as
> `${AGENT_ENV:NAME}`.** The upward report masks `env` values, config-file
> content and a spec's `api_token`; `args` are
> reported **verbatim**, so a literal secret in an argument reaches the gateway
> in plaintext and is shown to portal admins. A **placeholder** is safe in
> either field: the report never expands it, and the launch command the log
> stream reports masks every `${AGENT_ENV:NAME}`-derived value — in `args` as
> well as in `env` — as the placeholder that produced it, so the resolved value
> stays on this host.
>
> ```jsonc
> // NO -- a literal value reaches the gateway unmasked, in the report and in stderr
> "args": ["--api-key", "hf_realsecret"],
> "env":  { "HF_TOKEN": "hf_realsecret" },   // masked in the report, but never write it here either
>
> // YES -- a reference. Resolved on this host; masked wherever it is reported.
> "args": ["--api-key", "${AGENT_ENV:HF_TOKEN}"],
> "env":  { "HF_TOKEN": "${AGENT_ENV:HF_TOKEN}" },
> ```
>
> `env` remains the better place regardless, because in **file mode** the report
> — and the reported launch command with it — withholds every `env` value of a
> local document, placeholder or literal, while `args` are sent verbatim.
>
> The report is not the only upward channel either: on a non-zero exit the
> tail of the child's own stderr travels up with the telemetry and is shown on
> the runtime screen, and model servers routinely print their full command
> line at startup. Nothing agent-side can mask a value the child chose to
> print. That is a second reason an argument is the wrong place for a secret,
> and it applies to portal-authored specs too.

#### WSL2 workloads

On a Windows host the agent can run a model server **inside a WSL2
distribution** — vLLM, SGLang and other Linux-only engines — and manage it like
any other: start on demand, drain, stop, crash backoff, the runtime log, and the
memory budgets above. In the portal, set the spec's **Execution** to WSL, name
the distro, and give the binary and working directory as **Linux paths** inside
it. The selector appears once this agent (0.19.0 or later) has reported. The
agent stops the workload through a systemd scope inside the distro, measures its
RAM from that scope, and attributes its VRAM from the WSL VM's GPU memory.

Installing WSL, the distro and the engines is yours to do; the agent needs:

- **The agent runs as the Windows user that owns the distro**, never as
  LocalSystem. WSL is per Windows user (distros, the VM and `.wslconfig` all
  belong to one account) and refuses LocalSystem outright. Run the agent either
  as a **service under that user's account** (a service runs as LocalSystem
  unless you set its log-on account; with NSSM, set it on the *Log on* tab), which
  works with nobody logged on, or as a **plain process in that user's
  interactive session**, which ends at logoff together with every model it
  started.
- **systemd in the distro**, and **linger** for the user the workloads run as:
  `[boot]` `systemd=true` in the distro's `/etc/wsl.conf`, then
  `sudo loginctl enable-linger <linux user>` once, so that user's systemd manager
  is running whenever the agent needs it. That Linux user is
  `runtime_wsl_user`; it may not be `root` (or UID 0).
- **Keep the VM up.** In that Windows user's `%UserProfile%\.wslconfig`:

  ```ini
  [wsl2]
  vmIdleTimeout=-1

  [general]
  instanceIdleTimeout=-1
  ```

  Otherwise WSL shuts an idle distro (after 15 s) and VM (after 60 s) down under
  a loaded model. `[wsl2]` `memory=` is the VM's own RAM ceiling (half of the
  host's by default); the agent admits WSL models against it as well, minus
  `OP_AGENT_WSL_RAM_RESERVE_MB`.
- **Mirrored networking.** Add `networkingMode=mirrored` under `[wsl2]` in the
  same `.wslconfig`, then run `wsl --shutdown` once. The agent reaches the model
  at `127.0.0.1:<port>` from Windows. In a live test, WSL's default NAT mode did
  not forward a server listening on `127.0.0.1` inside the distro: from Windows
  the port refused the connection while it answered inside the distro. The
  symptom is a WSL spec that stays *starting* until its startup timeout, while
  the runtime log shows the server listening. To check, run
  `curl.exe http://127.0.0.1:<port>/health` on Windows while the model starts.
- **Models in the distro's own ext4 filesystem** (for example under
  `/opt` or the user's home), not under `/mnt/c`, which is far slower.
- **`build-essential` in the distro for vLLM** (`sudo apt install
  build-essential`): its Triton kernels are compiled with the system C compiler
  when the server starts.
- **The Windows NVIDIA driver only**: no Linux GPU driver inside the distro.
  The distro sees the host's GPU indices, so the spec's GPU list means the same
  cards as for a native spec.
- **The WSL settings.** Nothing starts in WSL mode until `runtime_wsl_user` is
  set and `runtime_allowed_binaries` holds a `wsl://<distro>/<absolute Linux
  path>` entry for the binary (`runtime_wsl_exe` defaults to
  `C:\Program Files\WSL\wsl.exe`). The same list holds your native binaries;
  a spec in a distro is permitted only by an entry for that distro, and native
  specs ignore `wsl://` entries. `runtime_allowed_dirs` takes `wsl://` entries
  the same way (and `runtime_allow_binary_dirs` covers the directory of each
  `wsl://` binary):

  ```json
  "runtime_wsl_user": "<linux user>",
  "runtime_allowed_binaries": ["wsl://Ubuntu-24.04/opt/vllm/.venv/bin/vllm"],
  "runtime_allowed_dirs": ["wsl://Ubuntu-24.04/opt/vllm"]
  ```

  A malformed `wsl:` entry in `runtime_allowed_dirs` (a backslash or a bad
  distro name after `wsl://`) is ignored and logged at startup, and so is a
  look-alike such as a wrong-case `WSL://` or a `wsl:/` typo, which is not a WSL
  entry but a native dir entry (it turns native `work_dir` containment on).
  Either way WSL `work_dir` containment stays on: only well-formed `wsl://` dir
  entries (and the binary directories with `runtime_allow_binary_dirs`) then
  permit a `work_dir`. An agent before 0.19.1 has no `wsl://` concept (such an
  entry is a useless native binary entry, or a native dir entry that turns
  native `work_dir` containment on) and still needs the old settings, so
  upgrade the agent first.

  The earlier `runtime_wsl_allowed_distros`, `runtime_wsl_allowed_binaries` and
  `runtime_wsl_allowed_dirs` settings (and their `OP_AGENT_RUNTIME_WSL_ALLOWED_*`
  environment variables) are gone. An old config that still carries one starts
  anyway; the agent logs a warning naming the replacement and ignores it.

For vLLM, bind `--host 127.0.0.1 --port ${PORT}` (reachable from Windows with
mirrored networking, above) and set `--gpu-memory-utilization` explicitly: the agent attributes
VRAM to a WSL model as the VM's growth across that model's start, which is a
stable figure when the engine reserves a fixed share of the card. Pass the API key
as `VLLM_API_KEY` in `env` rather than as `--api-key`, since an argument is
visible to `ps` inside the VM.

A wheel-only SGLang or sglang-omni install (a uv venv, no CUDA toolkit) needs
`CUDA_HOME=<venv>/lib/python3.12/site-packages/nvidia/cu13` and
`LIBRARY_PATH=/usr/lib/wsl/lib` in the spec `env`, `build-essential` and
`ninja-build` in the distro (a spec cannot set `PATH`, so the venv's own
`ninja` is not found), and the CUDA wheel fixes from
[sgl-project/sglang#49209](https://github.com/sgl-project/sglang/issues/49209).
`env` values are not expanded by a shell: write absolute paths. Details in
ADR-103.

**The WSL boundary is not a security boundary.** A Linux process can read the
Windows user's files under `/mnt/<drive>` and, through interop, start Windows
programs as that user, so a WSL model has the trust level of a native binary run
as the agent's account. Use a **dedicated distro** for the models and set
`[interop]` `enabled=false` in its `/etc/wsl.conf`. If no WSL spec uses config
files (`${CONFIG_DIR}` resolves to the agent's directory under `/mnt/<drive>`),
you can also turn the drive mounts off with `[automount]` `enabled=false`.

#### Strata on Windows

The portal's **Strata** spec type launches `strata serve` from the Strata Terminal
install. On the agent's side it needs:

- **The agent runs under the Windows account that installed Strata.** The install
  lives under that user's `%LOCALAPPDATA%`, and the run config's `component:`
  references resolve per user. An agent under LocalSystem or another account is not
  supported for Strata (a service runs as LocalSystem unless you set its log-on
  account; with NSSM, set it on the *Log on* tab).
- **The absolute path of `strata.cmd` in `runtime_allowed_binaries`**, exactly as the
  spec's binary field names it:

  ```json
  "runtime_allowed_binaries": [
    "C:\\Users\\<user>\\AppData\\Local\\Strata\\terminal\\current\\bin\\strata.cmd"
  ]
  ```

- **Agent 0.21.0 or later**, for `${HOST_GPU_IDS_MAIN_LAST}` (the run config's `gpu`
  value: the first GPU row is the main card, and Strata wants it last; `strata serve` has no `--gpu` flag) and for the
  process tree below.

The template sets **`process_tree: job`**: the agent starts `strata.cmd` suspended
inside a Windows job object and resumes it once assigned, so Strata's `python.exe`
and engine processes are all in the job. A stop sends Ctrl-Break to the tree (Strata
drains and quits its engine), and after the 5 s kill grace the whole job is
terminated; RAM and per-GPU VRAM are measured over every process in the job. If the
launcher exits, anything it left behind is killed at once, and if the agent itself
dies, Windows ends the whole job with it. A spec left at `direct` (the default) is
stopped by killing the launcher alone, which leaves the engine running, so keep `job`
for Strata. **Verified on hardware (2026-11-23):** the job-mode start with `strata.cmd`, per-GPU VRAM over the whole tree and the agent's probes, and the stop: Ctrl-Break reached Strata, which drained and exited 0 about 2.7 s after the stop; cmd.exe's "Terminate batch job" prompt only adds a log line and a root exit code of -1, and VRAM was freed within about 2 s, so `strata.cmd` stays the binary. The measured RAM (private working set over the job) under-counts: it read 7.7 GB while host RAM in use fell by about 11.5 GB at stop. **Still pending:** the RAM figure against Task Manager beyond that, Claude Code and Codex round trips and the console-less (service) agent path.

The key goes in the spec's environment as `STRATA_API_KEY=${API_TOKEN}`, never in
the run config file; the agent's own probes of the child (metrics, context,
capabilities) send it too. The template turns **visible devices on in args mode**
("Via args or config file"): the agent then sets no `CUDA_VISIBLE_DEVICES` (which
would renumber the cards from 0), and the run config's `gpu` list, in the host's card
numbers, is what restricts Strata to the spec's cards. Never switch the mode to
`env` for Strata. Strata's own engine wait defaults to 900 s, so the template's startup
timeout is 960 s; raise the parent `server_agent` application's timeout to at least
960000 ms, or the portal warns that a cold start can be cut short.

## Build & run

```sh
cd server-agent
go build -o server-agent .

OP_AGENT_GATEWAY_URL=https://gw.example \
OP_AGENT_TOKEN=<per-server-agent-token> \
./server-agent
```

Or with flags:

```sh
./server-agent -gateway-url=https://gw.example -token=<token> -interval=5s
```

The agent runs until it receives `SIGINT` (Ctrl-C) or `SIGTERM`, then shuts
down cleanly.

## Transport: POST vs WebSocket

The agent sends telemetry to the gateway over one of two transports, selected by
`OP_AGENT_TRANSPORT` / `-transport` / `transport` (config file). **The default is
`websocket`.**

- **`websocket`** (default) — the agent opens **one persistent WebSocket connection**
  to `/api/agent/v1/stream` and streams each sample as a frame. Per-sample overhead is
  just the frame bytes (no per-request HTTP headers, no response round-trip), so a
  **high send frequency** (down to the 250ms interval floor) is cheap. This is also
  the channel future live-streaming features use. **Deployment note:** over the NetBird
  mesh it needs no extra setup; reached through the **public nginx ingress** it needs a
  WebSocket `Upgrade` location for `/api/agent/v1/stream` (already in the bundled
  `gateway/deploy/nginx` configs — a custom reverse proxy in front must add one).
- **`post`** — one HTTP `POST` to `/api/agent/v1/telemetry` per sample, on the
  `OP_AGENT_INTERVAL` cadence. A single keep-alive connection is reused, so there is no
  TCP/TLS handshake per sample. Simple and works through any plain HTTP proxy.

Use plain HTTP POST instead by setting the transport to `post` — e.g.:

```sh
# environment variable
OP_AGENT_TRANSPORT=post ./server-agent

# or flag
./server-agent -gateway-url=https://gw.example -token=<token> -transport=post -interval=1s

# or in server-agent.json
{ "transport": "post", "interval": "1s" }
```

Removing the setting falls back to the `websocket` default. No gateway configuration
change is needed; the gateway accepts both transports simultaneously (different agents
can use different transports).

**WebSocket resilience.** The connection survives a gateway restart/redeploy: on a
graceful gateway shutdown the agent receives a clean close and reconnects within
seconds; on an error it reconnects with exponential backoff + jitter (infinite
retries). A silent/half-open gateway is detected by a keepalive ping probe (~40s)
and reconnected. Samples during a disconnect are dropped (latest-wins telemetry);
the hardware inventory is re-sent on every reconnect.

**Deployment note (public path only).** When the agent reaches the gateway over
the **NetBird mesh** (the usual setup), WebSocket needs no extra configuration.
When it reaches the gateway through the **public nginx ingress**, that nginx needs
a WebSocket `Upgrade` location for `/api/agent/v1/stream` (already included in the
bundled `gateway/deploy/nginx` configs). POST needs no such thing.

## Certificate installation

`cert_mode` (see [Configuration](#configuration)) controls whether the agent
fetches and installs the TLS certificate the gateway issues for this server —
Phase 2 of the gateway's certificate feature (see `gateway/deploy/README-certificates.md`
for the operator-facing runbook). Default is `off`: the agent never contacts the
certificate endpoint.

- **`off`** (default) — never fetch, never write anything.
- **`files`** — the agent periodically checks the gateway for this server's
  certificate and, on a real change, atomically writes five files into `cert_dir`:

  | File            | Contents                              | Mode       |
  | --------------- | -------------------------------------- | ---------- |
  | `fullchain.pem`  | leaf + chain (what most servers want)  | `0644`     |
  | `cert.pem`       | leaf only                              | `0644`     |
  | `chain.pem`      | chain only (no leaf)                   | `0644`     |
  | `ca.pem`         | the gateway's public CA bundle         | `0644`     |
  | `privkey.pem`    | the private key                        | **`0600`** |

  After a successful, changed installation the agent runs `cert_reload_command`
  (a plain local shell command, e.g. `systemctl reload nginx` or a custom script)
  so the local TLS consumer picks up the new certificate. **This command comes
  ONLY from this local config setting — the gateway can never deliver a command
  to run** (there is no field, frame, or code path anywhere in the protocol that
  lets it). On Windows, keep the command **quote-free** (no embedded quotes) —
  it runs via `cmd /C` with the raw string, so Go's usual argument-quoting rules
  do not apply.
- **`proxy`** — installs the certificate like `files` **and** additionally runs a
  TLS-terminating **reverse proxy** in front of the local application, so the
  gateway can reach it over HTTPS on a gateway-assigned proxy listen port. The
  agent fetches its proxy routes from the gateway, terminates TLS with the
  installed leaf, and forwards to the local upstream; the gateway reconciles the
  application's automatic HTTP→HTTPS switch. See
  [Certificates & TLS](../docs/architecture/cross-cutting/certificates-tls.md) for
  the full mechanism and `gateway/deploy/README-certificates.md` for the
  operator-facing runbook.

**Ownership.** `privkey.pem` is written `0600`, owned by whichever user the
agent process runs as. Whatever process actually consumes the TLS material
(nginx, a reverse proxy, …) must be able to **read** that file — either run it
as the same user the agent runs as, or have `cert_reload_command` `chown`/
`install` the files to the consumer's user immediately after writing. The agent
also needs write access to `cert_dir` (and permission to create it if missing).

**A failed fetch never touches existing files.** If the gateway is unreachable,
has no certificate configured for this server yet, or the request otherwise
fails, the agent leaves whatever is already on disk exactly as it is — no
partial write, no deletion, no empty file, no reload attempt. The next
successful fetch is what replaces the files, in one atomic pass per file.

## Collector auto-detection

On startup the agent probes for the GPU tooling present on the host and enables
each collector whose backing CLI/OS is available (checked in the order
nvidia → amd → apple):

- **NVIDIA** — `nvidia-smi` on `PATH`.
- **AMD** — `rocm-smi` on `PATH`.
- **Apple** — `ioreg` on a `darwin` host.

Hosts with no supported GPU tooling simply report host metrics only. The host
collector (CPU / memory / swap / load / network via `gopsutil`) always runs.

## Optional inference scrape

When `-metrics-url` / `OP_AGENT_METRICS_URL` is set, the agent additionally GETs
that Prometheus-text endpoint each cycle and reports the running/queued request
counts. The metric names are auto-detected per inference server (no format
setting): **vLLM** (`vllm:num_requests_running` → active,
`vllm:num_requests_waiting` → queue depth) and **llama.cpp** /
`llama-server --metrics` (`llamacpp:requests_processing` → active,
`llamacpp:requests_deferred` → queue depth). Any other exposition yields 0/0; a
scrape failure is logged and skipped without stopping the loop.

## Hardware inventory

On startup the agent collects a **static hardware inventory** once and sends it to
the gateway (over whichever transport is configured — a `system_report` frame on
each WebSocket connect, or a one-time + periodic `POST /api/agent/v1/system-report`
under POST mode). The gateway persists it per server, so the portal's per-server
**Hardware** view shows it even while the agent is offline.

Collected: CPU (model, vendor, physical cores, logical threads, base clock), RAM
total and — best-effort — per-DIMM (size/type/speed), mainboard + BIOS (vendor,
product, version), GPUs (model, VRAM, driver version), and OS/kernel/arch/hostname.
Everything is best-effort — a field the platform doesn't expose is simply blank.

- **Per-DIMM detail** is free on Windows and Intel macOS; Apple Silicon uses unified
  memory (total only). On **Linux** the DIMMs come from the SMBIOS table
  `/sys/firmware/dmi/tables/DMI`, which only root can read: an agent running as root
  parses it in-process, an agent running as an ordinary user gets the same list from
  the [privileged helper](#privileged-helper-linux), which parses it with the same
  code. Without either, RAM total only. `dmidecode` is not used and need not be
  installed. When an unprivileged agent's startup inventory has no modules (the
  helper did not answer, or answered with none), the agent retries just this
  fetch every 30 s until the first successful reply. A reply with modules
  updates the inventory, which is re-sent (Info `memory modules received from the
  helper`), so a helper that starts late still fills the **Hardware** view; an
  empty reply also ends the retry (Info `helper reports no memory modules`).
- **Privacy:** the agent never collects serial numbers, board/product/chassis UUIDs,
  or MAC addresses (and the gateway stores only a canonical blob with no such fields).

## Power metrics (CPU + total system watts)

The agent reports two best-effort, nullable host-level power metrics — **CPU package
watts** and **total system watts** — shown as their own charts in the portal's
"Leistung" (Performance) view. A metric is simply absent (no chart) where the
platform, permissions, or hardware do not provide it. There is no configuration for
the native path; the optional LibreHardwareMonitor source is enabled with
`OP_AGENT_LHM_URL`.

Per-platform prerequisites:

- **Linux (native RAPL, privileged helper, hwmon).** CPU watts come from the powercap
  RAPL sysfs energy counters (`/sys/class/powercap/intel-rapl:*/energy_uj`, the
  `package*` domains summed); system watts from a RAPL `psys` domain if present, else
  a hwmon power sensor (`/sys/class/hwmon/*/power*_input`, world-readable).
  `energy_uj` is root-only (mode 0400) since the PLATYPUS/CVE-2020-8694 mitigation.
  An agent running as root reads it directly. An agent running as an ordinary user
  reads the same counters from the **[privileged helper](#privileged-helper-linux)**
  (`server-agent helper`, a small root service) over a Unix socket; `helper_socket`
  defaults to the helper's socket on Linux, so installing the helper unit is all it
  takes. The sources run in the order native RAPL → helper → hwmon → hostpower HTTP →
  LibreHardwareMonitor, and the first non-null value per metric wins: a root agent (or
  one a udev rule grants read access to `energy_uj`) is unaffected by the helper, and
  an unprivileged agent gets the helper's `psys` before the hwmon sensor, exactly as a
  root agent would. The helper is never consulted when the agent runs as root. If
  neither root nor the helper is available → CPU watts `null` (no chart).
  `psys`/hwmon power sensors are often absent on rack servers → system watts `null`.
  RAPL is an energy counter, so the **first sample after start is `null`** (no delta
  yet).

- **macOS (native powermetrics).** CPU watts come from
  `powermetrics --samplers cpu_power`, which **requires root/sudo**. Run the agent as
  root or configure passwordless sudo for `powermetrics`. If unmet → CPU watts
  `null`. Total system watts is never available on macOS CGO-free → always `null`.

- **Windows.** There is no native CGO-free path (RAPL is ring-0/MSR). CPU watts are
  available via the optional LibreHardwareMonitor source below, or, for a guest on a
  Proxmox host, from the host's share (virtiofs or HTTP, see the last item); system
  watts are `null`.

- **LibreHardwareMonitor source (any OS, opt-in).** Set `OP_AGENT_LHM_URL` to the
  operator's LibreHardwareMonitor Remote Web Server `/data.json`
  (e.g. `http://127.0.0.1:8085/data.json`). Install **LibreHardwareMonitor ≥ 0.9.6**,
  run it elevated (its PawnIO driver must be allowed by HVCI/Memory-Integrity/
  Smart-App-Control/AV), and enable **Options → Remote Web Server**. If basic-auth is
  enabled, embed credentials in the URL. The agent ships/links nothing from LHM — the
  operator installs it. Unset/unreachable → power `null` (no chart). Per metric, the
  native reading wins and LHM fills gaps (native first).

- **Linux or Windows guest, host-attributed (opt-in).** Set `OP_AGENT_POWERCAP_ROOT` to a
  mounted `server-agent hostpower` tree, or `OP_AGENT_HOSTPOWER_URL` to its HTTP endpoint; see
  [below](#linux-or-windows-guest-on-a-proxmox-ve-host).

### Linux or Windows guest on a Proxmox VE host

A KVM guest has no RAPL: the MSRs are not passed through and
`/sys/class/powercap` is empty (Windows has no powercap at all), so
`cpu_power_w` stays `null` there. Instead, a small service on the Proxmox VE
(PVE 9.2) host measures the host's RAPL package energy, attributes it to each
VM by CPU time, and hands every VM its own cumulative energy counter over one
of three transports. The guest agent turns the counter into watts, exactly as
it does for bare-metal RAPL. This works for one PVE host; there is no cluster
support (see Limitations).

**1. Host service (once per host).** Install virtiofsd if you use virtiofs
(`apt install virtiofsd`), download the Linux agent binary to
`/usr/local/bin/server-agent`, and run it in `hostpower` mode as root (RAPL
`energy_uj` is root-only) under systemd, for example
`/etc/systemd/system/op-hostpower.service`:

```
[Unit]
Description=OnPrem AI Gateway hostpower (VM CPU power share)
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/server-agent hostpower -out /var/lib/op-hostpower
Restart=always
RestartSec=5s
StateDirectory=op-hostpower op-hostpower-tls
StateDirectoryMode=0700
ProtectSystem=strict
ReadWritePaths=/var/lib/op-hostpower /var/lib/op-hostpower-tls
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

To also serve the HTTP transports, add a listen address (the host's address
on the VM bridge, here a documentation address):

```
ExecStart=/usr/local/bin/server-agent hostpower -out /var/lib/op-hostpower -listen 192.0.2.10:9873
```

`ProtectSystem=strict` makes the whole file system read-only for the service
except `/dev`, `/proc` and `/sys`, which it only reads; `ReadWritePaths`
leaves the out directory and the TLS directory `/var/lib/op-hostpower-tls`
as its only writable paths, and `StateDirectory` creates both under
`/var/lib` before the unit starts (a missing `ReadWritePaths` directory fails
the start). The TLS directory is used only with `-tls` (see
[HTTPS with a pinned certificate](#https-with-a-pinned-certificate-both-modes));
it is a directory of its own because the out directory is the source of the
guests' shares and must never hold the private key. `network-online.target` and
`RestartSec=5s` matter with `-listen`: if the unit starts before the bridge
address exists, the listen fails and the service exits with 2, and with the
default 100 ms restart delay it would hit systemd's start limit and stay failed,
which also stops every virtiofs counter. If the PVE datacenter or host
firewall is on, allow TCP 9873 inbound on the host from the VMs. `StateDirectoryMode=0700` applies to
both directories: it keeps the per-VM trees and the TLS key readable by root
only; guests are unaffected because virtiofsd runs as root. The service creates the out directory `0700` when it creates it itself.

Usage: `server-agent hostpower [-out dir] [-interval duration]
[-listen host:port] [-auth mac,token] [-tokens path] [-tls]
[-tls-cert path] [-tls-key path] [-v]`.

| Option | Default | Meaning |
|---|---|---|
| `-out DIR` | `/var/lib/op-hostpower` | Where the per-VM trees are written |
| `-interval D` | `250ms` (minimum `100ms`) | Tick length; see the cadence rule below |
| `-listen host:port` | empty (HTTP off) | Serve the HTTP endpoint on this address |
| `-auth mac,token` | `mac,token` | Enabled HTTP identification modes |
| `-tokens PATH` | `/etc/op-hostpower/tokens.json` | Token store for token mode |
| `-tls` | off | Serve the `-listen` endpoint over HTTPS (TLS 1.3 only); needs `-listen` |
| `-tls-cert PATH` | `/var/lib/op-hostpower-tls/tls.crt` | TLS certificate (PEM); generated together with the key when both are absent |
| `-tls-key PATH` | `/var/lib/op-hostpower-tls/tls.key` | TLS private key (PEM) |
| `-v` | off | Debug logging |

Cadence rule: keep `-interval` at most a quarter of the guest's
`OP_AGENT_INTERVAL`, so each guest reading spans several host updates; a host
tick near the guest interval makes readings alternate between 0 W and twice
the true value. Keep the guest at its 1 s default `OP_AGENT_INTERVAL`: at the
agent's 250 ms floor the quarter rule would need a 62.5 ms host tick, below
the host's 100 ms minimum. The service needs no gateway token and contacts no
gateway; it opens a listener only when `-listen` is set. It warns at startup
when the host has no RAPL domain, when RAPL `energy_uj` is not readable (not
root; guests would report 0 W) or when there is no `qemu.slice`, and keeps
running. At startup it also removes leftover temp files from an earlier crash.
With `-auth` containing `mac` on a non-Linux build, or without the privilege
to open a raw socket (it needs `CAP_NET_RAW`, which the root service has), the
service refuses to start (exit 2); use `-auth token` there.

**2. Read-only source for the shares (once per host, recommended for
virtiofs).** Every share is writable from inside its VM unless its source is
read-only, so a guest could fill the host's root file system or plant symlinks
in it. Map the VMs to a read-only bind mount of the out directory instead of
the directory the service writes, for example with the host fstab line

```
/var/lib/op-hostpower /run/op-hostpower-ro none bind,ro 0 0
```

(create both directories once with
`mkdir -p /var/lib/op-hostpower /run/op-hostpower-ro`, then
`systemctl daemon-reload && mount /run/op-hostpower-ro`; at boot systemd
recreates the mount point under `/run` itself). An fstab mount (or an
equivalent systemd `.mount` unit) is part of `local-fs.target`, so it is
re-established on every boot before PVE autostarts VMs. The service keeps
writing to `/var/lib/op-hostpower`, and new VM directories appear in the bind.
The service confines every write to the VM's own directory regardless (a
symlink that leaves it is refused) and never reads a FIFO, other special file
or oversized file a guest placed there (it replaces it), as defense in depth.

**3. Choose a transport per VM.** All three run side by side from the one
service.

| Situation | Transport |
|---|---|
| Linux or Windows guest on the host's Linux bridge (the common case) | HTTP, MAC mode: set only `hostpower_url` |
| Guest not on the host's bridge (routed, NAT, Open vSwitch, SDN), or IPv6 only (then `-listen` must be an IPv6 address) | HTTP, token mode: `hostpower_url` and `hostpower_token` |
| Guest behind a router, NAT or proxy VM (for example an OPNsense or pfSense VM, or a reverse-proxy VM) | HTTP, token mode; MAC mode would serve it the router VM's counter |
| No network path between guest and host wanted, no migration needed | virtiofs |

Use exactly one transport per guest. If several are configured the collector
order decides (native/virtiofs first, then on Linux the privileged helper and
hwmon, then hostpower HTTP, then LibreHardwareMonitor) and only one value is
reported, so nothing is double counted. A guest has no helper: set
`helper_socket` to `off` in an unprivileged Linux guest (see
[Privileged helper (Linux)](#privileged-helper-linux)).

#### virtiofs (Linux and Windows guests)

The directory `/var/lib/op-hostpower/<vmid>` appears only after the service
has ticked while that VM is running (it discovers VMs by their live
`qemu.slice/<vmid>.scope`); for a stopped VM create it yourself with
`mkdir -p /var/lib/op-hostpower/<vmid>` (Linux guest) or
`mkdir -p /var/lib/op-hostpower/<vmid>/windows` (Windows guest, whose mapping
points at that subdirectory; virtiofsd needs it to exist or the VM fails to start). Each VM gets two trees:

- `<vmid>/intel-rapl:0/` for Linux guests,
- `<vmid>/windows/intel-rapl_0/` for Windows guests (the same files; the name
  has no `:`: a colon is not a legal Windows file-name character and how
  virtio-win presents such a host name is unverified, so the Windows tree avoids it).

Map the read-only view and attach it with the virtiofs cache disabled. For a
**Linux guest** map `<vmid>`:

```
pvesh create /cluster/mapping/dir --id hostpower-<vmid> --map node=<node>,path=/run/op-hostpower-ro/<vmid>
qm set <vmid> --virtiofs0 dirid=hostpower-<vmid>,cache=never
```

For a **Windows guest** map `<vmid>/windows` instead
(`path=/run/op-hostpower-ro/<vmid>/windows`) with the same `qm set` line.

`cache=never` matters: with PVE's default `cache=auto` the guest can read a
value up to about 1 s old after the host replaced the file, which brings back
the alternation between 0 W and twice the true value.

*Linux guest.* Mount the share read-only, for example with the fstab line
`hostpower-<vmid> /run/op-hostpower virtiofs ro,nofail 0 0`, and point the
agent at it with `OP_AGENT_POWERCAP_ROOT=/run/op-hostpower` (or
`-powercap-root` or `powercap_root` in the config file). The resolved
`powercap_root` is logged at debug level (`-v`); the startup `power_sources`
always lists `rapl` on Linux, so it does not prove the mount works. To check:
`cat /run/op-hostpower/intel-rapl:0/energy_uj` in the guest must rise between
two reads.

*Windows guest.* Install the virtio-win guest tools (they provide the
VirtIO-FS service) and WinFsp, configure the VirtIO-FS service with the mapping
tag and a drive letter, and set `powercap_root` to that drive root, for example
`"powercap_root": "Z:\\"` in the JSON config file (or `OP_AGENT_POWERCAP_ROOT=Z:\`).
Do not expose ACLs through the share (virtio-win guidance). The agent service
account (commonly LocalSystem) must be able to see the drive. To check: the
file `Z:\intel-rapl_0\energy_uj` must rise between two reads.

In both cases `cpu_power_w` turns non-null from the second sample (the first
is `null`, a RAPL delta, as on bare metal). Under steady load it must stay
roughly level; if it alternates between about 0 and about twice the expected
value, check `cache=never` and the cadence rule.

#### HTTP, MAC mode (no secret)

Set only the host URL in the guest, with `OP_AGENT_HOSTPOWER_URL=http://192.0.2.10:9873`
(or `-hostpower-url` or `hostpower_url` in the config file) and no token. The
guest asks `GET /v1/energy`; the host identifies the VM from the network path
itself and answers with that VM's counter.

How the host knows who is asking: the service watches (with a Linux
`AF_PACKET` raw socket) the TCP SYN of every connection to the listen port and
records which interface it entered on and its Ethernet source MAC. Only an
interface named `tap<vmid>i<n>` counts as a VM port, so the VM ID comes from
the bridge port the connection physically entered, not from anything the
sender claims, and every segment observed before the decision must enter on the
same tap (or on a transit interface such as the bridge). The SYN's source MAC
must also equal the MAC of that NIC (`net<n>`) in the VM's current config
(`/etc/pve/qemu-server/<vmid>.conf`, not its snapshots). ARP and the bridge
forwarding table are never consulted.

Requirements: Linux host, `CAP_NET_RAW` (root service), IPv4 only, and the VM's
NIC must be a QEMU tap on this host. It works with the PVE firewall enabled on
the NIC (the `fwbr`/`fwln`/`fwpr` interfaces are transit) and with the host
address on a VLAN device over the bridge (for example `vmbr0.10`). Open
vSwitch is not supported and fails closed (every VM gets `401`).

A VM that routes, NATs or proxies for others lends its identity: the identity
is the tap the connection entered, so a connection from a machine behind a
router VM (an OPNsense or pfSense VM, a reverse-proxy VM) enters on the router
VM's tap and receives the router VM's counter, silently. For networks behind
such a VM use token mode, or block TCP port 9873 (the listen port) on the
router VM for traffic from behind it. The guest agent logs the VM ID it is
served at Info (`hostpower: host serves this VM's counter`, with `vmid` and
`url`) on the first success and whenever it changes, so a guest that is served
another VM's counter shows it in its log.

Threat model. In scope and safe: a VM forging another VM's IP or MAC, a LAN
device forging a VM's IP or MAC, and replay. A layer-2 man-in-the-middle, where
a LAN device or a guest poisons the host's ARP cache or bridge forwarding
table for a VM's address and then relays that VM's traffic, can read the VM's
counter over plain HTTP (in token mode it can also copy the token); this holds
for MAC mode and token mode alike. With
[HTTPS and a pinned certificate](#https-with-a-pinned-certificate-both-modes)
that case is in scope: the relay sees only TLS 1.3 records, so it can neither
read nor alter the counter nor read a token, and without the host's private
key it cannot pose as the host. It can still drop or delay the traffic (the
guest then reports `null`). TLS does not change who is identified: a relay that
re-originates the connection enters the host on its own port, so it gets its
own counter or `401`, as without TLS. Out of scope remain a compromised host
(including theft of the key file, which needs root on the host) and a guest
without a pin. Mitigations without TLS, still useful with it: enable the PVE
firewall with MAC filter and IP filter on each VM NIC (this stops the guest
variant), and keep the host's bridge on a trusted LAN segment.

#### HTTP, token mode (routed networks, NAT, Open vSwitch, IPv6)

Manage per-VM tokens on the host, as root, outside the service (flags go
before the verb):

```
server-agent hostpower token [-tokens path] create <vmid>
server-agent hostpower token [-tokens path] revoke <vmid>
server-agent hostpower token [-tokens path] list
```

`create` creates or replaces the VM's token and prints it once; the store
(`/etc/op-hostpower/tokens.json` by default, file `0600`, directory `0700`)
keeps only its SHA-256, and `list` shows VM IDs and creation times, never
hashes. The running service picks up changes without a restart. Put the token
in the guest with `OP_AGENT_HOSTPOWER_TOKEN` (or `-hostpower-token` or
`hostpower_token`) next to `hostpower_url`; it is a secret and is never
logged. Token mode does not check the request's network origin.

Tokens follow clones and templates: a token baked into a template or a cloned
disk gives every clone the original VM's identity. Create a VM's token after
cloning, never put `hostpower_token` into a template, and revoke the token when
the VM is destroyed or its vmid is reused.

Over plain HTTP anyone who can read the traffic on the path can read the
token and the values, and copy and replay the token, so use plain HTTP only on
a trusted segment (see the threat model above). With
[HTTPS and a pinned certificate](#https-with-a-pinned-certificate-both-modes)
the token and the values travel encrypted, and a guest that is presented any
other certificate fails the handshake before the token is sent.

#### HTTPS with a pinned certificate (both modes)

Either HTTP mode can run over HTTPS. The service then presents a TLS
certificate, and the guest accepts exactly that certificate, identified by its
SHA-256 fingerprint (the pin). Identity does not change: MAC mode still
identifies the VM by the tap the TCP connection entered, token mode by the
token. TLS adds confidentiality and integrity, which closes the layer-2
man-in-the-middle case of the threat model above.

*On the host.* Add `-tls` to the listen line:

```
ExecStart=/usr/local/bin/server-agent hostpower -out /var/lib/op-hostpower -listen 192.0.2.10:9873 -tls
```

The unit needs the TLS state directory, as in the example unit above
(`StateDirectory=op-hostpower op-hostpower-tls` and
`/var/lib/op-hostpower-tls` in `ReadWritePaths`). With a unit that lacks it,
`ProtectSystem=strict` keeps the directory read-only, and the first start
fails (exit 2) with a read-only file system error and a hint to add both
lines. The service reads `-tls-cert` and `-tls-key` once at start:

- Both absent: it generates a self-signed ECDSA P-256 certificate (subject
  `CN=op-hostpower`, valid for 100 years, no host names) and writes the key,
  then the certificate, each `0600`. A missing directory is created `0700`;
  an existing one keeps its mode. If the certificate cannot be written, the key is removed again, so the next start
  generates afresh.
- Both present: it loads them as a pair (a key that does not belong to the
  certificate fails the start). They may be your own files, for example a pair
  signed by your CA, and may be symlinks to regular files. A key file readable
  by group or others is used, with a Warn.
- Exactly one present, a dangling symlink, or a path that is not a regular
  file: the service refuses to start (exit 2) and writes nothing; it never
  completes half a pair.

Neither path may lie inside the `-out` directory, which guests read over
virtiofs; the service refuses to start (exit 2) if one does. The check compares
the cleaned absolute paths and does not resolve symlinks.

The endpoint offers TLS 1.3 only, and a handshake must finish within 2 s.
Handshake errors (scanners, a guest with a wrong pin) are logged at Debug
only; other HTTP server errors, such as a failed accept, at Warn. A plain-HTTP request to the TLS port gets `400`. At
startup the service logs `hostpower: tls certificate` at Info with `cert` (the
path), `sha256` (the fingerprint) and `generated` (whether it was just
created), and its `hostpower: serving` line carries `tls=true`.

Print the fingerprint as root (flags go before the verb):

```
server-agent hostpower tls [-tls-cert path] fingerprint
```

It prints the SHA-256 of the certificate as uppercase hex with `:` between
bytes, the same value as
`openssl x509 -noout -fingerprint -sha256 -in /var/lib/op-hostpower-tls/tls.crt`
prints after its `Fingerprint=` prefix, so you can cross-check it. The verb
only reads (exit 1 when the certificate cannot be read or parsed, for example
before the first start with `-tls`; exit 2 on a usage error), never generates
a certificate, and is safe next to the running service.

*In the guest.* Point the agent at the HTTPS URL and paste the fingerprint as
printed:

```
OP_AGENT_HOSTPOWER_URL=https://192.0.2.10:9873
OP_AGENT_HOSTPOWER_TLS_FINGERPRINT=<the printed fingerprint>
```

(or `-hostpower-tls-fingerprint` or `hostpower_tls_fingerprint` in the config
file), next to `hostpower_token` in token mode. The value is 64 hex digits,
with or without `:` separators, in any case; surrounding whitespace is
ignored. It is not a secret: the resolved value, normalised to 64 lowercase
hex digits, is logged at Debug with the other settings; an invalid value is
logged as `(invalid)`, since it may be a token pasted into the wrong field.

| `hostpower_url` | `hostpower_tls_fingerprint` | Behaviour |
|---|---|---|
| `http://` | empty | Plain HTTP, as before |
| `https://` | valid | TLS 1.3; only a server certificate with this SHA-256 is accepted; its chain, host names and validity dates are not checked. Any other certificate fails the handshake before a byte of HTTP is sent, so a token never leaves the guest |
| `https://` | empty | Standard verification against the guest's system roots (not the agent's `ca_file` or `ca_pem`) and the URL's host name, for a certificate signed by a CA the guest trusts. A self-signed hostpower certificate fails every request. Startup warning |
| `http://` | set | Fail closed: every request fails without a connection, because the pin shows TLS was intended and a clear-text request would expose the token. Startup warning |
| any | invalid | Fail closed: every request fails. Startup warning |

A fingerprint set without `hostpower_url` is unused and logs a startup
warning.

*Rotation.* The guest pins the exact certificate, so every certificate change
needs the new fingerprint in every guest; until then that guest's requests fail
and `cpu_power_w` is `null`. To replace the generated certificate, delete both
files and restart the service, then copy the new fingerprint (from the startup
log or the `tls fingerprint` verb) into every guest. To use your own pair, put
both files in place (or point `-tls-cert` and `-tls-key` at them) and restart.

*Troubleshooting.* With a wrong or outdated pin the guest logs at Warn
`hostpower: server certificate does not match hostpower_tls_fingerprint`, with
`want`, `got` (the fingerprint the server presented) and `url`, once when the
mismatch begins and again only after a success in between; every failed
request is also logged at Debug as `hostpower: request failed` (see below). If
`got` is the host's current fingerprint, update the guest; if it is not,
something between guest and host presents another certificate. On the host,
failed handshakes are visible only with `-v`.

#### The HTTP endpoint (both modes)

**The endpoint.** `GET /v1/energy` answers `200` with
`{"vmid":"<vmid>","energy_uj":<n>,"max_energy_range_uj":0}` and
`Cache-Control: no-store`. A request that carries any `Authorization` header
(any scheme, even an empty one) is handled in token mode only, with no fallback
to MAC mode; a request without it is handled in MAC mode. Status codes: `401`
not identified (malformed authorization, unknown or revoked token, the mode
is disabled, capture unavailable, IPv6 source, observation overflow, no SYN
observed, handshake not observed, not a VM port, ambiguous or foreign ingress,
no VM config, MAC mismatch), `503`
identified but the VM has no live counter yet (just started, or stopped), `404`/`405` for another path or method, `429` above
100 requests per second from one source address (burst 200, checked before
identification) or above 10 requests per second for one VM (burst 20, checked
once the request is tied to a VM), so many token-mode guests behind one NAT
address are never limited by each other. Keep-alive is disabled, so every
request is its own connection. Rejections, `429`s included, are logged at Warn
at most once every 10 minutes per source address and reason (`rate limited
(source)` and `rate limited (vm)` for the two limits); served requests are
logged at Debug only; tokens and `Authorization` values are never logged. The guest client
ignores `HTTP_PROXY`/`HTTPS_PROXY` (through a proxy VM, MAC mode would
identify the proxy) and does not follow redirects. To diagnose a guest whose
`cpu_power_w` stays `null`, run its agent with `-v`: every failed request is
logged at Debug as `hostpower: request failed` with the URL and the status code
(`401`, `429`, `503`, ...) or the connection error, never the token.

**How the share is computed.** Per interval the service takes the host's
RAPL package energy delta and splits it across VMs by active CPU time (from
each VM's `qemu.slice/<vmid>.scope/cpu.stat` against `/proc/stat`), with the
idle floor included in the attribution, so the sum of all VM shares never
exceeds the package energy. Each VM gets `<out>/<vmid>/intel-rapl:0/` and
`<out>/<vmid>/windows/intel-rapl_0/` with `name`, `energy_uj` and
`max_energy_range_uj`, each replaced atomically (temp file and rename, no
fsync); the HTTP endpoint serves the same counter. `energy_uj` of the Linux
tree persists across service restarts. `max_energy_range_uj` is `0`, so the
guest reads a counter that ever goes backwards as 0 J rather than as a
wraparound.

**When it stops.** If RAPL becomes unreadable on the host while the service
runs, every VM's counter freezes and guests on any transport report a measured
0 W, not `null`; that also pulls the gateway's rolling-minimum idle baseline for
those servers down. If the service itself stops, virtiofs guests read the frozen
file and report 0 W, while HTTP guests get a refused connection, so
`cpu_power_w` goes `null` (or falls through to LibreHardwareMonitor if
configured). To notice: `systemctl status op-hostpower` on the host; for virtiofs
guests `energy_uj` under `/var/lib/op-hostpower/<vmid>/intel-rapl:0/` (or in the
guest) not changing between two reads while the VM runs; for HTTP guests a
`cpu_power_w` that turns `null` or a `curl` of `/v1/energy` that fails.

**Limitations.** A PVE VM with a virtiofs device cannot be live-migrated,
snapshotted with RAM state or hibernated, and does not support memory
hotplug. Single PVE host only: there is no cluster support, and after a
migration the HTTP transports report `null` until the guest's URL points at
the node that runs the VM (token mode also needs a new token on that node). `system_power_w` stays `null` in a guest. LXC
containers and macOS guests are out of scope.

## Privileged helper (Linux)

On bare-metal Linux two measurements need root: the RAPL energy counters (CPU
and `psys` system watts, `energy_uj` is mode 0400) and the SMBIOS table with
the DIMM details (`/sys/firmware/dmi/tables/DMI`, root-only). The agent also
starts the gateway-supplied model runtimes, so running it as root for two
numbers is the wrong trade. Instead a second, tiny process runs as root,
`server-agent helper`, and the agent runs as an ordinary user and reads both
values from the helper's Unix socket. The helper only collects: it starts no
process, opens no network socket, contacts no gateway and accepts no command;
it answers two read-only requests, and only from the configured agent user.
The same pair works on a Proxmox VE host, next to (not inside)
`op-hostpower.service`; the two root services are independent.

> **Hardware acceptance pending.** No Linux machine was available when the
> helper was built. It is covered by the Linux CI tests (a real socket with
> `SO_PEERCRED` accept and deny, directory and socket permissions) and by a run
> in a local Linux container (helper as root, clients as uid 1000, uid 2000 and
> root). The real-RAPL, real-DMI-table and systemd checks in the
> [checklist](#hardware-acceptance-checklist) below are still open, and the
> example helper unit's `CapabilityBoundingSet` and `SystemCallFilter` lines are
> **unverified** until then.

**What the agent needs.** Nothing: `helper_socket` defaults to
`/run/op-agent-helper/agent.sock` on Linux. The agent uses the helper only when
it does not run as root (a root agent reads RAPL and the DMI table itself and
never asks the helper) and only for values its own reads left empty. The
startup line's `power_sources` then lists `helper` (between `rapl` and
`hwmon`); that only shows the source is configured. The first successful reply
is logged at Info (`helper: serving RAPL counters`, with the number of zones);
failures are logged at Debug only (`helper: rapl request failed`,
`helper memory modules not available yet`; run with `-v` to see them). On an
unprivileged Linux agent **without** a helper, for example in a VM guest, set
`helper_socket` to `off` (`OP_AGENT_HELPER_SOCKET=off` or
`"helper_socket": "off"`): otherwise the agent tries the missing socket on
every sample and retries the memory modules every 30 s, at Debug level.

**1. Create the agent user** (once). The examples use `op-agent`:

```
useradd --system --user-group --no-create-home --home-dir /var/lib/op-agent --shell /usr/sbin/nologin op-agent
```

**2. Helper unit** `/etc/systemd/system/op-agent-helper.service`, with the
Linux agent binary at `/usr/local/bin/server-agent`:

```
[Unit]
Description=OnPrem AI Gateway agent helper (privileged RAPL and DIMM reads)
Before=op-server-agent.service

[Service]
ExecStart=/usr/local/bin/server-agent helper -agent-user op-agent
Restart=on-failure
RestartSec=5s
RuntimeDirectory=op-agent-helper
RuntimeDirectoryMode=0750
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateNetwork=yes
RestrictAddressFamilies=AF_UNIX
CapabilityBoundingSet=CAP_DAC_READ_SEARCH CAP_CHOWN CAP_FOWNER
SystemCallFilter=@system-service
SystemCallFilter=~@resources @mount @module @reboot @swap @raw-io @debug execve execveat
SystemCallErrorNumber=EPERM
MemoryDenyWriteExecute=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

`RuntimeDirectory=` creates the dedicated, empty directory
`/run/op-agent-helper` (its parent `/run` exists) and makes it writable under
`ProtectSystem=strict`; systemd removes it when the service stops. The point of
the hardening lines is: no exec, no network, read sysfs, manage one socket.
`@privileged` is not denied as a whole because it contains the `chown` the
helper needs, and `SystemCallErrorNumber=EPERM` makes a denied call fail
instead of killing the helper. The capability and system-call sets are
**unverified** on hardware (see above); adjust them with the checklist.

**3. Agent unit** `/etc/systemd/system/op-server-agent.service`, running the
agent as `op-agent`. Its config file holds the agent token, so keep it `0600`
and owned by `op-agent`. Relative paths in it (`ca_file`, `ca_cache_file`,
`runtime_config`, `runtime_cache`, `config_files_dir`) resolve beside it, in the
state directory the unit creates. An **empty** `runtime_cache` or
`config_files_dir`, as in the portal-generated config, does not: it defaults to
the binary's directory (`/usr/local/bin`), which `op-agent` cannot write, so
the runtime cache, the specs' config files and the RPC tensor cache would fail.
Set both to relative names in `server-agent.json`:

```
  "runtime_cache": "server-agent-runtime.cache.json",
  "config_files_dir": "server-agent-config-files",
```

The unit:

```
[Unit]
Description=OnPrem AI Gateway server agent
Wants=network-online.target op-agent-helper.service
After=network-online.target op-agent-helper.service

[Service]
User=op-agent
Group=op-agent
ExecStart=/usr/local/bin/server-agent -config /var/lib/op-agent/server-agent.json
Restart=on-failure
RestartSec=5s
StateDirectory=op-agent
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

Create the directory and the config file before the first start
(`install -d -m 0700 -o op-agent -g op-agent /var/lib/op-agent`, then copy
`server-agent.json` there with mode `0600`), then
`systemctl daemon-reload && systemctl enable --now op-agent-helper op-server-agent`.
`After=`/`Wants=` start the helper first, but the order is not what makes it
work: the agent asks the helper on every sample, so after a late start or a
restart of the helper the watts return one sample after it first answers (the
first reading is the baseline), and the memory modules arrive with the next
30 s retry.

Agent features that need more than an ordinary user, each with its usual
remedy and no code change:

- **GPU devices.** AMD's `/dev/kfd` and `/dev/dri/renderD*` (and on some
  distributions NVIDIA's device nodes) belong to the `render`/`video` groups:
  add `SupplementaryGroups=video render`.
- **Proxy mode on a port below 1024** (`cert_mode: proxy`): add
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`. Ambient capabilities are passed
  on to the managed model processes too, so a listen port of 1024 or above is
  the cleaner choice.
- **`cert_dir`** must be writable by `op-agent` (and readable by whatever
  consumes the files).
- **`cert_reload_command`** such as `systemctl reload nginx` needs a sudoers or
  polkit rule that lets `op-agent` run exactly that command.
- **Model files, runtime binaries and caches**: the managed model processes run
  as `op-agent` too, so it must be able to read the allowlisted binaries and the
  model directories and write the caches the runtimes use (its home is
  `/var/lib/op-agent` with the user above). The agent's own `runtime_cache` and
  `config_files_dir` must be writable as well: set them as in step 3, since
  their empty defaults point into the binary's directory.

**Helper options.** Usage: `server-agent helper -agent-user user
[-agent-group group] [-socket path] [-powercap-root dir] [-dmi-table file] [-v]`.

| Option | Default | Meaning |
|---|---|---|
| `-agent-user USER` | — (required) | The only uid allowed to use the socket: a user name or a decimal uid (canonical: `1000`, not `01000`). Root (uid 0) and `nobody` (uid 65534) are refused. A numeric uid without a passwd entry is accepted, but then `-agent-group` is required. |
| `-agent-group GROUP` | the agent user's primary group | Group (name or decimal gid) that owns the socket directory and the socket. It grants file access only; it is never used to authorize a connection. |
| `-socket PATH` | `/run/op-agent-helper/agent.sock` | The Unix socket; see the path rules below. |
| `-powercap-root DIR` | `/sys/class/powercap` | Where the RAPL zones are read. |
| `-dmi-table FILE` | `/sys/firmware/dmi/tables/DMI` | The raw SMBIOS table the DIMMs are parsed from. |
| `-v` | off | Debug logging (one line per served request). |

User and group names are looked up in `/etc/passwd` and `/etc/group` only (the
binary is built without CGO); for accounts from a directory service give
numeric ids. Run the helper as root. A startup error (missing or refused
`-agent-user`, an unsafe socket path, not Linux) prints a message and exits
with 2.

**Socket path rules.** The path must be absolute and clean and must not sit
directly in `/`. The parent of the socket directory must already exist (the
helper creates only the socket directory itself, mode `0750`); it is resolved
through symlinks once, and every directory from there up to `/` must be owned
by root and not writable by group or others, unless it carries the sticky bit
(a sticky world-writable directory such as `/tmp` is accepted, because others
cannot rename or remove root's entries in it). The socket directory must be a
real directory, not a symlink, owned by root and **dedicated**: it may contain
nothing but the socket (so the helper never re-owns a shared directory such as
`/run`; `RuntimeDirectory=` starts it empty). The helper then sets it to `0750`
`root:<agent group>` and the socket to `0660` `root:<agent group>`. A leftover
socket from a crash is removed; a socket another process still listens on is
never removed (`another helper is already listening on ...`), and any other
kind of file at the path is an error. On shutdown (SIGINT/SIGTERM) the socket
is removed.

**Who can connect.** Every connection is checked with `SO_PEERCRED` before a
byte is read: only the configured agent uid is served. Everyone else, root
included, is closed without a response, and the helper logs
`helper: connection rejected` (with `peer_uid`, `peer_pid` and the reason) at
Warn at most once every 10 minutes per peer uid. The model runtimes the agent
starts run under the same uid, so they can read the helper too; that is
accepted, because they are inside the agent's trust boundary anyway (they could
signal or trace the agent). What they can learn is limited by the 250 ms cache
below: at most four distinct RAPL values per second, a coarse average package
power, far coarser than the counter's update rate (about 50 µs) that the
PLATYPUS power side channel (CVE-2020-8694) relies on. Do not add other users
to the agent's group: it would let them reach the socket, though `SO_PEERCRED`
still turns them away.

**Endpoints** (HTTP over the socket, `GET` only, JSON, `Cache-Control:
no-store`; another path is `404`, another method `405`, more than 20 requests
per second (burst 40) across the whole socket is `429`; timeouts and header
limit as for the hostpower endpoint):

- `GET /v1/rapl` →
  `{"read_ns":…,"domains":[{"zone":"intel-rapl:0","name":"package-0","energy_uj":…,"max_energy_range_uj":…}, …]}`:
  every top-level zone `intel-rapl:N` (sub-zones such as `intel-rapl:0:0` are
  left out) with a name and a readable `energy_uj`, sorted by zone. The reading
  is cached and refreshed at most every 250 ms, however fast clients ask.
  `read_ns` is the system-wide `CLOCK_MONOTONIC` time in nanoseconds at which
  the counters were read; a cached response repeats it. The agent divides the
  energy delta by the difference of two `read_ns` values, so a cached reading
  is never misread as 0 W: it yields no value and the next fresh one covers the
  whole span. At the 250 ms interval floor about every other helper sample
  therefore carries no value, and that metric falls through to the next source
  (hwmon, hostpower, LHM) or stays empty for that sample; at the 1 s default
  every sample is fresh. Against a helper
  without `read_ns` the agent times the delta with its own clock. An empty
  list is valid (no RAPL); the helper warns at startup
  (`helper: no readable RAPL zone`) and keeps running.
- `GET /v1/memory-modules` → `{"modules":[{"locator":…,"size_bytes":…,"type":…,"speed_mhz":…}, …]}`,
  the inventory's DIMM shape. The table is parsed once at startup; an unreadable
  table logs `helper: DMI table not readable; serving no memory modules` at Warn
  and serves an empty list. Serial numbers, asset tags, part numbers and UUIDs
  are never decoded.

To query it by hand (as `op-agent`):
`curl --unix-socket /run/op-agent-helper/agent.sock http://op-agent-helper/v1/rapl`.

### Hardware acceptance checklist

Status: **pending** (no Linux machine was available). On a bare-metal Linux
server with Intel or AMD RAPL:

1. Install both units as above and start them. `journalctl -u op-agent-helper`
   shows `helper: serving` with the socket, `agent_uid` and `agent_gid`, and
   neither `helper: no readable RAPL zone` nor
   `helper: DMI table not readable; serving no memory modules`.
2. `ls -ld /run/op-agent-helper /run/op-agent-helper/agent.sock` shows
   `drwxr-x--- root op-agent` and `srw-rw---- root op-agent`.
3. `sudo -u op-agent curl --unix-socket /run/op-agent-helper/agent.sock http://op-agent-helper/v1/rapl`
   lists the same zones as `grep . /sys/class/powercap/intel-rapl:*/name`
   (top level only), and `energy_uj` rises between two calls more than 250 ms
   apart.
4. The same `curl` as root gets no HTTP response (curl reports an empty reply
   or a send/receive failure), and the helper logs
   `helper: connection rejected` with `peer_uid` 0; as another ordinary user
   it cannot connect at all (permission denied on the socket directory).
5. The agent's startup line lists `helper` in `power_sources`, the agent logs
   `helper: serving RAPL counters`, and from the second sample the portal shows
   CPU watts (and system watts where a `psys` zone exists). Under steady load
   they match what the same agent reports when run as root.
6. The portal's **Hardware** view lists the DIMMs. Compare them with
   `dmidecode -t memory` on the same machine: locator, size, type and speed
   (the rated speed, else the configured speed) must agree for every populated
   slot. Run the agent once as root and confirm the same list (its in-process
   parse of the table).
7. Late start: stop the helper, restart the agent, start the helper. Within
   30 s the agent logs `memory modules received from the helper` and the
   **Hardware** view fills in; the watts return from the second sample after
   the helper is up.
8. `systemd-analyze security op-agent-helper.service`, and the journal for
   denied system calls or capability errors over a day of operation; then
   tighten or correct `CapabilityBoundingSet` and `SystemCallFilter` (for
   example whether `CAP_DAC_READ_SEARCH` and `CAP_FOWNER` are needed at all)
   and record the verified sets here.

## Resilience

Each collection is best-effort: a failing GPU collector, host collector, or
scrape is logged and skipped so a partial sample still ships, and a send failure
never kills the loop. Under **POST** the agent retries up to 3 times with
exponential backoff on transport errors and `5xx` responses (a `4xx` is not
retried). Under **WebSocket** a send/connection failure triggers a reconnect with
exponential backoff + jitter (see [Transport](#transport-post-vs-websocket)); the
collection loop is never blocked waiting on a reconnect. The token is never
included in any log line or error.
