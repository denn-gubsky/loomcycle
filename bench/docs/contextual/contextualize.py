#!/usr/bin/env python3
"""Generate a situating context for every section chunk — Anthropic's Contextual Retrieval.

One LOCAL model call per chunk, whole document in the prompt, straight to Ollama: nothing
here can reach a paid provider, which is the point of calling Ollama directly rather than
through a tier that might fall through to the cloud.

The document goes FIRST and the chunk LAST so consecutive calls for one paper share a
prompt prefix; Ollama reuses the KV cache for it, which is what makes one call per chunk
affordable on a local model. Papers are therefore processed one at a time, in order.

Resumable: results append to contexts.jsonl keyed by (paper_id, path); a rerun skips
what is already there.
"""
import argparse
import json
import os
import sys
import time
import urllib.request

# Anthropic's published prompt (Contextual Retrieval, 2024), unchanged apart from the tags
# holding a paper rather than a generic document.
PROMPT = """<document>
{doc}
</document>
Here is the chunk we want to situate within the whole document
<chunk>
{chunk}
</chunk>
Please give a short succinct context to situate this chunk within the overall document for the purposes of improving search retrieval of the chunk. Answer only with the succinct context and nothing else."""


def call(host, model, prompt, num_ctx):
    body = {"model": model, "stream": False, "think": False,
            "options": {"num_ctx": num_ctx, "temperature": 0, "num_predict": 200},
            "messages": [{"role": "user", "content": prompt}]}
    r = urllib.request.Request(host + "/api/chat", data=json.dumps(body).encode(),
                               headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=900) as x:
        d = json.load(x)
    return d["message"]["content"].strip(), d.get("prompt_eval_count", 0), d.get("eval_count", 0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True, help="dir with papers.jsonl")
    ap.add_argument("--host", default="http://100.112.7.68:11434")
    ap.add_argument("--model", default="ornith-1.5:35b")
    ap.add_argument("--num-ctx", type=int, default=32768)
    a = ap.parse_args()

    out = os.path.join(a.probe, "contexts.jsonl")
    done = set()
    if os.path.exists(out):
        for line in open(out):
            r = json.loads(line)
            done.add((r["paper_id"], tuple(r["path"])))
    papers = [json.loads(l) for l in open(os.path.join(a.probe, "papers.jsonl"))]
    total = sum(len(p["sections"]) for p in papers)
    n, fails, t0 = len(done), 0, time.time()
    with open(out, "a") as f:
        for p in papers:
            for s in p["sections"]:
                key = (p["id"], tuple(s["path"]))
                if key in done:
                    continue
                prompt = PROMPT.format(doc=p["markdown"], chunk=s["text"])
                for attempt in range(3):
                    try:
                        t = time.time()
                        ctx, pin, pout = call(a.host, a.model, prompt, a.num_ctx)
                        break
                    except Exception as e:  # a CUDA fault or a dropped socket: retry, then record
                        err = str(e)
                        time.sleep(5 * (attempt + 1))
                else:
                    fails += 1
                    print("FAIL", p["id"], s["path"], err[:200], file=sys.stderr, flush=True)
                    continue
                f.write(json.dumps({"paper_id": p["id"], "path": s["path"], "context": ctx,
                                    "prompt_tokens": pin, "output_tokens": pout,
                                    "seconds": round(time.time() - t, 2)}) + "\n")
                f.flush()
                n += 1
                if n % 25 == 0:
                    rate = (time.time() - t0) / max(1, n - len(done))
                    print(f"{n}/{total} fails={fails} {rate:.1f}s/chunk eta={(total - n) * rate / 60:.0f}m", flush=True)
    print(f"done {n}/{total} fails={fails}", flush=True)


if __name__ == "__main__":
    main()
