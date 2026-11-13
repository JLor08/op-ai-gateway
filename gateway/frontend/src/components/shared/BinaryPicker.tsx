// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// A path field that offers the server's binary inventory (path, kind,
// effective version, "not allowed by the agent") and still accepts any typed
// path: an unknown path saves without a version.

import { useEffect, useState } from 'react';
import { Autocomplete, Box, TextField, Typography } from '@mui/material';
import type { ServerBinary } from '../../api';
import type { Translation } from './types';

type PickerKind = 'llama_server' | 'rpc_server';

/**
 * Loads a server's binary inventory for a picker. A failed load (or an api
 * that cannot list) yields an empty inventory: the picker then degrades to a
 * plain path field, which is always valid input.
 */
export function useServerBinaries(
  api: { serverBinaries?: (serverId: string) => Promise<{ data: ServerBinary[] }> },
  serverId: string,
): ServerBinary[] {
  const [state, setState] = useState<{ serverId: string; rows: ServerBinary[] }>({
    serverId,
    rows: [],
  });
  useEffect(() => {
    let cancelled = false;
    api
      .serverBinaries?.(serverId)
      .then((res) => {
        if (!cancelled) setState({ serverId, rows: res.data });
      })
      .catch(() => {
        if (!cancelled) setState({ serverId, rows: [] });
      });
    return () => {
      cancelled = true;
    };
  }, [api, serverId]);
  // Rows of another server never show while the new server's load is pending.
  return state.serverId === serverId ? state.rows : [];
}

/** The version the gateway acts on for `path`, '' when unknown or not listed. */
export function binaryVersionFor(binaries: ServerBinary[], path: string): string {
  return binaries.find((b) => b.path === path)?.effective_version ?? '';
}

const isWsl = (b: ServerBinary) => b.execution === 'wsl';

function optionLabel(o: string | ServerBinary): string {
  if (typeof o === 'string') return o;
  if (isWsl(o)) return o.exec_path;
  return o.path;
}

export function BinaryPicker({
  id,
  label,
  t,
  kind,
  value,
  onChange,
  onSelect,
  binaries,
  error,
  helperText,
  required,
  placeholder,
}: Readonly<{
  id: string;
  label: string;
  t: Translation;
  // llama_server also offers "other" binaries (sd-server, whisper-server, ...).
  kind: PickerKind;
  value: string;
  onChange: (value: string) => void;
  // Called when an inventory option (not free text) is chosen, so the form can
  // also take the row's execution mode and distro.
  onSelect?: (binary: ServerBinary) => void;
  binaries: ServerBinary[];
  error?: boolean;
  helperText?: string;
  required?: boolean;
  placeholder?: string;
}>) {
  // llama_server offers llama_server, vllm and other binaries; the RPC member
  // picker only its own kind and never a WSL entry.
  const options = binaries.filter((b) =>
    kind === 'rpc_server'
      ? b.kind === 'rpc_server' && !isWsl(b)
      : b.kind === 'llama_server' || b.kind === 'vllm' || b.kind === 'other',
  );
  return (
    <Autocomplete<ServerBinary, false, false, true>
      id={id}
      freeSolo
      size="small"
      fullWidth
      options={options}
      inputValue={value}
      onInputChange={(_e, next) => onChange(next)}
      onChange={(_e, picked) => {
        if (typeof picked !== 'string' && picked !== null) onSelect?.(picked);
      }}
      // MUI keys options by label by default; two rows can share a Linux path
      // (two distros, or a native and a WSL row), but `path` is unique.
      getOptionKey={(o) => (typeof o === 'string' ? o : o.path)}
      // A WSL option's label is its Linux path: MUI writes the label into the
      // input after a selection, and the spec's binary is that Linux path.
      getOptionLabel={optionLabel}
      isOptionEqualToValue={(o, v) => o.path === (typeof v === 'string' ? v : v.path)}
      renderOption={(props, option) => {
        const { key, ...rest } = props as { key?: string } & Record<string, unknown>;
        return (
          <Box
            component="li"
            key={key}
            {...rest}
            sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}
          >
            <Box component="span" sx={{ fontFamily: 'monospace' }}>
              {isWsl(option) ? option.exec_path : option.path}
            </Box>
            {isWsl(option) && (
              <Typography component="span" variant="caption" color="text.secondary">
                {`WSL2 · ${option.distro}`}
              </Typography>
            )}
            <Typography component="span" variant="caption" color="text.secondary">
              {option.effective_version || t.binariesVersionUnknown}
            </Typography>
            {!option.allowed && (
              <Typography component="span" variant="caption" color="warning.main">
                {t.binariesNotAllowed}
              </Typography>
            )}
          </Box>
        );
      }}
      renderInput={(params) => (
        <TextField
          {...params}
          label={label}
          required={required}
          placeholder={placeholder}
          error={error}
          helperText={helperText ?? (options.length > 0 ? t.binaryPickerHint : undefined)}
          // Keep `required` on the native input but hide the label asterisk, as
          // Field does for every other required field.
          slotProps={{
            ...params.slotProps,
            inputLabel: { ...params.slotProps?.inputLabel, required: false },
          }}
        />
      )}
    />
  );
}
