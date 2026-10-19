// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// configFilesN returns n distinct, individually valid config files.
func configFilesN(n int) []ConfigFileDTO {
	files := make([]ConfigFileDTO, 0, n)
	for i := range n {
		files = append(files, ConfigFileDTO{Name: "f" + string(rune('a'+i)) + ".json", Content: "{}"})
	}
	return files
}

// TestValidateRuntimeSpecConfigFiles pins the request-only validation of a
// runtime spec's config files (#257): every name is a single path segment,
// names are unique, the count and per-file size are bounded, and content never
// carries a secret placeholder -- the agent writes these files in the clear.
func TestValidateRuntimeSpecConfigFiles(t *testing.T) {
	maxContent := strings.Repeat("x", runtimeSpecConfigFileMaxBytes)

	tests := []struct {
		name  string
		files []ConfigFileDTO
		// wantJSON is the exact serialized column on success.
		wantJSON string
		// wantInvalid asserts errors.Is(err, ErrRuntimeSpecConfigFilesInvalid).
		wantInvalid bool
		// wantMsg, when set, must appear in the error text.
		wantMsg string
	}{
		{name: "nil list serializes to empty string", files: nil, wantJSON: ""},
		{name: "empty list serializes to empty string", files: []ConfigFileDTO{}, wantJSON: ""},
		{
			name: "valid two-file set marshals in order",
			files: []ConfigFileDTO{
				{Name: "server.json", Content: `{"port":8080}`},
				{Name: "model.toml", Content: "x = 1\n"},
			},
			wantJSON: `[{"name":"server.json","content":"{\"port\":8080}"},{"name":"model.toml","content":"x = 1\n"}]`,
		},
		{name: "empty content is allowed", files: []ConfigFileDTO{{Name: "empty.conf", Content: ""}}, wantJSON: `[{"name":"empty.conf","content":""}]`},
		{name: "dotfile and inner dots are single segments", files: []ConfigFileDTO{{Name: ".env.local", Content: "a"}, {Name: "a..b", Content: "b"}}, wantJSON: `[{"name":".env.local","content":"a"},{"name":"a..b","content":"b"}]`},
		{name: "exactly 16 files is allowed", files: configFilesN(16), wantJSON: marshalConfigFilesForTest(t, configFilesN(16))},
		{name: "exactly 64 KiB of content is allowed", files: []ConfigFileDTO{{Name: "big.conf", Content: maxContent}}, wantJSON: marshalConfigFilesForTest(t, []ConfigFileDTO{{Name: "big.conf", Content: maxContent}})},

		{name: "empty name", files: []ConfigFileDTO{{Name: "", Content: "a"}}, wantInvalid: true, wantMsg: "name"},
		{name: "blank name", files: []ConfigFileDTO{{Name: "   ", Content: "a"}}, wantInvalid: true},
		{name: "forward slash", files: []ConfigFileDTO{{Name: "sub/a.json", Content: "a"}}, wantInvalid: true},
		{name: "backslash", files: []ConfigFileDTO{{Name: `sub\a.json`, Content: "a"}}, wantInvalid: true},
		{name: "dot-dot", files: []ConfigFileDTO{{Name: "..", Content: "a"}}, wantInvalid: true},
		{name: "single dot", files: []ConfigFileDTO{{Name: ".", Content: "a"}}, wantInvalid: true},
		{name: "traversal", files: []ConfigFileDTO{{Name: "../a.json", Content: "a"}}, wantInvalid: true},
		{name: "leading slash", files: []ConfigFileDTO{{Name: "/etc/passwd", Content: "a"}}, wantInvalid: true},
		{name: "windows drive", files: []ConfigFileDTO{{Name: `C:\a.json`, Content: "a"}}, wantInvalid: true},
		{name: "windows alternate data stream", files: []ConfigFileDTO{{Name: "a.json:stream", Content: "a"}}, wantInvalid: true},
		{name: "NUL control character", files: []ConfigFileDTO{{Name: "a\x00.json", Content: "a"}}, wantInvalid: true},
		{name: "newline control character", files: []ConfigFileDTO{{Name: "a\n.json", Content: "a"}}, wantInvalid: true},
		{name: "DEL control character", files: []ConfigFileDTO{{Name: "a\x7f.json", Content: "a"}}, wantInvalid: true},

		{name: "duplicate names", files: []ConfigFileDTO{{Name: "a.json", Content: "1"}, {Name: "a.json", Content: "2"}}, wantInvalid: true, wantMsg: "duplicate"},
		{name: "names differing only by case collide", files: []ConfigFileDTO{{Name: "A.json", Content: "1"}, {Name: "a.json", Content: "2"}}, wantInvalid: true, wantMsg: "duplicate"},
		{name: "17 files", files: configFilesN(17), wantInvalid: true, wantMsg: "16"},
		{name: "64 KiB plus one byte", files: []ConfigFileDTO{{Name: "big.conf", Content: maxContent + "x"}}, wantInvalid: true, wantMsg: "big.conf"},

		{name: "API_TOKEN placeholder", files: []ConfigFileDTO{{Name: "a.json", Content: `{"key":"${API_TOKEN}"}`}}, wantInvalid: true, wantMsg: "secret"},
		{name: "AGENT_ENV placeholder", files: []ConfigFileDTO{{Name: "a.json", Content: `{"key":"${AGENT_ENV:HF_TOKEN}"}`}}, wantInvalid: true, wantMsg: "secret"},
		{name: "non-secret placeholders are allowed", files: []ConfigFileDTO{{Name: "a.json", Content: `{"port":"${PORT}","m":"${MODEL}"}`}}, wantJSON: `[{"name":"a.json","content":"{\"port\":\"${PORT}\",\"m\":\"${MODEL}\"}"}]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateRuntimeSpecConfigFiles(tc.files)
			if tc.wantInvalid {
				if !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
					t.Fatalf("err = %v, want ErrRuntimeSpecConfigFilesInvalid", err)
				}
				if got != "" {
					t.Fatalf("serialized = %q on error, want empty", got)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("err = %q, want it to mention %q", err.Error(), tc.wantMsg)
				}
				// A refusal must never echo file content: it can hold anything,
				// including the very secret that tripped the check.
				for _, f := range tc.files {
					if len(f.Content) >= 8 && len(f.Content) < 200 && strings.Contains(err.Error(), f.Content) {
						t.Fatalf("err = %q echoes file content %q", err.Error(), f.Content)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("validateRuntimeSpecConfigFiles: %v", err)
			}
			if got != tc.wantJSON {
				t.Fatalf("serialized = %q, want %q", got, tc.wantJSON)
			}
		})
	}
}

// marshalConfigFilesForTest builds the expected column value by hand, so the
// size-boundary cases do not need a 64 KiB literal in the table.
func marshalConfigFilesForTest(t *testing.T, files []ConfigFileDTO) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[")
	for i, f := range files {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"` + f.Name + `","content":"` + f.Content + `"}`)
	}
	b.WriteString("]")
	return b.String()
}

