// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, within } from '@testing-library/react';
import { Dashboard } from './Dashboard';
import { messages, type Locale } from '../i18n';
import type { DashboardResponse } from '../api';

afterEach(() => cleanup());

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`Dashboard live routes empty/loading label [${locale}]`, () => {
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

  describe(`Dashboard 24h token tile: three-state rule (#70) [${locale}]`, () => {
    const hosts = { healthy_hosts: '1/1', latency_p95_ms: 12 };
    // StatTile renders its card as <Paper component="article">, so the label's
    // closest <article> is exactly one tile.
    const tokenTile = () => screen.getByText(t.tokens24h).closest('article') as HTMLElement;

    it('dashes the tile, with its reason, when nothing in the window is token-metered', async () => {
      const dashboard: DashboardResponse = {
        metrics: { ...hosts, requests_24h: 10, tokens_24h: 0, non_token_requests_24h: 10 },
        routes: [],
      };
      render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
      expect(within(tokenTile()).getByText('—')).toBeInTheDocument();
      fireEvent.mouseOver(within(tokenTile()).getByText('—'));
      expect(await screen.findByText(t.activityNotTokenMetered)).toBeInTheDocument();
    });

    it('marks the tile and names the excluded count when the window is mixed', async () => {
      const dashboard: DashboardResponse = {
        metrics: { ...hosts, requests_24h: 10, tokens_24h: 300, non_token_requests_24h: 4 },
        routes: [],
      };
      render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
      // The sum is correct for the token-metered subset only, and the tooltip
      // says so -- a bare 300 would claim it covered all ten requests.
      expect(within(tokenTile()).getByText('300*')).toBeInTheDocument();
      fireEvent.mouseOver(within(tokenTile()).getByText('300*'));
      expect(await screen.findByText(t.activityMixedUnitsHint(4))).toBeInTheDocument();
    });

    it('prints a plain number when the whole window is token-metered', () => {
      const dashboard: DashboardResponse = {
        metrics: { ...hosts, requests_24h: 10, tokens_24h: 300, non_token_requests_24h: 0 },
        routes: [],
      };
      render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
      expect(within(tokenTile()).getByText('300')).toBeInTheDocument();
    });
  });
}
