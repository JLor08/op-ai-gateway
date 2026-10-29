// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useId, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react';
import { Alert, Box, Button, FormControlLabel, Switch, TextField, Typography } from '@mui/material';
import MicIcon from '@mui/icons-material/Mic';
import MicOffIcon from '@mui/icons-material/MicOff';
import PauseIcon from '@mui/icons-material/Pause';
import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import RestartAltIcon from '@mui/icons-material/RestartAlt';
import StopIcon from '@mui/icons-material/Stop';
import VolumeOffIcon from '@mui/icons-material/VolumeOff';
import VolumeUpIcon from '@mui/icons-material/VolumeUp';
import type { MessageKey, Translation } from './shared/types';
import { SearchableSelect } from './shared/SearchableSelect';
import { SpeechVoiceField } from './SpeechVoiceField';
import { DEFAULT_VOICE_SYSTEM_PROMPT, isCustomisedVoicePrompt } from './chat/ChatStore';
import type {
  VoiceChatApi,
  VoiceChatError,
  VoiceChatGap,
  VoiceChatModels,
  VoiceChatState,
  VoiceModelChoice,
  VoicePromptChoice,
} from './chat/ChatStore';

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
// Any other refusal (an unknown chat, a request it cannot read) is the generic sentence. The same
// table words the one 5xx the endpoint answers with a code of its own (the mint failed,
// portal.voice_chat_ticket_failed), which reaches the control as a dropped failure that carries it.
const REFUSAL_TEXT: Partial<Record<string, MessageKey>> = {
  'portal.voice_chat_asr_unavailable': 'chatVoiceErrorAsrUnavailable',
  'portal.voice_chat_llm_unavailable': 'chatVoiceErrorLlmUnavailable',
  'portal.voice_chat_tts_unavailable': 'chatVoiceErrorTtsUnavailable',
  'portal.voice_chat_coload_failed': 'chatVoiceErrorCoLoad',
  'portal.voice_chat_audio_settings_invalid': 'chatVoiceErrorAudioSettings',
  'portal.voice_chat_ticket_failed': 'chatVoiceErrorTicket',
};

// The sentence of ours for a gateway code, or undefined where none says more than the code does.
function sentenceForCode(code: string | undefined, t: Translation): string | undefined {
  const key = code ? REFUSAL_TEXT[code] : undefined;
  return key ? t[key] : undefined;
}

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
      // A failure a retry can get past; one that carries the gateway's code (a ticket the mint
      // failed to issue) is worded by it, and any other (a lost connection) is the generic one.
      return sentenceForCode(error.code, t) ?? t.chatVoiceErrorDropped;
    case 'turn':
      return withCode(t.chatVoiceErrorTurn);
    case 'refused':
      return sentenceForCode(error.code, t) ?? withCode(t.chatVoiceErrorRefused);
  }
}

// What the user has done to the conversation that the gateway's state does not say: the answer
// is paused, or their own microphone is muted. Both change what the indicator must tell them.
type UserHolds = { paused: boolean; micMuted: boolean };

// The states in which the answer's audio is played out, which is where it can be paused.
function playsAnswer(state: VoiceChatState): boolean {
  return state === 'speaking' || state === 'draining';
}

// The label of the indicator: the gateway's state, except where the user's own doing makes it
// untrue. A paused answer is not "speaking", and a muted microphone never "listens" -- the
// indicator is how the user SEES whether they are heard, so it must not say they are.
function labelKey(state: VoiceChatState, { paused, micMuted }: UserHolds): MessageKey {
  if (paused && playsAnswer(state)) return 'chatVoiceStatePaused';
  if (micMuted && state === 'listening') return 'chatVoiceStateMicMuted';
  return STATE_LABEL[state];
}

// The detail line under a state's label, where the state needs more than its label to be read
// right. While the answer is spoken the gateway discards the microphone (half-duplex, spec 6.2),
// and saying so is the point of the indicator: the user must not talk into a void. A muted
// microphone is the same void, by the user's own hand, and says so in every other state of a
// conversation that is on (push-to-talk is closed under it, so the idle hint would mislead).
function detailText(state: VoiceChatState, { micMuted }: UserHolds, t: Translation): string {
  if (playsAnswer(state)) return t.chatVoiceMicPaused;
  if (micMuted && state !== 'off') return t.chatVoiceMicMutedDetail;
  if (state === 'idle') return t.chatVoiceIdleHint;
  return '';
}

// The two states the gateway takes push-to-talk in: it starts a turn from `idle` (what is left
// after a failed turn), and in `listening` it ends the turn now instead of waiting for a pause.
// In every other state it ignores both, so the button is closed. It is closed under a muted
// microphone as well: a turn started with nothing to hear would end empty.
function takesPushToTalk(state: VoiceChatState, micMuted: boolean): boolean {
  return !micMuted && (state === 'listening' || state === 'idle');
}

