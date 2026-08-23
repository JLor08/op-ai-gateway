# NetBird-only transport — docker-compose runbook (Phase 2)

This makes the gateway↔AI-server plane run **only over the NetBird overlay**: the agent-ingest
endpoint (`/api/agent/v1/telemetry`) is reachable only over NetBird, and the gateway only dials
AI-server apps over NetBird. Inference clients (`/v1`, `/openai`, `/anthropic`) and the portal/admin
UI stay on the public nginx ingress.

The gateway joins the mesh via a **NetBird client sidecar** (`netbird` service). The `backend`
container shares that sidecar's network namespace (`network_mode: "service:netbird"`), so the gateway
process sees the `wt0` interface + the `100.x` NetBird IP and can bind its agent listener there.

> **The runtime `netbird_only` switch (System Settings) turns the restriction on/off live.** This
> compose stack provides the *capability* (the mesh-bound agent listener + the mesh route). The
> code-level details are in `docs/superpowers/specs/2026-07-25-netbird-only-transport-design.md`.

## 0. Prerequisites

- A NetBird deployment you administer (self-hosted management URL, or the NetBird cloud) + an
  admin API token.
- Docker + docker compose on the gateway host, with `/dev/net/tun` available (WireGuard).

## 1. Prepare NetBird (once)

1. In the NetBird dashboard, create a **group** for the gateway peer, e.g. `op-gw-portal`.
2. Create a **setup key** (reusable is convenient for re-provisioning; one-off works because the
   sidecar persists its identity) with `op-gw-portal` in its auto-groups.
3. Note the **management URL** (self-hosted) and your **admin API token** (for the gateway's NetBird
   module in System Settings).

## 2. Configure `.env`

Copy `.env.example` → `.env` and set at least:

```
NB_SETUP_KEY=<the setup key from step 1>
NB_MANAGEMENT_URL=https://netbird.example.com   # empty = NetBird cloud
NB_HOSTNAME=op-gateway                           # the peer name you'll pick in System Settings
OP_AI_GATEWAY_AGENT_PORT=8081                     # agent listener port on the wt0 IP (default)
```

Leave `OP_AI_GATEWAY_AGENT_ADDR` empty (the gateway peer is selected at runtime in System Settings).
Set `OP_AI_GATEWAY_AGENT_ADDR=<ip>:<port>` only if you want to hard-pin the bind address (e.g. from a
known NetBird IP) — it overrides the peer selection and skips the admin-API lookup at boot.

**Separate encrypted agent port (optional).** By default the agent listener speaks both plaintext and
TLS on the one port (first-byte sniffing). To run a *dedicated* TLS-only port alongside the plaintext
one, set `OP_AI_GATEWAY_AGENT_TLS_SEPARATE=true` (or flip `cert_mesh_tls_mode` to `separate` in the
portal at runtime). The TLS port is `OP_AI_GATEWAY_AGENT_TLS_PORT` (default `8443`), or pin the full
bind with `OP_AI_GATEWAY_AGENT_TLS_ADDR=<ip>:<port>` (empty = derive `<mesh-ip>:8443` from the peer,
like `AGENT_ADDR`). The separate mode adds the `op-gw-agent-ingest-tls` policy (§6). Full runbook +
the host-published exposure model: README-certificates.md §10.10.

## 3. Persistence — the critical part

The `netbird-client` named volume mounts **`/var/lib/netbird`**, which holds the WireGuard key + the
peer identity/config. It MUST survive restarts and image updates:

- On a plain restart or `docker compose pull && docker compose up -d`, the volume persists → the
  sidecar reuses its identity → the peer keeps the **same NetBird IP** → the gateway's agent listener
  rebinds the same address with no re-selection needed.
- **Without** the persisted volume, every recreate re-enrolls as a **new** peer (new IP). That breaks
  the gateway-peer selection + the agent-listener bind and fills the tracking group with dead peers.
- Therefore: **never `docker compose down -v`** in production (that deletes the volume). Back it up
  like any stateful volume (e.g. `docker run --rm -v deploy_netbird-client:/v -v $PWD:/b alpine tar
  czf /b/netbird-client.tgz -C /v .`).
- `NB_SETUP_KEY` is consumed only on the **first** enrollment (empty volume); with a persisted
  identity it is ignored, so leaving it in `.env` is idempotent.

## 4. Start + enroll

```
docker compose up -d
docker compose logs -f netbird   # watch it connect to the management server
```

The `netbird` peer registers using `NB_SETUP_KEY` and appears in your NetBird dashboard as
`op-gateway` (in `op-gw-portal`). The `backend` shares its netns; nginx (`web`) fronts the public
API at host `:8080`.

## 5. Point the gateway at its own peer + turn on the restriction

