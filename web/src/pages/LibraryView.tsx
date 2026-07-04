import { Outlet, useLocation, useNavigate } from "react-router-dom";
import { Library, type LibraryTab } from "@loomcycle/library";
import { usePrincipal } from "../components/Layout";

// LibraryView is a thin wrapper around the standalone <Library> component from
// @loomcycle/library (RFC AY step 3). The full substrate-browser implementation
// (lineage, create/fork/promote/retire, Claude Code import) now lives in the
// package; web consumes its SOURCE via a Vite alias so this build compiles it.
//
// CSS: we deliberately do NOT `import "@loomcycle/library/styles.css"`. The
// package emits the SAME class names (.library-view, .def-*, .lineage-*,
// .splitter*, .library-modal*, .import-*, .modal*) that web's global styles.css
// already styles — many shared with IntegrationsView and other views — so the
// existing global sheet renders the component identically with zero token drift.
// The package ships its own scoped styles.css only for EXTERNAL hosts.

// cookieFetch mirrors api.ts's jsonFetch transport EXACTLY: the Web UI is
// same-origin and rides the `loomcycle_session` HttpOnly cookie (no bearer), and
// a 401 bounces to the login page. Injecting it into the package's
// @loomcycle/client keeps that behavior transparent. Defined at module scope so
// its identity is stable across renders (the package memoizes its client on the
// connection's `fetch` reference).
const cookieFetch: typeof fetch = async (input, init) => {
  const r = await fetch(input, { ...init, credentials: "same-origin" });
  if (r.status === 401) {
    window.location.assign("/ui/login");
    // Navigation underway — never-settling so the caller doesn't flash an error
    // mid-redirect (matches jsonFetch's redirect-then-hang contract).
    return new Promise<Response>(() => {});
  }
  return r;
};

export default function LibraryView() {
  const principal = usePrincipal();
  const navigate = useNavigate();
  const loc = useLocation();

  // Bridge the route → the package's tab type so deep-links keep working. The
  // route uses "mcp-servers"; the package uses "mcp".
  const pkgTab: LibraryTab = loc.pathname.startsWith("/library/skills")
    ? "skills"
    : loc.pathname.startsWith("/library/mcp-servers")
      ? "mcp"
      : "agents";

  return (
    <>
      <Library
        connection={{ baseUrl: "", fetch: cookieFetch }}
        principal={principal ?? undefined}
        serverCapabilities={principal?.capabilities}
        tenantScope={principal && !principal.is_admin ? "tenant" : "admin"}
        tab={pkgTab}
        onTabChange={(t) =>
          navigate("/library/" + (t === "mcp" ? "mcp-servers" : t))
        }
      />
      {/* Keep the /library index-route redirect (main.tsx: /library → index →
          Navigate to /library/agents). Renders nothing on the concrete tabs. */}
      <Outlet />
    </>
  );
}
