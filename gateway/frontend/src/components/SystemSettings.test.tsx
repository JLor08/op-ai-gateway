// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SystemSettings } from './SystemSettings';
import { ToastProvider } from './shared/ToastProvider';
import { ThemeControlsContext, type ThemeControls } from '../theme/useThemeControls';
import { messages } from '../i18n';
import type { SystemSettings as SystemSettingsDTO } from '../api';
import type { PortalApi } from './shared/types';

// NOTE on i18n key reuse: SystemSettings' NetBird panel renders ONLY the enable
// checkbox (`settingsNetbirdTitle`/`settingsNetbirdIntro`/`settingsNetbirdEnable`).
// Everything else — url/groups/token/test + the operational settings — lives in
// the separate NetbirdSettings view (see NetbirdSettings.test.tsx), which reuses
// the SAME `settingsNetbird*` i18n keys for its own (different) panel; this is
// intentional (the design explicitly reuses existing keys, no new ones).

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

function renderSystemSettings(
  initial: SystemSettingsDTO,
  overrides: Partial<
    Pick<PortalApi, 'getSystemSettings' | 'testSmtp' | 'updateSystemSettings'>
  > = {},
) {
  let current = initial;
  const updateSystemSettings = vi.fn(async (body: Record<string, unknown>) => {
    current = { ...current, ...body } as SystemSettingsDTO;
    return current;
  });
  const fakeApi: Pick<PortalApi, 'getSystemSettings' | 'testSmtp' | 'updateSystemSettings'> = {
    getSystemSettings: vi.fn(async () => current),
    updateSystemSettings,
    testSmtp: vi.fn(async () => ({ ok: true })),
    ...overrides,
  };
  const onSaved = vi.fn();

  const themeControls: ThemeControls = {
    activeThemeId: 'default',
    setActiveThemeId: vi.fn(),
    reloadTheme: vi.fn(),
    pref: 'system',
    effective: 'light',
    setMode: vi.fn(),
    toggle: vi.fn(),
    hasDark: true,
    brand: { mark: { type: 'text', text: 'OP' }, title: 'AI Gateway' },
    productName: 'On-Prem AI Gateway',
  };

  render(
    <ThemeControlsContext.Provider value={themeControls}>
      <ToastProvider>
        <SystemSettings t={t} api={fakeApi} onSaved={onSaved} />
      </ToastProvider>
    </ThemeControlsContext.Provider>,
  );

  return { fakeApi, updateSystemSettings, onSaved };
}

afterEach(cleanup);

describe('SystemSettings theme picker', () => {
  it('renders theme options by display name (available_themes is [{id,name}])', async () => {
    renderSystemSettings(makeSettings({ available_themes: [{ id: 'default', name: 'Default' }] }));

    const select = await screen.findByLabelText(t.systemThemeLabel);
    fireEvent.mouseDown(select);
    expect(await screen.findByRole('option', { name: 'Default' })).toBeInTheDocument();
  });
});

describe('SystemSettings capture_override', () => {
  it('renders the capture_override checkbox unchecked and saves capture_override:true', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ capture_override: false }),
    );

    const checkbox = await screen.findByLabelText(t.captureOverrideLabel);
    expect(checkbox).not.toBeChecked();

    fireEvent.click(checkbox);
    expect(checkbox).toBeChecked();

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ capture_override: true });
  });

  it('calls onSaved after a successful save (so the shell can refresh the NetBird nav)', async () => {
    const { updateSystemSettings, onSaved } = renderSystemSettings(
      makeSettings({ capture_override: false }),
    );

    const checkbox = await screen.findByLabelText(t.captureOverrideLabel);
    fireEvent.click(checkbox);

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
  });
});

describe('SystemSettings health_check_interval_seconds', () => {
  it('renders the interval field with the current value and includes it in the save payload', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ health_check_interval_seconds: 30 }),
    );

    const field = (await screen.findByLabelText(t.healthIntervalLabel)) as HTMLInputElement;
    expect(field.value).toBe('30');

    fireEvent.change(field, { target: { value: '60' } });

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      health_check_interval_seconds: 60,
    });
  });

  it('disables save when the interval is out of the 5–3600 range', async () => {
    renderSystemSettings(makeSettings({ health_check_interval_seconds: 30 }));

    const field = (await screen.findByLabelText(t.healthIntervalLabel)) as HTMLInputElement;
    const save = screen.getByRole('button', { name: t.save });

    fireEvent.change(field, { target: { value: '4' } });
    expect(save).toBeDisabled();

    fireEvent.change(field, { target: { value: '3601' } });
    expect(save).toBeDisabled();

    fireEvent.change(field, { target: { value: '30' } });
    expect(save).not.toBeDisabled();
  });
});

