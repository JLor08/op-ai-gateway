// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

const t = messages.de;

// The seeded mock application is NOT always_reachable, so the app-health probe
// loop probes it via the mock prober. The gateway runs with
// OP_AI_GATEWAY_MOCK_UNREACHABLE=true (see playwright.health.config.ts), so the
// startup probe and its 2s retry both fail → the mock server derives to
// Unavailable and the gateway offers only reachable applications' models. This
// proves the user's headline requirement end to end: "Nur die Modelle von
// erreichbaren Anwendungen werden vom Gateway angeboten."
test("an unreachable server shows Unavailable and its models are hidden", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  // The portal loads servers + models once per mount. The gateway booted well
  // before this test (behind the preview build), so the probe (+retry) has long
  // since settled; reload-and-check retries the mount fetch to self-heal against
  // a slow first probe cycle without a fixed sleep.
  await expect(async () => {
    await page.reload();
    await page.getByRole("link", { name: t.servers }).click();
    const row = page.getByRole("row").filter({ hasText: "Mock Server" });
    await expect(row).toBeVisible({ timeout: 2000 });
    await expect(row.getByText(t.healthUnhealthy)).toBeVisible({ timeout: 2000 });
  }).toPass({ timeout: 15000 });

  // Modelle view: the mock's models are gone — the gateway offers only models of
  // reachable applications.
  await page.getByRole("link", { name: t.models }).click();
  await expect(page.getByText("qwen-coder")).toHaveCount(0);
  await expect(page.getByText("gpt-oss-20b")).toHaveCount(0);
});
