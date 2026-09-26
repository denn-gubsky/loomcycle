import { createContext, useContext, type ReactNode } from "react";
import type { LoomcycleClient, SubstrateToolInput } from "@loomcycle/client";
import type {
  DefListByNameResponse,
  DefRow,
  LibraryEntry,
  LibraryListResponse,
  SubstrateKind,
} from "../types";
import type { Connection } from "./createClient";

// LibraryDataLayer is the narrow data contract the Library components need — the
// nine reads/writes the substrate browser makes. Decoupling behind this
// interface lets a host inject the default client-backed implementation
// (dataLayerFromClient), or a custom one (e.g. a cookie-authed same-origin
// fetcher) without the components importing any global api module.
export interface LibraryDataLayer {
  listAgents(): Promise<LibraryListResponse>;
  listSkills(): Promise<LibraryListResponse>;
  listMcpServers(): Promise<LibraryListResponse>;
  listDefVersionsByName(
    kind: SubstrateKind,
    name: string,
  ): Promise<DefListByNameResponse>;
  createDef(
    kind: SubstrateKind,
    name: string,
    overlay: Record<string, unknown>,
    promote: boolean,
  ): Promise<DefRow>;
  forkDef(
    kind: SubstrateKind,
    name: string,
    overlay: Record<string, unknown>,
    promote: boolean,
    parentDefID?: string,
  ): Promise<DefRow>;
  promoteDef(kind: SubstrateKind, defID: string): Promise<unknown>;
  retireDef(kind: SubstrateKind, defID: string): Promise<unknown>;
  rediscoverMcpServerDef(name: string): Promise<DefRow>;
  /** HookDefs, one entry per name. Optional: the client has no list call for
   *  them, so only a data layer that can reach the runtime directly (the
   *  connection path) offers it — and the Hooks tab shows only when it does. */
  listHooks?(): Promise<LibraryListResponse>;
}

// dispatchFor picks the client's op-discriminated substrate method for a kind.
// Only the Library-driven kinds are supported; anything else throws
// loudly rather than silently no-op'ing.
function dispatchFor(
  client: LoomcycleClient,
  kind: SubstrateKind,
): (input: SubstrateToolInput) => Promise<unknown> {
  switch (kind) {
    case "agentdef":
      return (input) => client.agentDef(input);
    case "skilldef":
      return (input) => client.skillDef(input);
    case "mcpserverdef":
      return (input) => client.mcpServerDef(input);
    case "hookdef":
      return (input) => client.hookDef(input);
    default:
      throw new Error(
        `@loomcycle/library: substrate kind "${kind}" is not supported by the Library data layer`,
      );
  }
}

// dataLayerFromClient maps a @loomcycle/client instance onto the LibraryDataLayer.
//
// Field-name reconciliation with the wire contract (must match what the server
// accepts): the overlay is sent NESTED under `overlay` (not spread), def_id as
// `def_id`, parent as `parent_def_id`, retire as `{retired:true}` — identical to
// the Web UI's substrateDispatch. The client's list/agentDef/etc. return
// unknown (op-varying), so we cast to the kept types; listLibrary* return the
// client's own LibraryListResponse<T> which is structurally the same endpoint
// JSON as ours (ours additionally types the agents-only live_version_count /
// active_retired fields the server sends) — cast through unknown.
export function dataLayerFromClient(client: LoomcycleClient): LibraryDataLayer {
  return {
    listAgents: () =>
      client.listLibraryAgents() as unknown as Promise<LibraryListResponse>,
    listSkills: () =>
      client.listLibrarySkills() as unknown as Promise<LibraryListResponse>,
    listMcpServers: () =>
      client.listLibraryMcpServers() as unknown as Promise<LibraryListResponse>,
    listDefVersionsByName: (kind, name) =>
      dispatchFor(client, kind)({
        op: "list",
        name,
      }) as Promise<DefListByNameResponse>,
    createDef: (kind, name, overlay, promote) =>
      dispatchFor(client, kind)({
        op: "create",
        name,
        overlay,
        promote,
      }) as Promise<DefRow>,
    forkDef: (kind, name, overlay, promote, parentDefID) => {
      const input: SubstrateToolInput = { op: "fork", name, overlay, promote };
      if (parentDefID) input.parent_def_id = parentDefID;
      return dispatchFor(client, kind)(input) as Promise<DefRow>;
    },
    promoteDef: (kind, defID) =>
      dispatchFor(client, kind)({ op: "promote", def_id: defID }),
    retireDef: (kind, defID) =>
      dispatchFor(client, kind)({ op: "retire", def_id: defID, retired: true }),
    rediscoverMcpServerDef: (name) =>
      client.mcpServerDef({ op: "rediscover", name }) as Promise<DefRow>,
  };
}

