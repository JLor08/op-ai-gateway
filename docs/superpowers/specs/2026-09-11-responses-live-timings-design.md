# Responses live timings — design

Issue #81. This branch delivers **part 1 only**: the wiring. Part 2 (the gate, the
injection, the retry and the operator control) follows in its own branch.

## 1. What is wrong

For a streaming `/v1/responses` request in endpoint mode `passthrough`, the
running-connections panel's live tokens/sec cell is empty for the whole request.
The `*.delta` partials carry no usage, so there is no honest mid-stream source;
the figure appears once, at the terminal `response.completed` frame, and is
visible for about a second before the row leaves the panel. That is #77 working
as designed — on the flavor Codex actually uses, the panel's headline live column
is blank throughout.

llama.cpp will attach a top-level `timings` object to the **partials** when the
request body carries `timings_per_token: true`. Measured on the operator's
deployment (llama.cpp build `b10448-ad1de39e0`, an MTP model, through the
server-agent's runtime router): **39 of 48 frames carry it with the flag set,
exactly one — the terminal frame — without it.** One build on one deployment,
not a general guarantee.

Passthrough relays the client's body, and Codex does not set the flag.

## 2. Decisions

These are settled; this document records them rather than arguing them.

- **(a) An orthogonal boolean, not a fourth `EndpointMode` value.** `passthrough`
  plus telemetry is two axes, not one. The deciding reason is the failure mode:
  `tryProxyNative`'s `default:` branch serves an *unknown* mode as **translate**
  and `exhaustive` is not enabled, so a rollback to an older binary or a stale
  frontend bundle would silently downgrade Codex traffic to the lossy path. A
  boolean defaulting off cannot do that.
- **(b) Responses only.** llama.cpp attaches no `timings` to any Anthropic frame,
  and Anthropic passthrough already derives a live rate throughout, so the
  control is never offered on the `/v1/messages` side.
- **(c) The runtime spec carries it too.** For a `server_agent` application the
  resolved spec is the authority for endpoint behaviour — `responses_mode` and
  `messages_mode` live on `agent_runtime_specs` in parallel — so the boolean
  follows them, with the same backfill-from-parent-application shape migration 72
  used.
- **(d) Default: DDL `0`, and the create path writes `1` for a capable upstream
  kind.** An upgrade must not change any running deployment's behaviour; a newly
  created llama.cpp application gets it on. Precedent:
  `opportunistic_metrics_enabled integer not null default 0`.
- **(e) An impossible `true` is refused; a non-mention is cleared.** A request
  may not set `responses_live_timings_enabled` to **true** when the resulting
  type — the application type, or the runtime spec's effective type, that the
  write leaves behind — is not a live-timings-capable kind; such a request is
  **rejected with 400**, naming the kind. An **absent** field is never a
  rejection: on create the server picks by kind (decision (d)), and on update a
  stored `true` is **cleared** when the resulting type is incapable.

  This **supersedes** this decision's earlier wording, "A type change clears
  it.", and with it the silent normalisation that wording invited — storing
  `false` for a caller who explicitly asked for `true` and answering 200. The
  reason is one sentence: a write that stores something other than what it was
  asked to store is its own defect class, a write that lies about its result.
  (The same argument the tree already makes at
  `ErrApplicationProxyExcludedPortConflict`: "silently zeroing what the caller
  asked for in the same breath would be a lie".)

  What makes the pair usable is **assertion versus non-mention**. A retype that
  does not mention the field asserts nothing, so clearing overrides nothing the
  operator said; an explicit `true` against an incapable resulting type *is* an
  assertion that cannot hold, so it is refused rather than quietly rewritten.
  This works only because the request field is a **pointer** — absent and false
  must stay distinguishable, which §4 already requires for decision (d)'s sake.
  The rule is phrased over the *resulting* type, not over a transition, because
  the runtime-spec write is a full-document PUT with no retype event
  (decision (c)) — and because the invariant is a property of the resolved row,
  which is the shape `applyProxyExclusion`'s own RULE 4 already argues for.

  The invariant obtained: a stored `true` always means "this will inject once
  the verdict allows" (decision (f)). No "on but inert" state exists — which is
  the second reason for this variant, because part 2's operator control then
  needs no indicator explaining why a switch is doing nothing.
- **(f) Never optimistic** (part 2): injection happens only where the gateway
  already *knows* the upstream tolerates the key, through the existing
  three-layer `wantsLiveProgress` rule. No new guessing.
- **(g) No stripping** (part 2, deferred again): the same client already receives
  a `timings` object unconditionally on the terminal frame today and this gateway
  depends on reading it, so the fields are not foreign to it. Stripping is most of
  the cost and all of the corruption risk; if a real incompatibility appears it
  gets its own second boolean, whose meaning must be "remove what **we** added",
  never "remove `timings`".
- **(h) Rate only.** `predicted_n` also rides the partials, so an exact
  mid-stream count is available. Taking it is explicitly out of scope and stays
  an open question on #81.

## 3. What part 1 delivers

A boolean that reaches `routing.Target` and is read by nothing.

**No operator control ships in part 1.** A visible toggle that does nothing would
be a worse defect than the blank cell it promises to fix, so the portal control
lands in part 2 beside the code that honours it.

Scope:

1. Migration 80 adds `responses_live_timings_enabled integer not null default 0`
   to `applications` **and** `agent_runtime_specs`, through `addColumnIfMissing`,
   with the spec column backfilled from the parent application by
   `agent_runtime_specs.mapping_id → model_mappings.application_id → applications`
   — migration 72's join, in both drivers' dialects. Not added to
   `baselineCreateStatements`, which is frozen as of v60.
2. The store's hand-maintained paths carry it: `CreateApplication`'s positional
   insert, `UpdateApplication`'s `set` list, all three independently maintained
   `applications` select lists, both scan functions (an `int64` local plus a
   `!= 0` conversion, since SQLite has no bool), and the runtime-spec upsert and
   scans.
3. `routing.Application`, `routing.RuntimeSpec` and `routing.Target` gain the
   field, with `targetFrom` giving the spec precedence where it has one — the
   same precedence the modes already use.
4. The portal accepts and returns it: a **pointer** on both request shapes, the
   kind-dependent create default, decision (e)'s 400 for an explicit `true` on
   an incapable resulting type, the clear for an absent field on one, and the
   hand-written DTO mappers on both the application and the runtime-spec side.

## 4. The hazards this design must survive

Each was verified in the tree; a plan that does not address them will ship a
silent defect.

- **The routing join is the dangerous reader.** `applications` has three
  independently hand-maintained select lists feeding two scan functions, and
  `ActiveMappingsForModel` is the one that decides where live traffic goes. A
  column missed there reads back as a clean zero while every memory-backed portal
  test still passes.
- **A reordered select list does not fail loudly.** Both scan functions have
  fixed arity, so an *omitted* column errors at `Scan`, but two same-typed
  columns swapped read each other's values silently.
- **The PostgreSQL leg skips silently** without
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN`. This is a store change, so the leg is
  mandatory and its pass count must be stated, not assumed.
- **A plain Go `bool` on a request shape kills decisions (d) AND (e).** Absent
  and false are the same value, so the kind-dependent create default could never
  fire for any client that sends the key — and "the caller asserted `true`" could
  never be told apart from "the caller said nothing", which is the distinction
  decision (e)'s refuse-or-clear split rests on. Both request shapes take a
  pointer.
- **`putRequestFromDTO` is a hand-written spread and compiles without the new
  field.** Its own doc records that a defect of exactly this kind was already
  paid for once.
- **The DTO mappers are hand-written and the application side has no coverage
  test.** A field added to the DTO but not to the mapper is invisible.
- **`RuntimeSpecRequest` is derived by TypeScript `Omit`**, so adding a field to
  the `RuntimeSpec` interface silently makes it a *required* request field.
- **Frontend and backend defaults must not both claim authority.** If
  `applicationTypeDefaults.ts` gains a per-type `true` and the form sends the key
  unconditionally, the backend's kind-dependent default can never fire. Part 1
  ships no frontend default at all.
- **Decision (e)'s 400 reaches the portal form, not only API clients**
  (part 2). `ApplicationSection.tsx`'s `buildBody()` is one literal reused
  verbatim for create and update, so a field added there is restated on every
  save — and a retype in the portal would then assert the flag against the new
  type and earn a 400. The control must therefore gate *what it sends*, not
  only what it renders, exactly as `proxy_excluded` already does
  (`if (proxyExcluded === proxyExcludedSeed) delete body.proxy_excluded;`, with
  the create path gated on whether the control was rendered at all). The
  runtime-spec form is a full-document PUT and needs the same care.
- **Renaming `data-model.md`'s "Migration history (79 migrations)" heading breaks
  seven anchor links.** Update them with it.

## 5. Verification

Both Go modules' `golangci-lint fmt --diff` and `run`, `go test ./... -count=1`,
the frontend's four gates if any frontend file changes, both docs gates, and —
because this is a store change — the PostgreSQL conformance leg with its DSN
set, with the pass/skip counts recorded.

A test must exist that fails if the column is missing from the routing join
specifically, not only from the portal readers, since that is the one whose
absence is silent.
