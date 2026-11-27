// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// logFilesN returns n distinct, individually valid log files.
func logFilesN(n int) []LogFileDTO {
	files := make([]LogFileDTO, 0, n)
	for i := range n {
		files = append(files, LogFileDTO{Name: "l" + string(rune('a'+i)), Path: "${CONFIG_DIR}/l" + string(rune('a'+i)) + ".log"})
	}
	return files
}

// TestValidateRuntimeSpecLogFiles pins the request-only validation of a runtime
// spec's declared log files (#430): at most four, a restricted name that is
// unique case-insensitively and never "process", and a path that is non-empty,
// bounded, absolute (or ${CONFIG_DIR}-rooted) and free of secret placeholders.
func TestValidateRuntimeSpecLogFiles(t *testing.T) {
	tests := []struct {
		name        string
		files       []LogFileDTO
		wantJSON    string
		wantInvalid bool
		wantMsg     string
	}{
		{name: "nil list serializes to empty string", files: nil, wantJSON: ""},
		{name: "empty list serializes to empty string", files: []LogFileDTO{}, wantJSON: ""},
		{name: "config dir path", files: []LogFileDTO{{Name: "engine", Path: "${CONFIG_DIR}/e.log"}}, wantJSON: `[{"name":"engine","path":"${CONFIG_DIR}/e.log"}]`},
		{name: "posix absolute", files: []LogFileDTO{{Name: "x", Path: "/var/log/x"}}, wantJSON: `[{"name":"x","path":"/var/log/x"}]`},
		{name: "windows absolute", files: []LogFileDTO{{Name: "x", Path: `C:\logs\x.log`}}, wantJSON: `[{"name":"x","path":"C:\\logs\\x.log"}]`},
		{name: "name charset", files: []LogFileDTO{{Name: "A.b_c-1", Path: "/a"}}, wantJSON: `[{"name":"A.b_c-1","path":"/a"}]`},
		{name: "32 byte name", files: []LogFileDTO{{Name: strings.Repeat("a", 32), Path: "/a"}}, wantJSON: `[{"name":"` + strings.Repeat("a", 32) + `","path":"/a"}]`},
		{name: "exactly 4 files", files: logFilesN(4), wantJSON: `[{"name":"la","path":"${CONFIG_DIR}/la.log"},{"name":"lb","path":"${CONFIG_DIR}/lb.log"},{"name":"lc","path":"${CONFIG_DIR}/lc.log"},{"name":"ld","path":"${CONFIG_DIR}/ld.log"}]`},
		{name: "1024 byte path", files: []LogFileDTO{{Name: "x", Path: "/" + strings.Repeat("p", 1023)}}, wantJSON: `[{"name":"x","path":"/` + strings.Repeat("p", 1023) + `"}]`},

		{name: "5 files", files: logFilesN(5), wantInvalid: true, wantMsg: "4"},
		{name: "empty name", files: []LogFileDTO{{Name: "", Path: "/a"}}, wantInvalid: true, wantMsg: "log file 1"},
		{name: "name with space", files: []LogFileDTO{{Name: "a b", Path: "/a"}}, wantInvalid: true},
		{name: "name with slash", files: []LogFileDTO{{Name: "a/b", Path: "/a"}}, wantInvalid: true},
		{name: "33 byte name", files: []LogFileDTO{{Name: strings.Repeat("a", 33), Path: "/a"}}, wantInvalid: true},
		{name: "reserved process", files: []LogFileDTO{{Name: "process", Path: "/a"}}, wantInvalid: true, wantMsg: "process"},
		{name: "reserved process case-insensitive", files: []LogFileDTO{{Name: "PrOcEsS", Path: "/a"}}, wantInvalid: true, wantMsg: "reserved"},
		{name: "duplicate names", files: []LogFileDTO{{Name: "a", Path: "/a"}, {Name: "a", Path: "/b"}}, wantInvalid: true, wantMsg: "duplicate"},
		{name: "names differing by case collide", files: []LogFileDTO{{Name: "A", Path: "/a"}, {Name: "a", Path: "/b"}}, wantInvalid: true, wantMsg: "duplicate"},
		{name: "empty path", files: []LogFileDTO{{Name: "a", Path: ""}}, wantInvalid: true, wantMsg: `"a"`},
		{name: "1025 byte path", files: []LogFileDTO{{Name: "a", Path: "/" + strings.Repeat("p", 1024)}}, wantInvalid: true, wantMsg: "1024"},
		{name: "relative path", files: []LogFileDTO{{Name: "a", Path: "logs/x"}}, wantInvalid: true, wantMsg: "absolute"},
		{name: "drive relative path", files: []LogFileDTO{{Name: "a", Path: `C:x.log`}}, wantInvalid: true},
		{name: "NUL in path", files: []LogFileDTO{{Name: "a", Path: "/var/log/a\x00b"}}, wantInvalid: true, wantMsg: "control"},
		{name: "newline in path", files: []LogFileDTO{{Name: "a", Path: "/var/log/a\nb"}}, wantInvalid: true, wantMsg: "control"},
		{name: "DEL in path", files: []LogFileDTO{{Name: "a", Path: "/var/log/a\x7fb"}}, wantInvalid: true, wantMsg: "control"},
		{name: "config dir glued to a name", files: []LogFileDTO{{Name: "a", Path: "${CONFIG_DIR}x"}}, wantInvalid: true, wantMsg: "separator"},
		{name: "config dir glued to a dotted name", files: []LogFileDTO{{Name: "a", Path: "${CONFIG_DIR}.log"}}, wantInvalid: true, wantMsg: "absolute"},
		{name: "config dir alone", files: []LogFileDTO{{Name: "a", Path: "${CONFIG_DIR}"}}, wantJSON: `[{"name":"a","path":"${CONFIG_DIR}"}]`},
		{name: "config dir with backslash", files: []LogFileDTO{{Name: "a", Path: `${CONFIG_DIR}\e.log`}}, wantJSON: `[{"name":"a","path":"${CONFIG_DIR}\\e.log"}]`},
		{name: "API_TOKEN placeholder", files: []LogFileDTO{{Name: "a", Path: "${CONFIG_DIR}/${API_TOKEN}.log"}}, wantInvalid: true, wantMsg: "secret"},
		{name: "AGENT_ENV placeholder", files: []LogFileDTO{{Name: "a", Path: "/var/${AGENT_ENV:HF_TOKEN}.log"}}, wantInvalid: true, wantMsg: "secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateRuntimeSpecLogFiles(tc.files, nil)
			if tc.wantInvalid {
				if !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
					t.Fatalf("err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
				}
				if got != "" {
					t.Fatalf("serialized = %q on error, want empty", got)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("err = %q, want it to mention %q", err.Error(), tc.wantMsg)
				}
				// A refusal never quotes the path: it can hold a secret or an
				// operator-internal layout.
				for _, f := range tc.files {
					if len(f.Path) >= 4 && len(f.Path) < 200 && strings.Contains(err.Error(), f.Path) {
						t.Fatalf("err = %q quotes the path %q", err.Error(), f.Path)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("validateRuntimeSpecLogFiles: %v", err)
			}
			if got != tc.wantJSON {
				t.Fatalf("serialized = %q, want %q", got, tc.wantJSON)
			}
		})
	}
}

// TestValidateRuntimeSpecLogFilesRefusesAConfigFileName pins that a log file
// declared at exactly ${CONFIG_DIR}/<name> (either separator), where <name> is
// one of the spec's config_files compared case-insensitively, is refused: the
// agent removes a ${CONFIG_DIR} log file before exec and would delete the
// config file it has just written. The refusal names the log file, never the
// path; a deeper path, another name or an absolute path stays valid.
func TestValidateRuntimeSpecLogFilesRefusesAConfigFileName(t *testing.T) {
	configFiles := []ConfigFileDTO{{Name: "run.json", Content: "{}"}, {Name: "Server.Toml", Content: ""}}
	for _, tc := range []struct {
		name, path  string
		wantInvalid bool
	}{
		{"slash", "${CONFIG_DIR}/run.json", true},
		{"backslash", `${CONFIG_DIR}\run.json`, true},
		{"case-insensitive", "${CONFIG_DIR}/RUN.JSON", true},
		{"second config file", "${CONFIG_DIR}/server.toml", true},
		{"other name", "${CONFIG_DIR}/engine.log", false},
		{"subdirectory", "${CONFIG_DIR}/logs/run.json", false},
		{"prefix only", "${CONFIG_DIR}/run.json.log", false},
		{"absolute", "/var/log/run.json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateRuntimeSpecLogFiles([]LogFileDTO{{Name: "engine", Path: tc.path}}, configFiles)
			if !tc.wantInvalid {
				if err != nil {
					t.Fatalf("err = %v, want valid", err)
				}
				return
			}
			if !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
				t.Fatalf("err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
			}
			if msg := err.Error(); !strings.Contains(msg, `"engine"`) || strings.Contains(msg, tc.path) || strings.Contains(strings.ToLower(msg), "run.json") {
				t.Fatalf("err = %q, want it to name the log file and not the path", msg)
			}
		})
	}
	// Through the request validator: the spec's own config_files count.
	req := PutRuntimeSpecRequest{
		Binary:      "/usr/local/bin/llama-server",
		ConfigFiles: []ConfigFileDTO{{Name: "run.json", Content: "{}"}},
		LogFiles:    []LogFileDTO{{Name: "x", Path: "${CONFIG_DIR}/run.json"}},
	}
	if _, err := validateRuntimeSpecRequest(req); !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
		t.Fatalf("request err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
	}
}

