import { useState } from "react";
import UniverseNavigator, { type UniverseNavigatorProps } from "./UniverseNavigator";
import type { UniverseSelection } from "../types";

// UniverseView is the standalone root: the navigator with its own selection
// state and theme, for a host that shows the map on its own page (loomboard's
// gated view, a full-screen route). Styles ship separately:
// `import "@loomcycle/universe/styles.css"`.
export interface UniverseViewProps extends Omit<UniverseNavigatorProps, "selection"> {
  theme?: "light" | "dark";
  /** Initial selection; afterwards the view owns it. */
  defaultSelection?: UniverseSelection;
}

export default function UniverseView({ theme, defaultSelection, onSelect, ...rest }: UniverseViewProps) {
  const [selection, setSelection] = useState<UniverseSelection | undefined>(defaultSelection);
  return (
    <div className="loomcycle-universe-root" {...(theme ? { "data-theme": theme } : {})}>
      <UniverseNavigator
        {...rest}
        selection={selection}
        onSelect={(sel) => {
          setSelection(sel);
          onSelect?.(sel);
        }}
      />
    </div>
  );
}
