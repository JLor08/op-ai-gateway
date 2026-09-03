# Plan material — Area 05: Frontend application form + shared `ApiVariantControls` + types + type-defaults

Spec: `docs/superpowers/specs/2026-09-03-api-variant-endpoint-modes-design.md`
Worktree root (origin/main content): `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes`

All paths below are relative to `gateway/frontend/` unless absolute. Every line
reference is against the worktree files as they stand now.

---

## 0. Canonical interface names (this area)

**PRODUCES** (other areas consume these names — keep consistent):

- `EndpointMode` — TS enum string union, **added to `src/api/models.ts`**:
  `export type EndpointMode = 'disabled' | 'translate' | 'passthrough';`
  (mirrors the existing `ModelVisibility = 'shown' | 'hidden' | 'locked'` at
  `src/api/models.ts:211`; re-exported through the `src/api.ts` barrel via
  `export * from './api/models'` at `src/api.ts:40`, so importers write
  `import type { EndpointMode } from '../api'` / `'../../api'`).
- JSON/DTO fields (replace the two booleans): `responses_mode` / `messages_mode`
  of type `EndpointMode` on `PortalApplication`; optional on
  `CreateApplicationRequest` and `UpdateApplicationRequest`.
- Shared component `ApiVariantControls` (new file
  `src/components/shared/ApiVariantControls.tsx`) — props contract in §2.1.
- `TypeDefaults.responsesMode` / `TypeDefaults.messagesMode` (`EndpointMode`) in
  `src/components/shared/applicationTypeDefaults.ts` (replace
  `nativeResponses` / `nativeMessages`).
- New i18n keys (both `de` and `en`, TypeScript-enforced — see §4/GOTCHAS):
  `applicationResponsesMode`, `applicationMessagesMode` (dropdown field labels),
  `applicationModeDisabled`, `applicationModeTranslate`,
  `applicationModePassthrough` (dropdown option labels).

**CONSUMES** (from other areas, per spec §8):

- Backend `ApplicationDTO` must emit `responses_mode` / `messages_mode` as the
  three lowercase strings and accept them on create/update. The frontend types
  here are the mirror; if backend deviates, this area follows it.
- `RuntimeAdminSection.tsx` (a *different* area) also renders
  `ApiVariantControls`; that area drives the runtime-spec `api_flavors` +
  `responses_mode` + `messages_mode` fields (frontend `src/api/runtime.ts`,
  which today has **no** flavor/mode fields — `RuntimeSpec` at
  `src/api/runtime.ts:31-58`, `PutRuntimeSpecRequest = Omit<RuntimeSpec, ...>` at
  `:66`). The component contract below is deliberately form-agnostic so both
  consume it unchanged.

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 `src/api/models.ts` — the two booleans

`PortalApplication` (`src/api/models.ts:42-44`):
```ts
  // Native passthrough: proxy Codex (/v1/responses) resp. Claude Code
  // (/v1/messages) raw to the upstream instead of translating.
  native_responses: boolean;
  native_messages: boolean;
```

`CreateApplicationRequest` (`src/api/models.ts:86-87`):
```ts
  native_responses?: boolean;
  native_messages?: boolean;
```

`UpdateApplicationRequest` (`src/api/models.ts:122-123`):
```ts
  native_responses?: boolean;
  native_messages?: boolean;
```

Precedent for the enum union — `ModelVisibility` (`src/api/models.ts:211`):
```ts
export type ModelVisibility = 'shown' | 'hidden' | 'locked';
```

### 1.2 `src/components/shared/applicationTypeDefaults.ts`

`TypeDefaults` interface (`:14-23`):
```ts
export interface TypeDefaults {
  port: number;
  scheme: ApplicationScheme;
  nativeResponses: boolean; // Codex /v1/responses native passthrough
  nativeMessages: boolean; // Claude /v1/messages native passthrough
  loadedModelsPath: string;
  loadedModelsFormat: string;
  contextProbePath: string;
  timeoutMs: number;
}
```

Per-type table `applicationTypeDefaults` (`:26-94`) — current mode values per type:
| type | `nativeResponses` | `nativeMessages` |
|---|---|---|
| `ollama` (`:27-36`) | `false` | `true` |
| `vllm` (`:37-46`) | `true` | `true` |
| `llama_cpp` (`:47-56`) | `true` | `true` |
| `llama_swap` (`:57-66`) | `true` | `true` |
| `litellm` (`:67-76`) | `true` | `true` |
| `server_agent` (`:84-93`) | `false` | `false` |

`migrateTypeFields` (`:99-114`) — iterates `Object.keys(newDefaults)`, copies each
key whose `current` value still equals `oldDefaults[key]`. **Logic is
field-agnostic and stays as-is**; only the field names in the union change.

Import line (`:4`): `import type { ApplicationType, ApplicationScheme } from '../../api';`
(add `EndpointMode` here).

### 1.3 `src/components/ApplicationSection.tsx`

