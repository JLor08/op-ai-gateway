#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors
#
# Idempotently insert the AGPL-3.0 SPDX header into every in-scope source file.
# Safe to re-run: files that already carry an SPDX-License-Identifier are skipped.
set -euo pipefail

SPDX="SPDX-License-Identifier: AGPL-3.0-only"
COPY="Copyright (C) 2026 OnPrem AI Gateway contributors"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

is_excluded() {
  case "$1" in
    */node_modules/*|*/dist/*|*/.git/*|*/.claude/*|*/.worktrees/*|*/data/*|*/vendor/*) return 0 ;;
    *.md|*.json|*.lock|*.sum|LICENSE*|*/LICENSE*) return 0 ;;
    *.png|*.jpg|*.jpeg|*.gif|*.webp|*.ico|*.svg|*.woff|*.woff2|*.ttf|*.otf|*.eot) return 0 ;;
  esac
  return 1
}

# Emit the comment leader for a path, or "" to skip. Special-cases Dockerfile*/Makefile
# by basename so nested infra files are matched regardless of directory.
leader_for() {
  local base="${1##*/}"
  case "$base" in
    Dockerfile|Dockerfile.*|Makefile) echo "#" ; return ;;
  esac
  case "$1" in
    *.go|*.ts|*.tsx)                    echo "//" ;;
    *.sh|*.yml|*.yaml|*.conf)           echo "#" ;;
    *.html)                             echo "html" ;;
    *)                                  echo "" ;;
  esac
}

# Number of leading lines that MUST stay first (and therefore precede the header):
# a shell shebang, and Docker parser directives (# syntax= / # escape=), which are
# only honored when they are the very first lines of the file.
preamble_lines() {
  local f="$1" n=0 line
  case "${f##*/}" in
    Dockerfile|Dockerfile.*)
      while IFS= read -r line; do
        if [[ "$line" =~ ^#[[:space:]]*(syntax|escape)= ]]; then n=$((n + 1)); else break; fi
      done <"$f"
      echo "$n"; return ;;
  esac
  if head -1 "$f" | grep -q '^#!'; then echo 1; else echo 0; fi
}

add_header() {
  local f="$1" leader
  leader="$(leader_for "$f")"
  [ -z "$leader" ] && return 0
  grep -q "SPDX-License-Identifier" "$f" && return 0
  local tmp
  tmp="$(mktemp)"
  if [ "$leader" = "html" ]; then
    { printf '<!-- %s -->\n<!-- %s -->\n\n' "$SPDX" "$COPY"; cat "$f"; } >"$tmp"
  else
    local pre
    pre="$(preamble_lines "$f")"
    if [ "$pre" -gt 0 ]; then
      { head -n "$pre" "$f"; printf '%s %s\n%s %s\n\n' "$leader" "$SPDX" "$leader" "$COPY"; tail -n +"$((pre + 1))" "$f"; } >"$tmp"
    else
      { printf '%s %s\n%s %s\n\n' "$leader" "$SPDX" "$leader" "$COPY"; cat "$f"; } >"$tmp"
    fi
  fi
  # Preserve executability.
  if [ -x "$f" ]; then chmod +x "$tmp"; fi
  mv "$tmp" "$f"
}

count=0
# Tracked files plus new untracked (respecting .gitignore) so freshly created
# source is covered too.
while IFS= read -r f; do
  [ -z "$f" ] && continue
  [ -f "$f" ] || continue
  is_excluded "$f" && continue
  before="$(grep -c "SPDX-License-Identifier" "$f" || true)"
  add_header "$f"
  after="$(grep -c "SPDX-License-Identifier" "$f" || true)"
  if [ "$before" = "0" ] && [ "$after" != "0" ]; then count=$((count + 1)); fi
done < <(cat <(git ls-files) <(git ls-files --others --exclude-standard) | sort -u)

echo "add-license-headers: added header to $count file(s)"
