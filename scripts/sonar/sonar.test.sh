#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins sonar.sh's startup wait (wait_for_up) and the server-version record
# against a server image that is not the SonarQube release the data volume was
# created with. Run from anywhere:
#   bash scripts/sonar/sonar.test.sh
#
# Why this needs pinning: the server keeps its data in an embedded database
# that SonarQube can neither upgrade nor downgrade. A newer image on an older
# volume sits in DB_MIGRATION_NEEDED forever; an older image on a newer volume
# never answers and its container exits (code 0). A plain wait mistakes both
# for a slow start and sits out the whole SONAR_UP_TIMEOUT before a generic
# "timed out".
#
# Everything here is offline. Each case runs in its own bash process that
# sources sonar.sh (which runs main only when executed) and replaces the
# external commands it calls -- curl, docker, sleep, and docker compose through
# its compose() wrapper -- with the stubs below; require_cmd becomes a no-op.
# Every other sonar.sh function runs unchanged, under its own
# `set -euo pipefail`. No SonarQube server, no docker. The last cases check
# that both images stay pinned, and execute sonar.sh (--help, and an unknown
# subcommand) to pin that main still runs.
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SONAR_SH="$SCRIPT_DIR/sonar.sh"

# sonar.sh parses the status endpoint and the credentials with jq, and so do
# these cases through it. Missing jq is a failure, not a skip: a skipped suite
# reads as a pass.
command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is required for these tests"; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
check() {
  # $1 = description, $2 = expected (substring or "!" + substring), $3 = haystack
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
check_eq() {
  # $1 = description, $2 = expected, $3 = actual
  if [ "$2" = "$3" ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1 (expected '$2', got '$3')"
    fail=1
  fi
}

# The stubs read a per-case directory ($CASE):
#   status       one /api/system/status answer per curl call (the last line
#                repeats); FAIL makes that call fail like a refused connection
#   states       one container state per `compose ps` call (the last repeats)
#   logs         what `compose logs` prints first
#   logs-after   what it prints 0.2 s later, the way a real stack trace
#                trails the line that names the failure
#   images       what `compose config --images` prints
#   image-env    what `docker image inspect` prints, with sonar.sh's exact
#                --format, for the image named in `images` (the image's Env)
#   compose-fails  present: every compose call fails
# and record what sonar.sh did with the outside world:
#   compose      every compose invocation
#   docker-run   the arguments of `docker run` (the scanner), one per line
#   sleeps       one line per sleep
cat >"$TMP/stubs.sh" <<'EOF'
nth_line() { # <file> <n>: line n, or the last line when the file is shorter
  local total
  total="$(wc -l <"$1" | tr -d ' ')"
  if [ "$2" -gt "$total" ]; then tail -n 1 "$1"; else sed -n "${2}p" "$1"; fi
}
bump() { # <counter file>: increment it and print the new value
  local n
  n=$(( $(cat "$1" 2>/dev/null || echo 0) + 1 ))
  echo "$n" >"$1"
  echo "$n"
}
curl() {
  local arg url=""
  for arg in "$@"; do
    case "$arg" in http*) url="$arg" ;; esac
  done
  case "$url" in
    */api/system/status)
      local line
      line="$(nth_line "$CASE/status" "$(bump "$CASE/status-calls")")"
      [ "$line" != FAIL ] || return 7
      printf '%s\n' "$line"
      ;;
    */api/authentication/validate) printf '{"valid":true}\n' ;;
    *) return 22 ;;
  esac
}
compose() {
  echo "$*" >>"$CASE/compose"
  [ ! -f "$CASE/compose-fails" ] || return 1
  case "$*" in
    # Only sonar.sh's exact query answers: without -a docker lists no exited
    # container, and {{.Status}} would print "Exited (0) ...", not "exited".
    "ps -a --format {{.State}} sonarqube")
      local n
      n="$(bump "$CASE/ps-calls")"
      if [ -f "$CASE/states" ]; then nth_line "$CASE/states" "$n"; fi
      ;;
    "logs --tail=200 sonarqube")
      if [ -f "$CASE/logs" ]; then cat "$CASE/logs"; fi
      # Like the real compose, which is one process: a write into a pipe its
      # reader has closed ends it (SIGPIPE), and that is its exit status.
      if [ -f "$CASE/logs-after" ]; then command sleep 0.2; cat "$CASE/logs-after" || return; fi
      ;;
    "config --images")
      if [ -f "$CASE/images" ]; then cat "$CASE/images"; fi
      ;;
  esac
  return 0
}
docker() {
  # Only sonar.sh's exact query, for the image compose names, answers.
  if [ "$#" -eq 5 ] && [ "$1 $2 $3" = "image inspect --format" ] &&
    [ "$4" = '{{range .Config.Env}}{{println .}}{{end}}' ] &&
    [ -f "$CASE/images" ] && [ "$5" = "$(head -n 1 "$CASE/images")" ] &&
    [ -f "$CASE/image-env" ]; then
    cat "$CASE/image-env"
  elif [ "${1:-}" = run ]; then
    # The scanner: record what would run, then stop the scan here.
    printf '%s\n' "$@" >"$CASE/docker-run"
    return 1
  else
    return 1
  fi
}
sleep() { echo slept >>"$CASE/sleeps"; }
require_cmd() { :; } # nothing external is needed offline
EOF

