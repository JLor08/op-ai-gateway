// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/routing"
	"path"
	"strings"
)

// ModelInfo is a single upstream model's discoverable info. ContextSize is the max
// context in tokens (0 = unknown / not reported).
type ModelInfo struct {
	Name        string
	ContextSize int
	// LiveProgressSupport is the upstream build's verdict on the live-progress
	// request parameters (#51), as detected by detectLiveProgressSupport: ""
	// (never determined -- the body wasn't a llama.cpp /props document at all),
	// "supported", or "unsupported". "" must never overwrite an already-stored
	// verdict -- which the row model makes structural rather than a rule every
	// writer remembers: "unknown" is the ABSENCE of a
	// model_mapping_capabilities row, so a "" verdict has nothing to write.
	// See routing.WritableCapabilityRows and the context-probe pass in
	// cmd/gateway/app_health.go.
	LiveProgressSupport string
	// Caps is the auto-detected capability verdict set (#49 sub-project 2), as
	// detected by detectCapabilities. Each field is "" (never determined) |
	// "yes" | "no" (Extra carries capability names with no field of their own).
	// "" must never overwrite an already-stored verdict -- see
	// routing.WritableCapabilityRows, which is where that rule and the
	// operator's precedence rule both live, and the capability write in
	// cmd/gateway/app_health.go that feeds it.
	Caps Capabilities
}

// ModelInfoProber GETs target.Endpoint+probePath and parses model info (currently
// the llama.cpp /props shape: the loaded model's name + n_ctx). A blank probePath
// returns (nil, nil): the feature is off for the app. Tolerant — an unparseable
// body yields nil, never an error.
type ModelInfoProber interface {
	ProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error)
}

func fetchModelInfo(ctx context.Context, httpClient *http.Client, target routing.Target, probePath string) ([]ModelInfo, error) {
	probePath = strings.TrimSpace(probePath)
	if probePath == "" {
		return nil, nil
	}
	if !strings.HasPrefix(probePath, "/") {
		probePath = "/" + probePath
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, probePath), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	return parseModelInfo(body), nil
}

// parseModelInfo reads the llama.cpp /props shape: the loaded model's name (from
// "model", or basename of "model_path") + n_ctx (default_generation_settings.n_ctx,
// else top-level n_ctx) + the live-progress-capability verdict
// (detectLiveProgressSupport) + the capability verdict set (detectCapabilities).
//
// A model NAME is required for the context size but NOT for either verdict, and
// they are reported independently:
//
//   - name present: one entry carrying the name, the size, and both verdicts --
//     the ordinary case, unchanged.
//   - name absent, a live-progress verdict OR any capability determinable: one
//     NAMELESS entry carrying ONLY the verdict(s) -- no context size. Returning
//     nil here (as this did, before either verdict existed) silently discarded a
//     real verdict, because the evidence rule needs no name at all: a verdict is
//     a property of the server BUILD, while a context size is a property of a
//     MODEL. A capability is exactly the same kind of build property as the
//     live-progress verdict -- neither says anything about which model is
//     loaded -- which is why the nameless entry may carry a capability while
//     deliberately still carrying no n_ctx. The agent-side half never had this
//     coupling -- it hands the raw body straight to the detectors.
//   - neither name nor any verdict: nil, exactly as before.
//
// The nameless entry deliberately carries NO context size, even when the body
// reports an n_ctx. An unnamed size cannot be attributed: PickModelContextSize's
// first-positive fallback would hand it to whatever model was probed, and the
// single-probe pass's name equality would hand it to any mapping whose
// AppModelName is empty. The name matching that context attribution needs stays
// exactly as strict as it was.
func parseModelInfo(body []byte) []ModelInfo {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil
	}
	name := ""
	if s, ok := obj["model"].(string); ok && strings.TrimSpace(s) != "" {
		name = strings.TrimSpace(s)
	} else if s, ok := obj["model_path"].(string); ok && strings.TrimSpace(s) != "" {
		name = path.Base(strings.TrimSpace(s))
	}
	support := detectLiveProgressSupport(body)
	caps := detectCapabilities(body)
	if name == "" {
		if support == "" && !capsAny(caps) {
			return nil
		}
		return []ModelInfo{{LiveProgressSupport: support, Caps: caps}}
	}
	nctx := 0
	if dgs, ok := obj["default_generation_settings"].(map[string]any); ok {
		nctx = intFromAny(dgs["n_ctx"])
	}
	if nctx == 0 {
		nctx = intFromAny(obj["n_ctx"])
	}
	return []ModelInfo{{Name: name, ContextSize: nctx, LiveProgressSupport: support, Caps: caps}}
}

