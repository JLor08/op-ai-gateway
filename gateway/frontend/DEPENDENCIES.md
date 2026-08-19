# Dependency and licensing record

## Direct AGPLv3 dependency

| Package | Version | Purpose | License | Upstream |
|---|---:|---|---|---|
| `twenty-ui` | `1.0.0-alpha.0` | React UI component library; the status refresh control uses its `Button` component and its theme provider/styles. | AGPL-3.0 (as declared in the npm package) | https://github.com/twentyhq/twenty/tree/main/packages/twenty-ui |

The package is pinned exactly in `package.json` and integrity-locked in
`package-lock.json`. Its distributed `LICENSE` includes AGPLv3 and notes that
upstream enterprise-marked files may have separate commercial terms. This
project uses the published, non-enterprise `Button` component only; review the
upstream license and this record before upgrading or using additional exports.

The complete AGPLv3 text is provided in the repository-root `LICENSE`. The
corresponding source for this project is made available under AGPL-3.0-only.

This is a compliance record, not legal advice.
