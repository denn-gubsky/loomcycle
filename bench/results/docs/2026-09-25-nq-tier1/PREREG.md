# NQ tier 1 — header and rerank at scale. Written BEFORE the run.

Committed before any number exists. Every design choice being tested was fixed on QASPER,
so this whole corpus is out-of-sample.

**Set:** Natural Questions validation shards 0 and 1 (Hugging Face
`google-research-datasets/natural_questions`), shuffled with seed 7. Every question is kept
that has a PARAGRAPH long answer, a short answer, and a gold paragraph found in the
converted page (`nq_prepare.py`). **911 questions, 911 pages, 11,313 section chunks**, all
in one pooled store (tenant `nq-probe`). Pages are Wikipedia HTML converted to Markdown
with the real heading tree; tables, infoboxes, navigation and reference sections dropped.

**Query:** the bare question. NQ is open-domain, so finding the page is part of the task,
and no page title is given.

**Arms:** `plain`, `header` (`Article: <title>. Section: <heading path>.` prepended to the
indexed body), `header+rr` (local listwise rerank of the top 20, the QASPER prompt minus the
title line). Reader: ornith-1.5:35b, the QASPER reader prompt with "a Wikipedia article",
top 5. Three reader passes (seeds 0, 11, 23). An oracle pass (gold paragraphs) gives the
ceiling.

**Co-primary hypotheses** (retrieval, deterministic; Bonferroni α = 0.025 each):

- **H1:** `header` beats `plain` on R@5 (evidence paragraph in the reader's top 5), exact
  McNemar p < 0.025.
- **H2:** `header+rr` beats `header` on R@5, exact McNemar p < 0.025.

**Secondary** (reported whatever they show):

- **H3:** `header` beats `plain` on exact match (three-pass mean per question), paired
  bootstrap 95% CI lower bound above 0.
- **H4:** `header+rr` beats `header` on exact match, CI lower bound above 0.
- F1, R@1/3/10, and the oracle ceiling are reported alongside.

**What the result decides:**

- **H1 and H2 hold:** RFC DM proposes both — index-only header text beside the chunk, and
  an opt-in rerank step.
- **Only H2 holds:** the RFC proposes the rerank; the header is recorded as not transferring
  to open-domain queries.
- **Only H1 holds:** the RFC proposes the header; the rerank goes back for a probe.
- **Neither holds:** no RFC. The findings are written up and the epic's next probe is
  reconsidered.
