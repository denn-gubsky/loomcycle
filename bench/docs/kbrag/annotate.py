"""Drive kb/annotator over the tenant until every document is annotated.

    python3 -I bench/docs/kbrag/annotate.py --report-only          # write:report only
    python3 -I bench/docs/kbrag/annotate.py --deadline 2026-11-24T08:00:00Z --budget-usd 6
    python3 -I bench/docs/kbrag/annotate.py --status               # list_annotations summarize

A sweep report is taken first (annotate_report.json). Then `write: annotate`
sweep passes run one after another until a pass completes and a fresh report
shows nothing left to call. busy / provider_unavailable / failed passes back off.

Guards, checked before or after every pass (the reason goes to annotate.STOP and
the loop exits):
  * provider: every run of the pass's tree, and every row of the tenant's usage
    ledger written during the pass, must be anthropic/claude-haiku-5-5, ollama-local,
    or the deployment's embedder (code-js parents excepted);
  * budget: cumulative tenant spend (lc.spend()) at or over --budget-usd stops
    before the next pass;
  * --deadline (UTC ISO) and --max-passes.
Every pass appends one line to annotate.jsonl.
"""
import argparse, datetime as dt, json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lc

WORK = "/home/denn/workspace/kbrag-work/cqa"
LOG = os.path.join(WORK, "annotate.jsonl")
STOP = os.path.join(WORK, "annotate.STOP")


def now():
    return dt.datetime.now(dt.timezone.utc)


def iso(t):
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_report(text):
    text = (text or "").strip()
    i = text.find("{")
    try:
        return json.loads(text[i:]) if i >= 0 else None
    except ValueError:
        try:
            return json.JSONDecoder().raw_decode(text[i:])[0]
        except ValueError:
            return None


def annotator(args, timeout):
    # The annotator is code-js: its server-side active-time budget (1 h by
    # default) is raised to the pass timeout; a sweep of 652 pages needs more.
    r = lc.run("kb/annotator", json.dumps(dict(args, scope="tenant")), user_tier="free",
               timeout=timeout + 600, run_timeout_seconds=timeout)
    return r, parse_report(r["text"])


def stop(reason):
    with open(STOP, "w") as f:
        f.write(iso(now()) + " " + reason + "\n")
    print("STOP:", reason, flush=True)
    sys.exit(2)


def guard(tree, since):
    bad = lc.disallowed(tree["providers"]) if tree else []
    runs = [r for r in (tree or {}).get("runs", []) if (r["provider"], r["model"]) in bad]
    ledger_bad = lc.disallowed(lc.ledger_pairs(since))
    if bad or ledger_bad:
        stop("disallowed provider: run tree %s %s; usage ledger since %s %s" % (
            bad, [(r["run_id"], r["agent"]) for r in runs], since, ledger_bad))


def report(timeout):
    r, rep = annotator({"mode": "sweep", "write": "report"}, timeout)
    if rep is None:
        print("report run failed:", r["status"], r["error"], r["text"][:500], flush=True)
    return r, rep


def status():
    print(json.dumps(lc.doc("list_annotations", scope="tenant", summarize=True), indent=1))
    r = lc.doc("query_chunks", scope="tenant", sql="SELECT count(DISTINCT document_id) FROM chunks")
    print("documents in tenant (incl. housekeeping):", r["rows"][0][0])
    if os.path.exists(STOP):
        print("STOP file:", open(STOP).read().strip())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--deadline", help="UTC ISO time after which no new pass starts")
    ap.add_argument("--max-passes", type=int, default=200)
    ap.add_argument("--max-calls", type=int, default=1000, help="model calls per pass")
    ap.add_argument("--budget-usd", type=float, default=6.0)
    ap.add_argument("--pass-timeout", type=int, default=8 * 3600)
    ap.add_argument("--report-only", action="store_true")
    ap.add_argument("--skip-report", action="store_true", help="start annotating without the initial report")
    ap.add_argument("--status", action="store_true")
    a = ap.parse_args()
    if a.status:
        return status()
    if os.path.exists(STOP) and not a.report_only:
        raise SystemExit("annotate.STOP exists (%s); remove it to continue" % open(STOP).read().strip())
    deadline = dt.datetime.fromisoformat(a.deadline.replace("Z", "+00:00")) if a.deadline else None
    os.makedirs(WORK, exist_ok=True)
    print("server", lc.version(), "spend", lc.spend(), flush=True)

    since = iso(now())
    if a.skip_report:  # a fresh full report is already on disk (it takes ~1.5 h on 652 pages)
        print("skipping the initial report (--skip-report)", flush=True)
        rep = {}
    else:
        r, rep = report(a.pass_timeout)
        tree = lc.tree_usage(r["run_id"]) if r["run_id"] else None
        json.dump({"at": since, "run": r, "report": rep, "tree": tree}, open(os.path.join(WORK, "annotate_report.json"), "w"), indent=1)
        print("report:", json.dumps(rep), flush=True)
        guard(tree, since)
    if a.report_only or rep is None:
        return
    _loop(a, deadline)


def _loop(a, deadline):
    backoff, failures = 60, 0
    for n in range(1, a.max_passes + 1):
        if deadline and now() >= deadline:
            stop("deadline %s reached before pass %d" % (a.deadline, n))
        spent = lc.spend()
        if spent >= a.budget_usd:
            stop("budget: tenant spend %.4f USD >= --budget-usd %.2f before pass %d" % (spent, a.budget_usd, n))
        since, t0 = iso(now()), time.time()
        r, rep = annotator({"mode": "sweep", "write": "annotate", "max_calls": a.max_calls,
                            "max_writes": 4 * a.max_calls}, a.pass_timeout)
        tree = lc.tree_usage(r["run_id"]) if r["run_id"] else None
        after = lc.spend()
        st = (rep or {}).get("status") or r["status"]
        children = [x for x in (tree or {}).get("runs", []) if x["agent"] != "kb/annotator"]
        rec = {"pass": n, "started": since, "wall_s": round(time.time() - t0, 1), "run_id": r["run_id"],
               "run_status": r["status"], "error": r["error"], "report": rep,
               "totals": (tree or {}).get("totals"), "providers": (tree or {}).get("providers"),
               "child_runs": len(children), "spend_before": spent, "spend_after": after,
               "pass_usd": round(after - spent, 6)}
        with open(LOG, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print("pass %d: %s, %s child runs, %.0f s, $%.4f (total $%.4f), providers %s" % (
            n, st, len(children), rec["wall_s"], rec["pass_usd"], after, rec["providers"]), flush=True)
        guard(tree, since)
        if st in ("busy", "provider_unavailable", "failed", "lease_lost") or rep is None:
            failures += 1
            if failures >= 8:
                stop("8 consecutive unsuccessful passes; last: %s %s" % (st, r["error"]))
            time.sleep(backoff); backoff = min(backoff * 2, 1800)
            continue
        failures, backoff = 0, 60
        if st == "complete":
            _, check = report(a.pass_timeout)
            left = (check or {}).get("would_call")
            print("after pass %d: would_call %s" % (n, json.dumps(left)), flush=True)
            if check is not None and not any((left or {}).values() if isinstance(left, dict) else [left]):
                with open(os.path.join(WORK, "annotate.DONE"), "w") as f:
                    f.write(iso(now()) + " complete after %d passes; spend %.4f USD\n" % (n, lc.spend()))
                print("annotation complete", flush=True)
                return
    stop("max passes (%d) reached" % a.max_passes)


if __name__ == "__main__":
    main()
