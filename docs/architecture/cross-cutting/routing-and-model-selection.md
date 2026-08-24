# Routing & Model Selection

How a client's `(model, api_flavor)` request turns into a concrete
`scheme://domain:port` call to an Ollama, llama.cpp, or vLLM application, how
candidates are scored, and how the system learns each mapping's speed and safe
concurrency over time.

## 1. Data model

Routing is entirely data-driven: three store-backed types compose into the
candidate a request can be served from.

```mermaid
erDiagram
    AI_SERVER ||--o{ APPLICATION : hosts
    APPLICATION ||--o{ MODEL_MAPPING : serves
    MODEL_GROUP ||--o{ GROUP_MEMBER : orders

    AI_SERVER {
        string domain
        string status "active/disabled/maintenance"
        string health_status "unknown/healthy/degraded/unhealthy"
    }
    APPLICATION {
        string type "ollama/vllm/llama_cpp/llama_swap/litellm/mock"
        int port
        string scheme "http/https"
        int priority
        int weight
        int timeout_ms
        int affinity_ttl_seconds
        int admission_queue_timeout_seconds
        bool benchmark_schedule_enabled
        bool opportunistic_metrics_enabled
    }
    MODEL_MAPPING {
        string gateway_model_name
        string app_model_name
        float gen_tokens_per_second
        float prompt_tokens_per_second
        int context_size
        bool is_mtp
        int max_concurrency
        int recommended_concurrency
        float gen_tokens_per_second_at_capacity
        bool metrics_locked
    }
    MODEL_GROUP {
        bool loaded_only
        string member_order "priority/speed"
        int climb_speed_margin_percent
        float min_tokens_per_second
        string min_speed_fallback "error/ignore"
    }
```

A **`MappingCandidate`** (`internal/routing/store.go`) is the join of the three:
one routable path for a public *gateway model name*. `Store.ActiveMappingsForModel(ctx,
gatewayModel, apiFlavor)` returns every candidate whose mapping is
`status=active`, whose application serves `apiFlavor`, and (implicitly) whose
server exists — health/enablement are filtered later, in the resolver, not the
store query.

`ApplicationEndpoint(server, app)` (`internal/routing/store.go`) composes the
reachable base URL: `scheme://domain:port` plus the server's and application's
optional path suffixes. One exception — the P4 gateway-guided HTTPS switch: when
an application is flipped to proxied HTTPS (`Scheme=="https"` and
`ProxyListenPort != 0`), the reachable origin becomes the agent's local
TLS-terminating proxy listener instead of the plain upstream `Port`; this is
transparent to routing (`Target.Endpoint` is already resolved) and detailed in
[Certificates & TLS](certificates-tls.md).

Applications also carry the routing-tuning fields used below: `Priority` and
`Weight` (scoring), `TimeoutMS` (per-request upstream timeout), a routing
tuning validation rejects negative values for all of them at write time. The
provider dispatch mapping (`cmd/gateway/main.go: providerClients`):

| `AIServer.Provider` / `Application.Type` | `provider.Client` implementation |
|---|---|
| `mock` | `provider.NewMockWithDelay` (dev/test) |
| `ollama` | `provider.NewOllamaClient` |
| `vllm`, `llama_cpp`, `llama_swap`, `litellm` | `provider.NewOpenAICompatibleClient` (shared — all four speak the OpenAI HTTP dialect) |

`ModelGroup` also carries five selection-setting columns, added append-only by
migration `62`: `loaded_only`, `member_order`, `climb_speed_margin_percent`,
`min_tokens_per_second`, `min_speed_fallback` — all defaulting to today's
behavior. §5 documents what each one does and how they combine.

## 2. Resolution pipeline

`Resolver.Resolve` (`internal/routing/resolver.go`) is the single entry point,
invoked once per inference request with the caller's `auth.Token` and the
translated `inference.Request` — whose `Model` field the edge has already
derived from the client's requested name, per token (§2.1).

```mermaid
flowchart TD
    Start["Resolve(token, req)"] --> Override{"req.ServerOverrideID set?"}
    Override -->|yes| SO["resolveServerOverride:\nforce exact server,\nbypass groups/affinity/provisioning"]
    Override -->|no| IsGroup{"req.Model is an\nactive ModelGroup?"}
    IsGroup -->|yes, locked| ErrLocked["ErrNoModelRoute"]
    IsGroup -->|yes| Group["resolveGroup:\npriority walk / sticky pin\n(see §5)"]
    IsGroup -->|"no, but locked (group-only)"| ErrLocked
    IsGroup -->|no| Affinity{"token pinned via\nRouteAffinity and\nstill valid + provisioned?"}
    Affinity -->|hit| ReuseTarget["reuse pinned Target;\ntouch session reservation"]
    Affinity -->|miss| LoadCands["Store.ActiveMappingsForModel(model, flavor)"]
    LoadCands --> FilterProv["filterProvisioned\n(resource-group gate)"]
    FilterProv --> Select["selectCandidate (see §3)"]
    Select -->|all candidates at\neffective cap| Queue["AdmissionController.WaitForSlot\n(see §6)"]
    Queue -->|slot signalled / recheck tick| Select
    Queue -->|timeout / full / ctx done| ErrQueue["503: ErrAdmissionQueueTimeout\n/ ErrAdmissionQueueFull"]
    Select -->|no reachable/viable candidate| ErrHost["ErrNoHealthyHost"]
    Select -->|picked| Build["targetFrom(server, app, mapping)"]
    Build --> Pin{"AffinityTTLSeconds > 0\nand token.ID set?"}
    Pin -->|yes| Upsert["UpsertAffinity + reservation.touch"]
    Pin -->|no| Dispatch
    Upsert --> Dispatch["Target{Endpoint, ProviderModel,\nTimeout, APIToken, ...}\n→ provider.Multiplexer"]
    ReuseTarget --> Dispatch
    SO --> Dispatch
```

