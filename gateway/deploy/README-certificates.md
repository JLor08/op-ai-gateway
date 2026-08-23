# Certificate Management (TLS Certificates)

The gateway can automatically obtain and renew certificates for the names it manages (the internal AI server domains `*.int.<your-domain>`, and optionally the public portal domain as well) — either via **ACME** (the open RFC 8555 protocol, in practice usually spoken against Let's Encrypt, but usable against any RFC 8555 CA) or via a **self-operated internal CA** (no public reachability required). This document is the operations manual for both modes. From here on, "ACME" is consistently the name of the ISSUER MODE; "Let's Encrypt" refers exclusively to the specific CA/directory (production or staging) — the two terms are no longer used synonymously in this document.

Public domains (`cert_manage_public_domain`) and the gateway's own edge certificate (Section 6) each have their OWN issuer/ACME profile, independent of the internal (mesh) profile described here — see Section 10.8 for the complete description of this own-vs-shared-account mechanism.

> **Phase 1 note:** this is purely the issuance/renewal logic in the gateway. See Section 7 for what this phase does NOT yet do.

> **Prerequisite for BOTH modes (sqlite/postgres):** on a disk store (`OP_AI_GATEWAY_DB_DRIVER=sqlite|postgres`), `OP_AI_GATEWAY_CERT_ENCRYPTION_KEY` MUST be set (a 64-character hex string, `openssl rand -hex 32`). Every private key the certificate feature generates — leaf keys, the ACME account key, AND the internal CA's key — is sealed with this key before being written to the store; if the key is missing, sealing fails (the gateway NEVER writes a private key in plaintext).
>
> **This is a SEPARATE key, distinct from `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY`** (which seals payload captures and chat transcripts). There is **NO fallback** to the capture key: if the certificate key is not set, nothing is issued, even if a capture key exists. Do NOT use the same value for both — otherwise the separation is only nominal (the gateway writes a `WARN` line at startup if both variables have the same value).
>
> **Without the key, nothing is issued, and the portal shows the reason** as `cert_last_error` — the certificate view shows an error notice that names exactly this variable, and the gateway additionally logs it as `WARN`. What gets written to the store in this case differs by mode: in **`self_signed`** mode, `ReconcileCertificates` aborts at the CA gate and writes **only** this notice (no CA entry, no certificate entry); in **`acme`** mode the pass continues through to the first issuance attempt and therefore **additionally leaves a row with status `error`** per domain (with error text + backoff) in the certificate list. In BOTH modes: **no private key and no certificate material** is stored. So set the key BEFORE turning on `cert_enabled`.
>
> **Either a genuine 64-character hex value or not set at all — nothing in between.** The cipher is built unconditionally at startup, so a placeholder or any other non-hexadecimal value is a fatal misconfiguration: the gateway **will not start**, even if the certificate module is never enabled. Leaving it empty is the safe default (the gateway starts; the reason only appears once the module is enabled).
>
> **Rotation:** if the key is later swapped for a new one, the existing material becomes unreadable — but the gateway does not block because of this (every certificate secret is regenerable): in `acme` mode a new account key is registered, in `self_signed` mode a new internal CA is created; both are logged as `WARN`, naming the variable. **In `self_signed` mode, a new root means all clients must import the new bundle** — so rotate deliberately there. Already-issued leaf certificates remain untouched and `active`: they are only reissued at their normal renewal date (in `self_signed` mode, the root's fingerprint change additionally triggers reissuance). Their stored key is undecryptable until then — that only becomes apparent at distribution time (Phase 2), not in Phase 1.

## 1. Prerequisites (ACME mode)

- A **one-time, static wildcard A record** `*.int.<your-domain>` pointing to the public address of your reverse proxy/load balancer. Let's Encrypt validates via HTTP-01 over port 80 — so DNS must point stably at the address where this nginx (or an upstream proxy) listens.
- The **upstream reverse proxy forwards `/.well-known/acme-challenge/`** for exactly these hosts to the gateway (the bundled `nginx/default.conf` and `nginx/default.no-netbird.conf` already do this — both include the shared `location /.well-known/acme-challenge/` from `nginx/locations.conf`, see the file there). If an additional external proxy runs IN FRONT of this nginx (e.g., a cloud load balancer), it too must pass this path through anonymously (no auth) on port 80.
- **NetBird `dns_domain` set.** The internal names managed by this gateway are derived from the NetBird account domain (`*.int.<dns_domain>`); without a configured NetBird domain, the gateway has no names for which it could request certificates.
- For the public portal domain (optional, `cert_manage_public_domain`), a normal A/AAAA record pointing to this address is sufficient — no NetBird required, only port 80 reachable for the challenge.

None of these prerequisites apply for the **"own internal CA"** mode (`self_signed`) — see Section 5.

## 2. Turning it on

1. Enable **System Settings → "Certificate management active"** (`cert_enabled`).
2. In the new **"Certificates"** menu item: set **email** (ACME contact, `acme_email`), **base domain** (`cert_base_domain`, usually identical to the NetBird `dns_domain`), and **scope** (`cert_server_scope`: `selected` = only servers that are explicitly opted in, or `all` = every managed server).
3. **Test with the staging directory first** (`https://acme-staging-v02.api.letsencrypt.org/directory` instead of `acme_directory_url`'s production default) — the staging environment has its own, much more generous rate limits and issues certificates that no browser trusts (but which play through the entire issuance/renewal cycle for real). Only switch to the production directory once a staging run completes cleanly.
4. **Switching staging → production (or vice versa) creates a NEW ACME account** — this is intentional (an account is always bound to exactly ONE directory; the gateway detects the directory change itself and re-registers automatically, no manual step required).

## 3. Rate limits

Let's Encrypt's production limits (as of today, subject to change):

- **50 certificates per week per registered domain** (for us: per base domain `cert_base_domain`, across all `*.int.<domain>` names combined).
- **5 identical duplicates per week** (the same exact name set requested again).

The gateway additionally protects itself from submitting too many new orders in a single pass:

- **`certOrdersPerPass` = 5** new orders per reconcile pass (a code constant, not a setting) — if more names are due, the most urgent are ordered first, the rest follow in the next pass.
- **Backoff on errors: 5 min → 1 h → 6 h → 24 h** (increases with the number of consecutive failed attempts for the same name); if a certificate's remaining validity drops below 7 days, the backoff is capped at a maximum of 15 min, so that a persistent error doesn't sleep through until expiry.
- **Jitter** spreads out cohorts: due renewals get a deterministic, per-domain-stable time offset so that not all names become due on the same day and exhaust the weekly limits at once.

**`acme_weekly_limit` (and its two counterparts `cert_edge_acme_weekly_limit`/`cert_public_acme_weekly_limit`, see Section 10.8) are purely informational** — stored and shown in the panel, but NOT enforced by the gateway itself. No reconcile pass counts submitted orders against this value; the actual brake remains exclusively the ACME server's own (which rejects the order with a `rateLimited` error, which the gateway treats like any other issuance error — backoff, `last_error`, no effect on an already-valid certificate). For the two predefined directory entries in the panel, the field shows a FIXED, non-editable value that exactly reflects the real LE limits mentioned above: **production → 50, staging → "unlimited"** (internally `0`). Only with **"Custom URL"** does the field become editable — for a third, self-operated RFC 8555 CA with its own limit unknown to the gateway; **`0`** means "unlimited" there as well.

## 4. Diagnostics

- **Status/error per domain** can be seen in the **certificate list** (menu "Certificates"): each row shows the current status, the last result, and, on error, the exact error message.
- **`status = skipped`** means: the name is **not under the configured base domain** (`cert_base_domain`) — the gateway deliberately does not order a certificate for this name, without touching an existing valid entry. Check the base-domain setting, or whether the server/domain is actually supposed to be managed.
- **"Renew now"** (per domain) **ONLY resets the backoff target** — it does not trigger an immediate order outside the normal reconcile cadence and does not overwrite a still-valid certificate; the next reconcile round (default every 15 min, see `OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS`) then picks the name back up as due.
- A look at the gateway logs (System → Logs) at `debug` level shows every reconcile pass, including the names planned/skipped.

## 5. "Own internal CA" mode (`self_signed`)

Choose `cert_issuer_mode = self_signed` to operate entirely without public reachability or an external ACME instance:

- **None of the DNS/network prerequisites from Section 1 are needed** — no wildcard DNS record, no open port 80, no proxy forwarding rule for `/.well-known/acme-challenge/` (the route stays active, but is never hit because no ACME order ever runs in this mode — see the comments directly in the nginx configs), and no email address. **`OP_AI_GATEWAY_CERT_ENCRYPTION_KEY` (see the note box above) still applies here** — it now seals the internal CA's key instead of an ACME account key, and without it CA creation fails: the entire reconcile pass aborts, nothing is issued, and the reason appears as an error notice in the certificate view (`cert_last_error`, naming the variable) as well as an `slog.Warn` log "internal CA unavailable; skipping certificate pass".
- On the **first reconcile pass**, the gateway automatically creates a **root** (validity 10 years) and from then on individually signs every managed name with it. The **leaf lifetime** is configurable via `cert_self_signed_validity_days`.
- **Clients must import the root ONCE** so they trust the certificates issued by the gateway — download via the CA panel in the "Certificates" menu (`GET /api/system/certificates/ca`, returns the PEM bundle). The gateway and the Server-Agent themselves receive the root **automatically** starting with the follow-on phases P2/P3 of this feature series (not yet in Phase 1).
- The CA is **rotated `cert_ca_renew_before_days`** (default **365** days) before it expires: first a **bundle of the NEW AND the OLD root** is distributed (the old root remains valid until no client has only it in its trust store anymore), only THEN are the individual leaf certificates reissued with the new root. **Therefore, NEVER set the renewal window (`cert_ca_renew_before_days`) shorter than the time the bundle realistically needs to propagate to all clients** — otherwise the gateway would sign leafs with a root a client doesn't know yet, breaking that client's TLS connections.
- An immediate rotation can be forced via the endpoint `POST /api/system/certificates/ca/rotate` (system scope); a "reissue now" equivalent for ALL certificates is available via `POST /api/system/certificates/reissue-all`.

## 6. TLS to the upstream reverse proxy (edge certificate)

A **completely separate** certificate/mode from everything above: the internal (mesh) certificates secure the gateway → AI server path; the **edge certificate** is what the gateway hands to its **own nginx** — the TLS path between the **upstream reverse proxy** (the one reachable from the internet) and this nginx. Its own switch, its own issuer mode, its own name set — its own panel in the "Certificates" menu, directly below the internal settings.

**Configuration (menu "Certificates" → panel "Gateway nginx"):**

- **`cert_edge_enabled`** — the on/off switch.
- **`cert_edge_issuer_mode`** (`self_signed` | `acme`) — switchable **independently** of the internal `cert_issuer_mode`, in **either direction**, at any time, with no effect whatsoever on the internal certificates (this is exactly what `e2e:certificates`'s third scenario proves). In ACME mode, the **edge context shares by default** (`cert_edge_acme_shared=true`, the default) the global ACME account — email/directory URL are then the same `acme_email`/`acme_directory_url` fields as in the internal panel. **`cert_edge_acme_shared=false`** switches to an **OWN** account (own `cert_edge_acme_email`/`cert_edge_acme_directory_url`/`cert_edge_acme_weekly_limit`, in the same panel directly below this switch) — see Section 10.8 for the complete description of this own-vs-shared-account mechanism, which works identically for the edge and the public-domain context.
- **`cert_edge_names`** — a comma-separated list, **multiple names AND raw IP addresses** in a single certificate (the whole reason for the multi-name issuance in `internal/certissue`): useful when the upstream proxy addresses the gateway both via a DNS name and via a bare IP — with an IP connection, the client sends no SNI, so nginx can only present ONE certificate, which must carry the IP as a SAN. **ACME cannot validate an IP address** (no HTTP-01 for a bare IP) — an IP SAN is therefore only possible in `self_signed` mode; a save that would combine `cert_edge_issuer_mode=acme` with an IP in `cert_edge_names` is rejected server-side with `cert.invalid`.

**The three environment variables** (real process environment variables — not to be confused with the three `cert_edge_*` **settings** above, which live in the portal/store): one on the gateway side, two on the nginx side (read by `nginx-cert-entrypoint.sh`).

- **`OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR`** (gateway, default empty = no local delivery) — the shared volume: if this variable is set to a directory that BOTH the backend AND the nginx (`web`) container mount, the gateway delivers the certificate there **itself** — atomically (temp file + rename) and **only on an actual change** — three files: `edge-fullchain.pem` (0644), `edge-key.pem` (0600), and, only in `self_signed` mode, `edge-ca.pem` (0644, the internal root that the upstream proxy needs in order to trust this nginx). The bundled `docker-compose.yml`/`docker-compose.no-netbird.yml` already ships a shared named volume `certs` for this purpose (`/shared/certs` on the backend side, `/etc/nginx/certs` on the `web` side, default path `/shared/certs` — see `.env.example`); no manual copy step needed. If the variable stays empty (default), the gateway does not deliver it itself — exactly the case when nginx runs somewhere the gateway cannot write to (Kubernetes: backend and `web` are separate pods with no shared filesystem — see `k8s/README.md`).
- **`OP_NGINX_CERT_DIR`** (nginx container, default `/etc/nginx/certs`) — where the entrypoint wrapper expects the delivered chain/key. This path MUST match the mount point of the shared volume on the `web` side; the bundled `docker-compose*.yml`/`k8s` manifests already mount the shared volume exactly on this default path, so they do NOT set the variable explicitly at all — only set it for a deviating custom layout.
- **`OP_NGINX_CERT_POLL_SECONDS`** (nginx container, default `60`) — how often the entrypoint wrapper checks the fingerprint of the delivered chain to decide whether an `nginx -s reload` is due (see below). **Smaller** = a renewed/rotated certificate takes effect faster, but costs more frequent `openssl x509`/`pkey` calls; **larger** = less overhead, but slower failover. **`0` disables the background poller entirely** — nginx then NEVER reloads on its own, and a new delivery requires a manual `nginx -s reload`/container restart to take effect; a non-numeric or negative value is ignored at startup and reset to the default `60` (with a warning in the log).

**The bootstrap certificate (only relevant when the gateway itself delivers):** on the very first start — as long as nothing has yet been delivered by the gateway — the nginx entrypoint wrapper (`nginx-cert-entrypoint.sh`) writes a **disposable, self-signed** placeholder pair (`CN=OP AI Gateway BOOTSTRAP - not trusted`) just so nginx can start at all; the background poller described above (`OP_NGINX_CERT_POLL_SECONDS`) then watches the fingerprint of the delivered chain and only triggers `nginx -s reload` once a pair arrives that ACTUALLY matches (chain and key are delivered by the gateway as two separate atomic writes — a poll can land in between; the wrapper then NEVER reloads into a mismatched pair, but retries on the next poll instead).

**The key download (`GET /api/system/certificates/edge/key`) is ONLY the fallback path for when local delivery is impossible.** It is gated on exactly the condition that `OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR` + the last write attempt establish (`EdgeDeliveryCapable()`): as long as the gateway can deliver the key to its own nginx itself (or has at least not yet tried and failed), the endpoint refuses with **409 `certificate.edge_key_managed`** — the download button in the portal doesn't even appear then. Only once local delivery is impossible (variable empty, or the last write attempt failed) does the endpoint deliver the key — every successful retrieval logs an `slog.Warn` line with the calling user (never with the key itself).

**The generated nginx configuration** (button "Show nginx configuration" in the edge panel, `GET /api/system/certificates/edge/proxy-config`) provides a ready-made nginx config tailored to the currently stored settings for EXACTLY this upstream reverse proxy — including the ACME HTTP-01 forwarding for every name this gateway actually orders (never more, never a wildcard), and, in `self_signed` mode, a note on where to place the downloaded root on THIS proxy. It is a **snapshot** (the dialog says so too), generated purely from stored settings (plus a 60s-cached NetBird name resolution for the gateway's own name) — no filesystem access, no issuance status, and it contains no key material.

**Limits (known, accepted):**

- **Kubernetes:** no shared filesystem between `web` and `backend` — the operator manually downloads chain+key from the portal and creates them as the `Secret op-gateway-edge-tls` (renewal = the same step repeated). See `k8s/README.md` §3 — **including the bootstrap placeholder, which is needed there for the VERY FIRST installation** (the secret mount there is `readOnly`, so — unlike with Docker Compose — there is no bootstrap fallback that nginx could write itself).
- **`edge-key.pem` is `0600`** — readable only by the process that opens the file before privileges are dropped (for `nginx:1.27-alpine`, the **master process, which starts as root**); this is deliberately as tight as possible, not tighter than necessary.
- **A strict upstream proxy rejects the bootstrap certificate** — intentionally so (`CN=... BOOTSTRAP - not trusted`, self-signed, no real trust chain): on a freshly set up or completely wiped installation, the gateway's own nginx is therefore unreachable through such a proxy chain for a short, self-ending window (until the first real delivery) — not a permanent state.
- **A restart that falls exactly within the window between writing the chain and writing the key** (the two files are written one after another, not in a transaction) can produce a broken TLS listener until the next poll (≤60s) — it **recovers on its own**, it **does not get stuck** (the wrapper never reloads into a recognizably mismatched pair, see above).
- **Forward-looking note, not yet acute:** the bundled `nginx/default.conf` has `http2 on;` set on the `:443` block. A WebSocket agent (`transport=websocket`) connecting over a REAL, verified TLS connection to exactly this port would negotiate HTTP/2 with standard ALPN — and `Upgrade`/`Connection` would then no longer reach the backend (the Server-Agent client only forces HTTP/1.1 in its `tls_insecure` branch, see `server-agent/internal/client/ws.go`; the default transport has `ForceAttemptHTTP2` on). Today no agent points at `:443`, so nothing currently breaks — whoever later moves agent traffic onto this TLS path must either drop `http2 on;` there or explicitly force the agent onto HTTP/1.1.

## 7. What Phase 1 does NOT do

Phase 1 is exclusively the issuance/renewal logic in the gateway (ACME client + internal CA + reconcile loop + HTTP-01 route + management UI). Explicitly NOT included:

- **No distribution** of the issued certificates (or the CA root) to the AI servers themselves — the certificates live in the gateway/store, not on the servers.
- **No TLS on the agent endpoint** — Server-Agent telemetry continues to run as before (plain HTTP within the NetBird mesh, or via the existing transport configuration).
- **No https switch** of the public or internal endpoints themselves — nginx continues to terminate as before; this gateway feature only supplies the certificates/trust anchor for that.

These three points are planned for the follow-on phases **P2–P4** of this feature series.

**Status after Phase 4:** all three points above have since been implemented. Distribution to the AI servers themselves arrived with Phase 2 (opt-in via `cert_mode` in the Server-Agent, see Section 9). The other two points have been implemented since P3/P4: **TLS on the agent endpoint** — the gateway's encrypted mesh/WSS endpoint — with Phase 3 (Section 10), and the **automatic https switch** of application endpoints via the agent-side TLS proxy with Phase 4 (Section 11, `cert_mode=proxy` + `cert_https_switch_mode`). Both are opt-in and byte-neutral by default.

## 8. Plaintext gate (`cert_edge_require_https`)

A **separate, independently switchable** addition to the edge certificate from Section 6: once the gate is turned on, this gateway refuses **unencrypted portal/API traffic** — GET/HEAD get a **307** redirect to the https form of the same URL, every other method gets a hard **403 `certificate.https_required`**. Configuration + operation in the "Certificates" menu → panel "Plaintext gate", directly below the edge-certificate panel: the switch, two hop timestamps (last observed encrypted/unencrypted), and a "Check TLS now" button (the synthetic TLS self-test against the gateway's own nginx, see below).

### 8.1 What the gate covers — and what it does NOT (four always-open exceptions)

The gate **never** applies to four path/caller families, whether armed or not:

1. **`/.well-known/acme-challenge/`** — otherwise renewal of exactly the certificate that first makes enforcement possible would block itself (Let's Encrypt validates unencrypted over port 80 via HTTP-01).
2. **`/healthz`** — otherwise the Kubernetes **readiness AND liveness probes** would break (both query `/healthz:8080` unencrypted from the node — i.e., NOT via loopback, see `k8s/gateway.yaml`), as would the frontend's **connection lock**, which polls the same path every 4 seconds. (The compose files have NO `/healthz` health check — the only health check there is `pg_isready` on the `db` service; the path must still stay open because the frontend polls it in Compose too.)
3. **`/api/agent/v1/`** on the public mux — every agent route lives there as well, and in the no-NetBird topology this is the **only** way a Server-Agent can reach the gateway at all.
4. **Loopback/the internal trusted path** — background chat runs call the gateway's own `/v1/chat/completions` via `http://127.0.0.1:<port>`, with no hop header at all (they don't go through nginx). Without this exception, an armed gate would reject **every** chat run with 403.

**Honest framing: this is a misconfiguration brake, not an attacker boundary.** Anyone with direct network access to the backend port (i.e., bypassing nginx) can get around the gate by forging the hop header `X-OP-Edge-Scheme: https` — it is nginx that sets this header from `$scheme` and overwrites a client-sent value; anyone who bypasses nginx can send any value they like. Such a caller could even satisfy the arming prerequisite (Section 8.3) with forged evidence. This backend port is exactly **the plaintext surface that this feature cuts off at the upstream proxy** — not the only surface there is at all. Never market the gate as "zero plaintext."

### 8.2 Two real lockout scenarios

- **(a) The proxy speaks https to SOME clients, but not (anymore) to the operator's own route.** As long as any client gets through encrypted, the observation stays fresh (Section 8.3) and the gate stays active — but the operator's own PUT attempt (e.g., to turn the gate back off) is itself rejected with 403, because THIS EXACT connection arrives unencrypted. The portal is then no longer reachable via this path, and recovery runs through one of the two paths in Section 8.4.
  **The obvious variant — "I arm the gate while my own route is plaintext" — is no longer possible:** the arming prerequisite additionally requires that the arming request ITSELF arrives encrypted (Section 8.3, condition 2), otherwise **400 `certificate.edge_arm_requires_https`**. Whoever arms it therefore cannot lock themselves out at the moment of arming — the request that turns the gate on is, by definition, one that the now-armed gate lets through. What remains is the **later** break: if the operator's own route is switched to plaintext AFTER arming (proxy rebuild, a second ingress path, a different operator on a different route), the gate kicks in again — which is why this scenario is still documented here.
- **(b) An expired edge certificate** — the failure mode this feature can itself produce: a non-validating client (an agent, a `curl -k`) keeps the observation fresh despite an effectively broken certificate, while the operator's browser hard-rejects the chain and its http fallback is redirected into exactly this broken https connection (307 → https:// with an expired certificate). The operator then sees neither the portal nor a helpful error message — only a browser certificate error.

Neither scenario is expected with a working edge certificate and a correctly configured proxy — but that is exactly why they must be documented here: "it can't happen" is not a recovery strategy.

### 8.3 The arming prerequisite — and why a 400 is not a bug

The switch can only be turned on if **BOTH** conditions are met — each with its own error code, so the message doesn't contradict what the panel shows:

1. **An encrypted request was observed within the last 5 minutes** (the same window that also **automatically disarms** the armed gate again once the encrypted path breaks — see below). Otherwise: **400 `certificate.edge_https_not_observed`**.
2. **The arming request itself arrived encrypted.** Otherwise: **400 `certificate.edge_arm_requires_https`**. Condition 1 alone is satisfiable by ANY OTHER traffic — without condition 2, an operator whose own route is plaintext could arm the gate against another client's TLS observation and lock themselves out with their very next request (exactly scenario 8.2a). With condition 2, that is ruled out at the moment of arming.

The portal additionally locks the switch client-side for condition 1 (with a hint text), but in both cases the server-side gate is the real boundary. A "Check TLS now" click does not report whether an observation exists (it's a pure handshake, not a real hop through the upstream proxy) — only the two timestamps in the panel do that.

**Turning it OFF is free of both conditions** — disarming must always be possible, so the `curl` path from Section 8.4 (loopback, no hop header) can still turn the gate off. It CANNOT turn it on, conversely (condition 2 blocks that there) — that is intentional.

**The same 5-minute window automatically disarms the gate as soon as the encrypted path breaks:** if the upstream proxy stops terminating encrypted traffic (outage, misconfiguration, an expired certificate), the gate stops refusing anything within 5 minutes — it degrades on its own into a no-op instead of creating a permanent lockout on a broken TLS path. That is this design's deliberate trade-off: a self-healing failure is preferable to a permanently locked gateway.

**Important — do NOT close port 80 afterward.** The gate itself leaves `/.well-known/acme-challenge/` unchanged and unencrypted-open (Section 8.1, exception 1) — it only enforces https for portal/API traffic, not for the ACME challenge. That is exactly the obvious NEXT step an operator who just armed the gate might take: "everything now runs over https, so port 80 can be fully closed at the network perimeter (firewall/security group)." That would break automatic certificate renewal — Let's Encrypt **always** validates via HTTP-01 over port 80 (Section 1), independent of the plaintext gate. So port 80 must remain reachable even after `cert_edge_require_https` is armed.

### 8.4 Two recovery paths — with the exact commands

**Path 1 — the emergency switch (ALWAYS works, independent of the portal and of the settings store):** `OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE` is a real process environment variable read at startup that overrides EVERY refusal, no matter what `cert_edge_require_https` says in storage — which is exactly why it works even when the gate is currently blocking the portal itself. Set it to `1`/`true`/`yes`/`on` (any other value, or empty/unset, means the emergency switch is NOT active):

```bash
# docker compose (a running environment variable cannot be injected into a
# container after the fact -- add it to .env and re-create the container).
# As with path 2 below: WITHOUT -f the command hits docker-compose.yml; the
# No-NetBird variant needs its -f explicitly.
echo 'OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE=true' >> .env
docker compose up -d backend                                    # docker-compose.yml
docker compose -f docker-compose.no-netbird.yml up -d backend    # No-NetBird variant

# Kubernetes (patches the op-gateway deployment spec directly, which itself
# already triggers a rollout/restart of the pods -- no separate restart needed)
kubectl -n op-ai-gateway set env deployment/op-gateway \
  OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE=true
```

The gateway process must **restart** for this (the variable is only read at startup). Afterward the portal is reachable over plaintext again; turn the switch off in the portal, THEN remove the emergency switch again and restart once more — otherwise it stays around as a silent crutch.

**Path 2 — a disposable `curlimages/curl` container that joins a running container via a shared network namespace.** The gate itself exempts loopback calls (Section 8.1, exception 4), so a `curl` call running in the SAME network namespace as the backend can turn the gate off without resorting to the emergency switch and without restarting. `docker run --network container:<container>` (Docker), or an ephemeral debug container in the same pod (Kubernetes), works against **ANY running container** — not only one that explicitly shares a foreign namespace via `network_mode: service:X`. This holds true, therefore, in **all three topologies**: in `docker-compose.yml` (with the NetBird sidecar) the call targets the `netbird` container (whose namespace `backend` shares); in `docker-compose.no-netbird.yml` — where `backend` runs in its OWN namespace, with no sidecar at all — it instead targets the `backend` container directly; in Kubernetes, all containers of a pod share the same namespace anyway.

(The mechanism was verified against a real Docker daemon: a third container joining via `--network container:<ID>` reaches EVERY service bound by the target container itself — regardless of whether that container in turn shares a namespace with someone else.)

Four prerequisites whose absence makes the recovery look like a failure:

1. **Neither the backend nor the NetBird sidecar image ships a `curl` suitable for this call.** The backend image is **distroless** (no `curl`, no shell). The NetBird sidecar image is Alpine-based and only ships BusyBox `wget` — and `wget` **cannot send a `PUT` request** (only GET/POST), while WRITING settings — including this on/off switch — is possible **exclusively via PUT** at the `/api/system/settings` endpoint (no POST equivalent; the GET branch of the same endpoint only reads, see `server.go:4531` for the dispatcher's PUT branch). A `docker compose exec netbird sh` followed by `wget` therefore does NOT work for this call. The path that actually works is a THIRD, disposable container that joins the same network namespace via `--network container:<container>` (Docker) or as an ephemeral debug container in the same pod (Kubernetes), bringing along a real `curl` (`curlimages/curl`, a minimal, official image for exactly this purpose):
   ```bash
   # docker compose WITH NetBird sidecar (docker-compose.yml): the netbird
   # container owns the network namespace that `backend` shares via
   # network_mode: service:netbird -- the target is therefore `netbird`.
   docker run --rm --network container:$(docker compose ps -q netbird) \
     curlimages/curl:latest \
     -i -X PUT http://127.0.0.1:8080/api/system/settings \
     -H 'Content-Type: application/json' \
     -H 'X-OP-CSRF: 1' \
     -H 'Cookie: op_ai_gateway_session=<raw cookie value from the browser>' \
     -d '{"cert_edge_require_https": false}'

   # docker compose WITHOUT NetBird sidecar (docker-compose.no-netbird.yml):
   # `backend` runs in its OWN namespace -- the target is therefore directly
   # the backend container itself (same mechanism, different service name).
   docker run --rm \
     --network container:$(docker compose -f docker-compose.no-netbird.yml ps -q backend) \
     curlimages/curl:latest \
     -i -X PUT http://127.0.0.1:8080/api/system/settings \
     -H 'Content-Type: application/json' \
     -H 'X-OP-CSRF: 1' \
     -H 'Cookie: op_ai_gateway_session=<raw cookie value from the browser>' \
     -d '{"cert_edge_require_https": false}'

   # Kubernetes (all containers of ONE pod already share the same network
   # namespace anyway -- --target=backend additionally attaches the debug
   # container to its process namespace, not needed for reachability, but
   # handy for diagnostics)
   kubectl -n op-ai-gateway debug -it \
     "$(kubectl -n op-ai-gateway get pod -l app.kubernetes.io/name=op-gateway \
        -o jsonpath='{.items[0].metadata.name}')" \
     --image=curlimages/curl:latest --target=backend -- \
     curl -i -X PUT http://127.0.0.1:8080/api/system/settings \
     -H 'Content-Type: application/json' \
     -H 'X-OP-CSRF: 1' \
     -H 'Cookie: op_ai_gateway_session=<raw cookie value from the browser>' \
     -d '{"cert_edge_require_https": false}'
   ```
   (Both Docker variants were verified against a real Docker daemon before being included in this runbook: a third container that joins a running container's network namespace via `--network container:<ID>` reaches through it EVERY service listening in that namespace — both when that container [`netbird`] shares the namespace with ANOTHER container [`backend`], and when it [`backend` in the No-NetBird case] is a completely standalone container sharing with no one. Both forms were reproduced with two real `python3 -m http.server` containers and confirmed via `curl`.)
2. **The compose service name for this `--network container:…` target differs per compose variant** — `netbird` in `docker-compose.yml`, `backend` in `docker-compose.no-netbird.yml` (see the two commands above). There is no third case that would have to do entirely without this path — path 2 works in EVERY one of the three bundled topologies; only the target argument of `--network container:…` changes.
3. **The literal header `X-OP-CSRF: 1`** — cookie authentication requires it for every non-safe method (`authenticateWeb`), otherwise 403 `auth.csrf_required`.
4. **With `OP_AI_GATEWAY_SESSION_COOKIE_SECURE=true` set, a browser NEVER sends the session cookie over `http://127.0.0.1`** (the cookie carries the `Secure` attribute) — and `curl -b cookie.txt` with a cookie jar exported from the browser follows the same rule. The cookie must therefore be passed as a **raw `Cookie:` header** (copy the plain value from the browser's dev tools), NOT via `-b`/`--cookie-jar`. Without this step, the call returns **401**, which looks as though the path doesn't work. **The default, when the variable is NOT set, is `!isLoopbackAddr(cfg.Addr)`** (`main.go`, `resolveCookieSecure`) — for the usual bind `0.0.0.0:8080`, that is **`true`**: `.env.example:25` explicitly ships `false` (the Compose default, convenient for exactly this recovery path), while `k8s/configmap.yaml:37` explicitly ships `true` — so on a Kubernetes installation, the call almost always needs the raw cookie header.

(Port `8080` is the backend's `OP_AI_GATEWAY_ADDR` — unchanged in both compose variants as well as in the Kubernetes `op-gateway-config`, see the respective `docker-compose*.yml`/`configmap.yaml`.)

### 8.5 Logging — at most one denial line every 60 s

Every denial is logged, but **throttled**: the FIRST denial for a given path is logged immediately, after that at most one more line per path per **60 seconds** — a retrying client would otherwise fill the 2000-entry log ring and push out exactly the lines a locked-out operator would need to read. **Silence in the Logs menu therefore does NOT mean the lockout is over** — it only means that something has already been logged for the same path within the last minute.

### 8.6 The self-test target value differs by topology

`OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET` (the synthetic TLS self-test behind the "Check TLS now" button, Section 8's panel) is **not a universal value**:

- **docker compose (both variants):** `web:443` (the compose service name + the TLS port; already pre-filled this way in `.env.example`).
- **Kubernetes:** `op-gateway-web:443` (the `op-gateway-web` service that exposes exactly this port — see `k8s/web.yaml`; set in `k8s/configmap.yaml`).

**Empty (the default without explicit configuration) means: the self-test endpoint responds with 409 `certificate.edge_probe_not_configured`** — the backend cannot reach its own nginx on its own in EITHER topology without this variable being set explicitly (in Compose, the backend shares the NetBird sidecar's network namespace, and `web` is its own service; in Kubernetes, `backend` (in the `op-gateway` pod) and `op-gateway-web` run as separate pods).

### 8.7 Prerequisite: nginx must NOT reach the backend port via loopback

**This feature assumes that the upstream nginx and the gateway backend communicate over distinct network peers — NOT via `127.0.0.1`.** If nginx (for whatever reason, e.g. a custom, non-containerized deployment with both processes on the same host) reaches the backend over loopback, this renders the gate **both ineffective AND permanently unarmable**: every request — whether genuinely encrypted at the proxy or not — looks to the backend like an internal/loopback caller (Section 8.1, exception 4) and is therefore NEVER refused; for the same reason, `countsAsObservation` NEVER registers an observation, no matter what hop header nginx sets, and the switch stays permanently locked. The error message on an arming attempt (400 `certificate.edge_https_not_observed`) reads exactly as though the proxy speaks no TLS at all — even though the real cause is a network topology question, not a TLS question.

**Neither of the two bundled topologies does this:** docker-compose runs nginx (`web`) and `backend` as separate containers over the Compose bridge; Kubernetes runs them as separate pods over the cluster network. Both are real, non-loopback network hops. Anyone building a custom, deviating topology must explicitly preserve this property.

## 9. Distribution to the AI servers (Phase 2)

Up to this point (Sections 1–8), the gateway obtains and manages certificates exclusively in its own store — not a single byte of it lands on an AI server. Phase 2 closes exactly this gap, and does so in an **agent-driven** way: the **Server-Agent** (`server-agent/`, see `server-agent/README.md`) fetches its OWN server's certificate itself via an agent-token-authenticated endpoint, writes it to disk atomically, and reports back to the gateway what it actually installed. The gateway itself initiates nothing more than a **doorbell** — an empty "take a look" signal over the existing agent WebSocket connection; every decision about WHAT gets installed and HOW a reload is triggered stays with the agent and its own local configuration.

**Four building blocks that work together:**

1. **Read endpoint** `GET /api/agent/v1/certificate` (ungated on the NetBird-bound agent mux; on the public mux behind the same `netbird_only` gate as telemetry/stream/system-report — see Section 9.10) returns the chain, key, and trust bundle for EXACTLY the server whose agent token the request carries.
2. **Doorbell** — a `cert_update` frame over the existing agent WebSocket connection, triggered as soon as a `kind=server` row is successfully reissued. A pure wake-up call, not a command, no material (Section 9.4).
3. **Installation step in the Server-Agent** — fetch, pairing check, atomic write of the five files, optional local reload command (Sections 9.2–9.5).
4. **Feedback** — the agent reports, in its regular telemetry sample, which fingerprint/mode/CA roots it has actually installed right now. This feeds two things: the "Installed" column in the portal (Section 9.6) and the CA rotation brake (Section 9.7).

The agent also asks **automatically** on a fixed cadence, independent of the doorbell — the doorbell is an accelerator, not a replacement: **WebSocket transport ⇒ every 6 hours, POST transport ⇒ every 15 minutes** (an explicitly set `cert_poll_interval` value always wins, with a floor of 1 minute against an accidental self-DoS against a key-delivering endpoint). In addition, both every (re)connect of the WebSocket connection and the very first startup trigger an immediate sync — so a certificate issued during a disconnection doesn't wait up to 6 hours.

### 9.1 The three `cert_mode` values

The agent recognizes the `cert_mode` configuration (flag `-cert-mode`, env `OP_AGENT_CERT_MODE`, file field `cert_mode` — see `server-agent/README.md` for the complete configuration table):

- **`off`** (default) — the agent NEVER calls the certificate endpoint, NEVER writes to `cert_dir`, NEVER runs a reload command. Its sample reports `cert_mode:"off"`, which the CA rotation brake (Section 9.7) deliberately reads as "nothing to propagate here," instead of waiting 24 hours for a report that never arrives.
- **`files`** — the agent installs the certificate as the five files from Section 9.2 in `cert_dir` and, after a REAL change, runs the locally configured reload command.
- **`proxy`** — installs the certificate **exactly like `files`** (the same five files, the same reload hook) AND additionally puts the **agent-side, TLS-terminating reverse proxy** in front of the actual application. The agent fetches its route topology from the gateway, binds a TLS listener per route with the installed mesh leaf, and forwards the decrypted traffic to the local plaintext upstream. This is the **Phase 4** mode; the complete description — route topology, local routes, port allocation, automatic HTTPS switch — is in **Section 11**. `cert_dir` is required, just as with `files`.

### 9.2 The five files

When `cert_mode != off`, the agent writes to `cert_dir`:

| File            | Permissions | Content                                                  |
|-----------------|----------|----------------------------------------------------------|
| `fullchain.pem` | `0644`   | the full chain, as delivered by the gateway (leaf first) |
| `cert.pem`      | `0644`   | ONLY the leaf                                              |
| `chain.pem`     | `0644`   | everything except the leaf (the rest of the chain)                 |
| `ca.pem`        | `0644`   | the public trust bundle (internal root, if one exists) |
| `privkey.pem`   | **`0600`** | the leaf's private key                         |

**Two special cases, both deliberately conservative:**

- **If the bundle is empty** (no internal CA stored — e.g., pure `acme` mode with no `self_signed` history ever), `ca.pem` is neither written nor is an existing file deleted. An empty bundle from a single response must not wipe out an existing trust file that is still in use.
- **If the chain after the leaf is empty** (a one-certificate "chain" — practically never the case for this feature, since both ACME and internal chains carry leaf + issuer, but the code handles it rather than assuming it away), NO empty `chain.pem` is written; an existing, now-incorrect file is removed on a best-effort basis.

All five write operations are **atomic** (temp file directly IN the target directory, never via `$TMPDIR`, so that no `EXDEV` occurs across a volume boundary — exactly the bug the edge-certificate feature from Section 6 had already run into once) and **all run first**, with `privkey.pem` **last**: so a reload never sees a chain without a matching key. Writing only happens **on an actual change** — a leaf fingerprint change OR a pure bundle change (the latter is exactly the case of a CA rotation, which is why the composite ETag from Section 9's read endpoint exists, so it doesn't disappear behind a 304).

### 9.3 The hard boundary: the gateway never delivers a command

**The reload command lives exclusively in the local agent configuration** (`cert_reload_command`, flag `-cert-reload-command`, env `OP_AGENT_CERT_RELOAD_COMMAND`) — the gateway cannot supply it, cannot override it, cannot even hint at it. This boundary is not just convention: the certificate endpoint's wire contract (`certResponse`, see `fetch.go`) carries exactly seven fields — domain, fingerprint, chain, key, trust bundle, ETag, two timestamps — and **none of them is a path, a script, or a command**. The doorbell (the `cert_update` WS frame) is even narrower: its entire payload is **exactly one field**, `fingerprint`, hard-pinned to this key set (`TestCertUpdateFramePayloadIsExactlyFingerprint` fails as soon as any second field is added) — it is a notification, not an order.

After a fully completed and actually changed installation, the reload command runs through a shell (`sh -c` on Unix, a raw `CmdLine` on Windows, so that a composed command line with pipes/redirections isn't torn apart by `os/exec`'s own argument quoting), with a total budget of 30 seconds AND a `WaitDelay` of 5 seconds (without the latter, a child process holding the output pipes open could block `Wait` beyond the budget). If it fails, the just-installed files are left **exactly as they are** — a broken reload command is no reason to roll back a working certificate — and the log line names only the exit code, **never the command line itself** (it is local configuration and may itself carry secrets, e.g. a sudo password embedded in a wrapper script).

### 9.4 What a failed fetch does: nothing

Every response other than a genuine `200 OK` with actually changed data leaves the already-installed files **exactly as they are**:

- **`304 Not Modified`** — the normal case, nothing to do.
- **`404`** — "there is currently nothing for me to install." This is **explicitly not a revocation statement**: a 404 after a transient store error must not destroy a working TLS configuration, so a 404 **deletes nothing**.
- **`401`/`403`** — a line at `Warn` level, no other action.
- **any other status or a transport/decode error** — a line at `Debug` level, no other action; the next cadence (doorbell or automatic poll) tries again.

This holds symmetrically in every case: the `Report()` that the agent derives from its own on-disk state (never from a cached state) is the same call across all four paths — 200/304/404/error — and always returns the ACTUALLY installed state.

### 9.5 How an operator can see that it's working

Three independent signals, none of which requires access to the server itself:

- **The portal column "Installed"** (menu "Certificates", between "Remaining validity" and "Last error") shows, for every `kind=server` row, one of three states: **✓** when the last-reported leaf fingerprint matches the issued one, **✗** when a report exists but for a DIFFERENT leaf (a stale state), **—** when it was never reported OR the row is not `kind=server`. A tooltip shows the timestamp and the reported mode of the last report.
- **The audit line in the gateway log** — exactly one `slog.Info` line `"agent certificate served"` (with `server_id`, `domain`, `fingerprint`, `not_after`) per key actually issued; a 304 only logs at `Debug` level. No key material and no token ever appear in this line.
- **The agent itself with `-v`/`OP_AGENT_VERBOSE=1`** — every fetch, every installation, and every reload attempt is logged at debug level (see the agent logging extension in `server-agent/README.md`).

### 9.6 The CA rotation brake

A CA rotation (Section 5) first distributes a bundle of the NEW and the OLD root; only after that are leafs reissued with the new root. Without a brake, the gateway could reissue a leaf before the affected server has even seen the new bundle — the server would then be carrying a certificate that its own clients can't (yet) verify.

**The brake therefore holds back ONLY the issuer-mismatch reason for a reissuance — never a genuine, time-based renewal.** A certificate that is simply about to expire is always renewed, regardless of what an agent reports or doesn't report; the brake applies exclusively to exactly ONE case: the certificate is still valid for a long time, but its issuer fingerprint is the PREVIOUS root (not the current one) — and the agent has not yet confirmed that it knows the new root.

It is additionally capped at **24 hours** (`certCAPropagationTimeout`): after that, reissuance happens regardless, no matter what the agents report — a rotation must never remain permanently stuck on a single unresponsive agent.

**The complete fail-open list** (each of the following conditions individually keeps the brake from engaging, so reissuance proceeds normally):

- no report registry wired up;
- the row is not `kind=server` (gateway/public/edge rows have no associated agent);
- there is **no** `cert_ca_rotated_at` timestamp (no rotation has occurred);
- more than 24 hours have passed since the rotation;
- there is **no** report at all for this server;
- the last report says `cert_mode:"off"` (the agent explicitly installs nothing — so there is nothing to propagate);
- the last report carries **no** fingerprint;
- the row's issuer fingerprint is NOT the still-valid previous root (only a leaf that is REALLY still hanging off the previous root has anything to lose from an early reissuance at all).

**This completeness is the whole point, not an enumeration of edge cases:** in the DEFAULT configuration, an agent runs with `cert_mode=off` and therefore reports nothing at all — a brake that already held back on "no report" would have blocked EVERY rotation on EVERY installation that does not actively use Phase 2, for a full 24 hours.

### 9.7 The agent-token rule

A server **without** an agent token has no distribution path — nobody can fetch the material, and ordering one for it would only needlessly burn issuance rate limit. That's why a `kind=server` name only enters the reconcile's target set when `AgentTokenByServer` actually returns a token for it.

- The row is **not silently** skipped in this case — it stays visible, with status `skipped` and the reason `"no agent token: no distribution path"` in `last_error`.
- An already **valid** row (with material) is therefore **not pruned** — it runs out normally instead of being immediately discarded when a token is missing. Only a row WITHOUT material may disappear.

### 9.8 Operational prerequisites

- **`privkey.pem` is `0600` and owned by the OS user under which the Server-Agent runs.** A TLS consumer (e.g. nginx, llama.cpp, …) running as a DIFFERENT user **cannot read** this file. Two documented workarounds: run the consumer as the same user, or have the locally configured reload hook itself run `chown`/`install` before the consumer opens the file.
- **The agent must be able to create and write to `cert_dir` (or its parent directory)** — the installation creates the directory with `0755` if needed, but only if the process is authorized to do so.

### 9.9 Key exposure

This is the only endpoint in this gateway that ever releases a private key over the network — two points are therefore non-negotiable:

- **`netbird_agent_download_only` does NOT cover this endpoint** — that setting gates exclusively the agent binary/config download (`/api/agent/v1/download/...`). The certificate endpoint instead follows the same gate as telemetry/stream/system-report: `netbird_only`. Anyone who wants to restrict key retrieval to the NetBird mesh must therefore turn on `netbird_only` (not `netbird_agent_download_only`).
- **`tls_insecure` is not acceptable with this endpoint.** The agent would exchange its bearer token AND — on success — the freshly fetched private key over an unverified TLS connection; exactly the kind of connection `tls_insecure` allows is a man-in-the-middle target. `tls_insecure` is a debug switch for a test environment, not an option for an agent running `cert_mode != off`.

**The plaintext gate from Section 8 does NOT cover this endpoint.** `/api/agent/v1/` is one of the four paths the gate permanently leaves open (Section 8.1, point 3) — in the no-NetBird topology, it is the agents' only path. A redirect wouldn't save anything here anyway: the agent already sends its bearer token in the FIRST request, so an eavesdropper would already have the authorization that releases the key before any redirect could take effect. The effective safeguards are the two named above: `netbird_only` and an `https` `gateway_url` without `tls_insecure`.

### 9.10 Recovery

An agent that, for whatever reason, ends up in a broken or stale state recovers via: **clear `cert_dir` and restart the agent.** The "memo" that makes a conditional request (`If-None-Match`) possible in the first place is the ON-DISK STATE itself — not a separate, hidden cache. An empty `cert_dir` means: no valid chain, no matching key, and therefore an immediate unconditional fetch instead of an `If-None-Match`.

The same self-healing also kicks in WITHOUT a restart, for exactly the case of a half-completed installation (Section 9.12, point 1): the pairing check (leaf fingerprint against the public key of the installed `privkey.pem`) invalidates the memo as soon as the chain and key no longer match each other — the next cadence then reinstalls the ENTIRE set, instead of getting stuck behind a wrongly-matching 304.

### 9.11 Known Limits

1. **The rename window** — a crash exactly between two of the five atomic renames (chain written, key not yet, or vice versa) can leave a briefly mismatched pair. This is **self-healing on the next cadence** (see Section 9.10), **not a permanent stuck state** — because `privkey.pem` is renamed **last**, every partial install necessarily leaves a mismatched pair behind, and that is exactly what the check detects.
   **Precisely bounded:** the self-healing considers exclusively the pair `fullchain.pem` + `privkey.pem`. A manually deleted or corrupted `cert.pem`/`chain.pem` is not covered by this (a partial install cannot produce it) — the path from Section 9.10 is responsible for that.
2. **`cert_mode=proxy` installs like `files` and additionally operates the agent-side TLS proxy** (Phase 4, see Section 9.1 and the complete description in Section 11). For pure distribution (this Section 9), it behaves byte-identically to `files`; the proxy and the automatic HTTPS switch are the subject of Section 11.
3. **Feedback lives exclusively in memory** (like `AgentPresenceRegistry`) — after a gateway restart, "Installed" shows **`—`** until the next report arrives. In this window the CA rotation brake is also fail-open (Section 9.6), so a rotation is more likely to be carried out TOO EARLY than too late — the safe direction.
4. **Multiple certificate rows for the same server** (e.g. after a failed rename, see Section 6's `edgeRow` analog) — among the rows with **valid** material, the read endpoint returns the **freshest** one (`not_before`, then `issued_at`); the domain only decides in case of a tie. Pruning the superseded row is best-effort: a failed deletion is only logged, and until the next pass two simultaneously valid rows can exist — in that case the newer one wins, not the alphabetically first.
5. **A reload hook that RUNS and FAILS is not retried.** The side file with the ETag has already been written by the time the hook runs, so the next fetch gets a 304 — the files are correctly in place, but the service was never reloaded. The failure is recorded as `slog.Warn` with the **exit code** in the agent log (never with the command line). Remedy: fix the command and restart the agent, or clear the directory (Section 9.10) — the hook then runs again on the next full install. A hook **aborted by a shutdown**, by contrast, is safeguarded — but differently than it might first sound: the agent does **not wait** for it. The command is simply no longer killed on exit, so it keeps running as an **orphaned process** and terminates on its own. The time limit (`hookTimeout` + `WaitDelay`) therefore only applies while the agent is still alive; after that, nothing terminates it anymore, and any error message it still writes lands on a stderr that no longer exists. This is the deliberate trade-off: an orphaned reload is better than a certificate that got installed but never activated.
6. **There is no revocation/removal path.** A certificate once distributed stays on the server's disk until it expires or the operator deletes it by hand — even if the gateway-side row has since been deleted or replaced.
7. **The cross-module config drift protection is two-part.** The hand-maintained JSONC fixture in the agent module (`config_test.go`) only proves that EXACTLY this text parses into the documented defaults — it does not notice when a field is added to `buildAgentConfigJSON`/`buildServerAgentConfig` and the fixture isn't kept in sync. The mechanical guard, by contrast, is the gateway-side key-set test (`TestBuildAgentConfigJSONKeySet`), which checks a test-maintained expectation list against the JSON keys actually produced, and which must be deliberately touched when a field is added.

## 10. The gateway's mesh endpoint over HTTPS/WSS (Phase 3)

Phase 1 issued certificates, Phase 2 distributed them to the AI servers — neither switched **any** traffic to https. Phase 3 switches **exactly one** path over: the **gateway's mesh endpoint**, i.e. the second HTTP listener on the NetBird IP, through which the Server-Agent reports telemetry (POST **and** WebSocket), fetches its certificate, and loads binaries/config.

**Not in this phase, but in Phase 4** (Section 11): the agent-side TLS proxy in front of the actual application, `cert_mode=proxy`, and the automatic switch of applications to `https`. The public mux, the upstream reverse proxy, and the edge plaintext gate (Section 8) are untouched.

### 10.1 Prerequisites

- `cert_enabled=true` and issuer mode **`self_signed`** (P3 is primarily a self-signed feature — in `acme` mode, the mesh name is usually not even publicly certifiable at all, in which case the endpoint stays in plaintext; see 10.9).
- A resolvable **gateway domain** (`cert_gateway_domain`, otherwise derived from the NetBird gateway peer) — it is the canonical client name.
- An active **agent listener** (`OP_AI_GATEWAY_AGENT_ADDR`, or the mesh IP derived from the gateway peer + `OP_AI_GATEWAY_AGENT_PORT`).

As soon as a valid `kind=gateway` certificate is present, the listener **speaks both on the same port**: it reads the first byte of every connection (`0x16` = TLS handshake ⇒ TLS, otherwise plaintext). This makes the switchover **non-breaking and rolls out per agent** — an old agent on `http://…` keeps working until it is updated. Without a valid certificate, no sniffer is hooked in at all: the path is then byte-identical to before (no-op invariant).

### 10.2 New agent config keys and the public CA cache

The agent trusts the gateway certificate via **system roots ∪ additional anchors** (never `tls_insecure`). The additional anchors are cumulative:

| Key | Meaning |
|---|---|
| `ca_file` | operator-managed PEM path, only read, **never** overwritten |
| `cert_dir/ca.pem` | written by P2 as soon as `cert_mode != off` |
| `ca_cache_file` | **agent-managed** public cache (default `server-agent-ca.pem` next to the config) |
| `ca_pem` | bootstrap bundle **inline** in the generated config |

`ca_file`/`ca_cache_file` are resolved **relative to the loaded config file** (not to the working directory). All four carry **exclusively public** material. Content changes are detected via SHA-256 (not via mtime); the trust pool is rebuilt **in the running process** as needed — a CA rotation takes effect without a restart.

**Bootstrap:** A freshly downloaded `server-agent.json` defaults to `cert_mode: "off"`, so P2 writes no `ca.pem`. So that the very first TLS connection attempt still succeeds, the **generated** config carries the current bundle inline as `ca_pem` **and** a `ca_cache_file`. The agent atomically persists a successfully fetched, validated bundle into the cache and reports it as durable **only after that**. `ca_pem`/`ca_cache_file` are only set if the gateway leaf **actually delivered** is internally signed.

**No silent downgrade:** if the https connection fails certificate verification, the agent does **not** fall back to http; it logs the cause (throttled) and keeps retrying with backoff.

### 10.3 Safe rollout order

1. Roll out the new agent binary + config (`gateway_url=https://<fqdn>:<port>`, inline `ca_pem`). **A raw-IP `gateway_url` is not supported** — the FQDN is the canonical client name (the IP SAN is best-effort only and does not heal on an IP change, see 10.9).
2. In the portal (Certificates), watch the **"Transport"** column: `✓ TLS` per server means that agent last connected over TLS.
3. Wait until **no** server remains on the laggard list (every non-disabled token server shows `✓ TLS`).
4. **Only then** turn on the `cert_mesh_require_tls` gate (see 10.5).

### 10.4 CA rotation and `ca_rotation_pending_servers`

A CA rotation in `self_signed` mode runs deterministically:

> **publish new bundle → `ca_update` doorbell → agents validate and persist → fingerprints confirmed via telemetry → gateway leaf hot-swapped.**

As long as not **all** non-disabled token servers report the **new** root as **durably** loaded, the still-valid gateway leaf remains under the **previous** root — the certificate view shows the named set `ca_rotation_pending_servers`. No report, an old agent, or a cache write failure all count as "not yet ready." A leaf swap merely exchanges the listener's `atomic.Pointer` — **no rebind, no connection drop**.

### 10.5 The `cert_mesh_require_tls` gate (strict)

In the portal (Certificates, mesh header), the gate refuses every plaintext request to `/api/agent/v1/*` on the agent listener (**403 `certificate.mesh_tls_required`**); `/healthz` stays open.

- **Arming** is only possible once a Server-Agent has recently (< 5 min) connected over TLS (otherwise 400 `certificate.mesh_tls_not_observed`); the confirmation dialog names **by name** every server that has never been seen over TLS and would therefore be locked out.
- **Strict and not self-clearing** (deliberately different from the edge gate in Section 8): once armed, it refuses plaintext **unconditionally** until the operator turns it off again. It does **not** open itself if TLS breaks down fleet-wide — that is the price of "never a silent fallback to plaintext."
- **Turning it off** is never gated and takes effect immediately.

### 10.6 Important limits and the emergency switch

- **24-hour cap** on the CA rotation brake: if an agent is offline for longer or not updated, the gateway leaf is swapped after 24 h regardless — such a laggard can then fail against the new leaf.
- **Do not rotate twice:** a second CA rotation before the first one completes does not, as in Phase 2, produce a triple chain; exactly one previous root overlaps.
- **The cache folder must be writable** (`ca_cache_file`); otherwise the server counts as "not ready" and the brake holds (the safe direction). `ca_file` remains the alternative for fully operator-managed trust stores.
- **Lockout trap + emergency switch:** an armed gate plus an expired gateway leaf locks out the entire fleet (the agent rejects the leaf and does not degrade to http). The way back necessarily goes through the gateway: set `OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE=true` and **restart** the container — this variable overrides the stored gate and is read once at startup, independent of the settings store. Afterward, fix leaf renewal and remove the variable again.

### 10.7 Diagnostics

```bash
# Does the mesh listener speak TLS? (self_signed: verify against the bundle)
openssl s_client -connect <mesh-ip>:<agent-port> -servername <gateway-fqdn> \
  -CAfile /path/to/ca_cache_file </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256

# WSS reachability + the agent's view: the agent log shows the URL used,
# the anchor sources, and any verification error (throttled, without the key).
# The current leaf/CA fingerprint is shown in the portal (Certificates, mesh header)
# or in the certificate GET:
curl -s https://<gateway>/api/system/certificates -H 'X-OP-CSRF: 1' -b op-session.txt \
  | jq '.mesh'   # tls_active, fingerprint, require_tls, tls_observed, tls_pending_servers
```

### 10.8 Public domains: own issuer/ACME profile + export (appendix to Phase 1/2)

Since the certificate unification (Cert-Public-Unification), the public domain is no longer a pure name-suffix of the internal profile, but its own block in the "Certificates" menu (panel "Public domains", directly below the edge panel from Section 6) — with the same own-vs-shared-account mechanism as the edge context:

- **`cert_manage_public_domain`** — the on/off switch (unchanged since Phase 1); **`cert_public_domains`** — the comma-separated domain list.
- **`cert_public_issuer_mode`** (`""` | `self_signed` | `acme`) — an **own** issuer mode, switchable independently of the internal `cert_issuer_mode`. **Empty (the default) is itself a valid, active value**: "follow the internal/global `cert_issuer_mode`" — the panel offers this state as its own, default-selected option, not as a placeholder. Only an operator who explicitly chooses `acme` or `self_signed` decouples the public domain from the internal mode.
- **`cert_public_acme_shared`** (default **`true`**) — exactly like `cert_edge_acme_shared` in Section 6: `true` means "share the global ACME account" (`acme_email`/`acme_directory_url`), `false` switches to an **own** account — `cert_public_acme_email` / `cert_public_acme_directory_url` / `cert_public_acme_weekly_limit`, all in the same panel directly below the "Use own ACME settings" switch (the same UI component as in the edge panel). The weekly-limit value is — as described in Section 3 — purely informational: fixed for the predefined directories (production 50, staging unlimited), editable under "Custom URL" (`0` = unlimited), never enforced by the gateway in any case.

**Shared accounts are keyed by directory, not by context.** Whether the internal, the edge, or the public context uses its own account or the global one is decided by the respective `*_acme_shared` switch — but WHICH account a context actually ends up using is resolved internally by `accountFor` (`internal/portal/service_certificates.go`) via the configured directory URL: two contexts with a BYTE-IDENTICAL directory URL consequently register with the same ACME server as the SAME account and share its stored account key — even if, say, the edge context has set `cert_edge_acme_shared=false`, but its own directory URL happens to match the global one. Only a directory URL that is GENUINELY different creates a second, independent account (under its own settings key derived from the SHA-256 of the directory URL) — `e2e:certificates`'s seventh scenario demonstrates this: a public domain with its own ACME configuration is issued independently, without touching the edge or internal certificates.

**Byte-neutral upgrade — no migration, no behavior break.** Each of the settings named above has an absence-safe default that lets an existing deployment continue running exactly as before this unification: `cert_public_issuer_mode=""` continues to follow the internal mode, `cert_edge_acme_shared`/`cert_public_acme_shared` default to `true` without an explicit value (= "shared global account" — the already-registered account of an upgraded gateway continues to be used UNCHANGED, byte-identically, without re-registration, because the global directory URL keeps the same settings key unchanged since Phase 1), and the `kind` column in the certificate table (`public`/`edge`/`gateway`/`server`) is unchanged, the same column as in Phase 1 — no database migration was needed for this extension.

So that an upstream reverse proxy can turn off its own certbot, there are also two **system-scoped** GET routes (only when `cert_manage_public_domain` is on and the domain is listed in `cert_public_domains`):

```bash
# Public chain (no key)
curl -s https://<gateway>/api/system/certificates/public/<domain>/bundle \
  -H 'X-OP-CSRF: 1' -b op-session.txt -o public-fullchain.pem

# Private key — AUDITED: every release writes exactly one
# slog.Warn line (caller/domain ID, never the key); the response carries
# Cache-Control: no-store.
curl -s https://<gateway>/api/system/certificates/public/<domain>/key \
  -H 'X-OP-CSRF: 1' -b op-session.txt -o public-key.pem
```

The gate checks the **store row** (`kind=public`), not just the settings list: if `cert_gateway_domain` (or the edge primary domain) collides with a public domain, the only stored row is `kind=gateway`/`edge`, and the export returns **404** — the mesh or edge key never leaves the process. `409 certificate.public_not_managed` if management is off or the domain is not configured.

### 10.9 Known Limits (Phase 3)

1. **The global gate can silently mute a laggard immediately** — mitigated by the named list in the confirmation dialog, not eliminated.
2. **Transport observation lives only in memory** — after a gateway restart, the column shows `—`, and the gate can only be armed after a new TLS observation (the safe direction: it refuses to arm rather than arming blindly).
3. **Raw-IP `gateway_url` is not supported** — the FQDN is canonical; the IP SAN is best-effort for the IP known at issuance time and does **not** heal on a later IP change (only at the regular renewal).
4. **In `acme` mode**, the mesh name is usually not certifiable; the endpoint then stays in plaintext.
5. **A one-time rebind** during the initial transition from plaintext → TLS (milliseconds, absorbed by the agents' reconnect backoff); if the re-bind fails transiently, the manager immediately restores the plaintext listener (bundled retry), so the `netbird_only` isolation never breaks.
6. **An expired leaf continues to be served** — validity checking is the client's responsibility; the remaining validity is shown color-coded in the portal.

### 10.10 Separate encrypted agent port (`cert_mesh_tls_mode`)

By default, the mesh listener speaks both on **one** port (plaintext **and** TLS, via first-byte detection — see 10.1). Anyone who instead wants to run a **dedicated, TLS-only port** alongside the plaintext port (e.g. to publish TLS at the host while the plaintext port stays mesh-internal, or to handle both ports separately in the firewall/NetBird) switches the topology to **`separate`**.

**The mode — ENV default, portal override (three-valued, like `cert_public_issuer_mode`):**

| Value | Meaning |
|---|---|
| `OP_AI_GATEWAY_AGENT_TLS_SEPARATE` (bool, default `false`) | the **boot/default mode** (`false`=combined, `true`=separate) |
| Setting `cert_mesh_tls_mode` (`""` \| `combined` \| `separate`, default `""`) | the **runtime switch** in the portal |

`cert_mesh_tls_mode=""` (the default) is itself a valid, active value: "follow the ENV default." An explicit `combined`/`separate` wins. The effective state is shown **read-only** in the "Certificates" panel (mesh header): "Separate TLS port active" / "… not active."

- **`combined`** — today's behavior, one port, first-byte sniffer. Byte-identical to before this feature (no-op invariant).
- **`separate`** — the existing agent port (`OP_AI_GATEWAY_AGENT_ADDR`, or the mesh IP derived from the peer + `OP_AI_GATEWAY_AGENT_PORT`) serves **only plaintext now** (no sniffer), and a **dedicated TLS-only listener** comes up on the TLS port. Both run simultaneously, both go through the same agent mux + the mesh gate from 10.5. **"Pure TLS" in practice** = `separate` **plus** an armed `cert_mesh_require_tls`: the plaintext port then refuses unconditionally (403), the TLS port stays serviceable — no third mode needed.

If, in `separate` mode, there is (still) **no** valid `kind=gateway` leaf present, the TLS listener stays down and the plaintext listener keeps serving; a later reconcile tick brings the TLS listener up once material is available.

**The TLS port is deploy-time (env-only), shown read-only only.** The container cannot change what docker-compose publishes at its boundary, which is why there is **no editable portal field** — only the display "TLS port: N":

| Variable | Meaning |
|---|---|
| `OP_AI_GATEWAY_AGENT_TLS_PORT` (default `8443`) | port of the dedicated TLS listener, when bound via the gateway peer |
| `OP_AI_GATEWAY_AGENT_TLS_ADDR` (default `""`) | explicit `host:port` bind; wins over the peer derivation and **also supplies the read-only displayed port** (port component) |

**The generated agent config follows automatically:** in `separate` mode, `gateway_url` becomes `https://<gateway-fqdn>:<tls-port>` (the FQDN is canonical — raw IP is not supported, see 10.9). Unchanged in `combined` mode.

**The confirmation dialog before switching.** A mode change in the portal **immediately rebinds the mesh listener** (adds/removes the TLS port, or switches the main port between sniffer and raw TLS). This is why the select is gated behind a ConfirmDialog that states exactly that: **all currently connected Server-Agents will briefly disconnect and reconnect.** The switchover takes effect within one reconcile interval, with no process restart.

#### 10.10.1 Two publication models

1. **Mesh-bound (default):** the TLS listener binds the gateway mesh IP. There is **nothing** to publish at the host — it is reachable over the NetBird network and is opened by the new policy **`op-gw-agent-ingest-tls`** (see README-netbird.md §6; it exists **only** in `separate` mode and is deleted again when switching back to `combined`).
2. **Host-published (opt-in):** anyone who wants to expose the TLS port at the host sets `OP_AI_GATEWAY_AGENT_TLS_ADDR=0.0.0.0:<port>` **and** adds a `ports:` entry to the compose file (analogous to the plaintext agent port). Example:

   ```yaml
   services:
     backend:
       environment:
         OP_AI_GATEWAY_AGENT_TLS_ADDR: "0.0.0.0:8443"
       ports:
         - "8443:8443"   # dedicated encrypted agent port
   ```

   **Secured by the `netbird_only` source check.** A host-published agent listener (plaintext **or** TLS) would otherwise serve anyone. If `netbird_only` is on, the gateway compares the connection's **local address** (`http.LocalAddrContextKey`) with its own mesh peer IP: only connections that arrived via the `wt0`/mesh interface match — everything else is rejected with **403 `netbird.only`**. On a mesh-bound listener, the check is a **no-op** (local address = mesh IP). It fails **open** in every ambiguous case (no portal, `netbird_only` off, mesh IP not resolvable), so that a control-plane outage never cuts off the fleet.

#### 10.10.2 Known Limits (separate port)

- **A host-published TLS port that the host doesn't actually publish** is not verifiable from inside the container — the panel only shows the listener status (bound/not bound), not the host firewall/port forwarding.
- **The port number is deploy-time** — the portal only switches the *mode* and *displays* the effective port; changing it requires the ENV + a container/pod restart.
- **The one-time rebind** on a mode change is the same millisecond effect as in 10.9 point 5 — absorbed by the agents' reconnect backoff; a transient bind failure leaves the other listener untouched.

## 11. Agent-side TLS proxy and automatic HTTPS switch (Phase 4)

Phase 1 issued certificates, Phase 2 distributed them to the AI servers, Phase 3 switched the **gateway's mesh endpoint** to TLS. Up to this point, traffic from the gateway **to the actual application** (Ollama, vLLM, llama.cpp, …) still ran in plaintext: the issued `kind=server` leaf sat on the server's disk, but terminated nothing. Phase 4 closes this last gap **agent-driven** and without commands from the gateway:

- in mode **`cert_mode=proxy`**, the Server-Agent puts its own **TLS-terminating reverse proxy** in front of the application — it terminates TLS with exactly the mesh leaf that Phase 2 installs anyway, and forwards the decrypted traffic to the application's local plaintext port;
- for this, the gateway supplies the agent with a **gateway-directed route topology** (DATA only, never a command) via `GET /api/agent/v1/proxy-routes`;
- as soon as the agent reports that a proxy listener is actually terminating TLS, the gateway automatically switches the associated application to **`https`** and from then on routes via the proxy port — **verified against the internal CA**.

As already in Phase 2/3, the **"the gateway never delivers a command"** rule applies: `proxy-routes` is a pure data list (listen port, upstream, opaque app ID), and the agent decides locally what to do with it. No private keys in logs or DTOs; nothing in this phase ever disables TLS verification.

### 11.1 The `cert_mode=proxy` mode (Server-Agent)

`cert_mode=proxy` (flag `-cert-mode proxy`, env `OP_AGENT_CERT_MODE=proxy`, file field `cert_mode`) does **two things**:

1. **Installs like `files`** — the same five files in `cert_dir`, the same optional reload hook after a real change (Sections 9.2–9.5). `cert_dir` is therefore required, exactly as in `files` mode.
2. **TLS proxy** — for each desired route, the agent binds a TLS listener with the installed mesh leaf and puts a streaming `httputil.ReverseProxy` in front of it, pointing at the plaintext upstream (`FlushInterval:-1`, so that streamed responses aren't buffered; an unreachable upstream results in `502`).

**What the proxy binds to — the leaf determines the address.** The bind host is derived **from the identity of the installed leaf**: its first IP SAN, otherwise its first DNS SAN. This is exactly the mesh address the certificate was issued for — so the proxy listens exactly where mesh peers can reach it, and **never** on all interfaces. If the leaf carries no usable SAN (or none is installed yet), the route stays **pending** rather than falling back to all NICs. This makes the AI server's domain load-bearing: a `kind=server` row with an IP domain (e.g. behind NetBird) receives a leaf with that IP as an IP SAN, and the proxy binds that very IP.

The proxy runs on **the same poll cadence** as certificate distribution (Section 9): after a genuine reinstall (`ReloadCert`), the leafs in the running listeners are **hot-swapped**, without rebinding a socket; the route topology is then refreshed from the gateway. A gateway outage (transport error or `304 Not Modified`) **never** tears down running proxies — the most recently applied routes remain in place.

### 11.2 The gateway-directed route topology (`GET /api/agent/v1/proxy-routes`)

The agent fetches its desired routes via `GET /api/agent/v1/proxy-routes` — with the same transport contract as the certificate endpoint: bearer auth via the agent token, dual-registered on the NetBird-bound agent mux as well as (behind the `netbird_only` gate) on the public mux, and **conditional GETs via ETag** (unchanged topology ⇒ `304`, `Cache-Control: no-store` on every path). The response is a **pure data list**:

```json
{ "routes": [ { "listen": 8600, "upstream": "http://127.0.0.1:8080", "app_id": "app_…" } ], "etag": "…" }
```

- **`listen`** — the port on which the agent-side TLS proxy should bind (the application's `proxy_listen_port`, see 11.4).
- **`upstream`** — the local plaintext upstream to which decrypted traffic is forwarded; the gateway fixes it to `http://127.0.0.1:<app.Port>` (the agent runs on the same host as the application).
- **`app_id`** — only for observability/forward compatibility; the agent reconciles **purely on `listen`+`upstream`**, so that a pure app-ID change never churns a listener.

**Who gets a route.** The gateway only delivers routes at all for servers **within the HTTPS switch scope** (11.5), and among those, only for genuine **switch candidates**: an **active** application that is either `http` or already proxy-switched (`https` with `proxy_listen_port` set). A disabled app, or a standalone own-TLS `https` app (`proxy_listen_port` = 0), gets no listener — so the agent never opens a proxy port the switch wouldn't use anyway. A server **outside** the scope (in particular the byte-neutral default `manual`, see 11.5) gets an **empty** route list back — the agent then runs no local TLS proxy at all.

### 11.3 Local routes: `cert_proxy_routes` + `cert_proxy_routes_mode`

Besides the gateway-directed routes, the operator can give the agent **local routes** in the config file (pure file fields, no env variables):

```jsonc
{
  "cert_mode": "proxy",
  "cert_dir": "/etc/op-agent/certs",
  "cert_proxy_routes": [ { "listen": 8700, "upstream": "http://127.0.0.1:9000" } ],
  "cert_proxy_routes_mode": "fallback"
}
```

`cert_proxy_routes_mode` controls how a local route is resolved against a gateway-supplied route **on the same listen port**:

- **`fallback`** (default) — a local route **only** fills a listen port that the gateway did **not** supply; on a shared port, **the gateway wins**. This keeps gateway-directed operation authoritative, with local routes as a safety net (e.g. for a server not yet in scope, or for a path the gateway doesn't know about at all).
- **`override`** — a local route **wins** over a gateway route on the same listen port. For the rare case where the operator must deliberately override the gateway-directed assignment locally.

Malformed routes (invalid port, no absolute `http(s)` upstream with `host:port`) are already rejected by `config.Validate`. An empty `cert_proxy_routes_mode` is treated as `fallback`.

### 11.4 Port allocation: `cert_proxy_listen_port_base`

The listen port of each candidate (the application's `proxy_listen_port`) is **automatically allocated** by the gateway — the operator normally never sets it by hand. Allocation picks the lowest free port **≥ `cert_proxy_listen_port_base`**, unique per server, and is **idempotent** (a port already allocated is never reassigned). It happens **lazily**, as soon as the agent fetches its routes for the first time, and is persisted immediately so the port stays stable across calls and matches exactly where the switch later routes to.

| Setting | Meaning |
|---|---|
| `cert_proxy_listen_port_base` (default **8600**, valid `1024`–`65535`) | lower bound of automatic port allocation |

`proxy_listen_port` is **read-only** in the application DTO (`GET /api/portal/servers/{id}/applications`) — for operator visibility, not for editing. `0` means "not yet allocated."

### 11.5 Automatic HTTPS switch: `cert_https_switch_mode` + per-server override

Whether and for which servers the gateway **automatically switches applications to `https`** is controlled by a global mode plus a three-valued per-server override — the same pattern as `cert_public_issuer_mode` / `cert_mesh_tls_mode` (10.8, 10.10):

| Setting | Values | Meaning |
|---|---|---|
| `cert_https_switch_mode` | `manual` (default) \| `auto` \| `selected` | global mode |
| per-server `https_switch_override` | `""` \| `include` \| `exclude` | override (a single three-valued field) |

`PUT /api/portal/servers/{id}/https-switch` with `{"https_switch_override":"include"|"exclude"|""}` sets the override.

The **scope** (is a server included?) results from both:

- **`manual`** (default) — the gateway changes **no** app scheme, regardless of any override. Byte-neutral starting state; the server gets no routes at all (11.2), so the agent runs nothing.
- **`auto`** — **every** managed server is included, **except** one with override `exclude` (opt-out).
- **`selected`** — **only** a server with override `include` is included (opt-in).

**Exactly one three-valued field** (never two booleans) is deliberately chosen: this way, switching the global mode leaves no old counter-value that could resurrect itself under the new mode.

**Forward vs. backward — and why a silent agent reverses nothing.** The switch-reconcile runs on the same cadence as the certificate reconcile (Section 4/9.10) and additionally reacts to every save of a certificate setting:

- **Forward** — an `http` candidate app is switched to `https` **as soon as** the agent reports an **explicit `tls_active:true`** for its `proxy_listen_port`. From then on, the gateway routes via the proxy TLS port instead of directly to the plaintext port.
- **Backward** — a proxy-switched `https` app is reverted to `http` **only** if the agent reports an **explicit `tls_active:false`** for its port (the port is desired, but the local listener is not currently binding/serving).
- **A missing report is NOT a reversion.** An agent that goes quiet (terminated, restarted in a different mode, or simply without `proxy_routes` in its sample) counts as "no observation" — neither forward nor backward. This is **deliberate** (no flapping on a brief agent outage/reconnect): an already-set-up proxy is never torn down just because the agent was briefly silent. A genuine reversion requires the explicit `tls_active:false`.

### 11.6 The CA-trusting outbound transport (gateway→app against the internal CA)

So that the gateway, after the switch, can genuinely speak `https` **to the application** without disabling verification, it uses its own transport for outgoing app connections (`newOutboundAppTransport`): it **never** sets `InsecureSkipVerify`, but instead trusts the **system roots plus the operator's internal CA trust bundle**. After the switch, the agent-side proxy terminates with the `kind=server` leaf that this very internal CA issued — so the gateway→app hop is **verified against the internal CA**, using exactly the leaf the proxy holds.

**Two ways to check this path** (the e2e suite uses both ideas):

1. **Direct TLS dial** to the proxy port with the CA bundle fetched via `GET /api/system/certificates/ca` as the sole trust anchor (never `rejectUnauthorized:false`) — proves that the proxy holds a CA-chained leaf.
2. **A fully gateway-routed request** (model probe/chat) — this only succeeds if the CA-trusting outbound transport actually works, because in that case the gateway itself (not the test client) is the one verifying.

### 11.7 End-to-end flow

1. Set `cert_https_switch_mode=auto` (or `selected` + per-server `include`).
2. The server is NetBird-linked, added to certificate management, and has an agent token (Section 9) — the `kind=server` leaf is issued and provided with the IP/DNS SAN at which the proxy binds.
3. On the server, the Server-Agent runs with `cert_mode=proxy`: it installs the leaf, fetches the routes (`proxy_listen_port` gets allocated), binds the TLS proxy, and reports `tls_active:true`.
4. The switch-reconcile switches the application to `https` and routes via the proxy port — verified against the internal CA.
5. If the proxy listener goes down (`tls_active:false`), the next reconcile tick reverts the application to `http`; a merely **silent** agent explicitly does **not** do this.

### 11.8 Known Limits (Phase 4)

- **The upstream is fixed at `127.0.0.1:<app.Port>`** — the agent runs on the same host as the application; a remote upstream is not supported (local routes, 11.3, are meant for that).
- **`proxy_listen_port` is only allocated on the first route fetch** — until the agent has fetched its routes once, the DTO shows `0`.
- **A reversion requires an explicit `tls_active:false`** (11.5) — a terminated/removed agent does not reverse a switch; this is intentional (anti-flapping). Anyone who wants to deliberately revert an app sets it to `http` in the portal or removes the server from the scope.
