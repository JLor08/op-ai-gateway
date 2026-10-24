#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins the gateway's nginx configuration. Run from anywhere:
#   sh gateway/deploy/nginx-config.test.sh
#
# There are THREE hand-maintained copies of the same server configuration, and this
# nginx is the entrance to everything -- the portal, the whole API, and the login. A
# configuration that fails to load takes the gateway offline (this repo has had one
# outage of exactly that shape). So this script checks two independent things:
#
#  PART 1 (always): the header boundary. nginx DISCARDS the inherited proxy_set_header
#  set as soon as a block sets any header, so every block that sets one must set ALL
#  SIX X-OP-* directives: FIVE blankings -- the four internal auth / server-override
#  headers (which stop a client injecting them through the proxy) plus
#  X-OP-Edge-Self-Probe (a client-set marker SUPPRESSES an https observation, so it
#  could keep an armed plaintext gate extinguished against the attacker) -- and
#  X-OP-Edge-Scheme. The sixth is SET from $scheme and never blanked -- it is the
#  gateway's only unforgeable statement about the last hop, and a "" would make it
#  useless. Fifteen blocks: server-80, server-443 and the three WebSocket locations (the
#  agent stream, the live-transcription stream and the voice-chat stream), in each of the
#  three deployable configurations (the two compose variants share locations.conf, so the
#  fifteen live in ten physical text blocks).
#  It also pins the structural invariants: `listen 80` and the ACME challenge location
#  survive, `listen 443 ssl` exists, and the shared include is NOT a *.conf under
#  conf.d/ (nginx auto-includes those at HTTP level, where `location` is illegal).
#  And the two Voice Chat requirements that are NOT location-level: both server blocks
#  carry `large_client_header_buffers` of at least 16k (the WebSocket ticket is one header
#  line of up to 7 KiB), and no `proxy_read_timeout` anywhere is shorter than the co-load
#  gate's 120 s default (the ticket-mint POST rides the server-level value).
#
#  PART 2 (needs docker + openssl): `nginx -t` in the real nginx image for each of the
#  three configurations, with dummy certificates in place so the run tests SYNTAX and
#  directive validity. Skipped with a loud note when either tool is missing -- never
#  silently passed.
set -eu

cd "$(dirname "$0")"

NGINX_IMAGE=nginx:1.27-alpine
CONFIGMAP=k8s/nginx-configmap.yaml
REQUIRED_HEADERS="X-OP-Internal-Auth X-OP-Internal-User X-OP-Server-Override X-OP-Server-Override-Force X-OP-Edge-Self-Probe X-OP-Edge-Scheme"

