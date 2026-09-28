// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import "context"

type streamActivityKey struct{}

// WithStreamActivity returns ctx carrying fn, a hook that
// OpenAICompatibleClient.CompleteStream calls once for every SSE comment line
// it reads from the upstream (a line starting with ':', such as the agent
// router's `: keepalive` heartbeat), at the point in the stream where the line
// arrives. A comment carries no event, so the hook is the only thing that sees
// it: emit, the tracked usage and the live-progress retry behave exactly as
// without it. Native passthrough (ProxyNative) never calls the hook.
//
// fn runs on the goroutine that reads the stream, so it must return quickly. A
// nil fn leaves ctx unchanged.
func WithStreamActivity(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, streamActivityKey{}, fn)
}

// StreamActivityFrom returns the hook carried by ctx (via WithStreamActivity), or
// nil when none is present.
func StreamActivityFrom(ctx context.Context) func() {
	fn, _ := ctx.Value(streamActivityKey{}).(func())
	return fn
}
