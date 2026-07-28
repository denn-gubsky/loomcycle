/**
 * GET /api/health — same-origin proxy for the loomcycle runtime's /healthz.
 *
 * The runtime sends no Access-Control-Allow-Origin and registers no OPTIONS
 * handler, so the browser cannot read a cross-origin response from
 * app.loomcycle.cloud directly. Proxying here also puts a short edge cache in
 * front of the VPS, so a traffic spike hits it twice a minute rather than once
 * per visitor.
 *
 * /healthz is the runtime's only unauthenticated route (explicitly exempt from
 * the auth middleware), so no bearer is involved.
 *
 * Set RUNTIME_URL in the Pages project's environment to override the default.
 */
export async function onRequestGet({ env }) {
  const base = (env && env.RUNTIME_URL) || 'https://app.loomcycle.cloud';
  const headers = {
    'content-type': 'application/json; charset=utf-8',
    'cache-control': 'public, max-age=30, stale-while-revalidate=60',
  };
  try {
    const upstream = await fetch(base + '/healthz', {
      cf: { cacheTtl: 30, cacheEverything: true },
      signal: AbortSignal.timeout(4000),
    });
    if (!upstream.ok) {
      return new Response(JSON.stringify({ ok: false, error: 'upstream ' + upstream.status }), {
        status: 200, headers,
      });
    }
    /* pass the runtime's own shape through untouched:
       { ok, version, commit, built, uptime_seconds, metrics_enabled,
         replica_id, replicas[] } */
    return new Response(await upstream.text(), { status: 200, headers });
  } catch (err) {
    return new Response(JSON.stringify({ ok: false, error: 'unreachable' }), {
      status: 200, headers: { ...headers, 'cache-control': 'no-store' },
    });
  }
}
