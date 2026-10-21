// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Autocomplete, TextField } from '@mui/material';
import type { Translation } from './shared/types';

// The speech settings' voice field: a combobox over the voices the gateway has cached
// for the picked model (`voices`), that is STILL a free-text field. The cache holds only
// the voices the health loop has seen the model offer, so a list is an offer, never a
// constraint: any name may be typed, and what is typed is what the run sends. An empty
// `voices` (none cached, the lookup failed, or it is still in flight) leaves nothing to
// pick from and no hint -- the field is then the plain free-text field it was before
// voices were discoverable.
//
// The value is the thread's own `voice` string, never an option object: the field shows
// and edits that string, so a saved voice the list does not hold (a model that dropped
// it, a name typed for a model with no cache) stays exactly as it is instead of being
// blanked or swapped for a listed one. A blank value is "the model's default voice" and
// is never written (its hint is the placeholder).
export function SpeechVoiceField({
  id,
  value,
  voices,
  onChange,
  disabled,
  t,
}: Readonly<{
  id: string;
  value: string;
  voices: readonly string[];
  onChange: (voice: string) => void;
  disabled?: boolean;
  t: Translation;
}>) {
  const hasVoices = voices.length > 0;
  return (
    <Autocomplete
      freeSolo
      id={id}
      options={voices}
      value={value === '' ? null : value}
      // The input text IS the voice: typing, picking a listed voice (reason "reset"),
      // clearing and Enter on typed text all end in this one handler, so the field
      // follows whatever the store holds (a saved thread, another thread) with no
      // internal copy to drift from it.
      inputValue={value}
      onInputChange={(_event, next) => onChange(next)}
      // freeSolo hides MUI's dropdown arrow; show it while there is a list, so the
      // list is discoverable and not just reachable by typing or the arrow key.
      forcePopupIcon={hasVoices}
      disabled={disabled}
      fullWidth
      renderInput={(params) => (
        <TextField
          {...params}
          label={t.chatSpeechVoice}
          placeholder={t.chatSpeechVoiceDefault}
          helperText={hasVoices ? t.chatSpeechVoiceHint : undefined}
          // Pin the label shrunk and the notch open (as the prompt field does) so the
          // placeholder -- the model-default hint -- is visible instead of hidden
          // behind the resting label.
          slotProps={{
            ...params.slotProps,
            inputLabel: { ...params.slotProps.inputLabel, shrink: true },
            input: { ...params.slotProps.input, notched: true },
          }}
        />
      )}
    />
  );
}