// TestValidateRuntimeSpecRequestResolvesLogFiles pins that the request-level
// validator resolves the column, and that the log-files check runs LAST: a body
// invalid for an older reason keeps answering with that reason.
func TestValidateRuntimeSpecRequestResolvesLogFiles(t *testing.T) {
	base := PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server"}
	f, err := validateRuntimeSpecRequest(base)
	if err != nil || f.logFiles != "" {
		t.Fatalf("no files: logFiles = %q err = %v", f.logFiles, err)
	}
	with := base
	with.LogFiles = []LogFileDTO{{Name: "engine", Path: "/var/log/e.log"}}
	if f, err = validateRuntimeSpecRequest(with); err != nil || f.logFiles != `[{"name":"engine","path":"/var/log/e.log"}]` {
		t.Fatalf("with files: logFiles = %q err = %v", f.logFiles, err)
	}
	bad := base
	bad.LogFiles = []LogFileDTO{{Name: "process", Path: "/a"}}
	bad.ConfigFiles = []ConfigFileDTO{{Name: "../x", Content: "{}"}}
	if _, err = validateRuntimeSpecRequest(bad); !errors.Is(err, ErrRuntimeSpecConfigFilesInvalid) {
		t.Fatalf("doubly invalid err = %v, want the older config_files refusal first", err)
	}
	bad.ConfigFiles = nil
	if _, err = validateRuntimeSpecRequest(bad); !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
		t.Fatalf("bad log file err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
	}
}

