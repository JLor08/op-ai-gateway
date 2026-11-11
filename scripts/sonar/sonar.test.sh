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
#
# The scanner-mode cases (18-26) also stub `uname` and scanner_on_path, the
# one-line PATH lookup for sonar-scanner (so a scanner installed on the machine
# running the tests cannot leak in), and run a fake native scanner: a script
# that records its arguments and environment and writes the report-task.txt a
# real one leaves in its working directory.
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
#   docker-arch  what `docker info --format {{.Architecture}}` prints (absent:
#                docker does not answer)
#   uname-m      what `uname -m` prints (default x86_64)
#   on-path      the sonar-scanner a PATH lookup finds (absent: none)
#   scanner-version  what the fake native scanner prints for -v
# and record what sonar.sh did with the outside world:
#   compose      every compose invocation
#   docker-run   the arguments of `docker run` (the scanner), one per line
#   native-run   the arguments of the fake native scanner, one per line
#   native-env   its working directory and the scanner variables it saw
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
    */api/ce/task\?id=*) printf '{"task":{"status":"SUCCESS","analysisId":"A1"}}\n' ;;
    */api/qualitygates/project_status\?*) printf '{"projectStatus":{"status":"OK","conditions":[]}}\n' ;;
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
  elif [ "$*" = 'info --format {{.Architecture}}' ] && [ -f "$CASE/docker-arch" ]; then
    cat "$CASE/docker-arch"
  else
    return 1
  fi
}
sleep() { echo slept >>"$CASE/sleeps"; }
require_cmd() { :; } # nothing external is needed offline
uname() {
  if [ "${1:-}" = -m ]; then
    if [ -f "$CASE/uname-m" ]; then cat "$CASE/uname-m"; else echo x86_64; fi
  else
    command uname "$@"
  fi
}
scanner_on_path() {
  if [ -f "$CASE/on-path" ]; then cat "$CASE/on-path"; fi
}
EOF

# The fake native scanner: `-v` prints $CASE/scanner-version; a scan records
# its arguments and environment and writes report-task.txt into the
# sonar.working.directory it was given, as a real scanner does.
cat >"$TMP/fake-scanner" <<'EOF'
#!/bin/sh
if [ "${1:-}" = -v ]; then
  cat "$CASE/scanner-version" 2>/dev/null
  exit 0
fi
printf '%s\n' "$@" >"$CASE/native-run"
{
  echo "pwd=$(pwd)"
  echo "SONAR_HOST_URL=${SONAR_HOST_URL-<unset>}"
  echo "SONAR_TOKEN=${SONAR_TOKEN-<unset>}"
  echo "SONAR_SCANNER_OPTS=${SONAR_SCANNER_OPTS-<unset>}"
  echo "SONAR_SCANNER_JAVA_OPTS=${SONAR_SCANNER_JAVA_OPTS-<unset>}"
} >"$CASE/native-env"
wd=""
for a in "$@"; do
  case "$a" in -Dsonar.working.directory=*) wd="${a#*=}" ;; esac
done
[ -n "$wd" ] || exit 3
mkdir -p "$wd"
printf 'ceTaskUrl=http://127.0.0.1:9000/api/ce/task?id=T1\n' >"$wd/report-task.txt"
EOF
chmod +x "$TMP/fake-scanner"