// The longest voice system prompt the field takes. Generous -- the built-in prompt is about a
// sixth of it -- and only there so that a paste of a whole document is not carried into every run
// of the conversation.
const SYSTEM_PROMPT_MAX_LENGTH = 4000;

// The height the playback controls' room keeps while they are not drawn (and while they are): that
// of a row of small buttons, which is what they are -- one line (the 1.75 line height of their
// 0.8125rem text) plus 2 x 3px of padding and 2 x 1px of border. The line is in rem, as the buttons'
// text is, so the room follows a user's own default font size instead of being short of the row
// (or taller than it) for anyone whose default is not 16px. The room is what keeps the controls
// under it, and the error, from jumping each time an answer starts or ends; a row that wraps on a
// very narrow screen is taller than this and does move them, which is the lesser evil against a
// room that is never needed.
const PLAYBACK_ROOM_MIN_HEIGHT = 'calc(1.75 * 0.8125rem + 8px)';

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
  /**
   * The models of the conversation (the store's voiceChatModels): the pickers of its transcription
   * and speech models, and the thread's own chat model, named. A conversation that is on has them
   * closed -- its ticket holds the models it started with.
   */
  models: VoiceChatModels;
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
  models,
}: Readonly<VoiceChatControlProps>) {
  const transcriptId = useId();
  const answerId = useId();
  const { state, error } = voice;
  const on = state !== 'off';
  const listening = state === 'listening';
  const canTalk = takesPushToTalk(state, voice.micMuted);

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

      <VoiceModelPickers models={models} locked={on} t={t} />

      <ListeningIndicator
        state={state}
        holds={{ paused: voice.paused, micMuted: voice.micMuted }}
        t={t}
      />

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
          {/* The user's own microphone: there whenever the conversation is on, in every state. Its
              state is told by the label, which flips with it (and by the icon), and only by that:
              an `aria-pressed` beside a label that changes would say it twice. */}
          <Button
            variant={voice.micMuted ? 'contained' : 'outlined'}
            size="small"
            color={voice.micMuted ? 'warning' : 'inherit'}
            startIcon={voice.micMuted ? <MicOffIcon /> : <MicIcon />}
            onClick={() => voice.toggleMicMute()}
          >
            {voice.micMuted ? t.chatVoiceMicUnmute : t.chatVoiceMicMute}
          </Button>
          {/* The answer's speakers, beside it: also there whenever the conversation is on (#326), so
              it can be set ahead of the first answer and its state is seen between answers -- it
              holds for every answer of the conversation, not only the one that is heard now. Its
              state is told the same way as the microphone's, by its flipping label and icon. */}
          <Button
            variant={voice.muted ? 'contained' : 'outlined'}
            size="small"
            color={voice.muted ? 'warning' : 'inherit'}
            startIcon={voice.muted ? <VolumeOffIcon /> : <VolumeUpIcon />}
            onClick={() => voice.toggleMute()}
          >
            {voice.muted ? t.chatVoiceUnmute : t.chatVoiceMute}
          </Button>
          {hasAnswerInFlight(state) && (
            <Button variant="outlined" size="small" color="inherit" onClick={() => voice.cancel()}>
              {t.chatVoiceCancel}
            </Button>
          )}
        </Box>
      )}

      {/* The room of the playback controls: there for as long as the conversation is on, whether or
          not they are, so that they come and go inside it without moving anything around it. */}
      {on && (
        <Box data-testid="voice-playback-room" sx={{ minHeight: PLAYBACK_ROOM_MIN_HEIGHT }}>
          {voice.audioPlaying && playsAnswer(state) && <PlaybackControls voice={voice} t={t} />}
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

// The controls of the answer while it is heard (#315): pause and resume it, and stop the playback.
// They are only drawn while audio is being heard (`audioPlaying`), so a pause is never offered
// before the first chunk or in the gateway's tail after the audio is over, where it would do
// nothing -- in a room that is kept for them (see the control). The mute of the answer's speakers is
// not one of them: it is beside the microphone's, whenever the conversation is on (#326). Stop is
// `stopSpeaking`: it cuts the answer short and the conversation goes on -- `stop` ends the
// conversation, and Cancel (beside push-to-talk) abandons the turn.
function PlaybackControls({ voice, t }: Readonly<{ voice: VoiceChatApi; t: Translation }>) {
  return (
    <Box
      role="group"
      aria-label={t.chatVoicePlayback}
      sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1 }}
    >
      {voice.paused ? (
        <Button
          variant="contained"
          size="small"
          startIcon={<PlayArrowIcon />}
          onClick={() => voice.resume()}
        >
          {t.chatVoiceResume}
        </Button>
      ) : (
        <Button
          variant="outlined"
          size="small"
          startIcon={<PauseIcon />}
          onClick={() => voice.pause()}
        >
          {t.chatVoicePause}
        </Button>
      )}
      <Button
        variant="outlined"
        size="small"
        color="error"
        startIcon={<StopIcon />}
        onClick={() => voice.stopSpeaking()}
      >
        {t.chatVoiceStopPlayback}
      </Button>
    </Box>
  );
}

