# Implementation status — #340 model-instance autoscaling (branch-local)

Spec: docs/superpowers/specs/2026-11-02-model-instance-autoscaling-design.md
Plan: docs/superpowers/plans/2026-11-02-model-instance-autoscaling.md (21 tasks)
Execution: subagent-driven (fresh implementer + review per task).

| Task | Status | Notes |
|---|---|---|
| 1 | done | agent wire fields (replicable/min/max, status instance/startup_ms), `runtime_instances` feature, agent 0.16.0; `Status.Instance/StartupMS` are declared but populated in Task 4 |
| 2 | done | `runtime/instance.go`: InstanceAddress/ParseInstanceAddress/InstanceKey, ProtectedCount, InstanceAllowed, IsBurst, `MaxInstanceIndex`=63 (pure, nothing calls them yet) |
| 3 | done | `runtime/policy.go`: siblings co-reside + pair lookup by base (`pairAllowed`, `baseOf`/`specBase`), sibling never evictable, lower index not while a higher sibling stays (queued ones discounted, so one decision evicts S#2 then S#1; fix commit), surplus victims first, `PolicySnapshot.NoEvict` → `Decision.NoCapacity` for every non-OK answer incl. terminals (step 8); `Spec.InstanceOf/Instance`, `RunningProc.Spec/Instance/Surplus` declared, populated in Task 4 |
| 4 | done | `runtime/manager.go`: one specState per instance key (`S`, `S#k`; per-instance copy via `specForInstance`), `ResolveAddress` (exact upstream first, then `m#k` + `InstanceAllowed`), protected instances 1..P-1 created+started on Apply, burst `m#k` created on demand and admitted with `NoEvict` (`NoCapacity` → `ErrInstanceNoCapacity`, no waiter/state/bootstrap), sibling-inherited demand for every instance (`demandSpec`; k≥1 unknown → burst `ErrInstanceDemandUnknown`, baseline waits for a measurement), protected = Pinned in the snapshot, Surplus for running burst, idle unload highest index first, burst retired on exit (crashed/start_failed kept until named again or next Apply), disallowed instances drain gracefully, `StartupMS`, start-triggered measurement never skipped; `min_instances` clamped to 64 |
