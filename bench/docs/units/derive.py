"""Generate units for every policy through the operator pass, following the cursor.

    python3 derive.py --probe <dir> [--dry-run]   -> <dir>/derive.jsonl (one report per call)
"""
import argparse, json, os, time
import lc


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    pids = [json.loads(l)["id"] for l in open(os.path.join(a.probe, "policies.jsonl"))]
    with open(os.path.join(a.probe, "derive%s.jsonl" % ("-dry" if a.dry_run else "")), "a") as f:
        for pid in pids:
            cursor = ""
            while True:
                t = time.time()
                q = "/v1/_document/derive_units?scope=user&scope_id=%s&limit=100&dry_run=%s&after=%s" % (
                    pid, "true" if a.dry_run else "false", cursor)
                rep = lc.post(q)
                rep["policy_id"], rep["seconds"] = pid, round(time.time() - t, 1)
                rep.pop("samples", None)
                f.write(json.dumps(rep) + "\n")
                f.flush()
                print(pid, {k: rep.get(k) for k in ("generated", "rewritten", "up_to_date", "units_written", "failed")},
                      rep["seconds"], "s", flush=True)
                if not rep.get("more"):
                    break
                cursor = rep["next_cursor"]


if __name__ == "__main__":
    main()
