// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Single source of truth for "who may see which view": one registry entry per
// View id carries both the NAV gate (does the sidebar item show?) and the
// CONTENT gate (does the main area render it?) as the same `gate` function,
// plus the `render` that produces its content. NavSidebar filters its items
// with `gate`; App looks up the current view's entry and, if its gate holds,
// calls `render` — otherwise falls back to the dashboard entry (see App.tsx).
// Adding a view = one entry here + the `View` union member in shared/types.

import type { Dispatch, ReactNode, SetStateAction } from 'react';
import { Box } from '@mui/material';
import {
  Activity as ActivityIcon,
  Bot,
  Boxes,
  Cpu,
  FolderKanban,
  KeyRound,
  LayoutDashboard,
  MessageSquare,
  Network,
  ScrollText,
  Server,
  ShieldCheck,
  SlidersHorizontal,
  Users,
  UsersRound,
  Wrench,
  type LucideIcon,
} from 'lucide-react';
import type { DashboardResponse, ModelOption, PortalServer, PortalToken } from '../api';
import type { Locale } from '../i18n';
import type { MessageKey, NavItem, PortalApi, Translation, View } from './shared/types';
import { Dashboard } from './Dashboard';
import { Chat } from './Chat';
import { TokenList } from './TokenList';
import { Activity } from './Activity';
import { ModelList } from './ModelList';
import { ModelGroupSection } from './ModelGroupSection';
import { ServerList } from './ServerList';
import { ResourceGroupsView } from './ResourceGroupsView';
import { ServicesView } from './ServicesView';
import { UsersView } from './UsersView';
import { GroupsView } from './GroupsView';
import { ProjectsView } from './ProjectsView';
import { ManagementView } from './ManagementView';
import { ToolsView } from './ToolsView';
import { SystemSettings } from './SystemSettings';
import { NetbirdSettings } from './NetbirdSettings';
import { CertificateSettings } from './CertificateSettings';
import { LogsView } from './LogsView';
import { LegalPages } from './legal/LegalPages';
import { SectionStub } from './SectionStub';

// Flags a `gate` reads to decide whether a view (nav item + content) is
// currently allowed. Kept minimal and boolean-only; both NavSidebar and App
// build one of these from the same underlying state.
export type ViewGateCtx = {
  isAdmin: boolean;
  systemAdminMode: boolean;
  netbirdModuleEnabled: boolean;
  certificatesModuleEnabled: boolean;
};

// Everything a `render` needs to produce a view's content. Extends
// ViewGateCtx since several renders also branch on the same flags (e.g. the
// admin/non-admin split on `users`, or `isAdmin` picking manageModels).
export type ViewRenderCtx = ViewGateCtx & {
  t: Translation;
  api: PortalApi;
  locale: Locale;
  role: string;
  userId: string;
  dashboard: DashboardResponse | null;
  productName: string;
  tokens: PortalToken[];
  setTokens: Dispatch<SetStateAction<PortalToken[]>>;
  models: ModelOption[];
  manageModels: ModelOption[];
  servers: PortalServer[];
  setServers: Dispatch<SetStateAction<PortalServer[]>>;
  loading: boolean;
  modelDetailOpen: boolean;
  setModelDetailOpen: (open: boolean) => void;
  onModelsChanged: () => void;
  onActivityUnauthorized: () => void;
  onSystemSettingsSaved: () => void;
  onSelectLocale: (locale: Locale) => void;
};

export type ViewEntry = {
  id: View;
  // labelKey/icon/href are present only for views reachable from the nav
  // sidebar; the remaining views (management, the legal pages) are reached
  // via other UI (profile menu, footer links) and carry none of the three.
  labelKey?: MessageKey;
  icon?: LucideIcon;
  href?: string;
  gate: (ctx: ViewGateCtx) => boolean;
  render: (ctx: ViewRenderCtx) => ReactNode;
};

const alwaysVisible: ViewEntry['gate'] = () => true;

