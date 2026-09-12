# Responses live timings, part 2: the gate, the injection, the live count

Part 2 of issue #81. Part 1 (#82, merged as `09ffde4`) wired an operator boolean
from migration 80 through the store, `routing.Target`/`routing.RuntimeSpec`, a
capable-kind predicate and both portal API surfaces, and deliberately stopped
there: nothing acts on the value, and there is no portal control. **Part 2 adds
both** — the behaviour and the control.

When the switch is on for a llama.cpp upstream, the gateway adds
`"timings_per_token": true` to a **streaming native passthrough `/v1/responses`**
request, so llama.cpp attaches a top-level `timings` object to the partial
frames the gateway already knows how to read. The running-connections panel then
shows a live, upstream-reported tokens/sec for the whole request instead of for
the last second of it.

---

## 1. What was measured, and what is therefore not assumed

Measured against the operator's live deployment on 2026-09-12, through the
gateway: llama.cpp build `b10448-ad1de39e0` serving
`unsloth/Qwen3.8-27B-MTP-GGUF:BF16-Fix` via the server-agent runtime router, and
a vLLM upstream serving model `qwen3.8-27b-fp8` — **whose build identifier was
not recorded, and should be before anyone revisits D6.** One build of llama.cpp
on one deployment; not a general guarantee, the same caveat the repository
already applies to #80's measurement.

### M1 — llama.cpp's `/v1/responses` does not reject an unknown top-level key

A request carrying `"zzq_not_a_real_key": true` answered with an ordinary
completion body rather than an error object. The endpoint tolerates unknown
top-level keys rather than validating against a closed schema, which the
repository already explains: llama.cpp's request schema is pull-based, so a key
nobody asks for is never inspected.

This is the decisive measurement for the retry (§3, D3).

### M2 — the terminal `timings.predicted_n` equals `response.usage.output_tokens`

A **naturally ending** flagged stream (cap 400, actual 33): 40 data frames, 31
carrying a top-level `timings` — 27 `response.reasoning_text.delta`, 3
`response.output_text.delta`, and the terminal `response.completed`.

`predicted_n` runs 1…27 on the reasoning deltas, then 30, 31, 32 on the
output-text deltas: two tokens are generated across the
`output_item.added`/`content_part.added` pair, which carry no `timings`, so the
partial series is monotone but **not contiguous**. It reaches 33 only on the
terminal frame, where `response.usage.output_tokens` is also 33.

So the two count the same thing, reasoning tokens included — **but the highest
count visible on a partial is short of the recorded total.** The same gap
reproduces in the other captures and in issue #81's own series, so it is a
property of llama.cpp rather than a one-off.

> A first attempt at this measurement used `max_output_tokens: 60` and observed
> `60 == 60`. That proved nothing: the generation was truncated at the cap, so
> both numbers were the cap. Recorded because the trap caught this document's
> author twice — once here, and once in an earlier draft of D4, which concluded
> from the capped run that a live count "cannot disagree with" the total.

### M3 — the flag's effect reproduces, an explicit `false` is honoured upstream, and the wire cost is 2.5×

Same prompt, same generation length, three runs:

| Request | data frames | frames carrying `timings` | bytes |
|---|---|---|---|
| `timings_per_token: true` | 68 | **59** | 28 463 |
| key absent | 68 | **1** (terminal only) | 11 412 |
| `timings_per_token: false` | 65 | **1** (terminal only) | 10 614 |

This reproduces #81's observation — 39 of 48 frames with the flag, exactly one
without — and adds the case nobody had measured: llama.cpp treats an explicit
`false` exactly as it treats an absent key.

All three terminate at the same `output_tokens: 60`, so **2.49×** is the wire
cost of the flag for an identical generation. That figure is the size of what
the deferred strip (§6) would recover.

### M4 — vLLM accepts the key and produces nothing

vLLM serves `/v1/responses` — verified by the reply carrying vLLM-only fields
(`kv_transfer_params`, `ec_transfer_params`, `input_messages`,
`output_tokens_per_turn`) that a gateway-side translator would not invent, so
this was genuine passthrough. It answered 200 to `timings_per_token: true` and
200 to a fabricated key alike.

A streamed run with the flag: **48 frames, zero carrying `timings`** — not on
the partials, not on the terminal frame. For vLLM the key is not "tolerated but
unproven"; it is inert. `timings_per_token` is a llama.cpp parameter, and vLLM's
schema simply allows unknown fields.

