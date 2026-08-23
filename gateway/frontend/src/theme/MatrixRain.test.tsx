// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { MatrixRain } from './MatrixRain';

describe('MatrixRain', () => {
  it('renders an aria-hidden canvas and does not crash without a 2D context', () => {
    // jsdom's HTMLCanvasElement.getContext returns null; the component must bail gracefully.
    const { container } = render(<MatrixRain />);
    const canvas = container.querySelector('canvas');
    expect(canvas).not.toBeNull();
    expect(canvas?.getAttribute('aria-hidden')).toBe('true');
  });
});
