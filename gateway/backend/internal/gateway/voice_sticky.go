// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
)

// A voice chat keeps its turns on the instances it already uses. Each leg of the loop --
// the ASR, the LLM and the TTS -- remembers the gateway instance key (routing.InstanceKey)
// its last request ran on, and every request of that leg names it as the instance it
// prefers (preferredInstanceHeaderName); /v1 answers with the instance it routed the
// request to (servedInstanceHeaderName), which becomes the leg's next preference. Before
// the first turn each leg prefers the instance the ticket's gate selected -- for the TTS
// the one the pre-warm primed. The preference is advisory: routing honours it only for an
// instance of a replicable spec that is still a candidate, under its cap, through every
// gate and where the plain selection would not move the request off it for a loaded model
// or a swap (inference.Request.PreferredInstanceKey), and a leg moved elsewhere sticks to
// its new instance from then on. A leg served by a mapping without instances is never
// sticky (stickyInstance). No route_affinity row is involved: the legs run under a session
// principal without a token id.

// voiceStickiness is one voice chat session's per-leg instance memory. The ASR leg's dial
// goroutine and the voice run's LLM and speech goroutines read and write it concurrently,
// hence the lock. A nil *voiceStickiness -- a run or loopback outside a voice chat --
// prefers nothing and remembers nothing.
type voiceStickiness struct {
	mu   sync.Mutex
	last map[voiceLeg]string
}

// newVoiceStickiness starts a session's memory at the gate's targets: each leg prefers the
// instance its ticket was resolved (and, for the TTS, pre-warmed) on. A leg whose target is
// a mapping without instances (routing.Target.InstanceCapable false) starts with no
// preference and, since /v1 reports no instance for it, keeps none.
func newVoiceStickiness(claims voiceTicketClaims) *voiceStickiness {
	return &voiceStickiness{last: map[voiceLeg]string{
		voiceLegASR: stickyInstance(claims.ASR),
		voiceLegLLM: stickyInstance(claims.LLM),
		voiceLegTTS: stickyInstance(claims.TTS),
	}}
}

// stickyInstance is the instance key a leg resolved to target prefers next: target's own
// when it is an instance of a replicable spec, "" for any other target, which routing
// never honours a preference for.
func stickyInstance(target routing.Target) string {
	if !target.InstanceCapable {
		return ""
	}
	return target.InstanceKey
}

// prefer names leg's last instance on req, the loopback request about to be sent for it.
func (v *voiceStickiness) prefer(req *http.Request, leg voiceLeg) {
	if v == nil {
		return
	}
	v.mu.Lock()
	key := v.last[leg]
	v.mu.Unlock()
	if key != "" {
		req.Header.Set(preferredInstanceHeaderName, key)
	}
}

// observe remembers the instance /v1 reports it routed leg's request to (resp, a 2xx
// answer to a request prefer marked). An answer that names none -- a request served by a
// relay that does not report it -- leaves the leg's preference as it was.
func (v *voiceStickiness) observe(resp *http.Response, leg voiceLeg) {
	if v == nil || resp == nil {
		return
	}
	key := strings.TrimSpace(resp.Header.Get(servedInstanceHeaderName))
	if key == "" {
		return
	}
	v.mu.Lock()
	v.last[leg] = key
	v.mu.Unlock()
}

// markServedInstance tells the caller of a request that preferred an instance
// (inference.Request.PreferredInstanceKey -- set only on the internal loopback pair) which
// instance it was routed to, on the response w is about to write, when that is an instance
// of a replicable spec (stickyInstance). Every other request is answered as before. It
// must run before the response's headers are written; a relay that
// moves the request onto another instance afterwards reports the first one, which the
// leg then prefers next time and routing passes over when it is gone.
func markServedInstance(w http.ResponseWriter, req inference.Request, target routing.Target) {
	key := stickyInstance(target)
	if req.PreferredInstanceKey == "" || key == "" {
		return
	}
	w.Header().Set(servedInstanceHeaderName, key)
}
