// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChatMessage } from './ChatMessage';
import { messages } from '../i18n';

const t = messages.de;

afterEach(cleanup);

describe('ChatMessage', () => {
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
    expect(screen.getByText(/17 Zeichen, 2\.0s/)).toBeInTheDocument();
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
});
