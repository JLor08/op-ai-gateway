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

**Amended by [ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread) (the image kind splits the loopback leg off the
session leg).** The decision above is unchanged in substance and its two named
bearer-only endpoints are still bearer-only. What it could not distinguish, because
nothing needed the distinction yet, is that "the session" and "the internal
trusted-loopback path" are two separate legs and an endpoint may admit the second
without the first. `/v1/images/generations` does exactly that: the portal-chat run
executor reaches it over loopback (`requireInternalOrBearerAnyScope` →
`authenticateInternalOrBearer`, `internal/gateway/auth_internal_or_bearer.go`), while
a logged-in browser cannot call it at all. So `/v1/chat/completions` remains the only
inference endpoint reachable with a **session cookie**, and it is no longer the only
one reachable over **loopback** — read the parenthesis above as naming a leg that
ADR-043 later granted separately. See [Security, Authentication & Authorization
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance)
for the current three-leg table.

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
**Decision:** the inference endpoints read the body with no size cap (large
base64/multimodal requests); control-plane endpoints keep a 1 MiB cap. The portal
chat endpoints are the one control-plane exception: their bodies carry whole chat
documents and are capped at `portal.MaxChatRequestBytes` (the 4 MiB content cap
plus 1 MiB for the JSON envelope), which the bundled nginx allows on
`/api/portal/chats`. Any reverse
proxy must set `client_max_body_size 0` on the inference paths, and must allow at
least `portal.MaxChatRequestBytes` (5 MiB) on `/api/portal/chats`. **Consequence:**
the rule is per-endpoint-class, not a fixed list — there were four such
endpoints when this was decided and there are five since
`/v1/images/generations`, which reads an uncapped body for the same reason and
was admitted under this decision rather than amending it
([Compatibility & Inference
§10](cross-cutting/compatibility-and-inference.md#10-request-bodies-and-size-limits)).

## ADR-010 — Streaming: idle watchdog + lifted deadlines, no total cap
**Decision:** the inference endpoints lift the 30 s server read/write deadlines and
bound SSE streams by an inactivity watchdog plus client disconnect, not a total
cap; a stalled upstream ends with an in-band `stream_idle_timeout` frame.
**Consequence:** provider `CompleteStream` must not impose its own total deadline.
The per-target timeout (the application's `timeout_ms`) bounds non-streaming
completion, and on the gateway's own benchmark streams it also sets the
first-data budget: the larger of `timeout_ms` and the idle budget bounds a
stream that the upstream keeps alive with SSE comments but that has not yet
produced an event. That is not a total cap: once the first event arrives only
the idle watchdog applies, and `CompleteStream` still arms no timer of its own
([Compatibility & Inference
§7.2](cross-cutting/compatibility-and-inference.md#72-the-benchmark-stream-watchdog)).

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

## ADR-017 — Agent TLS proxy + HTTPS auto-switch: scope-exit revert, but no downgrade on broken TLS
**Decision:** `cert_mode=proxy` runs an agent-side TLS-terminating proxy in front of
the AI server; the gateway assigns a proxy listen port and reconciles an automatic
HTTP→HTTPS application switch (modes manual/auto/selected + a three-valued
per-server override). An explicit reported `tls_active:false` is **declined, never
reverted** — the gateway never answers a broken certificate or a dead listener by
putting inference traffic back on plaintext. **Consequence:** two automatic moves
that look alike are decided oppositely, and the difference is whether an operator
asked for anything. A `tls_active:false` report leaves the application on `https`
and **unreachable** until TLS works again; that availability is paid on purpose and
is not allowed to be silent — a `Warn` on **every** reconcile pass plus
`https_switch.unreachable_apps` in `GET /api/system/certificates`, rendered as an
error in the portal's certificate view — and recovery needs no action, because the
application was never moved. A **scope exit** still reverts to `http`
unconditionally, deliberately kept: the gateway itself withdrew the routes, so the
revert completes the operator's own action, and skipping it would strand the
application on `https` against a torn-down proxy port with no path back. Do not
"restore symmetry" by reinstating the downgrade; ADR-022's general keep-healthy rule
is deliberately not applied to plaintext downgrade.
→ [Certificates & TLS §7.1](cross-cutting/certificates-tls.md#71-no-automatic-downgrade-to-plaintext).

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

## ADR-024 — Managed runtime: the gateway specifies the launch, the AI server permits it
**Context:** replacing llama-swap means portal users author real command lines
that a process on an AI server then executes. **Decision:** split ownership. The
full launch specification (binary, argv, env, work dir, port, health path,
timeouts) lives in the gateway database and is portal-maintained; **what may
actually execute is decided only by the agent's own local configuration** — an
absolute-path binary allowlist plus permitted work/model directories — and the
agent `exec`s an argv array directly, with no shell, as an unprivileged user.
The two empty-list defaults are deliberately asymmetric: an empty **binary**
allowlist starts nothing (and says so with a visible `not_permitted` reason),
while an empty **directory** allowlist permits any `work_dir`, because the
directory check is defence in depth behind an already-allowlisted binary, not
the primary boundary. **Consequence:** enabling the feature is an explicit act
on each agent host and the gateway cannot widen it; a portal admin with
server-management rights chooses *what runs* only within that allowlist. A later
change that lets the gateway supply a shell string, or that reads an empty
allowlist as "allow all" for convenience, converts portal write access into
arbitrary code execution on every AI server.
→ [Agent-Managed Model Runtime §3](cross-cutting/agent-runtime-manager.md).

## ADR-025 — Agent capabilities negotiate by named feature flags, not versions
**Context:** the agent and gateway ship and upgrade independently, and version
comparison is fragile under forks and backports. **Decision:** gateway and agent
each declare a list of **named feature flags**, and a flag both sides declare is
active if and only if a string-equal name appears on both lists; a flag that
states a fact about one side alone need only be on that side's list (below).
Agent → gateway rides the telemetry sample's existing `capabilities` object;
gateway → agent is an ETag-conditional `GET /api/agent/v1/features` —
deliberately not a hello frame, so it works identically for POST and WebSocket
agents. Negotiation is re-decided
continuously, not at boot. Unknown names are ignored on both sides; a missing or
empty list, and a 404 on the features endpoint, all read as the empty set. One
flag per **shipped** capability. **Consequence:** `if agent_version >= X` is not
an acceptable gate anywhere; a mixed-version fleet degrades silently and
correctly; and a negotiated-away feature must be surfaced explicitly in the
portal rather than becoming a silent no-op.

A flag also earns its place when the capability it names is only a *fact the
agent states about itself*, and `runtime_config_ack`
([§7.2](cross-cutting/agent-runtime-manager.md#72-the-applied-document-acknowledgement))
is the clearest case: the agent needs no permission to report which
runtime-config document it has applied, and it sends the field unconditionally.
What the name buys is on the CONSUMER side — "no answer yet" and "no answer
ever" are the same silence on the wire, and no timeout separates them, so
without the name a gateway waiting for that report must either hang against
every older binary or abandon the report for everyone. The pattern to follow:
**when the absence of a message is the thing you have to interpret, the flag
gates the FALLBACK rather than the feature** — the behaviour without it is a
weaker but correct path, never "off".

The mirror image exists too: a fact the **gateway** states about itself, on the
gateway's list alone. `capability_source_sdcpp` (issue #154) says the gateway's
capability ingest accepts the `sdcpp_capabilities` source, and the agent reads
it before it sends that source at all, because a gateway without the name
would void every such capability pass. The agent declares nothing back: the
gateway is the side that must accept the source, nothing on the gateway waits
on the agent, and an agent-side name would therefore gate nothing — so a flag
on one list only is not a half-finished negotiation, it is the name placed on
the side whose fact it is.
→ [Agent-Managed Model Runtime §7](cross-cutting/agent-runtime-manager.md).

## ADR-026 — Gateway→agent control is desired state, not commands
**Context:** operator actions on a managed model must survive a WebSocket
reconnect. **Decision:** manual start/stop are **persisted desired-state
overrides** (`admin_state`: `''` | `force_running` | `force_stopped`), never
fire-once command frames. A command sent during a reconnect is silently lost, so
commands would demand acks, retries and dedup — while a persisted desired state
has to exist anyway for resync after any disconnect. **Consequence:** every
operator action is expressible as state, including *restart*, which the portal
drives as the sequence `force_stopped` → observe `stopped` → clear. There is no
restart endpoint, and the sequence therefore carries the whole correctness
burden (bounded wait, completion on a transition rather than a state, and no
silent clearing on timeout). A genuinely imperative action would need its own
frame type behind its own feature flag. A benchmark that writes an override
itself — the VRAM run's drain and a manual speed run's stop-all
([ADR-047](#adr-047--a-manual-speed-run-measures-every-agent-model-on-an-emptied-server-it-stops-the-servers-running-models-before-each-cold-pass-and-lifts-the-servers-pins-for-the-run)) —
clears it on every exit, because it created it, and records it in the override
lease that the gateway reconciles at start; the portal's own discipline is
unchanged.
→ [Agent-Managed Model Runtime §11.2](cross-cutting/agent-runtime-manager.md).

## ADR-027 — Model secrets never enter the gateway
**Context:** a launch spec's environment is exactly where a model server's
tokens live, and the gateway has a system-wide no-plaintext-secrets rule.
**Decision:** a spec's `env` **values are referential placeholders**
(`${AGENT_ENV:NAME}`) resolved on the AI server from the agent's own process
environment; `${PORT}`, `${MODEL}` and `${HOST_GPU_IDS}` are the only other
placeholders, and none of them carries a secret. A missing variable is a
hard error naming the variable, never a silent empty substitution; the
`OP_AGENT_*` namespace is refused before `getenv` is consulted; and the child's
environment is built from scratch rather than inherited. **Consequence:** the
gateway never stores or transports a model secret and the portal cannot leak
one, so no new exception to the secrets rule is needed. The accepted cost,
stated plainly: the secret must already exist on the AI server and the portal
cannot set it — the natural feature request "let me type the token in the
portal" must be refused. Note the residual: `args` are expanded the same way but
are **not** masked in the upward report, so secrets belong in `env` only.
→ [Agent-Managed Model Runtime §3.2](cross-cutting/agent-runtime-manager.md).

## ADR-028 — Runtime-config notifications are gated by write scope, not by changed field
**Context:** the agent's runtime-config document is derived from six kinds of
row, and a portal write that changes any of them must reach the agent promptly.
**Decision:** any successful write that **can** change a server's document
notifies that server's agent, and the decision is taken from the **write path's
own scope** — which row it writes, and for an application-owned row whether that
application is the server's `server_agent` one — never from which field the
request carried. A per-path "runtime-relevant fields" filter was rejected: it is
an uncompiled duplicate of the document's derivation in another file, which rots
the first time that derivation grows a field. **Consequence:** fifteen call sites
notify, some redundantly; over-notification is licensed by one fail-closed map
lookup at the delivery point plus the agent's ETag-based idempotence; the 60 s
agent poll is the backstop, so a missed notification degrades to "the change
takes effect after about a minute" rather than permanent divergence. Every new
write path in the portal service must be checked against this rule.
→ [Agent-Managed Model Runtime §9](cross-cutting/agent-runtime-manager.md).

## ADR-029 — Runtime-domain writes are full-document replaces, gated on their own GET
**Context:** the runtime spec, the co-residency pair list and the per-GPU budget
list are each edited as a whole. **Decision:** every runtime-domain write is a
**full replacement**, never a delta — and the rule that follows for any UI on top
of one is that **a control which triggers such a write must not exist until its
own GET has resolved**, must be disabled while a write is in flight, and must
treat a *failed reload over an existing payload* as not-ready. **Consequence:**
`null` (not loaded) and `[]` (loaded and empty) are different facts, and the
idiomatic `data ?? []` collapses them into silent data loss with a successful
200 and no error anywhere — a single click landing early once erased an
application's whole co-residency set, and a Save landing early erased a server's
whole budget set. The canonical rendering is the four-state
`loading | error | stale-error | ready` fallback, and a not-ready tab renders a
loading line *instead of* the form rather than a disabled form.
→ [Agent-Managed Model Runtime §11.1](cross-cutting/agent-runtime-manager.md).

## ADR-030 — Proxy participation is an operator-owned flag with a port invariant, not an encoding
**Context:** whether an application takes part in the gateway-guided TLS proxy
was encoded IMPLICITLY as `scheme == "https" && proxy_listen_port == 0`. That
encoding could not express the one thing the operator asked for — take a
**plain-http** application out of the proxy — because an enabled `http`
application is a candidate unconditionally, is assigned a port on the agent's
next fetch and is flipped to `https` on the next reconcile. **Decision:** a new
operator-owned column, `applications.proxy_excluded` (migration 70), is the
**authoritative and only** representation of participation, orthogonal to
`scheme`; migration 70 **backfills** the retired encoding into it, so the two do
not coexist. The backfill is not the only reader of that encoding: the write path
re-applies the same translation on **every** write (a request that says nothing
about participation and resolves to `https` with no proxy port is normalized to
excluded), which is what keeps the column authoritative for a row a pre-70 client
writes in the old spelling. Three fields carry one meaning each — participation,
transport, listener identity — held together by the invariant
**`ProxyExcluded == true` implies `ProxyListenPort == 0`**, enforced at the end of
the mutation block in both `CreateApplication` and `UpdateApplication` by a rule
that tests the POST-MUTATION row, because every rule that branches on the shape of
the request alone lets a two-request sequence through. **Consequence:** four other
derivations (`ApplicationEndpoint`, `activePortStrings`, `revertScopeExit`,
`HTTPSSwitchUnreachableApps`) each test `https && ProxyListenPort != 0`, which an
excluded application can never satisfy, so none of them changes; the candidate
predicate keeps its `https` arm as a **physical** guard (the proxy only fronts a
plaintext upstream) rather than as a second representation of intent. Rejected:
a `proxy_listen_port = -1` sentinel (two facts in one field, and an old binary
composes `https://domain:-1` — a silent permanent outage on exactly the
applications an operator excluded deliberately); and deriving participation
forever without a backfill (one fact, two storage states, reconciled only by a
review rule). Excluding an application **releases** its proxy port to the free
pool, which is a strict improvement on a non-candidate reserving a port against
every sibling forever; its accepted cost is that re-including later draws a
fresh number, so the exclusion is logged at `Warn` naming the released port.
This is **not** a reinstated automatic downgrade (ADR-017): the gateway never
writes a scheme on this path — it stores the scheme the operator sent — and
`revertScopeExit` is left unguarded on purpose so it remains the repair path for
an invariant-violating row. The portal's visibility gate is the server's
https-switch **scope**, which is durable, and never the agent's reported
`cert_mode`, whose absence is reachable twice (after every gateway restart, and
on a proxy-mode agent before its first leaf) — a control that hid on it would
vanish exactly while an operator was provisioning.
→ [Certificates & TLS §7](cross-cutting/certificates-tls.md), [Data Model](reference/data-model.md).

## ADR-031 — Per-process VRAM on Windows: PDH counters plus a D3DKMT LUID bridge
**Context:** co-residency admission charges a managed process its *measured*
VRAM wherever a measurement exists, and on Windows there was none to be had:
under the **WDDM** driver model the OS, not the NVIDIA driver, owns GPU memory,
so `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory` answers `[N/A]`
for `used_memory`. The failure was not benign — `[N/A]` parses to `0`, the
measurer returned a *non-nil* map of zeros, and a present key outranks the
operator's estimate, so every managed process on every Windows host was admitted
as needing **0 MB** and each GPU's budget read as entirely free. **Decision:**
measure Windows through the `\GPU Process Memory(*)\Dedicated Usage` **PDH**
counter, whose instance names carry the PID and the display-adapter **LUID**;
bridge that LUID to a PCI address with the user-mode-callable gdi32 **D3DKMT**
exports (`D3DKMTOpenAdapterFromLuid` +
`D3DKMTQueryAdapterInfo(KMTQAITYPE_ADAPTERADDRESS)`); and join the PCI address
to the GPU index specs and budgets are written in terms of via
`nvidia-smi --query-gpu=index,pci.bus_id`. The measurer is chosen by **build
tag** (`collector.NewVRAMMeasurer`), the compute-apps measurer is **never**
installed on Windows, and a measured `<= 0` no longer overrides an estimate on
**any** platform. All grammars, arithmetic and cache decisions live in a
build-tag-free file so Linux CI exercises the code Windows runs; only the
syscalls sit behind `//go:build windows`, pinned by compile-time struct-layout
assertions. **Rejected:** the **TCC** driver model, which does restore
per-process reporting but disables the adapter's display output and is
unavailable on most GeForce parts — these are workstation-class hosts;
installing compute-apps on Windows regardless (a non-nil map of zeros is *worse*
than no measurer, because `0` overrides the estimate while `nil` falls back to
it); reading the neighbouring `Shared Usage` / `Non Local Usage` counters as
VRAM-spillover detection (they read identically on all three GPUs of the probe
host, so they are not per-adapter figures); and a second `PdhCollectQueryData`
with a settling delay (the counter is a raw gauge — one collection already
returned correct values, and the delay would be spent on the manager's
serialized owner goroutine during an admission). **Consequence:** Windows
admission now runs on real numbers — within 0.04–0.8 % of
`nvidia-smi memory.used` per GPU, with attribution agreeing with `nvidia-smi`'s
own PID→GPU mapping for 15 of 15 PIDs on a 3-GPU host — but the syscall half is
verified by review, the compile-time assertions and out-of-band Windows runs
only, because CI builds nothing for Windows
([§11.3](11-risks-and-technical-debt.md#113-testing-blind-spots-to-remember)).
Two rules follow and must not be relaxed. First, the **negative** LUID cache may
record only durable findings, since a wrongly cached adapter loses its
measurement silently for the life of the process: D3DKMT *refusing to open* the
adapter (`STATUS_INVALID_PARAMETER`), an implausible address it *did* report, or
a *fresh and complete* `nvidia-smi` reading with no GPU at that address —
everything else, a `STATUS_DEVICE_REMOVED` from a TDR and any failure of the
address query included, is the absence of an answer and is retried. The rule is
an **allowlist** precisely because the two mistakes are not symmetric (three
wasted syscalls a cycle against a working GPU going unmeasured until the process
restarts), and it must stay one function in the build-tag-free half where CI can
test it — it was stated in this ADR and in the design doc while the code cached
*every* probe error alike, which is the kind of divergence `//go:build windows`
makes invisible. Second, a PCI address claimed by two cards is refused rather
than resolved to the last row, because `D3DKMT_ADAPTERADDRESS` reports no PCI
domain and a confident wrong GPU index is worse than none.
→ [Agent-Managed Model Runtime §5.3](cross-cutting/agent-runtime-manager.md#53-unknown-vram-resolves-itself-by-measurement).

## ADR-032 — Known VRAM demand outranks unknown: the unknown side blocks, never evicts
**Context:** the unknown-VRAM rule is symmetric — a candidate whose own demand
is unknown may start only *alone* on its GPUs (rule 4), and an occupant of
unknown demand blocks the cards it holds (rule 5, added alongside the Windows
measurer so that "alone" survives the next admission). Symmetric **eviction
rights** do not converge. With one spec estimated and one left blank on the same
card, rule 5 evicts the blank incumbent for the estimated candidate while rule 4
evicts the estimated incumbent for the blank candidate: alternating requests are
each served only after destroying the other's loaded model, forever.
**Decision:** make the two rules a **total order — known demand beats unknown
demand.** Rule 5 keeps its eviction right unchanged. Rule 4 gives its up: a
candidate whose own demand is unknown no longer evicts an occupant of **known**
demand, it *blocks* — the existing terminal `pending_vram_unknown` when that
occupant is **pinned** and can never leave, and `Wait` for every other one. The
block is **unconditional**: it stands even where the matrix or the arithmetic
would have evicted that same occupant anyway, because honouring the order only
where no other reason exists leaves those pairs evicting in both directions —
the same defect, one rule over.
**Rejected:** giving rule 4 the priority instead (it hands a misconfigured spec
the power to kill correctly configured ones, which is the wrong side of the trade
in every direction); charging an unknown demand the whole budget inside the
per-GPU arithmetic instead of naming the contention (the eviction loop releases
the same `0`, so the sum never comes down — that version evicts every idle
process on the card *and* still answers `Wait`); and breaking the tie on age or
`last_used` (still lets a blank spec destroy a configured one, and makes the
outcome depend on request order). **Consequence:** an unknown-demand spec may
still have a card to itself — that is rule 4's stated intent — but only a card
it can *get* to itself; what it loses is the privilege of evicting a working
model to get there, and the spec that loses the contest is always the
misconfigured one. Where **both** sides are
unknown neither outranks the other, and the pre-existing mutual eviction stands
(rule 5 still evicts an evictable unknown occupant) — unchanged by this decision
rather than fixed by it, and recorded as an acceptance in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances). The
price of the order is a `Wait` returned while an idle victim was plainly
available, and that wait is only as short as the occupant's own
`idle_timeout_seconds` (a `0` there means *never unload*): the ways out are a
measurement, or the operator's estimate on the spec that is missing one.
Because that price can be unbounded — an idle occupant with
`idle_timeout_seconds: 0` never leaves, and the candidate requeues to its
admission timeout on every request — **this `Wait` is the one that reports
itself**: `Admit` gives it a message and the manager records it as the blocked
spec's own `last_error`, naming the occupant and the card, which the portal
shows in an always-visible column. Every other `Wait` stays silent, since a
spec queued behind a busy neighbour is ordinary operation.
→ [Agent-Managed Model Runtime §5.2](cross-cutting/agent-runtime-manager.md#52-the-three-gates), [§5.3](cross-cutting/agent-runtime-manager.md#53-unknown-vram-resolves-itself-by-measurement).

## ADR-033 — Endpoint modes replace the `native_*` booleans: independent per-endpoint disable, per-spec snapshot
**Context:** whether the gateway proxied a Codex (`/v1/responses`) or Claude
Code (`/v1/messages`) request natively, or translated it to
`/v1/chat/completions`, was a per-application boolean
(`native_responses`/`native_messages`). That encoding could express only
*translate vs. pass-through* — there was no way to turn an endpoint **off**
while keeping the application's plain chat-completions traffic and its other
coding-agent endpoint alive, and no way to configure a `server_agent`
application's models independently of one another; every managed model shared
its parent application's one pair of booleans. **Decision:** replace the two
booleans with one three-state `routing.EndpointMode`
(`disabled`/`translate`/`passthrough`) per endpoint, on **two** levels: the
application (as before, now a richer type) and — new — each `server_agent`
runtime spec, which gains its own `api_flavors`/`responses_mode`/
`messages_mode` trio and becomes the **sole** authority for its model once
saved (the application's values are only the create-time template and the
no-spec fallback; a later application edit never propagates to an existing
spec — "Snapshot aus App"). An endpoint is served only when its coarse
`openai`/`anthropic` flavor is enabled **and** its mode is not `disabled`,
which is what makes the disable independent of the flavor checkbox: an
application can keep serving plain chat completions while refusing Codex's
`/v1/responses` specifically. Because a `server_agent` model's authoritative
mode is only knowable after routing has resolved which model a request means,
enforcement is split in two: an **ordinary** application's `disabled` endpoint
is excluded at route-selection time, refining the existing coarse flavor-based
candidacy gate to be endpoint-aware; a **`server_agent`** model's `disabled`
mode cannot be checked that early — its resolved runtime spec is the only
authority — so it is rejected at **dispatch** instead, once the
runtime spec has resolved — a new stable code, `responses.endpoint_disabled`/
`messages.endpoint_disabled` (HTTP 404), that never falls through to the lossy
translate path. Every application type now defaults both modes to
`passthrough` (research confirmed every supported upstream — Ollama, vLLM,
llama.cpp, llama-swap, LiteLLM — serves both native endpoints today), replacing
the old per-type translate exceptions. **Consequence:** the migration
(`application_endpoint_modes`) is additive only — it backfills
`applications.responses_mode`/`messages_mode` from the existing booleans
(`true`→`passthrough`, `false`→`translate`, so no application's behavior
changes on upgrade) and snapshots every existing `agent_runtime_specs` row from
its parent application's just-backfilled values; the `native_responses`/
`native_messages` columns stay in the schema, permanently inert, per the
append-only migration rule. The three new spec-level fields are **gateway-side
only** — the agent's runtime router forwards `/v1/responses` and `/v1/messages`
to its managed child verbatim and routes purely on the request's `model`
field, so the disabled/translate/passthrough decision never needs to reach it;
the fields were deliberately never added to `AgentRuntimeSpecDTO` or the
agent's `runtime.Spec` wire type, so `server-agent/` is untouched and
`agent.Version` is not bumped for this feature — pinned by a guard test
asserting the agent-facing runtime-config JSON never carries
`api_flavors`/`responses_mode`/`messages_mode`. **Rejected:** teaching the
agent's router about the two coding-agent paths so it could make the decision
locally (it would duplicate the gateway's own resolved-model knowledge and
require a wire/version bump for a decision the gateway already makes correctly
before dispatch); and dynamic inheritance of a `server_agent` application's
current values into its existing specs (an editor opening an old spec would
see values silently drift out from under it whenever a colleague edited the
application, defeating the point of a per-model override).
→ [Compatibility & Inference §6](cross-cutting/compatibility-and-inference.md#6-endpoint-modes-and-native-passthrough),
[Agent-Managed Model Runtime §7.1](cross-cutting/agent-runtime-manager.md#71-agent-versioning),
[§11.5](cross-cutting/agent-runtime-manager.md#115-what-each-remaining-tab-shows),
[Data Model §4](reference/data-model.md#4-migration-history-86-migrations),
[API Surface](reference/api-surface.md#api-variant-endpoint-modes-responses_mode--messages_mode).

## ADR-034 — GPU order is explicit; `set_visible_devices` gets an env or args mode
**Context:** the agent numerically sorted a spec's declared GPU indices before
building any visible-devices value — the env-mode variable and
`${HOST_GPU_IDS}` alike — discarding whatever order the operator gave the rows
in the portal. `set_visible_devices` could only set a whole-process visibility
variable, which hides every other card from the child; there was no way to
steer llama.cpp's own finer-grained `--device` flag without an operator
composing it by hand from `${HOST_GPU_IDS}`. **Decision:** persist the
operator's GPU order explicitly — `agent_runtime_spec_gpus.position`
(migration 73), read back `order by position, gpu_index` — and honor that
order everywhere a visibility value is built, replacing the ascending sort.
Give `set_visible_devices` a `visible_devices_mode` (`env`, the default and
today's behaviour, or `args`, which injects no visibility variable at all) and
three new exact-match placeholders, siblings of `${HOST_GPU_IDS}`:
`${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}`, each rendering the
same ordered, deduplicated GPU list as `<prefix><localIndex>` (`CUDA`,
`Vulkan`, `MTL` — llama.cpp's own Metal device name, not "Metal") for use in
`args`. Validate both knobs at save, before any mutation —
`runtime_spec.visible_devices_mode_invalid` for a mode outside `env`/`args`,
`runtime_spec.visible_devices_args_no_placeholder` for an `args`-mode spec
whose `args` mention none of the three placeholders. These two new rules are
portal-only: the agent degrades an unknown or empty mode to `env` rather than
refusing it and applies no args-placeholder check, so the file-mode path the
portal never reaches is not guarded against them. Only the existing conflict
and empty-GPU-list refusals are re-enforced by the agent at launch, and those
apply unchanged in both modes. Declare one agent feature flag for all of it, `gpu_selection`
(`Since: "0.4.0"`), since the order fix and the new mode ship in the same
agent release; `server-agent`'s `Version` moves `0.3.0` → `0.4.0`, MINOR per
the versioning rule ([§7.1](cross-cutting/agent-runtime-manager.md#71-agent-versioning)).
**Consequence:** migration 73 backfills every existing spec's `position` to
its prior ascending-by-`gpu_index` rank, so no already-deployed spec's
emitted order changes on upgrade — order only moves once an operator actively
reorders GPU rows in the portal. Unlike `${MODEL}`/`${HOST_GPU_IDS}`, which
shipped in `runtime_manager`'s own release so no agent that can run a spec at
all can lack them, `gpu_selection` ships two releases later (after
`runtime_config_ack`'s `0.3.0`), so already-deployed older agents that support
the managed runtime but predate it genuinely exist: such an agent silently
re-sorts a custom order back to ascending and fails to launch an `args`-mode
spec (the placeholder reaches llama.cpp as unparseable literal text). The
portal reads `agent_features` and shows a non-blocking "agent too old"
advisory — prominent for `args` mode, since the process would not start at
all; informational for a custom order, since the model still starts, only on
the wrong cards — never a blocked Save. A backend's local device index is its
**own** independent enumeration, unrelated to the host/PCI GPU index an
operator's other tooling reports, so `Vulkan0`/`MTL0` need not be host GPU 0;
an operator verifies the mapping with llama.cpp's own `--list-devices`.
`${METAL_DEVICES}` additionally does nothing useful except against a macOS
host running a llama.cpp build compiled with multi-device Metal support,
which the portal also flags when the placeholder is used against a
non-macOS agent.
→ [Agent-Managed Model Runtime §3.2](cross-cutting/agent-runtime-manager.md#32-placeholders-and-why-no-secret-enters-the-gateway),
[§3.3](cross-cutting/agent-runtime-manager.md#33-set_visible_devices-turning-the-gpu-list-into-an-enforcement),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[Data Model §4](reference/data-model.md#4-migration-history-86-migrations),
[API Surface](reference/api-surface.md#agent-managed-model-runtime).

## ADR-035 — The gateway owns the runtime-spec upstream token
**Context:** a runtime spec can now require its launched child process to
present an API token, and the gateway must authenticate to that same child
with the same token — the reverse of every other secret this system touches.
`${AGENT_ENV:NAME}` (ADR-027) exists precisely so a model secret never
reaches the gateway; `routing.Application.APIToken` is the one accepted
exception, an operator-set, sealed, write-only token the gateway already
decrypts and sends at the edge. The new requirement — a *child-launch*
secret the gateway must also know to authenticate upstream — cannot be
satisfied by either existing mechanism: `${AGENT_ENV:NAME}` never reaches the
gateway at all, so the gateway could not authenticate with it, and
`Application.APIToken` is one value per application, not one per mapping.
**Decision:** the gateway **owns and stores** the token — sealed at rest
with the existing capture cipher, generated with `crypto/rand` when the
operator picks `random`, never returned to the UI — rather than having the
agent generate one and report it up. A per-spec `api_token_mode`
(`off`/`set`/`random`/`app`, migration 74) selects the source; `app` is the
**default**, reusing `Application.APIToken` unchanged, because every mapping
already sends that token at the edge today and a default of `off` would have
silently disabled upstream authentication for every already-authenticated
application on upgrade. The token crosses to the agent in a **dedicated wire
field** (`Spec.api_token`), decrypted only at push time and never persisted
decrypted; a decrypt failure pushes an **empty** token (fail-closed), so the
agent hard-errors at `${API_TOKEN}` rather than launching a child with a
garbled or partial secret. Authentication at the edge and the injected value
at launch are resolved through **one function**,
`routing.SpecUpstreamAuth(spec, app)`, called from both the ordinary
resolver (`resolver.go` `targetFrom`) and the VRAM-benchmark/capacity-probe
Target builders (`internal/gateway/benchmark_runner.go`) — the second call
site closes a real gap a naive implementation left open: those builders read
`Application.APIToken` directly, so a `set`/`random` mapping's own scheduled
benchmark or capacity probe would 401 against its own child. `${API_TOKEN}`
resolution and masking are **new agent-side code**, not a reuse of the
`${AGENT_ENV:NAME}` path, even though both end up recorded as secret spans
feeding the same `masked` wire flag: `${AGENT_ENV:NAME}` reads the agent's
own environment, `${API_TOKEN}` reads a field the gateway put on the wire,
and folding the two together would let one variable's masking rule silently
govern the other's provenance. `${API_TOKEN}` is accepted in **both** Env
values and Args — Env is recommended and is what every documented backend
supports, but Args is not blocked, because the operator's fallback for a
backend that only takes the key as a flag matters more than the process
listing / server-log exposure that placement costs; the portal instead
shows a loud, non-blocking warning, the same trade ADR-027 already accepted
for `${AGENT_ENV:NAME}` in Args.
**Rejected:** an agent-generated token reported upward (would need a new
back-report channel and leaves a window where the gateway does not yet know
the value it must authenticate with); defaulting `api_token_mode` to `off`
(silently disables auth for every existing application on upgrade); teaching
the agent to generate `random` values itself (the gateway cannot then
authenticate without a report round trip, and "nobody sees it" is only true
if the party that never displays it is also the party that stores it);
gating the structural unsupported-backend warning on `application.type`, the
initially proposed design (a `server_agent` runtime spec always belongs to a
`server_agent` application — that field is constant and carries **no**
information about which child model server the spec actually launches, so a
switch on it can never distinguish Ollama from vLLM from llama.cpp).
**Consequence:** the portal's backend warning is necessarily
**general** — shown whenever `api_token_mode != off`, regardless of the
spec's binary — plus an optional, purely cosmetic hint derived from the
binary's own basename (matching `ollama` or `llama-swap`/`llama_swap`) that
appends a sentence to the same banner and never gates whether it renders;
narrowing it further would need a typed child-backend field on the runtime
spec that does not exist today. The honest limitation this decision cannot
lift: **Ollama has no native inbound-auth mechanism at all, LM Studio's
token is a GUI-only toggle, and llama-swap only reads `apiKeys` from its own
YAML** — against any of the three, `${API_TOKEN}` authenticates the gateway
side of the connection while the child never checks it, so the feature warns
rather than secures there. A second accepted gap: the decrypted token
travels the gateway↔agent channel in clear whenever the configured gateway
URL is `http://`, exactly as the agent's own bearer credential already does,
and nothing in this feature adds an enforcement or a warning for that
specifically — `portal.Service` does not hold the gateway's own public URL,
so the operator's TLS posture is depended on, not verified.
→ [Agent-Managed Model Runtime §3.2](cross-cutting/agent-runtime-manager.md#the-runtime-spec-api-token-a-deliberate-one-off-exception-to-no-secret-enters-the-gateway),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[§13](cross-cutting/agent-runtime-manager.md#13-known-limitations-and-accepted-risks),
[Data Model §4](reference/data-model.md#runtime-spec-api-token),
[API Surface](reference/api-surface.md#agent-managed-model-runtime).

## ADR-036 — Runtime probing reuses the per-runtime channel; `Type` drives derivation; only context is durable
**Context:** giving a `server_agent` mapping's own context window and live
request load — one process among several a single agent manages — needed a
carrier, a way to decide WHICH endpoints to probe per backend, and a
persistence answer for two numbers with very different lifetimes: context
size barely changes, active/queue changes every second. **Decision:**
four choices, taken together. (1) **Reuse the existing per-runtime
`RuntimeSample` channel** ([Telemetry
§8.3.2](cross-cutting/telemetry-usage-observability.md#832-shared-ingest-core))
rather than a new array or endpoint: `ContextSize`/`ActiveRequests`/
`QueueDepth` ride the same `runtimes[]` entries that already report
`state`/`pid`/`port`/`restarts`/`gpus`, so every existing contract on that
channel — full-snapshot replace, the absent-vs-empty rule, ingest ordering —
applies unchanged with zero new wire surface. (2) **A new explicit
`RuntimeSpec.Type`** (migration 75; `''` = auto-detect from `Binary`'s
basename, else `custom`) is the single foundation both the metrics endpoint
and the context endpoint derive from (`DeriveProbePaths`), instead of
teaching the agent per-binary heuristics of its own; resolution happens
**gateway-side** (`EffectiveRuntimeSpecType` + `DeriveProbePaths` in
`internal/portal`) and only the RESOLVED, concrete paths are pushed to the
agent — it never re-derives anything, it is handed exactly where to `GET`.
An operator's own `metrics_path`/`context_probe_path` override always wins
over the per-type default for its own field. (3) **Persist only the stable
figure.** Context size is written onto the mapping through the
**pre-existing** `UpdateMappingContextProbe` (provenance `"probe"` — the
same value and the same method the application-level context probe already
used, change-detected, respecting `metrics_locked`), while live
active/queue stays strictly **volatile**, in the in-RAM `RuntimeStatus`
registry that already never touches the database
([§10](cross-cutting/agent-runtime-manager.md#10-runtime-status-volatile-and-a-full-snapshot-every-time)).
Routing and the Models catalog read it live — a new routing-owned
`RuntimeModelStateChecker` adapter for scoring, and `injectRuntimeModelState`
on the portal DTO for the catalog
([§11.7](cross-cutting/agent-runtime-manager.md#117-live-runtime-state-on-the-models-catalog))
— never through a per-mapping active/queue column. (4) **No new lifecycle
state.** A loading instance is surfaced with the existing `StateStarting`
value, and routing gains one new, strictly subordinate preference: prefer an
already-`StateStarting` instance of the requested model over cold-starting a
stopped one, below the dominant already-loaded (`StateRunning`) partition
([Routing & Model Selection
§3](cross-cutting/routing-and-model-selection.md#3-candidate-scoring)).
**Consequence:** the sample's wire shape grew by exactly three additive,
non-`omitempty` ints, so an agent that never probes anything (or that predates
this feature) is byte-identical to before on every OTHER field, and a
consumer sees explicit zeros rather than an absent key to reason about.
Because live load is never written to disk, a gateway restart or a stale
agent shows no per-model active/queue for that mapping (routing falls back to
per-server telemetry) rather than serving a plausible-looking but stale
number — the same posture this feature's own VRAM measurements already take.
Because `Type` drives both probe paths from one field, fixing the type alone
(rather than hand-entering two endpoints) is normally enough, and the portal
always shows the resolved outcome next to the raw override so an auto-detect
result is never a guess. The new `runtime_model_probe` capability flag is
declared unconditionally by any agent that probes at all — the agent gates
none of its own behavior on it — and exists purely so the GATEWAY can tell a
sample that genuinely probed its children from one whose zeros mean "this
agent never fills this field": `ingestTelemetrySample` only writes the
context-probe write-back and only sums `runtimes[]` into the per-server
active/queue aggregate (replacing, never adding to, the legacy top-level
scrape — see below) when the reporting sample's own capabilities name it.
**Rejected:** a durable per-mapping active/queue column written every
telemetry cycle (live load is not a fact worth a row history, and it is
already served by the channel built for exactly that); a second top-level
array for per-model probe results mirroring the agent-wide scraper's own
fields (`runtimes[]` already is a per-model channel keyed by `spec_id`, and a
second array reporting the same specs invites the two to disagree about
which child is running); and a new lifecycle state for "loading" distinct
from `StateStarting` (routing's own loaded-model list already treats
`StateStarting` as not-yet-servable, so a second name for the same fact would
need every existing consumer taught about it for no new information). Left
deliberately unresolved by this decision, and not a regression it caused:
`OP_AGENT_METRICS_URL`'s agent-wide, single-external-endpoint scrape
(auto-detecting vLLM/llama.cpp counter names) is the pre-existing case for a
*classic*, non-managed application; it feeds the sample's **top-level**
`ActiveRequests`/`QueueDepth`, a disjoint field from anything `runtimes[]`
carries, so the two mechanisms coexist on one agent process without
conflict — see [Telemetry, Usage Analytics &
Observability §8.2.6](cross-cutting/telemetry-usage-observability.md#826-optional-inference-server-scraping).
→ [Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[§10](cross-cutting/agent-runtime-manager.md#10-runtime-status-volatile-and-a-full-snapshot-every-time),
[§11.7](cross-cutting/agent-runtime-manager.md#117-live-runtime-state-on-the-models-catalog),
[Routing & Model Selection
§3](cross-cutting/routing-and-model-selection.md#3-candidate-scoring),
[Telemetry, Usage Analytics & Observability
§8.3.2](cross-cutting/telemetry-usage-observability.md#832-shared-ingest-core),
[Data Model §4](reference/data-model.md#4-migration-history-86-migrations),
[API Surface](reference/api-surface.md#agent-managed-model-runtime).

## ADR-037 — The runtime router grows a GET-only per-model `/props` passthrough; the gateway probes through it with the spec's token
**Context:** #52 left an api-key-protected child's `timings_per_token`
verdict undeterminable — the loopback probe carries no credential, and
`401`/`403` are cached as conclusive refusals, not transient failures — a
regression specifically for a `custom`-typed protected child, whose shape
clause opts out with no persisted verdict to override it. The originally
sketched remedy, threading the spec's sealed token into the agent-local
probe, was rejected on two grounds: `runtime.Status` is copied into every
reported and logged place this feature touches, so a credential riding it
would leak into all of them; and the "the agent holds no token" premise that
motivated probing the child directly turned out false anyway — the
runtime-config push already carries the resolved `${API_TOKEN}` value
agent-side (a mismatch between how the agent comments on that value and how
it actually persists it is tracked separately as issue #61, and does not
change this decision's reasoning). The accurate invariant was never about
what the agent holds; it is
that the **router** injects no credential of its own and forwards
`Authorization` verbatim, exactly as it already does for ordinary inference.
**Decision:** mirror llama-swap's `/upstream/{model}/…` shape, with the
guardrails llama-swap itself lacks — GET-only, an allowlist of exactly
`/props`, `Status()`-only resolution that never starts or keeps alive a
child, and the model decomposed from the path by prefix/suffix rather than
segment matching (an upstream id may itself contain `/`)
([Agent-Managed Model Runtime §4.1](cross-cutting/agent-runtime-manager.md#41-control-routes)) —
and let the gateway's `{model}` app-health pass probe through it instead of
over loopback, attaching that mapping's own `routing.SpecUpstreamAuth`
credential, fail-closed-gated on the agent-declared `runtime_upstream_props`
capability so an older agent is never sent a probe its router would just
404 forever
([§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation)).
**Consequence:** `live_progress_support` now has two converging writers — the
agent's own loopback probe over telemetry, and the gateway's probe through
the router — that apply the identical evidence rule and the same
compare-to-stored discipline, so which one's write lands first for a given
mapping is never a contract; the loopback probe stays the fast, generally
available path for every unprotected child and the only path for an agent
that predates this feature, while the router passthrough is what resolves a
protected one. A `401`/`403` on this path is no longer an unresolvable dead
end but a typed, logged operator misconfiguration
(`provider.ErrAuthRejected`, surfaced as "check the runtime spec's API
token"). Widening the allowlist beyond `/props` — `/slots` for #49-2,
router-mode passthrough for #55 — is deliberately left to those issues; this
decision adds exactly the one route #58 needed.
→ [Agent-Managed Model Runtime
§4.1](cross-cutting/agent-runtime-manager.md#41-control-routes),
[§4.3](cross-cutting/agent-runtime-manager.md#43-stable-error-codes),
[§3.2](cross-cutting/agent-runtime-manager.md#32-placeholders-and-why-no-secret-enters-the-gateway),
[§10](cross-cutting/agent-runtime-manager.md#10-runtime-status-volatile-and-a-full-snapshot-every-time),
[Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests),
[API Surface](reference/api-surface.md#53-the-agents-own-router-port-not-a-gateway-endpoint).

## ADR-038 — Capability detection: one `/props` read, three states, an open vocabulary
**Context:** #49 sub-project 2 needed a persisted answer for "does this
upstream take images/video/audio, and does its chat template support tool
calls" — and the two existing precedents on `model_mappings` pointed in
opposite directions. `vision_capable` (migration 32) was a **bool**: its zero
value could not distinguish an observed "no" from "never probed," and the
models list already ANDs it across every mapping serving a gateway model name,
fail-closed by construction — so a single never-probed mapping silently
dragged a whole model's vision flag to `false`, and the portal chat's
image-attachment gate read exactly that aggregate. `live_progress_support`
(migration 76; [ADR-037](#adr-037--the-runtime-router-grows-a-get-only-per-model-props-passthrough-the-gateway-probes-through-it-with-the-specs-token)
later gave that verdict a second writer) was **three-state**
(`""`/`supported`/`unsupported`), written by a dedicated writer that carried no
`metrics_locked` guard and never touched `metrics_source` — because a build
capability, unlike a throughput figure, is not a number an operator vouches
for.
**Decision:** capability verdicts are **detected, never assumed**, and the
detector answers in three states. `detectCapabilities`
(`internal/provider/model_info.go`, byte-for-byte duplicated in
`server-agent/internal/collector/probe.go` under the two-Go-modules-no-shared-code
precedent) reads exactly two objects out of the same `/props` document a
single fetch already retrieves for the live-progress verdict:
`modalities.{vision,video,audio}` and `chat_template_caps.supports_tools`. A
key **present** as a bool is the verdict; a key **absent** is `""` —
undetermined, never a denial, because an older build simply predates the
field — and an undetermined verdict must never overwrite an established one.
The same `"role": "router"` gate the live-progress detector applies holds
here, for the same reason: llama.cpp's router-mode dummy document would
otherwise yield a wrong verdict about the router's own build. The vocabulary
is deliberately **open**: an upstream may report capability names this
codebase has never heard of (Ollama passes manifest-declared names through
verbatim), and those are carried, stored and displayed as-is rather than
dropped. Two caveats travel with the verdicts wherever they are shown, because
both invite a stronger reading than the field supports: `modalities.video`
means the binary was built with video support **and** the model has a vision
encoder, not that the model understands video; `supports_tools: false` means
the model has no native tool-call template, not that tool calls fail (with
`--jinja` a generic handler accepts tools for every model), so it means
degraded prompt quality, never a rejected request.

**Extended by issue #54: this decision now covers TWO detectors over two
documents, and the second one can only ever answer `yes`.**
`detectOllamaCapabilities` (`server-agent/internal/collector/probe.go`) reads
the `capabilities` array of a **`POST /api/show`** response — a SIBLING of
`detectCapabilities`, not a branch inside it, since the two documents share
no field — mapping `vision`/`tools`/`audio` onto the structured fields and
carrying every other name verbatim into the open vocabulary. `POST /api/show`
was chosen over `GET /api/tags`, which would cover a whole server in one
request but needs Ollama v0.30.0 and under-reports `tools`/`thinking` for
models whose template lives only in the GGUF. It lives in the **agent module
alone**, because only the agent probes `/api/show` — a copy in the gateway
would be dead code — and becomes a twin under the same drift discipline the
day the gateway gains its own Ollama probe.
**Only `yes` verdicts, and that asymmetry with the `/props` rule above is the
decision.** Ollama's array is **not exhaustive**: upstream logs
`"unknown capabilities for model"` for an empty result, the JSON field is
`omitempty`, a failed model-file read silently shortens the list, detection
is substring heuristics over the chat template, and one upstream filter
deliberately strips real vision/audio for some builds. Absence therefore
means "Ollama did not tell us", and deriving a `no` from it would encode read
failures, template heuristics and runner quirks as operator-visible denials
that the no-rewrite rule would then keep. Two names get specific treatment
for the same underlying reason — a name must carry evidence to become a row:
**`completion` is dropped**, because upstream *assumes* it whenever a model
has no `pooling_type` rather than detecting it; and **`image` is not vision
and is never folded into it**, because in Ollama's own model that name is
image *generation* (born `CapabilityImageGeneration`, error string "image
generation", required only by `/v1/images/generations`), so it reaches the
open vocabulary under the name Ollama actually used. The probe reports its
own provenance rather than letting the consumer infer it: the sample carries
`source` (`llama_cpp_props` | `ollama_api_show`), which ranks 1 with the
other probe through `capabilitySourceRank`'s default branch — no rank-table
edit — and the ingest boundary accepts **exactly those two probe names**,
voiding the whole pass for anything else, so an agent can never claim
`manual` or `vision_benchmark`
([ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)
owns the rank itself). Deliberately NOT decided here, each for a stated
reason: capability detection for a **directly configured** Ollama
application, which needs a POST-capable gateway probe *and* a fan-out
decision (one endpoint, many models, so one `/api/show` per mapping per
cycle); `/api/ps` as the context source, a different quantity from the model
maximum; and any widening of the agent router's `GET`-only upstream
allowlist.
**Extended by issue #154: a THIRD agent detector over a third document, and
the only one of the three that answers a single capability.**
`ProbeSdcppVerdicts` (`server-agent/internal/collector/sdcpp.go`) reads an
agent-launched `stable_diffusion_cpp` child's `GET /sdcpp/v1/capabilities`
and answers `image` alone, in both directions: `supported_modes` is
exhaustive, so a list holding `img_gen` is `yes`, a list without it is a real
`no`, and a document with no list answers nothing. Its rule is a twin of the
gateway's `parseSdcppCapabilities` (`internal/provider/sdcpp_capabilities.go`),
which reads the same document for an external application, under the drift
discipline above: the two must decide identically on identical input, and a
case table duplicated verbatim in both modules' tests pins both. The sample
names it `sdcpp_capabilities`, which ranks 1 through the same default branch
with no rank-table edit, so the ingest boundary now accepts **three** probe
names rather than two, and still voids the whole pass for anything else. The
third is accepted for the `image` row only: a live-progress verdict or any
other capability claimed under it is dropped row by row, at `Warn`, because
the document cannot have answered it. And unlike the first two, the agent runs
this detector, and sends a verdict it cached from it, only while the gateway
declares the feature `capability_source_sdcpp`, since a gateway that predates
the source would void the pass of every such sample. It asks the gateway at
most once every 30 s, for all such children together
([ADR-044 (e)](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor),
[Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)).
**Consequence:** the detector, its evidence rule and its refusals are what
this decision durably records. **Where the verdicts LAND is no longer this
decision's** — the first shape did not survive contact with the operator's
requirement, and [ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)
records what replaced it. That first shape persisted the verdicts as seven
wide columns on `model_mappings` (migration 77) and, for the one bool actual
consumers read, had a definitive `cap_vision` verdict additionally write
through the lock-respecting `UpdateMappingVisionCapable`, with
`metrics_locked` as the escape hatch. Both halves are gone — the three defects
the operator's requirement exposed are stated once, in ADR-039's Context. What
survives unchanged is that a capability is **not a metric**: no capability
writer consults `metrics_locked` or restamps
`metrics_source`/`metrics_updated_at`. vLLM and TGI also remain uncovered by
capability probing — neither one's reachable HTTP surface exposes a modality
or tool-support field at all, which [Telemetry, Usage Analytics &
Observability §8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)
records with the specifics so the gap is not rediscovered — leaving the vision
**benchmark** their only path to a vision verdict, unchanged by this decision
except in where it writes and what it may overwrite.
→ [Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests),
[Agent-Managed Model Runtime
§10](cross-cutting/agent-runtime-manager.md#10-runtime-status-volatile-and-a-full-snapshot-every-time),
[Data Model §4](reference/data-model.md#4-migration-history-86-migrations),
[API Surface](reference/api-surface.md#models-servers-applications-mappings).

## ADR-039 — Per-model capabilities are child rows with ranked provenance, and the eleven columns are dropped
**Context:** the wide-column shape [ADR-038](#adr-038--capability-detection-one-props-read-three-states-an-open-vocabulary)
first shipped had three defects, and the operator's requirement — "my answer
is the answer" — exposed all three at once. A column's zero value conflated
"no" with "never determined" (`is_mtp = 0` never meant more than "the name
heuristic did not match"). One mapping-wide provenance string could not say
*who* established *which* verdict, so the only precedence the schema could
express was "the probe wins unless the operator locks this mapping's metrics"
— a lock over numbers, pressed into service as a per-capability guarantee it
was never shaped for, and a probe overwrote a hand-set vision answer within
about one telemetry tick until it was locked. And because the upstream
vocabulary is open, a `cap_extra` JSON array had to ride beside the four real
columns as an escape hatch. **Decision:** move every per-model capability
verdict into a child table, `model_mapping_capabilities` (migration 78), and
make provenance the mechanism rather than a lock. **(a) The shape:** one row
per `(mapping_id, capability)` — that pair is the primary key — carrying
`verdict` (`yes` or `no`, and **nothing else**), `source`, and a `not null`
`checked_at`, with `mapping_id` a real `references model_mappings(id) on
delete cascade`. **The absence of a row means UNKNOWN.** That is what turns
"an undetermined verdict must never overwrite an established one" from a
convention every writer has to remember into a structural fact: there is no
empty verdict to write, `ValidateCapabilityRow` — the one function both
drivers call — rejects an empty `capability`, an empty `source`, and any
`verdict` that is neither `yes` nor `no`, and `DeleteMappingCapability` is the
only way back to unknown. The operator reaches it through the MAPPING UPDATE —
`UpdateMappingRequest.capability_verdicts`, a map keyed by capability name,
carried on the same PATCH as everything else rather than on an endpoint of its
own. That placement is structural: the mapping form seeds once and never
re-syncs, so a reset applied by a separate request would be undone by the
operator's next unrelated edit, which would re-establish a permanent `manual`
row with an ordinary 200. The response is therefore post-write truth, and the
form's two controls carry an explicit *unknown* state (an empty select option)
rather than a checkbox's two.

ONE field carries all three states, and that is the point rather than a
convenience. `"yes"`/`"no"` is compared against the STORED ROW — a missing row
counts as different — so an operator moving a control from unknown to `"no"`
writes the `manual` row that stops a later probe from overwriting their
judgement; `""` deletes the row; a value equal to what is stored writes
nothing. The `is_mtp`/`vision_capable` booleans beside it keep the
two-state-FOLD comparison they have always had, and that asymmetry is
deliberate: a boolean can be an artefact of the form having been submitted (an
old cached bundle, a script), so an unconditional `vision_capable: false`
against a mapping with no row must stay inert, while a PRESENT map key can only
be an explicit statement. Collapsing the two rules into one would either
re-open the minting defect or re-close the third state. The map accepts ANY
capability name — the vocabulary is open below — and rejects a blank one, a
value outside the three, a reserved (name, verdict) PAIR, stating a capability
whose legacy boolean the same request also sends, and two keys that name one
capability once trimmed. Its
store error is PROPAGATED, unlike the accompanying upsert's best-effort write:
relinquishing the verdict is the whole effect of the action, so swallowing the
failure would report success for nothing. What is NOT closed is minting one by
accident from a form gone stale mid-edit
([11.1](11-risks-and-technical-debt.md#111-operational-risks)). What is open
is the *vocabulary*, not the validation, with exactly one narrowing added after
the fact: the code reasons about `vision`, `video`, `audio`, `tools`, `mtp`,
`live_progress` and `speculation_observed` while an unrecognised upstream name is
accepted, stored and shown verbatim — which is why the open vocabulary needs no
escape hatch. The narrowing (issue #81) compares the (name, VERDICT) pair against a
short list, never the name alone: `live_progress: "no"`, and either verdict on
`speculation_observed`, may not be stated. A `manual` row is rank 3 and nothing
re-derives it, and for `live_progress` the row is read by the Responses
passthrough gate as a veto — so an operator could permanently and silently
disable their own live-timings switch, on an endpoint they were not thinking
about, with a request that looks entirely reasonable.

Two things are deliberately NOT refused, and both are load-bearing. `mtp` is
absent from every reserved list: it is the one internal name with an operator
control on the mapping form, whose rank-3 permanence is precisely how that
control works. And `live_progress: "yes"` stays accepted, because this row has
two consumers with OPPOSITE semantics — `internal/provider`'s `wantsLiveProgress`
returns true on `"supported"` *ahead of* its shape clause, which covers only
`llama_cpp` and `vllm`. The probes are DOCUMENT-keyed rather than type-keyed, so
they do establish this row for anything answering the configured probe path with
a llama.cpp `/props` document — a `server_agent` child resolved as `custom` but
running llama-server included, which `collector.LiveProgressProbePath` names as
the case its detector exists to recover. What is left without any probe answer is
a tolerant upstream serving no such document (`tgi`, a `litellm` forwarding
elsewhere) or a mapping with no probe path, and for those a manual `"yes"` is the
only opt-in that ever existed. Where a probe DOES answer, `manual` is rank 3
against its rank 1, so allowing `"yes"` is also how an operator overrides a
detector they distrust — what every other manual verdict is for. Refusing it
would have removed both to prevent nothing, since on the Responses side the same
verdict is a veto and permits nothing. A first cut of this narrowing was
name-keyed and did exactly that.

What the refusal costs is recorded rather than hidden, and it is **one
configuration rather than a class**. A manual `"no"` only ever bought something
where something else would otherwise SEND the parameters, and on the translate
path that is `wantsLiveProgress`'s shape clause — `llama_cpp` and `vllm` only.
Off that clause (`llama_swap`, `litellm`, `tgi`, `ollama`, `stable_diffusion_cpp`, `custom`) the absence
of a row already means "do not send", so the pin was redundant; vLLM tolerates
both parameters, so it has nothing to refuse; and for a `llama_cpp` upstream that
genuinely refuses them the `/props` detector writes `"unsupported"` **itself** at
rank 1, off a `params` object lacking `timings_per_token` — the older-build case.

The remainder is a `llama_cpp`-typed upstream that refuses the parameters and has
**no probe path configured**, which is the portal's default. There the pin was
the only durable way to stop paying one wasted round trip per mapping per memo
TTL, since neither rejection path persists anything. That operator gains a better
remedy than the one they lost: configuring the probe path lets the detector
answer at rank 1 and, unlike a manual row, be re-derived when the build changes.
Accepted on that basis — a silent permanent cross-endpoint veto is worse than a
bounded, self-healing round trip on one configuration that has its own fix
([Telemetry, Usage & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)).

The RESET stays open for every name, which is what keeps this compatible with
the paragraph above: a row a pre-narrowing build stored is still relinquishable,
so no row is made uncorrectable. Two verdicts reached the request path
through the candidate query's own **filtered** LEFT JOINs when this decision
was taken; `mtp` and its join went with the scorer's flat bonus, so today
there is one
([ADR-040](#adr-040--the-flat-mtp-bonus-is-deleted-mtp-splits-into-a-declared-trait-and-an-observed-one)). It lands on `MappingCandidate`,
deliberately **not** on `ModelMapping`: a mapping loaded through `MappingByID`
joins nothing, and a struct with no capability field cannot present a
plausible-looking but unpopulated verdict — anything holding only a mapping
has to ask for the rows. **(b) The precedence rule is a RANK,** not a
probe/not-probe split: `manual` 3 > `vision_benchmark` 2 >
`llama_cpp_props`/`ollama_api_show`/`legacy`/**any unrecognised source** 1 >
no row 0, and a write is permitted **iff `rank(incoming) >= rank(current)`**.
`WritableCapabilityRows` (pure, no I/O) applies it in each writer, because only
a writer knows what rank its own evidence carries and it also decides what is
worth writing at all (an unchanged verdict is skipped). But that check and the
write are two store calls, so a rank-3 `manual` committed in between would be
overwritten by a probe that read "no row"; the SQL upsert therefore repeats the
same `rank(incoming) >= rank(current)` inside its `on conflict` as a **backstop**
(`capabilityRankCase`, mirrored in `MemoryStore`, issue #79), and the two
orderings are pinned equal by a test. The Go check is still the primary and the
only one that skips an unchanged write; the SQL guard closes only the
interleaving the Go check cannot see.
Four consequences are load-bearing: an operator's verdict is permanent against
both the benchmark and every probe, with **no `metrics_locked` involved** —
the lock does not guard capabilities at all any more; the benchmark still
outranks every probe and loses only to an operator; an *equal* rank stays
writable, so a probe repairs its own drift after an upstream build changes;
and an unrecognised source ranking 1 fails safe toward "treat it as a probe"
rather than silently handing an unknown writer manual's immunity. `legacy` is
the source migration 78 stamps on a verdict inherited from a column whose real
origin is unknowable, ranked alongside a probe deliberately: treating a guess
as authoritative would freeze it in forever. A write whose verdict AND rank
both already match is dropped, so a capability — stable by nature — costs no
write per telemetry tick; an agreeing verdict at a HIGHER rank still writes,
because the rank is a fact of its own that only a write can change (a
benchmark confirming a probe's verdict has genuinely measured it, and leaving
the row at rank 1 would both misattribute it in the tooltip and leave a real
measurement overwritable by the next probe). Because the reported names are an
open vocabulary, the same rule enforces **one row per capability name** per
write, keeping the first occurrence: every producer emits its structured
verdicts before its open-vocabulary ones, so structured beats unstructured
deterministically instead of by whichever row the store's upsert loop happened
to apply last. **(c) The eleven columns are dropped** (migration 79), and
this is the line the decision draws. This repository's practice is to leave a
superseded column **permanently inert** — migration 72's replacement of the
`native_responses`/`native_messages` booleans left both in the schema ([ADR-033](#adr-033--endpoint-modes-replace-the-native_-booleans-independent-per-endpoint-disable-per-spec-snapshot))
— and this departs from it because the case differs on facts that are worth
stating rather than generalising: nine of the eleven had shipped in the
preceding two days (migrations 76 and 77), and **all** eleven had no reader
outside the one feature this change rewrites — the two older ones,
`vision_capable` (migration 32) and `is_mtp` (the baseline), were read only by
the models-list vision fold (and the portal chat's image gate behind it), the
scorer's MTP bonus, and the portal DTOs and mapping form that display and edit
these very verdicts, every one of which this change reroutes to the table in
the same unit of work. A long-established column with consumers beyond its own
feature still stays inert. **Consequence:** a filtered join keeps one row per
mapping — the `(mapping_id, capability)` primary key guarantees it — and
measured ≈ +6 µs against the query's ≈ 17 µs, where one *unfiltered* join
costs ≈ +79 µs and multiplies rows. There were two of them here, and
[ADR-040](#adr-040--the-flat-mtp-bonus-is-deleted-mtp-splits-into-a-declared-trait-and-an-observed-one) later removed the `mtp` one;
this measurement is what makes that deletion a saving on the request path
rather than a wash. Two read paths cannot use them and read the row
themselves instead: the affinity path (it resolves before the candidate query
and returns its pin early, on a mapping that came from
`MappingsByApplication`) and the benchmark runner (`benchmarkTargetFor`, whose
one read serves a capacity run's whole stream fan-out); all three producers
translate the same row through the same conversion, so none can drift into its
own spelling of "supported". `MappingCapabilitiesForMappings` is this
repository's **first** batch child-collection reader — every other child
collection is an N+1 loop in Go — justified by two multipliers on the
model-servers listing rather than by taste: the SSE stream recomputes the
whole listing on every loaded-registry change, and the model-group endpoint
calls the listing once per group member. Migration 78 *reads* the columns
migration 79 drops, so **their order is load-bearing** and nothing may ever be
inserted between them; a fresh install replays both and must land on the same
schema, with the same rows, as an upgraded database, which is pinned by its
own test. **One discipline this design earned, for whoever adds the next
capability: enumerate every writer and every reader before writing the rule.**
These rows have six writers (two probe paths, the vision benchmark, the MTP
name heuristic at *both* mapping-creation sites, the operator's form, and —
since [ADR-040](#adr-040--the-flat-mtp-bonus-is-deleted-mtp-splits-into-a-declared-trait-and-an-observed-one) — the request
path's own speculation observation) and six readers (the candidate query, the
affinity path, the benchmark runner, the models-list vision fold, the
portal's per-row chips, and the mapping DTO that seeds the operator's edit
form and is compared against on save). Both counts are one per writer or
reader that has a rule of its own, which is deliberately *not* the call-site
count: the six writers are five `UpsertMappingCapabilities` call sites,
because the operator's form and the name heuristic share the portal's single
helper. Enumerate the rules, not the lines. Every rule here is a rule about a
value all of them touch, and a rule written with only its own writer in mind
is what repeatedly failed: a probe outranking a human, a form save laundering
an unchanged submission into a permanent `manual` verdict, a heuristic wired
at one of its two creation sites. The rank a new
writer may claim, and what it must never overwrite, follow from that
enumeration — not from the writer's own point of view.
**Rejected:** keeping the columns and adding a `capabilities_locked` flag (a
second mapping-wide lock, still with no per-capability provenance, and still
asking an operator to pin a value they should simply be able to state); and a
third `unknown` verdict value instead of row absence (it would put back the
empty verdict every writer has to remember not to write, which is the bug
class this shape removes).
→ [Data Model §1](reference/data-model.md#1-current-tables-by-area),
[§4](reference/data-model.md#4-migration-history-86-migrations),
[Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests),
[Routing & Model Selection
§7](cross-cutting/routing-and-model-selection.md#7-model-selection-metrics),
[API Surface](reference/api-surface.md#models-servers-applications-mappings).
## ADR-040 — The flat MTP bonus is deleted; "MTP" splits into a declared trait and an observed one
**Context:** `mtpBonus = 30.0` lived inside `metricTiebreak`
(`internal/routing/scorer.go`), whose only other two terms are the *measured*
generation and prompt throughput — a guess about speed sitting beside a
measurement of the same thing, and its own comment justified it as exactly
that: a false positive "would later bias server selection toward a model that
is **not actually faster**". The guess came from a model NAME
(`IsMTPModelName`'s substring match), was written into an `mtp` capability row
at both mapping-creation sites, joined back onto the candidate, and read once —
by the bonus. **And the repository meant two different things by "MTP", in the
same comment block.** Every *definition* was about the model's architecture:
`routing/mtp.go`'s own list says DeepSeek-V3 "ship an MTP head" and GLM-4.5 /
4.6 "expose MTP", statements about weights. Every *justification* was about
deployment speed. The *placement* sided with the justification, because a peer
of two throughput terms is a speed claim whatever its comment says. The two
readings come apart in practice: a GLM-4.6 GGUF whose weights carry MTP heads,
served by a llama.cpp started without a draft model, drafts nothing at all —
an MTP model that is not speculating, scored as though it were. And the
repository had almost no words for the second reading. "Speculation" and
"draft model" appeared nowhere in it; "speculative decoding" appeared exactly
once, in [Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)'s
aside on why counting SSE deltas as tokens undercounts — a property of a
*stream* there, never of a model or of a deployment — and no identifier named
it. Meanwhile the property the bonus was really claiming is **observable**:
llama.cpp attaches drafted-token counters to the `timings` of a completion it
has just served, on traffic this gateway already relays.
**Decision: (a) delete the bonus.** `metricTiebreak`'s documented bound drops
from `50 + 20 + 30` to `50 + 20`, and `Route` no longer carries the flag at
all — after the deletion there is nothing left to assert at the scorer level,
because the compiler enforces it, which is stronger than a test. What replaces
it is the term that was already beside it: if a route is faster because its
server speculates, `effectiveGenTPS`'s measured tokens per second says so from
the effect rather than from a substring of a name. Because `scoringRoute`'s
fill was the **only** reader of `MappingCandidate.IsMTP` in the repository, the
whole chain went with it — the field, `MTPFromVerdict`, the `MemoryStore`
mirror, and the filtered `mtp` LEFT JOIN in `ActiveMappingsForModel` with its
bind argument, selected column and scan target.
**(b) The two readings are named, and each keeps one.** `mtp` keeps the
ARCHITECTURE reading its definitions always had — "this model ships an MTP
head" — and becomes display and operator-seed only: the name heuristic still
writes it at both creation sites, sourced `legacy`, which is what keeps a
guess overwritable instead of freezing it in, and the operator's three-state
control still overrules it. A false positive now costs a wrong verdict on the
mapping — one an operator can flip — not a wrong route. The DEPLOYMENT
reading, "the endpoint behind this mapping is actually drafting tokens", is a
claim about a different subject, so it gets a name of its own
rather than a redefinition of that one; and the deletion is what makes the two
separable at all, since a single flag that scores cannot mean both.
**(c) That name is `speculation_observed`, and it records an observation, not
a property of the model.** Not `mtp` (a different proposition) and not
`speculative`, which would read as a capability of the model rather than as
something seen of a deployment. It is written from the request path
(`recordUsage`, `internal/gateway/inference_complete.go`) when a relayed
completion's own usage carried `timings.draft_n`. `inference.Usage.DraftTokens
> 0` is the whole test, beyond a non-empty serving mapping id (which is the
row's key): deliberately not the opportunistic-metrics opt-in
beside it (that flag governs whether an application's traffic may move a
mapping's metric NUMBERS, and a capability is not one of those) and
deliberately not the request's status (a positive count can only have been
decoded off a real upstream response, whatever the client-visible outcome).
Source `llama_cpp_timings`, named for the document it read exactly as
`llama_cpp_props` and `ollama_api_show` are and for the same reason, and
ranked 1 through `capabilitySourceRank`'s **default** branch with no case of
its own and no rank-table edit — the `ollama_api_show` precedent: never able
to overwrite `manual` (3) or `vision_benchmark` (2), always able to repair its
own drift at 1 against 1. **It is positive-only, and structurally so:**
llama.cpp emits the counter under `if (n_draft_tokens > 0)`, so with
speculation off the key is **absent, not zero**. There is no wire state that
means "confirmed not speculating", so no code path writes a `no` for it, and
absence proves nothing — a cache hit, a short completion, a stream without its
usage chunk, an error, a non-llama.cpp upstream and a model nobody has yet
routed a request to are all indistinguishable from it, because they are all
the same thing: unknown. The portal carries that caveat on the chip's tooltip
in both languages, and places the chip last, after the traits a build or a
manifest declares. The write happens **at most once per mapping per gateway
lifetime**: an in-memory claim keyed by the serving mapping id
(`Target.RouteID`) is taken — test-and-set in one critical section — before
the store is consulted at all, because the rank rule would drop a redundant
write but asking it requires a READ, and a writer leaning on the rank rule
alone would query the database on every completion, forever, for every
speculating mapping in the fleet. That once-per-lifetime bound is also why the
name is **reserved** at the agent ingest, alongside `mtp` and `live_progress`:
a verdict arriving on an agent's open verdict list lands at rank 1, which TIES
this source and is therefore writable, and the repair that makes a tie safe
for the *cadence-driven* rank-1 writers — the losing writer rewrites its row
on the next tick — is precisely what a writer that never runs twice does not
have. An agent's `no` would otherwise stand until a restart. **That shape is
shared with `mtp`, not unique to this name, and an earlier wording of this
paragraph claimed otherwise — do not restore it.** `mtp`'s rank-1 `legacy`
row is written only for a brand-new mapping (the portal's own create path and
model discovery's reconcile) and nothing re-derives it afterwards
([§11.1](11-risks-and-technical-debt.md#111-operational-risks)), so a
tie-ranked write would never be repaired there either — which is why *both*
names are on the reserved list. The discriminator is the repair left over,
and it runs the other way round: `mtp` has an operator control on the mapping
form, whose rank-3 `manual` row outranks every automated writer permanently
and has to, because nothing re-derives `mtp` even across a restart, while
`speculation_observed` has no control on that form at all and is repaired
only by the next speculating completion after a restart.
**Consequence: the deletion made the request path CHEAPER, and that is the
opposite of the usual trade.** The candidate query keeps one filtered LEFT
JOIN instead of two, and the join it lost measured ≈ 6 µs against that query's
≈ 17 µs base ([ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)
recorded the measurement; the comment in `ActiveMappingsForModel` still
carries it, for the one join that remains). Every resolution of every model
pays that much less, permanently, because a feature was removed rather than
added — worth recording precisely because the usual deletion is argued on
clarity and buys no speed at all. Nothing the new verdict does gives it back:
it is written after the response has already been delivered, best-effort, and
behind the claim, so a speculating mapping costs one capability read and one
write in the whole life of the process — the read is what lets the rank rule
protect an operator's verdict, so it is not optional — and every completion
after that costs a map lookup.
**No capability verdict influences model selection any more** — `scorer.go`
names none, `live_progress` is the one verdict the candidate query still
joins, and it decides what the gateway SENDS upstream rather than which
upstream it picks. **That headline no longer holds, and the part of it that
changed is exactly one thing.** A capability verdict now *excludes* a
candidate: `filterCapable` refuses a mapping that lacks a required capability
([ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)).
What this entry said about *ranking* is still true and is what it was actually
defending — `scorer.go` still names no capability, and `live_progress` is
still the only verdict the candidate **query joins**, because the gate reads
its verdicts through a separate bulk call rather than a second join, so the
per-resolution cost this deletion bought is intact. The distinction to keep:
scoring is capability-blind by design; **candidacy is not, any more**
([Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).
**One discipline this deletion earned, for whoever deletes the next behaviour:
sweep the user-visible prose, not only the code — and sweep every module and
every test, not only the one the deletion lived in.** Removing the bonus took
four separate follow-up fixes, because its claim had been restated in six
places nothing pointed at from the scorer: the comments on the name
heuristic; the reserved-capability-name doc at the **gateway's** agent ingest
**and the mirror of it in the agent's own detector**, plus the test that pins
each of those two; and — found only because a task was explicitly told to
look for it — the **i18n copy under the operator's own MTP control**, which
went on telling them in both languages that resetting the verdict to unknown
would also drop "the MTP bonus in server selection". An i18n string is an
assertion about behaviour, it is the one the operator actually reads, and
nobody greps it ([Theming &
i18n §8](cross-cutting/theming-and-i18n.md#8-internationalization)).
The last three outlived every earlier sweep for two duller reasons, and they
generalise better than the i18n one. **Two live in `server-agent`**, a
separate Go module that imports nothing from the gateway: neither the
compiler nor a sweep run from `gateway/backend` reaches them, so a
cross-module claim has to be swept from the repository root or not at all.
**And two are *test* comments** (one of them is both), which a sweep aimed at
shipped code never opens — yet a test's doc comment is the place a reader
goes to learn *why* a rule exists. Enumerate the places a behaviour was
*explained* the way
[ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)
says to enumerate the writers and readers of a value.
**Rejected:** **llama.cpp's `/slots`**, which cannot answer either reading,
for four independent reasons. Its `speculative` bool is true for **every**
drafting technique, plain n-gram lookup included, so it is neither an MTP
signal nor evidence about MTP heads. The one field that names the technique —
`params`' `speculative.types`, carrying `draft-mtp` — lives under a slot's
`params`, which llama.cpp populates only once that slot has served a request,
so it reads empty on a server idle since boot, which is exactly the moment a
fresh mapping needs a verdict. The endpoint needs the child's own API key,
which this codebase does not always hold (only `/health` is public). And it
answers **501** when it is disabled. Its upstream README is stale about the
response as well, so those four had to be read out of the server's source
rather than its documentation — recorded here so the next reader does not
re-derive them, and so the forward reference to a "`/slots`-based MTP probe"
that once sat on `legacyMTPCapabilityRow` is not written again. — **A `no`
verdict for speculation, in any form:** no observation would justify one
(Decision (c)), and a `no` carried on a probe-ranked source would be a
permanent false claim that only an operator could clear. — **`draft_n_accepted`**,
llama.cpp's sibling counter: an acceptance *rate* is a performance measure
and belongs with the metrics, not with a capability verdict. — **Extending
[ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)
in place instead of writing this entry.** That decision is about the SHAPE of
a capability row — child rows, ranked provenance, the dropped columns — and
nothing here reverses or amends it: the new source needs no rank-table entry
(its own default branch was designed for this), and the deleted bonus is a
*routing* term ADR-039 mentions only in passing, as one of the readers the
dropped `is_mtp` column used to have. What this entry decides is a deletion, a
vocabulary and a rejection, none of which is a statement about the table. The
`ollama_api_show` extension of
[ADR-038](#adr-038--capability-detection-one-props-read-three-states-an-open-vocabulary)
is the counter-precedent and it does not apply: that one added a second
detector reading a second document — the same subject as the decision it
extended — whereas this changes what the scorer reads and what the words mean.
→ [Routing & Model Selection
§3.1](cross-cutting/routing-and-model-selection.md#31-the-score-function),
[§7](cross-cutting/routing-and-model-selection.md#7-model-selection-metrics),
[Telemetry, Usage Analytics & Observability
§8.4.3](cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests),
[Data Model §1](reference/data-model.md#1-current-tables-by-area),
[Agent-Managed Model Runtime
§11.7](cross-cutting/agent-runtime-manager.md#117-live-runtime-state-on-the-models-catalog),
[Glossary](12-glossary.md).

## ADR-041 — A billable measure is a (unit, quantity) pair, never a scalar
**Context:** `usage_events` was built for exactly one kind of request. Its
measure is five token columns, every aggregate over it is a `sum` of one of
those columns, and both readers that turn a request into a physical quantity —
`modeledEnergy`'s Tier 3 and the EWMA calibration in `energy_reconciler.go` —
are denominated in *watt-hours per token*. Issue #70 admits request families
whose natural measure is not a token at all: an image generation is billed per
image, speech synthesis and transcription per second of audio. The obvious
shape is one more numeric column — `units double precision` — and it is the
wrong one, because the number it would hold means something different on every
row and therefore means nothing about the table. **It would be a lie in three
directions at once.** *Aggregation:* `sum(units)` over a mixed population adds
tokens to images to seconds and returns a figure with no dimension, and
neither SQL nor Go would object — the sum is the natural thing to write, so the
column's mere existence is the defect. *Readability:* no reader of a single row
could recover whether a `4` was four tokens, four images or four seconds, so
every consumer would have to re-derive the unit from `req_path` — the endpoint
identity the column was supposed to be recording in the first place.
*Pricing:* a coefficient multiplied into an undeclared scalar is itself
undeclared, so a Wh-per-token number would be applied to a count of images and
yield a plausible, wrong watt-hour figure — worse than no figure, because
nothing downstream can tell it from a real one.

**Decision: the measure is the PAIR `(billing_unit, billing_quantity)`.** Both
columns arrive in the same migration ([v81](reference/data-model.md#4-migration-history-86-migrations)),
because a quantity without its unit is the scalar this entry rejects and a unit
without its quantity records nothing. The quantity is only ever read *through*
the unit: whoever wants a number must first agree what it counts. The
vocabulary is deliberately small and open only at the code level —
`BillingUnitTokens` (`""`), `BillingUnitImage` (`"image"`),
`BillingUnitAudioSecond` (`"audio_second"`) in
`internal/usage/billing.go`. Two named units, not three: `"character"` belongs
to an external price list rather than to this system, and audio.cpp's own
metering signal is a *duration*, so speech and transcription share one unit.

**(a) `""` means token-metered, and it is a POSITIVE assertion — a one-way
one.** This is what makes the pair a true no-op for every row that already
exists: the DDL defaults (`text not null default ''`,
`double precision not null default 0`) backfill the whole of history
*truthfully*, because every recorded request to date really was token-metered.
The asymmetry with `energy_source` is the part worth holding on to. There,
`""` means *not yet processed* and is the reconciler's own work queue, so a
value may be written into it and later replaced. Here `""` is a claim about
what was measured, and nothing may ever *default to* it: a row that arrives
carrying an unknown unit is not a token-metered row, and writing `""` over it
would manufacture the assertion. Reading `""` as "unknown" is the single
misreading that turns this column back into the lie it replaced.

**(b) The XOR is enforced, over SEVEN columns, and a violation is logged rather
than repaired.** `ValidateBillingXOR` requires that a token-metered row carry
no `billing_quantity`, and that a non-token row carry zero in the five token
counts **plus `prompt_per_second` and `tokens_per_second`**. The two rates are
in the invariant deliberately, and they are the non-obvious half of it: they
are not token *counts*, but `ComputeHistogram` bins over non-zero values only
by design, so a stray rate on a non-token row would enter the speed histograms
and there is nothing there that could fail. `recordUsage` validates, logs a
violation at `Error`, and then records the row **unmodified** — silently
repairing the data would destroy the evidence that a producer is wrong, and
dropping the row would lose a request from billing outright.

**(c) A unit may only come from ENDPOINT IDENTITY, never from the response.**
Nine of the twelve `recordUsage` call sites pass a zero `provider.Response`
(every error, timeout, disconnect and admission-rejection path — seven of ten
when this was decided, before the images endpoint added two more of the same
kind), so a
response-derived unit would stamp `""` on a *failed* image request and record
it as token-metered with a zero measure — the exact lie the pair exists to
prevent, arriving through the one path an operator is most likely to inspect.
The unit therefore travels on `usageMeta`, which the call site fills from the
endpoint it is serving, and is left at its zero value by every token-metered
call site.

**(d) There is deliberately NO normalizer.** An unknown unit is
`ErrBillingUnitUnknown`, not a clamp. Clamping to `""` would be the ordinary,
harmless-looking move — `NormalizeSort`'s default a few files away is exactly
that — but here it would assert token-metering about a request that is not
token-metered, which is (a) run backwards. Do not add one.

**Consequence: two code surfaces would each tempt a unit→token conversion, and
both are forbidden.** They are named here because in both cases the conversion
is the *shortest* available change, and neither would fail a test that only
checks for a number.

1. **`modeledEnergy`'s `coeff * ev.OutputTokens`.** The least-effort way to
   make Tier 3 produce a watt-hour figure for an image is to synthesize an
   `OutputTokens` value from the image count. Tier 3 is instead a token-only
   surface: a guard at the top of `modeledEnergy`, keyed on
   `ev.BillingUnit != usage.BillingUnitTokens`, returns the terminal provenance
   `unpriceable` ([Telemetry, Usage Analytics & Observability
   §8.4.4](cross-cutting/telemetry-usage-observability.md#844-energy-attribution)).
   The guard sits inside `modeledEnergy` rather than at a caller because Tier 3
   has **two** entry points — `ComputeEnergy` and `reconcileEnergyEvent`'s
   deleted-server shortcut — and a guard at one of them is not a guard.
2. **The EWMA calibration divisor in `energy_reconciler.go`.** Dividing
   `WhMarginal` by `billing_quantity` would write a per-image number into
   `energy_wh_per_token`, poisoning Tier 3 for every *chat* request on that
   mapping. That guard now carries an explicit
   `ev.BillingUnit == usage.BillingUnitTokens` conjunct rather than leaning on
   `OutputTokens > 0`, which holds only because a producer in another package
   honours an unenforced convention.

**Neither is acceptable, and the reason is not aesthetic: a fabricated token
count poisons `sum(total_tokens)`, which is the quota source of truth.**
`routing.Store.UsageAggregateSince` sums that column per principal per
calendar period, and `PrincipalLimiter.Admit` compares the sum against
`LimitConfig.TokenQuota` ([§8.4.5](cross-cutting/telemetry-usage-observability.md#845-cost-and-currency)).
A synthesized token count is therefore not a display artefact — it silently
consumes somebody's quota and eventually denies their chat traffic with a 429
for images they were never told cost tokens. The XOR's zeroed token columns are
what keep that sum honest, and (b) is why they are enforced rather than assumed.

**Any future per-unit coefficient must carry its own declared unit and apply
only on an exact match.** A coefficient of `0.4 Wh` is meaningless; `0.4 Wh per
image` is not. A mismatch between a coefficient's declared unit and the row's
`billing_unit` must yield **0 Wh and never a wrong Wh** — the same rule as (d),
one layer out. A bare coefficient without a declared unit is precisely the lie
a scalar `units` column would have been, re-introduced on the pricing side.

**Rejected:** **a scalar `units` column** — the Context is the whole argument.
— **A column per family** (`image_count`, `audio_seconds`, …): every new
endpoint family costs a migration, the XOR grows quadratically, and
`usage_events` already carries no foreign keys precisely so it can absorb
history it does not understand. — **Widening `UsageGroups`' `GROUP BY` with
`billing_unit`**, and **summing `billing_quantity` at group level.** Folding by
unit would silently change every existing grouped row's identity, and a summed
quantity across a mixed population is the dimensionless number this entry
rejects. What ships instead is a *disclosure*: a count of the non-token rows —
`NonTokenRequests` on `usage.StatTotals` and `usage.GroupBucket`,
`non_token_requests` on the wire — so a consumer can tell that a token
aggregate covers a subset, without the aggregate itself pretending otherwise
([§8.4.2](cross-cutting/telemetry-usage-observability.md#842-query-stats-groups-time-series)).
Only `UsageGroups` expresses that count in SQL, as a
`sum(case when billing_unit <> '' then 1 else 0 end)` aggregate; `StatTotals`
and `portal.DashboardMetrics` count it in Go over rows they already walk. All
three test against the empty sentinel rather than a list of known units, so the
classification is identical — including for a unit a future backend invents.
— **A second limiter dimension** keyed on `billing_unit`. #70 forbids it
explicitly; the capability gap it leaves (a non-token request consumes
`RequestQuota` and `CostBudget` but never `TokenQuota`) is recorded in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances) and
owned by issue #119. — **`real` for `billing_quantity`**: `double precision`
from the start, per [ADR-005](#adr-005--postgresql-needs-wide-column-types),
so the int4/float4 class cannot recur on a brand-new column.
→ [Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[§8.4.2](cross-cutting/telemetry-usage-observability.md#842-query-stats-groups-time-series),
[§8.4.4](cross-cutting/telemetry-usage-observability.md#844-energy-attribution),
[§8.4.5](cross-cutting/telemetry-usage-observability.md#845-cost-and-currency),
[Data Model §1](reference/data-model.md#1-current-tables-by-area),
[§4](reference/data-model.md#4-migration-history-86-migrations),
[Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks),
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances),
[Glossary](12-glossary.md).

## ADR-042 — The images gate keys on a required capability, and an absent verdict refuses
**Context:** `POST /v1/images/generations` (issue #71) is the first endpoint
whose model requirement is a hard **precondition** rather than a preference. A
chat model handed an image request does not produce a degraded image; it
produces a wrong answer, or an upstream error far from its cause. Capability
verdicts already existed as ranked child rows
([ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped))
with a researched three-state vocabulary
([ADR-038](#adr-038--capability-detection-one-props-read-three-states-an-open-vocabulary)),
and **nothing on the request path had ever refused a model for lacking one**:
the live-timings gate annotates, the models-list fold advertises, and the
scorer does not read a verdict at all. So the gate this endpoint needs is not
one more consumer of an existing filter — it is the gateway's first *excluding*
one, and the axis it keys on is the decision.

Issue #71 proposed that axis be a new **fine API flavor**, `openai_images`.
That cannot work, and the reason is one function: `routing.NormalizeAPIFlavor`
folds **every** `openai*` value to the coarse `openai` that a
`model_mapping`'s application actually declares support for
([Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model)).
A fine flavor therefore has nothing to refuse *with* — it is a label that
survives only as far as the first normalisation. `openai_images` does ship, as
`apiFlavorImages`, and it is exactly that: a label for usage rows, for
`upstreamPath`, and for its own `sessionEndpoint` case. It filters nothing.

**Decision: the gate keys on `inference.Request.RequiredCapabilities []string`,
set by an endpoint handler from its own identity and never from the request
body.** It is nil for chat, responses and messages — which is what makes this
a true no-op on every existing path — and `[]string{routing.CapabilityImage}`
for images. `Resolver.filterCapable` (`internal/routing/resolver.go`) drops
every candidate whose mapping does not carry a `yes` verdict for each required
name.

**The cost comparison is the whole argument, because the rejected shape was the
issue's own.** `RequiredCapabilities` needed **no store change at all**:
`MappingCapabilitiesForMappings` already existed on every driver — chunked in
SQLite, mirrored in `MemoryStore`, wrapped in the generated tracing decorator,
and already carrying an N+1 guard test in `internal/portal` — so admitting it
to `resolverStore` cost one interface line. There is **no new `LEFT JOIN`** and
**no capability field on `MappingCandidate`**: the gate reads verdicts in one
bulk call keyed on the candidate list it already holds, so the per-resolution
join cost stays where ADR-040 left it. A fine flavor, by contrast, would have
had to grow an application-declared support column, a candidacy mirror, a
normaliser exemption and three-driver conformance coverage — **per endpoint**,
because the next one (speech, multipart uploads: issues #68/#69) would repeat
all of it. The capability list generalises instead: those endpoints inherit
this gate, and both of its affinity write guards, by naming their own
capability.

**(a) An ABSENT capability row means unknown, and unknown REFUSES.** This
direction is chosen on evidence, not on taste. `Extra`-sourced rows are
written **yes-only** (`cmd/gateway/app_health.go`), and the Ollama detector
can structurally never emit `no` — ADR-038 records why absence there means
only "Ollama did not tell us". In a real fleet, therefore, almost no mapping
carries a `no` row for anything, so reading unknown as *permission* would
refuse nothing at all: it would reproduce the exact defect the gate exists to
fix, while looking like a gate. **A capability store error refuses too**,
rather than failing open, for the same reason — a filter that opens on a
transient read failure is a filter an outage can switch off.

**The day-one cost is stated here rather than left to be discovered: nothing
in a deployed fleet carries an `image` row, so every image request answers
404 `routing.model_not_capable` until an operator writes one.** The
enablement path already works today with no schema change and no API change —
`{"capability_verdicts":{"image":"yes"}}` on the mapping, a `manual` row,
rank 3. That is a deliberate trade: an endpoint that serves nothing until an
operator says which models generate images is strictly better than one that
routes an image request to whatever chat model answered last. (Amended by
[ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor):
the verdict now serves images only on a route that also declares the
`openai_images` flavor; see the amendment below.)

**(b) `(image, no)` is RESERVED before its writer exists, and that costs
nothing.** `reservedManualVerdicts` (`internal/portal/service_applications.go`)
is keyed on the `(capability, verdict)` **pair**; every other entry reserves a
verdict whose automated writer already ships. This one does not, and reserving
it anyway is free precisely *because* of (a): an absent row already refuses,
so "this model cannot generate images" is saying nothing an operator needs to
say. What it prevents is a **permanent veto** — a `manual` row is rank 3,
outranks every automated source, and nothing re-derives it, so a `no` written
before the capability writer lands would outrank that writer forever against a
genuinely capable model, and clearing it would need a migration that first has
to find such rows. `(image, yes)` stays writable, and is the operator's only
enablement path until the writer ships. Clearing a verdict is never refused —
the reservation is on *stating* the pair, not on the reset.

**(c) A refusal must be legible, and making it so fixed two PRE-EXISTING
502s.** `routing.ErrModelNotCapable` is deliberately not `ErrNoModelRoute`:
"this model cannot do that" and "there is no such model" are different facts,
and a client that cannot tell them apart can act on neither. It maps to code
`routing.model_not_capable` and **HTTP 404**. Folded into the same change,
because a gate that refuses legibly on one branch and illegibly on another is
not a legible gate: **`ErrNoModelRoute` moves 502 → 404 and `ErrNoHealthyHost`
moves 502 → 503.** Both had no case in `completionHTTPStatus` and fell through
to 502, which made a routing refusal indistinguishable from an upstream
outage. **The remap is global** — every inference endpoint, prefixed and
streaming variants included — and it is the one externally visible behavior
change here: a consumer that treats 502 as retryable and 404 as terminal now
sees the second where it used to see the first, which is the correct reading
of both facts.

**(d) `/v1/models` keeps advertising what the router refuses, and that is
recorded rather than fixed here.** `handleOpenAIModels`
(`internal/gateway/server.go`) calls
`Portal.ModelsForFlavor(ctx, token, routing.APIFlavorOpenAI)` — the coarse
flavor **hardcoded at the call site**, not derived from a request — and that
resolves through `modelFlavorSetsWithPreSuppress` → `flavorSetsFromViews`
(`internal/portal/service.go`), neither of which reads a capability row at all.
So an images-only model is still listed to a chat client, and a chat model is
still listed to an images client. **There are two consumers of this gap and
they are different code paths**: the listing above, and the unknown-model
redirect, which reads `ModelOffering.Callable` built by `ModelOfferingFor` —
also called with a coarse flavor
(`NormalizeAPIFlavor(shape.apiFlavor)`, `internal/gateway/inference_handlers.go`)
and also capability-blind ([Routing & Model Selection
§2.2](cross-cutting/routing-and-model-selection.md#22-callable-existing--and-why-the-listing-is-neither)).
Closing the gap means teaching **both** a dimension neither has, which is a
separate unit of work. It is axis-independent (not about images) and is logged
in [§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

**(e) The GROUP path deliberately does NOT return this sentinel.** Inside
`eligibleCandidates`, returning `ErrModelNotCapable` would abort the group
failover walk at the **first** incapable member, so an image request to a
group whose second member is capable would fail outright. The gate therefore
runs as an ordinary filter there, before `live` is taken, and an emptied member
reads as `memberNoMapping` — the same no-leak posture the provisioning gate
above it already uses. The residual is that an image request to an **all-chat
group** answers 404 `routing.no_model_route`, which is honest about not being
an outage but is byte-identical to a typo'd model name. Also in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

**(f) The automatic capability writer is a separate issue — and `image` must
NOT be added to `reservedAgentCapabilityNames`.** That list
(`internal/gateway/agent_ingest.go`) is what the ingest boundary refuses to
accept from an agent. Adding `image` to it would block the very writer this
design is waiting for **before it is written**, and the block would look like
a security decision rather than the mistake it is. The two lists are not
symmetric and must not be kept in step: `reservedManualVerdicts` reserves an
*operator's* `(name, verdict)` pair, `reservedAgentCapabilityNames` reserves a
*name* against an agent, and (b) is a statement about the first only.

**Rejected:** **`openai_images` as a gating fine API flavor** — the Context is
the whole argument; it ships as a label and nothing else. — **A second
`admitPrincipal` call site** for the new endpoint: there is exactly one in
`internal/gateway`, and its own comment records what broke when there were
four; the images handler consumes the shared `inferencePreflight` instead. —
**Gating inside `affinityApplicationStale`**: every rejection there *deletes*
the affinity row, and `AffinityKey.APIFlavor` is coarse, so an image request
declaring a pin stale would delete a chat client's pin. The gate sits in
`resolveAffinity` instead, where its refusal is non-destructive
([Routing & Model Selection §4](cross-cutting/routing-and-model-selection.md#4-route-affinity)).
— **A `route_affinity` migration** to make the key capability-aware: the two
pin-*creating* writes are guarded on a non-empty required list instead, which
needs no schema change and which #68/#69 inherit. — **A new runtime kind, an
`images_mode` application column, and a wider `captureMaxBytes`**: each would
have made an endpoint-shaped fact into a stored one. `sd-server` launched under
the existing `custom` kind ([Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#a-worked-sd-server-launch-under-stable_diffusion_cpp),
since reversed by ADR-044);
images has no per-application mode to read at all, which is why
`endpointModeFor` deliberately has no case for it; and the capture cap stays
1 MiB, so a large base64 response is captured truncated while the billable
count is scanned off the full byte stream regardless.

**Amended by [ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)
(`openai_images` becomes a coarse flavor, and stable-diffusion.cpp a type of its
own).** The capability gate, its fail-closed direction, (c), (e) and (f) stand
unchanged. Five statements above no longer describe the code. The Context's
"folds **every** `openai*` value to the coarse `openai`" and "It filters
nothing": `NormalizeAPIFlavor` now folds `openai_images` to itself, and
candidacy excludes an application that does not list it. The rejection of
`openai_images` "as a gating fine API flavor" still holds for a *fine* flavor;
what gates is a coarse one, which needed no per-endpoint column and no new
filter case — the cost comparison above was against the fine shape.
(b)'s "before its writer exists": `sdcpp_capabilities` now writes `(image, no)`
for an external `stable_diffusion_cpp` application, and the reservation stands
for exactly the reason (b) gives. And the rejected "new runtime kind": `sd-server`
now launches under the runtime spec type `stable_diffusion_cpp`. And (a)'s
"enablement path already works today", a `manual` `image: yes` verdict and
nothing else: a verdict now serves images only on a route that declares
`openai_images` — the application, and for an agent-launched child its runtime
spec too — and a route without the flavor answers 404 `routing.no_model_route`,
not `routing.model_not_capable`. Every image upstream is given the flavor
explicitly, which the `stable_diffusion_cpp` type's default does, and no
upgrade step adds it to a route that served images through `openai` before the
split (ADR-044's consequence). The day-one cost changed shape with
it: an external stable-diffusion.cpp application gets the flavor from its
type's default and its `image` verdict from that probe, while an agent-launched
one needs an operator for the flavor and gets its `image` verdict from its own
agent's read of the same document
([ADR-044 (e)](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)),
except before it has first run. (d) is narrowed rather than closed — see
ADR-044 (f), and
[ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec),
which narrows it again by taking the listing's flavors from the runtime spec.
And the
affinity premise of the Rejected "gating inside `affinityApplicationStale`"
no longer holds for images: `AffinityKey.APIFlavor` is still coarse, but the
coarse flavor of an image request is now `openai_images`, so its key never
matches a chat client's and it could not delete a chat pin. Both affinity
guards stay, keyed on the capability list, for a future capability-carrying
endpoint that does share a coarse flavor with a text one; for images they are
redundant, and the write guard only keeps image traffic pin-free
([Routing & Model Selection §4](cross-cutting/routing-and-model-selection.md#4-route-affinity)).
→ [API Compatibility & Inference
§3.4](cross-cutting/compatibility-and-inference.md#34-openai-images-generations),
[§13](cross-cutting/compatibility-and-inference.md#13-errors),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[§4](cross-cutting/routing-and-model-selection.md#4-route-affinity),
[§8](cross-cutting/routing-and-model-selection.md#8-errors-and-http-mapping),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances),
[HTTP API Surface
§1](reference/api-surface.md#1-inference--compatibility-endpoints).

## ADR-043 — The portal image turn: the model is the affordance, the kind is pinned to the thread
**Context:** [ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)
built `POST /v1/images/generations` and its capability gate, and nothing in the
portal could reach it — the run executor spoke one URL, and the verdict that
admits a model had no writer on any screen, so the endpoint served nothing and
no operator could change that. Closing the gap is not one feature but a handful
of decisions, each with a cheaper obvious answer that is wrong in a way no test
would catch. **Two structural facts drive all of them.** The endpoint is
buffered by construction — `stream: true` is refused outright
([API Compatibility & Inference
§3.4](cross-cutting/compatibility-and-inference.md#34-openai-images-generations))
— so a portal image turn emits **zero** incremental events across a wait
measured in minutes. And the artifact has to land in a chat transcript that is
one sealed document under a hard 4 MiB ceiling, which images, and essentially
nothing else, can exhaust.

**(a) The picked model is the ONLY affordance, and a model whose `image`
verdict is `yes` makes every turn in that thread an image turn.** No mode
toggle, no "generate an image" button, no sniffing the prompt for intent. This
mirrors how `vision` already works: `portal.ModelDTO.Vision` is AND-folded
across a mapping's servers in `modelsResponse` and the attach control is
enabled or not from that single flag; `image` folds identically and the
composer follows it. One axis is the point — the composer can then never offer
something the gate will refuse. (Amended by
[ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)
(f): the fold now also requires each mapping's route to declare
`openai_images`, because once that flavor gates candidacy the verdict alone no
longer makes the endpoint serve a model; that is what keeps this sentence
true.)

**Rejected: a mode toggle.** A second, independent axis of user intent makes
reachable a state the capability gate must then refuse **at the bottom of the
stack, after the user has already committed a prompt**, and it forces a second
question — what happens when the toggle is on and the model says no? — whose
only good answer is to disable the toggle, which is this decision with extra
steps.

**Consequence: `vision` and `image` are one word apart and point in opposite
directions, so the attach control stays gated on `vision` and its refusal had
to be reworded.** An image *generator* has no reason to accept an image
*input*, so an image-only thread whose mapping does not also declare `vision`
correctly has attachment disabled — but the tooltip said "this model does not
support images", about a model whose entire job is images. The generic string
now names what it actually gates, image **input**, and an image-generating
model gets its own sentence saying so explicitly
([§11](cross-cutting/compatibility-and-inference.md#11-multimodal-images) exists
to keep the two axes from being folded into one). **And the verdict
finally has a writer:** ADR-042 (b) left `(image, yes)` writable on purpose for
this, so the mapping form's capability write loop and the model-servers
capability display both carry `image` now. `(image, no)` stays reserved — the
UI writes the enabling verdict, never the refusing one.

**(b) The thread's kind is PINNED at its first send, and it is a UI constraint
— not an authorization.** (a) is a property of the **thread**; implemented
naively it is a property of the **currently-picked model**, which the user may
change between every turn. The gap is worse in the direction nobody expects.
Switching an image thread to a **text** model trips the role-blind
history-has-image guard and is refused — accidentally correct, and
unexplained. Switching it to a **vision** model *lifts* that guard, and the
thread's entire multi-megabyte image history is then POSTed to
`/v1/chat/completions` as vision input: a real cost and privacy surprise, on a
thread the rule called image-only. So the kind is persisted on
`portal.ChatRunSettings` as a `kind` **string** (not an `image_only` boolean:
the same value selects the request URL, and it extends to a future audio or
speech kind without a second flag), `omitempty` so every pre-existing chat
stays byte-identical on the wire, and the model picker filters to the thread's
kind rather than the picker deciding the thread's.

**The pin must be re-imposed server-side, and the obvious hook is not enough.**
`PrepareChatRun` never reads the persisted settings blob — it **replaces** it
wholesale with the settings submitted on this request — so a kind merely
written into the stored document is silently unpinned by the next send. It is
therefore lifted out of storage *before* that overwrite and forced back onto
the submitted settings.

**And the force cannot be conditioned on the stored kind being non-empty.**
Because the field is `omitempty`, a text thread stores **no** `kind` key and
reads back `""` — byte-indistinguishable from a thread that has never been
sent. A `stored.Kind != ""` guard therefore pins `image` and **cannot pin
`text`**: a client submitting `{"kind":"image"}` on a text thread's second send
would flip it, and its next turn would go to an endpoint that carries no
history at all, silently discarding the conversation. Distinguishing the two
cases needs a separate signal for *has this thread been sent before* — the
presence of messages in the stored document, captured **before** this send
appends to it. The residual is named at the code: a chat created with messages
already inside its client-supplied content would read as already-sent on what
is logically its first send. No call site seeds messages on create today; the
day one does, the heuristic has to become an explicit marker.

**The pin is a UI constraint and nothing more, and that is a requirement rather
than a nicety *because* the field is client-settable.** `startRunRequest.Settings`
is `portal.ChatRunSettings` **verbatim**, so every field added to that struct
becomes settable over the API the moment it exists — a first send can submit
any kind. A thread pinned to `image` whose mapping later loses its `image`
verdict (operator revoked, mapping changed) must fail the capability gate
exactly as an unpinned request would. The pin decides what the **composer
offers**; `Resolver.filterCapable` decides what the **gateway serves** and
remains the only authority ([Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).

**(c) The image is stored INLINE in the sealed transcript, and the 4 MiB
whole-document ceiling is accepted for v1 rather than worked around.** It is
persisted as an OpenAI-style `image_url` content part carrying a `data:` URL —
the identical shape an uploaded vision image already has, so history
construction feeds it back as a vision input on the next turn for free and the
existing renderer applies to it. This is policy-compliant rather than a hole in
the no-persist rule: chat transcripts are already among what the capture
encryption key seals ([Security, Authentication & Authorization
§13](cross-cutting/security-auth-rbac.md#13-secrets-at-rest)), so an inline
image inherits that guarantee instead of escaping it.

The cap (`portal.MaxChatContentBytes`) is measured against the **whole
pre-gzip document**, so every image in a thread shares one budget for the
thread's life and a busy thread runs out. That is accepted and **made loud**
rather than lifted: a failed terminal commit now ends the run as an **error**
instead of being logged while the run reports success
([§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground)),
which closes a silent data loss in which a too-large image turn rendered in the
browser and then vanished on reload. Lifting the ceiling is issue #124 and is
recorded in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

**Rejected: a separate blob store for v1.** It is the right end state and it is
a schema change across three drivers, an owner-scoped serving endpoint, a
delete cascade and a re-litigated sealing decision — none of which this feature
needs in order to work. Filed as #124. — **Rejected: a server-side re-encode to
fit the cap.** The client already downscales its **own uploads**; re-encoding
what the *model produced* is a different act, because it silently degrades the
artifact the user asked for. Upstream bytes are stored as-is, with a real
binary download so the user can keep the original.

**(d) The composer leads with the one number that is EXACT, and shows no
progress at all.** Four independent composer designs all spent their effort on
the single quantity nobody can bound — progress — and all four withheld the one
that is exact and knowable *before the user commits*: the remaining transcript
budget. So the composer states the room left in this chat, computed from the
client's own document size against the served cap, and **Send refuses up front**
when another image cannot fit. That is the repository's own rule applied one
layer up: `validateImagesRequest` answers 400 to a `response_format` it cannot
honour rather than relaying and mis-measuring, and spending minutes of
diffusion CPU on an artifact we can already prove we cannot store is the same
error. Every panel design surfaced the cap only *afterwards* — toasts, chips,
download rescues — which is four recovery mechanisms for a failure that can
simply be declined.

**The cap is SERVED, never duplicated.** `MaxChatContentBytes` was package-
private and no DTO carried it. A second `4 << 20` literal in TypeScript would
drift from the Go constant with nothing to catch it, and the failure mode is a
capacity line that confidently states the wrong number — the exact defect the
line exists to prevent. It rides on `ChatListResponse.max_content_bytes`, on
the listing the chat view already fetches, and it is deliberately **not**
`omitempty`: a missing field and a zero are the same thing on the wire, and the
portal reads zero as *capacity unknown* (no line, no refusal), so an accidental
zero disables the feature instead of inventing a number.

**During the wait, three elements, each backed by a value that exists:**
liveness from the run's own server-reported status; an elapsed clock from the
run's **server-measured** age, labelled as the wait rather than the work; and
one static sentence stating that there are no intermediate messages — the image
arrives finished or not at all. The age is measured by the server because a
client cannot measure it honestly: a reopened or second tab never witnessed the
moment of Send, and every view has to show the same true number. The clock is
`aria-hidden`, because the transcript is an `aria-live` log and a
once-per-second announcement would make the thread unusable with a screen
reader.

**The character counter is ABSENT, not zero.** An image run streams no text
either, so reusing the text pending state would render "0 characters" from the
first millisecond to the last — a *measured* zero where nothing was ever
measured. The distinction is not invented here: `proxyNative` already hands a
buffered relay a **nil** progress rather than an always-zero struct, for
precisely this reason, and a progress bar, a percentage or an ETA over an
endpoint that reports none of them would each be the same fabrication in a
louder form.

**(e) The run deadline applies to the IMAGE kind only.** A bound is what turns
an open-ended wait into one the UI can make a true promise about, and without
it the likeliest real failure is a user cancelling a run that would have
succeeded. It is applied in `executeRun` — the first point at which the run's
kind is known — as a child of the reservation's context, so Stop still cancels
it and the ordinary terminal path releases the timer.

**Bounding every run instead would be one branch fewer and a behaviour change
to an existing, overwhelmingly common path.** A text run streams deltas, so the
user can see for themselves that it is alive and the honesty argument does not
apply to it; a process-wide ceiling would newly kill long generations from a
slow local model that complete perfectly well today — a regression to an
existing feature arriving as a side effect of the image feature. If the text
kind should be bounded, that needs its own decision.

**Two traps make this more than a one-line change, and both are recorded
because both are silent and both would return under a refactor.**

1. **A timeout must not be reported as a user cancel.** `executeRun` turned
   *any* context error into the `canceled` terminal with an empty message, so a
   firing deadline would be indistinguishable from the user pressing Stop — the
   system telling someone who pressed nothing that they pressed it. A timeout
   carries its own terminal code on both branches it can land on: before the
   upstream answers, and mid-response once it has.
2. **The terminal commit must not inherit the deadline that ended the run —
   and must not therefore be unbounded.** The terminal step is called with the
   run's own context on every failing path, so once that context carries a
   deadline the `CommitAssistant` inside it is cancelled by the very timeout
   that ended the run — losing the turn instead of recording why it ended. The
   commit therefore runs on a context stripped of cancellation. Stripping
   alone, though, buys the opposite failure: `run.finish` and
   `chatRunRegistry.retire` both sit *below* the commit, so a store write that
   never returns leaves the run `running` with nothing to evict it and Stop
   unable to reach it — and, with `PUT /api/portal/chats/{id}` now refused
   while a run is active ([API Compatibility & Inference
   §12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground)),
   the chat unsaveable and unrenameable until a restart. So the context is
   **stripped and then given a fresh bound of its own**,
   `context.WithTimeout(context.WithoutCancel(ctx), chatRunCommitTimeout)`:
   the run's expired deadline still cannot reach the commit, while a hang
   becomes terminal instead of permanent. 30 s — two orders of magnitude above
   a legitimate 4 MiB sealed write, 20x below `imageRunDeadline`, equal to
   `runEvictionDelay`.

   **And the commit is not the only write that can strand a run, so the bound
   is applied at BOTH sites.** The periodic checkpoint
   (`consumeRunStream`) writes to the same store on its own
   `context.Background()`, and the `finish:` label **joins that goroutine
   before** calling the terminal step — so a checkpoint that never returns
   means the commit is never even entered and its bound cannot help. Bounding
   only the commit would leave the wedge fully reachable. The checkpoint is
   therefore bounded by the same `chatRunCommitTimeout`; its error is
   discarded exactly as before, because a lost checkpoint is recoverable by
   the next tick or by the commit.

   **The polarity is the reverse of what this ADR's other bounds suggest:
   TEXT is the exposed kind here, not image.** Only the text executor
   checkpoints — `executeImageRun` makes one buffered request and has no
   periodic write at all — so a hung store reaches a text run by two routes
   and an image run by one. The bound applying to text runs is therefore
   required rather than merely tolerated; a kilobyte text write that has not
   returned in 30 s is already pathological, so nothing legitimate is cut
   short ([§11.1](11-risks-and-technical-debt.md#111-operational-risks)).

**Rejected: reusing the streaming executor for the image kind.** It opens a
scanner over a delta stream, drives the periodic checkpoint goroutine and
computes TTFT, chars/s and tokens/s — every one of which assumes deltas that do
not exist here. A checkpoint would write a `pending` assistant turn with empty
content that the terminal commit then has to replace, and every rate it
computed would be invented rather than measured. The two halves share the
reservation, the deadline, the loopback header set and **one** terminal commit
function, and nothing else. — **Rejected: `requireWebAnyScope` on the images
endpoint.** A one-line change that grants strictly more than the feature needs:
the browser never calls this endpoint, only the run executor does, and
admitting the cookie leg would make `/v1/images/generations` directly reachable
from a logged-in browser session — falsifying the auth ladder in
[§2](02-constraints.md) as a side effect of a feature that never wanted it. The
narrow loopback-or-bearer helper keeps the documented boundary literally true
and still admits the executor ([Security, Authentication & Authorization
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance)).
→ [API Compatibility & Inference
§3.4](cross-cutting/compatibility-and-inference.md#34-openai-images-generations),
[§11](cross-cutting/compatibility-and-inference.md#11-multimodal-images),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Security, Authentication & Authorization
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance),
[§13](cross-cutting/security-auth-rbac.md#13-secrets-at-rest),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Constraints](02-constraints.md),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances),
[HTTP API Surface](reference/api-surface.md#tokens-chats-usage).

## ADR-044 — stable-diffusion.cpp is a first-class type, and `openai_images` is a coarse, opt-in flavor
**Context:** [ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)
built `POST /v1/images/generations` on a capability gate and kept
`openai_images` a label, because `NormalizeAPIFlavor` folded every `openai*`
value into `openai`. That left one direction open: a mapping's `image` verdict
switched images **on**, and nothing switched text **off**. Registering a
stable-diffusion.cpp `sd-server` under a borrowed type was wrong in four ways,
three of them measured against a real server: the stock `health_path` mode
probes `/v1/health`, which it does not serve, so the application was
unreachable after one cycle; the stock 30 s `timeout_ms` leaves too little
headroom, because a 512x512 image measured about 17 s (about 20 s on the first
request after idle) and the server's own limits permit far larger and slower
generations; and `/v1/models` names the static placeholder `sd-cpp-local`,
which the server does not dispatch on, so the mapping, the Runtime view and the
Activity list all showed that placeholder. The fourth follows from the routing
code rather than a measurement: the stock flavor pair made it a text
candidate.

**Decision (a): `openai_images` is a COARSE flavor, and it is opt-in
everywhere.** `NormalizeAPIFlavor` tests the images prefix before the generic
`openai` one, so the flavor folds to itself, and `applicationServesEndpoint`'s
ordinary default branch then admits only an application that lists it — no new
filter case, no endpoint mode, no schema change, since `api_flavors` already
existed on applications and runtime specs. An empty list on save still
becomes exactly `[openai, anthropic]`, and so does an absent one, except on an
application update, which keeps the stored list. That is the migration-safety
invariant, and it is load-bearing: an empty default that gained `openai_images`
would make every existing text-only application a candidate for image requests
it cannot serve
([Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model)).

**Consequence: an upgrade adds `openai_images` to no route, so image
generation configured through an `openai` application stops until an operator
ticks the flavor.** Serving images now takes the `openai_images` flavor as well
as an `(image, yes)` verdict: on the application, and for an agent-launched
child on its runtime spec too, since (b) holds the images relay to the spec's
own list. Before the split an application could not declare `openai_images`,
and the images endpoint folded images into `openai`, so an `(image, yes)`
mapping on an `openai` application served images. After the upgrade that model
answers 404 `routing.no_model_route` (or, for a token with the unknown-model
redirect on, the request goes to that token's image-capable fallback) until
the operator ticks `openai_images`
on the application and, for an agent-launched child, on its launch spec; the
application form and the launch-spec form both offer the flavor. No migration
does this automatically. That is a decision, taken because no deployment
configured image generation through an `openai` application. Every image
upstream is therefore given the flavor explicitly, by the `stable_diffusion_cpp`
application type's default or by the flavor checkbox; the launch-spec type sets
no flavor, so an agent-launched child needs the checkbox unless its parent
application lists only `openai_images`, which a spec's first write inherits.
(Amended by
[ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec)
(g): the backend still sets no flavor from the type, but the launch-spec form
ticks `openai_images` alone, with both endpoint modes `disabled`, when an
untouched spec's Type is switched to `stable_diffusion_cpp`, and restores what
it replaced on a switch back to an explicit non-sd type.)

**(b) Flavor exclusion stays two-staged, and the images relay joins the second
stage.** Candidacy filters on the application's flavors; a `server_agent`
mapping's narrower spec flavors are enforceable only after resolution, by
`targetServesFlavor`. That check ran only inside `tryProxyNative`, and the
images relay calls `proxyNative` directly, so an agent-managed child whose spec
lists only text would still have served an image request. The relay now applies
the same conjunct and refuses with 404 `routing.no_model_route`. Like
`tryProxyNative`'s check, the refusal is **not retried** against another
application that could serve the model
([API Compatibility & Inference §6](cross-cutting/compatibility-and-inference.md#6-endpoint-modes-and-native-passthrough)).
The reverse direction gets a narrower check. The text translate dispatch,
which every `/v1/chat/completions` request takes, read no spec flavors either,
so an agent-managed child whose spec lists only `openai_images` would have
been sent chat requests under a parent that keeps `openai` for its text
children. That dispatch now refuses an **images-only** target (`openai_images`
and neither text flavor) with the same 404, not retried either — and only
that: a spec that keeps a text flavor is deliberately not held to its list
there, because every such spec served chat completions before this flavor
existed ([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).

**(c) `stable_diffusion_cpp` is an application type whose value is its
defaults** — `model_sync` health, `/sdapi/v1/sd-models` with the
`sdcpp_models` format, `timeout_ms` 600000, `["openai_images"]`, both
coding-agent endpoint modes disabled. Three of them answer three of the traps
above: `model_sync` the missing `/v1/health`, 600000 the timeout, and
`["openai_images"]` the flavor pair. The fourth, the placeholder name, is
answered by type-derived discovery (below), not by a field; the loaded-models
path and format let the loaded probe parse the same listing, and the two
coding-agent modes are disabled because the server serves neither endpoint.
It dispatches to the shared OpenAI-compatible client, because the images relay
is a native passthrough and the Ollama client cannot proxy natively. **Model
discovery is derived from the type** (`provider.modelDiscoveryFor`) and reads
`/sdapi/v1/sd-models`, which reports the real model. It is deliberately **not**
derived from `loaded_models_path`: that field answers what is loaded right now
and has a stock value for most types, and discovery read from it disabled every
model that was merely not loaded — permanently, because reconcile never
re-enables a mapping it already has. Discovery stays fail-closed, and shares
only the name extraction with the loaded probe, so the two agree on a model's
name by construction. **"Loaded" for this type means "the server answers"**,
because the server's capability document carries no residency field; a
`loaded_only` group cannot constrain an sd mapping
([API Compatibility & Inference §8](cross-cutting/compatibility-and-inference.md#8-provider-clients)).

**(d) The runtime spec type `stable_diffusion_cpp` reverses a recorded
decision.** The runtime chapter chose `Type: "custom"` over a dedicated kind,
because the kind would buy only auto-detection and a label. The kind was
requested explicitly, and it now carries behaviour: whenever a spec's
`health_path` is empty the backend picks it from the spec's effective type, and
`/v1/models` for this kind removes the `/health` 404 that got every such child
killed; the form moves an untouched field and shows `sd-server`'s own argument
shape; and detection recognises the binary. `custom` with an explicit health
path still works
([Agent-Managed Model Runtime §3.4](cross-cutting/agent-runtime-manager.md#a-worked-sd-server-launch-under-stable_diffusion_cpp)).

**(e) `sdcpp_capabilities` is the writer ADR-042 (b) reserved `(image, no)`
for, and it now has two producers.** The gateway's health loop reads an
**external** application's `/sdcpp/v1/capabilities` directly; an
**agent-launched** child's own agent reads the identical document behind the
agent's router — which the health loop cannot reach, since the router passes
only `/props` through per model — and reports the verdict up the telemetry
channel under the same source name, but only while the gateway declares the
agent feature `capability_source_sdcpp` (issue #154). Either way
`supported_modes` is exhaustive, so it is the only source that answers
`image` in both directions, it ranks 1 like every probe so an operator's
verdict always wins, and the health loop's copy uses the document's
`model.stem` only to attribute a verdict to the mapping of that name — the
stem is not stored. The one remaining gap is a spec that has never run: the
images gate needs a `yes` verdict before the router will even start an
agent-launched child for an image request, and a child that has never run has
produced no document for either reader to have read. An operator's manual
`image: yes` still covers that first request, and it still wins forever after
by rank
([Routing & Model Selection §2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).

**(f) The portal listing carries `openai_images`, and `/v1/models` does not.**
The listing kept only `anthropic` and `openai`, so an images-only model reached
the chat with no flavors and no sd model was selectable at all. The listing's
known set now includes `openai_images`, while the seed fallback stays
text-only and `/v1/models` still asks for `openai` alone, so an external
client's chat picker is unchanged. The listing's `image` now requires, besides
the `image: yes` verdict, that the mapping's route declare `openai_images` —
the application's flavors and, for an agent-launched model with a spec, the
spec's — so a model the images endpoint would refuse is never an image model
in the portal. The portal chat offers a model that carries `openai`, or
`openai_images` together with `image: true` — the only images-only model the
gate would serve. ADR-042 (d)'s gap therefore narrows:
an images-only model on an application declaring only `openai_images` is no
longer advertised to chat clients, but the per-flavor filter behind
`/v1/models` and the listing's `flavors` field still read application flavors
rather than a spec's (only the `image` flag reads the spec), and `/v1/models`
still reads no capability row. Its second
consumer, the unknown-model redirect, asks the offering with the request's
required capabilities: for an images request a candidate must also carry
`image` by the listing's own image fold (`ModelOffering.Capable`), so a
fallback that generates images is taken and one the capability gate would
refuse is skipped rather than turned into a `model_not_capable` about a model
the client never named.

**Amended by [ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec)
(the listing's flavors read the spec).** (f)'s capability half stands: the
listing's `image` still requires an `image: yes` verdict and a route that
declares `openai_images`, the portal chat's offering rule is unchanged, and an
images request's redirect still takes only a `Capable` candidate. Its
listing-flavor half is superseded: "the per-flavor filter behind `/v1/models`
and the listing's `flavors` field still read application flavors rather than a
spec's" no longer describes the code. Both, and the offering the unknown-model
redirect reads for every flavor, now ask `routing`'s one flavor rule, which
takes a `server_agent` mapping's flavors and messages mode from its spec when
it has one. So an images-only child on an application that also declares
`openai` is no longer listed to chat clients or taken as a chat redirect
target, a name that no flavor serves lists `flavors: []`, and `/api/v0/models`
lists what `/v1/models` lists. What stays is that `/v1/models` reads no
capability row, and that an images client reading it still sees the chat
models; ADR-042 (d) is narrowed again, not closed.

**Rejected:** **discovery from `loaded_models_path`** — the consequence in (c)
was measured, not predicted. — **`openai_images` in the empty default** — (a).
— **An images endpoint mode** — the flavor already expresses "serves images, or
not", and images have no translate fallback for a mode to choose. —
**`openai_images` in the application form's default flavor list** — that list
seeds every new application whatever its type, so every one would become an
image candidate; the flavor is opt-in, set by the type's own defaults or by its
checkbox, which nothing ticks by default.
→ [Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model),
[§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[API Compatibility & Inference
§6](cross-cutting/compatibility-and-inference.md#6-endpoint-modes-and-native-passthrough),
[§8](cross-cutting/compatibility-and-inference.md#8-provider-clients),
[§9](cross-cutting/compatibility-and-inference.md#9-model-discovery),
[Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation),
[HTTP API Surface](reference/api-surface.md#application-type-api_flavors-and-loaded_models_format).

## ADR-045 — A model listing advertises what dispatch serves: one flavor rule, read from the spec
**Context:** every model listing took a model's flavors from its
**application** (`perNameFlavors` over `view.app.APIFlavors`), while dispatch
for a `server_agent` mapping with a runtime spec takes them from the **spec**
(`Resolver.targetFrom`), and the text translate dispatch refuses an
images-only target with 404 `routing.no_model_route`
([ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)
(b)). So an agent-launched `sd-server` child with the spec `[openai_images]`
was offered as a text model by the portal chat, `/v1/models`,
`/anthropic/v1/models` and the unknown-model redirect; every text request to
it failed, and nothing warned the operator (issue #160). The portal's Models
view's "Available via" column showed the same application-wide set on every
child, so on an application `[openai, anthropic, openai_images]` the text
children showed `openai_images` and the sd child `anthropic, openai`. And
`/api/v0/models` listed every portal model as `"llm"`, unfiltered, images-only
models included (issue #148). Only the listing's `image` flag already read the
spec (ADR-044 (f)).

**Decision (a): one rule, in `internal/routing`, read by dispatch and by every
listing.** `routing/served_flavors.go` is store-free and sits beside
`EffectiveRuntimeSpecType`. `EffectiveFields` returns a mapping's effective
flavors E, its responses and messages modes (M is the messages mode) and its
live-timings flag: the spec's for a `server_agent` application with a spec row
for the mapping — a stored `[]` counts as a spec, and `Enabled` is ignored —
and the application's otherwise. `targetFrom` builds its `Target` from it, and
`targetIsImagesOnly`, the warmer's check (`Server.mappingIsImagesOnly`) and the
benchmark scheduler's (`mappingSpecIsImagesOnly`, over the spec it reads with
`mappingRuntimeSpec`) call the moved `FlavorsAreImagesOnly`, so the
precedence exists once and neither background job changes behaviour. The
operator's manual runs read the same rule (`mappingSpecIsImagesOnly`), and so do
the model-server row's `load_refusal` and the mapping DTO's `images_only`,
which show the runs' decision in the portal
([ADR-046](#adr-046--a-request-scoped-router-ensure-route-start-a-managed-child-without-forwarding-a-request)).
Images-only keeps its definition: the list names `openai_images` and neither
`openai` nor `anthropic`, so an empty list is never images-only. With A the
application's flavors, the rule has a **flavor half** (`MappingHasAPIFlavor`)
and a **served** rule (`MappingServesAPIFlavor`: the flavor half plus the
mode):

| Flavor | Flavor half | Served (listed) | Mirrors |
|---|---|---|---|
| `openai` | `openai` ∈ A and E is not images-only | = flavor half | chat completions: `resolveTranslateTarget` → `targetIsImagesOnly` |
| `anthropic` | `anthropic` ∈ A and `anthropic` ∈ E | flavor half and M ≠ `disabled` | `/v1/messages`: `tryProxyNative`'s flavor check and mode read, and candidacy (`applicationServesEndpoint`) for an ordinary application |
| `openai_images` | `openai_images` ∈ A and `openai_images` ∈ E | = flavor half | the images relay's flavor stage; the capability stage stays with `image` and `Capable` |

The A-half of each is candidacy's own predicate, not a restatement —
`applicationHasAPIFlavor` for the flavor half, `applicationServesEndpoint` with
`openai_chat_completions`, `anthropic_messages` or `openai_images` for the
served rule — so a later change to candidacy moves the listing with it. For an
ordinary application `applicationServesEndpoint` already reads the messages
mode; for a `server_agent` application it reads the coarse flavor only, and E
and M complete the rule.

**Why `openai` is narrow and `anthropic` full.** The portal chat, the warmer
and the redirect's text leg call chat completions, which refuses only an
images-only target, so a child whose spec is `[anthropic]` or `[]` answers chat
and stays listed under `openai`. `/v1/messages` holds every request whose
resolve there succeeds to the full ADR-033 conjunct, flavor and mode, so the
`anthropic` listing does too.

**(b) Every listing and `Callable` use the served rule; `Existing` uses the
flavor half.** `perNameFlavors` is the only per-name flavor fold and takes the
rule to evaluate as a parameter. A spec index (`runtimeSpecIndex`, built by
`runtimeSpecIndexForViews`: the spec row per mapping, plus the applications
whose spec read failed) replaces the flavor-only map and is handed as one
value to `perNameFlavors`, `flavorSetsFromViews`, `existingNamesForFlavor`,
`capableNames` and `imageFlagsByName`; `modelsResponse` reuses the read it
already made, and `capableNames` loses its own. The served rule drives the
`flavors` of `Models()` and `ManageModels()`, `ModelsForFlavor` (behind
`/v1/models`, `/openai/v1/models` and `/anthropic/v1/models`),
`ModelOffering.Callable` and the group fold of `Capable`, and through them
`callableModelNames` and the redirect, with no code change there. `Existing`
uses the flavor half, for its names and for its group overlay alike: a name
that fails a flavor's flavor half does not exist under that flavor, exactly
like a name whose application lacks the flavor, while a name whose messages
endpoint is `disabled` still exists under `anthropic` but is not callable.
`/api/v0/models` (`handleLMStudioModels`, through `openAIModelDTOs`) keeps
only the DTOs whose `flavors` contain `openai`, and keeps `"type": "llm"`;
`modelsResponse` and `flavorSetsFromViews` apply the same views, group
overlay, suppression and override aliases, so it lists exactly `/v1/models`'
names, groups and aliases included. `ServerModels` stays flavor-blind.

**Consequence: requests of a token with the unknown-model redirect on change,
in both directions.** "Effective model" is the name after the override rows and
the catch-all ([Routing & Model Selection
§2.1](cross-cutting/routing-and-model-selection.md#21-per-token-model-resolution)).
These requests kept their model and got a 404; they now fail the flavor half,
as an application-level flavor absence does, and the narrow default redirects
them:

| Request | Effective model | Its 404 without the redirect |
|---|---|---|
| `/v1/chat/completions` | an images-only child | `routing.no_model_route` |
| `/v1/responses` | an images-only child | `responses.endpoint_disabled` |
| `/v1/messages` | an agent child whose spec lacks `anthropic` | `messages.endpoint_disabled` |
| `/v1/images/generations` | a name whose every mapping is an agent child whose spec lacks `openai_images` while its application declares it (the text children of a mixed application), or a group of only such names | `routing.no_model_route`, or `routing.model_not_capable` without a verdict |

Image candidates do not move, because `Capable` already read the spec. A
messages-disabled model — effective M `disabled`: an ordinary application, an
agent child's spec or its application fallback, and a group of only such
members — stays in `Existing(anthropic)`, so the narrow default leaves it its
404, as before; under `UnknownModelRedirectBlocked` it is now redirected, where
it used to be callable and kept. As `LastUsedModel` or fallback on
`/v1/messages` it is now skipped, where it used to be taken and then answered
404 (`routing.no_model_route` at candidacy, or `messages.endpoint_disabled`);
and every name that fails the served rule for a flavor is skipped as a
candidate for that flavor.

**(c) A failed spec read: text fail-open, image fail-closed.** For an
application whose `RuntimeSpecsByApplication` read fails, E = A and M is the
application's messages mode, for all three flavors, in `flavors`,
`ModelsForFlavor`, `Callable`, `Existing` and `callableModelNames` alike, while
`image` and the image `Capable` stay false and text `Capable` equals
`Callable`, as before. Shrinking `Existing` during a store blip would make the
redirect reroute requests, against its documented safe direction, and one bad
row already fails a whole application's read. The price is that the candidate
half admits an images-only `LastUsedModel` again while the read fails, exactly
as it did before this rule. The warning keeps "runtime spec read failed" and
the application id, is unthrottled because it fires only on a store error, and
no longer claims to come from the models listing, since `ModelsForFlavor` and
redirect-on requests reach it too.

**(d) A name whose served set is empty keeps its row and loses every
`Callable`.** Such a name has no mapping, and a group no offerable member,
that passes the served rule for any flavor: the operator's configuration (an
application without `openai_images` under a spec `[openai_images]`), an
ordinary `[anthropic]` application with messages `disabled`, an agent child
whose application lacks `openai` and whose spec is `[anthropic]` with messages
`disabled` (with `openai` in A that spec passes the `openai` row), a group
whose offerable members are all empty, and an offered alias onto one. The row
stays in `Models()` and `ManageModels()` with a non-nil `flavors: []`, because
`validateServiceAllowedModels` reads `Models()` and hiding it would break every
edit of a service whose allowlist names the model. It leaves `Existing` only
for the flavors whose flavor half it fails, so a messages-disabled `[anthropic]`
model still exists under `anthropic`. `callableModelNames` excludes it, so a
token override, rule target or fallback that names it is refused with 400
`portal.token_model_override_invalid`, and because the user-token editor
resends every model-valued field, an edit of a user token that already targets
it fails until the target changes. The six override pickers — the catch-all,
the rule targets and the fallback, on user and on service tokens — therefore
offer only names whose `flavors` are non-empty (`overrideTargets`,
`OverrideTargetSelect`). On a user token with a server override set, the
catch-all and rule-target pickers take their options from `ServerModels`
instead, which carries no flavors and keeps hidden names, minus the ids whose
`Models()` row has `flavors: []`, and the fallback picker is never narrowed to
the server. A saved value whose `Models()` row has `flavors: []` stays
visible, marked unavailable, the way the chat treats a vanished model, so the
operator sees why the save fails. Two known limits remain. `Models()` drops
hidden names, so the frontend cannot see whether a hidden name has an empty
served set: a saved value of that kind is never marked, in any picker, and
under a server override such a name is also offered and the save refuses it
(400 `portal.token_model_override_invalid`). `ServerModels` also ignores
provisioning and group locking, so the server-override list can offer other
names the save refuses.

**(e) The "Available via" column shows the flavors a model is offered under.**
A shared frontend helper (`offeredFlavors`) derives them from a DTO: its
`flavors`, minus `openai_images` unless `image` is true. `ModelList.tsx` shows
them, and the chat's `chatModels` uses the same helper, unchanged in
behaviour: it offers a model whose offered flavors include `openai` or
`openai_images`. A model whose `flavors` are exactly `[openai_images]` with
`image` false shows a muted "no image verdict" note, and one with
`flavors: []` a "none" warning chip; the chip's tooltip is static, one text
for a model row and one for a group row, because the DTO carries no
agent-launched marker and no member list, and for an admin it names both
places to check, the application's API flavors and modes and, for a model the
server agent launches, its launch spec. An image request without `image: yes`
is refused with `model_not_capable`, and the listing's `image` is the same
fold as `Capable`, so a displayed `openai_images` means an image request can
pass. The DTO's `flavors` keep meaning the routable flavors, which `Callable`
agrees with, so the subtraction is the frontend's.

**(f) Two operator warnings name the configurations the rule leaves
unserved.** `RuntimeWarnings` emits `api_flavors_not_on_application` when some
spec of a `server_agent` application lists a flavor the application does not
declare, which has no effect because candidacy reads the application's
flavors, and `api_flavors_text_on_stable_diffusion` when some spec whose
effective type is `stable_diffusion_cpp` has flavors that are not images-only
(an empty list included), which the `openai` rule would list as a text model
although `sd-server` has no chat endpoint (measured: 404). Disabled specs
count, because `targetFrom` ignores `Enabled`. The first also fires for a
harmless extra, such as a parent from which `anthropic` was removed on
purpose, so its label says the flavor has no effect, not that the spec is
broken.

**(g) The launch-spec form mirrors both, and moves untouched flavors on a type
switch.** While the form is open the warnings banner is not shown, so two
non-blocking alerts under the flavor controls say the same thing there: a
ticked flavor the parent application lacks, and ticked flavors that are not
images-only on a spec that is sd — Type `stable_diffusion_cpp`, or Auto with
the binary unchanged since load and a loaded `effective_type` of
`stable_diffusion_cpp`. Switching Type to `stable_diffusion_cpp` while the
flavors equal the parent template's sets `[openai_images]` with both endpoint
modes `disabled` and remembers what it replaced; switching from it to an
explicit non-sd type while the values are still exactly that default restores
them, or the parent template when nothing is remembered; switching to Auto
moves nothing, the rule the health path follows; and values the operator
changed stay. The backend stays type-agnostic about flavors.

**Residuals, recorded rather than fixed.** `/v1/responses` for a spec whose E
lacks `openai` and is not images-only: the listing says `openai`, and the
endpoint refuses (issue #150). No endpoint mode other than the messages mode
is reflected in a listing; `openai` stands for chat completions, which has no
mode. A name or group takes the union of its mappings' and members' served
sets and the AND of their `image`, so one that mixes a text member with an
images-only one keeps `openai`, and a text request can still land on the
images-only member (issue #145); one that mixes an image-capable mapping with
one lacking a verdict hides `openai_images` in the column, the fail-closed AND
the chat and the redirect use too.

**Cost.** With N the `server_agent` applications (at most one per server):
`Models()` and `ManageModels()` still read N specs, now reused;
`ModelsForFlavor` reads N over the visible views; `ModelOfferingFor` reads N
over the unfiltered views, once for its three sets, for a text request as for
an image one; `callableModelNames`, on token writes only, reads 3N. Dispatch,
the warmer, the benchmark scheduler and `RuntimeWarnings` read nothing new.

**Rejected:** **the full ADR-033 rule for `openai`** — it would unlist
`[anthropic]` and `[]` children that chat completions serves, and the chat
would lose working models. — **A second copy of the precedence in the
portal** — it would drift from `targetFrom` with nothing to catch it; one
function in `routing` is what both read. — **`Existing` on the served rule** —
a messages-disabled model would read as unknown, and the narrow default would
silently reroute a request whose 404 is a signal about a misconfiguration. —
**A text fail-closed spec read** — (c). — **Hiding a row whose served set is
empty** — (d). — **Dropping `openai_images` from the DTO's `flavors` without
`image`** — `flavors` would stop meaning the routable flavors; the column
subtracts it instead. — **An agent-launched marker or member list on the DTO
for the chip's tooltip** — the static texts name both places to check without
widening the DTO. — **Refusing, on save, a spec flavor the application lacks**
— it also fires for a harmless extra, and the override actions and the VRAM
benchmark replay a stored spec as is, so a refusal would fail them on a spec
whose parent later dropped the flavor. — **Detecting untouched flavors with the
form's touched flag** — it resets on every open; the form compares against the
parent template. — **Moving flavors on a switch to Auto** — an Auto spec with
an `sd-server` binary is still sd
([Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation)).
— **A type rule for llama-server and `openai_images`** — there is no evidence
for it, it contradicts Ollama's verdicts, and the column already hides an
`openai_images` without a verdict.
→ [Routing & Model Selection
§2.1](cross-cutting/routing-and-model-selection.md#21-per-token-model-resolution),
[§2.2](cross-cutting/routing-and-model-selection.md#22-callable-existing--and-why-the-listing-is-neither),
[§8](cross-cutting/routing-and-model-selection.md#8-errors-and-http-mapping),
[API Compatibility & Inference
§3.4](cross-cutting/compatibility-and-inference.md#34-openai-images-generations),
[§6](cross-cutting/compatibility-and-inference.md#6-endpoint-modes-and-native-passthrough),
[§9](cross-cutting/compatibility-and-inference.md#9-model-discovery),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#a-worked-sd-server-launch-under-stable_diffusion_cpp),
[§11.5](cross-cutting/agent-runtime-manager.md#115-what-each-remaining-tab-shows),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances),
[HTTP API Surface
§1](reference/api-surface.md#1-inference--compatibility-endpoints).

## ADR-046 — A request-scoped router ensure route: start a managed child without forwarding a request
**Context:** the Load run and the VRAM benchmark share one load core
(`ensureResidentForRun`), and it loads **by generating**: one `max_tokens: 1`
chat stream, so a backend that allocates its KV cache lazily has done so before
anything is measured. Behind the agent router that stream is also what starts
the child: the router starts it and then forwards the request to it. An
agent-launched `sd-server` child answers `POST /v1/chat/completions` with 404
and an empty body (measured), so a Load of it failed while the child it had
just started stayed up (issue #161). The context probe, the VRAM probe and the
speed, capacity and vision benchmarks sent the same child a chat prompt too;
only the benchmark scheduler and the model warmer skipped it
([ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec) (a)). The router had no way to start a child
except by forwarding a request that names it.

**Decision (a): the router grows `POST /ensure/{model}`, which starts a managed
child and forwards nothing.** The gateway sends no body, and any body a caller
sends anyway is read and discarded, never forwarded: net/http notices a client
that leaves only once the handler has read the body to its end, so an unread
body would keep a departed caller's place in the queue. A body over the router's
32 MiB bound gets 413 `runtime.request_too_large`, as on model routing, before
anything starts. `{model}` is the decoded path after `/ensure/` — the spec's
`upstream_model`, which is the mapping's `app_model_name` — so an id that
contains `/` works; the gateway builds the path with `ExpandModelPath`, which
escapes each segment and keeps `/`. Any other method on the path falls through
to model routing, as on the GET-only control routes. The handler calls
`EnsureRunning` on the request's own context and answers through the router's
existing code table: 404 `runtime.model_not_managed` (an unknown model, an
empty one, or no manager), 503 `runtime.admission_blocked` (a force-stopped
spec, or a start the admission gates refused), 504 `runtime.start_timeout`,
502 `runtime.start_failed` or `runtime.not_permitted`, and 502
`runtime.upstream_gone` for any other failure, such as the manager shutting
down. On success it calls
`release()` at once and then, only if the client is still connected, refreshes
the write deadline and writes 200 `{"status":"running"}`. There is no heartbeat
and no new error code: the route is one long call. That is safe as long as no
hop between the gateway and the router arms an idle or read timer shorter than
the wait. None of the bundled hops does — not the gateway's transport, not the
agent's TLS proxy, not the router's server — and it is how the non-streaming
proxy path already holds a cold start. A proxy an operator adds in between cuts
the call when its timeout is shorter, and the non-streaming path shares that
exposure (issue #169;
[Risks §11.1](11-risks-and-technical-debt.md#111-operational-risks)). The agent
waits up to the spec's `startup_timeout_seconds`, plus
`admission_wait_timeout_seconds` when that is above 0; at 0, which queues until
the client disconnects, the gateway's deadline is the bound. A caller that
disconnects while still queued drops its place, and nothing starts. A start
already under way still comes up and counts as a use — `lastUsed` is set and
nothing is in flight — so the idle policy applies to it as after any inference
request.

**The bodiless shape is the safety net for an older router.** An agent without
the route treats `POST /ensure/{model}` as an ordinary proxied request, and
model routing answers a request whose body names no model with 404
`runtime.model_not_managed` before it calls `EnsureRunning`. So the route starts
nothing on an agent that predates it, even if a gateway sent it there. A body
naming the model would have made the same request start the child on such an
agent and forward it as inference.

**It fits ADR-026, and ADR-037 is left as it was.** The start is scoped to the
request, persists nothing and travels on the data plane, like the inference
request that starts a cold child today; `admin_state`, `force_running` and
`pinned` are untouched, so desired state stays the only control channel
([ADR-026](#adr-026--gatewayagent-control-is-desired-state-not-commands)). The
route is not under `/upstream/`, whose allowlist ADR-037 limits to a GET of
`/props` that never starts or keeps alive a child
([ADR-037](#adr-037--the-runtime-router-grows-a-get-only-per-model-props-passthrough-the-gateway-probes-through-it-with-the-specs-token));
that allowlist is unchanged. Exposure is unchanged too: the router
authenticates nothing ([Agent-Managed Model Runtime
§4.6](cross-cutting/agent-runtime-manager.md#46-the-bind-host-is-operator-controlled)),
and a POST whose body names the model already starts its child.

**(b) The gateway sends the route only to an agent that declares it.** The agent
declares `runtime_ensure` on its own list only (`Since: "0.8.0"`, a MINOR bump
from `0.7.4`), and the gateway reads it off the agent's sample
(`s.AgentFeatures.Has(server.ID, runtimeEnsureFeature)`) without declaring it
back ([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)).
Without the name, an images-only Load or VRAM probe is refused with 409
`benchmark.agent_ensure_unsupported` before anything reaches the agent. Its
label names `runtime_ensure`, an agent 0.8.0 or newer, and an agent that has not
reported since the gateway restarted, because the gateway holds the declared
set in memory. A newer agent under an older gateway simply never receives the
route.

**(c) Only the Load and the VRAM probe of an images-only `server_agent` mapping
use it.** Images-only is ADR-045's rule over the mapping's effective flavors
(`mappingSpecIsImagesOnly`), not the spec's type: an sd spec that keeps a text
flavor is still loaded by generating, and the runtime warning
`api_flavors_text_on_stable_diffusion` names that configuration. A text child
keeps loading by generating, because the lazy-KV-cache argument holds for it
and a text Load also proves that chat works. The Load starter marks the target
(`benchmarkTarget.loadWithoutGenerating`); the VRAM probe's plan carries the
same decision (`vramRunPlanned.ensure`) onto the target it loads. The load core
keeps its already-resident short-circuit, and then, inside the same 503-retry
loop and under that loop's own deadline, calls the provider's `RuntimeEnsurer`
instead of streaming, so the window in which a just-cleared `force_stopped` has
not yet reached the agent is absorbed as it is for a text target. The provider sends the
bodiless POST with the same upstream credential as every provider call, arms no
timeout of its own, accepts only a 2xx whose `status` is `running` (a 2xx that
says anything else, or whose body is cut short, is
`provider.invalid_response`; a read that the deadline cuts off is
`provider.timeout`, and one that a cancellation cuts off is
`provider.unavailable`), keeps a 503 retryable
(`ErrUpstreamStarting`) and a 401 or 403 an auth rejection (`ErrAuthRejected`),
and keeps the router envelope's code (`RouterError`). A failed ensure is
recorded (`loadEnsureError`) in one of four texts: the loop's own deadline, when the
provider reports it as a timeout, as
`provider.timeout: not running within <loop bound> …`; a failure that carries
one of the codes the route answers as `<router code>: <hint> (<provider text>)`,
one hint per code, the start-timeout one quoting the target spec's
`startup_timeout_seconds`; a 2xx without `running`, or one cut short, as
`provider.invalid_response: the agent's ensure route answered without "running"`;
and any other failure with the provider's own text ([Agent-Managed Model
Runtime §11.9](cross-cutting/agent-runtime-manager.md#119-manual-runs-on-an-images-only-mapping)). "Loaded" then
means the child's health path answered 2xx — for `sd-server` that is
`/v1/models` by default — and the row turns loaded within about one telemetry
sample.

**The VRAM number comes with a caveat, not a refusal.** An ensure plan carries
the warning `first_generation_not_measured` from the start: the run measures
the child once it is up and healthy, and what `sd-server` allocates on top when
it first generates has not been measured. It is a warning rather than an
inconclusive reason, so the launch-spec form still offers the number, with the
caveat. The runner checks `runtime_ensure` again before it drains, for an
ensure plan only, so a run whose agent stopped declaring the name after the
trigger ends with an error naming `benchmark.agent_ensure_unsupported` before a
spec is written. The run's own `runtime_manager` re-check is left alone:
adding the name there would refuse every VRAM probe on an older agent, text
targets included.

**Consequence: the other manual runs of an images-only mapping are refused, and
a Load does not override desired state.** Every mapping-scope starter checks, in
order and before it reserves the server: authorization; a run already holding
the server (409 `benchmark.already_running` — first, because a VRAM run's drain
has stored `force_stopped` on every enabled spec, and a Load during it must not be told
to clear that); for a `server_agent` application, one read of the mapping's
spec that fails closed (500 `benchmark.request_failed`) and becomes the run's
target spec, so the run and its hints see what the check saw — except that the
VRAM probe takes its images-only decision from the fleet read its plan makes, a
second read; then the refusals. A
context probe and a mapping-scope speed, capacity, both or vision run get 409
`benchmark.images_only`, and so does a Load of an images-only mapping whose
application is not `server_agent`. A Load of a spec whose `admin_state` is
`force_stopped` gets 409 `benchmark.spec_force_stopped`, whether it would
generate or ensure: without the refusal it would retry the router's immediate
503 until its bound, and a Load must not clear an operator's override. Application and server
scope skip an images-only mapping instead (`results[].skipped: "images_only"`)
and name a mapping whose spec could not be read; only a scope with nothing left
to run is refused. The portal shows the same decisions before the click: the
mapping DTO's `images_only`, and the model-server row's `load_refusal`, which
one helper (`loadRefusal`) computes for both the row and the Load starter.

**Rejected:** **`/upstream/{model}/ensure`** — ADR-037 limits `/upstream/` to a
GET of `/props` that never starts or keeps alive a child. — **A body that names
the model** — an older router would start the child and forward the request.
— **`GET`** — the route has a side effect. — **Heartbeats** — they commit a 200
before the outcome is known. — **Start, then poll** — a detached waiter is
saved state. — **`force_running`** — it persists, and a VRAM probe refuses to
start against a stored override. — **A real image generation** — 17–20 s of GPU
for every Load. — **No credential on this one call** — it would be the provider
package's only exception. — **Detecting the route by trying it** — an older
agent's 404 carries the same code as an unmanaged model. — **Gating on
`agent_version`** — ADR-025. — **Falling back to the chat Load** — it cannot
succeed. — **Guessing "text" when the spec read fails** — that is the very
prompt the check exists to stop. — **A whole-run 500 at application or server
scope when one read fails** — it blocks the readable siblings. — **Recording the
skip inside the run loop, or in `error`** — the first writes history rows, the
second reads as a failure.
→ [Agent-Managed Model Runtime
§4.1](cross-cutting/agent-runtime-manager.md#41-control-routes),
[§4.4](cross-cutting/agent-runtime-manager.md#44-streaming-heartbeats-and-the-lazy-200),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[§11.6](cross-cutting/agent-runtime-manager.md#116-the-vram-benchmark-load-one-model-alone-and-measure-what-it-costs),
[§11.9](cross-cutting/agent-runtime-manager.md#119-manual-runs-on-an-images-only-mapping),
[API Compatibility & Inference
§7.2](cross-cutting/compatibility-and-inference.md#72-the-benchmark-stream-watchdog),
[Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks),
[HTTP API Surface](reference/api-surface.md#benchmark-load-and-context-probe-runs).

## ADR-047 — A manual speed run measures every agent model on an emptied server: it stops the server's running models before each cold pass and lifts the server's pins for the run
**Context:** the speed benchmark's load time is its cold pass's time to first
token minus its warm pass's, so the model has to be cold when the cold pass
starts. For a `server_agent` application nothing made it cold. The portal
stores the application's gateway-side probe fields empty, and the gateway
derives the agent router's own routes in their place ([Agent-Managed Model
Runtime §3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation)),
but the router has no unload route: it answers the unload that the cold pass
sends to other application types with 404 `runtime.model_not_managed`. So an
agent model that was resident when a run started recorded no load time. Desired
state can stop a model, but pins get in the way: a pinned child restarts as soon
as its `force_stopped` is cleared, so its measured load comes out too small, and
under a closed co-residency matrix a pinned neighbour blocks a cold target,
because the agent never evicts a pinned child. And where a cold target did
start, its time to first token included the exit of every neighbour it had to
evict: measured, 3.5 s for a model whose own load took 1.5 s, next to a
neighbour that took 2 s to exit. The column would then hold two meanings for
one model, depending on what ran beside it.

**Decision (a): the stop-all.** Before each agent target's cold pass
(`preStopServer`), the run force-stops, in one batched write, every model of the
server's agent application that has a process or reports a state the gateway
does not recognize, and the target itself whenever its own status row reads
anything other than `stopped`: a request for a spec in `backoff` waits for its
backoff timer, and that wait would land in the load time. The stop completes on
the first status frame received after the write in which every row is quiet
(`benchmarkRowQuiet`: a state without a process, and pid 0) and, when the
target read `backoff`, `start_failed` or `crashed` at selection, the target's
own row reads `stopped`, because the agent resets those states when it
applies the stop's document, which can be as late as its poll; all within
`benchmarkStopWaitBound` (120 s). The unpin goes out without a wait, so the
agent may still hold a formerly pinned neighbour pinned: a stop-set row that
was not quiet and turns quiet in the wait is the run's sign that the agent
holds the lifted pins, and without such a row every formerly pinned
neighbour, the target aside, already has to read `stopped` with pid 0. Two
narrow misses remain: a process that exits by itself inside the stop
document's delivery window is taken for that sign, and only the specs this
run unpinned are judged. The clear is one batched write as soon as the wait
ends, on a context that is not cancelled with the run, and
[ADR-048](#adr-048--the-runtime-config-push-is-one-worker-per-server-a-document-is-derived-only-after-the-previous-one-was-enqueued-the-last-reflects-the-latest-write-and-frames-are-spaced)'s
serializer delivers its document after the stop's. The portal's restart
([ADR-026](#adr-026--gatewayagent-control-is-desired-state-not-commands))
completes on a transition; this stop completes on a state, whatever caused it.
That is sound only because an unpinned child without `force_running` and
without a process starts only on a request, and the reservation sends it none:
routing skips a server a benchmark holds, and so does the model warmer. The
stop-all is bounded and clears on every exit. It never stops while every frame
of its 5 s selection window shows traffic in flight: a stop cuts off a stream
that outlasts the agent's 10 s drain, and the client sees a clean end of
stream, not an error. When the target itself was stopped, the cold pass
rides the router's 503 `runtime.admission_blocked` through the load loop
(`coldPassAfterStop`) until the clear reaches the agent, and keeps the time to
first token of the attempt that was served, which includes the whole start. The
stop runs per target, not once per run: each measured model stays resident
after its warm pass, so one stop before the first target would let the second
target's cold start evict the first and count that exit again.

**(b) Manual speed and both runs only, and one meaning for the column.** Only a
manual run in mode `speed` or `both` stops and unpins (`startBenchmark`, behind
`AuthorizeBenchmarkScope`). Scheduled runs never do, and neither does any other
run kind; the VRAM run keeps its own drain. Every run that measures a load
time, stopping or not, confirms one only when no other model of the application
has a process or can start one by itself, short of the two narrow misses (a)
names. On the target's own row: without a stop it reads `stopped` or it has no
row; with one it is quiet, and it has to read `stopped` only when it had read
`backoff`, `start_failed` or `crashed` at selection — `not_permitted` and
`pending_vram_unknown` wait behind no timer either way. Without a stop, every
other row of a non-empty runtime status also has to read `stopped` with pid 0
(`benchmarkOthersStopped`): without the unpin, a pinned neighbour in `backoff`
has no process now but restarts when its timer fires. So the column has one
meaning, the model's own load on an otherwise empty server, and a run that
cannot confirm keeps the last value.

**(c) The temporary unpin.** At run start, the run lifts the pin of every
enabled pinned spec of the server's agent application, measured or not, in one
batched write (`SetBenchmarkRuntimeSpecsPinned`) with one notification and no
wait. A pinned neighbour holds its VRAM, the agent never evicts it, and the
stop-all cannot stop it, because it would restart at the clear. After the whole
run, still inside the reservation, the run pins exactly those specs again
(`endBenchmarkUnpin`). Every spec write of the run is a compare-and-set: a spec
whose value changed in between is left alone, and a spec deleted before the
write re-reads it is gone and is not created again. A DELETE that lands between
that re-read and the upsert still stores the spec again under its old id,
because the upsert keys on `mapping_id` and the store has no conditional update
by id (an accepted limit, `priorRuntimeSpec`).

**(d) The override lease and its reconciler.** Before it writes them, the run
records the pins it owes and the `force_stopped` overrides it owes in one
`system_settings` row per server, `benchmark_override_lease:<server_id>`, and it
rewrites the row as it settles them; an empty row releases it. A
compare-and-set reconciler clears and re-pins whatever an earlier run left
behind, because it died or could not release the row, and logs what it settles
at Warn: at gateway start (`ReconcileBenchmarkOverrideLeases`, before the
listeners and the benchmark scheduler start), and at the next manual speed or
both run on that server, once that run has passed its own start gates. The
clear goes first, because a spec that stays `force_stopped` refuses every
request, while one that stays unpinned still serves on demand. The VRAM run
records its drain in the same row.

**(e) Gates.** Nothing is unpinned or stopped in file mode, without
`runtime_manager`, while any enabled spec on the server carries `force_running`
(it restarts at once after any stop and thrashes with the target), or when the
lease row cannot be read or written. A target is not stopped while every frame
of its selection window shows traffic in flight, while its stop set holds a spec
that the store's re-read shows pinned, disabled, unknown or carrying an
override, or while it has no status row. After a stop wait expired or a clear
failed, the run stops nothing more.

**Consequence: the load time is the model's own load on an otherwise empty
server, and the models that ran pay for it.** It resolves to the agent's 500 ms
health poll, and, short of (a)'s two narrow misses, no run records a load time
that includes making room. Every model that runs when an agent target starts
is stopped for that measurement. After the run only the last target and the
formerly pinned specs are up, and every other model pays one full load on its
next request, because the run does not restart it. A scheduled run records a
load time only on an otherwise empty server, so at most one per run. While a
run holds the server, the runtime section shows every pin lifted, and each
stopped model `force_stopped` for the stop wait, 5 ms to about 5 s over
WebSocket; the agent holds the override up to 0.25 s longer, until the clear's
document goes out. Two crash windows remain, the stop's and the unpin's: a
gateway that dies inside one leaves the overrides or the lifted pins in place
until it starts again against the same store, and the lease then puts them
back ([Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks)).

**The VRAM run's refusal of a pinned sibling is unchanged.** The VRAM run does
not lift pins: silently breaking an operator's standing instruction for a
benchmark is a worse surprise than refusing and naming it. The speed run meets
that reason differently, because it never lifts a pin or stops a model
silently. A Warn log names every lifted pin, and the benchmark panel names
every lifted and every stopped spec (`unpinned_spec_ids`, `stopped_spec_ids`).
The run puts each one back, and a failed re-pin (`repin_failed`) is the run's
error, while a failed clear is only the target's own result error.

**Fit with the other ADRs.**
[ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions):
only named feature flags are read. ADR-026: the benchmark is a second writer of
`admin_state` that clears on every exit, because it created the override, and
it completes on a state for the reason in (a).
[ADR-028](#adr-028--runtime-config-notifications-are-gated-by-write-scope-not-by-changed-field):
both batched writers (`SetBenchmarkRuntimeSpecsAdminState`,
`SetBenchmarkRuntimeSpecsPinned`) notify by their write scope, once per server
they stored to, after their last write; the lease row is not an input of the
runtime-config document and does not notify.
[ADR-029](#adr-029--runtime-domain-writes-are-full-document-replaces-gated-on-their-own-get):
the runtime section's override actions read their spec before they write it,
which applies "gated on its own GET" to the document the write replaces; a
cache loaded during a run carries the lifted pin, and the next override click
would otherwise undo the re-pin.
[ADR-037](#adr-037--the-runtime-router-grows-a-get-only-per-model-props-passthrough-the-gateway-probes-through-it-with-the-specs-token)
is unchanged.
[ADR-046](#adr-046--a-request-scoped-router-ensure-route-start-a-managed-child-without-forwarding-a-request):
exposure is unchanged, because no route is added. ADR-048: the run relies on
the serializer for the order of its documents, so it needs no delay of its own
between its writes.

**Rejected:** **an unload route on the router** — it exposes a one-request
stop on a router that authenticates nothing, which ends ADR-046's "exposure is
unchanged", and it needs an agent release. — **An unload generation in the
desired-state document** — it has no crash window, but needs a migration, an
agent release and an ADR of its own; it is the follow-up if unattended stops
are ever needed. — **One stop-all per run** — the second target's cold start
evicts the first and counts that exit. — **Stopping every enabled spec** — a
spec without a process has nothing to stop, so it only widens the override's
footprint and what a crash leaves behind. — **One write per spec
for the stop** — one document per spec, and an agent before 0.8.1, whose sync
is single-flight, can end on a partial one. — **The VRAM run's isolation wait
(`vramAwaitIsolation`)** — its acknowledgement proof or blind 60 s delay keeps
an isolation in force across a long measurement, and the stop needs quiet for a
moment only. — **A literal `stopped` for the stop wait** — up to 60 s more for
a child that crashed while draining and sits in `backoff`. — **Stopping a spec
with traffic in flight** — it ends a direct client's stream, silently. —
**Measuring a pinned target as it is** — the clear restarts it, so its load
comes out too small. — **Skipping pinned targets** — a pinned model would never
get a load time. — **A lazy unpin per target** — a pinned neighbour would stay
pinned, and the stop-all could not stop it. — **Per-spec unpins** — one
document per spec, the same partial-document risk a batched write avoids. —
**An acknowledgement gate** — an acknowledgement proves
that the agent once held a document, not that no older one follows it, and
only agents that declare it would benefit. — **A delay of one push bound
between the run's writes** — it guards timing, not order, and stays wrong when
a derive outlives its bound. — **Clearing `force_running` for the run** — it is
not a pin, and the model restarts at once. — **Restarting the stopped models
after the run** — it needs the ensure route, races user traffic once the
reservation is released, and under a closed matrix cannot rebuild the set that
ran before. — **Holding the reservation until the re-pinned models run** —
minutes of excluded traffic for a display metric.
→ [Agent-Managed Model Runtime
§9](cross-cutting/agent-runtime-manager.md#9-keeping-the-agent-current-the-notification-rule),
[§11.2](cross-cutting/agent-runtime-manager.md#112-restart-is-a-sequence-not-an-endpoint),
[§11.6](cross-cutting/agent-runtime-manager.md#116-the-vram-benchmark-load-one-model-alone-and-measure-what-it-costs),
[§11.9](cross-cutting/agent-runtime-manager.md#119-manual-runs-on-an-images-only-mapping),
[§11.10](cross-cutting/agent-runtime-manager.md#1110-load-time-of-an-agent-model-the-stop-all-the-temporary-unpin-and-the-override-lease),
[Routing & Model Selection
§7](cross-cutting/routing-and-model-selection.md#7-model-selection-metrics),
[Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks),
[HTTP API Surface](reference/api-surface.md#benchmark-load-and-context-probe-runs).

## ADR-048 — The runtime-config push is one worker per server: a document is derived only after the previous one was enqueued, the last reflects the latest write, and frames are spaced
**Context:** every portal write that can change a server's runtime-config
document notifies the server's agent
([ADR-028](#adr-028--runtime-config-notifications-are-gated-by-write-scope-not-by-changed-field)),
and the notification ran `PushRuntimeConfig` as one goroutine per write. Each
goroutine derived the document with several store reads under a 5 s bound and
then enqueued it on every open connection of the server, and nothing ordered
two of them. When a stop was written and its derive was still running as a
clear was written, the clear's goroutine enqueued its document first and the
stop's goroutine enqueued the stale one last. The connection's queue and the
agent's sequential read loop delivered both faithfully, and the agent, which
adopts every document whose ETag differs from its own, ended on the stop while
the store held the clear, until its next 60 s poll. A wedged store also
accumulated one goroutine per write. A second loss sits at the agent and is
not about order: an agent before 0.8.1 runs its runtime sync single-flight, so
a document that arrives while a sync runs is dropped, and two documents closer
together than one sync lose the second even when they arrive in order. A sync
includes a features round trip to the gateway, so a burst of documents sent
back to back left such an agent on the first, partial one; agent 0.8.1 owes a
trailing sync to such a document instead. The POST transport never had either
problem: it never pushes, and every poll derives at request time.
`TestRuntimeConfigPushOrdersDocumentsByDerive` and
`TestRuntimeConfigPushEndsOnTheStoreDocumentAfterAPortalBurst` pin the fix.

**Decision: `PushRuntimeConfig` hands the server id to a per-server worker
(`runtimeConfigPusher`, `internal/gateway/runtime_config_push.go`).** The hook
keeps its signature and its wiring.
- **(a) Order.** Passes for one server never overlap: a worker's map entry is
  created before its goroutine starts and deleted by that goroutine in the exit
  check that finds no pass owed, both under one leaf mutex. So document k+1 is
  derived only after document k was enqueued, dropped or failed to derive, and
  every connection of the server receives the documents in derive order.
- **(b) Latest wins.** A notification comes after its write's commit. It either
  starts a worker, whose first derive begins after it, or marks the running
  worker dirty before that worker's exit check, which owes one more pass whose
  derive starts after the check. So after every write a pass that sees it runs
  to completion; intermediate documents may be skipped. A derive is several
  separate reads, so a pass that runs alongside a write can send a partial
  document, and such a document is always followed by another pass. The dirty
  check and the delete from the map are one critical section: split, a
  notification between them is lost.
- **(c) The hook stays fast.** A notification takes one leaf mutex and at most
  starts a goroutine. Goroutines are bounded at one per server with pending
  work, and an idle server keeps no state.
- **Spacing.** After a pass that enqueued a frame on at least one connection,
  the worker waits `pushRuntimeConfigSpacing` (250 ms, per `Server`) before its
  next pass, and notifications in that window coalesce into that next pass. The
  first document after a quiet period goes out at once, and a pass that sent
  nothing (no connection, the POST transport, a refused gate) is not spaced.
  The spacing covers a sync shorter than 250 ms on an agent before 0.8.1; it is
  not an ordering mechanism, because the order is structural (a). It applies to
  every agent, with no feature flag: from 0.8.1 the agent owes a trailing sync
  and does not need it, but older agents stay in the field, and the gateway
  tells agents apart by feature flags, never by version
  ([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)).
  A flag would save at most 250 ms per document.
- **The derive bound stays 5 s per pass** (`pushRuntimeConfigTimeout`). A
  failed or timed-out derive enqueues nothing and is not retried; a pass a later
  notification owes still runs, and the poll backs it up.
- **No dedup.** A notification that lands after a derive already read its write
  yields one redundant frame, which the agent ignores by ETag. Dedup would need
  per-server ETag memory, and it would skip the re-enqueue that repairs a frame
  a full queue dropped.

**Consequences:** a caller that writes the document several times in a row
needs no delay of its own between the writes to keep their order, and the
documents every connection receives follow the writes' order, at least 250 ms
apart; a write that lands while a pass or its spacing wait runs is folded into
the next document.
A manual speed run's stop-all
([ADR-047](#adr-047--a-manual-speed-run-measures-every-agent-model-on-an-emptied-server-it-stops-the-servers-running-models-before-each-cold-pass-and-lifts-the-servers-pins-for-the-run))
relies on this order: it waits for no earlier document to land before it
writes, and holds no clear back for a minimum gap after its stop.
A wedged store delays later pushes for its server, behind one 5 s derive,
instead of letting them overtake it, and coalescing keeps that backlog to one
pass. A document can cost up to 250 ms when it follows another within 250 ms.
Workers are detached and not awaited at shutdown, as the goroutines were. What
remains for the poll and the reconnect resync: a sync longer than the spacing
on an agent before 0.8.1, a full send queue that drops the newest frame, a
failed derive, a server without an open connection, and the agent's own VRAM
writeback, which deliberately does not notify ([Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks)). From agent 0.8.1
the single-flight drop is gone: `triggerRuntimeSync` owes one trailing sync,
with the latest payload, to a document that arrives during a sync
(`TestRuntimeWakeDuringASyncEndsOnTheLatestDocument`).

**It fits ADR-026 and ADR-028.** The document stays desired state, never a
command, and every frame stays self-contained and idempotent
([ADR-026](#adr-026--gatewayagent-control-is-desired-state-not-commands)). The
notification rule is unchanged; only its delivery is serialized
([ADR-028](#adr-028--runtime-config-notifications-are-gated-by-write-scope-not-by-changed-field)).

**Rejected:** **A delay in each caller between two writes, one push bound
long** — it guards timing, not order, and stays wrong whenever a derive outlives
its bound. — **A debounce** — it delays every first click. — **Holding one lock
across the pass** — it blocks the hook and serializes all servers. — **Gating
sends on `runtime_config_ack`** — only agents that acknowledge benefit, the
acknowledgement trails by a telemetry sample, and the agent's VRAM writeback
makes the two ETags diverge. — **A per-connection latest slot for
`runtime_config`** — it would remove the full-queue residual, but it changes the
frame path that the log and certificate frames share, for a case that needs a
2 to 5 s socket stall. — **Skipping the derive when no connection is open** —
it saves one store read and makes a pass behave differently with and without a
connection. — **Routing the poll through the serializer** — a poll response and
a WebSocket frame travel on different channels, so a shared lock orders nothing
at the agent.
→ [Agent-Managed Model Runtime
§8.1](cross-cutting/agent-runtime-manager.md#81-gateway-mode-push-poll-and-a-disk-cache),
[§9](cross-cutting/agent-runtime-manager.md#9-keeping-the-agent-current-the-notification-rule),
[Risks & Technical Debt
§11.1](11-risks-and-technical-debt.md#111-operational-risks).

## ADR-049 — Vendor accounts are a first-class entity; the subscription-OAuth path is experimental and ToS-restricted
**Context:** a portal user wanted to route their own requests through an external
AI vendor account — a plain platform API key, or a consumer subscription (Claude
Pro/Max, ChatGPT/Codex) reached through the vendor's own OAuth login — alongside
the self-hosted AI servers. Two design questions were load-bearing. First,
**naming**: "provider" is already the backend client-adapter layer
(`internal/provider`), the `Application.Type` discriminator
(`routing.ProviderOllama` …), `Target.Provider`, `usage.Event.Provider`, and the
agent-reported `provider_health`. An external vendor *account/subscription* is a
different concept and must not collide. Second, **legitimacy**: the subscription
path reuses consumer-subscription OAuth tokens for inference through a third-party
gateway, which is against both vendors' consumer Terms — Anthropic enforces it
server-side (the credential is authorized only for use with Claude Code; the
forced `You are Claude Code` system block) — and every endpoint, client id, scope,
beta header, serving host, device-code path, token-claim name, request/refresh
encoding and rate-limit response-header name is undocumented and
reverse-engineered.

**Decision — five choices, taken together.**
- **(a) A first-class `vendor_account` entity, a sibling of `AIServer`, not folded
  into "provider".** Code/wire/schema name `vendor_account` (type `VendorAccount`,
  id prefix `va_`); enums `vendor ∈ {openai, anthropic}` (**extended by
  [ADR-052](#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)**: five OpenAI-compatible ids join it), `auth_type ∈ {api_key,
  subscription}`. It lives beside the untouched `internal/provider` package and
  *reuses* its clients to reach the vendor clouds — renaming `provider` would be a
  broad, unrelated refactor the repo rules forbid. The entity is stored across all
  three drivers (migration 82) and owned by exactly one user; sharing via resource
  groups is deferred and made additive, not pre-built.
- **(b) The subscription-OAuth path is accepted as experimental and
  ToS-restricted, on purpose.** It is built **for internal testing with the
  operator's own account**, flag-gated, disable-able, and must degrade gracefully
  when a vendor blocks or changes behavior; it must never be presented as a
  supported, multi-tenant production capability. The acceptance is deliberate, not
  an oversight ([§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
- **(c) Every reverse-engineered constant is confined and marked VERIFY-LIVE.**
  All vendor OAuth constants live in one file
  (`internal/vendorauth/constants.go`), read through an overridable `Endpoints`
  struct so a live operator or an `httptest` test can correct a rotated value
  without touching flow code; the dispatch literals the resolver needs
  (`internal/routing` cannot import `vendorauth`) are mirrored there with the same
  caveat. All parsing is tolerant — prefix-matched, fail-open, unknown → unknown
  never a fabricated `0`.
- **(d) A configurable routing precedence.** `vendor_account_routing_mode` ∈
  `{vendor_first, fallback_only}` (default `vendor_first`) sets whether a caller's
  own account wins when it serves the requested model, or is used only when no
  self-hosted/shared route exists. The resolver reads it through a cached accessor
  invalidated on a settings write.
- **(e) A master feature flag, off by default.** `vendor_accounts_enabled` (bool,
  default **off**). When off the "Anbieter" nav item is hidden, the CRUD/connect/check
  endpoints answer `409 vendor_accounts.module_disabled`, and the resolver's vendor
  branch and the model-listing overlay are no-ops — the same module-enable posture
  as the NetBird and certificate modules.

**Consequence:** the new serving path reuses the existing credential sealing
([ADR-007](#adr-007--secrets-at-rest-the-encplain-scheme)), the routing `Target`
and dispatch, and the `recordUsage` usage choke point, so the genuinely new code is
small: the entity + store, an OAuth subsystem (`internal/vendorauth`), a native
Anthropic Messages client and an OpenAI Responses translate client in
`internal/provider`, two static `Target` extensions (`ExtraHeaders` + a
`Masquerade` flag) plus an explicit `Subscription` trigger and a `VendorAccountID`
attribution field, and a header-scraped usage snapshot. The subscription path
requires `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY` because the OAuth token set is a
decryptable secret at rest (a keyless disk store rejects the write). Because the
subscription constants are undocumented, a large part of the subscription code
(endpoints, headers, model ids, refresh encoding, rate-limit header names) is
VERIFY-LIVE and can break without notice; the flag-gated, fail-open design is what
keeps that from affecting the self-hosted path or a deployment that leaves the
flag off.

**Rejected:** folding the account into the "provider" term (a four-way-overloaded
name that would collide with the adapter layer, the application type, the target
field and the usage field); renaming `internal/provider` to free the name (a broad
refactor the repo forbids, unrelated to this feature); building the subscription
path as a supported production capability (it rests on undocumented, ToS-violating,
server-side-enforced behavior — it is honest only as flag-gated experiment);
hardcoding the vendor constants inline across packages (they must be correctable in
one place against a live vendor); and shipping the master flag **on** by default (a
ToS-restricted, experimental path must be opt-in).

**Follow-ups landed under the same flag (no new decision).** Three changes built on
this entity without altering any of the five choices. *Credential validation:*
four model-independent probes (an OpenAI and an Anthropic one for each of the two
auth types) classify a credential as valid, invalid (HTTP 401 only) or
unverifiable, fail-soft, so a token import refuses a definitively rejected token
but is never blocked by an unreachable vendor, and an owner-only test-connection
endpoint (`POST /api/portal/vendor-accounts/{id}/check`) reports the same verdict;
the two subscription probe endpoints are reverse-engineered and join the
VERIFY-LIVE constants of choice (c) (as plain constants, not part of the
overridable `Endpoints`). *File-assisted import:* the portal parses a
Codex `auth.json` or Claude Code `.credentials.json` in the browser and sends only
the access token, refresh token and expiry to the existing import endpoint, so the
raw file never leaves the browser. *Catalog by auth type:* an OpenAI subscription
account is seeded with only the models the Codex backend serves, an `api_key`
account with the full set (accounts seeded earlier keep their rows; there is no
backfill; the seed is now only the create-time fallback, see below).
→ [External Vendor Accounts §3.5](cross-cutting/external-vendor-accounts.md#35-credential-validation).

**Follow-up: dynamic model discovery, a model prefix and dashboard visibility
(also no new decision).** A static model list is a guess that the vendors outrun,
and for a ChatGPT subscription it was a visibly wrong one: the Codex backend
serves newer model generations than any list kept in this repository. The gateway
now asks the vendor which models the account's own credential can use and
replaces the seed with the answer — at a subscription connect (best-effort, short
bound, never failing the connect) and on an explicit, owner-only
`POST /api/portal/vendor-accounts/{id}/models/refresh`. It is fail-soft in the
way the credential validation is: an unreachable, rejecting, empty or
unrecognizable answer leaves the existing models untouched, so a changed vendor
schema can never wipe a working catalog. An expired subscription token is renewed
first through the gateway's own locked refresher rather than by the portal,
because a refresh token is single-use and a second refresher would race the
dispatch for it. Choice (c) extends rather than changes: the Codex catalog request
is the one subscription endpoint an operator has confirmed live, the Anthropic
consumer-bearer listing is not, and the Codex `client_version` the request carries
is a system setting (`vendor_openai_codex_client_version`) that an operator has to
raise by hand when OpenAI ships a newer Codex app, or new models stay hidden — a
maintenance burden accepted with the reverse-engineered backend
([§11.1](11-risks-and-technical-debt.md#111-operational-risks)). Two smaller
pieces ride along. An optional per-account `model_prefix` is now **applied**: a
model is listed and requested as the prefix plus the vendor's id, and the vendor
is still sent the bare id. And the principal's own vendor models appear in the
portal dashboard's live-routes table, while the admin Models management page
(`ManageModels`) deliberately stays the system's real models, so the two admin
views differ on purpose.
→ [External Vendor Accounts §6](cross-cutting/external-vendor-accounts.md#6-dynamic-model-discovery-and-the-model-prefix).

**Follow-up: usage and limits made visible and refreshable (also no new
decision).** An OpenAI subscription's usage snapshot used to be refreshed only as
the last step of a models refresh or connect, and only the account's detail view
showed it. It now has its own owner-only
`POST /api/portal/vendor-accounts/{id}/usage/refresh`, which pulls the usage
without re-listing the models and is fail-soft the way the models refresh is. The
detail view's usage panel calls it **lazily when opened** and from a refresh
button, and a server-side TTL of five minutes, kept in memory per account and
recorded only when the vendor was actually reached, stops a view-triggered pull from
asking the vendor again and again. The list endpoint now carries each row's snapshot
(fail-soft, so a snapshot that cannot be read never fails the list), and the portal
dashboard gains a flag-gated "Anbieter — Nutzung & Limits" section that reads only
those stored snapshots and never calls the vendor. The panel shows only the limits
the vendor reports and keeps a titled frame with an empty-state line for an account
that reports none. Choice (c) extends rather than changes: the ChatGPT usage endpoint
is one more reverse-engineered, ToS-restricted vendor call, so it is asked only when
a person looks (human-correlated, TTL-capped), not on a timer; its Business-plan
response has been confirmed live once, while the Plus/Pro `rate_limit` window shape
is still unconfirmed ([§11.1](11-risks-and-technical-debt.md#111-operational-risks)).
A **background refresher** that keeps every account's usage current without anyone
opening it is deliberately not built and remains a follow-up, to be opt-in and
default-off.
→ [External Vendor Accounts §5](cross-cutting/external-vendor-accounts.md#5-usage--limits).

→ [External Vendor Accounts](cross-cutting/external-vendor-accounts.md),
[Risks & Technical Debt §11.1](11-risks-and-technical-debt.md#111-operational-risks) and
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances),
[Data Model §1](reference/data-model.md#external-vendor-accounts-anbieter),
[API Surface](reference/api-surface.md#vendor-accounts-anbieter),
[Configuration & Environment Variables](reference/config-env.md).

## ADR-050 — Vendor-account access is a per-token opt-in, enforced in listing and routing through one prefix helper
**Context:** a vendor account ([ADR-049](#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted))
is personal, but until now **every** API token its owner had ever issued could route
through it: connecting an account silently exposed it (a subscription, with its own
usage limits and ToS risk, included) to each key the owner had handed to a script, a
CI job or a colleague's tool, and there was no way to say "this key may use my
OpenAI account but not my Claude subscription". A second need rode along: two
accounts of one vendor can only be told apart by their account-level `model_prefix`,
which is the same for every key, so a client that must see a vendor's models under
their original names (or under a different namespace than the portal uses) had no
way to.

**Decision — five choices, taken together.**
- **(a) A per-token policy, stored as one JSON column.** `api_tokens.vendor_provider_access`
  (migration 85, `text not null default ''`) holds `{"all", "accounts":
  [{"account_id", "prefix_override"?: {"enabled", "value"}}]}`. `all: true` grants every
  active account of the owner — future ones included — under each account's own
  `model_prefix`; `all: false` grants only the listed accounts, an empty list none.
  Per listed account an optional **prefix override** replaces the account's prefix for
  that token, and **an enabled override with an empty value serves the account's
  models under their original names, with no prefix**. One JSON value, not a link
  table: the policy is a small atomic document that rides on the token row already read
  at every lookup (no extra query on the hot path), and a deleted account needs no
  cleanup because a stored id that names no account matches nothing.
- **(b) Enforcement in both the listing and the routing path, through one helper.**
  `routing.TokenVendorPrefix(access, account) → (prefix, allowed)` is the single
  source: the model-listing overlay (`ownVendorAccountModels`) and the resolver
  (`resolveVendorAccount`) both skip a denied account and use `prefix +
  upstream_model` as the public name, so **an advertised name is a routable name and
  the reverse**. The resolver matches on that name and reverse-maps it to the raw
  vendor slug for dispatch (`ProviderModel`), so an override is a per-token naming
  layer over the account's one stored catalog. A listing alone is not access control;
  routing alone would advertise names it refuses.
- **(c) Strict opt-in, existing tokens included, no backfill.** A new token and every
  token that exists at upgrade default to no vendor access (the column's empty
  default *is* the policy). Granting access is an explicit act of the owner in the
  token editor.
- **(d) Collisions: reject at save, with a deterministic backstop.** With `all: false`,
  a policy whose effective public names collide across the selected accounts'
  current models is refused at create/update with `400
  portal.token_vendor_access_conflict` (unknown, foreign or repeated accounts and
  malformed override values are `400 portal.token_vendor_access_invalid`). Because a
  vendor can ship a colliding model after the save, the resolver keeps a backstop: it
  walks the accounts in stable id order and the first match wins, so a collision
  never makes routing ambiguous. `all: true` has no save-time check (it names no
  accounts) and rests on the same backstop.
- **(e) The opt-in binds API tokens, not the owner's own session.** The interactive
  portal session (and the trusted loopback chat acting for it) has no `api_tokens`
  row to opt in on and must keep every account in the chat picker, the Models page and
  the dashboard, so `sessionPrincipal` carries `VendorAccess{All: true}`. A run-as
  token honors its own stored policy; a service token (no user id) owns no vendor
  accounts and stays inert.

**Consequence:** the change is a deliberate **breaking default** for any token that
was already calling a vendor model: after the upgrade it is refused as an unknown
model and no longer listed until its owner opts it in
([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)); it
is bounded by the default-off master flag, so only a deployment that enabled vendor
accounts is affected. The parity contract is one function, so the two paths cannot
drift, and the stored `gateway_model` is no longer the routing key (it equals
`model_prefix + upstream_model`, which is what the resolver computes, so the native
prefix is unchanged). An empty override can place a vendor's own names next to a
self-hosted model of the same name; the existing `vendor_account_routing_mode`
decides which answers, as it does today. A collision that emerges after the save
shadows the later account's model under that name until the owner separates the
prefixes. A token's policy can only narrow, never widen, owner scope: it names only
accounts the owner owns.

**Rejected:** defaulting existing tokens to `all: true` (keeps every old key working
but leaves every previously issued key with access to accounts it was never meant
for — the exposure this decision exists to close); per-vendor rather than
per-account granularity (it cannot separate two accounts of one vendor, or a
subscription from an API key); a junction table (more moving parts and orphan
cleanup for a small per-token document); enforcing in routing only or in the listing
only (the listing would advertise names routing refuses, or the reverse); applying
the strict default to the owner's interactive session too (it would empty the chat
picker and dashboard of an owner's own accounts, with no token row to repair it on);
an "all except X" mode and a token-level prefix for self-hosted models (deferred, not
needed for this need).
→ [External Vendor Accounts §11](cross-cutting/external-vendor-accounts.md#11-per-token-vendor-access),
[Data Model](reference/data-model.md#4-migration-history-86-migrations) (migration 85),
[API Surface](reference/api-surface.md#token-vendor-access),
[Risks & Technical Debt §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-051 — Anthropic translate prompt caching is flag-gated, decided at the gateway by a hybrid auto-switch, and placed by the provider
**Context:** Anthropic caches a prompt prefix only where the request carries a
`cache_control` breakpoint. The native `/v1/messages` passthrough already relays the
client's own markers, but the **translate** path (an OpenAI-dialect client or the
portal chat reaching an Anthropic vendor account, api-key or subscription) renders
the body itself and emitted none, so a long system prompt, tool list and growing
history were billed in full on every round. Marking blindly is not free either: a
cache write is billed at a premium over plain input, a prefix below the model's
cacheable minimum is silently not cached, and a large one-shot that nothing re-reads
would pay the premium for nothing. The usage store already records both cache-read
(`cached_tokens`) and cache-write (`cache_write_tokens`) tokens, so the feature needed
no new accounting.

**Decision — six choices, taken together.**
- **(a) A flag-gated, experimental, default-off system setting.**
  `anthropic_prompt_caching_enabled` (bool, a plain key/value row in
  `system_settings`, so no migration) with a System settings toggle, mirroring
  `vendor_accounts_enabled`. Off means the translate render is byte-identical to a
  build without the feature.
- **(b) A hybrid auto-switch rather than "always on" or a per-request knob.** The
  directive is set only when the flag is on **and** the target is an Anthropic
  translate target (`routing.ProviderVendorAnthropic`, which covers api-key **and**
  subscription) **and** the estimated prefix reaches the model's cacheable minimum
  (512 / 1024 / 2048 / 4096 tokens by model family, unknown = 1024) **and** the
  prefix is likely reused (the conversation already has an assistant turn, or it is
  a portal-chat session). Caching therefore turns on only where the prefix is big
  enough to cache and likely to be re-read; a large one-shot is never cached.
- **(c) The decision lives at the gateway, the placement at the provider.**
  `applyAnthropicCachePolicy` computes the verdict at both dispatch hooks and writes
  it onto the upstream copy of the request as an internal
  `inference.Request.PromptCache` directive (`json:"-"`, never client-visible); the
  Anthropic client's `anthropicRequestBody` only honors it. The gateway owns the
  inputs the policy needs (the flag, the session source, the routed target and its
  model) and the provider stays a pure renderer, so a provider that does not support
  caching simply ignores the field, and the neutral model grows one optional,
  provider-agnostic hint instead of an Anthropic detail.
- **(d) At most two breakpoints, 5-minute TTL only.** One on the **last system
  block** (`system` is rendered as a block array; Anthropic's order of tools, then
  system, then messages means it caches tools plus system, the stable prefix) and one
  on the **last content block of the latest turn** (the growing tail). The marker is
  `{"type":"ephemeral"}` with no `ttl`. **The 1-hour TTL is deferred**: it needs
  Anthropic's extended-cache-TTL beta header on the translate request, whose current
  string must be verified live, plus a way to expose the TTL in the setting and the
  UI. The directive's `TTL` field is reserved for it.
- **(e) The flag is read through a gateway-side TTL cache.** The policy runs on every
  Anthropic translate dispatch and the settings store read is an uncached full-table
  read, so `anthropicPromptCachingEnabledCached` caches it for 5 s and
  `handleSystemSettings` invalidates it on a PUT that carries the key: the same
  trade-off and value as the vendor flags' cache. An unreadable flag reports off.
- **(f) The native passthrough is left untouched, and the Activity display does not
  change.** The passthrough keeps forwarding the client's own `cache_control`
  verbatim (a gateway that rewrote it would defeat a client that places its markers
  deliberately). The cache-read tile and grouped column are already visible by
  default and now populate; the cache-write tile and column stay hidden by default.

**Consequence:** turning the flag on can only lower the bill of a conversation whose
prefix clears the minimum and is re-sent, at the price of a write premium on the
first round; a prefix that clears the minimum but is **not** actually re-read inside
the 5-minute TTL (a chat left idle, a changing prefix) pays that premium for nothing.
**Prefix stability becomes a correctness requirement of the render**: the system
text, the tool list and order and every earlier turn must be byte-identical from
round to round, or cache reads silently stop landing; this is pinned by a test
(`TestAnthropicCachedPrefixIsStableAcrossTurns`) and any later change to the
Anthropic request builder must keep it. The token estimate is a deliberately cheap
`chars / 4` that skips tool parameter schemas, so it errs low: a request just over
the minimum may go uncached, which is today's behavior and never a regression. The
placement assumes the subscription path's Claude-Code masquerade block stays first;
its text is left untouched and the marker goes on the last block (with no caller
system text that is the Claude-Code block itself). Because an Anthropic
target exists only through a vendor account, the setting is inert in a deployment
with vendor accounts off.

**Rejected:** always-on caching with no flag (a prefix below the minimum is a free
no-op, but an unreused prefix over it costs the write premium, and the feature rides
on the experimental vendor path); a flag alone with no reuse test (it would mark every
large one-shot); placing the policy inside the provider (it would need the system-settings
flag, which the provider has no access to, and would mix a cost-policy decision into
what is otherwise a pure renderer); a per-request or per-token opt-in knob (more surface
than the problem needs while the feature is experimental); reading the flag uncached
(one extra database round-trip on the hot path of every Anthropic dispatch); marking
the passthrough too (the client already controls its own breakpoints); shipping the
1-hour TTL in v1 (needs the extended-TTL beta header, which has to be verified live
first); intermediate breakpoints for very long single turns (deferred until it is
shown to matter).
→ [External Vendor Accounts, Translate prompt caching](cross-cutting/external-vendor-accounts.md#translate-prompt-caching),
[API Surface](reference/api-surface.md#4-system-endpoints-apisystem),
[Configuration & Environment Variables](reference/config-env.md#anthropic-prompt-caching-system-setting-no-env-var-form).

## ADR-052 — OpenAI-compatible vendors are presets over one OpenAI client: a stored root URL plus a registry-derived path prefix
**Context:** users wanted to route through more hosted providers than OpenAI and
Anthropic — x.ai (Grok), OpenRouter, the Kilo Gateway and Google Gemini — and
through "anything else that speaks OpenAI". All of them serve OpenAI
`chat/completions` with a Bearer key, so the existing `OpenAICompatibleClient`
already handles the wire. Two things were missing. A vendor's endpoint was a
literal in the resolver (`api.openai.com`, `api.anthropic.com`) with no stored base
URL, and the client composed the literal paths `/v1/chat/completions` and
`/v1/models`, which fit neither Gemini's shim (`/v1beta/openai/…`) nor the Kilo
Gateway (`/gateway/…`). The per-user vendor-account entity of
[ADR-049](#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted)
already supplies ownership, the per-token opt-in
([ADR-050](#adr-050--vendor-account-access-is-a-per-token-opt-in-enforced-in-listing-and-routing-through-one-prefix-helper)),
the model prefix and the flag, so the question was how to widen it without a
bespoke branch per provider.

**Decision — six choices, taken together.**
- **(a) The preset registry is data in `internal/routing`, not in the portal.**
  `vendor_presets.go` holds one `VendorPreset` per new vendor id (`xai`,
  `openrouter`, `kilo`, `google`, `openai_compatible`): its default root, its path
  prefix, how its key is validated, how a discovered model id is rewritten, and a
  reserved, empty usage slot. It sits in `routing` because the resolver needs the
  prefix and routing cannot import the portal; the portal reads the rest to compose
  plain URL strings, so `vendorauth` stays capture-only and never learns a vendor
  id or root. No new package and no new import edge, so the architecture-test
  allowlist is unchanged.
- **(b) `base_url` is stored as a root; the prefix rides on
  `Target.OpenAIPathPrefix`, and empty means `/v1`.** The new immutable column
  `vendor_accounts.base_url` (migration 86, `text not null default ''`, `''` for
  `openai`/`anthropic`) holds the root **without** a path prefix, so
  `https://api.x.ai`, `https://openrouter.ai/api`, `https://api.kilo.ai/api`. The
  prefix is looked up from the registry by vendor id at resolve time and set on
  the target; the client composes `{Endpoint}{prefix}/chat/completions` and
  `{prefix}/models`, and the gateway's usage-label path reads the same normalised
  value (`Target.OpenAIPathPrefixOrDefault`, the one place the default lives). An
  unset prefix is `/v1`, which is what every self-hosted target, probe target and
  OpenAI vendor target already used, so every existing caller composes
  byte-identical URLs; tests pin the composed chat and models URLs for the empty
  default and the Gemini and Kilo prefixes (the client) and for every preset (the
  registry).
- **(c) Translate-only and api-key-only.** The resolver builds the target with
  `Provider = vendor_openai` (the existing client, so no dispatch wiring changes),
  Bearer auth, `APIFlavors = [openai, anthropic]` and zero endpoint modes: no
  Responses or Messages passthrough, which exist only for the native OpenAI and
  Anthropic accounts. A subscription account of these vendors is refused
  (`vendor_account.auth_type_invalid`): none of these providers has a consumer
  subscription OAuth path, and the reverse-engineered, ToS-restricted one of
  ADR-049 (b) is not a model for new providers.
- **(d) Validation and discovery are per preset, with the existing uniform
  fail-soft classification.** Test connection probes `GET {base}{prefix}/models`
  (x.ai, Google, Custom), `GET {base}/v1/key` for OpenRouter (its `/v1/models` is
  public, so a listing would call every key valid) and nothing for Kilo (a public
  listing and no key endpoint, so the honest answer is "could not verify"). The
  classification is not bent per provider: 2xx is valid, 401 invalid, everything
  else unverifiable. x.ai and Gemini answer a wrong key with 400, so there a wrong
  key reads "could not verify", never "invalid" — fail-soft, and a key is never
  misreported as rejected. Discovery is `GET {base}{prefix}/models` (Gemini's
  leading `models/` is stripped from the ids), runs best-effort when an account is
  created with its key (bounded to 5 s, never failing the create, since these
  accounts have no static seed) and on Refresh, and its per-discovery cap rises
  from 500 to 1000 models for the aggregators (OpenRouter about 460, Kilo about 390
  at the time of writing).
- **(e) Gemini through its OpenAI shim now; the native dialect and usage are
  deferred.** Gemini is served at `/v1beta/openai` through the shared client. The
  native `generateContent` dialect and Gemini-only features are a later
  sub-project, and usage/credits display is another (the registry's usage slot is
  the seam): until then these accounts report no usage, the passive header scrape
  finding none of the headers it knows.
- **(f) The Custom base-URL trust model.** For `openai_compatible` the owner
  supplies the root. It must be **https** with a host and carry no userinfo, query,
  fragment or space (the key rides on every request, so plaintext and
  credential-in-URL are refused); the host is otherwise unrestricted. This is an
  on-prem gateway and the URL is set by an authenticated owner for their own
  account, so a Custom account can make the gateway call any https host the owner
  names, internal ones included: server-side request forgery is **accepted**, with
  its limits recorded in
  [§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances) —
  redirects are refused on validate and discovery, and there is no DNS-rebinding
  dial guard in v1.

**Consequence:** a new OpenAI-compatible provider is one registry entry (root,
prefix, probe, id rewrite) plus a vendor id and a UI label, not new client or
resolver code. The shared client now has a prefix seam that the self-hosted and
OpenAI paths also traverse, which is why the empty-means-`/v1` default and its
no-regression test are load-bearing: a change to the default silently moves every
OpenAI-dialect call in the gateway. The presets' roots, prefixes and probe
endpoints are VERIFY-LIVE data (confirmed on 2026-10-10) that a provider can move,
and a moved one degrades to "could not verify" or an unchanged catalog. `base_url`
being immutable keeps the root and the preset's behavior from drifting apart on an
existing account, at the price of a new account to change it. An OpenAI-compatible
account serves translate-only, so a Responses or Messages client reaches it through
the neutral model and loses what only the native vendors' passthrough carries.

**Rejected:** a bespoke resolver and client branch per provider (four copies of
the same wire, and every new provider a code change); a free-form `base_url` with
no preset identity (no way to know a provider's prefix, key probe or id rewrite,
and nothing for the later usage work to key on); a new leaf package for the
registry (extra architecture-test allowlist edges for what is a small table the
resolver must read, and routing cannot import the portal); storing the path prefix
in a column (a second source of truth that can disagree with the preset, and a
migration per provider that changes its path).
→ [External Vendor Accounts §4.4](cross-cutting/external-vendor-accounts.md#44-openai-compatible-vendors),
[API Surface](reference/api-surface.md#vendor-accounts-anbieter),
[Data Model](reference/data-model.md#4-migration-history-86-migrations) (migration 86),
[Risks & Technical Debt §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).
