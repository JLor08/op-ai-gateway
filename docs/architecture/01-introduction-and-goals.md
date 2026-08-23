# 1. Introduction & Goals

Purpose: what OnPrem AI Gateway is, the quality goals that shape its architecture,
and who it serves.

## 1.1 What it is

OnPrem AI Gateway (OP AI Gateway) is a self-hostable gateway and management portal
that sits between AI clients and self-operated inference servers. It presents
industry-standard, drop-in-compatible APIs to clients and routes each request to a
suitable backend model, while giving operators control, visibility, and governance
over an on-premises fleet of AI servers.

It consists of three deployable units:

- **Gateway** — a Go service (`op-ai-gateway`) that terminates client APIs, routes
  and dispatches inference, and serves the portal APIs.
- **Portal** — a React/TypeScript single-page application for administration, model
  and server management, analytics, and an in-browser chat playground.
- **Server-Agent** — a standalone, cross-platform Go binary (`op-ai-server-agent`)
  installed next to each AI server; it reports host, GPU, power, temperature, and
  hardware telemetry back to the gateway and can terminate mesh TLS in front of the
  local inference server.

## 1.2 Core requirements

- Expose **OpenAI-compatible**, **Anthropic-compatible**, **Codex**, and
  **Claude Code** client APIs, plus a built-in portal chat, and route them to
  Ollama / llama.cpp / vLLM backends.
- **Model-based routing** to multiple AI servers via operator-managed mappings,
  with **dynamic, load-aware route scoring** from live host telemetry.
- **Authentication and authorization**: local users (OIDC-ready), TOTP 2FA,
  user-created API tokens, and a layered role/group model.
- **Usage and token analytics** per request, user, token, and session, including
  cost and energy attribution.
- A **multi-platform reporting agent** for Linux, Windows, and macOS.
- **German and English** portal localization first, more later.
- Suitable for **on-premises / air-gapped-friendly** deployment (no mandatory
  external services; a private **NetBird mesh** option for gateway↔server links).

## 1.3 Quality goals

| Priority | Quality goal | What it means for the architecture |
|---|---|---|
| 1 | **Compatibility** | Clients built for OpenAI/Anthropic/Codex/Claude Code work unmodified; stable error codes; behavior matches the upstream contracts (streaming, tool-calls, images). |
| 2 | **Operability on-prem** | Runs as a small set of static, CGO-free containers; works with SQLite for a single node and PostgreSQL for production; no hard dependency on the public internet. |
| 3 | **Security & governance** | No plaintext secrets at rest; session + CSRF for browsers and bearer tokens for programs; layered RBAC; optional, redacted, encrypted payload capture; gateway-managed mTLS for the server mesh. |
| 4 | **Observability** | Per-request usage/cost/energy analytics; live host/GPU telemetry; OpenTelemetry tracing; live log streaming. |
| 5 | **Portability of the agent** | One agent binary per OS collects the richest telemetry each platform allows, degrading gracefully without elevated privileges. |
| 6 | **Adaptability** | Provider-neutral internal model; a dialect seam that keeps SQLite and PostgreSQL on one query set; a themable, localizable portal. |

## 1.4 Stakeholders

| Stakeholder | Concern |
|---|---|
| **Platform operator / system admin** | Deploy and run the gateway; manage AI servers, models, certificates, mesh, users, and system settings. |
| **Team admin / group manager** | Manage a delegated subset (servers, services, resource groups, group members) without full system rights. |
| **End user / developer** | Call the inference APIs with an API token, or use the portal chat; see own usage. |
| **AI-server owner** | Register and tune servers/applications/model-mappings they own. |
| **Auditor / security** | Verify secrets handling, access control, capture policy, and licensing. |
