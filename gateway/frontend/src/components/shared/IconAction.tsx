// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { useId } from 'react';
import { IconButton, Tooltip } from '@mui/material';
import { visuallyHidden } from '@mui/utils';

/**
 * Zeilen-Aktion als Icon-Button mit Tooltip; `label` ist zugleich der accessible name.
 *
 * `title` is the optional "why is this disabled" hint (RowAction.title, which
 * RowActionsCell forwards here on the inline path). It takes precedence over
 * `label` in the tooltip because the reason is strictly more informative than
 * the name, which the icon and `aria-label` already carry.
 *
 * Disabled uses `aria-disabled`, NOT the `disabled` prop, and that is
 * load-bearing for accessibility (issue #26). A real `disabled` attribute makes
 * the control both unfocusable and pointer-inert, so the Tooltip that explains
 * WHY the action is refused reaches nobody who is not hovering with a mouse --
 * keyboard and screen-reader users meet an unexplained grey button, in the one
 * place the UI says why. With `aria-disabled` the control stays focusable and
 * discoverable, while `label` stays the stable accessible NAME (a rename would
 * break name-based lookups and read worse to AT).
 *
 * The reason is announced on FOCUS through a persistent, visually-hidden element
 * wired with `aria-describedby` -- present in the accessibility tree at all
 * times, so a screen reader reads it the instant the control takes focus, not
 * ~100ms later when the Tooltip's hover-delay would elapse (a `describeChild`
 * Tooltip alone associates its text only while open, which is too late for the
 * focus announcement). The Tooltip still carries the same text for sighted
 * users, on hover and on focus, with `describeChild` so it never competes with
 * the accessible name.
 *
 * Two things the `disabled` prop used to supply and this must now do itself:
 * `onClick` is dropped when disabled -- the control is no longer inert, so the
 * DOM will not swallow the click -- and the disabled LOOK (greyed, no ripple, no
 * hover affordance) is reapplied by hand.
 */
export function IconAction({
  label,
  icon,
  onClick,
  color,
  disabled,
  title,
}: Readonly<{
  label: string;
  icon: ReactNode;
  onClick: () => void;
  color?: 'inherit' | 'error';
  disabled?: boolean;
  title?: string;
}>) {
  const reasonId = useId();
  // A reason is announced only when the action is disabled AND a reason was
  // given -- the case issue #26 is about. An enabled action needs no description.
  const showReason = Boolean(disabled && title);
  return (
    <Tooltip title={title ?? label} describeChild>
      <IconButton
        size="small"
        aria-label={label}
        color={color}
        aria-disabled={disabled || undefined}
        aria-describedby={showReason ? reasonId : undefined}
        disableRipple={disabled || undefined}
        onClick={disabled ? undefined : onClick}
        sx={
          disabled
            ? {
                color: 'action.disabled',
                cursor: 'default',
                '&:hover': { backgroundColor: 'transparent' },
              }
            : undefined
        }
      >
        {icon}
        {showReason ? (
          <span id={reasonId} style={visuallyHidden}>
            {title}
          </span>
        ) : null}
      </IconButton>
    </Tooltip>
  );
}
