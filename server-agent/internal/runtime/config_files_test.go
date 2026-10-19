// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// refuseGetenv fails the test if anything reads the agent's environment. The
// config-file content expander must never consult it: a secret reference is
// refused, not resolved, so a call here is already the leak.
func refuseGetenv(t *testing.T) func(string) string {
	t.Helper()
	return func(name string) string {
		t.Errorf("getenv(%q) was called: config file content must never read the agent environment", name)
		return "leaked-agent-env-value"
	}
}

func configFileSpec(id string, files ...ConfigFile) Spec {
	return Spec{
		ID:            id,
		Model:         id,
		UpstreamModel: "model-" + id,
		GPUs:          []SpecGPU{{Index: 3, VRAMMB: 1}, {Index: 1, VRAMMB: 1}},
		ConfigFiles:   files,
	}
}

// TestExpandConfigFileContentResolvesNonSecretTokens pins the non-secret token
// set the content expander shares with Args/Env expansion (one resolver, so the
// two cannot drift -- the GPU lists keep the spec's DECLARED order, 3 before 1),
// and that everything else -- a server's own templating -- passes through
// literally.
func TestExpandConfigFileContentResolvesNonSecretTokens(t *testing.T) {
	base := filepath.Join("var", "lib", "op-agent", "configs")
	spec := configFileSpec("spec-a")
	cases := []struct {
		name, in, want string
	}{
		{"port", `port=${PORT}`, `port=8123`},
		{"model", `id "${MODEL}"`, `id "model-spec-a"`},
		{"host gpu ids", `ids=${HOST_GPU_IDS}`, `ids=3,1`},
		{"cuda devices", `d=${CUDA_DEVICES}`, `d=CUDA3,CUDA1`},
		{"vulkan devices", `d=${VULKAN_DEVICES}`, `d=Vulkan3,Vulkan1`},
		{"metal devices", `d=${METAL_DEVICES}`, `d=MTL3,MTL1`},
		{"config dir", `dir=${CONFIG_DIR}`, `dir=` + filepath.Join(base, "spec-a")},
		{"several in one value", `{"m":"${MODEL}","p":${PORT},"p2":${PORT}}`, `{"m":"model-spec-a","p":8123,"p2":8123}`},
		{"no placeholder", "plain text\nwith lines\n", "plain text\nwith lines\n"},
		{"unknown tokens pass through", `${TRANSPORT} ${MODEL_PATH} ${CONFIG_DIRS} ${api_token} {{ tmpl }} $HOME`, `${TRANSPORT} ${MODEL_PATH} ${CONFIG_DIRS} ${api_token} {{ tmpl }} $HOME`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandConfigFileContent(tc.in, spec, 8123, GPUVendorNVIDIA, refuseGetenv(t), base)
			if err != nil {
				t.Fatalf("expandConfigFileContent(%q) error = %v, want %q", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("expandConfigFileContent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestConfigFileContentRejectsSecrets is the design rule: a secret never lands
// in a written file. ${API_TOKEN} and ${AGENT_ENV:NAME} are HARD ERRORS in
// content -- even when the spec carries a token and the agent has the variable,
// and without ever reading either -- and the error names the token, never a
// value.
func TestConfigFileContentRejectsSecrets(t *testing.T) {
	spec := configFileSpec("spec-a")
	spec.APIToken = "tok-super-secret"
	cases := []struct{ name, in string }{
		{"api token", `{"key":"${API_TOKEN}"}`},
		{"agent env", `{"key":"${AGENT_ENV:HF_TOKEN}"}`},
		{"agent env lowercase name", `{"key":"${AGENT_ENV:hf_token}"}`},
		{"agent own namespace", `${AGENT_ENV:OP_AGENT_TOKEN}`},
		{"agent env empty name", `${AGENT_ENV:}`},
		{"agent env typo", `${AGENT_ENVV:HF_TOKEN}`},
		{"agent env bare", `${AGENT_ENV}`},
		{"secret after good tokens", `${PORT} ${MODEL} ${API_TOKEN}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandConfigFileContent(tc.in, spec, 8123, GPUVendorNone, refuseGetenv(t), "base")
			if err == nil {
				t.Fatalf("expandConfigFileContent(%q) = %q, want an error: secrets must never be written to a file", tc.in, got)
			}
			if got != "" {
				t.Errorf("expandConfigFileContent returned content %q alongside its error", got)
			}
			for _, leak := range []string{"tok-super-secret", "leaked-agent-env-value"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q contains the secret value %q", err, leak)
				}
			}
		})
	}
}

// TestConfigFileContentMalformedPlaceholders: the PORT near-miss rule of
// Args/Env applies to content too, and the unresolvable non-secret tokens fail
// the same way they do there.
func TestConfigFileContentMalformedPlaceholders(t *testing.T) {
	spec := configFileSpec("spec-a")
	noGPU := Spec{ID: "spec-b"}
	cases := []struct {
		name string
		spec Spec
		in   string
	}{
		{"port typo", spec, `${PORTX}`},
		{"port lowercase", spec, `${port}`},
		{"port suffix", spec, `${PORT_1}`},
		{"model without upstream model", noGPU, `${MODEL}`},
		{"host gpu ids without gpus", noGPU, `${HOST_GPU_IDS}`},
		{"cuda devices without gpus", noGPU, `${CUDA_DEVICES}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := expandConfigFileContent(tc.in, tc.spec, 1, GPUVendorNone, refuseGetenv(t), "base"); err == nil {
				t.Fatalf("expandConfigFileContent(%q) = %q, want an error", tc.in, got)
			}
		})
	}
}

func TestWriteSpecConfigFilesExpandsAndWrites(t *testing.T) {
	base := t.TempDir()
	spec := configFileSpec("spec-a",
		ConfigFile{Name: "server.json", Content: `{"id":"${MODEL}","port":${PORT}}`},
		ConfigFile{Name: "models.ini", Content: "dir=${CONFIG_DIR}\n"},
	)
	if err := writeSpecConfigFiles(spec, 9001, GPUVendorNone, refuseGetenv(t), base); err != nil {
		t.Fatalf("writeSpecConfigFiles: %v", err)
	}

	want := map[string]string{
		"server.json": `{"id":"model-spec-a","port":9001}`,
		"models.ini":  "dir=" + filepath.Join(base, "spec-a") + "\n",
	}
	for name, content := range want {
		path := filepath.Join(base, "spec-a", name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != content {
			t.Errorf("%s = %q, want %q", path, got, content)
		}
		if goruntime.GOOS != "windows" {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s mode = %o, want 600", path, perm)
			}
		}
	}
	// Only the two files: no temp file is left behind by the atomic write.
	entries, err := os.ReadDir(filepath.Join(base, "spec-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("per-spec directory holds %d entries, want exactly %d (a stray temp file?): %v", len(entries), len(want), entries)
	}
}

func TestWriteSpecConfigFilesReplacesExistingContent(t *testing.T) {
	base := t.TempDir()
	spec := configFileSpec("spec-a", ConfigFile{Name: "server.json", Content: "v1"})
	if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err != nil {
		t.Fatal(err)
	}
	spec.ConfigFiles[0].Content = "v2"
	if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(base, "spec-a", "server.json"))
	if err != nil || string(got) != "v2" {
		t.Fatalf("server.json = %q (err %v), want v2", got, err)
	}
}

// TestTwoSpecsSameFilenameNoCollision: the per-spec directory is the whole
// namespace, so two specs that both ship a "server.json" land on two distinct
// paths and each keeps its own content.
func TestTwoSpecsSameFilenameNoCollision(t *testing.T) {
	base := t.TempDir()
	a := configFileSpec("spec-a", ConfigFile{Name: "server.json", Content: "from-a"})
	b := configFileSpec("spec-b", ConfigFile{Name: "server.json", Content: "from-b"})
	for _, s := range []Spec{a, b} {
		if err := writeSpecConfigFiles(s, 1, GPUVendorNone, nil, base); err != nil {
			t.Fatalf("writeSpecConfigFiles(%s): %v", s.ID, err)
		}
	}
	pathA := filepath.Join(base, "spec-a", "server.json")
	pathB := filepath.Join(base, "spec-b", "server.json")
	if pathA == pathB {
		t.Fatal("two specs resolved to the same path")
	}
	for path, want := range map[string]string{pathA: "from-a", pathB: "from-b"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q (err %v), want %q", path, got, err, want)
		}
	}
}

// TestWriteSpecConfigFilesWithoutFilesTouchesNothing: a spec with no
// config_files -- every spec that predates the facet -- must not need a
// configured base directory and must not create one.
func TestWriteSpecConfigFilesWithoutFilesTouchesNothing(t *testing.T) {
	base := filepath.Join(t.TempDir(), "never-created")
	for _, dir := range []string{base, ""} {
		if err := writeSpecConfigFiles(configFileSpec("spec-a"), 1, GPUVendorNone, nil, dir); err != nil {
			t.Errorf("no config files, base %q: error = %v, want nil", dir, err)
		}
		if _, err := planSpecConfigFiles(configFileSpec("spec-a"), 0, GPUVendorNone, nil, dir); err != nil {
			t.Errorf("no config files, base %q: plan error = %v, want nil", dir, err)
		}
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("base directory was created for a spec without config files (stat err = %v)", err)
	}
}

func TestWriteSpecConfigFilesRefusesAnUnconfiguredBase(t *testing.T) {
	spec := configFileSpec("spec-a", ConfigFile{Name: "server.json", Content: "x"})
	if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, ""); err == nil {
		t.Fatal("empty base directory: error = nil, want a refusal (an empty base would write relative to the working directory)")
	}
	if _, err := os.Stat("spec-a"); !os.IsNotExist(err) {
		_ = os.RemoveAll("spec-a")
		t.Fatalf("a relative spec-a directory was created in the working directory (stat err = %v)", err)
	}
}

// TestWriteSpecConfigFilesContainsPathEscape: filepath.Join cleans, so a
// file-mode spec ID of "../evil" would otherwise write beside the base. Every
// escaping or degenerate ID is refused before anything is written, and nothing
// appears outside the base.
func TestWriteSpecConfigFilesContainsPathEscape(t *testing.T) {
	for _, id := range []string{
		"../evil",
		"a/../../evil",
		"..",
		"../../evil",
		".",
		"",
		"x/..",
	} {
		t.Run("id="+id, func(t *testing.T) {
			root := t.TempDir()
			base := filepath.Join(root, "base")
			spec := configFileSpec(id, ConfigFile{Name: "server.json", Content: "pwned"})
			if _, err := planSpecConfigFiles(spec, 0, GPUVendorNone, nil, base); err == nil {
				t.Errorf("planSpecConfigFiles(id %q) = nil, want a containment refusal", id)
			}
			if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err == nil {
				t.Errorf("writeSpecConfigFiles(id %q) = nil, want a containment refusal", id)
			}
			var found []string
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					found = append(found, path)
				}
				return nil
			})
			if len(found) != 0 {
				t.Errorf("files were written for escaping id %q: %v", id, found)
			}
		})
	}
}

