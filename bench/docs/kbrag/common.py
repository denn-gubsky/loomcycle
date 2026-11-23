"""Shared pieces of the Study C harness: data, citations, scoring, retrieval metrics, usage."""
import collections, json, os, re, string, threading

import config

# Ids are not only 32-hex (a tenant may mint other shapes); a link ends at `?`, `)` or space.
CITE = re.compile(r"loom:(doc|chunk)/([A-Za-z0-9_-]+)")
# kb/ask's accounting lines: the ROUTE trailer, a survey's COVERAGE line and its UNCOVERED
# reason. They are bookkeeping, not answer, so neither the condenser nor the judge sees them.
TRAILER = re.compile(r"^\s*(ROUTE|COVERAGE|UNCOVERED):", re.I)


# --- io ----------------------------------------------------------------------

def jload(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def jdump(obj, path, **kw):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(obj, f, **kw)


def read_jsonl(path):
    if not os.path.exists(path):
        return []
    out = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    out.append(json.loads(line))
                except ValueError:  # a torn last line from a killed run is dropped, not fatal
                    pass
    return out


_APPEND_LOCK = threading.Lock()


def append_jsonl(path, row):
    """One whole line per call. The lock keeps lines from worker threads from interleaving
    (each is a single write of the full line, then fsync)."""
    line = json.dumps(row, ensure_ascii=False) + "\n"
    with _APPEND_LOCK:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "a", encoding="utf-8") as f:
            f.write(line)
            f.flush()
            os.fsync(f.fileno())


