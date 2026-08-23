# op-ai-gateway on Kubernetes — with NetBird-only transport (Phase 3)

A self-contained example manifest set that runs the gateway stack on Kubernetes as a
**NetBird data-plane peer**: the NetBird client runs as a **sidecar container in the
gateway Pod**, so the gateway process sees the `wt0` interface + the `100.x` NetBird IP
and binds its agent listener there. The peer identity is **persisted on a PVC** so the
NetBird IP survives pod restarts/reschedules. The public surface (portal, inference,
`/api`) is fronted by an Ingress; the agent + gateway→app plane runs only over NetBird.

This mirrors the Phase-2 docker-compose stack. Cross-reference the compose runbook
`../README-netbird.md` for the shared concepts (enrollment, the persistent-identity
rules, the System-Settings flow, the NetBird ACL, the sidecar-is-a-hard-dependency
caveat). The code-level design is in
`docs/superpowers/specs/2026-07-25-netbird-only-transport-design.md`.

## What's in here

| File | Object(s) |
| --- | --- |
| `kustomization.yaml` | ties the set together (`kubectl apply -k`), sets the namespace + shared labels + the image-override block |
| `namespace.yaml` | `Namespace op-ai-gateway` |
| `configmap.yaml` | `ConfigMap op-gateway-config` — non-secret `OP_AI_GATEWAY_*` env + `NB_MANAGEMENT_URL`/`NB_HOSTNAME` |
| `secret.example.yaml` | **template** `Secret op-gateway-secrets` (CHANGE-ME placeholders) — **NOT** in the kustomization; apply separately |
| `nginx-configmap.yaml` | `ConfigMap op-gateway-nginx` — the nginx SPA+proxy config (static upstream `op-gateway-backend:8080`) |
| `pvc.yaml` | `op-gateway-netbird-state` (→ `/var/lib/netbird`) + `op-gateway-data` (→ `/data`) + `op-gateway-agent-bin` (→ `/agents`, the cross-compiled server-agent download binaries), all RWO |
| `gateway.yaml` | `Deployment op-gateway` — custom `netbird` sidecar + `backend` in one pod, `replicas:1` + `strategy: Recreate`, shared in-memory `netbird-key` emptyDir for the self-enroll key handoff; a one-shot `agent-builder` initContainer cross-compiles the downloadable server-agent binaries into `op-gateway-agent-bin` when `BUILD_AGENTS=true` (see §9) |
| `backend-service.yaml` | `Service op-gateway-backend` (ClusterIP `:8080` only) |
| `web.yaml` | `Deployment op-gateway-web` (nginx SPA) + `Service op-gateway-web` (`:80`) |
| `postgres.yaml` | `StatefulSet op-gateway-db` (postgres:17-alpine) + headless `Service op-gateway-db` (`:5432`) |
| `ingress.yaml` | `Ingress` → `op-gateway-web:80`, placeholder host + TLS |
| `networkpolicy.yaml` | default-deny to `op-gateway`/`op-gateway-db` on eth0 (defense-in-depth) |

## 0. Prerequisites

- A Kubernetes cluster where **at least one node exposes `/dev/net/tun`** (WireGuard). The
  gateway pod mounts it via a `hostPath` — so the node must have it and your PodSecurity
  policy must allow a `hostPath` + the `NET_ADMIN`/`SYS_ADMIN`/`SYS_RESOURCE` capabilities
  (or `privileged: true` — see the netbird container comment in `gateway.yaml`).
- **The `op-ai-gateway` namespace is labeled `PodSecurity=privileged`** (in
  `namespace.yaml`) — this stack needs `NET_ADMIN` + a `/dev/net/tun` hostPath for
  WireGuard, which the `baseline`/`restricted` PodSecurity profiles reject. This applies to
  BOTH the capabilities form AND the `privileged: true` fallback — both require the
  `privileged` PSA level; the fallback does not lower the bar.
- A default **StorageClass** (or edit the `storageClassName` in `pvc.yaml` + the
  `volumeClaimTemplates` in `postgres.yaml`). The `op-gateway-netbird-state` and
  `op-gateway-data` volumes are **ReadWriteOnce**.
