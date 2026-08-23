#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

#
# nginx entrypoint wrapper for the gateway's TLS edge. Two jobs:
#
#   1. nginx must ALWAYS be able to start. `ssl_certificate` pointing at a missing
#      file is a LOAD error, not a per-request one, and this nginx is the entrance to
#      everything -- the portal, the whole API, and /api/auth/login. On a first deploy
#      the gateway has not delivered an edge certificate yet, so without this wrapper
#      the :443 block would take the entire gateway offline (the same total-outage
#      shape the NetBird sidecar once had). So when no certificate is present we write
#      an obvious THROWAWAY pair. It is fail-closed on purpose: a verifying peer MUST
#      reject `CN=OP AI Gateway BOOTSTRAP - not trusted`, which is correct -- it is not
#      the real certificate. A marker file records that the pair is a bootstrap.
#
#   2. A renewed certificate must take effect without an operator. nginx reads the
#      certificate only at start/reload, so a watcher polls the delivered chain's
#      fingerprint and reloads ONLY when it actually changed.
#
# Both jobs are best-effort: every failure path degrades and still hands control to
# the stock entrypoint, so this wrapper can never be the reason nginx does not run.
#
# POSIX sh: the nginx:1.27-alpine image has no bash. `openssl` is not in that image
# either -- Dockerfile.frontend installs it (apk add --no-cache openssl).
#
# NGINX_STOCK_ENTRYPOINT overrides the stock entrypoint path FOR TESTING ONLY; it
# defaults to the real image path so production behaviour is unchanged.
set -eu

CERT_DIR="${OP_NGINX_CERT_DIR:-/etc/nginx/certs}"
CHAIN="$CERT_DIR/edge-fullchain.pem"
KEY="$CERT_DIR/edge-key.pem"
MARKER="$CERT_DIR/.op-edge-bootstrap"
POLL="${OP_NGINX_CERT_POLL_SECONDS:-60}"
STOCK="${NGINX_STOCK_ENTRYPOINT:-/docker-entrypoint.sh}"
BOOTSTRAP_CN="OP AI Gateway BOOTSTRAP - not trusted"
MAIN_PID=$$

log() { echo "nginx-cert: $*" >&2; }

# sha256 fingerprint of the DELIVERED chain, or empty when it cannot be read right
# now (missing, being replaced, unreadable, no openssl). The watcher treats empty as
# "no information" and never as a change, so a transient read failure cannot trigger
# a spurious reload -- and cannot kill the watcher either.
fingerprint() {
    openssl x509 -in "$CHAIN" -noout -fingerprint -sha256 2>/dev/null || true
}

# True only when the chain and the key on disk are actually the SAME pair.
#
# This must be checked HERE because nothing downstream catches it: the gateway writes
# the pair as TWO separate atomic files (chain first, then key), so a poll can land
# between them -- and if the key write keeps failing, the chain's content-compare skips
# its rewrite, so the mismatch persists on disk rather than being a brief window.
# Reloading a mismatched pair can be SILENTLY fatal, and whether it is depends on the
# KEY TYPE (measured on nginx 1.27.5 / OpenSSL 3 in this image):
#   * same type (e.g. RSA cert + a different RSA key): OpenSSL compares them within one
#     pkey slot, `nginx -t` FAILS, and a reload is rejected -- loud, old config survives.
#   * DIFFERENT type (RSA cert + EC key): they occupy different pkey slots, nothing
#     compares them, `nginx -t` succeeds and `nginx -s reload` exits 0 (that exit code
#     only reports "signal sent") -- but every TLS handshake then fails with a
#     handshake_failure alert. The HTTPS edge is down while the log says it reloaded.
# The dangerous case is the COMMON one: the bootstrap key here is EC, so the very first
# real delivery is exactly "new RSA chain next to the old EC key". Hence this check.
#
# An unreadable file yields an empty string, and two empty strings must NOT compare
# equal, so each side is required to be non-empty first.
pair_ok() {
    _cpub=$(openssl x509 -in "$CHAIN" -noout -pubkey 2>/dev/null) || _cpub=""
    [ -n "$_cpub" ] || return 1
    _kpub=$(openssl pkey -in "$KEY" -pubout 2>/dev/null) || _kpub=""
    [ -n "$_kpub" ] || return 1
    [ "$_cpub" = "$_kpub" ]
}

# True when we can create files in $1 (creating it if needed). In Kubernetes the
# certificate arrives as a read-only Secret mount, so this is false there by design.
dir_writable() {
    [ -d "$1" ] || mkdir -p "$1" 2>/dev/null || return 1
    _probe="$1/.op-write-probe.$$"
    : > "$_probe" 2>/dev/null || return 1
    rm -f "$_probe" 2>/dev/null || true
    return 0
}

