// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { usageChipStatus } from './status';

describe('usageChipStatus', () => {
  it('maps a clean success', () => {
    expect(usageChipStatus('success', 200)).toBe('success');
  });

  it('maps an explicit error status', () => {
    expect(usageChipStatus('error', 500)).toBe('error');
  });

  it('treats a mid-stream failure (status=error, http_status=200) as error', () => {
    expect(usageChipStatus('error', 200)).toBe('error');
  });

  it('treats an http>=400 with success status as error', () => {
    expect(usageChipStatus('success', 503)).toBe('error');
  });

  it('falls back to standby for unexpected input', () => {
    expect(usageChipStatus('', 0)).toBe('standby');
    expect(usageChipStatus('weird', 0)).toBe('standby');
  });
});
