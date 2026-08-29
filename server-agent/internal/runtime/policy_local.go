// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// agentOwnEnvPrefix marks the agent's own configuration/credential
// namespace. ${AGENT_ENV:NAME} refuses any NAME starting with this prefix --
// the agent's own environment holds OP_AGENT_TOKEN (its gateway bearer
// credential, which authenticates the certificate, CA, and runtime-config
// endpoints), so without this refusal a gateway-supplied spec could read it
// straight back out and impersonate the agent.
const agentOwnEnvPrefix = "OP_AGENT_"

// baseEnvNames is the complete set of variables the agent copies from its
// OWN environment into a child's minimal base -- and, because the same list
// drives the spec-env reservation in ExpandPlaceholders, the complete set of
// names a spec may never set. ONE list with BOTH uses is deliberate: a name
// added to the base but not reserved hands a gateway-supplied spec a
// duplicate key, and os/exec resolves duplicates last-occurrence-wins (on
// Windows case-insensitively), so the spec's value would WIN over the
// agent's -- precisely the defect the PATH reservation exists to close.
//
// Every entry MUST be spelled in upper case: the reservation compares a
// spec key upper-cased, and these spellings are emitted verbatim into the
// child's environment. TestBaseEnvNamesAreUpperCase pins that invariant.
// Emitting the upper-cased spelling of a name Windows writes as "SystemRoot"
// or "windir" is safe for the same reason the reservation has to fold case
// at all: Windows resolves environment names through GetEnvironmentVariableW,
// which is case-INSENSITIVE, and so is the CRT's getenv there.
//
// The list is the UNION over the platforms the agent is built for, gated on
// presence -- not a runtime.GOOS switch and not a _windows.go/_unix.go
// split. Three reasons, in order of weight:
//
//  1. It stays observable. CI compiles nothing for Windows and runs no
//     Windows job (docs/architecture/11-risks-and-technical-debt.md §11.1),
//     so a GOOS-selected list would put the Windows half of this rule behind
//     a branch no test on any CI host can enter -- the same blind spot that
//     let two case-sensitivity defects ship. As a union it is exercised
//     end-to-end from a Linux or macOS test host through the injected
//     getenv seam.
//  2. It is already correct per platform, because presence does the
//     selecting. A Linux agent defines none of USERPROFILE, LOCALAPPDATA,
//     SYSTEMROOT, WINDIR, so its children receive exactly the PATH/HOME they
//     received before this list existed. A Windows agent normally defines no
//     HOME, so its children receive the Windows four instead. Where a host
//     genuinely defines both (Git-Bash/MSYS on Windows sets HOME; a Wine or
//     WSL-interop shell on Linux may export a Windows name), copying both is
//     the right answer anyway -- the value came from the agent's own
//     environment, which is the operator's, not the gateway's.
//  3. The reservation must be platform-independent regardless, on the
//     precedent already set for the AGENT_ENV guard below: one rule on every
//     platform is the only version a reader can check. Keeping the base
//     platform-independent too means there is one list, not a list and an
//     exception.
//
// Why each name is here -- the bar is "a normal process on that OS does not
// work without it", NOT "it might be handy", because every name added is a
// name an operator can no longer set through a spec:
//
//   - PATH -- where the child resolves helper binaries, and on Windows part
//     of the DLL search order. Agent-owned since the first version of this
//     function: a spec-supplied PATH would steer a permitted binary's dynamic
//     linker and undo the absolute-path allowlist.
//   - HOME -- the POSIX home indicator, from which config and cache
//     directories are derived (~/.cache/huggingface, $HOME/.ollama).
//   - USERPROFILE -- the Windows home indicator, and the one this list was
//     grown for. Windows normally sets no HOME at all, so a child launched by
//     the pre-union base got NO home indicator whatsoever and every per-user
//     path resolution failed; llama-server reported it as
//     "failed to initialize router models: Failed to determine HF cache
//     directory", because the Hugging Face cache root is ~/.cache/huggingface
//     and "~" on Windows IS %USERPROFILE% (Go's os.UserHomeDir, Python's
//     ntpath.expanduser and Node's os.homedir all read this one variable).
//   - LOCALAPPDATA -- the Windows per-user, machine-local cache/data root,
//     i.e. the platform's answer to $XDG_CACHE_HOME. This is NOT redundant
//     with USERPROFILE: llama.cpp's own fs_get_cache_directory() reads
//     LOCALAPPDATA directly on _WIN32 (LLAMA_CACHE overrides it) and fails
//     the same way when it is unset, one function over from the failure that
//     produced this fix. Ollama's Windows paths sit under it too. Passing
//     only USERPROFILE would have cured the reported symptom and left its
//     twin live.
//   - SYSTEMROOT -- do not delete this one as obviously unnecessary. Beyond
//     being where Windows resolves system DLLs, it is required for WINSOCK
//     INITIALISATION: a process whose environment block lacks SystemRoot
//     fails in WSAStartup with 10107 ("a system call that should never fail
//     has failed"), because ws2_32 reaches its provider catalog through that
//     path. Every process this package launches is a network server, so
//     omitting SystemRoot does not produce a missing-cache message -- it
//     produces a model server that cannot open a socket, reported as an
//     opaque startup crash. This is a well-known subprocess trap in other
//     runtimes (Java since JDK 1.5, CPython) for exactly the reason it bit
//     here: hand a child a hand-built environment and this is the variable
//     you forget.
//   - WINDIR -- the same directory under its legacy name, set on every
//     Windows install. Kept because tooling that reads only %WINDIR% still
//     exists and the value is a well-known constant path, so copying it adds
//     no exposure the SYSTEMROOT entry has not already accepted.
//
// Deliberately NOT here, each because a normal Windows process still works
// without it AND leaving it out keeps it available as an operator lever --
// an unreserved key is one a spec may set:
//
//   - TEMP / TMP -- GetTempPath falls back to %USERPROFILE% and then the
//     Windows directory, both of which the child can still resolve, so a
//     temp path exists either way. Leaving these out is the ACTIVE choice:
//     "put this model's scratch space on the big disk, not on C:" is a
//     legitimate per-spec need on a machine that downloads multi-gigabyte
//     weights, and reserving them would take that away.
//   - LOCALAPPDATA's roaming sibling APPDATA -- the roaming CONFIG root. No
//     model server keeps its caches there (roaming them across machines is
//     the opposite of what a weights cache wants), and the two consumers
//     checked read LOCALAPPDATA. Revisit if a real one is found; until then
//     a spec can still set it.
//   - PATHEXT -- only completes an EXTENSIONLESS command name. A spec's
//     binary is an absolute path, and os/exec resolves it in the AGENT's
//     process against the AGENT's environment, never against cmd.Env; both
//     cmd.exe and Go's LookPath fall back to a built-in
//     ".COM;.EXE;.BAT;.CMD" when it is unset.
//   - COMSPEC -- needed only by a child that shells out through the CRT's
//     system(), which falls back to finding cmd.exe on PATH, and the PATH we
//     pass contains System32.
//   - NUMBER_OF_PROCESSORS / PROCESSOR_ARCHITECTURE -- informational copies
//     of what GetSystemInfo and GetNativeSystemInfo report directly. Nothing
//     needs the environment copy to function.
//   - HOMEDRIVE / HOMEPATH -- the pre-USERPROFILE pair, consulted only as a
//     fallback when USERPROFILE is absent, which after this change it no
//     longer is. Worth naming explicitly: because HOME is reserved in ANY
//     spelling, this pair (or the tool's own variable -- HF_HOME,
//     HF_HUB_CACHE, XDG_CACHE_HOME, LLAMA_CACHE, none of them reserved) is
//     what a Windows operator has left to point a child at a different home
//     or cache. The reservation must not close the last door.
var baseEnvNames = []string{
	"PATH",
	"HOME",
	"USERPROFILE",
	"LOCALAPPDATA",
	"SYSTEMROOT",
	"WINDIR",
}

