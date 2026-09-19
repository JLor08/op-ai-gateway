# Portal Chat Image Generation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A portal chat user picks a model that can generate images, types a
prompt, and gets the generated image back in the thread — persisted with the
conversation, visible on reload, and billed like any other turn.

**Architecture:** The existing chat run executor gains a second target
(`POST /v1/images/generations` instead of `/v1/chat/completions`), chosen from
a `kind` pinned to the thread on its first send. The generated image is stored
inline in the sealed chat transcript as an OpenAI-style `image_url` content
part, exactly as an uploaded vision image already is. `/v1/images/generations`
moves from bearer-only auth to a narrow loopback-or-bearer leg so the run
executor can reach it without opening the endpoint to browsers. Because that
endpoint emits zero incremental events, the composer leads with the one number
that is exact and known before the user commits — the remaining transcript
capacity — and shows an elapsed clock rather than any form of progress.

**Tech Stack:** Go 1.x (two modules: `gateway/backend`, `server-agent`),
React + TypeScript + MUI (`gateway/frontend`), Vitest, Playwright, three store
drivers (memory, sqlite, postgres) behind a dialect seam.

**Source spec:** `docs/superpowers/specs/2026-09-18-portal-chat-images-design.md`
(437 lines). Read it before starting: it records what was decided, what was
rejected and why, and eight pre-existing defects this feature makes reachable.

## Global Constraints

Every task's requirements implicitly include this section.

**Branching (absolute, from `AGENTS.md`).** Never commit to or merge into
`main`. All work happens on `feat/portal-chat-images` in the worktree
`.worktrees/portal-chat-images`. The merge is performed by a human after CI is
green.

**Branch-local docs.** `docs/superpowers/**` must never land in `main`. It is
created and used freely while working; as the **last** step before the PR,
fold everything durable into `docs/architecture/` and delete the folder from
the branch. Verify with `git diff --name-only main...HEAD` that no
`docs/superpowers/` path appears.

**No agent version bump.** The version rule in `AGENTS.md` governs
`const Version` in `server-agent/internal/agent/agent.go` — the standalone
agent. This feature touches the gateway backend and the portal frontend only,
so **do not bump it**, and do not add a second version constant anywhere.

**Verification gates — the exact commands CI runs.** `make lint` and
`make test` do **not** cover all of them; run these directly.

Go, for each of the two modules (`gateway/backend` and `server-agent`):

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

`golangci-lint fmt --diff` is a separate gate from `run` and is **not** part of
`make lint-go`; gofumpt and gocritic findings surface only there. The pinned
version is `v2.13.1` (`make lint-install`).

Frontend, in `gateway/frontend`:

```bash
npm run format:check && npm run lint && npm run build && npm test
```

`npm run format:check` is prettier and is the gate a local `test`+`build`+`lint`
run misses most often. `npm run build` is `tsc && vite build`, so it is also the
type gate.

Docs:

```bash
make lint-docs
```

**Running a single test.** Go:
`cd gateway/backend && go test ./internal/gateway/ -run 'TestName' -v`.
Frontend: `cd gateway/frontend && npx vitest run src/components/Chat.test.tsx -t 'test name'`.

**Postgres.** Store and migration changes must be verified with the postgres
leg, which **skips silently** when the DSN is absent — a green run without it
proves nothing about postgres:

```bash
docker start op-pg-test
export OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable'
```

**e2e.** `make test-e2e` runs a suite **no CI job covers** and is red on `main`
itself — CI runs `npm run e2e:runtime` only (19 other Playwright suites are
local-only, `ci.yml:112-113`). Do not treat its failures as branch-caused
without a baseline on the merge-base, and do not treat green CI as evidence
about it.

**Every new user-facing string needs both locales.** `gateway/frontend/src/i18n.ts`
declares German and English blocks; a key added to one and not the other is a
type error at `npm run build`.

**Licence header.** Every new file starts with the two-line SPDX header used
throughout the repo:

```
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors
```

**Commit discipline.** Commit at the end of each task's step list. This repo's
GitHub squash-merge pre-fills the squash description from the **commit message
body**, not the PR body, so write substantive bodies: what changed, why, and
what was verified.

**Repo-facing text is English.** Commits, PR text, code comments and issue text
are English.

---

## File Structure

New files:

| File | Responsibility |
|---|---|
| `gateway/backend/internal/gateway/auth_internal_or_bearer.go` | The one new auth leg: loopback pair or bearer, no cookie. |
| `gateway/backend/internal/gateway/chat_runs_images.go` | The executor's images branch: body build, buffered relay, response decode into a content part. |
| `gateway/frontend/src/components/shared/downloadBinary.ts` | Saving a data URL as a real file (the existing `downloadText` cannot). |
| `gateway/frontend/src/components/shared/elapsed.ts` | `formatElapsed`, lifted out of `ActiveRequestsPanel.tsx` so two call sites share one implementation. |
| `gateway/frontend/src/components/ImageTurn.tsx` | Rendering a generated-image assistant turn: images, per-item download. |

`ImageTurn.tsx` sits in `components/`, **not** `components/chat/`: `ChatMessage.tsx` lives in `components/` and `arch.test.ts:256-270` fails the suite for any import of a `components/chat/` module other than `ChatStore.tsx` or `ChatSidebar.tsx` from outside that directory.

The rest is modification of existing files, listed per task.

**Why `chat_runs_images.go` is a new file rather than more of `chat_runs.go`.**
`chat_runs.go` is already ~700 lines carrying the registry, the SSE fan-out,
the executor and the terminal commit. The images branch is a self-contained
request/response shape with no streaming, so it has a clean boundary. Sonar
flagged cognitive complexity on this repo's large functions before (issue #71
branch findings); adding a second protocol inside `executeRun` would repeat
that.

---

### Task 1: Serve the `image` capability as a model flag

The capability exists and gates the endpoint, but nothing surfaces it, so the
portal cannot know which models generate images.

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` — `ModelDTO` (~`:962-999`), and **seven** sites in `modelsResponse`
- Test: `gateway/backend/internal/portal/service_models_image_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `portal.ModelDTO.Image bool` with JSON key `image`, true only when
  **every** offering mapping (or, for a group, every offerable member) has an
  `image` capability row whose verdict is `routing.CapabilityYes`.

**There are seven sites, not one.** `visionOn` is read or written at
`service.go:2094` (create), `:2099` (seed), `:2105` (AND-fold), `:2218` (group
member check), `:2259` (group entry assign), `:2305-2306` (alias/rule
redirect) and `:2327` (DTO assign). A change that mirrors only the first three
produces a flag that is silently `false` for every model group and every alias
— wired for plain models and broken everywhere else. Do all seven.

- [ ] **Step 1: Write the failing test**

Create `gateway/backend/internal/portal/service_models_image_test.go`. The
seeding helper in `service_models_vision_test.go` (`offerModelVision`,
`:245-268`) hardcodes `routing.CapabilityVision` and takes a `vision bool`, so
it cannot seed an image row — this test brings its own row-level helper,
modelled on `TestModelsResponseVisionReadsTheVisionRowAndNoOther`'s local
`offer`/`row` closures (`:112-132`).

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// The image flag is an AND across every offering mapping, fail-closed, exactly
// like vision: a "no" row and a MISSING row both count as not capable. It is
// also INDEPENDENT of vision -- a generator that accepts no image input is
// image=true, vision=false, and conflating the two is the specific error
// routing.CapabilityImage's doc comment warns about.
func TestModelsResponseImageFlag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	offer := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
	row := func(capability, verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{
			Capability: capability, Verdict: verdict,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
		}
	}

	// sd: image=yes and NO vision row -> image true, vision false.
	offer("srv_sd", "app_sd", "map_sd", "sd", row(routing.CapabilityImage, routing.CapabilityYes))
	// vis: vision=yes and NO image row -> image false, vision true.
	offer("srv_v", "app_v", "map_v", "vis", row(routing.CapabilityVision, routing.CapabilityYes))
	// nores: an image=no row -> false, same as absent.
	offer("srv_n", "app_n", "map_n", "nores", row(routing.CapabilityImage, routing.CapabilityNo))

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	if !byID["sd"].Image {
		t.Fatal("sd image = false, want true -- its image=yes row must be found")
	}
	if byID["sd"].Vision {
		t.Fatal("sd vision = true, want false -- image is not vision; it has no vision row")
	}
	if byID["vis"].Image {
		t.Fatal("vis image = true, want false -- a vision row says nothing about generation")
	}
	if byID["nores"].Image {
		t.Fatal("nores image = true, want false -- an explicit no row is not capable")
	}
}

// A model offered by TWO mappings is image-capable only when BOTH say yes.
//
// NOTE: this test does NOT pin the fail-closed seed, despite what an earlier
// draft of this comment claimed. One of its mappings says `no`, so the AND is
// false whether the accumulator starts at the seeded true or at Go's
// zero-value false. The seed's necessity is pinned by
// TestModelsResponseImageFlag's "sd" case -- a single yes-verdict mapping,
// which reads false without the seed. Verified by reverting the seed and
// watching which test goes red.
func TestModelsResponseImageAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	offerImage := func(srvID, appID, mappingID, gateway string, capable bool) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		verdict := routing.CapabilityNo
		if capable {
			verdict = routing.CapabilityYes
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, mappingID, []routing.CapabilityRow{{
			Capability: routing.CapabilityImage, Verdict: verdict,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
		}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}

	offerImage("srv_a", "app_a", "map_a", "shared", true)
	offerImage("srv_b", "app_b", "map_b", "shared", false)

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
	if byID["shared"].Image {
		t.Fatal("shared image = true, want false -- one offering mapping says no, and the fold is an AND")
	}
}
```

Note the import block: this repo uses **one merged, alphabetically sorted**
block with stdlib and `op-ai-gateway/...` interleaved (see
`service_models_vision_test.go:1-11`). A grouped stdlib/local style fails
`golangci-lint fmt --diff`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd gateway/backend && go test ./internal/portal -run 'TestModelsResponseImage' -count=1 -v
```

Expected: FAIL — `byID["sd"].Image` does not compile (`Image` undefined on
`ModelDTO`).

- [ ] **Step 3: Add the DTO field**

