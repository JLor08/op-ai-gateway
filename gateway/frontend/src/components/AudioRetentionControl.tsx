// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  Radio,
  RadioGroup,
  TextField,
  Typography,
} from '@mui/material';
import type { Translation } from './shared/types';
import {
  isKeepLastInvalid,
  type AudioSettings,
  type PendingAudioRetention,
} from './chat/ChatStore';

// The retention half of the speech thread's audio settings, which is all this control edits.
export type AudioRetention = Pick<AudioSettings, 'keepAudio' | 'keepLast'>;

// The speech settings' answer-audio retention (#281): "keep audio answers" (on by default)
// over a keep-all / keep-last-N choice. N is a blank-able text like the other numeric knobs
// and its field exists only for "keep last N"; blank means keep-all, so the radio cannot be
// derived from the text alone (a user who picked "keep last N" has not typed a count yet, and
// one who erased the last digit is mid-edit): the choice itself is local state, seeded from
// the stored text and NEVER written on its own -- only a click or a keystroke calls
// onChange, so showing a stored value never saves anything.
//
// Unchecking the box is "do not store": the run then drops the audio it streamed and keeps
// nothing. That only makes sense while the answer is heard live, so it is selectable only for
// a thread that streams (`speechStreaming`, the gate of the streaming toggle). A buffered
// answer is only ever heard from storage, and the run keeps one anyway. What is already
// stored is shown as it is, never corrected here: a thread that stored "do not store" but
// whose model stopped streaming shows the box unticked, and only SELECTING it is withheld, so
// the box can still be switched back on.
//
// A value that would delete audio already stored (a lower N, or "do not store", on a chat that
// holds more) is not applied by the store but HELD, and `pending` says so: the control keeps
// showing what was asked for, and a notice beside it names how many answers would lose their
// audio and offers to apply it (`onApply`, which deletes them) or discard it (`onDiscard`,
// which puts the control back and touches nothing). The notice is inline, not a dialog, so the
// count can be edited on with the field still in reach.
//
// Key it by the chat: the local choice belongs to one thread's text.
export function AudioRetentionControl({
  keepAudio,
  keepLast,
  speechStreaming,
  disabled,
  onChange,
  pending,
  onApply,
  onDiscard,
  t,
}: Readonly<{
  keepAudio: boolean;
  keepLast: string;
  speechStreaming: boolean;
  disabled: boolean;
  onChange: (patch: Partial<AudioRetention>) => void;
  pending: PendingAudioRetention | null;
  onApply: () => void;
  onDiscard: () => void;
  t: Translation;
}>) {
  const [pickedLast, setPickedLast] = useState(false);
  const lastChosen = pickedLast || keepLast !== '';
  const countInvalid = isKeepLastInvalid(keepLast);
  const withheld = keepAudio && !speechStreaming;

  return (
    <Box sx={{ display: 'grid', gap: 0.5, justifyItems: 'start' }}>
      <FormControlLabel
        sx={{ mr: 0 }}
        control={
          <Checkbox
            size="small"
            checked={keepAudio}
            onChange={(event) => onChange({ keepAudio: event.target.checked })}
            disabled={disabled || withheld}
            slotProps={{ input: { 'aria-describedby': 'chat-audio-keep-hint' } }}
          />
        }
        label={t.chatAudioKeep}
      />
      <Typography id="chat-audio-keep-hint" variant="caption" color="text.secondary">
        {t.chatAudioKeepHint}
      </Typography>
      {keepAudio && (
        <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-start', columnGap: 2 }}>
          <RadioGroup
            row
            aria-label={t.chatAudioKeep}
            value={lastChosen ? 'last' : 'all'}
            onChange={(event) => {
              const last = event.target.value === 'last';
              setPickedLast(last);
              // Choosing keep-all drops the count; choosing keep-last-N leaves the text
              // alone (blank until the user types one).
              if (!last) onChange({ keepLast: '' });
            }}
          >
            <FormControlLabel
              value="all"
              control={<Radio size="small" disabled={disabled} />}
              label={t.chatAudioKeepAll}
            />
            <FormControlLabel
              value="last"
              control={<Radio size="small" disabled={disabled} />}
              label={t.chatAudioKeepLast}
            />
          </RadioGroup>
          {lastChosen && (
            <TextField
              id="chat-audio-keep-last"
              label={t.chatAudioKeepLastCount}
              type="number"
              size="small"
              value={keepLast}
              onChange={(event) => {
                // Typing is a decision for keep-last-N, so erasing the digits (blank, which
                // reads as keep-all) leaves the field in place to be retyped.
                setPickedLast(true);
                onChange({ keepLast: event.target.value });
              }}
              disabled={disabled}
              error={countInvalid}
              helperText={countInvalid ? t.chatAudioKeepLastInvalid : undefined}
              slotProps={{ htmlInput: { min: 1, step: 1 } }}
              sx={{ width: 220 }}
            />
          )}
        </Box>
      )}
      {pending && (
        <Alert
          severity="warning"
          sx={{ justifySelf: 'stretch' }}
          action={
            <>
              <Button color="inherit" size="small" onClick={onDiscard}>
                {t.chatAudioPruneDiscard}
              </Button>
              <Button color="error" size="small" onClick={onApply} disabled={disabled}>
                {t.chatAudioPruneConfirm}
              </Button>
            </>
          }
        >
          {t.chatAudioPrunePending(pending.keepLast, pending.excess)}
        </Alert>
      )}
    </Box>
  );
}
