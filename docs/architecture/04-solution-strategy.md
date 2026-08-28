# 4. Solution Strategy

The handful of strategic decisions that shape everything else. Each links to where
it is realized; the reasoning is captured as ADRs in
[09 Architecture Decisions](09-architecture-decisions.md).

## 4.1 A provider-neutral core with thin compatibility edges

Client APIs (OpenAI, Anthropic, Codex, Claude Code) are translated at the edge into
one **provider-neutral inference model**, and provider clients translate that model
into each backend's dialect (Ollama, OpenAI-compatible for vLLM/llama.cpp, a mock).
Compatibility mapping is isolated in one package so new flavors or backends do not
leak into routing or persistence.
→ [API Compatibility & Inference](cross-cutting/compatibility-and-inference.md).

## 4.2 Mapping-based, telemetry-scored routing

Routing is **data-driven**, not hard-coded. Operators register AI servers, their
applications (a running inference endpoint with a type/port/scheme), and
**model mappings** (gateway model name ↔ application model name). At request time
the resolver turns a public model name + API flavor into a set of candidate
`(server, application, mapping)` tuples and **scores** them by live telemetry
(load, latency, error rate, availability), priority/weight, and capacity, then
dispatches to `scheme://domain:port`.
→ [Routing & Model Selection](cross-cutting/routing-and-model-selection.md).

## 4.3 One query set across three stores

A single **dialect seam** keeps SQLite and PostgreSQL on the same SQL, plus an
in-memory driver for dev/tests. Schema evolves through **forward-only versioned
migrations** applied transactionally on startup. This lets a single node run on a
file and production run on PostgreSQL without divergent code paths.
→ [Persistence](cross-cutting/persistence.md).

## 4.4 A separate, portable reporting agent

Telemetry is collected by a **standalone agent** that imports nothing from the
gateway and ships as one CGO-free binary per OS. It authenticates with a
per-server token, pushes host/GPU/power/temperature/hardware data over HTTP or a
WebSocket, and can optionally terminate mesh TLS in front of the local AI server.
→ [Telemetry & Observability](cross-cutting/telemetry-usage-observability.md).

That same agent can also **manage the model server processes** on its host, which
is how the gateway replaced llama-swap with first-party machinery. The split of
authority is the strategic part: the gateway holds the launch specification and
decides *when* a model runs, while the AI server's own operator holds a binary
allowlist that decides *what may run at all* — and the agent, not the gateway,
enforces admission (a co-residency matrix, a process limit, per-GPU VRAM budgets)
before it starts anything. The gateway sees one ordinary application per server,
so routing, TLS and the mesh surface are untouched, and the whole capability is
negotiated by feature flag, so an agent without it behaves exactly as before.
→ [Agent-Managed Model Runtime](cross-cutting/agent-runtime-manager.md).

## 4.5 Two auth modes, layered authorization

Browsers authenticate with a **session cookie + CSRF header**; programs use
**bearer API tokens**. `/v1/chat/completions` additionally accepts the session
(the other inference endpoints are bearer-only). Authorization is layered:
roles `user < admin < system_admin`, plus delegated **admin groups**, **step-up**
for sensitive system-admin actions, and scoping by **projects / user-groups /
service accounts / resource-groups**. Secrets are never stored in plaintext.
→ [Security, Authentication & Authorization](cross-cutting/security-auth-rbac.md).

## 4.6 Mesh-first server links with gateway-managed mTLS

Gateway↔AI-server traffic can run over a private **NetBird** mesh. The gateway
manages NetBird peers and policies and issues/rotates **mTLS certificates** from an
internal CA for the mesh listener, independently of the public edge. The agent can
run a TLS-terminating reverse proxy so applications are reached over HTTPS, with an
automatic HTTP→HTTPS switch reconciled by the gateway.
→ [Networking & Mesh](cross-cutting/networking-mesh.md) ·
[Certificates & TLS](cross-cutting/certificates-tls.md).

## 4.7 A dense operational portal: themable and localized

The portal is a MUI-based SPA driven by a **CSS-variable theme bridge**. Themes are
two-tier: built-in code themes shipped in the repo, and **external data-only
themes** operators deploy via a directory without rebuilding — so brand themes need
not live in the source tree. The UI is bilingual (German/English) from the start.
→ [Theming & Internationalization](cross-cutting/theming-and-i18n.md).

## 4.8 Observability as a first-class concern

Every request produces a usage event with cost and energy attribution; hosts stream
live telemetry; method-level **OpenTelemetry** tracing and a live **log stream**
give operators insight without external tooling being mandatory.
→ [Telemetry, Usage Analytics & Observability](cross-cutting/telemetry-usage-observability.md).
