// The API client and the session it holds.
//
// Two properties matter here and both are about failure. A refusal from the
// server arrives with a stable code, a message and often a remedy, and all
// three are carried through to the caller rather than flattened into "request
// failed" — the remedy is the most useful sentence in the system. And an
// access token that has expired is renewed once, transparently, because a
// cashier mid-payment should not be signed out between typing an amount and
// pressing enter.

const ACCESS_KEY = "flowed.access";
const REFRESH_KEY = "flowed.refresh";
const USER_KEY = "flowed.user";

let currentUser = null;

export const session = {
  token: () => localStorage.getItem(ACCESS_KEY),
  refreshToken: () => localStorage.getItem(REFRESH_KEY),
  user: () => {
    if (currentUser) return currentUser;
    const raw = localStorage.getItem(USER_KEY);
    currentUser = raw ? JSON.parse(raw) : null;
    return currentUser;
  },
  setUser(user) {
    currentUser = user;
    localStorage.setItem(USER_KEY, JSON.stringify(user));
  },
  async login(credentials) {
    const result = await api.post("/api/v1/auth/login", credentials, { anonymous: true });
    localStorage.setItem(ACCESS_KEY, result.access_token);
    if (result.refresh_token) localStorage.setItem(REFRESH_KEY, result.refresh_token);
    session.setUser(result.user);
    return result;
  },
  async logout() {
    // Told to the server, not just forgotten locally: a token the browser
    // discards is a token that still works for whoever else has it.
    try { await api.post("/api/v1/auth/logout", {}); } catch { /* already gone */ }
    session.clear();
  },
  clear() {
    currentUser = null;
    localStorage.removeItem(ACCESS_KEY);
    localStorage.removeItem(REFRESH_KEY);
    localStorage.removeItem(USER_KEY);
  },
};

// idempotencyKey mints the key a money-moving request carries.
//
// Generated here, per attempt, so that a retry of the same click — a flaky
// network, a double tap — reaches the server under one key and collects once.
function idempotencyKey() {
  if (crypto?.randomUUID) return crypto.randomUUID();
  return `k-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

class ApiError extends Error {
  constructor(status, body) {
    const error = body?.error ?? {};
    super(error.message ?? `HTTP ${status}`);
    this.status = status;
    this.code = error.code;
    this.details = error.details;
    this.requestId = error.request_id;
  }
}

async function request(method, path, body, options = {}) {
  const headers = { Accept: "application/json" };
  if (body !== undefined && !(body instanceof FormData)) headers["Content-Type"] = "application/json";
  if (!options.anonymous && session.token()) headers.Authorization = `Bearer ${session.token()}`;
  if (options.idempotent) headers["Idempotency-Key"] = options.key ?? idempotencyKey();

  const response = await fetch(path, {
    method,
    headers,
    body: body instanceof FormData ? body : body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (response.status === 401 && !options.anonymous && !options.retried && session.refreshToken()) {
    // One transparent renewal. A second failure means the session is genuinely
    // gone, and looping would turn an expired token into a request storm.
    if (await renew()) {
      return request(method, path, body, { ...options, retried: true, key: options.key });
    }
  }

  const text = await response.text();
  const payload = text ? safeParse(text) : null;

  if (!response.ok) throw new ApiError(response.status, payload);
  // The API wraps successful bodies in {data: …}; unwrapping here keeps every
  // screen from repeating it.
  return payload && Object.prototype.hasOwnProperty.call(payload, "data") ? payload.data : payload;
}

function safeParse(text) {
  try { return JSON.parse(text); } catch { return { raw: text }; }
}

async function renew() {
  try {
    const response = await fetch("/api/v1/auth/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: session.refreshToken() }),
    });
    if (!response.ok) return false;
    const body = await response.json();
    const data = body.data ?? body;
    localStorage.setItem(ACCESS_KEY, data.access_token);
    return true;
  } catch {
    return false;
  }
}

// download fetches a file with the credential in a header rather than in the
// URL, and hands back a blob for the browser to save or print.
async function download(path) {
  const response = await fetch(path, {
    headers: session.token() ? { Authorization: `Bearer ${session.token()}` } : {},
  });
  if (!response.ok) {
    const text = await response.text();
    throw new ApiError(response.status, safeParse(text));
  }
  return response.blob();
}

export const api = {
  get: (path, options) => request("GET", path, undefined, options),
  download,
  post: (path, body, options) => request("POST", path, body ?? {}, options),
  patch: (path, body, options) => request("PATCH", path, body ?? {}, options),
  upload: (path, formData, options) => request("POST", path, formData, options),
};
