// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";

const t = messages.de;

// Set totp_mode in System Settings (dev admin session required).
export async function setTotpMode(page: Page, mode: "off" | "optional" | "required"): Promise<void> {
  await page.getByRole("link", { name: t.system }).click();
  const label =
    mode === "optional" ? t.totpModeOptional : mode === "required" ? t.totpModeRequired : t.totpModeOff;
  await page.getByRole("combobox", { name: t.totpModeLabel }).click();
  await page.getByRole("option", { name: label, exact: true }).click();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();
}

// Open the profile view via the user menu (display name button -> "Profil" menu item).
export async function goToProfile(page: Page, displayName: string): Promise<void> {
  await page.getByRole("button", { name: displayName }).click();
  await page.getByRole("menuitem", { name: t.profile }).click();
}

// Click "Einrichten" in the profile TOTP panel and return the freshly generated
// pending secret read straight off the enroll response.
export async function enrollFromProfile(page: Page): Promise<string> {
  // Register the response listener BEFORE the click that triggers the POST,
  // via Promise.all, so the response can't arrive before we're listening
  // (a real race against the local in-memory gateway otherwise).
  const [res] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes("/api/portal/totp/enroll") && r.request().method() === "POST"
    ),
    page.getByRole("button", { name: t.totpEnrollButton }).click(),
  ]);
  expect(res.ok()).toBeTruthy();
  const body = (await res.json()) as { secret_base32: string };
  expect(body.secret_base32).toMatch(/^[A-Z2-7]+$/);
  return body.secret_base32;
}
