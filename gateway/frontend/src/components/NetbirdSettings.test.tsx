// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { NetbirdSettings } from './NetbirdSettings';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import { PortalApiError } from '../api';
import type {
  NetbirdNetwork,
  NetbirdStatus,
  NetbirdTokenStatus,
  RotateNetbirdTokenResult,
  SystemSettings as SystemSettingsDTO,
} from '../api';
import type { PortalApi } from './shared/types';

type NetbirdSettingsApi = Pick<
  PortalApi,
  | 'createGatewaySetupKey'
  | 'enrollGatewaySidecar'
  | 'getSystemSettings'
  | 'netbirdGroups'
  | 'netbirdNetwork'
  | 'netbirdPeers'
  | 'netbirdStatus'
  | 'netbirdTokenStatus'
  | 'rotateNetbirdToken'
  | 'servers'
  | 'testNetbird'
  | 'updateNetbirdNetwork'
  | 'updateSystemSettings'
>;

const t = messages.de;

function makeSettings(overrides: Partial<SystemSettingsDTO> = {}): SystemSettingsDTO {
  return {
    theme: 'default',
    available_themes: [{ id: 'default', name: 'Default' }],
    language: 'de',
    available_languages: ['de'],
    capture_retention_days: 30,
    capture_enabled: true,
    capture_override: false,
    health_check_interval_seconds: 30,
    agent_presence_timeout_seconds: 15,
    smtp_enabled: false,
    smtp_host: '',
    smtp_port: 587,
    smtp_username: '',
    smtp_password_set: false,
    smtp_from: '',
    smtp_from_name: '',
    smtp_tls_mode: 'starttls',
    totp_mode: 'off',
    route_affinity_session_mode: 'client_session',
    vision_probe_mode: 'accept',
    energy_default_price_per_kwh: 0,
    energy_default_pue: 0,
    energy_default_wh_per_token: 0,
    currency_usd_per_eur: 0,
    energy_default_price_unit: 'eur_cent',
    netbird_enabled: false,
    netbird_url: '',
    netbird_groups: [],
    netbird_token_set: false,
    netbird_only: false,
    netbird_gateway_peer_id: '',
    netbird_gateway_peer_name: '',
    netbird_manage_policies: false,
    netbird_policy_scope: 'auto',
    netbird_effective_policy_scope: 'selected',
    netbird_deny_by_default: false,
    netbird_deny_by_default_enforce: false,
    netbird_peer_sync_interval_seconds: 30,
    netbird_reconcile_interval_seconds: 60,
    netbird_allow_ping_gateway: false,
    netbird_allow_ping_all_servers: false,
    netbird_token_rotate_before_days: 14,
    netbird_agent_download_only: false,
    system_admin_mode_require_password: true,
    resource_provisioning_enforce: false,
    cert_enabled: false,
    cert_issuer_mode: 'acme',
    cert_self_signed_validity_days: 365,
    cert_ca_renew_before_days: 365,
    acme_email: '',
    acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
    cert_base_domain: '',
    cert_gateway_domain: '',
    cert_server_scope: 'selected',
    cert_manage_public_domain: false,
    cert_public_domains: [],
    cert_renew_before_days: 30,
    ...overrides,
  };
}

function makeTokenStatus(overrides: Partial<NetbirdTokenStatus> = {}): NetbirdTokenStatus {
  return {
    known: false,
    name: '',
    expiration_date: '',
    days_remaining: 0,
    last_used: '',
    ...overrides,
  };
}

function makeNetwork(overrides: Partial<NetbirdNetwork> = {}): NetbirdNetwork {
  return {
    dns_domain: '',
    network_range: '',
    network_range_v6: '',
    ipv6_enabled_groups: [],
    ...overrides,
  };
}

function renderNetbirdSettings(
  initial: SystemSettingsDTO,
  overrides: Partial<NetbirdSettingsApi> = {},
) {
  let current = initial;
  const updateSystemSettings = vi.fn(async (body: Record<string, unknown>) => {
    current = { ...current, ...body } as SystemSettingsDTO;
    return current;
  });
  const fakeApi = {
    getSystemSettings: vi.fn(async () => current),
    updateSystemSettings,
    // testNetbird now accepts an optional {url, token?} override (test the unsaved
    // credentials). The fake ignores the arg but records it for assertions.
    testNetbird: vi.fn(async () => ({ ok: true })),
    // The Netzwerk section (NetbirdNetworkPanel) reads + writes the NetBird account's
    // network settings live. Benign defaults (empty account, echo the PUT body).
    netbirdNetwork: vi.fn(async () => makeNetwork()),
    updateNetbirdNetwork: vi.fn(async (body: NetbirdNetwork) => body),
    // The URL/group/token module-config controls live in the Admin section. The
    // group picker lazily lists NetBird groups when the module is enabled + a token
    // is set; default to an empty list (the picker renders regardless).
    netbirdGroups: vi.fn(async () => ({ data: [] })),
    // The gateway-peer picker lists NetBird peers; the status block reads the live
    // transport status. Defaults keep both benign (no peers, listener inactive).
    netbirdPeers: vi.fn(async () => ({ data: [] })),
    // The gateway-peer picker greys out peers already linked to an AI-server; the
    // server list supplies those peer ids. Default: no servers (nothing greyed out).
    servers: vi.fn(async () => ({ data: [] })),
    netbirdStatus: vi.fn(async () => ({
      agent_listener_active: false,
      agent_listener_addr: '',
      netbird_only: false,
      gateway_peer_id: '',
      gateway_peer_connected: false,
      gateway_peer_name: '',
      sidecar_enroll_available: true,
      manage_policies: false,
      policy_scope: 'auto',
      effective_policy_scope: 'selected',
      deny_by_default: false,
      deny_by_default_enforce: false,
      managed_policy_count: 0,
      default_policy_present: true,
      default_policy_enabled: true,
      deny_by_default_drift: false,
    })),
    // The gateway setup-key mint (display-once): default returns a key + command.
    createGatewaySetupKey: vi.fn(async () => ({
      setup_key: 'GW-KEY-123',
      netbird_setup_command: 'netbird up --management-url https://nb.io --setup-key GW-KEY-123',
    })),
    // The sidecar self-enroll: mints a key + writes it to the shared volume.
    // Default returns a distinct key + command (also revealed as a fallback).
    enrollGatewaySidecar: vi.fn(async () => ({
      setup_key: 'SIDECAR-KEY-456',
      netbird_setup_command:
        'netbird up --management-url https://nb.io --setup-key SIDECAR-KEY-456',
    })),
    // Admin-token validity + rotation. A SUCCESSFUL netbirdTokenStatus is what
    // proves the stored admin token reaches NetBird (⇒ adminConnectionOk), so the
    // default resolving mock unlocks sections 2–4 whenever the module is
    // enabled + a token is set.
    netbirdTokenStatus: vi.fn(async () => makeTokenStatus()),
    rotateNetbirdToken: vi.fn(
      async () =>
        ({
          expiration_date: '2027-08-03T00:00:00Z',
          days_remaining: 365,
          old_deleted: true,
          old_unknown: false,
        }) satisfies RotateNetbirdTokenResult,
    ),
    ...overrides,
  };

  render(
    <ToastProvider>
      <NetbirdSettings t={t} api={fakeApi} />
    </ToastProvider>,
  );

  return { fakeApi, updateSystemSettings };
}