func TestPutRuntimeSpecPersistsLogFiles(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	req := PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		LogFiles: []LogFileDTO{{Name: "engine", Path: "${CONFIG_DIR}/engine.log"}},
	}
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, req); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	const want = `[{"name":"engine","path":"${CONFIG_DIR}/engine.log"}]`
	spec, ok, err := routeStore.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil || !ok || spec.LogFiles != want {
		t.Fatalf("stored LogFiles = %q ok=%v err=%v, want %q", spec.LogFiles, ok, err, want)
	}

	bad := req
	bad.LogFiles = []LogFileDTO{{Name: "engine", Path: "relative.log"}}
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, bad); !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
		t.Fatalf("PutRuntimeSpec(relative) err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
	}
	if spec, _, _ = routeStore.RuntimeSpecByMapping(ctx, mapping.ID); spec.LogFiles != want {
		t.Fatalf("stored LogFiles after refused write = %q, want unchanged", spec.LogFiles)
	}

	req.LogFiles = nil
	if _, err = svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, req); err != nil {
		t.Fatalf("PutRuntimeSpec(no files): %v", err)
	}
	if spec, _, _ = routeStore.RuntimeSpecByMapping(ctx, mapping.ID); spec.LogFiles != "" {
		t.Fatalf("stored LogFiles after clearing = %q, want empty", spec.LogFiles)
	}
}

