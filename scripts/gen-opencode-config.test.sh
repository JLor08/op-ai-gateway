#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins gen-opencode-config.sh. Run from anywhere:
#   sh scripts/gen-opencode-config.test.sh
#
# Why this needs pinning: the script's only input is the gateway's model-list
# endpoint, and it once read a different one (the LM Studio-shaped
# /api/v0/models, since removed) than the gateway serves today. The failure mode
# is silent in the worst way: the request 404s, or the field it reads is gone,
# and the generated opencode.json simply carries no models -- opencode then
# falls back to its own default context window and nobody notices. So every
# assumption the script makes about the gateway gets a case: which URL it asks
# for, which field it reads, which models it must skip, and what it does when
# there is nothing to emit.
#
# Everything here is offline: the script runs for real, but `curl` is a stub on
# PATH that serves a fixture and records the arguments it was called with. No
# network, no gateway. jq is the one real dependency (as it is for the script).
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GEN="$SCRIPT_DIR/gen-opencode-config.sh"

[ -x "$GEN" ] || { echo "FAIL: $GEN is missing or not executable"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq is required for these tests"; exit 0; }

# The script reads these as fallbacks for --url/--token; a developer's own
# shell must not leak into the cases.
unset OP_GATEWAY_URL OP_GATEWAY_TOKEN

fail=0
FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

mkdir -p "$FIX/bin"
# The stub: records its argv one per line, then either fails (as `curl -f` does
# on a 404) or prints the fixture body.
cat >"$FIX/bin/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$CURL_ARGS"
if [ -n "${CURL_FAIL:-}" ]; then
  exit 22
fi
cat "$CURL_BODY"
EOF
chmod +x "$FIX/bin/curl"

CURL_ARGS="$FIX/curl-args"
CURL_BODY="$FIX/body.json"
export CURL_ARGS CURL_BODY

OUT=""
ERR=""
STATUS=0

# Runs the script with the stub curl first on PATH; leaves stdout in $OUT,
# stderr in $ERR, the exit code in $STATUS.
run() {
  set +e
  OUT="$(PATH="$FIX/bin:$PATH" "$GEN" "$@" 2>"$FIX/err")"
  STATUS=$?
  set -e
  ERR="$(cat "$FIX/err")"
}

# $1 = description, $2 = wanted, $3 = got
check_eq() {
  if [ "$2" = "$3" ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1 (wanted '$2', got '$3')"
    fail=1
  fi
}

# $1 = description, $2 = substring that must appear ("!" prefix: must NOT), $3 = haystack
check_has() {
  case "$2" in
    '!'*)
      if printf '%s' "$3" | grep -qF -- "${2#!}"; then
        echo "FAIL: $1 (unexpectedly found '${2#!}')"
        fail=1
      else
        echo "ok: $1"
      fi
      ;;
    *)
      if printf '%s' "$3" | grep -qF -- "$2"; then
        echo "ok: $1"
      else
        echo "FAIL: $1 (missing '$2')"
        fail=1
      fi
      ;;
  esac
}

# $1 = description, $2 = jq filter that must be true of $OUT
check_jq() {
  if printf '%s' "$OUT" | jq -e "$2" >/dev/null 2>&1; then
    echo "ok: $1"
  else
    echo "FAIL: $1 (jq -e '$2' was not true of the output)"
    printf '%s\n' "$OUT" | sed 's/^/     | /'
    fail=1
  fi
}

# --------------------------------------------------------------------------
# The happy path: a /v1/models body in, the opencode.json provider block out.
#   m1  context_length 8192    -> kept, output = min(floor(8192/4), 8192) = 2048
#   m2  no context_length      -> skipped (the gateway OMITS the field at 0)
#   m3  context_length 0       -> skipped (belt and suspenders for the omission)
#   m4  context_length 100000  -> kept, output capped at 8192
#   m5  legacy max_context_length only -> skipped: the old field is not read
# --------------------------------------------------------------------------
cat >"$CURL_BODY" <<'JSON'
{"object":"list","data":[
  {"id":"m1","object":"model","owned_by":"op-ai-gateway","context_length":8192},
  {"id":"m2","object":"model","owned_by":"op-ai-gateway"},
  {"id":"m3","object":"model","owned_by":"op-ai-gateway","context_length":0},
  {"id":"m4","object":"model","owned_by":"op-ai-gateway","context_length":100000},
  {"id":"m5","object":"model","owned_by":"op-ai-gateway","max_context_length":4096}
]}
JSON
run --url https://gw.example --token tok-123 --output -
check_eq "a /v1/models list is turned into a config (exit 0)" 0 "$STATUS"
check_jq "only models carrying a positive context_length are emitted, with limit.context = context_length and limit.output = min(floor(context/4), 8192)" \
  '.provider["op-gateway"].models == {
     "m1": {"name": "m1", "limit": {"context": 8192,   "output": 2048}},
     "m4": {"name": "m4", "limit": {"context": 100000, "output": 8192}}
   }'
check_jq "the provider is the OpenAI-compatible one, pointed at <base>/v1 with the token as apiKey" \
  '.provider["op-gateway"].npm == "@ai-sdk/openai-compatible"
   and .provider["op-gateway"].options == {"baseURL": "https://gw.example/v1", "apiKey": "tok-123"}'
check_has "the model list is fetched from <base>/v1/models" "https://gw.example/v1/models" "$(cat "$CURL_ARGS")"
check_has "the fetch is authenticated with the bearer token" "Authorization: Bearer tok-123" "$(cat "$CURL_ARGS")"
check_has "the removed LM Studio endpoint is not asked for" "!/api/v0/" "$(cat "$CURL_ARGS")"
check_has "no warning when at least one model has a context size" "!warning" "$ERR"

