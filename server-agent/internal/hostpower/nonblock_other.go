// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !unix

package hostpower

// oNonblock is 0 where the OS has no O_NONBLOCK open flag (and no FIFOs a
// guest could place in a share); the Lstat and Stat regular-file checks and
// the bounded read still apply.
const oNonblock = 0
