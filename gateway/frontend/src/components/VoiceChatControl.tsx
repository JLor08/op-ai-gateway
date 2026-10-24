// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useId, useRef, type KeyboardEvent, type PointerEvent } from 'react';
import { Alert, Box, Button, FormControlLabel, Switch, Typography } from '@mui/material';
import MicIcon from '@mui/icons-material/Mic';
import MicOffIcon from '@mui/icons-material/MicOff';
import type { MessageKey, Translation } from './shared/types';
import type { VoiceChatApi, VoiceChatError, VoiceChatGap, VoiceChatState } from './chat/ChatStore';

// The label of each state of the conversation. `off` and `connecting` are the portal's own; the
// rest are the gateway's state machine (spec 5.3), mirrored as it reports them.
const STATE_LABEL = {
  off: 'chatVoiceStateOff',
  connecting: 'chatVoiceStateConnecting',
  listening: 'chatVoiceStateListening',
  thinking: 'chatVoiceStateThinking',
  speaking: 'chatVoiceStateSpeaking',
  draining: 'chatVoiceStateDraining',
  idle: 'chatVoiceStateIdle',
} satisfies Record<VoiceChatState, MessageKey>;

// Why the switch is closed for a thread whose models cannot do a spoken conversation.
const GAP_TEXT = {
  llm: 'chatVoiceUnavailableLlm',
  asr: 'chatVoiceUnavailableAsr',
  tts: 'chatVoiceUnavailableTts',
  checking: 'chatVoiceUnavailableChecking',
} satisfies Record<VoiceChatGap, MessageKey>;

// What the gateway says when it refuses a ticket, by its code (the gate's portal.voice_chat_*).
// Any other refusal (an unknown chat, a request it cannot read) is the generic sentence.
const REFUSAL_TEXT: Partial<Record<string, MessageKey>> = {
  'portal.voice_chat_asr_unavailable': 'chatVoiceErrorAsrUnavailable',
  'portal.voice_chat_llm_unavailable': 'chatVoiceErrorLlmUnavailable',
  'portal.voice_chat_tts_unavailable': 'chatVoiceErrorTtsUnavailable',
  'portal.voice_chat_coload_failed': 'chatVoiceErrorCoLoad',
  'portal.voice_chat_audio_settings_invalid': 'chatVoiceErrorAudioSettings',
  'portal.voice_chat_ticket_failed': 'chatVoiceErrorTicket',
};

// The sentence for a failure, by what kind it is and the gateway's code. `message` is the
// session's diagnostic English and is never shown; the code is, where no sentence of ours says
// more than it does (an unmapped refusal, a failed turn), because it is what an operator looks up.
function errorText(error: VoiceChatError, t: Translation): string {
  const withCode = (text: string) => (error.code ? `${text} (${error.code})` : text);
  switch (error.kind) {
    case 'denied':
      return t.chatRecordDenied;
    case 'unsupported':
      return t.chatVoiceErrorUnsupported;
    case 'capture':
      return t.chatVoiceErrorCapture;
    case 'dropped':
      return t.chatVoiceErrorDropped;
    case 'turn':
      return withCode(t.chatVoiceErrorTurn);
    case 'refused': {
      const key = error.code ? REFUSAL_TEXT[error.code] : undefined;
      return key ? t[key] : withCode(t.chatVoiceErrorRefused);
    }
  }
}

// The detail line under a state's label, where the state needs more than its label to be read
// right. While the answer is spoken the gateway discards the microphone (half-duplex, spec 6.2),
// and saying so is the point of the indicator: the user must not talk into a void.
function detailText(state: VoiceChatState, t: Translation): string {
  if (state === 'speaking' || state === 'draining') return t.chatVoiceMicPaused;
  if (state === 'idle') return t.chatVoiceIdleHint;
  return '';
}

// The two states the gateway takes push-to-talk in: it starts a turn from `idle` (what is left
// after a failed turn), and in `listening` it ends the turn now instead of waiting for a pause.
// In every other state it ignores both, so the button is closed.
function takesPushToTalk(state: VoiceChatState): boolean {
  return state === 'listening' || state === 'idle';
}

