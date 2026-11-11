import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  deleteTelemetry,
  getTelemetry,
  putTelemetry,
  testTelemetry,
  type TelemetryPolicy,
  type TelemetryPolicyInput,
  type TelemetryResult,
  type TelemetryStatus,
  type TelemetryTest,
} from "../api";
import { ApiError } from "../lib/apiError";
import { usePrincipal } from "../components/Layout";
import ErrorNotice from "../components/ErrorNotice";
import TenantCombobox from "../components/TenantCombobox";

// TelemetryView — a tenant's own trace export: where the tenant's spans go beside
// the operator's. An admin manages any tenant (picked like the Limits page's
// focus); a tenant operator its own. The server holds every rule — the page only
// catches an empty endpoint and an out-of-range ratio, so the rule and its
// wording stay in one place and a refusal shows the server's own sentence.

const PROTOCOLS = ["http/protobuf", "grpc"] as const;

// isTelemetryDisabled is the operator's gate being off: every route answers
// 404 feature_disabled then, which is a state of the deployment, not an error.
export function isTelemetryDisabled(e: unknown): boolean {
  return e instanceof ApiError && e.status === 404 && e.code === "feature_disabled";
}

// parseRatio reads the sampler ratio box: "" means the server's default, a
// number in [0, 1] is sent, anything else is refused before the round trip.
export function parseRatio(text: string): number | undefined | null {
  const t = text.trim();
  if (t === "") return undefined;
  const n = Number(t);
  return Number.isFinite(n) && n >= 0 && n <= 1 ? n : null;
}

export default function TelemetryView() {
  const principal = usePrincipal();
  // A null principal is open mode, which is admin-equivalent.
  const isAdmin = !principal || principal.is_admin;
  const [tenant, setTenant] = useState("");
  const [result, setResult] = useState<TelemetryResult | null>(null);
  const [disabled, setDisabled] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [flash, setFlash] = useState("");
  const [busy, setBusy] = useState(false);

  // run wraps every call the same way: the disabled gate is a page state, any
  // other refusal is shown, and a call's result replaces the page's view.
  const run = useCallback(async (call: () => Promise<TelemetryResult>, done?: string) => {
    setBusy(true);
    setFlash("");
    try {
      const res = await call();
      setResult(res);
      setDisabled(false);
      setErr(null);
      if (done) setFlash(done);
    } catch (e) {
      if (isTelemetryDisabled(e)) {
        setDisabled(true);
        setResult(null);
        setErr(null);
      } else {
        setErr(e);
      }
    } finally {
      setBusy(false);
    }
  }, []);

  const load = useCallback((t: string) => run(() => getTelemetry(t.trim() || undefined)), [run]);

  useEffect(() => {
    void load("");
    // Once on mount; later loads follow the tenant field's refresh and picks, so
    // a half-typed tenant id does not query the endpoint per keystroke.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (disabled) return <TelemetryDisabled />;

  // Writes address the tenant the page shows, so a tenant typed but not yet
  // loaded cannot receive a policy written while another tenant's is on screen.
  const shown = result?.tenant || undefined;
  const policy = result?.policy ?? null;

  return (
    <div className="settings-panel telemetry-view">
      <h2>Telemetry</h2>
      <p className="settings-help">
        Send this tenant&rsquo;s own traces to an OpenTelemetry collector of yours, beside the
        operator&rsquo;s. Only the tenant&rsquo;s spans are sent, with a minimal resource and no
        error text; runs that keep no trace are never sent.
      </p>

      <div className="telemetry-tenant">
        {isAdmin ? (
          <>
            <label>
              tenant
              <TenantCombobox
                value={tenant}
                onChange={setTenant}
                onPick={(t) => {
                  setTenant(t);
                  void load(t);
                }}
                enabled={isAdmin}
                placeholder="tenant id"
              />
            </label>
            <button type="button" className="ghost-btn" onClick={() => void load(tenant)} disabled={busy}>
              {busy ? "loading…" : "load"}
            </button>
          </>
        ) : (
          result && (
            <span className="settings-muted">
              tenant <code>{result.tenant}</code>
            </span>
          )
        )}
      </div>

      <ErrorNotice error={err} />
      {flash && <div className="settings-flash">{flash}</div>}

      {result && (
        <>
          <TelemetryForm
            // Remount on a new stored policy so the draft starts from it.
            key={`${result.tenant}|${policy?.updated_at ?? ""}|${policy ? "p" : "-"}`}
            policy={policy}
            busy={busy}
            onSave={(input) => run(() => putTelemetry(input, shown), "policy saved")}
            onRemove={() => {
              if (!window.confirm(`Stop exporting tenant "${result.tenant}"'s traces and remove its telemetry policy?`)) {
                return;
              }
              void run(() => deleteTelemetry(shown), "policy removed");
            }}
            onTest={() => run(() => testTelemetry(shown))}
          />
          {result.test && <TelemetryTestOutcome test={result.test} />}
          {result.status && <TelemetryStatusBlock status={result.status} />}
        </>
      )}
    </div>
  );
}

export function TelemetryDisabled() {
  return (
    <div className="settings-panel telemetry-view">
      <h2>Telemetry</h2>
      <p className="settings-help">
        Tenant telemetry export is not enabled on this deployment (the operator sets{" "}
        <code>LOOMCYCLE_TENANT_TELEMETRY=1</code>).
      </p>
    </div>
  );
}

export function TelemetryForm({
  policy,
  busy,
  onSave,
  onRemove,
  onTest,
}: {
  policy: TelemetryPolicy | null;
  busy: boolean;
  onSave: (input: TelemetryPolicyInput) => Promise<void> | void;
  onRemove: () => void;
  onTest: () => Promise<void> | void;
}) {
  const [endpoint, setEndpoint] = useState(policy?.endpoint ?? "");
  const [protocol, setProtocol] = useState(policy?.protocol || PROTOCOLS[0]);
  const [credential, setCredential] = useState(policy?.headers_credential ?? "");
  const [ratio, setRatio] = useState(policy ? String(policy.sampler_ratio) : "");
  const [err, setErr] = useState("");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const r = parseRatio(ratio);
    if (!endpoint.trim()) {
      setErr("endpoint is required: the OTLP base URL of your collector");
      return;
    }
    if (r === null) {
      setErr("sampler ratio must be a number from 0 to 1");
      return;
    }
    setErr("");
    const input: TelemetryPolicyInput = { endpoint: endpoint.trim(), protocol, signals: ["traces"] };
    if (credential.trim()) input.headers_credential = credential.trim();
    if (r !== undefined) input.sampler_ratio = r;
    void onSave(input);
  };

  return (
    <form className="telemetry-form" onSubmit={submit}>
      <label>
        endpoint
        <input
          name="endpoint"
          type="text"
          inputMode="url"
          value={endpoint}
          placeholder="https://otel.example.com:4318"
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setEndpoint(e.target.value)}
        />
        <span className="settings-muted">
          The collector&rsquo;s OTLP base URL. It must be https unless the operator allows http for a
          lab; a grpc endpoint is a host and port with no path.
        </span>
      </label>
      <label>
        protocol
        <select name="protocol" value={protocol} onChange={(e) => setProtocol(e.target.value)}>
          {PROTOCOLS.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
      </label>
      <label>
        headers credential
        <input
          name="headers_credential"
          value={credential}
          placeholder="(none)"
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setCredential(e.target.value)}
        />
        <span className="settings-muted">
          The name of a tenant credential (tenant credentials page) whose value is a header block,{" "}
          <code>name=value,name=value</code> — for example <code>authorization=Basic …,x-scope-orgid=…</code>.
          Its allowed URLs must restrict it to this endpoint.
        </span>
      </label>
      <label>
        sampler ratio
        <input
          name="sampler_ratio"
          type="number"
          min={0}
          max={1}
          // "any": a stored ratio such as 0.333 is valid, and a fixed step would
          // have the browser refuse to submit it.
          step="any"
          value={ratio}
          placeholder="1"
          onChange={(e) => setRatio(e.target.value)}
        />
        <span className="settings-muted">
          The share of traces sent, 0 to 1. Empty means 1; the operator may cap it lower.
        </span>
      </label>
      <fieldset className="telemetry-signals">
        <legend>signals</legend>
        <label>
          <input type="checkbox" checked disabled readOnly /> traces
        </label>
        <span className="settings-muted">Only traces are exported for now.</span>
      </fieldset>
      <p className="settings-muted">
        Content capture is not available: it is always off for a tenant&rsquo;s export.
      </p>
      {err && <div className="settings-error">{err}</div>}
      <div className="settings-row-actions">
        <button type="submit" disabled={busy}>
          save
        </button>
        <button type="button" className="ghost-btn" onClick={() => void onTest()} disabled={busy || !policy}>
          send test span
        </button>
        <button type="button" className="ghost-btn" onClick={onRemove} disabled={busy || !policy}>
          remove
        </button>
      </div>
      {policy?.updated_at && (
        <div className="settings-muted">
          saved <time dateTime={policy.updated_at}>{policy.updated_at}</time>
          {policy.updated_by && <> by {policy.updated_by}</>}
        </div>
      )}
    </form>
  );
}

