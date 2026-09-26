// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  Checkbox,
  FormControl,
  FormControlLabel,
  FormGroup,
  FormHelperText,
  FormLabel,
} from '@mui/material';
import { useId, type Ref } from 'react';

/**
 * fieldset/legend => role=group, benannt durch die Legende. `onToggle` meldet nur
 * den umgeschalteten Wert; der Aufrufer besitzt `selected` und die Dirty-Flag-Logik.
 * Optionales `ariaLabel` setzt das fieldset-aria-label (überschreibt die Legende für
 * den Accessible Name), sodass die Legende kurz/sichtbar bleiben kann.
 * Optionales `error` zeigt eine Validierungsmeldung unter der Gruppe, markiert sie
 * als fehlerhaft und verknüpft die Meldung per aria-describedby mit dem fieldset,
 * sodass Hilfstechnik sie zusammen mit der Gruppe ansagt.
 * Optionales `groupRef` reicht das fieldset heraus und macht es per Skript
 * fokussierbar (tabIndex -1, nicht in der Tab-Reihenfolge): Ein Aufrufer, der
 * einen Save wegen dieser Gruppe ablehnt, setzt den Fokus darauf, sodass die
 * Gruppe samt Meldung angesagt wird.
 */
export function CheckboxGroup({
  legend,
  ariaLabel,
  options,
  selected,
  onToggle,
  error,
  groupRef,
}: Readonly<{
  legend: string;
  ariaLabel?: string;
  options: { value: string; label: string }[];
  selected: string[];
  onToggle: (value: string) => void;
  error?: string;
  groupRef?: Ref<HTMLFieldSetElement>;
}>) {
  const errorId = useId();
  return (
    <FormControl
      component="fieldset"
      ref={groupRef}
      tabIndex={groupRef ? -1 : undefined}
      aria-label={ariaLabel}
      aria-describedby={error ? errorId : undefined}
      error={!!error}
      sx={{ m: 0, p: 0, border: 0 }}
    >
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
      {error ? <FormHelperText id={errorId}>{error}</FormHelperText> : null}
    </FormControl>
  );
}
