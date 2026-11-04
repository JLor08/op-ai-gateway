# Live-status row actions: transient start/stop, pin, per-instance scale (design)

Branch-local working file (removed before the PR). Approved by the user on 2026-11-04 ("so wie du empfiehlst").

## Goal

In the portal's live-status view, Play and Stop act once instead of setting a persistent override. A separate Pin button sets the override. On a replicable spec, Play keeps starting one more instance, and Stop on the highest running instance takes them down one at a time. Two log-view changes ship with this: opening the log from an instance row opens that instance's tab, and the "All" tab prefixes instance 0's lines with `[#0] ` when the view shows tabs.

## Decisions

- **D1 Play is transient.** It starts without persisting anything, and the model afterwards follows the normal idle, eviction and burst rules.
  - Non-replicable spec: Play ensures instance 0 and is shown only while the spec has no running or starting instance.
  - Replicable spec: Play stays visible while instances run and starts the next free instance, using the same index rule as the scaler (lowest free index; rows at crashed, start_failed or not_permitted count as free; an index whose address another mapping names, or that the scaler is ensuring right now, is skipped).
  - Play respects `max_instances` (0 means no cap beyond index 63). It does NOT apply the scaler's performance ceiling or host hold-off: it is the operator's explicit choice.
  - A burst instance still starts only into free room. The agent's 409 `runtime.instance_no_capacity` / `runtime.instance_demand_unknown` is shown to the operator.
- **D2 Stop is transient.** It stops one instance now and changes nothing in the config, so demand can start the instance again.
  - It is offered only on the row of the highest running or starting instance of the spec (non-replicable: instance 0).
  - It is refused for a protected instance (index < ProtectedCount = max(replicable ? min_instances : 0, pinned or force_running ? 1 : 0)), because the agent would restart it at once. The portal shows Stop disabled there, with a hover text that names the way out: clear the override, lower min instances, untick "Pinned", or use "Pin as stopped".
  - It is refused while a higher sibling runs (the highest index leaves first).
- **D3 Pin is one button that pins the current state.**
  - It is shown on the spec's primary row while `admin_state` is `''`.
  - If any instance of the spec is running or starting, it reads "Pin as running" and writes `force_running`; otherwise "Pin as stopped" and writes `force_stopped`. Both go through the existing full-spec PUT (`specBodyWithAdminState`).
  - "Clear override" stays as the unpin.
  - The rows' Force start and Force stop actions go away. The form's admin-override select keeps all three values.
  - Restart is unchanged.
- **D4** Play is disabled while the spec is `force_stopped`, with the hover "Pinned as stopped: clear the override first".
- **D5** Stop needs agent feature `runtime_stop` (agent 0.17.0). On an older agent Stop is shown disabled with a hover reason. Play works on every agent with `runtime_ensure`; an instance k >= 1 also needs `runtime_instances`.

## Contract

### Agent (server-agent 0.17.0)

- Feature `runtime_stop`, Since "0.17.0", in `internal/agent/features.go`. Version bump to 0.17.0.
- Route `POST /stop/{model}` on the router port, next to `/ensure`. `{model}` is an instance address (`m` or `m#k`), resolved by `Manager.ResolveAddress`.
- New owner command `Manager.Stop(ctx, model) error`. It begins a graceful drain (the existing `beginDrain` path) of that instance and records nothing persistent.
  - A burst instance is retired once its process has exited and nothing is pending.
  - An instance with queued requests may be started again by them, which is the normal demand behaviour.
- Responses (JSON error body as `/ensure` uses):
  - `200 {"state":"stopping"}` when a drain started or was already running.
  - `200 {"state":"stopped"}` when the instance has no process; this is idempotent.
  - `404 runtime.model_not_managed` for an unknown address or an index not allowed.
  - `409 runtime.instance_protected` when index < ProtectedCount.
  - `409 runtime.instance_not_highest` when a higher instance of the same spec has a process.
  - `200 {"state":"stopping"}` for a spec under `force_stopped` (it is already draining; nothing to do).

### Gateway (portal API)

- `POST /api/portal/mappings/{mappingID}/runtime/start`, no body.
  - Authorization as `PUT .../runtime-spec`.
  - The gateway picks the instance (D1) and calls the agent's `/ensure/{address}` (`provider.RuntimeEnsurer.EnsureRuntimeModel`) in the background, with a timeout derived from startup (as the scaler does).
  - It waits up to 5 s for an early answer:
    - Agent refusal within 5 s → mapped error: 409 `runtime.instance_no_capacity`, 409 `runtime.instance_demand_unknown`, or another mapped code.
    - Agent ready within 5 s → `200 {"instance":k,"state":"running"}`.
    - Neither → `202 {"instance":k,"state":"starting"}`. The status stream shows progress; a later failure is logged and visible on the row.
  - Refusals:
    - 409 `runtime.admin_stopped` (spec is force_stopped);
    - 409 `runtime.max_instances_reached`;
    - the existing benchmark-busy refusal;
    - 409 `runtime.agent_feature_missing` with `feature` (runtime_ensure, or runtime_instances for k >= 1);
    - 404 when the mapping has no runtime spec;
    - 503 when the server is not connected.
  - The scaler and the manual start must never pick the same index at the same time: share the index rule and the in-flight reservation.
- `POST /api/portal/mappings/{mappingID}/runtime/stop`, body `{"instance": k}`.
  - Same authorization.
  - Calls the agent's `/stop/{address}` through a new `provider` client method (bodiless POST, like `EnsureRuntimeModel`).
  - Returns 200 with the agent's state.
  - Refusals:
    - 409 `runtime.agent_feature_missing` (`runtime_stop`);
    - the agent's 409 `runtime.instance_protected` and `runtime.instance_not_highest`, passed through;
    - 404 `runtime.model_not_managed`;
    - 503 when the server is not connected.
- Both endpoints are refused during a benchmark on the server, like the spec PUT.
- Both are documented in `docs/architecture/reference/openapi.yaml` and the API surface. The new error codes go into the error-code tables.

### Portal

- `api/runtime.ts`: `startRuntime(mappingId)` and `stopRuntime(mappingId, instance)`.
- `RuntimeAdminSection.tsx` `statusActions` per D1 to D5:
  - Logs on every row.
  - Play on the primary row, per D1 and D4. For a replicable spec it stays while instances run; its hover says that it starts one more instance, and it is disabled with a reason at `max_instances`.
  - Stop on the row of the highest running or starting instance, per D2 and D5.
  - Pin on the primary row while admin_state is `''` (D3).
  - Clear override and Restart as today.
  - Refusals show as the existing row notice or snackbar with translated text (de + en) for every new error code.
- i18n parity in de and en.

## Docs

- `docs/architecture/cross-cutting/agent-runtime-manager.md`: the router's `/stop` route, the `runtime_stop` feature, the live-status actions.
- `docs/architecture/reference/openapi.yaml` and `api-surface.md`: the two endpoints.
- New ADR-095 in `09-architecture-decisions.md`: manual start and stop are transient, Pin is the persistent override, stop goes highest-index-first and never on a protected instance.
- `routing-and-model-selection.md` §10, if the scaler's index rule moves into a shared helper.
