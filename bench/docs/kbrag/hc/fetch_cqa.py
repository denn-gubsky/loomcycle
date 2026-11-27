"""Download the public ConditionalQA v1.0 files the split is built from.

    python3 -I bench/docs/kbrag/hc/fetch_cqa.py --dest <dir outside the repo>
    python3 -I bench/docs/measure/prepare.py --src <dir> --out <dir>/prepared
    python3 -I bench/docs/kbrag/hc/splits.py --questions <dir>/prepared/questions.jsonl

The dataset (github.com/haitian-sun/ConditionalQA, research use) is fetched as published;
nothing of it is committed except the question ids in splits.json.
"""
import argparse, hashlib, os, urllib.request

URL = "https://raw.githubusercontent.com/haitian-sun/ConditionalQA/master/v1_0/%s"
FILES = ("documents.json", "dev.json", "train.json")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dest", required=True)
    a = ap.parse_args()
    os.makedirs(a.dest, exist_ok=True)
    for name in FILES:
        path = os.path.join(a.dest, name)
        with urllib.request.urlopen(URL % name, timeout=300) as r:
            data = r.read()
        with open(path, "wb") as f:
            f.write(data)
        print("%s %d bytes sha256 %s" % (name, len(data), hashlib.sha256(data).hexdigest()))


if __name__ == "__main__":
    main()
