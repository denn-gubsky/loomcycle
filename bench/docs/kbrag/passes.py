"""The resumable model-call passes over the run files (condense, judge), with the guards
arms.py holds: spend cap before a call, provider violation after it.

`workers` keeps that many calls in flight (threads). Calls START in the order of `items`;
with workers > 1 they finish in any order (rows are keyed, never read by position). A guard
stop lets the calls in flight finish and write their rows whole, starts nothing new, and is
raised once every worker has stopped."""
import os, threading, time

import common, config
from common import Usage


class Stop(Exception):
    pass


def latest_ok_runs(arm, runs_dir=None):
    """(qid, rep) -> the latest run row of an arm, when that row is ok."""
    rows = common.latest(common.read_jsonl(os.path.join(runs_dir or config.RUNS, arm + ".jsonl")))
    return {k: r for k, r in rows.items() if r["status"] == "ok"}


def run_pass(lc, items, call, out_path, *, budget_usd, log=print, floor=config.COST_FLOOR_DEFAULT, workers=1):
    """items: [(key, src row)], key = (qid, rep, attempt). `call(src, usage) -> dict` makes
    the model call(s) and returns the output fields; a row with status ok is never redone.
    out_path: one file, or a function src -> file (the judge writes one file per arm)."""
    path_of = out_path if callable(out_path) else (lambda src: out_path)
    lock = threading.Lock()
    done, costs = set(), []
    for p in sorted({path_of(src) for _, src in items}):
        have = common.read_jsonl(p)
        done |= {(p, r["qid"], r["rep"], r["attempt"]) for r in have if r["status"] == "ok"}
        costs += [r["usage"]["total"]["usd_est"] for r in have if r["status"] == "ok"]
    todo = [(key, src) for key, src in items if (path_of(src),) + tuple(key) not in done]
    state = {"n": 0, "inflight": 0}

    def est():
        return common.p90(costs) if len(costs) >= 5 else floor

    def one(item):
        key, src = item
        if budget_usd is not None:
            with lock:  # calls in flight are not in the ledger yet: count each at the estimate
                spent = lc.spend()
                if spent + est() * (state["inflight"] + 1) > budget_usd:
                    raise Stop("spend cap: spent $%.4f + %d in flight + next call ~$%.4f each would pass $%.2f"
                               % (spent, state["inflight"], est(), budget_usd))
                state["inflight"] += 1
        else:
            with lock:
                state["inflight"] += 1
        try:
            usage = Usage()
            row = {"arm": src["arm"], "qid": src["qid"], "rep": src["rep"], "attempt": src["attempt"],
                   "profile": src.get("profile"), "status": "ok", "error": None}
            t0 = time.time()
            try:
                row.update(call(src, usage))
            except Exception as e:
                row["status"], row["error"] = "failed", "%s: %s" % (type(e).__name__, str(e)[:300])
            row["wall_s"] = round(time.time() - t0, 2)
            row["usage"] = usage.to_json()
            row["served"] = [list(p) for p in usage.served()]
            bad = usage.violations()
            if bad:
                row["status"] = "provider_violation"
                row["error"] = "served by %s" % ", ".join("%s/%s" % p for p in bad)
            common.append_jsonl(path_of(src), row)
        finally:
            with lock:
                state["inflight"] -= 1
        if bad:
            raise Stop("PROVIDER VIOLATION on %s %s: %s" % (src["arm"], src["qid"], row["error"]))
        with lock:
            if row["status"] == "ok":
                costs.append(row["usage"]["total"]["usd_est"])
            else:
                log("%s %s rep %d failed: %s" % (src["arm"], src["qid"], src["rep"], row["error"]))
            state["n"] += 1

    try:
        common.run_jobs(todo, one, workers)
    except Stop:
        raise
    return state["n"]
