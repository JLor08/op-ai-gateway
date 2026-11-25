# Privileged helper for the server agent on bare-metal Linux — design

Status: draft for review (2026-11-25). Branch-local; folded into
`docs/architecture/` and deleted before the pull request.

## 1. Intent

On bare-metal Linux the server agent needs root for two measurements:

- **RAPL energy counters** (`/sys/class/powercap/intel-rapl:*/energy_uj`,
  mode `0400` since the PLATYPUS / CVE-2020-8694 mitigation). Without them the
  agent reports no CPU watts and no `psys` system watts.
- **Per-DIMM detail** (today via `dmidecode -t memory`, which reads the
  root-only SMBIOS table). Without it the hardware inventory shows RAM total only.

The same agent process launches gateway-supplied model runtimes. Running it as
root to get two numbers is the wrong trade. The operator wants a privilege split
modelled on the Proxmox `hostpower` host/guest split: a tiny root process that
only **collects**, starts nothing, talks to nothing but a Unix socket — and the
normal agent running **unprivileged**, reading those values from the socket.

## 2. Goals, non-goals, anti-goals

**Goals**

- A new root mode `server-agent helper` serving, over a Unix socket, the raw
  top-level RAPL domain counters and the per-DIMM memory inventory.
- Only the configured agent identity may use the socket: file permissions plus
  an `SO_PEERCRED` check on every connection.
- The unprivileged agent picks up the helper automatically when its own native
  read fails for lack of permission, with zero configuration in the default
  layout, and shows CPU watts (and `psys` system watts where present) and DIMM
  detail exactly as a root agent would.
- Works on a plain Linux AI server (primary case) and on a Proxmox/QEMU host
  (alongside, not inside, the existing `op-hostpower` unit).

**Non-goals**

- macOS (`powermetrics` needs root too) — unchanged, out of scope.
- Windows — unchanged.
- Any change to `server-agent hostpower`, its wire format or its units.
- Any gateway, portal or wire-protocol change. The agent reports the same
  `sample` fields it reports today as root.
- Serving anything else (temperatures, hwmon, MSRs). Extensible later; not now.

**Anti-goals (a failure even if it "works")**

- The root helper executes any process (including `dmidecode`), opens any
  network socket, reads config from the gateway or accepts any write/command
  from the agent.
- Any local user other than the configured agent identity can read values from
  the socket.
- The agent silently loses data it had before: a root agent must keep working
  exactly as today (native reads win).
- Serial numbers, UUIDs or asset tags from the SMBIOS table ever leave the helper.

## 3. Decisions taken with the operator

1. **Two separate root units.** On a Proxmox host `op-hostpower.service`
   (guests) and `op-agent-helper.service` (local agent) run side by side. The
   helper is its own mode and package; `hostpower` stays untouched.
2. **Names.** Mode `server-agent helper`; unit `op-agent-helper.service`;
   socket `/run/op-agent-helper/agent.sock`; agent option `helper_socket` /
   `OP_AGENT_HELPER_SOCKET` / `-helper-socket`.
