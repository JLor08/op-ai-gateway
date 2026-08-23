# Configuration & Environment Variables (Reference)

Exhaustive listing of every environment variable read by the backend (`op-ai-gateway`, prefix `OP_AI_GATEWAY_`) and the ServerAgent (`op-ai-server-agent`, prefix `OP_AGENT_`), with defaults and parsing rules. For the loading model (precedence, config files, dev conveniences), see [Configuration](../cross-cutting/configuration.md).

## Legend: value parsing rules

| Type | Rule |
|---|---|
| `bool` | `1`/`true`/`yes`/`on` (case-insensitive) → true; anything else (including unset) → false, unless a different fallback is noted |
| `int` | empty/unparseable/`<= 0` → default |
| `int (floor F)` | as `int`, plus: a positive value below `F` is clamped up to `F` |
| `int (allow 0)` | empty/unparseable/negative → default; an explicit `0` is honored as-is (has a distinct "off" meaning) |
| `duration` | Go duration string (e.g. `30s`, `15m`, `2h`); empty/unparseable/`<= 0` → default |
| `duration (0 = auto)` | empty or a value that parses to `<= 0` → `0`, a first-class "automatic" value, not the default |
| `float [0,1]` | parsed as float64; out-of-range values are clamped into `[0,1]`, not defaulted |
| `string` | used verbatim; `""` may itself be a meaningful "disabled" value (noted per row) |

All backend variables are read through `internal/config/config.go`'s `Load()` (env > optional `OP_AI_GATEWAY_CONFIG` JSON file > default). All agent variables are read through `server-agent/internal/config/config.go`'s `Load()` (flag > env > optional `OP_AGENT_CONFIG` JSON file > default).

## Backend (`OP_AI_GATEWAY_*`)

### Server, network, and general

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_ADDR` | string | Main HTTP listen address (`host:port`) | `127.0.0.1:8080` |
| `OP_AI_GATEWAY_PUBLIC_URL` | string | External base URL used in generated links (e.g. bootstrap invite email) | `http://localhost:8080` |
| `OP_AI_GATEWAY_CONFIG` | string | Path to the optional JSON config file; unset → `gateway.json` next to the binary | `` (unset) |
| `OP_AI_GATEWAY_LOG_LEVEL` | string | slog level for the runtime logger + portal Logs view initial level (`debug`/`info`/`warn`/`error`, case-insensitive; unknown → `info`) | `info` |
| `OP_AI_GATEWAY_LOG_BUFFER_SIZE` | int | Capacity of the in-memory log ring backing the portal Logs view | `5000` |
| `OP_AI_GATEWAY_DEFAULT_LANGUAGE` | string | Default UI/session language (`de`/`en`) | `de` |
| `OP_AI_GATEWAY_THEMES_DIR` | string | Directory of externally supplied theme definitions (one subdirectory per theme id); missing/empty is not an error | `/themes` |
| `OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT` | duration (0 = auto/off) | Idle-stream watchdog timeout for streaming completions; a value parsing to `<= 0` explicitly disables the watchdog | `120s` |
| `OP_AI_GATEWAY_APP_HEALTH_PROBE_TIMEOUT` | duration | Per-probe HTTP timeout for the app-health reachability loop | `3s` |
| `OP_AI_GATEWAY_SEED_APP_HEALTH_MODE` | string | Test/e2e seam: seeds the mock application's `health_check_mode` directly (e.g. `model_sync`) | `` (default health-path probing) |

### Database

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_DB_DRIVER` | string | Storage backend: `memory` \| `sqlite` \| `postgres`; unrecognized value is a fatal startup error | `memory` |
| `OP_AI_GATEWAY_SQLITE_PATH` | string | SQLite database file path (driver `sqlite`) | `./data/op-ai-gateway.db` |
| `OP_AI_GATEWAY_POSTGRES_DSN` | string | PostgreSQL connection string (driver `postgres`) | `` |
| `OP_AI_GATEWAY_AUTO_MIGRATE` | bool | Apply pending forward-only migrations transactionally at startup | `true` |

### Bootstrap admin & API token (SQLite/PostgreSQL only)

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL` | string | Email of the seeded `system_admin`; must be set together with `BOOTSTRAP_API_TOKEN` | `` |
| `OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME` | string | Display name of the seeded admin | falls back to the email |
| `OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD` | string | Optional password that makes the seeded admin login-able immediately (adopted on a later redeploy only if the admin has no password yet) | `` (invite-link flow instead) |
| `OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN` | string | Plaintext secret for the seeded bootstrap API token (scopes `gateway:use`, `admin`) | `` |