// The states in which an answer is being prepared or spoken, which Stop abandons.
function hasAnswerInFlight(state: VoiceChatState): boolean {
  return state === 'thinking' || state === 'speaking' || state === 'draining';
}

export type VoiceChatControlProps = {
  t: Translation;
  /** The conversation (useVoiceChat): its state, what is said and answered, and its controls. */
  voice: VoiceChatApi;
  /** Whether the thread's models can do a spoken conversation (the store's voiceChatCapable). */
  capable: boolean;
  /** Which part is missing when they cannot (the store's voiceChatGap). */
  gap: VoiceChatGap | null;
  /** A chat run or a live recording holds the thread: a conversation cannot start now. */
  busy?: boolean;
};

// Why the switch is closed, in words on the screen (a tooltip would not be seen on a touch
// screen or read before the click): the missing model first, then a run in flight. The refusal
// has its own alert, so it has no reason here, and nor does a conversation that is on.
function closedReason(
  {
    on,
    refused,
    capable,
    busy,
  }: { on: boolean; refused: boolean; capable: boolean; busy: boolean },
  gap: VoiceChatGap | null,
  t: Translation,
): string {
  if (on || refused) return '';
  if (!capable) return gap ? t[GAP_TEXT[gap]] : t.chatVoiceUnavailableChecking;
  return busy ? t.chatBusy : '';
}

// The keys that hold the talk button down, as a press of it does: Space and Enter.
function isTalkKey(event: KeyboardEvent<HTMLButtonElement>): boolean {
  return event.key === ' ' || event.key === 'Enter';
}

// The release is delivered to the button even when the pointer has left it (a finger that slides
// off a phone's button still ends the turn). Not every environment has it.
function capturePointer(event: PointerEvent<HTMLButtonElement>) {
  try {
    event.currentTarget.setPointerCapture?.(event.pointerId);
  } catch {
    /* best-effort */
  }
}

// Push-to-talk is held: the press starts the turn, the release ends it. `held` is whether a
// press that has begun is still going on, so a release (or a cancelled press, or the focus
// leaving) ends a turn only once, and only a turn that this press began.
function useHeldPushToTalk(canTalk: boolean, voice: Pick<VoiceChatApi, 'pushStart' | 'pushEnd'>) {
  const held = useRef(false);
  function press() {
    if (held.current || !canTalk) return;
    held.current = true;
    voice.pushStart();
  }
  function release() {
    if (!held.current) return;
    held.current = false;
    voice.pushEnd();
  }
  // The button closes under a press that is still held (the gateway moved the conversation on, or
  // it ended): a release that never reaches a closed button must not leave the press standing, or
  // the next one would be taken for a repeat of it. The gateway has left the state a push_end
  // would be taken in, so none is sent.
  useEffect(() => {
    if (!canTalk) held.current = false;
  }, [canTalk]);

  function onPointerDown(event: PointerEvent<HTMLButtonElement>) {
    capturePointer(event);
    press();
  }
  function onKeyDown(event: KeyboardEvent<HTMLButtonElement>) {
    if (!isTalkKey(event)) return;
    // Neither the page's scroll (Space) nor the button's own click (Enter) is wanted.
    event.preventDefault();
    // The key repeat of a held key is the same press.
    if (!event.repeat) press();
  }
  function onKeyUp(event: KeyboardEvent<HTMLButtonElement>) {
    if (!isTalkKey(event)) return;
    event.preventDefault();
    release();
  }
  return { release, onPointerDown, onKeyDown, onKeyUp };
}

// The control is only mounted while the chat page shows a thread that Voice Chat is offered on:
// when it goes -- another page of the portal, another thread or model -- a conversation must not
// stay behind with the microphone on and nothing on screen to end it. The latest state and stop
// are read through refs, so this runs only when the control goes, not on every change.
function useStopWhenGone(state: VoiceChatState, stop: () => void) {
  const stateRef = useRef(state);
  stateRef.current = state;
  const stopRef = useRef(stop);
  stopRef.current = stop;
  useEffect(
    () => () => {
      if (stateRef.current !== 'off') stopRef.current();
    },
    [],
  );
}

