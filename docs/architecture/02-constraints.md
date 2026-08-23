# 2. Constraints

The boundary conditions the architecture must respect. These are load-bearing:
several architectural decisions exist specifically to satisfy them.

## 2.1 Technical constraints

| Constraint | Rationale / consequence |
|---|---|
| **Go backend** (gateway ≥1.25, server-agent ≥1.26), single static binary | The gateway and agent build as static, **CGO-free** binaries so they ship in a distroless image and cross-compile for Linux/Windows/macOS. This forbids CGo-only dependencies (e.g. the SQLite driver is the pure-Go `modernc.org/sqlite`). |
| **Node.js + npm** frontend (React 19, TypeScript, Vite, MUI) | The portal is a static SPA served under `/portal/`; no server-side rendering. |
| **Three persistence drivers** — memory, SQLite, PostgreSQL | All three are first-class and share one query set behind a `dialect` seam. Production target is PostgreSQL; SQLite is the single-node/local option; memory is for dev/tests. |
| **Distroless, non-root container** | No shell in the runtime image (health checks use a `-healthcheck` subcommand). Runs as an unprivileged user. |
| **Agent runs unprivileged where possible** | The agent collects the richest telemetry each platform allows without elevated rights, and degrades gracefully (e.g. some Linux power/sensor sources need root; the agent still runs without them). |
| **On-prem / no mandatory external services** | The gateway must run without the public internet. Optional integrations (public ACME for edge TLS, SMTP for invites, an OTLP tracing endpoint, a NetBird control plane) are opt-in. |
| **Provider-neutral internal model** | Compatibility adapters are isolated so new client flavors or backends can be added without leaking into routing or persistence. |

## 2.2 Organizational & legal constraints

| Constraint | Consequence |
|---|---|
| **License: AGPL-3.0-only** | Every source file carries an SPDX header; the running product surfaces the AGPL §13 "Appropriate Legal Notices" (portal footer + a `-license` flag on both binaries); a source-offer link is shown to network users. See [Licensing](cross-cutting/licensing.md). |
| **Dependency policy: AGPL-compatible only** | Permissive (MIT / BSD / Apache-2.0) **and** copyleft that is one-way compatible with AGPLv3 (AGPL-3.0, GPL-3.0-or-later, LGPL, MPL-2.0) are allowed. GPL-2.0-only without an "or later" clause, SSPL, BSL/source-available, and proprietary licenses are not. The *effective* license (including code a package bundles transitively) must be verified before adding a dependency. |
| **Attribution** | Third-party components are listed with their licenses in `THIRD-PARTY-NOTICES.md` (generated for backend, frontend, and agent). |
| **Localization from the start** | German and English ship together; translation keys are added in both languages at once. |

## 2.3 Security constraints (non-negotiable)

- **No plaintext secrets at rest.** Passwords are bcrypt-hashed; API-token secrets
  are stored hashed; decryptable secrets use the `enc:`/`plain:` scheme — sealed
  with a key, or held plaintext only in volatile memory, or rejected on disk when
  no key is present. DTOs expose only `*_set` flags, never secrets.
- **Prompts and responses are not persisted** except via the explicit, opt-in
  payload capture, which is either encrypted-at-rest or volatile-in-RAM and always
  redacts sensitive headers. See [Security](cross-cutting/security-auth-rbac.md).
- **Browser vs. program auth are separate and both supported.** Browsers use a
  server-side session cookie plus an `X-OP-CSRF` header on state-changing
  requests; programs use bearer API tokens. `/v1/chat/completions` also accepts
  the session; the other inference endpoints are bearer-only.
- **The public listener and the mesh (agent) listener are separate enforcement
  surfaces** with independent TLS and authorization.

## 2.4 Conventions

- Go module name `op-ai-gateway`; agent module `op-ai-server-agent`.
- Environment prefixes `OP_AI_GATEWAY_*` (gateway) and `OP_AGENT_*` (agent).
- Browser CSRF header `X-OP-CSRF`; run-as header `X-OP-Run-As-Token`.
- Schema changes are **forward-only, versioned migrations** — append, never edit a
  shipped migration. See [Persistence](cross-cutting/persistence.md).