---

## 2. What part 1 already provides

- `routing.Target.ResponsesLiveTimingsEnabled`, resolved spec-over-application,
  in scope as a named parameter of `gateway.proxyNative`. Nothing has to be
  threaded.
- `routing.LiveTimingsCapableKind`, with a pinned-size tripwire.
- The read side: `mergeResponsesUsage` already lifts `timings.predicted_per_second`
  off any Responses frame, and `publishProgress` deliberately publishes *this
  frame's* value rather than the accumulator's. The flag is the only missing
  input — `TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`
  already pins the whole chain for a **client-set** flag.
- The sibling mechanism: the gateway **already injects this exact key** on the
  translate path, gated, with one retry without it. Part 2 builds a sibling, not
  a first.

---

## 3. Decisions

### D1 — The injection lives in the gateway layer, beside the model rewrite

At `native_passthrough.go`'s body-building step, as a **separate** step next to
`rewriteModelField`, not inside it.

Beside it, because `rewriteModelField` returns the *original* slice in three
no-op branches, one of which — provider model equal to gateway model — is an
ordinary configuration that the portal's auto-sync path actively produces. An
injection folded in below that branch would silently never fire for those
mappings.

In the gateway layer rather than `provider.ProxyNative`, because the flavor, the
stream flag, the capture record and the panel row are all in scope there, while
`provider.ProxyNative` receives only `(ctx, target, path, body)` and has no way
to report back what it sent. Issue #81 suggested `provider.ProxyNative` "where
the memo lives"; D2 removes the memo from the design, and with it that reason.

**The new body must be a new slice.** In the no-op case the current helper
returns a slice aliasing the request's backing array — bytes the HTTP transport
is still reading while the request is in flight, and which are read again
afterwards to build the capture record. An in-place edit would race the send and
corrupt the capture.

### D2 — The gate is the operator's switch, the kind, the flavor and the stream — with a recorded rejection as a veto

Inject when **all** of:

1. `target.ResponsesLiveTimingsEnabled` is true;
2. the effective kind is llama.cpp — for a `server_agent` target that is
   `target.LiveProgressSpecType`, never `target.Provider`, which is the literal
   `"server_agent"`;
3. the request is the Responses flavor;
4. the request is streaming;
5. and the stored live-progress verdict is **not** an explicit negative —
   spelled `"unsupported"` on the target, which is what the one producer emits;
   a veto written against the capability row's own `"no"` would never fire.

Condition 3 must take the **fine** flavor as a parameter. `Target.APIFlavor` is
the coarse one and cannot tell `/v1/responses` from `/v1/chat/completions`.

Point 5 is a veto, not a requirement. Requiring a positive verdict would make
the switch silently dead wherever the capability probe never ran, which is the
worst possible failure for a control that is on.

This keeps the existing rule's layer 1 — *a recorded rejection beats everything*
— as a veto. Point 2 **is** that rule's shape clause, kept and hardened from a
fallback into a requirement, narrowed from two kinds to the one M4 leaves
standing. Only the positive-verdict layer is dropped, and dropping it is inert
on the population that passes point 2: within llama.cpp, a positive verdict
adds nothing the veto has not already allowed.

**This is #81 point 3 minus its positive layer.** That point asked for "never
optimistic — inject only where the gateway already *knows*". Point 2 is #81's
own shape clause narrowed to the one kind M1 and M3 measured, so this gate is
never *broader* than the existing one — only narrower, by an operator switch.

**`wantsLiveProgress` is not reused.** A fourth condition is ANDed at its only
call site, and its verdict describes the completion endpoint's parameter schema,
not `/v1/responses`. Part 2 gets its own predicate, in package `gateway`, over a
`routing.Target`.

**Accepted residual:** an old llama.cpp build that rejects unknown keys on
`/v1/responses` would answer 400 to a flagged request. M1 shows the measured
build does not. The blast radius is one application, the operator can switch it
off through the API field on either surface, and with no retry the failure is
immediate and visible rather than silent. D9 is what makes it diagnosable.

### D3 — No retry in this cut

Issue #81 raised a retry and required it to "distinguish a 400 the injected key
earned from a 400 the client's own body earned". Two things settle this:

