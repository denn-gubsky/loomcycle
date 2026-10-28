/**
 * @internal — reached through LoomcycleClient.deviceLogin().
 *
 * The client side of loomcycle's device login (RFC 8628, a `kind: device`
 * sign-in gate): ask the server for a code pair, show the person the short
 * code, poll until they approve it in a signed-in Web UI session, and return
 * the token issued to them. Both requests are unauthenticated — the device
 * code is the credential — so no bearer the client holds is sent with them.
 */

import { DeviceLoginError } from "./errors.js";
import type { _FetchContext } from "./fetch-helpers.js";
import type { DeviceLoginOptions, DeviceLoginResult } from "./types.js";

const GRANT_TYPE = "urn:ietf:params:oauth:grant-type:device_code";
/** RFC 8628 §3.5: a slow_down lengthens this and every later poll by 5 s. */
const SLOW_DOWN_STEP_MS = 5_000;
const DEFAULT_INTERVAL_S = 5;
/** The server issues codes for ten minutes; a longer claim is not trusted. */
const DEFAULT_EXPIRES_S = 600;
const MAX_EXPIRES_S = 3_600;

interface DeviceAuthorization {
  device_code?: unknown;
  user_code?: unknown;
  verification_uri?: unknown;
  verification_uri_complete?: unknown;
  expires_in?: unknown;
  interval?: unknown;
}

interface DeviceTokenAnswer {
  access_token?: unknown;
  token_type?: unknown;
  expires_in?: unknown;
  scope?: unknown;
  error?: unknown;
  error_description?: unknown;
}

async function postForm(
  ctx: _FetchContext,
  path: string,
  form: Record<string, string>,
  signal: AbortSignal | undefined,
): Promise<{ status: number; body: Record<string, unknown>; text: string }> {
  const res = await ctx.fetchImpl(`${ctx.baseUrl}${path}`, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams(form).toString(),
    signal,
  });
  const text = await res.text();
  let body: Record<string, unknown> = {};
  try {
    const parsed: unknown = JSON.parse(text);
    if (parsed && typeof parsed === "object") body = parsed as Record<string, unknown>;
  } catch {
    // A non-JSON answer is reported by status below.
  }
  return { status: res.status, body, text };
}

function sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason ?? new Error("aborted"));
      return;
    }
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal?.reason ?? new Error("aborted"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** A string shown to a person must not carry control characters a hostile
 *  server could use to rewrite what they see. */
function printable(s: unknown, max: number): s is string {
  return typeof s === "string" && s.length > 0 && s.length <= max && /^[\x20-\x7e]+$/.test(s);
}

function httpURL(s: unknown): s is string {
  if (!printable(s, 2048)) return false;
  try {
    const u = new URL(s);
    return u.protocol === "https:" || u.protocol === "http:";
  } catch {
    return false;
  }
}

function positiveInt(v: unknown): number | undefined {
  return typeof v === "number" && Number.isInteger(v) && v > 0 ? v : undefined;
}

export async function deviceLogin(
  ctx: _FetchContext,
  opts: DeviceLoginOptions,
): Promise<DeviceLoginResult> {
  const clientId = opts.clientId ?? "loomcycle-ts";
  const start = await postForm(ctx, "/v1/_auth/device", { client_id: clientId }, opts.signal);
  if (start.status !== 200) {
    const code = typeof start.body.code === "string" ? start.body.code : undefined;
    throw new DeviceLoginError(`the server refused to start a device login (HTTP ${start.status})`, {
      status: start.status,
      bodyText: start.text.slice(0, 1024),
      code,
    });
  }
  const da = start.body as DeviceAuthorization;
  if (
    typeof da.device_code !== "string" || da.device_code === "" ||
    !printable(da.user_code, 32) || !httpURL(da.verification_uri)
  ) {
    throw new DeviceLoginError("the server's device-login answer is malformed", { status: start.status });
  }
  const deviceCode = da.device_code;
  let expiresIn = positiveInt(da.expires_in) ?? DEFAULT_EXPIRES_S;
  if (expiresIn > MAX_EXPIRES_S) expiresIn = DEFAULT_EXPIRES_S;
  await opts.onPrompt({
    userCode: da.user_code,
    verificationUri: da.verification_uri,
    verificationUriComplete: httpURL(da.verification_uri_complete) ? da.verification_uri_complete : undefined,
    expiresIn,
  });

  let intervalMs = (positiveInt(da.interval) ?? DEFAULT_INTERVAL_S) * 1000;
  // A little past the code's own expiry, so the server's expired_token is
  // what ends a login nobody approved.
  const deadline = Date.now() + expiresIn * 1000 + 30_000;
  for (;;) {
    await sleep(intervalMs, opts.signal);
    if (Date.now() > deadline) {
      throw new DeviceLoginError("the code expired before it was approved", { code: "expired_token" });
    }
    const res = await postForm(
      ctx,
      "/v1/_auth/device/token",
      { grant_type: GRANT_TYPE, device_code: deviceCode, client_id: clientId },
      opts.signal,
    );
    const tok = res.body as DeviceTokenAnswer;
    if (res.status === 200 && typeof tok.access_token === "string" && tok.access_token !== "") {
      return {
        accessToken: tok.access_token,
        tokenType: typeof tok.token_type === "string" ? tok.token_type : "Bearer",
        expiresIn: positiveInt(tok.expires_in),
        scope: typeof tok.scope === "string" && tok.scope !== "" ? tok.scope.split(" ") : undefined,
      };
    }
    switch (tok.error) {
      case "authorization_pending":
        continue;
      case "slow_down":
        intervalMs += SLOW_DOWN_STEP_MS;
        continue;
    }
    // RFC 8628 refusals carry the code in `error`; loomcycle's own refusals
    // (device_login_disabled, …) carry it in `code` and a message in `error`.
    const code =
      typeof res.body.code === "string" ? res.body.code : typeof tok.error === "string" ? tok.error : undefined;
    const why =
      code === "access_denied"
        ? "the login was denied, or its approval no longer holds"
        : code === "expired_token"
          ? "the code expired before it was approved"
          : typeof tok.error_description === "string"
            ? tok.error_description
            : `the server refused the login (HTTP ${res.status})`;
    // A refusal's body carries no token. A 200 without a usable one is
    // malformed, and its body is not echoed, in case it holds one anyway.
    throw new DeviceLoginError(why, {
      status: res.status,
      bodyText: res.status === 200 ? undefined : res.text.slice(0, 1024),
      code,
    });
  }
}
