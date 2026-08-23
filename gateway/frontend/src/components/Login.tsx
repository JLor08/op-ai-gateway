// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type SubmitEvent, type ReactNode } from 'react';
import { Alert, Box, Button, Paper, Stack, Typography } from '@mui/material';
import type { Locale } from '../i18n';
import type { CurrentUser, TotpEnrollment } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { Field } from './shared/Field';
import { LanguageMenu } from './shared/LanguageMenu';
import { SecretReveal } from './shared/SecretReveal';
import { Brand } from '../theme/Brand';
import { useThemeControls } from '../theme/useThemeControls';

export function Login({
  t,
  api,
  locale,
  onSelectLocale,
  onSuccess,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'login'>;
  locale: Locale;
  onSelectLocale: (l: Locale) => void;
  onSuccess: (user: CurrentUser) => void;
}>) {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [code, setCode] = useState('');
  const [totpRequired, setTotpRequired] = useState(false);
  const [enrollment, setEnrollment] = useState<TotpEnrollment | null>(null);
  const { brand, productName } = useThemeControls();

  async function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const res = await api.login(email, password, totpRequired || enrollment ? code : undefined);
      if ('totp_required' in res) {
        setTotpRequired(true);
        return;
      }
      if ('totp_enrollment_required' in res) {
        setEnrollment(res);
        return;
      }
      onSuccess(res);
    } catch (err) {
      setError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  let content: ReactNode;
  if (enrollment) {
    content = (
      <Stack spacing={1.75}>
        <Typography variant="h6">{t.loginTotpEnrollTitle}</Typography>
        <Typography color="text.secondary">{t.totpEnrollScanHint}</Typography>
        <Box
          component="img"
          src={enrollment.qr_png_data_uri}
          alt={t.totpQrAlt}
          sx={{ width: 200, height: 200, alignSelf: 'center' }}
        />
        <SecretReveal
          title={t.totpSecretLabel}
          copyValue={enrollment.secret_base32}
          copyLabel={t.totpCopySecret}
        >
          <code>{enrollment.secret_base32}</code>
        </SecretReveal>
        <Field
          id="login-totp"
          label={t.totpCodeLabel}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          inputProps={{ inputMode: 'numeric', autoComplete: 'one-time-code' }}
          required
        />
        <Button type="submit" variant="contained" disabled={busy}>
          {t.loginVerifyButton}
        </Button>
      </Stack>
    );
  } else if (totpRequired) {
    content = (
      <Stack spacing={1.75}>
        <Typography variant="h6">{t.loginTotpTitle}</Typography>
        <Typography color="text.secondary">{t.loginTotpIntro}</Typography>
        <Field
          id="login-totp"
          label={t.totpCodeLabel}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          inputProps={{ inputMode: 'numeric', autoComplete: 'one-time-code' }}
          required
        />
        <Button type="submit" variant="contained" disabled={busy}>
          {t.loginVerifyButton}
        </Button>
      </Stack>
    );
  } else {
    content = (
      <>
        <Field
          id="login-email"
          label={t.emailLabel}
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          autoComplete="username"
          required
        />
        <Field
          id="login-password"
          label={t.passwordLabel}
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
          required
        />
        <Button type="submit" variant="contained" disabled={busy}>
          {t.loginButton}
        </Button>
      </>
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
        component="form"
        variant="outlined"
        onSubmit={submit}
        aria-labelledby="login-heading"
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
        <Typography id="login-heading" variant="h5" component="h1">
          {t.signIn}
        </Typography>
        <Typography color="text.secondary">{t.signInIntro}</Typography>
        {error && <Alert severity="error">{error}</Alert>}
        {content}
      </Paper>
    </Box>
  );
}