// GPUVendor names the GPU stack the agent actually found on THIS host. It is
// a hardware fact, discovered locally, never a gateway-supplied value: main.go
// derives it from collector.DetectGPUCollectors(), which probes for
// nvidia-smi, then rocm-smi, then Apple's ioreg, in that fixed order and keeps
// whichever reports Available() first. The string values are exactly
// collector.GPUCollector.Name()'s three values, so ParseGPUVendor can map one
// to the other without a second naming convention to keep in sync.
type GPUVendor string

const (
	// GPUVendorNone is a host with no GPU stack the agent recognises -- a
	// CPU-only machine, an Intel/oneAPI box, or a vendor added to the
	// collectors later but not here. Never an error: see VisibleDevicesVar.
	GPUVendorNone   GPUVendor = ""
	GPUVendorNVIDIA GPUVendor = "nvidia"
	GPUVendorAMD    GPUVendor = "amd"
	GPUVendorApple  GPUVendor = "apple"
)

// ParseGPUVendor maps a collector.GPUCollector.Name() to a GPUVendor,
// returning GPUVendorNone for any name this package does not know. Lives here
// rather than in main.go so the mapping is covered by this package's tests
// instead of by a func in package main that nothing exercises.
func ParseGPUVendor(name string) GPUVendor {
	switch GPUVendor(name) {
	case GPUVendorNVIDIA:
		return GPUVendorNVIDIA
	case GPUVendorAMD:
		return GPUVendorAMD
	case GPUVendorApple:
		return GPUVendorApple
	default:
		return GPUVendorNone
	}
}

// VisibleDevicesVar returns the environment variable that restricts a child
// to a chosen set of cards on vendor's stack, or "" when this agent has
// nothing to set.
//
// NVIDIA -> CUDA_VISIBLE_DEVICES. The universal one; every CUDA runtime, and
// therefore llama.cpp/vLLM/ollama on NVIDIA, honours it.
//
// AMD -> ROCR_VISIBLE_DEVICES, and DELIBERATELY NOTHING ELSE. Do not "fix"
// this by also setting HIP_VISIBLE_DEVICES: the two are not synonyms, they are
// STACKED filters. ROCR_VISIBLE_DEVICES is read by the ROCr runtime and
// selects from the machine's real cards; HIP_VISIBLE_DEVICES is read one layer
// up by HIP and selects from WHAT ROCR ALREADY LEFT VISIBLE. Setting both to
// the same host list is the classic double-filter trap -- with
// ROCR_VISIBLE_DEVICES=2,3 the HIP layer sees two devices numbered 0,1, so a
// simultaneous HIP_VISIBLE_DEVICES=2,3 selects two devices that do not exist
// and the child comes up with NO usable GPU at all. One level, the lowest one,
// is the whole of the correct answer.
//
// Apple and GPUVendorNone -> "", and that is a SUCCESSFUL no-op, never an
// error. Apple's unified memory has no per-card visibility concept to
// constrain (there is one integrated GPU and Metal always sees it), and a host
// with no recognised stack has nothing to name. A spec that asks for device
// pinning on such a host simply gets a child with no visibility variable --
// exactly the environment it would have received before this option existed.
func VisibleDevicesVar(vendor GPUVendor) string {
	switch vendor {
	case GPUVendorNVIDIA:
		return "CUDA_VISIBLE_DEVICES"
	case GPUVendorAMD:
		return "ROCR_VISIBLE_DEVICES"
	case GPUVendorApple, GPUVendorNone:
		return ""
	default:
		return ""
	}
}

// visibleDevicesOwnedVars is the set of environment variables the
// SetVisibleDevices option OWNS: a spec may not hand-set any of them while the
// option is on (ExpandPlaceholders refuses the pair outright -- trap 3).
//
// It is deliberately VENDOR-INDEPENDENT, i.e. the same three names are refused
// on every host, even though only one of them would ever be SET on any given
// one. Two reasons, and the second is the load-bearing one:
//
//  1. The portal enforces the identical rule at save time and cannot know the
//     agent's vendor (the gateway is OS- and hardware-agnostic; the config
//     document is authored before anyone knows which card is in the machine).
//     A vendor-dependent agent rule and a vendor-independent portal rule are
//     two rules that disagree -- the portal would refuse a spec the agent
//     accepts, which is the "two states that look identical and mean different
//     things" defect this option exists to remove, reintroduced in the
//     validator.
//  2. HIP_VISIBLE_DEVICES is in the set even though this agent never sets it,
//     precisely BECAUSE it double-filters what ROCR_VISIBLE_DEVICES already
//     filtered (see VisibleDevicesVar). A hand-set HIP_VISIBLE_DEVICES
//     alongside agent-managed ROCR filtering is the trap in its purest form,
//     so it must be refused rather than merged.
//
// NOT in the set, and that is also deliberate: ONEAPI_DEVICE_SELECTOR,
// GPU_DEVICE_ORDINAL and any other runtime-specific selector. This agent
// neither sets nor filters through those, so an operator combining the
// checkbox with a hand-written ONEAPI_DEVICE_SELECTOR (built from
// ${HOST_GPU_IDS}) is composing two independent things, not contradicting
// itself -- that composition is the documented escape hatch and must keep
// working.
var visibleDevicesOwnedVars = []string{
	"CUDA_VISIBLE_DEVICES",
	"ROCR_VISIBLE_DEVICES",
	"HIP_VISIBLE_DEVICES",
}

