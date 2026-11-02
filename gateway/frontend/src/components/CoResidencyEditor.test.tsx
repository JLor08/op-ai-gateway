// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  CoResidencyEditor,
  CORESIDENCY_LIST_WIDTH_KEY,
  type CoResidencyItem,
} from './CoResidencyEditor';
import type { CoResidency } from '../api';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// jsdom (25) has no PointerEvent, so Testing Library falls back to a bare Event
// and the pointer coordinates a drag test passes are silently dropped. A
// MouseEvent subclass carries clientX and pointerId the way a browser's does.
function installPointerEvent() {
  if (typeof window.PointerEvent !== 'undefined') return;
  class TestPointerEvent extends MouseEvent {
    readonly pointerId: number;
    constructor(type: string, init: PointerEventInit = {}) {
      super(type, init);
      this.pointerId = init.pointerId ?? 0;
    }
  }
  vi.stubGlobal('PointerEvent', TestPointerEvent);
}

// Ids sort alphabetically in the order m_asr1 < m_chat1 < m_chat2 < m_chat3 <
// m_tts1, which is NOT the display order (names sort differently), so a
// canonicalisation that trusted display order would get the pair backwards.
const items: CoResidencyItem[] = [
  { id: 'm_asr1', name: 'Whisper Large', role: 'asr' },
  { id: 'm_chat1', name: 'Llama 70B', role: 'chat' },
  { id: 'm_chat2', name: 'Qwen 32B', role: 'chat' },
  { id: 'm_chat3', name: 'Mistral 7B', role: 'chat' },
  { id: 'm_tts1', name: 'Kokoro', role: 'tts' },
];

