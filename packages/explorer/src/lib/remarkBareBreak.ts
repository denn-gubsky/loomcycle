// remarkBareBreak turns a bare <br> in a chunk body into a line break. An
// extraction writes one between a table cell's paragraphs, the only break a
// GFM cell can hold. react-markdown renders no raw HTML (no rehype-raw), so
// it would otherwise drop the tag and run the paragraphs together. Only the
// bare tag is read: any other HTML, a <br> with attributes included, stays
// as react-markdown leaves it. The runtime's renderer reads the same tag the
// same way (internal/files/render isBareBreak).

type MdNode = { type: string; value?: string; children?: MdNode[] };

const BARE_BREAK = /^<br\s*\/?>$/i;

function walk(node: MdNode): void {
  const kids = node.children;
  if (!kids) return;
  for (let i = 0; i < kids.length; i++) {
    const k = kids[i];
    if (k.type === "html" && BARE_BREAK.test(k.value ?? "")) {
      kids[i] = { type: "break" };
    } else {
      walk(k);
    }
  }
}

export default function remarkBareBreak() {
  return (tree: MdNode) => walk(tree);
}
