import { UniverseNavigator } from "@loomcycle/universe";
import "@loomcycle/universe/styles.css";
import type { NavigatorContext } from "@loomcycle/explorer";

// UniverseNavigatorPane is the explorer's `stars` navigator: the 3D star map of
// the scope (RFC FQ). It is its own module so PathTreeView can lazy-load it —
// three.js and the map stay out of the main bundle until a reader flips the
// explorer from `tree` to `stars`. The transport mirrors PathTreeView's
// cookieFetch (same-origin cookie, 401 → login), passed in by the caller.
export default function UniverseNavigatorPane({
  ctx,
  fetchFn,
}: {
  ctx: NavigatorContext;
  fetchFn: typeof fetch;
}) {
  return (
    <UniverseNavigator
      connection={{ baseUrl: "", fetch: fetchFn }}
      scope={ctx.scope}
      browse={ctx.browse}
      selection={ctx.selection}
      onSelect={ctx.onSelect}
      reloadToken={ctx.reloadToken}
      maximized={ctx.maximized}
      onToggleMaximize={ctx.onToggleMaximize}
      subject={ctx.subject}
    />
  );
}
