import { useCallback, useEffect, useState } from "react";
import {
  RoutingCandidate,
  RoutingResponse,
  RoutingTier,
  getRouting,
} from "../api";

// RoutingView — GET /v1/_routing.
//
// Answers "which provider + model will a consumer actually hit right now?"
// for every user_tier × tier. This is the resolution the loop performs at
// run time (per-agent overrides aside), surfaced so an operator can verify
// their provider_priority / tier config before a consumer discovers it the
// hard way.
//
// Two shapes, driven by the API (not a client-side role check): an admin
// response carries live availability per candidate (which one is SELECTED —
// the first reachable = what runs now) plus an active-providers header; a
// substrate:tenant response is the config cascade only. The UI keys the
// availability rendering off `resp.admin` AND field presence, so a stripped
// tenant payload simply renders the cascade without dots.

// tierOrder gives low/middle/high a stable visual rank; anything else sorts
// after, preserving server order.
function tierRank(tier: string): number {
  const i = ["low", "middle", "high"].indexOf(tier);
  return i === -1 ? 99 : i;
}

export default function RoutingView() {
  const [resp, setResp] = useState<RoutingResponse | null>(null);
  const [err, setErr] = useState<string>("");
  const [loading, setLoading] = useState(false);

  const fetchRouting = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      setResp(await getRouting());
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchRouting();
  }, [fetchRouting]);

  return (
    <div className="routing-view">
      <div className="routing-header">
        <div>
          <h1>routing</h1>
          <p className="routing-sub">
            The provider &amp; model each tier resolves to right now — top choice
            first, then the fallback cascade.
          </p>
        </div>
        <div className="routing-actions">
          {resp && (
            <span className="routing-generated">
              as of {new Date(resp.generated_at).toLocaleTimeString()}
            </span>
          )}
          <button
            type="button"
            className="ghost-btn"
            onClick={() => void fetchRouting()}
            disabled={loading}
          >
            {loading ? "refreshing…" : "refresh"}
          </button>
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      {resp && resp.admin && resp.providers && resp.providers.length > 0 && (
        <div className="routing-providers">
          <span className="routing-providers-label">providers</span>
          {resp.providers.map((p) => {
            const up = p.reachable && !p.excluded;
            return (
              <span
                key={p.provider}
                className={`routing-provider-chip ${up ? "up" : "down"}`}
                title={
                  p.excluded
                    ? "excluded from routing"
                    : p.last_error || (up ? "reachable" : "unreachable")
                }
              >
                <span
                  className={`routing-dot ${up ? "up" : "down"}`}
                  aria-hidden="true"
                />
                {p.provider}
              </span>
            );
          })}
        </div>
      )}

      {resp && resp.admin && (
        <div className="routing-legend">
          <span>
            <span className="routing-dot up" /> available
          </span>
          <span>
            <span className="routing-dot down" /> unavailable
          </span>
          <span className="routing-selected-key">selected</span> = what runs now
          (first available).
        </div>
      )}

      {resp && !resp.admin && (
        <p className="routing-tenant-note">
          Configured cascade. Live provider reachability is available to admins.
        </p>
      )}

      {resp &&
        resp.user_tiers.map((ut) => (
          <section key={ut.name || "__default__"} className="routing-usertier">
            <h2>
              {ut.name ? (
                <>
                  user tier <code>{ut.name}</code>
                </>
              ) : (
                "default routing"
              )}
            </h2>
            <div className="routing-tier-grid">
              {[...ut.tiers]
                .sort((a, b) => tierRank(a.tier) - tierRank(b.tier))
                .map((t) => (
                  <TierCard key={t.tier} tier={t} admin={resp.admin} />
                ))}
            </div>
          </section>
        ))}

      {resp && resp.user_tiers.length === 0 && (
        <div className="empty">no tiers configured.</div>
      )}
    </div>
  );
}

function TierCard({ tier, admin }: { tier: RoutingTier; admin: boolean }) {
  return (
    <div className="routing-tier-card">
      <div className="routing-tier-title">{tier.tier}</div>
      {tier.cascade.length === 0 ? (
        <div className="routing-tier-empty">no candidates</div>
      ) : (
        <ol className="routing-cascade">
          {tier.cascade.map((c, i) => (
            <CandidateRow
              key={`${c.provider}/${c.model}/${i}`}
              c={c}
              admin={admin}
            />
          ))}
        </ol>
      )}
    </div>
  );
}

function CandidateRow({ c, admin }: { c: RoutingCandidate; admin: boolean }) {
  // Availability dots only render when the field is present (admin payload).
  const hasAvail = admin && c.available !== undefined;
  const selected = c.selected === true;
  const rowClass = [
    "routing-cand",
    selected ? "selected" : "",
    hasAvail && !c.available ? "unavail" : "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <li className={rowClass}>
      <span className="routing-rank">{c.primary ? "top" : "fallback"}</span>
      {hasAvail && (
        <span
          className={`routing-dot ${c.available ? "up" : "down"}`}
          title={candidateStatusText(c)}
          aria-hidden="true"
        />
      )}
      <span className="routing-provider">{c.provider}</span>
      <span className="routing-model" title={c.model}>
        {c.model}
      </span>
      {selected && <span className="routing-selected-badge">selected</span>}
    </li>
  );
}

// candidateStatusText: a human hover string for the availability dot.
function candidateStatusText(c: RoutingCandidate): string {
  if (c.available) return "available";
  if (c.reachable === false) return "provider unreachable";
  if (c.stalled) return "model stalled";
  if (c.rate_limited) return "rate-limited";
  return "unavailable";
}