Module constant (`:47`): `const applicationFlavorOptions = ['openai', 'anthropic'];`
(kept — still used by openCreate `:293` and the flavors block).

Module helper `toggleFlavor` (`:118-120`) — **becomes unused** once the inline
flavors `CheckboxGroup` is replaced (only caller is `:830`); delete it (moves
into `ApiVariantControls`):
```ts
function toggleFlavor(list: string[], flavor: string): string[] {
  return list.includes(flavor) ? list.filter((item) => item !== flavor) : [...list, flavor];
}
```

`flavors` state (`:211`): `const [flavors, setFlavors] = useState<string[]>([...applicationFlavorOptions]);` (kept).

Native bool state (`:227-228`):
```ts
  const [nativeResponses, setNativeResponses] = useState(false);
  const [nativeMessages, setNativeMessages] = useState(false);
```

`openCreate` seeding (`:293`, `:304-305`):
```ts
    setFlavors([...applicationFlavorOptions]);       // :293
    ...
    setNativeResponses(d.nativeResponses);           // :304
    setNativeMessages(d.nativeMessages);             // :305
```

`handleTypeChange` migration (`:321-341`) — passes/reads the two bools:
```ts
  function handleTypeChange(newType: ApplicationType) {
    const patch = migrateTypeFields(type, newType, {
      port,
      scheme,
      nativeResponses,          // :325
      nativeMessages,           // :326
      loadedModelsPath,
      loadedModelsFormat,
      contextProbePath,
      timeoutMs,
    });
    if (patch.port !== undefined) setPort(patch.port);
    if (patch.scheme !== undefined) setScheme(patch.scheme);
    if (patch.nativeResponses !== undefined) setNativeResponses(patch.nativeResponses);   // :334
    if (patch.nativeMessages !== undefined) setNativeMessages(patch.nativeMessages);      // :335
    ...
    setType(newType);
  }
```

`openEdit` seeding (`:347`, `:362-363`):
```ts
    setFlavors([...app.api_flavors]);                // :347
    ...
    setNativeResponses(app.native_responses);        // :362
    setNativeMessages(app.native_messages);          // :363
```

`buildBody` writes (`:396`, `:407-408`):
```ts
      api_flavors: flavors,                          // :396
      ...
      native_responses: nativeResponses,             // :407
      native_messages: nativeMessages,               // :408
```

The two `CheckboxGroup` blocks + note in the JSX (`:826-849`) — the flavors group,
the native group, and the caption:
```tsx
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
              selected={[
                ...(nativeResponses ? ['responses'] : []),
                ...(nativeMessages ? ['messages'] : []),
              ]}
              onToggle={(v) => {
                if (v === 'responses') setNativeResponses((c) => !c);
                else if (v === 'messages') setNativeMessages((c) => !c);
              }}
            />
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t.applicationNativeNote}
            </Typography>
```
`CheckboxGroup` import (`:27`) and `SelectField` import (`:26`) both stay — used
elsewhere in the file (proxy control `:780`, metrics `:944`; scheme/status/health
selects). Add an `ApiVariantControls` import.

### 1.4 Shared building blocks the new component reuses

`SelectField` (`src/components/shared/SelectField.tsx`) props (`:8-19`):
```ts
type SelectFieldProps = {
  id: string;
  label?: string;
  value: string;
  onChange: TextFieldProps['onChange'];
  children: ReactNode;
  inputProps?: Record<string, unknown>;
} & Omit<TextFieldProps, 'id'|'label'|'value'|'onChange'|'select'|'slotProps'|'children'|'inputProps'>;
```
- Maps raw `<option value=... disabled?>` children to MUI `<MenuItem>` (`:42-56`);
  `disabled` on an `<option>` is honoured (`:49`).
- `onChange` fires `event.target.value` (TextField-select semantics).
- The whole field's `disabled` (and `helperText`) flow through `...rest`
  (`TextFieldProps`). A disabled `SelectField` renders `aria-disabled="true"` on
  its combobox — asserted at `ApplicationSection.test.tsx:669-671` for the scheme
  select. The selected option's label is the combobox's text content — asserted
  via `toHaveTextContent(...)` throughout the test.

`CheckboxGroup` (`src/components/shared/CheckboxGroup.tsx`) props (`:12-24`):
```ts
{ legend: string; ariaLabel?: string; options: {value:string;label:string}[];
  selected: string[]; onToggle: (value: string) => void; }
```
`fieldset`+`legend` → `role=group`; each `FormControlLabel` label = the option
label; `onToggle` reports only the toggled value (caller owns `selected`).

### 1.5 i18n current strings

