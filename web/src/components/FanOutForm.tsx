import { useState } from "react";
import { type StartRunRequest } from "../api";

// FanOutForm collects N run specs (each an {agent, prompt} row) and hands
// the built StartRunRequest[] up via onLaunch. The caller fires one run
// per spec concurrently. A shared user_id applies to every run in the
// batch (the common case — one operator launching a team). This is the
// "fire N independent top-level runs" ensemble mode.
interface Row {
  agent: string;
  prompt: string;
}

export default function FanOutForm({
  agents,
  defaultUserId,
  disabled,
  onLaunch,
}: {
  agents: string[];
  defaultUserId: string;
  disabled: boolean;
  onLaunch: (reqs: StartRunRequest[]) => void;
}) {
  const [rows, setRows] = useState<Row[]>([
    { agent: "", prompt: "" },
    { agent: "", prompt: "" },
  ]);
  const [userId, setUserId] = useState(defaultUserId);
  const [formErr, setFormErr] = useState<string | null>(null);

  const update = (i: number, patch: Partial<Row>) => {
    const next = [...rows];
    next[i] = { ...next[i]!, ...patch };
    setRows(next);
  };

  const handleLaunch = () => {
    setFormErr(null);
    const filled = rows.filter((r) => r.agent && r.prompt.trim());
    if (filled.length === 0) {
      setFormErr("Add at least one row with an agent and a prompt.");
      return;
    }
    const reqs: StartRunRequest[] = filled.map((r) => {
      const req: StartRunRequest = { agent: r.agent, prompt: r.prompt };
      if (userId.trim()) req.user_id = userId.trim();
      return req;
    });
    onLaunch(reqs);
  };

  return (
    <div className="fanout-form">
      <div className="library-form-row">
        <label htmlFor="fanout-user">user_id (shared, optional)</label>
        <input
          id="fanout-user"
          type="text"
          value={userId}
          onChange={(e) => setUserId(e.target.value)}
          disabled={disabled}
          placeholder="optional"
        />
      </div>

      <div className="fanout-rows">
        {rows.map((r, i) => (
          <div key={i} className="fanout-row">
            <select
              value={r.agent}
              onChange={(e) => update(i, { agent: e.target.value })}
              disabled={disabled}
            >
              <option value="">— agent —</option>
              {agents.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
            <textarea
              value={r.prompt}
              onChange={(e) => update(i, { prompt: e.target.value })}
              disabled={disabled}
              rows={2}
              placeholder="prompt for this run"
            />
            <button
              type="button"
              title="remove row"
              onClick={() => setRows(rows.filter((_, idx) => idx !== i))}
              disabled={disabled || rows.length === 1}
            >
              ×
            </button>
          </div>
        ))}
      </div>

      <div className="fanout-actions">
        <button
          type="button"
          onClick={() => setRows([...rows, { agent: "", prompt: "" }])}
          disabled={disabled}
        >
          + add run
        </button>
        <button
          type="button"
          className="primary"
          onClick={handleLaunch}
          disabled={disabled}
        >
          Launch all
        </button>
      </div>

      {formErr && <div className="modal-err">{formErr}</div>}
    </div>
  );
}
