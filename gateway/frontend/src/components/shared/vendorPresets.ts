// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { VendorAccountVendor } from '../../api';

// The vendors reached through a configurable root URL (the backend's
// routing.IsPresetVendor), in the order the create form lists them after openai
// and anthropic. All but google_native speak the OpenAI wire format;
// google_native speaks Google's native generateContent API but has the same
// create-form shape. They are api-key only and carry an immutable base_url;
// openai and anthropic have fixed hosts.
export const CONFIGURABLE_ROOT_VENDORS = [
  'xai',
  'openrouter',
  'kilo',
  'google',
  'google_native',
  'openai_compatible',
] as const satisfies readonly VendorAccountVendor[];

export type ConfigurableRootVendor = (typeof CONFIGURABLE_ROOT_VENDORS)[number];

/** Whether `vendor` is a configurable-root preset vendor (api-key only, configurable base URL; the backend's routing.IsPresetVendor). */
export function isConfigurableRootVendor(vendor: string): vendor is ConfigurableRootVendor {
  return (CONFIGURABLE_ROOT_VENDORS as readonly string[]).includes(vendor);
}

// The default root of each preset, shown as the base-URL field's PLACEHOLDER so
// the user sees what an empty field means. Display-only: the backend's vendor
// registry (routing.VendorPresetFor) is authoritative -- it applies the real
// default when base_url is omitted, so a drift here can only mislabel a
// placeholder, never change where a request goes. "Custom" has no default; its
// placeholder is a mere example of the shape.
const DEFAULT_BASE_URLS: Record<ConfigurableRootVendor, string> = {
  xai: 'https://api.x.ai',
  openrouter: 'https://openrouter.ai/api',
  kilo: 'https://api.kilo.ai/api',
  google: 'https://generativelanguage.googleapis.com',
  google_native: 'https://generativelanguage.googleapis.com',
  openai_compatible: 'https://llm.example.com',
};

/** The placeholder root for a vendor's base-URL field; "" for a vendor with a fixed host. */
export function defaultBaseUrl(vendor: string): string {
  return isConfigurableRootVendor(vendor) ? DEFAULT_BASE_URLS[vendor] : '';
}
