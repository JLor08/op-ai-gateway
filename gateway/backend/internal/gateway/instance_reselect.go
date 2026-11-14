// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
)

// A request routed to instance k >= 1 of a replicable spec can find the instance
// gone: the gateway chose it from the last telemetry sample, and the agent may have
// retired it (idle scale-down, a lowered max_instances, a restart) since. The agent
// router then refuses the address before anything reaches a child, and the request
// is re-selected once, with that instance excluded -- the client never sees the
// refusal. Instance 0 is never re-selected: its address is the one every mapping has
// always sent, and the agent's refusal of it is an answer about the model, not about
// a vanished instance.
//
// Every path that re-selects -- the buffered and streamed translate paths, every relay
// through proxyNative (passthrough, speech, images, image edit, transcription) and the
// bearer live session -- does it the same way:
//
//  1. The once-rule decides (reselectable): the lost target is an instance k >= 1 and
//     the request carries no exclusion yet. A request that already lost an instance is a
//     re-selection, which is never re-selected again.
//  2. The lost attempt leaves the instance before anything is selected again: its
//     active-request row is removed -- and a live session lets its live slot go first --
//     which frees the attempt's count on the instance and on its server and wakes a
//     request parked in the admission queue for that server. The re-selection may itself
//     park (every other instance busy); it then waits for capacity like any request, not
//     behind its own attempt, which no longer runs anywhere.
//  3. The request is resolved again with the instance excluded (withLostInstance) and
//     registered on the instance it is sent to.
//
// A request that cannot move off a gone instance -- it was re-selected already, or the
// instance it was re-selected onto is one its relay cannot take, or its session's audio
// can no longer be replayed -- is answered with the gateway's own error for the refusal
// (goneError: provider.unavailable, the code a chat completion gets for it), never the
// agent router's envelope, whose runtime.* codes are the agent's internals. One whose
// re-resolve is refused by routing (no host left, a full or timed-out queue) gets that
// routing answer -- except a translated stream (streamSession.reselect), which re-resolves
// from inside the stream it has begun: a refused re-resolve registers it back on the
// instance it lost, and the stream ends with the original gone error.
//
// The voice-chat gate re-selects as well, when the agent refuses the instance it ensures
// for a leg (ensureVoiceLeg): the same once-rule, no attempt to leave since the gate sends
// no request, and a refused re-resolve ends the leg with the original gone error too.

// isInstanceGoneError reports whether err is the agent router's refusal of an
// instance address it does not serve: 404 runtime.model_not_managed (the address is
// unknown, or no longer allowed) or runtime.model_not_running (the /live route's
// answer for an allowed instance that is not running), and 409
// runtime.instance_no_capacity (a burst instance that is not serving, or an address
// that moved during admission) or runtime.instance_demand_unknown. It reads the
// refusal's code and status only; whether the request addressed an instance k >= 1
// is the caller's to check.
func isInstanceGoneError(err error) bool {
	var re *provider.RouterError
	if !errors.As(err, &re) {
		return false
	}
	switch re.Status {
	case http.StatusNotFound:
		return re.Code == "runtime.model_not_managed" || re.Code == "runtime.model_not_running"
	case http.StatusConflict:
		return re.Code == "runtime.instance_no_capacity" || re.Code == "runtime.instance_demand_unknown"
	}
	return false
}

// reselectable is the once-rule every re-selecting path applies: a request whose
// attempt on target found the instance gone moves to another instance only when target
// is an instance k >= 1 and req carries no exclusion yet -- a request carrying one is a
// re-selection, which is never re-selected again.
func reselectable(target routing.Target, req inference.Request) bool {
	return target.Instance() >= 1 && len(req.ExcludeInstanceKeys) == 0
}

// reselectsLostInstance reports whether a request that failed with err on target is
// re-selected: the once-rule allows it (reselectable) and err says the instance is gone.
func reselectsLostInstance(target routing.Target, req inference.Request, err error) bool {
	return reselectable(target, req) && isInstanceGoneError(err)
}

// withLostInstance returns req with lost's instance, found gone, added to the instances
// its selections must not pick. The slice is copied, so the caller's request is
// untouched. The /v1/realtime target selection (selectRealtimeTarget) excludes with it
// too: there the instance is one that cannot serve the session (its agent has not
// declared realtime_proxy), not one found gone.
func withLostInstance(req inference.Request, lost routing.Target) inference.Request {
	req.ExcludeInstanceKeys = append(slices.Clone(req.ExcludeInstanceKeys), lost.InstanceKey)
	return req
}

// instanceGonePeekBytes bounds how much of a relayed refusal is read to recognise the
// router's error envelope, a single short line.
const instanceGonePeekBytes = 4 << 10

// peekInstanceGone reports whether resp, an answer the gateway relays itself, is the
// agent router's refusal of a gone instance (isInstanceGoneError). Only a 404 or 409
// is read at all. An answer that is not such a refusal gets what was read put back in
// front of the rest of its body, so it relays exactly as it arrived. One that is gets
// its whole envelope -- a single short line, already read -- as an in-memory body
// (which goneError reads), and its connection is released at once: the request moves
// on to another attempt, which must not hold this one open.
func peekInstanceGone(resp *provider.ProxyResponse) bool {
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusConflict {
		return false
	}
	orig := resp.Body
	head, err := io.ReadAll(io.LimitReader(orig, instanceGonePeekBytes))
	if err != nil || !isInstanceGoneError(provider.RouterStatusError(resp.StatusCode, head)) {
		resp.Body = replayedBody{Reader: io.MultiReader(bytes.NewReader(head), orig), Closer: orig}
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(orig, instanceGonePeekBytes))
	_ = orig.Close()
	resp.Body = io.NopCloser(bytes.NewReader(head))
	return true
}

// goneError is the error a gone instance's refusal resp maps to, once peekInstanceGone
// has recognised it: the router's code kept as a *provider.RouterError, whose status
// maps to provider.unavailable -- what the gateway answers for it (completionErrorCode)
// when the request cannot move to another instance, exactly as a chat completion's
// refusal is answered. It reads the envelope peekInstanceGone left in memory.
func goneError(resp *provider.ProxyResponse) error {
	body, _ := io.ReadAll(resp.Body)
	return provider.RouterStatusError(resp.StatusCode, body)
}

// relayAgain is the nativeRelay reselect hook of a caller whose path always answers:
// it runs again(req with lost's instance excluded), whose resolve, relay and usage row
// are the request's from then on.
func relayAgain(req inference.Request, again func(inference.Request)) func(lost routing.Target, gone error) {
	return func(lost routing.Target, _ error) {
		again(withLostInstance(req, lost))
	}
}
