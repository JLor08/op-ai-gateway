# Architecture

Clients call the Go backend through an internal load balancer. The backend
authenticates the caller, applies policy, selects an approved local upstream,
and emits audit/operational events. The React frontend consumes only the
management API; it never connects directly to model providers.

```text
Internal clients / React UI -> Go gateway -> approved local model providers
                               |-> audit log / metrics
                               |-> policy and identity store
```

All provider traffic should stay inside the organization's approved network.