// The Voice Chat mode of a text thread: the switch, the indicator of whether the system is
// listening, what is being said and answered, and push-to-talk. The conversation itself is the
// store's (voice and its controls); this only draws it.
//
// THE INDICATOR is the point of the component (spec 7), not decoration. During `speaking` and
// `draining` the gateway discards the microphone, so a user who cannot see that will talk into a
// void. It therefore says, at a glance and in words, whether the system hears them: ONLY
// `listening` has the active microphone and its label (with what is being said, below); every
// other state -- thinking, speaking, draining, idle, connecting, off -- has the muted microphone
// and the label of what the system is doing instead.
export function VoiceChatControl({
  t,
  voice,
  capable,
  gap,
  busy = false,
}: Readonly<VoiceChatControlProps>) {
  const transcriptId = useId();
  const answerId = useId();
  const { state, error } = voice;
  const on = state !== 'off';
  const listening = state === 'listening';
  const canTalk = takesPushToTalk(state);

  // A refused ticket is the gateway's answer to this very request, and it is the same next time
  // (a model it will not use, models it cannot hold together): the switch stays closed on it, and
  // says so, until the user has read it and dismissed it. Every other failure is one a new try can
  // get past, so the switch is simply open again.
  const refused = error?.kind === 'refused';
  // The way OUT is never closed: a conversation that is on can always be switched off, whatever
  // the catalogue or the thread are doing meanwhile.
  const toggleClosed = !on && (!capable || busy || refused);
  const reason = closedReason({ on, refused, capable, busy }, gap, t);

  const talk = useHeldPushToTalk(canTalk, voice);
  useStopWhenGone(state, voice.stop);

  return (
    <Box
      role="group"
      aria-label={t.chatVoice}
      sx={{
        display: 'grid',
        gap: 1.5,
        p: 1.5,
        border: '1px solid var(--line)',
        borderRadius: '8px',
      }}
    >
      <Box>
        <FormControlLabel
          control={
            <Switch
              checked={on}
              disabled={toggleClosed}
              // Straight from the click, with nothing awaited first: the microphone and the
              // browser's audio are only granted to a call made inside the user's gesture.
              onChange={() => {
                if (on) voice.stop();
                else if (!toggleClosed) voice.start();
              }}
            />
          }
          label={t.chatVoice}
        />
        {reason !== '' && (
          <Typography variant="caption" color="text.secondary" component="p" sx={{ m: 0 }}>
            {reason}
          </Typography>
        )}
        {reason === '' && !on && !refused && (
          <Typography variant="caption" color="text.secondary" component="p" sx={{ m: 0 }}>
            {t.chatVoiceHint}
          </Typography>
        )}
      </Box>

      <ListeningIndicator state={state} t={t} />

      {on && voice.liveTranscript !== '' && (
        <TurnText
          id={transcriptId}
          label={t.chatVoiceTranscript}
          text={voice.liveTranscript}
          // What the user is saying right now is the live part: it grows under their voice.
          emphasis={listening}
        />
      )}
      {on && voice.answerPreview !== '' && (
        <TurnText id={answerId} label={t.chatVoiceAnswer} text={voice.answerPreview} />
      )}

      {on && (
        <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1 }}>
          <Button
            variant={listening ? 'contained' : 'outlined'}
            size="small"
            startIcon={<MicIcon />}
            disabled={!canTalk}
            onPointerDown={talk.onPointerDown}
            onPointerUp={talk.release}
            onPointerCancel={talk.release}
            onKeyDown={talk.onKeyDown}
            onKeyUp={talk.onKeyUp}
            onBlur={talk.release}
            // A long press on a phone would otherwise open the context menu under the finger.
            onContextMenu={(event) => event.preventDefault()}
            sx={{ touchAction: 'none', userSelect: 'none', WebkitTouchCallout: 'none' }}
          >
            {t.chatVoicePushToTalk}
          </Button>
          {hasAnswerInFlight(state) && (
            <Button variant="outlined" size="small" color="inherit" onClick={() => voice.cancel()}>
              {t.chatVoiceCancel}
            </Button>
          )}
        </Box>
      )}

      {error && (
        <Alert
          severity="error"
          action={
            // A conversation that has ended shows why until it is read; one that is still on (a
            // failed turn) goes on, and the next press of the talk button clears it.
            on ? undefined : (
              <Button color="inherit" size="small" onClick={() => voice.stop()}>
                {t.chatVoiceDismiss}
              </Button>
            )
          }
        >
          {errorText(error, t)}
        </Alert>
      )}
    </Box>
  );
}

