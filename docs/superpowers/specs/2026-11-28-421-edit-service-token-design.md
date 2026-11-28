# #421 — Edit a service token's per-token options after creation

Branch-local working document (removed before the PR).

## Problem
Service tokens are create-only: `/api/portal/services/{id}/tokens/{tid}` is DELETE-only,
`/{tid}/rotate` replaces only the secret. Changing any option (claude_model_aliases,
claude_effort_compatibility, decisions_confidence_normalize, send_usage_limits,
codex_model_switch_prompt, vendor_access, overrides, redirect, logging) means deleting and
recreating — new id, new secret to redistribute. User tokens already have
`PATCH /api/portal/tokens/{id}` (`UpdateToken`).

## Design
### API
`PATCH /api/portal/services/{id}/tokens/{tid}` → `200` + `ServiceTokenDTO`.
Body `UpdateServiceTokenRequest`, every field a pointer: absent = keep, present = replace
(the `UpdateTokenRequest` semantics).

Editable fields (exactly the create request's settings):
`name`, `model_override`, `model_override_map`, `log_communication`, `secret`,
`unknown_model_redirect`, `unknown_model_redirect_blocked`, `unknown_model_fallback`,
`vendor_access`, `claude_model_aliases`, `claude_effort_compatibility`,
`decisions_confidence_normalize`, `send_usage_limits`, `codex_model_switch_prompt`.

Not editable (and absent from the request type, so unknown JSON keys are ignored like
everywhere else): `scopes` (always fixed `[llm:invoke]`), `status` (a service token's
liveness follows its service), `expires_at` (UpdateTokenRequest has none either),
`last_used_model` (inference path only), project / server override (not service-token
concepts). Out of scope per issue: rotation options.

### Service layer — `portal.Service.UpdateServiceToken(ctx, principal, serviceID, tokenID, req)`
Order (mirrors RotateServiceToken / DeleteServiceToken and UpdateToken):
1. `authorizeServiceTokens` (admin or any delegate tier; no-leak 404 `ErrServiceNotFound`).
2. `serviceTokenByID` (cross-service / unknown / non-service id → `ErrTokenNotFound`).
3. Management gate: `req.ClaudeEffortCompatibility != nil && !principalMayManageTokenSetting(principal)`
   → `ErrPrincipalForbidden`, before any field is applied (any value, on or off — the
   UpdateToken rule; on create only `true` is gated).
4. Apply fields in memory, validating with exactly the create-path validators:
   - name: trimmed, empty → `ErrServiceValidation` (create's rule; no uniqueness check, create has none).
   - model_override / model_override_map: `validateModelOverride[Rules]` against
     `callableModelNames(principal)`, built lazily at most once (UpdateToken's pattern).
   - redirect trio: validated together whenever any is present, against the values the
     update leaves, with the fallback checked against callable ∩ the service's allowlist
     (`intersectAllowedModels`, create's rule — see the long comment in CreateServiceToken).
   - vendor_access: `validateVendorAccess(ctx, serviceTokenPrincipal(svc.ID), …)` — reach of
     the SERVICE, never of the editing principal; non-nil replaces wholesale.
   - the five booleans + log_communication + secret: plain replace.
5. `UpdatedAt = now`; `s.tokens.UpdateTokenMetadata(ctx, record)` once.
6. **`s.mirrorServiceTokenState(ctx, svc.ID, svc.Status)` after the write.** Required: the
   memory driver's `UpdateTokenMetadata` rebuilds the cached bearer wholesale with
   `Active` from the token status alone and no `AllowedModels`, so without the mirror an
   edit would re-activate a disabled service's token and drop its allowlist (memory mode).
   SQL drivers join live per request; the mirror is a no-op there.
7. Return `serviceTokenDTO(record)`.

### Gateway layer
`handlePortalServiceTokenSingle` accepts DELETE and PATCH (Allow header lists both);
PATCH reads raw JSON (`readRawJSON`), malformed → `400 request.invalid_json`, errors via
`writePortalServiceError(w, err, "service.token_update_failed")`. Check that every sentinel
the update can return (ErrTokenNotFound, ErrPrincipalForbidden, override/redirect/vendor
validation errors, ErrServiceValidation) maps to the same status/code as on create.

### Portal UI — `ServiceTokensSection.tsx`
- `api.services.updateServiceToken(serviceId, tokenId, body)` (PATCH) + `UpdateServiceTokenRequest` type in `src/api/services.ts`.
- An "Edit" row action (EditIcon) opens the existing create form in edit mode, prefilled
  from the row (name, override rows/catch-all, redirect trio, the five switches, logging,
  secret flag, vendor-access entries). Submit sends PATCH with the full settings
  (a read-and-write-back is safe: every field is always emitted or omitempty-and-defaulted),
  replaces the row in the list, closes the form; no secret dialog.
- Title/submit label switch between create and edit; the management-only effort switch keeps
  whatever visibility rule it has on create.
- i18n keys de + en together.

### Docs (same branch)
Replace every "create-only / no field-level update / editing is #421" statement for service
tokens: `reference/api-surface.md` (routes table l.~162, field table rows, vendor-access
section, l.~1983 summary, `portal.principal_forbidden` row), `reference/openapi.yaml`
(new PATCH operation + request schema, ServiceToken description), `reference/data-model.md`,
`12-glossary.md` (vendor access entry), `cross-cutting/external-vendor-accounts.md` §11,
`cross-cutting/security-auth-rbac.md`, and Go doc comments in `service_services.go`.
ADR log: add a dated amendment note to ADR-050 (service-token vendor policy no longer
create-only) and ADR-110 / the line mentioning #421; no new ADR unless a real decision
arises. No server-agent change → no agent version bump.

### Tests
- portal: success per field group; absent keeps; cross-service/unknown → ErrTokenNotFound;
  stranger → ErrServiceNotFound; Token-Delegate allowed; effort flag from inference-only
  bearer → ErrPrincipalForbidden and nothing written; fallback outside service allowlist
  rejected; vendor access validated against the service; empty name rejected;
  memory-mode: editing a disabled service's token keeps the bearer inactive and keeps its
  AllowedModels; secret/prefix unchanged.
- gateway endpoint: PATCH 200 shape, 400 invalid JSON, 404 cross-service, 403 effort gate,
  405 + Allow on other methods.
- frontend: edit opens prefilled, PATCH body, list row updated, error surface.
