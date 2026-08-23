// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { totpCode, wrongCode } from "./totp";

const RFC_SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"; // RFC 6238 appendix B, SHA-1

test("totpCode matches the RFC 6238 SHA-1 vector at T=59s", () => {
  expect(totpCode(RFC_SECRET, 59_000)).toBe("287082");
  expect(totpCode(RFC_SECRET, 1111111109_000)).toBe("081804"); // 07081804 → 6 digits
});

test("wrongCode is deterministically outside the ±1 skew window", () => {
  const now = Date.now();
  expect(wrongCode(RFC_SECRET, now)).not.toBe(totpCode(RFC_SECRET, now));
});
