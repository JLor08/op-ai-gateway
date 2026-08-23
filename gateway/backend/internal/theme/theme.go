// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package theme loads and validates external, deployable theme definitions
// from a directory on disk. External themes are pure data (JSON tokens plus
// an optional favicon/logo image) supplied by the operator at deploy time —
// never compiled into the frontend bundle and never containing code.
//
// Load is the only entry point. It is tolerant by design: a missing themes
// directory is not an error (external themes are optional), and any single
// invalid theme directory is skipped with a slog.Warn rather than failing
// the whole load. Callers should invoke Load once at startup; a running
// process does not currently watch the directory for changes.
package theme

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Size caps for files read from a theme directory. Oversize files are
// skipped (with a warning) rather than truncated, so a malformed or
// malicious file never partially loads.
const (
	maxThemeJSONBytes = 256 * 1024
	maxImageBytes     = 1024 * 1024
)

// idPattern matches a valid theme id: it doubles as the theme's directory
// name, so it is restricted to a safe, lowercase, filesystem- and URL-
// friendly charset.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// knownTokenKeys is the allowlist of CSS-variable-backed token keys a
// theme.json "light"/"dark" block may set. Anything else is dropped (with a
// warning) so external themes stay confined to the tokens the frontend
// actually understands.
var knownTokenKeys = map[string]bool{
	"surface":       true,
	"page":          true,
	"text":          true,
	"muted":         true,
	"line":          true,
	"brandAccent":   true,
	"brandPrimary":  true,
	"chartSeries2":  true,
	"sidebar":       true,
	"sidebarActive": true,
	"successBg":     true,
	"successText":   true,
	"watchBg":       true,
	"watchText":     true,
	"standbyBg":     true,
	"standbyText":   true,
	"header":        true,
	"headerText":    true,
	"navText":       true,
	"navActiveText": true,
	"accentText":    true,
	"accentSoft":    true,
}

// Option is a lightweight, display-oriented view of a loaded theme, suitable
// for a theme picker.
type Option struct {
	ID, Name string
}

// Brand describes how a theme renders its brand mark. Type is "text" (the
// default) or "image"; Text is the wordmark shown for a text brand; Title is
// shown alongside the mark (e.g. "AI Gateway").
type Brand struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// Theme is a fully loaded and validated external theme. It is marshaled
// directly onto the wire as the "data" payload of GET /api/system/theme (see
// portal.ThemePublicView), so its JSON shape is part of the public API:
//
//   - FaviconPath/LogoPath are internal, absolute filesystem paths used only
//     to serve the asset endpoints (portal.Service.ExternalThemeAsset /
//     internal/gateway's handleSystemThemeAsset) -- never marshaled
//     (json:"-"). A server-local path is deployment-specific and irrelevant
//     to (and a needless disclosure for) an API caller.
//   - HasFavicon/HasLogo are the public, wire-safe signal a frontend uses to
//     know whether "…/api/system/themes/{id}/favicon" or "…/logo" resolves.
type Theme struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	ProductName string            `json:"productName"`
	Font        string            `json:"font"`
	Brand       Brand             `json:"brand"`
	Light       map[string]string `json:"light,omitempty"`
	Dark        map[string]string `json:"dark,omitempty"`
	HasFavicon  bool              `json:"hasFavicon"`
	HasLogo     bool              `json:"hasLogo"`
	FaviconPath string            `json:"-"`
	LogoPath    string            `json:"-"`
}

// Registry is an immutable, in-memory collection of loaded external themes,
// keyed by id.
type Registry struct {
	themes map[string]*Theme
}

// rawTheme mirrors the on-disk theme.json shape before validation.
type rawTheme struct {
	Name        string         `json:"name"`
	ProductName string         `json:"productName"`
	Font        string         `json:"font"`
	Brand       rawBrand       `json:"brand"`
	Light       map[string]any `json:"light"`
	Dark        map[string]any `json:"dark"`
}

type rawBrand struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// Load reads every subdirectory of dir as a candidate external theme.
//
// dir == "" or a directory that does not exist is not an error: it simply
// means there are no external themes, and Load returns an empty, non-nil
// Registry. Any other error reading the top-level directory is returned.
//
// Each subdirectory is validated independently; an invalid theme (bad id,
// missing/malformed theme.json, missing name) is skipped with a slog.Warn
// rather than failing the whole load.
//
// reserved is a caller-supplied set of ids that must never be satisfied by an
// external theme -- typically the caller's own built-in theme ids (e.g.
// portal.BuiltinThemeIDs()), so a built-in is never shadowed by a
// same-named external theme directory. This package has no notion of what a
// "built-in" is; it merely refuses to load a theme whose id appears in
// reserved, skipping it with a slog.Warn like any other invalid theme. An
// empty/omitted reserved list disables this check entirely.
func Load(dir string, reserved ...string) (*Registry, error) {
	reg := &Registry{themes: map[string]*Theme{}}
	if dir == "" {
		return reg, nil
	}

	reservedIDs := make(map[string]bool, len(reserved))
	for _, id := range reserved {
		reservedIDs[id] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, fmt.Errorf("theme: read themes dir %q: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if !idPattern.MatchString(id) {
			slog.Warn("theme: skipping invalid theme id", "id", id)
			continue
		}
		if reservedIDs[id] {
			slog.Warn("theme: skipping external theme, id collides with a reserved built-in theme id", "id", id)
			continue
		}
		th, ok := loadOne(filepath.Join(dir, id), id)
		if !ok {
			continue
		}
		reg.themes[id] = th
	}

	return reg, nil
}