// TestWriteSpecConfigFilesRefusesOSUnsafeNames: the write-boundary strictness
// ParseConfig deliberately does not apply (it must accept whatever the gateway
// stores, or one name would tear down the whole document). A bad name fails the
// spec, and fails it BEFORE any sibling file is written.
func TestWriteSpecConfigFilesRefusesOSUnsafeNames(t *testing.T) {
	bad := []string{
		"CON", "con", "Con.txt", "NUL.json", "aux", "PRN.log", "COM1", "com9.json", "LPT1", "lpt9.tar.gz",
		"CONIN$", "CONOUT$", "COM\u00b9", "LPT\u00b2.txt", "NUL .json",
		"a.json.", "a.json ", "trailing.", "dots..", "space ",
		"rtl\u202e.json", "zero\u200bwidth.json", "bom\ufeff.json", "joiner\u200d.json",
		// The gateway-mirrored rules, re-checked because the write boundary
		// must not trust that ParseConfig ran.
		"", "  ", ".", "..", "a/b", `a\b`, "c:evil", "ctrl\x01char",
	}
	for _, name := range bad {
		t.Run("name="+name, func(t *testing.T) {
			base := t.TempDir()
			spec := configFileSpec("spec-a",
				ConfigFile{Name: "fine.json", Content: "ok"},
				ConfigFile{Name: name, Content: "bad"},
			)
			if _, err := planSpecConfigFiles(spec, 0, GPUVendorNone, nil, base); err == nil {
				t.Errorf("planSpecConfigFiles accepted name %q", name)
			}
			if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err == nil {
				t.Errorf("writeSpecConfigFiles accepted name %q", name)
			}
			if entries, _ := os.ReadDir(base); len(entries) != 0 {
				t.Errorf("name %q: base directory is not empty after a refusal (the valid sibling was written first?): %v", name, entries)
			}
		})
	}

	good := []string{
		"server.json", "models.ini", "console.json", "CONFIG", "com10.json", "COM", "LPT", "a.b.json",
		".hidden", "my file.json", "con-fig.yaml", "nullable", "auxiliary.txt", "Straße.json", "日本語.json",
	}
	for _, name := range good {
		t.Run("good="+name, func(t *testing.T) {
			base := t.TempDir()
			spec := configFileSpec("spec-a", ConfigFile{Name: name, Content: "ok"})
			if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err != nil {
				t.Errorf("name %q was refused: %v", name, err)
			}
		})
	}
}