// Keyed by View so TypeScript enforces exhaustiveness (a new View union
// member that's missing here is a compile error). Insertion order doubles as
// nav display order for the entries that have an `href` — see `navItems`
// below.
export const viewRegistry: Record<View, ViewEntry> = {
  dashboard: {
    id: 'dashboard',
    labelKey: 'dashboard',
    href: '/dashboard',
    icon: LayoutDashboard,
    gate: alwaysVisible,
    render: (ctx) => (
      <Dashboard t={ctx.t} dashboard={ctx.dashboard} productName={ctx.productName} />
    ),
  },
  chat: {
    id: 'chat',
    labelKey: 'chat',
    href: '/chat',
    icon: MessageSquare,
    gate: alwaysVisible,
    render: (ctx) => <Chat t={ctx.t} />,
  },
  tokens: {
    id: 'tokens',
    labelKey: 'apiTokens',
    href: '/api-tokens',
    icon: KeyRound,
    gate: alwaysVisible,
    render: (ctx) => (
      <TokenList
        t={ctx.t}
        api={ctx.api}
        tokens={ctx.tokens}
        setTokens={ctx.setTokens}
        role={ctx.role}
        models={ctx.models}
        servers={ctx.servers}
        loading={ctx.loading}
      />
    ),
  },
  projects: {
    id: 'projects',
    labelKey: 'projects',
    href: '/projects',
    icon: FolderKanban,
    gate: alwaysVisible,
    render: (ctx) => <ProjectsView t={ctx.t} api={ctx.api} role={ctx.role} userId={ctx.userId} />,
  },
  usage: {
    id: 'usage',
    labelKey: 'usage',
    href: '/usage',
    icon: ActivityIcon,
    gate: alwaysVisible,
    render: (ctx) => (
      <Activity
        t={ctx.t}
        api={ctx.api}
        role={ctx.role}
        onUnauthorized={ctx.onActivityUnauthorized}
      />
    ),
  },
  models: {
    id: 'models',
    labelKey: 'models',
    href: '/models',
    icon: Cpu,
    gate: alwaysVisible,
    render: (ctx) => (
      <Box sx={{ display: 'grid', gap: 3 }}>
        <ModelList
          t={ctx.t}
          models={ctx.isAdmin ? ctx.manageModels : ctx.models}
          api={ctx.api}
          isAdmin={ctx.isAdmin}
          loading={ctx.loading}
          onModelsChanged={ctx.onModelsChanged}
          onDetailViewChange={ctx.setModelDetailOpen}
        />
        {ctx.isAdmin && !ctx.modelDetailOpen && (
          <ModelGroupSection
            t={ctx.t}
            api={ctx.api}
            models={ctx.manageModels}
            onModelsChanged={ctx.onModelsChanged}
          />
        )}
      </Box>
    ),
  },
  servers: {
    id: 'servers',
    labelKey: 'servers',
    href: '/servers',
    icon: Server,
    gate: alwaysVisible,
    render: (ctx) => (
      <ServerList
        t={ctx.t}
        api={ctx.api}
        servers={ctx.servers}
        setServers={ctx.setServers}
        role={ctx.role}
        isSystemAdmin={ctx.systemAdminMode}
        onModelsChanged={ctx.onModelsChanged}
        loading={ctx.loading}
      />
    ),
  },
  resourceGroups: {
    id: 'resourceGroups',
    labelKey: 'resourceGroups',
    href: '/resource-groups',
    icon: Boxes,
    gate: (ctx) => ctx.isAdmin,
    render: (ctx) => <ResourceGroupsView t={ctx.t} api={ctx.api} role={ctx.role} />,
  },
  services: {
    id: 'services',
    labelKey: 'services',
    href: '/services',
    icon: Bot,
    gate: alwaysVisible,
    render: (ctx) => (
      <ServicesView
        t={ctx.t}
        api={ctx.api}
        models={ctx.models}
        role={ctx.role}
        userId={ctx.userId}
      />
    ),
  },
  users: {
    id: 'users',
    labelKey: 'users',
    href: '/users',
    icon: Users,
    // Always visible (nav + content): admins get the full UsersView, everyone
    // else gets a stub. There is no "hidden" state for this view.
    gate: alwaysVisible,
    render: (ctx) =>
      ctx.isAdmin ? (
        <UsersView t={ctx.t} api={ctx.api} canAssignSystemAdmin={ctx.systemAdminMode} />
      ) : (
        <SectionStub title={ctx.t.users} t={ctx.t} />
      ),
  },
  groups: {
    id: 'groups',
    labelKey: 'groups',
    href: '/groups',
    icon: UsersRound,
    gate: alwaysVisible,
    render: (ctx) => (
      <GroupsView
        t={ctx.t}
        api={ctx.api}
        role={ctx.role}
        userId={ctx.userId}
        systemAdminMode={ctx.systemAdminMode}
      />
    ),
  },
  tools: {
    id: 'tools',
    labelKey: 'tools',
    href: '/tools',
    icon: Wrench,
    gate: (ctx) => ctx.isAdmin,
    render: (ctx) => <ToolsView t={ctx.t} api={ctx.api} />,
  },
  system: {
    id: 'system',
    labelKey: 'system',
    href: '/system',
    icon: SlidersHorizontal,
    gate: (ctx) => ctx.systemAdminMode,
    render: (ctx) => <SystemSettings t={ctx.t} api={ctx.api} onSaved={ctx.onSystemSettingsSaved} />,
  },
  netbird: {
    id: 'netbird',
    labelKey: 'settingsNetbirdTitle',
    href: '/netbird',
    icon: Network,
    gate: (ctx) => ctx.systemAdminMode && ctx.netbirdModuleEnabled,
    render: (ctx) => <NetbirdSettings t={ctx.t} api={ctx.api} />,
  },
  certificates: {
    id: 'certificates',
    labelKey: 'settingsCertificatesTitle',
    href: '/certificates',
    icon: ShieldCheck,
    gate: (ctx) => ctx.systemAdminMode && ctx.certificatesModuleEnabled,
    render: (ctx) => <CertificateSettings t={ctx.t} api={ctx.api} />,
  },
  logs: {
    id: 'logs',
    labelKey: 'logsNav',
    href: '/logs',
    icon: ScrollText,
    gate: (ctx) => ctx.systemAdminMode,
    render: (ctx) => <LogsView t={ctx.t} api={ctx.api} />,
  },
  // Not reachable from the nav sidebar (no labelKey/icon/href): `management`
  // opens from the user-profile menu item; the legal pages open from the
  // footer links. Their content gate is unconditional either way.
  management: {
    id: 'management',
    gate: alwaysVisible,
    render: (ctx) => (
      <ManagementView
        t={ctx.t}
        api={ctx.api}
        locale={ctx.locale}
        onSelectLocale={ctx.onSelectLocale}
      />
    ),
  },
  impressum: {
    id: 'impressum',
    gate: alwaysVisible,
    render: (ctx) => <LegalPages page="impressum" locale={ctx.locale} />,
  },
  nutzungsbedingungen: {
    id: 'nutzungsbedingungen',
    gate: alwaysVisible,
    render: (ctx) => <LegalPages page="nutzungsbedingungen" locale={ctx.locale} />,
  },
  datenschutz: {
    id: 'datenschutz',
    gate: alwaysVisible,
    render: (ctx) => <LegalPages page="datenschutz" locale={ctx.locale} />,
  },
};

// The sidebar's item list, derived from the registry in its declared order.
// Only entries that declare a href are nav-reachable.
export const navItems: NavItem[] = (Object.values(viewRegistry) as ViewEntry[])
  .filter(
    (entry): entry is ViewEntry & { labelKey: MessageKey; href: string; icon: LucideIcon } =>
      entry.href !== undefined && entry.labelKey !== undefined && entry.icon !== undefined,
  )
  .map((entry) => ({ id: entry.id, labelKey: entry.labelKey, href: entry.href, icon: entry.icon }));