// loadOne loads and validates a single theme directory. ok is false if the
// theme should be skipped (with a warning already logged).
func loadOne(themeDir, id string) (*Theme, bool) {
	jsonPath := filepath.Join(themeDir, "theme.json")
	data, ok := readCapped(jsonPath, maxThemeJSONBytes)
	if !ok {
		// readCapped already warned; theme.json is required.
		return nil, false
	}

	var raw rawTheme
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("theme: skipping theme with malformed theme.json", "id", id, "error", err)
		return nil, false
	}
	if raw.Name == "" {
		slog.Warn("theme: skipping theme with missing name", "id", id)
		return nil, false
	}

	brandType := raw.Brand.Type
	if brandType != "text" && brandType != "image" {
		brandType = "text"
	}

	th := &Theme{
		ID:          id,
		Name:        raw.Name,
		ProductName: raw.ProductName,
		Font:        raw.Font,
		Brand: Brand{
			Type:  brandType,
			Text:  raw.Brand.Text,
			Title: raw.Brand.Title,
		},
		Light: filterTokens(id, "light", raw.Light),
		Dark:  filterTokens(id, "dark", raw.Dark),
	}

	if p, ok := detectImage(themeDir, id, "favicon", []string{"favicon.png"}); ok {
		th.FaviconPath = p
		th.HasFavicon = true
	}
	if p, ok := detectImage(themeDir, id, "logo", []string{"logo.svg", "logo.png"}); ok {
		th.LogoPath = p
		th.HasLogo = true
	}

	return th, true
}

// readCapped reads path, warning and returning ok=false if it is missing,
// unreadable, or larger than maxBytes.
func readCapped(path string, maxBytes int64) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("theme: skipping theme, missing theme.json", "path", path)
		} else {
			slog.Warn("theme: skipping theme, cannot stat file", "path", path, "error", err)
		}
		return nil, false
	}
	if info.Size() > maxBytes {
		slog.Warn("theme: skipping oversize file", "path", path, "size", info.Size(), "max", maxBytes)
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("theme: skipping theme, cannot read file", "path", path, "error", err)
		return nil, false
	}
	return data, true
}

// filterTokens keeps only allowlisted, string-valued token keys, warning
// (once per dropped key) about anything else.
func filterTokens(id, block string, raw map[string]any) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if !knownTokenKeys[k] {
			slog.Warn("theme: dropping unknown token key", "id", id, "block", block, "key", k)
			continue
		}
		s, ok := v.(string)
		if !ok {
			slog.Warn("theme: dropping non-string token value", "id", id, "block", block, "key", k)
			continue
		}
		out[k] = s
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// detectImage looks for the first of candidates (in order) inside themeDir
// that exists and is within the image size cap, returning its absolute path.
func detectImage(themeDir, id, kind string, candidates []string) (string, bool) {
	for _, name := range candidates {
		path := filepath.Join(themeDir, name)
		info, err := os.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("theme: cannot stat "+kind+" candidate", "id", id, "path", path, "error", err)
			}
			continue
		}
		if info.Mode()&fs.ModeType != 0 {
			// Not a regular file (e.g. a directory named favicon.png).
			continue
		}
		if info.Size() > maxImageBytes {
			slog.Warn("theme: skipping oversize "+kind, "id", id, "path", path, "size", info.Size(), "max", maxImageBytes)
			continue
		}
		return path, true
	}
	return "", false
}

// Options returns every loaded theme as an {ID, Name} pair, sorted by ID.
// Suitable for driving a theme picker.
func (r *Registry) Options() []Option {
	opts := make([]Option, 0, len(r.themes))
	for _, th := range r.themes {
		opts = append(opts, Option{ID: th.ID, Name: th.Name})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].ID < opts[j].ID })
	return opts
}

// Get returns the loaded theme with the given id, if any.
func (r *Registry) Get(id string) (*Theme, bool) {
	th, ok := r.themes[id]
	return th, ok
}

// IDs returns every loaded theme id, in no particular order.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.themes))
	for id := range r.themes {
		ids = append(ids, id)
	}
	return ids
}
