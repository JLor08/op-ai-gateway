# GPU Selection — Plan Material: Area 04 Frontend

Area: Frontend — GPU-row reorder UI (drag + up/down), the `visible_devices_mode`
control, the two portal hints, TS types, i18n.

All paths are under the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/gpu-selection`.
Line numbers below are from the ACTUAL files read on branch `gpu-selection`.

Test framework: **Vitest + @testing-library/react** (jsdom). Run one file with
`cd gateway/frontend && npm test -- src/components/<File>.test.tsx`
(`npm test` = `vitest run`). Typecheck/build with
`cd gateway/frontend && npm run build` (`tsc && vite build`; tsc is the i18n /
type parity gate).

---

## 0. Key decisions resolved by reading the code (read these first)

1. **Agent host OS is ALREADY in the frontend — no `RuntimeReport.agent_os`
   needed.** `RuntimeAdminSection` already fetches the hardware report
   (`RuntimeAdminSection.tsx:1394` `const hardware = useLatestFetch(() =>
   api.serverHardware(server.id) …)`) and `HardwareReport.os` exists
   (`gateway/frontend/src/api/servers.ts:350` `os: string`). So the
   "Metal on non-macOS" hint reads `hardware.data?.report?.os` — the task's
   suggested `agent_os?: string` addition to `RuntimeReport` (runtime.ts) is
   **not required** and the backend area does not need a DTO change for it.
   **DEVIATION FLAG for the plan author:** drop the `RuntimeReport.agent_os`
   /`ServerRuntimeReportViewDTO.os` plumbing from scope unless another area
   wants it; the frontend consumes `HardwareReport.os` instead.

2. **`HardwareReport.os` is NOT a clean `"darwin"`.** The agent overwrites
   `runtime.GOOS` with gopsutil's platform string:
   `server-agent/internal/collector/hwinfo.go:36` sets `OS: runtime.GOOS` but
   `:59-62` overrides it with `hi.Platform + " " + hi.PlatformVersion`, e.g.
   `"darwin 15.1"` on macOS, `"ubuntu 22.04"` on Linux, `"Microsoft Windows 11
   …"` on Windows. So the macOS test must be a **case-insensitive substring**,
   not equality. Proposed helper: `isMacOsAgent(os) =>
   /darwin|mac ?os/i.test(os)`. (`makeHardware` in the test currently hardcodes
   `os: 'linux'` — see §GOTCHAS.)

3. **GPU rows stay `GpuRow[]` objects keyed by `rowKey`.** `moveColumn` /
   `useColumnDrag` operate on `string[]` of ids, so we reorder the **rowKey
   list** and map back to `GpuRow[]` (helper `reorderGpuRows` in §2.1). The TS
   `RuntimeSpecGPU` (runtime.ts:21-25) gains **nothing** — array order is the
   contract, exactly as the spec states.

4. **`SelectField` is a MUI (non-native) Select** (`components/shared/
   SelectField.tsx:33`, `native:false` at `:68`). Test idiom is
   `fireEvent.mouseDown(getByRole('combobox',{name}))` then
   `fireEvent.click(await findByRole('option',{name}))`; read current value with
   `getByRole('combobox',{name}).toHaveTextContent(optionLabel)` (confirmed idiom
   at e.g. `Activity.timeseries.test.tsx:167-168`, and
   `RuntimeAdminSection.test.tsx:791`).

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 `gateway/frontend/src/api/runtime.ts`

`RuntimeSpecGPU` — **unchanged** (kept here for reference), runtime.ts:21-25:
```ts
export interface RuntimeSpecGPU {
  index: number;
  vram_estimate_mb: number;
  vram_measured_mb: number;
}
```

