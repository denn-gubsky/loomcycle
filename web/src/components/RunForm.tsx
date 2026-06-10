import { useState } from "react";
import { type StartRunRequest } from "../api";

// RunForm collects the inputs for a single run and calls onSubmit with a
// built StartRunRequest. Presentational — the caller owns the useRunStream
// and decides what to do on submit (single run, or one cell of a fan-out).
// Advanced fields are collapsed by default; allowed_hosts follows the
// trust-boundary semantics (omit = no narrowing, deny-all = []).
export default function RunForm({
  agents,
  defaultUserId,
  submitting,
  onSubmit,
}: {
  agents: string[];
  defaultUserId: string;
  submitting: boolean;
  onSubmit: (req: StartRunRequest) => void;
}) {
  const [agent, setAgent] = useState("");
  const [prompt, setPrompt] = useState("");
  const [userId, setUserId] = useState(defaultUserId);
  const [userTier, setUserTier] = useState("");
  const [allowedHosts, setAllowedHosts] = useState("");
  const [denyAllHosts, setDenyAllHosts] = useState(false);
  const [webSearchFilter, setWebSearchFilter] = useState<"" | "drop" | "keep">("");
  const [metadataJSON, setMetadataJSON] = useState("");
  const [interactive, setInteractive] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setFormErr(null);
    if (!agent) {
      setFormErr("Pick an agent.");
      return;
    }
    if (!prompt.trim()) {
      setFormErr("Enter a prompt.");
      return;
    }

    const req: StartRunRequest = { agent, prompt };
    if (userId.trim()) req.user_id = userId.trim();
    if (userTier.trim()) req.user_tier = userTier.trim();
    // allowed_hosts trust boundary: deny-all wins ([]); else a non-empty
    // list narrows; an empty box means "no narrowing" → omit the field.
    if (denyAllHosts) {
      req.allowed_hosts = [];
    } else {
      const hosts = allowedHosts
        .split(",")
        .map((h) => h.trim())
        .filter(Boolean);
      if (hosts.length > 0) req.allowed_hosts = hosts;
    }
    if (webSearchFilter) req.web_search_filter = webSearchFilter;
    if (interactive) req.interactive = true;
    if (metadataJSON.trim()) {
      try {
        const parsed = JSON.parse(metadataJSON);
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("metadata must be a JSON object");
        }
        req.metadata = parsed as Record<string, unknown>;
      } catch (e) {
        setFormErr(`metadata JSON: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
    }
    onSubmit(req);
  };

  return (
    <form className="run-form" onSubmit={handleSubmit}>
      <div className="library-form-row">
        <label htmlFor="run-agent">agent</label>
        <select
          id="run-agent"
          value={agent}
          onChange={(e) => setAgent(e.target.value)}
          disabled={submitting}
        >
          <option value="">— pick an agent —</option>
          {agents.map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>
      </div>

      <div className="library-form-row">
        <label htmlFor="run-prompt">prompt</label>
        <textarea
          id="run-prompt"
          className="library-prompt-textarea"
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          disabled={submitting}
          rows={6}
          placeholder="What should the agent do?"
        />
      </div>

      <details className="run-form-advanced">
        <summary>advanced</summary>
        <div className="library-form-row library-form-row-quad">
          <label htmlFor="run-user-id">user_id</label>
          <input
            id="run-user-id"
            type="text"
            value={userId}
            onChange={(e) => setUserId(e.target.value)}
            disabled={submitting}
            placeholder="optional"
          />
          <label htmlFor="run-user-tier">user_tier</label>
          <input
            id="run-user-tier"
            type="text"
            value={userTier}
            onChange={(e) => setUserTier(e.target.value)}
            disabled={submitting}
            placeholder="optional (must match a configured tier)"
          />
        </div>
        <div className="library-form-row">
          <label htmlFor="run-hosts">
            allowed_hosts
            <span className="library-modal-field-hint">
              {" "}
              — comma-separated; empty = no narrowing; narrows the operator
              list, never widens
            </span>
          </label>
          <input
            id="run-hosts"
            type="text"
            value={allowedHosts}
            onChange={(e) => setAllowedHosts(e.target.value)}
            disabled={submitting || denyAllHosts}
            placeholder="api.example.com, docs.example.com"
          />
        </div>
        <div className="library-form-row library-form-row-checkbox">
          <label>
            <input
              type="checkbox"
              checked={denyAllHosts}
              onChange={(e) => setDenyAllHosts(e.target.checked)}
              disabled={submitting}
            />{" "}
            deny all network hosts for this run
          </label>
        </div>
        <div className="library-form-row">
          <label htmlFor="run-wsf">web_search_filter</label>
          <select
            id="run-wsf"
            value={webSearchFilter}
            onChange={(e) =>
              setWebSearchFilter(e.target.value as "" | "drop" | "keep")
            }
            disabled={submitting}
          >
            <option value="">(default)</option>
            <option value="drop">drop (only allowed-host results)</option>
            <option value="keep">keep (full result set)</option>
          </select>
        </div>
        <div className="library-form-row">
          <label htmlFor="run-metadata">
            metadata (JSON object, optional)
          </label>
          <textarea
            id="run-metadata"
            className="library-prompt-textarea mono"
            value={metadataJSON}
            onChange={(e) => setMetadataJSON(e.target.value)}
            disabled={submitting}
            rows={3}
            spellCheck={false}
            placeholder='{"repo": "acme/widgets"}'
          />
        </div>
      </details>

      <div className="library-form-row library-form-row-checkbox">
        <label>
          <input
            type="checkbox"
            checked={interactive}
            onChange={(e) => setInteractive(e.target.checked)}
            disabled={submitting}
          />{" "}
          interactive session — stays alive for steering (pair with an
          unbounded-iterations agent; Cancel to end)
        </label>
      </div>

      {formErr && <div className="modal-err">{formErr}</div>}

      <div className="run-form-actions">
        <button type="submit" className="primary" disabled={submitting}>
          {submitting ? "Running…" : "Run agent"}
        </button>
      </div>
    </form>
  );
}
