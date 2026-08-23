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
#  useless. Nine blocks: server-80, server-443 and the WebSocket location, in each of
#  the three deployable configurations (the two compose variants share locations.conf,
#  so the nine live in eight physical text blocks).
#  It also pins the structural invariants: `listen 80` and the ACME challenge location
#  survive, `listen 443 ssl` exists, and the shared include is NOT a *.conf under
#  conf.d/ (nginx auto-includes those at HTTP level, where `location` is illegal).
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
  [ "$blocks" -eq 3 ] \
    || bad "$name: expected 3 header-setting blocks (server-80, server-443, WebSocket location), found $blocks"
  total_blocks=$((total_blocks + blocks))
  IFS='
'
done
IFS=$old_ifs
note "total across the three configurations: $total_blocks (expected 9)"
[ "$total_blocks" -eq 9 ] || bad "expected 9 header-setting blocks in total, found $total_blocks"

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
