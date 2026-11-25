# VM CPU power share: host-attributed RAPL for guests (design)

Date: 2026-11-25. Status: draft, awaiting approval. Branch-local working
document (removed before the pull request; durable content folds into
`docs/architecture/`).

## 1. Intent and why

The server agent reports a nullable `cpu_power_w` per telemetry sample. On
bare-metal Linux it derives that from the RAPL powercap energy counters
(`/sys/class/powercap/intel-rapl:*/energy_uj`), and the gateway's energy
engine uses it (with GPU watts) as the Tier-1 "measured" power base for
per-request energy attribution (`serverPowerW` in
`gateway/backend/internal/gateway/energy_engine.go`).

Inside a KVM guest (Proxmox VE host, Linux guest) there is no RAPL: the MSRs
are not passed through, `/sys/class/powercap` is empty, hwmon has no power
sensors. Such a server therefore never gets a measured CPU-power base and
falls back to the operator-estimated Tier 2. A GPU passed through to the
guest is still measured directly (nvidia-smi inside the guest), so the
missing piece is precisely the CPU package share.

Goal: a guest-resident agent reports, as `cpu_power_w`, the share of the
host's CPU package power that this VM consumed, measured on the host and
attributed by CPU time, with no change to the wire format, gateway, energy
engine or portal.

## 2. Goals, non-goals, anti-goals

Goals

- A Linux guest on PVE 9.2 (virtiofs available natively) reports a
  host-attributed CPU package watt figure through the existing
  `cpu_power_w` field.
- The host-side piece is tiny, standalone, and needs no gateway contact: no
  token, no NetBird, no network listener.
- The sum of all VM shares on one host never exceeds the host's measured
  package power for the same interval.
- Reuse: the guest side reuses the existing RAPL collector unchanged except
  for a configurable powercap root; the host side reuses the agent binary
  and its RAPL reading code.

Non-goals (this version)

- Total system watts (`system_power_w`) for guests. The host's psys/hwmon
  or IPMI reading is not shared into guests. Stays `null` in the guest.
- A Proxmox host appearing in the gateway as a server of its own (the
  operator confirmed no agent runs on the host, so no double counting
  exists and no host/guest relationship is modelled).
- Windows guests. The design does not preclude them (the RAPL collector is
  build-tag-free, and virtiofs exists for Windows via WinFsp), but it is not
  built or documented now.
- Containers (LXC). Only QEMU VMs under `qemu.slice`.
- A provenance marker in the portal that this value is an attributed share
  rather than a direct measurement (operator decision: not needed).

Anti-goals (would make this a failure even if it works)

- The host service becoming a second agent: anything that needs the host to
  know about the gateway, be enrolled, hold credentials, or expose a port.
- A guest being able to read or influence another guest's share.
- Changing the meaning of `cpu_power_w` for bare-metal servers, or any
  gateway/portal code change.
- Operator-visible host names or VM names in repository text; use VM IDs
  and generic names in docs.

## 3. Constraints

- `energy_uj` on the host is root-only (PLATYPUS mitigation); the host
  service runs as root under systemd on the PVE host, which is standard
  there.
- PVE 9.x runs cgroup v2; every VM's QEMU process lives in
  `/sys/fs/cgroup/qemu.slice/<vmid>.scope/` with a `cpu.stat` carrying
  `usage_usec`.
