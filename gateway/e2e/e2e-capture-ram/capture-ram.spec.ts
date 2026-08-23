// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";
import { CAPTURE_RAM_ADMIN_EMAIL, CAPTURE_RAM_ADMIN_PASSWORD } from "../playwright.capture-ram.config";

const t = messages.de;
const PROBE = "e2e ram capture probe";

// The gateway caches the capture_enabled system setting for ~5s (captureEnabledCacheTTL)
// so it is not read from the DB on every proxied request. Waits after a toggle must
// exceed that window before the new value is guaranteed to be in effect.
const CAPTURE_ENABLED_TTL_MS = 6500;

async function fireInference(request: import("@playwright/test").APIRequestContext, secret: string, content: string) {
  const resp = await request.post("http://127.0.0.1:8091/v1/chat/completions", {
    headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
    data: { model: "qwen-coder", messages: [{ role: "user", content }] }
  });
  expect(resp.status()).toBe(200);
}

// End-to-end proof of the SP-C+ RAM-fallback capture path against a real gateway
// running in sqlite mode WITHOUT an encryption key — the path that used to
// disable capture entirely and now falls back to the volatile in-RAM
// MemoryCaptureStore (KeyVersion 0, plain gzip). Covers three SP-C+ headline
// behaviors live: (1) a RAM-captured request is viewable with the bearer secret
// redacted, (2) the substring/case-insensitive model+server filter works, and
// (3) a capture can be manually deleted while its usage-event metadata stays.
test("RAM-fallback capture is viewable + filterable + deletable", async ({ page, request }) => {
  // a. Log in as the bootstrapped, login-able system_admin.
  await login(page, CAPTURE_RAM_ADMIN_EMAIL, CAPTURE_RAM_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // b. Create an API token with "Kommunikation speichern" (log_communication) ON.
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click(); // open the create sub-view
  await page.locator("#token-name").fill("capture-ram-e2e-token");
  await page.getByRole("checkbox", { name: t.tokenLogCommunicationLabel }).check();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  // c. Drive one inference request with that bearer token (the isolated `request`
  // context carries no session cookie, so the token authenticates). With no
  // encryption key wired, log_communication now captures into RAM instead of
  // being silently disabled.
  const resp = await request.post("http://127.0.0.1:8091/v1/chat/completions", {
    headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
    data: { model: "qwen-coder", messages: [{ role: "user", content: PROBE }] }
  });
  expect(resp.status()).toBe(200);

  // d. Open Activity and wait for the has_capture-gated View button. This proves
  // the RAM capture landed AND that Service.Usage enrichment surfaces has_capture
  // for the MemoryCaptureStore (not just the SQL EXISTS path).
  const viewButton = page.getByRole("button", { name: t.activityColView }).first();
  await expect(async () => {
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(viewButton).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: 30000 });

  // e. The capture dialog shows the request body + model from the RAM store
  // (gunzip-only, KeyVersion 0), and the Authorization header is redacted.
  await viewButton.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText(t.captureDialogTitle)).toBeVisible();
  await expect(dialog).toContainText(PROBE);
  await expect(dialog).toContainText("qwen-coder");
  await expect(dialog).toContainText("[redacted]");
  await expect(dialog).not.toContainText(secret);
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();

  // f. Filter fix (SP-C+ Phase 1): substring + case-insensitive, over model and
  // over the server name — now driven via the per-column header filter popovers.
  // Scope to the usage-table region: the running-connections table above has an
  // identically-named Modell filter + Spalten control.
  const usage = page.getByRole("region", { name: t.usageTableTitle });
  const modelFilter = `${t.listFilter}: ${t.tableModel}`;
  const setModelFilter = async (value: string) => {
    await usage.getByRole("button", { name: modelFilter }).click();
    await page.getByRole("textbox", { name: modelFilter }).fill(value);
    await page.keyboard.press("Escape");
  };

  // Lowercase substring of the model still matches (server-side substring filter).
  await setModelFilter("qwen");
  await expect(async () => { await expect(viewButton).toBeVisible({ timeout: 2000 }); }).toPass({ timeout: 10000 });

  // A non-matching substring hides the row.
  await setModelFilter("zzz-no-such-model");
  await expect(async () => {
    await expect(page.getByRole("button", { name: t.activityColView })).toHaveCount(0, { timeout: 2000 });
  }).toPass({ timeout: 10000 });
  await setModelFilter("");

  // Server name is a default-hidden column now: reveal it via the usage table's
  // columns menu, then filter it (server_name OR host, case-insensitive substring).
  await usage.getByRole("button", { name: t.listColumns }).click();
  await page.getByRole("checkbox", { name: t.tableHost }).check();
  await page.keyboard.press("Escape");
  const serverFilter = `${t.listFilter}: ${t.tableHost}`;
  const setServerFilter = async (value: string) => {
    await usage.getByRole("button", { name: serverFilter }).click();
    await page.getByRole("textbox", { name: serverFilter }).fill(value);
    await page.keyboard.press("Escape");
  };
  await setServerFilter("mock");
  await expect(async () => { await expect(viewButton).toBeVisible({ timeout: 2000 }); }).toPass({ timeout: 10000 });
  await setServerFilter("");
  await expect(async () => { await expect(viewButton).toBeVisible({ timeout: 2000 }); }).toPass({ timeout: 10000 });

  // g. Manual delete (SP-C+ Phase 5): open the capture, delete it, confirm. The
  // capture blob is removed from RAM; the usage-event row stays but its View
  // button disappears (has_capture flips false after the silent refetch).
  await viewButton.click();
  await expect(dialog.getByText(t.captureDialogTitle)).toBeVisible();
  await dialog.getByRole("button", { name: t.captureDelete }).click();

  // The modal ConfirmDialog opens; the backgrounded capture dialog's Delete button
  // is aria-hidden, so this uniquely targets the confirm action.
  const confirm = page.getByRole("dialog").filter({ hasText: t.captureDeleteConfirm });
  await confirm.getByRole("button", { name: t.captureDelete }).click();

  // After delete: dialog closed and no View button remains (row kept, capture gone).
  await expect(async () => {
    await expect(page.getByRole("button", { name: t.activityColView })).toHaveCount(0, { timeout: 2000 });
  }).toPass({ timeout: 15000 });
});

