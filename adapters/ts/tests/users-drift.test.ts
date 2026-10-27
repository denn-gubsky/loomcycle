/**
 * The user and sign-in gate types against the Go structs that produce them.
 *
 * WHY. These interfaces are hand-written, and the mocked client tests only
 * echo what the test itself put in the mock: a field the server added — or a
 * status it started emitting, as `pending` was — compiles and passes every
 * one of them. So each interface is compared both ways with the json tags of
 * the Go struct that marshals into it (or that the server decodes it into),
 * and the status union with the server's own check.
 *
 * HOW. Text, like access-control-drift.test.ts: the Go source is read, never a
 * copy of its field list. A standalone checkout of the adapter (no Go tree)
 * has nothing to compare against and the suite is skipped.
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = (rel: string) => fileURLToPath(new URL(`../../../${rel}`, import.meta.url));
const haveGo = existsSync(root("go.mod")) && existsSync(root("internal/api/http/users_crud.go"));

function read(rel: string): string {
  return readFileSync(root(rel), "utf8");
}

const TYPES = fileURLToPath(new URL("../src/types.ts", import.meta.url));

/** The json keys of `type <name> struct { … }` in one Go file. A key of "-"
 *  is not on the wire. Fails when the struct is gone, so a rename cannot turn
 *  a check vacuous. */
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
  const src = readFileSync(TYPES, "utf8");
  const start = src.indexOf(`export interface ${name} {`);
  expect(start, `types.ts: interface ${name} not found`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf("\n}", start));
  const fields = [...body.matchAll(/^ {2}([a-z_][a-z0-9_]*)\??:/gm)].map((m) => m[1]!);
  expect(fields.length, `types.ts: ${name} has no fields`).toBeGreaterThan(0);
  return fields;
}

/** The members of `export type <name> = "a" | "b" …` in types.ts. */
function tsUnion(name: string): string[] {
  const src = readFileSync(TYPES, "utf8");
  const start = src.indexOf(`export type ${name} =`);
  expect(start, `types.ts: type ${name} not found`).toBeGreaterThan(-1);
  const body = src.slice(start, src.indexOf(";", start));
  return [...body.matchAll(/"([^"]+)"/g)].map((m) => m[1]!);
}

/** The body of `func <name>(` in one Go file, up to its closing brace. */
function goFunc(rel: string, name: string): string {
  const src = read(rel);
  const start = src.indexOf(`func ${name}(`);
  expect(start, `${rel}: func ${name} not found — renamed or moved?`).toBeGreaterThan(-1);
  return src.slice(start, src.indexOf("\n}", start));
}

const USERS = "internal/api/http/users.go";
const CRUD = "internal/api/http/users_crud.go";
const STORE = "internal/store/store.go";
const LINK = "internal/api/http/authgate_link.go";

/** TS interface → the Go struct whose JSON it receives (or, for a request
 *  body, the struct the server decodes it into), and the Go keys the TS
 *  interface deliberately leaves out. */
const PAIRS: Array<[string, [string, string], string[]?]> = [
  ["UserSummary", [USERS, "wireUserSummary"]],
  ["UserRecord", [CRUD, "wireUserRecord"]],
  ["CreateUserBody", [CRUD, "createUserRequest"]],
  ["UpdateUserBody", [CRUD, "updateUserRequest"]],
  ["UserIdentity", [LINK, "wireUserIdentity"]],
];

describe.skipIf(!haveGo)("user and sign-in gate types match the Go wire structs", () => {
  for (const [ts, [file, goName], omitted = []] of PAIRS) {
    it(`${ts} ⇔ ${goName}`, () => {
      const fields = new Set(tsFields(ts));
      const keys = new Set(goKeys(file, goName));
      for (const k of omitted) keys.delete(k);
      const missingInTs = [...keys].filter((k) => !fields.has(k)).sort();
      const unknownToGo = [...fields].filter((f) => !keys.has(f)).sort();
      expect(missingInTs, `Go sends/reads keys ${ts} lacks`).toEqual([]);
      expect(unknownToGo, `${ts} fields no Go struct has`).toEqual([]);
    });
  }

  it("UserStatus is every status the store accepts", () => {
    // ValidUserStatus names its statuses as literals and as constants
    // (UserStatusPending); resolve both, so a status added either way shows.
    const body = goFunc(STORE, "ValidUserStatus");
    const literals = [...body.matchAll(/"([^"]+)"/g)].map((m) => m[1]!);
    const consts = [...body.matchAll(/\b(UserStatus\w+)\b/g)].map((m) => {
      const def = new RegExp(String.raw`const ${m[1]} = "([^"]+)"`).exec(read(STORE));
      expect(def, `${STORE}: const ${m[1]} not found`).not.toBeNull();
      return def![1]!;
    });
    expect(tsUnion("UserStatus").sort()).toEqual([...new Set([...literals, ...consts])].sort());
  });
});
