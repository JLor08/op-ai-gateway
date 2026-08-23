// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Children, isValidElement, type ReactNode } from 'react';
import { MenuItem, TextField } from '@mui/material';
import type { TextFieldProps } from '@mui/material';

type SelectFieldProps = {
  id: string;
  label?: string;
  value: string;
  onChange: TextFieldProps['onChange'];
  children: ReactNode;
  /** Accessible name for a label-less (inline) select, forwarded as aria-label. */
  inputProps?: Record<string, unknown>;
} & Omit<
  TextFieldProps,
  'id' | 'label' | 'value' | 'onChange' | 'select' | 'slotProps' | 'children' | 'inputProps'
>;

type OptionProps = { value?: string | number; children?: ReactNode; disabled?: boolean };

/**
 * App-consistent single-select: a MUI `Select` (non-native) so its dropdown popup
 * matches the rest of the portal instead of the OS-native `<select>` popup.
 *
 * Call sites keep passing raw `<option value=...>` children (the API — and every
 * existing usage — is unchanged); this component transparently maps them to
 * `<MenuItem>` for the MUI menu. `value`/`onChange` behave exactly like a
 * TextField select (`event.target.value`). `inputProps` (e.g. an `aria-label`)
 * is forwarded to the select's input for label-less inline uses.
 */
export function SelectField({
  id,
  label,
  value,
  onChange,
  children,
  inputProps,
  ...rest
}: SelectFieldProps) {
  const items = Children.toArray(children).map((child, index) => {
    if (isValidElement(child) && child.type === 'option') {
      const { value: optValue, children: optLabel, disabled } = child.props as OptionProps;
      return (
        <MenuItem
          key={child.key ?? String(optValue ?? index)}
          value={optValue ?? ''}
          disabled={disabled}
        >
          {optLabel}
        </MenuItem>
      );
    }
    return child;
  });

  return (
    <TextField
      id={id}
      label={label}
      value={value}
      onChange={onChange}
      select
      // Keep the label shrunk + render empty values so a "" placeholder option
      // never overlaps the floating label.
      slotProps={{
        select: { native: false, displayEmpty: true, ...(inputProps ? { inputProps } : {}) },
        inputLabel: { shrink: true },
      }}
      size="small"
      fullWidth
      {...rest}
    >
      {items}
    </TextField>
  );
}