describe('SystemSettings agent_presence_timeout_seconds', () => {
  it('renders the agent-presence-timeout field with the current value and includes it in the save payload', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ agent_presence_timeout_seconds: 15 }),
    );

    const field = (await screen.findByLabelText(
      t.settingsAgentPresenceTimeoutLabel,
    )) as HTMLInputElement;
    expect(field.value).toBe('15');

    fireEvent.change(field, { target: { value: '30' } });

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      agent_presence_timeout_seconds: 30,
    });
  });

  it('disables save when the agent-presence timeout is out of the 3–3600 range', async () => {
    renderSystemSettings(makeSettings({ agent_presence_timeout_seconds: 15 }));

    const field = (await screen.findByLabelText(
      t.settingsAgentPresenceTimeoutLabel,
    )) as HTMLInputElement;
    const save = screen.getByRole('button', { name: t.save });

    fireEvent.change(field, { target: { value: '2' } });
    expect(save).toBeDisabled();

    fireEvent.change(field, { target: { value: '3601' } });
    expect(save).toBeDisabled();

    fireEvent.change(field, { target: { value: '15' } });
    expect(save).not.toBeDisabled();
  });
});

describe('SystemSettings SMTP panel', () => {
  it("keeps the password on save when the input is left blank (placeholder shows 'gesetzt')", async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        smtp_enabled: true,
        smtp_host: 'mx.local',
        smtp_from: 'no-reply@x.io',
        smtp_password_set: true,
      }),
    );
    const pw = (await screen.findByLabelText(t.smtpPasswordLabel)) as HTMLInputElement;
    expect(pw.placeholder).toBe(t.smtpPasswordSetPlaceholder);
    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).not.toHaveProperty('smtp_password');
  });

  it("sends smtp_password:'' when the clear affordance is used", async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        smtp_enabled: true,
        smtp_host: 'mx.local',
        smtp_from: 'no-reply@x.io',
        smtp_password_set: true,
      }),
    );
    fireEvent.click(await screen.findByRole('button', { name: t.smtpPasswordClear }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ smtp_password: '' });
  });

  it('blocks save while enabled without host/from', async () => {
    renderSystemSettings(makeSettings({ smtp_enabled: true, smtp_host: '', smtp_from: '' }));
    await screen.findByLabelText(t.smtpHostLabel);
    expect(screen.getByRole('button', { name: t.save })).toBeDisabled();
  });

  it('calls testSmtp and toasts the result', async () => {
    const { fakeApi } = renderSystemSettings(
      makeSettings({ smtp_enabled: true, smtp_host: 'mx', smtp_from: 'a@b.io' }),
    );
    fireEvent.change(await screen.findByLabelText(t.smtpTestToLabel), {
      target: { value: 'me@x.io' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.smtpTestButton }));
    await waitFor(() => expect(fakeApi.testSmtp).toHaveBeenCalledWith({ to: 'me@x.io' }));
    expect(await screen.findByText(t.smtpTestSuccess)).toBeInTheDocument();
  });
});

describe('SystemSettings totp_mode', () => {
  it('saves the selected totp_mode', async () => {
    const { updateSystemSettings } = renderSystemSettings(makeSettings({ totp_mode: 'off' }));
    fireEvent.mouseDown(await screen.findByLabelText(t.totpModeLabel));
    fireEvent.click(await screen.findByRole('option', { name: t.totpModeRequired }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ totp_mode: 'required' });
  });
});

describe('SystemSettings route_affinity_session_mode', () => {
  it('renders both options, defaults to the stored mode, and saves the selected mode', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ route_affinity_session_mode: 'client_session' }),
    );
    const field = await screen.findByRole('combobox', { name: t.settingsRouteAffinityModeLabel });
    expect(field).toHaveTextContent(t.settingsRouteAffinityModeClient);

    fireEvent.mouseDown(field);
    // Both options are present in the dropdown.
    expect(
      await screen.findByRole('option', { name: t.settingsRouteAffinityModeClient }),
    ).toBeInTheDocument();
    fireEvent.click(await screen.findByRole('option', { name: t.settingsRouteAffinityModeLegacy }));

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      route_affinity_session_mode: 'legacy_header',
    });
  });
});

describe('SystemSettings vision_probe_mode', () => {
  it('saves the selected vision_probe_mode', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ vision_probe_mode: 'accept' }),
    );
    fireEvent.mouseDown(await screen.findByLabelText(t.settingsVisionProbeMode));
    fireEvent.click(await screen.findByRole('option', { name: t.settingsVisionProbeModeVerify }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ vision_probe_mode: 'verify' });
  });
});