// A fully-configured module (url + stored token) so the admin-connection probe can
// succeed and unlock the Peer/Policies/Netzwerk sections.
function enabledSettings(overrides: Partial<SystemSettingsDTO> = {}): SystemSettingsDTO {
  return makeSettings({
    netbird_enabled: true,
    netbird_url: 'https://nb.io',
    netbird_token_set: true,
    ...overrides,
  });
}

// Returns the Save button that lives inside a given section (each of the four
// panels renders as an ARIA region named by its heading, each with its own Save).
function sectionSaveButton(sectionName: string): HTMLElement {
  const region = screen.getByRole('region', { name: sectionName });
  return within(region).getByRole('button', { name: t.save });
}

// Waits until the admin-connection gate has unlocked sections 2–4 (both
// admin-required hints have disappeared).
async function waitForUnlocked() {
  await waitFor(() =>
    expect(screen.queryAllByText(t.settingsNetbirdAdminRequired)).toHaveLength(0),
  );
}

afterEach(cleanup);

describe('NetbirdSettings page', () => {
  it('renders the four ordered sections + the Admin module-config controls', async () => {
    renderNetbirdSettings(makeSettings());
    // The page title appears (the PageTitle level-1 heading).
    expect((await screen.findAllByText(t.settingsNetbirdTitle)).length).toBeGreaterThan(0);
    // The four ordered section panels each render as a named region.
    await screen.findByRole('region', { name: t.settingsNetbirdSectionAdmin });
    screen.getByRole('region', { name: t.settingsNetbirdSectionNetwork });
    screen.getByRole('region', { name: t.settingsNetbirdSectionPeer });
    screen.getByRole('region', { name: t.settingsNetbirdSectionPolicies });
    // The url/token/test module-config fields live in the Admin section.
    await screen.findByLabelText(t.settingsNetbirdUrl);
    await screen.findByLabelText(t.settingsNetbirdToken);
    expect(screen.getByRole('button', { name: t.settingsNetbirdTest })).toBeInTheDocument();
    // The operational netbird_only toggle is present (in the Peer section).
    await screen.findByLabelText(t.settingsNetbirdOnly);
  });

  it('does NOT render the enable checkbox — it lives in System Settings', async () => {
    renderNetbirdSettings(enabledSettings());
    await screen.findByLabelText(t.settingsNetbirdOnly);
    expect(screen.queryByLabelText(t.settingsNetbirdEnable)).not.toBeInTheDocument();
  });
});

describe('NetbirdSettings admin-connection gate', () => {
  it('locks the Peer + Policies sections when the module is not configured (no admin connection)', async () => {
    renderNetbirdSettings(makeSettings());
    // The module is not enabled/token-set → no admin connection to probe → sections
    // 2–4 are locked: both hint alerts show and the controls are disabled.
    expect(await screen.findAllByText(t.settingsNetbirdAdminRequired)).toHaveLength(2);
    const toggle = (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement;
    expect(toggle).toBeDisabled();
    expect(sectionSaveButton(t.settingsNetbirdSectionPeer)).toBeDisabled();
    expect(sectionSaveButton(t.settingsNetbirdSectionPolicies)).toBeDisabled();
    // The Admin section is never locked — its Save is not gated by the connection.
    expect(sectionSaveButton(t.settingsNetbirdSectionAdmin)).not.toBeDisabled();
  });

  it('unlocks the Peer + Policies sections once the stored admin token reaches NetBird', async () => {
    renderNetbirdSettings(enabledSettings());
    // The default netbirdTokenStatus resolves → adminConnectionOk → sections unlock.
    await waitForUnlocked();
    const toggle = (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement;
    await waitFor(() => expect(toggle).not.toBeDisabled());
    expect(screen.queryAllByText(t.settingsNetbirdAdminRequired)).toHaveLength(0);
  });

  it('keeps the sections locked when the stored token is rejected (token-status throws)', async () => {
    const netbirdTokenStatus = vi.fn(async () => {
      throw new Error('401');
    });
    renderNetbirdSettings(enabledSettings(), { netbirdTokenStatus } as Partial<NetbirdSettingsApi>);
    await waitFor(() => expect(netbirdTokenStatus).toHaveBeenCalled());
    // The probe failed → the admin connection is not OK → sections stay locked.
    expect(screen.getAllByText(t.settingsNetbirdAdminRequired)).toHaveLength(2);
    expect(
      (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement,
    ).toBeDisabled();
  });
});

describe('NetbirdSettings disjoint per-section save', () => {
  it('Admin Save PUTs only the admin field set', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings());
    const url = (await screen.findByLabelText(t.settingsNetbirdUrl)) as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://nb2.io' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    const body = updateSystemSettings.mock.calls[0][0] as Record<string, unknown>;
    expect(body).toMatchObject({ netbird_url: 'https://nb2.io' });
    expect(body).toHaveProperty('netbird_groups');
    expect(body).toHaveProperty('netbird_peer_sync_interval_seconds');
    expect(body).toHaveProperty('netbird_token_rotate_before_days');
    // No fields belonging to the Peer / Policies sections, nor any foreign field.
    expect(body).not.toHaveProperty('netbird_only');
    expect(body).not.toHaveProperty('netbird_gateway_peer_id');
    expect(body).not.toHaveProperty('netbird_manage_policies');
    expect(body).not.toHaveProperty('netbird_reconcile_interval_seconds');
    expect(body).not.toHaveProperty('netbird_enabled');
    expect(body).not.toHaveProperty('theme');
  });

  it('Peer Save PUTs only the peer field set', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings());
    await waitForUnlocked();
    const toggle = (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement;
    await waitFor(() => expect(toggle).not.toBeDisabled());
    fireEvent.click(toggle);
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    const body = updateSystemSettings.mock.calls[0][0] as Record<string, unknown>;
    expect(body).toMatchObject({ netbird_only: true });
    expect(body).toHaveProperty('netbird_gateway_peer_id');
    expect(body).toHaveProperty('netbird_gateway_peer_name');
    // No admin / policy fields, no enable checkbox, no foreign field.
    expect(body).not.toHaveProperty('netbird_url');
    expect(body).not.toHaveProperty('netbird_groups');
    expect(body).not.toHaveProperty('netbird_peer_sync_interval_seconds');
    expect(body).not.toHaveProperty('netbird_manage_policies');
    expect(body).not.toHaveProperty('netbird_reconcile_interval_seconds');
    expect(body).not.toHaveProperty('netbird_enabled');
    expect(body).not.toHaveProperty('theme');
  });

  it('Policies Save PUTs only the policy field set', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings());
    await waitForUnlocked();
    const manage = (await screen.findByLabelText(
      t.settingsNetbirdManagePolicies,
    )) as HTMLInputElement;
    await waitFor(() => expect(manage).not.toBeDisabled());
    fireEvent.click(manage);
    const save = sectionSaveButton(t.settingsNetbirdSectionPolicies);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    const body = updateSystemSettings.mock.calls[0][0] as Record<string, unknown>;
    expect(body).toMatchObject({ netbird_manage_policies: true });
    expect(body).toHaveProperty('netbird_policy_scope');
    expect(body).toHaveProperty('netbird_deny_by_default');
    expect(body).toHaveProperty('netbird_deny_by_default_enforce');
    expect(body).toHaveProperty('netbird_allow_ping_gateway');
    expect(body).toHaveProperty('netbird_allow_ping_all_servers');
    expect(body).toHaveProperty('netbird_reconcile_interval_seconds');
    // No admin / peer fields, no enable checkbox, no foreign field.
    expect(body).not.toHaveProperty('netbird_url');
    expect(body).not.toHaveProperty('netbird_peer_sync_interval_seconds');
    expect(body).not.toHaveProperty('netbird_only');
    expect(body).not.toHaveProperty('netbird_gateway_peer_id');
    expect(body).not.toHaveProperty('netbird_enabled');
    expect(body).not.toHaveProperty('theme');
  });
});

