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

Every setting can be given three ways. Precedence, highest first:
**command-line flag > environment variable > config file > built-in default.**
`OP_AGENT_GATEWAY_URL` and `OP_AGENT_TOKEN` are required (from any source).

| Env                     | Flag             | Config key     | Default | Description                                                        |
| ----------------------- | ---------------- | -------------- | ------- | ------------------------------------------------------------------ |
| `OP_AGENT_GATEWAY_URL`  | `-gateway-url`   | `gateway_url`  | —       | Gateway base URL (absolute `http`/`https`), e.g. `https://gw.example`. **Required.** |
| `OP_AGENT_TOKEN`        | `-token`         | `token`        | —       | Per-server agent bearer token. **Required.**                       |
| `OP_AGENT_TRANSPORT`    | `-transport`     | `transport`    | `websocket` | Telemetry transport: `websocket` (one persistent connection) or `post` (one HTTP POST per sample). See [Transport](#transport-post-vs-websocket). |
| `OP_AGENT_INTERVAL`     | `-interval`      | `interval`     | `1s`    | Collection cadence as a Go duration (e.g. `500ms`, `5s`). Clamped up to a **250ms** floor; a non-positive value falls back to `1s`. |
| `OP_AGENT_SYSTEM_REPORT_INTERVAL` | `-system-report-interval` | `system_report_interval` | `30m` | POST-mode re-send cadence for the static hardware inventory (self-heals a gateway restart). Floored at `1m`. Ignored under `websocket` (which re-sends on each reconnect). |
| `OP_AGENT_METRICS_URL`  | `-metrics-url`   | `metrics_url`  | —       | Optional inference `/metrics` (Prometheus text) URL to scrape for active/queued requests. |
| `OP_AGENT_TLS_INSECURE` | `-tls-insecure`  | `tls_insecure` | `false` | Skip TLS certificate verification (self-signed dev gateways). `true`/`1`. |
| `OP_AGENT_LHM_URL`      | `-lhm-url`       | `lhm_url`      | —       | Optional LibreHardwareMonitor Remote Web Server `/data.json` URL for CPU (and best-effort system) power watts, e.g. `http://127.0.0.1:8085/data.json`. Empty disables it. The only Windows CPU-watt path; a Linux fallback when RAPL is unreadable. |
| `OP_AGENT_CERT_MODE`    | `-cert-mode`     | `cert_mode`    | `off`   | Certificate install mode: `off` (never fetch), `files` (write cert files + run the reload command), or `proxy` (install like `files` **and** run a TLS-terminating reverse proxy in front of the application). See [Certificate installation](#certificate-installation). |
| `OP_AGENT_CERT_DIR`     | `-cert-dir`      | `cert_dir`     | —       | Directory certificate files are written into. **Required** when `cert_mode` is not `off`. |
| `OP_AGENT_CERT_RELOAD_COMMAND` | `-cert-reload-command` | `cert_reload_command` | — | Local shell command run after a changed certificate is fully installed. Comes **only** from this local setting — the gateway can never deliver a command to run. |
| `OP_AGENT_CERT_POLL_INTERVAL` | `-cert-poll-interval` | `cert_poll_interval` | automatic | How often the agent checks the gateway for a new certificate, as a Go duration (e.g. `15m`). Empty or `0`/`0s` means automatic (`websocket` transport polls every 6h, `post` every 15m). A configured positive value is floored at **1 minute** (a faster poll against a key-serving endpoint is a self-inflicted DoS). |
| `OP_AGENT_CA_FILE` | `-ca-file` | `ca_file` | — | Optional operator-managed public CA bundle. Read-only: the agent never overwrites it. |
| `OP_AGENT_CA_CACHE_FILE` | `-ca-cache-file` | `ca_cache_file` | — | Optional agent-managed public CA cache. Generated self-signed mesh configs use `server-agent-ca.pem`. |
| `OP_AGENT_CA_PEM` | `-ca-pem` | `ca_pem` | — | Optional inline public CA bootstrap bundle, generated only when the currently served gateway leaf uses the internal CA. |
| `OP_AGENT_RUNTIME_SOURCE` | `-runtime-source` | `runtime_source` | `gateway` | Where the agent-managed model runtime's launch specs come from: `gateway` (fetched from `GET /api/agent/v1/runtime-config`, portal-maintained) or `file` (read from `runtime_config` locally and reported upward read-only). See [Managed model runtime](#managed-model-runtime). |
| `OP_AGENT_RUNTIME_CONFIG` | `-runtime-config` | `runtime_config` | — | Path to the local runtime-config JSON file. **Required** when `runtime_source` is `file`; ignored otherwise. |
| `OP_AGENT_RUNTIME_ALLOWED_BINARIES` | — (file/env only) | `runtime_allowed_binaries` | — (empty) | Absolute paths a launch spec's `binary` must match **exactly** to be permitted. **Empty means nothing may start at all** — a deliberate hard refusal, not a permissive default. This is the operator's boundary: the gateway decides *when and how* a model process runs, this list decides *whether it may run at all*. Env value is comma-separated. |
| `OP_AGENT_RUNTIME_ALLOWED_DIRS` | — (file/env only) | `runtime_allowed_dirs` | — (empty) | Permitted `work_dir` prefixes for launch specs. Unlike the binary allowlist, **empty means any `work_dir`** — an operator who does not care is not forced to enumerate one. Containment is a lexical, path-boundary check; symlinks are not resolved (see `withinDir` in `internal/runtime/policy_local.go` for the reasoning). Env value is comma-separated. |
| `OP_AGENT_RUNTIME_CACHE` | `-runtime-cache` | `runtime_cache` | `server-agent-runtime.cache.json` next to the binary | Where the last known-good runtime-config document is cached, so the agent can start (and keep) model processes before its first successful gateway contact. A relative config-file value is resolved beside that config file. |
| `OP_AGENT_RUNTIME_ROUTER_BIND` | `-runtime-router-bind` | `runtime_router_bind` | — (derive) | Bind host for the managed runtime's router port — the port the gateway sends inference requests to. Operator-only: the gateway supplies the router **port**, never its bind host. Empty means derive: the agent's own mesh identity, read from the **installed mesh leaf in `cert_dir`** — so the derivation only yields an address when `cert_mode` is not `off` **and** a certificate has actually been installed. Otherwise **all interfaces**, with a warning in the agent log. Since the portal's generated config ships `cert_mode: "off"` and `cert_dir: ""`, the default configuration always lands on all interfaces: set this explicitly (mesh IP, or `127.0.0.1`) on any host that is not mesh-only. |
| `OP_AGENT_VERBOSE`      | `-v` / `-verbose`| `verbose`      | `false` | Verbose mode: emit detailed **debug** logs to stderr — resolved config (token never logged), each collect cycle, and every telemetry POST with URL, HTTP status, duration, and retry/backoff. Use this to diagnose why the agent can't reach the gateway. |

### Config file

A JSON config file is an alternative to env vars / flags. By default the agent
looks for **`server-agent.json` next to the binary**; override the path with
`-config <path>` or `OP_AGENT_CONFIG`. All keys are optional; an env var or flag of
the same setting overrides the file. A missing default file is fine; an explicitly
requested file that is missing or malformed is a startup error.

Relative `ca_file`, `ca_cache_file`, `runtime_config` and `runtime_cache`
values are resolved beside the actually selected config file (after
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

`runtime_allowed_dirs` additionally restricts a spec's `work_dir`; it is
convenience/defence-in-depth, not a boundary (containment is a lexical
path-prefix check and does not resolve symlinks).

#### What the router port serves

Apart from three `GET` control paths — `/health` (and `/v1/health`), `/running`
(llama-swap shape, so the gateway's existing loaded-model detection works
unchanged) and `/v1/models` — **the router routes exclusively on a `model`
field in a JSON request body.** Everything else is a proxied inference request,
and to be proxied it must carry a body that parses as JSON and names a model
this agent manages. Consequences worth knowing before pointing an application
at it:

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
env **value** redacted (keys stay visible) and the portal renders it read-only,
including hiding start/stop — whoever owns the config owns the operations.

> **Put secrets in `env`, never in `args`.** The upward report masks `env`
> values only. `args` are **not** masked, and they are reported verbatim —
> even though `${AGENT_ENV:NAME}` placeholders are expanded in `args` exactly
> as they are in `env`, so a resolved secret in an argument reaches the
> gateway in plaintext and is shown to portal admins. This is the wire
> contract, not a bug, and writing a local `runtime.json` is the only way to
> create the hazard, which is why it is stated here:
>
> ```jsonc
> // NO -- reaches the gateway unmasked, in the report and in stderr
> "args": ["--api-key", "hf_realsecret"],
> "args": ["--api-key", "${AGENT_ENV:HF_TOKEN}"],
>
> // YES -- the value is masked in the report
> "env":  { "HF_TOKEN": "${AGENT_ENV:HF_TOKEN}" },
> ```
>
> The report is not the only upward channel either: on a non-zero exit the
> tail of the child's own stderr travels up with the telemetry and is shown on
> the runtime screen, and model servers routinely print their full command
> line at startup. That is a second reason an argument is the wrong place for
> a secret, and it applies to portal-authored specs too.

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
counts (`vllm:num_requests_running` → active, `vllm:num_requests_waiting` →
queue depth). Any other exposition is ignored; a scrape failure is logged and
skipped without stopping the loop.

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

- **Per-DIMM detail** is free on Windows and Intel macOS; on **Linux it needs root**
  (else RAM total only); Apple Silicon uses unified memory (total only).
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

- **Linux (native RAPL).** CPU watts come from the powercap RAPL sysfs energy
  counters (`/sys/class/powercap/intel-rapl:*/energy_uj`); system watts from a RAPL
  `psys` domain if present, else a hwmon power sensor
  (`/sys/class/hwmon/*/power*_input`). `energy_uj` is root-only (mode 0400) since the
  PLATYPUS/CVE-2020-8694 mitigation, so **run the agent as root** or apply a
  udev/`setcap` rule granting the agent's user read access to `energy_uj`. If unmet →
  CPU watts `null` (no chart). `psys`/hwmon power sensors are often absent on rack
  servers → system watts `null`. RAPL is an energy counter, so the **first sample
  after start is `null`** (no delta yet).

- **macOS (native powermetrics).** CPU watts come from
  `powermetrics --samplers cpu_power`, which **requires root/sudo**. Run the agent as
  root or configure passwordless sudo for `powermetrics`. If unmet → CPU watts
  `null`. Total system watts is never available on macOS CGO-free → always `null`.

- **Windows.** There is no native CGO-free path (RAPL is ring-0/MSR). CPU watts are
  available **only** via the optional LibreHardwareMonitor source below; system watts
  are `null`.

- **LibreHardwareMonitor source (any OS, opt-in).** Set `OP_AGENT_LHM_URL` to the
  operator's LibreHardwareMonitor Remote Web Server `/data.json`
  (e.g. `http://127.0.0.1:8085/data.json`). Install **LibreHardwareMonitor ≥ 0.9.6**,
  run it elevated (its PawnIO driver must be allowed by HVCI/Memory-Integrity/
  Smart-App-Control/AV), and enable **Options → Remote Web Server**. If basic-auth is
  enabled, embed credentials in the URL. The agent ships/links nothing from LHM — the
  operator installs it. Unset/unreachable → power `null` (no chart). Per metric, the
  native reading wins and LHM fills gaps (native first).

## Resilience

Each collection is best-effort: a failing GPU collector, host collector, or
scrape is logged and skipped so a partial sample still ships, and a send failure
never kills the loop. Under **POST** the agent retries up to 3 times with
exponential backoff on transport errors and `5xx` responses (a `4xx` is not
retried). Under **WebSocket** a send/connection failure triggers a reconnect with
exponential backoff + jitter (see [Transport](#transport-post-vs-websocket)); the
collection loop is never blocked waiting on a reconnect. The token is never
included in any log line or error.
