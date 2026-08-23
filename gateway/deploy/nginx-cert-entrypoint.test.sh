#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

#
# Behaviour test for nginx-cert-entrypoint.sh. Run from anywhere:
#   sh gateway/deploy/nginx-cert-entrypoint.test.sh
#
# The wrapper's whole reason to exist is that nginx must start even when the gateway
# has not delivered an edge certificate yet -- `ssl_certificate` on a missing file is
# a LOAD error and this nginx serves the portal, the API and the login. So these cases
# pin exactly that, plus the reload-on-change behaviour:
#
#   (i)   no certificate            -> a BOOTSTRAP pair is written (right CN, 0600 key),
#                                      marked, and the stock entrypoint is exec'd with
#                                      the original argv.
#   (ii)  a real certificate        -> NO bootstrap: no marker, both files byte-identical.
#   (iii) the certificate CHANGES   -> exactly ONE `nginx -s reload`.
#   (iv)  the certificate is stable -> NO reload at all.
#   (v)   no certificate + a NON-writable directory (the Kubernetes read-only Secret
#         case) -> a loud warning, but the stock entrypoint still runs: the wrapper
#         itself must never be the reason nginx does not start.
#
# Seams: NGINX_STOCK_ENTRYPOINT (a fake that records its argv, then lingers so the
# watcher has a live process to observe) and a fake `nginx` first on PATH that records
# every reload. Everything else is the real script.
set -u

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
WRAPPER="$SCRIPT_DIR/nginx-cert-entrypoint.sh"

[ -f "$WRAPPER" ] || { echo "FATAL: wrapper not found at $WRAPPER" >&2; exit 2; }
command -v openssl >/dev/null 2>&1 || { echo "FATAL: openssl is required to run this test" >&2; exit 2; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/op-nginx-cert-test.XXXXXX")
BIN="$TMP/bin"
mkdir -p "$BIN"

FAILED=0
BG_PID=""
fail() { echo "FAIL: $1"; FAILED=1; }
pass() { echo "PASS: $1"; }

cleanup() {
    [ -n "$BG_PID" ] && kill "$BG_PID" 2>/dev/null
    # The watcher runs in its own subshell; it self-exits within one poll interval once
    # the main process is gone (it checks kill -0 on the PID nginx would have).
    sleep 1.2
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# Fake stock entrypoint: record argv, then linger for $FAKE_SLEEP seconds so the
# watcher observes a live "nginx".
cat > "$TMP/fake-stock.sh" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$FAKE_RECORD"
sleep "${FAKE_SLEEP:-0}"
exit 0
EOF
chmod +x "$TMP/fake-stock.sh"

# Fake nginx: record every invocation (the watcher calls `nginx -s reload`).
cat > "$BIN/nginx" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$RELOAD_RECORD"
exit 0
EOF
chmod +x "$BIN/nginx"

# make_cert <keyfile> <certfile> <cn>: a real, distinct self-signed pair.
make_cert() {
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$3" \
        -keyout "$1" -out "$2" >/dev/null 2>&1
}

# wait_for_file <path> <timeout_secs>
wait_for_file() {
    _waited=0
    while [ ! -e "$1" ]; do
        [ "$_waited" -ge "$(( $2 * 10 ))" ] && return 1
        sleep 0.1
        _waited=$(( _waited + 1 ))
    done
    return 0
}

# run_wrapper <certdir> <poll> <fake_sleep> <record> <reload_record> <stderr> [args...]
run_wrapper() {
    _dir=$1; _poll=$2; _sleep=$3; _rec=$4; _rel=$5; _err=$6
    shift 6
    env PATH="$BIN:$PATH" \
        OP_NGINX_CERT_DIR="$_dir" \
        OP_NGINX_CERT_POLL_SECONDS="$_poll" \
        NGINX_STOCK_ENTRYPOINT="$TMP/fake-stock.sh" \
        FAKE_RECORD="$_rec" \
        FAKE_SLEEP="$_sleep" \
        RELOAD_RECORD="$_rel" \
        sh "$WRAPPER" "$@" 2> "$_err" &
    BG_PID=$!
}

reload_count() { [ -f "$1" ] && grep -c . "$1" 2>/dev/null || echo 0; }

# ---------------------------------------------------------------------------
# (i) empty certificate directory -> bootstrap written + marked + exec'd
# ---------------------------------------------------------------------------
case_i() {
    dir="$TMP/i/certs"; mkdir -p "$dir"
    rec="$TMP/i/record"; rel="$TMP/i/reload"; err="$TMP/i/err"

    # POLL=0 disables the watcher: this case is only about the bootstrap + exec.
    run_wrapper "$dir" 0 0 "$rec" "$rel" "$err" nginx -g "daemon off;"
    if ! wait_for_file "$rec" 15; then
        fail "(i) the stock entrypoint never ran -- nginx would not have started"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi
    wait "$BG_PID" 2>/dev/null; BG_PID=""

    [ -s "$dir/edge-fullchain.pem" ] || { fail "(i) no bootstrap chain was written"; return; }
    [ -s "$dir/edge-key.pem" ]       || { fail "(i) no bootstrap key was written"; return; }
    [ -f "$dir/.op-edge-bootstrap" ] || { fail "(i) the bootstrap marker file is missing"; return; }

    subject=$(openssl x509 -in "$dir/edge-fullchain.pem" -noout -subject 2>/dev/null)
    case "$subject" in
        *"OP AI Gateway BOOTSTRAP - not trusted"*) ;;
        *) fail "(i) bootstrap subject is '$subject', expected CN=OP AI Gateway BOOTSTRAP - not trusted"; return ;;
    esac

    # A world-readable private key would be a real defect, bootstrap or not.
    perms=$(ls -l "$dir/edge-key.pem" | cut -c1-10)
    [ "$perms" = "-rw-------" ] || { fail "(i) bootstrap key mode is '$perms', expected -rw------- (0600)"; return; }

    # argv must reach the stock entrypoint unchanged.
    argv=$(tr '\n' '|' < "$rec")
    [ "$argv" = "nginx|-g|daemon off;|" ] || { fail "(i) stock entrypoint got argv '$argv', expected 'nginx|-g|daemon off;|'"; return; }

    pass "(i) no certificate -> bootstrap pair (right CN, 0600 key) + marker, stock entrypoint exec'd with argv"
}

