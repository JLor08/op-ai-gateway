// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, inviteUser, setPassword } from "../e2e/helpers";
import { LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD } from "../playwright.limits.config";

const t = messages.de;
const GW = "http://127.0.0.1:8091";

// Seeded on the fresh sqlite DB by cmd/gateway's seedDefaultServerIfEmpty --
// the SAME mock server/app/mappings as memory mode (qwen-coder / gpt-oss-20b);
// see playwright.limits.config.ts's header comment for why this suite runs in
// sqlite mode rather than the default memory mode.
const MODEL = "qwen-coder";

// >= 10 chars per the password policy in internal/auth. Test-only.
const INVITED_USER_PASSWORD = "E2E-User-Pass-1";

// Opens a period SelectField (a non-native MUI Select) scoped to `scope` at
// the given index among the -- possibly several, identically-labelled --
// period dropdowns a LimitsEditor instance renders (request-quota=0,
// token-quota=1, cost-budget=2, in declaration order -- mirrors
// LimitsEditor.test.tsx's pickPeriod helper), and picks the option with the
// given label. The option itself is always looked up on `page` (not `scope`)
// because MUI portals its popup menu to document.body, outside any dialog's
// DOM subtree.
async function pickLimitPeriod(scope: Page | Locator, page: Page, index: number, optionLabel: string) {
  await scope.getByRole("combobox", { name: t.limitPeriodLabel }).nth(index).click();
  await page.getByRole("option", { name: optionLabel }).click();
}

type LimitsInput = {
  rateRequests?: number;
  rateWindowSeconds?: number;
  requestQuota?: number;
  tokenQuota?: number;
};

// Invites a fresh user, sets the given rate/quota limits on them via the
// admin Users > Limits dialog, redeems the invite + logs in as that user in
// an ISOLATED browser context, mints a personal API token (self-service,
// POST /api/portal/tokens), and returns the one-time secret. The isolated
// context is closed before returning (the caller only needs the secret,
// driven afterward via the plain `request` fixture -- pure bearer-token auth,
// no cookies involved).
//
// WHY a personal token (not a Service token, despite this suite proving the
// "Principal Limits" feature that also covers services): these three
// enforcement tests were originally written against a personal token to
// dodge a bug, unrelated to Phase 2, that once made a Service token 502
// before the limiter was even reached (route_affinity.user_id was `not null
// references users(id)`, and a service token's affinity pin has UserID=""
// -- see migration42Up's doc comment in internal/store/migrate.go for the
// full writeup). That bug is now FIXED (migration v42) and proven live by
// the dedicated "service token: serves inference end-to-end" test below; a
// personal/user-owned token still exercises the exact same
// PrincipalLimiter.Admit/Record code path a Service token would, so these
// three are kept as-is rather than churned for no behavioral gain.
async function inviteUserWithLimitsAndToken(
  adminPage: Page,
  browser: Browser,
  limits: LimitsInput
): Promise<{ secret: string; email: string }> {
  const { email, inviteUrl } = await inviteUser(adminPage, { role: "user" });

  await adminPage.getByRole("link", { name: t.users }).click();
  // inviteUser always names the invitee "E2E User" (see e2e/helpers.ts), so
  // once more than one is invited in this suite the row must be disambiguated
  // by its unique email instead.
  const row = adminPage.getByRole("row", { name: new RegExp(email) });
  await row.getByRole("button", { name: t.userActionLimits }).click();
  const dialog = adminPage.getByRole("dialog");
  if (limits.rateRequests !== undefined) {
    await dialog.locator("#user-limits-limit-rate-requests").fill(String(limits.rateRequests));
    await dialog.locator("#user-limits-limit-rate-window").fill(String(limits.rateWindowSeconds ?? 60));
  }
  if (limits.requestQuota !== undefined) {
    await dialog.locator("#user-limits-limit-request-quota").fill(String(limits.requestQuota));
    await pickLimitPeriod(dialog, adminPage, 0, t.limitPeriodDay);
  }
  if (limits.tokenQuota !== undefined) {
    await dialog.locator("#user-limits-limit-token-quota").fill(String(limits.tokenQuota));
    await pickLimitPeriod(dialog, adminPage, 1, t.limitPeriodDay);
  }
  await dialog.getByRole("button", { name: t.save }).click();
  await expect(dialog).not.toBeVisible();

  // browser.newContext() does NOT inherit the project's `use.baseURL` the way
  // the `page` fixture's default context does -- pass it explicitly so
  // login()'s relative page.goto("/portal/") resolves.
  const userContext = await browser.newContext({ baseURL: "http://127.0.0.1:4173" });
  const userPage = await userContext.newPage();
  await setPassword(userPage, inviteUrl, INVITED_USER_PASSWORD);
  // account.Service.SetPassword redeems the token, sets the password, AND
  // creates a session (the invited user is already logged in at this point) --
  // App.tsx renders the SetPassword view unconditionally while the URL still
  // carries the set-password token, regardless of auth state, so there is no
  // login FORM to fill here. Navigate away from it (into the now-authenticated
  // app) instead of calling login(), which would hang forever waiting for a
  // login form that never appears.
  await userPage.goto("/portal/");
  await expect(userPage.getByText(t.welcome)).toBeVisible();

  await userPage.getByRole("link", { name: t.apiTokens }).click();
  await userPage.getByRole("button", { name: t.tokenCreate }).click();
  await userPage.locator("#token-name").fill("e2e-limits-token");
  await userPage.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await userPage.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);

  await userContext.close();
  return { secret, email };
}

