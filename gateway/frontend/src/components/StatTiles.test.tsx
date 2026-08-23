// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StatTiles, formatEnergyWh } from './StatTiles';
import { SettingsMenu } from './SettingsMenu';
import {
  DEFAULT_TILE_ORDER,
  DEFAULT_HIDDEN_TILES,
  ACTIVITY_TILES,
  type TileId,
} from './activityTiles';
import { formatCost } from '../currency';
import { messages } from '../i18n';
import type { StatTotals } from '../api';

const t = messages.de;
// Default props threaded to every render() call below unless a test overrides them.
// hidden=[] shows every tile so the base label/count assertions stay meaningful;
// individual tests override `hidden`/`order` to exercise the new behaviour.
const defaultCostProps = { costUnit: 'eur_cent' as const, currencyFactor: 1 };
const showAll = { order: DEFAULT_TILE_ORDER, hidden: [] as TileId[] };

const totals: StatTotals = {
  total_requests: 42,
  error_count: 3,
  cached_tokens: 111,
  cache_write_tokens: 55,
  input_tokens: 250,
  output_tokens: 900,
};

const totalsWithEnergy: StatTotals = {
  ...totals,
  total_energy_wh: 1500,
  total_cost_eur: 0.5,
};

afterEach(cleanup);

describe('StatTiles', () => {
  it('renders all labelled totals when nothing is hidden', () => {
    render(<StatTiles t={t} totals={totals} {...showAll} {...defaultCostProps} />);
    for (const label of [
      t.activityTotalRequests,
      t.activityErrorCount,
      t.activityCachedTokens,
      t.activityCacheWriteTokens,
      t.activityInputTokens,
      t.activityOutputTokens,
    ]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
    expect(screen.getByText('55')).toBeInTheDocument();
    expect(screen.getByText('111')).toBeInTheDocument();
    expect(screen.getByText('250')).toBeInTheDocument();
    expect(screen.getByText('900')).toBeInTheDocument();
  });

  it('renders exactly eight tiles and no running-connections tile without runningCount', () => {
    render(<StatTiles t={t} totals={totals} {...showAll} {...defaultCostProps} />);
    expect(screen.getAllByRole('article')).toHaveLength(8);
    expect(screen.queryByText(t.activityActiveTitle)).not.toBeInTheDocument();
  });

  it('prepends a leading running-connections tile when runningCount is provided', () => {
    render(<StatTiles t={t} totals={totals} runningCount={4} {...showAll} {...defaultCostProps} />);
    const tiles = screen.getAllByRole('article');
    expect(tiles).toHaveLength(9);
    // The running-connections tile leads the row (first in DEFAULT_TILE_ORDER).
    expect(within(tiles[0]).getByText(t.activityActiveTitle)).toBeInTheDocument();
    expect(within(tiles[0]).getByText('4')).toBeInTheDocument();
  });

  it('renders the running-connections tile even at a zero count', () => {
    render(<StatTiles t={t} totals={totals} runningCount={0} {...showAll} {...defaultCostProps} />);
    const tiles = screen.getAllByRole('article');
    expect(tiles).toHaveLength(9);
    expect(within(tiles[0]).getByText(t.activityActiveTitle)).toBeInTheDocument();
    expect(within(tiles[0]).getByText('0')).toBeInTheDocument();
  });

  describe('visibility (default-hidden Cache-Write) + order', () => {
    it('hides the cache_write_tokens tile with the default hidden set', () => {
      render(
        <StatTiles
          t={t}
          totals={totals}
          order={DEFAULT_TILE_ORDER}
          hidden={DEFAULT_HIDDEN_TILES}
          {...defaultCostProps}
        />,
      );
      // Cache-Write is the only default-hidden tile.
      expect(screen.queryByText(t.activityCacheWriteTokens)).not.toBeInTheDocument();
      // The other totals still render.
      expect(screen.getByText(t.activityTotalRequests)).toBeInTheDocument();
      expect(screen.getByText(t.activityInputTokens)).toBeInTheDocument();
      // No runningCount -> running skipped; 8 catalogue tiles minus cache_write = 7.
      expect(screen.getAllByRole('article')).toHaveLength(7);
    });

    it('renders the cache_write_tokens tile once removed from hidden', () => {
      render(
        <StatTiles
          t={t}
          totals={totals}
          order={DEFAULT_TILE_ORDER}
          hidden={[]}
          {...defaultCostProps}
        />,
      );
      expect(screen.getByText(t.activityCacheWriteTokens)).toBeInTheDocument();
      expect(screen.getByText('55')).toBeInTheDocument();
    });

    it('respects the given tile order', () => {
      // Put cost first, then input, then total_requests; the rest hidden.
      render(
        <StatTiles
          t={t}
          totals={totalsWithEnergy}
          order={['cost', 'input_tokens', 'total_requests'] as TileId[]}
          hidden={[] as TileId[]}
          {...defaultCostProps}
        />,
      );
      const tiles = screen.getAllByRole('article');
      expect(tiles).toHaveLength(3);
      expect(within(tiles[0]).getByText(t.activityCostTile)).toBeInTheDocument();
      expect(within(tiles[1]).getByText(t.activityInputTokens)).toBeInTheDocument();
      expect(within(tiles[2]).getByText(t.activityTotalRequests)).toBeInTheDocument();
    });

    it('uses a responsive auto-fit grid', () => {
      render(<StatTiles t={t} totals={totals} {...showAll} {...defaultCostProps} />);
      const section = screen.getByLabelText(t.activityStatsLabel);
      expect(section).toHaveStyle({ gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))' });
    });
  });

  describe('energy + cost tiles (P3 T2 / currency-unit selector T5)', () => {
    it('renders a total-energy tile in Wh and a total-cost tile via formatCost', () => {
      render(<StatTiles t={t} totals={totalsWithEnergy} {...showAll} {...defaultCostProps} />);
      expect(screen.getByText(t.activityEnergyTile)).toBeInTheDocument();
      expect(screen.getByText(t.activityCostTile)).toBeInTheDocument();
      expect(screen.getByText(formatEnergyWh(1500))).toBeInTheDocument();
      expect(screen.getByText(formatCost(0.5, 'eur_cent', 1))).toBeInTheDocument();
    });

    it('renders the cost tile in the given unit/factor (e.g. eur)', () => {
      render(
        <StatTiles
          t={t}
          totals={totalsWithEnergy}
          {...showAll}
          costUnit="eur"
          currencyFactor={1}
        />,
      );
      expect(screen.getByText('€ 0.5000')).toBeInTheDocument();
      expect(screen.getByText(formatCost(0.5, 'eur', 1))).toBeInTheDocument();
    });

    it('formats energy in kWh once at or above 1000 Wh', () => {
      expect(formatEnergyWh(999)).toBe('999.0 Wh');
      expect(formatEnergyWh(1000)).toBe('1.00 kWh');
      expect(formatEnergyWh(2500)).toBe('2.50 kWh');
    });

    it('defaults the energy tile to zero and the cost tile to an em dash when the totals omit them', () => {
      render(<StatTiles t={t} totals={totals} {...showAll} {...defaultCostProps} />);
      expect(screen.getByText(formatEnergyWh(0))).toBeInTheDocument();
      // total_cost_eur is undefined -> formatCost renders "—" (0/undefined convention).
      expect(screen.getByText('—')).toBeInTheDocument();
    });
  });
});

// The reconcileHiddenTiles crash-safety guard that used to live in
// activityTiles.ts is now the generic reconcileHiddenIds in
// shared/useColumnSettings.ts (see useColumnSettings.test.tsx for its coverage).

describe('SettingsMenu', () => {
  const items = ACTIVITY_TILES.map((x) => ({
    id: x.id,
    label: t[x.labelKey as keyof typeof t] as string,
  }));
  const menuProps = {
    items,
    hidden: [] as string[],
    order: DEFAULT_TILE_ORDER as string[],
    buttonLabel: t.activityTilesButton,
    title: t.activityTilesTitle,
    resetLabel: t.listColumnsReset,
    moveLeftLabel: t.listColumnMoveLeft,
    moveRightLabel: t.listColumnMoveRight,
  };

  it('toggles an item and resets via the menu', () => {
    const onToggle = vi.fn();
    const onReorder = vi.fn();
    const onReset = vi.fn();
    render(
      <SettingsMenu {...menuProps} onToggle={onToggle} onReorder={onReorder} onReset={onReset} />,
    );

    // Menu is closed until the trigger button is clicked.
    expect(screen.queryByText(t.activityTilesTitle)).not.toBeInTheDocument();
    fireEvent.click(screen.getByLabelText(t.activityTilesButton));
    expect(screen.getByText(t.activityTilesTitle)).toBeInTheDocument();

    // Toggling the Cache-Write checkbox calls onToggle with its id.
    fireEvent.click(screen.getByLabelText(t.activityCacheWriteTokens));
    expect(onToggle).toHaveBeenCalledWith('cache_write_tokens');

    fireEvent.click(screen.getByText(t.listColumnsReset));
    expect(onReset).toHaveBeenCalledTimes(1);
  });
});