- An **Ingress controller** (the example annotations assume ingress-nginx; adjust
  `ingressClassName` + annotations for your controller) and a way to obtain a **TLS cert**
  (a `Secret op-gateway-tls`, or cert-manager — uncomment the annotation in `ingress.yaml`).
- The backend + frontend **images built and pushed to a registry you control**
  (`../Dockerfile.backend` / `../Dockerfile.frontend`), then set the real registry (below).
- A **NetBird deployment you administer** (self-hosted management URL or the NetBird cloud)
  + an admin API token. A CNI that enforces NetworkPolicy (Calico/Cilium) if you want the
  `networkpolicy.yaml` rules to bite (otherwise they are a harmless no-op).

## 1. Set the image registry

The manifests use placeholders `REGISTRY/op-ai-gateway-backend:latest` and
`REGISTRY/op-ai-gateway-frontend:latest`. Point them at your registry via the
kustomization `images:` block — either edit `kustomization.yaml` or:

```
cd gateway/deploy/k8s
kustomize edit set image \
  REGISTRY/op-ai-gateway-backend=registry.example.com/op-ai-gateway-backend:v1.2.3 \
  REGISTRY/op-ai-gateway-frontend=registry.example.com/op-ai-gateway-frontend:v1.2.3
```

## 2. Prepare NetBird (once)

Same as compose (see `../README-netbird.md` §1):

1. Create a **group** for the gateway peer, e.g. `op-gw-portal`.
2. Create a **setup key** with `op-gw-portal` in its auto-groups (one-off is fine — the
   sidecar persists its identity on the PVC).