In `gateway/backend/internal/portal/service.go`, append after the `Vision`
field (which is currently the struct's last, ending at `:999`):

```go
	// Image is true only when EVERY offering mapping (or, for a group, every
	// offerable member) has an "image" capability row whose verdict is yes
	// (AND aggregation, fail-closed), exactly like Vision. It is INDEPENDENT
	// of Vision: Image means the model GENERATES images, Vision means it
	// ACCEPTS them, and routing.CapabilityImage's doc comment is explicit that
	// mapping one onto the other "fails far from its cause".
	Image bool `json:"image"`
```

- [ ] **Step 4: Thread the accumulator through all seven sites**

4a. The row pick (`:2059-2074`). The existing comment justifies a single pass
by "asks each mapping exactly ONE capability question" — that becomes false, so
the comment is rewritten, and the early `break` goes (it would stop before
finding the second row):

```go
			// The fold below asks each mapping exactly TWO capability questions
			// (vision and image), so both rows are picked out here in one pass
			// rather than by keying that mapping's whole row set by name inside
			// the loop -- a map allocated per mapping to answer two lookups. A
			// mapping with no such row is simply absent, which is what the fold
			// reads as "not capable" (see below). There is no early break: it
			// would stop at whichever of the two rows came first.
			visionRows := make(map[string]routing.CapabilityRow, len(capsByMapping))
			imageRows := make(map[string]routing.CapabilityRow, len(capsByMapping))
			for mappingID, rows := range capsByMapping {
				for _, row := range rows {
					switch row.Capability {
					case routing.CapabilityVision:
						visionRows[mappingID] = row
					case routing.CapabilityImage:
						imageRows[mappingID] = row
					}
				}
			}
```

4b. Declare the accumulator beside `visionOn` (`:2094`):

```go
			// imageOn: same AND-fold, same fail-closed seeding, for the "image"
			// capability. Kept as a separate map rather than a struct so the
			// group and alias plumbing below mirrors visionOn line for line.
			imageOn := make(map[string]bool)
```

4c. Seed it to true on first sight, inside the `if _, ok := flavors[name]; !ok`
block (`:2097-2100`) — **the fold is fail-closed only because of this seed**:

```go
				if _, ok := flavors[name]; !ok {
					flavors[name] = make(map[string]struct{})
					visionOn[name] = true
					imageOn[name] = true
				}
```

4d. AND it down beside the vision fold (`:2105`):

```go
				imageOn[name] = imageOn[name] && imageRows[view.mapping.ID].Verdict == routing.CapabilityYes
```

4e. The group fold (`:2214-2224`). Add a sibling accumulator with the same
empty-member-set rule:

```go
				groupImage := make(map[string]bool, len(entries))
				for _, e := range entries {
					all := len(e.OrderedOfferableMembers) > 0
					for _, member := range e.OrderedOfferableMembers {
						if !imageOn[member] {
							all = false
							break
						}
					}
					groupImage[e.Name] = all
				}
```

4f. The group entry assignment, beside `:2259`:

```go
					imageOn[e.Name] = groupImage[e.Name]
```

4g. The alias/rule redirect, beside `:2305-2307`:

```go
					if v, ok := imageOn[rule.To]; ok {
						imageOn[name] = v
					}
```

4h. The DTO assignment, beside `:2327`:

```go
				dto.Image = imageOn[id]
```

- [ ] **Step 5: Correct the degrade log, which now says something false**

The capability-read degrade at `:2046-2058` logs
`"portal: models-listing capability read failed; vision withheld for every model"`.
After this task it withholds **both** flags. Update the message and the
comment above it:

```go
				slog.Warn("portal: models-listing capability read failed; vision and image withheld for every model",
					"mappings", len(mappingIDs), "err", capErr)
```

`TestModelsResponseCapabilityReadFailureDegradesAndLogs`
(`service_models_vision_test.go:162`) asserts on this degrade — read it and
update its expectation if it matches the message text. Also extend it to
assert `Image == false` under the same failure, since fail-closed for the new
flag is the same contract.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd gateway/backend && go test ./internal/portal -run 'TestModelsResponse' -count=1 -v
```

Expected: PASS, including the four pre-existing vision tests.
`TestModelsResponseVisionReadsTheVisionRowAndNoOther` seeds only `tools` and
`vision` rows, so reading a second capability does not disturb it.

- [ ] **Step 7: Full gates and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/portal/service.go gateway/backend/internal/portal/service_models_image_test.go gateway/backend/internal/portal/service_models_vision_test.go
git commit -m "feat: Surface the image capability as a model flag

ModelDTO gains Image, folded from the image capability row with the same
AND-across-offering-mappings, fail-closed rule as Vision, and independent of
it: image means the model GENERATES images, vision means it ACCEPTS them.

Threaded through all seven sites the vision accumulator occupies, not just the
fold: the row pick, the declaration, the fail-closed seed, the AND, the group
aggregation, the alias redirect and the DTO assignment. Wiring only the fold
would leave the flag silently false for every model group and every alias.

The row pick loses its early break and its comment, which justified one pass by
asking each mapping exactly one capability question -- now two. The
capability-read degrade's log line said vision was withheld; it withholds both,
so it now says so."
```

---

### Task 2: Operator UI for the `image` verdict

Without this the feature ships dead: the capability gate refuses by default and
no screen can change the verdict.

**Files:**
- Modify: `gateway/frontend/src/components/MappingForm.tsx` — the write loop (`:321-327`), the control block (after `:514`), and the seed/state pair mirroring `visionCapable`
- Modify: `gateway/frontend/src/components/ModelServersSection.tsx` — the displayed capability list (`:176-180`)
- Modify: `gateway/frontend/src/i18n.ts` — three keys, **both** locales
- Test: `gateway/frontend/src/components/MappingForm.test.tsx`, `gateway/frontend/src/components/ModelServersSection.test.tsx`, `gateway/frontend/src/i18n.test.ts`

**Interfaces:**
- Consumes: nothing. The backend already accepts the write —
  `mappingDTO` emits every determined row via `mappingCapabilityDTOs`, and
  `reservedManualVerdicts` (`service_applications.go:278-295`) already permits
  `(image, yes)`. No backend change belongs in this task.
- Produces: an operator path to `(image, yes)`, which every later task depends
  on for manual verification.

**The control offers two options, not three.** `reservedManualVerdicts`
refuses `(image, no)` — the backend returns `mapping.capability_reserved`
(`portal_mapping_endpoints.go:138`) — because a manual `no` outranks every
automated source forever and would permanently mask the sd-server capability
writer when it ships. So the image control offers **Unbekannt** and **Ja**
only. Rendering a "Nein" option that always 400s would be offering a control
that cannot work.

**The unknown hint must not promise re-derivation.** `image` has no automated
writer as of `b2bbb01`. `t.mappingVisionCapableUnknownHint` can say a probe
will determine it; the image hint cannot, and must say the operator decides.

- [ ] **Step 1: Write the failing tests**

In `MappingForm.test.tsx`, inside the existing
`for (const locale of ['de', 'en'] as readonly Locale[])` loop, in the same
describe block as the vision verdict tests:

```tsx
    it('sends the image verdict when the control is moved to yes', async () => {
      const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
      await pickOption(t.mappingImageCapable, t.mappingCapabilityYes);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0].capability_verdicts).toEqual({ image: 'yes' });
    });

    it('sends an empty image verdict when the control moves back to unknown', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image', 'yes')] }),
      });
      await pickOption(t.mappingImageCapable, t.mappingCapabilityUnknown);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      // '' asks for the row to be DELETED, which is the only way back out of a
      // `manual` verdict.
      expect(submitted[0].capability_verdicts).toEqual({ image: '' });
    });

    it('does not offer a "no" option for image, because the backend refuses it', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.mappingImageCapable }));
      expect(await screen.findByRole('option', { name: t.mappingCapabilityUnknown })).toBeInTheDocument();
      expect(screen.getByRole('option', { name: t.mappingCapabilityYes })).toBeInTheDocument();
      // reservedManualVerdicts refuses (image, no): a manual no outranks every
      // automated source forever and would mask the sd-server writer when it
      // ships. A control that always 400s is worse than an absent one.
      expect(screen.queryByRole('option', { name: t.mappingCapabilityNo })).not.toBeInTheDocument();
    });

    it('shows an image unknown hint that does not promise a probe', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      expect(screen.getByText(t.mappingImageCapableUnknownHint)).toBeInTheDocument();
      // The vision hint may promise re-derivation; image has no automated
      // writer yet, so its hint must not.
      expect(t.mappingImageCapableUnknownHint).not.toBe(t.mappingVisionCapableUnknownHint);
    });
```

In `ModelServersSection.test.tsx`, following the existing capability-chip
assertion shape (`:735-753`) and using that file's own four-argument `capRow`
(`:702-712` — **a different signature from MappingForm.test.tsx's**):

```tsx
    // Before this change an image row fell into the open-vocabulary bucket and
    // rendered as a raw lowercase `image` chip. It now has a translated label
    // and a fixed slot, so this test pins BOTH halves: the label appears and
    // the raw capability name does not.
    it('renders the image capability with a translated label, not as a raw chip', async () => {
      const rows = makeRows().map((r) =>
        r.mapping_id === 'map-a'
          ? { ...r, capabilities: [capRow('image', 'yes')] }
          : r,
      );
      const { api } = makeApi({
        modelServers: vi.fn().mockResolvedValue(rows),
      } as Partial<ModelServersSectionApi>);
      renderSection(api);
      await screen.findByText('GPU-Box-C');

      const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
      expect(within(cell).getByText(t.capabilityImage)).toBeInTheDocument();
      expect(within(cell).queryByText('image')).not.toBeInTheDocument();
      // Keyed by data-status, never by colour (house rule; this portal has no
      // red), and "active" like every other determined capability.
      expect(within(cell).getByText(t.capabilityImage)).toHaveAttribute('data-status', 'active');
    });

    it('renders no image chip for a missing row', async () => {
      const rows = makeRows().map((r) =>
        r.mapping_id === 'map-a' ? { ...r, capabilities: [capRow('vision', 'yes')] } : r,
      );
      const { api } = makeApi({
        modelServers: vi.fn().mockResolvedValue(rows),
      } as Partial<ModelServersSectionApi>);
      renderSection(api);
      await screen.findByText('GPU-Box-C');

      const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
      expect(within(cell).queryByText(t.capabilityImage)).not.toBeInTheDocument();
    });
```

`capRow` here takes four arguments (`capability, verdict, source?, checkedAt?`,
`:705-712`) and is **not** the same helper as `MappingForm.test.tsx`'s
two-argument `capRow`. The capabilities cell must be read through
`cellForColumn` (`:174-180`), never by a bare text query: neighbouring columns
render the identical `—` placeholder, so a positional or global assertion
survives a column being inserted ahead of the one under test.

In `i18n.test.ts`, extend with a per-feature key-presence block matching the
established shape (`:2659-2670`):

```ts
  describe('mapping image capability i18n keys', () => {
    for (const locale of ['de', 'en'] as readonly Locale[]) {
      it(`has the image capability keys in ${locale}`, () => {
        const t = messages[locale];
        expect(t.mappingImageCapable).toBeTruthy();
        expect(t.mappingImageCapableUnknownHint).toBeTruthy();
        expect(t.capabilityImage).toBeTruthy();
      });
    }
  });
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd gateway/frontend && npx vitest run src/components/MappingForm.test.tsx -t 'image'
```

Expected: FAIL — `t.mappingImageCapable` is `undefined`, so
`getByRole('combobox', { name: undefined })` cannot match.

- [ ] **Step 3: Add the three i18n keys to BOTH locales**

`i18n.ts` declares `export type PortalMessages = typeof de;` (`:2365`) and
`const en: PortalMessages = {` (`:2367`), so a key in one and not the other is
a **tsc error**, caught by `npm run build` and not by `npm test`. Add beside
the existing `mappingVisionCapable` / `capabilityVision` keys in each block:

German block:

```ts
  mappingImageCapable: 'Bilderzeugung',
  mappingImageCapableUnknownHint:
    'Unbekannt: für dieses Mapping ist nicht festgelegt, ob es Bilder erzeugt. Es gibt dafür keine automatische Erkennung — setze es auf Ja, um die Bilderzeugung für dieses Modell freizugeben.',
  capabilityImage: 'Bilderzeugung',
```

English block:

```ts
  mappingImageCapable: 'Image generation',
  mappingImageCapableUnknownHint:
    'Unknown: whether this mapping generates images is not on file. There is no automated detection for it — set it to Yes to enable image generation for this model.',
  capabilityImage: 'Image generation',
```

- [ ] **Step 4: Add the form state, the seed and the control**

Mirror the `visionCapable` / `visionCapableSeed` pair exactly (read
`MappingForm.tsx:187-227` for how a seed is captured at mount and deliberately
never re-synced), then add to the write loop at `:321-327`:

```tsx
    for (const control of [
      { capability: 'mtp', value: isMtp, seed: isMtpSeed },
      { capability: 'vision', value: visionCapable, seed: visionCapableSeed },
      { capability: 'image', value: imageCapable, seed: imageCapableSeed },
    ] as const) {
```

And the control itself, after the vision `SelectField` (ends `:514`) — note
it has **no** `no` option:

```tsx
        <SelectField
          id="mapping-image-capable"
          label={t.mappingImageCapable}
          value={imageCapable}
          onChange={(e) => setImageCapable(e.target.value as CapabilityChoice)}
          {...(imageCapable === '' ? { helperText: t.mappingImageCapableUnknownHint } : {})}
        >
          <option value="">{t.mappingCapabilityUnknown}</option>
          <option value="yes">{t.mappingCapabilityYes}</option>
        </SelectField>
```

- [ ] **Step 5: Add `image` to the displayed capability list**

In `ModelServersSection.tsx`, the list at `:176-180`:

```tsx
  { capability: 'image', label: (t) => t.capabilityImage },
```

Read the comment on `CAPABILITIES_COLUMN_EXCLUDED` (`:184-192`) first: that
list is **not** where `image` goes — it exists only for capabilities that have
their own dedicated `ListColumn` on the same table. Adding `image` to the
ordered list moves it out of the open-vocabulary bucket (`:224-238`), where it
currently renders as a verbatim `image` chip — so this step *changes* existing
output, and the test from Step 1 is what pins the new form.

- [ ] **Step 6: Run the tests and the full suite**

```bash
cd gateway/frontend && npx vitest run src/components/MappingForm.test.tsx src/components/ModelServersSection.test.tsx src/i18n.test.ts
```

Then the whole suite plus the type and format gates, because `npm test` does
not type-check and prettier is a separate CI step:

```bash
cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test
```

If `format:check` fails on the long hint strings, run `npm run format` and
re-check rather than hand-wrapping them.

- [ ] **Step 7: Commit**

```bash
git add gateway/frontend/src/components/MappingForm.tsx gateway/frontend/src/components/MappingForm.test.tsx gateway/frontend/src/components/ModelServersSection.tsx gateway/frontend/src/components/ModelServersSection.test.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/i18n.test.ts
git commit -m "feat: Give the operator a way to set and see the image capability

The image capability gated the images endpoint from the day it shipped, but no
screen could write or display its verdict, so the feature had no enablement
path. MappingForm gains an image control and ModelServersSection displays the
verdict beside the others.

The control deliberately offers only Unknown and Yes. reservedManualVerdicts
refuses (image, no) because a manual no outranks every automated source forever
and would mask the sd-server capability writer when it ships; rendering an
option that always returns mapping.capability_reserved would be offering a
control that cannot work.

The unknown hint says the operator decides rather than promising a probe, since
image has no automated writer yet.

Adding image to the ordered capability list also moves it out of
ModelServersSection's open-vocabulary bucket, where it had been rendering as a
raw lowercase chip."
```

---

### Task 3: A narrow loopback-or-bearer auth leg for `/v1/images/generations`

The run executor authenticates over the internal loopback pair as a token-less
session principal. `handleOpenAIImages` uses `requireAnyScope`, which is
bearer-only, so an image run cannot authenticate at all today.

**Files:**
- Create: `gateway/backend/internal/gateway/auth_internal_or_bearer.go`
- Modify: `gateway/backend/internal/gateway/images_handler.go:284`
- Modify: `docs/architecture/02-constraints.md:38` (the sentence this task makes imprecise)
- Test: `gateway/backend/internal/gateway/auth_internal_or_bearer_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func (s *Server) authenticateInternalOrBearer(w http.ResponseWriter, r *http.Request) (auth.Token, bool)`
  - `func (s *Server) requireInternalOrBearerAnyScope(w http.ResponseWriter, r *http.Request, scopes ...string) (auth.Token, bool)`

  Task 7's executor depends on this: without it the loopback hop 401s.

**Superseded during execution:** this task originally prescribed copying
`authenticateWeb`'s loopback block into the new helper, for side-by-side
readability. A task review raised that as an Important finding — two copies of
a constant-time comparison on an auth boundary, with nothing keeping them
equal — and the project owner ruled for extraction. The shared helper is
`func (s *Server) loopbackPrincipal(r *http.Request) (auth.Token, bool)` in
`auth.go`, called from `authenticateWeb` and from the new leg. It takes no
`ResponseWriter`, because never writing a response is what lets both callers
fall through to their next leg. `edgeGateInternalCaller` stays separate: it
only performs the comparison for a gate exemption and resolves no principal.

**Why not `requireWebAnyScope`.** That is a one-line change and grants strictly
more: it would also admit the **browser session cookie**, making
`/v1/images/generations` directly reachable from a logged-in browser and
falsifying `docs/architecture/02-constraints.md:38` for browsers as a side
effect of a feature that never wanted it. The narrow leg is
`authenticateWeb` minus its cookie block.

**Three things that make this more delicate than it looks:**

1. `authenticate` **writes a 401** before returning false (`server.go:1343`,
   `:1348`). So the loopback branch must run first and only then delegate —
   exactly as `authenticateWeb` does at `:112`. Calling `authenticate` first
   and then trying the loopback would emit a 401 body and then a second
   response.
2. The loopback branch is fail-closed on **three** independent conditions
   (`auth.go:85-88`): a non-empty `s.internalAuthSecret`, a non-nil `s.users`,
   and `UserByID` returning a nil error. Any of them failing must fall through
   **without writing a response**.
3. Use `subtle.ConstantTimeCompare`, guarded by `presented != ""`. Do not
   replace it with `==`;
   `TestEdgeGateInternalHeaderExemptionRequiresTheRealSecret`
   (`edge_scheme_test.go:218`) pins the analogous property on the sibling
   reader.

**Two routes reach this handler** — `/v1/images/generations` (`server.go:1171`)
and `/openai/v1/images/generations` (`:1170`). Widening the auth widens both,
and no test currently covers the alias. Cover it here.

- [ ] **Step 1: Write the failing tests**

Create `gateway/backend/internal/gateway/auth_internal_or_bearer_test.go`. The
loopback unit pattern already exists — `fakeUserLookup` and
`newInternalAuthServer(secret, users)` (`internal_auth_test.go:15-29`) — and
is reused verbatim rather than duplicated.

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/store"
	"testing"
)

// The images endpoint must admit the run executor's loopback principal, which
// carries no bearer token at all.
func TestInternalOrBearerAcceptsTheLoopbackPair(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user", ChatLogCommunication: true},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	tok, ok := s.authenticateInternalOrBearer(w, r)
	if !ok {
		t.Fatalf("expected the loopback pair to authenticate; status %d", w.Code)
	}
	if tok.UserID != "usr_1" || tok.ID != "" {
		t.Fatalf("unexpected principal: %+v", tok)
	}
	// A background run is never elevated: it is not an interactive session that
	// went through the System-Admin step-up.
	if tok.Elevated {
		t.Fatal("a loopback principal must not be elevated")
	}
}

// A wrong secret must fall through to the bearer leg WITHOUT the loopback
// branch having written anything -- and with no bearer present that leg then
// 401s. Fail-closed on each of the three loopback conditions.
func TestInternalOrBearerFallsThroughOnAWrongSecret(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user"},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "wrong")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("a wrong secret must not authenticate")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from the bearer leg", w.Code)
	}
}

