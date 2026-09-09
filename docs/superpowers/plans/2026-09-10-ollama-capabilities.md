# Ollama Capabilities Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Read Ollama's declared capabilities from `POST /api/show` into the per-model capability table, and fix the context probe that could never succeed because it sent GET to a POST-only endpoint (#54).

**Architecture:** A sibling detector and a sibling probe function in the agent's collector, one type branch at the agent's existing props-probe call site, and the method/body plumbing both of them need. The verdicts ride the existing telemetry path — no new writer, no store change, no schema change. The root-cause fix for #54's whole class is making the probe test helper assert the request it receives.

**Tech Stack:** Go 1.x, two modules (`gateway/backend`, `server-agent`), no new dependencies.

## Global Constraints

- **Only `yes` verdicts.** Ollama's capability list is not exhaustive (see spec §2); a missing name is UNKNOWN and must never become a `no`.
- **`completion` is dropped**, every other name is kept verbatim — including unknown publisher strings. Reason: upstream *assumes* `completion` when a model has no `pooling_type`, so it carries no evidence.
- **`image` is NOT `vision`.** `image` means image generation. Never map it onto the structured vision field.
- **Absence of a row means unknown**; `verdict` is only ever `yes` or `no`; `''` is never stored.
- **The rank rule is untouched:** `manual` 3 > `vision_benchmark` 2 > probe sources 1 > no row 0, write iff `rank(incoming) >= rank(current)`.
- **The emission order stays structured-fields-first**, because `WritableCapabilityRows` keeps the FIRST occurrence of a name and Ollama's names collide with ours by spelling (`vision`, `tools`, `audio`).
- **The nil vs non-nil-but-empty `*Capabilities` wire distinction is load-bearing** and must survive.
- **The two existing detector twins stay byte-identical** across the modules; do not touch `detectCapabilities` or `detectLiveProgressSupport`.
- **The context probe keeps reporting the model maximum.** `/api/ps` is out of scope.
- Every new/changed test must FAIL with its production change reverted. Revert **production files only** — reverting a test makes `go test -run X` report "ok" with zero tests, a fake pass.
- **Per-module gates before every commit:** touched Go module → `~/go/bin/golangci-lint fmt --diff` (must print nothing) + `~/go/bin/golangci-lint run` + `go test ./... -count=1`. Docs → `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- `go test ./internal/gateway/ -race` has a pre-existing failure on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (#53) — use plain `go test` there.
- Branch `ollama-capabilities`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/ollama-capabilities`. Never commit to `main`. Never use bare `git stash`. Never `cp -R` the worktree (its `.git` is a gitlink).
- `docs/superpowers/` is branch-local and removed before the PR.

---

## File Structure

- `server-agent/internal/collector/probe.go` — the detector, the probe sibling, and the method/body plumbing. All new agent-side logic lands here beside its existing neighbours.
- `server-agent/internal/collector/probe_test.go` — the helper fix and the per-type request table test.
- `server-agent/internal/agent/agent.go` — one type branch at the props call site, the context call site's new argument, and the context cache's model field.
- `gateway/backend/internal/routing/store.go` — one new source constant.
- `docs/architecture/…` — ADR-038's second detector, the telemetry section, the data-model source list.

---

### Task 1: The Ollama capability detector

**Files:**
- Modify: `server-agent/internal/collector/probe.go`
- Test: `server-agent/internal/collector/probe_test.go`

**Interfaces:**
- Consumes: the existing `Capabilities` struct (`Vision, Video, Audio, Tools string; Extra []string`).
- Produces: `detectOllamaCapabilities(body []byte) Capabilities`, used by Task 3.

- [ ] **Step 1: Write the failing test**

