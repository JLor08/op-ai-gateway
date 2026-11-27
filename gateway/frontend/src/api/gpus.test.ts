// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi } from '../api';
import {
  gpuDisplayName,
  gpuKey,
  hostGpuVendor,
  hostIsMixed,
  reportVendors,
  resolvedGpuKey,
  resolveAutoBackend,
  vendorLabel,
} from './gpus';
import type { HardwareGPU } from './servers';

const gpu = (vendor: HardwareGPU['vendor']): HardwareGPU => ({ index: 0, vendor }) as HardwareGPU;

describe('gpus', () => {
  it('builds keys and display names', () => {
    expect(gpuKey('amd', 1)).toBe('amd:1');
    expect(gpuKey(undefined, 1)).toBe(':1');
    expect(gpuDisplayName('nvidia', 0)).toBe('NVIDIA 0');
    expect(gpuDisplayName('apple', 0)).toBe('Apple 0');
    expect(gpuDisplayName('', 2)).toBe('GPU 2');
    expect(gpuDisplayName(undefined, 2)).toBe('GPU 2');
    expect(vendorLabel('amd')).toBe('AMD');
    expect(vendorLabel('')).toBe('');
  });

  it('lists distinct vendors in a fixed order', () => {
    expect(reportVendors([gpu('amd'), gpu('nvidia'), gpu('amd'), gpu(''), gpu(undefined)])).toEqual(
      ['nvidia', 'amd'],
    );
    expect(reportVendors([])).toEqual([]);
  });

  it('detects a mixed host', () => {
    expect(hostIsMixed([gpu('nvidia'), gpu('amd')])).toBe(true);
    expect(hostIsMixed([gpu('amd'), gpu('amd')])).toBe(false);
    expect(hostIsMixed([])).toBe(false);
  });

  it('reads the empty vendor as the host vendor', () => {
    expect(hostGpuVendor('amd', [gpu('nvidia')])).toBe('amd');
    expect(hostGpuVendor(undefined, [gpu('nvidia'), gpu('nvidia')])).toBe('nvidia');
    expect(hostGpuVendor('', [gpu('nvidia'), gpu('amd')])).toBe('');
    expect(hostGpuVendor(undefined, [gpu(undefined)])).toBe('');
    expect(resolvedGpuKey('', 1, 'nvidia')).toBe('nvidia:1');
    expect(resolvedGpuKey(undefined, 1, '')).toBe(':1');
    expect(resolvedGpuKey('amd', 1, 'nvidia')).toBe('amd:1');
  });

  it('resolves auto by the GPUs vendors', () => {
    expect(resolveAutoBackend('auto', ['nvidia'])).toEqual({ resolved: 'cuda', mismatch: false });
    expect(resolveAutoBackend('', ['amd'])).toEqual({ resolved: 'rocm', mismatch: false });
    expect(resolveAutoBackend('auto', ['apple'])).toEqual({ resolved: 'metal', mismatch: false });
    expect(resolveAutoBackend('auto', ['nvidia', 'amd'])).toEqual({
      resolved: 'vulkan',
      mismatch: false,
    });
    expect(resolveAutoBackend('auto', [])).toEqual({ resolved: null, mismatch: false });
  });

  it('flags explicit backends that do not fit the vendors', () => {
    expect(resolveAutoBackend('cuda', ['nvidia'])).toEqual({ resolved: 'cuda', mismatch: false });
    expect(resolveAutoBackend('cuda', ['nvidia', 'amd'])).toEqual({
      resolved: 'cuda',
      mismatch: true,
    });
    expect(resolveAutoBackend('rocm', ['nvidia'])).toEqual({ resolved: 'rocm', mismatch: true });
    expect(resolveAutoBackend('rocm', ['amd'])).toEqual({ resolved: 'rocm', mismatch: false });
    expect(resolveAutoBackend('metal', ['apple'])).toEqual({ resolved: 'metal', mismatch: false });
    expect(resolveAutoBackend('metal', ['nvidia'])).toEqual({ resolved: 'metal', mismatch: true });
    expect(resolveAutoBackend('vulkan', ['nvidia', 'amd'])).toEqual({
      resolved: 'vulkan',
      mismatch: false,
    });
    expect(resolveAutoBackend('sycl', ['nvidia', 'amd'])).toEqual({
      resolved: 'sycl',
      mismatch: true,
    });
    expect(resolveAutoBackend('sycl', ['nvidia'])).toEqual({ resolved: 'sycl', mismatch: false });
    expect(resolveAutoBackend('sycl', ['amd'])).toEqual({ resolved: 'sycl', mismatch: false });
  });

  it('ignores legacy vendorless entries', () => {
    expect(resolveAutoBackend('auto', ['nvidia', ''])).toEqual({
      resolved: 'cuda',
      mismatch: false,
    });
    expect(resolveAutoBackend('auto', [''])).toEqual({ resolved: null, mismatch: false });
    expect(resolveAutoBackend('cuda', [''])).toEqual({ resolved: 'cuda', mismatch: false });
    expect(resolveAutoBackend('rocm', ['amd', ''])).toEqual({ resolved: 'rocm', mismatch: false });
  });
});

describe('serversApi GPU vendor note', () => {
  it('POSTs the dismissal for the URL-encoded server id and accepts 204', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    await expect(createPortalApi(fetcher).dismissGpuVendorNote('srv/1')).resolves.toBeUndefined();
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv%2F1/gpu-vendor-note/dismiss');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });
});
