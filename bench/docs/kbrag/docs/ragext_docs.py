"""rag-ext for Study D: ../ragext.py's pipeline over the development tenant's corpus.

    python3 -I bench/docs/kbrag/docs/ragext_docs.py stats
    python3 -I bench/docs/kbrag/docs/ragext_docs.py build [--pace 0.2]    # dense index, resumable

The chunking, BM25, dense index, RRF and parent expansion are ../ragext.py's, imported
unchanged. What differs is only the prompts (dprompts: this corpus, not UK guidance), the
question (a plain string, no scenario) and the number of sections kept (k = 20, not 10).
The rerank is the listwise reader-model rerank of Amendment 0 §3.
"""
import argparse, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import dconfig, dprompts  # noqa: E402
import ragext  # noqa: E402  (../ragext.py: imported read-only)


def load_docs():
    with open(dconfig.SECTIONS, encoding="utf-8") as f:
        return json.load(f)["docs"]


class RagExtDocs(ragext.RagExt):
    def retrieve(self, lc, question, pair, usage, keep_sections=dconfig.K):
        rec = {"rerank_strategy": "listwise", "expansion_failed": False, "rerank_failed": False, "rerank_short": False}
        reply, err, _ = dprompts.ask(lc, dprompts.REWRITE.format(question=question), pair, usage, max_tokens=2048)
        rew = [s.strip() for s in (dprompts.first_json_list(reply) or []) if isinstance(s, str) and s.strip()][:3]
        if len(rew) < 3:
            rec["expansion_failed"] = True
            rec["expansion_error"] = err or "reply held %d usable rewrites" % len(rew)
        queries = [question] + rew
        rec["queries"] = queries
        vecs = lc.embed(queries)
        rec["embed"] = {"calls": 1, "chars": sum(len(x) for x in queries)}
        fused = ragext.rrf([self.leg_fuse(t, v) for t, v in zip(queries, vecs)], n=ragext.RERANK_POOL)
        rec["fused"] = [self.chunks[i]["id"] for i in fused]
        order = self.rerank(lc, question, fused, pair, usage, rec)
        rec["reranked_ids"] = [self.chunks[i]["id"] for i in order[:ragext.RERANK_KEEP]]
        # Parent expansion over the whole reordered pool, so 20 distinct sections remain
        # even when several windows of one section rank high.
        rec["sections"] = [list(s) for s in ragext.parents(self.chunks, order)[:keep_sections]]
        return rec

    def rerank(self, lc, question, fused, pair, usage, rec):
        """../ragext.py's rerank_listwise with this corpus's prompt (same rules)."""
        passages = "\n\n".join("%d. %s" % (n, self.chunks[i]["text"][:ragext.RERANK_CHARS]) for n, i in enumerate(fused, 1))
        keep = min(ragext.RERANK_KEEP, len(fused))
        reply, err, _ = dprompts.ask(lc, dprompts.RERANK.format(question=question, n=len(fused), keep=keep,
                                                                passages=passages), pair, usage)
        picked = []
        for x in dprompts.first_json_list(reply) or []:
            if isinstance(x, int) and not isinstance(x, bool) and 1 <= x <= len(fused) and fused[x - 1] not in picked:
                picked.append(fused[x - 1])
        if len(picked) < min(ragext.RERANK_MIN_VALID, len(fused)):
            rec["rerank_failed"] = True
            rec["rerank_error"] = err or "reply named %d valid passages" % len(picked)
            return fused
        rec["rerank_short"] = len(picked) < keep
        picked = picked[:keep]
        return picked + [i for i in fused if i not in picked]


def build(docs=None):
    docs = docs or load_docs()
    chunks = ragext.build_chunks(docs)
    return RagExtDocs(docs, ragext.Dense.load(dconfig.RAGEXT, chunks), chunks)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["stats", "build"])
    ap.add_argument("--pace", type=float, default=0.2, help="seconds between embed batches (shared GPU)")
    a = ap.parse_args()
    docs = load_docs()
    chunks = ragext.build_chunks(docs)
    if a.cmd == "stats":
        mpath = os.path.join(dconfig.RAGEXT, ragext.MANIFEST)
        m = json.load(open(mpath)) if os.path.exists(mpath) else {}
        print(json.dumps({"documents": len(docs), "documents_indexed": len({c["doc"] for c in chunks}),
                          "chunks": len(chunks), "chars": sum(len(c["text"]) for c in chunks),
                          "dense": {k: m.get(k) for k in ("done", "total", "dim", "calls", "chars", "wall_s", "errors")}},
                         indent=1))
        return
    import dlc
    dlc.assert_tenant()
    ok = ragext.build_dense(dlc, chunks, dconfig.RAGEXT, pace=a.pace, log=lambda s: print(s, flush=True))
    print("dense index", "complete" if ok else "stopped after repeated embed failures", flush=True)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