`RuntimeSpec` — the `set_visible_devices` neighbourhood is where the new field
goes, runtime.ts:57-58:
```ts
  set_visible_devices: boolean;
  gpus: RuntimeSpecGPU[];
```
`PutRuntimeSpecRequest` is derived, runtime.ts:75:
```ts
export type PutRuntimeSpecRequest = Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>;
```
`RuntimeReport`, runtime.ts:305-312 (only touch if OS plumbing is added — see
decision #1; recommended: leave as-is):
```ts
export interface RuntimeReport {
  available: boolean;
  collected_at?: string;
  updated_at?: string;
  report?: RuntimeReportContent | null;
  agent_version: string;
  agent_features: string[];
}
```

### 1.2 `gateway/frontend/src/components/RuntimeAdminSection.tsx`

`GpuRow` type, :150-155:
```ts
type GpuRow = {
  rowKey: string;
  index: number;
  vramEstimateMb: number;
  vramMeasuredMb: number;
};
```
`makeRowKey`, :144-148:
```ts
let nextRowKey = 0;
function makeRowKey(): string {
  nextRowKey += 1;
  return `row-${nextRowKey}`;
}
```
`emptySpec` tail, :195-200 (the `set_visible_devices: false,` line is the anchor
for the new field default):
```ts
    vram_locked: false,
    set_visible_devices: false,
    gpus: [],
    api_flavors: [],
    responses_mode: 'passthrough',
    messages_mode: 'passthrough',
```
State declarations, :2046-2047:
```ts
  const [setVisibleDevices, setSetVisibleDevices] = useState(false);
  const [gpuRows, setGpuRows] = useState<GpuRow[]>([]);
```
`resetSpecFields` tail, :2123-2126 (create path zeroing):
```ts
    setVramLocked(false);
    setSetVisibleDevices(false);
    setGpuRows([]);
  }
```
`hydrateSpecFields` GPU/visibility block, :2143-2151:
```ts
    setSetVisibleDevices(spec.set_visible_devices);
    setGpuRows(
      spec.gpus.map((g) => ({
        rowKey: makeRowKey(),
        index: g.index,
        vramEstimateMb: g.vram_estimate_mb,
        vramMeasuredMb: g.vram_measured_mb,
      })),
    );
```
`addGpuRow` / `removeGpuRow` / `updateGpuRow`, :2208-2230:
```ts
  function addGpuRow() {
    setGpuRows((rows) => {
      const used = new Set(rows.map((r) => r.index));
      let index = 0;
      while (used.has(index)) index++;
      return [...rows, { rowKey: makeRowKey(), index, vramEstimateMb: 0, vramMeasuredMb: 0 }];
    });
  }
  function removeGpuRow(idx: number) {
    setGpuRows((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateGpuRow(idx: number, patch: Partial<Pick<GpuRow, 'index' | 'vramEstimateMb'>>) {
    setGpuRows((rows) => rows.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  }
```
`buildSpecBody` GPU/visibility block, :2392-2400:
```ts
      set_visible_devices: setVisibleDevices,
      gpus: gpuRows.map((r) => ({
        index: r.index,
        vram_estimate_mb: r.vramEstimateMb,
        vram_measured_mb: r.vramMeasuredMb,
      })),
      api_flavors: specApiFlavors,
      responses_mode: specResponsesMode,
      messages_mode: specMessagesMode,
```
The `set_visible_devices` checkbox + its hint, :3547-3558 (the new mode control
goes directly BELOW `runtimeSpecSetVisibleDevicesHint`):
```tsx
            <FormControlLabel
              control={
                <Checkbox
                  checked={setVisibleDevices}
                  onChange={(e) => setSetVisibleDevices(e.target.checked)}
                />
              }
              label={t.runtimeSpecSetVisibleDevices}
            />
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecSetVisibleDevicesHint}
            </Typography>
```
The GPU rows JSX block, :3572-3691 — the map at :3588 is the reorder target
(each row is a `<Box key={row.rowKey} sx={{ display:'flex', gap:1.5,
alignItems:'center', flexWrap:'wrap' }}>` with a "Reported GPU" SelectField, a
GPU-index `Field`, a VRAM `Field`, the measured-VRAM text, `renderVramApply`, a
Remove `Button`, `renderVramCardCheck`). The Add button is at :3686-3690:
```tsx
              {gpuRows.map((row, idx) => (
                <Box
                  key={row.rowKey}
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                >
                  … (pick select, index field, vram field, measured, apply, remove) …
                </Box>
              ))}
              …
              <Box>
                <Button type="button" variant="outlined" size="small" onClick={addGpuRow}>
                  {t.runtimeSpecGpuAdd}
                </Button>
              </Box>
```
Report-derived features/version, :1702-1704:
```ts
  const reportReady = reportStatus === 'ready';
  const agentFeatures = reportReady ? (reportData?.agent_features ?? []) : [];
  const agentVersion = reportReady ? (reportData?.agent_version ?? '') : '';
```
`featureMismatch` derivation, :1716-1724, and its banner, :3785-3797 (the
**pattern to mirror** for the two new hints — an `<Alert severity="warning">`
that lists `agentVersion`/`agentFeatures`):
```ts
  const runtimeSilent =
    reportStatus === 'ready' && configuredSpecCount > 0 && statusRows.length === 0;
  …
  const featureMismatch =
    runtimeSilent && !agentNeverReported && !agentFeatures.includes('runtime_manager');
```
Hardware / OS source already present, :1394-1396:
```ts
  const hardware = useLatestFetch(() => api.serverHardware(server.id), [api, server.id]);
  const telemetryGpus: HardwareGPU[] =
    hardware.data?.available && hardware.data.report ? hardware.data.report.gpus : [];
```

### 1.3 House reorder pattern (reuse — exact signatures)

`gateway/frontend/src/components/shared/columnDrag.ts`:
```ts
export type DragPlace = 'before' | 'after';
export function moveColumn(order: string[], sourceId: string, targetId: string, place: DragPlace): string[]
export function useColumnDrag(
  onReorder: (sourceId: string, targetId: string, place: DragPlace) => void,
  orientation: 'horizontal' | 'vertical' = 'horizontal',
): { dragProps: (colId: string) => {...}, draggingId: string|null, overId: string|null, overPlace: DragPlace, clear: () => void }
export function columnDragSx(colId, draggingId, overId, overPlace, orientation='horizontal'): CSSObject
```
`gateway/frontend/src/components/shared/OrderedMemberList.tsx` — the swap +
drag + up/down template (:44-55, :96-133):
```ts
const { dragProps, draggingId, overId, overPlace } = useColumnDrag(
  (source, target, place) => onChange(moveColumn(members, source, target, place)),
  'vertical',
);
function swap(index: number, delta: number) {
  const target = index + delta;
  if (target < 0 || target >= members.length) return;
  const next = [...members];
  [next[index], next[target]] = [next[target], next[index]];
  onChange(next);
}
```
Row shell (drag props + sx + up/down IconButtons, disabled at ends), :80-133:
```tsx
<Box component="li" key={name} {...dragProps(name)}
  sx={{ display:'flex', alignItems:'center', gap:0.5, px:1, py:0.5,
        border:'1px solid var(--line)', borderRadius:1,
        ...columnDragSx(name, draggingId, overId, overPlace, 'vertical') }}>
  <DragIndicatorIcon fontSize="small" sx={{ color:'text.secondary', cursor:'grab' }} aria-hidden />
  …
  <IconButton size="small" aria-label={`${t.modelGroupMoveUp}: ${name}`}
    disabled={disabled || index === 0} onClick={() => swap(index, -1)}>
    <ArrowUpwardIcon fontSize="small" />
  </IconButton>
  <IconButton size="small" aria-label={`${t.modelGroupMoveDown}: ${name}`}
    disabled={disabled || index === members.length - 1} onClick={() => swap(index, 1)}>
    <ArrowDownwardIcon fontSize="small" />
  </IconButton>
</Box>
```
Icon imports to copy (OrderedMemberList.tsx:5-7):
```ts
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
```

### 1.4 i18n (parity is compile-enforced)

`gateway/frontend/src/i18n.ts`: `const de = {…} satisfies Record<string,
PortalMessageValue>` (:2114); `export type PortalMessages = typeof de` (:2116);
`const en: PortalMessages = {…}` (:2118); `export const messages: Record<Locale,
PortalMessages> = { de, en }` (:4152). Because `en` is typed as `typeof de`, a
key present in one locale and not the other is a **tsc error** — every new key
must be added to BOTH `de` and `en`.

Existing keys in play:
- `runtimeSpecSetVisibleDevices` (de :595, en :2676) + `…Hint` (de :596, en :2677)
- GPU block `runtimeSpecGpus` (de :503/en :2597), `runtimeSpecGpuIndex` (de
  :621/en :2694), `runtimeSpecGpuPick` (de :622/en :2695),
  `runtimeSpecGpuPickPlaceholder` (de :623/en :2696), `runtimeSpecGpuNoTelemetry`
  (de :624/en :2697), `runtimeSpecGpuAdd` (de :626/en :2699),
  `runtimeSpecGpuRemove` (de :627/en :2700)
- `modelGroupMoveUp` / `modelGroupMoveDown` (de :810-811, en :2880-2881) —
  values `'Nach oben'`/`'Nach unten'` and `'Move up'`/`'Move down'`.

`Translation` = `(typeof messages)['de']`; `MessageKey` = string-valued keys only
(`components/shared/types.ts:6-13`).

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized, with real code)

