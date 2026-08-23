#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
script="$here/build-agents.sh"
fail=0
check() { if [ "$1" != "$2" ]; then echo "FAIL: $3 (got '$1' want '$2')"; fail=1; else echo "ok: $3"; fi; }

# Case i: BUILD_AGENTS unset → no-op, no files written.
out1="$(mktemp -d)"
AGENT_OUT_DIR="$out1" AGENT_SRC_DIR="$repo_root/server-agent" sh "$script" >/dev/null
check "$(ls -A "$out1" | wc -l | tr -d ' ')" "0" "BUILD_AGENTS unset writes nothing"

# Case ii: BUILD_AGENTS=true with a 2-target subset (fast) → files + .exe + manifest.
out2="$(mktemp -d)"
BUILD_AGENTS=true AGENT_TARGETS="linux/amd64 windows/amd64" AGENT_BUILT_AT="2026-08-07T00:00:00Z" \
  AGENT_OUT_DIR="$out2" AGENT_SRC_DIR="$repo_root/server-agent" sh "$script" >/dev/null
check "$([ -f "$out2/server-agent-linux-amd64" ] && echo yes || echo no)" "yes" "linux binary present"
check "$([ -f "$out2/server-agent-windows-amd64.exe" ] && echo yes || echo no)" "yes" "windows binary has .exe"
check "$(grep -c '"filename":"server-agent-windows-amd64.exe"' "$out2/manifest.json")" "1" "manifest lists .exe filename"
check "$(grep -c '"agent_version":"' "$out2/manifest.json")" "1" "manifest has agent_version"
check "$(grep -o '"sha256":"' "$out2/manifest.json" | wc -l | tr -d ' ')" "2" "manifest has a sha256 per binary"
# manifest.json must be world-readable (the nonroot backend serving the volume reads it);
# mktemp defaults to 0600, so the script must chmod it. Char 8 of the ls perm string is
# the other-read bit ('r' for 0644, '-' for 0600).
check "$(ls -l "$out2/manifest.json" | awk '{print $1}' | cut -c8)" "r" "manifest is world-readable (not mktemp 0600)"

rm -rf "$out1" "$out2"
[ "$fail" = "0" ] && echo "ALL PASS" || { echo "FAILURES"; exit 1; }
