# Agent-reported LAN interfaces for `lan` RPC clusters, and the agent key — design

Status: draft, awaiting approval. Branch-local working file (removed before the PR).

## 1. Intent

A llama.cpp RPC cluster on the `lan` network reaches its members at
`ai_servers.lan_address`, else at the server's `Domain`. On a NetBird-enabled
server the peer sync rewrites `Domain` to the NetBird DNS name, so today an
admin must type the server's LAN IPv4 into the server dialog by hand before the
server can join a `lan` cluster
(`docs/architecture/cross-cutting/networking-mesh.md` §12,
`clusterAddress` / `memberBindIP` / `clusterHeadIdentity` in
`gateway/backend/internal/portal/service_cluster_config.go`,
`lanAddressKnown` / `checkServerInNetwork` in `service_cluster.go`).

The hand-typed IP is wrong when DHCP hands out a new lease. It is also
tedious, and it is error-prone on hosts with several networks.

The feature: the server agent reports its network interfaces with their IPv4
addresses. In the portal the admin picks one **interface**, not an IP, and the
gateway always uses that interface's current address. With no pick, the gateway
uses the most likely LAN interface.

The work introduces a per-agent secret, **`agent_key`** (§4). It is held in the
agent's config file, like the gateway's own keys in its config JSON. The same
key also settles issue #61 (§4b): the plaintext upstream token in the agent's
runtime-config cache. This change closes #61.

## 2. Goals

1. The agent reports the host's active network interfaces with their IPv4
   addresses, continuously enough that a DHCP change reaches the gateway
   within about a minute. NetBird and loopback are never reported as candidates.