Four branches are mutually exclusive and checked in this order: **server
override** (an explicit, pre-authorized escape hatch used by the portal's
"route to this server" action — it still requires the target to actually offer
the model and be enabled/reachable, unless the caller also forces through an
unreachable one), **model group** dispatch (§5), **route affinity** reuse, and
finally fresh candidate selection.

`filterProvisioned` drops candidates whose server the principal is not
authorized to use under resource-group provisioning (a nil `ProvisioningGate`,
the default, is a no-op — every candidate passes). This is the only place
authorization intersects routing; see
[Security, Authentication & Authorization](security-auth-rbac.md) for the gate
itself.

### 2.1 Per-token model resolution

Before the resolver sees anything, the edge decides **which name to resolve**.
`inferencePreflight` (`internal/gateway/inference_handlers.go`) is the single
place that happens — once per client request, for the translate handlers and
for native passthrough alike — and it walks a four-step chain:

| # | Step | Source | Applies when |
|---|---|---|---|
| 1 | Exact override row | `token.ModelOverrideRules[requested].To` | the token has a row for exactly this requested name |
| 2 | Catch-all override | `token.ModelOverride` | no row matched and the catch-all is set |
| 3 | Unknown-model redirect | `token.LastUsedModel`, then `token.UnknownModelFallback` | the token opted in **and** the name steps 1–2 produced does not apply (below) |
| 4 | — | — | nothing claimed the name: the request keeps it and fails exactly as it would without the redirect |

Steps 1–2 are `resolveModelOverride`; step 3 is `redirectUnknownModel`
(`internal/gateway/inference_redirect.go`). Step 3 judges the **effective**
name steps 1–2 produced, so an override that already resolves to a usable
model ends the chain there. The catch-all has no requested name of its own —
it is a single string, not a row — and therefore carries none of the per-row
listing switches (`offer`/`hide_target`, see
[API Compatibility & Inference §9](compatibility-and-inference.md)).

```mermaid
flowchart TD
    Req["client's requested model\n(inferencePreflight)"] --> Row{"exact override row\nfor this name?"}
    Row -->|yes| Eff["effective model"]
    Row -->|no| Catch{"catch-all\nModelOverride set?"}
    Catch -->|yes| Eff
    Catch -->|no| Eff
    Eff --> Opt{"UnknownModelRedirect\nopted in?"}
    Opt -->|no| Gates
    Opt -->|yes| Applies{"does the effective name apply?\n(Callable; widened by\nUnknownModelRedirectBlocked)"}
    Applies -->|yes| Gates
    Applies -->|no| Marker{"LastUsedModel\ncallable?"}
    Marker -->|yes| UseMarker["redirect onto the marker"] --> Gates
    Marker -->|no| Fallback{"UnknownModelFallback\ncallable?"}
    Fallback -->|yes| UseFB["redirect onto the fallback"] --> Gates
    Fallback -->|no| Keep["keep the requested model\n(today's error)"] --> Gates
    Gates["admission gates, unchanged:\nserver-override re-authorization,\nservice-token model allowlist,\nrate/quota/budget\n→ Resolver.Resolve (§2)"]
```

**The invariant.** The redirect target passes every admission gate exactly as
if the client had named it. The redirect changes *which* name is requested,
never *what* a token may reach. That is why step 3 sits where it does — before
`applyServerOverride`, `modelAllowed`, and `admitPrincipal`, not after them —
and why it is never terminal: with nothing usable to redirect onto, the
request keeps its model and produces the same error it always did.
`RequestedModel` still carries the client's original wish, so the usage event
records both names (see
[Telemetry, Usage Analytics & Observability](telemetry-usage-observability.md)).

**What "does not apply" means** has two widths, chosen per token:

| `UnknownModelRedirectBlocked` | The effective name is redirected when it is… |
|---|---|
| `false` (default) | not callable **and** not in `Existing` — i.e. there is no such model at all |
| `true` | not callable, full stop — including a model that exists but this token may not use |

