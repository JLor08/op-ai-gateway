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
//   - openLogFile is RESOLVED. It opens read-only without blocking, requires a
//     regular file with a single hard link, and checks the opened file's
//     FINAL path -- after every symlink and junction -- against the same
//     roots, each itself resolved. The lexical half alone would let a symlink
//     planted in ${CONFIG_DIR} point at /etc/shadow; this half is what makes
//     containment a boundary. The tailer runs it on every open and every
//     reopen after a rotation, because the file behind the path can change.
//
// The instance's ${CONFIG_DIR} is a root only while it is a REAL directory
// chain under config_files_dir (no symlink or junction from the base down):
// only the base is resolved, the instance part is joined to it. Otherwise a
// server able to replace its own instance directory with a link to /etc
// would make /etc an allowed root. The same condition guards
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
	// Roots are the allowed roots of this spec, as configured (instance
	// config dir first, then the applicable runtime_allowed_log_dirs bases):
	// openLogFile requires the opened file's final path strictly inside one
	// of them, each resolved at check time.
	Roots []string
	// ConfigBase and ConfigDir are the absolute config_files_dir and the
	// instance's ${CONFIG_DIR} beneath it ("" when the spec has none).
	// ConfigDir counts as a root only while it is a real directory chain
	// under ConfigBase (resolveInstanceConfigDir).
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
// the cleaned path and again on the resolved one, whatever the allowlist says.
var pseudoFSRoots = []string{"/proc", "/sys", "/dev"}

// windowsNameReason is the refusal reason for windowsNameRefused.
const windowsNameReason = "names a reserved device name, a stream, or a name ending in a dot or space, which Windows does not open as written"

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
	expanded, err := expandNonSecretText(lf.Path, surfaceLogFilePath, spec, port, vendor, vendors, configFilesDir)
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
	matched := ""
	if r.instOK && strictlyWithinDir(c, r.inst) {
		matched = r.inst
	} else {
		for _, root := range roots {
			if strictlyWithinDir(c, root) {
				matched = root
				break
			}
		}
	}
	if matched == "" {
		return logFileTarget{}, r.refuse(fmt.Sprintf("lies outside the spec's ${CONFIG_DIR} and every entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
	}
	// Windows opens "NUL" or "x.log." as something other than the name says;
	// checked on the part below the matched root, which the spec chose.
	if goruntime.GOOS == "windows" {
		if rel, err := filepath.Rel(matched, c); err != nil || windowsNameRefused(rel) {
			return logFileTarget{}, r.refuse(windowsNameReason)
		}
	}
	return r.target(c, r.withInst(roots)), nil
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
				// The file lives on the Windows side, so Windows naming rules
				// apply on every GOOS.
				if windowsNameRefused(rel) {
					return logFileTarget{}, r.refuse(windowsNameReason)
				}
				open := filepath.Join(r.inst, filepath.FromSlash(rel))
				// Re-asserted on the native path rather than assumed, as the last
				// line of defence for the containment property.
				if !strictlyWithinDir(open, r.inst) {
					return logFileTarget{}, r.refuse(fmt.Sprintf("does not map into the spec's ${CONFIG_DIR}; %s does not apply to it", logFileSettingNames))
				}
				return r.target(open, all), nil
			}
		}
	}
	if win, ok := wslPathToWindows(c); ok {
		for _, root := range winRoots {
			if rel, ok := relWithinWindowsDir(win, root); ok {
				if windowsNameRefused(rel) {
					return logFileTarget{}, r.refuse(windowsNameReason)
				}
				return r.target(win, all), nil
			}
		}
		return logFileTarget{}, r.refuse(fmt.Sprintf("is a /mnt/<drive> path outside the spec's ${CONFIG_DIR} and every native entry of %s; the list holds %d native entries", logFileSettingNames, r.nativeEntries))
	}
	return logFileTarget{}, r.refuse(fmt.Sprintf("is a path inside the wsl distro; only ${CONFIG_DIR} and /mnt/<drive> paths of a wsl spec are tailed in this version, even under a wsl:// entry of %s (%d wsl:// entries name this distro)", logFileSettingNames, distroEntries))
}

