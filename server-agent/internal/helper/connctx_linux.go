// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import (
	"context"
	"net"
)

// connContext attaches the connection's peer uid (SO_PEERCRED) to ctx.
func connContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	cred, err := peerCred(uc)
	if err != nil || cred == nil {
		return ctx
	}
	return context.WithValue(ctx, peerUIDKey{}, cred.Uid)
}
