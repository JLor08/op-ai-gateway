# Architecture Tests

The dependency directions and layer boundaries described in this
documentation are not convention-only: they are enforced by **architecture
tests** that run as ordinary tests inside the existing suites (`go test
./...` and `npm test`), and therefore in CI, with no extra tooling. A change
that introduces a new dependency edge fails a test until the edge is
consciously added to a checked-in allowlist — turning every architectural
change into an explicit, reviewable decision.

No third-party architecture-testing framework is used. The Go side is built
on `golang.org/x/tools/go/packages` (BSD-licensed); the frontend side uses
the TypeScript compiler API that is already a devDependency.

## Go: dependency rules (`internal/archtest` in both modules)

Two test families per module:

1. **Frozen import graph** (`TestAllowlistFrozen`): every internal package's
   set of module-internal imports (production code only, tests excluded) is
   compared against a checked-in allowlist — 60 edges across 25 packages in
   `op-ai-gateway`, 21 edges across 11 packages in `op-ai-server-agent`. Any
   NEW edge fails with a message naming it; adding it to the list is the
   deliberate act.
2. **Forbidden edges** (`TestForbiddenEdges`): explicit never-rules that
   encode the architecture's load-bearing directions even if the allowlist
   is later loosened.

Gateway backend (`op-ai-gateway`):

| Rule | Why |
|---|---|
| no `internal/*` → `cmd/gateway` | the composition root is the top of the graph |
| `routing` ↛ `store`, `portal`, `gateway`, `compat` | routing is domain-core; `store` depends on `routing`, never the reverse |
| `store` ↛ `portal`, `gateway` | persistence sits below the service layer |
| `portal` ↛ `gateway`, `compat` | the service layer never reaches up into HTTP or flavor translation |
| `compat` imports only `inference` among internals | API-flavor knowledge stays contained in the translation layer |
| `tracing` ↛ `portal`, `account`, `provider` | those packages' tracing decorators live inside them via the OTel global, precisely to avoid this cycle |
| `inference`, `usage`, `apierror`, `storeerr`, `theme`, `auth` import no internal package | leaf vocabulary/utility packages |

Server-agent (`op-ai-server-agent`):

| Rule | Why |
|---|---|
| `gwapi`, `certfiles` are stdlib-only | leaf contracts (gateway transport, cert-dir file layout) shared by the feature packages |
| `collector` ↛ `proxy`, `trust`, `client`, `agent` | telemetry collection is independent of transport and TLS machinery |
| `sample` imports no internal package | the wire-format vocabulary is a leaf |
| `trust`, `proxy`, `certinstall` do not import each other | their only integration is the shared `certfiles`/`gwapi` leaves and the filesystem contract |

## Frontend: layer boundaries (`src/arch.test.ts`)

A Vitest test walks `src/**/*.{ts,tsx}` (tests excluded), extracts static
imports via the TypeScript compiler API, and asserts:

| Rule | Why |
|---|---|
| `src/api/**` imports nothing from `components/**`, `App.tsx`, `i18n.ts`, `theme/**` | the HTTP client layer is a leaf over `fetch` |
| `i18n.ts`, `currency.ts` have no relative imports at all | pure vocabulary leaves |
| `theme/**` may use the `api` barrel and `components/shared/**`, but no views | theming is a peer of the shared layer |
| `components/shared/**` imports no non-shared component | shared is a lower layer than the views |
| only `src/api.ts` may deep-import `src/api/<module>` | the barrel is the single client surface; `PortalApi` stays one type |
| nothing imports `main.tsx`; only `main.tsx` imports `App.tsx` | entry chain |
| outside `components/chat/**`, only `ChatStore.tsx` and `ChatSidebar.tsx` are importable | the chat run/persistence internals (`chatDoc`, `useChatRuns`, `useChatPersistence`) are encapsulated |

A sanity check on the walker (minimum file and edge counts) prevents a
silently broken scanner from making every rule pass vacuously.

## Working with the tests

- **A test failed after adding an import:** decide consciously. Either the
  import violates a layer rule (fix the code), or it is a legitimate new
  dependency (add the edge to the allowlist / rule table in the same commit,
  where the reviewer sees it).
- **Adding a new package/module:** it participates automatically; its first
  internal imports must be added to the allowlist.
- **Forbidden rules are not to be weakened casually** — each encodes a
  documented decision (see [Risks & Technical Debt](../11-risks-and-technical-debt.md)
  and the package doc comments the rules cite).

Related guards with the same philosophy: the memory-vs-SQL store conformance
harnesses (`internal/store/routing_store_conformance_test.go`,
`internal/portal/store_conformance_test.go`), the settings-domain
classification guard (`internal/portal/service_system_settings_touches_test.go`),
and the i18n key-parity compile check (`PortalMessages = typeof de`).
