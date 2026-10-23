/**
 * The access-control types against the Go structs that produce them.
 *
 * WHY. The access-control rows have three producers per family — the stored
 * row (internal/store/acl.go), the config-declared entry the list routes add
 * beside it (internal/api/http/acl_mgmt.go) and, for decisions, the engine's
 * own types (internal/authz) — and the TS interfaces here are hand-written. A
 * field added to any of them, or a TS field none of them sends, would compile
 * and pass every mocked test. So each TS interface is compared both ways with
 * the json tags of the Go structs that marshal into it: every Go key must be
 * a TS field, and every TS field must be some Go struct's key.
 *
 * HOW. Text, like context-options.test.ts: the Go source is read, never a copy
 * of its field list. A standalone checkout of the adapter (no Go tree) has
 * nothing to compare against and the suite is skipped.
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = (rel: string) => fileURLToPath(new URL(`../../../${rel}`, import.meta.url));
const haveGo = existsSync(root("go.mod")) && existsSync(root("internal/store/acl.go"));

function read(rel: string): string {
  return readFileSync(root(rel), "utf8");
}

/** The json keys of `type <name> struct { … }` in one Go file. A key of "-"
 *  is not on the wire. Fails when the struct is gone, so a rename cannot
 *  turn a check vacuous. */
function goKeys(rel: string, name: string): string[] {
  const src = read(rel);
  const start = src.indexOf(`type ${name} struct {`);
  expect(start, `${rel}: type ${name} struct not found — renamed or moved?`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf("\n}", start));
  const keys = [...body.matchAll(/json:"([^",]*)/g)].map((m) => m[1]!).filter((k) => k !== "-");
  expect(keys.length, `${rel}: ${name} has no json tags`).toBeGreaterThan(0);
  return keys;
}

/** The top-level field names of `export interface <name> { … }` in types.ts. */
function tsFields(name: string): string[] {
  const src = readFileSync(fileURLToPath(new URL("../src/types.ts", import.meta.url)), "utf8");
  const start = src.indexOf(`export interface ${name} {`);
  expect(start, `types.ts: interface ${name} not found`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf("\n}", start));
  const fields = [...body.matchAll(/^ {2}([a-z_][a-z0-9_]*)\??:/gm)].map((m) => m[1]!);
  expect(fields.length, `types.ts: ${name} has no fields`).toBeGreaterThan(0);
  return fields;
}

/** The string constants of a Go enum type: `X <type> = "value"`. */
function goEnum(rel: string, type: string): string[] {
  const re = new RegExp(String.raw`\b\w+\s+${type}\s*=\s*"([^"]+)"`, "g");
  return [...read(rel).matchAll(re)].map((m) => m[1]!);
}

/** The members of `export type <name> = "a" | "b" …` in types.ts. */
function tsUnion(name: string): string[] {
  const src = readFileSync(fileURLToPath(new URL("../src/types.ts", import.meta.url)), "utf8");
  const start = src.indexOf(`export type ${name} =`);
  expect(start, `types.ts: type ${name} not found`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf(";", start));
  return [...body.matchAll(/"([^"]+)"/g)].map((m) => m[1]!);
}

const STORE = "internal/store/acl.go";
const HTTP = "internal/api/http/acl_mgmt.go";
const RULE = "internal/authz/rule.go";
const ENGINE = "internal/authz/engine.go";
const COND = "internal/authz/condition.go";

/** TS interface → the Go structs whose JSON it receives (or, for request
 *  bodies, the struct the server decodes it into). */
const PAIRS: Array<[string, Array<[string, string]>]> = [
  // Responses: a stored row and the config entry beside it.
  ["GroupRow", [[STORE, "GroupRow"], [HTTP, "aclConfigGroup"]]],
  ["GroupMember", [[STORE, "GroupMemberRow"]]],
  // aclConfigList is tenant_id + authz.ACL (embedded) + source.
  ["AclDefRow", [[STORE, "AclDefRow"], [HTTP, "aclConfigList"], [RULE, "ACL"]]],
  ["AclBinding", [[STORE, "AclBindingRow"], [HTTP, "aclConfigBinding"]]],
  ["AclList", [[RULE, "ACL"]]],
  ["AclRule", [[RULE, "Rule"]]],
  ["AclCondition", [[COND, "Condition"]]],
  ["AuthzReason", [[ENGINE, "Reason"]]],
  ["AuthzTrace", [[ENGINE, "Trace"]]],
  ["AuthzDecision", [[HTTP, "authzDecisionWire"]]],
  ["AuthzAppliedBinding", [[RULE, "Binding"]]],
  // Requests: what the server decodes.
  ["CreateGroupBody", [[HTTP, "groupRequest"]]],
  ["CreateAclBindingBody", [[HTTP, "bindingRequest"]]],
  ["AclDefInput", [[HTTP, "aclDefRequest"]]],
  ["AuthzRequestContext", [[HTTP, "authzContextRef"]]],
  ["AuthzEvaluationRequest", [[HTTP, "authzEvaluationRequest"]]],
  ["AuthzSimulateRequest", [[HTTP, "authzSimulateRequest"]]],
  ["AuthzDraftBinding", [[RULE, "Binding"]]],
];

describe.skipIf(!haveGo)("access-control types match the Go wire structs", () => {
  for (const [ts, goStructs] of PAIRS) {
    it(`${ts} ⇔ ${goStructs.map(([, n]) => n).join(" + ")}`, () => {
      const fields = new Set(tsFields(ts));
      const keys = new Set(goStructs.flatMap(([file, name]) => goKeys(file, name)));
      // AuthzDraftBinding: a draft has no id yet; the engine's Binding does.
      if (ts === "AuthzDraftBinding") keys.delete("id");
      const missingInTs = [...keys].filter((k) => !fields.has(k)).sort();
      const unknownToGo = [...fields].filter((f) => !keys.has(f)).sort();
      expect(missingInTs, `Go sends/reads keys ${ts} lacks`).toEqual([]);
      expect(unknownToGo, `${ts} fields no Go struct has`).toEqual([]);
    });
  }

  it("AclDefOp is the AclDef op table", () => {
    const src = read("internal/connector/aclmgmt.go");
    const m = /case ACLFamilyAclDef:[\s\S]*?return \[\]string\{([^}]*)\}/.exec(src);
    expect(m, "ACLFamilyAclDef case not found in ACLManagementOps").not.toBeNull();
    const ops = [...m![1]!.matchAll(/"([^"]+)"/g)].map((x) => x[1]!).sort();
    expect(tsUnion("AclDefOp").sort()).toEqual(ops);
  });

  it("the enums match their Go constants", () => {
    expect(tsUnion("AclSubjectKind").sort()).toEqual(goEnum(RULE, "SubjectKind").sort());
    expect(tsUnion("AclMode").sort()).toEqual(goEnum(ENGINE, "Mode").sort());
    expect(tsUnion("AuthzEvaluationsSemantic").sort()).toEqual(goEnum(ENGINE, "Semantic").sort());
    expect(tsUnion("AclEffect").sort()).toEqual(goEnum(RULE, "Effect").sort());
  });
});
