# Agent LAN Interfaces and Agent Key — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server agent reports its network interfaces. The gateway derives
each server's effective LAN address from a per-server source: `auto`, a chosen
interface, or `manual`. `lan` RPC clusters use that address and are rebuilt
when it changes. A new per-agent `agent_key` provides keyed interface ids and
encrypts the upstream token in the agent's runtime-config cache (closes #61).

**Architecture:**
- `agent_key` is a 64-hex config setting. The agent derives one subkey per
  purpose from it with HKDF-SHA256.
- The interface list rides on the live telemetry `Sample`, using the
  `Binaries` pattern: a pointer field, attached only on change, resent on
  reconnect.
- The gateway stores the last list in `ai_servers` through a targeted writer.
- `resolveLANAddress` in the portal package replaces every `lan_address` read
  in the cluster derivation and the save checks.
- The orchestrator diffs successive cluster plans to record address changes on
  `ClusterStatusDTO`.

**Tech Stack:** Go 1.26 (server-agent; stdlib `crypto/hkdf`, `crypto/aes` +
`cipher.NewGCM`, `crypto/hmac`), Go 1.25 (gateway backend), SQLite + Postgres
via `internal/store`, React + TypeScript + Vitest (portal).

**Spec:** `docs/superpowers/specs/2026-11-26-agent-lan-interfaces-design.md`

## Global Constraints

- Branch `feat/agent-lan-interfaces`. Never commit to or merge into `main`.
  Commits use Conventional Commit prefixes and carry substantive bodies.
- Repo text in English. No real LAN host names or addresses anywhere, fixtures
  included. Use 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and 10.0.0.0/8
  examples.
- IPv4 only. `mesh` clusters are unchanged.
- The raw MAC never leaves the host. `agent_key` never leaves the host and is
  never logged, never placed in a URL, never stored by the gateway.
- A NetBird address is never a LAN candidate. Drop 100.64.0.0/10, and the
  gateway also drops any address equal to the server's `netbird_ip`.
- Existing setups do not change:
  - a server with `lan_address` resolves `manual`;
  - a non-NetBird server with a `Domain` keeps dialing its `Domain` under
    `auto`;
  - the rollout relaunches no cluster.
- Under `lan_bind_only`, a missing chosen interface withholds the member and
  never falls back. Unknown never widens to `0.0.0.0`.
