#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors
#
# Headless SonarQube quality gate, driven entirely from the CLI (no browser
# needed) so a coding agent can start -> scan -> fetch findings -> tear down.
#
# Subcommands:
#   up                    start the SonarQube server (docker compose) and
#                         wait for it to come up, then bootstrap credentials.
#   bootstrap             idempotent: change the default admin password and
#                         mint an API token if we don't already have a
#                         working one. Called automatically by `up`/`scan`.
#   coverage              generate the three test-coverage reports the gate
#                         imports (frontend lcov via `npm run test:coverage`,
#                         Go coverprofiles for gateway/backend and
#                         server-agent). Reports land at gitignored paths
#                         (see sonar-project.properties' reportPaths).
#   scan                  run sonar-scanner-cli against the repo, wait for
#                         the compute-engine task, report the quality gate.
#                         `--strict` exits non-zero when the gate fails
#                         (default: report only -- a legacy codebase's first
#                         scan is expected to fail the default gate). Does
#                         NOT regenerate coverage -- stale or absent report
#                         files are tolerated (scanner logs a WARN and skips
#                         the coverage metrics for that run); run `coverage`
#                         (or `gate`) first for a scan that reflects current
#                         coverage.
#   gate                  `coverage` then `scan` -- the full loop a coding
#                         agent should run before trusting new_coverage.
#                         Accepts `--strict` (forwarded to `scan`).
#   findings              fetch open issues + security hotspots as JSON,
#                         print a severity-sorted summary. `--severity MIN`
#                         limits the printed issue listing (BLOCKER >
#                         CRITICAL > MAJOR > MINOR > INFO); the JSON written
#                         to disk is always the full, unfiltered result.
#   down                  docker compose down (volumes/data kept).
#   purge                 docker compose down -v (drops all persisted data).
#
# Credentials (admin password + API token) live in the gitignored
# .sonar-local/ directory at the repo root (dir 0700, files 0600) -- never
# committed. The server is loopback-only (127.0.0.1:9000).
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & constants
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
COMPOSE_PROJECT="op-ai-gateway-sonar"

SONAR_URL="http://127.0.0.1:9000"
PROJECT_KEY="op-ai-gateway"

LOCAL_DIR="$ROOT/.sonar-local"
CREDS_FILE="$LOCAL_DIR/credentials.json"
FINDINGS_FILE="$LOCAL_DIR/findings.json"
HOTSPOTS_FILE="$LOCAL_DIR/hotspots.json"

UP_TIMEOUT="${SONAR_UP_TIMEOUT:-300}"     # seconds to wait for the server to report UP
CE_TIMEOUT="${SONAR_CE_TIMEOUT:-900}"     # seconds to wait for the compute-engine task
POLL_INTERVAL=3

# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------

log() { printf '%s\n' "$*" >&2; }
die() { log "sonar.sh: error: $*"; exit 1; }

require_cmd() {
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || die "required command not found: $c"
  done
}

compose() {
  docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" "$@"
}

# Random password of length $1 (default 24). SonarQube 26.x enforces a
# complexity policy (upper+lower+digit+special, min length); guarantee one
# of each class up front, then pad with random alnum for entropy.
# `head -c` closing early sends SIGPIPE to the `tr` upstream of it; under
# `set -o pipefail` that would otherwise abort the whole script, so that
# failure is masked explicitly -- only head's output matters here.
random_secret() {
  local len="${1:-24}"
  local specials='!@#%^&*-_=+'
  local special="${specials:$((RANDOM % ${#specials})):1}"
  local tail_len=$((len > 4 ? len - 4 : 1))
  local tail
  tail="$({ LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null || true; } | head -c "$tail_len")"
  printf 'Aa1%s%s' "$special" "$tail"
}

ensure_local_dir() {
  mkdir -p "$LOCAL_DIR"
  chmod 700 "$LOCAL_DIR"
}

server_status() {
  curl -fsS "$SONAR_URL/api/system/status" 2>/dev/null | jq -r '.status // "UNREACHABLE"' 2>/dev/null || echo "UNREACHABLE"
}

# ---------------------------------------------------------------------------
# up / wait-for-up
# ---------------------------------------------------------------------------

