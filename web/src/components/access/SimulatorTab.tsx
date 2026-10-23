import { useState, type FormEvent } from "react";
import { authzEvaluate, authzExplain, authzSimulate, type AuthzDecision, type AuthzExplain, type AuthzSimulateResult } from "../../api";
import { catalogType, evaluationRequest, parseDraftDoc, selectorShape, type AclCatalog } from "../../lib/aclRules";
import ErrorNotice from "../ErrorNotice";

// SimulatorTab — "as <subject>, may I <action> <resource> [in this context]?"
// Saved state: the decision from /v1/_authz/evaluation and the rule trail from
// /v1/_authz/explain. Draft mode: the same question against the stored state
// plus unsaved lists and bindings via /v1/_authz/simulate — now vs with the
// draft. Nothing is written in either mode.

function idPlaceholder(cat: AclCatalog, type: string): string {
  const t = catalogType(cat, type);
  if (!t) return "";
  switch (selectorShape(t)) {
    case "glob":
      return `name, e.g. ${type === "tool" ? "Bash" : type === "agent" ? "chat/support" : "name"} (blank = any)`;
    case "enum":
      return `one of ${t.scopes!.join(", ")}`;
    case "scoped":
      return `${t.scopes![0]}${t.separator}${t.separator === ":" ? "/path" : "name"} (blank = any)`;
  }
}