// hostGPUIDs renders spec.GPUs as the comma-separated list of HOST GPU
// indices, ascending and deduplicated -- the value both SetVisibleDevices and
// ${HOST_GPU_IDS} emit. Empty string when the spec declares no GPUs; every
// caller treats that as a refusal rather than a value (trap 1).
//
// HOST indices, always. These are the numbers the AGENT's own nvidia-smi /
// rocm-smi report and the numbers the gateway's per-GPU budgets and
// measurement rows are keyed by. They are NOT the numbers the child will see:
// a child launched with CUDA_VISIBLE_DEVICES=3,4 enumerates its two devices as
// 0 and 1. That renumbering is trap 2, and it is why the placeholder is named
// HOST_GPU_IDS and not GPU_IDS.
//
// Sorted ascending rather than kept in the document's order because the order
// is not the operator's to begin with: the gateway's RuntimeSpecGPUs reads the
// rows back `order by gpu_index`, so a hand-chosen order never survives a save
// anyway. Emitting a stable ascending list makes the value a pure function of
// the declared SET, which is what the whole feature reasons about -- and it
// keeps the child's device 0 predictable (the lowest declared host index)
// instead of dependent on row order nobody controls.
//
// Deduplicated because a duplicate index is not merely redundant: CUDA stops
// parsing the list at the first invalid or repeated entry, so
// CUDA_VISIBLE_DEVICES=1,1,2 silently yields ONE visible device, not three.
// The gateway already refuses a duplicate index at save time; a file-mode
// document is hand-written and has no such gate.
func hostGPUIDs(spec Spec) string {
	if len(spec.GPUs) == 0 {
		return ""
	}
	indices := make([]int, 0, len(spec.GPUs))
	seen := make(map[int]bool, len(spec.GPUs))
	for _, g := range spec.GPUs {
		if seen[g.Index] {
			continue
		}
		seen[g.Index] = true
		indices = append(indices, g.Index)
	}
	sort.Ints(indices)
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = strconv.Itoa(idx)
	}
	return strings.Join(parts, ",")
}

// LocalPolicy is the agent-operator-controlled counterweight to the
// gateway-supplied Spec. The gateway decides WHEN and HOW a process runs
// (binary, args, env, work_dir); LocalPolicy, populated from the agent's OWN
// config (env var or local file, never from the gateway), decides WHETHER it
// may run AT ALL.
type LocalPolicy struct {
	// AllowedBinaries lists the absolute paths a spec's Binary must exactly
	// match to be permitted. EMPTY (the default, meaning the operator has
	// configured nothing) means NOTHING may start -- a deliberate hard
	// refusal, not a permissive default: a freshly provisioned agent must
	// not execute whatever the gateway happens to ask for.
	AllowedBinaries []string
	// AllowedDirs lists permitted work_dir prefixes. EMPTY means any
	// work_dir is permitted -- unlike AllowedBinaries, an operator who does
	// not care about work_dir containment is not forced to enumerate one.
	AllowedDirs []string
}

// Permit reports whether spec may be launched under p: nil when permitted,
// otherwise an error naming the violated rule. The error text names only
// the binary path or work_dir and the rule violated -- never a value from
// spec.Env or spec.Args -- so it is always safe to log verbatim or surface
// as a Status.LastError.Message alongside StateNotPermitted.
func (p LocalPolicy) Permit(spec Spec) error {
	if len(p.AllowedBinaries) == 0 {
		return fmt.Errorf("runtime: binary allowlist is empty, refusing to start %q (configure the agent's runtime_allowed_binaries / OP_AGENT_RUNTIME_ALLOWED_BINARIES)", spec.Binary)
	}
	// Absolute paths only -- this also closes the concrete bypass where a
	// config file's runtime_allowed_binaries contains an empty string (e.g.
	// [""]): resolveStringList only drops empty entries on the env path, so
	// a file-sourced [""] survives as a non-empty allowlist that would
	// otherwise match a spec with Binary: "" verbatim.
	if !filepath.IsAbs(spec.Binary) {
		return fmt.Errorf("runtime: binary %q must be an absolute path", spec.Binary)
	}
	binaryAllowed := false
	for _, b := range p.AllowedBinaries {
		if !filepath.IsAbs(b) {
			continue // a non-absolute allowlist entry can never permit anything
		}
		if spec.Binary == b {
			binaryAllowed = true
			break
		}
	}
	if !binaryAllowed {
		return fmt.Errorf("runtime: binary %q is not in the allowed-binaries list", spec.Binary)
	}

	if len(p.AllowedDirs) == 0 {
		return nil
	}
	// An empty work_dir gets its own message. It falls out of withinDir as
	// "not contained" (correctly -- the child would inherit the AGENT's
	// working directory, which is typically "/" for a service and is
	// certainly not inside a permitted model directory), but the generic
	// wording rendered as `work_dir "" is not within any allowed
	// directory`, which reads like a containment near-miss rather than
	// "the spec never set one". This message surfaces verbatim in the
	// portal as Status.LastError.Message next to StateNotPermitted, so it
	// is the only explanation an operator gets. It names the agent-side
	// setting (as the empty-allowlist message above already does) but
	// deliberately NOT the configured directory VALUES: the allowlist is
	// the agent operator's local filesystem layout, and this text travels
	// upward to the gateway.
	if spec.WorkDir == "" {
		return fmt.Errorf("runtime: spec sets no work_dir, but this agent restricts work directories to %d configured path(s) (runtime_allowed_dirs / OP_AGENT_RUNTIME_ALLOWED_DIRS); set the spec's work_dir to a path inside one of them", len(p.AllowedDirs))
	}
	for _, dir := range p.AllowedDirs {
		if withinDir(spec.WorkDir, allowedDirBase(dir)) { // R4: <dir>/* is an accepted synonym for the bare subtree
			return nil
		}
	}
	return fmt.Errorf("runtime: work_dir %q is not within any allowed directory", spec.WorkDir)
}