```go
func TestDetectOllamaCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Capabilities
	}{
		{"absent field is undetermined, not denial", `{"model_info":{}}`, Capabilities{}},
		{"empty array is undetermined", `{"capabilities":[]}`, Capabilities{}},
		{"structured names map to structured fields", `{"capabilities":["vision","tools","audio"]}`,
			Capabilities{Vision: "yes", Tools: "yes", Audio: "yes"}},
		{"completion is dropped, it is an upstream assumption", `{"capabilities":["completion"]}`, Capabilities{}},
		{"image is NOT vision", `{"capabilities":["image"]}`, Capabilities{Extra: []string{"image"}}},
		{"unknown publisher strings are kept verbatim", `{"capabilities":["thinking","weather.v2"]}`,
			Capabilities{Extra: []string{"thinking", "weather.v2"}}},
		{"duplicates collapse, first occurrence wins", `{"capabilities":["insert","insert"]}`,
			Capabilities{Extra: []string{"insert"}}},
		{"a name is never a no", `{"capabilities":["tools"]}`, Capabilities{Tools: "yes"}},
		{"malformed json yields nothing rather than panicking", `not json`, Capabilities{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectOllamaCapabilities([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("detectOllamaCapabilities(%s) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd server-agent && go test ./internal/collector/ -run TestDetectOllamaCapabilities`
Expected: FAIL, `undefined: detectOllamaCapabilities`.

- [ ] **Step 3: Implement it**

Place it directly below `detectCapabilities`, with a doc comment that (a) states the evidence rule and why a missing name is not a denial, (b) says `completion` is dropped because upstream assumes it, (c) says `image` means generation and must never reach the vision field, and (d) records that this function is agent-only today and becomes a byte-identical twin the day the gateway gains its own Ollama probe.

```go
func detectOllamaCapabilities(body []byte) Capabilities {
	var doc struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Capabilities{}
	}
	var caps Capabilities
	seen := make(map[string]bool, len(doc.Capabilities))
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
			caps.Extra = append(caps.Extra, name)
		}
	}
	return caps
}
```

- [ ] **Step 4: Run the test and the module gates**

Run: `cd server-agent && go test ./internal/collector/ -count=1 && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run`
Expected: PASS, and `fmt --diff` prints nothing.

- [ ] **Step 5: Prove the test is not vacuous**

Mutate production only, one at a time, and record which mutation broke which case: drop the `completion` skip; map `image` to `caps.Vision`; set `caps.Tools = "no"` for an unseen name; remove the `seen` dedup. Each must fail its own case.

- [ ] **Step 6: Commit**

```bash
git add server-agent/internal/collector/probe.go server-agent/internal/collector/probe_test.go
git commit -m "feat(agent): read Ollama's declared capabilities into a verdict set"
```

---

### Task 2: The probe test helper must assert the request, and the method/body plumbing

**Files:**
- Modify: `server-agent/internal/collector/probe_test.go` (the helper), `server-agent/internal/collector/probe.go` (the plumbing)

**Interfaces:**
- Produces: `fetchProbeBodyWith(ctx, client, baseURL, method, path string, body []byte) ([]byte, int, error)`, used by Tasks 3 and 4. `fetchProbeBody` stays as a GET wrapper so every existing caller is byte-identical.

This task is the root-cause fix for #54's whole class: `newProbeServer`'s handler is declared `func(w, _ *http.Request)`, so no test has ever asserted a probe's method, path or body.

- [ ] **Step 1: Widen the helper so a test can see the request**

Change `newProbeServer` to pass the `*http.Request` through (or to record method/path/body on a struct the caller can read). Every existing caller keeps compiling; do not change a single existing assertion.

- [ ] **Step 2: Write the table test pinning what each spec type issues TODAY**

```go
func TestProbeContextRequestShapePerSpecType(t *testing.T) {
	for _, tc := range []struct {
		specType   string
		path       string
		wantMethod string
		wantBody   string
	}{
		{"llama_cpp", "/props", http.MethodGet, ""},
		{"vllm", "/v1/models", http.MethodGet, ""},
		{"tgi", "/info", http.MethodGet, ""},
		{"custom", "/whatever", http.MethodGet, ""},
		{"ollama", "/api/show", http.MethodGet, ""}, // Task 4 flips this row to POST
	} {
		// assert the method, path and body the server actually received
	}
}
```

Use the real paths `routing.DeriveProbePaths` yields per type where one exists; the point is the method and the body, not the path's spelling.

- [ ] **Step 3: Run it — it must PASS against today's code**