- PVE 9.2 virtiofs: a VM with a virtiofs device loses live migration, RAM
  snapshots, hibernate and memory hotplug (PVE documentation, "Known
  Limitations"); accepted by the operator. A Datacenter directory mapping
  (`/cluster/mapping/dir`) per shared directory, attached with
  `qm set <vmid> --virtiofs0 dirid=<mapping>`; the `virtiofsd` package must
  be installed on the host. The guest mounts with
  `mount -t virtiofs <mapping> <mountpoint>`.
- The guest-side agent's RAPL collector already takes `powercapRoot` and
  `hwmonRoot` as constructor parameters (`newRAPLCollector` in
  `server-agent/internal/collector/rapl.go`); only the Linux wiring in
  `power_linux.go` hard-codes `/sys/class/powercap`.
- The agent binary has an early-argument mode pattern (`-license` in
  `server-agent/main.go`); a host mode follows the same shape.
- Agent `Version` (`server-agent/internal/agent/agent.go`) is bumped once per
  shipped change; this is a feature, so a minor bump (currently 0.20.1).
- AGENTS.md rules: feature branch + PR, spec and plan under
  `docs/superpowers/` removed before the PR, durable facts into
  `docs/architecture/`, English repo text.

## 4. Design

### 4.1 Shape

```
PVE host (root)                                   Linux guest
+-----------------------------------+             +-----------------------------+
| server-agent hostpower            |             | server-agent (normal mode)  |
|  reads /sys/class/powercap RAPL   |  virtiofs   |  OP_AGENT_POWERCAP_ROOT=    |
|  reads /proc/stat, cgroup cpu.stat|  per VM     |   /run/op-hostpower         |
|  writes per VM:                   | ==========> |  raplCollector reads        |
|   <out>/<vmid>/intel-rapl:0/      |             |   intel-rapl:0/energy_uj    |
|     name, energy_uj,              |             |  -> cpu_power_w as today    |
|     max_energy_range_uj           |             +-----------------------------+
+-----------------------------------+
```

The host fakes, per VM, exactly the sysfs tree the guest's RAPL collector
already understands. Everything downstream of the collector is untouched.

### 4.2 Host mode: `server-agent hostpower`

Invocation (early-argument mode, like `-license`):

```
server-agent hostpower [-out /var/lib/op-hostpower] [-interval 250ms] [-v]
```

Loop, every `interval`:

1. Read the host package energy exactly as the existing RAPL collector does
   (sum of `package-*` domains, wraparound-corrected). Δ E_pkg in joules for
   this tick. Reuse the existing reading code; do not duplicate it.
2. Read host busy CPU time from `/proc/stat` line `cpu`:
   busy = user + nice + system + irq + softirq + steal (guest time is already
   inside user/nice on the host and must not be added again). Δ busy_host.
3. For every directory `/sys/fs/cgroup/qemu.slice/<vmid>.scope`, read
   `cpu.stat` `usage_usec`. Δ busy_vm.
4. share_vm = Δ busy_vm / Δ busy_host, clamped to [0, 1]; if Δ busy_host is
   0 (or the sum of VM deltas exceeds the host delta due to sampling skew),
   normalise so the shares sum to at most 1.
5. E_vm += Δ E_pkg × share_vm, accumulated in a per-VM monotonic counter in
   microjoules (uint64).
6. Write `<out>/<vmid>/intel-rapl:0/{name,energy_uj,max_energy_range_uj}`:
   `name` = `package-0`, `energy_uj` = the counter, `max_energy_range_uj` =
   a fixed very large ceiling (2^63 − 1) so the guest never sees a wrap.
   Write via temp file + rename in the same directory so the guest never
   reads a half-written value.

Cadence (my call, see §6): the host writes much more often than the guest
samples. The guest agent collects every 1 s by default and reports
Δenergy/Δt between its own reads; the gateway's Tier-1 path needs gapless
samples at most 2 s apart with a positive power reading inside the request
window, and its idle tracker is a rolling minimum. If the host updated the
counter only every 1 s, phase jitter between the two loops would make the
guest alternate between 0 W and 2x readings, sink the idle tracker to 0 W
and push short requests to Tier 2. With a 250 ms host tick a 1 s guest
delta spans about four host updates, so the lumpiness stays within ±25 %.
`-interval` has a floor of 100 ms; the README states that it must be at
most a quarter of the guest's `OP_AGENT_INTERVAL`. The sysfs/cgroup reads
involved cost microseconds, so 4 Hz is negligible on the host.

Attribution policy (my call, see §6): all package power, including its idle
floor, is divided by active CPU time. A VM with no CPU time in a tick gets
0 J for that tick; a VM that is the only busy consumer gets the whole
package for that tick. This mirrors what a bare-metal server reports (the
full package) and keeps the gateway's idle-baseline tracker meaningful
(rolling minimum per server). Host-OS and other-VM time simply stay
unattributed.

State and lifecycle

- The `energy_uj` file is the persistent state. On start, the service reads
  any existing counter per VM directory and continues from it, so a service
  restart does not reset counters and the guest sees no spurious delta.
- VM directories are discovered on every tick from `qemu.slice`. A new VM
  gets a directory on first sight. A stopped VM's directory and counter
  remain frozen (its guest is down anyway); the operator may delete the
  directory.
- The first tick after start produces no delta (no previous reading), same
  as the guest collector's own first sample.
- Not root, no RAPL domains, or no `qemu.slice`: log once at startup and keep
  retrying each tick (the host may load `intel_rapl_common` later); never
  write bogus values.

Packaging

- systemd unit example in the agent README (`op-hostpower.service`:
  `ExecStart=/usr/local/bin/server-agent hostpower -out /var/lib/op-hostpower`,
  `Restart=always`). The operator downloads the Linux agent binary from the
  gateway's existing agent-binaries page.
- Per VM, the operator creates one directory mapping pointing at
  `<out>/<vmid>` and attaches it to that VM only. The service must create
  `<out>/<vmid>` before the mapping can be attached; the first tick does.

### 4.3 Guest side: configurable powercap root

New agent option, all three forms like `lhm_url`:

| env | flag | config file | default |
|-----|------|-------------|---------|
| `OP_AGENT_POWERCAP_ROOT` | `-powercap-root` | `powercap_root` | `/sys/class/powercap` |

`power_linux.go` passes the configured root instead of the constant. The
hwmon root stays default (a VM has no hwmon power sensors; harmless). The
RAPL collector itself is unchanged.

Operator steps in the guest: mount the virtiofs share (fstab line
`<mapping> /run/op-hostpower virtiofs ro,nofail 0 0` or a systemd mount
unit), set the option, restart the agent. The existing startup diagnostic
that lists power sources shows `rapl` as before.

On non-Linux builds the option is accepted and ignored (documented), so a
future Windows-guest path can pick it up without a config change.

### 4.4 Security and privacy

- The host service writes only inside `-out`, reads only sysfs/procfs.
- Each VM sees only its own directory; cross-VM isolation comes from the
  per-VM mapping, not from the service.
- A guest that scribbles into its own share corrupts only its own reading;
  mounting read-only in the guest is recommended but not required.
- No host names, VM names or IPs leave the host; VM IDs appear only in local
  paths.

### 4.5 Documentation (durable, goes to `docs/architecture/`)

- `server-agent/README.md` power section: new "Linux guest on Proxmox"
  subsection (host service, mappings, mount, option).
- `docs/architecture/reference/config-env.md`: the new option.
- `docs/architecture/09-architecture-decisions.md`: amend ADR-013 or add an
  ADR "VM CPU power via host-attributed synthetic RAPL" recording the
  attribution policy and the no-gateway-contact decision.
- `docs/architecture/07-deployment-view.md`: the host service as an optional
  component on a hypervisor.

### 4.6 Testing

- Host mode: pure-function core (share computation, normalisation, counter
  accumulation, persistence read-back) unit-tested against synthetic
  powercap/proc/cgroup roots passed as parameters, in the same style as
  `rapl_test.go`. Atomic write verified (no partial reads).
- Guest side: existing RAPL tests already cover a synthetic root; add one
  config test for the new option's resolution order.
- Manual acceptance on the operator's PVE 9.2 host: one guest shows a
  `cpu_power_w` chart in the portal; with two guests, the sum of their
  readings stays at or below the host's package watts read by hand.

## 5. Left to the builder

- Exact file layout of the host-mode code (a new `internal/hostpower`
  package is the obvious choice) and how it shares the RAPL reading helper
  with the collector package without an import cycle.
- Logging detail and `-v` output.
- How the README phrases the Proxmox UI steps versus the `pvesh`/`qm`
  commands.

## 6. Decisions made by the designer (not the operator)

- Transport: synthetic RAPL tree over virtiofs, not HTTP (would need guest
  identification and auth) and not vsock (new code on both sides, `args:`-only
  on PVE).
- Attribution by active CPU time with the idle floor included, no vCPU-based
  idle split in v1.
- Host piece is a mode of the agent binary, not a separate tool or script.
- Guest option named `OP_AGENT_POWERCAP_ROOT`; the collector is otherwise
  untouched.
- Counter persisted in the `energy_uj` file itself; no separate state file.
- Host write cadence 250 ms (floor 100 ms), at most a quarter of the guest
  sampling interval, so the guest's per-second delta averages several host
  updates; no smoothing in the guest collector.
- Minor agent version bump.

Operator-confirmed facts: PVE 9.2.32, virtiofs not yet in use, no agent on
the host, no gateway contact wanted for the host service, no portal
provenance marker needed. The operator accepts the PVE virtiofs
limitations for the affected guests (per the PVE 9.2 documentation: live
migration, snapshots with RAM state and hibernate are not available with a
virtiofs device, and memory hotplug does not work). The HTTP-exporter
variant (host listener on the bridge, guest identified by its SMBIOS UUID)
is therefore not pursued and is recorded only as the fallback should that
change.
