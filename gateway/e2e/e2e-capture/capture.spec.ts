// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, inviteUser, setPassword } from "../e2e/helpers";
import { CAPTURE_ADMIN_EMAIL, CAPTURE_ADMIN_PASSWORD } from "../playwright.capture.config";

const GW = "http://127.0.0.1:8091";

const t = messages.de;
const PROBE = "e2e capture probe";

// End-to-end proof of the SP-C payload capture path against a real sqlite-mode
// gateway (capture is SQLite-only). It logs in as the Part-A bootstrap admin,
// creates a log_communication token, drives one inference request through the
// gateway with that token, then opens the Activity capture dialog and asserts the
// captured body is visible while the Authorization secret is redacted.
test("captured request is viewable with the bearer secret redacted", async ({ page, request }) => {
  // a. Log in as the bootstrapped, login-able system_admin (Part A).
  await login(page, CAPTURE_ADMIN_EMAIL, CAPTURE_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // b. Create an API token with "Kommunikation speichern" (log_communication) ON.
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click(); // open the create sub-view
  await page.locator("#token-name").fill("capture-e2e-token");
  // exact: the ChatSession row adds a checkbox whose name CONTAINS this label.
  await page.getByRole("checkbox", { name: t.tokenLogCommunicationLabel, exact: true }).check();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  // c. Drive one inference request with that bearer token. The isolated `request`
  // context carries no session cookie, so the token (not the portal session)
  // authenticates and its log_communication flag triggers a capture.
  const resp = await request.post("http://127.0.0.1:8091/v1/chat/completions", {
    headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
    data: { model: "qwen-coder", messages: [{ role: "user", content: PROBE }] }
  });
  expect(resp.status()).toBe(200);

  // d. Open the Activity view and find the captured row's View (Ansicht) action.
  // The capture is persisted fire-and-forget, so reload + re-navigate until the
  // has_capture-gated View button appears.
  const viewButton = page.getByRole("button", { name: t.activityColView }).first();
  await expect(async () => {
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(viewButton).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: 30000 });
  await viewButton.click();

  // e. The capture dialog shows the request body + model, and the sensitive
  // Authorization header is redacted — the bearer secret must appear nowhere.
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText(t.captureDialogTitle)).toBeVisible();
  await expect(dialog).toContainText(PROBE);
  await expect(dialog).toContainText("qwen-coder");
  await expect(dialog).toContainText("[redacted]");
  await expect(dialog).not.toContainText(secret);
});

const OVERRIDE_PROBE = "e2e override probe zzq";

// Proves Feature 1 (capture_override): with the system setting ON, a request
// made with a token that has log_communication OFF is still captured. Enabling
// the setting flows System Settings -> system_settings store -> the cached
// CaptureOverride hook -> the capture gate. The default sort is created_at desc,
// so the override request is the newest row; its capture dialog must contain the
// override probe (a plain log_communication token would not have captured it).
test("capture_override forces capture for a token without log_communication", async ({ page, request }) => {
  await login(page, CAPTURE_ADMIN_EMAIL, CAPTURE_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // a. Turn on "Erfassung erzwingen" (capture_override) in System Settings.
  await page.getByRole("link", { name: t.system }).click();
  const overrideCheckbox = page.getByRole("checkbox", { name: t.captureOverrideLabel });
  await expect(overrideCheckbox).toBeVisible();
  await overrideCheckbox.check();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByText(t.systemSaved)).toBeVisible();

  // b. Create a token with log_communication OFF (do not check the box).
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click(); // open the create sub-view
  await page.locator("#token-name").fill("override-e2e-token");
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  // c. Drive a request with that non-logging token and confirm it was captured.
  // Re-send each iteration: the CaptureOverride hook has a ~5s TTL cache, so the
  // first request may predate the refreshed value, and capture is fire-and-forget.
  const viewButton = page.getByRole("button", { name: t.activityColView }).first();
  const dialog = page.getByRole("dialog");
  await expect(async () => {
    const resp = await request.post("http://127.0.0.1:8091/v1/chat/completions", {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: "qwen-coder", messages: [{ role: "user", content: OVERRIDE_PROBE }] }
    });
    expect(resp.status()).toBe(200);
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(viewButton).toBeVisible({ timeout: 3000 });
    await viewButton.click();
    await expect(dialog.getByText(t.captureDialogTitle)).toBeVisible({ timeout: 3000 });
    await expect(dialog).toContainText(OVERRIDE_PROBE, { timeout: 3000 });
  }).toPass({ timeout: 45000 });
});

