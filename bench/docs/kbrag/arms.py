"""Run the Study C arms (kb, rag-lc, rag-ext, oracle) and record one JSON line per run.

    python3 -I bench/docs/kbrag/arms.py --profile haiku --arms kb,rag-lc,rag-ext,oracle --deadline 2026-11-23T07:00:00Z

-> <work>/runs/<arm>.jsonl, one row per (arm, qid, rep, attempt); runs/versions.jsonl (the
deployment version at the start and end of every invocation, instrument check 9);
runs/events.jsonl (starts, ends and every guard stop).

Plan (PREREG "Runs"): rep 0 of every question first, then reps 1-2 of the fixed 100-question
spread sample; `--reps N` replaces that with reps 0..N-1 for every question. Per (question,
rep) the arms run in an order rotated by a seeded RNG, so server drift reaches all alike.

--no-spread runs rep 0 only; --spread-only runs only the spread reps (1-2 of the fixed 100),
so the two parts can be separate invocations into the same --out. Arms can be split across
invocations too (`--arms rag-lc,rag-ext,oracle` now, `--arms kb` later): rows are per arm.

--workers N keeps N runs in flight on threads. Runs are SUBMITTED (started) in the seeded
order above; with N > 1 they complete in a nondeterministic order, so row order in the files
is not the plan order (rows are keyed by (arm, qid, rep, attempt), never by position).

Resumable: an (arm, qid, rep) whose latest row is ok is skipped; a failed one is retried only
with --retry-failed, and at most --max-attempts times in all (PREREG check 8: retried once).

Guards, each checked before a run: --deadline (now + p90 of the arm's completed durations),
--budget-usd (lc.spend() + p90 of the arm's completed row costs, seeded with a floor). After a
run: any serving (provider, model) outside config.ALLOWED marks the row provider_violation
and stops everything.
"""
import argparse, datetime, json, os, random, sys, threading, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common, config, prompts  # noqa: E402
from common import Usage  # noqa: E402


class Stop(Exception):
    """A guard stopped the runner; the message says which and why."""


def now():
    return datetime.datetime.now(datetime.timezone.utc)


def iso(t=None):
    return (t or now()).strftime("%Y-%m-%dT%H:%M:%SZ")


# --- the arms ----------------------------------------------------------------------

def read_sections(lc, data, q, picks, pair, usage, row):
    """The shared reader call over at most K sections."""
    prompt = prompts.READER.format(sections=data.render(picks) or "(no sections)",
                                   question=prompts.question_text(q))
    raw, err, served = prompts.ask(lc, prompt, pair, usage)
    row["reader_served"] = list(served)
    row["read_sections"] = [list(p) for p in picks]
    if raw is None:
        row["status"], row["error"] = "failed", "reader: %s" % err
        return
    row["raw_answer"] = raw
    row["answer_line"] = common.answer_line(raw)
    row["citations"] = common.citations(raw)


def arm_kb(lc, data, q, rep, attempt, pair, usage, row, ctx):
    key = "%s:kb:%s:%d" % (ctx.get("key_prefix", "kbrag-cqa"), q["qid"], rep)
    if attempt > 1:  # a fresh key, or the server would hand back the failed run
        key += ":a%d" % attempt
    r = lc.run("kb/ask", prompts.question_text(q),
               metadata={"depth": "standard", "bench": "kbrag-cqa", "qid": q["qid"], "rep": rep},
               user_tier="free", idempotency_key=key)
    row["run_ids"] = [r.get("run_id")]
    text = r.get("text") or ""
    row["raw_answer"] = text
    row["route"] = common.route_trailer(text)
    row["citations"] = common.citations(text)
    _tree(lc, r.get("run_id"), usage, row)
    if r.get("status") != "completed":
        row["status"], row["error"] = "failed", "run %s: %s" % (r.get("status"), r.get("error"))
    elif not common.has_route(text):
        row["status"], row["error"] = "failed", "no ROUTE: trailer"


