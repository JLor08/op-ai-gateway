package wslentry

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		entry, distro, path string
		ok                  bool
	}{
		{"wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm", "Ubuntu-24.04", "/home/ai/vllm-env/bin/vllm", true},
		{"wsl://Ubuntu-24.04/", "Ubuntu-24.04", "/", true},
		{"WSL://Ubuntu-24.04/x", "", "", false},
		{"wsl:/Ubuntu/x", "", "", false},
		{"wsl://Ubuntu-24.04", "", "", false},
		{"wsl:///x", "", "", false},
		{"wsl://-bad/x", "", "", false},
		{`wsl://Ubuntu-24.04\home\x`, "", "", false},
		{`wsl://Ubuntu-24.04/home\x`, "", "", false},
		{"wsl://Ubuntu/a\x00b", "", "", false},
		{"/usr/bin/x", "", "", false},
		{`C:\x.exe`, "", "", false},
	} {
		d, p, ok := Parse(tc.entry)
		if d != tc.distro || p != tc.path || ok != tc.ok {
			t.Errorf("Parse(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.entry, d, p, ok, tc.distro, tc.path, tc.ok)
		}
		wantIs := len(tc.entry) >= 6 && tc.entry[:6] == "wsl://"
		if Is(tc.entry) != wantIs {
			t.Errorf("Is(%q) = %v, want %v", tc.entry, !wantIs, wantIs)
		}
	}
}