const SECRET_PROBE = "e2e secret probe yyx";

// Proves Feature 2 (Geheim/secret): a capture from a token with the "Geheim"
// flag is (a) visible to and toggleable by its OWNER, but (b) hidden from a
// DIFFERENT admin — the list marks it capture_locked (a non-clickable lock, no
// View) and the detail endpoint 404s — and (c) once the owner un-secrets it, the
// same admin can see it. Owner behavior is checked in the UI; the foreign-admin
// gate is checked at the API layer (decisive, no fragile scope-toggle nav).
test("secret capture: owner sees + toggles; a foreign admin is locked out until un-secreted", async ({ page, request, browser }) => {
  await login(page, CAPTURE_ADMIN_EMAIL, CAPTURE_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // a. Create a token with "Geheim" (secret) ON and drive one request with it.
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click(); // open the create sub-view
  await page.locator("#token-name").fill("geheim-token");
  // exact: the ChatSession row adds a "Geheim …" checkbox whose name CONTAINS this label.
  await page.getByRole("checkbox", { name: t.tokenSecretLabel, exact: true }).check();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  // b. Owner UI: the secret capture is viewable by its owner and offers the
  // "Sichtbar machen" toggle (can_toggle_secret). Retry: the isolated `request`
  // context carries no session cookie, so the bearer token (with its secret flag)
  // authenticates; capture is fire-and-forget.
  const viewButton = page.getByRole("button", { name: t.activityColView }).first();
  const dialog = page.getByRole("dialog");
  await expect(async () => {
    const resp = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: "qwen-coder", messages: [{ role: "user", content: SECRET_PROBE }] }
    });
    expect(resp.status()).toBe(200);
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(viewButton).toBeVisible({ timeout: 3000 });
    await viewButton.click();
    await expect(dialog).toContainText(SECRET_PROBE, { timeout: 3000 });
  }).toPass({ timeout: 45000 });
  await expect(dialog.getByRole("button", { name: t.captureMarkVisible })).toBeVisible();

  // c. Resolve the secret capture's usage-event id via the owner's usage API.
  const own = await page.request.get(`${GW}/api/portal/usage?scope=own&limit=100`, { headers: { "X-OP-CSRF": "1" } }).then((r) => r.json());
  const secretRow = own.data.find((row: { token_name: string }) => row.token_name === "geheim-token");
  expect(secretRow, "owner sees the geheim-token row").toBeTruthy();
  expect(secretRow.has_capture).toBe(true); // owner can open own secret capture
  const secretId: string = secretRow.id;
  await page.reload(); // close the dialog before navigating as the owner again

  // d. Invite a SECOND admin (B) and redeem the invite (auto-login) in a fresh
  // browser context, so B carries its own session — not the owner's.
  const { inviteUrl } = await inviteUser(page, { role: "admin" });
  const ctxB = await browser.newContext();
  const pageB = await ctxB.newPage();
  await setPassword(pageB, inviteUrl, "second-admin-pass-1");
  await pageB.goto("/portal/");
  await expect(pageB.getByText(t.welcome)).toBeVisible();

  // e. B is admin but NOT the owner: the secret row is capture_locked (not
  // has_capture) in scope=all, and the detail endpoint 404s.
  const asB = () => pageB.request.get(`${GW}/api/portal/usage?scope=all&limit=100`, { headers: { "X-OP-CSRF": "1" } }).then((r) => r.json());
  const allRows = await asB();
  const rowForB = allRows.data.find((row: { id: string }) => row.id === secretId);
  expect(rowForB, "admin B sees the row under scope=all").toBeTruthy();
  expect(rowForB.capture_locked).toBe(true);
  expect(rowForB.has_capture).toBeFalsy();
  const lockedDetail = await pageB.request.get(`${GW}/api/portal/usage/captures/${secretId}`, { headers: { "X-OP-CSRF": "1" } });
  expect(lockedDetail.status()).toBe(404);

  // f. Owner un-secrets THIS capture via the dialog toggle ("Sichtbar machen").
  await page.getByRole("link", { name: t.usage }).click();
  await viewButton.click();
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: t.captureMarkVisible }).click();

  // g. The same admin B can now see it: capture_locked flips off, has_capture on,
  // and the detail endpoint returns 200.
  await expect(async () => {
    const rows = await asB();
    const row = rows.data.find((r: { id: string }) => r.id === secretId);
    expect(row?.has_capture).toBe(true);
    expect(row?.capture_locked).toBeFalsy();
    const detail = await pageB.request.get(`${GW}/api/portal/usage/captures/${secretId}`, { headers: { "X-OP-CSRF": "1" } });
    expect(detail.status()).toBe(200);
  }).toPass({ timeout: 15000 });

  await ctxB.close();
});

