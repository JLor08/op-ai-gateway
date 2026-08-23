// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useMemo, useState, type MouseEvent } from 'react';
import { Box, Paper, Typography } from '@mui/material';
import type { Translation } from './shared/types';

export type LineSeries = { label: string; color: string; values: number[] };

// Upper bound on the number of points actually drawn. Long windows at fine
// resolutions can produce thousands of buckets (the backend coarsens the worst
// cases, but a 1y/1h window is still ~8760 points); rendering every one as a
// Bézier segment janks. We group into at most this many equal spans and average
// each series within a group — the visual trend is preserved without the cost.
const MAX_POINTS = 200;

/**
 * Downsample parallel `times`/`series` arrays to at most `maxPoints` points by
 * splitting the range into that many contiguous groups and averaging each
 * series within a group; the group's first bucket time labels it. A no-op when
 * already within the cap. Pure and exported for unit testing.
 */
export function downsample(
  times: string[],
  series: LineSeries[],
  maxPoints: number = MAX_POINTS,
): { times: string[]; series: LineSeries[] } {
  const n = times.length;
  if (n <= maxPoints || maxPoints <= 0) return { times, series };
  const outTimes: string[] = [];
  const outValues: number[][] = series.map(() => []);
  for (let g = 0; g < maxPoints; g++) {
    const lo = Math.floor((g * n) / maxPoints);
    const hi = Math.max(lo + 1, Math.floor(((g + 1) * n) / maxPoints));
    outTimes.push(times[lo]);
    series.forEach((s, si) => {
      let sum = 0;
      let cnt = 0;
      for (let i = lo; i < hi && i < n; i++) {
        sum += s.values[i] ?? 0;
        cnt++;
      }
      outValues[si].push(cnt > 0 ? sum / cnt : 0);
    });
  }
  return { times: outTimes, series: series.map((s, si) => ({ ...s, values: outValues[si] })) };
}

// Geometry (viewBox units). The SVG is responsive (width:100%), so these are
// intrinsic units, not pixels. Left/bottom margins leave room for the axis
// labels; the plot area is the inner rectangle.
const W = 320;
const H = 168;
const ML = 34; // left margin (y-axis tick labels)
const MR = 10; // right pad
const MT = 10; // top pad
const MB = 20; // bottom margin (x-axis labels)
// Legend band height (viewBox units) reserved at the top of the plot when there
// is more than one series. Because the legend lives INSIDE the SVG, every chart's
// viewBox (and therefore its rendered height) is identical — a legend simply
// shrinks that chart's plot area instead of making the whole card taller.
const LEGEND_H = 14;
// Above this many series an in-chart legend is noise (e.g. a 128-core CPU) and
// would overflow the fixed legend band — suppress it and let the lines speak.
const MAX_LEGEND_SERIES = 12;
// Cap the rows per tooltip column: beyond this the per-series rows flow into
// additional COLUMNS (grid-auto-flow: column) so a many-series chart (e.g. a
// per-core CPU graph) shows every core instead of clipping/scrolling a single
// tall column the pointer-events:none tooltip can't scroll.
const TOOLTIP_MAX_ROWS = 8;
const innerW = W - ML - MR;
const plotLeft = ML;
const plotRight = W - MR;

// Compact numeric label: round to 2 decimals and drop trailing zeros so counts
// render as integers ("4") and rates keep just enough precision ("4.5").
function fmtNum(v: number): string {
  return String(Math.round(v * 100) / 100);
}

/** The x-axis label at the first/last index is anchored outward so it doesn't
 * clip past the plot edge; every other label is centered on its tick. */
function xLabelAnchor(i: number, n: number): 'start' | 'end' | 'middle' {
  if (i === 0) return 'start';
  if (i === n - 1) return 'end';
  return 'middle';
}

/** Snaps a normalized step (1 <= norm < 10) to the nearest 1/2/5 multiplier. */
function niceStepMultiplier(norm: number): number {
  if (norm < 1.5) return 1;
  if (norm < 3) return 2;
  if (norm < 7) return 5;
  return 10;
}

/**
 * "Nice" y-axis ticks from 0 to a rounded ceiling >= max, aiming for ~`count`
 * intervals. Steps snap to 1/2/5 × 10^n so gridlines land on readable values;
 * the returned top tick is the y-domain maximum used for scaling.
 */
