import { useMemo, type ReactNode } from "react";
import type { LoomcycleClient } from "@loomcycle/client";
import { createLoomcycleClient, type Connection } from "../lib/createClient";
import {
  ExplorerDataProvider,
  dataLayerFromClient,
  type ExplorerDataLayer,
} from "../lib/dataLayer";

// ExplorerDataSource is the data-source contract shared by both public roots
// (<PathExplorer> / <DocumentViewer>). Provide exactly one; precedence is
// dataLayer > client > connection. The default path is `connection` → an
// internal LoomcycleClient.
export interface ExplorerDataSource {
  /** A raw connection (baseUrl + optional token + optional fetch override). */
  connection?: Connection;
  /** A prebuilt @loomcycle/client instance. */
  client?: LoomcycleClient;
  /** A fully custom data layer (e.g. a cookie-authed same-origin fetcher). */
  dataLayer?: ExplorerDataLayer;
}

// useResolvedDataLayer picks the data layer from the source props once per
// connection identity. Depending on the connection's PRIMITIVE fields (not the
// object) keeps an inline `connection={{...}}` from rebuilding the client every
// render.
export function useResolvedDataLayer(src: ExplorerDataSource): ExplorerDataLayer | null {
  const { dataLayer, client, connection } = src;
  return useMemo<ExplorerDataLayer | null>(() => {
    if (dataLayer) return dataLayer;
    if (client) return dataLayerFromClient(client);
    if (connection) return dataLayerFromClient(createLoomcycleClient(connection));
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataLayer, client, connection?.baseUrl, connection?.token, connection?.fetch]);
}

// ExplorerRoot renders the themeable `.loomcycle-explorer` wrapper + provides the
// data layer to nested components. When no data source resolved, it shows an
// inline error banner instead (so a misconfiguration is visible, not silent).
// The component NEVER redirects on 401 — the host owns the auth flow.
export function ExplorerRoot({
  theme,
  dataLayer,
  children,
}: {
  theme?: "light" | "dark";
  dataLayer: ExplorerDataLayer | null;
  children: ReactNode;
}) {
  const themeAttr = theme ? { "data-theme": theme } : {};
  if (!dataLayer) {
    return (
      <div className="loomcycle-explorer" {...themeAttr}>
        <div className="error-banner">
          @loomcycle/explorer: provide a <code>connection</code>, <code>client</code>, or{" "}
          <code>dataLayer</code> prop.
        </div>
      </div>
    );
  }
  return (
    <div className="loomcycle-explorer" {...themeAttr}>
      <ExplorerDataProvider value={dataLayer}>{children}</ExplorerDataProvider>
    </div>
  );
}
