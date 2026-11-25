// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package hostpower

import (
	"context"
	"errors"
	"log/slog"
)

// StartCapture is unavailable off Linux: MAC mode needs an AF_PACKET socket.
func StartCapture(_ context.Context, _ uint16, _ *ObservationTable, _ *slog.Logger) error {
	return errors.New("hostpower: MAC mode needs Linux")
}
