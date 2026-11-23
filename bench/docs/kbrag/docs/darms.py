"""Run the Study D arms (kb, rag-lc, rag-ext) and record one JSON line per run.

    python3 -I bench/docs/kbrag/docs/darms.py --out <root> --sets p7 --reps 3 --workers 4

-> <root>/runs/<arm>.jsonl, one row per (arm, qid, rep, attempt); runs/events.jsonl;
runs/versions.jsonl (instrument check 9).

Order: rep-major (rep 0 of every question, then rep 1, ...); per (question, rep) the three
arms run back to back in an order rotated by a seeded RNG, so drift on the server reaches
all arms alike. --qids limits the questions; --reps-for SET=N sets a set's runs per arm.

Guards before every run: the spend cap (tenant ledger increase over the recorded baseline
+ in-flight runs at each arm's p90 estimate, against --budget-usd) and an optional deadline.
After every run: a serving (provider, model) outside the policy (../config.allowed: Haiku,
any ollama-local model, code-js) marks the row provider_violation and stops everything;
every 20 runs and at the end the usage ledger since start is checked the same way.

Resumable: an (arm, qid, rep) whose latest row is ok is skipped; --retry-failed retries a
failed one once (PREREG check 8). Idempotency keys are prefixed by the root's name, so a
smoke root never hands its runs to the study root.
"""
import argparse, datetime, json, os, random, sys, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import dconfig, dprompts, questions  # noqa: E402
import common  # noqa: E402  (../common.py: Usage, citations, run_jobs, jsonl io)
from common import Usage  # noqa: E402


class Stop(Exception):
    pass


def iso(t=None):
    return (t or datetime.datetime.now(datetime.timezone.utc)).strftime("%Y-%m-%dT%H:%M:%SZ")


class Corpus:
    def __init__(self):
        s = json.load(open(dconfig.SECTIONS, encoding="utf-8"))
        self.docs, self.chunk_doc = s["docs"], s["chunk_doc"]
        self.section_of_chunk = {sec["chunk_id"]: (did, i) for did, d in self.docs.items()
                                 for i, sec in enumerate(d["sections"])}

    def readable(self, did, i):
        d = self.docs.get(did)
        return bool(d) and 0 <= i < len(d["sections"]) and bool(d["sections"][i]["text"].strip())

    def first_readable(self, secs, n):
        out, seen = [], set()
        for s in secs:
            s = tuple(s)
            if s in seen or not self.readable(*s):
                continue
            seen.add(s)
            out.append(s)
            if len(out) == n:
                break
        return out

    def render(self, picks, cap=dconfig.SECTION_CHARS):
        parts = []
        for did, i in picks:
            d = self.docs[did]
            s = d["sections"][i]
            text = s["text"] if len(s["text"]) <= cap else s["text"][:cap] + "\n[... section cut at %d characters]" % cap
            parts.append("[%s] %s\n%s" % (did, " > ".join([d["title"] or ""] + s["path"]), text))
        return "\n\n".join(parts)

    def cited_docs(self, cites, docs_only=False):
        out = set(cites.get("docs") or [])
        if not docs_only:
            out |= {self.chunk_doc[c] for c in cites.get("chunks") or [] if c in self.chunk_doc}
        return out


# --- the arms ---------------------------------------------------------------------

def reader(lc, corpus, q, picks, usage, row):
    prompt = dprompts.READER.format(sections=corpus.render(picks) or "(no sections)", question=q["text"])
    raw, err, served = dprompts.ask(lc, prompt, dconfig.READER, usage, max_tokens=8192)
    row["reader_served"] = list(served)
    row["read_sections"] = [list(p) for p in picks]
    if raw is None:
        row["status"], row["error"] = "failed", "reader: %s" % err
        return
    row["raw_answer"] = raw
    row["citations"] = common.citations(raw)


