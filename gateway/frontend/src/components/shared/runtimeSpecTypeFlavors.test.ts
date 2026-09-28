// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  flavorsAreImagesOnly,
  formSpecIsStableDiffusion,
  specFlavorsAfterTypeSwitch,
  stableDiffusionSpecFlavorDefaults,
  textFlavorsOnStableDiffusion,
  tickedFlavorMissingOnApplication,
  type RuntimeSpecFlavorValues,
} from './runtimeSpecTypeFlavors';

describe('flavorsAreImagesOnly', () => {
  it.each([
    [['openai_images'], true],
    [['openai_images', 'openai'], false],
    [['anthropic', 'openai_images'], false],
    [['openai', 'anthropic'], false],
    [[], false],
  ])('%j -> %s', (flavors, expected) => {
    expect(flavorsAreImagesOnly(flavors)).toBe(expected);
  });
});

describe('tickedFlavorMissingOnApplication', () => {
  it.each([
    [['openai_images'], ['openai', 'anthropic'], true],
    [['openai', 'openai_images'], ['openai', 'openai_images'], false],
    [['anthropic'], ['openai'], true],
    [['openai'], ['openai', 'anthropic', 'openai_images'], false],
    [[], ['openai'], false],
  ])('ticked %j under %j -> %s', (ticked, application, expected) => {
    expect(tickedFlavorMissingOnApplication(ticked, application)).toBe(expected);
  });
});

describe('formSpecIsStableDiffusion', () => {
  const loadedSd = {
    type: '' as const,
    binary: '/opt/sd/sd-server',
    effective_type: 'stable_diffusion_cpp',
  };

  it('is true for the explicit type, loaded or not', () => {
    expect(formSpecIsStableDiffusion('stable_diffusion_cpp', '/x/server', null)).toBe(true);
    expect(formSpecIsStableDiffusion('stable_diffusion_cpp', '/x/server', loadedSd)).toBe(true);
  });

  it('is true under Auto while the binary is the one the loaded sd spec resolved from', () => {
    expect(formSpecIsStableDiffusion('', '/opt/sd/sd-server', loadedSd)).toBe(true);
    expect(formSpecIsStableDiffusion('', '/opt/sd/sd-server ', loadedSd)).toBe(true);
  });

  it('is false under Auto once the binary changed, since the detection is Go-only', () => {
    expect(formSpecIsStableDiffusion('', '/opt/sd/sd-server-v2', loadedSd)).toBe(false);
  });

  it('is false under Auto on Create and for a loaded spec of another type', () => {
    expect(formSpecIsStableDiffusion('', '/opt/sd/sd-server', null)).toBe(false);
    expect(
      formSpecIsStableDiffusion('', '/usr/bin/llama-server', {
        type: '',
        binary: '/usr/bin/llama-server',
        effective_type: 'llama_cpp',
      }),
    ).toBe(false);
  });

  it('is false under Auto for a loaded spec whose sd type came from its explicit Type', () => {
    expect(
      formSpecIsStableDiffusion('', '/usr/local/bin/flux-wrapper.sh', {
        type: 'stable_diffusion_cpp',
        binary: '/usr/local/bin/flux-wrapper.sh',
        effective_type: 'stable_diffusion_cpp',
      }),
    ).toBe(false);
  });

  it('is false for an explicit non-sd type, whatever the loaded spec resolved to', () => {
    expect(formSpecIsStableDiffusion('llama_cpp', '/opt/sd/sd-server', loadedSd)).toBe(false);
  });
});

describe('textFlavorsOnStableDiffusion', () => {
  it.each([
    [['openai', 'anthropic'], true, true],
    [['openai_images', 'openai'], true, true],
    [[], true, true],
    [['openai_images'], true, false],
    [['openai', 'anthropic'], false, false],
  ])('ticked %j, sd %s -> %s', (ticked, isSd, expected) => {
    expect(textFlavorsOnStableDiffusion(ticked, isSd)).toBe(expected);
  });
});

