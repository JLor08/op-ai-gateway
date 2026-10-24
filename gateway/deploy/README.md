# Deployment

Deployment assets for **OnPrem AI Gateway** (OP AI Gateway): container images,
Docker Compose and Kubernetes manifests, the nginx reverse proxy, the deployable
themes directory, and operator runbooks.

> Architecture context: [Deployment View](../../docs/architecture/07-deployment-view.md).
> Topic runbooks: [Certificates](README-certificates.md) · [NetBird mesh](README-netbird.md).

## Contents

| Path | Purpose |
|---|---|
| `Dockerfile.backend` | Distroless, CGO-free `op-ai-gateway` image; bakes `themes/` at `/themes`; `-healthcheck` subcommand for the container HEALTHCHECK. |
| `Dockerfile.frontend` | nginx image serving the built SPA under `/portal/` and reverse-proxying backend paths. |
| `Dockerfile.agent-builder` | One-shot cross-compiler producing downloadable `server-agent` binaries. |
| `Dockerfile.netbird` | NetBird client sidecar + entrypoint wrapper (runtime setup-key via a shared-volume file). |
| `docker-compose.yml` | `web` + `backend` + `db` (PostgreSQL) with the NetBird sidecar (backend shares its network namespace). |
| `docker-compose.no-netbird.yml` | The same stack **without** the NetBird sidecar (backend in its own namespace). |
| `k8s/` | Kustomize base (gateway pod + sidecar, web tier, PostgreSQL, services, Ingress, NetworkPolicies, PVCs, ConfigMaps). |
| `k8s-no-netbird/` | Kustomize overlay of `k8s/` that removes the NetBird sidecar. |
| `nginx/` | The reverse-proxy config baked into the frontend image (path-split). |
| `themes/` | Deployable, data-only portal themes (baked into the image; mountable at runtime). See [Theming](../../docs/architecture/cross-cutting/theming-and-i18n.md) and `themes/README.md`. |
| `.env.example` | Template for the Compose environment. |
| `README-certificates.md` | Certificate management runbook (edge + mesh TLS, agent proxy). |
| `README-netbird.md` | NetBird-only transport runbook. |

## Quick start (Docker Compose)

The Docker build context is `gateway/`, so run these from `gateway/deploy`:

```bash
cd gateway/deploy
cp .env.example .env
```

Set a high-entropy bootstrap token in `.env`:

```bash
openssl rand -hex 32   # put the result in OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN
```

Build and start:

```bash
docker compose up --build
```

Open `http://localhost:8080/` (redirects to `/portal/`).

**First-run bootstrap:** the bootstrap admin has no password yet — read the
one-time set-password link from the backend logs:

```bash
docker compose logs backend
# bootstrap admin has no password yet; set one at
# http://localhost:8080/portal/set-password?token=<token>
```

Open that link, set a password, and log in.

For the stack without NetBird:

```bash
docker compose -f docker-compose.no-netbird.yml up --build
```

## Persistence & migrations

The default stack persists to **PostgreSQL** (`db` service, `pgdata` volume). The
backend waits for the database to be healthy and **auto-applies pending schema
migrations on startup** (`OP_AI_GATEWAY_AUTO_MIGRATE=true`), so redeploying a newer
image upgrades the schema in place. To use SQLite instead, set
`OP_AI_GATEWAY_DB_DRIVER=sqlite` + `OP_AI_GATEWAY_SQLITE_PATH` in `.env`.

Chat transcripts and payload captures are only persisted when
`OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY` is set; otherwise they use a volatile
in-RAM store. All other data (users, tokens, servers, usage, settings) persists
regardless. See [Persistence](../../docs/architecture/cross-cutting/persistence.md).

## Reverse proxy, TLS & body size

- `nginx/` implements the path-split: `/portal/*` → SPA, backend paths → gateway,
  `/` → `/portal/`.