describe('NetbirdSettings module-config (url/groups/token/test — Admin section)', () => {
  it('saves the url field via the Admin Save', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(
      makeSettings({ netbird_enabled: false, netbird_url: '', netbird_token_set: false }),
    );
    const url = (await screen.findByLabelText(t.settingsNetbirdUrl)) as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://nb.io' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_url: 'https://nb.io' });
  });

  it("keeps the token on save when the input is left blank (placeholder 'gesetzt'; value never populated)", async () => {
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings());
    const token = (await screen.findByLabelText(t.settingsNetbirdToken)) as HTMLInputElement;
    expect(token.placeholder).toBe(t.settingsNetbirdTokenSet);
    // Write-only: the field is NEVER populated from the DTO.
    expect(token.value).toBe('');
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).not.toHaveProperty('netbird_token');
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_url: 'https://nb.io' });
  });

  it("sends netbird_token:'' when the clear affordance is used", async () => {
    // Clearing the token requires the module disabled (an enabled module without
    // a token is an incomplete config → the Admin Save is gated off, by design).
    const { updateSystemSettings } = renderNetbirdSettings(
      makeSettings({ netbird_enabled: false, netbird_token_set: true }),
    );
    fireEvent.click(await screen.findByRole('button', { name: t.settingsNetbirdTokenClear }));
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_token: '' });
  });

  it('blocks the Admin save while the module is enabled without a url/token', async () => {
    renderNetbirdSettings(
      makeSettings({ netbird_enabled: true, netbird_url: '', netbird_token_set: false }),
    );
    await screen.findByLabelText(t.settingsNetbirdUrl);
    expect(sectionSaveButton(t.settingsNetbirdSectionAdmin)).toBeDisabled();
  });

  it('calls testNetbird with the current url and toasts the result', async () => {
    const { fakeApi } = renderNetbirdSettings(enabledSettings());
    fireEvent.click(await screen.findByRole('button', { name: t.settingsNetbirdTest }));
    await waitFor(() => expect(fakeApi.testNetbird).toHaveBeenCalled());
    expect(fakeApi.testNetbird).toHaveBeenCalledWith({ url: 'https://nb.io' });
    expect(await screen.findByText(t.settingsNetbirdTestOk)).toBeInTheDocument();
  });

  it('loads NetBird groups as a multi freeSolo picker and keeps typed (new) names', async () => {
    const netbirdGroups = vi.fn(async () => ({ data: [{ id: 'g1', name: 'policy-a' }] }));
    const { updateSystemSettings, fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdGroups,
    } as Partial<NetbirdSettingsApi>);
    const group = await screen.findByRole('combobox', { name: t.settingsNetbirdGroups });
    await waitFor(() => expect(fakeApi.netbirdGroups).toHaveBeenCalled());
    fireEvent.change(group, { target: { value: 'custom-group' } });
    fireEvent.keyDown(group, { key: 'Enter' });
    fireEvent.change(group, { target: { value: 'second-grp' } });
    fireEvent.keyDown(group, { key: 'Enter' });
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_groups: ['custom-group', 'second-grp'],
    });
  });

  it('falls back to a comma-separated text field when the groups load fails (split on save)', async () => {
    const netbirdGroups = vi.fn(async () => {
      throw new Error('nope');
    });
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings(), {
      netbirdGroups,
    } as Partial<NetbirdSettingsApi>);
    await waitFor(() => expect(netbirdGroups).toHaveBeenCalled());
    // The admin fallback is a plain textbox (no combobox named settingsNetbirdGroups).
    await waitFor(() =>
      expect(
        screen.queryByRole('combobox', { name: t.settingsNetbirdGroups }),
      ).not.toBeInTheDocument(),
    );
    const group = (await screen.findByLabelText(t.settingsNetbirdGroups)) as HTMLInputElement;
    fireEvent.change(group, { target: { value: 'alpha, beta' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_groups: ['alpha', 'beta'],
    });
  });
});

describe('NetbirdSettings test-without-save + confirm-to-save', () => {
  it('offers to save after a successful test with unsaved Admin changes, and saves on confirm', async () => {
    const { fakeApi, updateSystemSettings } = renderNetbirdSettings(enabledSettings());
    const url = (await screen.findByLabelText(t.settingsNetbirdUrl)) as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://nb-changed.io' } });
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdTest }));
    // Test sends the currently entered url; token omitted (not typed).
    await waitFor(() =>
      expect(fakeApi.testNetbird).toHaveBeenCalledWith({ url: 'https://nb-changed.io' }),
    );
    // The "save now?" confirm appears; the save has NOT fired yet.
    expect(await screen.findByText(t.settingsNetbirdTestSaveConfirmBody)).toBeInTheDocument();
    expect(updateSystemSettings).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdTestSaveConfirmAction }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_url: 'https://nb-changed.io',
    });
  });

  it('does NOT offer to save after a successful test with no unsaved Admin changes', async () => {
    const { fakeApi } = renderNetbirdSettings(enabledSettings());
    fireEvent.click(await screen.findByRole('button', { name: t.settingsNetbirdTest }));
    await waitFor(() => expect(fakeApi.testNetbird).toHaveBeenCalled());
    // Success toast, but no save-now confirm (nothing was edited).
    expect(await screen.findByText(t.settingsNetbirdTestOk)).toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdTestSaveConfirmBody)).not.toBeInTheDocument();
  });
});