describe('specFlavorsAfterTypeSwitch', () => {
  const template: RuntimeSpecFlavorValues = {
    apiFlavors: ['openai', 'anthropic'],
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
  };
  const sdDefault: RuntimeSpecFlavorValues = {
    apiFlavors: ['openai_images'],
    responsesMode: 'disabled',
    messagesMode: 'disabled',
  };

  it('is the sd default: openai_images with both modes disabled', () => {
    expect(stableDiffusionSpecFlavorDefaults).toEqual(sdDefault);
  });

  it('sets the sd default and remembers the values when switching to sd untouched', () => {
    const current: RuntimeSpecFlavorValues = {
      apiFlavors: ['anthropic', 'openai'],
      responsesMode: 'translate',
      messagesMode: 'passthrough',
    };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'llama_cpp',
        to: 'stable_diffusion_cpp',
        current,
        template,
        remembered: null,
      }),
    ).toEqual({ values: sdDefault, remembered: current });
  });

  it('moves nothing when switching to sd with flavors other than the template', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: '',
        to: 'stable_diffusion_cpp',
        current: { ...template, apiFlavors: ['openai'] },
        template,
        remembered: null,
      }),
    ).toBeNull();
  });

  it('restores the remembered values when switching from sd to an explicit type untouched', () => {
    const remembered: RuntimeSpecFlavorValues = {
      apiFlavors: ['openai', 'anthropic'],
      responsesMode: 'translate',
      messagesMode: 'passthrough',
    };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'vllm',
        current: sdDefault,
        template,
        remembered,
      }),
    ).toEqual({ values: remembered, remembered: null });
  });

  it('restores the template when nothing is remembered (a spec loaded as sd)', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'llama_cpp',
        current: sdDefault,
        template,
        remembered: null,
      }),
    ).toEqual({ values: template, remembered: null });
  });

  it('moves nothing when switching from sd with values other than the sd default', () => {
    const remembered = template;
    for (const current of [
      { ...sdDefault, apiFlavors: ['openai_images', 'openai'] },
      { ...sdDefault, messagesMode: 'passthrough' as const },
      { ...sdDefault, responsesMode: 'translate' as const },
    ]) {
      expect(
        specFlavorsAfterTypeSwitch({
          from: 'stable_diffusion_cpp',
          to: 'llama_cpp',
          current,
          template,
          remembered,
        }),
      ).toBeNull();
    }
  });

  it('moves nothing on a switch to Auto', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: '',
        current: sdDefault,
        template,
        remembered: template,
      }),
    ).toBeNull();
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'llama_cpp',
        to: '',
        current: template,
        template,
        remembered: null,
      }),
    ).toBeNull();
  });

  it('moves nothing between two non-sd types, even on values equal to the sd default', () => {
    for (const current of [template, sdDefault]) {
      expect(
        specFlavorsAfterTypeSwitch({
          from: 'llama_cpp',
          to: 'vllm',
          current,
          template,
          remembered: template,
        }),
      ).toBeNull();
    }
  });

  it('moves nothing when the type does not change', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'stable_diffusion_cpp',
        current: template,
        template,
        remembered: null,
      }),
    ).toBeNull();
  });

  it('returns copies, never the remembered or default arrays', () => {
    const remembered: RuntimeSpecFlavorValues = { ...template, apiFlavors: ['openai'] };
    const back = specFlavorsAfterTypeSwitch({
      from: 'stable_diffusion_cpp',
      to: 'vllm',
      current: sdDefault,
      template,
      remembered,
    });
    expect(back?.values.apiFlavors).not.toBe(remembered.apiFlavors);
    const toSd = specFlavorsAfterTypeSwitch({
      from: '',
      to: 'stable_diffusion_cpp',
      current: template,
      template,
      remembered: null,
    });
    expect(toSd?.values.apiFlavors).not.toBe(stableDiffusionSpecFlavorDefaults.apiFlavors);
  });
});
