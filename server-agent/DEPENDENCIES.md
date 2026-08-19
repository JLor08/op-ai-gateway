# Dependency and licensing record

## First-party library

| Component | Module path | Purpose | License | Source notice |
|---|---|---|---|---|
| `reporting` | `github.com/your-org/onprem-ai-gateway/gateway/server-agent/pkg/reporting` | Reads CPU, load average, memory, OS and architecture data and creates the gateway report payload. | AGPL-3.0-only | SPDX headers in Go source; full license text in the repository-root `LICENSE`. |

`reporting` is compiled into the agent binary and is supplied as corresponding
source with this AGPLv3 network service. It has no external runtime
dependencies and deliberately excludes model prompts, completions, credentials,
environment variables and process arguments from its reports.

This is a compliance record, not legal advice.
