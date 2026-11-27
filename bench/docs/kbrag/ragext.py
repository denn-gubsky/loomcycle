"""rag-ext: the advanced RAG pipeline outside loomcycle (PREREG "rag-ext, fixed here").

Standard library only. Chunks are heading sections (long ones cut into 400-token windows,
50 overlap) indexed under "<title> > <heading path>"; BM25 and dense legs, 50 candidates
each, fused by RRF (k 60); the question plus three reader rewrites, fused again; a rerank of
the top 50; parent expansion to whole sections.

    python3 -I bench/docs/kbrag/ragext.py stats
    python3 -I bench/docs/kbrag/ragext.py build [--pace 0.5]      # dense index, resumable
    python3 -I bench/docs/kbrag/ragext.py timing [--queries 20]   # offline per-query time

The dense index lives in config.RAGEXT: vectors.f32 (array('f'), unit-normalised, in
manifest order) and manifest.json (order, per-text sha256, progress and build cost).
"""
import argparse, hashlib, heapq, json, math, os, re, sys, time
from array import array

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import config, prompts  # noqa: E402

TOKEN = re.compile(r"\w+|[^\w\s]")
WINDOW, OVERLAP = 400, 50
K1, B = 1.2, 0.75
LEG, RRF_K = 50, 60
RERANK_POOL, RERANK_KEEP, RERANK_CHARS = 50, 20, 600
# A listwise reply naming fewer than this many valid passages is treated as failed.
RERANK_MIN_VALID = 10
BATCH = 32
MANIFEST, VECTORS = "manifest.json", "vectors.f32"

try:
    _dot = math.sumprod  # 3.12+: a C loop, ~10x a Python sum
except AttributeError:  # pragma: no cover
    import operator
    def _dot(a, b):
        return sum(map(operator.mul, a, b))


# --- chunking ------------------------------------------------------------------

def windows(text, size=WINDOW, overlap=OVERLAP):
    """Cut text into windows of `size` tokens overlapping by `overlap`, as slices of the
    original text (so the indexed text keeps its spacing and punctuation)."""
    spans = [m.span() for m in TOKEN.finditer(text)]
    if len(spans) <= size:
        return [text]
    out, step, start = [], size - overlap, 0
    while True:
        end = min(start + size, len(spans))
        out.append(text[spans[start][0]:spans[end - 1][1]])
        if end == len(spans):
            return out
        start += step


def header(doc, section):
    return " > ".join([doc["title"]] + list(section["path"]))


def build_chunks(docs):
    """[{id, doc, sec, text}] in a fixed order: page id, section index, window."""
    out = []
    for did in sorted(docs):
        d = docs[did]
        for i, s in enumerate(d["sections"]):
            if not s["text"].strip():
                continue
            h = header(d, s)
            for w, part in enumerate(windows(s["text"])):
                out.append({"id": "%s#%d#%d" % (did, i, w), "doc": did, "sec": i, "text": h + "\n" + part})
    return out


# --- BM25 ----------------------------------------------------------------------

def tokens(text):
    return [t.lower() for t in TOKEN.findall(text)]


class BM25:
    def __init__(self, texts):
        self.n = len(texts)
        self.post = {}
        self.dl = []
        for idx, t in enumerate(texts):
            toks = tokens(t)
            self.dl.append(len(toks))
            tf = {}
            for tok in toks:
                tf[tok] = tf.get(tok, 0) + 1
            for tok, c in tf.items():
                self.post.setdefault(tok, []).append((idx, c))
        self.avgdl = (sum(self.dl) / self.n) if self.n else 0.0
        # Lucene's idf: never negative, so a very common term cannot push a chunk down.
        self.idf = {t: math.log(1 + (self.n - len(p) + 0.5) / (len(p) + 0.5)) for t, p in self.post.items()}
        self.norm = [K1 * (1 - B + B * dl / self.avgdl) if self.avgdl else K1 for dl in self.dl]

    def search(self, query, n=LEG):
        acc = {}
        for tok in set(tokens(query)):
            p = self.post.get(tok)
            if not p:
                continue
            idf = self.idf[tok]
            for idx, tf in p:
                acc[idx] = acc.get(idx, 0.0) + idf * tf * (K1 + 1) / (tf + self.norm[idx])
        # Ties break on index so the ranking is deterministic.
        return [i for i, _ in heapq.nsmallest(n, acc.items(), key=lambda kv: (-kv[1], kv[0]))]


