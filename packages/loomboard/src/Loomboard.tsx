import {
  LoomboardRoot,
  useResolvedDataLayer,
  type LoomboardDataSource,
} from "./components/LoomboardRoot";
import LoomboardBody from "./components/LoomboardBody";
import type { BoardScope, SavedView } from "./types";

// Loomboard is the standalone, self-styling root for the view layer (RFC BT P1):
// saved table / cards / kanban / list views over loomcycle Documents. Mount it on
// its own page; it resolves its own data layer and wraps in the themeable
// `.loomcycle-loomboard` root. Styles ship separately:
// `import "@loomcycle/loomboard/styles.css"`.
export interface LoomboardProps extends LoomboardDataSource {
  /** Theming. Set → the root carries data-theme; omit → inherit an ancestor's
   *  data-theme (dark is the default palette). */
  theme?: "light" | "dark";
  /** Initial store scope. Default "user" (personal views); "tenant" is shared. */
  defaultScope?: BoardScope;
  /** Called on a load / save failure, in addition to the inline banner. NEVER
   *  redirects on 401 — the host owns the auth flow. */
  onError?: (e: unknown) => void;
  /** Open a star-map view (layout kind "universe", saved by
   *  @loomcycle/universe) in the host's star map. Without it such a view shows
   *  only a note saying where to open it. */
  onOpenUniverse?: (view: SavedView, scope: BoardScope) => void;
}

export default function Loomboard(props: LoomboardProps) {
  const { theme, defaultScope, onError, onOpenUniverse } = props;
  const resolved = useResolvedDataLayer(props);
  return (
    <LoomboardRoot theme={theme} dataLayer={resolved}>
      <LoomboardBody defaultScope={defaultScope} onError={onError} onOpenUniverse={onOpenUniverse} />
    </LoomboardRoot>
  );
}
