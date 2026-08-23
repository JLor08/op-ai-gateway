// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package theme_test

import (
	"op-ai-gateway/internal/theme"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mk writes <dir>/<id>/theme.json with the given raw JSON body.
func mk(t *testing.T, dir, id, json string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, id, "theme.json"), []byte(json))
}

// writeFile writes path with data, creating parent directories as needed.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadReadsValidExternalTheme(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME","brand":{"type":"text","text":"ACME","title":"AI Gateway"},"light":{"brandPrimary":"#123456"}}`)
	writeFile(t, filepath.Join(dir, "acme", "favicon.png"), []byte("\x89PNG..."))
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if got.Name != "ACME" || got.Light["brandPrimary"] != "#123456" {
		t.Fatalf("bad theme %+v", got)
	}
	if got.FaviconPath == "" {
		t.Fatal("favicon not detected")
	}
	if opts := reg.Options(); len(opts) != 1 || opts[0].ID != "acme" {
		t.Fatalf("bad options %+v", opts)
	}
}

func TestLoadSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "BadId", `{"name":"x"}`)  // invalid id (uppercase)
	mk(t, dir, "noname", `{"light":{}}`) // missing name
	mk(t, dir, "badjson", `{`)           // malformed
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.IDs()) != 0 {
		t.Fatalf("expected all skipped, got %v", reg.IDs())
	}
}

// TestLoadSkipsDirMissingThemeJSON confirms a valid-id directory with no
// theme.json at all is skipped like any other invalid theme (this exercises
// the os.IsNotExist branch of readCapped, which now also logs a slog.Warn
// instead of failing silently).
func TestLoadSkipsDirMissingThemeJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("empty"); ok {
		t.Fatal("directory missing theme.json should have been skipped")
	}
}

func TestLoadEmptyDirReturnsEmptyRegistry(t *testing.T) {
	reg, err := theme.Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): unexpected error %v", err)
	}
	if reg == nil {
		t.Fatal("Load(\"\") returned nil registry, want empty non-nil")
	}
	if len(reg.IDs()) != 0 {
		t.Fatalf("expected no themes, got %v", reg.IDs())
	}
}

func TestLoadMissingDirReturnsEmptyRegistry(t *testing.T) {
	reg, err := theme.Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Load(missing dir): unexpected error %v", err)
	}
	if reg == nil {
		t.Fatal("Load(missing dir) returned nil registry, want empty non-nil")
	}
	if len(reg.IDs()) != 0 {
		t.Fatalf("expected no themes, got %v", reg.IDs())
	}
}

func TestLoadDropsUnknownTokenKeys(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME","light":{"brandPrimary":"#123456","totallyUnknownKey":"#fff"}}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if _, present := got.Light["totallyUnknownKey"]; present {
		t.Fatalf("unknown token key was not dropped: %+v", got.Light)
	}
	if got.Light["brandPrimary"] != "#123456" {
		t.Fatalf("known token key lost: %+v", got.Light)
	}
}

func TestLoadRejectsNonStringTokenValues(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME","light":{"brandPrimary":123456}}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if _, present := got.Light["brandPrimary"]; present {
		t.Fatalf("non-string token value was not dropped: %+v", got.Light)
	}
}

func TestLoadBrandTypeDefaultsToText(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME"}`)
	mk(t, dir, "unknown-type", `{"name":"Unknown","brand":{"type":"video"}}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acme", "unknown-type"} {
		got, ok := reg.Get(id)
		if !ok {
			t.Fatalf("%s not loaded", id)
		}
		if got.Brand.Type != "text" {
			t.Fatalf("%s: Brand.Type = %q, want %q", id, got.Brand.Type, "text")
		}
	}
}

func TestLoadKeepsImageBrandType(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME","brand":{"type":"image","title":"AI Gateway"}}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if got.Brand.Type != "image" {
		t.Fatalf("Brand.Type = %q, want %q", got.Brand.Type, "image")
	}
}

func TestLoadDetectsLogoSvgAndPng(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "svg-logo", `{"name":"SVG"}`)
	writeFile(t, filepath.Join(dir, "svg-logo", "logo.svg"), []byte("<svg></svg>"))
	mk(t, dir, "png-logo", `{"name":"PNG"}`)
	writeFile(t, filepath.Join(dir, "png-logo", "logo.png"), []byte("\x89PNG..."))
	mk(t, dir, "no-logo", `{"name":"None"}`)

	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	svgTheme, _ := reg.Get("svg-logo")
	if svgTheme.LogoPath == "" {
		t.Fatal("svg logo not detected")
	}
	pngTheme, _ := reg.Get("png-logo")
	if pngTheme.LogoPath == "" {
		t.Fatal("png logo not detected")
	}
	noneTheme, _ := reg.Get("no-logo")
	if noneTheme.LogoPath != "" {
		t.Fatalf("logo path set with no logo file: %q", noneTheme.LogoPath)
	}
}

func TestLoadSkipsOversizeThemeJSON(t *testing.T) {
	dir := t.TempDir()
	big := `{"name":"` + strings.Repeat("x", 256*1024) + `"}`
	mk(t, dir, "toobig", big)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("toobig"); ok {
		t.Fatal("oversize theme.json should have been skipped")
	}
}

func TestLoadSkipsOversizeFavicon(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME"}`)
	writeFile(t, filepath.Join(dir, "acme", "favicon.png"), make([]byte, 1024*1024+1))
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if got.FaviconPath != "" {
		t.Fatal("oversize favicon should not have been detected")
	}
}

