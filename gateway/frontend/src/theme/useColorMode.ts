// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useState } from 'react';

export type ColorModePref = 'system' | 'light' | 'dark';

const KEY = 'op.colorMode';

function readPref(): ColorModePref {
  try {
    const v = localStorage.getItem(KEY);
    if (v === 'light' || v === 'dark' || v === 'system') return v;
  } catch {
    /* ignore (private mode / no storage) */
  }
  return 'system';
}

export function useColorMode(hasDark: boolean) {
  const [pref, setPref] = useState<ColorModePref>(readPref);
  const [osDark, setOsDark] = useState<boolean>(
    () =>
      typeof window !== 'undefined' &&
      !!window.matchMedia &&
      window.matchMedia('(prefers-color-scheme: dark)').matches,
  );

  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => setOsDark(mq.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  const setMode = useCallback((next: ColorModePref) => {
    setPref(next);
    try {
      localStorage.setItem(KEY, next);
    } catch {
      /* ignore (private mode / no storage) */
    }
  }, []);

  const wantsDark = pref === 'dark' || (pref === 'system' && osDark);
  const effective: 'light' | 'dark' = hasDark && wantsDark ? 'dark' : 'light';

  const toggle = useCallback(
    () => setMode(effective === 'dark' ? 'light' : 'dark'),
    [effective, setMode],
  );

  return { pref, effective, setMode, toggle };
}
