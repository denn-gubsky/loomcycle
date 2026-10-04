import { describe, expect, it } from "vitest";
import { filterOptions } from "./comboOptions";
import { NO_USER, appliedUserId, filterUsers, userOptions } from "./userOptions";

const users = [
  { user_id: "tok-loomcycle-dev2", running_count: 0, total_count: 2302 },
  { user_id: "alice", running_count: 3, total_count: 40 },
  { user_id: "bob", running_count: 0, total_count: 1 },
];
const ids = (opts: { id: string }[]) => opts.map((o) => o.id);

describe("userOptions", () => {
  it("keeps the server's order and labels each user with its activity", () => {
    expect(userOptions(users)).toEqual([
      { id: "tok-loomcycle-dev2", hint: "2302 runs" },
      { id: "alice", hint: "3 running" },
      { id: "bob", hint: "1 run" },
    ]);
  });

  it("drops blank and duplicate ids", () => {
    const dup = [...users, { user_id: "alice", running_count: 0, total_count: 9 }, { user_id: "", running_count: 0, total_count: 0 }];
    expect(ids(userOptions(dup))).toEqual(["tok-loomcycle-dev2", "alice", "bob"]);
  });
});

describe("filterUsers", () => {
  const known = userOptions(users);

  it("lists every user under `no user` when nothing is typed", () => {
    expect(ids(filterUsers(known, ""))).toEqual(["", "tok-loomcycle-dev2", "alice", "bob"]);
  });

  it("narrows to the users whose id contains the typed text", () => {
    expect(ids(filterUsers(known, "LOOM"))).toEqual(["", "tok-loomcycle-dev2"]);
    expect(ids(filterUsers(known, "b"))).toEqual(["", "bob"]);
  });

  it("offers no user row for an id it does not know, leaving Enter to apply it", () => {
    expect(filterUsers(known, "never-ran-anything")).toEqual([NO_USER]);
  });
});

describe("appliedUserId", () => {
  it("applies an id the list does not know, trimmed", () => {
    expect(appliedUserId("  never-ran-anything ")).toBe("never-ran-anything");
  });

  it("clears the user for blank text", () => {
    expect(appliedUserId("   ")).toBe("");
  });
});

describe("filterOptions", () => {
  it("puts prefix matches ahead of substring matches and keeps extra fields", () => {
    const rows = [{ id: "meta-acme", n: 1 }, { id: "acme", n: 2 }];
    expect(filterOptions(rows, "acme")).toEqual([{ id: "acme", n: 2 }, { id: "meta-acme", n: 1 }]);
  });
});