def tree(lc, run_id, usage, row):
    if not run_id:
        return
    try:
        t = lc.tree_usage(run_id)
        usage.add_tree(t)
        row["tree"] = [{k: r.get(k) for k in ("agent", "provider", "model", "status", "input_tokens", "output_tokens")}
                       for r in t["runs"]]
    except Exception as e:  # without the tree the provider guard cannot be checked
        row["status"], row["error"] = "failed", "tree_usage: %s" % str(e)[:300]


def key(ctx, arm, q, rep, attempt):
    return "%s:%s:%s:%d" % (ctx["prefix"], arm, q["id"], rep) + (":a%d" % attempt if attempt > 1 else "")


def arm_kb(lc, corpus, q, rep, attempt, usage, row, ctx):
    r = lc.run("kb/ask", q["text"], metadata={"depth": "standard", "bench": "kbrag-docs", "qid": q["id"], "rep": rep},
               user_tier="free", idempotency_key=key(ctx, "kb", q, rep, attempt), timeout=2400)
    row["run_ids"] = [r.get("run_id")]
    text = r.get("text") or ""
    row["raw_answer"] = text
    row["citations"] = common.citations(text)
    lines = [l.strip() for l in text.splitlines()]
    row["route"] = next((l for l in reversed(lines) if l.startswith("ROUTE:")), None)
    row["coverage"] = next((l for l in reversed(lines) if l.startswith("COVERAGE:")), None)
    tree(lc, r.get("run_id"), usage, row)
    if r.get("status") != "completed":
        row["status"], row["error"] = "failed", "run %s: %s" % (r.get("status"), r.get("error"))
    elif not row["route"]:
        row["status"], row["error"] = "failed", "no ROUTE: trailer"


def arm_raglc(lc, corpus, q, rep, attempt, usage, row, ctx):
    r = lc.run(dconfig.RAGLC_AGENT, q["text"], metadata={"bench": "kbrag-docs", "arm": "rag-lc", "qid": q["id"], "rep": rep},
               user_tier="free", idempotency_key=key(ctx, "rag-lc", q, rep, attempt))
    row["run_ids"] = [r.get("run_id")]
    tree(lc, r.get("run_id"), usage, row)
    if r.get("status") != "completed":
        row["status"], row["error"] = "failed", "search run %s: %s" % (r.get("status"), r.get("error"))
        return
    try:
        res = json.loads(r.get("text") or "")
    except ValueError:
        row["status"], row["error"] = "failed", "search result is not JSON: %s" % (r.get("text") or "")[:200]
        return
    hits = res.get("chunks") or res.get("results") or []
    ids = [h.get("chunk_id") or h.get("id") for h in hits if isinstance(h, dict)]
    secs = [corpus.section_of_chunk[c] for c in ids if c in corpus.section_of_chunk]
    picks = corpus.first_readable(secs, dconfig.K)
    row["retrieval"] = {
        "ranked_chunks": ids, "unmapped": [c for c in ids if c not in corpus.section_of_chunk],
        "docs_ranked": list(dict.fromkeys(corpus.chunk_doc.get(c) for c in ids if c in corpus.chunk_doc)),
        "sections": [list(s) for s in picks], "reranked": res.get("reranked"), "rerank_reason": res.get("rerank_reason"),
        "filter": res.get("filter"),
        # Instrument check 3: a passage-only search must carry neither field.
        "matched_annotation": sum(1 for h in hits if isinstance(h, dict) and "matched_annotation" in h),
        "summary": sum(1 for h in hits if isinstance(h, dict) and "summary" in h) + int("summary" in res)}
    reader(lc, corpus, q, picks, usage, row)


def arm_ragext(lc, corpus, q, rep, attempt, usage, row, ctx):
    rec = ctx["ragext"]().retrieve(lc, q["text"], dconfig.READER, usage, keep_sections=3 * dconfig.K)
    picks = corpus.first_readable(rec["sections"], dconfig.K)
    rec["sections"] = [list(s) for s in picks]
    row["retrieval"] = rec
    reader(lc, corpus, q, picks, usage, row)


ARM_FN = {"kb": arm_kb, "rag-lc": arm_raglc, "rag-ext": arm_ragext}


# --- the runner --------------------------------------------------------------------