describe('NetbirdSettings NetBird-only transport (Peer section)', () => {
  it('reflects the loaded netbird_only setting and persists a change via the Peer Save', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({ netbird_only: false }),
    );
    await waitForUnlocked();
    const toggle = (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement;
    await waitFor(() => expect(toggle).not.toBeDisabled());
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);
    expect(toggle).toBeChecked();
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_only: true });
  });

  it('renders the toggle checked when netbird_only is set', async () => {
    renderNetbirdSettings(enabledSettings({ netbird_only: true }));
    const toggle = (await screen.findByLabelText(t.settingsNetbirdOnly)) as HTMLInputElement;
    expect(toggle).toBeChecked();
  });

  it('lists gateway-peer options and sends netbird_gateway_peer_id on select', async () => {
    const netbirdPeers = vi.fn(async () => ({
      data: [
        { id: 'peer-gw', name: 'gateway', dns_label: 'gw.netbird.io', connected: true },
        { id: 'peer-x', name: 'other', dns_label: 'other.netbird.io', connected: false },
      ],
    }));
    const { updateSystemSettings, fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdPeers,
    } as Partial<NetbirdSettingsApi>);
    await waitForUnlocked();
    const pick = (await screen.findByRole('combobox', {
      name: t.settingsNetbirdGatewayPeer,
    })) as HTMLInputElement;
    await waitFor(() => expect(fakeApi.netbirdPeers).toHaveBeenCalled());
    await waitFor(() => expect(pick).not.toBeDisabled());
    fireEvent.change(pick, { target: { value: 'gateway' } });
    fireEvent.click(await screen.findByRole('option', { name: /gateway/ }));
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    // A gateway-peer change prompts a confirm dialog first (agents may lose the gateway).
    fireEvent.click(
      await screen.findByRole('button', { name: t.settingsNetbirdGatewayPeerChangeConfirmAction }),
    );
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_gateway_peer_id: 'peer-gw',
    });
  });

  it('shows the no-listener warning when netbird_only is on but no agent listener is active', async () => {
    const netbirdStatus = vi.fn(async () => netbirdStatusFixture({ netbird_only: true }));
    renderNetbirdSettings(enabledSettings({ netbird_only: true }), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    expect(await screen.findByText(t.settingsNetbirdOnlyNoListenerWarning)).toBeInTheDocument();
  });

  it('hides the no-listener warning when the agent listener is active (mutation guard)', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({
        agent_listener_active: true,
        agent_listener_addr: '100.1.2.3:8081',
        netbird_only: true,
        gateway_peer_id: 'peer-gw',
        gateway_peer_connected: true,
      }),
    );
    renderNetbirdSettings(enabledSettings({ netbird_only: true }), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    expect(
      await screen.findByText(t.settingsNetbirdListenerActive('100.1.2.3:8081')),
    ).toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdOnlyNoListenerWarning)).not.toBeInTheDocument();
  });

  it('hides the no-listener warning when netbird_only is off even without a listener', async () => {
    const netbirdStatus = vi.fn(async () => netbirdStatusFixture({ netbird_only: false }));
    renderNetbirdSettings(enabledSettings({ netbird_only: false }), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    await waitFor(() => expect(netbirdStatus).toHaveBeenCalled());
    expect(screen.queryByText(t.settingsNetbirdOnlyNoListenerWarning)).not.toBeInTheDocument();
  });

  it('hides the status block when the status call fails', async () => {
    const netbirdStatus = vi.fn(async () => {
      throw new Error('nope');
    });
    renderNetbirdSettings(enabledSettings({ netbird_only: true }), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    await waitFor(() => expect(netbirdStatus).toHaveBeenCalled());
    // Degrade gracefully: neither the status title nor the warning renders.
    expect(screen.queryByText(t.settingsNetbirdStatusTitle)).not.toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdOnlyNoListenerWarning)).not.toBeInTheDocument();
  });
});

// Builds a full NetbirdStatus fixture with the given overrides — used by the
// status/peer-name tests, which mostly care about gateway_peer_id/gateway_peer_name
// and want the rest at safe defaults.
function netbirdStatusFixture(overrides: Partial<NetbirdStatus> = {}): NetbirdStatus {
  return {
    agent_listener_active: false,
    agent_listener_addr: '',
    netbird_only: false,
    gateway_peer_id: '',
    gateway_peer_connected: false,
    gateway_peer_name: '',
    sidecar_enroll_available: false,
    manage_policies: false,
    policy_scope: 'auto',
    effective_policy_scope: 'selected',
    deny_by_default: false,
    deny_by_default_enforce: false,
    managed_policy_count: 0,
    default_policy_present: true,
    default_policy_enabled: true,
    deny_by_default_drift: false,
    ...overrides,
  };
}

