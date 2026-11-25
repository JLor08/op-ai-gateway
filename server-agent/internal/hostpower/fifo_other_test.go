// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !unix

package hostpower

import "errors"

func mkfifo(string) error { return errors.ErrUnsupported }