> Fixture note used by every task below: `makeSpec`
> (`RuntimeAdminSection.test.tsx:134-159`) and `makeReport` (:206-208) get one
> new field each in **Task 1** and **Task 3** respectively; later tasks assume
> those are in place.

### Task 1 — TS type + fixture: `RuntimeSpec.visible_devices_mode`

**Why first:** every other frontend change references the field; tsc must know it.

Edit `gateway/frontend/src/api/runtime.ts` — add after `set_visible_devices`
(runtime.ts:57), inside `RuntimeSpec`:
```ts
  // env: agent injects the visibility env var (today's behaviour, order-
  // preserving). args: agent injects nothing; the operator writes a
  // ${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES} placeholder into args.
  // Only meaningful when set_visible_devices is on; default 'env'. Mirrors the
  // Go RuntimeSpecDTO.visible_devices_mode.
  visible_devices_mode: 'env' | 'args';
```
`PutRuntimeSpecRequest` (Omit) picks it up automatically.

Edit `RuntimeAdminSection.tsx` `emptySpec` (after :195 `set_visible_devices:
false,`):
```ts
    set_visible_devices: false,
    visible_devices_mode: 'env',
```

Edit the test fixture `makeSpec` (RuntimeAdminSection.test.tsx:152-153):
```ts
    set_visible_devices: false,
    visible_devices_mode: 'env',
```

**Failing test (round-trip default is `'env'` in the create PUT).** Add to the
`describe('RuntimeAdminSection create (mapping + spec)')` block (after the
`api_flavors` test at :796-817):
```ts
  it('defaults visible_devices_mode to env in the spec PUT body', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.visible_devices_mode).toBe('env');
  });
```
Run: `cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx`
Expected: **FAIL** — `buildSpecBody` does not yet emit the field
(`undefined !== 'env'`), and until the wiring exists tsc/build also fails.