describe('SystemSettings energy defaults', () => {
  // energy_default_price_per_kwh is stored canonically in EUR; the price field
  // displays it converted into the current price_unit (default "eur_cent" per
  // makeSettings() => 0.3 EUR/kWh displays as "30").
  it('renders the energy-default fields with the current values (price shown in its unit) and includes them in the save payload', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        energy_default_price_per_kwh: 0.3,
        energy_default_pue: 1.3,
        energy_default_wh_per_token: 0.002,
      }),
    );

    const priceField = (await screen.findByLabelText(
      t.settingsEnergyPricePerKwh,
    )) as HTMLInputElement;
    expect(priceField.value).toBe('30');
    const pueField = screen.getByLabelText(t.settingsEnergyPue) as HTMLInputElement;
    expect(pueField.value).toBe('1.3');
    const whField = screen.getByLabelText(t.settingsEnergyWhPerToken) as HTMLInputElement;
    expect(whField.value).toBe('0.002');

    fireEvent.change(priceField, { target: { value: '32' } });
    fireEvent.change(pueField, { target: { value: '1.4' } });
    fireEvent.change(whField, { target: { value: '0.0025' } });

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      energy_default_price_per_kwh: 0.32,
      energy_default_price_unit: 'eur_cent',
      energy_default_pue: 1.4,
      energy_default_wh_per_token: 0.0025,
    });
  });

  it('disables save when an energy default is negative', async () => {
    renderSystemSettings(makeSettings({ energy_default_price_per_kwh: 0.3 }));

    const priceField = (await screen.findByLabelText(
      t.settingsEnergyPricePerKwh,
    )) as HTMLInputElement;
    const save = screen.getByRole('button', { name: t.save });

    fireEvent.change(priceField, { target: { value: '-0.1' } });
    expect(save).toBeDisabled();

    fireEvent.change(priceField, { target: { value: '30' } });
    expect(save).not.toBeDisabled();
  });
});

describe('SystemSettings currency factor + price unit', () => {
  it('saves energy_default_price_per_kwh normalized to EUR when entering a value in eur_cent', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        energy_default_price_per_kwh: 0,
        energy_default_price_unit: 'eur_cent',
        currency_usd_per_eur: 1.1,
      }),
    );

    const priceField = (await screen.findByLabelText(
      t.settingsEnergyPricePerKwh,
    )) as HTMLInputElement;
    fireEvent.change(priceField, { target: { value: '30' } });

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      energy_default_price_per_kwh: 0.3,
      energy_default_price_unit: 'eur_cent',
    });
  });

  it('round-trips the conversion-factor field into the save payload', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ currency_usd_per_eur: 0 }),
    );

    const factorField = (await screen.findByLabelText(t.systemCurrencyFactor)) as HTMLInputElement;
    expect(factorField.value).toBe('0');
    fireEvent.change(factorField, { target: { value: '1.1' } });

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ currency_usd_per_eur: 1.1 });
  });

  it('re-displays the same underlying price after switching units, without reinterpreting the typed number', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        energy_default_price_per_kwh: 0,
        energy_default_price_unit: 'eur_cent',
        currency_usd_per_eur: 0,
      }),
    );

    const priceField = (await screen.findByLabelText(
      t.settingsEnergyPricePerKwh,
    )) as HTMLInputElement;
    fireEvent.change(priceField, { target: { value: '30' } });

    const unitField = await screen.findByLabelText(t.priceUnitLabel);
    fireEvent.mouseDown(unitField);
    fireEvent.click(await screen.findByRole('option', { name: t.currencyUnitEur }));

    // 30 eur_cent == 0.3 EUR -> switching to "eur" must re-display "0.3", not "30".
    expect(priceField.value).toBe('0.3');

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      energy_default_price_per_kwh: 0.3,
      energy_default_price_unit: 'eur',
    });
  });

  it('hides USD units when the conversion factor is 0', async () => {
    renderSystemSettings(makeSettings({ currency_usd_per_eur: 0 }));
    const unitField = await screen.findByLabelText(t.priceUnitLabel);
    fireEvent.mouseDown(unitField);
    expect(screen.queryByRole('option', { name: t.currencyUnitUsd })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: t.currencyUnitUsdCent })).not.toBeInTheDocument();
  });

  // Review finding (MEDIUM, data-loss): a stored USD default price unit with
  // the conversion factor at 0 (USD unavailable) must degrade to eur_cent for
  // display/save — never show/save 0 (fromEur(x,"usd",0)===0) and never leave
  // the unit Select out of its own option set (which excludes USD at factor 0).
  it('degrades a stored USD price unit to eur_cent when the conversion factor is 0 (no data loss)', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({
        energy_default_price_unit: 'usd',
        energy_default_price_per_kwh: 0.3,
        currency_usd_per_eur: 0,
      }),
    );

    // The stored unit is "usd", but the factor is 0 -> the field must show the
    // eur_cent-converted price (30), never 0 (which fromEur(0.3,"usd",0) would give).
    const priceField = (await screen.findByLabelText(
      t.settingsEnergyPricePerKwh,
    )) as HTMLInputElement;
    expect(priceField.value).toBe('30');

    // The unit Select must show the degraded eur_cent option, never a blank/
    // out-of-range "usd".
    const unitField = screen.getByRole('combobox', { name: t.priceUnitLabel });
    expect(unitField).toHaveTextContent(t.currencyUnitEurCent);

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    // Must round-trip the original 0.3 EUR (via eur_cent "30"), never 0 and
    // never the raw stored "usd" unit.
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      energy_default_price_per_kwh: 0.3,
      energy_default_price_unit: 'eur_cent',
    });
  });
});