class Runner:
    def __init__(self, lc, corpus, qs, *, arms, out, budget, baseline, workers, retry_failed, deadline, log=print):
        self.lc, self.corpus, self.qs, self.arms = lc, corpus, qs, list(arms)
        self.dir = os.path.join(out, "runs")
        self.prefix = "kbrag-docs-" + os.path.basename(os.path.abspath(out))
        self.budget, self.baseline, self.workers = budget, baseline, workers
        self.retry_failed, self.deadline, self.log = retry_failed, deadline, log
        self.rows = {a: common.read_jsonl(self.path(a)) for a in dconfig.ARMS}
        self.lock = threading.RLock()
        self.inflight = {}
        self._rx, self._rx_lock = None, threading.Lock()

    def path(self, arm):
        return os.path.join(self.dir, arm + ".jsonl")

    def event(self, kind, **kw):
        common.append_jsonl(os.path.join(self.dir, "events.jsonl"), dict(kw, ts=iso(), kind=kind))

    def ragext(self):
        with self._rx_lock:
            if self._rx is None:
                import ragext_docs
                self._rx = ragext_docs.build()
            return self._rx

    def todo(self, arm, qid, rep):
        mine = [r for r in self.rows[arm] if r["qid"] == qid and r["rep"] == rep]
        if not mine:
            return True, 1
        if mine[-1]["status"] == "ok":
            return False, None
        return (self.retry_failed and len(mine) < 2), len(mine) + 1

    def jobs(self, plan):
        out = []
        for qid, rep in plan:
            order = list(self.arms)
            k = random.Random("order:%s:%d" % (qid, rep)).randrange(len(order))
            for arm in order[k:] + order[:k]:
                go, attempt = self.todo(arm, qid, rep)
                if go:
                    out.append((arm, qid, rep, attempt))
        return out

    def est(self, arm):
        c = [r["usage"]["total"]["usd_est"] for r in self.rows[arm] if r["status"] == "ok"]
        return common.p90(c) if len(c) >= 3 else dconfig.COST_FLOOR.get(arm, dconfig.COST_FLOOR_DEFAULT)

    def guard(self, arm):
        with self.lock:
            if self.deadline and datetime.datetime.now(datetime.timezone.utc) > self.deadline:
                raise Stop("deadline %s passed" % iso(self.deadline))
            spent = self.lc.spend() - self.baseline
            pending = sum(self.est(a) * n for a, n in self.inflight.items())
            if spent + pending + self.est(arm) > self.budget:
                raise Stop("spend cap: increase $%.4f + in flight ~$%.4f + next %s ~$%.4f would pass $%.2f"
                           % (spent, pending, arm, self.est(arm), self.budget))
            self.inflight[arm] = self.inflight.get(arm, 0) + 1

    def ledger_guard(self, since):
        try:
            rows = self.lc.usage_rows("provider,model", since)
        except Exception as e:
            self.log("ledger check skipped: %s" % str(e)[:200])
            return
        bad = sorted({(r.get("provider") or "", r.get("model") or "") for r in rows} - {("", "")})
        bad = [p for p in bad if not (common.config.allowed(*p) or p[1].startswith("bge-m3"))]
        if bad:
            self.event("provider_violation", source="usage ledger", served=bad)
            raise Stop("PROVIDER VIOLATION in the usage ledger since %s: %s" % (since, bad))

    def one(self, arm, qid, rep, attempt):
        q = self.qs[qid]
        usage = Usage()
        row = {"arm": arm, "qid": qid, "set": q["set"], "rep": rep, "attempt": attempt, "status": "ok", "error": None,
               "ts": iso()}
        t0 = time.time()
        try:
            ARM_FN[arm](self.lc, self.corpus, q, rep, attempt, usage, row, {"prefix": self.prefix, "ragext": self.ragext})
        except Stop:
            raise
        except Exception as e:
            row["status"], row["error"] = "failed", "%s: %s" % (type(e).__name__, str(e)[:300])
        row["wall_s"] = round(time.time() - t0, 2)
        row["usage"] = usage.to_json()
        row["served"] = [list(p) for p in usage.served()]
        bad = usage.violations()
        if bad:
            row["status"], row["error"] = "provider_violation", "served by %s" % bad
        with self.lock:
            common.append_jsonl(self.path(arm), row)
            self.rows[arm].append(row)
        if bad:
            self.event("provider_violation", arm=arm, qid=qid, rep=rep, served=row["served"])
            raise Stop("PROVIDER VIOLATION on %s %s rep %d: %s" % (arm, qid, rep, row["error"]))
        return row

    def run(self, plan):
        started = iso()
        jobs = self.jobs(plan)
        self.event("start", arms=self.arms, jobs=len(jobs), workers=self.workers)
        self.version("start")
        n = {"n": 0}

        def job(j):
            arm, qid, rep, attempt = j
            self.guard(arm)
            try:
                row = self.one(arm, qid, rep, attempt)
            finally:
                with self.lock:
                    self.inflight[arm] -= 1
            with self.lock:
                n["n"] += 1
                if row["status"] != "ok":
                    self.log("%s %s rep %d failed: %s" % (arm, qid, rep, row["error"]))
                if n["n"] % 20 == 0:
                    self.log("%s %d/%d runs; tenant increase $%.4f" % (iso(), n["n"], len(jobs), self.lc.spend() - self.baseline))
                    self.ledger_guard(started)

        try:
            common.run_jobs(jobs, job, self.workers)
            self.ledger_guard(started)
        except Stop as e:
            self.event("stop", reason=str(e), runs=n["n"])
            raise
        finally:
            self.version("end")
        self.event("end", runs=n["n"])
        return n["n"]

    def version(self, when):
        try:
            v = self.lc.version()
        except Exception as e:
            v = {"error": str(e)[:200]}
        common.append_jsonl(os.path.join(self.dir, "versions.jsonl"), dict(v, when=when, ts=iso()))


