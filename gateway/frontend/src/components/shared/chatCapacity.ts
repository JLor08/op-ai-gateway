// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// How much room an image thread has left inside the backend's per-chat content
// cap. A generated image is persisted as a base64 data URL INSIDE the chat
// document, so images — and essentially nothing else — are what fill that cap,
// and a thread that hits it loses the very artifact it just spent minutes
// producing (the run finishes, the save is refused). The composer therefore
// states the remaining capacity BEFORE the user commits, and refuses a send it
// can already tell will not fit.
//
// The cap itself is NEVER hardcoded here: it is portal.MaxChatContentBytes,
// served on the chat listing (ChatListResponse.max_content_bytes). A second
// copy of that constant in TypeScript would drift from the Go one with nothing
// to catch it, and the failure mode would be a capacity line that confidently
// states the wrong number — the exact defect this composer exists to avoid.
//
// This module lives in components/shared/ rather than components/chat/ because
// Chat.tsx (outside the chat/ import boundary) is one of its consumers, and it
// deliberately takes STRUCTURAL message shapes instead of importing
// ChatUiMessage from components/chat/chatDoc.ts, which that same boundary
// forbids (arch.test.ts).

// Fallback cost of one generated image, used only until the thread has
// actually produced one: a 1024x1024 PNG (~1.1 MiB) inflated by base64's 4/3.
// It is an estimate and the UI says "about"; once the thread has produced an
// image, the observed size replaces it and the number becomes grounded in what
// this model really emits.
export const ESTIMATED_IMAGE_BYTES = 1_500_000;

// The minimum this module needs of a transcript message. Structural on
// purpose — see the import-boundary note above.
type CapacityMessage = { content?: unknown };

type ImagePart = { image_url?: { url?: unknown } };

// Approximate size of the stored document, in the same units the backend's cap
// counts. Two deliberate approximations, both far inside the error bar of the
// per-image estimate this feeds:
//   - only the transcript is measured, not the settings block (a few hundred
//     bytes against a 4 MiB cap);
//   - JSON string LENGTH stands in for UTF-8 byte length, which is exact for
//     the base64 ASCII that dominates any transcript large enough to matter.
export function documentBytes(messages: readonly CapacityMessage[]): number {
  return JSON.stringify(messages).length;
}

// What one more image is expected to cost: the largest image this thread has
// already stored, or the fallback estimate while it has none. Largest (not
// average) because the question being answered is "can we be sure another one
// fits", and the thread's own output is the best available predictor of its
// next output's size.
export function imageCostBytes(messages: readonly CapacityMessage[]): number {
  let largest = 0;
  for (const message of messages) {
    if (!Array.isArray(message.content)) continue;
    for (const part of message.content as ImagePart[]) {
      const url = part?.image_url?.url;
      if (typeof url === 'string' && url.length > largest) largest = url.length;
    }
  }
  return largest > 0 ? largest : ESTIMATED_IMAGE_BYTES;
}

// How many more images are expected to fit, or null when the cap is unknown.
//
// null is a real state, not an error: a gateway older than the served
// max_content_bytes sends nothing, and the portal must then say nothing and
// refuse nothing rather than invent a number. Callers must treat null as
// "unknown", never as zero.
export function imagesLeft(
  messages: readonly CapacityMessage[],
  maxContentBytes: number,
): number | null {
  if (maxContentBytes <= 0) return null;
  const remaining = maxContentBytes - documentBytes(messages);
  return Math.max(0, Math.floor(remaining / imageCostBytes(messages)));
}
