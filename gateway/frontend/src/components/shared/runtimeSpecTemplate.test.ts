// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { runtimeSpecTemplate } from './runtimeSpecTemplate';

describe('runtimeSpecTemplate', () => {
  it('takes the parent flavors and both modes', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai', 'anthropic'],
        responses_mode: 'translate',
        messages_mode: 'disabled',
      }),
    ).toEqual({
      apiFlavors: ['openai', 'anthropic'],
      responsesMode: 'translate',
      messagesMode: 'disabled',
    });
  });

  it('leaves openai_images out when a text flavor survives', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai', 'openai_images'],
        responses_mode: 'passthrough',
        messages_mode: 'passthrough',
      }).apiFlavors,
    ).toEqual(['openai']);
  });

  it('keeps openai_images when the parent has no text flavor', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai_images'],
        responses_mode: 'passthrough',
        messages_mode: 'passthrough',
      }).apiFlavors,
    ).toEqual(['openai_images']);
  });

  // openai_audio is opt-in on the spec exactly as openai_images is: a
  // server_agent parent declares it for its speech children, and copying it
  // would make every new text model's spec admit speech requests too.
  it('leaves openai_audio out when a text flavor survives', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai', 'anthropic', 'openai_images', 'openai_audio'],
        responses_mode: 'passthrough',
        messages_mode: 'passthrough',
      }).apiFlavors,
    ).toEqual(['openai', 'anthropic']);
  });

  it('keeps openai_audio when the parent has no text flavor', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai_audio'],
        responses_mode: 'passthrough',
        messages_mode: 'passthrough',
      }).apiFlavors,
    ).toEqual(['openai_audio']);
  });

  it('keeps both specific flavors when the parent names no text flavor at all', () => {
    expect(
      runtimeSpecTemplate({
        api_flavors: ['openai_images', 'openai_audio'],
        responses_mode: 'passthrough',
        messages_mode: 'passthrough',
      }).apiFlavors,
    ).toEqual(['openai_images', 'openai_audio']);
  });

  it('returns a copy, never the parent array', () => {
    const parent = {
      api_flavors: ['openai_images'],
      responses_mode: 'passthrough' as const,
      messages_mode: 'passthrough' as const,
    };
    expect(runtimeSpecTemplate(parent).apiFlavors).not.toBe(parent.api_flavors);
  });
});