The narrow default is deliberate: a refusal on a model that *does* exist is a
signal about a misconfiguration, and silently routing around it costs whoever
debugs it later. The widened mode is the explicit opt-out from that.

**Cost and failure direction.** A token with the redirect off pays one boolean
test and no store work at all; only an opted-in token triggers the offering
lookup (one mapping traversal plus one group-overlay load per request,
uncached). `ModelOfferingFor` is **all-or-nothing**: on any store error every
set comes back empty, every candidate then reads as uncallable, the chain
declines, and the client sees today's ordinary error rather than a request
sent somewhere unintended. This is deliberately the opposite of the model
listing's fail-open, which must never blank the list a user is looking at.

**Configuration-time guard.** The catch-all, every rule's target, and the
redirect's fallback are all validated on write against one set — what the
**writing principal** can route to directly (`callableModelNames`,
`internal/portal/service.go`) — and an unroutable name is rejected with
`400 portal.token_model_override_invalid` on every token-write path there is:
user-token create and update, and service-token create (a service token has no
update path at all, so its model settings are fixed at creation). For a service
token that principal is the one **issuing** it — a service delegate or an
authorized admin-group manager — never the service itself: a token that does
not exist yet has no reachability of its own to compute. That is
safe because the check is a usability guard, not the enforcement point:
candidates are re-checked against the live offering on every request, and the
result still faces the service's own allowlist, so a value that goes stale — or
one the service may not use — is inert rather than dangerous.

