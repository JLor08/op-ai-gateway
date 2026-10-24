# 7. Deployment View

How OnPrem AI Gateway is packaged and run. Configuration details are in the
[Configuration reference](reference/config-env.md).

## 7.1 Images

Built from the `gateway/` build context (`gateway/deploy/`):

| Image | Base | Contents |
|---|---|---|
| **backend** (`Dockerfile.backend`) | `gcr.io/distroless/static-debian12:nonroot` | The static, CGO-free `op-ai-gateway` binary; a `/data` volume; the baked `deploy/themes/` at `/themes`; a `-healthcheck` subcommand for the container HEALTHCHECK (no shell in distroless). |
| **frontend** (`Dockerfile.frontend`) | nginx | The built SPA served under `/portal/`, reverse-proxying backend paths (path-split mirrors the Vite dev/preview proxy: `/portal/*` → SPA, backend paths → gateway, `/` → `/portal/`). |
| **agent-builder** (`Dockerfile.agent-builder`) | golang | One-shot cross-compiler that produces the `server-agent` binaries offered for download. |
| **netbird sidecar** (`Dockerfile.netbird`) | NetBird client | The stock client plus a small entrypoint wrapper that accepts a setup key at runtime via a shared-volume file. |

## 7.2 Docker Compose

```mermaid
flowchart TB
    subgraph host["Docker host"]
        web["web (nginx + SPA)"]
        backend["backend (op-ai-gateway)"]
        db[("db: postgres:17-alpine")]
        nb["netbird sidecar (optional)"]
        gwdata[("gwdata volume")]
        certs[("certs volume")]
    end
    users["Users / clients"] --> web --> backend
    backend --> db
    backend --- gwdata
    backend --- certs
    backend -. shares netns .- nb
```

- `docker-compose.yml` — `web` + `backend` + `db` (PostgreSQL), with the backend
  sharing the NetBird sidecar's network namespace for the mesh. The backend waits
  for `db` to be healthy and auto-migrates the schema on startup.
- `docker-compose.no-netbird.yml` — the same stack without the NetBird sidecar;
  the backend runs in its own network namespace and nginx proxies straight to it.
- Volumes: `gwdata` (state), `certs` (edge certificate delivery, shared with the
  web tier for bootstrap), plus agent-binary and PostgreSQL data volumes.
- External themes: `deploy/themes/` is baked into the image at `/themes`; a host
  directory can be mounted over it (`- ./themes:/themes:ro`) to add or override
  operator themes without rebuilding. See
  [Theming](cross-cutting/theming-and-i18n.md).

## 7.3 Kubernetes

- `gateway/deploy/k8s/` — a Kustomize base: the gateway pod (NetBird sidecar +
  gateway backend in one pod), a public `web` tier, PostgreSQL, ClusterIP
  services, an Ingress fronting the public surface only, NetworkPolicies, PVCs, and
  ConfigMaps/Secrets.
- `gateway/deploy/k8s-no-netbird/` — a Kustomize overlay (`resources: ../k8s`) that
  removes the NetBird sidecar (no `NET_ADMIN`/`SYS_ADMIN`, no `/dev/net/tun`) and
  patches the config accordingly.
- The public Ingress exposes only the public/edge surface; the portal/admin and
  agent/mesh surfaces are separate.