### Sessions & step-up auth

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_SESSION_COOKIE_SECURE` | string (tri-state) | `true`/`false` forces the session cookie's `Secure` flag; unset → auto = `!isLoopbackAddr(ADDR)` | `` (auto) |
| `OP_AI_GATEWAY_SESSION_IDLE_TTL` | duration | Session idle timeout | `12h` |
| `OP_AI_GATEWAY_SESSION_MAX_TTL` | duration | Absolute session lifetime | `168h` (7 days) |
| `OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS` | int | How long a sticky session holds a reserved concurrency slot (capacity cap); a value `<= 0` uses the default (cannot be set to 0 to disable) | `60` |
| `OP_AI_GATEWAY_SYSTEM_ADMIN_MODE_TTL_SECONDS` | int | System-Admin step-up elevation TTL before re-elevation is required | `900` (15 min) |

### Agent listener (mesh / NetBird-facing HTTP bind)

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_AGENT_ADDR` | string | Explicit `host:port` bind for a dedicated NetBird-only agent listener; empty = no dedicated listener (agent routes stay on the main listener) | `` |
| `OP_AI_GATEWAY_AGENT_PORT` | string | Port for the agent listener when its bind host is resolved from the selected gateway NetBird peer | `8081` |
| `OP_AI_GATEWAY_AGENT_TLS_SEPARATE` | bool | Env-fallback intent for a separate encrypted agent listener topology; overridden live by the `cert_mesh_tls_mode` system setting once set | `false` |
| `OP_AI_GATEWAY_AGENT_TLS_PORT` | string | Port for the separate encrypted agent listener when its bind host is resolved from the peer | `8443` |
| `OP_AI_GATEWAY_AGENT_TLS_ADDR` | string | Explicit bind for the separate encrypted agent listener; wins over `AGENT_TLS_PORT` when set | `` |
| `OP_AI_GATEWAY_AGENT_BINARY_DIR` | string | Directory containing the ServerAgent release manifest + platform binaries served by the agent-binary download endpoints | `/agents` |
| `OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS` | int | Freshness window: a ServerAgent counts as "reporting" if it POSTed telemetry within this window (env fallback for the operator-settable `agent_presence_timeout_seconds` system setting) | `15` |

### TLS / certificates

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_CERT_ENCRYPTION_KEY` | string | Hex AES-256 key sealing certificate private keys at rest (leaf keys, ACME account key, internal CA key). Own key, deliberately independent from the capture key. Empty → the certificate module refuses to seal (issuance surfaces as `cert_last_error`); the gateway still starts, since the module is optional | `` |
| `OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR` | string (trimmed) | Directory the gateway writes the edge (nginx-facing) certificate bundle into. Empty means the gateway *cannot* deliver it locally, which is what unlocks the key-download endpoint | `` |
| `OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET` | string (trimmed) | `host:port` of the gateway's own edge (nginx) TLS listener that the synthetic self-probe dials. Empty disables the probe endpoint (returns 409) | `` |
| `OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE` | bool | Kill switch: overrides the stored `cert_edge_require_https` setting so plaintext is never refused at the edge, regardless of the portal. Env-only, deliberately independent of the settings store so a locked-out operator can always recover by restarting with this set | `false` |
| `OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE` | bool | Same kill switch for the mesh (agent) listener's `cert_mesh_require_tls` setting | `false` |
| `OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS` | int (floor 60) | Cadence of the certificate reconcile loop (issuance/renewal due-checks; a no-op while the certificate module is disabled) | `900` (15 min) |

### Telemetry, retention, capacity, energy

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_TELEMETRY_RETENTION_HOURS` | int | Retention window for persisted rich telemetry samples; the prune loop drops older samples | `168` (7 days) |
| `OP_AI_GATEWAY_AVAILABILITY_RETENTION_HOURS` | int | Retention window for server availability history | `720` (30 days) |
| `OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS` | int | Recency window during which a server actively serving a model is protected from eviction by a request for a different model | `30` |
| `OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS` | int | Default cadence for scheduled benchmark mode when an application enables it without its own interval (floored further at an internal minimum) | `86400` (1 day) |
| `OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT` | int | Percent of total VRAM the capacity benchmark keeps free while ramping concurrency | `10` |
| `OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY` | int | Max concurrency the capacity-benchmark ramp probes up to | `64` |
| `OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS` | int | Settle time between capacity-benchmark ramp steps | `5` |
| `OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH` | int | Max over-capacity requests allowed to wait in the admission queue before new ones are rejected with 503 immediately | `128` |
| `OP_AI_GATEWAY_ENERGY_RECONCILE_INTERVAL_SECONDS` | int | Cadence of the energy-attribution background reconciler | `15` |
| `OP_AI_GATEWAY_ENERGY_SETTLE_SECONDS` | int | Delay after a request finishes before attributing its energy, so telemetry has time to land | `10` |
| `OP_AI_GATEWAY_ENERGY_IDLE_WINDOW_SECONDS` | int | Trailing window the per-server idle-wattage tracker uses for its rolling minimum power draw | `3600` (1 h) |

