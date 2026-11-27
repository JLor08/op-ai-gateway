// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// This file is the agent's path policy for a spec's log_files (#430, design
// §5): the gateway NAMES a file, and the agent alone decides whether it may
// read it. It is the first time the agent reads a file the gateway chose, so
// it is a security boundary, not a convenience guard like work_dir
// containment (withinDir's doc).
//
// Two halves, both required:
//
//   - resolveLogFile is LEXICAL. It expands the path (non-secret placeholders
//     only), maps a WSL spec's path to the agent-side file, refuses device,
//     UNC and pseudo-filesystem paths, and checks Clean + absolute +
//     separator-boundary containment against the allowed roots: the
//     instance's own ${CONFIG_DIR} and the agent-local runtime_allowed_log_dirs
//     (never gateway-supplied; empty means ${CONFIG_DIR} only).
//   - openLogFile is RESOLVED. It opens read-only without blocking, requires a
//     regular file, and checks the opened file's FINAL path -- after every
//     symlink and junction -- against the same roots, each itself resolved.
//     The lexical half alone would let a symlink planted in ${CONFIG_DIR}
//     point at /etc/shadow; this half is what makes containment a boundary.
//     The tailer runs it on every open and every reopen after a rotation,
//     because the file behind the path can change.
//
// Every refusal wraps errLogFileNotPermitted. Its text travels to the gateway,
// so it names the setting and a count, never a path: the agent-local paths go
// to the agent's own log (slog.Warn), the rule every policy refusal follows.

// errLogFileNotPermitted is wrapped by every log-file policy refusal, lexical
// or at open. A missing file is NOT one: it is a plain fs.ErrNotExist error,
// because a server may simply not have created its log yet.
var errLogFileNotPermitted = errors.New("runtime: log file not permitted")

// logFileTarget is one declared log file after resolveLogFile accepted it.
type logFileTarget struct {
	// Name is the declaration's name (the stream id).
	Name string
	// OpenPath is the agent-side path to open: for a WSL spec the native
	// Windows path of the file, never the distro path.
	OpenPath string
	// Roots are the allowed roots of this spec, as configured (instance
	// config dir first, then the applicable runtime_allowed_log_dirs bases):
	// openLogFile requires the opened file's final path strictly inside one
	// of them, each resolved at check time.
	Roots []string
	// InConfigDir reports that OpenPath lies inside the instance's own
	// ${CONFIG_DIR}: the agent removes such a file before exec (§5.3) and
	// never touches any other.
	InConfigDir bool
}

// logFileSettingNames is how every containment refusal names the setting.
const logFileSettingNames = "runtime_allowed_log_dirs (OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS)"

// pseudoFSRoots are the Linux kernel pseudo filesystems: their "files" are
// process memory, kernel state and devices, never a server's log. Refused on
// the cleaned path and again on the resolved one, whatever the allowlist says.
var pseudoFSRoots = []string{"/proc", "/sys", "/dev"}

