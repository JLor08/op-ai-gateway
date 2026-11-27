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

// The vendor the legacy empty vendor stands for on a host: the server's stored
// legacy vendor (the one its vendorless rows were mapped to), else the only
// vendor its hardware report lists, else '' (no report, a vendorless report
// from an older agent, or a mixed host with no legacy vendor stored).
export function hostGpuVendor(legacy: GpuVendor | undefined, gpus: HardwareGPU[]): GpuVendor {
  if (legacy) return legacy;
  const vendors = reportVendors(gpus);
  return vendors.length === 1 ? vendors[0] : '';
}

// gpuKey with the empty vendor read as hostVendor (hostGpuVendor): a legacy row
// and a vendor-qualified one at the same index name one card when the host's
// vendor is that vendor. Matching rows by this key keeps a spec saved before
// vendors existed lined up with a newer agent's vendor-qualified telemetry.
export function resolvedGpuKey(
  vendor: GpuVendor | undefined,
  index: number,
  hostVendor: GpuVendor,
): string {
  return gpuKey(vendor || hostVendor, index);
}

// Mirror of the backend/agent backend resolution (resolveSpecBackend): auto
// follows the GPUs' own vendors; an explicit backend that does not fit them is
// a mismatch. vulkan accepts any vendor; sycl cannot span nvidia and amd.
// '' entries (legacy rows of an older agent or a vendorless report) carry no
// vendor to judge by and are ignored, so they never force vulkan or a mismatch.
export function resolveAutoBackend(
  backend: DeviceBackend | '',
  allVendors: GpuVendor[],
): { resolved: DeviceBackend | null; mismatch: boolean } {
  const vendors: GpuVendor[] = allVendors.filter((v) => v !== '');
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
