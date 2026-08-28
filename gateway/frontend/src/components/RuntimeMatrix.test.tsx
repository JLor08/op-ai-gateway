// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RuntimeMatrix, type RuntimeMatrixSpec } from './RuntimeMatrix';
import { messages } from '../i18n';

const t = messages.de;

afterEach(cleanup);

// Ids deliberately increase alphabetically with array position (spec-a <
// spec-b < spec-c), i.e. the ORDINARY case where ids are assigned roughly in
// creation order -- this is enough to distinguish a naive "just pass
// (rowId, colId) through" implementation (which would emit the LATER
// position first, e.g. onToggle('spec-b', 'spec-a')) from a correct one
// (which always sorts, e.g. onToggle('spec-a', 'spec-b')), for every cell
// below the diagonal.
// Alpha touches BOTH GPUs so it shares one with each of the other two (GPU0
// with Bravo, GPU1 with Charlie); Bravo and Charlie themselves share nothing
// -- covering both the "shared GPU" and "disjoint GPUs" tooltip cases across
// the matrix's three cells.
const specs: RuntimeMatrixSpec[] = [
  {
    id: 'spec-a',
    model: 'Alpha',
    gpus: [
      { index: 0, vramMb: 20000 },
      { index: 1, vramMb: 2500 },
    ],
  },
  { id: 'spec-b', model: 'Bravo', gpus: [{ index: 0, vramMb: 24000 }] },
  { id: 'spec-c', model: 'Charlie', gpus: [{ index: 1, vramMb: 5000 }] },
];

function cellName(row: string, col: string): string {
  return `${t.runtimeMatrixCell}: ${row} + ${col}`;
}

function cell(row: string, col: string): HTMLElement {
  return screen.getByRole('checkbox', { name: cellName(row, col) });
}

// The glyph a cell is actually drawing. `@mui/icons-material` stamps each icon
// with `data-testid="<Name>Icon"` outside production builds, so this reads the
// rendered symbol rather than a prop we passed -- which is the whole point: the
// defect being guarded against is two states that LOOK the same.
function glyphOf(checkbox: HTMLElement): string {
  const svg = checkbox.parentElement?.querySelector('svg[data-testid]');
  return svg?.getAttribute('data-testid') ?? '(none)';
}

