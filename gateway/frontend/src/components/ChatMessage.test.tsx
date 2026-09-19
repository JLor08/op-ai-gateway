// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
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

    it('hides the clock from assistive technology', () => {
      render(
        <ChatMessage t={t} role="assistant" content="" streaming kind="image" elapsedMs={5_000} />,
      );
      // The transcript is an aria-live log; a ticking number would be announced
      // once a second and make the thread unusable with a screen reader.
      expect(screen.getByText('5s').closest('[aria-hidden="true"]')).not.toBeNull();
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
      // No kind prop: the existing counter must be untouched, because this is
      // the overwhelmingly common case.
      expect(screen.getByText(new RegExp(t.chatReasoningActive))).toBeInTheDocument();
      expect(screen.getByText(new RegExp(t.chatCharsUnit))).toBeInTheDocument();
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
          promptText="a cat"
          content={[{ type: 'image_url', image_url: { url: 'data:image/jpeg;base64,AAAA' } }]}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

      expect(anchor?.download).toBe('generated-image-1.jpg');
    });

    // The shape matches but the payload doesn't decode -- a truncated or
    // corrupted persisted transcript reaches exactly this. The download must
    // not crash (downloadBinary runs inside this onClick, with no error
    // boundary above it) and, since this whole feature exists to stop a
    // failure from being silent, the user must be told.
    it('tells the user when a generated image fails to download instead of failing silently', () => {
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
      expect(screen.getByText(t.chatImageDownloadError)).toBeInTheDocument();
    });
  });
}