def _tree(lc, run_id, usage, row):
    if not run_id:
        return
    try:
        usage.add_tree(lc.tree_usage(run_id))
    except Exception as e:  # without the tree the provider guard cannot be checked
        row["status"], row["error"] = "failed", "tree_usage: %s" % str(e)[:300]


def arm_raglc(lc, data, q, rep, attempt, pair, usage, row, ctx):
    key = "%s:rag-lc:%s:%d" % (ctx.get("key_prefix", "kbrag-cqa"), q["qid"], rep) + (":a%d" % attempt if attempt > 1 else "")
    r = lc.run(config.RAGLC_AGENT, q["query"],
               metadata={"bench": "kbrag-cqa", "arm": "rag-lc", "qid": q["qid"], "rep": rep},
               user_tier="free", idempotency_key=key)
    row["run_ids"] = [r.get("run_id")]
    _tree(lc, r.get("run_id"), usage, row)
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
    secs = [data.section_of_chunk[c] for c in ids if c in data.section_of_chunk]
    readable = common.readable_sections(data, secs, 10)
    row["retrieval"] = {
        "ranked_chunks": ids, "unmapped": [c for c in ids if c not in data.section_of_chunk],
        "sections": [list(s) for s in readable],
        "reranked": res.get("reranked"), "rerank_reason": res.get("rerank_reason"),
        # Instrument check 3: a passage-only search must carry neither field.
        "matched_annotation": sum(1 for h in hits if isinstance(h, dict) and "matched_annotation" in h),
        "summary": sum(1 for h in hits if isinstance(h, dict) and "summary" in h) + int("summary" in res),
    }
    read_sections(lc, data, q, readable[:config.K], pair, usage, row)


def arm_ragext(lc, data, q, rep, attempt, pair, usage, row, ctx):
    rec = ctx["ragext"]().retrieve(lc, q, pair, usage)
    secs = [tuple(s) for s in rec["sections"]]
    rec["sections"] = [list(s) for s in common.readable_sections(data, secs, 10)]
    row["retrieval"] = rec
    read_sections(lc, data, q, [tuple(s) for s in rec["sections"][:config.K]], pair, usage, row)


def arm_oracle(lc, data, q, rep, attempt, pair, usage, row, ctx):
    picks = [(q["doc_id"], i) for i in q["gold"] if data.readable(q["doc_id"], i)][:config.K]
    read_sections(lc, data, q, picks, pair, usage, row)


ARM_FN = {"kb": arm_kb, "rag-lc": arm_raglc, "rag-ext": arm_ragext, "oracle": arm_oracle}


# --- the runner --------------------------------------------------------------------

