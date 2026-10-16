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
const audioTypeDefaults = applicationTypeDefaults.audio_cpp;

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
 * What the form sets when Type switches to audio_cpp: the application form's
 * own default for that server kind, because audio.cpp serves speech synthesis
 * only. The speech twin of stableDiffusionSpecFlavorDefaults.
 */
export const audioCppSpecFlavorDefaults: RuntimeSpecFlavorValues = {
  apiFlavors: [...audioTypeDefaults.apiFlavors],
  responsesMode: audioTypeDefaults.responsesMode,
  messagesMode: audioTypeDefaults.messagesMode,
};

/**
 * The spec kinds whose Type switch defaults the API-variant block: each serves
 * one non-text API only, so the stock text flavors would make it a text
 * candidate. Every other kind has no entry and defaults nothing.
 */
const specTypeFlavorDefaults: Partial<Record<RuntimeSpec['type'], RuntimeSpecFlavorValues>> = {
  stable_diffusion_cpp: stableDiffusionSpecFlavorDefaults,
  audio_cpp: audioCppSpecFlavorDefaults,
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
 * judged against the parent template and the kinds' defaults (the sd and the
 * audio one, specTypeFlavorDefaults) rather than against a touched flag, so
 * the rule holds on Create and on Edit alike.
 *
 * - To a defaulting kind (stable_diffusion_cpp, audio_cpp) from the kind's
 *   own default values (the hop between the two kinds): the new default
 *   replaces them, and the remembered values stay what they were, since the
 *   operator's own are the ones before the first default.
 * - To a defaulting kind otherwise, with the flavors equal to the template's
 *   (order-insensitive): the kind's default replaces the values, and the
 *   current values are remembered.
 * - From a defaulting kind to an explicit kind without a default, with the
 *   values still exactly that kind's default: the remembered values come
 *   back, or the template when nothing is remembered (a spec loaded as that
 *   kind).
 * - To Auto nothing moves: an Auto spec whose binary is an sd-server or an
 *   audiocpp_server is still that kind, the same rule as the health path's.
 */
export function specFlavorsAfterTypeSwitch(
  change: RuntimeSpecTypeSwitch,
): RuntimeSpecTypeSwitchResult | null {
  const { from, to, current, template, remembered } = change;
  if (from === to || to === '') return null;
  const fromDefaults = specTypeFlavorDefaults[from];
  const leavingItsDefaults = fromDefaults !== undefined && sameFlavorValues(current, fromDefaults);
  const toDefaults = specTypeFlavorDefaults[to];
  if (toDefaults !== undefined) {
    // Judged against the kind being left first: with an images-only parent
    // the template's flavors equal the sd default's, and judging the hop
    // against the template would remember the default itself as if it were
    // the operator's own values.
    if (leavingItsDefaults) {
      return { values: copyFlavorValues(toDefaults), remembered };
    }
    if (!sameFlavorSet(current.apiFlavors, template.apiFlavors)) return null;
    return { values: copyFlavorValues(toDefaults), remembered: copyFlavorValues(current) };
  }
  if (!leavingItsDefaults) return null;
  return { values: copyFlavorValues(remembered ?? template), remembered: null };
}