describe('RuntimeMatrix lower-triangle rendering', () => {
  it('renders exactly n(n-1)/2 cells for n specs', () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    const cells = screen.getAllByRole('checkbox', {
      name: new RegExp(`^${t.runtimeMatrixCell}:`),
    });
    // n=3 -> n(n-1)/2 = 3: (Bravo,Alpha), (Charlie,Alpha), (Charlie,Bravo).
    expect(cells).toHaveLength(3);
    expect(cell('Bravo', 'Alpha')).toBeInTheDocument();
    expect(cell('Charlie', 'Alpha')).toBeInTheDocument();
    expect(cell('Charlie', 'Bravo')).toBeInTheDocument();
    // Diagonal and upper-triangle cells are never drawn: neither Alpha's own
    // row nor an (Alpha, Bravo)/(Alpha, Charlie) cell (upper triangle) exists.
    expect(
      screen.queryByLabelText(cellName('Alpha', 'Bravo'), { selector: 'input' }),
    ).not.toBeInTheDocument();
  });

  it('shows a hint instead of a table when fewer than two specs are given', () => {
    render(<RuntimeMatrix t={t} specs={[specs[0]]} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    expect(screen.getByText(t.runtimeMatrixNeedTwo)).toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});

describe('RuntimeMatrix canonicalisation', () => {
  it('emits the pair in canonical (sorted) order on toggle, regardless of visual row/column order', () => {
    const onToggle = vi.fn();
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={onToggle} budgets={{}} />);

    // Toggling FROM OFF is the path a real operator takes and the one that was
    // effectively unreachable: the off state used to render as a prohibition
    // symbol, so nobody clicked it. Assert the cell really is off first, so
    // this stays a from-off toggle rather than accidentally becoming an
    // untick.
    const bravoAlpha = cell('Bravo', 'Alpha');
    expect(bravoAlpha).not.toBeChecked();
    expect(bravoAlpha).not.toBeDisabled();

    // Visual click is on the (Bravo, Alpha) cell -- row=Bravo (id spec-b),
    // col=Alpha (id spec-a). A naive pass-through would call
    // onToggle('spec-b', 'spec-a'); the canonical/sorted call is the reverse.
    fireEvent.click(bravoAlpha);
    expect(onToggle).toHaveBeenCalledWith('spec-a', 'spec-b');

    fireEvent.click(cell('Charlie', 'Bravo'));
    expect(onToggle).toHaveBeenCalledWith('spec-b', 'spec-c');
  });

  it('renders an existing pair as allowed (checked) regardless of the order it is stored in', () => {
    // Fed non-canonical ("b" before "a") to prove the component itself does
    // not depend on upstream canonicalisation to render correctly.
    render(
      <RuntimeMatrix
        t={t}
        specs={specs}
        pairs={[['spec-b', 'spec-a']]}
        onToggle={vi.fn()}
        budgets={{}}
      />,
    );
    expect(cell('Bravo', 'Alpha')).toBeChecked();
    expect(cell('Charlie', 'Alpha')).not.toBeChecked();
  });
});

// The defect this whole group exists for: an operator wanting two models
// co-resident on DIFFERENT GPUs reported "in der Matrix kann ich kein Häkchen
// machen". Nothing in the stack prevented them -- the off state rendered as a
// crossed-circle BlockIcon (the vocabulary of "forbidden by the system") and
// the tooltip led with a note about disjoint GPUs, which read as the reason
// for the prohibition.
describe('RuntimeMatrix cell affordance', () => {
  it('renders the off state as an unset control, never as a prohibition symbol', () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    const off = cell('Bravo', 'Alpha');
    expect(glyphOf(off)).toBe('CheckBoxOutlineBlankIcon');
    expect(glyphOf(off)).not.toBe('BlockIcon');
  });

  it('keeps a green tick for the on state', () => {
    render(
      <RuntimeMatrix
        t={t}
        specs={specs}
        pairs={[['spec-a', 'spec-b']]}
        onToggle={vi.fn()}
        budgets={{}}
      />,
    );
    expect(glyphOf(cell('Bravo', 'Alpha'))).toBe('CheckBoxIcon');
  });

  // THE regression to guard. "Off" and "not editable" are different facts and
  // used to wear the same BlockIcon, separated only by MUI's disabled styling
  // -- the defect class this branch has hit repeatedly. All four (allowed ×
  // editable) renderings are pinned together, and the assertion is on the
  // glyph actually in the DOM, so collapsing any two of them fails here.
  it('draws four distinct glyphs across allowed × editable, so off never looks disabled', () => {
    const pair: [string, string][] = [['spec-a', 'spec-b']];
    const seen: Record<string, string> = {};

    for (const editable of [true, false]) {
      for (const allowed of [true, false]) {
        const { unmount } = render(
          <RuntimeMatrix
            t={t}
            specs={specs}
            pairs={allowed ? pair : []}
            onToggle={vi.fn()}
            budgets={{}}
            disabled={!editable}
            disabledReason={editable ? undefined : t.runtimeMatrixDisabledFileMode}
          />,
        );
        seen[`${allowed ? 'on' : 'off'}/${editable ? 'editable' : 'disabled'}`] = glyphOf(
          cell('Bravo', 'Alpha'),
        );
        unmount();
      }
    }

    expect(seen).toEqual({
      // Square family: a control you can operate.
      'on/editable': 'CheckBoxIcon',
      'off/editable': 'CheckBoxOutlineBlankIcon',
      // Circle family: a read-out you cannot. The crossed circle finally
      // means prohibition, and a disabled ALLOWED pair still reads as
      // allowed -- a single "disabled glyph" would have said the opposite.
      'on/disabled': 'CheckCircleIcon',
      'off/disabled': 'BlockIcon',
    });
    // Four states, four symbols -- stated separately so a future edit that
    // makes two of them equal fails on the property, not only on the table.
    expect(new Set(Object.values(seen)).size).toBe(4);
  });

  it('does not toggle a disabled cell and explains on hover why it cannot be clicked', async () => {
    const onToggle = vi.fn();
    render(
      <RuntimeMatrix
        t={t}
        specs={specs}
        pairs={[]}
        onToggle={onToggle}
        budgets={{}}
        disabled
        disabledReason={t.runtimeMatrixDisabledFileMode}
      />,
    );
    const disabledCell = cell('Bravo', 'Alpha');
    expect(disabledCell).toBeDisabled();
    fireEvent.click(disabledCell);
    expect(onToggle).not.toHaveBeenCalled();

    // A disabled control must say WHY -- the rule RowActionsCell/IconAction
    // were fixed for earlier on this branch. The reason leads the tooltip,
    // ahead of even the consequence: an operator who does not know the grid is
    // read-only will otherwise keep clicking a control that never responds.
    fireEvent.mouseOver(disabledCell);
    const tip = await screen.findByRole('tooltip');
    expect(tip).toHaveTextContent(t.runtimeMatrixDisabledFileMode);
    expect(tip.textContent?.indexOf(t.runtimeMatrixDisabledFileMode)).toBe(0);
  });

  // Stated as a difference rather than as a string match, so it holds without
  // reference to any particular message: a disabled cell used to hover
  // byte-identically to an editable one, which is the whole "why can't I click
  // this?" dead end.
  it('says something on hover that an editable cell does not', async () => {
    async function tooltipTextFor(disabled: boolean): Promise<string> {
      const { unmount } = render(
        <RuntimeMatrix
          t={t}
          specs={specs}
          pairs={[]}
          onToggle={vi.fn()}
          budgets={{}}
          disabled={disabled}
          disabledReason={disabled ? t.runtimeMatrixDisabledFileMode : undefined}
        />,
      );
      fireEvent.mouseOver(cell('Bravo', 'Alpha'));
      const text = (await screen.findByRole('tooltip')).textContent ?? '';
      unmount();
      return text;
    }

    const editable = await tooltipTextFor(false);
    const locked = await tooltipTextFor(true);
    expect(locked).not.toBe(editable);
    expect(locked.length).toBeGreaterThan(editable.length);
    // The extra content is a prefix, i.e. the reason leads.
    expect(locked.endsWith(editable)).toBe(true);
  });

  it('carries no disabled reason while the matrix is live', async () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    fireEvent.mouseOver(cell('Bravo', 'Alpha'));
    const tip = await screen.findByRole('tooltip');
    expect(tip).not.toHaveTextContent(t.runtimeMatrixDisabledFileMode);
    expect(tip).not.toHaveTextContent(t.runtimeMatrixDisabledSaving);
  });
});

