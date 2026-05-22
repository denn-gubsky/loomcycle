import { useEffect, useMemo, useState } from "react";
import {
  ChannelDescriptor,
  ChannelMessageItem,
  listChannels,
  peekChannel,
} from "../api";
import Splitter from "../components/Splitter";

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
  }, []);

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
      </div>
      {err && <div className="error-banner">Failed to load channels: {err}</div>}
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
        />
        {selected ? (
          <ChannelDetail channel={selected} />
        ) : (
          <div className="empty-state">Select a channel to inspect.</div>
        )}
      </Splitter>
    </div>
  );
}

type FilterKind = "all" | "system" | "global" | "user" | "agent";

function FilterChips({
  current,
  onChange,
}: {
  current: FilterKind;
  onChange: (f: FilterKind) => void;
}) {
  const opts: { kind: FilterKind; label: string }[] = [
    { kind: "all", label: "all" },
    { kind: "system", label: "_system/*" },
    { kind: "global", label: "global" },
    { kind: "user", label: "user" },
    { kind: "agent", label: "agent" },
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

function filterChannels(channels: ChannelDescriptor[], f: FilterKind): ChannelDescriptor[] {
  if (f === "all") return channels;
  if (f === "system") return channels.filter((c) => c.name.startsWith("_system/"));
  return channels.filter((c) => c.scope === f);
}

function ChannelsList({
  channels,
  selectedName,
  onSelect,
}: {
  channels: ChannelDescriptor[];
  selectedName: string;
  onSelect: (name: string) => void;
}) {
  if (channels.length === 0) {
    return (
      <div className="empty-state">No channels match the current filter.</div>
    );
  }
  return (
    <ul className="channels-list">
      {channels.map((c) => (
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
              {c.scope && <span className="channel-scope">{c.scope}</span>}
              {c.semantic && <span className="channel-semantic">{c.semantic}</span>}
              <span className="channel-count">
                {c.message_count} msg{c.message_count === 1 ? "" : "s"}
              </span>
            </span>
          </button>
        </li>
      ))}
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
  }, [channel.name]);

  return (
    <div className="channel-detail">
      <div className="channel-detail-header">
        <h3>{channel.name}</h3>
        <div className="channel-detail-meta">
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