- One ServerAgent version bump for the whole branch (AGENTS.md "ServerAgent
  Version Rule"). It is done in Task 12, based on `main`'s current version at
  that time.
- The store must work for memory, SQLite and Postgres. Run the Postgres leg
  with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set, as CI does.
- Per Go module, CI runs `golangci-lint fmt --diff` and `golangci-lint run`.
  The portal CI runs `npm run format:check`.
- Update `docs/architecture/` in the same task as the behavior it describes.

## Review Focus

1. **Several IPv4s on one interface** (a secondary address). The pick is
   stable across reports: the default-route source address if it is on that
   interface, else the numerically lowest RFC 1918 address. Test in Task 4.
2. **NetBird address slipping through.** macOS names the NetBird tun `utun*`,
   not `wt0`. An address in 100.64/10, or equal to `netbird_ip`, never becomes
   effective or part of the head identity. Tests in Task 4 (agent) and Task 8
   (gateway).
3. **A flapping cable.** Down for 3 s, then up again: no report change and no
   cluster notify. Tests in Task 5 (stability window) and Task 8 (notify only
   on an effective change).
4. **`agent_key` added after an interface was chosen by name.** The stored id
   is a name and the reported ids become keyed. The choice must still resolve
   through the name-match rule, with no fallback warning. Test in Task 7.
5. **Gateway restart before the agent reconnects.** Clusters derive from the
   persisted list. Test in Task 6 (store round-trip) and Task 7 (resolution
   reads only the stored row).

---

## Phase 1 — Agent key

### Task 1: `agent_key` setting and subkey derivation

**Files:**
- Create: `server-agent/internal/agentkey/agentkey.go`,
  `server-agent/internal/agentkey/agentkey_test.go`
- Modify: `server-agent/internal/config/config.go`
  - `Config` (~122): add `AgentKey string`, the 64-hex text.
  - `fileConfig` (~439): add `AgentKey string \`json:"agent_key"\``.
  - Resolve in `Load` from env `OP_AGENT_AGENT_KEY` or the file. Use the
    `resolveTrimmed` pattern. Add **no flag**: a secret on the command line
    shows in `ps`.
  - Validate the key in `Validate`.
- Modify: `server-agent/internal/config/config_test.go`,
  `server-agent/testdata/server-agent.config.jsonc` (add the key with its
  fixture value and an English `//` comment), `server-agent/README.md`
  (settings table), `server-agent/main.go` (startup log logs only
  `"agent_key_configured", cfg.AgentKey != ""`)
- Docs: `docs/architecture/reference/config-env.md`,
  `docs/architecture/cross-cutting/configuration.md`

**Interfaces:**
- Produces, in package `agentkey`:
  - `type Key struct` (unexported fields)
  - `func Parse(hexKey string) (*Key, error)` returns `nil, nil` for `""` and
    an error unless the key is exactly 64 hex characters.
  - `func (k *Key) Subkey(purpose string) []byte` returns 32 bytes.
    `hkdf.Key(sha256.New, master, nil, purpose, 32)`. A nil `*Key` returns
    nil.
  - `const PurposeInterfaceID = "op-ai-gateway/interface-id/v1"`
  - `const PurposeRuntimeCache = "op-ai-gateway/runtime-cache/v1"`
- Produces: `config.Config.AgentKey string`. `Validate` rejects a non-empty
  value that `agentkey.Parse` rejects, with
  `agent_key must be 64 hex characters (32 bytes)`.
- Golden fixture value:
  `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`

- [ ] **Step 1: Write failing tests**
  - `TestParseRejectsBadKeys`: `""` → nil, nil. Length 63, length 65 and
    non-hex input → error.
  - `TestSubkeyIsPerPurposeAndDeterministic`: same purpose gives equal bytes;
    different purposes differ; length is 32; a nil `*Key` returns nil.
  - `TestAgentKeyPrecedence` in config: env beats file, and both are trimmed.
  - `TestAgentKeyValidation`: a bad key makes `Load` fail with the message
    above.
- [ ] **Step 2: Run them.** `cd server-agent && go test ./internal/agentkey ./internal/config -run 'Parse|Subkey|AgentKey'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `agentkey.go` and the config wiring above. Add the
  golden line and the README row. `TestGeneratedAgentConfigLoadsToDocumentedDefaults`
  must assert that the golden's `agent_key` loads to the fixture value.
- [ ] **Step 4: Run** `cd server-agent && go test ./internal/agentkey ./internal/config`. Expected: PASS, including the golden, README and reflection tests.
- [ ] **Step 5: Commit** `feat(agent): add the agent_key setting and per-purpose subkeys`

### Task 2: Generated configs carry a fresh `agent_key`

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_binaries.go`
  - `buildAgentConfigJSON` (~362) gets a third parameter `agentKey string`,
    interpolated with `json.Marshal` after `token`.
  - `serveAgentConfig` (~282) generates the key with
    `newAgentConfigKey() (string, error)`: 32 bytes from `crypto/rand`, hex.
    A rand failure gives 500. The key is never logged.
- Modify: `gateway/backend/internal/gateway/agent_binaries_test.go`,
  `gateway/backend/internal/gateway/server_test.go`
  (`TestAgentTokenResponsesAndDirectDownloadShareConfigMaterial`; the material
  stays key-free, so its struct equality holds)
- Modify: `gateway/frontend/src/components/AgentTokenSection.tsx`
  - `buildServerAgentConfig(config, token, agentKey: string)`
  - `downloadConfig` generates the key with
    `crypto.getRandomValues(new Uint8Array(32))` → hex.
- Modify: `gateway/frontend/src/components/AgentTokenSection.test.tsx`

**Interfaces:**
- Consumes: the Task 1 golden fixture value.
- Produces:
  - `buildAgentConfigJSON(material agentConfigMaterial, token, agentKey string) string`
  - `buildServerAgentConfig(config: AgentConfigMaterial, token: string, agentKey: string): string`
  - `agentConfigMaterial` / `AgentConfigMaterial` are unchanged.

- [ ] **Step 1: Write failing tests**
  - Both golden tests pass the fixture key and still compare byte for byte.
  - `TestServeAgentConfigEmitsFreshAgentKey`: two downloads → each
    `agent_key` matches `^[0-9a-f]{64}$`, and the two differ.
  - Frontend: `'downloadConfig emits a fresh 64-hex agent_key per download'`,
    with the same assertions.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway -run 'AgentConfig|ServeAgentConfig'` and `cd gateway/frontend && npx vitest run src/components/AgentTokenSection.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement** the parameter on both producers and the key
  generation at both call sites. Update every other `buildServerAgentConfig`
  call site (grep).
- [ ] **Step 4: Run** the same commands. Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): generated agent configs carry a fresh agent_key`

## Phase 2 — Encrypted token in the runtime-config cache (#61)

### Task 3: Seal `api_token` in the disk cache

**Files:**
- Create: `server-agent/internal/runtime/cache_seal.go`,
  `server-agent/internal/runtime/cache_seal_test.go`
- Modify: `server-agent/internal/runtime/config_client.go`
  - `NewGatewaySource` (~194) delegates to the new constructor with a nil key.
  - `loadCachedConfig` and `persist` (~304) use the seal/open functions.
  - Correct the `writeFileAtomic` comment (~83-95) about secrets.
- Modify: `server-agent/internal/runtime/types.go:104-108` (the
  `Spec.APIToken` comment describes the cache copy: sealed with `agent_key`,
  else plaintext 0600)
- Modify: `server-agent/main.go:271`: pass
  `key.Subkey(agentkey.PurposeRuntimeCache)`, where `key` comes from
  `agentkey.Parse(cfg.AgentKey)`.
- Docs: the token section of `docs/architecture/cross-cutting/agent-runtime-manager.md`
  (around `${API_TOKEN}`) describes the disk copy and both modes.

**Interfaces:**
- Consumes: `agentkey.PurposeRuntimeCache`, `(*agentkey.Key).Subkey`.
- Produces:
  - `func NewGatewaySourceWithCacheKey(gatewayURL, token string, client *http.Client, cachePath string, cacheKey []byte) *GatewaySource`
    (nil key = today's behavior).
  - `func sealCacheTokens(cfg Config, key []byte) (Config, error)` returns a
    deep copy whose non-empty `Specs[i].APIToken` are replaced by
    `"sealed:v1:" + base64.RawStdEncoding(nonce‖ciphertext)`. It uses
    AES-256-GCM, a random 12-byte nonce and AAD = `[]byte(spec.ID)`. The
    input is never mutated.
  - `func openCacheTokens(cfg Config, key []byte) Config`
    - A token without the prefix is kept (an old plaintext cache).
    - A prefixed token that fails to open, or one met with a nil key, becomes
      `""`, logged once per spec at Warn as
      `runtime: cached api_token could not be opened; waiting for the gateway`
      with `spec_id`. Never the value.

- [ ] **Step 1: Write failing tests**
  - `TestPersistSealsAPITokenWithKey`: the cache file bytes do not contain
    the token, `api_token` starts with `sealed:v1:`, and the mode is still
    0600.
  - `TestCacheReloadWithSameKeyRestoresToken`.
  - `TestCacheReloadWithOtherKeyDropsTokenKeepsSpec`: the spec is present
    and `APIToken == ""`.
  - `TestPlaintextCacheIsReadAndResealedOnNextPersist`.
  - `TestNoKeyKeepsPlaintextCache`: today's behavior, so the existing
    `TestGatewaySourceFetchAndCache` stays green.
  - `TestSealDoesNotMutateInMemoryConfig`: the in-memory `cached` still holds
    the plaintext token.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime -run 'Seal|Cache|GatewaySource'`. Expected: the new tests FAIL.