// detectLiveProgressSupport is the live-progress-capability detector (#51): it
// decides whether the upstream build tolerates the live-progress request
// parameters (tokens/sec, TTFT) WITHOUT ever sending a request that risks a
// 400 to find out.
//
// The signal is the presence of the key "timings_per_token" inside
// default_generation_settings.params in a llama.cpp /props response. That
// object is a serialization of the COMPILED request-schema field list, so the
// key's presence asserts "this build's completion schema has that field" --
// not merely "this looks like llama.cpp". Presence is ALL that is checked:
// the value is always false (the handler default-constructs the params
// struct), so it carries no information and must never be read.
//
// Returns:
//   - "supported"    default_generation_settings.params is present AND
//     contains the key.
//   - "unsupported"  default_generation_settings.params is present but does
//     NOT contain the key -- a real verdict about a real build (an older
//     llama.cpp).
//   - ""             anything else: unparseable bytes, or a body that simply
//     isn't that document -- a vLLM /v1/models body, an Ollama /api/show
//     body, a TGI /info body, .... This is UNKNOWN, not a verdict, and the
//     caller must never let it overwrite an already-stored verdict. A
//     llama.cpp ROUTER-mode body ("role": "router", issue #55) lands here
//     too, even though it is /props-shaped: it describes the router's own
//     build, not the one serving this model.
//
// Do NOT simplify the "" case to "no key -> unsupported": a vLLM
// application's context probe fetches /v1/models, not /props, and
// timings_per_token is a llama.cpp request-schema field that vLLM's context
// probe was never asking about. Under that simplification every vLLM
// application would be marked unsupported and then silently dropped from the
// live-progress figure by the decision rule -- even though vLLM's own
// equivalent parameter still works.
//
// A sibling copy of this EXACT rule lives in the server-agent module: the two
// are separate Go modules and cannot share code, mirroring the "DUPLICATED
// locally on purpose" precedent at memory_probe.go:111. Whoever changes this
// rule must change that copy identically, or the two halves of this feature
// will drift.
func detectLiveProgressSupport(body []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return ""
	}
	// llama.cpp's ROUTER mode answers /props with a DUMMY document carrying
	// "role": "router" -- the ROUTER's own compiled schema, not that of the
	// server actually serving this model (issue #55). It is no evidence about
	// this model's upstream in either direction, so it yields UNDETERMINED.
	// Reading it as evidence would be worse than reading nothing: a wrong
	// "supported" is absorbed by the streaming retry, while a wrong
	// "unsupported" is PERMANENT and self-reinforcing, because every later
	// probe returns the same dummy and the no-rewrite guard then keeps it.
	if role, ok := obj["role"].(string); ok && role == "router" {
		return ""
	}
	dgs, ok := obj["default_generation_settings"].(map[string]any)
	if !ok {
		return ""
	}
	params, ok := dgs["params"].(map[string]any)
	if !ok {
		return ""
	}
	if _, present := params["timings_per_token"]; present {
		return "supported"
	}
	return "unsupported"
}

// Capabilities is the capability verdict set one probe document yields. Every
// field is "" (this document said nothing about it) | "yes" | "no". The zero
// value means "nothing determined", and no field may be written to storage
// when it is "" -- an undetermined capability simply gets no
// model_mapping_capabilities row (see routing.CapabilityRow).
type Capabilities struct {
	Vision string
	Video  string
	Audio  string
	Tools  string
	Extra  []string
}

// detectCapabilities is the capability detector for a llama.cpp /props
// document (#49 sub-project 2). It reads two objects and nothing else:
//
//   - modalities{vision,video,audio}: the server's own per-modality input
//     support. A key PRESENT as a bool answers yes/no; a key ABSENT from an
//     otherwise present modalities object stays "" -- an older build simply
//     predates it (audio landed 2025-05-23, video 2026-06-08), and absence is
//     not a denial.
//   - chat_template_caps.supports_tools: whether the model's chat template
//     NATIVELY supports tool calls. It is not a claim that tool calls work:
//     with --jinja (llama.cpp's default since 2025-11-27) tools are accepted
//     for every model through a generic handler, so "no" here means degraded
//     prompt quality, not a rejected request. The whole object is absent on
//     servers older than 2026-01-22, which is "" -- not "no".
//
// A caveat that must travel with cap_video wherever it is shown: upstream's
// modalities.video is true when the BINARY was built with video support AND
// the model has a vision encoder (mtmd_helper_support_video returns
// mtmd_support_vision under #ifdef MTMD_VIDEO). It is a build-plus-vision
// fact, not "this model understands video".
//
// The router gate is the same one detectLiveProgressSupport carries and for
// the same reason (#55): llama.cpp's ROUTER mode answers /props with a dummy
// describing the ROUTER's build, not the child's. A wrong verdict read from
// it would be permanent and self-reinforcing, because every later probe
// returns the same dummy and the no-rewrite guard then keeps it.
//
// This is a DUPLICATE, on purpose, of detectCapabilities in
// server-agent/internal/collector/probe.go -- the two are separate Go
// modules and cannot share code, mirroring the "DUPLICATED locally on
// purpose" precedent at gateway/backend/internal/provider/memory_probe.go:111.
// Whoever changes this rule must change that copy identically, or the two
// halves of this feature will drift. A reviewer finding them divergent is a
// real finding; finding them duplicated is expected.
func detectCapabilities(body []byte) Capabilities {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return Capabilities{}
	}
	if role, ok := obj["role"].(string); ok && role == "router" {
		return Capabilities{}
	}
	out := Capabilities{}
	if mods, ok := obj["modalities"].(map[string]any); ok {
		out.Vision = capVerdict(mods["vision"])
		out.Video = capVerdict(mods["video"])
		out.Audio = capVerdict(mods["audio"])
	}
	if caps, ok := obj["chat_template_caps"].(map[string]any); ok {
		out.Tools = capVerdict(caps["supports_tools"])
	}
	return out
}