# ---------------------------------------------------------------------------
# (ii) a real certificate present -> nothing is written, nothing is replaced
# ---------------------------------------------------------------------------
case_ii() {
    dir="$TMP/ii/certs"; mkdir -p "$dir"
    rec="$TMP/ii/record"; rel="$TMP/ii/reload"; err="$TMP/ii/err"
    make_cert "$dir/edge-key.pem" "$dir/edge-fullchain.pem" "real.example.test"
    cp "$dir/edge-key.pem" "$TMP/ii/key.orig"
    cp "$dir/edge-fullchain.pem" "$TMP/ii/chain.orig"

    run_wrapper "$dir" 0 0 "$rec" "$rel" "$err" nginx
    if ! wait_for_file "$rec" 15; then
        fail "(ii) the stock entrypoint never ran"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi
    wait "$BG_PID" 2>/dev/null; BG_PID=""

    [ ! -e "$dir/.op-edge-bootstrap" ] || { fail "(ii) a bootstrap marker was written even though a real certificate exists"; return; }
    cmp -s "$TMP/ii/chain.orig" "$dir/edge-fullchain.pem" || { fail "(ii) the real chain was modified"; return; }
    cmp -s "$TMP/ii/key.orig" "$dir/edge-key.pem"         || { fail "(ii) the real key was modified"; return; }
    # No stray temp files left behind either.
    leftovers=$(ls -A "$dir" | grep -v '^edge-\(fullchain\|key\)\.pem$' | tr '\n' ' ')
    [ -z "$leftovers" ] || { fail "(ii) unexpected files in the certificate directory: $leftovers"; return; }

    pass "(ii) a real certificate is left byte-identical and no bootstrap is written"
}

# ---------------------------------------------------------------------------
# (iii) the certificate changes -> exactly ONE reload
# ---------------------------------------------------------------------------
case_iii() {
    dir="$TMP/iii/certs"; mkdir -p "$dir"
    rec="$TMP/iii/record"; rel="$TMP/iii/reload"; err="$TMP/iii/err"
    make_cert "$dir/edge-key.pem" "$dir/edge-fullchain.pem" "first.example.test"
    make_cert "$TMP/iii/new-key.pem" "$TMP/iii/new-chain.pem" "second.example.test"

    run_wrapper "$dir" 1 8 "$rec" "$rel" "$err" nginx
    # Waiting for the stock entrypoint also guarantees the watcher's baseline
    # fingerprint (taken BEFORE the exec) is already the FIRST certificate.
    if ! wait_for_file "$rec" 15; then
        fail "(iii) the stock entrypoint never ran"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi

    # Deliver the new pair the way the gateway does: rename, so no half-written read.
    mv "$TMP/iii/new-key.pem" "$dir/edge-key.pem"
    mv "$TMP/iii/new-chain.pem" "$dir/edge-fullchain.pem"

    if ! wait_for_file "$rel" 6; then
        fail "(iii) the certificate changed but nginx was never reloaded"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi
    # Give the watcher two more intervals: a change must reload ONCE, not repeatedly.
    sleep 2.5
    count=$(reload_count "$rel")
    kill "$BG_PID" 2>/dev/null; BG_PID=""

    [ "$count" = "1" ] || { fail "(iii) expected exactly 1 reload, got $count: $(tr '\n' ',' < "$rel")"; return; }
    got=$(cat "$rel")
    [ "$got" = "-s reload" ] || { fail "(iii) watcher ran 'nginx $got', expected 'nginx -s reload'"; return; }

    pass "(iii) a changed certificate triggers exactly one 'nginx -s reload'"
}