3. Note your **management URL** + **admin API token** (for the System-Settings NetBird
   module — separate from the sidecar's `NB_SETUP_KEY`).

## 3. Create the secrets (applied separately — NOT via kustomize)

`secret.example.yaml` is a **template** and is deliberately excluded from
`kustomization.yaml` `resources:`, so `kubectl apply -k` never ships placeholder values.

```
cp secret.example.yaml secret.yaml          # keep secret.yaml out of git
# edit secret.yaml — replace every CHANGE-ME-* :
#   NB_SETUP_KEY                          = the setup key from step 2
#   POSTGRES_PASSWORD                     = a strong password
#   OP_AI_GATEWAY_POSTGRES_DSN            = ...:<that same password>@op-gateway-db:5432/gateway?sslmode=disable
#   OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN     = openssl rand -hex 32
#   OP_AI_GATEWAY_DEV_AGENT_TOKEN         = openssl rand -hex 32
#   OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY  = openssl rand -hex 32
kubectl apply -f namespace.yaml             # namespace must exist first
kubectl apply -f secret.yaml
```

`OP_AI_GATEWAY_CERT_ENCRYPTION_KEY` is commented out in the template and only needed for
the certificate module. It seals every certificate private key (leaf keys, the ACME account
key, the internal CA key) and has **no** fallback to the capture key, so use a real
`openssl rand -hex 32` value **different** from the capture key. Either give it a real
64-character hex value or leave the line out entirely — the cipher is built
unconditionally at startup, so a leftover placeholder (or any other non-hex value) is a
fatal misconfiguration and **the gateway refuses to start**, even if certificates are never
enabled. Omitting it is safe: the gateway starts, and if the module is switched on later the
portal's certificate view names this variable as the reason nothing is issued.

Also provide the **TLS** `Secret op-gateway-tls` referenced by the Ingress (or let
cert-manager create it). Set `NB_MANAGEMENT_URL` in `configmap.yaml` and the Ingress host
+ `OP_AI_GATEWAY_PUBLIC_URL` to your real domain.

**A THIRD Secret is required before the first apply: `op-gateway-edge-tls`.** This is the
gateway's OWN edge certificate (see `../README-certificates.md` §6) — `web.yaml` mounts it
into the `op-gateway-web` Deployment **`readOnly: true`**, and `nginx-configmap.yaml`'s
`:443` server block unconditionally references it (`ssl_certificate`/
`ssl_certificate_key` pointing at `/etc/nginx/certs/edge-{fullchain,key}.pem`) — so a
missing Secret is an nginx **load** error, not a per-request one.

On an **upgrade** that non-optional mount is the SAFE direction: the still-running old
pod keeps serving while the new one waits. On a **first install there is no old pod** —
`kubectl -n op-ai-gateway get pods -w` will show `op-gateway-web` stuck in
`ContainerCreating` forever if this Secret does not exist yet, because in Kubernetes
(unlike the bundled docker-compose, whose `certs` volume is writable so nginx's own
entrypoint can write a throwaway bootstrap pair — see `../nginx-cert-entrypoint.sh`) the
Secret mount is **read-only**, so there is no bootstrap fallback here at all.

Create it with a throwaway self-signed placeholder BEFORE the first `apply -k` (a
verifying upstream proxy will reject this placeholder, exactly like the compose
entrypoint's own bootstrap pair — that is by design, not a bug):

```
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
  -keyout edge-key.pem -out edge-fullchain.pem \
  -subj "/CN=op-gateway placeholder - not trusted"
kubectl -n op-ai-gateway create secret generic op-gateway-edge-tls \
  --from-file=edge-fullchain.pem=edge-fullchain.pem \
  --from-file=edge-key.pem=edge-key.pem
rm -f edge-key.pem edge-fullchain.pem   # do not leave the throwaway key on disk
```

Once the gateway is up, the certificate module is enabled, and its edge mode has issued a
real certificate, replace this placeholder with the real material downloaded from the
portal (Zertifikate → Gateway nginx → "Zertifikatskette herunterladen" +
"Privaten Schlüssel herunterladen" — the key download only appears there in the first
place because this pod topology cannot deliver it any other way, see §6 of the runbook
above):

```
kubectl -n op-ai-gateway create secret generic op-gateway-edge-tls \
  --from-file=edge-fullchain.pem=<portal bundle download> \
  --from-file=edge-key.pem=<portal key download> \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n op-ai-gateway rollout restart deploy/op-gateway-web
```

Rotate it the same way whenever the edge certificate is reissued/re-keyed (this Secret is
never updated automatically — nothing in the gateway can reach it from inside the
`op-gateway` pod).

## 4. Apply the stack

```
kubectl apply -k gateway/deploy/k8s
kubectl -n op-ai-gateway get pods -w
```

The `netbird` sidecar registers using `NB_SETUP_KEY` and appears in your NetBird
dashboard as `op-gateway` (in `op-gw-portal`). The `backend` shares the pod netns, so once
`wt0` is up it can bind the agent listener there. `op-gateway-web` (nginx) fronts the
public API through the Ingress.

> **Shared network namespace is automatic in a Pod** — there is no compose-style
> `network_mode` here. Both containers in the `op-gateway` pod share one netns, so the
> sidecar's `wt0`/`100.x` IP is directly usable by the backend.

## 5. Persistence — the critical part

`op-gateway-netbird-state` (RWO PVC, mounted at `/var/lib/netbird` on the `netbird`
container) holds the WireGuard key + peer identity. It **must** survive pod
restart/reschedule so the peer keeps the **same NetBird IP**:

- The `op-gateway` Deployment is `replicas: 1` + `strategy: Recreate` **on purpose**: a
  single-instance identity on an RWO volume must never have two pods contending for it. A
  rolling update would try to start a second pod on the same RWO PVC and wedge — `Recreate`
  tears the old pod down first.
- **Never delete the `op-gateway-netbird-state` PVC.** Without it, every recreate
  re-enrolls as a **new** peer (new IP), which breaks the gateway-peer selection + the
  agent-listener bind and litters the tracking group with dead peers. Back it up like any
  stateful volume (`kubectl cp` from the pod, or a `VolumeSnapshot`).
- `NB_SETUP_KEY` is consumed only on the **first** enrollment (empty volume); with a
  persisted identity it is ignored, so leaving it set is idempotent.

**StatefulSet is the idiomatic alternative** to `Deployment + Recreate` for this
single-instance-with-identity pattern (stable pod name + its own `volumeClaimTemplates`,
never two replicas on the RWO volume). The Deployment+Recreate form is used here to keep
the sidecar pod spec in one obvious place; converting `op-gateway` to a 1-replica
StatefulSet (governed by a headless service, PVCs via `volumeClaimTemplates`) is a valid
swap with the same persistence guarantees.

## 6. Point the gateway at its own peer + turn on the restriction

In the portal (System Settings, system-admin) — same flow as compose:

1. **NetBird module:** set the admin API URL + token (the gateway's admin-API client —
   separate from the sidecar's `NB_*`).
2. **Gateway peer:** select the `op-gateway` peer in the gateway-peer picker (stores
   `netbird_gateway_peer_id`). On the next backend start the gateway resolves that peer's
   IP and binds the agent listener to `100.x:8081`.
3. **Restart the backend pod** so the agent listener binds:
   ```
   kubectl -n op-ai-gateway rollout restart deploy/op-gateway
   ```
4. **Verify:** `GET /api/system/netbird/status` (or the System-Settings status note) shows
   `agent_listener_active: true` with the `100.x:8081` address. If `false`, the peer wasn't
   resolvable/local at boot (sidecar not connected yet / wrong peer / admin API
   unreachable) — the gateway degrades to the single public listener and logs a `Warn`; fix
   the cause and `rollout restart deploy/op-gateway` again.
5. **Point your ServerAgents** at the gateway's NetBird address —
   `http://<op-gateway 100.x IP or NetBird DNS>:8081` for `/api/agent/v1/telemetry`.
   **Plain `http`, NOT https** — the agent listener terminates no TLS;
   confidentiality/integrity come from the WireGuard tunnel. (TLS is only terminated at the
   Ingress on the public plane, which the agent plane bypasses.)
6. **Flip `netbird_only` ON.** The public listener then rejects `/api/agent` with `403
   netbird.only` and off-mesh AI-servers drop from routing. (Fail-safe: with no agent
   listener active, turning it on does NOT cut off agents on the public path — the status
   note warns you.)

## 7. Why 8081 is not a Service, and how agents reach it

The `op-gateway-backend` Service exposes **only 8080** (the management listener that nginx
proxies to). The agent listener (**8081**) binds the `wt0` NetBird IP and is deliberately
**not** in any Service and **not** a declared `containerPort` — putting it in a Service
would place the agent plane on the cluster network, defeating the NetBird-only boundary.
Agents reach it directly over the mesh at the pod's NetBird IP.

## 8. NetBird ACL + the NetworkPolicy scope limit

- **NetBird ACL (the real boundary for the agent plane):** allow the AI-server peer groups
  → `op-gw-portal` on TCP `8081`, and `op-gw-portal` → the AI-server groups on the app
  ports; deny everything else. See `../README-netbird.md` §6.
- **K8s NetworkPolicy cannot govern the agent plane.** `networkpolicy.yaml` hardens the
  pod's **cluster interface (eth0)** — default-deny ingress to `op-gateway` except from
  `op-gateway-web` → 8080, and to `op-gateway-db` except from `op-gateway` → 5432. The
  agent listener is on `wt0`, **outside the cluster CNI**, so no NetworkPolicy can restrict
  it — that isolation comes from the wt0-only socket bind + the NetBird ACL + (optionally) a
  host firewall dropping `8081` on every interface but `wt0`.

## 9. Updates

```
kustomize edit set image REGISTRY/op-ai-gateway-backend=...:vNEXT   # (or edit kustomization.yaml)
kubectl apply -k gateway/deploy/k8s
#   or, for a quick image bump:
# kubectl -n op-ai-gateway set image deploy/op-gateway backend=...:vNEXT
```

`Recreate` cycles the single pod; the `op-gateway-netbird-state` PVC persists, so the
sidecar keeps the **same peer/IP** and the agent listener rebinds the same address — **no
re-selection needed**. The backend auto-migrates the DB schema on start
(`OP_AI_GATEWAY_AUTO_MIGRATE=true`). If `agent_listener_active` is `false` after an update
(a boot race where the backend started before `wt0` was up), just
`kubectl -n op-ai-gateway rollout restart deploy/op-gateway`.

**Downloadable agent binaries.** The `agent-builder` initContainer rebuilds the
`server-agent` download binaries only when `BUILD_AGENTS: "true"` in the ConfigMap;
otherwise it is an instant no-op and the `op-gateway-agent-bin` PVC keeps the last
build across restarts. Set it to `"true"` (and bump the `op-ai-gateway-agent-builder`
image if the agent source changed) only when you want a fresh build, then
`kubectl apply -k …` — the pod re-runs the init build once. A first apply with
`BUILD_AGENTS` unset/`"false"` leaves the PVC empty until you build once. Operators
download a binary from the per-server **Server-Reporting-Agent** area in the portal,
or via `curl` with the server's agent token (`GET /api/agent/v1/download/<os>-<arch>`).
The binaries are world-readable (0755) so the nonroot backend serves them; if a
storage provisioner mounts the PVC non-world-readable, add pod
`securityContext.fsGroup: 65532`.

## 10. Caveats

- **Single-replica.** The RWO NetBird-state PVC + the single NetBird identity mean the
  gateway pod is `replicas: 1`. Do not scale it up (two peers, split identity). HA would
  need a different NetBird topology, out of scope here.
- **`/dev/net/tun` + capabilities.** The netbird container needs
  `NET_ADMIN`/`SYS_ADMIN`/`SYS_RESOURCE` **or** `privileged: true`, plus the node's
  `/dev/net/tun`. On a locked-down PodSecurity `restricted` namespace this pod will be
  rejected — run it in a namespace/policy that permits the caps + hostPath (or the
  privileged fallback).
- **NetBird runs as a native sidecar (requires Kubernetes ≥1.29).** In `gateway.yaml` the
  `netbird` container is an `initContainer` with `restartPolicy: Always` — a *native
  sidecar* (GA since k8s 1.29): it starts before `backend` and shuts down after it. On an
  **older cluster** without native-sidecar support, move the `netbird` entry back into
  `containers:` as a regular container (and drop its `restartPolicy: Always`) — it still
  works, at the cost of the boot-race below.
- **What a NetBird crash actually breaks (and what it doesn't).** The pod's network
  namespace is owned by the pod **sandbox (pause) container**, not by `netbird` — so a lone
  `netbird` crash does NOT tear down the netns or the backend. The kubelet restarts *only*
  the `netbird` container (native-sidecar `restartPolicy: Always`); `backend` keeps running
  and keeps its `:8080` management socket. What a crash disrupts is **`wt0`**: while NetBird
  re-establishes the tunnel the mesh is briefly down, and because the agent listener is
  bound to the `100.x` NetBird IP, it may need to **rebind** — do
  `kubectl -n op-ai-gateway rollout restart deploy/op-gateway` if `agent_listener_active`
  does not come back on its own. (An optional `netbird status` startupProbe — see the
  `gateway.yaml` comment — makes the native-sidecar restart gate readiness and re-establish
  wt0 before the backend is considered ready. **Do NOT enable that startupProbe with
  autonomous (Phase B) enrollment — it deadlocks; see §11.**) The public API (via the Ingress → nginx →
  `op-gateway-backend:8080`) is unaffected by a NetBird blip; it is only down when the whole
  pod is down.
- **Relay / host-netns caveat is n/a in-pod.** The compose stack notes a possible
  relayed-connection fallback when NetBird runs in a non-host netns; in a Pod the sidecar +
  backend share the pod netns and NetBird operates normally within it. If your environment
  needs host networking for direct connectivity, that is a cluster-specific change (hostNetwork
  pod) beyond this example.
- **Boot race, fail-safe.** As a native sidecar `netbird` *starts* before `backend`, but
  starting is not the same as `wt0` being up — without a readiness gate the backend can
  still bind before the tunnel has an IP, in which case the Phase-1 agent listener fails
  **safe** to the single public listener. Recover with
  `kubectl -n op-ai-gateway rollout restart deploy/op-gateway`. To eliminate the race
  entirely, add the optional `netbird status` **startupProbe** to the sidecar (see the
  `gateway.yaml` comment) so `backend` waits for mesh-readiness. On a pre-1.29 cluster where
  `netbird` is a plain `containers` entry, both containers simply start together and the
  same fail-safe + `rollout restart` applies. **Caveat: do NOT add that startupProbe when
  using autonomous (Phase B) sidecar enrollment (§11) — it deadlocks. The Phase-B wrapper
  blocks a fresh peer waiting for the gateway to write the minted key, but the gateway only
  writes it after `backend` starts, and the startupProbe holds `backend` until the sidecar
  is mesh-ready first → the sidecar never connects → the key is never written. The probe is
  only for the operator-provided `NB_SETUP_KEY` path.**

## 10a. ICMP ping action (optional)

The System-Settings **"Ping ausführen"** action runs a real but **unprivileged** ICMP echo from
the gateway to a selected server's address — it uses a datagram ICMP socket, **not** a raw socket,
so it needs **no `CAP_NET_RAW`** and does not weaken the distroless/nonroot posture. It relies on
the kernel sysctl `net.ipv4.ping_group_range` including the container GID (65532). **Docker sets
this by default** (`0 2147483647`), so compose works out of the box. On **Kubernetes** it is
usually NOT set for the pod, so add a pod-level sysctl to the gateway Deployment's
`spec.template.spec.securityContext`:

```yaml
      securityContext:
        sysctls:
          - name: net.ipv4.ping_group_range
            value: "0 2147483647"
```

`net.ipv4.ping_group_range` is an **unsafe** sysctl, so the kubelet must allow it
(`--allowed-unsafe-sysctls=net.ipv4.ping_group_range`) or it must be a node default. If the sysctl
does not permit the GID, the ping action returns a clear error (`icmp unavailable …`) and
**nothing else is affected** — the NetBird ping-allow policies and the rest of the gateway work
regardless. Do **not** add `CAP_NET_RAW`.

## 11. Autonomous sidecar self-enroll (Phase B)

Sections 2–5 use an **operator-provided** setup key (the `NB_SETUP_KEY` Secret, the Phase-3 style).
Phase B is the **autonomous alternative**: the gateway **mints** its own NetBird setup key from the
portal and hands it to the sidecar over a shared volume — **no dashboard step, no copy-paste**.

**How it works.** The `netbird` sidecar runs the **custom image** built from `../Dockerfile.netbird`
(the stock NetBird client + a small entrypoint wrapper) — set it via the kustomization `images:`
block (placeholder `REGISTRY/op-ai-gateway-netbird`, see §1). On a **fresh** peer, when `NB_SETUP_KEY`
is empty and `NB_ENROLL_KEY_FILE` is set, the wrapper **waits** for the gateway to drop a key file,
then loads it into `NB_SETUP_KEY` and runs the stock `netbird up`.

> **Why a separate `NB_ENROLL_KEY_FILE` and not netbird's own `NB_SETUP_KEY_FILE`?** netbird treats
> `--setup-key` (`NB_SETUP_KEY`) and `--setup-key-file` (`NB_SETUP_KEY_FILE`) as **mutually exclusive**
> and aborts **every** command if *both* env vars exist — even `NB_SETUP_KEY=""`. Because the backend
> and sidecar **share the Pod network namespace**, a crashing `netbird up` takes the netns down and the
> **entire public API (including `/api/auth/login`) returns 502**. So the wrapper reads the path from
> its own `NB_ENROLL_KEY_FILE`, hands netbird **only** `NB_SETUP_KEY`, and strips any
> `NB_SETUP_KEY_FILE`/empty `NB_SETUP_KEY` before `exec`.

The backend (`OP_AI_GATEWAY_NETBIRD_KEY_FILE` from the ConfigMap) and the sidecar (`NB_ENROLL_KEY_FILE`
in `gateway.yaml`) point at the **same file** on a **shared, TRANSIENT volume** — a pod `emptyDir`
with `medium: Memory` (`netbird-key` mounted at `/shared` in **both** containers). The Pod shares a
netns but not a filesystem, so the key needs a shared volume, and `medium: Memory` keeps the minted
key **off node disk**. Both default to `/shared/netbird-setup-key`.

**Steps.**

1. Build + push the custom sidecar image (`../Dockerfile.netbird`) and point the kustomization at it
   (`kustomize edit set image REGISTRY/op-ai-gateway-netbird=registry.example.com/op-ai-gateway-netbird:vX`).
2. Leave `NB_SETUP_KEY` **empty** in the Secret (so the wrapper takes the autonomous path). The
   ConfigMap ships `OP_AI_GATEWAY_NETBIRD_KEY_FILE=/shared/netbird-setup-key`; `gateway.yaml` ships the
   matching `NB_ENROLL_KEY_FILE`.
3. `kubectl apply -k gateway/deploy/k8s`. The fresh sidecar logs
   `netbird-enroll: not yet enrolled; waiting for the setup key ...` and blocks.
4. In System Settings, configure the **NetBird module** (admin API URL + token) as in §6, then click
   **"Sidecar enrollen"**. The gateway mints a one-off setup key (group `op-gw-portal`), writes it to
   `/shared/netbird-setup-key`, and returns it once (a fallback copy you can also paste manually).
5. The waiting sidecar reads the key and `netbird up`s → the `op-gateway` peer self-enrolls. Continue
   with §6 (select the gateway peer, `rollout restart deploy/op-gateway`, verify
   `agent_listener_active`, flip `netbird_only`).

**Notes.**

- **Marker on the persistent identity.** The wrapper touches `/var/lib/netbird/.gw-enroll-attempted`
  (on the `op-gateway-netbird-state` PVC) after taking an enrollment path, so a later
  restart/reschedule (when the transient `/shared` emptyDir is empty again) **skips the wait** and
  reconnects via the persisted identity. **Deleting the `op-gateway-netbird-state` PVC** (which also
  wipes the identity) removes the marker and correctly forces a fresh wait + re-enroll.
- **Updating the client.** Bump the base tag in `../Dockerfile.netbird` (`ARG NETBIRD_VERSION`),
  rebuild + push, and bump the image tag (`kustomize edit set image ...` + `apply -k`).
- **`NB_SETUP_KEY` disables the autonomous path.** If the `NB_SETUP_KEY` Secret is non-empty, the
  wrapper skips the wait and enrolls with it the old Phase-3 way. The **Phase A** manual button
  ("Gateway-Setup-Key erstellen", mint + display for manual paste) also still works — Phase B just
  automates the handoff.
- **A bad key** leaves the marker set, so a later restart won't re-wait; retry = delete the PVC (wipes
  the marker + identity) and re-enroll. The gateway mints a valid key, so this is a corner case.

## ServerAgent WebSocket transport

The agent plane is mesh-only here: port 8081 is deliberately off the Service/Ingress,
so agents reach the gateway's wt0 agent listener directly over the NetBird mesh. In
that (default) topology WebSocket transport (`OP_AGENT_TRANSPORT=websocket`) needs NO
manifest change.

If you deliberately expose the agent path through the public Ingress instead: nothing
needs adding to `nginx-configmap.yaml` — its `default.conf` key already carries its own
copy of the shared location list (mirroring `../nginx/locations.conf`, mounted alongside
it as the ConfigMap's `locations.conf` key), which already includes the
`location = /api/agent/v1/stream` upgrade block, and `ingress.yaml`'s catch-all `path: /`
rule already forwards that path to `op-gateway-web` (the nginx-ingress controller has
WebSocket support on by default). The one thing that actually gates it is the app-layer
`netbird_only` setting, which 403s the agent route on the public listener while it is on
— see `configmap.yaml`/README.md above and the design spec.