// withinDir reports whether candidate, once cleaned, is dir itself or sits
// strictly beneath it. Both sides are run through filepath.Clean first so a
// "../" traversal is resolved before comparison, and containment requires a
// path-separator boundary rather than a bare string prefix -- otherwise
// "/srv/models-evil" would read as inside "/srv/models" merely because the
// raw strings share a prefix.
//
// SYMLINKS ARE DELIBERATELY NOT RESOLVED -- accepted residual risk, recorded
// here so the next reader does not have to rediscover the reasoning (Task 13
// decision; stated operator-side in server-agent/README.md's
// runtime_allowed_dirs entry, which points back at this comment). Fix round
// 1, M5: this used to claim the acceptance was "catalogued in
// docs/architecture/11-risks-and-technical-debt.md §11.4". That section is
// real but has no such entry -- the architecture docs carry no
// runtime-manager content yet at all -- so the citation was a false trail.
// Cite something that exists, or nothing.
//
// The gap: a symlink at, or under, an allowed dir can point outside it, so a
// spec whose work_dir passes this lexical check can still give the child a
// working directory elsewhere on the filesystem.
//
// Why not filepath.EvalSymlinks: that call resolves the path as it exists AT
// CHECK TIME, and os/exec applies cmd.Dir at START time. Anything that can
// rewrite the symlink between those two moments defeats the check while
// making it LOOK enforced -- a textbook TOCTOU window, and a strictly worse
// position than the honest lexical check, because it would invite treating
// containment as a security boundary rather than the operator-convenience
// guard it is. (It would also break the legitimate case of an allowed dir
// that is itself a symlink to a mounted volume, which the lexical check
// accepts as written.)
//
// Why that is acceptable: work_dir containment is defense in depth, not the
// boundary. The boundary is AllowedBinaries -- an exact, absolute-path match
// against a list that comes ONLY from the agent's own local config, never
// from the gateway (LocalPolicy's doc comment). A gateway-supplied spec
// cannot choose WHAT executes; work_dir only influences where an
// already-allowlisted binary resolves relative paths. An attacker who can
// plant symlinks under the operator's model directory already has local
// write access there.
//
// The non-TOCTOU fix would be openat/O_NOFOLLOW-based resolution plus fchdir
// in the child, which os/exec's cmd.Dir cannot express portably; it is a
// platform-specific change well beyond this guard's value. If containment
// ever needs to BE a boundary, that is the direction -- not EvalSymlinks.
func withinDir(candidate, dir string) bool {
	if candidate == "" || dir == "" {
		return false
	}
	cleanCandidate := filepath.Clean(candidate)
	cleanDir := filepath.Clean(dir)
	if cleanCandidate == cleanDir {
		return true
	}
	return strings.HasPrefix(cleanCandidate, cleanDir+string(filepath.Separator))
}

// allowedDirBase reduces a runtime_allowed_dirs entry to the concrete
// directory whose subtree it permits. A bare entry is already a subtree
// (withinDir permits the dir and everything strictly beneath it), so it is
// returned unchanged. A trailing whole-segment wildcard -- "<dir>/*" or
// "<dir>\*" -- is an accepted, EXACTLY EQUIVALENT spelling of that subtree:
// the trailing separator+"*" is dropped and the base is returned. Both
// separators are recognised on EVERY GOOS (this is a Windows deployment and
// CI runs no Windows job, so the backslash form must be handled -- and
// unit-tested -- on a POSIX host); recognising either separator can only make
// the base SHORTER, never longer, so it can never widen the permitted set.
//
// A '*' that is not a whole trailing segment ("name*", "a/*/b", "**") is NOT a
// wildcard here and is returned verbatim; withinDir then treats it as an
// ordinary path character (filepath.Clean leaves it untouched), so the entry
// matches only a real directory literally named that -- nothing useful -- and
// "/srv/models*" does NOT permit "/srv/models-evil". A bare "*" reduces to ""
// so withinDir permits NOTHING (fail closed), never the whole filesystem.
//
// This is the ENTIRETY of R4's wildcard support, and deliberately so: the
// untrusted candidate is never matched against a glob -- there is no glob
// engine. This function only ever SHORTENS the trusted operator config entry
// to a concrete base, which is then handed to the unchanged, audited withinDir
// (filepath.Clean + separator boundary). A '*' therefore never participates in
// matching and can never swallow a separator to jump levels. Mid-path globbing
// is explicitly not supported.
func allowedDirBase(entry string) string {
	if entry == "*" {
		return ""
	}
	if strings.HasSuffix(entry, "/*") || strings.HasSuffix(entry, `\*`) {
		return entry[:len(entry)-2]
	}
	return entry
}

// placeholderPattern matches ANY "${...}" shape, including an empty body
// (${AGENT_ENV:} and ${} both match). ExpandPlaceholders classifies every
// match in a single pass over the ORIGINAL text -- see the classification
// logic inside ExpandPlaceholders's expand closure for the exact rule. Using
// one broad
// pattern (rather than a narrow pattern for the two valid forms plus a
// second pattern re-scanning the substituted result) is what makes that
// single-pass, original-text-only classification possible: the near-miss
// check needs to see every "${...}" occurrence exactly once, before any
// substitution happens, never after.
var placeholderPattern = regexp.MustCompile(`\$\{[^}]*\}`)

