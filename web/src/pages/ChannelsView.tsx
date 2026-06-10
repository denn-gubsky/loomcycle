import { useEffect, useMemo, useState } from "react";
import {
  awaitChannels,
  broadcastChannels,
  ChannelAwaitMode,
  ChannelDescriptor,
  ChannelMessageItem,
  ChannelScope,
  deleteChannel,
  listChannels,
  peekChannel,
  publishChannel,
} from "../api";
import Splitter from "../components/Splitter";
import ChannelEditModal from "../components/ChannelEditModal";

// ChannelsView is the v0.9.x Introspection surface for operator-
// declared channels. Three things together:
//
//   1. Aggregate stats per channel (message_count, oldest/newest
//      visible_at) — what's flowing where.
//   2. Per-scope filtering (system / global / user / agent) — so
//      operators can narrow to e.g. only _system/* channels.
//   3. Per-channel message peek — non-destructive read of recent
//      messages so operators can verify content shape without
//      hooking up a subscriber.

const REFRESH_MS = 10_000;

export default function ChannelsView() {
  const [channels, setChannels] = useState<ChannelDescriptor[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [filter, setFilter] = useState<FilterKind>("all");
  const [selectedName, setSelectedName] = useState<string>("");
  const [modalState, setModalState] = useState<
    | { kind: "create" }
    | { kind: "edit"; channel: ChannelDescriptor }
    | null
  >(null);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const [refreshTick, setRefreshTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const r = await listChannels();
        if (cancelled) return;
        setChannels(r.channels ?? []);
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [refreshTick]);

  const triggerRefresh = () => setRefreshTick((n) => n + 1);

  const handleDelete = async (channel: ChannelDescriptor) => {
    if (channel.source === "yaml") {
      setDeleteErr(
        `Cannot delete ${channel.name}: declared in operator yaml. Edit the yaml + restart instead.`,
      );
      return;
    }
    if (
      !window.confirm(
        `Delete channel "${channel.name}"? This also removes its persisted messages + cursors.`,
      )
    ) {
      return;
    }
    setDeleteErr(null);
    try {
      await deleteChannel(channel.name);
      triggerRefresh();
      if (selectedName === channel.name) setSelectedName("");
    } catch (e) {
      setDeleteErr(e instanceof Error ? e.message : String(e));
    }
  };

  const visible = useMemo(() => filterChannels(channels, filter), [channels, filter]);

  // Default-select the first visible row when nothing is selected, so
  // the right pane has content from page load.
  useEffect(() => {
    if (selectedName && visible.find((c) => c.name === selectedName)) return;
    setSelectedName(visible.length > 0 ? visible[0]!.name : "");
  }, [visible, selectedName]);

  const selected = useMemo(
    () => channels.find((c) => c.name === selectedName),
    [channels, selectedName],
  );

  return (
    <div className="channels-view">
      <div className="channels-toolbar">
        <FilterChips current={filter} onChange={setFilter} />
        <span className="channels-count">{visible.length} channels</span>
        <button
          type="button"
          className="primary channels-new-button"
          onClick={() => setModalState({ kind: "create" })}
        >
          + New channel
        </button>
      </div>
      {err && <div className="error-banner">Failed to load channels: {err}</div>}
      {deleteErr && <div className="error-banner">{deleteErr}</div>}
      <Splitter
        storageKey="loomcycle.channels.split"
        defaultLeftWidth={420}
        minLeftWidth={300}
        minRightWidth={360}
      >
        <ChannelsList
          channels={visible}
          selectedName={selectedName}
          onSelect={setSelectedName}
          onEdit={(c) => setModalState({ kind: "edit", channel: c })}
          onDelete={handleDelete}
        />
        {selected ? (
          <ChannelDetail channel={selected} />
        ) : (
          <div className="empty-state">Select a channel to inspect.</div>
        )}
      </Splitter>

      {modalState && (
        <ChannelEditModal
          mode={modalState.kind === "create" ? "create" : "edit"}
          existing={modalState.kind === "edit" ? modalState.channel : undefined}
          onClose={() => setModalState(null)}
          onSaved={(desc) => {
            setModalState(null);
            triggerRefresh();
            setSelectedName(desc.name);
          }}
        />
      )}
    </div>
  );
}

// v0.11.12 — extend the filter axis to include `runtime` and `orphan`
// source-tag filters. v0.11.5 added the channels CRUD substrate with
// every row carrying a `source: "yaml" | "runtime" | "orphan"` tag,
// but the filter row only had scope-based chips. Operators wanting to
// see "just the runtime-created channels" had to scan visually for
// the source chip on each row. The new chips filter by c.source
// directly.
type FilterKind =
  | "all"
  | "system"
  | "global"
  | "user"
  | "agent"
  | "yaml"
  | "runtime"
  | "orphan";

function FilterChips({
  current,
  onChange,
}: {
  current: FilterKind;
  onChange: (f: FilterKind) => void;
}) {
  const opts: { kind: FilterKind; label: string }[] = [
    { kind: "all", label: "all" },
    // scope filters (match c.scope)
    { kind: "system", label: "_system/*" },
    { kind: "global", label: "global" },
    { kind: "user", label: "user" },
    { kind: "agent", label: "agent" },
    // source-tag filters (match c.source)
    { kind: "yaml", label: "yaml" },
    { kind: "runtime", label: "runtime" },
    { kind: "orphan", label: "orphan" },
  ];
  return (
    <div className="filter-chips">
      {opts.map((o) => (
        <button
          key={o.kind}
          type="button"
          className={
            o.kind === current ? "filter-chip filter-chip-active" : "filter-chip"
          }
          onClick={() => onChange(o.kind)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

// sourceFilters is the closed set of source-tag filter values; the
// rest fall through to scope-based filtering. Keep in sync with the
// FilterKind type + the chip set above.
const sourceFilters: Record<string, boolean> = {
  yaml: true,
  runtime: true,
  orphan: true,
};

function filterChannels(channels: ChannelDescriptor[], f: FilterKind): ChannelDescriptor[] {
  if (f === "all") return channels;
  if (f === "system") return channels.filter((c) => c.name.startsWith("_system/"));
  if (sourceFilters[f]) return channels.filter((c) => c.source === f);
  return channels.filter((c) => c.scope === f);
}

function ChannelsList({
  channels,
  selectedName,
  onSelect,
  onEdit,
  onDelete,
}: {
  channels: ChannelDescriptor[];
  selectedName: string;
  onSelect: (name: string) => void;
  onEdit: (c: ChannelDescriptor) => void;
  onDelete: (c: ChannelDescriptor) => void;
}) {
  if (channels.length === 0) {
    return (
      <div className="empty-state">No channels match the current filter.</div>
    );
  }
  return (
    <ul className="channels-list">
      {channels.map((c) => {
        const isRuntime = c.source === "runtime";
        return (
          <li
            key={c.name}
            className={
              c.name === selectedName ? "channel-row channel-row-selected" : "channel-row"
            }
          >
            <button
              type="button"
              className="channel-row-button"
              onClick={() => onSelect(c.name)}
            >
              <span className="channel-name">{c.name}</span>
              <span className="channel-meta">
                {c.source && (
                  <span className={`channel-source channel-source-${c.source}`}>
                    {c.source}
                  </span>
                )}
                {c.scope && <span className="channel-scope">{c.scope}</span>}
                {c.semantic && <span className="channel-semantic">{c.semantic}</span>}
                <span className="channel-count">
                  {c.message_count} msg{c.message_count === 1 ? "" : "s"}
                </span>
              </span>
            </button>
            {isRuntime && (
              <span className="channel-row-actions">
                <button
                  type="button"
                  className="channel-row-action"
                  title="Edit channel"
                  onClick={(e) => {
                    e.stopPropagation();
                    onEdit(c);
                  }}
                >
                  Edit
                </button>
                <button
                  type="button"
                  className="channel-row-action channel-row-action-danger"
                  title="Delete channel"
                  onClick={(e) => {
                    e.stopPropagation();
                    onDelete(c);
                  }}
                >
                  Delete
                </button>
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function ChannelDetail({ channel }: { channel: ChannelDescriptor }) {
  // Peek is only well-defined for scope=global channels via this
  // endpoint. We attempt the peek and gracefully show an empty state
  // with an explanation when the channel uses a different scope.
  const [messages, setMessages] = useState<ChannelMessageItem[]>([]);
  const [peekErr, setPeekErr] = useState<string | null>(null);
  const [peekLoading, setPeekLoading] = useState(false);
  // Bumped after a successful publish to re-peek and show the new message.
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setPeekLoading(true);
    setPeekErr(null);
    peekChannel(channel.name, { maxMessages: 20 })
      .then((r) => {
        if (cancelled) return;
        setMessages(r.messages ?? []);
        setPeekLoading(false);
      })
      .catch((e: Error) => {
        if (cancelled) return;
        setPeekErr(e.message);
        setPeekLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [channel.name, reloadKey]);

  return (
    <div className="channel-detail">
      <div className="channel-detail-header">
        <h3>{channel.name}</h3>
        {channel.description && (
          <div className="channel-detail-description">{channel.description}</div>
        )}
        <div className="channel-detail-meta">
          {channel.source && (
            <span className={`channel-source channel-source-${channel.source}`}>
              {channel.source}
            </span>
          )}
          {channel.scope && <span>scope={channel.scope}</span>}
          {channel.semantic && <span>semantic={channel.semantic}</span>}
          {channel.publisher && <span>publisher={channel.publisher}</span>}
          {channel.period && <span>period={channel.period}</span>}
          {channel.default_ttl !== undefined && channel.default_ttl > 0 && (
            <span>ttl={channel.default_ttl}s</span>
          )}
          {channel.max_messages !== undefined && channel.max_messages > 0 && (
            <span>max={channel.max_messages}</span>
          )}
        </div>
      </div>

      <PublishForm
        channelName={channel.name}
        onPublished={() => setReloadKey((k) => k + 1)}
      />

      <BroadcastForm
        seedChannel={channel.name}
        onBroadcast={() => setReloadKey((k) => k + 1)}
      />

      <AwaitForm seedChannel={channel.name} />

      <div className="channel-detail-section">
        <h4>Recent messages (peek, scope=global)</h4>
        {peekLoading && <div className="loading-indicator">loading…</div>}
        {peekErr && (
          <div className="error-banner">
            Peek failed: {peekErr}
            <div className="error-banner-hint">
              Peek through this endpoint addresses scope=global. For
              scope=user / scope=agent channels, peek via the
              per-user / per-agent route.
            </div>
          </div>
        )}
        {!peekErr && messages.length === 0 && !peekLoading && (
          <div className="empty-state">No messages in scope=global.</div>
        )}
        {messages.length > 0 && (
          <ul className="channel-messages">
            {messages.map((m) => (
              <li key={m.id} className="channel-message">
                <div className="channel-message-meta mono">
                  {m.id} · {new Date(m.published_at).toLocaleString()}
                </div>
                <pre className="channel-message-value mono">
                  {JSON.stringify(m.value, null, 2)}
                </pre>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

// PublishForm posts a message to the selected channel via the admin
// publish route. payload is parsed client-side (must be valid, non-null
// JSON, matching the server's guard) so the operator sees a parse error
// before the round-trip; deliver_at is optional (RFC3339 → deferred).
function PublishForm({
  channelName,
  onPublished,
}: {
  channelName: string;
  onPublished: () => void;
}) {
  const [payloadJSON, setPayloadJSON] = useState('{\n  "hello": "world"\n}');
  const [deliverAt, setDeliverAt] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  // Reset the form when the operator switches channels.
  useEffect(() => {
    setErr(null);
    setOk(null);
  }, [channelName]);

  const handlePublish = async () => {
    setErr(null);
    setOk(null);
    const raw = payloadJSON.trim();
    if (!raw) {
      setErr("payload is required (non-empty JSON value).");
      return;
    }
    let payload: unknown;
    try {
      payload = JSON.parse(raw);
    } catch (e) {
      setErr(`payload is not valid JSON: ${e instanceof Error ? e.message : String(e)}`);
      return;
    }
    if (payload === null) {
      setErr("payload may not be null.");
      return;
    }
    const body: { payload: unknown; deliver_at?: string } = { payload };
    if (deliverAt.trim()) body.deliver_at = deliverAt.trim();
    setBusy(true);
    try {
      const r = await publishChannel(channelName, body);
      setOk(
        r.visible_at
          ? `Published ${r.msg_id} (deferred to ${r.visible_at}).`
          : `Published ${r.msg_id}.`,
      );
      onPublished();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="channel-detail-section">
      <h4>Publish a message</h4>
      <label className="modal-field">
        <span>payload (JSON)</span>
        <textarea
          value={payloadJSON}
          onChange={(e) => setPayloadJSON(e.target.value)}
          rows={5}
          className="mono"
          spellCheck={false}
          placeholder='{"key": "value"}'
        />
      </label>
      <label className="modal-field">
        <span>deliver_at (optional, RFC3339)</span>
        <input
          type="text"
          value={deliverAt}
          onChange={(e) => setDeliverAt(e.target.value)}
          placeholder="2026-01-01T00:00:00Z — omit for publish now"
        />
      </label>
      {err && <div className="error-banner">{err}</div>}
      {ok && <div className="flash-ok">{ok}</div>}
      <button
        type="button"
        className="primary"
        onClick={handlePublish}
        disabled={busy}
      >
        {busy ? "Publishing…" : "Publish"}
      </button>
    </div>
  );
}

// Server caps a fan-out / fan-in set at 32 channels (connector
// ChannelBroadcastRequest / ChannelAwaitRequest). Validate client-side so the
// operator sees the limit before the round-trip.
const MAX_FAN_CHANNELS = 32;

// parseChannelList splits a comma/newline-separated textarea into a deduped,
// trimmed, non-empty channel-name list (order preserved).
function parseChannelList(raw: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const tok of raw.split(/[\n,]+/)) {
    const name = tok.trim();
    if (name && !seen.has(name)) {
      seen.add(name);
      out.push(name);
    }
  }
  return out;
}

// BroadcastForm fans one payload OUT to a SET of channels (Channel.broadcast's
// wire twin). Seeded with the selected channel so the common "publish here +
// a couple more" case is one edit away. The op is atomic at the ACL pre-flight
// — one undeclared channel refuses the whole broadcast.
function BroadcastForm({
  seedChannel,
  onBroadcast,
}: {
  seedChannel: string;
  onBroadcast: () => void;
}) {
  const [channelsRaw, setChannelsRaw] = useState(seedChannel);
  const [scope, setScope] = useState<ChannelScope>("global");
  const [scopeID, setScopeID] = useState("");
  const [payloadJSON, setPayloadJSON] = useState('{\n  "hello": "world"\n}');
  const [deliverAt, setDeliverAt] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<{
    published: number;
    failed: number;
    rows: { channel: string; msg_id?: string; visible_at?: string; error?: string }[];
  } | null>(null);

  // Re-seed when the operator switches channels.
  useEffect(() => {
    setChannelsRaw(seedChannel);
    setErr(null);
    setResult(null);
  }, [seedChannel]);

  const handleBroadcast = async () => {
    setErr(null);
    setResult(null);
    const channels = parseChannelList(channelsRaw);
    if (channels.length === 0) {
      setErr("at least one channel is required.");
      return;
    }
    if (channels.length > MAX_FAN_CHANNELS) {
      setErr(`too many channels (${channels.length}); the server caps a broadcast at ${MAX_FAN_CHANNELS}.`);
      return;
    }
    if (scope !== "global" && !scopeID.trim()) {
      setErr(`scope_id is required for scope=${scope}.`);
      return;
    }
    const raw = payloadJSON.trim();
    if (!raw) {
      setErr("payload is required (non-empty JSON value).");
      return;
    }
    let payload: unknown;
    try {
      payload = JSON.parse(raw);
    } catch (e) {
      setErr(`payload is not valid JSON: ${e instanceof Error ? e.message : String(e)}`);
      return;
    }
    if (payload === null) {
      setErr("payload may not be null.");
      return;
    }
    setBusy(true);
    try {
      const r = await broadcastChannels({
        channels,
        scope,
        scope_id: scope === "global" ? undefined : scopeID.trim(),
        payload,
        deliver_at: deliverAt.trim() || undefined,
      });
      setResult({
        published: r.published,
        failed: r.failed,
        rows: (r.results ?? []).map((e) => ({
          channel: e.channel,
          msg_id: e.msg_id,
          visible_at: e.visible_at,
          error: e.error,
        })),
      });
      onBroadcast();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="channel-detail-section">
      <h4>Broadcast (fan-out)</h4>
      <label className="modal-field">
        <span>channels (comma / newline separated)</span>
        <textarea
          value={channelsRaw}
          onChange={(e) => setChannelsRaw(e.target.value)}
          rows={2}
          className="mono"
          spellCheck={false}
          placeholder="c1, c2, c3"
        />
      </label>
      <div className="channel-fan-row">
        <label className="modal-field">
          <span>scope</span>
          <select value={scope} onChange={(e) => setScope(e.target.value as ChannelScope)}>
            <option value="global">global</option>
            <option value="user">user</option>
            <option value="agent">agent</option>
          </select>
        </label>
        {scope !== "global" && (
          <label className="modal-field">
            <span>scope_id</span>
            <input
              type="text"
              value={scopeID}
              onChange={(e) => setScopeID(e.target.value)}
              placeholder={scope === "user" ? "user_id" : "agent_id"}
            />
          </label>
        )}
      </div>
      <label className="modal-field">
        <span>payload (JSON)</span>
        <textarea
          value={payloadJSON}
          onChange={(e) => setPayloadJSON(e.target.value)}
          rows={4}
          className="mono"
          spellCheck={false}
          placeholder='{"key": "value"}'
        />
      </label>
      <label className="modal-field">
        <span>deliver_at (optional, RFC3339)</span>
        <input
          type="text"
          value={deliverAt}
          onChange={(e) => setDeliverAt(e.target.value)}
          placeholder="2026-01-01T00:00:00Z — omit for broadcast now"
        />
      </label>
      {err && <div className="error-banner">{err}</div>}
      {result && (
        <div className={result.failed > 0 ? "flash-warn" : "flash-ok"}>
          Broadcast: {result.published} published, {result.failed} failed.
          <ul className="channel-fan-results">
            {result.rows.map((row) => (
              <li key={row.channel} className="mono">
                {row.error ? (
                  <span className="channel-fan-error">
                    {row.channel}: {row.error}
                  </span>
                ) : (
                  <span>
                    {row.channel}: {row.msg_id}
                    {row.visible_at ? ` (deferred → ${row.visible_at})` : ""}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
      <button type="button" className="primary" onClick={handleBroadcast} disabled={busy}>
        {busy ? "Broadcasting…" : "Broadcast"}
      </button>
    </div>
  );
}

// AwaitForm fans IN across a SET of channels (Channel.await's wire twin): a
// bounded long-poll that returns when the mode predicate is met or wait_ms
// elapses. Reads are NON-committing — this is a detection probe, not a
// subscribe. The whole call blocks (button busy up to wait_ms).
function AwaitForm({ seedChannel }: { seedChannel: string }) {
  const [channelsRaw, setChannelsRaw] = useState(seedChannel);
  const [scope, setScope] = useState<ChannelScope>("global");
  const [scopeID, setScopeID] = useState("");
  const [mode, setMode] = useState<ChannelAwaitMode>("any");
  const [n, setN] = useState("2");
  const [waitMS, setWaitMS] = useState("2000");
  const [maxMessages, setMaxMessages] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<{
    satisfied: boolean;
    timed_out: boolean;
    mode: string;
    fired: string[];
    total: number;
    results: Record<string, ChannelMessageItem[]>;
  } | null>(null);

  useEffect(() => {
    setChannelsRaw(seedChannel);
    setErr(null);
    setResult(null);
  }, [seedChannel]);

  const handleAwait = async () => {
    setErr(null);
    setResult(null);
    const channels = parseChannelList(channelsRaw);
    if (channels.length === 0) {
      setErr("at least one channel is required.");
      return;
    }
    if (channels.length > MAX_FAN_CHANNELS) {
      setErr(`too many channels (${channels.length}); the server caps an await at ${MAX_FAN_CHANNELS}.`);
      return;
    }
    if (scope !== "global" && !scopeID.trim()) {
      setErr(`scope_id is required for scope=${scope}.`);
      return;
    }
    let nVal: number | undefined;
    if (mode === "at_least") {
      nVal = parseInt(n.trim(), 10);
      if (!Number.isInteger(nVal) || nVal <= 0) {
        setErr("n must be a positive integer for mode=at_least.");
        return;
      }
    }
    // Bounded long-poll: clamp to 60s so an operator typo can't pin the tab.
    const wait = parseInt(waitMS.trim() || "2000", 10);
    if (!Number.isInteger(wait) || wait < 0 || wait > 60000) {
      setErr("wait_ms must be an integer between 0 and 60000.");
      return;
    }
    let maxMsg: number | undefined;
    if (maxMessages.trim()) {
      maxMsg = parseInt(maxMessages.trim(), 10);
      if (!Number.isInteger(maxMsg) || maxMsg < 0) {
        setErr("max_messages must be a non-negative integer.");
        return;
      }
    }
    setBusy(true);
    try {
      const r = await awaitChannels({
        channels,
        scope,
        scope_id: scope === "global" ? undefined : scopeID.trim(),
        mode,
        n: nVal,
        wait_ms: wait,
        max_messages: maxMsg,
      });
      const byChannel: Record<string, ChannelMessageItem[]> = {};
      for (const [name, entry] of Object.entries(r.results ?? {})) {
        byChannel[name] = entry.messages ?? [];
      }
      setResult({
        satisfied: r.satisfied,
        timed_out: r.timed_out,
        mode: r.mode,
        fired: r.fired ?? [],
        total: r.total_messages,
        results: byChannel,
      });
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="channel-detail-section">
      <h4>Await (fan-in)</h4>
      <label className="modal-field">
        <span>channels (comma / newline separated)</span>
        <textarea
          value={channelsRaw}
          onChange={(e) => setChannelsRaw(e.target.value)}
          rows={2}
          className="mono"
          spellCheck={false}
          placeholder="c1, c2, c3"
        />
      </label>
      <div className="channel-fan-row">
        <label className="modal-field">
          <span>scope</span>
          <select value={scope} onChange={(e) => setScope(e.target.value as ChannelScope)}>
            <option value="global">global</option>
            <option value="user">user</option>
            <option value="agent">agent</option>
          </select>
        </label>
        {scope !== "global" && (
          <label className="modal-field">
            <span>scope_id</span>
            <input
              type="text"
              value={scopeID}
              onChange={(e) => setScopeID(e.target.value)}
              placeholder={scope === "user" ? "user_id" : "agent_id"}
            />
          </label>
        )}
      </div>
      <div className="channel-fan-row">
        <label className="modal-field">
          <span>mode</span>
          <select value={mode} onChange={(e) => setMode(e.target.value as ChannelAwaitMode)}>
            <option value="any">any</option>
            <option value="all">all</option>
            <option value="at_least">at_least</option>
          </select>
        </label>
        {mode === "at_least" && (
          <label className="modal-field">
            <span>n</span>
            <input
              type="number"
              min="1"
              value={n}
              onChange={(e) => setN(e.target.value)}
            />
          </label>
        )}
        <label className="modal-field">
          <span>wait_ms (≤ 60000)</span>
          <input
            type="number"
            min="0"
            max="60000"
            value={waitMS}
            onChange={(e) => setWaitMS(e.target.value)}
          />
        </label>
        <label className="modal-field">
          <span>max_messages (optional)</span>
          <input
            type="number"
            min="0"
            value={maxMessages}
            onChange={(e) => setMaxMessages(e.target.value)}
            placeholder="per channel"
          />
        </label>
      </div>
      {err && <div className="error-banner">{err}</div>}
      {result && (
        <div className={result.satisfied ? "flash-ok" : "flash-warn"}>
          {result.satisfied
            ? `Satisfied (mode=${result.mode}).`
            : `Timed out (mode=${result.mode}).`}{" "}
          {result.total} message{result.total === 1 ? "" : "s"} across{" "}
          {result.fired.length} fired channel{result.fired.length === 1 ? "" : "s"}.
          {Object.entries(result.results).map(([name, msgs]) => (
            <div key={name} className="channel-fan-channel">
              <div className="channel-fan-channel-name mono">
                {name}
                {result.fired.includes(name) && (
                  <span className="channel-fan-fired">fired</span>
                )}{" "}
                · {msgs.length} msg{msgs.length === 1 ? "" : "s"}
              </div>
              {msgs.length > 0 && (
                <ul className="channel-messages">
                  {msgs.map((m) => (
                    <li key={m.id} className="channel-message">
                      <div className="channel-message-meta mono">
                        {m.id} · {new Date(m.published_at).toLocaleString()}
                      </div>
                      <pre className="channel-message-value mono">
                        {JSON.stringify(m.value, null, 2)}
                      </pre>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
      )}
      <button type="button" className="primary" onClick={handleAwait} disabled={busy}>
        {busy ? `Probing… (up to ${waitMS || "2000"}ms)` : "Await"}
      </button>
    </div>
  );
}
