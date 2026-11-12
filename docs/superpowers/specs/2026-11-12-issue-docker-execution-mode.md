# Agent runtime: run a RunSpec as a Docker container on any host OS (execution mode `docker`), with per-container RAM and VRAM

## Summary

Let the server agent manage a model server that runs as a **Docker container**, on Linux, Windows and macOS AI servers. The agent should handle it exactly like a native binary:

- start it on demand, drain it, stop it and kill it;
- classify its exit, including an OOM kill;
- route to it through the one router port;
- show its stdout/stderr as the runtime log;
- and **charge its RAM and VRAM to that container**, so that admission and co-residency keep working.

This is the second execution mode, beside the WSL2 mode in #381. Both share the same `execution` seam in the runtime spec. Docker matters because many engines ship as images first: vLLM, SGLang, and sglang-omni, whose documented route is Docker. Docker also brings the same workloads to Linux hosts without a Python environment per engine.

As in the WSL2 issue, **the resource accounting is the point**: a container that starts and stops but is charged too little RAM or VRAM breaks admission (§5.2). That outcome counts as failure even if everything else works.

## Motivation

- vLLM publishes `vllm/vllm-openai`, an image with exec-form `ENTRYPOINT ["vllm","serve"]` ([Dockerfile](https://github.com/vllm-project/vllm/blob/main/docker/Dockerfile)). SGLang publishes `lmsysorg/sglang`. sglang-omni recommends Docker over its pip route, which needs UCX 1.20.x with CUDA and verbs plus flash-attn-4 ([installation](https://sgl-project.github.io/sglang-omni/get_started/installation.html)).
- **Allowlisting `docker` as a binary today is unsafe and does not work** (agent 0.18.1 @ `f34ec050`):
  1. **Security.** `LocalPolicy.Permit` never checks args (`server-agent/internal/runtime/policy_local.go:403`). An allowlisted `docker` therefore permits `--privileged`, `-v /:/host`, `--pid host` or `-v /var/run/docker.sock:…`, all of which are root on the host ([Docker security](https://docs.docker.com/engine/security/)).
  2. **Stop.** Killing the `docker` CLI leaves the container running: `run.go` keeps the attach stream open "until the process in the container exits" and never stops the container ([docker/cli run.go](https://github.com/docker/cli/blob/master/cli/command/container/run.go)). A SIGKILL cannot be proxied. On Windows the agent's stop is `Process.Kill()` on the CLI (`proc_windows.go:37-51`).
  3. **Measurement.** The agent measures exactly one PID per instance (`manager.go:3442`), and here that PID is the CLI. The container's processes are invisible to it. The CLI's few MB of RSS count as a real RAM measurement and end the RAM cold-start bootstrap (ADR-086).

## Research findings

All facts were checked on 2026-11-12 against primary sources.

### Lifecycle

- **Start:** `docker run` without `-d` and without `-t` stays attached, and stdout and stderr remain **separate** (with `-t` they merge) ([run.go](https://github.com/docker/cli/blob/master/cli/command/container/run.go)). `--pull missing|always|never` sets the pull policy; with `never`, a missing image is an error ([docker run](https://docs.docker.com/reference/cli/docker/container/run/)).
- **Stop:** `docker stop -t <s>` sends the stop signal (SIGTERM by default), then SIGKILL after the grace period ([docker stop](https://docs.docker.com/reference/cli/docker/container/stop/)). `docker kill` sends SIGKILL ([docker kill](https://docs.docker.com/reference/cli/docker/container/kill/)).
- **PID 1:** PID 1 ignores signals that have a default action unless it installs a handler. `--init` runs docker-init (tini), which forwards signals and reaps children ([docker run](https://docs.docker.com/reference/cli/docker/container/run/)). The vLLM image defines no `STOPSIGNAL` and no `HEALTHCHECK`.
- **Exit:**
  - `docker run` returns 125 for a daemon error, 126 if the command cannot be invoked and 127 if it is not found ([containers run](https://docs.docker.com/engine/containers/run/)). Those collide with real container exit codes.
  - `docker inspect` reports `.State.ExitCode` and `.State.OOMKilled` without that ambiguity. So the agent must **not** use `--rm`, which would remove the container before it can be inspected. It inspects, then removes the container.
- **Names and orphans:** a container name can be reused only after the old container is removed ("Conflict. The container name … is already in use"). `docker ps -a --filter label=k=v` finds the agent's own containers by label ([docker ps](https://docs.docker.com/reference/cli/docker/container/ls/)).
- **Logs:**
  - The default `json-file` driver does not rotate: `max-size` is −1, unlimited ([json-file](https://docs.docker.com/engine/logging/drivers/json-file/)).
  - `docker logs -f --since` can re-attach after an agent restart ([docker logs](https://docs.docker.com/reference/cli/docker/container/logs/)).
- **Ports:** `-p` binds `0.0.0.0` by default, which makes the port reachable from the LAN. `-p 127.0.0.1:P:C` restricts it to loopback. SGLang binds `127.0.0.1` inside the container unless it is given `--host 0.0.0.0` ([server args](https://docs.sglang.io/advanced_features/server_arguments.html)).
- **Engine needs:** vLLM needs `--ipc=host` or `--shm-size` ([vLLM Docker](https://docs.vllm.ai/en/latest/deployment/docker.html)).
- **Environment:** `-e NAME` with no value takes the value from the CLI's own environment, and `--env-file` reads `NAME=value` lines from a file ([docker run](https://docs.docker.com/reference/cli/docker/container/run/)). Either way, a secret never has to appear in the CLI's argv.

### Resource measurement, per host shape

| | Native Linux (Docker Engine, NVIDIA Container Toolkit) | Docker Desktop on Windows (WSL2 backend) | Docker Engine inside a WSL2 distro | macOS (Docker Desktop, colima, OrbStack) |
|---|---|---|---|---|
| GPU access | `--gpus device=<uuid,…>` or CDI `--device nvidia.com/gpu=…` ([toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html)) | `--gpus=all` only ([Docker GPU](https://docs.docker.com/desktop/features/gpu/); NVIDIA WSL guide: "only `--gpus all` is supported") | `--gpus all` only | **none**: "there is no GPU passthrough for Metal in containers" ([Docker blog](https://www.docker.com/blog/docker-model-runner-vllm-metal-macos/)); only experimental Vulkan via libkrun/krunkit |
| Per-container VRAM | **exact**: host `nvidia-smi --query-compute-apps` reports host PIDs, which the container's cgroup (`cgroup.procs`) maps to the container. NVML inside a container sees nothing without `--pid=host` ([toolkit FAQ](https://nvidia.github.io/container-wiki/toolkit/faq.html)) | **none per container**: NVML has no "active compute process" in WSL ([CUDA on WSL](https://docs.nvidia.com/cuda/wsl-user-guide/index.html)). On the host, all VM GPU memory is charged to the VM worker process `vmwp`, with one same-named PDH instance per guest GPU process (measured in #381, Phase 0) | same as Docker Desktop | n/a |
| Per-container RAM | cgroup v2 `memory.current` and `memory.stat`; `docker stats` subtracts `inactive_file` ([docker stats](https://docs.docker.com/reference/cli/docker/container/stats/), [kernel](https://docs.kernel.org/admin-guide/cgroup-v2.html)) | same, inside the VM. The VM is capped by `.wslconfig` (50 % of RAM by default) | same | same, inside the VM |
| Constraints | daemon access is root-equivalent ("the `docker` group grants root-level privileges", [post-install](https://docs.docker.com/engine/install/linux-postinstall/)); rootless Docker and Podman exist | "not supported on server versions of Windows"; `docker-users` is "equivalent to granting administrative privileges"; paid above 250 employees or USD 10M revenue ([install](https://docs.docker.com/desktop/setup/install/windows-install/)); needs a logged-in user (open request, [roadmap#515](https://github.com/docker/roadmap/issues/515)) | the WSL per-user and Session 0 constraints of #381 | CPU-only for the CUDA images |

- **VRAM demand that can be declared rather than measured:**
  - vLLM `--gpu-memory-utilization` is "a per-instance limit". Its default has changed between versions (0.9 → 0.92 → 1.0 with the extensible KV cache, per the latest docs), so it is predictable only when it is set explicitly ([engine args](https://docs.vllm.ai/en/latest/configuration/engine_args.html)).
  - SGLang `--mem-fraction-static` defaults to "auto" with a fallback of 0.88.
  - Per-container GPU memory *limits* are not supported ([toolkit FAQ](https://nvidia.github.io/container-wiki/toolkit/faq.html)).

## Goals

- A spec with `execution.mode = "docker"` goes through the native lifecycle:
  - the same nine states, crash backoff, idle unload, pinning, instances (§6, §6.1);
  - manual start and stop (ADR-095);
  - the router (§4).
- **Admission never under-charges a container.**
  - On native Linux with NVIDIA, VRAM is measured per container, exactly.
  - Where Docker runs in a VM (Windows, macOS), VRAM follows the VM-level attribution of #381, and RAM is measured per container in both cases.
- A graceful stop reaches the container's whole process tree. No container survives a stop, an agent restart or an agent crash unaccounted for: an orphan sweep runs on every start.
- Logs and the resolved command work as for a native child (§14).
- The agent-local policy stays the authority (ADR-024): an image allowlist, a mount allowlist, and no free-form Docker flags.

## Non-goals

- Compose files, multi-container stacks, Kubernetes.
- Arbitrary `docker run` flags. The spec is a fixed, structured set; see "Security".
- Building images.
- GPU on macOS containers. The experimental Vulkan path is out of scope; macOS containers are CPU-only here.
- Per-process VRAM on AMD. The agent has no AMD per-process measurer today (§5.3), so a ROCm container runs on its estimate, exactly like a native ROCm child.

## Proposed design

### Spec shape

The same optional `execution` object as #381, with a `docker` variant:

```json
{
  "args": ["--model", "${MODEL}", "--gpu-memory-utilization", "0.45"],
  "env": {"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
  "execution": {
    "mode": "docker",
    "docker": {
      "image": "vllm/vllm-openai@sha256:…",
      "container_port": 8000,
      "mounts": [{"host": "/srv/models", "container": "/models", "read_only": true}],
      "shm_size_mb": 16384
    }
  }
}
```

- `binary` must be empty, because the image's entrypoint runs. `args` follow the image. `work_dir` maps to `--workdir` inside the container and is unconstrained, since the container is the boundary. Placeholders work as today (§3.2), and `${PORT}` stays the **host** port.
- **Gateway:** `execution_mode` plus a `docker` JSON column on `agent_runtime_specs` (an append-only migration, all three drivers); DTOs; the document builder.
- **Agent:** `ParseConfig` validation; a non-Docker host answers `not_permitted`; `execution` is part of the launch shape.
- **Flag:** `runtime_exec_docker` (MINOR bump).
  - The gateway must never send a `docker` spec to an agent that does not declare the flag. An older agent ignores the unknown `execution` field and would treat the spec as native.
  - The precedent is `Service.specAsPushed` / `agentRunsInstances` (`gateway/backend/internal/portal/service_runtime.go:3662`, `:3691`), with the document held as `503 runtime_config.agent_features_pending` while the agent's features are unknown (`gateway/backend/internal/gateway/agent_runtime.go:104`).

### Agent-local policy

| Setting | Rule | Empty means |
|---|---|---|
| `runtime_docker_cli` | absolute path of `docker` (or `podman`) | Docker mode is off |
| `runtime_docker_allowed_images` | exact `repo@sha256:…` or `repo:tag` (the operator chooses whether to pin digests) | **nothing starts** in Docker mode |
| `runtime_docker_allowed_mount_dirs` | host-path containment, the same `withinDir` semantics | **no mounts** |
| `runtime_docker_max_shm_mb` | upper bound for `shm_size_mb` | no shm beyond Docker's default |

There is **no field** for `--privileged`, `--network`, `--pid`, `--ipc`, `--device`, `--cap-add`, `--security-opt`, `--user` or extra volumes, so none of them can be expressed.

### Launch

```
docker create --name opagent-<agent>-<spec>-<instance>-<generation>
  --label op-ai-gateway.agent=<agent> --label op-ai-gateway.spec=<spec> …
  --init --pull never --restart no
  --log-driver local --log-opt max-size=<n>
  -p 127.0.0.1:${PORT}:<container_port>
  [--gpus …] [--shm-size …] [-v host:container[:ro] …] [--env-file <agent-owned file>]
  <image> <args…>
docker start --attach <name>
```

- `docker start --attach` on a container created without `-t` attaches the same way an attached `docker run` does, so stdout and stderr should stay separate (to confirm in Phase 1). Both feed the log buffer as for a native child.
- **No gateway-chosen environment ever reaches the CLI process itself.** The CLI has root-equivalent daemon access. `DOCKER_HOST`, `DOCKER_CONTEXT` and `DOCKER_CONFIG` would steer it, and a dynamic-loader variable such as `LD_PRELOAD` would run code inside it.
  - So the convenient `-e NAME` form, which takes the value from the CLI's environment, is **not** used.
  - Instead, the spec `env` and the agent's GPU variables go into a `--env-file`. The agent writes it with mode 0600 in an agent-owned directory and deletes it as soon as `docker create` returns, which is why create and start are separate steps. Values containing a newline are refused, because the env-file format cannot carry them.
  - A secret therefore sits on disk for the duration of one `docker create`. That is a narrower exception than ADR-066's rule that config files carry no secrets, and the ADR must record it. The Engine API (open question 2) would remove it.
- The agent derives the `--gpus` selector from `gpus[].index`: UUIDs on native Linux, `all` plus `CUDA_VISIBLE_DEVICES` where only `all` exists.
- **Image pulls:** Phase 1 uses `--pull never`, so the operator pre-pulls. A missing image is `start_failed`, with a message that names the image. An agent-side pull, as a separate step with its own timeout, is open question 1.

### Lifecycle mapping

| Agent step | `docker` mode |
|---|---|
| terminate (graceful) | `docker stop -t <kill grace> <name>`, which sends SIGTERM and has the daemon follow with SIGKILL after the grace |
| kill grace expired | `docker kill <name>`, then kill the CLI as the backstop |
| exit | after the CLI exits: `docker inspect` for `ExitCode` and `OOMKilled`, then `docker rm`. The CLI's own 125/126/127 are never read as the container's exit code. |
| memory-pressure signature (ADR-086) | `OOMKilled=true`: a direct record of what ADR-086 can only infer from a SIGKILL on native Linux, and available wherever Docker runs, Windows included |
| orphan sweep | at agent start, `docker ps -a --filter label=op-ai-gateway.agent=<agent>`, then stop and remove each container found |

### Resource accounting

The measurement source for a `docker` instance is its **container**, never the CLI's PID. The CLI's PID is excluded from the native measurers.

- **RAM:** `memory.current − inactive_file` of the container's cgroup, the `docker stats` definition and the same one WSL mode uses. One `docker stats --no-stream` call (or one cgroup read on native Linux) per beat covers all containers, off the owner goroutine, under the measurer deadline.
  - Native Linux: the host pool (rule 6) is unchanged, because container memory is host memory.
  - Docker in a VM: also charge the VM pool of #381.
- **VRAM, native Linux:** each instance gets the **set** of host PIDs in its container's cgroup (`cgroup.procs`). The existing compute-apps measurer sums the usage of that set per GPU. This is exact, and it includes vLLM's engine worker processes. Today a native child counts only its own PID (§13).
- **VRAM, Docker in a VM (Windows, macOS):** the VM-level attribution of #381, with start deltas under a per-VM start interlock and the remainder charged as occupied. Docker Desktop's containers and WSL workloads of the same Windows user share **one** VM, so they share one attribution domain. On macOS there is no GPU.
  - Until that attribution exists, a Docker-in-VM spec must be denied the one-shot VRAM bootstrap eviction (ADR-086). Today that grant is host-wide (`vramBootstrapEligible`, `server-agent/internal/runtime/manager.go:2719-2721`), and a Windows NVIDIA host has a measurer. This is the same per-spec "measurable" condition the WSL2 issue adds.

### Logs

- The attached CLI's stdout and stderr go to the spec's log buffer (§14).
- After an agent restart the sweep removes orphaned containers, so no re-attach is needed in Phase 1.
- The resolved command (§14.7) shows the full `docker run` argv, with `-e` names only.

### Portal

- An execution-mode selector (Native / Docker) shown when the agent declares `runtime_exec_docker`;
- fields for image, container port, mounts and shm size;
- a "Docker" marker on status rows;
- German and English keys added together.

### Security

- **Daemon access is root-equivalent on Linux and admin-equivalent on Docker Desktop** (sources above). An agent allowed to use Docker is no longer the unprivileged process ADR-024 assumes, even though the structured spec limits what the *gateway* can make it do. The README must say so, and recommend rootless Docker or rootless Podman where that matters. The ADR records it as a deliberate acceptance.
- **The gateway chooses only within the local allowlists:** an allowlisted image, mounts under allowlisted host directories (read-only by default), the container port and the args after the image.
- Published ports are always bound to `127.0.0.1`. The router stays the only way in.

## Phased plan

**Phase 1: native Linux with NVIDIA.**
- The `execution` seam: reuse the WSL2 issue's if it has landed, otherwise build it here.
- The Docker lifecycle, the orphan sweep and the inspect-based exit mapping.
- Per-container RAM from the cgroup, and per-container VRAM through the PID-set measurer.
- Local policy, the feature flag and the MINOR bump; the portal fields; docs (an agent-runtime-manager section, an ADR, the README).

**Phase 2: Docker in a VM.**
- Docker Desktop on Windows, and Docker Engine inside WSL through `wsl.exe --exec docker …`: lifecycle, per-container RAM and the VM pool. VRAM uses the WSL2 issue's VM attribution, so this depends on its Phase 2.
- macOS: lifecycle and RAM, CPU-only.

**Phase 3, optional:**
- agent-side image pulls with their own timeout and progress in the log;
- Podman verified as a drop-in CLI;
- presets for vLLM, SGLang and sglang-omni (for SGLang, `--host 0.0.0.0` inside the container).

## Open questions

1. **Image pulls.** Should the operator always pre-pull (`--pull never`), or should the agent pull? Images are often more than 10 GB, so an agent pull needs its own timeout, separate from the start timeout, and a visible progress line.
2. **CLI versus Engine API.** The CLI keeps Podman as a drop-in option and mirrors the exec-based design. The Engine API (moby's Go client, Apache-2.0) gives structured inspect, wait, stats and OOM events, and it sends env values straight to the daemon, so no `--env-file` is needed. It costs a large dependency and socket handling. Which one is the better long-term seam?
3. **Rootless.** Should Phase 1 support rootless Docker or Podman, whose cgroup paths differ, or only the system daemon?
4. **Tag versus digest in the image allowlist.** Is a mutable tag acceptable at all, or should the agent require digests?

## Architecture touched

- `agent-runtime-manager.md`: §3 (authority, local policy), §4 (router), §5.1–5.4 (measurement, pools), §6 (lifecycle), §13 (limitations), §14 (logs), plus the new execution-mode section shared with #381.
- ADRs: ADR-024 (authority, plus the root-equivalence acceptance), ADR-025, ADR-031 (Phase 2), ADR-083–086, ADR-090, ADR-095, plus the execution-mode ADR from #381.
- `11-risks-and-technical-debt.md` §11.4 (daemon access is root-equivalent).
- `server-agent/README.md`.

## Related

- #381: WSL2 execution mode on Windows (same seam; owns the VM-level VRAM attribution that Phase 2 reuses).
- #373 / PR #376: decision endpoints. vLLM is their first runtime, and its official image makes Docker the quickest way to run it on Linux hosts.