- [ ] **Step 3: Implement** `cache_seal.go`, the new constructor and the
  comment and doc fixes. Wire it in `main.go`.
- [ ] **Step 4: Run** `cd server-agent && go test ./internal/runtime ./internal/agent`. Expected: PASS.
- [ ] **Step 5: Commit** `fix(agent): seal the upstream token in the runtime-config cache (closes #61)`.
  The body states both modes and why the token stays cached.

## Phase 3 — Agent interface reporting

### Task 4: Interface collector

**Files:**
- Create: `server-agent/internal/collector/netif.go`,
  `server-agent/internal/collector/netif_test.go`
- Modify: `server-agent/internal/sample/sample.go` (new type and field near
  `Binaries`, ~98)

**Interfaces:**
- Produces in `sample`:
  ```go
  type NetInterface struct {
      ID        string   `json:"id"`
      IDKind    string   `json:"id_kind"` // "keyed" | "name"
      Name      string   `json:"name"`
      IPv4      []string `json:"ipv4"`      // sorted; Primary first
      Virtual   bool     `json:"virtual"`
      Preferred bool     `json:"preferred"` // exactly one or none per list
  }
  // in Sample:
  Interfaces *[]NetInterface `json:"interfaces,omitempty"` // nil = unchanged
  ```
- Produces in `collector`:
  - `type InterfaceCollector struct` with injectable `interfaces func() ([]net.Interface, error)`,
    `addrs func(net.Interface) ([]net.Addr, error)` and `routeSource func() (net.IP, error)`.
  - `func NewInterfaceCollector(idKey []byte) *InterfaceCollector`
  - `func (c *InterfaceCollector) Collect(ctx context.Context) ([]sample.NetInterface, error)`

**Rules (from spec §2.1, §4b and §5):**
- **Include** an interface only if it is up, not loopback, and has at least
  one IPv4 outside 169.254/16, 100.64/10 and 127/8.
- **Exclude** names matching `^wt[0-9]+$` (NetBird).
- **Virtual** when the name has one of these prefixes, case-insensitive:
  `docker`, `br-`, `veth`, `virbr`, `vethernet`, `vmnet`, `vboxnet`, `tun`,
  `tap`, `utun`, `zt`, `tailscale`, `wg`, `lxc`, `cni`, `flannel`.
- **IPv4 order:**
  1. the route source address, if it is on this interface;
  2. then RFC 1918 addresses, numerically ascending;
  3. then the rest, ascending.
- **`routeSource`** is the local address of `net.Dial("udp4", "192.0.2.1:9")`.
  UDP connect sends no packet. On failure there is no route source.
- **Preferred** goes to one interface, ranked in this order. The comparison
  is stable.
  1. The interface holding the route source.
  2. It has an RFC 1918 address.
  3. It is not virtual.
  4. Name order, as the tiebreaker.
- **ID:**
  - With a non-nil `idKey`: `IDKind "keyed"`, and `ID` = hex of the first
    16 bytes of HMAC-SHA256(idKey, `HardwareAddr`), or of the name when the
    `HardwareAddr` is empty.
  - Without a key: `IDKind "name"`, `ID = Name`.
- Output is sorted by `Name`.

- [ ] **Step 1: Write failing tests** with fake functions:
  - `TestCollectFiltersLoopbackDownLinkLocalAndNetbird`: `wt0` and `utun5`
    carry only 100.64.0.5, so neither appears.
  - `TestCollectOrdersAddressesRouteSourceFirst`: 10.0.0.9 and 10.0.0.3 plus
    route source 10.0.0.9 → `["10.0.0.9","10.0.0.3"]`.
  - `TestCollectPrefersRouteSourceThenPhysical`.
  - `TestCollectMarksVirtualInterfaces`: `vEthernet (WSL)` and `docker0` are
    virtual.
  - `TestCollectKeyedIDsStableAndHideMAC`: the same key and MAC give the same
    ID; the ID does not contain the MAC hex; two keys give different IDs.
  - `TestCollectNameIDsWithoutKey`.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector -run Collect`. Expected: FAIL.
- [ ] **Step 3: Implement** `netif.go` (stdlib `net`, `crypto/hmac`,
  `net/netip`; no OS-specific files).
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): collect LAN interface candidates`