describe('SystemSettings NetBird enable checkbox', () => {
  it('renders ONLY the NetBird enable checkbox — url/groups/token/test live in the separate NetbirdSettings view', async () => {
    renderSystemSettings(makeSettings());
    expect((await screen.findAllByText(t.settingsNetbirdTitle)).length).toBeGreaterThan(0);
    await screen.findByLabelText(t.settingsNetbirdEnable);
    expect(screen.queryByLabelText(t.settingsNetbirdUrl)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.settingsNetbirdToken)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.settingsNetbirdTest })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('combobox', { name: t.settingsNetbirdGroups }),
    ).not.toBeInTheDocument();
  });

  it('saves netbird_enabled only — no url/token/groups nor any operational NetBird field', async () => {
    const { updateSystemSettings } = renderSystemSettings(makeSettings({ netbird_enabled: false }));

    const toggle = (await screen.findByLabelText(t.settingsNetbirdEnable)) as HTMLInputElement;
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);
    expect(toggle).toBeChecked();

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    const body = updateSystemSettings.mock.calls[0][0] as Record<string, unknown>;
    expect(body).toMatchObject({ netbird_enabled: true });
    // The module-config (url/groups/token) AND operational NetBird fields all
    // belong to the separate NetbirdSettings save, not this one — the two
    // payloads must stay disjoint.
    expect(body).not.toHaveProperty('netbird_url');
    expect(body).not.toHaveProperty('netbird_groups');
    expect(body).not.toHaveProperty('netbird_token');
    expect(body).not.toHaveProperty('netbird_only');
    expect(body).not.toHaveProperty('netbird_gateway_peer_id');
    expect(body).not.toHaveProperty('netbird_gateway_peer_name');
    expect(body).not.toHaveProperty('netbird_manage_policies');
    expect(body).not.toHaveProperty('netbird_policy_scope');
    expect(body).not.toHaveProperty('netbird_deny_by_default');
    expect(body).not.toHaveProperty('netbird_deny_by_default_enforce');
    expect(body).not.toHaveProperty('netbird_peer_sync_interval_seconds');
    expect(body).not.toHaveProperty('netbird_reconcile_interval_seconds');
    expect(body).not.toHaveProperty('netbird_allow_ping_gateway');
    expect(body).not.toHaveProperty('netbird_allow_ping_all_servers');
  });

  it('save is never gated on url/token config — an enabled-but-unconfigured module can still be saved', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ netbird_enabled: true, netbird_url: '', netbird_token_set: false }),
    );
    const toggle = await screen.findByLabelText(t.settingsNetbirdEnable);
    const save = screen.getByRole('button', { name: t.save });
    expect(save).not.toBeDisabled();
    fireEvent.click(toggle);
    fireEvent.click(save);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ netbird_enabled: false });
  });
});

describe('SystemSettings resource_provisioning_enforce (Phase 2, spec 2026-08-12-resource-groups-phase-2-provisioning)', () => {
  it('renders the checkbox unchecked and saves resource_provisioning_enforce:true', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ resource_provisioning_enforce: false }),
    );

    const toggle = await screen.findByLabelText(t.settingsResourceProvisioningEnforceLabel);
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);
    expect(toggle).toBeChecked();

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      resource_provisioning_enforce: true,
    });
  });

  it('renders already-checked when the loaded setting is on, and saves the flag back off', async () => {
    const { updateSystemSettings } = renderSystemSettings(
      makeSettings({ resource_provisioning_enforce: true }),
    );

    const toggle = await screen.findByLabelText(t.settingsResourceProvisioningEnforceLabel);
    expect(toggle).toBeChecked();
    fireEvent.click(toggle);
    expect(toggle).not.toBeChecked();

    const save = screen.getByRole('button', { name: t.save });
    await waitFor(() => expect(save).not.toBeDisabled());
    fireEvent.click(save);

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalled());
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      resource_provisioning_enforce: false,
    });
  });
});
