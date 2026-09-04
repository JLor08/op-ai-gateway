// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// VisibleDevicesMode is the per-spec choice of HOW set_visible_devices is
// enforced when it is on: via the vendor visibility ENVIRONMENT variable
// (today's mechanism) or via a backend device placeholder the operator writes
// into the spec's ARGS (llama.cpp --device). Only meaningful when
// SetVisibleDevices is on; the default, and every pre-feature row, is "env".
// Serialized as its lowercase string, stored text (mirrors EndpointMode).
//
// Validated at the DTO edge (portal.validVisibleDevicesMode) rather than by a
// method here, exactly like EndpointMode.
type VisibleDevicesMode string

const (
	VisibleDevicesModeEnv  VisibleDevicesMode = "env"
	VisibleDevicesModeArgs VisibleDevicesMode = "args"
)