# ---------------------------------------------------------------------------
# (iv) nothing changes -> no reload at all
# ---------------------------------------------------------------------------
case_iv() {
    dir="$TMP/iv/certs"; mkdir -p "$dir"
    rec="$TMP/iv/record"; rel="$TMP/iv/reload"; err="$TMP/iv/err"
    make_cert "$dir/edge-key.pem" "$dir/edge-fullchain.pem" "stable.example.test"

    run_wrapper "$dir" 1 6 "$rec" "$rel" "$err" nginx
    if ! wait_for_file "$rec" 15; then
        fail "(iv) the stock entrypoint never ran"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi
    sleep 3.5    # >= three poll intervals
    count=$(reload_count "$rel")
    kill "$BG_PID" 2>/dev/null; BG_PID=""

    [ "$count" = "0" ] || { fail "(iv) certificate unchanged but nginx was reloaded $count time(s)"; return; }
    pass "(iv) an unchanged certificate never reloads nginx"
}

# ---------------------------------------------------------------------------
# (v) no certificate + non-writable directory (Kubernetes read-only Secret) ->
#     warn loudly, but still start nginx. Skipped as root, which ignores the mode.
# ---------------------------------------------------------------------------
case_v() {
    if [ "$(id -u)" = "0" ]; then
        echo "SKIP: (v) running as root -- directory permissions would not apply"
        return
    fi
    dir="$TMP/v/certs"; mkdir -p "$dir"
    rec="$TMP/v/record"; rel="$TMP/v/reload"; err="$TMP/v/err"
    chmod 0500 "$dir"

    run_wrapper "$dir" 0 0 "$rec" "$rel" "$err" nginx
    if ! wait_for_file "$rec" 15; then
        fail "(v) the wrapper did not reach the stock entrypoint on a read-only certificate directory"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; chmod 0700 "$dir"; return
    fi
    wait "$BG_PID" 2>/dev/null; BG_PID=""
    chmod 0700 "$dir"

    if ! grep -q "not writable" "$err"; then
        fail "(v) no warning about the non-writable certificate directory: $(cat "$err")"
        return
    fi
    pass "(v) non-writable certificate directory -> warns, still hands over to the stock entrypoint"
}

# ---------------------------------------------------------------------------
# (vi) REGRESSION -- the latched-dead-TLS bug. The gateway writes the pair as TWO
# atomic files (chain first), so the watcher can see a NEW chain next to the OLD key.
# Reloading that is silently fatal (nginx -t accepts it, nginx -s reload exits 0, every
# TLS handshake then fails) AND it latches: recording the new fingerprint means the
# later key write -- which does not change the chain -- never triggers another reload.
# So: while mismatched there must be NO reload, and once the key lands there must be
# exactly ONE. The second half is what proves "$last" was not advanced.
# ---------------------------------------------------------------------------
case_vi() {
    dir="$TMP/vi/certs"; mkdir -p "$dir"
    rec="$TMP/vi/record"; rel="$TMP/vi/reload"; err="$TMP/vi/err"
    make_cert "$dir/edge-key.pem" "$dir/edge-fullchain.pem" "old.example.test"
    make_cert "$TMP/vi/new-key.pem" "$TMP/vi/new-chain.pem" "new.example.test"

    run_wrapper "$dir" 1 12 "$rec" "$rel" "$err" nginx
    if ! wait_for_file "$rec" 15; then
        fail "(vi) the stock entrypoint never ran"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi

    # Phase 1: the NEW chain lands, the key has not caught up yet -> mismatched pair.
    cp "$TMP/vi/new-chain.pem" "$dir/edge-fullchain.pem"
    sleep 3.5                                   # >= three poll intervals
    count=$(reload_count "$rel")
    if [ "$count" != "0" ]; then
        fail "(vi) reloaded $count time(s) on a MISMATCHED pair -- that reload kills the TLS listener"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi

    # Phase 2: the key catches up. The chain did NOT change here, so this can only
    # reload if the mismatched chain was never recorded as "seen".
    cp "$TMP/vi/new-key.pem" "$dir/edge-key.pem"
    if ! wait_for_file "$rel" 6; then
        fail "(vi) LATCHED: the pair matches again but nginx was never reloaded"
        kill "$BG_PID" 2>/dev/null; BG_PID=""; return
    fi
    sleep 2.5                                   # and it must not keep firing
    count=$(reload_count "$rel")
    kill "$BG_PID" 2>/dev/null; BG_PID=""

    [ "$count" = "1" ] || { fail "(vi) expected exactly 1 reload after the key landed, got $count"; return; }
    pass "(vi) a mismatched chain/key never reloads, and the reload happens once the key lands (no latch)"
}

case_i
case_ii
case_iii
case_iv
case_v
case_vi

[ "$FAILED" -eq 0 ] || { echo "RESULT: FAILED"; exit 1; }
echo "RESULT: all cases passed"
exit 0