describe('NetbirdSettings gateway-peer-name field (live display, sticky edit, wish-name save)', () => {
  it('shows the live peer name from the status when there is no pending edit', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({
        gateway_peer_id: 'peer-1',
        gateway_peer_connected: true,
        gateway_peer_name: 'live-name',
      }),
    );
    renderNetbirdSettings(
      enabledSettings({
        netbird_gateway_peer_id: 'peer-1',
        netbird_gateway_peer_name: 'stored-wish-name',
      }),
      { netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    // The live name (from the status) wins over the stored wish-name for display.
    await waitFor(() => expect(field.value).toBe('live-name'));
  });

  it('falls back to the stored wish-name when the status has no live name yet', async () => {
    const netbirdStatus = vi.fn(async () => netbirdStatusFixture());
    renderNetbirdSettings(
      enabledSettings({
        netbird_gateway_peer_id: 'peer-1',
        netbird_gateway_peer_name: 'stored-wish-name',
      }),
      { netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    await waitFor(() => expect(netbirdStatus).toHaveBeenCalled());
    expect(field.value).toBe('stored-wish-name');
  });

  it('is sticky: a status refresh delivering a different live name after the operator has typed does not change the field', async () => {
    let resolveStatus: ((value: NetbirdStatus) => void) | null = null;
    const netbirdStatus = vi.fn(
      () =>
        new Promise<NetbirdStatus>((resolve) => {
          resolveStatus = resolve;
        }),
    );
    renderNetbirdSettings(
      enabledSettings({
        netbird_gateway_peer_id: 'peer-1',
        netbird_gateway_peer_name: 'stored-wish-name',
      }),
      { netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    // Unlock first so the peer-name field is editable.
    await waitForUnlocked();
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    await waitFor(() => expect(field).not.toBeDisabled());
    // Before the in-flight status call resolves, the field shows the stored wish-name.
    expect(field.value).toBe('stored-wish-name');
    fireEvent.change(field, { target: { value: 'typed-by-operator' } });
    expect(field.value).toBe('typed-by-operator');
    // The status refetch now resolves with a DIFFERENT live name — it must NOT
    // clobber what the operator already typed (the sticky-edit invariant).
    expect(resolveStatus).not.toBeNull();
    resolveStatus!(
      netbirdStatusFixture({
        gateway_peer_id: 'peer-1',
        gateway_peer_connected: true,
        gateway_peer_name: 'live-name-arrived-late',
      }),
    );
    // Wait for the resolved status to actually apply (the status block appearing
    // is proof the refetch was processed), then assert the field is unchanged.
    expect(await screen.findByText(t.settingsNetbirdStatusTitle)).toBeInTheDocument();
    expect(field.value).toBe('typed-by-operator');
  });

  it('a no-touch Save sends the stored wish-name, not the live name', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({
        gateway_peer_id: 'peer-1',
        gateway_peer_connected: true,
        gateway_peer_name: 'live-name',
      }),
    );
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({
        netbird_gateway_peer_id: 'peer-1',
        netbird_gateway_peer_name: 'stored-wish-name',
      }),
      { netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    await waitForUnlocked();
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    await waitFor(() => expect(field.value).toBe('live-name'));
    // No edit — click the Peer Save directly.
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_gateway_peer_name: 'stored-wish-name',
    });
  });

  it('an edited field sends the edited value on Save', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({
        gateway_peer_id: 'peer-1',
        gateway_peer_connected: true,
        gateway_peer_name: 'live-name',
      }),
    );
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({
        netbird_gateway_peer_id: 'peer-1',
        netbird_gateway_peer_name: 'stored-wish-name',
      }),
      { netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    await waitForUnlocked();
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    await waitFor(() => expect(field.value).toBe('live-name'));
    fireEvent.change(field, { target: { value: 'new-desired-name' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    // A gateway-peer name change prompts a confirm dialog first.
    fireEvent.click(
      await screen.findByRole('button', { name: t.settingsNetbirdGatewayPeerChangeConfirmAction }),
    );
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_gateway_peer_name: 'new-desired-name',
    });
  });
});

describe('NetbirdSettings gateway-peer change warning + linked-peer greyout', () => {
  it('prompts a confirm dialog before saving a gateway-peer change; cancel aborts the save', async () => {
    const netbirdPeers = vi.fn(async () => ({
      data: [{ id: 'peer-gw', name: 'gateway', dns_label: 'gw.netbird.io', connected: true }],
    }));
    const { updateSystemSettings } = renderNetbirdSettings(enabledSettings(), {
      netbirdPeers,
    } as Partial<NetbirdSettingsApi>);
    await waitForUnlocked();
    const pick = (await screen.findByRole('combobox', {
      name: t.settingsNetbirdGatewayPeer,
    })) as HTMLInputElement;
    await waitFor(() => expect(pick).not.toBeDisabled());
    fireEvent.change(pick, { target: { value: 'gateway' } });
    fireEvent.click(await screen.findByRole('option', { name: /gateway/ }));
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    // The warning is shown and the save has NOT fired yet.
    expect(
      await screen.findByText(t.settingsNetbirdGatewayPeerChangeConfirmBody),
    ).toBeInTheDocument();
    expect(updateSystemSettings).not.toHaveBeenCalled();
    // Cancel aborts: still no save.
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    await waitFor(() =>
      expect(
        screen.queryByText(t.settingsNetbirdGatewayPeerChangeConfirmBody),
      ).not.toBeInTheDocument(),
    );
    expect(updateSystemSettings).not.toHaveBeenCalled();
  });

  it('greys out + annotates a gateway-peer option already linked to an AI-server', async () => {
    const netbirdPeers = vi.fn(async () => ({
      data: [
        { id: 'peer-free', name: 'free', dns_label: 'free.netbird.io', connected: true },
        { id: 'peer-linked', name: 'linked', dns_label: 'linked.netbird.io', connected: true },
      ],
    }));
    const servers = vi.fn(async () => ({ data: [{ netbird_peer_id: 'peer-linked' }] }));
    renderNetbirdSettings(enabledSettings(), {
      netbirdPeers,
      servers,
    } as unknown as Partial<NetbirdSettingsApi>);
    await waitForUnlocked();
    const pick = (await screen.findByRole('combobox', {
      name: t.settingsNetbirdGatewayPeer,
    })) as HTMLInputElement;
    await waitFor(() => expect(pick).not.toBeDisabled());
    fireEvent.change(pick, { target: { value: 'linked' } });
    const linkedOption = await screen.findByRole('option', { name: /linked/ });
    // Disabled (already linked to a server) + carries the "(bereits verknüpft)" hint.
    await waitFor(() => expect(linkedOption).toHaveAttribute('aria-disabled', 'true'));
    expect(
      within(linkedOption).getByText(new RegExp(t.serverNetbirdPeerLinked)),
    ).toBeInTheDocument();
  });

  it('reloads the gateway-peer dropdown after a save so a renamed peer shows its current name', async () => {
    let call = 0;
    const netbirdPeers = vi.fn(async () => {
      call += 1;
      // 2nd load (after save) reflects the applied rename, as a real reconcile would.
      return {
        data: [
          {
            id: 'peer-1',
            name: call === 1 ? 'old-name' : 'new-name',
            dns_label: 'gw.netbird.io',
            connected: true,
          },
        ],
      };
    });
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({
        gateway_peer_id: 'peer-1',
        gateway_peer_connected: true,
        gateway_peer_name: 'old-name',
      }),
    );
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({ netbird_gateway_peer_id: 'peer-1', netbird_gateway_peer_name: 'old-name' }),
      { netbirdPeers, netbirdStatus } as Partial<NetbirdSettingsApi>,
    );
    await waitForUnlocked();
    const pick = (await screen.findByRole('combobox', {
      name: t.settingsNetbirdGatewayPeer,
    })) as HTMLInputElement;
    await waitFor(() => expect(netbirdPeers).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(pick.value).toContain('old-name'));
    // Rename in the name field + save (confirm the change dialog).
    const field = (await screen.findByLabelText(
      t.settingsNetbirdGatewayPeerName,
    )) as HTMLInputElement;
    fireEvent.change(field, { target: { value: 'new-name' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionPeer);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    fireEvent.click(
      await screen.findByRole('button', { name: t.settingsNetbirdGatewayPeerChangeConfirmAction }),
    );
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    // The dropdown reloaded (statusNonce) → the selected peer now shows its new name.
    await waitFor(() => expect(netbirdPeers).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(pick.value).toContain('new-name'));
  });
});

describe('NetbirdSettings re-enroll confirm (setup-key + sidecar)', () => {
  it('asks for confirmation before minting a setup key when a gateway peer already exists', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ gateway_peer_id: 'peer-1', sidecar_enroll_available: true }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    fireEvent.click(create);
    // The confirm dialog opens; the action must NOT have fired yet.
    expect(await screen.findByText(t.settingsNetbirdReenrollConfirmBody)).toBeInTheDocument();
    expect(fakeApi.createGatewaySetupKey).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdReenrollConfirmAction }));
    await waitFor(() => expect(fakeApi.createGatewaySetupKey).toHaveBeenCalled());
  });

  it('mints the setup key directly, with no confirm dialog, when there is no gateway peer yet', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ gateway_peer_id: '', sidecar_enroll_available: true }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    fireEvent.click(create);
    await waitFor(() => expect(fakeApi.createGatewaySetupKey).toHaveBeenCalled());
    expect(screen.queryByText(t.settingsNetbirdReenrollConfirmBody)).not.toBeInTheDocument();
  });

  it('asks for confirmation before enrolling the sidecar when a gateway peer already exists', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ gateway_peer_id: 'peer-1', sidecar_enroll_available: true }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    const enroll = await screen.findByRole('button', { name: t.settingsNetbirdSidecarEnroll });
    await waitFor(() => expect(enroll).not.toBeDisabled());
    fireEvent.click(enroll);
    expect(await screen.findByText(t.settingsNetbirdReenrollConfirmBody)).toBeInTheDocument();
    expect(fakeApi.enrollGatewaySidecar).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdReenrollConfirmAction }));
    await waitFor(() => expect(fakeApi.enrollGatewaySidecar).toHaveBeenCalled());
  });

  it('enrolls the sidecar directly, with no confirm dialog, when there is no gateway peer yet', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ gateway_peer_id: '', sidecar_enroll_available: true }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    const enroll = await screen.findByRole('button', { name: t.settingsNetbirdSidecarEnroll });
    await waitFor(() => expect(enroll).not.toBeDisabled());
    fireEvent.click(enroll);
    await waitFor(() => expect(fakeApi.enrollGatewaySidecar).toHaveBeenCalled());
    expect(screen.queryByText(t.settingsNetbirdReenrollConfirmBody)).not.toBeInTheDocument();
  });

  it('cancelling the confirm dialog never fires the action', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ gateway_peer_id: 'peer-1', sidecar_enroll_available: true }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdStatus,
    } as Partial<NetbirdSettingsApi>);
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    fireEvent.click(create);
    expect(await screen.findByText(t.settingsNetbirdReenrollConfirmBody)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    // The dialog exit-transitions (its content stays mounted briefly), so wait
    // for it to fully close before asserting it is gone.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(fakeApi.createGatewaySetupKey).not.toHaveBeenCalled();
  });
});

