# 6. Runtime View

Key runtime scenarios as sequence diagrams. Details of each subsystem are in the
[cross-cutting concepts](README.md#8-cross-cutting-concepts).

## 6.1 Inference request (routing, dispatch, streaming)

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway (edge)
    participant CO as compat
    participant R as routing (resolver+scorer)
    participant P as provider client
    participant AI as AI server
    participant U as usage

    C->>GW: POST /v1/chat/completions (Bearer token)
    GW->>GW: authenticate (bearer or session+CSRF)
    GW->>CO: translate request → internal model
    GW->>R: resolve(model, flavor) → candidates
    R->>R: score by telemetry, priority/weight, capacity
    R-->>GW: chosen (server, application, mapping)
    GW->>P: dispatch to scheme://domain:port (app model name)
    P->>AI: upstream request
    alt streaming
        AI-->>P: SSE chunks
        P-->>GW: chunks
        GW-->>C: SSE chunks (idle watchdog, deadlines lifted)
    else non-streaming
        AI-->>P: response
        P-->>GW: response
        GW-->>C: response
    end
    GW->>U: record usage event (tokens, cost, energy, status)
```

Notes: the four inference endpoints read the request body with no size cap (large
multimodal payloads) and lift the server read/write deadlines; SSE streams are
bounded by an idle watchdog and client disconnect, not a total cap. See
[Compatibility & Inference](cross-cutting/compatibility-and-inference.md).

## 6.2 Portal login (session + CSRF) with optional TOTP

```mermaid
sequenceDiagram
    participant B as Browser (SPA)
    participant GW as Gateway
    participant AC as account
    participant T as totp
    participant S as store

    B->>GW: POST /api/auth/login (email, password, X-OP-CSRF)
    GW->>AC: verify credentials (bcrypt)
    AC->>S: load user
    alt TOTP enabled/required
        AC-->>B: 2FA required
        B->>GW: POST /api/auth/login (+ TOTP code)
        GW->>T: verify code
    end
    AC->>S: create server-side session
    GW-->>B: Set-Cookie (session), principal + scopes
    B->>GW: GET /api/portal/me (cookie + X-OP-CSRF)
    GW-->>B: current user, role/scopes, session elevation
```

## 6.3 Server-Agent telemetry ingest

```mermaid
sequenceDiagram
    participant AG as Server-Agent
    participant GW as Gateway (agent mux)
    participant R as routing/telemetry store
    participant SSE as portal SSE

    loop every interval
        AG->>AG: collect host/GPU/power/temp
        AG->>GW: POST /api/agent/v1/telemetry (Bearer agent token)
        GW->>GW: derive server from token, whitelist fields
        GW->>R: store sample, update availability
        R-->>SSE: fan out live sample (per-server /events)
    end
    Note over AG,GW: WebSocket transport (/api/agent/v1/stream) is an<br/>alternative to per-interval POSTs — liveness via active Ping
    AG->>GW: POST /api/agent/v1/system-report (hardware inventory)
```

## 6.4 Model synchronization

```mermaid
sequenceDiagram
    participant A as Admin (portal)
    participant GW as Gateway
    participant PR as provider (ModelLister)
    participant AI as AI server
    participant S as store

    A->>GW: POST /api/portal/applications/{id}/sync-models
    GW->>PR: list upstream models
    PR->>AI: GET /v1/models or /api/tags
    AI-->>PR: model list
    PR-->>GW: models
    GW->>S: upsert model mappings for the application
    GW-->>A: updated mappings
```

## 6.5 Model-selection benchmark

```mermaid
sequenceDiagram
    participant A as Admin/scheduler
    participant GW as Gateway
    participant R as routing/benchmark
    participant AI as AI server
    participant SSE as portal SSE

    A->>GW: trigger benchmark (manual / scheduled / opportunistic)
    GW->>R: run capacity + speed probes
    R->>AI: probe requests (context size, throughput, concurrency)
    AI-->>R: measurements
    R->>R: persist metrics, update selection inputs
    R-->>SSE: live progress
    Note over R: metrics feed candidate scoring —<br/>hybrid swap-protection avoids thrashing
```

## 6.6 Mesh certificate issuance & HTTPS auto-switch

```mermaid
sequenceDiagram
    participant AG as Server-Agent
    participant GW as Gateway
    participant CA as certissue (internal CA)
    participant APP as AI server (behind agent proxy)

    AG->>GW: GET /api/agent/v1/ca
    GW-->>AG: CA bundle (trust store)
    AG->>GW: request leaf certificate
    GW->>CA: issue mesh leaf (SANs)
    CA-->>AG: leaf + key
    AG->>AG: start TLS-terminating proxy (cert_mode=proxy)
    AG->>GW: telemetry proxy_routes[] (tls_active:true)
    GW->>GW: ReconcileHTTPSSwitch → set application scheme=https
    Note over GW,APP: gateway now dispatches over HTTPS to the<br/>agent proxy — a scope-exit reverts to http
```

## 6.7 Agent-managed model runtime: on-demand start

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway
    participant RT as agent router
    participant MG as runtime manager (owner goroutine)
    participant CH as model process

    C->>GW: inference (gateway model name)
    GW->>RT: proxied request (app_model_name in JSON body)
    RT->>RT: buffer body, read `model` + `stream`
    RT->>MG: EnsureRunning(upstream model)
    MG->>MG: Permit (local policy) → Admit (matrix · limit · per-GPU VRAM)
    Note over MG: stopped → starting
    MG->>CH: exec (argv, scratch env, loopback port)
    MG->>CH: poll health until green (or startup_timeout_seconds)
    Note over MG: starting → running
    MG-->>RT: admitted + release()
    RT->>CH: forward, splice response unbuffered
    CH-->>C: tokens
    Note over MG,GW: every transition rings the doorbell →<br/>immediate coalesced telemetry sample →<br/>portal SSE shows stopped → starting → running
```

**The externally observable contract**, verified end to end with a real agent and
real child processes: an inference for a mapped model whose spec is not running
**blocks in the gateway** while the agent admits and starts the child, and the
request that triggered the start is the one that receives the answer — the first
request pays the start latency rather than failing fast. The state sequence on
the portal's runtime event stream is exactly `stopped → starting → running`, and
the status frame for the running spec reports the child's real `pid` and the
ephemeral loopback `port`. Nobody should redesign the router around a fail-fast
assumption.

**The degradation ladder is layered on purpose, and the WebSocket push is an
optimisation, not a dependency.** If a doorbell or a config frame is lost, the
agent's 60 s runtime poll catches up. If the gateway is entirely unreachable, the
agent keeps running from the last good runtime-config document it persisted
atomically to its own disk cache. In the reverse direction, every state
transition triggers an immediate coalesced out-of-band telemetry sample, so a
portal click feels like network latency rather than tick cadence. The
`RuntimeUpdates()` channel is buffered(1) with a non-blocking, coalescing send —
exactly like `certUpdates`. A reader who treats the push as *the* delivery
mechanism will "fix" a missed-frame bug with acks and retries instead of relying
on the poll and disk-cache fallbacks already designed in.

See [Agent-Managed Model Runtime](cross-cutting/agent-runtime-manager.md).

## 6.8 Theme resolution (built-in vs external)

```mermaid
sequenceDiagram
    participant B as Browser (ThemeRoot)
    participant GW as Gateway
    participant TH as theme (external loader)

    B->>GW: GET /api/system/theme (public, pre-auth)
    GW->>TH: is active id an external theme?
    alt external
        TH-->>GW: theme data (tokens, brand, product name)
        GW-->>B: {theme, source: external, data}
        B->>B: apply data as CSS variables, favicon/logo via /api/system/themes/{id}/…
    else built-in
        GW-->>B: {theme, source: builtin, data: null}
        B->>B: apply compiled built-in theme
    end
```
