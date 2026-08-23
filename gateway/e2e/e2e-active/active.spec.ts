// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

const t = messages.de;
const GW = "http://127.0.0.1:8091";

// Live proof of Feature B (running connections). The gateway runs with
// OP_AI_GATEWAY_MOCK_DELAY_MS=2500 (see playwright.active.config.ts), so a single
// inference request stays in-flight long enough to observe it in the "Laufende
// Verbindungen" panel and then watch it clear — driven live by the SSE poke the
// in-flight registry emits on request start and end.
test("a running request appears in the Laufende Verbindungen panel and clears when done", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await page.getByRole("link", { name: t.usage }).click();

  // The panel is a labelled <section> (role=region); scope every assertion to it
  // so a "qwen-coder" row in the COMPLETED table below can never be mistaken for
  // a running one.
  const panel = page.getByRole("region", { name: t.activityActiveTitle });
  await expect(panel).toBeVisible();
  await expect(panel.getByText(t.activityActiveEmpty)).toBeVisible();

  // The single top-level own/all scope toggle is present (the dev user is a
  // system_admin). The leading "running connections" stat tile starts at 0.
  await expect(page.getByLabel(t.activityScopeLabel)).toBeVisible();
  const runTile = page.getByRole("article").filter({ hasText: t.activityActiveTitle });
  await expect(runTile).toContainText("0");

  // Fire a delayed inference request WITHOUT awaiting. page.request carries the
  // dev session cookie, so it authenticates token-less (no bearer) and the mock
  // holds it ~2.5s.
  const inflight = page.request.post(`${GW}/v1/chat/completions`, {
    headers: { "Content-Type": "application/json", "X-OP-CSRF": "1" },
    data: { model: "qwen-coder", messages: [{ role: "user", content: "inflight probe" }], stream: false }
  });

  // While in flight: the SSE start-poke refetches the active list, the empty
  // state disappears and a running row for the model shows (token-less → the
  // "Sitzung (ohne Token)" label).
  await expect(panel.getByText(t.activityActiveEmpty)).toHaveCount(0, { timeout: 4000 });
  await expect(panel.getByText("qwen-coder")).toBeVisible();
  await expect(panel.getByText(t.activityActiveSession)).toBeVisible();
  // ...and the leading stat tile counts it live.
  await expect(runTile).toContainText("1");

  // Complete it; the SSE end-poke clears the panel back to the empty state and
  // the running-count tile back to 0.
  const resp = await inflight;
  expect(resp.status()).toBe(200);
  await expect(panel.getByText(t.activityActiveEmpty)).toBeVisible({ timeout: 8000 });
  await expect(runTile).toContainText("0");
});

// Scope persists across navigation, and the running panel shows the user by name
// in all-scope (a real "Benutzer" column, not a faint sub-line).
test("scope persists across navigation; running panel shows the user in all-scope", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await page.getByRole("link", { name: t.usage }).click();

  // Switch to "Alle Benutzer" (the scope is a MUI Select now, not a native <select>).
  await page.getByRole("combobox", { name: t.activityScopeLabel }).click();
  await page.getByRole("option", { name: t.activityScopeAll }).click();

  // Navigate away and back — the scope is still "all" (persisted).
  await page.getByRole("link", { name: t.dashboard }).click();
  await page.getByRole("link", { name: t.usage }).click();
  await expect(page.getByRole("combobox", { name: t.activityScopeLabel })).toHaveText(t.activityScopeAll);

  // The running panel now has a Benutzer column (visible even while empty).
  const panel = page.getByRole("region", { name: t.activityActiveTitle });
  await expect(panel.getByRole("columnheader", { name: t.activityColOwner })).toBeVisible();

  // Drive a token-less request; its row shows the user's display name ("Dev User").
  const inflight = page.request.post(`${GW}/v1/chat/completions`, {
    headers: { "Content-Type": "application/json", "X-OP-CSRF": "1" },
    data: { model: "qwen-coder", messages: [{ role: "user", content: "owner probe" }], stream: false }
  });
  await expect(panel.getByText("Dev User")).toBeVisible({ timeout: 4000 });
  const resp = await inflight;
  expect(resp.status()).toBe(200);
});

// Time-series charts: the three charts + window/resolution controls render, the
// controls persist across navigation, and a completed request makes the charts
// gain live data (the interactive overlay appears only once a series is non-zero).
test("time-series charts render, controls persist, and charts gain live data", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await page.getByRole("link", { name: t.usage }).click();

  // Controls + the two charts are present (Verbindungen + the combined Tokens/s
  // chart; charts show a no-data placeholder until there is data — the title
  // <svg role=img aria-label> is present either way). Window/resolution are MUI
  // Select dropdowns now (role=combobox), not toggle groups.
  await expect(page.getByRole("combobox", { name: t.activityTsWindowLabel })).toBeVisible();
  await expect(page.getByRole("combobox", { name: t.activityTsBucketLabel })).toBeVisible();
  await expect(page.getByRole("img", { name: t.activityTsConnections })).toBeVisible();
  await expect(page.getByRole("img", { name: t.activityTsTokenThroughput })).toBeVisible();

  // Resolution dropdown switches to "10s" and persists across a menu switch.
  await page.getByRole("combobox", { name: t.activityTsBucketLabel }).click();
  await page.getByRole("option", { name: "10s", exact: true }).click();
  await page.getByRole("link", { name: t.dashboard }).click();
  await page.getByRole("link", { name: t.usage }).click();
  await expect(page.getByRole("combobox", { name: t.activityTsBucketLabel })).toHaveText("10s");

  // Drive one completed request; the charts gain data → the interactive overlay
  // appears (proves the /usage/timeseries → chart pipeline live).
  const resp = await page.request.post(`${GW}/v1/chat/completions`, {
    headers: { "Content-Type": "application/json", "X-OP-CSRF": "1" },
    data: { model: "qwen-coder", messages: [{ role: "user", content: "chart probe" }], stream: false }
  });
  expect(resp.status()).toBe(200);
  await expect(page.getByTestId("ts-overlay").first()).toBeVisible({ timeout: 8000 });
});