test.describe("Principal Limits (Phase 2) — end-to-end enforcement", () => {
  test("rate limit: the second request within the window is refused with 429 + Retry-After", async ({
    page,
    request,
    browser
  }) => {
    await login(page, LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD);

    const { secret } = await inviteUserWithLimitsAndToken(page, browser, { rateRequests: 1, rateWindowSeconds: 60 });

    // First request: allowed, admits into the current 60s window's bucket.
    const first = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "first" }], stream: false }
    });
    expect(first.status()).toBe(200);

    // Second request: same window, bucket already at its cap of 1 -> refused.
    const second = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "second" }], stream: false }
    });
    expect(second.status()).toBe(429);
    const body = await second.json();
    expect(body.error?.code).toBe("limit.rate_limited");
    // The rate-limit denial (and only it) carries a whole-seconds Retry-After.
    const retryAfter = second.headers()["retry-after"];
    expect(retryAfter).toBeTruthy();
    expect(Number(retryAfter)).toBeGreaterThan(0);
  });

  test("request quota: the second request in the period is refused, and the admin view reads back the usage", async ({
    page,
    request,
    browser
  }) => {
    await login(page, LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD);

    const { secret, email } = await inviteUserWithLimitsAndToken(page, browser, { requestQuota: 1 });

    const first = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "first" }], stream: false }
    });
    expect(first.status()).toBe(200);

    const second = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "second" }], stream: false }
    });
    expect(second.status()).toBe(429);
    const body = await second.json();
    expect(body.error?.code).toBe("limit.request_quota_exceeded");

    // Read back the persisted aggregate through the SAME admin dialog used to
    // configure the limit: exactly ONE request was ever recorded (the second
    // was refused pre-Resolve, so recordUsage never ran for it).
    const row = page.getByRole("row", { name: new RegExp(email) });
    await row.getByRole("button", { name: t.userActionLimits }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(t.limitUsageRequestsLine(1, 1))).toBeVisible();
    await dialog.getByRole("button", { name: t.cancel }).click();
  });

  test("token quota: the second request is refused once the first response's tokens exceed the quota", async ({
    page,
    request,
    browser
  }) => {
    await login(page, LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD);

    // Threshold 1: ANY successful mock response (which always echoes at least
    // one word back, so total_tokens > 0) exceeds it, without depending on
    // the exact word count the mock's echo produces.
    const { secret } = await inviteUserWithLimitsAndToken(page, browser, { tokenQuota: 1 });

    const first = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "first" }], stream: false }
    });
    expect(first.status()).toBe(200);

    const second = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "second" }], stream: false }
    });
    expect(second.status()).toBe(429);
    const body = await second.json();
    expect(body.error?.code).toBe("limit.token_quota_exceeded");
  });

  // Service limits: a config-persistence proof ONLY -- NOT live enforcement
  // (kept scoped exactly as it was; live serving for a Service token is now
  // proven by the dedicated "service token: serves inference end-to-end"
  // test below).
  //
  // This test was originally written config-only because, at the time,
  // driving a real request through a Service token all the way to a
  // resolved route hit a confirmed bug this suite surfaced, unrelated to
  // Phase 2: routing.RouteAffinity.UserID is bound literally from
  // auth.Token.UserID, which is "" for a SERVICE token (a service has no
  // owning user); route_affinity.user_id was `not null references
  // users(id)`, so pinning an affinity for a service token -- which happens
  // on every resolved request whenever the serving application's affinity
  // TTL > 0 (the seeded mock app's default, 1800s, and
  // portal.normalizeApplicationAffinityTTLSeconds floors ANY value <= 0 back
  // up to that default -- there is no admin-facing way to configure it to
  // 0/off) -- violated that foreign key and 502'd with "store affinity:
  // store: not found" on any FK-enforcing store (sqlite/postgres); the
  // in-memory store never enforces FKs, which is why Phase 1's e2e (memory
  // mode) never caught it. FIXED in migration v42 (route_affinity.user_id is
  // now nullable; see migration42Up's doc comment in
  // internal/store/migrate.go) -- this test itself did not need to change,
  // since it never drove a request.
  //
  // The actual Service-principal Admit/Record logic this feature adds IS
  // already proven, independent of that (now-fixed) unrelated bug, by
  // internal/gateway/principal_limits_wiring_test.go's
  // TestPrincipalLimiterRateLimitDenies / …RequestQuotaDeniesAfterRecord /
  // …TokenQuotaDeniesAfterRecord / …CostBudgetDenies (all four exercise a
  // routing.PrincipalTypeService principal end-to-end through admitPrincipal
  // and writeLimitDenied, against a fake store that has no FK to trip), plus
  // internal/portal/service_limits_test.go's persistence/validation coverage.
  // What THIS test adds on top: proof that the Services UI itself lets an
  // admin configure every limit block and that the values round-trip through
  // a real save + reload against a real (sqlite) backend.
  test("service limits: the Limits block set at create time round-trips through the detail view", async ({
    page
  }) => {
    await login(page, LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD);

    await page.getByRole("link", { name: t.services }).click();
    await page.getByRole("button", { name: t.serviceCreate }).click();
    await page.locator("#service-name").fill("E2E Service Limits Config");
    await page.locator("#service-create-limit-rate-requests").fill("7");
    await page.locator("#service-create-limit-rate-window").fill("42");
    await page.locator("#service-create-limit-request-quota").fill("123");
    await pickLimitPeriod(page, page, 0, t.limitPeriodDay);
    await page.locator("#service-create-limit-token-quota").fill("456");
    await pickLimitPeriod(page, page, 1, t.limitPeriodWeek);
    await page.locator("#service-create-limit-cost-budget").fill("9.5");
    await pickLimitPeriod(page, page, 2, t.limitPeriodMonth);
    await page.getByRole("button", { name: t.serviceCreate }).click();

    // Back on the list -- open the freshly created service's detail view and
    // confirm every field survived the round-trip through the backend.
    const row = page.getByRole("row", { name: /E2E Service Limits Config/ });
    await row.getByRole("button", { name: t.modelDetailsAction }).click();

    await expect(page.locator("#service-detail-limit-rate-requests")).toHaveValue("7");
    await expect(page.locator("#service-detail-limit-rate-window")).toHaveValue("42");
    await expect(page.locator("#service-detail-limit-request-quota")).toHaveValue("123");
    await expect(page.locator("#service-detail-limit-token-quota")).toHaveValue("456");
    await expect(page.locator("#service-detail-limit-cost-budget")).toHaveValue("9.5");
    await expect(page.getByRole("combobox", { name: t.limitPeriodLabel }).nth(0)).toHaveText(t.limitPeriodDay);
    await expect(page.getByRole("combobox", { name: t.limitPeriodLabel }).nth(1)).toHaveText(t.limitPeriodWeek);
    await expect(page.getByRole("combobox", { name: t.limitPeriodLabel }).nth(2)).toHaveText(t.limitPeriodMonth);
  });

  // Service token — LIVE serving proof that the route_affinity.user_id
  // nullable fix (migration42Up, internal/store/migrate.go) actually closes
  // the gap the comment on inviteUserWithLimitsAndToken above describes:
  // route_affinity.user_id used to be `not null references users(id) on
  // delete cascade`, and a service token's affinity is pinned with
  // UserID="" (a service has no owning user) -- an empty string against a
  // NOT NULL FK column is checked against the referenced table (unlike a
  // genuine SQL NULL, which the constraint exempts), and users("") never
  // exists, so that pin 502'd on any FK-enforcing store (sqlite/postgres) --
  // exactly why the three enforcement tests above drive a personal token
  // instead, and why the "service limits" test above is a config-persistence
  // proof only. Now that the column is nullable and the write path converts
  // an empty UserID to a genuine NULL, this proves the fix end-to-end
  // against this suite's real sqlite-backed (foreign_keys=ON) gateway: a
  // freshly minted SERVICE token serves /v1/chat/completions successfully
  // (200, not 502) TWICE in a row -- the seeded mock app's affinity TTL is
  // 1800s (> 0; portal.normalizeApplicationAffinityTTLSeconds floors any
  // configured value <= 0 back up to it, so it can never be "off"), so the
  // FIRST request INSERTs the route_affinity row and the SECOND re-upserts
  // the SAME row (the sqlite `on conflict … do update` path) -- exercising
  // both write paths the fix touches.
  test("service token: serves inference end-to-end (proves the route_affinity.user_id nullable fix)", async ({
    page,
    request
  }) => {
    await login(page, LIMITS_ADMIN_EMAIL, LIMITS_ADMIN_PASSWORD);

    await page.getByRole("link", { name: t.services }).click();
    await page.getByRole("button", { name: t.serviceCreate }).click();
    await page.locator("#service-name").fill("E2E Service Token Serve");
    await page.getByRole("button", { name: t.serviceCreate }).click();

    const row = page.getByRole("row", { name: /E2E Service Token Serve/ });
    await row.getByRole("button", { name: t.modelDetailsAction }).click();

    // Opens the create-token dialog; its own submit button shares the same
    // label, so it must be scoped to the dialog once open (the background
    // "open dialog" trigger stays mounted behind it).
    await page.getByRole("button", { name: t.serviceTokenCreate }).click();
    const tokenDialog = page.getByRole("dialog");
    await tokenDialog.locator("#service-token-name").fill("e2e-serve-token");
    await tokenDialog.getByRole("button", { name: t.serviceTokenCreate }).click();
    const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
    expect(secret.length).toBeGreaterThan(0);

    const first = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "first" }], stream: false }
    });
    expect(first.status(), `first service-token request must serve (200), not 502: ${await first.text()}`).toBe(200);
    const firstBody = await first.json();
    expect(Array.isArray(firstBody.choices)).toBe(true);

    const second = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "second" }], stream: false }
    });
    expect(second.status(), `second service-token request (affinity re-upsert path) must serve (200): ${await second.text()}`).toBe(200);
  });

  // Cost-budget LIVE enforcement is deliberately NOT exercised here either:
  // the seeded mock provider always reports energy_wh=0 (no energy
  // attribution), so UsageAggregateSince's price-weighted cost sum stays 0
  // for every request this suite can drive -- a cost_budget threshold could
  // never trip without fabricating cost, which would not be a genuine proof.
  // The full config -> Admit -> HTTP 402 mapping chain (with a controlled,
  // non-zero cost from a fake store) is covered by
  // internal/gateway/principal_limits_wiring_test.go's
  // TestPrincipalLimiterCostBudgetDenies / TestWriteLimitDeniedCostBudget,
  // plus internal/portal/service_limits_test.go,
  // internal/portal/service_user_limits_test.go, internal/portal/limits_test.go,
  // and the store conformance suite (internal/store/conformance_test.go).
});
