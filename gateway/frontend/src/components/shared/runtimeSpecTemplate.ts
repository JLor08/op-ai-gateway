// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { EndpointMode, PortalApplication } from '../../api';

export type RuntimeSpecTemplate = {
  apiFlavors: string[];
  responsesMode: EndpointMode;
  messagesMode: EndpointMode;
};

/**
 * The API-variant block a launch spec's FIRST write starts from: a snapshot of
 * the parent application, so a new spec agrees with what the application
 * already exposes rather than a passthrough-only guess the operator has to
 * re-derive by hand. Used by the Create form and by Edit of a mapping that has
 * no spec row yet (the retry after a failed Create lands there), so both first
 * writes start from the same values.
 *
 * `openai_images` is left out: it is opt-in on the spec as everywhere else. A
 * server_agent parent declares it for its image children, and copying it
 * would make every new text model's spec admit image requests too. UNLESS the
 * parent has no text flavor to fall back to: dropping it from a parent whose
 * flavors are exactly [openai_images] would leave every flavor unticked, and
 * the form cannot be saved like that (the launch-spec form refuses an empty
 * list). Excepting the exception only when a text flavor survives the filter
 * keeps the images-only parent's child reachable while leaving the normal
 * case untouched.
 */
export function runtimeSpecTemplate(
  application: Pick<PortalApplication, 'api_flavors' | 'responses_mode' | 'messages_mode'>,
): RuntimeSpecTemplate {
  const parentFlavors = application.api_flavors;
  const parentHasTextFlavor = parentFlavors.some((flavor) => flavor !== 'openai_images');
  return {
    apiFlavors: parentHasTextFlavor
      ? parentFlavors.filter((flavor) => flavor !== 'openai_images')
      : [...parentFlavors],
    responsesMode: application.responses_mode,
    messagesMode: application.messages_mode,
  };
}
