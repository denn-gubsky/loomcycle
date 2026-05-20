// ActivityToolbar — top-of-page controls for the Activity Monitor.
// Owns no state of its own; the page passes current values + setters.
// Layout is one wrapped flex row so the toolbar collapses cleanly
// on narrow viewports.

export type ActivityMode = "live" | "1h" | "24h" | "7d";

export interface ActivityToolbarProps {
  mode: ActivityMode;
  onModeChange: (m: ActivityMode) => void;
  autoRefresh: boolean;
  onAutoRefreshChange: (v: boolean) => void;
  // Whether the system overlay toggle is available (server is
  // sending non-null system_* fields). The toggle is rendered
  // disabled when this is false.
  systemAvailable: boolean;
  showSystem: boolean;
  onShowSystemChange: (v: boolean) => void;
  showAdvanced: boolean;
  onShowAdvancedChange: (v: boolean) => void;
  // Optional status text shown to the right (e.g. "loading…",
  // "paused", "last update 3 s ago"). Free-form.
  statusText?: string;
}

const MODES: ActivityMode[] = ["live", "1h", "24h", "7d"];

export default function ActivityToolbar({
  mode,
  onModeChange,
  autoRefresh,
  onAutoRefreshChange,
  systemAvailable,
  showSystem,
  onShowSystemChange,
  showAdvanced,
  onShowAdvancedChange,
  statusText,
}: ActivityToolbarProps) {
  return (
    <div className="activity-toolbar">
      <div className="activity-toolbar-group">
        <span className="activity-toolbar-label">window</span>
        <div className="mode-tabs">
          {MODES.map((m) => (
            <button
              key={m}
              type="button"
              className={`mode-tab ${m === mode ? "on" : ""}`}
              onClick={() => onModeChange(m)}
            >
              {m}
            </button>
          ))}
        </div>
      </div>

      <button
        type="button"
        className={`toggle ${autoRefresh ? "on" : ""}`}
        onClick={() => onAutoRefreshChange(!autoRefresh)}
        title={autoRefresh ? "auto-refresh on — click to pause" : "auto-refresh paused — click to resume"}
      >
        {autoRefresh ? "⏸ pause" : "▶ resume"}
      </button>

      <button
        type="button"
        className={`toggle ${showSystem ? "on" : ""} ${!systemAvailable ? "disabled" : ""}`}
        onClick={() => {
          if (!systemAvailable) return;
          onShowSystemChange(!showSystem);
        }}
        disabled={!systemAvailable}
        title={
          systemAvailable
            ? "show host CPU + memory overlay"
            : "set LOOMCYCLE_METRICS_COLLECT_SYSTEM=1 on the server to enable"
        }
      >
        system overlay
      </button>

      <button
        type="button"
        className={`toggle ${showAdvanced ? "on" : ""}`}
        onClick={() => onShowAdvancedChange(!showAdvanced)}
        title="show diagnostic charts (goroutines, heap, system memory)"
      >
        advanced
      </button>

      {statusText && <span className="activity-toolbar-status">{statusText}</span>}
    </div>
  );
}
