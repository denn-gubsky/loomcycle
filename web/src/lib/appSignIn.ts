// App sign-in (an app such as loomboard signing a person in through this
// server's own gates). The app sends the browser to /v1/_oauth/authorize; with
// no session there, the server sends it here as /ui/login?oauth=<the authorize
// query>. After the person signs in, the page goes back to the authorize route
// with that same query, which now finds the session and hands the app its code.
//
// The query is passed through untouched and only ever appended to the fixed
// authorize path, so a crafted link can send the browser nowhere but the
// server's own authorize step, which checks the app and its return address.

export const AUTHORIZE_PATH = "/v1/_oauth/authorize";

/** The authorize query a login page was handed, "" for a plain sign-in. */
export function appSignInRequest(search: string): string {
  return new URLSearchParams(search).get("oauth") ?? "";
}

/** Where the browser goes once signed in: back to the authorize step for an
 *  app sign-in, else the Web UI. */
export function afterSignInHref(oauth: string): string {
  return oauth ? AUTHORIZE_PATH + "?" + oauth : "/ui";
}

/** The login page an app sign-in returns to after a redirect gate (an
 *  identity provider), so the page can continue to the app from there. */
export function appReturnTo(oauth: string): string {
  return oauth ? "/ui/login?" + new URLSearchParams({ oauth }).toString() : "";
}

const autoKey = "lc.oauth.continued";

/** Whether the page may continue an app sign-in on its own because the
 *  browser is already signed in — once per request, so a session the
 *  authorize step does not accept cannot bounce the browser in a loop. */
export function mayAutoContinue(oauth: string, storage: Pick<Storage, "getItem" | "setItem"> | null): boolean {
  if (!oauth) return false;
  try {
    if (storage?.getItem(autoKey) === oauth) return false;
    storage?.setItem(autoKey, oauth);
  } catch {
    // No storage: continue anyway; the authorize step decides.
  }
  return true;
}