**The last-used-model marker.** Every token records the gateway model or group
name of its last **successfully routed** request (`api_tokens.last_used_model`).
`Server.resolveTarget` (`internal/gateway/inference_resolve.go`) is the single
seam all three inference paths (`complete`, `tryProxyNative`, `beginStream`)
resolve through, so the marker is written in exactly one place. It is written
**only when the value changes** (the token row is already written on every
authentication; a second unconditional write per request would double that
load for no gain), **never on a failed resolve** (a typo or a dead model must
not become a token's redirect target), and a write error is logged and
swallowed — the marker is a convenience, never a reason to fail a request that
already has a live target. Over the portal API the marker is **read-only**: it
appears on token DTOs and on no request body, because a writable marker would
hand a client control over where its own unknown requests go.

### 2.2 Offered, callable, existing

`portal.ModelOffering` (`internal/portal/service_model_offering.go`) answers
the redirect's questions with three deliberately distinct per-flavor sets.
Confusing any two of them produces a wrong redirect:

| Set | Question it answers | Per-token reach | `hidden` names | `locked` names | override aliases |
|---|---|---|---|---|---|
| `Offered` | what a listing shows this token | applied | dropped | dropped | overlaid |
| `Callable` | what this token can route to directly | applied | **kept** | dropped | not applied |
| `Existing` | what exists at all | ignored | kept | kept | not applied |

`Callable ⊆ Existing`. `Offered` is neither a subset nor a superset of
`Callable`: it loses the suppressed names and gains the token's own aliases,
which are rewritten before routing and are therefore not routable names.

The split between the two `model_settings` suppression values is the
load-bearing part:

- **`hidden` is display suppression only.** The name drops out of listings and
  still routes perfectly under that same name, so it stays in `Callable`.
- **`locked` is a real access boundary.** The name is group-only
  (`GroupRegistry.DirectAllowed` is false, and the resolver turns that into
  `ErrNoModelRoute`, §8) — for a group name exactly as for a model name — so it
  is not callable. It stays in `Existing`, which is precisely what makes it the
  "exists but you cannot call it" case widened mode is for.

**The listing is a display, never an access control.** A name suppressed from
a token's listing — by `model_settings`, or by an override row's `hide_target`
(§2.1, and [API Compatibility & Inference §9](compatibility-and-inference.md))
— stays callable under its real name, exactly as before these switches existed.

The redirect therefore asks **both** its questions of `Callable`, never of
`Offered`: reading `Offered` would make widened mode fire on a request the
token was entitled to serve and reroute it away from a working model, and
would defeat a catch-all whose target happens to be hidden. `Existing` is
built without the per-token filter and without the listing switches, because
only that separation lets the redirect tell "no such model" from "not yours";
groups share the model namespace, so an active group's own name is in it too.

## 3. Candidate scoring

`selectCandidate` (`internal/routing/resolver.go`) narrows the candidate pool
through a fixed chain of filters, **each of which fails open**: if a filter
would empty the pool (or leave only non-viable survivors), routing falls back
to the pre-filter pool rather than refuse a request a degraded server could
still serve. Order matters — later filters see only what survived earlier ones:

| # | Filter | Rule | Fail-open behavior |
|---|---|---|---|
| 1 | Reachable/selectable | `server.Status==active`, `HealthStatus!=unhealthy`, application reachable (health-check probe), not `ServerBusy` (a benchmark is running on it) | hard — an empty result here is `ErrNoHealthyHost` |
| 2 | Context-fit | estimated prompt tokens (word count × 1.3) + `MaxTokens` must fit the mapping's known `ContextSize` (unknown/0 always fits) | on empty, route without the filter (warn) |
| 3 | Capacity cap (CP3) | keep candidates whose server is below its *effective cap* = `MaxConcurrency − reservedSessions(server)`; unknown `MaxConcurrency` (0) never caps | on empty **and** an admission controller is wired → queue (§6) instead of falling open; otherwise fail-open (warn) |
| 4 | Swap-protection | drop a **not-already-loaded** candidate whose server is actively serving (a request in flight, or completed within `swap_protect_window_seconds`) — loading it now would evict a resident model | on empty, or no viable survivor, fail-open (warn) |
| 5 | Prefer-loaded (dominant, not a filter) | if any survivor already has the requested model resident, that partition is preferred over the rest — but only if it yields a Score-viable pick | fails open to the full pool otherwise |
| 6 | `argmaxByScore` | pick the highest `Score()` (below) among what remains | `ok=false` → `ErrNoHealthyHost` |

Filters 3–5 are all gated on optional collaborators (`ServerActivityChecker`,
`LoadedModelChecker`) being wired; with none wired, resolution is exactly the
filter-1 → filter-6 path (the pre-capacity, pre-swap-protection behavior),
which is the intentional no-op invariant these features were added under.

### 3.1 The score function

`Score()` (`internal/routing/scorer.go`) combines live telemetry, static
priority/weight, and a bounded per-model metric tiebreak:

```mermaid
flowchart LR
    Base["base = 1000\n+ priority × 20\n+ weight"] --> Penalties["− activeRequests × 25\n− queueDepth × 20\n− latencyMS × 0.2\n− errorRate × 200\n− 500 if telemetry stale (> 2 min) / missing"]
    Penalties --> Gate{"score ≤ 0 ?"}
    Gate -->|yes| NonViable["non-viable — excluded\n(the viability gate)"]
    Gate -->|no| Tiebreak["+ metricTiebreak(route)\n(bounded ≤ 100)"]
    Tiebreak --> Final["final score"]
```

The viability gate runs **before** the tiebreak: a candidate whose live
health/load penalties already sank it to ≤0 is excluded outright, so a fast
benchmark score can never rescue a degraded server. `validTelemetry` also
rejects negative counters or an out-of-range/`NaN`/`Inf` error rate outright.

`metricTiebreak` (capped at `genThroughputBonusCap(50) +
promptThroughputBonusCap(20) + mtpBonus(30) = 100`, well under the 200-point
error-rate penalty and 500-point stale-telemetry penalty):

| Term | Formula | Cap |
|---|---|---|
| Generation throughput | `effectiveGenTPS(route) × 0.25` | 50 (at 200 tok/s) |
| Prompt (prefill) throughput | `PromptTokensPerSecond × 0.01` | 20 (at 2000 tok/s) |
| MTP flat bonus | `+30` if `IsMTP` | — |

`effectiveGenTPS` is the **load-aware effective-speed** term: it linearly
interpolates a mapping's single-request `GenTokensPerSecond` (at concurrency 1)
toward its measured `GenTokensPerSecondAtCapacity` (at
`RecommendedConcurrency`), using the route's *current* in-flight load
(`CurrentLoad`), and extrapolates beyond `RecommendedConcurrency` with the same
slope (clamped ≥ 0). A mapping with no capacity curve, or an idle server
(`CurrentLoad ≤ 1`), collapses to the flat `GenTokensPerSecond` — byte-identical
to the pre-capacity scoring. All-zero metrics contribute exactly 0 (an
unmeasured mapping scores exactly as it did before any metrics existed).

`routing.ScoreModelServers` (`internal/routing/score_servers.go`) reuses the
same `Score()` path to produce a read-only, session-independent ranking shown
in the portal (`GET /api/portal/model-servers`, its `/events` SSE sibling, and
`GET /api/portal/model-group-servers`) — it never mutates resolver state and
does not apply per-session swap-protection/reservation, since it represents the
*general* live order, not one request's pinned outcome.

The group variant additionally orders its rows by the group's **manual** member
order. It does not model the group's selection settings — `member_order=speed`,
`loaded_only` and the `min_tokens_per_second` floor all reorder or filter
members inside the resolver — so such a group may be served in a different order
than the portal shows. The ranking *within* one member is still the live
scorer's.

## 4. Route affinity

`RouteAffinity` (`internal/routing/store.go`) pins one `(APITokenID, Model,
APIFlavor, SessionID)` key to a concrete `(ApplicationID, ServerID)` so a
conversation keeps talking to the same server while a model is resident there.

- **Key.** `AffinityKey{APITokenID, Model, APIFlavor, SessionID}`, hashed to an
  opaque `aff_<12 bytes of SHA-256>` id. `SessionID` defaults to the
  best-effort **extracted client session** (`ClientSessionID` — Codex
  `session_id`/`prompt_cache_key`, Claude Code
  `x-claude-code-session-id`, portal chat id, or a generic
  `prompt_cache_key`/`user`); `SetAffinitySessionMode(legacy=true)` reverts to
  keying on the explicit `X-OP-AI-Gateway-Session-ID` header only.
- **TTL.** Set only when the serving application's `AffinityTTLSeconds > 0`;
  each hit extends `ExpiresAt` by the same window from *now* (sliding, not
  fixed). An expired, deleted, or now-invalid pin (application
  disabled/re-flavored, server unhealthy/unselectable, benchmark-busy, or the
  mapping gone) is deleted and resolution falls through to fresh selection.
- **Reservation.** Every affinity hit or fresh pin also `touch`es an in-memory,
  per-server `sessionReservation` (window = `session_reservation_window_seconds`,
  default 60s) recording that session as "live" there. The capacity cap (§3,
  filter 3) subtracts this count from `MaxConcurrency` so unpinned traffic
  cannot fill the slots active pinned conversations will return to.

## 5. Model groups

A **`ModelGroup`** (migration `22`) is offered to clients as a synthetic
gateway model name; requesting it walks its members in priority order instead
of resolving a single mapping.

- **Traversal strategies** (`FlattenGroup`, `internal/routing/group_flatten.go`)
  flatten nested subgroups (a member name that is itself a group) into one
  ordered, de-duplicated leaf-model list, each subgroup expanded by **its own**
  strategy:
  - `depth` (default/unknown) — expand each subgroup fully in place before the
    next member.
  - `breadth` — all direct leaf models first, then every subgroup's expansion.
  - `round_robin` — interleave one item per stream (direct models emit once on
    pass 1; each subgroup contributes its next item per pass) — deep pagination
    across sibling groups. A cyclic back-edge or an inactive subgroup
    contributes nothing.
- **Failover modes** (`resolveGroup`, `internal/routing/resolver.go`):
  - `sticky` — prefer the pinned member while it is available (`memberOK`); if
    it is down or every candidate is at capacity, fall through to a fresh
    priority walk and re-pin — never climbs back to a higher-priority member on
    its own.
  - `climb_up` — like sticky, but when the pin is available **and** a
    strictly higher-priority member is too, it switches now if that member is
    already loaded (a free climb — never trades a working pin for a cold
    start), otherwise fires a non-blocking, deduplicated `ModelWarmer.Warm`
    (`internal/gateway/model_warmer.go`, 60s in-flight/cooldown dedup, 60s
    absolute call timeout) and keeps serving the pin this turn; a later turn
    climbs once the target is loaded. Falling **down** when the pin is
    unavailable is always immediate, even to a cold member. Under `loaded_only`
    the warm call is skipped entirely — warming a cold member is exactly what
    the flag avoids. That suppression reads the group's **configured**
    `loaded_only`, so it also holds on the relaxation ladder's later attempts,
    whose loaded-only *filter* has already been dropped. Under
    `member_order=speed` the free-climb rule gains one
    more gate: see `climb_speed_margin_percent` below.
- **Selection settings** (`ModelGroup`, migration `62`) — four independent,
  combinable settings, all defaulting to today's behavior:
  - `loaded_only` — restricts availability to members with an already-loaded
    candidate. If nothing is loaded for the request, the restriction is
    dropped rather than dead-ending a request (see the relaxation ladder
    below).
  - `member_order` (`priority` (default) | `speed`) — `speed` ranks each member
    by its fastest *eligible* candidate's `effectiveGenTPS` (§3.1's load-aware
    speed), descending; an unmeasured member sorts last, and ties keep the
    manual order (`sort.SliceStable`), so a group with no measurements
    anywhere behaves exactly as it did before speed ordering existed. Ranking
    runs once per relaxation attempt, not once per request, because a
    member's eligible candidates — and therefore its rank — depend on which
    filters that attempt still has active.
  - `climb_speed_margin_percent` (default 20) — for a speed-ordered group,
    `climb_up` climbs only to a member whose speed exceeds the pin's by more
    than this margin; `0` is a legitimate "no margin" value (any strictly
    faster candidate then wins), and only a negative value is clamped to 0.
    A priority-ordered group ignores this setting entirely. The write API
    distinguishes the two: omitting the field on create applies the default
    20, while an explicit `0` persists as `0` — an unset field and a
    deliberate "no margin" policy are not the same thing.
  - `min_tokens_per_second` (0 = off) + `min_speed_fallback` (`error` |
    `ignore`) — a floor on the same load-aware `effectiveGenTPS`, applied per
    **candidate**, not per member, so a member with one fast and one slow
    candidate can never be served on the slow one. An unmeasured candidate
    (0 tok/s) never satisfies a nonzero floor. When no candidate anywhere
    reaches it, `error` surfaces `ErrNoHealthyHost` (502, §8); `ignore`
    re-resolves the group without the floor.
