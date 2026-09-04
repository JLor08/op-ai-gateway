# Plan material — Area 03: server-agent (GPU order + device placeholders + args-mode + feature flag/version)

Worktree root: `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/gpu-selection`
All paths below are relative to that root unless absolute. All line numbers verified against the
actual files on branch `gpu-selection` at read time.

Scope of this area (server-agent only):
- A1. `hostGPUIDs`: drop `sort.Ints`, preserve `spec.GPUs` array order, keep dedup.
- A2. Add three device placeholders `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}` via a
  shared helper, siblings of `${HOST_GPU_IDS}` in the same `expand` closure.
- A3. Add wire field `Spec.VisibleDevicesMode string json:"visible_devices_mode"`; skip env
  injection when it is `"args"`; keep the conflict trap and no-GPUs trap in both modes.
- A4. Register agent feature `{Name:"gpu_selection", Since:"0.4.0"}` and bump `const Version` to
  `"0.4.0"`; add the registry pin test.
- A5. Update the existing tests that encode the OLD ascending-sort contract (they must move to the
  new order-preserving contract in the same change).

Everything under `internal/routing`, `internal/store`, `internal/portal`, `internal/gateway`, and
`gateway/frontend` is OTHER areas — this bundle only names the wire field the agent consumes from
them.

---

## 1. CURRENT STATE (exact excerpts, file:line)

### 1.1 `server-agent/internal/runtime/policy_local.go`

**`hostGPUIDs` — the function to change (285-304).** Doc comment 260-284 currently justifies the
ascending sort and says the order "never survives a save"; that doc is now false and must be
rewritten.

```go
// policy_local.go:285-304
func hostGPUIDs(spec Spec) string {
	if len(spec.GPUs) == 0 {
		return ""
	}
	indices := make([]int, 0, len(spec.GPUs))
	seen := make(map[int]bool, len(spec.GPUs))
	for _, g := range spec.GPUs {
		if seen[g.Index] {
			continue
		}
		seen[g.Index] = true
		indices = append(indices, g.Index)
	}
	sort.Ints(indices)                        // <-- REMOVE THIS LINE (298)
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = strconv.Itoa(idx)
	}
	return strings.Join(parts, ",")
}
```

Doc-comment lines to rewrite (260-284), specifically the block at 272-278 ("Sorted ascending rather
than kept in the document's order because the order is not the operator's to begin with … a
hand-chosen order never survives a save anyway."). The dedup paragraph (280-284) stays true and
stays.

**`import "sort"` (line 11) STAYS.** `sort` is still used at line 937 (`sort.Strings(envKeys)`),
so removing the import is a compile error — do NOT touch the import block.

**`placeholderPattern` regex (543).** No change; the three new tokens are handled inside the closure
by exact-match, not by a new pattern.

```go
// policy_local.go:543
var placeholderPattern = regexp.MustCompile(`\$\{[^}]*\}`)
```

**`gpuIDs := hostGPUIDs(spec)` computed once (824).** No change; the new device placeholders reuse
the SAME source list.

```go
// policy_local.go:823-824
func expandSpec(spec Spec, port int, vendor GPUVendor, getenv func(string) string) (expandedSpec, error) {
	gpuIDs := hostGPUIDs(spec)
```

**The four placeholder cases in the `expand` closure (826-920).** The `${HOST_GPU_IDS}` case is the
insertion anchor — the three new cases go immediately after it (after 864), before the
`AGENT_ENV:` case (866):

```go
// policy_local.go:840-864 (existing, abbreviated)
if inner == "PORT" { ... }                 // 840-843
if inner == "MODEL" { ... }                // 848-854
if inner == "HOST_GPU_IDS" {               // 856-864  <-- new cases go AFTER this block
	if gpuIDs == "" {
		return "", nil, fmt.Errorf("${HOST_GPU_IDS} cannot be resolved: this spec declares no gpus, and an empty visible-devices value means NO device is visible, not every device")
	}
	b.WriteString(gpuIDs)
	continue
}
if name, ok := strings.CutPrefix(inner, "AGENT_ENV:"); ok && name != "" { ... }  // 866-903
// near-miss + passthrough                 // 905-916
```

Unknown-token passthrough (916 `b.WriteString(match)`) and the PORT/AGENT_ENV near-miss error
(912-915) are unchanged — the three new tokens neither start with `PORT` nor `AGENT_ENV`, so a
near-miss like `${CUDA_DEVICE}` or lowercase `${cuda_devices}` passes through literally, exactly as
`${HOST_GPU_IDS}` variants do (existing test `TestExpandPlaceholdersHostGPUIDs` sub-cases at
policy_local_test.go:1670-1683).

**The env-injection block (1006-1012) — skip when args mode:**

