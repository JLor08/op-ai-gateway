// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package helper

import (
	"context"
	"net"
)

// connContext is a no-op off Linux, where the helper cannot run.
func connContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
