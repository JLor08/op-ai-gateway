// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

const t = messages.de;
const GW = "http://127.0.0.1:8091";
const AGENT = "dev-agent-secret"; // seeded agent-token secret bound to mock-server on loopback

// A minimal-but-valid agent telemetry body (mirrors the perf SSE test's shape).
// server_id is omitted — the intake derives the target from the agent token — so a
// valid post succeeds (200) and the gateway logs a Debug "agent telemetry received"
// record. The bearer token is NEVER part of the logged attrs (server_id + counts
// only), which is exactly why this view is safe to expose to a system admin.
function validTelemetry(): string {
  return JSON.stringify({
    reported_at: new Date().toISOString(),
    cpu_load: 0.5,
    active_requests: 1,
    queue_depth: 0,
    provider_health: {},
    capabilities: {},
    host: { cpu_util_pct: 50, mem_used_bytes: 1, mem_total_bytes: 2 },
    gpus: [{ index: 0, name: "E2E-LOGS-GPU", util_pct: 10 }]
  });
}

// The system-admin Logs view renders live gateway logs and lets the admin change
// the log level at runtime. The full loop is exercised end to end: open the gated
// bottom nav item, switch the runtime level to Debug (a live PUT to the gateway),
// then drive a fresh Debug log line (a valid agent telemetry POST) and assert it
// arrives live over the SSE stream. The Debug line is captured ONLY because we
// switched to Debug — at the default info level it is filtered out — so its
// appearance also proves the runtime level switch actually took effect.
test("system-admin Logs view: live logs + runtime level switch", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  // Open the bottom "Logs" nav item (visible only to system_admin) and assert the
  // view + its runtime level dropdown are present.
  await page.getByRole("link", { name: t.logsNav }).click();
  await expect(page.getByRole("heading", { name: t.logsTitle })).toBeVisible();
  const levelSelect = page.getByRole("combobox", { name: t.logsLevelLabel });
  await expect(levelSelect).toBeVisible();

  // Switch the runtime log level to Debug (the SelectField is a non-native MUI
  // Select: open it, then pick the option).
  await levelSelect.click();
  await page.getByRole("option", { name: t.logsLevelDebug }).click();

  // The UI update is optimistic; confirm the GATEWAY actually applied the new level
  // before driving a Debug line, so there is no PUT-vs-POST race.
  await expect
    .poll(
      async () => {
        const r = await page.request.get(`${GW}/api/system/logs/level`);
        if (!r.ok()) return "";
        return (await r.json()).level;
      },
      { timeout: 10000, intervals: [200] }
    )
    .toBe("debug");

  // Drive a fresh gateway log line: a valid agent telemetry POST (Bearer token; no
  // CSRF on the machine endpoint) logs a Debug "agent telemetry received" record.
  const posted = await page.request.post(`${GW}/api/agent/v1/telemetry`, {
    headers: { Authorization: `Bearer ${AGENT}`, "Content-Type": "application/json" },
    data: validTelemetry()
  });
  expect(posted.status()).toBe(200);
  expect((await posted.json()).accepted).toBe(true);

  // The live SSE stream delivers the new Debug record and the view renders it. This
  // proves live delivery AND that the runtime level switch took effect (the line is
  // filtered out at the default info level).
  await expect(page.getByText("agent telemetry received").first()).toBeVisible({ timeout: 15000 });
});
