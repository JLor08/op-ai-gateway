# Kubernetes overlay — without the NetBird sidecar

Kustomize overlay of [`../k8s`](../k8s) that runs the gateway **without** the NetBird
client sidecar. The counterpart of [`../docker-compose.no-netbird.yml`](../docker-compose.no-netbird.yml).

Use it when the gateway is reached only over the public Ingress and must **not** be a
NetBird data-plane peer. The NetBird **module** (admin API + peer/setup-key management
for your AI-servers) still works; just leave the System-Settings **`netbird_only`**
switch OFF.

## What it changes vs. the base

- Removes the `netbird` native sidecar (initContainer) from the `op-gateway` pod, plus
  its `netbird-state` PVC, the in-memory `netbird-key` volume, and the `/dev/net/tun`
  hostPath. The overlay removes **only** the netbird sidecar (`initContainers[0]`), so
  the `agent-builder` initContainer + the `op-gateway-agent-bin` PVC are **kept** —
  agent-binary downloads (`BUILD_AGENTS`) work here too. The `backend` container keeps
  its `/data` PVC + the read-only `/agents` mount.
- Clears `OP_AI_GATEWAY_NETBIRD_KEY_FILE` → the System-Settings **"Sidecar enrollen"**
  button hides (there is no sidecar to consume a minted key).
- Relaxes the namespace PodSecurity level from `privileged` to **`baseline`** — without
  the sidecar's caps + hostPath, `privileged` is no longer required.

No nginx change is needed: in Kubernetes nginx already proxies to the
`op-gateway-backend` Service (the pod shares a netns natively), unlike the compose base
which pointed at the sidecar.

## Apply

The Secret is applied separately, exactly as for the base (see [`../k8s/README.md`](../k8s/README.md)):

```bash
kubectl apply -f gateway/deploy/k8s/secret.yaml    # your filled-in copy
kubectl apply -k gateway/deploy/k8s-no-netbird
```

**`op-gateway-edge-tls` is also required here, and BEFORE the first apply.** This overlay
patches only the `op-gateway` (backend) Deployment — `op-gateway-web` (nginx) is inherited
from `../k8s` completely unpatched, including its `readOnly` mount of the `op-gateway-edge-tls`
Secret (the gateway's own edge/TLS certificate) and `nginx-configmap.yaml`'s unconditional
`:443 ssl_certificate` reference. So exactly the same mandatory-Secret rule as the base
applies: on a first install with no old pod to fall back on, a missing `op-gateway-edge-tls`
leaves `op-gateway-web` stuck in `ContainerCreating` forever. See `../k8s/README.md` §3 for
the bootstrap placeholder (`openssl req` + `kubectl create secret`) and the rotation
commands — create it here the same way, before `kubectl apply -k gateway/deploy/k8s-no-netbird`.

Set your image registry the same way as the base (`kustomize edit set image ...` or the
`images:` block in `../k8s/kustomization.yaml`); the netbird image entry is simply unused
here.