func TestInternalOrBearerFallsThroughOnAnUnknownUser(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "nobody")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("an unresolvable user id must not authenticate")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestInternalOrBearerFallsThroughWhenNoSecretIsConfigured(t *testing.T) {
	s := newInternalAuthServer("", fakeUserLookup{"usr_1": {ID: "usr_1"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("an empty configured secret must never match")
	}
}

func TestInternalOrBearerFallsThroughWhenUsersIsNil(t *testing.T) {
	s := &Server{internalAuthSecret: "s3cret"} // users nil
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("a nil user lookup must not authenticate")
	}
}

var _ = store.User{} // keep the store import honest if the fixtures move
```

And the HTTP-level half, in `images_handler_test.go`, beside the existing
images tests. `postImages` (`:29-37`) hardcodes a bearer header and takes no
extra headers, so this adds a sibling rather than changing it — every one of
the eleven existing images tests goes through `postImages`.

```go
// postImagesWithHeaders is postImages' sibling for the auth tests: same mux,
// same path, but the caller owns the headers. postImages hardcodes a bearer and
// every existing images test depends on that, so it is left alone.
func postImagesWithHeaders(t *testing.T, srv *Server, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The bearer leg must keep working: every existing API client uses it.
func TestImagesStillAcceptsABearerToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	rec := postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The alias route shares the handler and therefore the widened auth. No test
// covered it before this change.
func TestImagesAliasRouteSharesTheAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	rec := postImagesWithHeaders(t, srv, "/openai/v1/images/generations",
		`{"model":"sd-turbo","prompt":"a cat"}`,
		map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alias route status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// THE BOUNDARY THIS TASK EXISTS TO PRESERVE: a browser session cookie must NOT
// reach this endpoint. newChatTestServer wires a real Account and loginCookie
// mints a real session, so this is the genuine article rather than a stub. A
// 401 proves auth refused; had the cookie been accepted the request would have
// failed LATER and differently (the capability gate, a 404), never with a 401.
func TestImagesRefusesASessionCookie(t *testing.T) {
	// Read chats_test.go:24-66 for newChatTestServer / seedLoginUser /
	// loginCookie and mint a cookie exactly as the chat tests do, then:
	//   rec := postImagesWithHeaders(t, srv, "/v1/images/generations",
	//       `{"model":"sd-turbo","prompt":"a cat"}`,
	//       map[string]string{"Cookie": cookie, csrfHeaderName: "1"})
	//   if rec.Code != http.StatusUnauthorized { t.Fatalf(...) }
}
```

> The last test's body is left as the exact three statements plus the two
> helper references it needs, because `loginCookie`'s and `seedLoginUser`'s
> argument lists live in two other files (`usage_activity_test.go:298`,
> `auth_test.go:42`) and must be read rather than guessed. Everything the
> assertion does is written out.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd gateway/backend && go test ./internal/gateway -run 'TestInternalOrBearer|TestImagesRefusesASessionCookie|TestImagesAliasRoute' -count=1 -v
```

Expected: FAIL to compile — `authenticateInternalOrBearer` undefined.

- [ ] **Step 3: Write the helper**

Create `gateway/backend/internal/gateway/auth_internal_or_bearer.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/subtle"
	"net/http"
	"op-ai-gateway/internal/auth"
)

// authenticateInternalOrBearer resolves a principal from the internal
// trusted-loopback header pair, else from a bearer token. It is authenticateWeb
// MINUS the session-cookie leg, and that omission is the point.
//
// Used by /v1/images/generations, which the portal-chat run executor reaches
// over the loopback pair (chat_runs.go sets internalAuthHeaderName +
// internalUserHeaderName and no bearer) while no browser ever calls it
// directly. Swapping the endpoint to requireWebAnyScope would have been one
// line and would ALSO have admitted the browser session cookie, making the
// endpoint reachable from a logged-in page and falsifying
// docs/architecture/02-constraints.md's "the other inference endpoints are
// bearer-only" for browsers -- as a side effect of a feature that never wanted
// it. See requireAnyScope (server.go) for the bearer-only original and
// requireWebAnyScope (auth.go) for the full web ladder.
//
// Order matters: the loopback branch runs FIRST because s.authenticate writes a
// 401 before returning false. Fail-closed on each of its three conditions --
// an absent configured secret, a nil user lookup, or any lookup error -- by
// falling through WITHOUT writing a response.
func (s *Server) authenticateInternalOrBearer(w http.ResponseWriter, r *http.Request) (auth.Token, bool) {
	if s.internalAuthSecret != "" && s.users != nil {
		if presented := r.Header.Get(internalAuthHeaderName); presented != "" &&
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.internalAuthSecret)) == 1 {
			if user, err := s.users.UserByID(r.Context(), r.Header.Get(internalUserHeaderName)); err == nil {
				// Never elevated: a background/internal run is not an
				// interactive session that went through the System-Admin
				// step-up, and the request carried no cookie to resolve
				// elevation from.
				return sessionPrincipal(user, false), true
			}
		}
	}
	return s.authenticate(w, r)
}

// requireInternalOrBearerAnyScope resolves a principal via
// authenticateInternalOrBearer and requires at least one of the given scopes.
// The scope check is identical to requireAnyScope's and requireWebAnyScope's;
// only the authentication legs differ.
func (s *Server) requireInternalOrBearerAnyScope(w http.ResponseWriter, r *http.Request, scopes ...string) (auth.Token, bool) {
	token, ok := s.authenticateInternalOrBearer(w, r)
	if !ok {
		return auth.Token{}, false
	}
	if !hasAnyScope(token, scopes) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient scope", ""))
		return auth.Token{}, false
	}
	return token, true
}
```

The `apierror` import is needed for the scope refusal — copy the import path
from `auth.go`'s own import block rather than guessing it.

- [ ] **Step 4: Point the handler at it**

`images_handler.go:284`:

```go
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
```

And update the handler's doc comment (`:269`) to say which legs it accepts and
why, since the previous comment implied bearer-only.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd gateway/backend && go test ./internal/gateway -run 'TestInternalOrBearer|TestImages' -count=1 -v
```

Expected: PASS, including all eleven pre-existing images tests through
`postImages`.

- [ ] **Step 6: Correct the architecture doc this task makes imprecise**

`docs/architecture/02-constraints.md:38` currently reads that
`/v1/chat/completions` also accepts the session while "the other inference
endpoints are bearer-only". After this task `/v1/images/generations` also
accepts the **loopback** leg. The sentence must distinguish the three legs:
bearer for API clients, the session cookie for `/v1/chat/completions` only,
and the internal loopback pair for the two endpoints the run executor calls.
Write it so a reader can still tell that **no browser** reaches the images
endpoint.

`make lint-docs` does not check auth prose, so this correctness is on this
step.

- [ ] **Step 7: Gates and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./... && cd ../.. && make lint-docs
```

```bash
git add gateway/backend/internal/gateway/auth_internal_or_bearer.go gateway/backend/internal/gateway/auth_internal_or_bearer_test.go gateway/backend/internal/gateway/images_handler.go gateway/backend/internal/gateway/images_handler_test.go docs/architecture/02-constraints.md
git commit -m "feat: Admit the loopback principal to the images endpoint, and only it

The portal-chat run executor authenticates over the internal loopback pair as a
token-less session principal, but handleOpenAIImages used requireAnyScope,
which reads a bearer and nothing else -- so an image run could not authenticate
at all.

The new leg is authenticateWeb minus its session-cookie branch. Swapping to
requireWebAnyScope would have been one line and would also have admitted the
browser cookie, making the endpoint reachable from a logged-in page and
falsifying the documented bearer-only boundary for browsers as a side effect.
No browser calls this endpoint; only the executor does.

The loopback branch runs before the bearer branch because authenticate writes a
401 before returning false, and it stays fail-closed on all three of its
conditions -- absent secret, nil user lookup, failed lookup -- by falling
through without writing a response. The secret comparison stays
constant-time.

Tests cover the loopback accept, all four fall-through paths, the preserved
bearer path, the previously untested /openai alias that shares the handler, and
the boundary this change exists to keep: a real session cookie from a real
logged-in account is still refused with a 401.

02-constraints.md's auth sentence now distinguishes the three legs instead of
calling every non-chat inference endpoint bearer-only."
```

---

### Task 4: Assistant turns can carry structured content

`AssistantTurn.Content` is a `string` and `writeAssistant` sets
`"content": turn.Content` unconditionally, so an image turn has nowhere to go.

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` — `AssistantTurn` (`:487-497`), `writeAssistant`'s `msg` map (`:537-542`)
- Test: `gateway/backend/internal/portal/service_chats_test.go`

**Interfaces:**
- Produces: `AssistantTurn.ContentParts json.RawMessage`. When
  `len(ContentParts) > 0` it is written as `content`; otherwise `Content`
  (the string) is, exactly as today. Task 7 sets it.

**Three traps, all verified by execution:**

1. **Add a field; do not change `Content`.** Every existing caller keeps
   compiling and the plain-text wire form stays byte-identical.
2. **Guard on `len(...) > 0`, not on `!= nil`.** A non-nil zero-length
   `json.RawMessage` makes `json.Marshal` **fail** with "unexpected end of JSON
   input", while a nil one marshals to `null`. Either would be a regression;
   the length guard avoids both.
3. **Do not replace the `msg` map with a struct.** `msg` is a
   `map[string]any`, so `encoding/json` sorts its keys alphabetically
   (`content, id, reasoning, reasoningMs, role, status, tokensPerSecond, tps,
   ttftMs`). A struct would emit declaration order and change every persisted
   document's byte layout for no reason.

No store, schema or migration change: the transcript is one opaque blob column
(`chats.blob`, `blob` on sqlite and `bytea` on postgres from a single DDL
string). Run the postgres leg anyway per the Global Constraints — a change
that *needed* one and did not get it would look identical locally.

- [ ] **Step 1: Write the failing test**

Append to `service_chats_test.go`, modelled on
`TestCheckpointThenCommitAssistant` (`:245-285`):

```go
// An image turn's content is a structured array of parts, not a string. The
// parts are written verbatim under `content` so buildAPIHistory feeds them back
// as a vision input on the next turn, exactly as an uploaded image already is.
func TestCommitAssistantWritesStructuredContentParts(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u1","role":"user","content":"a cat"}]}`),
	})

	parts := json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]`)
	if err := svc.CommitAssistant(ctx, owner, created.ID, AssistantTurn{ContentParts: parts}, "complete"); err != nil {
		t.Fatal(err)
	}

	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"content":[{"type":"image_url"`) {
		t.Fatalf("structured content not written verbatim: %s", got.Content)
	}
	if strings.Contains(string(got.Content), `"content":""`) {
		t.Fatalf("the empty Content string leaked into the document: %s", got.Content)
	}
	if !strings.Contains(string(got.Content), `"status":"complete"`) {
		t.Fatalf("status not written: %s", got.Content)
	}
}

