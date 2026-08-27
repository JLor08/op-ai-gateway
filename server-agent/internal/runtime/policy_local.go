// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"path/filepath"
	"regexp"
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
		if withinDir(spec.WorkDir, dir) {
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
// decision; catalogued in docs/architecture/11-risks-and-technical-debt.md
// §11.4 alongside the other deliberate acceptances).
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

// ExpandPlaceholders resolves every ${PORT} and ${AGENT_ENV:NAME} occurrence
// in spec.Args and spec.Env values, and builds the exact environment the
// child process will receive.
//
// ${PORT} becomes the decimal port chosen for this process. ${AGENT_ENV:NAME}
// resolves NAME from the agent's own process environment via getenv -- this
// is the only path a secret takes to reach a model process without ever
// being stored in the gateway database or sent over the wire. An unset (or
// empty) NAME is a hard error naming the variable: a missing secret must
// fail loudly at start time, never launch a child with an empty token that
// produces a confusing downstream auth failure instead.
//
// The returned env is os/exec-shaped "KEY=value" strings containing ONLY the
// expanded spec.Env entries plus PATH and HOME taken from the agent's own
// environment (present only if the agent itself has them) -- never the
// agent's full environment, which holds its gateway bearer token and every
// ${AGENT_ENV:...} secret configured for OTHER models. spec.Env entries are
// emitted in sorted-key order so the result is deterministic across calls
// (spec.Env is a Go map with randomized iteration order).
//
// A spec.Env key of PATH or HOME is refused outright rather than allowed to
// override the agent-provided base: these two are agent-owned, not
// spec-negotiable. A gateway-supplied PATH would let a spec choose where the
// child resolves shared libraries and helper subprocesses -- the same class
// of risk Permit's absolute-binary-path requirement exists to close -- so
// this treats "minimal base" as agent-controlled rather than
// spec-overridable.
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
// Arbitrary OTHER "${...}" text -- a model server's own templating syntax --
// still passes through untouched; this function owns only these two tokens.
func ExpandPlaceholders(spec Spec, port int, getenv func(string) string) (args []string, env []string, err error) {
	expand := func(s string) (string, error) {
		var firstErr error
		result := placeholderPattern.ReplaceAllStringFunc(s, func(match string) string {
			if firstErr != nil {
				return match
			}
			inner := match[2 : len(match)-1] // strip "${" and "}"

			if inner == "PORT" {
				return strconv.Itoa(port)
			}

			if name, ok := strings.CutPrefix(inner, "AGENT_ENV:"); ok && name != "" {
				if strings.HasPrefix(name, agentOwnEnvPrefix) {
					firstErr = fmt.Errorf("agent environment variable %q is in the agent's own %s namespace and may not be read via ${AGENT_ENV:...} (this would let a gateway-supplied spec exfiltrate the agent's own credentials)", name, agentOwnEnvPrefix)
					return match
				}
				val := getenv(name)
				if val == "" {
					firstErr = fmt.Errorf("required agent environment variable %q is not set (referenced via ${AGENT_ENV:%s})", name, name)
					return match
				}
				return val
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
				firstErr = fmt.Errorf("malformed placeholder %s looks like a PORT or AGENT_ENV reference but does not match the expected syntax", match)
				return match
			}
			return match
		})
		if firstErr != nil {
			return "", firstErr
		}
		return result, nil
	}

	expandedArgs := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		v, expandErr := expand(a)
		if expandErr != nil {
			return nil, nil, fmt.Errorf("runtime: expand arg %d: %w", i, expandErr)
		}
		expandedArgs[i] = v
	}

	envKeys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	// PATH and HOME are agent-owned, not spec-negotiable: refuse outright
	// rather than let a spec silently override them (see the PATH/HOME
	// decision in the Task 13 fix-round-1 report). This is checked
	// unconditionally, even when the agent's own environment has neither,
	// because the rule is about which party controls these keys, not about
	// detecting an actual collision.
	for _, k := range envKeys {
		if k == "PATH" || k == "HOME" {
			return nil, nil, fmt.Errorf("runtime: spec env %q is reserved for the agent-provided base environment and may not be set by a spec", k)
		}
	}

	// Minimal base: PATH/HOME from the agent's OWN environment, only if
	// present. Never the agent's full environment -- see doc comment above.
	resultEnv := make([]string, 0, len(envKeys)+2)
	if v := getenv("PATH"); v != "" {
		resultEnv = append(resultEnv, "PATH="+v)
	}
	if v := getenv("HOME"); v != "" {
		resultEnv = append(resultEnv, "HOME="+v)
	}
	for _, k := range envKeys {
		v, expandErr := expand(spec.Env[k])
		if expandErr != nil {
			return nil, nil, fmt.Errorf("runtime: expand env %s: %w", k, expandErr)
		}
		resultEnv = append(resultEnv, k+"="+v)
	}

	return expandedArgs, resultEnv, nil
}
