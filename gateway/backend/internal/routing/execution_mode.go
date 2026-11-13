// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// ExecutionMode is the per-spec choice of WHERE the agent runs the runtime:
// natively on the host ("native", today's behavior and the default of every
// pre-feature row) or inside a WSL distro on a Windows host ("wsl"). Serialized
// as its lowercase string, stored text (mirrors VisibleDevicesMode).
//
// Validated at the DTO edge (portal.validateRuntimeSpecExecution) rather than
// by a method here, exactly like EndpointMode.
type ExecutionMode string

const (
	ExecutionModeNative ExecutionMode = "native"
	ExecutionModeWSL    ExecutionMode = "wsl"
)