# new_case <name>: a fresh case directory, with a fresh SONAR_LOCAL_DIR in it.
new_case() {
  CASE="$TMP/$1"
  mkdir -p "$CASE/local"
  CASE_SONAR_IMAGE=""
}
# run <function> [args...]: run one sonar.sh function in its own bash process
# against the current case's stubs. Sets OUT (stdout+stderr) and RC. The case
# runs under the bash running this file ($BASH), not whichever bash is first on
# PATH, so `/bin/bash scripts/sonar/sonar.test.sh` really tests bash 3.2.
run() {
  OUT="$(CASE="$CASE" SONAR_LOCAL_DIR="$CASE/local" SONAR_UP_TIMEOUT="${UP_TIMEOUT:-300}" \
    SONAR_IMAGE="$CASE_SONAR_IMAGE" SONAR_SCANNER_IMAGE="" \
    "$BASH" -c '. "$1"; . "$2"; shift 2; "$@"' _ "$SONAR_SH" "$TMP/stubs.sh" "$@" 2>&1)"
  RC=$?
}
count() { # <file>: its line count, 0 when absent
  if [ -f "$1" ]; then wc -l <"$1" | tr -d ' '; else echo 0; fi
}
record() { # the case's recorded server version, or "absent"
  if [ -e "$CASE/local/server-version" ]; then cat "$CASE/local/server-version"; else echo absent; fi
}
composed() { cat "$CASE/compose" 2>/dev/null || true; }
status_json() { # <status> <version>
  printf '{"id":"x","version":"%s","status":"%s"}\n' "$2" "$1"
}
V268=26.8.0.126808
V269=26.9.0.129388
DB_NEWER='Caused by: Database was upgraded to a more recent version of SonarQube. A backup must probably be restored or the DB settings are incorrect.'

# --- 1. UP on the first poll ----------------------------------------------------
new_case up-first
status_json UP "$V268" >"$CASE/status"
run wait_for_up
check_eq "UP: exit 0" 0 "$RC"
check_eq "UP: no sleep" 0 "$(count "$CASE/sleeps")"

# --- 2. A slow start still waits for UP -----------------------------------------
new_case slow-start
{ status_json STARTING "$V268"; status_json STARTING "$V268"; status_json UP "$V268"; } >"$CASE/status"
printf 'running\n' >"$CASE/states"
run wait_for_up
check_eq "slow start: exit 0" 0 "$RC"
check_eq "slow start: two sleeps" 2 "$(count "$CASE/sleeps")"

