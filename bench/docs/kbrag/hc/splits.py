"""Freeze the ConditionalQA dev/test split (RFC HC §6.1).

    python3 -I bench/docs/kbrag/hc/splits.py --questions <prepared>/questions.jsonl   # writes splits.json
    python3 -I bench/docs/kbrag/hc/splits.py --questions <prepared>/questions.jsonl --verify

test = the 800 of bench/docs/measure/reader_subset.json (the passage-level test, T1/T2; never
tuned on). dev = every other answerable question bench/docs/measure/prepare.py keeps (the
kb/read prompt, the answer-first kb/search prompt, the option defaults; never tested on).
No randomness: the test set was drawn by RFC DM with random.Random(20260929), and dev is
its complement, so the split is stable by construction. splits.json holds ids only.
"""
import argparse, hashlib, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import hcconfig  # noqa: E402


class SplitError(ValueError):
    pass


def make_splits(answerable, test):
    """{"test": [...], "dev": [...]} as sorted id lists. Refuses a test id outside the
    answerable set and a duplicate on either side."""
    answerable, test = list(answerable), list(test)
    if len(set(answerable)) != len(answerable):
        raise SplitError("duplicate question id among the answerable questions")
    if len(set(test)) != len(test):
        raise SplitError("duplicate question id in the test set")
    outside = sorted(set(test) - set(answerable))
    if outside:
        raise SplitError("%d test ids are not answerable questions, e.g. %s" % (len(outside), outside[:3]))
    t = set(test)
    return {"test": sorted(t), "dev": sorted(q for q in set(answerable) if q not in t)}


def problems(split, answerable, test):
    """Why a split is not the frozen one for these inputs (empty when it is): the two sides
    disjoint, together exactly the answerable set, test exactly the given test set."""
    out = []
    d, t = set(split.get("dev") or []), set(split.get("test") or [])
    if d & t:
        out.append("dev and test share %d ids" % len(d & t))
    if (d | t) != set(answerable):
        out.append("dev + test is not the answerable set (missing %d, extra %d)"
                   % (len(set(answerable) - (d | t)), len((d | t) - set(answerable))))
    if t != set(test):
        out.append("test is not reader_subset.json (%d differ)" % len(t ^ set(test)))
    if split.get("dev") != sorted(d) or split.get("test") != sorted(t):
        out.append("lists are not sorted (the file must be byte-stable)")
    return out


def sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def load(path=None):
    with open(path or hcconfig.SPLITS, encoding="utf-8") as f:
        return json.load(f)


def which(split, qid):
    if qid in set(split["test"]):
        return "test"
    if qid in set(split["dev"]):
        return "dev"
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--questions", required=True, help="questions.jsonl from bench/docs/measure/prepare.py")
    ap.add_argument("--out", default=hcconfig.SPLITS)
    ap.add_argument("--verify", action="store_true", help="check --out against the inputs; write nothing")
    a = ap.parse_args()
    with open(a.questions, encoding="utf-8") as f:
        answerable = [json.loads(l)["qid"] for l in f if l.strip()]
    with open(hcconfig.SUBSET, encoding="utf-8") as f:
        test = json.load(f)
    if a.verify:
        p = problems(load(a.out), answerable, test)
        print(json.dumps({"ok": not p, "problems": p}, indent=1))
        sys.exit(0 if not p else 1)
    s = make_splits(answerable, test)
    out = {"about": "RFC HC frozen ConditionalQA split: test = bench/docs/measure/reader_subset.json, "
                    "dev = the other answerable questions of bench/docs/measure/prepare.py",
           "reader_subset_sha256": sha256(hcconfig.SUBSET), "n_answerable": len(answerable),
           "n_test": len(s["test"]), "n_dev": len(s["dev"]), "test": s["test"], "dev": s["dev"]}
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(out, f, indent=0)
        f.write("\n")
    print("answerable %d, test %d, dev %d -> %s" % (len(answerable), len(s["test"]), len(s["dev"]), a.out))


if __name__ == "__main__":
    main()
