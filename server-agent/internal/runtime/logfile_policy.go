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
	goruntime "runtime"
	"strings"
	"syscall"
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
//     UNC and pseudo-filesystem paths and (for Windows paths) reserved device
//     names and names Win32 silently rewrites, and checks Clean + absolute +
//     separator-boundary containment against the allowed roots: the
//     instance's own ${CONFIG_DIR} and the agent-local runtime_allowed_log_dirs
//     (never gateway-supplied; empty means ${CONFIG_DIR} only).
//   - openLogFile is ANCHORED. It opens the file read-only without blocking,
//     through an os.Root at the root the path matched, so every symlink and
//     junction below the root is followed only while it stays below it and
//     a parent swapped for a link cannot lead out; it then requires a
//     regular file with a single hard link, bound to the name inside the
//     root. The lexical half alone would let a symlink planted in
//     ${CONFIG_DIR} point at /etc/shadow; this half is what makes
//     containment a boundary. The tailer runs it on every open and every
//     reopen after a rotation, because the file behind the path can change.
//
// The instance's ${CONFIG_DIR} is a root only while it is a REAL directory
// chain under config_files_dir (no symlink or junction from the base down):
// openInstanceRoot walks it component by component on directory handles.
// Otherwise a server able to replace its own instance directory with a link
// to /etc would make /etc an allowed root. The same walk guards
// removeConfigDirLogFile, the only place the agent deletes a log file.
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
	// Root is the allowed root OpenPath matched, as configured (for a
	// ${CONFIG_DIR} file the instance config dir), and Rel is OpenPath below
	// it: a local, native-separated relative path. openLogFile opens exactly
	// Rel inside an os.Root anchored at Root, so nothing above Root is
	// resolved again once the root is open.
	Root string
	Rel  string
	// Roots are all the allowed roots of this spec, as configured (instance
	// config dir first, then the applicable runtime_allowed_log_dirs bases).
	// Refusal messages count them; the open uses Root alone.
	Roots []string
	// ConfigBase and ConfigDir are the absolute config_files_dir and the
	// instance's ${CONFIG_DIR} beneath it ("" when the spec has none).
	// ConfigDir counts as a root only while it is a real directory chain
	// under ConfigBase (openInstanceRoot).
	ConfigBase string
	ConfigDir  string
	// InConfigDir reports that OpenPath is a DIRECT child of the instance's
	// ${CONFIG_DIR} (filepath.Dir(OpenPath) == ConfigDir, exactly). Only such
	// a file is the generation's own: the agent removes it before exec
	// (§5.3, removeConfigDirLogFile, which re-verifies the directory at call
	// time) and tails it from offset 0. A file in a SUBDIRECTORY of
	// ${CONFIG_DIR} is still tailable but has InConfigDir false: it is
	// tailed like an external file (from EOF) and never removed, because the
	// subdirectory could be a link the server planted.
	InConfigDir bool
}

// logFileSettingNames is how every containment refusal names the setting.
const logFileSettingNames = "runtime_allowed_log_dirs (OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS)"

// pseudoFSRoots are the Linux kernel pseudo filesystems: their "files" are
// process memory, kernel state and devices, never a server's log. Refused on
// the cleaned path, whatever the allowlist says; the anchored open cannot
// reach them from a root outside them (an absolute link is never followed).
var pseudoFSRoots = []string{"/proc", "/sys", "/dev"}

// windowsNameReason is the refusal reason for windowsNameRefused.
const windowsNameReason = "names a reserved device name, a stream, or a name ending in a dot or space, which Windows does not open as written"

