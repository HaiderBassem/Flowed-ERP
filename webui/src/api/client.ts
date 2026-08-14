/**
 * The one wrapper every call goes through.
 *
 * §13 asks the client to enforce three things centrally rather than at thirty
 * call sites, because a rule applied at call sites is a rule somebody
 * eventually forgets:
 *
 *   1. an Idempotency-Key on every command that moves money, minted per
 *      attempt and reused across retries of that attempt;
 *   2. the refusal envelope read whole and carried through as a Refusal;
 *   3. one transparent renewal of an expired access token, so a cashier is not
 *      signed out between typing an amount and pressing F9.
 */

import { Refusal, refusalFrom, unreachable } from "./errors";

const BASE = "/api/v1";

const ACCESS_KEY = "flowed.access";
const REFRESH_KEY = "flowed.refresh";

export const tokens = {
  access: (): string | null => localStorage.getItem(ACCESS_KEY),
  refresh: (): string | null => localStorage.getItem(REFRESH_KEY),
  set(access: string, refresh?: string | null): void {
    localStorage.setItem(ACCESS_KEY, access);
    if (refresh) localStorage.setItem(REFRESH_KEY, refresh);
  },
  clear(): void {
    localStorage.removeItem(ACCESS_KEY);
    localStorage.removeItem(REFRESH_KEY);
  },
};

/**
 * newIdempotencyKey mints the key one attempt carries.
 *
 * Minted when a command sheet opens — not when the button is pressed — so that
 * a double press, a flaky network and an explicit retry all reach the server
 * under one key and collect once. §01 names the failure the alternative
 * produces: a fresh key per click is a double collection on one network blip.
 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `k-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

export interface RequestOptions {
  /** Skip the Authorization header — sign-in only. */
  anonymous?: boolean;
  /** The idempotency key for a money-moving command. Required by convention on POSTs that collect. */
  idempotencyKey?: string;
  signal?: AbortSignal;
  /** Query parameters; undefined and null values are dropped rather than sent empty. */
  query?: Record<string, string | number | boolean | null | undefined>;
  /** Internal: set once a renewal has already been attempted. */
  retried?: boolean;
}

type Method = "GET" | "POST" | "PATCH" | "PUT" | "DELETE";

function buildUrl(path: string, query: RequestOptions["query"]): string {
  const url = path.startsWith("/api") ? path : `${BASE}${path}`;
  if (!query) return url;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === "") continue;
    params.set(key, String(value));
  }
  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

async function request<T>(
  method: Method,
  path: string,
  body?: unknown,
  options: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const isForm = body instanceof FormData;
  if (body !== undefined && !isForm) headers["Content-Type"] = "application/json";
  if (!options.anonymous) {
    const access = tokens.access();
    if (access) headers["Authorization"] = `Bearer ${access}`;
  }
  if (options.idempotencyKey) headers["Idempotency-Key"] = options.idempotencyKey;

  let response: Response;
  try {
    response = await fetch(buildUrl(path, options.query), {
      method,
      headers,
      body: isForm ? body : body !== undefined ? JSON.stringify(body) : undefined,
      ...(options.signal ? { signal: options.signal } : {}),
    });
  } catch (cause) {
    // Not "failed": the request may well have reached the server and the
    // command may well have posted. Saying "failed" here is how a cashier ends
    // up collecting twice. The Refusal carries the honest framing and the
    // remedy — retry under the same key.
    if (options.signal?.aborted) throw cause;
    throw unreachable(cause);
  }

  if (response.status === 401 && !options.anonymous && !options.retried && tokens.refresh()) {
    // One renewal, then stop. Looping turns an expired session into a request
    // storm against an endpoint that is already refusing.
    if (await renew()) {
      return request<T>(method, path, body, { ...options, retried: true });
    }
  }

  const text = await response.text();
  const payload: unknown = text ? safeParse(text) : null;

  if (!response.ok) throw refusalFrom(response.status, payload, response.headers);

  // Successful bodies are wrapped in {data: …}; unwrapping here keeps every
  // screen from repeating it.
  if (payload && typeof payload === "object" && "data" in payload) {
    return (payload as { data: T }).data;
  }
  return payload as T;
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return { raw: text };
  }
}

async function renew(): Promise<boolean> {
  const refresh = tokens.refresh();
  if (!refresh) return false;
  try {
    const response = await fetch(`${BASE}/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ refresh_token: refresh }),
    });
    if (!response.ok) return false;
    const body = (await response.json()) as { data?: { access_token?: string } } & {
      access_token?: string;
    };
    const access = body.data?.access_token ?? body.access_token;
    if (!access) return false;
    tokens.set(access);
    return true;
  } catch {
    return false;
  }
}

/**
 * Fetch a document (a receipt) with the credential in a header rather than the
 * URL, and hand back the text for preview and print.
 */
async function document(path: string, query?: RequestOptions["query"]): Promise<string> {
  const access = tokens.access();
  let response: Response;
  try {
    response = await fetch(buildUrl(path, query), {
      headers: access ? { Authorization: `Bearer ${access}` } : {},
    });
  } catch (cause) {
    throw unreachable(cause);
  }
  const text = await response.text();
  if (!response.ok) throw refusalFrom(response.status, safeParse(text), response.headers);
  return text;
}

export const api = {
  get: <T>(path: string, options?: RequestOptions) =>
    request<T>("GET", path, undefined, options),
  post: <T>(path: string, body?: unknown, options?: RequestOptions) =>
    request<T>("POST", path, body ?? {}, options),
  patch: <T>(path: string, body?: unknown, options?: RequestOptions) =>
    request<T>("PATCH", path, body ?? {}, options),
  document,
};

export { Refusal };