### Task 5: Deliver interfaces on the telemetry sample

**Files:**
- Create: `server-agent/internal/agent/netif.go`,
  `server-agent/internal/agent/netif_test.go`
- Modify: `server-agent/internal/agent/agent.go`
  - `Agent` (~788) and `Deps` (~976) gain `Interfaces *collector.InterfaceCollector`;
    `NewFromDeps` copies it.
  - `collectOnce` (~2418) attaches `pending()`, and the post-success callback
    (~2682) calls `delivered`.
  - The reconnect and `reportTicker` paths (~1215, ~1239) call `resend()`,
    as `resendBinaries` does.
- Modify: `server-agent/main.go`: construct
  `collector.NewInterfaceCollector(key.Subkey(agentkey.PurposeInterfaceID))`.

**Interfaces:**
- Consumes: Task 4 `InterfaceCollector.Collect`, `sample.Sample.Interfaces`.
- Produces: `type interfaceInventory struct`, modelled on `binaryInventory`
  (`binaries.go:682-723`), with these methods:
  - `observe(now time.Time, list []sample.NetInterface)`
  - `pending() *[]sample.NetInterface`
  - `delivered(sum [32]byte)`
  - `resend()`
- `const interfaceStableFor = 10 * time.Second`
- `const interfaceCollectEvery = 5 * time.Second`
- `pending()` returns the list only when it differs (sha256 of the canonical
  JSON) from the last delivered one **and** has been observed unchanged for
  `interfaceStableFor`. The first observation after start is pending at once.
  An empty, stable list is sent as `[]`, not nil.

- [ ] **Step 1: Write failing tests** with an injected clock:
  - `TestInterfaceInventoryFirstListSentImmediately`.
  - `TestInterfaceInventoryWaitsForStability`: change at t0 → not pending at
    t0+5s, pending at t0+10s.
  - `TestInterfaceInventoryFlapIsSuppressed`: A → B for 3 s → A gives no
    pending.
  - `TestInterfaceInventoryResendAfterReconnect`.
  - `TestInterfaceInventoryEmptyListIsSent`.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/agent -run InterfaceInventory`. Expected: FAIL.
- [ ] **Step 3: Implement** the inventory and the wiring. Collection is
  throttled to `interfaceCollectEvery` inside `collectOnce`, and errors are
  logged at Debug without failing the sample.
- [ ] **Step 4: Run** `cd server-agent && go test ./internal/agent ./internal/client`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): report LAN interfaces on the telemetry sample`

## Phase 4 — Gateway storage and effective address

### Task 6: Migration 110 and the targeted writer

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go`: append
  `{version: 110, name: "server_lan_interfaces", up: migration110Up}`.
  `migration110Up` adds these columns on `ai_servers` via `addColumnIfMissing`:
  - `lan_interfaces text not null default ''` (`''` = never reported,
    `'[]'` = reported empty)
  - `lan_source text not null default ''`
  - `lan_interface_id text not null default ''`

  It then runs `update ai_servers set lan_source = case when lan_address <> '' then 'manual' else 'auto' end where lan_source = ''`.
- Create: `gateway/backend/internal/store/migration110_lan_interfaces_test.go`
- Modify: `gateway/backend/internal/routing/store.go`
  - `AIServer` gains `LANSource string`, `LANInterfaceID string` and
    `LANInterfaces string` (sanitized JSON array).
  - New interface `LANInterfacesStore` with
    `UpdateServerLANInterfaces(ctx context.Context, id, interfacesJSON string) error`,
    embedded in `Store`.
  - The `UpdateAIServer` doc lists the new excluded column.
- Modify: `gateway/backend/internal/routing/memory_store.go`
  (`UpdateAIServer` copies `LANInterfaces` back; new writer; `copyAIServer`)
- Modify: `gateway/backend/internal/store/sqlite_routes.go`
  - Insert, the `UpdateAIServer` set list (adds `lan_source` and
    `lan_interface_id`, **not** `lan_interfaces`), the five select lists and
    `scanAIServer`, and the new writer with `requireAffected`.
- Regenerate: `cd gateway/backend && go generate ./internal/tracing/...`
- Modify tests: `server_narrow_writers_conformance_test.go`,
  `ai_server_column_parity_test.go` (seed values and the `seeded` list in
  `TestAIServersSchemaColumnsAllCovered`), `sqlite_migration_test.go`
  (migration count)
- Docs: `docs/architecture/reference/data-model.md` (columns at ~77, a
  migration table row for 110)

**Interfaces:**
- Produces: `routing.AIServer.{LANSource, LANInterfaceID, LANInterfaces}`, and
  `(routing.Store).UpdateServerLANInterfaces(ctx, id, interfacesJSON string) error`
  (unknown id → `storeerr.ErrNotFound`).
- Produces: `routing.LANSourceAuto = "auto"`, `LANSourceInterface = "interface"`
  and `LANSourceManual = "manual"`.

- [ ] **Step 1: Write failing tests**
  - `TestMigration110AddsLANInterfaceColumns`: a row with `lan_address`
    becomes `manual`, one without becomes `auto`, and `lan_interfaces` is
    `''`.
  - `TestConformanceUpdateAIServerLeavesLANInterfaces`.
  - `TestConformanceUpdateServerLANInterfacesNarrowWriter`: it changes only
    that column, and an unknown id gives `ErrNotFound`.
  - The parity tests are extended.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store ./internal/routing -run 'Migration110|LANInterfaces|Parity|Columns'`. Expected: FAIL.
