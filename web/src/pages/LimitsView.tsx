import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  LimitPutBody,
  TokenLimit,
  deleteLimit,
  listLimits,
  putLimit,
} from "../api";

// LimitsView — GET/PUT/DELETE /v1/_limits (RFC AW Phase 1).
//
// Per-scope monthly token budgets: a soft ceiling (warn, the run continues) and
// a hard ceiling (this run finishes, the next is refused at admission) on a
// scope's calendar-month token total. Each row shows live month-to-date `used`
// so an operator sets a budget against real consumption. Tenant-scoped by the
// API (RFC AS): a substrate:tenant operator manages only its own tenant + its
// users; an admin sees all rows and may focus one tenant via ?tenant=. The page
// is data-driven — no client-side role branch (the server scopes + gates).

const SCOPES = ["operator", "tenant", "user"] as const;
type Scope = (typeof SCOPES)[number];

// fmtTokens mirrors UsageView's compact K/M formatter for the read-only `used`
// column. The soft/hard editors show raw integers instead (you type into them).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return String(n);
}

// parseLimit converts an editor string to the wire value: empty = null (leave
// the tier unset — no ceiling), otherwise a non-negative integer. type=number
// inputs make a malformed entry near-impossible; a stray one coerces to null
// (unset) rather than sending garbage, and the server also rejects negatives.
function parseLimit(s: string): number | null {
  const t = s.trim();
  if (t === "") return null;
  const n = Math.floor(Number(t));
  return Number.isFinite(n) && n >= 0 ? n : null;
}

// rowKey uniquely identifies a budget row across tenants + scopes (an admin's
// list can hold same-scope rows for many tenants).
function rowKey(r: TokenLimit): string {
  return `${r.tenant_id ?? ""}|${r.scope}|${r.scope_id ?? ""}`;
}

