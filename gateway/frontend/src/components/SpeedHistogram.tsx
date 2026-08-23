// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Tooltip, Typography } from '@mui/material';
import type { Histogram } from '../api';
import type { Translation } from './shared/types';

const W = 280;
const H = 140;
// Plot insets: leave room for the y-axis tick labels on the left and the
// x-axis min/max labels along the bottom so neither overlaps the bars.
const ML = 26; // left margin (y-axis labels)
const MR = 6; // right pad
const MT = 6; // top pad
const MB = 16; // bottom margin (x-axis labels)
const innerW = W - ML - MR;
const innerH = H - MT - MB;
const plotLeft = ML;
const plotTop = MT;
const plotBottom = MT + innerH; // == H - MB

const fmt = (v: number) => v.toFixed(1);

/**
 * Integer y-axis ticks from 0 to maxCount, at most ~4 ticks, never fractional.
 * The top tick is always maxCount; a near-duplicate neighbour is dropped so the
 * forced top label does not crowd the tick just below it.
 */
function yTicks(maxCount: number): number[] {
  const maxTicks = 4;
  const step = Math.max(1, Math.ceil(maxCount / (maxTicks - 1)));
  const ticks: number[] = [];
  for (let v = 0; v < maxCount; v += step) ticks.push(v);
  const last = ticks.at(-1)!;
  if (ticks.length > 1 && maxCount - last <= step / 2) ticks.pop();
  ticks.push(maxCount);
  return ticks;
}

/**
 * Hand-drawn SVG histogram (repo SVG precedent: MatrixLogo; no chart
 * library). Bars scale to the tallest bin; three dashed lines mark p50/p95/p99.
 * Integer y-axis ticks and the x-axis min/max are labelled around the plot, and
 * each bar carries a follow-cursor tooltip with its range and count. An empty
 * distribution (N==0) renders a "no data" placeholder instead.
 */
export function SpeedHistogram({
  t,
  title,
  histogram,
  unit,
}: Readonly<{
  t: Translation;
  title: string;
  histogram: Histogram;
  unit: string;
}>) {
  // Defensive: an older/again-nil backend (or any path that lets a nil Go slice
  // through) serializes bins to JSON null. Coalesce so bins.length never throws
  // and the empty distribution falls through to the no-data placeholder below.
  const bins = histogram.bins ?? [];

  if (bins.length === 0) {
    return (
      <Box>
        <Typography component="h3" variant="subtitle2" sx={{ mb: 0.5 }}>
          {title}
        </Typography>
        <Box
          role="img"
          aria-label={title}
          sx={{
            height: H,
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

  const maxCount = Math.max(...bins.map((b) => b.count), 1);
  const span = histogram.max - histogram.min || 1; // guard min==max (no div-by-0)
  const barW = innerW / bins.length;
  const xOf = (value: number) => plotLeft + ((value - histogram.min) / span) * innerW;
  const yOf = (count: number) => plotBottom - (count / maxCount) * innerH;
  const ticks = yTicks(maxCount);

  return (
    <Box>
      <Typography component="h3" variant="subtitle2" sx={{ mb: 0.5 }}>
        {title}
      </Typography>
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
        {ticks.map((tick) => {
          const y = yOf(tick);
          return (
            <g key={`y-${tick}`}>
              <line
                data-gridline={tick}
                x1={plotLeft}
                x2={W - MR}
                y1={y}
                y2={y}
                stroke="var(--line)"
                strokeWidth={0.5}
                opacity={0.5}
              />
              <text
                x={plotLeft - 3}
                y={y}
                textAnchor="end"
                dominantBaseline="middle"
                fontSize={8}
                fill="var(--muted)"
              >
                {tick}
              </text>
            </g>
          );
        })}
        {bins.map((bin, i) => {
          const h = (bin.count / maxCount) * innerH;
          return (
            <Tooltip
              key={`${bin.x0}-${bin.x1}`}
              followCursor
              title={
                <>
                  <Box component="span" sx={{ display: 'block' }}>
                    {`${fmt(bin.x0)} – ${fmt(bin.x1)} ${unit}`}
                  </Box>
                  <Box component="span" sx={{ display: 'block' }}>
                    {`${t.activityCount}: ${bin.count}`}
                  </Box>
                </>
              }
            >
              <rect
                data-bin={i}
                x={plotLeft + i * barW}
                y={plotBottom - h}
                width={Math.max(barW - 1, 1)}
                height={h}
                fill="var(--brand-primary)"
              />
            </Tooltip>
          );
        })}
        {(['p50', 'p95', 'p99'] as const).map((key) => {
          const x = xOf(histogram[key]);
          return (
            <line
              key={key}
              data-marker={key}
              x1={x}
              x2={x}
              y1={plotTop}
              y2={plotBottom}
              stroke="var(--brand-accent)"
              strokeWidth={1}
              strokeDasharray="4 3"
            />
          );
        })}
        <text x={plotLeft} y={H - 4} textAnchor="start" fontSize={8} fill="var(--muted)">
          {fmt(histogram.min)}
        </text>
        <text x={W - MR} y={H - 4} textAnchor="end" fontSize={8} fill="var(--muted)">
          {fmt(histogram.max)}
        </text>
      </Box>
    </Box>
  );
}
