import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import UserAccessCell, { type AccessSnapshot } from "./UserAccessCell";

const zero = "0001-01-01T00:00:00Z";
const snap: AccessSnapshot = {
  idx: {
    support: [{ group: "support", member_kind: "group", member_id: "temps", added_at: "" }],
    temps: [{ group: "temps", member_kind: "user", member_id: "carol", added_at: "" }],
  },
  bindings: [
    { binding_id: "1", acl: "mine", subject_kind: "user", subject_id: "carol", created_at: "", expires_at: zero, retired_at: zero },
    { binding_id: "2", acl: "support-rules", subject_kind: "group", subject_id: "support", created_at: "", expires_at: zero, retired_at: zero },
    { binding_id: "3", acl: "baseline", subject_kind: "tenant", created_at: "", expires_at: zero, retired_at: zero },
    { binding_id: "4", acl: "someone-else", subject_kind: "user", subject_id: "dave", created_at: "", expires_at: zero, retired_at: zero },
  ],
};

describe("UserAccessCell", () => {
  it("shows the user's groups and labels each binding by why it applies", () => {
    const html = renderToStaticMarkup(createElement(UserAccessCell, { subject: "carol", snap }));
    expect(html).toContain(">temps<");
    expect(html).toContain("support<span class=\"settings-muted\"> via temps</span>");
    expect(html).toContain("<code>mine</code> <span class=\"settings-muted\">own</span>");
    expect(html).toContain("<code>support-rules</code> <span class=\"settings-muted\">via group support</span>");
    expect(html).toContain("<code>baseline</code> <span class=\"settings-muted\">tenant-wide</span>");
    expect(html).not.toContain("someone-else");
  });

  it("says so when the user has no group and nothing bound, and waits without data", () => {
    const empty = renderToStaticMarkup(createElement(UserAccessCell, { subject: "zed", snap: { idx: {}, bindings: [] } }));
    expect(empty).toContain("no groups");
    expect(empty).toContain("no lists bound");
    expect(renderToStaticMarkup(createElement(UserAccessCell, { subject: "zed", snap: null }))).toContain("—");
  });
});