// TestValidateRuntimeSpecRequestResolvesConfigFiles pins that the request-level
// validator resolves the serialized column onto runtimeSpecFields, and that a
// bad config file is refused by the same entry point a PUT goes through.
func TestValidateRuntimeSpecRequestResolvesConfigFiles(t *testing.T) {
	base := PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server"}

	f, err := validateRuntimeSpecRequest(base)
	if err != nil {
		t.Fatalf("validateRuntimeSpecRequest(no files): %v", err)
	}
	if f.configFiles != "" {
		t.Fatalf("configFiles = %q with no files, want empty", f.configFiles)
	}

	withFiles := base
	withFiles.ConfigFiles = []ConfigFileDTO{{Name: "server.json", Content: "{}"}}
	f, err = validateRuntimeSpecRequest(withFiles)
	if err != nil {
		t.Fatalf("validateRuntimeSpecRequest(files): %v", err)
	}
	if want := `[{"name":"server.json","content":"{}"}]`; f.configFiles != want {
		t.Fatalf("configFiles = %q, want %q", f.configFiles, want)
	}

	bad := base
	bad.ConfigFiles = []ConfigFileDTO{{Name: "../x", Content: "{}"}}
	if _, err = validateRuntimeSpecRequest(bad); !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
		t.Fatalf("validateRuntimeSpecRequest(bad name) err = %v, want ErrRuntimeSpecConfigFilesInvalid", err)
	}
}

