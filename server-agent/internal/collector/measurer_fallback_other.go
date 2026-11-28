// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows && !linux

package collector

// fallbackVRAMMeasurer is the measurer behind NewVRAMMeasurer on every
// platform that is neither Windows nor Linux: nvidia-smi compute-apps keyed
// nvidia, or none. DRM fdinfo and KFD are Linux kernel interfaces, so there is
// no AMD source here.
func fallbackVRAMMeasurer() VRAMMeasurer {
	return nvidiaVRAMMeasurer(NewNvidiaComputeApps())
}