// ExpandPlaceholders resolves every ${PORT}, ${MODEL}, ${HOST_GPU_IDS} and
// ${AGENT_ENV:NAME} occurrence in spec.Args and spec.Env values, and builds
// the exact environment the child process will receive.
//
// ${PORT} becomes the decimal port chosen for this process. ${MODEL} becomes
// spec.UpstreamModel, the application-side model name (see the ${MODEL}
// paragraph further down for why it is that one and not spec.Model, and why
// it has no near-miss rule). ${AGENT_ENV:NAME}
// resolves NAME from the agent's own process environment via getenv -- this
// is the only path a secret takes to reach a model process without ever
// being stored in the gateway database or sent over the wire. An unset (or
// empty) NAME is a hard error naming the variable: a missing secret must
// fail loudly at start time, never launch a child with an empty token that
// produces a confusing downstream auth failure instead.
//
// The returned env is os/exec-shaped "KEY=value" strings containing ONLY the
// expanded spec.Env entries plus the baseEnvNames variables taken from the
// agent's own environment (each present only if the agent itself has it) --
// never the agent's full environment, which holds its gateway bearer token
// and every ${AGENT_ENV:...} secret configured for OTHER models. spec.Env
// entries are emitted in sorted-key order so the result is deterministic
// across calls (spec.Env is a Go map with randomized iteration order).
//
// That base is OS-appropriate, not POSIX-only: it carries HOME on unix and
// USERPROFILE/LOCALAPPDATA/SYSTEMROOT/WINDIR on Windows, which is what a
// Windows child needs to resolve a per-user cache directory at all -- and,
// for SYSTEMROOT, to initialise Winsock. baseEnvNames carries the full
// justification, name by name, including what was left out and why.
//
// A spec.Env key matching any baseEnvNames entry -- in ANY case spelling,
// since Windows resolves and deduplicates environment names
// case-insensitively -- is refused outright rather than allowed to override
// the agent-provided base: these are agent-owned, not spec-negotiable. A
// gateway-supplied PATH (or SystemRoot) would let a spec choose where the
// child resolves shared libraries and helper subprocesses -- the same class
// of risk Permit's absolute-binary-path requirement exists to close -- so
// this treats "minimal base" as agent-controlled rather than
// spec-overridable. The operator lever that survives is the tool's own
// variable (HF_HOME, HF_HUB_CACHE, XDG_CACHE_HOME, LLAMA_CACHE, HOMEDRIVE/
// HOMEPATH): none of those is reserved.
//
// A near-miss placeholder -- text shaped like "${...}" that does not exactly
// match ${PORT} or ${AGENT_ENV:NAME} (non-empty NAME), but whose inner text,
// upper-cased, STARTS WITH "PORT" or "AGENT_ENV" -- is a hard error naming
// the malformed token, on the same reasoning as the missing-variable error:
// silently launching with that text as a literal argument produces a
// confusing downstream auth or bind failure instead of an honest refusal
// here. This catches typos such as ${AGENT_ENVV:...}, ${PORTX}, ${PORT_1},
// the lowercase ${port}, and the empty-name ${AGENT_ENV:}.
//
// The check is a PREFIX match on the classification, not a substring
// (Contains) check -- and it runs during a SINGLE pass over the ORIGINAL
// text, never over the substituted result. Both properties are load-bearing
// fixes from a prior round that got this rule wrong:
//
//   - Prefix instead of Contains: a substring check refuses every
//     legitimate token that merely CONTAINS "PORT" or "AGENT_ENV" anywhere
//     -- ${TRANSPORT}, ${EXPORT_DIR}, ${REPORT_INTERVAL}, ${IMPORT_PATH},
//     ${SUPPORT_EMAIL}, ${MY_AGENT_ENVIRONMENT} -- breaking the arbitrary
//     "${...}" pass-through a model server's own templating syntax relies
//     on. A prefix match accepts all of these while still catching every
//     near-miss listed above, because a near-miss is always a malformed
//     PORT or AGENT_ENV token, never an unrelated word that happens to
//     embed one.
//   - Original text, not substituted result: classifying post-substitution
//     text means a RESOLVED secret whose value happens to contain a
//     "${...}"-shaped substring (plausible for a JSON blob or connection
//     string) gets misclassified as a near-miss and that literal fragment
//     of the secret is echoed into the error message -- exactly what this
//     file exists to prevent. Classifying each match during the single
//     ReplaceAllStringFunc pass over the ORIGINAL argument/env-value text
//     means resolved values are never re-scanned at all.
//
// Accepted, deliberate consequence: a hypothetical ${PORT_RANGE} intended as
// a model server's OWN templating token is refused rather than passed
// through. In this function's context that shape is far more likely a typo
// of ${PORT} than genuine unrelated templating, so failing loudly is the
// right trade -- unlike, say, ${TRANSPORT}, nothing plausible starts with
// "PORT" or "AGENT_ENV" except an attempt at one of these two placeholders.
//
// ${MODEL} is the THIRD token, and it is deliberately NOT part of that
// near-miss rule -- exact match only. It resolves to spec.UpstreamModel, the
// APPLICATION-side model name (the owning mapping's app_model_name), so a
// spec can write ["--alias", "${MODEL}"] or build a path from it. Anything
// else beginning with "MODEL" passes through literally like any other
// unrecognised "${...}".
//
// That asymmetry with ${PORT} is the point, not an oversight. The near-miss
// rule's own justification above is that "nothing plausible starts with PORT
// or AGENT_ENV except an attempt at one of these two placeholders". That
// reasoning simply does not transfer: ${MODEL_PATH}, ${MODELS_DIR},
// ${MODEL_ID} and ${MODEL_NAME} are all plausible tokens an operator wants
// passed through for a model server to expand itself, and a prefix rule on
// MODEL would refuse every one of them -- reintroducing, under a new name,
// exactly the defect a prior round fixed when a containment rule on PORT
// wrongly refused ${TRANSPORT}, ${EXPORT_DIR} and ${IMPORT_PATH}.
//
// The accepted cost, stated so it is a decision and not a surprise: a typo
// (${MDOEL}, or the lowercase ${model}) reaches the child as those literal
// characters instead of erroring. That is the same silent outcome every
// other unrecognised placeholder already has, and it is the cheaper of the
// two mistakes -- an over-eager refusal breaks working specs, a literal
// pass-through breaks only the spec that was already wrong.
//
// An EMPTY spec.UpstreamModel with ${MODEL} present is a hard error, on the
// same reasoning as an unset ${AGENT_ENV:NAME}: substituting "" would launch
// a child with `--alias ""` or a path with a hole in it and fail somewhere
// downstream, confusingly. The error is raised only when the placeholder is
// actually used, so a spec that never mentions ${MODEL} is unaffected by an
// empty upstream_model.
//
// Only spec.UpstreamModel is exposed. The gateway-facing spec.Model has no
// placeholder: it was not asked for, and a second token would have to be
// named so that neither reading is ambiguous (${GATEWAY_MODEL}, never
// ${MODEL_NAME}, which reads as a synonym of ${MODEL}). Adding one later is
// a behaviour change for anyone using that text as literal templating, which
// is the reason to name it right rather than early.
//
// ${HOST_GPU_IDS} is the FOURTH token, exact match only for the same reason as
// ${MODEL} (${GPU_IDS_FILE}, ${HOST_GPU_IDS_JSON} and friends are plausible
// pass-through tokens, and an over-eager prefix rule breaks working specs
// while a literal pass-through breaks only a spec that was already wrong). It
// resolves to hostGPUIDs(spec) -- the spec's own declared GPU indices as the
// HOST sees them, ascending, comma-separated, e.g. "2,3".
//
// It exists as the manual escape hatch beside spec.SetVisibleDevices: an
// operator on a runtime this agent has no vendor mapping for writes
// "ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}" themselves instead of
// hand-copying the index list into a second place where it can drift from the
// GPU rows that drive admission. The two compose: the checkbox sets the
// vendor-appropriate variable, the placeholder builds any other.
//
// THE NAME CARRIES THE INVARIANT ON PURPOSE. These are host indices; the child
// renumbers from zero (trap 2, see the SetVisibleDevices section below), so
// "3,4" here and "3,4" inside the child name different cards. A token called
// ${GPU_IDS} would have read as either. See hostGPUIDs for the ordering and
// deduplication rules.
//
// A spec that uses ${HOST_GPU_IDS} while declaring NO GPUs is a hard error, on
// exactly the ${MODEL}-with-empty-upstream_model reasoning: substituting ""
// would produce "ONEAPI_DEVICE_SELECTOR=level_zero:" or a bare
// "CUDA_VISIBLE_DEVICES=", and an EMPTY visibility value does not mean "no
// restriction" -- it means NOTHING IS VISIBLE. The error fires only where the
// placeholder is actually used.
//
// # SetVisibleDevices: making the GPU list an enforcement, not a declaration
//
// When spec.SetVisibleDevices is set, this function adds ONE agent-owned
// variable to the child's environment: VisibleDevicesVar(vendor) =
// hostGPUIDs(spec). vendor is the agent's own locally-detected hardware, never
// anything the gateway said. On Apple or an unrecognised stack the variable
// name is "" and nothing is added at all -- a successful no-op, not an error.
//
// The four traps this option has to defuse, and where each is handled:
//
//  1. AN EMPTY VALUE IS NOT "NO RESTRICTION" -- IT MEANS "NOTHING VISIBLE".
//     SetVisibleDevices with no GPU rows would emit "CUDA_VISIBLE_DEVICES="
//     and hide every card from the model, which presents as a model that
//     loads onto the CPU (or fails with "no CUDA-capable device is detected")
//     for reasons nothing in the config hints at. Refused here, before any
//     resource is acquired, and refused AGAIN at save time by the portal --
//     both, deliberately. The portal is where an operator gets an error they
//     can act on in the moment, but a file-mode agent
//     (OP_AGENT_RUNTIME_SOURCE=file) never passes through the portal at all,
//     so the portal alone would leave the whole file path unguarded. The
//     refusal here is VENDOR-INDEPENDENT -- an Apple host refuses the
//     combination too, even though it would set nothing -- because one rule
//     on every platform is the only version a reader can check (the same
//     reasoning baseEnvNames and the ${AGENT_ENV:...} case fold state), and
//     because a spec document that is silently fine on the laptop and
//     dangerous on the GPU box is the worst of the available behaviours.
//
//  2. THE CHILD RENUMBERS FROM 0. With CUDA_VISIBLE_DEVICES=3,4 the child
//     enumerates devices 0 and 1, not 3 and 4 -- so any ARGUMENT that names a
//     device number (--main-gpu, --tensor-split, --device, CUDA_DEVICE_ORDER
//     reasoning) is expressed in the CHILD's numbering from then on, while
//     the spec's GPU rows, the budgets and the measurements stay in the
//     host's. This is not enforceable here -- the agent cannot parse an
//     arbitrary model server's argv -- so it is surfaced where the operator
//     turns the option on: the portal states it at the checkbox, and the
//     architecture doc states it in full. It does NOT make two specs
//     interfere; see the isolation note below.
//
//  3. CONFLICT WITH A HAND-SET ENTRY. SetVisibleDevices together with any
//     visibleDevicesOwnedVars key in spec.Env is refused outright rather than
//     silently resolved in either direction. Both orderings are defensible
//     and that is exactly the problem: a spec where the checkbox is on and
//     CUDA_VISIBLE_DEVICES is also typed in looks identical whichever value
//     wins, so an operator cannot tell from the config which cards the model
//     is on. Refusing turns an invisible ambiguity into a message. Checked
//     case-insensitively, like every other env-key rule here, because Windows
//     resolves environment names case-insensitively and os/exec deduplicates
//     them that way.
//
//  4. HOST INDICES, NOT CHILD INDICES. The emitted value is hostGPUIDs(spec)
//     -- the spec's declared indices as the host sees them -- and so is
//     ${HOST_GPU_IDS}. Pinned in that token's name, in hostGPUIDs's doc, and
//     by TestExpandPlaceholdersHostGPUIDsAreHostIndices.
//
// ISOLATION IS STRUCTURAL, NOT ADDED. Every value above is computed from the
// spec passed to THIS call, and this function returns a fresh []string that
// becomes exactly one exec.Cmd.Env. There is no shared or process-wide state
// to leak between specs, so two specs on disjoint GPU sets running
// concurrently each receive only their own list by construction -- pinned
// against the real two-process path by
// TestManagerVisibleDevicesIsPerSpecIsolated rather than assumed.
//
// A spec with SetVisibleDevices FALSE takes none of these paths and receives a
// byte-identical environment to the one it received before this option existed
// (TestExpandPlaceholdersVisibleDevicesOffIsUnchanged).
//
// Arbitrary OTHER "${...}" text -- a model server's own templating syntax --
// still passes through untouched; this function owns only these four tokens.
// Provenance is recorded, not reconstructed. expandSpec below returns, beside
// the argv and environment the child actually receives, the exact byte spans
// of each result string that came from an ${AGENT_ENV:NAME} substitution --
// which is the one thing only this function can know, because it is the code
// that performed the substitution. command.go turns that into the masked,
// reportable ResolvedCommand; see its doc for why span-level provenance is
// the only honest basis for masking a resolved command.
func ExpandPlaceholders(spec Spec, port int, vendor GPUVendor, getenv func(string) string) (args []string, env []string, err error) {
	ex, err := expandSpec(spec, port, vendor, getenv)
	if err != nil {
		return nil, nil, err
	}
	return ex.args, ex.env, nil
}

