# Licensing & Third-Party Notices

OnPrem AI Gateway is free software licensed under the **GNU Affero General Public License v3.0, license-only (AGPL-3.0-only)**. This chapter covers how that license is recorded in the source, how the AGPL's network-use notice obligation is met by the running product, and the policy that governs which third-party dependencies the project may bundle.

## 1. Project license

| Scope | File |
|---|---|
| Whole repository | `LICENSE` (repo root) |
| Gateway backend (Go module `op-ai-gateway`) | `gateway/backend/LICENSE` |
| Frontend (npm package `op-ai-gateway-portal`) | `gateway/frontend/LICENSE` |
| Server-Agent (Go module `op-ai-server-agent`) | `server-agent/LICENSE` |

Every shipped module carries its own copy of the license text at its root, in addition to the repository-wide copy, so each part is independently and unambiguously AGPL-3.0-only even if extracted on its own.

Every source file additionally carries an SPDX header as its first lines:

```
SPDX-License-Identifier: AGPL-3.0-only
Copyright (C) 2026 OnPrem AI Gateway contributors
```

`scripts/add-license-headers.sh` applies this idempotently across the tree: it walks every tracked-or-untracked, non-ignored file (`git ls-files` + `git ls-files --others --exclude-standard`), skips binary/generated/vendored paths and files that already carry an `SPDX-License-Identifier` line, and picks the right comment leader per file type (`//` for `.go`/`.ts`/`.tsx`, `#` for `.sh`/`.yml`/`.yaml`/`.conf`/`Dockerfile`/`Makefile`, an HTML comment for `.html`), inserting the header after any shebang or Docker parser directive that must stay first.

## 2. Appropriate Legal Notices (AGPL §13)

AGPL-3.0 §13 requires that users interacting with the software remotely over a network be able to get its source. OnPrem AI Gateway surfaces that notice through three independent channels so it is visible in both a browser session and a plain binary invocation:

```mermaid
flowchart LR
    subgraph Runtime["Running product"]
        Footer["Portal footer<br/>(gateway/frontend/src/App.tsx)"]
        Flag["`-license` flag<br/>(cmd/gateway, server-agent)"]
        Log["Startup log line<br/>(cmd/gateway/main.go, server-agent/main.go)"]
    end
    Footer -->|"copyright · AGPL-3.0 link · source repo link"| User["Portal user (any browser session)"]
    Flag -->|"copyright notice + license id + source-offer URL"| Operator["Operator / CLI invocation"]
    Log -->|"program name · AGPL-3.0-only · source URL"| Operator
```

1. **Portal footer** — rendered on every page (`gateway/frontend/src/App.tsx`): the copyright line "Copyright (C) 2026 OnPrem AI Gateway contributors", a link to `https://www.gnu.org/licenses/agpl-3.0.html` labeled "AGPL-3.0", and a link to the source repository.
2. **`-license` flag on both binaries** — `gateway/backend/cmd/gateway/main.go` and `server-agent/main.go` both check for a `-license` argument before doing anything else (before config loading, before the health-check path) and print the notice via a shared `licenseNotice()` helper (`gateway/backend/cmd/gateway/license.go`, `server-agent/license.go`):

   ```go
   func licenseNotice() string {
       var b strings.Builder
       _, _ = agpl3.PrintCopyrightNotice(&b, programName, copyrightYear, copyrightAuthor, sourceURL)
       b.WriteString("    License: GNU AGPL-3.0-only <https://www.gnu.org/licenses/agpl-3.0.html>.\n")
       return b.String()
   }
   ```

   The copyright notice itself is rendered by `agpl3.PrintCopyrightNotice`, a helper from the AGPL-3.0-licensed `github.com/donyori/gogo/copyright/agpl3` package — an AGPL dependency used specifically to produce the AGPL notice, one instance of the copyleft dependency policy in §3 in action. `programName` differs per binary ("OP AI Gateway" vs. "OP AI Gateway Server-Agent"); `copyrightAuthor` ("OnPrem AI Gateway contributors") and `sourceURL` are shared constants.