2. Per server, the gateway stores a **LAN address source**:
   - `auto` (default): resolves in this order (author's decision):
     1. A server **without** NetBird and with a non-empty `Domain` keeps
        dialing its `Domain`, exactly as today, even once its agent reports
        interfaces. Upgrading the agent must not change its address or
        relaunch its clusters.
     2. Otherwise the most likely reported interface (§5).
     3. Otherwise no address (`ClusterWithheldAddressUnknown`, as today).

     Whoever wants a specific interface on a server like that picks it
     explicitly.
   - `interface`: one chosen interface. The gateway stores the identity of the
     interface (§4b), never its IP.
   - `manual`: today's hand-typed IPv4 (`lan_address`), kept for addresses the
     agent cannot see (NAT, a forwarded port, an agent that is too old).
3. The **effective LAN address** of a server is derived from the source and the
   latest report. Every place that reads `lan_address` today reads the
   effective address instead: cluster address, member bind address under
   `lan_bind_only`, and the `lan` network checks. *Author's call:* for the
   **head identity** (the allowlist members accept the head from), a head
   whose source is `auto` or `interface` sends **all** of its reported
   non-NetBird IPv4s, not only the effective one. A multi-homed head may
   reach a member through another NIC, and a relay member would otherwise
   refuse it. A head whose source is `manual` sends exactly the manual
   address, as today.
4. **Fallback.** When the chosen interface is missing from the latest report
   (cable unplugged, adapter gone, no IPv4), the effective address falls back
   to the most likely interface. The portal shows a warning on the server: the
   chosen interface is missing and a fallback is in use, naming the fallback.
   When the interface comes back, the choice applies again without any admin
   action.

   **Exception, `lan_bind_only` (author's decision):** on a server that listens
   on its LAN address only, a missing chosen interface does **not** fall back.
   The server's members are withheld (address unknown) and the portal warns,
   until the interface returns. The admin may have pinned the member to an
   isolated cluster network, and falling back would expose it on a wider one.
   As a head, such a server still uses the fallback for its identity. The rule
   only guards what it listens on.
5. **Automatic cluster rebuild.** When a server's effective LAN address
   changes, every `lan` cluster it takes part in (as head or member) is
   re-derived and pushed. *Builder's call (settled by the author):*
   "rebuild" means relaunching exactly what the existing launch comparison
   relaunches; nothing is forced on top of it.
   - A member's dial address is in the head's endpoint list, so the head
     relaunches.
   - A member relaunches itself only when its bind address changes
     (`lan_bind_only`).
   - A head's own identity change is applied live by the member agents
     (`sameLaunchShape`).

   The portal notice appears on every effective-address change of a
   participant of a `lan` cluster, whether or not something relaunched. It
   shows on the cluster head and on the server: which address changed from
   what to what, when, and whether a relaunch followed.
6. **Portal UI.** The server dialog (RPC/LAN section) replaces the bare
   LAN-address field with a source selector:
   - "Automatic" shows the address it currently resolves to and which
     interface it comes from, marked as auto-selected.
   - One entry per reported interface, with its name and current IPv4(s).
   - "Manual", with the IPv4 field as today.

## 3. Non-goals and anti-goals

**Non-goals**

- IPv6. `lan_address` is IPv4-only today and stays so.
- Changing `mesh` clusters. They keep using `netbird_ip`.
- Exposing interfaces anywhere other than the server's admin view.

**Anti-goals** (failures even if the feature "works")

- **No restart loops.** A flapping interface, or a report that briefly misses
  one, must not relaunch a running cluster again and again. One real change
  means one rebuild.
- **No silent widening.** Under `lan_bind_only` the member must never fall
  back to `0.0.0.0` because the address is unknown. That rule exists today
  (`ClusterWithheldAddressUnknown`) and stays.
- **Nothing changes for existing setups.** A server that has `lan_address` set
  today keeps exactly that address (it migrates to source `manual`). A server
  without one migrates to `auto`, whose first rule (§2.2) keeps a
  non-NetBird server on its `Domain`. So the rollout relaunches no cluster.
- **No NetBird address as a LAN address.** The gateway also discards any
  reported address equal to the server's `netbird_ip`.
- **No privacy regression.** The agent's reports carry no MAC addresses today,
  by explicit rule (`sample/system_report.go`,
  `telemetry-usage-observability.md`). The raw MAC still never leaves the
  host (§4b).

## 4. The agent key and interface identity

### 4a. The agent key (author's decisions)

- **Where it lives:** a new agent config setting, `agent_key`, a random secret
  of at least 32 bytes, encoded as text. Like the other settings it can also
  be set through an environment variable. The agent only reads it and never
  writes its own config, as today.
- **New configs get one automatically.** The portal's generated
  `server-agent.json` contains a fresh random `agent_key`:
  - In the browser path (`buildServerAgentConfig`,
    `AgentTokenSection.tsx`) it is generated client-side, so the gateway never
    sees it.
  - In the `curl` path (`/api/agent/v1/download/config`,
    `buildAgentConfigJSON`) the gateway generates it for that one response and
    never stores or logs it.
  - The shared golden (`server-agent/testdata/server-agent.config.jsonc`)
    carries a placeholder at that one value, and the byte-for-byte parity
    test compares with that value masked.
- **One key, several purposes.** The agent never uses `agent_key` directly. It
  derives one subkey per purpose with HKDF-SHA256 and a fixed purpose label,
  such as `op-ai-gateway/interface-id/v1` and
  `op-ai-gateway/runtime-cache/v1`. One purpose cannot weaken another, and
  later purposes slot in.
- **No key configured (existing installs):** everything still works, with
  the weaker fallbacks named in §4b and §4c. The portal shows a hint on the
  server that `agent_key` is missing and what adding it buys. For that, the
  agent reports whether a key is configured, never the key itself.
- **Key lost or changed:** stored interface choices read as "interface
  missing" (the fallback with a warning, §2.4), and a cached token cannot be
  opened (§4c). Both recover on their own or with one re-pick. This is
  acceptable.
- **Docs:** an ADR records the key, its storage and the purpose derivation.
  `configuration.md` documents the setting.

### 4b. Interface identity

The stored choice must survive a reboot and an IP change. It should ideally
also survive a rename (Windows renames adapters to "Ethernet 2" and the like).

Each reported interface carries an opaque, stable `id`:

- **With `agent_key`:** HMAC-SHA256 of the hardware address under the
  interface-id subkey, truncated. An interface without a hardware address
  (tun, some virtual adapters) hashes its name instead.
- **Without `agent_key`:** the `id` is the interface name. A renamed adapter
  then loses the choice and falls back with a warning.

Requirements:

- The raw MAC never leaves the host and is never stored on the gateway.
- The key is not stored on the gateway, so the stored ids cannot be reversed
  from the gateway's database. A plain unkeyed hash could be: the MAC space
  is small enough to brute-force.
- The same adapter on two servers yields different ids.
- The gateway matches the stored choice by `id` and shows `name` for display
  only.

The privacy rule in `sample/system_report.go` and in
`telemetry-usage-observability.md` is amended in the same change: hardware
addresses may leave the host only as such a keyed hash.

### 4c. The upstream token in the runtime-config cache (closes #61)

**Today:** `GatewaySource.persist`
(`server-agent/internal/runtime/config_client.go`) writes the whole runtime
document to disk, `api_token` included, in plaintext (file mode 0600). The doc
comment on `Spec.APIToken` (`types.go`) says the token is "never persisted by
the agent".

**Decision (author):** the token stays in the cache, so a managed child with
`${API_TOKEN}` still starts after an agent restart while the gateway is
unreachable. That is what the cache exists for.

- **With `agent_key`:** each spec's token is stored encrypted under the
  runtime-cache subkey, using AEAD (AES-256-GCM or XChaCha20-Poly1305,
  builder's call) with a fresh nonce. A copied cache file or a backup of it
  no longer exposes the token.
- **Without `agent_key`:** the behavior is unchanged (plaintext, 0600). The
  comment and docs now say so honestly. This is #61's option 1.
- **Reading the cache:**
  - An old plaintext cache is still read, and the next persist rewrites it in
    the new form.
  - A token that cannot be opened (key lost or changed) is treated as empty.
    The next gateway contact delivers it again. A spec that needs
    `${API_TOKEN}` stays down until then, as it would without a token, and
    the agent logs why. The token value is never logged.
- **Unchanged:** the wire push (`resolvePushToken`), the `${API_TOKEN}`
  expansion and the masking in reports (#61's own scope note).
- **Docs:** the comment on `Spec.APIToken`, the `writeFileAtomic` comment
  ("not supposed to carry secrets") and the token section of
  `agent-runtime-manager.md` are corrected to describe the disk copy.

## 5. "Most likely" interface (builder's call, within these rules)

The agent, which can see the routing table, marks one candidate as preferred.
The gateway uses the agent's mark when it can and applies its own
NetBird-address filter. Ranking, in order:

1. The interface that carries the default IPv4 route.
2. A private address (RFC 1918) over any other address. Never link-local
   (169.254/16), never 100.64/10 (CGNAT/NetBird range), never loopback.
3. Physical over virtual. Docker/bridge, Hyper-V/WSL vEthernet, VPN/tun/utun
   and NetBird (`wt*`) interfaces rank last or are excluded.
4. Ties are broken deterministically, so the pick does not flip between
   reports.

On Windows with WSL mirrored networking (required by the docs), the agent in
WSL sees the host's interfaces. No special case is needed.

## 6. Data and transport (builder's call, within these rules)

- The interface list is **live state**, not static inventory. The hardware
  `SystemReport` is collected once at startup, which is too static for this.
  Use the periodic sample or a dedicated agent message, either on every tick
  or whenever the list changes plus at connect.
- The gateway persists the last report per server, so a gateway restart can
  derive clusters before the agent reconnects. As with `netbird_ip`, the
  report is written through a **targeted writer**. The full-row server update
  never writes it, and the source fields are written only by the portal save.
- This needs a store migration. Run the SQLite and Postgres legs.
- An old agent that sends no interfaces gives an empty list. `auto` then
  behaves exactly as today (`Domain` rules). A server-agent protocol version
  bump follows the repo's usual rule.

## 7. Constraints

- Follow AGENTS.md: feature branch and PR, never commit to `main`.
- Update `docs/architecture/` (networking-mesh §12, data model, API surface,
  agent runtime manager §3.7, the ADR log) in the same change.
- Repo text in English.
- Keep the LAN host names and addresses of the user's machines out of repo
  text, test fixtures included. Use documentation ranges (192.0.2.0/24 etc.).

## 8. Left to the builder

The exact wire shape, the table/column layout, the debounce for the
anti-restart-loop rule, how the rebuild notice is stored and shown (an existing
runtime/cluster status surface is preferred over a new one), the exact
virtual-interface patterns per OS, the encoding of `agent_key`, the AEAD
choice and the phasing of the work.

The phasing has one rule (author's decision). The agent key (§4a) comes first,
and the encrypted token cache (§4c, #61) is a phase of its own right after it,
reviewable and shippable without the interface work.

## 9. Open questions

None. Interface identity and the key were decided in §4.