function niceTicks(max: number, count = 4): number[] {
  const safeMax = Math.max(max, 1);
  const rawStep = safeMax / count;
  const mag = Math.pow(10, Math.floor(Math.log10(rawStep)));
  const norm = rawStep / mag;
  // Round the step to the NEAREST 1/2/5 × 10^n so gridlines land on readable values.
  const niceStep = niceStepMultiplier(norm) * mag;
  // The top tick (the y-domain maximum) is niceStep rounded UP PAST the data max,
  // so the highest line always sits at or below the top gridline and never clips
  // out the top of the plot. Iterating by integer step count avoids float drift.
  const steps = Math.max(1, Math.ceil(safeMax / niceStep - 1e-9));
  const ticks: number[] = [];
  for (let k = 0; k <= steps; k++) {
    ticks.push(Math.round(k * niceStep * 1e6) / 1e6);
  }
  return ticks;
}

// Evenly spaced x-axis label indices (at most `maxLabels`), always including the
// first and last bucket, deduped for short series.
function labelIndices(n: number, maxLabels = 5): number[] {
  if (n <= 0) return [];
  const count = Math.min(maxLabels, n);
  if (count <= 1) return [0];
  const out: number[] = [];
  for (let k = 0; k < count; k++) {
    const i = Math.round((k * (n - 1)) / (count - 1));
    if (out.at(-1) !== i) out.push(i);
  }
  return out;
}

// Monotone cubic Hermite interpolation (Fritsch–Carlson) → cubic Bézier. Unlike a
// Catmull-Rom spline it never overshoots/undershoots: the curve stays within the
// value range of each segment (no bumps above a peak or below a trough).
// Exported for unit testing the no-overshoot property.
export function monotonePath(points: { x: number; y: number }[]): string {
  const n = points.length;
  if (n === 0) return '';
  if (n === 1) return `M ${points[0].x} ${points[0].y}`;
  if (n === 2) return `M ${points[0].x} ${points[0].y} L ${points[1].x} ${points[1].y}`;

  // Secant slopes between consecutive points.
  const dx: number[] = [];
  const slope: number[] = [];
  for (let i = 0; i < n - 1; i++) {
    const h = points[i + 1].x - points[i].x;
    dx.push(h);
    slope.push(h === 0 ? 0 : (points[i + 1].y - points[i].y) / h);
  }

  // Tangents: endpoints use the adjacent secant; interior points average the two
  // secants but are flattened to 0 at local extrema (sign change) to avoid overshoot.
  const m: number[] = new Array(n);
  m[0] = slope[0];
  m[n - 1] = slope[n - 2];
  for (let i = 1; i < n - 1; i++) {
    m[i] = slope[i - 1] * slope[i] <= 0 ? 0 : (slope[i - 1] + slope[i]) / 2;
  }

  // Fritsch–Carlson: clamp tangents so each segment stays monotone.
  for (let i = 0; i < n - 1; i++) {
    if (slope[i] === 0) {
      m[i] = 0;
      m[i + 1] = 0;
    } else {
      const a = m[i] / slope[i];
      const b = m[i + 1] / slope[i];
      const s = a * a + b * b;
      if (s > 9) {
        const tau = 3 / Math.sqrt(s);
        m[i] = tau * a * slope[i];
        m[i + 1] = tau * b * slope[i];
      }
    }
  }

  let d = `M ${points[0].x} ${points[0].y}`;
  for (let i = 0; i < n - 1; i++) {
    const h = dx[i];
    const cp1x = points[i].x + h / 3;
    const cp1y = points[i].y + (m[i] * h) / 3;
    const cp2x = points[i + 1].x - h / 3;
    const cp2y = points[i + 1].y - (m[i + 1] * h) / 3;
    d += ` C ${cp1x} ${cp1y}, ${cp2x} ${cp2y}, ${points[i + 1].x} ${points[i + 1].y}`;
  }
  return d;
}

/**
 * Hand-drawn SVG multi-series line chart (repo precedent: SpeedHistogram; no
 * chart library). Each series is a smooth line scaled to a shared 0..niceMax
 * y-axis; ~5 evenly-spaced x-axis time labels are drawn along the bottom. A
 * full-plot transparent overlay captures the mouse and shows a vertical guide,
 * a dot per series, and a tooltip listing the bucket time and each series value.
 * An empty window (no buckets, or every value 0) renders a no-data placeholder.
 */
