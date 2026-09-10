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
[Data Model §4](reference/data-model.md#4-migration-history-79-migrations),
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
[Data Model §4](reference/data-model.md#4-migration-history-79-migrations),
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
[Data Model §4](reference/data-model.md#4-migration-history-79-migrations),
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
[Data Model §4](reference/data-model.md#4-migration-history-79-migrations),
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
value outside the three, stating a capability whose legacy boolean the same
request also sends, and two keys that name one capability once trimmed. Its
store error is PROPAGATED, unlike the accompanying upsert's best-effort write:
relinquishing the verdict is the whole effect of the action, so swallowing the
failure would report success for nothing. What is NOT closed is minting one by
accident from a form gone stale mid-edit
([11.1](11-risks-and-technical-debt.md#111-operational-risks)). What is open
is the *vocabulary*, not the validation: no check compares a name against a
known list, so the code reasons about `vision`, `video`, `audio`, `tools`,
`mtp`, `live_progress` and `speculation_observed` while an unrecognised
upstream name is accepted,
stored and shown verbatim — which is why the open vocabulary needs no escape
hatch. Two verdicts reached the request path through the candidate query's own
**filtered** LEFT JOINs when this decision was taken; `mtp` and its join went
with the scorer's flat bonus, so today there is one
([ADR-040](#adr-040--the-flat-mtp-bonus-is-deleted-mtp-splits-into-a-declared-trait-and-an-observed-one)). It lands on `MappingCandidate`,
deliberately **not** on `ModelMapping`: a mapping loaded through `MappingByID`
joins nothing, and a struct with no capability field cannot present a
plausible-looking but unpopulated verdict — anything holding only a mapping
has to ask for the rows. **(b) The precedence rule is a RANK,** not a
probe/not-probe split: `manual` 3 > `vision_benchmark` 2 >
`llama_cpp_props`/`ollama_api_show`/`legacy`/**any unrecognised source** 1 >
no row 0, and a write is permitted **iff `rank(incoming) >= rank(current)`**
(`WritableCapabilityRows` — pure, no I/O, applied by each writer rather than
by the store, because only a writer knows what rank its own evidence carries).
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
feature still stays inert. **Consequence:** a filtered join keeps one
row per mapping — the `(mapping_id, capability)` primary key guarantees it —
and measured ≈ +6 µs against the query's ≈ 17 µs, where one *unfiltered*
join costs ≈ +79 µs and multiplies rows. There were two of them here, and
[ADR-040](#adr-040--the-flat-mtp-bonus-is-deleted-mtp-splits-into-a-declared-trait-and-an-observed-one) later removed the `mtp` one;
this measurement is what makes that deletion a saving on the request path
rather than a wash. Two read paths cannot use them and
read the row themselves instead: the affinity path (it resolves before the
candidate query and returns its pin early, on a mapping that came from
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
path's own speculation observation) and six readers (the candidate query, the affinity path, the benchmark runner, the
models-list vision fold, the portal's per-row chips, and the mapping DTO that
seeds the operator's edit form and is compared against on save). Every rule
here is a rule about a value all of them touch, and a rule written with only
its own writer in mind is what repeatedly failed: a probe outranking a human,
a form save laundering an unchanged submission into a permanent `manual`
verdict, a heuristic wired at one of its two creation sites. The rank a new
writer may claim, and what it must never overwrite, follow from that
enumeration — not from the writer's own point of view.
**Rejected:** keeping the columns and adding a `capabilities_locked` flag (a
second mapping-wide lock, still with no per-capability provenance, and still
asking an operator to pin a value they should simply be able to state); and a
third `unknown` verdict value instead of row absence (it would put back the
empty verdict every writer has to remember not to write, which is the bug
class this shape removes).
→ [Data Model §1](reference/data-model.md#1-current-tables-by-area),
[§4](reference/data-model.md#4-migration-history-79-migrations),
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
for every other rank-1 capability — the losing writer rewrites its row on the
next cadence tick — is precisely what a writer that never runs twice does not
have. An agent's `no` would otherwise stand until a restart.
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
behind the claim, so a speculating mapping costs one store round trip in the
whole life of the process and every completion after that costs a map lookup.
**No capability verdict influences model selection any more** — `scorer.go`
names none, `live_progress` is the one verdict the candidate query still
joins, and it decides what the gateway SENDS upstream rather than which
upstream it picks.
**One discipline this deletion earned, for whoever deletes the next behaviour:
sweep the user-visible prose, not only the code.** Removing the bonus took
three separate follow-up fixes, because its claim had been restated in three
places nothing pointed at from the scorer: a comment on the name heuristic,
the reserved-capability-name doc at the agent ingest, and — found only because
a task was explicitly told to look for it — the **i18n copy under the
operator's own MTP control**, which went on telling them in both languages
that resetting the verdict to unknown would also drop "the MTP bonus in server
selection". An i18n string is an assertion about behaviour, it is the one the
operator actually reads, and nobody greps it ([Theming &
i18n §8](cross-cutting/theming-and-i18n.md#8-internationalization)). Enumerate
the places a behaviour was *explained* the way
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
