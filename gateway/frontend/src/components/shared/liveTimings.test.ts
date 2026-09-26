// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  applicationLiveTimingsKind,
  applicationSendsLiveTimings,
  liveTimingsControlLayout,
  runtimeSpecLiveTimingsKind,
  runtimeSpecSendsLiveTimings,
  type LiveTimingsKind,
} from './liveTimings';
import type { ApplicationType, RuntimeSpec } from '../../api';

// The full LiveTimingsKind union, so the tests below fail to COMPILE if a state
// is added -- the same never-arm exhaustiveness the helpers themselves carry
// (issue #88). Adding a member here without a case in these tables is a type
// error; forgetting it entirely drops coverage but the helpers' own assertNever
// arms still fail the build.
const allLiveTimingsKinds: readonly LiveTimingsKind[] = [
  'capable',
  'incapable',
  'unknown',
  'delegated',
];

// Both lists are stated EXHAUSTIVELY, and it is worth being exact about what
// that does and does not buy, because the obvious reading is too generous.
//
// The Go set (routing.liveTimingsCapableKinds, read by
// routing.LiveTimingsCapableKind) is a map liveTimings.ts hand-copies, and
// nothing compiles the two together. This test CANNOT see that map. What it
// pins is the TypeScript half alone: that the hand-copy answers every member
// of both vocabularies the way it currently claims to, so an edit to the copy
// reds here in both directions. A kind added to the GO set with no edit here
// is still a silent portal that never offers the switch -- but nothing in this
// file notices, and a green frontend suite is not evidence about it.
//
// The guard on that seam lives on the Go side, and since issue #87 it reads
// THIS file: internal/routing.TestLiveTimingsCapableKindsMatchPortalHandCopy
// parses the liveTimingsCapableKinds Set in liveTimings.ts and asserts it is
// exactly the Go map, so a kind added to (or removed from) either set with no
// matching edit fails there. Its companion size pin,
// TestLiveTimingsCapableKindsSizeIsPinned, additionally breadcrumbs the docs,
// i18n strings and type comments that state the membership in prose only. Those
// are the tests to point an editor at, not this one.
//
// Each list is the key set of a Record over its union, which guards BOTH
// directions at compile time. REMOVING a member from ApplicationType or from
// RuntimeSpec['type'] leaves an unknown key here, and ADDING one leaves a
// required key missing; either fails tsc. A plain typed array guards only the
// first direction: a new member is simply absent, tsc stays quiet and every
// case still passes, so the list silently stops being exhaustive.
const applicationTypeKeys: Record<ApplicationType, true> = {
  ollama: true,
  vllm: true,
  llama_cpp: true,
  llama_swap: true,
  litellm: true,
  server_agent: true,
  stable_diffusion_cpp: true,
};
const allApplicationTypes = Object.keys(applicationTypeKeys) as ApplicationType[];

const specTypeKeys: Record<RuntimeSpec['type'], true> = {
  '': true,
  vllm: true,
  llama_cpp: true,
  tgi: true,
  ollama: true,
  stable_diffusion_cpp: true,
  custom: true,
};
const allSpecTypes = Object.keys(specTypeKeys) as RuntimeSpec['type'][];

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
  // own Type select is already the signal. The hazard is not a blank -- the
  // shared control renders this state perfectly well -- it is that the
  // sentence it renders tells the operator the switch lives on the launch
  // spec, which on the launch-spec form itself points them back at the form
  // they are already looking at.
  it('never answers delegated, for any spec type', () => {
    for (const specType of allSpecTypes) {
      expect(runtimeSpecLiveTimingsKind(specType), specType).not.toBe('delegated');
    }
  });
});

describe('applicationSendsLiveTimings', () => {
  // Only a capable kind sends the key; every other kind omits it. Preserves the
  // previous `=== 'capable'` exactly, exhaustively (issue #88).
  const expected: Record<LiveTimingsKind, boolean> = {
    capable: true,
    incapable: false,
    delegated: false,
    unknown: false,
  };
  for (const kind of allLiveTimingsKinds) {
    it(`${kind} -> ${expected[kind]}`, () => {
      expect(applicationSendsLiveTimings(kind)).toBe(expected[kind]);
    });
  }
});

describe('runtimeSpecSendsLiveTimings', () => {
  // Everything but incapable sends (the caller still gates on value !== undefined).
  // Preserves the previous `!== 'incapable'` exactly, exhaustively (issue #88).
  const expected: Record<LiveTimingsKind, boolean> = {
    capable: true,
    unknown: true,
    delegated: true,
    incapable: false,
  };
  for (const kind of allLiveTimingsKinds) {
    it(`${kind} -> ${expected[kind]}`, () => {
      expect(runtimeSpecSendsLiveTimings(kind)).toBe(expected[kind]);
    });
  }
});

describe('liveTimingsControlLayout', () => {
  // The shared control's rendering per kind: the checkbox (with its no-opinion
  // default and caption) for a settable kind, or a suppressed note otherwise.
  // Pins the mapping the plain-comparison suppression got wrong (issue #88).
  const expected: Record<LiveTimingsKind, ReturnType<typeof liveTimingsControlLayout>> = {
    capable: { checkbox: true, defaultChecked: true, note: 'default' },
    unknown: { checkbox: true, defaultChecked: false, note: 'auto' },
    incapable: { checkbox: false, note: 'unsupported' },
    delegated: { checkbox: false, note: 'delegated' },
  };
  for (const kind of allLiveTimingsKinds) {
    it(`${kind} -> ${JSON.stringify(expected[kind])}`, () => {
      expect(liveTimingsControlLayout(kind)).toEqual(expected[kind]);
    });
  }

  it('never shows the delegated note for a suppressed incapable kind (the #88 bug direction)', () => {
    const incapable = liveTimingsControlLayout('incapable');
    expect(incapable.checkbox).toBe(false);
    expect(incapable).not.toHaveProperty('defaultChecked');
    if (!incapable.checkbox) expect(incapable.note).toBe('unsupported');
  });
});