describe('RuntimeMatrix advisory tooltip', () => {
  // The wording defect: the operator's question is "may these two run
  // together?" and the tooltip answered a different one first.
  it('leads with the consequence of leaving the pair off, VRAM second', async () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    // Bravo and Charlie are GPU-disjoint -- the exact pair whose old tooltip
    // opened with "no shared GPU", which next to a crossed circle read as the
    // reason the pair was refused.
    fireEvent.mouseOver(cell('Charlie', 'Bravo'));
    const tip = await screen.findByRole('tooltip');
    const text = tip.textContent ?? '';

    // Asserted FIRST and deliberately in terms of a pre-existing key only:
    // this fails purely on ORDER, so it discriminates against the old tooltip
    // (where the no-shared-GPU line came first) without depending on the new
    // message existing at all.
    expect(text.indexOf(t.runtimeMatrixNoSharedGpu)).toBeGreaterThan(0);
    // ...and what precedes it is the consequence.
    expect(text.indexOf(t.runtimeMatrixConsequence)).toBe(0);
    expect(text.indexOf(t.runtimeMatrixConsequence)).toBeLessThan(
      text.indexOf(t.runtimeMatrixNoSharedGpu),
    );
  });

  it('names the rule that surprised a real operator: the matrix applies regardless of GPUs', async () => {
    // Not a wording assertion for its own sake. Rule 1 is checked over EVERY
    // running spec, disjoint cards included, so "separate GPUs need no
    // permission" is a reasonable reading and wrong -- and the tooltip is
    // where the operator was standing when it cost them.
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    fireEvent.mouseOver(cell('Charlie', 'Bravo'));
    const tip = await screen.findByRole('tooltip');
    expect(tip).toHaveTextContent(t.runtimeMatrixConsequence);
    for (const locale of ['de', 'en'] as const) {
      expect(messages[locale].runtimeMatrixConsequence).toMatch(/GPU/);
    }
  });

  it('shows the summed VRAM per shared GPU and flags an over-budget sum', async () => {
    // Alpha (GPU0: 20000) + Bravo (GPU0: 24000) = 44000, budget 46000: fine.
    render(
      <RuntimeMatrix
        t={t}
        specs={specs}
        pairs={[]}
        onToggle={vi.fn()}
        budgets={{ 0: 46000, 1: 4000 }}
      />,
    );
    fireEvent.mouseOver(cell('Bravo', 'Alpha'));
    expect(await screen.findByText('GPU 0: 44000 / 46000 MB')).toBeInTheDocument();

    // Charlie (GPU1: 5000) + Alpha (GPU1: 2500) = 7500, budget 4000 on GPU1
    // -- over budget.
    fireEvent.mouseOver(cell('Charlie', 'Alpha'));
    expect(
      await screen.findByText(`GPU 1: 7500 / 4000 MB (${t.runtimeMatrixOverBudget})`),
    ).toBeInTheDocument();
  });

  it('renders a budget of 0 exactly like an absent row, never as over budget', async () => {
    // A budget of 0 means "no budget for this GPU" = unconstrained, the same
    // as a GPU with no budget row (see the comment on MatrixTooltip). This
    // tooltip predicts what the agent will do, so a 0 must render as the bare
    // sum -- not "44000 / 0 MB (over budget)", a limit the agent's admission
    // policy does not enforce. 0 is reachable: a fresh budget row starts at 0
    // MB in the limits form, and clearing the MB field yields 0.
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{ 0: 0 }} />);
    fireEvent.mouseOver(cell('Bravo', 'Alpha'));
    expect(await screen.findByText('GPU 0: 44000 MB')).toBeInTheDocument();
    expect(screen.queryByText(/\/ 0 MB/)).not.toBeInTheDocument();
    expect(screen.queryByText(new RegExp(t.runtimeMatrixOverBudget))).not.toBeInTheDocument();
  });

  it('says something sensible for two specs that share no GPU', async () => {
    // Bravo touches GPU0 only, Charlie touches GPU1 only -- disjoint.
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    fireEvent.mouseOver(cell('Charlie', 'Bravo'));
    expect(await screen.findByText(t.runtimeMatrixNoSharedGpu)).toBeInTheDocument();
  });

  it('always includes the advisory note (the matrix is intent, not the veto)', async () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    fireEvent.mouseOver(cell('Bravo', 'Alpha'));
    expect(await screen.findByText(t.runtimeMatrixAdvisory)).toBeInTheDocument();
  });
});

