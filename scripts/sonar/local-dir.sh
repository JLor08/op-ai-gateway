# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors
# shellcheck shell=sh

# Shared resolution of the SonarQube .sonar-local directory. Sourced by both
# scripts/sonar/sonar.sh and scripts/sonar/branch-findings.sh so the two cannot
# drift -- they did once: a per-worktree default meant `sonar.sh findings` wrote
# into one worktree's .sonar-local while `branch-findings.sh` read from another.
#
# The SonarQube docker volumes are globally named, so they are shared across
# every git worktree on the same docker daemon; credentials and exports
# therefore belong in ONE place -- the MAIN worktree's .sonar-local, resolved
# via `git worktree list`. Honors SONAR_LOCAL_DIR; falls back to the passed root
# when git cannot resolve the main worktree (a non-git checkout, or a git
# failure).
#
# POSIX sh: sourced from bash sonar.sh and /bin/sh branch-findings.sh alike.
# Usage:  dir="$(sonar_local_dir "<repo-root>")"
sonar_local_dir() {
  _sld_root="$1"
  _sld_main="$(git -C "$_sld_root" worktree list --porcelain 2>/dev/null | awk '/^worktree /{print $2; exit}')"
  printf '%s\n' "${SONAR_LOCAL_DIR:-${_sld_main:-$_sld_root}/.sonar-local}"
}