### NetBird mesh

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_NETBIRD_KEY_FILE` | string | Absolute path to a shared-volume file the "enroll sidecar" action writes a minted NetBird setup key to. Empty disables autonomous sidecar enrollment | `` |
| `OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS` | int (floor 30) | Cadence of the NetBird peer-sync loop | `60` |
| `OP_AI_GATEWAY_NETBIRD_TOKEN_ROTATE_BEFORE_DAYS` | int (allow 0) | Auto-rotate the NetBird admin API token this many days before expiry (env fallback for the operator-settable setting); `0` explicitly disables auto-rotation | `14` |

### Tracing (OpenTelemetry, opt-in)

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_TRACING_ENABLED` | bool | Enables OpenTelemetry tracing. Off by default: the dynamic sampler drops every span, ~zero overhead | `false` |
| `OP_AI_GATEWAY_TRACING_SAMPLE_RATIO` | float [0,1] | Fraction of requests sampled when tracing is enabled | `1.0` |
| `OP_AI_GATEWAY_OTLP_ENDPOINT` | string | OTLP collector endpoint; if empty, sampled spans are generated but not exported | `` |

### Encrypted payload capture

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY` | string | Key sealing captured request/response payloads (and the SMTP password + NetBird admin token) at rest on a disk-backed store | `` |
| `OP_AI_GATEWAY_CAPTURE_MAX_BYTES` | int | Max captured payload size on a disk-backed store | `1048576` (1 MiB) |
| `OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES` | int | Max total bytes held by the in-memory capture/chat store (memory driver) | `67108864` (64 MiB) |

### Test / development seams

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_MOCK_DELAY_MS` | int (→ duration) | Artificial delay injected by the mock provider; production-safe at `0` | `0` |
| `OP_AI_GATEWAY_MOCK_UNREACHABLE` | bool | Forces the seeded mock provider unreachable (drops its application out of routing/model offering) | `false` |

### Memory-driver-only dev convenience variables (not part of `config.Config`)

These are read directly via `os.Getenv` in `cmd/gateway/main.go`, only apply when `OP_AI_GATEWAY_DB_DRIVER` is `memory` (or unset), and have no JSON config file equivalent. Their defaults are only used when `OP_AI_GATEWAY_ADDR` binds to a loopback address; on a non-loopback bind, an explicit value is required or startup fails.

| Variable | Purpose | Loopback-only default |
|---|---|---|
| `OP_AI_GATEWAY_DEV_TOKEN` | Bearer secret for the seeded dev API token (`tok_dev`, scopes `gateway:use`+`admin`) | `dev-secret` |
| `OP_AI_GATEWAY_DEV_PASSWORD` | Password for the seeded dev user (`usr_dev` / `dev@example.test`, role `system_admin`) | `dev-secret` |
| `OP_AI_GATEWAY_DEV_AGENT_TOKEN` | Agent bearer secret seeded for the mock AI server | `dev-agent-secret` |