- **The requirement is not one.** The cited precedent, the translate path's
  retry, makes no such distinction and deliberately does not: it checks three
  guards and no body content, and accepts misattributing a client-earned 400.
  Nothing in this repository classifies an upstream 4xx by body content.
- **The trigger cannot be exercised.** M1 measured that the endpoint accepts
  unknown top-level keys, and M3 measured a *streaming* request carrying the
  very key that would be injected — 68 frames, 59 timings-bearing, no rejection.
  A retry would be code that no upstream we can point at would fire; only a
  fabricated one does, which is how the sibling retry is tested.

Shipping it would also mean re-issuing on a context whose watchdog is already
armed, a deferred body close bound to the discarded response, and a latency
figure that includes the discarded attempt.

If a real rejection is ever observed, the retry is a self-contained follow-up:
the status is known before the first client byte and the untouched client bytes
are still in hand.

### D4 — `predicted_n` is read for the live view only, never into the accumulator

M2 shows the **terminal** `predicted_n` is the same quantity
`response.usage.output_tokens` reports, so the live count converges on the
recorded total rather than competing with it — which is exactly why it must not
*also* be written into the accumulator. Doing that would rewrite the recorded
row, `usage_events`, the Activity totals, the timeseries and the rate limiter's
input, for no new information.

Mid-stream it may trail the total by a token or two, and it skips values across
frames that carry no `timings`. **The live column is an upstream count, not a
count of what the client has received**, and no test may assert equality between
the last partial count and the recorded total — such a test passes on
cap-truncated data and fails on naturally-ending data.

**The carrier must not be `OutputTokens` or `TotalTokens`.** Those are what the
scanner hands `recordUsage`, and thence `usage_events`, Activity, the timeseries
and the limiter. A separately named field is safe even though the shared merge
writes it into the accumulator too, because the usage event is assembled
field-by-field.

**No new wire field.** The live-progress DTO already carries an output-token
count, populated by the same mechanism for `anthropic_messages`; publishing
`predicted_n` through the live-progress path fills it by construction. The panel
column renders that existing field. The carrier constraint above is about the
scanner's usage struct, which is a different value on a different path.

The panel gains a live output-tokens column, **default-hidden**. Default-visible
would break six existing frontend assertions by ambiguity — they sit in five
test cases, two sharing one — while hidden keeps them all green.

**Accepted consequence:** giving a Responses row an exact mid-stream count makes
a `gateway`-labelled mid-stream rate reachable on that flavor for the first
time, because the upstream rate series opens at `0.0` and only a positive rate
is stored, so the earliest timings-bearing partials carry a count and no rate.
This is accepted and pinned by a test rather than suppressed: the structural
invariant is that a gateway-derived rate is only ever computed over an exact
upstream count, and `predicted_n` is exactly that. Suppressing it would mean
inventing a per-flavor exception inside the one derivation this feature has.

### D5 — An explicit client `false` is honoured

The injection tests for **presence** of the key, not its value. A body that
already carries `timings_per_token` — `true` or `false` — is forwarded
unchanged. Overwriting a client's explicit `false` would be the silent
rewriting of a client request that this code path already objects to in its own
comments.

**Cost, accepted:** a client that sends `false` makes the operator's switch
ineffective for its own requests, with nothing in the panel explaining why.

### D6 — vLLM leaves the routing capable-kind set

M4 measured the key as completely inert on vLLM. A switch that is offered,
defaults **on** for newly created applications, and provably delivers nothing is
worse than one that is not offered. `LiveTimingsCapableKind` becomes true for
llama.cpp alone.

**`internal/provider`'s `liveProgressUpstreams` keeps vLLM.** It gates a
different parameter pair on a different endpoint, and that parameter is a
first-class vLLM field which works there. The two sets therefore stop being one
set: the cross-package parity test relaxes from equality to `capable ⊆ gate`,
and the two comments that assert the sets are identical *for the same reason*
are rewritten to record that M4 retired that reason.

The size pin's failure message currently says to go and make the provider-side
gate agree — **the wrong half to follow here**; it is amended in the same edit.

The companions are not all mechanical: one is a live assertion outside both
packages. The portal's create-default table has a vLLM row expecting `true`,
and it fails the moment the predicate narrows. Its own doc comment says the
rows deliberately disagree, so the row is corrected rather than deleted.

