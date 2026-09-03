# Plan material — Area 06: Frontend runtime-spec form + runtime types + i18n

Scope: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (the runtime-spec
create/edit form), `gateway/frontend/src/api/runtime.ts` (RuntimeSpec +
PutRuntimeSpecRequest), `gateway/frontend/src/i18n.ts` (de/en labels).

All paths below are under the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes/`.
Line numbers are from that worktree (origin/main content) at read time.

---

## 0. Canonical names for this area (keep consistent across tasks)

- Enum string union (frontend): `EndpointMode = 'disabled' | 'translate' | 'passthrough'`.
- New RuntimeSpec / PutRuntimeSpecRequest JSON fields: `api_flavors: string[]`,
  `responses_mode: EndpointMode`, `messages_mode: EndpointMode`.
- Shared component (produced by **area 05**, consumed here): `ApiVariantControls`.
- New i18n keys proposed below: `applicationModeDisabled`,
  `applicationModeTranslate`, `applicationModePassthrough`,
  `applicationResponsesMode`, `applicationMessagesMode` (+ reuse of the existing
  `applicationFlavors` legend and the `applicationNative*` strings, see §I18N).

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 `gateway/frontend/src/api/runtime.ts`

`RuntimeSpec` interface — the GET/PUT shape (lines 31-58). No flavors/modes today:

```ts
// runtime.ts:31
export interface RuntimeSpec {
  configured: boolean;
  id?: string;
  mapping_id: string;
  enabled: boolean;
  binary: string;
  args: string[];
  env: Record<string, string>;
  work_dir: string;
  listen_port: number;
  health_path: string;
  health_timeout_seconds: number;
  startup_timeout_seconds: number;
  idle_timeout_seconds: number;
  admission_wait_timeout_seconds: number;
  pinned: boolean;
  admin_state: string;
  vram_locked: boolean;
  set_visible_devices: boolean;
  gpus: RuntimeSpecGPU[];
}
```

`PutRuntimeSpecRequest` (line 66) — a derived `Omit`, so a field added to
`RuntimeSpec` (not in the omit list) is **automatically** part of the PUT body type:

```ts
// runtime.ts:66
export type PutRuntimeSpecRequest = Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>;
```

`putRuntimeSpec` client method (lines 312-320) — passes `body` verbatim, no change needed:

```ts
// runtime.ts:312
putRuntimeSpec: (mappingId: string, body: PutRuntimeSpecRequest) =>
  request<RuntimeSpec>(
    fetcher,
    `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`,
    { method: 'PUT', body },
  ),
