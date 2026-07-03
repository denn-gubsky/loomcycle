import { useCallback, useEffect, useMemo, useState } from "react";
import { UsageAggregate, UsageReportResponse, getUsage } from "../api";

// UsageView — GET /v1/_usage (RFC AV).
//
// Token usage + money spent from the per-call ledger (∪ the compact archive),
// grouped by any of tenant / user / provider / model / source. The
// operator-vs-tenant split is just `source` in the grouping: an operator-key run
// bills the operator AND counts as tenant consumption; a tenant-key run counts
// for the tenant only. Tenant-scoped by the API (a tenant operator sees only its
// own tenant; admin sees all + an optional tenant focus).

// The five whitelisted dimensions, in the server's canonical column order.
const DIMENSIONS: { key: string; label: string }[] = [
  { key: "tenant", label: "tenant" },
  { key: "user", label: "user" },
  { key: "provider", label: "provider" },
  { key: "model", label: "model" },
  { key: "source", label: "source" },
];

// toRFC3339 converts an <input type="datetime-local"> value (local time) to the
// RFC3339 the server expects. Empty in → empty out (caller skips the param).
function toRFC3339(local: string): string {
  if (!local) return "";
  const d = new Date(local);
  if (isNaN(d.getTime())) return "";
  return d.toISOString();
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return String(n);
}

function fmtCost(cost: number, currency?: string): string {
  const cur = currency || "USD";
  const digits = cost !== 0 && Math.abs(cost) < 1 ? 4 : 2;
  return `${cost.toFixed(digits)} ${cur}`;
}

// dimValue returns the cell text for a dimension on a row ("—" when blank, e.g.
// an operator row has an empty scope, or a dimension not grouped).
function dimValue(row: UsageAggregate, key: string): string {
  switch (key) {
    case "tenant":
      return row.tenant_id || "—";
    case "user":
      return row.user_id || "—";
    case "provider":
      return row.provider || "—";
    case "model":
      return row.model || "—";
    case "source":
      return row.credential_source || "—";
  }
  return "";
}

export default function UsageView() {
  // Default grouping is the operator-vs-tenant view.
  const [dims, setDims] = useState<Set<string>>(
    () => new Set(["tenant", "source"])
  );
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [tenant, setTenant] = useState("");
  const [resp, setResp] = useState<UsageReportResponse | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(false);

  const activeDims = useMemo(
    () => DIMENSIONS.filter((d) => dims.has(d.key)),
    [dims]
  );

  const fetchUsage = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const groupBy = DIMENSIONS.filter((d) => dims.has(d.key))
        .map((d) => d.key)
        .join(",");
      setResp(
        await getUsage({
          group_by: groupBy || undefined,
          from: toRFC3339(from) || undefined,
          to: toRFC3339(to) || undefined,
          tenant: tenant.trim() || undefined,
        })
      );
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [dims, from, to, tenant]);

  useEffect(() => {
    void fetchUsage();
    // Run once on mount; subsequent fetches are driven by the Apply button so a
    // half-typed filter doesn't spam the endpoint.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggleDim = (key: string) => {
    setDims((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  // Totals + the operator-vs-tenant split (computed client-side from the rows;
  // meaningful whenever `source` is grouped, else operator/tenant read 0).
  const totals = useMemo(() => {
    let cost = 0;
    let operator = 0;
    let tenantSpend = 0;
    let unpriced = 0;
    let currency = "";
    for (const r of resp?.rows ?? []) {
      cost += r.cost;
      unpriced += r.unpriced_calls;
      if (r.currency) currency = r.currency;
      if (r.credential_source === "operator") operator += r.cost;
      if (r.credential_source === "tenant" || r.credential_source === "user")
        tenantSpend += r.cost;
    }
    return { cost, operator, tenantSpend, unpriced, currency };
  }, [resp]);

  const sourceGrouped = dims.has("source");

  return (
    <div className="usage-view">
      <div className="usage-header">
        <div>
          <h1>usage</h1>
          <p className="usage-sub">
            Token usage &amp; cost from the per-call ledger. Group by{" "}
            <code>source</code> for the operator-vs-tenant split.
          </p>
        </div>
      </div>

      <div className="usage-controls">
        <div className="usage-dims">
          <span className="usage-ctl-label">group by</span>
          {DIMENSIONS.map((d) => (
            <label key={d.key} className="usage-dim-chip">
              <input
                type="checkbox"
                checked={dims.has(d.key)}
                onChange={() => toggleDim(d.key)}
              />
              {d.label}
            </label>
          ))}
        </div>
        <div className="usage-range">
          <label>
            from
            <input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
            />
          </label>
          <label>
            to
            <input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
            />
          </label>
          <label>
            tenant
            <input
              type="text"
              placeholder="(admin focus)"
              value={tenant}
              onChange={(e) => setTenant(e.target.value)}
            />
          </label>
          <button
            type="button"
            className="ghost-btn"
            onClick={() => void fetchUsage()}
            disabled={loading}
          >
            {loading ? "loading…" : "apply"}
          </button>
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      {resp && (
        <div className="usage-summary">
          <div className="usage-stat">
            <span className="usage-stat-label">total cost</span>
            <span className="usage-stat-value">
              {fmtCost(totals.cost, totals.currency)}
            </span>
          </div>
          {sourceGrouped && (
            <>
              <div className="usage-stat">
                <span className="usage-stat-label">operator bill</span>
                <span className="usage-stat-value">
                  {fmtCost(totals.operator, totals.currency)}
                </span>
              </div>
              <div className="usage-stat">
                <span className="usage-stat-label">tenant-funded</span>
                <span className="usage-stat-value">
                  {fmtCost(totals.tenantSpend, totals.currency)}
                </span>
              </div>
            </>
          )}
          {totals.unpriced > 0 && (
            <div className="usage-stat usage-warn">
              <span className="usage-stat-label">unpriced calls</span>
              <span className="usage-stat-value">{totals.unpriced}</span>
            </div>
          )}
        </div>
      )}

      {resp && resp.rows.length > 0 && (
        <div className="usage-table-wrap">
          <table className="usage-table">
            <thead>
              <tr>
                {activeDims.map((d) => (
                  <th key={d.key}>{d.label}</th>
                ))}
                <th className="num">input</th>
                <th className="num">output</th>
                <th className="num">cache</th>
                <th className="num">calls</th>
                <th className="num">cost</th>
              </tr>
            </thead>
            <tbody>
              {resp.rows.map((r, i) => (
                <tr key={i}>
                  {activeDims.map((d) => (
                    <td key={d.key} className={d.key === "source" ? `usage-src usage-src-${r.credential_source}` : ""}>
                      {dimValue(r, d.key)}
                    </td>
                  ))}
                  <td className="num">{fmtTokens(r.input_tokens)}</td>
                  <td className="num">{fmtTokens(r.output_tokens)}</td>
                  <td className="num">
                    {fmtTokens(r.cache_creation_tokens + r.cache_read_tokens)}
                  </td>
                  <td className="num">
                    {r.call_count}
                    {r.unpriced_calls > 0 && (
                      <span className="usage-unpriced" title="calls with no price">
                        {" "}
                        ({r.unpriced_calls} unpriced)
                      </span>
                    )}
                  </td>
                  <td className="num">{fmtCost(r.cost, r.currency)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {resp && resp.rows.length === 0 && (
        <div className="empty">no usage in this window.</div>
      )}
    </div>
  );
}
