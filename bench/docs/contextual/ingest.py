#!/usr/bin/env python3
"""Import the probe corpus once per arm, into its own scopes.

Arms differ ONLY in what is prepended to each section body before import — the heading
split, the embedder and the retriever are the same for all of them:

  plain    the section text as written
  header   "Paper: <title>. Section: <heading path>." — deterministic, no model. The control
           that says whether an LLM context beats simply naming where the chunk came from.
  context  the ornith-generated situating context (contextualize.py)
  both     header + context

Prepending to the BODY is how this probe reaches both indexes with no code change: the
body is what is embedded AND what the full-text leg matches, so the context lands in
both — Anthropic's contextual embeddings + contextual BM25.

Two layouts per arm:
  <arm>-pool         all papers in one scope — "which document is this about" is part of
                     the problem, which is where a situating context is supposed to help
  <arm>-<paper_id>   one scope per paper — isolates finding the right section
"""
import argparse
import json
import os
import urllib.request

ARMS = ("plain", "header", "context", "both")


def post(base, tok, path, body, scope_id):
    r = urllib.request.Request(base + path + "?scope_id=" + scope_id, data=json.dumps(body).encode(),
                               method="POST", headers={"Authorization": "Bearer " + tok,
                                                       "Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=600) as x:
        return json.load(x)


def prefix(arm, paper, section, ctx):
    header = "Paper: %s. Section: %s." % (paper["title"], " > ".join(section["path"]))
    if arm == "plain":
        return ""
    if arm == "header":
        return header
    if arm == "context":
        return ctx
    return header + " " + ctx


def arm_markdown(arm, paper, contexts):
    """Rewrite each section body in place. Section texts appear verbatim and in order in
    the rendered Markdown (prepare.py guarantees it), so a forward cursor finds each one
    after its heading and a repeated sentence elsewhere cannot be hit instead."""
    md, cur, out = paper["markdown"], 0, []
    for s in paper["sections"]:
        i = md.index(s["text"], cur)
        p = prefix(arm, paper, s, contexts.get((paper["id"], tuple(s["path"])), ""))
        out.append(md[cur:i])
        out.append((p + "\n\n" if p else "") + s["text"])
        cur = i + len(s["text"])
    out.append(md[cur:])
    return "".join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--base", default="http://127.0.0.1:8801")
    ap.add_argument("--arms", default=",".join(ARMS))
    ap.add_argument("--layouts", default="pool,paper")
    a = ap.parse_args()
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    papers = [json.loads(l) for l in open(os.path.join(a.probe, "papers.jsonl"))]
    contexts = {}
    cpath = os.path.join(a.probe, "contexts.jsonl")
    if os.path.exists(cpath):
        for l in open(cpath):
            r = json.loads(l)
            contexts[(r["paper_id"], tuple(r["path"]))] = r["context"]
    manifest_path = os.path.join(a.probe, "manifest.jsonl")
    done = set()
    if os.path.exists(manifest_path):
        done = {(m["arm"], m["scope_id"], m["paper_id"]) for m in map(json.loads, open(manifest_path))}
    with open(manifest_path, "a") as mf:
        for arm in a.arms.split(","):
            if arm in ("context", "both"):
                missing = sum(1 for p in papers for s in p["sections"] if (p["id"], tuple(s["path"])) not in contexts)
                if missing:
                    raise SystemExit("arm %s: %d sections have no context yet — run contextualize.py first" % (arm, missing))
            for layout in a.layouts.split(","):
                for p in papers:
                    sid = "%s-pool" % arm if layout == "pool" else "%s-%s" % (arm, p["id"])
                    if (arm, sid, p["id"]) in done:
                        continue
                    res = post(a.base, tok, "/v1/_document",
                               {"op": "import_md", "scope": "user", "markdown": arm_markdown(arm, p, contexts)}, sid)
                    if "document_id" not in res:
                        raise SystemExit("import failed %s %s: %s" % (sid, p["id"], json.dumps(res)[:300]))
                    mf.write(json.dumps({"arm": arm, "layout": layout, "scope_id": sid, "paper_id": p["id"],
                                         "document_id": res["document_id"], "chunks": res.get("chunks_created")}) + "\n")
                    mf.flush()
                print("imported", arm, layout, flush=True)


if __name__ == "__main__":
    main()
