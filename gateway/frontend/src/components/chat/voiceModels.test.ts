// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import type { ModelOption } from '../../api';
import {
  autoVoiceModel,
  effectiveVoiceModel,
  reconcileVoice,
  stickyAutoVoiceModel,
  thoseThatStream,
  effectiveVoice,
  stickyVoiceLegsOf,
  voiceModelChoice,
  withTtsModel,
  type VoiceList,
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

// A catalogue entry of an audio model, loaded on a server or not (`ModelOption.loaded`).
const catalogued = (id: string, loaded?: boolean): ModelOption => ({
  id,
  display_name: `${id} (display)`,
  flavors: ['openai_audio'],
  loading_on_count: 0,
  ...(loaded === undefined ? {} : { loaded, loaded_on: loaded ? ['server-1'] : [] }),
});

describe('autoVoiceModel', () => {
  it('is the first model that streams (the automatic pick), or none', () => {
    expect(autoVoiceModel(['asr-b', 'asr-a'])).toBe('asr-b');
    expect(autoVoiceModel([])).toBe('');
  });

  describe('with the catalogue to say which models are loaded', () => {
    it('prefers a loaded model over an earlier one that is not loaded', () => {
      const models = [catalogued('asr-a', false), catalogued('asr-b', true)];
      expect(autoVoiceModel(['asr-a', 'asr-b'], models)).toBe('asr-b');
    });

    it('takes the first of several loaded models, in the order it was given them', () => {
      const models = [
        catalogued('asr-a', false),
        catalogued('asr-b', true),
        catalogued('asr-c', true),
      ];
      expect(autoVoiceModel(['asr-a', 'asr-b', 'asr-c'], models)).toBe('asr-b');
      expect(autoVoiceModel(['asr-c', 'asr-b', 'asr-a'], models)).toBe('asr-c');
    });

    it('falls back to the first model that streams when none of them is loaded', () => {
      const models = [catalogued('asr-a', false), catalogued('asr-b'), catalogued('asr-c', false)];
      expect(autoVoiceModel(['asr-b', 'asr-a', 'asr-c'], models)).toBe('asr-b');
    });

    it('never picks a loaded model that does not stream: only the ones on offer are considered', () => {
      const models = [catalogued('asr-a', false), catalogued('asr-b', true)];
      expect(autoVoiceModel(['asr-a'], models)).toBe('asr-a');
    });

    it('treats a model the catalogue does not know as not loaded', () => {
      expect(autoVoiceModel(['asr-a', 'asr-b'], [catalogued('asr-b', true)])).toBe('asr-b');
      expect(autoVoiceModel(['asr-a', 'asr-b'], [])).toBe('asr-a');
    });

    it('is none when nothing streams, whatever is loaded', () => {
      expect(autoVoiceModel([], [catalogued('asr-a', true)])).toBe('');
    });
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

  describe('with the catalogue to say which models are loaded', () => {
    const models = [catalogued('asr-a', false), catalogued('asr-b', true)];

    it('is the loaded model while the user has chosen nothing, or a pick that is gone', () => {
      expect(effectiveVoiceModel('', streaming, models)).toBe('asr-b');
      expect(effectiveVoiceModel('asr-gone', streaming, models)).toBe('asr-b');
    });

    it("is still the user's pick when it is on offer, loaded or not", () => {
      expect(effectiveVoiceModel('asr-a', streaming, models)).toBe('asr-a');
    });
  });
});

describe('stickyAutoVoiceModel', () => {
  it('keeps a previous pick that still streams, even when another model is now loaded', () => {
    expect(stickyAutoVoiceModel('m1', ['m1', 'm2'], [catalogued('m2', true)])).toBe('m1');
  });

  it('keeps a previous pick that streams and is itself not loaded (a cold model stays)', () => {
    expect(
      stickyAutoVoiceModel('m1', ['m1', 'm2'], [catalogued('m1', false), catalogued('m2', true)]),
    ).toBe('m1');
  });

  it('re-resolves (prefer-loaded) when the previous pick no longer streams', () => {
    expect(stickyAutoVoiceModel('m1', ['m2', 'm3'], [catalogued('m3', true)])).toBe('m3');
  });

  it('is a fresh resolution when there is no previous pick', () => {
    expect(stickyAutoVoiceModel('', ['m2', 'm3'], [])).toBe('m2');
    expect(stickyAutoVoiceModel('', ['m2', 'm3'], [catalogued('m3', true)])).toBe('m3');
  });

  it('never reads an empty previous pick as a model, whatever an (impossible) empty id would say', () => {
    expect(stickyAutoVoiceModel('', ['', 'm2'], [catalogued('m2', true)])).toBe('m2');
  });

  it('is none when nothing streams, whatever the previous pick was', () => {
    expect(stickyAutoVoiceModel('m1', [], [catalogued('m1', true)])).toBe('');
    expect(stickyAutoVoiceModel('', [], [])).toBe('');
  });

  it('keeps the previous pick wherever it stands among the models that stream (not only the first)', () => {
    // m1 comes first and is the one that is loaded: a fresh resolution would say m1, so only the
    // kept previous pick (m2, second of the models that stream) can give m2.
    expect(stickyAutoVoiceModel('m2', ['m1', 'm2'], [catalogued('m1', true)])).toBe('m2');
  });
});

describe('stickyVoiceLegsOf', () => {
  const streaming: VoiceStreaming = { asr: ['asr-a', 'asr-b'], tts: ['tts-a', 'tts-clone'] };
  const NOTHING_HELD = { asr: '', tts: '' };
  // No voice saved, and so none attributed to a model.
  const NO_VOICE = { voice: '', voiceModel: '' };
  // The legs alone, resolved with no automatic pick held yet: what a conversation starts with.
  const voiceLegsOf = (
    streams: VoiceStreaming | null,
    picks: { asrModel: string; ttsModel: string },
    models: readonly ModelOption[] = [],
  ) => stickyVoiceLegsOf(streams, { ...picks, ...NO_VOICE }, models, NOTHING_HELD)?.legs ?? null;

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

  describe('with the catalogue to say which models are loaded', () => {
    const models = [
      catalogued('asr-a', false),
      catalogued('asr-b', true),
      catalogued('tts-a', false),
      catalogued('tts-clone', true),
    ];

    it('is the loaded model of each leg that has no pick of its own', () => {
      expect(voiceLegsOf(streaming, { asrModel: '', ttsModel: '' }, models)).toEqual({
        asr: 'asr-b',
        tts: 'tts-clone',
      });
    });

    it("keeps the user's pick of a leg, and is the loaded model for the other", () => {
      expect(voiceLegsOf(streaming, { asrModel: 'asr-a', ttsModel: '' }, models)).toEqual({
        asr: 'asr-a',
        tts: 'tts-clone',
      });
    });

    it('is still null while the gateway has not said which models stream', () => {
      expect(voiceLegsOf(null, { asrModel: '', ttsModel: '' }, models)).toBeNull();
    });
  });

  // The automatic pick held steady (#326): `held` is the model each leg's automatic pick resolved
  // to until now, and the result says what to hold from now on.
  describe('holding the automatic pick', () => {
    const picks = { asrModel: '', ttsModel: '', ...NO_VOICE };
    // asr-b and tts-clone are loaded: a fresh automatic pick would be those two.
    const models = [
      catalogued('asr-a', false),
      catalogued('asr-b', true),
      catalogued('tts-a', false),
      catalogued('tts-clone', true),
    ];

    it('seeds from a fresh automatic pick (prefer loaded) when nothing is held, and holds it', () => {
      expect(stickyVoiceLegsOf(streaming, picks, models, NOTHING_HELD)).toEqual({
        legs: { asr: 'asr-b', tts: 'tts-clone' },
        held: { asr: 'asr-b', tts: 'tts-clone' },
      });
    });

    it('keeps the held model of a leg while it streams, though another one is loaded', () => {
      expect(stickyVoiceLegsOf(streaming, picks, models, { asr: 'asr-a', tts: 'tts-a' })).toEqual({
        legs: { asr: 'asr-a', tts: 'tts-a' },
        held: { asr: 'asr-a', tts: 'tts-a' },
      });
    });

    it('moves a leg whose held model no longer streams, to a fresh pick, leg by leg', () => {
      // tts-b comes first, tts-clone is the one that is loaded: the fresh pick prefers it.
      const now: VoiceStreaming = { asr: ['asr-a', 'asr-b'], tts: ['tts-b', 'tts-clone'] };
      expect(stickyVoiceLegsOf(now, picks, models, { asr: 'asr-a', tts: 'tts-a' })).toEqual({
        legs: { asr: 'asr-a', tts: 'tts-clone' },
        held: { asr: 'asr-a', tts: 'tts-clone' },
      });
    });

    it("uses the user's pick of a leg and holds no automatic pick for it", () => {
      expect(
        stickyVoiceLegsOf(
          streaming,
          { asrModel: 'asr-a', ttsModel: 'tts-a', ...NO_VOICE },
          models,
          { asr: 'asr-b', tts: 'tts-clone' },
        ),
      ).toEqual({
        legs: { asr: 'asr-a', tts: 'tts-a' },
        held: { asr: '', tts: '' },
      });
    });

    it('picks afresh when the user returns a leg to automatic (nothing was held under the pick)', () => {
      const underPick = stickyVoiceLegsOf(
        streaming,
        { asrModel: 'asr-a', ttsModel: '', ...NO_VOICE },
        models,
        { asr: '', tts: 'tts-a' },
      );
      expect(underPick?.held).toEqual({ asr: '', tts: 'tts-a' });
      expect(stickyVoiceLegsOf(streaming, picks, models, underPick!.held)?.legs).toEqual({
        asr: 'asr-b',
        tts: 'tts-a',
      });
    });

    it('holds the automatic pick for a pick that is gone, which falls back to it', () => {
      expect(
        stickyVoiceLegsOf(streaming, { asrModel: 'asr-gone', ttsModel: '', ...NO_VOICE }, models, {
          asr: 'asr-a',
          tts: '',
        }),
      ).toEqual({
        legs: { asr: 'asr-a', tts: 'tts-clone' },
        held: { asr: 'asr-a', tts: 'tts-clone' },
      });
    });

    it('holds nothing for a leg of which no model streams', () => {
      expect(
        stickyVoiceLegsOf({ asr: [], tts: ['tts-a'] }, picks, models, { asr: 'asr-a', tts: '' }),
      ).toEqual({
        legs: { asr: '', tts: 'tts-a' },
        held: { asr: '', tts: 'tts-a' },
      });
    });

    // A pick made afresh starts, for the speech leg, from the model the saved voice was chosen for.
    describe('with a saved voice that was chosen for a model', () => {
      const withVoice = { asrModel: '', ttsModel: '', voice: 'marius', voiceModel: 'tts-a' };

      it('starts the speech leg from that model when it streams, though another is loaded', () => {
        expect(stickyVoiceLegsOf(streaming, withVoice, models, NOTHING_HELD)).toEqual({
          // The transcription leg has no such model: prefer-loaded, as before.
          legs: { asr: 'asr-b', tts: 'tts-a' },
          held: { asr: 'asr-b', tts: 'tts-a' },
        });
      });

      it('falls back to the fresh pick when that model no longer streams', () => {
        const now: VoiceStreaming = { asr: ['asr-a', 'asr-b'], tts: ['tts-b', 'tts-clone'] };
        expect(stickyVoiceLegsOf(now, withVoice, models, NOTHING_HELD)?.legs.tts).toBe('tts-clone');
      });

      it('ignores the model of a voice that is not there (no voice, or only blanks)', () => {
        expect(
          stickyVoiceLegsOf(streaming, { ...withVoice, voice: '' }, models, NOTHING_HELD)?.legs.tts,
        ).toBe('tts-clone');
        expect(
          stickyVoiceLegsOf(streaming, { ...withVoice, voice: '  ' }, models, NOTHING_HELD)?.legs
            .tts,
        ).toBe('tts-clone');
      });

      it('ignores a voice that has no model recorded', () => {
        expect(
          stickyVoiceLegsOf(streaming, { ...withVoice, voiceModel: '' }, models, NOTHING_HELD)?.legs
            .tts,
        ).toBe('tts-clone');
      });

      it('keeps what is held while it streams: the voice does not move a pick that was made', () => {
        expect(
          stickyVoiceLegsOf(streaming, withVoice, models, { asr: '', tts: 'tts-clone' })?.legs.tts,
        ).toBe('tts-clone');
      });

      it('goes back to that model when what was held stops streaming', () => {
        const now: VoiceStreaming = { asr: ['asr-a', 'asr-b'], tts: ['tts-a', 'tts-clone'] };
        expect(
          stickyVoiceLegsOf(now, withVoice, models, { asr: '', tts: 'tts-gone' })?.legs.tts,
        ).toBe('tts-a');
      });

      it("yields to the user's own pick of the speech model", () => {
        expect(
          stickyVoiceLegsOf(
            streaming,
            { ...withVoice, ttsModel: 'tts-clone' },
            models,
            NOTHING_HELD,
          ),
        ).toMatchObject({ legs: { tts: 'tts-clone' }, held: { tts: '' } });
      });
    });

    it('is null while the gateway has not said which models stream', () => {
      expect(stickyVoiceLegsOf(null, picks, models, { asr: 'asr-a', tts: 'tts-a' })).toBeNull();
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

// A saved voice belongs to the model it was chosen for (`voiceModel`). When the model that would
// speak is another one, the voice is used only if that model offers it -- and only once its voice
// list is KNOWN: a list that is still loading, or that failed, decides nothing. The result is the
// voice to speak in: the saved one as it is, or '' for "the model's own default".
describe('reconcileVoice', () => {
  const ready = (voices: string[]): VoiceList => ({ status: 'ready', voices });

  it('is the saved voice while it was chosen for the model that speaks (a typed name is kept)', () => {
    expect(
      reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm1', voices: ready([]) }),
    ).toBe('alloy');
  });

  it('keeps the voice when the new model offers it', () => {
    expect(
      reconcileVoice({
        voice: 'alloy',
        voiceModel: 'm1',
        effectiveModel: 'm2',
        voices: ready(['alloy']),
      }),
    ).toBe('alloy');
  });

  it('is no voice when the new model does not offer it', () => {
    expect(
      reconcileVoice({
        voice: 'alloy',
        voiceModel: 'm1',
        effectiveModel: 'm2',
        voices: ready(['nova']),
      }),
    ).toBe('');
  });

  it('defers while the new model voice list is not known: loading or failed keeps the voice', () => {
    const base = { voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm2' };
    expect(reconcileVoice({ ...base, voices: { status: 'loading' } })).toBe('alloy');
    expect(reconcileVoice({ ...base, voices: { status: 'failed' } })).toBe('alloy');
  });

  it('is the voice as it is without a voice, even when the attribution is stale (nothing would be sent)', () => {
    expect(
      reconcileVoice({
        voice: '',
        voiceModel: 'm1',
        effectiveModel: 'm2',
        voices: ready(['nova']),
      }),
    ).toBe('');
    expect(
      reconcileVoice({
        voice: '   ',
        voiceModel: 'm1',
        effectiveModel: 'm2',
        voices: ready(['nova']),
      }),
    ).toBe('   ');
  });

  it('keeps the voice without a model that would speak', () => {
    expect(
      reconcileVoice({
        voice: 'alloy',
        voiceModel: 'm1',
        effectiveModel: '',
        voices: ready(['nova']),
      }),
    ).toBe('alloy');
  });

  it('treats a thread saved before the attribution existed (voiceModel empty) as a differing model', () => {
    expect(
      reconcileVoice({
        voice: 'alloy',
        voiceModel: '',
        effectiveModel: 'm2',
        voices: ready(['alloy']),
      }),
    ).toBe('alloy');
    expect(
      reconcileVoice({
        voice: 'alloy',
        voiceModel: '',
        effectiveModel: 'm2',
        voices: ready(['nova']),
      }),
    ).toBe('');
  });

  it('compares the voice trimmed, as the saved and the spoken voice are (and returns it as saved)', () => {
    expect(
      reconcileVoice({
        voice: ' alloy ',
        voiceModel: 'm1',
        effectiveModel: 'm2',
        voices: ready(['alloy']),
      }),
    ).toBe(' alloy ');
  });
});

// A voice is a voice of the model that offered it: choosing another speech model must not carry it
// there (#318) -- but choosing the model that would speak anyway changes nothing about it. "The model
// that speaks now" is passed in by the caller (the conversation's effective, sticky pick), never
// recomputed here.
describe('withTtsModel', () => {
  const streaming = ['tts-a', 'tts-clone'];
  // The thread's own system prompt rides along: a speech model switch is not its business. The voice
  // was chosen for tts-a (`voiceModel`), the model that speaks while the thread is on automatic.
  const current = {
    asrModel: 'asr-b',
    ttsModel: '',
    voice: 'marius',
    voiceModel: 'tts-a',
    systemPrompt: 'Be terse.',
  };

  it('records the pick and clears the voice and its attribution when another model would now speak', () => {
    expect(withTtsModel(current, 'tts-clone', 'tts-a', streaming)).toEqual({
      asrModel: 'asr-b',
      ttsModel: 'tts-clone',
      voice: '',
      voiceModel: '',
      systemPrompt: 'Be terse.',
    });
    expect(
      withTtsModel({ ...current, ttsModel: 'tts-clone' }, '', 'tts-clone', streaming),
    ).toMatchObject({ ttsModel: '', voice: '', voiceModel: '' });
  });

  it('keeps the voice and its attribution when the model that would speak is the same one (automatic vs. the pick of it)', () => {
    expect(withTtsModel(current, 'tts-a', 'tts-a', streaming)).toEqual({
      ...current,
      ttsModel: 'tts-a',
    });
    expect(withTtsModel({ ...current, ttsModel: 'tts-a' }, '', 'tts-a', streaming)).toMatchObject({
      voice: 'marius',
      voiceModel: 'tts-a',
    });
    expect(withTtsModel(current, 'tts-clone', 'tts-a', streaming).asrModel).toBe('asr-b');
  });

  it('treats a pick that is gone as the automatic one, as the conversation does', () => {
    expect(withTtsModel(current, 'tts-gone', 'tts-a', streaming).voice).toBe('marius');
  });

  // The model that speaks is whatever the caller says it is -- the sticky automatic pick -- and not
  // what a fresh automatic resolution would give now. Here the sticky pick is tts-a while tts-clone
  // has since been loaded (so a fresh resolution would say tts-clone).
  describe('with a speaking model that differs from a fresh automatic resolution', () => {
    const models = [catalogued('tts-a', false), catalogued('tts-clone', true)];

    it('keeps the voice when the pick is the model that speaks now, though it is not the loaded one', () => {
      expect(withTtsModel(current, 'tts-a', 'tts-a', streaming, models)).toEqual({
        ...current,
        ttsModel: 'tts-a',
      });
    });

    it('clears the voice when the pick is the loaded model the conversation is not speaking with', () => {
      expect(withTtsModel(current, 'tts-clone', 'tts-a', streaming, models)).toEqual({
        ...current,
        ttsModel: 'tts-clone',
        voice: '',
        voiceModel: '',
      });
    });
  });

  // Picking automatic ('') resolves, afresh, to the loaded model: that is another model than the
  // sticky one that speaks now, so the voice goes with it.
  it('clears the voice when automatic would now resolve to another model than the one speaking', () => {
    const models = [catalogued('tts-a', false), catalogued('tts-clone', true)];
    expect(
      withTtsModel({ ...current, ttsModel: 'tts-a' }, '', 'tts-a', streaming, models),
    ).toMatchObject({ ttsModel: '', voice: '', voiceModel: '' });
  });
});
