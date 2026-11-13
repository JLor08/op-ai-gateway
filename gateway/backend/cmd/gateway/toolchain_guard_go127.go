// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build go1.27

package main

// This file stops the gateway binary from being built with Go 1.27 (issue #387).
//
// Go 1.27's net/http (golang/go commit f5c3217500dc, "net/http: don't rely on server request
// body not changing") makes chunkWriter.writeHeader call body.bodyRemains(), which takes the
// request body's mutex, for every request that sent Expect: 100-continue. body.Read holds that
// mutex for the whole blocking network read. The live-transcription relay
// (/v1/audio/transcriptions/live, also behind every Voice Chat turn) reads the request body in
// full duplex during the upstream dial, so its first SSE flush blocks until the client sends
// more audio -- and a client that waits for the SSE response first deadlocks. Go 1.26 read a
// lock-free flag there.
//
// Build with Go 1.26 instead, for example:
//
//	GOTOOLCHAIN=go1.26.5 go build ./cmd/gateway
//
// Remove this file (and internal/gateway/toolchain_guard_go127_test.go) only once a Go
// release no longer takes the body lock on that header-write path and the TestVoice* and
// TestLiveTranscriptionLoopback* tests in internal/gateway pass on it.
// docs/architecture/cross-cutting/development-and-quality.md, section 1, has the details.
var _ = go1_27_deadlocks_live_transcription_see_issue_387_build_with_GOTOOLCHAIN_go1_26_5
