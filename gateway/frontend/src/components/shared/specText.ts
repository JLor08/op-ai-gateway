// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Text <-> structure conversion for the runtime spec editors' args and env
// textareas, shared by the model spec form and the RPC member spec form.

import type { Translation } from './types';

// One argument per line. Splitting on spaces would corrupt any argument that
// legitimately contains one (model server command lines routinely have
// these), so a textarea line IS the argument. Lines are preserved verbatim
// -- including an internally blank line and one carrying only whitespace --
// because this form's whole job is faithfully representing a command line:
// filtering "empty-looking" lines would make a load -> edit(no-op) -> save
// round trip silently rewrite the spec's args. The ONE exception is a
// single trailing blank line, which is a textarea artifact (the user's
// cursor sitting on a fresh last line, or a copy-pasted trailing newline),
// not a deliberate trailing empty argument.
export function parseArgsText(text: string): string[] {
  if (text === '') return [];
  const lines = text.split('\n').map((line) => line.replace(/\r$/, ''));
  if (lines.length > 1 && lines.at(-1) === '') {
    lines.pop();
  }
  return lines;
}

export function formatArgsText(args: string[]): string {
  return args.join('\n');
}

// The agent's base-environment names (server-agent internal/runtime
// policy_local.go `baseEnvNames`), which it copies from its OWN environment
// into every child and therefore refuses as spec env keys. Mirrored here so
// the form says no before the round trip rather than letting the spec save
// and fail at process start as `not_permitted`.
//
// UPPER-CASE and compared upper-cased, exactly as the agent does: Windows
// resolves environment names case-insensitively, so "Path" and "SystemRoot"
// -- the only spellings a Windows operator would type -- are the same
// variables. The four Windows names are not decoration: the agent copies
// USERPROFILE/LOCALAPPDATA (per-user cache roots) and SYSTEMROOT/WINDIR
// (system DLLs, and Winsock initialisation) so a Windows child can resolve a
// home directory and open a socket at all.
const reservedEnvKeys = new Set([
  'PATH',
  'HOME',
  'USERPROFILE',
  'LOCALAPPDATA',
  'SYSTEMROOT',
  'WINDIR',
]);

// KEY=value per line, one env var per line. Blank/whitespace-only lines are
// skipped; a malformed (no "=") line is reported via the existing
// runtime_spec.env_invalid label so it reads the same way a backend
// rejection would. Only the KEY portion is trimmed (stray leading
// indentation from a paste) -- the VALUE is preserved byte-for-byte after
// the first "=" (a value may legitimately contain "=" itself, and trimming
// the whole line before splitting would silently eat meaningful leading/
// trailing whitespace IN the value). A key naming one of the agent's base
// environment variables is refused outright, in any capitalisation -- see
// reservedEnvKeys; the ${AGENT_ENV:OP_AGENT_*} / malformed-placeholder checks
// run separately, over both args and env values together, via
// validatePlaceholders.
export function parseEnvText(
  text: string,
  t: Translation,
): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\r$/, '');
    if (line.trim() === '') continue;
    const eq = line.indexOf('=');
    if (eq <= 0) {
      return { env: {}, error: t.errorRuntimeSpecEnvInvalid };
    }
    const key = line.slice(0, eq).trim();
    const value = line.slice(eq + 1);
    if (reservedEnvKeys.has(key.toUpperCase())) {
      return { env: {}, error: t.runtimeSpecEnvReserved };
    }
    env[key] = value;
  }
  return { env };
}

export function formatEnvText(env: Record<string, string>): string {
  return Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n');
}
