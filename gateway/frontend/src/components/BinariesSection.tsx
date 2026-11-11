// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The "Binaries" tab of a server's runtime admin: the llama-server and
// ggml-rpc-server binaries the agent reports (read-only except for a version
// override) next to the rows an operator adds by hand.

import { useEffect, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  IconButton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import EditIcon from '@mui/icons-material/Edit';
import TuneIcon from '@mui/icons-material/Tune';
import type { PutServerBinaryRequest, ServerBinary, ServerBinaryKind } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { useToast } from './shared/ToastProvider';

type Editor = {
  // 'new' creates a manual row; 'manual' edits one; 'override' sets the
  // version override of an agent row.
  mode: 'new' | 'manual' | 'override';
  path: string;
  kind: ServerBinaryKind;
  version: string;
  build: string;
  commit: string;
  override: string;
};

function kindLabel(kind: string, t: Translation): string {
  if (kind === 'llama_server') return t.binariesKindLlamaServer;
  if (kind === 'rpc_server') return t.binariesKindRpcServer;
  return t.binariesKindOther;
}

function editorFor(mode: Editor['mode'], b?: ServerBinary): Editor {
  return {
    mode,
    path: b?.path ?? '',
    kind: (b?.kind as ServerBinaryKind | undefined) ?? 'llama_server',
    version: b?.version ?? '',
    build: b?.build ?? '',
    commit: b?.commit ?? '',
    override: b?.version_override ?? '',
  };
}

export function BinariesSection({
  t,
  api,
  serverId,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    'serverBinaries' | 'createServerBinary' | 'updateServerBinary' | 'deleteServerBinary'
  >;
  serverId: string;
}>) {
  const { showError, showSuccess } = useToast();
  const {
    data,
    error: loadError,
    reload,
  } = useResource(() => api.serverBinaries(serverId), [api, serverId, t], t);
  const rows = data?.data ?? [];
  const [editor, setEditor] = useState<Editor | null>(null);
  const [busy, setBusy] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<ServerBinary | null>(null);

  // An editor for a row the list no longer has (deleted here or elsewhere)
  // closes: its save could only answer not-found.
  useEffect(() => {
    if (!data || !editor || editor.mode === 'new') return;
    if (!data.data.some((b) => b.path === editor.path)) setEditor(null);
  }, [data, editor]);

  function patch(next: Partial<Editor>) {
    setEditor((e) => (e ? { ...e, ...next } : e));
  }

  async function save() {
    if (!editor) return;
    setBusy(true);
    try {
      if (editor.mode === 'override') {
        await api.updateServerBinary(serverId, {
          path: editor.path,
          version_override: editor.override.trim(),
        });
      } else {
        const body: PutServerBinaryRequest = {
          path: editor.path.trim(),
          kind: editor.kind,
          version: editor.version.trim(),
          build: editor.build.trim(),
          commit: editor.commit.trim(),
        };
        if (editor.mode === 'new') await api.createServerBinary(serverId, body);
        else await api.updateServerBinary(serverId, body);
      }
      showSuccess(t.binariesSaved);
      setEditor(null);
      reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function remove(target: ServerBinary) {
    setBusy(true);
    try {
      await api.deleteServerBinary(serverId, target.path);
      showSuccess(t.binariesDeleted);
      reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
      setDeleteTarget(null);
    }
  }

  const title =
    editor?.mode === 'new'
      ? t.binariesNewTitle
      : editor?.mode === 'override'
        ? t.binariesOverrideTitle
        : t.binariesEditTitle;

  return (
    <Panel
      titleId="runtime-binaries-heading"
      title={t.binariesTab}
      subtitle={t.binariesIntro}
      actions={
        <Button startIcon={<AddIcon />} onClick={() => setEditor(editorFor('new'))}>
          {t.binariesAdd}
        </Button>
      }
    >
      {loadError && <Alert severity="error">{`${t.binariesLoadFailed} ${loadError}`}</Alert>}
      {!loadError && data !== null && rows.length === 0 && (
        <Typography color="text.secondary">{t.binariesEmpty}</Typography>
      )}
      {rows.length > 0 && (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.binariesPath}</TableCell>
              <TableCell>{t.binariesKind}</TableCell>
              <TableCell>{t.binariesVersion}</TableCell>
              <TableCell>{t.binariesSource}</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((b) => {
              const manual = b.source === 'manual';
              return (
                <TableRow key={b.path}>
                  <TableCell sx={{ fontFamily: 'monospace' }}>
                    {b.path}
                    {!b.allowed && (
                      <Typography
                        component="div"
                        variant="caption"
                        color="warning.main"
                        sx={{ fontFamily: 'inherit' }}
                      >
                        {t.binariesNotAllowed}
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>{kindLabel(b.kind, t)}</TableCell>
                  <TableCell>
                    {b.effective_version || t.binariesVersionUnknown}
                    {b.version_override !== '' && (
                      <Typography component="span" variant="caption" color="text.secondary">
                        {` (${t.binariesOverridden})`}
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>{manual ? t.binariesSourceManual : t.binariesSourceAgent}</TableCell>
                  <TableCell align="right">
                    {manual ? (
                      <>
                        <IconButton
                          size="small"
                          aria-label={`${t.binariesEdit}: ${b.path}`}
                          onClick={() => setEditor(editorFor('manual', b))}
                        >
                          <EditIcon fontSize="small" />
                        </IconButton>
                        <IconButton
                          size="small"
                          aria-label={`${t.binariesDelete}: ${b.path}`}
                          onClick={() => setDeleteTarget(b)}
                        >
                          <DeleteIcon fontSize="small" />
                        </IconButton>
                      </>
                    ) : (
                      <IconButton
                        size="small"
                        aria-label={`${t.binariesOverrideAction}: ${b.path}`}
                        onClick={() => setEditor(editorFor('override', b))}
                      >
                        <TuneIcon fontSize="small" />
                      </IconButton>
                    )}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}

      {editor && (
        <Box
          component="form"
          aria-label={title}
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
          sx={{ display: 'grid', gap: 1.5, mt: 2, maxWidth: 560 }}
        >
          <Typography variant="subtitle1" component="h3">
            {title}
          </Typography>
          <Field
            id="binary-path"
            label={t.binariesPath}
            value={editor.path}
            readOnly={editor.mode !== 'new'}
            onChange={(e) => patch({ path: e.target.value })}
            required
          />
          {editor.mode === 'override' ? (
            <Field
              id="binary-override"
              label={t.binariesOverride}
              value={editor.override}
              onChange={(e) => patch({ override: e.target.value })}
            />
          ) : (
            <>
              <SelectField
                id="binary-kind"
                label={t.binariesKind}
                value={editor.kind}
                onChange={(e) => patch({ kind: e.target.value as ServerBinaryKind })}
              >
                <option value="llama_server">{t.binariesKindLlamaServer}</option>
                <option value="rpc_server">{t.binariesKindRpcServer}</option>
                <option value="other">{t.binariesKindOther}</option>
              </SelectField>
              <Field
                id="binary-version"
                label={t.binariesVersion}
                value={editor.version}
                onChange={(e) => patch({ version: e.target.value })}
              />
              <Field
                id="binary-build"
                label={t.binariesBuild}
                value={editor.build}
                onChange={(e) => patch({ build: e.target.value })}
              />
              <Field
                id="binary-commit"
                label={t.binariesCommit}
                value={editor.commit}
                onChange={(e) => patch({ commit: e.target.value })}
              />
            </>
          )}
          <Box sx={{ display: 'flex', gap: 1 }}>
            <Button type="submit" variant="contained" disabled={busy}>
              {t.save}
            </Button>
            <Button onClick={() => setEditor(null)}>{t.cancel}</Button>
          </Box>
        </Box>
      )}

      <ConfirmDialog
        open={deleteTarget !== null}
        title={t.binariesDeleteTitle}
        body={deleteTarget?.path}
        confirmLabel={t.binariesDeleteConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => deleteTarget && void remove(deleteTarget)}
        onCancel={() => setDeleteTarget(null)}
      />
    </Panel>
  );
}
