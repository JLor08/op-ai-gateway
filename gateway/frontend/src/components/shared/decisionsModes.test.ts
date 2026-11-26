// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  decisionsModeForKind,
  decisionsPassthroughOffered,
  systemOneTranslateOffered,
} from './decisionsModes';

describe('decisions mode predicates', () => {
  it.each(['llama_cpp', 'vllm_sr', 'strata'])(
    'refuses passthrough and translate for %s',
    (kind) => {
      expect(decisionsPassthroughOffered(kind)).toBe(false);
      expect(systemOneTranslateOffered(kind)).toBe(false);
      expect(decisionsModeForKind('passthrough', kind)).toBe('auto');
    },
  );

  it.each(['vllm', 'custom', 'server_agent', 'sglang', ''])('offers both for %s', (kind) => {
    expect(decisionsPassthroughOffered(kind)).toBe(true);
    expect(systemOneTranslateOffered(kind)).toBe(true);
    expect(decisionsModeForKind('passthrough', kind)).toBe('passthrough');
  });
});