`src/i18n.ts` — `de` (`:371-376`):
```ts
  applicationFlavors: 'API-Varianten',
  applicationNativeLegend: 'Native Durchreichung',
  applicationNativeResponses: 'Codex (Responses-API) nativ durchreichen',
  applicationNativeMessages: 'Claude Code (Anthropic Messages) nativ durchreichen',
  applicationNativeNote:
    'Ist dies aktiv, werden Anfragen an /v1/responses bzw. /v1/messages unverändert ...',
```
`en` (`:2466-2471`):
```ts
  applicationFlavors: 'API flavors',
  applicationNativeLegend: 'Native passthrough',
  applicationNativeResponses: 'Pass Codex (Responses API) through natively',
  applicationNativeMessages: 'Pass Claude Code (Anthropic Messages) through natively',
  applicationNativeNote:
    'When enabled, requests to /v1/responses resp. /v1/messages are forwarded ...',
```
Type coupling: `PortalMessages = typeof de` (`src/i18n.ts:2114`); `const en:
PortalMessages` (`:2116`). **Any key added to `de` must be added to `en` or tsc
fails.** `applicationNativeLegend` becomes unused after the swap (was the native
group's legend) — leave it in place (harmless inert string) OR remove from both
locales; keep it to avoid churn, note it's now orphaned. `applicationNativeNote`
is reused verbatim by the new component. `applicationNativeResponses` /
`applicationNativeMessages` are superseded by the new field-label keys (see §13
open question — reuse vs fresh); this plan uses **fresh** keys and leaves the old
two in place inert.

### 1.6 Existing test assertions that must change

`src/components/ApplicationSection.test.tsx`:

- `makeApp` fixture (`:76-77`) sets `native_responses: false, native_messages:
  false`. Must become `responses_mode: 'passthrough', messages_mode:
  'passthrough'` (or any valid `EndpointMode`) so the `PortalApplication` type
  compiles.
- `describe('ApplicationSection native passthrough toggles')` (`:787-832`) asserts
  **checkboxes** by role/name and the boolean payload:
  ```ts
    const responses = screen.getByRole('checkbox', { name: t.applicationNativeResponses });
    const messages  = screen.getByRole('checkbox', { name: t.applicationNativeMessages });
    expect(responses.checked).toBe(false);
    expect(messages.checked).toBe(true);
    ...
    expect(created[0].native_responses).toBe(true);
    expect(created[0].native_messages).toBe(true);
    ...
    apps: [makeApp({ id: 'app_1', native_responses: true, native_messages: false })],
    ...
    expect(updated[0].body.native_responses).toBe(true);
    expect(updated[0].body.native_messages).toBe(true);
  ```
  This whole `describe` is rewritten to drive the two **dropdowns** (§3, Task 3).

- `applicationTypeDefaults.test.ts` (`src/components/shared/applicationTypeDefaults.test.ts`)
  asserts `nativeResponses`/`nativeMessages` in the `toEqual` snapshots
  (`:16-17`, `:29-30`, `:46-47`) and `expect(patch.nativeResponses).toBe(true)`
  (`:67`). Rewritten in Task 2.

### 1.7 Cross-file fixture ripple (flag for the plan author)

Changing `PortalApplication` (required `responses_mode`/`messages_mode`) breaks
every `PortalApplication` fixture in the suite under tsc, and the create-echo
fake in `App.test.tsx`. Files with `native_responses`/`native_messages` literals
that must be migrated to the new fields (verified by grep):
- `src/App.test.tsx:824-825, 856-857, 2011-2012, 2084-2085, 2149-2150, 2184-2185, 2249-2250`
  (the create-echo backend at `:824-857` reads `body.native_responses ?? false`
  — swap to `body.responses_mode ?? 'passthrough'`).
- `src/components/RuntimeAdminSection.test.tsx:91-92`
- `src/components/ServerList.test.tsx:176-177`
- `src/components/MappingSection.test.tsx:71-72`
- `src/components/BenchmarkSection.test.tsx:76-77`
- `src/components/ApplicationSection.test.tsx:76-77` (this area)

These live in other areas' test files; coordinate so the tsc gate
(`npx tsc --noEmit` / `npm run build`) stays green after the full change. vitest
itself (esbuild) does **not** type-check, so a per-file `npm test -- <file>` runs
green even while sibling fixtures are stale — the tsc gate is what catches them.

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized)

Framework: **Vitest** + `@testing-library/react` (jsdom). `npm test` = `vitest run`
(`package.json`). Setup file `src/vitest.setup.ts` (jest-dom matchers). Run one
file: `cd gateway/frontend && npm test -- <path>`. Filter by name:
`npm test -- -t "<substring>"`. Type gate: `cd gateway/frontend && npx tsc --noEmit`.

---

### Task 1 — `ApiVariantControls` component (self-contained; folds in `EndpointMode` + i18n keys)

This task does **not** touch `ApplicationSection.tsx`, so its test compiles and
runs in isolation even before the form is wired.

