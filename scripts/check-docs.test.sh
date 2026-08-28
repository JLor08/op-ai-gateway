#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins check-docs.sh. Run from anywhere:
#   sh scripts/check-docs.test.sh
#
# Why this needs pinning: a docs checker that reports success on a broken
# corpus is worse than no checker, because it converts "nobody looked" into
# "something looked and it was fine". Every failure mode the script claims to
# catch gets a case here that breaks exactly one thing in an otherwise clean
# fixture, and every place the script must stay quiet (links inside code
# fences, headings inside code fences, external URLs, branch-local scratch
# documents) gets a case too — the false-positive half is what makes the gate
# survivable.
#
# Everything here is offline: a throwaway git repository holding a miniature
# docs corpus, plus a copy of the checker. No network, no docker.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CHECKER="$SCRIPT_DIR/check-docs.sh"

[ -x "$CHECKER" ] || { echo "FAIL: $CHECKER is missing or not executable"; exit 1; }

fail=0
FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

OUT=""
STATUS=0

# Runs the checker over the fixture; leaves its output in $OUT, its exit code
# in $STATUS.
run() {
  set +e
  OUT="$("$FIX/scripts/check-docs.sh" 2>&1)"
  STATUS=$?
  set -e
}

# $1 = description, $2 = expected exit status, $3.. = substrings that must
# appear in the output ("!" prefix: must NOT appear).
expect() {
  desc="$1"
  want="$2"
  shift 2
  ok=1
  if [ "$STATUS" != "$want" ]; then
    echo "FAIL: $desc (exit $STATUS, wanted $want)"
    ok=0
  fi
  for pat in "$@"; do
    case "$pat" in
      '!'*)
        if printf '%s' "$OUT" | grep -qF -- "${pat#!}"; then
          echo "FAIL: $desc (unexpectedly found '${pat#!}')"
          ok=0
        fi
        ;;
      *)
        if ! printf '%s' "$OUT" | grep -qF -- "$pat"; then
          echo "FAIL: $desc (missing '$pat')"
          ok=0
        fi
        ;;
    esac
  done
  if [ "$ok" = 1 ]; then
    echo "ok: $desc"
  else
    fail=1
    printf '%s\n' "$OUT" | sed 's/^/     | /'
  fi
}

