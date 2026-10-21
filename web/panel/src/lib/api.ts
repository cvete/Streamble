// Thin fetch wrapper for /api/v1.

export class ApiError extends Error {
  status: number;
  details: string[];
  constructor(status: number, message: string, details: string[] = []) {
    super(message);
    this.status = status;
    this.details = details;
  }
}

type Listener = (status: number) => void;
const authListeners = new Set<Listener>();

/** Called with 401 (logged out) or 403 "change your password first". */
export function onAuthProblem(fn: Listener): () => void {
  authListeners.add(fn);
  return () => authListeners.delete(fn);
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (method !== 'GET') headers['X-Streamble-Request'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let res: Response;
  try {
    res = await fetch('/api/v1' + path, {
      method,
      headers,
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, 'The server cannot be reached.');
  }

  let data: any = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const message = data?.error ?? `Request failed (${res.status}).`;
    if (res.status === 401 && path !== '/auth/login') authListeners.forEach((fn) => fn(401));
    if (res.status === 403 && message === 'change your password first') authListeners.forEach((fn) => fn(403));
    throw new ApiError(res.status, capitalize(message), data?.details ?? []);
  }
  return data as T;
}

export const get = <T,>(path: string) => api<T>('GET', path);
export const post = <T,>(path: string, body?: unknown) => api<T>('POST', path, body ?? {});
export const put = <T,>(path: string, body: unknown) => api<T>('PUT', path, body);
export const del = <T,>(path: string) => api<T>('DELETE', path);

function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) + (/[.!?]$/.test(s) ? '' : '.') : s;
}

/** Path segments are validated names, but encode anyway. */
export const seg = (s: string) => s.split('/').map(encodeURIComponent).join('/');
