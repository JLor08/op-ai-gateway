// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Autocomplete, Box, InputAdornment, TextField } from '@mui/material';
import PriorityHighIcon from '@mui/icons-material/PriorityHigh';

export type SearchableOption = {
  value: string;
  label: string;
  // When true, a small marker is shown before the label in the dropdown (used to
  // flag models that are currently loaded). `loadedTitle` is its accessible title.
  loaded?: boolean;
  loadedTitle?: string;
};

/**
 * Filter logic for the searchable select (exported for unit testing): show ALL
 * options while the input still equals the selected option's label (the field was
 * just opened and the user hasn't typed a real query yet); once they type
 * something else, keep options whose label contains the query (case-insensitive).
 */
export function matchOptions(
  options: SearchableOption[],
  inputValue: string,
  selectedLabel: string | null,
): SearchableOption[] {
  if (selectedLabel !== null && inputValue === selectedLabel) return options;
  const q = inputValue.trim().toLowerCase();
  return q ? options.filter((o) => o.label.toLowerCase().includes(q)) : options;
}

/**
 * App-consistent, searchable single-select (MUI Autocomplete). Type to filter the
 * options; picking one calls `onChange(value)`. Clearing selects the empty value
 * (`""`) so an explicit "none" option round-trips. `value`/`onChange` mirror a
 * plain select (string in, string out), so it drops into existing state wiring.
 */
export function SearchableSelect({
  id,
  label,
  value,
  onChange,
  options,
  disabled,
  helperText,
  placeholder,
  unavailable,
  unavailableTitle,
}: Readonly<{
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: SearchableOption[];
  disabled?: boolean;
  helperText?: string;
  placeholder?: string;
  // When true, a red warning "!" is shown BEFORE the value in the field (in place
  // of the loaded dot) — used to flag that the selected value is currently
  // unavailable. `unavailableTitle` is its accessible tooltip.
  unavailable?: boolean;
  unavailableTitle?: string;
}>) {
  const selected = options.find((o) => o.value === value) ?? null;
  return (
    <Autocomplete
      id={id}
      options={options}
      value={selected}
      disabled={disabled}
      fullWidth
      size="small"
      autoHighlight
      selectOnFocus
      getOptionLabel={(o) => o.label}
      isOptionEqualToValue={(o, v) => o.value === v.value}
      filterOptions={(opts, state) =>
        matchOptions(opts, state.inputValue, selected ? selected.label : null)
      }
      onChange={(_event, next) => onChange(next ? next.value : '')}
      // Let the dropdown popup grow to fit its widest option (so long model names
      // show in FULL, never clipped/ellipsised). MUI sizes the popper to the input
      // width via an inline `style.width`; overriding that style here wins per-key
      // (MUI merges style, keeping its pointer-events guard) — reliable, unlike a
      // popper.js modifier which React's per-render width would race.
      slotProps={{
        popper: {
          placement: 'bottom-start',
          style: { width: 'max-content', minWidth: 220, maxWidth: 'min(90vw, 640px)' },
        },
      }}
      renderOption={(props, option) => {
        // Render each option on a SINGLE line (no wrapping) and WITHOUT truncation:
        // the popup is sized to content (see slotProps.popper) so the whole name is
        // visible.
        const { key, ...rest } = props as { key?: string } & Record<string, unknown>;
        return (
          <Box
            component="li"
            key={key}
            {...rest}
            sx={{ display: 'flex', alignItems: 'center', gap: 0.75, whiteSpace: 'nowrap' }}
          >
            {option.loaded ? (
              // Decorative dot (loaded state is also in the title tooltip); aria-hidden
              // so it does not pollute the option's accessible name (which must stay the
              // bare model name for name-based lookups).
              <Box
                component="span"
                aria-hidden
                title={option.loadedTitle}
                sx={{
                  flex: '0 0 auto',
                  width: 8,
                  height: 8,
                  borderRadius: '50%',
                  bgcolor: 'success.main',
                }}
              />
            ) : null}
            <Box component="span">{option.label}</Box>
          </Box>
        );
      }}
      renderInput={(params) => {
        // A leading start-adornment BEFORE the name signals the value's state,
        // consistent with the dropdown: a red "!" when unavailable (takes
        // precedence — an unavailable value can't be loaded), else the green dot
        // when the selected option is loaded, else whatever MUI already supplied.
        let startAdornment: ReactNode = params.slotProps.input?.startAdornment;
        if (unavailable) {
          startAdornment = (
            <InputAdornment position="start" sx={{ ml: 0.5, mr: 0 }}>
              <PriorityHighIcon
                data-testid="searchable-select-unavailable"
                color="error"
                fontSize="small"
                titleAccess={unavailableTitle}
              />
            </InputAdornment>
          );
        } else if (selected?.loaded) {
          startAdornment = (
            <InputAdornment position="start" sx={{ ml: 0.5, mr: 0 }}>
              <Box
                component="span"
                data-testid="searchable-select-loaded-dot"
                aria-hidden
                title={selected.loadedTitle}
                sx={{
                  width: 8,
                  height: 8,
                  borderRadius: '50%',
                  bgcolor: 'success.main',
                  flex: '0 0 auto',
                }}
              />
            </InputAdornment>
          );
        }
        return (
          <TextField
            {...params}
            label={label}
            placeholder={placeholder}
            helperText={helperText}
            // The single-line input can only show as much as its width; carry the
            // full selected label as a native tooltip so the complete name is
            // always available.
            slotProps={{
              ...params.slotProps,
              htmlInput: {
                ...params.slotProps.htmlInput,
                title: selected ? selected.label : undefined,
              },
              input: {
                ...params.slotProps.input,
                startAdornment,
              },
            }}
          />
        );
      }}
    />
  );
}