- **Evaluation order and relaxation.** `min_tokens_per_second` and
  `loaded_only` are both candidate-level filters inside `eligibleCandidates`
  (`internal/routing/resolver.go`, applied in that order — floor first, then
  loaded-only), and both the pin check (`selectMember`) and the priority/speed
  walk (`firstAvailable`) go through that same function — so a pinned
  candidate that drops below the floor, or a pinned member that is no longer
  loaded, simply stops counting as "available" and falls through to the walk,
  exactly like a down or at-capacity pin already does. Within one attempt,
  `resolveGroupOnce` first re-sorts the member list when `member_order=speed`
  (so the pin check, the climb comparison, and the walk all read the same
  "fastest first" order), then checks the pin — itself filtered by the floor
  and loaded-only rules above — and only then falls through to the walk,
  which applies the identical filters to every member it inspects.
  `resolveGroup` re-attempts the whole thing (re-sort included) under a
  **cumulative, monotone relaxation ladder** whenever an attempt finds nothing
  eligible (never on a store error or an admission-queue timeout — those
  propagate immediately):
  1. floor and `loaded_only` both applied, as configured;
  2. `loaded_only` dropped, floor still applied;
  3. neither applied — reached only when `min_speed_fallback=ignore`.

  The loaded-only filter is always dropped before the speed floor, and a
  filter already dropped never reappears in a later attempt. Three
  consequences follow directly from this design:
  - The floor reads the *load-aware* effective speed, so a member can drop
    below it purely because it is currently busy — nothing about the member
    itself needs to change.
  - A session loses its pin when its model is evicted under `loaded_only`,
    and likewise when a pinned candidate falls below the floor — the honest
    outcome in both cases, since continuing to serve the pin would mean
    loading a cold model or serving below the configured floor.
  - A speed-ordered group reads every member's mappings and telemetry on
    every request (`orderMembersBySpeed`), additively to whatever the walk
    itself reads, and the relaxation ladder can repeat that pass once per
    attempt — where a priority-ordered walk stops at the first available
    member. Only a group that opts into `member_order=speed` pays this cost.