describe('NetbirdSettings gateway setup key', () => {
  it('mints a key and reveals the setup_key + netbird_setup_command with copy affordances', async () => {
    const { fakeApi } = renderNetbirdSettings(enabledSettings());
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    fireEvent.click(create);
    await waitFor(() => expect(fakeApi.createGatewaySetupKey).toHaveBeenCalled());
    // The reveal dialog shows both the key AND the command (the mutation guard).
    expect(await screen.findByText('GW-KEY-123')).toBeInTheDocument();
    expect(
      await screen.findByText('netbird up --management-url https://nb.io --setup-key GW-KEY-123'),
    ).toBeInTheDocument();
    expect(screen.getByText(t.settingsNetbirdGatewayKeyHelp)).toBeInTheDocument();
  });

  it('resets the revealed command on close so a re-mint never shows the previous command', async () => {
    const commandA = 'netbird up --management-url https://nb.io --setup-key KEY-A';
    const commandB = 'netbird up --management-url https://nb.io --setup-key KEY-B';
    let call = 0;
    const createGatewaySetupKey = vi.fn(async () => {
      call += 1;
      return call === 1
        ? { setup_key: 'KEY-A', netbird_setup_command: commandA }
        : { setup_key: 'KEY-B', netbird_setup_command: commandB };
    });
    renderNetbirdSettings(enabledSettings(), {
      createGatewaySetupKey,
    } as Partial<NetbirdSettingsApi>);
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    // First mint -> the reveal shows command A.
    fireEvent.click(create);
    expect(await screen.findByText(commandA)).toBeInTheDocument();
    // Close via the Close button. The handler resets gatewayKey AND
    // gatewayKeyCommand, so command A must be gone IMMEDIATELY: the dialog is
    // still exit-transitioning (its content stays mounted for the transition), so
    // a stale command WOULD still render right here. This is the mutation guard —
    // leaving gatewayKeyCommand set keeps command A visible at this point.
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.captureClose }),
    );
    expect(screen.queryByText(commandA)).not.toBeInTheDocument();
    // Let the dialog fully close (while open MUI marks the page body aria-hidden,
    // so the create button is not accessible until the exit transition finishes).
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    // Re-mint with the API now returning command B -> only B appears; A never returns.
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdGatewayKeyCreate }));
    expect(await screen.findByText(commandB)).toBeInTheDocument();
    expect(screen.queryByText(commandA)).not.toBeInTheDocument();
    // Also cover the backdrop/escape onClose handler (same reset): closing via
    // Escape must clear command B immediately for the same reason.
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape', code: 'Escape' });
    expect(screen.queryByText(commandB)).not.toBeInTheDocument();
  });

  it('surfaces a toast and does NOT reveal a key when the mint fails', async () => {
    const createGatewaySetupKey = vi.fn(async () => {
      throw new Error('nb down');
    });
    renderNetbirdSettings(enabledSettings(), {
      createGatewaySetupKey,
    } as Partial<NetbirdSettingsApi>);
    const create = await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate });
    await waitFor(() => expect(create).not.toBeDisabled());
    fireEvent.click(create);
    await waitFor(() => expect(createGatewaySetupKey).toHaveBeenCalled());
    // A toast appears; the reveal (title + no key) never renders.
    expect(await screen.findByText('nb down')).toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdGatewayKeyHelp)).not.toBeInTheDocument();
  });

  it('hides the create button when the module is disabled or no token is set', async () => {
    // Module disabled.
    const { fakeApi } = renderNetbirdSettings(
      makeSettings({ netbird_enabled: false, netbird_token_set: true }),
    );
    await screen.findByLabelText(t.settingsNetbirdOnly);
    expect(
      screen.queryByRole('button', { name: t.settingsNetbirdGatewayKeyCreate }),
    ).not.toBeInTheDocument();
    expect(fakeApi.createGatewaySetupKey).not.toHaveBeenCalled();
    cleanup();
    // Enabled but no token stored.
    renderNetbirdSettings(
      makeSettings({
        netbird_enabled: true,
        netbird_url: 'https://nb.io',
        netbird_token_set: false,
      }),
    );
    await screen.findByLabelText(t.settingsNetbirdOnly);
    expect(
      screen.queryByRole('button', { name: t.settingsNetbirdGatewayKeyCreate }),
    ).not.toBeInTheDocument();
  });
});

