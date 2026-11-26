// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build unix

package hostpower

import "syscall"

// oNonblock makes opening a FIFO in a guest's share return at once instead of
// blocking until a writer appears.
const oNonblock = syscall.O_NONBLOCK
