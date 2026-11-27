// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { strataSpecTemplate } from './strataTemplate';

const declared = { declareEngineLog: true };

describe('strataSpecTemplate', () => {
  it('is the Strata launch template, bound to loopback behind the agent', () => {
    expect(strataSpecTemplate(declared)).toEqual({
      binaryHint: '%LOCALAPPDATA%\\Strata\\terminal\\current\\bin\\strata.cmd',
      args: [
        'serve',
        '--config',
        '${CONFIG_DIR}/strata.json',
        '--host',
        '127.0.0.1',
        '--port',
        '${PORT}',
        '--output',
        'jsonl',
        '--shutdown-timeout',
        '3',
      ],
      env: { STRATA_API_KEY: '${API_TOKEN}' },
      setVisibleDevices: true,
      visibleDevicesMode: 'args',
      startupTimeoutSeconds: 960,
      healthPath: '/health',
      flavors: ['openai', 'anthropic'],
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      processTree: 'job',
      logFiles: [{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }],
    });
  });

  it('returns a fresh value on every call', () => {
    const first = strataSpecTemplate(declared);
    first.args.push('--lazy');
    first.env.EXTRA = 'x';
    expect(strataSpecTemplate(declared).args).not.toContain('--lazy');
    expect(strataSpecTemplate(declared).env).toEqual({ STRATA_API_KEY: '${API_TOKEN}' });
  });
  it('returns a fresh logFiles list on every call', () => {
    strataSpecTemplate(declared).logFiles.push({ name: 'x', path: '/x' });
    expect(strataSpecTemplate(declared).logFiles).toHaveLength(1);
  });
  it('declares no engine log when the agent lacks runtime_log_files', () => {
    const template = strataSpecTemplate({ declareEngineLog: false });
    expect(template.logFiles).toEqual([]);
    expect({ ...template, logFiles: undefined }).toEqual({
      ...strataSpecTemplate(declared),
      logFiles: undefined,
    });
  });
  it('passes no --gpu flag: strata serve has none, the card list is in the config file', () => {
    expect(strataSpecTemplate(declared).args).not.toContain('--gpu');
  });
});