3. **A separate agent option, not `hostpower_url=unix://…`.** The helper serves
   different data (undivided host counters of all top-level domains plus the
   DIMM inventory, not a VM's share), so it gets its own key. This deliberately
   departs from the initial idea of extending `hostpower_url`.
4. **HTTP over the Unix socket.** Same request/response style and server
   hardening as the hostpower HTTP endpoint, so it stays testable with
   `net/http` on both sides.
5. **The helper parses the SMBIOS table itself** from
   `/sys/firmware/dmi/tables/DMI` (root-only) instead of running `dmidecode`,
   so the root process never spawns a child and its unit can forbid `exec`.
6. **Linux only.** macOS stays as it is.

## 4. Design

### 4.1 Helper process (`server-agent helper`, package `internal/helper`)

Command line:

```
server-agent helper [-socket PATH] -agent-user USER|UID [-agent-group GROUP|GID]
                    [-powercap-root DIR] [-dmi-table FILE] [-v]
```

| Flag | Default | Meaning |
|---|---|---|
| `-socket` | `/run/op-agent-helper/agent.sock` | Unix socket path |
| `-agent-user` | — (required) | The only uid allowed to connect; name or numeric |
| `-agent-group` | the agent user's primary group | Group owning the socket directory and socket |
| `-powercap-root` | `/sys/class/powercap` | RAPL source (tests point it at a synthetic tree) |
| `-dmi-table` | `/sys/firmware/dmi/tables/DMI` | SMBIOS table (tests point it at a fixture) |
| `-v` | off | Debug logging |

Behaviour:

- **Refuses to start** when not on Linux, when `-agent-user` is missing or
  resolves to uid 0, or when the socket path's parent cannot be secured (below).
  Name resolution uses `os/user` (pure Go, `/etc/passwd`/`/etc/group`).
- **Socket setup.** The parent directory is created if missing, must be a real
  directory (not a symlink), owned by root, and is set to `0750 root:<agent-group>`.
  A stale entry at the socket path is removed only if it is a socket; anything
  else is an error. The socket is created, then `chown root:<agent-group>` and
  `chmod 0660`. (Under systemd `RuntimeDirectory=` creates the directory; the
  helper still verifies and tightens it.)
- **Peer authorization.** Every accepted connection is checked with
  `SO_PEERCRED` before any byte is read: the peer uid must equal the agent uid
  (root peers are **not** implicitly allowed). A denied connection is closed
  without a response and logged at Warn, rate-limited (at most once per 10 min
  per peer uid). The decision is made in exactly one place (an `identifyPeer`
  step mirroring hostpower's `identify`); the accepted uid is attached to the
  request context.
- **Endpoints** (GET only; anything else 404/405; JSON;
  `Cache-Control: no-store`; bodies are small and capped by the client):
  - `GET /v1/rapl` → `{"domains":[{"name":"package-0","energy_uj":…,"max_energy_range_uj":…}, …]}`:
    every **top-level** `intel-rapl:N` directory (no `intel-rapl:N:M`
    subdomains) whose `energy_uj` is readable. **Served from a cache refreshed
    at most every 250 ms** (decided with the operator, see §4.4); a request
    after the cache has aged out triggers one fresh read.
    Domain identity is by `name` plus the directory index (`"zone":"intel-rapl:0"`)
    so the agent can track counters per domain across requests. An empty list
    is valid (no RAPL).
  - `GET /v1/memory-modules` → `{"modules":[{"locator":…,"size_bytes":…,"type":…,"speed_mhz":…}, …]}`
    using the existing `sample.MemoryModule` JSON shape. Parsed **once at
    startup** and cached (the table is static); a parse/read failure yields an
    empty list plus a startup Warn, not an exit.
- **Rate limit.** A single token bucket for the whole socket (e.g. 20 req/s,
  burst 40) — there is exactly one legitimate client. Over the limit → 429.
  It only bounds the helper's work; it is not the side-channel defence (§4.4).
- **Server hardening.** Same timeouts and header limit as `hostpower.NewServer`.
  The helper may reuse that constructor or carry its own equivalent; either way
  `internal/helper` gets an archtest rule like hostpower's: it must not import
  agent, client, config, proxy, runtime or trust (allowed: collector, sample,
  and hostpower only if the server constructor is reused).
- **Shutdown** on SIGINT/SIGTERM: stop serving, remove the socket file.

### 4.2 SMBIOS parser

- Reads the raw table file (`/sys/firmware/dmi/tables/DMI`, the structure table
  without the entry point; kernel ≥ 4.2).
- Walks structures (header: type, length, handle; formatted area; string set
  terminated by a double NUL) and decodes **type 17 (Memory Device)** only:
  - Size: word at 0x0C (`0` = empty slot → skipped, `0xFFFF` = unknown →
    skipped, bit 15 = KB granularity, `0x7FFF` → Extended Size dword at 0x1C,
    in MB).
  - Locator: string index at 0x10.
  - Memory Type: byte at 0x12, mapped to the SMBIOS names dmidecode prints
    (`DDR4`, `DDR5`, `LPDDR5`, `Other`, …); only `Unknown` → empty, exactly as
    `parseDmidecodeMemory` does today (`Other` is passed through).
  - Speed: the **rated** Speed (0x15; `0xFFFF` → Extended Speed dword at 0x54)
    if non-zero, else Configured Memory Speed (0x20; `0xFFFF` → 0x58). This is
    what a root agent reports today: dmidecode prints "Speed" first and
    `parseDmidecodeMemory` keeps the first non-zero value. MT/s reported as
    `speed_mhz`. *(Builder-check correction; my call.)*
- Bounds-checked everywhere: a truncated structure or string set ends parsing
  with whatever was decoded; never panics on malformed input (fuzz test).
- **Never decodes** serial number, asset tag, part number or any UUID.
- Lives in `internal/collector` (pure function over bytes), called by the
  helper **and by a root agent**: the euid-0 path reads the DMI table file
  in-process with the same parser instead of running `dmidecode`, so root and
  unprivileged agents cannot drift and the agent no longer needs `dmidecode`
  installed. `parseDmidecodeMemory` and the `dmidecode` exec are removed.
  The hardware-acceptance checklist compares the parser with `dmidecode` on a
  real machine. *(My call.)*

### 4.3 Agent side

- **Config.** `helper_socket` (file) / `OP_AGENT_HELPER_SOCKET` / `-helper-socket`.
  Default on Linux: `/run/op-agent-helper/agent.sock`; elsewhere empty. The
  value `off` (any source) disables the helper; an empty env var means unset,
  as everywhere else in the agent's config resolution. *(My call: the config
  resolver cannot tell an empty env var from an unset one.)* Logged at startup like the other
  power options. A value that is not an absolute path logs a startup warning.
  A configured path on a non-Linux OS logs a warning and is ignored.
- **Client.** One constructor builds the `http.Client` for the helper: the
  hostpower client's settings (no proxy, no redirects, short timeout) with a
  transport whose `DialContext` always dials the Unix socket path regardless of
  the URL host. The request URL is a fixed `http://op-agent-helper` + path, built
  in one place. Responses are size-capped per endpoint: 4 KiB for `/v1/rapl`,
  64 KiB for `/v1/memory-modules` (a fully populated 2-socket board with 48
  DIMMs is several KiB).
  (Implementation may extend `NewHostpowerHTTPClient` with a dialer option or
  add a sibling constructor sharing its setup.)
- **Power.** A new sub-collector `helper` (name shown in `power_sources`).
  To keep "exactly as a root agent" true for system watts, the native Linux
  collector is split into its RAPL part and its hwmon part, and the composite
  order on Linux becomes **native RAPL → helper → hwmon → hostpower HTTP → LHM**
  (first non-nil per metric). A root agent's result is unchanged (RAPL psys
  wins, hwmon fills only when there is no psys); an unprivileged agent gets the
  helper's psys before the world-readable hwmon sensor. *(My call.)*
  The helper sub-collector is **not added at all when the agent runs as
  euid 0** (it reads natively and the helper would deny root anyway). It fetches
  `/v1/rapl` and applies the exact RAPL semantics of `raplCollector`: sum the
  `package*` domains for CPU watts, `psys` for system watts, per-zone delta with
  wraparound correction, nil on the first sample and after any failure. Native
  RAPL stays first, so a root agent (or a udev-granted one) is unchanged and the
  helper only fills metrics native left nil. `Available()` is true when the
  socket path is set (it does not prove connectivity, same as rapl).
