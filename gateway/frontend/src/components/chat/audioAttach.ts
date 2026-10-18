// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Preparing the audio clip of a transcription turn for the composer: the checks a clip must pass
// before it may be attached (its format, its size) and its encoding as the inline data url the
// turn carries. The image counterpart is ../shared/imageAttach; the format rules live in
// chatDoc.ts (canonicalAudioMediaType), which this reads.

import { audioDataUrl, canonicalAudioMediaType } from './chatDoc';

// What a clip may be refused for: a format the backend does not take ('type'), a clip over the
// per-blob ceiling the backend stores ('size'), nothing recorded or an empty file ('empty'), or a
// file the browser could not read ('read').
export type AudioAttachReason = 'type' | 'size' | 'empty' | 'read';

export class AudioAttachError extends Error {
  readonly reason: AudioAttachReason;
  constructor(reason: AudioAttachReason) {
    super(reason);
    this.name = 'AudioAttachError';
    this.reason = reason;
  }
}

// The decoded size a clip may have: the backend stores up to 20 MiB per blob
// (DefaultMaxChatBlobBytes in portal/service_chats.go -- keep the two in step), and a clip over
// it would be refused when the run commits the turn, after the transcript was already extended.
// The same ceiling as an image's (imageAttach's MAX_IMAGE_BYTES).
export const MAX_AUDIO_BYTES = 20 * 1024 * 1024;

// The clip of the composer: the inline data url the turn will carry (`data:<canonical
// type>;base64,...`, also what the preview plays) and the name it is shown under -- the file's,
// or '' for a recording, which the composer labels itself.
export type AudioClip = { url: string; name: string };

// The bytes of a blob as standard-alphabet base64, which is what a FileReader makes of them: the
// payload of its data url, whatever type the reader put in front (it reports an unknown format as
// application/octet-stream, and the recorder's type with its codec parameter).
function readAsBase64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const { result } = reader;
      const comma = typeof result === 'string' ? result.indexOf(',') : -1;
      if (typeof result === 'string' && comma >= 0) resolve(result.slice(comma + 1));
      else reject(new AudioAttachError('read'));
    };
    reader.onerror = () => reject(new AudioAttachError('read'));
    reader.readAsDataURL(blob);
  });
}

// Vets a picked file or a recording and turns it into the composer's clip. The format is checked
// first (the picker's `accept` is only a hint, which a file dialog's "all files" ignores): the
// clip must be in one of the types the backend's allow-list names, under whatever spelling the
// platform reported (canonicalAudioMediaType maps the aliases and, for a file with no type, the
// extension), and it is sent under that canonical type. Then it must have bytes, and fit the
// size ceiling. `name` is the file's name ('' for a recording); a blob that is not a File has
// none, and its type alone decides.
export async function prepareAudioClip(file: Blob, name = ''): Promise<AudioClip> {
  const mediaType = canonicalAudioMediaType(file.type, name);
  if (mediaType === undefined) throw new AudioAttachError('type');
  if (file.size === 0) throw new AudioAttachError('empty');
  if (file.size > MAX_AUDIO_BYTES) throw new AudioAttachError('size');
  return { url: audioDataUrl(mediaType, await readAsBase64(file)), name };
}
