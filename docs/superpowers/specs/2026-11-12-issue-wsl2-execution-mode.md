# Agent runtime: run a RunSpec inside WSL2 on Windows hosts (execution mode `wsl`), with per-workload RAM and VRAM

## Summary

Let the server agent on a **Windows** AI server manage a model server that runs **inside WSL2**: vLLM, SGLang, sglang-omni and other Linux-only engines. The agent should treat such a workload like a native binary:

- start it on demand, drain it, stop it and kill it;
- classify its exit and apply backoff;
- route to it through the one router port;
- show its stdout/stderr as the runtime log;
- and, above all, **charge its RAM and VRAM to that workload**, so that admission and co-residency keep working when several models share a host.

This adds an **execution mode** to the runtime spec: `native` (today, the default) and `wsl`. A Docker mode for every host OS is a separate issue (#TBD). It builds on the same seam.

**The resource accounting is the point of this feature, not a nice-to-have.** A WSL workload that starts and stops cleanly but is charged too little RAM or VRAM breaks the arithmetic that admission (§5.2) depends on. Two co-resident models can then reach an OOM. That outcome counts as failure even if everything else works.

## Motivation

- The engines we want on Windows AI servers do not run on Windows natively:
  - vLLM: "vLLM does not support Windows natively", and it suggests WSL ([GPU install docs](https://docs.vllm.ai/en/latest/getting_started/installation/gpu/)).
  - SGLang: the install docs "target Linux with NVIDIA GPUs" ([install](https://docs.sglang.io/docs/get-started/install)), and a maintainer answered "no" to Windows support ([discussion #4095](https://github.com/sgl-project/sglang/discussions/4095)).
  - sglang-omni (TTS, ASR and omni models; [repo](https://github.com/sgl-project/sglang-omni)) documents Linux with NVIDIA as its main path and has no Windows guide ([installation](https://sgl-project.github.io/sglang-omni/get_started/installation.html)).
- vLLM ≥ 0.32.0 serves `POST /v1/systemone` and ≥ 0.33.0 serves OpenAI's `POST /v1/decisions` (#373, PR #376). A WSL-managed vLLM would make decision models available on Windows AI servers through the managed runtime.
- **Why not just allowlist `wsl.exe` as a binary today?** It looks like it works, but it breaks in four ways (agent 0.18.1 @ `f34ec050`):
  1. **Security.** `LocalPolicy.Permit` (`server-agent/internal/runtime/policy_local.go:403`) checks the binary and the work dir, never the arguments. An allowlisted `wsl.exe` therefore permits any command in any distro, as any Linux user, root included.
  2. **Stop.** On Windows, `terminateGroup` and `killGroup` are both `cmd.Process.Kill()` on the direct child (`proc_windows.go:37-51`, a documented known limitation). Killing `wsl.exe` sends the Linux process no signal. See "Stop" below for what happens instead.
  3. **Measurement.** The agent measures exactly one PID per instance (`measurementTargets`, `manager.go:3442`). Here that PID is `wsl.exe`, a thin client. VRAM is never found on it, because Windows charges WSL GPU memory to the VM process. Its few MB of private working set count as a **real** RAM measurement, which ends the RAM cold-start bootstrap (ADR-086) and under-charges the model.
  4. **Admission.** Points 2 and 3 together defeat the VRAM and RAM gates. Those gates are the only protection co-resident models have against an OOM.

## Research findings

All facts were checked on 2026-11-12 against primary sources. Microsoft's WSL is open source; "WSL source" means [microsoft/WSL @ c5361b4c](https://github.com/microsoft/WSL/tree/c5361b4c626bbaad7ace71a9687d4f541b4e282a) (master on that date). Facts drawn from reading that source rather than from documentation are marked *(source-read)*. Points that need a real Windows host are marked **unverified** and are collected in Phase 0.

### Launch

- `wsl.exe -d <distro> [-u <user>] [--cd <dir>] --exec <binary> <args…>`:
  - runs the binary **without a shell**. The help text says "Execute the specified command without using the default Linux shell". The argv comes from `CommandLineToArgvW`, the same rules Go's Windows command-line escaping follows *(source-read, `src/windows/common/WslClient.cpp`)*.
  - `-d`, `-u` and `--cd` must come before `--exec`.
  - Without `--exec`, including the `--` form, the command runs as `$SHELL -c …`. That form must never be used.
- **Environment.** Windows variables reach Linux only if they are listed in `WSLENV`, and the `/u` flag limits a variable to the Win32→WSL direction ([filesystems doc](https://learn.microsoft.com/en-us/windows/wsl/filesystems)). Init always sets `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG` and `WSL_DISTRO_NAME` *(source-read, `src/linux/init/init.cpp`)*.
- **stdout and stderr** of the Linux process arrive on `wsl.exe`'s stdout and stderr as **separate raw byte streams**, relayed over hvsockets *(source-read, `init.cpp`, `svccomm.cpp`)*. Because the streams are pipes, Python block-buffers its output, so the launch should set `PYTHONUNBUFFERED=1`.
- **`wsl.exe`'s own error messages go to stdout**, encoded as UTF-16 unless `WSL_UTF8=1` is set. A `wsl.exe`-level failure exits with −1 *(source-read)*.
- **Exit code.** For a normal exit, `wsl.exe` returns `WEXITSTATUS`. For a death by signal it returns the **raw signal number**, for example 15 for SIGTERM and 9 for SIGKILL, not 128+n *(source-read, `init.cpp`, `interop.cpp`)*.

### Stop

- **Killing `wsl.exe` sends no signal.** The Linux relay exits and closes the pty master. The workload, a `forkpty` session leader, then gets **SIGHUP** *(source-read, plus pty semantics)*. [WSL#3766](https://github.com/microsoft/WSL/issues/3766) reports that processes with redirected streams "receive only SIGHUP".
  - Python installs no SIGHUP handler, so vLLM dies abruptly, with no clean shutdown.
  - A child that called `setsid` survives as an orphan. It lives until the distro idles out, and only if nothing else keeps the distro running.
  - **Unverified on a host** (Phase 0, B5).
- **A graceful stop has to signal from inside the distro.** One way: `kill -TERM -- -<pgid>`; the PGID equals the workload's PID because of `forkpty`. A better way, when systemd runs in the distro:
  - run the workload under `systemd-run --scope --unit=<name>`;
  - stop it with `systemctl kill --signal=SIGTERM <name>.scope`, which reaches every process in the cgroup, including processes that called `setsid`.
  - A scope's command "is executed by systemd-run itself as parent process" ([systemd-run(1)](https://www.freedesktop.org/software/systemd/man/latest/systemd-run.html)), so it stays under the relay and keeps the distro alive.
  - Transient *services* do not keep the distro alive: "systemd services will NOT keep your WSL instance alive" ([Microsoft devblog](https://devblogs.microsoft.com/commandline/systemd-support-is-now-available-in-wsl/)).
- **Idle shutdown.** `[general] instanceIdleTimeout` (default 15000 ms) ends a distro that has no clients. `[wsl2] vmIdleTimeout` (default 60000 ms, Windows 11 only) shuts the VM down. `-1` disables either one ([wsl-config](https://learn.microsoft.com/en-us/windows/wsl/wsl-config); for `vmIdleTimeout`, the `>= 0` guard in `WslCoreConfig.h` *(source-read)*).
- vLLM's RFC for a graceful SIGTERM drain was closed "not planned" ([vllm#24885](https://github.com/vllm-project/vllm/issues/24885)). Rely on the agent's existing grace window and SIGKILL escalation, not on a drain inside the engine.

### Networking and health

- **NAT mode (the default).** `localhostForwarding=true` (also the default) makes ports "bound to wildcard or localhost in the WSL 2 VM … connectable from the host via localhost:port" ([wsl-config](https://learn.microsoft.com/en-us/windows/wsl/wsl-config)). The forwarder scans listening sockets **once per second** *(source-read, `src/linux/init/localhost.cpp`)*, so a port can need up to about 1 s to appear after `listen()`. The existing 500 ms health poll tolerates that.
- **Mirrored mode** needs Windows 11 22H2 or later. In mirrored mode `::1` is unsupported, and Linux cannot bind a port Windows already holds ([networking](https://learn.microsoft.com/en-us/windows/wsl/networking)).
- Either way, the agent's existing probe of `http://127.0.0.1:<port><health_path>` from Windows reaches the workload. vLLM's `/health` answers 200 or 503, and so does sglang-omni's. SGLang binds `--host 127.0.0.1` by default ([server args](https://docs.sglang.io/advanced_features/server_arguments.html)).

### GPU and VRAM

- CUDA in WSL goes through `/dev/dxg` and a `libcuda.so` stub that wraps the Windows driver. NVIDIA's [CUDA on WSL guide](https://docs.nvidia.com/cuda/wsl-user-guide/index.html) (v13.5, 2026-11-02) says to install no Linux GPU driver inside WSL. It also says GPU acceleration works only on GeForce/RTX cards in **WDDM** mode, not in TCC mode and not on data-center GPUs.
- **No per-process VRAM inside WSL.** The same guide says "GPU utilization, active compute process are some queries that are not yet supported" by NVML/`nvidia-smi` in WSL.
- **On the Windows side, all WSL GPU memory belongs to one host process.**
  - Microsoft's GPU-paravirtualization driver doc says "Each VM DXG process belongs to the same EPROCESS as the VM worker process" ([GPU-PV](https://learn.microsoft.com/en-us/windows-hardware/drivers/display/gpu-paravirtualization)), and identifies that process as `vmmem`.
  - A third-party tool that measured it on driver 610.88 found the memory charged to `vmwp.exe`. It also reports that WSL's adapter LUIDs differ from the host's ([shanselman/gputaskman](https://github.com/shanselman/gputaskman)).
  - Both sources agree on the consequence: **one host PID, per adapter, for every Linux process in the VM.** Neither documents a per-Linux-process split.
  - **Measured (Phase 0, B1, 2026-11-12):** on a Windows 11 Pro host with three NVIDIA GPUs, vLLM ran in WSL with `--gpu-memory-utilization 0.5` on a 24 GB card.
    - The WSL GPU memory appeared on **`vmwp`**, not `vmmem`: 12318 MiB on one adapter, and an instance with 0 MiB on each of the other two.
    - Its PDH instances carry the **same adapter LUIDs as ordinary host processes** such as `dwm`. So the existing LUID → PCI → index bridge (ADR-031) applies to it unchanged.
    - That adapter's `GPU Adapter Memory` total (13084 MiB) matched `nvidia-smi`'s `memory.used` for GPU index 0 (13077 MiB).
    - 12318 MiB is close to half of a 24 GB card, as the setting predicts.
    - The third-party report is therefore right about the process name. Its remark about differing LUIDs does not apply to the host-side PDH instances.
- **The engines reserve their VRAM up front, as a configurable fraction of the card:**
  - vLLM `--gpu-memory-utilization`: "a per-instance limit". Its default has moved between versions (0.9 → 0.92; the latest docs say 1.0 with the extensible KV cache). So it is a predictable number only when it is set explicitly ([engine args](https://docs.vllm.ai/en/latest/configuration/engine_args.html)).
  - SGLang `--mem-fraction-static`: weights plus the KV pool. sglang-omni accepts it for every engine stage ([config](https://sgl-project.github.io/sglang-omni/developer_reference/config.html)).
  - That makes a **start delta** a stable per-workload figure, as long as nothing else starts at the same moment. **Unverified** (Phase 0, B2).
- **Multi-GPU is unverified.** NVIDIA lists "On multi-GPU systems it is not possible to filter for specific GPU devices" next to its Docker `--gpus all` note. Whether `CUDA_VISIBLE_DEVICES` plus `CUDA_DEVICE_ORDER=PCI_BUS_ID` inside WSL selects the intended card, and whether the PCI bus IDs match the host's, is Phase 0, B3. It matters because a model that spans all three cards of a Windows host is a real case (agent-runtime-manager §5.3).

### RAM

- Windows sees the whole VM as one process (`vmmem` / `VmmemWSL`), page cache included. `autoMemoryReclaim` defaults to `dropCache` since [May 2024](https://devblogs.microsoft.com/commandline/whats-new-in-the-windows-subsystem-for-linux-in-may-2024/).
- **The VM has its own ceiling.** `.wslconfig` `memory=` defaults to 50 % of Windows RAM ([wsl-config](https://learn.microsoft.com/en-us/windows/wsl/wsl-config)). WSL workloads can therefore exhaust the VM while the host still has memory free. The VM's OOM killer then strikes inside it.
- **Per-workload RAM is available inside the VM**, through cgroup v2 ([kernel doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)):
  - `memory.current` is the total of the cgroup and its descendants, page cache included;
  - `memory.stat` splits it into `anon`, `file` and `inactive_file`;
  - `memory.events` counts `oom_kill`.
  - Docker's `docker stats` reports `memory.current − inactive_file` ([docker stats](https://docs.docker.com/reference/cli/docker/container/stats/)).
  - A systemd scope gives each workload its own cgroup, and that cgroup covers vLLM's worker processes too. The native path counts only the child's own PID (§13).

### Host requirements

- **Everything WSL is per Windows user:**
  - distros are registered under HKCU;
  - every user SID gets its own utility VM;
  - `.wslconfig` is read from that user's profile *(source-read, `registry.cpp`, `LxssUserSessionFactory.cpp`, `helpers.cpp`)*.
- **LocalSystem is refused:** `WSL_E_LOCAL_SYSTEM_NOT_SUPPORTED`, "Running WSL as local system is not supported." *(source-read)*. An agent installed as a Windows service with NSSM runs as LocalSystem unless configured otherwise: "Services, unless explicitly configured otherwise, run under the LOCALSYSTEM account" ([NSSM usage](https://nssm.cc/usage)). In `wsl` mode the agent service must run under the Windows user that owns the distro.
- **Session 0** (services, tasks that run "whether logged on or not", SSH) is unreliable for `wsl.exe`. [WSL#9231](https://github.com/microsoft/WSL/issues/9231), "Store WSL isn't accessible from Session 0", has been open since 2022 ("Known issue, we're working on a solution"). Reports since then disagree by Windows edition. **Unverified on Windows 11 Pro** (Phase 0, B4). If it fails, a per-user helper in the interactive session is needed. That would be a larger design and gets its own phase.
- **Model files** belong in the distro's ext4 filesystem, not on `/mnt/c`: "For the fastest performance speed, store your files in the WSL file system" ([filesystems](https://learn.microsoft.com/en-us/windows/wsl/filesystems)).

## Goals

- A spec with `execution.mode = "wsl"` goes through the same lifecycle as a native one:
  - the same nine states, crash backoff, idle unload, pinning, instances (§6, §6.1);
  - manual start and stop (ADR-095);
  - the router, heartbeats and the lazy 200 (§4).
- **Per-workload RAM and VRAM are charged so that admission never under-charges** (see "Resource accounting"). RAM is measured per workload. VRAM is measured per workload where the platform allows it. Where it does not, it is accounted for in aggregate, and that aggregate is always charged in full.
- A graceful stop (SIGTERM, then the grace window, then SIGKILL) reaches the **whole** Linux process tree. Nothing is left orphaned after a stop, an agent restart or an agent crash.
- stdout and stderr become the spec's runtime log (§14). The resolved command shows both what the operator wrote and the wrapper that actually ran (§14.7).
- The agent-local policy stays the only authority on what may run (ADR-024): no shell, an allowlist of distros and binaries, and fail-closed defaults.

## Non-goals

- Docker. It gets its own issue (#TBD), which reuses the execution-mode seam.
- WSL1, and Windows Server hosts. Server 2022/2025 is untested here; NVIDIA's WSL guide lists only Windows 10 21H2+ and Windows 11. Server can follow once someone verifies it.
- Installing or configuring WSL, distros, `.wslconfig` or the engines. That stays the operator's job, documented in the README.
- WebSocket endpoints such as sglang-omni's `/v1/realtime`. The router does not upgrade protocols (§4.5).
- Running a second, Linux agent inside WSL. That was considered and rejected: it would be a second admission owner for the same GPUs, which double-books VRAM against the Windows agent's native children.

## Proposed design

### Spec shape

The wire `Spec` and `RuntimeSpec` gain one optional object. When it is absent, the mode is `native`, and the spec is byte-identical to today's:

```json
{
  "binary": "/home/ai/vllm/.venv/bin/vllm",
  "args": ["serve", "${MODEL}", "--host", "127.0.0.1", "--port", "${PORT}", "--gpu-memory-utilization", "0.45"],
  "work_dir": "/home/ai/vllm",
  "env": {"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
  "execution": {"mode": "wsl", "wsl": {"distro": "Ubuntu-24.04"}}
}
```

- In `wsl` mode, `binary` and `work_dir` are **absolute Linux paths**. `args`, `env`, the placeholders (§3.2), `listen_port`, `health_path`, the timeouts, `gpus`, `ram_*` and the instance fields keep their meaning.
- The **Linux user is not a spec field.** It is agent-local configuration, so the gateway can never choose root (see "Agent-local policy").
- **Gateway side:**
  - new columns `agent_runtime_specs.execution_mode text not null default 'native'` and `wsl_distro text not null default ''` (an append-only migration, on all three drivers);
  - DTOs in `internal/portal/service_runtime.go`;
  - the agent document builder.
- Type autodetection (`routing/runtime_spec_type.go`) keeps working: it matches the basename of the Linux binary, so `…/bin/vllm` resolves to `vllm`.
- **Agent side:**
  - `ParseConfig` rejects an unknown mode the way it rejects an unknown `kind` (`withoutInvalidKinds`, `types.go:586-626`);
  - on a non-Windows host, a `wsl` spec is `not_permitted` with a clear message;
  - `execution` is part of the launch shape (`sameLaunchShape`).
- **Negotiation:** a new feature flag, `runtime_exec_wsl` (a MINOR agent bump; ADR-025).
  - **The gateway must never send a `wsl` spec to an agent that does not declare the flag.** An older agent ignores the unknown `execution` field and would treat the spec as native. On a Linux host it would then run an allowlisted Linux binary natively.
  - The gateway-side precedent is `Service.specAsPushed` / `agentRunsInstances` (`gateway/backend/internal/portal/service_runtime.go:3662`, `:3691`). While the agent's features are still unknown, the gateway holds the document with `503 runtime_config.agent_features_pending` (`gateway/backend/internal/gateway/agent_runtime.go:104`).
  - The portal says why a spec is withheld.

### Agent-local policy (ADR-024)

New settings, file and env only, with the same operator-only provenance as `runtime_allowed_binaries`:

| Setting | Rule | Empty means |
|---|---|---|
| `runtime_wsl_allowed_distros` | exact distro names | **nothing starts** in `wsl` mode |
| `runtime_wsl_allowed_binaries` | absolute Linux paths, exact match | **nothing starts** in `wsl` mode |
| `runtime_wsl_allowed_dirs` | Linux work-dir containment, same `withinDir` semantics with `/` | any `work_dir` |
| `runtime_wsl_user` | the Linux user every `wsl.exe` call runs as (`-u`); `root` and UID 0 are refused | required for `wsl` mode |
| `runtime_wsl_exe` | path to `wsl.exe` | `C:\Program Files\WSL\wsl.exe` |

The empty-list asymmetry stays the same as for native specs. So do the error-message rule (name the setting and the count, never the paths) and the reserved environment names.

### Launch

The agent builds the whole wrapper argv itself; nothing in it comes from the gateway except the spec's binary, args and env:

```
wsl.exe -d <distro> -u <runtime_wsl_user> --cd <work_dir> --exec
    systemd-run --user --scope --quiet --unit=opagent-<spec>-<instance>-<generation>
    -- <binary> <args…>
```

- **Invariant: no process that receives gateway-chosen env or argv ever runs as root.**
  - The first Linux process `wsl.exe` starts inherits every `WSLENV` variable, so a root wrapper would run with environment the gateway chose. `LD_PRELOAD` would then give the gateway root in the distro, and the native reserved-name list does not cover it.
  - The chain therefore uses the user's **own systemd manager** (`systemd-run --user --scope`) and never `-u root`. The scope's cgroup is delegated to the user, so it can still be read and signalled.
  - This needs a running user manager. The operator enables it once with `loginctl enable-linger <user>`. The agent sets `XDG_RUNTIME_DIR=/run/user/<uid>` so `systemd-run --user` finds the bus.
  - Whether this works in a stock Ubuntu 24.04 WSL image is **unverified** (Phase 0, B6). The fallback is a small agent-shipped Linux shim that is root-free as well (open question 5).
- **Environment.** The spec's `env`, the agent's GPU variables (`CUDA_VISIBLE_DEVICES`, plus `CUDA_DEVICE_ORDER=PCI_BUS_ID`), `XDG_RUNTIME_DIR`, `PYTHONUNBUFFERED=1` and `WSL_UTF8=1` are set on `wsl.exe`'s environment. Every name the Linux side needs is listed in `WSLENV` with `/u`.
- **Secrets** should travel by environment rather than argv, because an argv inside the VM is visible in `ps`. The rule for `${API_TOKEN}` and `${AGENT_ENV:…}` in `args` stays what it is for native specs: allowed, with the existing portal warning (§3.2). The vLLM preset uses `VLLM_API_KEY` from the environment instead of `--api-key`.
- **Port.** `${PORT}` is chosen on the Windows side, as today (`grabEphemeralPort`). The workload binds `127.0.0.1:${PORT}` inside the VM, and NAT forwarding or mirrored networking makes it reachable from Windows.
- **Config files** (ADR-066). The agent writes them on the Windows side as today, and `${CONFIG_DIR}` expands to the Linux path of that directory (`/mnt/c/…`). Open question 6 covers hosts with automount disabled.

### Lifecycle mapping

| Agent step | Native (Windows today) | `wsl` mode |
|---|---|---|
| start | `exec` the binary | `exec wsl.exe …` (above) |
| terminate (graceful) | none; kill | `wsl.exe -d D -u <user> --exec systemctl --user kill --signal=SIGTERM <unit>.scope` |
| kill grace expired | `Process.Kill()` | `systemctl --user kill --signal=SIGKILL <unit>.scope`, then kill `wsl.exe` as the backstop |
| exit code | process exit code | `wsl.exe` exit; −1 is a `wsl.exe` failure whose message (stdout, UTF-8) goes into `last_error`. **The code alone is ambiguous**: `exit(9)` and death by SIGKILL both return 9 (normal exits return `WEXITSTATUS`, signal deaths the raw signal number). The unit's own record (systemd's unit result, or the cgroup's counters) has to settle it (open question 8). |
| memory-pressure signature (ADR-086) | none on Windows (only the 90 %-of-pool criterion) | an OOM kill recorded for the scope (`memory.events` `oom_kill`, or systemd's unit result), or a last measured RAM of at least 90 % of the **VM** pool |
| orphan sweep | none | at agent start, `systemctl --user list-units 'opagent-*.scope'` in every allowed distro, and each one found is stopped |

The drain grace (10 s) and kill grace (5 s) carry over unchanged. The generation-identity rule (§6) keys on the process record, and the unit name carries the generation, so a late kill can never hit a successor.

### Resource accounting

**This is the part that must not be weakened.** The invariant: *for every GPU, the agent charges WSL workloads at least the VRAM the VM actually holds on that GPU; for RAM, every WSL workload is charged at least its own cgroup's usage, and the WSL set must also fit the VM's ceiling.*

**RAM (Phase 1).**
- The measurement source for a `wsl` instance is its scope's cgroup, not the `wsl.exe` PID. The agent reports `memory.current − inactive_file` (the `docker stats` definition, so both execution modes agree).
- It is read for all scopes in **one** `wsl.exe --exec` call per beat, with an agent-owned fixed argv, off the owner goroutine, under the measurer's 2 s deadline (§5.3).
- The `wsl.exe` PID is **excluded** from the native RAM and VRAM measurers. That removes the false "real" measurement described in Motivation, point 3.
- **Two pools:**
  - The **host pool** (rule 6, ADR-085) is unchanged. WSL memory is part of the VM process, so Windows `MemAvailable` already excludes it, and the existing add-back of measured managed RAM stays valid.
  - A **VM pool** is new: `VM MemAvailable + Σ measured RAM of this VM's workloads − reserve`, read from `/proc/meminfo` inside the VM. A `wsl` candidate must fit both pools. A demand larger than the VM's `MemTotal` on its own is `not_permitted`, mirroring rule 6's own-demand refusal.
- The cold-start bootstrap (ADR-086) works unchanged once a real measurement exists.

**VRAM (Phase 2).**
- **Source:** PDH `\GPU Process Memory(*)\Dedicated Usage` of the WSL VM's worker process (`vmwp`, measured in B1), summed per adapter and mapped to the GPU index through the existing LUID → PCI → index bridge (ADR-031). B1 showed that its LUIDs are the host's.
- **Attribution:**
  - **One WSL workload** using a GPU: the VM's figure on that GPU is that workload's figure. This is exact, apart from any non-agent GPU use inside the VM.
  - **Several:** each workload's figure is its **start delta** per GPU, the VM's figure once the instance has settled after its first passing health probe minus the figure just before exec.
    - WSL **starts** in one VM are serialized: one settle window at a time, through an interlock like the RAM-bootstrap one. Starts take minutes anyway.
    - **Stops are never delayed.** Manual stop (ADR-095), idle unload, eviction, spec removal and, above all, crashes and OOM kills cannot wait behind another workload's settle window.
    - Any other WSL workload that exits during a window therefore **corrects** the delta: its attributed figure is added back. A delta that cannot be corrected cleanly, for example because the exiting workload had no attribution itself, is voided. That workload then falls back to the remainder below until it is measured again.
  - **Remainder: this is what guarantees the invariant.**
    - On every beat, and synchronously at every admission (as the native admission-time measurement already is, §5.3), `R(g) = VM(g) − Σ attributed(g)` is computed from a **fresh** VM reading.
    - A positive remainder is charged to the GPU as occupied. It covers growth after settling, GPU use in the VM the agent did not start, and an attribution that came out too low. Admission therefore never sees free room that does not exist.
    - A negative remainder leaves the attributions alone; the max rule of ADR-084 keeps them.
    - An attribution that came out too **high** over-credits that workload's eviction. That is caught too: the candidate is admitted only after the victim has exited and a fresh reading shows the real free room.
    - Per-workload figures are best effort. The aggregate is what admission relies on.
  - The figures are written back as `vram_measured_mb` (the max-rule floor of ADR-084), so the portal and the next admission see them.
- **What it cannot do, stated plainly:**
  - a per-workload figure is a start delta, not a live per-process reading;
  - growth after settling is visible only in aggregate, through the remainder;
  - a GPU that is also used by non-agent Windows processes behaves as it does today, because the agent never charged those.
- **Until Phase 2 lands**, a `wsl` spec runs on its operator estimate. A spec with an unknown VRAM demand uses the existing unknown-VRAM rules (§5.3) and starts only alone on its GPU.
  - It must also be denied the one-shot bootstrap eviction (ADR-086). Today that grant is **host-wide**: `vramBootstrapEligible` checks only that some measurer is installed (`server-agent/internal/runtime/manager.go:2719-2721`), and a Windows NVIDIA host has the PDH measurer.
  - A `wsl` spec in Phase 1 would therefore be granted an eviction for a measurement that can never arrive. Phase 1 adds a **per-spec "measurable"** condition to that gate.
- **Optional convenience (Phase 3):** the portal can suggest `vram_estimate_mb` from `--gpu-memory-utilization` / `--mem-fraction-static` × the card's total, as a floor only.

### Logs

- `wsl.exe`'s stdout and stderr feed the spec's log buffer as today (§14). With `WSL_UTF8=1`, `wsl.exe`'s own errors arrive readable on stdout.
- The resolved command (§14.7) records the Linux binary, args, work dir and env as resolved (secrets masked as today), plus the wrapper argv and the `WSLENV` list.

### Portal

- In the runtime-spec editor (`RuntimeAdminSection.tsx`), an **execution mode** selector (Native / WSL2), shown only when the server's agent declares `runtime_exec_wsl`, and a **distro** field. In WSL mode the binary and work-dir labels say "Linux path".
- The status row and the Models catalog show a "WSL" marker. Measured VRAM in WSL mode is labelled as attributed from the VM, so an operator can tell it apart from a per-PID reading.
- German and English keys are added together (`i18n.ts`).

### Security

- No shell anywhere: always `--exec`, never `--` or `sh -c` with gateway input. The few agent-owned helper commands (the cgroup read, `systemctl --user`) use fixed argv.
- The gateway can choose only an allowlisted distro and an allowlisted Linux binary. It can never choose the Linux user, and nothing it chooses ever reaches a root process (the invariant under "Launch").
- **The WSL boundary is not a security boundary, and this design does not claim it is.** A Linux process can reach the Windows user's files through `/mnt/c` and, through interop, start Windows executables as that user. A `wsl` workload therefore has the same trust level as a native binary run as the agent's Windows account. That is acceptable only because the agent service now runs as an unprivileged Windows user rather than LocalSystem. The README should recommend a dedicated distro with `[interop] enabled=false` where config files are not needed through `/mnt/c`.
- Secrets: `${AGENT_ENV:NAME}` and `${API_TOKEN}` in `env` travel through `WSLENV`. In `args` they behave as for native specs (allowed, with the portal warning). The upward-report redaction (§8.3) is unchanged.

## Phased plan

**Phase 0: verify on a real Windows 11 host. No product code.**
The owner runs Phase 0 on a Windows 11 Pro AI server with several NVIDIA GPUs, the same host class as ADR-031's measurement. Each result is recorded in this issue before Phase 1 starts, together with the Windows build, the WSL version (`wsl --version`), the GPU count and the driver version. The Phase 1 and Phase 2 acceptance runs use the same host, because CI has no Windows job (§11.3).
- **B1** ✅ Which host process holds the VM's GPU memory, per adapter, and do its LUIDs resolve? Answer: `vmwp`, carrying the host's adapter LUIDs (see "GPU and VRAM").
- **B2** Is the VM's per-GPU figure additive across two vLLM starts, and does it fall when one stops?
- **B3** Multi-GPU: do the GPU indices and PCI bus IDs inside WSL match the host, and does `CUDA_VISIBLE_DEVICES` select the intended card?
- **B4** Does `wsl.exe` work from an NSSM service running as the distro-owning user (Session 0) on Windows 11 Pro?
- **B5** What happens to vLLM, and to its VRAM, when `wsl.exe` is killed?
- **B6** With linger enabled, does `wsl.exe -u <user> --exec systemd-run --user --scope --unit=… -- vllm serve …` work, without root? What do `memory.current`, `anon` and `file` of the scope show, compared with the RSS of the individual vLLM processes? Does `systemctl --user kill --signal=SIGTERM` stop vLLM cleanly and free its VRAM?

**Phase 1: execution-mode seam, WSL lifecycle and RAM.**
- Gateway: migration, store on all three drivers, DTOs, document builder, feature gate.
- Agent:
  - `Spec.Execution`, parse/validate, launch shape;
  - the new local-policy settings;
  - launch through the systemd scope, graceful stop and kill, exit mapping, orphan sweep;
  - exclusion of `wsl.exe` from the PID measurers;
  - cgroup RAM measurement and the VM pool;
  - logs and the resolved command;
  - feature flag `runtime_exec_wsl` and the MINOR bump.
- Portal: mode selector and distro field.
- Docs: a new agent-runtime-manager section, an ADR, and the README (agent service under a user account, `.wslconfig` recommendations).
- Windows-specific logic follows the `nvidia_pdh.go` split: pure logic untagged and unit-tested on Linux CI, with only the syscalls and exec calls behind `//go:build windows` (CI runs no Windows job; §11.3).

**Phase 2: VRAM attribution from the VM process.**
- PDH reading of the VM process;
- start delta under the WSL transition interlock;
- the remainder charge and the write-back;
- the portal's measurement-source label.

**Phase 3: conveniences.**
- vLLM and SGLang presets: `--host 127.0.0.1 --port ${PORT}`, the memory-fraction hint, and the estimate suggestion from that fraction;
- an `sglang` runtime kind with probe paths, if it is worth it.

**Phase 4 (only if B4 fails): a per-user helper** in the interactive session that performs the WSL steps for a service-hosted agent.

## Open questions

1. **Which `vmwp`.** B1 showed that the WSL VM's GPU memory sits on a `vmwp` process. Every Hyper-V VM has its own `vmwp`, though, so a host that also runs other VMs with GPU partitioning has several. How does the agent pick the WSL VM's worker reliably? Candidates: the VM's identity (the `NT VIRTUAL MACHINE\<VM GUID>` account, matched against WSL's utility VM), or the `vmwp` whose usage grows when a WSL start is observed. On a host without other GPU VMs there is only one, which is the case Phase 2 must handle first.
2. *(answered by B1: the LUIDs are the host's.)*
3. **Settle criterion for the start delta.** A fixed time after the first passing health probe, or two equal beats? vLLM captures CUDA graphs before it reports healthy, so the delta is probably stable at that point (unverified).
4. **Session 0.** If B4 fails, is a per-user helper acceptable, or should WSL mode require an interactive logon?
5. **User systemd scope versus a shim.** If `systemd-run --user` is unavailable or unreliable in WSL, should a tiny root-free Linux shim, cross-built from `server-agent`, own the process group and signal forwarding (SIGHUP → SIGTERM with grace) instead? Per-workload RAM would then come from the sum of PSS over the group instead of a cgroup.
6. **Config files on hosts with automount disabled.** Write them into the distro through a fixed `wsl.exe --exec` helper instead of `/mnt/c`?
7. **Host facts upward.** Should the agent report its WSL facts upward so the portal can display them: the VM's `MemTotal`, and whether WSL works for the service account? It must not list the allowlists, for the same reason the work-dir allowlist is never listed (§3.1).
8. **Exit cause after the scope is gone.** A transient scope is removed once its last process exits, so a counter read at exit time may already be gone. Which record survives long enough to tell an OOM kill or a SIGKILL from `exit(9)`: the last per-beat `memory.events` reading, systemd's unit result, or the VM kernel log? To be settled in Phase 1, with B6 as the testbed.

## Architecture touched

- `docs/architecture/cross-cutting/agent-runtime-manager.md`:
  - §3 authority and local policy; §3.2 placeholders; §3.3 visible devices;
  - §4 router; §5.1–5.4 admission, VRAM measurement and the pools;
  - §6 lifecycle; §13 limitations; §14 logs;
  - plus a new section for execution modes.
- ADRs: ADR-024 (authority), ADR-025 (feature flags), ADR-031 (PDH chain, extended to the VM process), ADR-032 (unknown VRAM), ADR-066 (config files), ADR-083/084/085/086 (budgets, the max rule, the RAM pool, the bootstrap), ADR-090 (instances: unit names carry the instance key), ADR-095 (manual start and stop). Plus a **new ADR** for the execution mode and its accounting invariant.
- `docs/architecture/11-risks-and-technical-debt.md` §11.3 (no Windows CI) and §11.4 (deliberate acceptances: start-delta attribution).
- `server-agent/README.md`: the agent service under a user account instead of LocalSystem; WSL host setup.

## Related

- #373 / PR #376: decision endpoints. vLLM is their first runtime, and a WSL-managed vLLM brings them to Windows AI servers.
- #TBD: Docker execution mode for all host OSes (same seam; native Linux Docker can measure VRAM per container exactly).
