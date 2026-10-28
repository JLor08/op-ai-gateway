// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The voices of the speech model a Voice Chat would speak with (#318), for its voice field: the
// voice names the gateway has cached for that model (api.modelVoices), the speech composer's
// lookup asked of the CONVERSATION's model instead of the thread's -- the conversation's speech
// model is the user's pick (or the automatic one), not the thread's own model.
//
// Like the composer's voices effect: a list belongs to the model it was fetched for and is never
// handed out for another (so a stale list is not on offer while the next lookup is in flight, not
// even for one render), a slow answer for a model that is no longer the one asked about is
// ignored, and a failed lookup is no voices (the field is then not shown, and the model's own
// default voice speaks). No lookup at all while there is no model.

import { useEffect, useState, type RefObject } from 'react';
import type { PortalApi } from '../../api';

// One shared empty list, so that "no voices" is the same value every time.
const NO_VOICES: readonly string[] = [];

type HeldVoices = { model: string; voices: readonly string[] };

export function useVoiceChatVoices(
  apiRef: RefObject<Pick<PortalApi, 'modelVoices'>>,
  model: string,
): readonly string[] {
  const [held, setHeld] = useState<HeldVoices>({ model: '', voices: NO_VOICES });

  useEffect(() => {
    if (model === '') return;
    let cancelled = false;
    void apiRef.current
      .modelVoices(model)
      .then((voices) => {
        if (!cancelled) setHeld({ model, voices });
      })
      .catch(() => {
        if (!cancelled) setHeld({ model, voices: NO_VOICES });
      });
    return () => {
      cancelled = true;
    };
  }, [apiRef, model]);

  return held.model === model ? held.voices : NO_VOICES;
}
