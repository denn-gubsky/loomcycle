import { useEffect, useRef, useState } from "react";
import {
  CredentialMeta,
  CredentialScope,
  oauthConnectStart,
  oauthConnectStatus,
  oauthDisconnect,
} from "../api";
import { refusalText } from "../lib/cryptoKeys";
import {
  ConnectDeps,
  ConnectPhase,
  OAUTH_CODE_HINT,
  abortableSleep,
  connectLabel,
  runOAuthConnect,
  stateBadge,
} from "../lib/oauthConnect";

const browserDeps: ConnectDeps = {
  start: oauthConnectStart,
  status: oauthConnectStatus,
  sleep: abortableSleep,
  now: () => Date.now(),
};

// OAuthConnect is an OAuth credential's detail in the credentials table: its
// state, the account it is connected as, and Connect / Reconnect /
// Disconnect. A connect shows the code to approve and watches the credential
// until it connects, is refused or the code expires. Only the newest connect
// is watched: starting again abandons the one before, as the server does.
export function OAuthConnect({
  meta,
  scope,
  onChanged,
  deps = browserDeps,
}: {
  meta: CredentialMeta;
  scope: CredentialScope;
  // Called after the credential's state changed here (connected, refused,
  // disconnected), so the page reloads its list.
  onChanged: () => void;
  deps?: ConnectDeps;
}) {
  const [phase, setPhase] = useState<ConnectPhase | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const flight = useRef<AbortController | null>(null);
  const onChangedRef = useRef(onChanged);
  onChangedRef.current = onChanged;

  // Leaving the page stops watching; the server keeps polling on its own.
  useEffect(() => () => flight.current?.abort(), []);

  const ref = { scope, name: meta.name };
  const o = meta.oauth;
  const badge = stateBadge(o?.state);
  const watching = phase?.kind === "starting" || phase?.kind === "waiting";

  const connect = () => {
    flight.current?.abort();
    const ctl = new AbortController();
    flight.current = ctl;
    setErr(null);
    setCopied(false);
    void runOAuthConnect(
      ref,
      deps,
      (p) => {
        setPhase(p);
        if (p.kind === "connected" || p.kind === "failed" || p.kind === "expired") onChangedRef.current();
      },
      ctl.signal,
    );
  };

  const disconnect = async () => {
    if (
      !confirm(
        `Disconnect the ${scope} OAuth credential "${meta.name}"? Its grant is revoked at the authorization server, and anything naming $oauth:${meta.name} stops working until it is connected again.`,
      )
    ) {
      return;
    }
    flight.current?.abort();
    setPhase(null);
    setBusy(true);
    setErr(null);
    try {
      await oauthDisconnect(ref);
      onChangedRef.current();
    } catch (e) {
      setErr(refusalText(e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  };

  const copyCode = async (code: string) => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className="oauth-connect">
      <span className={`status-pill ${badge.className}`} title={badge.title}>
        OAuth · {badge.label}
      </span>
      {o?.resource && <div className="settings-muted">for {o.resource}</div>}
      {o?.state === "connected" && o.connected_as && (
        <div className="settings-muted">connected as {o.connected_as}</div>
      )}
      {o?.state !== "connected" && o?.last_error && !watching && (
        <div className="settings-muted">{o.last_error}</div>
      )}
      <div className="oauth-connect-actions">
        <button type="button" className="ghost-btn" onClick={connect} disabled={busy || phase?.kind === "starting"}>
          {watching ? "Get a new code" : connectLabel(o?.state)}
        </button>{" "}
        {o?.state && o.state !== "revoked" && (
          <button type="button" className="ghost-btn danger" onClick={() => void disconnect()} disabled={busy}>
            Disconnect
          </button>
        )}
      </div>
      {err && <div className="settings-error">{err}</div>}
      {phase && <ConnectPhaseView phase={phase} copied={copied} onCopy={copyCode} onRestart={connect} />}
    </div>
  );
}

// ConnectPhaseView renders one step of a connect.
export function ConnectPhaseView({
  phase,
  copied,
  onCopy,
  onRestart,
}: {
  phase: ConnectPhase;
  copied: boolean;
  onCopy: (code: string) => void;
  onRestart: () => void;
}) {
  switch (phase.kind) {
    case "starting":
      return <div className="settings-muted">asking the authorization server for a code…</div>;
    case "waiting":
      return (
        <div className="token-reveal oauth-code" role="status">
          <div className="token-reveal-head">
            <strong>Approve this code to connect</strong>
            <span className="token-reveal-warn">{OAUTH_CODE_HINT}</span>
          </div>
          <div className="token-reveal-row">
            <code className="token-secret">{phase.userCode}</code>
            <button type="button" className="primary-btn" onClick={() => onCopy(phase.userCode)}>
              {copied ? "copied ✓" : "copy code"}
            </button>
            {phase.link && (
              <a className="ghost-btn" href={phase.link} target="_blank" rel="noopener noreferrer">
                open sign-in page
              </a>
            )}
          </div>
          <div className="token-reveal-meta">
            {phase.link ? "Opens " : "Open "}
            <code>{phase.verificationURI}</code>, enter the code and approve. Waiting for the approval…
          </div>
        </div>
      );
    case "connected":
      return (
        <div className="settings-flash">
          {phase.connectedAs ? `Connected as ${phase.connectedAs}.` : "Connected."}
        </div>
      );
    case "expired":
      return (
        <div className="settings-error">
          The code expired before it was approved.{" "}
          <button type="button" className="ghost-btn" onClick={onRestart}>
            Get a new code
          </button>{" "}
          {OAUTH_CODE_HINT}
        </div>
      );
    case "failed":
      return <div className="settings-error">Not connected: {phase.reason}</div>;
  }
}
