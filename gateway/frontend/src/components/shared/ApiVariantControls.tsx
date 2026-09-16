// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Checkbox, FormControlLabel, Typography } from '@mui/material';
import { CheckboxGroup } from './CheckboxGroup';
import { SelectField } from './SelectField';
import type { EndpointMode } from '../../api';
import { liveTimingsControlLayout, type LiveTimingsKind } from './liveTimings';
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
  liveTimings,
  liveTimingsKind,
  onFlavorsChange,
  onResponsesModeChange,
  onMessagesModeChange,
  onLiveTimingsChange,
}: Readonly<{
  t: Translation;
  apiFlavors: string[];
  responsesMode: EndpointMode;
  messagesMode: EndpointMode;
  // undefined = "no opinion": the FORM omits the key entirely and the backend
  // decides -- a first write takes the kind's own default, a later save keeps
  // the stored value. The launch-spec form needs that third state because
  // under Type "Auto" it cannot know the kind; the application form always
  // knows its `type` and never passes it.
  liveTimings: boolean | undefined;
  liveTimingsKind: LiveTimingsKind;
  onFlavorsChange: (flavors: string[]) => void;
  onResponsesModeChange: (mode: EndpointMode) => void;
  onMessagesModeChange: (mode: EndpointMode) => void;
  onLiveTimingsChange: (enabled: boolean) => void;
}>) {
  const openaiEnabled = apiFlavors.includes('openai');
  const anthropicEnabled = apiFlavors.includes('anthropic');
  // The exhaustive replacement for the plain-comparison suppression that once
  // let a fourth kind fall through to a working, do-nothing checkbox (issue #88).
  const liveTimingsLayout = liveTimingsControlLayout(liveTimingsKind);
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
      {liveTimingsLayout.checkbox ? (
        <>
          <FormControlLabel
            control={
              <Checkbox
                // `?? liveTimingsLayout.defaultChecked` is the honest rendering of
                // "no opinion", not a default: what the backend will apply for a
                // capable kind on a first write is ON, so the box says ON; for an
                // unknown kind nothing can be promised (defaultChecked is false)
                // and the caption below states the rule. Clicking either way
                // leaves a DEFINITE value, with deliberately no way back to "no
                // opinion" once the operator has said something.
                checked={liveTimings ?? liveTimingsLayout.defaultChecked}
                onChange={(e) => onLiveTimingsChange(e.target.checked)}
              />
            }
            label={t.applicationLiveTimings}
          />
          <Typography variant="caption" sx={{ color: 'text.secondary' }}>
            {liveTimingsLayout.note === 'auto'
              ? t.applicationLiveTimingsAutoNote
              : t.applicationLiveTimingsNote}
          </Typography>
        </>
      ) : (
        // Never a blank: the slot the checkbox would occupy still explains why
        // there is nothing to set. Rendering a DISABLED checkbox instead would be
        // worse -- it would show a value (ticked or not) that this form will not
        // send, since neither suppressed kind sends the key at all.
        //
        // Two kinds, two SENTENCES. `delegated` (server_agent) must not get the
        // unsupported note: the flag is real for such an application, it is simply
        // decided on the runtime spec instead -- and since runtime specs exist
        // only under server_agent, that note would tell precisely the operators
        // who CAN use this feature that they cannot.
        <Typography variant="caption" sx={{ color: 'text.secondary' }}>
          {liveTimingsLayout.note === 'delegated'
            ? t.applicationLiveTimingsDelegatedNote
            : t.applicationLiveTimingsUnsupportedNote}
        </Typography>
      )}
    </>
  );
}