In the portal (System Settings, system-admin):

1. **NetBird module:** set the admin API URL + token (this is the gateway's admin-API client, used to
   resolve the peer + manage AI-server peers — separate from the sidecar's `NB_*` env).
2. **Gateway peer:** select the `op-gateway` peer in the gateway-peer picker. This stores
   `netbird_gateway_peer_id`; on the next backend start the gateway resolves that peer's IP and binds
   the agent listener to `100.x:8081`.
3. **Restart the backend** so the agent listener binds:
   ```
   docker compose restart backend
   ```
4. Verify: `GET /api/system/netbird/status` (or the System-Settings status note) shows
   `agent_listener_active: true` with the `100.x:8081` address. If it is `false`, the peer wasn't
   resolvable/local at boot — check the sidecar is connected and the selected peer is `op-gateway`,
   then restart the backend again.
5. Point your ServerAgents at the gateway's NetBird address (`http://<op-gateway dns or 100.x>:8081`
   for `/api/agent/v1/telemetry`) instead of the public URL. **Note the scheme is plain `http`** —
   the agent listener terminates no TLS; confidentiality/integrity come from the WireGuard tunnel.
   (TLS is only terminated at nginx on the public plane, which the agent listener bypasses.)
6. Flip **`netbird_only` ON**. From now on the public listener rejects `/api/agent` (`403
   netbird.only`) and off-mesh AI-servers drop from routing. (Fail-safe: if no agent listener is
   active, turning it on does NOT cut off agents on the public path — the status note warns you.)

## 6. NetBird ACL policy (defense-in-depth)

The agent listener already binds the `wt0` IP only, so it is unreachable off-mesh by construction.
As an extra layer, add a NetBird **access policy**: allow the AI-server peer groups → `op-gw-portal`
on TCP `8081`, deny everything else. The gateway also needs to reach each AI-server app port over the
mesh (allow `op-gw-portal` → the AI-server groups on the app ports).

> **Auto-managed (recommended).** If you enable **`netbird_manage_policies`** in System-Settings →
> NetBird, the gateway maintains BOTH of these policies for you and you do NOT add them by hand:
> - `op-gw-access-<serverID>` — `op-gw-portal` → that server's tracking group on its **active app
>   ports** (least-privilege gateway→server; per-server scope/override applies).
> - `op-gw-agent-ingest` — **all** NetBird AI-server tracking groups → `op-gw-portal` on the **agent
>   port** (default `8081`; server→gateway telemetry). This one is account-wide and always covers every
>   NetBird server regardless of the per-server policy scope/override, so agent telemetry keeps working
>   even under **deny-by-default** (no account-wide "Default" All↔All policy). It reconciles once per
>   fleet pass (≤ the reconcile interval) — a freshly-enabled server appears in it on the next pass.
> - `op-gw-agent-ingest-tls` — **only in the separate encrypted agent-port mode** (`cert_mesh_tls_mode
>   =separate`, see README-certificates.md §10.10): the same all-server-groups → `op-gw-portal` shape as
>   `op-gw-agent-ingest`, but on the **dedicated TLS port** (`OP_AI_GATEWAY_AGENT_TLS_PORT`, default
>   `8443`). It is created only while the mode is `separate` and **deleted** on the next fleet pass after
>   switching back to `combined`; in `combined` mode `op-gw-agent-ingest` is the only agent policy.
>
> Every gateway-managed policy also carries an English `Description` on both the policy and its rule
> ("Managed by the OP AI Gateway — … Do not edit manually.") so the NetBird console shows who owns it;
> a manual edit is restored on the next reconcile (the description is part of the drift check).
>
> A NetBird server that is only **manually linked** (no `op-gw-<serverID>` tracking group) is not
> included in `op-gw-agent-ingest`/`op-gw-agent-ingest-tls`; add a manual ACL for it, or re-enroll it
> through the portal so it gets a tracking group. With `netbird_manage_policies` **off**, add the
> policies by hand as above.

## 7. Host firewall (defense-in-depth)

Optionally drop inbound `8081` on every host interface except `wt0` (e.g. an `iptables`/`nftables`
rule), so even a misconfiguration can't expose the agent port off-mesh.

## 8. Updates

```
docker compose pull
docker compose up -d
```

The `netbird-client` volume persists → the sidecar keeps the same peer/IP → the backend (recreated
because it shares the recreated netns) rebinds the agent listener to the same address. No re-selection
is needed. If `agent_listener_active` is `false` after an update (a boot race where the backend
started before `wt0` was up), just `docker compose restart backend`.