// target builds the accepted target for the agent-side path openPath.
func (r logFileResolution) target(openPath string, roots []string) logFileTarget {
	t := logFileTarget{Name: r.lf.Name, OpenPath: openPath, Roots: roots}
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

// resolveInstanceConfigDir resolves the instance config dir for comparison
// with a final path, WITHOUT following anything below the base: every
// component from base down to dir must be a real directory (os.Lstat; a
// symlink or a junction is refused), and the result is the resolved base
// joined with that relative part. The base itself is operator configuration
// and may be a link (a mounted volume); the part below it is reachable by the
// server and may not.
func resolveInstanceConfigDir(base, dir string) (string, error) {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: the instance config dir is not inside config_files_dir", errLogFileNotPermitted)
	}
	cur := base
	for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", openErr(err)
		}
		if !isRealDir(fi) {
			return "", fmt.Errorf("%w: the instance config dir is not a real directory under config_files_dir", errLogFileNotPermitted)
		}
	}
	rb, err := resolveLogRoot(base)
	if err != nil {
		return "", openErr(err)
	}
	return filepath.Join(rb, rel), nil
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
// a real directory chain under config_files_dir (resolveInstanceConfigDir),
// re-verified at call time.
//
// The removal itself runs on a directory handle, not a path: the instance dir
// is opened as an os.Root beneath an os.Root of the base, and that handle
// must be os.SameFile with the dir just Lstat'd as a real directory. A server
// that swaps its instance dir for a link after the check therefore gets a
// refusal, never a deletion through the link; once the handle is verified,
// later swaps no longer matter. The leaf is removed without following it
// (Root.Remove of a symlink removes the link), a leaf that is a directory is
// refused, and a missing dir or file is not an error.
func removeConfigDirLogFile(target logFileTarget) error {
	leaf := filepath.Clean(target.OpenPath)
	if !target.InConfigDir || target.ConfigDir == "" || filepath.Dir(leaf) != target.ConfigDir {
		return logRemoveRefusal(target, "is not a direct child of the spec's ${CONFIG_DIR}")
	}
	if _, err := resolveInstanceConfigDir(target.ConfigBase, target.ConfigDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no directory, so no file to remove
		}
		return logRemoveRefusal(target, "is not in a real directory under config_files_dir")
	}
	dirFi, err := os.Lstat(target.ConfigDir)
	if err != nil || !isRealDir(dirFi) {
		return logRemoveRefusal(target, "is not in a real directory under config_files_dir")
	}
	inst, err := openInstanceRoot(target.ConfigBase, target.ConfigDir)
	if err != nil {
		return logRemoveRefusal(target, "is in a directory the agent cannot open")
	}
	defer func() { _ = inst.Close() }()
	if rfi, err := inst.Stat("."); err != nil || !os.SameFile(dirFi, rfi) {
		return logRemoveRefusal(target, "is in a directory that changed while it was checked")
	}
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

