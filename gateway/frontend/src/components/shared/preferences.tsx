// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import type { PortalApi } from './types';

// Per-user UI preferences, persisted at the user profile via a generic KV store
// (GET /api/portal/preferences + PUT /api/portal/preferences/{key}). Values are
// arbitrary JSON owned by the caller; any future table/component picks its own key.
//
// The profile is the source of truth; localStorage mirrors each key so the first
// paint is instant (no flash of defaults) and settings survive offline. On load
// the server value wins over the mirror. Writes update the map + mirror
// immediately and PUT to the server debounced.
//
// usePreference degrades gracefully to a localStorage-only mode when no provider
// is mounted (e.g. in component unit tests), so consumers work either way.

const MIRROR_PREFIX = 'op.pref.';

function readMirror(key: string): unknown {
  try {
    const raw = window.localStorage?.getItem(MIRROR_PREFIX + key);
    return raw === null || raw === undefined ? undefined : JSON.parse(raw);
  } catch {
    return undefined;
  }
}

function writeMirror(key: string, value: unknown): void {
  try {
    window.localStorage?.setItem(MIRROR_PREFIX + key, JSON.stringify(value));
  } catch {
    /* best-effort */
  }
}

type Updater<T> = T | ((prev: T) => T);

type PrefsContextValue = {
  get: (key: string) => unknown;
  set: (key: string, updater: (prev: unknown) => unknown) => void;
  version: number;
};

const PreferencesContext = createContext<PrefsContextValue | null>(null);

export function PreferencesProvider({
  api,
  children,
}: Readonly<{
  api: Pick<PortalApi, 'preferences' | 'setPreference'>;
  children: ReactNode;
}>) {
  // In-memory map is the read source; seeded synchronously from the mirror, then
  // reconciled from the server. A version counter drives consumer re-renders.
  const mapRef = useRef<Record<string, unknown>>({});
  const [version, setVersion] = useState(0);
  const timers = useRef<Record<string, ReturnType<typeof setTimeout>>>({});
  // Values written this session with a debounced PUT still pending.
  const pending = useRef<Record<string, unknown>>({});
  // Keys the user has changed this session: local wins, so the async initial GET
  // must NOT clobber them (it reflects server state from before the change).
  const dirty = useRef<Set<string>>(new Set());

  const seeded = useRef(false);
  if (!seeded.current) {
    seeded.current = true;
    try {
      const ls = window.localStorage;
      for (let i = 0; i < (ls?.length ?? 0); i++) {
        const k = ls.key(i);
        if (k?.startsWith(MIRROR_PREFIX)) {
          try {
            mapRef.current[k.slice(MIRROR_PREFIX.length)] = JSON.parse(ls.getItem(k) as string);
          } catch {
            /* skip corrupt mirror entry */
          }
        }
      }
    } catch {
      /* localStorage unavailable */
    }
  }

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const server = await api.preferences();
        if (cancelled || !server || typeof server !== 'object') return;
        // Server wins over the mirror-seeded values — EXCEPT keys the user already
        // changed this session (those reflect a newer local intent and have a PUT
        // in flight), which we must not visibly revert.
        for (const [k, v] of Object.entries(server)) {
          if (dirty.current.has(k)) continue;
          mapRef.current[k] = v;
          writeMirror(k, v);
        }
        setVersion((n) => n + 1);
      } catch {
        /* keep mirror-seeded values; never fail the app */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Fire any debounced write immediately (used on teardown so an edit made within
  // the debounce window still reaches the server).
  const flush = useCallback(() => {
    for (const key of Object.keys(pending.current)) {
      const t = timers.current[key];
      if (t) clearTimeout(t);
      delete timers.current[key];
      const value = pending.current[key];
      delete pending.current[key];
      void api.setPreference(key, value).catch(() => {
        /* best-effort */
      });
    }
  }, [api]);

  useEffect(() => {
    // Flush on tab hide/close so a fast logout/reload doesn't drop a pending write.
    const onHide = () => flush();
    const onVisibility = () => {
      if (document.visibilityState === 'hidden') flush();
    };
    window.addEventListener('pagehide', onHide);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      window.removeEventListener('pagehide', onHide);
      document.removeEventListener('visibilitychange', onVisibility);
      flush(); // provider unmount (e.g. logout swaps in <Login>)
    };
  }, [flush]);

  const get = useCallback((key: string) => mapRef.current[key], []);
  const set = useCallback(
    (key: string, updater: (prev: unknown) => unknown) => {
      const value = updater(mapRef.current[key]);
      mapRef.current[key] = value;
      dirty.current.add(key);
      pending.current[key] = value;
      writeMirror(key, value);
      setVersion((n) => n + 1);
      const existing = timers.current[key];
      if (existing) clearTimeout(existing);
      timers.current[key] = setTimeout(() => {
        delete timers.current[key];
        delete pending.current[key];
        void api.setPreference(key, value).catch(() => {
          /* best-effort; the mirror keeps the value locally */
        });
      }, 400);
    },
    [api],
  );

  const value = useMemo<PrefsContextValue>(() => ({ get, set, version }), [get, set, version]);
  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>;
}

/**
 * Read/write a single per-user preference. Returns [value, setValue]; setValue
 * accepts a value or an updater and persists to the profile (or to localStorage
 * only when no PreferencesProvider is mounted).
 */
export function usePreference<T>(key: string, defaultValue: T): [T, (updater: Updater<T>) => void] {
  const ctx = useContext(PreferencesContext);
  // Local fallback state for the no-provider case; seeded from the mirror.
  const [local, setLocal] = useState<T | undefined>(() => {
    const m = readMirror(key);
    return m === undefined ? undefined : (m as T);
  });

  const raw = ctx ? ctx.get(key) : local;
  // Treat both missing (undefined) and a persisted JSON null as "use the default".
  // Shape mismatches beyond null are the consumer's responsibility to guard, since
  // this primitive is generic over T.
  const value = (raw ?? defaultValue) as T;

  const setValue = useCallback(
    (updater: Updater<T>) => {
      const applied = (prev: unknown): T => {
        const base = (prev === undefined ? defaultValue : prev) as T;
        return typeof updater === 'function' ? (updater as (p: T) => T)(base) : updater;
      };
      if (ctx) {
        ctx.set(key, applied);
      } else {
        setLocal((prev) => {
          const next = applied(prev);
          writeMirror(key, next);
          return next;
        });
      }
    },
    // defaultValue is static per call-site (column ids); intentionally omitted.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [ctx, key],
  );

  return [value, setValue];
}
