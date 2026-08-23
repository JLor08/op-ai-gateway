// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "./helpers";

test("unauthenticated visit to /portal/ shows the login screen", async ({ page }) => {
  await page.goto("/portal/");
  await expect(page.getByRole("heading", { name: messages.de.signIn })).toBeVisible();
  await expect(page.getByText(messages.de.welcome)).toHaveCount(0);
});

test("bare / redirects to /portal/ and shows login", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/portal\/$/);
  await expect(page.getByRole("heading", { name: messages.de.signIn })).toBeVisible();
});

test("dev admin logs in and sees the dashboard", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await expect(page.getByText(messages.de.welcome)).toBeVisible();
});

test("logout returns to the login screen", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await page.getByRole("button", { name: "Dev User" }).click();
  await page.getByRole("menuitem", { name: messages.de.logout }).click();
  await expect(page.getByRole("heading", { name: messages.de.signIn })).toBeVisible();
});

test("invalid credentials show a localized error", async ({ page }) => {
  await page.goto("/portal/");
  await page.locator("#login-email").fill("dev@example.test");
  await page.locator("#login-password").fill("wrong-password");
  await page.getByRole("button", { name: messages.de.loginButton }).click();
  await expect(page.getByRole("alert")).toContainText(messages.de.errorAuthInvalidCredentials);
  await expect(page.getByRole("heading", { name: messages.de.signIn })).toBeVisible();
});

test("login without the CSRF header is rejected with 403", async ({ request }) => {
  const res = await request.post("/api/auth/login", {
    headers: { "Content-Type": "application/json" },
    data: { email: "dev@example.test", password: "dev-secret" }
  });
  expect(res.status()).toBe(403);
});

test("backend path /v1/models reaches the gateway (401 without token)", async ({ request }) => {
  const res = await request.get("/v1/models");
  expect(res.status()).toBe(401);
});