- **Live transcription needs two nginx locations, and one Ingress caveat.** The bundled nginx (the compose
  files' `nginx/locations.conf`, mirrored in the k8s `nginx-configmap.yaml`) carries a WebSocket-upgrade location for
  the portal's `/api/portal/transcription-live` (its `Host` is `$http_host` so the port survives the gateway's same-origin
  check, [Networking & Mesh §10](cross-cutting/networking-mesh.md#10-agentgateway-websocket-transport)) and a full-duplex, unbuffered location for the bearer `POST /v1/audio/transcriptions/live` and its
  `/openai/` alias (`proxy_request_buffering off`; a body timeout as long as the stream). The portal WebSocket works
  through the Ingress, which already upgrades WebSockets. ingress-nginx, however, **buffers request bodies per Ingress,
  not per path**, so the shipped `ingress.yaml` does **not** stream the bearer endpoint full-duplex: a live PCM
  stream would be held back until it ended. Setting `nginx.ingress.kubernetes.io/proxy-request-buffering: "off"` on the
  shared Ingress would unbuffer every upload through the host, so the follow-up is a **dedicated Ingress for those two
  paths** carrying the annotation (`gateway/deploy/k8s/README.md` has the note; [ADR-073](09-architecture-decisions.md#adr-073--portal-live-transcription-loops-back-through-the-public-v1audiotranscriptionslive-one-canonical-relay-that-resolves-admits-guards-and-bills-once), [Risks & Technical Debt §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
- **Voice Chat adds one more nginx location and two requirements that are not location-level.** The same bundled nginx
  files carry a WebSocket-upgrade location for the portal's `/api/portal/voice-chat`, the live-transcription location
  again (`Host $http_host`, a 3600 s idle limit); it works through the Ingress for the same reason. The two
  requirements are server-level. **`large_client_header_buffers 4 16k`** is set in both server blocks, because the
  single-use ticket rides one `Sec-WebSocket-Protocol` header line of up to 7 KiB (it seals three routing targets, where
  the live-transcription ticket seals one) and nginx's 8k default would leave under 1 KiB of margin. A **read timeout of
  at least `OP_AI_GATEWAY_VOICE_COLOAD_TIMEOUT`** (120 s by default) covers the ticket mint `POST
  /api/portal/voice-chat-ticket`, which waits on the model co-load gate: it has no location of its own and rides
  `location /api/` on the server-level `proxy_read_timeout 3600s` (the Ingress's is 3600 s too). ingress-nginx's own
  header buffer is a controller-global ConfigMap key, not an annotation (`gateway/deploy/k8s/README.md`).
  `gateway/deploy/nginx-config.test.sh` pins all three ([ADR-078](09-architecture-decisions.md#adr-078--voice-chat-one-orchestration-websocket-composes-the-three-v1-cores-as-a-server-side-state-machine-with-a-pause-tolerant-continuation-window-a-deferred-commit-and-a-playout-aware-half-duplex-gate), [Networking & Mesh §10](cross-cutting/networking-mesh.md#10-agentgateway-websocket-transport)).

## 7.4 Listeners

The gateway runs distinct listeners with independent enforcement:

| Listener | Purpose | Auth / TLS |
|---|---|---|
| **Public** (`OP_AI_GATEWAY_ADDR`) | Inference APIs, portal/system APIs, SPA path-split target | Bearer / session+CSRF; **edge TLS** (public ACME) |
| **Agent / mesh** (`OP_AI_GATEWAY_AGENT_ADDR` / optional separate `AGENT_TLS_ADDR`) | `/api/agent/v1/*` telemetry, cert, proxy-routes, stream | Per-server **agent token**; **mesh mTLS** from the internal CA |
| **Health** | `/healthz` and the `-healthcheck` subcommand | none |

The agent endpoints are registered on both the public mux and the agent/mesh mux;
per-listener behavior (mesh observation, mesh-only enforcement) is threaded through
request context. See [Certificates & TLS](cross-cutting/certificates-tls.md).

## 7.5 Server-Agent deployment

The agent is a single binary per OS (Linux/Windows/macOS), downloadable from the
gateway (`/api/portal/agent-binaries`, served from the `agent-builder` output). It
is configured with `OP_AGENT_GATEWAY_URL` and a per-server `OP_AGENT_TOKEN`
(minted/rotated in the portal), plus optional certificate and metrics settings. It
runs unprivileged where possible, collecting the richest telemetry each platform
allows. See [Telemetry](cross-cutting/telemetry-usage-observability.md).

## 7.6 Startup & background work

On startup the gateway loads configuration (env-first), opens the store, applies
pending migrations (when `OP_AI_GATEWAY_AUTO_MIGRATE` is on), seeds the bootstrap
admin/token if configured, loads external themes, and starts background
reconcilers: certificate renewal/reconcile, NetBird token auto-rotation, energy
reconciliation, availability/telemetry retention pruning, and (opportunistic)
benchmark scheduling.
