// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "./helpers";

// Chats now persist server-side and the newest is auto-opened on load, so each
// test starts in a FRESH chat (via the sidebar's "Neuer Chat") to isolate its
// transcript from any chat a prior test left on the shared (memory) gateway.
async function openFreshChat(page: Page): Promise<void> {
  await page.getByRole("link", { name: messages.de.chat }).click();
  await page.getByRole("button", { name: messages.de.chatNewChat }).click();
  await expect(page.locator('[data-role="assistant"]')).toHaveCount(0);
}

// The assistant/user turn of the CURRENT chat: use .last() so a stray earlier
// bubble (shared gateway, async persistence) never trips strict-mode matching.
const lastAssistant = (page: Page) => page.locator('[data-role="assistant"]').last();

test("a user sends a chat message and sees the assistant reply stream in", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await openFreshChat(page);
  await page.locator("#chat-message").fill("Hallo Gateway");
  await page.getByRole("button", { name: messages.de.send }).click();
  // The dev mock provider streams "Mock response for <model>: <prompt>" over SSE;
  // toContainText auto-waits for the streamed tokens to arrive.
  await expect(lastAssistant(page)).toContainText("Mock");
});

// 1x1 transparent PNG (valid, tiny) for the attach flow.
const TINY_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64"
);

// The chat transcript + stream live in a store above the view switch, so leaving
// the Chat view no longer aborts/loses the reply.
test("chat keeps its answer across a menu switch (background store)", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await openFreshChat(page);
  await page.locator("#chat-message").fill("Hallo Gateway");
  await page.getByRole("button", { name: messages.de.send }).click();
  await expect(lastAssistant(page)).toContainText("Mock");
  // Navigate away then back.
  await page.getByRole("link", { name: messages.de.dashboard }).click();
  await expect(page.locator('[data-role="assistant"]')).toHaveCount(0); // chat view unmounted
  await page.getByRole("link", { name: messages.de.chat }).click();
  // The reply is present and complete — not aborted/lost by the view unmount.
  await expect(lastAssistant(page)).toContainText("Mock");
});

// A sent image + its answer survive navigation AND a full reload — now backed by
// the encrypted server chat store rather than localStorage.
test("chat remembers a sent image and its answer across navigation and reload", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  await openFreshChat(page);
  await page.locator('input[type="file"]').setInputFiles({ name: "pic.png", mimeType: "image/png", buffer: TINY_PNG });
  // Wait for the composer preview (attach is async: validate + downscale via canvas).
  await expect(page.getByAltText(messages.de.chatAttachedImage)).toHaveCount(1);
  await page.locator("#chat-message").fill("look at this");
  await page.getByRole("button", { name: messages.de.send }).click();
  await expect(lastAssistant(page)).toContainText("Mock");
  await expect(page.locator('[data-role="user"] img').last()).toBeVisible();

  // a. Switch menu away and back — the view unmounts but the store keeps the turn.
  await page.getByRole("link", { name: messages.de.dashboard }).click();
  await expect(page.locator('[data-role="user"]')).toHaveCount(0);
  await page.getByRole("link", { name: messages.de.chat }).click();
  await expect(page.locator('[data-role="user"] img').last()).toBeVisible();
  await expect(lastAssistant(page)).toContainText("Mock");

  // b. Full reload — the run already committed the transcript server-side
  // before its `done` SSE event arrived (finishRun calls CommitAssistant
  // before fanning out "done"), and the assistant reply above already waited
  // for that "Mock" text, so the server-side commit has already happened.
  await page.reload();
  await page.getByRole("link", { name: messages.de.chat }).click();
  await expect(page.locator('[data-role="user"] img').last()).toBeVisible();
  await expect(lastAssistant(page)).toContainText("Mock");
});
