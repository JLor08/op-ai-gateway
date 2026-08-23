// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Shared transport primitives used by every domain module below: the Fetcher
// type, the request/requestText helpers, the SSE reconnect scaffold, and the
// query-string builder. Kept import-free (no cross-domain types) so every
// domain module can depend on it without creating cycles.

export type Fetcher = typeof fetch;

export class PortalApiError extends Error {
  status: number;
  code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'PortalApiError';
    this.status = status;
    this.code = code;
  }
}

export function buildQueryString(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue;
    search.set(key, String(value));
  }
  const query = search.toString();
  return query ? `?${query}` : '';
}

// Shared reconnect-with-backoff scaffold for the SSE subscription methods
// across the domain modules (subscribeActivity, subscribeServerPerf,
// subscribeModelServers, subscribeLogs, subscribeBenchmark). Opens `url` with
// credentials, wires one named-event listener per `handlers` entry (resetting
// the backoff attempt counter before every dispatch, matching each event
// handler's original `attempt = 0` line), retries with exponential backoff
// (1000 * 2**attempt, capped at 30000ms) on error, and returns an idempotent
// unsubscribe that stops further reconnects, clears any pending timer, and
// closes the live source. `opts.onOpen` fires on every (re)open with whether a
// prior connection had already succeeded (true only from the second open
// onward, i.e. a genuine reconnect rather than the initial connect);
// `opts.onError` fires when the connection drops, before a reconnect is
// scheduled.
export function subscribeSSE(
  url: string,
  handlers: Record<string, (e: MessageEvent) => void>,
  opts?: { onOpen?: (reconnected: boolean) => void; onError?: () => void },
): () => void {
  let source: EventSource | null = null;
  let attempt = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let stopped = false;
  let hasConnected = false;

  const open = () => {
    if (stopped) return;
    source = new EventSource(url, { withCredentials: true });
    source.onopen = () => {
      attempt = 0;
      opts?.onOpen?.(hasConnected);
      hasConnected = true;
    };
    for (const [type, handler] of Object.entries(handlers)) {
      source.addEventListener(type, (e: Event) => {
        attempt = 0;
        handler(e as MessageEvent);
      });
    }
    source.onerror = () => {
      source?.close();
      source = null;
      if (stopped) return;
      opts?.onError?.();
      const delay = Math.min(1000 * 2 ** attempt, 30000);
      attempt += 1;
      timer = setTimeout(open, delay);
    };
  };
  open();

  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
    source?.close();
    source = null;
  };
}

export async function request<T>(
  fetcher: Fetcher,
  path: string,
  options: { method?: string; body?: unknown } = {},
): Promise<T> {
  const headers: Record<string, string> = {};
  const init: RequestInit = { headers, credentials: 'include' };
  if (options.method) {
    init.method = options.method;
  }
  const isMutating = options.method && options.method !== 'GET';
  if (isMutating) {
    headers['X-OP-CSRF'] = '1';
  }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(options.body);
  }
  const response = await fetcher(path, init);
  const payload = await readJSON(response);
  if (!response.ok) {
    const error = isErrorPayload(payload) ? payload.error : undefined;
    throw new PortalApiError(
      response.status,
      error?.code ?? 'request.failed',
      error?.message ?? (response.statusText || 'request failed'),
    );
  }
  if (payload === null) {
    throw new PortalApiError(response.status, 'request.invalid_response', 'invalid JSON response');
  }
  return payload as T;
}

// GET-only counterpart to request<T> for an endpoint that returns plain text
// (PEM material, a generated configuration file) rather than JSON -- reading the
// body as JSON here would throw on perfectly valid non-JSON text. Mirrors
// downloadAgentBinary's error handling exactly: a bare fetcher call, credentials
// included, and on !response.ok a PortalApiError carrying the JSON error body's
// CODE (falling back to a generic one when the error body itself is not JSON),
// so a caller can distinguish e.g. a 409 from a 404 from a generic 500.
export async function requestText(fetcher: Fetcher, path: string): Promise<string> {
  const response = await fetcher(path, { credentials: 'include' });
  if (!response.ok) {
    const payload = await response.json().catch(() => null);
    const error =
      payload && typeof payload === 'object'
        ? (payload as { error?: { code?: string; message?: string } }).error
        : undefined;
    throw new PortalApiError(
      response.status,
      error?.code ?? 'request.failed',
      error?.message ?? 'request failed',
    );
  }
  return response.text();
}

async function readJSON(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text.trim() === '') {
    return null;
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return null;
  }
}

function isErrorPayload(
  payload: unknown,
): payload is { error: { code?: string; message?: string } } {
  if (!payload || typeof payload !== 'object' || !('error' in payload)) {
    return false;
  }
  const error = (payload as { error: unknown }).error;
  return !!error && typeof error === 'object';
}
