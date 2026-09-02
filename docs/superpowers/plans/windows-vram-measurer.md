# Plan — Windows VRAM measurer

Spec: `../specs/windows-vram-measurer.md`. Branch-local; removed before the PR.

| # | Task | Verification |
|---|---|---|
| 1 | Agent `Version` 0.2.2 → 0.2.3 (once, early — AGENTS.md) | features guard test stays green |
| 2 | `nvidia_pdh.go`: pure parsing + aggregation, tag-free | TDD, red first; runs on Linux CI |
| 3 | `nvidia_pdh_windows.go`: PDH + D3DKMT syscalls, both caches, layout assertions | `GOOS=windows go build` + `go vet`; assertions proven to fail on a wrong layout |
| 4 | Measurer selection: Windows → PDH or nil, never compute-apps | test both selection arms |
| 5 | Zero-guard in `buildSnapshot` (separate commit) | red-then-green: a measured 0 must not override the estimate |
| 6 | Docs: `agent-runtime-manager.md` §5.3 (Windows is not "NVIDIA works"), ADR, §11 risks (D3DKMT devnotes, no Windows CI) | `make lint-docs` |
| 7 | Full gates + Sonar | `make test-go`, `make lint`, `GOOS=windows` build/vet, `make sonar-gate` or say it was skipped |
| 8 | Remove `docs/superpowers/`, `docs/implementation-status.md`; push; PR | `git diff --name-only main...HEAD` shows neither |

Order: 1 → 2 → 3 → 4 (5 independent, parallel) → 6 → 7 → 8.
