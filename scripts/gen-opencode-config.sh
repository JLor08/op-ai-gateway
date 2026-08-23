#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

#
# gen-opencode-config.sh
#
# Generate an opencode.json that uses the OP AI Gateway as an OpenAI-compatible
# provider with per-model context/output limits, so opencode's TUI shows the
# context-window fill ("Weg B" / deterministic fallback).
#
# It reads the gateway's LM Studio-compatible metadata endpoint
# (GET /api/v0/models) to learn each active gateway model's context window
# (max_context_length), and emits a provider block whose models carry
# limit.context (the reported window) and limit.output (min(context/4, 8192),
# the established heuristic). Models the gateway reports without a known context
# size are skipped (opencode falls back to its own default for those) — set the
# mapping's context_size in the gateway (via the /props probe or manually) to
# have them appear with a real value.
#
# The streaming token counter is delivered by the gateway itself (it emits a
# final usage chunk when the client sends stream_options.include_usage, which
# the AI SDK does) — no config needed for that half.
#
# Usage:
#   scripts/gen-opencode-config.sh --url <gateway-base-url> --token <bearer> \
#       [--provider <name>] [--output <file>]
#
#   --url       Gateway base origin, e.g. https://gateway.example
#               (a trailing "/" or "/v1" is stripped automatically). Required.
#   --token     Gateway API bearer token. Required. (Written into the generated
#               file as options.apiKey — treat that file as a secret.)
#   --provider  opencode provider id (default: op-gateway).
#   --output    Output path (default: opencode.json; use "-" for stdout).
#
# Environment fallbacks: OP_GATEWAY_URL, OP_GATEWAY_TOKEN.
#
# Requires: curl, jq.
set -euo pipefail

provider="op-gateway"
output="opencode.json"
url="${OP_GATEWAY_URL:-}"
token="${OP_GATEWAY_TOKEN:-}"

die() { printf 'error: %s\n' "$1" >&2; exit 1; }

usage() {
  sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url)      url="${2:-}"; shift 2 ;;
    --token)    token="${2:-}"; shift 2 ;;
    --provider) provider="${2:-}"; shift 2 ;;
    --output)   output="${2:-}"; shift 2 ;;
    -h|--help)  usage 0 ;;
    *)          die "unknown argument: $1 (try --help)" ;;
  esac
done

command -v curl >/dev/null 2>&1 || die "curl is required but not installed"
command -v jq   >/dev/null 2>&1 || die "jq is required but not installed (brew install jq / apt install jq)"

[ -n "$url" ]   || die "missing --url (or OP_GATEWAY_URL); gateway base origin, e.g. https://gateway.example"
[ -n "$token" ] || die "missing --token (or OP_GATEWAY_TOKEN)"

# Normalize: strip a trailing slash, then a trailing "/v1" if the user pasted the
# OpenAI base URL. We want the origin so we can hit /api/v0/models and build /v1.
base="${url%/}"
base="${base%/v1}"

models_url="$base/api/v0/models"

resp="$(curl -fsS "$models_url" -H "Authorization: Bearer $token")" \
  || die "request to $models_url failed (is the gateway deployed with this feature, and the token valid?)"

echo "$resp" | jq -e 'has("data") and (.data | type == "array")' >/dev/null 2>&1 \
  || die "unexpected response from $models_url (not an LM Studio model list): $resp"

config="$(printf '%s' "$resp" | jq \
  --arg gw "$base/v1" --arg tok "$token" --arg prov "$provider" '{
    provider: {
      ($prov): {
        npm: "@ai-sdk/openai-compatible",
        options: { baseURL: $gw, apiKey: $tok },
        models: (
          [ .data[]
            | select(.max_context_length != null and .max_context_length > 0)
            | { key: .id,
                value: { name: .id,
                         limit: { context: .max_context_length,
                                  output: ([ (.max_context_length / 4 | floor), 8192 ] | min) } } }
          ] | from_entries
        )
      }
    }
  }')"

count="$(printf '%s' "$config" | jq --arg prov "$provider" '.provider[$prov].models | length')"

if [ "$output" = "-" ]; then
  printf '%s\n' "$config"
else
  printf '%s\n' "$config" > "$output"
  printf 'wrote %s (%s model(s) with a known context size)\n' "$output" "$count" >&2
fi

if [ "$count" -eq 0 ]; then
  printf 'warning: no model reported a context size — set context_size on the mappings in the gateway (/props probe or manual), then re-run.\n' >&2
fi
