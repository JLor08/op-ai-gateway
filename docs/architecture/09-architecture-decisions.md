# 9. Architecture Decisions

Load-bearing decisions and the non-obvious consequences ("gotchas") that must
survive. Each entry: context → decision → consequence. All are **Accepted** and
reflected in the current code.

## ADR-001 — License: AGPL-3.0-only
**Context:** the product is network-delivered software whose source protection
matters. **Decision:** license the whole project `AGPL-3.0-only`; every source
file carries the SPDX header; the running product surfaces §13 notices (portal
footer + `-license` on both binaries). **Consequence:** dependencies must be
AGPL-compatible (permissive + AGPL/GPLv3+/LGPL/MPL-2.0); GPL-2.0-only-without-later,
SSPL, BSL/source-available and proprietary are excluded; verify a dependency's
*effective* license, including anything it bundles transitively.
→ [Licensing](cross-cutting/licensing.md).

## ADR-002 — Provider-neutral core, thin compatibility edges
**Decision:** translate all client flavors into one internal inference model at the
edge and keep compatibility mapping in a single package. **Consequence:** new
client flavors or backends don't leak into routing/persistence; the internal model
stays provider-neutral.

## ADR-003 — Mapping-based routing (no static route table)
**Context:** routes must be operator-managed data, not code. **Decision:** route
over **active model mappings**: gateway model → `(server, application, mapping)` →
`scheme://domain:port`. **Consequence:** the earlier static route store was removed;
affinity is keyed on application/server; there is no `model_routes` table.

## ADR-004 — Three store drivers behind one dialect seam
**Decision:** support memory / SQLite / PostgreSQL with one query set behind a
`dialect` seam; evolve schema via forward-only versioned migrations applied
transactionally on startup. **Consequence:** any SQLite-vs-PostgreSQL difference
lives in the seam, never inline; never edit a shipped migration — append.

## ADR-005 — PostgreSQL needs wide column types
**Context:** PostgreSQL `integer`/`real` silently truncate wide Go values while
SQLite's 64-bit INTEGER/REAL mask it. **Decision:** use `bigint` for 64-bit/byte
columns and `double precision` for float columns (and in `cast(… as …)` in
arithmetic/EWMA SQL). **Consequence:** this class of bug only surfaces on a real
PostgreSQL deployment; the conformance suite must run against PostgreSQL, not only
SQLite.

## ADR-006 — Two auth modes; chat completions also session-reachable
**Decision:** browsers use a server-side session cookie + `X-OP-CSRF`; programs use
bearer API tokens. `/v1/chat/completions` is the one inference endpoint that also
accepts the session (plus the internal trusted-loopback path); `/v1/responses` and
`/v1/messages` are bearer-only. **Consequence:** the portal chat can act under the
user's session without an API token — each chat turn runs as a server-side run
whose executor calls the gateway's own `/v1/chat/completions` over loopback and
streams the result to the browser via SSE (surviving page reloads/disconnects).

## ADR-007 — Secrets at rest: the `enc:`/`plain:` scheme
**Decision:** decryptable secrets are sealed with a key, or held plaintext only in
volatile RAM, or rejected on disk when no key is present; DTOs expose only `*_set`
flags. Passwords are bcrypt-hashed; token secrets are stored hashed.
**Consequence:** no plaintext secret is ever written unencrypted to disk by the
application.

## ADR-008 — Payload capture: opt-in, encrypted-or-volatile, redacted
**Context:** prompts/responses must not be persisted by default. **Decision:**
capture runs only when a global kill switch is on AND (a per-token flag OR a
system override); it is **encrypted-at-rest** (SQLite + encryption key) or
**volatile-in-RAM** (memory driver, or SQLite without a key) — never written
unencrypted to disk by the application. Sensitive headers are redacted; a "secret"
capture is visible only to its owner (even admins are excluded).
**Consequence:** OS-level swap/core-dump of RAM is explicitly out of scope.

## ADR-009 — No body-size cap on inference; 1 MiB on control-plane
**Decision:** the four inference endpoints read the body with no size cap (large
base64/multimodal requests); control-plane endpoints keep a 1 MiB cap. Any reverse
proxy must set `client_max_body_size 0` on the inference paths.

## ADR-010 — Streaming: idle watchdog + lifted deadlines, no total cap
**Decision:** the inference endpoints lift the 30 s server read/write deadlines and
bound SSE streams by an inactivity watchdog plus client disconnect, not a total
cap; a stalled upstream ends with an in-band `stream_idle_timeout` frame.
**Consequence:** provider `CompleteStream` must not impose its own total deadline
(the per-target timeout is for non-streaming completion only).

## ADR-011 — A standalone, CGO-free reporting agent
**Decision:** telemetry is collected by a separate binary (`op-ai-server-agent`)
that imports nothing from the gateway and cross-compiles CGO-free. It authenticates
with a per-server token and pushes over HTTP or WebSocket. **Consequence:** the
agent can be distributed and updated independently of the gateway.

