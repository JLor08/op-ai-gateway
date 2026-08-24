#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins branch-findings.sh, the "did MY branch introduce this?" filter. Run from
# anywhere:
#   sh scripts/sonar/branch-findings.test.sh
#
# Why this needs pinning: the local SonarQube is Community Build, which cannot
# analyze a branch against main (sonar.branch.name needs Developer Edition), and
# a NUMBER_OF_DAYS new-code window silently drops findings once a branch outlives
# the window -- a false green. So the branch comparison is computed from git
# instead, and these are exactly the cases where that computation can be wrong:
# the merge-base semantics (main's own later commits must NOT count as ours), and
# the three ways a line reaches the scanner (committed, uncommitted, untracked).
#
# Everything here is offline: a throwaway git repo plus a hand-written findings
# file. No SonarQube server, no docker.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FILTER="$SCRIPT_DIR/branch-findings.sh"

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

[ -x "$FILTER" ] || { echo "FAIL: $FILTER is missing or not executable"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq is required for these tests"; exit 0; }

REPO="$(mktemp -d)"
trap 'rm -rf "$REPO"' EXIT

ten_lines() {
  i=1
  while [ "$i" -le 10 ]; do
    echo "line $i"
    i=$((i + 1))
  done
}

# --- a repo whose branch diverges from main, exercising every reachability path
git init -q -b main "$REPO"
cd "$REPO"
git config user.email test@example.test
git config user.name "Test"
mkdir -p src
ten_lines >src/touched.txt
ten_lines >src/untouched.txt
ten_lines >src/wip.txt
ten_lines >src/shared.txt
git add -A
git commit -qm "base"

git checkout -qb feature
# committed change on the branch: line 5 of touched.txt
sed -i.bak '5s/.*/line 5 CHANGED/' src/touched.txt && rm -f src/touched.txt.bak
git commit -qam "branch edit"

# main moves on AFTER the branch point (line 7 of shared.txt). The branch never
# takes it, so it must not be attributed to the branch.
git checkout -q main
sed -i.bak '7s/.*/line 7 CHANGED ON MAIN/' src/shared.txt && rm -f src/shared.txt.bak
git commit -qam "main edit after branch point"
git checkout -q feature

# uncommitted working-tree edit (line 3 of wip.txt) -- the scanner sees this
sed -i.bak '3s/.*/line 3 WIP/' src/wip.txt && rm -f src/wip.txt.bak
# brand-new untracked file -- the scanner sees this too
echo "brand new" >src/brandnew.txt

mkdir -p .sonar-local
cat >.sonar-local/findings.json <<'JSON'
[
  {"key":"a","rule":"go:S1","severity":"CRITICAL","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/touched.txt","line":5,
   "textRange":{"startLine":5,"endLine":5},"message":"ON A CHANGED LINE"},
  {"key":"b","rule":"go:S2","severity":"MAJOR","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/touched.txt","line":9,
   "textRange":{"startLine":9,"endLine":9},"message":"SAME FILE UNTOUCHED LINE"},
  {"key":"c","rule":"go:S3","severity":"MAJOR","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/untouched.txt","line":2,
   "textRange":{"startLine":2,"endLine":2},"message":"UNTOUCHED FILE"},
  {"key":"d","rule":"go:S4","severity":"MINOR","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/wip.txt","line":3,
   "textRange":{"startLine":3,"endLine":3},"message":"ON AN UNCOMMITTED LINE"},
  {"key":"e","rule":"go:S5","severity":"INFO","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/brandnew.txt","line":1,
   "textRange":{"startLine":1,"endLine":1},"message":"IN AN UNTRACKED FILE"},
  {"key":"f","rule":"go:S6","severity":"BLOCKER","type":"BUG",
   "component":"op-ai-gateway:src/shared.txt","line":7,
   "textRange":{"startLine":7,"endLine":7},"message":"CHANGED ON MAIN NOT HERE"}
]
JSON

out="$(sh "$FILTER" --repo "$REPO" --base main 2>&1)" && rc=0 || rc=$?

echo "--- filter output ---"
echo "$out"
echo "---------------------"

check "reports a finding on a line the branch committed" "ON A CHANGED LINE" "$out"
check "ignores an untouched line in a touched file" '!SAME FILE UNTOUCHED LINE' "$out"
check "ignores a file the branch never touched" '!UNTOUCHED FILE' "$out"
check "reports a finding on an uncommitted working-tree line" "ON AN UNCOMMITTED LINE" "$out"
check "reports a finding in a brand-new untracked file" "IN AN UNTRACKED FILE" "$out"
check "ignores a line main changed after the branch point (merge-base semantics)" \
  '!CHANGED ON MAIN NOT HERE' "$out"

if [ "$rc" -eq 0 ]; then
  echo "FAIL: exit code was 0 although findings were attributed to the branch"
  fail=1
else
  echo "ok: non-zero exit code when the branch owns findings"
fi

# Worst first: severity order, not alphabetical order (INFO must not outrank MINOR).
sev_order="$(printf '%s' "$out" | awk '{print $1}' | grep -E '^(BLOCKER|CRITICAL|MAJOR|MINOR|INFO)$' | tr '\n' ' ')"
check "lists findings worst-severity first" "CRITICAL MINOR INFO" "$sev_order"

# --- a clean branch: no findings on its own lines -> exit 0
git checkout -q -b clean-branch main
git checkout -q feature -- /dev/null 2>/dev/null || true
cd "$REPO"
cat >.sonar-local/findings.json <<'JSON'
[
  {"key":"c","rule":"go:S3","severity":"MAJOR","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/untouched.txt","line":2,
   "textRange":{"startLine":2,"endLine":2},"message":"UNTOUCHED FILE"}
]
JSON
git checkout -- src/wip.txt 2>/dev/null || true
rm -f src/brandnew.txt

clean_out="$(sh "$FILTER" --repo "$REPO" --base main 2>&1)" && clean_rc=0 || clean_rc=$?
check "clean branch reports nothing of its own" '!UNTOUCHED FILE' "$clean_out"
if [ "$clean_rc" -eq 0 ]; then
  echo "ok: exit code 0 when the branch owns no findings"
else
  echo "FAIL: exit code $clean_rc on a clean branch, want 0"
  fail=1
fi

# --- a STALE local main must not be the default base when origin/main exists.
# This is not hypothetical: in a git worktree you typically only ever fetch
# origin/main and branch off it, so the local main ref can sit months behind. A
# stale base makes the merge base ancient and attributes half the repository to
# the branch, which looks like a working report rather than a broken one.
STALE="$(mktemp -d)"
git init -q -b main "$STALE"
cd "$STALE"
git config user.email test@example.test
git config user.name "Test"
mkdir -p src
ten_lines >src/touched.txt
ten_lines >src/shared.txt
git add -A
git commit -qm "old base"
old="$(git rev-parse HEAD)"

# main moves on, and origin/main records that; the LOCAL main stays behind.
sed -i.bak '7s/.*/line 7 CHANGED ON MAIN/' src/shared.txt && rm -f src/shared.txt.bak
git commit -qam "main moves on"
git update-ref refs/remotes/origin/main "$(git rev-parse HEAD)"
git checkout -qb feature
git branch -qf main "$old" # local main is now stale

sed -i.bak '5s/.*/line 5 CHANGED/' src/touched.txt && rm -f src/touched.txt.bak
git commit -qam "branch edit"

mkdir -p .sonar-local
cat >.sonar-local/findings.json <<'JSON'
[
  {"key":"a","rule":"go:S1","severity":"CRITICAL","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/touched.txt","line":5,
   "textRange":{"startLine":5,"endLine":5},"message":"OURS ON THE BRANCH"},
  {"key":"b","rule":"go:S2","severity":"MAJOR","type":"CODE_SMELL",
   "component":"op-ai-gateway:src/shared.txt","line":7,
   "textRange":{"startLine":7,"endLine":7},"message":"MAINS OWN LATER CHANGE"}
]
JSON

stale_out="$(sh "$FILTER" --repo "$STALE" 2>&1)" || true
check "defaults to origin/main rather than a stale local main" \
  '!MAINS OWN LATER CHANGE' "$stale_out"
check "still attributes the branch's own line with the default base" \
  "OURS ON THE BRANCH" "$stale_out"
check "names the base it actually used" "origin/main" "$stale_out"
rm -rf "$STALE"

if [ "$fail" -eq 0 ]; then
  echo "PASS"
else
  echo "FAILURES"
fi
exit "$fail"
