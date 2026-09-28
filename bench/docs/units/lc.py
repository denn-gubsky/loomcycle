"""Thin client for the probe's isolated loomcycle: agent runs and operator routes."""
import json, os, urllib.request

BASE = os.environ.get("LC_BASE", "http://127.0.0.1:8814")


def run(agent, user_id, prompt, timeout=900):
    """POST /v1/runs for a code-js agent and return its final text parsed as JSON."""
    body = {"agent": agent, "user_id": user_id,
            "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": prompt}]}]}
    req = urllib.request.Request(BASE + "/v1/runs", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    raw = urllib.request.urlopen(req, timeout=timeout).read().decode()
    text = None
    for block in raw.split("\n\n"):
        for line in block.splitlines():
            if line.startswith("data: "):
                d = json.loads(line[6:])
                if d.get("type") == "text":
                    text = d["text"]
                if d.get("type") == "error":
                    raise RuntimeError(d.get("error") or d)
    if text is None:
        raise RuntimeError("run produced no text")
    return json.loads(text)


def post(path, timeout=3600):
    req = urllib.request.Request(BASE + path, data=b"", method="POST")
    return json.load(urllib.request.urlopen(req, timeout=timeout))
