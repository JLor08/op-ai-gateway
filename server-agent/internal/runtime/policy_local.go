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
	binaryAllowed := false
	for _, b := range p.AllowedBinaries {
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

// placeholderPattern matches the two placeholders ExpandPlaceholders
// understands: ${PORT} and ${AGENT_ENV:NAME}. Anything else shaped like
// "${...}" is left untouched -- ExpandPlaceholders only owns these two.
var placeholderPattern = regexp.MustCompile(`\$\{(PORT|AGENT_ENV:[^}]+)\}`)

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
			name := strings.TrimPrefix(inner, "AGENT_ENV:")
			if strings.HasPrefix(name, agentOwnEnvPrefix) {
				firstErr = fmt.Errorf("agent environment variable %q is in the agent's own %s namespace and may not be read via ${AGENT_ENV:...} (this would let a gateway-supplied spec exfiltrate the agent's own credentials)", name, agentOwnEnvPrefix)
				return match
			}
			val := getenv(name)
			if val == "" {
				firstErr = fmt.Errorf("runtime: required agent environment variable %q is not set (referenced via ${AGENT_ENV:%s})", name, name)
				return match
			}
			return val
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