// capVerdict maps a JSON bool to "yes"/"no" and everything else -- absent,
// null, a string, a number -- to "" (not evidence).
func capVerdict(v any) string {
	b, ok := v.(bool)
	if !ok {
		return ""
	}
	if b {
		return "yes"
	}
	return "no"
}

// capsAny reports whether c carries any determined verdict or Extra entry --
// the "something to report" test parseModelInfo's nameless-entry branch and
// PickModelCapabilities both need. Capabilities carries a []string field
// (Extra), which makes it non-comparable with == (unlike LiveProgressSupport's
// plain string), so this is the explicit field-by-field equivalent of a
// `caps != Capabilities{}` check.
func capsAny(c Capabilities) bool {
	return c.Vision != "" || c.Video != "" || c.Audio != "" || c.Tools != "" || len(c.Extra) > 0
}

// ExpandModelPath substitutes the {model} placeholder in a probe path with the upstream model
// name, so a per-model endpoint (e.g. "/upstream/{model}/props") can be queried. Each "/"-split
// segment of the model name is URL-path-escaped (spaces/specials made safe) while "/" itself is
// kept literal, so a slash-bearing name like "openai/gpt-4o" stays a multi-segment path. Only the
// exact token "{model}" is replaced; any other "{...}" is left as-is. A path without "{model}" is
// returned unchanged.
func ExpandModelPath(path, model string) string {
	if !strings.Contains(path, "{model}") {
		return path
	}
	segs := strings.Split(model, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.ReplaceAll(path, "{model}", strings.Join(segs, "/"))
}

// PickModelContextSize returns the context size for a per-model probe of the given model: an info
// whose Name matches (case-sensitive) wins; otherwise the first info with a positive ContextSize
// (a per-model /props returns one model, so this is that model's value). Returns 0 when nothing
// usable is present.
func PickModelContextSize(infos []ModelInfo, model string) int {
	first := 0
	for _, info := range infos {
		if info.ContextSize <= 0 {
			continue
		}
		if info.Name == model {
			return info.ContextSize
		}
		if first == 0 {
			first = info.ContextSize
		}
	}
	return first
}

// PickModelLiveProgressSupport returns the live-progress-support verdict for a
// per-model probe of the given model: an info whose Name matches
// (case-sensitive) wins; otherwise the first info with a non-empty verdict (a
// per-model /props probe returns one model, so this is that model's value).
// Returns "" (unknown) when nothing usable is present -- callers must never
// let that overwrite an already-stored verdict. Mirrors PickModelContextSize.
func PickModelLiveProgressSupport(infos []ModelInfo, model string) string {
	first := ""
	for _, info := range infos {
		if info.LiveProgressSupport == "" {
			continue
		}
		if info.Name == model {
			return info.LiveProgressSupport
		}
		if first == "" {
			first = info.LiveProgressSupport
		}
	}
	return first
}

// PickModelCapabilities returns the capability verdict set for a per-model
// probe of the given model: an info whose Name matches (case-sensitive) wins;
// otherwise the first info with any non-empty verdict (a per-model /props
// probe returns one model, so this is that model's value). Returns the zero
// value (Capabilities{}) when nothing usable is present -- callers must never
// let that overwrite an already-stored verdict. Mirrors
// PickModelLiveProgressSupport.
func PickModelCapabilities(infos []ModelInfo, model string) Capabilities {
	first := Capabilities{}
	haveFirst := false
	for _, info := range infos {
		if !capsAny(info.Caps) {
			continue
		}
		if info.Name == model {
			return info.Caps
		}
		if !haveFirst {
			first = info.Caps
			haveFirst = true
		}
	}
	return first
}

// intFromAny coerces a JSON number (float64) or int to a non-negative int; anything
// else (or <= 0) -> 0.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return 0
}
