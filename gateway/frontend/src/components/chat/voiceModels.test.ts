// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import type { ModelOption } from '../../api';
import {
  autoVoiceModel,
  effectiveVoiceModel,
  thoseThatStream,
  effectiveVoice,
  voiceLegsOf,
  voiceModelChoice,
  withTtsModel,
  type VoiceStreaming,
} from './voiceModels';

describe('thoseThatStream', () => {
  it('keeps the models the gateway says stream, in the order it was given them', async () => {
    const asked: string[] = [];
    const got = await thoseThatStream(['a', 'b', 'c', 'd'], async (id) => {
      asked.push(id);
      return id !== 'b';
    });
    expect(got).toEqual(['a', 'c', 'd']);
    // Every candidate is asked, even after one said yes: the user may pick any of them.
    expect(asked.sort()).toEqual(['a', 'b', 'c', 'd']);
  });

  it('fails closed: a lookup that fails is a no, and does not spoil the others', async () => {
    const got = await thoseThatStream(['a', 'b', 'c'], async (id) => {
      if (id === 'b') throw new Error('unreachable');
      return true;
    });
    expect(got).toEqual(['a', 'c']);
  });

  it('is empty when nothing streams, and when there is nothing to ask', async () => {
    expect(await thoseThatStream(['a'], async () => false)).toEqual([]);
    expect(await thoseThatStream([], async () => true)).toEqual([]);
  });
});

describe('autoVoiceModel', () => {
  it('is the first model that streams (the automatic pick), or none', () => {
    expect(autoVoiceModel(['asr-b', 'asr-a'])).toBe('asr-b');
    expect(autoVoiceModel([])).toBe('');
  });
});

describe('effectiveVoiceModel', () => {
  const streaming = ['asr-a', 'asr-b'];

  it('is the automatic pick while the user has chosen nothing', () => {
    expect(effectiveVoiceModel('', streaming)).toBe('asr-a');
  });

  it("is the user's pick when it is one of the models that stream", () => {
    expect(effectiveVoiceModel('asr-b', streaming)).toBe('asr-b');
  });

  it('falls back to the automatic pick when the pick is gone (not on offer or no longer streaming)', () => {
    expect(effectiveVoiceModel('asr-gone', streaming)).toBe('asr-a');
  });

  it('is none when nothing streams, whatever was picked', () => {
    expect(effectiveVoiceModel('asr-b', [])).toBe('');
    expect(effectiveVoiceModel('', [])).toBe('');
  });
});

describe('voiceLegsOf', () => {
  const streaming: VoiceStreaming = { asr: ['asr-a', 'asr-b'], tts: ['tts-a', 'tts-clone'] };

  it('is null while the gateway has not said which models stream', () => {
    expect(voiceLegsOf(null, { asrModel: 'asr-b', ttsModel: '' })).toBeNull();
  });

  it('is the automatic pick of each leg when nothing was chosen', () => {
    expect(voiceLegsOf(streaming, { asrModel: '', ttsModel: '' })).toEqual({
      asr: 'asr-a',
      tts: 'tts-a',
    });
  });

  it("is the user's pick of each leg, independently", () => {
    expect(voiceLegsOf(streaming, { asrModel: 'asr-b', ttsModel: '' })).toEqual({
      asr: 'asr-b',
      tts: 'tts-a',
    });
    expect(voiceLegsOf(streaming, { asrModel: '', ttsModel: 'tts-clone' })).toEqual({
      asr: 'asr-a',
      tts: 'tts-clone',
    });
  });

  it('falls back leg by leg for a pick that is gone', () => {
    expect(voiceLegsOf(streaming, { asrModel: 'asr-gone', ttsModel: 'tts-clone' })).toEqual({
      asr: 'asr-a',
      tts: 'tts-clone',
    });
  });

  it('leaves a leg empty when no model of it streams (the gap the switch names)', () => {
    expect(voiceLegsOf({ asr: [], tts: ['tts-a'] }, { asrModel: 'asr-a', ttsModel: '' })).toEqual({
      asr: '',
      tts: 'tts-a',
    });
  });
});

