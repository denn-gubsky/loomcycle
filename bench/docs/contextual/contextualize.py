#!/usr/bin/env python3
"""Generate one short text per section chunk with a local model. Three modes:

  context     Anthropic's Contextual Retrieval: whole document + chunk → a situating context
  describe    the chunk ALONE (plus its title and heading path, for grounding) → a one-to-two
              sentence description of what it states. The light analogue of fact extraction:
              it reads one unit and says what is in it, with no document around it to lean on.
  mapcontext  Anthropic's prompt, but with the DOCUMENT MAP (title + every section's
              description, in order) standing in for the whole document. Needs describe first.
  units       the chunk alone → JSON {"claims": [...], "questions": [...]}: several atomic
              claims (the real analogue of memory facts: one specific statement each) and the
              questions the section answers (doc2query). Ollama's JSON mode, so it parses.

One LOCAL model call per chunk, whole document in the prompt, straight to Ollama: nothing
here can reach a paid provider, which is the point of calling Ollama directly rather than
through a tier that might fall through to the cloud.

The document goes FIRST and the chunk LAST so consecutive calls for one paper share a
prompt prefix; Ollama reuses the KV cache for it, which is what makes one call per chunk
affordable on a local model. Papers are therefore processed one at a time, in order.

Resumable: results append to <mode>.jsonl (contexts.jsonl for `context`) keyed by
(paper_id, path); a rerun skips what is already there.
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

DESCRIBE = """Paper: {title}
Section: {path}
<section>
{chunk}
</section>
Describe in one or two sentences what this section states. Name the specific methods, models, datasets, metrics, numbers and findings it contains, in the paper's own terms. Answer only with the description."""


def document_map(paper, descriptions):
    """The map a reader or a model sees: the title, then every section in document order with
    its heading path and description. Paths rather than bare headings, because a paper has
    several "Results" and a bare name would not say which."""
    lines = ["# " + paper["title"], ""]
    for s in paper["sections"]:
        d = descriptions.get((paper["id"], tuple(s["path"])), "")
        lines.append("- %s: %s" % (" > ".join(s["path"]), d))
    return "\n".join(lines)


UNITS = """Paper: {title}
Section: {path}
<section>
{chunk}
</section>
From this section write:
1. "claims": 3 to 6 atomic claims. Each is one self-contained sentence stating ONE specific fact from the section — a method, model, dataset, metric, number or finding — in the paper's own terms, understandable without the section.
2. "questions": 3 to 6 questions that this section answers, phrased as a reader of the paper would ask them.
If the section is very short, write fewer. Answer only with JSON: {{"claims": [...], "questions": [...]}}"""


def call(host, model, prompt, num_ctx, num_predict=200, json_mode=False):
    body = {"model": model, "stream": False, "think": False,
            "options": {"num_ctx": num_ctx, "temperature": 0, "num_predict": num_predict},
            "messages": [{"role": "user", "content": prompt}]}
    if json_mode:
        body["format"] = "json"
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
    # 16384 fits every prompt on this corpus. One value for every phase, because a change
    # makes Ollama reload the model, and reloads coincided with the CUDA faults.
    ap.add_argument("--num-ctx", type=int, default=16384)
    ap.add_argument("--mode", choices=["context", "describe", "mapcontext", "units"], default="context")
    ap.add_argument("--limit", type=int, default=0, help="stop after N new chunks (smoke tests)")
    a = ap.parse_args()

    out = os.path.join(a.probe, "contexts.jsonl" if a.mode == "context" else a.mode + ".jsonl")
    descriptions = {}
    if a.mode == "mapcontext":
        for line in open(os.path.join(a.probe, "describe.jsonl")):
            r = json.loads(line)
            descriptions[(r["paper_id"], tuple(r["path"]))] = r["context"]
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
                if a.mode == "context":
                    prompt = PROMPT.format(doc=p["markdown"], chunk=s["text"])
                elif a.mode == "units":
                    prompt = UNITS.format(title=p["title"], path=" > ".join(s["path"]), chunk=s["text"])
                elif a.mode == "describe":
                    prompt = DESCRIBE.format(title=p["title"], path=" > ".join(s["path"]), chunk=s["text"])
                else:
                    prompt = PROMPT.format(doc=document_map(p, descriptions), chunk=s["text"])
                for attempt in range(3):
                    try:
                        t = time.time()
                        if a.mode == "units":
                            ctx, pin, pout = call(a.host, a.model, prompt, a.num_ctx, 700, True)
                            json.loads(ctx)  # JSON mode should guarantee this; a failure retries
                        else:
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
                if a.limit and n - len(done) >= a.limit:
                    print("limit reached", flush=True)
                    return
                if n % 25 == 0:
                    rate = (time.time() - t0) / max(1, n - len(done))
                    print(f"{n}/{total} fails={fails} {rate:.1f}s/chunk eta={(total - n) * rate / 60:.0f}m", flush=True)
    print(f"done {n}/{total} fails={fails}", flush=True)


if __name__ == "__main__":
    main()
