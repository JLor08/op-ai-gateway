# Plan: realtime session max lifetime and periodic re-check

Branch `claude/realtime-session-lifetime`, cut from `origin/main` 9f4a5746. Gateway-only change: no agent version bump. All repo
text is in English.

## Problem

A `/v1/realtime` session (`gateway/backend/internal/gateway/realtime_ws.go`, ADR-104) is admitted once, before the
upgrade:
- authentication: `requireInternalOrBearerAnyScope`, either the bearer leg or the internal loopback leg;
- the gate, rate limit, quota and budget: `inferencePreflight` → `admitPrincipal` → `PrincipalLimiter.Admit`.

After the upgrade it lives until a close, an error, the 15-minute idle bound or shutdown. A busy session can run for
hours. During that time:
- a key that is revoked or expires after the upgrade keeps relaying;
- a principal whose token quota or cost budget is exhausted keeps consuming;
- the session's own consumption reaches the limiter only when its single usage row is written at the end.

## Design

1. **Maximum lifetime.** New setting `OP_AI_GATEWAY_REALTIME_MAX_SESSION`, type `duration (0 = off)`, default `60m`
   (the cap OpenAI's Realtime API applies).
   - Only an explicit `0`/`0s` turns it off. An absent, empty, unparseable or negative value uses the default. This
     follows the `OP_AI_GATEWAY_VOICE_MUTE_TEARDOWN` convention; state it explicitly in config-env.md.
   - Read it in `internal/config` (Load), and pass it to the gateway Server the way the other server options are
     passed. It must be overridable in tests, like `realtimeIdle`.
   - When the bound is reached, `relayRealtime` ends the session with **1008** and reason `max_session_duration` on
     both hops, through the existing close path (`closeRealtime`), so the agent hop gets the same close.
2. **Periodic re-check.** Every `realtimeRecheckInterval` (60 s), the session re-checks its principal. The interval
   is a constant, overridable on the Server for tests. The check runs off the relay loop's goroutine, so a slow check
   never stalls relaying, and each check gets its own short timeout context.
   - **Authentication:** re-run the leg that admitted the session, against the same request: the clone
     `realtimeAuthRequest(r)` produces, which covers the subprotocol key.
     - The **bearer leg** is `s.Tokens.LookupBearer`. It is in-memory: deterministic, no I/O errors. The check fails
       when:
       - the key is revoked, inactive or expired;
       - the key now resolves to a different token ID;
       - the token no longer has `gateway:use` or `llm:invoke` (its scopes were edited).
     - The **loopback leg** is the internal secret plus a user lookup (`loopbackPrincipal`).
       - A user that is definitely gone or disabled fails the check.
       - A transient lookup error (anything other than a definite not-found or disabled) does **not** end the
         session. It is logged at Warn and retried on the next tick.
       - Check what `s.users.UserByID` returns for not-found. If there is no sentinel, treat every error as transient
         and say so in the comment.
   - **Limits:** a new `PrincipalLimiter.Check(ctx, p)` that runs **only** the token quota and cost budget checks of
     `Admit`.
     - It skips the rate bucket, which would count each tick as a request.
     - It skips the request quota, which governs admitting requests; the session was already admitted.
     - A store error fails open, as in Admit.
   - **Close reasons:** each failure ends the session with 1008 and a reason:
     - `token_invalid` for an authentication failure;
     - `token_quota_exceeded` for a token-quota denial;
     - `cost_budget_exceeded` for a cost-budget denial.
   - **Accepted residual (document it):** the session's own running consumption is not added to the aggregate. It is
     billed by the one row written at the end, and its cost is price-weighted in the store. The maximum lifetime is
     what bounds a single session's overrun.
3. **One usage row per session, always.** Every new end path goes through the existing `relayRealtime` →
   `realtimeOutcomeOf` → deferred `recordRealtimeUsage`.
   - Usage-row error codes distinguish the gateway-initiated ends:
     - `realtime.max_session_duration`
     - `realtime.token_invalid`
     - `limit.token_quota_exceeded`
     - `limit.cost_budget_exceeded`

     Reuse the existing limit codes from `writeLimitDenied`. Do not use the generic `realtime.close_1008`, which stays
     for idle and for a 1008 sent by a peer.
   - The idle end is unchanged.
   - The billing is unchanged: the measure is audio seconds, tokens or a logged zero. A session the gateway closes is
     still billed for what it relayed: its status is `error`, but its measure is what was metered.
   - Check how `recordRealtimeUsage` and the usage pipeline treat an `error` row that carries a measure. Today an
     idle-closed session (also 1008) is an `error` row and bills its appends. The new ends must bill exactly like the
     idle end. Verify this and pin it in tests.
4. **Active Requests and shutdown:** no change. The end paths run inside the existing session lifecycle.

## Tasks

### Task 1: config, limiter Check, the relay (gateway code)

**Files:**
- `internal/config/config.go` (+ test): the setting.
- The Server wiring: find where `realtimeIdle` and the other options live (server.go ~:501, and cmd/gateway's
  wiring).
- `principal_limits.go` (+ test): `Check`.
- `realtime_ws.go` (+ `realtime_ws_test.go`): the lifetime timer, the re-check goroutine, the end codes.

**Tests:**
- **Lifetime:**
  - Reaching the lifetime ends a session with 1008 `max_session_duration` on both hops. It leaves one usage row,
    with the right error code and the metered measure.
  - A setting of `0` disables the lifetime.
  - A check that blocks does not stall relaying, and the session still closes at the lifetime.
- **Authentication:**
  - A token revoked mid-session ends the session at the next check: 1008 `token_invalid`, one row.
  - A token whose scopes were removed ends the same way.
  - A session that authenticated with the subprotocol key is re-checked too.
  - A loopback session whose user is gone ends; a transient lookup error keeps it open.
- **Limits:**
  - Exhausting the token quota or the cost budget mid-session ends the session with the matching reason and code.
  - A store error in Check keeps the session open.
  - `Check` never touches the rate bucket or the request quota: a principal at its rate limit or request quota keeps
    its session.
- **Config:** the default is 60m, an explicit `0` turns it off, and junk or a negative value means the default.
- The race detector is clean on the realtime tests.

### Task 2: docs

- **ADR-104** (`09-architecture-decisions.md`): amend the decision with the lifetime and the re-check, move out the
  residuals this resolves, and add the accepted residual from design 2.
- **agent-runtime-manager.md §13:** update the realtime limitation entries.
- **compatibility-and-inference.md §3.11:** add the new close reasons and codes to the list of how a session ends.
- **reference/api-surface.md** (the `/v1/realtime` row) and **reference/openapi.yaml**, if it documents realtime close
  codes.
- **reference/config-env.md:** add the new variable, following the legend.
- Run `make lint-docs`.

## Gates

- From `gateway/backend`:
  - `golangci-lint fmt --diff`
  - `GOTOOLCHAIN=go1.26.5 golangci-lint run ./...`
  - `GOTOOLCHAIN=go1.26.5 go test ./... -skip 'TestVoice|TestLiveTranscriptionLoopback'`
  - `go test -count=1 ./internal/archtest/`
  - `go test -race -run Realtime ./internal/gateway/`
- `make lint-docs`
- Local Sonar: judge only the findings attributed to this branch.