- **Memory modules.** `linuxMemoryModules`: euid 0 → in-process DMI parse
  (above); otherwise, if `helper_socket` is set, `GET /v1/memory-modules`
  within `hwCollectTimeout`; failure → nil (RAM total only, as today).
  Plumbing the socket path into the hardware collector is the builder's call.
- **Helper not ready at agent start.** The inventory is collected once at
  start (`collectHardware` in `agent.Run`). When the helper is configured and
  the memory-module fetch failed, the agent retries just that fetch every 30 s
  (the system-report cadence defaults to 30 min, too slow) until the first success, then updates the cached
  report and re-sends it. The units add `After=`/`Wants=` ordering, but the
  retry is what makes a helper restart or late start harmless. *(My call.)*
- **Logging.** First success from the helper at Info (once), failures at Debug,
  so an unprivileged agent without a helper does not spam logs. A denied
  connection shows on the helper side (Warn), on the agent side as a closed
  connection at Debug.

### 4.4 Threat model: processes sharing the agent's uid

The agent starts gateway-supplied runtimes as child processes under its own
uid (`internal/runtime` sets no credential), so `SO_PEERCRED` cannot tell the
agent from a runtime: **any process of the agent uid can read the helper.**
Decided with the operator:

- **Accepted:** same-uid processes are inside the agent's trust boundary
  anyway (they can signal or ptrace the agent). No per-pid check
  (`/proc/<pid>/exe` and the like) is built; it would be a false boundary.
