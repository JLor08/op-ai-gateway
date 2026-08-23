# CLAUDE.md

This repository uses `AGENTS.md` as the canonical agent instruction file.

For Claude Code:

- Read `AGENTS.md` before making changes — especially the Branching And
  Pull Requests rules: never commit to or merge into `main` yourself; all
  changes go through feature branches and pull requests.
- `docs/architecture/` (start at `docs/architecture/README.md`) is the
  canonical description of the system: structure, behavior, constraints,
  decisions, and quality gates.
- `docs/implementation-status.md` and `docs/superpowers/` are branch-local
  working files (read them if the current branch carries them; they are
  removed before every pull request and never exist on `main`).
- Keep this file as a pointer only. Do not add instructions, architecture
  notes, implementation history, verification logs, or handoff state here;
  update the canonical files above instead.