**1a. RED — write the test first.**
File: `src/components/shared/ApiVariantControls.test.tsx`
```tsx
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiVariantControls } from './ApiVariantControls';
import { messages } from '../../i18n';
import type { EndpointMode } from '../../api';

const t = messages.de;

afterEach(cleanup);

// A stateful harness so a checkbox toggle re-renders the controls with the new
// apiFlavors (the component is controlled; the parent owns the state).
function Harness({
  flavors = ['openai', 'anthropic'],
  responses = 'passthrough',
  msgs = 'passthrough',
}: {
  flavors?: string[];
  responses?: EndpointMode;
  msgs?: EndpointMode;
}) {
  const [apiFlavors, setApiFlavors] = useState<string[]>(flavors);
  const [responsesMode, setResponsesMode] = useState<EndpointMode>(responses);
  const [messagesMode, setMessagesMode] = useState<EndpointMode>(msgs);
  return (
    <ApiVariantControls
      t={t}
      apiFlavors={apiFlavors}
      responsesMode={responsesMode}
      messagesMode={messagesMode}
      onFlavorsChange={setApiFlavors}
      onResponsesModeChange={setResponsesMode}
      onMessagesModeChange={setMessagesMode}
    />
  );
}

const responsesField = () => screen.getByRole('combobox', { name: t.applicationResponsesMode });
const messagesField = () => screen.getByRole('combobox', { name: t.applicationMessagesMode });

describe('ApiVariantControls', () => {
  it('renders both flavor checkboxes and, when checked, enables the dropdowns showing the stored mode', () => {
    render(<Harness />);
    expect(screen.getByRole('checkbox', { name: 'openai' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'anthropic' })).toBeChecked();
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModePassthrough);
    expect(messagesField()).toHaveTextContent(t.applicationModePassthrough);
  });

  it('disables the Codex dropdown and shows Deaktiviert when openai is unchecked, ignoring the stored mode', () => {
    render(<Harness flavors={['anthropic']} responses="translate" />);
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);
    // The anthropic side is unaffected.
    expect(messagesField()).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('restores the stored mode when the flavor is re-checked (mode not clobbered while unchecked)', () => {
    render(<Harness responses="translate" />);
    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' })); // uncheck
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);
    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' })); // re-check
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
  });

  it('offers exactly the three modes in order and reports the chosen one', () => {
    render(<Harness />);
    fireEvent.mouseDown(messagesField());
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      t.applicationModeDisabled,
      t.applicationModeTranslate,
      t.applicationModePassthrough,
    ]);
    fireEvent.click(screen.getByRole('option', { name: t.applicationModeTranslate }));
    expect(messagesField()).toHaveTextContent(t.applicationModeTranslate);
  });

  it('reports the toggled flavor list', () => {
    const onFlavorsChange = vi.fn();
    render(
      <ApiVariantControls
        t={t}
        apiFlavors={['openai', 'anthropic']}
        responsesMode="passthrough"
        messagesMode="passthrough"
        onFlavorsChange={onFlavorsChange}
        onResponsesModeChange={() => {}}
        onMessagesModeChange={() => {}}
      />,
    );
    fireEvent.click(screen.getByRole('checkbox', { name: 'anthropic' }));
    expect(onFlavorsChange).toHaveBeenCalledWith(['openai']);
  });
});
```
Run: `cd gateway/frontend && npm test -- src/components/shared/ApiVariantControls.test.tsx`
Expected: **FAIL** — `Cannot find module './ApiVariantControls'` (and unknown i18n
keys / `EndpointMode`).

**1b. GREEN — minimal implementation (three edits, one new file).**

(i) `src/api/models.ts` — add the union next to `ModelVisibility` (after `:211`):
```ts
// Per-endpoint serving mode for the two coding-agent APIs (Codex /v1/responses,
// Claude Code /v1/messages): 'disabled' = not served, 'translate' = converted to
// /v1/chat/completions (lossy), 'passthrough' = proxied raw to the native path.
export type EndpointMode = 'disabled' | 'translate' | 'passthrough';
```

(ii) `src/i18n.ts` — add to **`de`** right after `applicationNativeNote` (`:376`):
```ts
  applicationResponsesMode: 'Codex (Responses-API)',
  applicationMessagesMode: 'Claude Code (Anthropic Messages)',
  applicationModeDisabled: 'Deaktiviert',
  applicationModeTranslate: 'Umwandlung',
  applicationModePassthrough: 'Durchreichen',
```
and the mirror in **`en`** right after `applicationNativeNote` (`:2471`):
```ts
  applicationResponsesMode: 'Codex (Responses API)',
  applicationMessagesMode: 'Claude Code (Anthropic Messages)',
  applicationModeDisabled: 'Disabled',
  applicationModeTranslate: 'Translate',
  applicationModePassthrough: 'Pass-through',
```

