// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { inviteUser, login, setPassword } from "./helpers";

// Generation now runs SERVER-SIDE: the browser POSTs
// /api/portal/chats/{id}/runs and subscribes to
// /api/portal/chats/{id}/runs/{runId}/events (SSE) for progress. These tests
// exercise that flow: concurrent chats each keep their own answer, a finished
// reply survives a reload (proving the server committed it), and Stop/cancel
// does not break the chat. The dev mock provider has no artificial delay, so
// every assertion below tolerates the run already being done by the time we
// look — never a fixed sleep.

// Each active/recently-finished run counts against a per-user cap
// (chatRunRegistry.maxPerUser = 5 in internal/gateway/chat_runs.go) that is
// only released `runEvictionDelay` (30s) after the run FINISHES, not while it
// is merely queued. This suite starts several runs per test (concurrency,
// persistence, stop), and chat.spec.ts starts a few more — all against the
// same shared (memory) gateway process. Sharing the single seeded
// "dev@example.test" account across both files would let their runs collide
// within that 30s window and 429 each other (ErrTooManyRuns) depending on
// run order, which is exactly what happened before this was isolated. So each
// test here logs in as its OWN freshly invited user (its own quota bucket),
// using the existing inviteUser/setPassword helpers (see invite.spec.ts).
async function freshChatUser(adminPage: Page, browser: Browser): Promise<Page> {
  await login(adminPage, "dev@example.test", "dev-secret");
  const { inviteUrl } = await inviteUser(adminPage);
  const context = await browser.newContext();
  const chatPage = await context.newPage();
  await setPassword(chatPage, inviteUrl, "chat-runs-e2e-password-1");
  // set-password auto-logs in; follow the success link to land on the dashboard.
  await chatPage.getByRole("link", { name: messages.de.signIn }).click();
  await expect(chatPage.getByText(messages.de.welcome)).toBeVisible();
  return chatPage;
}

// Mirrors chat.spec.ts: each test opens a FRESH chat so its transcript never
// collides with one left by a prior test on the shared (memory) gateway.
async function openFreshChat(page: Page): Promise<void> {
  await page.getByRole("link", { name: messages.de.chat }).click();
  await page.getByRole("button", { name: messages.de.chatNewChat }).click();
  await expect(page.locator('[data-role="assistant"]')).toHaveCount(0);
}

// The assistant/user turn of the CURRENT (active) chat only — background
// chats' messages live in an off-screen buffer, not the DOM.
const lastAssistant = (page: Page) => page.locator('[data-role="assistant"]').last();

// The chat list rail (Task 3.6 "ChatSidebar"), used to switch back to a
// chat that is not the active one.
const chatSidebar = (page: Page) => page.getByRole("navigation", { name: messages.de.chatSidebarLabel });

test("two chats can run concurrently and each keeps its own answer", async ({ page, browser }) => {
  const chatPage = await freshChatUser(page, browser);
  await openFreshChat(chatPage);

  await chatPage.locator("#chat-message").fill("frage eins");
  await chatPage.getByRole("button", { name: messages.de.send }).click();

  // Jump to a second, fresh chat right away — "Neuer Chat" is not gated on
  // the active chat's stream, so chat one's run keeps generating in the
  // background (its own EventSource + buffer) while chat two starts its own.
  await chatPage.getByRole("button", { name: messages.de.chatNewChat }).click();
  await expect(chatPage.locator('[data-role="assistant"]')).toHaveCount(0);

  await chatPage.locator("#chat-message").fill("frage zwei");
  await chatPage.getByRole("button", { name: messages.de.send }).click();
  // The mock echoes the prompt back ("Mock response for <model>: <prompt>"),
  // so this alone proves chat two got its OWN reply, not chat one's.
  await expect(lastAssistant(chatPage)).toContainText("Mock");
  await expect(lastAssistant(chatPage)).toContainText("frage zwei");

  // Switch back to the first chat via the sidebar (auto-titled from its first
  // user message); its own reply must still be there, untouched by chat two.
  await chatSidebar(chatPage).getByText("frage eins", { exact: true }).click();
  await expect(lastAssistant(chatPage)).toContainText("Mock");
  await expect(lastAssistant(chatPage)).toContainText("frage eins");

  await chatPage.context().close();
});

test("a finished reply survives reload (server-side persistence)", async ({ page, browser }) => {
  const chatPage = await freshChatUser(page, browser);
  await openFreshChat(chatPage);
  await chatPage.locator("#chat-message").fill("frage persistenz");
  await chatPage.getByRole("button", { name: messages.de.send }).click();
  // The run's `done` SSE event only arrives after the server has already
  // committed the turn (finishRun calls CommitAssistant before fanning out
  // "done"), so waiting for the full reply here means the reload below reopens
  // a chat the server already knows about — not one only the browser recalls.
  await expect(lastAssistant(chatPage)).toContainText("Mock");

  await chatPage.reload();
  await chatPage.getByRole("link", { name: messages.de.chat }).click();
  await expect(lastAssistant(chatPage)).toContainText("Mock");
  await expect(lastAssistant(chatPage)).toContainText("frage persistenz");

  await chatPage.context().close();
});

test("clicking Stop does not crash the chat", async ({ page, browser }) => {
  const chatPage = await freshChatUser(page, browser);
  await openFreshChat(chatPage);
  await chatPage.locator("#chat-message").fill("frage drei");
  await chatPage.getByRole("button", { name: messages.de.send }).click();

  // Click Stop when it is showing. The mock has no artificial delay, so the
  // run has very often already finished behind the scenes by the time we look
  // — cancelling an already-finished run is a safe no-op server-side (its
  // context is already done), so either way this must not crash the chat.
  // A genuinely-live mid-stream cancel is exercised separately by the Go unit
  // tests (TestCancelRunStopsIt), which use a paced (slow) provider to hold
  // the run open long enough to reliably land the cancel mid-flight — this
  // e2e intentionally does not race that timing with a real dev-mock browser
  // round trip.
  // Best-effort click with a short timeout: the instant mock can finish (and the
  // Stop button detach, reverting to Send) between an isVisible() check and the
  // click landing, so a plain click would retry until the test times out. A
  // failed/absent click just means the run already completed — a safe no-op.
  const stopButton = chatPage.getByRole("button", { name: messages.de.chatStop });
  await stopButton.click({ timeout: 1500 }).catch(() => {});

  // Whichever happened (canceled or already completed), the chat must not
  // crash and must still show the exchange: the user's own message stays put,
  // and — since the mock's instant completion means "already done" is by far
  // the overwhelmingly likely outcome here — the assistant's "Mock" reply is
  // present too.
  await expect(chatPage.locator('[data-role="user"]').last()).toContainText("frage drei");
  await expect(lastAssistant(chatPage)).toContainText("Mock");

  await chatPage.context().close();
});
