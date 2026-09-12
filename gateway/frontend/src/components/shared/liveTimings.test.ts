// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { applicationLiveTimingsKind, runtimeSpecLiveTimingsKind } from './liveTimings';
import type { ApplicationType, RuntimeSpec } from '../../api';

// Both lists are stated EXHAUSTIVELY and are the whole point of the test: the
// Go set (routing.liveTimingsCapableKinds, read by
// routing.LiveTimingsCapableKind) is a map this module hand-copies, and
// nothing compiles the two together. A kind added to the Go set without a
// matching edit here is a silent portal that never offers the switch; a kind
// left here after Go drops it is a portal that offers a switch every save
// refuses. Naming every member in both directions is what makes either
// direction fail loudly.
const allApplicationTypes: ApplicationType[] = [
  'ollama',
  'vllm',
  'llama_cpp',
  'llama_swap',
  'litellm',
  'server_agent',
];

const allSpecTypes: RuntimeSpec['type'][] = ['', 'vllm', 'llama_cpp', 'tgi', 'ollama', 'custom'];

describe('applicationLiveTimingsKind', () => {
  it('calls llama_cpp capable and every other application type incapable', () => {
    expect(applicationLiveTimingsKind('llama_cpp')).toBe('capable');
    for (const type of allApplicationTypes.filter((t) => t !== 'llama_cpp')) {
      expect(applicationLiveTimingsKind(type), type).toBe('incapable');
    }
  });

  // vLLM was in the capable set in part 1 and left it in part 2 (design D6):
  // the key is a llama.cpp parameter and was measured completely inert on
  // vLLM's /v1/responses -- 48 frames, zero carrying `timings`. A switch that
  // is offered and provably delivers nothing is worse than one that is not.
  it('does NOT call vllm capable', () => {
    expect(applicationLiveTimingsKind('vllm')).toBe('incapable');
  });

  // The application form always knows its own `type`, so it must never see
  // the third state; `unknown` belongs to the launch-spec form alone.
  it('never answers unknown, for any application type', () => {
    for (const type of allApplicationTypes) {
      expect(applicationLiveTimingsKind(type), type).not.toBe('unknown');
    }
  });
});

describe('runtimeSpecLiveTimingsKind', () => {
  it('answers unknown for Auto: the kind is detected from the binary, and that detection is Go-only', () => {
    expect(runtimeSpecLiveTimingsKind('')).toBe('unknown');
  });

  it('answers capable for an explicit llama_cpp and incapable for every other explicit kind', () => {
    expect(runtimeSpecLiveTimingsKind('llama_cpp')).toBe('capable');
    for (const specType of allSpecTypes.filter((s) => s !== '' && s !== 'llama_cpp')) {
      expect(runtimeSpecLiveTimingsKind(specType), specType).toBe('incapable');
    }
  });
});
