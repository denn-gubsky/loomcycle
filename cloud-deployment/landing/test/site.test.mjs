// Static checks of the site the landing serves (cloud-web/index.html, which
// has no build step): every inline script compiles, the markup is balanced,
// and the account-flow panes and controls the script wires up exist. The
// behaviour itself is driven by hand against a real runtime (see
// cloud-web/README.md).
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const html = fs.readFileSync(path.resolve(HERE, "../../../cloud-web/index.html"), "utf8");

test("every inline script compiles", () => {
  const scripts = [...html.matchAll(/<script\b(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi)].map((m) => m[1]);
  assert.ok(scripts.length >= 1);
  for (const [i, s] of scripts.entries()) {
    assert.doesNotThrow(() => new vm.Script(s, { filename: `inline-script-${i}.js` }), `inline script ${i}`);
  }
});

// withoutCodeBlocks drops scripts, styles and comments so only markup is
// counted. It is not a sanitizer (the result is never rendered), but it is
// written like one: case-insensitive, tolerant of `</script >`, and repeated
// until nothing changes, so a removal can't leave a new block behind.
function withoutCodeBlocks(s) {
  let prev;
  do {
    prev = s;
    s = s
      .replace(/<script\b[\s\S]*?<\/script\b[^>]*>/gi, "")
      .replace(/<style\b[\s\S]*?<\/style\b[^>]*>/gi, "")
      .replace(/<!--[\s\S]*?-->/g, "");
  } while (s !== prev);
  return s;
}

test("div, table, form and button tags are balanced", () => {
  const body = withoutCodeBlocks(html);
  for (const tag of ["div", "table", "tbody", "form", "button", "p"]) {
    const open = (body.match(new RegExp(`<${tag}[\\s>]`, "g")) || []).length;
    const close = (body.match(new RegExp(`</${tag}>`, "g")) || []).length;
    assert.equal(open, close, `<${tag}> ${open} open vs ${close} closed`);
  }
});

test("the account-flow markup the script wires up exists", () => {
  for (const sel of [
    'data-pane="4"', "data-gate-only", "data-legacy-only", "data-signup", "data-open-app", "data-manage-team",
    "data-acct-status", "data-team-status", "data-back-workspace", "data-reset-self", 'id="team-rows"',
    'id="invite-rows"', "data-invite-form", 'id="invite-email"', "data-linkbox", 'id="link-value"',
    "data-copy-link", "data-open-link", "data-link-title", "data-link-note", "data-vault-kicker",
  ]) {
    assert.ok(html.includes(sel), `missing ${sel}`);
  }
  // results are announced, and the invite field is labelled
  assert.match(html, /role="status" aria-live="polite" data-acct-status/);
  assert.match(html, /role="status" aria-live="polite" data-team-status/);
  assert.match(html, /<label for="invite-email">/);
});

test("the read-only vault offers no mint and no paste when accounts are on", () => {
  // The mint and paste controls sit inside the one data-legacy-only block
  // that applyMode() hides when /api/whoami reports auth_gate.
  const start = html.indexOf('<div data-legacy-only>');
  assert.ok(start > 0);
  const block = html.slice(start, html.indexOf("<!-- pane 4"));
  assert.ok(block.includes("data-mint") && block.includes("data-add-mode") && block.includes("data-add>"));
});

test("the signup redirect replaces history rather than pushing the invite link", () => {
  assert.match(html, /location\.replace\(res\.json\.redirect\)/);
  assert.doesNotMatch(html, /location\.(href|assign)\s*[=(]\s*res\.json\.(redirect|url)/);
});