# --- fusion ----------------------------------------------------------------------

def rrf(lists, k=RRF_K, n=None):
    score = {}
    for lst in lists:
        for rank, item in enumerate(lst):
            score[item] = score.get(item, 0.0) + 1.0 / (k + rank + 1)
    first = {}
    for lst in lists:
        for rank, item in enumerate(lst):
            first.setdefault(item, rank)
    ranked = sorted(score, key=lambda x: (-score[x], first[x], str(x)))
    return ranked[:n] if n else ranked


def parents(chunks, order):
    """Chunk indexes -> their whole sections, first occurrence kept, in rank order."""
    out, seen = [], set()
    for idx in order:
        c = chunks[idx]
        key = (c["doc"], c["sec"])
        if key not in seen:
            seen.add(key)
            out.append(key)
    return out


# --- dense -------------------------------------------------------------------------

def unit(v):
    n = math.sqrt(_dot(v, v)) or 1.0
    return [x / n for x in v]


def sha(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


class Dense:
    """Exact cosine over unit vectors held as one array('f') per chunk."""

    def __init__(self, vectors, dim):
        self.vecs, self.dim = vectors, dim

    @classmethod
    def load(cls, directory, chunks):
        m = _jload(os.path.join(directory, MANIFEST))
        if m["done"] != len(chunks) or m["order"] != [c["id"] for c in chunks]:
            raise SystemExit("rag-ext dense index is incomplete or stale (%d/%d); run `ragext.py build`"
                             % (m["done"], len(chunks)))
        if m["sha256"] != [sha(c["text"]) for c in chunks]:
            raise SystemExit("rag-ext dense index was built from other texts; rebuild it")
        dim = m["dim"]
        flat = array("f")
        with open(os.path.join(directory, VECTORS), "rb") as f:
            flat.frombytes(f.read())
        return cls([flat[i * dim:(i + 1) * dim] for i in range(m["done"])], dim)

    def search(self, qvec, n=LEG):
        q = unit(qvec)
        sims = [_dot(q, v) for v in self.vecs]
        return heapq.nlargest(n, range(len(sims)), key=sims.__getitem__)


def build_dense(lc, chunks, directory, *, pace=0.5, max_fail=3, log=print, sleep=time.sleep):
    """Embed every chunk in batches of BATCH, sequentially, appending to vectors.f32 and
    committing progress to the manifest after each batch, so a killed build resumes."""
    os.makedirs(directory, exist_ok=True)
    mpath, vpath = os.path.join(directory, MANIFEST), os.path.join(directory, VECTORS)
    order, shas = [c["id"] for c in chunks], [sha(c["text"]) for c in chunks]
    m = _jload(mpath) if os.path.exists(mpath) else None
    if m and (m["order"][:m["done"]] != order[:m["done"]] or m["sha256"][:m["done"]] != shas[:m["done"]]):
        raise SystemExit("existing dense index was built from other chunks; move it aside first")
    if not m:
        m = {"embedder": "lc.embed (deployment embedder)", "dim": None, "normalised": True, "done": 0,
             "chars": 0, "calls": 0, "wall_s": 0.0, "errors": 0}
    m["order"], m["sha256"], m["total"] = order, shas, len(chunks)
    # Drop any bytes past the committed prefix (a batch written but not committed).
    if os.path.exists(vpath) and m["dim"]:
        with open(vpath, "r+b") as f:
            f.truncate(m["done"] * m["dim"] * 4)
    fails = 0
    while m["done"] < len(chunks):
        batch = chunks[m["done"]:m["done"] + BATCH]
        texts = [c["text"] for c in batch]
        t0 = time.time()
        try:
            vecs = lc.embed(texts)
            if len(vecs) != len(texts):
                raise RuntimeError("embedder returned %d vectors for %d texts" % (len(vecs), len(texts)))
        except Exception as e:  # lc.call already retried; a fault here is persistent
            fails += 1
            m["errors"] += 1
            log("embed batch at %d failed (%d in a row): %s" % (m["done"], fails, str(e)[:200]))
            if fails >= max_fail:
                _commit(m, mpath)
                return False
            sleep(30 * fails)
            continue
        fails = 0
        dim = m["dim"] or len(vecs[0])
        if any(len(v) != dim for v in vecs):
            raise SystemExit("embedder dimension changed mid-build")
        m["dim"] = dim
        flat = array("f")
        for v in vecs:
            flat.extend(unit(v))
        with open(vpath, "ab") as f:
            f.write(flat.tobytes())
            f.flush()
            os.fsync(f.fileno())
        m["done"] += len(batch)
        m["calls"] += 1
        m["chars"] += sum(len(t) for t in texts)
        m["wall_s"] = round(m["wall_s"] + time.time() - t0, 2)
        _commit(m, mpath)
        if m["calls"] % 20 == 0:
            log("embedded %d/%d chunks, %d calls, %.0f s" % (m["done"], len(chunks), m["calls"], m["wall_s"]))
        if pace:
            sleep(pace)
    return True


def _jload(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _commit(m, path):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(m, f)
    os.replace(tmp, path)


# --- the pipeline -------------------------------------------------------------------

class RagExt:
    def __init__(self, docs, dense=None, chunks=None):
        self.chunks = chunks if chunks is not None else build_chunks(docs)
        self.bm25 = BM25([c["text"] for c in self.chunks])
        self.dense = dense

    def leg_fuse(self, text, qvec):
        return rrf([self.bm25.search(text, LEG), self.dense.search(qvec, LEG)])

    def retrieve(self, lc, q, pair, usage, strategy=None):
        """One question through the whole pipeline. Returns the record a runs row keeps."""
        strategy = strategy or config.RAGEXT_RERANK
        rec = {"rerank_strategy": strategy, "expansion_failed": False, "rerank_failed": False, "rerank_short": False}
        reply, err, _ = prompts.ask(lc, prompts.REWRITE.format(question=prompts.question_text(q)), pair, usage,
                                    max_tokens=2048)
        rew = [s.strip() for s in (prompts.first_json_list(reply) or []) if isinstance(s, str) and s.strip()][:3]
        if len(rew) < 3:
            rec["expansion_failed"] = True
            rec["expansion_error"] = err or "reply held %d usable rewrites" % len(rew)
        queries = [q["query"]] + rew
        rec["queries"] = queries
        vecs = lc.embed(queries)
        rec["embed"] = {"calls": 1, "chars": sum(len(x) for x in queries)}
        fused = rrf([self.leg_fuse(t, v) for t, v in zip(queries, vecs)], n=RERANK_POOL)
        rec["fused"] = [self.chunks[i]["id"] for i in fused]
        order = fused
        if strategy == "listwise":
            order = self.rerank_listwise(lc, q, fused, pair, usage, rec)
        elif strategy != "none":
            raise SystemExit("unknown rerank strategy %r" % strategy)
        rec["reranked_ids"] = [self.chunks[i]["id"] for i in order[:RERANK_KEEP]]
        rec["sections"] = [list(s) for s in parents(self.chunks, order)[:10]]
        return rec

    def rerank_listwise(self, lc, q, fused, pair, usage, rec):
        passages = "\n\n".join("%d. %s" % (n, self.chunks[i]["text"][:RERANK_CHARS]) for n, i in enumerate(fused, 1))
        keep = min(RERANK_KEEP, len(fused))
        reply, err, _ = prompts.ask(lc, prompts.RERANK.format(question=prompts.question_text(q), n=len(fused),
                                                              keep=keep, passages=passages), pair, usage)
        picked = []
        for x in prompts.first_json_list(reply) or []:
            if isinstance(x, int) and not isinstance(x, bool) and 1 <= x <= len(fused) and fused[x - 1] not in picked:
                picked.append(fused[x - 1])
        if len(picked) < min(RERANK_MIN_VALID, len(fused)):
            rec["rerank_failed"] = True
            rec["rerank_error"] = err or "reply named %d valid passages" % len(picked)
            return fused
        rec["rerank_short"] = len(picked) < keep
        picked = picked[:keep]
        # The passages the model left out keep their fused order after the ranked ones, so
        # parent expansion can still fill 10 sections.
        return picked + [i for i in fused if i not in picked]


# --- CLI -----------------------------------------------------------------------------

def _docs():
    sys.path.insert(0, config.HERE)
    from common import read_jsonl
    return {d["id"]: d for d in read_jsonl(os.path.join(config.PREPARED, "docs.jsonl"))}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["stats", "build", "timing"])
    ap.add_argument("--pace", type=float, default=0.5, help="seconds between embed batches (shared GPU)")
    ap.add_argument("--queries", type=int, default=20)
    a = ap.parse_args()
    docs = _docs()
    chunks = build_chunks(docs)
    if a.cmd == "stats":
        nsec = sum(1 for d in docs.values() for s in d["sections"] if s["text"].strip())
        mpath = os.path.join(config.RAGEXT, MANIFEST)
        m = _jload(mpath) if os.path.exists(mpath) else {}
        print(json.dumps({"pages": len(docs), "readable_sections": nsec, "chunks": len(chunks),
                          "windowed_sections": len({(c["doc"], c["sec"]) for c in chunks if not c["id"].endswith("#0")}),
                          "chars": sum(len(c["text"]) for c in chunks),
                          "dense": {k: m.get(k) for k in ("done", "total", "dim", "calls", "chars", "wall_s", "errors")}},
                         indent=2))
    elif a.cmd == "build":
        import lc
        ok = build_dense(lc, chunks, config.RAGEXT, pace=a.pace, log=lambda s: print(s, flush=True))
        print("dense index", "complete" if ok else "stopped after repeated embed failures", flush=True)
        sys.exit(0 if ok else 1)
    else:
        timing(docs, chunks, a.queries)


def timing(docs, chunks, nq):
    """Offline per-question retrieval time with random unit vectors standing in for the
    embeddings: 4 queries x (BM25 + dense) + 2 RRF rounds + parent expansion."""
    import random
    rng = random.Random(1)
    dim = 1024
    t0 = time.time()
    vecs = [array("f", unit([rng.gauss(0, 1) for _ in range(dim)])) for _ in chunks]
    t_vec = time.time() - t0
    t0 = time.time()
    r = RagExt(docs, Dense(vecs, dim), chunks)
    t_bm25 = time.time() - t0
    sys.path.insert(0, config.HERE)
    from common import read_jsonl
    qs = read_jsonl(os.path.join(config.PREPARED, "questions.jsonl"))[:nq]
    t0 = time.time()
    for q in qs:
        qv = [[rng.gauss(0, 1) for _ in range(dim)] for _ in range(4)]
        texts = [q["query"], q["question"], q["scenario"], q["query"][::-1]]
        fused = rrf([r.leg_fuse(t, v) for t, v in zip(texts, qv)], n=RERANK_POOL)
        parents(r.chunks, fused)
    per = (time.time() - t0) / max(1, len(qs))
    print(json.dumps({"chunks": len(chunks), "bm25_build_s": round(t_bm25, 2), "random_vectors_s": round(t_vec, 2),
                      "per_question_s (4 queries)": round(per, 3), "per_query_s": round(per / 4, 3)}, indent=2))


if __name__ == "__main__":
    main()