# --------------------------------------------------------------------------
# --url normalization: the user may paste the OpenAI base URL.
# --------------------------------------------------------------------------
run --url https://gw.example/v1 --token tok-123 --output -
check_has "a trailing /v1 is stripped before /v1/models is appended" "https://gw.example/v1/models" "$(cat "$CURL_ARGS")"
check_has "...and does not double up" "!/v1/v1" "$(cat "$CURL_ARGS")"
check_jq "...and options.baseURL stays <origin>/v1" \
  '.provider["op-gateway"].options.baseURL == "https://gw.example/v1"'

run --url https://gw.example/v1/ --token tok-123 --output -
check_has "a trailing /v1/ (slash, then /v1) is normalized the same way" "https://gw.example/v1/models" "$(cat "$CURL_ARGS")"
check_has "...with no doubled path" "!/v1/v1" "$(cat "$CURL_ARGS")"

run --url https://gw.example/ --token tok-123 --output -
check_has "a bare trailing slash is stripped" "https://gw.example/v1/models" "$(cat "$CURL_ARGS")"
check_has "...with no empty path segment" "!gw.example//" "$(cat "$CURL_ARGS")"

# --------------------------------------------------------------------------
# --provider, the environment fallbacks, and the output file.
# --------------------------------------------------------------------------
run --url https://gw.example --token tok-123 --provider mygw --output -
check_jq "--provider names the opencode provider id" \
  '.provider | keys == ["mygw"] and .mygw.models.m1.limit.context == 8192'

set +e
OUT="$(OP_GATEWAY_URL=https://gw.example OP_GATEWAY_TOKEN=env-tok PATH="$FIX/bin:$PATH" "$GEN" --output - 2>"$FIX/err")"
STATUS=$?
set -e
ERR="$(cat "$FIX/err")"
check_eq "OP_GATEWAY_URL / OP_GATEWAY_TOKEN stand in for the flags (exit 0)" 0 "$STATUS"
check_jq "...and the env token is the one sent and written" \
  '.provider["op-gateway"].options.apiKey == "env-tok"'

run --help
check_eq "--help succeeds (exit 0)" 0 "$STATUS"
check_has "...and documents every flag, including the last ones (the help text is carved out of the header comment)" "--output    Output path" "$OUT"
check_has "...and the endpoint it reads" "GET /v1/models" "$OUT"
check_has "...and no longer mentions the removed one" "!/api/v0/models" "$OUT"

run --url https://gw.example --token tok-123 --output "$FIX/opencode.json"
check_eq "--output <file> writes the file and prints nothing on stdout" "" "$OUT"
check_has "...and reports how many models carry a known context size" "wrote $FIX/opencode.json (2 model(s) with a known context size)" "$ERR"
check_eq "...and the file holds the same config" "2" "$(jq '.provider["op-gateway"].models | length' "$FIX/opencode.json")"

# --------------------------------------------------------------------------
# Nothing to emit: a well-formed list in which no model reports a context size
# must still produce a valid (empty) provider block and say what to do next.
# --------------------------------------------------------------------------
printf '%s' '{"object":"list","data":[{"id":"m2","object":"model","owned_by":"op-ai-gateway"}]}' >"$CURL_BODY"
run --url https://gw.example --token tok-123 --output -
check_eq "a list with no context sizes still succeeds (exit 0)" 0 "$STATUS"
check_jq "...and emits an empty models map" '.provider["op-gateway"].models == {}'
check_has "...and warns that no model reported a context size" "warning: no model reported a context size" "$ERR"
check_has "...and names an older gateway (no context_length on /v1/models) as the other cause" "predates context_length on /v1/models" "$ERR"

printf '%s' '{"object":"list","data":[]}' >"$CURL_BODY"
run --url https://gw.example --token tok-123 --output "$FIX/empty.json"
check_eq "an empty list succeeds (exit 0)" 0 "$STATUS"
check_has "...reports zero models" "(0 model(s) with a known context size)" "$ERR"
check_has "...and warns" "warning: no model reported a context size" "$ERR"
check_eq "...and writes an empty models map" "0" "$(jq '.provider["op-gateway"].models | length' "$FIX/empty.json")"

# --------------------------------------------------------------------------
# Failure modes: each must stop with a message, never write a half-config.
# --------------------------------------------------------------------------
printf '%s' '{"error":"unauthorized"}' >"$CURL_BODY"
run --url https://gw.example --token tok-123 --output -
check_eq "a body that is not a model list fails (exit 1)" 1 "$STATUS"
check_has "...and says so" "not an OpenAI model list" "$ERR"
check_has "...naming the URL it asked" "https://gw.example/v1/models" "$ERR"

printf '%s' '{"object":"list","data":"nope"}' >"$CURL_BODY"
run --url https://gw.example --token tok-123 --output -
check_eq "a list whose data is not an array fails (exit 1)" 1 "$STATUS"

CURL_FAIL=1
export CURL_FAIL
run --url https://gw.example --token tok-123 --output -
check_eq "a failed request (curl -f on a 404/401) fails (exit 1)" 1 "$STATUS"
check_has "...naming the URL it asked" "request to https://gw.example/v1/models failed" "$ERR"
unset CURL_FAIL

run --token tok-123 --output -
check_eq "a missing --url fails (exit 1)" 1 "$STATUS"
check_has "...and says which" "missing --url" "$ERR"

run --url https://gw.example --output -
check_eq "a missing --token fails (exit 1)" 1 "$STATUS"
check_has "...and says which" "missing --token" "$ERR"

if [ "$fail" = 0 ]; then
  echo "all gen-opencode-config cases passed"
else
  echo "gen-opencode-config.test.sh: FAILURES"
fi
exit "$fail"
