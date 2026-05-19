import { useSearchParams } from "react-router-dom";

// ViewToggle switches the agent detail pane between the rich
// EventCard panels view (the default, v0.8.19 behaviour) and the
// plain-text terminal-output view (v0.8.20 addition).
//
// State persists in the URL search param ?view= so reloads and
// deep links preserve the operator's choice. Default = "panels"
// (param absent or any value other than "terminal").

export type ViewMode = "panels" | "terminal";

export function useViewMode(): [ViewMode, (m: ViewMode) => void] {
  const [searchParams, setSearchParams] = useSearchParams();
  const mode: ViewMode = searchParams.get("view") === "terminal" ? "terminal" : "panels";
  const set = (m: ViewMode) => {
    const next = new URLSearchParams(searchParams);
    if (m === "panels") {
      next.delete("view");
    } else {
      next.set("view", "terminal");
    }
    // Replace history so toggling doesn't accumulate back-button entries.
    setSearchParams(next, { replace: true });
  };
  return [mode, set];
}

export interface ViewToggleProps {
  mode: ViewMode;
  onChange: (m: ViewMode) => void;
}

export default function ViewToggle({ mode, onChange }: ViewToggleProps) {
  return (
    <div className="view-toggle scope-tabs" role="tablist" aria-label="agent detail view mode">
      <button
        type="button"
        role="tab"
        aria-selected={mode === "panels"}
        className={mode === "panels" ? "on" : ""}
        onClick={() => onChange("panels")}
      >
        panels
      </button>
      <button
        type="button"
        role="tab"
        aria-selected={mode === "terminal"}
        className={mode === "terminal" ? "on" : ""}
        onClick={() => onChange("terminal")}
      >
        terminal
      </button>
    </div>
  );
}
