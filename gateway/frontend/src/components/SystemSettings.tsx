// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import { Box, Button, Checkbox, FormControlLabel, FormHelperText, Stack } from '@mui/material';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { useThemeControls } from '../theme/useThemeControls';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { Field } from './shared/Field';
import { useToast } from './shared/ToastProvider';
import { availableUnits, fromEur, toEur, type CurrencyUnit } from '../currency';

const LANGUAGE_LABELS: Record<string, string> = { de: 'Deutsch', en: 'English' };

// Renders a number for display without float noise (e.g. 0.3*100 must read
// "30", not "30.000000000000004"); rounds to 6 decimal places. Non-finite -> "".
function fmtNum(n: number): string {
  if (!Number.isFinite(n)) return '';
  return String(Math.round(n * 1e6) / 1e6);
}

// Option label for a currency-unit dropdown entry.
function unitLabel(t: Translation, u: CurrencyUnit): string {
  switch (u) {
    case 'eur':
      return t.currencyUnitEur;
    case 'eur_cent':
      return t.currencyUnitEurCent;
    case 'usd':
      return t.currencyUnitUsd;
    case 'usd_cent':
      return t.currencyUnitUsdCent;
  }
}

export function SystemSettings({
  t,
  api,
  onSaved,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'getSystemSettings' | 'testSmtp' | 'updateSystemSettings'>;
  onSaved?: () => void;
}>) {
  const { reloadTheme } = useThemeControls();
  const { showSuccess, showError } = useToast();
  const {
    data: settings,
    setData: setSettings,
    loading,
    error,
  } = useResource(() => api.getSystemSettings(), [api, t], t);
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const [pending, setPending] = useState<string | null>(null);
  const [pendingLang, setPendingLang] = useState<string | null>(null);
  const [pendingRetention, setPendingRetention] = useState<string | null>(null);
  const [pendingCaptureEnabled, setPendingCaptureEnabled] = useState<boolean | null>(null);
  const [pendingCaptureOverride, setPendingCaptureOverride] = useState<boolean | null>(null);
  const [pendingInterval, setPendingInterval] = useState<string | null>(null);
  const [pendingAgentPresenceTimeout, setPendingAgentPresenceTimeout] = useState<string | null>(
    null,
  );
  const [pendingTotpMode, setPendingTotpMode] = useState<string | null>(null);
  const [pendingAffinityMode, setPendingAffinityMode] = useState<string | null>(null);
  const [pendingVisionProbeMode, setPendingVisionProbeMode] = useState<string | null>(null);
  // pendingEnergyPricePerKwh holds the price DISPLAY string in the currently
  // selected unit (pendingPriceUnit ?? the stored default), never raw EUR —
  // see the seed/convert/save derivation below.
  const [pendingEnergyPricePerKwh, setPendingEnergyPricePerKwh] = useState<string | null>(null);
  const [pendingPriceUnit, setPendingPriceUnit] = useState<CurrencyUnit | null>(null);
  const [pendingCurrencyFactor, setPendingCurrencyFactor] = useState<string | null>(null);
  const [pendingEnergyPue, setPendingEnergyPue] = useState<string | null>(null);
  const [pendingEnergyWhPerToken, setPendingEnergyWhPerToken] = useState<string | null>(null);
  const [pendingSmtpEnabled, setPendingSmtpEnabled] = useState<boolean | null>(null);
  const [pendingHost, setPendingHost] = useState<string | null>(null);
  const [pendingPort, setPendingPort] = useState<string | null>(null);
  const [pendingUsername, setPendingUsername] = useState<string | null>(null);
  const [pendingFrom, setPendingFrom] = useState<string | null>(null);
  const [pendingFromName, setPendingFromName] = useState<string | null>(null);
  const [pendingTls, setPendingTls] = useState<string | null>(null);
  // Write-only password: "" = keep (untouched), a typed value = replace, cleared flag = send "".
  const [passwordInput, setPasswordInput] = useState('');
  const [passwordCleared, setPasswordCleared] = useState(false);
  const [testTo, setTestTo] = useState('');
  const [testing, setTesting] = useState(false);
  const [busy, setBusy] = useState(false);
  // NetBird MODULE enable checkbox — the ONLY NetBird control in System Settings.
  // Everything else (url/groups/token/test + the operational netbird_only,
  // gateway peer, setup-key, policy management) lives in the separate
  // NetbirdSettings view, which is only reachable once this checkbox is on. The
  // checkbox itself must stay reachable from here, always (else there would be
  // no way to ever turn the module on).
  const [pendingNetbirdEnabled, setPendingNetbirdEnabled] = useState<boolean | null>(null);
  // System-admin step-up mode: whether elevating requires re-entering the
  // account password (default true, matches the backend default).
  const [pendingSystemAdminRequirePassword, setPendingSystemAdminRequirePassword] = useState<
    boolean | null
  >(null);
  // Resource-group provisioning enforcement (Phase 2, spec
  // 2026-08-12-resource-groups-phase-2-provisioning): off (default) =
  // provisioning is an additional opt-in grant; on = deny-by-default.
  const [pendingResourceProvisioningEnforce, setPendingResourceProvisioningEnforce] = useState<
    boolean | null
  >(null);
  // TLS-certificate module enable checkbox — the ONLY certificate control in
  // System Settings. Everything else (issuer mode, ACME/self-signed fields,
  // internal-CA panel, certificate list, per-server override) lives in the
  // separate CertificateSettings view, which is only reachable once this
  // checkbox is on (mirrors the NetBird module checkbox above).
  const [pendingCertEnabled, setPendingCertEnabled] = useState<boolean | null>(null);

  const availableThemes = settings?.available_themes ?? [];
  const theme = pending ?? settings?.theme ?? '';
  const availableLanguages = settings?.available_languages ?? [];
  const language = pendingLang ?? settings?.language ?? '';
  const retentionValue = pendingRetention ?? String(settings?.capture_retention_days ?? 30);
  const retentionNum = Number(retentionValue);
  const retentionValid = Number.isInteger(retentionNum) && retentionNum >= 1 && retentionNum <= 365;
  const captureEnabled = pendingCaptureEnabled ?? settings?.capture_enabled ?? true;
  const captureOverride = pendingCaptureOverride ?? settings?.capture_override ?? false;
  const intervalValue = pendingInterval ?? String(settings?.health_check_interval_seconds ?? 30);
  const intervalNum = Number(intervalValue);
  const intervalValid = Number.isInteger(intervalNum) && intervalNum >= 5 && intervalNum <= 3600;
  const agentPresenceTimeoutValue =
    pendingAgentPresenceTimeout ?? String(settings?.agent_presence_timeout_seconds ?? 15);
  const agentPresenceTimeoutNum = Number(agentPresenceTimeoutValue);
  const agentPresenceTimeoutValid =
    Number.isInteger(agentPresenceTimeoutNum) &&
    agentPresenceTimeoutNum >= 3 &&
    agentPresenceTimeoutNum <= 3600;
  const totpMode = pendingTotpMode ?? settings?.totp_mode ?? 'off';
  const affinityMode =
    pendingAffinityMode ?? settings?.route_affinity_session_mode ?? 'client_session';
  const visionProbeMode = pendingVisionProbeMode ?? settings?.vision_probe_mode ?? 'accept';
  // Currency conversion factor (USD per 1 EUR) driving USD-unit availability;
  // arrives on the SAME settings load as the price/unit fields, so (unlike
  // ServerList's separate api.getCurrency() fetch) there is no factor-arrives-
  // after-seed race to guard against here.
  const currencyFactorValue = pendingCurrencyFactor ?? String(settings?.currency_usd_per_eur ?? 0);
  const currencyFactorNum = Number(currencyFactorValue);
  const currencyFactorValid = Number.isFinite(currencyFactorNum) && currencyFactorNum >= 0;

  const priceUnit: CurrencyUnit =
    pendingPriceUnit ?? settings?.energy_default_price_unit ?? 'eur_cent';
  const priceUnitOptions = availableUnits(currencyFactorNum);
  // A stored/pending USD unit degrades to eur_cent for DISPLAY/SAVE when the
  // conversion factor is 0 (USD unavailable) — never mutate the raw `priceUnit`
  // state itself, only derive this for rendering (mirrors Activity.tsx's
  // `effectiveCostUnit`). Without this, fromEur(x,"usd",0)===0 would show a
  // blank price AND a save would silently zero the stored EUR value.
  const effectiveUnit: CurrencyUnit = priceUnitOptions.includes(priceUnit) ? priceUnit : 'eur_cent';

  // The price field's DISPLAY value is the stored EUR price converted into the
  // EFFECTIVE unit; switching the unit re-displays the SAME underlying price
  // (see changePriceUnit), never a re-parse of the raw number. Once the
  // operator types, pendingEnergyPricePerKwh (in the CURRENT unit) takes over.
  const energyPricePerKwhValue =
    pendingEnergyPricePerKwh ??
    fmtNum(fromEur(settings?.energy_default_price_per_kwh ?? 0, effectiveUnit, currencyFactorNum));
  const energyPricePerKwhNum = Number(energyPricePerKwhValue);
  const energyPricePerKwhValid = Number.isFinite(energyPricePerKwhNum) && energyPricePerKwhNum >= 0;

  // Unit-dropdown change: convert the CURRENTLY SHOWN value from the effective
  // (in-range) unit to EUR and back into the new unit, so the underlying price
  // is preserved exactly (never reinterpreted as a raw number in the new unit).
  function changePriceUnit(newUnit: CurrencyUnit) {
    const eur = toEur(energyPricePerKwhNum, effectiveUnit, currencyFactorNum);
    setPendingEnergyPricePerKwh(fmtNum(fromEur(eur, newUnit, currencyFactorNum)));
    setPendingPriceUnit(newUnit);
  }
  const energyPueValue = pendingEnergyPue ?? String(settings?.energy_default_pue ?? 0);
  const energyPueNum = Number(energyPueValue);
  const energyPueValid = Number.isFinite(energyPueNum) && energyPueNum >= 0;
  const energyWhPerTokenValue =
    pendingEnergyWhPerToken ?? String(settings?.energy_default_wh_per_token ?? 0);
  const energyWhPerTokenNum = Number(energyWhPerTokenValue);
  const energyWhPerTokenValid = Number.isFinite(energyWhPerTokenNum) && energyWhPerTokenNum >= 0;
  const smtpEnabled = pendingSmtpEnabled ?? settings?.smtp_enabled ?? false;
  const smtpHost = pendingHost ?? settings?.smtp_host ?? '';
  const smtpPortStr = pendingPort ?? String(settings?.smtp_port ?? 587);
  const smtpPortNum = Number(smtpPortStr);
  const smtpPortValid = Number.isInteger(smtpPortNum) && smtpPortNum >= 1 && smtpPortNum <= 65535;
  const smtpUsername = pendingUsername ?? settings?.smtp_username ?? '';
  const smtpFrom = pendingFrom ?? settings?.smtp_from ?? '';
  const smtpFromName = pendingFromName ?? settings?.smtp_from_name ?? '';
  const smtpTls = pendingTls ?? settings?.smtp_tls_mode ?? 'starttls';
  // An invalid port must ALWAYS block Save (even with SMTP disabled): the save
  // payload sends smtp_port unconditionally, and Number("") is 0 which the
  // backend rejects (ErrSmtpPortInvalid). So gate on smtpPortValid regardless.
  const smtpConfigOk =
    smtpPortValid && (!smtpEnabled || (smtpHost.trim() !== '' && smtpFrom.trim() !== ''));

  const netbirdEnabled = pendingNetbirdEnabled ?? settings?.netbird_enabled ?? false;
  const systemAdminRequirePassword =
    pendingSystemAdminRequirePassword ?? settings?.system_admin_mode_require_password ?? true;
  const resourceProvisioningEnforce =
    pendingResourceProvisioningEnforce ?? settings?.resource_provisioning_enforce ?? false;
  const certEnabled = pendingCertEnabled ?? settings?.cert_enabled ?? false;

  async function save() {
    setBusy(true);
    try {
      let smtpPassword: string | undefined;
      if (passwordCleared) {
        smtpPassword = '';
      } else if (passwordInput !== '') {
        smtpPassword = passwordInput;
      }
      const updated = await api.updateSystemSettings({
        theme,
        language,
        capture_retention_days: retentionNum,
        capture_enabled: captureEnabled,
        capture_override: captureOverride,
        health_check_interval_seconds: intervalNum,
        agent_presence_timeout_seconds: agentPresenceTimeoutNum,
        totp_mode: totpMode,
        route_affinity_session_mode: affinityMode,
        vision_probe_mode: visionProbeMode,
        energy_default_price_per_kwh: toEur(energyPricePerKwhNum, effectiveUnit, currencyFactorNum),
        energy_default_price_unit: effectiveUnit,
        currency_usd_per_eur: currencyFactorNum,
        energy_default_pue: energyPueNum,
        energy_default_wh_per_token: energyWhPerTokenNum,
        smtp_enabled: smtpEnabled,
        smtp_host: smtpHost,
        smtp_port: smtpPortNum,
        smtp_username: smtpUsername,
        smtp_from: smtpFrom,
        smtp_from_name: smtpFromName,
        smtp_tls_mode: smtpTls,
        ...(smtpPassword !== undefined ? { smtp_password: smtpPassword } : {}),
        netbird_enabled: netbirdEnabled,
        system_admin_mode_require_password: systemAdminRequirePassword,
        resource_provisioning_enforce: resourceProvisioningEnforce,
        cert_enabled: certEnabled,
      });
      setSettings(updated);
      setPending(null);
      setPendingLang(null);
      setPendingRetention(null);
      setPendingCaptureEnabled(null);
      setPendingCaptureOverride(null);
      setPendingInterval(null);
      setPendingAgentPresenceTimeout(null);
      setPendingTotpMode(null);
      setPendingAffinityMode(null);
      setPendingVisionProbeMode(null);
      setPendingEnergyPricePerKwh(null);
      setPendingPriceUnit(null);
      setPendingCurrencyFactor(null);
      setPendingEnergyPue(null);
      setPendingEnergyWhPerToken(null);
      setPendingSmtpEnabled(null);
      setPendingHost(null);
      setPendingPort(null);
      setPendingUsername(null);
      setPendingFrom(null);
      setPendingFromName(null);
      setPendingTls(null);
      setPasswordInput('');
      setPasswordCleared(false);
      setPendingNetbirdEnabled(null);
      setPendingSystemAdminRequirePassword(null);
      setPendingResourceProvisioningEnforce(null);
      setPendingCertEnabled(null);
      showSuccess(t.systemSaved);
      reloadTheme();
      // The NetBird module toggle lives here; let the shell re-check it so the
      // NetBird nav item appears/disappears live (no manual refresh needed).
      onSaved?.();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function sendTest() {
    setTesting(true);
    try {
      const res = await api.testSmtp({ to: testTo.trim() || undefined });
      if (res.ok) showSuccess(t.smtpTestSuccess);
      else showError(t.smtpTestError(res.error ?? ''));
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setTesting(false);
    }
  }

  return (
    <>
      <PageTitle title={t.system} subtitle={t.systemIntro} />
      {loading && <p>{t.loading}</p>}
      <Stack spacing={2.5}>
        <Panel
          titleId="system-appearance-heading"
          title={t.systemAppearanceTitle}
          subtitle={t.systemAppearanceIntro}
        >
          <Stack spacing={3}>
            <SelectField
              id="system-theme"
              label={t.systemThemeLabel}
              value={theme}
              onChange={(event) => setPending(event.target.value)}
              helperText={t.systemThemeNote}
            >
              {availableThemes.map((option) => (
                <option value={option.id} key={option.id}>
                  {option.name}
                </option>
              ))}
            </SelectField>
            <SelectField
              id="system-language"
              label={t.systemLanguageLabel}
              value={language}
              onChange={(event) => setPendingLang(event.target.value)}
              helperText={t.systemLanguageNote}
            >
              {availableLanguages.map((option) => (
                <option value={option} key={option}>
                  {LANGUAGE_LABELS[option] ?? option}
                </option>
              ))}
            </SelectField>
          </Stack>
        </Panel>

        <Panel
          titleId="system-capture-heading"
          title={t.systemCaptureTitle}
          subtitle={t.systemCaptureIntro}
        >
          <Stack spacing={3}>
            <Box>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={captureEnabled}
                    onChange={(event) => setPendingCaptureEnabled(event.target.checked)}
                  />
                }
                label={t.captureEnabledLabel}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>{t.captureEnabledNote}</FormHelperText>
            </Box>
            <Box>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={captureOverride}
                    onChange={(event) => setPendingCaptureOverride(event.target.checked)}
                  />
                }
                label={t.captureOverrideLabel}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>{t.captureOverrideNote}</FormHelperText>
            </Box>
            <Field
              id="system-capture-retention"
              type="number"
              label={t.captureRetentionLabel}
              value={retentionValue}
              onChange={(event) => setPendingRetention(event.target.value)}
              error={!retentionValid}
              helperText={retentionValid ? t.captureRetentionNote : t.captureRetentionError}
              inputProps={{ min: 1, max: 365, step: 1 }}
            />
          </Stack>
        </Panel>

        <Panel titleId="system-health-heading" title={t.systemHealthTitle}>
          <Stack spacing={3}>
            <Field
              id="system-health-interval"
              type="number"
              label={t.healthIntervalLabel}
              value={intervalValue}
              onChange={(event) => setPendingInterval(event.target.value)}
              error={!intervalValid}
              helperText={intervalValid ? t.healthIntervalNote : t.healthIntervalError}
              inputProps={{ min: 5, max: 3600, step: 1 }}
            />
            <Field
              id="system-agent-presence-timeout"
              type="number"
              label={t.settingsAgentPresenceTimeoutLabel}
              value={agentPresenceTimeoutValue}
              onChange={(event) => setPendingAgentPresenceTimeout(event.target.value)}
              error={!agentPresenceTimeoutValid}
              helperText={
                agentPresenceTimeoutValid
                  ? t.settingsAgentPresenceTimeoutNote
                  : t.settingsAgentPresenceTimeoutError
              }
              inputProps={{ min: 3, max: 3600, step: 1 }}
            />
          </Stack>
        </Panel>

        <Panel titleId="system-totp-heading" title={t.totpTitle}>
          <Stack spacing={3}>
            <SelectField
              id="system-totp-mode"
              label={t.totpModeLabel}
              value={totpMode}
              onChange={(event) => setPendingTotpMode(event.target.value)}
              helperText={t.totpModeNote}
            >
              <option value="off">{t.totpModeOff}</option>
              <option value="optional">{t.totpModeOptional}</option>
              <option value="required">{t.totpModeRequired}</option>
            </SelectField>
            <FormControlLabel
              control={
                <Checkbox
                  checked={systemAdminRequirePassword}
                  onChange={(event) => setPendingSystemAdminRequirePassword(event.target.checked)}
                />
              }
              label={t.settingsSystemAdminRequirePassword}
            />
          </Stack>
        </Panel>

        <Panel titleId="system-affinity-heading" title={t.settingsRouteAffinityTitle}>
          <SelectField
            id="system-route-affinity-mode"
            label={t.settingsRouteAffinityModeLabel}
            value={affinityMode}
            onChange={(event) => setPendingAffinityMode(event.target.value)}
            helperText={t.settingsRouteAffinityModeNote}
          >
            <option value="client_session">{t.settingsRouteAffinityModeClient}</option>
            <option value="legacy_header">{t.settingsRouteAffinityModeLegacy}</option>
          </SelectField>
        </Panel>

        <Panel titleId="system-resource-provisioning-heading" title={t.resourceGroups}>
          <Box>
            <FormControlLabel
              control={
                <Checkbox
                  checked={resourceProvisioningEnforce}
                  onChange={(e) => setPendingResourceProvisioningEnforce(e.target.checked)}
                />
              }
              label={t.settingsResourceProvisioningEnforceLabel}
            />
            <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
              {t.settingsResourceProvisioningEnforceHelp}
            </FormHelperText>
          </Box>
        </Panel>

        <Panel titleId="system-vision-heading" title={t.settingsVisionSectionTitle}>
          <SelectField
            id="system-vision-probe-mode"
            label={t.settingsVisionProbeMode}
            value={visionProbeMode}
            onChange={(event) => setPendingVisionProbeMode(event.target.value)}
          >
            <option value="accept">{t.settingsVisionProbeModeAccept}</option>
            <option value="verify">{t.settingsVisionProbeModeVerify}</option>
          </SelectField>
        </Panel>

        <Panel titleId="system-energy-heading" title={t.settingsEnergyTitle}>
          <Stack spacing={3}>
            <Field
              id="system-currency-factor"
              type="number"
              label={t.systemCurrencyFactor}
              value={currencyFactorValue}
              onChange={(event) => setPendingCurrencyFactor(event.target.value)}
              error={!currencyFactorValid}
              helperText={t.systemCurrencyFactorHelp}
              inputProps={{ min: 0, step: 'any' }}
            />
            <SelectField
              id="system-price-unit"
              label={t.priceUnitLabel}
              value={effectiveUnit}
              onChange={(event) => changePriceUnit(event.target.value as CurrencyUnit)}
            >
              {priceUnitOptions.map((u) => (
                <option key={u} value={u}>
                  {unitLabel(t, u)}
                </option>
              ))}
            </SelectField>
            <Field
              id="system-energy-price-per-kwh"
              type="number"
              label={t.settingsEnergyPricePerKwh}
              value={energyPricePerKwhValue}
              onChange={(event) => setPendingEnergyPricePerKwh(event.target.value)}
              error={!energyPricePerKwhValid}
              inputProps={{ min: 0, step: 0.0001 }}
            />
            <Field
              id="system-energy-pue"
              type="number"
              label={t.settingsEnergyPue}
              value={energyPueValue}
              onChange={(event) => setPendingEnergyPue(event.target.value)}
              error={!energyPueValid}
              inputProps={{ min: 0, step: 0.01 }}
            />
            <Field
              id="system-energy-wh-per-token"
              type="number"
              label={t.settingsEnergyWhPerToken}
              value={energyWhPerTokenValue}
              onChange={(event) => setPendingEnergyWhPerToken(event.target.value)}
              error={!energyWhPerTokenValid}
              inputProps={{ min: 0, step: 0.0001 }}
            />
          </Stack>
        </Panel>

        <Panel titleId="system-smtp-heading" title={t.smtpTitle} subtitle={t.smtpIntro}>
          <Stack spacing={3}>
            <Box>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={smtpEnabled}
                    onChange={(e) => setPendingSmtpEnabled(e.target.checked)}
                  />
                }
                label={t.smtpEnabledLabel}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>{t.smtpEnabledNote}</FormHelperText>
            </Box>
            <Field
              id="smtp-host"
              label={t.smtpHostLabel}
              value={smtpHost}
              onChange={(e) => setPendingHost(e.target.value)}
            />
            <Field
              id="smtp-port"
              type="number"
              label={t.smtpPortLabel}
              value={smtpPortStr}
              onChange={(e) => setPendingPort(e.target.value)}
              error={!smtpPortValid}
              helperText={smtpPortValid ? undefined : t.smtpPortError}
              inputProps={{ min: 1, max: 65535, step: 1 }}
            />
            <Field
              id="smtp-username"
              label={t.smtpUsernameLabel}
              value={smtpUsername}
              onChange={(e) => setPendingUsername(e.target.value)}
              autoComplete="off"
            />
            <Box>
              <Field
                id="smtp-password"
                type="password"
                label={t.smtpPasswordLabel}
                value={passwordCleared ? '' : passwordInput}
                onChange={(e) => {
                  setPasswordInput(e.target.value);
                  setPasswordCleared(false);
                }}
                autoComplete="new-password"
                placeholder={
                  settings?.smtp_password_set && !passwordCleared
                    ? t.smtpPasswordSetPlaceholder
                    : undefined
                }
                helperText={t.smtpPasswordNote}
              />
              {settings?.smtp_password_set && !passwordCleared && (
                <Button
                  type="button"
                  size="small"
                  variant="text"
                  color="secondary"
                  onClick={() => {
                    setPasswordCleared(true);
                    setPasswordInput('');
                  }}
                >
                  {t.smtpPasswordClear}
                </Button>
              )}
            </Box>
            <Field
              id="smtp-from"
              label={t.smtpFromLabel}
              value={smtpFrom}
              onChange={(e) => setPendingFrom(e.target.value)}
            />
            <Field
              id="smtp-from-name"
              label={t.smtpFromNameLabel}
              value={smtpFromName}
              onChange={(e) => setPendingFromName(e.target.value)}
            />
            <SelectField
              id="smtp-tls"
              label={t.smtpTlsModeLabel}
              value={smtpTls}
              onChange={(e) => setPendingTls(e.target.value)}
            >
              <option value="starttls">{t.smtpTlsStartTls}</option>
              <option value="ssl">{t.smtpTlsSsl}</option>
              <option value="none">{t.smtpTlsNone}</option>
            </SelectField>
            <Box sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-start' }}>
              <Field
                id="smtp-test-to"
                label={t.smtpTestToLabel}
                value={testTo}
                onChange={(e) => setTestTo(e.target.value)}
              />
              <Button
                type="button"
                variant="outlined"
                disabled={testing || busy}
                onClick={sendTest}
              >
                {t.smtpTestButton}
              </Button>
            </Box>
          </Stack>
        </Panel>

        <Panel
          titleId="system-netbird-heading"
          title={t.settingsNetbirdTitle}
          subtitle={t.settingsNetbirdIntro}
        >
          <FormControlLabel
            control={
              <Checkbox
                checked={netbirdEnabled}
                onChange={(e) => setPendingNetbirdEnabled(e.target.checked)}
              />
            }
            label={t.settingsNetbirdEnable}
          />
        </Panel>

        {/* TLS-certificate module: ONLY the enable checkbox lives here (mirrors the
            NetBird panel above); issuer mode, ACME/self-signed fields, the internal-CA
            panel, and the certificate list all live in the separate CertificateSettings
            view, reachable once this checkbox is on. */}
        <Panel
          titleId="system-certificates-heading"
          title={t.settingsCertificatesTitle}
          subtitle={t.settingsCertificatesIntro}
        >
          <FormControlLabel
            control={
              <Checkbox
                checked={certEnabled}
                onChange={(e) => setPendingCertEnabled(e.target.checked)}
              />
            }
            label={t.settingsCertEnabled}
          />
        </Panel>

        <Box
          sx={{
            display: 'flex',
            justifyContent: 'flex-end',
            borderTop: 1,
            borderColor: 'divider',
            pt: 2.5,
          }}
        >
          <Button
            type="button"
            variant="contained"
            disabled={
              busy ||
              loading ||
              !theme ||
              !language ||
              !retentionValid ||
              !intervalValid ||
              !agentPresenceTimeoutValid ||
              !currencyFactorValid ||
              !energyPricePerKwhValid ||
              !energyPueValid ||
              !energyWhPerTokenValid ||
              !smtpConfigOk
            }
            onClick={save}
          >
            {t.save}
          </Button>
        </Box>
      </Stack>
    </>
  );
}
