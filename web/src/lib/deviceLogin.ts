// The device login's browser side (RFC FA P5, RFC 8628). A CLI or the MCP
// thin client shows a short code and the link /ui/device#BCDF-GHJK; the person
// opens it, signed in through any gate, sees what is asking, and approves or
// denies. The code travels in POST bodies only; the fragment never reaches a
// server log.

import { UI_REQUEST_HEADER } from "./uiSession";

export interface DeviceRequest {
  client_id: string;
  scopes: string[];
  source_addr: string;
  created_at: string;
  expires_at: string;
  tenant_id: string;
  subject: string;
}

export type DeviceResult<T> = { ok: true; data: T } | { ok: false; code: string; status: number };

async function post<T>(f: typeof fetch, path: string, body: unknown): Promise<DeviceResult<T>> {
  let r: Response;
  try {
    r = await f(path, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", [UI_REQUEST_HEADER]: "1" },
      body: JSON.stringify(body),
    });
  } catch {
    return { ok: false, code: "network", status: 0 };
  }
  let parsed: unknown = null;
  try {
    parsed = await r.json();
  } catch {
    parsed = null;
  }
  if (r.ok) return { ok: true, data: (parsed ?? {}) as T };
  const code = (parsed as { code?: unknown } | null)?.code;
  return { ok: false, code: typeof code === "string" ? code : "refused", status: r.status };
}

export const deviceInfo = (userCode: string, f: typeof fetch = fetch) =>
  post<DeviceRequest>(f, "/v1/_auth/device/info", { user_code: userCode });

export const decideDevice = (userCode: string, approve: boolean, f: typeof fetch = fetch) =>
  post<{ approved: boolean }>(f, "/v1/_auth/device/approve", { user_code: userCode, approve });

/** The code a verification link carries in its fragment ("#BCDF-GHJK"), or "". */
export function userCodeFromHash(hash: string): string {
  let raw: string;
  try {
    raw = decodeURIComponent(hash.startsWith("#") ? hash.slice(1) : hash).trim();
  } catch {
    // A malformed escape ("#%E0") is no code, not a crash of the page.
    return "";
  }
  return /^[A-Za-z]{4}-?[A-Za-z]{4}$/.test(raw) ? raw.toUpperCase() : "";
}

const messages: Record<string, string> = {
  code_invalid: "That code is not valid or has expired. Check it, or start the login again on your device.",
  too_many_attempts: "Too many wrong codes. Wait a few minutes and try again.",
  device_not_allowed: "Your account cannot approve a device login here.",
  session_required: "Sign in first, then come back to this page.",
  device_login_disabled: "Device login is not enabled on this server.",
  network: "The server could not be reached.",
};

export function deviceRefusalText(code: string): string {
  return messages[code] ?? "That did not work. Try again.";
}
