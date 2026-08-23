// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Autocomplete, TextField } from '@mui/material';

export type MultiSelectOption = { value: string; label: string; sublabel?: string };

/**
 * Searchable multi-select (MUI Autocomplete) with chips + type-ahead — the
 * scalable replacement for a long checkbox list (e.g. assigning owners from many
 * users). Fully controlled via `selected` (value ids) + `onChange`.
 */
export function MultiSelectField({
  id,
  label,
  options,
  selected,
  onChange,
  placeholder,
  disabled,
}: Readonly<{
  id: string;
  label: string;
  options: MultiSelectOption[];
  selected: string[];
  onChange: (values: string[]) => void;
  placeholder?: string;
  disabled?: boolean;
}>) {
  const byValue = new Map(options.map((o) => [o.value, o]));
  const value = selected
    .map((v) => byValue.get(v))
    .filter((o): o is MultiSelectOption => Boolean(o));
  return (
    <Autocomplete
      multiple
      id={id}
      size="small"
      disabled={disabled}
      options={options}
      value={value}
      onChange={(_event, next) => onChange(next.map((o) => o.value))}
      getOptionLabel={(o) => o.label}
      isOptionEqualToValue={(a, b) => a.value === b.value}
      filterSelectedOptions
      // Match against both the label and the sublabel (e.g. name + email).
      filterOptions={(opts, state) => {
        const q = state.inputValue.trim().toLowerCase();
        if (!q) return opts;
        return opts.filter(
          (o) => o.label.toLowerCase().includes(q) || (o.sublabel ?? '').toLowerCase().includes(q),
        );
      }}
      renderOption={(props, option) => {
        const { key, ...rest } = props as { key?: string } & Record<string, unknown>;
        return (
          <li key={option.value} {...rest}>
            <span>{option.label}</span>
            {option.sublabel && (
              <span style={{ marginLeft: 8, opacity: 0.6, fontSize: '0.85em' }}>
                {option.sublabel}
              </span>
            )}
          </li>
        );
      }}
      renderInput={(params) => (
        <TextField
          {...params}
          label={label}
          placeholder={value.length === 0 ? placeholder : undefined}
        />
      )}
    />
  );
}
