import { useMemo, type ReactNode } from "react";
import type { LoomcycleClient } from "@loomcycle/client";
import { createLoomcycleClient, type Connection } from "../lib/createClient";
import {
  MemoryDataProvider,
  dataLayerFromClient,
  dataLayerFromConnection,
  type MemoryDataLayer,
} from "../lib/dataLayer";

// MemoryDataSource is the data-source contract for the public <MemoryView> root.
// Provide exactly one; precedence is dataLayer > client > connection. The default
// path is `connection` → an internal LoomcycleClient.
export interface MemoryDataSource {
  /** A raw connection (baseUrl + optional token + optional fetch override). */
  connection?: Connection;
  /** A prebuilt @loomcycle/client instance. */
  client?: LoomcycleClient;
  /** A fully custom data layer (e.g. a cookie-authed same-origin fetcher). */
  dataLayer?: MemoryDataLayer;
}

// useResolvedDataLayer picks the data layer from the source props once per
// connection identity. Depending on the connection's PRIMITIVE fields (not the
// object) keeps an inline `connection={{...}}` from rebuilding the client every
// render.
export function useResolvedDataLayer(src: MemoryDataSource): MemoryDataLayer | null {
  const { dataLayer, client, connection } = src;
  return useMemo<MemoryDataLayer | null>(() => {
    if (dataLayer) return dataLayer;
    if (client) return dataLayerFromClient(client);
    // The connection path gets the change-feed tail too — it is the only path
    // holding the base URL + fetch that SSE needs (see dataLayerFromConnection).
    if (connection) return dataLayerFromConnection(connection, createLoomcycleClient(connection));
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataLayer, client, connection?.baseUrl, connection?.token, connection?.fetch]);
}

// MemoryViewRoot renders the themeable `.loomcycle-memory-view` wrapper + provides
// the data layer to the console. When no data source resolved, it shows an inline
// error banner instead (so a misconfiguration is visible, not silent). The
// component NEVER redirects on 401 — the host owns the auth flow.
export function MemoryViewRoot({
  theme,
  dataLayer,
  children,
}: {
  theme?: "light" | "dark";
  dataLayer: MemoryDataLayer | null;
  children: ReactNode;
}) {
  const themeAttr = theme ? { "data-theme": theme } : {};
  if (!dataLayer) {
    return (
      <div className="loomcycle-memory-view" {...themeAttr}>
        <div className="error-banner">
          @loomcycle/memory-view: provide a <code>connection</code>, <code>client</code>, or{" "}
          <code>dataLayer</code> prop.
        </div>
      </div>
    );
  }
  return (
    <div className="loomcycle-memory-view" {...themeAttr}>
      <MemoryDataProvider value={dataLayer}>{children}</MemoryDataProvider>
    </div>
  );
}
