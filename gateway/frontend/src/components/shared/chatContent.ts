// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The opaque chat-message content shapes shared by the chat store and the
// message renderer. Relocated out of the retired client-side streaming
// module — these types outlived that code because messages are persisted
// through them regardless of who drives the run.
export type ChatRole = 'system' | 'user' | 'assistant';
// An image part carries its purpose only when it is not the default: `purpose:
// 'mask'` marks the MASK of an image edit (inpainting) -- see maskPart below -- and
// a part without it is an ordinary image (an attachment, a source, a generated
// result).
//
// An `audio_url` part is audio in either direction, told apart by the role of the turn
// that carries it: `{ type: 'audio_url', audio_url: { url } }`. In an ASSISTANT turn it is
// the ANSWER of a speech (text-to-speech) turn, the audio twin of a generated image, the
// WAV as an inline `data:audio/wav;base64,...` url while the run streams. In a USER turn it
// is the INPUT of a transcription (speech-to-text) turn: the clip the composer attached or
// recorded, as an inline `data:audio/<type>;base64,...` url (wav, webm, mpeg, mp4 or ogg),
// which the turn consists of entirely (its reply is an ordinary text part, the transcript).
// Either way it is a /api/portal/chat-blobs/{id} reference once the run's commit moved it
// into the chat blob store, and the key and the `audio_url` name are the contract with the
// backend's executors and blob extraction (portal extractChatBlobs).
export type ChatContentPart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string }; purpose?: 'mask' }
  | { type: 'audio_url'; audio_url: { url: string } };
export type ChatContent = string | ChatContentPart[];

// The MASK of an image edit (inpainting) -- a PNG whose transparent pixels mark the
// region to edit, the size of the source image -- is a part of the user message
// like the source is:
//
//   { type: 'image_url', image_url: { url }, purpose: 'mask' }
//
// An image part like any other (so it is stored as a chat blob on the run commit,
// resolved back to its bytes by the same owner-scoped lookup, and survives an edit
// or a replay of the message exactly as the source does), told from a source by the
// `purpose` tag alone. The backend's executor reads the tag (portal.MessageImages,
// ImagePurposeMask) and sends the part as the multipart `mask` field of
// /v1/images/edits rather than as a second source image; the key and the value here
// are the contract with it. A mask is only ever the mask OF an edit: a message
// without a source image has none (chatDoc.ts userTurnContent never writes one).
//
// Defined here, next to the part shape, because the message renderer needs the
// predicate too and may not reach into components/chat/ (arch.test.ts); chatDoc.ts
// re-exports it for the chat module.
export const MASK_PURPOSE = 'mask' as const;

export function maskPart(url: string): ChatContentPart {
  return { type: 'image_url', image_url: { url }, purpose: MASK_PURPOSE };
}

export function isMaskPart(part: ChatContentPart): boolean {
  return part.type === 'image_url' && part.purpose === MASK_PURPOSE;
}

type ImagePart = Extract<ChatContentPart, { type: 'image_url' }>;

// The urls of the images of a message as the user sees them -- attachments, an edit's
// source, generated results -- in order: every image part but the MASK of an edit,
// which is shown apart under its own name. What a refine hands the composer as the
// source is one of these (a generated result's /chat-blobs/{id} reference), and the
// message renderer lists exactly these.
export function imageUrlsOf(content: ChatContent): string[] {
  if (typeof content === 'string') return [];
  return content
    .filter((part): part is ImagePart => part.type === 'image_url' && !isMaskPart(part))
    .map((part) => part.image_url.url);
}

type AudioPart = Extract<ChatContentPart, { type: 'audio_url' }>;

// The urls of the audio of a message -- a speech turn's synthesized reply, or a
// transcription turn's input clip -- in order: every audio part, whichever role carries
// it. The audio twin of imageUrlsOf above, which never lists one. The message renderer
// plays and offers for download exactly these.
export function audioUrlsOf(content: ChatContent): string[] {
  if (typeof content === 'string') return [];
  return content
    .filter((part): part is AudioPart => part.type === 'audio_url')
    .map((part) => part.audio_url.url);
}
