// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { TextField } from '@mui/material';
import type { TextFieldProps } from '@mui/material';

type FieldProps = {
  id: string;
  label?: string;
  value: string;
  onChange: TextFieldProps['onChange'];
  type?: string;
  required?: boolean;
  autoComplete?: string;
  /** Native <input> attributes (e.g. `{ "aria-label": ... }`). Forwarded to the input. */
  inputProps?: Record<string, unknown>;
} & Omit<
  TextFieldProps,
  | 'id'
  | 'label'
  | 'value'
  | 'onChange'
  | 'type'
  | 'required'
  | 'autoComplete'
  | 'select'
  | 'inputProps'
  | 'slotProps'
>;

/**
 * MUI-TextField-Wrapper. `id` landet auf dem Input und dem InputLabel-htmlFor,
 * daher löst getByLabelText(label) auf den Input auf und `#id` selektiert ihn.
 * `label` ist OPTIONAL: für Inline-Edit-Zellen weglassen und stattdessen einen
 * Accessible Name via `inputProps={{ "aria-label": ... }}` setzen (auf das native
 * <input> via slotProps.htmlInput weitergereicht); MUI rendert dann kein sichtbares Label.
 */
export function Field({
  id,
  label,
  type = 'text',
  value,
  onChange,
  required,
  autoComplete,
  inputProps,
  ...rest
}: FieldProps) {
  return (
    <TextField
      id={id}
      label={label}
      type={type}
      value={value}
      onChange={onChange}
      required={required}
      autoComplete={autoComplete}
      size="small"
      fullWidth
      slotProps={{
        // Keep `required` on the native <input> but suppress the visible label
        // asterisk so getByLabelText(label) still matches the exact label text.
        inputLabel: required ? { required: false } : undefined,
        ...(inputProps ? { htmlInput: inputProps } : {}),
      }}
      {...rest}
    />
  );
}