describe('NetbirdSettings sidecar enroll', () => {
  it('enrolls the sidecar: success toast + reveals the returned key + command', async () => {
    const { fakeApi } = renderNetbirdSettings(enabledSettings());
    const enroll = await screen.findByRole('button', { name: t.settingsNetbirdSidecarEnroll });
    await waitFor(() => expect(enroll).not.toBeDisabled());
    fireEvent.click(enroll);
    await waitFor(() => expect(fakeApi.enrollGatewaySidecar).toHaveBeenCalled());
    // Success toast confirms the write to the shared volume.
    expect(await screen.findByText(t.settingsNetbirdSidecarEnrolled)).toBeInTheDocument();
    // The reveal fallback shows the returned key AND command.
    expect(await screen.findByText('SIDECAR-KEY-456')).toBeInTheDocument();
    expect(
      await screen.findByText(
        'netbird up --management-url https://nb.io --setup-key SIDECAR-KEY-456',
      ),
    ).toBeInTheDocument();
  });

  it('shows the no-key-file toast (no reveal) on a 409 netbird.key_file_not_configured', async () => {
    const enrollGatewaySidecar = vi.fn(async () => {
      throw new PortalApiError(409, 'netbird.key_file_not_configured', 'key file not configured');
    });
    renderNetbirdSettings(enabledSettings(), {
      enrollGatewaySidecar,
    } as Partial<NetbirdSettingsApi>);
    const enroll = await screen.findByRole('button', { name: t.settingsNetbirdSidecarEnroll });
    await waitFor(() => expect(enroll).not.toBeDisabled());
    fireEvent.click(enroll);
    await waitFor(() => expect(enrollGatewaySidecar).toHaveBeenCalled());
    // The specific runbook toast appears; the reveal never opens.
    expect(await screen.findByText(t.settingsNetbirdSidecarNoKeyFile)).toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdGatewayKeyHelp)).not.toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('falls back to the generic error toast on a non-key-file error (mutation guard on the code match)', async () => {
    const enrollGatewaySidecar = vi.fn(async () => {
      throw new PortalApiError(502, 'netbird.auth_failed', 'auth failed');
    });
    renderNetbirdSettings(enabledSettings(), {
      enrollGatewaySidecar,
    } as Partial<NetbirdSettingsApi>);
    const enroll = await screen.findByRole('button', { name: t.settingsNetbirdSidecarEnroll });
    await waitFor(() => expect(enroll).not.toBeDisabled());
    fireEvent.click(enroll);
    await waitFor(() => expect(enrollGatewaySidecar).toHaveBeenCalled());
    // The generic formatPortalError toast (code prefix), NOT the key-file toast.
    expect(await screen.findByText(/netbird\.auth_failed/)).toBeInTheDocument();
    expect(screen.queryByText(t.settingsNetbirdSidecarNoKeyFile)).not.toBeInTheDocument();
  });

  it('hides the enroll button when the module is disabled or no token is set', async () => {
    const { fakeApi } = renderNetbirdSettings(
      makeSettings({ netbird_enabled: false, netbird_token_set: true }),
    );
    await screen.findByLabelText(t.settingsNetbirdOnly);
    expect(
      screen.queryByRole('button', { name: t.settingsNetbirdSidecarEnroll }),
    ).not.toBeInTheDocument();
    expect(fakeApi.enrollGatewaySidecar).not.toHaveBeenCalled();
    cleanup();
    renderNetbirdSettings(
      makeSettings({
        netbird_enabled: true,
        netbird_url: 'https://nb.io',
        netbird_token_set: false,
      }),
    );
    await screen.findByLabelText(t.settingsNetbirdOnly);
    expect(
      screen.queryByRole('button', { name: t.settingsNetbirdSidecarEnroll }),
    ).not.toBeInTheDocument();
  });

  it('hides the enroll button when no sidecar is wired (sidecar_enroll_available false), keeps the setup-key button', async () => {
    const netbirdStatus = vi.fn(async () =>
      netbirdStatusFixture({ sidecar_enroll_available: false }),
    );
    renderNetbirdSettings(enabledSettings(), { netbirdStatus } as Partial<NetbirdSettingsApi>);
    // The status fetch resolves; the setup-key button (not sidecar-gated) is present…
    expect(
      await screen.findByRole('button', { name: t.settingsNetbirdGatewayKeyCreate }),
    ).toBeInTheDocument();
    // …but the enroll button is absent because no key-file/sidecar is configured.
    expect(
      screen.queryByRole('button', { name: t.settingsNetbirdSidecarEnroll }),
    ).not.toBeInTheDocument();
  });
});

describe('NetbirdSettings policy management', () => {
  it('round-trips the policy-scope dropdown into the Policies Save payload', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({ netbird_policy_scope: 'auto' }),
    );
    await waitForUnlocked();
    fireEvent.mouseDown(await screen.findByLabelText(t.settingsNetbirdPolicyScope));
    fireEvent.click(await screen.findByRole('option', { name: t.settingsNetbirdPolicyScopeAll }));
    const save = sectionSaveButton(t.settingsNetbirdSectionPolicies);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_policy_scope: 'all' });
  });

  it('shows the resolved effective-scope hint only while scope is auto', async () => {
    renderNetbirdSettings(
      enabledSettings({ netbird_policy_scope: 'auto', netbird_effective_policy_scope: 'all' }),
    );
    await waitForUnlocked();
    expect(
      await screen.findByText(
        t.settingsNetbirdPolicyScopeEffective(t.settingsNetbirdPolicyScopeAll),
      ),
    ).toBeInTheDocument();
    // Switching to an explicit scope hides the (now-irrelevant) hint.
    fireEvent.mouseDown(screen.getByLabelText(t.settingsNetbirdPolicyScope));
    fireEvent.click(
      await screen.findByRole('option', { name: t.settingsNetbirdPolicyScopeSelected }),
    );
    expect(
      screen.queryByText(t.settingsNetbirdPolicyScopeEffective(t.settingsNetbirdPolicyScopeAll)),
    ).not.toBeInTheDocument();
  });

  it('disables the Admin Save + shows the peer-sync interval error when it exceeds the saved reconcile interval', async () => {
    renderNetbirdSettings(
      enabledSettings({
        netbird_peer_sync_interval_seconds: 30,
        netbird_reconcile_interval_seconds: 60,
      }),
    );
    const peerField = (await screen.findByLabelText(
      t.settingsNetbirdPeerInterval,
    )) as HTMLInputElement;
    const adminSave = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(adminSave).not.toBeDisabled());
    // 90 > the saved reconcile (60) → the Admin peer-sync field is invalid.
    fireEvent.change(peerField, { target: { value: '90' } });
    expect(adminSave).toBeDisabled();
    expect(screen.getAllByText(t.settingsNetbirdIntervalError).length).toBeGreaterThan(0);
    // A valid value (<= 60, >= 10) re-enables the Admin Save.
    fireEvent.change(peerField, { target: { value: '50' } });
    expect(adminSave).not.toBeDisabled();
  });

  it('disables the Policies Save + shows the reconcile interval error when it is below the saved peer-sync interval', async () => {
    renderNetbirdSettings(
      enabledSettings({
        netbird_peer_sync_interval_seconds: 30,
        netbird_reconcile_interval_seconds: 60,
      }),
    );
    await waitForUnlocked();
    const reconcileField = (await screen.findByLabelText(
      t.settingsNetbirdReconcileInterval,
    )) as HTMLInputElement;
    await waitFor(() => expect(reconcileField).not.toBeDisabled());
    const policiesSave = sectionSaveButton(t.settingsNetbirdSectionPolicies);
    await waitFor(() => expect(policiesSave).not.toBeDisabled());
    // 20 < the saved peer-sync (30) → the reconcile field is invalid.
    fireEvent.change(reconcileField, { target: { value: '20' } });
    expect(policiesSave).toBeDisabled();
    expect(screen.getAllByText(t.settingsNetbirdIntervalError).length).toBeGreaterThan(0);
    // A valid value (>= peer-sync 30, >= 10) re-enables the Policies Save.
    fireEvent.change(reconcileField, { target: { value: '120' } });
    expect(policiesSave).not.toBeDisabled();
  });

  it('surfaces the interval-order toast on a system.netbird_interval_order 400 (server-side backstop)', async () => {
    const updateSystemSettings = vi.fn(async () => {
      throw new PortalApiError(
        400,
        'system.netbird_interval_order',
        'peer-sync interval must be <= the reconcile interval',
      );
    });
    renderNetbirdSettings(enabledSettings(), {
      updateSystemSettings,
    } as Partial<NetbirdSettingsApi>);
    // The Admin Save is never locked and valid by default — use it to drive the backstop.
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(await screen.findByText(t.settingsNetbirdIntervalOrder)).toBeInTheDocument();
  });
});