def plan(qs, reps_for):
    """[(qid, rep)], rep-major, questions in a seeded order."""
    ids = sorted(qs)
    random.Random(20261123).shuffle(ids)
    top = max(reps_for.get(qs[q]["set"], 0) for q in ids) if ids else 0
    return [(q, r) for r in range(top) for q in ids if r < reps_for.get(qs[q]["set"], 0)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--sets", default="p7")
    ap.add_argument("--arms", default="kb,rag-lc,rag-ext")
    ap.add_argument("--qids")
    ap.add_argument("--reps", type=int, default=dconfig.REPS, help="runs per question per arm, every set")
    ap.add_argument("--reps-for", action="append", default=[], help="SET=N overrides --reps for one set")
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--retry-failed", action="store_true")
    ap.add_argument("--budget-usd", type=float, default=dconfig.BUDGET_USD)
    ap.add_argument("--deadline")
    a = ap.parse_args()
    import dlc
    dlc.assert_tenant()
    base = json.load(open(os.path.join(dconfig.WORK, "baseline.json")))["tenant_spend_usd"]
    qs = questions.load([s for s in a.sets.split(",") if s])
    if a.qids:
        keep = set(a.qids.split(","))
        qs = {k: v for k, v in qs.items() if k in keep}
    reps = {s: a.reps for s in ("p7", "waves", "known")}
    for x in a.reps_for:
        s, n = x.split("=")
        reps[s] = int(n)
    deadline = datetime.datetime.fromisoformat(a.deadline.replace("Z", "+00:00")) if a.deadline else None
    r = Runner(dlc, Corpus(), qs, arms=[x for x in a.arms.split(",") if x], out=a.out, budget=a.budget_usd,
               baseline=base, workers=a.workers, retry_failed=a.retry_failed, deadline=deadline,
               log=lambda s: print(s, flush=True))
    try:
        n = r.run(plan(qs, reps))
    except Stop as e:
        print("STOPPED: %s" % e, flush=True)
        sys.exit(2)
    print("done: %d runs" % n, flush=True)


if __name__ == "__main__":
    main()
