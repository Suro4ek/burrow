// Typed wrapper over the burrowd admin API.
//
// Every call is same-origin and relies on the session cookie, so there is no
// token handling here at all.

export interface Overview {
  version: string;
  base_domain: string;
  public_host: string;
  control_port: string;
  control_tls: boolean;
  scheme: string;
  tcp_min: number;
  tcp_max: number;
  started_at: string;
  tunnels: number;
  sessions: number;
  tokens: number;
  bytes_in: number;
  bytes_out: number;
}

export interface Token {
  id: string;
  token: string;
  name: string;
  subdomains?: string[];
  ports?: number[];
  max_tunnels?: number;
  deny_tcp?: boolean;
  disabled?: boolean;
  created_at: string;
  last_seen?: string;
  active_sessions: number;
  active_tunnels: number;
}

export interface Tunnel {
  id: string;
  proto: "http" | "tcp";
  public: string;
  local: string;
  port?: number;
  subdomain?: string;
  token_id: string;
  token_name: string;
  session_id: string;
  hostname?: string;
  agent_addr: string;
  created_at: string;
  conns: number;
  bytes_in: number;
  bytes_out: number;
  last_active_unix: number;
}

export interface Session {
  id: string;
  token_id: string;
  token_name: string;
  hostname?: string;
  agent_addr: string;
  connected_at: string;
  tunnels: number;
}

export interface TokenInput {
  name?: string;
  subdomains?: string[];
  ports?: number[];
  max_tunnels?: number;
  deny_tcp?: boolean;
  disabled?: boolean;
}

/** Thrown for any non-2xx response, carrying the server's message. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** True when the failure was "not signed in", which sends us to the login screen. */
export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401;
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const init: RequestInit = { method, credentials: "same-origin" };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }

  const res = await fetch(path, init);
  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let payload: unknown = undefined;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      // A non-JSON body means something in front of us (a proxy, a captive
      // portal) answered instead of burrowd; surface it rather than crash.
      throw new ApiError(text.slice(0, 200), res.status);
    }
  }

  if (!res.ok) {
    const msg =
      (payload as { error?: string } | undefined)?.error ??
      `request failed with status ${res.status}`;
    throw new ApiError(msg, res.status);
  }
  return payload as T;
}

export const api = {
  me: () => request<{ authenticated: boolean }>("GET", "/_api/me"),
  login: (password: string) =>
    request<{ ok: boolean }>("POST", "/_api/login", { password }),
  logout: () => request<{ ok: boolean }>("POST", "/_api/logout", {}),

  overview: () => request<Overview>("GET", "/_api/overview"),

  tokens: () =>
    request<{ tokens: Token[] }>("GET", "/_api/tokens").then((r) => r.tokens),
  createToken: (input: TokenInput) =>
    request<Token>("POST", "/_api/tokens", input),
  updateToken: (id: string, input: TokenInput) =>
    request<Token>("PATCH", `/_api/tokens/${encodeURIComponent(id)}`, input),
  rotateToken: (id: string) =>
    request<Token>("POST", `/_api/tokens/${encodeURIComponent(id)}/rotate`, {}),
  deleteToken: (id: string) =>
    request<void>("DELETE", `/_api/tokens/${encodeURIComponent(id)}`),

  tunnels: () =>
    request<{ tunnels: Tunnel[] }>("GET", "/_api/tunnels").then(
      (r) => r.tunnels,
    ),
  closeTunnel: (id: string) =>
    request<void>("DELETE", `/_api/tunnels/${encodeURIComponent(id)}`),

  sessions: () =>
    request<{ sessions: Session[] }>("GET", "/_api/sessions").then(
      (r) => r.sessions,
    ),
  closeSession: (id: string) =>
    request<void>("DELETE", `/_api/sessions/${encodeURIComponent(id)}`),
};
