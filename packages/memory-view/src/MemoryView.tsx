import {
  MemoryViewRoot,
  useResolvedDataLayer,
  type MemoryDataSource,
} from "./components/MemoryViewRoot";
import MemoryConsole from "./components/MemoryConsole";

// MemoryView is the standalone, self-styling root for the Memory k/v console —
// the scope/scope_id/key browser + entry editor + Vector Memory reembed flow.
// Mount it on its own page; it resolves its own data layer + wraps in the
// themeable `.loomcycle-memory-view` root. Styles ship separately:
// `import "@loomcycle/memory-view/styles.css"`.
export interface MemoryViewProps extends MemoryDataSource {
  /** Theming. Set → the root carries data-theme; omit → inherit an ancestor's
   *  data-theme (dark is the default palette). */
  theme?: "light" | "dark";
}

export default function MemoryView(props: MemoryViewProps) {
  const { theme } = props;
  const resolved = useResolvedDataLayer(props);
  return (
    <MemoryViewRoot theme={theme} dataLayer={resolved}>
      <MemoryConsole />
    </MemoryViewRoot>
  );
}
