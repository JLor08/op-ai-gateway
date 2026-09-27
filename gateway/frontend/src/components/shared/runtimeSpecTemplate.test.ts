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

  it('returns a copy, never the parent array', () => {
    const parent = {
      api_flavors: ['openai_images'],
      responses_mode: 'passthrough' as const,
      messages_mode: 'passthrough' as const,
    };
    expect(runtimeSpecTemplate(parent).apiFlavors).not.toBe(parent.api_flavors);
  });
});