**Stale rows persist.** The clear is prospective: a vLLM application created
between part 1 and part 2 keeps its stored `true` until its next save, when the
portal's clear fires. No migration and no one-time sweep — issue #81 decision
(e) puts that refusal in the portal, never in SQL, and the store's parity
fixture deliberately pins that every store path round-trips a `true` for any
kind.

### D7 — The capture keeps recording the client's bytes

Recording the injected key would make it visible to an operator debugging a 400.
It is deferred anyway: the capture already diverges from the upstream bytes
today, because the model rewrite re-serializes the object, and the code and the
security document both still claim the two are identical. Correcting that claim
is in scope (§4); changing what is captured is a behaviour change for every
passthrough request, and would put the internal provider model name in front of
the tenant. It belongs in its own change.

### D8 — The panel does not gain a "we asked" state

When the key is injected, the request succeeds, and no partial carries
`timings`, the row is byte-identical to a row where nothing was injected. After
D6 that case narrows to an old or unusual llama.cpp build.

No new DTO field, no fourth source label. The panel's job is to report the
number and where it came from; who asked for it is not a question the operator
is asking. Revisit if the case is ever observed.

### D9 — The injection is recorded in the request log

D3, D7 and D8 each defer something, and each deferral is defensible alone. Their
sum is not: if D2's residual ever fires, an operator opens the capture, sees the
**client's** body — a body that would not have earned that 400 — and has nothing
anywhere telling them the gateway added a key.

The cheapest honest close is neither a capture change nor a DTO field: the
existing per-request debug log line gains a field recording that the key was
injected. No wire change, no tenant-visible change, no new state to test in the
panel — and the operator debugging a 400 has one grep that explains it.

The operator-facing documentation states plainly that while this switch is on,
the recorded request body is **not** the body that was sent.

### D10 — The portal ships the control, on both surfaces

Part 1's reason for shipping none — *a visible toggle that did nothing would be
worse than the blank cell it promises to fix* — expires with this cut. Until it
ships, an operator with a **pre-existing** llama.cpp application has to PATCH the
API by hand, because migration 80's default is off and the kind-dependent `true`
applies only on create.

The control is a checkbox on the shared API-variant block that both forms
already render, which is stateless with required props — so each form supplies
its own state, its seed and its body field. The types come first: the two
request types on the application side, and the runtime spec's, where the request
type is derived by `Omit`, so **the field must be optional there** or the
`nil`-means-no-opinion semantics part 1 built are destroyed.

Three things make this harder than a checkbox:

- **The two surfaces learn capability from different inputs.** The application
  form knows `type` directly. The spec form has a **writable type select**, and
  the backend resolves the effective kind from that explicit type first, falling
  back to detection from the binary basename only when it is empty. So the spec
  form can mirror the application form's gating whenever a type is chosen, and
  needs a fallback only for the empty-type case, where the read-only echo of the
  loaded spec is its sole signal — undefined on create and stale once the binary
  is edited. Be permissive there and let the documented 400 answer.
- **The create default is on, so a plain `useState(false)` is wrong.** A
  checkbox that always sends `false` silently disagrees with the API's
  documented default and would turn the feature off for every application
  created through the portal.
- **The application form restates `type` on every save**, and the backend
  selects the 400 arm over the 409 precisely when the request carries a type. So
  an unconditional `true` on an incapable type does not merely fail to apply —
  it **blocks the whole save**. The control must not be able to send an
  impossible pair.

Part 1's three error codes have no entry in the portal's error-label map, so a
German-locale operator currently gets the machine code and an English sentence.
They are mapped in this cut.

---

## 4. Three sets of standing claims have to be rewritten

Each is a set of true-looking sentences that this cut makes false. Editing one
member of a set leaves the rest silently wrong. The docs check verifies links,
anchors and reachability; it cannot see a prose contradiction, and two of these
sites are test comments it cannot see at all.

**Set 1 — the injection prohibition.** Stated or cited in **at least nine**
places across the code and the canonical documents, including `proxyNative`'s
own doc comment 32 lines above the prohibition block, which says the only body
edit is the model rewrite. No single grep enumerates the set, because the sites
share no phrase. One member is **already stale**: it says the repository has not
captured whether llama.cpp attaches `timings` to Responses partials, which #80
measured and recorded one day after that comment was written.

**Set 2 — the capture-identity claim** (three sites, two in code and one in the
security document): that the passthrough capture's client bytes already equal
the upstream bytes. False today, before part 2 touches anything, because of the
model rewrite.