# Write the throwaway pair. temp file + rename so nothing ever observes a
# half-written certificate or key.
write_bootstrap() {
    _tmp_key="$KEY.op-tmp.$$"
    _tmp_chain="$CHAIN.op-tmp.$$"
    # EC keygen is fast and quiet. Fall back to RSA if this openssl cannot do EC:
    # failing to produce ANY certificate here costs the whole gateway, so the
    # fallback is worth three lines.
    if ! openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
            -keyout "$_tmp_key" -out "$_tmp_chain" -days 30 \
            -subj "/CN=$BOOTSTRAP_CN" >/dev/null 2>&1 \
       && ! openssl req -x509 -newkey rsa:2048 -nodes \
            -keyout "$_tmp_key" -out "$_tmp_chain" -days 30 \
            -subj "/CN=$BOOTSTRAP_CN" >/dev/null 2>&1; then
        rm -f "$_tmp_key" "$_tmp_chain" 2>/dev/null || true
        return 1
    fi
    # Same modes the gateway's own delivery uses: 0600 key, 0644 chain.
    chmod 0600 "$_tmp_key" 2>/dev/null || true
    chmod 0644 "$_tmp_chain" 2>/dev/null || true
    mv "$_tmp_key" "$KEY" && mv "$_tmp_chain" "$CHAIN" || {
        rm -f "$_tmp_key" "$_tmp_chain" 2>/dev/null || true
        return 1
    }
    # Inspectable state: "is this the real certificate or the bootstrap?"
    printf '%s\n' \
        "Bootstrap TLS certificate written by nginx-cert-entrypoint.sh." \
        "Subject: CN=$BOOTSTRAP_CN" \
        "NOT trusted by design -- a verifying peer must reject it." \
        "Removed automatically when the gateway delivers the real certificate." \
        > "$MARKER" 2>/dev/null || true
    return 0
}

if [ ! -s "$CHAIN" ] || [ ! -s "$KEY" ]; then
    if ! dir_writable "$CERT_DIR"; then
        log "WARNING: no edge certificate in $CERT_DIR and the directory is not writable,"
        log "WARNING: so no bootstrap pair can be written and nginx will refuse to start."
        log "WARNING: In Kubernetes the certificate is a read-only Secret mount -- create"
        log "WARNING: the op-gateway-edge-tls Secret (the exact command is in deploy/k8s/web.yaml)."
    elif write_bootstrap; then
        log "no edge certificate yet -- wrote a THROWAWAY bootstrap pair so nginx can start."
        log "Subject: CN=$BOOTSTRAP_CN. It is NOT trusted, by design: a verifying peer must"
        log "reject it. The gateway replaces it within one reconcile pass and this wrapper"
        log "then reloads nginx automatically."
    else
        log "WARNING: could not write a bootstrap certificate (openssl missing or failing);"
        log "WARNING: nginx will refuse to start because $CHAIN does not exist."
    fi
fi

# Read the baseline fingerprint BEFORE starting the watcher, so the watcher can never
# race the bootstrap write above (and so a change made immediately after startup is
# still detected).
BASELINE_FP="$(fingerprint)"
# If what nginx is about to load is ITSELF a mismatched pair -- a restart landing inside
# the gateway's two-write window, or after a key write that failed -- then the chain
# will never change again once the key catches up, and a fingerprint baseline would
# latch the broken listener exactly like the loop would. Treat it as unknown instead, so
# the first pair that verifies triggers a reload.
pair_ok || BASELINE_FP=""

case "$POLL" in
    ''|*[!0-9]*)
        log "OP_NGINX_CERT_POLL_SECONDS='$POLL' is not a non-negative integer -- using 60."
        POLL=60
        ;;
esac

if [ "$POLL" -gt 0 ]; then
    # Reload-on-change watcher. `set +e` because it must outlive every transient
    # failure: a half-written file, a vanished mount, a failing reload. It must also
    # never reload on its own -- only a fingerprint that really differs does that.
    (
        set +e
        last="$BASELINE_FP"
        while sleep "$POLL"; do
            # exec below keeps our PID, so this is nginx. If nginx is gone, so are we
            # (in a container PID 1 exiting kills us anyway; this covers the rest).
            kill -0 "$MAIN_PID" 2>/dev/null || exit 0
            now="$(fingerprint)"
            [ -n "$now" ] || continue           # unreadable now: no information, no reload
            [ "$now" != "$last" ] || continue   # unchanged: do NOT reload
            # The matching key may not have landed yet -- or may have failed to land.
            # Leave "$last" untouched so this is simply RETRIED on the next poll. The
            # alternative latches: reload into a dead TLS listener, record the new
            # fingerprint, and then never look again because the chain never changes
            # once the key catches up.
            pair_ok || continue
            log "edge certificate changed -- reloading nginx."
            if nginx -s reload; then
                # Whatever is on disk now came from the gateway, not from us.
                rm -f "$MARKER" 2>/dev/null || true
                last="$now"                     # advance ONLY after a verified reload
            else
                log "WARNING: nginx -s reload failed -- retrying on the next poll."
            fi
        done
    ) &
fi

# exec last: nginx stays PID 1's process in the normal way, so STOPSIGNAL/SIGQUIT
# shutdown and exit-code propagation behave exactly as in the stock image.
exec "$STOCK" "$@"
