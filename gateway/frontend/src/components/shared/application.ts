// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationStatus } from '../../api';
import type { MessageKey } from './types';

export const applicationStatusOptions: ApplicationStatus[] = ['active', 'disabled'];

export const applicationStatusLabelByKey: Record<ApplicationStatus, MessageKey> = {
  active: 'statusActive',
  disabled: 'statusDisabled',
};