- [ ] **Step 3: Implement** the migration, the struct fields, both stores and
  the tracing regeneration.
- [ ] **Step 4: Run**
  `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:5432/op_test?sslmode=disable go test ./internal/store ./internal/routing ./internal/tracing`
  against a local `postgres:17-alpine`, run alone. Expected: PASS, with no
  skipped Postgres subtests.
- [ ] **Step 5: Commit** `feat(store): persist agent-reported LAN interfaces and the LAN address source`

### Task 7: Effective LAN address in the cluster derivation

**Files:**
- Create: `gateway/backend/internal/portal/service_lan_address.go`,
  `gateway/backend/internal/portal/service_lan_address_test.go`
- Modify: `gateway/backend/internal/portal/service_cluster_config.go`
  - `clusterAddress` (~518), `memberBindIP` (~542) and `clusterHeadIdentity`
    (~558).
  - New constant
    `ClusterWithheldLANInterfaceMissing = "runtime.cluster_lan_interface_missing"`
    next to the others (~46-62).
- Modify: `gateway/backend/internal/portal/service_cluster.go` (`lanAddressKnown`, ~929)
- Modify tests: `service_cluster_config_test.go`
  (`TestAgentRuntimeConfigLanUsesLanAddressElseDomain` keeps passing
  unchanged for `manual` rows), `service_cluster_test.go`
  (`TestPutClusterSaveChecks` case ~480)
- Docs: `docs/architecture/cross-cutting/networking-mesh.md` §12,
  `agent-runtime-manager.md` §3.7 (addressing)

**Interfaces:**
- Consumes: Task 6 fields.
- Produces:
  ```go
  type LANInterfaceView struct { ID, IDKind, Name string; IPv4 []string; Virtual, Preferred bool }
  type LANResolution struct {
      Source        string   // effective: "manual" | "interface" | "auto" | "domain" | ""
      Address       string   // effective IPv4; "" when none (or Source "domain")
      InterfaceID   string
      InterfaceName string
      Fallback      bool     // chosen interface missing, auto pick in use
      IdentityIPs   []string // head identity: manual → [Address]; interface/auto → every reported IPv4, sorted; domain → nil
      Reported      bool     // agent has reported a list (lan_interfaces != '')
      KeyedIDs      bool     // every reported id_kind == "keyed"
      Interfaces    []LANInterfaceView
  }
  func LANResolutionOf(srv routing.AIServer) LANResolution
  ```

**Resolution rules (spec §2.2 and §2.4):**
1. **Source, read from the row.** Use `LANSource`. If it is `''`, it means
   `manual` when `LANAddress != ""`, else `auto`.
2. **`manual`:** `Address = LANAddress`.
3. **`interface`:**
   - Look the interface up by `ID == LANInterfaceID`. Failing that, take the
     interface whose `Name == LANInterfaceID`. This is the key-introduction
     rule (Review Focus 4).
   - Found with an IPv4: `Address = IPv4[0]`.
   - Otherwise: `Fallback = true`, and the auto pick applies.
4. **`auto`:**
   - When `!NetbirdEnabled && Domain != ""`, the result is `Source "domain"`.
   - Otherwise take the `Preferred` interface, else the first non-virtual
     interface, else none.

**Changes in the cluster derivation:**
- `clusterAddress` (lan branch):
  - Use `Address` when it is set.
  - `Source "domain"` returns the `Domain`.
  - Anything else gives `ClusterWithheldAddressUnknown`.
- `memberBindIP` with `LANBindOnly`:
  - `Fallback` gives `ClusterWithheldLANInterfaceMissing`.
  - An empty `Address` gives `ClusterWithheldAddressUnknown`.
- `clusterHeadIdentity` (lan): `IdentityIPs`, or `([]string{}, Domain)` for
  `Source "domain"`.
- `lanAddressKnown`: `Address != "" || Source == "domain"`.