## ADR-012 — Unprivileged ICMP with Linux Echo-ID handling
**Decision:** ICMP reachability uses datagram sockets (no `CAP_NET_RAW`; needs the
`ping_group_range` sysctl). **Consequence:** Linux `SOCK_DGRAM` rewrites the ICMP
Echo ID, so replies are matched by Type+Seq+Data, never by echo ID (macOS masks
this — a naive match passes locally and breaks on Linux).

## ADR-013 — Cross-platform power/temperature sources
**Decision:** collect watts and CPU temperature per platform: Linux RAPL sysfs
(root-only energy) + hwmon sensors (non-root); macOS `powermetrics` (sudo);
Windows via an operator-installed LibreHardwareMonitor `/data.json` (license-clean).
**Consequence:** several sources need privileges and are absent otherwise; the agent
degrades gracefully.

## ADR-014 — NetBird mesh with a gateway-managed, rotatable PAT
**Decision:** the gateway manages NetBird peers/policies and holds a rotatable admin
PAT (auto-rotates before expiry with rollback; metadata persisted before the
credential so a failed write never bricks the module; `0` disables auto-rotation).
**Consequence:** the token is never logged or returned by any endpoint/DTO; the
NetBird sidecar shares the gateway's network namespace, making it a potential SPOF
for the public API.

## ADR-015 — Agent↔gateway WebSocket transport liveness
**Decision:** the optional WS transport probes liveness with an active `conn.Ping`
(a read-deadline alone false-trips because Read swallows pings); reconnect resets
on stable, not on connect. **Consequence:** the nginx WS location must re-declare
the internal-header blanking and override `Connection`.

## ADR-016 — Internal CA; public and mesh TLS are separate surfaces
**Decision:** an internal CA issues mesh (mTLS) certificates for the agent listener;
edge TLS uses public ACME. The public and mesh listeners enforce TLS and
authorization independently. **Consequence:** certificate reconcilers keep a healthy
certificate on a transient dependency error (only a definitive empty result tears
it down).

## ADR-017 — Agent TLS proxy + HTTPS auto-switch with scope-exit revert
**Decision:** `cert_mode=proxy` runs an agent-side TLS-terminating proxy in front of
the AI server; the gateway assigns a proxy listen port and reconciles an automatic
HTTP→HTTPS application switch (modes manual/auto/selected + a three-valued
per-server override). **Consequence:** when a server leaves switch scope, the
reconcile must **revert** it to `http` (a scope-exit that only skips would strand an
application on `https` against a torn-down proxy port).

## ADR-018 — OpenTelemetry decorators via the global provider
**Decision:** tracing decorators are generated (gowrap) and live in their own
packages, wired through the OTel global, to avoid a tracing→portal→provider import
cycle. **Consequence:** the tracer is cached in an atomic pointer updated in Setup
(an init-cached global would only adopt the first provider and break multi-Setup
tests).

## ADR-019 — Admission-control queue is edge-triggered but liveness-checked
**Decision:** the model concurrency admission queue combines dequeue-on-signal with
a bounded liveness re-check and a wall-clock deadline. **Consequence:** lost wakeups
are invisible to the race detector, so the design must not rely on signals alone.

## ADR-020 — Two-tier themes (built-in code + external data)
**Context:** brand/operator themes should be deployable without publishing them in
the AGPL source tree. **Decision:** built-in code themes ship in the repo; external
**data-only** themes load at runtime from a directory (baked + mountable; private
dirs gitignored). External-theme assets are served via `<img>` (never inline SVG)
and hardened with CSP; a colliding external id never shadows a built-in.
**Consequence:** operator/brand themes stay out of the source tree yet ship with a
deployment.

## ADR-021 — Layered RBAC with delegation and step-up
**Decision:** roles `user < admin < system_admin`; `system_admin` also carries a
`system` scope; delegated admin groups manage subsets; sensitive system-admin
actions require a time-boxed step-up. A last-active-admin guard prevents lockout.
**Consequence:** authorization flags must be read from a role-scope that is always
present; making a role-scope conditional (e.g. step-up) can silently break paths
that read it as an authorization flag.

## ADR-022 — Reconcile loops keep healthy resources on transient errors
**Decision:** periodic reconcilers return a `(value, ok)` result; `ok=false` skips
the mutation (keep current), and only a definitive empty result tears a resource
down. Per-tick errors are logged at Debug, not Warn. **Consequence:** a transient
dependency error never tears down a healthy certificate, route, or peer.

## ADR-023 — A model listing is a display; reach is a separate set
**Context:** per-token settings shape both what a client *sees* in the model
listing (offered override aliases, hidden targets, `model_settings`
hidden/locked) and what a request may be *rewritten* to (override rules,
catch-all, unknown-model redirect). **Decision:** keep the two strictly apart in
three sets — *offered* (the listing, alias overlay applied), *callable* (what a
direct request can actually route to: excludes `locked`, includes merely
`hidden`), and *existing* (what exists at all, ignoring per-token reach) — and
let every rewrite decision read *callable*, never *offered*. A rewritten name
then passes every admission gate exactly as if the client had sent it.
**Consequence:** suppressing a name from a listing is never an access control,
and a rewrite can never widen what a token may reach; conversely, judging a
request by the listing would reroute requests the token was entitled to serve.
See [Routing & Model Selection §2.1–2.2](cross-cutting/routing-and-model-selection.md).
