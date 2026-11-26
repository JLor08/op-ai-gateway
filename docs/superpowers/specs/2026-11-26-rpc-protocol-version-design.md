# Design: RPC protocol version in the binary inventory (#441)

Date: 2026-11-26. Branch: `feat/441-rpc-protocol-version`.

## Problem

Automatic RPC cluster membership needs the member's `ggml-rpc-server` to carry
the same binary identity as the head's `llama-server` (commit, else build).
Identity-less builds (source tarball: `version: 1.1.0-dev (build 0, commit
unknown)`) never match, `commit unknown` counts as a real commit, and the
portal hides rejected servers without a reason. llama.cpp's real contract is
the RPC protocol version.

## Research (llama.cpp master, 2026-11-26)

- The llama.cpp semver (`LLAMA_VERSION_*`, e.g. `1.1.0-dev`) is not aligned
  with `RPC_PROTO_*`: 0.3.0, 0.4.1, 0.6.0 and 0.9.0 each span a protocol MAJOR
  change (0.9.0: 8.0.1 → 9.0.0 → 10.0.0). Equal llama major.minor is not a
  usable rule.
- Handshake: only the server reports its version; the client accepts iff
  `server.major == client.major && server.minor <= client.minor`. Patch is not
  checked, yet 8.0.0 → 8.0.1 inserted `GGML_OP_TOP_K_MASK` mid-enum, shifting
  the op numbers — a patch difference can compute wrong results silently.
- `ggml-rpc-server` prints `Starting RPC server vX.Y.Z` on stderr at the start
  of `ggml_backend_rpc_start_server`, before device init and before binding.
  It has no `--version`. `llama-server` does not print its own protocol.

## Decisions

1. **Agent probes native rpc-servers.** For every allowed native `rpc_server`
   the agent runs `<bin> -d CPU -H 127.0.0.1 -p <free port>`, reads combined
   output until the banner line, then kills the process. Timeout
   `binaryVersionTimeout` (10 s), minimal env, cache and retry backoff exactly
   like the `--version` probe (keyed by size and mtime). No banner → no
   protocol. `-d CPU` keeps GPUs uninitialised.
2. **No probe in WSL.** `wsl://` rpc_server entries are never run for the
   protocol (a WSL rpc-server is never a cluster member, and killing a
   process inside a distro is not reliable through `wsl.exe`).
3. **Sibling pairing per directory (both directions).** Binaries in one
   directory are one build:
   - `rpc_server`: version/build/commit from the agent-versioned
     `llama_server` in its directory (as today, else the path); protocol from
     its own probe.
   - `llama_server`: version/build/commit from its own `--version`; protocol
     from the first protocol-carrying `rpc_server` in its directory.
   - No llama-server in the directory → the rpc-server has only a protocol
     (plus whatever its path says, as today). No rpc-server → the
     llama-server has no protocol.
   - Pairing never crosses directories; several installations per host are
     independent.
4. **Wire and storage.** New field `rpc_protocol` (format `X.Y.Z`, else empty)
   on the agent sample `binaries[]` entry, `routing.ServerBinary.RPCProtocol`,
   column `server_binaries.rpc_protocol` (migration 117), and the portal
   binaries DTO. Manual rows have no protocol. Old gateways ignore the field;
   old agents send none → no feature flag. Agent PATCH bump to 0.25.2.
5. **Compatibility of head H (`llama_server`) and member M (`rpc_server`):**
   - both have a protocol: equal (full `X.Y.Z`) → compatible
     (*identical* if the build identity also matches, else *protocol*);
     different → **protocol mismatch**;
   - otherwise (either side lacks a protocol): build identity as today
     (commit, else build) → *identical*; both identities known, comparable and
     different → **build mismatch**; else **unknown**.
6. **Identity hygiene.** Commit `unknown` (any case) and build `0` count as
   absent, also inside a version override.
7. **Head known** = build identity known or protocol known.
8. **Automatic members:** among a server's eligible rpc_server rows, pick a
   compatible one — identical before protocol, then by path. No compatible row
   → the server is still listed, `compatible: false`, with
   `incompatible_reason` ∈ `no_rpc_binary | protocol_mismatch |
   build_mismatch | version_unknown | head_version_unknown` and, for a
   protocol mismatch, `member_rpc_protocol`. The list carries
   `head_rpc_protocol`; a match carries `rpc_binary_protocol`.
9. **Manual members:** the save refuses a protocol or build mismatch (as the
   version mismatch today, naming the protocols for a protocol mismatch); an
   unknown pair keeps the advisory `cluster_member_version_unknown` warning.
10. **Portal:** binaries table and binary picker show the protocol; the
    cluster candidate picker lists rejected automatic candidates, disabled,
    with the reason text instead of hiding them.
11. **Ingest retention:** an `rpc_server` re-report without protocol keeps the
    stored protocol (same trade-off as `keepKnownBinaryVersions`: the runtime
    banner check is the safety net); a `llama_server` row does not keep it
    (its protocol is derived from a sibling that may be gone).
12. **No relaunch:** the protocol is not part of the head's generation hash.

## Out of scope

- Using the handshake's minor/patch tolerance (deliberately stricter: full
  equality, see the mid-enum patch).
- An upstream llama.cpp change; a build→protocol lookup table.
- Probing WSL rpc-servers.

## Acceptance

- Same commit, git vs tarball: matches (both report the same protocol).
- Different commits, same protocol: offered and accepted.
- Different protocol: not offered; shown with "protocol X vs head Y".
- Head without protocol: needs build identity; else shown with a reason.
- Old agents: unchanged except the `unknown`/`0` hygiene.
- Several installations per host pair per directory.
