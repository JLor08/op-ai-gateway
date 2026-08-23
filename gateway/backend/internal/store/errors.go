// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import "op-ai-gateway/internal/storeerr"

var (
	ErrNotFound = storeerr.ErrNotFound
	ErrConflict = storeerr.ErrConflict
)
