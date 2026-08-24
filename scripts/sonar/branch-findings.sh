#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Answers one question before a pull request: "did MY branch introduce any of
# the findings SonarQube reports?"
#
# Why this exists instead of Sonar's own new-code period: the local server is
# Community Build, which refuses `sonar.branch.name` ("Developer Edition or
# above is required"), so it cannot compare a branch against main. The
# alternative server-side definitions all have a hole for this purpose --
# NUMBER_OF_DAYS silently stops counting a finding once the branch outlives the
# window (a false green, and after a rebase it also counts other people's recent
# commits as yours), and PREVIOUS_VERSION needs an analysis of the previous
# version in the local database, which a freshly bootstrapped server does not
# have. So the branch comparison is computed from git, which every developer has
# identically from the repository: no server state, nothing to share, and no
# dependence on how long the branch has been alive.
#
# It is post-processing only -- it never talks to SonarQube. Run a scan and
# export the findings first:
#   ./scripts/sonar/sonar.sh gate      # or: scan
#   ./scripts/sonar/sonar.sh findings
#   ./scripts/sonar/branch-findings.sh
#
# Exit codes: 0 = the branch owns no findings, 1 = it owns at least one (so this
# is usable as a pre-PR check), 2 = it could not run.
#
# Scope, stated plainly: this attributes findings by LINE. A finding your change
# causes elsewhere without touching that line (you changed a signature, a caller
# elsewhere now smells) is not attributed here, and coverage is not considered at
# all -- for those, read the quality gate itself.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"
BASE=""
FINDINGS=""

usage() {
  cat <<EOF
Usage: $0 [--base <ref>] [--findings <file>] [--repo <dir>]

  --base <ref>       branch/ref to compare against (default: origin/main when
                     that ref exists, else main)
  --findings <file>  Sonar issue export (default: <repo>/.sonar-local/findings.json)
  --repo <dir>       repository to inspect (default: this script's repository)
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) BASE="${2:?--base needs a ref}"; shift 2 ;;
    --findings) FINDINGS="${2:?--findings needs a file}"; shift 2 ;;
    --repo) REPO="${2:?--repo needs a directory}"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "error: jq is required" >&2; exit 2; }
cd "$REPO" || { echo "error: cannot enter $REPO" >&2; exit 2; }
[ -n "$FINDINGS" ] || FINDINGS="$REPO/.sonar-local/findings.json"
[ -f "$FINDINGS" ] || {
  echo "error: no findings export at $FINDINGS -- run 'sonar.sh findings' first" >&2
  exit 2
}

# Default to origin/main, not the local main: in a worktree you typically only
# fetch origin/main and branch off it, so the local ref can sit far behind --
# and a stale base makes the merge base ancient, which attributes half the
# repository to the branch and reads like a working report.
if [ -z "$BASE" ]; then
  if git rev-parse --verify --quiet refs/remotes/origin/main >/dev/null; then
    BASE="origin/main"
  else
    BASE="main"
  fi
fi

MERGE_BASE="$(git merge-base "$BASE" HEAD 2>/dev/null)" || {
  echo "error: no merge base between '$BASE' and HEAD" >&2
  exit 2
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Changed lines, as "<path>\t<firstLine>\t<lastLine>" (or "<path>\tALL"):
#
#  - Diffing the MERGE BASE against the working tree, not against the tip of
#    $BASE, is what makes this independent of the branch's age and immune to a
#    rebase: commits that landed on $BASE after the fork are not ours, and
#    uncommitted edits ARE ours (the scanner analysed the working tree).
#  - Untracked files are marked ALL: the scanner sees them, git diff does not.
git diff -U0 --no-color --no-prefix "$MERGE_BASE" -- | awk '
  /^\+\+\+ /  { path = substr($0, 5); next }
  /^@@ /      {
                if (path == "" || path == "/dev/null") next
                plus = $3; sub(/^\+/, "", plus)
                n = split(plus, part, ",")
                start = part[1] + 0
                len   = (n > 1 ? part[2] + 0 : 1)
                if (len > 0) printf "%s\t%d\t%d\n", path, start, start + len - 1
              }
' >"$WORK/changed.tsv"

git ls-files --others --exclude-standard | while IFS= read -r untracked; do
  [ -n "$untracked" ] && printf '%s\tALL\n' "$untracked"
done >>"$WORK/changed.tsv"

# Findings as "<path>\t<startLine>\t<endLine>\t<severity>\t<type>\t<rule>\t<message>".
# `component` is "<projectKey>:<repo-relative path>"; project keys carry no colon.
jq -r '
  .[]
  | [ (.component | sub("^[^:]*:"; ""))
    , (.textRange.startLine // .line // 0)
    , (.textRange.endLine // .textRange.startLine // .line // 0)
    , (.severity // "?")
    , (.type // "?")
    , (.rule // "?")
    , ((.message // "") | gsub("[\t\n]"; " "))
    ]
  | @tsv
' "$FINDINGS" >"$WORK/findings.tsv"

# Keep a finding when its line range overlaps a changed range in the same file.
awk -F'\t' '
  FILENAME == ARGV[1] {
    if ($2 == "ALL") { all[$1] = 1 }
    else { n = ++count[$1]; lo[$1, n] = $2 + 0; hi[$1, n] = $3 + 0 }
    next
  }
  {
    path = $1; s = $2 + 0; e = $3 + 0
    if (e < s) e = s
    if (path in all) { print; next }
    for (i = 1; i <= count[path]; i++) {
      if (s <= hi[path, i] && e >= lo[path, i]) { print; next }
    }
  }
' "$WORK/changed.tsv" "$WORK/findings.tsv" >"$WORK/owned.tsv"

total="$(wc -l <"$WORK/findings.tsv" | tr -d ' ')"
owned="$(wc -l <"$WORK/owned.tsv" | tr -d ' ')"

echo "Base:       $BASE (merge-base $(git rev-parse --short "$MERGE_BASE"))"
echo "Findings:   $FINDINGS ($total open)"
echo "Attributed: $owned on lines this branch changed"
echo

if [ "$owned" -eq 0 ]; then
  echo "No findings on this branch's own lines."
  exit 0
fi

# Worst first, then by file and line, so the worst thing is the first thing read.
# Severity needs a rank (the same order sonar.sh's sev_rank uses): sorting the
# names as text would put INFO above MAJOR and MINOR.
awk -F'\t' '
  BEGIN { r["BLOCKER"] = 0; r["CRITICAL"] = 1; r["MAJOR"] = 2; r["MINOR"] = 3; r["INFO"] = 4 }
  { printf "%d\t%s\n", ($4 in r ? r[$4] : 9), $0 }
' "$WORK/owned.tsv" | sort -t'	' -k1,1n -k2,2 -k3,3n | awk -F'\t' '
  { printf "%-8s %-11s %-26s %s:%s\n           %s\n", $5, $6, $7, $2, $3, $8 }
'
exit 1
