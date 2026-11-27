// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { GpuVendorNote } from './GpuVendorNote';
import { messages } from '../i18n';

const t = messages.en;

describe('GpuVendorNote', () => {
  it('names the vendor and hides itself once the dismissal succeeds', async () => {
    const onDismiss = vi.fn(async () => undefined);
    render(<GpuVendorNote t={t} vendor="amd" onDismiss={onDismiss} />);
    expect(screen.getByText(t.gpuLegacyMappedNote('AMD'))).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.gpuLegacyMappedDismiss }));
    await waitFor(() =>
      expect(screen.queryByText(t.gpuLegacyMappedNote('AMD'))).not.toBeInTheDocument(),
    );
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it('stays and reports the error when the dismissal fails', async () => {
    const failure = new Error('nope');
    const onError = vi.fn();
    render(
      <GpuVendorNote
        t={t}
        vendor="nvidia"
        onDismiss={vi.fn(async () => Promise.reject(failure))}
        onError={onError}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: t.gpuLegacyMappedDismiss }));
    await waitFor(() => expect(onError).toHaveBeenCalledWith(failure));
    expect(screen.getByText(t.gpuLegacyMappedNote('NVIDIA'))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.gpuLegacyMappedDismiss })).toBeEnabled();
  });

  it('renders nothing without a vendor to name', () => {
    const { container } = render(<GpuVendorNote t={t} vendor="" onDismiss={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });
});
