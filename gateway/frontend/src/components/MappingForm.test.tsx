// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MappingForm, type MappingFormValues } from './MappingForm';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { BenchmarkStatus, PortalModelMapping } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function makeMapping(overrides: Partial<PortalModelMapping> = {}): PortalModelMapping {
  return {
    id: 'map_1',
    application_id: 'app_1',
    gateway_model_name: 'gw-model',
    app_model_name: 'app-model',
    status: 'active',
    created_at: '2026-07-16T12:00:00Z',
    gen_tokens_per_second: 0,
    prompt_tokens_per_second: 0,
    load_time_ms: 0,
    context_size: 0,
    is_mtp: false,
    vision_capable: false,
    energy_wh_per_token: 0,
    metrics_locked: false,
    metrics_source: '',
    metrics_updated_at: null,
    max_concurrency: 0,
    recommended_concurrency: 0,
    gen_tokens_per_second_at_capacity: 0,
    ...overrides,
  };
}

const idle: BenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'application',
  total: 0,
  done: 0,
};

function renderForm(opts: { appNameReadOnly?: boolean } = {}) {
  const submitted: MappingFormValues[] = [];
  const api = {
    activeBenchmarks: vi.fn(async () => []),
    benchmarkStatus: vi.fn(async () => idle),
    probeMappingContext: vi.fn(async () => idle),
  } as unknown as Pick<PortalApi, 'activeBenchmarks' | 'benchmarkStatus' | 'probeMappingContext'>;
  render(
    <ToastProvider>
      <MappingForm
        t={t}
        api={api}
        serverId="srv_1"
        contextProbePath=""
        row={makeMapping()}
        appNameReadOnly={opts.appNameReadOnly}
        busy={false}
        onSubmit={(values) => submitted.push(values)}
        onCancel={() => {}}
        pollIntervalMs={0}
      />
    </ToastProvider>,
  );
  return { submitted };
}

afterEach(cleanup);

describe('MappingForm ownership boundary', () => {
  it('leaves both model names editable by default', () => {
    renderForm();
    // BEHAVIOURAL: an ordinary application has no runtime spec, so this screen
    // owns both names and neither field may be locked.
    expect(document.querySelector('#mapping-app-name')).not.toHaveAttribute('readonly');
    expect(document.querySelector('#mapping-gateway-name')).not.toHaveAttribute('readonly');
  });

  it('locks the application model name and cannot be typed into when the spec owns it', async () => {
    const { submitted } = renderForm({ appNameReadOnly: true });

    // STRUCTURAL: the attribute is the only thing a real browser honours, and
    // jsdom will happily fire a change event on a readOnly input regardless.
    const appField = document.querySelector('#mapping-app-name');
    expect(appField).toHaveAttribute('readonly');
    expect(screen.getByText(t.mappingAppNameReadOnly)).toBeInTheDocument();
    // ...and the gateway name stays editable: the MAPPING owns that one.
    expect(document.querySelector('#mapping-gateway-name')).not.toHaveAttribute('readonly');

    // BEHAVIOURAL, and the reason the read-only field carries a no-op onChange:
    // without it this fireEvent would still drive React state and the value
    // below would be the typed one, i.e. a write no real user can perform.
    fireEvent.change(appField!, { target: { value: 'typed-by-a-test' } });
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].app_model_name).toBe('app-model');
  });
});
