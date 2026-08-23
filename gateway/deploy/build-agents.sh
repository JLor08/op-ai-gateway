#!/usr/bin/env sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Cross-compile the server-agent for all supported targets into $AGENT_OUT_DIR and
# write manifest.json. Gated by BUILD_AGENTS: unless true/1/yes this is a no-op
# (exit 0) so a deploy that does not need a rebuild stays fast and keeps the
# existing volume contents.
set -eu

OUT="${AGENT_OUT_DIR:-/agents}"
SRC="${AGENT_SRC_DIR:-/src}"
TARGETS="${AGENT_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"

case "${BUILD_AGENTS:-false}" in
  true|TRUE|True|1|yes|YES) ;;
  *) echo "build-agents: BUILD_AGENTS not enabled — skipping build, keeping existing $OUT"; exit 0 ;;
esac

mkdir -p "$OUT"
cd "$SRC"

# Single-source the version from the code (it is compiled into the binary too).
VERSION="$(sed -n 's/.*Version = "\(.*\)".*/\1/p' internal/agent/agent.go | head -n1)"
GOVER="$(go version | awk '{print $3}')"
BUILT_AT="${AGENT_BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

entries=""
for t in $TARGETS; do
  os="${t%/*}"; arch="${t#*/}"
  name="server-agent-${os}-${arch}"
  [ "$os" = "windows" ] && name="${name}.exe"
  echo "build-agents: building $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$OUT/$name" .
  size="$(wc -c < "$OUT/$name" | tr -d ' ')"
  sha="$(sha256sum "$OUT/$name" | awk '{print $1}')"
  entry="{\"os\":\"$os\",\"arch\":\"$arch\",\"filename\":\"$name\",\"size\":$size,\"sha256\":\"$sha\"}"
  if [ -z "$entries" ]; then entries="$entry"; else entries="$entries,$entry"; fi
done

# Write atomically (temp in the same dir, then rename) so a reader never sees a
# half-written manifest. NOTE: mktemp creates the temp 0600; chmod it to 0644 BEFORE
# the rename so the nonroot backend (UID 65532) that serves this volume read-only can
# actually read the manifest — otherwise loadAgentManifest gets permission-denied and
# the download list shows as unavailable. (The go-build binaries are already 0755.)
tmp="$(mktemp "$OUT/.manifest.XXXXXX")"
printf '{"schema":1,"agent_version":"%s","go_version":"%s","built_at":"%s","binaries":[%s]}\n' \
  "$VERSION" "$GOVER" "$BUILT_AT" "$entries" > "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$OUT/manifest.json"
echo "build-agents: wrote $OUT/manifest.json (version=$VERSION)"
