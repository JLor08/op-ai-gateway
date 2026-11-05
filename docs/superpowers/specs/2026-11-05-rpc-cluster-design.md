# llama.cpp RPC cluster mode — design (issue #365, Stage 1)

**Status:** owner decisions settled and spec approved by the owner, 2026-11-05.
**Base:** `main` @ `2ad3696`; llama.cpp pinned at `2bc46ba` (`2bc46baa9207d098d03741ac394e7faaf1a955b6`).
**Branch-local working document.** Everything durable is folded into `docs/architecture/` (ADR-096 and the
chapters listed in §13) before the PR, and this file is deleted.

Issue #365 (body and its first comment) is the researched, adversarially verified proposal. This spec
adopts it as the baseline and records **every change** the owner decided on top of it. Where this spec
and the issue differ, this spec wins. Section numbers §1–§10 match the issue's *Proposed design*
sections so the two can be read side by side; the issue's evidence (file:line on `main`, upstream links)
is not repeated here unless a decision depends on it.

## 0. Intent

Run one model across several AI servers: a `llama_cpp` runspec, the **head**, runs `llama-server --rpc …`
and offloads part of the model to `ggml-rpc-server` processes, the **members**, on other AI servers. The
gateway configures, orders, holds, accounts and secures the cluster; it never sits on the RPC data path.
Performance comes first (owner): the transport is configurable per cluster, and a cluster in a plain LAN
without NetBird must work in direct mode.

Goals (Stage 1 = v1): R1, R2 through agent-managed `rpc_server` runspecs, R3–R10 (requirements table in the
issue). Non-goals: the gateway on the data path; authenticating or encrypting llama.cpp's RPC protocol; a
gateway-side model of each host's free room (ADR-092 stands); Stage 2 application members; everything in
the issue's *Later* list.

Anti-goals (a technically working result that is still a failure):
- a member that any other head (or any LAN host, beyond what the operator explicitly confirmed) can drive;
- a member evicted or idled out while its head uses it (R10), or a head that restarts because a member
  merely flapped in telemetry;
- an old agent that executes a literal `${RPC_SERVERS}` or runs a member as a model spec;
- RPC device names that silently point at the wrong member device (shifted `RPC<n>`).

## 1. Decisions (owner, 2026-11-05)