func TestLoadSvgLogoWinsOverPngInSameDir(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "both", `{"name":"Both"}`)
	writeFile(t, filepath.Join(dir, "both", "logo.svg"), []byte("<svg></svg>"))
	writeFile(t, filepath.Join(dir, "both", "logo.png"), []byte("\x89PNG..."))
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("both")
	if !ok {
		t.Fatal("both not loaded")
	}
	// detectImage tries ["logo.svg", "logo.png"] in order, so svg wins.
	if !strings.HasSuffix(got.LogoPath, "logo.svg") {
		t.Fatalf("expected logo.svg to win, got LogoPath=%q", got.LogoPath)
	}
}

func TestLoadSkipsOversizeLogo(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "acme", `{"name":"ACME"}`)
	writeFile(t, filepath.Join(dir, "acme", "logo.svg"), make([]byte, 1024*1024+1))
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("acme not loaded")
	}
	if got.LogoPath != "" {
		t.Fatal("oversize logo should not have been detected")
	}
}

func TestLoadAcceptsFilesAtExactCap(t *testing.T) {
	dir := t.TempDir()
	// theme.json exactly at the 256 KiB cap must be ACCEPTED (the check is a
	// strict `> max`, so size == max is fine). `{"name":"` (9) + pad + `"}` (2).
	exact := `{"name":"` + strings.Repeat("x", 256*1024-11) + `"}`
	if len(exact) != 256*1024 {
		t.Fatalf("test setup: json is %d bytes, want %d", len(exact), 256*1024)
	}
	mk(t, dir, "capjson", exact)
	// favicon exactly at the 1 MiB cap must also be accepted.
	mk(t, dir, "capfav", `{"name":"CapFav"}`)
	writeFile(t, filepath.Join(dir, "capfav", "favicon.png"), make([]byte, 1024*1024))
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("capjson"); !ok {
		t.Fatal("theme.json at exactly the cap should be accepted")
	}
	fav, ok := reg.Get("capfav")
	if !ok {
		t.Fatal("capfav not loaded")
	}
	if fav.FaviconPath == "" {
		t.Fatal("favicon at exactly the cap should be accepted")
	}
}

func TestOptionsSortedByID(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "zeta", `{"name":"Zeta"}`)
	mk(t, dir, "alpha", `{"name":"Alpha"}`)
	mk(t, dir, "mu", `{"name":"Mu"}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := reg.Options()
	if len(opts) != 3 {
		t.Fatalf("len(opts) = %d, want 3: %+v", len(opts), opts)
	}
	wantIDs := []string{"alpha", "mu", "zeta"}
	for i, id := range wantIDs {
		if opts[i].ID != id {
			t.Fatalf("opts[%d].ID = %q, want %q (opts=%+v)", i, opts[i].ID, id, opts)
		}
	}
}

// TestLoadSkipsReservedID confirms an external theme directory whose id
// collides with a caller-supplied reserved id (e.g. a built-in theme id) is
// skipped entirely, while a non-colliding external theme still loads.
func TestLoadSkipsReservedID(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "matrix", `{"name":"Fake Matrix"}`)
	mk(t, dir, "acme", `{"name":"ACME"}`)
	reg, err := theme.Load(dir, "default", "matrix", "skynet")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("matrix"); ok {
		t.Fatal("reserved id \"matrix\" should have been skipped, not loaded as an external theme")
	}
	got, ok := reg.Get("acme")
	if !ok {
		t.Fatal("non-colliding external theme \"acme\" should still load")
	}
	if got.Name != "ACME" {
		t.Fatalf("acme.Name = %q, want %q", got.Name, "ACME")
	}
	if len(reg.IDs()) != 1 {
		t.Fatalf("reg.IDs() = %v, want exactly [\"acme\"]", reg.IDs())
	}
}

// TestLoadWithoutReservedIDsAllowsAnyID confirms Load's reserved-id
// collision guard is opt-in: calling Load without any reserved ids (as
// every other test in this file does) never skips a theme on that basis.
func TestLoadWithoutReservedIDsAllowsAnyID(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "matrix", `{"name":"Fake Matrix"}`)
	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("matrix"); !ok {
		t.Fatal("\"matrix\" should load when no reserved ids are supplied")
	}
}

func TestGetUnknownIDReturnsFalse(t *testing.T) {
	reg, err := theme.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get on unknown id should return ok=false")
	}
}
