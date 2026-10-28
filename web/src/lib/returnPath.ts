// Where the login page sends the browser after sign-in: "/ui", or a `?next=`
// path on this server's own UI (the device-approval page sends a signed-out
// person to /ui/login?next=/ui/device). Only a plain /ui path is accepted —
// no scheme, host, "//", backslash, query or fragment — so the parameter can
// never become an open redirect to another site.
export function signedInDestination(search: string): string {
  let next: string | null = null;
  try {
    next = new URLSearchParams(search).get("next");
  } catch {
    next = null;
  }
  if (next && /^\/ui(\/[A-Za-z0-9_-]+)*\/?$/.test(next)) return next;
  return "/ui";
}
