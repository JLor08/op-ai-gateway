# Deployment

`compose.yaml` provides a local container baseline:

```bash
cp gateway/deploy/.env.example gateway/deploy/.env
docker compose -f gateway/deploy/compose.yaml up --build
```

Production manifests, Helm charts, and hardened reverse-proxy configuration
belong in this directory.