# --- 3. Volume older than the image, release recorded: fail fast, both ways ----
new_case older
{ status_json STARTING "$V269"; status_json DB_MIGRATION_NEEDED "$V269"; } >"$CASE/status"
printf 'running\n' >"$CASE/states"
printf '%s\n' "$V268" >"$CASE/local/server-version"
run wait_for_up
check_eq "older volume: exit 1" 1 "$RC"
check_eq "older volume: fails on the poll that sees it" 1 "$(count "$CASE/sleeps")"
check "older volume: names the running release and the state" "SonarQube $V269 reports DB_MIGRATION_NEEDED" "$OUT"
check "older volume: says which way the mismatch goes" "created by an older SonarQube" "$OUT"
check "older volume: names the matching image" "export SONAR_IMAGE=sonarqube:$V268-community" "$OUT"
check "older volume: names purge" "purge && " "$OUT"
check "older volume: no unset hint without an override" "!unset SONAR_IMAGE" "$OUT"
check "older volume: not the generic timeout" "!timed out" "$OUT"
check "older volume: stops the stuck container" "down" "$(composed)"
check "older volume: keeps the volume" "!-v" "$(composed)"
check_eq "older volume: the record is left alone" "$V268" "$(record)"

# --- 4. Same, with no release recorded --------------------------------------------
new_case older-unrecorded
status_json DB_MIGRATION_NEEDED "$V269" >"$CASE/status"
printf 'running\n' >"$CASE/states"
run wait_for_up
check_eq "older volume, unrecorded: exit 1" 1 "$RC"
check "older volume, unrecorded: says nothing is recorded" "None is recorded" "$OUT"
check "older volume, unrecorded: says how to read an image's release" "grep SONAR_VERSION" "$OUT"
check "older volume, unrecorded: still names purge" "purge && " "$OUT"
check "older volume, unrecorded: invents no release" "!export SONAR_IMAGE=sonarqube:2" "$OUT"

# --- 5. A record that names the failing release is stale ----------------------
new_case older-stale
status_json DB_MIGRATION_NEEDED "$V269" >"$CASE/status"
printf 'running\n' >"$CASE/states"
printf '%s\n' "$V269" >"$CASE/local/server-version"
run wait_for_up
check_eq "older volume, stale record: exit 1" 1 "$RC"
check "older volume, stale record: does not recommend the failing image" "!export SONAR_IMAGE=sonarqube:$V269" "$OUT"
check "older volume, stale record: falls back to the unrecorded hint" "None is recorded" "$OUT"

# --- 6. An override still set: the fresh start must drop it ------------------
new_case older-override
status_json DB_MIGRATION_NEEDED "$V269" >"$CASE/status"
printf 'running\n' >"$CASE/states"
CASE_SONAR_IMAGE=sonarqube:community
run wait_for_up
check_eq "override set: exit 1" 1 "$RC"
check "override set: way 2 unsets it first" "unset SONAR_IMAGE; " "$OUT"
check "override set: says why" "SONAR_IMAGE=sonarqube:community is set" "$OUT"

# --- 7. The release cannot be read after DB_MIGRATION_NEEDED -------------------
new_case older-no-version
{ status_json DB_MIGRATION_NEEDED "$V269"; printf 'FAIL\n'; } >"$CASE/status"
printf 'running\n' >"$CASE/states"
run wait_for_up
check_eq "older volume, version unreadable: exit 1" 1 "$RC"
check "older volume, version unreadable: says so" "SonarQube (version unknown) reports DB_MIGRATION_NEEDED" "$OUT"
check "older volume, version unreadable: still names both ways" "Two ways out" "$OUT"