(iii) New file `src/components/shared/ApiVariantControls.tsx`:
```tsx
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Typography } from '@mui/material';
import { CheckboxGroup } from './CheckboxGroup';
import { SelectField } from './SelectField';
import type { EndpointMode } from '../../api';
import type { Translation } from './types';

// The two base API-Varianten capability checkboxes. openai gates /v1/responses
// (Codex) AND /v1/chat/completions; anthropic gates /v1/messages (Claude Code).
const apiVariantFlavorOptions = ['openai', 'anthropic'];
const endpointModeOptions: EndpointMode[] = ['disabled', 'translate', 'passthrough'];

const endpointModeLabelByKey: Record<
  EndpointMode,
  'applicationModeDisabled' | 'applicationModeTranslate' | 'applicationModePassthrough'
> = {
  disabled: 'applicationModeDisabled',
  translate: 'applicationModeTranslate',
  passthrough: 'applicationModePassthrough',
};

function toggleFlavor(list: string[], flavor: string): string[] {
  return list.includes(flavor) ? list.filter((item) => item !== flavor) : [...list, flavor];
}

/**
 * The shared "API-Varianten" control block: the openai/anthropic capability
 * checkboxes plus one three-state endpoint-mode dropdown per coding-agent API.
 * Both ApplicationSection and RuntimeAdminSection render it, so the two forms
 * stay identical (design §5.1).
 *
 * Gating (§5.3): a dropdown whose flavor checkbox is UNCHECKED is disabled and
 * shows Deaktiviert regardless of the stored mode — the stored mode is left
 * untouched (only the DISPLAY is forced), so re-checking the flavor restores it.
 */
export function ApiVariantControls({
  t,
  apiFlavors,
  responsesMode,
  messagesMode,
  onFlavorsChange,
  onResponsesModeChange,
  onMessagesModeChange,
}: Readonly<{
  t: Translation;
  apiFlavors: string[];
  responsesMode: EndpointMode;
  messagesMode: EndpointMode;
  onFlavorsChange: (flavors: string[]) => void;
  onResponsesModeChange: (mode: EndpointMode) => void;
  onMessagesModeChange: (mode: EndpointMode) => void;
}>) {
  const openaiEnabled = apiFlavors.includes('openai');
  const anthropicEnabled = apiFlavors.includes('anthropic');
  return (
    <>
      <CheckboxGroup
        legend={t.applicationFlavors}
        options={apiVariantFlavorOptions.map((f) => ({ value: f, label: f }))}
        selected={apiFlavors}
        onToggle={(v) => onFlavorsChange(toggleFlavor(apiFlavors, v))}
      />
      <SelectField
        id="application-responses-mode"
        label={t.applicationResponsesMode}
        value={openaiEnabled ? responsesMode : 'disabled'}
        onChange={(e) => onResponsesModeChange(e.target.value as EndpointMode)}
        disabled={!openaiEnabled}
      >
        {endpointModeOptions.map((m) => (
          <option value={m} key={m}>
            {t[endpointModeLabelByKey[m]]}
          </option>
        ))}
      </SelectField>
      <SelectField
        id="application-messages-mode"
        label={t.applicationMessagesMode}
        value={anthropicEnabled ? messagesMode : 'disabled'}
        onChange={(e) => onMessagesModeChange(e.target.value as EndpointMode)}
        disabled={!anthropicEnabled}
      >
        {endpointModeOptions.map((m) => (
          <option value={m} key={m}>
            {t[endpointModeLabelByKey[m]]}
          </option>
        ))}
      </SelectField>
      <Typography variant="caption" sx={{ color: 'text.secondary' }}>
        {t.applicationNativeNote}
      </Typography>
    </>
  );
}
```
Run the same command. Expected: **PASS** (5/5).

> Note on `Translation` import: `src/components/shared/types.ts:8` defines
> `export type Translation = (typeof messages)['de']`. Existing shared components
> (e.g. `applicationTypeDefaults` consumers) already pass `t: Translation`.

---

### Task 2 — `applicationTypeDefaults` modes → `passthrough` for every type

**2a. RED — rewrite `applicationTypeDefaults.test.ts`** (`src/components/shared/`).
Replace the three `toEqual` snapshots' native fields and the migrate assertion:
```ts
  it('llama_swap has the loaded/probe/mode/port defaults', () => {
    expect(applicationTypeDefaults.llama_swap).toEqual({
      port: 8080,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/running',
      loadedModelsFormat: 'llama_swap',
      contextProbePath: '/upstream/{model}/props',
      timeoutMs: 30000,
    });
  });

  it('ollama defaults both endpoint modes to passthrough, /api/ps auto, no probe', () => {
    expect(applicationTypeDefaults.ollama).toEqual({
      port: 11434,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/api/ps',
      loadedModelsFormat: 'auto',
      contextProbePath: '',
      timeoutMs: 30000,
    });
  });

  it('server_agent defaults llama-swap-shaped loaded models, passthrough modes, plus a 10-minute timeout', () => {
    expect(applicationTypeDefaults.server_agent).toEqual({
      port: 8081,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/running',
      loadedModelsFormat: 'llama_swap',
      contextProbePath: '',
      timeoutMs: 600000,
    });
  });
```
And replace the `nativeResponses` migrate assertion (`:67`) — since every type now
shares `passthrough`, pick a genuinely differing field to prove migration:
```ts
  it('preserves a customized field and migrates untouched ones', () => {
    const current: TypeDefaults = { ...applicationTypeDefaults.ollama, port: 9999 };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    expect(patch.port).toBeUndefined(); // customized → kept
    expect(patch.loadedModelsPath).toBe('/v1/models'); // untouched → migrated
    expect(patch.loadedModelsFormat).toBe('openai');
  });

  // Both endpoint modes now default to passthrough for every type, so a switch
  // between types that both hold the default is a no-op for those two fields.
  it('leaves an already-passthrough mode alone across a type switch', () => {
    const current = { ...applicationTypeDefaults.ollama };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    expect(patch.responsesMode).toBeUndefined();
    expect(patch.messagesMode).toBeUndefined();
  });

  // A mode the operator moved off the shared default follows the general
  // contract: it is preserved (never clobbered back to passthrough).
  it('preserves a customized endpoint mode across a type switch', () => {
    const current: TypeDefaults = { ...applicationTypeDefaults.ollama, responsesMode: 'translate' };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    expect(patch.responsesMode).toBeUndefined();
  });
```
Run: `cd gateway/frontend && npm test -- src/components/shared/applicationTypeDefaults.test.ts`
Expected: **FAIL** (defaults still carry `nativeResponses`/`nativeMessages`).