class Runner:
    def __init__(self, lc, data, *, profile, arms, out_dir=None, retry_failed=False, max_attempts=2,
                 deadline=None, budget_usd=config.BUDGET_USD, ragext_factory=None, log=print, clock=now,
                 workers=1, key_prefix="kbrag-cqa"):
        self.key_prefix = key_prefix
        self.lc, self.data, self.profile, self.arms = lc, data, profile, list(arms)
        self.pair = config.reader(profile)
        self.dir = out_dir or config.RUNS
        self.retry_failed, self.max_attempts = retry_failed, max_attempts
        self.deadline, self.budget = deadline, budget_usd
        self.log, self.clock = log, clock
        self.workers = max(1, int(workers or 1))
        self._ragext, self._ragext_factory = None, ragext_factory
        self.rows = {a: common.read_jsonl(self.path(a)) for a in config.ARMS}
        # One lock for the shared state the workers touch: the rows lists, the in-flight
        # reservations the spend guard counts, the session's run counter.
        self.lock = threading.RLock()
        self._ragext_lock = threading.Lock()
        self.inflight = {}    # arm -> runs started and not yet written

    def path(self, arm):
        return os.path.join(self.dir, arm + ".jsonl")

    def event(self, kind, **kw):
        common.append_jsonl(os.path.join(self.dir, "events.jsonl"), dict(kw, ts=iso(self.clock()), kind=kind,
                                                                         profile=self.profile))

    def ragext(self):
        with self._ragext_lock:  # built once, by whichever worker asks first
            return self._ragext_build()

    def _ragext_build(self):
        if self._ragext is None:
            if self._ragext_factory:
                self._ragext = self._ragext_factory()
            else:
                import ragext
                chunks = ragext.build_chunks(self.data.docs)
                self._ragext = ragext.RagExt(self.data.docs, ragext.Dense.load(config.RAGEXT, chunks), chunks)
        return self._ragext

    def plan(self, qids, reps=None, spread="all"):
        """[(qid, rep)]: rep 0 of every question first, then the spread reps.
        spread="none" is rep 0 only (the primary runs); "only" is the spread reps only (1..2
        of the fixed 100-question sample), so the two can run as separate invocations."""
        if reps is not None:
            if spread != "all":
                raise SystemExit("--reps replaces the PREREG plan; it cannot be combined with --no-spread/--spread-only")
            return [(q, r) for r in range(reps) for q in qids]
        sample = common.spread_sample(self.data.subset)
        out = [(q, 0) for q in qids] if spread != "only" else []
        if spread != "none":
            for r in range(1, config.SPREAD_REPS):
                out += [(q, r) for q in qids if q in sample]
        return out

    def jobs(self, qids, reps=None, spread="all"):
        """[(arm, qid, rep, attempt)] in submission order: per (question, rep) the arms
        rotated by the seeded RNG; (arm, qid, rep) already ok, or out of attempts, left out."""
        out = []
        for qid, rep in self.plan(qids, reps, spread):
            order = list(self.arms)
            k = random.Random("order:%s:%d" % (qid, rep)).randrange(len(order))
            for arm in order[k:] + order[:k]:
                go, attempt = self.todo(arm, qid, rep)
                if go:
                    out.append((arm, qid, rep, attempt))
        return out

    def check_profile(self):
        seen = {r.get("profile") for a in self.arms for r in self.rows[a]}
        seen.discard(None)
        if seen and seen != {self.profile}:
            raise Stop("the run files already hold profile %s; a study never mixes profiles (move them aside "
                       "to start another study)" % ", ".join(sorted(seen)))

    def todo(self, arm, qid, rep):
        """(should run, attempt number)."""
        mine = [r for r in self.rows[arm] if r["qid"] == qid and r.get("rep", 0) == rep]
        if not mine:
            return True, 1
        if mine[-1]["status"] == "ok":
            return False, None
        n = len(mine)
        return (self.retry_failed and n < self.max_attempts), n + 1

    def guard_before(self, arm):
        """Checked before a run starts. On success one run of `arm` is reserved (in flight)
        until release(); under --workers the spend guard counts every reserved run at the
        arm's estimate, since none of them is in the ledger yet. Held under the lock, so two
        workers never both pass on the same headroom."""
        with self.lock:
            done = [r for r in self.rows[arm] if r["status"] == "ok"]
            if self.deadline:
                d = common.p90([r["wall_s"] for r in done])
                if d is not None and self.clock() + datetime.timedelta(seconds=d) > self.deadline:
                    raise Stop("deadline: the next %s run (p90 %.0f s) would pass %s" % (arm, d, iso(self.deadline)))
            if self.budget is not None:
                if not hasattr(self.lc, "spend"):
                    raise Stop("budget: lc.spend() is not available, so the spend cap cannot be held")
                spent = self.lc.spend()
                pending = sum(self.est(a) * n for a, n in self.inflight.items())
                est = self.est(arm)
                if spent + pending + est > self.budget:
                    raise Stop("spend cap: spent $%.4f + in flight ~$%.4f + next %s run ~$%.4f would pass $%.2f"
                               % (spent, pending, arm, est, self.budget))
            self.inflight[arm] = self.inflight.get(arm, 0) + 1

    def release(self, arm):
        with self.lock:
            self.inflight[arm] -= 1

    def est(self, arm):
        """p90 estimated cost of the arm's ok rows (the floor until 5 exist)."""
        costs = [r["usage"]["total"]["usd_est"] for r in self.rows[arm] if r["status"] == "ok"]
        return common.p90(costs) if len(costs) >= 5 else config.COST_FLOOR.get(arm, config.COST_FLOOR_DEFAULT)

    def one(self, arm, qid, rep, attempt):
        q = self.data.qs[qid]
        usage = Usage()
        row = {"arm": arm, "qid": qid, "rep": rep, "attempt": attempt, "profile": self.profile,
               "reader": list(self.pair), "status": "ok", "error": None, "ts": iso(self.clock())}
        t0 = time.time()
        try:
            ARM_FN[arm](self.lc, self.data, q, rep, attempt, self.pair, usage, row, {"ragext": self.ragext, "key_prefix": self.key_prefix})
        except Stop:
            raise
        except Exception as e:  # one broken run is a failed row, never a dead runner
            row["status"], row["error"] = "failed", "%s: %s" % (type(e).__name__, str(e)[:300])
        row["wall_s"] = round(time.time() - t0, 2)
        row["usage"] = usage.to_json()
        row["served"] = [list(p) for p in usage.served()]
        bad = usage.violations()
        if bad:
            row["status"] = "provider_violation"
            row["error"] = "served by %s, outside the study's allowed models" % ", ".join("%s/%s" % p for p in bad)
        with self.lock:
            common.append_jsonl(self.path(arm), row)
            self.rows[arm].append(row)
        if bad:
            self.event("provider_violation", arm=arm, qid=qid, rep=rep, served=row["served"])
            raise Stop("PROVIDER VIOLATION on %s %s rep %d: %s. The runner stopped; no run started after it "
                       "(with --workers, runs already in flight finished and wrote their rows)."
                       % (arm, qid, rep, row["error"]))
        return row

    def run(self, qids, reps=None, spread="all"):
        self.check_profile()
        started = iso(self.clock())
        jobs = self.jobs(qids, reps, spread)
        self.event("start", arms=self.arms, n_qids=len(qids), jobs=len(jobs), workers=self.workers, spread=spread)
        self.version("start")
        count = {"n": 0}

        def job(j):
            arm, qid, rep, attempt = j
            self.guard_before(arm)
            try:
                row = self.one(arm, qid, rep, attempt)
            finally:
                self.release(arm)
            with self.lock:
                count["n"] += 1
                n = count["n"]
                if row["status"] != "ok":
                    self.log("%s %s rep %d failed: %s" % (arm, qid, rep, row["error"]))
                if n % 25 == 0:
                    self.log("%d runs this session (last %s %s rep %d)" % (n, arm, qid, rep))
                    self.ledger_guard(started)

        try:
            common.run_jobs(jobs, job, self.workers)
        except Stop as e:
            self.event("stop", reason=str(e), runs=count["n"])
            self.version("end")
            raise
        try:
            self.ledger_guard(started)
        except Stop as e:
            self.event("stop", reason=str(e))
            raise
        finally:
            self.version("end")
        self.event("end", runs=count["n"])
        return count["n"]

    def ledger_guard(self, since):
        """The usage ledger's view: every (provider, model) billed on the tenant since this
        session started. It catches a call no run tree names (a rerank booked on a code-js
        run). It also sees other work on the tenant in that window, which is bound by the
        same policy, so a hit there is a violation all the same."""
        if not (hasattr(self.lc, "ledger_pairs") and hasattr(self.lc, "disallowed")):
            return
        try:
            bad = self.lc.disallowed(self.lc.ledger_pairs(since=since))
        except Exception as e:  # a failed read proves nothing either way; the run trees still guard
            self.log("usage ledger check skipped: %s" % str(e)[:200])
            return
        if bad:
            self.event("provider_violation", source="usage ledger", served=[list(p) for p in bad])
            raise Stop("PROVIDER VIOLATION in the usage ledger since %s: %s" % (since, ", ".join("%s/%s" % tuple(p) for p in bad)))

    def version(self, when):
        try:
            v = self.lc.version()
        except Exception as e:
            v = {"error": str(e)[:200]}
        common.append_jsonl(os.path.join(self.dir, "versions.jsonl"), dict(v, when=when, ts=iso(self.clock())))