describe('RuntimeMatrix disabled (file-mode read-only)', () => {
  it('disables every cell and never calls onToggle on click', () => {
    const onToggle = vi.fn();
    render(
      <RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={onToggle} budgets={{}} disabled />,
    );
    for (const [row, col] of [
      ['Bravo', 'Alpha'],
      ['Charlie', 'Alpha'],
      ['Charlie', 'Bravo'],
    ]) {
      const c = cell(row, col);
      expect(c).toBeDisabled();
      fireEvent.click(c);
    }
    expect(onToggle).not.toHaveBeenCalled();
  });
});

// Long model names plus a column per spec is a width problem the lower
// triangle makes worse with every spec added; a dozen models would scroll the
// headers out of view while the operator hunts for a cell.
describe('RuntimeMatrix rotated column headers', () => {
  function headerCells(container: HTMLElement): HTMLElement[] {
    // [corner, ...column headers]
    return Array.from(container.querySelectorAll('thead th'));
  }

  function rotatedLabel(container: HTMLElement, index: number): HTMLElement {
    const th = headerCells(container)[index + 1];
    const span = th.querySelector('span');
    if (span === null) throw new Error(`column header ${index} has no label element`);
    return span as HTMLElement;
  }

  it('rotates only the column headers, bottom-to-top, and leaves the corner alone', () => {
    const { container } = render(
      <RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );

    // vertical-rl flows text top-to-bottom; the extra 180° turn is what makes
    // it read bottom-to-top, i.e. the START of the name next to the cells it
    // labels. Both halves are load-bearing: dropping the rotate leaves the
    // labels upside down relative to the convention, and it is also what puts
    // a wrapped name's first line on the LEFT (measured), so a two-line header
    // still reads in order once your head is tilted the way the rotation
    // already assumes.
    for (let i = 0; i < 2; i++) {
      const style = getComputedStyle(rotatedLabel(container, i));
      expect(style.writingMode).toBe('vertical-rl');
      expect(style.transform).toBe('rotate(180deg)');
    }

    // The corner carries no rotated label at all.
    expect(headerCells(container)[0].querySelector('span')).toBeNull();
    // Row labels stay horizontal.
    const rowLabel = container.querySelector('tbody th');
    expect(rowLabel?.textContent).toBe('Bravo');
    expect(getComputedStyle(rowLabel as HTMLElement).writingMode).not.toBe('vertical-rl');
  });

  it('keeps each model name as plain text in its header, so screen readers and getByText still find it', () => {
    const { container } = render(
      <RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    // The rotation is CSS over intact text -- no images, no per-character
    // markup, which is what would have broken this, the column-header
    // <th scope="col"> association and the cells' aria-labels alike.
    // Alpha and Bravo are the columns (specs[0..n-2]).
    expect(
      headerCells(container)
        .slice(1)
        .map((th) => th.textContent),
    ).toEqual(['Alpha', 'Bravo']);
    expect(rotatedLabel(container, 0).textContent).toBe('Alpha');
    expect(rotatedLabel(container, 1).textContent).toBe('Bravo');
    // Reachable by text query, not only by DOM walk. 'Bravo' legitimately
    // appears twice (column header AND row label, since specs[1..n-1] are the
    // rows) -- Charlie is a row only, and never a column.
    expect(screen.getByText('Alpha')).toBe(rotatedLabel(container, 0));
    expect(screen.getAllByText('Bravo')).toHaveLength(2);
    expect(screen.getAllByText('Bravo')).toContain(rotatedLabel(container, 1));
  });

  it('caps the header inline size by WRAPPING, never by dropping characters', async () => {
    const longName = 'Qwen2.5-Coder-32B-Instruct-AWQ-long-enough-to-truncate';
    // 'Zulu', not 'Bravo': the specs are name-sorted now, so the partner has
    // to collate AFTER the long name for the long one to stay column 0. That
    // makes `rotatedLabel(container, 0)` below double as proof the sort ran.
    const longSpecs: RuntimeMatrixSpec[] = [
      { id: 'spec-a', model: longName, gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-b', model: 'Zulu', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const { container } = render(
      <RuntimeMatrix t={t} specs={longSpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );

    // These four assertions REPLACE three that shipped green and encoded a
    // defect. `writing-mode: vertical-rl` makes the inline axis vertical, so
    // `text-overflow: ellipsis` clipped at the inline END = the end of the
    // string; `rotate(180deg)` only moved the ellipsis GLYPH away from the
    // grid. The characters it dropped were the quantisation/context suffix --
    // the only difference between two builds of one model, which the name sort
    // now places in ADJACENT columns. Measured in Chromium at the shipped
    // 160px cap, `Qwen3-Coder-30B-A3B-Instruct-UD-Q4_K_XL` and
    // `...-UD-Q8_0` both rendered as `Qwen3-Coder-30B-A3…`.
    //
    // NOTE, honestly: jsdom does no layout, so this asserts the DECLARATIONS
    // that produce wrapping, not the resulting pixels. The two negatives are
    // the only thing here that fails if a CSS ellipsis is reintroduced; the
    // textContent assertions below (and the collision guard further down) are
    // the only thing that fails if a JS `slice()` is.
    const label = rotatedLabel(container, 0);
    const style = getComputedStyle(label);
    expect(style.maxHeight).toBe('160px');
    expect(style.whiteSpace).toBe('normal');
    expect(style.overflowWrap).toBe('anywhere');
    expect(style.textOverflow).not.toBe('ellipsis');
    expect(style.overflow).not.toBe('hidden');

    // Wrapping only ever spends a second line: the element still holds the
    // full name and hovering still surfaces it. Before the hover the name
    // appears once (the header itself); after it, twice (header + tooltip).
    expect(label.textContent).toBe(longName);
    expect(screen.getAllByText(longName)).toHaveLength(1);
    fireEvent.mouseOver(label);
    expect(await screen.findByRole('tooltip')).toHaveTextContent(longName);
    expect(screen.getAllByText(longName)).toHaveLength(2);
  });

  it('keeps the header tooltip separate from the cell tooltip', async () => {
    const { container } = render(
      <RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    // The header's tooltip is the model name and nothing else; conflating it
    // with the cell's would bury the consequence sentence under column labels.
    fireEvent.mouseOver(rotatedLabel(container, 0));
    const headerTip = await screen.findByRole('tooltip');
    expect(headerTip).toHaveTextContent('Alpha');
    expect(headerTip).not.toHaveTextContent(t.runtimeMatrixConsequence);
    expect(headerTip).not.toHaveTextContent(t.runtimeMatrixAdvisory);
  });

  // jsdom performs no layout, so pixel alignment cannot be measured here.
  // What CAN be asserted -- and what alignment actually reduces to inside a
  // <table> -- is that cell k of every body row sits under column header k.
  // HTML table layout carries that to pixels; a rotation-plus-truncation
  // layout that drifts does so by breaking this correspondence (or by
  // abandoning the table box, which the writing-mode approach deliberately
  // does not).
  // `reversed` feeds the SAME specs in the opposite order. Every assertion
  // below is written against `many` (name order), so the reversed case only
  // passes if the component sorted -- and, crucially, sorted the array BOTH
  // slices are taken from. Sorting one slice on its own desynchronises header
  // k from cell k, and the aria-label check at the bottom (built out of the
  // RENDERED row and column labels) is what detects that.
  it.each([
    [2, false],
    [3, false],
    [6, false],
    [2, true],
    [3, true],
    [6, true],
  ])(
    'puts every body cell under the header of its own column (%i specs, reversed=%s)',
    (n, reversed) => {
      const many: RuntimeMatrixSpec[] = Array.from({ length: n as number }, (_, i) => ({
        id: `spec-${i}`,
        model: `Model-${i}`,
        gpus: [{ index: i, vramMb: 1000 }],
      }));
      const input = reversed ? [...many].reverse() : many;
      const { container } = render(
        <RuntimeMatrix t={t} specs={input} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
      );

      const headers = headerCells(container);
      // One corner + one header per column spec (specs[0..n-2]).
      expect(headers).toHaveLength(n as number);
      const columnModels = headers.slice(1).map((th) => th.textContent);
      expect(columnModels).toEqual(many.slice(0, -1).map((s) => s.model));

      const rows = Array.from(container.querySelectorAll('tbody tr'));
      expect(rows).toHaveLength(n - 1);
      rows.forEach((tr, rowOffset) => {
        const rowModel = tr.querySelector('th')?.textContent;
        expect(rowModel).toBe(many[rowOffset + 1].model);
        const dataCells = Array.from(tr.querySelectorAll('td'));
        // The lower triangle: row rowOffset draws rowOffset+1 cells.
        expect(dataCells).toHaveLength(rowOffset + 1);
        dataCells.forEach((td, columnIndex) => {
          const input = td.querySelector('input[type="checkbox"]');
          expect(input?.getAttribute('aria-label')).toBe(
            `${t.runtimeMatrixCell}: ${rowModel} + ${columnModels[columnIndex]}`,
          );
        });
      });
    },
  );
});

// ---------------------------------------------------------------------------
// Requirement B: the grid is ordered BY MODEL NAME, not by whatever the caller
// happened to hand in (live, the store's `order by id` over random hex).
//
// ORDER IS FULLY OBSERVABLE IN JSDOM -- DOM order of `thead th`, `tbody tr`,
// `tbody th` -- so every assertion in this block is behavioural and every one
// of them fails against the unsorted component.
// ---------------------------------------------------------------------------
describe('RuntimeMatrix name ordering', () => {
  function columnLabels(container: HTMLElement): (string | null)[] {
    // [corner, ...column headers] -- drop the corner.
    return Array.from(container.querySelectorAll('thead th'))
      .slice(1)
      .map((th) => th.textContent);
  }

  function rowLabels(container: HTMLElement): (string | null)[] {
    return Array.from(container.querySelectorAll('tbody th')).map((th) => th.textContent);
  }

  it('orders rows and columns by model name, not by id and not by caller order', () => {
    // Ids deliberately run OPPOSITE to the names, which also kills a "sort by
    // id" implementation: spec-z/spec-a/spec-m against Charlie/Alpha/Bravo.
    const unsorted: RuntimeMatrixSpec[] = [
      { id: 'spec-z', model: 'Charlie', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-a', model: 'Alpha', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-m', model: 'Bravo', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const { container } = render(
      <RuntimeMatrix t={t} specs={unsorted} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    // Columns are ordered[0..n-2], rows ordered[1..n-1].
    expect(columnLabels(container)).toEqual(['Alpha', 'Bravo']);
    expect(rowLabels(container)).toEqual(['Bravo', 'Charlie']);
  });

  it('does not mutate the caller’s array', () => {
    // Both call sites hand in a fresh `.map()` result today, so a
    // `specs.sort(...)` in the component would be a LATENT mutation bug --
    // exactly the kind that survives review. The `readonly` prop type is the
    // first guard (it makes that a compile error); this is the runtime one,
    // which still holds if the type is ever widened back: sorting a frozen
    // array throws TypeError in strict mode, and ES modules are strict.
    const unsorted: RuntimeMatrixSpec[] = [
      { id: 'spec-z', model: 'Charlie', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-a', model: 'Alpha', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const frozen = Object.freeze([...unsorted]);
    const { container } = render(
      <RuntimeMatrix t={t} specs={frozen} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    expect(columnLabels(container)).toEqual(['Alpha']);
    // The caller's array is still in its own order.
    expect(frozen.map((sp) => sp.model)).toEqual(['Charlie', 'Alpha']);
  });

  it('breaks a duplicate-model tie by id, so a re-fetch cannot swap the two rows', () => {
    // Two specs may legally carry the SAME model string in file mode (the
    // agent rejects duplicate spec IDs, never duplicate Models). With no
    // tie-break these two fall back to caller order via the stable sort, and
    // the live caller order is the store's `order by id` over random hex --
    // so a re-fetch could move the checkbox under the operator's cursor.
    // Both duplicates read 'Dup', so the labels cannot tell them apart. A
    // THIRD spec makes the tie observable: the FIRST body row's only cell
    // pairs 'Alpha' with whichever 'Dup' the comparator put first, and
    // canonicalPair does not hide that (spec-x sorts after both).
    const dup: RuntimeMatrixSpec[] = [
      { id: 'spec-b', model: 'Dup', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-a', model: 'Dup', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-x', model: 'Alpha', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const onToggle = vi.fn();
    const { container } = render(
      <RuntimeMatrix t={t} specs={dup} pairs={[]} onToggle={onToggle} budgets={{}} />,
    );
    expect(columnLabels(container)).toEqual(['Alpha', 'Dup']);
    expect(rowLabels(container)).toEqual(['Dup', 'Dup']);

    const firstRowCells = container.querySelectorAll('tbody tr')[0].querySelectorAll('input');
    expect(firstRowCells).toHaveLength(1);
    fireEvent.click(firstRowCells[0]);
    // spec-a, not spec-b: without the id tie-break the stable sort leaves
    // caller order and this is onToggle('spec-b', 'spec-x').
    expect(onToggle).toHaveBeenCalledWith('spec-a', 'spec-x');
  });

  it('sorts a size ladder numerically: 8B before 70B before 405B', () => {
    // The ONE assertion pinning the deliberate `{ numeric: true }` deviation
    // from the house's bare `localeCompare`. Bare collation compares '4' < '7'
    // < '8' and yields 405B, 70B, 8B -- which an operator reads as broken.
    const ladder: RuntimeMatrixSpec[] = [
      { id: 'spec-a', model: 'Llama-3.1-70B', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-b', model: 'Llama-3.1-405B', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-c', model: 'Llama-3.1-8B', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const { container } = render(
      <RuntimeMatrix t={t} specs={ladder} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    expect(columnLabels(container)).toEqual(['Llama-3.1-8B', 'Llama-3.1-70B']);
    expect(rowLabels(container)).toEqual(['Llama-3.1-70B', 'Llama-3.1-405B']);
  });

  it('leaves the WIRE order alone: onToggle still emits the id-sorted pair', () => {
    // The alphabetically-FIRST model carries the code-unit-GREATEST id, so
    // display order and wire order genuinely disagree here. Catches anyone
    // "helpfully" making canonicalPair follow the new display comparator --
    // the backend stores and compares pairs by raw id (SetCoResidency).
    // Passed in reverse display order too, so this fails on the ordering as
    // well as guarding the wire format.
    const crossed: RuntimeMatrixSpec[] = [
      { id: 'spec-a', model: 'Bravo', gpus: [{ index: 0, vramMb: 1000 }] },
      { id: 'spec-z', model: 'Alpha', gpus: [{ index: 0, vramMb: 1000 }] },
    ];
    const onToggle = vi.fn();
    const { container } = render(
      <RuntimeMatrix t={t} specs={crossed} pairs={[]} onToggle={onToggle} budgets={{}} />,
    );
    expect(columnLabels(container)).toEqual(['Alpha']);
    expect(rowLabels(container)).toEqual(['Bravo']);
    fireEvent.click(cell('Bravo', 'Alpha'));
    expect(onToggle).toHaveBeenCalledWith('spec-a', 'spec-z');
  });
});

// ---------------------------------------------------------------------------
// Requirement A: the row-label column is bounded.
//
// BE HONEST ABOUT WHAT THESE CAN CATCH. jsdom performs no layout, so
// offsetWidth/scrollWidth/getBoundingClientRect are all zero and NOTHING here
// proves the label actually wraps rather than overflowing (that was measured
// in Chromium; the numbers are in the commit message and the architecture
// doc). Two different guards, neither of which catches the other's mistake:
//   * the DECLARATION assertions -- emotion injects real stylesheets, so
//     getComputedStyle resolves them -- are the only thing that fails if a CSS
//     ellipsis is reintroduced;
//   * the textContent/uniqueness assertions are the only thing that fails if a
//     JS `slice(0, N) + '…'` is.
// ---------------------------------------------------------------------------
describe('RuntimeMatrix row-label bounding', () => {
  // One real quantisation family: a shared 34-character prefix, differing only
  // in the suffix. Listed here in the order they are PASSED IN (deliberately
  // not the display order); `familyOrdered` below is the order the grid must
  // put them in.
  const family = [
    'Qwen3-Coder-30B-A3B-Instruct-UD-Q4_K_XL',
    'Qwen3-Coder-30B-A3B-Instruct-UD-Q8_0',
    'Qwen3-Coder-30B-A3B-Instruct-UD-Q5_K_XL',
  ];
  const familyOrdered = [family[0], family[2], family[1]];

  // Ids deliberately in a different order from the names.
  const familySpecs: RuntimeMatrixSpec[] = [
    { id: 'spec-c', model: family[0], gpus: [{ index: 0, vramMb: 1000 }] },
    { id: 'spec-a', model: family[1], gpus: [{ index: 0, vramMb: 1000 }] },
    { id: 'spec-b', model: family[2], gpus: [{ index: 0, vramMb: 1000 }] },
  ];

  function rowLabelSpan(container: HTMLElement, index: number): HTMLElement {
    const th = Array.from(container.querySelectorAll('tbody th'))[index];
    const span = th.querySelector('span');
    if (span === null) throw new Error(`row label ${index} has no label element`);
    return span as HTMLElement;
  }

  it('bounds the label with a width budget it meets by wrapping, not by eliding', () => {
    const { container } = render(
      <RuntimeMatrix t={t} specs={familySpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    const style = getComputedStyle(rowLabelSpan(container, 0));
    // `display: block` is what makes the cap work at all -- `max-width` has no
    // effect on an inline box (measured: the column stayed at its full width).
    expect(style.display).toBe('block');
    expect(style.maxWidth).toBe('160px');
    // `anywhere`, not `break-word`: only `anywhere` lowers the box's
    // min-content size, which is what forces the table column down to the cap.
    expect(style.overflowWrap).toBe('anywhere');
    // The two load-bearing negatives. An ellipsis here would render two sorted
    // neighbours identically -- see the collision test below.
    expect(style.textOverflow).not.toBe('ellipsis');
    expect(style.whiteSpace).not.toBe('nowrap');
  });

  it('never renders two different model names as the same label, on either axis', () => {
    // THE test the repo did not have. Its existing corpus is 5-10 character
    // names plus one 54-character outlier with no prefix-sharing sibling, so
    // it could not detect this at all. These three are one real quantisation
    // family: they share a 34-character prefix, and the name sort guarantees
    // they are NEIGHBOURS. An end-ellipsis at the 160px budget renders the
    // first two identically (measured: both `Qwen3-Coder-30B-A3…`), in a grid
    // whose entire purpose is "may THESE TWO run together".
    const { container } = render(
      <RuntimeMatrix t={t} specs={familySpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );

    const rowText = Array.from(container.querySelectorAll('tbody th')).map((th) => th.textContent);
    const colText = Array.from(container.querySelectorAll('thead th'))
      .slice(1)
      .map((th) => th.textContent);

    // Full names, character for character -- no ellipsis, no slice.
    expect(colText).toEqual(familyOrdered.slice(0, -1));
    expect(rowText).toEqual(familyOrdered.slice(1));
    // ...and stated as a property, so it holds for any future fixture too.
    expect(new Set(rowText).size).toBe(rowText.length);
    expect(new Set(colText).size).toBe(colText.length);
  });

  it('lets the table shrink to its content, so the cap reaches the column', () => {
    // STRUCTURE PIN, NOT BEHAVIOUR -- say so plainly. jsdom does no layout, so
    // this asserts the declaration only. It is here because the declaration is
    // easy to delete as "redundant" and the consequence is invisible in every
    // test: MUI's Table is `width: 100%`, and under `table-layout: auto` a
    // 100%-wide table redistributes its surplus to the columns in proportion
    // to their content. The 160px cap on the span then still sat inside a
    // 491px cell (measured in Chromium, 5 specs at a 1100px viewport), with a
    // ~330px gutter between a name and its own first checkbox. Shrink-to-fit
    // makes the measured column 192px at any viewport.
    const { container } = render(
      <RuntimeMatrix t={t} specs={familySpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    const table = container.querySelector('table');
    expect(getComputedStyle(table as HTMLElement).width).toBe('auto');
    // ...and the enclosure still absorbs a matrix too wide for the page.
    expect(getComputedStyle(table?.parentElement as HTMLElement).overflowX).toBe('auto');
  });

  it('adds no tooltip and no hidden second copy to the row label', () => {
    // Nothing is hidden, so a tooltip repeating fully-visible text is noise.
    // This also guards the other tempting "fix": rendering a truncated visible
    // copy next to a full hidden one, which would double the text count.
    const { container } = render(
      <RuntimeMatrix t={t} specs={familySpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />,
    );
    // family[1] (…Q8_0) collates last, so it is a ROW and never a column --
    // exactly one occurrence in the whole grid.
    expect(screen.getAllByText(family[1])).toHaveLength(1);
    fireEvent.mouseOver(rowLabelSpan(container, 1));
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('keeps the full name as the accessible name of both the row and the column header', () => {
    // The <th> keeps its bare props, so wrapping the text in a span must not
    // cost it its rowheader/columnheader role or its accessible name -- and
    // because nothing is JS-truncated, rendered text and aria text stay
    // identical, which is what keeps the alignment invariant constructible.
    render(<RuntimeMatrix t={t} specs={familySpecs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    expect(screen.getByRole('rowheader', { name: familyOrdered[2] })).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: familyOrdered[0] })).toBeInTheDocument();
    // The cell still carries BOTH full names.
    expect(cell(familyOrdered[2], familyOrdered[0])).toBeInTheDocument();
  });
});
