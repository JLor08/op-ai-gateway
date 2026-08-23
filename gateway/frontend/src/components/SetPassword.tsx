// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type SubmitEvent, type ReactNode } from 'react';
import { Alert, Box, Button, Paper, Typography } from '@mui/material';
import type { Locale } from '../i18n';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { Field } from './shared/Field';
import { LanguageMenu } from './shared/LanguageMenu';
import { SecretReveal } from './shared/SecretReveal';
import { Brand } from '../theme/Brand';
import { useThemeControls } from '../theme/useThemeControls';
import type { SetPasswordResponse } from '../api';

export function SetPassword({
  t,
  api,
  token,
  locale,
  onSelectLocale,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'login' | 'setPassword'>;
  token: string;
  locale: Locale;
  onSelectLocale: (l: Locale) => void;
}>) {
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState('');
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [enrollment, setEnrollment] = useState<Extract<
    SetPasswordResponse,
    { totp_enrollment_required: true }
  > | null>(null);
  const [code, setCode] = useState('');
  const { brand, productName } = useThemeControls();

  async function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (password !== confirm) {
      setError(t.passwordMismatch);
      return;
    }
    setBusy(true);
    setError('');
    try {
      const res = await api.setPassword(token, password);
      if ('totp_enrollment_required' in res) {
        setEnrollment(res);
        return;
      }
      setDone(true);
    } catch (err) {
      setError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function confirmEnroll(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!enrollment) return;
    setBusy(true);
    setError('');
    try {
      await api.login(enrollment.email, password, code);
      setDone(true);
    } catch (err) {
      setError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  let content: ReactNode;
  if (enrollment && !done) {
    content = (
      <Box component="form" onSubmit={confirmEnroll} sx={{ display: 'grid', gap: 1.75 }}>
        <Typography variant="h6">{t.loginTotpEnrollTitle}</Typography>
        <Typography color="text.secondary">{t.totpEnrollScanHint}</Typography>
        {error && <Alert severity="error">{error}</Alert>}
        <Box
          component="img"
          src={enrollment.qr_png_data_uri}
          alt={t.totpQrAlt}
          sx={{ width: 200, height: 200, justifySelf: 'center' }}
        />
        <SecretReveal
          title={t.totpSecretLabel}
          copyValue={enrollment.secret_base32}
          copyLabel={t.totpCopySecret}
        >
          <code>{enrollment.secret_base32}</code>
        </SecretReveal>
        <Field
          id="sp-totp"
          label={t.totpCodeLabel}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          inputProps={{ inputMode: 'numeric', autoComplete: 'one-time-code' }}
          required
        />
        <Button type="submit" variant="contained" disabled={busy}>
          {t.loginVerifyButton}
        </Button>
      </Box>
    );
  } else if (done) {
    content = (
      <Alert
        severity="success"
        action={
          <Button size="small" href={import.meta.env.BASE_URL}>
            {t.signIn}
          </Button>
        }
      >
        {t.setPasswordSuccess}
      </Alert>
    );
  } else {
    content = (
      <Box component="form" onSubmit={submit} sx={{ display: 'grid', gap: 1.75 }}>
        {error && <Alert severity="error">{error}</Alert>}
        <Field
          id="sp-password"
          label={t.newPasswordLabel}
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="new-password"
          required
        />
        <Field
          id="sp-confirm"
          label={t.confirmPasswordLabel}
          type="password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          autoComplete="new-password"
          required
        />
        <Button type="submit" variant="contained" disabled={busy}>
          {t.setPasswordButton}
        </Button>
      </Box>
    );
  }

  return (
    <Box
      sx={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        p: 3,
        position: 'relative',
        zIndex: 1,
      }}
    >
      <Paper
        variant="outlined"
        sx={{ display: 'grid', gap: 1.75, p: 4, width: 'min(420px, 100%)' }}
      >
        <Box
          sx={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            flexWrap: 'wrap',
            rowGap: 1,
            gap: 1.25,
          }}
        >
          <Brand brand={brand} label={productName} />
          <LanguageMenu locale={locale} onSelect={onSelectLocale} t={t} />
        </Box>
        <Typography variant="h5" component="h1">
          {t.setPasswordTitle}
        </Typography>
        <Typography color="text.secondary">{t.setPasswordIntro}</Typography>
        {content}
      </Paper>
    </Box>
  );
}
