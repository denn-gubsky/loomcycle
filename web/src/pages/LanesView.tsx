import { useEffect, useState } from "react";
import { getConcurrencyStats } from "../api";
import {
  type LanesView as LanesViewState,
  laneActive,
  laneQueued,
  laneSummary,
  lanesView,
  modelsInFlight,
  sortedLanes,
  sortedProviders,
} from "../lib/lanes";

// LanesView — the Lanes page, a read-only live view of GET
// /v1/_concurrency/stats: the global run queue, then each capped provider's
// lanes (chat / decision / embedder) and the models it has in flight.
//
// Admin-only: the route falls to the /v1/_* catch-all (ScopeAdmin), so the tab is
// in the admin group and a 403 is rendered as a state rather than an error. The
// counts are this replica's (docs/MULTI-REPLICA.md), so the view says so.

// POLL_MS: frequent enough to watch a queue drain, rare enough that an open tab
// is not a load source. Polling stops when the tab unmounts.
const POLL_MS = 3_000;

export default function LanesView() {
  const [view, setView] = useState<LanesViewState | null>(null);
  const [at, setAt] = useState<Date | null>(null);

  useEffect(() => {
    let live = true;
    // One request at a time: a reply slower than POLL_MS would otherwise race
    // the next one and could paint older counts over newer.
    let inFlight = false;
    const refresh = async () => {
      // A hidden browser tab has nobody watching; skip the request.
      if (inFlight || document.hidden) return;
      inFlight = true;
      try {
        const r = await getConcurrencyStats();
        if (!live) return;
        setView(lanesView(r));
        setAt(new Date());
      } catch (e) {
        if (live) setView({ kind: "error", message: e instanceof Error ? e.message : String(e) });
      } finally {
        inFlight = false;
      }
    };
    void refresh();
    const t = setInterval(refresh, POLL_MS);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, []);

  return (
    <div className="settings-panel">
      <h2>Lanes</h2>
      <p className="settings-help">
        Runs waiting for a run slot, and model calls waiting in each provider&rsquo;s
        lanes (<code>GET /v1/_concurrency/stats</code>). Counts are this replica&rsquo;s;
        refreshed every {POLL_MS / 1000} s
        {at && <> · as of {at.toLocaleTimeString()}</>}.
      </p>
      {view === null && <div className="settings-muted">loading…</div>}
      {view?.kind === "not_wired" && (
        <div className="settings-muted">Concurrency accounting is not wired on this instance.</div>
      )}
      {view?.kind === "forbidden" && (
        <div className="settings-muted">Lane counts are visible to an operator admin only.</div>
      )}
      {view?.kind === "error" && <div className="settings-error">{view.message}</div>}
      {(view?.kind === "uncapped" || view?.kind === "capped") && (
        <>
          <h3 className="settings-subhead">Run queue</h3>
          <table className="settings-table">
            <tbody>
              <tr>
                <td>active</td>
                <td>{view.stats.active}</td>
              </tr>
              <tr>
                <td>queued</td>
                <td>{view.stats.queued}</td>
              </tr>
              <tr>
                <td title="Runs that gave their slot back while a model call waits in a provider lane. They count against the queue bound with the queued runs.">
                  lent
                </td>
                <td>{view.stats.lent ?? 0}</td>
              </tr>
            </tbody>
          </table>
          {view.stats.per_user && Object.keys(view.stats.per_user).length > 0 && (
            <table className="settings-table">
              <thead>
                <tr>
                  <th>user</th>
                  <th>runs held</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(view.stats.per_user)
                  .sort(([a], [b]) => a.localeCompare(b))
                  .map(([user, n]) => (
                    <tr key={user}>
                      <td>
                        <code>{user}</code>
                      </td>
                      <td>{n}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          )}
        </>
      )}
      {view?.kind === "uncapped" && (
        <p className="settings-help">
          No provider caps model calls; set <code>max_concurrent</code> /{" "}
          <code>lanes</code> / <code>max_models_in_flight</code> under{" "}
          <code>providers.&lt;id&gt;</code>.
        </p>
      )}
      {view?.kind === "capped" &&
        sortedProviders(view.stats.providers).map(([id, p]) => {
          const lanes = sortedLanes(p);
          const models = modelsInFlight(p);
          return (
            <section key={id}>
              <h3 className="settings-subhead">
                <code>{id}</code>
              </h3>
              {lanes.length === 0 ? (
                <div className="settings-muted">No lanes — this provider caps only models in flight.</div>
              ) : (
                <table className="settings-table">
                  <thead>
                    <tr>
                      <th>lane</th>
                      <th>active</th>
                      <th>reserved</th>
                      <th>queued</th>
                      <th>grants</th>
                      <th>preemptions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {lanes.map(([kind, l]) => (
                      <tr key={kind} title={laneSummary(kind, l)}>
                        <td>{kind}</td>
                        <td>{laneActive(l)}</td>
                        <td>{l.reserved}</td>
                        <td>{laneQueued(l)}</td>
                        <td>{l.grants}</td>
                        <td>{l.preemptions}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {(models.length > 0 || (p.model_waiters ?? 0) > 0) && (
                <table className="settings-table">
                  <thead>
                    <tr>
                      <th>models in flight ({models.length})</th>
                      <th>calls</th>
                    </tr>
                  </thead>
                  <tbody>
                    {models.map(([model, n]) => (
                      <tr key={model}>
                        <td>
                          <code>{model}</code>
                        </td>
                        <td>{n}</td>
                      </tr>
                    ))}
                    {(p.model_waiters ?? 0) > 0 && (
                      <tr>
                        <td>waiting for a model slot</td>
                        <td>{p.model_waiters}</td>
                      </tr>
                    )}
                  </tbody>
                </table>
              )}
            </section>
          );
        })}
    </div>
  );
}
