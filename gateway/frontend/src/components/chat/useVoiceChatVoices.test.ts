// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PortalApi } from '../../api';
import { useVoiceChatVoices } from './useVoiceChatVoices';

type ModelVoices = PortalApi['modelVoices'];

// One pending lookup per call, so that a test decides when (and how) each model's answer arrives.
type Lookup = {
  model: string;
  resolve: (voices: string[]) => void;
  reject: (reason: unknown) => void;
};

function fakeApi() {
  const lookups: Lookup[] = [];
  const modelVoices = vi.fn<ModelVoices>(
    (model) =>
      new Promise<string[]>((resolve, reject) => {
        lookups.push({ model, resolve, reject });
      }),
  );
  const apiRef = { current: { modelVoices } };
  return { apiRef, modelVoices, lookups };
}

afterEach(cleanup);

describe('useVoiceChatVoices', () => {
  it('is loading until the lookup answers, then ready with the model voices', async () => {
    const { apiRef, modelVoices, lookups } = fakeApi();
    const { result } = renderHook(() => useVoiceChatVoices(apiRef, 'kokoro'));

    expect(result.current).toEqual({ status: 'loading' });
    expect(modelVoices).toHaveBeenCalledWith('kokoro');

    await act(async () => lookups[0]?.resolve(['af_heart', 'am_adam']));

    expect(result.current).toEqual({ status: 'ready', voices: ['af_heart', 'am_adam'] });
  });

  it('is ready with no voices for a model whose list is empty (known, not loading)', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result } = renderHook(() => useVoiceChatVoices(apiRef, 'kokoro'));

    await act(async () => lookups[0]?.resolve([]));

    expect(result.current).toEqual({ status: 'ready', voices: [] });
  });

  it('is failed when the lookup is rejected', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result } = renderHook(() => useVoiceChatVoices(apiRef, 'kokoro'));

    expect(result.current).toEqual({ status: 'loading' });
    await act(async () => lookups[0]?.reject(new Error('gateway unreachable')));

    expect(result.current).toEqual({ status: 'failed' });
  });

  it('is ready with no voices, without any lookup, while there is no model', () => {
    const { apiRef, modelVoices } = fakeApi();
    const { result } = renderHook(() => useVoiceChatVoices(apiRef, ''));

    expect(result.current).toEqual({ status: 'ready', voices: [] });
    expect(modelVoices).not.toHaveBeenCalled();
  });

  it('hands out the very same list object, render after render, while there is no model (a stable effect dependency)', () => {
    const { apiRef } = fakeApi();
    const { result, rerender } = renderHook(() => useVoiceChatVoices(apiRef, ''));
    const first = result.current;

    rerender();
    expect(result.current).toBe(first);

    rerender();
    expect(result.current).toBe(first);
  });

  it('never hands out one model list for another: loading while the next lookup is in flight', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    await act(async () => lookups[0]?.resolve(['af_heart']));
    expect(result.current).toEqual({ status: 'ready', voices: ['af_heart'] });

    rerender({ model: 'pocket' });

    // In the very render of the switch, before the effect or any answer: not kokoro's list.
    expect(result.current).toEqual({ status: 'loading' });
    expect(lookups[1]?.model).toBe('pocket');

    await act(async () => lookups[1]?.resolve(['alba']));
    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });
  });

  it('does not keep a failure for the next model: loading while its lookup is in flight', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    await act(async () => lookups[0]?.reject(new Error('boom')));
    expect(result.current).toEqual({ status: 'failed' });

    rerender({ model: 'pocket' });

    expect(result.current).toEqual({ status: 'loading' });
  });

  it('ignores a slow answer for a model that is no longer the one asked about', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    rerender({ model: 'pocket' });

    // kokoro's answer arrives late, after pocket was asked for: it must not show up as pocket's.
    await act(async () => lookups[0]?.resolve(['af_heart']));
    expect(result.current).toEqual({ status: 'loading' });

    await act(async () => lookups[1]?.resolve(['alba']));
    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });
  });

  it('ignores a slow failure for a model that is no longer the one asked about', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    rerender({ model: 'pocket' });

    await act(async () => lookups[0]?.reject(new Error('late failure')));
    expect(result.current).toEqual({ status: 'loading' });

    await act(async () => lookups[1]?.resolve(['alba']));
    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });
  });

  it('does not let a late answer for a previous model displace the current ready list', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    rerender({ model: 'pocket' });
    await act(async () => lookups[1]?.resolve(['alba']));
    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });

    // kokoro answers only now. Were it held, pocket would read as loading with no lookup left.
    await act(async () => lookups[0]?.resolve(['af_heart']));

    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });
  });

  it('does not let a late failure for a previous model displace the current ready list', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    rerender({ model: 'pocket' });
    await act(async () => lookups[1]?.resolve(['alba']));

    await act(async () => lookups[0]?.reject(new Error('late failure')));

    expect(result.current).toEqual({ status: 'ready', voices: ['alba'] });
  });

  it('keeps the newest answer when the model is switched away and back', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    rerender({ model: 'pocket' });
    rerender({ model: 'kokoro' });
    expect(lookups.map((l) => l.model)).toEqual(['kokoro', 'pocket', 'kokoro']);

    await act(async () => lookups[2]?.resolve(['fresh']));
    // The first kokoro lookup was superseded: its (older) answer must not overwrite the newer one.
    await act(async () => lookups[0]?.resolve(['stale']));

    expect(result.current).toEqual({ status: 'ready', voices: ['fresh'] });
  });

  it('is ready with no voices once the model is cleared, and not the previous list', async () => {
    const { apiRef, lookups } = fakeApi();
    const { result, rerender } = renderHook(({ model }) => useVoiceChatVoices(apiRef, model), {
      initialProps: { model: 'kokoro' },
    });
    await act(async () => lookups[0]?.resolve(['af_heart']));

    rerender({ model: '' });

    expect(result.current).toEqual({ status: 'ready', voices: [] });
  });
});
