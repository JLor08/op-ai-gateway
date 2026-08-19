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
