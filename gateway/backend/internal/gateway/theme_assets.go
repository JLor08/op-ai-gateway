// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/apierror"
	"os"
	"path/filepath"
	"strings"
)

const (
	codeThemeAssetNotFound = "theme.asset_not_found"
	msgThemeAssetNotFound  = "theme asset not found"
)

// handleSystemThemeAsset serves an external theme's favicon or logo image.
// It is public and pre-auth, like handleSystemTheme -- the login screen
// needs the favicon before any session exists.
//
// The trailing path is parsed as {id}/{kind}. id is resolved ONLY against
// the loaded theme registry, via Portal.ExternalThemeAsset -- it is never
// joined onto a directory as raw request input. A path-traversal id (e.g.
// "..%2f..%2fetc") can therefore never escape the themes directory: it
// simply fails to match any loaded theme id and 404s exactly like any other
// unknown id.
func (s *Server) handleSystemThemeAsset(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/system/themes/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeThemeAssetNotFound, msgThemeAssetNotFound, ""))
		return
	}
	id, kind := parts[0], parts[1]
	if kind != "favicon" && kind != "logo" {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeThemeAssetNotFound, msgThemeAssetNotFound, ""))
		return
	}
	path, ok := s.Portal.ExternalThemeAsset(id, kind)
	if !ok {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeThemeAssetNotFound, msgThemeAssetNotFound, ""))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeThemeAssetNotFound, msgThemeAssetNotFound, ""))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("theme.asset_read_failed", "stat failed", ""))
		return
	}
	w.Header().Set("Content-Type", themeAssetContentType(path))
	// Defense in depth: an operator-supplied logo.svg is untrusted content
	// (it can embed a <script>, even though the frontend's own <img> render
	// path never executes it). These headers neutralize that if someone
	// navigates directly to this endpoint: CSP disallows scripts/styles/
	// framing entirely bar inline style, "sandbox" additionally disables
	// script execution, popups, and top-level navigation for the response
	// even in a browser that ignores the finer-grained directives, and
	// nosniff stops the browser from re-interpreting the body as HTML
	// regardless of the declared Content-Type.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// themeAssetContentType maps a favicon/logo file's extension to its
// content-type. Candidates are limited to the loader's own allowlist
// (favicon.png, logo.svg, logo.png -- see internal/theme.loadOne), so this
// only ever sees one of those; anything else falls back to
// application/octet-stream rather than guessing.
func themeAssetContentType(path string) string {
	switch filepath.Ext(path) {
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}
