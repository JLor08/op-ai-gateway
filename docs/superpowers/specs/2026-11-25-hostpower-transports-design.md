# Hostpower transports: virtiofs for Linux and Windows, HTTP in MAC and token mode (design)

Date: 2026-11-25. Status: draft, awaiting approval. Branch-local working
document (removed before the pull request; durable content folds into
`docs/architecture/` and `server-agent/README.md`).

Builds on the merged VM CPU power share (PR #436, ADR-115): the
`server-agent hostpower` mode on a Proxmox VE host attributes RAPL package
energy to QEMU VMs by cgroup CPU time every 250 ms and publishes, per VM, a
synthetic powercap tree `<out>/<vmid>/intel-rapl:0/{name,energy_uj,
max_energy_range_uj}` that a Linux guest mounts over virtiofs and reads via
`OP_AGENT_POWERCAP_ROOT`.

## 1. Intent and why

Today only a Linux guest with a virtiofs mount can receive its share. The
operator wants to choose the transport per VM, for Linux and Windows guests
alike:

| Transport | Linux guest | Windows guest | Guest setup |
|---|---|---|---|
| virtiofs | exists (PR #436) | new | mount the share; set the powercap root |
| HTTP, MAC mode | new | new | set the host URL only |
| HTTP, token mode | new | new | set the host URL and a per-VM token |

Why each exists:

- **virtiofs** needs no network path between guest and host and keeps the
  guest agent reading a plain powercap tree. On Windows it needs the
  virtio-win guest tools and WinFsp, and the VM loses live migration, RAM
  snapshots, hibernate and memory hotplug.
- **HTTP, MAC mode** is the "almost automatic" path: no driver, no mount, no
  secret. The host identifies the asking VM from the network itself: the
  requester's MAC address, the bridge port it was learned on (whose name
  carries the VM ID), and the MAC configured for that VM. It works whenever
  the VM and the host's listen address share a Linux bridge, which is the
  operator's setup (VMs on `vmbr0` in the same LAN as the host).
- **HTTP, token mode** covers everything MAC mode cannot (routed networks,
  NAT, Open vSwitch, SDN overlays, IPv6) at the cost of one secret per VM.

Success: on the operator's PVE 9.2 host, a Linux and a Windows guest each
show a CPU-watt chart in the portal through each of the three transports,
and no machine other than the VM itself can obtain that VM's values.

## 2. Goals, non-goals, anti-goals

Goals

- Windows guests receive their share over virtiofs.
- Linux and Windows guests receive their share over HTTP in MAC mode and in
  token mode.
- The operator picks the transport per VM; all three run side by side on one
  host from one `hostpower` service.
- The same attribution, cadence and counter semantics as today (ADR-115):
  the guest computes watts from a cumulative microjoule counter, first sample
  null, a counter decrease reads as 0 J.
- Over HTTP a guest always receives exactly its own VM's counter.

Non-goals (this version)

- TLS for the HTTP endpoint (my call, see §7; the token mode is designed so
  TLS can be added without changing the guest's identity model).
- MAC mode over IPv6, routed networks, NAT, Open vSwitch or SDN overlays
  (token mode covers them).
- LXC containers, macOS guests (the guest collector builds everywhere but is
  documented for Linux and Windows only).
- A local privileged helper for bare-metal Linux (the root part serving RAPL
  and `dmidecode` data over a Unix socket to an unprivileged agent). Agreed
  as a separate follow-up branch on top of this one. This branch must not
  preclude it (see §4.3 and §4.6 "Follow-up seam").
- PVE clusters and live migration. The operator runs a single PVE host.
  Token store, counter and guest URL are per host; after a migration to
  another node the HTTP transports report null until the guest's URL points
  at a node that runs it (documented limitation in the README and the ADR).
- Total system watts for guests (stays null, as today).
- Any gateway or portal change beyond the downloadable agent-config
  templates, which must list every agent setting (repo guard).

Anti-goals (a failure even if it works)

- The host service becoming a second agent: no enrollment, no gateway
  contact, no gateway token. The existing archtest forbidden-edge rule for
  `internal/hostpower` stays.
- Any machine obtaining another VM's values: another VM, a LAN device, or a
  process on the host that is not root. This also covers the virtiofs trees:
  the state directory becomes `0700` (`StateDirectoryMode=0700` in the
  README unit; the service creates `<out>` with `0700` when it creates it
  itself). Guests are unaffected because virtiofsd runs as root and each
  share starts at `<vmid>`; verified on PVE 9.2 during acceptance.
  (Designer's call after the builder check.)
- HTTP enabled by default: it listens only when the operator sets an
  address.
- Tokens readable at rest on the host or written to any log.
- Double counting on a guest that sees more than one tree or transport.

## 3. Constraints

- Go 1.26, stdlib only, CGO-free (as the whole agent).
- PVE 9.2: VM NICs attach to a Linux bridge as `tap<vmid>i<n>`, or with the
  PVE firewall enabled on that NIC as `fwpr<vmid>p<n>` on the bridge (the tap
  then sits on an intermediate `fwbr<vmid>i<n>`). VM configs live in
  `/etc/pve/qemu-server/<vmid>.conf` (the node-local view of pmxcfs); a NIC
  line reads `net<n>: <model>=<MAC>,bridge=<bridge>[,firewall=1][,...]`.
- The host service runs as root under the hardened systemd unit from
  PR #436 (`ProtectSystem=strict`, `StateDirectory=op-hostpower`,
  `NoNewPrivileges`, `PrivateTmp`). `/proc`, `/sys` and `/etc/pve` stay
  readable under `ProtectSystem=strict`.
- Guest agent: `collectTimeout` is 2 s per collector call; the default
  sampling interval is 1 s.
- virtio-win virtiofs on Windows (WinFsp-based VirtIO-FS service): no
  documented file-name mapping; `:` is not a legal Windows file-name
  character (alternate-data-stream syntax). How a host name containing `:` is
  presented is unverified, and listing a directory that contains one might
  fail. The Windows tree must therefore contain no `:` anywhere.
- The Windows agent commonly runs as a service under LocalSystem.
- Repo rules (AGENTS.md): feature branch + PR, English repo text, no LAN
  host names in docs, durable docs in `docs/architecture/`, one MINOR agent
  version bump (currently `0.22.0`), a new ADR (next free number on main,
  currently ADR-116).

## 4. Design

### 4.1 Shape

```
PVE host: server-agent hostpower (root, systemd)
  tick (250 ms, unchanged): RAPL + /proc/stat + cgroup -> per-VM counter
  ├─ writes <out>/<vmid>/intel-rapl:0/...          (Linux virtiofs, unchanged)
  ├─ writes <out>/<vmid>/windows/intel-rapl_0/...  (Windows virtiofs, new)
  └─ serves GET /v1/energy on -listen              (HTTP, new; off by default)
         auth: Bearer token  -> token mode
               no header     -> MAC mode (if enabled)

Guest agent (Linux or Windows):
  power collectors, first non-nil wins:
    native  (RAPL; reads powercap_root on Linux AND now on Windows)
    hostpower HTTP (new; when hostpower_url is set)
    LHM     (unchanged)
```

The counter in the service's memory is the single source for every
transport. `energy_uj` in `intel-rapl:0` stays the persisted state that seeds
it on restart (unchanged).

### 4.2 Windows virtiofs tree

- Every tick the host additionally writes
  `<out>/<vmid>/windows/intel-rapl_0/{name,energy_uj,max_energy_range_uj}`
  with exactly the content of the Linux tree (`package-0`, the counter, `0`).
  It uses the same per-VM `os.Root` confinement, small-regular-file reads and
  temp+rename writes as `tree.go` today; a write failure there is handled and
  logged like a Linux-tree write failure (one VM's failure never stops other
  VMs; state-change logging).
- The Windows directory mapping points at `<vmid>/windows` (through the
  read-only bind mount, so `/run/op-hostpower-ro/<vmid>/windows`). The
  Windows share therefore contains no `:` name at all.
- The persisted counter is read only from the Linux tree; the Windows copy
  is write-only.
- `RemoveStaleTemps` also sweeps the Windows domain directory.
- Guest side: on Windows, `newNativePowerCollector(powercapRoot)` returns the
  RAPL collector when `powercapRoot` is non-empty (nil otherwise, as today).
  The RAPL collector gains a domain-name pattern chosen per platform:
  `intel-rapl:*` on Linux (unchanged behaviour), `intel-rapl_*` on Windows.
  A guest never globs both patterns, so it cannot double count. hwmon stays
  Linux-only.
- Windows guest setup (README): virtio-win guest tools (VirtIO-FS service)
  and WinFsp; VirtIO-FS service configured with the mapping tag and a drive
  letter; `powercap_root` set to that drive root, e.g. `Z:\`; ACLs not
  exposed (virtio-win guidance); `cache=never` on the host mapping as for
  Linux.

### 4.3 HTTP endpoint on the host

Flags (all optional; HTTP is off unless `-listen` is set):

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | empty (off) | `host:port` to serve on, e.g. the `vmbr0` address |
| `-auth` | `mac,token` | enabled modes, comma list of `mac` and `token` |
| `-tokens` | `/etc/op-hostpower/tokens.json` | token store (token mode) |

Endpoint: `GET /v1/energy`. Success, `200`, `Cache-Control: no-store`:

```json
{"vmid":"100","energy_uj":123456789,"max_energy_range_uj":0}
```

Rules:

- `Authorization: Bearer <token>` present → token mode only. An unknown,
  revoked or malformed token, or token mode disabled → `401`. No fallback to
  MAC mode, so a wrong token is never silently "fixed" by the network path.
- No `Authorization` header → MAC mode if enabled; unidentified → `401`.
- Identified VM without a counter yet (first ticks after start, or not a
  running QEMU VM) → `503`.
- Any other method or path → `405`/`404`. Request bodies are ignored.
- Server limits: `ReadHeaderTimeout` and `WriteTimeout` of a few seconds,
  small `MaxHeaderBytes`, a per-source-IP rate limit (generous for a 1 Hz
  poller, e.g. 10 requests/s burst 20) returning `429`.
- Logging: no token, no `Authorization` header value is ever logged.
  Rejections are logged per source address on state change only (as the
  write-failure logging), at Warn, with the reason (`no ARP entry`,
  `not a VM bridge port`, `MAC mismatch`, `unknown token`, ...).
- The handler reads the counter from the service under its existing mutex
  (a snapshot accessor; no map escapes the lock).

Follow-up seam (host): the server is started on a `net.Listener` the caller
creates, and the handler obtains the caller's identity through one internal
step (token, else MAC) that a later Unix-socket peer-credential identity can
join without restructuring the handler.

### 4.4 MAC mode: identifying the VM from the network

For a request from remote address `ip` (IPv4 only; an IPv4-mapped IPv6
address counts as IPv4; any other IPv6 → `401` in MAC mode):

1. **MAC:** look up `ip` in `/proc/net/arp`; require a complete entry
   (flags `0x2`) whose device is a Linux bridge. Result: `mac`, `bridge`.
   The entry is fresh because the host has just answered the guest's TCP
   handshake.
2. **Bridge port:** find the forwarding-database entry for `mac` on
   `bridge` and the port interface it was learned on (sysfs
   `/sys/class/net/<bridge>/brforward` plus `brif/<port>/port_no`, or an
   equivalent stdlib-only source the builder verifies on PVE 9.2).
   Local (host-owned) entries never match.
3. **VM ID:** the port name must match `^tap(\d+)i(\d+)$` or
   `^fwpr(\d+)p(\d+)$` → `vmid`, NIC index `n`. Anything else (physical
   uplink, `veth…` of LXC, bonds) → `401`.
4. **Config check:** `/etc/pve/qemu-server/<vmid>.conf` must contain
   `net<n>:` whose MAC equals `mac` (case-insensitive) and whose `bridge=`
   equals `bridge`. Mismatch → `401`.
5. The VM must be a running QEMU VM (present in the service's VM set).

Why this is safe (to be restated in the ADR): a VM that fakes another VM's
IP and poisons the host's ARP cache still has its frames learned on its own
port, so steps 2–4 resolve to itself and it gets its own values; a VM that
fakes another VM's MAC is learned on its own port, whose configured MAC
differs → `401`; a LAN device is learned on the physical uplink → `401`;
nothing secret travels, so nothing can be sniffed and replayed. The PVE
firewall's MAC filter, if enabled, blocks spoofed source MACs earlier still.

All paths (`/proc/net/arp`, `/sys/class/net`, `/etc/pve/qemu-server`) are
`Options` fields with these defaults so tests run against synthetic trees.
A short cache of a successful resolution (a few seconds) is allowed only if
it is keyed on the full tuple (IP, MAC, bridge port) re-read from ARP and the
FDB on every request, so a VM that takes over a cached IP never inherits
another VM's identity; caching only skips re-reading the VM config. Any
lookup failure is not cached. (Designer's call after the builder check.)

### 4.5 Token mode

- Token format: `ophp_` + 32 random bytes, base64url without padding.
- Store: `/etc/op-hostpower/tokens.json`, mode `0600`, root-owned,
  `{"<vmid>": {"sha256": "<hex>", "created": "<RFC 3339>"}}`. Only the
  SHA-256 of a token is stored; the plaintext is printed once at creation.
- Lookup: hash the presented token and compare against every stored hash in
  constant time; at most one VM may match.
- CLI (operator, as root, outside the service; same binary):
  - `server-agent hostpower token create <vmid> [-tokens PATH]`: creates or
    replaces that VM's token and prints it once.
  - `server-agent hostpower token revoke <vmid> [-tokens PATH]`.
  - `server-agent hostpower token list [-tokens PATH]`: VM IDs and creation
    times, never hashes.
  - Writes are atomic (temp + rename in the same directory, `0600`).
- The service reloads the store when its modification time changes (checked
  at most once per second), so create/revoke take effect without a restart.
  A store that is missing is "no tokens"; one that fails to parse keeps the
  last good set and logs once.
- A token maps to a VM ID; the request's network origin is not checked in
  token mode (that is the point of the mode).

### 4.6 Guest agent: HTTP collector

New agent options, all three forms like the existing ones:

| env | flag | config file | default |
|---|---|---|---|
| `OP_AGENT_HOSTPOWER_URL` | `-hostpower-url` | `hostpower_url` | empty (off) |
| `OP_AGENT_HOSTPOWER_TOKEN` | `-hostpower-token` | `hostpower_token` | empty (MAC mode) |

- New build-tag-free sub-collector (name `hostpower`) in the collector
  package, composed after the native collector and before LHM, on every OS.
- The HTTP client uses its own `http.Transport` with `Proxy: nil`, so
  `HTTP_PROXY`/`HTTPS_PROXY` are never honoured: through a proxy VM on the
  bridge, MAC mode would identify the proxy VM and silently hand its counter
  to the guest. (Designer's call after the builder check.)
- Each `Collect`: `GET <hostpower_url>/v1/energy` with a 1 s client timeout,
  `Authorization: Bearer <token>` when a token is set. Watts are computed
  exactly like RAPL: Δ`energy_uj`/Δt with the same wrap/decrease rule
  (reuse `raplDeltaJoules`; `max_energy_range_uj` 0 → a decrease is 0 J).
  First successful response → nil (no delta yet).
- Any error, non-200, unparsable body, or a changed `vmid` between responses
  → nil and drop the previous reading (re-baseline next time).
- Follow-up seam (guest): the request URL is built in one place from
  `hostpower_url`, and the HTTP client is constructed in one function, so a
  later `unix://` scheme (custom dialer) is a local change.
- The token is a secret: never logged; the startup diagnostic logs only
  `hostpower_url` and whether a token is set.
- The gateway's downloadable agent-config templates (Go + TSX) and the shared
  golden gain `hostpower_url` and `hostpower_token` with empty values and a
  comment, like `powercap_root`.
- One JSON response type, defined once; `internal/hostpower` may import it
  from `internal/collector` (the allowed edge), so host and guest cannot
  drift.

### 4.7 Choosing a transport (README guidance)

- Linux guest, VM on the host's bridge: MAC mode (only the URL).
- Windows guest, VM on the host's bridge: MAC mode (only the URL).
- VM not on the host's bridge, or IPv6 only: token mode.
- No network path to the host wanted, migration not needed: virtiofs.
- A guest should use exactly one transport. If several are configured, the
  collector order (native/virtiofs first, then HTTP) decides, and only one
  value is reported, so nothing is double counted.

### 4.8 Documentation (durable)

- `server-agent/README.md`: restructure the Proxmox section into a common
  host part and three guest transports (Linux/Windows virtiofs, HTTP MAC
  mode, HTTP token mode), with the systemd unit gaining `-listen`, the token
  CLI, Windows VirtIO-FS/WinFsp steps, a decision table, and the new agent
  options in the config table.
- New ADR (ADR-116 or the next free number): transports and the MAC-mode
  identity argument; TLS deferred; Windows tree without `:`.
- `telemetry-usage-observability.md`, `configuration.md`, `config-env.md`,
  deployment view §7.5: the HTTP listener, the new options.
- Agent `Version` MINOR bump.

### 4.9 Testing

- MAC resolution against synthetic `/proc/net/arp`, bridge sysfs (FDB and
  port numbers) and `qemu-server` configs: tap and fwpr ports, uplink port,
  `veth` port, MAC mismatch, wrong NIC index, wrong bridge, incomplete ARP
  entry, IPv6.
- Handler tests with `httptest` and an injected remote address: token
  precedence, `401`/`503`/`405`/`429`, `no-store`, no token in logs
  (recording slog handler).
- Token store: create/replace/revoke/list, `0600`, atomic write, reload on
  mtime change, corrupt store keeps the last good set, constant-time match.
- Windows tree: written each tick, confined, no `:` in any written name,
  stale temps swept.
- RAPL collector pattern: Linux pattern unchanged; Windows pattern reads
  `intel-rapl_*`; a tree with both names counts once on each platform.
- Guest HTTP collector against `httptest`: first sample nil, watts, error
  re-baseline, vmid change, decrease with range 0 → 0 W, token header.
- Manual acceptance on PVE 9.2 (PR checklist): MAC mode from a Linux and a
  Windows guest; a second VM faking the first VM's IP gets only its own
  values; a LAN machine gets `401`; token mode from a routed network; Windows
  virtiofs drive visible to the agent service account (LocalSystem) and
  readings not alternating with `cache=never`.

## 5. Left to the builder

- Package/file layout for the HTTP server, MAC resolver and token store
  inside `internal/hostpower` (one responsibility per file).
- The exact stdlib-only way to read the bridge FDB, verified on PVE 9.2.
- Rate-limit implementation details and exact timeout values within the
  stated bounds.
- How the token CLI subcommand is dispatched from `Main`.
- README phrasing and the order of the transport sections.

## 6. Decisions confirmed by the operator

- Both transports, selectable, for Linux and Windows guests.
- HTTP in two variants: MAC mode (almost automatic) and token mode.
- The VMs sit on `vmbr0` in the same LAN as the host.
- A single PVE host, no cluster, no live migration.
- "Reasonable" setup effort is acceptable; a guest must get its own values
  and no one else may get them.

## 7. Decisions made by the designer (not the operator)

- HTTP off by default; enabled by `-listen`.
- Token wins over MAC when a token is sent; no fallback on a bad token.
- MAC mode IPv4-only; identity from ARP + bridge FDB port name + VM config.
- Tokens stored hashed; CLI on the same binary; hot reload by mtime.
- No TLS in this version.
- Windows virtiofs via a separate `windows/` subtree with `intel-rapl_0`,
  mapped on its own, rather than an alias next to the Linux tree.
- Guest collector order: native/virtiofs, then HTTP, then LHM.
- State directory `0700`; MAC-resolution cache keyed on (IP, MAC, port)
  only; guest HTTP client ignores proxy environment variables.
