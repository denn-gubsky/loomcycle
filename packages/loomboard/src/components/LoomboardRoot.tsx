import { useMemo, type ReactNode } from "react";
import type { LoomcycleClient } from "@loomcycle/client";
import { createLoomcycleClient, type Connection } from "../lib/createClient";
import {
  LoomboardDataProvider,
  dataLayerFromClient,
  type LoomboardDataLayer,
} from "../lib/dataLayer";

// LoomboardDataSource is the data-source contract for the public <Loomboard>
// root. Provide exactly one; precedence is dataLayer > client > connection. The
// default path is `connection` → an internal LoomcycleClient.
export interface LoomboardDataSource {
  /** A raw connection (baseUrl + optional token + optional fetch override). */
  connection?: Connection;
  /** A prebuilt @loomcycle/client instance. */
  client?: LoomcycleClient;
  /** A fully custom data layer (e.g. a cookie-authed same-origin fetcher). */
  dataLayer?: LoomboardDataLayer;
}

// useResolvedDataLayer picks the data layer from the source props once per
// connection identity — keyed on the connection's PRIMITIVE fields so an inline
// `connection={{...}}` doesn't rebuild the client every render.
export function useResolvedDataLayer(src: LoomboardDataSource): LoomboardDataLayer | null {
  const { dataLayer, client, connection } = src;
  return useMemo<LoomboardDataLayer | null>(() => {
    if (dataLayer) return dataLayer;
    if (client) return dataLayerFromClient(client);
    if (connection) return dataLayerFromClient(createLoomcycleClient(connection));
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataLayer, client, connection?.baseUrl, connection?.token, connection?.fetch]);
}

// LoomboardRoot renders the themeable `.loomcycle-loomboard` wrapper + provides
// the data layer. With no data source it shows an inline banner (so a misconfig
// is visible, not silent). It NEVER redirects on 401 — the host owns auth.
export function LoomboardRoot({
  theme,
  dataLayer,
  children,
}: {
  theme?: "light" | "dark";
  dataLayer: LoomboardDataLayer | null;
  children: ReactNode;
}) {
  const themeAttr = theme ? { "data-theme": theme } : {};
  if (!dataLayer) {
    return (
      <div className="loomcycle-loomboard" {...themeAttr}>
        <div className="error-banner">
          @loomcycle/loomboard: provide a <code>connection</code>, <code>client</code>, or{" "}
          <code>dataLayer</code> prop.
        </div>
      </div>
    );
  }
  return (
    <div className="loomcycle-loomboard" {...themeAttr}>
      <LoomboardDataProvider value={dataLayer}>{children}</LoomboardDataProvider>
    </div>
  );
}