**2b. GREEN — edit `applicationTypeDefaults.ts`.**
Import (`:4`):
```ts
import type { ApplicationType, ApplicationScheme, EndpointMode } from '../../api';
```
Interface (`:14-23`) — replace the two bool fields:
```ts
export interface TypeDefaults {
  port: number;
  scheme: ApplicationScheme;
  responsesMode: EndpointMode; // Codex /v1/responses endpoint mode
  messagesMode: EndpointMode; // Claude Code /v1/messages endpoint mode
  loadedModelsPath: string;
  loadedModelsFormat: string;
  contextProbePath: string;
  timeoutMs: number;
}
```
Table (`:26-94`) — in **every** one of the six entries replace the
`nativeResponses`/`nativeMessages` pair with:
```ts
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
```
(Update the stale per-line comments — e.g. ollama's `// Ollama has no /v1/responses`
— to reflect that pass-through is the uniform default per design §6.)
`migrateTypeFields` (`:99-114`) is **unchanged** (field-agnostic). Run again:
Expected: **PASS**.

---

### Task 3 — Wire `ApplicationSection` to `ApiVariantControls` + update its test

**3a. RED — update `src/components/ApplicationSection.test.tsx`.**

(i) `makeApp` (`:76-77`) — swap the two lines:
```ts
    responses_mode: 'passthrough',
    messages_mode: 'passthrough',
```

(ii) Replace the whole `describe('ApplicationSection native passthrough toggles')`
(`:787-832`) with:
```tsx
// The three-state endpoint-mode dropdowns (design: API-variant endpoint modes)
// that replaced the two native-passthrough checkboxes. openai gates the Codex
// (Responses) dropdown; anthropic gates the Claude Code (Messages) dropdown.
describe('ApplicationSection API-variant endpoint modes', () => {
  const responsesField = () => screen.getByRole('combobox', { name: t.applicationResponsesMode });
  const messagesField = () => screen.getByRole('combobox', { name: t.applicationMessagesMode });

  async function selectMode(field: HTMLElement, optionLabel: string) {
    fireEvent.mouseDown(field);
    fireEvent.click(await screen.findByRole('option', { name: optionLabel }));
  }

  it('defaults both modes to Durchreichen and sends passthrough on create', async () => {
    const { created } = renderSection();
    openCreate();
    expect(responsesField()).toHaveTextContent(t.applicationModePassthrough);
    expect(messagesField()).toHaveTextContent(t.applicationModePassthrough);

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].responses_mode).toBe('passthrough');
    expect(created[0].messages_mode).toBe('passthrough');
  });

  it('populates the modes on edit and saves a change', async () => {
    const { updated } = renderSection({
      apps: [makeApp({ id: 'app_1', responses_mode: 'translate', messages_mode: 'disabled' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
    expect(messagesField()).toHaveTextContent(t.applicationModeDisabled);

    await selectMode(responsesField(), t.applicationModePassthrough);
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.responses_mode).toBe('passthrough');
    expect(updated[0].body.messages_mode).toBe('disabled');
  });

  it('disables the Codex dropdown when the openai flavor is unchecked, without clobbering the stored mode', async () => {
    const { created } = renderSection();
    openCreate();
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');

    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' }));
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].api_flavors).toEqual(['anthropic']);
    // The stored mode rides along untouched; the backend's effective rule gates
    // on the unchecked flavor.
    expect(created[0].responses_mode).toBe('passthrough');
  });
});
```
Run: `cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx`
Expected: **FAIL** (form still renders checkboxes / sends `native_*`; unknown
`responses_mode` on the payload).

**3b. GREEN — edit `src/components/ApplicationSection.tsx`.**