def run_jobs(jobs, fn, workers=1):
    """Run fn(job) for every job on `workers` threads, STARTING them in list order (each
    worker takes the next job off the list), so the submission order is the list's order;
    the completion order is not deterministic when workers > 1.

    The first exception any fn raises (a guard's Stop, or anything unexpected) stops new
    jobs from starting; jobs already in flight finish (their rows are written whole), every
    thread is joined, and that exception is re-raised. With workers=1 this is exactly a
    sequential loop that stops at the first exception."""
    jobs = list(jobs)
    lock = threading.Lock()
    state = {"next": 0, "err": None}

    def worker():
        while True:
            with lock:
                if state["err"] is not None or state["next"] >= len(jobs):
                    return
                job = jobs[state["next"]]
                state["next"] += 1
            try:
                fn(job)
            except BaseException as e:  # noqa: BLE001 - re-raised in the caller's thread
                with lock:
                    if state["err"] is None:
                        state["err"] = e
                return

    if workers <= 1:
        worker()
    else:
        threads = [threading.Thread(target=worker, daemon=True) for _ in range(workers)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
    if state["err"] is not None:
        raise state["err"]
    return state["next"]


def latest(rows, key=lambda r: (r["qid"], r.get("rep", 0))):
    """The last row per key: a retried (arm, qid, rep) supersedes its failed attempt."""
    out = {}
    for r in rows:
        out[key(r)] = r
    return out


# --- data --------------------------------------------------------------------

class Data:
    """The prepared corpus, the questions and (when ingestion has written them) the maps
    between ConditionalQA ids and the tenant's document and chunk ids."""

    def __init__(self, prepared=None, docmap=None, chunkmap=None, subset=None):
        prepared = prepared or config.PREPARED
        self.docs = {d["id"]: d for d in read_jsonl(os.path.join(prepared, "docs.jsonl"))}
        self.qs = {q["qid"]: q for q in read_jsonl(os.path.join(prepared, "questions.jsonl"))}
        self.subset = jload(subset or config.SUBSET)
        self.docmap = _load(docmap or config.DOCMAP)
        self.chunkmap = _load(chunkmap or config.CHUNKMAP)
        self.doc_of_tenant = {v: k for k, v in self.docmap.items()}
        self.section_of_chunk = {cid: (did, i) for did, ids in self.chunkmap.items()
                                 for i, cid in enumerate(ids) if cid}
        self.n_readable = sum(1 for d in self.docs.values() for s in d["sections"] if s["text"].strip())

    def readable(self, did, i):
        d = self.docs.get(did)
        return bool(d) and 0 <= i < len(d["sections"]) and bool(d["sections"][i]["text"].strip())

    def gold(self, qid):
        q = self.qs[qid]
        return {(q["doc_id"], i) for i in q["gold"]}

    def chance5(self, qid):
        return min(1.0, config.K * len(self.qs[qid]["gold"]) / self.n_readable)

    def tenant_doc(self, did):
        return self.docmap.get(did, did)

    def render(self, picks):
        """The reader's excerpts: `[<tenant document id>] <title> > <heading path>` + text."""
        parts = []
        for did, i in picks:
            d = self.docs[did]
            s = d["sections"][i]
            parts.append("[%s] %s\n%s" % (self.tenant_doc(did), " > ".join([d["title"]] + s["path"]), s["text"]))
        return "\n\n".join(parts)


def _load(path):
    return jload(path) if os.path.exists(path) else {}


def spread_sample(subset):
    import random
    # min(): a test subset may be smaller than the sample; the real one (800) never is.
    return set(random.Random(config.SPREAD_SEED).sample(sorted(subset), min(config.SPREAD_N, len(subset))))


# --- citations ---------------------------------------------------------------

def citations(text):
    docs, chunks = [], []
    for kind, cid in CITE.findall(text or ""):
        (docs if kind == "doc" else chunks).append(cid)
    return {"docs": list(dict.fromkeys(docs)), "chunks": list(dict.fromkeys(chunks))}


def cited_pages(data, cites, docs_only=False):
    """ConditionalQA page ids an answer cites; a chunk link counts for its page."""
    pages = {data.doc_of_tenant.get(d, d) for d in cites["docs"]}
    if not docs_only:
        pages |= {data.section_of_chunk[c][0] for c in cites["chunks"] if c in data.section_of_chunk}
    return pages


def has_route(text):
    return any(l.strip().startswith("ROUTE:") for l in (text or "").splitlines())


def route_trailer(text):
    lines = [l.strip() for l in (text or "").splitlines() if l.strip().startswith("ROUTE:")]
    return lines[-1] if lines else None


def strip_trailer(text):
    return "\n".join(l for l in (text or "").splitlines() if not TRAILER.match(l)).strip()


def strip_for_judge(text):
    """One answer as the judge sees it: no accounting lines, and no format that would tell
    an agent's answer from a reader's (links, the ANSWER:/SOURCES: scaffold)."""
    t = strip_trailer(text)
    t = re.sub(r"\[([^\]]*)\]\(loom:[^)]*\)", r"\1", t)
    t = CITE.sub("", t)
    lines = []
    for l in t.splitlines():
        if re.match(r"^\s*SOURCES:", l, re.I):
            continue
        lines.append(re.sub(r"^\s*ANSWER:\s*", "", l, flags=re.I))
    return re.sub(r"[ \t]+\n", "\n", "\n".join(lines)).strip()


def answer_line(text):
    """The reader's `ANSWER:` line (first one), or None."""
    for l in (text or "").splitlines():
        # Tolerates markdown bold around the label (`**ANSWER:** x`), which models add.
        m = re.match(r"^\s*\**\s*ANSWER\s*\**\s*:\s*\**\s*(.*?)\s*\**\s*$", l, re.I)
        if m:
            return m.group(1).strip()
    return None


# --- EM / F1 (SQuAD style, max over gold answers) ------------------------------

_PUNCT = set(string.punctuation)


def normalize(s):
    s = "".join(ch for ch in (s or "").lower() if ch not in _PUNCT)
    s = re.sub(r"\b(a|an|the)\b", " ", s)
    return " ".join(s.split())


def f1(pred, gold):
    p, g = normalize(pred).split(), normalize(gold).split()
    if not p or not g:
        return float(p == g)
    same = sum((collections.Counter(p) & collections.Counter(g)).values())
    if same == 0:
        return 0.0
    pr, rc = same / len(p), same / len(g)
    return 2 * pr * rc / (pr + rc)


def score(pred, golds):
    """(EM, F1). `NO ANSWER` from the condenser scores as an empty answer."""
    if pred is None or pred.strip().upper().rstrip(".") == "NO ANSWER":
        pred = ""
    return (max(float(normalize(pred) == normalize(g)) for g in golds),
            max(f1(pred, g) for g in golds))


# --- retrieval lists -----------------------------------------------------------

def readable_sections(data, sections, n=10):
    """The first n readable, distinct sections in rank order: a bodyless heading is skipped
    as a reader would skip it."""
    out, seen = [], set()
    for s in sections:
        s = tuple(s)
        if s in seen or not data.readable(*s):
            continue
        seen.add(s)
        out.append(s)
        if len(out) == n:
            break
    return out


def recall_at(ranked, gold, k):
    return float(any(tuple(s) in gold for s in ranked[:k]))


def mrr10(ranked, gold):
    for i, s in enumerate(ranked[:10]):
        if tuple(s) in gold:
            return 1.0 / (i + 1)
    return 0.0


# --- usage -------------------------------------------------------------------

class Usage:
    """Tokens, model calls and estimated USD of one row, per serving (provider, model)."""

    def __init__(self):
        self.by = {}
        self.calls_estimated = False
        self.unreported = 0   # chat calls whose response did not name what served them

    def _slot(self, provider, model):
        return self.by.setdefault("%s/%s" % (provider or "", model or ""), {
            "provider": provider or "", "model": model or "", "calls": 0,
            "input_tokens": 0, "output_tokens": 0, "cached_tokens": 0, "cache_creation_tokens": 0, "usd_est": 0.0})

    def add_chat(self, resp, requested):
        """One gateway chat call (counted even when it failed: it may have spent tokens)."""
        prov, model = resp.get("provider"), resp.get("model")
        if not prov and not model:
            prov, model = requested
            self.unreported += 1
        u = resp.get("usage") or {}
        part = {"input_tokens": u.get("input_tokens", u.get("prompt_tokens", 0)) or 0,
                "output_tokens": u.get("output_tokens", u.get("completion_tokens", 0)) or 0,
                "cached_tokens": u.get("cache_read_input_tokens", u.get("cached_tokens", 0)) or 0,
                "cache_creation_tokens": u.get("cache_creation_input_tokens", 0) or 0}
        self._add(prov, model, part, 1)
        return prov, model

    def add_tree(self, tree):
        """A run tree from lc.tree_usage: one row per run. A run may make several model
        calls; the tree reports none, so one call per run that names a model is a floor."""
        for r in tree.get("runs") or []:
            # A code-js parent with no tokens made no model call. One WITH tokens (a search
            # run's rerank is booked on it) still counts, under whatever it names.
            # The live tree names code-js runs `loomcycle/code-js`, so the model is not empty.
            if (r.get("provider") in config.NO_MODEL_PROVIDERS
                    and not (r.get("input_tokens") or r.get("output_tokens"))):
                continue
            self.calls_estimated = True
            self._add(r.get("provider"), r.get("model"), r, 1)

    def _add(self, prov, model, part, calls):
        s = self._slot(prov, model)
        s["calls"] += calls
        for k in ("input_tokens", "output_tokens", "cached_tokens", "cache_creation_tokens"):
            s[k] += part.get(k, 0) or 0
        s["usd_est"] = round(s["usd_est"] + config.price(prov, model, part), 6)

    def served(self):
        return sorted({(s["provider"], s["model"]) for s in self.by.values()})

    def violations(self, extra_pairs=()):
        pairs = set(self.served()) | {tuple(p) for p in extra_pairs}
        return sorted(p for p in pairs if not config.allowed(*p))

    def to_json(self):
        tot = {k: sum(s[k] for s in self.by.values())
               for k in ("calls", "input_tokens", "output_tokens", "cached_tokens", "cache_creation_tokens")}
        tot["usd_est"] = round(sum(s["usd_est"] for s in self.by.values()), 6)
        return {"total": tot, "by_model": self.by, "calls_estimated": self.calls_estimated,
                "chat_serving_unreported": self.unreported}


def main_model(usage_json):
    """The (provider, model) that wrote the most output tokens: a kb tree's de-facto reader."""
    best = None
    for s in (usage_json or {}).get("by_model", {}).values():
        if s["provider"] in config.NO_MODEL_PROVIDERS:
            continue
        if best is None or s["output_tokens"] > best["output_tokens"]:
            best = s
    return (best["provider"], best["model"]) if best else None


def p90(values):
    v = sorted(values)
    if not v:
        return None
    return v[min(len(v) - 1, int(0.9 * len(v)))]
