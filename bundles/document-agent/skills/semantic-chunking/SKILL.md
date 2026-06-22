---
name: semantic-chunking
description: Split arbitrary prose or a Markdown file into a sensible chunk hierarchy in a chunked-graph Document.
allowed_tools: [Document]
license: Apache-2.0
---

# Semantic chunking

When the operator hands you a body of text (pasted prose, a spec, a Markdown
file) and asks to "import it" or "chunk it", turn it into a hierarchy of
chunks — do NOT dump it into one giant chunk.

## Method
1. Read the text's own structure first: existing headings, numbered sections,
   topic shifts, and natural paragraphs are your chunk boundaries.
2. Choose a hierarchy: a top-level chunk per major section, child chunks per
   sub-section. Aim for chunks that are individually meaningful and editable —
   a few sentences to a few paragraphs each, not one-liners and not whole pages.
3. Give every chunk a short, descriptive **title** (it becomes the heading).
4. Create them top-down so parents exist before children:
   `Document op=create_chunk document_id=<id> parent_id=<parent> title="…" body="…"`
   (omit `parent_id` for a child of the root, or pass the document's root chunk).
5. Carry a `type` when the operator's domain implies one (e.g. `section`,
   `requirement`, `task`) and a `status` if they use a workflow.

## Notes
- If the text is a loomcycle export (it has `<!-- loom: … -->` comments), prefer
  the deterministic `import_md` op instead (see the md-import skill).
- Keep the operator's wording in the body; don't paraphrase unless asked.