## Agent (`OP_AGENT_*`)

Every row below also has a matching CLI flag (kebab-case, e.g. `-gateway-url`) and JSON config-file key (snake_case, e.g. `gateway_url`), all with the same precedence: flag > env > file > default.

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AGENT_GATEWAY_URL` | string, required | Gateway base URL (must be an absolute `http://`/`https://` URL) | — (startup error if empty) |
| `OP_AGENT_TOKEN` | string, required | Per-server agent bearer token issued by the portal | — (startup error if empty) |
| `OP_AGENT_CONFIG` | string | Path to the optional JSON config file; unset → `server-agent.json` next to the binary | `` |
| `OP_AGENT_INTERVAL` | duration (floor 250ms) | Telemetry collection cadence | `1s` |
| `OP_AGENT_SYSTEM_REPORT_INTERVAL` | duration (floor 1m) | Cadence at which the POST transport re-sends the static hardware inventory (self-heals a gateway restart); the WebSocket transport also re-sends on every reconnect | `30m` |
| `OP_AGENT_TRANSPORT` | string enum | Telemetry transport: `post` (one HTTP POST per sample) or `websocket` (one persistent connection) | `websocket` (resolved when unset — see [Configuration](../cross-cutting/configuration.md#2-sensible-defaults)) |
| `OP_AGENT_METRICS_URL` | string | Optional inference `/metrics` endpoint to scrape | `` (disabled) |
| `OP_AGENT_MODEL_STATUS_URL` | string | Optional endpoint polled each cycle for currently-loaded models | `` (disabled) |
| `OP_AGENT_MODEL_STATUS_FORMAT` | string enum | Response shape for `MODEL_STATUS_URL`: `openai` \| `llama_swap` \| `llama_cpp` \| `litellm` \| `` /`auto` (tolerant union) | `` (auto) |
| `OP_AGENT_LHM_URL` | string | Optional LibreHardwareMonitor Remote Web Server `/data.json` URL for CPU/system power on Windows | `` (disabled) |
| `OP_AGENT_CERT_MODE` | string enum | Certificate behavior: `off` (never fetch) \| `files` (write cert files + run `CERT_RELOAD_COMMAND` on change) \| `proxy` (`files` behavior + run the agent-side TLS proxy) | `off` |
| `OP_AGENT_CERT_DIR` | string | Directory certificate files are written into; required unless `CERT_MODE=off` | `` |
| `OP_AGENT_CERT_RELOAD_COMMAND` | string | Shell command run after a changed certificate is fully and atomically installed. Local-only — the gateway can never deliver a command to run | `` |
| `OP_AGENT_CERT_POLL_INTERVAL` | duration (0 = auto; floor 1m if explicit) | Certificate poll cadence; `0`/unset means automatic (derived from `TRANSPORT`: `websocket`→6h, `post`→15m) | `0` (auto) |
| `OP_AGENT_CA_FILE` | string | Optional operator-managed PEM trust bundle (agent only reads it) | `` |
| `OP_AGENT_CA_CACHE_FILE` | string | Optional public PEM cache the agent manages atomically | `` |
| `OP_AGENT_CA_PEM` | string | Optional inline bootstrap CA bundle (from a generated config) | `` |
| `OP_AGENT_TLS_INSECURE` | bool | Skip TLS certificate verification (development only) | `false` |
| `OP_AGENT_VERBOSE` | bool | Emit detailed debug logs to the console (alias of `-v`/`-verbose`) | `false` |

Two additional settings exist **only** as JSON config-file keys (no env-var or flag form): `cert_proxy_routes` (array of `{listen, upstream}`, the agent-side TLS proxy's local routes) and `cert_proxy_routes_mode` (`fallback` default, or `override`) — see [Configuration §7](../cross-cutting/configuration.md#7-configuring-the-serveragent).

## See also

- [Configuration](../cross-cutting/configuration.md) — the loading model, database drivers, bootstrap seeding, dev conveniences.
- [HTTP API Surface (Reference)](./api-surface.md) — the routes these settings gate.
