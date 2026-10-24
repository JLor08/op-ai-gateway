// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type ReactNode,
  type SetStateAction,
} from 'react';
import {
  NO_SPEECH_LANGUAGE,
  NO_SPEECH_STREAMING,
  type ActiveChatRun,
  type Chat,
  type ChatSummary,
  type ImageModelOptions,
  type ModelOption,
  type PortalApi,
  type PortalServer,
  type PortalToken,
  type ServerModelOption,
  type SpeechLanguageGate,
  type SpeechStreamingGate,
} from '../../api';
import type { Locale } from '../../i18n';
import type { Translation } from '../shared/types';
import { formatPortalError } from '../shared/format';
import { useToast } from '../shared/ToastProvider';
import { imageUrlsOf, type ChatContent } from '../shared/chatContent';
import {
  prepareImageDataUrl,
  prepareMaskDataUrl,
  ImageAttachError,
  isHeicFile,
} from '../shared/imageAttach';
import { offeredFlavors } from '../shared/offeredFlavors';
import {
  ACTIVE_ID_KEY,
  DEFAULTS,
  MAX_IMAGES,
  attachModeOf,
  audioAnswersBeyond,
  audioPassthroughMessage,
  audioSetting,
  audioSettingsFrom,
  audioTurnContent,
  byNewest,
  clearLegacyKeys,
  DEFAULT_AUDIO_SETTINGS,
  DEFAULT_IMAGE_SETTINGS,
  DEFAULT_TRANSCRIPTION_SETTINGS,
  IMAGE_EDIT_SOURCE_TYPES,
  deriveTitle,
  deriveTitleFromMessages,
  deriveTitleFromTranscript,
  historyHasImage,
  holdsStoredAudio,
  imageSetting,
  imageSettingsFrom,
  isChatBlobUrl,
  keepLastAudioOf,
  lastUserHasAudio,
  lastUserHasImage,
  lastUserText,
  lowersAudioRetention,
  lsGet,
  lsSet,
  nextId,
  normalizeDoc,
  parseAudioPassthrough,
  parseTranscriptionPassthrough,
  pruneAudioBeyond,
  readLegacyDoc,
  resolveTokenOverride,
  speechPassthroughInvalid,
  summaryOf,
  transcriptionPassthroughInvalid,
  transcriptionPassthroughMessage,
  transcriptionSetting,
  transcriptionSettingsFrom,
  userTurnContent,
  withAudioRemoved,
  type AttachMode,
  type AudioSettings,
  type ChatUiMessage,
  type ImageSettings,
  type TranscriptionSettings,
} from './chatDoc';
import { AudioAttachError, prepareAudioClip, type AudioClip } from './audioAttach';
import { liveSessionRunning } from './liveSession';
import { speechPlayback } from './speechPlayback';
import { useChatRuns } from './useChatRuns';
import { useChatPersistence } from './useChatPersistence';
import {
  useLiveTranscription,
  type LiveStart,
  type LiveTranscription,
} from './useLiveTranscription';
import {
  useVoiceChat,
  type VoiceChatApi,
  type VoiceChatStart,
  type VoiceChatState,
} from './useVoiceChat';

// Re-exported so existing/external imports of these types from ChatStore keep
// working unchanged after the FA-2 extraction into chatDoc.ts.
export type { ActiveChatDoc, ChatMessageStatus } from './chatDoc';
export type { AttachMode, ChatUiMessage };
// The transcription composer's clip, for the view that previews it.
export type { AudioClip };
// The live recording's state, for the view that shows it.
export type { LiveTranscription, LiveTranscriptionPhase } from './useLiveTranscription';
export { liveSessionRunning } from './liveSession';
// The Voice Chat conversation's state, for the view that shows it (VoiceChatControl).
export type { VoiceChatApi, VoiceChatError, VoiceChatState } from './useVoiceChat';

// The speech composer's advanced-passthrough check and the settings type it edits,
// re-exported for Chat.tsx for the same reason as the image presets below: the
// composer must not reach into chatDoc.ts (arch.test.ts), and it needs to name a
// passthrough text that is not a JSON object, or a keep-last count the wire would drop,
// before the user sends it.
export { audioPassthroughMessage, isKeepLastInvalid, parseAudioPassthrough } from './chatDoc';
export type { AudioPassthroughParse, AudioSettings } from './chatDoc';

// The transcription composer's twin of the above (its own reserved fields and message), and
// what its audio input needs: the attach picker's `accept` list and the bare media-type helper.
// (The clip itself is built inside the store, by handleFiles / attachRecording, which convert
// whatever audio the browser can decode to a 16 kHz mono WAV before it is attached.)
export {
  bareAudioMediaType,
  parseTranscriptionPassthrough,
  TRANSCRIPTION_AUDIO_ACCEPT,
  transcriptionPassthroughMessage,
} from './chatDoc';
export type { TranscriptionSettings } from './chatDoc';

// The image-settings presets (and the optionsOr helper that merges a per-model
// fetched list over them) the composer (Chat.tsx, OUTSIDE components/chat/)
// renders. Re-exported through this file because the chat module's only public
// surface is ChatStore.tsx / ChatSidebar.tsx -- Chat.tsx must not reach into
// chatDoc.ts directly (arch.test.ts enforces this).
export {
  IMAGE_EDIT_SOURCE_ACCEPT,
  IMAGE_LOSSY_FORMATS,
  IMAGE_N_MAX,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
  optionsOr,
} from './chatDoc';

// "No voices to offer": one shared empty list, so resetting the picker's voices on every
// model change is a no-op render (React bails out on an identical state value) instead of
// a fresh array each time.
const NO_VOICES: readonly string[] = [];

// Whether a catalogue model may be offered as a SPEECH model: the backend's `speech` verdict,
// minus a voice-CLONE model (speech_clone), which the text-only speech composer cannot drive
// (it needs a reference voice) -- unless it has configured preset voices (speech_clone_voices),
// which make it usable without one (#252, #301). The one rule of the chat's model list, of the
// speech composer (modelSpeechCapable) and of Voice Chat's voice.
function speechOffered(option: ModelOption | undefined): boolean {
  return (
    option?.speech === true && (option.speech_clone !== true || option.speech_clone_voices === true)
  );
}

// Whether a catalogue model is a voice-CLONE speech model driven by its configured PRESET voices
// (#301), the one speech model with no text-only default voice: a send with a blank voice relies
// on the operator having configured a default preset, which the gateway cannot see. The one rule
// of the effective model's modelPresetClone and of Voice Chat's choice of voice.
function presetCloneOffered(option: ModelOption | undefined): boolean {
  return (
    option?.speech === true && option.speech_clone === true && option.speech_clone_voices === true
  );
}

// The two models of a Voice Chat that the thread does not pick: the live transcription model
// that hears the user and the streaming speech model that answers aloud (the thread's own model
// is the third, the one that answers). '' is none that the gateway says streams.
type VoiceLegs = { asr: string; tts: string };

// What stands between a thread and a Voice Chat: the thread's own model cannot chat (it is
// unreachable, or no text model), no transcription model streams, no speech model streams, or the
// gateway has not yet said which do. null when nothing does: the conversation can be offered.
export type VoiceChatGap = 'llm' | 'asr' | 'tts' | 'checking';

function voiceGapOf(llmCapable: boolean, legs: VoiceLegs | null): VoiceChatGap | null {
  if (!llmCapable) return 'llm';
  if (legs === null) return 'checking';
  if (legs.asr === '') return 'asr';
  if (legs.tts === '') return 'tts';
  return null;
}

// The first of `ids`, in the catalogue's order, that `streams` says yes to ('' when none does).
// A lookup that fails is a no -- fail closed, like every gate of the composers.
async function firstThatStreams(
  ids: readonly string[],
  streams: (id: string) => Promise<boolean>,
): Promise<string> {
  const answers = await Promise.all(ids.map((id) => streams(id).catch(() => false)));
  const at = answers.indexOf(true);
  return at < 0 ? '' : ids[at];
}

// The per-message actions the transcript's rows are handed (handlersFor): the user
// turn's edit, the assistant turn's regenerate, -- on a generated image -- the
// refine that makes it the source of the next edit, and -- on a spoken answer -- the
// delete of its stored audio.
type MessageHandlers = {
  onEdit: (text: string) => void;
  onRegenerate: () => void;
  onRefine: (url: string) => void;
  onDeleteAudio: () => void;
};

// A tightening of the answer-audio retention that is waiting for the user's word (#281): it
// would remove the stored audio of `excess` answers, which cannot be undone, so it is held
// until confirmAudioRetention or cancelAudioRetention. `keepLast` is the count it would prune
// to (0 = keep none, i.e. do not store); `excess` is how many answers of the open transcript
// hold audio beyond it, recounted as the transcript changes.
export type PendingAudioRetention = { keepLast: number; excess: number };

// The retention half of AudioSettings, the part the composer's retention control edits.
type AudioRetention = Pick<AudioSettings, 'keepAudio' | 'keepLast'>;

// The retention change held for a confirm, and the chat it was asked of.
type HeldRetention = { chatId: string; retention: AudioRetention; keepLast: number };

function sameRetention(a: AudioRetention, b: AudioRetention): boolean {
  return a.keepAudio === b.keepAudio && a.keepLast === b.keepLast;
}

