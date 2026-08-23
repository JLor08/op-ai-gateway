// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, cleanup, within } from '@testing-library/react';
import { Dashboard } from './Dashboard';
import { messages } from '../i18n';
import type { DashboardResponse } from '../api';

const t = messages.de;

afterEach(() => cleanup());

describe('Dashboard live routes empty/loading label', () => {
  it('shows the loading label in the routes table before the dashboard has loaded', () => {
    render(<Dashboard t={t} dashboard={null} productName="X" />);
    // With no dashboard the metric tiles also render the loading placeholder, so
    // scope the assertion to the routes table region.
    const routesTable = screen.getByRole('table');
    expect(within(routesTable).getByText(t.loading)).toBeInTheDocument();
    expect(within(routesTable).queryByText(t.modelsEmpty)).toBeNull();
  });

  it('shows the empty label (not loading) once loaded with no routes', () => {
    const dashboard: DashboardResponse = {
      metrics: { requests_24h: 0, tokens_24h: 0, healthy_hosts: '0/0', latency_p95_ms: 0 },
      routes: [],
    };
    render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
    // The metric tiles are populated now, so t.loading must be gone from the page.
    const routesTable = screen.getByRole('table');
    expect(within(routesTable).getByText(t.modelsEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).toBeNull();
  });
});
