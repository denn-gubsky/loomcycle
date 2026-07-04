import { createContext, useContext, type ReactNode } from "react";
import type { LoomcycleClient, SubstrateToolInput } from "@loomcycle/client";
import type {
  DefListByNameResponse,
  DefRow,
  LibraryListResponse,
  SubstrateKind,
} from "../types";

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
}

// dispatchFor picks the client's op-discriminated substrate method for a kind.
// Only the three Library-driven kinds are supported; anything else throws
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
