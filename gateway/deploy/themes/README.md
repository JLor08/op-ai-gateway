# External themes

This directory is the operator-facing themes directory: a plain folder on
disk that the gateway backend reads at startup (`internal/theme.Load`, see
`gateway/backend/internal/theme/theme.go`). External themes are **pure
data** — a JSON token file plus optional image assets — never code, and
never compiled into the frontend bundle.

The directory the backend reads is `OP_AI_GATEWAY_THEMES_DIR`, which
defaults to `/themes` inside the container. In this repo, `/themes` is
populated two ways (see "Bake vs. mount" below):

- **baked**: `Dockerfile.backend` copies this directory (`deploy/themes`)
  into the image at `/themes`, so the `example` theme (and anything else you
  commit here) ships inside every build.
- **mounted**: a volume (Docker Compose) or ConfigMap (Kubernetes) overlays
  `/themes` at runtime, letting an operator add or replace themes without
  rebuilding the image.

## Layout

```
themes/
  <id>/
    theme.json       # required
    favicon.png       # optional
    logo.svg           # optional (SVG preferred over PNG if both present)
    logo.png           # optional
```

- `<id>` is the theme's directory name and becomes its identifier. It must
  match `^[a-z0-9][a-z0-9-]*$` (lowercase letters, digits, hyphens; must
  start with a letter or digit) — a directory whose name doesn't match is
  skipped with a warning, never an error.
- `theme.json` is required. A directory without one (or with a
  malformed/oversize one, or a missing `name`) is skipped with a
  `slog.Warn` — it will never take down the gateway at startup.
- `favicon.png` is optional. If present it is served at
  `GET /api/system/themes/{id}/favicon`.
- `logo.svg` / `logo.png` is optional. If both are present, `logo.svg` wins.
  Served at `GET /api/system/themes/{id}/logo`. **Note:** the frontend
  serves an SVG logo as `image/svg+xml` via `<img src>`, never inlined —
  do not rely on an SVG logo being inlined into the page DOM.
- File size caps: `theme.json` up to 256 KiB, each image up to 1 MiB.
  Oversize files are skipped (with a warning), never truncated.

## `theme.json`

```json
{
  "name": "Example",
  "productName": "AI Gateway",
  "font": "",
  "brand": { "type": "text", "text": "Example", "title": "AI Gateway" },
  "light": { "brandPrimary": "#2563eb" },
  "dark": { "brandPrimary": "#60a5fa" }
}
```

- `name` (required, non-empty): the display name shown in the theme
  picker.
- `productName`, `font`: optional display strings.
- `brand.type` is `"text"` or `"image"` (anything else falls back to
  `"text"`).
  - `"text"`: renders `brand.text` (falling back to `name` if empty) as a
    wordmark, alongside `brand.title`.
  - `"image"`: renders the theme's logo asset (see `logo.svg`/`logo.png`
    above) instead of a text wordmark.
- `light` / `dark`: optional maps of CSS-variable-backed design tokens.
  **You do not need to set all of them** — any token key you omit inherits
  its value from the built-in Default theme, so a minimal theme that only
  overrides a brand color and a couple of accents is entirely valid (see
  `example/theme.json`).

### Token keys

Only the following keys are recognized in `light`/`dark`; anything else is
dropped (with a warning) rather than applied. All values are strings (CSS
color values):

```
surface, page, text, muted, line, brandAccent, brandPrimary, chartSeries2,
sidebar, sidebarActive, successBg, successText, watchBg, watchText,
standbyBg, standbyText, header, headerText, navText, navActiveText,
accentText, accentSoft
```

## Bake vs. mount

Both mechanisms populate the same `/themes` directory inside the
container; they can be combined (a mounted volume overlays on top of the
baked image contents at the *directory* level — plan your theme ids so a
mount doesn't need to replace a baked directory it doesn't intend to).

- **Bake** (`Dockerfile.backend`): the `COPY deploy/themes /themes` line
  in the final image stage ships this directory's contents with every
  build. Good for a theme that should be part of the shipped artifact
  (e.g. this repo's `example/`).
- **Mount** (Docker Compose): a commented `volumes:` entry on the
  `backend` service in `docker-compose.yml` shows how to bind-mount a host
  directory over `/themes` (read-only) so an operator can drop private
  themes in without rebuilding the image.
- **Mount** (Kubernetes): a commented ConfigMap + `volumeMount` example in
  `k8s/gateway.yaml` shows the equivalent for a cluster deployment.

## Private themes are gitignored

This directory's `.gitignore` ignores everything except `.gitignore`
itself, `README.md`, and `example/`. Any theme you drop in here for local
testing or an operator deployment (e.g. `acme/`) is **not tracked** and
will never be committed — this project is AGPL-3.0-only, and a
third-party operator's branded theme (logos, wordmarks, colors) is not
something we want to accidentally publish. If you need a theme available
at build time, either commit it deliberately (only for themes that are
meant to be public, like `example/`) or deliver it via the mount path
above instead.