fail=0
note() { echo "  $*"; }
bad() { echo "FAIL $*"; fail=1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

abspath() { echo "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"; }

# Extract one embedded nginx file from the k8s ConfigMap (POSIX awk, no yq needed).
extract_key() { # extract_key <yaml> <key>  -> stdout, 4-space block indent stripped
  awk -v key="$2" '
    $0 == "  " key ": |" { inblock = 1; next }
    inblock && /^  [^ ]/ { inblock = 0 }          # next mapping key at 2-space indent
    inblock { sub(/^    /, ""); print }
  ' "$1"
}
extract_key "$CONFIGMAP" default.conf   > "$work/k8s-default.conf"
extract_key "$CONFIGMAP" locations.conf > "$work/k8s-locations.conf"
[ -s "$work/k8s-default.conf" ]   || bad "$CONFIGMAP: could not extract the 'default.conf' key"
[ -s "$work/k8s-locations.conf" ] || bad "$CONFIGMAP: could not extract the 'locations.conf' key"

# The three DEPLOYABLE configurations: "<name> <server file> <locations file>".
# The two compose variants SHARE nginx/locations.conf -- that is the point of the include.
CONFIGURATIONS="netbird-compose|nginx/default.conf|nginx/locations.conf
no-netbird-compose|nginx/default.no-netbird.conf|nginx/locations.conf
k8s|$work/k8s-default.conf|$work/k8s-locations.conf"

# Prints one line per block that sets ANY proxy_set_header:
#   <block label>|<header count>|<,X-OP names,>|<edge-scheme-blanked 0|1>
header_blocks() {
  awk '
    function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
    # Every X-OP-* header name on one line (a one-line block can set several).
    function xop(s,   out, nm) {
      out = ""
      while (match(s, /X-OP-[A-Za-z-]+/)) {
        nm = substr(s, RSTART, RLENGTH)
        out = out nm ","
        s = substr(s, RSTART + RLENGTH)
      }
      return out
    }
    function nhdr(s,   c) { c = gsub(/proxy_set_header/, "&", s); return c }
    {
      line = $0
      sub(/#.*/, "", line)                         # ignore comments
      opens  = gsub(/\{/, "{", line)
      closes = gsub(/\}/, "}", line)
      hdr = (line ~ /proxy_set_header/)

      # A SELF-CONTAINED one-line block -- `location /x/ { proxy_set_header ...; }` --
      # that sets a header. nginx discards the whole inherited proxy_set_header set for
      # it exactly like a multi-line block, so it must carry all six directives too.
      # It must NOT be attributed to the ENCLOSING block either: that hid such a block
      # behind the server block own six and made the leak invisible.
      if (opens > 0 && opens == closes && hdr) {
        lbl = trim(substr($0, 1, index($0, "{") - 1)) " @line " NR
        bl = (line ~ /X-OP-Edge-Scheme/ && line !~ /\$scheme/) ? 1 : 0
        printf "%s|%d|,%s|%d\n", lbl, nhdr(line), xop(line), bl
        next
      }
      if (hdr) {
        total[depth] += nhdr(line)
        names[depth] = names[depth] xop(line)
        if (line ~ /X-OP-Edge-Scheme/ && line !~ /\$scheme/) blanked[depth] = 1
      }
      if (opens > closes) {                        # a block opens and stays open
        depth++
        # Line number included: both server blocks are labelled "server".
        label[depth] = trim(substr($0, 1, index($0, "{") - 1)) " @line " NR
        total[depth] = 0; names[depth] = ""; blanked[depth] = 0
      } else if (closes > opens) {                 # a block closes
        if (total[depth] > 0)
          printf "%s|%d|,%s|%d\n", label[depth], total[depth], names[depth], blanked[depth]
        depth--
      }
    }
  ' "$1"
}

# Prints one line per BADLY-VALUED blanked header directive. The five blanked headers
# (four internal + X-OP-Edge-Self-Probe) must be set to literally "". Checking only
# that the NAME appears lets a pass-through through:
#   proxy_set_header X-OP-Internal-Auth $http_x_op_internal_auth;
# which hands the client own header straight to the backend -- the exact injection the
# blanking exists to stop. X-OP-Edge-Scheme is the ONLY exclusion: it is SET from
# $scheme and is checked the other way round (a "" there is fatal).
blank_violations() {
  awk '
    {
      line = $0
      sub(/#.*/, "", line)
      rest = line
      while (match(rest, /proxy_set_header[ \t]+X-OP-[A-Za-z-]+/)) {
        d = substr(rest, RSTART, RLENGTH)
        rest = substr(rest, RSTART + RLENGTH)
        match(d, /X-OP-[A-Za-z-]+/); nm = substr(d, RSTART, RLENGTH)
        val = rest                                   # value = up to the next ";"
        if (index(val, ";") > 0) val = substr(val, 1, index(val, ";") - 1)
        gsub(/^[ \t]+|[ \t]+$/, "", val)
        if (nm != "X-OP-Edge-Scheme" && val != "\"\"")
          printf "line %d: proxy_set_header %s %s\n", NR, nm, (val == "" ? "(no value)" : val)
      }
    }
  ' "$1"
}

echo "== part 1: six X-OP-* header directives per header-setting block =="
total_blocks=0
old_ifs=$IFS
IFS='
'
for entry in $CONFIGURATIONS; do
  IFS=$old_ifs
  name=$(echo "$entry" | cut -d'|' -f1)
  server=$(echo "$entry" | cut -d'|' -f2)
  locations=$(echo "$entry" | cut -d'|' -f3)
  blocks=0
  for f in "$server" "$locations"; do
    header_blocks "$f" > "$work/hb.txt"
    while IFS='|' read -r label count names blanked; do
      [ -n "${label:-}" ] || continue
      blocks=$((blocks + 1))
      for want in $REQUIRED_HEADERS; do
        case "$names" in
          *",$want,"*) ;;
          *) bad "$name ($f): block '$label' sets $count header(s) but never sets 'proxy_set_header $want'" ;;
        esac
      done
      [ "$blanked" != "1" ] \
        || bad "$name ($f): block '$label' blanks X-OP-Edge-Scheme -- it must be SET from \$scheme"
    done < "$work/hb.txt"
  done
  note "$name: $blocks header-setting block(s), all six directives present"
  [ "$blocks" -eq 5 ] \
    || bad "$name: expected 5 header-setting blocks (server-80, server-443, the agent, live-transcription and voice-chat WebSocket locations), found $blocks"
  total_blocks=$((total_blocks + blocks))
  IFS='
'
done
IFS=$old_ifs
note "total across the three configurations: $total_blocks (expected 15)"
[ "$total_blocks" -eq 15 ] || bad "expected 15 header-setting blocks in total, found $total_blocks"

# A blanked X-OP-Edge-Scheme anywhere is fatal, however it is written.
for f in nginx/default.conf nginx/default.no-netbird.conf nginx/locations.conf "$CONFIGMAP"; do
  ! grep -q 'X-OP-Edge-Scheme[[:space:]]*""' "$f" \
    || bad "$f: X-OP-Edge-Scheme is blanked, it must be \$scheme"
done

# ...and conversely, each of the five blanked headers must be set to literally "".
echo "== part 1b: the five blanked headers are BLANKED (\"\"), not passed through =="
for f in nginx/default.conf nginx/default.no-netbird.conf nginx/locations.conf \
         "$work/k8s-default.conf" "$work/k8s-locations.conf"; do
  viol=$(blank_violations "$f")
  if [ -n "$viol" ]; then
    bad "$f: header(s) not blanked -- a client value would reach the backend:"
    echo "$viol" | sed 's/^/      /'
  fi
done
note "checked 5 files (2 compose server configs, the shared locations list, 2 k8s extracts)"

# Structural invariants. `listen 80` carries the ACME HTTP-01 challenge route, and
# fleet-wide certificate renewal depends on it.
for f in nginx/default.conf nginx/default.no-netbird.conf "$work/k8s-default.conf"; do
  grep -q '^[[:space:]]*listen 80;' "$f" \
    || bad "$f: 'listen 80' is gone -- ACME HTTP-01 renewal depends on it"
  grep -q '^[[:space:]]*listen 443 ssl;' "$f" || bad "$f: missing 'listen 443 ssl'"
  # The include must NOT be a *.conf under conf.d/: nginx.conf does
  # `include /etc/nginx/conf.d/*.conf;` at HTTP level, where `location` is illegal.
  ! grep -q 'include[[:space:]]\{1,\}/etc/nginx/conf\.d/.*\.conf;' "$f" \
    || bad "$f: includes a *.conf from conf.d/ -- nginx also auto-includes it at HTTP level and would fail to start"
  grep -q 'include[[:space:]]\{1,\}/etc/nginx/op-locations\.conf;' "$f" \
    || bad "$f: missing 'include /etc/nginx/op-locations.conf;'"
done
for f in nginx/locations.conf "$work/k8s-locations.conf"; do
  grep -q 'location /\.well-known/acme-challenge/' "$f" \
    || bad "$f: the ACME HTTP-01 challenge location is gone"
  grep -q 'location = /api/agent/v1/stream' "$f" \
    || bad "$f: the agent WebSocket location is gone -- the agent transport depends on it"
  grep -q 'location = /api/portal/transcription-live ' "$f" \
    || bad "$f: the live-transcription WebSocket location is gone -- the portal's live mode depends on it"
  grep -q 'location = /api/portal/voice-chat ' "$f" \
    || bad "$f: the voice-chat WebSocket location is gone -- the portal's Voice Chat mode depends on it"
  # The live-transcription WebSocket forwards the browser's Host AS SENT ($http_host), NOT
  # $host like the agent stream just above it: the gateway's same-origin check on the
  # upgrade compares the Origin header's host AND PORT with the Host it sees, and $host
  # drops the port -- so a copy-paste from the agent block would silently 403 every live
  # upgrade on the compose ports (:8080/:8443). Pinned inside that one location only.
  awk '/^[ \t]*location[ \t]+=[ \t]+\/api\/portal\/transcription-live[ \t]/ { inb = 1 }
       inb { line = $0; sub(/#.*/, "", line) }
       inb && line ~ /proxy_set_header[ \t]+Host[ \t]+\$http_host[ \t]*;/ { ok = 1 }
       inb && /^[ \t]*}/ { inb = 0 }
       END { exit ok ? 0 : 1 }' "$f" \
    || bad "$f: the live-transcription WebSocket location does not set 'proxy_set_header Host \$http_host;' -- \$host drops the port and the gateway's same-origin check would 403 every upgrade on a non-default port"
  # The voice-chat WebSocket is the live-transcription one again (a browser upgrade guarded
  # by the same same-origin check), so the same pin applies -- and, since its location is
  # a copy of that one, that it still upgrades the connection at all.
  awk '/^[ \t]*location[ \t]+=[ \t]+\/api\/portal\/voice-chat[ \t]/ { inb = 1 }
       inb { line = $0; sub(/#.*/, "", line) }
       inb && line ~ /proxy_set_header[ \t]+Host[ \t]+\$http_host[ \t]*;/ { ok = 1 }
       inb && /^[ \t]*}/ { inb = 0 }
       END { exit ok ? 0 : 1 }' "$f" \
    || bad "$f: the voice-chat WebSocket location does not set 'proxy_set_header Host \$http_host;' -- \$host drops the port and the gateway's same-origin check would 403 every upgrade on a non-default port"
  awk '/^[ \t]*location[ \t]+=[ \t]+\/api\/portal\/voice-chat[ \t]/ { inb = 1 }
       inb { line = $0; sub(/#.*/, "", line) }
       inb && line ~ /proxy_set_header[ \t]+Upgrade[ \t]+\$http_upgrade[ \t]*;/ { up = 1 }
       inb && line ~ /proxy_set_header[ \t]+Connection[ \t]+"upgrade"[ \t]*;/ { conn = 1 }
       inb && /^[ \t]*}/ { inb = 0 }
       END { exit (up && conn) ? 0 : 1 }' "$f" \
    || bad "$f: the voice-chat WebSocket location does not send 'Upgrade \$http_upgrade' and 'Connection \"upgrade\"' -- the upgrade would never reach the gateway"
  # The bearer live endpoint is a chunked FULL-DUPLEX POST: without an unbuffered request
  # body nginx holds the PCM until the speaker stops and no transcript ever comes back.
  awk '/^[ \t]*location[ \t]+~[ \t]+\^\/\(\?:openai\/\)\?v1\/audio\/transcriptions\/live\$/ { inb = 1 }
       inb && /proxy_request_buffering[ \t]+off;/ { ok = 1 }
       inb && /^[ \t]*}/ { inb = 0 }
       END { exit ok ? 0 : 1 }' "$f" \
    || bad "$f: the bearer live-transcription location is gone or no longer sets 'proxy_request_buffering off' -- full-duplex streaming depends on it"
done

# Voice Chat's two requirements that are not location-level (see the voice-chat location in
# locations.conf for the whole story):
#  * Header size. The WebSocket ticket is ONE Sec-WebSocket-Protocol header line of up to
#    7 KiB; nginx's default of 8k per line leaves under 1 KiB of margin, so BOTH server
#    blocks of each configuration set large_client_header_buffers with at least 16k per
#    buffer. The directive is illegal in a location, so only a server-block copy counts.
#  * Read timeout. The ticket-mint POST has no location of its own (`location /api/` serves
#    it) and blocks on the co-load gate for up to OP_AI_GATEWAY_VOICE_COLOAD_TIMEOUT (120 s
#    by default), so every proxy_read_timeout must be at least 120 s, and the server blocks
#    must set one (nginx's default is 60 s).
for f in nginx/default.conf nginx/default.no-netbird.conf "$work/k8s-default.conf"; do
  n=$(awk '{ line = $0; sub(/#.*/, "", line) }
           match(line, /large_client_header_buffers[ \t]+[0-9]+[ \t]+[0-9]+[kKmM];/) {
             split(substr(line, RSTART, RLENGTH), p, /[ \t;]+/)
             size = p[3] + 0
             if (p[3] ~ /[mM]$/) size = size * 1024
             if (size >= 16) ok++
           }
           END { print ok + 0 }' "$f")
  [ "$n" -eq 2 ] \
    || bad "$f: $n server block(s) set 'large_client_header_buffers' of at least 16k, expected 2 -- the Voice Chat WebSocket ticket (one header line of up to 7 KiB) would not fit"
done
for f in nginx/default.conf nginx/default.no-netbird.conf nginx/locations.conf \
         "$work/k8s-default.conf" "$work/k8s-locations.conf"; do
  # Each proxy_read_timeout, converted to seconds (units: ms, s (the default), m, h).
  short=$(awk '{ line = $0; sub(/#.*/, "", line) }
               match(line, /proxy_read_timeout[ \t]+[0-9]+(ms|s|m|h)?;/) {
                 split(substr(line, RSTART, RLENGTH), p, /[ \t;]+/)
                 v = p[2] + 0
                 if (p[2] ~ /ms$/) v = v / 1000
                 else if (p[2] ~ /m$/) v = v * 60
                 else if (p[2] ~ /h$/) v = v * 3600
                 if (v < 120) printf "line %d: %s\n", NR, p[2]
               }' "$f")
  [ -z "$short" ] \
    || bad "$f: a proxy_read_timeout under 120 s would cut the Voice Chat ticket mint (it waits on the co-load gate): $short"
done
for f in nginx/default.conf nginx/default.no-netbird.conf "$work/k8s-default.conf"; do
  n=$(grep -c '^[[:space:]]*proxy_read_timeout[[:space:]]\{1,\}[0-9]' "$f" || true)
  [ "$n" -eq 2 ] \
    || bad "$f: $n server block(s) set proxy_read_timeout, expected 2 -- the Voice Chat ticket mint would fall back to nginx's 60 s default"
done
grep -q '/etc/nginx/op-locations.conf' Dockerfile.frontend \
  || bad "Dockerfile.frontend: locations.conf is not installed at /etc/nginx/op-locations.conf"

# The :443 block above is only ALWAYS loadable because the image entrypoint writes a
# bootstrap certificate when none has been delivered yet. If that wiring is dropped, a
# first deploy has no certificate and nginx refuses to start -- portal, API and login.
grep -q 'apk add --no-cache openssl' Dockerfile.frontend \
  || bad "Dockerfile.frontend: openssl is not installed -- the entrypoint cannot write a bootstrap certificate"
grep -q '^ENTRYPOINT.*nginx-cert-entrypoint\.sh' Dockerfile.frontend \
  || bad "Dockerfile.frontend: the nginx-cert entrypoint wrapper is not the ENTRYPOINT"
# Declaring ENTRYPOINT RESETS the base image's CMD, so it must be restated -- otherwise
# the wrapper exec's the stock entrypoint with no arguments and nginx never starts.
grep -q '^CMD \["nginx"' Dockerfile.frontend \
  || bad "Dockerfile.frontend: CMD [\"nginx\", ...] is missing -- ENTRYPOINT resets the base image CMD"
[ -x nginx-cert-entrypoint.sh ] \
  || note "nginx-cert-entrypoint.sh is not executable in the repo (the Dockerfile chmod's it, so this is cosmetic)"

echo "== part 2: nginx -t in $NGINX_IMAGE =="
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  note "SKIPPED: docker is unavailable -- part 1 ran, the real nginx -t did NOT."
elif ! command -v openssl >/dev/null 2>&1; then
  note "SKIPPED: openssl is unavailable, so no dummy certificate could be generated."
else
  # `nginx -t` LOADS the certificate, so it has to be a real one. Throwaway, 1 day.
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=nginx-config-test" \
    -keyout "$work/edge-key.pem" -out "$work/edge-fullchain.pem" >/dev/null 2>&1 \
    || bad "could not generate a dummy certificate with openssl"
  chmod 0644 "$work/edge-key.pem" "$work/edge-fullchain.pem"
  IFS='
'
  for entry in $CONFIGURATIONS; do
    IFS=$old_ifs
    name=$(echo "$entry" | cut -d'|' -f1)
    server=$(abspath "$(echo "$entry" | cut -d'|' -f2)")
    locations=$(abspath "$(echo "$entry" | cut -d'|' -f3)")
    # Mounted exactly where Dockerfile.frontend / k8s web.yaml install them.
    # --add-host: the k8s variant uses a LITERAL `proxy_pass http://op-gateway-backend:8080;`,
    # and nginx resolves a literal upstream host at CONFIG LOAD time -- in the cluster that
    # is the backend Service's DNS name. Without it nginx -t fails with "host not found in
    # upstream", which says nothing about our syntax. (The compose variants proxy_pass a
    # variable and resolve at request time, so the entry is inert for them.)
    if out=$(docker run --rm \
        --add-host op-gateway-backend:127.0.0.1 \
        -v "$server":/etc/nginx/conf.d/default.conf:ro \
        -v "$locations":/etc/nginx/op-locations.conf:ro \
        -v "$work/edge-fullchain.pem":/etc/nginx/certs/edge-fullchain.pem:ro \
        -v "$work/edge-key.pem":/etc/nginx/certs/edge-key.pem:ro \
        "$NGINX_IMAGE" nginx -t 2>&1); then
      note "$name: $(echo "$out" | tail -1)"
    else
      bad "$name: nginx -t rejected the configuration:"
      echo "$out" | sed 's/^/      /'
    fi
    IFS='
'
  done
  IFS=$old_ifs
fi

[ "$fail" -eq 0 ] || { echo "nginx-config.test.sh FAILED"; exit 1; }
echo OK