export function LineChart({
  t,
  title,
  unit,
  series: rawSeries,
  times: rawTimes,
}: Readonly<{
  t: Translation;
  title: string;
  unit: string;
  series: LineSeries[];
  times: string[];
}>) {
  const [hover, setHover] = useState<number | null>(null);

  // Cap the drawn point count so long/fine windows stay smooth (see downsample).
  const { times, series } = useMemo(() => downsample(rawTimes, rawSeries), [rawTimes, rawSeries]);

  const n = times.length;
  const hasData = n > 0 && series.some((s) => s.values.some((v) => v !== 0));

  // The title sits alone on its own row (never truncated). The multi-series legend
  // is drawn INSIDE the SVG (see below), so the card height is the title row + the
  // fixed-height SVG — identical for every chart, with no legend-induced steps.
  const header = (
    <Typography component="h3" variant="subtitle2" noWrap sx={{ mb: 0.5 }}>
      {title}
    </Typography>
  );

  // Reserve a legend band inside the plot only when there is more than one series;
  // this shrinks the plot area of that chart instead of growing the whole card.
  const showLegend = series.length > 1 && series.length <= MAX_LEGEND_SERIES;
  const legendH = showLegend ? LEGEND_H : 0;
  const plotTop = MT + legendH;
  const plotBottom = H - MB;
  const innerH = plotBottom - plotTop;

  if (!hasData) {
    return (
      <Box>
        {header}
        <Box
          role="img"
          aria-label={title}
          sx={{
            aspectRatio: `${W} / ${H}`,
            width: '100%',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            color: 'var(--muted)',
            fontWeight: 700,
            border: '1px solid var(--line)',
            bgcolor: 'var(--page)',
          }}
        >
          {t.activityNoData}
        </Box>
      </Box>
    );
  }

  const dataMax = Math.max(1, ...series.flatMap((s) => s.values));
  const ticks = niceTicks(dataMax);
  const domainMax = ticks.at(-1)!;

  const xOf = (i: number) => (n === 1 ? plotLeft + innerW / 2 : plotLeft + (i / (n - 1)) * innerW);
  const yOf = (v: number) => plotBottom - (v / domainMax) * innerH;

  const xLabels = labelIndices(n);

  // Map the pointer's clientX onto the plot's [0..n-1] bucket index. The overlay
  // rect spans exactly the plot width, so its own bounding rect is the mapping
  // frame (deterministic + testable by stubbing getBoundingClientRect).
  const onMove = (e: MouseEvent<SVGRectElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) {
      setHover(0);
      return;
    }
    const frac = (e.clientX - rect.left) / rect.width;
    const i = Math.round(frac * (n - 1));
    setHover(Math.max(0, Math.min(n - 1, i)));
  };

  const hovered = hover;
  // Horizontal anchor of the tooltip as a fraction of the container width. The
  // tooltip is then shifted by translateX(-hoverFrac*100%) of ITS OWN width, so
  // its box spans [hoverFrac*(W-tipW), hoverFrac*(W-tipW)+tipW] and stays fully
  // within [0, W] for any tipW <= W: left-anchored near the left edge, centered
  // in the middle, right-anchored near the right edge. That lets it extend over
  // the plot's inner margins (its "edges") without leaving the card and without
  // the near-edge shrink-to-fit squeeze that used to force line breaks.
  const hoverFrac = hovered === null ? 0 : xOf(hovered) / W;

  return (
    <Box sx={{ position: 'relative' }}>
      {header}
      <Box
        component="svg"
        role="img"
        aria-label={title}
        viewBox={`0 0 ${W} ${H}`}
        sx={{
          width: '100%',
          height: 'auto',
          border: '1px solid var(--line)',
          bgcolor: 'var(--page)',
        }}
      >
        {showLegend && (
          <g data-testid="ts-legend">
            {series.map((s, i) => {
              const x = plotLeft + i * (innerW / series.length);
              return (
                <g key={s.label}>
                  <rect x={x} y={MT + 1} width={6} height={6} rx={1} fill={s.color} />
                  <text x={x + 9} y={MT + 6.5} fontSize={7} fill="var(--muted)">
                    {s.label}
                  </text>
                </g>
              );
            })}
          </g>
        )}

        {ticks.map((tick) => {
          const y = yOf(tick);
          return (
            <g key={`y-${tick}`}>
              <line
                x1={plotLeft}
                x2={plotRight}
                y1={y}
                y2={y}
                stroke="var(--line)"
                strokeWidth={0.5}
                opacity={0.5}
              />
              <text
                data-ytick={fmtNum(tick)}
                x={plotLeft - 3}
                y={y}
                textAnchor="end"
                dominantBaseline="middle"
                fontSize={8}
                fill="var(--muted)"
              >
                {fmtNum(tick)}
              </text>
            </g>
          );
        })}

        {xLabels.map((i) => (
          <text
            key={`x-${i}`}
            data-xlabel={i}
            x={xOf(i)}
            y={H - 6}
            textAnchor={xLabelAnchor(i, n)}
            fontSize={8}
            fill="var(--muted)"
          >
            {times[i]}
          </text>
        ))}

        {series.map((s, si) => {
          const pts = s.values.map((v, i) => ({ x: xOf(i), y: yOf(v) }));
          return (
            <path
              key={s.label}
              data-series={si}
              d={monotonePath(pts)}
              fill="none"
              stroke={s.color}
              strokeWidth={1.5}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
          );
        })}

        {hovered !== null && (
          <>
            <line
              data-guide
              x1={xOf(hovered)}
              x2={xOf(hovered)}
              y1={plotTop}
              y2={plotBottom}
              stroke="var(--brand-accent)"
              strokeWidth={1}
              strokeDasharray="3 3"
              opacity={0.7}
            />
            {series.map((s, si) => (
              <circle
                key={s.label}
                data-dot={si}
                cx={xOf(hovered)}
                cy={yOf(s.values[hovered] ?? 0)}
                r={2.5}
                fill={s.color}
              />
            ))}
          </>
        )}

        {/* Transparent, full-plot overlay LAST so it sits on top and captures the mouse. */}
        <rect
          data-testid="ts-overlay"
          x={plotLeft}
          y={plotTop}
          width={innerW}
          height={innerH}
          fill="transparent"
          style={{ cursor: 'crosshair' }}
          onMouseMove={onMove}
          onMouseLeave={() => setHover(null)}
        />
      </Box>

      {hovered !== null && (
        <Paper
          data-testid="ts-tooltip"
          elevation={3}
          // `left`/`transform` track the cursor (change per hovered bucket) so
          // they live in an inline style, not sx, to avoid emotion generating a
          // fresh class each time. `width: max-content` sizes the box to its
          // natural one-line width regardless of how little room is left of the
          // cursor — that defeats the near-edge shrink-to-fit squeeze that used
          // to hard-wrap every row. It's clamped by maxWidth below.
          style={{
            left: `${hoverFrac * 100}%`,
            transform: `translateX(${-hoverFrac * 100}%)`,
            width: 'max-content',
          }}
          sx={{
            position: 'absolute',
            top: 0,
            px: 1,
            py: 0.5,
            pointerEvents: 'none',
            // Few-series charts stay clamped to the card width (the translate above
            // keeps them in [0, W]); a many-series chart flows into columns (below)
            // and is allowed to grow wider than the card — bounded to the viewport —
            // so every core is visible rather than clipped.
            maxWidth: series.length > MAX_LEGEND_SERIES ? 'min(92vw, 900px)' : '100%',
            overflowWrap: 'anywhere',
            // Bound the height as a final safety; the column flow below normally
            // keeps it well under this.
            maxHeight: '90%',
            overflowY: 'auto',
            zIndex: 2,
          }}
        >
          <Typography variant="caption" sx={{ display: 'block', fontWeight: 700 }}>
            {times[hovered]}
          </Typography>
          {/* Rows flow top-to-bottom then into additional COLUMNS once they exceed
              TOOLTIP_MAX_ROWS, so a per-core chart shows every core in a compact
              grid instead of a single clipped column. */}
          <Box
            data-testid="ts-tooltip-grid"
            sx={{
              display: 'grid',
              gridAutoFlow: 'column',
              gridTemplateRows: `repeat(${Math.min(series.length, TOOLTIP_MAX_ROWS)}, auto)`,
              columnGap: 1.25,
              rowGap: 0.25,
            }}
          >
            {series.map((s) => {
              const unitSuffix = unit ? ` ${unit}` : '';
              return (
                <Typography
                  key={s.label}
                  variant="caption"
                  sx={{ display: 'flex', alignItems: 'center', gap: 0.5, whiteSpace: 'nowrap' }}
                >
                  <Box
                    aria-hidden
                    component="span"
                    sx={{
                      width: 8,
                      height: 8,
                      borderRadius: '2px',
                      bgcolor: s.color,
                      flexShrink: 0,
                    }}
                  />
                  {`${s.label}: ${fmtNum(s.values[hovered] ?? 0)}${unitSuffix}`}
                </Typography>
              );
            })}
          </Box>
        </Paper>
      )}
    </Box>
  );
}
