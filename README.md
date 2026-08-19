# OnPrem AI Gateway

An on-premises, OpenAI-compatible gateway for routing internal AI workloads to
self-hosted providers. The project is intentionally split into a Go backend, a
React administration frontend, and shared gateway documentation/contracts.

> Status: foundation scaffold. It is not production-ready yet.

## Features planned

- OpenAI-compatible API facade for local model providers
- Configurable upstream routing and health checks
- API-key authentication, policy enforcement, audit logs, and rate limits
- React-based operations console
- Docker and Kubernetes deployment for isolated environments

## Repository layout

```text
gateway/       Gateway application and its shared contracts
  frontend/    React + TypeScript administration console
  backend/     Go HTTP service and gateway implementation
  deploy/      Container and infrastructure deployment definitions
  e2e/         End-to-end test suite and scenarios
server-agent/   Go reporting agent for AI/inference host telemetry
docs/          Security, deployment, and contribution documentation
```

## Quick start

Prerequisites: Go 1.23+ and Node.js 22+.

```bash
cp gateway/deploy/.env.example gateway/deploy/.env
cd gateway/backend && go run ./cmd/server
```

In a second terminal:

```bash
cd gateway/frontend
npm install
npm run dev
```

The backend exposes `GET /healthz` on port `8080` by default. The frontend is
served by Vite on port `5173` and proxies API calls to the backend.

## Tests

```bash
cd gateway/backend && go test ./...
cd ../e2e && npm ci && npx playwright install chromium && npm test
```

For a containerized local baseline, run
`docker compose -f gateway/deploy/compose.yaml up --build`. The UI is then
available at `http://localhost:3000` and the backend at
`http://localhost:8080`.

## License

Copyright (C) 2026 OnPrem AI Gateway contributors.

This program is free software: you can redistribute it and/or modify it under
the terms of the **GNU Affero General Public License v3.0 only**. See
[LICENSE](LICENSE). If you modify this software and make it available to users
over a network, AGPLv3 requires offering those users the corresponding source
code.

## Contributing

Please read [CONTRIBUTING.md](CONTRIBUTING.md) and follow the security process
in [SECURITY.md](SECURITY.md). Voluntary contributor recognition is maintained
in [CONTRIBUTORS.md](CONTRIBUTORS.md).
