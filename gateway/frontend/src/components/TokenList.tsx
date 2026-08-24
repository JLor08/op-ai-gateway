// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  useEffect,
  useRef,
  useState,
  type Dispatch,
  type SubmitEvent,
  type SetStateAction,
} from 'react';
import type {
  ModelOption,
  PortalServer,
  PortalToken,
  ProjectRef,
  ServerModelOption,
  UpdateTokenRequest,
} from '../api';
import type { Translation, MessageKey, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import {
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import AutorenewIcon from '@mui/icons-material/Autorenew';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { SecretReveal } from './shared/SecretReveal';
import { Field } from './shared/Field';
import { CheckboxGroup } from './shared/CheckboxGroup';
import { SearchableSelect } from './shared/SearchableSelect';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import {
  ModelOverrideEditor,
  buildOverrideMap,
  overrideRowsInvalid,
  overrideSummary,
  type OverrideRow,
} from './shared/ModelOverrideEditor';

const allTokenScopes = ['gateway:use', 'admin'] as const;

const tokenStatusLabelByKey: Record<PortalToken['status'], MessageKey> = {
  active: 'statusActive',
  disabled: 'statusDisabled',
  expired: 'statusExpired',
};

type Mode = 'list' | 'create' | { edit: PortalToken };

function toggleScope(list: string[], scope: string): string[] {
  return list.includes(scope) ? list.filter((s) => s !== scope) : [...list, scope];
}

export function TokenList({
  t,
  api,
  tokens,
  setTokens,
  role,
  models,
  servers = [],
  loading = false,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'createToken'
    | 'deleteToken'
    | 'myProjects'
    | 'rotateToken'
    | 'serverModels'
    | 'updateChatSettings'
    | 'updateToken'
  >;
  tokens: PortalToken[];
  setTokens: Dispatch<SetStateAction<PortalToken[]>>;
  role: string;
  models: ModelOption[];
  // The caller's manageable servers (api.servers() -> ListServers, which is
  // already manager-scoped server-side). Drives whether the server-override
  // picker renders at all (hidden entirely with zero manageable servers) and
  // its option list. Optional so pre-existing test renders that never touch
  // the picker keep working unchanged; defaults to [].
  servers?: PortalServer[];
  loading?: boolean;
}>) {
  const selectableScopes: string[] =
    role === 'admin' || role === 'system_admin' ? [...allTokenScopes] : ['gateway:use'];
  const { showError } = useToast();

  const [mode, setMode] = useState<Mode>('list');
  const [createdSecret, setCreatedSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  const [confirmingRotateId, setConfirmingRotateId] = useState('');

  // Shared create/edit form state.
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<string[]>(['gateway:use']);
  const [scopesDirty, setScopesDirty] = useState(false);
  // Model override: a per-requested-model mapping (rows) + an optional catch-all.
  // A row maps a requested model name (free text) -> a gateway model; the catch-all
  // applies to any requested model with no row. Empty rows + empty catch-all = off.
  const [overrideRows, setOverrideRows] = useState<OverrideRow[]>([]);
  const [catchAll, setCatchAll] = useState('');
  // Server override: forces every request on this token onto one specific
  // server the caller manages. "" = no override. The whole control is hidden
  // when the caller manages zero servers (see `servers` prop).
  const [serverOverride, setServerOverride] = useState('');
  const [serverOverrideForce, setServerOverrideForce] = useState(false);
  // The distinct gateway models the selected override server offers (see
  // api.serverModels), narrowing the model-override map's "to" dropdown once a
  // server override is picked. Empty while no override is set.
  const [serverOverrideModels, setServerOverrideModels] = useState<ServerModelOption[]>([]);
  // The unknown-model redirect (Task 8): a requested model this token cannot
  // route to is served by the token's last_used_model (read-only, display
  // only — see below), or else by unknownFallback, instead of failing.
  // unknownRedirectBlocked widens "unknown" to also cover a model that exists
  // but this token cannot call. Both sub-settings are disabled in the UI (and
  // ignored server-side) while unknownRedirect is off.
  const [unknownRedirect, setUnknownRedirect] = useState(false);
  const [unknownRedirectBlocked, setUnknownRedirectBlocked] = useState(false);
  const [unknownFallback, setUnknownFallback] = useState('');
  const [logCommunication, setLogCommunication] = useState(false);
  const [secret, setSecret] = useState(false);
  // Project attribution (spec: 2026-08-08-projects-design.md §6): "" = no project.
  // The picker's options are the caller's own eligible projects (api.myProjects()),
  // loaded lazily each time the form opens (create/edit) with a latest-wins guard
  // so a slow/stale response from a previous open can't clobber a newer one.
  const [projectId, setProjectId] = useState('');
  const [myProjects, setMyProjects] = useState<ProjectRef[]>([]);
  const projectsReqIdRef = useRef(0);

  // Synthetic, token-less ChatSession pseudo-token (own settings panel).
  const chatSession = tokens.find((row) => row.is_chat_session) ?? null;
  const realTokens = tokens.filter((row) => !row.is_chat_session);
  const [chatDraft, setChatDraft] = useState<{
    log_communication: boolean;
    secret: boolean;
  } | null>(null);

  useEffect(() => {
    if (mode === 'list') return;
    const reqId = ++projectsReqIdRef.current;
    api
      .myProjects()
      .then((rows) => {
        if (projectsReqIdRef.current === reqId) setMyProjects(rows);
      })
      .catch(() => {
        /* best-effort: fall through with the previously-loaded list */
      });
  }, [mode, api]);

  // Load the selected override server's offered models so the model-override
  // map's "to" dropdown can narrow to them. Latest-wins guard so a
  // slow/stale response for a previously-selected server can't clobber a
  // newer selection; clearing the override drops the list without a fetch.
  useEffect(() => {
    if (serverOverride === '') {
      setServerOverrideModels([]);
      return;
    }
    let cancelled = false;
    api
      .serverModels(serverOverride)
      .then((rows) => {
        if (!cancelled) setServerOverrideModels(rows);
      })
      .catch(() => {
        if (!cancelled) setServerOverrideModels([]);
      });
    return () => {
      cancelled = true;
    };
  }, [serverOverride, api]);

  // Model-override row-editor validity, used to gate submit below. buildOverrideMap,
  // overrideSummary and the row editor itself are shared with ServicesView via
  // ./shared/ModelOverrideEditor (FV-3).
  const overrideInvalid = overrideRowsInvalid(overrideRows);

  function openCreate() {
    setName('');
    setScopes(['gateway:use']);
    setScopesDirty(false);
    setOverrideRows([]);
    setCatchAll('');
    setServerOverride('');
    setServerOverrideForce(false);
    setUnknownRedirect(false);
    setUnknownRedirectBlocked(false);
    setUnknownFallback('');
    setLogCommunication(false);
    setSecret(false);
    setProjectId('');
    setMode('create');
  }

  function openEdit(row: PortalToken) {
    setName(row.name);
    setScopes(row.scopes.filter((scope) => selectableScopes.includes(scope)));
    setScopesDirty(false);
    setOverrideRows(
      Object.entries(row.model_override_map ?? {}).map(([from, entry]) => ({
        from,
        to: entry.to,
        // A missing switch (hand-written or older response) reads as false,
        // never crashes the editor.
        offer: entry.offer ?? false,
        hideTarget: entry.hide_target ?? false,
      })),
    );
    setCatchAll(row.model_override);
    setServerOverride(row.server_override ?? '');
    setServerOverrideForce(row.server_override_force_unreachable ?? false);
    setUnknownRedirect(row.unknown_model_redirect ?? false);
    setUnknownRedirectBlocked(row.unknown_model_redirect_blocked ?? false);
    setUnknownFallback(row.unknown_model_fallback ?? '');
    setLogCommunication(row.log_communication);
    setSecret(row.secret);
    setProjectId(row.project_id ?? '');
    setMode({ edit: row });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const response = await api.createToken({
        name,
        scopes,
        model_override: catchAll,
        model_override_map: buildOverrideMap(overrideRows),
        server_override: serverOverride,
        server_override_force_unreachable: serverOverrideForce,
        // last_used_model is deliberately never sent: it is read-only (see
        // PortalToken) and not part of CreateTokenRequest at all.
        unknown_model_redirect: unknownRedirect,
        unknown_model_redirect_blocked: unknownRedirectBlocked,
        unknown_model_fallback: unknownFallback,
        log_communication: logCommunication,
        secret,
        project_id: projectId,
      });
      setTokens((current) => [response.token, ...current]);
      setCreatedSecret(response.secret);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (mode === 'list' || mode === 'create') return;
    const id = mode.edit.id;
    try {
      // Only send scopes when the user actually changed them, so a non-admin
      // editing a token can't accidentally strip a scope they can't see.
      const body: UpdateTokenRequest = scopesDirty ? { name, scopes } : { name };
      body.model_override = catchAll;
      body.model_override_map = buildOverrideMap(overrideRows);
      body.server_override = serverOverride;
      body.server_override_force_unreachable = serverOverrideForce;
      // last_used_model is deliberately never sent: it is read-only (see
      // PortalToken) and not part of UpdateTokenRequest at all.
      body.unknown_model_redirect = unknownRedirect;
      body.unknown_model_redirect_blocked = unknownRedirectBlocked;
      body.unknown_model_fallback = unknownFallback;
      body.log_communication = logCommunication;
      body.secret = secret;
      body.project_id = projectId;
      const updated = await api.updateToken(id, body);
      setTokens((current) => current.map((row) => (row.id === id ? updated : row)));
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function toggleStatus(row: PortalToken) {
    const nextStatus = row.status === 'active' ? 'disabled' : 'active';
    try {
      const updated = await api.updateToken(row.id, { status: nextStatus });
      setTokens((current) => current.map((item) => (item.id === row.id ? updated : item)));
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function removeToken(id: string) {
    try {
      await api.deleteToken(id);
      setTokens((current) => current.filter((row) => row.id !== id));
      setConfirmingDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function rotateToken(id: string) {
    try {
      const res = await api.rotateToken(id);
      setTokens((current) => current.map((row) => (row.id === id ? res.token : row)));
      setCreatedSecret(res.secret);
      setConfirmingRotateId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function saveChatSettings(next: { log_communication: boolean; secret: boolean }) {
    try {
      await api.updateChatSettings(next);
      setTokens((current) =>
        current.map((row) =>
          row.is_chat_session
            ? { ...row, log_communication: next.log_communication, secret: next.secret }
            : row,
        ),
      );
      setChatDraft(null);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<PortalToken>[] = [
    { id: 'name', label: t.tableName, value: (r) => r.name, filter: 'text' },
    {
      id: 'scope',
      label: t.tableScope,
      value: (r) => r.scopes.join(', '),
      filter: 'text',
      render: (r) => r.scopes.join(', ') || '-',
    },
    {
      id: 'model',
      label: t.tokenModelOverrideColumn,
      value: (r) => overrideSummary(t, r),
      filter: 'text',
      render: (r) => overrideSummary(t, r) || '-',
    },
    {
      id: 'project',
      label: t.tokenProjectColumn,
      value: (r) => r.project_name || '',
      filter: 'text',
      defaultHidden: true,
      render: (r) => r.project_name || '-',
    },
    {
      id: 'last_used_model',
      label: t.tokenLastUsedModel,
      value: (r) => r.last_used_model || '',
      filter: 'text',
      defaultHidden: true,
      render: (r) => r.last_used_model || '—',
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (r) => r.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[tokenStatusLabelByKey[v as PortalToken['status']] ?? 'statusActive'],
      render: (r) => <StatusChip status={r.status} label={t[tokenStatusLabelByKey[r.status]]} />,
    },
  ];

  const rowActions = (row: PortalToken): RowAction[] => [
    {
      key: 'edit',
      label: t.tokenActionEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => openEdit(row),
    },
    {
      key: 'toggle',
      label: row.status === 'active' ? t.tokenActionDisable : t.tokenActionEnable,
      icon:
        row.status === 'active' ? (
          <BlockIcon fontSize="small" />
        ) : (
          <CheckCircleIcon fontSize="small" />
        ),
      onClick: () => void toggleStatus(row),
    },
    {
      key: 'rotate',
      label: t.tokenActionRotate,
      icon: <AutorenewIcon fontSize="small" />,
      onClick: () => setConfirmingRotateId(row.id),
    },
    {
      key: 'delete',
      label: t.tokenActionDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => setConfirmingDeleteId(row.id),
    },
  ];

  // Create / edit sub-view (input mask).
  if (mode !== 'list') {
    const editing = mode !== 'create';
    // Project picker options: the caller's own eligible projects, plus an
    // explicit "(no project)" empty option. If the token is currently
    // attributed to a project that dropped out of myProjects() (e.g. the
    // owner left it since assignment), keep it selectable/visible so the
    // field doesn't silently blank out an existing attribution.
    const projectOptions = [
      { value: '', label: t.tokenProjectNone },
      ...myProjects.map((p) => ({ value: p.id, label: p.name })),
    ];
    if (projectId !== '' && !myProjects.some((p) => p.id === projectId)) {
      const fallbackLabel = editing ? mode.edit.project_name || projectId : projectId;
      projectOptions.push({ value: projectId, label: fallbackLabel });
    }
    // Once a server override is selected, the model-override map's gateway-model
    // dropdown (both per-row and the catch-all) narrows to that server's OWN
    // offered models (api.serverModels) instead of the full model list.
    const overrideModelOptions = serverOverride !== '' ? serverOverrideModels : models;
    // The unknown-model redirect's fallback picker offers the SAME
    // model-plus-group list the caller loads for everything else (the
    // portal model listing already carries groups, marked `is_group`) —
    // no separate fetch, and deliberately NOT server-override-narrowed
    // (unlike overrideModelOptions above): the redirect is unrelated to a
    // server override.
    const unknownFallbackOptions = [
      { value: '', label: '-' },
      ...models.map((m) => ({ value: m.id, label: m.display_name })),
    ];
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.apiTokens, onClick: () => setMode('list') },
            { label: editing ? t.tokenEditTitle : t.tokenCreate },
          ]}
        />
        <Panel titleId="token-form-heading" title={editing ? t.tokenEditTitle : t.tokenCreate}>
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="token-name"
              label={t.tokenNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t.tokenCreatedName}
            />
            <CheckboxGroup
              legend={t.tokenScopesLabel}
              options={selectableScopes.map((scope) => ({ value: scope, label: scope }))}
              selected={scopes}
              onToggle={(scope) => {
                setScopesDirty(true);
                setScopes((current) => toggleScope(current, scope));
              }}
            />
            {/* The server-override control is hidden entirely when the caller
                manages zero servers — there is nothing to override onto. */}
            {servers.length > 0 && (
              <Box sx={{ display: 'grid', gap: 1 }}>
                <SearchableSelect
                  id="token-server-override"
                  label={t.serverOverrideLabel}
                  value={serverOverride}
                  onChange={setServerOverride}
                  helperText={t.serverOverrideNote}
                  options={[
                    { value: '', label: t.serverOverrideNone },
                    ...servers.map((server) => ({
                      value: server.id,
                      label:
                        server.status === 'maintenance'
                          ? `${server.name} (${t.statusMaintenance})`
                          : server.name,
                    })),
                  ]}
                />
                {serverOverride !== '' && (
                  <>
                    <FormControlLabel
                      control={
                        <Checkbox
                          checked={serverOverrideForce}
                          onChange={(e) => setServerOverrideForce(e.target.checked)}
                        />
                      }
                      label={t.serverOverrideForceLabel}
                    />
                    <Typography variant="caption" color="text.secondary">
                      {t.serverOverrideForceHelp}
                    </Typography>
                    <Typography variant="caption" color="text.secondary">
                      {t.serverOverrideFilteredHint}
                    </Typography>
                  </>
                )}
              </Box>
            )}
            <ModelOverrideEditor
              rows={overrideRows}
              onRowsChange={setOverrideRows}
              catchAll={catchAll}
              onCatchAllChange={setCatchAll}
              models={overrideModelOptions}
              t={t}
              idPrefix="token"
              catchAllId="token-model-catchall"
            />
            <Box sx={{ display: 'grid', gap: 1 }}>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={unknownRedirect}
                    onChange={(e) => {
                      const checked = e.target.checked;
                      setUnknownRedirect(checked);
                      // Switching the redirect off also clears both
                      // sub-settings in STATE, not just their disabled
                      // rendering: submitCreate/submitEdit send them
                      // unconditionally, and a saved fallback the UI shows
                      // as disabled/invisible would mislead the next reader
                      // of the request into thinking it is in effect (the
                      // backend clears them too, but the form must show
                      // exactly what it sends).
                      if (!checked) {
                        setUnknownRedirectBlocked(false);
                        setUnknownFallback('');
                      }
                    }}
                  />
                }
                label={t.tokenUnknownRedirect}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenUnknownRedirectHint}
              </Typography>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={unknownRedirectBlocked}
                    disabled={!unknownRedirect}
                    onChange={(e) => setUnknownRedirectBlocked(e.target.checked)}
                  />
                }
                label={t.tokenUnknownRedirectBlocked}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenUnknownRedirectBlockedHint}
              </Typography>
              <SearchableSelect
                id="token-unknown-fallback"
                label={t.tokenUnknownFallback}
                value={unknownFallback}
                onChange={setUnknownFallback}
                disabled={!unknownRedirect}
                options={unknownFallbackOptions}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenLastUsedModel}
              </Typography>
              <Typography variant="body2">
                {editing
                  ? mode.edit.last_used_model || t.tokenLastUsedModelNone
                  : t.tokenLastUsedModelNone}
              </Typography>
            </Box>
            <SearchableSelect
              id="token-project"
              label={t.tokenProjectLabel}
              value={projectId}
              onChange={setProjectId}
              helperText={t.tokenProjectNote}
              options={projectOptions}
            />
            <FormControlLabel
              control={
                <Checkbox
                  checked={logCommunication}
                  onChange={(e) => setLogCommunication(e.target.checked)}
                />
              }
              label={t.tokenLogCommunicationLabel}
            />
            <FormControlLabel
              control={<Checkbox checked={secret} onChange={(e) => setSecret(e.target.checked)} />}
              label={t.tokenSecretLabel}
            />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy || overrideInvalid}>
                {editing ? t.tokenActionSave : t.tokenCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  const chatDraftValue = chatSession
    ? (chatDraft ?? {
        log_communication: chatSession.log_communication,
        secret: chatSession.secret,
      })
    : null;

  return (
    <>
      <PageTitle
        title={t.apiTokens}
        subtitle={t.tokensIntro}
        action={
          <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
            {t.tokenCreate}
          </Button>
        }
      />

      {chatSession && chatDraftValue && (
        <Panel
          titleId="chat-session-heading"
          title={t.chatSessionName}
          subtitle={t.chatSessionHint}
        >
          <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1.5 }}>
            <FormControlLabel
              control={
                <Checkbox
                  checked={chatDraftValue.log_communication}
                  onChange={(e) =>
                    setChatDraft({ ...chatDraftValue, log_communication: e.target.checked })
                  }
                  slotProps={{
                    input: { 'aria-label': `${t.tokenLogCommunicationLabel} ${t.chatSessionName}` },
                  }}
                />
              }
              label={t.tokenLogCommunicationLabel}
            />
            <FormControlLabel
              control={
                <Checkbox
                  checked={chatDraftValue.secret}
                  onChange={(e) => setChatDraft({ ...chatDraftValue, secret: e.target.checked })}
                  slotProps={{
                    input: { 'aria-label': `${t.tokenSecretLabel} ${t.chatSessionName}` },
                  }}
                />
              }
              label={t.tokenSecretLabel}
            />
            <Button
              variant="outlined"
              size="small"
              type="button"
              onClick={() => saveChatSettings(chatDraftValue)}
            >
              {t.tokenActionSave}
            </Button>
          </Box>
        </Panel>
      )}

      <Panel titleId="token-heading" title={t.tokenListTitle}>
        <ListTable
          rows={realTokens}
          columns={columns}
          rowKey={(r) => r.id}
          actions={rowActions}
          storageKey="op.tokens"
          labels={listTableLabels(t)}
          loading={loading}
        />
      </Panel>

      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={t.tokenDeleteConfirm}
        confirmLabel={t.tokenActionDelete}
        cancelLabel={t.tokenActionCancel}
        onConfirm={() => removeToken(confirmingDeleteId)}
        onCancel={() => setConfirmingDeleteId('')}
      />

      <ConfirmDialog
        open={confirmingRotateId !== ''}
        title={t.tokenRotateConfirmTitle}
        body={t.tokenRotateConfirmBody}
        confirmLabel={t.tokenRotateConfirm}
        cancelLabel={t.tokenActionCancel}
        onConfirm={() => rotateToken(confirmingRotateId)}
        onCancel={() => setConfirmingRotateId('')}
      />

      <Dialog
        open={createdSecret !== ''}
        onClose={() => setCreatedSecret('')}
        maxWidth="md"
        fullWidth
      >
        <DialogTitle>{t.oneTimeSecret}</DialogTitle>
        <DialogContent>
          <SecretReveal
            title={t.oneTimeSecret}
            copyValue={createdSecret}
            copyLabel={t.oneTimeSecret}
            copiedLabel={t.copied}
          >
            <code>{createdSecret}</code>
          </SecretReveal>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setCreatedSecret('')}>{t.captureClose}</Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