export type ChatStore = {
  messages: ChatUiMessage[];
  // The ACTIVE chat's streaming flag (derived from runningChatIds). Kept as a
  // simple boolean so existing consumers (Chat.tsx / ChatSidebar.tsx) are
  // untouched by the multi-run rework.
  streaming: boolean;
  // Chats with a live server run (any chat, not just the active one). Drives
  // the sidebar's per-chat running indicator (Task 3.6). Always supplied by
  // the provider.
  runningChatIds: Set<string>;
  isChatRunning: (id: string) => boolean;
  input: string;
  images: string[];
  // The transcription composer's clip -- the audio the next send transcribes -- or null. It
  // exists only on a transcription thread (handleFiles takes audio there, and it is dropped when
  // the thread is anything else) and is the WHOLE turn: a transcription send needs it and sends
  // nothing else. `url` is the inline data url the turn will carry, already under its canonical
  // media type (what the preview plays too); `name` is the file's, '' for a recording.
  audio: AudioClip | null;
  // True while an attached clip is being converted to WAV (attachAudioClip): from the pick or the
  // recording's "use" until its clip lands or the attach fails. A long clip takes a moment, and
  // the composer shows that and holds its attach, record and Send controls meanwhile. It is
  // false again as soon as the clip is removed or sent, which abandons the attach in flight.
  audioConverting: boolean;
  model: string;
  systemPrompt: string;
  temperature: number;
  maxTokens: number;
  selectedTokenId: string;
  chatModels: ModelOption[];
  modelOptions: ModelOption[];
  usableTokens: PortalToken[];
  // The caller's manageable servers (see ChatStoreProvider's `servers` prop):
  // the server-override picker in the chat settings panel renders only when
  // this is non-empty, and its options come from it.
  manageableServers: PortalServer[];
  serverOverride: string;
  serverOverrideForceUnreachable: boolean;
  setServerOverride: Dispatch<SetStateAction<string>>;
  setServerOverrideForceUnreachable: Dispatch<SetStateAction<boolean>>;
  // True when the selected run-as token carries its OWN server override: the
  // chat's own server-override controls must be locked (disabled) and display
  // the TOKEN's server/force values instead (see effectiveServerOverride/
  // effectiveServerOverrideForce below) -- the backend's precedence is
  // token-first, so the chat's own picked server would not take effect anyway.
  serverOverrideLocksChat: boolean;
  // The server override actually in effect for the next run: the run-as
  // token's value while serverOverrideLocksChat is true, else the chat's own
  // `serverOverride`. Render this (not `serverOverride`) wherever the EFFECTIVE
  // value matters (the picker's displayed value, the model-dropdown filter).
  effectiveServerOverride: string;
  // Likewise the force-unreachable flag actually in effect.
  effectiveServerOverrideForce: boolean;
  // The offered models of the currently-selected (effective) override server
  // (empty while no override is in effect). Once an override is in effect,
  // feed the chat's model dropdown from THIS list instead of modelOptions.
  serverOverrideModels: ServerModelOption[];
  overrideModel: string;
  // True only for a pure catch-all override (a run-as token with a catch-all and
  // no per-model map entries): the model picker is locked to the forced model.
  // With per-model entries the picker stays editable (the pick is the map key).
  overrideLocksModel: boolean;
  // Whether the effective model (overrideModel || model) is currently among
  // the reachable chatModels. False while it's temporarily unreachable — the
  // model stays selected/visible (see modelOptions) but sending is blocked.
  modelAvailable: boolean;
  // Whether the effective model can process image attachments as INPUT of a text
  // turn. One of the flags attachMode (below) is derived from; the attach gate
  // itself (button disabled, send blocked, images cleared on switch) reads
  // attachMode, not this.
  modelVisionCapable: boolean;
  // Whether the effective model GENERATES images. Independent of
  // modelVisionCapable, which is whether it ACCEPTS them. Gates only what the
  // composer NEWLY OFFERS (the image-prompt affordances on an unpinned
  // thread) and feeds attachMode -- never what an existing thread DOES: that is
  // chatKind's job.
  modelImageCapable: boolean;
  // Whether the effective model may be the target of an image EDIT: the backend's
  // derived, operator-removable `image_edit` (ModelOption.image_edit, folded like
  // `image`), read as it is and never recomputed here -- the routing gate reads the
  // same derivation, so the composer never offers an edit the gate would refuse.
  // Independent of modelVisionCapable (an sd model edits and is vision:false).
  modelImageEdit: boolean;
  // Whether the effective model SYNTHESIZES speech (the backend's `speech` verdict,
  // ModelOption.speech, read as served). The speech twin of modelImageCapable: it gates
  // only what the composer NEWLY OFFERS on an unpinned thread (the speech composer, via
  // prospectiveKind) -- never what an existing thread DOES: that is chatKind's job.
  modelSpeechCapable: boolean;
  // Whether the effective model TRANSCRIBES speech (the backend's `transcription` verdict,
  // ModelOption.transcription, read as served). The speech flag's twin: it gates only what the
  // composer NEWLY OFFERS on an unpinned thread (the transcription composer, via
  // prospectiveKind) -- never what an existing thread DOES: that is chatKind's job.
  modelTranscriptionCapable: boolean;
  // What an attached image MEANS for the next send: 'edit' (the source of an image
  // edit), 'vision' (an input a text model reads) or 'none' (not accepted). One
  // predicate of chatKind and the model's flags (attachModeOf, chatDoc.ts) that
  // the attach control, the clear on a model switch, send() and the replay guard
  // all read. See ADR-043 (a), R1.
  attachMode: AttachMode;
  // The per-model sd-server image options (samplers / schedulers / output
  // formats / size limits / batch ceiling / defaults) of the effective model,
  // fetched on demand while that model GENERATES images (api.modelImageOptions).
  // null whenever there is nothing to narrow by: not an image model, the fetch
  // is in flight, or it failed. Every field of a fetched value is optional too
  // ("unknown"), so the composer's dropdowns merge it over their static
  // Phase 1b-i superset (optionsOr) rather than trusting it to be complete.
  modelImageOptions: ImageModelOptions | null;
  // The voice names the gateway has cached for the effective model, fetched on demand
  // while that model SYNTHESIZES speech (api.modelVoices), for the speech composer's
  // voice picker. Empty whenever there is nothing to offer: not a speech model, the
  // fetch is in flight, it failed, or the model has no cached voices. The picker is
  // then plain free text -- a list is only ever an offer, never a constraint, since
  // the cache holds just the voices the health loop has seen.
  modelVoices: readonly string[];
  // Whether the speech composer may offer STREAMING playback for the effective model: the
  // gateway's own answer (api.modelSpeechStreaming) that every mapping the model could be
  // served by streams its audio AND has its output rate known, which is what makes the
  // headerless PCM playable (ADR-068). False whenever there is nothing to offer -- not a
  // speech model, the lookup is in flight, it failed (fail closed), or the gateway said no --
  // and the thread then runs the buffered WAV run it always has. The gateway decides again
  // at run time; this only decides whether the toggle exists and whether the browser's audio
  // is readied when a turn is sent.
  speechStreaming: boolean;
  // Whether the speech composer may offer the LANGUAGE control for the effective model: the
  // gateway's own answer (api.modelSpeechLanguage) that the model takes a `language`
  // parameter. False whenever there is nothing to offer -- not a speech model, the lookup is
  // in flight, it failed (fail closed), or the gateway said no -- and the composer then has no
  // language field, so nothing is shown or pre-filled for a model that would drop the value.
  // The same answer's declared default drives the pre-fill of an empty speech thread (see the
  // store's pre-fill effect): this only decides whether the control exists.
  speechLanguage: boolean;
  // Whether the transcription composer may offer a LIVE recording for the effective model: the
  // gateway's own answer (api.modelTranscriptionStreaming) that every mapping the model could
  // be served by is known to stream a transcription (#266). The speech gate's twin, minus the
  // rate -- the browser fixes the PCM it streams. False whenever there is nothing to offer --
  // not a transcription model, the lookup is in flight, it failed (fail closed), or the gateway
  // said no -- and the thread then records and transcribes the buffered clip it always has. The
  // gateway decides again when the live ticket is minted; this only decides whether the live
  // mode exists.
  transcriptionStreaming: boolean;
  // The THREAD's kind: "" (text), "image", "speech" or "transcription". Pinned by the backend
  // at the thread's first send and authoritative from then on; while the thread is
  // still empty it follows the picked model, because that is what the next
  // send will pin it to. Render composer affordances off THIS, not off
  // modelImageCapable — a text thread stays a text thread even if the user
  // later picks an image, speech or transcription model.
  chatKind: string;
  // The image-generation knobs of the active thread (size / n / output_format /
  // output_compression), as concrete store state. The settings panel edits them
  // when chatKind === "image"; they are omitted from the wire and persistence
  // for a text thread (see imageSetting in chatDoc.ts).
  imageSettings: ImageSettings;
  setImageSettings: Dispatch<SetStateAction<ImageSettings>>;
  // The text-to-speech knobs of the active thread (voice / speed / passthrough text /
  // retention), as concrete store state. The speech composer edits them when chatKind ===
  // "speech"; they are omitted from the wire and persistence for any other kind (see
  // audioSetting in chatDoc.ts). A passthrough that is not a JSON object blocks the send
  // (see send()).
  //
  // These are the settings AS THE COMPOSER SHOWS THEM: a retention change that would remove
  // stored audio (pendingAudioRetention below) shows here while it waits for its confirm, so
  // the count being typed stays in its field, but it is neither saved nor sent until then --
  // the thread's own settings stay what they were.
  audioSettings: AudioSettings;
  // Edits the settings, from the composer's controls. A change of the retention to a stricter
  // value (fewer answers keep their audio, judged on the wire's keep_last_audio, not the raw
  // text) that the open chat's stored audio exceeds is HELD rather than applied; everything
  // else, a loosening included, is applied at once and never deletes anything.
  setAudioSettings: Dispatch<SetStateAction<AudioSettings>>;
  // The held stricter retention (see PendingAudioRetention), or null. The composer asks the
  // user about it; confirmAudioRetention persists it and prunes the chat's stored audio to it,
  // cancelAudioRetention puts the control back as it was and persists and deletes nothing.
  pendingAudioRetention: PendingAudioRetention | null;
  confirmAudioRetention: () => void;
  cancelAudioRetention: () => void;
  // The speech-to-text knobs of the active thread (language / prompt / temperature /
  // passthrough text), as concrete store state: the audio settings' twin. The transcription
  // composer edits them when chatKind === "transcription"; they are omitted from the wire and
  // persistence for any other kind (see transcriptionSetting in chatDoc.ts). A passthrough
  // that is not a JSON object, or that names a field the run owns, blocks the send (see send()).
  transcriptionSettings: TranscriptionSettings;
  setTranscriptionSettings: Dispatch<SetStateAction<TranscriptionSettings>>;
  // The active chat's LIVE RUN's own reported kind ("" text | "image" | "speech" |
  // "transcription"), read from useChatRuns' kindOf -- deliberately NOT chatKind above.
  // chatKind is the client's own "is this thread's first send" guess
  // (messagesRef.current.length === 0) and can disagree with the server's
  // (len(doc.Messages) > 0): a second tab whose transcript view is stale
  // after another tab's first send would still guess from the picked model
  // and could pin the wrong kind locally, and a client whose optimistic pin
  // survived a rolled-back send carries the same staleness. PrepareChatRun
  // forces the thread's real, pinned kind server-side on every send after
  // the first, and the run's own reports (the 201 / an active-runs entry /
  // an SSE snapshot) are that authoritative, post-force value -- exactly
  // what the executor is actually running. Undefined only when the active
  // chat has no run-registry entry at all.
  //
  // Chat.tsx passes this to ChatMessage ONLY for the row that is actually
  // streaming right now, the same gate as the `streaming` prop itself:
  // passing it to every row unconditionally would hand every ChatMessage a
  // prop that changes with `streaming`'s own churn and defeat its memo for
  // rows that are not the one in flight.
  runKind: string | undefined;
  // The active chat's live run elapsed ms, anchored on the run's own last
  // server-reported measurement and interpolated to "now" at render time
  // (see useChatRuns' elapsedMsOf). Undefined only when the active chat has
  // NO run-registry entry at all (never run, or forgotten) -- a run that has
  // gone terminal but is still lingering in the registry (the eviction
  // grace) returns its frozen final value here, not undefined. Gated into
  // ChatMessage exactly like runKind above, and for the same reason: this
  // value is recomputed on every render and would otherwise be a "different
  // float essentially every time", defeating ChatMessage's memo for the
  // WHOLE transcript on every token delta of any live run, not just the row
  // in flight.
  runElapsedMs: number | undefined;
  // Whether the active chat's running speech run is playing its audio live right now (a
  // streaming model's chunks, played as they arrive). The pending turn then says so instead
  // of "no intermediate news". False for every other run and once the run is over; gated into
  // ChatMessage like runKind above, for the same memo reason.
  runAudioLive: boolean;
  // Whether the user has muted that live playback (#281): the pending turn's Mute control
  // shows Unmute then. Local to this tab's speakers; the run is unaffected. Gated into
  // ChatMessage like runAudioLive.
  runAudioMuted: boolean;
  // Mute or unmute a chat's live speech playback (the Mute control; reversible). It is NOT
  // the abort: aborting a live speech run is `stop`, the ordinary cancel.
  muteAudio: (chatId: string, muted: boolean) => void;
  // Stop a chat's streamed audio once the run itself is already done and only its tail is still
  // playing out (#281): the Abort of the live controls after the run committed. It silences the
  // tail and clears the live mark, and touches neither the run nor the stored answer. While the
  // run is still running, Abort is `stop` (the cancel) instead.
  abortAudio: (chatId: string) => void;
  // Multi-chat layer.
  chats: ChatSummary[];
  activeChatId: string | null;
  chatsLoading: boolean;
  selectChat: (id: string) => void;
  deleteChat: (id: string) => void;
  renameChat: (id: string, title: string) => void;
  send: () => void;
  stop: () => void;
  newChat: () => void;
  setInput: Dispatch<SetStateAction<string>>;
  // Attach what the composer's attach control picked: on a transcription thread the first file
  // is the audio clip (replacing the one it had), anywhere else the images of a text or an
  // image-edit turn. Refusals (a format, a size) are toasts.
  handleFiles: (fileList: FileList | null) => Promise<void>;
  removeImage: (index: number) => void;
  // Attach a browser recording (the in-browser recorder's blob, typed as the recorder reports it:
  // audio/webm;codecs=opus) as the transcription turn's clip, by the same path and refusals as a
  // picked file. removeAudio drops the clip again.
  attachRecording: (blob: Blob) => Promise<void>;
  removeAudio: () => void;
  // The LIVE recording of a transcription thread (#266): the microphone streamed to the gateway
  // while the user speaks, with the transcript appearing as they talk. Offered by the composer
  // only while transcriptionStreaming is true. `liveSession` is its state and the transcript so
  // far (across reconnects); start it from the user's gesture (the click that asks for the
  // microphone), stop it to have the transcript committed as a completed turn of the chat (the
  // gateway's live endpoint has done, and billed, the work: no chat run is started), abort it to
  // throw it away. A connection that drops while recording is reconnected, a few times, as a new
  // session; the error state keeps what was transcribed, and keepLiveTranscript commits it as a
  // turn anyway. The recording belongs to the thread it started in: another chat or model, or live
  // going off offer, ends it.
  liveSession: LiveTranscription;
  startLiveTranscription: () => void;
  stopLiveTranscription: () => void;
  abortLiveTranscription: () => void;
  keepLiveTranscript: () => void;
  // VOICE CHAT (#312): a spoken conversation with the thread's model -- the microphone streamed
  // to the gateway, which hears the user (a live transcription model), has the thread's model
  // answer, and speaks the answer back (a streaming speech model), turn after turn, committing
  // each turn to the thread itself. `voiceChat` is the conversation: its state (whether the
  // system is listening), what is being said and answered, and its controls -- start it from the
  // user's gesture, as it asks for the microphone. It belongs to the thread it started in: another
  // chat, or another model, ends it.
  voiceChat: VoiceChatApi;
  // Whether this thread is one a Voice Chat could ever be offered on: a text thread, on a
  // catalogue that has any transcription or speech model at all. The view shows nothing for a
  // thread that is not (no disabled switch for a gateway with no audio at all).
  voiceChatOffered: boolean;
  // Whether the thread can do one: its model chats and is reachable, and the gateway says that a
  // transcription model and a speech model stream. The models the conversation then uses are the
  // thread's own, and the first transcription and speech models (in the catalogue's order) that
  // stream; the gateway decides again when the conversation's ticket is minted (it holds all
  // three in memory together, or refuses), which is the real admission. Equals
  // `voiceChatGap === null`.
  voiceChatCapable: boolean;
  // What is missing when it cannot (see VoiceChatGap), null when nothing is.
  voiceChatGap: VoiceChatGap | null;
  // The mask of an image EDIT (inpainting): one PNG data url, or null. It exists
  // only beside a source image in edit mode (handleMaskFile refuses it otherwise,
  // and it is dropped with its source or when the mode changes) and is sent as a
  // part of the turn tagged as a mask (chatDoc.ts maskPart). Distinct from `images`,
  // which are the sources.
  mask: string | null;
  handleMaskFile: (fileList: FileList | null) => Promise<void>;
  removeMask: () => void;
  // A counter the composer watches: it moves each time a refine seeds the composer,
  // which is the view's cue to put the cursor in the prompt field (the user's next
  // move is the instruction, and the composer may be far below the image they clicked).
  // Only its CHANGE means anything; it is 0 until the first refine.
  composerFocusRequest: number;
  setModel: Dispatch<SetStateAction<string>>;
  setSystemPrompt: Dispatch<SetStateAction<string>>;
  setTemperature: Dispatch<SetStateAction<number>>;
  setMaxTokens: Dispatch<SetStateAction<number>>;
  setSelectedTokenId: Dispatch<SetStateAction<string>>;
  handlersFor: (id: string) => MessageHandlers;
};

// The user-facing sentence for a failed attachment: the reason an image could not be
// prepared (type, size, or a decode failure -- or anything else, which is generic).
function attachErrorText(err: unknown, t: Translation): string {
  if (err instanceof ImageAttachError) {
    switch (err.reason) {
      case 'type':
        return t.chatImageErrorType;
      case 'size':
        return t.chatImageErrorSize;
      default:
        // "decode" (or any future reason) -> generic
        return t.chatImageError;
    }
  }
  return t.chatImageError;
}

// The user-facing sentence for a refused audio clip: its format, its size, an empty file, audio
// the browser could not decode to WAV -- or anything else, which is generic.
function audioAttachErrorText(err: unknown, t: Translation): string {
  if (err instanceof AudioAttachError) {
    switch (err.reason) {
      case 'type':
        return t.chatAudioErrorType;
      case 'size':
        return t.chatAudioErrorSize;
      case 'empty':
        return t.chatAudioErrorEmpty;
      case 'decode':
        return t.chatAudioErrorDecode;
      default:
        // "read" (or any future reason) -> generic
        return t.chatAudioError;
    }
  }
  return t.chatAudioError;
}

// Exported so tests can inject a store value directly (e.g. the Chat view)
// without spinning up the full stream driver.
export const ChatStoreContext = createContext<ChatStore | null>(null);

// The streaming flag lives in its OWN context with a primitive value, kept
// separate from the full store on purpose: the store `value` is a fresh object
// on every provider render (once per streamed token), so a consumer reading it
// would re-render per token. `useChatStreaming` reads this boolean context
// instead, so a per-token-only consumer like NavSidebar re-renders only when
// streaming actually flips. Default false = safe outside any provider.
export const ChatStreamingContext = createContext<boolean>(false);

export function useChatStore(): ChatStore {
  const c = useContext(ChatStoreContext);
  if (!c) throw new Error('useChatStore must be used within ChatStoreProvider');
  return c;
}

// Safe outside a provider (e.g. NavSidebar renders whether or not the chat store
// is mounted): no provider -> false. Subscribes ONLY to the streaming boolean,
// not the per-token store value (see ChatStreamingContext above).
export function useChatStreaming(): boolean {
  return useContext(ChatStreamingContext);
}

