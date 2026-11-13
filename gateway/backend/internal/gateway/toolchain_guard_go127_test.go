// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build go1.27

package gateway

// This file makes this package's tests fail to compile on Go 1.27, with a pointer to issue
// #387, instead of letting the live-transcription and Voice Chat loopback tests hang until the
// 10-minute test timeout. The reason is the same as for cmd/gateway/toolchain_guard_go127.go:
// Go 1.27's net/http takes the request body's mutex when it writes the response header of a
// request that sent Expect: 100-continue, and the live relay's full-duplex body reader holds
// that mutex while it waits for more audio. TestFullDuplexExpectContinueHeaderFlush is the
// minimal reproduction.
//
// Run the tests with Go 1.26 instead, for example:
//
//	GOTOOLCHAIN=go1.26.5 go test ./...
//
// Remove this file together with the cmd/gateway guard once a Go release fixes the regression.
var _ = go1_27_deadlocks_live_transcription_see_issue_387_test_with_GOTOOLCHAIN_go1_26_5