// TestWriteSpecConfigFilesSecretContentWritesNothing: a content fault in any
// file fails the whole spec before the first byte is written.
func TestWriteSpecConfigFilesSecretContentWritesNothing(t *testing.T) {
	base := t.TempDir()
	spec := configFileSpec("spec-a",
		ConfigFile{Name: "first.json", Content: "fine"},
		ConfigFile{Name: "second.json", Content: "${API_TOKEN}"},
	)
	if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err == nil {
		t.Fatal("secret content: error = nil, want a refusal")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("base is not empty after a refused spec: %v", entries)
	}
}

// TestWriteSpecConfigFilesFilesystemFailure: a genuine I/O failure surfaces as
// an error from the write (the manager maps it to start_failed + backoff).
func TestWriteSpecConfigFilesFilesystemFailure(t *testing.T) {
	// A base that lives UNDER a regular file: MkdirAll fails with ENOTDIR for
	// every user, root included (a chmod-based unwritable dir would not).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := configFileSpec("spec-a", ConfigFile{Name: "server.json", Content: "x"})
	base := filepath.Join(blocker, "configs")
	if _, err := planSpecConfigFiles(spec, 0, GPUVendorNone, nil, base); err != nil {
		t.Fatalf("planSpecConfigFiles must not touch the filesystem, got %v", err)
	}
	if err := writeSpecConfigFiles(spec, 1, GPUVendorNone, nil, base); err == nil {
		t.Fatal("write under a regular file: error = nil, want the filesystem error")
	}
}