export default function LimitsView() {
  const [rows, setRows] = useState<TokenLimit[] | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(false);
  const [tenant, setTenant] = useState("");

  const fetchLimits = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const resp = await listLimits(tenant.trim() || undefined);
      setRows(resp.limits ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [tenant]);

  useEffect(() => {
    void fetchLimits();
    // Run once on mount; subsequent fetches are driven by refresh (which reads
    // the current tenant box) so a half-typed filter doesn't spam the endpoint.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="limits-view">
      <div className="limits-header">
        <div>
          <h1>limits</h1>
          <p className="limits-sub">
            Per-scope monthly token budgets — a <code>soft</code> warning and a{" "}
            <code>hard</code> cap (the next run is refused once crossed). Set a
            ceiling against each scope's live month-to-date usage.
          </p>
        </div>
        <div className="limits-actions">
          <label className="limits-tenant">
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
            onClick={() => void fetchLimits()}
            disabled={loading}
          >
            {loading ? "loading…" : "refresh"}
          </button>
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      <AddLimitForm tenantFocus={tenant.trim()} onSaved={fetchLimits} />

      {rows && rows.length > 0 && (
        <div className="limits-table-wrap">
          <table className="limits-table">
            <thead>
              <tr>
                {/* tenant column added beyond the base spec: an admin's list
                    spans tenants and a scope=tenant row's scope_id is empty, so
                    the tenant id is the only disambiguator. */}
                <th>tenant</th>
                <th>scope</th>
                <th>scope_id</th>
                <th className="num">used (mtd)</th>
                <th className="num">soft</th>
                <th className="num">hard</th>
                <th className="limits-actions-col"></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <LimitRow key={rowKey(r)} row={r} onChanged={fetchLimits} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {rows && rows.length === 0 && (
        <div className="empty">no budgets set — every scope is unlimited.</div>
      )}
    </div>
  );
}

// LimitRow is one editable budget row. The soft/hard ceilings are always-live
// number inputs (empty = unset); Save upserts, Remove deletes (→ unlimited).
function LimitRow({
  row,
  onChanged,
}: {
  row: TokenLimit;
  onChanged: () => Promise<void>;
}) {
  const [soft, setSoft] = useState<string>(row.soft_limit == null ? "" : String(row.soft_limit));
  const [hard, setHard] = useState<string>(row.hard_limit == null ? "" : String(row.hard_limit));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // Re-sync the drafts when the persisted row changes under us (a refresh after
  // this or another operator's write). Keyed on the values, not the object.
  useEffect(() => {
    setSoft(row.soft_limit == null ? "" : String(row.soft_limit));
    setHard(row.hard_limit == null ? "" : String(row.hard_limit));
  }, [row.soft_limit, row.hard_limit]);

  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const body: LimitPutBody = {
        scope: row.scope,
        soft_limit: parseLimit(soft),
        hard_limit: parseLimit(hard),
      };
      // tenant_id addresses the row's tenant (admin); harmlessly ignored for a
      // scoped operator (its own tenant is stamped server-side).
      if (row.tenant_id) body.tenant_id = row.tenant_id;
      // scope_id carries the user subject only; tenant/operator rows have "".
      if (row.scope === "user" && row.scope_id) body.scope_id = row.scope_id;
      await putLimit(body);
      await onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    setErr("");
    try {
      await deleteLimit(
        row.scope,
        row.scope === "user" ? row.scope_id : undefined,
        row.tenant_id || undefined,
      );
      await onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <tr>
        <td>{row.tenant_id || "—"}</td>
        <td>{row.scope}</td>
        <td className="limits-scopeid">{row.scope_id || "—"}</td>
        <td className="num">{fmtTokens(row.used)}</td>
        <td className="num">
          <input
            className="limits-num"
            type="number"
            min={0}
            step={1000}
            placeholder="∞"
            value={soft}
            onChange={(e) => setSoft(e.target.value)}
            disabled={busy}
          />
        </td>
        <td className="num">
          <input
            className="limits-num"
            type="number"
            min={0}
            step={1000}
            placeholder="∞"
            value={hard}
            onChange={(e) => setHard(e.target.value)}
            disabled={busy}
          />
        </td>
        <td className="limits-row-actions">
          <button type="button" className="ghost-btn" onClick={() => void save()} disabled={busy}>
            {busy ? "…" : "save"}
          </button>
          <button
            type="button"
            className="ghost-btn limits-remove"
            onClick={() => void remove()}
            disabled={busy}
          >
            remove
          </button>
        </td>
      </tr>
      {err && (
        <tr>
          <td colSpan={7}>
            <div className="err limits-row-err">{err}</div>
          </td>
        </tr>
      )}
    </>
  );
}

// AddLimitForm creates a new budget row. Scope drives which id fields apply:
//   operator — global, no ids;
//   tenant   — tenant_id names the tenant (scope_id must be empty);
//   user     — scope_id is the subject, tenant_id names its tenant.
// A tenant operator may leave tenant_id blank (stamped from its identity).
function AddLimitForm({
  tenantFocus,
  onSaved,
}: {
  tenantFocus: string;
  onSaved: () => Promise<void>;
}) {
  const [scope, setScope] = useState<Scope>("tenant");
  const [tenantId, setTenantId] = useState("");
  const [scopeId, setScopeId] = useState("");
  const [soft, setSoft] = useState("");
  const [hard, setHard] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      const body: LimitPutBody = {
        scope,
        soft_limit: parseLimit(soft),
        hard_limit: parseLimit(hard),
      };
      if (scope !== "operator") {
        // Fall back to the admin tenant-focus box when the field is blank.
        const t = tenantId.trim() || tenantFocus;
        if (t) body.tenant_id = t;
      }
      if (scope === "user") {
        if (!scopeId.trim()) {
          setErr("scope_id (the user subject) is required for scope=user");
          setBusy(false);
          return;
        }
        body.scope_id = scopeId.trim();
      }
      await putLimit(body);
      setScopeId("");
      setSoft("");
      setHard("");
      await onSaved();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="limits-add" onSubmit={submit}>
      <span className="limits-add-label">add budget</span>
      <select value={scope} onChange={(e) => setScope(e.target.value as Scope)}>
        {SCOPES.map((s) => (
          <option key={s} value={s}>
            {s}
          </option>
        ))}
      </select>
      {scope !== "operator" && (
        <input
          type="text"
          placeholder={tenantFocus ? `tenant (${tenantFocus})` : "tenant id"}
          value={tenantId}
          onChange={(e) => setTenantId(e.target.value)}
          title="Tenant id (admin). Leave blank as a tenant operator — stamped from your identity."
        />
      )}
      {scope === "user" && (
        <input
          type="text"
          placeholder="user subject"
          value={scopeId}
          onChange={(e) => setScopeId(e.target.value)}
        />
      )}
      <input
        type="number"
        min={0}
        step={1000}
        placeholder="soft"
        value={soft}
        onChange={(e) => setSoft(e.target.value)}
      />
      <input
        type="number"
        min={0}
        step={1000}
        placeholder="hard"
        value={hard}
        onChange={(e) => setHard(e.target.value)}
      />
      <button type="submit" className="ghost-btn" disabled={busy}>
        {busy ? "…" : "add"}
      </button>
      {err && <span className="err limits-add-err">{err}</span>}
    </form>
  );
}