// The text path must stay byte-identical: ContentParts absent means the string
// is written exactly as before.
func TestCommitAssistantTextPathUnchanged(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u1","role":"user","content":"hi"}]}`),
	})

	if err := svc.CommitAssistant(ctx, owner, created.ID, AssistantTurn{Content: "full answer"}, "complete"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"content":"full answer"`) {
		t.Fatalf("the string content must still be written as a JSON string: %s", got.Content)
	}
}

// A zero-length (non-nil) ContentParts must not reach json.Marshal: it fails
// with "unexpected end of JSON input" and would break the commit entirely.
func TestCommitAssistantEmptyContentPartsFallsBackToTheString(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u1","role":"user","content":"hi"}]}`),
	})

	turn := AssistantTurn{Content: "text", ContentParts: json.RawMessage([]byte{})}
	if err := svc.CommitAssistant(ctx, owner, created.ID, turn, "complete"); err != nil {
		t.Fatalf("an empty ContentParts must not fail the commit: %v", err)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"content":"text"`) {
		t.Fatalf("expected the string fallback: %s", got.Content)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd gateway/backend && go test ./internal/portal -run 'TestCommitAssistant' -count=1 -v
```

Expected: FAIL to compile — `ContentParts` undefined.

- [ ] **Step 3: Add the field**

In `AssistantTurn` (`:487-497`), after `Content`:

```go
	// ContentParts is a structured message content (an array of OpenAI-style
	// content parts) for a turn whose output is not text -- an image run's
	// generated image_url part. When it is non-EMPTY it is written as the
	// message's `content` and Content is ignored; otherwise Content is written
	// as before, byte for byte.
	//
	// The guard is on LENGTH, not on nil: json.Marshal turns a nil RawMessage
	// into `null` but FAILS outright on a non-nil zero-length one
	// ("unexpected end of JSON input"), which would break the terminal commit.
	ContentParts json.RawMessage
```

- [ ] **Step 4: Use it in `writeAssistant`**

The `msg` map at `:537-542` stays a `map[string]any` — see trap 3:

```go
	msg := map[string]any{
		"id":     id,
		"role":   "assistant",
		"status": status,
	}
	if len(turn.ContentParts) > 0 {
		msg["content"] = turn.ContentParts
	} else {
		msg["content"] = turn.Content
	}
```

- [ ] **Step 5: Run the tests, the package, and the postgres leg**

```bash
cd gateway/backend && go test ./internal/portal -run 'TestCommitAssistant|TestCheckpointThenCommit' -count=1 -v
```

```bash
docker start op-pg-test
export OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable'
cd gateway/backend && go test ./internal/store/ -count=1
```

- [ ] **Step 6: Gates and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/portal/service_chats.go gateway/backend/internal/portal/service_chats_test.go
git commit -m "feat: Let an assistant turn carry structured content parts

An image turn's output is an array of OpenAI-style content parts, not a string,
and writeAssistant wrote turn.Content unconditionally.

Adds AssistantTurn.ContentParts rather than changing Content, so every existing
caller compiles untouched and the text wire form stays byte-identical. The msg
map stays a map[string]any deliberately: encoding/json sorts its keys, and a
struct would emit declaration order and rewrite the byte layout of every
persisted document for no reason.

The guard is on length, not nil. json.Marshal turns a nil RawMessage into null
but fails outright on a non-nil zero-length one, so a nil check would have left
a way to break the terminal commit.

No schema or migration change on any driver: the transcript is a single opaque
blob column. The postgres leg was run explicitly, since it skips silently and a
change that did need a migration would look identical without it."
```

---

### Task 5: Pin the thread's kind, server-side

See spec §6. The obvious place does not work: `PrepareChatRun` never reads the
persisted settings — it **replaces** them with the client's
(`doc.Settings = settingsRaw`, `service_chats.go:399`) — and
`startRunRequest.Settings` is `portal.ChatRunSettings` verbatim
(`chat_run_endpoints.go:23`), so a new field is client-settable the moment it
exists.

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` — `ChatRunSettings` (`:280-301`), `PrepareChatRun` (`:342-414`)
- Test: `gateway/backend/internal/portal/service_chats_kind_test.go` (create)

**Interfaces:**
- Produces: `ChatRunSettings.Kind string \`json:"kind,omitempty"\`` — `""`
  (text) or `"image"`. `PrepareChatRun` reads the stored kind **before** the
  overwrite and forces it back onto the submitted settings; the first send
  establishes it. Task 7 branches the executor on `prep.Settings.Kind`.

**Why a string, appended last.** A string is the same value that selects the
request URL and extends to a future audio kind without a second boolean.
Appending it **last** in the struct is what makes the empty case
byte-identical — verified by running `json.Marshal` on both struct shapes.

**Why the client's first-send value is accepted.** The pin is a UI constraint,
not an authorization (spec §6). A client that lies pins its own chat to a kind
whose sends the capability gate then refuses — self-inflicted, not a
privilege. Deriving the kind server-side would need the models fold, which does
**not** run on the send path today and costs several store reads per send. The
gate stays the authority; this field only decides what the composer offers.

- [ ] **Step 1: Write the failing test**

Create `gateway/backend/internal/portal/service_chats_kind_test.go`. The
settings round-trip reader already exists as `runSettingsFromContent(t,
content)` (`service_chats_server_override_test.go:80`) — read its signature
there and reuse it rather than re-parsing the document by hand.

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// The first send establishes the kind and it is persisted.
func TestPrepareChatRunPinsTheKindOnFirstSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings:    ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "image" {
		t.Fatalf("Kind = %q, want image", settings.Kind)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"image"`) {
		t.Fatalf("kind not persisted: %s", got.Content)
	}
}

// THE POINT OF THE TASK: a later send cannot change it, however the client
// submits it. ChatRunSettings is the POST body verbatim, so this is reachable
// from the API, not just from our own UI.
func TestPrepareChatRunRefusesToRepinTheKind(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings:    ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send submits the OPPOSITE kind (and an empty one, below).
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"hello"}`),
		Settings:    ChatRunSettings{Model: "llama", Kind: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "image" {
		t.Fatalf("Kind = %q, want the pinned image -- a later send must not unpin it", settings.Kind)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"image"`) {
		t.Fatalf("the pin was lost from the document: %s", got.Content)
	}
}

// A text chat stays byte-identical: an absent kind must not add the key. This
// is what `omitempty` plus last-in-the-struct buys, and it matters because the
// whole document is one blob that every save rewrites.
func TestPrepareChatRunOmitsAnEmptyKind(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
		Settings:    ChatRunSettings{Model: "llama"},
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := svc.GetChat(ctx, owner, created.ID)
	if strings.Contains(string(got.Content), "kind") {
		t.Fatalf("an empty kind must not appear in the persisted settings at all: %s", got.Content)
	}
}
```

`PrepareChatRun`'s signature is
`(ctx, owner, chatID string, req PrepareRunRequest) ([]ChatAPIMessage, ChatRunSettings, error)`
(`service_chats.go:342`), and `PrepareRunRequest` is
`{UserMessage json.RawMessage; EditedHistory []json.RawMessage; Settings ChatRunSettings}`
(`:307-311`).

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/backend && go test ./internal/portal -run 'TestPrepareChatRun.*Kind|TestPrepareChatRunRefusesToRepin' -count=1 -v
```

Expected: FAIL to compile — `Kind` undefined on `ChatRunSettings`.

- [ ] **Step 3: Add the field, LAST in the struct**

```go
	// Kind pins what this thread's runs are: "" (text, the default and every
	// pre-existing chat) or "image". It is established by the FIRST send and
	// then forced by PrepareChatRun on every later send, because the composer's
	// affordances follow the thread rather than the currently-picked model --
	// see the spec's "the kind is pinned to the thread at first send".
	//
	// It is a UI constraint, NOT an authorization: this struct is the POST
	// body verbatim (chat_run_endpoints.go), so a client can submit any value
	// on the first send. The routing capability gate remains the only
	// authority on what the gateway will actually serve.
	//
	// Declared LAST and omitempty on purpose: that is what keeps an existing
	// text chat's persisted settings byte-identical.
	Kind string `json:"kind,omitempty"`
```

- [ ] **Step 4: Force the pin in `PrepareChatRun`**

Before the marshal at `:395`, read the kind out of the document that was just
opened and re-impose it:

```go
	// The stored kind wins over whatever the client submitted. doc.Settings is
	// about to be REPLACED wholesale by the submitted settings (below), so the
	// pin has to be lifted out first or it is silently unpinned on every send.
	var stored struct {
		Kind string `json:"kind"`
	}
	if len(doc.Settings) > 0 {
		_ = json.Unmarshal(doc.Settings, &stored) // best-effort: a malformed blob leaves the kind unpinned, which is the pre-feature behaviour
	}
	if hadPriorMessages {
		// Force UNCONDITIONALLY on a later send, including back to the empty
		// (text) kind. Guarding on `stored.Kind != ""` cannot pin TEXT: the
		// field is omitempty, so a text thread stores no `kind` key at all and
		// reads back as "" -- indistinguishable from a thread that was never
		// sent. A client submitting {"kind":"image"} on the second send would
		// then flip the thread, and its next turn would go to the images
		// endpoint, which carries no history, silently discarding the
		// conversation. hadPriorMessages is captured from doc.Messages BEFORE
		// the append/replace above: the pin is established by the FIRST send,
		// so "has this thread been sent before?" is exactly the question.
		req.Settings.Kind = stored.Kind
	}
```

Place it immediately above `settingsRaw, err := json.Marshal(req.Settings)` so
the relationship is local and obvious.

**Caveat to document in the code.** `CreateChatRequest.Content` is
client-supplied opaque JSON, so a chat created with messages already in it
would have its first send treated as a later one and forced to text. No
current call site does that — chat creation seeds `messages: []` — but the
assumption belongs in the comment so the next reader can evaluate it rather
than rediscover it.

**Rejected: writing an explicit `"text"` kind on first send.** It would make
the stored data self-describing and the logic trivial, but it writes a new key
into every text chat's settings for the overwhelmingly common case, and the
whole document is one blob that every save rewrites. One boolean is cheaper.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd gateway/backend && go test ./internal/portal -count=1
```

Also run the server-override suite, which asserts on the **exact** persisted
settings JSON and is the one place a stray key shows up:

```bash
cd gateway/backend && go test ./internal/portal -run 'ServerOverride' -count=1 -v
```

And the HTTP-level chat test that pins the exact persisted settings
(`gateway/internal/gateway/chats_test.go`):

```bash
cd gateway/backend && go test ./internal/gateway -run 'TestPortalChats' -count=1 -v
```

- [ ] **Step 6: Gates and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/portal/service_chats.go gateway/backend/internal/portal/service_chats_kind_test.go
git commit -m "feat: Pin a chat thread's kind so a model switch cannot change it

A thread whose model generates images is image-only, and that has to be a
property of the THREAD: the model is switchable between every turn, and
switching an image thread to a vision model would otherwise POST the whole
multi-megabyte image history as vision input.

The obvious implementation does not work. PrepareChatRun never reads the
persisted settings -- it replaces them wholesale with the ones the client
submitted -- and ChatRunSettings IS the POST body, so a field added there is
client-settable immediately. A kind merely written into the settings is
therefore not a pin at all. It is now lifted out of the stored document before
the overwrite and forced back onto the submitted settings, so the first send
establishes it and no later send can move it.

Kind is a string rather than a boolean because it is the same value that
selects the request URL and extends to a future audio kind, and it is declared
last with omitempty because that is what keeps an existing text chat's
persisted settings byte-identical.

It is explicitly a UI constraint and not an authorization: the routing
capability gate stays the only authority on what the gateway serves. Deriving
the kind server-side instead would need the models capability fold, which does
not run on the send path and costs several store reads per send."
```

---

### Task 6: Give a run a start instant, a kind, and a deadline

Spec §3.6's elapsed clock reads a value that does not exist: the only
`time.Time` on a `ChatRun` is `endedAt` (`chat_runs.go:168`).

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs.go` — `ChatRun` (`:155-169`), `newChatRun` (`:171-177`), `snapshotLocked` (`:179-185`), `runEvent` (`:129-136`), `reserveRun` (`:436-444`), `executeRun`'s cancel branch (`:519-523`), `finishRun` (`:657-679`)
- Modify: `gateway/backend/internal/gateway/chat_run_endpoints.go` — `activeRunDTO` (`:34-38`), `startRunResponse` (`~:26-31`), `launchRun`'s call site (`:104`)
- Modify: `gateway/backend/internal/gateway/api.go` — **hand-edit only** if a signature changes; `docs/architecture/11-risks-and-technical-debt.md:170` records that the `interfacer` regeneration is broken
- Test: `gateway/backend/internal/gateway/chat_runs_test.go`

**Interfaces:**
- Produces:
  - `ChatRun.startedAt time.Time` — immutable after construction, so reading
    it under `r.mu` in `snapshotLocked` is safe.
  - `ChatRun.kind string` — set by `launchRun` from `prep.Settings.Kind`.
  - `runEvent.Kind string \`json:"kind,omitempty"\`` and
    `runEvent.ElapsedMs int64 \`json:"elapsed_ms,omitempty"\`` on the snapshot.
  - `activeRunDTO.Kind` / `.ElapsedMs`, and the same two on `startRunResponse`.
  - A configurable run deadline.

  Task 14's clock and Task 13's composer read these.

**The clock must be server-anchored.** Deriving elapsed time from the moment of
Send is not equivalent: a reopened tab never saw that moment, and a second tab
never saw it either. Both must display the same true number.

**`startRunResponse` carries the kind too, not just the snapshot.** Between the
201 and the first SSE snapshot the sending tab knows nothing about the run. If
the kind arrives only on the snapshot, that window renders the *text* pending
state — i.e. exactly the `0 Zeichen` counter this feature exists to remove, on
the most common path.

**Two traps in the deadline, both real:**

1. **A timeout would be reported as a user cancel, in TWO places — and the
   second is the one that matters here.** `executeRun` turns any context error
   into `finishRun(..., "canceled", "")` with an empty message (`:519-523`),
   and `consumeRunStream`'s scanner-error branch does the identical thing. The
   **second** is the branch an image run actually reaches: the loopback hop
   writes 200 plus the SSE headers before the provider has produced anything,
   so a dispatched-then-silent run hits its deadline while reading the body,
   not at `Do`. Fixing only `executeRun` leaves the image case reporting a
   timeout as a user cancel. Both branches need the
   `errors.Is(ctx.Err(), context.DeadlineExceeded)` split.
   *(Corrected during execution: this plan originally named only `executeRun`.)*
2. **The deadline could abort its own commit.** `finishRun` is called with the
   run's context on three paths (`:484`, `:489`, `:529`), and its terminal
   `CommitAssistant` (`:668`) would then be cancelled by the very timeout that
   ended the run — losing the turn instead of recording it. Detach it:
   `context.WithoutCancel`, already used in this codebase for exactly this
   reason (`benchmark_vram_runner.go:236`, `agent_netbird_gate.go:103`).

**Do not hold both locks.** `reg.mu` and a run's `r.mu` are never held at the
same time — `ActiveForUser` (`:324-339`) snapshots candidates under `reg.mu`,
releases it, and only then reads each run. Whatever surfaces the age in the
active-runs list must keep that order.

- [ ] **Step 1: Write the failing tests**

In `chat_runs_test.go`, using the existing `newRunTestServer(t)` /
`newRunTestServerWithProvider(t, prov)` and `waitFor(t, cond)` helpers
(`:233`, `:240`, `:296`) and the `pacedStreamer` provider fake:

```go
// The snapshot must carry a server-measured age, because a reopened tab never
// saw the moment of Send and must still show the same true number.
func TestRunSnapshotCarriesAServerMeasuredAge(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
	if err != nil {
		t.Fatal(err)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.ElapsedMs < 0 {
		t.Fatalf("ElapsedMs = %d, want >= 0", snap.ElapsedMs)
	}
	// A freshly-registered run is young; the point is that the field is
	// POPULATED from the run's own start instant rather than absent.
	if run.startedAt.IsZero() {
		t.Fatal("startedAt must be set at construction")
	}
}

// The kind reaches the client on the 201 as well as on the snapshot. Without it
// the sending tab renders the TEXT pending state for the whole window between
// the 201 and the first snapshot -- the 0-Zeichen counter, on the common path.
func TestStartRunResponseCarriesTheKind(t *testing.T) {
	srv, _, chatID := newRunTestServer(t)
	rec := startRunViaHandler(srv, chatID,
		`{"user_message":{"id":"u9","role":"user","content":"a cat"},"settings":{"model":"sd-turbo","kind":"image"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		RunID     string `json:"run_id"`
		Status    string `json:"status"`
		Kind      string `json:"kind"`
		ElapsedMs int64  `json:"elapsed_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode 201: %v (%s)", err, rec.Body.String())
	}
	if got.Kind != "image" {
		t.Fatalf("kind = %q on the 201, want image -- otherwise the sending tab "+
			"renders the text pending state until the first snapshot", got.Kind)
	}
	if got.RunID == "" {
		t.Fatal("run_id missing from the 201")
	}
}

// A deadline must NOT masquerade as a user cancel: the user pressed nothing.
func TestRunDeadlineIsDistinguishableFromACancel(t *testing.T) {
	restore := runDeadline
	runDeadline = 10 * time.Millisecond
	t.Cleanup(func() { runDeadline = restore })

	// A provider that never produces anything, so the deadline is what ends the
	// run. pacedStreamer with a gap far beyond the deadline is the existing fake
	// for "slow upstream" (server_stream_timeout_test.go).
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 1, gap: time.Minute})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		Settings: portal.ChatRunSettings{Model: "gpt-oss-20b"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", got)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.Err != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q -- an empty message is "+
			"what a USER cancel looks like, and conflating the two is the "+
			"silent-conflation this feature refuses", snap.Err, runTimedOutMessage)
	}
}
```

`startRunViaHandler(srv *Server, chatID, body string) *httptest.ResponseRecorder`
(`chat_run_endpoints_test.go:81`) takes no `*testing.T` and authenticates with
`authBearer(r, "dev-secret")` itself. `srv.startChatRun(owner, chatID, prep)`
returns `(*ChatRun, error)` (`chat_runs.go:467`). Check `pacedStreamer`'s field
names in `server_stream_timeout_test.go` before using it — the run tests inject
it as a `provider.Client`.

- [ ] **Step 2: Add the fields**

```go
type ChatRun struct {
	ID     string
	ChatID string
	UserID string

	// startedAt is the run's own start instant, set once at construction and
	// never written again -- so it is safe to read under r.mu in
	// snapshotLocked without a self-locking accessor. It exists because a
	// client cannot compute an honest age: a reopened or second tab never saw
	// the moment of Send, and every view must show the same true number.
	startedAt time.Time
	// kind mirrors the thread's pinned kind ("" for text, "image"), set by
	// launchRun from the prepared settings. It is the SAME value that selects
	// the request URL, so the UI can never describe a turn the executor did
	// not run.
	kind string

	mu          sync.Mutex
	...
}
```

`newChatRun` sets `startedAt`. It has no clock dependency today, so take the
time there directly rather than threading a `Clock` through — and say so in a
comment, because most of this package's timestamps do come from a clock.

- [ ] **Step 3: Make the deadline a package var and bound the run**

Beside `runCheckpointInterval` (`:414`):

```go
// runDeadline bounds a single chat run end to end. It exists so an unstreamed
// image run is a BOUNDED wait rather than an open-ended one: with no
// incremental events, "this finishes or fails within N minutes" is the only
// honest thing the UI can promise, and it has to be true. A package var, not a
// const, so tests can shrink it -- mirroring runCheckpointInterval.
var runDeadline = 10 * time.Minute
```

`reserveRun` (`:436-444`) becomes `context.WithTimeout(context.Background(),
runDeadline)`. Keep returning the `cancel` to the registry unchanged:
`releaseRun`, `cancelChat` and `handleCancelChatRun` all still need it.

- [ ] **Step 4: Handle both traps, and replace the two skipped tests**

Trap 1 — `executeRun`'s context branch (`:519-523`):

```go
		if ctx.Err() != nil {
			// A deadline is NOT a cancel, and reporting it as one would be the
			// same silent conflation this feature refuses elsewhere: the user
			// pressed nothing.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.finishRun(context.Background(), owner, run, "error", runTimedOutMessage)
				return
			}
			s.finishRun(context.Background(), owner, run, "canceled", "")
			return
		}
```

Define `runTimedOutMessage` as a coded string the frontend can map (Task 8
owns the mapping) rather than prose.

Trap 2 — inside `finishRun`, the terminal commit must not inherit the
deadline:

```go
	// The commit must outlive the deadline that ended the run: finishRun is
	// called with the run's own ctx on three paths, so on a timeout the ctx is
	// ALREADY expired and the commit below would be cancelled before it wrote
	// -- losing the turn instead of recording why it ended. Same idiom as
	// benchmark_vram_runner.go:236.
	commitCtx := context.WithoutCancel(ctx)
```

and use `commitCtx` for the `CommitAssistant` call at `:668`.

Now replace the two `t.Skip`s with real bodies: `runDeadline` exists, so the
deadline test can shrink it (save, override, `t.Cleanup` restore) and assert
the terminal status is `error` with `runTimedOutMessage`; and read
`startRunViaHandler`'s signature in `chat_run_endpoints_test.go` to finish the
201 test.

- [ ] **Step 5: Surface the two values on all three wire shapes**

`runEvent` gains `Kind` and `ElapsedMs` (both `omitempty`), populated in
`snapshotLocked` from `r.kind` and `time.Since(r.startedAt).Milliseconds()`.
Note the existing contract that `metrics` is present on `snapshot` and `done`
but never on `delta` — do not add these to deltas either; the client ticks its
own clock between snapshots from the anchor the snapshot gave it.

`activeRunDTO` and `startRunResponse` gain the same two fields. `launchRun`
(`:104`) sets `run.kind` from `settings.Kind` before the 201 is written.

- [ ] **Step 6: Gates and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

```bash
git add gateway/backend/internal/gateway/chat_runs.go gateway/backend/internal/gateway/chat_runs_test.go gateway/backend/internal/gateway/chat_run_endpoints.go gateway/backend/internal/gateway/chat_run_endpoints_test.go
git commit -m "feat: Give a chat run a start instant, a kind and a deadline

An unstreamed image run emits nothing between dispatch and its finished image,
so the only honest things a client can show are liveness, an elapsed time and a
bound. None of the three existed: the only time.Time on a ChatRun was endedAt,
nothing carried the kind, and a run was unbounded.

startedAt is set once at construction and never rewritten, so snapshotLocked
can read it under r.mu without a self-locking accessor. The age is
server-measured on purpose: a reopened or second tab never saw the moment of
Send, and every view has to show the same true number.

The kind rides on the 201 as well as on the snapshot. Between the 201 and the
first snapshot the sending tab would otherwise know nothing about the run and
render the text pending state -- the 0-Zeichen counter this feature exists to
remove, on the most common path.

The deadline needed two fixes beyond adding it. executeRun turned every context
error into 'canceled' with an empty message, so a timeout would have been
indistinguishable from the user pressing Stop; it now reports a timeout as its
own coded error. And finishRun runs on the run's own context on three paths, so
the deadline would have cancelled the very commit that records the outcome --
the commit is now detached with context.WithoutCancel, the same idiom the vram
runner already uses."
```

---

### Task 7: The executor's second target

**Files:**
- Create: `gateway/backend/internal/gateway/chat_runs_images.go`
- Modify: `gateway/backend/internal/gateway/chat_runs.go` — `executeRun` (`:476-536`) branches on `run.kind`
- Test: `gateway/backend/internal/gateway/chat_runs_images_test.go` (create)

**Interfaces:**
- Consumes: Task 3's auth leg, Task 4's `AssistantTurn.ContentParts`, Task 5's
  `Settings.Kind`, Task 6's `run.kind`.
- Produces: an image run that ends with a committed assistant turn whose
  content is `[{"type":"image_url","image_url":{"url":"data:<mime>;base64,<b64>"}}]`,
  one part per `data[]` item.

**Why a new file.** `consumeRunStream` (`:538`) opens a `bufio.Scanner` over
`resp.Body` and drives the checkpoint goroutine and the rate metrics — every
line of it assumes SSE. An images response is a single buffered JSON body with
no deltas, no checkpoints and no rates. Branching inside `executeRun` and
reusing none of the stream machinery is the honest structure.

**No Go type decodes an images response anywhere in the repo** — `b64_json`,
`output_format` and `revised_prompt` appear only in test fixtures and doc
comments. This task introduces the first one:

```go
// imagesResponse is the upstream body this relay consumes. The shape is taken
// from the repository's own fixtures
// (images_handler_test.go:351): {"created":1,"output_format":"png","data":[{"b64_json":"..."}]}.
//
// OutputFormat is what makes the data: URL's MIME type a REPORTED value rather
// than an assumption. Hardcoding image/png would be a fabricated measurement of
// the same class as a progress bar over an endpoint that reports no progress.
type imagesResponse struct {
	OutputFormat string `json:"output_format"`
	Data         []struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}
```

**`data[]` is plural by design.** `imagesDataCounter`'s own doc
(`images_handler.go:537-539`) says the billed quantity comes from the response
because "a partial failure makes those differ". Build one content part per
item; do not take `data[0]`.

**The request body sends `response_format: "b64_json"` explicitly.**
`validateImagesRequest` accepts absent-or-`b64_json` (`:396`), and sd-server
returns that shape regardless, but stating it makes the contract this executor
depends on visible instead of inherited from a default.

**`n`, `size`, `temperature` and `max_tokens` are not sent.**
`/v1/images/generations` uses none of the chat parameters, and `n` is not
exposed by this feature (spec §4). Sending a parameter the endpoint ignores
would be the enabled-but-inert control the spec rejects.

- [ ] **Step 1: Write the constructor this task needs, then the test**

No existing helper can drive an image run: `newRunTestServerWithProvider`
(`chat_runs_test.go:240-293`) has the loopback wiring, the chat store, the
cipher, `ChatRuns` and `selfBaseURL`, but seeds routes through
`seedGatewayTestRoutes`, which has no image-capable mapping — and
`newImageCapableTestServer` (`images_handler_test.go:279-330`) has the mapping
but none of the run machinery. So the first thing this task writes is their
union, in the new test file:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newImageRunTestServer is the union of newRunTestServerWithProvider (the run
// executor's loopback wiring, chat store, cipher, ChatRuns and selfBaseURL) and
// newImageCapableTestServer's seeding (an image:yes mapping reachable at a real
// httptest address). Neither existing helper can drive an image run on its own.
//
// The provider is the real OpenAI-compatible HTTP client, not provider.Mock:
// the images relay goes out over proxyNative and must actually reach upstream.
//
// ApplicationEndpoint builds the origin from the SERVER's Domain plus the
// APPLICATION's Scheme/Port, so upstreamURL has to be decomposed into those
// fields -- the same requirement newImageCapableTestServer documents.
func newImageRunTestServer(t *testing.T, upstreamURL string) (*Server, auth.Token, string) {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token",
		Active: true, Scopes: []string{"gateway:use"},
	}, "dev-secret")
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{
		ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User",
		Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: now, UpdatedAt: now,
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()

	ctx := context.Background()
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-turbo", Name: "SD Turbo Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo", ServerID: "srv-sd-turbo", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-turbo", ApplicationID: "app-sd-turbo", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-turbo", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: store.NewMemoryChatStore(0), Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:             routeStore,
		Portal:             svc,
		Captures:           &fakeCaptureStore{},
		Cipher:             cipher,
		InternalAuthSecret: "test-internal-secret",
		Users:              directory,
		ChatRuns:           NewChatRunRegistry(5),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(ctx, owner, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u","role":"user","content":"a cat"}]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return srv, owner, created.ID
}

// An image run ends with a committed assistant turn holding one image_url part
// per upstream data[] item, with the MIME taken from output_format rather than
// assumed.
func TestImageRunCommitsAnImagePartPerDataItem(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="},{"b64_json":"BB=="}]}`))
	}))
	defer upstream.Close()

	srv, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	// One part per data[] item -- the endpoint's own counter takes the billed
	// quantity from the response for exactly this reason, so the UI must not
	// assume a single image.
	if n := strings.Count(string(got.Content), `"type":"image_url"`); n != 2 {
		t.Fatalf("image_url parts = %d, want 2: %s", n, got.Content)
	}
	// The MIME is REPORTED, not assumed. Hardcoding image/png would be a
	// fabricated measurement of the same class as a fake progress bar.
	if strings.Count(string(got.Content), `data:image/png;base64,`) != 2 {
		t.Fatalf("data URLs must carry the upstream output_format: %s", got.Content)
	}
}

// A 2xx with an empty data[] is an ERROR, not an empty success: the endpoint
// already logs a counted zero on a 2xx at Error level, and committing an
// assistant turn with no image would render as a blank bubble.
func TestImageRunTreatsAnEmptyDataArrayAsAnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[]}`))
	}))
	defer upstream.Close()

	srv, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error for a zero-image 2xx", got)
	}
}
```

Check `srv.startChatRun`'s and `run.statusValue()`'s exact signatures in
`chat_runs_test.go:308-330` before running — the existing end-to-end run tests
use both, and `startChatRun` is the test-facing entry point that takes a
`PrepareRunResult`.

*(Corrected during execution, three defects in the above.* **One:** neither
prescribed test passes a `History`, so the prompt is empty and the endpoint
answers `400 images.prompt_required` — which made the commit test fail and,
worse, made the zero-`data[]` test pass on the **right status for the wrong
reason**. Pass a user message, and assert the exact terminal code rather than
the status alone. **Two:** `relayImages` collides with an existing
`(s *Server) relayImages` and will not compile; name the new functions
something else. **Three:** the header list below omits the **server-override
pair**. The images path runs the same `inferencePreflight`, so
`applyServerOverride` reads those headers there too, and leaving them off
silently ignores an image thread's server override — send them through one
shared helper with the chat hop.)*

- [ ] **Step 2: Run to verify failure**, then build `chat_runs_images.go` with:
  - `buildImagesBody(prep PrepareRunResult) ([]byte, error)` — `{model, prompt, response_format:"b64_json"}`, where the prompt is the **last user message's text**, extracted the way `extractText` (`service_chats.go:465-484`) already does for titles (it handles both the string and `[{type:"text"}]` shapes).
  - `relayImages(ctx, ...)` — POST to `s.selfBaseURL+"/v1/images/generations"` with the same header set `executeRun` builds today (`:492-496`: content type, CSRF, the loopback pair, the session header, and the run-as header when set), read the whole body, decode `imagesResponse`, build the parts.
  - `imagePartsFrom(resp imagesResponse) (json.RawMessage, error)` — one part per item; a zero-item response is an **error**, not an empty success (a counted zero on a 2xx is already logged at Error by the endpoint's own counter).

- [ ] **Step 3: Branch `executeRun`** on `run.kind == "image"` before
  `buildChatCompletionsBody`, and have the images path call `finishRun` with
  the committed parts. Leave the SSE path untouched.

- [ ] **Step 4: Gates and commit** (same command block as Task 6).

---

### Task 8: A failed commit is loud, and a non-200 keeps its body

Two silent-loss defects and the four unmapped error codes, together because
they are one user-visible outcome: the chat says what actually happened.

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs.go` — `finishRun` (`:657-679`), `executeRun`'s non-200 branch (`:528-531`)
- Modify: `gateway/frontend/src/components/shared/format.ts` — `errorLabelByCode`
- Modify: `gateway/frontend/src/i18n.ts` — the matching `errorX` keys, both locales
- Test: `gateway/backend/internal/gateway/chat_runs_test.go`, `gateway/frontend/src/components/shared/format.test.ts`, `gateway/frontend/src/i18n.test.ts`

**Interfaces:**
- Produces: a terminal run whose `error` carries a mapped code when the commit
  failed; and `errorLabelByCode` entries for `portal.chat_too_large`,
  `portal.chat_run_active`, `portal.chat_run_limit`,
  `mapping.capability_reserved`, the timeout code from Task 6
  (`gateway.chat_run_timeout`), the two codes Task 7 introduced
  (`gateway.chat_run_no_image`, `gateway.chat_run_image_format_unknown`), and
  the four `images.*` codes Task 7 made reachable from the chat
  (`images.prompt_required`, `images.stream_unsupported`,
  `images.response_format_unsupported`, `images.upstream_error`).

**This task's priority rose during execution.** Task 7 confirmed that a
too-large image turn is a **silent success today**: `ErrChatTooLarge` is only
logged, and the run still finishes `completed` with no turn at all. An image
turn is the first payload in this system that routinely approaches the 4 MiB
cap, so the sharpest open defect in the feature is the one this task closes.
Treat the loud-commit-failure half as the primary deliverable, not the error
map.

**The three constraints on an `errorLabelByCode` entry** are enforced by
`shared/format.test.ts`: the code must match
`/^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$/` (`:111-120`), the label must match
`/^error[A-Z]/` (`:122-128`), and the file pins literal wire codes (`:199-221`).

**A new wire error code needs an HTTP-level test.**
`docs/architecture/cross-cutting/development-and-quality.md:508-517` records why:
deleting an `errRow` once made a 409 answer as `500 application.request_failed`
and the whole backend package suite stayed green.

- [ ] **Step 1: Write the failing tests** — a Go test that a run whose commit
  fails with `ErrChatTooLarge` ends `error` with the mapped code rather than
  `completed`; a Go test that a non-200 from the loopback surfaces the
  upstream body's `error.code` instead of `"upstream status ..."`; and the
  three frontend tests (two in `format.test.ts` following `:199-221`, one
  i18n key-presence block).

- [ ] **Step 2: Make the commit failure terminal**

`finishRun`'s swallow (`:668-679`) keeps its `log.Printf` — the log line is
still wanted — but the run's terminal status becomes `error` with the mapped
code when the commit fails, so the browser is told. Read the existing comment
first: it says control flow is deliberately unchanged, and that reasoning is
what this step overturns, so the comment must be rewritten rather than left
contradicting the code.

- [ ] **Step 3: Keep the non-200 body**

`executeRun:528-531` reads nothing. Read it (bounded — reuse
`s.captureMaxBytes`, as `relayImagesUpstreamError` does at
`images_handler.go:480`), pull `error.code` out of the standard error envelope,
and fall back to the status line when the body is not that shape.

- [ ] **Step 4: Map the codes** in `errorLabelByCode` and add the `errorX`
  keys to **both** i18n locales.

- [ ] **Step 5: Gates and commit** — Go gates plus
  `cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test`.

---

### Task 9: `PUT /chats/{id}` refuses while a run is active

**Files:**
- Modify: `gateway/backend/internal/gateway/portal_chat_endpoints.go` — the `http.MethodPut` case (`:104-119`)
- Test: `gateway/backend/internal/gateway/chats_test.go`

**Interfaces:**
- Produces: `PUT /api/portal/chats/{id}` returns `409 portal.chat_run_active`
  while `s.ChatRuns` holds a run for that `(user, chat)`. Task 8 already
  mapped that code.

The `DELETE` case in the very same switch (`:120-123`) consults the registry
already. The save path does not, and the client-side guard is per-tab
(`useChatPersistence.ts:164`, over a `runsRef` populated only by this tab), so
a second already-open view PUTs its stale document over the transcript
mid-run. The exposure window is the run duration, which this feature multiplies
by roughly fifty, and what it overwrites is a just-committed image.

Guard with the registry the way `DELETE` does, and mind that `s.ChatRuns` is
**not** nil-defaulted (`server.go:813` assigns it bare, unlike `AppHealth`), so
a nil check is required rather than optional.

- [ ] **Step 1: Write the failing test** — using `newChatTestServer(t)`,
  `loginCookie` and `chatRequest` (`chats_test.go:24-66`): start a run for the
  chat via the registry, then `chatRequest(... http.MethodPut ...)` and assert
  409 with `portal.chat_run_active`; and assert a PUT with **no** active run
  still returns 200, so the guard is not a blanket refusal.

- [ ] **Step 2: Add the guard**, **Step 3: Run**, **Step 4: Gates and commit.**

---

### Task 10: The prune stops deleting structured content

Spec §7.1. Smallest task in the plan and the one without which Task 7's work
is invisible.

**Files:**
- Modify: `gateway/frontend/src/components/chat/useChatRuns.ts:177-179`
- Test: `gateway/frontend/src/components/chat/ChatStore.test.tsx`

**Interfaces:** none. Behaviour only.

**Fix the `typeof` branch, not the `.length` one.** There are two
empty-assistant prunes and only this one is wrong.
`pruneEmptyAssistantTail` (`chatDoc.ts:264-267`) tests
`last.content.length === 0` under a comment saying `.length` "covers both the
string and (defensively) the array shape" — correct, because a one-part image
array has length 1. Do not "align" them; make this one agree with that one.

- [ ] **Step 1: Write the failing test**

In `ChatStore.test.tsx`, using `renderProvider` / `waitForReady` / the
`FakeEventSource` install and the refetch-on-done pattern at `:956-1007` as
the model for driving a run to terminal:

```tsx
    // The prune exists to drop a bubble a run never wrote anything into. An
    // image turn's content is an ARRAY, and `typeof content === 'string'` is
    // false for it, so the old predicate called a one-image turn empty and
    // deleted the bubble holding it -- after a successful generation AND a
    // successful persist.
    it('keeps an assistant bubble whose content is a structured image array', async () => {
      renderProvider();
      await waitForReady();
      // Drive a run to terminal whose committed content is
      //   [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AA==' } }]
      // via the FakeEventSource + makeChatApi committed transcript, exactly as
      // the refetch-on-done test at :956-1007 does, then:
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('still prunes an assistant bubble with an empty string and no reasoning', async () => {
      // The regression guard: the prune must keep doing its actual job.
    });
```

> Both bodies above are outlines. They are the only outlines left in this plan
> and they are here because driving a run to terminal in `ChatStore.test.tsx`
> requires the `FakeEventSource` + `makeChatApi` choreography from `:956-1007`,
> which is 50 lines of that file's own idiom and must be read and adapted
> rather than transcribed. What each test must assert is stated exactly.

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/frontend && npx vitest run src/components/chat/ChatStore.test.tsx -t 'structured image array'
```

- [ ] **Step 3: Fix the predicate**

```ts
        // "Empty" means the run wrote nothing into this bubble. Length is the
        // right test for BOTH shapes -- a string's characters and a content
        // array's parts -- and `typeof content === 'string' ? … : true` was
        // not: it called every structured content empty, so an image turn's
        // bubble was deleted the moment it arrived. pruneEmptyAssistantTail
        // (chatDoc.ts) already tests length, and this now agrees with it.
        const empty = last.content.length === 0 && !last.reasoning;
```

- [ ] **Step 4: Run the whole suite** — this predicate is on the shared
  terminal path for every text run, so a component-scoped run is not enough:

```bash
cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test
```

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components/chat/useChatRuns.ts gateway/frontend/src/components/chat/ChatStore.test.tsx
git commit -m "fix: Stop the terminal prune from deleting a structured assistant turn

finishRun dropped an assistant bubble it considered empty, and its predicate
read (typeof content === 'string' ? content.length === 0 : true) -- so ANY
non-string content counted as empty. An image turn's content is an array of
parts, so the bubble holding a successfully generated AND successfully
persisted image was deleted the instant it arrived, surviving only through the
best-effort canonical refetch.

The sibling prune in chatDoc.ts already tested .length, under a comment saying
that covers both the string and the array shape. This one now agrees with it.
Unobservable before now, because assistants only ever produced text."
```

---

### Task 11: Render a generated image

**Files:**
- Create: `gateway/frontend/src/components/shared/downloadBinary.ts`
- Create: `gateway/frontend/src/components/ImageTurn.tsx`
- Modify: `gateway/frontend/src/components/ChatMessage.tsx` — hoist the `contentImages` call above the assistant branch's return (`:89`), and the user-branch render block (`:354-372`) stays as it is
- Modify: `gateway/frontend/src/i18n.ts` — two keys, both locales
- Test: `gateway/frontend/src/components/ChatMessage.test.tsx`, `gateway/frontend/src/i18n.test.ts`

**Interfaces:**
- Consumes: Task 7's committed content shape —
  `[{"type":"text","text":<revised_prompt>}?, {"type":"image_url","image_url":{"url":"data:<mime>;base64,<b64>"}}...]`
- Produces: `downloadBinary(filename: string, dataUrl: string): void` in
  `components/shared/downloadBinary.ts`.

**`revised_prompt` needs no new part type.** Task 7 emits it as an ordinary
`{type:'text'}` part, so `contentText` (`ChatMessage.tsx:18-21`) already renders
it and `buildAPIHistory` already carries it forward. Do not invent a field on
the `image_url` part.

**Four defects, one screen:**

1. `contentImages` (`:24-32`) is called at `:298`; the assistant branch returns
   at `:89`, so it never runs for an assistant message.
2. `downloadText` (`shared/download.ts`) wraps a **string** in a Blob — its own
   doc says it was written for PEM/text — so a data URL saves as a text file
   containing the data URL. **Leave `downloadText` untouched**: its
   detached-anchor click and its immediate `revokeObjectURL` are load-bearing
   for the certificate panels and there is no `download.test.ts` to catch a
   regression.
3. `:361` hardcodes `alt={t.chatAttachedImage}` ("Angehängtes Bild"), false for
   a generated image.
4. `:362-366` renders `72×72` with `objectFit:'cover'` — right for an upload
   thumbnail, wrong for the artifact the user asked for.

**The eight existing tests that select images by `altText(t.chatAttachedImage)`**
(`Chat.test.tsx:410`, `:421`, `:444`, `:461`, `:588`, `:599`, `:610`, `:615`)
cover **uploaded** images and must keep passing untouched. The alt change
applies to the **assistant** branch only. If one of them starts matching two
elements, the change leaked into the user branch.

**Keep the memo intact.** `ChatMessage`'s `React.memo` (`:399-403`) is a shallow
prop compare relying on referentially stable per-message callbacks. Do not add
an `onDownload` prop built as an inline arrow in the parent's render — it would
defeat the memo for every message in the thread. The handler lives inside
`ImageTurn`, where the data URL already is.

- [ ] **Step 1: Write the failing tests**

`ChatMessage` is tested standalone with literal props and no provider
(`ChatMessage.test.tsx:26-41`), which is the cheapest place to pin all of this.
Append inside the existing `for (const locale of ['de','en'])` loop:

```tsx
    it('renders a generated assistant image at full size with the prompt as its alt text', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat on a bicycle"
          content={[
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
          ]}
        />,
      );
      const images = screen.getAllByRole('img');
      expect(images).toHaveLength(1);
      expect(images[0]).toHaveAttribute('src', 'data:image/png;base64,AAAA');
      // The accessible name is the prompt, not "Angehängtes Bild": the alt text
      // of a generated image is what it was asked to be.
      expect(images[0]).toHaveAttribute('alt', 'a cat on a bicycle');
      // NOT the 72x72 objectFit:cover upload thumbnail -- this is the artifact.
      expect(images[0]).not.toHaveAttribute('width', '72');
    });

    it('renders one image and one download control per data item', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="two cats"
          content={[
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,BBBB' } },
          ]}
        />,
      );
      expect(screen.getAllByRole('img')).toHaveLength(2);
      // data[] is plural by design -- the endpoint's own counter takes the
      // billed quantity from the response because a partial failure makes n and
      // data[] differ.
      expect(screen.getAllByRole('button', { name: t.chatDownloadImage })).toHaveLength(2);
    });

    it('renders a revised prompt as ordinary text beside the image', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[
            { type: 'text', text: 'a photorealistic cat, studio lighting' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
          ]}
        />,
      );
      // The only substantive news this endpoint ever reports about a
      // generation, and it needs no new part type to carry it.
      expect(screen.getByText('a photorealistic cat, studio lighting')).toBeInTheDocument();
      expect(screen.getAllByRole('img')).toHaveLength(1);
    });

    it('still renders a plain text assistant answer unchanged', () => {
      render(<ChatMessage t={t} role="assistant" content={'**bold** answer'} />);
      expect(screen.getByText('bold').tagName).toBe('STRONG');
      expect(screen.queryAllByRole('img')).toHaveLength(0);
    });
