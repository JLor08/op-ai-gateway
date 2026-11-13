package runtime

import "op-ai-server-agent/internal/wslentry"

// splitAllowlist separates a list into its native entries (everything that is
// not a wsl:// entry, malformed ones included) and, per distro, the Linux paths
// of its well-formed wsl:// entries in list order. Malformed wsl:// entries
// land in neither: they permit nothing.
func splitAllowlist(list []string) (native []string, wsl map[string][]string) {
	for _, e := range list {
		if !wslentry.Is(e) {
			native = append(native, e)
			continue
		}
		if d, p, ok := wslentry.Parse(e); ok {
			if wsl == nil {
				wsl = make(map[string][]string)
			}
			wsl[d] = append(wsl[d], p)
		}
	}
	return native, wsl
}

// hasMalformedWSLEntry reports whether list holds a wslentry.LooksLike entry.
// For a directory list that is the operator's failed attempt to restrict WSL
// work_dirs, so WSL containment must stay on (fail closed) rather than fall
// back to "any work_dir".
func hasMalformedWSLEntry(list []string) bool {
	for _, e := range list {
		if wslentry.LooksLike(e) {
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
