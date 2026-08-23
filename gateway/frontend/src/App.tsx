// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  AppBar,
  Box,
  IconButton,
  Link,
  Toolbar,
  Tooltip,
  useMediaQuery,
  useTheme,
} from '@mui/material';
import MenuIcon from '@mui/icons-material/Menu';
import CloseIcon from '@mui/icons-material/Close';
import {
  createPortalApi,
  PortalApiError,
  type CurrentUser,
  type DashboardResponse,
  type ModelOption,
  type PortalServer,
  type PortalToken,
} from './api';
import type { Locale } from './i18n';
import { messages } from './i18n';
import type { View } from './components/shared/types';
import { formatPortalError } from './components/shared/format';
import { ChatStoreProvider } from './components/chat/ChatStore';
import { PreferencesProvider } from './components/shared/preferences';
import { ConnectionProvider } from './components/shared/connection';
import { Login } from './components/Login';
import { SetPassword } from './components/SetPassword';
import { navItems, viewRegistry, type ViewGateCtx, type ViewRenderCtx } from './components/views';
import { NavSidebar } from './components/NavSidebar';
import { SystemAdminModeControl } from './components/SystemAdminModeControl';
import { ColorModeMenu } from './theme/ColorModeMenu';
import { Brand } from './theme/Brand';
import { useThemeControls } from './theme/useThemeControls';
import { LanguageMenu } from './components/shared/LanguageMenu';
import { UserMenu } from './components/shared/UserMenu';

// Cadence for the live models-list poll while on the Models view. Offered/loaded/
// availability change on the 5–30s health-loop cadence, so 5s already leads them.
const MODELS_POLL_MS = 5000;

const toLocale = (value?: string): Locale => (value === 'en' ? 'en' : 'de');