// secretSpan is one byte range inside ONE expanded string that came from an
// ${AGENT_ENV:NAME} substitution, together with the variable name that
// supplied it.
//
// It exists so that masking can be a statement about PROVENANCE rather than
// about content. The alternative -- searching a resolved string for the
// secret's value -- is not merely less precise, it is actively wrong: a
// one-character secret would mask every occurrence of that character in
// "--port 54331", and a secret that happens to equal a substring of a
// legitimate value would silently swallow it. Recording the span at the moment
// of substitution is exact by construction and costs one integer pair.
//
// Offsets are into the EXPANDED string, and for an env entry they are into the
// final "KEY=value" form (expandSpec shifts them by len(key)+1), so a consumer
// never needs to know how the string was assembled.
type secretSpan struct {
	start int
	end   int
	name  string
}

// expandedSpec is expandSpec's full result: what the child receives, plus the
// provenance a reportable view of it needs.
//
// argSpans/envSpans are index-aligned with args/env (one entry per string,
// possibly nil). envFromSpec is index-aligned with env and marks the entries
// whose VALUE came from spec.Env, as opposed to the agent-provided base block
// (baseEnvNames) and the agent-computed GPU visibility variable. That
// distinction is not cosmetic: it is exactly the line the upward report draws
// when it masks env values, and ResolvedCommand reuses it rather than
// inventing a second, drifting rule.
type expandedSpec struct {
	args        []string
	env         []string
	argSpans    [][]secretSpan
	envSpans    [][]secretSpan
	envFromSpec []bool
}