| # | Decision |
|---|---|
| Q1 | **Changed from the issue: NetBird is not required.** Each cluster has a **network**: `mesh` (NetBird on head and every member, as the issue designs it) or `lan` (no NetBird; head and members share a local network). Direct mode works in both (§4a). |
| Q2 | Application members only on agent hosts, either transport (Stage 2, not built now). |
| Q3 | Default transport: in `mesh`, `direct` where its NetBird preconditions hold, else `relay`; in `lan`, `direct`. Recorded in ADR-096. |
| Q4 | A cold request parks while the cluster forms, bounded by the head's plus the slowest member's startup timeout, within the request's `timeout_ms`. |
| Q5 | Heads need an explicit startup timeout, required at save. |
| Q6 | The managed policy family covers every `mesh` cluster (like `op-gw-agent-ingest`), no per-server include/exclude. |
| Q7 | `mesh` + relay with deny-by-default off: warn, do not block (direct is blocked either way). |
| Q8 | Member energy = a share of the member host's power. |
| Q9 | Member cost uses each member server's own `price_per_kwh`. |
| Q10 | Member shares appear under the member server, aggregated (Security). |
| Q11 | Formation energy is charged to the requests that waited. |
| Q12 | A local start on a member host blocked only by a held member: the gateway may remove the cluster (head first) when the head may be removed; otherwise the start waits. |
| Q13 | No per-request VRAM/RAM metering in v1 (decided by the owner; member usage measured per run, stored with the head). |
| Q14 | RDMA direct links: later. |
| Q15 | A member serves one head; no sharing. |
| — | **Member order is operator-defined**, edited like GPU rows. It is the `--rpc` order and therefore the `RPC<n>` numbering and the `--tensor-split` positions; inside a member, its GPU row order (§3a). |
| — | **Cluster heads may use their own GPUs in either mode** (owner note, overrides issue §2's refusal of `--device`): env mode via `set_visible_devices`, or args mode via device placeholders that also name the RPC devices (§3a). |
| — | **Generic device placeholders** `${DEVICES}` and `${FIRST_LOCAL_DEVICE}` with a per-spec **device backend** (`auto` or explicit), plus the explicit `${RPC_DEVICES}` and `${CUDA_FIRST_DEVICE}`/`${VULKAN_FIRST_DEVICE}`/`${METAL_FIRST_DEVICE}` (§3a). |
| — | **Open follow-up, documented:** the backend's device enumeration order (above all Vulkan, and mixed-vendor hosts) is independent of host GPU indices; revisit later (e.g. matching `--list-devices` output). |
| — | **Two member modes per head** (owner): `manual` — the operator creates an `rpc_server` runspec on a member server and the head selects only the server (it must have such a spec); `automatic` — the head selects server + GPUs (ordered) + member settings, and the gateway derives and manages one `rpc_server` instance per (head, server) itself (§1b). |
| — | **Several members per server** in both modes: each member is its own `ggml-rpc-server` process with its own `-d` GPU set and port, serving one head. GPU availability is decided by the member host's admission at start (free, or freed by eviction; Q12 when a held member of another cluster blocks), never by a save-time exclusivity rule; overlapping GPU selections only warn. |
| — | **LAN address per server** (owner, after the builder check): an optional IPv4 field plus a "listen only on this address" switch; used in `lan` clusters as the dial target and the head's identity, required there for NetBird-enabled servers (§4a). |
| — | **Binary inventory per server** (owner): the agent reports its allowlisted binaries with kind and version; a new "Binaries" tab shows and edits them; runspecs pick binaries from it, and automatic members get the `ggml-rpc-server` whose version matches the head (§1c). |
| — | Stage 1 lands as **one PR**. Host-dependent Stage 0 measurements (direct vs relay, mesh P2P, firewalls, observer cost on Windows, LAN reachability of a mesh bind, NVIDIA/Vulkan members) are coordinated with the owner and only feed docs and portal hints; Stage 1 does not wait for them. |

## 2. Verified upstream facts the decisions rest on (llama.cpp @ `2bc46ba`)

Source reading; the local Stage 0 run (§12) confirms them on a real build.
- `--device`/`-dev`/`LLAMA_ARG_DEVICE` resolve device names **while parsing** (`parse_device_list`,
  `common/arg.cpp:1106-1126`) and refuse CPU-type devices. `RPC<n>` names come into existence in the `--rpc`
  handler (`add_rpc_devices`, `common/arg.cpp:1158-1176`, option at `:2630`).
- Parse order (`common_params_parse_ex`, `common/arg.cpp:752-868`): first **all environment variables, in
  option definition order** (`--rpc` is defined before `--device`), then argv **left to right**. Hence:
  argv `--rpc … --device …` works; env `LLAMA_ARG_RPC` + env `LLAMA_ARG_DEVICE` works; env `LLAMA_ARG_RPC` +
  argv `--device` works; env `LLAMA_ARG_DEVICE` + argv `--rpc` fails (`invalid device: RPC0`); `--rpc` in env
  **and** argv runs the handler twice and re-registers the servers.
- `RPC<n>` is a **process-wide per-device counter** in `--rpc` order: a member exposing k devices takes k
  consecutive names (`ggml/src/ggml-rpc/ggml-rpc.cpp:3321-3353`). RPC devices report type GPU
  (`:3019-3024`), so `--device` accepts them even for a CPU member.
- With an explicit device list, the model's device order is exactly the given order
  (`src/llama.cpp:160-184`); without one, llama.cpp puts RPC devices **first** "to minimize network
  transfers" (`src/llama.cpp:275`). The output layer lands on the **last** device
  (`src/llama-model.cpp:1783`), and `--fit` fills devices **back to front** (`common/fit.cpp:776, 842`).
  So "RPC first, local last" keeps the output layer local and lets `--fit` fill local GPUs first.
- The multimodal projector defaults to the first listed device (`common/arg.cpp:877`); `-mmdev`
  (`MTMD_BACKEND_DEVICE`) takes exactly one device (`common/arg.cpp:2560-2577`).
- `ggml-rpc-server -d` names devices exactly; an unknown name prints the available devices and exits with
  `No devices found` (`tools/rpc/rpc-server.cpp:154-222`). Without `-d` it exposes every non-CPU, non-ACCEL
  device — in a multi-backend build (e.g. CUDA + Vulkan) each card twice.
- `-H` accepts any IPv4 literal including `0.0.0.0` (only `inet_addr` validity is checked,
  `ggml/src/ggml-rpc/transport.cpp:738`); a non-loopback host only prints a warning.
- Device name prefixes: `CUDA`, `ROCm` (HIP builds), `MUSA`, `Vulkan`, `MTL` (Metal), `SYCL`.

## 3. Placeholders (R4) — changes and additions to issue §3

### 3.1 `${RPC_SERVERS}`

As issue §3 (name, exact-match, the `${API_TOKEN}`-style split: the gateway resolves configured membership —
never liveness — into a wire field the agent substitutes in args, env values and config files; empty value =
hard error `not_permitted`; launch-affecting; version gating with fail-closed documents). Changes:
- **Value per network:** `mesh`: `<member mesh IPv4>:<port>`; `lan`: `<member LAN address>:<port>`, where the LAN
  address is the server's `lan_address` when set, else its `Domain` (hostname or IPv4; llama.cpp resolves it with
  `gethostbyname`, IPv4 only) — §4a. The port is the member's RPC port (direct) or relay port (relay).