**Minimal implementation:** add state + wire into build/hydrate/reset.
- State next to `setVisibleDevices` (RuntimeAdminSection.tsx:2046-2047):
```ts
  const [setVisibleDevices, setSetVisibleDevices] = useState(false);
  const [visibleDevicesMode, setVisibleDevicesMode] = useState<'env' | 'args'>('env');
```
- `buildSpecBody` (after :2392 `set_visible_devices: setVisibleDevices,`):
```ts
      set_visible_devices: setVisibleDevices,
      visible_devices_mode: visibleDevicesMode,
```
- `hydrateSpecFields` (after :2143):
```ts
    setSetVisibleDevices(spec.set_visible_devices);
    setVisibleDevicesMode(spec.visible_devices_mode);
```
- `resetSpecFields` (after :2124 `setSetVisibleDevices(false);`):
```ts
    setSetVisibleDevices(false);
    setVisibleDevicesMode('env');
```
Run again → **PASS**. `npm run build` → tsc clean.

---

### Task 2 — GPU-row reorder (drag + up/down), PUT sends the new order

**Failing test.** New describe block in RuntimeAdminSection.test.tsx (place near
the other create/edit spec tests). It opens the edit form of a spec with GPUs
`[0,1,2]`, moves GPU 0 down once via the up/down control, saves, and asserts the
PUT `gpus` order:
```ts
describe('RuntimeAdminSection GPU-row ordering', () => {
  it('reorders GPU rows (move down) and sends the new gpus order in the PUT', async () => {
    const { putSpecs } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true,
          id: 'spec_1',
          mapping_id: 'map_1',
          gpus: [
            { index: 0, vram_estimate_mb: 1000, vram_measured_mb: 0 },
            { index: 1, vram_estimate_mb: 2000, vram_measured_mb: 0 },
            { index: 2, vram_estimate_mb: 3000, vram_measured_mb: 0 },
          ],
        }),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    // Move the first GPU row down one slot: 0,1,2 -> 1,0,2
    fireEvent.click(await screen.findByRole('button', { name: `${t.modelGroupMoveDown}: GPU 0` }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.gpus.map((g) => g.index)).toEqual([1, 0, 2]);
  });
});
```
(Edit-trigger label confirmed: the specs-list edit button is
`t.runtimeSpecEditAction` — existing edit tests open with
`screen.findByRole('button', { name: t.runtimeSpecEditAction })`, e.g.
RuntimeAdminSection.test.tsx:975,1041,1388. `t.save` is the edit-form submit at
RuntimeAdminSection.tsx:3695. NOTE `t.runtimeSpecEdit` is only the breadcrumb/
title, not a button.)

Run: `npm test -- src/components/RuntimeAdminSection.test.tsx`
Expected: **FAIL** — there is no move-down button (`findByRole` times out).

**Minimal implementation.**
1. New icon imports in RuntimeAdminSection.tsx (top of the MUI-icon imports):
```ts
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
```
and the reorder helpers:
```ts
import { useColumnDrag, columnDragSx, moveColumn } from './shared/columnDrag';
```
2. Reorder helpers next to `updateGpuRow` (~:2230):
```ts
  // Reorder the GPU rows by rowKey (moveColumn works on a string[] of ids;
  // rebuild the GpuRow[] in the new key order). Array order is the wire
  // contract (buildSpecBody sends gpuRows verbatim), so this is the whole of
  // Part A on the client.
  function reorderGpuRows(sourceKey: string, targetKey: string, place: 'before' | 'after') {
    setGpuRows((rows) => {
      const order = moveColumn(rows.map((r) => r.rowKey), sourceKey, targetKey, place);
      return order.map((k) => rows.find((r) => r.rowKey === k)!);
    });
  }
  function swapGpuRow(idx: number, delta: number) {
    setGpuRows((rows) => {
      const target = idx + delta;
      if (target < 0 || target >= rows.length) return rows;
      const next = [...rows];
      [next[idx], next[target]] = [next[target], next[idx]];
      return next;
    });
  }
```
3. Instantiate the drag hook inside the component body (near the other GPU-row
   helpers, top-level of `RuntimeAdminSection`, NOT inside JSX):
```ts
  const gpuDrag = useColumnDrag(reorderGpuRows, 'vertical');
```
4. In the GPU rows map (RuntimeAdminSection.tsx:3588-3592), spread the drag
   props + sx on each row `<Box>` and prepend a drag handle + the two arrows.
   The row becomes:
```tsx
              {gpuRows.map((row, idx) => (
                <Box
                  key={row.rowKey}
                  {...gpuDrag.dragProps(row.rowKey)}
                  sx={{
                    display: 'flex',
                    gap: 1.5,
                    alignItems: 'center',
                    flexWrap: 'wrap',
                    ...columnDragSx(
                      row.rowKey,
                      gpuDrag.draggingId,
                      gpuDrag.overId,
                      gpuDrag.overPlace,
                      'vertical',
                    ),
                  }}
                >
                  <DragIndicatorIcon
                    fontSize="small"
                    sx={{ color: 'text.secondary', cursor: 'grab' }}
                    aria-hidden
                  />
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveUp}: GPU ${row.index}`}
                    disabled={idx === 0}
                    onClick={() => swapGpuRow(idx, -1)}
                  >
                    <ArrowUpwardIcon fontSize="small" />
                  </IconButton>
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveDown}: GPU ${row.index}`}
                    disabled={idx === gpuRows.length - 1}
                    onClick={() => swapGpuRow(idx, 1)}
                  >
                    <ArrowDownwardIcon fontSize="small" />
                  </IconButton>
                  {/* existing: pick select, index field, vram field, measured,
                      renderVramApply, remove button, renderVramCardCheck */}
                  … unchanged row contents …
                </Box>
              ))}
```
`IconButton` is already imported (RuntimeAdminSection.tsx:12). The three
`@mui/icons-material` imports (DragIndicator/ArrowUpward/ArrowDownward) are new.

