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
each declare a list of **named feature flags**, and a feature is active if and
only if a string-equal name appears on both lists. Agent → gateway rides the
telemetry sample's existing `capabilities` object; gateway → agent is an
ETag-conditional `GET /api/agent/v1/features` — deliberately not a hello frame,
so it works identically for POST and WebSocket agents. Negotiation is re-decided
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
frame type behind its own feature flag.
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
the first time that derivation grows a field. **Consequence:** twelve call sites
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
[Data Model §4](reference/data-model.md#4-migration-history-73-migrations),
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
whose `args` mention none of the three placeholders — and enforce the
identical pair again at launch for the file-mode path the portal never
reaches; the existing conflict and empty-GPU-list refusals apply unchanged in
both modes. Declare one agent feature flag for all of it, `gpu_selection`
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
[Data Model §4](reference/data-model.md#4-migration-history-73-migrations),
[API Surface](reference/api-surface.md#agent-managed-model-runtime).
