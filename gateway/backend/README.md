# Backend

The Go service is the policy-enforcement and provider-routing boundary. It
currently exposes health and runtime status endpoints as the secure foundation
for later OpenAI-compatible proxy endpoints. It also includes the reusable
AGPLv3 `pkg/auditlog` library and a minimal audit endpoint:

```bash
curl -X POST http://localhost:8080/api/v1/audit/record -d 'example request'
```

The response contains a SHA-256 digest rather than the submitted payload. See
[DEPENDENCIES.md](DEPENDENCIES.md) for the AGPL dependency record.

```bash
go test ./...
go run ./cmd/server
```

## Architecture tests

`internal/arch` enforces the module's dependency layering: public packages
(`pkg/*`) may only depend on the standard library and other public packages,
internal packages never depend on `cmd/*`, executable entry points live under
`cmd/*`, and no import may cross into the `server-agent` module or pull in
external dependencies. These rules run as part of `go test ./...`.