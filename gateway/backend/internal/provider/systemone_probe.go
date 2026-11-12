// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
)

// systemOnePath is the System One decision endpoint the probe asks. It is the
// same upstream path the gateway relays POST /v1/systemone to.
const systemOnePath = "/v1/systemone"

// The verdicts ProbeSystemOne states. They are the capability store's own
// values (routing.CapabilityYes / routing.CapabilityNo), so the caller writes
// them as they come.
const (
	SystemOneProbeYes = routing.CapabilityYes
	SystemOneProbeNo  = routing.CapabilityNo
)

// ErrSystemOneProbeRefused marks a probe answer that is no verdict and will
// not change on its own: a 400, 405 or 422, the upstream refusing the request
// itself. It wraps ErrUnavailable like every other no-verdict error, and only
// paces the caller's retries (the health loop stretches its back-off for it);
// a timeout, a 408, a 429 or a 5xx can pass on their own and is not marked.
var ErrSystemOneProbeRefused = fmt.Errorf("%w (the upstream refused the probe request)", ErrUnavailable)

// systemOneProbeRefusal reports whether status is one ErrSystemOneProbeRefused
// marks.
func systemOneProbeRefusal(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// systemOneProbeQuestion is the one question of the probe: a tiny noul
// question about a non-empty state, which any System One server answers
// without loading anything the model does not already hold.
const systemOneProbeQuestion = "Is the state non-empty?"

// SystemOneProber is an optional provider capability: it POSTs one tiny System
// One request for model to target.Endpoint + /v1/systemone and states whether
// the upstream answers decisions:
//
//   - a 2xx whose JSON answers.probe is an object: SystemOneProbeYes;
//   - a 404 (no such route) or a 501 (a model the server cannot read):
//     SystemOneProbeNo;
//   - anything else -- a transport failure or timeout, a 400, a 5xx other than
//     501, a 2xx without the answer or that is not JSON -- is no verdict: "".
//
// One 404 is not a no: the agent router's OWN 404, its error envelope with a
// runtime.* code (runtime.model_not_managed for a model whose spec it no longer
// holds, say), comes from the router before any child was asked, so it says
// nothing about the model and is no verdict ("", ErrUnavailable). A 501 is a
// no whatever its body.
//
// "" is the only verdict that is not an answer, so the caller writes nothing
// for it and asks again later. err is nil for both real verdicts and, for "",
// says why there was none: for the log, and, through ErrSystemOneProbeRefused
// (a 400, 405 or 422), for how long the caller waits before asking again. A
// caller decides what to write by the verdict alone.
//
// It is a client method, like ProbeSdcppCapabilities, because the request must
// ride the client's own *http.Client, which cmd/gateway builds on the outbound
// app transport.
type SystemOneProber interface {
	ProbeSystemOne(ctx context.Context, target routing.Target, model string) (verdict string, err error)
}

var _ SystemOneProber = (*OpenAICompatibleClient)(nil)

// ProbeSystemOne sends the System One probe through this client's transport.
// See SystemOneProber.
func (c *OpenAICompatibleClient) ProbeSystemOne(ctx context.Context, target routing.Target, model string) (string, error) {
	return probeSystemOne(ctx, c.http, target, model)
}

// systemOneProbeBody is the probe request. The field order is the wire order.
type systemOneProbeBody struct {
	Model     string                               `json:"model"`
	State     string                               `json:"state"`
	Questions map[string]systemOneProbeQuestionDoc `json:"questions"`
}

type systemOneProbeQuestionDoc struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

// systemOneProbeAnswer is the part of the answer the probe reads.
type systemOneProbeAnswer struct {
	Answers struct {
		Probe json.RawMessage `json:"probe"`
	} `json:"answers"`
}

// agentRouterErrorCodePrefix starts every error code the server agent's
// runtime router writes itself (its errorEnvelope), as opposed to an answer
// the upstream child gave.
const agentRouterErrorCodePrefix = "runtime."

// agentRouterErrorCode returns the error.code of body when body is the agent
// router's own error envelope ({"error":{"code":"runtime.…","message":…}}),
// and "" for any other body, an empty or unparseable one included.
func agentRouterErrorCode(body []byte) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || !strings.HasPrefix(envelope.Error.Code, agentRouterErrorCodePrefix) {
		return ""
	}
	return envelope.Error.Code
}

// probeSystemOne is the POST behind ProbeSystemOne. Modelled on
// fetchSdcppDoc: it honours target.Timeout, attaches the per-application
// upstream credential from ctx, and bounds the answer at 1 MiB.
func probeSystemOne(ctx context.Context, httpClient *http.Client, target routing.Target, model string) (string, error) {
	payload, err := json.Marshal(systemOneProbeBody{
		Model: model,
		State: "ok",
		Questions: map[string]systemOneProbeQuestionDoc{
			"probe": {Type: "noul", Instructions: systemOneProbeQuestion},
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode request: %v", ErrUnavailable, err)
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, systemOnePath), bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	req.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotImplemented:
		return SystemOneProbeNo, nil
	case systemOneProbeRefusal(resp.StatusCode):
		return "", fmt.Errorf("%w: upstream status %d", ErrSystemOneProbeRefused, resp.StatusCode)
	case resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode >= 300):
		return "", unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		if code := agentRouterErrorCode(body); code != "" {
			return "", fmt.Errorf("%w: the agent router answered 404 %s", ErrUnavailable, code)
		}
		return SystemOneProbeNo, nil
	}
	var answer systemOneProbeAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%w: decode answer: %v", ErrInvalidResponse, err)
	}
	if probe := bytes.TrimSpace(answer.Answers.Probe); len(probe) == 0 || probe[0] != '{' {
		return "", fmt.Errorf("%w: the answer carries no probe answer object", ErrInvalidResponse)
	}
	return SystemOneProbeYes, nil
}
