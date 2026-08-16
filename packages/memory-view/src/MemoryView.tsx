import { useState, type ReactNode } from "react";
import {
  MemoryViewRoot,
  useResolvedDataLayer,
  type MemoryDataSource,
} from "./components/MemoryViewRoot";
import MemoryConsole from "./components/MemoryConsole";
import FactViewer from "./components/FactViewer";
import SearchPanel from "./components/SearchPanel";

// MemoryView is the standalone, self-styling root for the Memory view. It hosts
// three tabs over one injected data layer:
//   - Entries : the k/v Vector Memory console (scope/scope_id/key browser + editor).
//   - Facts   : the entity-tier fact browser (bi-temporal, supersession chain).
//   - Search  : off-run unified semantic search across k/v + document chunks.
// Mount it on its own page; it resolves its own data layer + wraps in the
// themeable `.loomcycle-memory-view` root. Styles ship separately:
// `import "@loomcycle/memory-view/styles.css"`.
export type MemoryTab = "entries" | "facts" | "search";

export interface MemoryViewProps extends MemoryDataSource {
  /** Theming. Set → the root carries data-theme; omit → inherit an ancestor's
   *  data-theme (dark is the default palette). */
  theme?: "light" | "dark";
  /** Initial tab. Default "entries" — the landing view is unchanged from P4a. */
  defaultTab?: MemoryTab;
  /** Offer a "run consolidation" affordance on the facts tab, calling this with the
   *  scope_id in view.
   *
   *  A CALLBACK RATHER THAN A URL, because starting a pass is the host's business: in
   *  the loomcycle console it stages a run at /run, a route an embedder of this package
   *  does not have. Omitting it hides the affordance entirely rather than rendering a
   *  link to nowhere. */
  onRunConsolidation?: (scopeId: string) => void;
}

export default function MemoryView(props: MemoryViewProps) {
  const { theme, defaultTab, onRunConsolidation } = props;
  const resolved = useResolvedDataLayer(props);
  return (
    <MemoryViewRoot theme={theme} dataLayer={resolved}>
      <MemoryTabs defaultTab={defaultTab ?? "entries"} onRunConsolidation={onRunConsolidation} />
    </MemoryViewRoot>
  );
}

// MemoryTabs renders the tab bar + the active panel. It lives INSIDE
// <MemoryViewRoot> so every panel reads the same injected data layer via
// useMemoryData(). The Facts/Search panels default to scope "user"; the console
// owns its own scope selection internally, so nothing is threaded down (the
// panels are independent browse surfaces, not a shared selection).
function MemoryTabs({
  defaultTab,
  onRunConsolidation,
}: {
  defaultTab: MemoryTab;
  onRunConsolidation?: (scopeId: string) => void;
}) {
  const [tab, setTab] = useState<MemoryTab>(defaultTab);
  return (
    <>
      <div className="memory-view-tabs" role="tablist">
        <TabButton tab="entries" active={tab} onSelect={setTab}>
          Entries
        </TabButton>
        <TabButton tab="facts" active={tab} onSelect={setTab}>
          Facts
        </TabButton>
        <TabButton tab="search" active={tab} onSelect={setTab}>
          Search
        </TabButton>
      </div>
      <div className="memory-view-tabpanel">
        {tab === "entries" && <MemoryConsole />}
        {tab === "facts" && <FactViewer onRunConsolidation={onRunConsolidation} />}
        {tab === "search" && <SearchPanel />}
      </div>
    </>
  );
}

function TabButton({
  tab,
  active,
  onSelect,
  children,
}: {
  tab: MemoryTab;
  active: MemoryTab;
  onSelect: (t: MemoryTab) => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active === tab}
      className={active === tab ? "on" : ""}
      onClick={() => onSelect(tab)}
    >
      {children}
    </button>
  );
}