export function ChatStoreProvider({
  api,
  models,
  tokens,
  servers,
  onRefresh,
  refreshModels,
  t,
  locale,
  children,
}: Readonly<{
  api: Pick<
    PortalApi,
    | 'activeChatRuns'
    | 'cancelChatRun'
    | 'chat'
    | 'chats'
    | 'createChat'
    | 'createLiveTranscriptionTicket'
    | 'createVoiceChatTicket'
    | 'deleteChat'
    | 'deleteMessageAudio'
    | 'modelImageOptions'
    | 'modelSpeechLanguage'
    | 'modelSpeechStreaming'
    | 'modelTranscriptionStreaming'
    | 'modelVoices'
    | 'pruneChatAudio'
    | 'saveChat'
    | 'saveChatKeepalive'
    | 'serverModels'
    | 'startChatRun'
  >;
  models: ModelOption[];
  tokens: PortalToken[];
  // The caller's MANAGEABLE servers (api.servers() -> ListServers, which is
  // already manager-scoped server-side). Drives whether the chat's
  // server-override picker renders at all (hidden with zero manageable
  // servers) and its option list. Optional so pre-existing test renders that
  // never touch the picker keep working unchanged; defaults to [].
  servers?: PortalServer[];
  // Called after every stream ends (reloads tokens/models). Accepts a sync
  // fire-and-forget refresh (App passes the SILENT loader) or an async one; the
  // store awaits it either way.
  onRefresh: () => void | Promise<void>;
  // Lightweight models-only refresh (no token/dashboard/server reload), polled
  // while the effective model is unavailable so the "!" clears automatically
  // once the upstream recovers. Optional so existing test renders that don't
  // care about recovery keep working unchanged.
  refreshModels?: () => void;
  t: Translation;
  // The portal's current UI language (the one `t` is in). The speech composer pre-fills its
  // language control with it (see the pre-fill effect), so a spoken answer follows the
  // language the user reads the portal in unless they say otherwise.
  locale: Locale;
  children: ReactNode;
}>) {
  // The active chat's live state. Content is loaded from the server (defaults
  // until the first chat is activated); the legacy localStorage seed is handled
  // in the one-time migration effect below, not here.
  const [model, setModel] = useState<string>(DEFAULTS.model);
  const [systemPrompt, setSystemPrompt] = useState<string>(DEFAULTS.system_prompt);
  const [temperature, setTemperature] = useState<number>(DEFAULTS.temperature);
  const [maxTokens, setMaxTokens] = useState<number>(DEFAULTS.max_tokens);
  // The active image thread's generation knobs (concrete, see ImageSettings).
  // Seeded from the loaded document in activateChat, sent on run via
  // currentSettings, persisted via buildDoc -- all gated on chatKind "image".
  const [imageSettings, setImageSettings] = useState<ImageSettings>(DEFAULT_IMAGE_SETTINGS);
  // The active speech thread's text-to-speech knobs (concrete, see AudioSettings): the
  // image settings' twin, seeded in activateChat, sent via currentSettings, persisted
  // via buildDoc -- all gated on chatKind "speech".
  //
  // The COMMITTED settings: what is saved and sent. The store's setAudioSettings (below) is the
  // only writer the composer gets, so a retention change that waits for a confirm never lands
  // here before it; activateChat seeds it directly.
  const [audioSettingsState, setAudioSettingsState] =
    useState<AudioSettings>(DEFAULT_AUDIO_SETTINGS);
  // The stricter retention waiting for the user's confirm (see setAudioSettings), kept beside
  // the committed settings and laid over them for display only. A ref mirrors it so the
  // setters and the confirm read the latest value between renders.
  const [heldRetentionState, setHeldRetentionState] = useState<HeldRetention | null>(null);
  const heldRetentionRef = useRef<HeldRetention | null>(null);
  const setHeldRetention = useCallback((held: HeldRetention | null) => {
    heldRetentionRef.current = held;
    setHeldRetentionState(held);
  }, []);
  // The active transcription thread's speech-to-text knobs (concrete, see
  // TranscriptionSettings): the audio settings' twin, seeded in activateChat, sent via
  // currentSettings, persisted via buildDoc -- all gated on chatKind "transcription".
  const [transcriptionSettings, setTranscriptionSettings] = useState<TranscriptionSettings>(
    DEFAULT_TRANSCRIPTION_SETTINGS,
  );
  const [messages, setMessages] = useState<ChatUiMessage[]>([]);
  const [input, setInput] = useState('');
  const [images, setImages] = useState<string[]>([]);
  // The transcription composer's clip (see ChatStore.audio).
  const [audio, setAudio] = useState<AudioClip | null>(null);
  // Whether the NEWEST attach's conversion is in flight (see ChatStore.audioConverting), and the
  // number of the newest attach: every attachAudioClip takes the next one, and a conversion that
  // finds it is no longer the newest on finishing is superseded and discarded. A clip that is
  // removed or sent takes a number too (abandonAudioAttach), which voids the attach in flight.
  const [audioConverting, setAudioConverting] = useState(false);
  const audioAttachSeqRef = useRef(0);
  // Voids the audio attach in flight, if any (the conversion carries on, but its result no longer
  // counts): the clip it was to land was removed or sent. Nothing is converting from then on.
  const abandonAudioAttach = useCallback(() => {
    audioAttachSeqRef.current += 1;
    setAudioConverting(false);
  }, []);
  const [mask, setMask] = useState<string | null>(null);
  const [composerFocusRequest, setComposerFocusRequest] = useState(0);
  const [selectedTokenId, setSelectedTokenId] = useState<string>(DEFAULTS.run_as_token_id);
  const [serverOverride, setServerOverride] = useState<string>(DEFAULTS.server_override);
  const [serverOverrideForceUnreachable, setServerOverrideForceUnreachable] = useState<boolean>(
    DEFAULTS.server_override_force_unreachable,
  );
  // The distinct gateway models the selected override server offers (see
  // api.serverModels), used to narrow the chat's model dropdown once a server
  // override is picked. Empty while no override is set.
  const [serverOverrideModels, setServerOverrideModels] = useState<ServerModelOption[]>([]);
  // The effective image model's per-model sd-server options (see api.modelImageOptions),
  // used to narrow the composer's image dropdowns. null while there is none.
  const [modelImageOptions, setModelImageOptions] = useState<ImageModelOptions | null>(null);
  // The effective speech model's cached voice names (see api.modelVoices), offered by the
  // speech composer's voice picker. Empty while there are none.
  const [modelVoices, setModelVoices] = useState<readonly string[]>(NO_VOICES);
  // The effective speech model's streaming gate (see api.modelSpeechStreaming): closed
  // until the gateway says otherwise.
  const [speechStreamingGate, setSpeechStreamingGate] =
    useState<SpeechStreamingGate>(NO_SPEECH_STREAMING);
  const speechStreamingRef = useRef(false);
  // The effective speech model's language gate (see api.modelSpeechLanguage): closed until
  // the gateway says otherwise.
  const [speechLanguageGate, setSpeechLanguageGate] =
    useState<SpeechLanguageGate>(NO_SPEECH_LANGUAGE);
  // The effective transcription model's live gate (see api.modelTranscriptionStreaming):
  // closed until the gateway says otherwise. A plain boolean: unlike the speech gate there is
  // no rate to carry, so nothing reads it between renders and it needs no ref.
  const [transcriptionStreaming, setTranscriptionStreaming] = useState(false);
  // The ears and the voice of a Voice Chat (see VoiceLegs), as the gateway's streaming gates
  // answered for the catalogue's transcription and speech models: null until they have (and
  // while they are asked again), so the answer is closed until the gateway says otherwise.
  const [voiceLegs, setVoiceLegs] = useState<VoiceLegs | null>(null);

  // The caller's manageable servers (undefined prop -> []): drives whether the
  // server-override picker renders at all and its option list.
  const manageableServers = useMemo(() => servers ?? [], [servers]);

  // The THREAD's pinned kind as last loaded/established ("" text | "image" | "speech" |
  // "transcription").
  // Real state, not a value read once at load: the pin is established by the
  // thread's FIRST send, and a live session (new chat -> pick an image model
  // -> send -> send again -> regenerate, no reload) must see it immediately.
  // Opaque string, never narrowed to the kinds this build knows, so an
  // unfamiliar one survives the save round trip instead of being erased. ""
  // is text: the SETTING is optional (omitted for a text thread, mirroring
  // the backend's omitempty) but the state is always a string, so nothing
  // downstream has to handle undefined.
  const [pinnedChatKind, setPinnedChatKind] = useState<string>('');

  // The chat list + which one is active.
  const [chats, setChats] = useState<ChatSummary[]>([]);
  const [activeChatId, setActiveChatId] = useState<string | null>(null);
  const [chatsLoading, setChatsLoading] = useState(true);

  const { showError } = useToast();

  // A model is offered when the chat can send it something the gateway will
  // serve: a text request (chat completions, the openai flavor) or an image
  // request (the openai_images flavor) or a speech or transcription request (the
  // openai_audio flavor). offeredFlavors keeps openai_images only behind an image
  // verdict, because the images gate refuses a model without image: yes -- an
  // images-only model (an sd-server, whose upstream has no chat endpoint) without
  // that verdict could only ever produce a request that fails. openai_audio is the
  // same story and is admitted only behind a speech or a transcription verdict
  // (the two endpoints are both served under that one flavor, and each gate
  // refuses a model without its own verdict), but that rule is the CHAT's alone
  // and lives here: offeredFlavors passes openai_audio through because it is
  // shared with the Models view, whose "Available via" column must keep showing
  // an audio row's flavor (unprobed ones and agent-managed ones whose agent has
  // not reported either verdict have none and are listed there anyway).
  // anthropic alone offers nothing: the chat sends no /v1/messages request.
  const chatModels = useMemo(
    () =>
      models.filter((option) => {
        const offered = offeredFlavors(option);
        return (
          offered.includes('openai') ||
          offered.includes('openai_images') ||
          // An openai_audio model is offered behind a speech OR transcription
          // verdict — except a voice-CLONE speech model (speech_clone), which the
          // text-only speech composer cannot drive (it needs a reference voice),
          // so it is excluded from the speech half (#252) UNLESS it has configured
          // preset voices (speech_clone_voices), which make it usable without a
          // reference voice (#301). A clone model that also transcribes is still
          // offered via the transcription half.
          (offered.includes('openai_audio') &&
            (speechOffered(option) || option.transcription === true))
        );
      }),
    [models],
  );
  // The dropdown's option list. A non-empty `model` that is not (or no longer)
  // in chatModels is injected as a synthetic trailing option so SearchableSelect
  // keeps showing + selecting it (a value not in `options` renders blank) —
  // this is what lets an unavailable saved model stay visible instead of
  // disappearing. Falls back to a single fabricated option when chatModels is
  // empty (e.g. before the list has loaded, or everything is unreachable), same
  // as before, but now also covers a saved model in that fallback.
  const modelOptions: ModelOption[] = useMemo(() => {
    const savedMissing = model !== '' && !chatModels.some((option) => option.id === model);
    // Keep a saved-but-currently-missing model visible (injected) so it stays
    // selected while unreachable; otherwise just the real models. An empty
    // selection injects nothing (the field is intentionally clearable/searchable).
    if (savedMissing)
      return [
        ...chatModels,
        { id: model, display_name: model, flavors: ['openai'], loading_on_count: 0 },
      ];
    return chatModels;
  }, [chatModels, model]);

  // Only the caller's own active gateway:use tokens may be selected as a
  // run-as target (mirrors the backend's AuthorizeRunAsToken check).
  const usableTokens = useMemo(
    () => tokens.filter((tk) => tk.status === 'active' && tk.scopes.includes('gateway:use')),
    [tokens],
  );
  const selectedToken = usableTokens.find((tk) => tk.id === selectedTokenId);
  // The run-as token's override for the CURRENTLY-selected model: an exact
  // per-model map entry wins, otherwise the catch-all (model_override), otherwise
  // none. Mirrors the backend's resolveModelOverride so the effective model shown
  // matches what the server will route (the server does the actual remap via the
  // run-as token; the chat sends the raw picked model, see currentSettings).
  const overrideModel = resolveTokenOverride(selectedToken, model);
  // A pure catch-all (a catch-all with no per-model entries) forces every model, so
  // the model picker is locked to it (old single-override behavior). With per-model
  // entries the picked model IS the map key, so the picker stays editable.
  const overrideMapHasEntries =
    !!selectedToken?.model_override_map && Object.keys(selectedToken.model_override_map).length > 0;
  const overrideLocksModel =
    !!selectedToken && !overrideMapHasEntries && (selectedToken.model_override ?? '') !== '';

  // Whether the run-as token carries its OWN server override: the backend's
  // precedence is token-first (a chat-header override loses to the token's
  // stored one — see applyServerOverride), so when the token has one, the
  // chat's own server-override controls are locked to display the token's
  // values instead of the chat's own picked server, mirroring overrideLocksModel
  // above. The chat's own `serverOverride`/`serverOverrideForceUnreachable`
  // state is left untouched while locked (it is what the picker reverts to once
  // the token is cleared).
  const serverOverrideLocksChat = !!selectedToken && (selectedToken.server_override ?? '') !== '';
  // The server override actually in effect for the next run: the token's value
  // while locked, else the chat's own picked server.
  const effectiveServerOverride = serverOverrideLocksChat
    ? (selectedToken?.server_override ?? '')
    : serverOverride;
  const effectiveServerOverrideForce = serverOverrideLocksChat
    ? !!selectedToken?.server_override_force_unreachable
    : serverOverrideForceUnreachable;

  // The model actually used for the next run (an active run-as override wins
  // over the chat's own `model`), and whether it currently appears in the
  // reachable-models list. Drives the red "!" indicator and the send guard.
  const effectiveModel = overrideModel || model;
  const modelAvailable =
    effectiveModel !== '' && chatModels.some((option) => option.id === effectiveModel);
  // Whether the effective model can process images (drives the chat image gate).
  // A model/group is vision-capable only when ALL its mappings/members are (the
  // backend already AND-aggregates this into ModelOption.vision).
  const modelVisionCapable =
    chatModels.find((option) => option.id === effectiveModel)?.vision ?? false;
  // Whether the effective model GENERATES images (drives the image-thread
  // composer). Independent of modelVisionCapable, which is whether it ACCEPTS
  // them; the backend AND-aggregates both into ModelOption.
  const modelImageCapable =
    chatModels.find((option) => option.id === effectiveModel)?.image ?? false;
  // Whether the effective model may be EDITED (ModelOption.image_edit, derived in
  // the backend from `image` unless an operator opted out). Read as served: the
  // gate and this flag are one derivation (routing.ImpliedImageEdit).
  const modelImageEdit =
    chatModels.find((option) => option.id === effectiveModel)?.image_edit ?? false;
  // Whether the effective model SYNTHESIZES speech (drives the speech-thread composer),
  // read as served like `image`: the backend's own fold of the speech verdict, minus a
  // voice-CLONE model (speech_clone), which the text-only speech composer cannot drive —
  // the same exclusion the picker applies (#252) — unless it has configured preset voices
  // (speech_clone_voices), the same exception the picker makes (#301). A clone model admitted
  // to the picker only by a manual transcription override would otherwise open the speech
  // composer here (the kind precedence is image → speech → transcription).
  const effectiveOption = chatModels.find((option) => option.id === effectiveModel);
  const modelSpeechCapable = speechOffered(effectiveOption);
  // Whether the effective model is a voice-CLONE model driven by its configured PRESET voices
  // (#301; presetCloneOffered). The voices effect below starts such a model on the first preset
  // voice it lists.
  const modelPresetClone = presetCloneOffered(effectiveOption);
  // Whether the effective model TRANSCRIBES speech (drives the transcription-thread composer),
  // read as served like `speech`: the backend's own fold of the transcription verdict.
  const modelTranscriptionCapable =
    chatModels.find((option) => option.id === effectiveModel)?.transcription ?? false;

  // The kind the NEXT send would pin, derived from the picked model. Only
  // ever consulted while the thread has no transcript at all (below), which
  // is why the synthetic model option injected for a picked-but-uncatalogued
  // model — no capability fields, so image === false — is harmless here: the
  // PINNED kind is the thread's truth the moment one exists.
  //
  // The precedence is image -> speech -> transcription, each kind the later one never
  // displaces: a model both image- and speech-capable is an image model here (image was the
  // kind first, so a model that gains a speech verdict never changes what an existing image
  // model does), and one both speech- and transcription-capable is a speech model. Nothing in
  // the catalogue carries two verdicts today: the audio.cpp probe gives an asr model a
  // transcription verdict only and a tts model a speech one only, so two arise solely from
  // manual configuration, and then the later mode is simply unreachable for that one model.
  let prospectiveKind = '';
  if (modelImageCapable) prospectiveKind = 'image';
  else if (modelSpeechCapable) prospectiveKind = 'speech';
  else if (modelTranscriptionCapable) prospectiveKind = 'transcription';
  // The thread's kind, and the whole distinction in one line: once the thread
  // has a transcript the backend has already pinned it (PrepareChatRun forces
  // the stored kind on every send after the first), so the pin wins and the
  // currently-picked model is irrelevant; while it is still empty there is no
  // pin yet, so the composer shows what the next send would establish.
  const chatKind = messages.length > 0 ? pinnedChatKind : prospectiveKind;
  // What an attached image means for the next send (see attachModeOf): derived
  // from the thread's kind, so an attachment follows the THREAD, never merely the
  // picked model.
  const attachMode = attachModeOf(chatKind, {
    vision: modelVisionCapable,
    image: modelImageCapable,
    imageEdit: modelImageEdit,
  });

  // VOICE CHAT (#312): what the thread, and the catalogue, can do for a spoken conversation.
  //
  // The thread's own model answers, so it must chat (the `openai` flavor it is offered under: an
  // audio- or images-only model has none) and be reachable, on a TEXT thread: the conversation
  // commits its turns as the text turns of the thread, which a speech, transcription or image
  // thread would not read as one.
  const voiceLlmCapable =
    modelAvailable &&
    chatKind === '' &&
    effectiveOption !== undefined &&
    offeredFlavors(effectiveOption).includes('openai');
  // The catalogue's candidates for the other two, the same models the chat's own composers offer
  // (speechOffered, the transcription verdict). Which of them STREAM is the gateway's to say.
  //
  // The speech candidates are ordered PLAIN models first, preset-CLONE ones (presetCloneOffered)
  // after, each group in the catalogue's order, since the first that streams is the one used: a
  // conversation's text thread carries no audio settings, so a plain model speaks in its default
  // voice while a preset clone would run with a BLANK voice and fail the first turn. A clone stays
  // eligible, for a catalogue with no plain streaming model (its first turn then relies on the
  // operator's default preset).
  const voiceAsrCandidates = useMemo(
    () => chatModels.filter((option) => option.transcription === true).map((option) => option.id),
    [chatModels],
  );
  const voiceTtsCandidates = useMemo(() => {
    const offered = chatModels.filter(speechOffered);
    return [
      ...offered.filter((option) => !presetCloneOffered(option)),
      ...offered.filter(presetCloneOffered),
    ].map((option) => option.id);
  }, [chatModels]);
  const voiceChatGap = voiceGapOf(voiceLlmCapable, voiceLegs);
  const voiceChatCapable = voiceChatGap === null;
  const voiceChatOffered =
    chatKind === '' && (voiceAsrCandidates.length > 0 || voiceTtsCandidates.length > 0);

  // Refs mirror the latest state so the stable callbacks below (send / edit /
  // regenerate / save) can read current values without being re-created every
  // render.
  const messagesRef = useRef(messages);
  const modelRef = useRef(model);
  const systemPromptRef = useRef(systemPrompt);
  const temperatureRef = useRef(temperature);
  const maxTokensRef = useRef(maxTokens);
  const imageSettingsRef = useRef(imageSettings);
  const audioSettingsRef = useRef(audioSettingsState);
  const transcriptionSettingsRef = useRef(transcriptionSettings);
  const selectedTokenIdRef = useRef(selectedTokenId);
  const serverOverrideRef = useRef(serverOverride);
  const serverOverrideForceUnreachableRef = useRef(serverOverrideForceUnreachable);

  const chatsRef = useRef(chats);
  const activeChatIdRef = useRef(activeChatId);
  // The active chat's current title (used by saves + auto-title); not React
  // state because it is only read in callbacks, never rendered directly.
  const activeTitleRef = useRef<string>('');
  // Monotonic token for async chat loads (selectChat). Each load captures the
  // current value; when it resolves it only activates if it is still the latest
  // request, so rapid clicks can't let a slow fetch win out of order.
  const loadReqRef = useRef(0);

  // api / onRefresh / t are read through refs so the stream + save callbacks
  // stay stable and always see the latest values (a background stream may
  // finish after a locale change or a fresh loadPortalData binding).
  const apiRef = useRef(api);
  apiRef.current = api;
  const onRefreshRef = useRef(onRefresh);
  onRefreshRef.current = onRefresh;
  const refreshModelsRef = useRef(refreshModels);
  refreshModelsRef.current = refreshModels;
  const tRef = useRef(t);
  tRef.current = t;
  // The UI language is read by the language pre-fill, which must not re-run when only the
  // portal's language changes (a field the user cleared would fill itself again).
  const localeRef = useRef(locale);
  localeRef.current = locale;
  const showErrorRef = useRef(showError);
  showErrorRef.current = showError;
  // Mirror the derived availability so the stable send/edit/regenerate callbacks
  // (which read via refs, not props) can gate on the latest value.
  const modelAvailableRef = useRef(modelAvailable);
  modelAvailableRef.current = modelAvailable;
  // Whether a microphone session of this thread is under way: a live recording (the
  // transcription composer's) or a Voice Chat conversation. Either commits to the thread by
  // itself, so the replay callbacks refuse while one runs. The recording (useLiveTranscription,
  // below) and the conversation (useVoiceChat) are created after those callbacks, so they read
  // it through this ref, set at render like the mirrors around it; send() reads the same fact
  // from the sessions' own states.
  const audioSessionRunningRef = useRef(false);
  // The chat of the Voice Chat conversation that was started last, and the chat that conversation
  // is holding now (null once it is off). A conversation holds its chat the way a live run does:
  // the gateway owns the thread's transcript while it commits turns, so this tab neither saves
  // the chat over it nor starts a run of its own in it (see isTranscriptHeld).
  const voiceChatIdRef = useRef<string | null>(null);
  const voiceHeldChatRef = useRef<string | null>(null);
  // Mirror the derived vision-capability so the stable send/edit/regenerate
  // callbacks (which read via refs, not props) can gate on the latest value.
  // Direct render-time assignment (not an effect) to match modelAvailableRef
  // above — an effect would lag by one passive-effect flush.
  const modelVisionCapableRef = useRef(modelVisionCapable);
  modelVisionCapableRef.current = modelVisionCapable;
  // Likewise the attach predicate and the one capability its refusal wording needs
  // (an image GENERATOR is told it takes no input, any other model that it cannot
  // read one).
  const modelImageCapableRef = useRef(modelImageCapable);
  modelImageCapableRef.current = modelImageCapable;
  const attachModeRef = useRef<AttachMode>(attachMode);
  attachModeRef.current = attachMode;
  // Mirror the thread's kind so the stable send/edit/regenerate callbacks and
  // buildDoc (which read via refs, not props) always see the latest value.
  // Direct render-time assignment, like modelAvailableRef/modelVisionCapableRef
  // above — an effect would lag by one passive-effect flush, and this ref used
  // to be written only in activateChat, which made it permanently stale for
  // every thread pinned during the current session.
  const chatKindRef = useRef<string>(chatKind);
  chatKindRef.current = chatKind;
  // The voice the preset-clone pre-select (the voices effect, below) wrote into a blank voice
  // field, or '' when it has written none. A model switch clears the voice only while it still
  // equals this value: a voice the user typed, or a saved thread carried, differs and is never
  // taken for the pre-select's. Written from effects and loads only, never during render, so
  // it needs no re-render of its own.
  const autoSelectedVoiceRef = useRef<string>('');

  // The composer's writer of the audio settings (#281). Every edit is applied at once EXCEPT
  // a retention that would remove audio: tightening the retention of a chat whose stored audio
  // exceeds the new limit is a bulk delete that cannot be undone, so that one change is HELD,
  // not applied -- it is shown (audioSettings below) but not saved or sent -- until the user
  // confirms it (confirmAudioRetention) or drops it (cancelAudioRetention). The other settings
  // of the same edit still apply, and the held value is replaced by the next edit of the
  // retention, so a count can be typed through a first digit that is stricter than the one
  // that ends up typed ("1" on the way to "10") without anything being deleted on the way.
  //
  // "Tighter" is judged on what the wire would carry (keepLastAudioOf: do-not-store is 0, a
  // count is N, keep-all is no limit), never on the raw count text: a blank or unusable count
  // is keep-all and loosens or changes nothing. And a tightening the chat's audio does not
  // exceed deletes nothing, so it needs no question.
  const setAudioSettings = useCallback<Dispatch<SetStateAction<AudioSettings>>>(
    (action) => {
      const committed = audioSettingsRef.current;
      const held = heldRetentionRef.current;
      const shown = held ? { ...committed, ...held.retention } : committed;
      const next = typeof action === 'function' ? action(shown) : action;
      const { keepAudio, keepLast, ...others } = next;
      const asked: AudioRetention = { keepAudio, keepLast };
      let applied: AudioSettings = { ...committed, ...others };
      if (sameRetention(asked, committed)) {
        // Back to what is stored: nothing to hold any more.
        setHeldRetention(null);
      } else {
        const candidate: AudioSettings = { ...committed, ...asked };
        const chatId = activeChatIdRef.current;
        const target = keepLastAudioOf(candidate);
        if (
          chatId &&
          target !== undefined &&
          lowersAudioRetention(committed, candidate) &&
          audioAnswersBeyond(messagesRef.current, target) > 0
        ) {
          setHeldRetention({ chatId, retention: asked, keepLast: target });
        } else {
          applied = { ...applied, ...asked };
          setHeldRetention(null);
        }
      }
      const changed = (Object.keys(applied) as (keyof AudioSettings)[]).some(
        (key) => applied[key] !== committed[key],
      );
      if (changed) {
        audioSettingsRef.current = applied;
        setAudioSettingsState(applied);
      }
    },
    [setHeldRetention],
  );
  // What the composer shows: the committed settings with the held retention over them.
  const shownAudioSettings = useMemo(
    () =>
      heldRetentionState
        ? { ...audioSettingsState, ...heldRetentionState.retention }
        : audioSettingsState,
    [audioSettingsState, heldRetentionState],
  );
  const pendingAudioRetention = useMemo<PendingAudioRetention | null>(
    () =>
      heldRetentionState
        ? {
            keepLast: heldRetentionState.keepLast,
            excess: audioAnswersBeyond(messages, heldRetentionState.keepLast),
          }
        : null,
    [heldRetentionState, messages],
  );

  // The run/SSE engine (FA-2): owns the per-chat run subscriptions + transcript
  // buffers behind a narrow interface. See useChatRuns.ts.
  const runs = useChatRuns(
    apiRef,
    activeChatIdRef,
    messagesRef,
    setMessages,
    onRefreshRef,
    showErrorRef,
    tRef,
  );
  const {
    runningChatIds,
    audioLiveChatIds,
    audioMutedChatIds,
    muteAudio,
    abortAudio,
    isRunning,
    statusOf: runStatusOf,
    runIdIfRunning,
    kindOf,
    elapsedMsOf,
    buffers: chatBuffers,
    registerRunning,
    subscribe: subscribeRun,
    forget: forgetRun,
    closeAll: closeAllRuns,
    refreshTranscript,
    onTerminal,
    onTranscriptStale,
  } = runs;

  // The active chat's streaming flag. A run is bound to a chat, not to "the UI",
  // so streaming reflects only the chat currently on screen; background runs
  // still progress and keep their own entry in runningChatIds.
  const streaming = activeChatId ? runningChatIds.has(activeChatId) : false;

  // The active chat's LIVE RUN's own reported kind and elapsed ms (both
  // undefined with no registry entry at all), recomputed at every render.
  // Deliberately NOT chatKind: chatKind is the client's own "is this thread's
  // first send" guess and can be stale relative to the server's view (a
  // second tab still holding messages = [] after another tab's first send,
  // or a client whose optimistic pin survived a rolled-back send) --
  // PrepareChatRun forces the thread's real pinned kind server-side on every
  // send after the first, and runKind is read from that authoritative,
  // post-force value (the 201 / an active-runs entry / an SSE snapshot),
  // never re-derived from settings. `streaming` above cannot be true for
  // this chat before kindOf/elapsedMsOf are already populated for it --
  // markRunning(true) is called only from inside the same openStream() call
  // that seeds them (useChatRuns.ts) -- so gating a render on `streaming` and
  // reading `runKind` together is safe with no window where they disagree.
  // ChatMessage's own ImagePendingTurn does the per-second ticking locally
  // (an image run emits no incremental events to re-render THIS on), so
  // runElapsedMs need not tick here.
  const runKind = activeChatId ? kindOf(activeChatId) : undefined;
  const runElapsedMs = activeChatId ? elapsedMsOf(activeChatId) : undefined;
  // Whether the active chat's run is playing streamed audio now (see useChatRuns).
  const runAudioLive = activeChatId ? audioLiveChatIds.has(activeChatId) : false;
  const runAudioMuted = activeChatId ? audioMutedChatIds.has(activeChatId) : false;

  // Whether the SERVER owns a chat's transcript now, so that this tab must not save its own copy
  // over it: a live run of ours (the engine's isRunning), or the Voice Chat conversation that is
  // holding the chat, whose turns the gateway commits itself -- a save from here could land
  // between a commit and the reload that catches this tab up with it (and, while the gateway's
  // run is in flight, is refused with a 409).
  const isTranscriptHeld = useCallback(
    (chatId: string) => isRunning(chatId) || voiceHeldChatRef.current === chatId,
    [isRunning],
  );

  // The persistence layer (FA-2): owns buildDoc/flushSave, the debounced-save
  // effect, the pagehide keepalive, and the unmount flush behind a narrow
  // interface. See useChatPersistence.ts.
  const persistence = useChatPersistence(
    {
      activeChatIdRef,
      messagesRef,
      modelRef,
      systemPromptRef,
      temperatureRef,
      maxTokensRef,
      imageSettingsRef,
      audioSettingsRef,
      transcriptionSettingsRef,
      selectedTokenIdRef,
      serverOverrideRef,
      serverOverrideForceUnreachableRef,
      chatKindRef,
      activeTitleRef,
      apiRef,
      showErrorRef,
      tRef,
    },
    {
      activeChatId,
      messages,
      model,
      systemPrompt,
      temperature,
      maxTokens,
      imageSettings,
      audioSettings: audioSettingsState,
      transcriptionSettings,
      selectedTokenId,
      serverOverride,
      serverOverrideForceUnreachable,
      kind: chatKind,
    },
    isTranscriptHeld,
    setChats,
  );
  const {
    flushSave,
    clearDirty,
    skipNextSave,
    cancelPendingSave,
    setTranscriptStale,
    flushOnUnmount,
  } = persistence;

  // Wire persistence's dirty-clearing into the run engine's terminal callback
  // (replaces a direct dirtyRef write formerly inline in finishRun). Cheap ref
  // write on every render, matching this file's existing "read latest via a
  // stable ref, assigned at render time" style (see apiRef/showErrorRef above).
  onTerminal((chatId) => {
    if (chatId === activeChatIdRef.current) clearDirty();
    // Names an untitled transcription chat after its transcript. Defined below, with the
    // renameChat it goes through; this callback only ever RUNS after the render is done.
    titleFromTranscript(chatId);
  });
  // ...and persistence's save suppression into the run engine's canonical
  // refetch. NOT gated on the active chat: the engine adopts background chats
  // too, and activateChat prefers a chat's streamed buffer over the freshly
  // loaded doc, so an unproven background buffer would otherwise become the
  // active transcript and be PUT over the server's good copy.
  onTranscriptStale(setTranscriptStale);

  useEffect(() => {
    messagesRef.current = messages;
    modelRef.current = model;
    systemPromptRef.current = systemPrompt;
    temperatureRef.current = temperature;
    maxTokensRef.current = maxTokens;
    imageSettingsRef.current = imageSettings;
    audioSettingsRef.current = audioSettingsState;
    speechStreamingRef.current = speechStreamingGate.streaming;
    transcriptionSettingsRef.current = transcriptionSettings;
    selectedTokenIdRef.current = selectedTokenId;
    serverOverrideRef.current = serverOverride;
    serverOverrideForceUnreachableRef.current = serverOverrideForceUnreachable;
    chatsRef.current = chats;
    activeChatIdRef.current = activeChatId;
  });

  // Cache per-message callbacks so memoized ChatMessage children keep stable
  // prop references across re-renders.
  const handlerCacheRef = useRef(new Map<string, MessageHandlers>());

  // Load a chat's content into the active state. Calls persistence's
  // skipNextSave so the resulting state changes do not trigger an immediate
  // re-save.
  const activateChat = useCallback(
    (chat: Chat) => {
      // Preserve the outgoing active chat's live transcript in its buffer so
      // returning to it shows the latest (esp. a background run still streaming).
      const outgoing = activeChatIdRef.current;
      if (outgoing) chatBuffers.set(outgoing, messagesRef.current);

      const doc = normalizeDoc(chat.content);
      skipNextSave();
      cancelPendingSave();
      setModel(doc.settings.model);
      setSystemPrompt(doc.settings.system_prompt);
      setTemperature(doc.settings.temperature);
      setMaxTokens(doc.settings.max_tokens);
      setSelectedTokenId(doc.settings.run_as_token_id);
      setServerOverride(doc.settings.server_override);
      setServerOverrideForceUnreachable(doc.settings.server_override_force_unreachable);
      // The thread's pinned kind, like every other setting: seeded from the
      // loaded document. A setting activateChat does NOT seed reverts to its
      // default here and is then written over the stored value by the next
      // debounced save, because the PUT full-replaces the content blob.
      setPinnedChatKind(doc.settings.kind ?? '');
      // The image knobs, seeded like every other setting: a thread activateChat
      // does not seed reverts to defaults and is written over the stored value
      // by the next save (the PUT full-replaces the blob). normalizeDoc kept the
      // persisted `image` only for an image thread, so a text thread seeds
      // defaults harmlessly.
      setImageSettings(imageSettingsFrom(doc.settings.image));
      // The audio knobs, seeded the same way and for the same reason (a speech thread
      // activateChat does not seed reverts to defaults and is written over the stored
      // value by the next save). normalizeDoc kept the persisted `audio` only for a
      // speech thread, so any other thread seeds defaults harmlessly.
      setAudioSettingsState(audioSettingsFrom(doc.settings.audio));
      // The loaded voice is this thread's own, whatever the pre-select wrote for the chat left
      // behind: forget that pick, or a saved voice equal to it would be cleared when this
      // thread's model takes over (the voices effect).
      autoSelectedVoiceRef.current = '';
      // A retention change held for another chat's confirm is not this chat's to apply.
      setHeldRetention(null);
      // The transcription knobs, seeded likewise: normalizeDoc kept the persisted
      // `transcription` only for a transcription thread, so any other thread seeds defaults
      // harmlessly, and one activateChat did not seed would be written over by the next save.
      setTranscriptionSettings(transcriptionSettingsFrom(doc.settings.transcription));
      // Seed the transcript from the per-chat buffer when this chat has an active
      // or recently-finished run (the server owns/owned its tail); otherwise use
      // the freshly-loaded server content. Prefer the buffer ONLY when it actually
      // holds streamed content — never an empty/fabricated buffer over the real
      // server doc. At bootstrap a run's metadata is registered before any
      // snapshot has streamed in, so the buffer is empty/absent and the doc wins.
      const buffered = chatBuffers.get(chat.id);
      const streamed =
        buffered && buffered.length > 0 && runStatusOf(chat.id) !== undefined ? buffered : null;
      let seed = streamed ?? doc.messages;
      // Seeding from `chat.content` is the one moment this client KNOWS its
      // transcript is the server's, so it is where a stale mark left by a
      // failed canonical refetch is lifted. Seeding from the buffer proves
      // nothing and therefore lifts nothing.
      if (!streamed) setTranscriptStale(chat.id, false);
      // Interrupted detection (Task 3.4): a trailing `pending` assistant with no
      // active run was cut off — a gateway restart lost the run from the registry.
      // Keep the partial output but mark it interrupted. A chat WITH an active run
      // is left alone; its subscription's snapshot replaces the tail live.
      if (runStatusOf(chat.id) !== 'running') {
        const last = seed.at(-1);
        if (last?.role === 'assistant' && last.status === 'pending') {
          seed = [...seed.slice(0, -1), { ...last, status: 'interrupted' as const }];
        }
      }
      setMessages(seed);
      messagesRef.current = seed;
      chatBuffers.set(chat.id, seed);
      handlerCacheRef.current.clear();
      // A source seeded by a refine is a reference to a blob of the chat it was
      // refined in, so it does not follow the user to another chat: sent there the run
      // would be refused (images.blob_not_owned). An uploaded image is just bytes and
      // goes where the composer goes, as it always did.
      setImages((prev) => prev.filter((url) => !isChatBlobUrl(url)));
      setActiveChatId(chat.id);
      activeChatIdRef.current = chat.id;
      activeTitleRef.current = chat.title ?? '';
      lsSet(ACTIVE_ID_KEY, chat.id);
    },
    [
      chatBuffers,
      runStatusOf,
      skipNextSave,
      cancelPendingSave,
      setTranscriptStale,
      setHeldRetention,
    ],
  );

  // The current per-chat generation settings sent when starting a run. The model
  // is the RAW picked model; when a run-as token is selected the server applies
  // that token's override map + catch-all (resolveModelOverride) exactly once —
  // pre-mapping here would double-apply (the catch-all would re-fire on an
  // already-mapped model). run_as_token_id carries the token so the server remaps.
  const currentSettings = useCallback(
    () => ({
      model: modelRef.current,
      system_prompt: systemPromptRef.current,
      temperature: temperatureRef.current,
      max_tokens: maxTokensRef.current,
      run_as_token_id: selectedTokenIdRef.current,
      server_override: serverOverrideRef.current,
      server_override_force_unreachable: serverOverrideForceUnreachableRef.current,
      // On the thread's FIRST send this establishes the pin; on every later
      // send the backend ignores it and forces the stored one, so sending the
      // thread's own kind is both correct and a no-op there.
      kind: chatKindRef.current,
      // The image knobs, present only on an image thread (imageSetting keys off
      // the kind). The backend forwards the set fields to the images endpoint.
      ...imageSetting(chatKindRef.current, imageSettingsRef.current),
      // The audio knobs, present only on a speech thread (audioSetting keys off the
      // kind), so a text or image thread's run body carries no audio key. The backend
      // forwards the set fields to /v1/audio/speech.
      ...audioSetting(chatKindRef.current, audioSettingsRef.current),
      // The transcription knobs, present only on a transcription thread
      // (transcriptionSetting keys off the kind), so a text, image or speech thread's run
      // body carries no transcription key. The backend forwards the set fields as form fields
      // of /v1/audio/transcriptions.
      ...transcriptionSetting(chatKindRef.current, transcriptionSettingsRef.current),
    }),
    [],
  );

  // buildDoc/flushSave, the debounced-save effect, and the pagehide keepalive
  // now live in useChatPersistence.ts (FA-2); `persistence` above exposes the
  // narrow interface this provider needs.

  // Flush immediately when a stream finishes (streaming true -> false): the
  // per-token writes were skipped, so this captures the final transcript (and
  // any auto-title) even if the user has navigated away.
  const prevStreamingRef = useRef(streaming);
  useEffect(() => {
    const was = prevStreamingRef.current;
    prevStreamingRef.current = streaming;
    if (was && !streaming) void flushSave();
  }, [streaming, flushSave]);

  // One-time bootstrap: load the chat list, migrate any legacy conversation,
  // and open a chat (there is always exactly one active chat afterwards). All
  // best-effort — a failed/empty fetch must not crash the app.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const list = await apiRef.current.chats();
        if (cancelled) return;
        const data = Array.isArray(list?.data) ? list.data : [];
        // Fetch the still-running server runs (Task 3.4) and REGISTER their
        // metadata into the run engine WITHOUT opening an EventSource yet.
        // Registering first keeps the opened chat's interrupted-detection
        // seeing the active run so it does not falsely mark a live pending
        // tail interrupted. Deferring the EventSource until AFTER the active
        // chat is loaded ensures no run snapshot is ever applied onto a
        // not-yet-loaded chat (which would fabricate a lone-message buffer and
        // then be trusted over the real server doc).
        let activeRuns: ActiveChatRun[] = [];
        try {
          const active = await apiRef.current.activeChatRuns();
          if (cancelled) return;
          activeRuns = active.data;
          for (const run of activeRuns)
            registerRunning(run.chat_id, run.run_id, run.kind, run.elapsed_ms);
        } catch {
          /* best-effort: no active-run replay */
        }
        if (data.length > 0) {
          setChats(byNewest(data));
          const storedId = lsGet(ACTIVE_ID_KEY);
          const target =
            (storedId && data.find((chat) => chat.id === storedId)) || byNewest(data)[0];
          const full = await apiRef.current.chat(target.id);
          if (cancelled) return;
          activateChat(full);
        } else {
          // Empty list: migrate a legacy conversation if present, else start fresh.
          const legacy = readLegacyDoc();
          const created = legacy
            ? await apiRef.current.createChat({
                title: deriveTitleFromMessages(legacy.messages),
                content: legacy,
              })
            : await apiRef.current.createChat({
                title: '',
                content: { settings: DEFAULTS, messages: [] },
              });
          clearLegacyKeys();
          if (cancelled) return;
          setChats([summaryOf(created)]);
          activateChat(created);
        }
        // Now open the EventSources. The active chat's transcript is loaded, so
        // its snapshot updates the already-shown trailing assistant; a background
        // running chat is seeded from its server doc inside subscribeRun before
        // its snapshot is applied.
        for (const run of activeRuns)
          subscribeRun(run.chat_id, run.run_id, run.kind, run.elapsed_ms);
      } catch (err) {
        if (!cancelled) showErrorRef.current(formatPortalError(err, tRef.current));
      } finally {
        if (!cancelled) setChatsLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activateChat]);

  // The provider is mounted app-wide (above the view switch), so navigation no
  // longer unmounts it — in-flight runs keep generating server-side. This effect
  // fires ONLY on a real provider unmount (logout — a client-side state change,
  // NOT a page reload), never on navigation. Close every open EventSource (the
  // server run keeps going and is re-subscribed on next load) and flush a final
  // save for the active chat unless it is mid-run (server owns that transcript).
  useEffect(
    () => () => {
      closeAllRuns();
      flushOnUnmount();
    },
    [closeAllRuns, flushOnUnmount],
  );

  // No auto-selection of a model: an empty selection is a valid state the user can
  // return to (clear the field to search freely). A fresh chat therefore starts
  // with NO model — the user picks one, and Send stays disabled until they do
  // (modelAvailable below). A non-empty saved model also stays selected even when
  // momentarily missing from chatModels (upstream unreachable), shown as
  // unavailable until it returns.

  // While a PICKED model is unavailable, lightly poll the models list so the "!"
  // clears and Send re-enables automatically once it becomes reachable again, with
  // no user action needed. Skipped when nothing is selected (effectiveModel === "")
  // — there is no model to wait for — and stops as soon as it's available.
  useEffect(() => {
    if (modelAvailable || effectiveModel === '') return;
    const id = setInterval(() => {
      refreshModelsRef.current?.();
    }, 15000);
    return () => clearInterval(id);
  }, [modelAvailable, effectiveModel]);

  // Clear a composer attachment the (newly) effective model will not use —
  // e.g. switching from a vision-capable model to one that is not, with an
  // image still attached. Runs whenever the attach predicate changes (it is
  // derived from the thread's kind and the model's flags, see attachModeOf); a
  // no-op while there is nothing attached.
  //
  // An attachment is cleared only when the new mode accepts NONE. Moving between
  // 'vision' and 'edit' (an empty thread's kind follows the picked model, so
  // picking an image model after attaching to a vision one) keeps it: it is the
  // user's operand either way, and the server decides what it is from the thread.
  // What a mode cannot take it refuses where it is used (send(), handleFiles).
  //
  // The image-GENERATOR case is not the same refusal as a plain non-vision
  // model: such a model may well be vision-capable too, and a generator that
  // cannot be edited (an operator's opt-out) takes no input at all -- an image
  // run without a source is a generation, so the attachment would be dropped in
  // silence. It says so in its own words.
  useEffect(() => {
    if (images.length === 0 || attachMode !== 'none') return;
    setImages([]);
    showError(modelImageCapable ? t.chatImageGeneratorNoInput : t.chatImageModelUnsupported);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attachMode]);

  // A mask is the mask OF an edit's source image, so it cannot outlive either: it
  // goes when the source is removed (or sent) and when the attach mode is no longer
  // 'edit' -- a text model reading the same image has no use for one. Nothing needs
  // to be said: the strip it was in shows it gone, and the source's own clearing
  // (above) already speaks when a model takes no image at all.
  useEffect(() => {
    if (mask === null) return;
    if (attachMode !== 'edit' || images.length === 0) setMask(null);
  }, [mask, attachMode, images.length]);

  // A clip belongs to the transcription composer: once the thread is anything else (a different
  // model picked on an empty thread, another chat opened) there is no control that shows or
  // removes it, and it would otherwise sit unseen in memory -- up to 20 MB of it -- ready to
  // reappear in a later transcription composer. So it goes with the composer, without a word:
  // there is nothing on screen left to say it about.
  useEffect(() => {
    if (audio !== null && chatKind !== 'transcription') setAudio(null);
  }, [audio, chatKind]);

  // Reset a stale run-as selection: onRefresh reloads `tokens` after every
  // stream, and a chat may persist a run-as token that was later
  // deleted/disabled/expired or lost gateway:use — it would otherwise keep
  // being sent as X-OP-Run-As-Token (backend 403) and leave the native
  // <select value={...}> uncontrolled. Fall back to the session ("") default.
  useEffect(() => {
    if (selectedTokenId !== '' && !usableTokens.some((tk) => tk.id === selectedTokenId)) {
      setSelectedTokenId('');
    }
  }, [usableTokens, selectedTokenId]);

  // Mirror the run-as reset above for a stale server override: `servers`
  // reloads after every stream (onRefresh), and the picked server may have
  // been deleted or lost from the caller's manageable set since it was
  // selected. The server ALSO self-heals this on every run (PrepareChatRun),
  // but resetting here too keeps the picker's own display honest instead of
  // silently rendering blank (a value absent from its options).
  useEffect(() => {
    if (serverOverride !== '' && !manageableServers.some((s) => s.id === serverOverride)) {
      setServerOverride('');
      setServerOverrideForceUnreachable(false);
    }
  }, [manageableServers, serverOverride]);

  // Load the EFFECTIVE override server's offered models so the chat's model
  // dropdown can narrow to them (Task 6, step 3; keyed on the effective value
  // per Task 2 so a token-locked server also filters the dropdown). Latest-wins
  // guard so a slow/stale response for a previously-selected server can't
  // clobber a newer selection; an empty override clears the list without a
  // fetch.
  useEffect(() => {
    if (effectiveServerOverride === '') {
      setServerOverrideModels([]);
      return;
    }
    let cancelled = false;
    void apiRef.current
      .serverModels(effectiveServerOverride)
      .then((rows) => {
        if (!cancelled) setServerOverrideModels(rows);
      })
      .catch(() => {
        if (!cancelled) setServerOverrideModels([]);
      });
    return () => {
      cancelled = true;
    };
  }, [effectiveServerOverride]);

  // Load the effective IMAGE model's per-model sd-server options so the
  // composer's image dropdowns can narrow to what that model actually supports.
  // Same shape as the serverOverrideModels effect above: latest-wins guard so a
  // slow/stale response for a previously-picked model can't clobber a newer
  // pick, a non-image (or no) model clears without a fetch, and a failure
  // clears too -- null makes the dropdowns fall back to the static superset.
  // Unlike that effect it also clears on every model change (see below).
  // Keyed on the picked model, not the thread's kind: an existing text thread
  // that picks an image model still fetches, which is harmless because the
  // composer only renders these controls for an image thread.
  useEffect(() => {
    // Cleared up front, not only on the non-image branch: these options belong
    // to ONE model, so the previous model's must not stay exposed (and
    // selectable) while the next model's fetch is in flight.
    setModelImageOptions(null);
    if (!modelImageCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelImageOptions(effectiveModel)
      .then((options) => {
        if (!cancelled) setModelImageOptions(options);
      })
      .catch(() => {
        if (!cancelled) setModelImageOptions(null);
      });
    return () => {
      cancelled = true;
    };
  }, [modelImageCapable, effectiveModel]);

  // Load the effective SPEECH model's cached voices so the speech composer's voice
  // picker can offer them. The image-options effect above, for voices: cleared up
  // front (a voice belongs to ONE model, so the previous model's must not stay on
  // offer while the next fetch is in flight -- it would be sent and refused), a
  // latest-wins guard against a slow stale response, and a non-speech (or no) model
  // clears without a fetch. FAIL-OPEN: a failed fetch and an empty answer are the same
  // empty list, which leaves the voice field the free-text field it always was.
  // Keyed on the picked model, not the thread's kind, like the image options: the
  // composer only renders the picker for a speech thread.
  //
  // A preset-voice CLONE model (modelPresetClone) also gets a default voice from this answer:
  // the first voice it lists fills a BLANK voice field, so the first send does not depend on a
  // default preset the operator may not have configured (a plain tts model keeps its blank
  // voice, the server's own default). The language pre-fill's shape, below: done when the
  // answer ARRIVES -- never on the user's own edits, so a voice they cleared stays cleared --
  // and only into a blank field of a thread that has not spoken yet, so a voice the thread
  // already has (typed, or saved) is never overwritten, nor is the voice of a thread whose
  // earlier answers were made without one. It runs in this model's own answer (guarded by
  // `cancelled`), so another model's voices, still held while this fetch is in flight, are
  // never taken for this one's.
  //
  // That pick belongs to the model that offered it, and the no-overwrite rule above would
  // otherwise carry it to the next model, which may not have the voice (a send then fails
  // server-side). So the value the pre-select wrote is remembered (autoSelectedVoiceRef), and
  // every run of this effect -- a model switch -- first clears the voice IF it still equals
  // that value: the next preset-clone model's own answer then fills the blank, and a plain tts
  // model keeps the blank (the server's default). Only the pre-select's own value is ever
  // cleared, so a voice the user typed over it, or a saved thread's, survives the switch (the
  // user's edits need no interception: a differing value is simply not the remembered one),
  // and a voice the user cleared stays cleared.
  useEffect(() => {
    setModelVoices(NO_VOICES);
    const autoSet = autoSelectedVoiceRef.current;
    autoSelectedVoiceRef.current = '';
    if (autoSet !== '') {
      setAudioSettings((s) => (s.voice === autoSet ? { ...s, voice: '' } : s));
    }
    if (!modelSpeechCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelVoices(effectiveModel)
      .then((voices) => {
        if (cancelled) return;
        setModelVoices(voices);
        const first = voices[0];
        if (
          modelPresetClone &&
          first !== undefined &&
          chatKindRef.current === 'speech' &&
          messagesRef.current.length === 0 &&
          audioSettingsRef.current.voice === ''
        ) {
          // Remember what is written, but only when it is: a voice already there is not ours.
          autoSelectedVoiceRef.current = first;
          setAudioSettings((s) => (s.voice === '' ? { ...s, voice: first } : s));
        }
      })
      .catch(() => {
        if (!cancelled) setModelVoices(NO_VOICES);
      });
    return () => {
      cancelled = true;
    };
  }, [modelSpeechCapable, modelPresetClone, effectiveModel, setAudioSettings]);

  // Ask the gateway whether the effective SPEECH model may be streamed (ADR-068), for the
  // composer's toggle. The voices effect above, for the streaming gate: closed up front (the
  // answer belongs to ONE model, and a stale yes must not stay on offer while the next
  // lookup is in flight -- the toggle would promise streaming the gateway will not do), a
  // latest-wins guard against a slow stale response, and no lookup at all for a non-speech
  // (or no) model. FAIL CLOSED: a failed lookup is a no, which leaves the thread on the
  // buffered WAV run it always had. The deps list ends at the model, like the voices
  // effect's: the answer does not depend on the thread.
  useEffect(() => {
    setSpeechStreamingGate(NO_SPEECH_STREAMING);
    if (!modelSpeechCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelSpeechStreaming(effectiveModel)
      .then((gate) => {
        if (!cancelled) setSpeechStreamingGate(gate);
      })
      .catch(() => {
        if (!cancelled) setSpeechStreamingGate(NO_SPEECH_STREAMING);
      });
    return () => {
      cancelled = true;
    };
  }, [modelSpeechCapable, effectiveModel]);

  // Ask the gateway whether the effective SPEECH model takes a language (settings.audio.
  // language), for the composer's language control. The streaming gate's effect above, for the
  // language gate: closed up front (the answer belongs to ONE model, and a stale yes must not
  // stay on offer while the next lookup is in flight -- the field would promise a knob the
  // next model drops), a latest-wins guard against a slow stale response, and no lookup at all
  // for a non-speech (or no) model. FAIL CLOSED: a failed lookup is a no, which leaves the
  // composer without a language field, as before this gate existed. The deps list ends at the
  // model, like the streaming gate's: the answer does not depend on the thread.
  useEffect(() => {
    setSpeechLanguageGate(NO_SPEECH_LANGUAGE);
    if (!modelSpeechCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelSpeechLanguage(effectiveModel)
      .then((gate) => {
        if (!cancelled) setSpeechLanguageGate(gate);
      })
      .catch(() => {
        if (!cancelled) setSpeechLanguageGate(NO_SPEECH_LANGUAGE);
      });
    return () => {
      cancelled = true;
    };
  }, [modelSpeechCapable, effectiveModel]);

  // Pre-fill the language of an EMPTY speech thread with the portal's UI language, when the
  // model takes a language and declares a CONCRETE non-auto default (not "auto", and not an
  // empty "the voice/model decides" default): such a model will not detect the language itself,
  // so audio.cpp's confucius4 (default "zh") speaks Chinese unless told otherwise, and a German
  // text would come out as Chinese. Runs when a gate answer ARRIVES -- never on the
  // user's own edits, so a field they cleared stays cleared -- and only fills a blank field of
  // a thread that has not spoken yet: a language the thread already has (typed, or saved) is
  // never overwritten, and neither is the settings of a thread whose earlier answers were made
  // without one. Another kind of thread (a text thread with a speech model picked) is not a
  // speech thread and gets nothing. Switching to a different speech model re-fires this and
  // still refills only a BLANK field (never a typed or saved one). Declared after the effect that
  // mirrors the refs it reads.
  useEffect(() => {
    if (!speechLanguageGate.accepts) return;
    // Skip a model that decides the language itself: a literal "auto" default, OR no
    // declared default at all. An empty default means "the model/voice decides" (e.g.
    // kokoro defaults to the selected voice prefix) -- auto-like, not a wrong-language
    // default worth correcting, and the model may not even support the UI language
    // (kokoro rejects "de"). Only a CONCRETE non-auto default (confucius4's "zh") is
    // the case this pre-fill exists for.
    const declaredDefault = speechLanguageGate.default.trim().toLowerCase();
    if (declaredDefault === '' || declaredDefault === 'auto') return;
    if (chatKindRef.current !== 'speech' || messagesRef.current.length > 0) return;
    setAudioSettings((s) => (s.language === '' ? { ...s, language: localeRef.current } : s));
  }, [speechLanguageGate, setAudioSettings]);

  // Ask the gateway whether the effective TRANSCRIPTION model may go live (#266), for the
  // composer's live record mode. The speech gate's effect above, for the transcription twin:
  // closed up front (the answer belongs to ONE model, and a stale yes must not stay on offer
  // while the next lookup is in flight -- the mode would promise a session the gateway will
  // refuse a ticket for), a latest-wins guard against a slow stale response, and no lookup at
  // all for a non-transcription (or no) model. FAIL CLOSED: a failed lookup is a no, which
  // leaves the thread on the buffered recording it always had. The deps list ends at the
  // model, like the speech gate's: the answer does not depend on the thread.
  useEffect(() => {
    setTranscriptionStreaming(false);
    if (!modelTranscriptionCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelTranscriptionStreaming(effectiveModel)
      .then((streams) => {
        if (!cancelled) setTranscriptionStreaming(streams);
      })
      .catch(() => {
        if (!cancelled) setTranscriptionStreaming(false);
      });
    return () => {
      cancelled = true;
    };
  }, [modelTranscriptionCapable, effectiveModel]);

  // Ask the gateway which of the catalogue's transcription and speech models STREAM, for Voice
  // Chat (#312): the two gates above, asked of every candidate instead of the picked model, as the
  // conversation's ears and voice are not the thread's to pick (see voiceChatCapable). The first
  // of each, in the catalogue's order (the speech models plain-first, see voiceTtsCandidates),
  // that the gateway says streams is used. Closed up front and asked again when the candidates
  // change, or when the thread becomes (or stops being) one a conversation could run on; a
  // latest-wins guard against a slow stale answer; no lookup at all for a thread that cannot
  // chat. FAIL CLOSED: a lookup that fails is a no. The candidates are read through a ref and the
  // effect keyed on their identity, so that a refreshed models list with the same candidates does
  // not ask again.
  const voiceCandidatesRef = useRef({ asr: voiceAsrCandidates, tts: voiceTtsCandidates });
  voiceCandidatesRef.current = { asr: voiceAsrCandidates, tts: voiceTtsCandidates };
  const voiceCandidatesKey = `${voiceAsrCandidates.join('\n')}\u0000${voiceTtsCandidates.join('\n')}`;
  useEffect(() => {
    setVoiceLegs(null);
    if (!voiceLlmCapable) return;
    const { asr, tts } = voiceCandidatesRef.current;
    let cancelled = false;
    void Promise.all([
      firstThatStreams(asr, (id) => apiRef.current.modelTranscriptionStreaming(id)),
      firstThatStreams(
        tts,
        async (id) => (await apiRef.current.modelSpeechStreaming(id)).streaming,
      ),
    ]).then(([asrModel, ttsModel]) => {
      if (!cancelled) setVoiceLegs({ asr: asrModel, tts: ttsModel });
    });
    return () => {
      cancelled = true;
    };
  }, [voiceCandidatesKey, voiceLlmCapable]);

  // Ready the browser's audio from inside the user's gesture, for a turn that will stream.
  // A page may only make sound once the user has acted on it, and Safari wants the audio
  // context created and resumed INSIDE the handler of the click that sends: by the time the
  // first chunk arrives over the network the gesture is long gone. So send() and the replay
  // entry points call this before their first await -- and only for a speech thread that has
  // streaming on and available, so no other send touches the browser's audio.
  const primeSpeechPlayback = useCallback(() => {
    if (
      chatKindRef.current === 'speech' &&
      audioSettingsRef.current.stream &&
      speechStreamingRef.current
    ) {
      speechPlayback.prime();
    }
  }, []);

  // The run/SSE engine's subscribe/finishRun/adoptCanonicalTranscript/
  // ensureAssistant/writeAssistant/updateChatMessages/markRunning now live in
  // useChatRuns.ts (FA-2); `runs` above exposes the narrow interface this
  // provider needs (subscribeRun, isRunning, chatBuffers, onTerminal, ...).

  // The refusal for a run of this thread whose advanced passthrough cannot be sent, as the
  // sentence to show ('' when it can): a speech or transcription thread whose passthrough text
  // is not a JSON object (the setting would omit it, and the user's extra fields would vanish
  // from the request in silence, with the Advanced section that holds them perhaps collapsed)
  // or names a field the run owns (the run would commit the turn and then fail it
  // server-side). Read by send() and by the replay entry points, which send the thread's
  // CURRENT settings too; only the thread's own kind has a passthrough, so another kind's
  // stale text never blocks anything.
  const passthroughRefusal = useCallback((): string => {
    const kind = chatKindRef.current;
    if (speechPassthroughInvalid(kind, audioSettingsRef.current)) {
      return audioPassthroughMessage(
        parseAudioPassthrough(audioSettingsRef.current.passthrough),
        tRef.current,
      );
    }
    if (transcriptionPassthroughInvalid(kind, transcriptionSettingsRef.current)) {
      return transcriptionPassthroughMessage(
        parseTranscriptionPassthrough(transcriptionSettingsRef.current.passthrough),
        tRef.current,
      );
    }
    return '';
  }, []);

  // Start a run whose message history REPLACES the chat's transcript (edit /
  // regenerate). The optimistic history is shown immediately; the server run
  // then generates a fresh assistant turn appended after it.
  const startRunWithHistory = useCallback(
    async (chatId: string, history: ChatUiMessage[]) => {
      // A live recording or a Voice Chat conversation of this thread is under way: each commits
      // its own turns, and a replay started now would own the transcript they are to land in (a
      // recording's commit would then fail with chatBusy). Refused like send()'s, before any
      // state is touched.
      if (audioSessionRunningRef.current) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      // Block edit/regenerate while the effective model is unavailable — BEFORE
      // any state mutation. Otherwise the setMessages(history) below would
      // truncate the transcript (dropping the assistant turn) and the debounced
      // save would PUT that truncation, permanently losing data, all for a run
      // that is doomed anyway. Mirrors send()'s guard for these entry points.
      if (!modelAvailableRef.current) {
        showErrorRef.current(tRef.current.chatModelUnavailable);
        return;
      }
      // Same guard as send()'s for a speech or transcription thread's advanced
      // passthrough: a replay sends the thread's CURRENT settings too, and one that is not a
      // JSON object would be dropped from the request in silence, and one that names a field
      // the run owns would fail the replay after the turn is committed.
      const refusal = passthroughRefusal();
      if (refusal) {
        showErrorRef.current(refusal);
        return;
      }
      // A transcription replay transcribes the LAST user turn's clip and reads nothing else
      // of the history, so a turn without one has nothing to transcribe -- a stray text turn
      // (an edit of it keeps only its text), or one whose clip an earlier edit dropped. The
      // run would be started after the transcript was truncated and the endpoint would
      // refuse it with nothing to say about why; refused here instead, with the transcript
      // untouched. Judged on the REPLAYED history, like the image guard below: replaying an
      // earlier turn drops every later clip.
      if (chatKindRef.current === 'transcription' && !lastUserHasAudio(history)) {
        showErrorRef.current(tRef.current.chatTranscriptionReplayNoAudio);
        return;
      }
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
      // An IMAGE thread's request carries no history: the executor reads the
      // LAST user message and nothing else, so an older image -- an earlier
      // edit's source, any generated result -- is never sent and has nothing to
      // refuse. Without that exemption an image thread refused its own
      // regenerate from the second turn onward, telling the user that a model
      // whose only purpose is images does not support images. The exemption
      // stops exactly at the last user message, though: when IT carries an image
      // the replayed turn is an EDIT (ADR-043 R1), and an edit needs a model the
      // gateway will serve one for -- the same predicate that offered the attach
      // control. Refused here, before any state is touched, rather than started
      // and answered by the endpoint's own refusal after the transcript was
      // truncated.
      //
      // An edit also needs its prompt, the same rule the composer holds at send():
      // a replay whose last user message has an image but no text (an edit that
      // blanks the prompt, or a turn the old composer stored without one) would be
      // truncated, started, and then refused by the endpoint's images.prompt_required.
      if (chatKindRef.current === 'image') {
        if (lastUserHasImage(history)) {
          if (attachModeRef.current !== 'edit') {
            showErrorRef.current(
              modelImageCapableRef.current
                ? tRef.current.chatImageGeneratorNoInput
                : tRef.current.chatImageModelUnsupported,
            );
            return;
          }
          if (lastUserText(history).trim() === '') {
            showErrorRef.current(tRef.current.chatImageEditPromptRequired);
            return;
          }
        }
      } else if (!modelVisionCapableRef.current && historyHasImage(history)) {
        showErrorRef.current(tRef.current.chatImageModelUnsupported);
        return;
      }
      setMessages(history);
      messagesRef.current = history;
      chatBuffers.set(chatId, history);
      handlerCacheRef.current.clear();
      primeSpeechPlayback();
      try {
        const res = await apiRef.current.startChatRun(chatId, {
          edited_history: history.map((m) => ({ ...m })),
          settings: currentSettings(),
        });
        subscribeRun(chatId, res.run_id, res.kind, res.elapsed_ms);
      } catch (err) {
        showErrorRef.current(formatPortalError(err, tRef.current));
      }
    },
    [subscribeRun, currentSettings, chatBuffers, passthroughRefusal, primeSpeechPlayback],
  );

  // Set the active chat's title from the first user message, when it is still
  // untitled. Updates the sidebar immediately; persisted on the next save.
  const maybeAutoTitle = useCallback((text: string) => {
    const id = activeChatIdRef.current;
    if (!id) return;
    const hasUserMessage = messagesRef.current.some((message) => message.role === 'user');
    if (hasUserMessage) return;
    const current = activeTitleRef.current.trim();
    if (current !== '' && current !== tRef.current.chatNewChat) return;
    const title = deriveTitle(text);
    if (!title) return;
    activeTitleRef.current = title;
    setChats((prev) => prev.map((chat) => (chat.id === id ? { ...chat, title } : chat)));
  }, []);

  const editUserMessage = useCallback(
    (id: string, text: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      if (isRunning(chatId) || audioSessionRunningRef.current) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      const current = messagesRef.current;
      const index = current.findIndex((message) => message.id === id);
      if (index < 0) return;
      const original = current[index];
      let content: ChatContent = text;
      if (Array.isArray(original.content)) {
        // Every image part of the turn goes back as it was -- an image thread's edit
        // turn keeps its source, and a masked edit its tagged mask part too (it is an
        // image_url part like the source), in their original order. So does a
        // transcription turn's clip (its audio_url part, which only a USER turn of that kind
        // carries): it is the one thing the run reads, so an edit that dropped it would leave
        // the replayed turn with nothing to transcribe.
        const attachments = original.content.filter(
          (part) => part.type === 'image_url' || part.type === 'audio_url',
        );
        content = [{ type: 'text', text }, ...attachments];
      }
      const edited: ChatUiMessage = {
        ...original,
        content,
        reasoning: undefined,
        reasoningMs: undefined,
        ttftMs: undefined,
        tps: undefined,
      };
      void startRunWithHistory(chatId, [...current.slice(0, index), edited]);
    },
    [startRunWithHistory, isRunning],
  );

  const regenerate = useCallback(
    (id: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      if (isRunning(chatId) || audioSessionRunningRef.current) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      const current = messagesRef.current;
      const index = current.findIndex((message) => message.id === id);
      if (index < 0) return;
      const history = current.slice(0, index); // drop this assistant turn + anything after
      if (history.length === 0) return;
      void startRunWithHistory(chatId, history);
    },
    [startRunWithHistory, isRunning],
  );

  // Refine a generated image in place: seed the composer with it as the source of the
  // next edit, BY REFERENCE. The url is the image part's own /api/portal/chat-blobs/{id}
  // (an old chat's inline data url is the one other shape it can have), so the source
  // part of the turn the user sends is that same string -- nothing is fetched, copied
  // or re-embedded, and the transcript never grows by the image's bytes. The run's
  // commit vets the reference like any other (it must be a blob of THIS chat), and the
  // executor resolves it to bytes as it does a stored source (PrepareChatRun).
  //
  // It only SEEDS: an edit is the prompt applied to the image, so the user types the
  // instruction and sends, and send() holds the prompt rule as it does for an upload.
  // Nothing is started or stored here, and no mask is attached for them (the mask slot
  // is theirs, as it is for any source).
  //
  // Same gates as the attach control and the replay guard, from the same predicate: an
  // edit-capable model on an image thread (attachMode 'edit'), and no live run in this
  // chat. The view offers the control only where both hold; this refuses the rest
  // (a stale click, a model switched between render and click) in the same words.
  // Scope is the generated result: an assistant turn's own image, by id and url, so a
  // user turn's image or a url the turn does not hold is a no-op, not a source.
  const refineImage = useCallback(
    (id: string, url: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      const message = messagesRef.current.find((m) => m.id === id);
      if (message?.role !== 'assistant' || !imageUrlsOf(message.content).includes(url)) return;
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      if (attachModeRef.current !== 'edit') {
        showErrorRef.current(
          modelImageCapableRef.current
            ? tRef.current.chatImageGeneratorNoInput
            : tRef.current.chatImageModelUnsupported,
        );
        return;
      }
      // An edit has ONE source, so the refined image takes the place of whatever was
      // attached -- and of the mask drawn for that: it would not fit this image.
      setImages([url]);
      setMask(null);
      setComposerFocusRequest((n) => n + 1);
    },
    [isRunning],
  );

  // Bring a chat's LOCAL transcript in line with what the gateway just did to its stored one
  // (an audio delete or prune). The gateway rewrote the stored document and deleted the audio's
  // blobs; the local copy still holds the audio_url parts, and a PUT replaces the whole stored
  // document -- so a later save of that stale copy (any settings edit would make one) would put
  // back parts pointing at deleted blobs, which serve a 404. Rewriting the copy, and the buffer
  // a reopened chat is seeded from, is what makes every later save carry the not-stored state:
  // the gateway does not serialize its audio rewrites against a PUT, so the client must not be
  // the one that writes the old document back. The rewrite changes `messages`, which also
  // schedules a save of its own that re-asserts the marker.
  const rewriteTranscript = useCallback(
    (chatId: string, rewrite: (messages: ChatUiMessage[]) => ChatUiMessage[]) => {
      if (chatId === activeChatIdRef.current) {
        const next = rewrite(messagesRef.current);
        if (next === messagesRef.current) return;
        messagesRef.current = next;
        setMessages(next);
        chatBuffers.set(chatId, next);
        return;
      }
      // Not the open chat (the user moved on while the request was in flight): its buffer
      // is what a reopened chat with a lingering run is seeded from.
      const buffered = chatBuffers.get(chatId);
      if (buffered) chatBuffers.set(chatId, rewrite(buffered));
    },
    [chatBuffers],
  );

  // Delete the stored audio of ONE spoken answer: ask the gateway, and only once it has done it
  // show the answer as not stored (the not-stored bubble, which offers a regenerate). Gated like
  // the other per-message actions -- not while the chat has a live run, which owns the
  // transcript (the gateway refuses it with a 409 too) -- and only for an assistant answer that
  // holds audio: a user turn's audio is a transcription clip the user supplied, and a text or
  // already-removed answer has none, so none of them is a request. The control that calls this
  // asks the user first (a per-answer inline confirm); this does not.
  const deleteAudio = useCallback(
    (id: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      const message = messagesRef.current.find((m) => m.id === id);
      if (!message || !holdsStoredAudio(message)) return;
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      void (async () => {
        try {
          await apiRef.current.deleteMessageAudio(chatId, id);
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
          return;
        }
        rewriteTranscript(chatId, (current) => {
          const next = current.map((m) => (m.id === id ? withAudioRemoved(m) : m));
          // The same array when the answer is no longer there (or holds no audio any more):
          // there is nothing to rewrite, so nothing to save.
          return next.some((m, i) => m !== current[i]) ? next : current;
        });
      })();
    },
    [isRunning, rewriteTranscript],
  );

  // Confirm the held stricter retention (#281): prune the chat's stored audio to its count --
  // the gateway decides which answers go from its own stored document; the request is only the
  // number -- and, once that has happened, commit the new setting (the autosave persists it) and
  // show the pruned answers as not stored. In that order on purpose: a refused or failed prune
  // leaves everything as it was, the held value still pending (so it can be confirmed again or
  // dropped), and the setting is never saved for a prune that did not happen. Refused while the
  // chat has a live run, as the gateway refuses it too.
  const confirmAudioRetention = useCallback(() => {
    const held = heldRetentionRef.current;
    if (!held) return;
    const chatId = held.chatId;
    if (chatId !== activeChatIdRef.current) {
      setHeldRetention(null);
      return;
    }
    if (isRunning(chatId)) {
      showErrorRef.current(tRef.current.chatBusy);
      return;
    }
    void (async () => {
      try {
        await apiRef.current.pruneChatAudio(chatId, held.keepLast);
      } catch (err) {
        showErrorRef.current(formatPortalError(err, tRef.current));
        return;
      }
      rewriteTranscript(chatId, (current) => pruneAudioBeyond(current, held.keepLast));
      // The retention is the chat's now -- unless the user left the chat meanwhile, in which
      // case the held value was already dropped with it and is not another chat's to take. (A
      // retention edit made while the prune was in flight is replaced by the confirmed value.)
      if (chatId !== activeChatIdRef.current) return;
      const applied = { ...audioSettingsRef.current, ...held.retention };
      audioSettingsRef.current = applied;
      setAudioSettingsState(applied);
      setHeldRetention(null);
    })();
  }, [isRunning, rewriteTranscript, setHeldRetention]);

  // Drop the held stricter retention: the control goes back to the stored one, and nothing is
  // saved or deleted.
  const cancelAudioRetention = useCallback(() => setHeldRetention(null), [setHeldRetention]);

  const handlersFor = useCallback(
    (id: string) => {
      const cache = handlerCacheRef.current;
      let handlers = cache.get(id);
      if (!handlers) {
        handlers = {
          onEdit: (text: string) => editUserMessage(id, text),
          onRegenerate: () => regenerate(id),
          onRefine: (url: string) => refineImage(id, url),
          onDeleteAudio: () => deleteAudio(id),
        };
        cache.set(id, handlers);
      }
      return handlers;
    },
    [editUserMessage, regenerate, refineImage, deleteAudio],
  );

  // The live recording of a transcription thread (useLiveTranscription.ts), for the composer's
  // live mode. It starts only where the composer offers it -- a transcription thread whose model
  // the gateway says streams (transcriptionStreaming) -- and belongs to the thread it started in.
  //
  // What the thread says to Start (the model it records for, the language its settings name; the
  // refusals, in the same words as send()'s for the same two reasons) and how a finished
  // recording lands in it are decided HERE, because they read the thread's own state: the
  // recording itself never touches the chat. A finished recording becomes ONE completed turn,
  // appended directly -- NOT started as a chat run, because the gateway's live endpoint has
  // already done, and billed, the work: a marker for the recording as the user's turn (there is
  // no clip to show, and the transcript is all that is kept), and the transcript as the answer,
  // in the shape the run's own commit gives a transcription answer, so it renders, titles the
  // chat and is saved like one. The first turn pins the thread's kind, as the first send does. It
  // is refused, with the sentence of why, while a run is in flight (the server owns the
  // transcript then; the recording keeps its text and may be committed again), and for a
  // thread that is no longer the one it was recorded for.
  const live = useLiveTranscription({
    apiRef,
    tRef,
    showErrorRef,
    thread: {
      chatId: activeChatId,
      model: effectiveModel,
      available: transcriptionStreaming && chatKind === 'transcription',
    },
    begin: (): LiveStart | null => {
      const chatId = activeChatIdRef.current;
      if (!chatId || chatKindRef.current !== 'transcription' || !transcriptionStreaming) {
        return null;
      }
      if (!modelAvailable) {
        showErrorRef.current(tRef.current.chatModelUnavailable);
        return null;
      }
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return null;
      }
      return { chatId, model: effectiveModel, language: transcriptionSettings.language.trim() };
    },
    commit: (chatId, transcript) => {
      if (chatId !== activeChatIdRef.current || chatKindRef.current !== 'transcription') {
        return tRef.current.chatTranscriptionLiveError;
      }
      if (isRunning(chatId)) return tRef.current.chatBusy;
      if (messagesRef.current.length === 0) setPinnedChatKind(chatKindRef.current);
      maybeAutoTitle(transcript);
      const next: ChatUiMessage[] = [
        ...messagesRef.current,
        { id: nextId(), role: 'user', content: tRef.current.chatTranscriptionLiveTurn },
        { id: nextId(), role: 'assistant', content: transcript, status: 'complete' },
      ];
      setMessages(next);
      messagesRef.current = next;
      chatBuffers.set(chatId, next);
      return null;
    },
  });
  const { liveSession } = live;

  // The Voice Chat conversation of the thread (useVoiceChat.ts), for the Voice Chat control. Like
  // the live recording above, what the thread says to Start is decided HERE, because it reads the
  // thread's own state; the conversation itself never touches the chat. Its turns are not
  // appended from here at all: the gateway commits each (the user's words and the answer, text and
  // stored audio, together), and the thread is reloaded from it (refreshTranscript) when the turn
  // is over, which is how the stored turns reach the transcript, through the same rows as any
  // other turn.
  const voiceChat = useVoiceChat({
    apiRef,
    activeChatId,
    begin: (): VoiceChatStart | null => {
      // THIS render's chat, which is the activeChatId the conversation is handed -- not the ref's,
      // which activateChat moves a render ahead of it: a conversation for a chat other than the
      // open one would run, with the microphone on, until the next change of chat (it is ended
      // only when activeChatId CHANGES).
      const chatId = activeChatId;
      if (!chatId || voiceLegs === null || voiceChatGap !== null) return null;
      if (isRunning(chatId) || audioSessionRunningRef.current) {
        showErrorRef.current(tRef.current.chatBusy);
        return null;
      }
      voiceChatIdRef.current = chatId;
      return { chatId, asrModel: voiceLegs.asr, llmModel: effectiveModel, ttsModel: voiceLegs.tts };
    },
    refreshThread: refreshTranscript,
  });
  const { state: voiceState, stop: stopVoiceChat } = voiceChat;
  audioSessionRunningRef.current = liveSessionRunning(liveSession.state) || voiceState !== 'off';
  voiceHeldChatRef.current = voiceState === 'off' ? null : voiceChatIdRef.current;

  // A conversation belongs to the chat and the model it started with (its ticket holds the three
  // models in memory together): when either changes it is no longer the conversation it was
  // started as, so it ends, like a live recording does -- and what it left behind goes with it. An
  // error is one chat's, and the gateway's refusal of one model's ticket must not close the switch
  // of the next chat or model. (The chat's end is the conversation's own too; the error is not.)
  useEffect(() => {
    stopVoiceChat();
  }, [activeChatId, effectiveModel, stopVoiceChat]);

  // A conversation that ends in the middle of a turn may have ended after the gateway committed
  // that turn and before this tab was told (`audio_done`): the connection dropped, or the user
  // switched Voice Chat off, in between. The turn is then on the server and not on the screen,
  // and a later save of this tab's transcript would PUT over it. So an end that follows a turn in
  // flight reloads the thread once more (it changes nothing the thread shows when nothing was
  // committed); and any end saves what was held back while the conversation held the chat.
  const voiceStateRef = useRef<VoiceChatState>('off');
  useEffect(() => {
    const previous = voiceStateRef.current;
    voiceStateRef.current = voiceState;
    if (voiceState !== 'off' || previous === 'off') return;
    const chatId = voiceChatIdRef.current;
    if (chatId && (previous === 'thinking' || previous === 'speaking' || previous === 'draining')) {
      void refreshTranscript(chatId);
    }
    // After the reload above has marked the chat stale, which makes this a no-op: the reload's
    // own adopt is what saves, once it has landed.
    void flushSave();
  }, [voiceState, refreshTranscript, flushSave]);

  // Send the composed message: append the optimistic user bubble, clear the
  // composer, then start a SERVER run and subscribe to it. One run per chat —
  // refuse if this chat already has a live run. On a hard start failure the
  // optimistic user bubble is rolled back.
  async function send() {
    const chatId = activeChatIdRef.current;
    if (!chatId) return;
    if (!modelAvailable) {
      showErrorRef.current(tRef.current.chatModelUnavailable);
      return;
    }
    if (isRunning(chatId)) {
      showErrorRef.current(tRef.current.chatBusy);
      return;
    }
    // A live recording or a Voice Chat conversation of this thread is under way (its Send is
    // closed too): each commits its own turns, and a run started now would own the transcript
    // they are to land in.
    if (liveSessionRunning(liveSession.state) || voiceChat.state !== 'off') {
      showErrorRef.current(tRef.current.chatBusy);
      return;
    }
    // A transcription turn is its clip and nothing else: no text stands in for it (the composer
    // has no text field, and whatever was typed before the model was picked is not part of it),
    // and no image reaches it (the thread's attach mode is 'none', which also clears one). So
    // it needs the clip, and ignores the text and the images.
    const transcribing = chatKindRef.current === 'transcription';
    const clip = transcribing ? audio : null;
    if (transcribing && clip === null) {
      showErrorRef.current(tRef.current.chatTranscriptionNeedsAudio);
      return;
    }
    const text = transcribing ? '' : input.trim();
    if (!clip && !text && images.length === 0) return;
    if (!transcribing && images.length > 0) {
      const mode = attachModeRef.current;
      if (mode === 'none') {
        showErrorRef.current(
          modelImageCapableRef.current
            ? tRef.current.chatImageGeneratorNoInput
            : tRef.current.chatImageModelUnsupported,
        );
        return;
      }
      // An edit has one source image. (The picker takes one in this mode; this is
      // the carry-over from a vision model that had several attached. Nothing is
      // dropped: the user removes the extras and sends.)
      if (mode === 'edit' && images.length > 1) {
        showErrorRef.current(tRef.current.chatImageEditSingleSource);
        return;
      }
      // An edit is the prompt applied to the image: without the prompt the endpoint
      // answers images.prompt_required, but only after the turn was stored. Refused
      // here instead, with the composer left as it is (the image stays attached).
      if (mode === 'edit' && !text) {
        showErrorRef.current(tRef.current.chatImageEditPromptRequired);
        return;
      }
    }
    // A speech or transcription thread's advanced passthrough must be a JSON object (or
    // blank) that names none of the fields the run owns: any other text would be dropped
    // from the run in silence (audioSetting / transcriptionSetting omit it) while the
    // Advanced section that holds it may be collapsed, and a reserved field would fail the
    // run only after the turn is committed. Refused here with the composer left as it is,
    // before anything is stored or cleared.
    const refusal = passthroughRefusal();
    if (refusal) {
      showErrorRef.current(refusal);
      return;
    }
    // This send pins the thread's kind server-side when the thread is still
    // empty (PrepareChatRun accepts the client's kind only on a genuine first
    // send). Record the same pin here, in the same batch as the optimistic
    // user bubble: from the next render on, `messages` is non-empty and the
    // thread's kind comes from the pin rather than from the picked model.
    if (messagesRef.current.length === 0) setPinnedChatKind(chatKindRef.current);

    // The text, the attached source and -- for an edit -- the mask, tagged as one; or, on a
    // transcription thread, the clip alone.
    const content = clip
      ? audioTurnContent(clip.url)
      : userTurnContent(text, images, mask, attachModeRef.current);
    maybeAutoTitle(text);
    const userMessage: ChatUiMessage = { id: nextId(), role: 'user', content };
    const next = [...messagesRef.current, userMessage];
    setMessages(next);
    messagesRef.current = next;
    chatBuffers.set(chatId, next);
    setInput('');
    setImages([]);
    setMask(null);
    setAudio(null);
    // A clip still being converted was meant to replace the one just sent: it must not turn up
    // in the composer afterwards, as if it had been attached to the next turn.
    abandonAudioAttach();

    primeSpeechPlayback();
    try {
      const res = await apiRef.current.startChatRun(chatId, {
        user_message: content,
        settings: currentSettings(),
      });
      subscribeRun(chatId, res.run_id, res.kind, res.elapsed_ms);
    } catch (err) {
      showErrorRef.current(formatPortalError(err, tRef.current));
      const rolledBack = messagesRef.current.filter((m) => m.id !== userMessage.id);
      setMessages(rolledBack);
      messagesRef.current = rolledBack;
      chatBuffers.set(chatId, rolledBack);
    }
  }

  // Stop the active chat's run by asking the server to cancel it; the UI updates
  // when the run's `done` (status canceled) event arrives over the stream.
  const stop = useCallback(() => {
    const chatId = activeChatIdRef.current;
    if (!chatId) return;
    const runId = runIdIfRunning(chatId);
    if (runId) void apiRef.current.cancelChatRun(chatId, runId);
  }, [runIdIfRunning]);

  // New chat: flushes the current chat, then creates + activates a fresh empty
  // server chat. Any in-flight run keeps generating in the background (its
  // EventSource stays open). Best-effort — a failed POST still clears locally.
  const newChat = useCallback(() => {
    void (async () => {
      await flushSave();
      handlerCacheRef.current.clear();
      try {
        const created = await apiRef.current.createChat({
          title: '',
          content: { settings: DEFAULTS, messages: [] },
        });
        setChats((prev) =>
          byNewest([summaryOf(created), ...prev.filter((chat) => chat.id !== created.id)]),
        );
        activateChat(created);
      } catch (err) {
        showErrorRef.current(formatPortalError(err, tRef.current));
        skipNextSave();
        setMessages([]);
        messagesRef.current = [];
      }
    })();
  }, [flushSave, activateChat, skipNextSave]);

  // Switch chats: always allowed now (background runs keep progressing via their
  // own EventSource + buffer). Flushes the current chat, then loads the target's
  // content — or seeds from its buffer when it has an active/recent run.
  const selectChat = useCallback(
    (id: string) => {
      if (id === activeChatIdRef.current) return;
      const req = ++loadReqRef.current;
      void (async () => {
        await flushSave();
        try {
          const full = await apiRef.current.chat(id);
          // A later click superseded this load — drop the stale result so the
          // last-clicked chat always wins, regardless of fetch order.
          if (req !== loadReqRef.current) return;
          activateChat(full);
        } catch (err) {
          if (req !== loadReqRef.current) return;
          showErrorRef.current(formatPortalError(err, tRef.current));
        }
      })();
    },
    [flushSave, activateChat],
  );

  const deleteChat = useCallback(
    (id: string) => {
      void (async () => {
        // Close any open run subscription for the deleted chat and drop its
        // per-chat run/buffer bookkeeping (the backend DELETE also cancels the
        // server-side run).
        forgetRun(id);
        // Deleting the active chat: cancel any pending debounced save first so it
        // can't fire a PUT against the row we're about to remove (which would 404
        // and pop a spurious "chat not found" toast just after a successful delete).
        if (id === activeChatIdRef.current) cancelPendingSave();
        try {
          await apiRef.current.deleteChat(id);
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
          return;
        }
        const remaining = chatsRef.current.filter((chat) => chat.id !== id);
        setChats(remaining);
        chatsRef.current = remaining;
        if (id !== activeChatIdRef.current) return;
        // Deleted the active chat: open the next newest, or start fresh.
        try {
          if (remaining.length > 0) {
            const full = await apiRef.current.chat(byNewest(remaining)[0].id);
            activateChat(full);
          } else {
            const created = await apiRef.current.createChat({
              title: '',
              content: { settings: DEFAULTS, messages: [] },
            });
            setChats([summaryOf(created)]);
            activateChat(created);
          }
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
        }
      })();
    },
    [activateChat, forgetRun, cancelPendingSave],
  );

  const renameChat = useCallback((id: string, title: string) => {
    const trimmed = title.trim();
    if (!trimmed) return;
    // Captured BEFORE the optimistic write, so the catch below can put both
    // back. `previousTitle` is undefined only for an id that is not in the
    // list at all, in which case the optimistic map below is a no-op too and
    // there is nothing to roll back.
    //
    // `activeIdAtWrite` is captured too, and it is the load-bearing one: it
    // is what lets the catch tell "the ref still belongs to the chat I wrote
    // it for" from "the user has since moved on", which the ids alone at
    // catch time cannot express. See the catch.
    const previousTitle = chatsRef.current.find((chat) => chat.id === id)?.title;
    const activeIdAtWrite = activeChatIdRef.current;
    const wroteActiveTitle = id === activeIdAtWrite;
    const previousActiveTitle = activeTitleRef.current;
    setChats((prev) => prev.map((chat) => (chat.id === id ? { ...chat, title: trimmed } : chat)));
    if (wroteActiveTitle) activeTitleRef.current = trimmed;
    void (async () => {
      try {
        // The backend's PUT keeps the stored content when the body carries
        // no `content` key, so a rename writes only the title and never
        // depends on -- or risks overwriting with -- the local transcript.
        const saved = await apiRef.current.saveChat(id, { title: trimmed });
        setChats((prev) =>
          byNewest(
            prev.map((chat) =>
              chat.id === id ? { ...chat, title: saved.title, updated_at: saved.updated_at } : chat,
            ),
          ),
        );
      } catch (err) {
        // Roll the optimistic title back. Without this the sidebar keeps
        // showing a name the server rejected until the next reload, when it
        // silently reverts -- a toast saying "that failed" next to a title
        // that looks like it succeeded. The one writer of the four with no
        // client-side run gate, so the 409 that PUT /chats/{id} answers
        // while a run is active reaches it by design.
        //
        // On EVERY failure code, not just 409: a rename refused for any
        // reason (404 on a chat deleted in another tab, a 5xx, ...) leaves
        // the same lie on screen, and "which codes revert" is a distinction
        // nothing downstream could act on.
        //
        // THE REF IS NOT THIS CHAT'S TITLE. `activeTitleRef` is the title
        // the debounced autosave PUTs for whatever chat is active RIGHT
        // NOW, and "right now" is after an await that the user spent doing
        // whatever they liked -- including switching chats. An
        // unconditional restore here therefore writes one chat's old title
        // into another chat's ref, and the next autosave persists the
        // rename the server just refused onto a chat nobody renamed, with
        // no toast about it: rename Alpha, click over to Beta, watch the
        // 409 this rollback exists for, and Beta is saved as "Alpha".
        //
        // So the restore undoes the write above only when the ref still
        // belongs to the chat that write was for: this call wrote it
        // (`wroteActiveTitle`) AND the active chat has not changed since
        // (`activeChatIdRef.current === activeIdAtWrite`). Both conditions
        // are needed, and each rules out a different hazard:
        //
        //   - not written, still same active chat -- a rename of a
        //     NON-active chat. The ref is some other chat's title and was
        //     never ours to touch.
        //   - written, active chat changed -- the Alpha/Beta case above.
        //     `activateChat` already set the ref to the NEW chat's own
        //     title (it writes activeChatIdRef and activeTitleRef in the
        //     same breath), so there is nothing to repair and everything to
        //     break.
        //   - not written, active chat changed TO the renamed one -- the
        //     mirror image: `previousActiveTitle` is the title of a chat
        //     that is no longer active, and `activateChat` has again
        //     already set the ref correctly.
        //
        // The list row (above) has no such problem: it is keyed by id, so
        // it is always safe to put back.
        if (previousTitle !== undefined) {
          setChats((prev) =>
            prev.map((chat) => (chat.id === id ? { ...chat, title: previousTitle } : chat)),
          );
        }
        if (wroteActiveTitle && activeChatIdRef.current === activeIdAtWrite) {
          activeTitleRef.current = previousActiveTitle;
        }
        showErrorRef.current(formatPortalError(err, tRef.current));
      }
    })();
  }, []);

  // Name an untitled TRANSCRIPTION chat after its transcript when a run of it completes (Fork 4
  // of the transcription design). A transcription thread's first user turn is audio only, so
  // maybeAutoTitle (which names a chat from the first message's TEXT, at send) has nothing to
  // use and the chat would stay untitled; the transcript is the natural name, and it exists
  // only now. Wired into the run engine's terminal callback (above), so it fires once per run
  // that reached a terminal state, for ANY chat -- the run's own reported kind and status
  // decide, never the active chat's.
  //
  // Gated on the run's own kind ("transcription": the post-force value the executor ran, not
  // the picker) so a text or speech chat is named as before, on a COMPLETED run (a failed or
  // canceled one has no transcript worth a name), and on the chat having no title yet, the
  // new-chat placeholder counting as none exactly as maybeAutoTitle counts it. An empty
  // transcript names nothing: the chat keeps the sidebar's localized "untitled" placeholder.
  //
  // It goes through renameChat, so the title shows at once and is PERSISTED by a title-only
  // save that never touches the transcript -- which is what keeps it for a chat that is not
  // the active one (the debounced autosave only ever saves the active chat), and for one whose
  // canonical refetch failed (its saves are blocked, see setTranscriptStale).
  const titleFromTranscript = useCallback(
    (chatId: string) => {
      if (kindOf(chatId) !== 'transcription' || runStatusOf(chatId) !== 'completed') return;
      const current = (
        chatId === activeChatIdRef.current
          ? activeTitleRef.current
          : (chatsRef.current.find((chat) => chat.id === chatId)?.title ?? '')
      ).trim();
      if (current !== '' && current !== tRef.current.chatNewChat) return;
      const title = deriveTitleFromTranscript(chatBuffers.get(chatId) ?? []);
      if (title) renameChat(chatId, title);
    },
    [kindOf, runStatusOf, chatBuffers, renameChat],
  );

  const removeImage = useCallback(
    (index: number) => setImages((prev) => prev.filter((_, position) => position !== index)),
    [],
  );

  // Vet and attach one audio clip (a picked file, or a recording with no name) as the
  // transcription turn's clip, replacing the one it had; a refused clip leaves that one as it
  // was and says why. The clip is converted to WAV first (prepareAudioClip), which takes a moment
  // for a long one: audioConverting says so meanwhile. It lands only while the thread is still a
  // transcription one: the picker may have been left behind by a model switch while the file was
  // being converted.
  //
  // The conversions may overlap and finish in any order (a long clip, then a short one), so each
  // attach takes the next number and only the NEWEST one counts: a conversion that finishes
  // after another attach started, or after the clip was removed or sent (abandonAudioAttach), is
  // superseded -- its clip is not set, and neither is its refusal said, nor does it end the
  // converting state that now belongs to the attach that superseded it.
  async function attachAudioClip(file: Blob, name: string) {
    const seq = ++audioAttachSeqRef.current;
    setAudioConverting(true);
    try {
      const clip = await prepareAudioClip(file, name);
      if (seq === audioAttachSeqRef.current && chatKindRef.current === 'transcription') {
        setAudio(clip);
      }
    } catch (err) {
      if (seq === audioAttachSeqRef.current) showError(audioAttachErrorText(err, t));
    } finally {
      if (seq === audioAttachSeqRef.current) setAudioConverting(false);
    }
  }

  const attachRecording = (blob: Blob) => attachAudioClip(blob, '');
  const removeAudio = useCallback(() => {
    setAudio(null);
    abandonAudioAttach();
  }, [abandonAudioAttach]);

  async function handleFiles(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return;
    // On a transcription thread the attachment is the audio clip -- the one thing the turn is --
    // not an image: the attach control there offers audio only, and what it picks is the first
    // file (a turn has one clip).
    if (chatKind === 'transcription') {
      await attachAudioClip(fileList[0], fileList[0].name);
      return;
    }
    // An image edit has ONE source image; every other attachment is capped at
    // MAX_IMAGES. The cap follows the attach mode like everything else about the
    // attachment does.
    const limit = attachMode === 'edit' ? 1 : MAX_IMAGES;
    // Local running counter: the closed-over `images.length` is stale across the
    // awaits below, so a multi-file select would never trip the cap mid-batch.
    let count = images.length;
    let lastError = '';
    for (const file of Array.from(fileList)) {
      if (count >= limit) {
        lastError = limit === 1 ? t.chatImageEditSingleSource : t.chatImageErrorCount;
        break;
      }
      // The edit route of sd-server reads png and jpeg only. The picker offers just
      // those (and HEIC/HEIF, which prepareImageDataUrl converts to JPEG, so what is
      // attached -- and sent on -- is a JPEG), but `accept` is a hint a file dialog's
      // "all files" (or a drop) does not honor, so the type is checked here too.
      if (
        attachMode === 'edit' &&
        !IMAGE_EDIT_SOURCE_TYPES.includes(file.type.toLowerCase()) &&
        !isHeicFile(file)
      ) {
        lastError = t.chatImageEditSourceType;
        continue;
      }
      try {
        const url = await prepareImageDataUrl(file);
        setImages((prev) => (prev.length >= limit ? prev : [...prev, url]));
        count += 1;
      } catch (err) {
        lastError = attachErrorText(err, t);
      }
    }
    // Set one trailing error so a multi-file batch shows a single toast rather
    // than flickering per file (and no stale error survives a later success).
    if (lastError) showError(lastError);
  }

  const removeMask = useCallback(() => setMask(null), []);

  // The mask slot of an edit: ONE PNG, and only beside a source image. It is sized to
  // the source as it is actually sent (prepareMaskDataUrl: size cap, decode check, the
  // source's own pixel size), NOT clamped to the 1568 px longest edge an uploaded
  // source is downscaled to. The two sources differ: an upload is downscaled to that
  // cap, but a refined image is sent by reference at its stored, natural size
  // (1792x1024 is a size the composer offers), and the gateway refuses a mask that is
  // not exactly the first source's size -- a clamped mask would never fit a refined
  // image, and the user has no way to make one that does. A PNG stays a PNG, alpha
  // included. The gateway checks the rest (a transparent region to edit, and the size
  // of a mask that was not of the source's shape) and says so with images.mask_invalid.
  async function handleMaskFile(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return;
    if (attachMode !== 'edit' || images.length === 0) {
      showError(t.chatMaskNeedsSource);
      return;
    }
    const file = fileList[0];
    // The mask's alpha is the selection, so only a PNG can carry it (a jpeg has no
    // alpha, a webp or gif would be refused by the gateway). `accept` is a hint, so
    // the type is checked here too.
    if (file.type.toLowerCase() !== 'image/png') {
      showError(t.chatMaskErrorType);
      return;
    }
    try {
      setMask(await prepareMaskDataUrl(file, images[0]));
    } catch (err) {
      showError(attachErrorText(err, t));
    }
  }

  const value: ChatStore = {
    messages,
    streaming,
    runningChatIds,
    isChatRunning: (id: string) => runningChatIds.has(id),
    input,
    images,
    audio,
    audioConverting,
    model,
    systemPrompt,
    temperature,
    maxTokens,
    selectedTokenId,
    chatModels,
    modelOptions,
    usableTokens,
    manageableServers,
    serverOverride,
    serverOverrideForceUnreachable,
    setServerOverride,
    setServerOverrideForceUnreachable,
    serverOverrideLocksChat,
    effectiveServerOverride,
    effectiveServerOverrideForce,
    serverOverrideModels,
    overrideModel,
    overrideLocksModel,
    modelAvailable,
    modelVisionCapable,
    modelImageCapable,
    modelImageEdit,
    modelSpeechCapable,
    modelTranscriptionCapable,
    attachMode,
    modelImageOptions,
    modelVoices,
    speechStreaming: speechStreamingGate.streaming,
    speechLanguage: speechLanguageGate.accepts,
    transcriptionStreaming,
    chatKind,
    imageSettings,
    setImageSettings,
    audioSettings: shownAudioSettings,
    setAudioSettings,
    pendingAudioRetention,
    confirmAudioRetention,
    cancelAudioRetention,
    transcriptionSettings,
    setTranscriptionSettings,
    runKind,
    runElapsedMs,
    runAudioLive,
    runAudioMuted,
    muteAudio,
    abortAudio,
    chats,
    activeChatId,
    chatsLoading,
    selectChat,
    deleteChat,
    renameChat,
    send,
    stop,
    newChat,
    setInput,
    handleFiles,
    removeImage,
    attachRecording,
    removeAudio,
    liveSession,
    startLiveTranscription: live.startLiveTranscription,
    stopLiveTranscription: live.stopLiveTranscription,
    abortLiveTranscription: live.abortLiveTranscription,
    keepLiveTranscript: live.keepLiveTranscript,
    voiceChat,
    voiceChatOffered,
    voiceChatCapable,
    voiceChatGap,
    mask,
    handleMaskFile,
    removeMask,
    composerFocusRequest,
    setModel,
    setSystemPrompt,
    setTemperature,
    setMaxTokens,
    setSelectedTokenId,
    handlersFor,
  };

  // Streaming boolean in its own (primitive) provider so per-token store-value
  // churn does not re-render streaming-only consumers (NavSidebar).
  return (
    <ChatStreamingContext.Provider value={streaming}>
      <ChatStoreContext.Provider value={value}>{children}</ChatStoreContext.Provider>
    </ChatStreamingContext.Provider>
  );
}