> **Sidecar is a hard dependency of the whole gateway.** Because the backend's listener lives in the
> `netbird` netns, nginx proxies the ENTIRE public surface (portal, inference, `/api/**`, `/healthz`)
> to `netbird:8080`. If the sidecar is down, the public API returns 502 too — not just the agent
> plane. And **never `docker compose restart netbird` on its own**: recreating the sidecar rebuilds
> the netns and orphans the backend's networking until the backend is restarted too. Use
> `docker compose up -d` (which recreates dependents) or restart both.

## 9. Optional: make the backend wait for the mesh automatically

> **Do NOT enable this `netbird status` healthcheck when using autonomous (Phase B) sidecar
> enrollment (§11).** It deadlocks: the Phase-B wrapper blocks a fresh peer waiting for the gateway
> to write the minted key, but the gateway only writes it after the `backend` starts, and switching
> the dependency to `condition: service_healthy` gates `backend` on the sidecar being mesh-ready
> first → the sidecar never connects → the backend never starts → the key is never written (circular
> wait). This healthcheck is **only** for the operator-provided `NB_SETUP_KEY` path (§1–§10), where
> the sidecar can enroll without waiting on the backend. Keep the `service_started` default for
> Phase B (the Phase-1 fail-safe already prevents an outage).

The `backend` depends on `netbird` with `condition: service_started` (the NetBird client has no
built-in healthcheck), so on a co-restart the backend can occasionally start before `wt0` has an IP →
the agent listener fails safe to the public-only listener until the next backend restart. To wait
automatically, add a healthcheck to the `netbird` service and switch the backend dependency to
`service_healthy`. Tune the grep to your NetBird version's `netbird status` output:

```yaml
  netbird:
    # ...
    healthcheck:
      test: ["CMD-SHELL", "netbird status 2>/dev/null | grep -qi 'management: *connected'"]
      interval: 10s
      timeout: 5s
      retries: 12
      start_period: 30s
  backend:
    depends_on:
      netbird:
        condition: service_healthy   # instead of service_started
      db:
        condition: service_healthy
```

> Caution: a wrong grep string marks `netbird` permanently unhealthy → the backend never starts.
> Validate the exact `netbird status` output in your version before enabling this, and keep the
> `service_started` default if unsure (the Phase-1 fail-safe already prevents an outage). The
> `CMD-SHELL` healthcheck also assumes the image ships `/bin/sh` + `grep` — verify that too (or use
> an exec-form check against a binary you know exists).

## 10. Troubleshooting

- **`agent_listener_active: false`** — the selected peer's IP wasn't a local interface at boot
  (sidecar not connected yet / wrong peer picked / admin API unreachable). The gateway degrades to
  the single public listener and logs a `Warn` ("agent listener bind failed / could not resolve the
  gateway peer IP"). Fix the cause, then `docker compose restart backend`.
- **Agents can't reach the gateway** — confirm they target the `100.x:8081` (or the peer's NetBird
  DNS) address, that both peers are in the mesh, and (if set) the NetBird ACL allows the AI-server
  groups → `op-gw-portal` on `8081`.
