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
  do the same, or large requests are rejected. Control-plane `/api/*` keeps a
  1 MiB cap.
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
