// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// SafeProbePath reports whether p is a safe relative probe path to append to
// the agent's own "http://127.0.0.1:PORT" loopback base: empty (nothing to
// probe), or a single-"/"-rooted path carrying no scheme, no protocol-relative
// "//" authority, and no whitespace/control bytes. It is the agent's
// defense-in-depth SSRF guard, mirroring the portal's safeRelativeProbePath:
// concatenating a value like "@evil:9999/x", "//evil", or "http://evil" onto
// the loopback base would otherwise re-parse to an off-loopback Host and turn
// a local probe into an outbound request. probeRuntimeChild skips a probe
// whose path fails this check, so even a bad path that somehow reached the
// agent never dials off-loopback. Valid paths ("/metrics", "/v1/models",
// "/props", "/api/show") all pass.
func SafeProbePath(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	if strings.Contains(p, "://") {
		return false
	}
	for i := 0; i < len(p); i++ {
		// Reject every byte at or below ASCII space (control chars, tab, CR,
		// NL, and space itself) and DEL.
		if b := p[i]; b <= 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// ProbeContext fetches baseURL+contextPath and extracts the served model's
// context length, per specType's JSON convention. specType is the resolved
// lowercase RuntimeSpecType string ("vllm" | "llama_cpp" | "tgi" | "ollama" |
// "custom" | ""); the collector package does not import the gateway routing
// package, so this is a plain string rather than a shared type.
//
// Every type but "ollama" issues a bodyless GET, as it always has. "ollama"
// is the one deliberate exception (issue #54): Ollama's /api/show is
// POST-only -- since server v0.7.0 it answers a GET with a 405 (text/plain,
// no context data at all; before that, a 404) via Go's
// http.ServeMux-equivalent HandleMethodNotAllowed, so a GET can never
// succeed against it. ProbeContext therefore POSTs {"model": "<model>"} for
// this one type, built with json.Marshal (never string concatenation -- a
// model name can contain characters, e.g. a literal '"', that need
// escaping). model is REQUIRED for this type: an empty model returns
// ErrOllamaModelRequired without issuing any request at all, because
// Ollama's own answer to a modelless POST is a 400 "model is required" --
// sending it would only spend a round trip to learn nothing conclusive, and
// a clear local error beats a misleading upstream one. Every other type
// ignores model entirely, including a non-empty one -- passing a model for
// a non-ollama spec is harmless, not an error.
//
// Extraction rules verified against upstream sources on 2026-09-07:
//
//   - vllm: GET /v1/models -> {"data":[{"max_model_len": N, ...}, ...]}.
//     max_model_len is a top-level field on each ModelCard entry.
//     Source: vllm-project/vllm, vllm/entrypoints/serve/engine/protocol.py
//     (ModelCard.max_model_len),
//     https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/serve/engine/protocol.py
//
//   - llama_cpp: GET /props -> {"default_generation_settings":{"n_ctx": N,
//     ...}, ...}. n_ctx is NESTED under default_generation_settings, not a
//     top-level field (the naive top-level assumption is wrong).
//     Source: ggml-org/llama.cpp, tools/server/README.md,
//     https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
//
//   - tgi: GET /info -> {"max_total_tokens": N, ...}. max_total_tokens is a
//     top-level field of the Info response.
//     Source: huggingface/text-generation-inference, docs/openapi.json
//     "Info" schema,
//     https://github.com/huggingface/text-generation-inference/blob/main/docs/openapi.json
//
//   - ollama: POST /api/show, body {"model": "<model>"} ->
//     {"model_info":{"<arch>.context_length": N, ...}, ...}. The key is
//     architecture-prefixed (e.g. "llama.context_length",
//     "qwen2.context_length"), so it is matched by suffix, not an exact key.
//     Source: ollama/ollama, docs/api.md "Show Model Information",
//     https://github.com/ollama/ollama/blob/main/docs/api.md
//
//   - "custom", "", and any other value: best-effort — scans the decoded
//     JSON body at any depth for the first key matching n_ctx, max_model_len,
//     or context_length (exactly, or by a ".context_length" suffix to also
//     catch the ollama-style architecture-prefixed key).
//
// Missing or unparseable data returns (0, err); ProbeContext never panics.
//
// client is the HTTP client used to issue the request. The caller (agent.go's
// probeRuntimeChild) passes its private keep-alives-disabled client -- the
// same one used for the metrics scrape -- because a managed child's loopback
// port is an OS-assigned, recyclable ephemeral port: a keep-alive connection
// left open past a child's restart/exit could otherwise be transparently
// reused against a different process later assigned that same port. A nil
// client falls back to http.DefaultClient, for existing/incidental callers
// that have no such concern.
func ProbeContext(ctx context.Context, client *http.Client, baseURL, specType, contextPath, model string) (int, error) {
	path := strings.TrimSpace(contextPath)
	if path == "" {
		return 0, fmt.Errorf("probe context: no context path configured")
	}

	normalizedType := strings.ToLower(strings.TrimSpace(specType))

	var (
		body []byte
		err  error
	)
	if normalizedType == "ollama" {
		model = strings.TrimSpace(model)
		if model == "" {
			return 0, ErrOllamaModelRequired
		}
		reqBody, merr := json.Marshal(struct {
			Model string `json:"model"`
		}{Model: model})
		if merr != nil {
			return 0, fmt.Errorf("probe context: %w", merr)
		}
		body, _, err = fetchProbeBodyWith(ctx, client, baseURL, http.MethodPost, path, reqBody)
	} else {
		body, _, err = fetchProbeBody(ctx, client, baseURL, path)
	}
	if err != nil {
		return 0, fmt.Errorf("probe context: %w", err)
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, fmt.Errorf("probe context: parse response: %w", err)
	}

	n, ok := extractContext(normalizedType, v)
	if !ok {
		return 0, fmt.Errorf("probe context: no context field found for spec type %q at %s", specType, path)
	}
	return n, nil
}

// ErrOllamaModelRequired is the distinct error ProbeContext returns for the
// "ollama" spec type when no model name is available, WITHOUT issuing any
// request: Ollama's POST /api/show answers a modelless request with 400
// "model is required", so sending it would only spend a round trip to learn
// nothing conclusive that this local check does not already know. Callers
// that want to tell this apart from every other probe failure (a transport
// error, a non-2xx status, an unparseable body, a missing context field) can
// match on it with errors.Is.
var ErrOllamaModelRequired = errors.New("probe context: ollama requires a model name")

// fetchProbeBody issues the GET that /props-, /v1/models-, and /info-shaped
// probes need. It is a thin wrapper so that every long-standing caller keeps
// its exact shape; fetchProbeBodyWith carries the method and body a
// POST-only upstream endpoint needs (Ollama's /api/show -- see issue #54).
func fetchProbeBody(ctx context.Context, client *http.Client, baseURL, path string) ([]byte, int, error) {
	return fetchProbeBodyWith(ctx, client, baseURL, http.MethodGet, path, nil)
}

// fetchProbeBodyWith is the common request-and-read-body step shared by
// ProbeContext and ProbePropsVerdicts: build baseURL+path, issue method
// against it (writing body as the request body and setting
// Content-Type: application/json when body is non-empty) through client
// (falling back to http.DefaultClient for a nil one, matching ProbeContext's
// long-standing contract), and return the raw response body on a 2xx
// status. It never interprets the bytes -- each caller applies its own
// parse/evidence rule to the same body.
//
// The returned status is the HTTP status code actually received, or 0 if no
// response was ever received at all (a transport-level failure: connection
// refused, timeout, DNS failure, ...). ProbeContext ignores it -- its error
// handling and caching policy are unchanged by this. ProbePropsVerdicts
// uses it to tell a transient failure (status 0, or a non-2xx status that
// says nothing final -- a 5xx above all) from a CONCLUSIVE refusal (404,
// 401, 403, 405) when deciding whether an undetermined verdict set is safe
// to cache; see its own comment for why exactly those four are conclusive.
func fetchProbeBodyWith(ctx context.Context, client *http.Client, baseURL, method, path string, body []byte) ([]byte, int, error) {
	if client == nil {
		client = http.DefaultClient
	}

	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("probe: upstream status %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return respBody, resp.StatusCode, err
}

// LiveProgressProbePath is the fixed path GETted for the live-progress-
// capability verdict (issue #51, task 4) and, since #49-2, every other
// /props-derived capability verdict too -- ProbePropsVerdicts fetches this
// path exactly once and derives all of them from that same document. Unlike
// contextPath above, this path is NOT type-derived and NOT overridable:
// routing.DeriveProbePaths gives a "custom"-typed spec no context path at
// all, so a custom-typed child (DetectRuntimeSpecType's fallback, or an
// explicit operator choice) would otherwise never be probed here -- exactly
// the case this detector exists to recover. The agent's probeRuntimeChildProps
// (agent.go) GETs this path unconditionally for every StateRunning child with
// a live port, regardless of st.Type or st.ContextProbePath.
const LiveProgressProbePath = "/props"

// PropsVerdicts is everything one /props document tells us. Widened from a
// single live-progress verdict (#51) so that a llama_cpp child is fetched
// ONCE per cache miss rather than once per verdict kind (#49-2): the context
// probe already GETs /props separately, and a third GET for capabilities
// would have made three requests per cycle for the same document.
type PropsVerdicts struct {
	// LiveProgress is the unchanged #51 verdict: "supported" | "unsupported"
	// | "" (unknown).
	LiveProgress string
	// Caps is the capability verdict set from the same document (#49-2).
	Caps Capabilities
}

// ProbePropsVerdicts GETs baseURL+LiveProgressProbePath and returns every
// verdict that one /props document yields, together in a PropsVerdicts: the
// live-progress-capability verdict -- "supported", "unsupported", or ""
// (unknown -- the body is not a llama.cpp /props document at all) -- and the
// capability verdict set from the identical bytes. It reuses fetchProbeBody,
// the exact GET-and-read-body step ProbeContext uses, then hands the raw
// bytes to detectLiveProgressSupport and detectCapabilities for their own
// evidence rules -- ONE fetch, both verdict kinds, which is the entire point
// of this function (see PropsVerdicts' doc comment): a llama_cpp child used
// to be fetched once per verdict kind, and is now fetched once per cache
// miss regardless of how many kinds of verdict the document yields.
//
// This is a SIBLING of ProbeContext, not a case folded into it:
// ProbeContext is hard-typed to (int, error) and every extractor beneath it
// (extractContext and friends) returns (int, bool) -- a tri-state verdict
// cannot ride that chain, and widening it would touch every extractor for a
// capability that has nothing to do with context-size extraction. Keeping
// this a separate function is the deliberate shape choice.
//
// The second return, stable, tells the caller whether this outcome
// (including a zero-value PropsVerdicts{}) is safe to cache and stop asking
// about, or must be retried next cycle. This is a resource-usage fix: without
// it, a non-llama.cpp child would have its /props endpoint hit every single
// collect cycle for its entire lifetime, because "" was never cached at all.
// The split is about WHY no verdict could be determined, not about the
// verdict's value:
//
//   - stable == true: the endpoint answered CONCLUSIVELY. Either a real
//     /props document (verdict "supported"/"unsupported", exactly as
//     before), or a status that settles the question for this pid -- 404
//     (no such route on this build), 401/403 (the route is behind an api
//     key this probe cannot supply), 405 (not for GET) -- or any other
//     syntactically well-formed body that simply isn't that document (a
//     vLLM/Ollama/TGI body, say). None of that can change while this
//     process keeps running: the binary behind it, and the credential it
//     was launched with, do not change.
//   - stable == false: no conclusive answer was possible -- the fetch never
//     got an HTTP response at all (connection refused, timeout, ...), the
//     response was some OTHER non-2xx status (a 5xx above all), or the body
//     was syntactically invalid/truncated JSON. Any of these can describe a
//     child that is merely still warming up, so the caller must NOT cache
//     "" here.
//
// A caller that collapses this into one branch either reintroduces the
// permanent per-cycle /props traffic (by never caching) or permanently
// misses a verdict for a slow-starting child (by caching everything).
//
// Caps rides the exact same fetch and the exact same stable/transient split
// as LiveProgress: it is derived from the identical document, so there is no
// separate cache-ability question to answer for it (#49-2) -- a caller never
// needs to ask "was Caps determined?" independently of "was LiveProgress
// determined?"; the single stable return answers both at once.
func ProbePropsVerdicts(ctx context.Context, client *http.Client, baseURL string) (verdicts PropsVerdicts, stable bool) {
	body, status, err := fetchProbeBody(ctx, client, baseURL, LiveProgressProbePath)
	if err != nil {
		// No conclusive body in hand -- but SOME statuses are still a
		// conclusive answer to "will this endpoint ever hand me a /props
		// document?", and those must be cached or the caller re-GETs
		// /props on every collect cycle (1 s default, 250 ms floor) for
		// the child's entire lifetime.
		//
		// Conclusive, because none of them can change while THIS pid
		// keeps running -- they are properties of the binary's routing
		// table and of the credential it was started with, both fixed at
		// exec time:
		//   - 404: this build has no such route.
		//   - 401/403: the route exists but demands a credential this
		//     probe does not have and cannot obtain. `llama-server
		//     --api-key ${API_TOKEN}` is a first-class supported spec
		//     shape, and llama.cpp marks only /health and /v1/health as
		//     public -- /props is behind the key. The agent's probe
		//     cannot authenticate (runtime.Status carries no token), so
		//     asking again buys nothing; the verdict stays undetermined
		//     and the decision falls back to the shape clause. Lifting
		//     that limitation is issue #58.
		//   - 405: the route exists but not for GET, which is the same
		//     kind of fixed, build-level fact as a 404.
		//
		// NOT conclusive: status 0 (no HTTP response at all -- connection
		// refused, timeout) and every other non-2xx, notably 5xx. Those
		// all describe a child that may merely still be starting up, so
		// they must be retried rather than cached.
		stable := status == http.StatusNotFound ||
			status == http.StatusUnauthorized ||
			status == http.StatusForbidden ||
			status == http.StatusMethodNotAllowed
		return PropsVerdicts{}, stable
	}
	if !json.Valid(body) {
		// Syntactically invalid/truncated JSON reads as a child still
		// mid-response, not a conclusive answer -- do not cache it.
		return PropsVerdicts{}, false
	}
	return PropsVerdicts{
		LiveProgress: detectLiveProgressSupport(body),
		Caps:         detectCapabilities(body),
	}, true
}

// detectLiveProgressSupport is the agent-side half of the live-progress-
// capability detector (issue #51): it decides whether the upstream build
// tolerates the live-progress request parameters (tokens/sec, TTFT) WITHOUT
// ever sending a request that risks a 400 to find out.
//
// The signal is the presence of the key "timings_per_token" inside
// default_generation_settings.params in a llama.cpp /props response. That
// object is a serialization of the COMPILED request-schema field list, so
// the key's presence asserts "this build's completion schema has that
// field" -- not merely "this looks like llama.cpp". Presence is ALL that is
// checked: the value is always false (the handler default-constructs the
// params struct), so it carries no information and must never be read.
//
// Returns:
//   - "supported"    default_generation_settings.params is present AND
//     contains the key.
//   - "unsupported"  default_generation_settings.params is present but does
//     NOT contain the key -- a real verdict about a real build (an older
//     llama.cpp).
//   - ""             anything else: unparseable bytes, or a body that
//     simply isn't that document -- a vLLM /v1/models body, an Ollama
//     /api/show body, a TGI /info body, .... This is UNKNOWN, not a
//     verdict, and callers must never let it overwrite an already-cached
//     verdict. A llama.cpp ROUTER-mode body ("role": "router", issue #55)
//     lands here too, even though it is /props-shaped: it describes the
//     router's own build, not the one serving this model.
//
// This is a DUPLICATE, on purpose, of detectLiveProgressSupport in
// gateway/backend/internal/provider/model_info.go -- the two are separate Go
// modules (gateway/backend and server-agent) and cannot share code,
// mirroring the "DUPLICATED locally on purpose" precedent at
// gateway/backend/internal/provider/memory_probe.go:111. Whoever changes
// this rule must change that copy identically, or the two halves of this
// feature will drift. A reviewer finding them divergent is a real finding;
// finding them duplicated is expected.
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
// when it is "" -- see routing.CapabilityVerdicts.
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
// gateway/backend/internal/provider/model_info.go -- the two are separate Go
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

// reservedOllamaCapabilityNames are the names detectOllamaCapabilities
// refuses to carry into Extra no matter what /api/show declares, because the
// consumer REASONS about them and this document cannot be evidence for
// either one.
//
// DEFENCE IN DEPTH ONLY. The load-bearing rule is the gateway's, at the
// ingest boundary where the agent's bytes arrive
// (reservedAgentCapabilityNames in internal/gateway/agent_ingest.go, whose
// doc carries the full argument): a filter here protects only against a
// publisher string reaching an honest agent's Extra list, while the threat
// that matters is a buggy or hostile agent putting the name straight into
// the verdicts it sends, on a path this function is not on at all. Keeping
// both means an honest agent never puts a name on the wire that the gateway
// would only have to drop, and the drop stays visible on whichever side it
// happens.
//
// Why exactly these two, when "vision"/"tools"/"audio" are claimed by the
// switch above as real verdicts: Ollama's capability array says nothing
// about either. "mtp" is not detected anywhere today -- the row comes from
// the portal's operator checkbox or its model-name heuristic, and it feeds
// the router's +30 MTP bonus. "live_progress" has a dedicated wire field of
// its own, and for an Ollama child that field is always "" (Ollama exposes
// no timings_per_token-style surface), so a publisher's string would not
// merely collide with the dedicated answer -- it would BE the answer, and
// the router would then send timings_per_token to an upstream that does not
// understand it.
var reservedOllamaCapabilityNames = map[string]bool{
	"mtp":           true,
	"live_progress": true,
}

// maxOllamaExtraCapabilities bounds how many names detectOllamaCapabilities
// carries into Capabilities.Extra out of ONE /api/show document, and
// maxOllamaCapabilityNameBytes bounds how long one such name may be. Both
// numbers are read off a ceiling the system actually has rather than picked
// for looking reasonable, and both CLAMP rather than reject -- the same
// discipline as maxRuntimeSamplesPerSample on the gateway's ingest side.
//
//   - THE FRAME, 1 MiB. Every carried name becomes one telemetry wire entry
//     (`{"name":"...","verdict":"yes"}` -- ~34 bytes of scaffolding plus the
//     name itself) inside the ONE agent->gateway WebSocket frame, whose cap
//     is gwapi.MaxWSFrameBytes / the gateway's maxAgentFrameBytes. A frame
//     one byte over that is not a dropped message: the read fails and the
//     socket closes 1009, taking down the single connection telemetry, the
//     system and runtime reports, the runtime_config push and the
//     certificate doorbell all share -- and nothing on the write path sizes
//     a telemetry frame against the cap. Measured before this bound
//     existed: a 965,058-byte /api/show body (valid JSON, comfortably under
//     fetchProbeBodyWith's own 1 MiB read cap) declared 107,615 short names
//     and produced a 3,655,456-byte capabilities object -- 3.5x the frame
//     cap, rebuilt every cycle, because a stable verdict set is CACHED for
//     the whole pid generation.
//
//     What this bound buys, stated exactly, because the residual is a
//     PRODUCT and it would be easy to overstate the closure: the PER-CHILD
//     cost is bounded -- 64 names of at most 128 bytes is at most ~10 KiB
//     (64 * (34 + 128) = 10,368 bytes) -- and it no longer follows what a
//     third-party model manifest declares. The FLEET-WIDE total is NOT
//     bounded here, and nothing else bounds it either: it is children x
//     per-child cost, so what it now scales with is the number of children
//     an OPERATOR configured. Measured on real frames: 64 worst-case
//     children -> 668,033 bytes, 100 -> 1,043,621, and 101 worst-case
//     ollama children overflow the 1 MiB frame -- against ~75 KiB for 256
//     children carrying no capabilities object at all. The gateway's own
//     maxRuntimeSamplesPerSample = 256 shows the system does contemplate
//     that many runtime entries.
//
//     That residual needs far harder input than the defect this bound
//     closes (101+ concurrently running ollama children, EACH with a padded
//     manifest, versus one model pull), and closing it properly is not a
//     name bound at all: it needs the agent's write path to size a
//     telemetry frame against the cap, which neither client/ws.go nor
//     WSSender.Post does today. That is pre-existing and belongs to its own
//     change. The precedent this pair is read off does the product in its
//     own comment (runtime_logs.go's ~8 KiB at the ceiling) because ITS
//     count -- watched specs per server -- is bounded too; this one's is
//     not, which is the one way the two are not alike.
//
//   - THE INDEX, 2704 bytes. A name is half of
//     model_mapping_capabilities' primary key (mapping_id, capability), and
//     a PostgreSQL btree index tuple may not exceed that. Since
//     UpsertMappingCapabilities is atomic, ONE over-long name fails the
//     whole statement and drops EVERY capability row for that mapping --
//     silently, one function above a Debug-level log. 128 bytes sits a
//     factor of 21 below the ceiling, with room for the mapping_id sharing
//     the tuple.
//
// Both are orders of magnitude above the real vocabulary: Ollama's own
// detection declares at most eight names, the longest of them "completion"
// at ten bytes. They are also the pair this system already uses against
// this same 1 MiB frame ceiling -- the runtime-log subscribe path bounds a
// spec id at 128 bytes and one server's watched specs at 64 for the
// identical reason. That path REJECTS where this one clamps, and the
// difference is what the caller can express: a rejected subscription would
// hand back a window guaranteed to stay empty, while a dropped capability
// leaves a row absent, which this model already reads as "unknown".
//
// The input side is bounded too, by maxOllamaShowConclusiveBodyBytes on
// ProbeOllamaVerdicts.
const (
	maxOllamaExtraCapabilities   = 64
	maxOllamaCapabilityNameBytes = 128
)

// detectOllamaCapabilities is the Ollama sibling of detectCapabilities
// (#54 project, task 1): it reads the "capabilities" array of a
// POST /api/show response body and maps each declared name to a
// Capabilities verdict.
//
// The evidence rule is asymmetric with detectCapabilities' modalities/
// chat_template_caps rule, and deliberately so: this function can NEVER
// produce "no". Verified against Ollama's own Go source (server/model.go /
// server/routes.go, ollama/ollama): the capabilities array it returns is NOT
// exhaustive. Upstream logs "unknown capabilities for model" when detection
// yields an empty result rather than treating that as a real answer; the
// JSON field is `omitempty`, so it can be entirely absent for reasons that
// have nothing to do with what the model can do; a failed model-file read
// silently shortens the list before it is ever serialized; the detection
// itself is substring heuristics run over the chat template, not a
// declared/compiled fact; and at least one filter deliberately strips a real
// capability back out for certain builds. A name's ABSENCE from this array
// therefore means "Ollama did not tell us", never "this model cannot do
// that" -- so every field this function writes is "" (unknown) or "yes",
// and a caller must never read a zero Capabilities field coming out of this
// function as "no".
//
//   - "completion" is dropped on purpose, not filed under Extra as unknown
//     noise: upstream ASSUMES this capability whenever a model has no
//     pooling_type, rather than detecting it, so its presence or absence in
//     the array is not evidence of anything and must not be surfaced at
//     all.
//   - "image" is NOT vision. In Ollama's own model this name is the
//     image-GENERATION capability -- it was introduced as
//     CapabilityImageGeneration, its error string reads "image generation",
//     and the only code path that ever required it guards the
//     /v1/images/generations endpoint. It is filed under Extra like any
//     other unmapped name and must never be written to the Vision field.
//   - unrecognized names ("thinking", a future/publisher-specific string,
//     ...) are carried into Extra so they are visible without this detector
//     needing to know about them in advance. Carried, not copied
//     byte-for-byte: every name is trimmed and lower-cased first, so that
//     " Vision " matches the structured field and " Weather.V2 " reaches
//     Extra as "weather.v2". Ollama's own capability tokens are already
//     lower-case, so this only ever normalises a publisher's string.
//   - duplicates collapse to their first occurrence, in both the structured
//     fields (idempotent by construction) and Extra (explicit dedup).
//   - a RESERVED name (reservedOllamaCapabilityNames -- "mtp",
//     "live_progress") is skipped: the consumer reasons about both and this
//     document is evidence for neither. Defence in depth for the gateway's
//     own rule, which is the load-bearing one.
//   - the carried names are BOUNDED, in count and in length
//     (maxOllamaExtraCapabilities / maxOllamaCapabilityNameBytes, whose own
//     doc records the two ceilings those numbers are read off). Past the
//     count later names are dropped but the loop keeps SCANNING, so a tail
//     of publisher strings can never displace a structured verdict; past
//     the length a name is DROPPED rather than truncated, because a
//     truncated name is a different capability. Either drop is reported
//     once, at Warn.
//
// This function is AGENT-ONLY today: server-agent is the only module that
// probes Ollama's /api/show, so unlike detectCapabilities and
// detectLiveProgressSupport above -- which exist as byte-identical copies in
// both server-agent and gateway/backend because both modules probe
// llama.cpp's /props -- this one has no gateway-side twin yet. It becomes
// one, and must then be kept byte-identical to it the same way, the day the
// gateway gains its own Ollama probe.
func detectOllamaCapabilities(body []byte) Capabilities {
	var doc struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Capabilities{}
	}
	var caps Capabilities
	var droppedOverLength, droppedOverLimit, droppedReserved int
	// The dedup hint is the BOUND, not len(doc.Capabilities): the array's
	// length is upstream-controlled, and sizing a map from it would let a
	// declaration this function is about to clamp anyway drive the one
	// allocation made before the clamp can. The map still grows to however
	// many distinct names actually arrive -- this only refuses to
	// pre-reserve for them.
	seen := make(map[string]bool, maxOllamaExtraCapabilities)
	for _, raw := range doc.Capabilities {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || name == "completion" || seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case "vision":
			caps.Vision = "yes"
		case "tools":
			caps.Tools = "yes"
		case "audio":
			caps.Audio = "yes"
		default:
			// Three reasons a name goes no further, and one thing they
			// have in common: none of them stops the LOOP. A break at the
			// count clamp would let a hostile tail of publisher strings
			// displace the structured verdicts above -- 64 junk names
			// followed by "vision" would lose the one verdict a consumer
			// actually reasons about.
			switch {
			case reservedOllamaCapabilityNames[name]:
				droppedReserved++
			case len(name) > maxOllamaCapabilityNameBytes:
				// DROPPED, deliberately not truncated: a truncated name
				// is a DIFFERENT capability, and it would be written as a
				// confident "yes" under a name nothing upstream ever
				// declared.
				droppedOverLength++
			case len(caps.Extra) >= maxOllamaExtraCapabilities:
				droppedOverLimit++
			default:
				caps.Extra = append(caps.Extra, name)
			}
		}
	}
	if dropped := droppedOverLength + droppedOverLimit + droppedReserved; dropped > 0 {
		// A clamp is a DEGRADE, so it is observable: one Warn at the site
		// that drops, naming how many names went and why. Warn rather than
		// Debug because the agent's own default level is info
		// (main.newLogger takes Debug only under --verbose), so a Debug
		// line is invisible in every default deployment -- the identical
		// argument the gateway's ingest makes for its own drop warnings.
		// Without it the clamp is silent by construction: the rows simply
		// never appear, and a missing row is what this model already means
		// by "unknown".
		//
		// It repeats once per pid generation, not once per cycle: a stable
		// verdict set is cached by the caller (probeRuntimeChildProps).
		slog.Warn("ollama capability names dropped from the /api/show array",
			"declared", len(doc.Capabilities),
			"dropped", dropped,
			"dropped_over_length", droppedOverLength,
			"dropped_over_limit", droppedOverLimit,
			"dropped_reserved", droppedReserved,
			"kept", len(caps.Extra),
			"max_names", maxOllamaExtraCapabilities,
			"max_name_bytes", maxOllamaCapabilityNameBytes)
	}
	return caps
}

// ollamaShowPath is the fixed, POST-only path ProbeOllamaVerdicts issues its
// request against. It is not a caller-supplied contextPath like the one
// ProbeContext's "ollama" dispatch takes: this probe always targets exactly
// this path, the same way LiveProgressProbePath is fixed for
// ProbePropsVerdicts.
const ollamaShowPath = "/api/show"

// maxOllamaShowConclusiveBodyBytes bounds the INPUT side of Ollama's
// capability detection, which detectOllamaCapabilities' own two clamps
// cannot: a verdict set that comes back stable is cached for the whole pid
// generation, so an implausible document must not get to freeze whichever
// names the clamp happened to keep. Past this size the body is reported as
// no verdicts at all, which leaves every capability UNKNOWN -- the state
// this model expresses natively as the absence of a row.
//
// 256 KiB is a quarter of the 1 MiB ceiling that matters twice over
// (fetchProbeBodyWith's read cap and the agent->gateway frame cap), and
// roughly fifty times the largest real answer: this probe sends
// {"model": "..."} with no "verbose" flag, so Ollama returns the compact
// form of /api/show -- a few KB.
//
// The verdict is CONCLUSIVE (stable == true), which is the existing rule
// applied rather than a new one: "any other well-formed body that simply is
// not that document" has always been conclusive here, and a complete body
// this far past any real /api/show is exactly that. The alternative --
// treating it as transient -- would re-read and re-parse a quarter-megabyte
// document once per collect cycle for the child's whole life and, since the
// caller only logs a retry at Debug, do it invisibly; conclusive costs one
// read and one Warn per pid generation. A body that is not complete keeps
// its own answer: the json.Valid check below runs first and still says
// "ask again".
const maxOllamaShowConclusiveBodyBytes = 1 << 18

// ProbeOllamaVerdicts is the Ollama sibling of ProbePropsVerdicts (issue
// #54): it POSTs baseURL+ollamaShowPath ("/api/show") with body
// {"model": model} and derives the capability verdict set
// detectOllamaCapabilities (task 1) reads from that one document, reusing
// fetchProbeBodyWith (task 2) for the request/response plumbing.
//
// LiveProgress is ALWAYS "" here, never a real verdict of either kind:
// Ollama exposes no timings_per_token-style surface at all, so there is no
// evidence to read one way or the other. "" is the value that means exactly
// that -- downstream, "" means "write no row", so this unknown can never
// become a permanent false denial the way a fabricated "no" would.
//
// stable follows the IDENTICAL conclusive/transient split ProbePropsVerdicts
// documents on its own comment, reused verbatim rather than restated here:
// 404/401/403/405 are conclusive (fixed properties of the binary's routing
// table and the credential it was started with, both fixed at exec time);
// status 0 (no HTTP response at all) and everything else -- 5xx above all --
// are transient and must be retried, never cached.
//
// An empty model issues NO request at all: Ollama's /api/show requires
// {"model": "<name>"} and answers 400 "model is required" without one, so
// sending it would only spend a round trip to learn nothing conclusive. The
// caller gets stable == false (ask again once a model name is known).
func ProbeOllamaVerdicts(ctx context.Context, client *http.Client, baseURL, model string) (PropsVerdicts, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return PropsVerdicts{}, false
	}

	reqBody, err := json.Marshal(struct {
		Model string `json:"model"`
	}{Model: model})
	if err != nil {
		return PropsVerdicts{}, false
	}

	body, status, err := fetchProbeBodyWith(ctx, client, baseURL, http.MethodPost, ollamaShowPath, reqBody)
	if err != nil {
		// See ProbePropsVerdicts' comment for the full reasoning -- it is
		// identical here: these four statuses are fixed, build-level facts
		// that cannot change while this pid keeps running, so they are safe
		// to cache; status 0 and everything else (5xx above all) are not.
		stable := status == http.StatusNotFound ||
			status == http.StatusUnauthorized ||
			status == http.StatusForbidden ||
			status == http.StatusMethodNotAllowed
		return PropsVerdicts{}, stable
	}
	if !json.Valid(body) {
		// Syntactically invalid/truncated JSON reads as a child still
		// mid-response, not a conclusive answer -- do not cache it. This
		// check stays FIRST so a body truncated by the 1 MiB read cap
		// keeps its long-standing "ask again" answer rather than being
		// re-classified by the size rule below.
		return PropsVerdicts{}, false
	}
	if len(body) > maxOllamaShowConclusiveBodyBytes {
		slog.Warn("ollama /api/show body implausibly large, reporting no capability verdicts",
			"bytes", len(body), "max_bytes", maxOllamaShowConclusiveBodyBytes)
		return PropsVerdicts{}, true
	}
	return PropsVerdicts{
		LiveProgress: "",
		Caps:         detectOllamaCapabilities(body),
	}, true
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

// extractContext dispatches to the per-specType extraction rule.
func extractContext(specType string, v any) (int, bool) {
	switch specType {
	case "vllm":
		return extractVLLMContext(v)
	case "llama_cpp":
		return extractLlamaCppContext(v)
	case "tgi":
		return extractTGIContext(v)
	case "ollama":
		return extractOllamaContext(v)
	default: // "custom", "", and any unrecognized type.
		return extractBestEffortContext(v)
	}
}

// extractVLLMContext reads data[].max_model_len from a vLLM /v1/models body,
// taking the first entry that carries the field.
func extractVLLMContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	arr, ok := obj["data"].([]any)
	if !ok {
		return 0, false
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := asInt(m["max_model_len"]); ok {
			return n, true
		}
	}
	return 0, false
}

