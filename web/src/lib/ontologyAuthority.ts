import type { Principal } from "../api";

/** Whether p decides the tenant ontology: confirms or reverts it, adopts a
 *  standard type or relation kind into it, and accepts or rejects a
 *  suggestion. An admin, the legacy token, open mode or a tenant operator; a
 *  member reads the ontology and suggests through the Document tool, and the
 *  server refuses it these routes (403). Unknown (null) offers nothing. */
export function canDecideOntology(p: Principal | null | undefined): boolean {
  if (!p) return false;
  return p.is_admin || p.legacy || !!p.open_mode || p.scopes.includes("substrate:tenant");
}