Run again → **PASS**.

**Note on the aria-label:** using `GPU ${row.index}` (not `rowKey`) makes the
label operator-meaningful and testable, matching OrderedMemberList's
`${t.modelGroupMoveUp}: ${name}`. If two rows share an index mid-edit the labels
collide — acceptable for a hint control, but the reorder test above picks a spec
whose indices are distinct so `findByRole('button',{name:'…GPU 0'})` is
unambiguous.

---

### Task 3 — `visible_devices_mode` SelectField (shown when the checkbox is on) + round-trip

**Failing test 3a (control appears and round-trips to args).**
```ts
describe('RuntimeAdminSection visibility mode', () => {
  it('shows the mode select only when set_visible_devices is on and round-trips args', async () => {
    const { putSpecs } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true,
          id: 'spec_1',
          mapping_id: 'map_1',
          set_visible_devices: true,
          visible_devices_mode: 'env',
          args: ['--device', '${CUDA_DEVICES}'],
          gpus: [{ index: 0, vram_estimate_mb: 1000, vram_measured_mb: 0 }],
        }),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    // Control is present because the checkbox is on, and shows the stored 'env'.
    const combo = screen.getByRole('combobox', { name: t.runtimeSpecVisibleDevicesMode });
    expect(combo).toHaveTextContent(t.runtimeSpecVisibleDevicesModeEnv);
    // Switch to args (MUI Select idiom).
    fireEvent.mouseDown(combo);
    fireEvent.click(await screen.findByRole('option', { name: t.runtimeSpecVisibleDevicesModeArgs }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.visible_devices_mode).toBe('args');
  });

  it('hides the mode select when set_visible_devices is off', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, id: 'spec_1', mapping_id: 'map_1',
          set_visible_devices: false }),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    expect(
      screen.queryByRole('combobox', { name: t.runtimeSpecVisibleDevicesMode }),
    ).not.toBeInTheDocument();
  });
});
```
Run: expected **FAIL** (`t.runtimeSpecVisibleDevicesMode` is `undefined` → tsc
error first; after the i18n keys land in Task 5, the control still does not
render → runtime FAIL).

**Recommended task order in practice:** land the **i18n keys (Task 5)** and the
type field (Task 1) before running Task 3 so tsc is green while iterating; the
failing assertion that drives the impl is "combobox not found / PUT not 'args'".

**Minimal implementation** — insert directly under the
`runtimeSpecSetVisibleDevicesHint` caption (RuntimeAdminSection.tsx:3558):
```tsx
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecSetVisibleDevicesHint}
            </Typography>
            {setVisibleDevices && (
              <>
                <SelectField
                  id="runtime-spec-visible-devices-mode"
                  label={t.runtimeSpecVisibleDevicesMode}
                  value={visibleDevicesMode}
                  onChange={(e) => setVisibleDevicesMode(e.target.value as 'env' | 'args')}
                  sx={{ maxWidth: 340 }}
                >
                  <option value="env">{t.runtimeSpecVisibleDevicesModeEnv}</option>
                  <option value="args">{t.runtimeSpecVisibleDevicesModeArgs}</option>
                </SelectField>
                {visibleDevicesMode === 'args' && (
                  <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
                    {t.runtimeSpecVisibleDevicesModeArgsHint}
                  </Typography>
                )}
              </>
            )}
```
Run → **PASS**. (`SelectField` already imported at RuntimeAdminSection.tsx:56.)

---

### Task 4 — the two portal hints (agent too old; Metal on non-macOS)

Both are HINTS: they never block Save; they mirror the `featureMismatch`
`<Alert severity="warning">` pattern (RuntimeAdminSection.tsx:3785-3797). Render
them **inside the spec form**, just above/below the GPU block, since they depend
on form state (`visibleDevicesMode`, `gpuRows` order, `argsText`).