// The pickers of the conversation's two models (#317), the live transcription model that hears the
// user and the speech model that answers aloud, one field each, the speech model's voice (#318)
// under its picker -- the speech composer's own voice field, shown only when the model has voices
// to offer (a model with none speaks in its own default) -- and under them the third model --
// the thread's own, which answers -- named: it is picked with the thread's model selector, so it
// has no field here. Closed until the gateway has said which models stream (there is nothing to
// choose from), and while a conversation is on (its ticket holds the models it started with); NOT
// closed by a switch that is closed for another reason (a missing chat model, a run in flight):
// the user may pick ahead.
//
// Under them, the thread's voice system prompt (#314), in a collapsed section (VoicePromptSection):
// a multi-line field, pre-filled with the built-in prompt, that the user may edit for this thread.
// It has nothing to do with which models stream, so it is not waiting for the gateway; it is closed
// while a conversation is on, so a conversation runs on the prompt it began with. It is bounded in
// length, and the control under it puts the built-in prompt back (closed with the field, and when
// the field already holds it).
function VoiceModelPickers({
  models,
  locked,
  t,
}: Readonly<{ models: VoiceChatModels; locked: boolean; t: Translation }>) {
  const closed = locked || !models.ready;
  return (
    <Box sx={{ display: 'grid', gap: 0.75 }}>
      <Box
        sx={{
          display: 'grid',
          gap: 1.5,
          gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 240px), 1fr))',
        }}
      >
        <VoiceModelPicker
          id="chat-voice-asr-model"
          label={t.chatVoiceAsrModel}
          choice={models.asr}
          disabled={closed}
          t={t}
        />
        {/* The speech model's picker and, under it, its voice: one column, so the voice reads as
            the speech model's and not the transcription model's. */}
        <Box sx={{ display: 'grid', gap: 1.5, alignContent: 'start' }}>
          <VoiceModelPicker
            id="chat-voice-tts-model"
            label={t.chatVoiceTtsModel}
            choice={models.tts}
            disabled={closed}
            t={t}
          />
          {models.voice.voices.length > 0 && (
            <SpeechVoiceField
              id="chat-voice-tts-voice"
              value={models.voice.value}
              voices={models.voice.voices}
              onChange={models.voice.set}
              disabled={closed}
              t={t}
            />
          )}
        </Box>
      </Box>
      {models.llm !== '' && (
        <Typography variant="caption" color="text.secondary" component="p" sx={{ m: 0 }}>
          {t.chatVoiceLlmNote(models.llm)}
        </Typography>
      )}
      <VoicePromptSection prompt={models.systemPrompt} locked={locked} t={t} />
    </Box>
  );
}

