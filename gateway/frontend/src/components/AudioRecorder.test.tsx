// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioRecorder } from './AudioRecorder';
import { messages, type Locale } from '../i18n';

// jsdom has no microphone, no MediaRecorder and no object URLs: this is the browser's recording
// stack, small enough to drive from a test. The fake recorder mirrors the parts of the real one
// the component reads -- `start()` begins, `stop()` flushes one last `dataavailable` and then fires
// `stop` -- and reports the type a Chromium recorder does (webm with an opus codec parameter).
class FakeMediaRecorder {
  static instances: FakeMediaRecorder[] = [];
  // What the next recorder produces on stop(); a test empties it to record nothing.
  static chunks: () => Blob[] = () => [
    new Blob(['voice-bytes'], { type: 'audio/webm;codecs=opus' }),
  ];
  // Set to make the constructor throw (an unsupported configuration).
  static constructError: Error | null = null;

  state: 'inactive' | 'recording' = 'inactive';
  mimeType = 'audio/webm;codecs=opus';
  ondataavailable: ((event: { data: Blob }) => void) | null = null;
  onstop: (() => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  stream: MediaStream;
  startCalls = 0;
  stopCalls = 0;

  constructor(stream: MediaStream) {
    if (FakeMediaRecorder.constructError) throw FakeMediaRecorder.constructError;
    this.stream = stream;
    FakeMediaRecorder.instances.push(this);
  }

  start() {
    this.startCalls += 1;
    this.state = 'recording';
  }

  stop() {
    this.stopCalls += 1;
    this.state = 'inactive';
    for (const data of FakeMediaRecorder.chunks()) this.ondataavailable?.({ data });
    this.onstop?.();
  }
}

// A microphone stream: one track whose stop() is the "microphone released" signal.
function makeStream() {
  const track = { stop: vi.fn() };
  const stream = { getTracks: () => [track] } as unknown as MediaStream;
  return { stream, track };
}

type Mic = ReturnType<typeof makeStream>;
let mic: Mic;
let getUserMedia: ReturnType<typeof vi.fn>;
let createObjectURL: ReturnType<typeof vi.fn>;
let revokeObjectURL: ReturnType<typeof vi.fn>;
let urlSeq = 0;

function installMediaStack() {
  mic = makeStream();
  getUserMedia = vi.fn(async () => mic.stream);
  Object.defineProperty(navigator, 'mediaDevices', {
    value: { getUserMedia },
    configurable: true,
  });
  FakeMediaRecorder.instances = [];
  FakeMediaRecorder.constructError = null;
  FakeMediaRecorder.chunks = () => [new Blob(['voice-bytes'], { type: 'audio/webm;codecs=opus' })];
  vi.stubGlobal('MediaRecorder', FakeMediaRecorder);
  urlSeq = 0;
  createObjectURL = vi.fn(() => `blob:recording-${++urlSeq}`);
  revokeObjectURL = vi.fn();
  Object.defineProperty(URL, 'createObjectURL', { value: createObjectURL, configurable: true });
  Object.defineProperty(URL, 'revokeObjectURL', { value: revokeObjectURL, configurable: true });
}

beforeEach(() => {
  installMediaStack();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, 'mediaDevices');
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderRecorder(props: { disabled?: boolean } = {}) {
    const onRecorded = vi.fn();
    const view = render(<AudioRecorder t={t} onRecorded={onRecorded} {...props} />);
    return { onRecorded, ...view };
  }

  // Clicks Record and lets the permission prompt (the awaited getUserMedia) settle.
  async function startRecording() {
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
    });
  }

  async function stopRecording() {
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStop }));
    });
  }

  describe(`AudioRecorder [${locale}]`, () => {
    it('starts idle: a record button and nothing else', () => {
      renderRecorder();

      expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
      expect(screen.queryByRole('button', { name: t.chatRecordStop })).not.toBeInTheDocument();
      expect(screen.queryByRole('timer')).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatRecordPreview)).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      // No microphone is asked for until the user asks.
      expect(getUserMedia).not.toHaveBeenCalled();
    });

    it('asks for the microphone, audio only, and records', async () => {
      renderRecorder();
      await startRecording();

      expect(getUserMedia).toHaveBeenCalledWith({ audio: true });
      expect(FakeMediaRecorder.instances).toHaveLength(1);
      expect(FakeMediaRecorder.instances[0].startCalls).toBe(1);
      expect(FakeMediaRecorder.instances[0].stream).toBe(mic.stream);
      // The controls follow the state: stop (and a clock) instead of record.
      expect(screen.getByRole('button', { name: t.chatRecordStop })).toBeEnabled();
      expect(screen.queryByRole('button', { name: t.chatRecordStart })).not.toBeInTheDocument();
      expect(screen.getByRole('timer', { name: t.chatRecordElapsed })).toHaveTextContent('0:00');
    });

    it('says it is waiting while the permission prompt is open, and cannot be started twice', async () => {
      let allow: (stream: MediaStream) => void = () => {};
      getUserMedia.mockImplementationOnce(
        () => new Promise<MediaStream>((resolve) => (allow = resolve)),
      );
      renderRecorder();
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));

      expect(await screen.findByText(t.chatRecordRequesting)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeDisabled();
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
      expect(getUserMedia).toHaveBeenCalledTimes(1);

      await act(async () => allow(mic.stream));
      expect(screen.queryByText(t.chatRecordRequesting)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatRecordStop })).toBeInTheDocument();
    });

    it('shows an elapsed clock that follows the recording', async () => {
      vi.useFakeTimers();
      renderRecorder();
      await startRecording();

      await act(async () => {
        vi.advanceTimersByTime(3000);
      });
      expect(screen.getByRole('timer')).toHaveTextContent('0:03');

      await act(async () => {
        vi.advanceTimersByTime(62_000);
      });
      expect(screen.getByRole('timer')).toHaveTextContent('1:05');
    });

    it('stops, releases the microphone and offers a preview of the recording', async () => {
      renderRecorder();
      await startRecording();
      await stopRecording();

      expect(FakeMediaRecorder.instances[0].stopCalls).toBe(1);
      // The browser's "recording" indicator goes out with the track.
      expect(mic.track.stop).toHaveBeenCalled();
      // The clock is gone, a player is there and plays an object URL of the recording.
      expect(screen.queryByRole('timer')).not.toBeInTheDocument();
      const preview = screen.getByLabelText(t.chatRecordPreview);
      expect(preview.tagName).toBe('AUDIO');
      expect(preview).toHaveAttribute('src', 'blob:recording-1');
      expect(preview).toHaveAttribute('controls');
      const made = createObjectURL.mock.calls[0][0] as Blob;
      expect(made.type).toBe('audio/webm;codecs=opus');
      expect(screen.getByRole('button', { name: t.chatRecordUse })).toBeEnabled();
      expect(screen.getByRole('button', { name: t.chatRecordDiscard })).toBeEnabled();
      // Nothing is attached until the user says so.
    });

    it('offers the recording as an audio/webm blob when it is used, and revokes the preview', async () => {
      const { onRecorded } = renderRecorder();
      await startRecording();
      await stopRecording();
      expect(onRecorded).not.toHaveBeenCalled();

      fireEvent.click(screen.getByRole('button', { name: t.chatRecordUse }));

      expect(onRecorded).toHaveBeenCalledTimes(1);
      const blob = onRecorded.mock.calls[0][0] as Blob;
      expect(blob).toBeInstanceOf(Blob);
      // The recorder's own type, parameter and all: normalizing it to the bare canonical
      // type is the attach's job (it needs to for any clip), not the recorder's.
      expect(blob.type).toBe('audio/webm;codecs=opus');
      expect(blob.size).toBeGreaterThan(0);
      expect(revokeObjectURL).toHaveBeenCalledWith('blob:recording-1');
      // Back to idle, ready for another take.
      expect(screen.queryByLabelText(t.chatRecordPreview)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
    });

    it('collects every chunk the recorder delivers into one blob', async () => {
      FakeMediaRecorder.chunks = () => [
        new Blob(['ab'], { type: 'audio/webm;codecs=opus' }),
        new Blob(['cdef'], { type: 'audio/webm;codecs=opus' }),
      ];
      const { onRecorded } = renderRecorder();
      await startRecording();
      await stopRecording();
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordUse }));

      expect((onRecorded.mock.calls[0][0] as Blob).size).toBe(6);
    });

    it('discards a recording: nothing is offered, the preview is revoked and it can start over', async () => {
      const { onRecorded } = renderRecorder();
      await startRecording();
      await stopRecording();

      fireEvent.click(screen.getByRole('button', { name: t.chatRecordDiscard }));

      expect(onRecorded).not.toHaveBeenCalled();
      expect(revokeObjectURL).toHaveBeenCalledWith('blob:recording-1');
      expect(screen.queryByLabelText(t.chatRecordPreview)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();

      // The next take is a fresh one, with its own preview url.
      await startRecording();
      await stopRecording();
      expect(screen.getByLabelText(t.chatRecordPreview)).toHaveAttribute('src', 'blob:recording-2');
      expect(FakeMediaRecorder.instances).toHaveLength(2);
    });

    describe('when the microphone cannot be used', () => {
      it.each([
        ['NotAllowedError', t.chatRecordDenied],
        ['SecurityError', t.chatRecordDenied],
        ['NotFoundError', t.chatRecordFailed],
        ['NotReadableError', t.chatRecordFailed],
      ])('says so for %s, without crashing, and allows another try', async (name, message) => {
        getUserMedia.mockRejectedValueOnce(new DOMException('no mic', name));
        const { onRecorded } = renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(message);
        expect(FakeMediaRecorder.instances).toHaveLength(0);
        expect(onRecorded).not.toHaveBeenCalled();
        // Idle again: a user who fixes the permission can click once more...
        const record = screen.getByRole('button', { name: t.chatRecordStart });
        expect(record).toBeEnabled();

        // ...and the stale error goes the moment they do.
        await act(async () => {
          fireEvent.click(record);
        });
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(screen.getByRole('button', { name: t.chatRecordStop })).toBeInTheDocument();
      });

      it('copes with a rejection that is not a DOMException', async () => {
        getUserMedia.mockRejectedValueOnce('boom');
        renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordFailed);
      });

      it('says the browser cannot record when there is no mediaDevices (an insecure page)', async () => {
        Reflect.deleteProperty(navigator, 'mediaDevices');
        renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordUnsupported);
        expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
      });

      it('says the browser cannot record when there is no getUserMedia', async () => {
        Object.defineProperty(navigator, 'mediaDevices', { value: {}, configurable: true });
        renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordUnsupported);
      });

      it('says the browser cannot record when there is no MediaRecorder, and never asks for the microphone', async () => {
        vi.stubGlobal('MediaRecorder', undefined);
        renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordUnsupported);
        expect(getUserMedia).not.toHaveBeenCalled();
      });

      it('releases the microphone again when the recorder cannot be built', async () => {
        FakeMediaRecorder.constructError = new Error('unsupported');
        renderRecorder();
        await startRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordFailed);
        expect(mic.track.stop).toHaveBeenCalled();
        expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
      });

      it('fails a recording that captured nothing, and releases everything', async () => {
        FakeMediaRecorder.chunks = () => [];
        const { onRecorded } = renderRecorder();
        await startRecording();
        await stopRecording();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordFailed);
        expect(onRecorded).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
        expect(mic.track.stop).toHaveBeenCalled();
        expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
      });

      it('ends a recording the recorder itself reports as failed', async () => {
        const { onRecorded } = renderRecorder();
        await startRecording();

        act(() => {
          FakeMediaRecorder.instances[0].onerror?.({});
        });

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordFailed);
        expect(mic.track.stop).toHaveBeenCalled();
        expect(screen.queryByRole('timer')).not.toBeInTheDocument();
        expect(onRecorded).not.toHaveBeenCalled();
      });
    });

    describe('when the recorder goes away', () => {
      it('stops a recording in progress and releases the microphone, offering nothing', async () => {
        const { onRecorded, unmount } = renderRecorder();
        await startRecording();

        unmount();

        expect(mic.track.stop).toHaveBeenCalled();
        expect(onRecorded).not.toHaveBeenCalled();
        // The recorder's late `stop` event finds nobody to tell and makes no preview.
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('stops the clock with it', async () => {
        vi.useFakeTimers();
        const { unmount } = renderRecorder();
        await startRecording();
        unmount();

        // A still-ticking interval would set state on an unmounted component; advancing time
        // after unmount must be a no-op.
        expect(() => vi.advanceTimersByTime(5000)).not.toThrow();
        expect(vi.getTimerCount()).toBe(0);
      });

      it('revokes the preview that was never used', async () => {
        const { unmount } = renderRecorder();
        await startRecording();
        await stopRecording();
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledWith('blob:recording-1');
      });

      it('releases a microphone that was granted after it left', async () => {
        let allow: (stream: MediaStream) => void = () => {};
        getUserMedia.mockImplementationOnce(
          () => new Promise<MediaStream>((resolve) => (allow = resolve)),
        );
        const { unmount } = renderRecorder();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
        unmount();

        await act(async () => allow(mic.stream));

        // Nobody is there to record: the late grant is let go at once, not held open.
        expect(mic.track.stop).toHaveBeenCalled();
        expect(FakeMediaRecorder.instances).toHaveLength(0);
      });
    });

    it('cannot start while disabled (a run is in flight)', () => {
      renderRecorder({ disabled: true });

      expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeDisabled();
    });
  });
}
