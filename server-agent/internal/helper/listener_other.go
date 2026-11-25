// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package helper

import (
	"errors"
	"log/slog"
	"net"
)

// Listen is Linux-only: peer authorization relies on SO_PEERCRED.
func Listen(string, Identity, *slog.Logger) (net.Listener, error) {
	return nil, errors.New("helper: only supported on Linux")
}