- **Relayed connections / slow** — NetBird in a non-host netns can fall back to relayed connections
  (netbird issue #2604). If you need direct connectivity, run the sidecar with `network_mode: host`
  (and bind the backend accordingly) as a fallback topology.
- **Never** run `docker compose down -v` — it deletes the `netbird-client` volume and forces a new
  peer identity.

## 11. Autonomous sidecar self-enroll (Phase B)

Sections 1–10 use an **operator-provided** setup key (`NB_SETUP_KEY` in `.env`, the Phase-2 style).
Phase B is the **autonomous alternative**: the gateway **mints** its own NetBird setup key from the
portal and hands it to the sidecar over a shared volume — **no dashboard step, no copy-paste**.

**How it works.** The `netbird` service builds the **custom image** from `Dockerfile.netbird` (the
stock NetBird client + a small entrypoint wrapper). On a **fresh** peer, when `NB_SETUP_KEY` is empty
and `NB_ENROLL_KEY_FILE` is set, the wrapper **waits** for the gateway to drop a key file, then loads
it into `NB_SETUP_KEY` and runs the stock `netbird up`. The gateway writes that file when you press a
button in System Settings.

> **Why a separate `NB_ENROLL_KEY_FILE` and not netbird's own `NB_SETUP_KEY_FILE`?** netbird's CLI
> treats `--setup-key` (`NB_SETUP_KEY`) and `--setup-key-file` (`NB_SETUP_KEY_FILE`) as **mutually
> exclusive** and aborts **every** command if *both* env vars exist — even `NB_SETUP_KEY=""`. Because
> the backend and sidecar **share a network namespace**, a crashing `netbird up` takes the netns down
> and the **entire public API (including `/api/auth/login`) returns 502**. So the wrapper reads the
> path from its own `NB_ENROLL_KEY_FILE`, hands netbird **only** `NB_SETUP_KEY`, and strips any
> `NB_SETUP_KEY_FILE`/empty `NB_SETUP_KEY` before `exec`.

The backend (`OP_AI_GATEWAY_NETBIRD_KEY_FILE`) and the sidecar (`NB_ENROLL_KEY_FILE`) point at the
**same file** on a **shared, TRANSIENT tmpfs volume** (`netbird-key:/shared`) — the two containers
share a netns but not a filesystem, so the key needs a shared volume, and tmpfs keeps the minted key
**in memory** (it never lands on persistent disk). Both default to `/shared/netbird-setup-key`.

**Steps.**

1. Ensure `.env` leaves `NB_SETUP_KEY` **empty** and keeps the two defaults
   `OP_AI_GATEWAY_NETBIRD_KEY_FILE=/shared/netbird-setup-key` + `NB_ENROLL_KEY_FILE=/shared/netbird-setup-key`.
2. `docker compose up -d` (this **builds** the custom sidecar from `Dockerfile.netbird`). The fresh
   sidecar logs `netbird-enroll: not yet enrolled; waiting for the setup key ...` and blocks.
3. In System Settings, configure the **NetBird module** (admin API URL + token) as in §5, then click
   **"Sidecar enrollen"**. The gateway mints a one-off setup key (group `op-gw-portal`), writes it to
   `/shared/netbird-setup-key`, and returns it once (a fallback copy you can also paste manually).
4. The waiting sidecar reads the key and `netbird up`s → the `op-gateway` peer self-enrolls. Continue
   with §5 (select the gateway peer, restart the backend, verify `agent_listener_active`, flip
   `netbird_only`).

**Notes.**

- **Marker on the persistent identity.** The wrapper touches `/var/lib/netbird/.gw-enroll-attempted`
  after taking an enrollment path, so a later restart (when the transient `/shared` volume is empty
  again) **skips the wait** and just reconnects via the persisted `netbird-client` identity. **Wiping
  `/var/lib/netbird`** (which also wipes the identity) removes the marker and correctly forces a fresh
  wait + re-enroll.
- **Updating the client.** Bump the base tag in `Dockerfile.netbird` (`ARG NETBIRD_VERSION`) and
  `docker compose build netbird && docker compose up -d`.
- **`NB_SETUP_KEY` disables the autonomous path.** If you set `NB_SETUP_KEY` in `.env`, the wrapper
  skips the wait and enrolls with that env key the old Phase-2 way. The **Phase A** manual button
  ("Gateway-Setup-Key erstellen", which mints + displays a key for you to paste) also still works
  regardless — Phase B just automates the handoff.
- **A bad key** leaves the marker set, so a later restart won't re-wait; retry = wipe the marker (and
  the `netbird-client` volume) then re-enroll. The gateway mints a valid key, so this is a corner case.

## Agent transport: POST vs WebSocket

The ServerAgent sends telemetry over HTTP POST by default (one request per sample,
3 s interval). Set `transport=websocket` (agent flag `-transport=websocket`, env
`OP_AGENT_TRANSPORT=websocket`, or `"transport":"websocket"` in `server-agent.json`)
to stream over one persistent WebSocket connection instead; the interval may then go
as low as 250 ms.

- **NetBird mesh path** (agent → the gateway's wt0 agent listener on `:8081`): WebSocket
  works with NO change — no nginx is involved.
- **Public path** (agent → nginx → backend): the `location = /api/agent/v1/stream` block
  in `nginx/locations.conf` (included by BOTH `default.conf` and
  `default.no-netbird.conf` — see that file's own header comment) proxies the
  WebSocket upgrade. No other change is needed.
- **Graceful restart**: on SIGTERM the gateway closes each open agent WebSocket with
  `1001 GoingAway`; agents reconnect automatically within seconds (a clean close uses a
  short jittered delay; any error uses exponential backoff, base 500 ms, cap 30 s).
- **A TLS edge, not just the plain listener:** the same nginx now also terminates
  TLS on `:443` (the gateway's own edge certificate — see `README-certificates.md`
  §6), and that `:443` server block has `http2 on;`. A WebSocket agent that connects
  there over a REAL, verified TLS connection would negotiate HTTP/2 by default (Go's
  standard transport, `ForceAttemptHTTP2`), and `Upgrade`/`Connection` would never
  reach the backend — the ServerAgent's WebSocket client only forces HTTP/1.1 in its
  `tls_insecure` branch (`server-agent/internal/client/ws.go`). Nothing points an
  agent at `:443` today, so this is not an active problem — but whoever moves agent
  traffic onto this TLS edge later must either drop `http2 on;` for that path or force
  HTTP/1.1 on the agent side.