- Import (near `:27`): `import { ApiVariantControls } from './shared/ApiVariantControls';`
  and `import type { ... , EndpointMode } from '../api';` (add `EndpointMode` to the
  existing type-import block `:10-19`).
- Delete `toggleFlavor` (`:118-120`) — now unused.
- Replace the state (`:227-228`):
  ```ts
    const [responsesMode, setResponsesMode] = useState<EndpointMode>('passthrough');
    const [messagesMode, setMessagesMode] = useState<EndpointMode>('passthrough');
  ```
- `openCreate` (`:304-305`):
  ```ts
      setResponsesMode(d.responsesMode);
      setMessagesMode(d.messagesMode);
  ```
- `handleTypeChange` (`:325-326`, `:334-335`):
  ```ts
        responsesMode,
        messagesMode,
  ...
      if (patch.responsesMode !== undefined) setResponsesMode(patch.responsesMode);
      if (patch.messagesMode !== undefined) setMessagesMode(patch.messagesMode);
  ```
- `openEdit` (`:362-363`):
  ```ts
      setResponsesMode(app.responses_mode);
      setMessagesMode(app.messages_mode);
  ```
- `buildBody` (`:407-408`):
  ```ts
      responses_mode: responsesMode,
      messages_mode: messagesMode,
  ```
  (`api_flavors: flavors,` at `:396` stays.)
- Replace the JSX block (`:826-849`, both `CheckboxGroup`s + the caption) with:
  ```tsx
            <ApiVariantControls
              t={t}
              apiFlavors={flavors}
              responsesMode={responsesMode}
              messagesMode={messagesMode}
              onFlavorsChange={setFlavors}
              onResponsesModeChange={setResponsesMode}
              onMessagesModeChange={setMessagesMode}
            />
  ```
Run again: Expected **PASS**.

**3c. Type gate + fixture ripple (Task 4).** `cd gateway/frontend && npx tsc
--noEmit` now fails only in the sibling test fixtures listed in §1.7. Fix each
(`native_responses`/`native_messages` → `responses_mode`/`messages_mode` with a
valid `EndpointMode`; in `App.test.tsx` the create-echo backend reads
`body.responses_mode ?? 'passthrough'`). Then the whole suite + tsc are green:
```
cd gateway/frontend && npm test && npx tsc --noEmit
```

---

### Task 4 — `models.ts` DTO field swap (do together with Task 3b or immediately after)

`src/api/models.ts`:
- `PortalApplication` (`:42-44`):
  ```ts
    // Per-endpoint serving mode for Codex (/v1/responses) and Claude Code
    // (/v1/messages): disabled | translate | passthrough (replaces the old
    // native_* booleans).
    responses_mode: EndpointMode;
    messages_mode: EndpointMode;
  ```
- `CreateApplicationRequest` (`:86-87`):
  ```ts
    responses_mode?: EndpointMode;
    messages_mode?: EndpointMode;
  ```
- `UpdateApplicationRequest` (`:122-123`) — optional, keep-if-absent, mirroring the
  current pointer-bool semantics:
  ```ts
    responses_mode?: EndpointMode;
    messages_mode?: EndpointMode;
  ```
(No test of its own — compile-checked via tsc and exercised by Tasks 1/3.)

---

## 3. INTERFACES (produced / consumed) — canonical names

Deviations from the spec's suggested Go/JSON names on the frontend side:

| Spec canonical | This area uses | Deviation? |
|---|---|---|
| `EndpointMode` union `'disabled'\|'translate'\|'passthrough'` | `EndpointMode` in `src/api/models.ts` | none |
| JSON `responses_mode` / `messages_mode` | `responses_mode` / `messages_mode` on all three DTOs | none |
| Component `ApiVariantControls` | `ApiVariantControls` (`src/components/shared/`) | none |
| — | `TypeDefaults.responsesMode` / `.messagesMode` (camelCase, form-state naming) | camelCase is the existing `TypeDefaults` convention (`nativeResponses` etc.); JSON stays snake_case |

Component **props contract** (frozen for `RuntimeAdminSection` to reuse verbatim):
```ts
{
  t: Translation;
  apiFlavors: string[];
  responsesMode: EndpointMode;
  messagesMode: EndpointMode;
  onFlavorsChange: (flavors: string[]) => void;      // receives the FULL new array
  onResponsesModeChange: (mode: EndpointMode) => void;
  onMessagesModeChange: (mode: EndpointMode) => void;
}
```
The component owns `toggleFlavor` and the gating rule; the parent only stores the
three values. `onFlavorsChange` gets the whole toggled array, so a parent can pass
`setFlavors` directly.

New i18n keys added (both locales): `applicationResponsesMode`,
`applicationMessagesMode`, `applicationModeDisabled`, `applicationModeTranslate`,
`applicationModePassthrough`. Reused: `applicationFlavors`, `applicationNativeNote`.
Orphaned-but-kept: `applicationNativeLegend`, `applicationNativeResponses`,
`applicationNativeMessages` (see §13 open question).

