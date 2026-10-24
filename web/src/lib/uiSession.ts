// Web UI session plumbing (RFC FA P0).
//
// The browser holds an HttpOnly session cookie and nothing else — no bearer.
// The server refuses a state-changing request that a session cookie
// authenticates unless it carries X-Loomcycle-UI: 1 (its CSRF defence: another
// site cannot add a custom header without a CORS preflight the server never
// grants). installUIRequestHeader adds it to EVERY same-origin fetch at boot,
// rather than at each of the many call sites (api.ts, the page-local fetches,
// the @loomcycle/* packages given a fetch), so a call site added later cannot
// forget it. Cross-origin requests are left alone.

export const UI_REQUEST_HEADER = "X-Loomcycle-UI";

function requestURL(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

/** Wraps fetchImpl so every request to `origin` carries the UI header. */
export function withUIHeader(fetchImpl: typeof fetch, origin: string): typeof fetch {
  return (input: RequestInfo | URL, init?: RequestInit) => {
    let sameOrigin = false;
    try {
      sameOrigin = new URL(requestURL(input), origin).origin === origin;
    } catch {
      sameOrigin = false;
    }
    if (!sameOrigin) return fetchImpl(input, init);
    const base = init?.headers ?? (input instanceof Request ? input.headers : undefined);
    const headers = new Headers(base);
    headers.set(UI_REQUEST_HEADER, "1");
    return fetchImpl(input, { ...init, headers });
  };
}

/** Installs the UI header on the page's fetch. Call once, before rendering. */
export function installUIRequestHeader(win: Window): void {
  win.fetch = withUIHeader(win.fetch.bind(win), win.location.origin);
}

/** Ends the session on the server (POST /v1/_auth/logout). The server also
 *  clears the cookies and sends Clear-Site-Data. */
export async function endSession(fetchImpl: typeof fetch = fetch): Promise<boolean> {
  try {
    const r = await fetchImpl("/v1/_auth/logout", {
      method: "POST",
      credentials: "same-origin",
      headers: { [UI_REQUEST_HEADER]: "1" },
    });
    return r.ok;
  } catch {
    return false;
  }
}

/** The sign-out a person asked for (a click): end the session, then go to the
 *  login page. Never call it on page load — a link from another site could
 *  then sign the user out, since this same-origin POST carries the cookie. */
export async function signOut(
  fetchImpl: typeof fetch = fetch,
  go: (url: string) => void = (url) => window.location.assign(url),
): Promise<boolean> {
  const ok = await endSession(fetchImpl);
  if (ok) go("/ui/login");
  return ok;
}

/** One sign-in option, as /ui/login-config.json lists it. */
export interface LoginGate {
  name: string;
  kind: string;
  label: string;
}

export interface LoginConfig {
  mode: string;
  gates: LoginGate[];
  origins: string[];
  /** Why this browser cannot start a session here (e.g. insecure_transport). */
  sessionRefused: string;
}

/** Reads /ui/login-config.json defensively: a field of the wrong shape is
 *  treated as absent, so an older or newer server never breaks the page. */
export function parseLoginConfig(raw: unknown): LoginConfig {
  const c = (raw && typeof raw === "object" ? raw : {}) as Record<string, unknown>;
  const gates = Array.isArray(c.gates)
    ? c.gates.filter(
        (g): g is LoginGate =>
          !!g &&
          typeof (g as LoginGate).name === "string" &&
          typeof (g as LoginGate).kind === "string" &&
          typeof (g as LoginGate).label === "string",
      )
    : [];
  return {
    mode: typeof c.mode === "string" ? c.mode : "",
    gates,
    origins: Array.isArray(c.login_origins) ? c.login_origins.filter((o): o is string => typeof o === "string") : [],
    sessionRefused: typeof c.session_refused === "string" ? c.session_refused : "",
  };
}
