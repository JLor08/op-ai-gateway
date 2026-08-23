// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";
import { SMTP_HTTP_BASE, SMTP_HOST, SMTP_PORT } from "../playwright.smtp.config";

const t = messages.de;

type Caught = { from: string; to: string[]; data: string };

async function caught(page: Page): Promise<Caught[]> {
  const res = await page.request.get(`${SMTP_HTTP_BASE}/messages`);
  expect(res.ok()).toBeTruthy();
  return (await res.json()) as Caught[];
}

async function resetCatcher(page: Page): Promise<void> {
  const res = await page.request.delete(`${SMTP_HTTP_BASE}/messages`);
  expect(res.ok()).toBeTruthy();
}

// Fill + enable SMTP in System Settings, pointing at the local plaintext catcher.
async function enableSmtp(page: Page): Promise<void> {
  await page.getByRole("link", { name: t.system }).click();
  await page.locator("#smtp-host").fill(SMTP_HOST);
  await page.locator("#smtp-port").fill(SMTP_PORT);
  await page.locator("#smtp-from").fill("gateway@example.test");
  // TLS mode is a MUI Select (SelectField): open it, pick "none" (plain, no auth).
  await page.getByRole("combobox", { name: t.smtpTlsModeLabel }).click();
  await page.getByRole("option", { name: t.smtpTlsNone }).click();
  const toggle = page.getByLabel(t.smtpEnabledLabel);
  if (!(await toggle.isChecked())) await toggle.check();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();
}

async function createUser(page: Page, email: string): Promise<string> {
  await page.getByRole("link", { name: t.users }).click();
  await page.getByRole("button", { name: t.userCreate }).click();
  await page.locator("#user-email").fill(email);
  await page.locator("#user-name").fill("SMTP E2E User");
  await page.getByRole("button", { name: t.userCreate }).click();
  // The invite dialog always shows the link (fallback), regardless of send outcome.
  const link = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  await page.getByRole("button", { name: t.captureClose }).click();
  return link;
}

test("SMTP on: Testmail + user-create both deliver via the catcher", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await resetCatcher(page);
  await enableSmtp(page);

  // 'Testmail senden' → default recipient is the caller (dev@example.test).
  await page.getByRole("button", { name: t.smtpTestButton }).click();
  await expect
    .poll(async () => (await caught(page)).length, { timeout: 10000 })
    .toBeGreaterThan(0);
  await resetCatcher(page);

  // Creating a user emails the set-password link, and still shows it as fallback.
  const email = `smtp-${Date.now()}@example.test`;
  const link = await createUser(page, email);
  expect(link).toContain("/set-password?token=");

  await expect
    .poll(
      async () => {
        const msgs = await caught(page);
        return msgs.some((m) => m.to.includes(email) && m.data.includes("/set-password?token="));
      },
      { timeout: 10000 }
    )
    .toBe(true);
});

test("SMTP off: user-create sends no email but still shows the link", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  // Turn SMTP off explicitly (independent of test order).
  await page.getByRole("link", { name: t.system }).click();
  const toggle = page.getByLabel(t.smtpEnabledLabel);
  if (await toggle.isChecked()) await toggle.uncheck();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();
  await resetCatcher(page);

  const email = `nosmtp-${Date.now()}@example.test`;
  const link = await createUser(page, email);
  expect(link).toContain("/set-password?token=");

  // Give any (erroneous) send a moment, then assert nothing was caught for it.
  await page.waitForTimeout(1500);
  const msgs = await caught(page);
  expect(msgs.some((m) => m.to.includes(email))).toBe(false);
});