wait_for_up() {
  local elapsed=0 status
  log "Waiting for SonarQube to report UP (timeout ${UP_TIMEOUT}s) ..."
  while [ "$elapsed" -lt "$UP_TIMEOUT" ]; do
    status="$(server_status)"
    case "$status" in
      UP) log "SonarQube is UP (after ${elapsed}s)."; return 0 ;;
      DOWN|"" ) : ;; # transient during first-time ES bootstrap; keep polling
      *) log "  ... status=${status} (${elapsed}s elapsed)" ;;
    esac
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
    if [ $((elapsed % 15)) -eq 0 ]; then
      log "  ... still waiting (status=${status}, ${elapsed}s elapsed)"
    fi
  done
  log "SonarQube did not reach UP within ${UP_TIMEOUT}s. Recent container logs:"
  compose logs --tail=60 sonarqube >&2 || true
  die "timed out waiting for SonarQube"
}

cmd_up() {
  require_cmd docker curl jq
  log "Starting SonarQube (docker compose) ..."
  compose up -d
  wait_for_up
  cmd_bootstrap
  log ""
  log "SonarQube ready:  ${SONAR_URL}"
  log "Credentials:      ${CREDS_FILE}"
}

# ---------------------------------------------------------------------------
# bootstrap (idempotent)
# ---------------------------------------------------------------------------

token_is_valid() {
  local token="$1"
  [ -n "$token" ] || return 1
  local resp
  resp="$(curl -fsS -u "${token}:" "$SONAR_URL/api/authentication/validate" 2>/dev/null)" || return 1
  [ "$(echo "$resp" | jq -r '.valid // false')" = "true" ]
}

password_is_valid() {
  local pass="$1"
  [ -n "$pass" ] || return 1
  local resp
  resp="$(curl -fsS -u "admin:${pass}" "$SONAR_URL/api/authentication/validate" 2>/dev/null)" || return 1
  [ "$(echo "$resp" | jq -r '.valid // false')" = "true" ]
}

mint_token() {
  local password="$1" resp
  # Revoke any stale token of the same name first (no-op if it doesn't exist).
  curl -fsS -u "admin:${password}" -X POST "$SONAR_URL/api/user_tokens/revoke" \
    --data-urlencode "name=agent-token" >/dev/null 2>&1 || true
  resp="$(curl -fsS -u "admin:${password}" -X POST "$SONAR_URL/api/user_tokens/generate" \
    --data-urlencode "name=agent-token")" || die "failed to generate API token"
  echo "$resp" | jq -r '.token'
}

write_creds() {
  local password="$1" token="$2"
  ensure_local_dir
  local tmp="$LOCAL_DIR/.credentials.json.tmp"
  jq -n --arg p "$password" --arg t "$token" \
    '{admin_password: $p, token: $t, login: "admin"}' >"$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CREDS_FILE"
}

cmd_bootstrap() {
  require_cmd curl jq
  [ "$(server_status)" = "UP" ] || die "SonarQube is not UP yet; run 'sonar.sh up' first"

  ensure_local_dir
  local stored_password="" stored_token=""
  if [ -f "$CREDS_FILE" ]; then
    stored_password="$(jq -r '.admin_password // empty' "$CREDS_FILE" 2>/dev/null || true)"
    stored_token="$(jq -r '.token // empty' "$CREDS_FILE" 2>/dev/null || true)"
  fi

  # Case 1: we already have a working token (the common re-run path). Done.
  if token_is_valid "$stored_token"; then
    log "bootstrap: existing token in ${CREDS_FILE} is still valid; nothing to do."
    return 0
  fi

  # Case 2: token is gone/invalid but the stored admin password still works
  # (e.g. a token was revoked server-side, or credentials.json was rewritten
  # after a partial failure). Just mint a fresh token.
  if password_is_valid "$stored_password"; then
    log "bootstrap: stored admin password still valid; minting a new token."
    local token
    token="$(mint_token "$stored_password")"
    write_creds "$stored_password" "$token"
    log "bootstrap: new token stored."
    return 0
  fi

  # Case 3: fresh volume (or file-exists-but-volume-fresh) -- SonarQube still
  # has the factory default admin/admin and is waiting for the forced
  # first-login password change.
  local new_password
  new_password="$(random_secret 24)"
  local http_code body_file
  body_file="$(mktemp)"
  http_code="$(curl -s -o "$body_file" -w '%{http_code}' -u "admin:admin" \
    -X POST "$SONAR_URL/api/users/change_password" \
    --data-urlencode "login=admin" \
    --data-urlencode "previousPassword=admin" \
    --data-urlencode "password=${new_password}")"
  if [ "$http_code" = "204" ]; then
    rm -f "$body_file"
    log "bootstrap: default admin password changed."
    local token
    token="$(mint_token "$new_password")"
    write_creds "$new_password" "$token"
    log "bootstrap: new token stored."
    return 0
  fi

  # Case 4: genuinely locked out -- the volume persists with a non-default
  # admin password we do not know (credentials.json lost/stale and default
  # admin/admin already changed in a prior run). We cannot recover the
  # password without either the file or the DB; fail with an actionable
  # message instead of hanging or silently corrupting state.
  log "bootstrap: could not authenticate as admin. Response from change_password:"
  cat "$body_file" >&2 || true
  rm -f "$body_file"
  die "admin credentials are unknown (default admin/admin rejected, no valid stored password). The SonarQube data volume already has a custom admin password from a previous run that ${CREDS_FILE} no longer has a record of. Reset with: '$0 purge && $0 up' (drops all analysis history), or restore a working .sonar-local/credentials.json backup."
}

