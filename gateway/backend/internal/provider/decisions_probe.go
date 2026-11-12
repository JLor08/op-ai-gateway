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
)

// decisionsPath is the OpenAI Decisions endpoint the probe asks. It is the
// same upstream path the gateway relays POST /v1/decisions to.
const decisionsPath = "/v1/decisions"

// The verdicts ProbeOpenAIDecisions states: the capability store's own values.
const (
	DecisionsProbeYes = routing.CapabilityYes
	DecisionsProbeNo  = routing.CapabilityNo
)

// ErrDecisionsProbeRefused is ErrSystemOneProbeRefused for the OpenAI
// Decisions probe: a 400, 405 or 422, the upstream refusing the request
// itself. It wraps ErrUnavailable, so it is no verdict, and only paces the
// caller's retries.
var ErrDecisionsProbeRefused = fmt.Errorf("%w (the upstream refused the probe request)", ErrUnavailable)

// decisionsProbeQuestion is the one question of the probe: a tiny predicate
// over a non-empty input.
const decisionsProbeQuestion = "Is the input non-empty?"

// DecisionsProber is an optional provider capability: it POSTs one tiny OpenAI
// Decisions request for model to target.Endpoint + /v1/decisions and states
// whether the upstream answers that API natively (ADR-098). The rules are those
// of SystemOneProber:
//
//   - a 2xx whose JSON answers is a non-empty array with an object first
//     element: DecisionsProbeYes;
//   - a 404 or a 501: DecisionsProbeNo, except the agent router's own 404 (an
//     error envelope with a runtime.* code), which is no verdict;
//   - a 400, 405 or 422: no verdict, through ErrDecisionsProbeRefused;
//   - anything else is no verdict: "".
type DecisionsProber interface {
	ProbeOpenAIDecisions(ctx context.Context, target routing.Target, model string) (verdict string, err error)
}

var _ DecisionsProber = (*OpenAICompatibleClient)(nil)

// ProbeOpenAIDecisions sends the OpenAI Decisions probe through this client's
// transport. See DecisionsProber.
func (c *OpenAICompatibleClient) ProbeOpenAIDecisions(ctx context.Context, target routing.Target, model string) (string, error) {
	return probeOpenAIDecisions(ctx, c.http, target, model)
}

// decisionsProbeBody is the probe request. The field order is the wire order.
// The question's name must be a string: vLLM rejects null.
type decisionsProbeBody struct {
	Model     string                      `json:"model"`
	Input     string                      `json:"input"`
	Questions []decisionsProbeQuestionDoc `json:"questions"`
}

type decisionsProbeQuestionDoc struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

// probeOpenAIDecisions is the POST behind ProbeOpenAIDecisions, shaped like
// probeSystemOne: it honours target.Timeout, attaches the per-application
// upstream credential from ctx, and bounds the answer at 1 MiB.
func probeOpenAIDecisions(ctx context.Context, httpClient *http.Client, target routing.Target, model string) (string, error) {
	payload, err := json.Marshal(decisionsProbeBody{
		Model: model,
		Input: "ok",
		Questions: []decisionsProbeQuestionDoc{
			{Type: "predicate", Name: "probe", Instructions: decisionsProbeQuestion},
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, decisionsPath), bytes.NewReader(payload))
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
	return decisionsProbeVerdict(resp)
}

// decisionsProbeVerdict classifies the upstream's answer to the probe.
func decisionsProbeVerdict(resp *http.Response) (string, error) {
	switch {
	case resp.StatusCode == http.StatusNotImplemented:
		return DecisionsProbeNo, nil
	case systemOneProbeRefusal(resp.StatusCode):
		return "", fmt.Errorf("%w: upstream status %d", ErrDecisionsProbeRefused, resp.StatusCode)
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
		return DecisionsProbeNo, nil
	}
	var answer struct {
		Answers []json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%w: decode answer: %v", ErrInvalidResponse, err)
	}
	if len(answer.Answers) == 0 || !bytes.HasPrefix(bytes.TrimSpace(answer.Answers[0]), []byte("{")) {
		return "", fmt.Errorf("%w: the answer carries no answer object", ErrInvalidResponse)
	}
	return DecisionsProbeYes, nil
}