- **Wire shape:** `rpc_endpoints` is an ordered list of `{address, device_count}` (my call: the agent needs the
  per-member device count for §3a, and the order is the operator's member order).

### 3.2 `${RPC_DEVICES}` (new, head only)

`RPC0,RPC1,…,RPC(n−1)`, n = Σ `device_count` over the members. A member's `device_count` is the number of its
**deduplicated GPU rows** (manual) or selected GPUs (automatic), or 1 for a CPU member. This is exact only because the member's device list is
fixed by the agent (§1a below). Using it on a spec that is not a cluster head is a hard error.

### 3.3 `${CUDA_FIRST_DEVICE}`, `${VULKAN_FIRST_DEVICE}`, `${METAL_FIRST_DEVICE}` (new, any spec)

The first GPU in the spec's declared order, rendered like its `${…_DEVICES}` sibling (`CUDA3` for GPUs `[3,2]`).
No GPU rows = hard error. Purpose: `-mmdev`, which takes exactly one device.

### 3.4 `${DEVICES}` and `${FIRST_LOCAL_DEVICE}` (new, generic) with a per-spec device backend

- **Device backend** (new spec field `device_backend`): `auto` (default) | `cuda` | `rocm` | `vulkan` | `metal` |
  `sycl`, rendered with prefix `CUDA`/`ROCm`/`Vulkan`/`MTL`/`SYCL`. `auto` is resolved **on the agent** from the
  vendor of the spec's declared GPUs as the agent's own GPU detection reports them: only NVIDIA → `cuda`, only
  AMD → `rocm`, Apple → `metal`, mixed vendors → `vulkan`. (`MUSA` is not offered; SYCL only explicitly.)
- **`${DEVICES}`**: on a cluster head, the RPC devices first, then the local GPUs in declared order
  (`RPC0,RPC1,CUDA3,CUDA2`); on any other spec the local GPUs only. A cluster head without GPU rows renders only
  its RPC devices. A non-head spec without GPU rows: hard error (same reasoning as `${CUDA_DEVICES}`).
- **`${FIRST_LOCAL_DEVICE}`**: the first local GPU in declared order; **on a cluster head without GPU rows it
  falls back to the first RPC device (`RPC0`)** (owner); on a non-head spec without GPU rows: hard error.
- **Env mode renumbering:** when `set_visible_devices` is on in `env` mode **and the agent actually injected a
  visibility variable**, the child renumbers from 0, so the local part renders `<prefix>0…<prefix>(k−1)` in
  declared order. Otherwise (args mode, or no variable injected, e.g. Apple) it renders the declared indices,
  as `${CUDA_DEVICES}` does today.
- **Wrong backend:** a prefix the binary does not have fails at parse (`invalid device: …` from `llama-server`,
  `unknown device: …`/`No devices found` from `ggml-rpc-server`). The agent maps these exit messages to the
  terminal code `runtime.device_backend_mismatch`, whose message names the spec's device backend and the
  override. No automatic retries until the configuration changes.
- **Known limitation (documented follow-up):** the number in a backend device name is the backend's own
  enumeration, not the host index. CUDA matches `nvidia-smi` order only with `CUDA_DEVICE_ORDER=PCI_BUS_ID`
  (the agent does not set it: it would renumber existing specs); Vulkan, and above all mixed-vendor hosts,
  enumerate independently. The operator verifies with `--list-devices`. A later change may match the binary's
  `--list-devices` output to host GPUs.
- The `set_visible_devices` args-mode rule ("an `args`-mode spec must carry a device placeholder") accepts
  `${DEVICES}` like the three `${…_DEVICES}` tokens; the `${…FIRST…}` tokens alone do not count (they do not
  restrict offloading).
- All placeholder lists (agent policy, portal mirror, docs) gain the new tokens; exact-match like their siblings.
  The gateway needs no new wire field for them beyond `rpc_endpoints`, `device_backend` and the cluster-head flag.

### 3a. Cluster head device rules (replaces issue §2's `--device` refusal)

Save checks (backend, mirrored in the portal; the agent re-checks what it can at launch):
1. `${RPC_SERVERS}` appears **exactly once**, either as the value of `--rpc` in args or as `LLAMA_ARG_RPC` in env —
   never both (double registration).
2. If a device list is given (`--device`/`-dev` in args, or `LLAMA_ARG_DEVICE` in env), it must contain the RPC
   devices: `${DEVICES}` or `${RPC_DEVICES}`. Otherwise the members are connected but never used.
3. Ordering: a device list in **args** requires `--rpc` earlier in args **or** RPC via env; a device list in
   **env** requires RPC via env.
4. Portal hints (not blocks): recommend the RPC devices **first** (§2: output layer on the last device; `--fit`
   fills back to front); recommend `-mmdev ${FIRST_LOCAL_DEVICE}` when the device list starts with RPC devices
   (the projector would otherwise land on `RPC0`).
5. Env mode (`set_visible_devices` env, no device list) stays as today; llama.cpp then orders RPC first itself.

### 1b. Member modes (owner decision; extends issue §1–§2)

Each cluster head has `member_mode` = `manual` | `automatic` (default `automatic` — my call; it needs no member-side
spec work). The mode applies to all members of the head; switching it replaces the member list (the portal confirms,
and a running cluster is stopped head first, like any membership edit).

**`manual`** — the issue's model:
- The operator creates an `rpc_server` runspec on the member server (kind `rpc_server`, mapping-less, owned by that
  server's `server_agent` application, D4): binary, GPU rows (none = CPU member), `-t` and other free args,
  `cache_enabled`, `device_backend`, VRAM/RAM estimates, `allowed_head_groups` (consent).
- The head's member list holds **server references**, ordered. The picker offers only servers that the caller may
  manage, that have at least one `rpc_server` spec not yet used by another head, and whose consent admits the head.
  A server with exactly one free spec resolves to it; with several, the picker asks which spec (each spec serves one
  head). Saving a head whose referenced spec is gone, disabled, or taken is refused.
- Deleting or disabling a referenced spec is refused (issue §6 *Changes*).

**`automatic`** — the gateway manages the member:
- The head's member list holds entries **server + ordered GPU selection** (an empty selection = CPU member) + member
  settings: `threads` (`-t`, optional), `cache_enabled`, `device_backend` (default `auto`), VRAM/RAM estimate
  overrides. Entry order is the member order; the selection order is the device order inside the member.
- The gateway derives one `rpc_server` spec per entry into the member server's runtime document, with a stable id
  derived from the head spec id and the member server id (e.g. `rpc-<head spec id>-<server id>`; the plan fixes the
  format). It has no row in `agent_runtime_specs`; its GPU rows, settings and the measured usage live on the
  membership row. The binary is the member server's `ggml-rpc-server` from its binary inventory whose version
  matches the head's `llama-server` (§1c).
- A second head selecting other GPUs of the same server gets a second derived instance with its own `-d` and port.
- **Per-server RPC settings** (new, on the AI server's runtime settings, required before the server is offered in
  automatic mode): `rpc_member_enabled` and `rpc_allowed_head_groups` (consent; default the server's own admin groups).
  No binary setting: the binary comes from the inventory (§1c).

**Both modes:**
- One entry per server per head (two members of the same head on one server are refused: combine the GPUs).
- Several members on one server for different heads are allowed. GPU selections that overlap with another member or
  any other spec on that server only warn in the portal; whether a GPU is free, or can be freed, is decided by the
  member host's admission when `ensureCluster` starts the member (eviction of idle occupants as usual; a held member
  of another cluster triggers Q12: remove that cluster head first when its head may be removed, else the start fails
  with a reason naming the blocking cluster).
- The member's process, ports, relay, observer, telemetry, usage rows (D1), energy part and NetBird rule are per
  member instance in both modes; the agent cannot tell a derived spec from a manual one except by a `derived` flag
  used only for display.
- Authorization: membership writes need management rights on the head's server and every member server (or system
  scope), plus the member side's consent (`allowed_head_groups` of the manual spec, or `rpc_allowed_head_groups` of
  the server in automatic mode).

