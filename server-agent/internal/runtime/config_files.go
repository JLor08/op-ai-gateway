// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// This file is the agent's side of a spec's config_files facet (#257): before
// the model process is exec'd, each ConfigFile is placeholder-expanded and
// written to <configFilesDir>/<spec.ID>/<name>, the same directory
// ${CONFIG_DIR} resolves to, so the spec's Args/Env can point the server at it.
//
// THE DESIGN RULES, because each one is a decision and not an accident:
//
//   - NO SECRETS IN FILES. Content resolves only the non-secret placeholders.
//     ${API_TOKEN} and ${AGENT_ENV:NAME} are hard errors: the file lands on a
//     disk other local users and backups can read, and unlike a child's
//     environment it outlives the process. Secrets keep travelling through
//     Args/Env, where the reported command masks them.
//   - TWO KINDS OF FAILURE. A fault in the document itself -- a secret or
//     malformed placeholder, an OS-unsafe file name, a spec ID that escapes the
//     base -- is DETERMINISTIC: it cannot fix itself on retry, so it is found by
//     the same pre-port dry run as a bad Args placeholder and reported as
//     not_permitted (planSpecConfigFiles; it is port-independent, so port 0
//     sees exactly what the real launch will). Only a genuine filesystem
//     failure (unwritable directory, full disk, failed rename) is a start
//     failure that backs off and retries (writeSpecConfigFiles).
//   - NEVER IN WorkDir, never outside the base. The write location is derived
//     from the agent's OWN config_files_dir and the spec ID alone.

// plannedConfigFile is one config file after validation and expansion: where it
// goes and exactly the bytes it will hold.
type plannedConfigFile struct {
	path    string
	content string
}

// expandConfigFileContent resolves the NON-SECRET placeholders in one config
// file's content: ${PORT}, ${MODEL}, ${HOST_GPU_IDS}, ${CUDA_DEVICES},
// ${VULKAN_DEVICES}, ${METAL_DEVICES} and ${CONFIG_DIR}. It resolves them with
// resolveNonSecretToken, the same code expandSpec uses for Args/Env, so a token
// means the same thing in a file as on the command line.
//
// It is deliberately NOT expandSpec: that resolves ${API_TOKEN} and
// ${AGENT_ENV:NAME}, masks them in the reported command and records where they
// landed -- all of which is meaningless for a file that is written to disk and
// never reported. Here those two secret tokens are HARD ERRORS, as is any
// malformed token the Args/Env near-miss rule would refuse (anything whose
// upper-cased inner text starts with PORT or AGENT_ENV that is not exactly a
// valid token). Every other "${...}" -- a server's own templating -- passes
// through literally, as in Args/Env, and so does all non-placeholder text.
//
// The vendor and getenv parameters are accepted so the signature mirrors
// expandSpec's, but they are never used: no non-secret token depends on the GPU
// vendor, and the agent's environment must never be read for a file -- a
// ${AGENT_ENV:NAME} is refused without looking the variable up. An error names
// the offending token's text, never any value.
func expandConfigFileContent(content string, spec Spec, port int, _ GPUVendor, _ func(string) string, configFilesDir string) (string, error) {
	matches := placeholderPattern.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	gpuIDs := hostGPUIDs(spec)

	var b strings.Builder
	copied := 0
	for _, m := range matches {
		b.WriteString(content[copied:m[0]])
		copied = m[1]
		match := content[m[0]:m[1]]
		inner := match[2 : len(match)-1] // strip "${" and "}"

		v, handled, err := resolveNonSecretToken(inner, spec, port, gpuIDs, configFilesDir)
		if err != nil {
			return "", err
		}
		if handled {
			b.WriteString(v)
			continue
		}

		// The two secret tokens. Exact-match ${API_TOKEN}; for the agent
		// environment ANY ${AGENT_ENV...} spelling is refused (valid, empty-name
		// or typo'd), because the only safe thing to do with a reference to a
		// secret source in a file is not to guess what was meant.
		if inner == "API_TOKEN" {
			return "", fmt.Errorf("%s may not appear in config file content: a written file never carries a secret (pass it through args or env instead)", match)
		}
		upper := strings.ToUpper(inner)
		if strings.HasPrefix(upper, "AGENT_ENV") {
			return "", fmt.Errorf("%s may not appear in config file content: a written file never carries a secret read from the agent environment (pass it through env instead)", match)
		}
		if strings.HasPrefix(upper, "PORT") {
			return "", fmt.Errorf("malformed placeholder %s looks like a PORT reference but does not match the expected syntax", match)
		}
		b.WriteString(match)
	}
	b.WriteString(content[copied:])
	return b.String(), nil
}

