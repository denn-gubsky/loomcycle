"""The study's spend cap: $20, and every runner stops before a step that could take the SUM of
both tenants' ledger increases over the recorded baseline past $19 (plan §3.5).

    python3 -I bench/docs/kbrag/hc/spendguard.py --record    # writes <work>/baseline.json (once, before any spend)
    python3 -I bench/docs/kbrag/hc/spendguard.py             # prints the increase so far

The baseline is taken once, before the dev tuning and the annotator sweeps, so they count
against the same cap as the scored runs.
"""
import argparse, datetime, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import hcconfig  # noqa: E402


class Spend:
    """increase() = sum over the tenants of (ledger now - ledger at baseline)."""

    def __init__(self, clients, baseline):
        missing = sorted(set(clients) - set(baseline))
        if missing:
            raise SystemExit("baseline.json has no entry for %s: record the baseline first" % ", ".join(missing))
        self.clients, self.baseline = clients, baseline

    def increase(self):
        return round(sum(c.spend() - self.baseline[name] for name, c in self.clients.items()), 6)


class View:
    """A client whose spend() is the study-wide increase: what ../passes.run_pass and
    ../annotate.py compare against their budget, so their own guards hold the study's cap."""

    def __init__(self, lc, spend):
        self._lc, self._spend = lc, spend

    def spend(self):
        return self._spend.increase()

    def __getattr__(self, name):
        return getattr(self._lc, name)


def baseline_path():
    return os.path.join(hcconfig.work(), "baseline.json")


def load_baseline():
    with open(baseline_path(), encoding="utf-8") as f:
        return json.load(f)["tenant_spend_usd"]


def all_clients():
    import client
    return {s: client.for_set(s) for s in hcconfig.SETS}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--record", action="store_true")
    a = ap.parse_args()
    clients = all_clients()
    for c in clients.values():
        c.assert_tenant()
    if a.record:
        if os.path.exists(baseline_path()):
            raise SystemExit("baseline.json exists; the baseline is recorded once per study")
        os.makedirs(hcconfig.work(), exist_ok=True)
        b = {"at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
             "tenant_spend_usd": {s: c.spend() for s, c in clients.items()}}
        with open(baseline_path(), "w", encoding="utf-8") as f:
            json.dump(b, f, indent=1)
        print(json.dumps(b, indent=1))
        return
    print(json.dumps({"increase_usd": Spend(clients, load_baseline()).increase(), "stop_at_usd": hcconfig.BUDGET_USD,
                      "cap_usd": hcconfig.CAP_USD}, indent=1))


if __name__ == "__main__":
    main()