# ---------------------------------------------------------------------------
# coverage
# ---------------------------------------------------------------------------

# Run one coverage-generating step, timed, with clear start/end progress
# output (each step takes minutes; silent shells look hung).
run_timed() {
  local label="$1" report_rel="$2"
  shift 2
  log ""
  log "== coverage: ${label} =="
  local start_ts end_ts
  start_ts="$(date +%s)"
  ( "$@" )
  end_ts="$(date +%s)"
  log "== coverage: ${label} done in $((end_ts - start_ts))s -> ${report_rel} =="
}

cmd_coverage() {
  require_cmd npm go
  [ "$#" -eq 0 ] || die "coverage: unknown argument: $1"

  run_timed "frontend (vitest --coverage, lcov)" \
    "gateway/frontend/coverage/lcov.info" \
    bash -c "cd '$ROOT/gateway/frontend' && npm run test:coverage"

  run_timed "gateway/backend (go test -coverprofile)" \
    "gateway/backend/coverage.out" \
    bash -c "cd '$ROOT/gateway/backend' && go test ./... -coverprofile=coverage.out -covermode=atomic -timeout=25m"

  run_timed "server-agent (go test -coverprofile)" \
    "server-agent/coverage.out" \
    bash -c "cd '$ROOT/server-agent' && go test ./... -coverprofile=coverage.out -covermode=atomic -timeout=25m"

  log ""
  log "coverage: all reports generated."
}

# ---------------------------------------------------------------------------
# scan
# ---------------------------------------------------------------------------

current_token() {
  [ -f "$CREDS_FILE" ] || die "no credentials yet; run 'sonar.sh up' first"
  jq -r '.token // empty' "$CREDS_FILE"
}