const CHATSESSION_PROBE = "e2e chatsession probe wzt";

// Proves Feature 5 (ChatSession): the token-overview has a non-deletable
// "ChatSession" row whose settings persist on the user profile and apply to chat
// run WITHOUT a token (session cookie, token_id==""). We first turn OFF the
// capture_override left on by the override test, so the capture here is driven
// purely by the ChatSession log_communication flag; the Geheim flag makes the
// resulting capture secret (owner-only), proving both profile flags flow through.
test("ChatSession row is non-deletable and captures token-less chat with the Geheim default", async ({ page }) => {
  await login(page, CAPTURE_ADMIN_EMAIL, CAPTURE_ADMIN_PASSWORD);
  await expect(page.getByText(t.welcome)).toBeVisible();

  // a. Isolate from the override test: ensure capture_override is OFF.
  await page.getByRole("link", { name: t.system }).click();
  const override = page.getByRole("checkbox", { name: t.captureOverrideLabel });
  await expect(override).toBeVisible();
  if (await override.isChecked()) {
    await override.uncheck();
    await page.getByRole("button", { name: t.save }).click();
    await expect(page.getByText(t.systemSaved)).toBeVisible();
  }

  // b. The ChatSession row exists, is non-deletable, and its two flags are editable.
  await page.getByRole("link", { name: t.apiTokens }).click();
  // The ChatSession pseudo-token is now its own settings Panel (role=region),
  // not a table row. It has no delete action and its two flags are editable.
  const chatPanel = page.getByRole("region", { name: t.chatSessionName });
  await expect(chatPanel).toBeVisible();
  await expect(chatPanel.getByRole("button", { name: t.tokenActionDelete })).toHaveCount(0);
  await chatPanel.getByRole("checkbox", { name: `${t.tokenLogCommunicationLabel} ${t.chatSessionName}` }).check();
  await chatPanel.getByRole("checkbox", { name: `${t.tokenSecretLabel} ${t.chatSessionName}` }).check();
  await chatPanel.getByRole("button", { name: t.tokenActionSave }).click();

  // c. Drive a TOKEN-LESS chat request (session cookie + CSRF, no bearer) and
  // confirm it was captured (log_communication) and secret (Geheim default →
  // owner sees the "Sichtbar machen" toggle). Retry around the ~5s settings-hook TTL.
  const viewButton = page.getByRole("button", { name: t.activityColView }).first();
  const dialog = page.getByRole("dialog");
  await expect(async () => {
    const resp = await page.request.post(`${GW}/v1/chat/completions`, {
      headers: { "Content-Type": "application/json", "X-OP-CSRF": "1" },
      data: { model: "qwen-coder", messages: [{ role: "user", content: CHATSESSION_PROBE }] }
    });
    expect(resp.status()).toBe(200);
    await page.reload();
    await page.getByRole("link", { name: t.usage }).click();
    await expect(viewButton).toBeVisible({ timeout: 3000 });
    await viewButton.click();
    await expect(dialog).toContainText(CHATSESSION_PROBE, { timeout: 3000 });
    // Assert the secret state INSIDE the loop: the ChatSession Save persists the
    // profile flags asynchronously, so an early request can be captured before
    // chat_secret lands. Requiring the owner-only "Sichtbar machen" toggle here
    // keeps re-sending until a captured request reflects the Geheim default.
    await expect(dialog.getByRole("button", { name: t.captureMarkVisible })).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: 45000 });
});
