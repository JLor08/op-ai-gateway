// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This module is a barrel: every DTO type + endpoint-method factory used to
// live in one ~3500-line file. They now live in domain modules under ./api/
// (transport primitives, auth, tokens, users, groups, resourceGroups,
// projects, servers, services, models, usage, system, netbird, chat); this
// file re-exports every one of them unchanged and composes createPortalApi
// by spreading each domain's factory. `PortalApi = ReturnType<typeof
// createPortalApi>` stays structurally identical to before the split, so
// every existing `import { X } from '../api'` site keeps compiling with no
// changes.

import { type Fetcher } from './api/transport';
import { authApi } from './api/auth';
import { tokensApi } from './api/tokens';
import { usersApi } from './api/users';
import { groupsApi } from './api/groups';
import { resourceGroupsApi } from './api/resourceGroups';
import { projectsApi } from './api/projects';
import { serversApi } from './api/servers';
import { servicesApi } from './api/services';
import { modelsApi } from './api/models';
import { usageApi } from './api/usage';
import { systemApi } from './api/system';
import { netbirdApi } from './api/netbird';
import { chatApi } from './api/chat';
import { runtimeApi } from './api/runtime';

export { PortalApiError, buildQueryString } from './api/transport';

export * from './api/auth';
export * from './api/tokens';
export * from './api/users';
export * from './api/groups';
export * from './api/resourceGroups';
export * from './api/projects';
export * from './api/servers';
export * from './api/services';
export * from './api/models';
export * from './api/usage';
export * from './api/system';
export * from './api/netbird';
export * from './api/chat';
export * from './api/runtime';

export function createPortalApi(fetcher: Fetcher = fetch) {
  return {
    ...authApi(fetcher),
    ...usersApi(fetcher),
    ...tokensApi(fetcher),
    ...groupsApi(fetcher),
    ...resourceGroupsApi(fetcher),
    ...projectsApi(fetcher),
    ...serversApi(fetcher),
    ...servicesApi(fetcher),
    ...modelsApi(fetcher),
    ...usageApi(fetcher),
    ...systemApi(fetcher),
    ...netbirdApi(fetcher),
    ...chatApi(fetcher),
    ...runtimeApi(fetcher),
  };
}

// The concrete client shape, so callers (e.g. ChatStoreProvider) can accept a
// `PortalApi` prop rather than re-deriving the return type at each site.
export type PortalApi = ReturnType<typeof createPortalApi>;
