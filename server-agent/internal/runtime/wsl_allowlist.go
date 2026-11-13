package runtime

import "strings"

// WSLEntryScheme prefixes an allowlist entry that names a binary (or a
// directory) inside a WSL2 distribution: wsl://<distro>/<absolute Linux path>,
// e.g. wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm. The scheme is exactly
// lowercase; WSL:// or wsl:/ is not a WSL entry.
const WSLEntryScheme = "wsl://"

// IsWSLEntry reports whether entry carries the wsl:// scheme, well-formed or
// not. A native allowlist ignores every such entry, so a malformed one can
// never be misread as a native path.
func IsWSLEntry(entry string) bool {
	return strings.HasPrefix(entry, WSLEntryScheme)
}

// ParseWSLEntry splits a well-formed wsl:// entry into its distro and Linux
// path. The distro is everything up to the first "/" after the scheme and must
// match wslDistroPattern; the Linux path is the rest INCLUDING that "/", with
// no NUL and no backslash, and is returned verbatim (no cleaning), exactly as a
// native entry is compared. ok is false for anything else.
func ParseWSLEntry(entry string) (distro, linuxPath string, ok bool) {
	if !IsWSLEntry(entry) {
		return "", "", false
	}
	rest := entry[len(WSLEntryScheme):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", false
	}
	distro, linuxPath = rest[:i], rest[i:]
	if !wslDistroPattern.MatchString(distro) || strings.ContainsAny(linuxPath, "\x00\\") {
		return "", "", false
	}
	return distro, linuxPath, true
}

// splitAllowlist separates a list into its native entries (everything that is
// not a wsl:// entry, malformed ones included) and, per distro, the Linux paths
// of its well-formed wsl:// entries in list order. Malformed wsl:// entries
// land in neither: they permit nothing.
func splitAllowlist(list []string) (native []string, wsl map[string][]string) {
	for _, e := range list {
		if !IsWSLEntry(e) {
			native = append(native, e)
			continue
		}
		if d, p, ok := ParseWSLEntry(e); ok {
			if wsl == nil {
				wsl = make(map[string][]string)
			}
			wsl[d] = append(wsl[d], p)
		}
	}
	return native, wsl
}

// LooksLikeWSLEntry reports whether entry is a failed attempt at a WSL entry:
// its lowercased form starts with "wsl:" but ParseWSLEntry rejects it. That
// covers a malformed wsl:// entry, a wrong-case WSL:// one, and a wsl:/ or
// wsl:x typo. It is the one look-alike rule shared by the fail-closed work_dir
// containment (hasMalformedWSLEntry) and the startup warnings in the config
// package.
func LooksLikeWSLEntry(entry string) bool {
	if len(entry) < 4 || !strings.EqualFold(entry[:4], "wsl:") {
		return false
	}
	_, _, ok := ParseWSLEntry(entry)
	return !ok
}

// hasMalformedWSLEntry reports whether list holds a LooksLikeWSLEntry entry.
// For a directory list that is the operator's failed attempt to restrict WSL
// work_dirs, so WSL containment must stay on (fail closed) rather than fall
// back to "any work_dir".
func hasMalformedWSLEntry(list []string) bool {
	for _, e := range list {
		if LooksLikeWSLEntry(e) {
			return true
		}
	}
	return false
}

// wslDistroAllowed reports whether a well-formed wsl:// binary entry names
// distro. It gates every call into a distro the agent makes on its own
// (pre-warm, orphan sweep): a distro no entry names is never passed to
// wsl.exe -d.
func (p LocalPolicy) wslDistroAllowed(distro string) bool {
	_, wsl := splitAllowlist(p.AllowedBinaries)
	return len(wsl[distro]) > 0
}
