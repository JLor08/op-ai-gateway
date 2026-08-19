# Deployment baseline

Run the backend behind an internal TLS-terminating reverse proxy. Bind it only
to trusted network interfaces, use a secrets manager for credentials, and route
providers through allow-listed internal addresses. Production deployments should
add persistent audit storage, identity integration, rate limiting, and metrics.
