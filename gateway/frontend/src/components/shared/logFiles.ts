// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Early-feedback checks for a runtime spec's log files (#430), run in the
// launch-spec form before any request is sent.
//
// THE GATEWAY IS THE AUTHORITY. These mirror validateRuntimeSpecLogFiles in
// gateway/backend/internal/portal/service_runtime.go rule for rule -- no laxer
// (a save the form lets through must not be one the gateway answers with a
// 400, which on a CREATE would leave a half-made mapping behind) and no
// stricter (a spec the gateway accepts must stay saveable here). The agent
// re-checks the path and applies its own allowlist; that layer is not mirrored.

import type { LogFile } from '../../api';

export type { LogFile };

/** runtimeSpecLogFilesMax in service_runtime.go. */
export const LOG_FILES_MAX = 4;
/** runtimeSpecLogFilePathMaxBytes in service_runtime.go (in BYTES). */
export const LOG_FILE_PATH_MAX_BYTES = 1024;

const CONFIG_DIR = '${CONFIG_DIR}';
// runtimeSpecConfigFileSecretPlaceholders, shared with the config files: a path
// travels in the clear and is reported, so neither may appear in it.
const SECRET_PLACEHOLDERS = ['${API_TOKEN}', '${AGENT_ENV:'];
// runtimeSpecLogFileNamePattern.
const NAME_PATTERN = /^[A-Za-z0-9._-]{1,32}$/;

export type LogFileIssue =
  | 'name_blank'
  | 'name_invalid'
  | 'name_reserved'
  | 'name_duplicate'
  | 'path_blank'
  | 'path_too_long'
  | 'path_control'
  | 'path_secret'
  | 'path_not_absolute';

const utf8 = new TextEncoder();

// unicode.IsControl: the C0 range, DEL and the C1 range.
function hasControlCharacter(text: string): boolean {
  for (const ch of text) {
    const code = ch.codePointAt(0) ?? 0;
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true;
  }
  return false;
}

// hasLogFileConfigDirRoot: the prefix must be followed by a separator or the
// end, so "${CONFIG_DIR}x" is not a path under the config directory.
function hasConfigDirRoot(path: string): boolean {
  if (!path.startsWith(CONFIG_DIR)) return false;
  const rest = path.slice(CONFIG_DIR.length);
  return rest === '' || rest[0] === '/' || rest[0] === '\\';
}

// The absolute-path helpers below transcribe isAbsPOSIXBinaryPath /
// isAbsWindowsBinaryPath and their volume-name helpers from the gateway. They
// work on UTF-8 BYTES like the Go originals: every character that matters is
// ASCII, and byte indexing keeps "does byte 1 equal ':'" exact for a path that
// starts with a multi-byte character.
const BS = 0x5c;
const SLASH = 0x2f;
const COLON = 0x3a;

function isSep(c: number): boolean {
  return c === BS || c === SLASH;
}

function upper(c: number): number {
  return c >= 0x61 && c <= 0x7a ? c - 0x20 : c;
}

function bytesOf(text: string): number[] {
  return Array.from(utf8.encode(text));
}

// windowsPathHasPrefixFold: ASCII case-insensitive, `\` and `/` equivalent; a
// longer path must continue with a separator.
function hasPrefixFold(path: number[], prefix: string): boolean {
  if (path.length < prefix.length) return false;
  for (let i = 0; i < prefix.length; i += 1) {
    const p = prefix.charCodeAt(i);
    if (isSep(p)) {
      if (!isSep(path[i]!)) return false;
    } else if (upper(p) !== upper(path[i]!)) {
      return false;
    }
  }
  return path.length === prefix.length || isSep(path[prefix.length]!);
}

// windowsUNCLen: the length of a UNC path's `host\share` volume prefix.
function uncLen(path: number[], prefixLen: number): number {
  let separators = 0;
  for (let i = prefixLen; i < path.length; i += 1) {
    if (isSep(path[i]!)) {
      separators += 1;
      if (separators === 2) return i;
    }
  }
  return path.length;
}

// windowsVolumeNameLen (internal/filepathlite.volumeNameLen, windows build).
function volumeNameLen(path: number[]): number {
  if (path.length >= 2 && path[1] === COLON) return 2;
  if (path.length === 0 || !isSep(path[0]!)) return 0;
  if (hasPrefixFold(path, '\\\\.\\UNC')) return uncLen(path, '\\\\.\\UNC\\'.length);
  if (hasPrefixFold(path, '\\\\.') || hasPrefixFold(path, '\\\\?') || hasPrefixFold(path, '\\??')) {
    if (path.length === 3) return 3;
    const afterPrefix = path.slice(4);
    const cut = afterPrefix.findIndex(isSep);
    if (cut < 0) return path.length;
    const restLen = afterPrefix.length - cut - 1;
    return path.length - restLen - 1;
  }
  if (path.length >= 2 && isSep(path[1]!)) return uncLen(path, 2);
  return 0;
}

// isAbsWindowsBinaryPath: a drive letter with either separator, a UNC share or
// a device form. Drive-relative (`C:foo`, `C:`) and root-relative (`\foo`)
// forms are not absolute.
function isAbsWindowsPath(path: string): boolean {
  const bytes = bytesOf(path);
  const volumeLen = volumeNameLen(bytes);
  if (volumeLen === 0) return false;
  if (isSep(bytes[0]!) && isSep(bytes[1]!)) return true;
  const rest = bytes.slice(volumeLen);
  if (rest.length === 0) return false;
  return isSep(rest[0]!);
}

/**
 * The first problem of each row, in the gateway's own order per file (name,
 * reserved, repeat, path blank, length, control character, secret placeholder,
 * root), or null for a clean row. Index-aligned with `files`. A repeat is
 * flagged on the LATER row only, names compared case-insensitively; a row whose
 * own name is refused is not counted as having claimed it. The entry COUNT is
 * the add button's business (LOG_FILES_MAX), not a row's.
 */
export function logFileIssues(files: readonly LogFile[]): (LogFileIssue | null)[] {
  const seen = new Set<string>();
  return files.map((file) => {
    if (file.name.trim() === '') return 'name_blank';
    if (!NAME_PATTERN.test(file.name)) return 'name_invalid';
    const key = file.name.toLowerCase();
    if (key === 'process') return 'name_reserved';
    if (seen.has(key)) return 'name_duplicate';
    seen.add(key);
    if (file.path === '') return 'path_blank';
    if (utf8.encode(file.path).length > LOG_FILE_PATH_MAX_BYTES) return 'path_too_long';
    if (hasControlCharacter(file.path)) return 'path_control';
    if (SECRET_PLACEHOLDERS.some((placeholder) => file.path.includes(placeholder))) {
      return 'path_secret';
    }
    if (
      !hasConfigDirRoot(file.path) &&
      !file.path.startsWith('/') &&
      !isAbsWindowsPath(file.path)
    ) {
      return 'path_not_absolute';
    }
    return null;
  });
}