# --- 8. Volume newer than the image: the container exits, logs say why ---------
# The line that names the failure is followed, 0.2 s later, by the rest of the
# stack trace. Read with an early-exiting `grep -q` under pipefail, compose
# would die of SIGPIPE on that trailing output and the check would read false.
new_case newer
printf 'FAIL\n' >"$CASE/status"
printf 'running\nexited\n' >"$CASE/states"
printf 'web[] WARN Exception during startup\n%s\n' "$DB_NEWER" >"$CASE/logs"
for i in $(seq 1 40); do printf '\tat org.sonar.server.platform.Frame%s(Frame.java:%s)\n' "$i" "$i"; done >"$CASE/logs-after"
printf 'app[] INFO SonarQube is stopped\n' >>"$CASE/logs-after"
printf 'sonarqube:%s-community\n' "$V268" >"$CASE/images"
printf 'SONARQUBE_HOME=/opt/sonarqube\nSONAR_VERSION=%s\nSQ_DATA_DIR=/opt/sonarqube/data\n' "$V268" >"$CASE/image-env"
printf '%s\n' "$V269" >"$CASE/local/server-version"
run wait_for_up
check_eq "newer volume: exit 1" 1 "$RC"
check_eq "newer volume: fails on the poll that sees the exit" 1 "$(count "$CASE/sleeps")"
check "newer volume: says which way the mismatch goes" "created by a newer SonarQube" "$OUT"
check "newer volume: names both ways" "Two ways out" "$OUT"
check "newer volume: is not the generic exit" "!SonarQube exited during startup" "$OUT"
check "newer volume: names the matching image" "export SONAR_IMAGE=sonarqube:$V269-community" "$OUT"
check "newer volume: names purge" "purge && " "$OUT"
check "newer volume: not the generic timeout" "!timed out" "$OUT"
check "newer volume: reads the last 200 log lines" "logs --tail=200 sonarqube" "$(composed)"
check "newer volume: removes the exited container" "down" "$(composed)"
check "newer volume: keeps the volume" "!-v" "$(composed)"

# --- 9. Newer volume, and the record names the image that just failed ---------
new_case newer-stale
printf 'FAIL\n' >"$CASE/status"
printf 'exited\n' >"$CASE/states"
printf '%s\n' "$DB_NEWER" >"$CASE/logs"
printf 'sonarqube:%s-community\n' "$V268" >"$CASE/images"
printf 'SONARQUBE_HOME=/opt/sonarqube\nSONAR_VERSION=%s\nSQ_DATA_DIR=/opt/sonarqube/data\n' "$V268" >"$CASE/image-env"
printf '%s\n' "$V268" >"$CASE/local/server-version"
run wait_for_up
check_eq "newer volume, stale record: exit 1" 1 "$RC"
check "newer volume, stale record: does not recommend the failing image" "!export SONAR_IMAGE=sonarqube:$V268" "$OUT"
check "newer volume, stale record: falls back to the unrecorded hint" "None is recorded" "$OUT"

# --- 10. The container exits for another reason: fail fast, show the logs -------
new_case other-exit
printf 'FAIL\n' >"$CASE/status"
printf 'exited\n' >"$CASE/states"
printf 'java.lang.OutOfMemoryError: something else entirely\n' >"$CASE/logs"
run wait_for_up
check_eq "other exit: exit 1" 1 "$RC"
check_eq "other exit: no sleep" 0 "$(count "$CASE/sleeps")"
check "other exit: says the container exited" "SonarQube exited during startup" "$OUT"
check "other exit: shows the logs" "something else entirely" "$OUT"
check "other exit: is no version mismatch" "!Two ways out" "$OUT"
check "other exit: leaves the container for inspection" "!down" "$(composed)"

# --- 11. Never UP, never exits: the timeout still applies -----------------------
new_case timeout
status_json STARTING "$V268" >"$CASE/status"
printf 'running\n' >"$CASE/states"
UP_TIMEOUT=6 run wait_for_up
check_eq "timeout: exit 1" 1 "$RC"
check "timeout: says so" "timed out waiting for SonarQube" "$OUT"
check_eq "timeout: polls until the budget is spent" 2 "$(count "$CASE/sleeps")"

# --- 12. bootstrap records the release, over a stale record ---------------------
# Every up and scan passes through bootstrap with the server UP, a scan against
# an already running server included.
new_case bootstrap-records
status_json UP "$V268" >"$CASE/status"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
printf '%s\n' "$V269" >"$CASE/local/server-version"
run cmd_bootstrap
check_eq "bootstrap: exit 0" 0 "$RC"
check_eq "bootstrap: overwrites the record with the running release" "$V268" "$(record)"

# --- 13. bootstrap records nothing when the release cannot be read -------------
new_case bootstrap-no-version
{ status_json UP "$V268"; printf 'FAIL\n'; } >"$CASE/status"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
run cmd_bootstrap
check_eq "bootstrap, version unreadable: exit 0" 0 "$RC"
check_eq "bootstrap, version unreadable: no record written" absent "$(record)"