// resolveLogFile is the lexical half of the policy for one declared log file
// of spec (see the file comment). port, topo and configFilesDir feed the
// placeholder expansion exactly as for config-file content, so ${CONFIG_DIR}
// names the same per-instance directory the child sees.
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
func (p LocalPolicy) resolveLogFile(spec Spec, lf LogFile, port int, topo GPUTopology, configFilesDir string) (logFileTarget, error) {
	expanded, err := expandNonSecretText(lf.Path, surfaceLogFilePath, spec, port, topo, configFilesDir)
	if err != nil {
		slog.Warn("runtime: log file path does not expand", "spec", spec.ID, "log_file", lf.Name, "error", err)
		return logFileTarget{}, fmt.Errorf("%w: spec %q log file %q: %w", errLogFileNotPermitted, spec.ID, lf.Name, err)
	}
	r := logFileResolution{spec: spec, lf: lf, expanded: expanded}
	r.base, r.inst, r.instOK = instanceConfigDir(spec, configFilesDir)
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
	base          string // the absolute config_files_dir, when instOK
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
	matched := r.matchRoot(c, roots)
	if matched == "" {
		return logFileTarget{}, r.refuse(fmt.Sprintf("lies outside the spec's ${CONFIG_DIR} and every entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
	}
	rel, err := filepath.Rel(matched, c)
	if err != nil {
		return logFileTarget{}, r.refuse(fmt.Sprintf("does not lie below its root; %s does not apply to it", logFileSettingNames))
	}
	// Windows opens "NUL" or "x.log." as something other than the name says;
	// checked on the part below the matched root, which the spec chose.
	if goruntime.GOOS == "windows" && windowsNameRefused(rel) {
		return logFileTarget{}, r.refuse(windowsNameReason)
	}
	if !filepath.IsLocal(rel) {
		return logFileTarget{}, r.refuse(fmt.Sprintf("does not lie below its root; %s does not apply to it", logFileSettingNames))
	}
	return r.target(c, matched, rel, r.withInst(roots)), nil
}

// matchRoot returns the allowed root the cleaned native path c lies strictly
// inside: the instance config dir when there is one and c is in it, otherwise
// the first of roots that contains c, in their configured order; "" when none
// does.
func (r logFileResolution) matchRoot(c string, roots []string) string {
	if r.instOK && strictlyWithinDir(c, r.inst) {
		return r.inst
	}
	for _, root := range roots {
		if strictlyWithinDir(c, root) {
			return root
		}
	}
	return ""
}

func (r logFileResolution) resolveWSL(configFilesDir string, winRoots []string, distroEntries int) (logFileTarget, error) {
	// A backslash or a colon means different things on the two sides of the
	// boundary (wslPathToWindows' doc), so neither is mapped.
	if !strings.HasPrefix(r.expanded, "/") || hasTwoLeadingSeparators(r.expanded) || strings.ContainsAny(r.expanded, `\:`) {
		return logFileTarget{}, r.refuse("is not an absolute POSIX path without backslashes or colons, which a wsl spec's log file path must be")
	}
	c := path.Clean(r.expanded)
	all := r.withInst(winRoots)
	if t, ok, err := r.resolveWSLConfigDir(configFilesDir, c, all); ok {
		return t, err
	}
	if win, ok := wslPathToWindows(c); ok {
		return r.resolveWSLDrive(win, winRoots, all)
	}
	return logFileTarget{}, r.refuse(fmt.Sprintf("is a path inside the wsl distro; only ${CONFIG_DIR} and /mnt/<drive> paths of a wsl spec are tailed in this version, even under a wsl:// entry of %s (%d wsl:// entries name this distro)", logFileSettingNames, distroEntries))
}

// resolveWSLConfigDir is resolveWSL's first branch: the cleaned distro path c
// inside ${CONFIG_DIR} as the distro sees it (windowsPathToWSL of
// configFilesDir, joined with spec.ID), mapped back to the native instance
// directory. ok is false -- and nothing is decided or logged -- when the spec
// has no instance config dir, configFilesDir has no distro form, the instance
// dir would be the distro base itself, or c does not lie below it; resolveWSL
// then tries its other branches. With ok true the result is final: the
// accepted target, or a refusal (Windows naming rules, which apply on every
// GOOS because the file lives on the Windows side, or the re-asserted native
// containment).
func (r logFileResolution) resolveWSLConfigDir(configFilesDir, c string, all []string) (t logFileTarget, ok bool, err error) {
	if !r.instOK {
		return logFileTarget{}, false, nil
	}
	dir, derr := windowsPathToWSL(configFilesDir)
	if derr != nil {
		return logFileTarget{}, false, nil
	}
	wslInst := path.Join(dir, r.spec.ID)
	rel, below := strings.CutPrefix(c, wslInst+"/")
	if !below || wslInst == path.Clean(dir) {
		return logFileTarget{}, false, nil
	}
	// The file lives on the Windows side, so Windows naming rules
	// apply on every GOOS.
	if windowsNameRefused(rel) {
		return logFileTarget{}, true, r.refuse(windowsNameReason)
	}
	open := filepath.Join(r.inst, filepath.FromSlash(rel))
	// Re-asserted on the native path rather than assumed, as the last
	// line of defence for the containment property.
	if !strictlyWithinDir(open, r.inst) {
		return logFileTarget{}, true, r.refuse(fmt.Sprintf("does not map into the spec's ${CONFIG_DIR}; %s does not apply to it", logFileSettingNames))
	}
	return r.target(open, r.inst, filepath.FromSlash(rel), all), true, nil
}

// resolveWSLDrive is resolveWSL's /mnt/<drive> branch: win is the Windows
// path the distro path maps to, checked against the NATIVE entries (winRoots,
// in their configured order), because the file lives there. Windows naming
// rules apply below the matched root.
func (r logFileResolution) resolveWSLDrive(win string, winRoots, all []string) (logFileTarget, error) {
	for _, root := range winRoots {
		if rel, ok := relWithinWindowsDir(win, root); ok {
			if windowsNameRefused(rel) {
				return logFileTarget{}, r.refuse(windowsNameReason)
			}
			return r.target(win, root, filepath.FromSlash(rel), all), nil
		}
	}
	return logFileTarget{}, r.refuse(fmt.Sprintf("is a /mnt/<drive> path outside the spec's ${CONFIG_DIR} and every native entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
}

// target builds the accepted target for the agent-side path openPath, which
// lies at rel below the allowed root root.
func (r logFileResolution) target(openPath, root, rel string, roots []string) logFileTarget {
	t := logFileTarget{Name: r.lf.Name, OpenPath: openPath, Root: root, Rel: rel, Roots: roots}
	if r.instOK {
		t.ConfigBase, t.ConfigDir = r.base, r.inst
		t.InConfigDir = filepath.Dir(openPath) == r.inst
	}
	return t
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
// planSpecConfigFiles writes to, together with that absolute base. ok is false
// when there is none -- no config_files_dir, or a spec ID that does not name a
// directory strictly inside it -- and then nothing lies in it.
func instanceConfigDir(spec Spec, configFilesDir string) (base, dir string, ok bool) {
	if configFilesDir == "" {
		return "", "", false
	}
	base, err := filepath.Abs(configFilesDir)
	if err != nil {
		return "", "", false
	}
	dir = filepath.Join(base, spec.ID)
	if !strictlyWithinDir(dir, base) {
		return "", "", false
	}
	return base, dir, true
}

// errInstanceDirNotReal is the refusal cause of an instance config dir that
// is not a real directory chain under config_files_dir.
var errInstanceDirNotReal = fmt.Errorf("%w: the instance config dir is not a real directory under config_files_dir", errLogFileNotPermitted)

// openInstanceRoot opens the instance config dir as an os.Root WITHOUT
// following anything below the base. It opens the base (operator
// configuration, which may itself be a link, such as a mounted volume), then
// walks down one component at a time, each step anchored at the directory
// handle of the step before: the component is Root.Lstat'ed (a symlink, a
// junction or any other reparse point is refused), opened with
// Root.OpenRoot, and the opened directory must be os.SameFile with the one
// just Lstat'ed. A server that swaps a component for a link between the two
// calls gets a refusal; once a handle is verified, later swaps of its name do
// not move it. The part below the base is reachable by the server, so a link
// there must not turn another directory (another instance's, or /etc) into
// this instance's root. A missing component is an fs.ErrNotExist error; a
// refusal wraps errLogFileNotPermitted (errInstanceDirNotReal).
func openInstanceRoot(base, dir string) (*os.Root, error) {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%w: the instance config dir is not inside config_files_dir", errLogFileNotPermitted)
	}
	cur, err := os.OpenRoot(base)
	if err != nil {
		return nil, openErr(err)
	}
	for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
		next, err := openRealSubdir(cur, seg)
		_ = cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openRealSubdir is one step of openInstanceRoot: the single component seg
// of parent, which must be a real directory, opened as an os.Root.
func openRealSubdir(parent *os.Root, seg string) (*os.Root, error) {
	fi, err := parent.Lstat(seg)
	if err != nil {
		return nil, openErr(err)
	}
	if !isRealDir(fi) {
		return nil, errInstanceDirNotReal
	}
	next, err := parent.OpenRoot(seg)
	if err != nil {
		return nil, openErr(err)
	}
	if nfi, err := next.Stat("."); err != nil || !os.SameFile(fi, nfi) {
		_ = next.Close()
		return nil, errInstanceDirNotReal
	}
	return next, nil
}

// isRealDir reports a directory that is nothing else: not a symlink, and not a
// junction or other reparse point. Go reports a junction as ModeIrregular on
// Windows; isReparsePoint checks the attribute itself, so the answer does not
// rest on that mapping alone.
func isRealDir(fi fs.FileInfo) bool {
	return fi.Mode().Type() == fs.ModeDir && !isReparsePoint(fi)
}

// removeConfigDirLogFile removes target's file before exec (§5.3), the only
// deletion the log-file feature performs. It refuses (errLogFileNotPermitted)
// unless target is a direct child of the instance config dir and that dir is
// a real directory chain under config_files_dir, re-verified at call time.
//
// The removal runs on a directory handle, not a path: the instance dir is
// opened by openInstanceRoot, which verifies every component below the base
// as a real directory on the handle it then uses, so a server that swaps its
// instance dir (or a component above it) for a link gets a refusal, never a
// deletion through the link. The leaf is removed without following it
// (Root.Remove of a symlink removes the link), a leaf that is a directory is
// refused, and a missing dir or file is not an error.
func removeConfigDirLogFile(target logFileTarget) error {
	leaf := filepath.Clean(target.OpenPath)
	if !target.InConfigDir || target.ConfigDir == "" || filepath.Dir(leaf) != target.ConfigDir {
		return logRemoveRefusal(target, "is not a direct child of the spec's ${CONFIG_DIR}")
	}
	inst, err := openInstanceRoot(target.ConfigBase, target.ConfigDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // no directory, so no file to remove
	case errors.Is(err, errLogFileNotPermitted):
		return logRemoveRefusal(target, "is not in a real directory under config_files_dir")
	case err != nil:
		return logRemoveRefusal(target, "is in a directory the agent cannot open")
	}
	defer func() { _ = inst.Close() }()
	name := filepath.Base(leaf)
	fi, err := inst.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return openErr(err)
	}
	if fi.IsDir() {
		return logRemoveRefusal(target, "is a directory")
	}
	if err := inst.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return openErr(err)
	}
	return nil
}

func logRemoveRefusal(target logFileTarget, reason string) error {
	slog.Warn("runtime: log file not removed", "log_file", target.Name, "path", target.OpenPath, "reason", reason)
	return fmt.Errorf("%w: log file %q %s, so the agent does not remove it; see the agent log", errLogFileNotPermitted, target.Name, reason)
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

// relWithinWindowsDir is strictlyWithinDir for two Windows drive paths on any
// GOOS; it also returns candidate's slash-separated part below dir.
func relWithinWindowsDir(candidate, dir string) (string, bool) {
	c, ok1 := windowsDirKey(candidate)
	d, ok2 := windowsDirKey(dir)
	if !ok1 || !ok2 || c == d {
		return "", false
	}
	rel, ok := strings.CutPrefix(c, strings.TrimSuffix(d, "/")+"/")
	return rel, ok
}

// windowsNameRefused reports whether any component of rel (either separator)
// is a name Windows does not open as written: a reserved DOS device name
// (CON, PRN, AUX, NUL, CONIN$, CONOUT$, COM0-9, LPT0-9 and the superscript
// digits), with or without an extension; a name ending in a dot or a space,
// which Win32 strips (so "x.log." opens "x.log"); and a colon, which names an
// alternate data stream. It is pure string logic, so it is tested on every
// GOOS.
func windowsNameRefused(rel string) bool {
	for seg := range strings.FieldsFuncSeq(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			return true
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") || strings.ContainsRune(seg, ':') {
			return true
		}
		stem := seg
		if i := strings.IndexByte(stem, '.'); i >= 0 {
			stem = stem[:i]
		}
		if isReservedWindowsName(strings.ToUpper(strings.TrimRight(stem, " "))) {
			return true
		}
	}
	return false
}

func isReservedWindowsName(upper string) bool {
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if digit, ok := strings.CutPrefix(upper, prefix); ok {
			switch digit {
			case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
				return true
			}
		}
	}
	return false
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
// the policy (see the file comment).
//
// Containment is by construction, not by comparing paths afterwards: an
// os.Root is opened at target.Root (for a ${CONFIG_DIR} file the instance
// dir, through openInstanceRoot's real-directory walk; otherwise the
// configured root, whose own links are operator configuration and are
// followed once), and target.Rel is opened INSIDE it (openInRoot, per
// platform). os.Root walks the relative path one component at a time from the
// root's handle (openat with O_NOFOLLOW on Unix, NtCreateFile relative to the
// parent handle with OBJ_DONT_REPARSE on Windows). It follows a symlink only
// when the link stays below the root, refuses an absolute link, a junction to
// an absolute target and a ".." that climbs above the root, and resolves ".."
// by restarting from the root rather than by opening the parent, so swapping
// a parent directory for a link mid-walk cannot lead outside. The platform
// open never blocks, never hinders the server's writes, renames or deletes,
// and refuses anything but a regular file.
//
// Containment of the opened file still leaves hard links: an inside name of an
// outside file passes every walk. So, after the open (bindOpenedLogFile):
//
//  1. the descriptor's link count must be 1 (an early refusal; 0 means the
//     file lost its last name and is errLogFileVanished);
//  2. target.Rel is observed ONCE more through the same root
//     (statLogFileName: one Root.Stat on Unix, one GetFileInformationByHandle
//     on a handle opened through the root on Windows). That observation must
//     show the descriptor's file and a link count of 1. It is root-anchored,
//     so it re-resolves nothing above the root, and it follows a link below
//     the root only if the link stays below it, so the name it finds is an
//     inside name. An inside name that is this file, on a file with a single
//     name, means no outside name existed when the count was read.
//
// The residual window is honest but narrow: the lookup of the last component
// and the read of the link count are separate steps inside the kernel (or two
// calls on one handle on Windows), so a name can change between them, and a
// directory the walk already holds can be renamed away from below the root
// without the walk noticing (os.Root tracks directories by handle). Exploiting
// either needs a server that can create and remove a hard link to an outside
// file it could not read itself: possible on macOS, and on Linux without
// fs.protected_hardlinks, not where that protection is on.
//
// Classification: a name that is missing in the root, or that names another
// file, or that now leads out of the root, AFTER the open succeeded, is
// errLogFileVanished (an fs.ErrNotExist, like a file not created yet): a
// rotation or a delete in the window, which the tailer retries, and the
// retry's own open refuses a lasting escape. A link count above 1, a file that
// is not regular, and an escape os.Root refuses AT the open wrap
// errLogFileNotPermitted. On a refusal the file is closed. Any other error
// carries the OS cause without the path.
func openLogFile(target logFileTarget) (*os.File, error) {
	if target.Root == "" || !filepath.IsLocal(target.Rel) {
		return nil, logOpenRefusal(target, "does not lie below an allowed root")
	}
	root, err := openLogRoot(target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := openInRoot(root, target)
	if err != nil {
		return nil, err
	}
	if err := bindOpenedLogFile(root, f, target); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// openLogRoot opens the os.Root openLogFile anchors at (see its doc).
func openLogRoot(target logFileTarget) (*os.Root, error) {
	if target.ConfigDir != "" && target.Root == target.ConfigDir {
		root, err := openInstanceRoot(target.ConfigBase, target.ConfigDir)
		if errors.Is(err, errLogFileNotPermitted) {
			return nil, logOpenRefusal(target, "is in an instance config dir that is not a real directory under config_files_dir")
		}
		return root, err
	}
	root, err := os.OpenRoot(target.Root)
	if err != nil {
		return nil, openErr(err)
	}
	return root, nil
}

// classifyInRootErr turns an error of opening or stat'ing target.Rel through
// the root, BEFORE the open succeeded, into the policy's answer: a missing
// name stays an fs.ErrNotExist error, os.Root's refusal of a path that would
// leave the root (and a symlink loop, which os.Root also stops) is a refusal,
// and anything else is the OS cause without the path.
func classifyInRootErr(target logFileTarget, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return openErr(err)
	}
	if isRootEscape(err) {
		return logOpenRefusal(target, "is reached through a link or a path that leaves its allowed root")
	}
	return openErr(err)
}

// isRootEscape reports os.Root's refusal to leave the root. os.Root signals it
// with an unexported error ("path escapes from parent", os/file.go
// errPathEscapes) inside the *PathError, which is the only cause it returns
// that is not a syscall.Errno; ELOOP is counted too (a link loop, or more than
// os.Root's eight links).
func isRootEscape(err error) bool {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return false
	}
	var errno syscall.Errno
	if errors.As(pe.Err, &errno) {
		return errno == syscall.ELOOP
	}
	return true
}

// logFileOpenStage names the moments after the open the test seam can
// interpose at: the real windows in which a server can change the file's
// names while the agent looks.
type logFileOpenStage int

const (
	stageAfterOpen      logFileOpenStage = iota // the file is open; before step 1
	stageAfterLinkCount                         // between steps 1 and 2
)

// logFileOpenSeam, when set, runs at each stage of bindOpenedLogFile. It is a
// TEST SEAM only, nil in production: the tests use it to change the file's
// names at exactly the moments the binding guards.
var logFileOpenSeam func(logFileOpenStage)

func runLogFileOpenSeam(stage logFileOpenStage) {
	if logFileOpenSeam != nil {
		logFileOpenSeam(stage)
	}
}

// errLogFileVanished is the fs.ErrNotExist of a file whose name went away (or
// came to name something else) while openLogFile looked at it.
var errLogFileVanished = fmt.Errorf("runtime: open log file: the file was removed while it was opened: %w", fs.ErrNotExist)

// nameBinding is what ONE observation of target.Rel through the root says,
// compared with the opened file: whether the name leads to the same file as
// the descriptor, whether that is a plain regular file, and how many names the
// file has.
type nameBinding struct {
	same    bool
	regular bool
	nlink   uint64
}

// bindOpenedLogFile runs steps 1 and 2 of openLogFile's doc on the opened f.
func bindOpenedLogFile(root *os.Root, f *os.File, target logFileTarget) error {
	runLogFileOpenSeam(stageAfterOpen)
	n, err := openedLinkCount(f)
	switch {
	case err != nil:
		return logOpenRefusal(target, "has a link count that cannot be read")
	case n == 0:
		return errLogFileVanished
	case n != 1:
		return logOpenRefusal(target, "has more than one hard link")
	}
	runLogFileOpenSeam(stageAfterLinkCount)
	b, err := statLogFileName(root, f, target.Rel)
	switch {
	case err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || isRootEscape(err)):
		// Deleted, rotated, or a parent replaced (by a file, or by a link
		// leading out of the root) after the open.
		return errLogFileVanished
	case err != nil:
		return openErr(err)
	case !b.same:
		// Rotated, or replaced by a directory or a link, after the open.
		return errLogFileVanished
	case !b.regular:
		return logOpenRefusal(target, "is not a regular file")
	case b.nlink != 1:
		return logOpenRefusal(target, "has more than one hard link")
	}
	return nil
}

// logOpenRefusal is the refusal of an opened (or about-to-be-opened) file.
func logOpenRefusal(target logFileTarget, reason string) error {
	slog.Warn("runtime: log file refused at open", "log_file", target.Name, "path", target.OpenPath, "root", target.Root, "reason", reason)
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
