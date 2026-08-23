// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/portal"
)

// proxyStatusReader adapts the gateway-owned *gateway.AgentProxyStatusRegistry
// to portal.ProxyRouteStatusReader, the shape the https-auto-switch reconcile
// consumes. The bridge lives here in the composition root because portal cannot
// import internal/gateway (gateway imports portal); it maps the registry's
// gateway.ProxyRouteStatus snapshot into the portal-owned struct. A nil registry
// (never wired) or a never-reported server yields nil -- "no observation", which
// the reconcile treats as neither a forward nor a revert.
type proxyStatusReader struct {
	reg *gateway.AgentProxyStatusRegistry
}

func (p proxyStatusReader) ProxyRouteStatuses(serverID string) []portal.ProxyRouteStatus {
	src := p.reg.Status(serverID)
	if len(src) == 0 {
		return nil
	}
	out := make([]portal.ProxyRouteStatus, len(src))
	for i, st := range src {
		out[i] = portal.ProxyRouteStatus{Listen: st.Listen, TLSActive: st.TLSActive}
	}
	return out
}