// extractLlamaCppContext reads default_generation_settings.n_ctx from a
// llama.cpp /props body (falling back to a top-level n_ctx, tolerating a
// future/alternate shape).
func extractLlamaCppContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	if dgs, ok := obj["default_generation_settings"].(map[string]any); ok {
		if n, ok := asInt(dgs["n_ctx"]); ok {
			return n, true
		}
	}
	if n, ok := asInt(obj["n_ctx"]); ok {
		return n, true
	}
	return 0, false
}

// extractTGIContext reads max_total_tokens from a TGI /info body.
func extractTGIContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	return asInt(obj["max_total_tokens"])
}

// extractOllamaContext reads the architecture-prefixed "<arch>.context_length"
// key out of model_info in an Ollama /api/show body. Keys are visited in
// sorted order so the result is deterministic if a body ever carried more
// than one (it should not).
func extractOllamaContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	mi, ok := obj["model_info"].(map[string]any)
	if !ok {
		return 0, false
	}
	keys := make([]string, 0, len(mi))
	for k := range mi {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := asInt(mi[k]); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// bestEffortContextKeys is the fallback key list scanned for a "custom" or
// unrecognized spec type, in priority order.
var bestEffortContextKeys = []string{"n_ctx", "max_model_len", "context_length"}

// extractBestEffortContext scans the decoded JSON body at any depth for the
// first key matching one of bestEffortContextKeys (exactly, or by a
// ".context_length" suffix, to also catch an ollama-style architecture-
// prefixed key such as "llama.context_length").
func extractBestEffortContext(v any) (int, bool) {
	for _, key := range bestEffortContextKeys {
		if n, ok := findKeyOrSuffix(v, key); ok {
			return n, true
		}
	}
	return 0, false
}

// findKeyOrSuffix searches v depth-first for the first key equal to key, or
// (when key is "context_length") ending in ".context_length". Dispatches on
// v's shape; the actual per-shape scan lives in findInMap/findInSlice.
func findKeyOrSuffix(v any, key string) (int, bool) {
	switch t := v.(type) {
	case map[string]any:
		return findInMap(t, key)
	case []any:
		return findInSlice(t, key)
	default:
		return 0, false
	}
}

// findInMap looks for key as an exact top-level key of t first, then (only for
// key == "context_length") as an architecture-prefixed suffix match among t's
// other keys, then descends depth-first into every value of t. Keys are
// visited in sorted order for a deterministic result.
func findInMap(t map[string]any, key string) (int, bool) {
	if raw, ok := t[key]; ok {
		if n, ok := asInt(raw); ok {
			return n, true
		}
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if isContextLengthSuffixKey(key, k) {
			if n, ok := asInt(t[k]); ok {
				return n, true
			}
		}
		if n, ok := findKeyOrSuffix(t[k], key); ok {
			return n, true
		}
	}
	return 0, false
}

// isContextLengthSuffixKey reports whether k is an architecture-prefixed
// "<arch>.context_length" match for the search key (e.g. "llama.context_length"
// matching key "context_length"). Only ever true when key is exactly
// "context_length" and k isn't that exact key itself.
func isContextLengthSuffixKey(key, k string) bool {
	return key == "context_length" && k != key && strings.HasSuffix(k, "."+key)
}

// findInSlice searches each element of t, in order, for key.
func findInSlice(t []any, key string) (int, bool) {
	for _, item := range t {
		if n, ok := findKeyOrSuffix(item, key); ok {
			return n, true
		}
	}
	return 0, false
}

// asInt converts a decoded JSON numeric value (float64 from encoding/json, or
// json.Number/int defensively) to an int. Any other type (including a
// non-numeric string) yields (0, false).
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}