// The thread's voice system prompt (#314) and the control that puts the built-in one back, in a
// section that is COLLAPSED until its heading is clicked: the prompt is a tuning knob few
// conversations touch, and a field of several lines would otherwise crowd the pickers and the
// indicator that matter while talking. The same toggle as the speech thread's "Advanced" toggles
// (Chat.tsx): a text button with `aria-expanded`, the content not mounted while it is shut. The
// heading stays usable while a conversation is on -- the field under it is what is closed then, so
// a run keeps the prompt it began with, but it may still be read. While the section is shut and the
// prompt is the thread's own (isCustomisedVoicePrompt, the rule the store saves by), the heading
// carries a small "edited" marker (#326): the edit would otherwise be invisible. It is decorative
// (aria-hidden, so it does not change the toggle's name) and the toggle is DESCRIBED by it, so a
// screen reader says it too; open, the field is on screen and the marker is gone, so it does not
// come and go while the user types.
function VoicePromptSection({
  prompt,
  locked,
  t,
}: Readonly<{ prompt: VoicePromptChoice; locked: boolean; t: Translation }>) {
  const [open, setOpen] = useState(false);
  const regionId = useId();
  const editedId = useId();
  const showEdited = !open && isCustomisedVoicePrompt(prompt.value);
  return (
    <Box sx={{ display: 'grid', gap: 0.75 }}>
      <Button
        variant="text"
        size="small"
        aria-expanded={open}
        aria-controls={open ? regionId : undefined}
        aria-describedby={showEdited ? editedId : undefined}
        onClick={() => setOpen((wasOpen) => !wasOpen)}
        sx={{ justifySelf: 'start' }}
      >
        {t.chatVoiceSystemPrompt}
        {showEdited && (
          <Box
            component="span"
            id={editedId}
            aria-hidden
            sx={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: 0.5,
              ml: 1,
              color: 'text.secondary',
              fontSize: 'caption.fontSize',
            }}
          >
            <Box
              component="span"
              sx={{ width: 8, height: 8, borderRadius: '50%', bgcolor: 'warning.main' }}
            />
            {t.chatVoiceSystemPromptEdited}
          </Box>
        )}
      </Button>
      {open && (
        <Box id={regionId} sx={{ display: 'grid', gap: 0.75 }}>
          <TextField
            id="chat-voice-system-prompt"
            label={t.chatVoiceSystemPrompt}
            helperText={t.chatVoiceSystemPromptHint}
            multiline
            minRows={3}
            maxRows={12}
            fullWidth
            value={prompt.value}
            onChange={(event) => prompt.set(event.target.value)}
            disabled={locked}
            slotProps={{ htmlInput: { maxLength: SYSTEM_PROMPT_MAX_LENGTH } }}
            sx={{ mt: 0.75 }}
          />
          <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button
              size="small"
              color="inherit"
              startIcon={<RestartAltIcon />}
              disabled={locked || prompt.value === DEFAULT_VOICE_SYSTEM_PROMPT}
              onClick={() => prompt.set(DEFAULT_VOICE_SYSTEM_PROMPT)}
            >
              {t.chatVoiceSystemPromptReset}
            </Button>
          </Box>
        </Box>
      )}
    </Box>
  );
}

// One picker: "automatic" (no pick) and the models on offer, by their display names, a loaded one
// marked with a green dot. Under an automatic pick the field says which model that is.
function VoiceModelPicker({
  id,
  label,
  choice,
  disabled,
  t,
}: Readonly<{
  id: string;
  label: string;
  choice: VoiceModelChoice;
  disabled: boolean;
  t: Translation;
}>) {
  const options = [
    { value: '', label: t.chatVoiceModelAuto },
    // A loaded model carries the green dot of the chat's own model picker (SearchableSelect draws
    // it from `loaded`, in the list and, once picked, in the field), titled by the servers it is
    // loaded on when the catalogue names them.
    ...choice.options.map((option) => ({
      value: option.id,
      label: option.display_name,
      loaded: option.loaded,
      loadedTitle:
        option.loaded_on && option.loaded_on.length > 0
          ? `${t.chatModelLoadedOn}: ${option.loaded_on.join(', ')}`
          : t.chatModelLoaded,
    })),
  ];
  const used = choice.options.find((option) => option.id === choice.used);
  const showsUsed = choice.pick === '' && choice.used !== '';
  return (
    <SearchableSelect
      id={id}
      label={label}
      value={choice.pick}
      onChange={choice.set}
      options={options}
      disabled={disabled}
      helperText={showsUsed ? t.chatVoiceModelUsing(used?.display_name ?? choice.used) : undefined}
    />
  );
}

// The indicator of whether the system hears the user (spec 7): a status region with the
// microphone in a circle and the label of the state, and under it the detail where the state
// needs one. Active: the hot microphone, in the colour of a recording, and pulsing (unless the
// user asked for no motion). Not listening: the same circle, grey and still, with the muted
// microphone and the label of what the system is doing instead.
function ListeningIndicator({
  state,
  holds,
  t,
}: Readonly<{ state: VoiceChatState; holds: UserHolds; t: Translation }>) {
  const on = state !== 'off';
  // Only a microphone that is on, in the one state that takes the user's voice, hears them.
  const listening = state === 'listening' && !holds.micMuted;
  const detail = detailText(state, holds, t);
  const badgeSize = on ? 48 : 32;
  const Icon = listening ? MicIcon : MicOffIcon;
  return (
    <Box
      role="status"
      aria-live="polite"
      aria-label={t.chatVoiceIndicator}
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: 1.5,
        minHeight: on ? 56 : 32,
        // It must not scroll out of sight under a long thread, or a long transcript and answer
        // (#318): it sticks to the edge of the viewport it would leave, within the control, on the
        // page's own background so that what scrolls under it does not show through.
        position: 'sticky',
        top: 0,
        bottom: 0,
        zIndex: 1,
        py: 0.5,
        bgcolor: 'var(--page)',
      }}
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
          {t[labelKey(state, holds)]}
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