```

And a download test. jsdom implements neither `URL.createObjectURL` nor
`revokeObjectURL`; copy the stub from `EdgeCertificatePanel.test.tsx:96-104`
into a `beforeEach` in this file:

```tsx
    it('downloads an image as binary, not as a text file containing the data URL', () => {
      const blobs: Blob[] = [];
      const createObjectURL = vi.fn((b: Blob) => {
        blobs.push(b);
        return 'blob:stub';
      });
      vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }]}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      expect(createObjectURL).toHaveBeenCalledTimes(1);
      // The saved Blob must be the DECODED bytes with the reported media type.
      // downloadText would have produced a text/plain Blob whose content is the
      // literal "data:image/png;base64,AAAA" string -- a text file, not an image.
      expect(blobs[0].type).toBe('image/png');
      expect(blobs[0].size).toBe(3); // "AAAA" base64-decodes to 3 bytes
    });
```

In `i18n.test.ts`, a per-feature key block in the established shape:

```ts
  describe('chat generated-image i18n keys', () => {
    for (const locale of ['de', 'en'] as readonly Locale[]) {
      it(`has the generated-image keys in ${locale}`, () => {
        const t = messages[locale];
        expect(t.chatDownloadImage).toBeTruthy();
        expect(t.chatGeneratedImage).toBeTruthy();
        // The generated-image alt must not reuse the ATTACHED-image string.
        expect(t.chatGeneratedImage).not.toBe(t.chatAttachedImage);
      });
    }
  });
