// traceURL fills a run's trace id into the operator's trace link template
// (LOOMCYCLE_TRACE_URL_TEMPLATE). null unless both are present. The server
// checks the template is http(s) at boot; the scheme is checked again here
// because the result becomes an href, where a javascript: URL would run.
export function traceURL(template: string | undefined, traceId: string | undefined): string | null {
  if (!template || !traceId || !/^[0-9a-f]{32}$/.test(traceId)) return null;
  const href = template.split("{trace_id}").join(traceId);
  try {
    const u = new URL(href);
    return u.protocol === "http:" || u.protocol === "https:" ? href : null;
  } catch {
    return null;
  }
}
