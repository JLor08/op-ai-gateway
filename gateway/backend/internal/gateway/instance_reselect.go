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

// reselectsLostInstance reports whether a request that failed with err on target is
// re-selected: target is instance k >= 1, err says that instance is gone, and the
// request has not been re-selected already -- a request carrying an exclusion is a
// re-selection, which is never re-selected again.
func reselectsLostInstance(target routing.Target, req inference.Request, err error) bool {
	return target.Instance() >= 1 && len(req.ExcludeInstanceKeys) == 0 && isInstanceGoneError(err)
}

// withLostInstance returns req with lost's instance, found gone, added to the instances
// its selections must not pick. The slice is copied, so the caller's request is
// untouched.
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
// its whole envelope -- a single short line, already read -- as an in-memory body, and
// its connection is released at once: the request moves on to another attempt, which
// must not hold this one open, and the refusal can still be relayed if no other
// attempt is found.
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

// relayAgain is the nativeRelay reselect hook of a caller whose path always answers:
// it runs again(req with lost's instance excluded) and reports true. nil when req is
// itself a re-selection, which is never re-selected again.
func relayAgain(req inference.Request, again func(inference.Request)) func(lost routing.Target) bool {
	if len(req.ExcludeInstanceKeys) > 0 {
		return nil
	}
	return func(lost routing.Target) bool {
		again(withLostInstance(req, lost))
		return true
	}
}