describe('NetbirdSettings ping-allow toggles', () => {
  it('toggles the two ping-allow switches into the Policies Save payload', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({ netbird_allow_ping_gateway: false, netbird_allow_ping_all_servers: false }),
    );
    await waitForUnlocked();
    const gatewaySwitch = (await screen.findByLabelText(
      t.settingsNetbirdAllowPingGateway,
    )) as HTMLInputElement;
    const allServersSwitch = (await screen.findByLabelText(
      t.settingsNetbirdAllowPingAllServers,
    )) as HTMLInputElement;
    await waitFor(() => expect(gatewaySwitch).not.toBeDisabled());
    expect(gatewaySwitch).not.toBeChecked();
    expect(allServersSwitch).not.toBeChecked();
    fireEvent.click(gatewaySwitch);
    fireEvent.click(allServersSwitch);
    expect(gatewaySwitch).toBeChecked();
    expect(allServersSwitch).toBeChecked();
    const save = sectionSaveButton(t.settingsNetbirdSectionPolicies);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_allow_ping_gateway: true,
      netbird_allow_ping_all_servers: true,
    });
  });
});

describe('NetbirdSettings token validity + rotation', () => {
  it('renders the validity line from the live token status when known', async () => {
    const netbirdTokenStatus = vi.fn(async () =>
      makeTokenStatus({ known: true, expiration_date: '2027-08-03', days_remaining: 42 }),
    );
    renderNetbirdSettings(enabledSettings(), { netbirdTokenStatus } as Partial<NetbirdSettingsApi>);
    expect(
      await screen.findByText(t.settingsNetbirdTokenValid('2027-08-03', 42)),
    ).toBeInTheDocument();
  });

  it('renders the unknown-validity label when the token status is not known', async () => {
    const netbirdTokenStatus = vi.fn(async () => makeTokenStatus({ known: false }));
    renderNetbirdSettings(enabledSettings(), { netbirdTokenStatus } as Partial<NetbirdSettingsApi>);
    expect(await screen.findByText(t.settingsNetbirdTokenValidUnknown)).toBeInTheDocument();
    expect(screen.queryByText(/Token gültig bis/)).not.toBeInTheDocument();
  });

  it('rotates the token after confirmation, shows a success toast, and reloads the validity line', async () => {
    const netbirdTokenStatus = vi
      .fn()
      .mockResolvedValueOnce(
        makeTokenStatus({ known: true, expiration_date: '2027-01-01', days_remaining: 5 }),
      )
      .mockResolvedValueOnce(
        makeTokenStatus({ known: true, expiration_date: '2028-01-01', days_remaining: 365 }),
      );
    const rotateNetbirdToken = vi.fn(
      async () =>
        ({
          expiration_date: '2028-01-01',
          days_remaining: 365,
          old_deleted: true,
          old_unknown: false,
        }) satisfies RotateNetbirdTokenResult,
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      netbirdTokenStatus,
      rotateNetbirdToken,
    } as Partial<NetbirdSettingsApi>);
    // Initial load shows the near-expiry status.
    await screen.findByText(t.settingsNetbirdTokenValid('2027-01-01', 5));
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdRotate }));
    // A confirm dialog gates the actual rotation call.
    expect(await screen.findByText(t.settingsNetbirdRotateConfirmBody)).toBeInTheDocument();
    expect(fakeApi.rotateNetbirdToken).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdRotateConfirmAction }));
    await waitFor(() => expect(fakeApi.rotateNetbirdToken).toHaveBeenCalled());
    // Success toast carries the new expiry/days AND the old-deleted tail.
    expect(
      await screen.findByText(
        `${t.settingsNetbirdRotateOk('2028-01-01', 365)} ${t.settingsNetbirdRotateOldDeleted}`,
      ),
    ).toBeInTheDocument();
    // The validity line reloads (statusNonce bump) to the freshly rotated token.
    expect(
      await screen.findByText(t.settingsNetbirdTokenValid('2028-01-01', 365)),
    ).toBeInTheDocument();
    expect(fakeApi.netbirdTokenStatus).toHaveBeenCalledTimes(2);
  });

  it('appends the old-token-unknown tail when the previous token could not be identified', async () => {
    const rotateNetbirdToken = vi.fn(
      async () =>
        ({
          expiration_date: '2028-01-01',
          days_remaining: 365,
          old_deleted: false,
          old_unknown: true,
        }) satisfies RotateNetbirdTokenResult,
    );
    renderNetbirdSettings(enabledSettings(), { rotateNetbirdToken } as Partial<NetbirdSettingsApi>);
    fireEvent.click(await screen.findByRole('button', { name: t.settingsNetbirdRotate }));
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdRotateConfirmAction }));
    await waitFor(() => expect(rotateNetbirdToken).toHaveBeenCalled());
    expect(
      await screen.findByText(
        `${t.settingsNetbirdRotateOk('2028-01-01', 365)} ${t.settingsNetbirdRotateOldUnknown}`,
      ),
    ).toBeInTheDocument();
  });

  it('shows a rollback toast when the rotation fails, leaving the previous token untouched', async () => {
    const rotateNetbirdToken = vi.fn(async () => {
      throw new Error('netbird rejected the new token');
    });
    const netbirdTokenStatus = vi.fn(async () =>
      makeTokenStatus({ known: true, expiration_date: '2027-01-01', days_remaining: 5 }),
    );
    const { fakeApi } = renderNetbirdSettings(enabledSettings(), {
      rotateNetbirdToken,
      netbirdTokenStatus,
    } as Partial<NetbirdSettingsApi>);
    await screen.findByText(t.settingsNetbirdTokenValid('2027-01-01', 5));
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdRotate }));
    fireEvent.click(screen.getByRole('button', { name: t.settingsNetbirdRotateConfirmAction }));
    await waitFor(() => expect(fakeApi.rotateNetbirdToken).toHaveBeenCalled());
    expect(await screen.findByText(t.settingsNetbirdRotateFailed)).toBeInTheDocument();
    // The (unchanged) previous validity is still what's displayed — no reload
    // was warranted since the rotation never persisted anything new.
    expect(
      await screen.findByText(t.settingsNetbirdTokenValid('2027-01-01', 5)),
    ).toBeInTheDocument();
  });

  it('cancelling the rotate confirm dialog never calls rotateNetbirdToken', async () => {
    const { fakeApi } = renderNetbirdSettings(enabledSettings());
    fireEvent.click(await screen.findByRole('button', { name: t.settingsNetbirdRotate }));
    expect(await screen.findByText(t.settingsNetbirdRotateConfirmBody)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(fakeApi.rotateNetbirdToken).not.toHaveBeenCalled();
  });

  it('shows the auto-rotation threshold field, defaulted from settings, and includes it in the Admin save body', async () => {
    const { updateSystemSettings } = renderNetbirdSettings(
      enabledSettings({ netbird_token_rotate_before_days: 14 }),
    );
    const field = (await screen.findByLabelText(
      t.settingsNetbirdTokenRotateBefore,
    )) as HTMLInputElement;
    expect(field.value).toBe('14');
    fireEvent.change(field, { target: { value: '7' } });
    const save = sectionSaveButton(t.settingsNetbirdSectionAdmin);
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      netbird_token_rotate_before_days: 7,
    });
  });
});