```go
// policy_local.go:1006-1012 (CURRENT)
if spec.SetVisibleDevices {
	if name := VisibleDevicesVar(vendor); name != "" {
		resultEnv = append(resultEnv, name+"="+gpuIDs)
		resultSpans = append(resultSpans, nil)
		fromSpec = append(fromSpec, false)
	}
}
```

**The conflict trap + no-GPUs trap (971-980) — KEEP in both modes, no change:**

```go
// policy_local.go:971-980 (CURRENT — stays exactly as is)
if spec.SetVisibleDevices {
	for _, k := range envKeys {
		if slices.Contains(visibleDevicesOwnedVars, strings.ToUpper(k)) {
			return expandedSpec{}, fmt.Errorf("runtime: spec env %q conflicts with set_visible_devices: ...", k)
		}
	}
	if gpuIDs == "" {
		return expandedSpec{}, fmt.Errorf("runtime: set_visible_devices is on but this spec declares no gpus: ...")
	}
}
```
This block is guarded only on `spec.SetVisibleDevices`, NOT on the mode, so it already holds in both
modes — leave it. (Design §4.3: a hand-set `CUDA_VISIBLE_DEVICES` in args mode would remap the CUDA
namespace and break `--device` numbering, so the refusal must stay.)

**`VisibleDevicesVar` (212-223).** No change; still the vendor→env-var-name map used by env mode.

### 1.2 `server-agent/internal/runtime/types.go`

**`SpecGPU` (28-32)** — the array element. The array order of `[]SpecGPU` IS the contract (no
`position` field on the wire; the gateway sends the rows already in position order). No field added
here:

