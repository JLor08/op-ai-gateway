// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { RuntimeSpec } from '../../api';
import { applicationTypeDefaults } from './applicationTypeDefaults';
import type { RuntimeSpecTemplate } from './runtimeSpecTemplate';

/**
 * The launch-spec form's API-variant block: the ticked flavors and both
 * endpoint modes. The same three fields as the parent application's template.
 */
export type RuntimeSpecFlavorValues = RuntimeSpecTemplate;

const sdTypeDefaults = applicationTypeDefaults.stable_diffusion_cpp;

/**
 * What the form sets when Type switches to stable_diffusion_cpp: the
 * application form's own default for that server kind, because sd-server
 * serves image generation only.
 */
export const stableDiffusionSpecFlavorDefaults: RuntimeSpecFlavorValues = {
  apiFlavors: [...sdTypeDefaults.apiFlavors],
  responsesMode: sdTypeDefaults.responsesMode,
  messagesMode: sdTypeDefaults.messagesMode,
};

/**
 * Mirrors routing.FlavorsAreImagesOnly: the list names openai_images and
 * neither text flavor. An empty list is never images-only.
 */
export function flavorsAreImagesOnly(flavors: readonly string[]): boolean {
  if (flavors.includes('openai') || flavors.includes('anthropic')) return false;
  return flavors.includes('openai_images');
}

/**
 * Whether a ticked flavor is missing from the parent application. Candidacy
 * reads the application's flavors, so such a flavor has no effect for the
 * spec. Mirrors the api_flavors_not_on_application warning.
 */
export function tickedFlavorMissingOnApplication(
  ticked: readonly string[],
  applicationFlavors: readonly string[],
): boolean {
  return ticked.some((flavor) => !applicationFlavors.includes(flavor));
}

/** The Type, binary and resolved type of the spec an Edit form was loaded from. */
export type LoadedSpecType = Pick<RuntimeSpec, 'type' | 'binary' | 'effective_type'>;

/**
 * Whether the form describes a stable_diffusion_cpp spec: the Type select
 * says so, or Type is Auto, the loaded spec was Auto too and resolved to it,
 * and the binary is still the loaded one (compared trimmed, as the form saves
 * it). A loaded spec with an explicit Type resolved to that Type whatever its
 * binary, so its effective_type says nothing about how Auto reads the binary.
 * The binary detection itself is Go-only (routing.DetectRuntimeSpecType), so
 * neither such a spec nor a changed binary is judged under Auto here.
 */
export function formSpecIsStableDiffusion(
  type: RuntimeSpec['type'],
  binary: string,
  loaded: LoadedSpecType | null,
): boolean {
  if (type === 'stable_diffusion_cpp') return true;
  if (type !== '' || loaded?.type !== '') return false;
  return binary.trim() === loaded.binary.trim() && loaded.effective_type === 'stable_diffusion_cpp';
}

/**
 * Whether a stable_diffusion_cpp spec has a text flavor ticked, or none at
 * all. sd-server has no chat endpoint, and a list that is not images-only is
 * listed as a text model. Mirrors the api_flavors_text_on_stable_diffusion
 * warning.
 */
export function textFlavorsOnStableDiffusion(
  ticked: readonly string[],
  specIsStableDiffusion: boolean,
): boolean {
  return specIsStableDiffusion && !flavorsAreImagesOnly(ticked);
}

// A flavor list is a set: toggling a flavor off and on again moves it to the
// end, so order carries no meaning.
function sameFlavorSet(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  const byName = (x: string, y: string) => x.localeCompare(y);
  const sortedB = [...b].sort(byName);
  return [...a].sort(byName).every((flavor, i) => flavor === sortedB[i]);
}

function sameFlavorValues(a: RuntimeSpecFlavorValues, b: RuntimeSpecFlavorValues): boolean {
  return (
    sameFlavorSet(a.apiFlavors, b.apiFlavors) &&
    a.responsesMode === b.responsesMode &&
    a.messagesMode === b.messagesMode
  );
}

function copyFlavorValues(values: RuntimeSpecFlavorValues): RuntimeSpecFlavorValues {
  return { ...values, apiFlavors: [...values.apiFlavors] };
}

export type RuntimeSpecTypeSwitch = {
  from: RuntimeSpec['type'];
  to: RuntimeSpec['type'];
  current: RuntimeSpecFlavorValues;
  template: RuntimeSpecFlavorValues;
  remembered: RuntimeSpecFlavorValues | null;
};

export type RuntimeSpecTypeSwitchResult = {
  values: RuntimeSpecFlavorValues;
  remembered: RuntimeSpecFlavorValues | null;
};

/**
 * The API-variant block after the Type select changes, or null when the
 * values stay as they are. Only values the operator has not changed move,
 * judged against the parent template and the sd default rather than against
 * a touched flag, so the rule holds on Create and on Edit alike.
 *
 * - To stable_diffusion_cpp, with the flavors equal to the template's
 *   (order-insensitive): the sd default replaces them, and the current values
 *   are remembered.
 * - From stable_diffusion_cpp to an explicit non-sd type, with the values
 *   still exactly the sd default: the remembered values come back, or the
 *   template when nothing is remembered (a spec loaded as sd).
 * - To Auto nothing moves: an Auto spec with an sd-server binary is still sd,
 *   the same rule as the health path's.
 */
export function specFlavorsAfterTypeSwitch(
  change: RuntimeSpecTypeSwitch,
): RuntimeSpecTypeSwitchResult | null {
  const { from, to, current, template, remembered } = change;
  if (from === to || to === '') return null;
  if (to === 'stable_diffusion_cpp') {
    if (!sameFlavorSet(current.apiFlavors, template.apiFlavors)) return null;
    return {
      values: copyFlavorValues(stableDiffusionSpecFlavorDefaults),
      remembered: copyFlavorValues(current),
    };
  }
  if (from !== 'stable_diffusion_cpp') return null;
  if (!sameFlavorValues(current, stableDiffusionSpecFlavorDefaults)) return null;
  return { values: copyFlavorValues(remembered ?? template), remembered: null };
}
