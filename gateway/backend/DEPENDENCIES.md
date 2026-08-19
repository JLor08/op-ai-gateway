# Dependency and licensing record

## First-party library

| Component | Module path | Purpose | License | Source notice |
|---|---|---|---|---|
| `auditlog` | `github.com/your-org/onprem-ai-gateway/gateway/backend/pkg/auditlog` | Creates privacy-preserving audit events using SHA-256 payload digests. | AGPL-3.0-only | SPDX headers in every Go source file; full text in the repository-root `LICENSE`. |

`auditlog` is a first-party Go library that is compiled into the gateway binary.
It is part of the corresponding source offered with this AGPLv3 network service.
It deliberately does not retain or return raw prompts/completions.

## Third-party dependencies

There are no third-party production Go dependencies in this initial backend.
Before adding one, record its exact module version, SPDX identifier, upstream
source, copyright/notice requirements, and any source-offer obligations here
and in `THIRD_PARTY_NOTICES.md`.

This document is a compliance record, not legal advice.