3. **Startup log line** — both binaries also log the same information unconditionally on every normal start: `"%s (AGPL-3.0-only) — source: %s"`.

## 3. Dependency license policy

The project bundles third-party code under two allowed categories; anything outside them is forbidden regardless of how convenient it would be.

| Category | Allowed | Rationale |
|---|---|---|
| Permissive | MIT, BSD (2/3-Clause), Apache-2.0 | Compatible with (sub-license-able into) AGPL-3.0 in either direction |
| Copyleft | AGPL-3.0, GPL-3.0-or-later, LGPL (any version), MPL-2.0 | One-way or mutually compatible with AGPL-3.0; bundling them keeps the combined work AGPL-3.0-only |
| **Forbidden** | GPL-2.0-**only** (no "or later" grant), SSPL, BSL / other source-available licenses, proprietary/unlicensed code | GPL-2.0-only has no compatible relicensing path into AGPL-3.0; SSPL and BSL are not OSI-approved open-source licenses and carry redistribution restrictions incompatible with shipping this project freely |

The policy is enforced on the **effective** license — the one a package transitively bundles — not just the license declared at the top level; a permissively-licensed package that vendors a forbidden-license file still fails the check. Two concrete dependencies illustrate the copyleft side of the policy in the current dependency graph (`THIRD-PARTY-NOTICES.md`):

- `github.com/donyori/gogo` (and its `copyright/agpl3` subpackage) — **AGPL-3.0**, used by both binaries for the `-license` notice itself (§2).
- `heic-to` (frontend, HEIC image decoding) — **LGPL-3.0**, bundled under the same permissive-plus-copyleft policy.

## 4. `THIRD-PARTY-NOTICES.md`

`THIRD-PARTY-NOTICES.md` (repo root) is a **generated** file — the header comment says so explicitly — listing every third-party dependency actually bundled, grouped by shipped component:

| Section | Source | Tool |
|---|---|---|
| Backend (Go module `op-ai-gateway`) | `gateway/backend`, built for `linux`/`darwin`/`windows` | `go-licenses csv` |
| Server-Agent (Go module `op-ai-server-agent`) | `server-agent`, built for `linux`/`darwin`/`windows`/`plan9`/`aix` | `go-licenses csv` |
| Frontend (npm package `op-ai-gateway-portal`) | `gateway/frontend`, **production dependencies only** (build-time-only tooling such as Vite/Vitest/TypeScript is excluded — it is never shipped to a browser) | `npx license-checker --production` |

It is regenerated with `scripts/gen-third-party-notices.sh`, never hand-edited. The script:

- Runs each license scanner across multiple `GOOS` values so platform-specific dependencies (e.g. Windows-only or `plan9`/`aix`-only transitive deps pulled in by `gopsutil`) are captured, then unions and de-duplicates the results by module name.
- Filters out the project's own modules (`op-ai-gateway`, `op-ai-server-agent`) — this file is third-party notices only, not a manifest of the project's own code.
- Relabels one dependency (`modernc.org/mathutil`) whose license `go-licenses` cannot auto-classify from its module path, so the table still shows the correct `BSD-3-Clause`.
- Greps its own output for known AGPL-incompatible markers (`GPL-2.0-only`, `SSPL`, `Business Source`/`BUSL`, `proprietary`, `UNLICENSED`) and prints a warning to stderr if any appear — a best-effort tripwire, not a hard gate, so a hit is a signal to review §3 manually rather than a fully automated policy enforcement.

## See also

- [Constraints](../02-constraints.md) — the organizational/legal constraint summary this chapter expands on.
- [Theming & Internationalization](theming-and-i18n.md) — why operator-branded external themes are gitignored rather than committed, given this license.
- [Security, Authentication & Authorization](security-auth-rbac.md) — the secret-at-rest and capture-encryption rules that apply independently of licensing.