Run: `cd server-agent && go test ./internal/collector/ -run TestProbeContextRequestShapePerSpecType -count=1`
Expected: PASS. This is the test that will FAIL in Task 4 until the ollama row is flipped — that is the discriminator that proves the fix.

- [ ] **Step 4: Add the method/body plumbing, with no behaviour change**

```go
// fetchProbeBody issues the GET that /props-shaped probes need. It is a
// thin wrapper so that every long-standing caller keeps its exact shape;
// fetchProbeBodyWith carries the method and body a POST-only upstream
// endpoint needs (Ollama's /api/show -- see issue #54).
func fetchProbeBody(ctx context.Context, client *http.Client, baseURL, path string) ([]byte, int, error) {
	return fetchProbeBodyWith(ctx, client, baseURL, http.MethodGet, path, nil)
}

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
	// ... the rest byte-for-byte as it is today, including the 1<<20 LimitReader
}
```

Move `fetchProbeBody`'s existing doc comment onto `fetchProbeBodyWith` and leave a short pointer on the wrapper; the conclusive-status contract it documents is unchanged and both callers depend on it.

- [ ] **Step 5: Run the module gates**

Run: `cd server-agent && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./... -count=1`
Expected: all green, `fmt --diff` silent. No existing test's assertions changed.

- [ ] **Step 6: Prove the new test discriminates**

Mutate `fetchProbeBody`'s wrapper to pass `http.MethodPost`: every row of the table must fail. Restore.

- [ ] **Step 7: Commit**

```bash
git add server-agent/internal/collector/probe.go server-agent/internal/collector/probe_test.go
git commit -m "test(agent): the probe test server asserts the request it receives"
```

---

### Task 3: `ProbeOllamaVerdicts`

**Files:**
- Modify: `server-agent/internal/collector/probe.go`
- Test: `server-agent/internal/collector/probe_test.go`

**Interfaces:**
- Consumes: `detectOllamaCapabilities` (Task 1), `fetchProbeBodyWith` (Task 2).
- Produces: `ProbeOllamaVerdicts(ctx context.Context, client *http.Client, baseURL, model string) (PropsVerdicts, bool)`, used by Task 5.

- [ ] **Step 1: Write the failing tests**

Cases: a 200 with capabilities yields those verdicts and `stable == true`; `LiveProgress` is always `""` (Ollama has no such surface — an unknown must never become a denial); the request is a POST to `/api/show` with body `{"model":"<model>"}` and `Content-Type: application/json`; an empty model returns `stable == false` with no request sent at all (Ollama would answer 400 `model is required`, so sending it buys nothing); 404/401/403/405 return `stable == true` with an empty verdict set (conclusive: fixed at exec time); status 0 and 500 return `stable == false`.

- [ ] **Step 2: Run and watch it fail** — `undefined: ProbeOllamaVerdicts`.

- [ ] **Step 3: Implement it**

Reuse `ProbePropsVerdicts`' conclusive-status set verbatim (404/401/403/405 conclusive; 0 and everything else transient) and say in the doc comment that the reason is identical — those are properties of the binary's routing table and its credential, both fixed at exec time.

- [ ] **Step 4: Run the tests and the module gates.**

- [ ] **Step 5: Prove non-vacuity** — mutate: return `LiveProgress: "no"`; treat 500 as conclusive; send the request with an empty model. Record which mutation broke which case.

- [ ] **Step 6: Commit**

```bash
git commit -m "feat(agent): probe Ollama's /api/show for a capability verdict set"
```

---

### Task 4: Fix #54 — the context probe POSTs for Ollama

**Files:**
- Modify: `server-agent/internal/collector/probe.go`, `server-agent/internal/agent/agent.go`
- Test: `server-agent/internal/collector/probe_test.go`, `server-agent/internal/agent/…`

**Interfaces:**
- `ProbeContext` gains a trailing `model string` parameter: `ProbeContext(ctx, client, baseURL, specType, contextPath, model string) (int, error)`.

- [ ] **Step 1: Flip the ollama row of Task 2's table test to POST with a body**

It must FAIL now. That failure is the bug, reproduced by a test for the first time.

- [ ] **Step 2: Implement**

