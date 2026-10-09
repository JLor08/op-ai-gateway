// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { messages, type Locale } from '../../i18n';
import { serverLabel, shortAccountId, type ServerLabelRow } from './vendorServerLabel';

const FULL_ID = 'va_0123456789abcdef0123456789abcdef';

function row(overrides: Partial<ServerLabelRow> = {}): ServerLabelRow {
  return { server_name: '', host: '', provider: 'vendor_openai', ...overrides };
}

describe('shortAccountId', () => {
  it('keeps the type prefix and the first 8 characters after it, marking the cut', () => {
    expect(shortAccountId(FULL_ID)).toBe('va_01234567…');
  });

  it('returns an id that already fits unchanged (no ellipsis)', () => {
    expect(shortAccountId('va_abc')).toBe('va_abc');
    expect(shortAccountId('va_01234567')).toBe('va_01234567');
  });

  it('returns "" for an empty or absent id', () => {
    expect(shortAccountId('')).toBe('');
    expect(shortAccountId(undefined)).toBe('');
  });
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`serverLabel [${locale}]`, () => {
    it('passes a self-hosted server_name through unchanged, with no title', () => {
      expect(serverLabel(t, row({ server_name: 'GPU 1', provider: 'llama_cpp' }))).toEqual({
        text: 'GPU 1',
      });
    });

    it('falls back to host when server_name is empty', () => {
      expect(serverLabel(t, row({ host: 'gpu-host', provider: 'llama_cpp' }))).toEqual({
        text: 'gpu-host',
      });
    });

    it('prefers server_name/host even when the provider happens to be a vendor one', () => {
      expect(
        serverLabel(
          t,
          row({ server_name: 'GPU 1', provider: 'vendor_openai', account_name: 'Work' }),
        ),
      ).toEqual({ text: 'GPU 1' });
    });

    it('shows "<OpenAI> · <account name>" for an OpenAI api-key vendor row', () => {
      expect(serverLabel(t, row({ account_id: FULL_ID, account_name: 'Work key' }))).toEqual({
        text: `${t.vendorOpenAI} · Work key`,
      });
    });

    it('maps the OpenAI subscription provider to the OpenAI vendor', () => {
      expect(
        serverLabel(
          t,
          row({
            provider: 'vendor_openai_subscription',
            account_id: FULL_ID,
            account_name: 'Team seat',
          }),
        ),
      ).toEqual({ text: `${t.vendorOpenAI} · Team seat` });
    });

    it('maps vendor_anthropic to the Anthropic vendor', () => {
      expect(
        serverLabel(t, row({ provider: 'vendor_anthropic', account_name: 'Claude key' })),
      ).toEqual({ text: `${t.vendorAnthropic} · Claude key` });
    });

    it('falls back to the short account id plus the full id as title when the name is empty', () => {
      expect(serverLabel(t, row({ account_id: FULL_ID, account_name: '' }))).toEqual({
        text: `${t.vendorOpenAI} · va_01234567…`,
        title: FULL_ID,
      });
      // An absent account_name (the field is omitted for a non-owner) behaves the same.
      expect(serverLabel(t, row({ account_id: FULL_ID }))).toEqual({
        text: `${t.vendorOpenAI} · va_01234567…`,
        title: FULL_ID,
      });
    });

    it('treats a whitespace-only account name as empty', () => {
      expect(serverLabel(t, row({ account_id: FULL_ID, account_name: '   ' }))).toEqual({
        text: `${t.vendorOpenAI} · va_01234567…`,
        title: FULL_ID,
      });
    });

    it('shows just the vendor label when the row carries neither a name nor an id', () => {
      expect(serverLabel(t, row())).toEqual({ text: t.vendorOpenAI });
    });

    it('shows an unknown vendor kind as sent (vendorLabel passthrough)', () => {
      expect(serverLabel(t, row({ provider: 'vendor_acme', account_name: 'X' }))).toEqual({
        text: 'acme · X',
      });
    });

    it('returns "" for a non-vendor row with no server_name/host', () => {
      expect(serverLabel(t, row({ provider: 'llama_cpp' }))).toEqual({ text: '' });
      expect(serverLabel(t, row({ provider: '' }))).toEqual({ text: '' });
    });
  });
}
