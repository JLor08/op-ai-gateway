// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Which models a Voice Chat conversation (issue #312) uses for the two legs the thread does not
// pick for itself -- the live transcription model that hears the user (ASR) and the streaming
// speech model that answers aloud (TTS) -- once the user can pick them (#317). The pure rules,
// kept apart from the store so they can be read and tested on their own:
//
//   - the gateway says which of the catalogue's candidates STREAM (`thoseThatStream`), and only
//     those are on offer, for the user to pick from and for the automatic pick to choose from;
//   - the AUTOMATIC pick is the first of them, in the candidates' order (`autoVoiceModel`; the
//     speech candidates are ordered plain models first, preset-clone ones after, by the store);
//   - the user's pick wins while it is one of them, and otherwise the automatic pick does -- a
//     pick that is gone (the model left the catalogue, is unreachable, or no longer streams) is
//     a graceful fallback, never a stuck conversation (`effectiveVoiceModel`);
//   - the pickers show exactly that: the models on offer, the pick that is on offer, and the model
//     that would be used (`voiceModelChoice`).

import type { ModelOption } from '../../api';
import type { VoiceChatSettings } from './chatDoc';

/** The models the gateway says stream, per leg, in the candidates' order. */
export type VoiceStreaming = { asr: readonly string[]; tts: readonly string[] };

/** The model each leg would use ('' is none: nothing of that leg streams). */
export type VoiceLegs = { asr: string; tts: string };

/**
 * The `ids` that `streams` says yes to, in order. A lookup that fails is a no -- fail closed, like
 * every gate of the composers -- and does not spoil the answers for the others. Every id is asked
 * (in parallel), not only up to the first yes: the user may pick any of those that stream.
 */
export async function thoseThatStream(
  ids: readonly string[],
  streams: (id: string) => Promise<boolean>,
): Promise<string[]> {
  const answers = await Promise.all(ids.map((id) => streams(id).catch(() => false)));
  return ids.filter((_id, index) => answers[index]);
}

/** The automatic pick of a leg: the first model that streams, or '' when none does. */
export function autoVoiceModel(streaming: readonly string[]): string {
  return streaming[0] ?? '';
}

/**
 * The model a leg uses: the user's `pick` when it is one of the models that stream, else the
 * automatic pick (so a pick that is gone falls back, and '' -- no pick -- is the automatic one).
 */
export function effectiveVoiceModel(pick: string, streaming: readonly string[]): string {
  return pick !== '' && streaming.includes(pick) ? pick : autoVoiceModel(streaming);
}

/**
 * Both legs of a conversation, or null while the gateway has not yet said which models stream
 * (the switch is then closed: it must not promise a session it would refuse a ticket for).
 */
export function voiceLegsOf(
  streaming: VoiceStreaming | null,
  picks: VoiceChatSettings,
): VoiceLegs | null {
  if (streaming === null) return null;
  return {
    asr: effectiveVoiceModel(picks.asrModel, streaming.asr),
    tts: effectiveVoiceModel(picks.ttsModel, streaming.tts),
  };
}

/**
 * One of the two models of a Voice Chat that the thread does not pick for itself, the live
 * transcription model that hears the user (asr) and the streaming speech model that answers aloud
 * (tts), as the conversation's picker shows it (#317). The thread's own model is the third, the
 * one that answers, and it is picked with the thread's model selector.
 */
export type VoiceModelChoice = {
  /**
   * The models on offer: the candidates the gateway says STREAM, in the order the automatic pick
   * goes through them (the speech ones plain-first). Until the gateway has said -- and for a
   * thread that cannot chat, for which it is not asked -- the capability-filtered candidates
   * themselves: the picker is closed then (`VoiceChatModels.ready`), but still names the pick.
   */
  options: readonly ModelOption[];
  /**
   * The thread's own pick that is on offer, '' for none -- "automatic" -- and for a pick that is
   * gone (it left the catalogue, or no longer streams): the picker then shows automatic, and the
   * name stays in the thread, so it is picked again when the model is back. While the gateway has
   * not yet said what streams, the saved pick as it is.
   */
  pick: string;
  /**
   * The model a conversation would be started with: the pick while it is on offer, else the
   * automatic pick (the first model that streams); '' when none streams or none is known yet.
   */
  used: string;
  /** Choose a model ('' chooses automatic). Saved with the thread. */
  set: (model: string) => void;
};

/**
 * The models of a Voice Chat as the control shows them: the two pickers, whether the gateway has
 * said which models stream (until it has, there is nothing to choose from and the pickers are
 * closed), and the third model, the thread's own -- the one that answers, by its display name ('' when
 * the thread has none). The control only SHOWS that one: it is picked with the thread's model
 * selector, which is the LLM picker of the conversation.
 */
export type VoiceChatModels = {
  ready: boolean;
  llm: string;
  asr: VoiceModelChoice;
  tts: VoiceModelChoice;
};

/** What the picker of one leg is built from (see voiceModelChoice). */
type VoiceModelChoiceArgs = {
  /** The catalogue's models, to give each offered id its entry. */
  models: readonly ModelOption[];
  /** The leg's capability-filtered candidates (ids), in the automatic pick's order. */
  candidates: readonly string[];
  /** The ones the gateway says stream, or null while it has not said. */
  streaming: readonly string[] | null;
  /** The thread's saved pick ('' for none) and the model the conversation would use. */
  saved: string;
  used: string;
  set: (model: string) => void;
};

/**
 * The picker of one leg: the models on offer, the pick that is on offer (a saved pick that is
 * gone shows as automatic, see VoiceModelChoice.pick), the model that would be used, and the way
 * to choose. An id that is no longer in the catalogue has no entry to offer.
 */
export function voiceModelChoice({
  models,
  candidates,
  streaming,
  saved,
  used,
  set,
}: VoiceModelChoiceArgs): VoiceModelChoice {
  const offered = streaming ?? candidates;
  const options = offered
    .map((id) => models.find((option) => option.id === id))
    .filter((option): option is ModelOption => option !== undefined);
  const pick = streaming === null || streaming.includes(saved) ? saved : '';
  return { options, pick, used, set };
}