In `ProbeContext`, when the normalised spec type is `ollama`: build `{"model": model}` and call `fetchProbeBodyWith` with POST; every other type keeps the GET wrapper. If the type is `ollama` and `model` is empty, return a distinct error without issuing a request. `extractOllamaContext` and `DeriveProbePaths`' `/api/show` default stay byte-for-byte — only the verb was ever wrong.

At the call site (`probeRuntimeChildContext`), pass `st.Model`, and add a `model` field to `runtimeCtxEntry` compared on the cache-hit path — a spec whose model changed must not serve a context size measured for the previous one. The existing comment about config-only edits explains why; extend it rather than replacing it.

- [ ] **Step 3: Run the tests** — the flipped row passes, every other row unchanged.

- [ ] **Step 4: Module gates.**

- [ ] **Step 5: Prove non-vacuity** — revert the POST branch: the ollama row fails. Revert the cache's model comparison: the cache test fails.

- [ ] **Step 6: Commit**

```bash
git commit -m "fix(agent): the Ollama context probe POSTs /api/show, as upstream requires

Fixes #54"
```

---

### Task 5: Wire the capability probe in, and name the source

**Files:**
- Modify: `server-agent/internal/agent/agent.go`, `gateway/backend/internal/routing/store.go`
- Test: `server-agent/internal/agent/…`, and whichever gateway test covers the source constants

- [ ] **Step 1: Write the failing test** — an `ollama`-typed `StateRunning` child yields a sample whose `Capabilities` carry Ollama's names, and a `llama_cpp`-typed child is byte-identical to today.

- [ ] **Step 2: Implement the branch**

In `probeRuntimeChildProps`, branch on `st.Type == "ollama"` to choose `ProbeOllamaVerdicts(cctx, client, base, st.Model)` over `ProbePropsVerdicts(cctx, client, base)`. Everything else stays: the `(SpecID, PID)` cache, the transient/conclusive handling, `capabilitiesSample`, the wire. Update that function's doc comment, which currently says `/props` is probed "regardless of `st.Type`" — the reasoning (that `custom` is the fallback and gets no derived path) survives, and the branch is for `ollama` only.

- [ ] **Step 3: Add the source constant**

`CapabilitySourceOllamaAPIShow = "ollama_api_show"` beside the existing sources. **Verify, do not assume:** that `capabilitySourceRank`'s default branch gives it rank 1, that `ValidateCapabilityRow` accepts it, and that no allowlist anywhere rejects an unrecognised source. Then find every place that maps a source to a display string and add it.

Set it where the ingest builds rows from a runtime sample, so an Ollama-sourced row is not mislabelled `llama_cpp_props`. **This is the one step whose blast radius reaches the gateway module — check whether the sample carries enough to tell the two apart, and if it does not, say so in the report rather than guessing.**

- [ ] **Step 4: Run both modules' gates.**

- [ ] **Step 5: Prove non-vacuity** — remove the branch: the ollama sample test fails. Force the source to `llama_cpp_props`: the source test fails.

- [ ] **Step 6: Commit**

```bash
git commit -m "feat: Ollama capability verdicts reach the mapping, sourced ollama_api_show"
```

---

### Task 6: Documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-038 now covers two detectors), `docs/architecture/cross-cutting/telemetry-usage-observability.md`, `docs/architecture/reference/data-model.md` (the source vocabulary), and whichever document describes the agent's probes and spec types.

- [ ] **Step 1: Re-locate every passage by heading and wording, not by line number.**
- [ ] **Step 2: Record, with the reasoning, that Ollama's list is not exhaustive and therefore yields only `yes` rows; that `completion` is dropped as an upstream assumption; that `image` means generation and is never folded into vision; that the new source ranks with the probes; and that #54's class is prevented by the test helper asserting the request.**
- [ ] **Step 3: State what is deliberately NOT covered** — directly configured Ollama applications (and why: a POST-capable gateway probe plus a fan-out decision), `/api/ps` for the context size, and any widening of the agent router's GET-only allowlist.
- [ ] **Step 4: Run the docs gates** — `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- [ ] **Step 5: Commit.**