### 1c. Binary inventory per server (owner decision, 2026-11-05)

Different llama.cpp versions can be installed side by side, and head and members must run compatible builds (same
protocol major, member minor not newer, same `GRAPH_CACHE_MAX` — in practice the same build). Therefore:
- **The agent reports its binary inventory**: every entry of its local `runtime_allowed_binaries` with path, kind
  (from the file name: `llama-server`, `ggml-rpc-server` or the old `rpc-server`, else `other`) and version.
  Version: for `llama-server`, the agent runs `<binary> --version` once and caches the result by path, size and
  modification time (`version: 0.9.0-dev (build 1, commit 2bc46ba)` → build `1`, commit `2bc46ba`; only allowlisted
  binaries, only this flag, bounded by a 10 s timeout); a `ggml-rpc-server` inherits the version of a `llama-server` in
  the same directory (same build), else the path name is searched (`b<digits>` release tag or a 7–40 hex commit
  segment), else the version stays unknown. The inventory rides on the telemetry sample (top-level `binaries`) on the
  first sample after start and whenever it changed; otherwise it is omitted.
- **New "Binaries" tab** in the server_agent application view, before "Runtime limits": path, kind, version, version
  source and origin (agent or manual); a detected version can be overridden; manual entries can be added (e.g. for a
  file-mode agent); entries the agent does not allow are marked, since they would fail with `not_permitted`.
