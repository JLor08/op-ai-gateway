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

describe('RuntimeMatrix lower-triangle rendering', () => {
  it('renders exactly n(n-1)/2 cells for n specs', () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    const cells = screen.getAllByRole('button', {
      name: new RegExp(`^${t.runtimeMatrixCell}:`),
    });
    // n=3 -> n(n-1)/2 = 3: (Bravo,Alpha), (Charlie,Alpha), (Charlie,Bravo).
    expect(cells).toHaveLength(3);
    expect(
      screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Bravo` }),
    ).toBeInTheDocument();
    // Diagonal and upper-triangle cells are never drawn: neither Alpha's own
    // row nor an (Alpha, Bravo)/(Alpha, Charlie) cell (upper triangle) exists.
    expect(
      screen.queryByRole('button', { name: `${t.runtimeMatrixCell}: Alpha + Bravo` }),
    ).not.toBeInTheDocument();
  });

  it('shows a hint instead of a table when fewer than two specs are given', () => {
    render(<RuntimeMatrix t={t} specs={[specs[0]]} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    expect(screen.getByText(t.runtimeMatrixNeedTwo)).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});

describe('RuntimeMatrix canonicalisation', () => {
  it('emits the pair in canonical (sorted) order on toggle, regardless of visual row/column order', () => {
    const onToggle = vi.fn();
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={onToggle} budgets={{}} />);

    // Visual click is on the (Bravo, Alpha) cell -- row=Bravo (id spec-b),
    // col=Alpha (id spec-a). A naive pass-through would call
    // onToggle('spec-b', 'spec-a'); the canonical/sorted call is the reverse.
    fireEvent.click(screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` }));
    expect(onToggle).toHaveBeenCalledWith('spec-a', 'spec-b');

    fireEvent.click(
      screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Bravo` }),
    );
    expect(onToggle).toHaveBeenCalledWith('spec-b', 'spec-c');
  });

  it('renders an existing pair as allowed (aria-pressed) regardless of the order it is stored in', () => {
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
    const cell = screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` });
    expect(cell).toHaveAttribute('aria-pressed', 'true');
    const other = screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` });
    expect(other).toHaveAttribute('aria-pressed', 'false');
  });
});

describe('RuntimeMatrix advisory tooltip', () => {
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
    const bravoAlpha = screen.getByRole('button', {
      name: `${t.runtimeMatrixCell}: Bravo + Alpha`,
    });
    fireEvent.mouseOver(bravoAlpha);
    expect(await screen.findByText('GPU 0: 44000 / 46000 MB')).toBeInTheDocument();

    // Charlie (GPU1: 5000) + Alpha (GPU1: 2500) = 7500, budget 4000 on GPU1
    // -- over budget.
    const charlieAlpha = screen.getByRole('button', {
      name: `${t.runtimeMatrixCell}: Charlie + Alpha`,
    });
    fireEvent.mouseOver(charlieAlpha);
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
    const bravoAlpha = screen.getByRole('button', {
      name: `${t.runtimeMatrixCell}: Bravo + Alpha`,
    });
    fireEvent.mouseOver(bravoAlpha);
    expect(await screen.findByText('GPU 0: 44000 MB')).toBeInTheDocument();
    expect(screen.queryByText(/\/ 0 MB/)).not.toBeInTheDocument();
    expect(screen.queryByText(new RegExp(t.runtimeMatrixOverBudget))).not.toBeInTheDocument();
  });

  it('says something sensible for two specs that share no GPU', async () => {
    // Bravo touches GPU0 only, Charlie touches GPU1 only -- disjoint.
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    const cell = screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Bravo` });
    fireEvent.mouseOver(cell);
    expect(await screen.findByText(t.runtimeMatrixNoSharedGpu)).toBeInTheDocument();
  });

  it('always includes the advisory note (the matrix is intent, not the veto)', async () => {
    render(<RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={vi.fn()} budgets={{}} />);
    const cell = screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` });
    fireEvent.mouseOver(cell);
    expect(await screen.findByText(t.runtimeMatrixAdvisory)).toBeInTheDocument();
  });
});

describe('RuntimeMatrix disabled (file-mode read-only)', () => {
  it('disables every cell and never calls onToggle on click', () => {
    const onToggle = vi.fn();
    render(
      <RuntimeMatrix t={t} specs={specs} pairs={[]} onToggle={onToggle} budgets={{}} disabled />,
    );
    const cell = screen.getByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` });
    expect(cell).toBeDisabled();
    fireEvent.click(cell);
    expect(onToggle).not.toHaveBeenCalled();
  });
});
