# Gateway contracts

This directory contains the complete gateway application and its stable
boundary between clients, the administration UI, and the gateway service.

- `openapi.yaml` describes the initial management and health endpoints.
- `policies/` is reserved for declarative routing and safety policies.
- `architecture.md` explains the target deployment shape.
- `frontend/` contains the React administration console.
- `backend/` contains the Go gateway service.
- `deploy/` contains local and production deployment assets.
- `e2e/` contains black-box integration scenarios.
