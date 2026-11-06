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

export function BinaryPicker({
  id,
  label,
  t,
  kind,
  value,
  onChange,
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
  binaries: ServerBinary[];
  error?: boolean;
  helperText?: string;
  required?: boolean;
  placeholder?: string;
}>) {
  const options = binaries.filter(
    (b) => b.kind === kind || (kind === 'llama_server' && b.kind === 'other'),
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
      getOptionLabel={(o) => (typeof o === 'string' ? o : o.path)}
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
              {option.path}
            </Box>
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