---

## 4. GOTCHAS (plan author must not miss)

1. **i18n is type-locked.** `PortalMessages = typeof de` and `en: PortalMessages`
   (`src/i18n.ts:2114,2116`) — every new key must be added to BOTH `de` and `en`
   or tsc fails. There is no runtime parity test for these specific keys, but the
   `de`/`en` shape mismatch is a compile error. Keep the key order aligned for
   readability (insert after `applicationNativeNote` in each locale).
2. **vitest does not type-check.** esbuild transpiles per file, so a single-file
   `npm test -- <file>` runs even while sibling fixtures are stale. The tsc gate
   (`npx tsc --noEmit`, part of `npm run build`) is what catches the fixture
   ripple in §1.7 — run it before claiming done, and migrate all five other test
   files' `PortalApplication` fixtures + the `App.test.tsx` create-echo backend.
3. **MUI Select interaction pattern** (copy from existing tests): to read the
   value use `toHaveTextContent(optionLabel)`; to change it,
   `fireEvent.mouseDown(combobox)` then `fireEvent.click(await
   screen.findByRole('option', { name }))`. Options render in a portal on
   `document.body`. A disabled `SelectField` sets `aria-disabled="true"` on the
   combobox (assert with `toHaveAttribute('aria-disabled','true')`). See
   `ApplicationSection.test.tsx` `selectHealthMode` (`:244-247`) and the scheme
   disabled assertion (`:669-671`).
4. **Gating must not mutate stored state** (design §3.2/§5.3). The dropdown's
   *display* is forced to `'disabled'` when its flavor is unchecked
   (`value={openaiEnabled ? responsesMode : 'disabled'}`), but the parent's
   `responsesMode`/`messagesMode` state is left as-is, and `buildBody` sends the
   stored value. Task 3b's third test pins this (uncheck → still sends the stored
   mode). Do **not** add a `useEffect` that resets the mode to `disabled` when a
   flavor is unchecked — that would break "re-check restores the prior mode".
5. **`makeApp` and all `PortalApplication` fixtures now require the two new
   required fields.** Missing them is a tsc error (they're non-optional on
   `PortalApplication`). Give them a valid `EndpointMode` default
   (`'passthrough'`).
6. **`toggleFlavor` moves into the component.** After removing the inline flavors
   `CheckboxGroup` from `ApplicationSection.tsx`, the module-level `toggleFlavor`
   (`:118-120`) has no caller — leaving it triggers the `no-unused-vars` lint;
   delete it. `applicationFlavorOptions` (`:47`) stays (openCreate `:293`).
7. **Do not touch `src/api/runtime.ts` here.** The runtime-spec flavors/modes
   (`api_flavors`, `responses_mode`, `messages_mode` on `RuntimeSpec` /
   `PutRuntimeSpecRequest`) and `RuntimeAdminSection` wiring are a separate area;
   this area only guarantees `ApiVariantControls`' props contract is form-agnostic
   so that area can drop it in. The `RuntimeAdminSection.test.tsx:91-92` fixture
   fix in §1.7 is the only cross-boundary edit this area's tsc gate forces.
8. **No new `@mui` deps / no form library.** Controlled inputs only, matching the
   existing file (plain `useState`, MUI `TextField`/`Checkbox` via the shared
   wrappers). The component adds only a `Typography` import from `@mui/material`.
9. **`applicationNativeNote` reuse.** The existing note (`src/i18n.ts:376/2471`)
   still reads "requests to /v1/responses resp. /v1/messages are forwarded
   unchanged (no conversion)" — it now sits under the dropdowns. Consider updating
   its wording to name the three states, but that is cosmetic; the tests don't
   assert on it. Flag for the doc/i18n reviewer.

---

## 5. Exact run commands (summary)

- Component: `cd gateway/frontend && npm test -- src/components/shared/ApiVariantControls.test.tsx`
- Type defaults: `cd gateway/frontend && npm test -- src/components/shared/applicationTypeDefaults.test.ts`
- App form: `cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx`
- Whole suite + type gate: `cd gateway/frontend && npm test && npx tsc --noEmit`
- Filter by name within a file: append `-t "API-variant"` etc.

---

## 13. Open questions surfaced (align with spec §13)

- **Field-label keys.** This plan adds fresh `applicationResponsesMode` /
  `applicationMessagesMode` and leaves the old `applicationNativeResponses` /
  `applicationNativeMessages` (whose text says "nativ durchreichen", now
  inaccurate as a dropdown label) inert. The spec §5.2/§13 lists "reuse vs fresh
  keys" as undecided — if the reviewer prefers reuse, rewrite the two existing
  keys' text to the neutral API name and drop the two fresh keys. Pick one before
  Task 1b so the component's `label={...}` targets the right key.
- **`applicationNativeLegend` / note fate.** Now orphaned (legend) / reused
  (note). Decide whether to delete the legend key from both locales or keep it.
