#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

#
# Behaviour test for netbird-enroll-entrypoint.sh.
#
# It drives the wrapper with a FAKE stock entrypoint (recorded via NB_STOCK_ENTRYPOINT)
# that writes the NB_SETUP_KEY it was handed to a record file, then exits 0. Each case
# uses a private scratch dir for the marker + key file. Run: bash netbird-enroll-entrypoint.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER="$SCRIPT_DIR/netbird-enroll-entrypoint.sh"

if [[ ! -f "$WRAPPER" ]]; then
  echo "FATAL: wrapper not found at $WRAPPER" >&2
  exit 2
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/nb-enroll-test.XXXXXX")"
FAKE="$TMP/fake-stock-entrypoint.sh"
cat > "$FAKE" <<'EOF'
#!/usr/bin/env bash
# Fake stock entrypoint: record what netbird would see. $FAKE_RECORD gets the setup
# key value; $FAKE_RECORD.env records — one token per line — which of the two
# mutually-exclusive setup-key env vars are still PRESENT (KEY / KEY_FILE). netbird
# aborts if BOTH are present, so the wrapper must leave at most one.
printf '%s' "${NB_SETUP_KEY:-}" > "$FAKE_RECORD"
printf '%s' "${NB_MANAGEMENT_URL:-}" > "$FAKE_RECORD.mgmt"
: > "$FAKE_RECORD.env"
[[ -n "${NB_SETUP_KEY+x}" ]] && echo KEY >> "$FAKE_RECORD.env"
[[ -n "${NB_SETUP_KEY_FILE+x}" ]] && echo KEY_FILE >> "$FAKE_RECORD.env"
exit 0
EOF
chmod +x "$FAKE"