```go
// types.go:28-32
type SpecGPU struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"` // 0 = unknown demand, never a real zero-cost claim
}
```

**`Spec` (34-63)** — add `VisibleDevicesMode`. Insert between `SetVisibleDevices` (61) and
`AdminState` (62):

```go
// types.go:54-63 (CURRENT)
	// SetVisibleDevices asks the agent to CONSTRAIN this spec's child ...
	SetVisibleDevices bool   `json:"set_visible_devices"`
	AdminState        string `json:"admin_state"` // "" | "force_running" | "force_stopped"
}
```
`GPUs []SpecGPU json:"gpus"` is at line 46; `ParseConfig` already normalizes it to non-nil
(273-275) and preserves slice order (no re-sort anywhere) — so the agent already receives GPUs in
whatever order the gateway sends. The only agent-side ordering bug is `sort.Ints` inside
`hostGPUIDs`; fixing A1 makes the whole path order-preserving.

`VisibleDevicesMode` is a scalar `string`; an absent field unmarshals to `""`, which the agent
treats as env-mode (today's behavior for an older gateway). No `ParseConfig` normalization needed.

### 1.3 `server-agent/internal/agent/features.go` + `agent.go`

**`Features` slice (43-90)** — currently three entries; append `gpu_selection`:

```go
// features.go:43-90 (tail)
	{Name: "runtime_config_ack", Since: "0.3.0"},
}
```
`FeatureNames()` (93-99), `capabilitiesTemplate`/`mustMarshalCapabilities` (157-167) and
`capabilitiesJSON()` (177-181) all derive from `Features` automatically — adding one entry needs no
edit there.

**`const Version` (agent.go:77)** — bump `0.3.0` → `0.4.0`:

```go
// agent.go:77
const Version = "0.3.0"
```
The long comment block above it (agent.go ~40-76) currently justifies the `0.3.0` bump; extend it
with the `gpu_selection` flag as this branch's reason (see Task 4).

### 1.4 Existing tests that encode the OLD ascending contract (WILL FAIL after A1 — must be updated)

Every one of these declares GPUs in a NON-ascending order and asserts the ascending-sorted output.
After A1 the output is the declared order. They move to the new contract in the same task:

- `internal/runtime/policy_local_test.go`
  - `visibleDevicesSpec()` (1331-1342): `GPUs: [{5},{2}]`; doc 1326-1330 says "Descending in the
    declaration proves the ascending sort". Fixture value goes `"2,5"` → `"5,2"`. Doc must change to
    "proves the declared order is preserved".
  - `TestExpandPlaceholdersSetsVisibleDevicesPerVendor` (1449-1450): `want "2,5"` → `"5,2"`; message
    "ascending" → "in operator order".
  - `TestExpandPlaceholdersVisibleDevicesConflictRefused` (1558-1562): `"level_zero:2,5"` →
    `"level_zero:5,2"`, `"2,5"` → `"5,2"`.
  - `TestExpandPlaceholdersHostGPUIDs` (1609-1647): doc "ascending" → "operator order"; case
    "sorted ascending regardless of declared order" `[{5},{2}] want "2,5"` → name "declared order
    preserved" / `"5,2"`; case "three cards" `[{7},{0},{4}] want "0,4,7"` → `"7,0,4"`. The "single"
    (`"3"`) and "duplicates collapse" (`[{1},{1},{2}]`→`"1,2"`) cases are unchanged.
  - `TestExpandPlaceholdersHostGPUIDsAreHostIndices` (1696-1718): `GPUs: [{4},{6}]` → `"4,6"` — this
    one is ascending == declared order, so **no change needed** (note it, don't touch it).
- `internal/runtime/command_test.go`
  - `TestResolvedCommandReportsPlaceholdersExpanded` (41-83): `GPUs: [{3},{2}]` (52); asserts
    `"level_zero:2,3"` (74) and `"2,3"` (77-78); comment "HOST indices, ascending" (72-73). All go
    to `"3,2"` / "operator order".
  - The other `command_test.go` visible-devices specs are ascending or single, **unaffected**:
    line 203 `[{2},{3}]`→`"2,3"`, line 251 `[{5}]`→`"5"`.
- `internal/runtime/manager_test.go` — `TestManagerVisibleDevicesIsPerSpecIsolated` specA `[{0},{1},
  {2}]`, specB `[{5},{6}]` (both ascending) and `TestManagerVisibleDevicesOffLeavesTheChildEnviron‐
  mentAlone` `[{0},{1}]` (option off): **all ascending / unaffected**. No edits.
- `internal/runtime/driver_logs_test.go:147,163` and `logs_frame_test.go:62` set
  `Env: ["CUDA_VISIBLE_DEVICES=2,3"]` as a literal pre-resolved child env (not via `spec.GPUs`):
  **unaffected**.

Exhaustive grep basis: `grep -rn 'CUDA_VISIBLE_DEVICES|level_zero:|"[0-9],[0-9]|visibleDevicesSpec'
internal/runtime/*_test.go` — the four spots above are the complete set that asserts a value derived
from a non-ascending `spec.GPUs`.

---

## 2. PROPOSED TDD TASKS (ordered, real code)

Test package for the runtime tests is `package runtime` (white-box — `policy_local_test.go:4`,
`command_test.go:4`), so unexported `hostGPUIDs`, `deviceList`, `expandSpec`, `VisibleDevicesModeArgs`
are directly reachable. Feature tests are `package agent` (`features_test.go:4`).

Run command for every runtime/agent task:
```
cd server-agent && go test ./internal/runtime/... ./internal/agent/... -count=1
```
Narrow while iterating, e.g.:
```
cd server-agent && go test ./internal/runtime/ -run TestHostGPUIDs -count=1 -v
cd server-agent && go test ./internal/agent/ -run TestGPUSelectionFeatureIsDeclared -count=1 -v
```

### Task A1 — `hostGPUIDs` preserves array order, keeps dedup

**RED — add to `internal/runtime/policy_local_test.go`** (a direct unit pin, plus the existing
tests that will flip are updated in this same task):

```go
// TestHostGPUIDsPreservesOrderAndDedups pins the order contract: the value is
// spec.GPUs in the operator's DECLARED array order, deduplicated
// (first-occurrence wins), NOT sorted. The gateway now persists an explicit
// position column, so the array order is the operator's choice and must reach
// the visibility variable and ${HOST_GPU_IDS} intact.
func TestHostGPUIDsPreservesOrderAndDedups(t *testing.T) {
	cases := []struct {
		name string
		gpus []SpecGPU
		want string
	}{
		{"single", []SpecGPU{{Index: 3}}, "3"},
		{"declared order preserved, not sorted", []SpecGPU{{Index: 5}, {Index: 2}}, "5,2"},
		{"three cards keep operator order", []SpecGPU{{Index: 7}, {Index: 0}, {Index: 4}}, "7,0,4"},
		{"duplicate collapses, first occurrence wins", []SpecGPU{{Index: 3}, {Index: 2}, {Index: 3}}, "3,2"},
		{"no gpus is empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostGPUIDs(Spec{GPUs: tc.gpus}); got != tc.want {
				t.Errorf("hostGPUIDs(%v) = %q, want %q", tc.gpus, got, tc.want)
			}
		})
	}
}
```

Also, in the SAME commit, update the existing OLD-contract assertions listed in §1.4
(`visibleDevicesSpec` value, `TestExpandPlaceholdersSetsVisibleDevicesPerVendor`,
`TestExpandPlaceholdersVisibleDevicesConflictRefused`, `TestExpandPlaceholdersHostGPUIDs`,
`command_test.go`'s `TestResolvedCommandReportsPlaceholdersExpanded`). Concretely, the string edits:
`"2,5"`→`"5,2"`, `"level_zero:2,5"`→`"level_zero:5,2"`, `"0,4,7"`→`"7,0,4"`, `"2,3"`→`"3,2"`,
`"level_zero:2,3"`→`"level_zero:3,2"`, and reword the "ascending" comments.

Expected before impl: `go test ./internal/runtime/ -run 'TestHostGPUIDs|TestExpandPlaceholdersHostGPUIDs|TestExpandPlaceholdersSetsVisibleDevices|TestResolvedCommandReportsPlaceholdersExpanded' -count=1`
FAILS (new test wants `5,2`/`7,0,4`; the still-sorted impl returns `2,5`/`0,4,7`).

**GREEN — implement in `internal/runtime/policy_local.go`.** Introduce a shared ordered/deduped
index helper and refactor `hostGPUIDs` onto it (this same helper is reused by Task A2's `deviceList`):

```go
// gpuIndices returns spec.GPUs' host indices in the operator's DECLARED array
// order, deduplicated (first occurrence wins). It is the single ordered/deduped
// index list every visibility rendering is built from -- hostGPUIDs and
// deviceList differ only in how they format it. Empty when the spec declares no
// GPUs; every caller treats that as a refusal, not a value (trap 1).
func gpuIndices(spec Spec) []int {
	indices := make([]int, 0, len(spec.GPUs))
	seen := make(map[int]bool, len(spec.GPUs))
	for _, g := range spec.GPUs {
		if seen[g.Index] {
			continue
		}
		seen[g.Index] = true
		indices = append(indices, g.Index)
	}
	return indices
}

// hostGPUIDs renders the ordered/deduped host indices as a comma-separated
// decimal list ("5,2"): the value both SetVisibleDevices (env mode) and
// ${HOST_GPU_IDS} emit. Empty string when the spec declares no GPUs.
//
// DECLARED ORDER, not sorted. The gateway persists an explicit position column
// (agent_runtime_spec_gpus.position) and reads the rows back ORDER BY position,
// so spec.GPUs arrives in the operator's chosen order; that order is honored
// here and reaches the child's visibility variable and --device list intact.
//
// HOST indices, always [... keep the existing 266-270 host-vs-child paragraph ...].
//
// Deduplicated [... keep the existing 280-284 dedup paragraph ...].
func hostGPUIDs(spec Spec) string {
	indices := gpuIndices(spec)
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = strconv.Itoa(idx)
	}
	return strings.Join(parts, ",")
}
```
(The explicit `if len(spec.GPUs)==0 { return "" }` early return is dropped — `gpuIndices` returns an
empty slice and `strings.Join(nil, ",") == ""`, so the empty case is preserved. `sort.Ints` is gone;
the `sort` import stays for `sort.Strings` at line 937.)

Expected after: the run above PASSES; full suite still green.

### Task A2 — the three device placeholders `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}`

**Prefix table (exact strings):**

| Placeholder token | prefix passed to `deviceList` | example for `GPUs:[{3},{2}]` |
|---|---|---|
| `${CUDA_DEVICES}`   | `"CUDA"`   | `CUDA3,CUDA2`     |
| `${VULKAN_DEVICES}` | `"Vulkan"` | `Vulkan3,Vulkan2` |
| `${METAL_DEVICES}`  | `"MTL"`    | `MTL3,MTL2`       |

Each emits `<prefix><host_index>` per row, in operator order, deduplicated — from the SAME
`gpuIndices(spec)` list `hostGPUIDs` uses. Empty GPU list ⇒ the same hard error `${HOST_GPU_IDS}`
raises. These expand wherever they appear (args or env), regardless of `SetVisibleDevices`, exactly
like `${HOST_GPU_IDS}` — the gateway validation (other area) enforces that one is PRESENT when
args-mode+checkbox.

**RED — add to `internal/runtime/policy_local_test.go`:**

```go
// TestExpandPlaceholdersDeviceLists pins the three llama.cpp --device
// placeholders: each emits <Backend><host index> per selected GPU, in the
// operator's declared order, deduplicated, comma-joined -- the same ordered
// index list as ${HOST_GPU_IDS}, only backend-prefixed.
func TestExpandPlaceholdersDeviceLists(t *testing.T) {
	getenv := func(string) string { return "" }
	cases := []struct {
		placeholder string
		want        string
	}{
		{"${CUDA_DEVICES}", "CUDA3,CUDA2"},
		{"${VULKAN_DEVICES}", "Vulkan3,Vulkan2"},
		{"${METAL_DEVICES}", "MTL3,MTL2"},
	}
	for _, tc := range cases {
		t.Run(tc.placeholder, func(t *testing.T) {
			spec := Spec{
				Args: []string{"--device", tc.placeholder},
				GPUs: []SpecGPU{{Index: 3}, {Index: 2}}, // non-ascending: proves order, not sort
			}
			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if args[1] != tc.want {
				t.Errorf("%s = %q, want %q (<prefix><host index> in operator order)", tc.placeholder, args[1], tc.want)
			}
		})
		t.Run(tc.placeholder+"/no gpus refused", func(t *testing.T) {
			spec := Spec{Args: []string{"--device", tc.placeholder}}
			if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv); err == nil {
				t.Fatalf("%s on a spec with no gpus must refuse, not substitute an empty device list", tc.placeholder)
			}
		})
	}

	t.Run("dedup keeps first-occurrence order", func(t *testing.T) {
		spec := Spec{Args: []string{"${CUDA_DEVICES}"}, GPUs: []SpecGPU{{Index: 3}, {Index: 2}, {Index: 3}}}
		args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ExpandPlaceholders: %v", err)
		}
		if args[0] != "CUDA3,CUDA2" {
			t.Errorf("args[0] = %q, want CUDA3,CUDA2", args[0])
		}
	})

	// A near-miss passes through literally, same rule as ${HOST_GPU_IDS}: these
	// tokens are exact-match, and neither starts with PORT/AGENT_ENV.
	for _, value := range []string{"${CUDA_DEVICE}", "${cuda_devices}", "${METAL_DEVICES_JSON}"} {
		t.Run("passes through literally: "+value, func(t *testing.T) {
			spec := Spec{Args: []string{value}, GPUs: []SpecGPU{{Index: 1}}}
			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders(%s) should pass through untouched, got: %v", value, err)
			}
			if args[0] != value {
				t.Errorf("args[0] = %q, want the literal %q", args[0], value)
			}
		})
	}
}
```

Expected before impl: `go test ./internal/runtime/ -run TestExpandPlaceholdersDeviceLists -count=1`
FAILS — the tokens are unrecognized, so `${CUDA_DEVICES}` passes through and `args[1]` is
`"${CUDA_DEVICES}"`, not `"CUDA3,CUDA2"`.

**GREEN — `internal/runtime/policy_local.go`.** Add `deviceList` beside `hostGPUIDs`:

```go
// deviceList renders the ordered/deduped host indices (gpuIndices) as
// backend-prefixed llama.cpp --device names -- "CUDA3,CUDA2" for prefix
// "CUDA", "Vulkan3,Vulkan2" for "Vulkan", "MTL3,MTL2" for "MTL". It is the
// value the ${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES} placeholders
// emit; the order and dedup are identical to hostGPUIDs, only the formatting
// differs. Empty string when the spec declares no GPUs.
//
// CAVEAT (documented, not enforced): a backend enumerates its own devices
// independently of the host's GPU index, so Vulkan2/MTL2 are not guaranteed to
// be the same physical card as host index 2; the operator verifies with
// --list-devices. CUDA with CUDA_VISIBLE_DEVICES unset matches the host order.
func deviceList(spec Spec, prefix string) string {
	indices := gpuIndices(spec)
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = prefix + strconv.Itoa(idx)
	}
	return strings.Join(parts, ",")
}
```

Add the three cases inside the `expand` closure, right after the `${HOST_GPU_IDS}` case (after
line 864):

```go
			// The three llama.cpp --device placeholders, exact-match siblings of
			// ${HOST_GPU_IDS}: same ordered/deduped index list, backend-prefixed.
			// Same empty-list refusal, on the same reasoning (an empty --device
			// list selects NO device, not every device).
			if inner == "CUDA_DEVICES" {
				if gpuIDs == "" {
					return "", nil, fmt.Errorf("${CUDA_DEVICES} cannot be resolved: this spec declares no gpus, and an empty device list selects NO device, not every device")
				}
				b.WriteString(deviceList(spec, "CUDA"))
				continue
			}
			if inner == "VULKAN_DEVICES" {
				if gpuIDs == "" {
					return "", nil, fmt.Errorf("${VULKAN_DEVICES} cannot be resolved: this spec declares no gpus, and an empty device list selects NO device, not every device")
				}
				b.WriteString(deviceList(spec, "Vulkan"))
				continue
			}
			if inner == "METAL_DEVICES" {
				if gpuIDs == "" {
					return "", nil, fmt.Errorf("${METAL_DEVICES} cannot be resolved: this spec declares no gpus, and an empty device list selects NO device, not every device")
				}
				b.WriteString(deviceList(spec, "MTL"))
				continue
			}
```
(`gpuIDs` is the already-computed `hostGPUIDs(spec)` from line 824; `gpuIDs == ""` is the exact
"no gpus" signal, so the three cases reuse it rather than re-testing `len(spec.GPUs)`.)

Expected after: the run PASSES.

### Task A3 — `Spec.VisibleDevicesMode` wire field + args-mode env-injection skip

**RED — add to `internal/runtime/policy_local_test.go`:**

```go
// TestExpandPlaceholdersVisibleDevicesArgsModeSkipsEnvInjection pins Part B:
// in "args" mode the agent injects NO visibility env var (the child sees every
// card and --device does the selecting in host numbering); in "env" mode, and
// for an empty/unknown mode (an older gateway never sends the field), it injects
// the vendor variable exactly as before -- now order-preserving.
func TestExpandPlaceholdersVisibleDevicesArgsModeSkipsEnvInjection(t *testing.T) {
	getenv := func(string) string { return "" }
	base := func() Spec {
		return Spec{
			Binary:            "/usr/bin/llama-server",
			Args:              []string{"--device", "${CUDA_DEVICES}"},
			GPUs:              []SpecGPU{{Index: 3}, {Index: 2}},
			SetVisibleDevices: true,
		}
	}

	t.Run("args mode injects no visibility variable", func(t *testing.T) {
		spec := base()
		spec.VisibleDevicesMode = VisibleDevicesModeArgs
		args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ExpandPlaceholders: %v", err)
		}
		if v, present := envValue(env, "CUDA_VISIBLE_DEVICES"); present {
			t.Errorf("env injected CUDA_VISIBLE_DEVICES=%q in args mode; the child must see every card so --device numbering stays the host's", v)
		}
		if args[1] != "CUDA3,CUDA2" {
			t.Errorf("args[1] = %q, want the expanded device list CUDA3,CUDA2", args[1])
		}
	})

	for _, mode := range []string{VisibleDevicesModeEnv, ""} {
		t.Run("mode="+mode+" injects the visibility variable in operator order", func(t *testing.T) {
			spec := base()
			spec.VisibleDevicesMode = mode
			_, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if v, ok := envValue(env, "CUDA_VISIBLE_DEVICES"); !ok || v != "3,2" {
				t.Errorf("CUDA_VISIBLE_DEVICES = %q (present=%v), want 3,2 (env mode, declared order)", v, ok)
			}
		})
	}
}

// TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes pins that the
// trap-3 refusal (a hand-set visibility var alongside the checkbox) is NOT
// weakened by args mode: a hand-set CUDA_VISIBLE_DEVICES in args mode would
// remap the CUDA namespace and break the child's --device numbering, so it is
// refused there too.
func TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, mode := range []string{VisibleDevicesModeEnv, VisibleDevicesModeArgs} {
		t.Run("mode="+mode, func(t *testing.T) {
			spec := Spec{
				Args:               []string{"--device", "${CUDA_DEVICES}"},
				Env:                map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"},
				GPUs:               []SpecGPU{{Index: 3}, {Index: 2}},
				SetVisibleDevices:  true,
				VisibleDevicesMode: mode,
			}
			if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv); err == nil {
				t.Fatalf("a hand-set CUDA_VISIBLE_DEVICES must be refused in %q mode too", mode)
			}
		})
	}
}
```

Expected before impl: FAILS to COMPILE — `VisibleDevicesModeArgs`, `VisibleDevicesModeEnv`, and
`spec.VisibleDevicesMode` don't exist yet. (A compile failure is the RED for a new field.)

**GREEN — `internal/runtime/types.go`.** Add the field to `Spec` (between 61 and 62) and the two
wire-value constants:

```go
	SetVisibleDevices bool `json:"set_visible_devices"`
	// VisibleDevicesMode chooses HOW SetVisibleDevices enforces, and is only
	// meaningful when it is on. "env" (the default) injects the vendor
	// visibility variable, exactly as before. "args" injects NOTHING: the
	// operator selects devices through a ${CUDA_DEVICES}/${VULKAN_DEVICES}/
	// ${METAL_DEVICES} placeholder in Args, and the child sees every card so
	// the placeholder's host-index numbering is what --device consumes. An
	// empty or unknown value is env-mode -- the behavior an older gateway that
	// never sends this field produces.
	VisibleDevicesMode string `json:"visible_devices_mode"`
	AdminState         string `json:"admin_state"` // "" | "force_running" | "force_stopped"
}

// VisibleDevicesMode wire values. The agent distinguishes only "args" from
// everything else; these constants keep the comparison off a magic string.
const (
	VisibleDevicesModeEnv  = "env"
	VisibleDevicesModeArgs = "args"
)
```

**GREEN — `internal/runtime/policy_local.go`, env-injection block (1006-1012):** add the mode guard:

```go
	if spec.SetVisibleDevices && spec.VisibleDevicesMode != VisibleDevicesModeArgs {
		if name := VisibleDevicesVar(vendor); name != "" {
			resultEnv = append(resultEnv, name+"="+gpuIDs)
			resultSpans = append(resultSpans, nil)
			fromSpec = append(fromSpec, false)
		}
	}
```
The conflict/no-GPUs trap at 971-980 stays as-is (guarded only on `SetVisibleDevices`), so it already
holds in both modes — `TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes` passes without
touching it.

Expected after: both new tests PASS; `TestExpandPlaceholdersVisibleDevicesOffIsUnchanged` and the
per-vendor test still PASS (default/empty mode == env behavior).

### Task A4 — feature flag `gpu_selection` + `Version` 0.4.0

**RED — add to `internal/agent/features_test.go`** (mirrors `TestRuntimeConfigAckFeatureIsDeclared`
at 161-173):

```go
// TestGPUSelectionFeatureIsDeclared pins the GPU-selection feature's exact wire
// NAME and the version it ships in. The gateway checks this literal string
// before it tells the portal the connected agent can honor a custom GPU order
// and expand the ${..._DEVICES} placeholders; a rename here would break that
// negotiation silently. Same two-sides-of-one-contract reasoning as
// TestRuntimeConfigAckFeatureIsDeclared.
func TestGPUSelectionFeatureIsDeclared(t *testing.T) {
	const name = "gpu_selection"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.4.0" {
			t.Fatalf("feature %q Since = %q, want 0.4.0 (the branch's single bump)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway cannot know the agent honors GPU order / device placeholders: %+v", name, Features)
}
```

Expected before impl: FAILS twice — `TestGPUSelectionFeatureIsDeclared` fails (feature absent), and
once the feature is added at `Since:"0.4.0"` with `Version` still `"0.3.0"`, `TestFeatureRegistry`
(features_test.go:75-90) fails its `semverLTE(f.Since, Version)` guard. Both are the intended RED.

**GREEN — `internal/agent/features.go`,** append to `Features` (after line 89):

```go
	{Name: "runtime_config_ack", Since: "0.3.0"},
	// gpu_selection: this agent honors the operator's explicit GPU array order
	// (no longer sorting spec.GPUs ascending) in the visibility variable and
	// ${HOST_GPU_IDS}, expands the ${CUDA_DEVICES}/${VULKAN_DEVICES}/
	// ${METAL_DEVICES} device placeholders, and honors
	// visible_devices_mode="args" (inject no visibility env var; the operator
	// selects devices via a --device placeholder). ONE flag for both behaviors:
	// they ship together. Declared for the PORTAL's benefit -- the agent gates
	// none of its own behavior on it, it always honors what it receives -- so
	// the portal can warn that an older agent will ignore a custom order and
	// will fail to launch an args-mode spec (the placeholder passes through
	// literally). Since 0.4.0: agent.Features gains an entry, so the rule is a
	// MINOR bump (see agent.go's Version block).
	{Name: "gpu_selection", Since: "0.4.0"},
}
```

**GREEN — `internal/agent/agent.go`,** bump the constant (line 77) and extend its comment block to
name `gpu_selection` as this branch's MINOR reason:

```go
const Version = "0.4.0"
```

Expected after: `go test ./internal/agent/... -count=1` PASSES (`TestFeatureRegistry`,
`TestFeatureNamesMatchesRegistry`, `TestCapabilitiesJSONShape` — which counts `len(Features)`
dynamically, so it needs no edit — and the new pin all green).

### Task A5 — full green + doc-comment truth

Run the whole area:
```
cd server-agent && go test ./internal/runtime/... ./internal/agent/... -count=1
```
Confirm the `hostGPUIDs` doc comment (policy_local.go 260-284) and the `visibleDevicesSpec` fixture
comment (policy_local_test.go 1326-1330) no longer claim ascending/sort. This task carries no new
behavior — it is the "all tests green + comments match code" gate.

---

## 3. INTERFACES

### PRODUCES (other areas consume these agent-side names / wire keys)

- Wire field on the runtime-config `Spec`: `VisibleDevicesMode string` with JSON key
  **`visible_devices_mode`** (`server-agent/internal/runtime/types.go`, on `Spec`). The gateway's
  `AgentRuntimeSpecDTO` must marshal this exact key. Values `"env"` / `"args"`; absent/empty ==
  env-mode.
- Constants `VisibleDevicesModeEnv = "env"`, `VisibleDevicesModeArgs = "args"`
  (`package runtime`) — the string values the gateway must send.
- Agent placeholder tokens the operator writes into `args`, expanded by the agent:
  **`${CUDA_DEVICES}` → `CUDA<idx>,…`**, **`${VULKAN_DEVICES}` → `Vulkan<idx>,…`**,
  **`${METAL_DEVICES}` → `MTL<idx>,…`** — in operator (position) order, deduped. The gateway's
  args-mode validation (other area) must scan `req.Args` for at least one of these three literal
  tokens.
- Agent capability flag name: **`gpu_selection`** (`agent.Features`, `Since:"0.4.0"`), rides on
  telemetry `capabilities.features`. The gateway's `agentFeaturesRegistry.Has(serverID,
  "gpu_selection")` and the portal's `agentFeatures.includes('gpu_selection')` consume this literal.
- Agent version: **`const Version = "0.4.0"`** (`agent.go`), reported as top-level `agent_version`.

### CONSUMES (produced by other areas)

- `Spec.GPUs []SpecGPU` arriving in **position order** — the store/gateway now persists
  `agent_runtime_spec_gpus.position` and reads `ORDER BY position`, so the agent receives GPUs
  already in the operator's order and simply preserves it (array order is the contract; no
  `position` field on the wire `SpecGPU`). The agent's only obligation is to stop re-sorting.

### Canonical-name check vs. the prompt's list (no deviations)

- `agent_runtime_spec_gpus.position` / `routing.RuntimeSpecGPU.Position` — other area; agent consumes
  its effect (ordered `[]SpecGPU`) only. ✓
- `agent_runtime_specs.visible_devices_mode` / DTO key `visible_devices_mode` — agent wire type is
  `Spec.VisibleDevicesMode string` (the prompt specifies plain `string` on the agent side; the small
  enum type lives on `routing.RuntimeSpec` in the gateway area). ✓ No deviation.
- Placeholders `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}` → prefixes `CUDA`/`Vulkan`/
  `MTL`, operator order, deduped. ✓
- Feature `{Name:"gpu_selection", Since:"0.4.0"}`; `const Version -> "0.4.0"`. ✓
- Error sentinels `runtime_spec.visible_devices_mode_invalid` /
  `runtime_spec.visible_devices_args_no_placeholder` (HTTP 400) — GATEWAY area, not the agent. The
  agent raises its own launch-time errors (Go `fmt.Errorf`, no wire code) for an empty device list;
  it does NOT emit those two portal sentinels.

---

## 4. GOTCHAS

- **Test framework:** stdlib `testing`, white-box `package runtime` / `package agent`, table-driven
  with `t.Run`. Run: `cd server-agent && go test ./internal/runtime/... ./internal/agent/...
  -count=1`. Helpers already present and reused: `envValue(env, name) (string, bool)`
  (policy_local_test.go:1350), `visibleDevicesSpec()` (1331), `commandFor(...)` (command_test.go).
- **The order change breaks 4 existing test spots** (all listed in §1.4) because they encode the OLD
  ascending contract with non-ascending GPU declarations. They MUST be rewritten to the
  order-preserving contract in Task A1's commit, or `go test ./internal/runtime/...` stays red even
  after the impl is correct. This is the single most likely thing to trip up the plan executor.
  `TestExpandPlaceholdersHostGPUIDsAreHostIndices` (`[4,6]`) and the `manager_test`/`command_test`
  ascending specs do NOT change — don't "fix" them.
- **`sort` import stays** (policy_local.go:11) — `sort.Strings(envKeys)` at line 937 still uses it.
  Removing only `sort.Ints` is correct; removing the import is a compile error.
- **`gpuIDs == ""` is the reused "no gpus" signal** in the three new placeholder cases — it equals
  `hostGPUIDs(spec) == ""` which is exactly `len(gpuIndices(spec)) == 0`. Do not add a second
  `len(spec.GPUs)` check.
- **Device placeholders expand unconditionally** (like `${HOST_GPU_IDS}`), NOT gated on
  `SetVisibleDevices` or on the mode — the closure owns pure text substitution. The
  "must have a placeholder in args-mode" rule is the GATEWAY's validation, not the agent's.
- **Empty/unknown `visible_devices_mode` == env-mode** is a hard-dependency safety property: an
  older agent (pre-`gpu_selection`) has no `VisibleDevicesMode` field, unmarshals it away, and
  injects the env var (env behavior) while passing a `${..._DEVICES}` token through literally →
  launch error. That is why args mode is gated behind the capability flag with a prominent portal
  hint (design §4.3, §5.2) — an agent-side concern only insofar as the agent must default to env.
- **`METAL` token → `MTL` prefix** (not `Metal`). Easy to get wrong; the prefix table above is
  authoritative. CUDA→`CUDA`, VULKAN→`Vulkan` (mixed case), METAL→`MTL`.
- **Version-bump rule (AGENTS.md §"ServerAgent Version Rule", lines 154-203):** ONE bump per branch.
  `gpu_selection` is a new `agent.Features` entry ⇒ MINOR ⇒ `0.3.0`→`0.4.0`, edited ONLY at
  `const Version` in `agent.go:77`. Do not add a second bump for the other agent changes on this
  branch; do not put the version anywhere else (a nested `capabilities.agent_version` was removed on
  purpose). `TestFeatureRegistry` enforces `Since ≤ Version`, turning a forgotten bump into a red
  test.
- **`capabilitiesJSON` needs no edit** — it and `TestCapabilitiesJSONShape` count `len(Features)`
  dynamically, so the new flag flows through automatically; only add the dedicated pin test.
- **Spec ambiguity resolved:** design §10 asks whether the conflict trap stays active in args mode —
  YES (design §4.3 confirms; kept unconditionally under `SetVisibleDevices`). The device-placeholder
  helper location/sharing (§10) is resolved here: `gpuIndices(spec) []int` shared by both
  `hostGPUIDs` and `deviceList(spec, prefix string) string`, all in `policy_local.go`.
