// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CaptureDialog, deriveChat, deriveReasoning } from './CaptureDialog';
import { messages } from '../i18n';
import type { CaptureDetail } from '../api';

const t = messages.de;

function makeDetail(overrides: Partial<CaptureDetail> = {}): CaptureDetail {
  return {
    id: 'req_1',
    api_flavor: 'openai_chat_completions',
    http_status: 200,
    created_at: '2026-07-10T12:00:00Z',
    req_headers: { 'Content-Type': ['application/json'] },
    req_body: `{"model":"m"}`,
    resp_headers: { 'Content-Type': ['application/json'] },
    resp_body: `{"choices":[{"message":{"role":"assistant","content":"hi from openai"}}]}`,
    truncated: false,
    secret: false,
    can_toggle_secret: false,
    ...overrides,
  };
}

afterEach(cleanup);

describe('CaptureDialog', () => {
  it('renders nothing when closed', () => {
    render(
      <CaptureDialog
        t={t}
        open={false}
        onClose={vi.fn()}
        detail={makeDetail()}
        loading={false}
        error=""
      />,
    );
    expect(screen.queryByText(t.captureDialogTitle)).not.toBeInTheDocument();
  });

  it('shows the header tables and does not render redacted request headers', () => {
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({
          req_headers: { 'Content-Type': ['application/json'], 'X-Trace': ['abc'] },
        })}
        loading={false}
        error=""
      />,
    );
    expect(screen.getByText(t.captureReqHeaders)).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'X-Trace' })).toBeInTheDocument();
    // Authorization was redacted server-side; the fixture omits it entirely.
    expect(screen.queryByRole('cell', { name: 'Authorization' })).not.toBeInTheDocument();
  });

  it('toggles the request body between pretty and raw', () => {
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ req_body: `{"a":1}` })}
        loading={false}
        error=""
      />,
    );
    // pretty-printed inserts a space after the colon
    expect(screen.getByText(/"a": 1/)).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole('button', { name: t.captureRaw })[0]); // request Raw
    expect(screen.getByText(`{"a":1}`)).toBeInTheDocument();
  });

  it('shows chat-derived assistant text and toggles to raw', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={makeDetail()} loading={false} error="" />,
    );
    expect(screen.getByText('hi from openai')).toBeInTheDocument(); // Chat view (default)
    fireEvent.click(screen.getAllByRole('button', { name: t.captureRaw })[1]); // response Raw
    expect(screen.getByText(/"content":"hi from openai"/)).toBeInTheDocument();
  });

  it('shows the http status badge and a truncated badge only when truncated', () => {
    const { unmount } = render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ http_status: 200 })}
        loading={false}
        error=""
      />,
    );
    expect(screen.getByText(`${t.captureHttpStatus}: 200`)).toBeInTheDocument();
    expect(screen.queryByText(t.captureTruncated)).not.toBeInTheDocument();
    unmount();
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ truncated: true })}
        loading={false}
        error=""
      />,
    );
    expect(screen.getByText(t.captureTruncated)).toBeInTheDocument();
  });

  it('calls onClose from the close button', () => {
    const onClose = vi.fn();
    render(
      <CaptureDialog t={t} open onClose={onClose} detail={makeDetail()} loading={false} error="" />,
    );
    fireEvent.click(screen.getByRole('button', { name: t.captureClose }));
    expect(onClose).toHaveBeenCalled();
  });

  it('shows an inline error message', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={null} loading={false} error="boom" />,
    );
    expect(screen.getByRole('alert')).toHaveTextContent('boom');
  });

  it('hides the translated-upstream sections when there is no translated communication', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={makeDetail()} loading={false} error="" />,
    );
    expect(screen.queryByText(t.captureTranslatedReqTitle)).not.toBeInTheDocument();
    expect(screen.queryByText(t.captureTranslatedRespTitle)).not.toBeInTheDocument();
  });

  it('shows the translated upstream request/response when present', () => {
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({
          translated_req_headers: { 'Content-Type': ['application/json'] },
          translated_req_body: `{"model":"upstream-model","messages":[]}`,
          translated_resp_headers: { 'Content-Type': ['application/json'] },
          translated_resp_body: `{"choices":[{"message":{"content":"pong"}}]}`,
        })}
        loading={false}
        error=""
      />,
    );
    expect(screen.getByText(t.captureTranslatedReqTitle)).toBeInTheDocument();
    expect(screen.getByText(t.captureTranslatedRespTitle)).toBeInTheDocument();
    expect(screen.getByText(t.captureTranslatedNote)).toBeInTheDocument();
    expect(screen.getByText(/"model":"upstream-model"/)).toBeInTheDocument();
    expect(screen.getByText(/"content":"pong"/)).toBeInTheDocument();
  });

  it('shows a Delete button and calls onRequestDelete when provided', () => {
    const onRequestDelete = vi.fn();
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail()}
        loading={false}
        error=""
        onRequestDelete={onRequestDelete}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: t.captureDelete }));
    expect(onRequestDelete).toHaveBeenCalled();
  });

  it('does not render a Delete button when onRequestDelete is omitted', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={makeDetail()} loading={false} error="" />,
    );
    expect(screen.queryByRole('button', { name: t.captureDelete })).not.toBeInTheDocument();
  });

  it("shows a 'mark secret' toggle for a non-secret capture the owner can toggle", () => {
    const onToggleSecret = vi.fn();
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ can_toggle_secret: true, secret: false })}
        loading={false}
        error=""
        onToggleSecret={onToggleSecret}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: t.captureMarkSecret }));
    expect(onToggleSecret).toHaveBeenCalledWith(true);
  });

  it("shows a 'make visible' toggle for a secret capture the owner can toggle", () => {
    const onToggleSecret = vi.fn();
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ can_toggle_secret: true, secret: true })}
        loading={false}
        error=""
        onToggleSecret={onToggleSecret}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: t.captureMarkVisible }));
    expect(onToggleSecret).toHaveBeenCalledWith(false);
  });

  it('splits the response body chat view into a Denken and an Ausgabe block', () => {
    const sse =
      'data: {"choices":[{"delta":{"reasoning_content":"Let me think."}}]}\n\n' +
      'data: {"choices":[{"delta":{"content":"The answer."}}]}\n\n' +
      'data: [DONE]\n\n';
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ resp_body: sse })}
        loading={false}
        error=""
      />,
    );
    // chat is the default response view
    expect(screen.getByText(t.captureThinking)).toBeInTheDocument();
    expect(screen.getByText('Let me think.')).toBeInTheDocument();
    expect(screen.getByText(t.captureOutput)).toBeInTheDocument();
    expect(screen.getByText('The answer.')).toBeInTheDocument();
  });

  it('shows only the raw body (no Denken/Ausgabe split) in the response raw view', () => {
    const sse =
      'data: {"choices":[{"delta":{"reasoning_content":"Let me think."}}]}\n\n' +
      'data: {"choices":[{"delta":{"content":"The answer."}}]}\n\n' +
      'data: [DONE]\n\n';
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ resp_body: sse })}
        loading={false}
        error=""
      />,
    );
    fireEvent.click(screen.getAllByRole('button', { name: t.captureRaw })[1]); // response Raw
    expect(screen.queryByText(t.captureThinking)).not.toBeInTheDocument();
    expect(screen.queryByText(t.captureOutput)).not.toBeInTheDocument();
    expect(screen.queryByText('Let me think.')).not.toBeInTheDocument();
    expect(
      screen.getByText(
        (_, el) => el?.tagName === 'PRE' && (el.textContent ?? '').includes('reasoning_content'),
      ),
    ).toBeInTheDocument();
  });

  it('omits the Denken block when there is no reasoning, still showing the output', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={makeDetail()} loading={false} error="" />,
    );
    expect(screen.queryByText(t.captureThinking)).not.toBeInTheDocument();
    expect(screen.getByText(t.captureOutput)).toBeInTheDocument();
    expect(screen.getByText('hi from openai')).toBeInTheDocument();
  });

  it('renders the four sections as accordions that start expanded and collapse on summary click', () => {
    render(
      <CaptureDialog t={t} open onClose={vi.fn()} detail={makeDetail()} loading={false} error="" />,
    );
    for (const heading of [
      t.captureReqHeaders,
      t.captureReqBody,
      t.captureRespHeaders,
      t.captureRespBody,
    ]) {
      const summary = screen.getByRole('button', { name: heading });
      expect(summary).toHaveAttribute('aria-expanded', 'true');
    }
    const reqHeaders = screen.getByRole('button', { name: t.captureReqHeaders });
    fireEvent.click(reqHeaders);
    expect(reqHeaders).toHaveAttribute('aria-expanded', 'false');
  });

  it('clicking Pretty/Raw/Chat/Copy controls does not toggle the enclosing accordion', () => {
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ req_body: `{"a":1}` })}
        loading={false}
        error=""
      />,
    );
    const reqBody = screen.getByRole('button', { name: t.captureReqBody });
    const respBody = screen.getByRole('button', { name: t.captureRespBody });
    expect(reqBody).toHaveAttribute('aria-expanded', 'true');
    expect(respBody).toHaveAttribute('aria-expanded', 'true');

    fireEvent.click(screen.getAllByRole('button', { name: t.captureRaw })[0]); // request Raw
    fireEvent.click(screen.getByRole('button', { name: t.capturePretty })); // request Pretty
    fireEvent.click(screen.getByRole('button', { name: t.captureChat })); // response Chat
    fireEvent.click(screen.getAllByRole('button', { name: t.captureCopy })[0]); // a Copy button

    expect(reqBody).toHaveAttribute('aria-expanded', 'true');
    expect(respBody).toHaveAttribute('aria-expanded', 'true');
  });

  it('does not render a secret toggle when can_toggle_secret is false', () => {
    render(
      <CaptureDialog
        t={t}
        open
        onClose={vi.fn()}
        detail={makeDetail({ can_toggle_secret: false })}
        loading={false}
        error=""
        onToggleSecret={vi.fn()}
      />,
    );
    expect(screen.queryByRole('button', { name: t.captureMarkSecret })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.captureMarkVisible })).not.toBeInTheDocument();
  });
});

