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

// systemdUserFlag points systemd-run and systemctl at the calling user's own
// systemd manager, the one every unit the agent launches inside a distro lives
// in: the launch (buildWSLLaunch), the stop and the orphan sweep all address it.
const systemdUserFlag = "--user"

// wslUnitRune reports whether r may appear in a unit name as the agent builds
// it: [A-Za-z0-9_-].
func wslUnitRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

// wslNonceLen is the length of a run nonce (newWSLNonce): 8 lowercase hex
// characters.
const wslNonceLen = 8

// isWSLNonce reports whether s has the shape newWSLNonce draws.
func isWSLNonce(s string) bool {
	if len(s) != wslNonceLen {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// parseWSLUnitName is wslUnitName read backwards: it splits name (without
// ".scope") into the run nonce, the cleaned instance key and the generation,
// and reports whether name has exactly the shape wslUnitName builds from a
// nonce newWSLNonce drew -- the prefix, an 8-hex nonce, a non-empty key of at
// most wslUnitNameKeyMax unit runes, and a decimal generation as FormatUint
// writes it. The key may itself contain "-", so the generation is what follows
// the last one. The orphan sweep kills only names this accepts: an
// opagent-prefixed unit of any other shape is not one the agent made, so it is
// the operator's, and left alone.
func parseWSLUnitName(name string) (nonce, key string, gen uint64, ok bool) {
	rest, ok := strings.CutPrefix(name, wslUnitPrefix)
	if !ok || len(rest) <= wslNonceLen {
		return "", "", 0, false
	}
	nonce, rest = rest[:wslNonceLen], rest[wslNonceLen:]
	rest, ok = strings.CutPrefix(rest, "-")
	if !ok || !isWSLNonce(nonce) {
		return "", "", 0, false
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return "", "", 0, false
	}
	key, genText := rest[:i], rest[i+1:]
	if key == "" || len(key) > wslUnitNameKeyMax || strings.IndexFunc(key, func(r rune) bool { return !wslUnitRune(r) }) >= 0 {
		return "", "", 0, false
	}
	gen, err := strconv.ParseUint(genText, 10, 64)
	if err != nil || strconv.FormatUint(gen, 10) != genText {
		return "", "", 0, false
	}
	return nonce, key, gen, true
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

// wslLaunchInput is what one wsl launch is built from (buildWSLLaunch), besides
// the operator's WSL policy: the distro and the UID of the policy's user inside
// it, the unit name (wslUnitName), the working directory inside the distro, and
// the operator's binary with its expanded args and Windows-side env.
type wslLaunchInput struct {
	UID     int
	Distro  string
	Unit    string
	WorkDir string
	Binary  string
	Args    []string
	Env     []string
}

// buildWSLLaunch assembles the wsl.exe command. It is pure: no process, no
// filesystem, so the exact argv and environment are unit-tested on any OS.
//
// The argv uses --exec, never a shell, and always "-u <pol.User>": the agent
// never passes root, so no gateway-chosen argument or variable reaches a root
// process (policy refuses root as the user as well). systemd-run --scope puts
// the child in a unit named in.Unit inside the user's systemd manager, which
// is what makes the tree stoppable and measurable as a group.
//
// in.Env is the expanded Windows-side environment. It is kept as a prefix of
// the result so a caller can tell the entries the agent added
// (result[len(in.Env):]). Agent-owned entries are added only when the key is
// absent, so a spec's explicit PYTHONUNBUFFERED wins; WSLENV is the exception,
// always the agent's (a spec's own is refused by checkSpecEnv).
//
// WSLENV is what carries variables across the Windows/WSL boundary: every name
// except the Windows base block (PATH, HOME, ... would be meaningless or
// harmful inside the distro) and WSL_UTF8 (read by wsl.exe itself) is listed
// with /u, "only when launching from Win32 to WSL".
func buildWSLLaunch(pol WSLPolicy, in wslLaunchInput) wslLaunch {
	argv := []string{
		"-d", in.Distro, "-u", pol.User, "--cd", in.WorkDir, "--exec",
		"systemd-run", systemdUserFlag, "--scope", "--quiet", "--unit=" + in.Unit, "--",
		in.Binary,
	}
	prefix := len(argv)
	argv = append(argv, in.Args...)

	out := slices.Clone(in.Env)
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
	setDefault("XDG_RUNTIME_DIR", "/run/user/"+strconv.Itoa(in.UID))
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

	return wslLaunch{Exe: pol.Exe, Args: argv, Env: out, Unit: in.Unit, PrefixLen: prefix}
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
// It first prints its sentinel line, wslSampleSentinel: wsl.exe may print
// messages of its own on stdout ahead of the command's output, so
// parseWSLSample takes nothing before that line for the script's. For each
// unit it then prints "unit <u> <memory.current> <inactive_file>" from the
// unit's scope cgroup under the calling user's systemd manager (the path
// systemd-run --user --scope gives it; Phase 0 B6), or "unit <u> -" when those
// files cannot be read: a unit that has already gone, or a cgroup layout the
// agent does not know. It ends with "meminfo <MemTotal kB> <MemAvailable kB>"
// for the whole VM, which every distro shares. POSIX sh with printf, cat and
// awk (and id for the uid in the path), nothing a minimal distro lacks.
const wslSampleScript = "printf '%s\\n' '" + wslSampleSentinel + "'\n" + `uid=$(id -u)
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

// wslSampleSentinel is the first line wslSampleScript prints, versioned so a
// change of the script's output grammar can change it too. It holds no quote:
// the script prints it from a single-quoted word.
const wslSampleSentinel = "opagent-sample v1"

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
// printed as "-": realRAM's rule, "unknown", never "uses nothing".
//
// Everything before the script's sentinel line (wslSampleSentinel) is
// wsl.exe's, not the script's, and is skipped; an answer without that line
// holds nothing that is certainly the script's, and fails. After it, any line
// it does not recognise, or a missing or repeated meminfo line, fails the
// whole answer: a garbled sample is no sample.
func parseWSLSample(out []byte) (ramByUnit map[string]int, vm wslVMInfo, err error) {
	text := string(out)
	body, err := wslSampleBody(text)
	if err != nil {
		return nil, wslVMInfo{}, err
	}
	var r wslSampleReader
	for line := range strings.Lines(text[body:]) {
		if err = r.readLine(line); err != nil {
			return nil, wslVMInfo{}, err
		}
	}
	if !r.seenMeminfo {
		return nil, wslVMInfo{}, fmt.Errorf("runtime: the WSL RAM sample has no meminfo line")
	}
	return r.ramByUnit, r.vm, nil
}

// wslSampleBody returns where the script's own output starts in text, a
// sampling call's stdout: just past its sentinel line (wslSampleSentinel),
// whatever wsl.exe printed ahead of it. Output without that line fails, naming
// how it began.
func wslSampleBody(text string) (int, error) {
	body := 0
	for line := range strings.Lines(text) {
		body += len(line)
		if strings.TrimSpace(line) == wslSampleSentinel {
			return body, nil
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return 0, fmt.Errorf("runtime: the WSL RAM sample has no %q line; its output began %q", wslSampleSentinel, clipText(strings.TrimSpace(first), 120))
}

// wslSampleGarbage is the error for a line of a sample that parseWSLSample
// does not accept.
func wslSampleGarbage(line string) error {
	return fmt.Errorf("runtime: unexpected line in the WSL RAM sample: %q", clipText(strings.TrimSpace(line), 120))
}

// wslSampleReader is what parseWSLSample has read of the script's output so
// far, past its sentinel line: the units' positive figures (nil while there are
// none) and the VM's meminfo, once seen.
type wslSampleReader struct {
	ramByUnit   map[string]int
	vm          wslVMInfo
	seenMeminfo bool
}

// readLine takes one line of the script's output: a blank line or a unit
// printed as "-" is skipped, a unit's figures or the first meminfo line are
// read, and anything else -- a second meminfo line among it -- fails.
func (r *wslSampleReader) readLine(line string) error {
	f := strings.Fields(line)
	switch {
	case len(f) == 0:
		return nil
	case f[0] == "unit" && len(f) == 3 && f[2] == "-":
		return nil
	case f[0] == "unit" && len(f) == 4:
		return r.readUnit(f, line)
	case f[0] == "meminfo" && len(f) == 3 && !r.seenMeminfo:
		return r.readMeminfo(f, line)
	default:
		return wslSampleGarbage(line)
	}
}

// readUnit takes a "unit <u> <memory.current> <inactive_file>" line, split
// into f: the unit's figure, in MiB, is the difference, recorded only when it
// is positive (absent otherwise, never "uses nothing"). Figures that are not
// numbers fail the line.
func (r *wslSampleReader) readUnit(f []string, line string) error {
	current, err1 := strconv.ParseUint(f[2], 10, 63)
	inactive, err2 := strconv.ParseUint(f[3], 10, 63)
	if err1 != nil || err2 != nil {
		return wslSampleGarbage(line)
	}
	if current <= inactive {
		return nil
	}
	if mb := int((current - inactive) >> 20); mb > 0 {
		if r.ramByUnit == nil {
			r.ramByUnit = make(map[string]int)
		}
		r.ramByUnit[f[1]] = mb
	}
	return nil
}

// readMeminfo takes the "meminfo <MemTotal kB> <MemAvailable kB>" line, split
// into f, as the VM's figures in MiB. Figures that are not numbers, or a total
// under 1 MiB, fail the line.
func (r *wslSampleReader) readMeminfo(f []string, line string) error {
	total, err1 := strconv.ParseUint(f[1], 10, 63)
	avail, err2 := strconv.ParseUint(f[2], 10, 63)
	if err1 != nil || err2 != nil || total>>10 == 0 {
		return wslSampleGarbage(line)
	}
	r.vm = wslVMInfo{TotalMB: int(total >> 10), AvailableMB: int(avail >> 10)}
	r.seenMeminfo = true
	return nil
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