Derived values — add near the other derived form values in the component body:
```ts
  // Part A/B agent-capability + platform hints (non-blocking). agentFeatures /
  // agentVersion are the report-derived values (:1703-1704); the host OS comes
  // from the hardware report already fetched (:1394-1396) — NOT a new DTO field.
  const agentHasGpuSelection = agentFeatures.includes('gpu_selection');
  const gpuOrderIsCustom = gpuRows.some((r, i, all) => i > 0 && r.index < all[i - 1].index);
  const argsHaveMetalDevices = parseArgsText(argsText).some((a) => a.includes('${METAL_DEVICES}'));
  const agentOs = hardware.data?.available && hardware.data.report ? hardware.data.report.os : '';
  const agentOsKnown = agentOs !== '';
  const isMacOsAgent = /darwin|mac ?os/i.test(agentOs);
  // Prominent when args mode (the process would fail to start on an old agent);
  // informational when only the order is custom (order is ignored until >=0.4.0).
  const showAgentTooOldArgs =
    setVisibleDevices && visibleDevicesMode === 'args' && reportReady && !agentHasGpuSelection;
  const showAgentTooOldOrder =
    !showAgentTooOldArgs && gpuOrderIsCustom && reportReady && !agentHasGpuSelection;
  const showMetalNonMacos =
    setVisibleDevices &&
    visibleDevicesMode === 'args' &&
    argsHaveMetalDevices &&
    agentOsKnown &&
    !isMacOsAgent;
```
JSX (place at the top of the GPU `<Box sx={{ display:'grid', gap:1 }}>` block,
i.e. right after RuntimeAdminSection.tsx:3574 `{t.runtimeSpecGpus}` heading, or
directly under the mode select):
```tsx
              {showAgentTooOldArgs && (
                <Alert severity="warning">
                  {`${t.runtimeSpecAgentTooOldArgs} (${t.runtimeAgentVersion}: ${agentVersion || '—'})`}
                </Alert>
              )}
              {showAgentTooOldOrder && (
                <Alert severity="info">
                  {`${t.runtimeSpecAgentTooOldOrder} (${t.runtimeAgentVersion}: ${agentVersion || '—'})`}
                </Alert>
              )}
              {showMetalNonMacos && (
                <Alert severity="warning">{t.runtimeSpecMetalNonMacos}</Alert>
              )}
```
`Alert` is already imported (used at :3776, :3785). `parseArgsText`
(RuntimeAdminSection.tsx:516) and `reportReady`/`agentVersion`/`agentFeatures`
(:1702-1704) already exist.

**Failing test 4a — args-mode + old agent → prominent hint.**
```ts
  it('warns prominently when args mode is on and the agent lacks gpu_selection', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true, id: 'spec_1', mapping_id: 'map_1',
          set_visible_devices: true, visible_devices_mode: 'args',
          args: ['--device', '${CUDA_DEVICES}'],
          gpus: [{ index: 0, vram_estimate_mb: 1000, vram_measured_mb: 0 }],
        }),
      },
      report: makeReport({ agent_version: '0.3.0', agent_features: ['runtime_manager'] }),
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    expect(await screen.findByText(new RegExp(t.runtimeSpecAgentTooOldArgs))).toBeInTheDocument();
  });
```
**Failing test 4b — Metal placeholder + non-macOS agent → hint.**
```ts
  it('warns that ${METAL_DEVICES} needs a macOS host when the agent OS is not darwin', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true, id: 'spec_1', mapping_id: 'map_1',
          set_visible_devices: true, visible_devices_mode: 'args',
          args: ['--device', '${METAL_DEVICES}'],
          gpus: [{ index: 0, vram_estimate_mb: 1000, vram_measured_mb: 0 }],
        }),
      },
      // makeHardware(...) reports os:'linux' (non-macOS) — see GOTCHAS.
      hardware: makeHardware([{ index: 0, name: 'Card', memory_total_bytes: 0 }]),
      report: makeReport({ agent_version: '0.4.0', agent_features: ['runtime_manager', 'gpu_selection'] }),
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    expect(await screen.findByText(t.runtimeSpecMetalNonMacos)).toBeInTheDocument();
  });
```
**Suppression test (macOS agent → no hint)** — needs a `darwin` hardware
fixture; see GOTCHAS for extending `makeHardware`:
```ts
  it('does not warn about Metal when the agent OS is darwin', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true, id: 'spec_1', mapping_id: 'map_1',
          set_visible_devices: true, visible_devices_mode: 'args',
          args: ['--device', '${METAL_DEVICES}'],
          gpus: [{ index: 0, vram_estimate_mb: 1000, vram_measured_mb: 0 }],
        }),
      },
      hardware: makeHardware([{ index: 0, name: 'Apple M3', memory_total_bytes: 0 }], 'darwin 15.1'),
      report: makeReport({ agent_version: '0.4.0', agent_features: ['gpu_selection'] }),
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary); // form is open
    expect(screen.queryByText(t.runtimeSpecMetalNonMacos)).not.toBeInTheDocument();
  });
```
Run all: expected **FAIL** before impl (hints absent), **PASS** after.

---

### Task 5 — i18n keys (de + en) — parity is compile-enforced

Add to BOTH locales. Suggested placement: mode keys right after
`runtimeSpecSetVisibleDevicesHint` (de :596 / en :2677); hint keys near the GPU
block (after `runtimeSpecGpuRemove`, de :627 / en :2700). Reuse
`modelGroupMoveUp`/`modelGroupMoveDown` for the arrows (no new move keys needed).