// expandSpec is ExpandPlaceholders' worker. Everything in ExpandPlaceholders'
// doc comment describes this function; the exported wrapper exists only
// because most callers want nothing but the argv and environment.
//
// The single pass over the ORIGINAL text, and the classification order inside
// it, are unchanged from the version that used ReplaceAllStringFunc -- both
// properties are load-bearing fixes from prior rounds (see the doc comment).
// The rewrite to an index walk is what makes the substitution's byte offsets
// observable; it changes no classification and no error. A failing match
// returns immediately rather than setting a first-error and walking on, which
// yields the identical error because the walk was, and is, left to right.
func expandSpec(spec Spec, port int, vendor GPUVendor, getenv func(string) string) (expandedSpec, error) {
	gpuIDs := hostGPUIDs(spec)

	expand := func(s string) (string, []secretSpan, error) {
		matches := placeholderPattern.FindAllStringIndex(s, -1)
		if len(matches) == 0 {
			return s, nil, nil
		}
		var b strings.Builder
		var spans []secretSpan
		copied := 0
		for _, m := range matches {
			b.WriteString(s[copied:m[0]])
			copied = m[1]
			match := s[m[0]:m[1]]
			inner := match[2 : len(match)-1] // strip "${" and "}"

			if inner == "PORT" {
				b.WriteString(strconv.Itoa(port))
				continue
			}

			// EXACT match only -- ${MODEL...} anything is NOT a near-miss.
			// See the doc comment above for why the ${PORT} prefix rule
			// deliberately does not extend here.
			if inner == "MODEL" {
				if spec.UpstreamModel == "" {
					return "", nil, fmt.Errorf("${MODEL} cannot be resolved: this spec has no upstream_model (the owning mapping's app_model_name is empty)")
				}
				b.WriteString(spec.UpstreamModel)
				continue
			}

			// EXACT match only, same reasoning as ${MODEL} above. HOST, not
			// child, indices -- the name says so because the digits do not.
			if inner == "HOST_GPU_IDS" {
				if gpuIDs == "" {
					return "", nil, fmt.Errorf("${HOST_GPU_IDS} cannot be resolved: this spec declares no gpus, and an empty visible-devices value means NO device is visible, not every device")
				}
				b.WriteString(gpuIDs)
				continue
			}

			if name, ok := strings.CutPrefix(inner, "AGENT_ENV:"); ok && name != "" {
				// CASE-INSENSITIVE, and that is a security property, not a
				// nicety (S1). The refusal decides whether a
				// gateway-supplied spec may read the agent's OWN namespace;
				// the LOOKUP it guards is os.Getenv, which on Windows is
				// GetEnvironmentVariableW and resolves case-INSENSITIVELY.
				// A case-sensitive guard in front of a case-insensitive
				// lookup is not a guard: ${AGENT_ENV:op_agent_token} walked
				// straight past it and received OP_AGENT_TOKEN -- the
				// agent's gateway bearer credential, which authenticates the
				// certificate endpoint that issues a private key. The value
				// then has a ready path back to the gateway even without
				// network egress from the child, because a model server that
				// echoes its argv (vLLM and llama.cpp both do) and then
				// exits non-zero has that line captured into
				// LastError.StderrTail and reported upward.
				//
				// Deliberately folded on unix too, where a variable named
				// "op_agent_token" IS distinct from "OP_AGENT_TOKEN":
				// refusing that hypothetical is the safe direction, and one
				// rule on every platform is the only version a reader can
				// check. Note the near-miss classifier ten lines below
				// already upper-cases before comparing -- the asymmetry was
				// visible inside this one function.
				if strings.HasPrefix(strings.ToUpper(name), agentOwnEnvPrefix) {
					return "", nil, fmt.Errorf("agent environment variable %q is in the agent's own %s namespace and may not be read via ${AGENT_ENV:...} (this would let a gateway-supplied spec exfiltrate the agent's own credentials)", name, agentOwnEnvPrefix)
				}
				val := getenv(name)
				if val == "" {
					return "", nil, fmt.Errorf("required agent environment variable %q is not set (referenced via ${AGENT_ENV:%s})", name, name)
				}
				// The ONE place a secret enters a resolved string, and
				// therefore the one place its extent can be recorded exactly.
				start := b.Len()
				b.WriteString(val)
				spans = append(spans, secretSpan{start: start, end: b.Len(), name: name})
				continue
			}

			// Not a valid ${PORT} or ${AGENT_ENV:NAME}. Near-miss iff the
			// upper-cased inner text STARTS WITH one of the two supported
			// prefixes -- this also catches the empty-name ${AGENT_ENV:}
			// falling through from the case above. Anything else (including
			// text that merely CONTAINS "PORT" or "AGENT_ENV") passes
			// through untouched -- see the doc comment above for why this
			// must be a prefix match, not Contains.
			upper := strings.ToUpper(inner)
			if strings.HasPrefix(upper, "PORT") || strings.HasPrefix(upper, "AGENT_ENV") {
				return "", nil, fmt.Errorf("malformed placeholder %s looks like a PORT or AGENT_ENV reference but does not match the expected syntax", match)
			}
			b.WriteString(match)
		}
		b.WriteString(s[copied:])
		return b.String(), spans, nil
	}

	expandedArgs := make([]string, len(spec.Args))
	argSpans := make([][]secretSpan, len(spec.Args))
	for i, a := range spec.Args {
		v, spans, expandErr := expand(a)
		if expandErr != nil {
			return expandedSpec{}, fmt.Errorf("runtime: expand arg %d: %w", i, expandErr)
		}
		expandedArgs[i] = v
		argSpans[i] = spans
	}

	envKeys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	// Every baseEnvNames entry is agent-owned, not spec-negotiable: refuse
	// outright rather than let a spec silently override one (see the
	// PATH/HOME decision in the Task 13 fix-round-1 report, and baseEnvNames
	// for why the reservation set IS the base set rather than a copy of it
	// that can drift). This is checked unconditionally, even when the
	// agent's own environment defines none of them, because the rule is
	// about which party controls these keys, not about detecting an actual
	// collision.
	for _, k := range envKeys {
		// Case-insensitive for the same reason as the AGENT_ENV guard above
		// (S2): on Windows the native spelling is "Path", and os/exec
		// deduplicates the child's environment case-insensitively there --
		// so a spec key of "Path" passed this reservation and then WON
		// against the agent-provided PATH, handing a gateway-supplied spec
		// exactly the control over library and helper-binary resolution
		// this rule exists to keep agent-side. Refusing a lowercase "path"
		// on unix, where it is a different variable, is the safe direction.
		// The same fold is what makes the Windows names here meaningful at
		// all: a spec key of "SystemRoot" -- the ONLY spelling a Windows
		// operator would ever type -- is a DLL-resolution hijack of exactly
		// the class this reservation exists to close, and an unfolded
		// comparison against "SYSTEMROOT" would wave it through.
		if slices.Contains(baseEnvNames, strings.ToUpper(k)) {
			return expandedSpec{}, fmt.Errorf("runtime: spec env %q is reserved for the agent-provided base environment and may not be set by a spec", k)
		}
	}

	// Trap 3, and trap 1, in that order -- both BEFORE any value is built, so
	// a spec that can never launch fails on the same validate-before-acquire
	// path as every other refusal here (startProcess's dry run reaches this
	// code before grabEphemeralPort). Both are vendor-independent; see the
	// SetVisibleDevices section of the doc comment for why.
	if spec.SetVisibleDevices {
		for _, k := range envKeys {
			if slices.Contains(visibleDevicesOwnedVars, strings.ToUpper(k)) {
				return expandedSpec{}, fmt.Errorf("runtime: spec env %q conflicts with set_visible_devices: this spec both asks the agent to set the gpu visibility variable and sets one by hand, and the two cannot be resolved into a single unambiguous answer -- turn set_visible_devices off, or remove the env entry", k)
			}
		}
		if gpuIDs == "" {
			return expandedSpec{}, fmt.Errorf("runtime: set_visible_devices is on but this spec declares no gpus: an empty visible-devices value means NO device is visible, not every device, so the child would see no gpu at all -- add the gpu rows this model runs on, or turn set_visible_devices off")
		}
	}

	// Minimal base: baseEnvNames from the agent's OWN environment, each only
	// if actually present -- never fabricated, and never the agent's full
	// environment (see doc comment above). Which of them exist is what makes
	// this list correct on both platform families; see baseEnvNames.
	resultEnv := make([]string, 0, len(envKeys)+len(baseEnvNames)+1)
	// Index-aligned with resultEnv throughout: every append below appends to
	// all three, so a later reader can never be off by one between a value
	// and its provenance.
	resultSpans := make([][]secretSpan, 0, cap(resultEnv))
	fromSpec := make([]bool, 0, cap(resultEnv))
	for _, name := range baseEnvNames {
		if v := getenv(name); v != "" {
			resultEnv = append(resultEnv, name+"="+v)
			resultSpans = append(resultSpans, nil)
			fromSpec = append(fromSpec, false)
		}
	}
	// The agent-owned visibility variable joins the agent-provided base block,
	// not the spec block, because that is what it is: a value this agent
	// computed from local hardware. It cannot collide with a spec key -- the
	// conflict refusal above is what guarantees that, exactly as the
	// baseEnvNames reservation guarantees it for PATH. A "" name means Apple
	// or an unrecognised stack: nothing is appended and the child's
	// environment is byte-identical to the SetVisibleDevices-off one.
	if spec.SetVisibleDevices {
		if name := VisibleDevicesVar(vendor); name != "" {
			resultEnv = append(resultEnv, name+"="+gpuIDs)
			resultSpans = append(resultSpans, nil)
			fromSpec = append(fromSpec, false)
		}
	}
	for _, k := range envKeys {
		v, spans, expandErr := expand(spec.Env[k])
		if expandErr != nil {
			return expandedSpec{}, fmt.Errorf("runtime: expand env %s: %w", k, expandErr)
		}
		// Shift into the final "KEY=value" form so every offset in
		// expandedSpec means the same thing: a range of the string as it
		// appears in env.
		shift := len(k) + 1
		for i := range spans {
			spans[i].start += shift
			spans[i].end += shift
		}
		resultEnv = append(resultEnv, k+"="+v)
		resultSpans = append(resultSpans, spans)
		fromSpec = append(fromSpec, true)
	}

	return expandedSpec{
		args:        expandedArgs,
		env:         resultEnv,
		argSpans:    argSpans,
		envSpans:    resultSpans,
		envFromSpec: fromSpec,
	}, nil
}
