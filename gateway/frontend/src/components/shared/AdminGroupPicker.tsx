// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Alert, Box, Typography } from '@mui/material';
import type { AdminGroupCandidate } from '../../api';
import { SelectField } from './SelectField';
import { MultiSelectField } from './MultiSelectField';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  type AdminGroupLinkageLabels,
} from './adminGroupLinkage';

/**
 * The CREATE-form admin-group linkage picker (Phase B/C, spec 2026-08-10;
 * Resource Groups Phase 1, spec 2026-08-11): shared by ServerList.tsx,
 * ServicesView.tsx and ResourceGroupsView.tsx (FV-1 -- extracted from three
 * identical JSX blocks). `candidates` is the caller's RAW candidate set;
 * this component derives the system-group step + the narrowed admin-group
 * set itself (via the same shared helpers the owning view uses for its own
 * submit-gating/payload, so the two derivations can never drift). `idPrefix`
 * preserves each view's existing aria ids (e.g. "server-system-group",
 * "service-admin-groups").
 */
export function AdminGroupPicker({
  idPrefix,
  candidates,
  systemGroupId,
  onSystemGroupIdChange,
  adminGroupIds,
  onAdminGroupIdsChange,
  labels,
}: Readonly<{
  idPrefix: string;
  candidates: AdminGroupCandidate[];
  systemGroupId: string;
  onSystemGroupIdChange: (id: string) => void;
  adminGroupIds: string[];
  onAdminGroupIdsChange: (ids: string[]) => void;
  labels: AdminGroupLinkageLabels;
}>) {
  const distinct = distinctParentGroups(candidates);
  const effectiveSystemGroupId = distinct.length === 1 ? distinct[0].id : systemGroupId;
  const effectiveCandidates = candidatesUnderSystemGroup(
    candidates,
    distinct,
    effectiveSystemGroupId,
  );

  if (candidates.length === 0) {
    return (
      <Box>
        <Alert severity="warning">{labels.noCandidatesHint}</Alert>
      </Box>
    );
  }

  let systemGroupField: ReactNode = null;
  if (distinct.length > 1) {
    systemGroupField = (
      <SelectField
        id={`${idPrefix}-system-group`}
        label={labels.systemGroupLabel}
        value={systemGroupId}
        onChange={(e) => {
          onSystemGroupIdChange(e.target.value);
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
  if (effectiveCandidates.length === 1) {
    adminGroupField = (
      <Typography variant="body2" color="text.secondary">
        {labels.adminGroupAuto(effectiveCandidates[0].name)}
      </Typography>
    );
  } else if (effectiveCandidates.length > 1) {
    adminGroupField = (
      <MultiSelectField
        id={`${idPrefix}-admin-groups`}
        label={labels.adminGroupLabel}
        options={effectiveCandidates.map((c) => ({ value: c.id, label: c.name }))}
        selected={adminGroupIds}
        onChange={onAdminGroupIdsChange}
      />
    );
  }

  return (
    <Box>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
        {systemGroupField}
        {adminGroupField}
      </Box>
    </Box>
  );
}
