# Plan: ROCm member VRAM exemption

Spec: `docs/superpowers/specs/2026-11-26-rocm-member-vram-exemption.md`. Branch-local.

## Phase 1 — backend rule (TDD)

Files: `gateway/backend/internal/portal/service_cluster.go`,
`service_rpc_members.go`, their `_test.go` files.

1. RED: add tests
   - cluster save, automatic member on a Linux report with two "AMD Radeon RX 6900 XT"
     GPUs (with pci_bus_id), no estimate, head with `--tensor-split` absent:
     - device_backend `rocm` → saves (no estimate / tensor-split refusal);
     - device_backend `auto` → saves;
     - device_backend `vulkan` → `ErrRPCMemberVRAMEstimateRequired`;
     - same report with `os: windows` + `rocm` → `ErrRPCMemberVRAMEstimateRequired`;
     - mixed report (one NVIDIA + one AMD) + `rocm` on the AMD index → still refused.
     - with an estimate given and backend `vulkan`, no `--tensor-split` →
       `ErrClusterTensorSplitRequired` (unchanged), with `rocm` → saves.
   - manual member spec PUT (requireRPCMemberVRAMEstimates): Linux AMD report + `rocm`
     or `auto` without estimate → ok; `vulkan` → refused; Windows → refused.
   - manual member used by a head: its tensor-split requirement follows its stored
     spec's DeviceBackend the same way.
   Follow the existing fixtures in the test files (they already seed AMD reports
   with pci_bus_id from #433). Run and confirm the new cases fail for the right reason.
2. GREEN: one shared predicate, e.g. extend `hostGPU` with `amd bool` and keep the
   report's OS / an `allAMD` flag per server, plus a helper
   `memberGPUMeasurable(host serverGPUs, idx int, backend string) bool`. Use it in
   `checkClusterMemberGPUs` (estimate + tensor-split; manual entries take the backend
   from `e.spec.DeviceBackend`, automatic from `e.req.DeviceBackend`) and in
   `requireRPCMemberVRAMEstimates` (pass the request's device backend; check the caller).
   Reuse the existing AMD keyword list from `gpuLooksNVIDIA` (extract `gpuLooksAMD`).
   Update the doc comments on both checks and on `ErrRPCMemberVRAMEstimateRequired`.
3. Run `go test ./internal/portal/ ./internal/gateway/` and
   `golangci-lint fmt --diff` + `golangci-lint run ./...` in `gateway/backend`.

## Phase 2 — messages and docs

1. Backend message in `internal/gateway/portal_rpc_member_endpoints.go` (and any
   other mapping of the two codes, grep `cluster_tensor_split_required`).
2. Frontend `gateway/frontend/src/i18n.ts`: `errorRuntimeSpecMemberVramEstimateRequired`
   and `errorClusterTensorSplitRequired`, DE + EN (texts in the spec). Check UI hint texts
   in the cluster/member forms that state the old rule (grep i18n for "NVIDIA").
   Run `npm test -- format i18n`, `npm run build`, `npm run lint`, `npm run format:check`.
3. Architecture docs per the spec's Docs list; `make lint-docs`.