// resolveLogFile is the lexical half of the policy for one declared log file
// of spec (see the file comment). port, vendor, vendors and configFilesDir
// feed the placeholder expansion exactly as for config-file content, so
// ${CONFIG_DIR} names the same per-instance directory the child sees.
//
// A native spec's path must be absolute after expansion and lie strictly
// inside filepath.Join(abs(configFilesDir), spec.ID) or a native
// runtime_allowed_log_dirs base. A WSL spec's path is an absolute POSIX path
// inside the distro (§5.4), and in this cut it can be only one of two things:
//
//   - inside ${CONFIG_DIR} as the distro sees it (windowsPathToWSL of
//     configFilesDir, joined with spec.ID): mapped back to the native instance
//     directory, which is what is opened;
//   - a /mnt/<drive> path: mapped to its Windows path (wslPathToWindows) and
//     checked against the NATIVE entries, because the file lives there.
//
// Every other distro path is refused, even under a matching wsl:// entry: the
// symlink check through \\wsl.localhost is not verified yet, and until it is
// such a path fails closed rather than skipping that check.
func (p LocalPolicy) resolveLogFile(spec Spec, lf LogFile, port int, vendor GPUVendor, vendors GPUVendors, configFilesDir string) (logFileTarget, error) {
	expanded, err := expandNonSecretText(lf.Path, whatLogFilePath, spec, port, vendor, vendors, configFilesDir)
	if err != nil {
		slog.Warn("runtime: log file path does not expand", "spec", spec.ID, "log_file", lf.Name, "error", err)
		return logFileTarget{}, fmt.Errorf("%w: spec %q log file %q: %w", errLogFileNotPermitted, spec.ID, lf.Name, err)
	}
	r := logFileResolution{spec: spec, lf: lf, expanded: expanded}
	r.inst, r.instOK = instanceConfigDir(spec, configFilesDir)
	native, wsl := splitAllowlist(p.AllowedLogDirs)
	r.nativeEntries = len(native)
	if spec.IsWSL() {
		return r.resolveWSL(configFilesDir, windowsLogRoots(native), len(wsl[spec.WSLDistro()]))
	}
	return r.resolveNative(nativeLogRoots(native))
}

// logFileResolution carries one resolveLogFile call's inputs to its branches.
type logFileResolution struct {
	spec          Spec
	lf            LogFile
	expanded      string
	inst          string // the native instance config dir, when instOK
	instOK        bool
	nativeEntries int
}