`de`:
```ts
  runtimeSpecVisibleDevicesMode: 'Sichtbarkeit erzwingen über',
  runtimeSpecVisibleDevicesModeEnv: 'Umgebungsvariable',
  runtimeSpecVisibleDevicesModeArgs: 'Argumente (--device)',
  runtimeSpecVisibleDevicesModeArgsHint:
    'Im Argument-Modus setzt der Agent keine Sichtbarkeits-Variable. Schreiben Sie stattdessen einen Geräte-Platzhalter in die Argumente – je nach llama.cpp-Build ${CUDA_DEVICES}, ${VULKAN_DEVICES} oder ${METAL_DEVICES}. Der Platzhalter wird zu den ausgewählten Karten in der angegebenen Reihenfolge expandiert (z. B. CUDA2,CUDA3); mindestens einer der drei muss vorhanden sein.',
  runtimeSpecAgentTooOldArgs:
    'Der verbundene Agent kann den Geräte-Platzhalter nicht auflösen (benötigt Version 0.4.0 oder neuer) – der Prozess würde nicht starten.',
  runtimeSpecAgentTooOldOrder:
    'Die gewählte GPU-Reihenfolge wird ignoriert, bis der Agent auf Version 0.4.0 oder neuer aktualisiert ist.',
  runtimeSpecMetalNonMacos:
    '${METAL_DEVICES} funktioniert nur auf einem macOS-Host; der verbundene Agent läuft auf einem anderen Betriebssystem.',
```
`en`:
```ts
  runtimeSpecVisibleDevicesMode: 'Enforce visibility via',
  runtimeSpecVisibleDevicesModeEnv: 'Environment variable',
  runtimeSpecVisibleDevicesModeArgs: 'Arguments (--device)',
  runtimeSpecVisibleDevicesModeArgsHint:
    'In arguments mode the agent sets no visibility variable. Instead write a device placeholder into the args — ${CUDA_DEVICES}, ${VULKAN_DEVICES} or ${METAL_DEVICES}, depending on your llama.cpp build. It expands to the selected cards in the chosen order (e.g. CUDA2,CUDA3); at least one of the three must be present.',
  runtimeSpecAgentTooOldArgs:
    'The connected agent cannot expand the device placeholder (needs version 0.4.0 or newer) — the process would fail to start.',
  runtimeSpecAgentTooOldOrder:
    'The chosen GPU order is ignored until the agent is updated to version 0.4.0 or newer.',
  runtimeSpecMetalNonMacos:
    '${METAL_DEVICES} only works on a macOS host; the connected agent runs on a different operating system.',
```
Run: `cd gateway/frontend && npm run build` → tsc must be clean (missing key in
either locale is a compile error; excess-property is too). Then re-run the file's
tests.

---

## 3. INTERFACES

### PRODUCES (this area is the source of truth for)
- **TS `RuntimeSpec.visible_devices_mode: 'env' | 'args'`** (`api/runtime.ts`) —
  DTO json key `visible_devices_mode`; must match backend `RuntimeSpecDTO` /
  `PutRuntimeSpecRequest` / `AgentRuntimeSpecDTO` (backend area) and
  `routing.RuntimeSpec.VisibleDevicesMode`. Default `'env'`.
- **GPU order = array order** in the PUT `gpus` (`buildSpecBody`), unchanged
  shape `{index, vram_estimate_mb, vram_measured_mb}`. Consumed by the backend
  (`putRuntimeSpec` sets `Position = i`).
- New i18n keys (both locales): `runtimeSpecVisibleDevicesMode`,
  `runtimeSpecVisibleDevicesModeEnv`, `runtimeSpecVisibleDevicesModeArgs`,
  `runtimeSpecVisibleDevicesModeArgsHint`, `runtimeSpecAgentTooOldArgs`,
  `runtimeSpecAgentTooOldOrder`, `runtimeSpecMetalNonMacos`.
- Reused (no new keys): `modelGroupMoveUp` / `modelGroupMoveDown` for the GPU
  up/down arrows.
- Reused component API: `moveColumn`, `useColumnDrag('vertical')`, `columnDragSx`
  (`components/shared/columnDrag.ts`); the OrderedMemberList swap/arrow pattern.

### CONSUMES (produced by other areas / already present)
- **Agent feature string `'gpu_selection'`** — the hints test
  `agentFeatures.includes('gpu_selection')`. Comes from the agent's
  `agent.Features` `{Name:"gpu_selection", Since:"0.4.0"}` → telemetry →
  `RuntimeReport.agent_features` (already plumbed).
- **`RuntimeReport.agent_version` / `agent_features`** (runtime.ts:310-311) —
  already present; no change.
- **`HardwareReport.os`** (`api/servers.ts:350`) via `api.serverHardware` — the
  agent host OS, ALREADY fetched at RuntimeAdminSection.tsx:1394. Value is a
  gopsutil platform string (e.g. `"darwin 15.1"`, `"ubuntu 22.04"`), NOT a bare
  GOOS token.
- Agent placeholders the args-mode hint text names (agent area produces the
  expansion): `${CUDA_DEVICES}` / `${VULKAN_DEVICES}` / `${METAL_DEVICES}` →
  `<Backend><index>,…` (`CUDA`/`Vulkan`/`MTL`) in operator order, deduped.
