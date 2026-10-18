// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Preparing the audio clip of a transcription turn for the composer: the checks a clip must pass
// before it may be attached (what it is, its size), its conversion to WAV, and its encoding as the
// inline data url the turn carries. The image counterpart is ../shared/imageAttach; the
// conversion is ../shared/audioToWav and the data url's format is chatDoc.ts (audioDataUrl).

import { AudioDecodeError, WAV_MEDIA_TYPE, audioBlobToWav } from '../shared/audioToWav';
import { audioDataUrl, bareAudioMediaType } from './chatDoc';

// What a clip may be refused for: a file that is no audio ('type'), a clip over the per-blob
// ceiling the backend stores ('size'), nothing recorded or an empty file ('empty'), audio the
// browser cannot decode ('decode'), or a file the browser could not read ('read').
export type AudioAttachReason = 'type' | 'size' | 'empty' | 'decode' | 'read';

export class AudioAttachError extends Error {
  readonly reason: AudioAttachReason;
  constructor(reason: AudioAttachReason) {
    super(reason);
    this.name = 'AudioAttachError';
    this.reason = reason;
  }
}

// The size a clip may have: the backend stores up to 20 MiB per blob
// (DefaultMaxChatBlobBytes in portal/service_chats.go -- keep the two in step), and a clip over
// it would be refused when the run commits the turn, after the transcript was already extended.
// The same ceiling as an image's (imageAttach's MAX_IMAGE_BYTES). It is checked on the upload
// (before it is decoded) and again on the WAV it becomes, which is what is stored: a WAV of
// 16 kHz mono 16 bit is 32 KB a second, so a small compressed upload can come out far larger.
export const MAX_AUDIO_BYTES = 20 * 1024 * 1024;

// The clip of the composer: the inline data url the turn will carry (`data:audio/wav;base64,...`,
// also what the preview plays) and the name it is shown under -- the file's, or '' for a
// recording, which the composer labels itself.
export type AudioClip = { url: string; name: string };

// The bytes of a blob as standard-alphabet base64, which is what a FileReader makes of them: the
// payload of its data url, whatever type the reader put in front (the converted clip is
// audio/wav, but a reader reports an unknown format as application/octet-stream).
function readAsBase64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const { result } = reader;
      // The LAST comma: base64 never contains one, but a media-type parameter in the header
      // (a blob typed `audio/x-foo;a=b,c`) may, and slicing at the first would keep part of it.
      const comma = typeof result === 'string' ? result.lastIndexOf(',') : -1;
      if (typeof result === 'string' && comma >= 0) resolve(result.slice(comma + 1));
      else reject(new AudioAttachError('read'));
    };
    reader.onerror = () => reject(new AudioAttachError('read'));
    reader.readAsDataURL(blob);
  });
}

// Whether a file's reported type leaves it possibly audio: audio/*, the video containers that
// platforms give an audio-only file (Chrome types every .webm video/webm), Firefox's and
// Windows's application/ogg -- and no type at all, or the generic one a file dialog falls back
// to for a format it does not know. The browser's decoder is the judge of what is really audio
// (it reads the bytes, not the type or the name), so this only turns away what is plainly
// something else: an image, a document, text.
function mayBeAudio(mediaType: string): boolean {
  const bare = bareAudioMediaType(mediaType);
  return (
    bare === '' ||
    bare === 'application/octet-stream' ||
    bare === 'application/ogg' ||
    bare.startsWith('audio/') ||
    bare.startsWith('video/')
  );
}

// Converts the upload to the WAV the transcription server reads (audioBlobToWav). Audio the
// browser cannot decode is refused as such ('decode'); any other failure is an unreadable clip.
async function convertToWav(file: Blob): Promise<Blob> {
  try {
    return await audioBlobToWav(file);
  } catch (err) {
    throw new AudioAttachError(err instanceof AudioDecodeError ? 'decode' : 'read');
  }
}

// Vets a picked file or a recording and turns it into the composer's clip. The type is checked
// first (the picker's `accept` is only a hint, which a file dialog's "all files" ignores): it
// must not be plainly something other than audio. Then it must have bytes and fit the size
// ceiling. Then it is converted, in the browser, to a mono 16 kHz WAV -- the one format the
// transcription server takes, whatever the file or the recorder made (webm/opus, mp4/aac, mp3,
// ogg, flac ...) -- and the WAV, which must fit the ceiling too, is what is encoded: the clip is
// always `data:audio/wav`, so what the user hears in the preview is what is stored and sent.
// `name` is the file's name ('' for a recording); a blob that is not a File has none, and its
// type alone decides.
export async function prepareAudioClip(file: Blob, name = ''): Promise<AudioClip> {
  if (!mayBeAudio(file.type)) throw new AudioAttachError('type');
  if (file.size === 0) throw new AudioAttachError('empty');
  if (file.size > MAX_AUDIO_BYTES) throw new AudioAttachError('size');
  const wav = await convertToWav(file);
  if (wav.size > MAX_AUDIO_BYTES) throw new AudioAttachError('size');
  return { url: audioDataUrl(WAV_MEDIA_TYPE, await readAsBase64(wav)), name };
}
