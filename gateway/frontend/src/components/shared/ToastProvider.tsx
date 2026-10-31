// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Box } from '@mui/material';
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import type { ReactNode } from 'react';

type ToastSeverity = 'success' | 'error';
type ToastItem = { id: number; severity: ToastSeverity; message: string };
type ToastApi = { showSuccess: (message: string) => void; showError: (message: string) => void };

const ToastContext = createContext<ToastApi | null>(null);

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error('useToast must be used within a ToastProvider');
  return ctx;
}

/**
 * Fixer Stack oben rechts UNTER der 72px-AppBar. Erfolgs-Toasts blenden nach ~4000ms
 * aus (per-Item-Timer); Fehler-Toasts bleiben bis der Nutzer sie über das X schließt.
 * Jede Alert trägt per Default role="alert". Ausstehende Auto-Dismiss-Timer werden beim
 * Unmount gestoppt, damit ein später Tick nicht setItems nach dem Teardown des Host-Fensters
 * aufruft (sonst crasht ein verwaister Timer fremde Test-Suiten mit "window is not defined").
 */
export function ToastProvider({ children }: Readonly<{ children: ReactNode }>) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextId = useRef(0);
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());

  const dismiss = useCallback((id: number) => {
    setItems((current) => current.filter((item) => item.id !== id));
  }, []);

  const push = useCallback(
    (severity: ToastSeverity, message: string) => {
      const id = nextId.current++;
      setItems((current) => [...current, { id, severity, message }]);
      if (severity === 'success') {
        const handle = setTimeout(() => {
          timers.current.delete(handle);
          dismiss(id);
        }, 4000);
        timers.current.add(handle);
      }
    },
    [dismiss],
  );

  useEffect(() => {
    const pending = timers.current;
    return () => {
      for (const handle of pending) clearTimeout(handle);
      pending.clear();
    };
  }, []);

  const api = useMemo<ToastApi>(
    () => ({
      showSuccess: (message) => push('success', message),
      showError: (message) => push('error', message),
    }),
    [push],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      <Box
        sx={{
          position: 'fixed',
          top: 80,
          right: 16,
          zIndex: (theme) => theme.zIndex.snackbar,
          display: 'flex',
          flexDirection: 'column',
          gap: 1,
        }}
      >
        {items.map((item) => (
          <Alert
            key={item.id}
            severity={item.severity}
            variant="filled"
            onClose={() => dismiss(item.id)}
          >
            {item.message}
          </Alert>
        ))}
      </Box>
    </ToastContext.Provider>
  );
}
