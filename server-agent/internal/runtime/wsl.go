package runtime

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// wslLaunch is the fully built wsl.exe invocation for one WSL spec: what
// startProcess execs. Exe is the Windows wsl.exe path, Args the whole wrapper
// argv (wsl.exe options, systemd-run, then the operator's binary and args), Env
// the Windows-side environment of wsl.exe (the expanded env plus the
// agent-owned entries), Unit the transient systemd unit name that lets the
// agent address the whole process tree inside the distro.
//
// PrefixLen is the number of Args before the operator's own args: the wrapper
// plus the binary. withWrapper uses it to shift secret spans, so a masked
// argument stays masked at its new index.
type wslLaunch struct {
	Exe       string
	Args      []string
	Env       []string
	Unit      string
	PrefixLen int
}

// wslUnitNameKeyMax bounds the instance-key part of a unit name; systemd unit
// names are limited to 255 bytes and the key is gateway-chosen.
const wslUnitNameKeyMax = 64

// wslUnitPrefix opens every unit name the agent launches, and is what the
// orphan sweep lists by.
const wslUnitPrefix = "opagent-"

// wslUnitRune reports whether r may appear in a unit name as the agent builds
// it: [A-Za-z0-9_-].
func wslUnitRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

// wslUnitName builds the systemd unit name for one launch. The nonce makes it
// unique to this agent run, the generation to this launch of the instance. Any
// character outside [A-Za-z0-9_-] becomes "_": the name ends up in an argv
// (--unit=...) and in the agent's own fixed sampling script's positional
// arguments, so it must not carry anything a unit name or a shell could treat
// specially.
func wslUnitName(nonce, instanceKey string, gen uint64) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if wslUnitRune(r) {
				return r
			}
			return '_'
		}, s)
	}
	key := clean(instanceKey)
	if len(key) > wslUnitNameKeyMax {
		key = key[:wslUnitNameKeyMax]
	}
	return wslUnitPrefix + clean(nonce) + "-" + key + "-" + strconv.FormatUint(gen, 10)
}

// buildWSLLaunch assembles the wsl.exe command. It is pure: no process, no
// filesystem, so the exact argv and environment are unit-tested on any OS.
//
// The argv uses --exec, never a shell, and always "-u <pol.User>": the agent
// never passes root, so no gateway-chosen argument or variable reaches a root
// process (policy refuses root as the user as well). systemd-run --scope puts
// the child in a unit named Unit inside the user's systemd manager, which is
// what makes the tree stoppable and measurable as a group.
//
// env is the expanded Windows-side environment. It is kept as a prefix of the
// result so a caller can tell the entries the agent added (result[len(env):]).
// Agent-owned entries are added only when the key is absent, so a spec's
// explicit PYTHONUNBUFFERED wins; WSLENV is the exception, always the agent's
// (a spec's own is refused by checkSpecEnv).
//
// WSLENV is what carries variables across the Windows/WSL boundary: every name
// except the Windows base block (PATH, HOME, ... would be meaningless or
// harmful inside the distro) and WSL_UTF8 (read by wsl.exe itself) is listed
// with /u, "only when launching from Win32 to WSL".
func buildWSLLaunch(pol WSLPolicy, uid int, distro, unit, workDir, binary string, args, env []string) wslLaunch {
	argv := []string{
		"-d", distro, "-u", pol.User, "--cd", workDir, "--exec",
		"systemd-run", "--user", "--scope", "--quiet", "--unit=" + unit, "--",
		binary,
	}
	prefix := len(argv)
	argv = append(argv, args...)

	out := slices.Clone(env)
	has := func(name string) bool {
		for _, e := range out {
			if k, _, _ := strings.Cut(e, "="); strings.EqualFold(k, name) {
				return true
			}
		}
		return false
	}
	setDefault := func(name, value string) {
		if !has(name) {
			out = append(out, name+"="+value)
		}
	}
	// The child enumerates GPUs by PCI bus order so CUDA_VISIBLE_DEVICES means
	// the same cards as the host's indices (see the SetVisibleDevices notes).
	if has("CUDA_VISIBLE_DEVICES") {
		setDefault("CUDA_DEVICE_ORDER", "PCI_BUS_ID")
	}
	setDefault("PYTHONUNBUFFERED", "1")
	setDefault("XDG_RUNTIME_DIR", "/run/user/"+strconv.Itoa(uid))
	setDefault("WSL_UTF8", "1")

	var names []string
	for _, e := range out {
		k, _, _ := strings.Cut(e, "=")
		if k == "" || strings.EqualFold(k, "WSL_UTF8") || isBaseEnvName(k) {
			continue
		}
		names = append(names, k+"/u")
	}
	slices.Sort(names)
	out = append(out, "WSLENV="+strings.Join(names, ":"))

	return wslLaunch{Exe: pol.Exe, Args: argv, Env: out, Unit: unit, PrefixLen: prefix}
}

func isBaseEnvName(k string) bool {
	for _, b := range baseEnvNames {
		if strings.EqualFold(k, b) {
			return true
		}
	}
	return false
}

// windowsPathToWSL translates a Windows drive path to its /mnt/<drive> mount
// inside a distro. An already-POSIX absolute path passes through. UNC and
// relative paths are refused: neither has a mount the agent can name without
// guessing, and a wrong guess would point a model server at a different file.
func windowsPathToWSL(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p), nil
	}
	if len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') &&
		(len(p) == 2 || p[2] == '\\' || p[2] == '/') {
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		return path.Clean("/mnt/" + strings.ToLower(p[:1]) + "/" + rest), nil
	}
	return "", fmt.Errorf("runtime: %q is not a drive path that maps into a WSL distro (UNC and relative paths cannot)", p)
}

// withWrapper returns ex rewritten for a WSL launch: args become the full
// wrapper argv, with nil spans for the wrapper entries so the operator's args
// keep their secret spans at the shifted indices, and env gains the entries
// the launcher added (l.Env past len(ex.env), which must have been built from
// this ex.env). The added entries are agent-computed: no span, not from spec.
// ex itself is left unchanged.
func (ex expandedSpec) withWrapper(l wslLaunch) expandedSpec {
	out := ex
	out.args = slices.Clone(l.Args)
	out.argSpans = append(make([][]secretSpan, l.PrefixLen), ex.argSpans...)
	extra := l.Env[len(ex.env):]
	out.env = slices.Clone(l.Env)
	out.envSpans = append(slices.Clone(ex.envSpans), make([][]secretSpan, len(extra))...)
	out.envFromSpec = append(slices.Clone(ex.envFromSpec), make([]bool, len(extra))...)
	return out
}
