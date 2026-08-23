// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Collapse, IconButton, Typography } from '@mui/material';
import KeyboardArrowDown from '@mui/icons-material/KeyboardArrowDown';
import KeyboardArrowRight from '@mui/icons-material/KeyboardArrowRight';
import type { TimeSeries, UsageStats } from '../api';
import type { Translation } from './shared/types';
import { SelectField } from './shared/SelectField';
import { SpeedHistogram } from './SpeedHistogram';
import { LineChart } from './LineChart';
import {
  TS_WINDOWS,
  TS_BUCKETS,
  TS_WINDOW_SECONDS,
  formatTsSeconds,
  type TsWindow,
  type TsBucket,
} from './activityColumns';

// Local time label for a bucket start. Sub-hour resolutions show a wall-clock
// time (HH:MM[:SS]); hour-and-coarser resolutions add/switch to the date so the
// x-axis stays legible across multi-day/week/month windows. `bucketSeconds` is
// the (possibly coarsened) resolution reported by the server.
function tsTimeLabel(iso: string, bucketSeconds: number): string {
  const d = new Date(iso);
  if (bucketSeconds >= 86400) {
    // Day-and-coarser: date only.
    return d.toLocaleDateString(undefined, { day: '2-digit', month: '2-digit' });
  }
  if (bucketSeconds >= 3600) {
    // Hour resolution: date + hour, so buckets across days don't collide.
    return `${d.toLocaleDateString(undefined, { day: '2-digit', month: '2-digit' })} ${d.toLocaleTimeString(
      undefined,
      { hour: '2-digit', minute: '2-digit' },
    )}`;
  }
  if (bucketSeconds >= 60) {
    // Minute resolution: HH:MM (drop seconds).
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  }
  // Sub-minute: full wall-clock with seconds.
  return d.toLocaleTimeString();
}

export type ActivityChartsProps = {
  t: Translation;
  // Non-null: the parent only renders this section once stats have loaded
  // (mirrors the original `{stats && (...)}` guard in Activity.tsx).
  stats: UsageStats;
  // Best-effort like the other Activity fetches: null (initial load / error)
  // falls through to each LineChart's own no-data placeholder.
  timeSeries: TimeSeries | null;
  tsWindow: TsWindow;
  tsBucket: TsBucket;
  onTsWindow: (w: TsWindow) => void;
  onTsBucket: (b: TsBucket) => void;
  collapsed: boolean;
  onToggleCollapsed: () => void;
};

// Collapsible time-series charts section: the header toggle folds the whole
// section — the window/resolution controls AND the line charts (per the
// user's request), not just the graphs. The timeseries fetch keeps running
// regardless of collapse (owned by useActivityData, outside this component).
// Moved verbatim out of Activity.tsx — only the closure over props changed.
export function ActivityCharts({
  t,
  stats,
  timeSeries,
  tsWindow,
  tsBucket,
  onTsWindow,
  onTsBucket,
  collapsed,
  onToggleCollapsed,
}: Readonly<ActivityChartsProps>) {
  // Time-series chart inputs. When the series is null (initial load / error) the
  // arrays are empty and each LineChart falls through to its no-data placeholder.
  const tsPoints = timeSeries?.points ?? [];
  // Label buckets by the resolution the server actually used (coarsened for very
  // long windows), falling back to the requested bucket before the first fetch.
  const tsBucketSeconds = timeSeries?.bucket_seconds ?? tsBucket;
  const tsTimes = tsPoints.map((p) => tsTimeLabel(p.t, tsBucketSeconds));

  return (
    <>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, mb: 1 }}>
        <IconButton
          size="small"
          aria-label={t.activityChartsToggle}
          aria-expanded={!collapsed}
          onClick={onToggleCollapsed}
        >
          {collapsed ? (
            <KeyboardArrowRight fontSize="small" />
          ) : (
            <KeyboardArrowDown fontSize="small" />
          )}
        </IconButton>
        <Typography variant="subtitle1" component="h2">
          {t.activityChartsTitle}
        </Typography>
      </Box>
      <Collapse in={!collapsed} timeout="auto" unmountOnExit>
        {/* Shared window + resolution controls governing all three line charts.
          Dropdowns (many window/resolution options); labels are formatted from
          the duration so there is no i18n key per option. */}
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 2, mb: 2, alignItems: 'flex-start' }}>
          <Box sx={{ minWidth: 150 }}>
            <SelectField
              id="activity-ts-window"
              label={t.activityTsWindowLabel}
              value={tsWindow}
              onChange={(e) => onTsWindow(e.target.value as TsWindow)}
            >
              {TS_WINDOWS.map((w) => (
                <option key={w} value={w}>
                  {formatTsSeconds(TS_WINDOW_SECONDS[w], t)}
                </option>
              ))}
            </SelectField>
          </Box>
          <Box sx={{ minWidth: 150 }}>
            <SelectField
              id="activity-ts-bucket"
              label={t.activityTsBucketLabel}
              value={String(tsBucket)}
              onChange={(e) => onTsBucket(Number(e.target.value) as TsBucket)}
            >
              {TS_BUCKETS.map((b) => (
                <option key={b} value={String(b)}>
                  {formatTsSeconds(b, t)}
                </option>
              ))}
            </SelectField>
          </Box>
        </Box>

        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))' },
            gap: 2,
            mb: 3,
          }}
        >
          <LineChart
            t={t}
            title={t.activityTsConnections}
            unit={t.activityTsUnitReqPerSec}
            times={tsTimes}
            series={[
              {
                label: t.activityTsConnectionsThroughput,
                color: 'var(--brand-primary)',
                values: tsPoints.map((p) => p.connections),
              },
              {
                label: t.activityTsConcurrency,
                color: 'var(--chart-series-2)',
                values: tsPoints.map((p) => p.concurrency),
              },
            ]}
          />
          <LineChart
            t={t}
            title={t.activityTsTokenThroughput}
            unit={t.activityTsUnitTokPerSec}
            times={tsTimes}
            series={[
              {
                label: t.activityTsPromptThroughput,
                color: 'var(--brand-primary)',
                values: tsPoints.map((p) => p.prompt_tokens_per_second),
              },
              {
                label: t.activityTsCompletionThroughput,
                color: 'var(--chart-series-2)',
                values: tsPoints.map((p) => p.completion_tokens_per_second),
              },
            ]}
          />
          {/* Additive P3 T2 chart: energy per bucket, fed from the same
            time-series fetch + shared window/resolution controls above. */}
          <LineChart
            t={t}
            title={t.activityEnergyChart}
            unit="Wh"
            times={tsTimes}
            series={[
              {
                label: t.activityEnergyTile,
                color: 'var(--brand-primary)',
                values: tsPoints.map((p) => p.energy_wh ?? 0),
              },
            ]}
          />
        </Box>

        {/* The two bar-chart histograms (Prompt-Verarbeitung + Token-Generierung)
          live INSIDE the same Collapse so folding the charts section hides them
          too (per the user's request), not just the line charts above. */}
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))' },
            gap: 2,
            mb: 3,
          }}
        >
          <SpeedHistogram
            t={t}
            title={t.activityPromptSpeed}
            histogram={stats.prompt_per_second}
            unit={t.activityPromptSpeedUnit}
          />
          <SpeedHistogram
            t={t}
            title={t.activityTokenSpeed}
            histogram={stats.tokens_per_second}
            unit={t.activityTokenSpeedUnit}
          />
        </Box>
      </Collapse>
    </>
  );
}