// openInstanceRoot opens dir as an os.Root, reached through an os.Root of
// base, so the walk from base to dir cannot leave base.
func openInstanceRoot(base, dir string) (*os.Root, error) {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return nil, err
	}
	b, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()
	return b.OpenRoot(rel)
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
// the policy (see the file comment). The platform open (openLogFileHandle)
// never blocks, never hinders the server's writes, renames or deletes, and
// refuses anything but a regular file. Then, in this order, because the
// server can change the file's names while the agent looks:
//
//  1. the descriptor's link count must be 1 (checkLinkCount): a hard link
//     inside an allowed directory to a file outside it has an inside final
//     path, so no path check can see it;
//  2. the final path (openedFinalPath) must still name THIS file: not a
//     " (deleted)" readlink, and os.Lstat of it a regular file that
//     os.SameFile matches with the descriptor. Without this, a server that
//     unlinks the inside name right after the open leaves the agent with an
//     outside file and an inside path;
//  3. the link count is read again and must still be 1, so a name added or
//     removed while 2 ran cannot slip through;
//  4. the final path must lie strictly inside one of target.Roots, each
//     resolved the same way, and outside the pseudo filesystems.
//
// On a refusal the file is closed and the error wraps errLogFileNotPermitted.
// A file whose last name went away during these steps (link count 0) is
// fs.ErrNotExist, like a file not created yet: the tailer retries it. Any
// other error carries the OS cause without the path.
func openLogFile(target logFileTarget) (*os.File, error) {
	f, err := openLogFileHandle(target)
	if err != nil {
		return nil, err
	}
	if _, err := checkOpenedLogFile(f, target); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// afterLogFileFinalPath, when set, runs right after openLogFile has computed
// the opened file's final path. It is a TEST SEAM only: the tests use it to
// change the file's names at the one moment the race checks guard.
var afterLogFileFinalPath func()

// errLogFileVanished is the fs.ErrNotExist of a file whose last name was
// removed while openLogFile looked at it.
var errLogFileVanished = fmt.Errorf("runtime: open log file: the file was removed while it was opened: %w", fs.ErrNotExist)

// checkOpenedLogFile runs steps 1-4 of openLogFile on the opened f and returns
// the final path, or the refusal (or errLogFileVanished).
func checkOpenedLogFile(f *os.File, target logFileTarget) (string, error) {
	if err := checkLinkCount(f, target, ""); err != nil {
		return "", err
	}
	final, err := openedFinalPath(f, target.OpenPath)
	if err != nil {
		return "", logOpenRefusal(target, "", "has a final path that cannot be determined")
	}
	if afterLogFileFinalPath != nil {
		afterLogFileFinalPath()
	}
	if err := bindFinalPath(f, target, final); err != nil {
		return "", err
	}
	if err := checkLinkCount(f, target, final); err != nil {
		return "", err
	}
	if hasTwoLeadingSeparators(final) || !filepath.IsAbs(final) {
		return "", logOpenRefusal(target, final, "resolves to a UNC, device or relative path")
	}
	if underPseudoFS(final) {
		return "", logOpenRefusal(target, final, "resolves under a kernel pseudo filesystem (proc, sys or dev)")
	}
	for _, root := range target.Roots {
		var resolved string
		var err error
		if target.ConfigDir != "" && root == target.ConfigDir {
			resolved, err = resolveInstanceConfigDir(target.ConfigBase, target.ConfigDir)
		} else {
			resolved, err = resolveLogRoot(root)
		}
		if err != nil {
			continue // a root that does not exist, or may not be used, contains nothing
		}
		if strictlyWithinDir(final, resolved) {
			return final, nil
		}
	}
	return "", logOpenRefusal(target, final, "resolves outside every allowed root")
}

// checkLinkCount requires the descriptor's link count to be exactly 1; 0
// means the file has no name left (errLogFileVanished).
func checkLinkCount(f *os.File, target logFileTarget, final string) error {
	n, err := openedLinkCount(f)
	switch {
	case err != nil:
		return logOpenRefusal(target, final, "has a link count that cannot be read")
	case n == 0:
		return errLogFileVanished
	case n != 1:
		return logOpenRefusal(target, final, "has more than one hard link")
	}
	return nil
}

// bindFinalPath requires final to still name the file f is open on. When it
// does not, a link count of 0 means the file is simply gone (rotated or
// deleted: errLogFileVanished); anything else is a refusal, because the file
// the agent holds is no longer the one the path check would vouch for.
func bindFinalPath(f *os.File, target logFileTarget, final string) error {
	refuse := func(reason string) error {
		if n, err := openedLinkCount(f); err == nil && n == 0 {
			return errLogFileVanished
		}
		return logOpenRefusal(target, final, reason)
	}
	if strings.HasSuffix(final, " (deleted)") {
		return refuse("lost its name while it was opened")
	}
	lfi, err := os.Lstat(final)
	if err != nil {
		return refuse("lost its name while it was opened")
	}
	if !lfi.Mode().IsRegular() {
		return refuse("has a final path that is not a regular file")
	}
	ofi, err := f.Stat()
	if err != nil || !os.SameFile(ofi, lfi) {
		return refuse("has a final path that names another file")
	}
	return nil
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