describe('deriveChat', () => {
  it('openai chat completions -> message content', () => {
    expect(deriveChat('openai_chat_completions', `{"choices":[{"message":{"content":"A"}}]}`)).toBe(
      'A',
    );
  });
  it('anthropic messages -> joined content text', () => {
    expect(
      deriveChat(
        'anthropic_messages',
        `{"content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}`,
      ),
    ).toBe('AB');
  });
  it('openai responses -> output text', () => {
    expect(
      deriveChat(
        'openai_responses',
        `{"output":[{"content":[{"type":"output_text","text":"A"}]}]}`,
      ),
    ).toBe('A');
  });
  it('portal chat -> message content', () => {
    expect(deriveChat('portal_chat', `{"message":{"role":"assistant","content":"hi"}}`)).toBe('hi');
  });
  it('openai streaming SSE -> accumulated deltas, skipping [DONE]', () => {
    const sse =
      'data: {"choices":[{"delta":{"content":"He"}}]}\n\n' +
      'data: {"choices":[{"delta":{"content":"llo"}}]}\n\n' +
      'data: [DONE]\n\n';
    expect(deriveChat('openai_chat_completions', sse)).toBe('Hello');
  });
});

describe('deriveReasoning', () => {
  it('streaming SSE -> accumulated reasoning_content deltas, skipping [DONE] and junk', () => {
    const sse =
      'data: {"choices":[{"delta":{"reasoning_content":"Let me "}}]}\n\n' +
      'data: not-json\n\n' +
      'data: {"choices":[{"delta":{"reasoning_content":"think."}}]}\n\n' +
      'data: [DONE]\n\n';
    expect(deriveReasoning('openai_chat_completions', sse)).toBe('Let me think.');
  });
  it('streaming SSE -> falls back to delta.reasoning when reasoning_content is absent', () => {
    const sse =
      'data: {"choices":[{"delta":{"reasoning":"A"}}]}\n\n' +
      'data: {"choices":[{"delta":{"reasoning":"B"}}]}\n\n';
    expect(deriveReasoning('openai_chat_completions', sse)).toBe('AB');
  });
  it('openai chat completions non-stream -> message.reasoning_content', () => {
    expect(
      deriveReasoning(
        'openai_chat_completions',
        `{"choices":[{"message":{"reasoning_content":"R","content":"A"}}]}`,
      ),
    ).toBe('R');
  });
  it('openai chat completions non-stream -> falls back to message.reasoning', () => {
    expect(
      deriveReasoning(
        'openai_chat_completions',
        `{"choices":[{"message":{"reasoning":"R","content":"A"}}]}`,
      ),
    ).toBe('R');
  });
  it('anthropic messages -> joined thinking parts', () => {
    expect(
      deriveReasoning(
        'anthropic_messages',
        `{"content":[{"type":"thinking","thinking":"one "},{"type":"text","text":"answer"},{"type":"thinking","thinking":"two"}]}`,
      ),
    ).toBe('one two');
  });
  it('anthropic messages -> thinking part falls back to reasoning field', () => {
    expect(
      deriveReasoning('anthropic_messages', `{"content":[{"type":"thinking","reasoning":"R"}]}`),
    ).toBe('R');
  });
  it('no reasoning present -> empty string', () => {
    expect(
      deriveReasoning('openai_chat_completions', `{"choices":[{"message":{"content":"A"}}]}`),
    ).toBe('');
    expect(deriveReasoning('openai_chat_completions', 'not json')).toBe('');
    expect(deriveReasoning('portal_chat', `{"message":{"content":"hi"}}`)).toBe('');
  });
});
