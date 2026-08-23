// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";

const t = messages.de;

// The seeded mock application is booted in model_sync mode
// (OP_AI_GATEWAY_SEED_APP_HEALTH_MODE=model_sync), so the app-health loop checks
// its reachability via the model-discovery endpoint (ListModels), not an HTTP
// health path. With OP_AI_GATEWAY_MOCK_UNREACHABLE=true that discovery call (and
// its retry) fails → the app is unreachable → the mock server derives to
// Unavailable and the gateway offers only reachable applications' models. This
// proves the model_sync path end to end: reachability via the model-abgleich
// endpoint gates the offered models ("Nur die Modelle von erreichbaren
// Anwendungen werden vom Gateway angeboten").
test("a model_sync server whose discovery fails shows Unavailable and hides its models", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");

  // The portal loads servers + models once per mount; the gateway booted behind
  // the preview build, so the probe (+retry) has settled. Reload-and-check
  // retries the mount fetch to self-heal against a slow first cycle.
  await expect(async () => {
    await page.reload();
    await page.getByRole("link", { name: t.servers }).click();
    const row = page.getByRole("row").filter({ hasText: "Mock Server" });
    await expect(row).toBeVisible({ timeout: 2000 });
    await expect(row.getByText(t.healthUnhealthy)).toBeVisible({ timeout: 2000 });
  }).toPass({ timeout: 15000 });

  // Modelle view: the model_sync app's models are gone — reachability via the
  // discovery endpoint failed, so the gateway offers none of them.
  await page.getByRole("link", { name: t.models }).click();
  await expect(page.getByText("qwen-coder")).toHaveCount(0);
  await expect(page.getByText("gpt-oss-20b")).toHaveCount(0);
});