- [ ] **Step 1: Write failing tests**
  - `TestLANResolutionOf`, table-driven. One case per rule above, plus:
    - an interface choice missing → `Fallback`, preferred used;
    - a name-kind stored id with keyed reports → resolved by name, no
      fallback;
    - a NetBird server under `auto` with no report → empty;
    - a non-NetBird server with Domain and a report → `domain`;
    - a stored row only, no live agent (Review Focus 5).
  - `TestBindOnlyMemberWithheldWhenInterfaceMissing`.
  - `TestHeadIdentityCarriesAllReportedIPv4s`.
  - `TestExistingManualAndDomainSetupsDeriveUnchanged`: the generation and
    the endpoints equal the pre-change values for a `manual` row and a
    `Domain` row.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal -run 'LANResolution|BindOnlyMember|HeadIdentity|ExistingManual'`. Expected: FAIL.
- [ ] **Step 3: Implement** `service_lan_address.go` and switch the four
  readers.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/portal`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): derive lan cluster addresses from the agent-reported interfaces`

### Task 8: Ingest the interface list

**Files:**
- Create: `gateway/backend/internal/gateway/agent_ingest_lan.go`,
  `gateway/backend/internal/gateway/agent_ingest_lan_test.go`
- Modify: `gateway/backend/internal/gateway/agent_ingest.go`
  - `agentTelemetryRequest` (~126) gains
    `Interfaces *[]agentNetInterface \`json:"interfaces,omitempty"\``.
  - `ingestTelemetrySample` calls `writeBackLANInterfaces` next to
    `writeBackBinaries` (~2332).

**Interfaces:**
- Consumes: Task 6 `UpdateServerLANInterfaces`; Task 7
  `portal.LANResolutionOf(srv routing.AIServer) portal.LANResolution`.
- Produces:
  - `type agentNetInterface struct`, the mirror of `sample.NetInterface`.
  - `func sanitizeNetInterfaces(in []agentNetInterface, netbirdIP string) []agentNetInterface`:
    - caps at 32 interfaces and 8 IPv4s each, and clamps strings to 64;
    - keeps only valid IPv4 literals;
    - drops 100.64/10, link-local, loopback, and the `netbird_ip`;
    - drops interfaces left with no address;
    - keeps `id_kind` in {`keyed`, `name`}, else drops the entry;
    - keeps at most one `preferred`;
    - sorts by `name`.
  - `func (s *Server) writeBackLANInterfaces(ctx context.Context, serverID string, reported *[]agentNetInterface)`:
    - nil → return;
    - canonical JSON equal to stored → return;
    - otherwise write;
    - then call `s.Portal.NotifyClustersOfServer(ctx, serverID)` only when
      `LANResolutionOf` before and after differ in `Address`,
      `IdentityIPs` or `Fallback`.

- [ ] **Step 1: Write failing tests**
  - `TestSanitizeNetInterfacesDropsNetbirdAndJunk`: 100.64.1.2, the server's
    netbird_ip 192.0.2.77, an IPv6 address, "not-an-ip", and a 70-char name.
  - `TestWriteBackLANInterfacesSkipsUnchanged`: no store write and no notify.
  - `TestWriteBackLANInterfacesNotifiesOnlyOnEffectiveChange`: adding a
    second non-preferred interface writes but does not notify; changing the
    preferred interface's IP notifies once.
  - `TestTelemetryWithoutInterfacesLeavesStoredList`: an old agent sends no
    field.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway -run 'NetInterfaces|LANInterfaces|TelemetryWithoutInterfaces'`. Expected: FAIL.
- [ ] **Step 3: Implement** the mirror type, the sanitizer and the writer.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/gateway`. Expected: PASS. A known flake, `TestVoiceDrainCounter…`, may time out under full load; re-run it in isolation.
- [ ] **Step 5: Commit** `feat(gateway): ingest agent-reported LAN interfaces`

### Task 9: Server API — source, choice and resolution

**Files:**
- Modify: `gateway/backend/internal/portal/service.go`
  - `ServerDTO` (~1715) gains `LANSource string json:"lan_source"`,
    `LANInterfaceID string json:"lan_interface_id"` and
    `LAN LANResolutionDTO json:"lan"`.
  - `UpdateServerRequest` (~1807) gains `LANSource *string json:"lan_source,omitempty"`
    and `LANInterfaceID *string json:"lan_interface_id,omitempty"`.
  - `UpdateServer` (~4361) applies and validates them.
  - `serverDTOWith` (~5034) fills `LAN` from `LANResolutionOf`.
- Modify: `gateway/backend/internal/portal/service_server_rpc.go`
  - `ErrServerLANSourceInvalid = errors.New("server.lan_source_invalid")`
  - `ErrServerLANInterfaceRequired = errors.New("server.lan_interface_required")`
- Modify: `gateway/backend/internal/gateway/portal_server_endpoints.go`
  (`portalServerErrRows` ~444: both new codes → 400)
- Modify tests: `service_server_rpc_test.go`, `portal_server_endpoints_test.go`
- Docs: `docs/architecture/reference/api-surface.md` (~334 server row)

**Interfaces:**
- Consumes: Task 7 `LANResolution`.
- Produces: JSON `lan` on every server response, with fields
  `source, address, interface_id, interface_name, fallback, identity_ips, reported, keyed_ids, interfaces[] {id,id_kind,name,ipv4,virtual,preferred}`.
- Validation:
  - `lan_source` must be one of `auto`, `interface` or `manual`.
  - `interface` needs a non-empty `lan_interface_id`, either in the request
    or already stored. It does **not** need to be reported right now.
- `UpdateServer` keeps its existing `NotifyClustersOfServer` defer.

- [ ] **Step 1: Write failing tests**
  - `TestUpdateServerLANSourceRoundTrip`.
  - `TestUpdateServerRejectsBadLANSource`.
  - `TestUpdateServerInterfaceSourceNeedsID`.
  - `TestServerDTOCarriesLANResolution`.
  - `TestHandlePortalServerUpdateLANSourceInvalidReturns400`.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal ./internal/gateway -run 'LANSource|InterfaceSource|LANResolution'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/portal ./internal/gateway`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): server API for the LAN address source`

