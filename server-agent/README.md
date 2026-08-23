# AI Server Reporting Agent

A small Go daemon for an AI/inference host. It periodically sends CPU usage,
one-minute load average, memory usage, CPU count, OS and architecture to the
OnPrem AI Gateway. On Linux, it reads the standard `/proc` interfaces; on other
systems unavailable values are sent as `0`.

## Run

```bash
GATEWAY_URL=http://gateway.internal:8080 \
AGENT_ID=inference-01 \
GATEWAY_AGENT_TOKEN=replace-me \
go run ./cmd/agent
```

Configuration:

- `GATEWAY_URL` — required gateway base URL
- `AGENT_ID` — optional stable node identity; defaults to hostname
- `GATEWAY_AGENT_TOKEN` — optional bearer token
- `REPORT_INTERVAL` — optional Go duration; defaults to `15s`

See [DEPENDENCIES.md](DEPENDENCIES.md) for the AGPLv3 library/dependency record.

## Architecture tests

`internal/arch` enforces the module's dependency layering: the reporting
collector (`pkg/reporting`) stays transport-agnostic (no `net/*` imports),
public packages may only depend on the standard library and other public
packages, executable entry points live under `cmd/*`, and no import may cross
into the `gateway/backend` module or pull in external dependencies. These
rules run as part of `go test ./...`.