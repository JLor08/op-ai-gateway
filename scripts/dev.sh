#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

set -euo pipefail

GATEWAY_ADDR="127.0.0.1:8091"
PUBLIC_URL="http://127.0.0.1:4173/portal"
HEALTH_URL="http://127.0.0.1:8091/healthz"
PORTAL_URL="http://127.0.0.1:4173/portal/"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

GATEWAY_BIN="$(mktemp -t op-ai-gateway-dev.XXXXXX)"
cleanup() {
  if [ -n "${GATEWAY_PID:-}" ]; then
    kill "$GATEWAY_PID" 2>/dev/null || true
  fi
  rm -f "$GATEWAY_BIN"
}
trap cleanup EXIT INT TERM

echo "Building gateway ..."
(cd gateway/backend && go build -o "$GATEWAY_BIN" ./cmd/gateway)

echo "Starting gateway (memory mode) on ${GATEWAY_ADDR} ..."
OP_AI_GATEWAY_ADDR="$GATEWAY_ADDR" OP_AI_GATEWAY_PUBLIC_URL="$PUBLIC_URL" \
  "$GATEWAY_BIN" &
GATEWAY_PID=$!

echo "Waiting for the gateway to become ready ..."
for _ in $(seq 1 60); do
  if curl -fsS "$HEALTH_URL" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    echo "Gateway exited before becoming ready." >&2
    exit 1
  fi
  sleep 1
done

if [ ! -d gateway/frontend/node_modules ]; then
  echo "Installing frontend dependencies (first run) ..."
  (cd gateway/frontend && npm install)
fi

echo ""
echo "  Portal ready:  ${PORTAL_URL}"
echo "  Login:         dev@example.test / dev-secret"
echo "  Ctrl-C stops both the gateway and the dev server."
echo ""

cd gateway/frontend && npm run dev
