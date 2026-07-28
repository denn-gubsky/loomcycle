/**
 * GET /api/config — same-origin proxy for the loomcycle runtime's GET /v1/config.
 *
 * Deliberately sends NO Authorization header: publicOrAuthMiddleware only serves
 * the `public` disclosure view when no credential is presented (a presented-but-
 * invalid bearer is an error, not a request for less detail). The public view
 * carries version, boolean features, providers, models and search — and omits
 * commit, build_time, url, user_tiers and limits.
 *
 * Requires LOOMCYCLE_PUBLIC_CONFIG on the runtime; without that opt-in the route
 * is authenticated and this proxy will get a 401, which the page renders as
 * "configuration not published".
 *
 * The proxy exists because loomcycle sends no Access-Control-Allow-Origin and
 * registers no OPTIONS handler, so the browser could not read a cross-origin
 * response. It also puts a short edge cache in front of the VPS.
 */
export async function onRequestGet({ env }) {
  const base = (env && env.RUNTIME_URL) || 'https://app.loomcycle.cloud';
  const headers = {
    'content-type': 'application/json; charset=utf-8',
    'cache-control': 'public, max-age=60, stale-while-revalidate=300',
  };
  try {
    const upstream = await fetch(base + '/v1/config', {
      cf: { cacheTtl: 60, cacheEverything: true },
      signal: AbortSignal.timeout(5000),
    });
    if (!upstream.ok) {
      return new Response(
        JSON.stringify({ error: 'upstream', status: upstream.status }),
        { status: 200, headers: { ...headers, 'cache-control': 'no-store' } },
      );
    }
    return new Response(await upstream.text(), { status: 200, headers });
  } catch (err) {
    return new Response(JSON.stringify({ error: 'unreachable' }), {
      status: 200, headers: { ...headers, 'cache-control': 'no-store' },
    });
  }
}