cmd_scan() {
  require_cmd docker curl jq
  local strict=0
  for arg in "$@"; do
    case "$arg" in
      --strict) strict=1 ;;
      *) die "scan: unknown argument: $arg" ;;
    esac
  done

  if [ "$(server_status)" != "UP" ]; then
    log "SonarQube is not running yet; starting it ..."
    compose up -d
    wait_for_up
  fi
  cmd_bootstrap
  local token
  token="$(current_token)"

  rm -rf "$ROOT/.scannerwork"

  # ROOT is a linked git worktree: its .git is a *file* with an absolute
  # "gitdir:" pointer into the main repo's .git/worktrees/<name>, which lives
  # outside the mounted tree. The scanner's git SCM provider (JGit) needs
  # that absolute path to resolve inside the container too, so bind-mount
  # the common .git dir read-only at the identical path. Not needed (and
  # skipped) for a plain, non-worktree checkout where .git is already a real
  # directory inside ROOT.
  local extra_mounts=()
  if [ -f "$ROOT/.git" ]; then
    local git_common_dir
    git_common_dir="$(git -C "$ROOT" rev-parse --git-common-dir 2>/dev/null || true)"
    if [ -n "$git_common_dir" ] && [ -d "$git_common_dir" ]; then
      extra_mounts+=(-v "${git_common_dir}:${git_common_dir}:ro")
    fi
  fi

  log "Running sonar-scanner-cli against ${ROOT} ..."
  local start_ts end_ts
  start_ts="$(date +%s)"
  # The image's default sonar.working.directory lives under /tmp inside the
  # container, which vanishes with --rm before we can read report-task.txt.
  # SCANNER_WORKDIR_PATH (supported by the image's entrypoint) points it at
  # the mounted tree instead so the report survives the container's exit.
  docker run --rm \
    --network sonar-net \
    -e SONAR_HOST_URL="http://sonarqube:9000" \
    -e SONAR_TOKEN="${token}" \
    -e SONAR_SCANNER_OPTS="${SONAR_SCANNER_OPTS:--Xmx2048m}" \
    -e SCANNER_WORKDIR_PATH="/usr/src/.scannerwork" \
    -v "${ROOT}:/usr/src" \
    "${extra_mounts[@]}" \
    -w /usr/src \
    sonarsource/sonar-scanner-cli
  end_ts="$(date +%s)"
  log "Scanner finished in $((end_ts - start_ts))s."

  local report_file="$ROOT/.scannerwork/report-task.txt"
  [ -f "$report_file" ] || die "scanner did not produce ${report_file}"

  local ce_task_url task_id
  ce_task_url="$(grep '^ceTaskUrl=' "$report_file" | cut -d= -f2- || true)"
  task_id="$(printf '%s' "$ce_task_url" | sed -n 's/.*[?&]id=\([^&]*\).*/\1/p')"
  [ -n "$task_id" ] || die "could not parse compute-engine task id from ${report_file}"

  log "Waiting for compute-engine task ${task_id} (timeout ${CE_TIMEOUT}s) ..."
  local elapsed=0 ce_status="" resp
  while [ "$elapsed" -lt "$CE_TIMEOUT" ]; do
    resp="$(curl -fsS -u "${token}:" "${SONAR_URL}/api/ce/task?id=${task_id}")"
    ce_status="$(echo "$resp" | jq -r '.task.status')"
    case "$ce_status" in
      SUCCESS|FAILED|CANCELED) break ;;
    esac
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
    if [ $((elapsed % 15)) -eq 0 ]; then
      log "  ... status=${ce_status} (${elapsed}s elapsed)"
    fi
  done

  [ "$ce_status" = "SUCCESS" ] || die "compute-engine task ended with status=${ce_status}"

  local analysis_id
  analysis_id="$(echo "$resp" | jq -r '.task.analysisId')"
  log "Compute-engine task SUCCESS (analysisId=${analysis_id})."

  local gate_resp gate_status
  gate_resp="$(curl -fsS -u "${token}:" "${SONAR_URL}/api/qualitygates/project_status?analysisId=${analysis_id}")"
  gate_status="$(echo "$gate_resp" | jq -r '.projectStatus.status')"

  echo ""
  if [ "$gate_status" = "OK" ]; then
    echo "Quality gate: PASS"
  else
    echo "Quality gate: FAIL (status=${gate_status})"
  fi
  echo "$gate_resp" | jq -r '
    .projectStatus.conditions[]? |
    "  [\(.status)] \(.metricKey) \(.comparator) \(.errorThreshold // "-") (actual: \(.actualValue // "-"))"
  '
  echo ""
  echo "Dashboard: ${SONAR_URL}/dashboard?id=${PROJECT_KEY}"

  if [ "$strict" -eq 1 ] && [ "$gate_status" != "OK" ]; then
    die "quality gate failed (--strict)"
  fi
  return 0
}

# ---------------------------------------------------------------------------
# gate = coverage + scan
# ---------------------------------------------------------------------------

cmd_gate() {
  cmd_coverage
  cmd_scan "$@"
}

# ---------------------------------------------------------------------------
# findings
# ---------------------------------------------------------------------------

sev_rank() {
  case "$1" in
    BLOCKER) echo 0 ;;
    CRITICAL) echo 1 ;;
    MAJOR) echo 2 ;;
    MINOR) echo 3 ;;
    INFO) echo 4 ;;
    *) echo 9 ;;
  esac
}

fetch_paginated() {
  # $1 = output file, $2 = base URL (no p= param), $3 = jq field holding the array (issues|hotspots)
  local out="$1" base_url="$2" field="$3" token
  token="$(current_token)"
  local tmpdir page=1 resp n
  tmpdir="$(mktemp -d)"
  while :; do
    resp="$(curl -fsS -u "${token}:" "${base_url}&p=${page}")"
    echo "$resp" | jq ".${field}" >"${tmpdir}/page_$(printf '%05d' "$page").json"
    n="$(echo "$resp" | jq ".${field} | length")"
    if [ "$n" -lt 500 ]; then break; fi
    page=$((page + 1))
    if [ "$page" -gt 200 ]; then
      log "findings: aborting pagination for ${field} after 200 pages"
      break
    fi
  done
  jq -s '[.[][]]' "${tmpdir}"/page_*.json >"$out"
  rm -rf "$tmpdir"
}