// TelemetryTestOutcome shows the server's error text verbatim: it is already
// the part that is the tenant's to read (the collector's answer, a refused
// credential), and paraphrasing it would lose the detail that fixes it.
export function TelemetryTestOutcome({ test }: { test: TelemetryTest }) {
  return test.ok ? (
    <div className="settings-flash" role="status">
      test span delivered in {test.duration_ms} ms
    </div>
  ) : (
    <div className="settings-error" role="alert">
      test span failed after {test.duration_ms} ms: {test.error || "no reason given"}
    </div>
  );
}

export function TelemetryStatusBlock({ status }: { status: TelemetryStatus }) {
  return (
    <>
      <h3 className="settings-subhead">
        Export status
        {status.replica && (
          <span className="settings-muted">
            {" "}
            on replica <code>{status.replica}</code>
          </span>
        )}
      </h3>
      <p className="settings-help">Each replica exports on its own, so these counts are this replica&rsquo;s alone.</p>
      <dl className="settings-stats">
        <div>
          <dt>exporting</dt>
          <dd>{status.exporting ? "yes" : "no"}</dd>
        </div>
        <div>
          <dt>exported spans</dt>
          <dd>{status.exported_spans}</dd>
        </div>
        <div>
          <dt>dropped spans</dt>
          <dd>{status.dropped_spans}</dd>
        </div>
        <div>
          <dt>export failures</dt>
          <dd>{status.export_failures}</dd>
        </div>
        <div>
          <dt>last success</dt>
          <dd className="telemetry-time">
            {status.last_success_at ? <time dateTime={status.last_success_at}>{status.last_success_at}</time> : "—"}
          </dd>
        </div>
      </dl>
      {status.last_error && (
        <div className="settings-error">
          last error
          {status.last_error_at && (
            <>
              {" "}
              at <time dateTime={status.last_error_at}>{status.last_error_at}</time>
            </>
          )}
          : {status.last_error}
        </div>
      )}
    </>
  );
}
