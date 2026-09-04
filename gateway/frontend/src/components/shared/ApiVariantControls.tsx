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