# A miniature but complete corpus: a root README, an architecture index, a
# chapter, a cross-cutting document reachable only transitively, and an OpenAPI
# document. Rebuilt from scratch for every case so the cases cannot interfere.
build() {
  rm -rf "$FIX"
  mkdir -p "$FIX/scripts" "$FIX/docs/architecture/cross-cutting" \
           "$FIX/docs/architecture/reference" "$FIX/docs/superpowers/plans"
  cp "$CHECKER" "$FIX/scripts/check-docs.sh"
  : >"$FIX/LICENSE"

  cat >"$FIX/README.md" <<'MD'
# Fixture

See the [architecture documentation](docs/architecture/README.md) and the
[licence](LICENSE). An external link is out of scope:
[upstream](https://example.invalid/whatever).
MD

  cat >"$FIX/docs/architecture/README.md" <<'MD'
# Architecture

## Contents

1. [Introduction](01-introduction.md) — and its [goals](01-introduction.md#1-goals--non-goals)
2. [HTTP API](reference/api.md) (machine-readable: [openapi.yaml](reference/openapi.yaml))
3. [Configuration](reference/config-env.md)
MD

  cat >"$FIX/docs/architecture/01-introduction.md" <<'MD'
# Introduction

## 1. Goals & Non-Goals

Deep dive: [managed runtime](cross-cutting/runtime.md), specifically its
[second notes section](cross-cutting/runtime.md#notes-1) and the
[gate](cross-cutting/runtime.md#the-edge_schemego-gate).

## 2. Decisions

- [ADR-007](cross-cutting/runtime.md#adr-007--the-gateway-specifies-nothing--guesses)
MD

  cat >"$FIX/docs/architecture/cross-cutting/runtime.md" <<'MD'
# Managed Runtime

Back to the [index](../README.md) and out to [AGENTS](../../../README.md).

### The `edge_scheme.go` gate

### ADR-007 — The gateway specifies, nothing → guesses

## Notes

```sh
# Fake Heading
see [nowhere](does-not-exist.md)
```

Inline code is not a link either: `[nowhere](also-missing.md)`.

## Notes
MD

  cat >"$FIX/docs/architecture/reference/api.md" <<'MD'
# HTTP API

The machine-readable spec is [openapi.yaml](openapi.yaml).
MD

  cat >"$FIX/docs/architecture/reference/openapi.yaml" <<'MD'
# SPDX-License-Identifier: AGPL-3.0-only
openapi: 3.1.0
info:
  title: Fixture API
  version: 0.1.0
  description: >
    A block scalar whose body must not be read as structure:
    key: value, - item, {braces}, and even $ref: '#/nope'.
servers:
  - url: http://127.0.0.1:8080
    description: Local
  - url: /
    description: Bundled
security: []
paths:
  /healthz:
    get:
      responses:
        '200': { $ref: '#/components/responses/Healthz' }
        '401': { $ref: '#/components/responses/Unauthorized' }
  '/v1/models/{id}':
    get:
      security: [{ bearerAuth: [] }]
      responses:
        '200':
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Model' }
components:
  securitySchemes:
    bearerAuth: { type: http, scheme: bearer }
  responses:
    Healthz:
      description: ok
    Unauthorized:
      description: no
  schemas:
    Model:
      type: object
MD

  cat >"$FIX/docs/superpowers/plans/scratch.md" <<'MD'
# Scratch Plan

Branch-local, never merged, and deliberately not gated:
[gone](../../../nowhere-at-all.md).

## Task 1
MD

  # Check 4's two inputs. Two settings with flags, one deliberately without --
  # the same three shapes the real table has.
  mkdir -p "$FIX/server-agent/internal/config"
  cat >"$FIX/docs/architecture/reference/config-env.md" <<'MD'
# Configuration

Back to the [index](../README.md).

## Gateway (`OP_AI_GATEWAY_*`)

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AI_GATEWAY_ADDR` | string | Listen address | `:8080` |

## Agent (`OP_AGENT_*`)

Every row below also has a matching CLI flag except those marked **no flag form**.

| Variable | Type | Purpose | Default |
|---|---|---|---|
| `OP_AGENT_GATEWAY_URL` | string, required | Gateway base URL | — |
| `OP_AGENT_TLS_INSECURE` | bool | Skip TLS verification | `false` |
| `OP_AGENT_RUNTIME_ALLOWED_DIRS` | string list (comma-separated; **no flag form**) | Permitted work_dir prefixes | `` (any) |
MD

  cat >"$FIX/server-agent/internal/config/config.go" <<'GO'
package config

func load(args []string) {
	fs := flag.NewFlagSet("server-agent", flag.ContinueOnError)
	gatewayURL := fs.String("gateway-url", "", "gateway base URL (env OP_AGENT_GATEWAY_URL)")
	tlsInsecure := fs.Bool("tls-insecure", false, "skip TLS verification (env OP_AGENT_TLS_INSECURE)")
	_, _ = gatewayURL, tlsInsecure
}
GO

  git -C "$FIX" init -q .
}

# --------------------------------------------------------------------------
# The clean fixture must pass, and must not report the things it must ignore.
# --------------------------------------------------------------------------
build; run
expect "a clean corpus passes" 0 \
  "check-docs: OK" \
  "docs/architecture files: 6, reachable from the index: 6" \
  '$refs: 3' \
  '!does-not-exist.md' \
  '!also-missing.md' \
  '!nowhere-at-all.md' \
  '!example.invalid'

# --------------------------------------------------------------------------
# 1. Links and anchors
# --------------------------------------------------------------------------
build
sed 's#(01-introduction.md)#(01-intruduction.md)#' "$FIX/docs/architecture/README.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/README.md"
run
expect "a link to a file that does not exist fails" 1 \
  "link target does not exist: 01-intruduction.md" "check-docs: FAILED"

build
sed 's/#1-goals--non-goals/#1-goals-non-goals/' "$FIX/docs/architecture/README.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/README.md"
run
expect "a plausible single-hyphen anchor typo fails (removed punctuation leaves TWO hyphens)" 1 \
  "anchor not found in docs/architecture/01-introduction.md: #1-goals-non-goals"

build
sed 's/#adr-007--the-gateway-specifies-nothing--guesses/#adr-007-the-gateway-specifies-nothing-guesses/' \
  "$FIX/docs/architecture/01-introduction.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/01-introduction.md"
run
expect "a generated ADR anchor (em dash + comma + arrow) is checked, not trusted" 1 \
  "anchor not found in docs/architecture/cross-cutting/runtime.md: #adr-007-the-gateway-specifies-nothing-guesses"

build
sed 's/#notes-1/#notes-2/' "$FIX/docs/architecture/01-introduction.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/01-introduction.md"
run
expect "repeated headings are numbered -1, -2 ... and #notes-2 does not exist" 1 \
  "anchor not found in docs/architecture/cross-cutting/runtime.md: #notes-2"

build
printf '\nA fenced heading is not an anchor: [x](cross-cutting/runtime.md#fake-heading).\n' \
  >>"$FIX/docs/architecture/01-introduction.md"
run
expect "a heading inside a fenced code block yields no anchor" 1 \
  "anchor not found in docs/architecture/cross-cutting/runtime.md: #fake-heading"

build
printf '\n[escape](../../../../outside.md)\n' >>"$FIX/docs/architecture/01-introduction.md"
run
expect "a relative link that climbs out of the repository fails" 1 \
  "link escapes the repository: ../../../../outside.md"

# --------------------------------------------------------------------------
# 2. Index reachability
# --------------------------------------------------------------------------
build
cat >"$FIX/docs/architecture/cross-cutting/orphan.md" <<'MD'
# Orphan

A new cross-cutting concept nobody added to the index.
MD
run
expect "an architecture file no index reaches fails" 1 \
  "docs/architecture/cross-cutting/orphan.md: not reachable from docs/architecture/README.md" \
  "docs/architecture files: 7, reachable from the index: 6"

build
# The index links 01-introduction.md, which links runtime.md; the index itself
# never mentions runtime.md. That must still count as reachable.
if grep -q 'cross-cutting/runtime.md' "$FIX/docs/architecture/README.md"; then
  echo "FAIL: fixture no longer exercises transitive reachability"; fail=1
fi
run
expect "reachability is transitive, not just direct" 0 \
  "check-docs: OK" "docs/architecture files: 6, reachable from the index: 6"

build
grep -v 'cross-cutting/runtime.md' "$FIX/docs/architecture/01-introduction.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/01-introduction.md"
run
expect "cutting the only path to a document fails" 1 \
  "docs/architecture/cross-cutting/runtime.md: not reachable from"

# --------------------------------------------------------------------------
# 3. The OpenAPI document
# --------------------------------------------------------------------------
build
sed "s#/components/responses/Healthz'#/components/responses/Healthzz'#" \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a dangling \$ref fails" 1 "dangling \$ref: #/components/responses/Healthzz"

build
sed "s|'#/components/schemas/Model'|'other.yaml#/Model'|" \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a \$ref into another document fails (this spec is single-file)" 1 \
  "\$ref is not a local pointer into this document"

build
sed 's/^  version: 0.1.0$/  version: 0.1.0\n  title: Duplicated/' \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a duplicate key in one mapping fails (YAML would silently keep one)" 1 \
  'duplicate key "title" in /info'

build
sed 's/^info:$/info:\n   stray: x/' \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "indentation that lands between levels fails" 1 "indentation lands between levels"

build
sed 's/^  version: 0.1.0$/\tversion: 0.1.0/' \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a tab in indentation fails" 1 "tab in indentation"

build
printf 'trailing garbage that is not yaml\n' >>"$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a line outside the supported YAML subset fails loudly instead of being skipped" 1 \
  "cannot parse: neither a mapping key nor a sequence item"

build
sed 's/^security: \[\]$/security: [{ a: [] },/' \
  "$FIX/docs/architecture/reference/openapi.yaml" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/openapi.yaml"
run
expect "a multi-line flow collection is refused, not silently mis-read" 1 \
  "multi-line flow collections are not supported"

# --------------------------------------------------------------------------
# 4: config-env.md's agent table vs. the flags server-agent registers
# --------------------------------------------------------------------------
build
run
expect "the clean agent table passes and reports what it checked" 0 \
  "agent settings checked: 3, registered flags: 2"

build
sed 's/^| `OP_AGENT_RUNTIME_ALLOWED_DIRS` | string list (comma-separated; \*\*no flag form\*\*) |/| `OP_AGENT_RUNTIME_ALLOWED_DIRS` | string list |/' \
  "$FIX/docs/architecture/reference/config-env.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/config-env.md"
run
expect "a row that claims a flag the agent does not register fails" 1 \
  "the table claims -runtime-allowed-dirs for OP_AGENT_RUNTIME_ALLOWED_DIRS" \
  "passing it is a startup error"

build
sed 's/^| `OP_AGENT_TLS_INSECURE` | bool |/| `OP_AGENT_TLS_INSECURE` | bool (**no flag form**) |/' \
  "$FIX/docs/architecture/reference/config-env.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/config-env.md"
run
expect "the reverse drift fails too: marked \"no flag form\" while the flag exists" 1 \
  'OP_AGENT_TLS_INSECURE is marked "no flag form" but -tls-insecure is registered'

build
sed 's/^## Agent (`OP_AGENT_\*`)$/## Agent settings/' \
  "$FIX/docs/architecture/reference/config-env.md" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/docs/architecture/reference/config-env.md"
run
expect "renaming the agent table out from under the check fails loudly, not silently" 1 \
  "no OP_AGENT_* rows found"

build
sed 's/fs\.String(/fs.StringVarRenamed(/; s/fs\.Bool(/fs.BoolVarRenamed(/' \
  "$FIX/server-agent/internal/config/config.go" >"$FIX/t" \
  && mv "$FIX/t" "$FIX/server-agent/internal/config/config.go"
run
expect "an extractor that no longer matches the Go source fails instead of passing vacuously" 1 \
  "no fs.String/fs.Bool registrations found"

build
rm -f "$FIX/server-agent/internal/config/config.go"
run
expect "the check skips, rather than fails, where the agent module is absent" 0 \
  "skipped:" "!agent settings checked"

if [ "$fail" = 0 ]; then
  echo "all check-docs cases passed"
else
  echo "check-docs.test.sh: FAILURES"
fi
exit "$fail"
