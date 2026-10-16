// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  audioCppSpecFlavorDefaults,
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

// audio_cpp is the second kind whose spec form defaults the API-variant block
// (speech only, both coding-agent modes disabled), under the same preservation
// contract as stable_diffusion_cpp: only values the form put there move.
describe('specFlavorsAfterTypeSwitch for audio_cpp', () => {
  const template: RuntimeSpecFlavorValues = {
    apiFlavors: ['openai', 'anthropic'],
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
  };
  const audioDefault: RuntimeSpecFlavorValues = {
    apiFlavors: ['openai_audio'],
    responsesMode: 'disabled',
    messagesMode: 'disabled',
  };
  const sdDefault: RuntimeSpecFlavorValues = {
    apiFlavors: ['openai_images'],
    responsesMode: 'disabled',
    messagesMode: 'disabled',
  };

  it('is the audio default: openai_audio with both modes disabled', () => {
    expect(audioCppSpecFlavorDefaults).toEqual(audioDefault);
  });

  it('sets the audio default and remembers the values when switching to audio_cpp untouched', () => {
    const current: RuntimeSpecFlavorValues = {
      apiFlavors: ['anthropic', 'openai'],
      responsesMode: 'translate',
      messagesMode: 'passthrough',
    };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'llama_cpp',
        to: 'audio_cpp',
        current,
        template,
        remembered: null,
      }),
    ).toEqual({ values: audioDefault, remembered: current });
  });

  it('moves nothing when switching to audio_cpp with flavors other than the template', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: '',
        to: 'audio_cpp',
        current: { ...template, apiFlavors: ['openai'] },
        template,
        remembered: null,
      }),
    ).toBeNull();
  });

  it('restores the remembered values when switching from audio_cpp to an explicit type untouched', () => {
    const remembered: RuntimeSpecFlavorValues = { ...template, responsesMode: 'translate' };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'audio_cpp',
        to: 'vllm',
        current: audioDefault,
        template,
        remembered,
      }),
    ).toEqual({ values: remembered, remembered: null });
  });

  it('restores the template when nothing is remembered (a spec loaded as audio_cpp)', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'audio_cpp',
        to: 'llama_cpp',
        current: audioDefault,
        template,
        remembered: null,
      }),
    ).toEqual({ values: template, remembered: null });
  });

  it('moves nothing when switching from audio_cpp with values other than the audio default', () => {
    for (const current of [
      { ...audioDefault, apiFlavors: ['openai_audio', 'openai'] },
      { ...audioDefault, messagesMode: 'passthrough' as const },
      { ...audioDefault, responsesMode: 'translate' as const },
    ]) {
      expect(
        specFlavorsAfterTypeSwitch({
          from: 'audio_cpp',
          to: 'llama_cpp',
          current,
          template,
          remembered: template,
        }),
      ).toBeNull();
    }
  });

  it('moves nothing on a switch to Auto, and nothing when the type does not change', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'audio_cpp',
        to: '',
        current: audioDefault,
        template,
        remembered: template,
      }),
    ).toBeNull();
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'audio_cpp',
        to: 'audio_cpp',
        current: template,
        template,
        remembered: null,
      }),
    ).toBeNull();
  });

  // Between the two flavor-defaulting kinds the replaced values must survive
  // the hop: the remembered slot keeps what the operator had BEFORE the first
  // default, so the trip back to an ordinary type still restores it.
  it('hops from the sd default to the audio default without forgetting the original values', () => {
    const original: RuntimeSpecFlavorValues = { ...template, responsesMode: 'translate' };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'audio_cpp',
        current: sdDefault,
        template,
        remembered: original,
      }),
    ).toEqual({ values: audioDefault, remembered: original });
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'audio_cpp',
        to: 'stable_diffusion_cpp',
        current: audioDefault,
        template,
        remembered: original,
      }),
    ).toEqual({ values: sdDefault, remembered: original });
  });

  it('moves nothing on a hop between the two kinds when the values are not the default', () => {
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'audio_cpp',
        current: { ...sdDefault, apiFlavors: ['openai_images', 'openai'] },
        template,
        remembered: template,
      }),
    ).toBeNull();
  });

  it("does not remember the sd default as the operator's values when the parent is images-only", () => {
    // The template's flavors equal the sd default's here, so judging the hop
    // against the template would remember the sd default itself; the hop is
    // judged against the kind it leaves first.
    const imagesTemplate: RuntimeSpecFlavorValues = { ...sdDefault };
    expect(
      specFlavorsAfterTypeSwitch({
        from: 'stable_diffusion_cpp',
        to: 'audio_cpp',
        current: sdDefault,
        template: imagesTemplate,
        remembered: null,
      }),
    ).toEqual({ values: audioDefault, remembered: null });
  });

  it('returns copies, never the default arrays', () => {
    const toAudio = specFlavorsAfterTypeSwitch({
      from: '',
      to: 'audio_cpp',
      current: template,
      template,
      remembered: null,
    });
    expect(toAudio?.values.apiFlavors).not.toBe(audioCppSpecFlavorDefaults.apiFlavors);
  });
});