- **Side channel (PLATYPUS / CVE-2020-8694):** the attack needs RAPL samples at
  the counter's update rate (~50 µs). The 250 ms cache caps every reader at
  4 distinct values per second regardless of request rate (the same
  granularity Proxmox guests get from hostpower's 250 ms tick), so a runtime
  learns only coarse average package power — roughly what the portal's watt
  chart shows. Residual risk recorded in the ADR and the risk table; the full
  fix (runtimes under their own uid) is out of scope.
- **Out of scope:** a same-uid process flooding the socket so the agent gets
  429s and loses watts for a while (denial of service by a process that could
  kill the agent outright).

### 4.5 Deployment (README examples; the repo ships no unit files today)

`/etc/systemd/system/op-agent-helper.service`, roughly:

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
MemoryDenyWriteExecute=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

Exact capability and syscall sets are verified on hardware and adjusted (the
point is: no exec, no network, read sysfs + manage one socket; `@privileged`
is not denied wholesale because it contains the `chown` the helper needs).
The README also gets an example unprivileged agent unit (`User=op-agent`,
`After=`/`Wants=op-agent-helper.service`) to show the pair, with a short caveat
list for agent features that need more than an ordinary user, each with its
usual remedy and no code change: GPU device access (`SupplementaryGroups=video
render`), proxy mode on a port below 1024 (`AmbientCapabilities=CAP_NET_BIND_SERVICE`),
`cert_dir` ownership, and a `cert_reload_command` such as `systemctl reload nginx`
(a sudoers or polkit rule). *(My call.)*

## 5. Tests (must exist)

- Peer credentials: a real Unix socket in a temp dir (Linux): the configured uid
  (the test's own uid) is accepted; a configured different uid makes the same
  connection be denied (closed, no body). Logic split so the decision function
  is unit-tested on any OS; the `SO_PEERCRED` glue is Linux-tagged.
- Socket permissions: directory `0750`, socket `0660`, stale-socket removal,
  refusal on a symlinked directory or a non-socket at the path.
- Startup refusals: missing `-agent-user`, uid 0, non-Linux.
- Endpoints: `/v1/rapl` over a synthetic powercap tree (top-level only,
  unreadable domain skipped), `/v1/memory-modules`, 404/405/429.
- SMBIOS parser: tables built by an in-test encoder with real-world values
  (no Linux box is available: DDR4 server module, DDR5 desktop module, an
  extended-size module, an empty slot, a short 2.6-era structure); a real table
  from the container run is added when it can be captured, malformed and
  truncated input, a fuzz test; no serial/asset fields in the output type.
- Agent: unix:// client against an `httptest`-style server on a Unix socket;
  helper power sub-collector deltas, wraparound, failure reset, ordering in the
  composite (native non-nil wins; helper fills nil); memory-modules fallback;
  config parsing/defaults/warnings.
- Archtest rule for `internal/helper`.

## 6. Docs

- `server-agent/README.md`: power section (Linux: helper instead of "run as
  root"), hardware-inventory note, config table row, new "Privileged helper"
  section with the unit files.
- New ADR (next number, ADR-117) in `docs/architecture/09-architecture-decisions.md`.
- `docs/architecture/reference/config-env.md`, `cross-cutting/configuration.md`,
  `07-deployment-view.md`, `cross-cutting/architecture-tests.md`,
  `11-risks-and-technical-debt.md` (the "privileged telemetry sources absent" row),
  telemetry doc where it describes Linux power/DIMM sources.

## 7. Done when

- **No Linux box is available now** (operator, 2026-11-25). Substitute
  evidence in this change: the Linux CI runs the real-socket tests
  (`SO_PEERCRED` accept/deny, permissions), and a local Linux-container run
  (only with the operator's OK to start Docker) starts the helper as root
  against a synthetic powercap tree and a captured DMI table and an agent
  binary as a non-root uid, showing `power_sources` with `helper`, CPU watts
  from the synthetic counters, the DIMM list, and a foreign uid denied.
- The hardware acceptance (real RAPL, real DMI table, the systemd unit under
  `systemd-analyze security`) is written as a step-by-step checklist in the
  README and recorded as **pending** in the ADR and the risk table, as the repo
  does for other hardware-pending items. The example unit's capability and
  syscall sets are marked unverified until then.
- Tests above exist and pass with `-race`; `make fmt`, `make lint-go`,
  `make lint-docs` and the local Sonar gate are green; frontend gates only if
  templates change (not expected).

## 8. Left to the builder

- Whether `internal/helper` reuses `hostpower.NewServer` or carries its own.
- How the socket path reaches the hardware collector.
- Exact rate-limit numbers and log wording.
- The memory-type name table's extent (cover what SMBIOS 3.7 defines).