describe('voiceModelChoice', () => {
  const entry = (id: string): ModelOption => ({
    id,
    display_name: `${id} (display)`,
    flavors: ['openai_audio'],
    loading_on_count: 0,
  });
  const models = ['asr-a', 'asr-b', 'asr-c'].map(entry);
  const set = vi.fn();
  const choice = (over: Partial<Parameters<typeof voiceModelChoice>[0]> = {}) =>
    voiceModelChoice({
      models,
      candidates: ['asr-a', 'asr-b', 'asr-c'],
      streaming: ['asr-a', 'asr-b'],
      saved: '',
      used: 'asr-a',
      set,
      ...over,
    });

  it('offers the catalogue entries of the models that stream, in their order', () => {
    expect(choice().options.map((option) => option.id)).toEqual(['asr-a', 'asr-b']);
    expect(choice({ streaming: ['asr-b', 'asr-a'] }).options.map((o) => o.id)).toEqual([
      'asr-b',
      'asr-a',
    ]);
  });

  it('skips a model that is no longer in the catalogue', () => {
    expect(choice({ streaming: ['asr-a', 'asr-gone'] }).options.map((o) => o.id)).toEqual([
      'asr-a',
    ]);
  });

  it('offers the candidates themselves while the gateway has not said which stream', () => {
    const open = choice({ streaming: null, used: '' });
    expect(open.options.map((option) => option.id)).toEqual(['asr-a', 'asr-b', 'asr-c']);
  });

  it('shows the saved pick while it is on offer', () => {
    expect(choice({ saved: 'asr-b' }).pick).toBe('asr-b');
  });

  it('shows automatic for a saved pick that is gone, or no longer streams', () => {
    expect(choice({ saved: 'asr-gone' }).pick).toBe('');
    // asr-c is a candidate, but the gateway says it does not stream.
    expect(choice({ saved: 'asr-c' }).pick).toBe('');
  });

  it('shows the saved pick as it is while the gateway has not said (nothing to judge it by)', () => {
    expect(choice({ streaming: null, saved: 'asr-c', used: '' }).pick).toBe('asr-c');
  });

  it('hands over the model that would be used, and the way to choose', () => {
    const got = choice({ used: 'asr-b' });
    expect(got.used).toBe('asr-b');
    got.set('asr-b');
    expect(set).toHaveBeenCalledWith('asr-b');
  });
});

// The voice a conversation speaks in (#318): what the dropdown shows is what the ticket carries.
describe('effectiveVoice', () => {
  const voices = ['alba', 'marius'];

  it("is the user's pick, for a plain model and a preset-clone one alike", () => {
    expect(effectiveVoice('marius', voices, false)).toBe('marius');
    expect(effectiveVoice('marius', voices, true)).toBe('marius');
  });

  it('keeps a typed name the list does not hold: the list is an offer, never a constraint', () => {
    expect(effectiveVoice('typed-by-hand', voices, true)).toBe('typed-by-hand');
  });

  it('pre-selects the first preset of a preset-clone model that has no pick, so it never runs blank', () => {
    expect(effectiveVoice('', voices, true)).toBe('alba');
    expect(effectiveVoice('   ', voices, true)).toBe('alba');
  });

  it('leaves a plain model that has no pick on its own default voice', () => {
    expect(effectiveVoice('', voices, false)).toBe('');
  });

  it('is none while the model has no voices to show -- the field is hidden, so a stale pick is not sent', () => {
    expect(effectiveVoice('alba', [], false)).toBe('');
    expect(effectiveVoice('alba', [], true)).toBe('');
    expect(effectiveVoice('', [], true)).toBe('');
  });
});

// A voice is a voice of the model that offered it: choosing another speech model must not carry it
// there (#318) -- but choosing the model that would speak anyway changes nothing about it.
describe('withTtsModel', () => {
  const streaming = ['tts-a', 'tts-clone'];
  // The thread's own system prompt rides along: a speech model switch is not its business.
  const current = { asrModel: 'asr-b', ttsModel: '', voice: 'marius', systemPrompt: 'Be terse.' };

  it('records the pick and clears the voice when another model would now speak', () => {
    expect(withTtsModel(current, 'tts-clone', streaming)).toEqual({
      asrModel: 'asr-b',
      ttsModel: 'tts-clone',
      voice: '',
      systemPrompt: 'Be terse.',
    });
    expect(withTtsModel({ ...current, ttsModel: 'tts-clone' }, '', streaming).voice).toBe('');
  });

  it('keeps the voice when the model that would speak is the same one (automatic vs. the pick of it)', () => {
    expect(withTtsModel(current, 'tts-a', streaming)).toEqual({ ...current, ttsModel: 'tts-a' });
    expect(withTtsModel({ ...current, ttsModel: 'tts-a' }, '', streaming).voice).toBe('marius');
    expect(withTtsModel(current, 'tts-clone', streaming).asrModel).toBe('asr-b');
  });

  it('treats a pick that is gone as the automatic one, as the conversation does', () => {
    expect(withTtsModel(current, 'tts-gone', streaming).voice).toBe('marius');
  });
});
