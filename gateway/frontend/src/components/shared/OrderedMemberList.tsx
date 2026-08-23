// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, IconButton, Typography } from '@mui/material';
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import CloseIcon from '@mui/icons-material/Close';
import type { ModelOption } from '../../api';
import type { Translation } from './types';
import { SearchableSelect } from './SearchableSelect';
import { useColumnDrag, columnDragSx, moveColumn } from './columnDrag';

/**
 * Controlled ordered-member picker for a model group. `members` is the ordered
 * list of gateway model NAMES (array index = priority; index 0 = highest). Each
 * row is drag-reorderable (via useColumnDrag) and carries up/down + remove
 * buttons; an add control below appends a model OR another group (nested
 * groups are flattened per their own traversal strategy — see FlattenGroup),
 * lowest priority. Visibility is NOT edited here — it is a per-model property
 * (edited in the models list).
 */
export function OrderedMemberList({
  members,
  onChange,
  available,
  t,
  disabled = false,
  selfName,
}: Readonly<{
  members: string[];
  onChange: (members: string[]) => void;
  available: ModelOption[];
  t: Translation;
  disabled?: boolean;
  /**
   * The gateway_model_name of the group being created/edited (excluded from the
   * add-member options so a group can't be added as its own member). On create
   * this is the not-yet-saved name currently typed into the form; on edit it is
   * the existing group's name.
   */
  selfName?: string;
}>) {
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(
    (source, target, place) => onChange(moveColumn(members, source, target, place)),
    'vertical',
  );

  function swap(index: number, delta: number) {
    const target = index + delta;
    if (target < 0 || target >= members.length) return;
    const next = [...members];
    [next[index], next[target]] = [next[target], next[index]];
    onChange(next);
  }

  function remove(name: string) {
    onChange(members.filter((m) => m !== name));
  }

  // Add options: any model OR group (a group member is expanded per its own
  // traversal strategy — see FlattenGroup), excluding the group being edited
  // itself (self-reference is the obvious cycle case the client can block
  // outright; a deeper indirect cycle is rejected by the backend with a 400)
  // and members already added.
  const selfLower = (selfName ?? '').toLowerCase();
  const addOptions = available
    .filter((m) => m.id.toLowerCase() !== selfLower && !members.includes(m.id))
    .map((m) => ({
      value: m.id,
      label: (m.display_name || m.id) + (m.is_group ? ` (${t.modelGroupChip})` : ''),
    }));

  return (
    <Box sx={{ display: 'grid', gap: 1 }}>
      <Typography variant="subtitle2" component="h3">
        {t.modelGroupMembers}
      </Typography>
      <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0, display: 'grid', gap: 0.5 }}>
        {members.map((name, index) => (
          <Box
            component="li"
            key={name}
            {...dragProps(name)}
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: 0.5,
              px: 1,
              py: 0.5,
              border: '1px solid var(--line)',
              borderRadius: 1,
              ...columnDragSx(name, draggingId, overId, overPlace, 'vertical'),
            }}
          >
            <DragIndicatorIcon
              fontSize="small"
              sx={{ color: 'text.secondary', cursor: 'grab' }}
              aria-hidden
            />
            <Typography
              component="span"
              sx={{ flexGrow: 1, minWidth: 0, overflowWrap: 'anywhere' }}
            >
              {name}
            </Typography>
            <IconButton
              size="small"
              aria-label={`${t.modelGroupMoveUp}: ${name}`}
              disabled={disabled || index === 0}
              onClick={() => swap(index, -1)}
            >
              <ArrowUpwardIcon fontSize="small" />
            </IconButton>
            <IconButton
              size="small"
              aria-label={`${t.modelGroupMoveDown}: ${name}`}
              disabled={disabled || index === members.length - 1}
              onClick={() => swap(index, 1)}
            >
              <ArrowDownwardIcon fontSize="small" />
            </IconButton>
            <IconButton
              size="small"
              color="error"
              aria-label={`${t.modelGroupRemoveMember}: ${name}`}
              disabled={disabled}
              onClick={() => remove(name)}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Box>
        ))}
      </Box>
      <SearchableSelect
        id="model-group-add-member"
        label={t.modelGroupAddMember}
        value=""
        onChange={(value) => {
          if (value && !members.includes(value)) onChange([...members, value]);
        }}
        options={addOptions}
        disabled={disabled}
      />
    </Box>
  );
}