- The inference endpoints (`/v1/`, `/openai/`, `/anthropic/`) accept **unbounded**
  request bodies (large multimodal input); the bundled nginx sets
  `client_max_body_size 0` on those paths. A custom reverse proxy in front **must**
  do the same, or large requests are rejected. Control-plane `/api/*` keeps
  nginx's 1 MiB default, except `/api/portal/chats`: it carries whole chat
  documents and allows 5 MiB, the gateway's own chat request cap. A custom
  reverse proxy in front **must** allow at least that on `/api/portal/chats`
  too, or saving a large chat document (or sending a large image upload, which
  rides inline in the run-start body) is refused with an HTML 413.
- Live transcription adds two locations to the same file: a WebSocket upgrade for
  `/api/portal/transcription-live` (with `Host $http_host`, so the port survives the gateway's same-origin
  check; a custom reverse proxy must forward the original `Host` with its port) and an unbuffered, full-duplex one
  for the bearer `POST /v1/audio/transcriptions/live` and its `/openai/` alias (`proxy_request_buffering off`, no
  body cap, a long body timeout). A custom proxy in front must not buffer that request body either. On Kubernetes
  the ingress-nginx buffering is per Ingress, so a dedicated Ingress for those two paths is the follow-up (see
  `k8s/README.md`).
- Voice Chat adds one location to the same file: a WebSocket upgrade for `/api/portal/voice-chat`, the
  live-transcription one again (`Host $http_host`, a 3600 s idle limit), plus two requirements that are not
  location-level. Both server blocks set `large_client_header_buffers 4 16k`: the single-use ticket rides one
  `Sec-WebSocket-Protocol` header line of up to 7 KiB (it seals three routing targets), and nginx's default of 8k per
  line would leave under 1 KiB of margin. And the ticket mint, `POST /api/portal/voice-chat-ticket`, waits on the
  model co-load gate for up to `OP_AI_GATEWAY_VOICE_COLOAD_TIMEOUT` (120 s by default), so it is served by
  `location /api/` on the server-level `proxy_read_timeout 3600s`; nginx's own 60 s default would cut a cold start
  short. A custom reverse proxy in front **must** forward the original `Host` with its port, accept a header line of at
  least 8 KiB, and allow the ticket POST at least 120 s of read time. The edge proxy configuration the
  Certificates page generates carries the same location and header buffers.
- `OP_AI_GATEWAY_SESSION_COOKIE_SECURE=false` (in `.env.example`) is required for
  the plain-HTTP Compose stack; set it `true` once TLS terminates in front.
- Set `OP_AI_GATEWAY_PUBLIC_URL` to include the `/portal` segment so invite links
  resolve to the SPA route.
- Edge and mesh TLS are separate surfaces — see [Certificates](README-certificates.md)
  and [Certificates & TLS](../../docs/architecture/cross-cutting/certificates-tls.md).

## External themes

`themes/` is baked into the backend image at `/themes`
(`OP_AI_GATEWAY_THEMES_DIR`). To add or override operator/brand themes at runtime
without rebuilding, mount a host directory over it, e.g. in `docker-compose.yml`:

```yaml
    volumes:
      - ./themes:/themes:ro
```

Private theme directories placed under `themes/` are gitignored (never committed).
See `themes/README.md`.

## Downloadable server-agent binaries

A one-shot `agent-builder` service cross-compiles the agent for all supported
platforms into the `agent-bin` volume when `BUILD_AGENTS=true`:

```bash
docker compose build agent-builder && BUILD_AGENTS=true docker compose up -d
```

Leave `BUILD_AGENTS=false` for fast redeploys (the volume keeps the last build).
Operators then download a binary and the annotated `server-agent.json` from the
per-server area in the portal, or headless with the server's agent token
(`GET /api/agent/v1/download/<os>-<arch>` and `…/download/config`). See
[`server-agent/README.md`](../../server-agent/README.md).

## Deploy self-tests

Small shell self-tests validate the deploy assets (nginx config, compose env
rendering, entrypoints, agent build):

```bash
cd gateway/deploy
for t in *.test.sh; do bash "$t"; done
```
