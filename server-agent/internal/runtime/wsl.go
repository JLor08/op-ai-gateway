package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
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
//
// Two separators at the start are UNC on Windows in either spelling ("//srv/x"
// as much as `\\srv\x`), so they are refused even though POSIX would read
// "//srv/x" as /srv/x. A drive path whose ".." run climbs above the drive's
// root is refused too, not clamped: path.Clean would turn `C:\a\..\..\x` into
// /mnt/x, outside the drive's mount, and Windows itself clamps it to C:\x, so
// neither reading is one the agent can hand over as "the same directory".
func windowsPathToWSL(p string) (string, error) {
	if len(p) >= 2 && isWindowsSep(p[0]) && isWindowsSep(p[1]) {
		return "", fmt.Errorf("runtime: %q is a UNC path, which does not map into a WSL distro", p)
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p), nil
	}
	if len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') &&
		(len(p) == 2 || isWindowsSep(p[2])) {
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		if climbsAboveRoot(rest) {
			return "", fmt.Errorf("runtime: %q climbs above its drive's root, which does not map into a WSL distro", p)
		}
		return path.Clean("/mnt/" + strings.ToLower(p[:1]) + "/" + rest), nil
	}
	return "", fmt.Errorf("runtime: %q is not a drive path that maps into a WSL distro (UNC and relative paths cannot)", p)
}

func isWindowsSep(c byte) bool { return c == '/' || c == '\\' }

// climbsAboveRoot reports whether the slash-separated path p, read from a
// root, has a ".." that would step above that root at any point.
func climbsAboveRoot(p string) bool {
	depth := 0
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
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

// wslSampleScript is the agent's one fixed sampling script, run as
// "sh -c <script> sh <unit...>" inside a distro (sampleWSL). It is the only
// shell the agent ever starts there, and nothing the gateway sends reaches it:
// the script is this constant, and the unit names -- agent-built, see
// wslUnitName -- arrive as positional arguments, read only as "$@" and quoted
// words, never spliced into the program text.
//
// For each unit it prints "unit <u> <memory.current> <inactive_file>" from the
// unit's scope cgroup under the calling user's systemd manager (the path
// systemd-run --user --scope gives it; Phase 0 B6), or "unit <u> -" when those
// files cannot be read: a unit that has already gone, or a cgroup layout the
// agent does not know. It ends with "meminfo <MemTotal kB> <MemAvailable kB>"
// for the whole VM, which every distro shares. POSIX sh with cat and awk (and
// id for the uid in the path), nothing a minimal distro lacks.
const wslSampleScript = `uid=$(id -u)
app=/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service/app.slice
for u in "$@"; do
	s=$app/$u.scope
	c=$(cat "$s/memory.current" 2>/dev/null)
	i=$(awk '$1 == "inactive_file" { print $2 }' "$s/memory.stat" 2>/dev/null)
	if [ -n "$c" ] && [ -n "$i" ]; then
		printf 'unit %s %s %s\n' "$u" "$c" "$i"
	else
		printf 'unit %s -\n' "$u"
	fi
done
awk '$1 == "MemTotal:" { t = $2 } $1 == "MemAvailable:" { a = $2 } END { print "meminfo", t, a }' /proc/meminfo
`

// wslSampleTimeout bounds one beat's sampling calls, all distros together:
// the same 2 s the native measurers bound themselves to, because the calls
// ride the same off-owner measurement (dispatchMeasurement) and Close waits
// for it.
const wslSampleTimeout = 2 * time.Second

// wslVMInfo is the WSL VM's own memory, in MiB, as /proc/meminfo inside it
// reports it: the ceiling .wslconfig gives the VM (MemTotal) and what the VM
// can still hand out (MemAvailable). All distros of one Windows user share the
// VM, so any distro's answer describes it.
type wslVMInfo struct {
	TotalMB, AvailableMB int
}

// parseWSLSample reads what wslSampleScript printed: each unit's RAM figure in
// MiB, memory.current minus inactive_file (the docker stats definition, so the
// figure does not count reclaimable page cache such as a model file's), and
// the VM's meminfo. A figure that is not positive is absent, like a unit
// printed as "-": realRAM's rule, "unknown", never "uses nothing". Any line it
// does not recognise, or a missing or repeated meminfo line, fails the whole
// answer: a garbled sample is no sample.
func parseWSLSample(out []byte) (ramByUnit map[string]int, vm wslVMInfo, err error) {
	garbage := func(line string) error {
		return fmt.Errorf("runtime: unexpected line in the WSL RAM sample: %q", clipText(strings.TrimSpace(line), 120))
	}
	seenMeminfo := false
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case f[0] == "unit" && len(f) == 3 && f[2] == "-":
		case f[0] == "unit" && len(f) == 4:
			current, err1 := strconv.ParseUint(f[2], 10, 63)
			inactive, err2 := strconv.ParseUint(f[3], 10, 63)
			if err1 != nil || err2 != nil {
				return nil, wslVMInfo{}, garbage(line)
			}
			if current <= inactive {
				continue
			}
			if mb := int((current - inactive) >> 20); mb > 0 {
				if ramByUnit == nil {
					ramByUnit = make(map[string]int)
				}
				ramByUnit[f[1]] = mb
			}
		case f[0] == "meminfo" && len(f) == 3 && !seenMeminfo:
			total, err1 := strconv.ParseUint(f[1], 10, 63)
			avail, err2 := strconv.ParseUint(f[2], 10, 63)
			if err1 != nil || err2 != nil || total>>10 == 0 {
				return nil, wslVMInfo{}, garbage(line)
			}
			vm = wslVMInfo{TotalMB: int(total >> 10), AvailableMB: int(avail >> 10)}
			seenMeminfo = true
		default:
			return nil, wslVMInfo{}, garbage(line)
		}
	}
	if !seenMeminfo {
		return nil, wslVMInfo{}, fmt.Errorf("runtime: the WSL RAM sample has no meminfo line")
	}
	return ramByUnit, vm, nil
}

