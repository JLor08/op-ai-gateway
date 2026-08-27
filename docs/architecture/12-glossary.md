# 12. Glossary

Domain and technical terms used throughout this documentation.

| Term | Definition |
|---|---|
| **OnPrem AI Gateway (OP AI Gateway)** | This product: a self-hostable AI gateway and management portal. |
| **Gateway** | The Go backend service (`op-ai-gateway`) that terminates client APIs, routes/dispatches inference, and serves portal/system/agent APIs. |
| **Portal** | The React/TypeScript SPA for administration and the chat playground, served under `/portal/`. |
| **Server-Agent** | The standalone Go binary (`op-ai-server-agent`) installed next to an AI server that reports telemetry, can terminate mesh TLS, and can manage the local model server processes. |
| **AI server** | A host running an inference engine (Ollama, llama.cpp, or vLLM) that the gateway dispatches to. |
| **Application** | A running inference endpoint on an AI server (type, port, scheme, API flavors, tuning fields). One AI server can have several. |
| **Model mapping** | The link between a public gateway model name and an application's model name; routing operates over **active** mappings. |
| **Model group** | A named, possibly nested grouping of models with a traversal strategy. |
| **Resolver** | The routing component that turns a model name + API flavor into candidate `(server, application, mapping)` tuples. |
| **Candidate scoring** | Ranking candidates by telemetry (load, latency, error rate, availability), priority/weight, and capacity. |
| **Route affinity** | Sticky routing keyed on application/server (per token/user, with a TTL). |
| **Model override rule** | A per-token row `requested name -> {to, offer, hide_target}`: rewrite this requested name to `to`, optionally advertise the requested name in this token's listing (`offer`) and drop the target's own name from it (`hide_target`). |
| **Unknown-model redirect** | A per-token opt-in: a requested model that does not apply is served by the token's last successfully routed model, then by a configured fallback, instead of failing. |
| **Offered / callable / existing** | The three model sets kept apart per token: what a listing shows, what a direct request can actually route to, and what exists at all. |
| **API flavor** | The client dialect: OpenAI-compatible, Anthropic-compatible, Codex (`/v1/responses`), or Claude Code (`/v1/messages`). |
| **Provider** | A backend client adapter (Ollama, OpenAI-compatible for vLLM/llama.cpp, or the mock). |
| **Agent token** | A per-server bearer token authenticating the Server-Agent's telemetry/cert calls. |
| **API token** | A user-created bearer token for calling the inference APIs. |
| **Session + CSRF** | Browser auth: a server-side session cookie plus the `X-OP-CSRF` header on state-changing requests. |
| **Run-as token** | The `X-OP-Run-As-Token` used by the portal chat to act under a chosen API token. |
| **Principal / scope** | The authenticated identity and its scopes (`admin`, `system`); roles are `user < admin < system_admin`. |
| **Admin group** | A group with delegated management rights over a subset (servers/services/resource groups/members). |
| **Step-up (system-admin mode)** | A time-boxed elevation required for sensitive system-admin actions. |
| **Service account** | A non-human principal with delegates and an allowed-model list. |
| **Project / user group / resource group** | Scoping and organization constructs for members, quotas, and provisioning. |
| **Principal limits** | Per-principal usage/quota limits. |
| **Usage event** | A per-request record (tokens, cost, energy, status, attribution) used for analytics. |
| **Payload capture** | The opt-in, encrypted-or-volatile, redacted storage of request/response payloads. |
| **Telemetry** | Host/GPU/power/temperature/hardware data pushed by the Server-Agent. |
| **Managed runtime** | The agent-managed model runtime: model server *processes* started and stopped on demand by the Server-Agent from launch specifications held in the gateway. Replaces llama-swap. See [Agent-Managed Model Runtime](cross-cutting/agent-runtime-manager.md). |
| **Launch spec** | The full command for one managed model — binary, argv, environment, working directory, listen port, health path, timeouts — stored per model mapping and edited in the portal. |
| **Router port** | The single HTTP port an agent exposes for all its managed models; it reads the `model` field out of each request body and reverse-proxies to the matching child process (which binds loopback only). |
| **Admission** | The agent-side decision, taken before any model process starts, that the new process may run alongside those already running: the co-residency matrix, the process limit, and the per-GPU VRAM budgets, all three. |
| **Co-residency matrix** | The operator's pairwise statement of which two managed models may run at the same time. Row present means pair allowed; it expresses *intent* and covers non-VRAM constraints, while the VRAM arithmetic is the veto. |
| **VRAM budget** | A per-`(server, GPU index)` megabyte ceiling that admission sums declared and measured demand against. An *absent* row is unconstrained; a row of `0` is a real zero budget. |
| **Feature flag (agent capability)** | A named capability, e.g. `runtime_manager`, that gateway and agent each declare; a feature is active only where both lists intersect. Versions are never compared ([ADR-025](09-architecture-decisions.md#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)). |
| **Admin state** | The persisted desired-state override on a managed model: `''` (none), `force_running`, or `force_stopped`. There is no restart command — a restart is a sequence over these states. |
| **File mode** | An agent whose managed-runtime configuration comes from a local file instead of the gateway. It reports its effective configuration upward with environment values masked, and the portal becomes read-only for that server. |
| **Dialect seam** | The abstraction that keeps SQLite and PostgreSQL on one query set (`internal/store/dialect.go`). |
| **Migration** | A forward-only, versioned schema change applied transactionally on startup. |
| **Store driver** | One of `memory`, `sqlite`, `postgres`. |
| **Edge TLS** | TLS on the public listener (public ACME). |
| **Mesh TLS** | mTLS on the agent/mesh listener, issued by the internal CA. |
| **Internal CA** | The gateway-managed certificate authority for the mesh. |
| **`cert_mode=proxy`** | Agent mode that runs a TLS-terminating reverse proxy in front of the local AI server. |
| **HTTPS auto-switch** | Reconciled switching of an application's scheme to `https` (modes manual/auto/selected + per-server override). |
| **NetBird mesh** | The private WireGuard mesh linking the gateway and AI servers; peers/policies are gateway-managed. |
| **Built-in theme** | A theme compiled into the frontend bundle (may contain code). |
| **External (data) theme** | A data-only theme loaded at runtime from the themes directory; not part of the source tree. |
| **CSS-variable bridge** | The mechanism by which `ThemeRoot` writes theme tokens as CSS custom properties consumed app-wide. |
| **Dispatch** | Forwarding a resolved inference request to `scheme://domain:port` as the application's provider type. |
| **SSE** | Server-Sent Events, used for streaming inference and live portal updates (telemetry, logs, benchmarks). |
| **Reconciler** | A periodic background loop (certs, NetBird token, energy, availability) that keeps state consistent. |
| **SPDX header** | The per-file license identifier (`AGPL-3.0-only`) plus copyright line. |
