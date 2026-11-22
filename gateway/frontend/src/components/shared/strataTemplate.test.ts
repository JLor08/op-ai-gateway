// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { strataSpecTemplate } from './strataTemplate';

describe('strataSpecTemplate', () => {
  it('is the Strata launch template, bound to loopback behind the agent', () => {
    expect(strataSpecTemplate()).toEqual({
      binaryHint: '%LOCALAPPDATA%\\Strata\\terminal\\current\\bin\\strata.cmd',
      args: [
        'serve',
        '--config',
        '${CONFIG_DIR}/strata.json',
        '--host',
        '127.0.0.1',
        '--port',
        '${PORT}',
        '--gpu',
        '${HOST_GPU_IDS_MAIN_LAST}',
        '--output',
        'jsonl',
        '--shutdown-timeout',
        '3',
      ],
      env: { STRATA_API_KEY: '${API_TOKEN}' },
      setVisibleDevices: false,
      startupTimeoutSeconds: 960,
      healthPath: '/health',
      flavors: ['openai', 'anthropic'],
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
    });
  });

  it('returns a fresh value on every call', () => {
    const first = strataSpecTemplate();
    first.args.push('--lazy');
    first.env.EXTRA = 'x';
    expect(strataSpecTemplate().args).not.toContain('--lazy');
    expect(strataSpecTemplate().env).toEqual({ STRATA_API_KEY: '${API_TOKEN}' });
  });
});
