// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useMemo, useState } from 'react';
import { Box, Paper, Typography } from '@mui/material';
import type { Translation } from './shared/types';
import { formatTsSeconds } from './activityColumns';

export type TimelineState =
  | 'healthy'
  | 'degraded'
  | 'unhealthy'
  | 'present'
  | 'absent'
  | 'connected'
  | 'disconnected'
  | 'unknown';
export type UptimeSegment = { start: string; end: string; state: TimelineState };

const W = 1000;
const H = 48;

export function UptimeTimeline({
  t,
  title,
  segments,
  windowFrom,
  windowTo,
  colorForState,
  labelForState,
}: Readonly<{
  t: Translation;
  title: string;
  segments: UptimeSegment[];
  windowFrom: string;
  windowTo: string;
  colorForState: (s: TimelineState) => string;
  labelForState: (s: TimelineState) => string;
}>) {
  const [hover, setHover] = useState<{ x: number; text: string } | null>(null);
  const from = new Date(windowFrom).getTime();
  const to = new Date(windowTo).getTime();
  const span = Math.max(1, to - from);

  const rects = useMemo(
    () =>
      segments.map((seg, i) => {
        const a = Math.max(from, new Date(seg.start).getTime());
        const b = Math.min(to, new Date(seg.end).getTime());
        const x = ((a - from) / span) * W;
        const w = Math.max(0.5, ((b - a) / span) * W);
        const durS = Math.round((b - a) / 1000);
        const text = `${labelForState(seg.state)} · ${new Date(a).toLocaleString()}–${new Date(b).toLocaleTimeString()} (${formatTsSeconds(durS, t)})`;
        return { key: i, x, w, color: colorForState(seg.state), text };
      }),
    [segments, from, to, span, colorForState, labelForState, t],
  );

  const labels = useMemo(() => {
    const n = 5;
    return Array.from({ length: n }, (_, i) => {
      const frac = i / (n - 1);
      const ms = from + frac * span;
      return { frac, label: new Date(ms).toLocaleTimeString() };
    });
  }, [from, span]);

  return (
    <Box sx={{ position: 'relative' }}>
      <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
        {title}
      </Typography>
      <Box
        component="svg"
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        sx={{
          width: '100%',
          height: 40,
          border: '1px solid var(--line)',
          borderRadius: 1,
          bgcolor: 'var(--page)',
          display: 'block',
        }}
      >
        {rects.map((r) => (
          <rect
            key={r.key}
            data-segment
            x={r.x}
            y={0}
            width={r.w}
            height={H}
            fill={r.color}
            onMouseEnter={() => setHover({ x: r.x + r.w / 2, text: r.text })}
            onMouseLeave={() => setHover(null)}
          />
        ))}
      </Box>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', mt: 0.25 }}>
        {labels.map((l) => (
          <Typography key={l.frac} variant="caption" color="text.secondary">
            {l.label}
          </Typography>
        ))}
      </Box>
      {hover && (
        <Paper
          elevation={3}
          sx={{
            position: 'absolute',
            top: 22,
            left: `${(hover.x / W) * 100}%`,
            transform: 'translateX(-50%)',
            px: 1,
            py: 0.5,
            pointerEvents: 'none',
            whiteSpace: 'nowrap',
            fontSize: 12,
            zIndex: 2,
            maxWidth: '100%',
          }}
        >
          {hover.text}
        </Paper>
      )}
    </Box>
  );
}