// sampleWSL runs the sampling script once per distro in unitsByDistro, with
// that distro's units, and returns every answered unit's figure and the VM's
// meminfo (nil when no distro answered). It runs off the owner, inside
// dispatchMeasurement's goroutine, under one wslSampleTimeout for all distros.
// Distros are asked in sorted order, so a call log is reproducible.
//
// A distro whose call fails or whose answer does not parse contributes
// nothing, and a Warn says why: like a native measurer that answers nothing,
// its units keep their previous figures (applyMeasurement), and the pool keeps
// its previous reading when no distro answered at all.
func sampleWSL(run wslRunner, pol WSLPolicy, env []string, unitsByDistro map[string][]string) (ramByUnit map[string]int, vm *wslVMInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), wslSampleTimeout)
	defer cancel()
	for _, distro := range slices.Sorted(maps.Keys(unitsByDistro)) {
		command := append([]string{"sh", "-c", wslSampleScript, "sh"}, unitsByDistro[distro]...)
		out, err := runWSLHelperCtx(ctx, run, pol, distro, env, command...)
		if err == nil {
			var byUnit map[string]int
			var info wslVMInfo
			byUnit, info, err = parseWSLSample(out)
			if err == nil {
				for u, mb := range byUnit {
					if ramByUnit == nil {
						ramByUnit = make(map[string]int)
					}
					ramByUnit[u] = mb
				}
				vm = &info
				continue
			}
		}
		slog.Warn("runtime: could not sample the RAM of the WSL workloads", "wsl_distro", distro, "err", err)
	}
	return ramByUnit, vm
}

// wslRAMBudgetMB is the WSL VM RAM pool a wsl candidate must also fit
// (PolicySnapshot.WSLRAMBudgetMB): the VM's MemAvailable plus what the agent's
// own scopes held in that SAME sample (heldMB), minus the operator's reserve.
// It is the host pool's arithmetic (hostRAMBudgetMB) applied to the VM, and
// the add-back exists for the same reason: MemAvailable already excludes what
// the managed scopes hold, which rule 6's VM twin sums again.
//
// The add-back is taken from the sample, not from the processes alive when the
// snapshot is built, and that is what keeps the two halves of the sum about one
// instant. The host pool can afford to add back the live set, because its
// sample is replaced every second; this one is replaced only by the
// measurement beat, only while a wsl process is live to be measured or a wsl
// start waits on the pool (owner.wslVMWaitDistros). With the live set's
// measured RAM at snapshot time as the add-back, a scope that exited after the
// sample takes its share of the add-back with it while the sample's
// MemAvailable still lacks it. That is exactly the state after a wsl start
// evicts the last idle wsl workload to make room: the victim is gone and no
// wsl process is left to sample, so the start the eviction was for would find
// the room missing. Nothing measured would ever show it again, and the start
// would wait for good; even the VM-only sample a waiting start now asks for
// would only cost it a wsl.exe round trip it does not need. The same-sample
// add-back admits it at once.
//
// No sample (vm nil) is 0, unconstrained, as everywhere; a real one floors at
// 1, never 0, for the reason hostRAMBudgetMB gives.
func wslRAMBudgetMB(vm *wslVMInfo, heldMB, reserveMB int) int {
	if vm == nil {
		return 0
	}
	return max(1, vm.AvailableMB+heldMB-reserveMB)
}

// wslRAMCeilingMB is the most the VM pool could ever offer, for the terminal
// own-demand refusal (PolicySnapshot.WSLRAMCeilingMB): the VM's MemTotal minus
// the reserve. A demand above it cannot fit the VM whatever is evicted.
func wslRAMCeilingMB(vm *wslVMInfo, reserveMB int) int {
	if vm == nil {
		return 0
	}
	return max(1, vm.TotalMB-reserveMB)
}