// The indicator of whether the system hears the user (spec 7): a status region with the
// microphone in a circle and the label of the state, and under it the detail where the state
// needs one. Active: the hot microphone, in the colour of a recording, and pulsing (unless the
// user asked for no motion). Not listening: the same circle, grey and still, with the muted
// microphone and the label of what the system is doing instead.
function ListeningIndicator({ state, t }: Readonly<{ state: VoiceChatState; t: Translation }>) {
  const on = state !== 'off';
  const listening = state === 'listening';
  const detail = detailText(state, t);
  const badgeSize = on ? 48 : 32;
  const Icon = listening ? MicIcon : MicOffIcon;
  return (
    <Box
      role="status"
      aria-live="polite"
      aria-label={t.chatVoiceIndicator}
      sx={{ display: 'flex', alignItems: 'center', gap: 1.5, minHeight: on ? 56 : 32 }}
    >
      <Box
        aria-hidden
        sx={{
          flex: '0 0 auto',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          width: badgeSize,
          height: badgeSize,
          borderRadius: '50%',
          // Active: the hot microphone, in the colour of a recording, and pulsing (unless the
          // user asked for no motion). Not listening: the same circle, grey and still.
          bgcolor: listening ? 'error.main' : 'action.disabledBackground',
          color: listening ? 'error.contrastText' : 'text.disabled',
          '@keyframes op-voice-pulse': {
            '0%': { boxShadow: '0 0 0 0 rgba(211, 47, 47, 0.55)' },
            '70%': { boxShadow: '0 0 0 14px rgba(211, 47, 47, 0)' },
            '100%': { boxShadow: '0 0 0 0 rgba(211, 47, 47, 0)' },
          },
          '@media (prefers-reduced-motion: no-preference)': listening
            ? { animation: 'op-voice-pulse 1.6s ease-out infinite' }
            : {},
        }}
      >
        <Icon fontSize={on ? 'medium' : 'small'} />
      </Box>
      <Box sx={{ minWidth: 0 }}>
        <Typography
          sx={{
            fontWeight: 700,
            fontSize: on ? '1.1rem' : undefined,
            color: listening ? 'error.main' : 'text.secondary',
          }}
        >
          {t[STATE_LABEL[state]]}
        </Typography>
        {detail !== '' && (
          <Typography variant="caption" color="text.secondary" component="p" sx={{ m: 0 }}>
            {detail}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

// One text of the turn in progress, under its caption: what the user has said, or the answer so
// far. Provisional both (the thread shows them as the turn it stores once it is committed).
function TurnText({
  id,
  label,
  text,
  emphasis = false,
}: Readonly<{ id: string; label: string; text: string; emphasis?: boolean }>) {
  return (
    <Box sx={{ display: 'grid', gap: 0.25 }}>
      <Typography id={id} variant="caption" color="text.secondary">
        {label}
      </Typography>
      <Box
        role="region"
        aria-labelledby={id}
        aria-live="polite"
        sx={{
          maxHeight: 240,
          overflowY: 'auto',
          p: 1,
          border: '1px solid var(--line)',
          borderRadius: '8px',
          bgcolor: 'var(--page)',
          color: 'var(--text)',
          lineHeight: 1.55,
          whiteSpace: 'pre-wrap',
          overflowWrap: 'anywhere',
          fontWeight: emphasis ? 600 : 400,
        }}
      >
        {text}
      </Box>
    </Box>
  );
}
