// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { SpeedHistogram } from './SpeedHistogram';
import { messages } from '../i18n';
import type { Histogram } from '../api';

const t = messages.de;

const filled: Histogram = {
  bins: [
    { x0: 0, x1: 10, count: 2 },
    { x0: 10, x1: 20, count: 5 },
    { x0: 20, x1: 30, count: 1 },
  ],
  min: 0,
  max: 30,
  bin_size: 10,
  p50: 15,
  p95: 28,
  p99: 29,
};

const empty: Histogram = {
  bins: [],
  min: 0,
  max: 0,
  bin_size: 0,
  p50: 0,
  p95: 0,
  p99: 0,
};

afterEach(cleanup);

describe('SpeedHistogram', () => {
  it('draws a bar per bin, three percentile markers, and the title', () => {
    const { container } = render(
      <SpeedHistogram
        t={t}
        title={t.activityPromptSpeed}
        histogram={filled}
        unit={t.activityPromptSpeedUnit}
      />,
    );
    expect(screen.getByText(t.activityPromptSpeed)).toBeInTheDocument();
    expect(container.querySelectorAll('[data-bin]')).toHaveLength(3);
    expect(container.querySelector('[data-marker="p50"]')).toBeInTheDocument();
    expect(container.querySelector('[data-marker="p95"]')).toBeInTheDocument();
    expect(container.querySelector('[data-marker="p99"]')).toBeInTheDocument();
    // graphic is labelled for assistive tech
    expect(screen.getByRole('img', { name: t.activityPromptSpeed })).toBeInTheDocument();
  });

  it('shows the no-data placeholder and no bars for an empty distribution', () => {
    const { container } = render(
      <SpeedHistogram
        t={t}
        title={t.activityTokenSpeed}
        histogram={empty}
        unit={t.activityTokenSpeedUnit}
      />,
    );
    expect(screen.getByText(t.activityNoData)).toBeInTheDocument();
    expect(container.querySelectorAll('[data-bin]')).toHaveLength(0);
    expect(container.querySelector('[data-marker]')).toBeNull();
  });

  // Defensive: a nil Go slice serializes to bins:null. The component must coalesce
  // and render the no-data placeholder instead of throwing on null.length.
  it('renders the no-data placeholder when bins is null (no throw)', () => {
    const nullBins = { ...empty, bins: null } as unknown as Histogram;
    const { container } = render(
      <SpeedHistogram
        t={t}
        title={t.activityTokenSpeed}
        histogram={nullBins}
        unit={t.activityTokenSpeedUnit}
      />,
    );
    expect(screen.getByText(t.activityNoData)).toBeInTheDocument();
    expect(container.querySelectorAll('[data-bin]')).toHaveLength(0);
  });

  it('renders x-axis min/max labels and the y-axis maxCount tick', () => {
    render(
      <SpeedHistogram
        t={t}
        title={t.activityPromptSpeed}
        histogram={filled}
        unit={t.activityPromptSpeedUnit}
      />,
    );
    // X-axis ends: histogram.min / histogram.max at 1 decimal.
    expect(screen.getByText('0.0')).toBeInTheDocument();
    expect(screen.getByText('30.0')).toBeInTheDocument();
    // Y-axis top tick is the max bin count (integer, never fractional).
    const maxCount = Math.max(...filled.bins.map((b) => b.count));
    expect(maxCount).toBe(5);
    expect(screen.getByText(String(maxCount))).toBeInTheDocument();
  });

  it('shows a per-bar tooltip with the bin range and count on hover', async () => {
    const { container } = render(
      <SpeedHistogram
        t={t}
        title={t.activityPromptSpeed}
        histogram={filled}
        unit={t.activityPromptSpeedUnit}
      />,
    );
    // Second bin: x0=10, x1=20, count=5.
    const bar = container.querySelector('[data-bin="1"]') as SVGRectElement;
    expect(bar).not.toBeNull();
    fireEvent.mouseOver(bar);
    fireEvent.mouseMove(bar, { clientX: 20, clientY: 20 });
    // MUI Tooltip renders into a portal; wait for it to open.
    expect(await screen.findByText(`10.0 – 20.0 ${t.activityPromptSpeedUnit}`)).toBeInTheDocument();
    expect(screen.getByText(`${t.activityCount}: 5`)).toBeInTheDocument();
  });
});
