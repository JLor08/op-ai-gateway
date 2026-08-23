// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type SubmitEvent, type ReactNode } from 'react';
import { Box, Button, Stack, Typography } from '@mui/material';
import type { Translation, PortalApi } from './shared/types';
import type { Locale } from '../i18n';
import { formatPortalError } from './shared/format';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { useToast } from './shared/ToastProvider';
import { useResource } from './shared/useResource';
import { SecretReveal } from './shared/SecretReveal';
import type { TotpEnrollment } from '../api';

export function ManagementView({
  t,
  api,
  locale,
  onSelectLocale,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'changePassword' | 'me' | 'totpConfirm' | 'totpDisable' | 'totpEnroll'>;
  locale: Locale;
  onSelectLocale: (l: Locale) => void;
}>) {
  const { showSuccess, showError } = useToast();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);

  const { data: me, setData: setMe } = useResource(() => api.me(), [api], t);
  const totpMode = me?.totp_mode ?? 'off';
  const totpEnabled = me?.totp_enabled ?? false;
  const [enrollment, setEnrollment] = useState<TotpEnrollment | null>(null);
  const [totpCode, setTotpCode] = useState('');
  const [totpBusy, setTotpBusy] = useState(false);

  async function startEnroll() {
    setTotpBusy(true);
    try {
      setEnrollment(await api.totpEnroll());
      setTotpCode('');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setTotpBusy(false);
    }
  }
  async function confirmEnroll() {
    setTotpBusy(true);
    try {
      setMe(await api.totpConfirm(totpCode));
      setEnrollment(null);
      setTotpCode('');
      showSuccess(t.totpConfirmSuccess);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setTotpBusy(false);
    }
  }
  async function disableTotp() {
    setTotpBusy(true);
    try {
      await api.totpDisable(totpCode);
      setMe(await api.me());
      setTotpCode('');
      showSuccess(t.totpDisableSuccess);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setTotpBusy(false);
    }
  }

  async function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (next !== confirm) {
      showError(t.passwordMismatch);
      return;
    }
    setBusy(true);
    try {
      await api.changePassword(current, next);
      showSuccess(t.changePasswordSuccess);
      setCurrent('');
      setNext('');
      setConfirm('');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  let totpContent: ReactNode = null;
  if (enrollment) {
    totpContent = (
      <>
        <Typography color="text.secondary">{t.totpEnrollScanHint}</Typography>
        <Box
          component="img"
          src={enrollment.qr_png_data_uri}
          alt={t.totpQrAlt}
          sx={{ width: 200, height: 200 }}
        />
        <SecretReveal
          title={t.totpSecretLabel}
          copyValue={enrollment.secret_base32}
          copyLabel={t.totpCopySecret}
        >
          <code>{enrollment.secret_base32}</code>
        </SecretReveal>
        <Field
          id="totp-code"
          label={t.totpCodeLabel}
          value={totpCode}
          onChange={(e) => setTotpCode(e.target.value)}
          inputProps={{ inputMode: 'numeric', autoComplete: 'one-time-code' }}
        />
        <Box sx={{ display: 'flex', gap: 1.5 }}>
          <Button
            variant="contained"
            disabled={totpBusy || totpCode === ''}
            onClick={() => void confirmEnroll()}
          >
            {t.totpConfirmButton}
          </Button>
          <Button
            variant="text"
            color="secondary"
            disabled={totpBusy}
            onClick={() => {
              setEnrollment(null);
              setTotpCode('');
            }}
          >
            {t.cancel}
          </Button>
        </Box>
      </>
    );
  } else if (totpEnabled) {
    totpContent = (
      <>
        <Typography>{t.totpStatusEnabled}</Typography>
        {(totpMode === 'optional' || totpMode === 'off') && (
          <>
            <Typography color="text.secondary">{t.totpDisableHint}</Typography>
            <Field
              id="totp-code"
              label={t.totpCodeLabel}
              value={totpCode}
              onChange={(e) => setTotpCode(e.target.value)}
              inputProps={{ inputMode: 'numeric', autoComplete: 'one-time-code' }}
            />
            <Button
              variant="outlined"
              color="error"
              disabled={totpBusy || totpCode === ''}
              onClick={() => void disableTotp()}
            >
              {t.totpDisableButton}
            </Button>
          </>
        )}
      </>
    );
  } else if (totpMode !== 'off') {
    totpContent = (
      <>
        <Typography>{t.totpStatusDisabled}</Typography>
        <Button
          variant="contained"
          disabled={totpBusy}
          onClick={() => void startEnroll()}
          sx={{ justifySelf: 'start' }}
        >
          {t.totpEnrollButton}
        </Button>
      </>
    );
  }

  return (
    <>
      <PageTitle title={t.profile} subtitle={t.changePassword} />
      <Panel titleId="change-password-heading" title={t.changePassword}>
        <Box component="form" onSubmit={submit}>
          <Stack spacing={2}>
            <Field
              id="cp-current"
              label={t.currentPasswordLabel}
              type="password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              autoComplete="current-password"
              required
            />
            <Field
              id="cp-new"
              label={t.newPasswordLabel}
              type="password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoComplete="new-password"
              required
            />
            <Field
              id="cp-confirm"
              label={t.confirmPasswordLabel}
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              required
            />
            <Button type="submit" variant="contained" disabled={busy}>
              {t.changePasswordButton}
            </Button>
          </Stack>
        </Box>
      </Panel>
      {(totpMode !== 'off' || totpEnabled) && (
        <Panel titleId="totp-heading" title={t.totpTitle} subtitle={t.totpIntro}>
          <Stack spacing={2}>{totpContent}</Stack>
        </Panel>
      )}
      <Panel titleId="profile-language-heading" title={t.profileLanguageTitle}>
        <SelectField
          id="profile-language"
          label={t.profileLanguageLabel}
          value={locale}
          onChange={(event) => onSelectLocale(event.target.value === 'en' ? 'en' : 'de')}
        >
          <option value="de">Deutsch</option>
          <option value="en">English</option>
        </SelectField>
      </Panel>
    </>
  );
}