export default function SimulatorTab({
  tenant,
  catalog,
  draft,
  onDraft,
  useDraft,
  onUseDraft,
}: {
  tenant?: string;
  catalog: AclCatalog | null;
  draft: string;
  onDraft: (text: string) => void;
  useDraft: boolean;
  onUseDraft: (on: boolean) => void;
}) {
  const [subject, setSubject] = useState("");
  const [type, setType] = useState("agent");
  const [action, setAction] = useState("run");
  const [id, setId] = useState("");
  const [owner, setOwner] = useState("");
  const [transport, setTransport] = useState("");
  const [interactive, setInteractive] = useState(false);
  const [agent, setAgent] = useState("");
  const [depth, setDepth] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [saved, setSaved] = useState<{ decision: AuthzDecision; explain: AuthzExplain } | null>(null);
  const [sim, setSim] = useState<{ result: AuthzSimulateResult; missing: string[] } | null>(null);

  if (!catalog) return <div className="settings-muted">loading the catalog…</div>;
  const verbs = catalogType(catalog, type)?.verbs ?? [];

  const ask = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setErr(null);
    setSaved(null);
    setSim(null);
    const req = evaluationRequest({ subject, action, type, id, owner, context: { transport, interactive, agent, depth } });
    setBusy(true);
    try {
      if (useDraft) {
        const doc = parseDraftDoc(draft);
        if (!doc.ok) {
          setErr(new Error(`draft: ${doc.error}`));
          return;
        }
        const r = await authzSimulate({ subject: req.subject, acls: doc.acls, bindings: doc.bindings, requests: [req] }, tenant);
        const first = (r.results ?? [])[0];
        if (first) setSim({ result: first, missing: r.missing_acls ?? [] });
      } else {
        const [decision, explain] = await Promise.all([authzEvaluate(req, tenant), authzExplain(req, tenant)]);
        setSaved({ decision, explain });
      }
    } catch (e2) {
      setErr(e2);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="acl-tab">
      <form className="acl-sim-form" onSubmit={ask}>
        <div className="acl-sim-question">
          <span>as</span>
          <input type="text" aria-label="subject" placeholder="subject (blank = you)" value={subject} onChange={(e) => setSubject(e.target.value)} />
          <span>may I</span>
          <select aria-label="action" value={action} onChange={(e) => setAction(e.target.value)}>
            {verbs.map((v) => (
              <option key={v} value={v}>
                {v}
              </option>
            ))}
          </select>
          <select
            aria-label="resource type"
            value={type}
            onChange={(e) => {
              const next = e.target.value;
              setType(next);
              const nv = catalogType(catalog, next)?.verbs ?? [];
              if (!nv.includes(action)) setAction(nv[0] ?? "");
              setId("");
            }}
          >
            {catalog.types.map((t) => (
              <option key={t.type} value={t.type}>
                {t.type}
              </option>
            ))}
          </select>
          <span>:</span>
          <input type="text" aria-label="resource id" placeholder={idPlaceholder(catalog, type)} value={id} onChange={(e) => setId(e.target.value)} />
          <span>?</span>
        </div>
        <details className="acl-sim-context">
          <summary>context (optional)</summary>
          <div className="acl-form-row">
            <label className="acl-inline-label">
              transport
              <select aria-label="transport" value={transport} onChange={(e) => setTransport(e.target.value)}>
                <option value="">this request’s</option>
                {catalog.transports.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </label>
            <label className="acl-check">
              <input type="checkbox" checked={interactive} onChange={(e) => setInteractive(e.target.checked)} /> interactive run
            </label>
            <input type="text" aria-label="resource owner" placeholder="resource owner (for owner_is_self)" value={owner} onChange={(e) => setOwner(e.target.value)} />
            <input type="text" aria-label="run agent" placeholder="inside a run of agent…" value={agent} onChange={(e) => setAgent(e.target.value)} />
            <input type="text" aria-label="spawn depth" inputMode="numeric" placeholder="spawn depth" value={depth} onChange={(e) => setDepth(e.target.value)} />
          </div>
        </details>
        <div className="acl-form-row">
          <label className="acl-check">
            <input type="checkbox" checked={useDraft} onChange={(e) => onUseDraft(e.target.checked)} /> evaluate against a draft (unsaved lists and
            bindings — nothing is written)
          </label>
          <button type="submit" className="primary-btn" disabled={busy || !action}>
            {busy ? "asking…" : "ask"}
          </button>
        </div>
        {useDraft && (
          <div className="acl-sim-draft">
            <p className="settings-help">
              YAML with <code>acls</code> (lists added or replacing a stored list of the same name) and <code>bindings</code> (added to the
              stored ones). “simulate unsaved” in the Policies editor fills this in.
            </p>
            <textarea
              className="acl-text"
              aria-label="draft"
              spellCheck={false}
              rows={12}
              value={draft}
              placeholder={"acls:\n  - name: interns\n    rules:\n      - effect: deny\n        actions: [invoke]\n        resources: ['tool:Bash']\nbindings:\n  - acl: interns\n    subject_kind: user\n    subject_id: alice"}
              onChange={(e) => onDraft(e.target.value)}
            />
          </div>
        )}
      </form>
      <ErrorNotice error={err} />
      {saved && <SavedAnswer decision={saved.decision} explain={saved.explain} />}
      {sim && <DraftAnswer result={sim.result} missing={sim.missing} />}
    </div>
  );
}

export function OutcomePill({ d }: { d: AuthzDecision }) {
  const cls = d.outcome === "allow" ? "status-ok" : d.outcome === "deny" ? "acl-pill-deny" : "status-warn";
  return <span className={"status-pill " + cls}>{d.outcome === "no_match" ? "no rule matched" : d.outcome}</span>;
}

function outcomeNote(d: AuthzDecision): string {
  if (d.outcome === "no_match") {
    return d.mode === "enforce"
      ? "No rule answers this; in enforce mode that is a refusal."
      : `No rule answers this; in ${d.mode} mode the token’s scopes decide.`;
  }
  if (d.tenant_floor) return "Refused by the tenant floor: the resource is in another tenant.";
  return d.outcome === "allow" ? "A rule allows it and no rule denies it." : "A rule denies it; a deny beats every allow.";
}

export function SavedAnswer({ decision, explain }: { decision: AuthzDecision; explain: AuthzExplain }) {
  const traces = explain.traces ?? [];
  const applied = explain.applied_bindings ?? [];
  const missing = explain.missing_acls ?? [];
  return (
    <div className="acl-answer">
      <div className="acl-answer-head">
        <OutcomePill d={decision} /> <span className="settings-muted">mode {decision.mode}</span>
      </div>
      <p className="settings-help">{outcomeNote(decision)}</p>
      {(decision.reasons ?? []).length > 0 && (
        <div>
          decided by{" "}
          {(decision.reasons ?? []).map((r, i) => (
            <span key={i} className="acl-reason">
              <code>{r.acl}</code> rule {r.rule} ({r.effect})
            </span>
          ))}
        </div>
      )}
      <h4>rule trail</h4>
      {traces.length === 0 ? (
        <div className="settings-muted">no rule of an applicable list names this resource type.</div>
      ) : (
        <table className="settings-table acl-table">
          <thead>
            <tr>
              <th>list</th>
              <th>rule</th>
              <th>effect</th>
              <th>matched</th>
              <th>why</th>
            </tr>
          </thead>
          <tbody>
            {traces.map((t, i) => (
              <tr key={i} className={t.matched ? "acl-trace-matched" : undefined}>
                <td>
                  <code>{t.acl}</code>
                </td>
                <td>{t.rule}</td>
                <td>{t.effect}</td>
                <td>{t.matched ? "yes" : "no"}</td>
                <td className="settings-muted">{t.why}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="acl-answer-meta">
        <div>
          groups: {(explain.groups ?? []).length ? (explain.groups ?? []).map((g) => <code key={g}>{g} </code>) : <span className="settings-muted">none</span>}
        </div>
        <div>
          bindings applied:{" "}
          {applied.length ? (
            applied.map((b, i) => (
              <span key={i} className="acl-reason">
                <code>{b.acl}</code> → {b.subject_kind}
                {b.subject_id ? ` ${b.subject_id}` : ""}
              </span>
            ))
          ) : (
            <span className="settings-muted">none</span>
          )}
        </div>
        {missing.length > 0 && <div className="settings-error">bound but not found (bind to a list that exists): {missing.join(", ")}</div>}
      </div>
    </div>
  );
}

export function DraftAnswer({ result, missing }: { result: AuthzSimulateResult; missing: string[] }) {
  return (
    <div className="acl-answer">
      <div className="acl-answer-head">
        now <OutcomePill d={result.now} /> → with the draft <OutcomePill d={result.with_draft} />{" "}
        <span className={result.changed ? "acl-changed" : "settings-muted"}>{result.changed ? "the draft changes the answer" : "no change"}</span>
      </div>
      <p className="settings-help">{outcomeNote(result.with_draft)}</p>
      {(result.with_draft.reasons ?? []).length > 0 && (
        <div>
          with the draft, decided by{" "}
          {(result.with_draft.reasons ?? []).map((r, i) => (
            <span key={i} className="acl-reason">
              <code>{r.acl}</code> rule {r.rule} ({r.effect})
            </span>
          ))}
        </div>
      )}
      {missing.length > 0 && <div className="settings-error">bound but not found: {missing.join(", ")}</div>}
    </div>
  );
}