// planSpecConfigFiles validates and expands every one of spec's config files
// and returns what writeSpecConfigFiles will write, touching the filesystem not
// at all. It is the single place every DETERMINISTIC config fault is detected,
// so startProcess can run it with port 0 in its pre-port dry run (the port only
// ever substitutes ${PORT}) and get a faithful verdict before a port is
// acquired or a byte is written; writeSpecConfigFiles runs it again with the
// real port so the dry run and the write cannot disagree.
//
// A spec with no config files plans nothing and requires nothing -- not even a
// base directory -- so every spec that predates the facet is untouched.
//
// Checks, all of which must pass before any file is written:
//
//   - a base directory is configured (an empty one would resolve a relative
//     path against the agent's working directory);
//   - the spec's directory, filepath.Join(base, spec.ID), lies STRICTLY inside
//     the base. Join cleans, so a file-mode spec ID of "../evil" or "a/../.."
//     would otherwise resolve beside or above the base, and "" or "." would put
//     the files directly in it. Compared as cleaned absolute paths via
//     withinDir, which is lexical on purpose (see its doc: a symlink the
//     operator planted under their own config directory is theirs to explain);
//   - each file name is acceptable at the write boundary (checkConfigFileName);
//   - each file's content expands (expandConfigFileContent).
func planSpecConfigFiles(spec Spec, port int, vendor GPUVendor, getenv func(string) string, configFilesDir string) ([]plannedConfigFile, error) {
	if len(spec.ConfigFiles) == 0 {
		return nil, nil
	}
	if configFilesDir == "" {
		return nil, fmt.Errorf("runtime: spec %q has config_files but this agent has no config_files_dir configured", spec.ID)
	}
	baseAbs, err := filepath.Abs(configFilesDir)
	if err != nil {
		return nil, fmt.Errorf("runtime: resolve config_files_dir %q: %w", configFilesDir, err)
	}
	specDir := filepath.Join(baseAbs, spec.ID)
	if specDir == baseAbs || !withinDir(specDir, baseAbs) {
		return nil, fmt.Errorf("runtime: spec id %q does not name a directory strictly inside config_files_dir: it resolves to %s, which is outside %s or is the base itself", spec.ID, specDir, baseAbs)
	}

	files := make([]plannedConfigFile, 0, len(spec.ConfigFiles))
	for _, f := range spec.ConfigFiles {
		if err := checkConfigFileName(f.Name); err != nil {
			return nil, fmt.Errorf("runtime: spec %q config file name %q: %w", spec.ID, f.Name, err)
		}
		path := filepath.Join(specDir, f.Name)
		// A validated name is one plain segment, so the file is a DIRECT child of
		// the spec directory. Re-asserted on the joined path rather than assumed,
		// as the last line of defence for the containment property.
		if filepath.Dir(path) != specDir {
			return nil, fmt.Errorf("runtime: spec %q config file %q resolves to %s, outside its directory %s", spec.ID, f.Name, path, specDir)
		}
		content, err := expandConfigFileContent(f.Content, spec, port, vendor, getenv, configFilesDir)
		if err != nil {
			return nil, fmt.Errorf("runtime: spec %q config file %q: %w", spec.ID, f.Name, err)
		}
		files = append(files, plannedConfigFile{path: path, content: content})
	}
	return files, nil
}

// writeSpecConfigFiles provisions spec's config files: each is expanded and
// written to <configFilesDir>/<spec.ID>/<name> atomically at mode 0600 (the
// per-spec directory is created 0755 if needed) by writeFileAtomic, so a
// concurrent reader -- or a crash mid-write -- sees the old file or the new
// one, never a torn one. The whole spec is validated (planSpecConfigFiles)
// before the first byte is written, so a bad second file cannot leave a
// half-provisioned directory behind.
//
// An error from the planning half is a deterministic config fault that
// startProcess's dry run has already reported as not_permitted, so by the time
// this runs any error it returns is a filesystem failure.
//
// Stale files are not pruned: a file dropped from config_files stays on disk
// until the per-spec directory is removed by hand.
func writeSpecConfigFiles(spec Spec, port int, vendor GPUVendor, getenv func(string) string, configFilesDir string) error {
	files, err := planSpecConfigFiles(spec, port, vendor, getenv, configFilesDir)
	if err != nil {
		return err
	}
	for i, f := range files {
		if err := writeFileAtomic(f.path, []byte(f.content)); err != nil {
			return fmt.Errorf("runtime: spec %q: write config file %q: %w", spec.ID, spec.ConfigFiles[i].Name, err)
		}
	}
	return nil
}

// checkConfigFileName is the WRITE-BOUNDARY name check. validConfigFileName
// (the rules ParseConfig enforces, byte-for-byte the gateway's) is re-applied
// because this function must not trust that ParseConfig ran, and then the
// strictness ParseConfig must NOT have is added: a name the gateway accepted
// but this host's filesystem would mangle or alias fails THIS spec, not the
// whole runtime document.
//
// Applied on every GOOS, not only Windows: the document may be authored on one
// OS and the same name must not mean different files on another.
//
//   - A trailing dot or space. Windows silently strips them, so "a.json." and
//     "a.json" would be the SAME file there while the case-insensitive
//     duplicate check in ParseConfig sees two names.
//   - A Windows reserved device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9, and
//     the superscript and CONIN$/CONOUT$ forms), case-insensitively and with or
//     without an extension -- "nul.json" opens the NUL device on Windows rather
//     than creating a file.
//   - A Unicode format character (category Cf): the bidirectional overrides
//     (U+202E makes the characters after it display right-to-left, so a name
//     can read as a different extension than it has), zero-width spaces and
//     joiners, the BOM. Invisible in a listing, they make two distinct names
//     look identical.
func checkConfigFileName(name string) error {
	if !validConfigFileName(name) {
		return fmt.Errorf("must be a single file name (no path separators, \"..\", drive or stream marker, or control characters)")
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("must not end with a dot or a space (Windows strips them, so the name would alias another)")
	}
	for _, r := range name {
		if unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("must not contain an invisible Unicode format character (U+%04X)", r)
		}
	}
	if isWindowsReservedName(name) {
		return fmt.Errorf("is a reserved Windows device name")
	}
	return nil
}

// isWindowsReservedName reports whether name is, or begins with the stem of, a
// Windows device name: the part before the FIRST dot (a reserved stem is
// reserved with any extension, "con.tar.gz" included), with trailing spaces
// dropped and the case folded.
func isWindowsReservedName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if digit, ok := strings.CutPrefix(stem, prefix); ok {
			switch digit {
			case "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
				return true
			}
		}
	}
	return false
}
