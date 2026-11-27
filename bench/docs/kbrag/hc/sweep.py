"""The annotator sweep before the first scored run (RFC HC §6.4 fix 2), on one set's tenant.

    python3 -I bench/docs/kbrag/hc/sweep.py --set cqa --deadline 2026-12-01T08:00:00Z
    python3 -I bench/docs/kbrag/hc/sweep.py --set p7  --deadline 2026-12-01T08:00:00Z
    then: precheck.py --set <set>   (check 2 is taken AFTER the sweep)

Drives ../annotate.py's loop unchanged (a `kb/annotator` report, then `mode: sweep,
write: annotate` passes until a fresh report shows nothing left to call), with its provider
guard, bound to this set's tenant. Its budget is the study's: annotate.py stops when spend()
reaches --budget-usd, and spend() here is the increase of BOTH tenants' ledgers over the
recorded baseline (spendguard.py), so the sweep can never take the study past $19.
Logs go to <work>/<set>/annotate.jsonl; a guard stop writes <work>/<set>/annotate.STOP.
"""
import argparse, datetime as dt, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import hcconfig, spendguard  # noqa: E402
import annotate  # noqa: E402  (../annotate.py: the Study C sweep loop)


def bind(lc, work_dir):
    """Point ../annotate.py's module state at this tenant and work directory."""
    os.makedirs(work_dir, exist_ok=True)
    annotate.lc = lc
    annotate.WORK = work_dir
    annotate.LOG = os.path.join(work_dir, "annotate.jsonl")
    annotate.STOP = os.path.join(work_dir, "annotate.STOP")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", required=True, choices=hcconfig.SETS)
    ap.add_argument("--deadline", required=True, help="UTC ISO time after which no new pass starts")
    ap.add_argument("--max-passes", type=int, default=200)
    ap.add_argument("--max-calls", type=int, default=1000)
    ap.add_argument("--budget-usd", type=float, default=hcconfig.BUDGET_USD,
                    help="stop when the study-wide ledger increase reaches this (default the study's $19)")
    ap.add_argument("--pass-timeout", type=int, default=8 * 3600)
    a = ap.parse_args()
    clients = spendguard.all_clients()
    for c in clients.values():
        c.assert_tenant()
    spend = spendguard.Spend(clients, spendguard.load_baseline())
    bind(spendguard.View(clients[a.set], spend), os.path.join(hcconfig.work(), a.set))
    if os.path.exists(annotate.STOP):
        raise SystemExit("annotate.STOP exists for %s; read it and remove it to continue" % a.set)
    deadline = dt.datetime.fromisoformat(a.deadline.replace("Z", "+00:00"))
    since = annotate.iso(annotate.now())
    r, rep = annotate.report(a.pass_timeout)
    tree = annotate.lc.tree_usage(r["run_id"]) if r["run_id"] else None
    with open(os.path.join(annotate.WORK, "annotate_report.json"), "w") as f:
        json.dump({"at": since, "report": rep, "run_id": r["run_id"], "status": r["status"]}, f, indent=1)
    annotate.guard(tree, since)
    if rep is None:
        raise SystemExit("the initial report failed: %s" % r.get("error"))
    annotate._loop(argparse.Namespace(max_passes=a.max_passes, max_calls=a.max_calls, budget_usd=a.budget_usd,
                                      pass_timeout=a.pass_timeout, deadline=a.deadline), deadline)


if __name__ == "__main__":
    main()
