# 10. Quality Requirements

Concrete quality scenarios that the architecture is expected to satisfy. These
refine the quality goals in [1.3](01-introduction-and-goals.md#13-quality-goals).

## 10.1 Quality tree (priorities)

1. Compatibility · 2. Operability (on-prem) · 3. Security & governance ·
4. Observability · 5. Agent portability · 6. Adaptability.

## 10.2 Scenarios

| # | Quality | Scenario | Expected response |
|---|---|---|---|
| Q1 | Compatibility | An unmodified OpenAI/Anthropic/Codex/Claude Code client sends a chat/messages/responses request (streaming, tool-calls, or images). | It works with the same request/response shape and stable error codes as the upstream API. |
| Q2 | Compatibility | A client streams and the upstream stalls. | The stream ends with an in-band idle-timeout frame; the client is not left hanging without bound. |
| Q3 | Operability | A single-node operator deploys with SQLite; a production operator deploys with PostgreSQL. | Same code paths; schema auto-migrates on startup; the conformance suite passes on both. |
| Q4 | Operability | The gateway is deployed air-gapped (no public internet). | It runs; only opt-in integrations (public ACME, SMTP, OTLP, NetBird control plane) are unavailable, and their absence is handled gracefully. |
| Q5 | Security | An attacker inspects the database / disk. | No plaintext passwords, token secrets, or decryptable secrets; captures are encrypted or were never on disk. |
| Q6 | Security | A browser request forges a cross-site state change. | Rejected without the `X-OP-CSRF` header; the session cookie alone is insufficient for state changes. |
| Q7 | Security | An external theme ships a malicious SVG logo. | It is served as `image/svg+xml` via `<img>` (no inline execution) with a hardening CSP; a colliding id cannot shadow a built-in theme. |
| Q8 | Observability | An operator needs to see load and cost. | Live per-server telemetry (SSE), per-request usage with cost and energy attribution, OTel traces, and a live log stream are available. |
| Q9 | Agent portability | The agent runs on Linux/Windows/macOS, unprivileged. | It collects the richest telemetry each platform allows without elevated rights and degrades gracefully where a source needs privileges. |
| Q10 | Availability | A transient dependency error occurs in a reconcile loop (cert/netbird/route). | The healthy resource is kept; only a definitive empty result tears it down. |
| Q11 | Adaptability | A new backend model server or a new brand theme is added. | Model server: a new provider adapter behind the provider interface. Theme: a data-only theme dropped into the themes directory, no rebuild. |

## 10.3 How quality is verified

- **Go unit + integration tests** across all backend packages; a **store
  conformance suite** run against SQLite (always) and PostgreSQL (when a DSN is
  set).
- **Frontend unit tests** (Vitest) and a **type-checked build**.
- **Playwright end-to-end suites** driving the real built portal against the real
  gateway (auth, invites, chat, capture, telemetry/agent, certificates, TOTP,
  SMTP, groups/projects/services/resource-groups, logs, and more).
- **Server-Agent** build + tests across target OSes (build-tagged, CGO-free).
- **Architecture tests** (dependency rules in both Go modules and the frontend)
  run inside the normal test suites — see
  [Architecture Tests](cross-cutting/architecture-tests.md).
- **Formatting, linting, CI, and a local SonarQube quality gate** — tooling,
  make targets, and gate semantics are documented in
  [Development Tooling & Quality Gates](cross-cutting/development-and-quality.md).
