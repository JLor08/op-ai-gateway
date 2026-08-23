// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { UptimeTimeline, type UptimeSegment, type TimelineState } from './UptimeTimeline';
import { messages } from '../i18n';

const t = messages.de;
const color = () => '#000';
const label = () => 'x';

describe('UptimeTimeline', () => {
  it('renders one rect per segment positioned within the window', () => {
    const from = '2026-01-01T00:00:00Z';
    const to = '2026-01-01T04:00:00Z';
    const segments: UptimeSegment[] = [
      { start: from, end: '2026-01-01T01:00:00Z', state: 'healthy' },
      { start: '2026-01-01T01:00:00Z', end: '2026-01-01T02:00:00Z', state: 'unhealthy' },
      { start: '2026-01-01T02:00:00Z', end: to, state: 'healthy' },
    ];
    const { container } = render(
      <UptimeTimeline
        t={t}
        title={t.availabilityHealthTimeline}
        segments={segments}
        windowFrom={from}
        windowTo={to}
        colorForState={color}
        labelForState={label}
      />,
    );
    const rects = Array.from(container.querySelectorAll('rect[data-segment]'));
    expect(rects).toHaveLength(3);
    expect(screen.getByText(t.availabilityHealthTimeline)).toBeInTheDocument();

    // Positioning: derived from the component's W=1000 over the 4h window, so each
    // hour maps to 1000 * (1/4) = 250px. Guards the x/width formulas (a mutation of
    // (a - from) -> (a - to) drives x off-window and fails these assertions).
    const expected = [
      { x: 0, w: 250 }, // 00:00–01:00
      { x: 250, w: 250 }, // 01:00–02:00
      { x: 500, w: 500 }, // 02:00–04:00 (two hours)
    ];
    rects.forEach((rect, i) => {
      expect(Number(rect.getAttribute('x'))).toBeCloseTo(expected[i].x, 0);
      expect(Number(rect.getAttribute('width'))).toBeCloseTo(expected[i].w, 0);
    });
  });

  it('renders an empty-safe timeline with no segments', () => {
    const { container } = render(
      <UptimeTimeline
        t={t}
        title="x"
        segments={[]}
        windowFrom="2026-01-01T00:00:00Z"
        windowTo="2026-01-01T01:00:00Z"
        colorForState={color}
        labelForState={label}
      />,
    );
    expect(container.querySelectorAll('rect[data-segment]')).toHaveLength(0);
  });

  it('renders connected/disconnected NetBird segments', () => {
    const from = '2026-01-01T00:00:00Z';
    const to = '2026-01-01T02:00:00Z';
    const segments: UptimeSegment[] = [
      { start: from, end: '2026-01-01T01:00:00Z', state: 'connected' },
      { start: '2026-01-01T01:00:00Z', end: to, state: 'disconnected' },
    ];
    const seen: TimelineState[] = [];
    const { container } = render(
      <UptimeTimeline
        t={t}
        title="nb"
        segments={segments}
        windowFrom={from}
        windowTo={to}
        colorForState={(s) => {
          seen.push(s);
          return s === 'connected' ? '#0a0' : '#a00';
        }}
        labelForState={(s) => (s === 'connected' ? 'up' : 'down')}
      />,
    );
    expect(container.querySelectorAll('rect[data-segment]')).toHaveLength(2);
    expect(seen).toContain('connected');
    expect(seen).toContain('disconnected');
  });
});
