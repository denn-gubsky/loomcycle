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

// withoutCodeBlocks returns the markup outside scripts, styles and comments,
// so only real tags are counted. It scans and copies rather than deleting
// matches, so it is not a sanitizer and is never used as one (the result is
// only counted, never rendered).
const BLOCKS = [["<script", "</script", ">"], ["<style", "</style", ">"], ["<!--", "-->", ""]];
function withoutCodeBlocks(s) {
  const lower = s.toLowerCase();
  let out = "";
  let i = 0;
  while (i < s.length) {
    let start = -1;
    let block = null;
    for (const b of BLOCKS) {
      const at = lower.indexOf(b[0], i);
      if (at >= 0 && (start < 0 || at < start)) [start, block] = [at, b];
    }
    if (start < 0) return out + s.slice(i);
    out += s.slice(i, start);
    const close = lower.indexOf(block[1], start + block[0].length);
    if (close < 0) return out; // an unterminated block runs to the end
    const after = block[2] ? lower.indexOf(block[2], close + block[1].length) : close + block[1].length - 1;
    if (after < 0) return out;
    i = after + 1;
  }
  return out;
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

test("shared mode has its own block and the vault (with its loomboard handoff) can be hidden whole", () => {
  for (const sel of ["data-shared-only", "data-request-access", "data-open-app-shared", "data-shared-status",
    "data-shared-tenant", 'href="https://board.loomcycle.cloud"']) {
    assert.ok(html.includes(sel), `missing ${sel}`);
  }
  assert.match(html, /role="status" aria-live="polite" data-shared-status/);
  // applyMode() hides [data-vault-section] in shared mode; the token rows (where
  // the "loomboard →" handoff buttons are rendered) and mint/paste live inside it.
  const start = html.indexOf("<div data-vault-section>");
  assert.ok(start > 0);
  const block = html.slice(start, html.indexOf("<!-- pane 4"));
  assert.ok(block.includes('id="token-rows"') && block.includes("data-mint") && block.includes("data-add-mode"));
  // the shared block offers no team console
  const shared = html.slice(html.indexOf("<div class=\"acct\" data-shared-only"), start);
  assert.doesNotMatch(shared, /data-manage-team/);
});

test("the signup redirect replaces history rather than pushing the invite link", () => {
  assert.match(html, /location\.replace\(res\.json\.redirect\)/);
  assert.doesNotMatch(html, /location\.(href|assign)\s*[=(]\s*res\.json\.(redirect|url)/);
});

// ─── the privacy notice and the terms (cloud-web/privacy.html, terms.html) ────
const WEB = path.resolve(HERE, "../../../cloud-web");
const privacy = fs.readFileSync(path.join(WEB, "privacy.html"), "utf8");
const terms = fs.readFileSync(path.join(WEB, "terms.html"), "utf8");
// The plain pages: each with its title and the section ids it must keep.
const PAGES = [
  { file: "privacy.html", page: privacy, title: "Privacy",
    ids: ["controller", "shared", "keep", "retention", "where", "erasure"] },
  { file: "terms.html", page: terms, title: "Terms",
    ids: ["preview", "accounts", "use", "shared", "content", "warranty", "changes", "general", "contact"] },
];

// localRefs lists the href/src values of a page that point inside the site.
function localRefs(page) {
  return [...page.matchAll(/\b(?:href|src)="([^"]*)"/g)].map((m) => m[1])
    .filter((u) => !/^(?:[a-z]+:|\/\/|#)/i.test(u));
}

test("the footer has no dead links and points at the terms and the privacy notice", () => {
  const footer = html.slice(html.indexOf("<footer>"), html.indexOf("</footer>"));
  assert.ok(footer.length > 0);
  assert.doesNotMatch(footer, /href="#"/);
  assert.match(footer, /<a href="terms\.html">Terms<\/a>\s*<a href="privacy\.html">Privacy<\/a>/);
});

test("the shared sign-up says what members see and links the privacy notice", () => {
  const shared = html.slice(html.indexOf("<div class=\"acct\" data-shared-only"), html.indexOf("<div data-vault-section>"));
  assert.match(shared, /Approved members can see what other members share in the shared workspace/);
  assert.match(shared, /Your conversations are visible to you and to the workspace&rsquo;s operators; other members cannot open them\./);
  assert.doesNotMatch(shared, /open other members/);
  assert.match(shared, /<a href="privacy\.html">Privacy<\/a>/);
  assert.match(shared, /<a href="terms\.html">Terms<\/a>/);
});

// Only the plain pages' references are checked: index.html also names the
// hero and mascot clips, which are git-ignored (assets/*.mp4) and copied to the
// host separately.
for (const { file, page, title, ids } of PAGES) {
  test(`every local link and asset of ${file} exists`, () => {
    const refs = localRefs(page);
    assert.ok(refs.includes("index.html") && refs.includes("./ds-base.js"));
    for (const ref of refs) {
      assert.ok(fs.existsSync(path.join(WEB, ref.split(/[?#]/)[0])), `${file}: ${ref} does not exist`);
    }
  });

  test(`${file} is plain markup: balanced, no inline script, the site's stylesheet loader`, () => {
    // Closing tags match with optional whitespace (`</script >`), as browsers do.
    const scripts = [...page.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)];
    assert.equal((page.match(/<script\b/gi) || []).length, scripts.length, "every <script> is closed");
    assert.deepEqual(scripts.map((m) => [m[1].trim(), m[2].trim()]), [['src="./ds-base.js"', ""]]);
    assert.doesNotMatch(page, /\son[a-z]+=/i, "no inline event handlers");
    const body = withoutCodeBlocks(page);
    for (const tag of ["div", "section", "ul", "li", "p", "nav", "footer"]) {
      const open = (body.match(new RegExp(`<${tag}[\\s>]`, "g")) || []).length;
      const close = (body.match(new RegExp(`</${tag}>`, "g")) || []).length;
      assert.equal(open, close, `<${tag}> ${open} open vs ${close} closed`);
    }
    assert.ok(page.includes(`<title>${title} · loomcycle.cloud</title>`), "title");
    for (const id of ids) assert.ok(page.includes(`id="${id}"`), `missing #${id}`);
  });
}

test("the privacy notice and the terms link each other", () => {
  assert.match(privacy, /<a href="terms\.html">terms of use<\/a>/);
  assert.match(privacy, /<a href="terms\.html">Terms<\/a>/);
  assert.match(terms, /<a href="privacy\.html">Privacy<\/a>/);
});

// A generic preview-service terms page: it names the contact and the shared
// workspace's rule, and invents no jurisdiction or price.
test("the terms mark a preview service, give the contact and the conversation rule", () => {
  assert.match(terms, /loomcycle\.cloud is a preview service/);
  assert.match(terms, /<a href="mailto:privacy@loomcycle\.cloud">privacy@loomcycle\.cloud<\/a>/);
  assert.match(terms, /Last updated \d{1,2} [A-Z][a-z]+ \d{4}\./);
  assert.match(terms, /as is and as available/);
  assert.match(terms, /Your conversations are visible to you and to the workspace's operators; other members cannot open them\./);
  assert.doesNotMatch(terms, /governed by the laws of|jurisdiction|courts of|\$\d|€\d|£\d/i);
});

// The statements the owner answered for: who the controller is, where to
// write, the date the retention settings were read on, and who can open a
// member's conversations (members read only their own runs).
test("the privacy notice names the controller, the contact, its date and the conversation rule", () => {
  assert.match(privacy, /The loomcycle Authors are the controller of the personal data processed on loomcycle\.cloud/);
  assert.match(privacy, /<a href="mailto:privacy@loomcycle\.cloud">privacy@loomcycle\.cloud<\/a>/);
  assert.match(privacy, /Last updated \d{1,2} [A-Z][a-z]+ \d{4}\./);
  assert.match(privacy, /Your conversations are visible to you and to the workspace's operators; other members cannot open them\./);
  assert.doesNotMatch(privacy, /so can other approved members/);
  assert.match(privacy, /Cloudflare Web Analytics/);
});