### Task 10: Address-change notices on the cluster status and the server

**Files:**
- Modify: `gateway/backend/internal/portal/service_cluster_plan.go`
  - `ClusterStatusDTO` (~269) gains
    `AddressChanges []ClusterAddressChangeDTO json:"address_changes"`.
  - `headClusterStatus` (~381) redacts `From`/`To` for hidden positions,
    exactly as it does for breaches.
  - The plan types carry the derived addresses: `ClusterPlanMember.Address string`
    and `ClusterPlan.HeadIdentity []string`, filled by `ClusterPlan` (~72) from
    the derivation.
- Modify: `gateway/backend/internal/gateway/cluster_orchestrator.go`
  - `rememberPlanLocked` (~740) diffs the previous `lastPlan` and the new one.
    Per member position whose `Address` changed it appends
    `{Position, Server, From, To, At, Relaunched: true}`. A changed
    `HeadIdentity` appends `{Head: true, …, Relaunched: false}`.
  - It keeps the newest 5 per head in `clusterHeadState`.
  - `ClusterStatus` (~481) copies them out.
  - New method `AddressChangesForServer(serverID string) []portal.ServerLANAddressChangeDTO`.
    It scans the in-memory head states for entries whose `ServerID` matches,
    newest first. No new storage.
- Modify: `gateway/backend/internal/portal/service_cluster_plan.go`
  (`ClusterStatusReader`, ~352): add `AddressChangesForServer`.
- Modify: `gateway/backend/internal/portal/service.go`
  - `ServerDTO` gains `LANAddressChanges []ServerLANAddressChangeDTO json:"lan_address_changes"`.
  - `serverDTOWith` (~5034) fills it from the reader. A nil reader gives an
    empty slice.
  - `HeadName` is set only when the principal may see the head spec (use the
    existing runtime-spec visibility check). Otherwise it is `""`, and the
    portal says "a cluster".
- Modify tests: `cluster_orchestrator_test.go`, `service_cluster_plan_test.go`,
  `service_server_rpc_test.go`

**Interfaces:**
- Produces:
  ```go
  type ClusterAddressChangeDTO struct {
      Position   *int      `json:"position,omitempty"`
      Head       bool      `json:"head"`
      Server     string    `json:"server"`    // server name, for display
      ServerID   string    `json:"server_id"`
      From       string    `json:"from"`
      To         string    `json:"to"`
      At         time.Time `json:"at"`
      Relaunched bool      `json:"relaunched"`
  }
  type ServerLANAddressChangeDTO struct {
      HeadSpecID string    `json:"head_spec_id"`
      HeadName   string    `json:"head_name"` // "" when the principal may not see the head
      AsHead     bool      `json:"as_head"`   // this server is the cluster's head
      From       string    `json:"from"`
      To         string    `json:"to"`
      At         time.Time `json:"at"`
      Relaunched bool      `json:"relaunched"`
  }
  ```
- The same change shows on both surfaces, from one in-memory record. After a
  gateway restart both start empty. That is acceptable: the notice describes
  recent changes, not an audit trail.

- [ ] **Step 1: Write failing tests**
  - `TestOrchestratorRecordsMemberAddressChange`.
  - `TestOrchestratorRecordsHeadIdentityChangeWithoutRelaunch`.
  - `TestOrchestratorKeepsFiveNewestAddressChanges`.
  - `TestOrchestratorRecordsNothingForUnchangedPlan`.
  - `TestClusterStatusRedactsHiddenAddressChanges`.
  - `TestAddressChangesForServerCollectsAcrossClusters`: one server is a
    member of two heads, and both changes are returned, newest first.
  - `TestServerDTOHidesHeadNameWithoutSpecAccess`.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway ./internal/portal -run 'AddressChange'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/gateway ./internal/portal`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(gateway): record LAN address changes on the cluster status and the server`

## Phase 5 — Portal UI

### Task 11: Source selector, warnings and change notices

**Files:**
- Modify: `gateway/frontend/src/api/servers.ts`: `PortalServer` (~141) gains
  `lan_source`, `lan_interface_id`, `lan: LanResolution` and
  `lan_address_changes: ServerLanAddressChange[]`;
  `UpdateServerRequest` (~234) gains `lan_source?` and `lan_interface_id?`.
- Modify: `gateway/frontend/src/components/ServerList.tsx`
  - Replace `Field id="server-lan-address"` (~1333) with a select
    `id="server-lan-source"` that has these options:
    - `auto`, labelled with the resolved address and interface, or "not
      resolvable";
    - one per `lan.interfaces` entry: `name — ipv4.join(', ')`, plus a
      "virtual" tag;
    - `manual`, which reveals the existing IPv4 field.
  - The stored choice that is currently missing stays selectable, marked
    "missing".
  - Seed in `openEdit` (~595). PATCH only the changed fields in `submitEdit`
    (~732).
  - Show an `Alert` warning when `lan.fallback` is set, naming the missing
    interface and the fallback.
  - Show the server's `lan_address_changes` under the selector. Each entry
    says from → to, when, the cluster (`head_name`, or "a cluster"), and
    whether a relaunch followed. The list is newest first; show at most 5.
  - Show an info hint when `lan.reported && !lan.keyed_ids` (agent_key
    missing) or `!lan.reported` (agent too old: only `auto`/`manual` are
    offered).
