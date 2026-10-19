// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Early-feedback checks for a runtime spec's config files (#257), run in the
// launch-spec form before any request is sent.
//
// THE GATEWAY IS THE AUTHORITY. These mirror validateRuntimeSpecConfigFiles /
// configFileNameIsSingleSegment in gateway/backend/internal/portal/
// service_runtime.go rule for rule -- no laxer (a save the form lets through
// must not be one the gateway then answers with a 400, which on a CREATE would
// leave a half-made mapping behind, because the mapping is written before its
// spec) and no stricter (a spec the gateway accepts must stay saveable here).
// The agent re-checks the names itself before writing; that stricter layer
// (trailing dot/space, Windows device names, Unicode format characters) is
// deliberately not mirrored, for the same reason the gateway does not.

/** runtimeSpecConfigFilesMax in service_runtime.go. */
export const CONFIG_FILES_MAX = 16;
/** runtimeSpecConfigFileMaxBytes in service_runtime.go (64 KiB, in BYTES). */
export const CONFIG_FILE_MAX_BYTES = 64 << 10;

// runtimeSpecConfigFileSecretPlaceholders: the two placeholders that resolve
// to a secret at launch. A config file is written in the clear, so neither may
// appear in its content. "${AGENT_ENV:" without the closing brace, exactly as
// the gateway has it: ANY spelling of the reference is refused, valid or not.
const SECRET_PLACEHOLDERS = ['${API_TOKEN}', '${AGENT_ENV:'];

export type ConfigFileIssue =
  'name_blank' | 'name_invalid' | 'name_duplicate' | 'content_too_large' | 'content_secret';

const utf8 = new TextEncoder();

// unicode.IsControl: the C0 range, DEL and the C1 range.
function hasControlCharacter(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) {
    const code = text.codePointAt(i) ?? 0;
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true;
  }
  return false;
}

/**
 * Whether `name` is one plain file name: non-blank, not "." or "..", no path
 * separator of either OS, no ":" (volume or alternate-data-stream marker) and
 * no control character. Null when it is.
 */
export function configFileNameIssue(name: string): 'name_blank' | 'name_invalid' | null {
  if (name.trim() === '') return 'name_blank';
  if (name === '.' || name === '..') return 'name_invalid';
  if (/[/\\:]/.test(name) || hasControlCharacter(name)) return 'name_invalid';
  return null;
}

/**
 * The first problem of each row, in the gateway's own order per file (name,
 * repeat, size, secret placeholder), or null for a clean row. Index-aligned
 * with `files`. A repeat is flagged on the LATER row only, names compared
 * case-insensitively; a row whose own name is invalid is not counted as
 * having claimed it.
 */
export function configFileIssues(
  files: readonly { name: string; content: string }[],
): (ConfigFileIssue | null)[] {
  const seen = new Set<string>();
  return files.map((file) => {
    const nameIssue = configFileNameIssue(file.name);
    if (nameIssue) return nameIssue;
    const key = file.name.toLowerCase();
    if (seen.has(key)) return 'name_duplicate';
    seen.add(key);
    if (utf8.encode(file.content).length > CONFIG_FILE_MAX_BYTES) return 'content_too_large';
    if (SECRET_PLACEHOLDERS.some((placeholder) => file.content.includes(placeholder))) {
      return 'content_secret';
    }
    return null;
  });
}
