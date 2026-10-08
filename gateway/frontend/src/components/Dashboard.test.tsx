// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
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

  describe(`Dashboard live routes: vendor-account models [${locale}]`, () => {
    const metrics = { requests_24h: 0, tokens_24h: 0, healthy_hosts: '1/1', latency_p95_ms: 0 };
    // The shapes the backend's dashboardRouteData sends: a self-hosted route
    // (mapping id, application type, server name) next to the principal's own
    // vendor-account model (vendor, account name, the PREFIXED gateway model).
    const selfHosted = {
      id: 'map_1',
      model: 'qwen-coder',
      provider: 'vllm',
      host: 'Server One',
      status: 'active',
    } as const;
    const vendorModel = {
      id: 'va_1:chatgpt/gpt-6-luna',
      model: 'chatgpt/gpt-6-luna',
      provider: 'openai',
      host: 'My ChatGPT',
      status: 'active',
    } as const;

    it('lists a vendor-account model under its prefixed name, next to the self-hosted routes', () => {
      const dashboard: DashboardResponse = { metrics, routes: [selfHosted, vendorModel] };
      render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
      const routesTable = screen.getByRole('table');
      // The vendor model is a row of its own: prefixed name, vendor, account.
      const vendorRow = within(routesTable).getByText('chatgpt/gpt-6-luna').closest('tr');
      expect(vendorRow).not.toBeNull();
      expect(within(vendorRow as HTMLElement).getByText('openai')).toBeInTheDocument();
      expect(within(vendorRow as HTMLElement).getByText('My ChatGPT')).toBeInTheDocument();
      expect(
        within(vendorRow as HTMLElement).getByText(messages[locale].statusActive),
      ).toBeInTheDocument();
      // ... and the self-hosted route is still there, the bare slug is not.
      expect(within(routesTable).getByText('qwen-coder')).toBeInTheDocument();
      expect(within(routesTable).queryByText('gpt-6-luna')).toBeNull();
      // Neither the empty nor the loading label shows once there are rows.
      expect(within(routesTable).queryByText(t.modelsEmpty)).toBeNull();
    });

    it('shows a vendor-account model even when no self-hosted route exists', () => {
      const dashboard: DashboardResponse = { metrics, routes: [vendorModel] };
      render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
      const routesTable = screen.getByRole('table');
      expect(within(routesTable).getByText('chatgpt/gpt-6-luna')).toBeInTheDocument();
      expect(within(routesTable).queryByText(t.modelsEmpty)).toBeNull();
    });

    it('renders two accounts that share a name and a model as two distinct rows', () => {
      // Row identity is the route id, not model+host: two accounts both called
      // "ChatGPT" serving the same model must not collide on a React key.
      const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
      try {
        const twin = (id: string) => ({ ...vendorModel, id, host: 'ChatGPT' });
        const dashboard: DashboardResponse = {
          metrics,
          routes: [twin('va_1:chatgpt/gpt-6-luna'), twin('va_2:chatgpt/gpt-6-luna')],
        };
        render(<Dashboard t={t} dashboard={dashboard} productName="X" />);
        const routesTable = screen.getByRole('table');
        expect(within(routesTable).getAllByText('chatgpt/gpt-6-luna')).toHaveLength(2);
        expect(errors).not.toHaveBeenCalled();
      } finally {
        errors.mockRestore();
      }
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
