import { useCallback, useEffect, useState } from "react";
import {
  RoutingCandidate,
  RoutingDecision,
  RoutingDecisionModel,
  RoutingEmbedder,
  RoutingResponse,
  RoutingTier,
  SearchRoutingProvider,
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
// Every principal gets live availability per candidate (which one is SELECTED
// — the first reachable = what runs now) plus an active-providers header. The
// UI renders those purely by FIELD PRESENCE, not a client-side role check: a
// tenant's payload has last_error redacted and, under the operator-key gate, is
// filtered to the providers it can key — the same fields, fewer rows.

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

  // Availability is present for every principal now (the backend populates it
  // for admin + tenant alike). Render the dots + legend purely by field
  // presence — a redacted/filtered tenant payload still shows what it has.
  const hasAvailability =
    (!!resp?.providers && resp.providers.length > 0) ||
    !!(resp?.user_tiers ?? []).some((ut) =>
      (ut.tiers ?? []).some((t) =>
        (t.cascade ?? []).some((c) => c.available !== undefined),
      ),
    );

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

      {resp && resp.providers && resp.providers.length > 0 && (
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
                {p.auth && (
                  <span className="routing-provider-auth"> · {p.auth}</span>
                )}
              </span>
            );
          })}
        </div>
      )}

      {hasAvailability && (
        <div className="routing-legend">
          <span>
            <span className="routing-dot up" /> available
          </span>
          <span>
            <span className="routing-dot down" /> unavailable
          </span>
          <span className="routing-selected-key">selected</span> = what runs now
          (first available).
          {resp && (resp.embedder || resp.decision) && (
            <span>
              <span className="routing-dot warn" /> service model not yet
              confirmed (embedder untried, or decision model not pulled)
            </span>
          )}
        </div>
      )}

      {resp && resp.operator_key_restricted && (
        <p className="routing-tenant-note">
          Operator API keys are not available to your tenant — only providers you
          have your own credentials for are shown.
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
              {[...(ut.tiers ?? [])]
                .sort((a, b) => tierRank(a.tier) - tierRank(b.tier))
                .map((t) => (
                  <TierCard key={t.tier} tier={t} />
                ))}
            </div>
          </section>
        ))}

      {resp && resp.user_tiers.length === 0 && (
        <div className="empty">no tiers configured.</div>
      )}

      {resp && resp.search && resp.search.length > 0 && (
        <section className="routing-usertier">
          <h2>search providers</h2>
          <div className="routing-tier-grid">
            <div className="routing-tier-card">
              <div className="routing-tier-title">web search</div>
              <ol className="routing-cascade">
                {resp.search.map((sp, i) => (
                  <SearchRow key={`${sp.provider}/${i}`} sp={sp} />
                ))}
              </ol>
            </div>
          </div>
        </section>
      )}

      {resp && (resp.embedder || resp.decision) && (
        <section className="routing-usertier">
          <h2>service models</h2>
          <div className="routing-tier-grid">
            {resp.embedder && <EmbedderCard e={resp.embedder} />}
            {resp.decision && <DecisionCard d={resp.decision} />}
          </div>
        </section>
      )}
    </div>
  );
}

// EmbedderCard shows the memory embedder. Its dot is the embedder's observed
// health, not a probe: amber "untried" means nothing has been embedded since
// boot, so whether it works is unknown.
function EmbedderCard({ e }: { e: RoutingEmbedder }) {
  const dot = e.state === "ok" ? "up" : e.state === "failing" ? "down" : "warn";
  return (
    <div className="routing-tier-card">
      <div className="routing-tier-title">embedder</div>
      <ol className="routing-cascade">
        <RouteRow
          unavail={e.state === "failing"}
          dot={dot}
          dotTitle={embedderStatusText(e)}
          main={e.model ?? e.alias ?? "(no model)"}
          rank="memory"
          detail={e.provider}
        />
      </ol>
      <div className="routing-service-note">
        {e.alias && (
          <>
            alias <code>{e.alias}</code> ·{" "}
          </>
        )}
        {embedderStatusText(e)}
        {e.dimension ? ` · ${e.dimension} dimensions` : ""}
      </div>
    </div>
  );
}

function embedderStatusText(e: RoutingEmbedder): string {
  if (e.state === "untried") return "untried: nothing embedded since boot";
  if (e.state === "failing") {
    const kind = e.last_failure_kind ? `, last: ${e.last_failure_kind}` : "";
    return `failing: last call failed (${e.failures} of ${e.calls} failed${kind})`;
  }
  return `ok: ${e.calls} calls since boot, ${e.failures} failed`;
}

