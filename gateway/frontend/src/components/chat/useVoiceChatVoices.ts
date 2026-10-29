// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The voices of the speech model a Voice Chat would speak with (#318), for its voice field: the
// voice names the gateway has cached for that model (api.modelVoices), the speech composer's
// lookup asked of the CONVERSATION's model instead of the thread's -- the conversation's speech
// model is the user's pick (or the automatic one), not the thread's own model.
//
// The answer is a VoiceList (#326), not a bare list: "not yet known" (`loading`) and "could not be
// found out" (`failed`) are different from "known" (`ready`, possibly with no voices), and the
// voice reconciliation must not clear a saved voice on the strength of a list that has not arrived
// -- an empty array could not tell those apart. A failed lookup still means the field is not
// shown and the model's own default voice speaks; it just no longer reads as "this model has none".
//
// Like the composer's voices effect: a list belongs to the model it was fetched for and is never
// handed out for another (so a stale list is not on offer while the next lookup is in flight, not
// even for one render -- that is `loading`), and a slow answer, or a slow failure, for a model that
// is no longer the one asked about is ignored. No lookup at all while there is no model: with
// nothing to ask about there is nothing to wait for, so that is `ready` with no voices.

import { useEffect, useState, type RefObject } from 'react';
import type { PortalApi } from '../../api';
import type { VoiceList } from './voiceModels';

// Shared values, so that each state is the same object every time it is reported (a consumer that
// depends on the list does not see a new one per render).
const LOADING: VoiceList = { status: 'loading' };
const NO_MODEL: VoiceList = { status: 'ready', voices: [] };
const FAILED: VoiceList = { status: 'failed' };

type HeldVoices = { model: string; list: VoiceList };

export function useVoiceChatVoices(
  apiRef: RefObject<Pick<PortalApi, 'modelVoices'>>,
  model: string,
): VoiceList {
  const [held, setHeld] = useState<HeldVoices>({ model: '', list: LOADING });

  useEffect(() => {
    if (model === '') return;
    let cancelled = false;
    void apiRef.current
      .modelVoices(model)
      .then((voices) => {
        if (!cancelled) setHeld({ model, list: { status: 'ready', voices } });
      })
      .catch(() => {
        if (!cancelled) setHeld({ model, list: FAILED });
      });
    return () => {
      cancelled = true;
    };
  }, [apiRef, model]);

  if (model === '') return NO_MODEL;
  return held.model === model ? held.list : LOADING;
}