def key_prefix(out):
    """The idempotency-key namespace of an output root. The server keeps a run's key for
    good (a unique index), so a smoke or pilot that reused the study's keys would hand its
    runs back to the study. The study root gets `kbrag-cqa-study`; any other root its own
    name. (The 2026-11-23 smoke used the bare `kbrag-cqa` prefix for 4 subset questions,
    which is why the study does not.)"""
    if os.path.abspath(out) == os.path.abspath(config.WORK):
        return "kbrag-cqa-study"
    return "kbrag-cqa-" + os.path.basename(os.path.abspath(out))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", default=config.DEFAULT_PROFILE, choices=sorted(config.PROFILES))
    ap.add_argument("--arms", default="kb,rag-lc,rag-ext,oracle")
    ap.add_argument("--qids", help="comma-separated qids (default: the 800 of reader_subset.json)")
    ap.add_argument("--limit", type=int)
    ap.add_argument("--reps", type=int, help="reps 0..N-1 for every question (default: the PREREG plan)")
    ap.add_argument("--retry-failed", action="store_true")
    ap.add_argument("--max-attempts", type=int, default=2)
    ap.add_argument("--deadline", help="UTC ISO time, e.g. 2026-11-23T07:00:00Z")
    ap.add_argument("--budget-usd", type=float, default=config.BUDGET_USD)
    ap.add_argument("--out", default=config.WORK,
                    help="output root: rows go to <out>/runs (default %(default)s); a smoke or pilot uses its own "
                         "root so its rows never mix with the study's")
    ap.add_argument("--workers", type=int, default=1,
                    help="runs in flight at once (threads; default 1). Runs START in the seeded order; they "
                         "complete in whatever order the server finishes them")
    sp = ap.add_mutually_exclusive_group()
    sp.add_argument("--no-spread", action="store_true", help="rep 0 only (the primary runs), no spread reps")
    sp.add_argument("--spread-only", action="store_true", help="only reps 1-2 of the fixed 100-question spread sample")
    a = ap.parse_args()
    spread = "none" if a.no_spread else "only" if a.spread_only else "all"
    arms = [x for x in a.arms.split(",") if x]
    for x in arms:
        if x not in ARM_FN:
            raise SystemExit("unknown arm %r" % x)
    data = common.Data()
    if not data.docmap or not data.chunkmap:
        raise SystemExit("docmap.json / chunks.json are missing under %s: ingestion has not finished" % config.WORK)
    qids = a.qids.split(",") if a.qids else list(data.subset)
    if a.limit:
        qids = qids[:a.limit]
    deadline = None
    if a.deadline:
        deadline = datetime.datetime.fromisoformat(a.deadline.replace("Z", "+00:00"))
        if deadline.tzinfo is None:
            deadline = deadline.replace(tzinfo=datetime.timezone.utc)
    import lc
    r = Runner(lc, data, profile=a.profile, arms=arms, out_dir=os.path.join(a.out, "runs"),
               retry_failed=a.retry_failed, max_attempts=a.max_attempts, deadline=deadline,
               budget_usd=a.budget_usd, log=lambda s: print(s, flush=True), workers=a.workers,
               key_prefix=key_prefix(a.out))
    try:
        n = r.run(qids, a.reps, spread)
    except Stop as e:
        print("STOPPED: %s" % e, flush=True)
        sys.exit(2)
    print("done: %d runs" % n, flush=True)


if __name__ == "__main__":
    main()
