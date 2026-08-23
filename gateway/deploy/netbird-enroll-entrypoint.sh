#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

#
# NetBird sidecar self-enroll wrapper.
#
# The gateway can mint a NetBird setup key and drop it into a file on a shared
# (transient) volume. This wrapper waits for that file on a FRESH peer, loads the
# key into NB_SETUP_KEY, and then hands control to the stock NetBird entrypoint —
# whose `netbird up` consumes NB_SETUP_KEY from the environment. The result: the
# gateway peer enrolls itself, with no operator copy-paste.
#
# CRITICAL: netbird's CLI treats `--setup-key` (env NB_SETUP_KEY) and
# `--setup-key-file` (env NB_SETUP_KEY_FILE) as MUTUALLY EXCLUSIVE — if BOTH env
# vars merely EXIST (even NB_SETUP_KEY=""), EVERY netbird command aborts with
# "if any flags in the group [setup-key setup-key-file] are set none of the others
# can be". In this deployment the backend + sidecar SHARE a network namespace, so a
# crashing `netbird up` takes the whole netns down and the public API (incl.
# /api/auth/login) 502s. Therefore this wrapper hands netbird EXACTLY ONE key
# source: it reads the key-file path from OUR OWN env var NB_ENROLL_KEY_FILE (which
# netbird ignores), and before exec it (a) unsets NB_SETUP_KEY_FILE so netbird never
# sees a setup-key-file, and (b) unsets an EMPTY NB_SETUP_KEY so netbird sees a
# setup-key only when it is a real value.
#
# Behaviour / invariants:
#   * NB_SETUP_KEY set directly (the Phase-2/3 env style) STILL works untouched:
#     the wait is skipped and the stock entrypoint enrolls as before.
#   * The marker file lives on the PERSISTENT /var/lib/netbird volume (alongside
#     the WireGuard identity). Once an enrollment path has been taken it is set, so
#     a later restart — when the transient key volume is empty again — skips the
#     wait and just reconnects via the persisted identity. Wiping /var/lib/netbird
#     (which also wipes the identity) correctly forces a fresh wait.
#   * A BAD key → the marker is still set → a later restart won't re-wait and the
#     peer stays unenrolled (retry = wipe the marker + the volume, then re-enroll).
#     Acceptable, because the gateway mints a valid key.
#
# NB_STOCK_ENTRYPOINT overrides the stock entrypoint path FOR TESTING ONLY; it
# defaults to the real image path so production behaviour is unchanged.
set -euo pipefail
MARKER="${NB_ENROLL_MARKER:-/var/lib/netbird/.gw-enroll-attempted}"
# Our own key-file env var (netbird ignores it). Fall back to a stray NB_SETUP_KEY_FILE
# so a not-yet-redeployed env still resolves the path; it is unset below regardless.
KEY_FILE="${NB_ENROLL_KEY_FILE:-${NB_SETUP_KEY_FILE:-}}"
STOCK="${NB_STOCK_ENTRYPOINT:-/usr/local/bin/netbird-entrypoint.sh}"
# netbird must never see a setup-key-file (mutually exclusive with NB_SETUP_KEY).
unset NB_SETUP_KEY_FILE || true
# An EMPTY-but-present NB_SETUP_KEY still trips netbird's flag-set check — drop it so
# netbird sees NB_SETUP_KEY only when it holds a real value.
if [[ -z "${NB_SETUP_KEY:-}" ]]; then unset NB_SETUP_KEY || true; fi
# Fresh peer (no marker) + no direct NB_SETUP_KEY + a key-file path configured:
# block until the gateway drops the key file, then hand it to netbird via NB_SETUP_KEY
# (the stock entrypoint's `netbird up` consumes NB_SETUP_KEY from the env).
if [[ ! -f "$MARKER" && -z "${NB_SETUP_KEY:-}" && -n "$KEY_FILE" ]]; then
  echo "netbird-enroll: not yet enrolled; waiting for the setup key at ${KEY_FILE} ..." >&2
  while [[ ! -s "$KEY_FILE" ]]; do sleep 2; done
  NB_SETUP_KEY="$(tr -d '[:space:]' < "$KEY_FILE")"; export NB_SETUP_KEY
  echo "netbird-enroll: setup key loaded; enrolling." >&2
fi
# The gateway also drops the management URL (from System Settings) next to the key, so
# the autonomous `netbird up` targets the same server the key was minted on. An explicit
# NB_MANAGEMENT_URL env WINS; otherwise use the companion file when present.
if [[ -z "${NB_MANAGEMENT_URL:-}" && -n "$KEY_FILE" && -s "${KEY_FILE}.mgmt-url" ]]; then
  NB_MANAGEMENT_URL="$(tr -d '[:space:]' < "${KEY_FILE}.mgmt-url")"; export NB_MANAGEMENT_URL
fi
# Mark that an enrollment path was taken so future restarts (the transient key volume
# is now empty) skip the wait and just reconnect via the persistent identity.
mkdir -p "$(dirname "$MARKER")" 2>/dev/null || true
touch "$MARKER" 2>/dev/null || true
exec "$STOCK" "$@"
