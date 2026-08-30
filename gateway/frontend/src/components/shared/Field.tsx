// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { TextField } from '@mui/material';
import type { TextFieldProps, Theme } from '@mui/material';

type FieldProps = {
  id: string;
  label?: string;
  value: string;
  onChange: TextFieldProps['onChange'];
  type?: string;
  required?: boolean;
  autoComplete?: string;
  /**
   * Renders the field as read-only: sets the native `readOnly` on the input
   * AND strips the editable outline chrome (see `readOnlyOutlineSx`). Prefer
   * this over `inputProps={{ readOnly: true }}`, which leaves the field looking
   * editable. Read-only (not `disabled`): the field stays in the tab order and
   * legible to assistive tech — its whole job is to be READ.
   */
  readOnly?: boolean;
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
 * BUG 1 fix (unconditional). An empty outlined field, on its FIRST focus,
 * briefly paints the 2px focus border straight across the shrinking label:
 * MUI moves the label up with `duration.shorter` and NO delay, but opens the
 * notch legend (`max-width: 100%`) with a 50ms DELAY (NotchedOutline.js, the
 * `withLabel && notched` variant), so for ~50ms the label has risen onto the
 * top border line while the notch is still closed. Removing the legend's
 * transition opens the gap in lockstep with focus, so the label never rises
 * onto a solid border. Steady-state notch geometry is unchanged (the legend
 * still reaches its full width; only the animation to it is dropped), and it
 * is safe for single-line and label-less fields, which share the same flaw.
 */
const notchLockstepSx = {
  '& .MuiOutlinedInput-notchedOutline legend': { transition: 'none' },
} as const;

/**
 * BUG 2 fix (only when `readOnly`). A read-only outlined input otherwise keeps
 * the editable chrome: `inputProps={{ readOnly: true }}` marks only the inner
 * <input>, while MUI's hover-darken and `Mui-focused` 2px brand ring live on
 * the ROOT and key on hover/focus, never on read-only. Pin the notched outline
 * to its resting colour and width across hover and focus so the field reads as
 * static. Keyed on the PROP, never a label — an editable field (the mapping's
 * gateway-name rename, every other field) passes no `readOnly` and is
 * untouched, keeping its hover border and brand focus ring. Mode-aware:
 * hard-coding the light rgba would be wrong in dark mode. Mirrors MUI's own
 * default resting colour (OutlinedInput.js).
 */
const readOnlyOutlineSx = (theme: Theme) => {
  const resting =
    theme.palette.mode === 'dark' ? 'rgba(255, 255, 255, 0.23)' : 'rgba(0, 0, 0, 0.23)';
  return {
    '& .MuiOutlinedInput-root:hover .MuiOutlinedInput-notchedOutline': { borderColor: resting },
    // borderWidth: 1 undoes MUI's `Mui-focused` borderWidth: 2.
    '& .MuiOutlinedInput-root.Mui-focused .MuiOutlinedInput-notchedOutline': {
      borderColor: resting,
      borderWidth: 1,
    },
    // The label otherwise turns brand-green on focus (MUI's `Mui-focused`
    // label colour), which suggests "editable" exactly as the ring did. Pin it
    // to the resting label colour so a read-only field reads static on every
    // axis, not just the border. `text.secondary` is MUI's own resting
    // InputLabel colour, so this is mode-aware for free.
    '& .MuiInputLabel-root.Mui-focused': { color: theme.palette.text.secondary },
  };
};

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
  readOnly,
  inputProps,
  sx,
  ...rest
}: FieldProps) {
  // Fold the native readOnly onto the htmlInput slot so `value` cannot be typed
  // over, keeping any caller inputProps (e.g. an aria-label).
  const htmlInput = readOnly ? { ...inputProps, readOnly: true } : inputProps;
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
      // A structural marker the read-only visual keys off — jsdom can assert
      // it (and its absence on editable fields) even though it cannot see the
      // pixel treatment itself.
      {...(readOnly ? { 'data-readonly': 'true' } : {})}
      slotProps={{
        // Keep `required` on the native <input> but suppress the visible label
        // asterisk so getByLabelText(label) still matches the exact label text.
        inputLabel: required ? { required: false } : undefined,
        ...(htmlInput ? { htmlInput } : {}),
      }}
      // Merge (never overwrite) any caller sx: the array form lets the notch
      // fix, the read-only pin, and the caller's own styles all apply.
      sx={[
        notchLockstepSx,
        readOnly ? readOnlyOutlineSx : false,
        ...(Array.isArray(sx) ? sx : sx ? [sx] : []),
      ]}
      {...rest}
    />
  );
}
