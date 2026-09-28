// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ModelOption } from '../../api';

// The flavors under which a listed model can actually be used. A row's
// `flavors` are its routable flavors; `openai_images` additionally needs an
// "image: yes" verdict, because the images gate refuses a model without one
// (`routing.model_not_capable`). The row's `image` flag is the same fold as
// that gate (ModelDTO.Image: every offering mapping, or every offerable
// member of a group, has the verdict), so `openai_images` counts only when
// `image` is true. Every other flavor passes through in the row's own order.
//
// Shared by the chat's model list and the Models view's "Available via"
// column, so both say the same thing about the same row.
export function offeredFlavors(model: Pick<ModelOption, 'flavors' | 'image'>): string[] {
  return model.flavors.filter((flavor) => flavor !== 'openai_images' || model.image === true);
}
