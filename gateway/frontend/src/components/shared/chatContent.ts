// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The opaque chat-message content shapes shared by the chat store and the
// message renderer. Relocated out of the retired client-side streaming
// module — these types outlived that code because messages are persisted
// through them regardless of who drives the run.
export type ChatRole = 'system' | 'user' | 'assistant';
export type ChatContentPart =
  { type: 'text'; text: string } | { type: 'image_url'; image_url: { url: string } };
export type ChatContent = string | ChatContentPart[];