const escapeRe = (s: string) => s.replaceAll(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`);

/**
 * A stateful parent, the way RuntimeAdminSection drives the editor: it owns
 * the document, the editor reports the COMPLETE next document through onChange,
 * and the parent feeds it back. `seen` records every document the editor
 * emitted, in order.
 */
function Harness({
  initial,
  seen,
  list = items,
  disabled,
  disabledReason,
  locale,
}: Readonly<{
  initial: CoResidency;
  seen: CoResidency[];
  list?: CoResidencyItem[];
  disabled?: boolean;
  disabledReason?: string;
  locale: Locale;
}>) {
  const [doc, setDoc] = useState(initial);
  return (
    <CoResidencyEditor
      t={messages[locale]}
      items={list}
      mode={doc.mode}
      pairs={doc.pairs}
      onChange={(next) => {
        seen.push(next);
        setDoc(next);
      }}
      disabled={disabled}
      disabledReason={disabledReason}
    />
  );
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  const empty = (mode: CoResidency['mode'] = 'opt_out'): CoResidency => ({ mode, pairs: [] });

  function pick(name: string) {
    fireEvent.click(screen.getByRole('option', { name: new RegExp(escapeRe(name)) }));
  }
  function memberControl(name: string): HTMLElement {
    return screen.getByRole('group', { name: t.coresidencyMemberControl(name) });
  }
  function groupControl(roleLabel: string): HTMLElement {
    return screen.getByRole('group', { name: t.coresidencyGroupControl(roleLabel) });
  }
  function groupRegion(roleLabel: string): HTMLElement {
    return screen.getByRole('region', { name: roleLabel });
  }
  function verdictButton(control: HTMLElement, label: string): HTMLElement {
    return within(control).getByRole('button', { name: label });
  }
  function pressedLabels(control: HTMLElement): string[] {
    return within(control)
      .getAllByRole('button')
      .filter((b) => b.getAttribute('aria-pressed') === 'true')
      .map((b) => b.textContent ?? '');
  }
  function renderEditor(initial: CoResidency, extra: Partial<Parameters<typeof Harness>[0]> = {}) {
    const seen: CoResidency[] = [];
    render(<Harness initial={initial} seen={seen} locale={locale} {...extra} />);
    return seen;
  }

  describe(`CoResidencyEditor pairs [${locale}]`, () => {
    it('setting a member to "forbid" emits a canonical pair carrying verdict:"forbid" in the full set', () => {
      const seen = renderEditor(empty());
      pick('Qwen 32B'); // m_chat2 selected; m_chat1 sorts BEFORE it

      fireEvent.click(verdictButton(memberControl('Llama 70B'), t.coresidencyVerdictForbid));

      expect(seen).toHaveLength(1);
      expect(seen[0]).toEqual({
        mode: 'opt_out',
        pairs: [{ a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' }],
      });
    });

    it('canonicalises from the other side too: selecting the LOWER id emits the same pair', () => {
      const seen = renderEditor(empty());
      pick('Llama 70B'); // m_chat1 selected

      fireEvent.click(verdictButton(memberControl('Qwen 32B'), t.coresidencyVerdictForbid));

      expect(seen[0].pairs).toEqual([{ a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' }]);
    });

    it('edits ONE symmetric pair: the other side of A<->B reads and clears the same entry', () => {
      const seen = renderEditor(empty());
      pick('Qwen 32B');
      fireEvent.click(verdictButton(memberControl('Llama 70B'), t.coresidencyVerdictForbid));

      // From Llama's side the member is Qwen, and it is the SAME pair.
      pick('Llama 70B');
      expect(pressedLabels(memberControl('Qwen 32B'))).toEqual([t.coresidencyVerdictForbid]);
      fireEvent.click(verdictButton(memberControl('Qwen 32B'), t.coresidencyVerdictAllow));

      // Replaced, not duplicated: still exactly one explicit pair.
      expect(seen.at(-1)?.pairs).toEqual([{ a: 'm_chat1', b: 'm_chat2', verdict: 'allow' }]);
    });

    it('setting a pair back to "default" REMOVES its explicit entry (absence is default)', () => {
      const seen = renderEditor({
        mode: 'opt_out',
        pairs: [{ a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' }],
      });
      pick('Llama 70B');
      fireEvent.click(verdictButton(memberControl('Qwen 32B'), t.coresidencyVerdictDefaultAllowed));

      expect(seen.at(-1)).toEqual({ mode: 'opt_out', pairs: [] });
    });

    it('always emits the FULL set: pairs it did not touch -- shown or not -- ride along', () => {
      const orphan = { a: 'm_chat1', b: 'm_gone', verdict: 'allow' as const };
      const kept = { a: 'm_asr1', b: 'm_tts1', verdict: 'forbid' as const };
      const seen = renderEditor({ mode: 'opt_in', pairs: [kept, orphan] });
      pick('Qwen 32B');

      fireEvent.click(verdictButton(memberControl('Llama 70B'), t.coresidencyVerdictAllow));

      expect(seen[0].mode).toBe('opt_in');
      expect(seen[0].pairs).toHaveLength(3);
      expect(seen[0].pairs).toEqual(
        expect.arrayContaining([kept, orphan, { a: 'm_chat1', b: 'm_chat2', verdict: 'allow' }]),
      );
    });

    it('never offers the selected model a control against itself', () => {
      renderEditor(empty());
      pick('Llama 70B');
      expect(
        screen.queryByRole('group', { name: t.coresidencyMemberControl('Llama 70B') }),
      ).toBeNull();
      expect(memberControl('Qwen 32B')).toBeInTheDocument();
    });
  });

  describe(`CoResidencyEditor groups [${locale}]`, () => {
    it('a group bulk-set marks every member, and the group reads "mixed" once one member is overridden', () => {
      const seen = renderEditor(empty());
      pick('Whisper Large'); // m_asr1: groups tts (1) and chat (3)

      fireEvent.click(
        verdictButton(groupControl(t.coresidencyRoleChat), t.coresidencyVerdictForbid),
      );

      expect(seen).toHaveLength(1); // one write for the whole group
      expect(seen[0].pairs).toHaveLength(3);
      expect(seen[0].pairs).toEqual(
        expect.arrayContaining([
          { a: 'm_asr1', b: 'm_chat1', verdict: 'forbid' },
          { a: 'm_asr1', b: 'm_chat2', verdict: 'forbid' },
          { a: 'm_asr1', b: 'm_chat3', verdict: 'forbid' },
        ]),
      );
      for (const member of ['Llama 70B', 'Qwen 32B', 'Mistral 7B']) {
        expect(pressedLabels(memberControl(member))).toEqual([t.coresidencyVerdictForbid]);
      }
      expect(pressedLabels(groupControl(t.coresidencyRoleChat))).toEqual([
        t.coresidencyVerdictForbid,
      ]);
      expect(screen.queryByText(t.coresidencyGroupMixed)).toBeNull();
      // The other group was not touched.
      expect(pressedLabels(memberControl('Kokoro'))).toEqual([t.coresidencyVerdictDefaultAllowed]);

      // Override ONE member of the group.
      fireEvent.click(verdictButton(memberControl('Qwen 32B'), t.coresidencyVerdictDefaultAllowed));

      expect(seen.at(-1)?.pairs).toHaveLength(2);
      expect(
        within(groupRegion(t.coresidencyRoleChat)).getByText(t.coresidencyGroupMixed),
      ).toBeInTheDocument();
      // Mixed means no single segment is selected on the group control.
      expect(pressedLabels(groupControl(t.coresidencyRoleChat))).toEqual([]);
      expect(pressedLabels(memberControl('Qwen 32B'))).toEqual([
        t.coresidencyVerdictDefaultAllowed,
      ]);
      expect(pressedLabels(memberControl('Llama 70B'))).toEqual([t.coresidencyVerdictForbid]);
    });

    it('a uniform group shows members inheriting "from group"; a mixed one does not', () => {
      renderEditor(empty());
      pick('Whisper Large');
      const chat = groupRegion(t.coresidencyRoleChat);

      // Three chat members, all default -> all inherit the group's value.
      expect(within(chat).getAllByText(t.coresidencyFromGroup)).toHaveLength(3);

      fireEvent.click(verdictButton(memberControl('Qwen 32B'), t.coresidencyVerdictAllow));
      expect(within(chat).queryByText(t.coresidencyFromGroup)).toBeNull();
      expect(within(chat).getByText(t.coresidencyGroupMixed)).toBeInTheDocument();

      // Bulk-setting the group again makes it uniform and clears the marker.
      fireEvent.click(
        verdictButton(groupControl(t.coresidencyRoleChat), t.coresidencyVerdictDefaultAllowed),
      );
      expect(within(chat).queryByText(t.coresidencyGroupMixed)).toBeNull();
      expect(within(chat).getAllByText(t.coresidencyFromGroup)).toHaveLength(3);
    });

    it('a group bulk-set to "default" clears the explicit entries of its members only', () => {
      const keep = { a: 'm_asr1', b: 'm_tts1', verdict: 'forbid' as const };
      const seen = renderEditor({
        mode: 'opt_out',
        pairs: [
          keep,
          { a: 'm_asr1', b: 'm_chat1', verdict: 'forbid' },
          { a: 'm_asr1', b: 'm_chat2', verdict: 'allow' },
        ],
      });
      pick('Whisper Large');
      expect(
        within(groupRegion(t.coresidencyRoleChat)).getByText(t.coresidencyGroupMixed),
      ).toBeInTheDocument();

      fireEvent.click(
        verdictButton(groupControl(t.coresidencyRoleChat), t.coresidencyVerdictDefaultAllowed),
      );

      expect(seen.at(-1)).toEqual({ mode: 'opt_out', pairs: [keep] });
    });

    it("groups follow each item's role in a fixed order, hide empty groups and never show the selection in its own list", () => {
      const list: CoResidencyItem[] = [
        { id: 'm_01', name: 'Alpha', role: 'chat' },
        // Named like a chat model, but its role says embedding: the group follows the role.
        { id: 'm_02', name: 'Qwen3-Embedding-8B', role: 'embedding' },
        { id: 'm_03', name: 'Bravo', role: 'chat' },
        { id: 'm_04', name: 'Flux', role: 'image' },
        { id: 'm_05', name: 'Rerank-Large', role: 'reranker' },
        { id: 'm_06', name: 'Whisper', role: 'asr' },
        { id: 'm_07', name: 'Piper', role: 'tts' },
        { id: 'm_08', name: 'Mystery', role: 'other' },
      ];
      renderEditor(empty(), { list });
      pick('Alpha');

      const regions = screen.getAllByRole('region').map((r) => r.getAttribute('aria-label'));
      expect(regions).toEqual([
        t.coresidencyRoleAsr,
        t.coresidencyRoleTts,
        t.coresidencyRoleImage,
        t.coresidencyRoleEmbedding,
        t.coresidencyRoleReranker,
        t.coresidencyRoleChat,
        t.coresidencyRoleOther,
      ]);
      // The embedding model is in the embedding group and NOT in chat; chat holds
      // only Bravo (Alpha is the selection).
      const embedding = groupRegion(t.coresidencyRoleEmbedding);
      expect(
        within(embedding).getByRole('group', {
          name: t.coresidencyMemberControl('Qwen3-Embedding-8B'),
        }),
      ).toBeInTheDocument();
      const chat = groupRegion(t.coresidencyRoleChat);
      expect(
        within(chat).queryByRole('group', {
          name: t.coresidencyMemberControl('Qwen3-Embedding-8B'),
        }),
      ).toBeNull();
      expect(within(chat).getAllByRole('group')).toHaveLength(2); // group control + Bravo
    });

    it('an unknown future role falls into the "other" group rather than disappearing', () => {
      const list = [
        { id: 'm_01', name: 'Alpha', role: 'chat' },
        { id: 'm_02', name: 'Video-Model', role: 'video' },
      ] as unknown as CoResidencyItem[];
      renderEditor(empty(), { list });
      pick('Alpha');
      expect(
        within(groupRegion(t.coresidencyRoleOther)).getByRole('group', {
          name: t.coresidencyMemberControl('Video-Model'),
        }),
      ).toBeInTheDocument();
    });

    it('groups collapse and expand without losing their header control', () => {
      renderEditor(empty());
      pick('Whisper Large');
      const chat = groupRegion(t.coresidencyRoleChat);
      const toggle = within(chat).getByRole('button', {
        name: t.coresidencyGroupToggle(t.coresidencyRoleChat),
      });
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
      expect(
        screen.getByRole('group', { name: t.coresidencyMemberControl('Llama 70B') }),
      ).toBeInTheDocument();

      fireEvent.click(toggle);

      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(
        screen.queryByRole('group', { name: t.coresidencyMemberControl('Llama 70B') }),
      ).toBeNull();
      // The group-level control stays, so a collapsed group can still be bulk-set.
      expect(groupControl(t.coresidencyRoleChat)).toBeInTheDocument();
      // The other group is unaffected.
      expect(memberControl('Kokoro')).toBeInTheDocument();

      fireEvent.click(toggle);
      expect(memberControl('Llama 70B')).toBeInTheDocument();
    });
  });

  describe(`CoResidencyEditor mode [${locale}]`, () => {
    const explicit: CoResidency = {
      mode: 'opt_out',
      pairs: [
        { a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' },
        { a: 'm_asr1', b: 'm_chat1', verdict: 'allow' },
      ],
    };

    it('switching the mode re-labels "default" WITHOUT clearing explicit cells, and submits the new mode with the pairs intact', () => {
      const seen = renderEditor(explicit);
      pick('Llama 70B');

      // opt_out: default means allowed; the explicit cells show their own verdicts.
      expect(screen.getByText(t.coresidencyDefaultAllowedHint)).toBeInTheDocument();
      expect(pressedLabels(memberControl('Qwen 32B'))).toEqual([t.coresidencyVerdictForbid]);
      expect(pressedLabels(memberControl('Whisper Large'))).toEqual([t.coresidencyVerdictAllow]);
      expect(pressedLabels(memberControl('Mistral 7B'))).toEqual([
        t.coresidencyVerdictDefaultAllowed,
      ]);
      expect(screen.queryByRole('button', { name: t.coresidencyVerdictDefaultDenied })).toBeNull();

      fireEvent.click(screen.getByRole('button', { name: t.coresidencyModeOptIn }));

      // Submitted: the new mode and EXACTLY the explicit set it read.
      expect(seen).toHaveLength(1);
      expect(seen[0]).toEqual({ mode: 'opt_in', pairs: explicit.pairs });
      // Re-labelled: the undecided cell now reads "not allowed", the hint follows...
      expect(screen.getByText(t.coresidencyDefaultDeniedHint)).toBeInTheDocument();
      expect(screen.queryByText(t.coresidencyDefaultAllowedHint)).toBeNull();
      expect(pressedLabels(memberControl('Mistral 7B'))).toEqual([
        t.coresidencyVerdictDefaultDenied,
      ]);
      expect(screen.queryByRole('button', { name: t.coresidencyVerdictDefaultAllowed })).toBeNull();
      // ...and every explicit cell is exactly where it was.
      expect(pressedLabels(memberControl('Qwen 32B'))).toEqual([t.coresidencyVerdictForbid]);
      expect(pressedLabels(memberControl('Whisper Large'))).toEqual([t.coresidencyVerdictAllow]);
    });

    it('marks the active mode and ignores a click on it (no write)', () => {
      const seen = renderEditor(explicit);
      const optOut = screen.getByRole('button', { name: t.coresidencyModeOptOut });
      expect(optOut).toHaveAttribute('aria-pressed', 'true');
      expect(screen.getByRole('button', { name: t.coresidencyModeOptIn })).toHaveAttribute(
        'aria-pressed',
        'false',
      );

      fireEvent.click(optOut);

      expect(seen).toHaveLength(0);
    });

    it('re-labels a group control\'s "default" per mode too', () => {
      renderEditor({ mode: 'opt_in', pairs: [] });
      pick('Whisper Large');
      expect(pressedLabels(groupControl(t.coresidencyRoleChat))).toEqual([
        t.coresidencyVerdictDefaultDenied,
      ]);
    });
  });

  describe(`CoResidencyEditor master list [${locale}]`, () => {
    it('lists every model by name and shows the explicit-pair count per model', () => {
      renderEditor({
        mode: 'opt_out',
        pairs: [
          { a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' },
          { a: 'm_asr1', b: 'm_chat1', verdict: 'allow' },
        ],
      });
      const options = screen.getAllByRole('option');
      expect(options.map((o) => o.textContent)).toEqual([
        'Kokoro',
        'Llama 70B2',
        'Mistral 7B',
        'Qwen 32B1',
        'Whisper Large1',
      ]);
      expect(
        within(screen.getByRole('option', { name: /Llama 70B/ })).getByLabelText(
          t.coresidencyExceptions(2),
        ),
      ).toBeInTheDocument();
    });

    it('selects the first model by default and moves the detail when another is picked', () => {
      renderEditor(empty());
      expect(screen.getByRole('option', { name: /Kokoro/ })).toHaveAttribute(
        'aria-selected',
        'true',
      );
      expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('Kokoro');

      pick('Qwen 32B');

      expect(screen.getByRole('option', { name: /Qwen 32B/ })).toHaveAttribute(
        'aria-selected',
        'true',
      );
      expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('Qwen 32B');
    });

    it('filters the list by the search text, case-insensitively, and says so when nothing matches', () => {
      renderEditor(empty());
      const search = screen.getByRole('textbox', { name: t.coresidencySearch });

      fireEvent.change(search, { target: { value: 'QWEN' } });
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['Qwen 32B']);

      fireEvent.change(search, { target: { value: 'zzz' } });
      expect(screen.queryAllByRole('option')).toHaveLength(0);
      expect(screen.getByText(t.coresidencyNoMatch)).toBeInTheDocument();
    });

    it('asks for a second model when there is nothing to pair', () => {
      renderEditor(empty(), { list: [items[0]] });
      expect(screen.getByText(t.coresidencyNeedTwo)).toBeInTheDocument();
      expect(screen.queryByRole('option')).toBeNull();
    });
  });

  describe(`CoResidencyEditor long names [${locale}]`, () => {
    const longName = 'Qwen3-Coder-30B-A3B-Instruct-UD-Q4_K_XL-with-an-extra-long-suffix-262144-ctx';
    const longList: CoResidencyItem[] = [
      { id: 'm_a', name: longName, role: 'chat' },
      { id: 'm_b', name: `${longName}-b`, role: 'chat' },
    ];

    it('truncates names with an ellipsis and offers the full name on hover in the list and in rows', () => {
      renderEditor(empty(), { list: longList });

      // The list carries the full name as its hover text...
      const inList = within(screen.getAllByRole('option')[0]).getByTitle(longName);
      expect(getComputedStyle(inList).textOverflow).toBe('ellipsis');
      expect(getComputedStyle(inList).whiteSpace).toBe('nowrap');
      // ...and so does the member row (the other model; the selection is the first).
      const row = within(memberControl(`${longName}-b`).parentElement as HTMLElement).getByTitle(
        `${longName}-b`,
      );
      expect(getComputedStyle(row).textOverflow).toBe('ellipsis');
    });

    it('shows the selected model in full at the top, never truncated', () => {
      renderEditor(empty(), { list: longList });
      const heading = screen.getByRole('heading', { level: 3 });
      expect(heading).toHaveTextContent(longName);
      expect(getComputedStyle(heading).textOverflow).not.toBe('ellipsis');
      expect(getComputedStyle(heading).whiteSpace).not.toBe('nowrap');
    });
  });

  describe(`CoResidencyEditor divider [${locale}]`, () => {
    const separator = () => screen.getByRole('separator', { name: t.coresidencyResize });

    it('resizes from the keyboard and remembers the width', () => {
      renderEditor(empty());
      const before = Number(separator().getAttribute('aria-valuenow'));

      fireEvent.keyDown(separator(), { key: 'ArrowRight' });
      const after = Number(separator().getAttribute('aria-valuenow'));
      expect(after).toBeGreaterThan(before);
      expect(window.localStorage.getItem(CORESIDENCY_LIST_WIDTH_KEY)).toBe(String(after));

      // A fresh mount starts from the remembered width.
      cleanup();
      renderEditor(empty());
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(after);
    });

    it('is draggable, stays within its bounds, and remembers where it was dropped', () => {
      installPointerEvent();
      renderEditor(empty());
      const min = Number(separator().getAttribute('aria-valuemin'));
      const max = Number(separator().getAttribute('aria-valuemax'));

      fireEvent.pointerDown(separator(), { clientX: 300, pointerId: 1 });
      fireEvent.pointerMove(separator(), { clientX: 340, pointerId: 1 });
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(340);

      fireEvent.pointerMove(separator(), { clientX: 99999, pointerId: 1 });
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(max);
      fireEvent.pointerMove(separator(), { clientX: -50, pointerId: 1 });
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(min);

      fireEvent.pointerMove(separator(), { clientX: 400, pointerId: 1 });
      fireEvent.pointerUp(separator(), { clientX: 400, pointerId: 1 });
      expect(window.localStorage.getItem(CORESIDENCY_LIST_WIDTH_KEY)).toBe('400');

      // After the drop a move no longer resizes.
      fireEvent.pointerMove(separator(), { clientX: 250, pointerId: 1 });
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(400);
    });

    // The detail pane holds the pinned Allowed / Default / Forbidden control (up
    // to ~380px in German under opt-in) plus its row's padding and a name, so the
    // list may only grow until the detail pane has 620px left (and the divider's
    // own 6px). With the list at its static 560 maximum on a ~990px container the
    // detail pane was ~74px and the control scrolled out of view.
    describe('against the container width', () => {
      const SEPARATOR = 6;
      const DETAIL_MIN = 620;

      function stubContainerWidth(width: number) {
        const rect = () =>
          ({
            x: 0,
            y: 0,
            left: 0,
            top: 0,
            right: width,
            bottom: 400,
            width,
            height: 400,
            toJSON: () => ({}),
          }) as DOMRect;
        return vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(rect);
      }
      const valueNow = () => Number(separator().getAttribute('aria-valuenow'));
      const valueMax = () => Number(separator().getAttribute('aria-valuemax'));

      afterEach(() => vi.restoreAllMocks());

      it('caps the upper bound so the detail pane keeps room for its control', () => {
        installPointerEvent();
        stubContainerWidth(1000);
        renderEditor(empty());
        expect(valueMax()).toBe(1000 - SEPARATOR - DETAIL_MIN);

        fireEvent.pointerDown(separator(), { clientX: 300, pointerId: 1 });
        fireEvent.pointerMove(separator(), { clientX: 99999, pointerId: 1 });
        expect(valueNow()).toBe(valueMax());
        expect(1000 - SEPARATOR - valueNow()).toBeGreaterThanOrEqual(DETAIL_MIN);
        fireEvent.pointerUp(separator(), { clientX: 99999, pointerId: 1 });

        fireEvent.keyDown(separator(), { key: 'Home' });
        fireEvent.keyDown(separator(), { key: 'End' });
        expect(valueNow()).toBe(1000 - SEPARATOR - DETAIL_MIN);
        fireEvent.keyDown(separator(), { key: 'ArrowRight' });
        expect(valueNow()).toBe(1000 - SEPARATOR - DETAIL_MIN);
      });

      it('shows a remembered width that no longer fits clamped, without forgetting it', () => {
        stubContainerWidth(1000);
        window.localStorage.setItem(CORESIDENCY_LIST_WIDTH_KEY, '560');
        renderEditor(empty());

        expect(valueNow()).toBe(1000 - SEPARATOR - DETAIL_MIN);
        // Merely viewing on a narrow window does not overwrite the remembered
        // width a wide one chose.
        expect(window.localStorage.getItem(CORESIDENCY_LIST_WIDTH_KEY)).toBe('560');
      });

      it('re-clamps when the container shrinks, and gives the width back when it grows', () => {
        const rects = stubContainerWidth(1400);
        let notify: () => void = () => {};
        vi.stubGlobal(
          'ResizeObserver',
          class {
            constructor(callback: () => void) {
              notify = callback;
            }
            observe() {}
            disconnect() {}
          },
        );
        window.localStorage.setItem(CORESIDENCY_LIST_WIDTH_KEY, '400');
        renderEditor(empty());
        expect(valueMax()).toBe(560); // 1400 - 626 = 774, capped by the static bound
        expect(valueNow()).toBe(400);

        rects.mockRestore();
        stubContainerWidth(900);
        act(() => notify());
        expect(valueMax()).toBe(900 - SEPARATOR - DETAIL_MIN);
        expect(valueNow()).toBe(900 - SEPARATOR - DETAIL_MIN);

        vi.restoreAllMocks();
        stubContainerWidth(1400);
        act(() => notify());
        expect(valueNow()).toBe(400);
      });
    });

    it('ignores a corrupt or out-of-range remembered width', () => {
      const min = 160;
      window.localStorage.setItem(CORESIDENCY_LIST_WIDTH_KEY, 'not-a-number');
      renderEditor(empty());
      const fallback = Number(separator().getAttribute('aria-valuenow'));
      expect(fallback).toBeGreaterThanOrEqual(min);
      cleanup();

      window.localStorage.setItem(CORESIDENCY_LIST_WIDTH_KEY, '1');
      renderEditor(empty());
      expect(Number(separator().getAttribute('aria-valuenow'))).toBe(fallback);
    });

    it('works when storage is unavailable: reads and writes are best-effort', () => {
      const broken = {
        getItem: () => {
          throw new Error('denied');
        },
        setItem: () => {
          throw new Error('denied');
        },
        removeItem: () => {},
        clear: () => {},
        key: () => null,
        length: 0,
      } satisfies Storage;
      vi.stubGlobal('localStorage', broken);

      renderEditor(empty());
      const before = Number(separator().getAttribute('aria-valuenow'));
      expect(() => fireEvent.keyDown(separator(), { key: 'ArrowRight' })).not.toThrow();
      expect(Number(separator().getAttribute('aria-valuenow'))).toBeGreaterThan(before);
    });
  });

  describe(`CoResidencyEditor disabled [${locale}]`, () => {
    it('disables every verdict control and the mode toggle, and says why', () => {
      const seen = renderEditor(
        { mode: 'opt_out', pairs: [{ a: 'm_chat1', b: 'm_chat2', verdict: 'forbid' }] },
        { disabled: true, disabledReason: t.coresidencySaving },
      );
      pick('Whisper Large');

      expect(screen.getByText(t.coresidencySaving)).toBeInTheDocument();
      const forbid = verdictButton(memberControl('Llama 70B'), t.coresidencyVerdictForbid);
      expect(forbid).toBeDisabled();
      expect(
        verdictButton(groupControl(t.coresidencyRoleChat), t.coresidencyVerdictForbid),
      ).toBeDisabled();
      expect(screen.getByRole('button', { name: t.coresidencyModeOptIn })).toBeDisabled();
      fireEvent.click(forbid);
      expect(seen).toHaveLength(0);
      // Looking is never disabled: the list still selects.
      pick('Kokoro');
      expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('Kokoro');
    });
  });
}