- **Error mapping** (§3g of the design): zero members, or no member with any
  live mapping → `ErrNoModelRoute` (unknown model); members exist but all are
  gated (down/non-viable/at-capacity with no admission controller, **or every
  candidate removed by the speed floor or the loaded-only filter**) →
  `ErrNoHealthyHost` — a member that is live but gated by policy is never
  reported as an unknown model.
- **Live priority.** `GroupRegistry` (`internal/gateway/group_registry.go`) is
  the resolver's `GroupResolver`: a pull-fed, in-memory snapshot rebuilt by
  `RefreshGroups` after every group/member/model-setting write (create, update,
  reorder, delete) and swapped in atomically under a lock, so a priority reorder
  or an added/removed member takes effect on the very next request — no
  restart, no cache TTL. A model's `ModelSetting.Visibility=="locked"` makes it
  reachable only via a group (`DirectAllowed` returns false for a direct
  request). The portal's "is this group loaded" rule (`internal/portal/service.go`)
  differs for a `loaded_only` group: normally a group is loaded iff its
  highest-priority *offerable* member is loaded, but for `loaded_only` **any**
  offerable member being loaded makes the group loaded, and `LoadedOn` is the
  union of those members' servers — because for such a group, any of them is
  what would actually be served.

## 6. Concurrency capacity (CP1–CP4)

Four cooperating pieces turn "the model is registered" into "the gateway knows
how many concurrent requests it can safely take and queues the rest instead of
overloading it."

### 6.1 CP1/CP2 — the capacity benchmark engine

`measureMappingCapacity` (`internal/gateway/benchmark_capacity.go`) runs an
OOM-safe concurrency ramp: after a warm pass (ensuring the model is resident),
it fires `n = 1, 2, 4, 8, …` concurrent streaming requests per level (capped at
`capacity_max_concurrency`, default 64) and evaluates **every** applicable stop
signal after each level — never escalating past a tripped one:

| Signal | Trip condition | Meaning |
|---|---|---|
| `error` | any request in the level failed | server can't sustain this level |
| `memory` | VRAM or RAM free fraction < `capacity_vram_safety_margin_percent` (default 10%) on a *fresh* telemetry sample (waited for via a settle interval, default 5s) | the hard OOM guard |
| `queue` | the app's capacity-probe path reported `requests_deferred > 0` **at the peak** while the burst was in flight | this level is over capacity |
| `latency` | mean latency > 4× the level-1 latency | latency collapse — the only ceiling on a target with no probe and flat (pre-allocated) VRAM |
| `slot_ceiling` | all probed slots busy but nothing queued (`processing ≥ total_slots`, `deferred ≤ 0`) | this level **is** the ceiling and served fine — counted as good, then stop escalating |