- Backend error sentinels (gateway area; not surfaced as new frontend strings,
  they arrive through the existing portal-error channel):
  `runtime_spec.visible_devices_mode_invalid`,
  `runtime_spec.visible_devices_args_no_placeholder` (both HTTP 400). If the plan
  wants friendly frontend copy for the args-no-placeholder 400, add a
  `formatPortalError` mapping — but the client-side `showAgentTooOldArgs` hint and
  the backend validation already cover the case; treat frontend copy as optional.

### DEVIATIONS from the task's canonical names (flag for plan author)
- **No `RuntimeReport.agent_os` / `ServerRuntimeReportViewDTO.os` needed.** OS is
  already available via `HardwareReport.os`. Recommend dropping the OS DTO
  plumbing from the backend/portal scope unless another consumer wants it.
- The macOS check is a **substring** (`/darwin|mac ?os/i`), because
  `HardwareReport.os` is `"darwin 15.1"`-style, not `"darwin"`. `agent_os !==
  'darwin'` (as literally written in the task) would misfire.

---

## 4. GOTCHAS

- **Run/build.** `cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx`
  (single file; `npm test` = `vitest run`). Typecheck + i18n parity gate:
  `npm run build` (`tsc && vite build`). Both must be green before PR.
- **i18n parity is compile-time.** `en: PortalMessages = typeof de`
  (i18n.ts:2116-2118). Add every new key to BOTH `de` and `en` or tsc fails.
  Function-valued keys are excluded from `MessageKey`; all keys here are plain
  strings, fine for `t[key]`/`toHaveTextContent`.
- **MUI Select, not native.** `SelectField` renders a non-native MUI Select
  (`native:false`, SelectField.tsx:68). In tests: open with
  `fireEvent.mouseDown(getByRole('combobox',{name:label}))`, pick with
  `fireEvent.click(await findByRole('option',{name:optionLabel}))`; read with
  `getByRole('combobox',{name}).toHaveTextContent(optionLabel)`. Idiom proven at
  `Activity.timeseries.test.tsx:167-168`, `RuntimeAdminSection.test.tsx:791`.
  The combobox's accessible name is its `label` prop, so `t.runtimeSpecVisibleDevicesMode`
  is the `{name}` — distinct from the per-row `t.runtimeSpecGpuPick` selects.
- **`makeHardware` hardcodes `os:'linux'`** (RuntimeAdminSection.test.tsx:164-179,
  the `os` is at :171). The non-macOS Metal test works as-is. The macOS
  suppression test needs a `darwin` OS, so extend the helper to take an optional
  os:
  ```ts
  function makeHardware(gpus: HardwareGPU[], os = 'linux'): HardwareResponse {
    return { available: true, report: { …, os, … } };  // set report.os = os
  }
  ```
  All existing call sites (they pass one arg) keep `'linux'`.
- **`reportReady` gating.** `agentFeatures`/`agentVersion` are `[]`/`''` until
  `reportStatus === 'ready'` (RuntimeAdminSection.tsx:1702-1704). Gate the
  agent-too-old hints on `reportReady` (as shown) so they do not flash "too old"
  during the report fetch. The default `makeReport()` has
  `agent_features: []` — a test that must NOT show the "too old" hint should pass
  a report whose features include `'gpu_selection'`, or leave the form in `env`
  mode with an ascending GPU order.
- **`gpuOrderIsCustom` heuristic.** `gpuRows.some((r,i,all)=> i>0 && r.index <
  all[i-1].index)` = "not strictly ascending by index". After the backfill
  migration, an untouched spec loads ascending (position = gpu_index rank), so the
  informational order-hint only fires once an operator actually reorders — which
  matches the design's "no existing spec changes on upgrade" property. This is a
  hint only; do not gate Save on it.
- **Row identity for drag.** Reorder by `row.rowKey` (stable, unique per page
  load — makeRowKey, :144-148), never by array index or GPU index (indices may
  collide mid-edit, per the `updateGpuRow` comment :2223-2227). `moveColumn`
  bails safely when source===target or an id is missing (columnDrag.ts:53-54).
- **Keep `duplicateGpuIndex` submit check.** Reordering does not change the
  submit-time duplicate-index guard (`submitCreate` :2417-2419); leave it.
- **buildSpecBody sends `gpuRows` verbatim** (:2393-2397) — once the rows are
  reordered in state, the PUT order follows for free. That is the entire
  client-side of Part A; no explicit `position` field on the wire.
- **Spec ambiguity resolved:** design §10 asks "is agent os already surfaced?"
  → YES, `HardwareReport.os`, already fetched (decision #1). And "SelectField or
  radio?" → **SelectField** (matches the app's standard for a small enum, and
  the existing GPU-pick / admin-state / responses-mode controls in this very
  form are all `SelectField`).
- **Edit-form opener label.** Tests above use `t.runtimeSpecEditAction` (the
  specs-list Edit button — used by existing tests at
  RuntimeAdminSection.test.tsx:975,1041,1388) and `t.save` (the edit submit,
  RuntimeAdminSection.tsx:3695). `t.runtimeSpecEdit` is the breadcrumb/title
  only, not a button — do not use it as a `findByRole('button')` name.
  `IconButton` is already imported (RuntimeAdminSection.tsx:12).