- Modify: `gateway/frontend/src/components/ClusterHeadEditor.tsx` (~915): use
  `headServer.lan.address` / `lan.source` instead of `lan_address`.
- Modify: `gateway/frontend/src/api/runtime.ts` (`ClusterStatus`, ~90-124),
  `gateway/frontend/src/components/shared/clusterStatus.ts` (~100) and
  `gateway/frontend/src/components/ClusterProgress.tsx` (~240): render
  `address_changes` next to the breaches, under
  `data-testid="cluster-address-changes"`.
- Modify: `gateway/frontend/src/components/shared/format.ts` (`errorLabelByCode`):
  `server.lan_source_invalid`, `server.lan_interface_required` and
  `runtime.cluster_lan_interface_missing`.
- Modify: `gateway/frontend/src/i18n.ts`: add the keys below in **both** `de`
  and `en`:
  - `serverLanSourceLabel`, `serverLanSourceHelp`
  - `serverLanSourceAuto` ("Automatisch – {address} ({interface})" /
    "Automatic – {address} ({interface})"), `serverLanSourceAutoNone`
  - `serverLanSourceManual`, `serverLanInterfaceVirtual`,
    `serverLanInterfaceMissing`
  - `serverLanFallbackWarning`, `serverLanAgentKeyHint`,
    `serverLanNotReportedHint`
  - `errorServerLanSourceInvalid`, `errorServerLanInterfaceRequired`,
    `errorClusterLanInterfaceMissing`
  - `clusterAddressChangesTitle`, `clusterAddressChangeMember`,
    `clusterAddressChangeHead`
  - `serverLanAddressChangesTitle`, `serverLanAddressChangeEntry`,
    `serverLanAddressChangeUnnamedCluster`
- Modify tests: `ServerList.test.tsx` (the cases at ~3203-3233 move to the
  source selector; the `AgentConfigMaterial` literals at ~303/334 are
  unchanged), `ClusterHeadEditor.test.tsx` (~747), `ClusterProgress` tests
  and `rpcMembers.test.ts`.

**Interfaces:**
- Consumes: Task 9 JSON `lan`; Task 10 `address_changes` and
  `lan_address_changes`.
- Produces:
  ```ts
  type LanResolution = {
    source: string; address: string;
    interface_id: string; interface_name: string;
    fallback: boolean; identity_ips: string[];
    reported: boolean; keyed_ids: boolean;
    interfaces: LanInterface[];
  }
  type LanInterface = {
    id: string; id_kind: string; name: string;
    ipv4: string[]; virtual: boolean; preferred: boolean;
  }
  type ServerLanAddressChange = {
    head_spec_id: string; head_name: string; as_head: boolean;
    from: string; to: string; at: string; relaunched: boolean;
  }
  ```

- [ ] **Step 1: Write failing tests**
  - `'offers auto, every reported interface and manual'`
  - `'PATCHes lan_source and lan_interface_id only when changed'`
  - `'shows the fallback warning naming the missing interface'`
  - `'shows the agent_key hint when ids are name-based'`
  - `'offers only auto and manual when the agent reports no interfaces'`
  - `'flags a head whose effective LAN address is unresolvable'`
  - `'renders address changes with from, to and relaunch'`
  - `'lists the server\'s address changes from every cluster'`
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/ServerList.test.tsx src/components/ClusterHeadEditor.test.tsx src/components/ClusterProgress.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check`. Expected: all pass.
- [ ] **Step 5: Commit** `feat(portal): choose the LAN interface per server and show address changes`

## Phase 6 — Decision record, version, verification

### Task 12: ADR, privacy rule, version bump, full verification

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md`: a new ADR using
  the next free number (ADR-106 today; re-check after merging `origin/main`).
  It records:
  - `agent_key`, its storage and the HKDF purposes;
  - keyed interface ids;
  - the sealed token cache, closing #61;
  - the LAN address source with its resolution order.
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md`
  (~20, ~129) and `server-agent/internal/sample/system_report.go:11-12` (the
  comment). A hardware address may leave the host only as a keyed hash in
  the interface list, and `SystemReport` still carries none.
- Modify: `server-agent/internal/agent/agent.go:444`: bump `Version` by
  PATCH, because no `Features` flag is added. Base it on `main`'s current
  value after `git merge origin/main`. Add the history entry. Update the
  three pinned tests in `features_test.go` (~618, ~642, ~699).

- [ ] **Step 1:** `git fetch origin && git merge origin/main`. Resolve any
  conflicts, then set the version from `main`'s current value.
- [ ] **Step 2: Run** the full suite:
  - `cd server-agent && go test ./...`
  - `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./...`
  - `golangci-lint fmt --diff && golangci-lint run`, in both modules
  - `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check`
  - `scripts/check-docs.sh`
  - Linux-tagged agent tests in the local `golang:1.26` container
  - Expected: all green.
- [ ] **Step 3: Run** `make sonar-gate` (in the background), then branch
  findings. Judge by new-code findings only.
- [ ] **Step 4: Commit** `docs: record the agent key and LAN interface decisions; bump the server agent`