The memory mode (agent telemetry vs. upstream probe vs. latency-only fallback)
is decided once per run. Every attempted level is recorded as a
`routing.CapacityLevel` (aggregate/per-request tok/s, mean latency, successes/
errors, VRAM/RAM free %, deferred/processing/total slots, `StopReason`) —
persisted as one `BenchmarkRun{Kind:"capacity"}` history row with the full
curve JSON-encoded in `CapacityCurve`, and distilled into:

- **`MaxConcurrency`** — the highest level that passed every check (0 ⇒ not
  even one concurrent request worked; nothing is persisted).
- **`RecommendedConcurrency`** — the highest level whose mean latency stayed
  within 1.5× the level-1 latency (the "no visible slowdown" knee).
- **`GenTokensPerSecondAtCapacity`** — the mean per-request generation rate at
  `RecommendedConcurrency` — feeds `effectiveGenTPS` (§3.1).

`UpdateMappingCapacityMetrics` persists the three distilled scalars atomically,
only when `metrics_locked` is false.

### 6.2 CP3 — the routing capacity cap

Filter 3 of `selectCandidate` (§3): `effectiveCap = MaxConcurrency −
reservedSessions(server)`, compared against the live in-flight count from
`ServerActivityChecker`. `MaxConcurrency == 0` (unmeasured) never caps —
capacity-aware routing is opt-in per mapping, driven purely by whether a
capacity benchmark has ever run.

### 6.3 CP4 — the admission queue

When every candidate is at its effective cap **and** an `AdmissionController`
is wired, `Resolve` parks instead of failing open, via `admissionQueue`
(`internal/gateway/admission_queue.go`) — a bounded, FIFO, cancellable wait
queue, lost-wakeup-safe by construction:

```mermaid
stateDiagram-v2
    [*] --> Enqueued: WaitForSlot(serverIDs, timeout)
    Enqueued --> Rejected: queue at max_depth (immediate, 503)
    Enqueued --> Signalled: release(serverID) hits the\nFRONT unsignalled waiter watching it
    Enqueued --> Rechecked: 250ms liveness re-check elapses\n(no release required)
    Enqueued --> TimedOut: wall-clock deadline elapsed (503)
    Enqueued --> Cancelled: ctx.Done()
    Signalled --> Dequeued: removed from FIFO on signal
    Rechecked --> Dequeued: removed from FIFO on wake
    Dequeued --> Retry: Resolve re-runs selectCandidate
    Retry --> Enqueued: still at cap → re-park (at the back)
    Retry --> [*]: slot available → served
```

Key properties:

- **Dequeue-on-signal.** `release(serverID)` signals *and* removes the front
  waiter watching that server in one locked step, so two queued waiters can
  never claim the same freed slot and N distinct frees wake N distinct
  waiters — this is what makes the hand-off race-safe.
- **Bounded liveness re-check** (`admissionRecheckInterval`, 250ms). A backstop
  independent of the release signal: it guarantees a slot that frees in the
  window between the resolver's cap-check and the waiter's enqueue (or a
  release that raced/coalesced) is never missed for longer than one interval.
  Without it, an unbounded wait could hang on a lost wakeup with a free slot
  sitting idle.
- **Wall-clock deadline, not a per-wait budget.** The timeout passed to
  `Resolve`'s retry loop is `admission_queue_timeout_seconds` (max across the
  candidate applications; 0 = unbounded) resolved to an **absolute deadline**
  once, at the first park. Each subsequent `WaitForSlot` call gets only the
  *remaining* time — otherwise the 250ms re-check would silently re-arm the
  full budget every iteration and a request under sustained saturation would
  never actually hit its configured 503.
- **Bounded depth** (`admission_queue_max_depth`, default 128) rejects
  immediately (`ErrAdmissionQueueFull`, no wait) once full, rather than piling
  up unbounded parked goroutines.

The model-group path (§5) queues on the **union** of every at-capacity
member's candidate servers, using the same absolute-deadline discipline, and
never silently promotes an established finite deadline back to unbounded even
if the at-capacity set later becomes all-unbounded mid-wait.

## 7. Model-selection metrics

Metrics that feed §3.1's scoring come from three independent sources, all
gated by `metrics_locked` (a manually-pinned mapping never auto-overwrites):

| Source | `metrics_source` | What it measures | Trigger |
|---|---|---|---|
| **Benchmark run** | `"benchmark"` | cold-minus-warm load time, generation/prompt tok/s (`measureMapping` / `measureSpeedTarget`) | manual or scheduled (below) |
| **Opportunistic EWMA** | `"opportunistic"` | gen/prompt tok/s, blended (α=0.2) from every successful **real** inference on an app with `OpportunisticMetricsEnabled` | every live request, no explicit run |
| **Context probe** | (context-size fields only) | usable context window, via the app's `context_probe_path` (e.g. llama.cpp `/props`) | during a benchmark's warm pass, or standalone (`startContextProbe`) |

**Benchmark trigger modes:**

