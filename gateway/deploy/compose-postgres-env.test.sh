#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

# Pins Postgres credential sourcing from `.env` for both docker-compose stacks. Run
# from anywhere:
#   sh gateway/deploy/compose-postgres-env.test.sh
#
# The bundled `db` service and the `backend` service's assembled
# OP_AI_GATEWAY_POSTGRES_DSN must both come from POSTGRES_USER / POSTGRES_PASSWORD /
# POSTGRES_DB in `.env` -- an operator changes credentials there ONLY, never in the
# compose files. This renders both docker-compose.yml and
# docker-compose.no-netbird.yml with `docker compose config` twice each: once with
# custom POSTGRES_* values (proving real interpolation, not a hardcoded echo of the
# default) and once with none set (proving the fallback defaults are byte-identical
# to today's hardcoded values: gateway / change-me / gateway -- no DB migration is
# expected from this change).
set -eu

cd "$(dirname "$0")"

fail=0
note() { echo "  $*"; }
bad() { echo "FAIL $*"; fail=1; }

# `backend` keeps `env_file: .env`, and `docker compose config` refuses to render a
# service whose env_file does not exist on disk. A fresh checkout has no `.env` (it
# is gitignored) -- create a throwaway one from .env.example for the duration of this
# test and remove it again, so the test is self-contained and never touches a real
# operator `.env`.
created_env=0
if [ ! -f .env ]; then
  cp .env.example .env
  created_env=1
fi
cleanup() { [ "$created_env" -eq 1 ] && rm -f .env; return 0; }
trap cleanup EXIT INT TERM

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo "SKIPPED: docker (with the compose plugin) is unavailable -- cannot render 'docker compose config'."
  exit 0
fi

# has <haystack> <needle> -- plain substring test, tolerant of the YAML emitter
# choosing to quote a scalar value (or not).
has() {
  case "$1" in
    *"$2"*) return 0 ;;
    *) return 1 ;;
  esac
}

for f in docker-compose.yml docker-compose.no-netbird.yml; do
  echo "== $f: custom POSTGRES_* env interpolates =="
  if out=$(POSTGRES_USER=alice POSTGRES_PASSWORD=s3cret POSTGRES_DB=mydb docker compose -f "$f" config 2>&1); then
    has "$out" 'POSTGRES_USER: alice' \
      || bad "$f: rendered 'db' service is missing POSTGRES_USER: alice"
    has "$out" 'POSTGRES_PASSWORD: s3cret' \
      || bad "$f: rendered 'db' service is missing POSTGRES_PASSWORD: s3cret"
    has "$out" 'POSTGRES_DB: mydb' \
      || bad "$f: rendered 'db' service is missing POSTGRES_DB: mydb"
    has "$out" 'postgres://alice:s3cret@db:5432/mydb' \
      || bad "$f: backend OP_AI_GATEWAY_POSTGRES_DSN did not interpolate to postgres://alice:s3cret@db:5432/mydb"
    has "$out" 'pg_isready -U alice' \
      || bad "$f: db healthcheck did not interpolate to pg_isready -U alice (still probes the default user?)"
    note "$f: custom env reached both the db service and the backend DSN"
  else
    bad "$f: 'docker compose config' failed with custom POSTGRES_* env:"
    echo "$out" | sed 's/^/      /'
  fi

  echo "== $f: unset POSTGRES_* env renders today's defaults =="
  if out=$(env -u POSTGRES_USER -u POSTGRES_PASSWORD -u POSTGRES_DB docker compose -f "$f" config 2>&1); then
    has "$out" 'POSTGRES_USER: gateway' \
      || bad "$f: default POSTGRES_USER did not render as 'gateway'"
    has "$out" 'POSTGRES_PASSWORD: change-me' \
      || bad "$f: default POSTGRES_PASSWORD did not render as 'change-me'"
    has "$out" 'POSTGRES_DB: gateway' \
      || bad "$f: default POSTGRES_DB did not render as 'gateway'"
    has "$out" 'postgres://gateway:change-me@db:5432/gateway' \
      || bad "$f: default backend OP_AI_GATEWAY_POSTGRES_DSN did not render postgres://gateway:change-me@db:5432/gateway"
    has "$out" 'pg_isready -U gateway' \
      || bad "$f: default db healthcheck did not render pg_isready -U gateway"
    note "$f: unset env renders the byte-identical gateway/change-me/gateway defaults"
  else
    bad "$f: 'docker compose config' failed with no POSTGRES_* env set:"
    echo "$out" | sed 's/^/      /'
  fi
done

[ "$fail" -eq 0 ] || { echo "compose-postgres-env.test.sh FAILED"; exit 1; }
echo OK