- **Runspecs pick their binary from the inventory** (picker with versions; free text stays possible for existing specs,
  then without a known version). Manual member specs pick a `ggml-rpc-server` the same way.
- **Version matching:** automatic mode selects on each member server the `ggml-rpc-server` whose version (commit,
  else build) equals the head binary's; servers without one are not offered ("no rpc-server in version X"); several
  matches → the first by path. Manual mode: both versions known and different → refused
  (`runtime_spec.cluster_version_mismatch`); either unknown → warning. The runtime protocol check
  (`runtime.cluster_version_mismatch`) stays the safety net.

### 1a. Member command line (changes to issue §1)

- **The agent owns `-H`, `-p` and `-d` on `rpc_server` specs** (my call, tightening the issue): `-d` is injected
  as `${DEVICES}` rendered with the member's device backend over its GPU rows (manual) or GPU selection
  (automatic), `-d CPU` for a CPU member; an
  operator-set `-H`, `-p`, `-d` or `-c` is refused (save check and agent). `-c` is driven by `cache_enabled`.
  Operator args remain for `-t` and anything else upstream adds later. Env mode (`set_visible_devices` env)
  is refused on members: without `-d` a multi-backend build exposes each card twice and shifts every `RPC<n>`.
- `-H` by transport and network: relay → `127.0.0.1` (ephemeral `${PORT}`, dialed only by the local relay);
  direct + `mesh` → the member's mesh IPv4 exactly; direct + `lan` → `0.0.0.0`, or the server's `lan_address`
  when its "listen only on this address" switch (`lan_bind_only`) is on (while that address is not local the
  member stays pending with `bind_mismatch`, D3).
- The agent injects `GGML_RPC_NO_RDMA=1` (members) and `GGML_RPC_NO_COMM=1` (heads), as the issue says.
- Everything else in issue §1 Stage 1 stands (kind beside type, addressed by spec id, non-replicable, out of
  routing/model sync/`LoadedModels`/scaler; TCP readiness; no per-child collect probes; `-c` per-head
  `LLAMA_CACHE`, purge, size, free-disk floor; save requires GPU rows except for a CPU member; VRAM estimate
  required on members with non-NVIDIA GPUs).

## 4. Transport and network (changes to issue §4)

The two transports (`direct`, `relay`) are unchanged in mechanism. They now combine with the network:

| | `mesh` (NetBird) | `lan` (no NetBird) |
|---|---|---|
| Address in `${RPC_SERVERS}` | member mesh IPv4 | member LAN address (`lan_address`, else `Domain`) |
| direct: server binds | mesh IPv4 exactly | `0.0.0.0`, or `lan_address` with `lan_bind_only` |
| direct: who can connect | NetBird policy (needs policy management + enforced deny-by-default + `Default` off) | **anyone who reaches the port** (operator confirmation required) |
| relay: relay binds | mesh IPv4 exactly | `0.0.0.0`, or `lan_address` with `lan_bind_only` |
| relay: who can connect | allowlist (head mesh IP) | allowlist (head's `lan_address`, else IPv4s resolved from its `Domain`) |
| managed NetBird policy | `op-gw-rpc-<member server id>` (one rule per member instance) | none |

**4a. LAN network.**
- **LAN address per server** (new `ai_servers` fields): `lan_address` (optional IPv4 literal) and `lan_bind_only`
  (bool, default off). In a `lan` cluster a server's LAN address is `lan_address` when set, else its `Domain`.
  `Domain` cannot serve on a NetBird-enabled server (peer sync rewrites it to the NetBird DNS name), so **a
  NetBird-enabled head or member in a `lan` cluster requires `lan_address`** (save check
  `runtime_spec.cluster_lan_address_required`). Clusters that mix NetBird and non-NetBird servers therefore run as
  `lan` with explicit LAN addresses where needed.
- Requirements: head and every member have a LAN address (above); the head and member agents declare
  `rpc_cluster`. No NetBird requirement.
- **Direct + LAN requires an explicit acknowledgement** stored on the head (`lan_exposure_ack`), set in the portal
  after a confirmation that says: the member's RPC port is reachable unauthenticated on all of the member's
  interfaces (or on the bind address), and anyone reaching it can allocate memory and run graphs on its devices;
  relay or a bind address narrows it. A save without it is refused (`runtime.cluster_lan_ack_required`).
- **Head identity for the observer and the relay allowlist:** in `mesh` the head's mesh IP; in `lan` the head's
  `lan_address` when set, else its `Domain`, which the member agent resolves to IPv4 addresses itself (Go resolver),
  refreshes every 60 s, and treats any of them as the head. A resolution failure keeps the last good set; with no good set,
  the relay refuses all and the observer reports every peer as unexpected (fail-closed). Connections from the
  member's own addresses are not breaches.
- Default network on enabling a cluster: `mesh` when head and every member are NetBird-enabled with a known mesh
  IP, else `lan`. The operator may switch.
- The mesh-only rules of the issue (§4 direct precondition list, drift handling, the refusal to turn the NetBird
  switches off while a direct cluster exists, §5 policy) apply to `mesh` clusters only.

Everything else in issue §4 (hold by observation in direct mode, relay package wiring, one head per member,
hold on first byte, keepalive, stall signal, telemetry, the per-server port allocator, the reverse member→head
index and its re-push triggers — extended by `Domain`, the RPC bind address and `cluster_network` changes) stands.

## 5–9. Unchanged from the issue except as noted

- **§5 Network policy:** only for `mesh` clusters (Q6: every mesh cluster). Q7: mesh + relay with deny-by-default
  off warns.
- **§6 Start orchestration:** as the issue (members first in parallel, then the head with the cluster-start marker;
  head guard; three gate states for routing; triggers; watchdog; failures; R10 removal through the head; Q4 park
  bound; Q5 explicit startup timeout; Q12 remove-or-wait). The probe before exec in `lan` dials `Domain:port`.
  `runtime.device_backend_mismatch` joins the terminal codes.
- **§7 VRAM/RAM accounting:** as the issue (D1 peak rule, ownership via the member spec, demand pushed to the
  member, cluster totals with lower-bound marking, CPU members RAM-only, Apple members need a budget, holds, short
  member idle timeout D2, co-residency exemption).
- **§8 Energy:** as the issue with Q8 (host share), Q9 (member's own price), Q10, Q11, plus the Tier 2 PUE fix and
  the `usage.Event` doc location fix.
- **§9 Operator workflow:** step 1 depends on the member mode — `manual`: create an `rpc_server` spec per member
  server (no `-d`; the agent sets it); `automatic`: enable RPC membership in each member server's runtime settings and make sure its
  binary inventory (§1c) has a `ggml-rpc-server` of the head's version; step 2 picks the members (servers, or servers + GPUs + settings), the
  network (`mesh`/`lan`, LAN addresses where needed) and, for direct + `lan`, the acknowledgement; step 3 becomes: `--rpc ${RPC_SERVERS}` (or
  `LLAMA_ARG_RPC`), and for local GPUs either env mode or `--device ${DEVICES}` (`-mmdev ${FIRST_LOCAL_DEVICE}` for
  vision models), with `--ctx-size` and optional `--tensor-split` as the issue says (`--tensor-split` positions
  follow the device order).

## 10. Data model and API — changes to issue §10

- Specs: as the issue for manual members (nullable `mapping_id`, `kind`, `application_id`, `allowed_head_groups`,
  `cache_enabled`; heads `cluster_enabled`, `rpc_transport`) **plus** heads `member_mode` (`manual`|`automatic`),
  `cluster_network` (`mesh`|`lan`), `lan_exposure_ack`, and `device_backend` on every spec (default `auto`).
- `ai_servers`: `netbird_ip` (mesh, as the issue) **plus** `lan_address` (optional IPv4 literal), `lan_bind_only`,
  and the automatic-mode RPC settings `rpc_member_enabled`, `rpc_allowed_head_groups`.
- `server_binaries` (§1c): `(server_id, path)` primary key, `kind`, `version`, `commit`, `version_source`
  (`agent`/`path`/`manual`), `source` (`agent`/`manual`), `allowed` (agent-reported allowlist status), `reported_at`.
- Membership rows (`runtime_spec_cluster_members`): position = operator order; `member_kind` = `spec` (manual,
  `member_id` = the `rpc_server` spec id) or `auto` (automatic, keyed by `member_server_id`, with the member settings
  `threads`, `cache_enabled`, `device_backend`, `vram_estimate_mb` per GPU, `ram_estimate_mb`, and the ordered GPU
  selection in a child table). Unique on `(head_spec_id, member_server_id)` (one member per server per head) and, for
  `spec`, on `member_id` (one head per manual spec). Measured usage (D1) on the membership row and its per-GPU child
  rows in both modes, as the issue's §10 describes.
- Agent wire: `rpc_endpoints` becomes `[{address, device_count}]`; `rpc_bind`/`rpc_relays` carry the head's allowed
  identity as `{head_spec_id, ips[], domain}`; `device_backend` on every spec; `cluster_head` marker on head specs
  (needed for `${RPC_DEVICES}`/`${DEVICES}` and the fallback of `${FIRST_LOCAL_DEVICE}`).
- Error codes: the issue's list plus `runtime.device_backend_mismatch` and `runtime.rpc_device_count_mismatch`
  (agent lifecycle, terminal until the configuration changes; §12) and the save
  codes `runtime_spec.cluster_rpc_placeholder_missing`, `runtime_spec.cluster_rpc_twice`,
  `runtime_spec.cluster_device_list_missing_rpc`, `runtime_spec.cluster_device_order` (device list parsed before
  RPC), `runtime_spec.member_flag_reserved` (`-H`/`-p`/`-d`/`-c` on a member), `runtime.cluster_lan_ack_required`,
`runtime_spec.cluster_lan_address_required`, `runtime_spec.cluster_member_server_twice`,
`runtime_spec.cluster_member_spec_taken`, `runtime_spec.cluster_member_rpc_disabled` (automatic mode on a server
without RPC settings)
  (working names; the plan fixes the final set).

## 11. Security — changes to the issue

- `mesh` clusters: as the issue.
- `lan` clusters: direct mode exposes the member's RPC port to every host that reaches it, unauthenticated (accepted
  by the owner per cluster through `lan_exposure_ack`); the observer reports unexpected peers but cannot prevent
  them. The RPC bind address narrows the interfaces; relay mode adds the allowlist (head IPv4s from `Domain`) and
  one-head exclusivity. DNS is now in the trust path of the `lan` allowlist (a spoofed `Domain` resolution can admit
  another host); documented as residual risk, with relay + a fixed head IP literal in `Domain` as the hardening.
- Authorization (rights on every involved server, member consent via `allowed_head_groups`, 404-no-leak) unchanged.

## 12. Stage 0 on this branch

Local (M4 Max, Metal + CPU, approved downloads: llama.cpp @ `2bc46ba`, `stories260K.gguf`): confirm the §2 parse
rules on a real build (both orders, env/argv mixes, double `--rpc`), `RPC<n>` counting with a 2-device member,
`-d CPU`, `-H 0.0.0.0`, `GGML_RPC_NO_COMM`/`GGML_RPC_NO_RDMA`, the `-c` path under `LLAMA_CACHE`, a blocked second
client, memory freed on disconnect, what a CPU member reports as free memory, `--fit` with members, Metal buffers in
RSS, and the projector device without `--device`. Results are recorded here and folded into the docs.

**Local results (2026-11-05, macOS M4 Max, Metal build with `-DGGML_RPC=ON` @ `2bc46ba`, `stories260K.gguf`,
all on 127.0.0.1):**
- Parse rules confirmed on the real build: argv `--rpc … --device RPC0,RPC1,RPC2,MTL0` → healthy and generates;
  env `LLAMA_ARG_RPC` + env `LLAMA_ARG_DEVICE` → healthy; env `LLAMA_ARG_RPC` + argv `--device` → healthy; argv
  `--device RPC0 …` before `--rpc` → `error while handling argument "--device": invalid device: RPC0`; env
  `LLAMA_ARG_DEVICE` + argv `--rpc` → `error while handling environment variable "LLAMA_ARG_DEVICE": invalid
  device: RPC0`; a name beyond the member devices (`RPC3`) → `invalid device: RPC3`.
- **`--rpc` in env and argv is additive, not an override**, despite llama.cpp's own warning (`… will be
  overwritten by command line argument --rpc`): env `LLAMA_ARG_RPC=B` + argv `--rpc A` registers B as `RPC0` and A
  as `RPC1…`. Identical values register once. This makes §3a rule 1 ("exactly once") mandatory, not cosmetic.
- `RPC<n>` counting confirmed: a member started with `-d MTL0,CPU` gives `RPC0` (its MTL0) and `RPC1` (its CPU), the
  next member `RPC2`. `--list-devices` together with `--rpc` prints the mapping (`RPC1: 127.0.0.1:50152 …`).
- The member's start banner prints `Starting RPC server v8.0.1`, the endpoint, and a `Devices:` block with one
  line per exposed device — the member agent can read the protocol version and **verify its device count**
  against the expected `device_count` (my call: a mismatch fails the member start with
  `runtime.rpc_device_count_mismatch`, so `RPC<n>` can never silently shift).
- **A CPU member reports the host's total RAM as free** (`CPU: Apple M4 Max (36864 MiB, 36864 MiB free)` on a
  36 GiB host), so the automatic split sizes it as if all host RAM were free. Confirms the portal's
  `--tensor-split` recommendation for heads with CPU members (issue §7).
- **A second client blocks and then takes over:** a second head against a held member stays alive and unhealthy
  (blocked in the handshake); once the first head disconnects, the second one completes its start and holds the
  member. In direct mode a queued foreign head would therefore inherit the member — the observer must report it
  and the gateway must treat a `held_by` that names another head as a breach.
- Losing a member aborts the head (`Remote RPC server crashed or returned malformed response`, `ggml_abort`).
- The head connects and disconnects several times per member during startup (member log: repeated
  `Accepted client connection` / `Client connection closed`), then holds one established connection per member
  while running.
- No communicator listener (`port + 1000`) appeared in a default two-member run, with or without
  `GGML_RPC_NO_COMM=1`; forcing the variable stays (harmless, and it closes the path on builds that do negotiate).
- Member memory returns to the idle level after the head disconnects (RSS 51.4 MB idle → 52.5 MB held → 51.4 MB);
  the tiny model makes this a weak signal only.
- Test-model caveat: a Metal-backed member crashes with `ggml-metal-ops.cpp:215: unsupported op` when the head runs
  flash attention for `stories260K` (its head size is unsupported on Metal, and the RPC device does not tell the
  head). `-fa off` avoids it. The CI e2e uses a CPU build, where this does not arise; the docs note that an RPC
  device cannot report unsupported ops.
- Not testable locally: `-H 0.0.0.0` (binding all interfaces was refused by the local sandbox; source-verified
  only, §2), `-c` (the tiny model has no tensor over 10 MiB), Metal buffers in RSS and `--fit` with members (need a
  model of real size), the projector device (needs an mmproj file). These move to the owner-host list.

Owner hosts (coordinated, not guessed): direct vs relay throughput and load times with 1–2 members incl. a CPU
member; NetBird P2P vs relayed; Windows/macOS firewall admission of the RPC/relay ports; observer cost on Windows;
LAN reachability of a mesh-IP bind; NVIDIA and Vulkan members.

## 13. Tests and docs

As the issue's first comment (*Stage 1 tests and docs*), plus:
- Agent: rendering of `${RPC_DEVICES}`, `${DEVICES}`, `${FIRST_LOCAL_DEVICE}` (incl. the RPC fallback), the three
  `${…_FIRST_DEVICE}` tokens, `auto` backend resolution per vendor mix, env-mode renumbering, the
  `device_backend_mismatch` exit mapping; member `-d` injection and refusal of operator `-H`/`-p`/`-d`/`-c`; LAN
  bind (`0.0.0.0` / bind address) and Domain-resolved allowlist incl. fail-closed on resolution failure.
- Gateway: §3a save checks (both device modes, all env/argv mixes); `lan` derivation (`lan_address`/`Domain`
  endpoints and head identity, `lan_address` required on NetBird servers, no policy, ack required for direct);
  network switch re-push; both member modes (manual picker/refusals, automatic derivation with stable ids, two heads
  on disjoint GPUs of one server → two instances and ports, one-entry-per-server rule, mode switch replaces the list,
  consent in both modes, derived member admission blocked by a held member → Q12).
- CI real-cluster e2e: one head in args mode (`--device ${DEVICES}`, CPU build → RPC devices only) and one in env
  mode, `lan` network, both transports.
- Docs: ADR-096 records the transport/network defaults, the placeholder family and the device-order rule; the
  placeholder section of `agent-runtime-manager.md` gains the new tokens; the Vulkan/enumeration-order limitation is
  a documented follow-up in §11 risks.
