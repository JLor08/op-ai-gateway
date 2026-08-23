// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Alert, Box, Button, Typography } from '@mui/material';
import type { AdminGroupCandidate } from '../../api';
import { SelectField } from './SelectField';
import { MultiSelectField } from './MultiSelectField';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  type AdminGroupLinkageLabels,
} from './adminGroupLinkage';

/**
 * The EDIT-form admin-groups editor (Phase B/C, spec 2026-08-10; Resource
 * Groups Phase 1, spec 2026-08-11): renders the entity's current admin-group
 * linkage + its own Save button, shared by ServerList.tsx, ServicesView.tsx
 * and ResourceGroupsView.tsx (FV-1 -- extracted from three near-identical
 * JSX blocks).
 *
 * `ungrouped` is the create-style "choose a system group, then its admin
 * groups" recovery flow for an entity that has NO containment root yet
 * (pre-Phase-B/C migrated servers/services, system_group_id==""). It is
 * OPTIONAL: a resource group always has a root after create (>=1 admin
 * group is required at creation, which always derives a non-empty root --
 * see ResourceGroupsView's own comment), so ResourceGroupsView omits it and
 * always renders the fixed-root `fixedOptions` multi-select.
 *
 * `onSave` is called with the EFFECTIVE ids to persist (handling the
 * ungrouped-recovery auto-select the same way the picker does), so the
 * caller's save handler takes the ids as a parameter rather than closing
 * over a separately-derived "effective" value.
 */
export function AdminGroupsEditor({
  idPrefix,
  candidates,
  fixedOptions,
  adminGroupIds,
  onAdminGroupIdsChange,
  busy,
  onSave,
  labels,
  ungrouped,
}: Readonly<{
  idPrefix: string;
  candidates: AdminGroupCandidate[];
  fixedOptions: AdminGroupCandidate[];
  adminGroupIds: string[];
  onAdminGroupIdsChange: (ids: string[]) => void;
  busy: boolean;
  onSave: (ids: string[]) => void;
  labels: AdminGroupLinkageLabels & { saveLabel: string };
  ungrouped?: {
    isUngrouped: boolean;
    systemGroupId: string;
    onSystemGroupIdChange: (id: string) => void;
  };
}>) {
  const distinct = ungrouped ? distinctParentGroups(candidates) : [];
  let effectiveSystemGroupId = '';
  if (ungrouped !== undefined) {
    effectiveSystemGroupId = distinct.length === 1 ? distinct[0].id : ungrouped.systemGroupId;
  }
  const ungroupedCandidates = ungrouped
    ? candidatesUnderSystemGroup(candidates, distinct, effectiveSystemGroupId)
    : [];
  const ungroupedAdminGroupIds =
    ungroupedCandidates.length === 1 ? [ungroupedCandidates[0].id] : adminGroupIds;
  const effectiveAdminGroupIds = ungrouped?.isUngrouped ? ungroupedAdminGroupIds : adminGroupIds;

  let editor: ReactNode;
  if (ungrouped?.isUngrouped) {
    if (candidates.length === 0) {
      editor = <Alert severity="warning">{labels.noCandidatesHint}</Alert>;
    } else {
      let systemGroupField: ReactNode = null;
      if (distinct.length > 1) {
        systemGroupField = (
          <SelectField
            id={`${idPrefix}-system-group-edit`}
            label={labels.systemGroupLabel}
            value={ungrouped.systemGroupId}
            onChange={(e) => {
              ungrouped.onSystemGroupIdChange(e.target.value);
              onAdminGroupIdsChange([]);
            }}
          >
            <option value="" />
            {distinct.map((g) => (
              <option value={g.id} key={g.id}>
                {g.name}
              </option>
            ))}
          </SelectField>
        );
      } else if (distinct.length === 1) {
        systemGroupField = (
          <Typography variant="body2" color="text.secondary">
            {labels.systemGroupAuto(distinct[0].name)}
          </Typography>
        );
      }
      let adminGroupField: ReactNode = null;
      if (ungroupedCandidates.length === 1) {
        adminGroupField = (
          <Typography variant="body2" color="text.secondary">
            {labels.adminGroupAuto(ungroupedCandidates[0].name)}
          </Typography>
        );
      } else if (ungroupedCandidates.length > 1) {
        adminGroupField = (
          <MultiSelectField
            id={`${idPrefix}-admin-groups-edit`}
            label={labels.adminGroupLabel}
            options={ungroupedCandidates.map((c) => ({ value: c.id, label: c.name }))}
            selected={adminGroupIds}
            onChange={onAdminGroupIdsChange}
          />
        );
      }
      editor = (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
          {systemGroupField}
          {adminGroupField}
        </Box>
      );
    }
  } else {
    editor = (
      <MultiSelectField
        id={`${idPrefix}-admin-groups-edit`}
        label={labels.adminGroupLabel}
        options={fixedOptions.map((c) => ({ value: c.id, label: c.name }))}
        selected={adminGroupIds}
        onChange={onAdminGroupIdsChange}
      />
    );
  }

  return (
    <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
      {editor}
      <Box>
        <Button
          type="button"
          variant="contained"
          disabled={busy || effectiveAdminGroupIds.length === 0}
          onClick={() => onSave(effectiveAdminGroupIds)}
        >
          {labels.saveLabel}
        </Button>
      </Box>
    </Box>
  );
}
