// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

// This file only carries the last go:generate step of the package: its name sorts
// after every generated file, so the SPDX header is re-applied after gowrap ran
// (see generate.go).

//go:generate bash ../../../../scripts/add-license-headers.sh routingstore_gen.go ../account/api_tracing_gen.go ../portal/api_tracing_gen.go