// DecisionCard lists the decision models, the default first.
function DecisionCard({ d }: { d: RoutingDecision }) {
  const models = [...(d.models ?? [])].sort(
    (a, b) => Number(!!b.default) - Number(!!a.default),
  );
  return (
    <div className="routing-tier-card">
      <div className="routing-tier-title">decision</div>
      {models.length === 0 ? (
        <div className="routing-tier-empty">no decision models you can key</div>
      ) : (
        <ol className="routing-cascade">
          {models.map((m) => (
            <DecisionRow key={m.name} m={m} />
          ))}
        </ol>
      )}
    </div>
  );
}

function DecisionRow({ m }: { m: RoutingDecisionModel }) {
  // No dot when the provider was never probed: there is nothing to report.
  const hasState = m.reachable !== undefined;
  const dot = m.available ? "up" : m.reachable && m.listed === false ? "warn" : "down";
  return (
    <RouteRow
      unavail={hasState && !m.available}
      dot={hasState ? dot : undefined}
      dotTitle={decisionStatusText(m)}
      main={m.name}
      rank={m.default ? "default" : "allowed"}
      detail={`${m.provider}/${m.model}`}
    />
  );
}

function decisionStatusText(m: RoutingDecisionModel): string {
  if (m.available) return "available";
  if (m.reachable === false) return "provider unreachable";
  if (m.listed === false) return "provider reachable, model not in its model list (not pulled?)";
  return "unavailable";
}

function SearchRow({ sp }: { sp: SearchRoutingProvider }) {
  const hasAvail = sp.available !== undefined;
  return (
    <RouteRow
      selected={sp.selected === true}
      unavail={hasAvail && !sp.available}
      dot={hasAvail ? (sp.available ? "up" : "down") : undefined}
      dotTitle={searchStatusText(sp)}
      main={sp.provider}
      rank={sp.primary ? "top" : "fallback"}
      detail={sp.keyable === false ? "no key for your scope" : undefined}
    />
  );
}

// searchStatusText: a human hover string for a search provider's dot.
function searchStatusText(sp: SearchRoutingProvider): string {
  if (sp.keyable === false) return "no usable key for your scope";
  if (sp.available) return "available";
  if (sp.last_error) return `in cooldown: ${sp.last_error}`;
  if (sp.reachable === false) return "in failure cooldown";
  return "unavailable";
}

function TierCard({ tier }: { tier: RoutingTier }) {
  // Defensive: the backend now serializes an empty cascade as [], but guard null
  // too so a single tier can never blank the whole routing view.
  const cascade = tier.cascade ?? [];
  return (
    <div className="routing-tier-card">
      <div className="routing-tier-title">{tier.tier}</div>
      {cascade.length === 0 ? (
        <div className="routing-tier-empty">no candidates</div>
      ) : (
        <ol className="routing-cascade">
          {cascade.map((c, i) => (
            <CandidateRow key={`${c.provider}/${c.model}/${i}`} c={c} />
          ))}
        </ol>
      )}
    </div>
  );
}

function CandidateRow({ c }: { c: RoutingCandidate }) {
  // Availability dots render purely by field presence — the backend populates
  // these for a tenant too now (admin only differs by seeing last_error).
  const hasAvail = c.available !== undefined;
  return (
    <RouteRow
      selected={c.selected === true}
      unavail={hasAvail && !c.available}
      dot={hasAvail ? (c.available ? "up" : "down") : undefined}
      dotTitle={candidateStatusText(c)}
      main={c.model}
      rank={c.primary ? "top" : "fallback"}
      detail={c.provider}
    />
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

// RouteRow is one entry of a cascade. The model is what an operator reads the
// page for, so it takes the first line whole and wraps rather than being cut
// off; where it sits in the cascade and which provider serves it go on a
// smaller line beneath, with the "selected" badge.
function RouteRow(props: {
  main: string;
  rank: string;
  detail?: string;
  dot?: "up" | "down" | "warn";
  dotTitle?: string;
  selected?: boolean;
  unavail?: boolean;
}) {
  const rowClass = ["routing-cand", props.selected ? "selected" : "", props.unavail ? "unavail" : ""]
    .filter(Boolean)
    .join(" ");
  return (
    <li className={rowClass}>
      {props.dot ? (
        <span className={`routing-dot ${props.dot}`} title={props.dotTitle} aria-hidden="true" />
      ) : (
        <span className="routing-dot-space" aria-hidden="true" />
      )}
      <div className="routing-cand-body">
        <span className="routing-model">{props.main}</span>
        <span className="routing-cand-meta">
          <span className="routing-rank">{props.rank}</span>
          {props.detail && <span className="routing-provider">{props.detail}</span>}
          {props.selected && <span className="routing-selected-badge">selected</span>}
        </span>
      </div>
    </li>
  );
}
