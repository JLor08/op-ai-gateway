// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { DeviceBackend } from './runtime';
import type { HardwareGPU } from './servers';

// A GPU's vendor as the backend reports it. '' is the legacy "the host's only
// vendor" value carried by rows written before vendors existed.
export type GpuVendor = 'nvidia' | 'amd' | 'apple' | '';

const vendorOrder: GpuVendor[] = ['nvidia', 'amd', 'apple'];

// The identity of a GPU: indexes restart per vendor, so (vendor, index) is the
// key. An absent vendor is the legacy empty one.
export function gpuKey(vendor: GpuVendor | undefined, index: number): string {
  return `${vendor ?? ''}:${index}`;
}

// Brand names are not translated.
export function vendorLabel(vendor: GpuVendor): string {
  switch (vendor) {
    case 'nvidia':
      return 'NVIDIA';
    case 'amd':
      return 'AMD';
    case 'apple':
      return 'Apple';
    default:
      return '';
  }
}

// How a human reads a GPU: "NVIDIA 0", "AMD 1"; "GPU 1" without a vendor.
export function gpuDisplayName(vendor: GpuVendor | undefined, index: number): string {
  const label = vendorLabel(vendor ?? '');
  return `${label || 'GPU'} ${index}`;
}

// The distinct vendors of a hardware report, in the order nvidia, amd, apple.
// GPUs without a vendor contribute nothing.
export function reportVendors(gpus: HardwareGPU[]): GpuVendor[] {
  const seen = new Set<GpuVendor>();
  for (const g of gpus) seen.add(g.vendor ?? '');
  return vendorOrder.filter((v) => seen.has(v));
}

export function hostIsMixed(gpus: HardwareGPU[]): boolean {
  return reportVendors(gpus).length > 1;
}

// Mirror of the backend/agent backend resolution (resolveSpecBackend): auto
// follows the GPUs' own vendors; an explicit backend that does not fit them is
// a mismatch. vulkan accepts any vendor; sycl cannot span nvidia and amd.
export function resolveAutoBackend(
  backend: DeviceBackend | '',
  vendors: GpuVendor[],
): { resolved: DeviceBackend | null; mismatch: boolean } {
  const has = (v: GpuVendor) => vendors.includes(v);
  const only = (v: GpuVendor) => vendors.length > 0 && vendors.every((x) => x === v);
  if (backend === '' || backend === 'auto') {
    if (vendors.length === 0) return { resolved: null, mismatch: false };
    if (only('nvidia')) return { resolved: 'cuda', mismatch: false };
    if (only('amd')) return { resolved: 'rocm', mismatch: false };
    if (only('apple')) return { resolved: 'metal', mismatch: false };
    return { resolved: 'vulkan', mismatch: false };
  }
  switch (backend) {
    case 'cuda':
      return { resolved: backend, mismatch: vendors.some((v) => v !== 'nvidia') };
    case 'rocm':
      return { resolved: backend, mismatch: vendors.some((v) => v !== 'amd') };
    case 'metal':
      return { resolved: backend, mismatch: vendors.some((v) => v !== 'apple') };
    case 'sycl':
      return { resolved: backend, mismatch: has('nvidia') && has('amd') };
    default:
      return { resolved: backend, mismatch: false };
  }
}