```

### 1.2 `gateway/frontend/src/components/RuntimeAdminSection.tsx`

**The `application` prop IS in scope** (this is the key finding for create pre-fill).
It is a required `PortalApplication` prop, destructured at the top of the component
and used throughout (`application.id`, `application.endpoint`,
`application.context_probe_path`):

```ts
// RuntimeAdminSection.tsx:946
export function RuntimeAdminSection({
  t,
  api,
  server,
  application,                 // <-- PortalApplication, in scope for the whole form
  trail = [],
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, ... | 'putRuntimeSpec' | ...>;   // line 967 lists putRuntimeSpec
  server: PortalServer;
  application: PortalApplication;   // line 1002
  ...
}>) {
```

So on spec CREATE we read `application.api_flavors`, `application.responses_mode`,
`application.messages_mode` directly — no new prop, no extra fetch.

**Spec-form state hooks** (lines 2025-2042) — where the three new pieces of form state go:

```ts
// RuntimeAdminSection.tsx:2025
const [gatewayName, setGatewayName] = useState('');
const [appName, setAppName] = useState('');
const [enabled, setEnabled] = useState(true);
const [binary, setBinary] = useState('');
const [argsText, setArgsText] = useState('');
const [envText, setEnvText] = useState('');
const [workDir, setWorkDir] = useState('');
const [listenPort, setListenPort] = useState(0);
const [healthPath, setHealthPath] = useState('/health');
...
const [setVisibleDevices, setSetVisibleDevices] = useState(false);
const [gpuRows, setGpuRows] = useState<GpuRow[]>([]);
```

**`resetSpecFields`** (lines 2101-2118) — called by `openCreate`; resets every non-name field:

```ts
// RuntimeAdminSection.tsx:2101
function resetSpecFields() {
  setEnabled(true);
  setBinary('');
  ...
  setGpuRows([]);
}
```

**`hydrateSpecFields`** (lines 2120-2144) — called by `openEdit`; loads a spec's stored values:

```ts
// RuntimeAdminSection.tsx:2120
function hydrateSpecFields(spec: RuntimeSpec) {
  setEnabled(spec.enabled);
  setBinary(spec.binary);
  ...
  setGpuRows(spec.gpus.map((g) => ({ ... })));
}
```

**`openCreate`** (lines 2146-2151) — note it does NOT snapshot from `application` today:

```ts
// RuntimeAdminSection.tsx:2146
function openCreate() {
  setGatewayName('');
  setAppName('');
  resetSpecFields();
  setSpecMode('create');
}
```

**`openEdit`** (lines 2161-2189) — GETs the spec fresh, then `hydrateSpecFields(spec)`:

```ts
// RuntimeAdminSection.tsx:2161
async function openEdit(mapping: PortalModelMapping) {
  setGatewayName(mapping.gateway_model_name);
  setAppName(mapping.app_model_name);
  setLoadingEditFor(mapping.id);
  const seen = beginSpecRead(mapping.id);
  try {
    const spec = await api.runtimeSpec(mapping.id);
    commitSpecRead(mapping.id, seen, spec);
    ...
    hydrateSpecFields(spec);
    setSpecMode({ kind: 'edit', mapping });
  } ...
}
```

**`buildSpecBody`** (lines 2359-2382) — builds the PUT body as an **explicit literal**
(so the three new fields must be added here too — the `Omit` type change is not enough):

```ts
// RuntimeAdminSection.tsx:2359
function buildSpecBody(args: string[], env: Record<string, string>): PutRuntimeSpecRequest {
  return {
    enabled,
    binary: binary.trim(),
    args,
    env,
    work_dir: workDir.trim(),
    listen_port: listenPort,
    health_path: healthPath.trim(),
    health_timeout_seconds: healthTimeoutSeconds,
    startup_timeout_seconds: startupTimeoutSeconds,
    idle_timeout_seconds: idleTimeoutSeconds,
    admission_wait_timeout_seconds: admissionWaitTimeoutSeconds,
    pinned,
    admin_state: adminState,
    vram_locked: vramLocked,
    set_visible_devices: setVisibleDevices,
    gpus: gpuRows.map((r) => ({ index: r.index, vram_estimate_mb: r.vramEstimateMb, vram_measured_mb: r.vramMeasuredMb })),
  };
}
```

`submitCreate` (2384-2470) and `submitEdit` (2472-2542) both call
`api.putRuntimeSpec(<id>, buildSpecBody(args, parsedEnv.env))` — no change needed
in either beyond what `buildSpecBody` now returns (create at line 2459, edit at 2531).

**`specBodyWithAdminState`** (lines 481-484) — the override/restart PUT builder. It uses
rest-spread, so it carries the three new fields through **automatically**:

```ts
// RuntimeAdminSection.tsx:481
function specBodyWithAdminState(spec: RuntimeSpec, adminState: string): PutRuntimeSpecRequest {
  const { configured, id, mapping_id, ...rest } = spec;
  return { ...rest, admin_state: adminState };
}
```

**The spec form JSX branch** starts at line 3270 (`if (specMode !== 'list')`); the
"config section" heading (line 3344) is the anchor to render `ApiVariantControls`
after (mirroring where ApplicationSection places its native block):

```tsx
// RuntimeAdminSection.tsx:3344
<Typography variant="subtitle2" component="h3" sx={{ mt: 1 }}>
  {t.runtimeSpecConfigSection}
</Typography>
<FormControlLabel
  control={<Checkbox checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />}
  label={t.runtimeSpecEnabled}
/>
```

### 1.3 The block area 05 extracts into `ApiVariantControls` (current source in `ApplicationSection.tsx`)

For reference — the exact controls the shared component reproduces (flavors
CheckboxGroup lines 826-831, native-passthrough CheckboxGroup+note lines 832-849).
These become two flavor checkboxes + two `SelectField` dropdowns per the spec:

```tsx
// ApplicationSection.tsx:826
<CheckboxGroup
  legend={t.applicationFlavors}
  options={applicationFlavorOptions.map((f) => ({ value: f, label: f }))}
  selected={flavors}
  onToggle={(v) => setFlavors((current) => toggleFlavor(current, v))}
/>
<CheckboxGroup
  legend={t.applicationNativeLegend}
  options={[
    { value: 'responses', label: t.applicationNativeResponses },
    { value: 'messages', label: t.applicationNativeMessages },
  ]}
  selected={[...(nativeResponses ? ['responses'] : []), ...(nativeMessages ? ['messages'] : [])]}
  onToggle={(v) => { if (v === 'responses') setNativeResponses((c) => !c); else if (v === 'messages') setNativeMessages((c) => !c); }}
/>
<Typography variant="caption" sx={{ color: 'text.secondary' }}>{t.applicationNativeNote}</Typography>
```

`applicationFlavorOptions = ['openai', 'anthropic']` (ApplicationSection.tsx:47).

### 1.4 Shared building blocks

- `SelectField` (`components/shared/SelectField.tsx`) — MUI select that accepts raw
  `<option value=...>` children; `value: string`, `onChange` is TextField's
  (`e.target.value`). Supports `disabled` and `helperText` (both used elsewhere in these forms).
- `CheckboxGroup` (`components/shared/CheckboxGroup.tsx`) — props `{ legend, options:{value,label}[], selected:string[], onToggle:(value)=>void }`.
- `EndpointMode` string union will be exported from `api/models.ts` by the DTO/models
  area and reaches this file via the `../api` barrel (`src/api.ts` does
  `export * from './api/models'` and `export * from './api/runtime'`).

### 1.5 i18n current strings — `gateway/frontend/src/i18n.ts`

de block (lines 371-376):

```ts
// i18n.ts:371
applicationFlavors: 'API-Varianten',
applicationNativeLegend: 'Native Durchreichung',
applicationNativeResponses: 'Codex (Responses-API) nativ durchreichen',
applicationNativeMessages: 'Claude Code (Anthropic Messages) nativ durchreichen',
applicationNativeNote:
  'Ist dies aktiv, werden Anfragen an /v1/responses bzw. /v1/messages unverändert an die Anwendung durchgereicht (keine Umwandlung), statt sie ins interne Format zu übersetzen. Voraussetzung: die passende API-Variante (openai bzw. anthropic) ist aktiviert und die Anwendung unterstützt den Endpunkt nativ.',
```

en block (lines 2466-2471):

```ts
// i18n.ts:2466
applicationFlavors: 'API flavors',
applicationNativeLegend: 'Native passthrough',
applicationNativeResponses: 'Pass Codex (Responses API) through natively',
applicationNativeMessages: 'Pass Claude Code (Anthropic Messages) through natively',
applicationNativeNote:
  'When enabled, requests to /v1/responses resp. /v1/messages are forwarded to the application unchanged (no conversion) instead of being translated to the internal format. Requires the matching API flavor (openai resp. anthropic) to be enabled and the application to support the endpoint natively.',
```

**i18n parity mechanism** (this is the whole enforcement — no runtime "all keys" test):

```ts
// i18n.ts:13   PortalMessageValue = string | (a)=>string | ...
// i18n.ts:14   const de = { ... } satisfies Record<string, PortalMessageValue>;   (ends line 2112)
// i18n.ts:2114 export type PortalMessages = typeof de;
// i18n.ts:2116 const en: PortalMessages = { ... };                                (ends line 4146)
// i18n.ts:4148 export const messages: Record<Locale, PortalMessages> = { de, en };
```

`Translation = (typeof messages)['de']` (`components/shared/types.ts:8`), and
`MessageKey` is the string-valued keys of it (types.ts:12-14). **Consequence:** adding a
key to `de` makes it part of `PortalMessages`, so `en` MUST define the same key or
`tsc` fails. Adding to only one locale is a compile error — that is the parity guard.
`i18n.test.ts` additionally has per-feature `it(...)` blocks listing keys and asserting
`typeof messages.de[k] === 'string'` and same for `en`; there is no generic
`Object.keys(de)` vs `Object.keys(en)` diff test, so extending i18n.test.ts is optional
polish, not required for parity.

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized)

Framework: **Vitest + @testing-library/react** (jsdom). Run from `gateway/frontend`.
Single-file run: `npm test -- src/components/RuntimeAdminSection.test.tsx`
(or `npx vitest run src/components/RuntimeAdminSection.test.tsx`). i18n test:
`npm test -- src/i18n.test.ts`. Whole suite + typecheck: `npm test` then `npm run build`.

> Ordering note: this area depends on (a) `EndpointMode` + `PortalApplication.{responses_mode,messages_mode}`
> from the models/DTO area, and (b) the `ApiVariantControls` component from **area 05**.
> Task 06.1 (runtime.ts types) has no such dependency and can land first. Tasks 06.2-06.4
> (form wiring) require area 05's component to exist; sequence them after it or stub the
> import per §GOTCHAS.

### Task 06.1 — Add `api_flavors` + `responses_mode` + `messages_mode` to `RuntimeSpec`/`PutRuntimeSpecRequest`

There is no dedicated unit test for `runtime.ts` types; the type is exercised through
the component test. The minimal, type-checked change:

Edit `gateway/frontend/src/api/runtime.ts`, inside `interface RuntimeSpec` (after
`set_visible_devices` / `gpus`, or grouped with a comment). `PutRuntimeSpecRequest`
needs no edit — the three fields are not in the `Omit` list, so they flow in.

```ts
// runtime.ts — add to interface RuntimeSpec (imported EndpointMode from './models')
import type { EndpointMode } from './models';   // add to the top imports
// ...
export interface RuntimeSpec {
  // ...existing fields...
  set_visible_devices: boolean;
  gpus: RuntimeSpecGPU[];
  // Per-endpoint API-variant snapshot for this managed model (gateway-side only;
  // NOT sent to the agent). api_flavors gates routing eligibility; the two modes
  // decide disabled / translate / passthrough for /v1/responses and /v1/messages.
  // A full-document upsert stores all three explicitly (snapshot from the app on
  // create; see RuntimeAdminSection). Mirrors the Go RuntimeSpecDTO.
  api_flavors: string[];
  responses_mode: EndpointMode;
  messages_mode: EndpointMode;
}
```

Verify: `npm run build` (tsc) in `gateway/frontend`. Expected: passes once
`EndpointMode` is exported from `api/models.ts` (models area) — otherwise the import
errors, which is the correct signal that this task is blocked on that area.

(If a test is wanted for the type surface, the component test in 06.4 already asserts the
three fields appear in `putSpecs[0].body`, which covers it end-to-end.)

### Task 06.2 — Render `ApiVariantControls` inside the spec form (fixture + wiring)

**Failing test** — add to `RuntimeAdminSection.test.tsx` (create-form describe block,
alongside the existing create tests at line ~724). Uses the same idiom
(`findByRole` create button, then `getByLabelText`):

```ts
// RuntimeAdminSection.test.tsx  (new it)
it('shows the API-variant controls in the spec create form', async () => {
  renderSection();
  fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
  // The two mode dropdowns, labeled for Codex (Responses) and Claude Code (Messages).
  expect(screen.getByLabelText(t.applicationResponsesMode)).toBeInTheDocument();
  expect(screen.getByLabelText(t.applicationMessagesMode)).toBeInTheDocument();
  // The flavor checkboxes come along too (shared component owns them).
  expect(screen.getByRole('checkbox', { name: 'openai' })).toBeInTheDocument();
  expect(screen.getByRole('checkbox', { name: 'anthropic' })).toBeInTheDocument();
});
```

Run: `npm test -- src/components/RuntimeAdminSection.test.tsx`. Expected FAIL
(labels not found) before implementation.

**Minimal implementation** — add form state (after line 2042), and render the component
in the form (after the config-section heading at line 3344). Import `ApiVariantControls`
and `EndpointMode`:

```ts
// top imports
import { ApiVariantControls } from './shared/ApiVariantControls';   // from area 05
import type { EndpointMode } from '../api';
```

```ts
// after RuntimeAdminSection.tsx:2042 (with the other spec-form hooks)
const [specApiFlavors, setSpecApiFlavors] = useState<string[]>([]);
const [specResponsesMode, setSpecResponsesMode] = useState<EndpointMode>('passthrough');
const [specMessagesMode, setSpecMessagesMode] = useState<EndpointMode>('passthrough');
```

```tsx
// in the spec form, immediately after the runtimeSpecConfigSection heading (line 3344 area)
<ApiVariantControls
  t={t}
  apiFlavors={specApiFlavors}
  responsesMode={specResponsesMode}
  messagesMode={specMessagesMode}
  onFlavorsChange={setSpecApiFlavors}
  onResponsesModeChange={setSpecResponsesMode}
  onMessagesModeChange={setSpecMessagesMode}
/>
```

Run again. Expected PASS.

### Task 06.3 — Pre-fill on CREATE from the parent app; show stored values on EDIT

**Failing test** (two cases):

```ts
it('pre-fills the API-variant controls from the parent app on create', async () => {
  renderSection({
    application: {
      ...application,
      api_flavors: ['openai'],
      responses_mode: 'translate',
      messages_mode: 'disabled',
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
  // openai checked, anthropic unchecked (unchecked => its dropdown is disabled+Deaktiviert per area 05).
  expect(screen.getByRole('checkbox', { name: 'openai' })).toBeChecked();
  expect(screen.getByRole('checkbox', { name: 'anthropic' })).not.toBeChecked();
  // Codex mode shows the app's stored 'translate'.
  expect(screen.getByLabelText(t.applicationResponsesMode)).toHaveValue('translate');
});

it('shows the spec stored API-variant values on edit', async () => {
  const spec = makeSpec({
    mapping_id: 'map_1', configured: true,
    api_flavors: ['anthropic'], responses_mode: 'disabled', messages_mode: 'passthrough',
  });
  renderSection({ mappings: [makeMapping({ id: 'map_1' })], specsByMappingId: { map_1: spec } });
  fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEdit })); // or the row edit action
  expect(screen.getByRole('checkbox', { name: 'anthropic' })).toBeChecked();
  expect(await screen.findByLabelText(t.applicationMessagesMode)).toHaveValue('passthrough');
});
```

(The edit path opens via the specs-list row action — reuse whatever the existing edit
tests use to reach `openEdit`; grep the test file for `runtimeSpecEdit` / the row edit
label to copy the exact trigger.)

**Minimal implementation** — snapshot in `openCreate`, hydrate in `hydrateSpecFields`:

```ts
// openCreate (line 2146): add snapshot from the parent app
function openCreate() {
  setGatewayName('');
  setAppName('');
  resetSpecFields();
  setSpecApiFlavors([...application.api_flavors]);
  setSpecResponsesMode(application.responses_mode);
  setSpecMessagesMode(application.messages_mode);
  setSpecMode('create');
}
```

```ts
// hydrateSpecFields (line 2120): add stored spec values
function hydrateSpecFields(spec: RuntimeSpec) {
  // ...existing...
  setSpecApiFlavors([...spec.api_flavors]);
  setSpecResponsesMode(spec.responses_mode);
  setSpecMessagesMode(spec.messages_mode);
}
```

Optional but consistent: reset in `resetSpecFields` (line 2101) so a stale value never
leaks between an edit and a later create — but since `openCreate` overwrites all three
right after calling `resetSpecFields`, add there only if you prefer symmetry:
`setSpecApiFlavors([]); setSpecResponsesMode('passthrough'); setSpecMessagesMode('passthrough');`.
(Note the module-default `application` fixture has `api_flavors: []`; tests that assert a
pre-filled checkbox must pass an `application` override with the flavor set, as above.)

Run. Expected PASS.

### Task 06.4 — Assemble the three fields into the PUT body

**Failing test**:

```ts
it('sends api_flavors + responses_mode + messages_mode in the spec PUT body', async () => {
  const { created, putSpecs } = renderSection({
    application: { ...application, api_flavors: ['openai', 'anthropic'],
                   responses_mode: 'passthrough', messages_mode: 'translate' },
  });
  fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
  fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
  fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), { target: { value: '/usr/bin/llama-server' } });
  fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

  await waitFor(() => expect(putSpecs).toHaveLength(1));
  expect(putSpecs[0].body.api_flavors).toEqual(['openai', 'anthropic']);
  expect(putSpecs[0].body.responses_mode).toBe('passthrough');
  expect(putSpecs[0].body.messages_mode).toBe('translate');
});
```

**Minimal implementation** — extend `buildSpecBody` (line 2359):

```ts
function buildSpecBody(args: string[], env: Record<string, string>): PutRuntimeSpecRequest {
  return {
    enabled,
    // ...existing fields unchanged...
    gpus: gpuRows.map((r) => ({ index: r.index, vram_estimate_mb: r.vramEstimateMb, vram_measured_mb: r.vramMeasuredMb })),
    api_flavors: specApiFlavors,
    responses_mode: specResponsesMode,
    messages_mode: specMessagesMode,
  };
}
```

`submitCreate` (2459) and `submitEdit` (2531) already call `buildSpecBody`, so nothing
else changes. Run. Expected PASS.

### Task 06.5 — i18n keys in BOTH de and en

**Failing test** — add to `i18n.test.ts` (a small key-presence block, matching the
existing style, e.g. near the flavor/native assertions):

```ts
describe('endpoint-mode i18n keys', () => {
  it('defines the endpoint-mode dropdown keys in de and en', () => {
    const keys = [
      'applicationResponsesMode',
      'applicationMessagesMode',
      'applicationModeDisabled',
      'applicationModeTranslate',
      'applicationModePassthrough',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
    }
  });
});
```

Run: `npm test -- src/i18n.test.ts`. Expected FAIL (keys undefined) before implementation.

**Minimal implementation** — add to the de block (near line 376) and the identical keys
to the en block (near line 2471). Proposed strings:

```ts
// de (after applicationNativeNote, ~line 376)
applicationResponsesMode: 'Codex (Responses-API)',
applicationMessagesMode: 'Claude Code (Anthropic Messages)',
applicationModeDisabled: 'Deaktiviert',
applicationModeTranslate: 'Umwandlung',
applicationModePassthrough: 'Durchreichen',
```

```ts
// en (after applicationNativeNote, ~line 2471)
applicationResponsesMode: 'Codex (Responses API)',
applicationMessagesMode: 'Claude Code (Anthropic Messages)',
applicationModeDisabled: 'Disabled',
applicationModeTranslate: 'Translate',
applicationModePassthrough: 'Pass-through',
```

These five keys are consumed by `ApiVariantControls` (area 05) AND, for the dropdown
labels, referenced in this area's tests via `t.applicationResponsesMode` /
`t.applicationMessagesMode`. Reuse decision (spec §13 open item): the existing
`applicationNativeResponses/Messages` strings ("… nativ durchreichen") describe a
boolean, so they are **not** reused as dropdown labels — the dropdown label is the
endpoint name and the three options carry the mode. Keep `applicationFlavors` ("API-Varianten"/"API flavors")
as the flavor checkbox legend. `applicationNativeLegend` / `applicationNativeNote` /
`applicationNativeResponses` / `applicationNativeMessages` become dead once area 05 stops
rendering the old CheckboxGroup — flag for area 05 to decide removal vs. repurpose; this
area does not remove them.

Run all: `npm test` (Vitest) then `npm run build` (tsc parity guard). Expected PASS.

---

## 3. INTERFACES

### PRODUCES (other areas consume)

- `RuntimeSpec.api_flavors: string[]`, `RuntimeSpec.responses_mode: EndpointMode`,
  `RuntimeSpec.messages_mode: EndpointMode` (runtime.ts) — and via the derived `Omit`,
  the same three on `PutRuntimeSpecRequest`. JSON keys `api_flavors`, `responses_mode`,
  `messages_mode` MUST match the Go `RuntimeSpecDTO` / `PutRuntimeSpecRequest` from the
  backend portal area byte-for-byte (runtime.ts's own header comment mandates this mirror).
- i18n keys (de+en): `applicationResponsesMode`, `applicationMessagesMode`,
  `applicationModeDisabled`, `applicationModeTranslate`,
  `applicationModePassthrough`.

### CONSUMES (from other areas)

- **From the models/DTO area** (`api/models.ts`): `export type EndpointMode = 'disabled' | 'translate' | 'passthrough';`
  and `PortalApplication.responses_mode: EndpointMode` + `PortalApplication.messages_mode: EndpointMode`
  (replacing `native_responses`/`native_messages: boolean`). This area's create pre-fill
  reads `application.responses_mode` / `application.messages_mode`; if those fields don't
  exist yet, this area does not compile.
- **From area 05** (`components/shared/ApiVariantControls.tsx`): the shared component.
  Expected props contract used here:

  ```ts
  type ApiVariantControlsProps = {
    t: Translation;
    apiFlavors: string[];
    responsesMode: EndpointMode;
    messagesMode: EndpointMode;
    onFlavorsChange: (next: string[]) => void;      // component owns the toggle math
    onResponsesModeChange: (next: EndpointMode) => void;
    onMessagesModeChange: (next: EndpointMode) => void;
  };
  ```

  The component owns the §5.3 gating (a dropdown is disabled + shows Deaktiviert when its
  flavor is unchecked), the option labels (the three `applicationMode*` keys), the
  flavor checkboxes (openai/anthropic), and the dropdown labels
  (`applicationResponsesMode` / `applicationMessagesMode`).

  **Deviation from the canonical prop shape in the task brief** (`values: {...}` +
  "onChange handlers"): the brief describes a grouped `values` object. This area proposes
  the three-flat-handlers shape above because both parent forms keep flavors/modes as
  three separate `useState` values (ApplicationSection: `flavors`, `nativeResponses`→`responsesMode`,
  `nativeMessages`→`messagesMode`; this form: `specApiFlavors`, `specResponsesMode`,
  `specMessagesMode`), so flat setters map 1:1 to `setState` with zero glue. **Area 05
  owns the final prop shape** — whichever it picks, this area adapts its three call-site
  props to match. No `disabled`/`readonly` prop and no label-override prop is needed by
  the spec form (it renders the identical control set as the app form, spec §5.1).

---

## 4. GOTCHAS

- **Test framework/run**: Vitest (`vitest run`), @testing-library/react, jsdom. From
  `gateway/frontend`: whole suite `npm test`; one file `npm test -- src/components/RuntimeAdminSection.test.tsx`
  or `npm test -- src/i18n.test.ts`; typecheck/build `npm run build`; lint `npm run lint`.
  `t = messages.de` in the component test — assert against German strings.
- **`buildSpecBody` is an explicit literal, not a spread** (line 2359) — adding fields to
  the `RuntimeSpec` type alone will NOT put them in the PUT body. You must add the three
  keys to `buildSpecBody` (Task 06.4) or the create/edit save silently omits them.
- **`specBodyWithAdminState` (line 481) uses rest-spread** — it carries the three new
  fields through automatically from a loaded spec, so override/restart PUTs preserve them.
  No edit needed there, but it means the round-trip is only correct if the GET response
  actually carries them (backend DTO area).
- **`PutRuntimeSpecRequest` is `Omit<RuntimeSpec, ...>` (line 66)** — do NOT re-declare
  the fields there; add them to `RuntimeSpec` and they flow in. Adding them to both is
  redundant and risks drift.
- **Test fixtures need updating** (compile-blocking): `RuntimeAdminSection.test.tsx`
  `makeSpec` factory (line 134-156) must gain `api_flavors: []`, `responses_mode: 'passthrough'`,
  `messages_mode: 'passthrough'`; the `application` fixture (line 73-107) must replace
  `native_responses`/`native_messages` (lines 91-92) with `responses_mode`/`messages_mode`
  (+ keep/set `api_flavors`). The api-mock `putRuntimeSpec` (line 355-365) echoes `...body`,
  so it needs no change. Also check `fullSpec` (line 1573) and `expectedBody` (line 1608)
  and any `PutRuntimeSpecRequest` literal in the override/restart tests — the derived
  `Omit` now requires the three fields, so every explicit `PutRuntimeSpecRequest` literal
  in the test file must include them or `tsc` fails. Grep the test for `PutRuntimeSpecRequest`
  and every `makeSpec(`/`fullSpec(` call site.
- **i18n parity is compile-time only**: adding a key to `de` forces it into `en` via
  `en: PortalMessages` (i18n.ts:2116). There is no `Object.keys(de) vs en` runtime test —
  so a missing en key is a `tsc` error caught by `npm run build`, not a Vitest failure.
  Add both locales in the same commit.
- **`application` fixture default has `api_flavors: []`** (test line 80) — a create-pre-fill
  test asserting a checked flavor MUST pass an `application` override (see Task 06.3).
- **Cross-area sequencing**: Task 06.1 (runtime.ts) is safe first. Tasks 06.2-06.4 import
  `ApiVariantControls` from area 05 and `EndpointMode` from the models area — do not land
  them before those exist, or stub the import temporarily. `EndpointMode` reaches this file
  through the `../api` barrel (`src/api.ts` re-exports `./api/models` and `./api/runtime`).
- **No agent/wire change** (spec §9): these three fields are gateway-portal-only. Nothing
  here touches `AgentRuntimeSpecDTO`, the SSE stream types, or `server-agent/`.
- **The `RuntimeSpec` doc header** (runtime.ts:6-11) states the type mirrors the Go DTOs
  byte-for-byte — keep the new field names/order/JSON tags in lockstep with the backend
  `RuntimeSpecDTO` (portal `service_runtime.go`), which is a separate area's deliverable.
