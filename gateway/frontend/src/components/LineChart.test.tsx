// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { LineChart } from './LineChart';
import { messages } from '../i18n';

const t = messages.de;

const times = ['12:00:00', '12:00:05', '12:00:10', '12:00:15'];
const twoSeries = [
  { label: 'Requests', color: 'var(--brand-primary)', values: [1, 3, 2, 4] },
  { label: 'Concurrent', color: 'var(--brand-accent)', values: [0, 1, 1, 2] },
];

// jsdom returns a zero-size rect for getBoundingClientRect; the overlay maps
// clientX -> bucket index by its own rect, so give it a deterministic geometry.
function stubOverlayRect(overlay: Element, width = 100) {
  overlay.getBoundingClientRect = () =>
    ({
      left: 0,
      top: 0,
      right: width,
      bottom: 100,
      width,
      height: 100,
      x: 0,
      y: 0,
      toJSON() {},
    }) as DOMRect;
}

afterEach(cleanup);

describe('LineChart', () => {
  it('shows the legend for a small multi-series chart but hides it above 12 series', () => {
    // 2 series -> legend present.
    const { container: few } = render(
      <LineChart t={t} title="Few" unit="%" series={twoSeries} times={times} />,
    );
    expect(few.querySelector('[data-testid="ts-legend"]')).toBeInTheDocument();
    cleanup();
    // 20 series (e.g. a 20-core CPU) -> legend suppressed, but all lines drawn.
    const many = Array.from({ length: 20 }, (_, i) => ({
      label: `Core ${i}`,
      color: '#123456',
      values: [1, 2, 3, 4],
    }));
    const { container: lots } = render(
      <LineChart t={t} title="Many" unit="%" series={many} times={times} />,
    );
    expect(lots.querySelector('[data-testid="ts-legend"]')).not.toBeInTheDocument();
    expect(lots.querySelectorAll('[data-series]')).toHaveLength(20);
  });

  it('draws one smooth path per series and the title', () => {
    const { container } = render(
      <LineChart t={t} title="Verbindungen" unit="req" series={twoSeries} times={times} />,
    );
    expect(screen.getByText('Verbindungen')).toBeInTheDocument();
    expect(container.querySelectorAll('[data-series]')).toHaveLength(2);
  });

  it('renders y-axis ticks (incl. the max) and x-axis time labels', () => {
    const { container } = render(
      <LineChart t={t} title="Verbindungen" unit="req" series={twoSeries} times={times} />,
    );
    // Data max across all series is 4 -> nice top tick 4.
    expect(container.querySelector('[data-ytick="4"]')).toBeInTheDocument();
    expect(container.querySelector('[data-ytick="0"]')).toBeInTheDocument();
    // First and last bucket times are labelled.
    expect(screen.getByText('12:00:00')).toBeInTheDocument();
    expect(screen.getByText('12:00:15')).toBeInTheDocument();
  });

  it('scales the y-domain above the data max so lines never clip out the top', () => {
    // Regression: a spiky max (90) previously produced a nice step of 50, whose
    // last multiple <= 90 is 50 -> the top tick was 50 < 90, so the peak mapped
    // above the plot and the line ran out the top. The top tick must cover 90.
    const { container } = render(
      <LineChart
        t={t}
        title="Spiky"
        unit="%"
        series={[{ label: 'GPU', color: 'var(--brand-primary)', values: [5, 90, 12, 30] }]}
        times={times}
      />,
    );
    const tickValues = Array.from(container.querySelectorAll('[data-ytick]')).map((el) =>
      parseFloat(el.getAttribute('data-ytick') ?? 'NaN'),
    );
    expect(tickValues.length).toBeGreaterThan(0);
    // The domain max is the largest tick; it must be >= the data max (90), which
    // guarantees yOf(90) >= plotTop (no line above the plot).
    expect(Math.max(...tickValues)).toBeGreaterThanOrEqual(90);
    expect(tickValues).toContain(0);
  });

  it('draws thin series lines', () => {
    const { container } = render(
      <LineChart t={t} title="Verbindungen" unit="req" series={twoSeries} times={times} />,
    );
    for (const path of Array.from(container.querySelectorAll('[data-series]'))) {
      expect(path.getAttribute('stroke-width')).toBe('1.5');
    }
  });

  it('renders a legend row for multi-series', () => {
    render(<LineChart t={t} title="Verbindungen" unit="req" series={twoSeries} times={times} />);
    const legend = screen.getByTestId('ts-legend');
    expect(legend).toHaveTextContent('Requests');
    expect(legend).toHaveTextContent('Concurrent');
  });

  it('omits the legend for a single series', () => {
    render(
      <LineChart
        t={t}
        title="Prompt"
        unit="tok/s"
        series={[{ label: 'Prompt', color: 'var(--brand-primary)', values: [1, 2, 3, 4] }]}
        times={times}
      />,
    );
    expect(screen.queryByTestId('ts-legend')).not.toBeInTheDocument();
  });

  it('shows the no-data placeholder when there are no time buckets', () => {
    const { container } = render(
      <LineChart t={t} title="Verbindungen" unit="req" series={[]} times={[]} />,
    );
    expect(screen.getByText(t.activityNoData)).toBeInTheDocument();
    expect(container.querySelectorAll('[data-series]')).toHaveLength(0);
  });

  it('shows the no-data placeholder when every value is zero across all series', () => {
    const { container } = render(
      <LineChart
        t={t}
        title="Verbindungen"
        unit="req"
        series={[{ label: 'Requests', color: 'var(--brand-primary)', values: [0, 0, 0, 0] }]}
        times={times}
      />,
    );
    expect(screen.getByText(t.activityNoData)).toBeInTheDocument();
    expect(container.querySelectorAll('[data-series]')).toHaveLength(0);
  });

  it('shows a hover tooltip with the bucket time and each series value, then clears on leave', async () => {
    const { container } = render(
      <LineChart t={t} title="Verbindungen" unit="req" series={twoSeries} times={times} />,
    );
    const overlay = container.querySelector('[data-testid="ts-overlay"]') as SVGRectElement;
    expect(overlay).not.toBeNull();
    stubOverlayRect(overlay, 100);

    // clientX at the far right -> last bucket (index 3): time 12:00:15, values 4 and 2.
    fireEvent.mouseMove(overlay, { clientX: 100 });
    const tooltip = await screen.findByTestId('ts-tooltip');
    expect(tooltip).toHaveTextContent('12:00:15');
    expect(tooltip).toHaveTextContent('Requests: 4 req');
    expect(tooltip).toHaveTextContent('Concurrent: 2 req');
    // A vertical guide + one dot per series mark the hovered bucket.
    expect(container.querySelector('[data-guide]')).toBeInTheDocument();
    expect(container.querySelectorAll('[data-dot]')).toHaveLength(2);

    fireEvent.mouseLeave(overlay);
    expect(screen.queryByTestId('ts-tooltip')).not.toBeInTheDocument();
    expect(container.querySelector('[data-guide]')).not.toBeInTheDocument();
  });

  it('maps the mouse x to the nearest bucket and formats fractional values', async () => {
    const { container } = render(
      <LineChart
        t={t}
        title="Prompt"
        unit="tok/s"
        series={[{ label: 'Prompt', color: 'var(--brand-primary)', values: [0, 4.5, 9, 1.25] }]}
        times={times}
      />,
    );
    const overlay = container.querySelector('[data-testid="ts-overlay"]') as SVGRectElement;
    stubOverlayRect(overlay, 90);
    // 30/90 = 0.333 -> round(0.333 * 3) = 1 -> second bucket (value 4.5).
    fireEvent.mouseMove(overlay, { clientX: 30 });
    const tooltip = await screen.findByTestId('ts-tooltip');
    expect(tooltip).toHaveTextContent('12:00:05');
    expect(tooltip).toHaveTextContent('Prompt: 4.5 tok/s');
  });

  it('sizes the tooltip to its content and keeps it inside the container near the right edge', async () => {
    // Long labels near the right edge used to hard-wrap into many lines because
    // the shrink-to-fit width collapsed to the tiny space left of the cursor.
    // `width: max-content` sizes to the natural one-line width instead, and the
    // continuous anchor sets transform = translateX(-left%) of the tooltip's OWN
    // width, so for any tooltip width <= container width the box stays in [0, W].
    const { container } = render(
      <LineChart
        t={t}
        title="GPU"
        unit="%"
        series={[{ label: 'GPU-0 VRAM belegt sehr lang', color: '#3b82f6', values: [1, 2, 3, 99] }]}
        times={times}
      />,
    );
    const overlay = container.querySelector('[data-testid="ts-overlay"]') as SVGRectElement;
    stubOverlayRect(overlay, 100);
    // Far right -> last bucket, hover fraction near the right edge.
    fireEvent.mouseMove(overlay, { clientX: 100 });
    const tooltip = (await screen.findByTestId('ts-tooltip')) as HTMLElement;

    // Sized to content (defeats the near-edge shrink-to-fit squeeze).
    expect(tooltip.style.width).toBe('max-content');

    // transform translateX% is exactly the negation of left% -> box within [0,W].
    const leftPct = parseFloat(tooltip.style.left);
    const m = tooltip.style.transform.match(/translateX\((-?[\d.]+)%\)/);
    expect(m).not.toBeNull();
    expect(parseFloat((m as RegExpMatchArray)[1])).toBeCloseTo(-leftPct, 5);
    // Near the right edge the tooltip is pulled left (negative translate).
    expect(leftPct).toBeGreaterThan(50);
  });

  it('flows the tooltip into columns for a many-series chart so EVERY core is shown', async () => {
    // 20 cores: a single tall column would clip under the height cap (and the
    // tooltip is pointer-events:none, so unscrollable) — the rows must flow into
    // additional columns instead.
    const cores = Array.from({ length: 20 }, (_, i) => ({
      label: `Core ${i}`,
      color: '#3b82f6',
      values: [i, i, i, i],
    }));
    const { container } = render(
      <LineChart t={t} title="CPU pro Core" unit="%" series={cores} times={times} />,
    );
    const overlay = container.querySelector('[data-testid="ts-overlay"]') as SVGRectElement;
    stubOverlayRect(overlay, 100);
    fireEvent.mouseMove(overlay, { clientX: 100 });

    const tooltip = await screen.findByTestId('ts-tooltip');
    // Every core's row is present (not clipped) — the far-right bucket is index 3.
    for (let i = 0; i < 20; i++) {
      expect(tooltip).toHaveTextContent(`Core ${i}: ${i} %`);
    }
    // The rows are laid out in a column-flowing grid (multi-column) rather than one
    // tall list.
    const grid = await screen.findByTestId('ts-tooltip-grid');
    expect(grid).toHaveStyle({ display: 'grid', gridAutoFlow: 'column' });
  });
});