func TestRuntimeSpecReadBackCarriesLogFiles(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
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
	if empty.LogFiles == nil || len(empty.LogFiles) != 0 {
		t.Fatalf("unconfigured LogFiles = %#v, want non-nil empty", empty.LogFiles)
	}
	files := []LogFileDTO{{Name: "a", Path: "/var/log/a.log"}, {Name: "b", Path: `C:\logs\b.log`}}
	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/usr/local/bin/llama-server", LogFiles: files})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	for _, dto := range []RuntimeSpecDTO{put, got} {
		if !slices.Equal(dto.LogFiles, files) {
			t.Fatalf("LogFiles = %#v, want %#v", dto.LogFiles, files)
		}
	}
	// A benchmark write spreads the read-back through PutRuntimeSpec; it must
	// not clear the operator's log files.
	if req := putRequestFromDTO(got); !slices.Equal(req.LogFiles, files) {
		t.Fatalf("putRequestFromDTO dropped the log files: %#v", req.LogFiles)
	}
	raw, err := json.Marshal(empty)
	if err != nil || !strings.Contains(string(raw), `"log_files":[]`) {
		t.Fatalf("read-back wire = %s err=%v, want an explicit log_files []", raw, err)
	}
}

func TestDecodeRuntimeSpecLogFiles(t *testing.T) {
	for _, stored := range []string{"", "null", "[]"} {
		files, err := decodeRuntimeSpecLogFiles(stored)
		if err != nil || files == nil || len(files) != 0 {
			t.Fatalf("decode(%q) = %#v, %v; want non-nil empty, nil", stored, files, err)
		}
	}
	if _, err := decodeRuntimeSpecLogFiles("{not json"); !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
		t.Fatalf("decode(corrupt) err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
	}
}

// TestAgentRuntimeSpecDTOCarriesLogFiles pins the agent-wire DTO (#430): the
// stored column decodes onto AgentRuntimeSpecDTO.LogFiles, and an empty one is
// OMITTED from the wire so a document without log files hashes exactly as it
// did before the field existed.
func TestAgentRuntimeSpecDTOCarriesLogFiles(t *testing.T) {
	mapping := routing.ModelMapping{GatewayModelName: "qwen", AppModelName: "qwen-upstream"}
	build := func(stored string) (AgentRuntimeSpecDTO, error) {
		return agentRuntimeSpecDTO(routing.RuntimeSpec{
			ID: "rspec_x", Binary: "/usr/bin/x", Args: "[]", Env: "{}", LogFiles: stored,
		}, mapping, nil, 0)
	}
	dto, err := build(`[{"name":"engine","path":"${CONFIG_DIR}/e.log"}]`)
	if err != nil {
		t.Fatalf("agentRuntimeSpecDTO: %v", err)
	}
	if want := []LogFileDTO{{Name: "engine", Path: "${CONFIG_DIR}/e.log"}}; !slices.Equal(dto.LogFiles, want) {
		t.Fatalf("LogFiles = %#v, want %#v", dto.LogFiles, want)
	}
	raw, _ := json.Marshal(dto)
	if !strings.Contains(string(raw), `"log_files":[{"name":"engine","path":"${CONFIG_DIR}/e.log"}]`) {
		t.Fatalf("wire = %s, want the log_files list", raw)
	}

	for _, stored := range []string{"", "null", "[]"} {
		dto, err = build(stored)
		if err != nil {
			t.Fatalf("agentRuntimeSpecDTO(%q): %v", stored, err)
		}
		raw, _ = json.Marshal(dto)
		if strings.Contains(string(raw), "log_files") {
			t.Fatalf("wire for stored %q = %s, want log_files omitted", stored, raw)
		}
	}
	if _, err = build("{not json"); !errors.Is(err, ErrRuntimeSpecLogFilesInvalid) {
		t.Fatalf("corrupt err = %v, want ErrRuntimeSpecLogFilesInvalid", err)
	}
}
