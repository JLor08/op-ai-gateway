// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Typography } from '@mui/material';
import { ChatMessage } from './ChatMessage';
import { messages, type Locale } from '../i18n';

// Captured once, before any test can have spied on it -- vitest's retry: 2
// (vite.config.ts) reruns a failing test's body (including a fresh
// vi.spyOn(document, 'createElement')) without an intervening restore unless
// afterEach does it. Re-reading document.createElement INSIDE a test body
// would then capture the previous attempt's still-installed spy instead of
// the native function, and re-wrapping that spy's own mockImplementation to
// call itself is a direct infinite recursion (seen as a real failure while
// writing this file's anchor-attribute tests). Capturing the native function
// here, once, and restoring mocks in afterEach, avoids both.
const nativeCreateElement = document.createElement.bind(document);

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  // The stored-image download tests stub fetch (and, like the data: URL ones,
  // URL); neither may leak into the next test.
  vi.unstubAllGlobals();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`ChatMessage [${locale}]`, () => {
    it('renders assistant markdown as HTML', () => {
      render(<ChatMessage t={t} role="assistant" content={'**bold** and `code`'} />);
      expect(screen.getByText('bold').tagName).toBe('STRONG');
      expect(screen.getByText('code').tagName).toBe('CODE');
    });

    it('shows a reasoning block when reasoning is present', () => {
      render(<ChatMessage t={t} role="assistant" content="answer" reasoning="thinking about it" />);
      expect(screen.getByText(/thinking about it/)).toBeInTheDocument();
    });

    it('renders attached images for a user message', () => {
      render(
        <ChatMessage
          t={t}
          role="user"
          content={[
            { type: 'text', text: 'look' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
          ]}
        />,
      );
      expect(screen.getByText('look')).toBeInTheDocument();
      const images = screen.getAllByRole('img');
      expect(images).toHaveLength(1);
      expect(images[0]).toHaveAttribute('src', 'data:image/png;base64,AAAA');
    });

    it('shows the mask of a masked edit apart from the attached image, under its own name', () => {
      render(
        <ChatMessage
          t={t}
          role="user"
          content={[
            { type: 'text', text: 'fill the sky' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,SRC' } },
            {
              type: 'image_url',
              image_url: { url: 'data:image/png;base64,MASK' },
              purpose: 'mask',
            },
          ]}
        />,
      );
      const attached = screen.getAllByAltText(t.chatAttachedImage);
      expect(attached).toHaveLength(1);
      expect(attached[0]).toHaveAttribute('src', 'data:image/png;base64,SRC');
      const masks = screen.getAllByAltText(t.chatAttachedMask);
      expect(masks).toHaveLength(1);
      expect(masks[0]).toHaveAttribute('src', 'data:image/png;base64,MASK');
    });

    it('saves an edited user message', () => {
      const onEdit = vi.fn();
      render(<ChatMessage t={t} role="user" content="original" onEdit={onEdit} />);
      fireEvent.click(screen.getByRole('button', { name: t.chatEdit }));
      const textarea = screen.getByLabelText(t.messageLabel);
      fireEvent.change(textarea, { target: { value: 'edited text' } });
      fireEvent.click(screen.getByRole('button', { name: t.chatSave }));
      expect(onEdit).toHaveBeenCalledWith('edited text');
    });

    it('restores the original text on cancel without calling onEdit', () => {
      const onEdit = vi.fn();
      render(<ChatMessage t={t} role="user" content="original" onEdit={onEdit} />);
      fireEvent.click(screen.getByRole('button', { name: t.chatEdit }));
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'changed' } });
      fireEvent.click(screen.getByRole('button', { name: t.chatCancel }));
      expect(screen.getByText('original')).toBeInTheDocument();
      expect(onEdit).not.toHaveBeenCalled();
    });

    it('fires onRegenerate when the regenerate button is clicked', () => {
      const onRegenerate = vi.fn();
      render(<ChatMessage t={t} role="assistant" content="answer" onRegenerate={onRegenerate} />);
      fireEvent.click(screen.getByRole('button', { name: t.chatRegenerate }));
      expect(onRegenerate).toHaveBeenCalledTimes(1);
    });

    it('shows the active reasoning summary and a cursor while streaming with no answer yet', () => {
      render(<ChatMessage t={t} role="assistant" content="" streaming reasoning="still working" />);
      expect(screen.getByText((c) => c.startsWith(t.chatReasoningActive))).toBeInTheDocument();
      expect(screen.getByTestId('chat-stream-cursor')).toBeInTheDocument();
    });

    it('shows the image wait label, a clock and the no-news sentence while an image run is pending', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          content=""
          streaming
          kind="image"
          elapsedMs={107_000}
        />,
      );
      expect(screen.getByText(t.chatImageRunPending)).toBeInTheDocument();
      expect(screen.getByText('1:47')).toBeInTheDocument();
      expect(screen.getByText(t.chatImageNoIntermediateNews)).toBeInTheDocument();
    });

    it('shows NO character counter for a pending image run', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={0} />,
      );
      // The counter is honest for text -- the number is real and it moves. For
      // an image run there is nothing to count, so it must be ABSENT, not zero:
      // proxyNative makes the same distinction one layer down when it gives a
      // buffered relay progress = nil rather than an always-zero struct.
      expect(screen.queryByText(new RegExp(t.chatCharsUnit))).toBeNull();
      expect(screen.queryByText(new RegExp(t.chatReasoningActive))).toBeNull();
    });

    it('hides the clock from assistive technology, but not the label or sentence', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={5_000} />,
      );
      // The transcript is an aria-live log; a ticking number would be announced
      // once a second and make the thread unusable with a screen reader.
      expect(screen.getByText('5s').closest('[aria-hidden="true"]')).not.toBeNull();
      // The POSITIVE half: a mutation that hides the whole bubble (or the
      // label specifically) would also pass the assertion above, and is the
      // failure mode that makes this state useless to a screen-reader user
      // -- the label and sentence must still be announced.
      expect(screen.getByText(t.chatImageRunPending).closest('[aria-hidden="true"]')).toBeNull();
    });

    it('ticks the clock once a second from the server anchor', async () => {
      vi.useFakeTimers();
      try {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="image"
            elapsedMs={3_000}
          />,
        );
        expect(screen.getByText('3s')).toBeInTheDocument();
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2_000);
        });
        expect(screen.getByText('5s')).toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    });

    it('leaves the text pending state exactly as it was', () => {
      render(<ChatMessage t={t} role="assistant" content="" streaming reasoning="thinking" />);
      // No kind prop: the existing summary must be untouched byte-for-byte,
      // because this is the overwhelmingly common path. Asserted as the
      // exact rendered string, not a regex built from t.chatReasoningActive
      // -- that string's literal "..." is itself a regex wildcard matching
      // ANY three characters, so /Denkt .../ (or /Thinking .../) would still
      // match a changed separator or trailing text, and a second, separate
      // regex for chatCharsUnit would still pass even if the character
      // count itself were deleted from between them.
      expect(
        screen.getByText(`${t.chatReasoningActive} · ${'thinking'.length} ${t.chatCharsUnit}`),
      ).toBeInTheDocument();
    });

    it('does not show the image pending state once an image has arrived, even while still streaming', () => {
      // images.length === 0 is the guard that keeps ImagePendingTurn from
      // replacing an arriving image in the same render.
      render(
        <ChatMessage
          t={t}
          role="assistant"
          content={[{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }]}
          streaming
          kind="image"
          elapsedMs={12_000}
        />,
      );
      expect(screen.queryByText(t.chatImageRunPending)).toBeNull();
      expect(screen.getAllByRole('img')).toHaveLength(1);
    });

    // A speech (text-to-speech) run is as silent as an image run -- the endpoint
    // answers once, with the finished WAV -- so it gets the same non-streaming wait,
    // worded for audio.
    describe('a speech run', () => {
      it('shows the audio wait label, a clock and the no-news sentence while pending', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            elapsedMs={107_000}
          />,
        );
        expect(screen.getByText(t.chatAudioRunPending)).toBeInTheDocument();
        expect(screen.getByText('1:47')).toBeInTheDocument();
        expect(screen.getByText(t.chatAudioNoIntermediateNews)).toBeInTheDocument();
        // Not the image copy, and not the text-pending state either.
        expect(screen.queryByText(t.chatImageRunPending)).toBeNull();
        expect(screen.queryByText(new RegExp(t.chatCharsUnit))).toBeNull();
        expect(screen.queryByText(new RegExp(t.chatReasoningActive))).toBeNull();
        expect(screen.queryByTestId('chat-stream-cursor')).toBeNull();
      });

      it('hides the ticking clock from assistive technology, but not the label or sentence', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            elapsedMs={5_000}
          />,
        );
        expect(screen.getByText('5s').closest('[aria-hidden="true"]')).not.toBeNull();
        expect(screen.getByText(t.chatAudioRunPending).closest('[aria-hidden="true"]')).toBeNull();
        expect(
          screen.getByText(t.chatAudioNoIntermediateNews).closest('[aria-hidden="true"]'),
        ).toBeNull();
      });

      it('ticks the clock once a second from the server anchor', async () => {
        vi.useFakeTimers();
        try {
          render(
            <ChatMessage
              t={t}
              role="assistant"
              content=""
              streaming
              kind="speech"
              elapsedMs={3_000}
            />,
          );
          expect(screen.getByText('3s')).toBeInTheDocument();
          await act(async () => {
            await vi.advanceTimersByTimeAsync(2_000);
          });
          expect(screen.getByText('5s')).toBeInTheDocument();
        } finally {
          vi.useRealTimers();
        }
      });

      // A streaming model's audio is played AS IT IS GENERATED (ADR-068), so the wait is no
      // longer silent: "no intermediate news" would be false while the user is listening. The
      // pending turn says it is playing live, and that the finished audio follows.
      it('says the audio is playing live, not that no news is coming, while a streamed run plays', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            audioLive
            elapsedMs={4_000}
          />,
        );
        expect(screen.getByText(t.chatAudioStreamLive)).toBeInTheDocument();
        expect(screen.getByText(t.chatAudioStreamLiveHint)).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
        expect(screen.queryByText(t.chatAudioNoIntermediateNews)).toBeNull();
        // The clock still ticks, hidden from assistive technology like the other pending turns.
        expect(screen.getByText('4s').closest('[aria-hidden="true"]')).not.toBeNull();
        expect(screen.getByText(t.chatAudioStreamLive).closest('[aria-hidden="true"]')).toBeNull();
      });

      // The live controls (#281) ride the same row as the live wording: the store's mute state
      // and its two handlers reach the bubble untouched.
      it('threads the live mute state and handlers to the speech bubble', () => {
        const onMuteAudio = vi.fn();
        const onAbortAudio = vi.fn();
        const { rerender } = render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            audioLive
            audioMuted={false}
            onMuteAudio={onMuteAudio}
            onAbortAudio={onAbortAudio}
            elapsedMs={4_000}
          />,
        );
        fireEvent.click(screen.getByRole('button', { name: t.chatAudioMute }));
        expect(onMuteAudio).toHaveBeenCalledWith(true);
        fireEvent.click(screen.getByRole('button', { name: t.chatAudioAbort }));
        expect(onAbortAudio).toHaveBeenCalledTimes(1);

        rerender(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            audioLive
            audioMuted
            onMuteAudio={onMuteAudio}
            onAbortAudio={onAbortAudio}
            elapsedMs={4_000}
          />,
        );
        expect(screen.getByRole('button', { name: t.chatAudioUnmute })).toBeInTheDocument();
      });

      it('shows no live controls for a speech run that is not playing live', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            audioLive={false}
            onMuteAudio={vi.fn()}
            onAbortAudio={vi.fn()}
            elapsedMs={4_000}
          />,
        );
        expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
        expect(screen.queryByRole('button', { name: t.chatAudioAbort })).toBeNull();
      });

      it('keeps the silent wait for a speech run that is not playing live', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            audioLive={false}
            elapsedMs={4_000}
          />,
        );
        expect(screen.getByText(t.chatAudioRunPending)).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioStreamLive)).toBeNull();
      });

      // The draining tail (#281): once the run has committed (streaming false) its saved player
      // appears, but the streamed tail may still be playing out. While it is (audioLive), the
      // Mute/Stop controls ride alongside AudioTurn so the user can still silence it.
      describe('the controls while the tail plays out alongside the saved player', () => {
        const stored = [
          { type: 'audio_url' as const, audio_url: { url: 'data:audio/wav;base64,AAAA' } },
        ];

        it('shows Mute and Stop next to the saved player while the tail plays out', () => {
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={stored}
              audioLive
              audioMuted={false}
              onMuteAudio={vi.fn()}
              onAbortAudio={vi.fn()}
            />,
          );
          // The saved player is there AND the live controls are beside it. The tail-window Stop
          // reads "Stop playback" (it only ends playback), not "Abort generation".
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
          expect(screen.getByRole('button', { name: t.chatAudioMute })).toBeInTheDocument();
          expect(screen.getByRole('button', { name: t.chatAudioStopPlayback })).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatAudioAbort })).toBeNull();
        });

        it('calls the handlers: Mute toggles, Stop silences the tail', () => {
          const onMuteAudio = vi.fn();
          const onAbortAudio = vi.fn();
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={stored}
              audioLive
              audioMuted={false}
              onMuteAudio={onMuteAudio}
              onAbortAudio={onAbortAudio}
            />,
          );
          fireEvent.click(screen.getByRole('button', { name: t.chatAudioMute }));
          expect(onMuteAudio).toHaveBeenCalledWith(true);
          fireEvent.click(screen.getByRole('button', { name: t.chatAudioStopPlayback }));
          expect(onAbortAudio).toHaveBeenCalledTimes(1);
        });

        it('shows Unmute while the tail is muted', () => {
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={stored}
              audioLive
              audioMuted
              onMuteAudio={vi.fn()}
              onAbortAudio={vi.fn()}
            />,
          );
          expect(screen.getByRole('button', { name: t.chatAudioUnmute })).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
        });

        it('drops the controls once the tail has drained (audioLive false)', () => {
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={stored}
              audioLive={false}
              onMuteAudio={vi.fn()}
              onAbortAudio={vi.fn()}
            />,
          );
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
          expect(screen.queryByRole('button', { name: t.chatAudioStopPlayback })).toBeNull();
        });

        it('shows no tail controls for a completed buffered answer (never played out live)', () => {
          // A buffered answer is never live: no audioLive prop reaches it, so no controls, just
          // the saved player.
          render(<ChatMessage t={t} role="assistant" turnId="msg_a1" content={stored} />);
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
          expect(screen.queryByRole('button', { name: t.chatAudioStopPlayback })).toBeNull();
        });

        // Finding 2: a streamed retention-0 answer commits the `audio_removed` marker and NO
        // audio_url parts, so AudioNotStoredTurn renders -- but its streamed tail is still playing
        // out, so the controls must ride beside the placeholder too, not only beside a saved player.
        it('shows the tail controls beside the not-stored placeholder of a retention-0 answer', () => {
          const removed = [{ type: 'audio_removed' as const }];
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={removed}
              audioLive
              audioMuted={false}
              onMuteAudio={vi.fn()}
              onAbortAudio={vi.fn()}
            />,
          );
          // The not-stored placeholder is shown (no player), AND the tail controls are beside it.
          expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
          expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
          expect(screen.getByRole('button', { name: t.chatAudioMute })).toBeInTheDocument();
          expect(screen.getByRole('button', { name: t.chatAudioStopPlayback })).toBeInTheDocument();
        });

        it('drops the placeholder tail controls once the retention-0 tail has drained', () => {
          const removed = [{ type: 'audio_removed' as const }];
          render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_a1"
              content={removed}
              audioLive={false}
              onMuteAudio={vi.fn()}
              onAbortAudio={vi.fn()}
            />,
          );
          expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
          expect(screen.queryByRole('button', { name: t.chatAudioStopPlayback })).toBeNull();
        });
      });

      it('does not show the pending state once the audio has arrived, even while still streaming', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } }]}
            streaming
            kind="speech"
            elapsedMs={12_000}
          />,
        );
        expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
        expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
      });

      it('shows no pending state when the run is not streaming (a failed or finished turn)', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content="the run failed"
            kind="speech"
            elapsedMs={1_000}
          />,
        );
        expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
        expect(screen.getByText('the run failed')).toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
      });

      it('renders a finished turn as an <audio controls> player and a download button', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            turnId="msg_a1b2"
            content={[{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } }]}
          />,
        );
        const audio = screen.getByLabelText(t.chatGeneratedAudio);
        expect(audio.tagName).toBe('AUDIO');
        expect(audio).toHaveAttribute('controls');
        expect(audio).toHaveAttribute('src', 'data:audio/wav;base64,AAAA');
        expect(screen.getByRole('button', { name: t.chatDownloadAudio })).toBeInTheDocument();
        // An audio part is not an image: no thumbnail, no image download.
        expect(screen.queryAllByRole('img')).toHaveLength(0);
        expect(screen.queryByRole('button', { name: t.chatDownloadImage })).toBeNull();
      });

      // Review Focus 3, end to end through the message: the stored reply plays from an
      // object URL, and that URL is released when the turn leaves the transcript.
      it('plays a stored reply from a fetched object URL and revokes it on unmount', async () => {
        const createObjectURL = vi.fn(() => 'blob:speech-reply');
        const revokeObjectURL = vi.fn();
        vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
        const fetchMock = vi.fn(async () => ({
          ok: true,
          status: 200,
          blob: async () => new Blob([new Uint8Array([1, 2, 3])], { type: 'audio/wav' }),
        }));
        vi.stubGlobal('fetch', fetchMock);

        const { unmount } = render(
          <ChatMessage
            t={t}
            role="assistant"
            turnId="msg_a1b2"
            content={[
              {
                type: 'audio_url',
                audio_url: { url: '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10' },
              },
            ]}
          />,
        );

        await waitFor(() =>
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toHaveAttribute(
            'src',
            'blob:speech-reply',
          ),
        );
        expect(fetchMock).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledWith('blob:speech-reply');
      });

      it('does not re-fetch the audio when the message re-renders with the same content', async () => {
        vi.stubGlobal('URL', {
          ...URL,
          createObjectURL: vi.fn(() => 'blob:speech-reply'),
          revokeObjectURL: vi.fn(),
        });
        const fetchMock = vi.fn(async () => ({
          ok: true,
          status: 200,
          blob: async () => new Blob([new Uint8Array([1])], { type: 'audio/wav' }),
        }));
        vi.stubGlobal('fetch', fetchMock);
        const content = [
          {
            type: 'audio_url' as const,
            audio_url: { url: '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10' },
          },
        ];

        const { rerender } = render(
          <ChatMessage t={t} role="assistant" turnId="msg_a1b2" content={content} />,
        );
        await waitFor(() =>
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toHaveAttribute('src'),
        );
        rerender(
          <ChatMessage
            t={t}
            role="assistant"
            turnId="msg_a1b2"
            content={[...content]}
            ttftMs={500}
          />,
        );

        expect(fetchMock).toHaveBeenCalledTimes(1);
      });

      // A long answer is saved as several ordered parts. The assistant turn plays them as ONE
      // clip: one player and one download, not a player per part (and not only the first).
      describe('a reply saved as several parts', () => {
        const PART_URLS = [
          '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10',
          '/api/portal/chat-blobs/0a9d3f3e-2b0c-4f55-8f55-6d6f1c5a7a11',
          '/api/portal/chat-blobs/3c7e9d10-5a41-4b8e-9f27-1d2c3b4a5e6f',
        ];
        const partsOf = (urls: string[]) =>
          urls.map((url) => ({ type: 'audio_url' as const, audio_url: { url } }));

        // A canonical mono 16-bit 16 kHz WAV of `samples` zero samples.
        function silentWav(samples: number): Uint8Array<ArrayBuffer> {
          const out = new Uint8Array(44 + samples * 2);
          const view = new DataView(out.buffer);
          const ascii = (offset: number, text: string) =>
            text.split('').forEach((c, i) => view.setUint8(offset + i, c.charCodeAt(0)));
          ascii(0, 'RIFF');
          view.setUint32(4, 36 + samples * 2, true);
          ascii(8, 'WAVE');
          ascii(12, 'fmt ');
          view.setUint32(16, 16, true);
          view.setUint16(20, 1, true);
          view.setUint16(22, 1, true);
          view.setUint32(24, 16000, true);
          view.setUint32(28, 32000, true);
          view.setUint16(32, 2, true);
          view.setUint16(34, 16, true);
          ascii(36, 'data');
          view.setUint32(40, samples * 2, true);
          return out;
        }

        it('renders exactly ONE player and one download button for three parts', async () => {
          const createObjectURL = vi.fn(() => 'blob:whole-reply');
          vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });
          const fetchMock = vi.fn(async () => ({
            ok: true,
            status: 200,
            blob: async () => new Blob([silentWav(4)], { type: 'audio/wav' }),
          }));
          vi.stubGlobal('fetch', fetchMock);

          const { container } = render(
            <ChatMessage t={t} role="assistant" turnId="msg_s1" content={partsOf(PART_URLS)} />,
          );

          await waitFor(() =>
            expect(screen.getByLabelText(t.chatGeneratedAudio)).toHaveAttribute(
              'src',
              'blob:whole-reply',
            ),
          );
          expect(container.querySelectorAll('audio')).toHaveLength(1);
          expect(screen.getAllByRole('button', { name: t.chatDownloadAudio })).toHaveLength(1);
          // Every part was fetched, and they became ONE object URL.
          expect(fetchMock).toHaveBeenCalledTimes(3);
          expect(createObjectURL).toHaveBeenCalledTimes(1);
        });

        it('renders ONE player for a one-part reply, as before', () => {
          const { container } = render(
            <ChatMessage
              t={t}
              role="assistant"
              turnId="msg_s1"
              content={partsOf(['data:audio/wav;base64,AAAA'])}
            />,
          );

          expect(container.querySelectorAll('audio')).toHaveLength(1);
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toHaveAttribute(
            'src',
            'data:audio/wav;base64,AAAA',
          );
        });

        it('is not the pending state once the first part has arrived, even while streaming', () => {
          vi.stubGlobal(
            'fetch',
            vi.fn(() => new Promise(() => {})),
          );

          render(
            <ChatMessage
              t={t}
              role="assistant"
              content={partsOf(PART_URLS)}
              kind="speech"
              streaming
              elapsedMs={1_000}
            />,
          );

          expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
          expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
        });
      });
    });

    // An answer whose audio is gone (pruned by the retention knob, deleted, never stored, or
    // aborted) carries the `audio_removed` marker (#281) instead of an audio part. A TERMINAL
    // turn with the marker is the not-stored bubble: one placeholder line and a regenerate
    // button where the player was, with whatever text the answer holds left in place.
    describe('a speech answer whose audio is not stored', () => {
      const MARKER = { type: 'audio_removed' as const };
      const BLOB = '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10';

      it('renders the placeholder with no player and no download', () => {
        const { container } = render(
          <ChatMessage t={t} role="assistant" content={[MARKER]} turnId="a1" />,
        );

        expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
        expect(container.querySelector('audio')).toBeNull();
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
        expect(screen.queryByRole('button', { name: t.chatDownloadAudio })).toBeNull();
      });

      it('is not the wait of a pending run: no clock, no no-news sentence', () => {
        render(<ChatMessage t={t} role="assistant" content={[MARKER]} kind="speech" />);

        expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
        expect(screen.queryByText(t.chatAudioNoIntermediateNews)).toBeNull();
      });

      // Review focus: the marker answer is a real transcript message, so its regenerate is the
      // row's own onRegenerate (Chat.tsx binds the message id; Chat.test.tsx pins that
      // end to end) -- called, once, from the button INSIDE the placeholder.
      it('regenerates through the row onRegenerate from the placeholder button', () => {
        const onRegenerate = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[MARKER]}
            turnId="a1"
            onRegenerate={onRegenerate}
          />,
        );

        const placeholder = screen.getByRole('group', { name: t.chatAudioNotStored });
        fireEvent.click(within(placeholder).getByRole('button', { name: t.chatRegenerate }));

        expect(onRegenerate).toHaveBeenCalledTimes(1);
      });

      it('offers ONE regenerate, the placeholder one, not a second in the action stack', () => {
        render(<ChatMessage t={t} role="assistant" content={[MARKER]} onRegenerate={() => {}} />);

        const placeholder = screen.getByRole('group', { name: t.chatAudioNotStored });
        expect(screen.getAllByRole('button', { name: t.chatRegenerate })).toHaveLength(1);
        expect(within(placeholder).getAllByRole('button', { name: t.chatRegenerate })).toHaveLength(
          1,
        );
      });

      it('keeps the regenerate of the action stack for an answer that still holds audio', () => {
        const onRegenerate = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } }]}
            onRegenerate={onRegenerate}
          />,
        );

        fireEvent.click(screen.getByRole('button', { name: t.chatRegenerate }));
        expect(onRegenerate).toHaveBeenCalledTimes(1);
        expect(screen.queryByRole('group', { name: t.chatAudioNotStored })).toBeNull();
      });

      it('offers no regenerate where the row has none to give', () => {
        render(<ChatMessage t={t} role="assistant" content={[MARKER]} />);

        expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
        expect(screen.queryByRole('button', { name: t.chatRegenerate })).toBeNull();
      });

      it('disables regenerate while no run may start, like the action stack one', () => {
        const onRegenerate = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[MARKER]}
            onRegenerate={onRegenerate}
            canRun={false}
          />,
        );

        const placeholder = screen.getByRole('group', { name: t.chatAudioNotStored });
        const button = within(placeholder).getByRole('button', { name: t.chatRegenerate });
        expect(button).toBeDisabled();
        fireEvent.click(button);
        expect(onRegenerate).not.toHaveBeenCalled();
      });

      it('keeps the text of a mixed answer and puts the placeholder where the player was', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'text', text: 'Hello there' }, MARKER]}
            onRegenerate={() => {}}
          />,
        );

        const text = screen.getByText('Hello there');
        const placeholder = screen.getByText(t.chatAudioNotStored);
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
        // The text comes first, the placeholder after it: the player's slot.
        expect(
          text.compareDocumentPosition(placeholder) & Node.DOCUMENT_POSITION_FOLLOWING,
        ).toBeTruthy();
      });

      it('shows no placeholder for a turn that still holds audio, and the player plays it', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } }]}
          />,
        );

        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
        expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
      });

      it('plays audio that is still stored even beside a stray marker', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[MARKER, { type: 'audio_url', audio_url: { url: BLOB } }]}
          />,
        );

        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
        expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
      });

      it('is still the pending wait while the run streams and nothing has arrived', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="speech"
            elapsedMs={5_000}
            onRegenerate={() => {}}
          />,
        );

        expect(screen.getByText(t.chatAudioRunPending)).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
      });

      it('is not shown for the row of a run that is still streaming', () => {
        render(<ChatMessage t={t} role="assistant" content={[MARKER]} streaming kind="speech" />);

        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
      });

      it('is not what a genuinely failed turn shows: its error stays', () => {
        render(
          <ChatMessage t={t} role="assistant" content="the run failed" kind="speech" turnId="a1" />,
        );

        expect(screen.getByText('the run failed')).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
      });

      it('is never a user turn: a marker on a user message renders nothing of it', () => {
        render(<ChatMessage t={t} role="user" content={[{ type: 'text', text: 'hi' }, MARKER]} />);

        expect(screen.getByText('hi')).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioNotStored)).toBeNull();
      });
    });

    // The delete of a stored speech answer's audio (#281): a trash icon on the answer's action
    // stack that asks INLINE first (a confirm in the stack itself, no dialog), because the
    // audio is gone for good once the gateway has removed it -- regenerate is the way back.
    // The row's `onDeleteAudio` is the store's per-message handler (the message id is bound by
    // Chat.tsx's handlersFor; Chat.test.tsx pins that end to end), so it takes no argument.
    describe('deleting the audio of a stored speech answer', () => {
      const AUDIO = {
        type: 'audio_url' as const,
        audio_url: { url: 'data:audio/wav;base64,AAAA' },
      };
      const MARKER = { type: 'audio_removed' as const };
      const trash = () => screen.queryByRole('button', { name: t.chatAudioDelete });
      const confirmButton = () => screen.queryByRole('button', { name: t.chatAudioDeleteAction });
      const cancelButton = () => screen.queryByRole('button', { name: t.chatCancel });

      it('offers a delete (trash) button on the action stack, asking nothing yet', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[AUDIO]}
            turnId="a1"
            onDeleteAudio={onDeleteAudio}
          />,
        );

        expect(trash()).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioDeleteConfirm)).toBeNull();
        expect(confirmButton()).toBeNull();
        expect(cancelButton()).toBeNull();
        expect(onDeleteAudio).not.toHaveBeenCalled();
      });

      it('asks inline, in place of the icon, when it is clicked: Delete and Cancel, no dialog', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );

        fireEvent.click(trash()!);

        expect(screen.getByText(t.chatAudioDeleteConfirm)).toBeInTheDocument();
        expect(confirmButton()).toBeInTheDocument();
        expect(cancelButton()).toBeInTheDocument();
        // Inline: the confirm is part of the bubble, not a modal over it.
        expect(screen.queryByRole('dialog')).toBeNull();
        expect(screen.queryByRole('alertdialog')).toBeNull();
        // The icon gave way to the confirm, and nothing was deleted by merely asking.
        expect(trash()).toBeNull();
        expect(onDeleteAudio).not.toHaveBeenCalled();
      });

      it('deletes once when the confirm is accepted, and closes the confirm', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );

        fireEvent.click(trash()!);
        fireEvent.click(confirmButton()!);

        expect(onDeleteAudio).toHaveBeenCalledTimes(1);
        expect(screen.queryByText(t.chatAudioDeleteConfirm)).toBeNull();
        expect(confirmButton()).toBeNull();
      });

      it('restores the icon and deletes nothing when the confirm is cancelled', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );

        fireEvent.click(trash()!);
        fireEvent.click(cancelButton()!);

        expect(trash()).toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioDeleteConfirm)).toBeNull();
        expect(confirmButton()).toBeNull();
        expect(cancelButton()).toBeNull();
        expect(onDeleteAudio).not.toHaveBeenCalled();
      });

      it('can be asked again after a cancel', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );

        fireEvent.click(trash()!);
        fireEvent.click(cancelButton()!);
        fireEvent.click(trash()!);
        fireEvent.click(confirmButton()!);

        expect(onDeleteAudio).toHaveBeenCalledTimes(1);
      });

      it('words the confirm as final and points at regenerate as the way back', () => {
        // The wording is the requirement: once deleted the audio is gone, and "Regenerate"
        // (the button the not-stored bubble then offers) is how to get one again.
        expect(t.chatAudioDeleteConfirm).toContain(t.chatRegenerate);
      });

      it('moves focus to Cancel when asking, and back to the icon when cancelled', () => {
        render(<ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={() => {}} />);

        fireEvent.click(trash()!);
        // The safe answer holds the focus the removed icon left behind.
        expect(cancelButton()).toHaveFocus();

        fireEvent.click(cancelButton()!);
        expect(trash()).toHaveFocus();
      });

      it('is ABSENT on a not-stored bubble: there is nothing to delete', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[MARKER]}
            onRegenerate={() => {}}
            onDeleteAudio={() => {}}
          />,
        );

        expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
        expect(trash()).toBeNull();
        expect(confirmButton()).toBeNull();
      });

      it('is ABSENT on a mixed text + marker answer too', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'text', text: 'Hello there' }, MARKER]}
            onDeleteAudio={() => {}}
          />,
        );

        expect(screen.getByText('Hello there')).toBeInTheDocument();
        expect(trash()).toBeNull();
      });

      it('is shown on a mixed text + audio answer', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'text', text: 'Hello there' }, AUDIO]}
            onDeleteAudio={() => {}}
          />,
        );

        expect(trash()).toBeInTheDocument();
      });

      it('is absent on an answer without audio, whatever the row offers', () => {
        render(<ChatMessage t={t} role="assistant" content="plain" onDeleteAudio={() => {}} />);

        expect(trash()).toBeNull();
      });

      it('is absent where the row has no delete to give', () => {
        render(<ChatMessage t={t} role="assistant" content={[AUDIO]} onRegenerate={() => {}} />);

        expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
        expect(trash()).toBeNull();
      });

      it('is absent on the row of a run that is still streaming', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[AUDIO]}
            streaming
            kind="speech"
            onDeleteAudio={() => {}}
          />,
        );

        expect(trash()).toBeNull();
      });

      it('is never on a user turn: its audio is a transcription clip the user supplied', () => {
        render(
          <ChatMessage t={t} role="user" content={[AUDIO]} onDeleteAudio={() => {}} turnId="u1" />,
        );

        expect(trash()).toBeNull();
      });

      it('is not disabled by an unavailable model: deleting starts no run', () => {
        const onDeleteAudio = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[AUDIO]}
            canRun={false}
            onDeleteAudio={onDeleteAudio}
          />,
        );

        expect(trash()).toBeEnabled();
      });

      it('keeps the regenerate, copy and view-source buttons beside it', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[AUDIO]}
            onRegenerate={() => {}}
            onDeleteAudio={() => {}}
          />,
        );

        expect(screen.getByRole('button', { name: t.chatRegenerate })).toBeInTheDocument();
        expect(screen.getByRole('button', { name: t.chatCopy })).toBeInTheDocument();
        expect(screen.getByRole('button', { name: t.chatViewRaw })).toBeInTheDocument();
        expect(trash()).toBeInTheDocument();
      });

      it('does not carry an open confirm over to audio that comes back later', () => {
        const onDeleteAudio = vi.fn();
        const { rerender } = render(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );
        fireEvent.click(trash()!);
        expect(confirmButton()).toBeInTheDocument();

        // The audio goes (pruned elsewhere) while the question is open ...
        rerender(
          <ChatMessage t={t} role="assistant" content={[MARKER]} onDeleteAudio={onDeleteAudio} />,
        );
        expect(confirmButton()).toBeNull();
        // ... and a later audio of this row starts from the icon, not from a question about it.
        rerender(
          <ChatMessage t={t} role="assistant" content={[AUDIO]} onDeleteAudio={onDeleteAudio} />,
        );
        expect(trash()).toBeInTheDocument();
        expect(confirmButton()).toBeNull();
        expect(screen.queryByText(t.chatAudioDeleteConfirm)).toBeNull();
      });
    });

    // A transcription (speech-to-text) run is silent like a speech run -- the endpoint answers
    // once, with the finished transcript -- so it gets a wait of its own, not the text-pending
    // state's "reasoning active - 0 chars" (a measured zero where nothing was ever measured).
    describe('a transcription run', () => {
      const STORED_CLIP = '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10';
      const INLINE_CLIP = 'data:audio/webm;base64,AAAA';

      it('shows the transcription wait label, a clock and the no-news sentence while pending', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content=""
            streaming
            kind="transcription"
            elapsedMs={107_000}
          />,
        );
        expect(screen.getByText(t.chatTranscriptionRunPending)).toBeInTheDocument();
        expect(screen.getByText('1:47')).toBeInTheDocument();
        expect(screen.getByText(t.chatTranscriptionNoIntermediateNews)).toBeInTheDocument();
        // Not the text-pending state ("Denkt ... 0 Zeichen", the cursor), and not another kind's
        // wait copy.
        expect(screen.queryByText(new RegExp(t.chatCharsUnit))).toBeNull();
        expect(screen.queryByText(new RegExp(t.chatReasoningActive))).toBeNull();
        expect(screen.queryByTestId('chat-stream-cursor')).toBeNull();
        expect(screen.queryByText(t.chatAudioRunPending)).toBeNull();
        expect(screen.queryByText(t.chatImageRunPending)).toBeNull();
        // The output is text: no player, no download.
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
      });

      it('does not show the pending state once transcript text has arrived, even while streaming', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content={[{ type: 'text', text: 'hello there' }]}
            streaming
            kind="transcription"
            elapsedMs={12_000}
          />,
        );
        expect(screen.queryByText(t.chatTranscriptionRunPending)).toBeNull();
        expect(screen.getByText('hello there')).toBeInTheDocument();
      });

      it('shows no pending state when the run is not streaming (a failed or finished turn)', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            content="the run failed"
            kind="transcription"
            elapsedMs={1_000}
          />,
        );
        expect(screen.queryByText(t.chatTranscriptionRunPending)).toBeNull();
        expect(screen.getByText('the run failed')).toBeInTheDocument();
      });

      it('renders a finished transcript as ordinary text, with no player and no download', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            turnId="msg_t1"
            content={[{ type: 'text', text: 'Guten Morgen, wie geht es Ihnen?' }]}
          />,
        );
        expect(screen.getByText('Guten Morgen, wie geht es Ihnen?')).toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
        expect(screen.queryByRole('button', { name: t.chatDownloadAudio })).toBeNull();
        // The ordinary assistant controls are all there.
        expect(screen.getByRole('button', { name: t.chatCopy })).toBeInTheDocument();
      });

      describe('the user turn that carries the clip', () => {
        const player = () => screen.getByLabelText(t.chatAttachedAudio) as HTMLAudioElement;

        it('renders an inline clip as an <audio controls> player with a download button', () => {
          render(
            <ChatMessage
              t={t}
              role="user"
              turnId="msg_u1"
              content={[{ type: 'audio_url', audio_url: { url: INLINE_CLIP } }]}
            />,
          );
          expect(player().tagName).toBe('AUDIO');
          expect(player()).toHaveAttribute('controls');
          expect(player()).toHaveAttribute('src', INLINE_CLIP);
          expect(screen.getByRole('button', { name: t.chatDownloadAudio })).toBeInTheDocument();
          // An audio part is not an image.
          expect(screen.queryAllByRole('img')).toHaveLength(0);
          // It is the user's own clip, not generated audio.
          expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
        });

        it('plays a stored clip from a fetched object URL and revokes it on unmount', async () => {
          const createObjectURL = vi.fn(() => 'blob:input-clip');
          const revokeObjectURL = vi.fn();
          vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
          const fetchMock = vi.fn(async () => ({
            ok: true,
            status: 200,
            blob: async () => new Blob([new Uint8Array([1, 2, 3])], { type: 'audio/webm' }),
          }));
          vi.stubGlobal('fetch', fetchMock);

          const { unmount } = render(
            <ChatMessage
              t={t}
              role="user"
              turnId="msg_u1"
              content={[{ type: 'audio_url', audio_url: { url: STORED_CLIP } }]}
            />,
          );

          await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:input-clip'));
          expect(fetchMock).toHaveBeenCalledTimes(1);
          expect(fetchMock).toHaveBeenCalledWith(
            STORED_CLIP,
            expect.objectContaining({ credentials: 'same-origin' }),
          );
          expect(revokeObjectURL).not.toHaveBeenCalled();

          unmount();

          expect(revokeObjectURL).toHaveBeenCalledTimes(1);
          expect(revokeObjectURL).toHaveBeenCalledWith('blob:input-clip');
        });

        it('saves the clip under an input name and the extension of its own type', async () => {
          vi.stubGlobal('URL', {
            ...URL,
            createObjectURL: vi.fn(() => 'blob:input-clip'),
            revokeObjectURL: vi.fn(),
          });
          vi.stubGlobal(
            'fetch',
            vi.fn(async () => ({
              ok: true,
              status: 200,
              blob: async () => new Blob([new Uint8Array([1])], { type: 'audio/webm' }),
            })),
          );
          const names: string[] = [];
          vi.spyOn(document, 'createElement').mockImplementation(
            (tag: string, options?: unknown) => {
              const el = nativeCreateElement(tag, options as ElementCreationOptions);
              if (tag === 'a') {
                const anchor = el as HTMLAnchorElement;
                anchor.click = () => names.push(anchor.download);
              }
              return el;
            },
          );

          render(
            <ChatMessage
              t={t}
              role="user"
              turnId="msg_u1"
              content={[{ type: 'audio_url', audio_url: { url: STORED_CLIP } }]}
            />,
          );
          fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

          await waitFor(() => expect(names).toEqual(['input-audio-msg_u1.webm']));
        });

        it('has no edit control on an audio-only bubble, whatever onEdit is passed', () => {
          render(
            <ChatMessage
              t={t}
              role="user"
              content={[{ type: 'audio_url', audio_url: { url: INLINE_CLIP } }]}
              onEdit={vi.fn()}
            />,
          );
          expect(screen.queryByRole('button', { name: t.chatEdit })).toBeNull();
        });

        it('keeps the edit control on a text bubble, and on a text-and-image bubble', () => {
          const first = render(<ChatMessage t={t} role="user" content="words" onEdit={vi.fn()} />);
          expect(screen.getByRole('button', { name: t.chatEdit })).toBeInTheDocument();
          first.unmount();
          render(
            <ChatMessage
              t={t}
              role="user"
              content={[
                { type: 'text', text: 'look' },
                { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
              ]}
              onEdit={vi.fn()}
            />,
          );
          expect(screen.getByRole('button', { name: t.chatEdit })).toBeInTheDocument();
        });

        // Several clips on one user turn are NOT stitched (they are separate recordings): one
        // player per clip, each its own download.
        it('still renders one player per clip on a user turn with several audio parts', () => {
          const { container } = render(
            <ChatMessage
              t={t}
              role="user"
              turnId="msg_u1"
              content={['AAAA', 'BBBB', 'CCCC'].map((payload) => ({
                type: 'audio_url' as const,
                audio_url: { url: `data:audio/webm;base64,${payload}` },
              }))}
            />,
          );

          expect(container.querySelectorAll('audio')).toHaveLength(3);
          expect(screen.getAllByLabelText(t.chatAttachedAudio)).toHaveLength(3);
          expect(screen.getAllByRole('button', { name: t.chatDownloadAudio })).toHaveLength(3);
        });

        it('shows no player on a user turn without audio', () => {
          render(<ChatMessage t={t} role="user" content="just words" />);
          expect(screen.queryByLabelText(t.chatAttachedAudio)).toBeNull();
          expect(screen.queryByRole('button', { name: t.chatDownloadAudio })).toBeNull();
        });
      });

      // A speech thread is untouched: its audio is the ASSISTANT's, still the generated-audio
      // player, never the attached-clip one.
      it('leaves a speech reply on the generated-audio player', () => {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            turnId="msg_s1"
            content={[{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } }]}
          />,
        );
        expect(screen.getByLabelText(t.chatGeneratedAudio)).toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatAttachedAudio)).toBeNull();
      });
    });

    it('summarizes reasoning with char count and seconds once finished', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          content="answer"
          reasoning="thinking about it"
          reasoningMs={2000}
        />,
      );
      expect(
        screen.getByText((c) => c.includes(`17 ${t.chatCharsUnit}, 2.0s`)),
      ).toBeInTheDocument();
    });

    it('shows chars/s and, once known, tokens/s side by side (issue #56)', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          content="answer"
          ttftMs={500}
          tps={30}
          tokensPerSecond={10}
        />,
      );
      const line = screen.getByText(
        (c) =>
          c.includes(`30 ${t.chatCharsPerSecUnit}`) && c.includes(`10 ${t.chatTokensPerSecUnit}`),
      );
      expect(line).toBeInTheDocument();
      // Rounded, not raw, and ordered chars/s then tokens/s.
      expect(line.textContent).toContain(
        `30 ${t.chatCharsPerSecUnit} · 10 ${t.chatTokensPerSecUnit}`,
      );
    });

    it('omits tokens/s until it is known, still showing chars/s', () => {
      render(<ChatMessage t={t} role="assistant" content="answer" ttftMs={500} tps={30} />);
      expect(
        screen.getByText((c) => c.includes(`30 ${t.chatCharsPerSecUnit}`)),
      ).toBeInTheDocument();
      expect(screen.queryByText((c) => c.includes(t.chatTokensPerSecUnit))).not.toBeInTheDocument();
    });

    it('makes the assistant bubble hug its content with a request-width floor, but never the user bubble', () => {
      const assistant = render(<ChatMessage t={t} role="assistant" content="short" />);
      const answer = assistant.container.querySelector('[data-role="assistant"]');
      // Hugs content, right-aligned, but floors at the request bubble's fixed
      // width (760px via min()) so a short answer is never narrower than a question.
      expect(answer).toHaveStyle({
        width: 'fit-content',
        marginLeft: 'auto',
        minWidth: 'min(760px, 100%)',
      });
      cleanup();

      const user = render(<ChatMessage t={t} role="user" content="short" />);
      const prompt = user.container.querySelector('[data-role="user"]');
      // User bubble keeps its fixed-width block layout (no fit-content).
      expect(prompt).not.toHaveStyle({ width: 'fit-content' });
    });

    it('renders a generated assistant image at full size with the prompt as its alt text', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat on a bicycle"
          content={[{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }]}
        />,
      );
      const images = screen.getAllByRole('img');
      expect(images).toHaveLength(1);
      expect(images[0]).toHaveAttribute('src', 'data:image/png;base64,AAAA');
      // The accessible name is the prompt, not "Angehängtes Bild": the alt text
      // of a generated image is what it was asked to be.
      expect(images[0]).toHaveAttribute('alt', 'a cat on a bicycle');
      // NOT the 72x72 objectFit:cover upload thumbnail -- this is the artifact.
      expect(images[0]).not.toHaveAttribute('width', '72');
    });

    it('renders one image and one download control per data item', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="two cats"
          content={[
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,BBBB' } },
          ]}
        />,
      );
      expect(screen.getAllByRole('img')).toHaveLength(2);
      // data[] is plural by design -- the endpoint's own counter takes the
      // billed quantity from the response because a partial failure makes n and
      // data[] differ.
      expect(screen.getAllByRole('button', { name: t.chatDownloadImage })).toHaveLength(2);
    });

    describe('refining a generated image', () => {
      const first = '/api/portal/chat-blobs/blob_first';
      const second = '/api/portal/chat-blobs/blob_second';
      const twoImages = [
        { type: 'image_url' as const, image_url: { url: first } },
        { type: 'image_url' as const, image_url: { url: second } },
      ];

      it("offers one refine control per image, handing over that image's own url", () => {
        const onRefine = vi.fn();
        render(
          <ChatMessage
            t={t}
            role="assistant"
            promptText="two cats"
            content={twoImages}
            onRefine={onRefine}
          />,
        );

        const buttons = screen.getAllByRole('button', { name: t.chatRefineImage });
        expect(buttons).toHaveLength(2);
        fireEvent.click(buttons[1]);
        expect(onRefine).toHaveBeenCalledTimes(1);
        expect(onRefine).toHaveBeenLastCalledWith(second);
        fireEvent.click(buttons[0]);
        expect(onRefine).toHaveBeenLastCalledWith(first);
      });

      it('offers none unless the store allows it: the default is no control', () => {
        render(<ChatMessage t={t} role="assistant" content={twoImages} />);

        expect(screen.queryByRole('button', { name: t.chatRefineImage })).toBeNull();
        // The download control is unaffected.
        expect(screen.getAllByRole('button', { name: t.chatDownloadImage })).toHaveLength(2);
      });

      it('never puts one on an image of a user turn, whatever is passed', () => {
        render(
          <ChatMessage t={t} role="user" content={twoImages} onEdit={vi.fn()} onRefine={vi.fn()} />,
        );

        expect(screen.queryByRole('button', { name: t.chatRefineImage })).toBeNull();
      });

      it('has no control on a message without an image', () => {
        render(<ChatMessage t={t} role="assistant" content="just words" onRefine={vi.fn()} />);

        expect(screen.queryByRole('button', { name: t.chatRefineImage })).toBeNull();
      });
    });

    it('renders a revised prompt as ordinary text beside the image', () => {
      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[
            { type: 'text', text: 'a photorealistic cat, studio lighting' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
          ]}
        />,
      );
      // The only substantive news this endpoint ever reports about a
      // generation, and it needs no new part type to carry it.
      expect(screen.getByText('a photorealistic cat, studio lighting')).toBeInTheDocument();
      expect(screen.getAllByRole('img')).toHaveLength(1);
    });

    it('still renders a plain text assistant answer unchanged', () => {
      render(<ChatMessage t={t} role="assistant" content={'**bold** answer'} />);
      expect(screen.getByText('bold').tagName).toBe('STRONG');
      expect(screen.queryAllByRole('img')).toHaveLength(0);
    });

    it('downloads an image as binary, not as a text file containing the data URL', () => {
      const blobs: Blob[] = [];
      const createObjectURL = vi.fn((b: Blob) => {
        blobs.push(b);
        return 'blob:stub';
      });
      vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }]}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      expect(createObjectURL).toHaveBeenCalledTimes(1);
      // The saved Blob must be the DECODED bytes with the reported media type.
      // downloadText would have produced a text/plain Blob whose content is the
      // literal "data:image/png;base64,AAAA" string -- a text file, not an image.
      expect(blobs[0].type).toBe('image/png');
      expect(blobs[0].size).toBe(3); // "AAAA" base64-decodes to 3 bytes
    });

    // Guards against a hardcoded 'image/png' passing the test above for the
    // wrong reason (its fixture also happens to be png). sd-server's own
    // output_format decides the media type, and it is not always png -- so
    // the saved Blob's type must come from the data URL itself.
    it('downloads an image using the media type reported by its own data URL, not a hardcoded png', () => {
      const blobs: Blob[] = [];
      const createObjectURL = vi.fn((b: Blob) => {
        blobs.push(b);
        return 'blob:stub';
      });
      vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[{ type: 'image_url', image_url: { url: 'data:image/webp;base64,QUJDRA==' } }]}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      expect(createObjectURL).toHaveBeenCalledTimes(1);
      expect(blobs[0].type).toBe('image/webp');
      expect(blobs[0].size).toBe(4); // "ABCD" base64-decodes to 4 bytes
    });

    // Pins the wiring from ChatMessage/ImageTurn through to downloadBinary's
    // extensionFor, not just extensionFor in isolation: a mutation that makes
    // extensionFor return the wrong extension (or a component that stops
    // calling it) must fail here too, not just in downloadBinary's own test
    // file. jpeg is used deliberately -- it is the one branch (renamed to
    // jpg) a reader would not predict from the media type alone.
    it("sets the downloaded anchor's filename from the image's own media type", () => {
      let anchor: HTMLAnchorElement | undefined;
      vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
        const el = nativeCreateElement(tag, options as ElementCreationOptions);
        if (tag === 'a') anchor = el as HTMLAnchorElement;
        return el;
      });
      vi.stubGlobal('URL', {
        ...URL,
        createObjectURL: vi.fn(() => 'blob:stub'),
        revokeObjectURL: vi.fn(),
      });

      render(
        <ChatMessage
          t={t}
          role="assistant"
          turnId="msg_a1b2"
          promptText="a cat"
          content={[{ type: 'image_url', image_url: { url: 'data:image/jpeg;base64,AAAA' } }]}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      // The turn id is in the name too: the index alone restarts at 1 in
      // every turn, so two image turns in one conversation offered the same
      // `generated-image-1.jpg` for two different images.
      expect(anchor?.download).toBe('generated-image-msg_a1b2-1.jpg');
    });

    // The collision itself, at the component boundary: the same index in two
    // different turns must not produce the same file name. (Chat.test.tsx
    // pins the wiring that supplies the id from the transcript.)
    it("names two turns' first images differently", () => {
      const names: string[] = [];
      vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
        const el = nativeCreateElement(tag, options as ElementCreationOptions);
        if (tag === 'a') {
          const anchorEl = el as HTMLAnchorElement;
          anchorEl.click = () => names.push(anchorEl.download);
        }
        return el;
      });
      vi.stubGlobal('URL', {
        ...URL,
        createObjectURL: vi.fn(() => 'blob:stub'),
        revokeObjectURL: vi.fn(),
      });

      const content = [
        { type: 'image_url' as const, image_url: { url: 'data:image/png;base64,AAAA' } },
      ];
      const { unmount } = render(
        <ChatMessage t={t} role="assistant" turnId="msg_first" content={content} />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));
      unmount();
      render(<ChatMessage t={t} role="assistant" turnId="msg_second" content={content} />);
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      expect(names).toHaveLength(2);
      expect(names[0]).not.toBe(names[1]);
    });

    // The shape matches but the payload doesn't decode -- a truncated or
    // corrupted persisted transcript reaches exactly this. The download must
    // not crash (downloadBinary runs inside this onClick, with no error
    // boundary above it) and, since this whole feature exists to stop a
    // failure from being silent, the user must be told.
    it('tells the user when a generated image fails to download instead of failing silently', async () => {
      vi.stubGlobal('URL', {
        ...URL,
        createObjectURL: vi.fn(() => 'blob:stub'),
        revokeObjectURL: vi.fn(),
      });

      render(
        <ChatMessage
          t={t}
          role="assistant"
          promptText="a cat"
          content={[
            { type: 'image_url', image_url: { url: 'data:image/png;base64,not-valid-base64!!!' } },
          ]}
        />,
      );

      expect(() =>
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage })),
      ).not.toThrow();
      // The download is async now (a stored image is fetched first), so the
      // failure message lands a microtask after the click.
      const alert = await screen.findByText(t.chatImageDownloadError);
      expect(alert).toBeInTheDocument();
      // ...and it READS as a failure. This was hardcoded to the brand teal
      // (var(--brand-accent)) -- the portal's accent colour -- while every
      // other Typography role="alert" in the codebase is color="error". A
      // failure message in the accent colour is a failure the eye does not
      // register as one. Compared against a reference element rather than a
      // literal colour, so a theme change moves both together.
      render(
        <Typography color="error" data-testid="error-reference">
          reference
        </Typography>,
      );
      expect(window.getComputedStyle(alert).color).toBe(
        window.getComputedStyle(screen.getByTestId('error-reference')).color,
      );
    });

    // downloadBinary resolves false for every failure it knows of, but a browser
    // API can still throw while the file is being built (here: no object URL can
    // be created for the decoded blob). That used to escape ImageTurn's click
    // handler as an unhandled promise rejection with nothing shown; it has to
    // surface as the same inline error, and not reject.
    it('tells the user, without an unhandled rejection, when building the file throws', async () => {
      const unhandled = vi.fn();
      process.on('unhandledRejection', unhandled);
      try {
        vi.stubGlobal('URL', {
          ...URL,
          createObjectURL: vi.fn(() => {
            throw new Error('object url quota exceeded');
          }),
          revokeObjectURL: vi.fn(),
        });

        render(
          <ChatMessage
            t={t}
            role="assistant"
            promptText="a cat"
            content={[{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }]}
          />,
        );
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

        expect(await screen.findByText(t.chatImageDownloadError)).toBeInTheDocument();
        // Let any rejection that escaped the handler reach the process hook.
        await new Promise((resolve) => setTimeout(resolve, 0));
        expect(unhandled).not.toHaveBeenCalled();
      } finally {
        process.off('unhandledRejection', unhandled);
      }
    });

    // Stored images (binary payload store) are short gateway urls, not data:
    // URLs. Save has to fetch the bytes from the owner-scoped endpoint, and the
    // saved file's extension is whatever that response's Content-Type says --
    // the url itself says nothing about the media type.
    describe('a stored image served from the gateway', () => {
      const BLOB_URL = '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10';

      function renderStoredImage(turnId = 'msg_a1b2') {
        render(
          <ChatMessage
            t={t}
            role="assistant"
            turnId={turnId}
            promptText="a cat"
            content={[{ type: 'image_url', image_url: { url: BLOB_URL } }]}
          />,
        );
      }

      it("fetches the image with credentials and names the file from the response's Content-Type", async () => {
        let anchor: HTMLAnchorElement | undefined;
        const clicked: string[] = [];
        vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
          const el = nativeCreateElement(tag, options as ElementCreationOptions);
          if (tag === 'a') {
            const anchorEl = el as HTMLAnchorElement;
            anchor = anchorEl;
            // jsdom cannot navigate to the blob: URL the download builds.
            anchorEl.click = () => clicked.push(anchorEl.download);
          }
          return el;
        });
        const blobs: Blob[] = [];
        const createObjectURL = vi.fn((b: Blob) => {
          blobs.push(b);
          return 'blob:stub';
        });
        const revokeObjectURL = vi.fn();
        vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
        const fetchMock = vi.fn(async () => ({
          ok: true,
          status: 200,
          blob: async () => new Blob([new Uint8Array([1, 2, 3, 4])], { type: 'image/jpeg' }),
        }));
        vi.stubGlobal('fetch', fetchMock);

        renderStoredImage();
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

        await waitFor(() => expect(clicked).toHaveLength(1));
        expect(fetchMock).toHaveBeenCalledWith(BLOB_URL, { credentials: 'same-origin' });
        // Extension from the served media type (jpeg -> jpg), turn id in the
        // name exactly as for a data: URL.
        expect(anchor?.download).toBe('generated-image-msg_a1b2-1.jpg');
        expect(blobs[0].type).toBe('image/jpeg');
        expect(blobs[0].size).toBe(4);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:stub');
        expect(screen.queryByText(t.chatImageDownloadError)).not.toBeInTheDocument();
      });

      it('tells the user when the stored image cannot be fetched', async () => {
        const createObjectURL = vi.fn(() => 'blob:stub');
        vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => ({ ok: false, status: 404, blob: async () => new Blob([]) })),
        );

        renderStoredImage();
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

        expect(await screen.findByText(t.chatImageDownloadError)).toBeInTheDocument();
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('tells the user, without throwing, when the request itself fails', async () => {
        vi.stubGlobal('URL', {
          ...URL,
          createObjectURL: vi.fn(() => 'blob:stub'),
          revokeObjectURL: vi.fn(),
        });
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => {
            throw new TypeError('Failed to fetch');
          }),
        );

        renderStoredImage();
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

        expect(await screen.findByText(t.chatImageDownloadError)).toBeInTheDocument();
      });

      it('clears an earlier failure message once a later save succeeds', async () => {
        vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
          const el = nativeCreateElement(tag, options as ElementCreationOptions);
          if (tag === 'a') (el as HTMLAnchorElement).click = () => {};
          return el;
        });
        vi.stubGlobal('URL', {
          ...URL,
          createObjectURL: vi.fn(() => 'blob:stub'),
          revokeObjectURL: vi.fn(),
        });
        const fetchMock = vi
          .fn()
          .mockResolvedValueOnce({ ok: false, status: 404, blob: async () => new Blob([]) })
          .mockResolvedValueOnce({
            ok: true,
            status: 200,
            blob: async () => new Blob([new Uint8Array([1])], { type: 'image/png' }),
          });
        vi.stubGlobal('fetch', fetchMock);

        renderStoredImage();
        const save = screen.getByRole('button', { name: t.chatDownloadImage });
        fireEvent.click(save);
        expect(await screen.findByText(t.chatImageDownloadError)).toBeInTheDocument();

        fireEvent.click(save);
        await waitFor(() =>
          expect(screen.queryByText(t.chatImageDownloadError)).not.toBeInTheDocument(),
        );
        expect(fetchMock).toHaveBeenCalledTimes(2);
      });
    });
  });
}
