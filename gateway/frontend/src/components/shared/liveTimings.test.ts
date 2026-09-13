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
  it('calls llama_cpp capable and every other DIRECT application type incapable', () => {
    expect(applicationLiveTimingsKind('llama_cpp')).toBe('capable');
    for (const type of allApplicationTypes.filter(
      (t) => t !== 'llama_cpp' && t !== 'server_agent',
    )) {
      expect(applicationLiveTimingsKind(type), type).toBe('incapable');
    }
  });

  // server_agent is NOT incapable, and calling it that was a real defect: the
  // resolver overwrites the application's stored value with the SPEC's for a
  // server_agent app, and the request-path gate judges the spec's effective
  // kind rather than the application type. Runtime specs exist only under
  // server_agent applications, so every managed llama.cpp runtime -- this
  // feature's primary deployment shape -- is reached through this form. Told
  // "only llama.cpp, not available for this type", such an operator concludes
  // the feature does not apply to them and never reaches the launch spec where
  // the switch actually lives.
  it('calls server_agent delegated: the launch spec decides, not the application row', () => {
    expect(applicationLiveTimingsKind('server_agent')).toBe('delegated');
  });

  // Still no checkbox for it, though -- 'delegated' shares that with
  // 'incapable'. The application row's value IS overwritten by the spec's, so
  // a control here would be a second switch that does nothing.
  it('never answers capable for server_agent, so no application-side control can appear', () => {
    expect(applicationLiveTimingsKind('server_agent')).not.toBe('capable');
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

  // 'delegated' is the APPLICATION form's answer for server_agent and has no
  // meaning here: a runtime spec has nothing further to delegate to, and its
  // own Type select is already the signal. Pinned so a later edit cannot
  // quietly hand this side a state it has no rendering for.
  it('never answers delegated, for any spec type', () => {
    for (const specType of allSpecTypes) {
      expect(runtimeSpecLiveTimingsKind(specType), specType).not.toBe('delegated');
    }
  });
});