# --- 14. up names a SONAR_IMAGE override -----------------------------------------
new_case up-override
status_json UP "$V268" >"$CASE/status"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
CASE_SONAR_IMAGE=sonarqube:community
run cmd_up
check_eq "up with override: exit 0" 0 "$RC"
check "up with override: says it is in effect" "note: SONAR_IMAGE=sonarqube:community overrides" "$OUT"

# --- 14b. scan starts a server that is not UP, names the override, runs the pin
# ROOT is pointed at the case: scan clears ROOT/.scannerwork and mounts ROOT,
# and a plain directory (no .git file) leaves its extra-mounts array empty --
# the expansion /bin/bash 3.2 rejected under set -u. The docker stub stops the
# scan at the scanner.
new_case scan-start
mkdir -p "$CASE/root/.scannerwork"
{ printf 'FAIL\n'; status_json UP "$V268"; } >"$CASE/status"
printf 'running\n' >"$CASE/states"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
CASE_SONAR_IMAGE=sonarqube:community
run eval 'ROOT="$CASE/root"; cmd_scan'
check_eq "scan start: stops at the scanner stub" 1 "$RC"
check "scan start: starts the server" "up -d" "$(composed)"
check "scan start: says the override is in effect" "note: SONAR_IMAGE=sonarqube:community overrides" "$OUT"
check "scan start: no unbound array" "!unbound variable" "$OUT"
check "scan start: runs the pinned scanner" "sonarsource/sonar-scanner-cli:12.1.0.3233_8.0.1" "$(cat "$CASE/docker-run" 2>/dev/null)"
check_eq "scan start: records the release on the way" "$V268" "$(record)"

# --- 15. purge drops the record with the volume -----------------------------------
new_case purge
printf '%s\n' "$V268" >"$CASE/local/server-version"
run cmd_purge
check_eq "purge: exit 0" 0 "$RC"
check "purge: drops the volumes" "down -v" "$(composed)"
check_eq "purge: record removed" absent "$(record)"

# --- 15b. purge names a SONAR_IMAGE override that would recreate the volume ----
new_case purge-override
CASE_SONAR_IMAGE=sonarqube:community
run cmd_purge
check_eq "purge with override: exit 0" 0 "$RC"
check "purge with override: says to unset it" "SONAR_IMAGE=sonarqube:community is set; unset it" "$OUT"

# --- 16. A failed purge keeps the record, since the volume is still there ------
new_case purge-fails
printf '%s\n' "$V268" >"$CASE/local/server-version"
: >"$CASE/compose-fails"
run cmd_purge
check_eq "failed purge: non-zero exit" 1 "$RC"
check_eq "failed purge: record kept" "$V268" "$(record)"

# --- 17a. Both images stay pinned to full release tags ---------------------------
# A floating tag (community, latest, a bare major) is what broke the gate.
check "compose pins a full server release" "sonarqube:" \
  "$(grep -E '^[[:space:]]*image: \$\{SONAR_IMAGE:-sonarqube:[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+-community\}$' "$SCRIPT_DIR/docker-compose.yml")"
check "sonar.sh pins a full scanner release" "sonar-scanner-cli:" \
  "$(grep -E '^SCANNER_IMAGE_DEFAULT="sonarsource/sonar-scanner-cli:[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+_[0-9]+\.[0-9]+\.[0-9]+"$' "$SONAR_SH")"

# --- 17. Executed rather than sourced, sonar.sh still runs main ----------------
new_case executed
OUT="$(SONAR_LOCAL_DIR="$CASE/local" "$BASH" "$SONAR_SH" --help 2>&1)"; RC=$?
check_eq "executed --help: exit 0" 0 "$RC"
check "executed --help: prints the usage" "Usage:" "$OUT"
OUT="$(SONAR_LOCAL_DIR="$CASE/local" "$BASH" "$SONAR_SH" bogus 2>&1)"; RC=$?
check_eq "executed bogus: exit 1" 1 "$RC"
check "executed bogus: names it" "unknown subcommand: bogus" "$OUT"

echo
if [ "$fail" -eq 0 ]; then
  echo "sonar.test.sh: all cases passed"
else
  echo "sonar.test.sh: FAILURES above"
fi
exit "$fail"