# new_case <name>: a fresh case directory, with a fresh SONAR_LOCAL_DIR in it.
new_case() {
  CASE="$TMP/$1"
  mkdir -p "$CASE/local"
  CASE_SONAR_IMAGE=""
  CASE_MODE=""
  CASE_BIN=""
  CASE_OPTS=""
  CASE_JAVA_OPTS=""
  CASE_EXTRA=""
}
# run <function> [args...]: run one sonar.sh function in its own bash process
# against the current case's stubs. Sets OUT (stdout+stderr) and RC. The case
# runs under the bash running this file ($BASH), not whichever bash is first on
# PATH, so `/bin/bash scripts/sonar/sonar.test.sh` really tests bash 3.2.
run() {
  OUT="$(CASE="$CASE" SONAR_LOCAL_DIR="$CASE/local" SONAR_UP_TIMEOUT="${UP_TIMEOUT:-300}" \
    SONAR_IMAGE="$CASE_SONAR_IMAGE" SONAR_SCANNER_IMAGE="" \
    SONAR_SCANNER_MODE="$CASE_MODE" SONAR_SCANNER_BIN="$CASE_BIN" \
    SONAR_SCANNER_OPTS="$CASE_OPTS" SONAR_SCANNER_JAVA_OPTS="$CASE_JAVA_OPTS" \
    SONAR_SCANNER_EXTRA_ARGS="$CASE_EXTRA" \
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
check "scan start: auto without a native scanner picks docker" "Scanner mode: docker (auto: no sonar-scanner" "$OUT"
# The docker command, whole: as it always was, plus the engine's default heap.
check_eq "scan start: the docker command" "$(printf '%s\n' run --rm --network sonar-net \
  -e SONAR_HOST_URL=http://sonarqube:9000 -e SONAR_TOKEN=t -e SONAR_SCANNER_OPTS=-Xmx2048m \
  -e SONAR_SCANNER_JAVA_OPTS=-Xmx5g \
  -e SCANNER_WORKDIR_PATH=/usr/src/.scannerwork -v "$CASE/root:/usr/src" -w /usr/src \
  sonarsource/sonar-scanner-cli:12.1.0.3233_8.0.1 -Dsonar.projectVersion=unknown)" \
  "$(cat "$CASE/docker-run" 2>/dev/null)"
check "scan start: prints the docker default engine heap" "Engine JVM options: -Xmx5g (the docker mode default" "$OUT"
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

# --- 18. Mode selection --------------------------------------------------------
# select_scanner_mode prints the mode and logs why; OUT holds both, and the mode
# is its last line.
mode_of() { printf '%s\n' "$OUT" | tail -n 1; }

new_case mode-explicit-docker
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'aarch64\n' >"$CASE/docker-arch"
CASE_MODE=docker
run select_scanner_mode
check_eq "explicit docker: exit 0" 0 "$RC"
check_eq "explicit docker: wins over an arm64 host with a scanner" docker "$(mode_of)"
check "explicit docker: says it was asked for" "Scanner mode: docker (SONAR_SCANNER_MODE=docker)" "$OUT"

new_case mode-auto-arm-docker-host
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'aarch64\n' >"$CASE/docker-arch"
run select_scanner_mode
check_eq "auto, arm64 docker host, scanner on PATH: native" native "$(mode_of)"
check "auto, arm64 docker host: says why" "Scanner mode: native (auto: $TMP/fake-scanner found; docker host aarch64" "$OUT"

new_case mode-auto-arm-machine
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'arm64\n' >"$CASE/uname-m"
run select_scanner_mode
check_eq "auto, docker silent, arm64 machine: native" native "$(mode_of)"
check "auto, docker silent: names the unknown docker host" "docker host unknown, this machine arm64" "$OUT"

new_case mode-auto-amd64
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'x86_64\n' >"$CASE/docker-arch"
run select_scanner_mode
check_eq "auto, amd64 everywhere: docker" docker "$(mode_of)"

new_case mode-auto-no-scanner
printf 'aarch64\n' >"$CASE/docker-arch"
printf 'arm64\n' >"$CASE/uname-m"
run select_scanner_mode
check_eq "auto, arm64 without a scanner: docker" docker "$(mode_of)"

new_case mode-auto-bin
printf 'arm64\n' >"$CASE/uname-m"
CASE_BIN="$TMP/fake-scanner"
run select_scanner_mode
check_eq "auto, SONAR_SCANNER_BIN counts as available: native" native "$(mode_of)"

new_case mode-bogus
CASE_MODE=podman
run select_scanner_mode
check_eq "unknown mode: exit 1" 1 "$RC"
check "unknown mode: names the choices" "SONAR_SCANNER_MODE must be docker, native or auto (got 'podman')" "$OUT"

# --- 19. Version comparison, and the floor ---------------------------------------
for pair in "8.0.1 8.0.1 0" "8.4.0.16527 8.0.1 0" "10.0 8.0.1 0" "8.0.0.9 8.0.1 1" "7.9.9 8.0.1 1" "8 8.0.1 1" "08.1 8.0.1 0"; do
  read -r have want rc <<<"$pair"
  new_case "version-$have"
  run version_at_least "$have" "$want"
  check_eq "version_at_least $have $want" "$rc" "$RC"
done
new_case min-version
run eval 'printf "%s\n" "$NATIVE_SCANNER_MIN_VERSION"'
check_eq "the native floor is the CLI release the pinned image bundles" \
  "$(sed -n 's/^SCANNER_IMAGE_DEFAULT="[^"]*_\([0-9.]*\)"$/\1/p' "$SONAR_SH")" "$OUT"

# --- 20. native without a scanner fails before anything starts -------------------
new_case native-missing
printf 'FAIL\n' >"$CASE/status"
CASE_MODE=native
run cmd_scan
check_eq "native, no scanner: exit 1" 1 "$RC"
check "native, no scanner: says how to install one" "brew install sonar-scanner" "$OUT"
check_eq "native, no scanner: starts no server" "" "$(composed)"

# --- 21. A native scanner older than the floor is refused ------------------------
# The two -v formats: CLI 6 and later, and before.
for old in "07:43:59.640 INFO  SonarScanner CLI 7.2.0.5079" "INFO: SonarScanner 4.8.0.2856"; do
  new_case native-old
  printf 'FAIL\n' >"$CASE/status"
  printf '%s\n' "$old" >"$CASE/scanner-version"
  CASE_MODE=native
  CASE_BIN="$TMP/fake-scanner"
  run cmd_scan
  check_eq "native, too old ($old): exit 1" 1 "$RC"
  check "native, too old ($old): names both versions" "is older than 8.0.1" "$OUT"
  check "native, too old ($old): says how to upgrade" "brew upgrade sonar-scanner" "$OUT"
  check_eq "native, too old ($old): starts no server" "" "$(composed)"
done

# --- 22. A version that cannot be read is refused --------------------------------
new_case native-unreadable
printf 'Could not find java executable\n' >"$CASE/scanner-version"
CASE_MODE=native
CASE_BIN="$TMP/fake-scanner"
run cmd_scan
check_eq "native, version unreadable: exit 1" 1 "$RC"
check "native, version unreadable: says so" "could not read a SonarScanner version" "$OUT"

# --- 23. A SONAR_SCANNER_BIN that is no executable file -------------------------
new_case native-bin-bad
CASE_MODE=native
CASE_BIN="$CASE/nope"
run cmd_scan
check_eq "native, bad SONAR_SCANNER_BIN: exit 1" 1 "$RC"
check "native, bad SONAR_SCANNER_BIN: names it" "$CASE/nope is not an executable file" "$OUT"

# --- 24. A native scan: the docker path's properties, with the host's paths -----
new_case native-scan
mkdir -p "$CASE/root/.scannerwork"
: >"$CASE/root/.scannerwork/stale"
status_json UP "$V268" >"$CASE/status"
printf '{"admin_password":"p","token":"tok-123","login":"admin"}\n' >"$CASE/local/credentials.json"
printf '07:43:59.640 INFO  SonarScanner CLI 8.4.0.16527\n' >"$CASE/scanner-version"
CASE_MODE=native
CASE_BIN="$TMP/fake-scanner"
CASE_JAVA_OPTS="-Xmx12g -XX:+UseG1GC"
CASE_EXTRA="-Dsonar.javascript.node.maxspace=4608  -Dsonar.go.duration.statistics=true"
run eval 'ROOT="$CASE/root"; cmd_scan'
check_eq "native scan: exit 0" 0 "$RC"
check "native scan: names the scanner and its version" "Native scanner: $TMP/fake-scanner (SonarScanner CLI 8.4.0.16527; minimum 8.0.1)" "$OUT"
check_eq "native scan: the scanner's arguments" "$(printf '%s\n' \
  -Dsonar.host.url=http://127.0.0.1:9000 \
  "-Dsonar.projectBaseDir=$CASE/root" \
  "-Dsonar.working.directory=$CASE/root/.scannerwork" \
  -Dsonar.projectVersion=unknown \
  -Dsonar.javascript.node.maxspace=4608 \
  -Dsonar.go.duration.statistics=true)" "$(cat "$CASE/native-run" 2>/dev/null)"
check_eq "native scan: its environment" "$(printf '%s\n' \
  "pwd=$CASE/root" \
  SONAR_HOST_URL=http://127.0.0.1:9000 \
  SONAR_TOKEN=tok-123 \
  SONAR_SCANNER_OPTS=-Xmx2048m \
  "SONAR_SCANNER_JAVA_OPTS=-Xmx12g -XX:+UseG1GC")" "$(cat "$CASE/native-env" 2>/dev/null)"
check "native scan: the token is not on the command line" "!tok-123" "$(cat "$CASE/native-run" 2>/dev/null)"
check "native scan: clears the previous working directory" "!stale" "$(ls "$CASE/root/.scannerwork")"
check "native scan: reads the report and reaches the gate" "Quality gate: PASS" "$OUT"
check_eq "native scan: runs no container" absent "$(cat "$CASE/docker-run" 2>/dev/null || echo absent)"
check "native scan: no JVM-options note when the engine heap is set" "!sizes only the scanner's launcher" "$OUT"
check "native scan: prints the engine options it was given" "Engine JVM options: -Xmx12g -XX:+UseG1GC (SONAR_SCANNER_JAVA_OPTS)." "$OUT"

# --- 25. auto picks the scanner on PATH on an arm64 host and runs it -------------
new_case auto-native-scan
mkdir -p "$CASE/root"
status_json UP "$V268" >"$CASE/status"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
printf 'SonarScanner CLI 8.0.1.6346\n' >"$CASE/scanner-version"
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'aarch64\n' >"$CASE/docker-arch"
CASE_OPTS="-Xmx8g"
run eval 'ROOT="$CASE/root"; cmd_scan'
check_eq "auto native scan: exit 0" 0 "$RC"
check "auto native scan: ran the scanner on PATH" "-Dsonar.host.url=http://127.0.0.1:9000" "$(cat "$CASE/native-run" 2>/dev/null)"
check "auto native scan: passes SONAR_SCANNER_OPTS" "SONAR_SCANNER_OPTS=-Xmx8g" "$(cat "$CASE/native-env" 2>/dev/null)"
check "auto native scan: gives the engine the native default heap" "SONAR_SCANNER_JAVA_OPTS=-Xmx6g" "$(cat "$CASE/native-env" 2>/dev/null)"
check "auto native scan: prints the native default engine heap" "Engine JVM options: -Xmx6g (the native mode default" "$OUT"
check "auto native scan: says a launcher heap does not size the analysis" "SONAR_SCANNER_OPTS sizes only the scanner's launcher JVM" "$OUT"

# --- 26. docker mode carries the engine options and the extra arguments ---------
new_case docker-extras
mkdir -p "$CASE/root"
status_json UP "$V268" >"$CASE/status"
printf '{"admin_password":"p","token":"t","login":"admin"}\n' >"$CASE/local/credentials.json"
printf '%s\n' "$TMP/fake-scanner" >"$CASE/on-path"
printf 'aarch64\n' >"$CASE/docker-arch"
CASE_MODE=docker
CASE_OPTS="-Xmx3g"
CASE_JAVA_OPTS="-Xmx8g -XX:+UseG1GC"
CASE_EXTRA="-Dsonar.javascript.node.maxspace=4608"
run eval 'ROOT="$CASE/root"; cmd_scan'
check_eq "docker extras: the docker command" "$(printf '%s\n' run --rm --network sonar-net \
  -e SONAR_HOST_URL=http://sonarqube:9000 -e SONAR_TOKEN=t -e SONAR_SCANNER_OPTS=-Xmx3g \
  -e "SONAR_SCANNER_JAVA_OPTS=-Xmx8g -XX:+UseG1GC" \
  -e SCANNER_WORKDIR_PATH=/usr/src/.scannerwork -v "$CASE/root:/usr/src" -w /usr/src \
  sonarsource/sonar-scanner-cli:12.1.0.3233_8.0.1 -Dsonar.projectVersion=unknown \
  -Dsonar.javascript.node.maxspace=4608)" "$(cat "$CASE/docker-run" 2>/dev/null)"
check_eq "docker extras: the native scanner did not run" absent "$(cat "$CASE/native-run" 2>/dev/null || echo absent)"

# --- 27. The engine's JVM options: a set value wins, else the mode's default -----
for c in "native||-Xmx6g" "docker||-Xmx5g" "native|-Xmx3g -XX:+UseG1GC|-Xmx3g -XX:+UseG1GC" "docker|-Xmx9g|-Xmx9g"; do
  IFS='|' read -r m set want <<<"$c"
  new_case engine-opts
  CASE_JAVA_OPTS="$set"
  run engine_java_opts "$m"
  check_eq "engine options, $m mode, SONAR_SCANNER_JAVA_OPTS='$set'" "$want" "$OUT"
done

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
