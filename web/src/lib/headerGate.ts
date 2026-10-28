// The header gate's browser side (RFC FA P3). A proxy in front of loomcycle
// (Cloudflare Access, an AWS ALB, a forward-auth proxy) already signed the
// person in and adds a signed assertion to every request it forwards, so
// there is nothing to type: the login page offers one button, and its
// same-origin POST carries the proxy's assertion to the server, which turns
// it into a session. Never sent on page load — a page that signs in when it
// opens can be made to by any link.

import { UI_REQUEST_HEADER } from "./uiSession";

/** Signs in through a header gate. "" on success, else the refusal code. */
export async function signInThroughProxy(gate: string, fetchImpl: typeof fetch = fetch): Promise<string> {
  let r: Response;
  try {
    r = await fetchImpl("/v1/_auth/complete/" + encodeURIComponent(gate), {
      method: "POST",
      credentials: "same-origin",
      headers: { [UI_REQUEST_HEADER]: "1" },
    });
  } catch {
    return "network";
  }
  if (r.status === 204) return "";
  try {
    const body = (await r.json()) as { code?: string };
    return body.code || "refused";
  } catch {
    return "refused";
  }
}

/** What to tell the person for a refusal. */
export function proxyRefusalText(code: string, label: string): string {
  switch (code) {
    case "no_assertion":
      return `This page did not reach loomcycle through ${label}. Open loomcycle at the address ${label} protects.`;
    case "invalid_credentials":
      return `${label} did not vouch for you, or your account here is not enabled. Ask your administrator.`;
    case "insecure_transport":
      return "This server only signs you in over HTTPS.";
    case "cross_site_login_refused":
      return "Sign-in must start from this page.";
    case "network":
      return "The server could not be reached — try again.";
  }
  return "Sign-in failed — try again.";
}