func (r logFileResolution) resolveNative(roots []string) (logFileTarget, error) {
	if hasTwoLeadingSeparators(r.expanded) {
		return logFileTarget{}, r.refuse("names a UNC or device path, which is never tailed")
	}
	if !filepath.IsAbs(r.expanded) {
		return logFileTarget{}, r.refuse("is not an absolute path after placeholder expansion")
	}
	c := filepath.Clean(r.expanded)
	if underPseudoFS(c) {
		return logFileTarget{}, r.refuse("lies under a kernel pseudo filesystem (proc, sys or dev), which is never tailed")
	}
	all := r.withInst(roots)
	if r.instOK && strictlyWithinDir(c, r.inst) {
		return logFileTarget{Name: r.lf.Name, OpenPath: c, Roots: all, InConfigDir: true}, nil
	}
	for _, root := range roots {
		if strictlyWithinDir(c, root) {
			return logFileTarget{Name: r.lf.Name, OpenPath: c, Roots: all}, nil
		}
	}
	return logFileTarget{}, r.refuse(fmt.Sprintf("lies outside the spec's ${CONFIG_DIR} and every entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
}

func (r logFileResolution) resolveWSL(configFilesDir string, winRoots []string, distroEntries int) (logFileTarget, error) {
	// A backslash or a colon means different things on the two sides of the
	// boundary (wslPathToWindows' doc), so neither is mapped.
	if !strings.HasPrefix(r.expanded, "/") || hasTwoLeadingSeparators(r.expanded) || strings.ContainsAny(r.expanded, `\:`) {
		return logFileTarget{}, r.refuse("is not an absolute POSIX path without backslashes or colons, which a wsl spec's log file path must be")
	}
	c := path.Clean(r.expanded)
	all := r.withInst(winRoots)
	if r.instOK {
		if dir, err := windowsPathToWSL(configFilesDir); err == nil {
			wslInst := path.Join(dir, r.spec.ID)
			if rel, ok := strings.CutPrefix(c, wslInst+"/"); ok && wslInst != path.Clean(dir) {
				open := filepath.Join(r.inst, filepath.FromSlash(rel))
				// Re-asserted on the native path rather than assumed, as the last
				// line of defence for the containment property.
				if !strictlyWithinDir(open, r.inst) {
					return logFileTarget{}, r.refuse(fmt.Sprintf("does not map into the spec's ${CONFIG_DIR}; %s does not apply to it", logFileSettingNames))
				}
				return logFileTarget{Name: r.lf.Name, OpenPath: open, Roots: all, InConfigDir: true}, nil
			}
		}
	}
	if win, ok := wslPathToWindows(c); ok {
		for _, root := range winRoots {
			if strictlyWithinWindowsDir(win, root) {
				return logFileTarget{Name: r.lf.Name, OpenPath: win, Roots: all}, nil
			}
		}
		return logFileTarget{}, r.refuse(fmt.Sprintf("is a /mnt/<drive> path outside the spec's ${CONFIG_DIR} and every native entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
	}
	return logFileTarget{}, r.refuse(fmt.Sprintf("is a path inside the wsl distro; only ${CONFIG_DIR} and /mnt/<drive> paths of a wsl spec are tailed in this version, even under a wsl:// entry of %s (%d wsl:// entries name this distro)", logFileSettingNames, distroEntries))
}

// withInst prepends the instance config dir, when there is one, to roots.
func (r logFileResolution) withInst(roots []string) []string {
	all := make([]string, 0, len(roots)+1)
	if r.instOK {
		all = append(all, r.inst)
	}
	return append(all, roots...)
}

// refuse logs the refused path to the agent's own log and returns the
// gateway-bound error, which names the spec, the declaration and the reason,
// never a path.
func (r logFileResolution) refuse(reason string) error {
	slog.Warn("runtime: log file path refused", "spec", r.spec.ID, "log_file", r.lf.Name, "path", r.expanded, "reason", reason)
	return fmt.Errorf("%w: spec %q log file %q %s; see the agent log", errLogFileNotPermitted, r.spec.ID, r.lf.Name, reason)
}

// instanceConfigDir is the instance's ${CONFIG_DIR} as an absolute native
// path: filepath.Join(abs(configFilesDir), spec.ID), the directory
// planSpecConfigFiles writes to. ok is false when there is none -- no
// config_files_dir, or a spec ID that does not name a directory strictly
// inside it -- and then nothing lies in it.
func instanceConfigDir(spec Spec, configFilesDir string) (string, bool) {
	if configFilesDir == "" {
		return "", false
	}
	base, err := filepath.Abs(configFilesDir)
	if err != nil {
		return "", false
	}
	dir := filepath.Join(base, spec.ID)
	if !strictlyWithinDir(dir, base) {
		return "", false
	}
	return dir, true
}

// nativeLogRoots reduces the native entries to the bases they permit
// (allowedDirBase), dropping every one that can permit nothing or that must
// not: a relative entry (already warned about at config load), the empty
// base of "*" or "/*", and a UNC base -- a spec never names a UNC path, and a
// link resolving onto a share is refused rather than matched.
func nativeLogRoots(native []string) []string {
	var roots []string
	for _, e := range native {
		b := allowedDirBase(e)
		if b == "" || hasTwoLeadingSeparators(b) || !filepath.IsAbs(b) {
			continue
		}
		roots = append(roots, filepath.Clean(b))
	}
	return roots
}

// windowsLogRoots is nativeLogRoots for the /mnt/<drive> paths of a WSL spec:
// the native entries that are Windows drive paths below a drive root, on
// every GOOS (the agent that runs WSL specs is a Windows agent; the check is
// lexical, so it is testable on the Linux runner). A drive root ("C:\" or the
// "C:" that "C:\*" reduces to) permits nothing, as "/" does for withinDir.
func windowsLogRoots(native []string) []string {
	var roots []string
	for _, e := range native {
		if k, ok := windowsDirKey(allowedDirBase(e)); ok && len(k) > len("c:/") {
			roots = append(roots, allowedDirBase(e))
		}
	}
	return roots
}

// windowsDirKey is a Windows drive path in a comparable form: forward slashes,
// lower-case drive letter, path.Clean'd. Only the drive letter is
// case-folded; the rest compares case-sensitively, like withinDir. ok is
// false for anything that is not drive-absolute.
func windowsDirKey(p string) (string, bool) {
	if len(p) < 3 || p[1] != ':' || !isWindowsSep(p[2]) {
		return "", false
	}
	d := p[0] | 0x20
	if d < 'a' || d > 'z' {
		return "", false
	}
	return path.Clean(string(d) + ":/" + strings.ReplaceAll(p[3:], `\`, "/")), true
}

// strictlyWithinWindowsDir is strictlyWithinDir for two Windows drive paths on
// any GOOS.
func strictlyWithinWindowsDir(candidate, dir string) bool {
	c, ok1 := windowsDirKey(candidate)
	d, ok2 := windowsDirKey(dir)
	return ok1 && ok2 && strings.HasPrefix(c, strings.TrimSuffix(d, "/")+"/") && c != d
}

// strictlyWithinDir is withinDir minus the dir itself: a log file is a file
// beneath a root, never the root.
func strictlyWithinDir(candidate, dir string) bool {
	return withinDir(candidate, dir) && filepath.Clean(candidate) != filepath.Clean(dir)
}

// hasTwoLeadingSeparators reports a UNC or device-namespace path in either
// spelling (`\\server\share`, `\\.\pipe`, `\\?\C:`, "//server/share"), on
// every GOOS: POSIX leaves "//" implementation-defined, so it is refused too.
func hasTwoLeadingSeparators(p string) bool {
	return len(p) >= 2 && isWindowsSep(p[0]) && isWindowsSep(p[1])
}

func underPseudoFS(p string) bool {
	for _, root := range pseudoFSRoots {
		if withinDir(p, root) {
			return true
		}
	}
	return false
}

// openLogFile opens target read-only for tailing and runs the resolved half of
// the policy (see the file comment): the platform open (openLogFileHandle)
// never blocks and never hinders the server's writes, renames or deletes; the
// opened file must be a regular file whose final path lies strictly inside one
// of target.Roots, each resolved the same way, and outside the pseudo
// filesystems. On a refusal the file is closed and the error wraps
// errLogFileNotPermitted; any other error (a missing file above all) carries
// the OS cause without the path.
func openLogFile(target logFileTarget) (*os.File, error) {
	f, final, err := openLogFileHandle(target)
	if err != nil {
		return nil, err
	}
	if reason := checkOpenedLogFile(f, final, target.Roots); reason != "" {
		_ = f.Close()
		return nil, logOpenRefusal(target, final, reason)
	}
	return f, nil
}

// checkOpenedLogFile returns why the opened file f, whose final path is final,
// is refused, or "" when it is permitted.
func checkOpenedLogFile(f *os.File, final string, roots []string) string {
	fi, err := f.Stat()
	if err != nil {
		return "cannot be inspected after opening"
	}
	if !fi.Mode().IsRegular() {
		return "is not a regular file"
	}
	if hasTwoLeadingSeparators(final) || !filepath.IsAbs(final) {
		return "resolves to a UNC, device or relative path"
	}
	if underPseudoFS(final) {
		return "resolves under a kernel pseudo filesystem (proc, sys or dev)"
	}
	for _, root := range roots {
		resolved, err := resolveLogRoot(root)
		if err != nil {
			continue // a root that does not exist contains nothing
		}
		if strictlyWithinDir(final, resolved) {
			return ""
		}
	}
	return "resolves outside every allowed root"
}

// logOpenRefusal is the refusal of an opened (or about-to-be-opened) file.
func logOpenRefusal(target logFileTarget, final, reason string) error {
	slog.Warn("runtime: log file refused at open", "log_file", target.Name, "path", target.OpenPath, "resolved", final, "reason", reason)
	return fmt.Errorf("%w: log file %q %s (allowed: the spec's ${CONFIG_DIR} and %s, %d roots); see the agent log", errLogFileNotPermitted, target.Name, reason, logFileSettingNames, len(target.Roots))
}

// openErr strips the path from an OS error before it travels anywhere: the
// cause stays wrapped, so errors.Is(err, fs.ErrNotExist) still holds.
func openErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("runtime: open log file: %w", pe.Err)
	}
	return fmt.Errorf("runtime: open log file: %w", err)
}