**Set 3 — the "no mid-stream source" claim** (three sites, two of them test
comments): that the Responses partials carry no usage at all, and that a
mid-stream rate exists only when the client asked. D4 makes both false.

**Set 4 — the "nothing reads the resolved flag" family**, eight members written
by part 1: three in `internal/routing` (the target field's doc comment, the
precedence test's "this test is the whole of the field's current contract", and
the capable-kind predicate's "nothing about it belongs to the request path"),
falsified by the gate; and **five sentences across three canonical documents**,
falsified by the injection. One of the five carries no claim of its own — it
says "for the same reason as the column above" — so it inherits its falsity by
reference, and reads as still-true prose if the sentence it points at is
corrected and it is not. Every one of the latter three also says "and nothing retries without
it", which D3 keeps **true** — so those sentences are edited, not deleted.

Note that Set 3's per-flavor table cell carries a second false clause beside the
one about mid-stream counts: the same cell says the gateway derives no rate on
this flavor, which D4's accepted consequence retires.

Also expiring with this cut: part 1's recorded reason for shipping no control —
*a visible toggle that did nothing would be worse than the blank cell it
promises to fix* — and the two documents that record the capable set as
`llama_cpp`/`vllm`.

The plan enumerates every site.

---

## 5. Hazards this design must respect

- **The positive path can ship completely untested with every existing test
  green.** The two "the relayed body must not grow the flag" assertions stay
  silent for a *gated* injection, because their fixture never sets the flag.
  Keep them — they still catch an ungated injection — and build new fixtures
  whose **resolved** targets have the flag on and a capable kind: one ordinary
  llama.cpp application, and one `server_agent` mapping whose runtime spec
  carries **both** the type and the flag, since spec-over-application precedence
  means a spec row without the flag resolves it back to false. The existing
  helper's application type is load-bearing for its other callers and stays as
  it is.
- **The gate's other four conditions have no negatives.** A gate that forgets
  the flavor check, the stream check or the veto leaves the whole suite green.
  Four named negatives on the positive fixture: `/v1/messages`; non-streaming
  `/v1/responses`; a seeded live-progress verdict of `no`; and a client-sent
  `false` — the last asserting the **value**, not merely the key's presence.
- **A test can stay green while its message goes false.** The client-timings
  test asserts a live output-token count of zero *because "the partials still
  report no usage"*. D4 makes that reason false; the assertion survives only on
  fixture shape, because its partials carry a `timings` object with no
  `predicted_n` — a shape llama.cpp was never observed to emit (90 of 90
  measured partials carry one). The fixture grows a `predicted_n` so the
  assertion restates the new rule, and a separate case keeps a `timings` object
  *without* one, which must still yield no count.
- **A flagged stream that ends without a terminal frame** now has per-frame
  rates where it previously had none. Whatever reaches the routing throughput
  EWMA must be the rate the stream reported, not the generation's peak — the
  defect #80 has just finished removing. Note that the **operator's switch**,
  not only a client, now selects requests into that population.
- **`target.Provider` is the wrong kind field** for a `server_agent` target.
- **The nil-map trap:** a `null` body decodes into a nil map and panics on
  assignment. It is unreachable on this path today, but the guard lives in a
  different file from the helper, so a new helper must not assume it.
- **Re-serializing changes key order and HTML-escapes `<>&`.** Only a flagged
  request whose model rewrite was a **no-op** newly pays this; a request whose
  model was already being rewritten pays it today.
- **The 32 MiB boundary.** The gateway does not cap these bodies — they carry
  base64 image data — but the server-agent router does, at 32 MiB. The injection
  adds roughly 25 bytes, so a request sitting inside that margin newly fails.
  Accepted: one request at one boundary.
- **A `true` can exist on an incapable row** — the store is policy-free by
  design and its parity fixture deliberately seeds exactly that — so the
  request-path gate re-checks the kind rather than trusting the portal's
  invariant.

---

## 6. Out of scope

The stripping of the injected `timings` from relayed frames — its own boolean,
as part 1 decided; M3 measures what it would recover: 2.49× on the wire.
`/v1/messages` in any form; vLLM's own live-rate parameter; changing what the
capture records; a freshness or endpoint-scoping rule for the capability
verdict; and refusing `live_progress` as a manual capability name.