BG_PID=""
cleanup() {
  [[ -n "$BG_PID" ]] && kill "$BG_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT

FAILED=0
fail() { echo "FAIL: $1"; FAILED=1; }
pass() { echo "PASS: $1"; }

# wait_for_file <path> <timeout_secs>: 0 if the file appears within the timeout.
wait_for_file() {
  local path="$1" timeout="$2" waited=0
  while [[ ! -e "$path" ]]; do
    if (( waited >= timeout * 10 )); then return 1; fi
    sleep 0.1
    waited=$(( waited + 1 ))
  done
  return 0
}

# ---------------------------------------------------------------------------
# Case (i): no marker + no NB_SETUP_KEY + KEY_FILE set → block until the key is
# written, then hand the trimmed key to the fake + create the marker.
# ---------------------------------------------------------------------------
case_i() {
  local dir="$TMP/i" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"
  keyfile="$dir/key"        # does not exist yet
  record="$dir/record"

  env -u NB_SETUP_KEY \
    NB_ENROLL_MARKER="$marker" \
    NB_ENROLL_KEY_FILE="$keyfile" \
    NB_STOCK_ENTRYPOINT="$FAKE" \
    FAKE_RECORD="$record" \
    bash "$WRAPPER" &
  BG_PID=$!

  # It must still be waiting after ~1s: the fake has not run yet.
  sleep 1
  if [[ -e "$record" ]]; then
    fail "(i) wrapper did not wait — fake ran before the key file existed"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi

  # Drop the key (with surrounding whitespace, to prove trimming).
  printf '  mykey123\n' > "$keyfile"

  if ! wait_for_file "$record" 10; then
    fail "(i) fake never ran after the key was written"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""

  local got; got="$(cat "$record")"
  if [[ "$got" != "mykey123" ]]; then
    fail "(i) fake saw NB_SETUP_KEY='$got', expected 'mykey123' (trimmed)"
    return
  fi
  if [[ ! -f "$marker" ]]; then
    fail "(i) marker was not created after enrollment"
    return
  fi
  pass "(i) waits for the key, trims it, hands it to netbird, and sets the marker"
}

# ---------------------------------------------------------------------------
# Case (ii): marker present + no key + KEY_FILE points at a missing path → do
# NOT block; exec the fake immediately with an empty NB_SETUP_KEY.
# ---------------------------------------------------------------------------
case_ii() {
  local dir="$TMP/ii" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"
  keyfile="$dir/key-missing"   # never created
  record="$dir/record"
  touch "$marker"

  env -u NB_SETUP_KEY \
    NB_ENROLL_MARKER="$marker" \
    NB_ENROLL_KEY_FILE="$keyfile" \
    NB_STOCK_ENTRYPOINT="$FAKE" \
    FAKE_RECORD="$record" \
    bash "$WRAPPER" &
  BG_PID=$!

  if ! wait_for_file "$record" 3; then
    fail "(ii) wrapper blocked despite the marker being present"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""

  local got; got="$(cat "$record")"
  if [[ -n "$got" ]]; then
    fail "(ii) fake saw NB_SETUP_KEY='$got', expected empty"
    return
  fi
  pass "(ii) marker present → exec's immediately with an empty NB_SETUP_KEY"
}

# ---------------------------------------------------------------------------
# Case (iii): NB_SETUP_KEY set in the env (no marker) → do NOT block; the fake
# sees the env key and the marker is created.
# ---------------------------------------------------------------------------
case_iii() {
  local dir="$TMP/iii" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"
  keyfile="$dir/key-missing"   # never created; must be ignored
  record="$dir/record"

  env \
    NB_SETUP_KEY="envkey" \
    NB_ENROLL_MARKER="$marker" \
    NB_ENROLL_KEY_FILE="$keyfile" \
    NB_STOCK_ENTRYPOINT="$FAKE" \
    FAKE_RECORD="$record" \
    bash "$WRAPPER" &
  BG_PID=$!

  if ! wait_for_file "$record" 3; then
    fail "(iii) wrapper blocked despite NB_SETUP_KEY being set"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""

  local got; got="$(cat "$record")"
  if [[ "$got" != "envkey" ]]; then
    fail "(iii) fake saw NB_SETUP_KEY='$got', expected 'envkey'"
    return
  fi
  if [[ ! -f "$marker" ]]; then
    fail "(iii) marker was not created"
    return
  fi
  pass "(iii) NB_SETUP_KEY env set → exec's immediately + sets the marker"
}

# ---------------------------------------------------------------------------
# Case (iv) — REGRESSION for the 502-login bug: the sidecar env carries BOTH an
# EMPTY NB_SETUP_KEY (compose default) AND a legacy NB_SETUP_KEY_FILE. netbird's CLI
# aborts every command when BOTH setup-key flags are present, which (shared netns)
# 502s the whole gateway. The wrapper MUST hand netbird NEITHER — an unset
# NB_SETUP_KEY_FILE and, because the key is empty, an unset NB_SETUP_KEY.
# ---------------------------------------------------------------------------
case_iv() {
  local dir="$TMP/iv" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"
  keyfile="$dir/key-missing"   # never created
  record="$dir/record"
  touch "$marker"              # already enrolled → no wait

  env \
    NB_SETUP_KEY="" \
    NB_SETUP_KEY_FILE="$keyfile" \
    NB_ENROLL_MARKER="$marker" \
    NB_STOCK_ENTRYPOINT="$FAKE" \
    FAKE_RECORD="$record" \
    bash "$WRAPPER" &
  BG_PID=$!

  if ! wait_for_file "$record" 3; then
    fail "(iv) wrapper never exec'd the stock entrypoint"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""

  local present; present="$(sort "$record.env" 2>/dev/null | tr '\n' ',')"
  if [[ -n "$present" ]]; then
    fail "(iv) netbird would still see setup-key env var(s): [$present] — mutual-exclusion crash"
    return
  fi
  pass "(iv) empty NB_SETUP_KEY + legacy NB_SETUP_KEY_FILE → netbird sees NEITHER (no crash)"
}

# ---------------------------------------------------------------------------
# Case (v): fresh enroll via the LEGACY NB_SETUP_KEY_FILE var (not-yet-renamed env)
# → the wrapper still resolves the path (fallback), loads the key into NB_SETUP_KEY,
# and strips NB_SETUP_KEY_FILE so netbird sees ONLY the key.
# ---------------------------------------------------------------------------
case_v() {
  local dir="$TMP/v" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"
  keyfile="$dir/key"
  record="$dir/record"
  printf 'legacykey\n' > "$keyfile"

  env -u NB_SETUP_KEY \
    NB_SETUP_KEY_FILE="$keyfile" \
    NB_ENROLL_MARKER="$marker" \
    NB_STOCK_ENTRYPOINT="$FAKE" \
    FAKE_RECORD="$record" \
    bash "$WRAPPER" &
  BG_PID=$!

  if ! wait_for_file "$record" 5; then
    fail "(v) wrapper never exec'd after the legacy key file was present"
    kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""
    return
  fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""

  local got present
  got="$(cat "$record")"
  present="$(sort "$record.env" 2>/dev/null | tr '\n' ',')"
  if [[ "$got" != "legacykey" ]]; then
    fail "(v) fake saw NB_SETUP_KEY='$got', expected 'legacykey'"
    return
  fi
  if [[ "$present" != "KEY," ]]; then
    fail "(v) netbird env was [$present], expected only KEY (NB_SETUP_KEY_FILE must be stripped)"
    return
  fi
  pass "(v) legacy NB_SETUP_KEY_FILE resolves the path but netbird sees only NB_SETUP_KEY"
}

# ---------------------------------------------------------------------------
# Case (vi): a mgmt-url companion file next to the key → the wrapper loads it into
# NB_MANAGEMENT_URL (so the sidecar targets the gateway-configured server), and an
# explicit NB_MANAGEMENT_URL env still WINS.
# ---------------------------------------------------------------------------
case_vi() {
  local dir="$TMP/vi" marker keyfile record
  mkdir -p "$dir"
  marker="$dir/marker"; keyfile="$dir/key"; record="$dir/record"
  printf 'k\n' > "$keyfile"
  printf '  https://nb.example.test\n' > "$keyfile.mgmt-url"

  # (a) no env → companion file is used
  env -u NB_SETUP_KEY -u NB_MANAGEMENT_URL \
    NB_ENROLL_MARKER="$marker" NB_ENROLL_KEY_FILE="$keyfile" \
    NB_STOCK_ENTRYPOINT="$FAKE" FAKE_RECORD="$record" bash "$WRAPPER" &
  BG_PID=$!
  if ! wait_for_file "$record" 5; then fail "(vi) wrapper never exec'd"; kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""; return; fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""
  if [[ "$(cat "$record.mgmt")" != "https://nb.example.test" ]]; then
    fail "(vi) NB_MANAGEMENT_URL from companion file = '$(cat "$record.mgmt")', expected trimmed URL"; return
  fi

  # (b) explicit env wins over the companion file
  rm -f "$record" "$record.mgmt"; touch "$marker"
  env -u NB_SETUP_KEY NB_MANAGEMENT_URL="https://env.example.test" \
    NB_ENROLL_MARKER="$marker" NB_ENROLL_KEY_FILE="$keyfile" \
    NB_STOCK_ENTRYPOINT="$FAKE" FAKE_RECORD="$record" bash "$WRAPPER" &
  BG_PID=$!
  if ! wait_for_file "$record" 5; then fail "(vi) wrapper never exec'd (env case)"; kill "$BG_PID" 2>/dev/null; wait "$BG_PID" 2>/dev/null; BG_PID=""; return; fi
  wait "$BG_PID" 2>/dev/null; BG_PID=""
  if [[ "$(cat "$record.mgmt")" != "https://env.example.test" ]]; then
    fail "(vi) env NB_MANAGEMENT_URL did not win: got '$(cat "$record.mgmt")'"; return
  fi
  pass "(vi) mgmt-url companion → NB_MANAGEMENT_URL, explicit env wins"
}

case_i
case_ii
case_iii
case_iv
case_v
case_vi

if (( FAILED != 0 )); then
  echo "RESULT: FAILED"
  exit 1
fi
echo "RESULT: all 6 cases passed"
exit 0