// HookDefNameSummary is one row of GET /v1/_hookdef/names.
interface HookDefNameSummary {
  name: string;
  version_count: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  live_version_count?: number;
  active_retired?: boolean;
}

// hookEntriesFromNames adapts the names roll-up to the LibraryEntry rows the
// lineage panel lists. HookDefs have no static (yaml) side. An admin sees every
// tenant's names, so one name can arrive once per tenant: the list shows it
// once, with the versions of all of them counted (opening it lists every one).
export function hookEntriesFromNames(names: HookDefNameSummary[] | null | undefined): LibraryEntry[] {
  const byName = new Map<string, LibraryEntry>();
  for (const n of names ?? []) {
    const prev = byName.get(n.name);
    if (prev) {
      prev.version_count += n.version_count;
      prev.live_version_count = (prev.live_version_count ?? 0) + (n.live_version_count ?? 0);
      continue;
    }
    byName.set(n.name, {
      name: n.name,
      source: "dynamic-only",
      in_static: false,
      in_substrate: true,
      version_count: n.version_count,
      active_def_id: n.active_def_id,
      latest_version: n.latest_version,
      last_updated: n.last_updated,
      live_version_count: n.live_version_count,
      active_retired: n.active_retired,
    });
  }
  return [...byName.values()];
}

// dataLayerFromConnection is dataLayerFromClient plus what the client cannot
// do: listing HookDefs, read straight from the runtime with the connection's
// own base URL, token and fetch.
export function dataLayerFromConnection(conn: Connection, client: LoomcycleClient): LibraryDataLayer {
  return {
    ...dataLayerFromClient(client),
    listHooks: async () => {
      const headers: Record<string, string> = { Accept: "application/json" };
      if (conn.token) headers.Authorization = `Bearer ${conn.token}`;
      const doFetch = conn.fetch ?? ((i: RequestInfo | URL, init?: RequestInit) => fetch(i, init));
      const r = await doFetch(`${conn.baseUrl}/v1/_hookdef/names`, { headers });
      if (!r.ok) throw new Error(`GET /v1/_hookdef/names: HTTP ${r.status}`);
      const body = (await r.json()) as { names?: HookDefNameSummary[] | null };
      return { entries: hookEntriesFromNames(body.names) };
    },
  };
}

// The data layer reaches the components through context — no module-global
// singleton. <Library> builds it once (useMemo over connection identity) and
// provides it; nested panels/modals read it via useLibraryData().
const LibraryDataContext = createContext<LibraryDataLayer | null>(null);

export function LibraryDataProvider({
  value,
  children,
}: {
  value: LibraryDataLayer;
  children: ReactNode;
}) {
  return (
    <LibraryDataContext.Provider value={value}>
      {children}
    </LibraryDataContext.Provider>
  );
}

export function useLibraryData(): LibraryDataLayer {
  const v = useContext(LibraryDataContext);
  if (!v) {
    throw new Error(
      "useLibraryData must be used within <Library> (no LibraryDataLayer in context)",
    );
  }
  return v;
}
