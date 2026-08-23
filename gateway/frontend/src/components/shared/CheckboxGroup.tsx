// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Checkbox, FormControl, FormControlLabel, FormGroup, FormLabel } from '@mui/material';

/**
 * fieldset/legend => role=group, benannt durch die Legende. `onToggle` meldet nur
 * den umgeschalteten Wert; der Aufrufer besitzt `selected` und die Dirty-Flag-Logik.
 * Optionales `ariaLabel` setzt das fieldset-aria-label (überschreibt die Legende für
 * den Accessible Name), sodass die Legende kurz/sichtbar bleiben kann.
 */
export function CheckboxGroup({
  legend,
  ariaLabel,
  options,
  selected,
  onToggle,
}: Readonly<{
  legend: string;
  ariaLabel?: string;
  options: { value: string; label: string }[];
  selected: string[];
  onToggle: (value: string) => void;
}>) {
  return (
    <FormControl component="fieldset" aria-label={ariaLabel} sx={{ m: 0, p: 0, border: 0 }}>
      <FormLabel component="legend">{legend}</FormLabel>
      <FormGroup sx={{ flexDirection: 'row', flexWrap: 'wrap', gap: 1.5 }}>
        {options.map((o) => (
          <FormControlLabel
            key={o.value}
            control={
              <Checkbox checked={selected.includes(o.value)} onChange={() => onToggle(o.value)} />
            }
            label={o.label}
          />
        ))}
      </FormGroup>
    </FormControl>
  );
}