| Mode | How it starts | Notes |
|---|---|---|
| Manual | `POST` on a server/application/mapping scope (`startBenchmark`, `internal/gateway/benchmark_endpoints.go`) | 202 + status; 409 if a run is already in flight on that server, or the server has live in-flight traffic (idle-gated) |
| Scheduled | `Application.BenchmarkScheduleEnabled` + `BenchmarkScheduleIntervalSeconds` (floored at 60s), driven by `StartBenchmarkScheduler`'s 1-minute tick (`internal/gateway/benchmark_scheduler.go`) | speed-only, per-app cadence, idle-gated exactly like manual, skips `metrics_locked` mappings |
| Opportunistic | no run at all — an ambient side effect of `OpportunisticMetricsEnabled` on served traffic | see table above |

A benchmark **measurement kind** (`runBenchmark`'s `mode` argument — distinct
from the trigger modes above) selects what a run does per mapping: `"speed"`
(load time + throughput, the default), `"capacity"` (the CP1/CP2 ramp, §6.1),
`"both"` (speed then capacity, so `metrics_source` ends `"capacity"`), or
`"vision"` (an image-acceptance probe, out of this chapter's scope). Exactly
one benchmark runs per server at a time (`BenchmarkRegistry.TryStart`); the
server is excluded from routing (`ServerBusy`) for its duration.

`IsMTPModelName` (`internal/routing/mtp.go`) is the **MTP heuristic**: a
best-effort, conservative name-substring/token match (a known model family
like `deepseek-v3`/`glm-4.5`, or a standalone `mtp` token) that seeds a new
mapping's `IsMTP` default at creation time — always operator-overridable, and
itself protected by `metrics_locked` once set. It deliberately favors false
negatives over false positives, since a wrong `+30` MTP bonus would bias
selection toward a model that is not actually faster.

**Hybrid swap protection** combines two independent, fail-open signals before a
candidate is allowed to evict a resident model: the *loaded-state* partition
(§3, filter 5 — dominant when it yields a viable pick) and the *activity*
window (§3, filter 4 — in-flight requests, or a completion within
`swap_protect_window_seconds`, default 30s). Either signal alone can protect a
busy server; neither is consulted for a candidate that already has the
requested model resident (serving it is never a swap).

**Live SSE benchmark progress.** `BenchmarkRegistry` (`internal/gateway/benchmark.go`)
holds at most one run per server and fans out every `snapshot`/`progress` frame
to subscribers over `GET /api/portal/servers/{id}/benchmark/events`
(`handleBenchmarkEvents`, `internal/gateway/perf_endpoints.go`): a `snapshot`
frame on connect, then a `progress` frame after each measured mapping and the
terminal finish (including the live `current_concurrency` level during a
capacity ramp), with a 25s heartbeat. A slow subscriber simply drops frames
(each is a full status, not a delta) and recovers on the next one.
`GET /api/portal/benchmarks/active` lists every currently-running benchmark the
caller may see, for a dashboard-style overview without subscribing per server.

## 8. Errors and HTTP mapping

| Routing error | HTTP status | When |
|---|---|---|
| `ErrNoModelRoute` | 502 | no active mapping exists at all for the model/flavor (or the model is a locked group-only name requested directly) |
| `ErrNoHealthyHost` | 502 | mappings exist but every candidate is gated (unhealthy/unreachable/busy/non-viable) |
| `ErrAdmissionQueueTimeout` | 503 | an admission-queued request's deadline elapsed before a slot freed |
| `ErrAdmissionQueueFull` | 503 | the admission queue was already at `admission_queue_max_depth` |
| `ErrServerOverrideModelUnavailable` | 404 | a server-override request named a server that does not offer the model via a live mapping |
| `ErrServerOverrideServerUnavailable` | 502 | a server-override request named a disabled/unreachable server and did not force through it |

## 9. Configuration reference

| Env var | Default | Governs |
|---|---|---|
| `OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS` | 30 | §3 filter 4 recency window |
| `OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS` | 60 | §4 reservation liveness window |
| `OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT` | 10 | §6.1 hard OOM guard |
| `OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY` | 64 | §6.1 ramp ceiling |
| `OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS` | 5 | §6.1 inter-level telemetry settle wait |
| `OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH` | 128 | §6.3 queue bound |
| `OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS` | 86400 | §7 scheduled-mode default cadence |

See [Configuration](configuration.md) for the full variable list.

## Related chapters

- [API Compatibility & Inference](compatibility-and-inference.md) — how a
  client request becomes the `inference.Request` this chapter routes.
- [Security, Authentication & Authorization](security-auth-rbac.md) — the
  `ProvisioningGate` and per-token/service authorization consulted before a
  candidate is offered.
- [Telemetry, Usage Analytics & Observability](telemetry-usage-observability.md) —
  the `ServerTelemetry` this chapter scores against, and how it is produced.
- [Persistence](persistence.md) — the `Store` interface and its SQLite/
  PostgreSQL/memory implementations.
