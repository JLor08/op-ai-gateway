package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckLogFiles(t *testing.T) {
	ok := LogFile{Name: "engine", Path: "${CONFIG_DIR}/engine.log"}
	five := make([]LogFile, 5)
	for i := range five {
		five[i] = LogFile{Name: "f" + string(rune('a'+i)), Path: "${CONFIG_DIR}/x.log"}
	}
	for _, tc := range []struct {
		name    string
		files   []LogFile
		wantErr bool
	}{
		{"none", nil, false},
		{"ok", []LogFile{ok}, false},
		{"four entries", five[:4], false},
		{"five entries", five, true},
		{"empty name", []LogFile{{Name: "", Path: ok.Path}}, true},
		{"slash in name", []LogFile{{Name: "a/b", Path: ok.Path}}, true},
		{"33 char name", []LogFile{{Name: strings.Repeat("a", 33), Path: ok.Path}}, true},
		{"32 char name", []LogFile{{Name: strings.Repeat("a", 32), Path: ok.Path}}, false},
		{"reserved process", []LogFile{{Name: "Process", Path: ok.Path}}, true},
		{"duplicate case-insensitive", []LogFile{{Name: "Engine", Path: ok.Path}, {Name: "engine", Path: ok.Path}}, true},
		{"empty path", []LogFile{{Name: "engine", Path: ""}}, true},
		{"api token in path", []LogFile{{Name: "engine", Path: "/tmp/${API_TOKEN}.log"}}, true},
		{"agent env in path", []LogFile{{Name: "engine", Path: "/tmp/${AGENT_ENV:X}.log"}}, true},
		{"1024 byte path", []LogFile{{Name: "engine", Path: strings.Repeat("a", 1024)}}, false},
		{"1025 byte path", []LogFile{{Name: "engine", Path: strings.Repeat("a", 1025)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLogFiles(Spec{ID: "s", LogFiles: tc.files})
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkLogFiles() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Code != ParseErrorInvalidLogFiles {
				t.Fatalf("err = %v, want a *ParseError with code %q", err, ParseErrorInvalidLogFiles)
			}
		})
	}
}

func TestParseConfigRejectsInvalidLogFiles(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"specs":[{"id":"a","binary":"/bin/a","log_files":[{"name":"process","path":"x.log"}]}]}`))
	if err == nil {
		t.Fatal("ParseConfig() accepted a reserved log file name, want an error")
	}
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Code != ParseErrorInvalidLogFiles {
		t.Fatalf("err = %v, want a *ParseError with code %q", err, ParseErrorInvalidLogFiles)
	}
	if len(cfg.Specs) != 0 {
		t.Fatalf("a rejected document must return the zero Config, got %d specs", len(cfg.Specs))
	}
	good, err := ParseConfig([]byte(`{"specs":[{"id":"a","binary":"/bin/a","log_files":[{"name":"engine","path":"${CONFIG_DIR}/e.log"}]}]}`))
	if err != nil {
		t.Fatalf("ParseConfig() rejected a valid log_files: %v", err)
	}
	if len(good.Specs[0].LogFiles) != 1 || good.Specs[0].LogFiles[0].Name != "engine" {
		t.Fatalf("LogFiles = %+v, want the one declared entry", good.Specs[0].LogFiles)
	}
}

func TestSameLaunchShapeIgnoresLogFiles(t *testing.T) {
	a := Spec{ID: "S", Binary: "/bin/a"}
	b := a
	b.LogFiles = []LogFile{{Name: "engine", Path: "${CONFIG_DIR}/e.log"}}
	if !sameLaunchShape(a, b) {
		t.Fatal("specs differing only in log_files differ in launch shape, want the same (the tailer reads it live)")
	}
}