```

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/frontend && npx vitest run src/components/ChatMessage.test.tsx -t 'generated'
```

Expected: FAIL — `promptText` is not a prop of `ChatMessage`, and no image is
rendered for an assistant message.

- [ ] **Step 3: Write `downloadBinary`**

```ts
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Saving a data: URL as a real file. The sibling downloadText wraps a STRING in
// a Blob -- it was written for PEM/text blobs and its own doc says so -- so
// handing it a data URL saves a text file whose contents are the data URL
// itself. That is not a smaller problem than a missing button: the user thinks
// they saved their image.
//
// The media type is taken from the data URL's own prefix, which Task 7 fills
// from the upstream response's output_format. Nothing here assumes PNG.
export function downloadBinary(filename: string, dataUrl: string) {
  const match = /^data:([^;,]+);base64,(.*)$/s.exec(dataUrl);
  if (!match) return;
  const [, mime, b64] = match;
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  const url = URL.createObjectURL(new Blob([bytes], { type: mime }));
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
  } finally {
    URL.revokeObjectURL(url);
  }
}

// extensionFor maps a media type to a file extension for the download name. A
// bare fallback rather than a lookup table: the only producer today is
// sd-server via output_format, and inventing entries for types nothing emits
// would be dead code.
export function extensionFor(dataUrl: string): string {
  const match = /^data:image\/([a-z0-9+.-]+);base64,/i.exec(dataUrl);
  return match ? match[1].replace('jpeg', 'jpg') : 'bin';
}
```

- [ ] **Step 4: Write `ImageTurn.tsx`**

A presentational component taking the image URLs and the prompt, rendering each
image at natural size capped to the bubble, each with its own download button.
It owns the `downloadBinary` call so no unstable callback prop reaches
`ChatMessage` and defeats its memo. Style with the existing CSS variables
(`--line`, `--surface`) and MUI `sx`, matching the user-branch block's border
and radius but without the `72×72` / `objectFit:'cover'` crop.

- [ ] **Step 5: Hoist the extractor and branch the assistant render**

In `ChatMessage.tsx`, move the `const images = contentImages(content);`
computation from `:298` to **above** the `if (role === 'assistant')` at `:78`
so both branches can read it. Add a `promptText?: string` prop, passed by
`Chat.tsx` from the preceding user turn's text, and render `ImageTurn` inside
the assistant branch when `images.length > 0`. Leave the user-branch block at
`:354-372` exactly as it is — that is what keeps the eight `chatAttachedImage`
tests green.