// TestPutRuntimeSpecPersistsConfigFiles drives the full write path: a valid
// set lands on the stored spec as the serialized column, an invalid one is
// refused before anything is stored, and a later PUT that omits the field
// clears it (a PUT is a full document, not a patch).
func TestPutRuntimeSpecPersistsConfigFiles(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	req := PutRuntimeSpecRequest{
		Enabled:     true,
		Binary:      "/usr/local/bin/llama-server",
		ConfigFiles: []ConfigFileDTO{{Name: "server.json", Content: `{"a":1}`}},
	}
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, req); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	spec, ok, err := routeStore.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping ok=%v err=%v", ok, err)
	}
	if want := `[{"name":"server.json","content":"{\"a\":1}"}]`; spec.ConfigFiles != want {
		t.Fatalf("stored ConfigFiles = %q, want %q", spec.ConfigFiles, want)
	}

	// An invalid set is refused and leaves the stored row untouched.
	badReq := req
	badReq.ConfigFiles = []ConfigFileDTO{{Name: "server.json", Content: "${API_TOKEN}"}}
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, badReq); !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
		t.Fatalf("PutRuntimeSpec(secret placeholder) err = %v, want ErrRuntimeSpecConfigFilesInvalid", err)
	}
	spec, _, _ = routeStore.RuntimeSpecByMapping(ctx, mapping.ID)
	if want := `[{"name":"server.json","content":"{\"a\":1}"}]`; spec.ConfigFiles != want {
		t.Fatalf("stored ConfigFiles after refused write = %q, want it unchanged (%q)", spec.ConfigFiles, want)
	}

	// Omitting the field on a full-document PUT clears it.
	req.ConfigFiles = nil
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, req); err != nil {
		t.Fatalf("PutRuntimeSpec(no files): %v", err)
	}
	spec, _, _ = routeStore.RuntimeSpecByMapping(ctx, mapping.ID)
	if spec.ConfigFiles != "" {
		t.Fatalf("stored ConfigFiles after clearing = %q, want empty", spec.ConfigFiles)
	}
}

// TestRuntimeSpecReadBackCarriesConfigFiles pins the read side: a stored set
// comes back on GetRuntimeSpec as a list, a spec-less mapping and a spec with
// none both read as [] (never null), and a spec the benchmark writers spread
// back through PutRuntimeSpec keeps its files (putRequestFromDTO round-trip).
func TestRuntimeSpecReadBackCarriesConfigFiles(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	empty, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec(unconfigured): %v", err)
	}
	if empty.ConfigFiles == nil || len(empty.ConfigFiles) != 0 {
		t.Fatalf("unconfigured ConfigFiles = %#v, want non-nil empty", empty.ConfigFiles)
	}

	files := []ConfigFileDTO{{Name: "a.json", Content: "{}"}, {Name: "b.toml", Content: "x = 1\n"}}
	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", ConfigFiles: files})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	for _, dto := range []RuntimeSpecDTO{put, got} {
		if !slices.Equal(dto.ConfigFiles, files) {
			t.Fatalf("ConfigFiles = %#v, want %#v", dto.ConfigFiles, files)
		}
	}
	if req := putRequestFromDTO(got); !slices.Equal(req.ConfigFiles, files) {
		t.Fatalf("putRequestFromDTO dropped the config files: %#v", req.ConfigFiles)
	}

	cleared, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec(no files): %v", err)
	}
	if cleared.ConfigFiles == nil || len(cleared.ConfigFiles) != 0 {
		t.Fatalf("cleared ConfigFiles = %#v, want non-nil empty", cleared.ConfigFiles)
	}
}

// TestDecodeRuntimeSpecConfigFiles pins the stored-column reader: "" is the
// legitimate stored form of "none" (every pre-#257 row), and corruption is a
// domain sentinel rather than a raw JSON error.
func TestDecodeRuntimeSpecConfigFiles(t *testing.T) {
	for _, stored := range []string{"", "null", "[]"} {
		files, err := decodeRuntimeSpecConfigFiles(stored)
		if err != nil || files == nil || len(files) != 0 {
			t.Fatalf("decode(%q) = %#v, %v; want non-nil empty, nil", stored, files, err)
		}
	}
	if _, err := decodeRuntimeSpecConfigFiles("{not json"); !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
		t.Fatalf("decode(corrupt) err = %v, want ErrRuntimeSpecConfigFilesInvalid", err)
	}
}