export default function App() {
  const [locale, setLocale] = useState<Locale>('de');
  const [view, setView] = useState<View>('dashboard');
  const theme = useTheme();
  const { brand, productName } = useThemeControls();
  const isNarrow = useMediaQuery(theme.breakpoints.down('md'));
  const [navExpanded, setNavExpanded] = useState(!isNarrow);
  useEffect(() => {
    setNavExpanded(!isNarrow);
  }, [isNarrow]);
  const api = useMemo(() => createPortalApi(), []);
  const [authState, setAuthState] = useState<'loading' | 'login' | 'authenticated'>('loading');
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  const [dashboard, setDashboard] = useState<DashboardResponse | null>(null);
  const [tokens, setTokens] = useState<PortalToken[]>([]);
  const [models, setModels] = useState<ModelOption[]>([]);
  // manageModels is the UNSUPPRESSED listing (hidden/locked models included) for
  // the admin management surface (ModelList editor + ModelGroupSection picker).
  // The endpoint ignores ?manage=1 for non-admins, so for them it equals models.
  const [manageModels, setManageModels] = useState<ModelOption[]>([]);
  // True while ModelList shows a per-server model detail — hides the group table.
  const [modelDetailOpen, setModelDetailOpen] = useState(false);
  const [servers, setServers] = useState<PortalServer[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  // Bumped whenever System-Admin mode is entered/left. Used as the key on the
  // routed content so self-fetching views (GroupsView/UsersView/…) remount and
  // re-fetch under the new scope; prop-fed views get fresh data from the
  // accompanying loadPortalData (see the elevation effect below).
  const [contentEpoch, setContentEpoch] = useState(0);
  // Gates the NetBird nav item + view: only shown when the module is enabled AND
  // the current user is a system_admin (checked separately at the render gate).
  const [netbirdModuleEnabled, setNetbirdModuleEnabled] = useState(false);
  // Gates the "certificates" nav item + view the same way (module-enabled +
  // system_admin, checked separately at the render gate).
  const [certificatesModuleEnabled, setCertificatesModuleEnabled] = useState(false);
  const t = messages[locale];
  const tRef = useRef(t);
  tRef.current = t;
  const isAdmin = currentUser?.role === 'admin' || currentUser?.role === 'system_admin';
  // System-admin step-up mode: a system_admin session starts as a plain admin
  // (system scope withheld) until it elevates via SystemAdminModeControl. Every
  // capability gate that needs the `system` scope reads THIS, not `role`.
  const systemAdminMode = currentUser?.system_admin_mode ?? false;

  const setPasswordToken =
    typeof window !== 'undefined' &&
    window.location.pathname === `${import.meta.env.BASE_URL}set-password`
      ? new URLSearchParams(window.location.search).get('token')
      : null;

  const loadPortalData = useCallback(
    async (opts?: { silent?: boolean }) => {
      const silent = opts?.silent ?? false;
      if (!silent) setLoading(true);
      setError('');
      try {
        const [
          userResponse,
          dashboardResponse,
          tokenResponse,
          modelResponse,
          manageModelResponse,
          serverResponse,
        ] = await Promise.all([
          api.me(),
          api.dashboard(),
          api.tokens(),
          api.models(),
          api.manageModels(),
          api.servers(),
        ]);
        setCurrentUser(userResponse);
        setDashboard(dashboardResponse);
        setTokens(tokenResponse.data);
        setModels(modelResponse.data);
        setManageModels(manageModelResponse.data);
        setServers(serverResponse.data);
        setAuthState('authenticated');
      } catch (err) {
        if (err instanceof PortalApiError && err.status === 401) {
          setAuthState('login');
          return;
        }
        setError(formatPortalError(err, tRef.current));
      } finally {
        if (!silent) setLoading(false);
      }
    },
    [api],
  );

  const loadRef = useRef(loadPortalData);
  loadRef.current = loadPortalData;
  const didMountRef = useRef(false);
  useEffect(() => {
    if (!didMountRef.current) {
      didMountRef.current = true;
      return;
    }
    void loadRef.current({ silent: true });
  }, [view]);

  const refreshPortalDataSilently = useCallback(() => {
    void loadPortalData({ silent: true });
  }, [loadPortalData]);

  // Live Models view: poll models (+ manageModels for admins) every ~5s while the
  // Models view is shown, so Angeboten / Geladen / availability stay current. Silent
  // (does not touch `loading`); latest-wins + a cancel guard; a transient error keeps
  // the last-known lists. No immediate tick — navigation into the view already refetched.
  useEffect(() => {
    if (authState !== 'authenticated' || view !== 'models') return;
    let cancelled = false;
    let seq = 0;
    const tick = () => {
      const mine = ++seq;
      Promise.all([api.models(), isAdmin ? api.manageModels() : Promise.resolve(null)])
        .then(([m, mm]) => {
          if (cancelled || mine !== seq) return;
          setModels(m.data);
          if (mm) setManageModels(mm.data);
        })
        .catch(() => {
          /* non-blocking — keep the last-known lists on a transient error */
        });
    };
    const id = setInterval(tick, MODELS_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [api, authState, view, isAdmin]);

  // Lightweight models-only refresh for ChatStore's unavailable-model recovery
  // poll — avoids the heavier full loadPortalData (me/dashboard/tokens/servers)
  // on a 15s cadence. Best-effort: a failed poll just tries again next tick.
  const refreshModels = useCallback(() => {
    void api
      .models()
      .then((res) => setModels(res.data))
      .catch(() => {});
  }, [api]);

  const handleActivityAuthError = useCallback(() => {
    setAuthState('login');
  }, []);

  const bootstrap = useCallback(async () => {
    try {
      const sessionState = await api.session();
      if (!sessionState.authenticated) {
        setLocale(toLocale(sessionState.default_language));
        setAuthState('login');
        return;
      }
      if (sessionState.user) {
        setCurrentUser(sessionState.user);
        setLocale(toLocale(sessionState.user.preferred_language));
      }
      await loadPortalData();
    } catch {
      setAuthState('login');
    }
  }, [api, loadPortalData]);

  const selectLocale = useCallback(
    (next: Locale) => {
      setLocale(next);
      if (authState === 'authenticated') {
        api
          .updatePreferredLanguage(next)
          .then(setCurrentUser)
          .catch(() => {});
      }
    },
    [api, authState],
  );

  useEffect(() => {
    if (setPasswordToken !== null) {
      return;
    }
    void bootstrap();
  }, [bootstrap, setPasswordToken]);

  // NetBird nav item + view gate: any portal user can call this (boolean-only, no
  // secret leak), so it resolves regardless of role; the render gate additionally
  // requires system_admin. Gated on authenticated (a protected /api/portal/*
  // endpoint must never be called pre-login). A load/network error leaves the
  // flag false (hidden). Uses `module_enabled` (the RAW enable checkbox, which
  // now lives in System Settings) rather than `enabled` (fully configured) so
  // the NetBird nav item appears as soon as the checkbox is flipped on — before
  // url/token are configured — letting a system-admin reach the view to finish
  // setup instead of being locked out by a chicken-and-egg gate.
  const refreshNetbirdModule = useCallback(() => {
    api
      .netbirdEnabled()
      .then((r) => setNetbirdModuleEnabled(Boolean(r.module_enabled)))
      .catch(() => {
        /* not reachable / not configured → the NetBird nav item stays hidden */
      });
  }, [api]);
  // Fetch the NetBird module flag on login; SystemSettings also calls this after a
  // save (via onSaved) so toggling the enable checkbox shows/hides the NetBird nav
  // item live, without a manual page refresh.
  useEffect(() => {
    if (authState === 'authenticated') refreshNetbirdModule();
  }, [authState, refreshNetbirdModule]);

  // Certificates nav item + view gate: mirrors refreshNetbirdModule (portal-scoped,
  // boolean-only, so it resolves for any authenticated user); the render gate
  // additionally requires system_admin.
  const refreshCertificatesModule = useCallback(() => {
    api
      .certificatesEnabled()
      .then((r) => setCertificatesModuleEnabled(Boolean(r.module_enabled)))
      .catch(() => {
        /* not reachable / not configured → the certificates nav item stays hidden */
      });
  }, [api]);
  useEffect(() => {
    if (authState === 'authenticated') refreshCertificatesModule();
  }, [authState, refreshCertificatesModule]);

  // System-admin mode auto-drop: while elevated with a known expiry, schedule a
  // session refetch right at that deadline so the UI de-elevates itself (every
  // systemAdminMode-gated nav item/view re-gates) without waiting for the next
  // unrelated navigation to notice. A negative/NaN remaining time (already
  // expired / unparseable) refetches immediately. Cleared on every change/unmount.
  useEffect(() => {
    if (!currentUser?.system_admin_mode || !currentUser.system_admin_mode_expires_at) return;
    const expiresAt = new Date(currentUser.system_admin_mode_expires_at).getTime();
    const remainingMs = expiresAt - Date.now();
    const delay = Number.isFinite(remainingMs) && remainingMs > 0 ? remainingMs : 0;
    const timer = window.setTimeout(() => {
      api
        .session()
        .then((s) => {
          if (s.authenticated && s.user) setCurrentUser(s.user);
        })
        .catch(() => {
          /* transient — the next navigation's loadPortalData will 401 if the session truly expired */
        });
    }, delay);
    return () => window.clearTimeout(timer);
  }, [api, currentUser?.system_admin_mode, currentUser?.system_admin_mode_expires_at]);

  // When System-Admin mode is entered or left, the caller's `system` scope
  // changes, so scope-dependent content (the group landscape, the admin user
  // list, …) must be re-fetched — otherwise the current view keeps showing the
  // previous scope's data (too much after leaving, too little after entering).
  // This one effect handles BOTH the manual enter/leave (SystemAdminModeControl)
  // AND the auto-drop-on-expiry path, since both flip `system_admin_mode`. On a
  // real transition it (a) bumps contentEpoch → the routed content remounts so
  // self-fetching views re-fetch on mount, and (b) reloads the prop-fed App data.
  const prevElevatedRef = useRef(systemAdminMode);
  useEffect(() => {
    if (prevElevatedRef.current === systemAdminMode) return;
    prevElevatedRef.current = systemAdminMode;
    setContentEpoch((n) => n + 1);
    void loadPortalData({ silent: true });
  }, [systemAdminMode, loadPortalData]);

  async function handleLogout() {
    try {
      await api.logout();
    } catch {
      // ignore logout transport errors; treat as logged out
    }
    setCurrentUser(null);
    setAuthState('login');
  }

  if (setPasswordToken !== null) {
    return (
      <SetPassword
        t={t}
        api={api}
        token={setPasswordToken}
        locale={locale}
        onSelectLocale={selectLocale}
      />
    );
  }
  if (authState === 'loading') {
    return (
      <Box
        sx={{
          minHeight: '100vh',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          p: 3,
          bgcolor: 'var(--page)',
        }}
      >
        <Box
          role="status"
          sx={{
            fontWeight: 700,
            py: 1.5,
            px: 1.75,
            bgcolor: 'var(--accent-soft)',
            border: '1px solid var(--line)',
            color: 'var(--brand-primary)',
          }}
        >
          {t.loading}
        </Box>
      </Box>
    );
  }
  if (authState === 'login') {
    return (
      <Login
        t={t}
        api={api}
        locale={locale}
        onSelectLocale={selectLocale}
        onSuccess={(user) => {
          setLocale(toLocale(user.preferred_language));
          setAuthState('loading');
          void loadPortalData();
        }}
      />
    );
  }

  const role = currentUser?.role ?? 'user';
  const userId = currentUser?.id ?? '';
  // The one gate every view (nav item + content) is checked against — see
  // viewRegistry in components/views.tsx. NavSidebar builds the same shape
  // from the same underlying flags, so nav visibility and content visibility
  // can never diverge.
  const gateCtx: ViewGateCtx = {
    isAdmin,
    systemAdminMode,
    netbirdModuleEnabled,
    certificatesModuleEnabled,
  };
  const renderCtx: ViewRenderCtx = {
    ...gateCtx,
    t,
    api,
    locale,
    role,
    userId,
    dashboard,
    productName,
    tokens,
    setTokens,
    models,
    manageModels,
    servers,
    setServers,
    loading,
    modelDetailOpen,
    setModelDetailOpen,
    onModelsChanged: refreshPortalDataSilently,
    onActivityUnauthorized: handleActivityAuthError,
    onSystemSettingsSaved: () => {
      refreshNetbirdModule();
      refreshCertificatesModule();
    },
    onSelectLocale: selectLocale,
  };
  const requestedEntry = viewRegistry[view];
  // Deliberate fallback: if the requested view's gate no longer holds (e.g.
  // systemAdminMode auto-drops while `system`/`netbird`/`certificates`/`logs`
  // is active), show the dashboard instead of rendering nothing. `view`
  // itself is left untouched — re-elevating shows the still-selected view
  // again without an extra navigation.
  const activeEntry = requestedEntry.gate(gateCtx) ? requestedEntry : viewRegistry.dashboard;

  return (
    <ConnectionProvider api={api} t={t}>
      <PreferencesProvider api={api}>
        <ChatStoreProvider
          api={api}
          models={models}
          tokens={tokens}
          servers={servers}
          onRefresh={refreshPortalDataSilently}
          refreshModels={refreshModels}
          t={t}
        >
          <Box
            sx={{
              height: '100dvh',
              overflow: 'hidden',
              display: 'flex',
              flexDirection: 'column',
              position: 'relative',
              zIndex: 1,
            }}
          >
            <AppBar
              position="sticky"
              elevation={0}
              sx={{
                top: 0,
                bgcolor: 'var(--header)',
                color: 'var(--header-text)',
                borderBottom: '1px solid var(--line)',
              }}
            >
              <Toolbar sx={{ minHeight: '72px !important', gap: 2, px: { xs: 2, sm: 3.5 } }}>
                <Tooltip title={navExpanded ? t.collapseNavigation : t.openNavigation}>
                  <IconButton
                    color="inherit"
                    edge="start"
                    aria-label={navExpanded ? t.collapseNavigation : t.openNavigation}
                    onClick={() => setNavExpanded((v) => !v)}
                  >
                    {navExpanded ? <CloseIcon /> : <MenuIcon />}
                  </IconButton>
                </Tooltip>
                <Brand brand={brand} label={productName} />
                <Box sx={{ ml: 'auto', display: 'flex', alignItems: 'center', gap: 1 }}>
                  <ColorModeMenu t={t} />
                  <LanguageMenu locale={locale} onSelect={selectLocale} t={t} />
                  <Box
                    component="span"
                    aria-hidden="true"
                    sx={{
                      width: '1px',
                      height: 30,
                      bgcolor: 'currentColor',
                      opacity: 0.25,
                      flex: '0 0 auto',
                      mx: 0.5,
                    }}
                  />
                  <UserMenu
                    displayName={currentUser?.display_name ?? t.unknownUser}
                    onProfile={() => setView('management')}
                    onLogout={handleLogout}
                    t={t}
                    systemAdminSlot={(closeMenu) => (
                      <SystemAdminModeControl
                        t={t}
                        api={api}
                        currentUser={currentUser}
                        onChanged={setCurrentUser}
                        onAction={closeMenu}
                      />
                    )}
                  />
                </Box>
              </Toolbar>
            </AppBar>

            <Box
              sx={{
                flex: 1,
                minHeight: 0,
                display: 'flex',
                overflow: 'hidden',
                position: 'relative',
              }}
            >
              <NavSidebar
                navItems={navItems}
                view={view}
                onSelect={setView}
                currentUser={currentUser}
                expanded={navExpanded}
                netbirdModuleEnabled={netbirdModuleEnabled}
                certificatesModuleEnabled={certificatesModuleEnabled}
                systemAdminMode={systemAdminMode}
                t={t}
              />
              <Box
                key={contentEpoch}
                component="main"
                sx={{
                  flex: 1,
                  minWidth: 0,
                  overflowY: 'auto',
                  minHeight: 0,
                  pt: '34px',
                  px: 'clamp(20px, 4vw, 54px)',
                  pb: '64px',
                }}
              >
                {loading && (
                  <Box
                    role="status"
                    sx={{
                      fontWeight: 700,
                      mb: 2.25,
                      py: 1.5,
                      px: 1.75,
                      bgcolor: 'var(--accent-soft)',
                      border: '1px solid var(--line)',
                      color: 'var(--brand-primary)',
                    }}
                  >
                    {t.loading}
                  </Box>
                )}
                {error && (
                  <Alert severity="error" role="alert" sx={{ mb: 2.25 }}>
                    {t.portalError}: {error}
                  </Alert>
                )}
                {activeEntry.render(renderCtx)}
              </Box>
            </Box>

            <Box
              component="footer"
              sx={{
                minHeight: 48,
                flexShrink: 0,
                bgcolor: 'var(--surface)',
                borderTop: '4px solid transparent',
                borderImage: 'linear-gradient(90deg, var(--brand-accent), var(--brand-primary)) 1',
                display: 'flex',
                alignItems: 'center',
                gap: 3,
                px: 2.25,
                py: 1.25,
                color: 'var(--muted)',
                fontSize: 14,
              }}
            >
              <Box component="span" sx={{ mr: 'auto' }}>
                Copyright (C) 2026 OnPrem AI Gateway contributors
              </Box>
              <Link
                href="https://www.gnu.org/licenses/agpl-3.0.html"
                target="_blank"
                rel="noopener noreferrer"
                underline="none"
                color="inherit"
                sx={{ '&:hover': { color: 'var(--brand-primary)' } }}
              >
                AGPL-3.0
              </Link>
              <Link
                href="https://github.com/JLor08/op-ai-gateway"
                target="_blank"
                rel="noopener noreferrer"
                underline="none"
                color="inherit"
                sx={{ '&:hover': { color: 'var(--brand-primary)' } }}
              >
                {t.sourceCode}
              </Link>
              <Link
                component="button"
                type="button"
                onClick={() => setView('datenschutz')}
                underline="none"
                color="inherit"
                sx={{ font: 'inherit', '&:hover': { color: 'var(--brand-primary)' } }}
              >
                {t.privacy}
              </Link>
              <Link
                component="button"
                type="button"
                onClick={() => setView('nutzungsbedingungen')}
                underline="none"
                color="inherit"
                sx={{ font: 'inherit', '&:hover': { color: 'var(--brand-primary)' } }}
              >
                {t.terms}
              </Link>
              <Link
                component="button"
                type="button"
                onClick={() => setView('impressum')}
                underline="none"
                color="inherit"
                sx={{ font: 'inherit', '&:hover': { color: 'var(--brand-primary)' } }}
              >
                {t.imprint}
              </Link>
            </Box>
          </Box>
        </ChatStoreProvider>
      </PreferencesProvider>
    </ConnectionProvider>
  );
}
