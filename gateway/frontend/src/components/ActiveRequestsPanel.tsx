// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useReducer } from 'react';
import type { ActiveRequest } from '../api';
import type { Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { ActivityScope } from './ActivityToolbar';

// Elapsed since a request started: "Xs" under a minute, otherwise "m:ss".
function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 60) return `${total}s`;
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}

export type ActiveRequestsPanelProps = {
  t: Translation;
  active: ActiveRequest[];
  effectiveScope: ActivityScope;
};

// Live view of in-flight requests: fed straight from the `active` array (SSE +
// 1s tick), so matching new connections appear and finished ones drop out;
// filters are client-side over the current set. Moved verbatim out of
// Activity.tsx (the activeColumns catalogue + the Panel/ListTable + the 1s
// elapsed ticker) — only the closure over props changed.
export function ActiveRequestsPanel({
  t,
  active,
  effectiveScope,
}: Readonly<ActiveRequestsPanelProps>) {
  // Forces a re-render every second so the elapsed column keeps ticking; the
  // tick counter itself is never read (elapsed is derived from Date.now() at
  // render time), so a reducer-based force-update is the idiomatic shape.
  const [, tick] = useReducer((n: number) => n + 1, 0);

  // Tick once a second ONLY while requests are running, so the elapsed column stays
  // live; the interval is torn down as soon as the list empties (or on unmount).
  useEffect(() => {
    if (active.length === 0) return;
    const id = window.setInterval(tick, 1000);
    return () => window.clearInterval(id);
  }, [active.length]);

  // Read once per render; the 1s ticker re-renders so this stays current.
  const now = Date.now();
  const activeShowOwner = effectiveScope === 'all';
  // Running-connections table columns (client-side ListTable; live from `active`).
  // Default order: Benutzer, Token, Angefragt, Model, Pfad, Server, Stream,
  // Laufzeit. Owner
  // is scope-gated via `available`. Laufzeit is derived (re-rendered each 1s tick).
  const activeColumns: ListColumn<ActiveRequest>[] = [
    {
      id: 'owner',
      label: t.activityColOwner,
      value: (a) => a.user_name || a.user_id,
      filter: 'text',
      available: activeShowOwner,
    },
    // Token-less (session) chat carries the user's name as token_name; label it
    // as the session path instead of showing the name as if it were a token.
    {
      id: 'token',
      label: t.tableToken,
      value: (a) => (a.token_id ? a.token_name : t.activityActiveSession),
      filter: 'text',
    },
    // Optional (hidden by default): the Service Account that served the request,
    // if any (client-side filtered, mirrors the session/agent_id columns below).
    {
      id: 'service_name',
      label: t.activityColService,
      value: (a) => a.service_name || '',
      filter: 'text',
      defaultHidden: true,
    },
    // The model the client asked for before a token override rewrote it.
    // Visible by default, like its counterpart in the completed-requests table:
    // seeing it without waiting for the request to finish is the whole point of
    // carrying it here. Empty only on the paths that never resolved a model.
    {
      id: 'requested_model',
      label: t.tableRequestedModel,
      value: (a) => a.requested_model,
      render: (a) => a.requested_model || '-',
      filter: 'text',
    },
    { id: 'model', label: t.tableModel, value: (a) => a.model, filter: 'text' },
    { id: 'req_path', label: t.activityColPath, value: (a) => a.req_path, filter: 'text' },
    // Optional (hidden by default): the upstream path the request is calling —
    // interesting when the built-in translation is used (differs from req_path).
    {
      id: 'provider_path',
      label: t.activityColProviderPath,
      value: (a) => a.provider_path,
      filter: 'text',
      defaultHidden: true,
    },
    // Optional (hidden by default): the upstream model the provider receives (empty
    // when the requested model is used as-is). Mirrors the usage table's column.
    {
      id: 'provider_model',
      label: t.activityColProviderModel,
      value: (a) => a.provider_model,
      filter: 'text',
      defaultHidden: true,
    },
    // Optional (hidden by default): the client/agent session id + its source
    // (e.g. codex/claude-code). Client-side filtered via the value accessor.
    {
      id: 'session',
      label: t.activityColSession,
      value: (a) => a.session_id || '',
      filter: 'text',
      defaultHidden: true,
      render: (a) => (a.session_id ? `${a.session_source || '?'} · ${a.session_id}` : '-'),
    },
    {
      id: 'agent_id',
      label: t.activityColAgent,
      value: (a) => a.agent_id || '',
      filter: 'text',
      defaultHidden: true,
    },
    {
      id: 'server',
      label: t.tableHost,
      value: (a) => a.server_name,
      filter: 'text',
      render: (a) => a.server_name || '-',
    },
    {
      id: 'stream',
      label: t.activityColStream,
      value: (a) => (a.stream ? t.yes : t.no),
      filter: 'enum',
      searchable: false,
      render: (a) => (a.stream ? '✓' : '–'),
    },
    {
      id: 'elapsed',
      label: t.activityActiveElapsed,
      value: () => '',
      searchable: false,
      sortable: false,
      render: (a) => formatElapsed(now - new Date(a.started_at).getTime()),
    },
  ];

  return (
    <Panel titleId="active-heading" title={t.activityActiveTitle}>
      <ListTable
        rows={active}
        columns={activeColumns}
        rowKey={(a) => a.id}
        storageKey="op.activeRequests"
        minWidth={560}
        labels={listTableLabels(t, {
          empty: t.activityActiveEmpty,
          loading: undefined,
        })}
      />
    </Panel>
  );
}