cmd_findings() {
  require_cmd curl jq
  local min_sev=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --severity) min_sev="${2:-}"; shift 2 ;;
      *) die "findings: unknown argument: $1" ;;
    esac
  done
  [ "$(server_status)" = "UP" ] || die "SonarQube is not UP; run 'sonar.sh up' first"

  ensure_local_dir
  log "Fetching issues ..."
  fetch_paginated "$FINDINGS_FILE" \
    "${SONAR_URL}/api/issues/search?componentKeys=${PROJECT_KEY}&resolved=false&ps=500" \
    "issues"
  log "Fetching security hotspots ..."
  fetch_paginated "$HOTSPOTS_FILE" \
    "${SONAR_URL}/api/hotspots/search?projectKey=${PROJECT_KEY}&ps=500" \
    "hotspots"
  chmod 600 "$FINDINGS_FILE" "$HOTSPOTS_FILE"

  local total_issues total_hotspots
  total_issues="$(jq 'length' "$FINDINGS_FILE")"
  total_hotspots="$(jq 'length' "$HOTSPOTS_FILE")"

  echo ""
  echo "Issues: ${total_issues} total"
  jq -r 'group_by(.severity) | map({severity: .[0].severity, n: length})[] | "  \(.severity): \(.n)"' "$FINDINGS_FILE" \
    | sort -t: -k1,1
  echo "By type:"
  jq -r 'group_by(.type) | map({type: .[0].type, n: length})[] | "  \(.type): \(.n)"' "$FINDINGS_FILE"
  echo ""
  echo "Hotspots: ${total_hotspots} total"
  jq -r 'group_by(.vulnerabilityProbability) | map({p: .[0].vulnerabilityProbability, n: length})[] | "  \(.p): \(.n)"' "$HOTSPOTS_FILE"
  echo ""

  local min_rank=9
  if [ -n "$min_sev" ]; then
    min_rank="$(sev_rank "$min_sev")"
    echo "Issues (severity >= ${min_sev}):"
  else
    echo "Issues:"
  fi

  jq -r --argjson minrank "$min_rank" '
    def rank: {"BLOCKER":0,"CRITICAL":1,"MAJOR":2,"MINOR":3,"INFO":4}[.severity] // 9;
    map(select(rank <= $minrank))
    | sort_by(rank)
    | .[]
    | "\(.severity) \(.type) \(.rule) \(.component | sub("^[^:]+:";"")):\(.line // (.textRange.startLine // "-")) \(.message)"
  ' "$FINDINGS_FILE"

  echo ""
  echo "Hotspots:"
  jq -r '
    .[]
    | "HOTSPOT \(.vulnerabilityProbability) \(.ruleKey // .rule // "-") \(.component | sub("^[^:]+:";"")):\(.line // "-") \(.message)"
  ' "$HOTSPOTS_FILE"

  echo ""
  echo "Raw JSON: ${FINDINGS_FILE} (${total_issues} issues), ${HOTSPOTS_FILE} (${total_hotspots} hotspots)"
}

# ---------------------------------------------------------------------------
# down / purge
# ---------------------------------------------------------------------------

cmd_down() {
  require_cmd docker
  compose down
}

cmd_purge() {
  require_cmd docker
  compose down -v
}

# ---------------------------------------------------------------------------
# dispatch
# ---------------------------------------------------------------------------

usage() {
  cat <<EOF
Usage: $0 <up|bootstrap|coverage|scan|gate|findings|down|purge> [options]

  up                  start SonarQube, wait for it, bootstrap credentials
  bootstrap           idempotently ensure a working admin token exists
  coverage            generate frontend lcov + both Go coverprofiles
  scan [--strict]     run sonar-scanner-cli, report the quality gate
                      (--strict: exit non-zero if the gate fails). Uses
                      whatever coverage reports already exist on disk;
                      run 'coverage' or 'gate' first to refresh them.
  gate [--strict]     coverage, then scan (the full agent loop)
  findings [--severity MIN]
                      fetch issues + hotspots, print a summary
  down                docker compose down (keep volumes)
  purge               docker compose down -v (drop all data)
EOF
}

main() {
  local cmd="${1:-}"
  [ -n "$cmd" ] || { usage; exit 1; }
  shift
  case "$cmd" in
    up) cmd_up "$@" ;;
    bootstrap) cmd_bootstrap "$@" ;;
    coverage) cmd_coverage "$@" ;;
    scan) cmd_scan "$@" ;;
    gate) cmd_gate "$@" ;;
    findings) cmd_findings "$@" ;;
    down) cmd_down "$@" ;;
    purge) cmd_purge "$@" ;;
    -h|--help) usage ;;
    *) log "unknown subcommand: $cmd"; usage; exit 1 ;;
  esac
}

main "$@"