// SP-C+ Phase 2: the global capture_enabled kill switch, live. Toggling it OFF in
// System Settings must stop NEW captures system-wide (the per-token opt-in is no
// longer sufficient); toggling it back ON resumes capturing. Uses relative View-
// button counts so it is independent of any captures left by the first test.
test("capture_enabled kill switch suppresses new captures, re-enabling resumes", async ({ page, request }) => {
  await login(page, CAPTURE_RAM_ADMIN_EMAIL, CAPTURE_RAM_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // Own log_communication token for this test.
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click(); // open the create sub-view
  await page.locator("#token-name").fill("capture-ram-toggle-token");
  await page.getByRole("checkbox", { name: t.tokenLogCommunicationLabel }).check();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  const viewButtons = page.getByRole("button", { name: t.activityColView });
  const gotoActivity = async () => {
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(page.getByRole("link", { name: t.usage })).toBeVisible();
  };

  // Baseline: how many captured rows exist right now (before any toggling).
  await gotoActivity();
  await page.waitForTimeout(800);
  const baseline = await viewButtons.count();

  // a. Turn capture_enabled OFF (system_admin only) and save.
  await page.getByRole("link", { name: t.system }).click();
  const toggle = page.getByRole("checkbox", { name: t.captureEnabledLabel });
  await expect(toggle).toBeChecked(); // default true
  await toggle.uncheck();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();
  await page.waitForTimeout(CAPTURE_ENABLED_TTL_MS); // let the gateway's TTL cache expire

  // b. Fire an inference request with the log_communication token. It must succeed
  // but NOT produce a capture — the kill switch overrides the per-token opt-in.
  await fireInference(request, secret, "capture disabled probe");
  await page.waitForTimeout(3000); // fire-and-forget capture would have landed by now
  await gotoActivity();
  expect(await viewButtons.count()).toBe(baseline); // unchanged -> nothing captured

  // c. Turn capture_enabled back ON and save.
  await page.getByRole("link", { name: t.system }).click();
  const toggleBack = page.getByRole("checkbox", { name: t.captureEnabledLabel });
  await expect(toggleBack).not.toBeChecked();
  await toggleBack.check();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();
  await page.waitForTimeout(CAPTURE_ENABLED_TTL_MS);

  // d. Fire again — capturing resumes, so exactly one new View button appears.
  await fireInference(request, secret, "capture re-enabled probe");
  await expect(async () => {
    await gotoActivity();
    expect(await viewButtons.count()).toBe(baseline + 1);
  }).toPass({ timeout: 20000 });
});
