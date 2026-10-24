// OIDC gate plumbing for the Web UI (RFC FA P1). A sign-in through an identity
// provider is a top-level navigation: the server sets a short login cookie,
// sends the browser to the provider, and the provider sends it back to the
// server, which answers with a session cookie and a redirect into the UI. The
// page never sees a provider token; all it does is link to the start route
// and, after a failure, show the opaque reason the server put in ?error=.

/** The kinds that sign in by redirecting to a provider. */
export const REDIRECT_GATE_KINDS = ["oidc"];

/** The start URL of a redirect login. returnTo must be a /ui path (the server
 *  refuses anything else); reauth asks the provider to authenticate again. */
export function redirectLoginHref(gate: string, returnTo = "", reauth = false): string {
  const q = new URLSearchParams();
  if (returnTo.startsWith("/ui")) q.set("return_to", returnTo);
  if (reauth) q.set("reauth", "1");
  const qs = q.toString();
  return "/v1/_auth/login/" + encodeURIComponent(gate) + (qs ? "?" + qs : "");
}

const errorText: Record<string, string> = {
  login_failed: "Sign-in was refused. If you should have access, ask your administrator.",
  login_expired: "The sign-in took too long or was already used. Try again.",
  access_denied: "The identity provider did not let you sign in.",
  insecure_transport:
    "This server only signs you in over HTTPS. Open it through its https:// address, or ask the operator to enable sign-in over plain HTTP for this network.",
  too_many_attempts: "Too many sign-in attempts from this network. Wait a few minutes and try again.",
  gate_unavailable: "That sign-in method is unavailable right now. Try again later.",
  return_to_refused: "That sign-in link is not valid.",
};

/** The message for a ?error= code a redirect login came back with. */
export function redirectLoginError(code: string): string {
  if (!code) return "";
  return errorText[code] ?? "Sign-in failed — try again.";
}