- [ ] **Step 6: Add the two i18n keys to BOTH locales**

```ts
  chatDownloadImage: 'Bild herunterladen',
  chatGeneratedImage: 'Erzeugtes Bild',
```

```ts
  chatDownloadImage: 'Download image',
  chatGeneratedImage: 'Generated image',
```

`chatGeneratedImage` is the alt-text fallback for a generated image whose
prompt is unavailable (an old transcript, or a regenerated turn whose preceding
user message was edited away). The prompt is preferred when present.

- [ ] **Step 7: Run the tests, then the full frontend gate block**

```bash
cd gateway/frontend && npx vitest run src/components/ChatMessage.test.tsx src/components/Chat.test.tsx src/i18n.test.ts
```

`Chat.test.tsx` is in that list on purpose: it holds the eight uploaded-image
tests that must not change.

```bash
cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test
```

- [ ] **Step 8: Commit**

```bash
git add gateway/frontend/src/components/shared/downloadBinary.ts gateway/frontend/src/components/ImageTurn.tsx gateway/frontend/src/components/ChatMessage.tsx gateway/frontend/src/components/ChatMessage.test.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/i18n.test.ts
git commit -m "feat: Render a generated image as the artifact it is

Four defects on one screen, none of them observable before assistants could
produce images.

contentImages was called after the assistant branch had already returned, so it
never ran for an assistant message -- the extractor existed and was
unreachable. The call is hoisted above the branch.

The alt text was the hardcoded 'attached image' string, which is false for a
generated one; the accessible name is now the prompt, falling back to a
generated-image label when the prompt is gone. The size was the 72x72
objectFit:cover upload thumbnail, which cropped the thing the user asked for;
a generated image renders at its natural size, capped to the bubble.

The download path could not download an image at all. downloadText wraps a
STRING in a Blob -- it was written for PEM text and says so -- so a data URL
saved as a text file containing the data URL. downloadBinary decodes the base64
and uses the media type the data URL carries, which comes from the upstream
response's output_format rather than an assumed image/png. downloadText is left
untouched: its detached-anchor click and immediate revoke are load-bearing for
the certificate panels and have no test of their own.

And every data[] item is rendered with its own download control, because the
response is plural by design. A revised_prompt needs no new part type: it
arrives as an ordinary text part and the existing text renderer shows it.

The download handler lives inside ImageTurn rather than arriving as a prop, so
ChatMessage's shallow-compare memo is not defeated for every message in the
thread. ImageTurn sits in components/ rather than components/chat/ because
arch.test.ts fails the suite for a cross-boundary import of that directory."
```

### Task 12: Edit and regenerate stop refusing an image thread

**Files:**
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx:715-723`
- Test: `gateway/frontend/src/components/chat/ChatStore.test.tsx`

**Interfaces:** consumes Task 5's pinned kind as it reaches the store. No new
exports.

**The fix is kind-aware, not role-aware — this overrides spec §7.2's implied
fix.** §7.2 describes the defect correctly (`historyHasImage`,
`chatDoc.ts:273-277`, is role-blind and matches the assistant's own generated
images), but making it role-aware would be wrong:

- For a **text** thread the guard is *correct exactly as it stands*, including
  its role-blindness. `buildAPIHistory` passes every message's content through
  verbatim, so an assistant-generated image in a replayed history really does
  become a vision input on the next chat completion. Refusing that on a
  non-vision model is the right behaviour, and its existing comment
  (`:715-719`) explains why it checks `history` rather than
  `messagesRef.current`.
- For an **image** thread the guard is simply irrelevant: Task 7's request body
  is `{model, prompt, response_format}` and carries **no history at all**, so
  there is no vision input to refuse.

So the guard becomes conditional on the thread's kind. That is also what stops
the false `chatImageModelUnsupported` — "Dieses Modell unterstützt keine
Bilder." — from appearing on a model whose only purpose is images.

**`chatImageModelUnsupported` has four call sites and five tests.** This task
touches only the one at `:721`. The others (the attach tooltip `Chat.tsx:384`,
the clear-attachments toast `ChatStore.tsx:642`, and the `send()` guard
`:838`) belong to Task 13.

**The existing test at `ChatStore.test.tsx:1484-1513` must keep passing
unchanged.** It regenerates on a non-vision **text** model with an uploaded
image in the replayed history, and blocking that is exactly the behaviour being
preserved. Add the new case beside it; do not edit it.

- [ ] **Step 1: Write the failing test**

Inside the same `describe` as the existing guard tests, reusing that block's
`imageContent` fixture (`:1447-1450`) and the `renderProvider` / `waitForReady`
/ `makeChatApi` harness:

```tsx
    // An image thread's request body carries no history at all, so the vision
    // guard has nothing to protect and must not fire. Before this change it
    // did, and it told the user that a model whose only purpose is images
    // "does not support images" -- from the SECOND image turn onward, because
    // that is when the replayed history first contains one.
    it('regenerates in an image thread whose history contains generated images', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: models[0].id, kind: 'image' },
            messages: [
              { id: 'u1', role: 'user', content: 'a cat' },
              { id: 'a1', role: 'assistant', content: imageContent, status: 'complete' },
              { id: 'u2', role: 'user', content: 'a dog' },
              { id: 'a2', role: 'assistant', content: imageContent, status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

      // The run starts and no refusal toast appears.
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
    });
```

Note the fixture pins `kind: 'image'` in the persisted settings — that is how
the store learns the thread's kind, and it is the same key Task 5 persists.

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/frontend && npx vitest run src/components/chat/ChatStore.test.tsx -t 'image thread whose history contains generated images'
```

Expected: FAIL — `startChatRun` was never called and the refusal toast is
present, because the role-blind guard matched the assistant's own images.

- [ ] **Step 3: Make the guard conditional on the kind**

```tsx
      // Same guard for image support: the REPLAYED history (what is actually
      // sent as edited_history below) must not carry an image the model can't
      // process. Checking `history` (not the full messagesRef.current) matters
      // when regenerating/editing an EARLIER turn: truncation drops everything
      // after it, so a later turn's image is never resent and must not block.
      //
      // It is deliberately role-BLIND for a text thread: buildAPIHistory
      // forwards every message's content verbatim, so an assistant's own
      // generated image in a replayed history becomes a vision input just as a
      // user's upload does, and refusing it on a non-vision model is correct.
      //
      // An IMAGE thread is exempt because its request body carries no history
      // at all -- just {model, prompt, response_format} -- so there is no
      // vision input to refuse. Without this exemption an image thread refused
      // its own regenerate from the second turn onward, telling the user that a
      // model whose only purpose is images does not support images.
      if (
        chatKindRef.current !== 'image' &&
        !modelVisionCapableRef.current &&
        historyHasImage(history)
      ) {
        showErrorRef.current(tRef.current.chatImageModelUnsupported);
        return;
      }
```

`chatKindRef` is the ref mirroring the thread's pinned kind. If Task 13 has not
landed yet, add the ref here (beside `modelVisionCapableRef`, following the
refs block's own comment at `:311-312` about why the stable callbacks read
refs) and let Task 13 reuse it — whichever task runs first owns it.

- [ ] **Step 4: Run both tests**

```bash
cd gateway/frontend && npx vitest run src/components/chat/ChatStore.test.tsx -t 'regenerat'
```

Expected: PASS, including `:1484`'s unchanged text-thread refusal and
`:1452`'s earlier-turn case.

- [ ] **Step 5: Full gates and commit**

```bash
cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test
```

```bash
git add gateway/frontend/src/components/chat/ChatStore.tsx gateway/frontend/src/components/chat/ChatStore.test.tsx
git commit -m "fix: Stop an image thread from refusing its own regenerate

historyHasImage is role-blind, so the vision guard on edit/regenerate matched
the assistant's OWN generated images. From the second image turn onward --
the first point at which a replayed history contains one -- regenerating was
refused with 'this model does not support images', on a model whose only
purpose is images.

The fix is kind-aware rather than role-aware, and that distinction matters. For
a text thread the guard is correct exactly as it stands, role-blindness
included: buildAPIHistory forwards content verbatim, so an assistant's
generated image in a replayed history becomes a vision input just as a user's
upload does, and refusing it on a non-vision model is right. An image thread is
exempt because its request body carries no history at all -- only the model,
the prompt and the response format -- so there is nothing to refuse.

The existing text-thread refusal test is untouched, because the behaviour it
pins is the behaviour being kept."
```

### Task 13: The composer before Send

Spec §3.6's pre-send half — the relabelled prompt field, the capacity line and
the refusal — plus spec §5's image-only affordances and §7.9's dead keepalive.

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` — export the cap; `ChatListResponse` (`:68`) carries it
- Modify: `gateway/frontend/src/api/models.ts` — `ModelOption` (`:402`) gains `image?: boolean` beside `vision?: boolean` (`:425`)
- Modify: `gateway/frontend/src/api/chat.ts` — `ChatSettings` (`:19-31`) **and** the duplicated inline settings shape in `StartChatRunBody` (`:49-57`)
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx` — `modelImageCapable` beside `modelVisionCapable` (`:308-309`), `chatKind` + its ref, `activateChat` (`:451-499`), `send()` (`:824-848`), the clear-attachments effect (`:639-645`)
- Modify: `gateway/frontend/src/components/Chat.tsx` — the prompt field (`:309-335`), the capacity line, the attach gate (`:391`) and its tooltip (`:384`)
- Modify: `gateway/frontend/src/components/chat/useChatPersistence.ts` — the `state` type (`:76-86`), its destructure (`:108-118`), the debounced effect's dep array (`:202-213`), and the keepalive guard (`:228`)
- Modify: `gateway/frontend/src/i18n.ts`
- Test: `Chat.test.tsx`, `ChatStore.test.tsx`, `ChatSidebar.test.tsx`, `i18n.test.ts`

**Interfaces:**
- Consumes Task 1's `ModelDTO.Image` and Task 5's persisted `kind`.
- Produces `ChatStore.modelImageCapable: boolean`, `ChatStore.chatKind: string`,
  and a served `max_content_bytes`.

**Serve the cap on the chat list; do not duplicate it.**
`maxChatContentBytes` is package-private to `portal` (`service_chats.go:43`)
and no DTO carries it. A second `4 << 20` in TypeScript would drift from the Go
constant with nothing to catch it, and the failure mode is a capacity line that
confidently states the wrong number — the exact defect this composer exists to
avoid. `handlePortalChatSettings` is **not** the place: it is PUT-only and
about capture flags, and its own comment says there is no GET there. Put it on
`ChatListResponse` (`{Data []ChatSummaryDTO}`, `:68`), which the chat view
already fetches:

```go
// ChatListResponse is the chat list plus the limits a client needs to stay
// inside it. MaxContentBytes is maxChatContentBytes: the client cannot
// otherwise know it (it is package-private and on no other DTO), and a client
// that hardcodes its own copy would drift from this one silently.
type ChatListResponse struct {
	Data           []ChatSummaryDTO `json:"data"`
	MaxContentBytes int             `json:"max_content_bytes"`
}
```

**Five things that fail silently if missed:**

1. **A new persisted setting needs three lockstep edits** in
   `useChatPersistence.ts`: the `state` parameter type (`:76-86`), its
   destructure (`:108-118`), and the debounced effect's **explicit** dep array
   (`:202-213`), which sits under
   `// eslint-disable-next-line react-hooks/exhaustive-deps`. Miss the dep and
   the setting never triggers a save, with no warning.
2. **`activateChat` destroys an unhandled setting.** It seeds React state from
   `normalizeDoc`'s output (`:451-499`); a setting not loaded there reverts to
   its default and the next debounced save writes that default over the stored
   value.
3. **`ChatSidebar.test.tsx`'s `makeStore` (`:13-65`) enumerates all 47
   `ChatStore` fields** and is typed `ChatStore` with no cast, so a new
   required field is a **tsc** error there — and `npm test` does not
   type-check. Only `npm run build` catches it.
4. **`api/chat.ts` declares the settings shape twice** (`:19-31` and inline at
   `:49-57`). Add `kind` in both.
5. **The synthetic injected model option carries no capability fields**
   (`ChatStore.tsx:247-258` injects `{id, display_name, flavors,
   loading_on_count}` for a picked model absent from the catalogue), so it
   derives `image === false`. That is fine and deliberate: the **pinned kind**
   is the thread's truth, and `modelImageCapable` only gates what the composer
   newly offers. Do not derive the thread's behaviour from the model flag.

**`ModelOption.image` must be OPTIONAL.** `vision?` is optional and
`ChatStore.test.tsx`'s module-level model fixture omits it entirely — that
omission is what makes the file's non-vision guard tests work
(`:1444-1446` says so). A required field would break them.

**`components/chat/**` is an enforced import boundary**
(`arch.test.ts:136-139`, `:256-270`): only `ChatStore.tsx` and
`ChatSidebar.tsx` may be imported from outside. A capacity helper used by
`Chat.tsx` belongs in `components/shared/`.

**Also here: the pagehide keepalive, dead for every image thread** (§7.9).
`useChatPersistence.ts:228` is
`if (JSON.stringify(payload).length > 60000) return;` — a silent return, and a
single inline base64 image is far past it, so a last-moment change is lost on
navigate-away in exactly the threads this feature creates. Do **not** raise the
limit: it exists for a real keepalive payload ceiling. Make the skip
observable instead — the debounced save has no such limit and remains the path
that actually persists an image turn — and say in the commit body which of the
two you chose and why.

- [ ] **Step 1: Write the failing tests**

In `Chat.test.tsx`, inside the locale loop, following the drive-a-send pattern
at `:333-345` and using `renderChat` / `waitForChatReady` / `pickOption`:

```tsx
    it('labels the prompt field as an image prompt for an image-capable model', async () => {
      renderChat(undefined, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
    });

    it('sends the image kind in the started run settings', async () => {
      renderChat(undefined, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat on a bicycle' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.kind).toBe('image');
    });

    it('disables the attach button for an image model that is not vision-capable', async () => {
      renderChat(undefined, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      // An image GENERATOR has no reason to accept an image INPUT, and the two
      // are different capabilities. The tooltip must say which one it means.
      expect(screen.getByRole('button', { name: t.chatAttachImage })).toBeDisabled();
    });

    it('shows the remaining capacity for an image thread', async () => {
      renderChat(undefined, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      // The one number in this feature that is exact and known BEFORE the user
      // commits to a multi-minute wait.
      expect(screen.getByTestId('chat-capacity')).toBeInTheDocument();
    });
```

`imageModels` is a new describe-local fixture beside the existing
`nonVisionModels` / `mixedModels` (`Chat.test.tsx:532-566` shows their shape) —
the same objects with `image: true` and no `vision`.

In `ChatStore.test.tsx`, the refusal and the persistence round-trip:

```tsx
    it('refuses to send when the transcript has no room for another image', async () => {
      // Seed a chat whose document is already within one image of
      // max_content_bytes, then attempt a send and assert startChatRun was
      // NEVER called and the capacity error was shown. Spending minutes of
      // upstream CPU on an artifact we can already prove we cannot store is the
      // failure this refusal exists to prevent.
      renderProvider();
      await waitForReady();
      // ... seed via makeChatApi with a large content blob, then:
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(await screen.findByText(t.chatCapacityExhausted)).toBeTruthy();
    });

    it('persists the pinned kind across a save and a reload', async () => {
      // The three lockstep edits in useChatPersistence are what make this pass;
      // miss the dep array and the setting simply never triggers a save.
      renderProvider();
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled());
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { kind?: string } };
      };
      expect(saved.content.settings.kind).toBe('image');
    });
```

The first body's seeding step is left as a comment because the size of the
blob depends on the served `max_content_bytes` this task introduces; everything
it must assert is written out.

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/frontend && npx vitest run src/components/Chat.test.tsx -t 'image prompt'
```

- [ ] **Step 3: Backend — export and serve the cap**

Rename `maxChatContentBytes` to an exported `MaxChatContentBytes` (or keep the
private const and add an exported accessor — either is fine, one of them), add
the field to `ChatListResponse`, and populate it in `ListChats`. Add a Go test
that the listing's JSON carries a non-zero `max_content_bytes`, because nothing
else would notice it silently becoming zero.

- [ ] **Step 4: Frontend — the flag, the kind, and the composer**

`modelImageCapable` mirrors `:308-309` exactly:

```tsx
  // Whether the effective model GENERATES images (drives the image-thread
  // composer). Independent of modelVisionCapable, which is whether it ACCEPTS
  // them; the backend AND-aggregates both into ModelOption.
  const modelImageCapable =
    chatModels.find((option) => option.id === effectiveModel)?.image ?? false;
```

Then: `chatKind` state seeded in `activateChat` from the document's
`settings.kind`; the prompt field's label switches to
`t.chatImagePromptLabel` when the thread is an image thread; the capacity line
renders beside it; `send()` gains the capacity refusal **before** it builds the
user message; the attach button's condition becomes
`disabled={c.streaming || !c.modelVisionCapable || c.modelImageCapable}` and
its tooltip says which capability it means.

- [ ] **Step 4b: Add the composer's new i18n keys to BOTH locales**

The Step 1 tests reference these; without them `getByLabelText(undefined)`
cannot match and `npm run build` fails on the `PortalMessages` parity type.

```ts
  chatImagePromptLabel: 'Bildprompt',
  chatImageOnlyHint: 'Dieses Modell erzeugt Bilder. Dieser Thread nimmt nur Bildprompts an.',
  chatCapacityRemaining: 'Platz für etwa noch {count} Bild(er) in diesem Chat.',
  chatCapacityExhausted:
    'Dieser Chat hat keinen Platz mehr für ein weiteres Bild. Lade die vorhandenen Bilder herunter und beginne einen neuen Chat.',
```

```ts
  chatImagePromptLabel: 'Image prompt',
  chatImageOnlyHint: 'This model generates images. This thread accepts image prompts only.',
  chatCapacityRemaining: 'Room for about {count} more image(s) in this chat.',
  chatCapacityExhausted:
    'This chat has no room for another image. Download the images you have and start a new chat.',
```

Check how this file already interpolates a count before writing
`{count}` — grep for an existing parameterised message and follow that
mechanism rather than inventing one.

- [ ] **Step 5: The attach tooltip string, which is currently false**

`chatImageModelUnsupported` reads "Dieses Modell unterstützt keine Bilder." /
"This model does not support images." (`i18n.ts:191`, `:2539`) and gates image
**input**. On an image-generating model without vision the portal would tell
the user that model does not support images. Reword it to name what it gates —
image input — and add a distinct string for the image-model case. Four call
sites share this key (`Chat.tsx:384`, `ChatStore.tsx:642`, `:721`, `:838`) and
five tests reference it; Task 12 owns `:721`.

- [ ] **Step 6: The three lockstep persistence edits, plus the keepalive**

Add `kind` to the `state` type, the destructure, and the dep array. Then
handle `:228` per the decision above.

- [ ] **Step 7: Build FIRST, then the rest of the gates**

`npm run build` is the gate that catches `makeStore`'s missing field and the
i18n parity break; `npm test` catches neither.

```bash
cd gateway/frontend && npm run build && npm run format:check && npm run lint && npm test
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

- [ ] **Step 8: Commit.**

---

### Task 14: The composer during generation

Spec §3.6's during half: liveness, a server-anchored clock, one static
sentence — and no progress of any kind.

**Files:**
- Create: `gateway/frontend/src/components/shared/elapsed.ts`
- Modify: `gateway/frontend/src/components/ActiveRequestsPanel.tsx:13-19` — import the lifted helper instead of declaring it
- Modify: `gateway/frontend/src/api/chat.ts` — `ActiveChatRun` (`:45`) and the 201 response type gain `kind` and `elapsed_ms`
- Modify: `gateway/frontend/src/components/chat/useChatRuns.ts` — carry the kind and the age anchor through `RunState`
- Modify: `gateway/frontend/src/components/Chat.tsx:290` — pass the kind through to `ChatMessage`
- Modify: `gateway/frontend/src/components/ChatMessage.tsx:79-87` — the pending render
- Modify: `gateway/frontend/src/i18n.ts`
- Test: `ChatMessage.test.tsx`, `ChatStore.test.tsx`, `i18n.test.ts`

**Interfaces:**
- Consumes Task 6's `kind` and `elapsed_ms` on the 201 and the snapshot.
- Produces `formatElapsed(ms: number): string` in
  `components/shared/elapsed.ts`.

**Lift `formatElapsed`, do not copy it.** It exists verbatim at
`ActiveRequestsPanel.tsx:13-19` and is not exported:

```ts
// Elapsed since a request started: "Xs" under a minute, otherwise "m:ss".
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 60) return `${total}s`;
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}
```

Move it to the new module, export it, and have `ActiveRequestsPanel` import it.
Two copies of a time formatter drift.

**Why `elapsed_ms` and not `started_at`, diverging from the repo's own
precedent.** The active-requests DTO sends `started_at` as RFC3339
(`active_requests.go:199`, `:318`) and the panel computes the difference against
the local clock. That is fine there: the numbers are seconds and the audience
is an operator diagnostic. Here the clock is the **only** signal during a
multi-minute wait shown to an end user, so a client clock skewed by N would
display an elapsed time wrong by N — a confidently wrong headline number, which
is exactly what this composer exists to avoid. `elapsed_ms` plus a
`performance.now()` anchor compares no two clocks and cannot skew. Record this
divergence in the commit body so the next reader does not "fix" it back.

**Anchor once, interpolate locally.** Take `elapsed_ms` from the snapshot,
record `performance.now()` at that instant, and tick from the difference. The
server value is the anchor; the local clock only fills the gap between
snapshots. This is what makes a reopened tab and the sending tab agree.

**The clock must be `aria-hidden`.** The transcript box is `role="log"` with
`aria-live="polite"` and `aria-relevant="additions text"`
(`Chat.tsx:266-269`), so an unhidden once-per-second clock would announce every
tick and make the thread unusable with a screen reader. The label and the
static sentence are announced; the number is not.

**Replace the fabricated counter, do not feed it.** `ChatMessage.tsx:82-83`
renders `${t.chatReasoningActive} · ${reasoningText.length} ${t.chatCharsUnit}`
whenever `streaming && text.length === 0` — which for an image run is the
**entire** run, so it would read `Denkt ... · 0 Zeichen` from the first
millisecond to the last. Branch on the kind **before** that condition, not
inside it.

- [ ] **Step 1: Write the failing tests**

In `ChatMessage.test.tsx`, standalone props as that file does:

```tsx
    it('shows the image wait label, a clock and the no-news sentence while an image run is pending', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={107_000} />,
      );
      expect(screen.getByText(t.chatImageRunPending)).toBeInTheDocument();
      expect(screen.getByText('1:47')).toBeInTheDocument();
      expect(screen.getByText(t.chatImageNoIntermediateNews)).toBeInTheDocument();
    });

    it('shows NO character counter for a pending image run', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={0} />,
      );
      // The counter is honest for text -- the number is real and it moves. For
      // an image run there is nothing to count, so it must be ABSENT, not zero:
      // proxyNative makes the same distinction one layer down when it gives a
      // buffered relay progress = nil rather than an always-zero struct.
      expect(screen.queryByText(new RegExp(t.chatCharsUnit))).toBeNull();
      expect(screen.queryByText(new RegExp(t.chatReasoningActive))).toBeNull();
    });

    it('hides the clock from assistive technology', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={5_000} />,
      );
      // The transcript is an aria-live log; a ticking number would be announced
      // once a second and make the thread unusable with a screen reader.
      expect(screen.getByText('5s').closest('[aria-hidden="true"]')).not.toBeNull();
    });

    it('leaves the text pending state exactly as it was', () => {
      render(<ChatMessage t={t} role="assistant" content="" streaming reasoning="thinking" />);
      // No kind prop: the existing counter must be untouched, because this is
      // the overwhelmingly common case.
      expect(screen.getByText(new RegExp(t.chatReasoningActive))).toBeInTheDocument();
      expect(screen.getByText(new RegExp(t.chatCharsUnit))).toBeInTheDocument();
    });
```

And the ticker, with the repo's proven fake-timer pattern
(`Activity.active.test.tsx:365-399` — `vi.useFakeTimers()` +
`advanceTimersByTimeAsync`, inside a `try`/`finally` that restores real
timers):

```tsx
    it('ticks the clock once a second from the server anchor', async () => {
      vi.useFakeTimers();
      try {
        render(
          <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={3_000} />,
        );
        expect(screen.getByText('3s')).toBeInTheDocument();
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2_000);
        });
        expect(screen.getByText('5s')).toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    });
```

In `i18n.test.ts`, a key-presence block for `chatImageRunPending` and
`chatImageNoIntermediateNews`.

- [ ] **Step 2: Run to verify failure**

```bash
cd gateway/frontend && npx vitest run src/components/ChatMessage.test.tsx -t 'image run'
```

- [ ] **Step 3: Create `shared/elapsed.ts`** with the exported helper above and
  make `ActiveRequestsPanel` import it. Run that panel's own tests immediately:

```bash
cd gateway/frontend && npx vitest run src/components/Activity.active.test.tsx
```

- [ ] **Step 4: Thread the kind and the anchor**

`api/chat.ts`: `ActiveChatRun` and the start response gain `kind?: string` and
`elapsed_ms?: number`. `useChatRuns`: `RunState` carries the kind and, on each
snapshot, `{ serverElapsedMs, anchor: performance.now() }`. `Chat.tsx:290`
already computes the `streaming` prop per message — pass `kind` and the derived
`elapsedMs` the same way.

- [ ] **Step 5: Branch the pending render**

In `ChatMessage.tsx`, above the existing `showReasoning` computation at `:79`:

```tsx
    // An image run emits ZERO incremental events -- the endpoint refuses
    // `stream` outright -- so there is nothing to count and the character
    // counter below would read "0 Zeichen" for the whole multi-minute wait.
    // Three elements instead, each backed by a value that exists: liveness from
    // the run's server-reported status, an elapsed clock anchored on the
    // server's own measurement, and one sentence stating that no intermediate
    // news is coming.
    if (streaming && kind === 'image' && images.length === 0) {
      return <ImagePendingTurn t={t} elapsedMs={elapsedMs} roleLabel={roleLabel} />;
    }
```

`images.length === 0` keeps the pending state from replacing an arriving image
in the same render. `ImagePendingTurn` lives beside `ImageTurn` in
`components/` (not `components/chat/` — see Task 11).

- [ ] **Step 6: i18n, both locales**

```ts
  chatImageRunPending: 'Warte auf Bild',
  chatImageNoIntermediateNews: 'Keine Zwischenmeldungen – das Bild erscheint fertig oder gar nicht.',
```

```ts
  chatImageRunPending: 'Waiting for image',
  chatImageNoIntermediateNews: 'No intermediate updates – the image arrives finished or not at all.',
```

Deliberately **not** "Bild wird erzeugt": between dispatch and terminal the
request may still be queued for admission or waiting on a model load, during
which "is being generated" is false. "Warte auf Bild" is true for the whole
span and makes the clock's referent unambiguous — the wait, not the work.

- [ ] **Step 7: Full gates and commit**

```bash
cd gateway/frontend && npm run build && npm run format:check && npm run lint && npm test
```

### Task 15: Fold the branch-local docs into the architecture and remove them

**This is the last task and it is mandatory.** `docs/superpowers/**` must never
land in `main` (Global Constraints).

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` — a new ADR
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` — the portal-chat image path
- Modify: `docs/architecture/cross-cutting/security-auth-rbac.md` — the third auth leg
- Modify: `docs/architecture/02-constraints.md` — already touched in Task 3; re-read it in full here
- Delete: `docs/superpowers/specs/2026-09-18-portal-chat-images-design.md`, `docs/superpowers/plans/2026-09-18-portal-chat-images.md`, and the `docs/superpowers/` tree

**The ADR is numbered next after ADR-042** (#71 was ADR-042, #70 was ADR-041).
Read the tail of `09-architecture-decisions.md` and take the next number rather
than assuming 043.

**What must survive the deletion,** because it is decided rather than
historical: the pinned thread kind and why it is a UI constraint rather than an
authorization; the narrow loopback-or-bearer leg and why not the full web
ladder; inline storage in the sealed transcript with the 4 MiB ceiling accepted
and the follow-up issue referenced; the composer's rule that it leads with the
exact number and shows no progress; and the run deadline with its two traps.

- [ ] **Step 1:** Write the ADR and the cross-cutting updates.
- [ ] **Step 2:** `make lint-docs`.
- [ ] **Step 3:** Delete the branch-local tree:

```bash
git rm -r docs/superpowers
```

- [ ] **Step 4:** Verify nothing branch-local reaches the PR diff:

```bash
git diff --name-only main...HEAD | grep -E 'docs/superpowers|implementation-status' && echo "STOP: branch-local files in the diff" || echo "clean"
```

- [ ] **Step 5:** Run every gate one final time, including the authoritative
  Sonar pair:

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
cd ../server-agent && golangci-lint fmt --diff && golangci-lint run && go test ./...
cd ../frontend && npm run format:check && npm run lint && npm run build && npm test
cd ../.. && make lint-docs
```

```bash
make sonar-findings && make sonar-branch-findings
```

`make sonar-gate` is **advisory and exits 0 even on FAIL**; the branch-findings
pair is the authoritative gate. Judge by findings attributed to this branch's
changed lines, not by the legacy total. Note that `.sonar-local/` resolves to
the **main** worktree's copy, so a scan is shared across worktrees.

- [ ] **Step 6:** Commit and open the pull request. **Never merge it.**
