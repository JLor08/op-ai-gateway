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
ADR-043 later granted separately. `/v1/images/edits`, added by
[ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance),
takes the same loopback-or-bearer leg as `/v1/images/generations` and never the
session cookie; the portal's edit turns reach it over the same loopback pair.
`/v1/audio/speech` takes that leg too
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)
(j)), and the portal's speech turns reach it over the same loopback pair
([ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)). See
[Security, Authentication & Authorization
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
`POST /v1/audio/speech` is a further uncapped JSON endpoint, admitted the
same way ([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)):
its body is a short JSON object, and the gateway buffers it whole like the others.

**Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance)
(the one capped inference body).** `POST /v1/images/edits` is the sixth inference
endpoint and the **first that does not read uncapped**: its body is
`multipart/form-data` (not one JSON value, so `readRawJSONUnlimited` cannot read
it) and the handler holds it whole in memory — it peeks the form before routing
and, for a mask conversion or a model-name rewrite, rebuilds it — so an unbounded
read there is a memory exposure the JSON endpoints' uncapped read (matching
llama-swap) does not have to answer for. It reads at most
`OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES` (default **32 MiB**, equal to the server
agent router's own request cap; junk, `0` or a negative value uses the default):
a declared `Content-Length` over it is refused without reading a byte, an
undeclared body is stopped by `http.MaxBytesReader`, both `413
request.body_too_large`. Because preparing a body can grow it (a converted mask, a
longer model name, a new boundary), a body bound for an agent-launched model is
re-checked against the agent's own 32 MiB after the rebuild and refused with the
same code rather than sent. The bundled nginx already has `client_max_body_size 0`
on `/v1/`; a reverse proxy in front must allow at least the configured value on
`/v1/images/edits`, and the gateway's setting, not the proxy's, is then the
effective limit ([Configuration
Reference](reference/config-env.md#encrypted-payload-capture)).

**Amended again by [ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint)
(the second capped inference body).** `POST /v1/audio/transcriptions` is the eighth
inference endpoint and the second whose body is `multipart/form-data`, so it reads
capped for the same reason, at its own setting,
`OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES` (default **32 MiB**, the agent
router's cap; junk, `0` or a negative value uses the default), refused `413
request.body_too_large` from a declared `Content-Length` without a read or by
`http.MaxBytesReader`, and re-checked against the agent's 32 MiB after a rebuild. It
is a variable of its own so that audio and images are sized independently. The same
value also bounds the **response** the relay buffers to read the trailing `timing` block,
so a 2xx over it is refused 502 `transcription.upstream_error` rather than clipped
([Compatibility & Inference
§3.7](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions)).

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
would void every such capability pass. `capability_source_audiocpp` (issue #255) is the
same shape for the `audiocpp_models` source and an agent-launched audio.cpp child. It is
also why streaming transcription (issue #262,
[ADR-065](#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript))
needed **no new flag**: the agent's `mode`-derived `transcription_streaming` verdict rides
this same already-declared source, so adding it was a **PATCH** bump (0.9.1 → 0.9.2), not a
new `agent.Features` entry — a verdict whose absence already means the correct buffered
behaviour, the "the flag gates the FALLBACK" pattern above. The
agent declares nothing back: the gateway is the side that must accept the source,
nothing on the gateway waits on the agent, and an agent-side name would therefore gate
nothing — so a flag on one list only is not a half-finished negotiation, it is the
name placed on the side whose fact it is.

`runtime_config_files` (issue #257,
[ADR-066](#adr-066--agent-provisioned-config-files-one-child-per-spec-a-0n-config-files-facet-written-to-an-agent-owned-directory-a-config_dir-placeholder-and-non-secret-content-only))
is the portal-informational shape on the agent's list alone: the agent always
honors the config files it receives and gates nothing on the flag, while the
portal reads it to warn that a connected agent older than `0.10.0` would launch the
child without the file — a fact no probe can tell apart from a spec that never asked
for one.
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
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
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
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
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
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
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

**Amended by [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)
(a second allowlisted endpoint, issue #251).** "An allowlist of exactly `/props`" is now an
allowlist of exactly **two** endpoints: `GET /upstream/{model}/v1/audio/voices` joined it,
for an audio.cpp child's voice listing, behind its own agent feature (`runtime_voices`, agent
0.11.0) and its own ADR, as this decision said a widening would need to be. Every guardrail
above carries over unchanged (GET-only, `Status()`-only and never-start, the path
decomposition, the gateway's fail-closed gate on the declared feature); what became
per-endpoint is whether the inbound query is forwarded (`forwardQuery`: kept for `/props`,
dropped for voices, because audio.cpp ignores `?model=`).
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
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
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
[§4](reference/data-model.md#4-migration-history-95-migrations),
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
columns arrive in the same migration ([v81](reference/data-model.md#4-migration-history-95-migrations)),
because a quantity without its unit is the scalar this entry rejects and a unit
without its quantity records nothing. The quantity is only ever read *through*
the unit: whoever wants a number must first agree what it counts. The
vocabulary is deliberately small and open only at the code level —
`BillingUnitTokens` (`""`), `BillingUnitImage` (`"image"`),
`BillingUnitAudioSecond` (`"audio_second"`) in
`internal/usage/billing.go`. Two named units, not three: `"character"` belongs
to an external price list rather than to this system, and audio.cpp's own
metering signal is a *duration*, so speech and transcription share one unit.
(Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives): a third non-token unit, `BillingUnitAudioBytes`
(`"audio_bytes"`), joined the vocabulary for a streamed speech response whose duration
cannot yet be derived; see the amendment after the Rejected list.)

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
(Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives): a non-token endpoint may carry a success-path
*measurement-unit* refinement chosen from mapping state, never from the response, provided every
failure row keeps the endpoint-identity unit; see the amendment after the Rejected list.)
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

**Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)
(streaming speech synthesis, issues #253 and #263): a fourth unit, and a success-path
measurement-unit refinement with failure rows uniform.** A streamed speech response carries
no duration, so the relay counts the PCM bytes it relayed. `usage.BillingUnitAudioBytes =
"audio_bytes"` joins the vocabulary (`ValidBillingUnit` accepts it; the XOR and the schema
needed no change, `billing_unit` being open text) and is the **fallback** measure of a
cleanly completed streamed call whose mapping has no cached output byte rate yet; once the
rate is cached the same call bills `audio_second`, bytes over the rate. Decision (c) is
**preserved in the form that protects it**: every failure, refusal and incomplete-stream row
of the speech endpoint stays `(audio_second, 0)`, the endpoint-identity unit, so a failed
request is never mislabelled; only the **success** path chooses between the two audio units,
and it chooses from **mapping state** (is the rate cached?), not from the response body. The
amendment is exactly this: a non-token endpoint may carry a success-path *measurement unit*
refinement, provided its failure rows are uniform. `audio_bytes` is unpriceable in Tier 3 (the
modeled tier's non-token guard) until a coefficient declares that unit.
→ [Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[§8.4.2](cross-cutting/telemetry-usage-observability.md#842-query-stats-groups-time-series),
[§8.4.4](cross-cutting/telemetry-usage-observability.md#844-energy-attribution),
[§8.4.5](cross-cutting/telemetry-usage-observability.md#845-cost-and-currency),
[Data Model §1](reference/data-model.md#1-current-tables-by-area),
[§4](reference/data-model.md#4-migration-history-95-migrations),
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
(Amended by [ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec)
(h): `handleOpenAIModels` now keeps the `Models()` rows whose `flavors` contain
`openai`, which are the same names; no capability row decides which names are
listed, though `Models()` does batch-read the capability rows to fill each row's
`vision`, `image` and `image_edit` fields. The hardcoded flavor is now
`openAIModelDTOs`'s.)
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

**Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance)
(`image_edit` is a derived capability, and it is on the agent's reserved list).**
The gate, its fail-closed direction, (c), (e) and (f) stand for every capability
that is read from a row. One name is not: `image_edit`, the capability of
`POST /v1/images/edits`, is **derived** from `image` at the gate
(`routing.ImpliedImageEdit` = `image: yes` and not an operator's `image_edit: no`),
so two statements above no longer hold for it. The Decision's "a `yes` verdict for
each required name" and (a)'s "an ABSENT capability row means unknown, and unknown
REFUSES" — for `image_edit` an absent row means *derive from `image`*, which is the
one documented exception, and it is bounded: any other required name is still a
literal `yes` row, and a failed capability read still refuses (no rows derive
nothing). And (f)'s "`image` must NOT be added to `reservedAgentCapabilityNames`"
has a counterpart rather than a contradiction: `reservedAgentCapabilityNames`
**gains `image_edit`**, because — unlike `image` — no legitimate agent writer
exists for it, and an agent-reported `image_edit: no` would otherwise silently
disable editing at rank 1. `reservedManualVerdicts` is **unchanged**: `(image_edit,
no)` is the operator's opt-out and is already writable (the vocabulary is open),
while `(image, no)` stays reserved by (b). The two lists still must not be kept in
step. → [Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate).

**Generalised by [ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)
(the second endpoint, `POST /v1/audio/speech`).** The Decision's claim that the next
endpoint "inherits this gate, and both of its affinity write guards, by naming their
own capability" is now exercised: speech declares `RequiredCapabilities = ["speech"]`
and neither the capability gate nor the affinity guards changed. Two forecasts above did not come true as
written. Speech did **not** share a coarse flavor with a text endpoint, as the
affinity paragraph allowed for: it took a coarse flavor of its own, `openai_audio`,
so the guards stay redundant for it exactly as for images. And a capability list is
now known to do a second job, keeping a request off the vendor-account path, which
excludes the images flavor by name and has no audio twin.

**Generalised again by [ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint)
(the third endpoint, `POST /v1/audio/transcriptions`).** Transcription declares
`RequiredCapabilities = ["transcription"]` and neither the gate nor the affinity guards
changed. It shares speech's coarse flavor, so the guards are redundant for it as for
speech, and its one new lesson is that two endpoints may share a flavor, which moves
their differences off the flavor string and onto the endpoint.

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
verdict is `yes` makes every turn in that thread an image turn.** (Amended by
[ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance):
"the ONLY affordance" no longer holds for a model that can also be edited — an
image attached to the turn is a second one, see the end of this paragraph.) No mode
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
true.) (Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance) — R1, the attached image: "One axis" is superseded.
For a model that is also **edit-capable** (`image_edit`, derived from `image` and
removable by an operator, folded across a model's mappings exactly as `image` is,
`ModelDTO.ImageEdit`) an image **attached** to a turn of an image thread makes
that turn an **edit** of it. No toggle and no "edit" button come back with it:
the attachment is the user's *operand*, not a mode, and what the composer offers
is still decided by the picked model through a model-derived flag. The sentence's
last clause is kept by the new flag rather than by the old axis — the composer
offers the attach control only where the gate will serve an edit, and the same
derivation feeds both — and the single predicate that says so
(`attachMode`, a function of the thread's kind and the picked model's flags)
deliberately does **not** consult `vision`: an sd model is `vision: false` and
edits. A turn is an edit or a generation by what its last user message carries;
the thread's kind stays `image` across both.)

**Rejected: a mode toggle.** A second, independent axis of user intent makes
reachable a state the capability gate must then refuse **at the bottom of the
stack, after the user has already committed a prompt**, and it forces a second
question — what happens when the toggle is on and the model says no? — whose
only good answer is to disable the toggle, which is this decision with extra
steps.

**Consequence: `vision` and `image` are one word apart and point in opposite
directions, so the attach control stays gated on `vision` and its refusal had
to be reworded.** (Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance):
on an **image** thread the attach control is no longer gated on `vision` but on
`image_edit`, see the end of this paragraph.) An image *generator* has no reason to accept an image
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
UI writes the enabling verdict, never the refusing one. (Amended by
[ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance): an
image *generator* may well accept an image — as the source of an edit. On an
image thread the attach control therefore follows `image_edit`, never `vision`:
it is enabled for an edit-capable model and refused for a generator whose
operator opted it out of editing, which keeps this paragraph's sentence about a
generator taking no input, now as the refusal of exactly that model. The two
axes stay separate — `vision` is whether a *text* model reads an image, and on a
text thread the control still needs it and still refuses a model that also
generates images. The prompt field's hint gets an edit-capable variant beside
`chatImageOnlyHint`.)

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
speech kind without a second flag; **`speech` is that kind now**, a third value beside
`""` and `image` that selects `/v1/audio/speech`, with the pin, the clearing of
non-matching settings and the UI-constraint-not-authorization rule below carried over
unchanged, [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs) (a)) (Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance):
the kind alone no longer selects the request URL — for the image kind the URL is a function of the kind
**and** the turn: a last user message that carries an image posts to
`/v1/images/edits`, any other to `/v1/images/generations`. The kind stays `image`
across both; there is no edit kind, and the decision is the executor's, read off
the prepared history, never a client setting), `omitempty` so every pre-existing chat
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
whole-document ceiling is accepted for v1 rather than worked around.** (Amended
by [ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url): the payload
now lives in its own sealed store and this ceiling no longer binds it, see the
amendment at the end of (c).) It is
persisted as an OpenAI-style `image_url` content part carrying a `data:` URL —
the identical shape an uploaded vision image already has, so history
construction feeds it back as a vision input on the next turn for free and the
existing renderer applies to it. (Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance):
"feeds it back for free" holds for a *text* thread's vision replay only. An image thread's executor reads
no history but its LAST user message, whose images it sends as the source of an
edit — an upload's stored reference, resolved to bytes server-side exactly as a
text run resolves its history's, or the stored `image_url` of an earlier result
that a later turn names as its source, which is the same shape reused and never
copied or re-embedded. An older image of the thread is still never sent.) This is policy-compliant rather than a hole in
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
needs in order to work. Filed as #124, and **resolved** by [ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url),
which built exactly that store. — **Rejected: a server-side re-encode to
fit the cap.** The client already downscales its **own uploads**; re-encoding
what the *model produced* is a different act, because it silently degrades the
artifact the user asked for. Upstream bytes are stored as-is, with a real
binary download so the user can keep the original.

**Amended by [ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url) (the payloads leave the transcript).** The shape
this decision chose stands: an image is still an OpenAI-style `image_url` part,
it still replays as a vision input for free, it still inherits the transcript's
sealing (now through the same cipher on its own row), and "stored as-is, with a
real binary download" still holds. Four statements above no longer describe the
code. (i) "Stored INLINE" and the 4 MiB ceiling "accepted for v1": a generated
(and an uploaded) image is extracted at the run commit into the owner-scoped
`chat_blobs` store and the part carries `/api/portal/chat-blobs/{id}`. (ii) The
cap as a budget every image shares "for the thread's life": it is still
`portal.MaxChatContentBytes` and still checked on the sealed document, but it
bounds the **text** only, so a busy image thread no longer runs out and a legacy
inline thread migrates on its next run. (iii) "Rejected: a separate blob store
for v1" and "Lifting the ceiling is issue #124": both are resolved, and the
inline-image entry in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances) is
retired (what is accepted now is recorded there in its place). (iv) "The image
rides inside the sealed blob": true only of a chat that has not run since the
change. What stays is the loudness: a failed
terminal commit still ends the run as an error (its causes are now a too-large
*text* document or a payload the store refuses, no longer an image that outgrew
the shared budget), and the terminal event still carries only what was stored.

**(d) The composer leads with the one number that is EXACT, and shows no
progress at all.** (Superseded in part by [ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url) (g): the
remaining-capacity line and the Send refusal described here are removed; the
wait elements and the absent character counter stand.) Four independent composer designs all spent their effort on
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
simply be declined. (Amended by [ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance): an edit turn adds an
upload to the composer — and, for an inpainting edit, a mask — and neither has a
number of its own to state either. Since
[ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url)
a chat image is a reference out of the sealed transcript, so the shared 4 MiB
budget this paragraph measures against does not bind an edit at all. Three
different bounds apply, by where the bytes are. A stored image — an edit's source
or mask once it is a `chat_blobs` row, so a **refine**'s source, which is a stored
blob resolved server-side — is bounded by the store's per-blob cap
`MaxChatBlobBytes` (default 20 MiB, also the composer's per-image cap), and the
whole edit body the executor then sends over loopback — source, mask and a
handful of fields — by the edits endpoint's own multipart cap
(`OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES`, default 32 MiB, see
[ADR-009](#adr-009--no-body-size-cap-on-inference-1-mib-on-control-plane)), not by
the 4 MiB text-document cap; two maximal stored files can exceed 32 MiB together,
and the run then ends `request.body_too_large`. A freshly **uploaded** source or
mask is different: it additionally rides **inline** in the run-start body
(`user_message`), which the run-start endpoint reads under
`portal.MaxChatRequestBytes` (5 MiB, the 4 MiB content cap plus the JSON envelope,
[ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url)
(f): "one upload still rides inline in the run body"). An upload of an edit is
therefore refused `413 request.body_too_large` at about 5 MiB, long before the
20 MiB and 32 MiB figures, for the composer and a raw API client alike; only a
stored source can approach those. What the composer
does refuse up front is the edit shape the server would otherwise answer with
its own refusal after the fact: more than one source image, an edit with no
prompt, a source that is not PNG or JPEG (a HEIC photo is converted to JPEG
first), and an attachment on a model the gateway will not edit.)

**The cap is SERVED, never duplicated.** `MaxChatContentBytes` was package-
private and no DTO carried it. A second `4 << 20` literal in TypeScript would
drift from the Go constant with nothing to catch it, and the failure mode is a
capacity line that confidently states the wrong number — the exact defect the
line exists to prevent. It rides on `ChatListResponse.max_content_bytes`, on
the listing the chat view already fetches, and it is deliberately **not**
`omitempty`: a missing field and a zero are the same thing on the wire, and the
portal reads zero as *capacity unknown* (no line, no refusal), so an accidental
zero disables the feature instead of inventing a number.

**Amended by [ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url) (g) (the capacity estimate is removed).** The
argument above was sound while an image consumed the shared budget the composer
could measure. Once the payloads live in `chat_blobs` it is not: a stored image
is a ~60-character reference, so the client's estimate (the largest `image_url`
URL as the per-image cost, divided into the served cap) would report millions of
images left, and the cap no longer binds images at all. The composer therefore
**states no remaining room, and `send()` no longer refuses an image turn on
capacity**; the estimate (`imageCostBytes`, `imagesLeft`, the document-size
helper), its strings and its tests are deleted rather than retuned. What
survives of this paragraph: `ChatListResponse.max_content_bytes` is **still
served**, still not `omitempty` and still `portal.MaxChatContentBytes`, but it is
informational and **no portal code consumes it**; "served, never duplicated" is
the rule if anything ever shows it again. A text document that overflows the cap
is still the server's `400 portal.chat_too_large`, and the new bounds that
replace the shared budget (`MaxChatBlobBytes`, `MaxRunHistoryBytes`) are
server-side and not estimated in the client.

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

**(e) The run deadline applies to the IMAGE kind only.** (Amended by
[ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(b): the **speech** kind is bounded too, by a variable of its own,
`speechRunDeadline`, of the same 10 minutes and for the same reason, since a speech run
is also one buffered request with no incremental output. Read "the IMAGE kind only" in
this choice as "the kinds that emit nothing between dispatch and the finished artifact".
The text kind stays unbounded, and the checkpoint polarity at the end of this choice
holds for speech as it does for image: only the text executor checkpoints.) A bound is what turns
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
only `/props` through per model (an audio.cpp child's voices, from 0.11.0, are
the one other endpoint, [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)) — and reports the verdict up the telemetry
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
gate would serve. (Amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(e): the chat also offers an `openai_audio` model that carries `speech: true`, by a rule
of the chat's own that leaves the shared offering function as it is.) ADR-042 (d)'s gap therefore narrows:
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
target, a name that no flavor serves lists `flavors: []`, and the LM Studio
listing lists what `/v1/models` lists (`/api/v0/models` then, `/api/v1/models`
since ADR-045 (h)). What stays is that no capability row decides which names
`/v1/models` lists (the membership is flavor-based; since ADR-045 (h) the
capability rows only fill the `Models()` row fields behind it), and that an
images client reading it still sees the chat models; ADR-042 (d) is narrowed
again, not closed.

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
`/api/v0/models`, then the LM Studio-shaped listing, listed every portal model
as `"llm"`, unfiltered, images-only models included (issue #148; that listing is
retired, see (h)). Only the listing's `image` flag already read the spec
(ADR-044 (f)).

**(Amended — model context-size discovery, issue #171: (h) retires
`/api/v0/models`, serves LM Studio's v2 listing at `/api/v1/models`, adds
`context_length` to `/v1/models` and records the tools policy of the LM Studio
listing. Everything else below stands; the sentences it changes point to
(h).)**

**(Amended — full model capabilities, issue #222: the `capabilities` object is
extended from `{vision, trained_for_tool_use}` to `{vision, image, image_edit,
trained_for_tool_use}` and is now carried on `/v1/models` too; both listings
build it from one shared `modelCapabilities` helper, so they stay identical.
`vision` and `trained_for_tool_use` are the keys an LM Studio client / opencode
read; `image` and `image_edit` are gateway extensions beyond those two keys.)**

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
`flavors` of `Models()` and `ManageModels()`, `ModelsForFlavor` (the name set
a test pins every listing to; it was the source behind `/anthropic/v1/models`
and, before (h), `/v1/models`, both of which now read the `Models()` rows —
`/anthropic/v1/models` since issue #225, (h) item 5),
`ModelOffering.Callable` and the group fold of `Capable`, and through them
`callableModelNames` and the redirect, with no code change there. `Existing`
uses the flavor half, for its names and for its group overlay alike: a name
that fails a flavor's flavor half does not exist under that flavor, exactly
like a name whose application lacks the flavor, while a name whose messages
endpoint is `disabled` still exists under `anthropic` but is not callable.
The LM Studio listing keeps only the DTOs whose `flavors` contain `openai`
(through `openAIModelDTOs`), and keeps `"type": "llm"`; `modelsResponse` and
`flavorSetsFromViews` apply the same views, group overlay, suppression and
override aliases, so it lists exactly `/v1/models`' names, groups and aliases
included. (Amended by (h): this was `/api/v0/models`, `handleLMStudioModels`;
`/api/v1/models`, `handleLMStudioV1Models`, now does it, and `/v1/models`
reads the same `openAIModelDTOs` rows.) `ServerModels` stays flavor-blind.

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
`openai_images` (amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(e): it also offers an `openai_audio` model that carries `speech: true`, by a rule of the
chat's own applied on top of the helper, which this choice keeps unchanged). A model whose `flavors` are exactly `[openai_images]` with
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

**(h) Amended — the LM Studio listing moves from v0 to v2, and `/v1/models`
carries the context window (model context-size discovery, issue #171).** This
ADR kept an LM Studio-shaped `GET /api/v0/models` that reported each model's
`max_context_length`, on the belief, repeated in four places (the compatibility
doc, the API surface table, the OpenAPI spec and the handler's comments), that
opencode auto-detects a model's context window from it. Per issue #171's
reading of opencode's source (not verified against opencode here) it does not:
no current opencode reads v0, and its LM Studio provider discovers from LM
Studio's native v2 listing, `GET /api/v1/models`, which the gateway answered
with 404, and a config-file `openai-compatible` provider discovers nothing.
The v0 listing's only in-repo consumer was `scripts/gen-opencode-config.sh`. The
decisions, which leave (a) to (g) as they are:

1. **`/api/v0/models` is retired, not deprecated.** The route,
   `handleLMStudioModels` and `lmStudioModelsFromDTOs` are gone, and the path
   falls to the catch-all (404 `request.not_found`). `openAIModelDTOs` stays,
   as the one source both `openai` listings below read.
2. **`GET /api/v1/models` and `/openai/api/v1/models` serve LM Studio's v2
   shape** (`handleLMStudioV1Models`): `{"models":[{"type":"llm","key",
   "display_name","max_context_length","loaded_instances","capabilities"}]}`,
   one entry per `openAIModelDTOs` row, so the ids are `/v1/models`' ids, groups
   and offered aliases included. `max_context_length` is **always** present,
   `0` when unknown: v2 requires the key, and listing a model with `0` keeps it
   visible (a client that discovers is understood to replace its model list, so
   omitting the key would hide the model) while the client keeps its own
   default for `0`.
   `loaded_instances` is `[{"config":{"context_length":N}}]` for a loaded model
   and `[]` otherwise; `models` and every `loaded_instances` encode as `[]`,
   never `null`, because opencode is reported to decode the response strictly,
   so one `null` would fail the whole listing. The pair is registered under `/api/v1/models` and
   `/openai/api/v1/models` and **not** under `/anthropic/`, which has no OpenAI
   chat-completions route a discovered model could be used with. Its auth is `requireAnyScope`,
   bearer-only, exactly `/v1/models`' (so a service token's `llm:invoke`
   discovers; v0's `requireScope("gateway:use")` refused it): discovery is not
   invocation, and the allowlist restricts the second only.
3. **`/v1/models` gains `context_length`.** It is built from the same
   `openAIModelDTOs(Portal.Models(token))` rows instead of the ids-only
   `ModelsForFlavor`, which returns the same names (a test pins the
   equivalence); `/openai/v1/models` shares the handler. The field is the
   portal's `ContextSize`, the minimum known across the offering mappings, and
   is **omitted** when it is `0`, unlike `max_context_length`: a client that
   reads `context_length ?? fallback` would be blocked by a `0`. The two
   endpoints therefore keep different contracts, deliberately. No released
   opencode is known to read this field yet; it is the place a generic
   OpenAI-compatible client reads a model's window, and
   `scripts/gen-opencode-config.sh` reads it
   (same `limit.context` / `limit.output = min(floor(context/4), 8192)` output,
   models without a context skipped).
4. **The tools policy: `capabilities.trained_for_tool_use` is the constant
   `true`** for every chat row. The gateway tracks no per-model tool support
   and the `Models()` rows carry no provider type, so there is nothing to
   derive it from. The caveat is the native Ollama client, the one place the
   gateway drops tools (it sends none to `/api/chat`): a model backed only by a
   native `ollama` application is advertised as tool-capable and loses its
   tools. No current deployment runs one; making the flag truthful is tracked as
   issue #219 and is out of scope here.

5. **`/anthropic/v1/models` follows the same pattern (issue #225).**
   `handleAnthropicModels` no longer calls the ids-only `ModelsForFlavor`: it
   keeps the `Models()` rows whose `flavors` contain `anthropic`
   (`anthropicModelDTOs`, the twin of `openAIModelDTOs` over one shared
   `modelDTOsOnFlavor`), so that a generated Claude alias row keeps the
   `original_model_id` and the honest `display_name` its `ModelDTO` carries
   ([ADR-058](#adr-058--claude-model-aliases-are-computed-per-token-from-one-shared-derivation-hashed-over-the-gateway-id-and-carried-as-sibling-metadata)).
   `ModelsForFlavor` stays, as the ids-only reference and `portal.API` method
   that all three listings are pinned against — the anthropic ids in order, for
   a token with and without the setting — and no longer as the production
   source of any listing. The cost is the one item 3 describes for
   `/v1/models`, now also paid by every `/anthropic/v1/models` call.

**Consequence.** An unknown window still has one fix for both listings: set the
mapping's `context_size`, by the `/props` probe or by hand. A vendor-only
model has no `context_size`, so it lists without `context_length` and with
`max_context_length: 0`. `/v1/models` now does one `Models()` read where it
did a `ModelsForFlavor` read (and `/anthropic/v1/models` likewise since issue
#225, item 5), and `Models()` does more than the ids-only list:
on top of the visible views, group overlay and runtime-spec read both share, it
adds a batch capability read (`MappingCapabilitiesForMappings`, for the
`vision`, `image` and `image_edit` flags — which, since issue #222, the OpenAI
listing now also emits in its `capabilities` object), a
second `model_settings` read (for each row's `Visibility`), and the per-view
loaded-state, offered-on, context and group assembly. That is the work the
portal chat picker and `/api/v1/models` already do per request, now also paid
by every `/v1/models` call.
**Rejected:** **keeping `/api/v0/models` as a deprecated alias** — no known
client reads it, and a dead listing is what produced the four wrong claims. —
**`context_length: 0` on `/v1/models`** — it blocks a client's fallback. —
**Omitting `max_context_length` at `0` on `/api/v1/models`** — v2 requires it,
and the model would vanish from a discovering client. — **A superseding
ADR** — the change amends (b)'s listing sources and keeps the rest, so it is
recorded inline.

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
  as the NetBird and certificate modules. A finer switch for the ToS-restricted
  subscription path alone, `vendor_subscriptions_enabled` (default **on**), turns
  off only subscription-type accounts and leaves api-key ones untouched (see
  [External Vendor Accounts §7](cross-cutting/external-vendor-accounts.md#7-feature-flag-and-routing-mode)).

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

**Amended by [ADR-060](#adr-060--shared-vendor-accounts-a-tiered-share-level-a-pure-routing-grant-authorization-by-level-and-a-per-account-opt-in) (sharing).** Choice (a)'s deferral of
sharing is over: an account stays owned by exactly one user and personal by default, but
its owner, if an admin, can now raise it to a share level and offer it through resource
groups, and the "owner-only" wording of the follow-ups above (the test-connection, the
models refresh and the usage refresh) now reads "the owner, or a co-manager of a shared
account, and the owner alone for a subscription account". The five choices of this ADR
are otherwise unchanged: the subscription path stays experimental and ToS-restricted,
and sharing one is capped and separately switched.

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
[Data Model](reference/data-model.md#4-migration-history-95-migrations) (migration 85),
[API Surface](reference/api-surface.md#token-vendor-access),
[Risks & Technical Debt §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

**Amended by [ADR-060](#adr-060--shared-vendor-accounts-a-tiered-share-level-a-pure-routing-grant-authorization-by-level-and-a-per-account-opt-in) (sharing).** The policy and the single helper
stand, with three changes. The policy may now also name an account **shared** with the
token's principal, and **`all: true` covers only the principal's own accounts**, never a
shared one, so borrowing stays a named choice (this refines (a), (d) and (e): the
interactive session is still `all: true`, and so sees its own accounts only; (d) is spelled out
at the end). `routing.TokenVendorPrefix`
takes the whole token, because the own-versus-shared line depends on its principal. And a
**service token** is no longer inert: it carries the policy, create-only, and reaches a
shared account only when it names it and the service is directly provisioned into a
resource group that contains it (this supersedes (e)'s "stays inert"). On (d) and the
Consequence's last sentence: `all: true` now coexists with
explicit entries naming a shared account, and each such entry is validated and
collision-checked at save like an `all: false` entry (an id the token's principal cannot
reach is the "unknown" `400`), so only an entry naming an **own** account goes unchecked
and rests on the backstop, and a policy now names accounts its principal can *reach*, not
only ones the owner owns, though it still never widens that reach.

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
  key reads "could not verify", never "invalid" — fail-soft: a non-401 response
  never reads as invalid. A moved endpoint that begins answering 401 would show a
  false "invalid" on Test connection, which blocks nothing (a key is not probed
  when it is saved). Discovery is `GET {base}{prefix}/models` (Gemini's
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
  finding none of the headers it knows. (Taken up by
  [ADR-053](#adr-053--usagecredits-for-api-key-vendor-accounts-a-per-preset-usage-strategy-over-the-existing-oauth-only-pull-path):
  OpenRouter and Kilo now report usage; x.ai, Google and Custom still do not. The
  native `generateContent` dialect is taken up by
  [ADR-054](#adr-054--native-google-gemini-dialect-its-own-vendor-id-and-provider-kind-selected-by-the-preset-dialect-field-over-a-leaf-translation-package),
  which supersedes this deferral: it ships as a separate `google_native` account and
  leaves the shim `google` account described here unchanged.)
- **(f) The owner-supplied base-URL trust model.** The root is supplied by the
  owner for `openai_compatible` (Custom), where it is required, and may be supplied
  for any of the four named presets as an override of their pinned default root
  (and, since [ADR-054](#adr-054--native-google-gemini-dialect-its-own-vendor-id-and-provider-kind-selected-by-the-preset-dialect-field-over-a-leaf-translation-package),
  for the native Gemini `google_native` preset);
  the trust model applies to every account whose owner supplies one. It must be
  **https** with a host and carry no userinfo, query, fragment or space (the key
  rides on every request, so plaintext and credential-in-URL are refused); the
  host is otherwise unrestricted. This is an on-prem gateway and the URL is set by
  an authenticated owner for their own account, so such an account can make the
  gateway call any https host the owner names, internal ones included:
  server-side request forgery is **accepted**, with its limits recorded in
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
[Data Model](reference/data-model.md#4-migration-history-95-migrations) (migration 86),
[Risks & Technical Debt §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-053 — Usage/credits for api-key vendor accounts: a per-preset usage strategy over the existing OAuth-only pull path
**Context:** [ADR-052](#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)
left usage and credits for the OpenAI-compatible vendors to a later sub-project and
reserved an empty usage slot in the preset registry. The machinery they need already
existed for an OpenAI subscription: the per-account snapshot, its DTO, the dedicated
refresh endpoint with its lazy TTL and the availability-aware panel. Its pull was
OAuth-only, though: the gate was "an OpenAI subscription", the refresh opened a
token set, and every write merged into the row the passive header scrape also
writes. What can be asked with the **stored inference key** differs per provider
(checked on 2026-10-11): OpenRouter answers a per-key limit and usage at
`GET /v1/key`; Kilo has an undocumented balance endpoint that its own client calls
with an OAuth token; x.ai keeps balance and usage behind a separate Management API
with its own key and a team id; Gemini has no usage or balance API at all; and a
Custom endpoint has no standard one.

**Decision — seven choices, taken together.**
- **(a) The OAuth-only pull is extended to api-key accounts, gated by the preset's
  `UsageVia`.** An api-key account has an active pull when its preset names a usage
  strategy (`openrouter_key`, `kilo_balance`); the registry is the only thing that
  says so, not a vendor-id list in the portal. The native OpenAI and Anthropic
  api-key accounts are not in the registry and keep no pull, and x.ai, Google and
  Custom keep their `UsageVia` empty and with it the panel's availability-aware
  empty state. The fetch is split per credential kind and the store tail is shared;
  the dedicated refresh branches on an api-key account **before** its OAuth-token
  step (which would answer "not connected" for an account that has no token set).
  The same pull also runs best-effort, never failing the call, as the last step of a
  models refresh and so of an OpenAI-compatible create with its key, inside the
  existing bounds. No new endpoint, DTO field or frontend type: the refresh endpoint
  and its answer are reused as they are.
- **(b) `UsagePath` is root-relative.** The preset gains one field, the root-relative
  URL suffix of the usage request, composed as `TrimRight(base_url, "/") + UsagePath`
  by the portal, so `vendorauth` receives a finished URL and stays free of any vendor
  root (it stays capture-only). It is not relative to the path prefix because Kilo's
  balance (`/profile/balance`) lies **outside** its `/gateway` prefix, while
  OpenRouter's `/v1/key` is expressible from the root as well and is the very URL its
  key probe asks (ADR-052 (d)): one rule, no per-provider special case.
- **(c) The existing snapshot, DTO and panel are reused; there is no migration.**
  OpenRouter's key limit maps onto the `spend_*` fields and `credit_status`, Kilo's
  balance onto `credit_balance`, so the panel's existing credit row renders them.
  OpenRouter's richer fields (daily, weekly and monthly usage, the free-tier counters,
  BYOK) have no home in the snapshot and are **deferred**: showing them would need new
  columns, a migration and a Postgres leg for what is v1 advisory data.
- **(d) The api-key pull stores what it fetched, with no merge.** The merge rule of
  the subscription path exists because two writers each know part of the row and a
  partial reading must not blank what the other learned. An OpenRouter or Kilo account
  has one writer: the scrape looks only for the Codex `x-codex-*` headers, which these
  providers never send, so it writes nothing for them. The merge's "an unknown
  incoming field is no news" would then only do harm, because it cannot express "now
  absent": an owner who removes a key's cap would keep seeing the old limit and spend.
  A failed pull still writes nothing, so the last good snapshot stays.
- **(e) OpenRouter reports the key, not the account, and an unlimited key reads
  "Unlimited".** `/v1/key` answers the limit and usage of the one key the gateway
  holds, in USD; the account's credit balance needs a management key the gateway never
  asks for, so `credit_balance` stays empty. A capped key fills the spend fields
  (`spend_used` is limit minus remaining, consistent with the bar); a key with an
  explicit `null` limit maps to `credit_status = unlimited` with every spend field
  left empty, because the body's all-time `usage` total shown beside "Unlimited" would
  read as a period figure.
- **(f) Kilo is best effort.** The balance endpoint is undocumented and read off a
  client that authenticates it with an OAuth token; whether the api-key type the
  gateway stores is accepted at all is unconfirmed. Any 401, 403 or 404, a redirect or a
  body without a plausible `balance` is `unverifiable` and leaves the panel's
  empty-state line; nothing is raised. The unit is not asserted either: `spend_unit`
  stays empty.
- **(g) An unreadable key is a 409, a missing key is `unverifiable`.** An api-key
  account whose sealed key cannot be opened answers the documented
  `409 vendor_account.credential_unreadable`, the same state-of-the-account error a
  subscription's token set gives, because it is fixed by re-entering the key and says
  something the caller can act on. An account with no key set has nothing to ask with
  and answers `unverifiable` ("no API key is set"), the fail-soft status of every
  other pull that could not be made.

**Consequence:** a provider that offers a usage query with an inference key is one
registry entry (`UsageVia`, `UsagePath`), one `vendorauth` fetcher and one case in the
portal's strategy dispatch, with no schema work. There is no migration, so no
PostgreSQL leg is needed for this change. The two usage endpoints are the least certain
data in the registry: OpenRouter's shape is derived from its documentation
(REVERSE-ENGINEERED, VERIFY-LIVE, read by a tolerant parser) and Kilo's endpoint is
undocumented and may not accept the stored key, so Kilo's usage may simply never appear
([Risks §11.1](11-risks-and-technical-debt.md#111-operational-risks)). The pull that
trails a models refresh does not consult the lazy TTL (the subscription's never did), so
it asks the provider on every models refresh; only the dedicated on-view call is
rate-limited. The frontend names the two api-key vendors itself, mirroring `UsageVia`;
the backend registry is the authority, so a drift between them shows or hides a Refresh
button and nothing else. The shared spend display rounds the used amount to one
decimal, which suits credits and loses the cents of a USD figure: a follow-up, not
part of this decision.

**Rejected:** new DTO columns for the richer OpenRouter data (daily, weekly and
monthly usage, free-tier counters) in v1: a migration and a Postgres leg for figures
no one asked for, which can be added when they are; an OpenRouter-only hardcode in the
portal (the next provider with a usage query would be a second branch, and the registry
already is the place that says what a provider supports); the x.ai Management API
(balance and usage there need a second credential and a team id, which an account
that stores one inference key cannot supply, and asking for it widens what an account
holds); merging the api-key snapshot like the subscription's (it cannot express a
removed cap and there is no second writer to protect).
→ [External Vendor Accounts §5.2](cross-cutting/external-vendor-accounts.md#52-active-pull-openai-subscription-openrouter-and-kilo-api-keys),
[§4.4](cross-cutting/external-vendor-accounts.md#44-openai-compatible-vendors),
[API Surface](reference/api-surface.md#vendor-accounts-anbieter),
[Risks & Technical Debt §11.1](11-risks-and-technical-debt.md#111-operational-risks).

## ADR-054 — Native Google Gemini dialect: its own vendor id and provider kind, selected by the preset Dialect field, over a leaf translation package
**Context:** [ADR-052](#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)
(e) served Gemini through Google's OpenAI-compatibility shim and deferred the native
dialect; this decision **supersedes that deferral** (the shim account itself stays).
The shim is a lossy surface: it carries what OpenAI's chat completions
carry, and Gemini's own `generateContent` wire is what a Gemini-shaped client and
Gemini-only behaviour need. Three things stood in the way of a native client. The
neutral `inference.Request` has no slot for what Gemini function calling requires
on a later turn: the function **name** (a `functionResponse` is keyed by name, not
by call id) and a part's opaque **`thoughtSignature`** (Gemini 3 answers a
function-calling turn with a 400 when it is not echoed back). The credential rides
a different header (`x-goog-api-key`, which the capture redaction did not know), and
the model sits in the URL path rather than the body. And the same wire will also be
needed *inbound* (a Gemini-compatible endpoint, #204), so its translation should not
be written twice or buried in one client.

**Decision — eight choices, taken together.**
- **(a) A distinct vendor id selects the native dialect, via a `Dialect` field on
  the preset.** A new vendor id `google_native` (`routing.VendorGoogleNative`) is a
  separate account kind next to the shim `google`, not a toggle on it and not a
  global switch. `VendorPreset` gains `Dialect string` (`""` = OpenAI wire, `"gemini"`
  = native), and the one registry predicate is split in three:
  `IsPresetVendor` (any preset account: api-key only, a configurable root, fail-closed
  on an empty root, discovery on create, an empty static seed), `IsOpenAICompatibleVendor`
  (a preset whose `Dialect` is empty: the OpenAI-wire branch) and `IsNativeGeminiVendor`
  (`Dialect == "gemini"`). The preset-account behaviour follows `IsPresetVendor`; the
  wire follows the dialect. **No migration:** `vendor_accounts.vendor` is free text
  with no check, so an owner keeps a shim and a native account side by side, and
  rollback is deleting the native one.
- **(b) A fourth vendor provider kind, `vendor_gemini`.** `routing.ProviderVendorGemini`
  joins `vendor_openai`, `vendor_openai_subscription` and `vendor_anthropic`, and a
  native `provider.GeminiClient` (`Client`, `StreamingClient`, `NativeProxyClient`) is
  registered for it. The resolver tests the native dialect **before** the generic
  OpenAI-compatible case, which is load-bearing: the generic branch would otherwise set
  the OpenAI path prefix on a target whose client composes its own
  `/v1beta/models/{model}:generateContent` path (`OpenAIPathPrefixFor` also answers
  empty for a native preset, as a second guard). The target is built from the account's
  stored root with `APITokenHeader = x-goog-api-key`, `APIFlavors = [openai, anthropic]`
  and both endpoint modes zero, the same served-flavor shape as every other api-key
  vendor, so the listing overlay and dispatch agree.
- **(c) `x-goog-api-key` auth, no redirects, and a redacted capture.** The client
  sends the key only in the `x-goog-api-key` header, never the `?key=` query parameter
  (a credential in the URL reaches logs), with the model path-escaped in the URL and
  the streaming action (`:streamGenerateContent?alt=sse`) in the URL, not the body. It
  uses a no-redirect HTTP client on every path: `net/http` strips `Authorization`, but
  not a custom credential header, when it follows a redirect off the host, and the root
  is owner-supplied (ADR-052 (f)). A non-2xx answer surfaces a bounded body snippet, so
  a 400 `INVALID_ARGUMENT` explains itself. `x-goog-api-key` joins the gateway's
  capture-redaction allowlist, because the provider's capture sink records the
  **outbound** request headers and an unlisted credential header would be stored in
  clear when payload capture is on.
- **(d) `internal/geminiwire` is the shared, leaf translation home.** The Gemini wire
  structs and the neutral-to-Gemini and Gemini-to-neutral converters live in their own
  package, which imports **only** `internal/inference` (an architecture-test edge, with
  a forbidden-edge test that pins it, like `compat`). `provider` imports it, so it
  cannot return a `provider.Response` and returns a package-local `Result` that the
  client copies. The outbound halves are built now (`RenderRequest`, `ParseResponse`,
  `ParseStreamChunk`); the inbound halves (`ParseRequest`, `RenderResponse`,
  `RenderStreamChunk`) are reserved for #204, so both directions end up in one
  package and can be round-trip tested. `RenderRequest` is total (it returns no error:
  malformed tool arguments become `{}`, unsupported media is dropped), so the only
  encode error is the client's `json.Marshal`.
- **(e) The function name and `thoughtSignature` ride in the neutral tool-call id.**
  Every dialect echoes a tool-call id verbatim (`tool_call_id`, `call_id`,
  `tool_use_id`), so the parser folds the function name, the part's
  `thoughtSignature`, and a per-response call index into `ToolCall.ID` as `gcw1:` plus
  the base64url of a small JSON object (`n`/`s`/`x`), and the renderer decodes it on the
  next turn to restore the model turn's `thoughtSignature` and the `functionResponse`
  name. The codec is reversible and rejects a malformed or `null` payload. The `x` index
  keeps two parallel calls to the same function with the same (or no) signature distinct
  (a strict downstream client keys on `tool_call_id`); it is omitted when 0, ignored on
  decode, and never sent to Gemini. The index is per response, so the first unsigned call
  of each turn repeats across turns — accepted, since a per-response nonce would break
  the determinism the rest of the codec relies on. An id this package did not mint (history from
  another provider) falls back to the name of an **earlier** assistant call with that id,
  and carries no signature. A non-object tool result is wrapped as `{"result": text}`.
- **(f) Translation at the neutral model's expressiveness, with Gemini's own
  conventions where they differ.** `system` and `developer` messages become
  `systemInstruction`; consecutive same-role turns merge and empty ones drop; tools
  become `functionDeclarations[].parametersJsonSchema` and `tool_choice` a
  `functionCallingConfig` mode; `reasoning_effort` becomes a `thinkingConfig` (a
  token budget for the 2.5 family, a level for 3.x, the family sniffed from the model
  name); an image data URI becomes `inlineData`. On the way back `thought` parts are
  the reasoning, `functionCall` parts the tool calls, a finish reason folds to the
  OpenAI vocabulary (`STOP` is `tool_calls` when a call is present, safety-class
  reasons are `content_filter`), and a prompt-blocked reply (HTTP 200, no candidates)
  is an empty `content_filter` answer, never a crash on `candidates[0]`. Usage follows
  the OpenAI convention: input includes the cached tokens and output includes the
  reasoning tokens, so input and output stay consistent with the reported total. The
  stream is SSE with no `[DONE]`: it is complete only when a chunk reports a finish
  reason, the last cumulative `usageMetadata` is the answer's usage, and a mid-stream
  error frame or an end without a finish reason is `provider.unavailable` rather than a
  truncated success.
- **(g) Native discovery and validation live in the portal's existing seam.** Model
  discovery is `vendorauth.DiscoverGeminiModels`: the paged `GET {base}/v1beta/models`
  with `x-goog-api-key`, keeping only models whose `supportedGenerationMethods` contain
  `generateContent` and stripping the `models/` prefix; the key probe is
  `vendorauth.ValidateGeminiAPIKey` with the uniform classification. Both receive a
  portal-composed URL, so `vendorauth` still knows no vendor root. The preset carries no
  usage strategy: Gemini has no usage API.
- **(h) Translate-only for now; passthrough and broad fidelity are separate work.**
  Both endpoint modes stay zero, so an inbound Responses or Messages request is
  translated through the neutral model. Lossless passthrough, meaning the inbound
  Gemini endpoint where a Gemini-shaped client connects, is deferred to #204, which
  reuses `geminiwire` (and the client already implements `ProxyNative` for it). The
  features the neutral model cannot carry (structured output, safety settings,
  non-image media, fine thinking control) are not forced through translate: they are
  either delivered losslessly by that passthrough or by the translate-fidelity extension
  tracked as #208.

**Consequence:** a Gemini account's tool-call ids are long, opaque strings on the
OpenAI and Anthropic wires; a client must echo them verbatim, which every
conforming one does. A shim and a native account of Google serving the same model
names collide at resolve time (the first active account by id wins; a token with a
non-"all" access policy refuses the duplicate at save,
[External Vendor Accounts §11.3](cross-cutting/external-vendor-accounts.md#113-name-collisions)),
so the advice is a distinct model prefix on one of them. The native wire's
behaviour was designed from documentation without a live key, so several details are
VERIFY-LIVE data that degrade conservatively
([External Vendor Accounts §10](cross-cutting/external-vendor-accounts.md#10-tos-experimental-status-and-verify-live)):
the thinking field names and budgets, `parametersJsonSchema` per model, whether an
external image URL is accepted, what Gemini 3 does with a history that carries no
signature, and the status a wrong key gets. Only a function-call part's signature is
carried (the neutral model has no other slot). The translate path inherits the neutral
model's ceiling: a client that needs a Gemini-only feature waits for #204 or #208.

**Rejected:** a per-account `dialect` toggle on the existing `google` account (a column
and a migration, an in-place behaviour change for every existing shim account, and no
way to run both side by side); a global switch (the same, for the whole gateway);
widening the neutral model with a `thoughtSignature` and name slot (every provider and
inbound parser would change for one dialect's quirk, which is what #208 weighs on its
own); writing the Gemini wire inside `provider` (the inbound endpoint of #204 could not
reuse it without importing the whole provider package, and the two directions could not
be tested against each other); the `?key=` query-parameter credential (it puts the key
in URLs and logs); following redirects (the custom header would travel with them).
→ [External Vendor Accounts §4.5](cross-cutting/external-vendor-accounts.md#45-native-google-gemini),
[§4.4](cross-cutting/external-vendor-accounts.md#44-openai-compatible-vendors),
[API Compatibility & Inference §8](cross-cutting/compatibility-and-inference.md#8-provider-clients),
[Building Block View](05-building-block-view.md#52-level-2--gateway-backend-components),
[API Surface](reference/api-surface.md#vendor-accounts-anbieter).

## ADR-055 — Per-model sd-server image options are read live on pick from two volatile caches and never stored
**Context:** the portal image composer
([ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread))
first offered sd-server's native knobs from a static sd-cpp sampler/scheduler
superset, because the gateway forwards every pick opaquely and owns none of that
vocabulary. The real answer is per model: each `sd-server` states its own
samplers, schedulers, output formats, size and batch limits and defaults in
`GET /sdcpp/v1/capabilities`, the document the `image` verdict is already read
from ([ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)
(e)). Two things made the obvious designs wrong. An `sd-server` is served two ways
at once — an **external** `stable_diffusion_cpp` application the gateway can
reach, and an **agent-launched** child behind the agent's router, which the
gateway's health loop cannot reach — so a gateway-side probe alone covers half the
fleet. And the existing home for "what the document said",
`model_mapping_capabilities`
([ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)),
is the wrong shape for it.

**Decision — five choices, taken together.**
- **(a) One read-only endpoint, read live when the composer picks an image
  model.** `GET /api/portal/model-image-options?name=<model>` answers the options
  the model's backing `sd-server`(s) state; the composer fetches it on pick and
  clears it on every model change. Image options are near-static per model, so
  there is no SSE variant. The run path is untouched: this decision replaces the
  dropdown *vocabulary* and adds default hints, nothing about how a run is sent.
- **(b) Cache, don't store — two volatile caches, one per deployment shape, written
  by background probes.** The health loop reads an external application's document
  into `AppImageOptionsRegistry` (keyed by application id); the agent reads an
  agent-launched child's document itself and reports it on its telemetry sample,
  which the gateway holds as `RuntimeStatusDTO.ImageOptions` in the volatile
  runtime-status registry (keyed by server and spec id, never put on the status
  SSE frame). The portal GET only reads them, the existing "health loop writes,
  portal reads" split, so it never probes upstream and an unreachable server can
  neither slow nor fail it. **Nothing is persisted**, and no migration is added.
- **(c) A lenient decode, kept SEPARATE from the strict verdict decode.** The
  options decode is its own struct and function (twinned in the gateway's
  `provider` and the agent's `collector`, the same drift discipline as
  `parseSdcppCapabilities`) and reads whatever it can, leaving the rest at zero =
  unknown. The verdict decode stays strict and fail-closed exactly as
  [ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)
  and ADR-044 need it, so **a malformed options document can yield partial or empty
  options but can never suppress or alter the routing `image` verdict.**
- **(d) Visibility in the portal layer, aggregation as "what every backing server
  can do".** `portal.Service.ModelImageOptionViews` applies `model-servers`'
  suppression and resource-group filtering and keeps only sd-server mappings, so a
  model the token cannot route to reads no cache and answers an empty object.
  `aggregateImageOptions` then **intersects** the three lists, takes the
  **tightest envelope** of the size limits (and the smallest `max_batch_count`),
  and keeps a default only where every server that states it agrees. A request can
  land on any server behind the model, so only what all of them accept may be
  offered; a mixed external plus agent-launched set unifies into one answer. Any
  emitted size bound satisfies `min <= max` with no negatives (an inverted axis is
  dropped whole); `clip_skip` and `seed` may legitimately be `-1`.
- **(e) Fail-open, and defaults are hints.** An empty or absent field means
  **unknown, not "none supported"**: the composer falls back to the static
  superset for that field, including when disjoint backing-server sets intersect
  to nothing. Reported defaults are shown as placeholder/helper text and **never
  pre-filled**, because a pre-filled knob would be sent as an explicit override and
  break "blank = leave sd-server's own default". The portal's count `n` keeps its
  hard cap of 8 (a portal-side per-request limit, not a model limit), which a model's
  `max_batch_count` may only narrow.

**Why ADR-039's table is NOT extended.** `model_mapping_capabilities` holds
exactly one kind of fact: a `yes`/`no` verdict with a rank, a source and a
`checked_at`, where absence means unknown and an operator's `manual` row outranks
every probe. Image options are an open vocabulary of names, integers, ranges and
defaults with no yes/no reading, no rank to arbitrate and no operator override;
`ValidateCapabilityRow` would refuse them, and `routing.CapabilityRow` has four
fields and no room for them. Making them rows would also mean a schema, a
migration and conformance on both drivers for a best-effort UX hint whose worst
failure is the dropdown showing the static superset it showed before this
feature. The verdict table is also the input to a fail-closed gate; mixing
lenient data into it would put the routing path one parsing bug away from an
option list.

**Consequence:** a gateway restart empties both caches. An agent-launched child's
options return on the next telemetry sample (about a second); an external
application's return on its next health tick, up to that application's longest
probe interval, and until then the composer shows the static superset. That
warm-up gap is accepted and documented rather than closed with persistence. An
agent whose gateway does not declare `capability_source_sdcpp` reports no options,
which reads as unknown. Because options are fail-open, the composer may offer a
value one backing build ignores, which is the pre-existing behaviour of the static
list. Two copies of the lenient decode must be kept identical by hand, pinned by a
case table duplicated in both modules' tests.

**Rejected:** extending `model_mapping_capabilities` or adding an options table
(above); probing the `sd-server` synchronously from the portal GET (it puts an
upstream round trip and its failure modes on a user's click, and cannot reach an
agent-launched child at all, which only the agent can read); reading only the
external path and leaving agent-launched children on the static superset (half the
fleet, and the same model would behave differently by deployment shape); a union
of the backing servers' lists (it would offer values some servers refuse, so the
same request would succeed or be ignored depending on routing); fail-closed
semantics for an empty field (an unread model would get an empty dropdown); and
pre-filling the reported defaults (turns every untouched knob into a sent
override).

**Amended by [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)
(the first persistent per-mapping cache).** The decision above stands for image options:
they stay in the two volatile caches, nothing about them is stored, and no migration was
added for them. The title's "never stored" is a statement about image options, not a
gateway-wide rule. Image options can be re-read from the capabilities document at any
time, which is what makes losing them on a restart cheap. A TTS voice list cannot: audio.cpp
states it only for the model it has **loaded**, so a picker that must work while the model is
unloaded needs the names kept, and the gateway now has a persistent per-mapping cache
(`model_mapping_cache`, migration 94) for exactly that kind of fact. The reasoning that kept
the options out of `model_mapping_capabilities` applies unchanged to the voices.
→ [API Compatibility & Inference §3.4](cross-cutting/compatibility-and-inference.md#34-openai-images-generations),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[API Surface](reference/api-surface.md#models-servers-applications-mappings),
[Agent-Managed Model Runtime](cross-cutting/agent-runtime-manager.md).

## ADR-056 — Chat binary payloads live in an owner-scoped, sealed `chat_blobs` store and the transcript references them by gateway URL
**Context:** [ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread)
(c) stored a generated image **inline** in the chat's one sealed transcript and
accepted that the whole-document ceiling (`portal.MaxChatContentBytes`, 4 MiB)
is then a budget every image in the thread shares for its life, so a busy thread
held roughly eight to twelve images and then could not take another turn. It
named a store of the payloads' own as "the right end state" and filed it as
issue #124. An *uploaded* vision image shared the same budget. The cap exists to
bound the **text** document and stays; what exhausted it was the binary
payloads, and taking them out of the document is the whole point. That also
removes the only limit on stored and in-flight image bytes, so the decision
includes the bounds that replace it.

**Decision — seven choices, taken together.**
- **(a) A generic, owner-scoped binary store, `chat_blobs`, sealed with the
  same cipher as the transcript.** One row per payload: `id` (`blob_` + 32 hex),
  `chat_id` → `chats(id)` `on delete cascade`, `key_version`, `blob` (the
  **decoded bytes** sealed with the capture cipher, `KeyVersion` 1, or plain
  with `KeyVersion` 0 on the volatile RAM path), `content_type`, `byte_len`
  (`bigint`, [ADR-005](#adr-005--postgresql-needs-wide-column-types)) and
  `created_at` (migration 87, additive, the frozen baseline untouched). **The
  owner is derived through `chat_id → chats.user_id`**; there is deliberately no
  denormalised `user_id`, which could disagree with the chat's owner and would
  duplicate a cascade that already reaches the blobs transitively through
  `chats`. **Delete is the cascade and nothing else**: retention is the chat's
  lifetime, with no prune and no dedup (every payload gets a random id).
  **No gzip**: images are already compressed, and a second pass would cost CPU
  for nothing. Sealing with the existing cipher adds **no new decryptable
  secret**, so the at-rest guarantee
  ([§13](cross-cutting/security-auth-rbac.md#13-secrets-at-rest)) is exactly what
  it was for an inline image. The store is **payload-type-agnostic** (bytes plus a
  content type), and **images are the only consumer in v1**: the type
  restriction lives in one allow-list in the service layer. The store and the
  serving path are ready for another payload type (audio, PDF, a file), which
  would need a deliberate allow-list entry, a part shape the extract and
  resolve walk understands, and a renderer — but no new storage layer; none of
  that exists yet. (Amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
  (c): audio is now the second consumer. `audio/wav` is on the allow-list, an
  `audio_url` part is extracted, and `AudioTurn` renders it, with no new storage layer
  and no schema change; the resolve walk stayed image-only — until [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn) generalised it to the audio family so a transcription run resolves its own input.) The memory driver keeps a parallel map that is dropped
  with its chat on both `DeleteChat` and the FIFO `evict()`, and whose bytes
  **count against the 64 MiB memory budget** — otherwise the RAM path would be
  unbounded, which is the very problem this decision exists to fix on the other
  drivers.
- **(b) The transcript references a payload by a gateway URL in the part that
  already carried it — `image_url` is reused, not replaced.** An image part's
  `url` is `/api/portal/chat-blobs/{id}` instead of a `data:` URL. The renderer
  (`<img src={url}>`) and the wire format are unchanged, history construction
  still treats it as an `image_url` part. The store and the serving endpoint are
  payload-type-agnostic, but only `image_url` parts were extracted and resolved
  at first (the walk knew no other part shape). (Amended by
  [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
  (c): the extraction at the run commit also walks `audio_url` parts through the shared
  `rewriteBlobParts` — `rewriteImageParts` was folded into it — each kind taking only its
  own media family. The resolution of a run's history stayed image-only until
  [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn)
  generalised the resolve walks to a `blobPartKind`, so a transcription run resolves its own
  audio input.) **Old inline `data:`
  parts keep rendering and keep running** (additive, no forced migration): a
  legacy chat converts lazily, on its next run, and one that is never run again
  stays inline and works.
- **(c) Payloads are extracted at the two run-commit paths, not on create and
  not on a bare autosave.** `extractChatBlobs` runs in `PrepareChatRun` (an
  *uploaded* image, an `edited_history` image, a legacy inline one) and in
  `writeAssistant` (a *generated* image), after the chat row exists and
  **before** the 4 MiB cap check — so a legacy 3.9 MiB thread, whose images
  leave the document, can take its next turn. It moves each inline `data:` image
  into a `ChatBlob`, rewrites the part's URL, and persists the rewritten
  document **and** its new blobs in one store transaction
  (`SaveChatWithBlobs`), so a refused or failed turn leaves no orphan. It
  deliberately does **not** run on `CreateChat` (the chat row a blob's foreign
  key points at does not exist yet, and a created chat is bounded by the old cap
  anyway) or on the bare autosave `PUT` (the composer only persists a new large
  payload by sending it through a run; a title or text edit carries
  already-stored URLs), which also avoids re-extracting and orphaning on every
  save. It is **idempotent** (a URL that is already a gateway URL is left
  alone). A gateway URL in *request-supplied* content (`edited_history`, the new
  turn) must reference a blob **this chat holds**, else `images.blob_not_owned`
  — a client cannot graft another chat's or user's id into its own transcript;
  that check starts at `verifyFrom`, the first message the request supplied, so
  the images the chat itself stored are not re-verified on every turn and every
  periodic checkpoint. **Base64 is standard-padded only**: a `data:` URL in
  another alphabet, unpadded or with a parameter such as `charset` is refused
  rather than guessed at, so what is stored is what was uploaded, byte for
  byte, and what is resolved later is re-encoded identically. **Adoption needs
  no new client code**: the existing post-run canonical refetch
  (`adoptCanonicalTranscript`) delivers the rewritten URLs, and `activateChat`
  covers create; the run's SSE `done` still carries the pre-rewrite `data:`
  part, which the refetch then swaps.
- **(d) The gateway resolves references back to bytes in one place,
  `PrepareChatRun`, chat-scoped and remote-safe.** An upstream cannot reach a
  gateway URL, and the Anthropic adapter's image-source conversion returns nil
  for a URL that is neither `data:` nor `http(s)`, so a reference left in the
  request would make the image **silently vanish** and the model answer
  without seeing it. So for a **non-image** run `PrepareChatRun` replaces every
  `/chat-blobs/` URL in the history it returns with a `data:` URL of the real
  bytes, through **one** batched `ChatBlobsByIDs(chatID, ids)` — scoped to the
  chat in the query itself, so a grafted foreign id is simply absent and can
  never resolve to someone else's bytes. A reference with no stored bytes
  (a foreign or never-stored id) becomes a
  `[image unavailable]` text part: the model is told, the run goes on, and the
  stored document is untouched. The image kind skips resolution entirely (its
  executor reads only the last prompt's text). The resolution sits in
  `PrepareChatRun` because it has the context, the owner and the store, while
  `buildAPIHistory` is pure and `buildChatCompletionsBody` has no store handle.
  It runs before the commit, so a refused run stores nothing and a retry is not
  a duplicate turn.
- **(e) The serving endpoint is hardened, because the content type is
  attacker-influenced on a same-origin, cookie-authenticated, navigable URL.**
  `GET /api/portal/chat-blobs/{id}` takes the standard portal auth (the
  browser's `<img>` sends the session cookie), and authorises on the **blob**, never on a transcript that mentions it (a
  transcript can carry any id — create and a bare autosave do not validate the
  URLs they store): the blob's chat must belong to the authenticated user, decided
  with two metadata-only lookups before any bytes are read or decrypted. Every
  way of not being the owner of a servable blob — missing, someone else's,
  chat deleted, evicted, an unlisted type — is the same `404
  portal.chat_blob_not_found`, with no existence leak; a store or cipher failure
  is `500 portal.chat_blob_failed`, never a 404 (which would turn a broken key
  into an empty image). The stored type is checked against an **allow-list**
  (`image/png`, `image/jpeg`, `image/webp`, `image/gif`) at store time
  (`images.blob_type_unsupported`, so `text/html` and `image/svg+xml` never get
  in) **and again at read time**, and nothing else is ever echoed as
  `Content-Type`. (Amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
  (c): the list also holds `audio/wav`, which only an `audio_url` part may carry, so the
  type the endpoint returns is one of five strings — nine since [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn) added the four transcription input types.) The body is written whole with a
  `Content-Length`; the handler does not go through `http.ServeContent` and answers **no
  `Range` request**, which an `<img>` does not need and an audio player on Safari or
  iOS likely does, so the portal's player plays an object URL it fetched instead of this URL
  (ADR-063 (d)). The response carries `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'`, `Content-Disposition: inline`
  and `Cache-Control: private, no-store` — the id is immutable, but the bytes are
  decrypted and private, and a long-lived `immutable` entry would park them in
  the browser's disk cache. The cookie leg of the auth is the **passive** session
  check (`ResolveSessionDetailNoTouch`, so an `<img>` load does not extend the
  idle window): a thread of N images opens N of these in parallel, and a
  `TouchSession` write per request would stampede the connection pool; it is
  sound because the chat load that issued them already refreshed the session, and
  a passive fetch must not be able to keep a session alive on its own. The
  route is not an inference endpoint, and the ownership check applies whatever
  the credential.
- **(f) Replacement bounds, both configurable, both refusing rather than
  trimming.** `MaxChatBlobBytes` (`OP_AI_GATEWAY_MAX_CHAT_BLOB_BYTES`, default
  20 MiB, the composer's own upload limit) caps one decoded payload, enforced at
  extract (`images.blob_too_large`). `MaxRunHistoryBytes`
  (`OP_AI_GATEWAY_MAX_RUN_HISTORY_BYTES`, default 32 MiB decoded) caps the image
  bytes one run's upstream history carries, counted per occurrence, so a long
  vision thread cannot assemble a request of hundreds of megabytes; a run over
  it is refused `400 portal.chat_run_history_too_large` before its turn is
  stored (the declared sizes are checked first so a refusal decrypts nothing,
  then the bytes actually unsealed). Neither of the two **text** limits moves:
  the 4 MiB document cap and the 5 MiB request cap (one upload still rides
  inline in the run body) stay as they were. Per-chat growth is bounded only by
  the chat's lifetime; a soft per-chat ceiling is deferred
  ([§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
- **(g) The frontend stops estimating.** The image capacity estimate is removed
  (a ~60-character URL would make "images left" a nonsense figure, and the cap no
  longer binds images), so the composer states no remaining room and `send()` no
  longer refuses on it; the text overflow stays the server's `400
  portal.chat_too_large`. `max_content_bytes` is still served on the chat
  listing and is now informational. Saving a stored image fetches the serving
  endpoint (a `data:` URL, from an old chat, is still decoded in place), and the
  four new run-start codes have labels.

**Consequence:** a thread is no longer limited by image count, the sealed
transcript is small (so the keepalive and the autosave stay cheap), and "no
4 MiB limit" means "not that shared one", not "unbounded": one payload is
bounded, one run's resolved history is bounded, one request body is bounded. Two
costs are accepted on purpose. **Blobs are retained for the chat's lifetime and
an edit or regenerate that drops a message leaves its image behind** — no prune,
no reference counting — the price of cascade-only deletion. **A RAM-store chat
that is evicted takes its images with it** (the chat and its blobs are dropped
together), so an already-open tab's image URLs then answer 404 and a run on that
chat is refused as chat-not-found; `[image unavailable]` arises only for a
foreign or missing id in a chat that still exists. The failure of a
*generated* image's extraction at the assistant commit (it is too large or of an
unlisted type) ends the run as `gateway.chat_run_commit_failed`
([ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread)
(c)); the three `images.blob_*` codes and the history bound are answered by the
run-start `POST`. An id in a transcript is a claim, never a grant: authorisation
is at read (serve) and at resolve (chat-scoped), so neither create nor autosave
validates the URLs it stores.

**Rejected:** an image-specific table or endpoint (a `content_type` column and
an allow-list make the generic store free, and audio or a document would
otherwise need a second layer); a denormalised `user_id` (above); extraction on
`CreateChat` or on the autosave `PUT` (the FK ordering, and a re-extract plus
orphan on every save); resolution in `buildAPIHistory` or
`buildChatCompletionsBody` (pure, and no store handle); leaving gateway URLs in
the upstream request (a remote provider cannot fetch them and the Anthropic
adapter drops them without an error); trimming the oldest images to fit
`MaxRunHistoryBytes` (the model would answer a different conversation than the
user sees, silently — a refusal is a clear, retryable answer); a new content
part type instead of reusing `image_url` (a second wire format and a renderer
fork for nothing; for an *image*, since [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(c) adds `audio_url` for a WAV, which `image_url` would mislabel as a picture); a long `immutable` cache lifetime (above); gzip of the
payload (above); content-hash dedup, a dedicated upload endpoint, a forced
one-time migration of old chats and a soft per-chat ceiling (all deferred, none
needed for correctness).
→ [Data Model](reference/data-model.md#portal-chat) (migration 87),
[HTTP API Surface](reference/api-surface.md#tokens-chats-usage),
[Security, Authentication & Authorization
§13](cross-cutting/security-auth-rbac.md#13-secrets-at-rest),
[API Compatibility & Inference
§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-057 — Image edit: a derived `image_edit` capability, a multipart passthrough, and the attached image as the edit affordance
**Context:** [ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)
and [ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread)
shipped image *generation*; editing an image is the other half of issue #69, and
it is a different request, not a flag on the first. It is the gateway's **first
multipart request path** (every other inference endpoint reads one JSON value),
it needs a capability that **nothing can probe** (sd-server serves
`/v1/images/edits` whenever its loaded model can generate, and publishes no
per-model edit signal, so "which models may edit" is gateway policy, not a probe
result), and two upstream behaviors do not match the OpenAI contract the public
endpoint promises ((e) and (f)). Each decision here has a cheaper obvious answer
that is wrong in a way no happy-path test would catch.

**(a) `image_edit` is DERIVED from `image` and operator-removable — one
predicate, read by every consumer.** `routing.ImpliedImageEdit`
(`internal/routing/resolver.go`) is `image: yes` **and not** `image_edit: no`.
There is no probe and no producer: an absent `image_edit` row does not mean
*unknown*, it means *derive from `image`*, and `image_edit: yes` is inert (it
cannot grant what `image` does not). This is the **one documented exception to
[ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses)
(a)'s "unknown refuses"**, and it is bounded: every *other* required capability
is still a literal `yes` row, an absent one refuses, and an empty row set (a
failed capability read) derives nothing (`ImpliedImageEdit(nil)` is false), so
the fail-closed direction survives. The rule is single-sourced, because the
repository's own precedent
([ADR-045](#adr-045--a-model-listing-advertises-what-dispatch-serves-one-flavor-rule-read-from-the-spec))
is that one function is what everyone reads, and three readers consume it:
(1) the **gate**, `capabilityRowsSatisfy`, through which every satisfaction
decision funnels (`filterCapable` at its three sites and the affinity read), so
one special case covers routing; (2) **`capableNames`**
(`portal/service_model_offering.go`), which used to return the empty set for any
required capability but `image` and therefore made the unknown-model redirect
dead on arrival for edits (it now folds `image` and `image_edit` and
intersects); (3) the **`ModelDTO` fold**, which gains `ModelDTO.ImageEdit`
(`image_edit` on the wire, never `omitempty`), ANDed across a model's mappings
and carrying the same `openai_images` flavor conjunct `image` already has. The
invariant is the one ADR-043 states for the composer — **the composer never
offers what the gate will refuse** — not that the two agree exactly: the gate is
per-candidate any-of and the fold ANDs, so a model on server A (opted out) and
server B (not) is *served* but *advertised false*, the safe direction and the
same asymmetry `image` has.

**The operator's remedy is a per-mapping opt-out, and it needed new UI.** The
mapping form's image control deliberately has no `no` option (`(image, no)` is
reserved, ADR-042 (b)), so a `no` on `image` was never available to say "this
model generates but must not be edited". A separate `image_edit` control carries
`unknown` (= derived) and `no` (= the opt-out), never `yes`; the model-servers
column renders the opt-out as one neutral chip and deliberately draws no
"derived-on" chip, which would be a second copy of the derivation in the client.
**Accepted: the derivation is type-blind.** It keys on the `openai_images` flavor
plus an `image: yes` verdict, so a non-sd upstream that a mapping declares
`openai_images` with a manual `image: yes` (a LiteLLM or custom backend with no
`/v1/images/edits`) is edit-capable too, and its edit fails upstream. That is the
price of "auto-on, operator removes", and the `image_edit: no` row is the remedy.

**(b) The two reserved lists are not symmetric, and the first draft inverted
both.** `reservedAgentCapabilityNames` (`internal/gateway/agent_ingest.go`) is the
list of *names an agent may not report*, and it **GAINS `image_edit`**: no
legitimate agent writer exists (nothing an agent can observe produces it), and
without the entry a buggy or hostile agent could report `image_edit: no` at rank
1 and silently disable editing, re-asserting it over the operator's reset on every
sample. This is the **reverse of ADR-042 (f)**, which keeps `image` *off* that
list precisely because a legitimate writer exists for it.
`reservedManualVerdicts` (`portal/service_applications.go`) is a deny list of
`(capability, verdict)` pairs an *operator* may not state, and it is
**UNCHANGED**: the capability vocabulary is open, so `(image_edit, no)` is
already writable, and adding it would forbid the very opt-out (a) needs.
`(image, no)` stays reserved, which is exactly why the opt-out is a row of its
own. Only the list's doc comment grew, and a test pins the non-change
(`(image_edit, no)` writable and resettable end to end).

**(c) R1 — an attached image makes the turn an edit, and the attachment is the
operand, not a mode.** This is the amendment to ADR-043 (a) ("the ONLY
affordance"), and its rejected toggle is *still* rejected, for the same reason: a
second axis of intent makes reachable a state the gate must refuse at the bottom
of the stack, after the user committed a prompt. What changed is that an uploaded
image is not a mode — it is what the user is editing — and what the composer
offers is still decided by the picked model through one model-derived flag
(`image_edit`), so the composer can still never offer what the gate refuses. One
predicate, `attachModeOf` (`chatDoc.ts`, a function of the thread's **kind** and
the picked model's flags), drives the attach button, the clear-on-switch effect,
`send()`, the file picker and the replay guard; on an image thread it reads
`image_edit` and **never `vision`** (an sd model is `vision: false` and edits), on
a text thread it is unchanged. The thread's kind stays `image` across both turn
types and **there is no edit kind**: for the image kind the URL is a function of
the kind *and* the turn — a last user message that carries an image posts to
`/v1/images/edits`, any other to `/v1/images/generations` — and the decision is
the executor's, read off the prepared history (`imageEditInputsOf`), never a
client setting. Only the **last** user message is read: an earlier user turn's
images were an earlier turn's, and an assistant turn's images are its results;
none is sent anywhere.

The run-start step that makes this safe is `resolveImageRunSource`: for an image
run it resolves **only the last user message** to bytes through the same
chat-scoped, size-bounded resolution a text run uses, and a reference the chat
does not hold is refused `400 images.blob_not_owned` before the turn is stored —
*deliberately not* the text run's `[image unavailable]` placeholder tolerance,
because an edit that silently lost its source would answer as an unrelated
generation. The executor then fails the run with
`gateway.chat_run_image_source_unreadable` and **sends nothing** for anything
that is still not bytes (an external link — a run never fetches one — an
unresolved reference, a bad base64, a non-image type): the prompt alone, or the
edit without the unreadable image, would answer a request the user did not make.

**(d) The public endpoint is a multipart PASSTHROUGH to sd-server's own
`/v1/images/edits`, not a JSON translate.** `POST /v1/images/edits` (and its
`/openai/` alias) is the images endpoint's sibling in everything but the body: the
same `requireInternalOrBearerAnyScope` (a bearer, or the loopback pair the
portal's executor uses — **never** a session cookie, the boundary ADR-043
protected), the same run-as block, one `inferencePreflight` (so still exactly one
`admitPrincipal` call site) with `RequiredCapabilities = [image_edit]` set from
the handler's identity, the same resolve and served-flavor conjunct (extracted to
the shared `resolveImagesTarget`, not copied), a new `sessionEndpoint` value
(`endpointImagesEdit`) and an endpoint-aware `upstreamPath` and `recordUsage` (so
an edit's usage row records `/v1/images/edits`, not the generations path). What is
new: the handler reads the body whole, bounded (j), and walks it **once** with
`mime/multipart` and the request's own boundary — never `ParseMultipartForm`,
which spills to temp files and consumes the body this handler must still relay —
peeking `model`, `prompt`, `response_format` and `stream` (each at most once: two
`model` fields would be two answers to "which model", and the agent's router
reads it again), counting the non-empty `image` / `image[]` parts, and reading
**every** part to its end (`mime/multipart` swallows a truncated part it merely
steps over, which would otherwise fail later, after admission). A missing model,
prompt or image is 400 (`images.model_required`, `images.prompt_required`, and the
new `images.image_required`) **before** the preflight, so none spends an
admission. `provider.ProxyNative` gained a `contentType` parameter (all six
implementers and every JSON caller, which pass `application/json` unchanged), and
the mock provider's native proxy answers an images path with a one-image
generations body so an edit round-trips end to end without an upstream. The relay
sends the client's bytes **verbatim** with the client's own Content-Type
**unless** something in the form must change — then it is rebuilt **once**,
byte-faithfully part by part (headers, order, binary content), with a fresh
boundary: the `model` field must carry the mapping's own name (`ProviderModel`)
for **every** target — an agent's router keys the child on it, and an external
upstream is handed it exactly as the JSON generations relay does — and the mask is
replaced by its converted form (f). The comparison is made against the *form's*
model, not against the mapping's two names, because a token override or redirect
can land on a mapping whose names are equal while the form still carries the
client's. Billing counts **output** images only (the unit and quantity logic is
the generations endpoint's, the response being the same document); the source and
the mask are unmetered.

**Rejected: a JSON translate to `/sdapi/v1/img2img` or the async
`/sdcpp/v1/img_gen` surface.** It would make the gateway an authority on a second
sd-server API and on its capability document, to avoid forwarding a body
sd-server already reads. **Rejected: `r.ParseMultipartForm`** (above).
**Rejected: a verbatim-only relay**, which is correct for an external sd-server
(it ignores `model`) and wrong for the agent's router, whose multipart model peek
keys on the mapped name.

**(e) `strength` rides the `<sd_cpp_extra_args>` block, as a pointer.**
sd-server's edit route reads **no `strength` form field**: it takes the value from
the extra-args block Phase 1b already plumbs and otherwise uses **0.75**, a weak
edit for reference-edit models. The image settings gain one more advanced knob
that renders into that same block (`ChatImageSettings.Strength`,
`sdExtraArgs.Strength`, both `*float64` so an explicit 0 is a value and blank is
omitted, leaving sd-server's own default), with a tooltip saying so. It is not
gated on edit: a thread that sets it sends it on a generation too, where
sd-server ignores it without a source image.

**(f) The mask is converted, because OpenAI's polarity is the opposite of sd's.**
An OpenAI edit marks the region to regenerate with **transparency** (an RGBA PNG,
transparent = edit); sd-server reads the `mask` field as **luminance** (white =
regenerate, black = keep) and **drops the alpha channel**, so a mask relayed as
sent edits exactly the wrong region — the picture's content. The gateway converts
on the way through (`convertImageEditMask`, `images_edit_mask.go`: an 8-bit
grayscale PNG, luminance = 255 − alpha, so a soft edge stays soft), which is what
makes the public endpoint genuinely OpenAI-compatible instead of sd-compatible,
and lets the portal emit the same mask through the same conversion. The checks
read headers first, before any pixel is allocated, and the whole conversion runs
before the preflight, so none of its refusals spends an admission: a PNG; exactly
the **first** source's size (the first `image[]` part when there is one, else the
first `image`, as sd-server picks; further `image[]` parts are references of any
size); at most **16 megapixels**, read off the header (a PNG is bytes of header
for any pixel count, so a body cap does not bound what a decode allocates — a
70000 x 70000 mask is KBs on the wire and tens of GB decoded); and **at least one
transparent pixel**, because an opaque mask would otherwise be a silent all-black
"edit nothing" that still cost a full diffusion run (a luminance mask sent to the
OpenAI endpoint ends up here). They answer `400 images.mask_invalid` (size, cap,
no region) and `400 images.edit_unsupported_format` (a mask that is not a readable
PNG, or — only when a mask is sent, since without one nothing is decoded — a
source that is not a readable PNG or JPEG). A mask edit is always a rebuild; the
converted part is named `mask.png` because sd-server needs a file part and a
client-chosen name may be non-ASCII.

The portal's mask is the same artifact. It is an ordinary `image_url` content part
carrying one extra member, `purpose: "mask"` — a sibling tag, not a new part type,
so the blob extraction, the owner-scoped resolution, `editUserMessage`, the replay
path and the renderer all inherit the source's whole lifecycle with no new code
path. A part without the member is a source, so a no-mask edit is byte-identical
to a whole-image one. The executor sends the tagged part as the `mask` field and
counts **only sources** for the `image` / `image[]` spelling; a mask with **no
source** is not an edit and is ignored (not even decoded); a second mask, or one
that is not a PNG, fails the run with `gateway.chat_run_image_source_unreadable`,
because the part is labelled `image/png` and refusing beats mislabelling. The
composer's mask slot takes a PNG only, needs a source first, and a mask is dropped
with its source. The in-browser mask-*drawing* canvas is deferred (it needs the
same backend), and the client does not compare mask and source pixel sizes: a
mismatch comes back as `images.mask_invalid` after the turn is stored.

**(g) The agent gains a `multipart_proxy` feature, the gateway gates on it, and
the agent is 0.9.0.** The router (`server-agent/internal/runtime/router.go`) read
`model` out of a JSON body and answered every other body
`404 runtime.model_not_managed`, so an edit relayed to an old router is
indistinguishable from an unmanaged model. The router now branches on
`multipart/form-data`: it peeks the `model` part with the same
`mime/multipart.Reader` and the same notion of a model part the gateway uses (a
text scan would let the two disagree about "which model", an allowlist-bypass
differential), walks the whole body, refuses a second `model` part or one over
1 MiB, **always** takes the plain proxy (a multipart request has no stream
notion), and answers the same 404 for a missing or malformed one, before any child
starts. JSON stays the default, byte-identical. `agent.Features` gains
`multipart_proxy`, which makes `const Version` **MINOR: 0.8.2 → 0.9.0** under the
repository's rule (a flag entered the registry; the changed request shape alone
would be PATCH). The gateway negotiates by the **name**, never the number
([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)):
an edit to a `server_agent` target whose agent has not declared it is refused
`404 images.edit_unavailable` before anything is relayed, and that gate shipped
**before** the agent change, so no window exists in which an agent target silently
breaks. It is **fail-closed by construction** (`AgentFeatures.Has` is false for a
server that never reported — which includes *every* server after a gateway
restart, until its next telemetry sample — and for a last sample lacking the
name), it sits after the resolve and the served-flavor conjunct, and it gates the
**edit only**: generations are JSON and every router serves them, and an external
`openai_images` target has no agent in front of it. The name is on the agent's
list alone (like `runtime_ensure`), spelled identically on both sides because the
agent module imports nothing from the gateway. The status is 404, a sibling of
`routing.model_not_capable`, **not** 409, 501 or 5xx: the OpenAI SDKs retry
408/409/429 and every 5xx, and a refusal only a different agent build can change
must not be retried. Like the served-flavor conjunct it is **not retried** against
a sibling application that could have served the model (recorded, not fixed).

**(h) Iterative refine is reference-not-copy.** Post-[ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url)
a generated image is already a `/api/portal/chat-blobs/{id}` reference in the
transcript, so "refine this image" does not copy, re-embed or invent a part type:
a control on each generated image of an assistant turn (offered only where
`attachMode` is `edit`) **seeds the composer** — it never sends — with that
image's own existing reference, string for string, and the next send is an
ordinary edit turn whose source is that reference. The executor needed no change
(it already resolves any reference in the last user message), which is the point:
the run commit vets that the blob is this chat's, a foreign or missing one fails
closed, and no `data:` URL appears in the request. Two consequences of using a
**chat-bound** reference as a composer attachment are handled: it is dropped on a
chat switch (sent in another chat it would be refused as not owned), where an
uploaded `data:` attachment still carries across; and an old inline `data:` result
is seeded verbatim and migrates on its run as an upload does. Regenerating or
editing a refine turn replays the same reference.

**(i) A webp or gif edit SOURCE is refused in the portal's executor, and webp
OUTPUT is silently defaulted on edits.** sd-server's edit route reads **PNG and
JPEG only**, and nothing between a portal run and it checks a whole-image edit's
source type (the gateway's endpoint converts a mask, and only *then* looks at the
source), so a webp or gif source would be forwarded and come back as the
upstream's own failure, `images.upstream_error`, whose "please try again" can never
come true. `imageEditInputsOf` therefore refuses a readable source of any other
type with `gateway.chat_run_image_source_unsupported` **before** anything is sent,
a code of its own (not `images.edit_unsupported_format`, whose copy is about masks,
and not `…_unreadable`, whose "attach it again" would change nothing), with copy
that says what to do: set the image format to PNG or JPEG and regenerate the image
first. This covers the ways such a source arrives — a refine of the webp result of
a thread whose format is webp, a replay of an older upload, a regenerate — and the
composer additionally restricts a *new* attachment to PNG/JPEG (a HEIC photo is
converted to JPEG, as on the vision path). **The public passthrough is
deliberately NOT so restricted**: an external client may send what its own sd build
reads. The mirror-image behavior, **accepted and documented rather than fixed**: an
edit's form **omits** an `output_format` outside `{png, jpeg}` together with its
`output_compression`, so a thread set to **webp** gets the upstream's own default
(png or jpeg) on its edit turns — silently, from the user's point of view — while
the thread's setting is untouched and its generations still honor it. The omission
is in the executor, not the composer, because the executor is the one place that
knows a turn is an edit on every path (send, edit-message, regenerate) and it does
not rewrite the persisted setting. The two together mean **a webp-generating
thread's results cannot be refined until its format is PNG or JPEG**, which the
message says.

**(j) The body-size cap, and how it relates to the agent's.** The JSON inference
endpoints read uncapped, to match llama-swap
([ADR-009](#adr-009--no-body-size-cap-on-inference-1-mib-on-control-plane)); an
edit cannot, because the handler holds the whole multipart body in memory to peek
the form and, for a mask or a model rewrite, rebuild it, so an unbounded read is a
memory exposure this endpoint has to answer for.
`OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES` (default **32 MiB**; junk, 0 or negative
→ the default) bounds what the **client** sends: a declared `Content-Length` over
it is refused without reading a byte, an undeclared body is stopped by
`http.MaxBytesReader`, both `413 request.body_too_large`. The default equals the
**agent router's own** `MaxBytesReader` (32 MiB), and the relationship is
*enforced, not assumed*: preparing a body can **grow** it (the converted mask of a
highly compressed one, the longer model name, the new boundary), so the body
handed to an agent-launched target is re-checked against the agent's 32 MiB
**after** any rebuild and refused `413 request.body_too_large` at the gateway
rather than sent to come back as the agent's `runtime.request_too_large`. Raising
the setting therefore never lets an oversized body reach an agent; it only admits
larger ones to external upstreams. The bundled nginx already allows
`client_max_body_size 0` on `/v1/`, so the gateway's setting is the effective
limit. Capture does not store the binary either: the request envelope for a
multipart body is a JSON summary (part names, file names, types, sizes and the
**text** fields; every file part `redacted`, failing closed on a body it cannot
read), because `string(raw)` through `json.Marshal` would corrupt binary into
U+FFFD and clip a file mid-way ([Security, Authentication & Authorization
§14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers)).

**Consequence:** editing is available wherever generation is, with one operator
opt-out per mapping and no new durable state (the capability reuses the verdict
table and one opt-out row), through a single endpoint the portal and API clients
share. Accepted costs, stated so they are not discovered: the derivation is
type-blind (a); the composer's edit offering follows `ModelDTO.ImageEdit`, which
**does not know about `multipart_proxy`**, so a portal user can pick an
agent-hosted model on an agent that has not declared it and meet the 404 — the
code has its own label, and its message names both causes (an agent older than
0.9.0, or one that has not reported since the gateway or the agent restarted,
which its next telemetry sample closes); the composer takes a single source (the
executor itself is lossless and sends repeated `image[]` in order, so lifting that
cap needs no backend change); and the whole path is exercised against the real
gateway handler, a stub upstream and sd-server's source, **not against a live
sd-server** (the mask polarity, the `strength` block and the canonical multipart
part-header spelling are read from its source, not observed running).

**Rejected:** an auto-written `image_edit: yes` row (rank 1 or 2) with a manual
`no` (rank 3) outranking it — a new producer in both the health loop and the agent
twin, where derive-at-the-gate needs neither (the same spirit as ADR-055's no new
durable store for a derivable value). — **A mode toggle or an "edit" button** (c).
— **A separate edit `kind`**, or a client setting that picks the URL: the decision
is a function of the prepared history and belongs to the executor (c). —
**Adding `(image_edit, no)` to `reservedManualVerdicts`**, or leaving `image_edit`
off `reservedAgentCapabilityNames` (b). — **Plumbing `multipart_proxy` into the
composer's offering**: a second axis in the offering for a condition that is
transient and recoverable, where the 404's label is the safety net. —
**Narrowing the public endpoint to PNG/JPEG sources** (i). — **Restricting the
composer's output-format list while a source is attached** (i): the omission is in
the executor, where it holds on every path. — **Metering input images** (d): the
unit is output images, and a two-input edit bills like a zero-input generation.
→ [API Compatibility & Inference
§3.5](cross-cutting/compatibility-and-inference.md#35-openai-images-edits),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Agent-Managed Model Runtime
§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Security, Authentication & Authorization
§14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[Configuration Reference](reference/config-env.md),
[HTTP API Surface
§1](reference/api-surface.md#1-inference--compatibility-endpoints).

## ADR-058 — Claude model aliases are computed per token from one shared derivation, hashed over the gateway id, and carried as sibling metadata

**Context:** Claude Desktop 2.37337.1 and Claude Code 2.1.295 (checked
2026-10-15) filter the `id` of a discovered model and reject several
non-Anthropic family names such as `qwen` and `gpt`, so a local model under its
real id cannot be selected. They accept a neutral id such as
`anthropic-local-556f772a70b1`, do not scan `display_name` or unknown extension
attributes for those names, and discard unknown discovery attributes they do not
use (an observation of the tested versions, not a documented guarantee). Issue
#225 asked for a per-token opt-in that offers every eligible local model under
such an id, in discovery **and** in inference, with the real id recoverable from
discovery alone so a config-sync script needs only an inference token. Each
decision below has a cheaper obvious answer that is wrong in a way no
happy-path test would catch.

**(a) Aliases are computed on every use, never materialized.** They are derived
from the token's current, alias-free listing and are never written into its
`model_override_map`. A new eligible model appears and a removed one disappears
without any sync step, the operator's manual rules stay exactly as written, and
disabling the setting removes the generated rows and the generated resolution
together.

**(b) One derivation feeds both callers, so a listed alias is a resolvable
alias.** `claudeAliasesFromListing` (`internal/portal/claude_aliases.go`) is
called by both discovery paths (`modelsResponse` and
`modelFlavorSetsWithPreSuppress`, which hold the listing in hand) and, through
`claudeAliasesForToken`, by `Service.ClaudeAliasTarget`, which inference
resolution calls from `resolveModelOverride`. It reads the **alias-free** listing
(after the group overlay and the visibility suppression, before any alias), so
no alias is derived from another alias or from a manual one, and resolution is a
single lookup of the requested id whose result is returned as it is. The
resolution sits in the shared preflight, so every gate judges the target and
`RequestedModel` keeps the alias for usage.

**(c) The identity is the gateway id; the encoding is hexadecimal with a sliding
window; it is a compatibility contract.** The alias is `anthropic-local-` plus
the first 12 lowercase hex characters of the SHA-256 of the UTF-8 gateway model
or group id — never the upstream model name, a label, a list position or the
loaded state, so it is stable across reordering, restarts, metadata, load events
and group-member changes. If a window contains a denylisted family marker (only
`abab` is expressible in hex), the window **slides** along the 64-character
digest and the id stays hexadecimal, rather than switching encodings; a digest
with no clean window skips the alias with a diagnostic. Clients persist alias
ids, so the prefix, the hash, the window length and order and the hex-expressible
denylist entries are pinned by tests. Distinct ids that derive the same alias are
**both** skipped, and an alias equal to a real name or a manual rule is skipped
with the real identity or the rule winning, because routing one alias to two
targets, or letting a generated id shadow a configured one, is worse than a
missing alias.

**(d) Precedence: an explicit rule, then the generated alias, then the catch-all,
then the unknown-model redirect.** A manual rule always wins, so the derivation
also gives no alias to a name a rule is keyed on or hides with `hide_target`
(the alias would disagree with the rule, and its `original_model_id` would reveal
a target the rule keeps out of discovery). The generated alias beats the
catch-all because an alias names one specific model, while the catch-all
rewrites whatever nothing more specific claims.

**(e) Eligibility is decided by composition and fails in the safe direction.**
"Local" holds because the alias-free listing is composed only of mapping views
and the groups over them, never of vendor accounts, so no display label is
interpreted; a vendor-account model, an Anthropic account's included, never
receives an alias. A name a vendor account of the token also serves is excluded
**whatever the vendor mode**, because the mode is runtime-configurable and an
alias offered under `fallback_only` would dispatch to the vendor after a flip to
`vendor_first`. When the visibility overlay cannot be loaded the listing fails
open but the derivation fails closed, because an alias would reveal what the
operator hid.

**(f) The metadata is two top-level siblings of `capabilities`, on generated
rows only.** `original_model_id` and the honest `display_name` are written by one
helper, `addClaudeAliasMetadata`, for `/v1/models`, `/openai/v1/models`,
`/api/v1/models` and `/anthropic/v1/models`, so the listings cannot drift. They
reuse the existing extension layer of the row rather than a new vendor
namespace, and a manual override alias never carries them, since it may
intentionally hide its target. The label is the target's own, never a Claude
model name, so the alias row cannot pass for a genuine Anthropic model.

**(g) `/anthropic/v1/models` lists from the `Models()` rows.** Only a row carries
the alias metadata, so `handleAnthropicModels` moves to the pattern ADR-045 (h)
gave `/v1/models` (the `Models()` rows narrowed by flavor), and `ModelsForFlavor`
stays as the ids-only reference the listings are pinned to.

**Consequence.** A token without the setting pays one boolean test. An opted-in
token pays a derivation (one mapping traversal, one group-overlay load, one
runtime-spec read per `server_agent` application and the vendor read, uncached)
only for a requested id that starts with `anthropic-local-`, and on every listing
call. The migration is one `integer not null default 0` column whose default is
the backfill (v88). The feature leans on an observed client filter: a client that
changes it can break the aliases without any gateway change, and the setting
stays opt-in for that reason.

**Rejected:** **Writing the aliases into `model_override_map`** — the manual map
would drift from the models and the operator's own rules would be mixed with
generated ones. — **Two derivations, one for the listing and one for the
resolver** — a listed alias could stop resolving, or a resolvable one go
unlisted. — **A decimal or other non-hex fallback encoding** — the ids would stop
being uniformly hexadecimal and the fallback would be a second format to
maintain. — **Metadata nested under a vendor namespace, or on every override
alias** — the sibling keys reuse the existing row, and a manual alias may hide
its target on purpose. — **Rewriting the response `model` back to the alias** —
the tested client accepts the served id, so it is not required. — **Failing open
when visibility is unknown** — the alias would reveal a hidden model. — **A
vendor-mode-dependent shadow check** — the mode can flip between discovery and
resolution.

→ [API Compatibility & Inference
§9.1](cross-cutting/compatibility-and-inference.md#91-generated-claude-model-aliases),
[Routing & Model Selection
§2.4](cross-cutting/routing-and-model-selection.md#24-generated-claude-model-aliases),
[Security, Authentication & Authorization
§4](cross-cutting/security-auth-rbac.md#claude-model-aliases-are-discovery-and-resolution-not-authorization),
[Persistence §3](cross-cutting/persistence.md#3-the-migration-runner),
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
[HTTP API Surface](reference/api-surface.md#generated-claude-model-aliases).

## ADR-059 — Claude Code effort is delivered as a chat-template kwarg on both paths, for llama.cpp and vLLM, and is a management-only token setting

**Context:** Claude Code states its reasoning effort (`/effort low|medium|xhigh`)
as `output_config.effort` on every Messages request. A local Qwen served by
llama.cpp or vLLM ignores it: Qwen's chat template, including the fixed one
this deployment runs (`froggeric/Qwen-Fixed-Chat-Templates`), reads effort only
from the jinja variable `reasoning_effort`, which the server fills from
`chat_template_kwargs`, and llama.cpp's Anthropic-to-OpenAI converter at the
pinned build (`08698c81`) drops both `output_config.effort` and a top-level
`reasoning_effort`. Issue #230 asked for a per-token opt-in that makes the
picked level reach the template, on native passthrough and on translation,
without touching any other backend. Each decision below has a cheaper obvious
answer that is wrong in a way no happy-path test would catch.

**(a) One mechanism, on both paths: `chat_template_kwargs.reasoning_effort`.**
It is the only field that reaches Qwen's template on every backend and build
the gateway targets, so it is the one lever on both paths: a value-lossless
body edit on the native relay and a provider emission on the translate path. The
translate path also keeps sending the top-level `reasoning_effort`, which is
harmless and enough for llama.cpp and the newest vLLM, but it is not the lever
because a released vLLM needs the kwargs.

**(b) Scope is llama.cpp and vLLM, selected by the resolved target's effective
kind.** `targetUsesChatTemplateKwargs` reads `Target.Provider`, or
`Target.LiveProgressSpecType` when the provider is the literal `server_agent`,
and accepts exactly `llama_cpp` and `vllm`: direct and agent-managed alike. It
follows the `wantsResponsesLiveTimings` precedent and is never inferred from an
alias name, a model family or the presence of `/v1/messages`. It excludes
`llama_swap` (it proxies to an arbitrary downstream), Ollama, generic
OpenAI-compatible applications, native Anthropic and vendor accounts. It is a
separate predicate from `routing.LiveTimingsCapableKind`, whose membership only
coincides with it today, and a cross-vocabulary test pins that
`RuntimeSpecTypeLlamaCpp` and `RuntimeSpecTypeVLLM` spell the same strings as
`ProviderLlamaCPP` and `ProviderVLLM`.

**(c) The effort is forwarded verbatim.** It is a trimmed JSON string passed on
by name, with no enum and no `high`-to-`xhigh` mapping: the template aliases
names itself, and a mapping would be the gateway guessing about a template it
cannot see. A missing, `null` or blank effort injects nothing and invents no
default.

**(d) The native edit is a third, separate edit, and the only one that can
reject.** `proxyNative`'s edits are the model rewrite, llama.cpp's
`timings_per_token` and this one, each in its own step and each returning a
**fresh** slice, so the client's bytes (`rel.raw`), which the payload capture
reads after dispatch, are never written to. It runs only when the token opted
in, the target is capable and the request is the Anthropic Messages flavor, so
any other request is not even inspected. A malformed effort, a non-object
`chat_template_kwargs` or a `chat_template_kwargs.reasoning_effort` that
conflicts is `400 messages.invalid_output_config`, because the alternative is
to overwrite or drop a value the client sent. A JSON `null` kwargs is read as
absent, as `effort: null` is, so the two sibling nulls are not read opposite
ways. The rejection uses `writeJSONCaptured` and `recordUsage`, like the
endpoint-disabled refusal and unlike `writeRequestError`, so an operator can see
it in Activity and Logs.

**(e) The translate request stays provider-neutral, and the effort is cleared
off scope.** The parser takes a bool option (`ParseAnthropicMessagesWithOptions`)
and never token storage, and reads `output_config.effort` into the neutral
`ReasoningEffort` only when it is on; with it off `output_config` is not
inspected, so no request is newly rejected. A policy step after resolution
(`applyChatTemplateEffortPolicy`, the shape of `applyAnthropicCachePolicy`) works
on the upstream copy: for a capable target it sets the internal directive
`ReasoningEffortViaTemplateKwargs` (`json:"-"`), and for **any other** target it
**clears** the effort. That clearing is the decision that is easy to omit,
because the field is only ever filled by this feature on this path, and leaving
it would leak the setting off its scope: a top-level `reasoning_effort` to a
generic or vendor OpenAI-compatible target, a `thinkingConfig` to Gemini, an
overridden default to an OpenAI Responses target. A direct OpenAI request that
sets its own effort is exempt, because the policy acts only on the Anthropic
Messages flavor.

**(f) Setting it is management-only, enforced in the service layer, and distinct
from the alias flag.** The token routes are gated by `requireWebScope(gateway:use)`,
which every user bearer carries, so an inference-only bearer could otherwise
turn on, or off, a switch that changes what the gateway sends upstream, for its
own token. `principalMayManageTokenSetting` (`owner.ID == "" || isAdmin(owner)`)
allows a portal session, whose principal has an empty token id, and an
admin-scope bearer, and nothing else, on `CreateToken` (only when enabling),
`UpdateToken` (any present value) and `CreateServiceToken` (when enabling),
before anything is applied. The refusal is `403 portal.principal_forbidden`, which
makes the sentinel HTTP-reachable for the first time: the create handler needed
its own error-ladder row, or the refusal would have been a 500. `claude_model_aliases`
keeps its non-gated behavior, since it changes a name and not what is sent
upstream.

**(g) It is its own flag and its own column (migration 89).** `integer not null
default 0` through `addColumnIfMissing`, the shape of migration 88, so the default
backfills every token to off. It is independent of `claude_model_aliases`:
enabling one never enables the other, and an alias-less token can use it.

**Consequence.** A token without the setting pays one boolean test on the native
path and the parser's option check; a flag-off request, whatever its
`output_config`, behaves exactly as before. An opted-in request to a capable
native target pays one JSON decode of the body to find the effort (a re-marshal
only when it adds the kwarg), and the re-serialized body is value-identical, not
byte-identical. The feature depends on what the deployed template and build do
with the value: a template that reads only `low`, `medium` and `high` would see a
raw `xhigh`, and a later llama.cpp that maps `output_config.effort` itself would
make the native edit redundant, so a smoke test that the template consumes the
injected value is the check to run.

**Rejected:** **A top-level `reasoning_effort` only** — llama.cpp's Messages
converter drops it and a released vLLM does not read it. — **Relying on
`output_config` passing through** — no template reads it. — **A llama.cpp-only
selector** — vLLM serves the same template and needs the same value. — **An enum
or `high`-to-`xhigh` remap in the gateway** — it second-guesses a template it
cannot see. — **Selecting by alias name, model family or endpoint** — none of
them says what serves the request. — **Leaving the effort on a non-capable
translate target** — it leaks as a top-level field, a Gemini `thinkingConfig` or
a Responses effort. — **Always validating `output_config`** — a flag-off token
would be newly rejected for a field it has always been free to send.
— **Silently overwriting a conflicting `reasoning_effort` or a non-object
`chat_template_kwargs`** — it discards what the client chose. — **Gating the
setting on the HTTP scope only** — an inference-only bearer holds it. — **Gating
it on the alias flag, or folding both into one setting** — they are independent
decisions with different authority.

→ [API Compatibility & Inference
§9.2](cross-cutting/compatibility-and-inference.md#92-claude-code-reasoning-effort-compatibility),
[Security, Authentication & Authorization
§4](cross-cutting/security-auth-rbac.md#claude-code-effort-compatibility-is-a-management-only-setting),
[Persistence §3](cross-cutting/persistence.md#3-the-migration-runner),
[Data Model §4](reference/data-model.md#4-migration-history-95-migrations),
[HTTP API Surface](reference/api-surface.md#claude-code-effort-compatibility).

## ADR-060 — Shared vendor accounts: a tiered share level, a pure routing grant, authorization by level, and a per-account opt-in

**Context:** a vendor account ([ADR-049](#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted))
was strictly personal: owned by one user, reachable only by that user's session and
by the tokens that user had opted in ([ADR-050](#adr-050--vendor-account-access-is-a-per-token-opt-in-enforced-in-listing-and-routing-through-one-prefix-helper)).
An operator then wanted the **organization's** key or subscription usable by other
users and by services, managed by a team rather than one person, without handing
anyone the secret. ADR-049 had named this its second phase ("a sharing link table plus
a resolver filter extension") and kept the model additive on purpose. The obvious
answer is to copy how AI servers are managed and provisioned, because the portal
already has every piece of it (admin groups, a containment root, resource groups, the
provisioning gate, a co-manager permission). That answer is right for management and
**wrong, in a way no happy-path test would catch, for routing**: a server is a shared
*resource* that defaults open to everyone, a vendor account is a *credential* that must
default to its owner. Each decision below has a cheaper obvious alternative that leaks
the key or hands a delegate the owner's token.

**Decision — five choices, taken together.**
- **(a) A tiered `share_level` enum, `none` / `shared` / `full`, not a boolean.**
  `vendor_accounts.share_level` (migration 91, `text not null default 'none'`) puts an
  account in the sharing model and sets how much a co-manager may do. `none` is the
  untouched personal account and what every existing row is, by the column default
  alone, with no data step and no behavior change on upgrade. `shared` lets a co-manager
  **operate** the account (status, model prefix, rename, test, refresh); `full` is
  **complete owner-equivalent delegation** (the credential, the group and
  resource-group assignment, the delete and the lowering too), except raising the
  level. Three named states express a reach that two flags cannot, and give the
  subscription cap (d) something to be capped at. The stored level is read **live** by
  every gate, listing and grant, so a lowering never waits on a cache; the two sharing
  switches come from one 5 s cache that every portal settings write drops, so a pause from
  the portal is just as immediate and only an out-of-band edit of a switch has a tail of up
  to 5 s.
- **(b) Management mirrors AI servers; routing eligibility is a PURE grant.** A shared
  account is assigned to **management groups** (`vendor_account_admin_groups`), rooted
  in one **containment root** (`system_group_id`, immutable while shared) and linked
  into **resource groups** (`resource_group_vendor_accounts`) by two paths (the
  owner's self-service, the resource-group manager's side), and managed by co-managers
  holding a sixth permission, `can_manage_vendor_accounts` (migrations 90 to 92). But the
  routing/usage grant is **not** the server rule: an account is available iff it is
  **owned**, or it is **live `shared`/`full` and linked to a resource group the
  principal is provisioned into**, **fail-closed**. `AllowedServerIDs` makes a server in
  no provisioned group available to *everyone* in its default opt-in mode, where it also
  fails open on a store error; copying that for a credential would leak it. The vendor
  grant reads neither `resource_provisioning_enforce` nor an open-pool branch, so a
  shared account in no group is usable **only by its owner**; the owner's own accounts
  are always returned and only the shared arm fails closed; an unwired enumerator seam is
  **owner-only, never allow-all** (the inverse of `ProvisioningGate`'s nil). There are **two
  enumerators**: the narrow routing one (own, then live-shared-via-group), parity-bound
  with the model listings so an advertised name is a routable one, and the wider
  management union (own, managed, member) that only the Anbieter page reads and routing
  never does.
- **(c) Authorization by share level, with `full` = owner-equivalent.** Two write gates
  plus a read gate; every decision that lets a non-owner through on the strength of the
  level reads the *effective* level (`none` while the sharing master switch is off). The
  write gates are **operate** (the owner, or a CM at `shared`/`full`) and **credential**
  (the owner, or a CM at `full`; also the connect calls, the delete, the lowering and the
  management-group assignment). **PATCH is field-level and deny-by-default**: `name`,
  `status` and `model_prefix` take the operate gate and **every other field** the
  credential gate, so an overlooked field can never fall through to the weaker one, and
  an operate-only PATCH writes only the columns it names. **System scope stays read-only
  and never bypasses a write.** Raising is the owner who is an admin *right now*;
  lowering needs only ownership at `shared` (an owner who lost the role can still
  un-share). A caller who **cannot see** the account gets the `404` of an unknown id; a
  CM who **can** gets a `403`, because "not found" would be a lie to them, a deliberate
  departure from the servers' posture, where every object-level denial is a `404`
  (`server.forbidden` is only a create or a field the caller may not set). A **member** reads a minimal,
  allowlist-built key-free view and operates nothing, and no response to any viewer
  ever carries a credential.
- **(d) A subscription is capped at `shared`, and its token-opening operate calls stay
  owner-only.** The subscription path is ToS-restricted and "built for internal testing,
  never a supported multi-tenant capability" (ADR-049 (b)), so sharing one is allowed
  but fenced: it may be raised to `shared` **never `full`** (its OAuth credential stays
  strictly owner-only), only while its own switch `vendor_subscription_sharing_enabled`
  (default off, subordinate to the master switch) is on, and even at `shared` the
  **connection test, the model refresh and the usage refresh** are owner-only for it,
  because each opens the token set and the two refreshes can persist a rotated
  **single-use** refresh token or trip an account-wide `needs_reconnect`. A CM keeps
  status, prefix and rename. The rule tests the auth type, not the level, so it fails
  closed. The cap and its switch are checked inside the raise itself, so a subscription
  can never be raised ungated.
- **(e) The per-token opt-in stays explicit and is per account for a shared one, on user
  and service tokens alike.** Reach is necessary, never sufficient: a token must **name**
  the account in its `vendor_provider_access` policy, with the optional prefix override of
  ADR-050. `all: true` covers only the principal's **own** accounts (and so does a portal
  session, which is always `all: true`), never a shared one, so borrowing another owner's
  credential is always a deliberate, named choice; an `all: true` policy may still carry
  an entry naming a non-own account, which is a live opt-in validated like any other.
  The own-versus-shared line is one function (`TokenOwnsVendorAccount`). The save-time
  check validates against the **token's own principal** (the owning user, or the service),
  never the admin saving it, and is **advisory**: routing re-evaluates the grant on every
  request and is the sole security gate. A **service token** gets the opt-in as new,
  **create-only** plumbing (`vendor_access` on the create request and DTO, preserved by a
  rotation, changed only by recreating the token) and reaches a shared account only when
  the service is **directly provisioned** into a containing resource group, the way it
  reaches servers; a service-side resource-group self-service makes that provisioning
  visible and editable.

**Consequence:** a deployment that never turns on `vendor_account_sharing_enabled`
(default off, fail-closed) behaves as personal accounts always did: every account reads
as `none`, nothing stored is touched, and the switch is a **pause, not an un-share**. With it on,
the gates, listings and grant are all live reads, so a downgrade, a removed provision or
a paused switch takes effect on the next request and a stale link never grants. The costs
are named, not hidden ([Risks §11.1](11-risks-and-technical-debt.md#111-operational-risks),
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)). The grant runs
on every routed request that reaches the vendor branch, before the token's policy is
consulted (its structural reads are uncached on purpose, so the grant stays live; the
sharing switches it asks for come from one 5 s invalidatable cache that every settings
write drops). `vendor_account_usage` stays **one
snapshot per account**, mixing borrowers, who spend the owner's rate-limit windows and
credits. The owner is single (`owner_user_id … on delete cascade`): removing the owning
user's row would hard-delete a `full`, org-shared account and revoke every borrower, with no
ownership transfer, a named follow-up. A `disabled` resource group still grants its linked
accounts, as for servers. Management is not usage: a CM who is not provisioned into a
containing group can operate an account but cannot route through it. A credential-gated
PATCH still rewrites the whole row, so the lost-update debt on a concurrent token rotation
remains, and its trigger is any credential-gated PATCH, an echoed `base_url` or `auth_type`
included, not only an `api_key` clear.

**Rejected:** copying the server open-pool rule or its fail-open direction for a credential
(it would hand every user the owner's key); a boolean `shared` flag (it cannot express the
operate-versus-owner-equivalent reach, nor cap a subscription); making every shared account
owner-equivalent (a co-manager of a `shared` account could then replace the key or delete the
account) or none of them (nothing to delegate); letting system scope write to a shared
account (the writes send the owner's credential upstream); one enumerator for routing and
management (the page must show an account a CM manages without being provisioned into it,
and routing must stay narrow and parity-bound); letting `all: true` or a portal session
auto-borrow a shared account (borrowing would not be a deliberate act); a nil seam that
allows all (an unwired grant must fail closed); authorizing PATCH by a list of
credential-sensitive fields (a list of what is sensitive is what an overlooked field falls
out of); validating a token's opt-in against the admin saving it (a token does not inherit
its creator's reach); an editable service-token opt-in (it would be the only mutable
service-token setting); per-borrower usage snapshots and limits (deferred, the events
already carry the data); an ownership-transfer operation (named follow-up); and sharing a
subscription as `full` or without its own switch (the ToS-restricted path must stay
opt-in and owner-credentialed).
→ [External Vendor Accounts §1](cross-cutting/external-vendor-accounts.md#1-the-entity-and-its-ownership),
[§4.6](cross-cutting/external-vendor-accounts.md#46-routing-to-a-shared-account-the-pure-grant),
[§8](cross-cutting/external-vendor-accounts.md#8-authorization-by-share-level),
[§11](cross-cutting/external-vendor-accounts.md#11-per-token-vendor-access),
[Security §12](cross-cutting/security-auth-rbac.md#12-delegated--resource-scoped-authorization),
[Data Model](reference/data-model.md#4-migration-history-95-migrations) (migrations 90 to 92),
[API Surface](reference/api-surface.md#vendor-accounts-anbieter),
[Risks & Technical Debt §11.1](11-risks-and-technical-debt.md#111-operational-risks) and
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-061 — Speech is the second endpoint on the capability gate: a distinct `speech` capability, a coarse `openai_audio` flavor, the `audio_cpp` kind, and a binary relay that serves WAV only

**Context:** `POST /v1/audio/speech` (issue #68) is the next multi-modal endpoint
after image generation
([ADR-042](#adr-042--the-images-gate-keys-on-a-required-capability-and-an-absent-verdict-refuses),
[ADR-044](#adr-044--stable-diffusioncpp-is-a-first-class-type-and-openai_images-is-a-coarse-opt-in-flavor)).
It bills on the non-token pair of
[ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar), and it is
the first whose response is **not JSON at all**: it relays a binary `audio/wav`
body. The images template carries over almost whole, a dedicated handler relaying
through `proxyNative` behind a required-capability gate, so this entry records what
speech decided *differently*, and why. The inputs are facts about the backend,
audio.cpp's `audiocpp_server` (v0.9.1, probed live), that no design could have chosen:

- Its capability signal is the per-model `task` field of its own `GET /v1/models`
  (`tts` and `clon` synthesise speech, `asr` transcribes), and that listing names
  **loaded** models only.
- It answers `response_format` `wav` or absent with a binary `audio/wav` body, a
  `Content-Length` and metering headers (`X-AudioCPP-Audio-Duration-Ms`,
  `X-AudioCPP-Wall-Ms`, `X-AudioCPP-RTF`). It answers `mp3` and every other
  unsupported value with **raw WAV, still labelled `audio/wav`**, and answers `json`
  and `b64_json` with base64 WAV in a JSON body that carries **none** of the
  metering headers.
- Its errors are OpenAI-object-shaped (`{"error":{"message","type":"server_error"}}`)
  but it answers **HTTP 500 for client errors** (a missing `input`, an unknown model),
  has no authentication of its own, and refuses a streaming request to an offline
  model with a 500.

**Decision — ten choices, taken together.**

- **(a) `speech` is a capability of its own, and the images gate carries it
  unchanged.** `routing.CapabilitySpeech = "speech"` means the model **synthesises**
  speech. It is not `CapabilityAudio`, which means the model **accepts** audio as input
  (a llama.cpp `/props` modality with its own writer), exactly as `image` is not
  `vision`; the two can co-occur and neither is ever mapped onto the other. It also
  does **not** coincide with the billable unit (`audio_second`), unlike `image`: a
  capability names what a model can do, the unit names what a served request is metered
  in. The handler sets `RequiredCapabilities = ["speech"]` from its own identity,
  and the existing `filterCapable` refuses a model without a `yes` verdict on every
  resolve branch, **the affinity pin and the server override included**
  ([Routing & Model Selection §2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).
  ADR-042's three rules all carry over: an absent row is unknown and **refuses**, a
  store error refuses, and `(speech, no)` is reserved in `reservedManualVerdicts` so a
  rank-3 manual `no` can never veto the probe that is entitled to write it. The
  capability gate and the affinity guards did not change for speech, which is the return
  on ADR-042's cost comparison: the endpoint paid for a constant, a flavor case in
  `NormalizeAPIFlavor` and a verdict writer. One dependency is worth
  naming because it is easy to lose: the capability list is also what keeps a speech
  request **off the vendor-account path**. `vendorAccountRoutingEligible` excludes the
  images flavor by name and has no `openai_audio` twin; it declines a request only
  because it carries a capability list, and a vendor model is matched by name, never by
  flavor. A handler that dropped the list would skip the gate **and** become routable
  to a hosted vendor model of the same name, so a test pins the list.
- **(b) `openai_audio` is a COARSE flavor, opt-in everywhere, under the same
  migration-safety rule as `openai_images`.** `NormalizeAPIFlavor` tests the images and
  audio prefixes before the generic `openai` one, so the flavor folds to itself and
  candidacy admits only an application that lists it. An empty list on save still
  becomes exactly `[openai, anthropic]`. Because the flavor is a flavor of its own, a
  speech request's affinity key can never collide with a chat client's, and speech
  needs no endpoint mode and no translate path; `upstreamPath` answers
  `/v1/audio/speech` directly. **The audio-only rule is the images-only rule, applied
  to the second non-text flavor.** `routing.FlavorsAreAudioOnly` (the list names
  `openai_audio` and neither text flavor) keeps an audio-only server **out of the chat
  flavors**: the `openai` row of the listing and candidacy rule refuses an images-only
  **or** an audio-only list, so such a model is not listed by `/v1/models`, is not a
  redirect target for a text request, is refused by the text translate dispatch (404
  `routing.no_model_route`), and is never sent a chat prompt by the background jobs
  that send a mapping one of their own: the model warmer, the scheduled and manual
  benchmarks, the context probe, a Load and the VRAM probe. A benchmark refusal
  carries its own code, `benchmark.audio_only` (409), and the portal's mapping DTO
  carries `audio_only` beside `images_only`
  ([Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model)).
- **(c) audio.cpp is two things under one name, in two namespaces.** The **application
  type** `audio_cpp` (`routing.ProviderAudioCpp`) is a **static**, operator-run
  `audiocpp_server`: no runtime spec, dispatched to the shared OpenAI-compatible client
  (the relay is a native passthrough, and that client also reads the listing for the
  verdict). Its defaults follow the images type's logic: `timeout_ms` 600000 (a
  synthesis is a non-streaming request governed by one total deadline; the one default
  the backend applies too), the `["openai_audio"]` flavor, and both coding-agent
  endpoint modes disabled (both set by the application form, as for images). Unlike
  `stable_diffusion_cpp` it keeps the `health_path` mode, with the path `/health`,
  because audio.cpp serves `GET /health` and answers `/v1/health` with 404. The
  **runtime spec kind** `audio_cpp` (`routing.RuntimeSpecTypeAudioCpp`) is the
  agent-managed counterpart: detected from an `audiocpp_server` or `audio.cpp` binary
  basename, with the probe paths `("", "")` (no `/metrics`, and a speech model has no
  context window to probe)
  ([Agent-Managed Model Runtime §3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation)).
  **The verdict writer is gated on the application type, not on the spec kind.** The
  first design gated on `server_agent` plus a spec of the kind, and that is a gate that
  never opens in a real deployment: a static server has no spec, and the agent router's
  `/v1/models` carries no `task`, so the only server it could reach was one that could
  state no verdict.
- **(d) The verdict is read from `GET /v1/models`, per mapping, by the gateway's own
  health loop.** *(Spelling correction, issue #272: audio.cpp spells the clone task
  `clon`, not `clone` — its load API rejects `clone` and `/v1/models` reports `clon`,
  verified against v0.9.1. The parser originally matched `clone`, so a real clone model
  fell to the "any other task" branch below and read `(speech, no)` — refused everywhere,
  and #252's `speech_clone` verdict never fired. Both parser copies and this
  document now say `clon`.)* `probeAudioCppModels` finds the entry whose `id` equals the mapping's
  application-side model name and derives: `task` `tts` or `clon` is `(speech, yes)`;
  the model present with any other non-empty task is `(speech, no)`, authoritative
  because the listing states each loaded model's task (the loaded-only reading in this
  paragraph is superseded, see the note at the end of this choice); the model absent, the task
  absent or the body unreadable writes **nothing**, because none of those says the
  model cannot speak and a `no` can switch a route off. It is a new probe-rank source,
  `audiocpp_models`, ranking 1 with the other probes (never able to outrank a `manual`
  or `vision_benchmark` row, always able to repair its own drift), and it writes the
  `speech` row (the `transcription` row joined it in ADR-062 (c), the
  `transcription_streaming` row in ADR-065, the `speech_clone` row in issue #252 —
  `clon` → yes, `tts` → no, all derived from the same per-model `task` the `speech`
  verdict reads — and the `speech_streaming` row in [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives), derived from the
  per-model `mode` instead). **A model that is
  not loaded reads as unknown and refuses**
  until the health loop sees it loaded; for an operator-run server the mapped model is
  loaded, and `speech: yes` set by hand covers the rest. (*Superseded in part by
  [ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint) (c) and
  the amendment note below: the listing was found to name an unloaded model too, with
  `loaded: false`, and the prober never decoded `loaded`, so read "a model that is not
  loaded" in this choice and in the Consequence as "a model the listing does not
  name".*) **The agent-managed source was deferred here, and it now exists** (issue
  #255, agent 0.9.1). The agent router's `/v1/models` still carries no `task`, so the
  agent does not read it: it GETs an agent-launched `audio_cpp` child's **own**
  `/v1/models` over loopback, the base address every per-child probe already uses,
  derives the verdicts with a deliberate duplicate of the gateway's parser (the two
  copies pinned to one case table), and reports them under the same `audiocpp_models`
  source, **only while the gateway declares `capability_source_audiocpp`** (a name on the
  gateway's list alone, as `capability_source_sdcpp` is for images: the agent's own
  `Features` gained no entry, so the version bump is a PATCH). The ingest accepts the
  source for the `speech` and `transcription` rows only, at the probe rank, so an
  operator's manual `speech: yes` (rank 3) still outranks it and stays the override, and
  the enablement path for an agent before 0.9.1 and for a model the listing does not
  name.
- **(e) The request surface is WAV-only, and streaming is refused rather than
  half-served.** `response_format` must be `wav` or absent, and **every other value is
  400 `speech.response_format_unsupported`**: `mp3`, `opus`, `flac`, `aac` and `pcm`
  **and also `json` and `b64_json`**. There are two reasons, and the second is the
  decisive one. Forwarding `mp3` would silently hand a strict client WAV labelled
  `audio/wav`, which is worse than a refusal. And `json` and `b64_json` are not OpenAI
  speech formats and carry **no metering header**, so a relayed one would bill zero
  seconds on every success and log an Error each time. `stream: true` and any
  `stream_format` (`sse` or `audio`) are 400 `speech.stream_unsupported`: the relay's
  buffering and billing contract is one complete audio response. `stream: false`, an
  absent `stream` and a null `stream_format` describe what the endpoint already does and
  are accepted. The handler validates its own shape, a non-empty `model`
  (`speech.model_required`) and a non-empty `input` (`speech.input_required`),
  **before the relay**, because audio.cpp would answer those with a 500 that the gateway
  would hand on as a server failure. Both deferrals have a stated way back: streaming
  needs the `mode=streaming` model contract and its SSE events, and `json`/`b64_json`
  need a RIFF-header duration decode for billing. **(Amended by
  [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives), issues #253 and #263: the streaming half was taken. `stream: true`
  is admitted, relayed for a model whose `speech_streaming` verdict is `yes` and answered as
  the one-shot WAV otherwise; while streaming `response_format` must be `pcm` or absent
  (audio.cpp serves streaming only as `pcm`, so `wav` plus a stream stays 400
  `speech.stream_unsupported`), and `stream_format` `sse` and `audio` are relayed. A
  request that does not stream took `wav` or absent only. The `json`/`b64_json`
  half stands.)** **(Amended again by [ADR-069](#adr-069--non-stream-speech-pcm-is-a-wav-header-strip-not-a-transcode-the-gateway-relays-the-data-chunk-of-a-16-bit-mono-wav-as-headerless-pcm-refuses-any-other-clip-after-synthesis-and-leaves-mp3opusflacaac-to-264), issue #253 part 1: a request that does
  not stream now also takes `pcm`, served as the upstream WAV's `data` chunk, headerless, with
  `Content-Type: application/octet-stream`; `mp3`, `opus`, `flac`, `aac`, `json` and
  `b64_json` stand refused, the compressed ones moving to issue #264.)**
- **(f) The relay forwards the upstream `Content-Length` and still does not buffer.**
  `proxyNative` relays every native body chunked (it flushes per chunk and sets no
  length). For audio that is a loss a client can see: the body is one sized file,
  audio.cpp always declares its length, and a player shows a duration and a download its
  progress from it. For the speech flavor **only**, the upstream's declared
  `Content-Length` is set on the client response before the header is written, while the
  copier still moves 32 KiB at a time with a flush per chunk, so net/http frames by the
  declared length and the bytes keep streaming. An upstream that declared none stays
  chunked, and chat, Responses, Messages and images keep relaying chunked as before.
  **(Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives): a streamed speech answer, an event stream or raw PCM, is
  relayed chunked and the length is not set; a stream request answered with a plain
  `audio/wav` keeps it.)** The
  length is read off the upstream header (the provider response type carries no length
  field), which is sound because Go's client drops that header for a chunked or
  decompressed response, so a surviving value is the length the copier writes. The
  usage scanner is nil for speech, as for images: a binary body has no chat usage to
  find, and scanning it would retain audio per in-flight request to learn that.
- **(g) Speech bills `audio_second` from the upstream's own duration header.** **(Amended by
  [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives): that is the one-shot WAV's measure. A STREAMED response has no such
  header, so its quantity is counted from the relayed PCM bytes, in `audio_second` over a
  cached output byte rate or, with none, in the new `audio_bytes` unit, on a clean
  completion only; every failure row below stays `audio_second` with quantity 0.)** The unit
  is endpoint identity, as for images, so every speech `recordUsage` site, a refused or
  failed request included, carries `billing_unit = audio_second`. The quantity is a
  **measurement** and comes from the response: `X-AudioCPP-Audio-Duration-Ms` divided by
  1000, set only on a clean `success` (the full body reached the client). A success with
  **no usable duration** (absent, non-numeric, zero, negative, or `NaN`/`Inf`, which a
  float parse accepts and which would poison every sum) records `0` and is logged once at
  `Error`, because the pair has no "unknown", so the gap is made loud instead of
  repaired. Tokens and both rates stay zero, so `ValidateBillingXOR` holds, and the
  row's `provider_path` is `/v1/audio/speech` (its `req_path` is the path the client
  called, that or the `/openai/` alias), so Activity never labels it a chat completion
  ([Telemetry §8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event)).
- **(h) Capture stores metadata for a binary response, never the bytes.** The capture
  envelope stores a body as a string that `json.Marshal` rewrites to valid UTF-8 and the
  capture cap clips mid-file, so a captured WAV would be a corrupted fragment. A response
  whose own `Content-Type` is `audio/*` is therefore stored as a small JSON summary
  (content type, the size the relay actually wrote, `redacted: true`, and the upstream's
  `X-AudioCPP-*` headers). The gate is the **response** type, not the request flavor, so
  an error response (`application/json`) is captured like any other body. The request
  JSON, which carries the `input` text the opt-in capture exists for, is captured as
  usual
  ([Security §14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers)).
  **(Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives): a STREAMED speech response is `text/event-stream` or
  `application/octet-stream` and is still captured as this redacted summary, never as the
  event text; the capture's endpoint is stamped speech so the binary-speech backstop
  redacts it.)**
- **(i) A non-2xx upstream response is normalised to the OpenAI error shape.** The relay
  buffers the (bounded) error body and, in order: an OpenAI error object passes through
  **byte for byte** (audio.cpp's own shape, so its attested `type` is not overwritten
  with ours); a plain `{"error":"..."}` string is wrapped with the gateway's own
  `type: upstream_error` and `code: speech.upstream_error`; anything else is relayed
  unchanged. The upstream's status is relayed as it came, and a failed relay records an
  `audio_second` error row with quantity zero. The normalisation is gated on the speech
  flavor and a non-2xx status, so a 2xx WAV still streams and no other flavor's error
  handling changed.
- **(j) The admission surface is the images one, and `GET /v1/audio/voices` is
  deliberately not proxied.** Auth is a bearer token **or** the internal loopback pair,
  never the session cookie (a third user of that leg after generations and edits; the
  portal chat's speech run is now a caller of it, amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)),
  with the same run-as block copied in. The voices listing is an audio.cpp and llama-swap
  convention, not an OpenAI endpoint, so clients learn voice names out of band.
  **(Amended by [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter),
  issue #251: the gateway still does not PROXY the upstream's listing, but it now serves
  `GET /v1/audio/voices` itself, as a read of a persistent per-mapping cache filled while
  the model is loaded, on the same bearer-or-loopback leg, so a client can learn voice
  names from the gateway.)**

**Consequence:** speech has the images day-one cost, in a milder form. A deployment with
no `openai_audio` application serves no speech: a route without the flavor answers 404
`routing.no_model_route`, and a route with it and no `(speech, yes)` verdict answers 404
`routing.model_not_capable`. For an `audio_cpp` application that verdict arrives with the
first health tick that finds the mapped model loaded, and a model that is not loaded
stays unknown until it is (superseded in part, as in choice (d): read "not loaded" as
"not named by the listing", [ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint) (c)). Every speech request is one non-streaming call governed by one
**total** deadline, the application's `timeout_ms`: the `audio_cpp` type defaults it to
600000, an application of any other type that serves speech needs it raised by hand, and
the idle watchdog does not apply. Speech takes **no affinity pin**, by the guard ADR-042
wrote for exactly this: a capability-carrying request never creates one. The costs
accepted are named, not hidden
([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)): no
streaming (lifted by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)), no `mp3`/`opus`/`flac`/`json`, an unlisted model reading as unknown (an
agent-launched `audio_cpp` child's verdict comes from the agent's probe of its own
listing, once per process generation), and a capture gate that trusts the upstream's
`Content-Type`. The portal's Models view reads no speech flag, so its "Available via"
column cannot gate `openai_audio` behind a verdict the way it gates `openai_images`
behind `image`; the request itself is still gated server-side.

**Rejected:** **a new fine flavor or a per-capability column** (ADR-042's argument,
unchanged and now twice avoided). **Mapping `speech` onto `audio`** (`audio` is an input
modality with its own writer; advertising a synthesiser as a consumer, or the reverse,
fails far from its cause). **Relaying `mp3` as the upstream answers it** (a client would
be handed WAV labelled `audio/wav`). **Relaying `json`/`b64_json`** (no metering header,
so every success bills zero seconds). **Relaying `stream`** (a shape the relay was not
built, metered or tested for). **Buffering the clip to learn its length** (the clip is
megabytes, and the declared length is the upstream's own). **Storing the WAV in the
capture** (a corrupted, clipped fragment that bloats every capture). **Gating the verdict
writer on `server_agent` plus a spec kind** (it never opens for a static server, and the
agent router's listing states no `task`). **Normalising an already-OpenAI-shaped error**
(it would overwrite the upstream's attested type with the gateway's). **Proxying the
voices listing** (not an OpenAI endpoint; ADR-067 later added a cache-backed endpoint
instead of a proxy).

**Amended by [ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint)
(transcription, the third endpoint on the gate).** Speech's `openai_audio` flavor and `audio_cpp`
kind are now shared with `POST /v1/audio/transcriptions`, which changes three claims
above. (b): the flavor no longer implies the endpoint, so what differs between the two
is keyed on the **endpoint**, and (i) and (g) are carved the same way (a transcription
is normalised under `transcription.upstream_error` and billed from its response body, not
a header). (d): the same probe now writes **two** rows from one `task`, so an `asr` model
reads `(speech, no)` **and** `(transcription, yes)`; and the live probe that wrote
ADR-062 saw the listing name an **unloaded** model with `loaded: false`, so the claim that
it names loaded models only was not a measured property of the server. The parser never
read `loaded`, so nothing in the code changed. (h): the `audio/*` Content-Type gate is
unchanged, but the flavor-plus-2xx **backstop** behind it,
`isBinarySpeechResponse`, now excludes a transcription, whose 2xx body is a JSON
transcript the capture must keep.

**Amended by [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(the portal chat is now a speech caller).** (j)'s loopback leg has a caller: the portal
chat's `speech` run kind posts to this endpoint through the internal pair, with the
run-as header, and the WAV it gets back is stored as a chat blob. The endpoint, its
validation and its relay did not change. The Models listing now carries a `speech` flag
(`ModelDTO.Speech`, from the fold the unknown-model redirect already read), which the
chat's picker reads; the Models view's "Available via" column still does not, so the
Consequence's sentence about it stands. (j)'s decision not to proxy
`GET /v1/audio/voices` also stood at that point, and it was why the chat's `voice` field
was free text.

**Amended by [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)
(voices are served from a persistent cache; issue #251).** Both halves of that last sentence
are superseded. The gateway now answers `GET /v1/audio/voices` itself (and the portal has its
own session-cookie `GET /api/portal/model-voices`), reading the names the health loop cached
while the model was loaded rather than proxying the upstream, and the chat's `voice` field is
a discoverable combobox over them that still accepts free text. The upstream's behavior is
unchanged and is what shaped this: it lists voices only for its loaded model, so a name list
has to be kept, and an unknown voice still answers HTTP 500, which the endpoint relays like
any other speech error.

**Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)
(streaming speech synthesis; issues #253 and #263).** The streaming deferral of (e) is
lifted, and four neighbouring claims move with it. (e): `stream: true` is admitted and relayed
as audio.cpp's SSE or raw PCM for a mapping whose new `speech_streaming` verdict is `yes`
(the buffered one-shot answers every other mapping, and a retry-buffered net covers a stale
`yes`); while streaming, `response_format` must be `pcm` or absent and `wav` plus a stream is
still 400 `speech.stream_unsupported`; a request that does not stream still takes `wav` only.
(f): the upstream `Content-Length` is not forwarded for a streamed answer. (g): a streamed
call has no duration header, so it is billed from the PCM bytes the relay counted, in
`audio_second` over the mapping's cached output byte rate or in the new `audio_bytes` unit
when none is cached, on a clean completion only, and every failure row stays `(audio_second,
0)`. (h): the streamed capture stays a redacted summary, by an explicit endpoint stamp. (d):
the same probe now also writes the `speech_streaming` row, from `mode`. The accepted cost "no
streaming" in the Consequence is gone; "no `mp3`/`opus`/`flac`/`json`" stands.

**Amended by [ADR-069](#adr-069--non-stream-speech-pcm-is-a-wav-header-strip-not-a-transcode-the-gateway-relays-the-data-chunk-of-a-16-bit-mono-wav-as-headerless-pcm-refuses-any-other-clip-after-synthesis-and-leaves-mp3opusflacaac-to-264)
(non-stream `pcm`; issue #253 part 1).** A live re-verification on 2026-10-22 found audio.cpp
v0.9.1 still ignoring `response_format` on the binary path (every value answers a WAV), so (e)'s
refusals of the compressed formats stand and move to issue #264 (opt-in transcoding). One value
is lifted: a request that does not stream accepts `wav`, `pcm` or an absent `response_format`,
and `pcm` is the upstream WAV with its RIFF/WAVE header stripped, relayed as headerless
signed 16-bit little-endian mono PCM (`application/octet-stream`, `Content-Length` the data
chunk's, so (f)'s length is the data chunk's for this answer), refused with 502
`speech.pcm_unavailable` and billed `(audio_second, 0)` when the clip is not 16-bit mono PCM. (g)
is unchanged: the duration header still meters it. The strip is a container operation, not
re-encoding, so ADR-063/ADR-064's "never re-encodes" holds.

→ [API Compatibility & Inference §3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech),
[Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model),
[§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Security §14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-062 — Transcription is multipart in and JSON out on the shared `openai_audio` flavor: a distinct `transcription` capability, billing read from the response body, and every divergence from speech keyed on the endpoint

**Context:** `POST /v1/audio/transcriptions` (issue #69) is the last of the
originally paired multipart endpoints, after the image edit
([ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance))
and speech
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)).
It is the mirror image of speech on two axes at once. **Like speech** it takes the
capability gate, the coarse `openai_audio` flavor, the static `audio_cpp` application
type, the non-token `audio_second` unit of
[ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar) and no
affinity pin. **Like the image edit** its request is `multipart/form-data` (an audio
upload), so it rides the multipart request path: a model peeked from the form, a
bounded buffered body, a model rewrite when the provider's name differs. And it is
**unlike both** in one decisive way: the billable quantity, the input audio's
duration, is in the **JSON response body**, not in a header, so it is the first
endpoint whose relay must parse a response to meter the request. This entry records
what transcription decided *differently* from speech and from the edit, and why. The
inputs are facts about the backend, audio.cpp's `audiocpp_server` (v0.9.1, probed
live on 2026-10-16 with an `asr` model and a short WAV), that no design could have
chosen:

- Its capability signal is the per-model `task` of its own `GET /v1/models` (`asr`
  transcribes). That listing also named an **unloaded** `asr` model, with
  `loaded: false`.
- A success is `application/json`, `{"text": ..., "timing": {"wall_ms", "audio_duration_ms",
  "rtf"}}`, and `audio_duration_ms` is the input audio's duration. `timing` comes
  **after** the text, and there is **no `X-AudioCPP-*` header** on this endpoint.
- It **ignores `response_format`**: `json`, `verbose_json` and `text` all return the
  identical `{"text", "timing"}` document. It never produces OpenAI's `verbose_json`
  (no `segments`, no `language`) or plain text.
- Its errors are OpenAI-object-shaped but **HTTP 500** with `type: "server_error"`
  even for a client error (a missing file, an unknown model), and it has no
  authentication of its own. Its first call after a model load pays a warm-up of
  several seconds; a warm call is several times faster than real time.

**Decision — eleven choices, taken together.**

- **(a) The flavor is shared, so every divergence from speech is keyed on the
  endpoint, never on the flavor string.** Transcription reuses the coarse
  `openai_audio` flavor and the `audio_cpp` application type and runtime kind; it needs
  no flavor, type or transport of its own, and `FlavorsAreAudioOnly` already keeps an
  audio-only server off chat, warmer and benchmark traffic. The price is that code which
  tested `apiFlavor == apiFlavorSpeech` now sees two endpoints, and each such test was
  audited. Four behaviors differ and each is keyed on `endpoint == endpointTranscription`
  (`sessionEndpoint`): `upstreamPath` answers `/v1/audio/transcriptions` and **must do so
  before** the speech flavor branch; the **billing-quantity source** is the body, not
  the header; the **error-relay gate**, which is the speech normaliser for speech and
  its own for transcription; and the **capture predicate** (h). The carve-outs on the
  speech side are all written **negatively** (`setSpeechBillingQuantity`,
  `needsSpeechErrorRelay` and `isBinarySpeechResponse` each skip a transcription and
  nothing else), so a speech call site built without an endpoint, whose zero value is
  `endpointChat`, is still a speech call site. What stays flavor-keyed is right for
  both: `billingUnitFor` answers `audio_second` for the flavor, and `newUsageScanner`
  answers nil, since neither response has chat usage in it.
- **(b) `transcription` is a capability of its own, and the gate carries it
  unchanged.** `routing.CapabilityTranscription = "transcription"` means the model
  **transcribes** audio to text. It is neither `CapabilityAudio` (the model *accepts*
  audio as chat input, a llama.cpp `/props` modality with its own writer) nor
  `CapabilitySpeech` (the model *synthesises* speech); the three are orthogonal, can
  co-occur on one model and are never mapped onto each other. The handler sets
  `RequiredCapabilities = ["transcription"]` from its own identity, and ADR-042's three
  rules carry over: `filterCapable` refuses a model without a `yes` verdict on every
  resolve branch, **the affinity pin and the server override included**; an absent row
  is unknown and **refuses**; and `(transcription, no)` is reserved in
  `reservedManualVerdicts`, so a rank-3 manual `no` can never veto the probe that is
  entitled to write it. `(transcription, yes)` stays writable, and the name stays
  **off** `reservedAgentCapabilityNames`, because the agent reports it (issue #255), and
  an agent-reported `no` is legitimate. The list does its second job as it does for
  speech: it keeps the request off the vendor-account path, which has no `openai_audio`
  twin of its images exclusion.
  **The capability has no portal fold.** `hasCapabilityFold` admits `image`,
  `image_edit` and `speech` only, so `ModelOffering.Capable` is empty for a transcription
  request: the unknown-model redirect never takes a `LastUsedModel` or fallback for it
  (fail-closed, a plain unknown-model answer instead of a redirect), and the Models
  listing carries no transcription flag (speech has one, `ModelDTO.Speech`, since
  [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)).
  The gate itself is unaffected.
  **Amended by [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn):
  the capability now HAS a portal fold.** Issue #249 added `ModelDTO.Transcription` (the
  listing flag, `transcriptionFlagsByName`, speech's `ModelDTO.Speech` twin) **and** the
  unknown-model redirect candidacy (`hasCapabilityFold` and `capabilityFlagsByName` now
  admit `transcription`), so `ModelOffering.Capable` is no longer empty for a transcription
  request and the Models listing carries a `transcription` flag. The paragraph above
  records the as-landed ADR-062 state; the gate is still unaffected.
- **(c) One probe, one `task`, both capabilities.** The `audiocpp_models` source and
  `probeAudioCppModels` are unchanged in shape and now decide **two** rows from the one
  `task` they read: `asr` writes `(transcription, yes)` and `(speech, no)`; `tts` and
  `clon` write `(speech, yes)` and `(transcription, no)`; any other non-empty task
  writes both `no`, authoritative because the listing states each model's task; an
  absent task, a model the listing does not name or an unreadable body writes
  **nothing**, because none of those says a model cannot transcribe and a `no` can
  switch a route off. The prober decodes `task` only and never `loaded`: whether a model
  is resident right now is the **health** axis, not the capability axis, so an `asr`
  model the listing names with `loaded: false` still reads `(transcription, yes)` and
  an operator loads the mapped model. That sits against ADR-061 (d), which describes
  the listing as naming loaded models only, while the transcription probe saw an unloaded
  one named. Nothing in the parser depends on which holds (speech reads it the same
  way), but "a model that is not loaded reads as unknown" is a floor, not a guarantee,
  on that server version. **The agent-managed
  source exists, as for speech** (ADR-061 (d), issue #255): the agent reads an
  agent-launched `audio_cpp` child's own `/v1/models` and reports both rows under
  `audiocpp_models` while the gateway declares `capability_source_audiocpp`. The ingest
  accepts the source for `speech` and `transcription` only, which is why this name was
  left off `reservedAgentCapabilityNames`, and an operator's manual `transcription: yes`
  (rank 3) still outranks it.
- **(d) The request is multipart, rides the edit's path, and is not relayed purely
  verbatim.** The body is read whole under a **cap of its own**,
  `OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES` (default 32 MiB, the agent
  router's own cap; independent of the image-edit cap so an operator can size audio
  separately), then walked once with `mime/multipart` by the request's own boundary,
  never `ParseMultipartForm`, with every part read to its end. The **model is read
  exactly as the agent router's `peekMultipartModel` reads it**, a single-use part
  whose `FormName()` is `model` with the whole body walked, because the gateway
  allowlists the name it read and the router then starts a process on the name *it*
  read, and a parity test pins this parser to the image edit's. The handler validates
  its own shape **before** the preflight that makes the one admission call: a non-empty
  `model` (`transcription.model_required`) and **exactly one non-empty part named
  `file`**, zero being `transcription.file_required` and more than one
  `transcription.invalid`, a malformed or non-multipart body also being
  `transcription.invalid`. Only `file` counts: audio.cpp reads the upload from that
  field alone, so the image edit's `image[]` spelling has no twin. A JSON body, which
  audio.cpp itself accepts, is refused: OpenAI's contract is multipart. **An empty
  `file` part is no file for validation but is stripped before the relay**: audio.cpp,
  handed two `file` parts, could take the empty one, so the relay rebuilds the body
  (every part's headers, bytes and position copied, a fresh boundary, the `model` value
  replaced when the mapping's own name differs, every empty `file` part dropped) whenever
  the model needs a rewrite **or** an empty `file` part is present. The client's bytes
  are relayed verbatim, boundary included, only when neither holds. An
  agent-launched target is sent the upload only when its agent has declared
  `multipart_proxy` (fail-closed, 404 `transcription.unavailable`, **no agent version
  bump** because the router is blind to the path), and the body actually handed to it is
  re-checked against the agent's own 32 MiB **after** the rebuild (413
  `request.body_too_large`, recorded, not sent).
- **(e) Transcription bills `audio_second` from the response body.** The unit is endpoint
  identity, so every `recordUsage` site, a refused or failed request included, carries
  `billing_unit = audio_second` with tokens and both rates at zero
  (`ValidateBillingXOR`). The quantity is the **response's own measurement**,
  `timing.audio_duration_ms` divided by 1000, which is where audio.cpp reports it on
  this endpoint, **not a header** as speech's is. It is set only on a clean `success`,
  once the full body has reached the client, so a client that disconnects mid-write
  records `0`, as speech's does. A success with **no usable duration** (an absent
  `timing` or field, a `null`, a non-number, a numeric **string** included, zero, a
  negative, a value past float range, or a body that is no JSON object) records `0` and
  is logged once at `Error`, because the pair has no "unknown" and the gap is made
  loud, not repaired (ADR-041). The log line carries the body's **size** only: the
  body is the transcript, which is the user's content.
  **Amended by [ADR-065](#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript):
  a STREAMED call bills from the WAV header instead.** A streamed response carries no
  `timing.audio_duration_ms` (audio.cpp's `transcript.text.done` has only `ttft_ms`), so
  issue #262 bills a streamed call from the **uploaded WAV header** (the `data` chunk bytes
  over the byte-rate), set only on a clean `transcript.text.done` + `[DONE]` completion and
  a loud zero otherwise, with a forged-header consistency guard. The **non-streaming path is
  unchanged** — still the response `timing.audio_duration_ms` the paragraph above records.
- **(f) The relay buffers the response up to the body cap, writes it verbatim, and an
  over-cap response is a 502.** `timing` is trailing, so the duration is unknown until
  the body has been read to its end, and the gateway must not have sent a byte by then,
  or an unreadable or oversized body could not be turned into an error. So the one
  success in `proxyNative` that cannot stream goes **read, decide, write**: it reads up
  to the transcription cap through a `LimitReader` of `limit + 1`, so that exceeding it is
  **detected rather than clipped**; then the buffered bytes go to the client verbatim,
  with the upstream's status and `Content-Type`, an exact `Content-Length` and the
  `timing` block **kept** (the project proxies natively shaped backends and does not
  translate; OpenAI clients ignore the unknown field). A response **over the cap**, or
  one that **breaks off mid-read**, is HTTP **502** `transcription.upstream_error` (the
  gateway's own OpenAI-shaped error, `type: upstream_error`, there being no upstream
  status worth relaying): nothing of the body is written, the row is an error row with
  quantity `0`, and a transcript is **never delivered in part**. The bound is the body
  cap, **not** `captureMaxBytes`, which is for error bodies and would hand the client a
  truncated success. 32 MiB is orders of magnitude above any real transcript (a 32 MiB
  upload transcribes to a few MB of text at most), and memory under concurrency is
  bounded by that cap per in-flight request, which is the price of metering a trailing
  field. There is no idle watchdog and no usage scanner: the request is one
  non-streaming call under the application's `timeout_ms` as its single **total**
  deadline.
- **(g) The request surface takes `json` only, and streaming is refused.**
  `response_format` must be `json` or absent (whitespace trimmed, the match
  case-sensitive as OpenAI's is), and **every other value is 400
  `transcription.response_format_unsupported`**: `verbose_json`, `text`, `srt`, `vtt`
  and anything that is no transcription format at all. audio.cpp only ever produces its
  own `{"text", "timing"}` document, so `verbose_json` would promise `segments` and a
  `language` it does not produce, and the plain-text formats would promise a non-JSON
  body that carries no duration, so a relayed one would bill zero on every success. It is
  the tight mirror of speech's WAV-only surface, with the same way back: a real
  `verbose_json` shape, or honoring `text`, `srt` and `vtt`, needs a translation this
  project does not do. `stream` on, a non-boolean `stream` and any `stream_format` are
  400 `transcription.stream_unsupported`, because the relay's buffering and billing
  contract is one complete JSON response; `stream: false` and an absent or empty `stream`
  and `stream_format` are accepted.
  **Amended by [ADR-065](#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript):
  `stream=true` is now RELAYED for a streaming model.** Issue #262 lifted the `stream=true`
  refusal: when the mapped model's `transcription_streaming` verdict is `yes` (the model is
  `mode=streaming`) the gateway relays audio.cpp's SSE unbuffered. A stale `yes` that draws
  the `500 "requires a model configured with mode=streaming"` is retried **once as a
  buffered one-shot** (with `stream` dropped), not re-streamed, and an absent/`no` verdict
  buffers from the start. The refusal code `transcription.stream_unsupported` now fires
  **only** for a non-boolean `stream` or any `stream_format`; `stream: false`, absent and
  empty keep the buffered one-shot above. The paragraph records the as-landed ADR-062 state.
- **(h) Capture stores the form and the transcript, and the binary predicate is
  endpoint-gated.** The multipart **request** is captured by the edit's path as a JSON
  summary: the text fields kept, the `file` part `redacted`. The **response** is the JSON
  transcript, stored as an ordinary text body under the opt-in, and it is
  **output-sensitive**: it is the user's spoken content, and no transcription-specific
  redaction applies to it. It gets what every captured body gets: the capture's own
  per-body size cap (a long transcript is stored clipped, with `truncated` set, while the
  client still receives all of it) and the same sealing at rest
  ([Security §14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers)).
  The flavor was the wrong key here: `isBinarySpeechResponse` stored **every** 2xx
  `openai_audio` response as a redacted binary summary, which would have turned a
  transcript into a metadata stub, so `captureInput` gained an `Endpoint`, stamped by the
  transcription success relay, and the predicate skips `endpointTranscription`. The
  carve-out is negative, so a speech capture built without an endpoint stays redacted:
  the failure that leaves is a transcript lost to a summary, never a WAV in the envelope.
  The capture's text-part bound must stay at least each endpoint's own field cap (both
  1 MiB), or a field a handler accepted would be redacted.
- **(i) A non-2xx upstream response is normalised to the OpenAI error shape.** As
  ADR-061 (i), keyed on the endpoint: the error body is buffered, bounded by
  `captureMaxBytes`; an OpenAI error object passes through **byte for byte** (audio.cpp's
  attested `type` is not overwritten); a plain `{"error":"..."}` string is wrapped with
  `type: upstream_error` and `code: transcription.upstream_error`; anything else is
  relayed unchanged under its own `Content-Type`; the upstream's status is relayed as it
  came, and a failed relay records an `audio_second` error row with quantity zero. The
  500 that audio.cpp answers for a client error is why validation (d) and (g) run up
  front rather than leaving the client with a "server failure" about its own mistake.
- **(j) The admission surface is the image and speech one.** Auth is
  `requireInternalOrBearerAnyScope`: a bearer token **or** the internal loopback pair,
  never the session cookie (a fourth user of that leg, which at the time of ADR-062 had no
  portal caller: the portal chat's speech run calls `/v1/audio/speech` over the same leg,
  [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs),
  while transcription was API-only, (k) — **[ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn)
  since gave it a portal loopback caller**), with the shared run-as block. The request takes **no affinity pin**, by the
  guard ADR-042 wrote for exactly this, and has an empty `sessionEndpoint` case (OpenAI's
  transcription request has no session field, and its body is multipart). The handler lifts
  the connection deadlines, since an upload can take longer than 30 seconds to arrive.
- **(k) Transcription is API-only in the portal, with a chip and a mock.** The operator
  portal gains a `transcription` capability chip (a `KNOWN_CAPABILITY_ORDER` entry and a
  DE/EN string), so a `transcription: yes` verdict renders as a translated chip instead of
  a raw standby string, and there is no transcription composer (the portal chat's speech
  composer is [ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)'s, and transcription in the chat is deferred). The built-in mock provider answers the
  transcription path with the `{text, timing}` document, so a dev or e2e run is not handed
  a Responses event stream.
  **Amended by [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn):
  transcription is no longer API-only in the portal.** Issue #249 added the transcription
  composer (the fourth run kind), so the chat now reaches this endpoint over loopback; the
  capability chip and the mock are unchanged.

**Consequence:** transcription has speech's day-one cost. A deployment with no
`openai_audio` application serves no transcription: a route without the flavor answers
404 `routing.no_model_route`, and one with it and no `(transcription, yes)` verdict
answers 404 `routing.model_not_capable`. For an `audio_cpp` application the verdict
arrives with the first health tick that finds the mapped model named in the listing. Every
request is one non-streaming call, so a first call after a model load can occupy several
seconds of warm-up inside the application's `timeout_ms` (600000 by default for the
`audio_cpp` type; any other type that serves transcription needs it raised by hand). The
memory profile is the new cost: a transcription holds its **upload** whole (up to 32 MiB,
and a second copy while a rebuild is in flight) and, separately, its **response** up to
the same cap, per in-flight request. The costs accepted are named, not hidden
([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)): no
streaming, no `response_format` but `json`, bounded buffering of the upload **and** the
response, an unlisted model reading as unknown (an agent-launched child's verdict
arrives from the agent's probe of its own listing, once per process generation), no
portal fold (so no redirect candidate and no listing flag — **reversed by [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn)**, which added both for the portal chat), a capture gate that trusts
the endpoint, and a captured transcript that is output-sensitive.

**Rejected:** **A fine flavor of its own** (ADR-042's and ADR-061's argument, now three
times avoided; the shared flavor costs one rule, to key on the endpoint, and an audit).
**Mapping `transcription` onto `audio` or `speech`** (an input modality with its own
writer, and the opposite direction; advertising one as the other fails far from its
cause). **Billing from the request** (decoding every container format to learn a
duration the upstream has already measured, and a number that need not agree with the
one it computes). **Streaming the response while scanning for `timing`** (the field is
trailing, so by the time it is known the status and the transcript have gone, and an
unreadable or oversized body can no longer become an error). **Clipping the buffer at
`captureMaxBytes`** (a silently truncated success, billed at zero). **Honoring `text`,
`srt`, `vtt` or `verbose_json`** (a shape the upstream does not produce, and a billing
gap for the non-JSON ones). **Accepting `file[]`, or several files** (audio.cpp reads
`file` alone, and a form with two audio files is two answers to "which audio").
**Relaying an empty `file` part** (the upstream could read it instead of the upload).
**Passing audio.cpp's 500 for a client error through** (the gateway would hand a client's
own mistake on as a server failure). **Normalising an already OpenAI-shaped error** (it
would overwrite the upstream's attested type). **A flavor-keyed capture predicate** (a
transcript stored as a redacted summary, with the opt-in's whole point lost). **Sharing
the image edit's body cap** (audio and images are sized by different operators for
different reasons). **Gating the verdict writer on `server_agent`** (as ADR-061 rejected
it). **Serving a JSON-bodied request** (audio.cpp accepts it; OpenAI's contract is
multipart, and a second body shape doubles the validation and capture surface).
→ [API Compatibility & Inference §3.7](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions),
[§10](cross-cutting/compatibility-and-inference.md#10-request-bodies-and-size-limits),
[Routing & Model Selection §1](cross-cutting/routing-and-model-selection.md#1-data-model),
[§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Security §14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[Agent-Managed Model Runtime
§3.4](cross-cutting/agent-runtime-manager.md#34-runtime-server-kind-and-per-kind-probe-path-derivation),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Configuration Reference](reference/config-env.md#encrypted-payload-capture),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-063 — Speech in the portal chat is a third run kind: the model is the affordance, the run reuses the speech endpoint over loopback, and the WAV lives in `chat_blobs`
**Context:** `POST /v1/audio/speech`
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only),
issue #68) was built as an API and, deliberately, so that the portal chat could call
it: its auth leg is the internal trusted-loopback pair, never the session cookie
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)
(j), the leg [ADR-006](#adr-006--two-auth-modes-chat-completions-also-session-reachable)
split off for images). Nothing in the portal used it, so an operator with a
text-to-speech model could not hear it without writing a client. Issue #248 closes
that: pick a TTS model in the chat, type text, and get a **playable, downloadable
audio reply**. The feature is overwhelmingly a mirror of the image turn
([ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread))
and of the blob store
([ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url)),
re-pointed at the audio endpoint, so this entry records only where speech had to
decide *differently*, and why. Four facts about the pieces it joins that no design
could have chosen:

- `/v1/audio/speech` answers **one buffered, binary `audio/wav` body** and refuses a
  streamed request, so a speech turn, like an image turn, emits **zero** incremental
  events across a wait of seconds (the first synthesis after a model load pays a
  warm-up).
- audio.cpp's voice names are **not discoverable through the gateway**: ADR-061 (j)
  does not proxy `GET /v1/audio/voices` (an audio.cpp and llama-swap convention, not an
  OpenAI endpoint), and the agent router has no voices route. audio.cpp also has no
  capabilities endpoint (the analogue of `/sdcpp/v1/capabilities` answers 404), so its
  native knobs (a kokoro model's `seed`, `language`, `phonemes`, ...) cannot be learned
  at runtime either. (**The voice half is superseded by
  [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)**:
  voice names are now cached while the model is loaded and served by the gateway, and the
  agent router has a voices route. The native-knobs half stands.)
- The blob-serving endpoint answers **no `Range` request**: it writes the decrypted
  body with a `Content-Length` and does not go through `http.ServeContent`
  ([ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url)
  (e), a handler locked down on purpose). Safari and iOS will likely not play audio from
  a URL that cannot be range-read (WebKit's known behaviour, not measured here).
- The `speech` verdict is the verdict of audio.cpp's `tts` **and** `clon` tasks
  (ADR-061 (d)), and a clone model needs a reference voice a text-only chat cannot
  supply.

**Decision — eight choices, taken together.**

- **(a) `speech` is a third run kind, and the picked model is its only affordance.**
  `portal.ChatRunSettings.Kind` gains `speech` beside `""` (text) and `image`, and
  everything [ADR-043](#adr-043--the-portal-image-turn-the-model-is-the-affordance-the-kind-is-pinned-to-the-thread)
  (a) and (b) decided for the image kind carries over unchanged: no mode toggle and no
  "speak" button; the kind is established by the thread's first send, forced back on
  every later one by `PrepareChatRun` (the stored-kind lift and the has-the-thread-been-sent
  signal, never a `stored.Kind != ""` guard), and **a UI constraint, not an
  authorization**: a thread pinned to `speech` whose mapping loses its `(speech, yes)`
  verdict fails the capability gate exactly as an unpinned request would, and
  `filterCapable` stays the only authority
  ([Routing & Model Selection §2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).
  The speech knobs are a `ChatAudioSettings` (`voice`, `speed`, and the passthrough
  of (g)) on `ChatRunSettings.Audio`, declared **after** `Image` and `omitempty`, and
  `PrepareChatRun` clears it for every other kind, so a text or image chat's stored
  and wire bytes are unchanged, even against a raw API client. The settings are **not**
  force-pinned: the composer sends the thread's current voice and speed on every send, so
  they can change between turns. A speech thread takes the **text** branch of
  run-start history resolution, which looks at image parts only, so a stored WAV is
  never fetched, unsealed or counted against `MaxRunHistoryBytes`. The kind is
  surfaced like the image kind's (`kind` on the run's `201`, the active-runs rows and
  the `snapshot`/`done` events), and the browser picks the speech wait state from the
  **run's own** reported kind, never from the thread guess, for the reason ADR-043 gives.
  A model that is both `image` and `speech` capable is an image model (image was the
  kind first, so a model that gains a speech verdict never changes what an existing
  image model does); no catalogue entry is both today.
- **(b) The executor reuses `POST /v1/audio/speech` over loopback; there is no second
  audio path, and nothing on the endpoint changed.** `executeAudioRun`
  (`chat_runs_audio.go`) is the image executor's near twin and shares with the text
  half exactly what the image half shares: the reservation, the deadline, the loopback
  header set (`setRunLoopbackHeaders`: the internal pair, the chat-id session header
  and the run-as header) and the one terminal commit
  (`finishRunWithParts`). It cannot reuse the stream consumer, for ADR-043's reason:
  that assumes deltas and a checkpoint, and there is nothing to checkpoint. The body is
  `model` (the thread's pinned model), `input` (the **last user message's text**) and
  whichever of `voice` and `speed` the thread set, plus the passthrough of (g);
  `response_format` is **not stated**, because the endpoint answers WAV when it is
  absent and refuses every other value (ADR-061 (e)), so there is nothing to choose and
  nothing for an unset field to default to. Auth, run-as billing and capture are the
  loopback path's, unchanged: the call bills `audio_second` under the run-as token and,
  where the token or the system opts in to capture, stores the request JSON and a
  redacted summary of the audio, never the WAV
  ([Telemetry §8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
  [Security §14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers)).
  The run is bounded like an image run (`runDeadlineFor` gives the speech kind
  `speechRunDeadline`, 10 minutes, a variable of its own so the two promises can move
  independently), a firing deadline is `gateway.chat_run_timeout` and never a cancel,
  and the terminal commit keeps its fresh 30 s bound (ADR-043 (e), which this decision amends).
  **The response is read whole and checked, never assumed.** It is not bounded by
  `captureMaxBytes` (an ordinary clip exceeds 1 MiB; the store's `MaxChatBlobBytes`
  refusing the commit is the backstop, as for images). A 2xx with an empty body is
  `gateway.chat_run_no_audio` (an error, not an empty success: a turn with no audio
  would render as a blank bubble); a 2xx whose body cannot be read to its end, or whose
  `Content-Type` is not a WAV label (`audio/wav`, `audio/x-wav`, `audio/wave`,
  `audio/vnd.wave`, parameters ignored), is
  `gateway.chat_run_speech_response_unreadable`, because the media type is the one
  fact about the audio that must come from the response and a default would stay
  stated in the transcript and in every download; a loopback hop that never completed
  is `gateway.chat_run_speech_dispatch_failed` (the Go error names the gateway's own
  address, so it goes to the log); and a non-2xx surfaces the endpoint's own code
  through `upstreamErrorCode` (`speech.input_required`, `speech.upstream_error`, ...).
  Every one of them, and the endpoint's own `speech.*` refusals, has a German and an
  English label in the portal (`errorLabelByCode`), pinned by exhaustiveness tests in
  both directions.
- **(c) The reply is one `audio_url` part, and the WAV is moved into `chat_blobs` like an
  image.** The executor commits exactly one part,
  `{"type":"audio_url","audio_url":{"url":"data:audio/wav;base64,…"}}`, `image_url`'s
  twin (same nesting, `type` first), so the extraction and the renderer each learn one
  more part type rather than a new document shape. `image_url` itself could not be
  reused, as ADR-056 (b) did for an uploaded image: a WAV filed under it would be read
  by every consumer as a picture, and a run's history would send audio upstream as vision
  input. `extractChatBlobs` now walks both kinds (`blobPartKind`), and **each kind accepts
  only its own media family**: an image part takes `image/*` and an audio part takes
  `audio/wav`, so neither can be filed under the other. `chatBlobMediaTypes`, the
  allow-list that is ADR-056 (e)'s stored-XSS control, gains exactly one entry,
  `audio/wav`, and the executor normalises every WAV alias to that one string so the
  store and the renderer key on a single value (it grows deliberately, per consumer, as
  ADR-056 (a) said it must). The history resolution stays **image-only**
  (`resolveChatBlobRefs`, `usableChatBlobs`), and `usableChatBlobs` now additionally
  requires an image type for an image reference: without it an `image_url` reference
  naming a WAV blob would have resolved to `data:audio/wav` inside an image part.
  (Amended by [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn):
  the history resolution is **no longer image-only**. A transcription run resolves its own
  **audio** input — the resolve walks were generalised to take a `blobPartKind` and a new
  `resolveTranscriptionRunSource` resolves the last user message's clip — so "reads image
  parts alone" holds for a text, image or speech run, not for a transcription one.) This
  is the "allow-list entry, a part shape the extract and resolve walk understands, and a
  renderer" ADR-056 (a) listed as everything an audio consumer would need and the
  statement that **no new storage layer** would. There is **no schema change**: the store
  is payload-type-agnostic (bytes plus a content type), so the migration, the cascade, the
  sealing and the memory budget are exactly what they were. The serving endpoint changes
  only the allow-list its read-time re-check consults; its headers (`nosniff`,
  `Content-Security-Policy: default-src 'none'`, `Content-Disposition: inline`,
  `Cache-Control: private, no-store`) and its owner scoping are untouched. A WAV the
  store refuses at the assistant commit (too large, or a type off the list) ends the run
  as `gateway.chat_run_commit_failed`, as an image does.
- **(d) Playback fetches the blob into an object URL.** `AudioTurn` renders an HTML5
  `<audio controls>` and a download button, but does **not** hand the gateway URL to the
  element: the endpoint answers no `Range` request, and Safari and iOS will likely not play
  from a URL that cannot be range-read (WebKit's known behaviour, not measured here). It fetches the stored URL **once**, same-origin
  with the session cookie (the endpoint is owner-scoped), and plays an object URL made
  from the bytes. An object URL pins the whole WAV in memory until it is revoked and a
  transcript can hold many audio turns, so the effect's cleanup **revokes it** when the turn
  unmounts or the URL changes, and aborts a request still in flight (a response that
  lands after the abort creates nothing, since nothing would ever revoke it). A run's
  own terminal event carries the part as it was produced (the inline `data:audio/wav`
  URL), which the element plays directly until the post-`done` refetch swaps in the
  stored URL; any other shape is refused inline rather than fetched. Download goes
  through `downloadBinary`, a second fetch on click only, so the saved file's type
  and extension come from what the server actually served; its extension map is now keyed
  on the **whole media type** (`image/png`, `image/jpeg`, `image/webp`, `image/gif`,
  `audio/wav`), so a known subtype under the wrong top-level type borrows nothing, and
  the file is named from the turn's own id, as an image's is. The wait is
  `AudioPendingTurn`, the image wait's twin over a shared `RunPendingTurn`: liveness from
  the run's own status, a **server-measured** elapsed clock and one static sentence that no
  intermediate news is coming, with **no** progress figure, for ADR-043 (d)'s reason.
  **Hardening note, not a defect:** the playback fetch relies on the same
  `isFetchableUrl` guard the image download uses (`downloadBinary.ts`, now exported), and
  that guard is a **literal-prefix** check, `url.startsWith('/api/portal/chat-blobs/')`,
  on a URL that comes out of a persisted transcript. It names no host (`//host/x`,
  `/\host/x` and every absolute URL fail the prefix), but it does not normalise path
  segments, so a stored `/api/portal/chat-blobs/../…` would be resolved by the browser to
  another path **on the same origin**. That is still a relative, same-origin `GET` with
  the user's own cookie, planted only by someone who can already write that user's own
  transcript, and its response goes to an object URL that is played or saved under an
  allow-listed extension: not SSRF and not a cross-user read. Tightening it to the exact
  blob-id shape is a cheap, deferred hardening, and no code changes here.
- **(e) The chat's picker widens, and the shared offering view does not.** The Models
  listing carries a **`speech` flag** on `ModelDTO` (`/api/portal/models`, always
  present), the one fail-closed fold the unknown-model redirect already read
  (`speechFlagsByName`): every offering mapping holds a `(speech, yes)` verdict **and**
  declares `openai_audio` on its application and, for an agent-launched model with a
  spec, on the spec; AND-ed across a model's mappings and a group's offerable members,
  inherited by an alias, `false` for every model when the capability read fails. It is
  not on `/v1/models` or the LM Studio listing. The chat's own filter
  (`chatModels` in `ChatStore.tsx`) admits an `openai_audio` model **only** behind
  `speech: true` (since [ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn), also behind `transcription: true`),
  the polarity `openai_images` has behind `image`, and the effective
  model's flag selects the speech composer for a thread that has no transcript yet.
  Since issue #252 the speech half is `speech: true` **and not**
  `speech_clone: true`: a voice-clone model is dropped from the picker (choice (h),
  the refinement that reversed it), while the transcription half is unchanged.
  **The shared `offeredFlavors` is deliberately untouched.** It also feeds the Models
  view's "Available via" column, and gating `openai_audio` behind a speech verdict there
  would drop the flavor from transcription, unprobed and not-yet-reported agent-managed
  audio rows (and from the image-worded "no verdict" notes that read it). The chat needs
  a stricter rule than that column, so the stricter rule lives in the chat and the
  shared function keeps answering what it did. An agent-managed `audio_cpp` model appears in the chat once its
  agent has reported a `(speech, yes)` verdict from the child's own listing (ADR-061 (d),
  agent 0.9.1) or an operator has set `speech: yes` by hand.
- **(f) `voice` was free text with no voices dropdown; it is now a combobox. (AMENDED by
  [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter),
  issue #251 — see the amendment below.)** The settings panel
  carries a `voice` field whose placeholder hints that the model's own default
  applies and is **never written** (blank leaves the model on its default voice, the
  rule the image defaults follow), and a `speed` number field (placeholder `1.0`, passed
  through as given: the model's own range applies and the composer does not clamp
  it). There is no `response_format` control, only a static caption that the reply is
  always WAV (the endpoint's WAV-only surface, ADR-061 (e)), and no image attach (the
  attach mode of a speech thread is `none`). As first shipped the field was plain free
  text and there was no dropdown, by a chain of two reversals: discovering voices needed
  the gateway to proxy `/v1/audio/voices`, which ADR-061 (j) refused on purpose, **and**
  the agent router to grow a voices route, which would likely be an agent change with its
  own feature flag and version bump. Each was a decision of its own, so a discoverable voice
  list was deferred to its own ADR, and the free-text field was the approved first step. No
  `/api/portal/model-audio-options` endpoint exists: the voice list has its own route (below)
  and the other speech knobs have nothing to put in one.

  **Amendment (issue #251, ADR-067):** both reversals were taken. The gateway now serves
  the cached voices and the agent router has the `runtime_voices` route, so the field is a
  **combobox**: a dropdown of the voices cached for the picked model that **also accepts
  free text** (`SpeechVoiceField.tsx`, an MUI `Autocomplete` in `freeSolo` mode), still bound
  to the existing `voice` field of `ChatAudioSettings` (the field audio.cpp consumes, so
  nothing on the run path changed). The names come from the session-cookie
  `GET /api/portal/model-voices?name=`, fetched on pick and cleared on every model change,
  and the field is the plain free-text field again whenever there is no list (none cached,
  an unknown or not-visible model, a failed fetch): a list is an offer, never a
  constraint. The model's-default placeholder and the never-write-a-blank rule are unchanged.
- **(g) The Advanced section is a JSON passthrough, guarded against the fields the run
  owns.** A collapsible "Advanced" holds one text box for a **JSON object**; every key is
  merged into the request body as a top-level field, verbatim (`map[string]json.RawMessage`,
  so a string, number, bool or object reaches audio.cpp with the type the user wrote),
  which is how the OpenAI `instructions` field and audio.cpp's native knobs travel. It is
  not an `<sd_cpp_extra_args>` block: that is sd-server's convention for a prompt that
  has nowhere else to carry a knob, and speech is JSON in. `model`, `input`, `stream`
  and `response_format` are **reserved**, matched ignoring case (the readers of the body
  decode that way): a key that names one fails the run with
  `gateway.chat_run_speech_reserved_field` and **sends nothing**, since forwarding the
  rest without the offending key would answer a request the user did not make. The
  guard turns what would be an opaque upstream refusal, or an override of the run's own
  value, into one that names its cause. `stream_format` is not reserved, because the
  endpoint already refuses it with `speech.stream_unsupported`. A key that repeats a
  first-class setting that **is** set (`voice`, `speed`) is not sent twice: the visible
  control wins, and with it blank the passthrough key is the only statement of it and
  goes through. On the client, text that is not a JSON object, or that names a reserved
  key, blocks both Send and a replay (regenerate, edit a message), with the Advanced
  section held open and the error inline: the first because `audioSetting` would
  otherwise omit it from the request in silence, the second so that a turn is not
  committed only to fail. The reserved-key list is therefore **copied** to the client
  (`SPEECH_RESERVED_FIELDS` in `chatDoc.ts`, matched ignoring case, the inline error
  naming the offending field), and the backend check stays the authority for every other
  caller and the backstop should the two lists ever drift.
- **(h) Clone-task models are offered and may fail. (RESOLVED by issue #252 — see the
  amendment below.)** The `speech` verdict does not
  distinguish `tts` from `clon` (ADR-061 (d)) and the model DTO carries no task, so a clone
  model carries the flag and appears in the chat. Used text-only it has no reference
  voice, so it can answer an upstream error, which the run surfaces as the endpoint's
  own code. Excluding clone models would need a new per-mapping fact the verdict does not
  hold. Recorded as an accepted limitation
  ([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).

  **Amendment (issue #252):** the per-mapping fact now exists. A new NON-ROUTING
  refinement capability `speech_clone` (`routing.CapabilitySpeechClone`, the twin of
  `transcription_streaming`) is derived from the same audio.cpp task (`clon` → yes,
  `tts` → no, nothing for a non-speech model), written by both producers of the
  `audiocpp_models` source (the gateway health loop and the agent, which bumps to 0.10.1,
  a PATCH — `audioCppProbeCapabilities` gains the fourth name), and folded into
  `ModelDTO.SpeechClone` (`speechCloneFlagsByName`, the fail-OPEN OR inverse of the speech
  fold). The chat picker's speech half is now `speech && !speech_clone`, so a clone model
  is no longer offered. `speech` is unchanged, so the direct `/v1/audio/speech` endpoint
  still serves a clone model to a bearer client that can supply a reference voice; only
  the chat affordance is withheld, and `(speech_clone, no)` is reserved as a manual verdict
  so an operator cannot permanently force-offer a clone model. This REVERSES the "A
  clone-model filter" rejection below. (Option 2 — a reference-voice attach/record control
  in the composer — remains out of scope, overlapping the transcription composer's
  audio-input work.)

**Consequence:** a speech turn has the image turn's costs in a milder form. It is one
buffered request with no progress to show, so the wait is the elapsed clock and an
honest "no intermediate news"; the first synthesis after a model load can take long
enough to matter inside the 10-minute bound. The WAV is uncompressed, so a long text can
produce a payload near `MaxChatBlobBytes` (20 MiB by default), and the commit of one over
it fails the run as `gateway.chat_run_commit_failed`; the executor holds each in-flight
run's whole response in memory, and the browser holds one object URL per audio turn it
has rendered until it unmounts. Stored WAVs are retained for the chat's lifetime like
every blob, with no prune (ADR-056). The gateway never converts or re-encodes: the reply
is WAV because the endpoint serves only WAV, and mp3, streaming and per-model native
controls need the translation, streaming contract or discovery this project has not
built. Nothing else moved: no change to `handleOpenAISpeech` or the relay, no schema
change, no migration, no new auth leg, and no ServerAgent version bump. The portal
side is gateway and frontend only.

**Rejected:** **A mode toggle or a "speak" button** (ADR-043's argument, the composer
could then offer what the gate refuses). **Widening the shared `offeredFlavors`** (it
would regress the Models view for every audio row without a speech verdict; the chat's
stricter rule belongs in the chat). **A voices dropdown now** (it needed the gateway to
proxy a listing ADR-061 (j) refused and an agent route that did not exist — **reversed by
issue #251 (choice (f) amendment, ADR-067): both now exist and the dropdown is the
combobox**), and **a `/api/portal/model-audio-options` endpoint** (nothing to serve). **Reusing `image_url`
for the WAV** (every consumer would read audio as a picture). **A clone-model filter**
(the verdict does not distinguish the task — **reversed by issue #252 (choice (h)
amendment): the `speech_clone` refinement now distinguishes it, so the filter exists**). **Playing the blob URL directly** (no
`Range`, so Safari and iOS would likely not play it) and **making the blob handler
range-capable** (a change to a handler locked down on purpose, for one consumer; an
object URL costs the player a fetch it makes anyway). **Stating `response_format`**
(the endpoint answers WAV without it and refuses every other value). **An
`<sd_cpp_extra_args>`-style tag** (a prompt-embedded convention for a different
upstream; speech is JSON in). **Dropping a reserved passthrough key and sending the
rest** (a request the user did not make). **Storing the WAV inline in the transcript**
(ADR-056 retired that for images, and a WAV is larger). **A second blob endpoint or
table for audio** (the store is payload-type-agnostic).

**Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)
(streaming speech synthesis; issues #253 and #263).** Two of this entry's claims no longer
hold unconditionally. The Context's first fact and the Consequence's "one buffered request with
no progress to show" are now the **buffered** run: for a model the gateway can stream (its
`speech_streaming` verdict is `yes` for every mapping the model could route to, each with a
cached output byte rate, and the thread has not opted out) the run asks the same endpoint for a
stream, plays the audio in the browser as it arrives through a new `audio` run event, and
commits the same single WAV turn at the end; anything else is the run described above,
unchanged. And (g)'s reserved passthrough fields gain `stream_format`, because the run states
`stream` and `stream_format` itself when it streams (the endpoint no longer refuses a streamed
request, so the earlier reasoning that `stream_format` need not be reserved is gone). The
run's 10-minute deadline bounds a streaming run only until its first chunk of audio, as for a
streaming transcription. A new session-cookie `GET /api/portal/model-speech-streaming` is the
composer's gate.

→ [API Compatibility & Inference
§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[§3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech),
[§9](cross-cutting/compatibility-and-inference.md#9-model-discovery),
[Runtime View §6.1.3](06-runtime-view.md#613-portal-speech-chat-turn-through-loopback-to-a-chat-blob),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Security, Authentication & Authorization
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance),
[§13.1](cross-cutting/security-auth-rbac.md#131-chat-binary-payloads-a-sealed-store-ownership-on-read-a-hardened-serving-url),
[Data Model](reference/data-model.md#portal-chat),
[HTTP API Surface](reference/api-surface.md#tokens-chats-usage),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-064 — Transcription in the portal chat is the fourth run kind: the model is the affordance, the audio input lives in `chat_blobs` and is relayed multipart over loopback, and the reply is the ordinary text turn
**Context:** `POST /v1/audio/transcriptions`
([ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint),
issue #69) was built as an API and, like the speech endpoint before it, so that the
portal chat could call it: its auth leg is the internal trusted-loopback pair, never the
session cookie (ADR-062 (j), the leg [ADR-006](#adr-006--two-auth-modes-chat-completions-also-session-reachable)
split off for images). Nothing in the portal used it — ADR-062 (k) deferred the composer
on purpose — so an operator with a transcription (ASR) model could not use it without
writing a client. Issue #249 closes that: pick a transcription model in the chat, attach
an audio file **or record audio in the browser**, and get the **transcript back as a
plain text reply**. The feature is overwhelmingly a mirror of two existing features at
once: the speech turn
([ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs))
for the kind / settings / picker / offering spine, and the image **edit** turn
([ADR-057](#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance))
for the multipart request and the stored-input-resolved-before-the-run spine. But it
**inverts the speech data flow**: speech is text IN → a WAV OUT (the audio is the
*output*, in `chat_blobs`,
[ADR-056](#adr-056--chat-binary-payloads-live-in-an-owner-scoped-sealed-chat_blobs-store-and-the-transcript-references-them-by-gateway-url));
transcription is audio IN (the clip is the *input*, in `chat_blobs`, exactly like an
image-edit source) → text OUT (the ordinary text turn, no new part). This entry records
only where transcription had to decide *differently*, and why. Four facts about the
pieces it joins that no design could have chosen:

- `/v1/audio/transcriptions` is **multipart in** (the clip in a single `file` part) and
  answers **one buffered JSON `{text, timing}` document**, refusing any `response_format`
  but `json`/absent and refusing a stream (ADR-062 (d), (g)). So a transcription turn,
  like a speech or image turn, emits **zero** incremental events across a wait of seconds
  (the first ASR call after a model load pays a warm-up).
- The browser's `MediaRecorder` produces a container the **browser** chooses, not one the
  operator picks: `audio/webm;codecs=opus` in Chromium, `audio/mp4` in Safari,
  `audio/ogg` in Firefox, with the `codecs` parameter riding on the type string. And
  **audio.cpp's transcription endpoint reads WAV only.** The first design relayed the
  recorder's container as recorded, with whether audio.cpp decodes it left as an open
  item; a **live probe** against a live audio.cpp server (2026-10-18) settled it: a
  `webm`, `ogg`, `m4a` or `mp3` upload is refused with **HTTP 400**, *"only WAV audio
  uploads are currently supported for transcription; MP3 support is planned"*, so a
  recording, and any compressed file a user attaches, cannot be transcribed as it is.
- audio.cpp bills `audio_second` from the **response body** (`timing.audio_duration_ms`),
  and the **endpoint**, not the chat run, meters it (ADR-062 (e)).
- A transcription model and a speech model are **distinct verdicts** on the one shared
  `openai_audio` flavor (ADR-062 (c)): `asr` ⇒ `(transcription, yes)` and `(speech, no)`,
  `tts`/`clon` ⇒ the reverse. The audio.cpp probe never produces a model that is both; a
  model carrying both verdicts can arise only from manual or agent config.

**Decision — nine choices, taken together.**

- **(a) `transcription` is a fourth run kind, and the picked model is its only
  affordance.** `portal.ChatRunSettings.Kind` gains `transcription` beside `""` (text),
  `image` and `speech`, and everything ADR-063 (a) decided for the speech kind carries
  over: no mode toggle and no "transcribe" button; the kind is established by the thread's
  first send and forced back on every later one by `PrepareChatRun`; and it is a **UI
  constraint, not an authorization** — a thread pinned to `transcription` whose mapping
  loses its `(transcription, yes)` verdict fails the capability gate exactly as an
  unpinned request would, and `filterCapable` stays the only authority
  ([Routing & Model Selection §2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate)).
  The knobs are a `ChatTranscriptionSettings` (the fields of (g)) on
  `ChatRunSettings.Transcription`, declared **last, after `Audio`**, and `omitempty`;
  `PrepareChatRun` clears it for every other kind, and clears `Audio`/`Image` for a
  transcription thread, so a text, image or speech chat's stored and wire bytes are
  unchanged, even against a raw API client. The settings are **not** force-pinned: the
  composer sends the thread's current settings on every send. The kind is surfaced like
  the others' (`kind` on the run's `201`, the active-runs rows and the `snapshot`/`done`
  events), and the browser picks the wait state from the **run's own** reported kind. The
  frontend's `prospectiveKind` chain for an empty thread is **`image → speech →
  transcription`**, placing transcription **after** speech to keep ADR-063 (a)'s
  image-before-speech precedence: this matters only for a model carrying *both* `speech`
  and `transcription` verdicts, which the audio.cpp probe never produces and which only
  manual/agent config can make, and then the non-chosen mode is simply unreachable for
  that one model (accepted; a disambiguation affordance is a later enhancement).
- **(b) The clip is a user-turn input, stored like a speech reply and resolved before the
  run like an image-edit source.** The attached or recorded audio is a **user-turn
  `audio_url` part** — the same part type ADR-063 (c) added for the speech *output*, since
  `audio_url` is the audio part regardless of turn role, as `image_url` serves both
  uploaded and generated images. `extractChatBlobs` already moves it into `chat_blobs` (it
  walks `audio_url`), so the storage spine is unchanged. A new
  `resolveTranscriptionRunSource` (`resolveImageRunSource`'s audio twin) resolves the
  **last** user message's clip from the blob store and **refuses** a run whose clip the
  chat does not hold (`ErrChatBlobNotOwned`): a fresh upload is inline and costs no store
  read, while a replay or regenerate carries a `/api/portal/chat-blobs/{id}` reference
  that must be resolved. `PrepareChatRun`'s resolution dispatch becomes **three-way** —
  `image → resolveImageRunSource`, `transcription → resolveTranscriptionRunSource`, else
  the default `resolveChatBlobRefs` (the image-family wrapper over the generalised
  `resolveBlobRefs`, taken by a text or speech run). The resolve walks (`collectBlobRefIDs`,
  `usableChatBlobs`, `substituteBlobRefs`, `rewriteBlobParts`) are **generalised to take a
  `blobPartKind`** so
  the audio family can be resolved — the change ADR-063 (c) deliberately deferred (it
  recorded "the history resolution stays image-only"), now made and amended there.
- **(c) The reply is the ordinary text turn; no new output part, and no blob for the
  output.** The response JSON's `text` becomes one `{"type":"text","text":<transcript>}`
  part committed via `finishRunWithParts` and rendered by the existing text renderer. The
  `timing` block is consumed by the endpoint's own billing, not by the chat run. There is
  **no `AudioTurn` for the output, no new content-part type, and no `chat_blobs` row for
  the output** — the exact inverse of speech, which added an `audio_url` output part and a
  WAV blob because *its* reply is binary. An empty or absent `text` is
  `gateway.chat_run_transcription_no_transcript` (an error, not a blank turn).
- **(d) The request is multipart-in (image-edit shape), not JSON-in (speech shape).**
  `executeTranscriptionRun` (`chat_runs_transcription.go`) reads the **last user
  message**'s audio via a new `portal.MessageAudio` (the `audio_url` twin of
  `MessageImages`) and `portal.DecodeAudioDataURL` (`decodeDataURL` with `audioPartKind`),
  and `buildTranscriptionBody` writes a `multipart/form-data` body — the clip as the
  single `file` part (a filename by media type, mirroring `imageEditFilenames`), `model`,
  the set `language`/`prompt`/`temperature` as form fields, and the Advanced passthrough
  fields — then POSTs `/v1/audio/transcriptions` via `setRunLoopbackHeaders` and
  **overrides the `Content-Type`** with the multipart one exactly as the image-edit hop
  does. `response_format` is **not sent**, because the endpoint answers JSON when it is
  absent and refuses every other value (ADR-062 (g)) — the twin of speech not sending
  `response_format`. A reserved-field guard (`transcriptionReservedFields`:
  `model`, `file`, `response_format`, `stream`, matched ignoring case) rejects a
  passthrough key that names one with `gateway.chat_run_transcription_reserved_field` and
  **sends nothing** — the multipart analog of speech's `model`/`input`/`stream`/
  `response_format` guard. A clip that cannot be turned into bytes (an external link, an
  unresolved reference, bad base64, a non-audio type) is
  `gateway.chat_run_transcription_source_unreadable`; a loopback hop that never completed
  is `gateway.chat_run_transcription_dispatch_failed`; a 2xx whose body cannot be read is
  `gateway.chat_run_transcription_response_unreadable`; and a non-2xx surfaces the
  endpoint's own `transcription.*` code. Every one of these run codes, and the endpoint
  refusals a run relays, has a German and an English label (`errorLabelByCode`), pinned by
  exhaustiveness tests — **except** `transcription.model_required` and `transcription.invalid`,
  left unlabelled on purpose (as `speech.model_required` and `images.model_required` are): a
  run always sends the thread's pinned model, and a structurally broken form is not a
  user-correctable run condition. The gateway converts nothing: the clip — a WAV by then, because
  the composer made it one ((e), (h)) — is relayed byte for byte.
- **(e) The blob allow-list's audio family keeps the broader input formats, and the family
  split holds.** `chatBlobMediaTypes`, ADR-056 (e)'s stored-XSS control, gains
  `audio/webm`, `audio/mpeg`, `audio/mp4` and `audio/ogg` beside `audio/wav`.
  `blobPartKind.accepts` keys on the `audio/` prefix **and** the allow-list, so a widened
  audio type joins the **audio family only**: an image part still cannot take it, and an
  image type is still refused by an audio part (the separation ADR-063 (c) established).
  These are **input** formats — what a client may upload for a run to read — the other
  direction from a generated image or a speech WAV. They were listed for the composer that
  was first designed to relay the recorder's container, and **they stay deliberately
  although the composer now produces only `audio/wav`** ((h)): the store is
  payload-type-agnostic (bytes plus a content type), so the wider list costs nothing, is
  as safe as the narrow one (it is an exact-match allow-list of audio types, checked on
  write and again on read), and is harmless forward compatibility — the server's own
  refusal says MP3 support is planned, and a composer that relays more than WAV later then
  needs no backend change. What the composer **no longer does** is relabel a clip to a
  canonical type: the per-format alias and extension tables it needed to hit the exact
  allow-list (`canonicalAudioMediaType`, the `TRANSCRIPTION_AUDIO_TYPES` list, the mapping of
  `audio/x-m4a`, `application/ogg`, `video/webm` and the other platform spellings, an
  extension fallback for an empty type) are **removed**, because every clip is converted and
  then labelled with the one fixed type `audio/wav`, an exact member of the list, whatever
  the upload was. What **survives** is parameter stripping: `bareAudioMediaType` still
  reduces a type to its bare `type/subtype` (dropping a `;codecs=…` parameter, which the
  backend refuses in a data url), in the pre-decode type gate (`mayBeAudio` in
  `audioAttach.ts`) and inside `audioDataUrl`. There is **no schema change**: the
  migration, the cascade, the sealing and the memory budget are exactly what they were.
- **(f) The transcription portal fold is new, and it amends ADR-062 (b).** The Models
  listing carries a **`transcription` flag** on `ModelDTO` (`/api/portal/models`, always
  present) via a new `transcriptionFlagsByName` = a `(transcription, yes)` verdict **and**
  `viewServesAudio` (the shared `openai_audio` flavor half, reused from speech), AND-ed
  across a model's mappings and a group's offerable members, inherited by an alias,
  fail-closed on a failed capability read — the exact twin of `speechFlagsByName`.
  `routing.CapabilityTranscription` is added to `hasCapabilityFold` and a `transcription`
  case to `capabilityFlagsByName`, and `transcriptionOn` is wired into the DTO builder
  (model, group, alias) beside `speechOn`, so the listing flag and the unknown-model
  redirect's `ModelOffering.Capable` stay **one rule** (as they are for speech): a token
  with the unknown-model redirect on that names an unknown transcription model now reaches
  a transcription-capable `LastUsedModel` or fallback, instead of the plain unknown-model
  refusal. **Only the chat's `chatModels` filter widens**, to admit an `openai_audio`
  model behind `transcription: true`; the shared `offeredFlavors` and the Models view are
  deliberately **untouched** — gating the flavor there would drop it from speech-less,
  unprobed and not-yet-reported agent-managed audio rows (the ADR-063 (e) correction). This
  retires ADR-062 (b)'s "the capability has no portal fold": transcription now has the
  listing flag and the redirect candidacy that ADR-062 recorded it lacked.
- **(g) The settings surface is `language`/`prompt`/`temperature` plus an Advanced JSON
  passthrough, and there is no `response_format` control.** The settings panel
  (collapsible, mirroring the speech one) carries free-text `language` and `prompt` and a
  `temperature` number, each blank-able (a blank field is omitted, leaving the model's own
  default). A collapsible **Advanced** section holds one text box for a JSON object merged
  into the request body as form fields verbatim, which is how audio.cpp's native knobs
  travel; `model`, `file`, `response_format` and `stream` are **reserved**, matched
  ignoring case — the list is **copied** to the client (`TRANSCRIPTION_RESERVED_FIELDS` in
  `chatDoc.ts`), where text that is not a JSON object or that names a reserved key blocks
  both Send and a replay with the section held open and the error inline, so a turn is not
  committed only to fail; the backend check stays the authority for every other caller.
  There is **no `response_format` control**: the gateway forces JSON and refuses every
  other value (ADR-062 (g)), so a static caption states the reply is **text** — the twin
  of speech's "always WAV" caption. Send is enabled once a clip is attached or recorded.
- **(h) The composer records or attaches audio and converts it to WAV, and the input clip
  plays through a fetched object URL.** A transcription thread's composer replaces the
  text prompt with an audio input control: a **file-attach** button (reuse `handleFiles`, audio accept) **and** a
  **record** button (`MediaRecorder`; the mic permission is requested on record). The
  microphone **tracks are released** (`stopTracks`) at stop (the recorder's `onstop`
  finish), on a failed start, on a recorder error and on unmount, so the browser's recording
  indicator never lingers — the care issue #249 took after its own early defect of leaving
  the mic held when the recorder could not start (#249's own fix); **send and
  discard** operate on an already-stopped recorder and only revoke the preview's object URL.
  The attached or recorded clip shows in the user bubble as an audio player: the
  ADR-063 (d) `AudioTurn` is **reused for the input**, fetching the blob once into an
  object URL (the blob endpoint answers no `Range` request, ADR-056 (e)) and **revoking it
  on unmount**. The attach mode of a transcription thread is otherwise `none` (it takes no
  image). **Every clip is converted to WAV in the browser, before it enters the turn**,
  because the live probe showed audio.cpp reads WAV only (see Context). `prepareAudioClip`
  (`chat/audioAttach.ts`), the one path both a picked file and the recorder's "Use
  recording" take, calls `audioBlobToWav` (`shared/audioToWav.ts`, dependency free): WebAudio's
  `decodeAudioData` **decodes** whatever the browser can (it sniffs the bytes, not the type
  or the name, so webm/opus, mp4/aac, mp3, ogg, flac and wav all work wherever the browser
  has the codec; Safari's `webkit` prefixes are honoured) → an
  `OfflineAudioContext(1, frames, 16000)` **resamples and downmixes** (the browser's own
  mix to mono and conversion to 16 kHz, the rate speech models take, which also keeps the
  clip small) → the samples are written as a canonical 44-byte RIFF/WAVE header plus
  **16-bit little-endian PCM**: a **16 kHz mono 16-bit PCM WAV**. The clip that is stored
  and sent is therefore always `data:audio/wav`, so what the user hears in the preview is
  exactly what is stored and relayed. The type gate before decoding is only "plainly not
  audio is refused" (an image, text or a PDF); audio the browser cannot decode, a browser
  without WebAudio and a clip with no frames are refused in the composer with a decode
  message and **nothing is attached**; and the 20 MiB ceiling is checked on the upload
  **and again on the WAV**, since 16 kHz mono 16-bit is 32 KB a second and a small
  compressed upload can come out far larger. The conversion takes a moment for a long
  clip, so `ChatStore.attachAudioClip` (the one caller of `prepareAudioClip`) exposes
  `audioConverting`, and the composer shows an indeterminate "Converting audio…"
  indicator (a spinner and its label, announced as a status) beside the attach controls
  while it runs, holding the attach button, the recorder's start and "Use recording", and
  Send until the clip has landed or the attach has failed. The conversions may overlap and
  finish in any order, so **each attach takes the next number from a monotonic counter and
  only the newest one counts**: a conversion that finishes after another attach started —
  or after the clip was removed or sent, which takes a number too and so voids the attach
  in flight — is discarded (its clip is not set, its refusal is not said, and it does not
  end the converting state that belongs to the newer attach). The attach picker's `accept`
  is `audio/*` plus extension fallbacks for platforms that do not type an audio file as
  `audio/*`. The conversion's own costs — decode memory, no progress figure, the two-stage resample
  (`decodeAudioData` to the context's own rate, then the offline context to 16 kHz), old
  Safari — are accepted in
  [Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).
- **(i) The wait is `TranscriptionPendingTurn`, and an untitled transcription chat is
  titled from its transcript.** The run is one buffered zero-delta call (like image and
  speech), so the text-pending "reasoning active · 0 chars" indicator must not be the wait;
  `TranscriptionPendingTurn` (`AudioPendingTurn`'s twin over the shared `RunPendingTurn`:
  liveness from the run's own status, a **server-measured** elapsed clock, one static
  caption, **no** progress figure) is gated on `kind === 'transcription'` in
  `ChatMessage.tsx` beside the image and speech pending guards. A transcription thread's
  **first user turn is audio-only**, so both the backend `deriveChatTitle` and the frontend
  `deriveTitleFromMessages` (the first user message's *text*) find nothing; an untitled
  transcription chat is therefore **titled from its transcript** on run completion
  (`titleFromTranscript`), the natural mirror of "title from the first message's content",
  falling back to the localized "Untitled" placeholder the sidebar already shows.

**Consequence:** a transcription turn is the inverse of a speech turn at the same cost. It
is one buffered request with no progress to show, so the wait is the elapsed clock and an
honest "no intermediate news"; the first ASR call after a model load can take long enough
to matter inside the `transcriptionRunDeadline` 10-minute bound (`runDeadlineFor`, a
variable of its own so the three bounded kinds can move independently). One clip is bounded
at **20 MiB** by default: the composer rejects a larger file at the client's
`MAX_AUDIO_BYTES` (20 MiB), and the store refuses a larger blob at `MaxChatBlobBytes`
(`OP_AI_GATEWAY_MAX_CHAT_BLOB_BYTES`, default 20 MiB, operator-configurable), the same
store limit a speech WAV is held to. The cap applies to both the upload and the WAV the
composer produces from it, and at 16 kHz mono 16-bit (about 32 KB a second) a 20 MiB WAV
is about 10.9 minutes of audio, which is the longest clip the composer can attach. The
executor holds the clip whole in memory for the multipart body, and the browser holds one
object URL per input clip it has rendered until it unmounts, as a speech reply's player
does. Stored clips are retained for the chat's lifetime like every blob, with no prune
(ADR-056). The **gateway still never converts or re-encodes**: the conversion that audio.cpp's WAV-only
input requires happens in the **browser**, so the stored clip and the upstream `file`
part are the same WAV, relayed byte for byte, and in-browser recording and any attached
format the browser can decode work end to end (the `e2e:chat-transcription` suite drives
a real WebM/Opus recording, a WebM file and an MP3 file through the real WebAudio
conversion and asserts that the upstream receives a 16 kHz mono 16-bit PCM WAV). The price
is paid in the user's browser: the clip is decoded in memory before it is sent, a long file
shows only an indeterminate "converting" indicator and no progress figure, and the audio is
resampled twice (`decodeAudioData` first brings it to the `AudioContext`'s device-dependent rate, then
the offline context to 16 kHz), each stage of an implementation-defined quality; each is
a stated acceptance in
[§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances). Nothing else
moved: no change to `handleOpenAITranscription` or the relay, no schema change, no
migration, no new auth leg (the loopback pair ADR-062 (j) already admits gains its first
portal caller), and **no ServerAgent version bump** (no agent change). The portal side is
gateway and frontend only.

**Amended by [ADR-065](#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript):
the transcription run can now STREAM, and #262 does bump the agent.** Issue #262
(ADR-065) gives `executeTranscriptionRun` a streaming branch: against a `mode=streaming`
model the run POSTs `stream=true` over loopback and renders the transcript
**incrementally** from `transcript.text.delta` events, reusing the text run's delta/
checkpoint plumbing, so the "one buffered zero-delta call" of (i) is no longer the only
shape — `TranscriptionPendingTurn` now shows only until the first delta, and the run keeps
its full deadline until then. The run still handles a buffered answer (the gateway decides
stream-vs-buffered), and the ordinary-text-turn reply of (c) is unchanged. Unlike #249,
#262 **does** bump the ServerAgent (0.9.1 → 0.9.2, a PATCH on #258's wire) so an
agent-launched child can report the `transcription_streaming` verdict.

**Rejected:** **Reusing `ChatAudioSettings` for the knobs** (speech's `voice`/`speed` are
disjoint from a transcription's `language`/`prompt`/`temperature`; a shared carrier would
hold fields neither kind uses and muddle the pin/clear). **A `response_format` control**
(the gateway forces JSON and refuses every other value, ADR-062 (g), so a control would
offer what the endpoint refuses — the twin of the rejected speech `response_format`
control). **An output blob or a new output part for the transcript** (the reply is text;
the inverse of speech, which needed a part and a blob because its reply is binary).
**Widening the shared `offeredFlavors`** (it would regress the Models view for every audio
row without a transcription or speech verdict; the chat's stricter rule belongs in the
chat, the ADR-063 (e) correction). **Gateway-side transcoding of the clip to WAV** (the
project never re-encodes, ADR-063; the conversion is client-side, which is where the
recorder's audio is born and where the codecs already are). **Relaying the recorder's
container byte for byte** (the first design: the live probe showed audio.cpp reads WAV
only, so every recording and every compressed attachment would fail upstream). **An
audio-conversion library in the portal** (WebAudio already decodes, downmixes and
resamples, and the encoder is a 44-byte header). **A second multipart/audio code path** (the image-edit
multipart machinery and the ADR-063 blob/player machinery are reused with a transcription
branch, not a parallel system). **A disambiguation affordance for a model carrying both
audio verdicts** (it arises only from manual/agent config, never from the audio.cpp probe;
the non-chosen mode is simply unreachable for that one model, a later enhancement).
→ [API Compatibility & Inference
§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[§3.7](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions),
[Runtime View
§6.1.4](06-runtime-view.md#614-portal-transcription-chat-turn-through-loopback-from-a-chat-blob),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[§2.2](cross-cutting/routing-and-model-selection.md#22-callable-existing--and-why-the-listing-is-neither),
[Security, Authentication & Authorization
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance),
[§13.1](cross-cutting/security-auth-rbac.md#131-chat-binary-payloads-a-sealed-store-ownership-on-read-a-hardened-serving-url),
[Data Model](reference/data-model.md#portal-chat),
[HTTP API Surface](reference/api-surface.md#tokens-chats-usage),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-065 — Streaming transcription: the gateway relays the SSE for a streaming model, gated by a transcription_streaming verdict with a retry-buffered-on-500 safety-net, bills the streamed call from the WAV header, and the portal chat run streams the transcript
**Context:** transcription — the `/v1/audio/transcriptions` endpoint
([ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint),
issue #69) and the portal transcription run
([ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn),
issue #249) — was built as **one buffered call**: the gateway refused `stream`
(ADR-062 (g), `transcription.stream_unsupported`) and answered one complete `{text,
timing}` document, and the portal run showed a zero-delta wait. A live probe against a
local audio.cpp `audiocpp_server` (2026-10-18) settled what ADR-062 had no reason to
examine: audio.cpp **does** stream transcription — the same `POST
/v1/audio/transcriptions` with `stream=true`, requiring a model **loaded with
`mode=streaming`** (an offline model answers `500 "requires a model configured with
mode=streaming"`). The wire format is OpenAI-style SSE: `transcript.text.delta`
(incremental) → `transcript.text.done` (the full `text` plus `timing.ttft_ms`) → `[DONE]`.
A `mode=streaming` model also serves the non-streaming one-shot, a superset. Issue #262
lifts the refusal so the gateway relays streaming transcription and the portal renders the
transcript as it arrives. Four facts about the backend shaped every choice below, none of
them a design's to pick:

- Streaming needs `mode=streaming`; `mode` (`offline` | `streaming`) is a field of
  audio.cpp's own `GET /v1/models`, beside the `task` ADR-062 (c) already reads, but was
  not captured.
- The streamed `transcript.text.done` event carries **no `audio_duration_ms`** (two
  probes; only `ttft_ms`), so a streamed call has **no response-side billable duration** —
  the one input ADR-062 (e) billed from is simply absent on the stream.
- The runtime is reached **agent-mediated (per run-spec)**: the audio.cpp process runs
  statically (models managed in its own UI), but the gateway's visibility is the run-spec,
  so reading `mode` for an agent-launched child needs the ServerAgent to report it.
- `mode` can go stale: a model is swapped or unloaded in audio.cpp's UI without an agent
  (or health-loop) restart, so a verdict that said `streaming` can be wrong at the moment
  of the call.

**Decision — six choices, taken together, every one a reversal scoped to the streaming
case and leaving the buffered path byte-identical.**

- **(a) The `stream` refusal is lifted for `stream=true`, and the gateway relays the SSE
  unbuffered.** `validateTranscriptionRequest` no longer refuses a `true` `stream`; when
  the gateway streams it sends `stream=true` upstream and relays the event stream
  **chunked, byte for byte**, the one success in the transcription relay that does **not**
  go read-decide-write (ADR-062 (f)). What **stays refused** with 400
  `transcription.stream_unsupported` is a **non-boolean `stream`** (a typo is a client
  error, not a silent buffered fallback) and **any `stream_format`** (audio.cpp emits its
  own SSE shape and the gateway translates nothing — the tight mirror of ADR-062 (g)'s
  `response_format` rule). `stream: false`, an absent or empty `stream`, and no
  `stream_format` keep the buffered one-shot of ADR-062 exactly.
- **(b) The stream-vs-buffered decision is taken up front from a per-mapping
  `transcription_streaming` verdict, with retry-buffered-on-500 as the safety-net.** The
  runtime's per-model `mode` is captured as a **capability verdict** in the project's
  existing per-mapping vocabulary — `transcription_streaming`: **yes** = the model is
  `mode=streaming`, **no** = offline, **absent** = unknown (and it is written **only for an
  ASR model**, the one axis on which streaming is a question; it rides the same
  `audiocpp_models` source and `/v1/models` read as the `transcription`/`speech` verdicts,
  (f)). The handler reads it keyed on the resolved route (`MappingCapabilities(ctx,
  target.RouteID)`, not a new `routing.Target` field): **yes** and the client asked
  `stream=true` → stream the SSE; **no / absent / a read error** → the buffered one-shot
  (drop `stream`, return the complete JSON). The decision is **off-by-default-feeling**: an
  unprobed, offline or not-yet-reported model simply buffers as before, and a client that
  asked `stream=true` against one gets a valid buffered answer, never an error.
  **Safety-net:** when the gateway streamed on a `yes` that had gone stale and the upstream
  answers `500 "requires a model configured with mode=streaming"`, it **re-sends the same
  request once with `stream` dropped** and returns the one-shot JSON. The upload is already
  buffered whole in memory (ADR-062 (d)), so the retry re-sends the same bytes and nothing
  of the failed stream has been written to the client yet; any **other** upstream error
  surfaces as ADR-062 (i) already relays it. So the verdict is a pure optimisation of the
  up-front choice: without it, or with it stale, the client still gets a correct answer and
  the route self-heals at the next probe.
- **(c) A streamed call bills `audio_second` from the uploaded WAV header, on a clean
  completion only, loud-zero otherwise, with a forged-header guard.** A streamed response
  has no `audio_duration_ms` (Context), so the one input ADR-062 (e) billed from is gone.
  The handler already holds the upload whole (ADR-062 (d)); it parses the **WAV header**
  (the `data` chunk's byte count over the format's byte-rate = duration — plain RIFF/WAVE
  header math, no codec; the upload is a WAV because audio.cpp refuses a non-WAV one, which
  then never completes, #249/#259) and
  sets `audio_second = duration / 1000` **only on a clean stream completion** — the
  terminal `transcript.text.done` **and** `[DONE]` both seen. A stream that ends without
  both while the relay otherwise ended `success` (an error-status cut is already an error
  row), and an upload that is not a parseable PCM WAV, records a **loud zero**: `0`, logged
  once at `Error`, because the `(unit, quantity)` pair has no "unknown" and the gap is made
  loud, not repaired ([ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar)).
  A **forged-header guard** bounds under-billing from both directions: the uploaded bytes
  must not exceed the header's declared `data` size by more than about 16 KiB — real audio
  **hidden behind a small or zero `data` chunk** (which would under-report the duration) is
  rejected to a loud zero — and the `fmt` chunk must be self-consistent
  (`byteRate == sampleRate * blockAlign`, `blockAlign == channels * ceil(bits/8)`), which
  blocks an **inflated byte-rate** that would shrink the computed duration. A `data` size
  that declares **more** than the upload holds (a truncated file, or an oversized
  declaration) is no measurement either: the helper returns `(0, false)`, which is a
  **loud zero** — never a normal bill of the declared duration. That zero is no under-bill,
  because audio.cpp refuses such an upload outright: probed live against audio.cpp v0.9.1,
  a truncated file, a `data` size declared 1 MiB too large and `0xFFFFFFFF` all fail with
  HTTP 500 `failed to read WAV data chunk`, in a buffered request and in a `stream=true`
  request to a `mode=streaming` model alike (the same JSON error arrives before any SSE
  starts, and on an offline-mode model the oversized files fail on that read before the
  streaming-mode check, so it is a shared front end), so the call never completes and no
  clip is transcribed for the zero. A future audio.cpp that decodes a short `data` chunk would turn
  it into an under-bill, answered by billing from the bytes the upload holds. The residual a
  real file can carry is that trailing slack (about 16 KiB, ≈ 0.5 s at 16 kHz mono 16-bit,
  longer at a lower byte rate), and a consistently forged header is self-defeating because
  audio.cpp would then mis-transcribe the clip it describes. This reverses ADR-062 (e)
  **for the streaming case only**, and only to the WAV-header duration (exact for PCM WAV,
  not an arbitrary-container decode); the **non-streaming path is unchanged**, still billing
  from the response `timing.audio_duration_ms`.
- **(d) A streamed response is captured as its assembled transcript, endpoint-stamped.**
  The buffered capture stores the JSON transcript (ADR-062 (h)); a streamed call captures
  the transcript **assembled from the deltas** as the output body, under the same opt-in,
  the same capture cap and the same sealing at rest, with the multipart request captured as
  today (the `file` part redacted). The capture's `Endpoint` **must be stamped as the
  transcription endpoint**, because the event-stream body would otherwise be caught by the
  binary-speech capture backstop (`isBinarySpeechResponse`, keyed on the shared
  `openai_audio` flavor and a 2xx status) and stored as a redacted binary stub rather than
  the text it is — the same stamp ADR-062 (h) added for the buffered transcript, now
  carried on the streaming path too.
- **(e) The portal transcription run streams the transcript.** `executeTranscriptionRun`
  gains a streaming branch: it POSTs `stream=true` over the loopback and consumes the SSE
  with a **transcription-specific consumer** (`transcript.text.delta` → accumulate and
  publish a run delta/checkpoint; `transcript.text.done`/`[DONE]` → the terminal commit of
  the full text), reusing the **text run's** reservation / deadline / delta / checkpoint
  plumbing rather than the chat-completions SSE parser, and **not** the zero-delta image/
  speech path. The reply is still the ordinary text turn (ADR-064 (c)); it just fills
  incrementally, with `TranscriptionPendingTurn` shown until the first delta. Because the
  run requests streaming but the gateway (b) may still deliver buffered, the branch also
  handles a non-streamed answer. The run keeps its **full deadline until the first delta**
  — a `mode=streaming` model's cold start / time-to-first-token can exceed a short idle
  watchdog — and the usual idle/checkpoint cadence applies once deltas flow; the gateway's
  own streamed relay has a matching **first-byte deadline grace** ((f), [Risks
  §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
- **(f) The ServerAgent reports the verdict as a PATCH bump, no new `agent.Features`
  flag, no migration.** For a **direct** (static) `audio_cpp` application the gateway's
  health loop decodes `mode` from `/v1/models` and writes the `transcription_streaming`
  row itself; no agent is involved. For an **agent-launched** child the ServerAgent decodes
  `mode` in its audio collector and emits the verdict — and this rides PR #258's **existing
  one-sided gateway-declared flag `capability_source_audiocpp`** and the `audiocpp_models`
  source as **one new capability name**, so it needs **no new `agent.Features` entry**,
  which makes it a **PATCH** bump (0.9.1 → 0.9.2), not MINOR
  ([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions):
  a flag earns its place only when the consumer must interpret the *absence* of a message,
  and here an absent verdict already means "buffered", which is correct). There is **no
  migration**: `model_mapping_capabilities` is an open vocabulary (ADR-062 (c)), so the new
  name is just another row. An older agent that omits the verdict leaves it **absent** →
  the buffered-plus-retry path of (b), a graceful degrade with no hard version dependency.
  The agent caches verdicts once per process generation, so a UI mode-change without a
  restart goes **stale** — which is exactly the case (b)'s retry-on-500 exists to cover.
  The gateway relaxes its agent-ingest allowlist to admit the name on the agent path only;
  `(transcription_streaming, no)` is reserved against a manual write and against a probe
  being outranked by a stale manual override, as the other audio verdicts are.

**Consequence:** streaming transcription is a staged, backward-compatible addition, not a
new code path. The relay, the WAV-header billing, the capture and the retry-buffered-on-500
safety-net (stage 1) stand on their own and are live-verifiable through the API: the handler
streams only on a `yes`, so the mismatch 500 can only follow one — a manual one while only
stage 1 exists, the probe's once stages 2 and 3 land — for a model that is not
`mode=streaming`, and is then retried buffered; a model with no verdict is not streamed at
all and never draws it. The up-front `transcription_streaming` verdict (stages 2 and 3 — the
direct-app `mode` read and the agent report) is then what takes the decision up front, and
nothing breaks without it or with it stale: a `yes` streams (the retry covering a rare stale
`yes`), and an **absent** verdict buffers and self-heals once the model is probed, so no steady-state request streams against a model not known to support it. Stage
4 (the portal streaming run) is the UX. The non-streaming one-shot path is byte-identical
when `stream` is absent or the model is offline, and #259's client-side WAV conversion is
untouched. The accepted nuances are named, not hidden: a stream the upstream closes cleanly
but **without the terminal `[DONE]`** bills a loud zero that reads in the usage UI as a
`success` of quantity 0 beside an Error log; the first-byte grace lets a cold
`mode=streaming` model take its warm-up; and the forged-header residual is the trailing
slack a real WAV can hide (about 0.5 s at 16 kHz mono 16-bit) ([Risks
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).

**Rejected:** **Translating `stream_format`** (audio.cpp emits its own SSE; the project
proxies natively shaped backends and does not translate, ADR-062). **Buffering the streamed
response to read a `timing`** (there is none on the stream, and buffering defeats
streaming). **Billing a streamed call from the request by decoding the container** (the
upload is WAV, so the header math is exact and needs no codec; a general decode is the
thing ADR-062 rejected). **Billing on a partial stream** (a stream that ends without
`transcript.text.done` + `[DONE]` is a loud zero; a partial transcript is not a billable
measurement). **A new `agent.Features` flag for the verdict** (an absent verdict already
means the correct buffered behaviour, so the name would gate nothing — ADR-025). **A new
per-model struct or a migration** (the per-mapping capability vocabulary is open; `mode`
is one more verdict name). **A new `routing.Target` field for the verdict** (a keyed
`MappingCapabilities` read in the handler is enough, and error/absent already falls to
buffered). **A hard version dependency on agent 0.9.2** (an older agent degrades to
buffered and self-heals once upgraded). **A second audio code path** (the transcription
handler and the text run's streaming machinery are reused with a `stream` branch).
→ [API Compatibility & Inference
§3.7](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions),
[§7](cross-cutting/compatibility-and-inference.md#7-streaming-lifecycle),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Security, Authentication & Authorization
§14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[Agent-Managed Model Runtime
§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[Runtime View
§6.1.2](06-runtime-view.md#612-transcription-relay-post-v1audiotranscriptions),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Glossary](12-glossary.md),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-066 — Agent-provisioned config files: one child per spec, a 0..N config-files facet written to an agent-owned directory, a `${CONFIG_DIR}` placeholder, and non-secret content only
**Context:** a managed child's launch specification could express only a
**command line** — `binary`, `args`, `env`, `work_dir` — and the only file the
agent ever wrote was its own runtime-config cache, never a config file *for* the
process it launched. A server that cannot be driven entirely by argv therefore
could not be managed end to end from the run-spec form. **audio.cpp** is the
concrete case
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only),
[ADR-062](#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint)):
it reads the models it serves from a JSON config (`server.json`, an array of
`{id, path, task, mode}`) that the operator had to create **by hand on the host**,
outside the gateway, and pass as a fixed `--config` argument. Two problems
followed. **Operability:** the portal could not provision the file, so audio.cpp
was not manageable end to end. **Drift:** the model id in that hand-made file is
decoupled from the spec's `upstream_model` — the application-side name the
gateway rewrites a request to on the native relay those two ADRs describe — and a
mismatch is audio.cpp's `unknown model id: <name>`, surfaced as HTTP 500. Issue
#257.

**Decision — eight choices, taken together.**

- **(a) One child per spec (Variant A); a shared server (Variant B) is rejected.**
  Each spec provisions its own config file(s), its own port and its own GPU, which
  reuses the one-child-per-spec runtime, port and GPU accounting unchanged. Variant
  B — one audio.cpp serving several models from a single `server.json` assembled
  from several specs by a reconciler — is more efficient but breaks the
  one-child-per-spec assumption (lifecycle, admission, GPU accounting, idle and stop
  would all become server-scoped) and needs a conflict-free many-specs-to-one-file
  reconciler. Running N processes (more memory and VRAM, repeated loading, N health
  checks) is the accepted cost.
- **(b) A general `config_files` facet: 0..N `{name, content}` files per spec.** It
  is not audio.cpp-specific, so the per-model or per-voice file another server needs
  (PocketTTS's `--voice-ref`) is covered without a second pass. It is stored as
  `agent_runtime_specs.config_files`, an opaque JSON `text` column added by
  **migration 93** (append-only, `text not null default ''`, `''` = none, so every
  existing spec is unchanged; the portal validates it, the agent decodes it, the
  store never parses it —
  [ADR-004](#adr-004--three-store-drivers-behind-one-dialect-seam),
  [ADR-005](#adr-005--postgresql-needs-wide-column-types)), carried to the agent as
  `specs[].config_files` of the runtime-config document and edited in the portal's
  launch-spec form. Names are single file names, unique case-insensitively; at most
  16 files of at most 64 KiB per spec.
- **(c) The agent writes the files into an agent-owned, configurable directory —
  never `work_dir` — under a per-spec subdirectory.** `work_dir` is
  operator-chosen and may not be writable (the binary's own directory, a read-only
  mount), so writing there would turn "can this spec start" into a question about a
  directory the agent does not own. The agent instead writes to
  `<config_files_dir>/<spec-id>/<name>`. `config_files_dir` is a new agent setting
  on the usual triple source — flag `-config-files-dir`, env
  `OP_AGENT_CONFIG_FILES_DIR`, config key `config_files_dir`, default
  `server-agent-config-files` beside the binary, a relative file value resolved
  beside the config file — and it is **operator-only, like the allowlists**: the
  gateway names a file's relative name and content, never where on the host it
  lands ([ADR-024](#adr-024--managed-runtime-the-gateway-specifies-the-launch-the-ai-server-permits-it)).
  The per-spec subdirectory makes uniqueness structural: two specs that both name
  their file `server.json` cannot overwrite each other. Each file is written
  atomically (a temp file inside the target directory, `0600`, renamed into place),
  **on every launch, after the port is chosen and before the exec**.
- **(d) `${CONFIG_DIR}` is a first-class placeholder that resolves to the
  directory.** It becomes `<config_files_dir>/<spec-id>` in `args`, `env` **and**
  file content, so an operator writes `--config ${CONFIG_DIR}/server.json` and a
  multi-file config can name a sibling by absolute path. A per-file form such as
  `${CONFIG:server.json}` was rejected as redundant: the operator already names the
  file in the editor and joins it in `args`. It is exact-match only, a plain path
  rather than a secret, and so is never masked in the reported command.
- **(e) Content expands the non-secret tokens only; the two secret tokens are
  refused.** A file's content runs through a small content expander over the
  **same resolver** `args`/`env` use — `${PORT}`, `${MODEL}`, `${HOST_GPU_IDS}`,
  `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}` and `${CONFIG_DIR}` — so
  the audio.cpp `id` derives from `${MODEL}` and the listen port from `${PORT}`,
  which removes the drift **by construction**. `${API_TOKEN}` and `${AGENT_ENV:…}`
  are **hard errors** in content, enforced by the gateway at submit time and again
  by the agent: a written file outlives the process and sits where other local
  users and backups can read it, so a decrypted secret is never placed there
  ([ADR-027](#adr-027--model-secrets-never-enter-the-gateway),
  [ADR-035](#adr-035--the-gateway-owns-the-runtime-spec-upstream-token)). This is
  also why the content expander is deliberately **not** `expandSpec`, which
  resolves the two secret tokens and records spans to mask them in the reported
  command — meaningless for a file that is never reported.
- **(f) A failure split: deterministic faults are `not_permitted`, filesystem I/O
  is `start_failed`.** A secret or malformed placeholder in content, an unsafe file
  name, or a spec id that would escape the base directory can never succeed on
  retry, so it is found by the **pre-port dry run** — the same one a bad `args`
  placeholder already goes through, port 0, because the port only substitutes
  `${PORT}` — and takes the rate-limited `not_permitted` path before a port is
  acquired or a byte is written. A genuine write failure (an unwritable directory, a
  full disk, a failed rename) is an environmental launch failure, not a policy
  denial: it takes `start_failed` with crash backoff and a message naming the file
  or directory, never `not_permitted`.
- **(g) A feature flag and one version bump.** Writing a config file is a new
  observable agent behavior, so it goes through feature negotiation
  ([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)):
  the agent declares `runtime_config_files` (`Since: "0.10.0"`) and ServerAgent moves
  `0.9.2` → `0.10.0`, MINOR because `agent.Features` gained an entry — **one bump for
  the whole branch**. The flag has the `gpu_selection` shape: **declared for the
  portal's benefit**, gating nothing on the agent (which always honors the files it
  receives) and nothing on the gateway. An older agent ignores the unknown field and
  launches the child with the file missing, which no probe can tell apart from a spec
  that never asked for one, so the portal warns — non-blocking — when a spec has
  files and the connected agent has not declared the name.
- **(h) File-mode report content redaction.** In file mode the agent's upward
  report already masks every spec's `env` **values**, because a hand-written
  `runtime.json` may carry a literal secret the gateway must never receive.
  `config_files` rides the same report and is equally secret-bearing, so each file
  keeps its `name` and has its `content` masked the same way. The **write step is
  unchanged** in file mode — the files are written, unmasked, for the child exactly
  as in gateway mode. The gateway's ingest mirror of a spec carries no
  `config_files` field, so the facet never reaches storage either.

**Consequence:** the whole audio.cpp lifecycle can be driven from the gateway, and
the `unknown model id` drift that came from a hand-maintained id is gone as a
*cause* (the 500 handler itself is unchanged). A spec with no config files is
byte-for-byte unchanged, so nothing about existing deployments moves. A change to a
spec's `config_files` is launch-affecting, so a content edit relaunches the child.
The accepted nuances are named, not hidden. **Nothing garbage-collects the
directory**: a file dropped from a spec, and a removed spec's whole `<spec-id>/`
directory, stay on disk (overwrite-on-launch only). **Secrets cannot be placed in a
file**, so a server that can take a credential only from a file cannot be fully
provisioned this way. The per-spec directory's containment is **lexical**, like the
work-directory check. A malformed or repeated name is a hard parse error for the
whole file-mode document (`invalid_config_file_name`, a new closed `parse_error`
code on the three-sided contract), and `ParseConfig` applies the gateway's name rule
and **no stricter**, because it is the single parser for both modes and a stricter
rule would drop a document the gateway already accepted; the platform strictness (a
trailing dot or space, Windows device names, invisible Unicode format characters) is
applied at the write boundary instead, where it fails one spec rather than the
document.

**Rejected:** **A shared server assembled from several specs** (Variant B, (a)).
**Writing into `work_dir`** (it may not be writable, and two specs could share it).
**A per-file placeholder `${CONFIG:<name>}`** (redundant beside the directory form).
**Expanding `${API_TOKEN}` or `${AGENT_ENV:…}` inside a file**, even at `0600`
(a decrypted secret at rest on disk, outliving the process). **Reusing `expandSpec`
for content** (it resolves and masks secrets, neither of which applies to a written
file). **Taking a write failure down the `not_permitted` path**, or a deterministic
fault down the backoff path (the first claims a configuration can never work when
the disk is merely full; the second hammers the filesystem over a spec that cannot
succeed). **A gateway-side gate on the flag** (the document that carries
`config_files` is the one every agent already polls, so there is no route to
withhold). **Garbage-collecting a removed spec's directory** (deferred;
overwrite-on-launch is enough for the current need).
→ [Agent-Managed Model Runtime
§3.5](cross-cutting/agent-runtime-manager.md#35-config-files-the-agent-writes-before-it-launches-the-child),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[§8.3](cross-cutting/agent-runtime-manager.md#83-the-upward-report-and-what-it-redacts),
[Configuration & Environment Variables](reference/config-env.md),
[HTTP API Surface](reference/api-surface.md),
[Data Model §4](reference/data-model.md#runtime-spec-config-files),
[Security, Authentication & Authorization
§8.2](cross-cutting/security-auth-rbac.md#82-free-form-text-from-an-agent-is-redacted-safe-by-default).

## ADR-067 — A persistent per-mapping cache, first used for TTS voices: fetched while the model is loaded, change-detected, and read by a picker and a fail-open routing filter
**Context:** the portal chat's speech composer took `voice` as free text because the
gateway did not proxy `GET /v1/audio/voices`
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)
(j)) and the agent router had no voices route
([ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(f)), so a dropdown had nothing to be filled from. Issue #251 asked for the
discoverable picker, and the obstacle behind it is a property of the upstream, which
was **live-verified** against an audio.cpp v0.9.1 server before anything was built:
`GET /v1/audio/voices` answers a flat `{"voices": ["demo_1_man", ...]}` (names only: no
id, language or gender objects) for **whichever model that server has loaded right
now**, and it **ignores a `?model=` query** (the same answer for no parameter, a valid
id and a bogus one); `GET /v1/models` carries `loaded: true|false` per model; a listed
name is consumed by the **existing speech `voice` field** (honored and validated
against the model's cached voice references, an exact match on what is a directory
name; `voice_id` is ignored), and an **unknown voice answers HTTP 500**, not 400; a
request with no voice gets a default one. So voices exist only while a model is
loaded, and a picker that must work while it is **unloaded**, or after a gateway
restart, needs the names to be kept. That rules out
[ADR-055](#adr-055--per-model-sd-server-image-options-are-read-live-on-pick-from-two-volatile-caches-and-never-stored)'s
answer: an `sd-server`'s options can be re-read from its capabilities document at any
time, so a volatile cache re-warms; a voice list can only be read during a window the
gateway does not control, so losing it costs the picker. It also rules out
`model_mapping_capabilities`
([ADR-039](#adr-039--per-model-capabilities-are-child-rows-with-ranked-provenance-and-the-eleven-columns-are-dropped)),
which holds one kind of fact (a ranked `yes`/`no` verdict) for the same reason ADR-055
gave.

**Decision — six choices, taken together.**

- **(a) A general, persistent per-mapping cache: the first.** A new table
  `model_mapping_cache` (**migration 94**, append-only) holds `(mapping_id,
  cache_type, scope_key, payload, updated_at)`, primary key `(mapping_id, cache_type,
  scope_key)`, `mapping_id` a real foreign key `on delete cascade`, `payload` a
  `text` column of opaque consumer-owned JSON (`''` is a legitimate payload: "fetched,
  nothing to offer"), `updated_at` the writer's clock. It is general on purpose: a later
  per-mapping discovered fact adds a `cache_type`, not a table
  ([ADR-004](#adr-004--three-store-drivers-behind-one-dialect-seam),
  [ADR-005](#adr-005--postgresql-needs-wide-column-types)). Voices are the first type:
  `cache_type = "voices"` (`routing.ModelMappingCacheVoices`), `scope_key = ""` (one row
  per mapping; the column is there for a later sub-mapping axis), the payload a JSON
  array of voice **names** in canonical form (deduplicated, sorted bytewise, blank
  names dropped, every other name kept verbatim because the upstream matches exactly,
  `[]` and never `null` for none). The unit is `routing.ModelMappingCacheRow`; all three
  drivers implement it (memory with a hand-rolled foreign-key existence check). The rows
  are **invalidated when the mapping's `AppModelName` changes** — in the store, in the
  same transaction as `UpdateMapping`/`UpdateMappingEditable`, so a rename the database
  refuses rolls the delete back — and they cascade away with the mapping. The cached
  names are facts about the model the mapping pointed at when they were written.
- **(b) Voices become reachable by two paths.** A gateway provider capability,
  `provider.AudioCppVoicesLister` (`(*OpenAICompatibleClient).ListVoices`, dispatched by
  the `Multiplexer` under a tracing span), reaches a **static** `audio_cpp` server
  directly; it never sends `?model=` (the upstream ignores it) and it **distinguishes a
  genuine empty listing (success, an empty non-nil slice) from a failure** (transport, a
  non-2xx, an undecodable body, all `ErrUnavailable`), which is what lets the writer
  below tell "no voices" from "could not ask". For an **agent-launched** child, which
  listens on a loopback port of the agent's host, the agent's router grows
  `GET /upstream/{model}/v1/audio/voices`: the second endpoint on the `/upstream`
  allowlist beside `/props`
  ([ADR-037](#adr-037--the-runtime-router-grows-a-get-only-per-model-props-passthrough-the-gateway-probes-through-it-with-the-specs-token)),
  **Status-only and never-start** (a cold child answers 404
  `runtime.model_not_running`, never a boot), outbound path exactly `/v1/audio/voices`,
  relayed verbatim, the spec's token forwarded as for `/props`. The allowlist is now a
  table of two endpoints with a **per-endpoint `forwardQuery`**: `/props` keeps the
  inbound query (llama.cpp reads `?model=`), the voices read drops it. The route is a new
  observable agent behavior, so it goes through feature negotiation
  ([ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)):
  the agent declares `runtime_voices` (`Since: "0.11.0"`), ServerAgent moves
  `0.10.2` → `0.11.0` (MINOR, `agent.Features` gained an entry; **one bump for the whole
  branch**), and the gateway names it `gateway.RuntimeVoicesFeature`. The gateway's
  agent-child pass checks it **fail-closed**, the `runtime_upstream_props` shape: an
  older router never serves the voices route and answers a 404 —
  `runtime.upstream_endpoint_not_allowed` from 0.7.0 on, `runtime.model_not_managed`
  before 0.7.0 — so the read cannot succeed there, and the gateway sends it only to an
  agent that declared `runtime_voices`. A server on an agent without the flag has no
  cached voices, which the picker handles fail-open.
- **(c) Population is change-detected fetch-on-load, strict about which model it
  attributes to.** The gateway has no "just became loaded" event, so the health loop
  does the restart-surviving thing the capability writers do
  (`writeCapabilityRows`): on each pass, **while the mapping's model is observed
  loaded**, it asks for the voices and **writes only when the canonical payload differs
  from the stored one**, so a steady server costs a read per tick, never a write. The
  write is for the mapping's **currently loaded model only**, because audio.cpp answers
  for "the loaded model" whatever was asked; asking while another model is loaded would
  attribute that model's voices here. Two producers share one fetch-and-write step
  (`fetchAndCacheVoices`): the static `audio_cpp` pass (`probeAudioCppModels`, which now
  also decodes `loaded` from the same `/v1/models` read) and the agent-child pass
  (`probeAgentVoices`, gated on the agent's loaded-model report, the spec kind `audio_cpp`
  and the `runtime_voices` flag, with the per-mapping `SpecUpstreamAuth` credential and
  the target `<router>/upstream/<model>`). **The write policy:** a lister **success**,
  a genuine empty one included, is written when changed; a lister **error**, and a cold
  child's 404, writes nothing and keeps the row (last known good), and opens a 5-minute
  per-mapping back-off (`audioCppVoicesBackoff`, cleared by a success or by the model
  leaving the loaded set) so a server without the listing is not asked every tick. **Only
  a plain tts model is asked**: `(speech, yes)` and **not** `(speech_clone, yes)`
  ([ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
  (h), #252): the static pass reads that off the very listing it just fetched, the
  agent pass off the stored rows through `routing.CapabilityRowsPlainTTS`, the one rule
  the read side applies too. A clone model is driven by a reference recording and has no
  named voices; a non-speech model has none at all.
- **(d) Two read endpoints over one union core.** `GET /v1/audio/voices?model=<gateway
  model name>` (alias `/openai/v1/audio/voices`; `handleAudioVoices`) is the public one:
  **bearer or the internal loopback pair, never the session cookie**, scope `gateway:use`
  or `llm:invoke`, the speech endpoints' own auth leg
  ([ADR-006](#adr-006--two-auth-modes-chat-completions-also-session-reachable)). It
  **reads the cache and nothing else** — no model is loaded, no upstream is asked — and
  answers `{"model": "<as asked>", "voices": [...]}`, the union across everything the name
  resolves to, a group name expanded to its members. `?model=` selects which cache rows to
  read; the upstream's own parameter is moot. Its visibility gate is
  **routing-consistent** (`Resolver.ReachableCandidates`: the group expansion, the
  locked/direct rule, resource-group provisioning), preceded by the token's own model
  override and a service token's allowlist, and followed by the **current** plain-tts
  verdict at read time (a row that outlived a flip to `speech_clone` or `speech: no` offers
  nothing). Every "nothing to show" case — an unknown name, a model the token cannot
  route to or may not use, a non-speech or clone model, nothing cached yet — is the same
  **200 with an empty `voices`** (no-leak: the answer cannot probe which models exist); a
  missing or blank `model` is **400 `model.name_required`**, and a store failure is
  **500 `model.voices_failed`**, never read as empty. A browser cannot use that endpoint,
  so the picker has its own **session-cookie** route, `GET /api/portal/model-voices?name=`
  (`handlePortalModelVoices`, `requireWebScope(gateway:use)`, a `name` query parameter like
  `model-image-options`), under the **portal's** visibility rule
  (`Portal.ModelVoiceMappingIDs`). **The rule, precisely:** hidden/locked suppression
  (`modelServersSuppressed`, a non-admin only) applies to the **requested name only**,
  the group's own name included; the **members of a group are filtered by provisioning
  alone**, so a locked member's voices are listed through its group (a hidden or locked
  model is still a full member of a group, and `locked` means "reachable through a group,
  not directly"). Both routes share one core, `cachedVoicesForMappings` (the current
  verdict gate, the cache read, a Warn-and-skip for an undecodable row, dedupe, sort), and
  differ only in who may see which mapping. Voices stay out of `/v1/models`.
- **(e) A chosen voice routes to a server that has it — and the filter is fail-open.** The
  speech handler **sniffs `voice`** from the raw body (`sniffSpeechVoice`: a non-blank JSON
  string, exactly as sent and not trimmed, else none) into a dedicated
  `inference.Request.RequiredVoice` field, deliberately **not** a capability token: a
  capability fails closed, and an existing bearer client sending a voice the gateway never
  heard of, or hitting a cache that was never filled, must keep working as it always did.
  The rule — **unknown is not absent**: a candidate is *known-present* when its cached
  list contains the voice (an exact match, on a mapping that is a plain tts model now),
  *known-absent* when it has a cached list that does **not** contain it (a cached-empty
  list included), and of **unknown** voice status when it has no cached voices row at all
  (or an undecodable one). Routing **excludes only the known-absent candidates**;
  known-present and unknown ones stay eligible (fail open), so a healthy same-model
  sibling that was never voice-cached keeps serving as a failover/load target. **When no
  candidate is known-absent, nothing is filtered; and when EVERY candidate is known-absent
  the filter also stands aside** (nothing is known to offer the voice, so the request routes
  unfiltered and the upstream decides) — only a MIX, at least one known-absent alongside at
  least one eligible candidate, actually restricts routing. The single-model path applies it after the
  capability gate and before the admission parameters. A **group** needs a **pre-pass**,
  not a per-member rule (`groupVoiceScope`): `eligibleCandidates` sees one member at a
  time, so a rule "restrict this member if one of *its* candidates lists the voice"
  leaves a voice-less member fully eligible and priority or speed order can then pick it
  ahead of the member that has the voice — the group is never narrowed. The pre-pass is
  computed **once over the flattened group** (`memberPool`, the same provisioning,
  endpoint-mode and capability filters the walk uses, so it cannot count a candidate the
  walk would not see) and carried **outside `GroupPolicy`** (`groupResolve.voice`), which
  is the one thing the relaxation ladder rewrites, so a relaxed attempt can drop
  `loaded_only` and the speed floor but **never relaxes the voice**; it is threaded into
  `eligibleCandidates`, `selectMember`, `orderMembersBySpeed`, `firstAvailable` and
  `climbTarget`. Two exemptions: a **server override** names one server, so there is
  nothing to choose between and the voice does not veto it, and a **sticky pin** is not
  taken for a voiced single-model request (the pin was chosen without the voice and would
  bypass it; no such pin can exist today, so this keeps the contract rather than changing
  a path), while a group's pin passes through the voice-restricted `selectMember`. When the
  voice **narrowed** the pool (excluded at least one *known-absent* candidate while an
  eligible candidate remained) and every remaining eligible candidate is unusable, the
  answer is **`routing.ErrVoiceUnavailable`** → **HTTP 503 `routing.voice_unavailable`**
  instead of the generic `routing.no_healthy_host`: the generic code says the model has no
  host, which is false when other servers of the model are healthy and are known only to
  lack the voice. It fires **exactly then** — and **never** for a voice no candidate is
  known to lack (all known-present or unknown), **nor** for one no candidate is known to
  offer (every candidate known-absent, or every candidate of unknown status): those route
  unfiltered, fail open, and the upstream decides. The two reads the
  pre-pass makes fail **differently, on purpose**: an unreadable **voices cache** fails
  **open** (logged at Warn, routed as if no voice were named — the cache exists only for
  this preference), while a failing capability **verdict** read is **returned**, like the
  capability gate it belongs to.
- **(f) The portal field becomes a combobox bound to the existing `voice` field.**
  `SpeechVoiceField.tsx` (a free-text-capable MUI `Autocomplete`) offers the cached
  names and still accepts any typed name. It is keyed on the **existing** `voice` string
  (the live probe shows no other field consumes a listed name, so `ChatAudioSettings` is
  unchanged and there is no backend change on the run path). The list comes from
  `api.modelVoices` and a `ChatStore` effect shaped like the image-options one: cleared
  first on every model change (a voice belongs to one model), a latest-wins guard against a
  slow stale response, **fail-open** (a failed fetch and an empty answer are the same empty
  list, which leaves the plain free-text field it was before). A saved voice the list omits
  stays shown, a blank is "the model's default" and is never written, and the helper hint
  appears only while there is a list; the label, hint and placeholder are German and
  English together.

**Consequence:** a speech model's voices stay on offer while it is unloaded and across a
gateway restart, and a chosen voice reaches a server that has it. The costs are named.
**A mapping has no row until its model has been observed loaded once** under a gateway
that carries this change (and for up to five minutes after a transient first-fetch failure,
the back-off), and a server on an agent older than 0.11.0 never has one; both read as
**unknown voice status** (not "absent") and stay eligible fail-open. A list is an **offer,
never a constraint**: the cache holds only what the health loop saw, so the field stays
free text, and a voice no cached list is known to offer — a free-text name, or one every
candidate is known-absent for — still reaches the upstream, which answers **500** (not 400)
through the ordinary `normalizeSpeechUpstreamError`. The filter only steers AWAY FROM
servers known to lack the voice while a known-present or unknown one remains; it never
refuses a request no server is known to be able to serve.
**Voices are names only**, because that is all audio.cpp
states; richer display (language, gender) would need a new `cache_type` or a new payload
shape. **A single audio.cpp server holding TWO tts models loaded at once is unverified**:
`/v1/audio/voices` ignores `?model=` and answers for "the loaded model", so two loaded tts
mappings on one server could cache the same voices. The **same root cause** also appears as
a **cross-GET / hot-swap window**: the loaded-model check and the `ListVoices` fetch are
two unsynchronized GETs, so a static multi-model server that swaps its loaded model between
them can have its voices misattributed. Both are harmless on the documented
single-model-loaded deployment, the agent-child path is immune, and the attribution
self-heals on the next tick (the upstream validates the name, so the worst case is a 500,
not silent wrong audio). The common offline-mode case, where
one model is loaded at a time, is correct, and no code guard was built; the follow-up is a
live check with two models
([Risks §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)). A verdict
flip needs no delete, because the read-side verdict gate hides a stale row and the next
load's tts-gated write stops refreshing it.

**Amends:** [ADR-055](#adr-055--per-model-sd-server-image-options-are-read-live-on-pick-from-two-volatile-caches-and-never-stored)
(image options remain volatile and unstored; what changes is that "cache, don't store" is
no longer the only pattern, because a fact obtainable only while the model is loaded must
persist),
[ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only)
(j) (`GET /v1/audio/voices` now exists, as a read of the cache and not a proxy of the
upstream),
[ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs)
(f) (the voice field is a combobox, not free text with no dropdown) and
[ADR-037](#adr-037--the-runtime-router-grows-a-get-only-per-model-props-passthrough-the-gateway-probes-through-it-with-the-specs-token)
(the `/upstream` allowlist holds two endpoints, not exactly `/props`).

**Rejected:** **A volatile cache like ADR-055's** (empty whenever the model is unloaded and
after every restart, which is the whole use case). **Extending `model_mapping_capabilities`**
(a ranked yes/no verdict, not a name list). **Proxying the listing live** (nothing to ask
while unloaded, and for an agent-launched child it would have to start a cold one, which a
listing must never do). **A fail-closed voice filter, or a `voice:<name>` capability
token** (it would break existing free-text bearer clients and an empty or fresh cache).
**A per-member group filter** (it never narrows a group, above). **Relaxing the voice on
the ladder** (serving it from a member without it would be the wrong answer, not a degraded
one). **The browser reading the public endpoint** (it refuses the session cookie, and the
internal pair is blanked at the edge), hence the portal route. **Fetching on a "became
loaded" event** (the gateway has none; the change-detected tick is restart-surviving).
**Writing for a model the mapping does not currently point at, or from an unloaded one**
(it would attribute another model's voices to this mapping). **A code guard for two tts
models loaded at once** (unverifiable on the available server; the limitation is recorded
instead).

**Amended by [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)
(streaming speech synthesis; issues #253 and #263): the cache has more than one consumer.**
Choice (a)'s "voices are the first type" now has a successor: `cache_type =
"output_byte_rate"` (`routing.ModelMappingCacheOutputByteRate`, scope `""`, the payload a plain
decimal integer of PCM bytes per second) holds the rate that turns a streamed speech response's
bytes into audio seconds, written by the speech benchmark and read on the billing hot path and
the portal's streaming gate, and `cache_type = "output_byte_rate_model"` is its companion row
holding the loaded-model identity the rate was measured under. Neither needed a table or a
migration, which is the generality the entry argued for. The rename invalidation stands; what is
added is that a rate is a property of the model, which a rename does not see, so the health loop
retires a rate (an empty payload, read everywhere as no rate) when the loaded-model identity it
observes differs from the companion row's, and re-runs the benchmark. A fact obtainable only
while a model is loaded persisting past an unload is exactly why the rate lives here and not in
a volatile cache.
→ [API Compatibility & Inference
§3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Routing & Model Selection
§2.5](cross-cutting/routing-and-model-selection.md#25-the-voice-filter-speech-only-fail-open),
[Agent-Managed Model Runtime
§4.1](cross-cutting/agent-runtime-manager.md#41-control-routes),
[§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[Persistence](cross-cutting/persistence.md),
[Data Model](reference/data-model.md#the-per-mapping-cache-table),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-068 — Streaming speech synthesis: the gateway relays the PCM stream of a streaming model, meters it from the bytes it relayed (`audio_bytes`, or `audio_second` over a benchmark-learned byte rate), and the portal composer plays it as it arrives
**Context:** `POST /v1/audio/speech`
([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only), issue #68) relays **one complete `audio/wav` body** and bills
`audio_second` from the `X-AudioCPP-Audio-Duration-Ms` response header
(ADR-061 (g)); it refuses `stream` (ADR-061 (e), `speech.stream_unsupported`) because the
relay's "one complete response" contract is exactly what carries that duration. Issue #253
(part 2) wants speech streaming, and issue #263 is its metering prerequisite: a streamed
response carries **no** single-response duration, so a streaming relay built the obvious
way would bill `audio_second` with quantity `0` on every successful call, a loud zero
([ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar)) on the **common** case. The transcription half of #263 had already
shipped: [ADR-065](#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript) bills a streamed transcription from the **uploaded** WAV
header. Speech is the mirror image and has no such input, because the billable audio is
the **output**, which the stream does not describe. A live probe on 2026-10-21 against
audio.cpp v0.9.1 on an operator LAN host (a `tts` model of family `audio8_tts`, 44100 Hz
mono 16-bit) settled what none of this could assume. Five facts about the backend shaped
every choice below, none of them a design's to pick:

- **Streaming needs `mode=streaming`, and such a model is a superset.** With an `offline`
  model every streamed speech request answers `500 {"error":{"message":"speech streaming
  requires a model configured with mode=streaming"}}`, the gate ADR-065 found for
  transcription. A `mode=streaming` model serves a streamed request **and**, for
  `stream=false`, still answers `200 audio/wav` with the duration header. Only `offline`
  is restrictive. `mode` (`offline` | `streaming`) is a field of audio.cpp's `GET
  /v1/models`, beside the `task` ADR-061 (d) reads.
- **A streamed response is headerless PCM, with no duration and no format on the wire.**
  `stream=true` (the default) or `stream_format=sse` answers `text/event-stream`: a
  `speech.audio.delta` per chunk (base64 PCM in `audio`) → `speech.audio.done` (**only**
  `timing.ttft_ms`, no `audio_duration_ms`, two probes) → `[DONE]`. `stream_format=audio`
  answers `application/octet-stream`, chunked, raw PCM (first bytes `e7 ff ea ff`, not
  `RIFF`). Streaming accepts **`response_format=pcm` or absent only**: `wav` while
  streaming answers `500 "streaming speech currently supports response_format=pcm"`, so
  there is never a WAV container, and no `fmt` chunk, on a stream. There is no
  `X-AudioCPP-Audio-Duration-Ms` header either.
- **No API states the output sample rate.** `GET /v1/models/{id}` is 404 and the package
  listing carries no rate. The rate is the model's native one, stable per model and
  unexposed.
- **The duration header is exactly `data_bytes ÷ byteRate`.** The probe's baseline WAV had
  331776 data bytes over a byte rate of 88200, 3.7616 s, which is the header's 3761.633 ms.
  So counting PCM bytes and dividing by the byte rate is the **identical** number audio.cpp
  reports, not an approximation. Streamed raw PCM (319488 B) and the summed SSE deltas
  divided by 88200 gave plausible clip lengths and only 88200 fits (24 kHz would give
  impossible ones), so the **streamed PCM byte rate equals the non-stream WAV byte rate**
  for the same model.
- **The deployment launches streaming models directly as `mode=streaming`.** audio.cpp is
  normally off and is started per run-spec (agent-managed); there are no manual changes
  outside the spec. So the response type, streamed or buffered, is chosen **per request**,
  and the gateway **follows** `mode` and never flips it.

**Decision — six choices, taken together. Every one is a reversal scoped to the streaming
case and leaves the buffered path and every non-streaming metering byte-identical.**

- **(a) A `speech_streaming` verdict, written from `mode` for a speech model only, as a
  PATCH agent bump.** The per-mapping vocabulary gains `speech_streaming`
  (`routing.CapabilitySpeechStreaming`), ADR-065 (b)'s `transcription_streaming` on the
  synthesiser's side: **yes** = the mapped model is `mode=streaming`, **no** = `offline`,
  **absent** = unknown (and no row for any other `mode`). It is written **only beside a
  `(speech, yes)`**, for a `tts` or `clon` model (the axis on which streaming is a
  question for speech, so an `asr` or music model carries no row, not even a `no`; an `asr`
  model's `mode` stays the `transcription_streaming` verdict), from the same per-model `mode`
  of the same `/v1/models` listing and on the same `audiocpp_models` source at probe rank 1.
  A **static** `audio_cpp` application has it written by the gateway's health loop
  (`audioCppCapabilityRows`); an **agent-launched** child has it decoded and reported by
  the ServerAgent's audio collector, riding PR #258's one-sided gateway-declared flag
  `capability_source_audiocpp` as **one new capability name**. So it is a **PATCH** bump,
  ServerAgent **0.11.0 → 0.11.1**, with **no new `agent.Features` entry** (an absent
  verdict already means "buffered", so a flag would gate nothing,
  [ADR-025](#adr-025--agent-capabilities-negotiate-by-named-feature-flags-not-versions)) and **no migration** (the capability vocabulary is open, ADR-062
  (c)); an agent older than 0.11.1 never reports it, which degrades to the buffered path.
  The ingest allowlist (`audioCppProbeCapabilities`) gains the name, it is **not** in
  `reservedAgentCapabilityNames`, and `(speech_streaming, no)` is reserved against a manual
  write and against a probe being outranked by a stale manual override, as every audio
  verdict is. The speech handler reads it keyed on the resolved route
  (`mappingStreamsSpeech`, `MappingCapabilities(ctx, target.RouteID)`, not a new
  `routing.Target` field; the shared `mappingStreamingVerdictIsYes` serves both audio
  endpoints): **yes** and the client asked `stream=true` → stream; **no / absent / a read
  error** → buffered. An unprobed or offline model simply buffers, and a client that asked
  `stream=true` against one gets a valid one-shot WAV, never an error. The agent caches
  verdicts once per process generation, so a UI mode-change without a restart goes stale,
  which is what (b)'s retry exists for.
- **(b) The streamed relay: the refusal is lifted for a stream that asks for PCM, and the
  relay is unbuffered, byte for byte.** `validateSpeechRequest` no longer refuses
  `stream: true`. What stays refused with 400 `speech.stream_unsupported` is a **non-boolean
  `stream`**, a `stream_format` on a request that does not stream or outside the two audio.cpp
  emits (`sse`, `audio`; a JSON `null` is absent), and **`response_format=wav` on a
  stream**, because audio.cpp serves streaming only as `pcm` and 500s a wav stream, so the
  gateway says so up front instead of forwarding a request that cannot succeed. Every other
  `response_format` stays 400 `speech.response_format_unsupported`: `wav` or absent for a
  request that does not stream (unchanged), `pcm` or absent for one that does. `sse` and
  `audio` are **relayed as the client wrote them**, the gateway translating nothing.
  - *The decision.* Taken up front from (a)'s verdict: a `yes` sends `stream=true` upstream
    and relays the event stream or raw PCM **chunked, byte for byte**, through the same
    native copier every native relay uses, with the idle watchdog once bytes flow and a
    **first-byte grace** (the larger of the application's `timeout_ms` and the idle timeout,
    `streamFirstByteBudget`, ADR-065's rule extended to the speech endpoint) so a cold
    `mode=streaming` model's time to first audio is not cut short. A `no`, an absent
    verdict or a read error decides **buffered**: the body is sent without
    `stream`, `stream_format` and `response_format` (`speechBufferedBody`), one upstream
    call, and the client gets the one-shot WAV. That WAV is what a client that sent
    `response_format: pcm` beside `stream: true` is handed by a route that does not
    stream, labelled `audio/wav`; it is the answer that is valid for every model.
  - *The safety net.* A streamed request answered by `500` whose body names `speech
    streaming requires a model configured with mode=streaming` (a `yes` gone stale: a model
    swapped to offline behind the verdict's back) is **re-sent once with `stream` and its
    companions dropped** and answers the buffered WAV. The refused attempt wrote nothing
    and recorded nothing; any other upstream error surfaces as ADR-061 (i) relays it. The
    retry machinery is ADR-065's (`peekStreamingRefusal`, now taking the sentence to look
    for, because the two endpoints' refusals are worded differently).
  - *Two things the stream changes in the relay.* The upstream `Content-Length` that
    ADR-061 (f) forwards is **skipped** for an answer that is a stream (a chunked stream has
    none, and an intermediary's length would not describe it); a stream request answered
    with a plain `audio/wav` keeps it. And the usage scanner stays nil and the tee buffer is
    not filled: a streamed call has no token usage, and megabytes of base64 audio teed for
    a capture that stores a summary would be spent on nothing.
  - *The capture stays a redacted binary summary.* A streamed speech response is audio
    output, so it is captured as the same `{content_type, size, redacted: true}` summary
    every speech response gets (ADR-061 (h)), **not** assembled like a transcription
    (ADR-065 (d)); the base64 is no text a reader of the capture could use. Because the
    stream's `Content-Type` is `text/event-stream`, the capture's endpoint is **stamped
    as speech** (`speechStreamScan.stampCapture`), so the binary-speech backstop
    (`isBinarySpeechResponse`) redacts it instead of storing the event text.
- **(c) A streamed call is metered from the PCM bytes it relayed: `audio_second` over a
  cached byte rate when one is known, the new `audio_bytes` unit when not, and zero in the
  endpoint's own unit for everything else.** A stream states no duration, so the relay
  **counts** what it relays. A tee (`speechStreamScan`, `transcriptionStreamScan`'s
  sibling) is fed every chunk **after** it was written and flushed to the client, so it
  never holds a byte back: for an event stream it sums the decoded base64 `audio` payloads
  of the `speech.audio.delta` events, for raw PCM the bytes of the body. A **clean
  completion** is an event stream's `speech.audio.done` followed by `[DONE]`, or a raw body
  the upstream closed at a clean EOF (a read error is never a clean close); a count the
  scan could not make whole (an unreadable JSON frame, a delta that is not base64, a line
  past its 16 MiB bound) marks it lost and is not a completion, because billing it would be
  a silent under-bill. The measure is then decided in one place
  (`setStreamedSpeechBillingQuantity`):

  | Outcome | Billed |
  |---|---|
  | Clean completion **and** the mapping's output byte rate is cached | `(audio_second, pcmBytes ÷ byteRate)`, the number audio.cpp's own header would have stated |
  | Clean completion, rate **not** cached | `(audio_bytes, pcmBytes)`, an exact count, never a guess |
  | Anything else: an upstream error status, a copy error, an idle timeout, a client that went away, a stream that ended without its terminal events or with a lost count, a stream that carried no audio | `(audio_second, 0)`, uniform with every other speech error row |

  The two failure shapes that would otherwise read as a success that billed nothing (a
  stream that ended without a completion the gateway can count, and one that completed
  carrying no audio) are logged once at `Error`, ADR-041's loud-zero rule; a row that is
  already an error row carries its cause in its own code and is not logged again. **The
  quantity is never a partial count**: a stream cut short delivered some audio and bills
  none, which is also what ADR-065 (c) decided for a partial transcript. **A client
  disconnect seen when the stream terminates is an error row, whatever it cut off**: the
  relay decides the status as soon as the copy returns (`nativeTerminalStatus`:
  `r.Context().Err() != nil` is `provider.client_disconnected`), and the setter returns
  before it reads the rate for any status but `success`, so a hang-up mid-stream **or right
  after the final frame** is `(audio_second, 0)` with that code, never a billed stream. The
  rate is nevertheless read on a context stripped of cancellation (`context.WithoutCancel`);
  that is **defensive**, covering only the window after the status decision (a request
  context cancelled between that decision and the store read), so a cancelled read cannot
  drop a stream the relay had already judged a success to bytes.
  - *Amends [ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar): a new unit, and a success-path measurement-unit
    refinement with failure rows uniform.* `usage.BillingUnitAudioBytes = "audio_bytes"`
    joins the code-level vocabulary (`ValidBillingUnit` accepts it); its quantity is the
    relayed PCM payload in bytes. `ValidateBillingXOR` is generic and needed no change (a
    non-token row zeroes the seven token-denominated columns, and the usage scanner is nil for
    speech), and there is **no migration**: `billing_unit` is an open text column and
    `billing_quantity` a `double`. ADR-041 (c) says a unit may only come from the endpoint's
    identity and never from the response, and it is **preserved in the form that protects
    it**: the unit stays audio-derived, **every failure row stays `audio_second`** (the
    endpoint-identity unit, so a failed streamed request is never mislabelled), and only the
    **success** path chooses between `audio_second` and `audio_bytes`, and it chooses from
    **mapping state** (is the byte rate cached?), not from anything in the response. That is
    a deliberate amendment: a non-token endpoint may carry a success-path *measurement-unit*
    refinement, provided its failure rows are uniform. `audio_bytes` is **unpriceable in Tier
    3**: the modeled tier's guard (`modeledEnergy` answers `unpriceable` for any non-token
    unit) already holds, and ADR-041's rule that a coefficient must carry the unit it declares
    means no modeled figure exists until a coefficient declares `audio_bytes` (the token-blind
    measured and estimated tiers still apply on a host with power telemetry). `NonTokenRequests` counts any
    `billing_unit <> ''` row, so disclosure works unchanged, and the portal's unit chip shows
    the raw value with no tooltip (its help table has no entry, which is honest silence).
- **(d) The output byte rate: a per-mapping value learned by the benchmark, held in the
  generic cache.** The **byte rate** here is the PCM *format* constant, `sampleRate ×
  channels × bytesPerSample` as one integer (88200 B/s for the probed model): the divisor
  that turns PCM bytes into seconds. It is **not** a throughput (`bytes ÷ wall time` varies
  per run, and billing from it would be a wrong bill). It is read from the `fmt` chunk of a
  non-stream WAV answer, which states it and is self-checked (`wavConsistentByteRate`:
  `byteRate == sampleRate × blockAlign`, `blockAlign == channels × ceil(bits/8)`), and it
  equals `data_bytes ÷ (header duration ÷ 1000)`. It is held as a `cache_type` of
  [ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter)'s **generic** `model_mapping_cache`, its second consumer, with **no new
  table and no migration**: `routing.ModelMappingCacheOutputByteRate` (`output_byte_rate`),
  scope `""`, the payload a **plain decimal integer**, read on the billing path
  (`cachedOutputByteRate`, via `routing.ParseOutputByteRate`, the one definition of a
  usable rate: positive and fitting a `uint32`) and on the portal's gate. It inherits the
  foreign-key cascade and the rename invalidation of every cache row. Four properties of
  that rate are recorded, because a wrong rate is a wrong bill, worse than a loud zero:
  - *The rate is never guessed.* It comes from a real non-stream observation or it does not
    exist; an absent, empty or unparseable row reads as "no rate" and the call bills
    `audio_bytes`.
  - *Mono, 16-bit integer PCM.* A byte rate alone cannot say what it is the rate **of**
    (96000 B/s is 48 kHz mono 16-bit and equally 24 kHz stereo 16-bit or 24 kHz mono
    float32), and the portal must play and store a stream as one format. A streamed
    response is OpenAI's `pcm`, signed 16-bit little-endian mono, so the benchmark
    **caches a rate only from a WAV whose `fmt` is integer PCM (tag 1), one channel,
    16 bits** (`wavFormat.isMono16PCM`). Wherever a rate exists, therefore,
    `sampleRate = byteRate ÷ 2`, and a **non-conforming model is simply not rate-cached**: it
    stays on `audio_bytes` and plays buffered in the portal. When a run observes a
    non-conforming WAV, a rate cached earlier for the mapping (by a model since swapped
    behind the name) is **retired** (an empty payload) so a stale rate cannot keep the
    portal's gate open on audio it cannot play.
  - *The streamed rate equals the buffered rate.* A streamed call is billed from a rate
    read off a **non-stream** WAV, so the design assumes audio.cpp streams at the model's
    native rate, the buffered WAV's. It was verified equal for the probed model (both
    44100). It is also **required for the client**: a client receives headerless PCM and can
    only play it at one consistent, known rate, so a stream resampled differently from the
    WAV would break playback before it mis-billed. The named residual is a future model or
    engine that resamples only the stream (Consequence, [Risks
    §11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
  - *A rate is a property of the model, not of the mapping's name.* ADR-067's
    invalidation fires on an `app_model_name` rename and cannot see a model swapped behind
    the same name, so a **companion** cache row, `routing.ModelMappingCacheOutputByteRateModel`
    (`output_byte_rate_model`, scope `""`, the payload the **loaded-model identity** the
    rate was measured under), sits beside the rate and **never inside it**, so the rate's
    payload stays the plain decimal billing parses. The identity is the model file audio.cpp's
    listing states for a static server (`path`) and a fingerprint of the launch spec for an
    agent-launched child (`launch:` plus the first 16 hex digits of a SHA-256 over the
    binary, arguments, environment placeholders, config files and working directory, each
    length-prefixed). The health loop compares the identity it observes with the companion row
    (`ensureSpeechRate`): a rate with no recorded identity (one a manual benchmark cached) has
    the observed identity **adopted**; a rate recorded under another identity is **invalidated
    at once** (an empty payload, so a streamed call bills the honest `audio_bytes` and never
    a wrong `audio_second`) and the benchmark re-run, whatever back-off an earlier trigger
    opened, because a changed model is not a retry; an unstated identity is never taken for a
    change.
- **(e) The speech benchmark: synthesizes straight to the mapping's upstream, learns the
  rate, and records audio perf metrics.** Audio mappings were excluded from every benchmark
  (`benchmark.audio_only`, ADR-061 (b)) because the benchmark engine measures with
  **streaming chat completions**, which a native speech relay cannot reuse. A new mode,
  `POST .../benchmark?mode=speech` (the mapping, application and server scopes of the
  existing starters, `benchmarkModeSpeech`), is modelled on the vision benchmark's shape
  (`runSpeechTarget`: measure, write what was established, append a kind-tagged history
  row). It **inverts the audio-only rule for speech only**: it runs exactly on the plain
  text-to-speech mappings the chat benchmark refuses (`partitionSpeechBenchmarkViews`, the
  `speech: yes` and not `speech_clone: yes` rule of `routing.CapabilityRowsPlainTTS`,
  on a mapping the gateway would route speech to), while the chat modes keep refusing audio.
  A scope with no such mapping answers a new **409 `benchmark.speech_unavailable`**, and an
  application- or server-scope run records the closed skip values **`not_speech`** (a chat
  model, a transcriber, an unprobed model) and **`speech_clone`** (a voice-cloning model,
  whose synthesis needs a server-side reference WAV and mandatory `reference_text`, so a
  plain `{model, input}` request answers 500). **Clone models therefore stay on
  `audio_bytes`**: their streaming still works, since the verdict is written for `clon`
  too, and metering a clone call in seconds is deferred with the rest of the clone epic
  (issue #272).
  - *The mechanism is a DIRECT native call, not a loopback.* The plan was to send the
    synthesis through the internal loopback pair that the portal's speech run uses
    (ADR-063). **As built it is not**: the benchmark calls
    `provider.NativeProxyClient.ProxyNative` on the mapping's own application endpoint, the
    primitive the gateway's own speech relay is built on and the benchmark subsystem's own
    mechanism for addressing a mapping (`benchmarkTargetReq`). Three blockers made the
    loopback unfit: the run **holds the server's reservation**
    (`BenchmarkRegistry.TryStart`), which the resolver honours by excluding the server, so a
    request routed through the gateway would never find the server under test; the loopback
    pair authenticates as a real user and the scheduled run and the health loop's automatic
    trigger have **no principal** to speak as; and the relay answers its client the body,
    `Content-Type` and `Content-Length` only, so audio.cpp's `X-AudioCPP-Wall-Ms`,
    `X-AudioCPP-RTF` and duration headers, which the perf figures use, are **not forwarded**
    (and the relay would record a usage row, which a benchmark is not). A direct call
    reaches a direct `audio_cpp` application and an agent-launched child alike (the agent's
    router starts a child that is not running, as for the chat benchmark), needs no
    credentials of its own beyond the application's upstream token, and reads the upstream's
    own headers.
  - *What it does.* It sends one fixed short non-stream request (`{model, input}`, the
    application's own model id and a fixed sentence, so runs of one model compare), bounded
    by `coldStartBudget` (the larger of `timeout_ms` and the idle budget, long enough for
    the router to start a cold child), accepts only a 2xx `audio/wav` answer, reads the
    byte rate from the WAV `fmt` (the mono 16-bit guard of (d) applies) and writes it, under
    the **mapping id** the billing path reads it by, beside the identity companion once the
    rate is written. A failed measurement writes no rate and leaves a cached one as it was.
  - *Audio perf metrics, and migration 95.* The run appends a **`kind=speech`** history row
    with `ttft_ms`, `wall_ms` and `rtf`, three **nullable** columns added to
    `model_mapping_benchmarks` by **migration 95** (`bigint`, `bigint`, `double precision`
    per [ADR-005](#adr-005--postgresql-needs-wide-column-types); no default and no backfill, so a row that measured none reads as
    NULL, not zero; the store writes NULL for a zero and reads NULL as zero). This is the
    branch's only migration. `wall_ms` is audio.cpp's own `X-AudioCPP-Wall-Ms` when it sent
    one, else the measured elapsed time, and `rtf` its `X-AudioCPP-RTF` or else wall time over
    the clip's length. **`ttft_ms` on this call is not a streamed time to first audio**: the
    request does not stream and audio.cpp answers a non-stream call only once the whole clip
    is synthesized, so it is roughly **synthesis time plus any cold start** (for an
    agent-launched child the router holds the request until the child is ready, so it can far
    exceed `wall_ms`); the portal labels it so. The result and the history DTO carry the three
    figures (and the result `output_byte_rate`).
  - *It runs automatically.* The health loop already visits each audio mapping while its
    model is loaded (ADR-067's voices gate). For a **loaded**, plain-TTS mapping whose
    `speech_streaming` verdict is `yes` it asks the gateway to start the speech benchmark
    (`TriggerSpeechRateBenchmark`, scope `speech-rate`) when `ensureSpeechRate` finds no usable
    cached rate or a rate recorded under another model identity. The loop **never
    synthesizes inline**: the trigger decides, takes the server's single benchmark slot, gives
    it back if the server has in-flight requests (like the scheduled benchmark it never
    competes with live traffic) and launches the run on a background context. Static
    `audio_cpp` applications are triggered from the same `/v1/models` read that states the
    verdict (and `loaded` and `path`); an agent-launched child rides the voices pass's
    eligibility (running, a spec of kind `audio_cpp`, the agent declaring
    `runtime_voices`), so it **never starts a cold child**. A run that was started opens a
    5-minute back-off per mapping (`speechRateRetryBackoff`) so a benchmark that produced no
    rate costs one run per window, not one per tick. The auto-trigger is narrower than the
    manual mode (`speech_streaming: yes`, since the rate is read only by a streamed call and
    the portal's streaming gate); the manual mode runs on every plain-TTS mapping. Until a
    mapping has been benchmarked, its streamed calls bill the honest `audio_bytes`.
- **(f) The portal: the speech run gains a streaming branch, on by default where the
  gateway can honor it.** `executeAudioRun` (ADR-063) keeps its buffered path and gains a
  streaming one, ADR-065 (e)'s shape for the transcription run: the run POSTs `stream=true`
  with `stream_format=sse` (and no `response_format`) over the loopback to the gateway's own
  endpoint, so the relay and its metering are the public endpoint's, not a second audio path;
  reads the SSE (`consumeSpeechStream`); and commits **the same turn as the buffered run**,
  one `audio_url` part holding an assembled WAV that `chat_blobs` stores as before (the
  canonical 44-byte PCM header written from the cached rate, `wavFromPCM16`). It reuses the
  run's reservation, first-delta deadline (armed by the executor and **lifted at the first
  chunk**, the transcription run's arrangement, with the relay's idle watchdog bounding it
  after), TTFT metric and single terminal commit. It commits only a stream that ended with
  `speech.audio.done` and then `[DONE]` and some audio: a stream cut short commits nothing
  (an ended context is reported as itself; anything else is
  `gateway.chat_run_speech_response_unreadable`), and an empty one is
  `gateway.chat_run_no_audio`. The branch also handles a one-shot answer (a route whose
  verdict went stale is answered buffered), read for what it **is**.
  - *Progressive playback.* Each `speech.audio.delta` becomes a new run SSE event, **`audio`**
    (`{"audio": {"seq", "rate", "data"}}`: the chunk's base64 PCM exactly as the stream
    carried it, numbered, with the sample rate, since a stream carries no format). It is
    fanned out to the run's subscribers and **buffered nowhere**: not in the run's text
    buffer, so never in a checkpoint, and a late subscriber is served the terminal turn's
    WAV, not a replay. The browser (`speechPlayback.ts`) decodes the chunks and schedules
    them back to back on one shared `AudioContext`'s own clock with **scheduled
    `AudioBufferSourceNode`s**, which are gapless without an AudioWorklet (a second thread, a
    separately served module and an underrun protocol this does not need). It plays only an
    **unbroken** stream from chunk 0 at one rate, because the run's events are lossy by design
    (a subscriber whose buffer is full is dropped events, an audible gap for audio): the
    first irregularity, a context that is not running (a tab that re-adopted the run without
    a gesture), or a format it cannot play abandons live playback for the run, and the
    committed WAV is the complete audio and is rendered by `AudioTurn` exactly like a buffered
    reply. Live playback is an enhancement that can be abandoned without losing anything.
  - *The gate: stream only where the stream is playable.* The run streams only when
    `portalSpeechStreamRate` says so: **every** mapping a speech request for the model could be
    routed to (the portal-visible plain-TTS ones, `Portal.ModelVoiceMappingIDs`, what the
    voice picker asks) is `(speech_streaming, yes)` **and** has a usable cached rate, **and
    they agree on it**, and the rate is a whole number of 16-bit frames within 8 to 192 kHz
    when halved. A route is picked per request after the gate is asked, so a model with two
    mappings streams only if it makes no difference which serves it. The gate fails closed
    (an empty candidate set, a store error). The run evaluates it **again at run time**
    (`speechRunStreamRate`, for the model the request will route for, the run-as token's
    override applied), never trusting the client, whose toggle is a choice and not an
    authority. Anything else is today's buffered run. So the portal never plays un-playable
    PCM and never needs the `audio_bytes` path: portal streaming and `audio_second` billing
    become available together, both gated on the cached rate.
  - *Default ON for a capable thread, opt-out.* `settings.audio.stream` (a pointer, so an
    unset field and an explicit `false` stay distinguishable) is the thread's choice; only an
    explicit `false` is persisted, so an existing thread's settings stay byte-identical.
    The composer shows a "stream" checkbox, checked by default, only for a model the gate
    says can stream. `stream_format` joins `model`, `input`, `stream` and `response_format` as a
    **reserved** passthrough field (the run states `stream` and `stream_format` itself when it
    streams and omits both when it does not).
  - *New surfaces.* **`GET /api/portal/model-speech-streaming?name=<model>`**
    (`handlePortalModelSpeechStreaming`, session cookie, `gateway:use`, the voices endpoint's
    twin: `name` as a query parameter, blank → 400 `model.name_required`, one no-leak 200 for
    every "no", 500 `model.speech_streaming_failed` for a store failure, never read as a "no")
    answers `{"streaming": true, "sample_rate": <Hz>}` or `{"streaming": false}`; the
    **`audio`** run event; `settings.audio.stream`; a **`speech_streaming` capability chip** on
    the Models view (shown on a `yes` only); and the benchmark section renders the speech
    mode's results (a per-result line, a history table for `kind=speech`, and the
    `not_speech`/`speech_clone` skips).

**Consequence:** streaming speech is a staged, backward-compatible addition, not a new
audio path. The relay, the byte metering with its `audio_bytes` fallback and the
retry-buffered-on-500 net stand on their own (stage 1: a streamed call bills honestly before
any rate exists); the benchmark is what upgrades a mapping from `audio_bytes` to
`audio_second` (stage 2); the verdict is what takes the decision up front (stage 3); the
portal run is the UX (stage 4). Nothing breaks without a later stage or with a stale
one: an absent verdict buffers, an absent rate bills bytes and plays buffered, and a stale
`yes` retries buffered. The non-streaming path, every error row and the buffered portal run
are byte-identical. The accepted costs and residuals are named, not hidden ([Risks
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)):

- **The rate-equality assumption (d).** A streamed call is billed from a rate read off a
  non-stream WAV. It holds for the probed model and is required for client playback, but a
  future model or engine that resamples only the stream would mis-bill; the mitigations are
  the operator's trusted server, a re-check on an audio.cpp upgrade and the identity-change
  re-benchmark.
- **The raw-path completion residual.** SSE proves a clean completion with its terminal
  frames; the raw `application/octet-stream` shape has none, so it is billed from the PCM byte
  count on a **clean HTTP close** (a mid-stream copy error is an error row). That bills
  exactly what the client received, so a partial delivery is no full-amount over-bill, but a
  body the trusted upstream truncates **and** closes cleanly would bill as complete. SSE is
  the default shape and the primary path.
- **Clone models bill `audio_bytes`**, and so does any mapping not yet benchmarked or
  whose model is not mono 16-bit PCM. `audio_bytes` is unpriceable in Tier 3 until a coefficient
  declares that unit, so those calls carry no modeled energy figure (a host with measured or
  estimated power still attributes some).
- **A permanently non-conforming or non-benchmarkable model is retried every five minutes.**
  The back-off is a **fixed** window: a model whose WAV is not mono 16-bit PCM, or whose
  synthesis keeps failing, is benchmarked again every `speechRateRetryBackoff`, writing an
  error history row and holding the server's benchmark slot (which excludes the server from
  routing) for the length of each run. An escalating back-off is the known fix; it is **not
  built** and is recorded as debt.
- **Partial delivery bills nothing.** A client that disconnects mid-stream, or right after the
  final frame, is a `provider.client_disconnected` error row of `(audio_second, 0)`; the audio
  it heard is not billed, matching ADR-065 (c).
- **The portal's live playback is best-effort.** It needs a running `AudioContext`, plays only
  an unbroken stream and abandons at the first gap; the run's fan-out drops events for a
  subscriber whose buffer is full (which a reconnect's fresh snapshot repairs), and the
  executor holds each streaming run's whole PCM in memory as the buffered run held its WAV.

**Amends:** [ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar) (the `audio_bytes` unit; (c)'s "unit from endpoint identity"
now has a success-path measurement-unit refinement with failure rows uniform, above),
[ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only) ((e): streaming is **lifted** for `stream=true` with `response_format=pcm`
or absent, `wav` plus a stream stays a 400 because audio.cpp serves streaming as `pcm` only,
`stream_format` `sse` and `audio` are relayed and a request that does not stream still takes
`wav` only; (f): the upstream `Content-Length` is not forwarded for a streamed answer; (g): a
streamed call's quantity is counted from the relayed PCM, never read from a header; (h): the
streamed capture stays a redacted summary, now by an explicit stamp; (d): the verdict probe
also writes `speech_streaming`; the Consequence's "no streaming" cost is lifted),
[ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs) (the speech run is no longer always one buffered zero-delta request: it
streams where the gate allows, `stream_format` is a reserved passthrough field, and the
run's deadline bounds a streaming run only until its first chunk) and
[ADR-067](#adr-067--a-persistent-per-mapping-cache-first-used-for-tts-voices-fetched-while-the-model-is-loaded-change-detected-and-read-by-a-picker-and-a-fail-open-routing-filter) (the per-mapping cache's second consumers: `output_byte_rate` and its
`output_byte_rate_model` companion; the rename invalidation stays and a loaded-model
identity change now also retires a rate).

**Rejected:** **Translating `stream_format` or the event shape** (audio.cpp emits its own;
the project proxies natively shaped backends and does not translate). **Billing a streamed
call `audio_second` 0 until a rate exists** (a loud zero on the common success; `audio_bytes`
is exact and never guessed). **Guessing the rate, from a constant, from a per-family table or
from `bytes ÷ wall time`** (ADR-041: a wrong rate is a wrong bill, worse than a loud zero;
the rate is observed from a real WAV `fmt` or it does not exist). **Reading the rate from the
stream** (headerless PCM; no API states it). **Billing a partial or aborted stream** (a
partial count is no measurement; the zero is loud). **Assembling the streamed capture like a
transcription** (it is audio, and the base64 is no text a viewer could use). **A synthesis
benchmark through the gateway's own loopback** (the run holds the reservation that excludes
the server, no principal exists for a scheduled run, and the relay forwards none of the
metering headers, (e)). **Triggering the benchmark inline from the health loop** (a tick
must stay cheap; the trigger only launches). **Storing the identity inside the rate's
payload** (a billing-hot-path parse of a structured payload, and a row that could be mistaken
for a rate; the companion row keeps the rate a plain decimal). **A new table or a migration
for the rate** (the generic cache is exactly for a fact learned while the model is loaded).
**A new `agent.Features` flag for the verdict** (an absent verdict already means the correct
buffered behaviour, ADR-025). **Flipping a model's `mode` from the gateway** (mode is
run-spec and config; the gateway only reads and follows it). **Playing the portal stream
without a known rate or with an assumed one** (a wrong rate plays and stores audio at the
wrong speed). **An AudioWorklet for playback** (scheduled buffer sources are gapless with
none of its machinery). **A second audio code path for the portal** (the run is the
existing speech run with a stream branch).

**Amended by [ADR-069](#adr-069--non-stream-speech-pcm-is-a-wav-header-strip-not-a-transcode-the-gateway-relays-the-data-chunk-of-a-16-bit-mono-wav-as-headerless-pcm-refuses-any-other-clip-after-synthesis-and-leaves-mp3opusflacaac-to-264)
(non-stream `pcm`; issue #253 part 1).** (b)'s validation line for a request that does not
stream, "`wav` or absent (unchanged)", now reads `wav`, `pcm` or absent: a one-shot `pcm`
request is served as the upstream WAV's `data` chunk, headerless. The buffered downgrade of a
`stream: true` request (a route not known to stream, or the stale-`yes` retry) is left exactly
as decided here and still answers a one-shot `audio/wav`, whatever `response_format` it named.
→ [API Compatibility & Inference
§3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech),
[§7](cross-cutting/compatibility-and-inference.md#7-streaming-lifecycle),
[§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground),
[Routing & Model Selection
§2.3](cross-cutting/routing-and-model-selection.md#23-the-capability-gate),
[Telemetry, Usage Analytics & Observability
§8.4.1](cross-cutting/telemetry-usage-observability.md#841-the-usage-event),
[Security, Authentication & Authorization
§14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[Agent-Managed Model Runtime
§7](cross-cutting/agent-runtime-manager.md#7-feature-negotiation),
[Runtime View §6.1.1](06-runtime-view.md#611-speech-relay-post-v1audiospeech),
[§6.5](06-runtime-view.md#65-model-selection-benchmark),
[Data Model](reference/data-model.md#4-migration-history-95-migrations),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Glossary](12-glossary.md),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).

## ADR-069 — Non-stream speech `pcm` is a WAV-header strip, not a transcode: the gateway relays the data chunk of a 16-bit mono WAV as headerless PCM, refuses any other clip after synthesis, and leaves mp3/opus/flac/aac to #264
**Context:** `POST /v1/audio/speech` ([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only), issue #68) was made WAV-only on
purpose: audio.cpp answers `mp3`, `opus`, `flac`, `aac` and `pcm` with a WAV it still labels
`audio/wav`, so relaying one of them would hand a strict client the wrong bytes without
saying so (ADR-061's Context and (e)). [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives) lifted the streaming half (issue #253
part 2). Issue #253 part 1 asks for the other half, **non-WAV output formats on a request that
does not stream**, on the premise that audio.cpp now honors `response_format`. A **live
re-verification on 2026-10-22** against audio.cpp v0.9.1 on an operator audio.cpp LAN host (a
`tts` model of family `audio8_tts`, 44100 Hz mono 16-bit) disproved the premise and left
ADR-061's Context finding exactly where it was:

- **`mp3`, `opus`, `flac`, `aac` and `pcm` all return a plain 44100 Hz mono 16-bit WAV**:
  `Content-Type: audio/wav`, first bytes `RIFF…WAVE`, `fmt` tag 1, `data` bytes ÷ 88200
  equal to `X-AudioCPP-Audio-Duration-Ms` exactly, and the full `X-AudioCPP-*` metering
  headers. The differences in size and duration between the probe's responses were TTS
  synthesis non-determinism, not different formats.
- **`json` and `b64_json` answer `application/json` `{"audio":"<base64 WAV>"}`** with **no**
  metering headers.

So the backend **ignores `response_format` on the binary path and has no encoder**. Metering
was never the blocker; the missing encoder is. A compressed format cannot be relayed, because
the backend never produces one, and producing one in the gateway is **re-encoding**, which
[ADR-063](#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs) ("the gateway never converts or re-encodes") and
[ADR-064](#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn) deliberately reject and which issue **#264** (opt-in server-side
transcoding) owns. What *is* deliverable without an encoder is raw PCM. The WAV audio.cpp
returns is signed 16-bit little-endian PCM inside a 44-byte RIFF/WAVE wrapper, and a wrapper
is a container, not a codec: taking it off leaves the samples the backend synthesized, byte
for byte.

**Decision — five choices, taken together. The reversal is scoped to one value on one path;
everything else is byte-identical.**

- **(a) Only `pcm`, and only on a request that does not stream, is lifted.**
  `validateSpeechResponseFormat` now admits **`wav`, `pcm` or an absent value** on a request
  that does not stream. `mp3`, `opus`, `flac`, `aac`, `json`, `b64_json` and any non-string
  value stay 400 `speech.response_format_unsupported` (the message now names the accepted
  set), because the gateway cannot honor them honestly: there is no encoder behind the first
  four (they move to #264), and the last two carry no metering header (ADR-061 (e)). The
  streaming branch of the validation is untouched (`pcm` or absent; `wav` beside a stream is
  still 400 `speech.stream_unsupported`). The strip is keyed on the **client's own stream
  intent**, not on the route: a `{stream: true, response_format: "pcm"}` request that
  ADR-068 (b) answers buffered (a route not known to stream, or the stale-`yes` retry) is
  still the one-shot `audio/wav` it always was, because `speechBufferedBody` drops
  `response_format` and that answer is ADR-068 (b)'s, "valid for every model". The new
  contract belongs to a request that did not ask to stream.
- **(b) A header strip, not a transcode.** For a one-shot `pcm` request
  (`sniffSpeechResponseFormat`, read tolerantly from the body validation already admitted)
  the gateway forces the **upstream** `response_format` to `wav` (`speechPCMUpstreamBody`:
  every other field kept exactly as written, a number keeping its full precision) and marks
  the relay `speechPCMStrip`. The 2xx path of `proxyNative` then reads the WAV header from
  the upstream body through a counting reader with **`wavStripToData`**, which is
  [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives)'s `wavduration.go` chunk walker (RIFF check, foreign chunks and the
  tail of a longer `fmt ` chunk skipped, pad bytes honoured, the `fmt ` decode and its
  self-consistency check shared with `wavFormatFromReader` through `wavFormatFromFields`)
  stopped at the first byte of the `data` chunk instead of read to its end. It relays exactly
  the `data` chunk's declared size (an `io.LimitReader`) through the **ordinary native
  copier** as `Content-Type: application/octet-stream` with `Content-Length` set to the data
  size. The header is read **before** the client's response headers are written, because the
  length the client is told must be the data size and not the WAV's; the upstream's own
  full-WAV `Content-Length` is therefore not forwarded for this answer (it would overstate a
  headerless body). The strip is the exact inverse of ADR-068's `wavFromPCM16` (which *adds*
  the 44-byte header to streamed PCM), and ADR-064 itself calls the WAV encoder "a 44-byte
  header": there is **no decode, no resample and no codec**, so it stays inside
  ADR-063/ADR-064's "the gateway never re-encodes". Real compressed formats need an encoder
  and are issue #264.
- **(c) A conformance gate, `isMono16PCM`, and a post-synthesis failure code.** Raw PCM is
  only honest if the clip *is* signed 16-bit little-endian mono, the one definition of
  "conforming" ADR-068 (d) already uses (`wavFormat.isMono16PCM`: integer PCM tag 1, one
  channel, 16 bits). The clip is relayed only when **all** of these hold: the header parsed
  (`wavStripToData` succeeded), the format is mono 16-bit PCM, the upstream declared a usable
  `Content-Length`, and the header bytes read plus the declared data size fit within that
  length. Anything else is **502 `speech.pcm_unavailable`**, an OpenAI-shaped error object
  (`type: upstream_error`, a message naming the model and the format and never the
  operator's server) written *in the copier's place*, so **no partial PCM and no lying
  `Content-Length` is ever sent**: the check completes before the first client byte. Three
  inputs reach it: a non-16-bit-mono WAV from an exotic future model, a structurally unsound
  or over-declared WAV (a streaming writer's `0xFFFFFFFF` data size, say), and a 2xx with no
  usable `Content-Length` (the overrun guard needs the declared length to bound the data
  chunk; unreachable with the trusted upstream, which always declares one, ADR-061 (f)). The
  format is known only once audio.cpp has answered, so this is a **post-synthesis** code:
  it is not the preflight `speech.response_format_unsupported` (the client's request was
  valid, and that code would mislead it) and not a 400 (a backend limitation, not the
  client's fault), and `validateSpeechRequest` never returns it. The row is an `error` row
  billed **`(audio_second, 0)`**, uniform with every other failed-relay row (ADR-061 (i)),
  even though the operator did pay for the synthesis; billing the produced duration on a
  request the client was told failed is rejected, a failure row being a failure row. The
  probed model always conforms, so today the gate guards only a model or engine that does
  not.
- **(d) Metering is unchanged.** `setSpeechBillingQuantity` reads the **upstream** WAV's
  `X-AudioCPP-Audio-Duration-Ms`, which the strip leaves in the upstream response headers, so
  a non-stream `pcm` call is billed `audio_second` from the header, exact, on a clean
  `success`, exactly as a `wav` call is ([ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only) (g)). It never uses
  `audio_bytes` (that is the streaming fallback for a response with no duration, and a
  non-stream `pcm` response always has the header), and every failure row, `pcm_unavailable`
  included, is `(audio_second, 0)`. No new unit ([ADR-041](#adr-041--a-billable-measure-is-a-unit-quantity-pair-never-a-scalar)'s pair is untouched).
- **(e) Capture is unchanged, and the contract is headerless.** The response is
  `application/octet-stream`, not `audio/*`, so ADR-061 (h)'s type gate does not fire; the
  **binary-speech backstop** (`isBinarySpeechResponse`: speech flavor, not a transcription,
  2xx) redacts it to the `{content_type, size, redacted: true}` summary. No endpoint stamp is
  needed on this path (the stamp is a streaming-only device, ADR-068 (b)): the non-stream
  relay leaves `Endpoint` at its zero value, which is not a transcription, so the backstop
  already matches, and a test pins it. The `pcm_unavailable` error is `application/json` and
  is captured like any other error body. The answer is **headerless by contract**: the client
  receives signed 16-bit little-endian mono PCM at the model's **native rate** (44100 Hz for
  the probed model, not the fixed 24 kHz OpenAI documents for its own `pcm`) and learns that
  rate out of band, as for ADR-068's `stream_format=audio`; the gateway adds no header to
  state it.

**Consequence:** a small, backward-compatible addition with no new moving part. **No
migration, no ServerAgent version bump and no new billing unit**; the buffered `wav` path
(and an absent `response_format`), the streaming path and its downgrade, the transcription
endpoint and every other flavor are byte-for-byte unchanged, and the portal is untouched (its
buffered speech turn stays WAV, and its streaming run already uses PCM internally). The costs
and residuals are named ([Risks
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances)):

- **Native rate, headerless.** A client that asked for `pcm` must know the model's sample rate
  out of band; nothing on the wire states it, and it differs from OpenAI's fixed 24 kHz. This
  is the contract ADR-068 already accepted for a stream.
- **A `pcm` request differs by whether it streams.** `{response_format: "pcm"}` without
  `stream` is headerless PCM, while `{stream: true, response_format: "pcm"}` to a route that is
  not known to stream is still a one-shot `audio/wav` (ADR-068 (b), unchanged). A client that
  needs a deterministic answer should not send `stream: true`.
- **A non-conforming clip is a 502 that bills zero.** A model whose native audio is not 16-bit
  mono PCM cannot serve `pcm`; the operator absorbs that synthesis cost on each such request.
  It is a safe failure, never a truncated body.
- **A 2xx without a usable `Content-Length` is refused**, because the overrun guard needs the
  declared length. Safe, and unreachable with the trusted upstream.
- **`mp3`, `opus`, `flac` and `aac` stay refused** until #264 decides where a transcoder lives
  (gateway or agent, opt-in per application or run-spec); this entry makes the backend's
  absence of an encoder, rather than a metering gap, the stated reason.

**Amends:** [ADR-061](#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only) ((e): a request that does not stream now accepts `wav`,
`pcm` or an absent `response_format`, where it accepted `wav` or absent only; `mp3`, `opus`,
`flac`, `aac`, `json` and `b64_json` stand refused. (g) is **unchanged**: the duration
header still meters a `pcm` answer. (f) is carved for this answer: its `Content-Length` is
the data chunk's, not the upstream's). [ADR-068](#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives) ((b)'s validation line, "`wav` or
absent for a request that does not stream (unchanged)", now reads `wav`, `pcm` or absent; its
buffered downgrade of a stream request is left exactly as decided).

**Rejected:** **Relaying audio.cpp's substituted WAV under a compressed label** (`mp3`,
`opus`, `flac`, `aac`: the very anti-goal ADR-061 (e) named, a strict client handed the wrong
bytes without being told). **Transcoding to `mp3`/`opus` in the gateway** (re-encoding;
ADR-063 and ADR-064's rejection stands, and #264 owns the opt-in design). **Relaying `pcm` as
the WAV the backend sends** (the client asked for a headerless format and would be handed a
container instead: wrong bytes, the failure ADR-061 (e) refused for `mp3`). **Billing a rejected
clip by its produced duration** (a failure row is a failure row, ADR-061 (i)). **Reusing
`speech.response_format_unsupported` for a clip that is not 16-bit mono** (the request was
valid; the failure is the backend's and is known only after synthesis). **Streaming the
header-stripped body without reading the header first** (the `Content-Length` must be the data
size, and a clip that cannot be served must be refused before the first byte). **A new billing
unit or a response header stating the sample rate** (the duration header already meters it,
and a rate header is a new contract no client asked for). **A portal format picker** (`pcm`
is a raw-API niche; the portal's buffered turn stays WAV and its streaming run already plays
PCM).
→ [API Compatibility & Inference
§3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech),
[Runtime View §6.1.1](06-runtime-view.md#611-speech-relay-post-v1audiospeech),
[Security, Authentication & Authorization
§14](cross-cutting/security-auth-rbac.md#14-capture-redaction-of-sensitive-headers),
[HTTP API Surface](reference/api-surface.md#1-inference--compatibility-endpoints),
[Risks & Technical Debt
§11.4](11-risks-and-technical-debt.md#114-deliberate-design-acceptances).
